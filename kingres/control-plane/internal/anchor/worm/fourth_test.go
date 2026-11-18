// SPDX-License-Identifier: AGPL-3.0-only

package worm

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Tests of the fourth review (A4-01, A4-02, A4-03, A4-07; corrections 95 to 97).

func sameEvidence(a, b anchor.WORMEvidence) bool {
	x, err1 := anchor.EncodeEvidence(a)
	y, err2 := anchor.EncodeEvidence(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// offsetClock is the clock of the cluster: the true clock plus an offset that a
// test sets (the store keeps the true one).
func offsetClock(c *vclock, off *atomic.Int64) opt {
	return func(_ *Config, o *Options) {
		o.Clock = func() time.Time { return c.now().Add(time.Duration(off.Load())) }
	}
}

func submitStore(t *testing.T, f *fakeS3, tg *Target, p anchor.Payload) error {
	t.Helper()
	_, err := tg.Submit(context.Background(), p)
	return err
}

// A4-02 (1): a store that does not keep the attempt, or returns it changed, is
// found out at the read-back of the write: a code of its own, permanent, the
// object stays, and the text says what the operator does.
func TestFreshWriteRequiresTheAttemptEchoedAsSent(t *testing.T) {
	cases := map[string]func(f *fakeS3){
		"dropped":          func(f *fakeS3) { f.dropAttempt = true },
		"garbage":          func(f *fakeS3) { f.attemptRewrite = func(string) string { return "garbage" } },
		"another-valid":    func(f *fakeS3) { f.attemptRewrite = func(string) string { return "2020-01-01T00:00:00Z" } },
		"joined":           func(f *fakeS3) { f.attemptRewrite = func(a string) string { return a + "," + a } },
		"two-header-lines": func(f *fakeS3) { f.attemptExtra = []string{"2020-01-01T00:00:00Z"} },
		"lower-case-z":     func(f *fakeS3) { f.attemptRewrite = func(a string) string { return strings.ToLower(a) } },
		"fraction": func(f *fakeS3) {
			f.attemptRewrite = func(a string) string { return strings.TrimSuffix(a, "Z") + ".5Z" }
		},
		"offset-time-zone": func(f *fakeS3) {
			f.attemptRewrite = func(a string) string { return strings.TrimSuffix(a, "Z") + "+00:00" }
		},
		"leading-space":     func(f *fakeS3) { f.attemptRewrite = func(a string) string { return a + " x" } },
		"empty-after-write": func(f *fakeS3) { f.attemptRewrite = func(string) string { return "" } },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeS3()
			defer f.close()
			setup(f)
			tg := newTarget(t, f)
			_, err := tg.Submit(context.Background(), testPayload(t, "41", 1))
			ae := expectErr(t, err, anchor.ClassPermanent, "no_user_metadata")
			for _, s := range []string{"does not keep user metadata", "stays", "Retry"} {
				if !strings.Contains(ae.Error(), s) {
					t.Fatalf("text lacks %q: %v", s, ae)
				}
			}
			if n := len(f.stored(keyOf("41"))); n != 1 {
				t.Fatalf("%d versions", n)
			}
			noSigErrors(t, f)
		})
	}
	// the control: a store that keeps the metadata is not refused
	f := newFakeS3()
	defer f.close()
	if _, ev := mustSubmit(t, newTarget(t, f), testPayload(t, "41", 1)); ev.Found || ev.Attempt == nil {
		t.Fatalf("%+v", ev)
	}
}

// A4-02 (1), what the operator can do: after the refusal a Retry finds the object
// and accepts it under the wide window of the start, and the evidence says that no
// attempt was used (attempt null). The record does not spin: the refusal is
// permanent, so nothing retries by itself.
func TestRetryAfterNoUserMetadataStatesNullAttempt(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	f.dropAttempt = true
	tg := newTarget(t, f)
	p := testPayload(t, "42", 1)
	err := submitStore(t, f, tg, p)
	expectErr(t, err, anchor.ClassPermanent, "no_user_metadata")
	rc, ev := mustSubmit(t, tg, p)
	if rc.State != anchor.ReceiptAnchored || !ev.Found || ev.Attempt != nil {
		t.Fatalf("%+v", ev)
	}
	if n := len(f.stored(keyOf("42"))); n != 1 {
		t.Fatalf("%d versions", n)
	}
}

// A4-02 (2): a value that is present and malformed is an error for that version,
// never "absent": the five forms of the review, found after a good write.
func TestMalformedAttemptOfAFoundVersionIsAnError(t *testing.T) {
	forms := map[string]func(f *fakeS3){
		"garbage": func(f *fakeS3) { f.attemptRewrite = func(string) string { return "garbage" } },
		"offset-zone": func(f *fakeS3) {
			f.attemptRewrite = func(a string) string { return strings.TrimSuffix(a, "Z") + "+00:00" }
		},
		"fraction": func(f *fakeS3) {
			f.attemptRewrite = func(a string) string { return strings.TrimSuffix(a, "Z") + ".5Z" }
		},
		"lower-case-z":     func(f *fakeS3) { f.attemptRewrite = func(a string) string { return strings.ToLower(a) } },
		"joined":           func(f *fakeS3) { f.attemptRewrite = func(a string) string { return a + "," + a } },
		"two-header-lines": func(f *fakeS3) { f.attemptExtra = []string{"2020-01-01T00:00:00Z"} },
		"empty":            func(f *fakeS3) { f.attemptRewrite = func(string) string { return "" } },
	}
	for name, setup := range forms {
		t.Run(name, func(t *testing.T) {
			clk := newVclock()
			f := vstore(clk, 0)
			defer f.close()
			tg := newTarget(t, f, withClock(clk))
			p := startedAt(t, "43", clk.now())
			mustSubmit(t, tg, p) // our version, written well
			setup(f)             // the store now returns the header in another form
			clk.advance(time.Minute)
			_, err := tg.Submit(context.Background(), p)
			ae := expectErr(t, err, anchor.ClassPermanent, "attempt_malformed")
			if !strings.Contains(ae.Error(), "generation") {
				t.Fatalf("no way out in the text: %v", ae)
			}
		})
	}
	// the control: the header absent is a version without the metadata (accepted,
	// attempt null)
	clk := newVclock()
	f := vstore(clk, 0)
	defer f.close()
	tg := newTarget(t, f, withClock(clk))
	p := startedAt(t, "43", clk.now())
	mustSubmit(t, tg, p)
	f.dropAttempt = true
	clk.advance(time.Minute)
	_, ev := mustSubmit(t, tg, p)
	if !ev.Found || ev.Attempt != nil {
		t.Fatalf("%+v", ev)
	}
}

// A4-02 (3): the evidence states which window was applied.
func TestEvidenceStatesTheAttemptThatWasUsed(t *testing.T) {
	clk := newVclock()
	f := vstore(clk, 0)
	defer f.close()
	tg := newTarget(t, f, withClock(clk))
	p := startedAt(t, "44", clk.now())
	first := anchor.FormatTime(clk.now())
	_, ev1 := mustSubmit(t, tg, p)
	if ev1.Found || ev1.Attempt == nil || *ev1.Attempt != first {
		t.Fatalf("fresh: %+v", ev1)
	}
	clk.advance(2 * time.Hour)
	_, ev2 := mustSubmit(t, tg, p)
	if !ev2.Found || ev2.Attempt == nil || *ev2.Attempt != first {
		t.Fatalf("found with the metadata must state the attempt of the call that wrote it, not of this call: %+v", ev2)
	}
	// a version of another principal: no metadata, null
	k := keyOf("45")
	q := startedAt(t, "45", clk.now())
	v := f.addVersion(k, q.SignedNote, "COMPLIANCE", clk.now().Add(30*24*time.Hour))
	_ = v
	_, ev3 := mustSubmit(t, tg, q)
	if !ev3.Found || ev3.Attempt != nil {
		t.Fatalf("foreign: %+v", ev3)
	}
	// the probe of the review: store 24 h behind, record 48 h old, the put succeeds,
	// one HEAD fails, the metadata is gone from the stored version: the wide window
	// applies (as documented) and the evidence now says so
	clk2 := newVclock()
	g := vstore(clk2, -24*time.Hour)
	defer g.close()
	tg2 := newTarget(t, g, withClock(clk2))
	r := startedAt(t, "46", clk2.now().Add(-48*time.Hour))
	g.headFailures = 1
	_, err := tg2.Submit(context.Background(), r)
	expectErr(t, err, anchor.ClassRetryable, "")
	g.dropAttempt = true
	clk2.advance(time.Minute)
	_, ev4 := mustSubmit(t, tg2, r)
	if !ev4.Found || ev4.Attempt != nil {
		t.Fatalf("%+v", ev4)
	}
	// and with the metadata kept the same store is refused (the narrow window)
	clk3 := newVclock()
	h := vstore(clk3, -24*time.Hour)
	defer h.close()
	tg3 := newTarget(t, h, withClock(clk3))
	s := startedAt(t, "46", clk3.now().Add(-48*time.Hour))
	h.headFailures = 1
	_, err = tg3.Submit(context.Background(), s)
	expectErr(t, err, anchor.ClassRetryable, "")
	clk3.advance(time.Minute)
	_, err = tg3.Submit(context.Background(), s)
	expectErr(t, err, anchor.ClassPermanent, "store_time_early")
}

// A4-02 (4): what the fresh rule does when the metadata is lost. The store is 24
// hours behind and the record 48 hours old, and the store keeps no metadata: the
// time of the fresh write is refused by the fresh rule (the tolerance around the
// clock of the call), whatever the store does with the header. A fresh write
// judged like a found one would take the wide window of the start (48 hours),
// pass the time, and be refused for its metadata instead.
func TestFreshRuleDoesNotDependOnTheMetadata(t *testing.T) {
	clk := newVclock()
	f := vstore(clk, -24*time.Hour)
	defer f.close()
	f.dropAttempt = true
	tg := newTarget(t, f, withClock(clk))
	_, err := tg.Submit(context.Background(), startedAt(t, "47", clk.now().Add(-48*time.Hour)))
	expectErr(t, err, anchor.ClassPermanent, "store_time_early")
	// and a store that is on time but drops the metadata is refused for that
	clk2 := newVclock()
	g := vstore(clk2, 0)
	defer g.close()
	g.dropAttempt = true
	_, err = newTarget(t, g, withClock(clk2)).Submit(context.Background(), startedAt(t, "47", clk2.now().Add(-48*time.Hour)))
	expectErr(t, err, anchor.ClassPermanent, "no_user_metadata")
}

// A4-01 (a) and (c): the cluster clock wrong for one call, in each direction,
// then right; then a Retry (refused, with the true text); then the next
// generation of the key (anchored).
func TestClockFaultOfOneCallRetryThenNewGeneration(t *testing.T) {
	t.Run("clock-behind", func(t *testing.T) {
		clk := newVclock()
		f := vstore(clk, 0)
		defer f.close()
		var off atomic.Int64
		tg := newTarget(t, f, withClock(clk), offsetClock(clk, &off))
		off.Store(int64(-10 * time.Minute))
		wrongStart := clk.now().Add(-10 * time.Minute)
		p := startedAt(t, "51", wrongStart)
		err := submitStore(t, f, tg, p)
		ae := expectErr(t, err, anchor.ClassRefused, "store_time_skew")
		for _, s := range []string{"waiting and a Retry find the same object", "new key generation", "keyPrefix"} {
			if !strings.Contains(ae.Error(), s) {
				t.Fatalf("text lacks %q: %v", s, ae)
			}
		}
		if strings.Contains(ae.Error(), "then Retry") {
			t.Fatalf("the text still promises a Retry that cannot work: %v", ae)
		}
		// the clock is right again; the operator presses Retry: the same refusal
		off.Store(0)
		for _, wait := range []time.Duration{time.Minute, 48 * time.Hour} {
			clk.advance(wait)
			err = submitStore(t, f, tg, p)
			ae = expectErr(t, err, anchor.ClassRefused, "store_time_skew")
			if !strings.Contains(ae.Error(), "waiting and a Retry find the same object") {
				t.Fatal(ae)
			}
		}
		if n := len(f.stored(keyOf("51"))); n != 1 {
			t.Fatalf("%d versions", n)
		}
		// the next generation, with a new start, is anchored
		p1, err := p.WithGeneration(1, clk.now())
		if err != nil {
			t.Fatal(err)
		}
		rc, ev := mustSubmit(t, tg, p1)
		if rc.State != anchor.ReceiptAnchored || ev.Found || ev.Generation != 1 || !strings.HasSuffix(ev.Key, "/00000000000000000051.g1.note") {
			t.Fatalf("%+v", ev)
		}
		if n := len(f.stored(keyOf("51"))); n != 1 {
			t.Fatalf("generation 0 changed: %d versions", n)
		}
		if err := VerifyNote(ev, aggID, p1.SignedNote, p1.DigestHex()); err != nil {
			t.Fatal(err)
		}
		bad := ev
		bad.Generation = 0
		if VerifyNote(bad, aggID, p1.SignedNote, p1.DigestHex()) == nil {
			t.Fatal("the key of generation 1 was accepted as the key of generation 0")
		}
	})
	t.Run("clock-ahead", func(t *testing.T) {
		clk := newVclock()
		f := vstore(clk, 0)
		defer f.close()
		var off atomic.Int64
		tg := newTarget(t, f, withClock(clk), offsetClock(clk, &off))
		off.Store(int64(10 * time.Minute))
		wrongStart := clk.now().Add(10 * time.Minute)
		p := startedAt(t, "52", wrongStart)
		err := submitStore(t, f, tg, p)
		ae := expectErr(t, err, anchor.ClassPermanent, "store_time_early")
		if !strings.Contains(ae.Error(), "new key generation") {
			t.Fatal(ae)
		}
		off.Store(0)
		// the persisted start is the wrong clock's and still ahead
		err = submitStore(t, f, tg, p)
		expectErr(t, err, anchor.ClassRetryable, "submit_start_ahead")
		clk.advance(6 * time.Minute)
		err = submitStore(t, f, tg, p)
		ae = expectErr(t, err, anchor.ClassPermanent, "store_time_early")
		if !strings.Contains(ae.Error(), "new key generation") {
			t.Fatal(ae)
		}
		p1, err := p.WithGeneration(1, clk.now())
		if err != nil {
			t.Fatal(err)
		}
		rc, ev := mustSubmit(t, tg, p1)
		if rc.State != anchor.ReceiptAnchored || ev.Found || ev.Generation != 1 {
			t.Fatalf("%+v", ev)
		}
		// generation 2 after a further fault is its own key again
		p2, err := p1.WithGeneration(2, clk.now())
		if err != nil {
			t.Fatal(err)
		}
		_, ev2 := mustSubmit(t, tg, p2)
		if ev2.Generation != 2 || !strings.HasSuffix(ev2.Key, ".g2.note") || ev2.VersionID == ev.VersionID {
			t.Fatalf("%+v", ev2)
		}
	})
	// an object without the start of its call (another principal's) that is ahead
	// can be accepted once the clock has caught up: the text keeps saying Retry
	t.Run("found-without-metadata-ahead-heals", func(t *testing.T) {
		clk := newVclock()
		f := vstore(clk, 0)
		defer f.close()
		tg := newTarget(t, f, withClock(clk))
		p := startedAt(t, "53", clk.now())
		v := f.addVersion(keyOf("53"), p.SignedNote, "COMPLIANCE", clk.now().Add(31*24*time.Hour))
		f.mu.Lock()
		v.lastMod = clk.now().Add(20 * time.Minute)
		f.mu.Unlock()
		err := submitStore(t, f, tg, p)
		ae := expectErr(t, err, anchor.ClassRefused, "store_time_skew")
		if !strings.Contains(ae.Error(), "then Retry") || strings.Contains(ae.Error(), "waiting and a Retry find the same object") {
			t.Fatal(ae)
		}
		clk.advance(16 * time.Minute)
		if rc, _ := mustSubmit(t, tg, p); rc.State != anchor.ReceiptAnchored {
			t.Fatal(rc.State)
		}
	})
}

func TestObjectKeyGeneration(t *testing.T) {
	if k := ObjectKeyGen("/p/", "ab", 17, 0); k != ObjectKey("/p/", "ab", 17) || k != "p/ab/00000000000000000017.note" {
		t.Fatal(k)
	}
	if k := ObjectKeyGen("p", "ab", 17, 1); k != "p/ab/00000000000000000017.g1.note" {
		t.Fatal(k)
	}
	if k := ObjectKeyGen("p", "ab", 18446744073709551615, 1000); k != "p/ab/18446744073709551615.g1000.note" {
		t.Fatal(k)
	}
	// generations are distinct keys, and no key is the prefix of another
	seen := map[string]bool{}
	for g := uint32(0); g <= 12; g++ {
		k := ObjectKeyGen("p", "ab", 3, g)
		if seen[k] {
			t.Fatal(k)
		}
		for o := range seen {
			if strings.HasPrefix(o, k) || strings.HasPrefix(k, o) {
				t.Fatalf("%q and %q", o, k)
			}
		}
		seen[k] = true
	}
}

// A4-03: the latest version with our bytes does not decide alone. The three
// probes of the review: our version written and valid, then a foreign copy of the
// note placed after it that is unlocked (the store refuses the retention call),
// that carries a forged attempt in the future, or that is locked in a weaker mode.
func TestForeignLaterCopyOfOurBytesDoesNotEndTheRecord(t *testing.T) {
	type probe struct {
		name  string
		setup func(clk *vclock, f *fakeS3, key string, p anchor.Payload)
		cfg   []opt
	}
	probes := []probe{
		{"unlocked-and-the-store-refuses-retention", func(clk *vclock, f *fakeS3, key string, p anchor.Payload) {
			f.addVersion(key, p.SignedNote, "", time.Time{})
			f.retentionStatus = 403
		}, nil},
		{"forged-future-attempt", func(clk *vclock, f *fakeS3, key string, p anchor.Payload) {
			v := f.addVersion(key, p.SignedNote, "COMPLIANCE", clk.now().Add(30*24*time.Hour))
			f.mu.Lock()
			v.attempt = anchor.FormatTime(clk.now().Add(time.Hour))
			f.mu.Unlock()
		}, nil},
		{"weaker-mode", func(clk *vclock, f *fakeS3, key string, p anchor.Payload) {
			f.addVersion(key, p.SignedNote, "GOVERNANCE", clk.now().Add(30*24*time.Hour))
		}, nil},
		{"malformed-attempt", func(clk *vclock, f *fakeS3, key string, p anchor.Payload) {
			v := f.addVersion(key, p.SignedNote, "COMPLIANCE", clk.now().Add(30*24*time.Hour))
			f.mu.Lock()
			v.attempt = "garbage"
			f.mu.Unlock()
		}, nil},
	}
	for _, pr := range probes {
		t.Run(pr.name, func(t *testing.T) {
			clk := newVclock()
			f := vstore(clk, 0)
			defer f.close()
			tg := newTarget(t, f, withClock(clk))
			p := startedAt(t, "61", clk.now())
			_, ev1 := mustSubmit(t, tg, p)
			clk.advance(10 * time.Minute)
			pr.setup(clk, f, keyOf("61"), p)
			if n := len(f.stored(keyOf("61"))); n != 2 {
				t.Fatalf("%d versions", n)
			}
			rc, ev2 := mustSubmit(t, tg, p)
			if rc.State != anchor.ReceiptAnchored || ev2.VersionID != ev1.VersionID || !ev2.Found {
				t.Fatalf("the record was not completed with our version: %+v", ev2)
			}
		})
	}
	// every copy failing: the error is that of the latest, and it is not a conflict
	clk := newVclock()
	f := vstore(clk, 0)
	defer f.close()
	tg := newTarget(t, f, withClock(clk))
	p := startedAt(t, "62", clk.now())
	f.addVersion(keyOf("62"), p.SignedNote, "", time.Time{})
	f.addVersion(keyOf("62"), p.SignedNote, "GOVERNANCE", clk.now().Add(30*24*time.Hour))
	f.retentionStatus = 403
	err := submitStore(t, f, tg, p)
	expectErr(t, err, anchor.ClassPermanent, "not_locked")
	// bounded: the versions read are at most MaxVersionsChecked
	clk2 := newVclock()
	g := vstore(clk2, 0)
	defer g.close()
	tg2 := newTarget(t, g, withClock(clk2))
	q := startedAt(t, "63", clk2.now())
	mustSubmit(t, tg2, q)
	for i := 0; i < MaxVersionsChecked+3; i++ {
		g.addVersion(keyOf("63"), q.SignedNote, "GOVERNANCE", clk2.now().Add(30*24*time.Hour))
	}
	g.mu.Lock()
	before := len(g.reqs)
	g.mu.Unlock()
	err = submitStore(t, g, tg2, q)
	expectErr(t, err, anchor.ClassPermanent, "not_locked")
	gets := 0
	for _, r := range g.requests()[before:] {
		if r.method == "GET" {
			gets++
		}
	}
	if gets > MaxVersionsChecked+MaxListPages+2 {
		t.Fatalf("%d GET requests: not bounded", gets)
	}
}

// A4-07: a found version older than its own retention period gets its retention
// extended to now plus the configured days, like a fresh anchor, and is verified
// again; the old floor in the past let a lock that ends in a minute pass.
func TestOldFoundObjectIsExtendedToNowPlusTheDays(t *testing.T) {
	oneDay := func(c *Config, _ *Options) { c.RetentionDays = 1 }
	setup := func(t *testing.T) (*vclock, *fakeS3, *Target, anchor.Payload, anchor.WORMEvidence) {
		clk := newVclock()
		f := vstore(clk, 0)
		tg := newTarget(t, f, withClock(clk), oneDay)
		p := startedAt(t, "71", clk.now())
		_, ev := mustSubmit(t, tg, p)
		return clk, f, tg, p, ev
	}
	t.Run("lock-ends-in-a-minute-is-repaired", func(t *testing.T) {
		clk, f, tg, p, ev1 := setup(t)
		defer f.close()
		clk.advance(24*time.Hour - time.Minute) // the lock ends in one minute
		rc, ev := mustSubmit(t, tg, p)
		want := anchor.FormatTime(clk.now().Add(24 * time.Hour))
		if rc.State != anchor.ReceiptAnchored || !ev.Found || f.retentionCalls != 1 || ev.RetainUntil != want || ev.VersionID != ev1.VersionID {
			t.Fatalf("calls %d: %+v want retain %s", f.retentionCalls, ev, want)
		}
		if got := f.stored(keyOf("71"))[0].retain; !strings.HasPrefix(got, strings.TrimSuffix(want, "Z")) {
			t.Fatalf("stored %s want %s", got, want)
		}
	})
	t.Run("the-store-refuses-the-repair", func(t *testing.T) {
		clk, f, tg, p, _ := setup(t)
		defer f.close()
		clk.advance(24*time.Hour - time.Minute)
		f.retentionStatus = 403
		err := submitStore(t, f, tg, p)
		ae := expectErr(t, err, anchor.ClassPermanent, "not_locked")
		if f.retentionCalls != 1 || !strings.Contains(ae.Error(), "apply") {
			t.Fatalf("calls %d: %v", f.retentionCalls, ae)
		}
	})
	t.Run("a-lock-that-covers-now-plus-the-days-is-not-touched", func(t *testing.T) {
		clk, f, tg, p, _ := setup(t)
		defer f.close()
		clk.advance(2 * 24 * time.Hour) // older than its retention period
		lock(f, f.stored(keyOf("71"))[0], "COMPLIANCE", clk.now().Add(24*time.Hour-time.Minute))
		_, ev := mustSubmit(t, tg, p)
		if f.retentionCalls != 0 || !ev.Found {
			t.Fatalf("calls %d: %+v", f.retentionCalls, ev)
		}
	})
	t.Run("an-expired-lock-is-extended-too", func(t *testing.T) {
		clk, f, tg, p, _ := setup(t)
		defer f.close()
		clk.advance(3 * 24 * time.Hour)
		_, ev := mustSubmit(t, tg, p)
		if f.retentionCalls != 1 || ev.RetainUntil != anchor.FormatTime(clk.now().Add(24*time.Hour)) {
			t.Fatalf("calls %d: %+v", f.retentionCalls, ev)
		}
	})
	t.Run("a-young-object-keeps-the-floor-of-its-own-time", func(t *testing.T) {
		clk, f, tg, p, _ := setup(t)
		defer f.close()
		clk.advance(time.Hour)
		_, ev := mustSubmit(t, tg, p)
		if f.retentionCalls != 0 || !ev.Found {
			t.Fatalf("calls %d: %+v", f.retentionCalls, ev)
		}
	})
}

// A repair is offered for a found version whose lock is too short (the mutant
// "repair never offered" of the review): one PutObjectRetention call on that
// version, and the evidence names the extended date.
func TestRepairIsOfferedForAShortLock(t *testing.T) {
	clk := newVclock()
	f := vstore(clk, 0)
	defer f.close()
	tg := newTarget(t, f, withClock(clk)) // 30 days
	p := startedAt(t, "72", clk.now())
	mustSubmit(t, tg, p)
	clk.advance(time.Hour)
	lock(f, f.stored(keyOf("72"))[0], "COMPLIANCE", clk.now().Add(2*time.Hour))
	rc, ev := mustSubmit(t, tg, p)
	if rc.State != anchor.ReceiptAnchored || f.retentionCalls != 1 || ev.RetainUntil != anchor.FormatTime(clk.now().Add(30*24*time.Hour)) {
		t.Fatalf("calls %d: %+v", f.retentionCalls, ev)
	}
}
