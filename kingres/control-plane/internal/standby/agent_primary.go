// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
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
func (a *Agent) maintainPrimaries(ctx context.Context) error {
	primaries := a.localPrimaries()
	served := map[int32]bool{}
	var first error
	for _, lp := range primaries {
		served[lp.Group] = true
		if err := a.maintainPrimary(ctx, lp); err != nil && first == nil {
			first = err
		}
	}
	// A group this server does not run the primary of any more has nothing to report.
	a.mu.Lock()
	for g := range a.primaries {
		if !served[g] {
			delete(a.primaries, g)
			delete(a.slotWarn, g)
		}
	}
	a.rounds++
	a.mu.Unlock()
	return first
}

func (a *Agent) maintainPrimary(ctx context.Context, lp localPrimary) error {
	g := lp.Group
	st := a.local.snapshot()
	entries := st.entriesOfGroup(g)
	c, err := a.Svc.InstanceConn(ctx, lp.Dir, "postgres")
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

	// What the primary names now: a standby already in the list stays (it was checked when it went in);
	// a new name goes in only when the standby is registered in the catalog, streams, and the extension
	// agrees that the name matches it (kingres_shard 0.5, sequence 1 step 2).
	settings, err := a.mgr().Settings(lp.Dir)
	if err != nil {
		return fmt.Errorf("reading the managed settings: %w", err)
	}
	listed := ListedNames(settings.Extra[syncNamesKey])
	frozen := a.isFrozen(g)
	gone := a.goneServers()

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
		if !dropGoneEntry(e, gone, frozen) && a.mayName(ctx, lp.Dir, e, listed, streaming) {
			names = append(names, AppName(e.ID))
		}
		if streaming[AppName(e.ID)] {
			streamingIDs[e.ID] = true
			ps.Streaming = append(ps.Streaming, e.ID)
		}
	}
	ps.NeedsRebuild = sortedIDs(needs)

	// synchronous_standby_names.
	applied := settings.Extra[syncNamesKey]
	a.mu.Lock()
	ss := a.syncs[g]
	if ss == nil {
		ss = &SyncState{}
		a.syncs[g] = ss
	}
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
		if err := a.mgr().WriteManaged(lp.Dir, next); err != nil {
			return fmt.Errorf("writing synchronous_standby_names: %w", err)
		}
		if err := a.mgr().Reload(ctx, lp.Dir); err != nil {
			return fmt.Errorf("reloading the primary instance: %w", err)
		}
		applied = *d.Set
		a.audit(ctx, "synchronousStandbyNamesChanged", strconv.Itoa(int(g)), audit.OutcomeSuccess,
			map[string]any{"group": g, "synchronousStandbyNames": applied, "withoutSynchronousStandby": d.WithoutSynchronousStandby})
	}
	ps.SynchronousStandbyNames, ps.Without = applied, d.WithoutSynchronousStandby
	a.mu.Lock()
	a.primaries[g], a.slotWarn[g] = ps, warned
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
	if len(a.primaries) == 0 {
		return nil, nil
	}
	var conds []monitor.Condition
	for _, g := range a.primaryGroups() {
		ps := a.primaries[g]
		if ps.Without {
			conds = append(conds, monitor.Condition{Kind: "groupWithoutSynchronousStandby", Severity: "warning",
				Subject: fmt.Sprintf("server:%s|group:%d", a.Self, g),
				Message: fmt.Sprintf("Shard group %d is running without a synchronous standby: its standbys have not streamed for longer than the grace period, so commits are acknowledged by the primary on server %s alone and a failure of that server now loses them.", g, a.Self)})
		}
		for id, bytesRetained := range a.slotWarn[g] {
			conds = append(conds, monitor.Condition{Kind: "replicationLagHigh", Severity: "warning",
				Subject: fmt.Sprintf("server:%s|group:%d|standby:%d", a.Self, g, id),
				Message: fmt.Sprintf("The slot of standby %d of shard group %d retains %d bytes of WAL on server %s; it is dropped at the retention limit and the standby is then rebuilt.", id, g, bytesRetained, a.Self)})
		}
	}
	return conds, map[string]bool{"groupWithoutSynchronousStandby": true, "replicationLagHigh": true}
}

// mayName says whether the name of a standby may be in synchronous_standby_names
// now. A name that is in the list stays until the plan drops the standby. A new
// one needs the standby's node id (it is registered, which the extension's check
// of the name requires), a streaming walsender of that name, a name that is safe
// to render, and the extension's own answer (kingres.standby_name_matches on this
// primary): a name that matches nobody makes every commit of the group wait for
// ever.
func (a *Agent) mayName(ctx context.Context, dir int, e Entry, listed map[string]bool, streaming map[string]bool) bool {
	name := AppName(e.ID)
	if listed[strings.ToLower(name)] {
		return true
	}
	if e.CatalogNode == 0 || !SafeName(name) || !StreamingUnder(streaming, name) {
		return false
	}
	match := a.NameMatches
	if match == nil {
		match = a.nameMatchesInDatabase
	}
	ok, err := match(ctx, dir, name)
	if err != nil {
		a.Log.Debug("standby: the extension could not check a standby name", "name", name, "error", err)
		return false
	}
	return ok
}

// nameMatchesInDatabase asks the extension on this server's primary.
func (a *Agent) nameMatchesInDatabase(ctx context.Context, dir int, name string) (bool, error) {
	c, err := a.Svc.InstanceConn(ctx, dir, sysdb.Database)
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var ok bool
	err = c.QueryRow(ctx, `SELECT kingres.standby_name_matches($1)`, name).Scan(&ok)
	return ok, err
}
