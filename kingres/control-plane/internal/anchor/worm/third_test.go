// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package worm

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Tests of the third review (A3-01, A3-02, A3-08): the time of the store, the
// retention floor, the evidence members, the lock modes.

// vclock is a clock that a test moves, shared by the target and the fake store
// (the store adds its own skew).
type vclock struct {
	mu sync.Mutex
	t  time.Time
}

func newVclock() *vclock { return &vclock{t: time.Now().UTC().Truncate(time.Second)} }

func (c *vclock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *vclock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func withClock(c *vclock) opt { return func(_ *Config, o *Options) { o.Clock = c.now } }

// vstore is a fake store on the virtual clock, with the skew given.
func vstore(c *vclock, skew time.Duration) *fakeS3 {
	f := newFakeS3()
	f.clock, f.skew = c.now, skew
	return f
}

func startedAt(t *testing.T, size string, start time.Time) anchor.Payload {
	t.Helper()
	return payloadAt(t, size, start)
}

// A3-01, probe 1: the put succeeds and the HEAD of the read-back fails; the next
// Submit finds the object and judges it. The store is 20 minutes behind: the
// object is refused, whatever the age of the record. A caller that wrongly
// passes the creation of the record (here 30 minutes before the call) as the
// start is refused all the same, because the object carries the start of the
// call that wrote it.
func TestStoreBehindAfterATransientReadBackFailureIsRefused(t *testing.T) {
	for name, recordAge := range map[string]time.Duration{"start-is-the-call": 0, "record-30-minutes-old": 30 * time.Minute, "record-5-days-old": 5 * 24 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			clk := newVclock()
			f := vstore(clk, -20*time.Minute)
			defer f.close()
			tg := newTarget(t, f, withClock(clk))
			p := startedAt(t, "31", clk.now().Add(-recordAge))
			f.headFailures = 1
			_, err := tg.Submit(context.Background(), p)
			expectErr(t, err, anchor.ClassRetryable, "")
			if n := len(f.stored(keyOf("31"))); n != 1 {
				t.Fatalf("%d versions after the first call", n)
			}
			clk.advance(time.Minute)
			_, err = tg.Submit(context.Background(), p)
			ae := expectErr(t, err, anchor.ClassPermanent, "store_time_early")
			if !strings.Contains(ae.Error(), "clock") {
				t.Fatal(ae)
			}
			// and on every later attempt, after any delay
			clk.advance(6 * 24 * time.Hour)
			_, err = tg.Submit(context.Background(), p)
			expectErr(t, err, anchor.ClassPermanent, "store_time_early")
		})
	}
}

// A3-01, probe 2: refused for time, then Retry (which keeps the start, so the
// same payload): the second Submit is refused again, silently no more, however
// long the record sat failed.
func TestRetryAfterATimeRefusalIsRefusedAgain(t *testing.T) {
	for name, recordAge := range map[string]time.Duration{"young": 0, "2-hours-old": 2 * time.Hour, "5-days-old": 5 * 24 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			clk := newVclock()
			f := vstore(clk, -time.Hour)
			defer f.close()
			tg := newTarget(t, f, withClock(clk))
			p := startedAt(t, "32", clk.now().Add(-recordAge))
			_, err := tg.Submit(context.Background(), p)
			expectErr(t, err, anchor.ClassPermanent, "store_time_early")
			rec := anchor.NewRecord(1, "agg", 32, p.Digest, clk.now())
			failed, alert, aerr := rec.AfterError(err, clk.now(), 0.5, anchor.Limits{})
			if aerr != nil || failed.State != anchor.StateFailed || alert != anchor.AlertFailed {
				t.Fatalf("%+v %q %v", failed, alert, aerr)
			}
			// five days later the operator presses Retry
			clk.advance(5 * 24 * time.Hour)
			retried, err := failed.Retry(clk.now())
			if err != nil || retried.State != anchor.StatePending {
				t.Fatalf("%+v %v", retried, err)
			}
			_, err = tg.Submit(context.Background(), p)
			expectErr(t, err, anchor.ClassPermanent, "store_time_early")
			if n := len(f.stored(keyOf("32"))); n != 1 {
				t.Fatalf("%d versions", n)
			}
		})
	}
}

// A3-01, the store ahead: a fresh write is refused (class refused, retried after
// 1, 5 and 15 minutes), and the object stays refused on every later attempt,
// because it carries the start of the call that wrote it: the clock catching up
// does not make a time that is ahead of the write acceptable.
func TestStoreAheadStaysRefusedWhenTheClockCatchesUp(t *testing.T) {
	clk := newVclock()
	f := vstore(clk, 20*time.Minute)
	defer f.close()
	tg := newTarget(t, f, withClock(clk))
	p := startedAt(t, "33", clk.now())
	for i, wait := range []time.Duration{0, time.Minute, 5 * time.Minute, 15 * time.Minute, 24 * time.Hour} {
		clk.advance(wait)
		_, err := tg.Submit(context.Background(), p)
		expectErr(t, err, anchor.ClassRefused, "store_time_skew")
		if i == 3 && len(f.stored(keyOf("33"))) != 1 {
			t.Fatal("a second version was written")
		}
	}
}

// A3-01, the fresh rule against the existing rule (the rules differ, and a test
// pins it): the same store time, 30 minutes behind, with a start one hour before
// the call. As a version that this call writes it is refused (it must be within
// the tolerance of now); as a version found without the start of the call that
// wrote it (another principal's) it is accepted by the window of the start.
func TestFreshRuleIsTighterThanTheExistingRule(t *testing.T) {
	clk := newVclock()
	start := clk.now().Add(-time.Hour)
	// fresh: refused
	f := vstore(clk, -30*time.Minute)
	_, err := newTarget(t, f, withClock(clk)).Submit(context.Background(), startedAt(t, "34", start))
	expectErr(t, err, anchor.ClassPermanent, "store_time_early")
	f.close()
	// found, without the attempt metadata: accepted, and the evidence says the
	// start that was applied and that the version was found
	g := newFakeS3()
	g.clock = clk.now
	defer g.close()
	p := startedAt(t, "34", start)
	v := g.addVersion(keyOf("34"), p.SignedNote, "COMPLIANCE", clk.now().Add(30*24*time.Hour))
	g.mu.Lock()
	v.lastMod = clk.now().Add(-30 * time.Minute)
	g.mu.Unlock()
	rc, ev := mustSubmit(t, newTarget(t, g, withClock(clk)), p)
	if rc.State != anchor.ReceiptAnchored || !ev.Found || ev.SubmitStart != anchor.FormatTime(start) || ev.LastModified != anchor.FormatTime(clk.now().Add(-30*time.Minute)) {
		t.Fatalf("%+v", ev)
	}
	// the window of the start: one hour and 6 minutes before the call is refused
	h := newFakeS3()
	h.clock = clk.now
	defer h.close()
	w := h.addVersion(keyOf("34"), p.SignedNote, "COMPLIANCE", clk.now().Add(30*24*time.Hour))
	h.mu.Lock()
	w.lastMod = start.Add(-6 * time.Minute)
	h.mu.Unlock()
	_, err = newTarget(t, h, withClock(clk)).Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassPermanent, "store_time_early")
}

// A3-01 and A3-02: crash recovery completes after any delay. The first call
// wrote the object and the process died before the record was written; a day, a
// month or a year later (the cluster clock moves, the store's follows) the same
// payload finds the version and is anchored with the time that the store stated.
func TestCrashRecoveryCompletesAfterAnyDelay(t *testing.T) {
	for _, delay := range []time.Duration{time.Minute, 9 * 24 * time.Hour, 60 * 24 * time.Hour, 400 * 24 * time.Hour} {
		clk := newVclock()
		f := vstore(clk, 0)
		tg := newTarget(t, f, withClock(clk), func(c *Config, _ *Options) { c.RetentionDays = 3650 })
		p := startedAt(t, "35", clk.now())
		_, ev1 := mustSubmit(t, tg, p) // the receipt is lost
		clk.advance(delay)
		rc, ev2 := mustSubmit(t, tg, p)
		if rc.State != anchor.ReceiptAnchored || !ev2.Found || ev2.VersionID != ev1.VersionID || ev2.LastModified != ev1.LastModified || f.retentionCalls != 0 {
			t.Fatalf("delay %v: %+v", delay, ev2)
		}
		f.close()
	}
}

// A3-01: the operator repairs the bucket (locks the version the store left
// unlocked) and the record is retried much later: completed after any delay.
func TestOperatorRepairThenRetryAfterAnyDelay(t *testing.T) {
	for _, delay := range []time.Duration{time.Minute, 9 * 24 * time.Hour, 60 * 24 * time.Hour} {
		clk := newVclock()
		f := vstore(clk, 0)
		f.retentionStatus = 403
		tg := newTarget(t, f, withClock(clk))
		p := startedAt(t, "36", clk.now())
		v := f.addVersion(keyOf("36"), p.SignedNote, "", time.Time{}) // unlocked, written by the store
		_, err := tg.Submit(context.Background(), p)
		expectErr(t, err, anchor.ClassPermanent, "not_locked")
		clk.advance(delay)
		lock(f, v, "COMPLIANCE", clk.now().Add(30*24*time.Hour))
		rc, ev := mustSubmit(t, tg, p)
		if rc.State != anchor.ReceiptAnchored || ev.VersionID != v.id || !ev.Found {
			t.Fatalf("delay %v: %+v", delay, ev)
		}
		f.close()
	}
}

// A3-01: every put carries the start of the call that wrote it as user metadata
// (and so does the first probe object of Test, which is written as a first Submit
// writes: correction 105), and the evidence says what start was applied.
func TestPutCarriesTheStartOfTheCall(t *testing.T) {
	clk := newVclock()
	f := vstore(clk, 0)
	defer f.close()
	tg := newTarget(t, f, withClock(clk))
	start := clk.now().Add(-time.Minute)
	_, ev := mustSubmit(t, tg, startedAt(t, "37", start))
	if ev.SubmitStart != anchor.FormatTime(start) || ev.Found {
		t.Fatalf("%+v", ev)
	}
	put := f.requests()[0]
	if got := put.header.Get("X-Amz-Meta-Kingres-Attempt"); got != anchor.FormatTime(clk.now()) {
		t.Fatalf("attempt metadata %q, want %q", got, anchor.FormatTime(clk.now()))
	}
	if vs := f.stored(keyOf("37")); len(vs) != 1 || vs[0].attempt != anchor.FormatTime(clk.now()) {
		t.Fatal("the store did not keep it")
	}
	g := newFakeS3()
	defer g.close()
	res := newTarget(t, g).Test(context.Background())
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
	with := 0
	for _, r := range g.requests() {
		if r.method == "PUT" && r.header.Get("X-Amz-Meta-Kingres-Attempt") != "" {
			with++
		}
	}
	if with != 1 {
		t.Fatalf("%d probe puts carry the metadata, want the first only", with)
	}
}

// A3-02: the edges of the start. Whatever the caller states, an object that this
// library wrote is judged by the start of the call that wrote it too, so a start
// of 1970 does not widen its window; for a version found without that start the
// window is the stated start less the tolerance, and the evidence says which.
func TestSubmitStartEdges(t *testing.T) {
	ctx := context.Background()
	t.Run("1970-fresh-write-is-judged-by-now", func(t *testing.T) {
		clk := newVclock()
		f := vstore(clk, -time.Hour)
		defer f.close()
		_, err := newTarget(t, f, withClock(clk)).Submit(ctx, startedAt(t, "40", time.Unix(1, 0)))
		expectErr(t, err, anchor.ClassPermanent, "store_time_early")
	})
	t.Run("1970-own-object-is-judged-by-its-call", func(t *testing.T) {
		clk := newVclock()
		f := vstore(clk, -time.Hour)
		defer f.close()
		tg := newTarget(t, f, withClock(clk))
		p := startedAt(t, "41", time.Unix(1, 0))
		f.headFailures = 1
		_, err := tg.Submit(ctx, p)
		expectErr(t, err, anchor.ClassRetryable, "")
		_, err = tg.Submit(ctx, p)
		expectErr(t, err, anchor.ClassPermanent, "store_time_early")
	})
	t.Run("1970-foreign-object-gets-the-wide-window-and-the-evidence-says-so", func(t *testing.T) {
		clk := newVclock()
		f := vstore(clk, 0)
		defer f.close()
		p := startedAt(t, "42", time.Unix(1, 0))
		v := f.addVersion(keyOf("42"), p.SignedNote, "COMPLIANCE", clk.now().Add(30*24*time.Hour))
		f.mu.Lock()
		v.lastMod = clk.now().Add(-3 * 365 * 24 * time.Hour)
		f.mu.Unlock()
		_, ev := mustSubmit(t, newTarget(t, f, withClock(clk)), p)
		if ev.SubmitStart != "1970-01-01T00:00:01Z" || !ev.Found {
			t.Fatalf("%+v", ev)
		}
	})
	t.Run("a-year-ago", func(t *testing.T) {
		clk := newVclock()
		f := vstore(clk, 0)
		defer f.close()
		_, ev := mustSubmit(t, newTarget(t, f, withClock(clk)), startedAt(t, "43", clk.now().Add(-365*24*time.Hour)))
		if ev.Found || ev.SubmitStart != anchor.FormatTime(clk.now().Add(-365*24*time.Hour)) {
			t.Fatalf("%+v", ev)
		}
	})
	t.Run("now-plus-4-minutes-works-including-recovery", func(t *testing.T) {
		clk := newVclock()
		f := vstore(clk, 0)
		defer f.close()
		tg := newTarget(t, f, withClock(clk))
		p := startedAt(t, "44", clk.now().Add(4*time.Minute))
		mustSubmit(t, tg, p)
		clk.advance(time.Minute)
		if _, ev := mustSubmit(t, tg, p); !ev.Found {
			t.Fatalf("%+v", ev)
		}
	})
	t.Run("now-plus-6-minutes-sends-nothing", func(t *testing.T) {
		clk := newVclock()
		f := vstore(clk, 0)
		defer f.close()
		_, err := newTarget(t, f, withClock(clk)).Submit(ctx, startedAt(t, "45", clk.now().Add(6*time.Minute)))
		ae := expectErr(t, err, anchor.ClassRetryable, "submit_start_ahead")
		if !strings.Contains(ae.Error(), "clock") || len(f.requests()) != 0 {
			t.Fatalf("%v, %d requests", ae, len(f.requests()))
		}
	})
	t.Run("different-between-calls-changes-the-verdict", func(t *testing.T) {
		clk := newVclock()
		f := vstore(clk, 0)
		defer f.close()
		tg := newTarget(t, f, withClock(clk))
		p1 := startedAt(t, "46", clk.now().Add(-time.Hour))
		v := f.addVersion(keyOf("46"), p1.SignedNote, "COMPLIANCE", clk.now().Add(30*24*time.Hour))
		f.mu.Lock()
		v.lastMod = clk.now().Add(-3 * time.Minute)
		f.mu.Unlock()
		if _, ev := mustSubmit(t, tg, p1); ev.SubmitStart != anchor.FormatTime(clk.now().Add(-time.Hour)) {
			t.Fatalf("%+v", ev)
		}
		// the same object, a start 4 minutes ahead: the window moves with it and
		// the object is now earlier than it
		_, err := tg.Submit(ctx, startedAt(t, "46", clk.now().Add(4*time.Minute)))
		expectErr(t, err, anchor.ClassPermanent, "store_time_early")
	})
}

// A3-02: the retention floor comes from the store's own timeline. An existing
// locked version that expires in two minutes is not accepted because a stated
// start of 1970 makes the floor the past: it is repaired to the configured days.
func TestRetentionFloorIsNotTakenFromTheCaller(t *testing.T) {
	clk := newVclock()
	f := vstore(clk, 0)
	defer f.close()
	tg := newTarget(t, f, withClock(clk), func(c *Config, _ *Options) { c.RetentionDays = 3650 })
	p := startedAt(t, "47", time.Unix(1, 0))
	f.addVersion(keyOf("47"), p.SignedNote, "COMPLIANCE", clk.now().Add(2*time.Minute))
	_, ev := mustSubmit(t, tg, p)
	until, _ := anchor.ParseTime(ev.RetainUntil)
	if f.retentionCalls != 1 || until.Before(clk.now().Add(3650*24*time.Hour-10*time.Minute)) {
		t.Fatalf("retention calls %d, until %v", f.retentionCalls, until)
	}
}

// A3-08: the evidence says whether the version was written by this call or found,
// including the version that another principal wrote, which is adopted.
func TestEvidenceSaysFoundOrWritten(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "48", 1)
	if _, ev := mustSubmit(t, tg, p); ev.Found {
		t.Fatal("a fresh write says found")
	}
	if _, ev := mustSubmit(t, tg, p); !ev.Found {
		t.Fatal("the second call does not say found")
	}
	g := newFakeS3()
	defer g.close()
	q := testPayload(t, "49", 1)
	g.addVersion(keyOf("49"), q.SignedNote, "", time.Time{}) // a foreign, unlocked version
	if _, ev := mustSubmit(t, newTarget(t, g), q); !ev.Found {
		t.Fatal("an adopted foreign version does not say found")
	}
}

// A3-08: the text of the not-locked error is right for a lock that is too short
// or has expired, not only for a missing one.
func TestNotLockedTextCoversAShortLock(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	f.retentionStatus = 403
	p := testPayload(t, "50", 1)
	f.addVersion(keyOf("50"), p.SignedNote, "COMPLIANCE", time.Now().Add(time.Hour)) // 30 days configured
	_, err := newTarget(t, f).Submit(context.Background(), p)
	ae := expectErr(t, err, anchor.ClassPermanent, "not_locked")
	if !strings.Contains(ae.Error(), "too short or has expired") || strings.Contains(ae.Error(), "exists without a lock") {
		t.Fatal(ae)
	}
}

// A3-08: the modes. COMPLIANCE found where GOVERNANCE is configured is the
// stronger lock and is accepted; GOVERNANCE found where COMPLIANCE is
// configured is refused (a principal with the bypass permission could delete
// it). A short COMPLIANCE lock under a GOVERNANCE configuration is repaired in
// COMPLIANCE, never weakened.
func TestLockModesFound(t *testing.T) {
	ctx := context.Background()
	f := newFakeS3()
	defer f.close()
	p := testPayload(t, "51", 1)
	v := f.addVersion(keyOf("51"), p.SignedNote, "COMPLIANCE", time.Now().Add(31*24*time.Hour))
	_, ev := mustSubmit(t, newTarget(t, f, mode("governance")), p)
	if ev.LockMode != "COMPLIANCE" || ev.VersionID != v.id || f.retentionCalls != 0 {
		t.Fatalf("%+v", ev)
	}
	g := newFakeS3()
	defer g.close()
	q := testPayload(t, "52", 1)
	g.addVersion(keyOf("52"), q.SignedNote, "GOVERNANCE", time.Now().Add(31*24*time.Hour))
	_, err := newTarget(t, g, mode("compliance")).Submit(ctx, q)
	ae := expectErr(t, err, anchor.ClassPermanent, "not_locked")
	if !strings.Contains(ae.Error(), "GOVERNANCE") || !strings.Contains(ae.Error(), "weaker") {
		t.Fatal(ae)
	}
	if g.retentionCalls != 0 {
		t.Fatal("a governance version was touched")
	}
	h := newFakeS3()
	defer h.close()
	r := testPayload(t, "53", 1)
	h.addVersion(keyOf("53"), r.SignedNote, "COMPLIANCE", time.Now().Add(time.Hour)) // short
	_, ev = mustSubmit(t, newTarget(t, h, mode("governance")), r)
	if h.retentionCalls != 1 || ev.LockMode != "COMPLIANCE" {
		t.Fatalf("retention calls %d, %+v", h.retentionCalls, ev)
	}
}

// A3-01 without If-None-Match (the check-then-put path): the object also carries
// the start of its call, so a Retry with the creation of the record as the start
// is still refused for a store that is behind.
func TestStoreBehindWithoutIfNoneMatch(t *testing.T) {
	clk := newVclock()
	f := vstore(clk, -20*time.Minute)
	defer f.close()
	tg := newTarget(t, f, withClock(clk), inm("off"))
	p := startedAt(t, "38", clk.now().Add(-30*time.Minute))
	f.headFailures = 1
	_, err := tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassRetryable, "")
	clk.advance(time.Minute)
	_, err = tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassPermanent, "store_time_early")
}
