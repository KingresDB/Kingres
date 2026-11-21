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
