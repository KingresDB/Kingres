// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package lease

import (
	"context"
	"testing"
	"time"
)

// unattachedRig adopts group 6 as kind on a rig whose single instance runs, wins the lease once, and lets every other server go silent.
func unattachedRig(t *testing.T, kind string, enforced bool) *rig {
	t.Helper()
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, enforced)
	r.m.c.Source = fakeSource{Topology{Groups: nil, Members: []string{"a", "b", "c"}}}
	if err := r.m.Adopt(6, kind); err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx)
	if !r.m.Holds(6) {
		t.Fatal("the adopted lease was not won")
	}
	r.settle(t)
	r.voter.down["b"], r.voter.down["c"] = true, true
	return r
}

func runPast(r *rig) {
	ctx := context.Background()
	for step := time.Duration(0); step < 2*dur; step += 100 * time.Millisecond {
		r.clk.Advance(100 * time.Millisecond)
		r.m.Renew(ctx)
		r.m.Check(ctx)
	}
}

// Phase 5 review L1: the holder of a promotion that lost its lease before the lease was attached (the promoting call answers lease_not_held) may run a promoted
// instance that nothing guards; the lease code stops it itself, without waiting for the watchdog, whether the group is enforced or not, and writes the fence marker.
func TestAPromotedInstanceWhoseLeaseWasLostBeforeTheAttachIsStopped(t *testing.T) {
	for _, enforced := range []bool{true, false} {
		r := unattachedRig(t, AdoptHolder, enforced)
		runPast(r)
		if running, stops, _ := r.inst.state(); running || stops == 0 {
			t.Fatalf("enforced=%v: the instance of a promotion whose lease was lost is still running (stops %d)", enforced, stops)
		}
		if !markerExists(r.m.c.Dir, FencedMarker, 6) {
			t.Fatalf("enforced=%v: the fence marker of the group was not written", enforced)
		}
	}
}

// The fence holder of a failover guards no instance: losing its lease stops nothing.
func TestTheFenceHolderLosingItsLeaseStopsNothing(t *testing.T) {
	r := unattachedRig(t, AdoptFence, true)
	runPast(r)
	if running, stops, _ := r.inst.state(); !running || stops != 0 {
		t.Fatalf("an adopted fence lease that was lost stopped an instance (running %v, stops %d)", running, stops)
	}
	if markerExists(r.m.c.Dir, FencedMarker, 6) {
		t.Fatal("an adopted fence lease that was lost wrote a fence marker")
	}
}

// A holder whose instance does not run has nothing to stop and writes nothing: the standby is still a standby.
func TestAHolderWithoutARunningInstanceWritesNoMarker(t *testing.T) {
	r := unattachedRig(t, AdoptHolder, true)
	_ = r.inst.StopImmediate(context.Background())
	runPast(r)
	if markerExists(r.m.c.Dir, FencedMarker, 6) {
		t.Fatal("a fence marker was written for an instance that does not run")
	}
}

// Phase 5 review M5: a stop of the promoted instance that failed is repeated every FenceRetry until it works, and the failure is reported meanwhile.
func TestAFailedStopOfAPromotedInstanceWhoseLeaseWasLostIsRepeated(t *testing.T) {
	r := unattachedRig(t, AdoptHolder, true)
	fs := &failingStop{fakeInstance: r.inst, fails: 2}
	r.m.c.Instance = fs
	failed := 0
	r.m.c.OnFenceFailed = func(int32, error) { failed++ }
	runPast(r)
	if running, stops, _ := r.inst.state(); running || stops == 0 {
		t.Fatalf("the instance still runs after the failed stops were repeated (stops %d)", stops)
	}
	if failed < 2 {
		t.Fatalf("the failed stops were reported %d times, want 2", failed)
	}
}
