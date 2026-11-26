// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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

func running(kind, subj string) task.Task {
	return task.Task{Kind: kind, State: task.Running, SubjectType: "instance", SubjectID: subj, CreatedBy: "me",
		Steps: []task.StepState{{Name: "step", State: task.StepRunning}}}
}

func recordEngine(t *testing.T) *task.Engine {
	t.Helper()
	return engine(t, task.NewMemStore(), "h1", newClock(), task.Definition{Kind: "k", Steps: []task.Step{{Name: "s", Run: func(context.Context, *task.Env) error { return nil }}}})
}

// A recorded task is written as it is, the subject stays busy until it is
// settled, and settling writes the end.
func TestRecordSettleAndSubjectBusy(t *testing.T) {
	e := recordEngine(t)
	ctx := context.Background()
	got, err := e.Record(ctx, running("instance.stop", "1"))
	if err != nil || got.ID == 0 || got.State != task.Running {
		t.Fatalf("record: %+v %v", got, err)
	}
	if _, err := e.Record(ctx, running("instance.start", "1")); !errors.Is(err, task.ErrSubjectBusy) {
		t.Errorf("a second active task of the subject: %v", err)
	}
	got.State, got.Percent, got.Steps = task.Done, 100, []task.StepState{{Name: "step", State: task.StepDone}}
	if err := e.Settle(ctx, got); err != nil {
		t.Fatal(err)
	}
	back, _ := e.Get(ctx, got.ID)
	if back.State != task.Done || back.Percent != 100 || back.Steps[0].State != task.StepDone {
		t.Errorf("settled: %+v", back)
	}
	// The subject is free again; the end can be corrected (a stop that failed).
	got.State, got.Error = task.Failed, &task.Error{Code: task.CodeStepFailed, Message: "did not stop"}
	if err := e.Settle(ctx, got); err != nil {
		t.Fatal(err)
	}
	if back, _ := e.Get(ctx, got.ID); back.State != task.Failed || back.Error == nil {
		t.Errorf("corrected: %+v", back)
	}
	if _, err := e.Record(ctx, running("instance.start", "1")); err != nil {
		t.Errorf("the subject must be free after the end: %v", err)
	}
	if err := e.Settle(ctx, task.Task{ID: 999, State: task.Done}); !errors.Is(err, task.ErrNotFound) {
		t.Errorf("settling an unknown task: %v", err)
	}
}

// A task announced with an id (reserved, or far above the sequence) is written
// under it, also when it is already finished, and writing it twice is harmless.
func TestRecordFinishedUnderTheAnnouncedID(t *testing.T) {
	e := recordEngine(t)
	ctx := context.Background()
	id := e.ReserveID(ctx)
	if id == 0 {
		t.Fatal("no id reserved")
	}
	now := time.Now().UTC()
	in := running("instance.start", "1")
	in.ID, in.State, in.Percent, in.CreatedAt, in.UpdatedAt = id, task.Done, 100, now, now
	in.Steps = []task.StepState{{Name: "Start the instance", State: task.StepDone}}
	for i := 0; i < 2; i++ {
		if _, err := e.Record(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.Get(ctx, id)
	if err != nil || got.Kind != "instance.start" || got.State != task.Done {
		t.Fatalf("recorded: %+v %v", got, err)
	}
	tk, _ := e.Submit(ctx, "k", task.Subject{Type: "x", ID: "1"}, nil, "me")
	if tk.ID == id {
		t.Errorf("id %d reused", id)
	}
}

// The PostgreSQL store records, settles, reserves ids and imports under an
// announced id like the memory store, and a recorded task whose owner died is
// claimed by the engine once its lease ran out.
func TestPGStoreRecordSettleReserveAndOrphans(t *testing.T) {
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
	interrupted := task.Definition{Kind: "instance.stop", Steps: []task.Step{{Name: "Stop the instance", Run: func(context.Context, *task.Env) error {
		return errors.New("the server ended before the operation finished")
	}}}}
	e := engine(t, st, "A", clk, interrupted)
	rec, err := e.Record(ctx, running("instance.stop", "7"))
	if err != nil || rec.ID == 0 || rec.State != task.Running || rec.CreatedBy != "me" {
		t.Fatalf("record: %+v %v", rec, err)
	}
	if _, err := e.Record(ctx, running("instance.start", "7")); !errors.Is(err, task.ErrSubjectBusy) {
		t.Errorf("busy: %v", err)
	}
	if ran, _ := e.RunOnce(ctx); ran {
		t.Error("the engine took a recorded task before its lease ran out")
	}
	// Settle it done, then corrected to failed (what a stop that did not stop does).
	done := rec
	done.State, done.Percent, done.Steps = task.Done, 100, []task.StepState{{Name: "step", State: task.StepDone}}
	if err := e.Settle(ctx, done); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(ctx, rec.ID); got.State != task.Done || got.Percent != 100 || got.Holder != "" {
		t.Errorf("settled: %+v", got)
	}
	done.State, done.Error = task.Failed, &task.Error{Code: task.CodeStepFailed, Message: "did not stop", Retryable: true}
	if err := e.Settle(ctx, done); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(ctx, rec.ID); got.State != task.Failed || got.Error == nil || got.Error.Message != "did not stop" {
		t.Errorf("corrected: %+v", got)
	}
	// A reserved id is used by a task recorded later, also already finished; the
	// sequence does not give it out again.
	id := e.ReserveID(ctx)
	if id == 0 {
		t.Fatal("no id reserved")
	}
	now := time.Now().UTC()
	late := running("instance.start", "7")
	late.ID, late.State, late.Percent, late.CreatedAt, late.UpdatedAt = id, task.Done, 100, now, now
	late.Steps = []task.StepState{{Name: "Start the instance", State: task.StepDone}}
	for i := 0; i < 2; i++ {
		if _, err := e.Record(ctx, late); err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}
	if got, err := e.Get(ctx, id); err != nil || got.Kind != "instance.start" || got.State != task.Done {
		t.Fatalf("imported: %+v %v", got, err)
	}
	if next, _ := e.Submit(ctx, "instance.stop", task.Subject{Type: "instance", ID: "8"}, nil, "me"); next.ID <= id {
		t.Errorf("the sequence gave out %d after %d was reserved", next.ID, id)
	}
	// An orphan: recorded as running, never settled; after the lease the engine fails it.
	orphan, err := e.Record(ctx, running("instance.stop", "9"))
	if err != nil {
		t.Fatal(err)
	}
	expireLease(t, conn, orphan.ID)
	for {
		ran, rerr := e.RunOnce(ctx)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if !ran {
			break
		}
	}
	got, _ := e.Get(ctx, orphan.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Step != "Stop the instance" {
		t.Errorf("orphan: %+v", got)
	}
}
