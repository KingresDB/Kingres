// SPDX-License-Identifier: PostgreSQL

package task_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func TestPGStoreRunsTasksLikeTheMemStore(t *testing.T) {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	conn := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	st := task.NewPGStore(conn)
	clk := newClock()
	c := &counter{n: map[string]int{}}
	d := defn(c, "a", "b", "c")

	a := engine(t, st, "A", clk, d)
	tk, err := a.Submit(ctx, "test.kind", task.Subject{Type: "database", ID: "shop"}, map[string]int{"n": 1}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Submit(ctx, "test.kind", task.Subject{Type: "database", ID: "shop"}, nil, "bob"); !errors.Is(err, task.ErrSubjectBusy) {
		t.Errorf("second task for the subject: %v", err)
	}
	// Take it with A, let the lease expire, B takes over and finishes it.
	claimed, err := st.Claim(ctx, "A", clk.Now(), 30*time.Second)
	if err != nil || claimed == nil || claimed.ID != tk.ID {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if _, err := st.SetStep(ctx, tk.ID, "A", 0, task.StepDone, 33, clk.Now()); err != nil {
		t.Fatal(err)
	}
	b := engine(t, st, "B", clk, d)
	if ran, _ := b.RunOnce(ctx); ran {
		t.Error("B took a task with a live lease")
	}
	clk.Add(time.Minute)
	if ran, err := b.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("B: %v %v", ran, err)
	}
	if c.get("a") != 0 || c.get("b") != 1 || c.get("c") != 1 {
		t.Errorf("B ran a=%d b=%d c=%d; step a was already recorded as done", c.get("a"), c.get("b"), c.get("c"))
	}
	got, err := b.Get(ctx, tk.ID)
	if err != nil || got.State != task.Done || got.Percent != 100 || got.CreatedBy != "alice" || got.DoneSteps() != 3 {
		t.Errorf("%+v %v", got, err)
	}
	if _, err := st.SetStep(ctx, tk.ID, "A", 1, task.StepDone, 66, clk.Now()); !errors.Is(err, task.ErrLeaseLost) {
		t.Errorf("zombie write: %v", err)
	}
	// Cancel a pending task; a failed step is recorded.
	p, _ := a.Submit(ctx, "test.kind", task.Subject{Type: "database", ID: "shop"}, nil, "u")
	if err := a.Cancel(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if g, _ := a.Get(ctx, p.ID); g.State != task.Failed || g.Error == nil || g.Error.Code != task.CodeCanceled {
		t.Errorf("cancel: %+v", g)
	}
	if err := a.Cancel(ctx, p.ID); !errors.Is(err, task.ErrNotCancellable) {
		t.Errorf("cancel again: %v", err)
	}
	if _, err := a.Get(ctx, 12345); !errors.Is(err, task.ErrNotFound) {
		t.Errorf("unknown task: %v", err)
	}
	if l, err := st.List(ctx, 10); err != nil || len(l) != 2 || l[0].ID < l[1].ID {
		t.Errorf("list: %v %v", l, err)
	}
}
