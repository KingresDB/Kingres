// SPDX-License-Identifier: PostgreSQL

package task_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

var errDead = errors.New("process is dead")

// deadStore stops every store write once the simulated process has crashed,
// so nothing the engine does after the crash point can be recorded.
type deadStore struct {
	task.Store
	dead atomic.Bool
}

func (d *deadStore) check() error {
	if d.dead.Load() {
		return errDead
	}
	return nil
}

func (d *deadStore) Renew(ctx context.Context, id int64, h string, now time.Time, l time.Duration) error {
	if err := d.check(); err != nil {
		return err
	}
	return d.Store.Renew(ctx, id, h, now, l)
}

func (d *deadStore) SetStep(ctx context.Context, id int64, h string, i int, s string, p float64, now time.Time) (bool, error) {
	if err := d.check(); err != nil {
		return false, err
	}
	return d.Store.SetStep(ctx, id, h, i, s, p, now)
}

func (d *deadStore) Finish(ctx context.Context, id int64, h string, s task.State, e *task.Error, now time.Time) error {
	if err := d.check(); err != nil {
		return err
	}
	return d.Store.Finish(ctx, id, h, s, e, now)
}

type crashed struct{}

// crashDef is a four-step task: a, n (not idempotent), c, d.
func crashDef(c *counter) task.Definition {
	d := defn(c, "a", "n", "c", "d")
	d.Steps[1].NonIdempotent = true
	return d
}

// TestCrashAtEveryStepBoundary kills the engine at each named crash point of
// each step and checks what the next engine does: it resumes at the first step
// not recorded as done; a step that ran but was not recorded runs again unless
// it is NonIdempotent, in which case the task fails with step_interrupted; no
// step runs twice once it was recorded as done; the task never stays running.
func TestCrashAtEveryStepBoundary(t *testing.T) {
	names := []string{"a", "n", "c", "d"}
	for _, point := range []string{task.FaultBeforeStep, task.FaultStepStarted, task.FaultStepRan, task.FaultStepRecorded} {
		for k := 0; k < 4; k++ {
			t.Run(fmt.Sprintf("%s#%d", point, k), func(t *testing.T) {
				c := &counter{n: map[string]int{}}
				mem := task.NewMemStore()
				ds := &deadStore{Store: mem}
				clk := newClock()
				ctx := context.Background()
				hook := func(p string, _ int64, step int) {
					if p == point && step == k {
						ds.dead.Store(true)
						panic(crashed{})
					}
				}
				a, err := task.NewEngine(ds, []task.Definition{crashDef(c)}, task.Options{Holder: "A", Lease: 30 * time.Second, Now: clk.Now, StoreTries: 1, Fault: hook})
				if err != nil {
					t.Fatal(err)
				}
				tk, _ := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
				func() {
					defer func() {
						if r := recover(); r != nil {
							if _, ok := r.(crashed); !ok {
								panic(r)
							}
						}
					}()
					_, _ = a.RunOnce(ctx)
				}()
				if !ds.dead.Load() {
					t.Fatal("the crash point was never reached")
				}
				mid, _ := mem.Get(ctx, tk.ID)
				if mid.State != task.Running {
					t.Fatalf("after the crash the row is %s, want running with a lease", mid.State)
				}
				b := engine(t, mem, "B", clk, crashDef(c))
				if ran, _ := b.RunOnce(ctx); ran {
					t.Fatal("B took the task before the lease expired")
				}
				clk.Add(31 * time.Second)
				if ran, err := b.RunOnce(ctx); !ran || err != nil {
					t.Fatalf("B: %v %v", ran, err)
				}
				got, _ := b.Get(ctx, tk.ID)
				if got.State != task.Done && got.State != task.Failed {
					t.Fatalf("task left %s", got.State)
				}
				interrupted := k == 1 && (point == task.FaultStepStarted || point == task.FaultStepRan)
				if interrupted {
					if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeStepInterrupted || got.Error.Step != "n" || got.Error.Message == "" {
						t.Fatalf("want a clean step_interrupted failure, got %s %+v", got.State, got.Error)
					}
					if c.get("c") != 0 || c.get("d") != 0 {
						t.Errorf("steps after the interrupted one ran: c=%d d=%d", c.get("c"), c.get("d"))
					}
				} else if got.State != task.Done {
					t.Fatalf("want done, got %s %+v", got.State, got.Error)
				}
				if c.get("n") > 1 {
					t.Errorf("the non-idempotent step ran %d times", c.get("n"))
				}
				for i, n := range names {
					max := 1
					if point == task.FaultStepRan && i == k {
						max = 2 // effect happened, record did not: at-least-once
					}
					if c.get(n) > max {
						t.Errorf("step %s ran %d times (max %d)", n, c.get(n), max)
					}
					if !interrupted && c.get(n) < 1 {
						t.Errorf("step %s never ran", n)
					}
				}
			})
		}
	}
}

// TestCrashAtTheFinish covers the two points around recording the end.
func TestCrashAtTheFinish(t *testing.T) {
	for _, point := range []string{task.FaultBeforeFinish, task.FaultAfterFinish} {
		t.Run(point, func(t *testing.T) {
			c := &counter{n: map[string]int{}}
			mem := task.NewMemStore()
			ds := &deadStore{Store: mem}
			clk := newClock()
			ctx := context.Background()
			hook := func(p string, _ int64, _ int) {
				if p == point {
					ds.dead.Store(true)
					panic(crashed{})
				}
			}
			a, _ := task.NewEngine(ds, []task.Definition{crashDef(c)}, task.Options{Holder: "A", Lease: 30 * time.Second, Now: clk.Now, StoreTries: 1, Fault: hook})
			tk, _ := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
			func() {
				defer func() { _ = recover() }()
				_, _ = a.RunOnce(ctx)
			}()
			if point == task.FaultAfterFinish {
				// The record of the end was written before the crash.
				ds.dead.Store(false)
			}
			b := engine(t, mem, "B", clk, crashDef(c))
			clk.Add(31 * time.Second)
			ran, err := b.RunOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if wantRan := point == task.FaultBeforeFinish; ran != wantRan {
				t.Errorf("B ran a task: %v, want %v", ran, wantRan)
			}
			got, _ := b.Get(ctx, tk.ID)
			if got.State != task.Done {
				t.Fatalf("state %s", got.State)
			}
			for _, n := range []string{"a", "n", "c", "d"} {
				if c.get(n) != 1 {
					t.Errorf("step %s ran %d times", n, c.get(n))
				}
			}
		})
	}
}

// TestCrashAtTheLeaseRenewal kills the process while a step runs, at the
// renewal tick of its lease.
func TestCrashAtTheLeaseRenewal(t *testing.T) {
	for _, k := range []int{0, 1} {
		t.Run(fmt.Sprintf("step%d", k), func(t *testing.T) {
			c := &counter{n: map[string]int{}}
			mem := task.NewMemStore()
			ds := &deadStore{Store: mem}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			d := crashDef(c)
			name := d.Steps[k].Name
			d.Steps[k].Run = func(ctx context.Context, _ *task.Env) error {
				c.inc(name)
				<-ctx.Done() // a long step: the renewal tick fires meanwhile
				return ctx.Err()
			}
			hook := func(p string, _ int64, _ int) {
				if p == task.FaultLeaseRenew {
					ds.dead.Store(true)
					stop()
				}
			}
			a, _ := task.NewEngine(ds, []task.Definition{d}, task.Options{Holder: "A", Lease: 60 * time.Millisecond, StoreTries: 1, Fault: hook})
			tk, _ := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
			_, _ = a.RunOnce(ctx)
			if !ds.dead.Load() {
				t.Fatal("no lease renewal happened")
			}
			time.Sleep(150 * time.Millisecond) // the lease expires
			d2 := crashDef(c)
			b, _ := task.NewEngine(mem, []task.Definition{d2}, task.Options{Holder: "B", Lease: time.Minute, StoreTries: 1})
			if ran, err := b.RunOnce(context.Background()); !ran || err != nil {
				t.Fatalf("B: %v %v", ran, err)
			}
			got, _ := b.Get(context.Background(), tk.ID)
			if k == 1 {
				if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeStepInterrupted || c.get("n") != 1 {
					t.Fatalf("%s %+v n=%d", got.State, got.Error, c.get("n"))
				}
			} else if got.State != task.Done || c.get("a") != 2 || c.get("n") != 1 {
				t.Fatalf("%s %+v a=%d n=%d", got.State, got.Error, c.get("a"), c.get("n"))
			}
		})
	}
}

func TestFaultPointsAreListed(t *testing.T) {
	if len(task.FaultPoints) != 7 {
		t.Errorf("fault points: %v", task.FaultPoints)
	}
}
