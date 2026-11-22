// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package task_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// recoverDef is the four steps of the crash tests, with a Recover on the step that must not run twice.
func recoverDef(c *counter, rec func(ctx context.Context, env *task.Env) (bool, error)) task.Definition {
	d := defn(c, "a", "n", "c", "d")
	d.Steps[1].NonIdempotent = true
	d.Steps[1].Recover = rec
	return d
}

// TestRecoverOfANonIdempotentStep: a run that ended while a NonIdempotent step ran finds the step recorded as running. With a
// Recover the step says whether its effect is there: true records it as done without running it, false runs it (it provably did
// not happen), an error is the old failure step_interrupted.
func TestRecoverOfANonIdempotentStep(t *testing.T) {
	yes := func(context.Context, *task.Env) (bool, error) { return true, nil }
	no := func(context.Context, *task.Env) (bool, error) { return false, nil }
	for _, tc := range []struct {
		name      string
		point     string
		recoverFn func(context.Context, *task.Env) (bool, error)
		wantState task.State
		wantRuns  int // runs of n in all
		wantCode  string
	}{
		// the run ended after the effect: the step is not run again
		{"effect there", task.FaultStepRan, yes, task.Done, 1, ""},
		// the run ended before the effect: the step runs once, in the run that recovers
		{"effect not there", task.FaultStepStarted, no, task.Done, 1, ""},
		{"cannot tell", task.FaultStepRan, func(context.Context, *task.Env) (bool, error) { return false, errors.New("catalog unreadable") }, task.Failed, 1, task.CodeStepInterrupted},
		{"no recover", task.FaultStepRan, nil, task.Failed, 1, task.CodeStepInterrupted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &counter{n: map[string]int{}}
			clk := newClock()
			mem := task.NewMemStoreClock(clk.Now)
			ds := &deadStore{Store: mem}
			ctx := context.Background()
			hook := func(p string, _ int64, step int) {
				if p == tc.point && step == 1 {
					ds.dead.Store(true)
					panic(crashed{})
				}
			}
			mk := func() task.Definition { return recoverDef(c, tc.recoverFn) }
			a, err := task.NewEngine(ds, []task.Definition{mk()}, task.Options{Holder: "A", Lease: 30 * time.Second, Now: clk.Now, StoreTries: 1, Fault: hook})
			if err != nil {
				t.Fatal(err)
			}
			tk, _ := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
			func() {
				defer func() { _ = recover() }()
				_, _ = a.RunOnce(ctx)
			}()
			b := engine(t, mem, "B", clk, mk())
			clk.Add(31 * time.Second)
			if ran, err := b.RunOnce(ctx); !ran || err != nil {
				t.Fatalf("B: %v %v", ran, err)
			}
			got, _ := b.Get(ctx, tk.ID)
			if got.State != tc.wantState {
				t.Fatalf("state %s %+v, want %s", got.State, got.Error, tc.wantState)
			}
			if tc.wantCode != "" && (got.Error == nil || got.Error.Code != tc.wantCode) {
				t.Fatalf("error %+v, want code %s", got.Error, tc.wantCode)
			}
			// the step ran at most once in all (before the crash when its effect is there, after it when it is not)
			if n := c.get("n"); n != tc.wantRuns {
				t.Errorf("the non-idempotent step ran %d times, want %d", n, tc.wantRuns)
			}
			if tc.wantState == task.Done {
				for _, s := range []string{"a", "n", "c", "d"} {
					if c.get(s) < 1 {
						t.Errorf("step %s never ran", s)
					}
				}
				for i, st := range got.Steps {
					if st.State != task.StepDone {
						t.Errorf("step %d is %s", i, st.State)
					}
				}
			} else if c.get("c") != 0 {
				t.Errorf("a step after the interrupted one ran")
			}
		})
	}
}

// TestEnvCancelRequested: a long step asks whether a cancel was requested, which the engine looks at only between steps.
func TestEnvCancelRequested(t *testing.T) {
	clk := newClock()
	mem := task.NewMemStoreClock(clk.Now)
	ctx := context.Background()
	var seen []bool
	var eng *task.Engine
	var id int64
	d := task.Definition{Kind: "test.kind", Steps: []task.Step{
		{Name: "first", Run: func(context.Context, *task.Env) error { return nil }},
		{Name: "long", Run: func(ctx context.Context, env *task.Env) error {
			seen = append(seen, env.CancelRequested(ctx))
			if err := eng.Cancel(ctx, id); err != nil {
				return err
			}
			seen = append(seen, env.CancelRequested(ctx))
			return nil
		}},
		{Name: "last", Run: func(context.Context, *task.Env) error { return nil }},
	}}
	eng = engine(t, mem, "A", clk, d)
	tk, _ := eng.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	id = tk.ID
	if _, err := eng.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != "[false true]" {
		t.Errorf("CancelRequested before and after the request: %v", seen)
	}
	// the mark is followed at the boundary after the step
	got, _ := eng.Get(ctx, id)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled {
		t.Errorf("task = %s %+v", got.State, got.Error)
	}
}

// The tasks of the schemas, a failover and a rebalance exclude each other in the memory store as the trigger of migration 0041 does
// in the system database.
func TestSchemaTasksExcludeAFailoverAndARebalance(t *testing.T) {
	ctx := context.Background()
	mk := func(kinds ...string) *task.Engine {
		mem := task.NewMemStore()
		var defs []task.Definition
		for _, k := range kinds {
			defs = append(defs, task.Definition{Kind: k, Steps: []task.Step{{Name: "x", Run: func(context.Context, *task.Env) error { return nil }}}})
		}
		e, err := task.NewEngine(mem, defs, task.Options{Holder: "A", Lease: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	submit := func(e *task.Engine, kind string) error {
		_, err := e.Submit(ctx, kind, task.Subject{Type: "x", ID: kind}, nil, "u")
		return err
	}
	both := func(first, second string, excluded bool) {
		t.Helper()
		e := mk(first, second)
		if err := submit(e, first); err != nil {
			t.Fatal(err)
		}
		var x *task.ExcludedError
		err := submit(e, second)
		if excluded != errors.As(err, &x) {
			t.Errorf("%s while %s is unfinished: %v, excluded want %v", second, first, err, excluded)
		}
	}
	for _, k := range []string{"schema.distribute", "schema.move", "schema.undistribute"} {
		both("server.failover", k, true)
		both(k, "server.failover", true)
		both(k, "replication.restorePreferredPrimaries", true)
	}
	// a rebalance excludes the schema tasks that move a bucket, and only those
	for _, k := range []string{"schema.move", "schema.undistribute"} {
		both("rebalance", k, true)
		both(k, "rebalance", true)
	}
	// a change of the servers excludes the two that move a bucket, both ways, and not a distribution
	for _, s := range []string{"server.add", "server.remove", "server.leave"} {
		for _, k := range []string{"schema.move", "schema.undistribute"} {
			both(s, k, true)
			both(k, s, true)
		}
		both(s, "schema.distribute", false)
		both("schema.distribute", s, false)
	}
	both("rebalance", "schema.distribute", false)
	both("schema.distribute", "rebalance", false)
	// the text of the refusal names the schema task
	e := mk("schema.move", "server.failover")
	if err := submit(e, "schema.move"); err != nil {
		t.Fatal(err)
	}
	var x *task.ExcludedError
	if err := submit(e, "server.failover"); !errors.As(err, &x) || x.Problem().Code != "operation_in_progress" {
		t.Errorf("a failover while a schema move runs: %v", err)
	}
}

// A run that is shut down while its Recover reads the effect did not learn anything: the task is not failed with step_interrupted
// (a permanent verdict by a run that was going away); the step stays recorded as running and the next run asks again.
func TestRecoverCutOffByAShutdownDoesNotFailTheTask(t *testing.T) {
	c := &counter{n: map[string]int{}}
	clk := newClock()
	mem := task.NewMemStoreClock(clk.Now)
	ds := &deadStore{Store: mem}
	hook := func(p string, _ int64, step int) {
		if p == task.FaultStepRan && step == 1 {
			ds.dead.Store(true)
			panic(crashed{})
		}
	}
	first := func(context.Context, *task.Env) (bool, error) { return false, errors.New("unused") }
	a, err := task.NewEngine(ds, []task.Definition{recoverDef(c, first)}, task.Options{Holder: "A", Lease: 30 * time.Second, Now: clk.Now, StoreTries: 1, Fault: hook})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tk, _ := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	func() {
		defer func() { _ = recover() }()
		_, _ = a.RunOnce(ctx)
	}()
	// B is shut down in the middle of Recover
	shutdown, stop := context.WithCancel(ctx)
	cut := func(rctx context.Context, _ *task.Env) (bool, error) {
		stop()
		<-rctx.Done()
		return false, rctx.Err()
	}
	b := engine(t, mem, "B", clk, recoverDef(c, cut))
	clk.Add(31 * time.Second)
	_, _ = b.RunOnce(shutdown)
	got, _ := b.Get(ctx, tk.ID)
	if got.State == task.Failed {
		t.Fatalf("the task was failed by a run that was shut down: %+v", got.Error)
	}
	// the next run asks again, and this time the effect is there
	d := engine(t, mem, "D", clk, recoverDef(c, func(context.Context, *task.Env) (bool, error) { return true, nil }))
	clk.Add(31 * time.Second)
	if ran, err := d.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("D: %v %v", ran, err)
	}
	if got, _ = d.Get(ctx, tk.ID); got.State != task.Done {
		t.Fatalf("task after the next run: %s %+v", got.State, got.Error)
	}
	if n := c.get("n"); n != 1 {
		t.Errorf("the step ran %d times", n)
	}
}
