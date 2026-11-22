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

// failingGet is a store whose Get can be made to fail.
type failingGet struct {
	task.Store
	fail atomic.Bool
}

func (f *failingGet) Get(ctx context.Context, id int64) (task.Task, error) {
	if f.fail.Load() {
		return task.Task{}, errors.New("the store cannot be read")
	}
	return f.Store.Get(ctx, id)
}

// FO-11: a cancel is never recorded without the guard of the kind having been asked: when the task cannot be read
// the request is refused and nothing is marked.
func TestACancelIsNotRecordedWhenTheGuardCannotBeAsked(t *testing.T) {
	var past atomic.Bool
	past.Store(true)
	d := task.Definition{Kind: "test.guarded", Cancellable: func(task.Task) bool { return !past.Load() }}
	d.Steps = []task.Step{{Name: "a", Run: func(context.Context, *task.Env) error { return nil }}}
	var open atomic.Bool
	open.Store(true)
	st := &failingGet{Store: task.NewMemStore()}
	e := gated(t, st, &open, d)
	tk, err := e.Submit(context.Background(), "test.guarded", task.Subject{Type: "server", ID: "s1"}, struct{}{}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	st.fail.Store(true)
	if err := e.Cancel(context.Background(), tk.ID); err == nil {
		t.Fatal("a cancel was accepted although the guard could not be asked")
	}
	st.fail.Store(false)
	if got, _ := e.Get(context.Background(), tk.ID); got.CancelRequested {
		t.Fatal("the refused cancel was recorded")
	}
}

// FO-11: the guard accepted the request and the run passed the point of no return before the boundary: the mark is
// ignored there and the task goes on.
func TestARunIgnoresACancelMarkPastThePointOfNoReturn(t *testing.T) {
	var past atomic.Bool
	var e *task.Engine
	var id atomic.Int64
	ran := map[string]bool{}
	d := task.Definition{Kind: "test.guarded", Cancellable: func(task.Task) bool { return !past.Load() }}
	d.Steps = []task.Step{
		{Name: "a", Run: func(ctx context.Context, _ *task.Env) error {
			if err := e.Cancel(ctx, id.Load()); err != nil { // accepted: the point of no return is not passed yet
				return err
			}
			past.Store(true)
			ran["a"] = true
			return nil
		}},
		{Name: "b", Run: func(context.Context, *task.Env) error { ran["b"] = true; return nil }},
	}
	var open atomic.Bool
	open.Store(true)
	e = gated(t, task.NewMemStore(), &open, d)
	tk, err := e.Submit(context.Background(), "test.guarded", task.Subject{Type: "server", ID: "s1"}, struct{}{}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	id.Store(tk.ID)
	if _, err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Done || !ran["b"] {
		t.Fatalf("a mark past the point of no return stopped the task: %+v ran %v", got, ran)
	}
}

// F4-08. A cancel ends an interruptible step while it runs, within the poll of the engine and not after the step's own bound; a step that is
// not interruptible is not touched; a cancel the guard refuses is ignored.
func TestACancelEndsAnInterruptibleStepWhileItRuns(t *testing.T) {
	started := make(chan struct{}, 1)
	var hooks atomic.Int32
	d := task.Definition{Kind: "test.interruptible", Cancellable: func(task.Task) bool { return true },
		OnFail: func(context.Context, task.Task) { hooks.Add(1) }}
	d.Steps = []task.Step{{Name: "wait", Interruptible: true, Run: func(ctx context.Context, _ *task.Env) error {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Minute):
			return errors.New("the step ran to its own bound")
		}
	}}, {Name: "after", Run: func(context.Context, *task.Env) error { t.Error("a step after the cancel ran"); return nil }}}
	st := task.NewMemStore()
	e, err := task.NewEngine(st, []task.Definition{d}, task.Options{Holder: "A", CancelPoll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := e.Submit(context.Background(), "test.interruptible", task.Subject{Type: "x", ID: "1"}, struct{}{}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, rerr := e.RunOnce(context.Background()); done <- rerr }()
	<-started
	begin := time.Now()
	if err := e.Cancel(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case rerr := <-done:
		if rerr != nil {
			t.Fatal(rerr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancel did not end the running step")
	}
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled || hooks.Load() != 1 || time.Since(begin) > 5*time.Second {
		t.Fatalf("%+v %+v hooks %d after %v", got, got.Error, hooks.Load(), time.Since(begin))
	}
	if got.Steps[0].State != task.StepPending {
		t.Errorf("the interrupted step is %q, want pending", got.Steps[0].State)
	}
}

func TestACancelDoesNotEndAStepThatIsNotInterruptible(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	d := task.Definition{Kind: "test.plain", Cancellable: func(task.Task) bool { return true }}
	d.Steps = []task.Step{{Name: "work", Run: func(ctx context.Context, _ *task.Env) error {
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}}
	e, err := task.NewEngine(task.NewMemStore(), []task.Definition{d}, task.Options{Holder: "A", CancelPoll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := e.Submit(context.Background(), "test.plain", task.Subject{Type: "x", ID: "1"}, struct{}{}, "alice")
	done := make(chan struct{})
	go func() { _, _ = e.RunOnce(context.Background()); close(done) }()
	<-started
	if err := e.Cancel(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("a step that is not interruptible was ended by the cancel")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	<-done
}
