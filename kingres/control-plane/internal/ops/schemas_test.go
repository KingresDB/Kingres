// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ops

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func TestNotDistributableListsEveryBlocker(t *testing.T) {
	err := notDistributable([]api.SchemaBlocker{
		{ObjectKind: "materialized view", ObjectName: "t7.mv", Reason: "not supported"},
		{ObjectKind: "table", ObjectName: "t7.log", Reason: "unlogged"},
	})
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != problem.SchemaNotDistributable || len(p.Errors) != 2 || p.Errors[0].Field != "t7.mv" {
		t.Fatalf("%v", err)
	}
}

func TestSchemaErrorMapsTheExtensionsMessages(t *testing.T) {
	cases := map[string]problem.Code{
		`schema_pinned: schema "a" holds the ledger table`:                      problem.SchemaPinned,
		`CREATE MATERIALIZED VIEW is not supported in a distributed schema yet`: problem.SchemaNotDistributable,
		`schema "a" is already distributed`:                                     problem.Conflict,
	}
	for msg, want := range cases {
		err := schemaError(&pgconn.PgError{Message: msg})
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != want {
			t.Errorf("%q: %v", msg, err)
		}
	}
	if schemaError(nil) != nil {
		t.Error("nil stays nil")
	}
	if err := schemaError(&pgconn.PgError{Message: "boom"}); err == nil || err.Error() != "boom" {
		t.Errorf("other errors keep their text: %v", err)
	}
}

func TestQuoteIdentKeepsHostileNamesInert(t *testing.T) {
	if got := quoteIdent(`a"; DROP SCHEMA x; --`); got != `"a""; DROP SCHEMA x; --"` {
		t.Fatal(got)
	}
	if quoteIdent("Tenant 7") != `"Tenant 7"` {
		t.Fatal("quoting keeps case and blanks")
	}
}

func TestMoveCancellableUntilTheSwitch(t *testing.T) {
	mk := func(states map[string]string) task.Task {
		var steps []task.StepState
		for n, s := range states {
			steps = append(steps, task.StepState{Name: n, State: s})
		}
		return task.Task{Steps: steps}
	}
	if !moveCancellable(mk(map[string]string{stepMoveCheck: task.StepDone, stepMoveCopy: task.StepRunning})) {
		t.Error("cancellable while copying")
	}
	if moveCancellable(mk(map[string]string{stepMoveCopy: task.StepDone, stepMoveSwitch: task.StepRunning})) {
		t.Error("not cancellable in the switch")
	}
	if moveCancellable(mk(map[string]string{stepMoveSwitch: task.StepDone, stepMoveClean: task.StepDone})) {
		t.Error("not cancellable when finished")
	}
}

// The switch of a move happens inside the call of the copy step, so the guard reads the phase that the step records: a request that
// arrives while the extension is switching (or after the call has ended) is refused, and so is the mark of one that arrived before.
func TestMoveCancelGuardFollowsThePhaseOfTheCopyStep(t *testing.T) {
	copying := func(detail string, copy string) task.Task {
		return task.Task{Detail: detail, Steps: []task.StepState{{Name: stepMoveCheck, State: task.StepDone}, {Name: stepMoveCopy, State: copy}}}
	}
	for detail, want := range map[string]bool{
		"preparing: checking and recording the move":                true,
		"copying: copying the rows to shard group 2":                true,
		"catching_up: the copy on shard group 2 catches up":         true,
		"switching: the writers of this schema wait for the switch": false,
		"cleaning: the schema is served by its new group":           false,
		"done: ": false,
		moveTooLateText + " (copying: copying the rows to shard group 2)": false,
		"": true,
	} {
		if got := moveCancellable(copying(detail, task.StepRunning)); got != want {
			t.Errorf("detail %q: cancellable %v, want %v", detail, got, want)
		}
	}
	// the copy step is done: the schema is on its target, the mark of a request that came late is not followed
	if moveCancellable(copying("", task.StepDone)) {
		t.Error("not cancellable once the copy step is done")
	}
}

func TestUndistributeCancelGuard(t *testing.T) {
	mk := func(detail string, states ...string) task.Task {
		names := []string{stepUndistCheck, stepUndistMoveFirst, stepUndistMake}
		t := task.Task{Detail: detail}
		for i, s := range states {
			t.Steps = append(t.Steps, task.StepState{Name: names[i], State: s})
		}
		return t
	}
	if !undistributeCancellable(mk("", task.StepRunning)) {
		t.Error("cancellable in the check")
	}
	if !undistributeCancellable(mk("copying: x", task.StepDone, task.StepRunning)) {
		t.Error("cancellable while the move first copies")
	}
	if undistributeCancellable(mk("switching: x", task.StepDone, task.StepRunning)) {
		t.Error("not cancellable while the move first switches")
	}
	// the move first is over (its detail still says cleaning): the task can still be cancelled before the schema is made local
	if !undistributeCancellable(mk("cleaning: x", task.StepDone, task.StepDone, task.StepPending)) {
		t.Error("cancellable between the move first and the making local")
	}
	if undistributeCancellable(mk("", task.StepDone, task.StepDone, task.StepRunning)) {
		t.Error("not cancellable while the schema is made local")
	}
}

func TestMoveRecordEnds(t *testing.T) {
	for phase, want := range map[string]bool{"preparing": false, "copying": false, "catching_up": false, "switching": false, "cleaning": true, "done": true, "failed": true, "cancelled": true} {
		if got := (moveRecord{Phase: phase}).terminal(); got != want {
			t.Errorf("%s terminal %v", phase, got)
		}
	}
	for phase, want := range map[string]bool{"preparing": true, "copying": true, "catching_up": true, "switching": false, "cleaning": false} {
		if got := (moveRecord{Phase: phase}).stoppable(); got != want {
			t.Errorf("%s stoppable %v", phase, got)
		}
	}
	var coded task.Coded = moveCancelled{group: 3}
	if coded.TaskCode() != task.CodeCanceled || coded.TaskRetryable() {
		t.Error("an undone move fails with the code cancelled")
	}
}
