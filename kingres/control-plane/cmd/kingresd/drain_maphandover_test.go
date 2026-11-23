// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package main

import (
	"context"
	"errors"
	"testing"

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
