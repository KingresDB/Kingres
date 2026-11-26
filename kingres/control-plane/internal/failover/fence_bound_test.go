// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package failover

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
)

// Phase 5 review M3: the call that takes the fence lease keeps failing with an error that says nothing about the lease (a
// 503 of a lease manager that is not up, a 409, a bad answer). The step used to log it and try again for ever, with the
// freeze of the group lapsing and no way to end the task. It now ends after the wait for the lease: a failover is paused
// (nothing was promoted), a switchover is rolled back, and the task detail says what fails meanwhile.
func TestAFenceLeaseCallThatKeepsFailingPausesAFailover(t *testing.T) {
	w := newWorld()
	w.leaseErr = &CallError{Status: 503, Code: "not_available", Message: "the lease manager is not running"}
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, hardInput())
	var pe *PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("%s: want a pause, got %v", name, err)
	}
	if name != StepName(1, StepLease) {
		t.Errorf("paused in %q", name)
	}
	if w.index("promote s") >= 0 {
		t.Fatal("promoted without the lease")
	}
	if n := w.count("lease-fence s2"); n < 2 || n > 1000 {
		t.Errorf("%d attempts", n)
	}
}

func TestAFenceLeaseCallThatKeepsFailingRollsASwitchoverBack(t *testing.T) {
	w := gracefulWorld()
	w.leaseErr = errors.New("a decoding error")
	r := w.runner()
	w.fakeClock(r)
	_, err := runAll(t, r, gracefulInput())
	failedWith(t, w, err, CodeLeaseUnavailable)
	if w.index("promote s2") >= 0 {
		t.Fatal("promoted without the lease")
	}
	if w.index("resume") < 0 && w.index("unquiesce s1") < 0 {
		t.Errorf("the switchover was not rolled back:\n%s", strings.Join(w.log, "\n"))
	}
}

// A step that waits renews the freeze, so that it does not lapse while the step waits for the old lease.
func TestTheFreezeIsRenewedWhileTheStepWaitsForTheLease(t *testing.T) {
	w := newWorld()
	w.oldAlive, w.oldWritable = true, true
	w.live["s1"] = "live"
	w.oldRefusesDem = errors.New("the stop failed")
	w.leaseHeldByOld = true
	r := w.runner()
	r.LeaseWait = 400 * time.Second
	w.fakeClock(r)
	_, _ = runAll(t, r, hardInput())
	if n := w.count("freeze s1"); n < 14 {
		t.Fatalf("the freeze was sent %d times in a wait of 400 s", n)
	}
}

// Phase 5 review L-1: the renewal of the freeze inside the wait for the lease is an upkeep, not a step of its own. When the
// old primary is already stopped (a graceful switchover) its freeze handler may answer an error, or its state may say the group
// is not frozen; the step must go on waiting for the lease and end the way the wait ends, not fail with the group stopped
// and no rollback.
func TestAFreezeRenewalThatFailsDoesNotEndTheWaitForTheLease(t *testing.T) {
	for name, set := range map[string]func(w *world){
		"an error": func(w *world) {
			w.renewFreezeErr = &CallError{Status: 500, Code: "internal", Message: "the freeze handler failed"}
		},
		"not frozen": func(w *world) { w.renewUnfrozen = true },
	} {
		t.Run(name, func(t *testing.T) {
			w := gracefulWorld()
			w.leaseHeldByOld = true
			set(w)
			r := w.runner()
			r.LeaseWait = 100 * time.Second
			w.fakeClock(r)
			_, err := runAll(t, r, gracefulInput())
			failedWith(t, w, err, CodeLeaseStillHeld)
		})
	}
}

// A renewal that the callee answers as fenced (it has seen a newer run of the task) still stands the step down.
func TestAFreezeRenewalAnsweredAsFencedStandsTheStepDown(t *testing.T) {
	w := gracefulWorld()
	w.leaseHeldByOld = true
	w.renewFreezeErr = &CallError{Status: 409, Code: "fenced", Message: "a newer run"}
	r := w.runner()
	r.LeaseWait = 100 * time.Second
	w.fakeClock(r)
	_, err := runAll(t, r, gracefulInput())
	if !errors.Is(err, fence.ErrFenced) {
		t.Fatalf("want the stand-down, got %v", err)
	}
}

// Phase 5 review L-2: errors of the lease call that are separated by a time without a majority are not one continuous run of
// errors. The time of the first error is forgotten when the majority is lost, so the wait for the lease starts again when it
// is heard again.
func TestTheWaitAfterLeaseCallErrorsStartsAgainWhenTheMajorityIsHeardAgain(t *testing.T) {
	w := newWorld()
	w.leaseErr = &CallError{Status: 503, Code: "not_available", Message: "the lease manager is not running"}
	r := w.runner()
	r.LeaseWait = 3 * time.Second
	r.QuorumWait = 100 * time.Second
	start := time.Unix(1_000_000, 0)
	now := start
	r.Now = func() time.Time { return now }
	r.Sleep = func(ctx context.Context, d time.Duration) error { now = now.Add(d); return ctx.Err() }
	w.liveFn = func(string) string { // no majority from the 2nd to the 60th second
		if e := now.Sub(start); e >= 2*time.Second && e < 60*time.Second {
			return ""
		}
		return "live"
	}
	_, err := runAll(t, r, hardInput())
	var pe *PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a pause, got %v", err)
	}
	if e := now.Sub(start); e < 63*time.Second {
		t.Fatalf("paused %v after the start: the errors before the loss of the majority were counted", e)
	}
}

// Phase 5 review L-3: a failover that pauses in the wait for the lease does not leave this server renewing the fence lease it
// may have adopted (the call that failed may have been answered after the adoption, and a lease taken before the majority was
// lost is still renewed): the adoption has no lifetime, and the old primary could not renew meanwhile. The pause releases it.
func TestAPausedWaitForTheLeaseReleasesTheFenceAdoption(t *testing.T) {
	t.Run("lease call fails", func(t *testing.T) {
		w := newWorld()
		w.leaseErr = &CallError{Status: 503, Code: "not_available", Message: "the lease manager is not running"}
		r := w.runner()
		w.fakeClock(r)
		_, err := runAll(t, r, hardInput())
		var pe *PauseError
		if !errors.As(err, &pe) {
			t.Fatalf("want a pause, got %v", err)
		}
		if w.index("lease-release s2") < 0 {
			t.Fatalf("the adoption was not released:\n%s", strings.Join(w.log, "\n"))
		}
	})
	t.Run("majority lost while waiting", func(t *testing.T) {
		w := newWorld()
		w.leaseHeldByOld = true
		r := w.runner()
		r.QuorumWait = 5 * time.Second
		r.LeaseWait = 100 * time.Second
		start := time.Unix(1_000_000, 0)
		now := start
		r.Now = func() time.Time { return now }
		r.Sleep = func(ctx context.Context, d time.Duration) error { now = now.Add(d); return ctx.Err() }
		w.liveFn = func(string) string {
			if now.Sub(start) >= 2*time.Second {
				return ""
			}
			return "live"
		}
		_, err := runAll(t, r, hardInput())
		var pe *PauseError
		if !errors.As(err, &pe) {
			t.Fatalf("want a pause, got %v", err)
		}
		if w.index("lease-release s2") < 0 {
			t.Fatalf("the adoption was not released:\n%s", strings.Join(w.log, "\n"))
		}
	})
}
