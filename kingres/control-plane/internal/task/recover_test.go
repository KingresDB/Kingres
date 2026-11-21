// SPDX-License-Identifier: AGPL-3.0-only

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
	for _, tc := range []struct {
		name      string
		point     string
		effect    bool // the effect of step n is there (the run ended after it)
		recoverFn func(effect bool) func(context.Context, *task.Env) (bool, error)
		wantState task.State
		wantRuns  int // runs of n in all
		wantCode  string
	}{
		{"effect there", task.FaultStepRan, true, func(e bool) func(context.Context, *task.Env) (bool, error) {
			return func(context.Context, *task.Env) (bool, error) { return e, nil }
		}, task.Done, 1, ""},
		{"effect not there", task.FaultStepStarted, false, func(e bool) func(context.Context, *task.Env) (bool, error) {
			return func(context.Context, *task.Env) (bool, error) { return e, nil }
		}, task.Done, 1, ""},
		{"cannot tell", task.FaultStepRan, true, func(bool) func(context.Context, *task.Env) (bool, error) {
			return func(context.Context, *task.Env) (bool, error) { return false, errors.New("catalog unreadable") }
		}, task.Failed, 1, task.CodeStepInterrupted},
		{"no recover", task.FaultStepRan, true, func(bool) func(context.Context, *task.Env) (bool, error) { return nil }, task.Failed, 1, task.CodeStepInterrupted},
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
			mk := func() task.Definition { return recoverDef(c, tc.recoverFn(tc.effect)) }
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
			if n := c.get("n"); n > tc.wantRuns {
				t.Errorf("the non-idempotent step ran %d times", n)
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

// TestEnvItemsAndCancelRequest: a long step records how far it is and asks whether a cancel was requested, which the engine
// looks at only between steps.
func TestEnvItemsAndCancelRequest(t *testing.T) {
	clk := newClock()
	mem := task.NewMemStoreClock(clk.Now)
	ctx := context.Background()
	var seen []bool
	var eng *task.Engine
	var id int64
	d := task.Definition{Kind: "test.kind", Steps: []task.Step{
		{Name: "first", Run: func(context.Context, *task.Env) error { return nil }},
		{Name: "long", Run: func(ctx context.Context, env *task.Env) error {
			if err := env.SetItems(ctx, 10, 100); err != nil {
				return err
			}
			seen = append(seen, env.CancelRequested(ctx))
			if err := eng.Cancel(ctx, id); err != nil {
				return err
			}
			seen = append(seen, env.CancelRequested(ctx))
			return env.SetItems(ctx, 40, 100)
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
	done, total, ok := mem.StepItems(id, 1)
	if !ok || done != 40 || total != 100 {
		t.Errorf("items of step 1 = %d of %d (%v), want 40 of 100", done, total, ok)
	}
	if _, _, ok := mem.StepItems(id, 0); ok {
		t.Errorf("step 0 recorded items")
	}
	// the mark is followed at the boundary after the step
	got, _ := eng.Get(ctx, id)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled {
		t.Errorf("task = %s %+v", got.State, got.Error)
	}
}

// The tasks of the schemas and a failover exclude each other in the memory store as the trigger of migration 0050 does in the
// system database.
func TestSchemaTasksAndAFailoverExcludeEachOther(t *testing.T) {
	ctx := context.Background()
	mk := func(kinds ...string) (*task.Engine, *task.MemStore) {
		mem := task.NewMemStore()
		var defs []task.Definition
		for _, k := range kinds {
			defs = append(defs, task.Definition{Kind: k, Steps: []task.Step{{Name: "x", Run: func(context.Context, *task.Env) error { return nil }}}})
		}
		e, err := task.NewEngine(mem, defs, task.Options{Holder: "A", Lease: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return e, mem
	}
	for _, k := range []string{"schema.distribute", "schema.move", "schema.undistribute"} {
		e, _ := mk(k, "server.failover")
		if _, err := e.Submit(ctx, "server.failover", task.Subject{Type: "server", ID: "a"}, nil, "u"); err != nil {
			t.Fatal(err)
		}
		var x *task.ExcludedError
		if _, err := e.Submit(ctx, k, task.Subject{Type: "schema", ID: "db/s"}, nil, "u"); !errors.As(err, &x) {
			t.Errorf("%s while a failover is unfinished: %v, want an ExcludedError", k, err)
		}
		e2, _ := mk(k, "server.failover")
		if _, err := e2.Submit(ctx, k, task.Subject{Type: "schema", ID: "db/s"}, nil, "u"); err != nil {
			t.Fatal(err)
		}
		if _, err := e2.Submit(ctx, "server.failover", task.Subject{Type: "server", ID: "a"}, nil, "u"); !errors.As(err, &x) {
			t.Errorf("a failover while %s is unfinished: %v, want an ExcludedError", k, err)
		}
	}
}
