// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"context"
	"strconv"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
)

// ManagedStandbyInstanceBase is added to the id of a standby kingresd manages to
// make its instanceId in the replication view: such a standby is not in the
// sharding catalog (kingres_shard 0.2 has no standby nodes), so it has no node id,
// and the offset keeps the number away from the node ids of the catalog. When the
// catalog can hold standbys the real node id replaces it.
const ManagedStandbyInstanceBase = 1 << 20

// addManagedStandbys adds the standbys kingresd manages to a group of the
// replication view: their state, the synchronous state and the lag the primary
// reports for the walsender whose application name is the standby's. A group with
// a standby that does not stream is degraded.
func addManagedStandbys(ctx context.Context, sg *api.ShardGroup, g int32, rd primaryReading, src ha.Standbys) {
	list, err := src.List(ctx)
	if err != nil {
		return
	}
	used := map[int]bool{}
	for _, sb := range list {
		if sb.Group != g {
			continue
		}
		m := api.ReplicationMember{
			InstanceID: int32(ManagedStandbyInstanceBase + sb.ID), ServerID: sb.ServerID, Role: "standby",
			State: "starting", Port: sb.Port, Timeline: 1,
		}
		if sg.Primary != nil {
			m.Timeline = sg.Primary.Timeline
		}
		if sb.State == "running" {
			m.State = "running"
		}
		streaming := false
		for i, w := range rd.senders {
			if used[i] || w.AppName != "kingres_standby_"+strconv.FormatInt(sb.ID, 10) {
				continue
			}
			used[i] = true
			streaming = w.State == "streaming"
			switch w.SyncState {
			case "async", "potential", "sync", "quorum":
				ss := w.SyncState
				m.SyncState = &ss
			}
			m.LagBytes, m.LagSeconds = w.LagBytes, w.LagSeconds
			break
		}
		if sg.Primary != nil && !streaming && sg.Health == "healthy" {
			sg.Health = "degraded"
		}
		sg.Standbys = append(sg.Standbys, m)
	}
}
