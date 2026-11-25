// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/mapgroup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
)

// The callee side of the hard failover of the map group of a live leader's server (design ha-phase5-part2.md).

type fenceWorld struct {
	calls     []string
	running   bool
	stopFails bool // the instance does not stop
	relErr    error
	candErr   error
	transErr  error
	granted   int
	cand      string
}

func (f *fenceWorld) StopImmediate(context.Context, int) error {
	f.calls = append(f.calls, "stop")
	if !f.stopFails {
		f.running = false
	}
	return nil
}
func (f *fenceWorld) Running(context.Context, int) (bool, error) { return f.running, nil }
func (f *fenceWorld) RecordCandidate(_ int32, s string) error {
	if f.candErr == nil {
		f.cand = s
	}
	f.calls = append(f.calls, "candidate "+s)
	return f.candErr
}
func (f *fenceWorld) Relinquish(int32) error {
	f.calls = append(f.calls, "relinquish")
	return f.relErr
}
func (f *fenceWorld) Transfer(_ context.Context, _ int32, to string) (lease.TransferResult, error) {
	f.calls = append(f.calls, "transfer "+to)
	if f.transErr != nil {
		return lease.TransferResult{}, f.transErr
	}
	return lease.TransferResult{Granted: f.granted, Need: 2}, nil
}

func runFence(t *testing.T, w *fenceWorld, r failover.DemoteRequest) (failover.DemoteAnswer, error) {
	t.Helper()
	out, err := fencedStop(context.Background(), w, w, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 1, r)
	if err != nil {
		return failover.DemoteAnswer{}, err
	}
	return out.(failover.DemoteAnswer), nil
}

// The order is the fence: the candidate is named, the renewal ends and the marker is written, the instance stops, and only then is the lease handed over.
func TestTheFencedStopOfTheMapGroupNamesTheCandidateEndsTheRenewalStopsAndThenHandsOverTheLease(t *testing.T) {
	w := &fenceWorld{running: true, granted: 2}
	ans, err := runFence(t, w, failover.DemoteRequest{Group: 0, Mode: "immediate", TransferTo: "s2", Candidate: "s2"})
	if err != nil || !ans.Stopped || !ans.Transferred {
		t.Fatalf("%+v %v", ans, err)
	}
	if got := strings.Join(w.calls, ","); got != "candidate s2,relinquish,stop,transfer s2" {
		t.Fatalf("the order: %s", got)
	}
}

// A primary that is still running after the stop keeps a lease that nobody renews: the lease is not handed over, whoever asked for it (two writable primaries
// otherwise), and the watchdog stops the instance when the lease runs out.
func TestTheLeaseIsNotHandedOverWhileTheOldPrimaryStillRuns(t *testing.T) {
	w := &fenceWorld{running: true, stopFails: true, granted: 2}
	ans, err := runFence(t, w, failover.DemoteRequest{Group: 0, Mode: "immediate", TransferTo: "s2", Candidate: "s2"})
	if err != nil || ans.Stopped || ans.Transferred {
		t.Fatalf("%+v %v", ans, err)
	}
	for _, c := range w.calls {
		if strings.HasPrefix(c, "transfer") {
			t.Fatalf("the lease was handed over: %v", w.calls)
		}
	}
	if w.calls[0] != "candidate s2" || w.calls[1] != "relinquish" {
		t.Fatalf("the renewal must end before the stop: %v", w.calls)
	}
}

// A hand-over that a majority did not accept is not "transferred": the lease runs out by itself and the candidate takes it then.
func TestAHandOverThatNoMajorityAcceptedIsNotReportedAsMade(t *testing.T) {
	w := &fenceWorld{running: true, granted: 1}
	ans, err := runFence(t, w, failover.DemoteRequest{Group: 0, Mode: "immediate", TransferTo: "s2"})
	if err != nil || !ans.Stopped || ans.Transferred {
		t.Fatalf("%+v %v", ans, err)
	}
	w = &fenceWorld{running: true, transErr: errors.New("no members")}
	if ans, err = runFence(t, w, failover.DemoteRequest{Group: 0, Mode: "immediate", TransferTo: "s2"}); err != nil || !ans.Stopped || ans.Transferred {
		t.Fatalf("%+v %v", ans, err)
	}
}

// When the marker or the record cannot be written nothing is stopped: an error answer means that the primary still runs and still holds its lease.
func TestAFenceThatCannotWriteItsMarkerStopsNothing(t *testing.T) {
	w := &fenceWorld{running: true, relErr: errors.New("disk full")}
	if _, err := runFence(t, w, failover.DemoteRequest{Group: 0, Mode: "immediate", TransferTo: "s2", Candidate: "s2"}); err == nil {
		t.Fatal("no error")
	}
	for _, c := range w.calls {
		if c == "stop" {
			t.Fatalf("stopped after the marker failed: %v", w.calls)
		}
	}
	w = &fenceWorld{running: true, candErr: errors.New("disk full")}
	if _, err := runFence(t, w, failover.DemoteRequest{Group: 0, Mode: "immediate", TransferTo: "s2", Candidate: "s2"}); err == nil || len(w.calls) != 1 {
		t.Fatalf("a failed record must end the call before the lease renewal does: %v %v", err, w.calls)
	}
}

// The ordinary groups do not record a candidate (only the map group's hold needs the word of the server it was handed to).
func TestOnlyTheMapGroupRecordsACandidate(t *testing.T) {
	w := &fenceWorld{running: true, granted: 2}
	if _, err := runFence(t, w, failover.DemoteRequest{Group: 3, Mode: "immediate", TransferTo: "s2", Candidate: "s2"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.calls, ","); got != "relinquish,stop,transfer s2" {
		t.Fatalf("%s", got)
	}
	w = &fenceWorld{running: true, granted: 2}
	if _, err := runFence(t, w, failover.DemoteRequest{Group: 0, Mode: "immediate", TransferTo: "s2"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.calls, ","); got != "relinquish,stop,transfer s2" {
		t.Fatalf("without a candidate: %s", got)
	}
}

// The request of the old leader's own task: from that server only; a hard failover keeps the acceptance of the operator and the facts it carries, a planned switchover
// has none (there is no loss to accept).
func TestTheHardRequestOfTheOldLeaderKeepsTheDecisionAndTheFacts(t *testing.T) {
	m, _ := abandonRig(t)
	mk := func(hard bool) *mapStartBody {
		return &mapStartBody{Server: "s1", AllowDataLoss: true, Planned: &journal.Planned{TaskID: 7, StandbyID: 3, Hard: hard,
			Facts: &journal.PlannedFacts{SyncStreaming: true}}}
	}
	b := mk(true)
	if err := acceptPlanned(m, "s1", b); err != nil || !b.AllowDataLoss || b.Planned.Facts == nil || !b.Planned.Facts.SyncStreaming || b.Planned.Checkpoint != "" {
		t.Fatalf("hard: %+v %v", b, err)
	}
	b = mk(false)
	if err := acceptPlanned(m, "s1", b); err != nil || b.AllowDataLoss || b.Planned.Facts != nil {
		t.Fatalf("planned: %+v %v", b, err)
	}
	callErr(t, acceptPlanned(m, "s9", mk(true)), 403, "forbidden")
	bad := mk(true)
	bad.Planned.StandbyID = 0
	callErr(t, acceptPlanned(m, "s1", bad), 403, "forbidden")
}

// A hard failover that the old leader's task gave up is not started again by the same task, as for a planned switchover.
func TestAGivenUpHardFailoverIsNotStartedAgainByTheSameTask(t *testing.T) {
	m, js := abandonRig(t)
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	_, _ = js.SetPlanned(journal.Planned{TaskID: 7, StandbyID: 3, Hard: true})
	abandonWatch(m)
	if ok, err := m.abandonLocal(context.Background(), "operator"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	b := &mapStartBody{Server: "s1", Planned: &journal.Planned{TaskID: 7, StandbyID: 3, Hard: true}}
	callErr(t, acceptPlanned(m, "s1", b), 409, "conflict")
}

// The repeated request of an operator for a hard failover that paused is taken by the bootstrap even though the old server is heard: the old leader's task
// cannot take it, its system database is the stopped primary.
func TestOnlyAHardFailoverOfALiveLeadersServerIsInProgress(t *testing.T) {
	m, js := abandonRig(t)
	m.runner = &mapgroup.Runner{Journal: js}
	m.self = "s2"
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	if m.runner.HardInProgress("s1") {
		t.Fatal("an ordinary bootstrap is not a hard failover of a live leader's server")
	}
	_, _ = js.SetPlanned(journal.Planned{TaskID: 7, StandbyID: 3, Hard: true})
	if !m.runner.HardInProgress("s1") || m.runner.HardInProgress("s0") {
		t.Fatal("HardInProgress")
	}
}

func (f *fenceWorld) Candidate(int32) string { return f.cand }
func (f *fenceWorld) ForgetCandidate(int32) {
	f.calls = append(f.calls, "forget")
	f.cand = ""
}

// M1. A record that names another server is not overwritten (the real manager refuses it), nothing is stopped; a record this call wrote is removed when the
// marker cannot be written.
func TestAFencedStopDoesNotMoveARecordedCandidateAndDoesNotLeaveItsOwn(t *testing.T) {
	w := &fenceWorld{running: true, granted: 2, cand: "s3", candErr: errors.New("handed to s3 already")}
	if _, err := runFence(t, w, failover.DemoteRequest{Group: 0, Mode: "immediate", TransferTo: "s2", Candidate: "s2"}); err == nil {
		t.Fatal("a different candidate was accepted")
	}
	for _, c := range w.calls {
		if c == "stop" || c == "relinquish" {
			t.Fatalf("something was done after the refusal: %v", w.calls)
		}
	}
	w = &fenceWorld{running: true, relErr: errors.New("disk full")}
	if _, err := runFence(t, w, failover.DemoteRequest{Group: 0, Mode: "immediate", TransferTo: "s2", Candidate: "s2"}); err == nil {
		t.Fatal("no error")
	}
	if w.cand != "" {
		t.Fatalf("the record outlived the failed call: %q", w.cand)
	}
	w = &fenceWorld{running: true, relErr: errors.New("disk full"), cand: "s2"}
	_, _ = runFence(t, w, failover.DemoteRequest{Group: 0, Mode: "immediate", TransferTo: "s2", Candidate: "s2"})
	if w.cand != "s2" {
		t.Fatal("a record of an earlier attempt was removed by a call that did not write it")
	}
}

// P6. A hard failover request that carries a proof is refused at the door (it is not the request of an honest task), and a planned switchover's proof is still cleared.
func TestAHardRequestThatCarriesAProofIsRefused(t *testing.T) {
	m, _ := abandonRig(t)
	b := &mapStartBody{Server: "s1", Planned: &journal.Planned{TaskID: 7, StandbyID: 3, Hard: true, Checkpoint: "0/6000028"}}
	callErr(t, acceptPlanned(m, "s1", b), 400, "bad_request")
	b = &mapStartBody{Server: "s1", Planned: &journal.Planned{TaskID: 7, StandbyID: 3, Hard: true, ClusterState: "shut down"}}
	callErr(t, acceptPlanned(m, "s1", b), 400, "bad_request")
}

// P4. The repeated request of an operator for a hard failover in progress names the target the journal has, or none when it has none.
func TestARepeatedHardRequestWithAnotherTargetIsRefused(t *testing.T) {
	m, js := abandonRig(t)
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	_, _ = js.SetPlanned(journal.Planned{TaskID: 7, StandbyID: 3, Hard: true, Target: "s3"})
	req := func(target string) failover.Request {
		r := failover.Request{Mode: failover.ModeHard}
		if target != "" {
			r.Targets = []failover.TargetRequest{{ShardGroupID: 0, ServerID: target}}
		}
		return r
	}
	if err := m.targetMatchesTheJournal("s1", req("s3")); err != nil {
		t.Fatalf("the same target: %v", err)
	}
	for _, other := range []string{"s4", ""} {
		if err := m.targetMatchesTheJournal("s1", req(other)); err == nil || !strings.Contains(err.Error(), "abandon") {
			t.Fatalf("target %q: %v", other, err)
		}
	}
	if err := m.targetMatchesTheJournal("s9", req("s4")); err != nil {
		t.Fatalf("another server is not this journal's: %v", err)
	}
}

// P2. The operator declares the candidate lost: the hold of the old leader ends without its word, only when the hold was made for that server (or has no record), the
// server is not heard, and every other server answers and none is promoted or running a bootstrap.
func TestADeclaredLostCandidateEndsTheHoldWithoutItsWord(t *testing.T) {
	ctx := context.Background()
	in := mapBootstrapStatus{Standby: "in_recovery"}
	down := errors.New("down")
	cases := []struct {
		name    string
		cand    string
		lost    string
		live    string
		answers map[string]any
		want    bool
	}{
		{"the candidate is lost and the others show a standby in recovery", "s2", "s2", "dead", map[string]any{"s2": down, "s3": in}, true},
		{"no record of the hold", "", "s2", "dead", map[string]any{"s2": down, "s3": in}, true},
		{"the hold was made for another server", "s3", "s2", "dead", map[string]any{"s2": down, "s3": in}, false},
		{"the server is heard", "s2", "s2", "live", map[string]any{"s2": in, "s3": in}, false},
		{"another server does not answer", "s2", "s2", "dead", map[string]any{"s2": down, "s3": down}, false},
		{"another standby was promoted", "s2", "s2", "dead", map[string]any{"s2": down, "s3": mapBootstrapStatus{Standby: "primary"}}, false},
		{"a bootstrap that names the old leader is active elsewhere", "s2", "s2", "dead", map[string]any{"s2": down, "s3": mapBootstrapStatus{Active: true, Standby: "in_recovery"}}, false},
		{"without the declaration the dead candidate blocks", "s2", "", "dead", map[string]any{"s2": down, "s3": in}, false},
	}
	for _, c := range cases {
		r := newResolveRig(t, c.answers)
		r.cand = c.cand
		r.m.heldBack = func() bool { return true }
		r.m.liveOf = func(context.Context, string) string { return c.live }
		got, why := r.m.resolveHeldBack(ctx, true, c.lost)
		if got != c.want || (!got && why == "") || (got && r.ended != 1) {
			t.Errorf("%s: resumed %v (%q), ended %d; want %v", c.name, got, why, r.ended, c.want)
		}
	}
}

// P2. The operator's call with a lost server: refused (409) while the cluster hears that server, and the declaration reaches the resume call of every server.
func TestTheOperatorsAbandonCarriesTheLostServerToTheResumeCalls(t *testing.T) {
	ctx := context.Background()
	m, _ := abandonRig(t)
	m.self = "s3"
	m.memberList = func(context.Context) ([]string, error) { return []string{"s1", "s2", "s3", "s4"}, nil }
	m.oldLeaderOf = func(context.Context) (string, error) { return "s4", nil } // the catalog names s4 as the primary of the map group
	m.heldBack = func() bool { return false }
	m.liveOf = func(_ context.Context, s string) string {
		if s == "s2" {
			return "live"
		}
		return "dead"
	}
	if _, err := m.Abandon(ctx, "ops", opID, "x", "s2"); err == nil || !strings.Contains(err.Error(), "heard") {
		t.Fatalf("a heard server was declared lost: %v", err)
	}
	var lost []string
	m.callPeer = func(_ context.Context, server, _, path string, in, out any) error {
		if path == "/failover/map/held" {
			b, _ := json.Marshal(mapHeldAnswer{Held: server == "s4"}) // only s4 holds its primary back
			return json.Unmarshal(b, out)
		}
		if path == "/failover/map/resume" {
			if server != "s4" || in.(mapResumeBody).Proof == nil {
				t.Errorf("the declaration was sent to %s (proof %v): only the held-back server gets it, with a proof", server, in.(mapResumeBody).Proof != nil)
			}
			lost = append(lost, in.(mapResumeBody).Lost)
			b, _ := json.Marshal(mapResumeAnswer{Resumed: server == "s4"})
			return json.Unmarshal(b, out)
		}
		b, _ := json.Marshal(mapAbandonAnswer{})
		return json.Unmarshal(b, out)
	}
	res, err := m.Abandon(ctx, "ops", opID, "x", "s9")
	if err != nil || res.ResumedServer != "s4" || len(lost) == 0 || lost[0] != "s9" {
		t.Fatalf("%+v %v %v", res, err, lost)
	}
}

// opID is the identity of an operator's session in the tests of the abandon call.
var opID = internalapi.ForwardIdentity{Username: "ops", ClusterRoles: []string{"kingres_admin"}, ProofKey: make([]byte, 32)}
