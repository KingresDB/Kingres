// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package task_test

import (
	"context"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// hidden is a store that does not offer ActiveFor: the engine reads a window of the newest tasks.
type hidden struct{ task.Store }

func activeEngine(t *testing.T, st task.Store) *task.Engine {
	t.Helper()
	def := task.Definition{Kind: "k", Steps: []task.Step{{Name: "a", Run: func(context.Context, *task.Env) error { return nil }}}}
	e, err := task.NewEngine(st, []task.Definition{def}, task.Options{Holder: "h", Lease: time.Minute, StoreTries: 1})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ActiveFor sees an unfinished task of a subject however many tasks came after it, and not a finished one or one of another kind or subject.
func TestActiveForSeesATaskBeyondTheNewestWindow(t *testing.T) {
	ctx := context.Background()
	for name, mk := range map[string]func(task.Store) task.Store{"store answers": func(s task.Store) task.Store { return s }} {
		t.Run(name, func(t *testing.T) {
			st := mk(task.NewMemStore())
			e := activeEngine(t, st)
			first, err := e.Submit(ctx, "k", task.Subject{Type: "table", ID: "db/s.t"}, struct{}{}, "u")
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 1500; i++ { // newer tasks, all finished at once
				tk, err := e.Submit(ctx, "k", task.Subject{Type: "table", ID: "db/s.other"}, struct{}{}, "u")
				if err != nil {
					t.Fatal(err)
				}
				if err := e.RunNow(ctx, tk.ID, nil); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := e.ActiveFor(ctx, "k", "table", "db/s.t"); err != nil || !ok {
				t.Fatalf("an unfinished task 1500 tasks back: %v %v", ok, err)
			}
			if ok, _ := e.ActiveFor(ctx, "k", "table", "db/s.other"); ok {
				t.Error("finished tasks are not active")
			}
			if ok, _ := e.ActiveFor(ctx, "x", "table", "db/s.t"); ok {
				t.Error("another kind")
			}
			if err := e.RunNow(ctx, first.ID, nil); err != nil {
				t.Fatal(err)
			}
			if ok, _ := e.ActiveFor(ctx, "k", "table", "db/s.t"); ok {
				t.Error("a finished task is not active")
			}
		})
	}
}

// A store without ActiveFor is read through the newest thousand tasks.
func TestActiveForFallsBackToTheNewestTasks(t *testing.T) {
	ctx := context.Background()
	e := activeEngine(t, hidden{task.NewMemStore()})
	if _, err := e.Submit(ctx, "k", task.Subject{Type: "table", ID: "a"}, struct{}{}, "u"); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.ActiveFor(ctx, "k", "table", "a"); err != nil || !ok {
		t.Errorf("%v %v", ok, err)
	}
	if ok, _ := e.ActiveFor(ctx, "k", "table", "b"); ok {
		t.Error("another subject")
	}
}
