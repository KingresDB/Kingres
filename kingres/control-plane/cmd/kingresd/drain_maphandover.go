// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package main

import (
	"context"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/rebalance"
)

// wireMapGroupHandOver connects step 4 of the drain (design 11.3) to the planned switchover: the drain asks for a graceful failover of
// shard group 0 on the server that runs its primary, to a standby that the failover service chooses on another server. The call
// returns once the failover task exists; the drain is paused meanwhile and the watch of the new manager leader resumes it
// (rebalance/handover.go).
func wireMapGroupHandOver(rbs *rebalance.Service, fos *failover.Service) {
	rbs.HandOverMapGroup = func(ctx context.Context, actor, server string) error {
		_, _, err := fos.Start(ctx, actor, server, failover.Request{Mode: failover.ModeGraceful, ShardGroupIDs: []int32{0},
			Comment: "hand over the map group: drain of server " + server})
		return err
	}
}
