// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package lease

import (
	"context"
	"testing"
)

// F2-06: an instance that may be the primary of a group and that the role files cannot attribute to it is held like
// the primary: a fence stops it, and a hand-over of the lease is refused while it runs.

func unattributedRig(t *testing.T) (*rig, *fakeInstance) {
	t.Helper()
	r := newRig(t, []string{"a", "b", "c"}, true)
	stray := &fakeInstance{running: true}
	// No instance is attributed to the group (the role file cannot be read), one stray instance may be its primary.
	r.m.c.InstanceFor = func(int32) (Instance, bool) { return nil, false }
	r.m.c.Unattributed = func(g int32) []Instance {
		if g == 1 {
			return []Instance{stray}
		}
		return nil
	}
	return r, stray
}

func TestAFenceStopsAnInstanceThatCannotBeAttributedToTheGroup(t *testing.T) {
	ctx := context.Background()
	r, stray := unattributedRig(t)
	if err := r.m.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if up, stops, _ := stray.state(); up || stops != 1 {
		t.Fatalf("the unattributed instance was not stopped by the fence: running=%v stops=%d", up, stops)
	}
	if !markerExists(r.m.c.Dir, FencedMarker, 1) {
		t.Fatal("the fence wrote no marker")
	}
}

func TestALostLeaseStopsAnInstanceThatCannotBeAttributedToTheGroup(t *testing.T) {
	ctx := context.Background()
	r, stray := unattributedRig(t)
	r.m.Renew(ctx)
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.clk.Advance(dur)
	r.m.Renew(ctx)
	r.m.Check(ctx)
	if up, _, _ := stray.state(); up {
		t.Fatal("a lost lease left the unattributed instance running")
	}
}

func TestAHandOverIsRefusedWhileAnUnattributedInstanceRuns(t *testing.T) {
	ctx := context.Background()
	r, stray := unattributedRig(t)
	r.m.Renew(ctx)
	if _, err := r.m.Transfer(ctx, 1, "b"); err == nil {
		t.Fatal("Transfer was accepted while an instance that may be the primary runs")
	}
	if _, err := r.m.ReleaseGroup(ctx, 1); err == nil {
		t.Fatal("ReleaseGroup was accepted while an instance that may be the primary runs")
	}
	_ = stray.StopImmediate(ctx)
	if _, err := r.m.ReleaseGroup(ctx, 1); err != nil {
		t.Fatalf("a stopped instance must not block the release: %v", err)
	}
}

// The stop of an instance that cannot be attributed to the group is repeated, as the stop of the primary is (F2-05).
func TestAStopThatFailedOnAnUnattributedInstanceIsRepeated(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	stray := &fakeInstance{running: true}
	failing := &failingStop{fakeInstance: stray, fails: 1}
	r.m.c.InstanceFor = func(int32) (Instance, bool) { return nil, false }
	r.m.c.Unattributed = func(g int32) []Instance {
		if g == 1 {
			return []Instance{failing}
		}
		return nil
	}
	r.m.Renew(ctx)
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.clk.Advance(dur)
	r.m.Renew(ctx)
	r.m.Check(ctx) // the first stop fails
	if up, _, _ := stray.state(); !up {
		t.Fatal("the first stop should have failed")
	}
	if got := r.m.FenceFailures(); len(got) != 1 {
		t.Fatalf("the failed stop is not listed: %v", got)
	}
	r.clk.Advance(FenceRetry)
	r.m.Check(ctx)
	if up, _, _ := stray.state(); up {
		t.Fatal("the stop of the unattributed instance was not repeated")
	}
}

// A group in the cached topology of a server that runs no instance for it any more (its old primary was rebuilt as a
// standby while the catalog could not be read) writes no fence marker when its lease is lost: the marker would hold the
// next primary of the group back for ever.
func TestALostLeaseOfAGroupWithNoInstanceWritesNoMarker(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.c.InstanceFor = func(int32) (Instance, bool) { return nil, false }
	r.m.Renew(ctx)
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.clk.Advance(dur)
	r.m.Renew(ctx)
	r.m.Check(ctx)
	if markerExists(r.m.c.Dir, FencedMarker, 1) {
		t.Fatal("a fence marker was written for a group no instance of this server serves")
	}
}
