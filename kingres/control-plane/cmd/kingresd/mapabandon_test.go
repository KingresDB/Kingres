// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

func abandonRig(t *testing.T) (*mapFailover, *journal.Store) {
	t.Helper()
	js, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &mapFailover{journal: js, log: slog.New(slog.NewTextHandler(io.Discard, nil)), self: "s2"}, js
}

// F4-06. An operator gives up a bootstrap that did not reach the promotion: it ends at once, nothing is in progress after it, and a second call
// finds none.
func TestABootstrapThatDidNotPromoteCanBeGivenUp(t *testing.T) {
	m, js := abandonRig(t)
	if _, err := js.Begin("b1", "s1", 3, "manual", "alice", false); err != nil {
		t.Fatal(err)
	}
	ok, err := m.abandonLocal(context.Background(), "stuck")
	if err != nil || !ok {
		t.Fatalf("abandon: %v %v", ok, err)
	}
	if js.Active() {
		t.Fatal("the bootstrap is still in progress")
	}
	if ok, err := m.abandonLocal(context.Background(), "again"); ok || err != nil {
		t.Fatalf("a second call: %v %v", ok, err)
	}
}

// A bootstrap that promoted the standby cannot be given up: 409, and it stays in progress.
func TestABootstrapThatPromotedCannotBeGivenUp(t *testing.T) {
	m, js := abandonRig(t)
	if _, err := js.Begin("b1", "s1", 3, "manual", "alice", false); err != nil {
		t.Fatal(err)
	}
	if err := js.Done(journal.StepPromote); err != nil {
		t.Fatal(err)
	}
	ok, err := m.abandonLocal(context.Background(), "stuck")
	var p *problem.Problem
	if ok || !errors.As(err, &p) || p.Code != problem.Conflict {
		t.Fatalf("abandon after the promotion: %v %v", ok, err)
	}
	if !js.Active() {
		t.Fatal("the bootstrap was given up after it promoted")
	}
}

// Nothing in progress: false, no error.
func TestNothingToGiveUp(t *testing.T) {
	m, _ := abandonRig(t)
	if ok, err := m.abandonLocal(context.Background(), "x"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
}

type abandonSeen struct {
	released int
	told     []string
}

func abandonWatch(m *mapFailover) *abandonSeen {
	s := &abandonSeen{}
	m.releaseFence = func(context.Context) error { s.released++; return nil }
	m.tellOldLeader = func(_ context.Context, old string, task int64, _ string) error {
		s.told = append(s.told, fmt.Sprintf("%s/%d", old, task))
		return nil
	}
	return s
}

// F5-02. A bootstrap that is given up lets go of the fence and holder lease it holds: an old primary that is stopped must be able to win its lease again.
func TestAnAbandonedBootstrapLetsGoOfItsLease(t *testing.T) {
	m, js := abandonRig(t)
	seen := abandonWatch(m)
	if _, err := js.Begin("b1", "s1", 3, "manual", "alice", false); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.abandonLocal(context.Background(), "stuck"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if seen.released != 1 || len(seen.told) != 0 {
		t.Fatalf("released %d, told %v", seen.released, seen.told)
	}
	// nothing to give up: nothing released
	if ok, _ := m.abandonLocal(context.Background(), "again"); ok || seen.released != 1 {
		t.Fatalf("a second call released again: %v %d", ok, seen.released)
	}
}

// A bootstrap that promoted keeps its lease.
func TestABootstrapThatPromotedKeepsItsLease(t *testing.T) {
	m, js := abandonRig(t)
	seen := abandonWatch(m)
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	_ = js.Done(journal.StepPromote)
	if _, err := m.abandonLocal(context.Background(), "x"); err == nil || seen.released != 0 {
		t.Fatalf("%v released %d", err, seen.released)
	}
}

// F5-08. A planned bootstrap that is given up also tells the old leader, whose task rolls back.
func TestAGivenUpPlannedBootstrapTellsTheOldLeader(t *testing.T) {
	m, js := abandonRig(t)
	seen := abandonWatch(m)
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	_, _ = js.SetPlanned(journal.Planned{TaskID: 42, StandbyID: 3})
	if ok, err := m.abandonLocal(context.Background(), "operator"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if seen.released != 1 || len(seen.told) != 1 || seen.told[0] != "s1/42" {
		t.Fatalf("released %d, told %v", seen.released, seen.told)
	}
}

// F5-08. The call between servers: a planned bootstrap is given up by the old leader or a standby's server, not by any member.
func TestOnlyTheOldLeaderOrACandidateGivesUpAPlannedBootstrap(t *testing.T) {
	m, js := abandonRig(t)
	abandonWatch(m)
	m.candidateCheck = func(_ context.Context, s string) bool { return s == "s3" }
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	_, _ = js.SetPlanned(journal.Planned{TaskID: 42, StandbyID: 3})
	d := &haDeps{MapFailover: m}
	body := []byte(`{"reason":"x"}`)
	_, err := handleMapAbandon(context.Background(), d, "s9", body)
	callErr(t, err, 403, "forbidden")
	if !js.Active() {
		t.Fatal("a member that is not the old leader gave the bootstrap up")
	}
	out, err := handleMapAbandon(context.Background(), d, "s1", body)
	if err != nil || out.(mapAbandonAnswer).Abandoned != true {
		t.Fatalf("the old leader: %v %v", out, err)
	}
}

func TestAStandbysServerMayGiveUpAPlannedBootstrap(t *testing.T) {
	m, js := abandonRig(t)
	abandonWatch(m)
	m.candidateCheck = func(_ context.Context, s string) bool { return s == "s3" }
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	_, _ = js.SetPlanned(journal.Planned{TaskID: 42, StandbyID: 3})
	if _, err := handleMapAbandon(context.Background(), &haDeps{MapFailover: m}, "s3", []byte(`{"reason":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if js.Active() {
		t.Fatal("not given up")
	}
}

// F5-02. The monitor gives up a bootstrap that is stuck (older than the bound, or of a term the catalog has left) and lets go of its lease with it.
func TestTheMonitorLetsGoOfTheLeaseOfAStuckBootstrap(t *testing.T) {
	m, js := abandonRig(t)
	seen := abandonWatch(m)
	cur, _ := js.Begin("b1", "s1", 3, "automatic", "system", false)
	if m.abandonIfStuck(context.Background(), cur, 3, cur.StartedAt.Add(time.Minute)) {
		t.Fatal("a young bootstrap was given up")
	}
	if !m.abandonIfStuck(context.Background(), cur, 3, cur.StartedAt.Add(mapAbandonAfter+time.Minute)) || seen.released != 1 {
		t.Fatalf("an old bootstrap: released %d", seen.released)
	}
}

// F5-03. The call that promotes was begun and its answer is not recorded: the bootstrap is given up only when the standby is asked and is still in
// recovery; a standby that cannot be asked, or is out of recovery, keeps the bootstrap.
func TestABegunPromotionIsGivenUpOnlyForAStandbyThatIsStillInRecovery(t *testing.T) {
	cases := []struct {
		name string
		rec  func(context.Context) (bool, error)
		want bool
	}{
		{"still a standby", func(context.Context) (bool, error) { return true, nil }, true},
		{"promoted", func(context.Context) (bool, error) { return false, nil }, false},
		{"cannot be asked", func(context.Context) (bool, error) { return false, errors.New("down") }, false},
		{"no way to ask", nil, false},
	}
	for _, c := range cases {
		m, js := abandonRig(t)
		abandonWatch(m)
		m.inRecovery = c.rec
		_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
		_ = js.PromotionIntent()
		ok, err := m.abandonLocal(context.Background(), "x")
		if ok != c.want || (!c.want && err == nil) || js.Active() == c.want {
			t.Errorf("%s: given up %v err %v active %v", c.name, ok, err, js.Active())
		}
	}
}

// F5-09, F5-12. A refusal belongs to the task that asked for the bootstrap and to the server it asked: another caller, another task or an old refusal is not
// this task's, and a new start clears it.
func TestARefusalBelongsToTheTaskAndTheServerThatWereAsked(t *testing.T) {
	saved := mapPlanned
	mapPlanned = &plannedBox{}
	defer func() { mapPlanned = saved }()
	ctx := context.Background()
	body := func(task int64) []byte {
		b, _ := json.Marshal(mapRefusedBody{Reason: "no proof", TaskID: task})
		return b
	}
	// nothing was started: refused
	_, err := handleMapRefused(ctx, &haDeps{}, "s2", body(7))
	callErr(t, err, 409, "conflict")
	mapPlanned.started("s2", 7)
	_, err = handleMapRefused(ctx, &haDeps{}, "s3", body(7)) // another server
	callErr(t, err, 409, "conflict")
	_, err = handleMapRefused(ctx, &haDeps{}, "s2", body(8)) // another task
	callErr(t, err, 409, "conflict")
	if _, ok := mapPlanned.refusal(7); ok {
		t.Fatal("a refusal that was not accepted is remembered")
	}
	if _, err = handleMapRefused(ctx, &haDeps{}, "s2", body(7)); err != nil {
		t.Fatal(err)
	}
	if r, ok := mapPlanned.refusal(7); !ok || r != "no proof" {
		t.Fatalf("%q %v", r, ok)
	}
	if _, ok := mapPlanned.refusal(8); ok {
		t.Fatal("the refusal of task 7 answers for task 8")
	}
	mapPlanned.started("s2", 9) // a new task clears it
	if _, ok := mapPlanned.refusal(7); ok {
		t.Fatal("an old refusal survived a new start")
	}
}

// F5-12. The planned form of map/start is accepted from the old server only and with a task and a standby; the proof is accepted from the old server only,
// for a planned journal, once.
func TestThePlannedStartAndTheProofAreAcceptedFromTheOldServerOnly(t *testing.T) {
	ctx := context.Background()
	m, js := abandonRig(t)
	d := &haDeps{MapFailover: m}
	start := func(caller string, p *journal.Planned) error {
		b, _ := json.Marshal(mapStartBody{Server: "s1", Planned: p})
		_, err := handleMapStart(ctx, d, caller, b)
		return err
	}
	callErr(t, start("s9", &journal.Planned{TaskID: 7, StandbyID: 3}), 403, "forbidden") // not the old server
	callErr(t, start("s1", &journal.Planned{TaskID: 0, StandbyID: 3}), 403, "forbidden") // no task
	callErr(t, start("s1", &journal.Planned{TaskID: 7, StandbyID: 0}), 403, "forbidden") // no standby
	if js.Active() {
		t.Fatal("a refused start began a journal")
	}
	// the proof: no planned journal yet
	proof := func(caller, cp string) error {
		b, _ := json.Marshal(mapProofBody{Checkpoint: cp, ClusterState: "shut down"})
		_, err := handleMapProof(ctx, d, caller, b)
		return err
	}
	callErr(t, proof("s1", "0/1"), 409, "conflict")
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	_, _ = js.SetPlanned(journal.Planned{TaskID: 7, StandbyID: 3})
	callErr(t, proof("s9", "0/1"), 403, "forbidden")
	if err := proof("s1", "0/6000028"); err != nil {
		t.Fatal(err)
	}
	if err := proof("s1", "0/6000028"); err != nil {
		t.Fatalf("the same proof again: %v", err)
	}
	callErr(t, proof("s1", "0/7000000"), 409, "conflict") // a differing second proof
}

// A planned switchover that was given up is not started again by the same task (its rollback could not be recorded, so the engine runs the step again).
func TestAGivenUpPlannedSwitchoverIsNotStartedAgainByTheSameTask(t *testing.T) {
	ctx := context.Background()
	m, js := abandonRig(t)
	d := &haDeps{MapFailover: m}
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	_, _ = js.SetPlanned(journal.Planned{TaskID: 7, StandbyID: 3})
	if err := js.Abandon("rolled back"); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(mapStartBody{Server: "s1", Planned: &journal.Planned{TaskID: 7, StandbyID: 3}})
	_, err := handleMapStart(ctx, d, "s1", b)
	callErr(t, err, 409, "conflict")
	if js.Active() {
		t.Fatal("the same task began a second bootstrap")
	}
}
