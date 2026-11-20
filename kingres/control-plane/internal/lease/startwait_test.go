// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// restart builds the lease manager of a kingresd that starts again over the same
// lease directory, with the instance in the state running, and runs what main
// does before it starts the instance (PrepareStart).
func (r *rig) restart(t *testing.T, running bool) (m *Manager, waiting bool) {
	t.Helper()
	r.inst.mu.Lock()
	r.inst.running = running
	r.inst.mu.Unlock()
	r.audit = nil
	dir := filepath.Join(r.dir, "lease")
	waiting, err := PrepareStart(dir, func(int32) bool { return running })
	if err != nil {
		t.Fatal(err)
	}
	m, err = New(Config{Self: "a", Clock: r.clk, Duration: dur, Renew: 2 * time.Second, Margin: margin,
		Grantor: r.m.c.Grantor, Voter: r.voter, Source: failingSource{}, Instance: r.inst,
		Enforced: func(int32) bool { return *r.enf }, Dir: dir,
		Audit: func(_ context.Context, action string, _ int32, _ string) { r.audit = append(r.audit, action) }})
	if err != nil {
		t.Fatal(err)
	}
	return m, waiting
}

// A server of a cold-started cluster comes up before a majority: its primary is not
// started, nothing is fenced, nothing is audited, and the lease shows as not held.
// When the majority arrives the primary is started once.
func TestColdStartWithoutAMajorityStartsNothingAndFencesNothing(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx) // the first run: the lease is won, the group is recorded as enforced
	r.settle(t)
	dir := filepath.Join(r.dir, "lease")

	m, waiting := r.restart(t, false) // an orderly stop of the whole cluster, then a start
	if !waiting {
		t.Fatal("the lease is enforced and the instance is not running: it must wait for the lease")
	}
	if err := Gate(dir, testPrimary)(1); err == nil {
		t.Fatal("the start gate must refuse while the instance waits for its lease")
	}
	r.voter.down["b"], r.voter.down["c"] = true, true // the others are still starting
	r.clk.Advance(dur + margin)                       // the grants of the first run are over
	for i := 0; i < 5; i++ {
		m.Renew(ctx)
		r.clk.Advance(2 * time.Second)
		m.Check(ctx)
	}
	m.wg.Wait()
	if up, stops, starts := r.inst.state(); up || stops != 0 || starts != 0 {
		t.Fatalf("running=%v stops=%d starts=%d: nothing may start or stop without a majority", up, stops, starts)
	}
	if len(r.audit) != 0 {
		t.Fatalf("audit events for a server that never held the lease in this run: %v", r.audit)
	}
	if _, err := os.Stat(filepath.Join(dir, "fenced.1")); err == nil {
		t.Fatal("a fence marker was written for an instance that was never started")
	}
	if i := m.Info(); len(i) != 1 || i[0].State != StateNone || !i[0].Enforced {
		t.Fatalf("the lease must show as not held yet (none), not lost: %+v", i)
	}

	r.voter.down["b"], r.voter.down["c"] = false, false // the majority is up
	m.Renew(ctx)
	m.wg.Wait()
	if up, stops, starts := r.inst.state(); !up || stops != 0 || starts != 1 {
		t.Fatalf("running=%v stops=%d starts=%d: the primary must start once, when the lease is held", up, stops, starts)
	}
	if !m.Holds(1) {
		t.Fatal("the lease must be held")
	}
	if err := Gate(dir, testPrimary)(1); err != nil {
		t.Fatalf("the gate must open once the instance was started: %v", err)
	}
	r.clk.Advance(2 * time.Second)
	m.Renew(ctx)
	m.wg.Wait()
	m.Check(ctx)
	if _, stops, starts := r.inst.state(); stops != 0 || starts != 1 {
		t.Fatalf("stops=%d starts=%d: it must be started once and stay up", stops, starts)
	}
	if len(r.audit) != 0 {
		t.Fatalf("audit events on a clean cold start: %v", r.audit)
	}
}

// The instance runs when kingresd starts (kingresd was restarted, the database kept
// running): nothing is judged before the first round, and a round that wins the
// lease neither stops nor starts anything.
func TestRunningInstanceIsKeptAcrossARestartOfTheManager(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	r.settle(t)
	m, waiting := r.restart(t, true)
	if waiting {
		t.Fatal("a running instance must not wait")
	}
	if err := Gate(filepath.Join(r.dir, "lease"), testPrimary)(1); err != nil {
		t.Fatal("the gate must be open for a running instance")
	}
	r.clk.Advance(dur * 2) // a long time before the first round has finished
	m.Check(ctx)
	if _, stops, _ := r.inst.state(); stops != 0 {
		t.Fatal("nothing may be judged before the first round has finished")
	}
	m.Renew(ctx)
	m.wg.Wait()
	m.Check(ctx)
	if up, stops, starts := r.inst.state(); !up || stops != 0 || starts != 0 {
		t.Fatalf("running=%v stops=%d starts=%d: a healthy primary must be left alone", up, stops, starts)
	}
	if len(r.audit) != 0 {
		t.Fatalf("audit: %v", r.audit)
	}
}

// A standby instance of another group runs on this server whatever the state of this
// server's own lease.
func TestGateNeverHoldsBackAStandby(t *testing.T) {
	dir := tempDir(t)
	if _, err := PrepareStart(dir, func(int32) bool { return true }); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{FencedMarker + ".1", WaitMarker + ".1"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Gate(dir, testPrimary)(1); err == nil {
		t.Fatal("the primary must be held back")
	}
	if err := Gate(dir, testPrimary)(instance.StandbyBaseID + 2); err != nil {
		t.Fatalf("a standby must not be held back: %v", err)
	}
}

// An instance that was stopped on purpose while kingresd runs (stopThisServerInstance)
// is not started when a lease is acquired: no marker, no start.
func TestOperatorStoppedInstanceIsNotStartedOnAcquisition(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	r.settle(t)
	r.inst.mu.Lock()
	r.inst.running = false // the API stopped it
	r.inst.mu.Unlock()
	for i := 0; i < 3; i++ {
		r.clk.Advance(2 * time.Second)
		r.m.Renew(ctx)
		r.m.wg.Wait()
		r.m.Check(ctx)
	}
	if up, stops, starts := r.inst.state(); up || starts != 0 || stops != 0 {
		t.Fatalf("running=%v stops=%d starts=%d: an instance stopped on purpose stays stopped", up, stops, starts)
	}
	if Waiting(filepath.Join(r.dir, "lease"), 1) {
		t.Fatal("an operator's stop must not create a marker")
	}
}

// A group whose lease is not enforced (no record) is started by kingresd at once and
// behaves as before: a lost lease is reported and nothing is stopped.
func TestNotEnforcedGroupStartsAtOnceAndBehavesAsBefore(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.Renew(ctx)
	r.settle(t)
	m, waiting := r.restart(t, false)
	if waiting {
		t.Fatal("a group that is not enforced must not wait")
	}
	if err := Gate(filepath.Join(r.dir, "lease"), testPrimary)(1); err != nil {
		t.Fatalf("the gate must let kingresd start the instance at once: %v", err)
	}
	r.inst.mu.Lock()
	r.inst.running = true // kingresd's own start
	r.inst.mu.Unlock()
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.clk.Advance(dur + margin)
	m.Renew(ctx)
	m.Check(ctx)
	if up, stops, starts := r.inst.state(); !up || stops != 0 || starts != 0 {
		t.Fatalf("running=%v stops=%d starts=%d", up, stops, starts)
	}
	if len(r.audit) != 1 || r.audit[0] != AuditLeaseLost {
		t.Fatalf("audit: %v", r.audit)
	}
}

// What the manager last knew is on disk, written when it changes, and a group
// without a record is not enforced.
func TestEnforcedRecordFollowsTheManager(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a"}, true)
	dir := filepath.Join(r.dir, "lease")
	if got := readEnforced(dir).Groups; len(got) != 0 {
		t.Fatalf("no record yet: %v", got)
	}
	r.m.Renew(ctx)
	if got := readEnforced(dir).Groups; len(got) != 1 || got[0] != 1 {
		t.Fatalf("the enforced group must be recorded: %v", got)
	}
	*r.enf = false
	r.m.Renew(ctx)
	if got := readEnforced(dir).Groups; len(got) != 0 {
		t.Fatalf("a group that is no longer enforced must leave the record: %v", got)
	}
	if w, err := PrepareStart(dir, func(int32) bool { return false }); err != nil || w {
		t.Fatalf("no enforced group: no waiting (%v, %v)", w, err)
	}
}

// A start that fails keeps the instance held back, and the next round tries again.
func TestFailedStartAfterAcquisitionIsRetried(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a"}, true)
	r.m.Renew(ctx)
	m, _ := r.restart(t, false)
	m.c.Instance = &failingStart{fakeInstance: r.inst, left: 1}
	m.Renew(ctx)
	m.wg.Wait()
	if !Waiting(filepath.Join(r.dir, "lease"), 1) {
		t.Fatal("a failed start must keep the marker")
	}
	r.clk.Advance(2 * time.Second)
	m.Renew(ctx)
	m.wg.Wait()
	if up, _, _ := r.inst.state(); !up || Waiting(filepath.Join(r.dir, "lease"), 1) {
		t.Fatal("the next round must start the instance")
	}
}

type failingStart struct {
	*fakeInstance
	left int
}

func (f *failingStart) Start(ctx context.Context) error {
	if f.left > 0 {
		f.left--
		return errors.New("pg_ctl failed")
	}
	return f.fakeInstance.Start(ctx)
}

// testPrimary is the layout of the tests: the instances from 100 up are standbys, the others
// are primaries of group 1.
func testPrimary(id int) (int32, bool) { return 1, id < instance.StandbyBaseID }
