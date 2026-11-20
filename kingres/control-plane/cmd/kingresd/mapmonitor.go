// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"sort"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
)

// The candidate monitor of the map group (design ha-phase5-part2.md, 3.4 and 4.1): on every server that hosts a ready standby of group 0,
// the same evidence and the same settings as the loop on the leader, for the one case in which the leader is the server that is gone.
// The candidates are ranked by the id of their standby and wait a stagger of 30 s per rank, then check again that nothing changed; the
// lease makes the choice safe whatever the order (one holder), and B4 hands the journal to a better candidate by position.

// mapStagger is the wait per rank.
const mapStagger = 30 * time.Second

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
	if m.journal.Active() {
		return false
	}
	ports := &mapPorts{m: m}
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
	d := failover.Evaluate(failover.AutoView{Settings: st, Servers: []failover.AutoServer{sv}, HearsMajority: live*2 > len(members), Now: time.Now()})
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
