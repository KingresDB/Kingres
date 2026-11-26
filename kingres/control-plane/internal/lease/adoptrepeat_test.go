// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package lease

import (
	"context"
	"testing"
	"time"
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

// The state that an ask keeps is only what the rounds said: whether the lease is held is judged by the clock, the flag of a lost lease and the grants,
// never by the fact that rounds were run once. The cases below are the ways in which a kept state must not report a held lease.

// Held ends with the time of the lease, whatever the kept state says.
func TestAKeptStateIsNotHeldAfterTheLeaseExpired(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	r.clk.Advance(dur - margin + time.Millisecond)
	if err := r.m.Adopt(1, AdoptFence); err != nil {
		t.Fatal(err)
	}
	if st := r.m.AdoptionStatus(1); st.Held || st.Rounds != 1 {
		t.Fatalf("the lease ran out and the status says held (or lost its rounds): %+v", st)
	}
}

// The lease went to another server: the rounds are refused, the status is not held and names the holder.
func TestAKeptStateIsNotHeldWhenTheLeaseWentToAnotherServer(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	r.clk.Advance(dur + margin + time.Second) // the grants of this server ran out at the grantors
	for _, s := range []string{"b", "c"} {
		if rep, _ := r.voter.grantors[s].Grant(1, "x", dur, false); !rep.Granted {
			t.Fatalf("the other server was not granted at %s: %+v", s, rep)
		}
	}
	// The self grantor stays a's, so this server is refused by two of three.
	r.m.Renew(ctx)
	if err := r.m.Adopt(1, AdoptFence); err != nil {
		t.Fatal(err)
	}
	st := r.m.AdoptionStatus(1)
	if st.Held || st.RefusedBy["b"] != "x" || st.RefusedBy["c"] != "x" {
		t.Fatalf("the lease is another server's and the status says %+v", st)
	}
}

// A lease that was lost and fenced is not held, and a repeated ask does not make it so.
func TestAKeptStateIsNotHeldAfterTheGroupWasFenced(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	r.voter.down["b"], r.voter.down["c"] = true, true
	for i := 0; i < 8; i++ {
		r.clk.Advance(2 * time.Second)
		r.m.Renew(ctx)
		r.m.Check(ctx)
	}
	if up, stops, _ := r.inst.state(); up || stops != 1 {
		t.Fatalf("the lost lease did not fence the instance: running=%v stops=%d", up, stops)
	}
	for i := 0; i < 2; i++ {
		if err := r.m.Adopt(1, AdoptFence); err != nil {
			t.Fatal(err)
		}
		if st := r.m.AdoptionStatus(1); st.Held {
			t.Fatalf("a fenced group is reported held after ask %d: %+v", i+1, st)
		}
	}
}

// A group that the grantors released starts from an empty state, and is judged from its own first round.
func TestAdoptingAfterTheReleaseStartsFromAnEmptyState(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	r.inst.mu.Lock()
	r.inst.running = false // the primary was stopped: the lease may be released
	r.inst.mu.Unlock()
	if res, err := r.m.ReleaseGroup(ctx, 1); err != nil || res.Granted < res.Need {
		t.Fatalf("the release: %+v %v", res, err)
	}
	if err := r.m.Adopt(1, AdoptFence); err != nil {
		t.Fatal(err)
	}
	if st := r.m.AdoptionStatus(1); st.Held || st.Rounds != 0 {
		t.Fatalf("the state of the released lease was kept: %+v", st)
	}
}

// The state is wiped by a new adoption of a group that the topology does not name to this server.
func TestAdoptingAGroupTheCatalogDoesNotNameWipesTheState(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.c.Source = fakeSource{Topology{Groups: nil, Members: []string{"a", "b", "c"}}}
	if err := r.m.Adopt(5, AdoptFence); err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx)
	if !r.m.AdoptionStatus(5).Held {
		t.Fatal("the adopted group is renewed")
	}
	r.m.mu.Lock()
	delete(r.m.adopted, 5) // an adoption that ended while its rounds are kept
	r.m.mu.Unlock()
	if err := r.m.Adopt(5, AdoptFence); err != nil {
		t.Fatal(err)
	}
	if st := r.m.AdoptionStatus(5); st.Held || st.Rounds != 0 {
		t.Fatalf("a group the topology does not name kept its state: %+v", st)
	}
}

// Without a topology the answer is "not named": the state is wiped.
func TestAdoptingWithoutAKnownTopologyWipesTheState(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	if !r.m.AdoptionStatus(1).Held {
		t.Fatal("the group is renewed from the topology")
	}
	r.m.mu.Lock()
	r.m.haveT = false
	r.m.mu.Unlock()
	if err := r.m.Adopt(1, AdoptFence); err != nil {
		t.Fatal(err)
	}
	if st := r.m.AdoptionStatus(1); st.Held || st.Rounds != 0 {
		t.Fatalf("a group with no known topology kept its state: %+v", st)
	}
}

// A group that is flagged as lost is not held although its clock has not run out (the flag is what a fence leaves until a round with a majority clears it).
func TestAKeptStateOfALostGroupIsNotHeld(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	r.m.mu.Lock()
	r.m.groups[1].lost = true
	r.m.mu.Unlock()
	if err := r.m.Adopt(1, AdoptFence); err != nil {
		t.Fatal(err)
	}
	if st := r.m.AdoptionStatus(1); st.Held || st.Rounds != 1 {
		t.Fatalf("a lost group is reported held, or its rounds were wiped: %+v", st)
	}
}
