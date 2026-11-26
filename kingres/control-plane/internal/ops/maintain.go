// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/partsched"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The task partition.maintain (phase 12, analytics-phase12-storage.md section 10.6): one run of the time-partitioning policy of a table. The scheduler
// (internal/partsched) starts it for a policy that is due, runPartitionPolicy starts it by hand. The steps call the procedures of kingres_timepart in the
// database of the table; a run is a record of the extension (kingres.timepart_run), and kingres_cp.partition_run_task says which task made it.
//
// Every step is idempotent. begin looks for the run of this task before it makes one; a phase of a run records each action before and after it, so
// a task that is taken over after a crash goes on with the action it was in; finish of a run that is finished does it again with the same result.
//
// A run is always closed with kingres.timepart_finish_run, whatever happened: that call moves the policy's next_run_at (so that a policy that fails is not
// started again at every tick) and prunes the old runs. A run that failed before the finish step is closed by the failure itself (failRun) and, for a
// failure that no step saw (the task is cancelled, the process lost its lease for good), by the hook OnFail of the kind.

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

// Timings that tests shorten.
var (
	// maintainWait is how long begin waits before it asks again while another run of the table is going; maintainWaitLimit bounds the waiting.
	maintainWait      = 5 * time.Second
	maintainWaitLimit = 30 * time.Minute
	// maintainSkew is how far the time of a run may be from the time of the instance (the clock that decides what is old). A run for another time would
	// retire or create the partitions of that time and park the schedule there.
	maintainSkew = 5 * time.Minute
	// maintainBackoff is how long a table whose last run failed is left alone by the scheduler, in addition to the next_run_at that the end of a run sets.
	maintainBackoff = 5 * time.Minute
	// maintainHorizon, with the policy's create_ahead, is how long after the last run of a policy a scheduled run may be for: a stop of kingresd that is longer, or a clock that
	// stepped forward by more, is refused with a ClockError.
	maintainHorizon = 7 * 24 * time.Hour
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
	}, OnFail: o.maintainOnFail}
}

// PrepareMaintain validates a run of the policy of a table and returns the task input. asOf nil is the time of the instance. The time of a run must be within
// maintainSkew of the time of the instance, so that a request, or a clock of the leader that jumped, cannot make a run act on another time (a dry
// replay at another time is planPartitionPolicy). trigger is schedule or manual; only a manual run is refused when the policy converts and the library of
// kingres_columnar is not preloaded: a scheduled run goes on creating and retiring, and the extension records the conversion it cannot do.
func (o *Ops) PrepareMaintain(ctx context.Context, actor, db, schema, table string, asOf *time.Time, trigger string) (any, error) {
	if err := checkNames(db, schema, table); err != nil {
		return nil, err
	}
	if err := o.ensureExtensions(ctx, db, []string{ExtTimepart}, false); err != nil {
		return nil, err
	}
	var columnarAfter bool
	var now, last time.Time
	var runEverySecs, aheadSecs float64
	var owner ownerInfo
	allowed, err := o.nonSuperuserOwnersAllowed(ctx)
	if err != nil {
		return nil, err
	}
	err = o.policyTx(ctx, db, func(tx pgx.Tx) error {
		if err := requirePartitioned(ctx, tx, schema, table); err != nil {
			return err
		}
		// The last run of the policy (or the policy itself, for one that never ran) is what a time is compared with: a clock of the host that stepped forward moves the
		// instance and the leader alike, and only the policy's own history shows it.
		err := tx.QueryRow(ctx, `SELECT p.columnar_after IS NOT NULL, now(),
			coalesce((SELECT max(r.as_of) FROM kingres.timepart_run r WHERE r.schema_name = p.schema_name AND r.table_name = p.table_name), p.updated_at),
			extract(epoch FROM p.run_every)::float8, extract(epoch FROM p.create_ahead)::float8
			FROM kingres.timepart_policy p WHERE p.schema_name = $1 AND p.table_name = $2`, schema, table).Scan(&columnarAfter, &now, &last, &runEverySecs, &aheadSecs)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s has no partitioning policy.", schema, table))
		}
		if err != nil {
			return err
		}
		owner, err = tableOwnerOf(ctx, tx, schema, table)
		return err
	})
	if err != nil {
		return nil, wrap(err)
	}
	if who := untrustedOwner(allowed, owner); who != "" {
		if trigger == "schedule" {
			return nil, &partsched.BlockedError{Msg: ownerNotTrustedText(who)}
		}
		return nil, ownerRefusal(who)
	}
	now, last = now.UTC(), last.UTC()
	at := now
	if asOf != nil {
		at = asOf.UTC()
		if d := at.Sub(now); d > maintainSkew || d < -maintainSkew {
			if trigger == "schedule" {
				return nil, &partsched.ClockError{Msg: fmt.Sprintf("the clock of the leader (%s) differs from the clock of the instance (%s) by more than %s", at.Format(time.RFC3339), now.Format(time.RFC3339), maintainSkew)}
			}
			return nil, problem.Validation("The time of the run is not the time of the server.", field("/asOf", fmt.Sprintf("must be within %s of the time of the server (%s)", maintainSkew, now.Format(time.RFC3339)), "out_of_range"))
		}
	}
	// A scheduled run is for the time of the clock, and the clock of the host can step (an NTP step, a resumed virtual machine, a date set by hand): the instance and the
	// leader then agree on a time that retention would act on. A time far after the last run of the policy is such a step, or a stop of kingresd longer than the policy can
	// bridge; either way a person looks: one alert, and a run by hand (which is bounded by the instance's time only) goes on when the time is right.
	if trigger == "schedule" && !last.IsZero() {
		horizon := maintainHorizon + time.Duration(aheadSecs*float64(time.Second))
		if gap := at.Sub(last); gap > horizon {
			return nil, &partsched.ClockError{Msg: fmt.Sprintf("the time of this run (%s) is %s after the last run of the policy (%s), more than the %s that a stop of kingresd is allowed to be: the clock of the host may have jumped. "+
				"Run the policy by hand when the time is right", at.Format(time.RFC3339), gap.Round(time.Minute), last.Format(time.RFC3339), horizon.Round(time.Hour))}
		}
	}
	_ = runEverySecs
	if columnarAfter && trigger != "schedule" {
		if err := o.ensureExtensions(ctx, db, []string{ExtColumnar}, true); err != nil {
			return nil, err
		}
	}
	return maintainParams{Database: db, Schema: schema, Table: table, AsOf: at.Format(time.RFC3339Nano), Trigger: trigger, Actor: actor}, nil
}

// StartMaintain starts the task for a table whose policy is due (the scheduler). It reports false, without an error, when a task of the table is
// unfinished or when the last run of the table failed less than maintainBackoff ago: the scheduler asks again at the next tick.
func (o *Ops) StartMaintain(ctx context.Context, db, schema, table string, asOf time.Time) (bool, error) {
	if running, err := o.maintainRunning(ctx, db, schema, table); err != nil || running {
		return false, err
	}
	if o.taskFailedRecently(subjectOf(db, schema, table)) || o.recentlyFailed(ctx, db, schema, table) {
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

// recentlyFailed tells whether the last run of the table ended failed within maintainBackoff (by the clock of the instance).
func (o *Ops) recentlyFailed(ctx context.Context, db, schema, table string) bool {
	var yes bool
	err := o.policyTx(ctx, db, func(tx pgx.Tx) error {
		// the last run only: a run that succeeded after a failure lifts the back-off
		err := tx.QueryRow(ctx, `SELECT r.state = 'failed' AND r.finished_at > now() - make_interval(secs => $3::float8) FROM kingres.timepart_run r
			WHERE r.schema_name = $1 AND r.table_name = $2 ORDER BY r.run_id DESC LIMIT 1`, schema, table, maintainBackoff.Seconds()).Scan(&yes)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return err == nil && yes
}

// failedTasks remembers, by subject, when a task partition.maintain last failed: a failure before any run exists (begin_run raises) leaves no failed run to
// back off from, and without this the scheduler would start a task at every tick. The state is in this process: the policy itself carries the same failure in
// its next_run_at (maintainOnFail), which survives a change of the leader.
type failedTasks struct {
	sync.Mutex
	at map[string]time.Time
}

func (o *Ops) noteTaskFailure(subject string) {
	o.failed.Lock()
	defer o.failed.Unlock()
	if o.failed.at == nil {
		o.failed.at = map[string]time.Time{}
	}
	o.failed.at[subject] = time.Now()
}

func (o *Ops) noteTaskSuccess(subject string) {
	o.failed.Lock()
	defer o.failed.Unlock()
	delete(o.failed.at, subject)
}

func (o *Ops) taskFailedRecently(subject string) bool {
	o.failed.Lock()
	defer o.failed.Unlock()
	at, ok := o.failed.at[subject]
	return ok && time.Since(at) < maintainBackoff
}

// moveSchedule moves the next run of the policy by the back-off: the failure of a task that had no run to close is recorded on the policy itself.
func (o *Ops) moveSchedule(ctx context.Context, db, schema, table string) {
	_ = o.policyTx(ctx, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kingres.timepart_policy SET next_run_at = now() + make_interval(secs => $3::float8) WHERE schema_name = $1 AND table_name = $2`, schema, table, maintainBackoff.Seconds())
		return err
	})
}

// maintainConn opens a service connection to the database of the table that trusts the owners of the tables; see trustedConn.
func (o *Ops) maintainConn(ctx context.Context, db string) (*pgx.Conn, error) {
	return o.trustedConn(ctx, db, extguard.Timepart)
}

// trustedConn opens a service connection with kingres_timepart.trust_table_owners on and intervals in ISO 8601. The setting is the rule for every
// operation that drives the DDL of kingres_timepart (a run of a policy and the conversion of a partition): the owner of the table is a role of this
// cluster, and the code it can cause to run in the session (an index expression, a default, a trigger on a partition) runs as the service role for the time
// of the call. The connection is closed when the call ends, which ends whatever the code left in the session. The README and the contract say so.
func (o *Ops) trustedConn(ctx context.Context, db string, need ...string) (*pgx.Conn, error) {
	if o.Service == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server has no managed instance.")
	}
	c, err := o.Service.ServiceConn(ctx, db)
	if err != nil {
		return nil, connectProblem(err)
	}
	// the session goes on to call the schema kingres as a superuser: the extensions it needs must be there and own it
	for _, e := range need {
		if err := requireExt(ctx, c, e); err != nil {
			_ = c.Close(context.WithoutCancel(ctx))
			return nil, err
		}
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

// runInfo is the state of a run and its error text as the instance has them. A run that the instance does not have is an error: the run was made by
// another server's instance and this server cannot go on with it.
func (o *Ops) runInfo(ctx context.Context, db string, run int64) (state, errText string, err error) {
	c, err := o.maintainConn(ctx, db)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var e *string
	err = c.QueryRow(ctx, `SELECT state, error FROM kingres.timepart_run_data WHERE run_id = $1`, run).Scan(&state, &e)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("run %d, which this task made, is not in the instance of this server (the task was taken over by a server whose instance does not hold it): the run cannot go on", run)
	}
	if e != nil {
		errText = *e
	}
	return state, errText, err
}

// finishRun closes a run: its final state, the next run time of the policy and the pruning of old runs. It is idempotent.
func (o *Ops) finishRun(ctx context.Context, db string, run int64) error {
	c, err := o.maintainConn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	_, err = c.Exec(ctx, `SELECT kingres.timepart_finish_run($1)`, run)
	return err
}

// stepMaintainBegin makes the run, or finds the one this task made. While another run of the table is going it waits and says so. A run that the extension
// records as failed at once (the owner changed, the policy was never verified) is closed here and fails the task with the reason.
func (o *Ops) stepMaintainBegin(ctx context.Context, env *task.Env) error {
	p, err := inputOf[maintainParams](env.Task)
	if err != nil {
		return err
	}
	if run, err := o.runOfTask(ctx, p.Database, env.Task.ID); err != nil {
		return err
	} else if run != 0 {
		// The run of an earlier pass of this step. If that pass died after the extension recorded the run as failed and before it was closed, the reason is not lost.
		state, errText, err := o.runInfo(ctx, p.Database, run)
		if err != nil {
			return err
		}
		if state == "failed" {
			if err := o.finishRun(ctx, p.Database, run); err != nil {
				return err
			}
			return &extError{code: problem.PartitionPolicyConflict, msg: "The run could not begin: " + errText}
		}
		return nil
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
			if err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO kingres_cp.partition_run_task (database_name, run_id, task_id, started_by) VALUES ($1, $2, $3, $4)
					ON CONFLICT (database_name, run_id) DO NOTHING`, p.Database, run, env.Task.ID, p.Actor)
				return err
			}); err != nil {
				return err
			}
			state, errText, err := o.runInfo(ctx, p.Database, run)
			if err != nil {
				return err
			}
			if state == "failed" {
				if err := o.finishRun(ctx, p.Database, run); err != nil {
					return err
				}
				o.countRun("failed")
				return &extError{code: problem.PartitionPolicyConflict, msg: "The run could not begin: " + errText}
			}
			return nil
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
		case "42704", "42501":
			return &extError{code: problem.PartitionPolicyConflict, msg: pe.Message}
		}
	}
	return err
}

// stepMaintainPhase runs one phase of the run: create, convert or retire. A phase that finds nothing to do succeeds. An action that failed (a lock
// timeout) does not fail the step: the run ends partial and the next run tries again. An error of the phase closes the run as failed.
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
	st, _, err := o.runInfo(ctx, p.Database, run)
	return p, run, st, err
}

// failRun marks a run failed after a step raised an error and closes it, so that the next run time moves (the policy is not started again at every tick) and
// the old runs are pruned. Best effort: a run that cannot be marked now is marked by the next begin as interrupted.
func (o *Ops) failRun(ctx context.Context, db string, run int64, cause error) {
	ctx = context.WithoutCancel(ctx)
	c, err := o.maintainConn(ctx, db)
	if err != nil {
		return
	}
	msg := cause.Error()
	var pe *pgconn.PgError
	if errors.As(cause, &pe) {
		msg = pe.Code + ": " + pe.Message
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, _ = c.Exec(ctx, `UPDATE kingres.timepart_run_data SET state = 'failed', finished_at = now(), error = $2 WHERE run_id = $1 AND state = 'running'`, run, msg)
	_ = c.Close(ctx)
	if err := o.finishRun(ctx, db, run); err != nil && o.Log != nil {
		o.Log.Warn("closing a failed run of a partition policy failed", "database", db, "run", run, "error", err)
	}
	o.countRun("failed")
}

// maintainOnFail runs after the task was recorded as failed or cancelled: a run that is still going (a step failed in a way that no step saw, the task was
// cancelled) is marked failed and closed, so that the schedule moves on.
func (o *Ops) maintainOnFail(ctx context.Context, t task.Task) {
	p, err := inputOf[maintainParams](t)
	if err != nil {
		return
	}
	o.noteTaskFailure(p.subject())
	run, err := o.runOfTask(ctx, p.Database, t.ID)
	if err != nil {
		return
	}
	if run == 0 { // the task failed before it had a run (begin_run raised): the failure is on the policy
		o.moveSchedule(ctx, p.Database, p.Schema, p.Table)
		return
	}
	state, _, err := o.runInfo(ctx, p.Database, run)
	if err != nil || state != "running" {
		return
	}
	cause := errors.New("the task ended before the run did")
	if t.Error != nil {
		cause = fmt.Errorf("%s: %s", t.Error.Code, t.Error.Message)
	}
	o.failRun(ctx, p.Database, run, cause)
}

// stepMaintainFinish closes the run: its state, the next run time, the pruning of old runs. It also closes a run that failed or was closed before, which
// moves the schedule once more with the same result.
func (o *Ops) stepMaintainFinish(ctx context.Context, env *task.Env) error {
	p, run, state, err := o.currentRun(ctx, env)
	if err != nil {
		return err
	}
	if err := o.finishRun(ctx, p.Database, run); err != nil {
		return maintainError(err)
	}
	final, _, err := o.runInfo(ctx, p.Database, run)
	if err != nil {
		return err
	}
	o.noteTaskSuccess(p.subject())
	if state == "running" { // the first close of this run
		o.countRun(final)
		if n := o.failedActions(ctx, p.Database, run); n > 0 {
			o.metrics.actionsFailed.Add(n)
		}
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
