// SPDX-License-Identifier: AGPL-3.0-only

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
// each other, four groups of 250 ms each make every group's renewal period one second, and a lease that is usable for
// 800 ms from the start of its round lapses although every round succeeded. The test runs the real loop with a real clock,
// four groups and grantors that are slow, and requires that no lease is ever lost and no instance is stopped.
func TestTheLeasesOfSeveralGroupsAreRenewedSideBySideWithSlowGrantors(t *testing.T) {
	members := []string{"a", "b", "c"}
	clk := clock.NewReal()
	voter := &fakeVoter{grantors: map[string]*Grantor{}, down: map[string]bool{}}
	for _, s := range members[1:] {
		voter.grantors[s] = newGrantor(t, clk, filepath.Join(tempDir(t), s))
	}
	dir := tempDir(t)
	inst := &fakeInstance{running: true}
	groups := []int32{1, 2, 3, 4}
	m, err := New(Config{Self: "a", Clock: clk, Duration: time.Second, Renew: 100 * time.Millisecond, Margin: 200 * time.Millisecond,
		Grantor: newGrantor(t, clk, filepath.Join(dir, "grants")), Voter: slowVoter{Voter: voter, delay: 250 * time.Millisecond},
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
	deadline := time.Now().Add(5 * time.Second)
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
	end := time.Now().Add(3 * time.Second)
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
