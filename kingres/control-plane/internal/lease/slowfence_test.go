// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package lease

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// blockedStop is an instance whose immediate stop does not return until release is closed.
type blockedStop struct {
	*fakeInstance
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockedStop) StopImmediate(ctx context.Context) error {
	b.calls.Add(1)
	<-b.release
	return b.fakeInstance.StopImmediate(ctx)
}

// Phase 5 review M-A: the stop of group 1 hangs; the lease of group 2 runs out later and must be judged and fenced at once,
// not after the stop of group 1 returned. The loop's check does not wait for the fences, and no second fence of group 1 is
// started while the first runs.
func TestAHangingFenceDoesNotDelayTheJudgementOfAnotherGroup(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.c.Source = fakeSource{Topology{Groups: []int32{1, 2}, Members: []string{"a", "b", "c"}}}
	r.m.c.Audit = func(context.Context, string, int32, string) {}
	i1 := &blockedStop{fakeInstance: &fakeInstance{running: true}, release: make(chan struct{})}
	i2 := &fakeInstance{running: true}
	r.m.c.InstanceFor = func(g int32) (Instance, bool) {
		if g == 1 {
			return i1, true
		}
		return i2, true
	}
	t.Cleanup(func() {
		select {
		case <-i1.release:
		default:
			close(i1.release)
		}
	})
	r.m.Renew(ctx)
	r.voter.down["b"], r.voter.down["c"] = true, true
	// Group 2's lease outlives group 1's by five seconds.
	r.m.mu.Lock()
	r.m.groups[2].validUntil += 5 * time.Second
	r.m.mu.Unlock()
	r.clk.Advance(dur - margin + time.Second) // only group 1 is over
	done := make(chan struct{})
	go func() { r.m.check(ctx, false); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the check waited for the fence of group 1")
	}
	waitFor(t, func() bool { return i1.calls.Load() == 1 })
	r.clk.Advance(5 * time.Second) // group 2 is over; the retry time of group 1 has passed too
	r.m.check(ctx, false)
	waitFor(t, func() bool { up, _, _ := i2.state(); return !up })
	r.m.check(ctx, false)
	time.Sleep(100 * time.Millisecond)
	if n := i1.calls.Load(); n != 1 {
		t.Fatalf("a second fence of group 1 was started while the first runs (%d stops)", n)
	}
	close(i1.release)
	r.m.wg.Wait()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the condition did not come true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A fence that is still on its way to the stop (here held in the audit call before it) is not joined by a second one when the
// retry time passes.
func TestNoSecondFenceStartsWhileTheFirstIsOnItsWay(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	hold := make(chan struct{})
	rel := make(chan struct{})
	close(rel)
	stopper := &blockedStop{fakeInstance: r.inst, release: rel}
	r.m.c.Instance = stopper
	var audits atomic.Int32
	r.m.c.Audit = func(_ context.Context, action string, _ int32, _ string) {
		if action == AuditLeaseLost {
			audits.Add(1)
			<-hold
		}
	}
	t.Cleanup(func() { close(hold) })
	r.m.Renew(ctx)
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.clk.Advance(dur)
	r.m.check(ctx, false)
	waitFor(t, func() bool { return audits.Load() == 1 })
	r.clk.Advance(FenceRetry)
	r.m.check(ctx, false)
	r.clk.Advance(FenceRetry)
	r.m.check(ctx, false)
	time.Sleep(100 * time.Millisecond)
	if n := audits.Load(); n != 1 || stopper.calls.Load() != 0 {
		t.Fatalf("a second fence of the same group runs (%d losses, %d stops)", n, stopper.calls.Load())
	}
}
