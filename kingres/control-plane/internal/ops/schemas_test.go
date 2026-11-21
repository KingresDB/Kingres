// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// stepsOf builds a task with the named steps in the given states.
func stepsOf(kind string, state task.State, detail string, steps ...task.StepState) task.Task {
	return task.Task{Kind: kind, State: state, Detail: detail, Steps: steps}
}

// A move can be cancelled until the phase switching: not once the Switch or the clean-up step has started, not when the detail
// of the running step says the phase is switching, cleaning or done, and not when the task is over.
func TestSchemaMoveCancellableUntilTheSwitch(t *testing.T) {
	pending := func(names ...string) []task.StepState {
		var out []task.StepState
		for _, n := range names {
			out = append(out, task.StepState{Name: n, State: task.StepPending})
		}
		return out
	}
	move := func(states map[string]string) []task.StepState {
		var out []task.StepState
		for _, n := range []string{StepSchemaMoveCheck, StepSchemaIdentity, StepSchemaCopy, StepSchemaSwitch, StepSchemaCleanup} {
			s := task.StepPending
			if v, ok := states[n]; ok {
				s = v
			}
			out = append(out, task.StepState{Name: n, State: s})
		}
		return out
	}
	for _, tc := range []struct {
		name string
		t    task.Task
		want bool
	}{
		{"pending", stepsOf(KindSchemaMove, task.Pending, "", pending(StepSchemaMoveCheck, StepSchemaCopy)...), true},
		{"copying", stepsOf(KindSchemaMove, task.Running, "copying: copying the rows to shard group 1", move(map[string]string{StepSchemaMoveCheck: task.StepDone, StepSchemaCopy: task.StepRunning})...), true},
		{"catching up", stepsOf(KindSchemaMove, task.Running, "catching_up: the copy catches up", move(map[string]string{StepSchemaCopy: task.StepRunning})...), true},
		{"switching", stepsOf(KindSchemaMove, task.Running, "switching: the writers of this schema wait for the switch", move(map[string]string{StepSchemaCopy: task.StepRunning})...), false},
		{"cleaning", stepsOf(KindSchemaMove, task.Running, "cleaning: the schema is served by its new group", move(map[string]string{StepSchemaCopy: task.StepRunning})...), false},
		{"switch step started", stepsOf(KindSchemaMove, task.Running, "", move(map[string]string{StepSchemaCopy: task.StepDone, StepSchemaSwitch: task.StepRunning})...), false},
		{"clean-up step done", stepsOf(KindSchemaMove, task.Running, "", move(map[string]string{StepSchemaCopy: task.StepDone, StepSchemaSwitch: task.StepDone, StepSchemaCleanup: task.StepDone})...), false},
		{"finished", stepsOf(KindSchemaMove, task.Done, "", move(nil)...), false},
		{"failed", stepsOf(KindSchemaMove, task.Failed, "", move(nil)...), false},
		{"undistribute before the local step", stepsOf(KindSchemaUndistribute, task.Running, "copying: x", task.StepState{Name: StepSchemaMoveFirst, State: task.StepRunning}, task.StepState{Name: StepSchemaLocal, State: task.StepPending}), true},
		{"undistribute at the local step", stepsOf(KindSchemaUndistribute, task.Running, "", task.StepState{Name: StepSchemaMoveFirst, State: task.StepDone}, task.StepState{Name: StepSchemaLocal, State: task.StepRunning}), false},
	} {
		if got := schemaMoveCancellable(tc.t); got != tc.want {
			t.Errorf("%s: cancellable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The blockers of a schema are the entries of errors of the 422: the object is the field, the reason the code.
func TestBlockerProblemListsEveryBlocker(t *testing.T) {
	p := blockerProblem("tenant_1", []api.SchemaBlocker{
		{ObjectKind: "materialized view", ObjectName: `tenant_1."mv"`, Reason: "a materialized view is not supported in a distributed schema yet"},
		{ObjectKind: "rule", ObjectName: "tenant_1.t.r", Reason: "a rule is not supported in a distributed schema yet"},
	})
	if p.Code != problem.SchemaNotDistributable || p.Status != 422 || len(p.Errors) != 2 {
		t.Fatalf("problem = %+v", p)
	}
	if e := p.Errors[0]; e.Field != `tenant_1."mv"` || e.Code != "a materialized view is not supported in a distributed schema yet" || !strings.Contains(e.Message, "materialized view") {
		t.Errorf("first error = %+v", e)
	}
	if !strings.Contains(p.Detail, "2 object") {
		t.Errorf("detail = %q", p.Detail)
	}
}

// What a step records of an error is the message of PostgreSQL with its detail and hint, or the text of a problem.
func TestPgTextKeepsWhatTheExtensionSays(t *testing.T) {
	e := &pgconn.PgError{Code: "55000", Message: "schema \"x\" holds rows on shard group 0, not on group 1", Detail: "d", Hint: "move it afterwards"}
	if got := pgText(e); got != `schema "x" holds rows on shard group 0, not on group 1 (d) move it afterwards` {
		t.Errorf("pgText = %q", got)
	}
	if got := pgText(errors.Join(errors.New("wrapped"), e)); !strings.Contains(got, "holds rows") {
		t.Errorf("wrapped PgError: %q", got)
	}
	p := blockerProblem("s", []api.SchemaBlocker{{ObjectKind: "view", ObjectName: "s.v", Reason: "reads another schema"}})
	if got := pgText(p); !strings.Contains(got, "reads another schema") {
		t.Errorf("problem text = %q", got)
	}
	if got := pgText(errors.New("plain")); got != "plain" {
		t.Errorf("plain = %q", got)
	}
}

// The extension is needed in the version of the schemas: no catalog is a conflict with the state, an old extension is not supported.
func TestSchemaExtNeed(t *testing.T) {
	for _, tc := range []struct {
		e    schemaExt
		code problem.Code
	}{
		{schemaExt{}, problem.Conflict},
		{schemaExt{catalog: true}, problem.UnsupportedOp},
		{schemaExt{catalog: true, schemas: true, mode: "both"}, ""},
	} {
		err := tc.e.need("shop")
		var p *problem.Problem
		switch {
		case tc.code == "" && err != nil:
			t.Errorf("%+v: %v", tc.e, err)
		case tc.code != "" && (!errors.As(err, &p) || p.Code != tc.code):
			t.Errorf("%+v: %v, want %s", tc.e, err, tc.code)
		}
	}
}

// A move that has ended is terminal; one that is still being prepared, copied, caught up or switched is not.
func TestMoveRowTerminal(t *testing.T) {
	for phase, want := range map[string]bool{"preparing": false, "copying": false, "catching_up": false, "switching": false,
		"cleaning": true, "done": true, "failed": true, "cancelled": true} {
		if got := (moveRow{phase: phase}).terminal(); got != want {
			t.Errorf("%s terminal = %v, want %v", phase, got, want)
		}
	}
	if !strings.Contains(phaseText(moveRow{phase: "copying", target: 3}), "shard group 3") || !strings.Contains(phaseText(moveRow{phase: "switching"}), "writers") {
		t.Error("the text of the phases does not say what happens")
	}
}

// A step that stopped for a cancel request is recorded with the code cancelled and is not retried.
func TestCancelledErrorIsCoded(t *testing.T) {
	var c task.Coded = &cancelledError{msg: "x"}
	if c.TaskCode() != task.CodeCanceled || c.TaskRetryable() {
		t.Errorf("code %s retryable %v", c.TaskCode(), c.TaskRetryable())
	}
}

// The definitions of the three kinds have the steps of the design, in order, and the step that must not run twice can say
// whether its effect is there.
func TestSchemaDefinitionsHaveTheNamedSteps(t *testing.T) {
	o := &Ops{}
	got := map[string][]task.Step{}
	for _, d := range o.schemaDefinitions() {
		got[d.Kind] = d.Steps
		if (d.Kind == KindSchemaMove || d.Kind == KindSchemaUndistribute) && d.Cancellable == nil {
			t.Errorf("%s has no cancel guard", d.Kind)
		}
	}
	names := func(k string) string {
		var n []string
		for _, s := range got[k] {
			n = append(n, s.Name)
		}
		return strings.Join(n, "|")
	}
	for k, want := range map[string]string{
		KindSchemaDistribute:   "Check the schema|Set REPLICA IDENTITY FULL|Make the schema distributed|Verify on every server",
		KindSchemaUndistribute: "Check the schema|Move first|Make the schema local|Verify on every server",
		KindSchemaMove:         "Check the move|Set REPLICA IDENTITY FULL|Copy the data|Switch|Clean up the old copy",
	} {
		if names(k) != want {
			t.Errorf("%s steps = %s, want %s", k, names(k), want)
		}
	}
	for _, k := range []string{KindSchemaDistribute, KindSchemaUndistribute} {
		var ni int
		for _, s := range got[k] {
			if s.NonIdempotent {
				ni++
				if s.Recover == nil {
					t.Errorf("%s: the step %s must not run twice and has no way to read its effect", k, s.Name)
				}
			}
		}
		if ni != 1 {
			t.Errorf("%s has %d non-idempotent steps, want 1", k, ni)
		}
	}
	for _, s := range got[KindSchemaMove] {
		if s.NonIdempotent {
			t.Errorf("the step %s of the move is idempotent (a repeated run follows the move that is there)", s.Name)
		}
	}
}

// Hostile names are refused where PostgreSQL could not take them and nowhere else.
func TestValidSchemaNames(t *testing.T) {
	for _, tc := range []struct {
		db, schema string
		ok         bool
	}{
		{"shop", "tenant_1", true},
		{"shop", "Te\"n ant;'\\ --é \U0001F4A5", true},
		{"shop", "pg_catalog", true}, // refused later, with the reason, by the extension's inventory
		{"shop", strings.Repeat("x", 63), true},
		{"shop", strings.Repeat("x", 64), false},
		{"shop", "", false},
		{"shop", "a\x00b", false},
		{"shop", "a\nb", false},
		{"", "s", false},
	} {
		err := validSchemaNames(tc.db, tc.schema)
		if (err == nil) != tc.ok {
			t.Errorf("%q %q: %v, want ok=%v", tc.db, tc.schema, err, tc.ok)
		}
	}
}
