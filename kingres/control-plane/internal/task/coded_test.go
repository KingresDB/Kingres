// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package task_test

import (
	"context"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

type codedErr struct{}

func (codedErr) Error() string       { return "lease_still_held: the group is alive" }
func (codedErr) TaskCode() string    { return "lease_still_held" }
func (codedErr) TaskRetryable() bool { return true }

// A step that fails with a task.Coded error records its own code and retry hint; any other
// error is recorded as step_failed (the codes of the failover task are documented, not an enum).
func TestAStepErrorCanCarryItsOwnCode(t *testing.T) {
	clk := newClock()
	def := task.Definition{Kind: "test.coded", Steps: []task.Step{{Name: "a", Run: func(context.Context, *task.Env) error { return codedErr{} }}}}
	e, err := task.NewEngine(task.NewMemStore(), []task.Definition{def}, task.Options{Holder: "h1", Lease: 30 * time.Second, Now: clk.Now, StoreTries: 1})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := e.Submit(context.Background(), "test.coded", task.Subject{Type: "server", ID: "s1"}, struct{}{}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	_ = e.RunNow(context.Background(), tk.ID, nil)
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != "lease_still_held" || !got.Error.Retryable || got.Error.Step != "a" {
		t.Fatalf("task %+v error %+v", got.State, got.Error)
	}
}
