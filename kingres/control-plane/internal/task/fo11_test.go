// SPDX-License-Identifier: AGPL-3.0-only

package task_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

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
