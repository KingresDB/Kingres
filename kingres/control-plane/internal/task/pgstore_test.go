// SPDX-License-Identifier: AGPL-3.0-only

package task_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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
	claimed, err := st.Claim(ctx, "A", time.Hour)
	if err != nil || claimed == nil || claimed.ID != tk.ID || claimed.Epoch != 1 {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if _, err := st.SetStep(ctx, tk.ID, task.Lease{Holder: "A", Epoch: 1}, 0, task.StepDone, 33); err != nil {
		t.Fatal(err)
	}
	b := engine(t, st, "B", clk, d)
	if ran, _ := b.RunOnce(ctx); ran {
		t.Error("B took a task with a live lease")
	}
	expireLease(t, conn, tk.ID) // the database's clock decides, so the test moves the expiry, not a clock
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
	if _, err := st.SetStep(ctx, tk.ID, task.Lease{Holder: "A", Epoch: 1}, 1, task.StepDone, 66); !errors.Is(err, task.ErrLeaseLost) {
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

// expireLease makes the lease of a task end now on the database's clock.
func expireLease(t *testing.T, db interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}, id int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `UPDATE kingres_cp.task SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// TestPGStoreLeaseUsesTheDatabaseClock is CP2-06 against the real store: an engine
// whose own clock is five minutes ahead cannot take a lease that is alive on the
// database's clock, the epoch grows with every claim, and a step is not marked
// running on an expired lease.
func TestPGStoreLeaseUsesTheDatabaseClock(t *testing.T) {
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
	skewed := newClock()
	skewed.Add(5 * time.Minute)
	c := &counter{n: map[string]int{}}
	d := defn(c, "a")
	a := engine(t, st, "A", clk, d)
	b := engine(t, st, "B", skewed, d)
	tk, err := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.Claim(ctx, "A", time.Hour)
	if err != nil || first == nil || first.Epoch != 1 {
		t.Fatalf("claim: %+v %v", first, err)
	}
	if ran, err := b.RunOnce(ctx); ran || err != nil {
		t.Fatalf("B ran=%v err=%v: the lease is alive on the database's clock", ran, err)
	}
	expireLease(t, conn, tk.ID)
	zombie := task.Lease{Holder: "A", Epoch: first.Epoch}
	if _, err := st.SetStep(ctx, tk.ID, zombie, 0, task.StepRunning, 0); !errors.Is(err, task.ErrLeaseLost) {
		t.Errorf("running mark on an expired lease: %v", err)
	}
	second, err := st.Claim(ctx, "A", time.Hour)
	if err != nil || second == nil || second.Epoch != 2 {
		t.Fatalf("second claim: %+v %v", second, err)
	}
	if _, err := st.SetStep(ctx, tk.ID, zombie, 0, task.StepDone, 100); !errors.Is(err, task.ErrLeaseLost) {
		t.Errorf("write of the first run after the same holder claimed again: %v", err)
	}
	if err := st.Renew(ctx, tk.ID, task.Lease{Holder: "A", Epoch: 2}, time.Hour); err != nil {
		t.Errorf("renew of the current run: %v", err)
	}
}
