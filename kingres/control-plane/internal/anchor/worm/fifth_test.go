// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package worm

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Tests of the fifth review (A5-01, A5-02, A5-06, A5-09, A5-10; correction 105).

// A5-01: a Retry after each of the three forms of the refusal no_user_metadata
// does what the text of the refusal says. The header dropped: the Retry accepts
// the object under the wide window (attempt null). The header changed or repeated:
// the Retry is refused for good, the text of that refusal does not send the
// operator to a new generation, and no second object is written.
func TestRetryAfterEachFormOfNoUserMetadata(t *testing.T) {
	forms := []struct {
		name  string
		setup func(f *fakeS3)
		drop  bool
	}{
		{"dropped", func(f *fakeS3) { f.dropAttempt = true }, true},
		{"changed", func(f *fakeS3) { f.attemptRewrite = func(string) string { return "garbage" } }, false},
		{"repeated", func(f *fakeS3) { f.attemptExtra = []string{"2020-01-01T00:00:00Z"} }, false},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			f := newFakeS3()
			defer f.close()
			form.setup(f)
			tg := newTarget(t, f)
			p := testPayload(t, "81", 1)
			_, err := tg.Submit(context.Background(), p)
			first := expectErr(t, err, anchor.ClassPermanent, "no_user_metadata")
			rc, err := tg.Submit(context.Background(), p)
			if form.drop {
				if err != nil || !strings.Contains(first.Error(), "accepts it under the wider window") || strings.Contains(first.Error(), "refused for good") {
					t.Fatalf("the text of the first refusal does not describe the dropped form: %v / %v", first, err)
				}
				ev, perr := anchor.ParseWORMEvidence(rc.Evidence)
				if perr != nil || rc.State != anchor.ReceiptAnchored || !ev.Found || ev.Attempt != nil {
					t.Fatalf("%+v", ev)
				}
			} else {
				if !strings.Contains(first.Error(), "refused for good") || strings.Contains(first.Error(), "accepts it under the wider window") {
					t.Fatalf("the text of the first refusal promises a Retry that is refused: %v", first)
				}
				second := expectErr(t, err, anchor.ClassPermanent, "attempt_malformed")
				// the text names both causes and says that a new generation does not
				// help when the cause is the store
				for _, s := range []string{"If another principal wrote it", "If this package wrote it", "a new key generation does not help", "only another store"} {
					if !strings.Contains(second.Error(), s) {
						t.Fatalf("text lacks %q: %v", s, second)
					}
				}
			}
			if n := len(f.stored(keyOf("81"))); n != 1 {
				t.Fatalf("%d versions: a Retry must not write another object", n)
			}
			noSigErrors(t, f)
		})
	}
}

func checkOf(t *testing.T, r anchor.TestResult, name string) anchor.Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, r.Checks)
	return anchor.Check{}
}

// A5-02: the Test button exercises what the first Submit will meet.
func TestTestButtonExercisesWhatSubmitMeets(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		f := newFakeS3()
		defer f.close()
		r := newTarget(t, f).Test(context.Background())
		if !r.OK() {
			t.Fatalf("%+v", r)
		}
		for _, n := range []string{"user-metadata", "store-time", "retention"} {
			if c := checkOf(t, r, n); c.Status != anchor.CheckOK {
				t.Fatalf("%s: %+v", n, c)
			}
		}
		// the cost of the probe: one version under the probe prefix, locked by the
		// retention check, and one retention request
		if f.retentionCalls != 1 {
			t.Fatalf("%d retention calls", f.retentionCalls)
		}
		n := 0
		for k, vs := range f.objects {
			if strings.Contains(k, "/_probe/") {
				n += len(vs)
				for _, v := range vs {
					if v.mode != "COMPLIANCE" || v.retain == "" {
						t.Fatalf("the probe version is not locked by the check: %+v", v)
					}
				}
			}
		}
		if n != 1 {
			t.Fatalf("%d probe versions", n)
		}
		noSigErrors(t, f)
	})
	t.Run("metadata", func(t *testing.T) {
		forms := map[string]func(f *fakeS3){
			"dropped":  func(f *fakeS3) { f.dropAttempt = true },
			"changed":  func(f *fakeS3) { f.attemptRewrite = func(string) string { return "garbage" } },
			"repeated": func(f *fakeS3) { f.attemptExtra = []string{"2020-01-01T00:00:00Z"} },
		}
		for name, setup := range forms {
			f := newFakeS3()
			setup(f)
			r := newTarget(t, f).Test(context.Background())
			f.close()
			c := checkOf(t, r, "user-metadata")
			if c.Status != anchor.CheckFailed || r.OK() || !strings.Contains(c.Text, "no_user_metadata") {
				t.Fatalf("%s: %+v", name, c)
			}
		}
	})
	t.Run("store-time", func(t *testing.T) {
		for _, tc := range []struct {
			skew time.Duration
			want string
		}{
			{10 * time.Minute, anchor.CheckFailed}, {-10 * time.Minute, anchor.CheckFailed},
			{time.Minute, anchor.CheckOK}, {-time.Minute, anchor.CheckOK},
		} {
			f := newFakeS3()
			f.skew = tc.skew
			r := newTarget(t, f).Test(context.Background())
			f.close()
			c := checkOf(t, r, "store-time")
			if c.Status != tc.want || r.OK() != (tc.want == anchor.CheckOK) {
				t.Fatalf("skew %v: %+v", tc.skew, c)
			}
		}
	})
	t.Run("retention-refused-is-a-warning", func(t *testing.T) {
		f := newFakeS3()
		defer f.close()
		f.retentionStatus = 403
		r := newTarget(t, f).Test(context.Background())
		c := checkOf(t, r, "retention")
		if c.Status != anchor.CheckWarning || !r.OK() || !strings.Contains(c.Text, "PutObjectRetention") || !strings.Contains(c.Text, "repair") {
			t.Fatalf("%+v", c)
		}
	})
}

// A5-09: a refusal whose verdict is final (an object that carries its attempt)
// fails the record at once, with the alert of a failed record; a refusal that
// can heal keeps its waits.
func TestFinalRefusalFailsTheRecordAtOnce(t *testing.T) {
	clk := newVclock()
	f := vstore(clk, 10*time.Minute) // the store is ahead: the fresh write is refused
	defer f.close()
	tg := newTarget(t, f, withClock(clk))
	p := startedAt(t, "82", clk.now())
	_, err := tg.Submit(context.Background(), p)
	ae := expectErr(t, err, anchor.ClassRefused, "store_time_skew")
	if !ae.Final || !anchor.IsFinal(err) {
		t.Fatalf("the refusal of an object that carries its attempt is not marked final: %+v", ae)
	}
	rec := anchor.NewRecord(1, "log", 82, p.Digest, clk.now())
	out, alert, aerr := rec.AfterError(err, clk.now(), 0.5, anchor.Limits{})
	if aerr != nil || out.State != anchor.StateFailed || alert != anchor.AlertFailed || out.NextAttemptAt != nil {
		t.Fatalf("%+v %q %v", out, alert, aerr)
	}
	// an object without the attempt that is ahead can heal: not final, waits
	f2 := vstore(clk, 0)
	defer f2.close()
	tg2 := newTarget(t, f2, withClock(clk))
	q := startedAt(t, "83", clk.now())
	f2.skew = 10 * time.Minute
	f2.addVersion(keyOf("83"), q.SignedNote, "COMPLIANCE", clk.now().Add(40*24*time.Hour))
	_, err = tg2.Submit(context.Background(), q)
	ae = expectErr(t, err, anchor.ClassRefused, "store_time_skew")
	if ae.Final {
		t.Fatalf("a found object without the attempt is marked final: %+v", ae)
	}
	out, alert, aerr = rec.AfterError(err, clk.now(), 0.5, anchor.Limits{})
	if aerr != nil || out.State != anchor.StatePending || alert != anchor.AlertNone || out.NextAttemptAt == nil {
		t.Fatalf("%+v %q %v", out, alert, aerr)
	}
}

// versions of our bytes, newest last, each failing in its own way (A5-10, M24):
// the error that comes back is that of the latest version.
func TestErrorOfTheLatestVersionComesBackWhenEveryVersionFails(t *testing.T) {
	clk := newVclock()
	f := vstore(clk, 0)
	defer f.close()
	tg := newTarget(t, f, withClock(clk))
	p := startedAt(t, "84", clk.now())
	key := keyOf("84")
	f.addVersion(key, p.SignedNote, "", time.Time{}) // the repair is refused below
	f.addVersion(key, p.SignedNote, "GOVERNANCE", clk.now().Add(30*24*time.Hour))
	latest := f.addVersion(key, p.SignedNote, "COMPLIANCE", clk.now().Add(30*24*time.Hour))
	latest.attempt = "garbage"
	f.retentionStatus = 403
	_, err := tg.Submit(context.Background(), p)
	ae := expectErr(t, err, anchor.ClassPermanent, "attempt_malformed")
	if !strings.Contains(ae.Error(), latest.id) {
		t.Fatalf("the error is not that of the latest version %s: %v", latest.id, ae)
	}
}

// cancelRT cancels the context of the call after the first HEAD that follows the
// listing of the versions, and counts the requests that are made after that.
type cancelRT struct {
	base   http.RoundTripper
	mu     sync.Mutex
	cancel context.CancelFunc
	listed bool
	done   bool
	after  int
}

func (c *cancelRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	if c.done {
		c.after++
	}
	c.mu.Unlock()
	resp, err := c.base.RoundTrip(req)
	c.mu.Lock()
	if strings.Contains(req.URL.RawQuery, "versions") {
		c.listed = true
	} else if err == nil && req.Method == http.MethodHead && c.listed && !c.done {
		c.done = true
		c.cancel()
	}
	c.mu.Unlock()
	return resp, err
}

// A5-10, M25: when the context ends during the walk over the versions, the walk
// stops: no further version is looked at.
func TestWalkStopsWhenTheContextEnds(t *testing.T) {
	clk := newVclock()
	f := vstore(clk, 0)
	defer f.close()
	rt := &cancelRT{base: http.DefaultTransport}
	tg := newTarget(t, f, withClock(clk), withClient(&http.Client{Transport: rt}))
	p := startedAt(t, "85", clk.now())
	key := keyOf("85")
	for i := 0; i < 5; i++ {
		f.addVersion(key, p.SignedNote, "GOVERNANCE", clk.now().Add(30*24*time.Hour))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.cancel = cancel
	_, err := tg.Submit(ctx, p)
	if err == nil || !rt.done {
		t.Fatalf("%v (cancelled: %v)", err, rt.done)
	}
	if rt.after != 0 {
		t.Fatalf("%d requests after the context ended: the walk went on", rt.after)
	}
}

// A5-06, the probe of the review: copies of our bytes with a malformed attempt
// placed after ours. Up to the bound the record completes; at the bound the error
// says that the bound was reached and that our version may be hidden, and it does
// not send the operator to a new generation.
func TestCopiesOfOurBytesHidingOurVersion(t *testing.T) {
	run := func(t *testing.T, copies int) error {
		clk := newVclock()
		f := vstore(clk, 0)
		defer f.close()
		tg := newTarget(t, f, withClock(clk))
		p := startedAt(t, "86", clk.now())
		mustSubmit(t, tg, p)
		for i := 0; i < copies; i++ {
			v := f.addVersion(keyOf("86"), p.SignedNote, "COMPLIANCE", clk.now().Add(30*24*time.Hour))
			v.attempt = "garbage"
		}
		_, err := tg.Submit(context.Background(), p)
		return err
	}
	if err := run(t, MaxVersionsChecked-2); err != nil {
		t.Fatalf("%d copies: %v", MaxVersionsChecked-2, err)
	}
	err := run(t, MaxVersionsChecked+1)
	ae := expectErr(t, err, anchor.ClassPermanent, "versions_bound_reached")
	for _, s := range []string{"reached its bound", "may be hidden", "a new key generation does not help", "nobody else"} {
		if !strings.Contains(ae.Error(), s) {
			t.Fatalf("text lacks %q: %v", s, ae)
		}
	}
	if strings.Contains(ae.Error(), "anchor this checkpoint under a new key generation") {
		t.Fatalf("the text sends the operator to a new generation: %v", ae)
	}
	if anchor.CodeOf(ae.Err) != "attempt_malformed" {
		t.Fatalf("the cause is not kept: %v", ae.Err)
	}
}

// A5-10, M22: the key of the evidence must END in the key of the note; a key
// that merely contains it is another key.
func TestVerifyNoteRefusesAKeyThatOnlyContainsTheNoteKey(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	p := testPayload(t, "87", 1)
	_, ev := mustSubmit(t, newTarget(t, f), p)
	if err := VerifyNote(ev, aggID, p.SignedNote, p.DigestHex()); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".bak", "/x", "0"} {
		bad := ev
		bad.Key = ev.Key + suffix
		if VerifyNote(bad, aggID, p.SignedNote, p.DigestHex()) == nil {
			t.Fatalf("the key %q was accepted", bad.Key)
		}
	}
}
