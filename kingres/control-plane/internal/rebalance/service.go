// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// defaultMaxConcurrentMoves is what a start without maxConcurrentMoves records: this version runs one move at
// a time (contract RebalanceOptions.maxConcurrentMoves, RB-12).
const defaultMaxConcurrentMoves = 1

// Step names (shown to users).
const (
	StepCheck = "Check that the cluster can rebalance"
	StepPlan  = "Compute the plan"
	StepMove  = "Move buckets"
	StepWrite = "Write the report"
)

// Service is the rebalance as the HTTP layer and the task engine use it.
type Service struct {
	Conns   Conns
	Backend *PG
	// Engine returns the task engine, nil before it exists.
	Engine func() *task.Engine
	// Background runs f in the background, waited for at shutdown.
	Background func(f func())
	// BGFn returns the context of background runs (it exists only once the engine does).
	BGFn  func() context.Context
	Audit func(ctx context.Context, ev audit.Event)
	// Summary tells whether the cluster needs a rebalance and why (the read model).
	Summary func(ctx context.Context) (needed bool, reasons []string, err error)
	Log     *slog.Logger
	// Loop settings; zero values are the defaults of Loop.
	MaxAttempts int
	Backoff     time.Duration
	Poll        time.Duration
	// ServerStatus tells the status of a server ("dead" refuses a drain); nil skips the check.
	ServerStatus func(ctx context.Context, id string) string
	// SetShouldHaveShards writes the flag of a server to the catalog of every database (the drain's second step).
	SetShouldHaveShards func(ctx context.Context, server string, want bool) error
	// RemoveServer starts the task server.remove (a drain with removeWhenDone, after the drain task ended).
	RemoveServer func(ctx context.Context, actor, id string) (task.Task, error)
	// RelocateStandbys takes the server out of the ring of the standbys, asks for a reconcile and returns how many standbys the server
	// still hosts. nil: the step is skipped.
	RelocateStandbys func(ctx context.Context, server string) (int, error)
	// HandOverMapGroup asks for the planned switchover of the map group (shard group 0) of the server to a standby on another server and
	// returns once the failover task exists (or the problem that refuses it). nil: a server that hosts the map group primary is refused.
	HandOverMapGroup func(ctx context.Context, actor, server string) error
	// HostsMapGroup says whether the server runs the primary of group 0; nil reads the catalog.
	HostsMapGroup func(ctx context.Context, server string) (bool, error)
	// Handover keeps the marker of a hand-over in progress; nil keeps it in the system database.
	Handover HandoverStore
	// StandbyLag is the largest lag in bytes of the standbys of a group behind its primary; nil asks the primary of the group.
	StandbyLag func(ctx context.Context, group int32) (int64, error)
	// DrainWait bounds the wait for the server to own no bucket (default 30 minutes).
	DrainWait time.Duration
	// DrainPoll is the pause between two looks at a drain (default 3 s).
	DrainPoll time.Duration
	// IsLeader tells whether this server is the manager leader; the automatic job does nothing elsewhere. nil: always.
	IsLeader func(ctx context.Context) bool
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// input is the input of the task: the options only (no secrets).
type input struct {
	Options Options `json:"options"`
}

// Definitions returns the task definition of the rebalance.
func (s *Service) Definitions() []task.Definition {
	step := func(name string, run func(ctx context.Context, env *task.Env, rows *PGRows, o Options) error) task.Step {
		return task.Step{Name: name, Run: func(ctx context.Context, env *task.Env) error {
			var in input
			_ = json.Unmarshal(env.Task.Input, &in)
			rows := &PGRows{Conns: s.Conns, TaskID: env.Task.ID}
			o := in.Options
			// A start that resumed a stopped run may have changed the options.
			if run, err := rows.Run(ctx); err == nil {
				o = run.Options
			}
			return run(ctx, env, rows, o)
		}}
	}
	return []task.Definition{{
		Kind: Kind,
		Steps: []task.Step{
			step(StepCheck, s.stepCheck),
			step(StepDrainFlag, s.stepDrainFlag),
			step(StepPlan, s.stepPlan),
			step(StepMove, s.stepMove),
			step(StepDrainEmpty, s.stepDrainEmpty),
			step(StepDrainMapGroup, s.stepDrainMapGroup),
			step(StepDrainStandbys, s.stepDrainStandbys),
			step(StepWrite, s.stepWrite),
			step(StepDrainEnd, s.stepDrainEnd),
		},
		OnFail: s.onFail,
	}, s.splitDefinition()}
}

func (s *Service) stepCheck(ctx context.Context, env *task.Env, rows *PGRows, o Options) error {
	dbs, err := s.Backend.Databases(ctx, o)
	if err != nil {
		return err
	}
	before, err := s.Backend.Placement(ctx, dbs)
	if err != nil {
		return err
	}
	if _, err := rows.CreateRun(ctx, o, before); err != nil {
		return err
	}
	return s.Backend.Check(ctx, dbs)
}

func (s *Service) stepPlan(ctx context.Context, env *task.Env, rows *PGRows, o Options) error {
	dbs, err := s.Backend.Databases(ctx, o)
	if err != nil {
		return err
	}
	plan, err := s.Backend.Plan(ctx, dbs, o)
	if err != nil {
		return err
	}
	run, err := rows.Run(ctx)
	if err != nil {
		return err
	}
	if len(plan) > run.Planned {
		if err := rows.SetPlanned(ctx, len(plan)); err != nil {
			return err
		}
	}
	return env.SetDetail(ctx, fmt.Sprintf("%d bucket moves planned", len(plan)))
}

func (s *Service) loop(rows Rows) *Loop {
	return &Loop{Backend: s.Backend, Rows: rows, MaxAttempts: s.MaxAttempts, Backoff: s.Backoff, Poll: s.Poll, Guard: s.autoGuard}
}

func (s *Service) stepMove(ctx context.Context, _ *task.Env, rows *PGRows, o Options) error {
	return s.loop(rows).Moves(ctx, o)
}

func (s *Service) stepWrite(ctx context.Context, _ *task.Env, rows *PGRows, o Options) error {
	moves, err := rows.Moves(ctx)
	if err != nil {
		return err
	}
	dbs, err := s.Backend.Databases(ctx, o)
	if err != nil {
		return err
	}
	after, err := s.Backend.Placement(ctx, dbs)
	if err != nil {
		return err
	}
	attempts := s.MaxAttempts
	v := Judge(moves, attempts)
	if err := rows.Settle(ctx, v.Outcome, "", v.Text, after); err != nil {
		return err
	}
	// A failed run is audited once, by onFail (the step returns the error below); a run that ended well here.
	if v.Outcome != OutcomeFailed {
		s.audit(ctx, "rebalance."+v.Outcome, rows.TaskID, map[string]any{"movesDone": v.Done, "movesFailed": v.Failed, "movesSkipped": v.Skipped})
	}
	if v.Outcome == OutcomeFailed {
		return errors.New(v.Text)
	}
	return nil
}

// onFail finishes the run of a task that the engine has just recorded as failed or cancelled (the engine
// calls it only after its own record succeeded under the lease, so it never runs for a holder that lost the
// task; it also runs for a paused task that a person cancels). A move still marked running is not running
// any more (the extension's recovery undoes it).
//
// A cancel is the person's decision and not a failure: the run ends as "cancelled" (stopped by the
// operator; the audit log of cancelTask names who), its moves in flight as cancelled, and no taskFailed
// alert is raised. Anything else is a failure: outcome "failed", an alert.
func (s *Service) onFail(ctx context.Context, t task.Task) {
	cancelled := t.Error != nil && t.Error.Code == task.CodeCanceled
	text := "The rebalance task failed."
	if t.Error != nil {
		text = t.Error.Message
	}
	err := s.sys(ctx, func(c *pgx.Conn) error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		if cancelled {
			if _, err := tx.Exec(ctx, `UPDATE kingres_cp.rebalance_move SET state = 'cancelled', finished_at = clock_timestamp(),
				error = 'canceled: the run was canceled while the move was in flight' WHERE task_id = $1 AND state = 'running'`, t.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET outcome = 'cancelled', stopped_by = 'operator', stop_requested = false,
				finished_at = clock_timestamp(), error = NULL WHERE task_id = $1 AND (outcome IS NULL OR outcome = 'stopped')`, t.ID); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		if _, err := tx.Exec(ctx, `UPDATE kingres_cp.rebalance_move SET state = 'failed', finished_at = clock_timestamp(),
			error = 'interrupted: the run ended while the move was in flight' WHERE task_id = $1 AND state = 'running'`, t.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET outcome = 'failed', finished_at = clock_timestamp(), error = $2
			WHERE task_id = $1 AND (outcome IS NULL OR outcome = 'stopped')`, t.ID, clipText(text, 500)); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		s.log().Warn("recording the end of a rebalance", "task", t.ID, "error", err)
	}
	if cancelled {
		s.audit(ctx, "rebalance.cancelled", t.ID, nil)
		return
	}
	s.raiseFailed(ctx, t.ID, text)
	s.audit(ctx, "rebalance.failed", t.ID, map[string]any{"error": text})
}

// raiseFailed raises the alert taskFailed for the rebalance, with the task as its
// subject (AlertSubject.taskId), so that the alert links to the task and the report.
func (s *Service) raiseFailed(ctx context.Context, id int64, text string) {
	err := s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `INSERT INTO kingres_cp.alert (severity, kind, subject, message, raised_at)
			VALUES ('warning', 'taskFailed', $1, $2, now()) ON CONFLICT (kind, subject) WHERE cleared_at IS NULL DO NOTHING`,
			"task:"+strconv.FormatInt(id, 10), "The rebalance (task "+strconv.FormatInt(id, 10)+") failed: "+clipText(text, 300))
		return err
	})
	if err != nil {
		s.log().Warn("raising the alert for a failed rebalance", "task", id, "error", err)
	}
}

func (s *Service) audit(ctx context.Context, action string, id int64, details map[string]any) {
	if s.Audit == nil {
		return
	}
	s.Audit(context.WithoutCancel(ctx), audit.Event{Actor: "system", Source: "system", Action: action, ResourceType: "rebalance",
		ResourceID: strconv.FormatInt(id, 10), Outcome: audit.OutcomeSuccess, Details: details})
}

func (s *Service) sys(ctx context.Context, f func(c *pgx.Conn) error) error {
	c, err := s.Conns.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return f(c)
}

// active is the unfinished rebalance task, if any.
type active struct {
	ID        int64
	State     task.State
	UpdatedAt time.Time
	Detail    string
}

func (s *Service) active(ctx context.Context) (*active, error) {
	var a active
	var detail *string
	err := s.sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT id, state, updated_at, progress->>'detail' FROM kingres_cp.task
			WHERE kind = 'rebalance' AND state IN ('pending', 'running', 'paused') ORDER BY id DESC LIMIT 1`).Scan(&a.ID, &a.State, &a.UpdatedAt, &detail)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if detail != nil {
		a.Detail = *detail
	}
	return &a, err
}

// validate checks the options of a request.
func (s *Service) validate(ctx context.Context, o Options) error {
	var errs []problem.FieldError
	if o.MaxConcurrentMoves != 0 && (o.MaxConcurrentMoves < 1 || o.MaxConcurrentMoves > 64) {
		errs = append(errs, problem.FieldError{Field: "/maxConcurrentMoves", Message: "must be 1 to 64", Code: "out_of_range"})
	}
	errs = append(errs, validateNew(o)...)
	if len(o.Databases) > 0 {
		all, err := s.Backend.Databases(ctx, Options{})
		if err != nil {
			return err
		}
		known := map[string]bool{}
		for _, d := range all {
			known[d] = true
		}
		for i, d := range o.Databases {
			if !known[d] {
				errs = append(errs, problem.FieldError{Field: "/databases/" + strconv.Itoa(i), Message: "no such database with distributed tables", Code: "unknown_database"})
			}
		}
	}
	if len(errs) > 0 {
		return problem.Validation("The rebalance options are invalid.", errs...)
	}
	return nil
}

// Start starts a rebalance, or starts a stopped one again.
func (s *Service) Start(ctx context.Context, actor string, o Options) (task.Task, error) {
	return s.start(ctx, actor, o, false)
}

// start is Start; a drain (o.DrainServer set) is allowed to have nothing to move yet: the flag of the
// server is set by its second step, and the plan is made as if it were already set.
func (s *Service) start(ctx context.Context, actor string, o Options, drain bool) (task.Task, error) {
	eng := s.Engine()
	if eng == nil {
		s.log().Warn("rebalance: the task engine is not running")
		return task.Task{}, problem.New(problem.ServiceUnavailable, "The task engine is not running yet.").WithRetryAfter(5)
	}
	if err := s.validate(ctx, o); err != nil {
		return task.Task{}, err
	}
	o = s.resolve(ctx, o)
	if o.Trigger != TriggerAutomatic {
		s.acknowledge(ctx)
	}
	a, err := s.active(ctx)
	if err != nil {
		s.log().Warn("rebalance: reading the unfinished run", "error", err)
		return task.Task{}, problem.New(problem.ServiceUnavailable, "The system database is not available.").WithRetryAfter(5)
	}
	if a != nil && a.State != task.Paused {
		return task.Task{}, problem.New(problem.RebalanceInProgress, "A rebalance is already running (task "+strconv.FormatInt(a.ID, 10)+").")
	}
	dbs, err := s.Backend.Databases(ctx, o)
	if err != nil {
		s.log().Warn("rebalance: listing the databases", "error", err)
		return task.Task{}, problem.New(problem.ServiceUnavailable, "The databases cannot be read now.").WithRetryAfter(5)
	}
	if len(dbs) == 0 {
		return task.Task{}, problem.New(problem.NothingToRebalance, "There is no database with distributed tables to rebalance.")
	}
	// The plan first: an empty plan is the answer whatever the state of the nodes (a cluster of one
	// server has no other node to move to, and its check of the nodes has nothing to say).
	plan, err := s.Backend.Plan(ctx, dbs, o)
	if err != nil {
		return task.Task{}, asProblem(err)
	}
	if a == nil && len(plan) == 0 && !drain {
		return task.Task{}, problem.New(problem.NothingToRebalance, "The buckets are placed as the plan would place them; nothing to move.")
	}
	if err := s.Backend.Check(ctx, dbs); err != nil {
		return task.Task{}, asProblem(err)
	}
	if a != nil {
		return s.resume(ctx, eng, a, actor, o)
	}
	if o.MaxConcurrentMoves == 0 {
		o.MaxConcurrentMoves = defaultMaxConcurrentMoves
	}
	t, err := eng.Submit(ctx, Kind, task.Subject{Type: SubjectType, ID: SubjectID}, input{Options: o}, actor)
	if errors.Is(err, task.ErrSubjectBusy) {
		return task.Task{}, problem.New(problem.RebalanceInProgress, "A rebalance is already running.")
	}
	var x *task.ExcludedError
	if errors.As(err, &x) {
		return task.Task{}, x.Problem()
	}
	if err != nil {
		return task.Task{}, err
	}
	before, _ := s.Backend.Placement(ctx, dbs)
	rows := &PGRows{Conns: s.Conns, TaskID: t.ID}
	if _, err := rows.CreateRun(ctx, o, before); err != nil {
		s.log().Warn("recording the rebalance run; the first step repeats it", "task", t.ID, "error", err)
	}
	s.run(eng, t.ID)
	s.audit(ctx, "rebalance.started", t.ID, map[string]any{"actor": actor, "plannedMoves": len(plan)})
	return t, nil
}

func (s *Service) resume(ctx context.Context, eng *task.Engine, a *active, actor string, o Options) (task.Task, error) {
	if o.MaxConcurrentMoves == 0 {
		o.MaxConcurrentMoves = defaultMaxConcurrentMoves
	}
	err := s.sys(ctx, func(c *pgx.Conn) error { return ResumeRun(ctx, c, a.ID, o) })
	var x *task.ExcludedError
	switch {
	case errors.Is(err, task.ErrNotPaused):
		return task.Task{}, problem.New(problem.RebalanceInProgress, "A rebalance is already running.")
	case errors.As(err, &x):
		return task.Task{}, x.Problem()
	case err != nil:
		return task.Task{}, err
	}
	s.run(eng, a.ID)
	s.audit(ctx, "rebalance.resumed", a.ID, map[string]any{"actor": actor})
	return eng.Get(ctx, a.ID)
}

// run starts the task on this server (the manager leader) in the background.
func (s *Service) run(eng *task.Engine, id int64) {
	bg := context.Background()
	if s.BGFn != nil {
		bg = s.BGFn()
	}
	f := func() {
		if err := eng.RunNow(bg, id, nil); err != nil && bg.Err() == nil {
			s.log().Error("rebalance task ended with an error", "task", id, "error", err)
		}
	}
	if s.Background != nil {
		s.Background(f)
		return
	}
	go f()
}

func asProblem(err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	slog.Warn("rebalance: the cluster could not be inspected", "error", err)
	return problem.New(problem.ServiceUnavailable, "The cluster cannot be inspected now; the log of kingresd has the reason.").WithRetryAfter(5)
}

// Stop asks the running rebalance to stop. It answers once the request is
// recorded; the status says stopping until the run has stopped.
func (s *Service) Stop(ctx context.Context, actor string) error {
	a, err := s.active(ctx)
	if err != nil {
		return problem.New(problem.ServiceUnavailable, "The system database is not available.").WithRetryAfter(5)
	}
	if a == nil || a.State == task.Paused {
		return problem.New(problem.RebalanceNotRunning, "No rebalance is running.")
	}
	var ok bool
	if err := s.sys(ctx, func(c *pgx.Conn) error {
		var err error
		ok, err = RequestStop(ctx, c, a.ID, "operator")
		return err
	}); err != nil {
		return err
	}
	if !ok {
		return problem.New(problem.RebalanceNotRunning, "No rebalance is running.")
	}
	s.audit(ctx, "rebalance.stop_requested", a.ID, map[string]any{"actor": actor})
	return nil
}

// StopForFailover asks the rebalance that is running to stop because the server serverID has to be failed over (the
// design of the failover, section 4.5, stops the rebalance first: a rebalance excludes a failover while it is
// pending or running, migration 0009, and a rebalance with a stuck move would hold an automatic failover back for as long as the
// move takes to settle). It is the flag of Stop with the stopped-by value "safetyRule" (a value of the contract,
// no new one is needed). It reports whether a rebalance was running; nothing running is not an error. The caller
// waits for the stop with WaitStopped.
//
// What makes the stop short, even for a stuck move: the loop reads the flag at every poll of a move in flight,
// while it waits for a move that a run left behind (Recover) and while a call waits for the extension, and a wait
// ends at the flag; a move that is switching is not interrupted and ends within the extension's own bounds.
func (s *Service) StopForFailover(ctx context.Context, serverID string) (bool, error) {
	a, err := s.active(ctx)
	if err != nil {
		return false, err
	}
	if a == nil || a.State == task.Paused {
		return false, nil
	}
	var ok bool
	if err := s.sys(ctx, func(c *pgx.Conn) error {
		var err error
		ok, err = RequestStop(ctx, c, a.ID, "safetyRule")
		return err
	}); err != nil {
		return false, err
	}
	if ok {
		s.audit(ctx, "rebalance.stop_requested", a.ID, map[string]any{"actor": "system", "reason": "failover of server " + serverID})
	}
	return ok, nil
}

// WaitStopped waits until no rebalance is pending or running (it is paused, or it has ended), for at most bound,
// and says whether that happened. The check is the one of the exclusion: a paused rebalance does not exclude a failover.
func (s *Service) WaitStopped(ctx context.Context, bound time.Duration) (bool, error) {
	deadline := time.Now().Add(bound)
	for {
		a, err := s.active(ctx)
		if err != nil {
			return false, err
		}
		if a == nil || a.State == task.Paused {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
// Reads

func phaseOf(extPhase string) string {
	switch extPhase {
	case "preparing":
		return "prepare"
	case "copying":
		return "copy"
	case "catching_up":
		return "catchUp"
	case "switching":
		return "switch"
	default:
		return "cleanup"
	}
}

func optionsOf(o Options) *api.RebalanceOptions {
	return &api.RebalanceOptions{Databases: o.Databases, MaxConcurrentMoves: o.MaxConcurrentMoves,
		Strategy: o.Strategy, Threshold: o.Threshold, DrainOnly: o.DrainOnly, MaxMovesPerServer: o.MaxMovesPerServer,
		AcknowledgeFailures: o.AcknowledgeFailures, CleanupDelaySeconds: o.CleanupDelaySeconds}
}

// acknowledge records that an operator started a run: failures of the last automatic run no longer suspend
// the automatic mode (design 8.3, "a manual start acknowledges it").
func (s *Service) acknowledge(ctx context.Context) {
	_ = s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ('autostate.acknowledgedAt', to_jsonb(clock_timestamp()))
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`)
		return err
	})
}

// resolve fills the options a request left out from the cluster settings; a system database that cannot be read
// leaves the defaults of the settings.
func (s *Service) resolve(ctx context.Context, o Options) Options {
	set := DefaultSettings()
	if err := s.sys(ctx, func(c *pgx.Conn) (err error) { set, err = ReadSettings(ctx, c); return }); err != nil {
		s.log().Warn("rebalance: reading the settings; the defaults are used", "error", err)
		set = DefaultSettings()
	}
	return Resolve(o, set)
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Status is getRebalanceStatus.
func (s *Service) Status(ctx context.Context) (api.RebalanceStatus, error) {
	st := api.RebalanceStatus{State: "idle", Reasons: []string{}, InFlightMoves: []api.BucketMove{}}
	if s.Summary != nil {
		needed, reasons, err := s.Summary(ctx)
		if err != nil {
			return st, err
		}
		st.Needed = needed
		if reasons != nil {
			st.Reasons = reasons
		}
	}
	a, err := s.active(ctx)
	if err != nil {
		return st, err
	}
	var exclude int64
	if a != nil {
		exclude = a.ID
		id := a.ID
		st.TaskID = &id
		rows := &PGRows{Conns: s.Conns, TaskID: a.ID}
		run, err := rows.Run(ctx)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return st, err
		}
		moves, err := rows.Moves(ctx)
		if err != nil {
			return st, err
		}
		started := run.StartedAt
		if started.IsZero() {
			started = a.UpdatedAt
		}
		st.StartedAt = &started
		st.Options = optionsOf(run.Options)
		st.Strategy, st.Threshold, st.Trigger = run.Options.Strategy, run.Options.Threshold, run.Options.Trigger
		switch {
		case a.State == task.Paused:
			st.State = "stopped"
			st.StoppedBy = strp(run.StoppedBy)
			st.PausedReason = strp(a.Detail)
		case run.StopRequested:
			st.State = "stopping"
		default:
			st.State = "running"
		}
		pr := progressOf(run, moves)
		st.Progress = &pr
		if st.State == "stopped" {
			// A stopped run has no move in flight: a move that the loss of its source left unfinished (a failover of the source)
			// is settled by the extension when the rebalance is started again (resume_bucket_moves), not by this run.
			pr.MovesInFlight = 0
		}
		for _, m := range moves {
			if m.State != MoveRunning || st.State == "stopped" {
				continue
			}
			ph := m.Phase
			if ph == "" {
				ph = "preparing"
			}
			st.InFlightMoves = append(st.InFlightMoves, api.BucketMove{DatabaseName: m.Move.Database, ColocationID: m.Move.Colocation,
				Bucket: m.Move.Bucket, FromShardGroupID: m.Move.From, ToShardGroupID: m.Move.To, FromServerID: m.FromServer, ToServerID: m.ToServer,
				Phase: phaseOf(ph), Percent: m.Percent, SizeBytes: m.Size})
		}
	}
	st.Automatic = s.AutoStatus(ctx)
	last, err := s.latest(ctx, exclude)
	if err != nil {
		return st, err
	}
	st.LastReport = last
	if a == nil && last != nil && last.Outcome == OutcomeFailed {
		st.State = "failed"
	}
	return st, nil
}

func progressOf(run Run, moves []MoveRow) api.RebalanceProgress {
	var p api.RebalanceProgress
	moves = Current(moves)
	for _, m := range moves {
		switch m.State {
		case MoveDone:
			p.MovesDone++
			if m.Size != nil {
				p.BytesMoved += *m.Size
			}
		case MoveRunning:
			p.MovesInFlight++
		case MoveFailed:
			p.MovesFailed++
		case MoveSkipped, MoveCancelled:
			p.MovesSkipped++
		}
	}
	p.MovesTotal = max(run.Planned, len(moves))
	if p.MovesTotal > 0 {
		var part float64
		for _, m := range moves {
			if m.State == MoveRunning {
				part += m.Percent / 100
			}
		}
		p.Percent = min(100, (float64(p.MovesDone)+part)*100/float64(p.MovesTotal))
	}
	return p
}

// currentMoves selects the rows of a run that count in its totals (see Current): not a failed, skipped or
// cancelled row of a bucket that has a later row.
const currentMoves = `kingres_cp.rebalance_move m WHERE m.task_id = r.task_id AND NOT (m.state IN ('failed', 'skipped', 'cancelled') AND EXISTS
		(SELECT 1 FROM kingres_cp.rebalance_move x WHERE x.task_id = m.task_id AND x.database_name = m.database_name
			AND x.colocation_id = m.colocation_id AND x.bucket = m.bucket AND x.seq > m.seq))`

var summarySQL = `SELECT ` + runColsOf("r.") + `, t.updated_at,
	(SELECT count(*) FROM ` + currentMoves + ` AND m.state = 'done'),
	(SELECT count(*) FROM ` + currentMoves + ` AND m.state = 'failed'),
	(SELECT count(*) FROM ` + currentMoves + ` AND m.state IN ('skipped', 'cancelled')),
	(SELECT count(*) FROM ` + currentMoves + `),
	(SELECT coalesce(sum(size_bytes), 0) FROM ` + currentMoves + ` AND m.state = 'done')
	FROM kingres_cp.rebalance_run r JOIN kingres_cp.task t ON t.id = r.task_id`

func scanSummary(row pgx.Row) (api.RebalanceReportSummary, error) {
	var run Run
	var opts, before, after []byte
	var updated time.Time
	var done, failed, skipped, total, bytes int64
	if err := row.Scan(&run.TaskID, &opts, &run.StartedAt, &run.FinishedAt, &run.Outcome, &run.StopRequested, &run.StoppedBy,
		&run.Planned, &before, &after, &run.Error, &updated, &done, &failed, &skipped, &total, &bytes); err != nil {
		return api.RebalanceReportSummary{}, err
	}
	_ = json.Unmarshal(opts, &run.Options)
	end := updated
	if run.FinishedAt != nil {
		end = *run.FinishedAt
	}
	if end.Before(run.StartedAt) {
		end = run.StartedAt
	}
	sum := api.RebalanceReportSummary{ID: run.TaskID, Outcome: run.Outcome, StartedAt: run.StartedAt.UTC(), FinishedAt: end.UTC(),
		Options: optionsOf(run.Options), StoppedBy: strp(run.StoppedBy), Error: strp(run.Error),
		Trigger: run.Options.Trigger, Strategy: run.Options.Strategy, Threshold: run.Options.Threshold,
		Totals: api.ReportTotals{MovesTotal: int(max(int64(run.Planned), total)), MovesDone: int(done), MovesFailed: int(failed),
			MovesSkipped: int(skipped), BytesMoved: bytes, DurationSeconds: int64(end.Sub(run.StartedAt).Seconds())}}
	return sum, nil
}

// latest is the summary of the newest run that has an outcome, other than exclude.
func (s *Service) latest(ctx context.Context, exclude int64) (*api.RebalanceReportSummary, error) {
	var out *api.RebalanceReportSummary
	err := s.sys(ctx, func(c *pgx.Conn) error {
		sum, err := scanSummary(c.QueryRow(ctx, summarySQL+` WHERE r.outcome IS NOT NULL AND r.task_id <> $1 ORDER BY r.task_id DESC LIMIT 1`, exclude))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = &sum
		return nil
	})
	return out, err
}

// Reports is listRebalanceReports: newest first, the cursor is the id of the
// last report of the previous page.
func (s *Service) Reports(ctx context.Context, limit int, cursor string) (api.RebalanceReportPage, error) {
	page := api.RebalanceReportPage{Items: []api.RebalanceReportSummary{}}
	var after int64 = 1 << 62
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 1 {
			return page, problem.Validation("The cursor is not valid.", problem.FieldError{Field: "cursor", Message: "not a cursor of this listing", Code: "invalid"})
		}
		after = n
	}
	err := s.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, summarySQL+` WHERE r.outcome IS NOT NULL AND r.task_id < $1 ORDER BY r.task_id DESC LIMIT $2`, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sum, err := scanSummary(rows)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, sum)
		}
		return rows.Err()
	})
	if err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		next := strconv.FormatInt(page.Items[limit-1].ID, 10)
		page.NextCursor = &next
	}
	return page, nil
}

func placementEntries(p []Placement) []api.PlacementEntry {
	out := make([]api.PlacementEntry, len(p))
	for i, x := range p {
		out[i] = api.PlacementEntry{DatabaseName: x.Database, ServerID: x.Server, PrimaryBuckets: x.Buckets}
	}
	return out
}

// Report is getRebalanceReport; not found gives a 404 problem.
func (s *Service) Report(ctx context.Context, id int64) (api.RebalanceReport, error) {
	var rep api.RebalanceReport
	err := s.sys(ctx, func(c *pgx.Conn) error {
		sum, err := scanSummary(c.QueryRow(ctx, summarySQL+` WHERE r.task_id = $1 AND r.outcome IS NOT NULL`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return problem.New(problem.NotFound, "No rebalance report "+strconv.FormatInt(id, 10)+".")
		}
		if err != nil {
			return err
		}
		rep.RebalanceReportSummary = sum
		rows := &PGRows{Conns: s.Conns, TaskID: id}
		run, err := rows.Run(ctx)
		if err != nil {
			return err
		}
		rep.PlacementBefore, rep.PlacementAfter = placementEntries(run.Before), placementEntries(run.After)
		moves, err := rows.Moves(ctx)
		if err != nil {
			return err
		}
		rep.Moves = reportMoves(moves)
		return nil
	})
	return rep, err
}

func reportMoves(moves []MoveRow) []api.ReportMove {
	out := make([]api.ReportMove, 0, len(moves))
	for _, m := range moves {
		rm := api.ReportMove{DatabaseName: m.Move.Database, ColocationID: m.Move.Colocation, Bucket: m.Move.Bucket,
			FromShardGroupID: m.Move.From, ToShardGroupID: m.Move.To, FromServerID: m.FromServer, ToServerID: m.ToServer,
			SizeBytes: m.Size, Attempts: m.Attempts, Error: strp(m.Error), Reason: m.Move.Reason}
		if m.FinishedAt != nil {
			rm.DurationSeconds = m.FinishedAt.Sub(m.StartedAt).Seconds()
		}
		switch m.State {
		case MoveDone:
			rm.Outcome = "done"
			rm.Error = nil
		case MoveSkipped:
			rm.Outcome = "skipped"
			rm.SkipReason = strp(m.SkipReason)
		case MoveCancelled:
			rm.Outcome, rm.SkipReason = "skipped", strp("stopped")
		default:
			rm.Outcome = "failed"
		}
		out = append(out, rm)
	}
	return out
}

// Preview is previewRebalancePlan: the moves a start would make now, in order.
func (s *Service) Preview(ctx context.Context, req api.RebalancePlanRequest) (api.RebalancePlan, error) {
	o := Options{Databases: req.Databases, MaxConcurrentMoves: req.MaxConcurrentMoves, Strategy: req.Strategy, Threshold: req.Threshold,
		DrainOnly: req.DrainOnly, MaxMovesPerServer: req.MaxMovesPerServer, ExcludeServers: req.ExcludeServerIDs}
	if err := s.validate(ctx, o); err != nil {
		return api.RebalancePlan{}, err
	}
	o = s.resolve(ctx, o)
	limit := req.MoveLimit
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 5000 {
		return api.RebalancePlan{}, problem.Validation("The plan request is invalid.", problem.FieldError{Field: "/moveLimit", Message: "must be 1 to 5000", Code: "out_of_range"})
	}
	dbs, err := s.Backend.Databases(ctx, o)
	if err != nil {
		return api.RebalancePlan{}, asProblem(err)
	}
	plan := api.RebalancePlan{Moves: []api.PlannedMove{}, ServerImpacts: []api.ServerImpact{}, Warnings: []string{}}
	if len(dbs) == 0 {
		return plan, nil
	}
	moves, err := s.Backend.Plan(ctx, dbs, o)
	if err != nil {
		return plan, asProblem(err)
	}
	if err := s.Backend.Check(ctx, dbs); err != nil {
		plan.Warnings = append(plan.Warnings, "A start would be refused now: "+asProblem(err).(*problem.Problem).Detail)
	}
	placement, err := s.Backend.Placement(ctx, dbs)
	if err != nil {
		return plan, asProblem(err)
	}
	before, after := map[string]int{}, map[string]int{}
	for _, p := range placement {
		before[p.Server] += p.Buckets
		after[p.Server] += p.Buckets
	}
	var total int64
	known := true
	serverOf := map[string]string{}
	of := func(db string, g int32) string {
		k := db + "/" + strconv.Itoa(int(g))
		if v, ok := serverOf[k]; ok {
			return v
		}
		v := s.Backend.ServerOf(ctx, db, g)
		serverOf[k] = v
		return v
	}
	for i, m := range moves {
		from, to := of(m.Database, m.From), of(m.Database, m.To)
		after[from]--
		after[to]++
		if _, ok := before[to]; !ok {
			before[to] = 0
		}
		if m.Size != nil {
			total += *m.Size
		} else {
			known = false
		}
		if i < limit {
			pm := api.PlannedMove{DatabaseName: m.Database, ColocationID: m.Colocation, Bucket: m.Bucket,
				FromShardGroupID: m.From, ToShardGroupID: m.To, FromServerID: from, ToServerID: to, EstimatedSizeBytes: m.Size, Rank: max(0, 1000-i),
				Reason: m.Reason}
			if m.Cost > 0 {
				c := m.Cost
				pm.Cost = &c
			}
			plan.Moves = append(plan.Moves, pm)
		}
	}
	plan.Needed = len(moves) > 0
	plan.Truncated = len(moves) > limit
	plan.Strategy, plan.Threshold = o.Strategy, o.Threshold
	loadBefore, loadAfter, imbalance := s.loadsOf(ctx, dbs, o, moves, of)
	plan.Totals.MoveCount = len(moves)
	if known && len(moves) > 0 {
		plan.Totals.TotalBytes = &total
	}
	servers := make([]string, 0, len(before))
	for sv := range before {
		servers = append(servers, sv)
	}
	sort.Strings(servers)
	for _, sv := range servers {
		im := api.ServerImpact{ServerID: sv, PrimaryBucketsBefore: before[sv], PrimaryBucketsAfter: after[sv]}
		if lb, ok := loadBefore[sv]; ok {
			la := loadAfter[sv]
			im.LoadBefore, im.LoadAfter = &lb, &la
		}
		plan.ServerImpacts = append(plan.ServerImpacts, im)
	}
	plan.Imbalance = imbalance
	for _, db := range dbs {
		for _, w := range s.groupWarnings(ctx, db) {
			plan.Warnings = append(plan.Warnings, w)
		}
	}
	return plan, nil
}

// loadsOf is the load of every server by the strategy before and after the plan, and the largest deviation of a
// receiver from the ideal now; all empty when the extension has no group_load.
func (s *Service) loadsOf(ctx context.Context, dbs []string, o Options, moves []Move, serverOf func(db string, g int32) string) (before, after map[string]float64, imbalance *float64) {
	loads, err := s.Backend.Loads(ctx, dbs, o)
	if err != nil {
		return nil, nil, nil
	}
	before, after = map[string]float64{}, map[string]float64{}
	worst := 0.0
	for _, l := range loads {
		if l.Server == "" {
			continue
		}
		before[l.Server] += l.Load
		after[l.Server] += l.Load
		if l.Receiving && l.Deviation > worst {
			worst = l.Deviation
		}
	}
	for _, m := range moves {
		c := float64(m.Cost)
		if c == 0 && m.Size != nil {
			c = float64(*m.Size)
		}
		after[serverOf(m.Database, m.From)] -= c
		after[serverOf(m.Database, m.To)] += c
	}
	return before, after, &worst
}

// groupWarnings are the groups of a database that have buckets and no active primary (design 4.1): their
// buckets are not planned.
func (s *Service) groupWarnings(ctx context.Context, db string) []string {
	var out []string
	_ = s.Backend.with(ctx, db, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT DISTINCT m.group_id FROM kingres.bucket_map m WHERE NOT EXISTS (
			SELECT 1 FROM kingres.node n WHERE n.group_id = m.group_id AND n.role = 'primary' AND n.is_active) ORDER BY 1`)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
		for _, g := range ids {
			out = append(out, fmt.Sprintf("groupUnavailable: shard group %d of database %s has no active primary; its buckets are not planned.", g, db))
		}
		return err
	})
	return out
}
