// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co
package task_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// storeCancelsCtx ends the context of the request as soon as the conditional cancel has been written, as a client that went away at that moment.
type storeCancelsCtx struct {
	*task.MemStore
	stop context.CancelFunc
}

func (s *storeCancelsCtx) RequestCancelIf(ctx context.Context, id int64, now time.Time, seen task.State, fail *task.Error) error {
	err := s.MemStore.RequestCancelIf(ctx, id, now, seen, fail)
	if err == nil {
		s.stop()
	}
	return err
}

// Get fails for a request that is gone, as a store of a database does.
func (s *storeCancelsCtx) Get(ctx context.Context, id int64) (task.Task, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, err
	}
	return s.MemStore.Get(ctx, id)
}

// M1: the hook of a task that a cancel ended without a run does not depend on the request: a request that is gone after the write does not skip it.
func TestTheHookOfACancelledPausedTaskRunsWhenTheRequestIsGone(t *testing.T) {
	clk := newClock()
	mem := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	_, tk := pausedCommitted(t, mem, clk, &hooks, func(context.Context, task.Task) string { return "ended" })
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	st := &storeCancelsCtx{MemStore: mem, stop: stop}
	e2 := engine(t, st, "B", clk, committedDef(&hooks, nil))
	if err := e2.Cancel(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("the request was not ended by the test")
	}
	if got, _ := mem.Get(context.Background(), tk.ID); got.State != task.Failed || hooks.Load() != 1 {
		t.Fatalf("the task is %s and the hook ran %d times", got.State, hooks.Load())
	}
}

// storeAlwaysChanged answers every conditional cancel with ErrStateChanged, as a task that changes state at each try.
type storeAlwaysChanged struct {
	*task.MemStore
	tries atomic.Int32
}

func (s *storeAlwaysChanged) RequestCancelIf(context.Context, int64, time.Time, task.State, *task.Error) error {
	s.tries.Add(1)
	return task.ErrStateChanged
}

// M4: five lost races end in a defined answer (ErrStateChanged, nothing recorded), and the note that asks the servers is built once, not at each try.
func TestACancelThatLosesEveryRaceAnswersAndBuildsTheNoteOnce(t *testing.T) {
	clk := newClock()
	mem := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	_, tk := pausedCommitted(t, mem, clk, &hooks, nil)
	st := &storeAlwaysChanged{MemStore: mem}
	var notes atomic.Int32
	d := committedDef(&hooks, nil)
	d.CanceledNote = func(context.Context, task.Task) string { notes.Add(1); return "ended" }
	e2 := engine(t, st, "B", clk, d)
	err := e2.Cancel(context.Background(), tk.ID)
	if !errors.Is(err, task.ErrStateChanged) {
		t.Fatalf("the answer after the last try: %v", err)
	}
	if st.tries.Load() != 5 || notes.Load() != 1 {
		t.Fatalf("%d tries, the note was built %d times", st.tries.Load(), notes.Load())
	}
	if got, _ := mem.Get(context.Background(), tk.ID); got.State != task.Paused || got.Error != nil || hooks.Load() != 0 {
		t.Fatalf("a lost race changed the task: %s %+v, hooks %d", got.State, got.Error, hooks.Load())
	}
}

// L2: a committed task that was resumed and that no run has claimed yet (pending) is cancelled like a paused one: it ends with the note, nothing is undone, the
// hook runs once; an uncommitted pending task that ran before is cancelled with its hook (it has work to undo).
func TestAPendingCommittedTaskIsCancellableLikeAPausedOne(t *testing.T) {
	for _, m := range storeMakers() {
		t.Run(m.name, func(t *testing.T) {
			clk := newClock()
			st := m.make(t, clk)
			var hooks atomic.Int32
			e, tk := pausedCommitted(t, st, clk, &hooks, func(context.Context, task.Task) string { return "ended with the note" })
			ctx := context.Background()
			if err := e.Resume(ctx, tk.ID); err != nil {
				t.Fatal(err)
			}
			if got, _ := e.Get(ctx, tk.ID); got.State != task.Pending {
				t.Fatalf("the task is %s", got.State)
			}
			if err := e.Cancel(ctx, tk.ID); err != nil {
				t.Fatal(err)
			}
			got, _ := e.Get(ctx, tk.ID)
			if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled || got.Error.Message != "ended with the note" || hooks.Load() != 1 {
				t.Fatalf("after the cancel: %s %+v, hooks %d", got.State, got.Error, hooks.Load())
			}
		})
	}
}

// H1b: the step of a task that has passed its point of no return is not interrupted by a cancel (the engine leaves it to the step, which goes forward and pauses
// itself); the step of a task that has not is.
func TestACancelDoesNotInterruptTheStepOfACommittedTask(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	var interrupted atomic.Bool
	marked := make(chan struct{})
	d := committedDef(&hooks, func(ctx context.Context, env *task.Env) error {
		if err := env.SetMark(ctx, "decided"); err != nil {
			return err
		}
		close(marked)
		select {
		case <-ctx.Done():
			interrupted.Store(true)
			return ctx.Err()
		case <-time.After(2500 * time.Millisecond):
			return nil
		}
	})
	d.Steps[0].Interruptible = true
	d.Cancellable = func(t task.Task) bool { return t.State != task.Done && t.State != task.Failed }
	e := engine(t, st, "A", clk, d)
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.committed", task.Subject{Type: "x", ID: "1"}, nil, "u")
	done := make(chan error, 1)
	go func() { _, err := e.RunOnce(ctx); done <- err }()
	<-marked
	if err := e.Cancel(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if interrupted.Load() {
		t.Error("the step of a committed task was interrupted by a cancel")
	}
	if got, _ := e.Get(ctx, tk.ID); got.State == task.Failed || hooks.Load() != 0 {
		t.Errorf("the task is %s and its hook ran %d times", got.State, hooks.Load())
	}
}

// pausedUncommitted makes a task of a kind without a point of no return that ran, paused and was resumed: it is pending again, its lease epoch is above zero.
func pausedUncommitted(t *testing.T, st task.Store, clk *clock, hooks *atomic.Int32, kind string) (*task.Engine, task.Task) {
	t.Helper()
	d := task.Definition{Kind: kind, Steps: []task.Step{{Name: "s", Run: func(context.Context, *task.Env) error { return task.Pause("waiting") }}},
		OnFail: func(context.Context, task.Task) { hooks.Add(1) }}
	e := engine(t, st, "A", clk, d)
	ctx := context.Background()
	tk, err := e.Submit(ctx, kind, task.Subject{Type: "x", ID: "1"}, nil, "u")
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := e.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("the run: %v %v", ran, err)
	}
	if got, _ := e.Get(ctx, tk.ID); got.State != task.Paused {
		t.Fatalf("the task is %s", got.State)
	}
	if err := e.Resume(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(ctx, tk.ID)
	return e, got
}

// L6: a pending task that is not committed and that ran before (a resumed one) is cancelled with its hook, once; a pending task that never ran is cancelled without it.
func TestAPendingUncommittedTaskGetsItsHookOnlyWhenItRanBefore(t *testing.T) {
	for _, m := range storeMakers() {
		t.Run(m.name, func(t *testing.T) {
			clk := newClock()
			st := m.make(t, clk)
			var hooks atomic.Int32
			e, tk := pausedUncommitted(t, st, clk, &hooks, "test.resumed")
			ctx := context.Background()
			if err := e.Cancel(ctx, tk.ID); err != nil {
				t.Fatal(err)
			}
			if got, _ := e.Get(ctx, tk.ID); got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled || hooks.Load() != 1 {
				t.Fatalf("after the cancel of the resumed task: %s %+v, hooks %d", got.State, got.Error, hooks.Load())
			}
			// a task that never ran
			never, err := e.Submit(ctx, "test.resumed", task.Subject{Type: "x", ID: "2"}, nil, "u")
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Cancel(ctx, never.ID); err != nil {
				t.Fatal(err)
			}
			if got, _ := e.Get(ctx, never.ID); got.State != task.Failed || hooks.Load() != 1 {
				t.Fatalf("after the cancel of the task that never ran: %s, hooks %d", got.State, hooks.Load())
			}
		})
	}
}

// L6 and M1: the hook of a resumed pending task does not depend on the request either.
func TestTheHookOfACancelledPendingTaskRunsWhenTheRequestIsGone(t *testing.T) {
	clk := newClock()
	mem := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	_, tk := pausedUncommitted(t, mem, clk, &hooks, "test.resumed")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	st := &storeCancelsCtx{MemStore: mem, stop: stop}
	d := task.Definition{Kind: "test.resumed", Steps: []task.Step{{Name: "s", Run: func(context.Context, *task.Env) error { return nil }}},
		OnFail: func(context.Context, task.Task) { hooks.Add(1) }}
	e2 := engine(t, st, "B", clk, d)
	if err := e2.Cancel(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("the request was not ended by the test")
	}
	if got, _ := mem.Get(context.Background(), tk.ID); got.State != task.Failed || hooks.Load() != 1 {
		t.Fatalf("the task is %s and the hook ran %d times", got.State, hooks.Load())
	}
}
