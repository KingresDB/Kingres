// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// walSender is one row of pg_stat_replication.
type walSender struct {
	AppName    string
	ClientAddr string
	State      string
	SyncState  string
	LagBytes   *int64
	LagSeconds *float64
}

// primaryReading is what is read from the primary of one group.
type primaryReading struct {
	senders  []walSender
	slots    []api.ReplicationSlot
	timeline int
	ok       bool
}

// matches tells whether a walsender belongs to the standby node. The agent
// that creates standbys is specified to set application_name to
// "kingres_node_<node_id>"; the node id alone and the node's host as client
// address are accepted as well.
func (w walSender) matches(n node) bool {
	id := strconv.Itoa(int(n.ID))
	switch w.AppName {
	case "kingres_node_" + id, "node_" + id, "node" + id, id:
		return true
	}
	return w.ClientAddr != "" && w.ClientAddr == n.Host
}

// readPrimary queries pg_stat_replication and pg_replication_slots on the
// primary instance. Failure to reach it is not an error of the whole
// topology: the group is then reported as degraded.
func (s *Store) readPrimary(ctx context.Context, n node) primaryReading {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := s.connectInstance(ctx, n.Host, n.Port)
	if err != nil {
		s.opts.Logger.Warn("cannot reach primary for replication status",
			"node_id", n.ID, "host", n.Host, "port", n.Port, "error", err)
		return primaryReading{timeline: 1}
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	r := primaryReading{timeline: 1, ok: true}
	rows, err := conn.Query(ctx, `SELECT application_name, coalesce(client_addr::text, ''), coalesce(state, ''),
			coalesce(sync_state, ''), pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)::bigint,
			extract(epoch FROM replay_lag)::float8
		FROM pg_stat_replication ORDER BY application_name`)
	if err == nil {
		r.senders, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (walSender, error) {
			var w walSender
			err := row.Scan(&w.AppName, &w.ClientAddr, &w.State, &w.SyncState, &w.LagBytes, &w.LagSeconds)
			return w, err
		})
	}
	if err != nil {
		s.opts.Logger.Warn("reading pg_stat_replication failed", "node_id", n.ID, "error", err)
		r.ok = false
	}
	srows, err := conn.Query(ctx, `SELECT slot_name, slot_type, active,
			coalesce(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn), 0)::bigint
		FROM pg_replication_slots ORDER BY slot_name`)
	if err == nil {
		r.slots, err = pgx.CollectRows(srows, func(row pgx.CollectableRow) (api.ReplicationSlot, error) {
			sl := api.ReplicationSlot{InstanceID: n.ID}
			err := row.Scan(&sl.Name, &sl.Type, &sl.Active, &sl.RetainedWalBytes)
			return sl, err
		})
	}
	if err != nil {
		s.opts.Logger.Warn("reading pg_replication_slots failed", "node_id", n.ID, "error", err)
	}
	// pg_control_checkpoint needs privileges that a monitoring role may lack.
	var tl int
	if err := conn.QueryRow(ctx, "SELECT timeline_id FROM pg_control_checkpoint()").Scan(&tl); err == nil && tl > 0 {
		r.timeline = tl
	}
	return r
}

// ReplicationTopology implements store.Store.
func (s *Store) ReplicationTopology(ctx context.Context) (api.ReplicationTopology, error) {
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return api.ReplicationTopology{}, err
	}
	groupIDs := make([]int32, 0, len(cat.groups))
	for g := range cat.groups {
		groupIDs = append(groupIDs, g)
	}
	sort.Slice(groupIDs, func(i, j int) bool { return groupIDs[i] < groupIDs[j] })

	readings := make([]primaryReading, len(groupIDs))
	var wg sync.WaitGroup
	for i, g := range groupIDs {
		pr, ok := cat.activePrimary(g)
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			readings[i] = s.readPrimary(ctx, pr)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return api.ReplicationTopology{}, fmt.Errorf("reading replication topology: %w", err)
	}

	topo := api.ReplicationTopology{GeneratedAt: s.now().UTC(), Groups: make([]api.ShardGroup, 0, len(groupIDs))}
	for i, g := range groupIDs {
		sg := buildGroup(cat, g, readings[i])
		addManagedStandbys(ctx, &sg, g, readings[i], s.opts.HA.Standbys())
		sg.Lease = leaseView(s.opts.HA, g)
		topo.Groups = append(topo.Groups, sg)
	}
	return topo, nil
}

// buildGroup assembles one shard group of the topology.
func buildGroup(cat *catalog, g int32, rd primaryReading) api.ShardGroup {
	nodes := cat.groups[g]
	group := api.ShardGroup{
		ID: g, IsMapGroup: g == 0, Health: "healthy",
		Standbys: []api.ReplicationMember{}, Slots: []api.ReplicationSlot{},
		PreferredPrimaryServerID: cat.byNode[nodes[0].ID].ID,
	}
	primary, hasPrimary := cat.activePrimary(g)
	if hasPrimary {
		m := api.ReplicationMember{
			InstanceID: primary.ID, ServerID: cat.byNode[primary.ID].ID, Role: "primary",
			State: "running", Port: primary.Port, Timeline: rd.timeline,
			IsOnPreferredServer: cat.byNode[primary.ID].ID == group.PreferredPrimaryServerID,
		}
		group.Primary = &m
		if rd.slots != nil {
			group.Slots = rd.slots
		}
	} else {
		group.Health = "unavailable"
	}
	used := map[int]bool{}
	for _, n := range nodes {
		if n.Role != "standby" {
			continue
		}
		m := api.ReplicationMember{
			InstanceID: n.ID, ServerID: cat.byNode[n.ID].ID, Role: "standby",
			State: "stopped", Port: n.Port, Timeline: rd.timeline,
		}
		if n.Active {
			m.State = "running"
			streaming := false
			for i, w := range rd.senders {
				if used[i] || !w.matches(n) {
					continue
				}
				used[i] = true
				streaming = w.State == "streaming"
				if w.SyncState == "async" || w.SyncState == "potential" || w.SyncState == "sync" || w.SyncState == "quorum" {
					ss := w.SyncState
					m.SyncState = &ss
				}
				m.LagBytes = w.LagBytes
				m.LagSeconds = w.LagSeconds
				break
			}
			if hasPrimary && !streaming && group.Health == "healthy" {
				group.Health = "degraded"
			}
		} else if group.Health == "healthy" {
			group.Health = "degraded"
		}
		group.Standbys = append(group.Standbys, m)
	}
	if hasPrimary && !rd.ok && group.Health == "healthy" {
		group.Health = "degraded"
	}
	return group
}
