// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"sync"
	"testing"
	"time"
)

// gateVoter holds the renewal requests of the groups chosen by hold until gate is closed (or the request's context ends), and
// records the groups it was asked to renew.
type gateVoter struct {
	Voter
	hold func(int32) bool
	gate chan struct{}
	mu   sync.Mutex
	sent []int32
	once sync.Once
}

func (v *gateVoter) open() { v.once.Do(func() { close(v.gate) }) }

// queued returns the one group of 1..9 that has asked no grantor while eight others have (the one that waits for a place),
// and false before that state is reached.
func (v *gateVoter) queued() (int32, bool) {
	var missing []int32
	for g := int32(1); g <= 9; g++ {
		if v.sentFor(g) == 0 {
			missing = append(missing, g)
		}
	}
	if len(missing) != 1 {
		return 0, false
	}
	return missing[0], true
}

func (v *gateVoter) Grant(ctx context.Context, server string, req Request) (Reply, error) {
	if !req.Release {
		v.mu.Lock()
		v.sent = append(v.sent, req.Group)
		v.mu.Unlock()
		if v.hold(req.Group) {
			select {
			case <-v.gate:
			case <-ctx.Done():
				return Reply{}, ctx.Err()
			}
		}
	}
	return v.Voter.Grant(ctx, server, req)
}

func (v *gateVoter) sentFor(g int32) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, x := range v.sent {
		if x == g {
			n++
		}
	}
	return n
}

// queuedRig is a server with nine groups whose rounds hang in their grantors, so that one round waits for a
// place of the semaphore.
func queuedRig(t *testing.T) (*rig, *gateVoter, []int32) {
	t.Helper()
	r := newRig(t, []string{"a", "b", "c"}, false)
	groups := []int32{1, 2, 3, 4, 5, 6, 7, 8, 9}
	r.m.c.Source = fakeSource{Topology{Groups: groups, Members: []string{"a", "b", "c"}}}
	r.inst.running = false // no instance serves the groups, so a release is allowed
	gv := &gateVoter{Voter: r.voter, hold: func(int32) bool { return true }, gate: make(chan struct{})}
	r.m.c.Voter = gv
	t.Cleanup(gv.open)
	r.m.refreshTopology(context.Background())
	return r, gv, groups
}

// Phase 5 review M-C (a): a group that waits for a place of the semaphore holds no round lock, so the release of that
// group does not wait behind the rounds that hang in front of it.
func TestTheReleaseOfAQueuedGroupDoesNotWaitBehindOtherRounds(t *testing.T) {
	ctx := context.Background()
	r, gv, _ := queuedRig(t)
	done := make(chan struct{})
	go func() { defer close(done); r.m.renewGroups(ctx, true) }()
	var q int32
	waitFor(t, func() bool { var ok bool; q, ok = gv.queued(); return ok })
	released := make(chan error, 1)
	go func() { _, err := r.m.ReleaseGroup(ctx, q); released <- err }()
	select {
	case err := <-released:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the release of group %d waited behind the rounds of other groups", q)
	}
	gv.open()
	<-done
	if n := gv.sentFor(q); n != 0 {
		t.Fatalf("group %d was renewed after its release (%d requests)", q, n)
	}
}

// Phase 5 review M-C (b): a round that waited in the queue uses the topology of the moment it runs. Group 9 is removed from
// the list while it waits; it must not be renewed.
func TestAQueuedRoundReadsTheTopologyAfterItsTurn(t *testing.T) {
	ctx := context.Background()
	r, gv, _ := queuedRig(t)
	done := make(chan struct{})
	go func() { defer close(done); r.m.renewGroups(ctx, true) }()
	var q int32
	waitFor(t, func() bool { var ok bool; q, ok = gv.queued(); return ok })
	r.m.mu.Lock()
	var rest []int32
	for _, g := range r.m.topo.Groups {
		if g != q {
			rest = append(rest, g)
		}
	}
	r.m.topo.Groups = rest // the queued group left the catalog while it waited
	r.m.mu.Unlock()
	gv.open()
	<-done
	r.m.fl.waitAll()
	if n := gv.sentFor(q); n != 0 {
		t.Fatalf("group %d left the topology while it waited and was renewed (%d requests)", q, n)
	}
}

// The semaphore hands its places on in the order of asking, and a waiter whose context ends gives nothing away.
func TestTheRoundSemaphoreIsFirstComeFirstServed(t *testing.T) {
	s := newFairSem(2)
	ctx := context.Background()
	if !s.acquire(ctx) || !s.acquire(ctx) {
		t.Fatal("two free places")
	}
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.acquire(ctx) {
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
			}
		}()
		time.Sleep(20 * time.Millisecond) // the waiters ask in the order 0..5
	}
	for i := 0; i < 6; i++ {
		s.release()
		time.Sleep(20 * time.Millisecond)
	}
	wg.Wait()
	for i, x := range order {
		if x != i {
			t.Fatalf("places were handed over in the order %v", order)
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if s.acquire(cctx) {
		t.Fatal("no place is free, the cancelled call must not get one")
	}
	s.release()
	s.release()
	if !s.acquire(ctx) || !s.acquire(ctx) {
		t.Fatal("a cancelled waiter kept a place")
	}
}
