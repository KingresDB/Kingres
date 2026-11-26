// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package task_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// A task of a kind with a Committed guard that has passed its point of no return is never failed or cancelled by the engine: it is paused with a detail that says
// that it is committed and not finished, its hook does not run, and a resume goes on from the mark.

func committedDef(hooks *atomic.Int32, run func(ctx context.Context, env *task.Env) error) task.Definition {
	return task.Definition{Kind: "test.committed",
		Steps: []task.Step{{Name: "last", Run: run}},
		Committed: func(t task.Task) (string, bool) {
			if t.Mark == "decided" || t.Mark == task.MarkUnreadable {
				return "NOTE: committed, not finished.", true
			}
			return "", false
		},
		OnFail: func(context.Context, task.Task) { hooks.Add(1) }}
}

func TestACommittedTaskIsPausedByAStepErrorAndResumes(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	runs := 0
	d := committedDef(&hooks, func(ctx context.Context, env *task.Env) error {
		runs++
		if runs == 1 {
			if err := env.SetMark(ctx, "decided"); err != nil {
				return err
			}
			return errors.New("the store went away")
		}
		if env.Task.Mark != "decided" {
			return fmt.Errorf("the mark is %q", env.Task.Mark)
		}
		return nil
	})
	e := engine(t, st, "A", clk, d)
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.committed", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if ran, err := e.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("first run: %v %v", ran, err)
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Paused || hooks.Load() != 0 || got.Error != nil {
		t.Fatalf("state %s hooks %d error %v: a committed task must be paused, not failed", got.State, hooks.Load(), got.Error)
	}
	if !strings.Contains(got.Detail, "NOTE: committed, not finished.") || !strings.Contains(got.Detail, "the store went away") {
		t.Errorf("the detail does not say what happened: %q", got.Detail)
	}
	if err := e.Resume(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if ran, err := e.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("second run: %v %v", ran, err)
	}
	if got, _ := e.Get(ctx, tk.ID); got.State != task.Done {
		t.Fatalf("after the resume: %s %+v", got.State, got.Error)
	}
}

func TestAnUncommittedTaskWithTheGuardStillFails(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	e := engine(t, st, "A", clk, committedDef(&hooks, func(context.Context, *task.Env) error { return errors.New("boom") }))
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.committed", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(ctx, tk.ID); got.State != task.Failed || hooks.Load() != 1 {
		t.Fatalf("state %s hooks %d", got.State, hooks.Load())
	}
}

// The give-up after repeated fence refusals does not fail a committed task either.
func TestACommittedTaskIsPausedWhenTheFenceGiveUpComes(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	runs := 0
	e := engine(t, st, "A", clk, committedDef(&hooks, func(ctx context.Context, env *task.Env) error {
		runs++
		if env.Task.Mark == "" {
			if err := env.SetMark(ctx, "decided"); err != nil {
				return err
			}
		}
		return fmt.Errorf("calling a server: %w", fence.ErrFenced)
	}))
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.committed", task.Subject{Type: "x", ID: "1"}, nil, "u")
	for pass := 1; pass <= task.MaxFencedRuns; pass++ {
		_, _ = e.RunOnce(ctx)
		clk.Add(time.Minute)
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Paused || hooks.Load() != 0 || runs != task.MaxFencedRuns {
		t.Fatalf("state %s hooks %d runs %d error %v", got.State, hooks.Load(), runs, got.Error)
	}
	if !strings.Contains(got.Detail, "NOTE: committed, not finished.") || !strings.Contains(got.Detail, "newer run") {
		t.Errorf("the detail: %q", got.Detail)
	}
}

// A cancel request that a committed task honours pauses it: nothing is undone and the hook does not run.
func TestACancelOfACommittedTaskPausesIt(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	var hooks atomic.Int32
	var e *task.Engine
	e = engine(t, st, "A", clk, committedDef(&hooks, func(ctx context.Context, env *task.Env) error {
		if err := env.SetMark(ctx, "decided"); err != nil {
			return err
		}
		if err := e.Cancel(ctx, env.Task.ID); err != nil {
			return err
		}
		return task.Pause("the step saw the request")
	}))
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.committed", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Paused || hooks.Load() != 0 || got.Mark != "decided" {
		t.Fatalf("state %s hooks %d mark %q error %v", got.State, hooks.Load(), got.Mark, got.Error)
	}
	if !strings.Contains(got.Detail, "cancel was requested") {
		t.Errorf("the detail: %q", got.Detail)
	}
	if err := e.Resume(context.Background(), tk.ID); err != nil {
		t.Fatalf("a paused committed task resumes: %v", err)
	}
}

// getFailsAfterFinish is a store whose reads fail once a task was recorded as ended: the hook of the engine cannot read the mark.
type getFailsAfterFinish struct {
	*task.MemStore
	ended atomic.Bool
}

func (s *getFailsAfterFinish) Finish(ctx context.Context, id int64, l task.Lease, state task.State, e *task.Error) error {
	err := s.MemStore.Finish(ctx, id, l, state, e)
	if err == nil {
		s.ended.Store(true)
	}
	return err
}

func (s *getFailsAfterFinish) Get(ctx context.Context, id int64) (task.Task, error) {
	if s.ended.Load() {
		return task.Task{}, errors.New("the store cannot be read")
	}
	return s.MemStore.Get(ctx, id)
}

// The engine side of MarkUnreadable: a hook whose store cannot be asked is told so (a hook must read it as "possibly decided"), and the hook still runs.
func TestTheHookIsToldWhenTheMarkCannotBeRead(t *testing.T) {
	clk := newClock()
	st := &getFailsAfterFinish{MemStore: task.NewMemStoreClock(clk.Now)}
	var seen atomic.Value
	d := task.Definition{Kind: "test.unreadable",
		Steps:  []task.Step{{Name: "boom", Run: func(ctx context.Context, env *task.Env) error { return errors.New("boom") }}},
		OnFail: func(_ context.Context, tk task.Task) { seen.Store(tk.Mark) }}
	e := engine(t, st, "A", clk, d)
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.unreadable", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := seen.Load().(string); got != task.MarkUnreadable {
		t.Fatalf("the hook of task %d saw the mark %q", tk.ID, got)
	}
}

// FileStore keeps the mark: it is on disk when the file is read again.
func TestFileStoreKeepsTheMarkAcrossAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	fs, err := task.OpenFileStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var hooks atomic.Int32
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	d := committedDef(&hooks, func(ctx context.Context, env *task.Env) error {
		if err := env.SetMark(ctx, "decided"); err != nil {
			return err
		}
		stop() // the process ends here: nothing else is saved after the mark
		return ctx.Err()
	})
	e, err := task.NewEngine(fs, []task.Definition{d}, task.Options{Holder: "A", Lease: time.Minute, StoreTries: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tk, err := e.Submit(ctx, "test.committed", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = e.RunOnce(runCtx)
	if got, _ := fs.Get(ctx, tk.ID); got.Mark != "decided" {
		t.Fatalf("the mark in the open store: %q", got.Mark)
	}
	again, err := task.OpenFileStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Get(ctx, tk.ID)
	if err != nil || got.Mark != "decided" {
		t.Fatalf("the mark after the reload: %q %v", got.Mark, err)
	}
	// a write without the lease is refused by the file store too
	if err := again.SetMark(ctx, tk.ID, task.Lease{Holder: "intruder", Epoch: 99}, "forged"); err == nil {
		t.Error("a mark was written without the lease")
	}
}
