// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
)

// B7 of the bootstrap of the map group: the rebalance that the old leader left running is paused by direct updates (the engine
// is not running), so that the failover task can be recorded (migration 0009 excludes it while a rebalance is active); a second
// call finds nothing to pause.
func TestTheRebalanceOfTheOldLeaderIsPausedWithoutTheEngine(t *testing.T) {
	e, _ := newGuardEnv(t) // a rebalance task, claimed (running)
	ctx := context.Background()
	if _, err := e.sys.Exec(ctx, `INSERT INTO kingres_cp.rebalance_run (task_id, options, started_at) VALUES ($1, '{}', clock_timestamp())`, e.id); err != nil {
		t.Fatal(err)
	}
	n, err := PauseForMapGroupFailover(ctx, e.sys)
	if err != nil || n != 1 {
		t.Fatalf("paused %d: %v", n, err)
	}
	var state, outcome, by string
	if err := e.sys.QueryRow(ctx, `SELECT t.state, coalesce(r.outcome, ''), coalesce(r.stopped_by, '') FROM kingres_cp.task t JOIN kingres_cp.rebalance_run r ON r.task_id = t.id WHERE t.id = $1`, e.id).
		Scan(&state, &outcome, &by); err != nil || state != "paused" || outcome != "stopped" || by != "safetyRule" {
		t.Fatalf("state %q outcome %q by %q (%v)", state, outcome, by, err)
	}
	if n, err := PauseForMapGroupFailover(ctx, e.sys); err != nil || n != 0 {
		t.Fatalf("second call paused %d: %v", n, err)
	}
}

// B7: the task of the failover is recorded once for one journal, with the steps of group 0 up to the catalog step done, the
// run and group rows, and the events of the journal.
func TestTheTaskOfTheMapGroupFailoverIsRecordedOnceWithItsFirstStepsDone(t *testing.T) {
	e, _ := newGuardEnv(t)
	ctx := context.Background()
	if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.task SET state = 'done', lease_holder = NULL, lease_expires_at = NULL`); err != nil {
		t.Fatal(err)
	}
	r := &failover.Runner{}
	rec := failover.MapRecord{JournalID: "j1", Input: failover.Input{Mode: failover.ModeHard, ServerID: "s1", Groups: []int32{0, 2}, Trigger: "manual", RequestedBy: "admin"},
		OldNode: 1, NewNode: 4, NewServer: "s2", ChosenStandby: 10, TermBefore: 5, TermAfter: 6, Timeline: 2,
		Events: []failover.MapEvent{{Step: "B5 promote", Kind: "promoted"}, {Step: "B6", Kind: "catalogPromoted"}}}
	id, err := r.RecordMapGroupRun(ctx, e.sys, rec)
	if err != nil || id == 0 {
		t.Fatalf("%d %v", id, err)
	}
	again, err := r.RecordMapGroupRun(ctx, e.sys, rec)
	if err != nil || again != id {
		t.Fatalf("a second record: %d %v (want %d)", again, err, id)
	}
	var steps string
	var state string
	if err := e.sys.QueryRow(ctx, `SELECT state, progress->'steps' #>> '{0,state}' FROM kingres_cp.task WHERE id = $1`, id).Scan(&state, &steps); err != nil || state != "pending" || steps != "done" {
		t.Fatalf("task state %q first step %q (%v)", state, steps, err)
	}
	names, _ := r.StepNames(rec.Input)
	var done int
	if err := e.sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.task t, jsonb_array_elements(t.progress->'steps') s WHERE t.id = $1 AND s->>'state' = 'done'`, id).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if want := 8; done != want { // the check, and group 0 up to the catalog step
		t.Fatalf("%d steps done of %d, want %d", done, len(names), want)
	}
	var groups, events int
	if err := e.sys.QueryRow(ctx, `SELECT (SELECT count(*) FROM kingres_cp.failover_group WHERE task_id = $1), (SELECT count(*) FROM kingres_cp.failover_event WHERE task_id = $1)`, id).Scan(&groups, &events); err != nil || groups != 2 || events != 2 {
		t.Fatalf("groups %d events %d (%v)", groups, events, err)
	}
}
