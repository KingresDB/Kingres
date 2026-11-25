// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package task_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The cancel of a paused committed task is the exit of a task that cannot be resumed; the guard of the cancel is part of the store's write; a request that the
// guard refused does not stay behind.

type storeMaker struct {
	name string
	make func(t *testing.T, clk *clock) task.Store
}

func storeMakers() []storeMaker {
	return []storeMaker{
		{"mem", func(t *testing.T, clk *clock) task.Store { return task.NewMemStoreClock(clk.Now) }},
		{"file", func(t *testing.T, clk *clock) task.Store {
			fs, err := task.OpenFileStore(filepath.Join(t.TempDir(), "tasks.json"), nil)
			if err != nil {
				t.Fatal(err)
			}
			return fs
		}},
		{"pg", func(t *testing.T, clk *clock) task.Store {
			in := pgtest.Start(t, nil)
			ctx := context.Background()
			if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
				t.Fatal(err)
			}
			conn := in.Connect(t, "kingres_system")
			if _, err := sysdb.Migrate(ctx, conn); err != nil {
				t.Fatal(err)
			}
			return task.NewPGStore(conn)
		}},
	}
}

// pausedCommitted makes a committed task that is paused (the step records the mark and fails).
func pausedCommitted(t *testing.T, st task.Store, clk *clock, hooks *atomic.Int32, note func(context.Context, task.Task) string) (*task.Engine, task.Task) {
	t.Helper()
	d := committedDef(hooks, func(ctx context.Context, env *task.Env) error {
		if err := env.SetMark(ctx, "decided"); err != nil {
			return err
		}
		return errors.New("a server is gone")
	})
	d.CanceledNote = note
	e := engine(t, st, "A", clk, d)
	ctx := context.Background()
	tk, err := e.Submit(ctx, "test.committed", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := e.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("the run: %v %v", ran, err)
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Paused || got.Mark != "decided" {
		t.Fatalf("the task is %s with the mark %q", got.State, got.Mark)
	}
	return e, got
}

// M4: the write of the store names the state that the caller decided on. A task that is paused is not touched by a request made for a running one, and a request
// for a paused one ends it once.
func TestCondCancelNamesTheStateItDecidedOn(t *testing.T) {
	for _, m := range storeMakers() {
		t.Run(m.name, func(t *testing.T) {
			clk := newClock()
			st := m.make(t, clk)
			var hooks atomic.Int32
			_, tk := pausedCommitted(t, st, clk, &hooks, nil)
			cs, ok := st.(task.CondCancelStore)
			if !ok {
				t.Fatal("the store has no conditional cancel")
			}
			ctx := context.Background()
			fail := &task.Error{Code: task.CodeCanceled, Message: "ended by the operator"}
			if err := cs.RequestCancelIf(ctx, tk.ID, clk.Now(), task.Running, fail); !errors.Is(err, task.ErrStateChanged) {
				t.Fatalf("a request for a running task on a paused one: %v", err)
			}
			if err := cs.RequestCancelIf(ctx, tk.ID, clk.Now(), task.Pending, fail); !errors.Is(err, task.ErrStateChanged) {
				t.Fatalf("a request for a pending task on a paused one: %v", err)
			}
			got, _ := st.Get(ctx, tk.ID)
			if got.State != task.Paused || got.CancelRequested || got.Error != nil {
				t.Fatalf("the refused requests changed the task: %s cancel %v %+v", got.State, got.CancelRequested, got.Error)
			}
			if err := cs.RequestCancelIf(ctx, tk.ID, clk.Now(), task.Paused, fail); err != nil {
				t.Fatalf("the request for the paused task: %v", err)
			}
			got, _ = st.Get(ctx, tk.ID)
			if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled || got.Error.Message != "ended by the operator" || got.Mark != "decided" {
				t.Fatalf("after the request: %s %+v mark %q", got.State, got.Error, got.Mark)
			}
			if err := cs.RequestCancelIf(ctx, tk.ID, clk.Now(), task.Paused, fail); !errors.Is(err, task.ErrNotCancellable) {
				t.Fatalf("a second request: %v", err)
			}
			if err := cs.RequestCancelIf(ctx, 987654, clk.Now(), task.Paused, fail); !errors.Is(err, task.ErrNotFound) {
				t.Fatalf("an unknown task: %v", err)
			}
		})
	}
}

// H1: an operator cancels a paused committed task: it ends as failed with the message of the kind, nothing is resumed, and the mark is kept.
func TestACancelOfAPausedCommittedTaskEndsItWithTheNote(t *testing.T) {
	for _, m := range storeMakers() {
		t.Run(m.name, func(t *testing.T) {
			clk := newClock()
			st := m.make(t, clk)
			var hooks atomic.Int32
			e, tk := pausedCommitted(t, st, clk, &hooks, func(_ context.Context, tk task.Task) string {
				return "NOTE for task " + tk.Mark + ": finish it by hand."
			})
			ctx := context.Background()
			if err := e.Cancel(ctx, tk.ID); err != nil {
				t.Fatal(err)
			}
			got, _ := e.Get(ctx, tk.ID)
			if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled || got.Error.Message != "NOTE for task decided: finish it by hand." {
				t.Fatalf("after the cancel: %s %+v", got.State, got.Error)
			}
			if err := e.Resume(ctx, tk.ID); err == nil {
				t.Error("a task that was ended can be resumed")
			}
			if err := e.Cancel(ctx, tk.ID); !errors.Is(err, task.ErrNotCancellable) {
				t.Errorf("a second cancel: %v", err)
			}
		})
	}
}

// staleGet is a store whose first read of the task shows an older state, as a read that was made before the task changed.
type staleGet struct {
	*task.MemStore
	stale atomic.Pointer[task.Task]
}

func (s *staleGet) Get(ctx context.Context, id int64) (task.Task, error) {
	if old := s.stale.Swap(nil); old != nil {
		return *old, nil
	}
	return s.MemStore.Get(ctx, id)
}

// M4: a cancel that was decided on a read that shows the task running (a pause for a committed task) never ends it when the task paused in between: the request is
// met by the pause, and the task is not failed by the same click.
func TestAStaleReadCannotEndACommittedTaskThatPausedMeanwhile(t *testing.T) {
	clk := newClock()
	st := &staleGet{MemStore: task.NewMemStoreClock(clk.Now)}
	var hooks atomic.Int32
	_, tk := pausedCommitted(t, st, clk, &hooks, func(context.Context, task.Task) string { return "ended" })
	ctx := context.Background()
	view := tk
	view.State = task.Running
	st.stale.Store(&view)
	d := committedDef(&hooks, nil)
	d.CanceledNote = func(context.Context, task.Task) string { return "ended" }
	e2 := engine(t, st, "B", clk, d)
	if err := e2.Cancel(ctx, tk.ID); err != nil {
		t.Fatalf("the cancel: %v", err)
	}
	got, _ := st.MemStore.Get(ctx, tk.ID)
	if got.State != task.Paused || got.Error != nil {
		t.Fatalf("a stale read ended the committed task: %s %+v", got.State, got.Error)
	}
}

// H1b: the commit mark is conditional, in the statement of the store, on no cancel request: a cancel that was accepted before wins and nothing is written; a
// cancel that comes after the mark is a cancel of a committed task. A run without the lease gets the answer of a lost lease, not that of a cancel.
func TestTheMarkIsNotWrittenOverACancelRequest(t *testing.T) {
	for _, m := range storeMakers() {
		t.Run(m.name, func(t *testing.T) {
			clk := newClock()
			st := m.make(t, clk)
			var e *task.Engine
			var cancelled, marked, forged error
			var markAfterCancel, markAfterOK string
			d := task.Definition{Kind: "test.mark", Steps: []task.Step{{Name: "s", Run: func(ctx context.Context, env *task.Env) error {
				// no request yet: the mark is written
				marked = env.SetMark(ctx, "first")
				markAfterOK, _ = env.ReadMark(ctx)
				if err := e.Cancel(ctx, env.Task.ID); err != nil {
					return err
				}
				// a request is recorded: the next mark is refused and the store keeps the old one
				cancelled = env.SetMark(ctx, "second")
				markAfterCancel, _ = env.ReadMark(ctx)
				forged = st.(task.MarkStore).SetMark(ctx, env.Task.ID, task.Lease{Holder: "intruder", Epoch: 99}, "forged")
				return nil
			}}}}
			e = engine(t, st, "A", clk, d)
			ctx := context.Background()
			_, _ = e.Submit(ctx, "test.mark", task.Subject{Type: "x", ID: "1"}, nil, "u")
			if ran, err := e.RunOnce(ctx); !ran || err != nil {
				t.Fatalf("the run: %v %v", ran, err)
			}
			if marked != nil || markAfterOK != "first" {
				t.Errorf("a mark without a request: %v, the store holds %q", marked, markAfterOK)
			}
			if !errors.Is(cancelled, task.ErrCancelRequested) || markAfterCancel != "first" {
				t.Errorf("a mark over a request: %v, the store holds %q", cancelled, markAfterCancel)
			}
			if !errors.Is(forged, task.ErrLeaseLost) {
				t.Errorf("a mark of a run without the lease: %v", forged)
			}
		})
	}
}

// H1a: a task that waits in a step (a server that is gone) is ended by a cancel as a task that fails before its point of no return: the step sees the cancel, the
// hook runs, and a cancel that comes after the mark is written is not undone (the task is held, not ended).
func TestACancelReachesAStepThatWaitsBeforeTheMark(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	entered := make(chan struct{})
	d := committedDef(&hooks, func(ctx context.Context, env *task.Env) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	d.Steps[0].Interruptible = true
	d.Cancellable = func(t task.Task) bool { return t.State != task.Done && t.State != task.Failed }
	e := engine(t, st, "A", clk, d)
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.committed", task.Subject{Type: "x", ID: "1"}, nil, "u")
	done := make(chan error, 1)
	go func() { _, err := e.RunOnce(ctx); done <- err }()
	<-entered
	if err := e.Cancel(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled || hooks.Load() != 1 {
		t.Fatalf("after the cancel: %s %+v, hooks %d", got.State, got.Error, hooks.Load())
	}
}

// M3: a note that is cut is cut between two characters.
func TestACutNoteIsStillText(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	d := committedDef(&hooks, func(ctx context.Context, env *task.Env) error {
		if err := env.SetMark(ctx, "decided"); err != nil {
			return err
		}
		return errors.New(strings.Repeat("é", 400))
	})
	d.Committed = func(t task.Task) (string, bool) {
		return "a" + strings.Repeat("é", 400), t.Mark == "decided"
	}
	e := engine(t, st, "A", clk, d)
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.committed", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Paused || !utf8.ValidString(got.Detail) || !strings.HasSuffix(got.Detail, "...") {
		t.Fatalf("state %s, valid %v, detail %q", got.State, utf8.ValidString(got.Detail), got.Detail)
	}
}
