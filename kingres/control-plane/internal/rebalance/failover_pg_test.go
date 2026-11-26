// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package rebalance

import (
	"context"
	"testing"
	"time"
)

// RBR-06: what the failover needs from the rebalance. StopForFailover sets the flag of a stop with the
// stopped-by value "safetyRule"; WaitStopped says when nothing is pending or running any more, and does not say it
// before: a bound that ends gives false, so that the caller can go on asking and raise its alert.
func TestStopForFailoverSetsTheFlagAndWaitStoppedWaitsForThePause(t *testing.T) {
	e, lease := newGuardEnv(t) // a rebalance task, claimed (running)
	ctx := context.Background()
	s := &Service{Conns: e}
	if _, err := e.sys.Exec(ctx, `INSERT INTO kingres_cp.rebalance_run (task_id, options, started_at) VALUES ($1, '{}', clock_timestamp())`, e.id); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.StopForFailover(ctx, "s2"); err != nil || !ok {
		t.Fatalf("StopForFailover: %v %v", ok, err)
	}
	var stop bool
	var by string
	if err := e.sys.QueryRow(ctx, `SELECT stop_requested, coalesce(stopped_by, '') FROM kingres_cp.rebalance_run WHERE task_id = $1`, e.id).Scan(&stop, &by); err != nil || !stop || by != "safetyRule" {
		t.Fatalf("run: stop %v by %q (%v)", stop, by, err)
	}
	if ok, err := s.WaitStopped(ctx, 300*time.Millisecond); err != nil || ok {
		t.Fatalf("WaitStopped while the task runs: %v %v, want false after the bound", ok, err)
	}
	// The loop paused the task: the wait ends, and a second request has nothing to stop.
	if err := e.st.Pause(ctx, e.id, lease, "Stopped by safetyRule"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.WaitStopped(ctx, 5*time.Second); err != nil || !ok {
		t.Fatalf("WaitStopped after the pause: %v %v", ok, err)
	}
	if ok, err := s.StopForFailover(ctx, "s2"); err != nil || ok {
		t.Fatalf("StopForFailover of a paused rebalance: %v %v, want false", ok, err)
	}
}
