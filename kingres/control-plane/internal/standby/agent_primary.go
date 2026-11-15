// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
)

// syncNamesKey is the setting this agent owns in the managed configuration of the
// primary instance. It lives in kingres-managed.conf (not ALTER SYSTEM) so that
// every setting kingresd writes stays in one file that a restart reads.
const syncNamesKey = "synchronous_standby_names"

const slotPrefix = "kingres_standby_"

type slotRow struct {
	name     string
	active   bool
	retained int64
	lost     bool
}

// maintainPrimary is the primary's side of the group, run by the server that runs
// the primary: slots exist for the standbys of the plan and for no others, a slot
// that retains too much WAL raises an alert and then is dropped, a standby whose
// slot is lost is marked for a rebuild, and synchronous_standby_names follows the
// rule of SyncState.Decide.
func (a *Agent) maintainPrimary(ctx context.Context) error {
	g, ok := a.localGroup(ctx)
	if !ok {
		return nil
	}
	st := a.local.snapshot()
	entries := st.entriesOfGroup(g)
	c, err := a.Svc.ServiceConn(ctx, "postgres")
	if err != nil {
		return fmt.Errorf("connecting to the primary instance: %w", err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var recovery bool
	if err := c.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&recovery); err != nil || recovery {
		return err // not a primary (after a failover in a later phase)
	}

	streaming := map[string]bool{}
	rows, err := c.Query(ctx, `SELECT application_name, state FROM pg_stat_replication`)
	if err != nil {
		return fmt.Errorf("reading pg_stat_replication: %w", err)
	}
	for rows.Next() {
		var app, state string
		if err := rows.Scan(&app, &state); err != nil {
			rows.Close()
			return err
		}
		if state == "streaming" {
			streaming[app] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	slots := map[string]slotRow{}
	rows, err = c.Query(ctx, `SELECT slot_name, active, coalesce(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn), 0)::bigint,
			coalesce(wal_status, '') = 'lost'
		FROM pg_replication_slots WHERE slot_type = 'physical' AND slot_name LIKE 'kingres\_standby\_%'`)
	if err != nil {
		return fmt.Errorf("reading pg_replication_slots: %w", err)
	}
	for rows.Next() {
		var s slotRow
		if err := rows.Scan(&s.name, &s.active, &s.retained, &s.lost); err != nil {
			rows.Close()
			return err
		}
		slots[s.name] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	planned := map[string]bool{}
	for _, e := range entries {
		planned[SlotName(e.ID)] = true
	}
	// A slot that belongs to no standby of the plan would hold WAL for nothing.
	for name, s := range slots {
		if !planned[name] && !s.active {
			if err := a.dropSlot(ctx, c, name); err == nil {
				delete(slots, name)
				a.audit(ctx, "replicationSlotDropped", strings.TrimPrefix(name, slotPrefix), audit.OutcomeSuccess, map[string]any{"group": g, "reason": "not in the plan"})
			}
		}
	}

	ps := &PrimaryStatus{Group: g, Streaming: []int64{}, Slots: []SlotStatus{}, NeedsRebuild: []int64{}}
	warned := map[int64]int64{}
	needs := map[int64]bool{}
	var names []string
	streamingIDs := map[int64]bool{}
	for _, e := range entries {
		name := SlotName(e.ID)
		s, have := slots[name]
		if dg, dropped := st.Dropped[e.ID]; dropped && dg >= e.Generation {
			needs[e.ID] = true
			continue
		}
		if !e.Ready {
			continue // being created: its slot is the creator's business
		}
		if !have || s.lost {
			// A slot that is gone cannot be made again: the new one would start at the
			// current position and the standby needs older WAL. It is made again from a
			// base backup.
			if err := a.local.markDropped(e); err == nil {
				needs[e.ID] = true
				a.audit(ctx, "replicationSlotDropped", strconv.FormatInt(e.ID, 10), audit.OutcomeSuccess, map[string]any{"group": g, "reason": "the slot was lost"})
			}
			continue
		}
		ps.Slots = append(ps.Slots, SlotStatus{ID: e.ID, RetainedBytes: s.retained, Active: s.active})
		switch SlotPolicy(s.retained, a.SlotWarn, a.SlotLimit) {
		case SlotWarn:
			warned[e.ID] = s.retained
		case SlotDrop:
			if err := a.dropSlot(ctx, c, name); err != nil {
				a.Log.Warn("standby: dropping a slot over the retention limit", "standby", e.ID, "error", err)
				warned[e.ID] = s.retained
				break
			}
			if err := a.local.markDropped(e); err == nil {
				needs[e.ID] = true
			}
			a.audit(ctx, "replicationSlotDropped", strconv.FormatInt(e.ID, 10), audit.OutcomeSuccess,
				map[string]any{"group": g, "reason": "retention limit exceeded", "retainedBytes": s.retained})
			continue
		}
		names = append(names, AppName(e.ID))
		if streaming[AppName(e.ID)] {
			streamingIDs[e.ID] = true
			ps.Streaming = append(ps.Streaming, e.ID)
		}
	}
	ps.NeedsRebuild = sortedIDs(needs)

	// synchronous_standby_names.
	settings, err := a.mgr().Settings(cluster.NodeID)
	if err != nil {
		return fmt.Errorf("reading the managed settings: %w", err)
	}
	applied := settings.Extra[syncNamesKey]
	a.mu.Lock()
	ss := a.syncs[g]
	if ss == nil {
		ss = &SyncState{}
		a.syncs[g] = ss
	}
	frozen := a.Frozen != nil && a.Frozen(g)
	d := ss.Decide(SyncInput{Now: a.Clock.Mono(), Planned: names, Streaming: streaming, Applied: applied, Frozen: frozen, Grace: a.Grace})
	a.mu.Unlock()
	if d.Set != nil {
		next := settings
		next.Extra = map[string]string{}
		for k, v := range settings.Extra {
			next.Extra[k] = v
		}
		if *d.Set == "" {
			delete(next.Extra, syncNamesKey)
		} else {
			next.Extra[syncNamesKey] = *d.Set
		}
		if err := a.mgr().WriteManaged(cluster.NodeID, next); err != nil {
			return fmt.Errorf("writing synchronous_standby_names: %w", err)
		}
		if err := a.mgr().Reload(ctx, cluster.NodeID); err != nil {
			return fmt.Errorf("reloading the primary instance: %w", err)
		}
		applied = *d.Set
		a.audit(ctx, "synchronousStandbyNamesChanged", strconv.Itoa(int(g)), audit.OutcomeSuccess,
			map[string]any{"group": g, "synchronousStandbyNames": applied, "withoutSynchronousStandby": d.WithoutSynchronousStandby})
	}
	ps.SynchronousStandbyNames, ps.Without = applied, d.WithoutSynchronousStandby
	a.mu.Lock()
	a.primary, a.slotWarn = ps, warned
	a.mu.Unlock()
	return nil
}

// Probe returns the alert conditions of this server's side of its group, for the
// monitor: groupWithoutSynchronousStandby while the group runs without a
// synchronous standby, and replicationLagHigh for a standby whose slot retains
// more WAL than the warning limit. Only the kinds in the second result were
// evaluated (and may be cleared); nothing is evaluated before the first round of
// maintainPrimary.
func (a *Agent) Probe(_ context.Context, _ time.Time) ([]monitor.Condition, map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.primary == nil {
		return nil, nil
	}
	var conds []monitor.Condition
	g := a.primary.Group
	if a.primary.Without {
		conds = append(conds, monitor.Condition{Kind: "groupWithoutSynchronousStandby", Severity: "warning",
			Subject: fmt.Sprintf("server:%s|group:%d", a.Self, g),
			Message: fmt.Sprintf("Shard group %d is running without a synchronous standby: its standbys have not streamed for longer than the grace period, so commits are acknowledged by the primary on server %s alone and a failure of that server now loses them.", g, a.Self)})
	}
	for id, bytesRetained := range a.slotWarn {
		conds = append(conds, monitor.Condition{Kind: "replicationLagHigh", Severity: "warning",
			Subject: fmt.Sprintf("server:%s|group:%d|standby:%d", a.Self, g, id),
			Message: fmt.Sprintf("The slot of standby %d of shard group %d retains %d bytes of WAL on server %s; it is dropped at the retention limit and the standby is then rebuilt.", id, g, bytesRetained, a.Self)})
	}
	return conds, map[string]bool{"groupWithoutSynchronousStandby": true, "replicationLagHigh": true}
}
