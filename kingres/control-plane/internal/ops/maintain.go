// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The task partition.maintain (phase 12, analytics-phase12-storage.md section 10.6): one run of the time-partitioning policy of a table. The scheduler
// (internal/partsched) starts it for a policy that is due, runPartitionPolicy starts it by hand. The steps call the procedures of kingres_timepart in the
// database of the table; a run is a record of the extension (kingres.timepart_run), and kingres_cp.partition_run_task says which task made it.
//
// Every step is idempotent. begin looks for the run of this task before it makes one; a phase of a run records each action before and after it, so
// a task that is taken over after a crash goes on with the action it was in; finish of a run that is finished does nothing.

// KindMaintain is the task kind.
const KindMaintain = "partition.maintain"

// The names of the steps (shown to users).
const (
	stepMaintainBegin   = "Begin the run"
	stepMaintainCreate  = "Create partitions"
	stepMaintainConvert = "Convert partitions"
	stepMaintainRetire  = "Retire partitions"
	stepMaintainFinish  = "Finish the run"
)

// maintainWait is how long begin waits before it asks again while another run of the table is going; maintainWaitLimit bounds the waiting. Variables for tests.
var (
	maintainWait      = 5 * time.Second
	maintainWaitLimit = 30 * time.Minute
)

// maintainParams is the input of the task.
type maintainParams struct {
	Database string `json:"database"`
	Schema   string `json:"schema"`
	Table    string `json:"table"`
	// AsOf is the time the run is for, fixed when the task is made (RFC 3339), so that a task taken over after a crash plans the same actions.
	AsOf string `json:"asOf"`
	// Trigger is schedule or manual.
	Trigger string `json:"trigger"`
	// Actor is the role that asked, or system for the scheduler.
	Actor string `json:"actor"`
}

func (p maintainParams) subject() string { return subjectOf(p.Database, p.Schema, p.Table) }

func (o *Ops) maintainDefinition() task.Definition {
	phase := func(name, ph string) task.Step {
		return task.Step{Name: name, Interruptible: true, Run: func(ctx context.Context, env *task.Env) error { return o.stepMaintainPhase(ctx, env, ph) }}
	}
	return task.Definition{Kind: KindMaintain, Steps: []task.Step{
		{Name: stepMaintainBegin, Interruptible: true, Run: o.stepMaintainBegin},
		phase(stepMaintainCreate, "create"),
		phase(stepMaintainConvert, "convert"),
		phase(stepMaintainRetire, "retire"),
		{Name: stepMaintainFinish, Run: o.stepMaintainFinish},
	}}
}

// PrepareMaintain validates a run of the policy of a table and returns the task input. asOf nil is now. trigger is schedule or manual.
func (o *Ops) PrepareMaintain(ctx context.Context, actor, db, schema, table string, asOf *time.Time, trigger string) (any, error) {
	if err := checkNames(db, schema, table); err != nil {
		return nil, err
	}
	at := time.Now().UTC()
	if asOf != nil {
		at = asOf.UTC()
	}
	p := maintainParams{Database: db, Schema: schema, Table: table, AsOf: at.Format(time.RFC3339Nano), Trigger: trigger, Actor: actor}
	if err := o.ensureExtensions(ctx, db, []string{ExtTimepart}, false); err != nil {
		return nil, err
	}
	var columnarAfter bool
	err := o.policyTx(ctx, db, func(tx pgx.Tx) error {
		if err := requirePartitioned(ctx, tx, schema, table); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT columnar_after IS NOT NULL FROM kingres.timepart_policy WHERE schema_name = $1 AND table_name = $2`, schema, table).Scan(&columnarAfter)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s has no partitioning policy.", schema, table))
		}
		return err
	})
	if err != nil {
		return nil, wrap(err)
	}
	if columnarAfter {
		if err := o.ensureExtensions(ctx, db, []string{ExtColumnar}, true); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// StartMaintain starts the task for a table whose policy is due (the scheduler). It reports false, without an error, when a task of the table is
// unfinished: the scheduler asks again at the next tick.
func (o *Ops) StartMaintain(ctx context.Context, db, schema, table string, asOf time.Time) (bool, error) {
	if o.maintainRunning(ctx, db, schema, table) {
		return false, nil
	}
	in, err := o.PrepareMaintain(ctx, "system", db, schema, table, &asOf, "schedule")
	if err != nil {
		return false, err
	}
	if _, err := o.StartTask(ctx, KindMaintain, "table", subjectOf(db, schema, table), in, "system"); err != nil {
		var pr *problem.Problem
		if errors.As(err, &pr) && pr.Code == problem.OperationInProgress {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// maintainConn opens a service connection to the database of the table, trusting the owners of the tables as the service role trusts itself
// (the extension refuses to run code of a table owner in a superuser session otherwise).
func (o *Ops) maintainConn(ctx context.Context, db string) (*pgx.Conn, error) {
	if o.Service == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server has no managed instance.")
	}
	c, err := o.Service.ServiceConn(ctx, db)
	if err != nil {
		return nil, connectProblem(err)
	}
	if _, err := c.Exec(ctx, `SELECT pg_catalog.set_config('kingres_timepart.trust_table_owners', 'on', false), pg_catalog.set_config('intervalstyle', 'iso_8601', false)`); err != nil {
		_ = c.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return c, nil
}

// runOfTask returns the run that this task made, 0 when it made none yet.
func (o *Ops) runOfTask(ctx context.Context, db string, taskID int64) (int64, error) {
	var run int64
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT run_id FROM kingres_cp.partition_run_task WHERE database_name = $1 AND task_id = $2`, db, taskID).Scan(&run)
		if errors.Is(err, pgx.ErrNoRows) {
			run = 0
			return nil
		}
		return err
	})
	return run, err
}

func (o *Ops) runState(ctx context.Context, db string, run int64) (string, error) {
	c, err := o.maintainConn(ctx, db)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var st string
	err = c.QueryRow(ctx, `SELECT state FROM kingres.timepart_run_data WHERE run_id = $1`, run).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return st, err
}

// stepMaintainBegin makes the run, or finds the one this task made. While another run of the table is going it waits and says so.
func (o *Ops) stepMaintainBegin(ctx context.Context, env *task.Env) error {
	p, err := inputOf[maintainParams](env.Task)
	if err != nil {
		return err
	}
	if run, err := o.runOfTask(ctx, p.Database, env.Task.ID); err != nil {
		return err
	} else if run != 0 {
		return nil // the run of an earlier pass of this step; the next steps read its state
	}
	asOf, err := time.Parse(time.RFC3339Nano, p.AsOf)
	if err != nil {
		return fmt.Errorf("reading the time of the run: %w", err)
	}
	deadline := time.Now().Add(maintainWaitLimit)
	for {
		run, err := o.beginRun(ctx, p, asOf)
		if err == nil {
			_ = env.SetDetail(ctx, "")
			return o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO kingres_cp.partition_run_task (database_name, run_id, task_id, started_by) VALUES ($1, $2, $3, $4)
					ON CONFLICT (database_name, run_id) DO NOTHING`, p.Database, run, env.Task.ID, p.Actor)
				return err
			})
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "55P03" {
			return maintainError(err)
		}
		_ = env.SetDetail(ctx, "Waiting: "+pe.Message)
		if time.Now().After(deadline) {
			return maintainError(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(maintainWait):
		}
	}
}

func (o *Ops) beginRun(ctx context.Context, p maintainParams, asOf time.Time) (int64, error) {
	c, err := o.maintainConn(ctx, p.Database)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	trigger := p.Trigger
	if trigger != "schedule" {
		trigger = "manual"
	}
	var run int64
	err = c.QueryRow(ctx, `SELECT kingres.timepart_begin_run($1::regclass, $2, $3)`, qualified(p.Schema, p.Table), asOf, trigger).Scan(&run)
	return run, err
}

// maintainError gives a refusal of the extension a code: a table that lost its policy or its partitioning is partition_policy_conflict.
func maintainError(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "42704":
			return &extError{code: problem.PartitionPolicyConflict, msg: pe.Message}
		case "42501":
			return &extError{code: problem.PartitionPolicyConflict, msg: pe.Message}
		}
	}
	return err
}

// stepMaintainPhase runs one phase of the run: create, convert or retire. A phase that finds nothing to do succeeds. An action that failed (a lock
// timeout) does not fail the step: the run ends partial and the next run tries again.
func (o *Ops) stepMaintainPhase(ctx context.Context, env *task.Env, phase string) error {
	p, run, state, err := o.currentRun(ctx, env)
	if err != nil {
		return err
	}
	if state != "running" {
		return env.SetDetail(ctx, fmt.Sprintf("Run %d is %s; nothing to do.", run, state))
	}
	c, err := o.maintainConn(ctx, p.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if _, err := c.Exec(ctx, `CALL kingres.timepart_run_phase($1, $2)`, run, phase); err != nil {
		if ctx.Err() == nil {
			o.failRun(ctx, p.Database, run, err)
		}
		return maintainError(err)
	}
	return nil
}

func (o *Ops) currentRun(ctx context.Context, env *task.Env) (maintainParams, int64, string, error) {
	p, err := inputOf[maintainParams](env.Task)
	if err != nil {
		return p, 0, "", err
	}
	run, err := o.runOfTask(ctx, p.Database, env.Task.ID)
	if err != nil {
		return p, 0, "", err
	}
	if run == 0 {
		return p, 0, "", errors.New("the run of this task was not made")
	}
	st, err := o.runState(ctx, p.Database, run)
	return p, run, st, err
}

// failRun marks a run failed after a step raised an error (the run would otherwise stay running until the next run found its session gone).
func (o *Ops) failRun(ctx context.Context, db string, run int64, cause error) {
	c, err := o.maintainConn(context.WithoutCancel(ctx), db)
	if err != nil {
		return
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	msg := cause.Error()
	var pe *pgconn.PgError
	if errors.As(cause, &pe) {
		msg = pe.Code + ": " + pe.Message
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, _ = c.Exec(context.WithoutCancel(ctx), `UPDATE kingres.timepart_run_data SET state = 'failed', finished_at = now(), error = $2 WHERE run_id = $1 AND state = 'running'`, run, msg)
}

// stepMaintainFinish closes the run: its state, the next run time, the pruning of old runs.
func (o *Ops) stepMaintainFinish(ctx context.Context, env *task.Env) error {
	p, run, state, err := o.currentRun(ctx, env)
	if err != nil {
		return err
	}
	if state != "running" {
		o.countRun(state)
		return env.SetDetail(ctx, fmt.Sprintf("Run %d is %s.", run, state))
	}
	c, err := o.maintainConn(ctx, p.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if _, err := c.Exec(ctx, `SELECT kingres.timepart_finish_run($1)`, run); err != nil {
		return maintainError(err)
	}
	final, err := o.runState(ctx, p.Database, run)
	if err != nil {
		return err
	}
	o.countRun(final)
	if n := o.failedActions(ctx, p.Database, run); n > 0 {
		o.metrics.actionsFailed.Add(n)
	}
	return nil
}

func (o *Ops) failedActions(ctx context.Context, db string, run int64) int64 {
	c, err := o.maintainConn(ctx, db)
	if err != nil {
		return 0
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var n int64
	_ = c.QueryRow(ctx, `SELECT count(*) FROM kingres.timepart_run_action_data WHERE run_id = $1 AND state = 'failed'`, run).Scan(&n)
	return n
}

func (o *Ops) countRun(state string) {
	o.metrics.runs.Add(1)
	if state == "failed" || state == "partial" {
		o.metrics.runsBad.Add(1)
	}
}

// partitionMetrics are the counters of the runs this server started as the leader.
type partitionMetrics struct {
	runs, runsBad, actionsFailed atomic.Int64
}

// PartitionMetrics returns the counters for the Prometheus endpoint: runs finished, runs that ended failed or partial, actions that failed.
func (o *Ops) PartitionMetrics() (runs, bad, actionsFailed int64) {
	return o.metrics.runs.Load(), o.metrics.runsBad.Load(), o.metrics.actionsFailed.Load()
}

var _ = strings.TrimSpace
var _ = json.Marshal
