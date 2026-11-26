// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package worm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Tests of the second review (A2-01, A2-03, A2-06, A2-10, A2-11).

func payloadAt(t testing.TB, size string, first time.Time) anchor.Payload {
	t.Helper()
	p, err := anchor.NewPayload(aggID, testNote(t, size, 1), first)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func lock(f *fakeS3, v *fakeVersion, mode string, retain time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v.mode, v.retain = mode, retain.UTC().Format("2006-01-02T15:04:05.000Z")
}

// A2-01: a store whose clock was refused for a fresh write is refused again on
// every later attempt for that object. The first attempt wrote and locked the
// object; the retry finds it and judges its time against the first attempt of
// the record, not by a wide window of age.
func TestSkewedStoreIsRefusedOnEveryAttempt(t *testing.T) {
	type bad struct {
		skew  time.Duration
		class anchor.Class
		code  string
	}
	for name, c := range map[string]bad{
		"5-days-behind":  {-5 * 24 * time.Hour, anchor.ClassPermanent, "store_time_early"},
		"7-days-behind":  {-7 * 24 * time.Hour, anchor.ClassPermanent, "store_time_early"},
		"6-minutes-late": {-6 * time.Minute, anchor.ClassPermanent, "store_time_early"},
		"1-hour-ahead":   {time.Hour, anchor.ClassRefused, "store_time_skew"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeS3()
			defer f.close()
			f.skew = c.skew
			tg := newTarget(t, f)
			p := testPayload(t, "21", 1)
			for i := 0; i < 3; i++ {
				_, err := tg.Submit(context.Background(), p)
				expectErr(t, err, c.class, c.code)
			}
			if n := len(f.stored(keyOf("21"))); n != 1 {
				t.Fatalf("%d versions of the key", n)
			}
			// the record fails at once when the class is permanent, and the
			// alert text can be taken from the code and the text
			if c.class == anchor.ClassPermanent {
				_, err := tg.Submit(context.Background(), p)
				r := anchor.NewRecord(1, "agg", 21, p.Digest, time.Now())
				out, alert, aerr := r.AfterError(err, time.Now(), 0.5, anchor.Limits{})
				if aerr != nil || out.State != anchor.StateFailed || alert != anchor.AlertFailed || !strings.Contains(out.LastError, "clock") {
					t.Fatalf("%+v %q %v", out, alert, aerr)
				}
			}
		})
	}
}

// A2-01 and A2-03: an object that a first attempt wrote long before (a crash
// between the write and the record) is accepted when its time is inside the
// window of the first attempt, however old it is; the window is the first
// attempt minus the tolerance, to the clock plus the tolerance.
func TestOldObjectFromACrashStillCompletes(t *testing.T) {
	for _, age := range []time.Duration{time.Hour, 9 * 24 * time.Hour, 40 * 24 * time.Hour, 400 * 24 * time.Hour} {
		f := newFakeS3()
		first := time.Now().UTC().Add(-age).Truncate(time.Second)
		p := payloadAt(t, "22", first)
		v := f.addVersion(keyOf("22"), p.SignedNote, "COMPLIANCE", time.Now().Add(30*24*time.Hour))
		f.mu.Lock()
		v.lastMod = first.Add(10 * time.Second)
		f.mu.Unlock()
		rc, ev := mustSubmit(t, newTarget(t, f), p)
		if rc.State != anchor.ReceiptAnchored || ev.LastModified != anchor.FormatTime(first.Add(10*time.Second)) || ev.VersionID != v.id || f.retentionCalls != 0 {
			t.Fatalf("age %v: %+v retention calls %d", age, ev, f.retentionCalls)
		}
		f.close()
	}
	// the edges of the window, with literals: the first attempt less 5 minutes
	// is the earliest time accepted
	for name, c := range map[string]struct {
		off time.Duration
		ok  bool
	}{
		"4-minutes-before": {-4 * time.Minute, true},
		"6-minutes-before": {-6 * time.Minute, false},
		"1-hour-before":    {-time.Hour, false},
	} {
		f := newFakeS3()
		first := time.Now().UTC().Add(-3 * 24 * time.Hour).Truncate(time.Second)
		p := payloadAt(t, "23", first)
		v := f.addVersion(keyOf("23"), p.SignedNote, "COMPLIANCE", time.Now().Add(30*24*time.Hour))
		f.mu.Lock()
		v.lastMod = first.Add(c.off)
		f.mu.Unlock()
		_, err := newTarget(t, f).Submit(context.Background(), p)
		if c.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !c.ok {
			expectErr(t, err, anchor.ClassPermanent, "store_time_early")
		}
		f.close()
	}
}

// A2-03: the recovery path that the error text names (the operator applies the
// lock with the store's tools, then Retry) works after any delay.
func TestRecoveryPathWorksAfterAnyDelay(t *testing.T) {
	for _, age := range []time.Duration{time.Minute, 9 * 24 * time.Hour, 60 * 24 * time.Hour} {
		f := newFakeS3()
		f.retentionStatus = 403 // the store refuses PutObjectRetention
		first := time.Now().UTC().Add(-age).Truncate(time.Second)
		p := payloadAt(t, "24", first)
		v := f.addVersion(keyOf("24"), p.SignedNote, "", time.Time{}) // unlocked
		f.mu.Lock()
		v.lastMod = first.Add(time.Second)
		f.mu.Unlock()
		tg := newTarget(t, f)
		_, err := tg.Submit(context.Background(), p)
		expectErr(t, err, anchor.ClassPermanent, "not_locked")
		// the operator locks that version, the record is retried much later
		lock(f, v, "COMPLIANCE", time.Now().Add(30*24*time.Hour))
		rc, ev := mustSubmit(t, tg, p)
		if rc.State != anchor.ReceiptAnchored || ev.VersionID != v.id {
			t.Fatalf("age %v: %+v", age, ev)
		}
		f.close()
	}
}

// The time of the first attempt is not in the future: that is a problem of
// this server's clock, retried, and nothing is sent to the store.
func TestFirstAttemptInTheFutureIsRetryable(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	_, err := tg.Submit(context.Background(), payloadAt(t, "25", time.Now().Add(time.Hour)))
	ae := expectErr(t, err, anchor.ClassRetryable, "submit_start_ahead")
	if !strings.Contains(ae.Error(), "clock") || len(f.requests()) != 0 {
		t.Fatalf("%v, %d requests", ae, len(f.requests()))
	}
	// four minutes ahead is inside the tolerance
	if _, ev := mustSubmit(t, tg, payloadAt(t, "25", time.Now().Add(4*time.Minute))); ev.LastModified == "" {
		t.Fatal()
	}
}

// A payload without the time of the first attempt is refused by every Submit.
func TestSubmitRequiresTheFirstAttempt(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	p, err := anchor.ParsePayload(aggID, testNote(t, "26", 1))
	if err != nil {
		t.Fatal(err)
	}
	_, err = newTarget(t, f).Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassPermanent, "bad_payload")
	if len(f.requests()) != 0 {
		t.Fatal("request sent")
	}
}

// A2-10: an existing version whose retention is earlier than the first attempt
// asked, less the write tolerance of five minutes, is repaired (the retention
// is applied and the version verified again); one inside the tolerance is
// not touched.
func TestExistingShortRetentionIsRepaired(t *testing.T) {
	for name, c := range map[string]struct {
		days    int
		retain  time.Duration // from now
		repairs int
	}{
		"23-days-of-30":       {30, 23 * 24 * time.Hour, 1},
		"1-day-asked-1h-left": {1, time.Hour, 1},
		// the floor is taken from the store's own time: the Last-Modified plus the
		// days, less the tolerance of the time (5 minutes) and the retention
		// tolerance (5 minutes)
		"just-inside":  {30, 30*24*time.Hour - 9*time.Minute, 0},
		"just-outside": {30, 30*24*time.Hour - 11*time.Minute, 1},
		"longer":       {30, 40 * 24 * time.Hour, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeS3()
			defer f.close()
			tg := newTarget(t, f, func(cfg *Config, o *Options) { cfg.RetentionDays = c.days })
			p := testPayload(t, "27", 1)
			f.addVersion(keyOf("27"), p.SignedNote, "COMPLIANCE", time.Now().Add(c.retain))
			_, ev := mustSubmit(t, tg, p)
			until, _ := time.Parse(anchor.TimeLayout, ev.RetainUntil)
			if f.retentionCalls != c.repairs {
				t.Fatalf("retention calls %d, want %d (%+v)", f.retentionCalls, c.repairs, ev)
			}
			if min := time.Now().Add(time.Duration(c.days)*24*time.Hour - 10*time.Minute); until.Before(min) && c.repairs > 0 {
				t.Fatalf("retention %v after the repair", until)
			}
		})
	}
	// a crash recovery long after the first attempt does not extend a
	// version that satisfied the first request
	f := newFakeS3()
	defer f.close()
	first := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	p := payloadAt(t, "28", first)
	v := f.addVersion(keyOf("28"), p.SignedNote, "COMPLIANCE", first.Add(30*24*time.Hour-4*time.Minute))
	f.mu.Lock()
	v.lastMod = first.Add(time.Second)
	f.mu.Unlock()
	mustSubmit(t, newTarget(t, f), p)
	if f.retentionCalls != 0 {
		t.Fatal("a version that met the first request was extended")
	}
}

// A2-06: a HEAD that answers with another version id than the one asked for
// is a mismatch, whatever the rest says.
func TestHeadAnswersAnotherVersion(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	f.headVersion = "some-other-version"
	_, err := newTarget(t, f).Submit(context.Background(), testPayload(t, "29", 1))
	ae := expectErr(t, err, anchor.ClassPermanent, "readback_mismatch")
	if !strings.Contains(ae.Error(), "version id") {
		t.Fatal(ae)
	}
}

// A2-06: the limits of what is read from a version listing, each at its
// boundary, with literals (the constants themselves are asserted below).
func TestListingLimits(t *testing.T) {
	if MaxListBody != 1<<20 || MaxListPages != 5 || MaxVersionsChecked != 25 {
		t.Fatalf("the limits changed: %d %d %d", MaxListBody, MaxListPages, MaxVersionsChecked)
	}
	// a key with one version of ours and n foreign ones after it
	build := func(t *testing.T, foreign int, listPage int) (*fakeS3, *Target, anchor.Payload) {
		f := newFakeS3()
		f.listPage = listPage
		tg := newTarget(t, f)
		p := testPayload(t, "30", 1)
		mustSubmit(t, tg, p)
		for i := 0; i < foreign; i++ {
			f.addVersion(keyOf("30"), []byte{byte(i), 7, 7}, "", time.Time{})
		}
		return f, tg, p
	}
	t.Run("versions-checked", func(t *testing.T) {
		// ours is the 25th version in the listing: found; the 26th: not
		f, tg, p := build(t, 24, 0)
		mustSubmit(t, tg, p)
		f.close()
		f, tg, p = build(t, 25, 0)
		_, err := tg.Submit(context.Background(), p)
		expectErr(t, err, anchor.ClassConflict, "worm_conflict")
		f.close()
	})
	t.Run("pages", func(t *testing.T) {
		// one version per page: ours on the 5th page is found, on the 6th not
		f, tg, p := build(t, 4, 1)
		before := f.listCalls
		mustSubmit(t, tg, p)
		if n := f.listCalls - before; n != 5 {
			t.Errorf("%d list calls, want 5", n)
		}
		f.close()
		f, tg, p = build(t, 5, 1)
		before = f.listCalls
		_, err := tg.Submit(context.Background(), p)
		expectErr(t, err, anchor.ClassConflict, "worm_conflict")
		if n := f.listCalls - before; n != 5 {
			t.Errorf("%d list calls, want 5 (no sixth page)", n)
		}
		f.close()
	})
	t.Run("body", func(t *testing.T) {
		// a listing of exactly 1 MiB is read; one byte more is refused
		f, tg, p := build(t, 1, 0)
		defer f.close()
		f.listExact = 1 << 20
		mustSubmit(t, tg, p)
		f.listExact = 1<<20 + 1
		_, err := tg.Submit(context.Background(), p)
		if err == nil || anchor.CodeOf(err) == "worm_conflict" || !strings.Contains(err.Error(), "larger") {
			t.Fatalf("%v", err)
		}
	})
}
