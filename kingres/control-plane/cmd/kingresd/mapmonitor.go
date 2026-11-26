// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"sort"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
)

// The candidate monitor of the map group (design ha-phase5-part2.md, 3.4 and 4.1): on every server that hosts a ready standby of group 0,
// the same evidence and the same settings as the loop on the leader, for the one case in which the leader is the server that is gone.
// The candidates are ranked by the id of their standby and wait a stagger of 30 s per rank, then check again that nothing changed; the
// lease makes the choice safe whatever the order (one holder), and B4 hands the journal to a better candidate by position.

// mapStagger is the wait per rank.
const mapStagger = 90 * time.Second

// mapAbandonAfter is how long a bootstrap that did not reach the promotion may stand before the monitor gives it up.
const mapAbandonAfter = 15 * time.Minute

type mapMonitor struct {
	m              *mapFailover
	failureTimeout time.Duration
	interval       time.Duration
	stagger        time.Duration
	// since is when this monitor first saw the evidence for the term and server it names.
	sinceFor  string
	sinceTerm int64
	since     time.Time
}

func newMapMonitor(m *mapFailover, failureTimeout time.Duration) *mapMonitor {
	return &mapMonitor{m: m, failureTimeout: failureTimeout, interval: 5 * time.Second, stagger: mapStagger}
}

// Run loops until ctx ends.
func (mm *mapMonitor) Run(ctx context.Context) {
	for ctx.Err() == nil {
		mm.step(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(mm.interval):
		}
	}
}

// rank is the position of this server among the servers that host a ready, registered standby of group 0 (0 when it is not one).
func (mm *mapMonitor) rank(ports *mapPorts, ctx context.Context) (int, bool) {
	recs, err := ports.Standbys(ctx)
	if err != nil {
		return 0, false
	}
	var ids []int64
	mine := int64(0)
	for _, r := range recs {
		if r.State != "ready" || r.CatalogNode == 0 {
			continue
		}
		ids = append(ids, r.ID)
		if r.Server == mm.m.self {
			mine = r.ID
		}
	}
	if mine == 0 {
		return 0, false
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i, id := range ids {
		if id == mine {
			return i, true
		}
	}
	return 0, false
}

// step is one look. It returns true when it asked for the bootstrap.
func (mm *mapMonitor) step(ctx context.Context) bool {
	m := mm.m
	ports := &mapPorts{m: m}
	if m.journal.Active() {
		// A bootstrap that did not reach the promotion and is old, or of a term the catalog has left behind, is given up (F4-06): the loser of
		// two candidates, or a start against a server that was not lost, must not keep this monitor, the gate and the status for ever.
		cur, ok := m.journal.Current()
		term, _, err := ports.CatalogTerm(ctx)
		if !ok || err != nil {
			return false
		}
		m.abandonIfStuck(ctx, cur, term, time.Now())
		if m.journal.Active() {
			return false
		}
	}
	rank, ok := mm.rank(ports, ctx)
	if !ok {
		return false
	}
	term, primary, err := ports.CatalogTerm(ctx)
	if err != nil || primary == "" || primary == m.self {
		mm.since = time.Time{}
		return false
	}
	conn, err := m.sysConn(ctx)
	if err != nil {
		return false
	}
	st, err := autoSettings(ctx, conn, mm.failureTimeout)
	_ = conn.Close(context.WithoutCancel(ctx))
	if err != nil {
		return false
	}
	members, err := ports.Members(ctx)
	if err != nil {
		return false
	}
	live := 0
	for _, x := range members {
		if x == m.self || m.pl.Live(ctx, x) == "live" {
			live++
		}
	}
	sv := failover.AutoServer{ID: primary, Members: len(members), Groups: []int32{0}, WithStandby: []int32{0}, Dead: m.pl.Live(ctx, primary) == "dead"}
	if silence, ok := m.pl.reg.Liveness().Silence(primary); ok {
		sv.Silence = silence
	}
	if sv.Dead && sv.Silence >= st.EffectiveTimeout() {
		sv.NotLiveFor, sv.Answered = corroborate(ctx, m, members, primary)
	}
	// The cool-down of the map group is the time since its last bootstrap ended (F4-14); a bootstrap in progress was left above.
	view := failover.AutoView{Settings: st, Servers: []failover.AutoServer{sv}, HearsMajority: live*2 > len(members), Now: time.Now()}
	if cur, ok := m.journal.Current(); ok && !cur.Done[journal.StepGateOpen].IsZero() {
		view.LastEnded = map[int32]time.Time{0: cur.Done[journal.StepGateOpen]}
	}
	d := failover.Evaluate(view)
	if d.Start == "" {
		mm.since = time.Time{}
		return false
	}
	// The evidence holds: wait the stagger of this rank, and look again at the same leader and the same term.
	if mm.since.IsZero() || mm.sinceFor != primary || mm.sinceTerm != term {
		mm.since, mm.sinceFor, mm.sinceTerm = time.Now(), primary, term
	}
	if time.Since(mm.since) < time.Duration(rank)*mm.stagger {
		return false
	}
	m.log.Warn("the manager leader's server is lost: this server promotes the map group", "server", primary, "rank", rank, "reason", d.Reason)
	if err := m.begin(ctx, mapStartBody{Server: primary, RequestedBy: "system", Trigger: "automatic", Comment: "automatic failover of the map group"}); err != nil {
		m.log.Warn("the automatic failover of the map group did not start", "error", err.Error())
		return false
	}
	mm.since = time.Time{}
	return true
}

// abandonIfStuck gives up the bootstrap in progress when it did not reach the promotion and is older than mapAbandonAfter or of a term that the catalog
// has left behind, and lets go of what it holds (F5-02).
func (m *mapFailover) abandonIfStuck(ctx context.Context, cur journal.Journal, catalogTerm int64, now time.Time) bool {
	stuck, why := journal.Stuck(cur, catalogTerm, now, mapAbandonAfter)
	if !stuck {
		return false
	}
	m.log.Warn("the bootstrap of the map group is given up", "reason", why)
	ok, _ := m.abandonLocal(ctx, why)
	return ok
}
