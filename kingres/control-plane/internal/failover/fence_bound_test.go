// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"errors"
	"strings"
	"testing"
	"time"
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
