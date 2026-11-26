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

// The mark of a task is written by a step under its lease, read back, kept across runs, and shown to the hook of the kind as the store has it (the copy of the claim
// does not have it).
func markScenario(t *testing.T, st task.Store, clk *clock) {
	t.Helper()
	ctx := context.Background()
	var hookMark, secondRunMark string
	runs := 0
	var stop context.CancelFunc
	d := task.Definition{Kind: "test.mark", Steps: []task.Step{{Name: "decide", Run: func(ctx context.Context, env *task.Env) error {
		runs++
		if runs == 1 {
			if env.Task.Mark != "" {
				t.Errorf("a new task has the mark %q", env.Task.Mark)
			}
			if err := env.SetMark(ctx, "decided"); err != nil {
				return err
			}
			if got, err := env.ReadMark(ctx); err != nil || got != "decided" {
				t.Errorf("read back: %q %v", got, err)
			}
			stop() // the process stops here
			return ctx.Err()
		}
		secondRunMark = env.Task.Mark
		return errors.New("fails after the mark")
	}}}, OnFail: func(ctx context.Context, tk task.Task) { hookMark = tk.Mark }}
	e1 := engine(t, st, "A", clk, d)
	tk, err := e1.Submit(ctx, "test.mark", task.Subject{Type: "x", ID: "y"}, nil, "u")
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	stop = cancel
	defer cancel()
	_ = e1.RunNow(runCtx, tk.ID, nil)
	if got, _ := st.Get(ctx, tk.ID); got.Mark != "decided" {
		t.Fatalf("the mark in the store after the first run: %q", got.Mark)
	}
	// a run that does not hold the lease cannot write a mark
	if ms, ok := st.(task.MarkStore); !ok {
		t.Fatal("the store has no marks")
	} else if err := ms.SetMark(ctx, tk.ID, task.Lease{Holder: "intruder", Epoch: 99}, "forged"); err == nil {
		t.Error("a mark was written without the lease")
	}
	if got, _ := st.Get(ctx, tk.ID); got.Mark != "decided" {
		t.Errorf("the mark after the refused write: %q", got.Mark)
	}
	// the next run sees it, and so does the hook
	expire(t, st, clk, tk.ID)
	e2 := engine(t, st, "B", clk, d)
	if ran, err := e2.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("the second run: %v %v", ran, err)
	}
	if secondRunMark != "decided" || hookMark != "decided" {
		t.Errorf("second run saw %q, hook saw %q", secondRunMark, hookMark)
	}
}

func TestMarkIsKeptAndShownToTheHookMemStore(t *testing.T) {
	clk := newClock()
	markScenario(t, task.NewMemStoreClock(clk.Now), clk)
}

func TestMarkIsKeptAndShownToTheHookPGStore(t *testing.T) {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	conn := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	pgExpire = func(t *testing.T, id int64) { expireLease(t, conn, id) }
	defer func() { pgExpire = nil }()
	markScenario(t, task.NewPGStore(conn), newClock())
}

var pgExpire func(t *testing.T, id int64)

// expire lets the lease of the first run run out (the database's clock decides for the PostgreSQL store).
func expire(t *testing.T, st task.Store, clk *clock, id int64) {
	t.Helper()
	if pgExpire != nil {
		pgExpire(t, id)
		return
	}
	clk.Add(2 * time.Minute)
}
