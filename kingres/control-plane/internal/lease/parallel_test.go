// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package lease

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
)

// slowVoter answers like the wrapped voter, after a pause that ends early with the context.
type slowVoter struct {
	Voter
	delay time.Duration
}

func (v slowVoter) Grant(ctx context.Context, server string, req Request) (Reply, error) {
	select {
	case <-time.After(v.delay):
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
	return v.Voter.Grant(ctx, server, req)
}

// Phase 5 review M2: a server that runs the primaries of several groups renews them side by side. With rounds that follow
// each other, six groups of 700 ms each make every group's renewal period 4.2 s, and a lease that is usable for
// 3.2 s from the start of its round lapses although every round succeeded. The test runs the real loop with a real clock,
// six groups and grantors that are slow, and requires that no lease is ever lost and no instance is stopped.
func TestTheLeasesOfSeveralGroupsAreRenewedSideBySideWithSlowGrantors(t *testing.T) {
	members := []string{"a", "b", "c"}
	clk := clock.NewReal()
	voter := &fakeVoter{grantors: map[string]*Grantor{}, down: map[string]bool{}}
	for _, s := range members[1:] {
		voter.grantors[s] = newGrantor(t, clk, filepath.Join(tempDir(t), s))
	}
	dir := tempDir(t)
	inst := &fakeInstance{running: true}
	groups := []int32{1, 2, 3, 4, 5, 6}
	m, err := New(Config{Self: "a", Clock: clk, Duration: 4 * time.Second, Renew: 200 * time.Millisecond, Margin: 800 * time.Millisecond,
		Grantor: newGrantor(t, clk, filepath.Join(dir, "grants")), Voter: slowVoter{Voter: voter, delay: 700 * time.Millisecond},
		Source: fakeSource{Topology{Groups: groups, Members: members}}, Instance: inst,
		Enforced: func(int32) bool { return true }, Dir: filepath.Join(dir, "lease")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); <-done; m.fl.waitAll() }()
	// Every group holds a lease after the first rounds.
	deadline := time.Now().Add(15 * time.Second)
	for {
		all := true
		for _, g := range groups {
			all = all && m.Holds(g)
		}
		if all {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the leases were not obtained")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// From then on every lease is renewed in time: every group holds at every sample over three seconds.
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		for _, g := range groups {
			if !m.Holds(g) {
				t.Fatalf("the lease of group %d lapsed although every round succeeded", g)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if up, _, _ := inst.state(); !up {
		t.Fatal("the instance was fenced")
	}
}

// delayedStop is an instance whose immediate stop takes a while.
type delayedStop struct {
	*fakeInstance
	d time.Duration
}

func (s delayedStop) StopImmediate(ctx context.Context) error {
	time.Sleep(s.d)
	return s.fakeInstance.StopImmediate(ctx)
}

// Phase 5 review L3: the leases of two groups run out together and the stop of each hangs for 600 ms; the fences run side by side, so Check
// ends after about one stop and not after two.
func TestTheFencesOfSeveralGroupsRunSideBySide(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.c.Source = fakeSource{Topology{Groups: []int32{1, 2}, Members: []string{"a", "b", "c"}}}
	r.m.c.Audit = func(context.Context, string, int32, string) {} // the rig's recorder is not safe for fences that run side by side
	i1 := &fakeInstance{running: true}
	i2 := &fakeInstance{running: true}
	r.m.c.InstanceFor = func(g int32) (Instance, bool) {
		if g == 1 {
			return delayedStop{i1, 600 * time.Millisecond}, true
		}
		return delayedStop{i2, 600 * time.Millisecond}, true
	}
	r.m.Renew(ctx)
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.clk.Advance(dur)
	r.m.Renew(ctx)
	start := time.Now()
	r.m.Check(ctx)
	if took := time.Since(start); took > 1000*time.Millisecond {
		t.Fatalf("the fences took %v: they ran one after the other", took)
	}
	for g, in := range map[int32]*fakeInstance{1: i1, 2: i2} {
		if up, _, _ := in.state(); up {
			t.Fatalf("group %d was not fenced", g)
		}
	}
}
