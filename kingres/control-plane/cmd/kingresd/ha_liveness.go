// SPDX-License-Identifier: PostgreSQL

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/liveness"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// processClock is the one clock of the high-availability features: monotonic for
// decisions, wall (shifted by the fault hook's clock step in tests) for display.
func processClock(d *haDeps) clock.Clock {
	return clock.Stepped{Base: clock.NewReal(), Offset: d.Fault.ClockOffset}
}

// startLiveness starts the heartbeats: this server sends one to every other
// server of the peer directory each heartbeat-interval, answers theirs, and
// keeps the tracker that the status of servers and the alerts are read from.
// Without an internal API there is nobody to exchange heartbeats with.
func startLiveness(d *haDeps) error {
	if d.Cfg.InternalListen == "" || !d.Svc.Enabled() || d.Peers == nil {
		return nil
	}
	h := d.Cfg.HA
	clk := processClock(d)
	tr := liveness.New(clk, liveness.Config{Interval: h.HeartbeatInterval, Suspect: h.SuspectTimeout, Failure: h.FailureTimeout}, d.ServerHost)
	d.Registry.SetLiveness(tr)

	timeout := min(max(2*time.Second, 2*h.HeartbeatInterval), h.FailureTimeout/2)
	pc := peerclient.New(peerclient.Config{Self: d.ServerHost, PKIDir: d.Svc.PKIDir(), Peers: d.Peers, Revoked: d.Svc.RevokedSerials, Timeout: timeout, Fault: d.Fault})
	d.OnCertificateInstalled(pc.ForgetAll)
	started := time.Now()
	uptime := func() int64 { return int64(time.Since(started).Seconds()) }

	d.Calls.Register("POST /heartbeat", func(_ context.Context, peer string, body []byte) (any, error) {
		var req wire.HeartbeatRequest
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				return nil, &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: "The heartbeat is not valid."}
			}
		}
		tr.Heard(peer)
		return wire.HeartbeatResponse{ServerID: d.ServerHost, UptimeSeconds: uptime()}, nil
	})

	beat := &liveness.Beater{Tracker: tr, Interval: h.HeartbeatInterval, Timeout: timeout,
		Peers: func() []string {
			all := d.Peers.All()
			ids := make([]string, 0, len(all))
			for _, p := range all {
				ids = append(ids, p.ServerID)
			}
			return ids
		},
		Send: func(ctx context.Context, server string) error {
			var resp wire.HeartbeatResponse
			err := pc.Do(ctx, server, http.MethodPost, "/heartbeat", wire.HeartbeatRequest{IntervalMillis: h.HeartbeatInterval.Milliseconds(), UptimeSeconds: uptime()}, &resp)
			if err != nil {
				return err
			}
			if resp.ServerID != server {
				return fmt.Errorf("the server that answered is %q, not %q", resp.ServerID, server)
			}
			return nil
		}}
	d.WG.Add(3)
	go func() { defer d.WG.Done(); beat.Run(d.BG) }()
	go func() { defer d.WG.Done(); runPeerAlerts(d, tr) }()
	go func() { defer d.WG.Done(); rememberLeader(d) }()
	return nil
}

// rememberLeader keeps the leader the catalog names in memory, once per
// heartbeat interval, so that the trust decision of leaderTrusted never has to
// read the database on the path of a request.
func rememberLeader(d *haDeps) {
	t := time.NewTicker(d.Cfg.HA.HeartbeatInterval)
	defer t.Stop()
	for {
		ctx, cancel := context.WithTimeout(d.BG, 3*time.Second)
		if id, err := d.Leaders.Get(ctx); err == nil && id != "" {
			d.LastLeader.set(id)
		}
		cancel()
		select {
		case <-d.BG.Done():
			return
		case <-t.C:
		}
	}
}

// leaderTrusted is the decision of CP2-07 (README, "Heartbeats and liveness"): a
// server trusts its own copy of roles and grants for authorizing sessions while
// it is the leader, while it knows no leader yet, and while the leader was heard
// within the leader-loss bound. The leader pushes every change to roles, grants
// and settings to the servers; one that has not heard from it for the bound can
// no longer tell whether a role was revoked meanwhile. A blip shorter than the
// bound changes nothing.
func leaderTrusted(reg *ha.Registry, last *leaderMemory, self string, bound time.Duration) func() bool {
	return func() bool {
		id := last.get()
		if id == "" || id == self {
			return true
		}
		silence, ok := reg.Liveness().Silence(id)
		return !ok || silence <= bound
	}
}

// runPeerAlerts raises serverSuspect and serverDown for the other servers while
// this server is the manager leader (the alert table of the leader is where the
// cluster's alerts about servers live; the instance alerts of each server are the
// monitor's). It clears them when the server is heard again. Reconciling is
// idempotent; it is also repeated every ten rounds so that a restart of this
// server recovers the alerts.
func runPeerAlerts(d *haDeps, tr *liveness.Tracker) {
	t := time.NewTicker(d.Cfg.HA.HeartbeatInterval)
	defer t.Stop()
	last := map[string]ha.PeerState{}
	round := 0
	for {
		select {
		case <-d.BG.Done():
			return
		case <-t.C:
		}
		round++
		ctx, cancel := context.WithTimeout(d.BG, 5*time.Second)
		reconcilePeerAlerts(ctx, d, tr, last, round%10 == 0)
		cancel()
	}
}

func reconcilePeerAlerts(ctx context.Context, d *haDeps, tr *liveness.Tracker, last map[string]ha.PeerState, force bool) {
	if id, err := d.Leaders.Get(ctx); err != nil || id != d.ServerHost {
		clear(last)
		return
	}
	changed := force
	for _, id := range tr.Expected() {
		if tr.State(id) != last[id] {
			changed = true
		}
	}
	if !changed {
		return
	}
	c, err := d.Svc.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return // the instance is not reachable now; the next round tries again
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, `SELECT server_id, status FROM kingres_cp.server`)
	if err != nil {
		d.Log.Debug("reading the servers for the liveness alerts", "error", err)
		return
	}
	status, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var s [2]string
		return s, r.Scan(&s[0], &s[1])
	})
	if err != nil {
		return
	}
	now := time.Now()
	for _, s := range status {
		id := s[0]
		if id == d.ServerHost {
			continue
		}
		var conds []monitor.Condition
		st := tr.State(id)
		if s[1] == "active" {
			silence, _ := tr.Silence(id)
			conds = monitor.PeerLivenessConditions(id, st, silence)
		}
		if err := monitor.ReconcileScope(ctx, c, "server:"+id, conds, monitor.LivenessKinds, now); err != nil {
			d.Log.Warn("updating the liveness alerts", "server", id, "error", err)
			return
		}
		last[id] = st
	}
}
