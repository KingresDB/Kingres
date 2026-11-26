// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package lease

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
)

const (
	dur    = 10 * time.Second
	margin = 3 * time.Second
)

func newGrantor(t *testing.T, clk clock.Clock, dir string) *Grantor {
	t.Helper()
	g, err := NewGrantor(clk, margin, dir)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGrantorRefusesAnotherHolderUntilExpiryPlusMargin(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	g := newGrantor(t, clk, tempDir(t))
	if r, _ := g.Grant(1, "a", dur, false); !r.Granted {
		t.Fatal("the first request must be granted")
	}
	clk.Advance(dur + margin - time.Millisecond)
	if r, _ := g.Grant(1, "b", dur, false); r.Granted || r.Holder != "a" || r.WaitMs <= 0 {
		t.Fatalf("b was granted a lease a still has (plus the margin): %+v", r)
	}
	if r, _ := g.Grant(1, "b", dur, true); r.Granted {
		t.Fatal("a dry run must give the same answer")
	}
	clk.Advance(2 * time.Millisecond)
	if r, _ := g.Grant(1, "b", dur, true); !r.Granted {
		t.Fatal("after expiry plus margin another holder is granted")
	}
	if r, _ := g.Grant(1, "a", dur, false); !r.Granted {
		t.Fatal("a dry run granted nothing, the holder may still renew")
	}
	if r, _ := g.Grant(2, "b", dur, false); !r.Granted {
		t.Fatal("groups are independent")
	}
}

func TestGrantorRenewalExtendsTheHold(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	g := newGrantor(t, clk, tempDir(t))
	_, _ = g.Grant(1, "a", dur, false)
	clk.Advance(8 * time.Second)
	_, _ = g.Grant(1, "a", dur, false)
	clk.Advance(8 * time.Second) // 16 s after the first grant, 8 after the renewal
	if r, _ := g.Grant(1, "b", dur, false); r.Granted {
		t.Fatal("a renewed; b must wait for the renewed lease")
	}
}

func TestGrantorRestartDoesNotGrantTwice(t *testing.T) {
	dir := tempDir(t)
	clk := clock.NewFake(time.Unix(0, 0))
	g := newGrantor(t, clk, dir)
	_, _ = g.Grant(1, "a", dur, false)
	// The process restarts: its monotonic clock starts again, and the lease may
	// have been renewed an instant before the crash.
	clk2 := clock.NewFake(time.Unix(100000, 0))
	g2 := newGrantor(t, clk2, dir)
	if r, _ := g2.Grant(1, "b", dur, false); r.Granted {
		t.Fatal("a restarted grantor granted a lease it had granted to a within its lifetime")
	}
	if r, _ := g2.Grant(1, "a", dur, false); !r.Granted {
		t.Fatal("the holder is still the holder after the restart")
	}
	clk2.Advance(dur + margin + time.Millisecond)
	if r, _ := g2.Grant(1, "b", dur, false); !r.Granted {
		t.Fatal("after duration plus margin since the restart the lease is free")
	}
	// b's grant replaced a's in the file.
	g3 := newGrantor(t, clock.NewFake(time.Unix(0, 0)), dir)
	if s := g3.Snapshot(); len(s) != 1 || s[0].Holder != "b" {
		t.Fatalf("the file does not hold the latest holder: %+v", s)
	}
}

func TestGrantorRefusesWhenTheGrantCannotBeRecorded(t *testing.T) {
	dir := tempDir(t)
	clk := clock.NewFake(time.Unix(0, 0))
	g := newGrantor(t, clk, dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil { // the directory is now a file
		t.Skip("cannot break the directory on this system")
	}
	if r, err := g.Grant(1, "a", dur, false); err == nil || r.Granted {
		t.Fatalf("an unrecorded grant was reported: %+v %v", r, err)
	}
	if len(g.Snapshot()) != 0 {
		t.Fatal("an unrecorded grant was remembered")
	}
}

func TestGrantorRejectsACorruptFile(t *testing.T) {
	dir := tempDir(t)
	if err := os.WriteFile(filepath.Join(dir, GrantsFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGrantor(clock.NewFake(time.Unix(0, 0)), margin, dir); err == nil {
		t.Fatal("a corrupt grants file must stop the grantor, not be ignored")
	}
}

// ---- the holder ----

type fakeInstance struct {
	mu      sync.Mutex
	running bool
	stops   int
	starts  int
}

func (f *fakeInstance) Running(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, nil
}
func (f *fakeInstance) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = true
	f.starts++
	return nil
}
func (f *fakeInstance) StopImmediate(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
	f.stops++
	return nil
}
func (f *fakeInstance) state() (running bool, stops, starts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, f.stops, f.starts
}

type fakeSource struct{ t Topology }

func (s fakeSource) Topology(context.Context) (Topology, error) { return s.t, nil }

// fakeVoter answers from real grantors of the other servers; a server in down
// does not answer, and gate (when set) blocks every call until it is closed.
type fakeVoter struct {
	mu       sync.Mutex
	grantors map[string]*Grantor
	down     map[string]bool
	gate     chan struct{}
	asked    []string
	// hang is the servers that accept the call and never answer (a peer that is stopped, a connection that is lost without a reset).
	hang map[string]bool
}

func (v *fakeVoter) Grant(ctx context.Context, server string, req Request) (Reply, error) {
	v.mu.Lock()
	gate, down, g, hang := v.gate, v.down[server], v.grantors[server], v.hang[server]
	v.asked = append(v.asked, server)
	v.mu.Unlock()
	if hang {
		<-ctx.Done()
		return Reply{}, ctx.Err()
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return Reply{}, ctx.Err()
		}
	}
	if down || g == nil {
		return Reply{}, errors.New("unreachable")
	}
	switch {
	case req.Release:
		return g.Release(req.Group, "a")
	case req.TransferTo != "":
		return g.Transfer(req.Group, "a", req.TransferTo, time.Duration(req.DurationMs)*time.Millisecond)
	}
	return g.Grant(req.Group, "a", time.Duration(req.DurationMs)*time.Millisecond, req.DryRun)
}

type rig struct {
	clk   *clock.Fake
	m     *Manager
	inst  *fakeInstance
	voter *fakeVoter
	dir   string
	enf   *bool
	audit []string
}

func newRig(t *testing.T, members []string, enforced bool) *rig {
	t.Helper()
	clk := clock.NewFake(time.Unix(0, 0))
	r := &rig{clk: clk, inst: &fakeInstance{running: true}, dir: tempDir(t), enf: &enforced}
	r.voter = &fakeVoter{grantors: map[string]*Grantor{}, down: map[string]bool{}}
	for _, s := range members {
		if s != "a" {
			r.voter.grantors[s] = newGrantor(t, clk, filepath.Join(tempDir(t), s))
		}
	}
	m, err := New(Config{Self: "a", Clock: clk, Duration: dur, Renew: 2 * time.Second, Margin: margin,
		Grantor: newGrantor(t, clk, filepath.Join(r.dir, "grants")), Voter: r.voter,
		Source: fakeSource{Topology{Groups: []int32{1}, Members: members}}, Instance: r.inst,
		Enforced: func(int32) bool { return *r.enf }, Dir: filepath.Join(r.dir, "lease"),
		Audit: func(_ context.Context, action string, _ int32, _ string) { r.audit = append(r.audit, action) }})
	if err != nil {
		t.Fatal(err)
	}
	r.m = m
	// A round ends when a majority answered; its last request may still be writing into the grantor's directory.
	t.Cleanup(func() { m.wg.Wait(); m.fl.waitAll() })
	return r
}

func TestHolderNeedsAMajority(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.m.Renew(ctx)
	if r.m.Holds(1) {
		t.Fatal("one of three servers is not a majority")
	}
	r.voter.down["b"] = false
	r.m.Renew(ctx)
	if !r.m.Holds(1) {
		t.Fatal("two of three servers are a majority")
	}
}

func TestOneServerClusterGrantsItself(t *testing.T) {
	r := newRig(t, []string{"a"}, true)
	r.m.Renew(context.Background())
	if !r.m.Holds(1) {
		t.Fatal("a one-server cluster must grant itself the lease")
	}
	if len(r.voter.asked) != 0 {
		t.Fatalf("a one-server cluster asked others: %v", r.voter.asked)
	}
}

func TestHoldsAppliesTheMarginAndExpires(t *testing.T) {
	r := newRig(t, []string{"a"}, true)
	r.m.Renew(context.Background())
	r.clk.Advance(dur - margin - time.Millisecond)
	if !r.m.Holds(1) {
		t.Fatal("the lease is valid until duration minus margin")
	}
	r.clk.Advance(2 * time.Millisecond)
	if r.m.Holds(1) {
		t.Fatal("after duration minus margin the holder must not act on the lease")
	}
}

func TestEnforcedLossFencesBeforeAnotherHolderCanBeGranted(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	r.settle(t)
	grantAt := r.clk.Mono() // b and c granted at this instant of their clocks (same fake clock)
	r.voter.down["b"], r.voter.down["c"] = true, true
	// The renewals fail from now on. Walk time forward; the first moment at which
	// Check fences must be before the first moment b or c would grant to another.
	var fencedAt time.Duration = -1
	var freeAt time.Duration = -1
	for step := time.Duration(0); step < 2*dur; step += 100 * time.Millisecond {
		r.clk.Advance(100 * time.Millisecond)
		r.m.Renew(ctx)
		r.m.Check(ctx)
		if _, stops, _ := r.inst.state(); stops > 0 && fencedAt < 0 {
			fencedAt = r.clk.Mono() - grantAt
		}
		if rep, _ := r.voter.grantors["b"].Grant(1, "x", dur, true); rep.Granted && freeAt < 0 {
			freeAt = r.clk.Mono() - grantAt
		}
	}
	if fencedAt < 0 || freeAt < 0 {
		t.Fatalf("fencedAt %v freeAt %v: both must happen", fencedAt, freeAt)
	}
	if fencedAt >= freeAt {
		t.Fatalf("the primary was fenced at +%v but the lease was free at +%v", fencedAt, freeAt)
	}
	if freeAt-fencedAt < margin-200*time.Millisecond {
		t.Fatalf("the margin between fence (+%v) and grant (+%v) is below the safety margin", fencedAt, freeAt)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "lease", "fenced.1")); err != nil {
		t.Fatal("the fence marker was not written")
	}
	if err := Gate(filepath.Join(r.dir, "lease"), testPrimary)(1); err == nil {
		t.Fatal("the start gate must refuse while the marker exists")
	}
}

func TestUnenforcedLossOnlyReports(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, false)
	r.m.Renew(ctx)
	r.settle(t)
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.clk.Advance(dur)
	r.m.Renew(ctx)
	r.m.Check(ctx)
	if _, stops, _ := r.inst.state(); stops != 0 {
		t.Fatal("a group without a standby must not be stopped")
	}
	if i := r.m.Info(); len(i) == 0 || i[0].State != StateLost || i[0].Enforced {
		t.Fatalf("the loss must be reported: %+v", i)
	}
	if len(r.audit) != 1 || r.audit[0] != AuditLeaseLost {
		t.Fatalf("audit: %v", r.audit)
	}
}

func TestLeaseRegainedStartsTheFencedPrimaryAndClearsTheMarker(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	r.settle(t)
	r.voter.down["b"], r.voter.down["c"] = true, true
	r.clk.Advance(dur)
	r.m.Renew(ctx)
	r.m.Check(ctx)
	if up, _, _ := r.inst.state(); up {
		t.Fatal("fenced")
	}
	r.voter.down["b"], r.voter.down["c"] = false, false
	r.clk.Advance(margin + time.Second) // the old lease is over everywhere
	r.m.Renew(ctx)
	r.m.wg.Wait() // the start runs beside the rounds
	if up, _, starts := r.inst.state(); !up || starts != 1 {
		t.Fatal("the primary must start again once the lease is held")
	}
	if err := Gate(filepath.Join(r.dir, "lease"), testPrimary)(1); err != nil {
		t.Fatal("the gate must open again")
	}
	want := []string{AuditLeaseLost, AuditLeaseFenced, AuditLeaseAcquired}
	if len(r.audit) != 3 || r.audit[0] != want[0] || r.audit[1] != want[1] || r.audit[2] != want[2] {
		t.Fatalf("audit: %v", r.audit)
	}
}

// A process that is paused between starting a round and receiving its answers
// must not believe in a lease computed from the time the answers arrive.
func TestPausedRoundGivesNoLeaseFromTheLateAnswers(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	gate := make(chan struct{})
	r.voter.mu.Lock()
	r.voter.gate = gate
	r.voter.mu.Unlock()
	done := make(chan struct{})
	go func() { r.m.Renew(ctx); close(done) }()
	// The round started at mono 0; the process is "paused" for a whole lease.
	for {
		r.voter.mu.Lock()
		n := len(r.voter.asked)
		r.voter.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	r.clk.Advance(dur + time.Second)
	close(gate)
	<-done
	r.settle(t)
	if r.m.Holds(1) {
		t.Fatal("a lease was computed from the arrival of answers that were late by more than the lease")
	}
}

func TestStaleTopologyIsCachedAcrossARestart(t *testing.T) {
	r := newRig(t, []string{"a", "b"}, false)
	r.m.Renew(context.Background())
	m2, err := New(Config{Self: "a", Clock: r.clk, Duration: dur, Renew: time.Second, Margin: margin, Dir: filepath.Join(r.dir, "lease"),
		Grantor: r.m.c.Grantor, Voter: r.voter, Source: failingSource{}, Instance: r.inst})
	if err != nil {
		t.Fatal(err)
	}
	if !m2.haveT || len(m2.topo.Members) != 2 {
		t.Fatalf("the topology was not cached: %+v", m2.topo)
	}
	m2.Renew(context.Background())
	if !m2.Holds(1) {
		t.Fatal("a restart with the catalog unreadable must still renew from the cached topology")
	}
}

type failingSource struct{}

func (failingSource) Topology(context.Context) (Topology, error) {
	return Topology{}, errors.New("down")
}

// settle waits until every reachable fake grantor has recorded the grant of the
// round that just returned: a round returns as soon as it has a majority, and the
// remaining requests are still on their way.
func (r *rig) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		done := true
		r.voter.mu.Lock()
		for s, g := range r.voter.grantors {
			if !r.voter.down[s] && len(g.Snapshot()) == 0 {
				done = false
			}
		}
		r.voter.mu.Unlock()
		if done {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the grantors did not record the round")
}

// A restarted kingresd that finds an enforced primary running is "confirming"
// until its first round is done: the watchdog still counts down the lease of the
// previous run, so the API must not sign anybody in meanwhile.
func TestConfirmingUntilTheFirstRoundAfterAStartIsDone(t *testing.T) {
	r := newRig(t, []string{"a", "b"}, true)
	r.m.Renew(context.Background())
	m2, err := New(Config{Self: "a", Clock: r.clk, Duration: dur, Renew: time.Second, Margin: margin, Dir: filepath.Join(r.dir, "lease"),
		Grantor: r.m.c.Grantor, Voter: r.voter, Source: fakeSource{r.m.topo}, Instance: r.inst,
		Enforced: func(int32) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if !m2.Confirming() {
		t.Fatal("a restarted manager of an enforced group must be confirming before its first round")
	}
	m2.Renew(context.Background())
	if m2.Confirming() {
		t.Fatal("still confirming after the first round")
	}
	// A group that is not enforced never holds the API back.
	m3, err := New(Config{Self: "a", Clock: r.clk, Duration: dur, Renew: time.Second, Margin: margin, Dir: filepath.Join(r.dir, "lease"),
		Grantor: r.m.c.Grantor, Voter: r.voter, Source: fakeSource{r.m.topo}, Instance: r.inst})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(r.dir, "lease", enforcedFile))
	if m3.Confirming() {
		t.Fatal("an unenforced group was held back")
	}
}

// A renewal that fails while the lease still holds does not make the group "confirming": the instance
// runs until the lease ends and the API serves meanwhile (a follower that lost its leader for a few seconds
// keeps working). When the lease has ended the manager judges the group lost and fences it, which the
// requests meet as an instance that is not there; Confirming is only for the time before the first round.
func TestConfirmingIsNotTrueWhileAFailedRenewalLeavesTheLeaseValid(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	if r.m.Confirming() {
		t.Fatal("confirming after a round that won the lease")
	}
	r.voter.down["b"], r.voter.down["c"] = true, true
	for i := 0; i < 2; i++ {
		r.clk.Advance(2 * time.Second)
		r.m.Renew(ctx) // fails: no majority
		r.m.Check(ctx)
		if !r.m.Holds(1) {
			t.Fatal("the lease must still hold in the first seconds of the loss")
		}
		if r.m.Confirming() {
			t.Fatalf("confirming %d s into a loss that the lease still covers: sign-ins would be refused well before the lease ends", 2*(i+1))
		}
	}
	r.clk.Advance(dur) // well past the end of the lease
	r.m.Check(ctx)
	if r.m.Holds(1) {
		t.Fatal("the lease must have ended")
	}
	if running, stops, _ := r.inst.state(); running || stops == 0 {
		t.Fatal("an enforced group that lost its lease must be fenced")
	}
	if r.m.Confirming() {
		t.Fatal("a group that was judged lost is being fenced, not confirmed")
	}
}

// A round ends when a majority has answered: a grantor that never answers (a stopped peer, a connection lost without a reset) does
// not hold the round, and the lease is renewed every time. The wait for the slowest grantor would run the lease out (duration
// minus margin) on every loss of one server.
func TestARoundDoesNotWaitForAGrantorThatNeverAnswers(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.voter.mu.Lock()
	r.voter.hang = map[string]bool{"c": true}
	r.voter.mu.Unlock()
	for i := 0; i < 3; i++ {
		start := time.Now()
		r.m.Renew(ctx)
		if d := time.Since(start); d > time.Second {
			t.Fatalf("round %d took %v: it waited for the grantor that does not answer", i, d)
		}
		if !r.m.Holds(1) {
			t.Fatalf("round %d: the lease was not renewed although two of three servers answered", i)
		}
		r.clk.Advance(2 * time.Second)
	}
}

// flipSource is a topology source that hangs until its context ends once stuck is set (the catalog of the server cannot be read:
// its instance is busy or being restarted).
type flipSource struct {
	t     Topology
	mu    sync.Mutex
	stuck bool
}

func (s *flipSource) Topology(ctx context.Context) (Topology, error) {
	s.mu.Lock()
	stuck := s.stuck
	s.mu.Unlock()
	if stuck {
		<-ctx.Done()
		return Topology{}, ctx.Err()
	}
	return s.t, nil
}

// The read of the topology is bounded: a catalog that does not answer must not stop the rounds (the cached topology is used, as for
// any other error), or the lease runs out because nothing asked for it.
func TestACatalogThatDoesNotAnswerDoesNotStopTheRounds(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	src := &flipSource{t: Topology{Groups: []int32{1}, Members: []string{"a", "b", "c"}}}
	r.m.c.Source = src
	r.m.Renew(ctx) // the topology is known now
	src.mu.Lock()
	src.stuck = true
	src.mu.Unlock()
	r.m.c.Renew = 300 * time.Millisecond
	r.clk.Advance(2 * time.Second)
	start := time.Now()
	r.m.Renew(ctx)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the round waited %v for the catalog", d)
	}
	r.settle(t)
	if r.m.state(1).lastStart != r.clk.Mono() {
		t.Fatal("no round was asked while the catalog did not answer")
	}
}
