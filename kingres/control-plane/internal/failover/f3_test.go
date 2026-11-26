// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package failover

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// runUpTo runs the steps up to and including index last and returns them with the env.
func runUpTo(t *testing.T, r *Runner, in Input, last int) ([]task.Step, *task.Env) {
	t.Helper()
	raw, _ := json.Marshal(in)
	steps, _ := r.steps(raw)
	env := &task.Env{Task: task.Task{ID: 1, Input: raw}}
	for _, s := range steps[:last+1] {
		if err := s.Run(context.Background(), env); err != nil && !isSkip(err) {
			t.Fatalf("%s: %v", s.Name, err)
		}
	}
	return steps, env
}

// F3-10: the recovery of prepared transactions and check_cluster act in every database that has a catalog.
func TestPreparedTransactionsAreRecoveredInEveryDatabase(t *testing.T) {
	w := newWorld()
	w.shop = shopNodes()
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	for _, want := range []string{"recover-prepared " + SystemDB, "recover-prepared shop"} {
		if w.index(want) < 0 {
			t.Errorf("%q was not run:\n%s", want, strings.Join(w.log, "\n"))
		}
	}
}

// F3-04: a database whose catalog lacks the old primary is refused at the request, not found after the promotion.
func TestADatabaseWithoutTheOldPrimaryRefusesTheFailover(t *testing.T) {
	w := newWorld()
	w.shop = append([]Node(nil), shopNodes()...)
	w.shop[1].Host = "s9" // the old primary is not in that catalog by host
	ref := w.runner().Preflight(context.Background(), hardInput())
	if ref == nil || ref.Code != "cluster_inconsistent" || !strings.Contains(ref.Message, "shop") {
		t.Fatalf("got %v", ref)
	}
}

// F3-04: a catalog that cannot be resolved after the promotion pauses the task; it never fails it.
func TestADatabaseThatAppearsAfterThePromotionPausesTheTask(t *testing.T) {
	w := newWorld()
	r := w.runner()
	w.fakeClock(r)
	steps, env := runUpTo(t, r, hardInput(), 6)            // check ... promote
	w.shop = append(shopNodes()[:2:2], shopNodes()[3:]...) // a database appeared after the promotion, without the promoted standby
	err := steps[7].Run(context.Background(), env)
	var pe *PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("the catalog step after a database appeared: %v (want a pause)", err)
	}
	var fe *Error
	if errors.As(err, &fe) {
		t.Fatalf("the task would fail: %v", err)
	}
}

// F3-06: a pinned standby whose server cannot be read before anything was promoted is replaced.
func TestAPinnedStandbyThatCannotBeReadIsReplaced(t *testing.T) {
	w := newWorld()
	r := w.runner()
	w.fakeClock(r)
	steps, env := runUpTo(t, r, hardInput(), 5) // check, freeze, stop, lease, choose (s2 pinned)
	if g := w.rows.groups[1]; g.ChosenStandby != 10 {
		t.Fatalf("pinned %d", g.ChosenStandby)
	}
	w.posErr["s2"] = ErrUnreachable // the pinned standby's server dies before the lease is handed over
	w.alerts = nil
	in := hardInput()
	in.AllowDataLoss = true // the loss of an unreadable standby is accepted by the operator
	raw, _ := json.Marshal(in)
	env.Task.Input = raw
	if err := steps[4].Run(context.Background(), env); err != nil {
		t.Fatalf("choose again: %v", err)
	}
	if g := w.rows.groups[1]; g.ChosenStandby != 11 || g.NewServer != "s3" {
		t.Fatalf("the pin was not replaced: %+v", g)
	}
}

// F3-06: a pin whose standby can be read is kept, and the verdict that was accepted is not recomputed for another one.
func TestAPinnedStandbyThatCanBeReadIsKept(t *testing.T) {
	w := newWorld()
	r := w.runner()
	w.fakeClock(r)
	steps, env := runUpTo(t, r, hardInput(), 5)
	w.pos["s3"] = Position{Server: "s3", Group: 1, InRecovery: true, Timeline: 1, ReceiveLsn: "0/9000000", ReplayLsn: "0/9000000", SystemIdentifier: "9"}
	if err := steps[4].Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if g := w.rows.groups[1]; g.ChosenStandby != 10 {
		t.Fatalf("a readable pin was replaced: %+v", g)
	}
}

// F3-11: an instance promoted by hand is not adopted without the operator's acceptance, and is recorded as a possible loss.
func TestAHandPromotedStandbyIsNotAdoptedWithoutAcceptance(t *testing.T) {
	w := newWorld()
	w.promoted["s3"] = true
	w.lease["s3"] = true
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, hardInput())
	var pe *PauseError
	if !errors.As(err, &pe) || name != StepName(1, StepChoose) || !strings.Contains(pe.Reason, "outside this failover") {
		t.Fatalf("%s: %v", name, err)
	}
	if w.index("promote_node") >= 0 {
		t.Fatal("the catalog followed a promotion nobody accepted")
	}
	w2 := newWorld()
	w2.promoted["s3"] = true
	w2.lease["s3"] = true
	r2 := w2.runner()
	w2.fakeClock(r2)
	in := hardInput()
	in.AllowDataLoss = true
	if name, err := runAll(t, r2, in); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var d DataLoss
	if json.Unmarshal(w2.rows.groups[1].DataLoss, &d) != nil || d.Verdict != VerdictPossible {
		t.Fatalf("the adoption has no verdict on record: %s", w2.rows.groups[1].DataLoss)
	}
}

// F3-15: a run that skipped a standby ends completedWithWarnings, and one that did not ends completed.
func TestAWarningMakesTheOutcomeCompletedWithWarnings(t *testing.T) {
	w := newWorld()
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if w.rows.outcome != "completed" {
		t.Fatalf("outcome %q", w.rows.outcome)
	}
	w2 := newWorld()
	r2 := w2.runner()
	w2.fakeClock(r2)
	_, env := runUpTo(t, r2, hardInput(), 14)
	_ = w2.rows.Event(context.Background(), nil, "x", "repointSkipped", nil)
	raw, _ := json.Marshal(hardInput())
	steps, _ := r2.steps(raw)
	if err := steps[len(steps)-1].Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if w2.rows.outcome != "completedWithWarnings" {
		t.Fatalf("outcome %q", w2.rows.outcome)
	}
}

// F3-07: a live old primary is not stopped while no standby can be read.
func TestALiveOldPrimaryIsNotStoppedWithoutAReadableStandby(t *testing.T) {
	w := newWorld()
	w.oldAlive, w.oldWritable = true, true
	w.live["s1"] = "live"
	w.posErr["s2"], w.posErr["s3"] = ErrUnreachable, ErrUnreachable
	r := w.runner()
	w.fakeClock(r)
	in := hardInput()
	in.AllowDataLoss = true
	name, err := runAll(t, r, in)
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != CodeNoCandidate || name != StepName(1, StepStop) {
		t.Fatalf("%s: %v", name, err)
	}
	if w.index("demote") >= 0 {
		t.Fatal("the old primary was stopped")
	}
}

// F3-02, F3-12: a server that does not answer, and of which the leader cannot show that it knew of a standby, is not
// failed over unless the operator accepts it.
func TestAnUnreachablePrimaryWithoutAProvenPlanIsRefused(t *testing.T) {
	w := newWorld()
	w.facts.PrimaryPlanned = 0
	ref := w.runner().Preflight(context.Background(), hardInput())
	if ref == nil || ref.Code != "cluster_inconsistent" || !strings.Contains(ref.Message, "allowDataLoss") {
		t.Fatalf("got %v", ref)
	}
	in := hardInput()
	in.AllowDataLoss = true
	if ref := w.runner().Preflight(context.Background(), in); ref != nil {
		t.Fatalf("an accepted request was refused: %v", ref)
	}
	w.facts.PrimaryPlanned = 1
	if ref := w.runner().Preflight(context.Background(), hardInput()); ref != nil {
		t.Fatalf("a proven plan was refused: %v", ref)
	}
}
