// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

// schemaError tells the refusals of the extension by SQLSTATE: KS020 (pinned), 0A000 (not distributable) and 42710 (already distributed).
// The wording is not read, except for the pin of the extension 0.8, which gave 55000 with "schema_pinned" in the text.
func TestSchemaErrorMapsTheExtensionsSQLSTATEs(t *testing.T) {
	cases := []struct {
		code, msg string
		want      problem.Code
	}{
		{"KS020", `schema_pinned: schema "a" holds the ledger table`, problem.SchemaPinned},
		{"KS020", `the schema cannot move any more: another wording`, problem.SchemaPinned},
		{"55000", `schema_pinned: schema "a" holds the ledger table`, problem.SchemaPinned}, // extension 0.8
		{"0A000", `CREATE MATERIALIZED VIEW is not supported in a distributed schema yet`, problem.SchemaNotDistributable},
		{"0A000", `another wording`, problem.SchemaNotDistributable},
		{"42710", `schema "a" is already distributed`, problem.Conflict},
		{"42710", `another wording`, problem.Conflict},
	}
	for _, c := range cases {
		err := schemaError(&pgconn.PgError{Code: c.code, Message: c.msg})
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != c.want {
			t.Errorf("%s %q: %v", c.code, c.msg, err)
		}
	}
	// the wording under another SQLSTATE is nothing: the error keeps its text
	for _, msg := range []string{`schema "a" is already distributed`, `x is not supported`, `schema "a" cannot be distributed`, `schema_pinned: x`} {
		err := schemaError(&pgconn.PgError{Code: "XX000", Message: msg})
		var p *problem.Problem
		if errors.As(err, &p) || err == nil || err.Error() != msg {
			t.Errorf("%q under XX000: %v", msg, err)
		}
	}
	if schemaError(nil) != nil {
		t.Error("nil stays nil")
	}
	if err := schemaError(&pgconn.PgError{Message: "boom"}); err == nil || err.Error() != "boom" {
		t.Errorf("other errors keep their text: %v", err)
	}
}

// A catalog function called on a node that is not the map group primary answers KS022 (KS010 of the extension 0.8 and 55000 stay accepted).
func TestIsNotMapPrimary(t *testing.T) {
	for code, want := range map[string]bool{"KS022": true, "KS010": true, "55000": true, "KS011": false, "XX000": false, "": false} {
		if got := isNotMapPrimary(&pgconn.PgError{Code: code, Message: "this function can only run on the map group primary"}); got != want {
			t.Errorf("%q: %v", code, got)
		}
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

// Only the refusal of the extension for a move that is switching is "too late"; a failure of the call itself is tried again.
func TestCancelRefusedIsTheExtensionsRefusalOnly(t *testing.T) {
	if !cancelRefused(&pgconn.PgError{Code: "55000", Message: "bucket move 3 is switching and cannot be cancelled"}) {
		t.Error("55000 is the refusal")
	}
	for _, err := range []error{errors.New("conn closed"), &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}, &pgconn.PgError{Code: "08006"}, nil} {
		if cancelRefused(err) {
			t.Errorf("%v is not a refusal", err)
		}
	}
}

// The note of a cancel request that came too late stays on the task when the clean-up step runs; any other detail is replaced.
func TestCleanUpStepKeepsTheLateCancelNote(t *testing.T) {
	for _, tc := range []struct {
		first    string
		wantKept bool
	}{{moveTooLateText + ": the move had passed the point", true}, {"cleaning: the schema is served by its new group", false}} {
		mem := task.NewMemStore()
		o := &Ops{}
		def := task.Definition{Kind: "test.move", Steps: []task.Step{
			{Name: "copy", Run: func(ctx context.Context, env *task.Env) error { return env.SetDetail(ctx, tc.first) }},
			{Name: "clean", Run: o.sMoveClean},
		}}
		eng, err := task.NewEngine(mem, []task.Definition{def}, task.Options{Holder: "A", Lease: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		tk, _ := eng.Submit(ctx, "test.move", task.Subject{Type: "schema", ID: "d/s"}, nil, "u")
		if _, err := eng.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := eng.Get(ctx, tk.ID)
		if kept := strings.HasPrefix(got.Detail, moveTooLateText); kept != tc.wantKept {
			t.Errorf("detail %q after the clean-up step, kept %v, want %v", got.Detail, kept, tc.wantKept)
		}
	}
}
