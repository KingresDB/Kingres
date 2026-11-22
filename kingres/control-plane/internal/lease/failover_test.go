// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
)

// ---- T17: the grantor additions ----

// transferTo moves the lease only when the caller is the holder of record, even
// before the lease ran out: then nobody else can have it for a fresh duration and
// the old holder cannot be granted it again.
func TestTransferOnlyFromTheHolderAndAtomically(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	g := newGrantor(t, clk, tempDir(t))
	_, _ = g.Grant(1, "a", dur, false)
	clk.Advance(3 * time.Second) // a's lease has not run out
	if r, _ := g.Transfer(1, "b", "c", dur); r.Granted || r.Holder != "a" || r.WaitMs <= 0 {
		t.Fatalf("a caller that is not the holder moved the lease: %+v", r)
	}
	if r, _ := g.Transfer(2, "a", "b", dur); r.Granted {
		t.Fatalf("a group without a grant cannot be transferred: %+v", r)
	}
	r, err := g.Transfer(1, "a", "b", dur)
	if err != nil || !r.Granted || r.Holder != "b" {
		t.Fatalf("the holder could not hand the lease over before it ran out: %+v %v", r, err)
	}
	// There is no instant at which the group has no holder: a is refused now, and b renews.
	if r, _ := g.Grant(1, "a", dur, false); r.Granted || r.Holder != "b" {
		t.Fatalf("the old holder was granted the lease again: %+v", r)
	}
	if r, _ := g.Grant(1, "b", dur, false); !r.Granted {
		t.Fatal("the new holder must be granted")
	}
	// A third server is still refused until the new holder's lease plus the margin is over.
	clk.Advance(dur + margin - time.Millisecond)
	if r, _ := g.Grant(1, "c", dur, true); r.Granted {
		t.Fatal("a third server was granted inside the new holder's lease")
	}
}

// The transfer is on disk before it is answered and survives a restart of the
// grantor, which then refuses the old holder for duration plus margin.
func TestTransferIsPersistedBeforeTheAnswerAndSurvivesARestart(t *testing.T) {
	dir := tempDir(t)
	clk := clock.NewFake(time.Unix(0, 0))
	g := newGrantor(t, clk, dir)
	_, _ = g.Grant(1, "a", dur, false)
	if r, _ := g.Transfer(1, "a", "b", dur); !r.Granted {
		t.Fatal("transfer refused")
	}
	g2 := newGrantor(t, clock.NewFake(time.Unix(50000, 0)), dir)
	if s := g2.Snapshot(); len(s) != 1 || s[0].Holder != "b" {
		t.Fatalf("the file does not name the new holder: %+v", s)
	}
	if r, _ := g2.Grant(1, "a", dur, false); r.Granted {
		t.Fatal("a restarted grantor granted the old holder again")
	}
}

// A transfer that cannot be recorded is refused and changes nothing.
func TestTransferThatCannotBeRecordedIsRefused(t *testing.T) {
	dir := tempDir(t)
	clk := clock.NewFake(time.Unix(0, 0))
	g := newGrantor(t, clk, dir)
	_, _ = g.Grant(1, "a", dur, false)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Skip("cannot break the directory on this system")
	}
	if r, err := g.Transfer(1, "a", "b", dur); err == nil || r.Granted {
		t.Fatalf("an unrecorded transfer was reported: %+v %v", r, err)
	}
	if s := g.Snapshot(); len(s) != 1 || s[0].Holder != "a" {
		t.Fatalf("an unrecorded transfer was remembered: %+v", s)
	}
}

// release deletes the grant of the holder at once; nobody else can release it.
func TestReleaseFreesTheLeaseAtOnceForTheHolderOnly(t *testing.T) {
	dir := tempDir(t)
	clk := clock.NewFake(time.Unix(0, 0))
	g := newGrantor(t, clk, dir)
	_, _ = g.Grant(1, "a", dur, false)
	if r, _ := g.Release(1, "b"); r.Granted {
		t.Fatal("a server that does not hold the lease released it")
	}
	if r, _ := g.Grant(1, "b", dur, true); r.Granted {
		t.Fatal("the lease must still be a's")
	}
	if r, err := g.Release(1, "a"); err != nil || !r.Granted {
		t.Fatalf("the holder could not release: %+v %v", r, err)
	}
	if r, _ := g.Grant(1, "b", dur, false); !r.Granted {
		t.Fatal("after a release the lease is free at once, without the margin")
	}
	if r, _ := g.Release(9, "a"); !r.Granted {
		t.Fatal("releasing a group that has no grant has reached its aim")
	}
	g2 := newGrantor(t, clock.NewFake(time.Unix(9, 0)), dir)
	if s := g2.Snapshot(); len(s) != 1 || s[0].Holder != "b" {
		t.Fatalf("the release is not on disk: %+v", s)
	}
}

// ---- the holder additions ----

// An adopted group is renewed although the catalog does not name this server as its
// primary; until Attach, losing the lease fences nothing (there is no instance);
// after Attach it fences like any primary's.
func TestAdoptRenewsAndOnlyAttachedLeasesFence(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.c.Source = fakeSource{Topology{Groups: nil, Members: []string{"a", "b", "c"}}}
	if err := r.m.Adopt(5, AdoptFence); err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx)
	if st := r.m.AdoptionStatus(5); !st.Held || st.Rounds != 1 {
		t.Fatalf("an adopted group must be renewed: %+v", st)
	}
	if !r.m.Holds(5) {
		t.Fatal("Holds must be true for an adopted group with a lease")
	}
	// The lease is lost: nothing to stop, no marker.
	r.voter.down["b"], r.voter.down["c"] = true, true
	for i := 0; i < 8; i++ {
		r.clk.Advance(2 * time.Second)
		r.m.Renew(ctx)
		r.m.Check(ctx)
	}
	if up, stops, _ := r.inst.state(); !up || stops != 0 {
		t.Fatalf("losing an adopted lease stopped an instance: running=%v stops=%d", up, stops)
	}
	if AnyFencedFile(r.dir) {
		t.Fatal("a fence marker was written for a lease that guards no instance")
	}
	// Attached: the same loss fences.
	r.voter.down["b"], r.voter.down["c"] = false, false
	if err := r.m.Adopt(6, AdoptHolder); err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx)
	if !r.m.Holds(6) {
		t.Fatal("the holder adoption was not renewed")
	}
	if err := r.m.Attach(6); err != nil {
		t.Fatal(err)
	}
	r.voter.down["b"], r.voter.down["c"] = true, true
	for i := 0; i < 8; i++ {
		r.clk.Advance(2 * time.Second)
		r.m.Renew(ctx)
		r.m.Check(ctx)
	}
	if up, stops, _ := r.inst.state(); up || stops != 1 {
		t.Fatalf("an attached lease that was lost must stop the instance: running=%v stops=%d", up, stops)
	}
	if !markerExists(filepath.Join(r.dir, "lease"), FencedMarker, 6) {
		t.Fatal("the fence marker of the attached group is missing")
	}
	if err := r.m.Attach(77); err == nil {
		t.Fatal("Attach of a group that is not adopted must fail")
	}
}

// AdoptionStatus tells a step what to wait for: how long until enough grantors
// would grant, and who refused.
func TestAdoptionStatusReportsTheWaitAndTheRefusers(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.c.Source = fakeSource{Topology{Groups: nil, Members: []string{"a", "b", "c"}}}
	// b and c remember another holder of group 7.
	for _, s := range []string{"b", "c"} {
		if _, err := r.voter.grantors[s].Grant(7, "z", dur, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.m.Adopt(7, AdoptFence); err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx)
	st := r.m.AdoptionStatus(7)
	if st.Held || st.WaitMs <= 0 || st.RefusedBy["b"] != "z" || st.RefusedBy["c"] != "z" {
		t.Fatalf("the status must say how long and who: %+v", st)
	}
	r.clk.Advance(dur + margin + time.Second)
	r.m.Renew(ctx)
	if st := r.m.AdoptionStatus(7); !st.Held || st.WaitMs != 0 {
		t.Fatalf("after the old lease ran out the group is held: %+v", st)
	}
}

// Transfer hands the lease to another server at a majority of the grantors; this
// manager then stops renewing, does not hold the group and keeps that across a
// restart (the marker).
func TestTransferStopsRenewingAndSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.Renew(ctx)
	if !r.m.Holds(1) {
		t.Fatal("setup: the lease is not held")
	}
	_ = r.inst.StopImmediate(ctx) // a transfer needs the stop confirmed (FO-07)
	res, err := r.m.Transfer(ctx, 1, "b")
	if err != nil || res.Granted < res.Need {
		t.Fatalf("transfer: %+v %v", res, err)
	}
	if r.m.Holds(1) {
		t.Fatal("this server still holds a lease it handed over")
	}
	if !r.m.Relinquished(1) {
		t.Fatal("a transferred group must be relinquished")
	}
	// A majority of the three grantors (this server's own and at least one of the others) moved the lease; the
	// third may still be answering, so only the majority is asserted.
	moved := 0
	for _, s := range []string{"b", "c"} {
		if e := r.voter.grantors[s].Snapshot(); len(e) == 1 && e[0].Holder == "b" {
			moved++
		}
	}
	if own := r.m.c.Grantor.Snapshot(); len(own) != 1 || own[0].Holder != "b" || moved < 1 {
		t.Fatalf("the lease was not moved at a majority: own %+v, others moved %d", own, moved)
	}
	r.m.Renew(ctx)
	if r.m.AdoptionStatus(1).Rounds != 0 {
		t.Fatal("a transferred group was renewed")
	}
	// kingresd restarts: the marker keeps it from renewing what it gave away.
	m2, err := New(Config{Self: "a", Clock: r.clk, Duration: dur, Renew: 2 * time.Second, Margin: margin,
		Grantor: r.m.c.Grantor, Voter: r.voter, Source: r.m.c.Source, Instance: r.inst,
		Enforced: func(int32) bool { return false }, Dir: filepath.Join(r.dir, "lease")})
	if err != nil {
		t.Fatal(err)
	}
	m2.Renew(ctx)
	if m2.Holds(1) || m2.AdoptionStatus(1).Rounds != 0 {
		t.Fatal("a restarted manager renewed a group that was transferred")
	}
	// A second failover of the same group adopts it again by decision of the task (FO-10): the renewal starts again,
	// but the marker file stays, because it is what holds the old primary directory back (F2-04).
	if err := m2.Adopt(1, AdoptFence); err != nil {
		t.Fatalf("an explicit adoption of a group this server handed over earlier: %v", err)
	}
	if m2.Relinquished(1) {
		t.Fatal("an adopted group is still marked relinquished in memory")
	}
	if !markerExists(filepath.Join(r.dir, "lease"), RelinquishedMarker, 1) {
		t.Fatal("the adoption removed the mark that holds the old primary directory back (F2-04)")
	}
	m2.Unadopt(1)
	if err := m2.Relinquish(1); err != nil {
		t.Fatal(err)
	}
	if err := m2.Unrelinquish(1); err != nil {
		t.Fatal(err)
	}
	m2.Renew(ctx)
	if m2.AdoptionStatus(1).Rounds != 1 {
		t.Fatal("after Unrelinquish the group is renewed again")
	}
}

// A transfer that no majority acknowledged changes nothing here: the lease is still held.
func TestTransferWithoutAMajorityKeepsTheLease(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.Renew(ctx)
	r.voter.down["b"], r.voter.down["c"] = true, true
	_ = r.inst.StopImmediate(ctx)
	if res, err := r.m.Transfer(ctx, 1, "b"); err == nil || res.Granted >= res.Need {
		t.Fatalf("a transfer reached %+v without a majority: %v", res, err)
	}
	if !r.m.Holds(1) || r.m.Relinquished(1) {
		t.Fatal("a failed transfer must leave the lease and the renewal as they were")
	}
}

// Relinquish stops renewing but keeps judging: the lease that was held still ends
// the instance at validUntil minus the margin, so a handover that fails to stop the
// primary does not let it act beyond what it held. The primary may not start while
// the marker is there.
func TestRelinquishKeepsCheckingAndHoldsTheInstanceBack(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	if err := r.m.Relinquish(1); err != nil {
		t.Fatal(err)
	}
	rounds := r.m.AdoptionStatus(1).Rounds
	r.m.Renew(ctx)
	if r.m.AdoptionStatus(1).Rounds != rounds {
		t.Fatal("a relinquished group was renewed")
	}
	dir := filepath.Join(r.dir, "lease")
	if err := Gate(dir, testPrimary)(1); err == nil {
		t.Fatal("the primary of a relinquished group must be held back")
	}
	r.clk.Advance(dur - margin + time.Millisecond)
	r.m.Check(ctx)
	if up, stops, _ := r.inst.state(); up || stops != 1 {
		t.Fatalf("the lease that ran out after Relinquish did not stop the instance: running=%v stops=%d", up, stops)
	}
	if err := r.m.Unrelinquish(1); err != nil {
		t.Fatal(err)
	}
	if err := Gate(dir, testPrimary)(1); err == nil {
		t.Log("the fence marker of the lost lease still holds the instance back, as it must")
	}
}

// AnyFencedFile is the test's own look at the lease directory of a rig.
func AnyFencedFile(rigDir string) bool { return AnyFenced(filepath.Join(rigDir, "lease")) }

// The clean stop of a planned switchover holds the primary back but goes on renewing the lease until the stop is over: a shutdown that takes
// longer than the lease has left (a slow walsender, a slow checkpoint: here, three lease durations of the fake clock) is not fenced in the middle,
// which would make it an unclean one. When the stop ends cleanly the lease is released; when it does not, Relinquish ends the renewal and the
// lease lapses and fences as before.
func TestHoldBackKeepsRenewingThroughASlowStop(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	if err := r.m.HoldBack(1); err != nil {
		t.Fatal(err)
	}
	if err := Gate(filepath.Join(r.dir, "lease"), testPrimary)(1); err == nil {
		t.Fatal("the primary of a group that is held back must not be started")
	}
	if r.m.Relinquished(1) {
		t.Fatal("HoldBack stopped the renewal")
	}
	for elapsed := time.Duration(0); elapsed < 3*dur; elapsed += 2 * time.Second {
		r.clk.Advance(2 * time.Second)
		r.m.Renew(ctx)
		r.settle(t)
		r.m.Check(ctx)
		if up, stops, _ := r.inst.state(); !up || stops != 0 {
			t.Fatalf("the slow stop was fenced after %v: running=%v stops=%d", elapsed+2*time.Second, up, stops)
		}
		if !r.m.Holds(1) {
			t.Fatalf("the lease was lost after %v", elapsed+2*time.Second)
		}
	}
	// The stop did not end cleanly: the renewal ends, the lease lapses and the fence stops what still runs.
	if err := r.m.Relinquish(1); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(dur - margin + time.Millisecond)
	r.m.Renew(ctx)
	r.m.Check(ctx)
	if up, stops, _ := r.inst.state(); up || stops != 1 {
		t.Fatalf("a stop that did not end cleanly was not fenced when the lease lapsed: running=%v stops=%d", up, stops)
	}
}

// The state of a group that a release or a demotion dropped between a round and the write of the watchdog file is not dereferenced (a cluster test of the
// rollback of a map group switchover crashed kingresd on it).
func TestTheWatchdogFileOfAGroupWhoseStateWasDroppedIsNotWritten(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	wf := filepath.Join(r.dir, "instance", "lease.json")
	if err := os.MkdirAll(filepath.Dir(wf), 0o700); err != nil {
		t.Fatal(err)
	}
	r.m.c.WatchdogFile = wf
	r.m.Renew(ctx)
	r.m.mu.Lock()
	delete(r.m.groups, 1)
	r.m.mu.Unlock()
	r.m.writeWatchdogFile(1) // must not panic
}

// F6-05. The three writers of the watchdog file (a round, the idle refresh, the Off file of a demotion) write under one lock, from the reading of the state to the end
// of the write: a round that took its sequence number before a demotion cannot write after the demotion's Off file and arm the watchdog of a standby.
func TestTheWritersOfTheWatchdogFileExcludeEachOther(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	wf := filepath.Join(r.dir, "instance", "lease.json")
	if err := os.MkdirAll(filepath.Dir(wf), 0o700); err != nil {
		t.Fatal(err)
	}
	r.m.c.WatchdogFile = wf
	r.m.Renew(ctx)
	for name, write := range map[string]func(){
		"a round":      func() { r.m.writeWatchdogFile(1) },
		"the idle one": func() { r.m.writeWatchdogIdle(wf) },
		"the Off file": func() { r.m.writeWatchdogOff(wf) },
	} {
		r.m.wmu.Lock() // another writer is in the middle of its write
		done := make(chan struct{})
		go func() { write(); close(done) }()
		select {
		case <-done:
			r.m.wmu.Unlock()
			t.Fatalf("%s wrote while another writer held the lock of the file", name)
		case <-time.After(150 * time.Millisecond):
		}
		r.m.wmu.Unlock()
		<-done
	}
}
