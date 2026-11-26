// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// stallCluster forms three servers with real standbys (no forced enforcement) and short leases (duration 6 s, margin
// 2 s: a lease is lost when no round that started within the last 4 s succeeded), a table on every primary, and
// returns the group of each server's own primary with the standby view of every group.
func stallCluster(t *testing.T) (*clustertest.Cluster, []int32, map[int][]standbyView) {
	t.Helper()
	c := clustertest.FormWithExtra(t, 3, append(append([]string(nil), standbyArgs...), leaseFlags...), nil)
	waitStandbys(t, c, 1)
	groups := make([]int32, len(c.Nodes))
	for i, n := range c.Nodes {
		pg := n.ServicePG("kingres_system")
		if err := pg.QueryRow(context.Background(), `SELECT group_id FROM kingres.node WHERE host = $1 AND role = 'primary'`, n.Host).Scan(&groups[i]); err != nil {
			t.Fatalf("group of node %d: %v", i, err)
		}
		mustExec(t, n.ServicePG("postgres"), `CREATE TABLE IF NOT EXISTS lease_probe (n bigserial PRIMARY KEY, at timestamptz DEFAULT now())`)
	}
	for i, n := range c.Nodes {
		waitLease(t, c.Nodes[(i+1)%3], n, groups[i], "held", time.Minute)
		waitWatchdog(t, n)
	}
	return c, groups, topology(t, c.Leader())
}

// standbyInRecovery asks the standby instance on port (a copy: the service role has the password of the primary's
// server) whether it is still in recovery. A standby that cannot be asked counts as still a standby (it is not writable
// either way); only a successful answer "false" is a second primary.
func standbyInRecovery(port int, pw string) (inRecovery, asked bool) {
	cfg, _ := pgx.ParseConfig("")
	cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = "127.0.0.1", uint16(port), "postgres", "kingresd", pw //nolint:gosec // test port
	cfg.TLSConfig, cfg.Fallbacks, cfg.ConnectTimeout = nil, nil, 2*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return true, false
	}
	defer func() { _ = c.Close(context.Background()) }()
	if err := c.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err != nil {
		return true, false
	}
	return inRecovery, true
}

// noSecondPrimary samples the standbys of the groups until stop is closed and records any that left recovery.
func noSecondPrimary(t *testing.T, c *clustertest.Cluster, top map[int][]standbyView, groups []int32) (stop func() []string) {
	t.Helper()
	pw := map[int]string{}
	for i, g := range groups {
		pw[int(g)] = servicePassword(t, c.Nodes[i])
	}
	done := make(chan struct{})
	var bad []string
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			case <-time.After(300 * time.Millisecond):
			}
			for g, sbs := range top {
				for _, s := range sbs {
					if in, asked := standbyInRecovery(s.port, pw[g]); asked && !in {
						bad = append(bad, fmt.Sprintf("the standby of group %d on port %d accepts writes at %s", g, s.port, time.Now().Format("15:04:05")))
					}
				}
			}
		}
	}()
	return func() []string { close(done); <-finished; return bad }
}

// Two grantors stalled (SIGSTOP) for longer than the margin: the third server's primary has no majority, so its lease is
// lost by design, and exactly this happens: the primary is fenced within the bound (duration minus margin, plus the
// judging and the stop), no write is acknowledged after that, no standby becomes a primary (nothing is promoted),
// and when the stall ends the primary starts again by itself and takes writes. The bound is a fact of the
// protocol, so the test names it, instead of passing or failing with the load.
func TestAPrimaryWithoutAMajorityOfGrantorsIsFencedAndComesBackByItself(t *testing.T) {
	c, groups, top := stallCluster(t)
	victim, a, b := c.Nodes[1], c.Nodes[0], c.Nodes[2]
	w := startWriter(t, victim)
	w.waitAcks(t, 5, time.Minute)
	stopSampling := noSecondPrimary(t, c, top, groups)
	resumed := false
	defer func() {
		if !resumed {
			a.Resume()
			b.Resume()
		}
	}()
	stalledAt := time.Now()
	a.Pause()
	b.Pause()
	// 4 s of lease left at most, plus the judging every 100 ms and an immediate stop: 12 s is generous and still a bound.
	waitInstance(t, victim, false, 12*time.Second)
	fencedAfter := time.Since(stalledAt)
	if fencedAfter < 2*time.Second {
		t.Fatalf("the primary was fenced after %v: the lease cannot have run out that early", fencedAfter)
	}
	lastAck, _ := w.last()
	if lastAck.After(stalledAt.Add(fencedAfter)) {
		t.Fatalf("a write was acknowledged after the primary was fenced")
	}
	time.Sleep(4 * time.Second) // a stall well beyond the lease: it stays down
	if instanceAccepts(victim) {
		t.Fatal("the fenced primary came back without a lease while the grantors were stalled")
	}
	a.Resume()
	b.Resume()
	resumed = true
	waitInstance(t, victim, true, 2*time.Minute)
	waitLease(t, a, victim, groups[1], "held", time.Minute)
	_, before := w.last()
	w.waitAcks(t, before, time.Minute)
	if bad := stopSampling(); len(bad) > 0 {
		t.Fatalf("a standby became a primary during the stall: %v", bad)
	}
	var rows int
	if err := victim.ServicePG("postgres").QueryRow(context.Background(), `SELECT count(*) FROM lease_probe`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if _, acks := w.last(); rows < acks {
		t.Fatalf("%d rows for %d acknowledged writes: an acknowledged write is lost", rows, acks)
	}
}

// The leader's kingresd stalled (SIGSTOP) for longer than the margin: the other two servers still have a majority
// (themselves), so their primaries are never fenced; the leader's own primary has nobody to renew its lease and is
// stopped by its watchdog (its kingresd cannot) within the bound; when the stall ends it starts again by itself.
func TestAStalledLeaderFencesOnlyItsOwnPrimaryAndComesBackByItself(t *testing.T) {
	c, groups, top := stallCluster(t)
	leader := c.Leader()
	var others []*clustertest.Node
	for _, n := range c.Nodes {
		if n != leader {
			others = append(others, n)
		}
	}
	ws := []*writer{startWriter(t, others[0]), startWriter(t, others[1])}
	for _, w := range ws {
		w.waitAcks(t, 5, time.Minute)
	}
	stopSampling := noSecondPrimary(t, c, top, groups)
	resumed := false
	defer func() {
		if !resumed {
			leader.Resume()
		}
	}()
	stalledAt := time.Now()
	leader.Pause()
	waitInstance(t, leader, false, 12*time.Second)
	if d := time.Since(stalledAt); d < 2*time.Second {
		t.Fatalf("the leader's primary was stopped after %v: the lease cannot have run out that early", d)
	}
	// the others kept their leases the whole time
	for i, n := range others {
		if !instanceAccepts(n) {
			t.Fatalf("the primary of node %d was fenced although it has a majority", n.Index)
		}
		_, before := ws[i].last()
		ws[i].waitAcks(t, before, 30*time.Second)
	}
	time.Sleep(3 * time.Second)
	leader.Resume()
	resumed = true
	waitInstance(t, leader, true, 2*time.Minute)
	for i, n := range others {
		if !instanceAccepts(n) {
			t.Fatalf("the primary of node %d was stopped by the stall of the leader", n.Index)
		}
		_ = i
	}
	if bad := stopSampling(); len(bad) > 0 {
		t.Fatalf("a standby became a primary during the stall: %v", bad)
	}
}
