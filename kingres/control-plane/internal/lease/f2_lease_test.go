// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

// recordingVoter records, for every request, the group and whether it was a release, and calls onFirstGrant once, in
// the middle of the first plain renewal it sees.
type recordingVoter struct {
	*fakeVoter
	mu           sync.Mutex
	calls        []string
	onFirstGrant func()
	// ready, when set, holds every plain request for group 1 until it is true (bounded): no vote of the round of group 1
	// returns before the release of group 2 has marked the group.
	ready func() bool
	once  sync.Once
}

func (v *recordingVoter) Grant(ctx context.Context, server string, req Request) (Reply, error) {
	v.mu.Lock()
	kind := "grant"
	if req.Release {
		kind = "release"
	}
	v.calls = append(v.calls, fmt.Sprintf("%s:%d", kind, req.Group))
	v.mu.Unlock()
	if !req.Release && req.TransferTo == "" && v.onFirstGrant != nil {
		v.once.Do(v.onFirstGrant)
	}
	if !req.Release && req.TransferTo == "" && req.Group == 1 && v.ready != nil {
		for deadline := time.Now().Add(10 * time.Second); !v.ready() && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
	}
	return v.fakeVoter.Grant(ctx, server, req)
}

func (v *recordingVoter) seen() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.calls...)
}

// F2-02, the case of the review: a server runs groups 1 and 2, the pass of Renew has taken the list [1, 2], and while it
// works on group 1 the failover releases group 2. The pass must not then send a plain renewal for group 2: a grantor
// would hold the group for the releaser again and the new holder would be refused for duration plus margin. The test
// fails when the check in Renew's loop (mayRenew) is removed. Nothing here depends on speed: the release starts
// inside the first request of the round of group 1 and the test waits, with a bound, for the group to be marked.
func TestARoundIsNotSentForAGroupReleasedInTheMiddleOfAPass(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.c.Source = fakeSource{Topology{Groups: []int32{1, 2}, Members: []string{"a", "b", "c"}}}
	rv := &recordingVoter{fakeVoter: r.voter}
	r.m.c.Voter = rv
	r.m.Renew(ctx)   // both groups held
	r.m.fl.waitAll() // the requests of that round to the last grantor are over before the count below starts
	_ = r.inst.StopImmediate(ctx)
	released := make(chan error, 1)
	rv.onFirstGrant = func() {
		go func() {
			_, err := r.m.ReleaseGroup(ctx, 2)
			released <- err
		}()
	}
	rv.ready = func() bool { return r.m.Relinquished(2) }
	mark := len(rv.seen())
	r.m.Renew(ctx)
	select {
	case err := <-released:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the release did not finish")
	}
	after := rv.seen()[mark:]
	sawRelease := false
	for _, c := range after {
		switch c {
		case "release:2":
			sawRelease = true
		case "grant:2":
			// The rounds of the groups run side by side: a renewal of group 2 may have been sent before the release was
			// asked for (the release waits for it), never after the release began.
			if sawRelease {
				t.Fatalf("a renewal of group 2 was sent after its release: %v", after)
			}
		}
	}
	if !sawRelease {
		t.Fatalf("no release was sent: %v", after)
	}
}

// The check that a round makes with the lock of its group held: a group that was released after the list of the pass was
// taken is not asked for (F2-02). Seen directly, since the rounds of a pass no longer follow each other.
func TestARoundIsNotSentForAGroupThatWasReleasedAfterTheListWasTaken(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	top := Topology{Groups: []int32{1, 2}, Members: []string{"a", "b", "c"}}
	r.m.c.Source = fakeSource{top}
	rv := &recordingVoter{fakeVoter: r.voter}
	r.m.c.Voter = rv
	r.m.Renew(ctx)
	r.m.fl.waitAll()
	mark := len(rv.seen())
	r.m.mu.Lock()
	r.m.relinq[2] = true // released after the pass took its list
	r.m.mu.Unlock()
	r.m.renewOne(ctx, 2)
	r.m.fl.waitAll()
	for _, c := range rv.seen()[mark:] {
		if c == "grant:2" {
			t.Fatal("a renewal was sent for a group that was released")
		}
	}
}
