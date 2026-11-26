// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// stopTimeout bounds the shutdown of the instance; startTimeout the start and
// the wait for the system database; settleTimeout one write of a task.
const (
	stopTimeout   = 5 * time.Minute
	startTimeout  = 5 * time.Minute
	databaseWait  = 90 * time.Second
	settleTimeout = 30 * time.Second
	stopStep      = "Stop the instance"
	startStep     = "Start the instance"
	restartStep   = "Restart the instance"
)

// InstanceDefinitions are the definitions of the three kinds the engine does not
// run (instance.start, instance.stop and instance.restart are recorded and settled by their owner,
// see RecordStore). They exist for the case the owner ended before it settled
// the task (kingresd was killed): the recorded lease runs out, the engine claims
// the task and this step fails it.
func (o *Ops) InstanceDefinitions() []task.Definition {
	interrupted := func(context.Context, *task.Env) error {
		return errors.New("the server ended before the operation finished; check the state of the instance")
	}
	return []task.Definition{
		{Kind: KindInstanceStop, Steps: []task.Step{{Name: stopStep, Run: interrupted}}},
		{Kind: KindInstanceStart, Steps: []task.Step{{Name: startStep, Run: interrupted}}},
		{Kind: KindInstanceRestart, Steps: []task.Step{{Name: restartStep, Run: interrupted}}},
	}
}

func clip(s string) string {
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}

// newInstanceTask is the task of an instance operation, running its one step.
func newInstanceTask(kind, step, id, actor string) task.Task {
	now := time.Now().UTC()
	in, _ := json.Marshal(map[string]string{"instanceId": id})
	return task.Task{Kind: kind, State: task.Running, SubjectType: "instance", SubjectID: id, Input: in,
		Steps: []task.StepState{{Name: step, State: task.StepRunning}}, CreatedAt: now, UpdatedAt: now, CreatedBy: actor}
}

// ended returns t finished: done, or failed with err.
func ended(t task.Task, err error) task.Task {
	t.UpdatedAt = time.Now().UTC()
	if err == nil {
		t.State, t.Percent = task.Done, 100
		t.Steps = []task.StepState{{Name: t.Steps[0].Name, State: task.StepDone}}
		return t
	}
	t.State, t.Percent = task.Failed, 0
	t.Steps = []task.StepState{{Name: t.Steps[0].Name, State: task.StepFailed}}
	t.Error = &task.Error{Code: task.CodeStepFailed, Step: t.Steps[0].Name, Retryable: true, Message: clip(err.Error())}
	return t
}

// nextTaskID is the id announced for a task that cannot be written now: the one
// reserved while the database was up, else a number far above any sequence value
// (microseconds since 1970, below 2^53 so that JSON clients keep it exactly).
func (o *Ops) nextTaskID() int64 {
	o.life.mu.Lock()
	defer o.life.mu.Unlock()
	if id := o.life.reserved; id > 0 {
		o.life.reserved = 0
		return id
	}
	id := time.Now().UnixMicro()
	if id <= o.life.lastID {
		id = o.life.lastID + 1
	}
	o.life.lastID = id
	return id
}

// openTask writes the running task. When it cannot be written now (the system
// database is down, the leader cannot be reached) the task is kept in memory
// under an announced id and written when that is possible again; recorded tells
// which happened.
func (o *Ops) openTask(ctx context.Context, rec TaskRecorder, t task.Task) (out task.Task, recorded bool, err error) {
	wctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	t.ID = o.takeReserved()
	got, err := rec.Record(wctx, t)
	switch {
	case err == nil:
		return got, true, nil
	case errors.Is(err, task.ErrSubjectBusy):
		return task.Task{}, false, problem.New(problem.OperationInProgress, "Another operation on this instance is still running.")
	case errors.As(err, new(*task.ExcludedError)):
		// The instance belongs to a shard group that a failover is working on (migration 0011, KX004): the
		// stop or restart must not run now, and it is not kept in memory to run later.
		return task.Task{}, false, problem.New(problem.FailoverInProgress, "A failover of this instance's shard group is in progress; try again when it has finished.")
	}
	// A stop or a restart of a primary that a lease protects does not run on a record that cannot be written: the record
	// is where migrations 0013 and 0014 refuse it while a failover of its group is unfinished, and a server that cannot
	// reach the leader is exactly the one a failover is for (F2-09). A start, which never endangers a copy, is kept in
	// memory as before.
	if (t.Kind == KindInstanceStop || t.Kind == KindInstanceRestart) && o.EnforcedPrimary != nil && o.EnforcedPrimary() {
		o.Log.Warn("a stop or restart is refused: its record cannot be written and this server runs the primary of a group with a standby", "kind", t.Kind, "error", err)
		return task.Task{}, false, problem.New(problem.ServiceUnavailable, "The operation cannot be recorded now, and this server runs the primary of a shard group that has a standby; try again when the leader can be reached.").WithRetryAfter(5)
	}
	o.Log.Warn("the task of an instance operation cannot be written now; it is kept in memory", "kind", t.Kind, "error", err)
	if t.ID == 0 {
		t.ID = o.nextTaskID()
	}
	return t, false, nil
}

func (o *Ops) takeReserved() int64 {
	o.life.mu.Lock()
	defer o.life.mu.Unlock()
	id := o.life.reserved
	o.life.reserved = 0
	return id
}

// keep remembers a finished task that could not be written, for the next time it
// can.
func (o *Ops) keep(t task.Task) {
	o.life.mu.Lock()
	o.life.pending = append(o.life.pending, t)
	o.life.mu.Unlock()
}

// flush writes the finished tasks that were kept in memory.
func (o *Ops) flush(ctx context.Context, rec TaskRecorder) {
	o.life.mu.Lock()
	ts := o.life.pending
	o.life.pending = nil
	o.life.mu.Unlock()
	for _, t := range ts {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
		got, err := rec.Record(wctx, t)
		if err == nil && got.State != t.State {
			err = rec.Settle(wctx, t) // the row exists from an earlier write: settle it
		}
		cancel()
		if err != nil {
			o.Log.Error("writing the task of an instance operation", "task", t.ID, "kind", t.Kind, "error", err)
			o.keep(t)
		}
	}
}

// StopInstance validates the request against the state of the instance and
// starts the stop. The caller has checked force and no_replica_available
// (SoleUpToDateCopy). The task is recorded as running, returned with 202, and
// recorded as done just before the shutdown is requested; if the instance does
// not stop it is corrected to failed.
func (o *Ops) StopInstance(ctx context.Context, id, actor string) (task.Task, error) {
	lc, err := o.lifecycleCtl()
	if err != nil {
		return task.Task{}, err
	}
	undo, err := o.beginOp(ctx, "stop")
	if err != nil {
		return task.Task{}, err
	}
	rec := o.recorder()
	t, recorded, err := o.openTask(ctx, rec, newInstanceTask(KindInstanceStop, stopStep, id, actor))
	if err != nil {
		undo()
		return task.Task{}, err
	}
	o.wg.Add(1)
	go func() { //nolint:contextcheck // the run outlives the request
		defer o.wg.Done()
		o.runStop(lc, rec, t, recorded)
	}()
	return t, nil
}

func (o *Ops) runStop(lc LifecycleControl, rec TaskRecorder, t task.Task, recorded bool) {
	ctx := context.WithoutCancel(o.bg())
	if recorded {
		// While the database is up: an id for the task of the start that follows.
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if id := rec.Reserve(rctx); id > 0 {
			o.life.mu.Lock()
			o.life.reserved = id
			o.life.mu.Unlock()
		}
		cancel()
	}
	// From here on the system database of this server is going away.
	o.life.set(StateStopping, true)
	done := ended(t, nil)
	if recorded {
		sctx, cancel := context.WithTimeout(ctx, settleTimeout)
		err := rec.Settle(sctx, done)
		cancel()
		if err != nil {
			o.Log.Error("recording the stop as done before the shutdown failed; the task is kept in memory", "task", t.ID, "error", err)
			recorded = false
		}
	}
	sctx, cancel := context.WithTimeout(ctx, stopTimeout)
	err := lc.Stop(sctx, cluster.NodeID)
	if err == nil {
		if up, rerr := lc.Running(sctx, cluster.NodeID); rerr == nil && up {
			err = errors.New("the instance is still running after the shutdown")
		}
	}
	cancel()
	if err != nil {
		o.Log.Error("the instance did not stop", "task", t.ID, "error", err)
		o.life.set("", false)
		failed := ended(t, err)
		if recorded {
			fctx, fcancel := context.WithTimeout(ctx, settleTimeout)
			serr := rec.Settle(fctx, failed)
			fcancel()
			if serr != nil {
				o.Log.Error("correcting the task of the stop to failed", "task", t.ID, "error", serr)
				o.keep(failed)
			}
		} else {
			o.keep(failed)
			o.flush(ctx, rec)
		}
		return
	}
	o.life.set(StateStopped, true)
	if !recorded {
		o.keep(done)
	}
}

// StartInstance starts the stopped local instance. The system database of the
// leader is not reachable (it is hosted by the instance), so there the task is
// kept in memory under its announced id and written, finished, when the
// database is back (getTask answers 503 until then). A follower records it with
// the leader at once.
func (o *Ops) StartInstance(ctx context.Context, id, actor string) (task.Task, error) {
	lc, err := o.lifecycleCtl()
	if err != nil {
		return task.Task{}, err
	}
	undo, err := o.beginOp(ctx, "start")
	if err != nil {
		return task.Task{}, err
	}
	rec := o.recorder()
	t, recorded, err := o.openTask(ctx, rec, newInstanceTask(KindInstanceStart, startStep, id, actor))
	if err != nil {
		undo()
		return task.Task{}, err
	}
	o.Log.Info("starting the instance", "task", t.ID, "actor", actor, "recorded", recorded)
	o.wg.Add(1)
	go func() { //nolint:contextcheck // the run outlives the request
		defer o.wg.Done()
		o.runStart(lc, rec, t, recorded)
	}()
	return t, nil
}

func (o *Ops) runStart(lc LifecycleControl, rec TaskRecorder, t task.Task, recorded bool) {
	ctx, cancel := context.WithTimeout(o.bg(), startTimeout)
	defer cancel()
	err := lc.Start(ctx, cluster.NodeID)
	if err == nil {
		err = o.waitSystemDatabase(ctx)
	}
	fin := ended(t, err)
	if err != nil {
		o.Log.Error("the instance did not start", "task", t.ID, "error", err)
		// Still not running: stay stopped (and answer system_database_stopped), so
		// that the start can be tried again; running: the normal checks apply.
		if up, rerr := lc.Running(context.WithoutCancel(ctx), cluster.NodeID); rerr == nil && up {
			o.life.set("", false)
		} else {
			o.life.set(StateStopped, true)
		}
	}
	wctx := context.WithoutCancel(ctx)
	if recorded {
		sctx, scancel := context.WithTimeout(wctx, settleTimeout)
		serr := rec.Settle(sctx, fin)
		scancel()
		if serr != nil {
			o.Log.Error("writing the end of the start task", "task", t.ID, "error", serr)
			o.keep(fin)
		}
	} else {
		o.keep(fin)
	}
	if err == nil {
		// The database is back: write what was kept in memory, then open up.
		o.flush(wctx, rec)
		o.life.set("", false)
		o.Log.Info("the instance is running again", "task", t.ID)
	}
}

// waitSystemDatabase waits until a service connection to the system database
// works (the instance accepts connections before recovery may be over).
func (o *Ops) waitSystemDatabase(ctx context.Context) error {
	if o.Service == nil {
		return nil
	}
	deadline := time.Now().Add(databaseWait)
	for {
		c, err := o.Service.ServiceConn(ctx, sysdb.Database)
		if err == nil {
			err = c.Ping(ctx)
			_ = c.Close(context.WithoutCancel(ctx))
			if err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// SoleUpToDateCopy reports whether the instance is the only up-to-date copy of
// its shard group while replicas exist that are not caught up: stopping or
// restarting it then puts data at risk (409 no_replica_available unless force).
// A copy is up to date when its process runs and, for a standby, it is no more
// than MaxCaughtUpLagBytes behind (an unknown lag counts as behind). A group
// without replicas never blocks.
func SoleUpToDateCopy(topo api.ReplicationTopology, instanceID int32) bool {
	for _, g := range topo.Groups {
		var members []api.ReplicationMember
		if g.Primary != nil {
			members = append(members, *g.Primary)
		}
		members = append(members, g.Standbys...)
		mine := false
		for _, m := range members {
			if m.InstanceID == instanceID {
				mine = true
			}
		}
		if !mine || len(members) < 2 {
			continue
		}
		upToDate, meUpToDate := 0, false
		for _, m := range members {
			if !copyUpToDate(m) {
				continue
			}
			upToDate++
			if m.InstanceID == instanceID {
				meUpToDate = true
			}
		}
		if meUpToDate && upToDate == 1 {
			return true
		}
	}
	return false
}

// MaxCaughtUpLagBytes is the lag up to which a standby counts as caught up.
const MaxCaughtUpLagBytes = 1 << 20

func copyUpToDate(m api.ReplicationMember) bool {
	if m.State != StateRunning {
		return false
	}
	if m.Role == "primary" {
		return true
	}
	return m.LagBytes != nil && *m.LagBytes <= MaxCaughtUpLagBytes
}

// InstanceIDString formats an instance id for a task subject.
func InstanceIDString(id int32) string { return strconv.FormatInt(int64(id), 10) }

// RestartInstance restarts the local instance as a task after the state checks.
// Like the stop and start it is recorded and settled by this code, not run by
// the engine, so that it also works on a server that is not the leader; the end
// is written when the instance (and with it the system database) is back.
func (o *Ops) RestartInstance(ctx context.Context, id, actor string) (task.Task, error) {
	lc, err := o.lifecycleCtl()
	if err != nil {
		return task.Task{}, err
	}
	undo, err := o.beginOp(ctx, "restart")
	if err != nil {
		return task.Task{}, err
	}
	rec := o.recorder()
	t, recorded, err := o.openTask(ctx, rec, newInstanceTask(KindInstanceRestart, restartStep, id, actor))
	if err != nil {
		undo()
		return task.Task{}, err
	}
	o.wg.Add(1)
	go func() { //nolint:contextcheck // the run outlives the request
		defer o.wg.Done()
		o.runRestart(lc, rec, t, recorded)
	}()
	return t, nil
}

func (o *Ops) runRestart(lc LifecycleControl, rec TaskRecorder, t task.Task, recorded bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(o.bg()), stopTimeout+startTimeout)
	defer cancel()
	err := lc.Restart(ctx, cluster.NodeID)
	if err == nil {
		err = o.waitSystemDatabase(ctx)
	}
	if err != nil {
		o.Log.Error("the instance did not restart", "task", t.ID, "error", err)
	}
	fin := ended(t, err)
	if recorded {
		sctx, scancel := context.WithTimeout(ctx, settleTimeout)
		serr := rec.Settle(sctx, fin)
		scancel()
		if serr != nil {
			o.Log.Error("writing the end of the restart task", "task", t.ID, "error", serr)
			o.keep(fin)
		}
	} else {
		o.keep(fin)
	}
	o.life.clearIf(StateRestarting)
	if err == nil {
		o.flush(ctx, rec)
	}
}

// BeginConfigRestart brackets the restart that the configuration code (phase 13, config.restart) makes of
// an instance: for the instance that holds the system database it checks the state like
// restartThisServerInstance does, enters the state "restarting" (so that the server page does not say
// "running" while it is down, and no start or stop runs meanwhile) and returns the function that waits for
// the system database and leaves the state. Another instance (a standby, a primary that a failover
// brought here) is restarted by the manager alone, which holds the lease gate (P13R-06).
func (o *Ops) BeginConfigRestart(ctx context.Context, id int) (func(context.Context, error), error) {
	if id != cluster.NodeID {
		return func(context.Context, error) {}, nil
	}
	_, err := o.beginOp(ctx, "restart")
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, err error) {
		if err == nil {
			if werr := o.waitSystemDatabase(ctx); werr != nil {
				o.Log.Error("the system database did not come back after the configuration restart", "error", werr)
			}
		}
		o.life.clearIf(StateRestarting)
	}, nil
}
