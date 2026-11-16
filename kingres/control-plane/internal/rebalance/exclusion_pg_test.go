// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func newEnvNoTask(t *testing.T) *guardEnv {
	t.Helper()
	e, _ := newGuardEnv(t)
	// newGuardEnv made and claimed a rebalance task; end it so the tests start clean.
	if _, err := e.sys.Exec(context.Background(), `UPDATE kingres_cp.task SET state = 'done', lease_holder = NULL, lease_expires_at = NULL`); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *guardEnv) create(t *testing.T, st task.Store, kind, subject string) (task.Task, error) {
	t.Helper()
	typ := "server"
	if kind == Kind {
		typ, subject = SubjectType, SubjectID
	}
	return st.Create(context.Background(), task.Task{Kind: kind, SubjectType: typ, SubjectID: subject, Input: []byte("{}"),
		Steps: []task.StepState{{Name: "x", State: task.StepPending}}, CreatedAt: time.Now(), UpdatedAt: time.Now()})
}

// waitForLockWait waits until another session waits for an advisory lock.
func waitForLockWait(t *testing.T, c *pgx.Conn) {
	t.Helper()
	for i := 0; i < 200; i++ {
		var n int
		if err := c.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the second start never waited for the lock")
}

// RB-10: a server.remove whose insert is in flight (not yet committed) and a rebalance start that arrives
// meanwhile are ordered by the database: the start waits, and is refused when the removal committed. Before
// migration 0009 both tasks were created.
func TestRebalanceStartAndServerRemoveCannotBothBeCreated(t *testing.T) {
	e := newEnvNoTask(t)
	ctx := context.Background()
	tx, err := e.sys.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, input, progress, created_at, updated_at, created_by)
		VALUES ('server.remove', 'pending', 'server', 's2', '{}', '{"steps":[]}', now(), now(), 'u')`); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	other := e.in.Connect(t, "kingres_system")
	go func() { _, err := e.create(t, task.NewPGStore(other), Kind, ""); res <- err }()
	waitForLockWait(t, e.sys)
	select {
	case err := <-res:
		t.Fatalf("the rebalance was created while a removal was being created: %v", err)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var x *task.ExcludedError
	if err := <-res; !errors.As(err, &x) || x.Other != "server.remove" {
		t.Fatalf("rebalance start: %v, want an ExcludedError naming server.remove", err)
	}
	if p := x.Problem(); p.Code != "operation_in_progress" {
		t.Errorf("problem %q", p.Code)
	}
}

// The other order: the rebalance is being created, a removal arrives, and is refused with rebalance_in_progress.
func TestServerRemoveRefusedWhileARebalanceIsBeingCreated(t *testing.T) {
	e := newEnvNoTask(t)
	ctx := context.Background()
	tx, err := e.sys.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, input, progress, created_at, updated_at, created_by)
		VALUES ('rebalance', 'pending', 'cluster', 'cluster', '{}', '{"steps":[]}', now(), now(), 'u')`); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	other := e.in.Connect(t, "kingres_system")
	go func() { _, err := e.create(t, task.NewPGStore(other), "server.remove", "s2"); res <- err }()
	waitForLockWait(t, e.sys)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var x *task.ExcludedError
	if err := <-res; !errors.As(err, &x) || x.Problem().Code != "rebalance_in_progress" {
		t.Fatalf("server.remove: %v", err)
	}
}

// A stopped (paused) rebalance does not exclude a removal (contract 0.9.0), but resuming it while the removal is
// unfinished is refused, and the run is not touched.
func TestResumeOfAStoppedRebalanceIsRefusedWhileAServerOperationRuns(t *testing.T) {
	e := newEnvNoTask(t)
	ctx := context.Background()
	st := task.NewPGStore(e.sys)
	rb, err := e.create(t, st, Kind, "")
	if err != nil {
		t.Fatal(err)
	}
	rows := &PGRows{Conns: e, TaskID: rb.ID}
	if _, err := rows.CreateRun(ctx, Options{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.task SET state = 'paused' WHERE id = $1`, rb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET outcome = 'stopped', stopped_by = 'operator' WHERE task_id = $1`, rb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.create(t, st, "server.remove", "s2"); err != nil {
		t.Fatalf("a removal beside a stopped rebalance: %v", err)
	}
	var x *task.ExcludedError
	if err := ResumeRun(ctx, e.sys, rb.ID, Options{}); !errors.As(err, &x) {
		t.Fatalf("resume: %v, want ExcludedError", err)
	}
	var state, outcome string
	var round int
	if err := e.sys.QueryRow(ctx, `SELECT t.state, r.outcome, r.round FROM kingres_cp.task t JOIN kingres_cp.rebalance_run r ON r.task_id = t.id WHERE t.id = $1`, rb.ID).Scan(&state, &outcome, &round); err != nil {
		t.Fatal(err)
	}
	if state != "paused" || outcome != "stopped" || round != 0 {
		t.Errorf("state %s outcome %s round %d: the refused resume changed something", state, outcome, round)
	}
}

// RB-07/RB-11: starting a stopped run again begins a new round and replaces the options, also with none.
func TestResumeRunBeginsARoundAndTakesTheNewOptions(t *testing.T) {
	e := newEnvNoTask(t)
	ctx := context.Background()
	st := task.NewPGStore(e.sys)
	rb, _ := e.create(t, st, Kind, "")
	rows := &PGRows{Conns: e, TaskID: rb.ID}
	if _, err := rows.CreateRun(ctx, Options{Databases: []string{"shop"}}, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = e.sys.Exec(ctx, `UPDATE kingres_cp.task SET state = 'paused' WHERE id = $1`, rb.ID)
	_, _ = e.sys.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET outcome = 'stopped', stop_requested = true WHERE task_id = $1`, rb.ID)
	if err := ResumeRun(ctx, e.sys, rb.ID, Options{MaxConcurrentMoves: 1}); err != nil {
		t.Fatal(err)
	}
	run, err := rows.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run.Round != 1 || len(run.Options.Databases) != 0 || run.Outcome != "" || run.StopRequested {
		t.Errorf("run after start again: %+v", run)
	}
	if err := ResumeRun(ctx, e.sys, rb.ID, Options{}); !errors.Is(err, task.ErrNotPaused) {
		t.Errorf("a second start: %v", err)
	}
}

// RB-08 on a real system database: a cancel of a paused (stopped) rebalance through the engine ends the run as
// cancelled, by the operator, its moves in flight as cancelled, and raises no taskFailed alert. A failure still does.
func TestCancelOfAStoppedRebalanceIsNotAFailure(t *testing.T) {
	e := newEnvNoTask(t)
	ctx := context.Background()
	st := task.NewPGStore(e.sys)
	svc := &Service{Conns: e}
	eng, err := task.NewEngine(st, svc.Definitions(), task.Options{Holder: "A"})
	if err != nil {
		t.Fatal(err)
	}
	rb, err := e.create(t, st, Kind, "")
	if err != nil {
		t.Fatal(err)
	}
	rows := &PGRows{Conns: e, TaskID: rb.ID}
	if _, err := rows.CreateRun(ctx, Options{}, nil); err != nil {
		t.Fatal(err)
	}
	// a move that was in flight when the run was stopped
	_, _ = e.sys.Exec(ctx, `INSERT INTO kingres_cp.rebalance_move (task_id, seq, database_name, colocation_id, bucket, from_group, to_group, state, started_at)
		VALUES ($1, 1, 'shop', 1, 3, 1, 2, 'running', now())`, rb.ID)
	_, _ = e.sys.Exec(ctx, `UPDATE kingres_cp.task SET state = 'paused' WHERE id = $1`, rb.ID)
	_, _ = e.sys.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET outcome = 'stopped', stopped_by = 'operator' WHERE task_id = $1`, rb.ID)
	got, _ := eng.Get(ctx, rb.ID)
	if !task.Cancellable(got) {
		t.Fatal("a paused rebalance must say it can be cancelled")
	}
	if err := eng.Cancel(ctx, rb.ID); err != nil {
		t.Fatal(err)
	}
	run, err := rows.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != OutcomeCancelled || run.StoppedBy != "operator" || run.FinishedAt == nil || run.Error != "" {
		t.Errorf("run %+v", run)
	}
	mv, _ := rows.Moves(ctx)
	if len(mv) != 1 || mv[0].State != MoveCancelled {
		t.Errorf("moves %+v", mv)
	}
	var alerts int
	_ = e.sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.alert WHERE kind = 'taskFailed'`).Scan(&alerts)
	if alerts != 0 {
		t.Errorf("%d taskFailed alerts for a cancel", alerts)
	}
	// a failure is still a failure
	svc.onFail(ctx, task.Task{ID: rb.ID, Error: &task.Error{Code: task.CodeStepFailed, Message: "boom"}})
	_ = e.sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.alert WHERE kind = 'taskFailed'`).Scan(&alerts)
	if alerts != 1 {
		t.Errorf("%d taskFailed alerts after a failure, want 1", alerts)
	}
}
