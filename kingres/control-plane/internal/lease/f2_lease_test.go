// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"path/filepath"
	"testing"
)

// F2-04: after a hand-over the gate holds the old primary directory back; an adoption (the fence holder of a later
// failover) must not let it start; Attach (the instance of this adoption is the primary) clears the hold.
func TestAnAdoptionKeepsTheOldPrimaryDirectoryHeldBack(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.c.InstanceFor = func(int32) (Instance, bool) { return nil, false }
	dir := filepath.Join(r.dir, "lease")
	if err := r.m.Adopt(5, AdoptFence); err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx)
	if _, err := r.m.Transfer(ctx, 5, "b"); err != nil {
		t.Fatal(err)
	}
	if !markerExists(dir, RelinquishedMarker, 5) {
		t.Fatal("a transfer wrote no marker")
	}
	if err := r.m.Adopt(5, AdoptFence); err != nil {
		t.Fatal(err)
	}
	if !markerExists(dir, RelinquishedMarker, 5) {
		t.Fatal("the adoption cleared the hold on the old primary directory")
	}
}

// F2-03: a release writes its marker before the first request, so a restart in the middle of a release does not
// renew the group.
func TestAReleaseWritesItsMarkerFirst(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.Renew(ctx)
	_ = r.inst.StopImmediate(ctx)
	if _, err := r.m.ReleaseGroup(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if !markerExists(filepath.Join(r.dir, "lease"), RelinquishedMarker, 1) {
		t.Fatal("a release left no marker on disk")
	}
	m2, err := New(Config{Self: "a", Clock: r.clk, Duration: r.m.c.Duration, Renew: r.m.c.Renew, Margin: r.m.c.Margin,
		Grantor: r.m.c.Grantor, Voter: r.voter, Source: r.m.c.Source, Instance: r.inst,
		Enforced: func(int32) bool { return false }, Dir: filepath.Join(r.dir, "lease")})
	if err != nil {
		t.Fatal(err)
	}
	if !m2.Relinquished(1) {
		t.Fatal("a manager started after a release does not know that the group was given away")
	}
}

// F2-11: Attach before the lease is held is an error, not a nil pointer.
func TestAttachBeforeTheLeaseIsHeldIsRefused(t *testing.T) {
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.c.InstanceFor = func(int32) (Instance, bool) { return nil, false }
	if err := r.m.Adopt(7, AdoptHolder); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Attach(7); err == nil {
		t.Fatal("Attach was accepted before a round held the lease")
	}
}

// F2-02: a group that was released while the round list was taken is not asked for.
func TestARoundIsNotSentForAGroupReleasedMeanwhile(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.Renew(ctx)
	_ = r.inst.StopImmediate(ctx)
	if _, err := r.m.ReleaseGroup(ctx, 1); err != nil {
		t.Fatal(err)
	}
	before := len(r.voter.asked)
	r.m.Renew(ctx)
	if len(r.voter.asked) != before {
		t.Fatalf("a renewal was sent for a released group: %v", r.voter.asked[before:])
	}
	if r.m.mayRenew(1, Topology{Groups: []int32{1}}) {
		t.Fatal("mayRenew says a relinquished group may be renewed")
	}
}
