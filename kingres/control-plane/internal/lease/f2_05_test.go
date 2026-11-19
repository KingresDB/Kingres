// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// failingStop is an instance whose immediate stop fails a number of times.
type failingStop struct {
	*fakeInstance
	mu    sync.Mutex
	fails int
	tries int
}

func (f *failingStop) StopImmediate(ctx context.Context) error {
	f.mu.Lock()
	f.tries++
	if f.tries <= f.fails {
		f.mu.Unlock()
		return errors.New("the stop failed")
	}
	f.mu.Unlock()
	return f.fakeInstance.StopImmediate(ctx)
}

// F2-05 (a): a stop that failed is repeated every FenceRetry until the primary is down, and every failure is reported.
func TestAFenceThatFailedIsRepeatedUntilItSucceeds(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	fs := &failingStop{fakeInstance: r.inst, fails: 2}
	r.m.c.Instance = fs
	var failed int
	r.m.c.OnFenceFailed = func(int32, error) { failed++ }
	r.m.Renew(ctx)
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.clk.Advance(dur) // the lease is over
	r.m.Renew(ctx)
	r.m.Check(ctx) // the first attempt (lose) fails
	if up, _, _ := r.inst.state(); !up {
		t.Fatal("the stop should have failed the first time")
	}
	for i := 0; i < 4; i++ {
		r.clk.Advance(FenceRetry)
		r.m.Check(ctx)
	}
	if up, _, _ := r.inst.state(); up {
		t.Fatal("the primary still runs after the stop was repeated")
	}
	if failed < 1 {
		t.Fatal("a failed stop raised nothing")
	}
}

// F2-05 (b): a group that is not enforced when its lease runs out and becomes enforced later is stopped at a later
// check; the manager used to mark the lease lost and never look again.
func TestAGroupThatBecomesEnforcedAfterItsLeaseRanOutIsStopped(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.Renew(ctx)
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.clk.Advance(dur)
	r.m.Renew(ctx)
	r.m.Check(ctx)
	if up, _, _ := r.inst.state(); !up {
		t.Fatal("an unenforced group lost its primary")
	}
	*r.enf = true // a standby was added
	r.clk.Advance(FenceRetry)
	r.m.Check(ctx)
	if up, _, _ := r.inst.state(); up {
		t.Fatal("the primary of a group that became enforced runs with an expired lease")
	}
	_ = time.Second
}

// F2-05 (c): a group whose primary could not be stopped is listed (the alert oldPrimaryNotStopped is raised from it)
// until the stop succeeds.
func TestAFenceThatCannotStopThePrimaryIsListedUntilItDoes(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.c.Instance = &failingStop{fakeInstance: r.inst, fails: 2}
	r.m.Renew(ctx)
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.clk.Advance(dur)
	r.m.Renew(ctx)
	r.m.Check(ctx)
	if got := r.m.FenceFailures(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("a failed stop is not listed: %v", got)
	}
	for i := 0; i < 4; i++ {
		r.clk.Advance(FenceRetry)
		r.m.Check(ctx)
	}
	if got := r.m.FenceFailures(); len(got) != 0 {
		t.Fatalf("the list still names a group whose primary was stopped: %v", got)
	}
}
