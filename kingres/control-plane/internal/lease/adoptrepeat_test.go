// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package lease

import (
	"context"
	"testing"
)

// A step that waits for the lease asks for the adoption again every second and reads the status right after. When the catalog already names this
// server the primary of the group (the restore of a group to the server it is on: the old primary stops, and the lease is waited for by the
// server that ran it), the renewal pass drops the adoption on every pass, so each ask made a new adoption and wiped the rounds that the pass had
// just run: the status never showed a held lease and the wait ended after its bound with "held by another server" and nobody named.
func TestAdoptingAgainDoesNotWipeTheRoundsOfAGroupTheCatalogNamesToThisServer(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.c.Source = fakeSource{Topology{Groups: []int32{5}, Members: []string{"a", "b", "c"}}}
	r.m.Renew(ctx) // the topology is read: the group is renewed from the catalog
	// The group was handed over: renewal stopped.
	if err := r.m.Relinquish(5); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Adopt(5, AdoptFence); err != nil { // the first ask of the step
		t.Fatal(err)
	}
	r.m.Renew(ctx) // a renewal pass: the catalog names the group, so the adoption ends and a round is run
	if st := r.m.AdoptionStatus(5); !st.Held {
		t.Fatalf("the first round must hold the lease: %+v", st)
	}
	// The next ask of the step, after the pass has dropped the adoption.
	if err := r.m.Adopt(5, AdoptFence); err != nil {
		t.Fatal(err)
	}
	if st := r.m.AdoptionStatus(5); !st.Held || st.Rounds != 1 {
		t.Fatalf("asking again wiped the rounds of a lease that is held: %+v", st)
	}
}

// A group that was given up is judged from its own first round: the ask that takes it again does not report the lease of before.
func TestAdoptingAGroupThatWasGivenUpIsJudgedFromItsOwnFirstRound(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.c.Source = fakeSource{Topology{Groups: []int32{5}, Members: []string{"a", "b", "c"}}}
	r.m.Renew(ctx)
	if !r.m.AdoptionStatus(5).Held {
		t.Fatal("the group is renewed from the catalog")
	}
	if err := r.m.Relinquish(5); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Adopt(5, AdoptFence); err != nil {
		t.Fatal(err)
	}
	if st := r.m.AdoptionStatus(5); st.Held || st.Rounds != 0 {
		t.Fatalf("the lease of before the hand-over was reported as the lease of the adoption: %+v", st)
	}
}
