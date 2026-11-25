// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package main

import (
	"context"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/rebalance"
)

// wireMapGroupHandOver connects step 4 of the drain (design 11.3) to the planned switchover: the drain asks for a graceful failover of
// shard group 0 on the server that runs its primary, to a standby that the failover service chooses on another server. The call
// returns once the failover task exists; the drain is paused meanwhile and the watch of the new manager leader resumes it
// (rebalance/handover.go). It refuses while any failover task is unfinished: Start would otherwise resume a paused failover of this
// server (an unrelated one, waiting for an operator) instead of making the switchover.
//
// A test can hold the request at the fault point "drain:handover" (mode "hold") after the marker says requesting, to end the server there.
func wireMapGroupHandOver(rbs *rebalance.Service, fos *failover.Service, fh *fault.Hook) {
	rbs.HandOverMapGroup = handOverHook(fos.Unfinished, func(ctx context.Context, actor, server string) error {
		if err := holdAtFault(ctx, fh, "drain:handover"); err != nil {
			return err
		}
		_, _, err := fos.Start(ctx, actor, server, failover.Request{Mode: failover.ModeGraceful, ShardGroupIDs: []int32{0},
			Comment: "hand over the map group: drain of server " + server})
		return err
	})
}

// handOverHook is the hook with its two parts given: whether a failover is unfinished, and the start of the switchover.
func handOverHook(unfinished func(context.Context) (bool, error), start func(ctx context.Context, actor, server string) error) func(ctx context.Context, actor, server string) error {
	return func(ctx context.Context, actor, server string) error {
		busy, err := unfinished(ctx)
		if err != nil {
			return problem.New(problem.ServiceUnavailable, "The failover tasks cannot be read now.").WithRetryAfter(5)
		}
		if busy {
			return problem.New(problem.FailoverInProgress, "A failover task is unfinished (running or waiting for an operator); the map group is not handed over while it is.")
		}
		return start(ctx, actor, server)
	}
}

// holdAtFault waits while the fault file asks to hold at the point (mode "hold"), and returns when the fault is cleared or ctx ends.
func holdAtFault(ctx context.Context, fh *fault.Hook, point string) error {
	for fh.FailoverInjection(point) == "hold" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}
