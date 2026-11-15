// SPDX-License-Identifier: AGPL-3.0-only

package task_test

import (
	"context"
	"errors"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// A step that returns Pause leaves the task paused with its earlier steps done;
// Resume makes it pending, and the paused step runs again from its start.
func TestPauseAndResume(t *testing.T) {
	runs := map[string]int{}
	pauseOnce := true
	d := task.Definition{Kind: "test.kind", Steps: []task.Step{
		{Name: "a", Run: func(context.Context, *task.Env) error { runs["a"]++; return nil }},
		{Name: "b", Run: func(context.Context, *task.Env) error {
			runs["b"]++
			if pauseOnce {
				pauseOnce = false
				return task.Pause("stopped by the operator")
			}
			return nil
		}},
		{Name: "c", Run: func(context.Context, *task.Env) error { runs["c"]++; return nil }},
	}}
	st := task.NewMemStore()
	e := engine(t, st, "h1", newClock(), d)
	ctx := context.Background()
	tk, err := e.Submit(ctx, "test.kind", task.Subject{Type: "cluster", ID: "cluster"}, nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := e.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("RunOnce = %v, %v", ran, err)
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Paused || got.Holder != "" || got.Steps[0].State != task.StepDone || got.Steps[1].State != task.StepPending {
		t.Fatalf("after the pause: %+v", got)
	}
	if got.Detail != "stopped by the operator" {
		t.Errorf("detail = %q", got.Detail)
	}
	if ran, _ := e.RunOnce(ctx); ran {
		t.Fatal("a paused task was claimed")
	}
	// The one-task-per-subject rule still holds while the task is paused.
	if _, err := e.Submit(ctx, "test.kind", task.Subject{Type: "cluster", ID: "cluster"}, nil, "bob"); !errors.Is(err, task.ErrSubjectBusy) {
		t.Errorf("a second task for the subject: %v", err)
	}
	if err := e.Resume(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(ctx, tk.ID); !errors.Is(err, task.ErrNotPaused) {
		t.Errorf("a second resume: %v", err)
	}
	if ran, err := e.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("RunOnce after resume = %v, %v", ran, err)
	}
	got, _ = e.Get(ctx, tk.ID)
	if got.State != task.Done || runs["a"] != 1 || runs["b"] != 2 || runs["c"] != 1 {
		t.Errorf("state %s, runs %v", got.State, runs)
	}
}

// Cancelling a paused task fails it at once and runs the definition's cleanup.
func TestCancelPausedTask(t *testing.T) {
	cleaned := false
	d := task.Definition{Kind: "test.kind",
		Steps:  []task.Step{{Name: "a", Run: func(context.Context, *task.Env) error { return task.Pause("later") }}},
		OnFail: func(context.Context, task.Task) { cleaned = true }}
	e := engine(t, task.NewMemStore(), "h1", newClock(), d)
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.kind", task.Subject{Type: "cluster", ID: "cluster"}, nil, "alice")
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled || !cleaned {
		t.Errorf("state %s, error %+v, cleaned %v", got.State, got.Error, cleaned)
	}
}

// A step that returns Skip is recorded as skipped (not done), its reason is the
// task's detail, the task goes on and finishes, and a restart does not run the
// skipped step again.
func TestSkippedStep(t *testing.T) {
	runs := map[string]int{}
	d := task.Definition{Kind: "test.kind", Steps: []task.Step{
		{Name: "a", Run: func(context.Context, *task.Env) error { runs["a"]++; return nil }},
		{Name: "b", Run: func(context.Context, *task.Env) error {
			runs["b"]++
			return task.Skip("skipped: needs kingres_shard 0.5")
		}},
		{Name: "c", Run: func(context.Context, *task.Env) error { runs["c"]++; return nil }},
	}}
	st := task.NewMemStore()
	e := engine(t, st, "h1", newClock(), d)
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.kind", task.Subject{Type: "replication", ID: "standbys"}, nil, "alice")
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Done || got.Steps[1].State != task.StepSkipped || got.Steps[0].State != task.StepDone || got.Steps[2].State != task.StepDone {
		t.Fatalf("task %+v", got)
	}
	if got.Detail != "skipped: needs kingres_shard 0.5" {
		t.Errorf("detail %q", got.Detail)
	}
	if got.DoneSteps() != 3 {
		t.Errorf("DoneSteps = %d", got.DoneSteps())
	}
	if runs["a"] != 1 || runs["b"] != 1 || runs["c"] != 1 {
		t.Errorf("runs %v", runs)
	}
}
