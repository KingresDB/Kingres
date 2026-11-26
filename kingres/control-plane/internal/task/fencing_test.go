// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package task_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The fencing rule of the engine (README of the control plane): a holder that lost the lease of a
// task does nothing more in its name. These tests are its proof; each one fails on the engine
// as it was before (the hook ran, or the task was failed for good).

// supersede lets A start the step of a task, then takes the task over as B (A's lease ran out) and
// lets A's step return stepErr. It returns what A's RunOnce returned and the task as stored.
func supersede(t *testing.T, stepErr error, hooks *atomic.Int32) (error, task.Task) {
	t.Helper()
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	started, release := make(chan struct{}), make(chan struct{})
	d := task.Definition{Kind: "test.kind",
		Steps: []task.Step{{Name: "work", Run: func(context.Context, *task.Env) error {
			started <- struct{}{}
			<-release
			return stepErr
		}}},
		OnFail: func(context.Context, task.Task) { hooks.Add(1) }}
	a := engine(t, st, "A", clk, d)
	ctx := context.Background()
	tk, err := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := a.RunOnce(ctx); done <- err }()
	<-started
	clk.Add(time.Minute) // A's lease ran out on the store's clock ...
	if b, err := st.Claim(ctx, "B", 30*time.Second); err != nil || b == nil || b.Epoch != 2 {
		t.Fatalf("B's claim: %+v %v", b, err)
	}
	close(release) // ... and B holds the task when A's step ends
	runErr := <-done
	got, _ := a.Get(ctx, tk.ID)
	return runErr, got
}

// RB-01: a step error of a superseded holder that is not the lease error itself (a
// connection error caused by the pause, a timeout) used to be recorded as a failure of the task
// and to run OnFail, which rewrote the records of the new holder.
func TestSupersededHolderRunsNoHookWhateverTheStepError(t *testing.T) {
	for name, stepErr := range map[string]error{
		"plain error":          errors.New("connection reset"),
		"lease error":          fmt.Errorf("recording the move: %w", task.ErrLeaseLost),
		"fenced by the callee": fmt.Errorf("calling a server: %w", fence.ErrFenced),
	} {
		t.Run(name, func(t *testing.T) {
			var hooks atomic.Int32
			runErr, got := supersede(t, stepErr, &hooks)
			if !errors.Is(runErr, task.ErrLeaseLost) {
				t.Errorf("A ended with %v, want ErrLeaseLost", runErr)
			}
			if hooks.Load() != 0 {
				t.Errorf("OnFail ran %d times for a holder that lost the lease", hooks.Load())
			}
			if got.State != task.Running || got.Holder != "B" || got.Epoch != 2 || got.Error != nil {
				t.Errorf("the task of the new holder was changed: state %s holder %q epoch %d error %v", got.State, got.Holder, got.Epoch, got.Error)
			}
		})
	}
}

// RB-02: a lease that ran out while nobody took the task over is not a failure either. The task stays
// running with an expired lease, the next poll claims it (the epoch grows) and the step runs again.
func TestExpiredUnclaimedLeaseIsClaimedAgainNotFailed(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	var runs int
	d := task.Definition{Kind: "test.kind",
		Steps: []task.Step{{Name: "work", Run: func(context.Context, *task.Env) error {
			runs++
			if runs == 1 {
				clk.Add(time.Minute) // the process was frozen past its lease; nobody claimed meanwhile
				return fmt.Errorf("recording the move: %w", task.ErrLeaseLost)
			}
			return nil
		}}},
		OnFail: func(context.Context, task.Task) { hooks.Add(1) }}
	a := engine(t, st, "A", clk, d)
	ctx := context.Background()
	tk, _ := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if _, err := a.RunOnce(ctx); !errors.Is(err, task.ErrLeaseLost) {
		t.Fatalf("first run: %v", err)
	}
	got, _ := a.Get(ctx, tk.ID)
	if got.State != task.Running || got.Error != nil || hooks.Load() != 0 {
		t.Fatalf("after the hiccup: state %s error %v hooks %d", got.State, got.Error, hooks.Load())
	}
	if ran, err := a.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("claim again: %v %v", ran, err)
	}
	got, _ = a.Get(ctx, tk.ID)
	if got.State != task.Done || got.Epoch != 2 || runs != 2 || hooks.Load() != 0 {
		t.Errorf("state %s epoch %d runs %d hooks %d", got.State, got.Epoch, runs, hooks.Load())
	}
}

// The hook sees the task as recorded (state failed, the error), also when the step failed.
func TestHookSeesTheRecordedFailure(t *testing.T) {
	var seen task.Task
	d := task.Definition{Kind: "test.kind",
		Steps:  []task.Step{{Name: "work", Run: func(context.Context, *task.Env) error { return errors.New("disk full") }}},
		OnFail: func(_ context.Context, tk task.Task) { seen = tk }}
	e := engine(t, task.NewMemStore(), "A", newClock(), d)
	ctx := context.Background()
	if _, err := e.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if seen.State != task.Failed || seen.Error == nil || seen.Error.Code != task.CodeStepFailed || seen.Error.Message != "disk full" {
		t.Errorf("hook saw state %s error %+v", seen.State, seen.Error)
	}
}

// RB-08: a paused task can be cancelled; the hook sees a failed task with the code cancelled.
func TestCancelPausedTaskIsCancellableAndTheHookSeesTheRecord(t *testing.T) {
	var seen task.Task
	var hooks int
	d := task.Definition{Kind: "test.kind",
		Steps:  []task.Step{{Name: "a", Run: func(context.Context, *task.Env) error { return task.Pause("later") }}},
		OnFail: func(_ context.Context, tk task.Task) { seen = tk; hooks++ }}
	e := engine(t, task.NewMemStore(), "h1", newClock(), d)
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	paused, _ := e.Get(ctx, tk.ID)
	if !task.Cancellable(paused) {
		t.Fatal("a paused task must say that it can be cancelled")
	}
	if err := e.Cancel(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if hooks != 1 || seen.State != task.Failed || seen.Error == nil || seen.Error.Code != task.CodeCanceled {
		t.Errorf("hooks %d, hook saw state %s error %+v", hooks, seen.State, seen.Error)
	}
	if err := e.Cancel(ctx, tk.ID); !errors.Is(err, task.ErrNotCancellable) || hooks != 1 {
		t.Errorf("a second cancel: %v (hooks %d)", err, hooks)
	}
}

// RB-08: a cancel that arrives while the step runs and finds the step pausing ends the task as
// cancelled; it does not leave it paused for somebody to cancel again.
func TestCancelRequestedWhileRunningWinsOverAPause(t *testing.T) {
	var hooks int
	st := task.NewMemStore()
	var e *task.Engine
	var id int64
	d := task.Definition{Kind: "test.kind",
		Steps: []task.Step{{Name: "a", Run: func(ctx context.Context, _ *task.Env) error {
			if err := e.Cancel(ctx, id); err != nil {
				return err
			}
			return task.Pause("stopped")
		}}},
		OnFail: func(context.Context, task.Task) { hooks++ }}
	e = engine(t, st, "h1", newClock(), d)
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	id = tk.ID
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled || hooks != 1 {
		t.Errorf("state %s error %+v hooks %d", got.State, got.Error, hooks)
	}
}

// TestPGSupersededHolderRunsNoHook is RB-01 on a real system database: A's step is frozen, A's
// lease runs out, B claims the task (epoch 2) and A's step then returns an error. A's engine must
// leave the row of B alone and run no hook.
func TestPGSupersededHolderRunsNoHook(t *testing.T) {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	conn := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	st := task.NewPGStore(conn)
	var hooks atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	d := task.Definition{Kind: "test.kind",
		Steps: []task.Step{{Name: "work", Run: func(context.Context, *task.Env) error {
			started <- struct{}{}
			<-release
			return errors.New("connection reset")
		}}},
		OnFail: func(context.Context, task.Task) { hooks.Add(1) }}
	a := engine(t, st, "A", newClock(), d)
	tk, err := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := a.RunOnce(ctx); done <- err }()
	<-started
	if _, err := conn.Exec(ctx, `UPDATE kingres_cp.task SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, tk.ID); err != nil {
		t.Fatal(err)
	}
	if b, err := st.Claim(ctx, "B", time.Hour); err != nil || b == nil || b.Epoch != 2 {
		t.Fatalf("B's claim: %+v %v", b, err)
	}
	close(release)
	if err := <-done; !errors.Is(err, task.ErrLeaseLost) {
		t.Errorf("A ended with %v, want ErrLeaseLost", err)
	}
	got, _ := a.Get(ctx, tk.ID)
	if got.State != task.Running || got.Holder != "B" || got.Error != nil || hooks.Load() != 0 {
		t.Errorf("state %s holder %q error %v hooks %d", got.State, got.Holder, got.Error, hooks.Load())
	}
}

// RBR-07: a callee that keeps refusing the fence of a task that the store still lets this server hold (its
// mark is above any epoch that is reached, as after a restore of the system database) made the task loop
// for ever, a lease period per pass, silently. After MaxFencedRuns passes the task fails and its hook runs (the
// rebalance raises its alert there); before that every pass leaves the task running and runs no hook.
func TestACalleeThatAlwaysRefusesTheFenceFailsTheTaskAfterABound(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	var runs int
	d := task.Definition{Kind: "test.kind",
		Steps: []task.Step{{Name: "call a server", Run: func(context.Context, *task.Env) error {
			runs++
			return fmt.Errorf("calling a server: %w", fence.ErrFenced)
		}}},
		OnFail: func(context.Context, task.Task) { hooks.Add(1) }}
	a := engine(t, st, "A", clk, d)
	ctx := context.Background()
	tk, _ := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	for pass := 1; pass < task.MaxFencedRuns; pass++ {
		if _, err := a.RunOnce(ctx); !errors.Is(err, task.ErrLeaseLost) {
			t.Fatalf("pass %d: %v, want the run to stand down", pass, err)
		}
		if got, _ := a.Get(ctx, tk.ID); got.State != task.Running || hooks.Load() != 0 {
			t.Fatalf("pass %d: state %s, hooks %d: a pass below the bound must leave the task running", pass, got.State, hooks.Load())
		}
		clk.Add(time.Minute) // the lease of the run that stood down ends; the next poll claims the task again
	}
	if ran, err := a.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("the last pass: %v %v", ran, err)
	}
	got, _ := a.Get(ctx, tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeFenced || hooks.Load() != 1 || runs != task.MaxFencedRuns {
		t.Fatalf("after the bound: state %s error %+v hooks %d runs %d", got.State, got.Error, hooks.Load(), runs)
	}
}
