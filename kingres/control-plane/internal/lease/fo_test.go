// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FO-03: an instance whose role file has no group yet (the layout of the earlier version, until the first look at the
// catalog) is held back by the marker of any group. PrepareStart migrates a legacy marker to per group markers
// and removes the legacy file before the instance is started; the gate must still hold it.
func TestTheGateOfAnInstanceWithoutAGroupHoldsForMigratedMarkers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FencedMarker), []byte("fenced\n"), 0o600); err != nil { // left by the earlier version
		t.Fatal(err)
	}
	b, _ := json.Marshal(Topology{Groups: []int32{3}, Members: []string{"a", "b", "c"}})
	if err := os.WriteFile(filepath.Join(dir, topologyFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	unknown := func(int) (int32, bool) { return -1, true } // instance.GroupUnknown, held back like a primary
	if err := Gate(dir, unknown)(1); err == nil {
		t.Fatal("before the migration the legacy marker must hold the instance (setup)")
	}
	if _, err := PrepareStart(dir, func(int32) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(FencedPath(dir, 3)); err != nil {
		t.Fatalf("setup: the per group marker is missing: %v", err)
	}
	if err := Gate(dir, unknown)(1); err == nil {
		t.Fatal("the marker of group 3 was migrated and the gate of an instance whose group is not known lets it start")
	}
	// a known group of another number is not held back by it
	if err := Gate(dir, func(int) (int32, bool) { return 4, true })(1); err != nil {
		t.Fatalf("a group with no marker must start: %v", err)
	}
	// and with no marker at all the unknown instance starts
	_ = os.Remove(FencedPath(dir, 3))
	if err := Gate(dir, unknown)(1); err != nil {
		t.Fatalf("no marker of any group: %v", err)
	}
}

// FO-07: the lease of a group is not handed over while an instance of the group runs on this server.
func TestALeaseIsNotHandedOverWhileTheInstanceRuns(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	if res, err := r.m.Transfer(ctx, 1, "b"); err == nil {
		t.Fatalf("a transfer with the primary running was accepted: %+v", res)
	}
	if res, err := r.m.ReleaseGroup(ctx, 1); err == nil {
		t.Fatalf("a release with the primary running was accepted: %+v", res)
	}
	if !r.m.Holds(1) || r.m.Relinquished(1) {
		t.Fatal("a refused hand-over changed the state")
	}
	_ = r.inst.StopImmediate(ctx)
	if _, err := r.m.Transfer(ctx, 1, "b"); err != nil {
		t.Fatalf("after the stop: %v", err)
	}
}

// FO-06: the repeat of a transfer whose answer was lost finds the lease at `to` and reports it done.
func TestARepeatedTransferIsDone(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.Renew(ctx)
	_ = r.inst.StopImmediate(ctx)
	if _, err := r.m.Transfer(ctx, 1, "b"); err != nil {
		t.Fatal(err)
	}
	res, err := r.m.Transfer(ctx, 1, "b")
	if err != nil || res.Granted < res.Need {
		t.Fatalf("the repeat of a transfer that took effect: %+v %v", res, err)
	}
	// a transfer to somebody else, after the lease went to b, is not done
	if _, err := r.m.Transfer(ctx, 1, "c"); err == nil {
		t.Fatal("a transfer of a lease that b holds must not be reported done for c")
	}
}

// oldVoter answers like the handler of the version before the transfer: TransferTo and Release are unknown to it,
// so every call is a plain grant for the caller.
type oldVoter struct{ inner *fakeVoter }

func (v oldVoter) Grant(ctx context.Context, server string, req Request) (Reply, error) {
	g := v.inner.grantors[server]
	if g == nil {
		return Reply{}, context.DeadlineExceeded
	}
	return g.Grant(req.Group, "a", time.Duration(req.DurationMs)*time.Millisecond, req.DryRun)
}

// FO-06: grantors of the version before the transfer and the release do not move or free the lease, whatever they
// answer: the hand-over is refused cleanly and nothing here changes.
func TestAHandOverToGrantorsOfAnOlderVersionIsRefused(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.Renew(ctx)
	_ = r.inst.StopImmediate(ctx)
	r.m.c.Voter = oldVoter{r.voter}
	if res, err := r.m.Transfer(ctx, 1, "b"); err == nil {
		t.Fatalf("a transfer to old grantors was reported done: %+v", res)
	}
	if res, err := r.m.ReleaseGroup(ctx, 1); err == nil {
		t.Fatalf("a release by old grantors was reported done: %+v", res)
	}
	if r.m.Relinquished(1) {
		t.Fatal("a refused hand-over relinquished the group")
	}
}

// FO-10: after a transfer by a fence holder, the same group can be adopted again by a later failover.
func TestAGroupHandedOverCanBeAdoptedByALaterFailover(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.c.Source = fakeSource{Topology{Groups: nil, Members: []string{"a", "b", "c"}}}
	r.m.c.InstanceFor = func(int32) (Instance, bool) { return nil, false } // the fence holder has no instance
	if err := r.m.Adopt(5, AdoptFence); err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx)
	if _, err := r.m.Transfer(ctx, 5, "b"); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(10 * time.Minute)
	if err := r.m.Adopt(5, AdoptHolder); err != nil {
		t.Fatalf("the second adoption of the same group: %v", err)
	}
}

// FO-13: a renewal that was sent before a release cannot be received by a grantor after it: the release waits for
// the round in flight, and the lease is free at the grantors at the end.
func TestAReleaseWaitsForTheRenewalInFlight(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.Renew(ctx)
	_ = r.inst.StopImmediate(ctx)
	r.voter.mu.Lock()
	r.voter.gate = make(chan struct{})
	gate := r.voter.gate
	r.voter.mu.Unlock()
	renewed := make(chan struct{})
	go func() { r.m.Renew(ctx); close(renewed) }()
	asked := func() int { r.voter.mu.Lock(); defer r.voter.mu.Unlock(); return len(r.voter.asked) }
	base := asked()
	for deadline := time.Now().Add(5 * time.Second); asked() < base+2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	released := make(chan error, 1)
	go func() { _, err := r.m.ReleaseGroup(ctx, 1); released <- err }()
	// The release has started when it has marked the group (it does so before it waits for the round in flight): wait
	// for that condition, with a bound, instead of a fixed time (F2-16). From then on it can only be blocked behind
	// the round that holds the gate, and the negative check below looks at it for a short window.
	for deadline := time.Now().Add(5 * time.Second); !r.m.Relinquished(1) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if !r.m.Relinquished(1) {
		t.Fatal("the release did not start")
	}
	for deadline := time.Now().Add(200 * time.Millisecond); asked() == base+2 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if n := asked(); n != base+2 {
		t.Fatalf("the release sent its requests while a renewal was in flight (%d asks, want %d)", n, base+2)
	}
	close(gate)
	<-renewed
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"b", "c"} {
		if e := r.voter.grantors[s].Snapshot(); len(e) != 0 {
			t.Fatalf("the lease was granted again at %s after the release: %+v", s, e)
		}
	}
}
