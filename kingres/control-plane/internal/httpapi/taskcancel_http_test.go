// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The answers of POST /tasks/{id}/cancel for the tasks that cannot be cancelled and for a cancel that lost every race, through the router.

// changing is a task store whose conditional cancel always finds the task in another state, as a task that pauses and resumes at every try.
type changing struct {
	*task.MemStore
	tries int
}

func (s *changing) RequestCancelIf(context.Context, int64, time.Time, task.State, *task.Error) error {
	s.tries++
	return task.ErrStateChanged
}

func cancelEnv(t *testing.T, st task.Store, guard func(task.Task) bool) (*env, *task.Engine) {
	t.Helper()
	o := &ops.Ops{Log: slog.New(slog.DiscardHandler)}
	e := newEnv(t, func(d *Deps) { d.Ops = o })
	e.initialize()
	def := task.Definition{Kind: "test.cancel", Cancellable: guard,
		Steps: []task.Step{{Name: "x", Run: func(context.Context, *task.Env) error { return nil }}}}
	eng, err := task.NewEngine(st, []task.Definition{def}, task.Options{Holder: "test", Logger: slog.New(slog.DiscardHandler), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	o.SetEngine(eng)
	t.Cleanup(o.Wait)
	return e, eng
}

func (e *env) cancelRequest(id int64) (int, map[string]any) {
	e.t.Helper()
	w := e.do(req{method: "POST", path: fmt.Sprintf("/api/v1/tasks/%d/cancel", id), bearer: e.token(auth.RoleAdmin), tls: true})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// M4: a cancel that is refused for five tries in a row answers 409 operation_in_progress (a code of the contract), not 500, and changes nothing.
func TestACancelThatLosesEveryRaceAnswers409(t *testing.T) {
	st := &changing{MemStore: task.NewMemStore()}
	e, eng := cancelEnv(t, st, nil)
	tk, err := eng.Submit(context.Background(), "test.cancel", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if err != nil {
		t.Fatal(err)
	}
	code, out := e.cancelRequest(tk.ID)
	if code != 409 || out["code"] != "operation_in_progress" || !strings.Contains(fmt.Sprint(out["detail"]), "changed") {
		t.Fatalf("%d %v", code, out)
	}
	if st.tries != 5 {
		t.Errorf("%d tries", st.tries)
	}
	if got, _ := eng.Get(context.Background(), tk.ID); got.State != task.Pending || got.Error != nil {
		t.Errorf("the task was changed: %s %+v", got.State, got.Error)
	}
}

// L2: the 409 of a cancel says which of the two it is: the task has finished, or it is past the point after which it cannot be cancelled.
func TestTheTextOfTheRefusedCancelFitsTheState(t *testing.T) {
	e, eng := cancelEnv(t, task.NewMemStore(), func(task.Task) bool { return false })
	ctx := context.Background()
	active, _ := eng.Submit(ctx, "test.cancel", task.Subject{Type: "x", ID: "1"}, nil, "u")
	e.mem.TaskList = append(e.mem.TaskList, api.Task{ID: active.ID, Kind: "test.cancel", State: "pending"})
	code, out := e.cancelRequest(active.ID)
	if code != 409 || out["code"] != "task_not_cancellable" || !strings.Contains(fmt.Sprint(out["detail"]), "past the point") {
		t.Errorf("an active task past its point: %d %v", code, out)
	}
	e.mem.TaskList[0].State = "paused"
	if code, out = e.cancelRequest(active.ID); code != 409 || !strings.Contains(fmt.Sprint(out["detail"]), "past the point") {
		t.Errorf("a paused task past its point: %d %v", code, out)
	}
	e.mem.TaskList[0].State = "done"
	if code, out = e.cancelRequest(active.ID); code != 409 || out["code"] != "task_not_cancellable" || !strings.Contains(fmt.Sprint(out["detail"]), "has finished") {
		t.Errorf("a finished task: %d %v", code, out)
	}
}

// L6: the read of a database.import says that every unfinished import can be cancelled, a running committed one included (the cancel then pauses it): the flag
// does not depend on the steps or on the mark, which the API does not show.
func TestTheReadOfADatabaseImportIsCancellableWhileItIsUnfinished(t *testing.T) {
	o := &ops.Ops{Log: slog.New(slog.DiscardHandler)}
	if _, err := task.NewEngine(task.NewMemStore(), o.Definitions(), task.Options{Holder: "A", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	steps := []api.TaskStep{{Name: "Set the sequences", State: task.StepDone}, {Name: "Verify", State: task.StepDone}, {Name: "Take the final name", State: task.StepRunning}}
	for _, state := range []string{"pending", "running", "paused"} {
		in := api.Task{Kind: "database.import", State: state, Cancellable: true, Progress: api.TaskProgress{Steps: steps}}
		if !withCancelGuard(in).Cancellable {
			t.Errorf("an import that is %s in its last step is not cancellable", state)
		}
	}
	for _, state := range []string{"done", "failed"} {
		in := api.Task{Kind: "database.import", State: state, Cancellable: false, Progress: api.TaskProgress{Steps: steps}}
		if withCancelGuard(in).Cancellable {
			t.Errorf("an import that is %s is cancellable", state)
		}
	}
}
