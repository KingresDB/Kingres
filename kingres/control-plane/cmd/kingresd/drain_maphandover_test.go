// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// L2. The hand-over of the map group never starts while a failover task is unfinished (Start would resume a paused failover of the
// server instead of making the switchover), and says why.
func TestTheHandOverHookRefusesWhileAFailoverIsUnfinished(t *testing.T) {
	ctx := context.Background()
	started := 0
	start := func(context.Context, string, string) error { started++; return nil }
	var p *problem.Problem

	busy := handOverHook(func(context.Context) (bool, error) { return true, nil }, start)
	if err := busy(ctx, "admin", "a"); !errors.As(err, &p) || p.Code != problem.FailoverInProgress || started != 0 {
		t.Fatalf("a failover is unfinished: %v (started %d)", err, started)
	}
	unreadable := handOverHook(func(context.Context) (bool, error) { return false, errors.New("no database") }, start)
	if err := unreadable(ctx, "admin", "a"); !errors.As(err, &p) || p.Code != problem.ServiceUnavailable || started != 0 {
		t.Fatalf("the failover tasks cannot be read: %v (started %d)", err, started)
	}
	free := handOverHook(func(context.Context) (bool, error) { return false, nil }, start)
	if err := free(ctx, "admin", "a"); err != nil || started != 1 {
		t.Fatalf("nothing unfinished: %v (started %d)", err, started)
	}
	failing := handOverHook(func(context.Context) (bool, error) { return false, nil },
		func(context.Context, string, string) error { return problem.New(problem.NoReplicaAvailable, "none") })
	if err := failing(ctx, "admin", "a"); !errors.As(err, &p) || p.Code != problem.NoReplicaAvailable {
		t.Fatalf("the refusal of the switchover is passed on: %v", err)
	}
}

// The fault point holds the request while the fault file says hold, lets go when it is cleared, ignores another point, and ends with the context.
func TestTheHoldAtAFaultPointEndsWhenTheFaultIsCleared(t *testing.T) {
	dir := t.TempDir()
	fh := fault.New(dir)
	write := func(s fault.Spec) {
		b, _ := json.Marshal(s)
		tmp := filepath.Join(dir, "tmp")
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, fault.FileName)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if err := holdAtFault(ctx, fh, "drain:handover"); err != nil {
		t.Fatalf("no fault: %v", err)
	}
	write(fault.Spec{FailoverPoint: "another:point", FailoverMode: "hold"})
	if err := holdAtFault(ctx, fh, "drain:handover"); err != nil {
		t.Fatalf("a hold at another point: %v", err)
	}
	write(fault.Spec{FailoverPoint: "drain:handover", FailoverMode: "hold"})
	done := make(chan error, 1)
	go func() { done <- holdAtFault(ctx, fh, "drain:handover") }()
	select {
	case err := <-done:
		t.Fatalf("the hold ended while the fault is in force: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	write(fault.Spec{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the hold ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hold did not end when the fault was cleared")
	}
	write(fault.Spec{FailoverPoint: "drain:handover", FailoverMode: "hold"})
	cctx, cancel := context.WithCancel(ctx)
	go func() { done <- holdAtFault(cctx, fh, "drain:handover") }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the hold ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hold did not end with the context")
	}
}
