// SPDX-License-Identifier: AGPL-3.0-only

package task_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func gated(t *testing.T, st task.Store, open *atomic.Bool, d ...task.Definition) *task.Engine {
	t.Helper()
	clk := newClock()
	e, err := task.NewEngine(st, d, task.Options{Holder: "h1", Lease: 30 * time.Second, Now: clk.Now, StoreTries: 1, Gate: open.Load})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// T16: a closed gate claims and runs nothing; the task stays pending and runs once the gate opens.
func TestAClosedGateClaimsAndRunsNothing(t *testing.T) {
	c := &counter{n: map[string]int{}}
	var open atomic.Bool
	st := task.NewMemStore()
	e := gated(t, st, &open, defn(c, "a", "b"))
	tk, err := e.Submit(context.Background(), "test.kind", task.Subject{Type: "database", ID: "shop"}, struct{}{}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := e.RunOnce(context.Background()); ran || err != nil {
		t.Fatalf("a closed gate ran a task: %v %v", ran, err)
	}
	if err := e.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	if c.get("a") != 0 {
		t.Fatal("RunNow ran a step through a closed gate")
	}
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Pending || got.Holder != "" {
		t.Fatalf("the task was claimed through a closed gate: %+v", got)
	}
	open.Store(true)
	if ran, err := e.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("an open gate must run the task: %v %v", ran, err)
	}
	if got, _ := e.Get(context.Background(), tk.ID); got.State != task.Done || c.get("a") != 1 || c.get("b") != 1 {
		t.Fatalf("after the gate opened: %+v", got)
	}
}

// A run stands down at the next step boundary when the gate closes (a server that is promoted as the map group's
// standby must not run a task of the old leader), records nothing and runs no hook; the task is claimed and
// resumed at the step it had reached once the gate is open again.
func TestARunStandsDownAtAStepBoundaryWhenTheGateCloses(t *testing.T) {
	c := &counter{n: map[string]int{}}
	var open atomic.Bool
	open.Store(true)
	hook := 0
	d := task.Definition{Kind: "test.kind", OnFail: func(context.Context, task.Task) { hook++ }}
	d.Steps = []task.Step{
		{Name: "a", Run: func(context.Context, *task.Env) error { c.inc("a"); open.Store(false); return nil }},
		{Name: "b", Run: func(context.Context, *task.Env) error { c.inc("b"); return nil }},
	}
	st := task.NewMemStore()
	e := gated(t, st, &open, d)
	tk, _ := e.Submit(context.Background(), "test.kind", task.Subject{Type: "database", ID: "shop"}, struct{}{}, "alice")
	if _, err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.get("a") != 1 || c.get("b") != 0 {
		t.Fatalf("a ran %d, b ran %d: the run must stop at the boundary", c.get("a"), c.get("b"))
	}
	got, _ := e.Get(context.Background(), tk.ID)
	if !got.State.Active() || hook != 0 {
		t.Fatalf("a stand-down must record nothing and run no hook: %+v hook=%d", got, hook)
	}
	open.Store(true)
	// A stood-down run is not finished: its lease lapses and the next claim resumes it at step b.
	if got.State != task.Running || got.DoneSteps() != 1 {
		t.Fatalf("the task must stay running with step a done: %+v", got)
	}
}

// The cancel guard: a kind can say that a task is not cancellable in its present state; the answer of the engine and
// the REST layer's task.Cancellable are the same.
func TestTheCancelGuardRefusesPastThePointOfNoReturn(t *testing.T) {
	var past atomic.Bool
	d := task.Definition{Kind: "test.guarded", Cancellable: func(task.Task) bool { return !past.Load() }}
	d.Steps = []task.Step{{Name: "a", Run: func(context.Context, *task.Env) error { return nil }}}
	var open atomic.Bool
	open.Store(true)
	st := task.NewMemStore()
	e := gated(t, st, &open, d)
	tk, err := e.Submit(context.Background(), "test.guarded", task.Subject{Type: "server", ID: "s1"}, struct{}{}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !task.Cancellable(tk) {
		t.Fatal("before the point of no return the task is cancellable")
	}
	past.Store(true)
	if task.Cancellable(tk) {
		t.Fatal("task.Cancellable must follow the guard")
	}
	if err := e.Cancel(context.Background(), tk.ID); !errors.Is(err, task.ErrNotCancellable) {
		t.Fatalf("Cancel past the point of no return: %v", err)
	}
	if got, _ := e.Get(context.Background(), tk.ID); got.CancelRequested {
		t.Fatal("a refused cancel must not be recorded")
	}
	past.Store(false)
	if err := e.Cancel(context.Background(), tk.ID); err != nil {
		t.Fatalf("Cancel before it: %v", err)
	}
}

// The failover class is exclusive in the memory store as in the database (migration 0011).
func TestMemStoreExcludesASecondFailoverClassTask(t *testing.T) {
	st := task.NewMemStore()
	mk := func(kind, subject string) error {
		_, err := st.Create(context.Background(), task.Task{Kind: kind, SubjectType: "server", SubjectID: subject, State: task.Pending})
		return err
	}
	if err := mk("server.failover", "s1"); err != nil {
		t.Fatal(err)
	}
	var x *task.ExcludedError
	if err := mk("replication.restorePreferredPrimaries", "cluster"); !errors.As(err, &x) || x.Problem().Code != "failover_in_progress" {
		t.Fatalf("a restore during a failover: %v", err)
	}
	if err := mk("server.failover", "s2"); !errors.As(err, &x) {
		t.Fatalf("a second failover: %v", err)
	}
	if err := mk("rebalance", "x"); !errors.As(err, &x) {
		t.Fatalf("a rebalance during a failover: %v", err)
	}
}
