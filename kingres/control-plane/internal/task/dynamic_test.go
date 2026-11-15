// SPDX-License-Identifier: PostgreSQL

package task_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func TestDynamicStepsOnePerServerAndDetail(t *testing.T) {
	clk := newClock()
	c := &counter{n: map[string]int{}}
	def := task.Definition{Kind: "dyn", Dynamic: func(in json.RawMessage) ([]task.Step, error) {
		var p struct{ Servers []string }
		if err := json.Unmarshal(in, &p); err != nil {
			return nil, err
		}
		steps := []task.Step{{Name: "local", Run: func(context.Context, *task.Env) error { c.inc("local"); return nil }}}
		for _, s := range p.Servers {
			steps = append(steps, task.Step{Name: "on " + s, Run: func(ctx context.Context, env *task.Env) error {
				c.inc(s)
				return env.SetDetail(ctx, "waiting for "+s)
			}})
		}
		return steps, nil
	}}
	st := task.NewMemStore()
	e := engine(t, st, "h1", clk, def)
	tk, err := e.Submit(context.Background(), "dyn", task.Subject{Type: "x", ID: "1"}, map[string]any{"Servers": []string{"a", "b"}}, "me")
	if err != nil {
		t.Fatal(err)
	}
	if len(tk.Steps) != 3 || tk.Steps[2].Name != "on b" {
		t.Fatalf("steps %v", tk.Steps)
	}
	if err := e.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Done || c.get("a") != 1 || c.get("b") != 1 || got.Detail != "waiting for b" {
		t.Errorf("state %s counts %v detail %q", got.State, c.n, got.Detail)
	}
}

func TestOnFailRunsForFailedTasksOnly(t *testing.T) {
	clk := newClock()
	var failed []int64
	boom := errors.New("boom")
	mk := func(kind string, err error) task.Definition {
		return task.Definition{Kind: kind, Steps: []task.Step{{Name: "s", Run: func(context.Context, *task.Env) error { return err }}},
			OnFail: func(_ context.Context, t task.Task) { failed = append(failed, t.ID) }}
	}
	e := engine(t, task.NewMemStore(), "h1", clk, mk("ok", nil), mk("bad", boom))
	a, _ := e.Submit(context.Background(), "ok", task.Subject{Type: "x", ID: "1"}, nil, "me")
	b, _ := e.Submit(context.Background(), "bad", task.Subject{Type: "x", ID: "2"}, nil, "me")
	_ = e.RunNow(context.Background(), a.ID, nil)
	_ = e.RunNow(context.Background(), b.ID, nil)
	if len(failed) != 1 || failed[0] != b.ID {
		t.Errorf("OnFail ran for %v, want only %d", failed, b.ID)
	}
}

func TestRegisterAddsADefinitionLater(t *testing.T) {
	e := engine(t, task.NewMemStore(), "h1", newClock(), defn(&counter{n: map[string]int{}}, "x"))
	if err := e.Register(task.Definition{Kind: "late", Steps: []task.Step{{Name: "s", Run: func(context.Context, *task.Env) error { return nil }}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Register(task.Definition{Kind: "late", Steps: []task.Step{{Name: "s"}}}); err == nil {
		t.Error("a duplicate definition was accepted")
	}
	if _, err := e.Submit(context.Background(), "late", task.Subject{Type: "x", ID: "1"}, nil, "me"); err != nil {
		t.Error(err)
	}
}

func TestSubmitHeldIsNeverClaimedByAPollerWithoutItsSecret(t *testing.T) {
	st := task.NewMemStore()
	def := task.Definition{Kind: "secret", Steps: []task.Step{{Name: "needs", NeedsSecret: "pw", Run: func(context.Context, *task.Env) error { return nil }}}}
	e, err := task.NewEngine(st, []task.Definition{def}, task.Options{Holder: "h", Poll: time.Millisecond, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx) // a poller that claims every pending task it sees
	for i := 0; i < 200; i++ {
		tk, run, err := e.SubmitHeld(ctx, "secret", task.Subject{Type: "x", ID: fmt.Sprint(i)}, nil, "me")
		if err != nil {
			t.Fatal(err)
		}
		if tk.State != task.Running {
			t.Fatalf("held task state %s", tk.State)
		}
		if err := run(ctx, map[string]string{"pw": "x"}); err != nil {
			t.Fatal(err)
		}
		got, _ := e.Get(ctx, tk.ID)
		if got.State != task.Done {
			t.Fatalf("task %d ended %s: %v (a poller ran it without the secret)", i, got.State, got.Error)
		}
	}
}
