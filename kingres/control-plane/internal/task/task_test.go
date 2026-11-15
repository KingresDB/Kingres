// SPDX-License-Identifier: AGPL-3.0-only

package task_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// counter records how often each step ran.
type counter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *counter) inc(k string) { c.mu.Lock(); c.n[k]++; c.mu.Unlock() }
func (c *counter) get(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[k]
}

func defn(c *counter, names ...string) task.Definition {
	d := task.Definition{Kind: "test.kind"}
	for _, n := range names {
		d.Steps = append(d.Steps, task.Step{Name: n, Run: func(context.Context, *task.Env) error { c.inc(n); return nil }})
	}
	return d
}

func engine(t *testing.T, st task.Store, holder string, clk *clock, d ...task.Definition) *task.Engine {
	t.Helper()
	e, err := task.NewEngine(st, d, task.Options{Holder: holder, Lease: 30 * time.Second, Now: clk.Now, StoreTries: 1})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)} }

func TestStepsRunInOrderAndTaskFinishes(t *testing.T) {
	c := &counter{n: map[string]int{}}
	var order []string
	d := task.Definition{Kind: "test.kind"}
	for _, n := range []string{"a", "b", "c"} {
		d.Steps = append(d.Steps, task.Step{Name: n, Run: func(context.Context, *task.Env) error { order = append(order, n); c.inc(n); return nil }})
	}
	st := task.NewMemStore()
	e := engine(t, st, "h1", newClock(), d)
	tk, err := e.Submit(context.Background(), "test.kind", task.Subject{Type: "database", ID: "shop"}, map[string]string{"x": "y"}, "alice")
	if err != nil || tk.State != task.Pending {
		t.Fatalf("submit: %v %+v", err, tk)
	}
	if ran, err := e.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("RunOnce = %v, %v", ran, err)
	}
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Done || got.Percent != 100 || strings.Join(order, "") != "abc" {
		t.Errorf("state %s percent %v order %v", got.State, got.Percent, order)
	}
	for _, s := range got.Steps {
		if s.State != task.StepDone {
			t.Errorf("step %s is %s", s.Name, s.State)
		}
	}
	if ran, _ := e.RunOnce(context.Background()); ran {
		t.Error("a finished task ran again")
	}
}

// TestResumeAfterInterruptionAtEveryBoundary simulates a crash of the leader
// at every point where the store is written (before a step is marked running
// and before it is marked done) and checks that a second engine resumes at
// the first step not recorded as done, finishing with every step run at
// least once and each recorded-done step exactly once.
func TestResumeAfterInterruptionAtEveryBoundary(t *testing.T) {
	const steps = 4
	for _, point := range []string{"setstep:running", "setstep:done"} {
		for k := 0; k < steps; k++ {
			t.Run(point+"#"+string(rune('0'+k)), func(t *testing.T) {
				c := &counter{n: map[string]int{}}
				d := defn(c, "s0", "s1", "s2", "s3")
				clk := newClock()
				st := task.NewMemStoreClock(clk.Now)
				calls := 0
				crash := errors.New("simulated crash")
				st.FailOn(func(op string) error {
					if op == point {
						calls++
						if calls == k+1 {
							return crash
						}
					}
					return nil
				})
				a := engine(t, st, "A", clk, d)
				tk, _ := a.Submit(context.Background(), "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
				if _, err := a.RunOnce(context.Background()); !errors.Is(err, crash) {
					t.Fatalf("A: %v", err)
				}
				st.FailOn(nil)
				b := engine(t, st, "B", clk, d)
				if ran, _ := b.RunOnce(context.Background()); ran {
					t.Fatal("B took the task before the lease expired")
				}
				clk.Add(31 * time.Second)
				if ran, err := b.RunOnce(context.Background()); !ran || err != nil {
					t.Fatalf("B: %v %v", ran, err)
				}
				got, _ := b.Get(context.Background(), tk.ID)
				if got.State != task.Done {
					t.Fatalf("state %s", got.State)
				}
				for i := 0; i < steps; i++ {
					name := "s" + string(rune('0'+i))
					n := c.get(name)
					// The step interrupted at the crash may have run twice
					// (its effect happened, its completion was not recorded).
					max := 1
					if (point == "setstep:done" && i == k) || (point == "setstep:running" && false) {
						max = 2
					}
					if n < 1 || n > max {
						t.Errorf("step %s ran %d times (max %d)", name, n, max)
					}
				}
			})
		}
	}
}

func TestZombieLeaderCannotWrite(t *testing.T) {
	c := &counter{n: map[string]int{}}
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	ctx := context.Background()
	e := engine(t, st, "A", clk, defn(c, "only"))
	tk, _ := e.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	a, err := st.Claim(ctx, "A", 30*time.Second)
	if err != nil || a == nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	b, _ := st.Claim(ctx, "B", 30*time.Second)
	if b == nil {
		t.Fatal("takeover failed")
	}
	zombie := task.Lease{Holder: "A", Epoch: a.Epoch}
	if _, err := st.SetStep(ctx, tk.ID, zombie, 0, task.StepDone, 100); !errors.Is(err, task.ErrLeaseLost) {
		t.Errorf("zombie SetStep: %v", err)
	}
	if err := st.Finish(ctx, tk.ID, zombie, task.Done, nil); !errors.Is(err, task.ErrLeaseLost) {
		t.Errorf("zombie Finish: %v", err)
	}
	if err := st.Renew(ctx, tk.ID, zombie, time.Minute); !errors.Is(err, task.ErrLeaseLost) {
		t.Errorf("zombie Renew: %v", err)
	}
}

func TestStepFailureIsRecordedAndStopsTheTask(t *testing.T) {
	c := &counter{n: map[string]int{}}
	d := defn(c, "a", "b", "c")
	d.Steps[1].Run = func(context.Context, *task.Env) error { return errors.New("disk full") }
	st := task.NewMemStore()
	e := engine(t, st, "A", newClock(), d)
	tk, _ := e.Submit(context.Background(), "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if _, err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeStepFailed || got.Error.Step != "b" || !strings.Contains(got.Error.Message, "disk full") {
		t.Errorf("%+v %+v", got, got.Error)
	}
	if got.Steps[0].State != task.StepDone || got.Steps[1].State != task.StepFailed || got.Steps[2].State != task.StepPending || c.get("c") != 0 {
		t.Errorf("steps %+v", got.Steps)
	}
	// The subject is free again.
	if _, err := e.Submit(context.Background(), "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u"); err != nil {
		t.Errorf("resubmit after failure: %v", err)
	}
}

func TestCancellationBetweenSteps(t *testing.T) {
	c := &counter{n: map[string]int{}}
	d := defn(c, "a", "b", "c")
	st := task.NewMemStore()
	e := engine(t, st, "A", newClock(), d)
	tk, _ := e.Submit(context.Background(), "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	d.Steps[1].Run = nil
	// Cancel from inside step b: it finishes, c never starts.
	e2, _ := task.NewEngine(st, []task.Definition{{Kind: "test.kind", Steps: []task.Step{
		{Name: "a", Run: func(context.Context, *task.Env) error { c.inc("a"); return nil }},
		{Name: "b", Irreversible: true, Run: func(ctx context.Context, _ *task.Env) error {
			c.inc("b")
			return e.Cancel(ctx, tk.ID)
		}},
		{Name: "c", Run: func(context.Context, *task.Env) error { c.inc("c"); return nil }},
	}}}, task.Options{Holder: "A"})
	if _, err := e2.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := e2.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled || c.get("c") != 0 || c.get("b") != 1 {
		t.Errorf("%+v %+v c=%d", got, got.Error, c.get("c"))
	}
	if !strings.Contains(got.Error.Message, "irreversible") {
		t.Errorf("message does not mention irreversible steps: %s", got.Error.Message)
	}
	// A pending task is canceled directly; a finished one cannot be canceled.
	p, _ := e.Submit(context.Background(), "test.kind", task.Subject{Type: "x", ID: "2"}, nil, "u")
	if err := e.Cancel(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.Get(context.Background(), p.ID); g.State != task.Failed || g.Error.Code != task.CodeCanceled {
		t.Errorf("pending cancel: %+v", g)
	}
	if err := e.Cancel(context.Background(), p.ID); !errors.Is(err, task.ErrNotCancellable) {
		t.Errorf("second cancel: %v", err)
	}
	if err := e.Cancel(context.Background(), 999); !errors.Is(err, task.ErrNotFound) {
		t.Errorf("cancel of unknown: %v", err)
	}
}

func TestOneTaskPerSubject(t *testing.T) {
	c := &counter{n: map[string]int{}}
	st := task.NewMemStore()
	e := engine(t, st, "A", newClock(), defn(c, "a"))
	ctx := context.Background()
	if _, err := e.Submit(ctx, "test.kind", task.Subject{Type: "database", ID: "shop"}, nil, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(ctx, "test.kind", task.Subject{Type: "database", ID: "shop"}, nil, "u"); !errors.Is(err, task.ErrSubjectBusy) {
		t.Errorf("second task for one subject: %v", err)
	}
	if _, err := e.Submit(ctx, "test.kind", task.Subject{Type: "database", ID: "other"}, nil, "u"); err != nil {
		t.Errorf("other subject refused: %v", err)
	}
	if _, err := e.Submit(ctx, "unknown", task.Subject{Type: "a", ID: "b"}, nil, "u"); !errors.Is(err, task.ErrUnknownKind) {
		t.Errorf("unknown kind: %v", err)
	}
	// Finish the first one: the subject is free again.
	for i := 0; i < 2; i++ {
		_, _ = e.RunOnce(ctx)
	}
	if _, err := e.Submit(ctx, "test.kind", task.Subject{Type: "database", ID: "shop"}, nil, "u"); err != nil {
		t.Errorf("subject still busy after the task finished: %v", err)
	}
}

func TestSecretsReachOnlyTheRunThatHasThem(t *testing.T) {
	const marker = "SECRET-MARKER-9f3a"
	var seen string
	d := task.Definition{Kind: "test.kind", Steps: []task.Step{
		{Name: "plain", Run: func(context.Context, *task.Env) error { return nil }},
		{Name: "needs", NeedsSecret: "password", Run: func(_ context.Context, env *task.Env) error {
			seen, _ = env.Secret("password")
			return nil
		}},
	}}
	st := task.NewMemStore()
	e := engine(t, st, "A", newClock(), d)
	ctx := context.Background()
	tk, _ := e.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, map[string]string{"user": "admin"}, "u")
	if err := e.RunNow(ctx, tk.ID, map[string]string{"password": marker}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Done || seen != marker {
		t.Fatalf("state %s seen %q", got.State, seen)
	}
	if strings.Contains(string(got.Input), marker) || strings.Contains(got.Kind+got.CreatedBy+got.SubjectID, marker) {
		t.Error("secret reached the stored task")
	}
	// Without the secret (a resume after a restart) the task fails cleanly.
	tk2, _ := e.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "2"}, nil, "u")
	_, _ = e.RunOnce(ctx)
	got2, _ := e.Get(ctx, tk2.ID)
	if got2.State != task.Failed || got2.Error.Code != task.CodeSecretUnavailable || got2.Steps[0].State != task.StepDone {
		t.Errorf("%+v %+v", got2, got2.Error)
	}
}

func TestEngineValidation(t *testing.T) {
	st := task.NewMemStore()
	if _, err := task.NewEngine(st, nil, task.Options{}); err == nil {
		t.Error("engine without holder")
	}
	if _, err := task.NewEngine(st, []task.Definition{{Kind: "k"}}, task.Options{Holder: "h"}); err == nil {
		t.Error("definition without steps")
	}
	d := defn(&counter{n: map[string]int{}}, "a")
	if _, err := task.NewEngine(st, []task.Definition{d, d}, task.Options{Holder: "h"}); err == nil {
		t.Error("duplicate kind")
	}
}
