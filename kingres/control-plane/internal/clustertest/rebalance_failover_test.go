// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// A failover, a switchover and a failover of the map group with a bucket move in flight (T9 of ha-phase5-part2.md, P4D-01 of
// the closing review of phase 4). The one place where the guarantees of a move (no row lost or doubled, one owner of a bucket, no
// move left unfinished, a rebalance that can be started again) meet the failover code. The move is held at a fault point of the
// extension (the mechanism of rebalance_faults_test.go) while the server that is the source of the move is lost or handed over.
//
// What every case asserts at its end: every acknowledged row of the writers is there and no id is there twice; every bucket has
// one owner and that owner is the same in the catalog of every server that is up; no move of the extension is unfinished and
// check_cluster has nothing that forbids a move; the rebalance is not running and the run's report says what became of each move.

// mvDB is the database of the failover tests (foCluster).
const mvDB = "fo"

// mvEnv is a cluster of three servers with a sharded database, the victim group g whose primary runs on old (not the leader's
// server), the server nw of its standby, and writers through the entry points of the servers named.
type mvEnv struct {
	c       *clustertest.Cluster
	port    int
	pool    *x509.CertPool
	leader  *clustertest.Node
	g       int
	old, nw *clustertest.Node
	w       *foWriters
}

// mvGateAt arms a fault point of the extension on the leader's instance for new sessions of the failover database; the move that
// reaches it waits until release or drop. drop removes the gate file only (a move that is already waiting goes on at once, and a
// point reached later does not wait); release also removes the setting, which needs the leader's instance to be up.
func mvGateAt(t *testing.T, c *clustertest.Cluster, point string) (reached func(time.Duration), release, drop func()) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "gate")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	set := func(q string) {
		conn := c.Leader().ServicePG("postgres")
		defer func() { _ = conn.Close(context.Background()) }()
		if _, err := conn.Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	set(`ALTER DATABASE ` + mvDB + ` SET kingres_shard.fault_injection = '` + point + `:wait:` + gate + `'`)
	reached = func(d time.Duration) {
		t.Helper()
		until(t, d, "the move to reach "+point, func() bool { _, err := os.Stat(gate + ".reached"); return err == nil })
	}
	drop = func() { _ = os.Remove(gate) }
	release = func() {
		_ = os.Remove(gate)
		set(`ALTER DATABASE ` + mvDB + ` RESET kingres_shard.fault_injection`)
	}
	return reached, release, drop
}

// mvCluster forms the cluster of foCluster, finds the victim group and drains the server old of its buckets (shouldHaveShards
// false), so that a rebalance moves the buckets of group g away. The writers run through the servers in writeVia.
func mvCluster(t *testing.T, lease ...string) (*mvEnv, func(...*clustertest.Node)) {
	t.Helper()
	var c *clustertest.Cluster
	var port int
	var pool *x509.CertPool
	if len(lease) == 0 {
		c, port, pool = foCluster(t)
	} else {
		c, port, pool = foClusterWith(t, lease...)
	}
	leader := c.Leader()
	g, old, nw := foVictim(t, c)
	waitFacts(t, leader, g)
	e := &mvEnv{c: c, port: port, pool: pool, leader: leader, g: g, old: old, nw: nw}
	leader.Expect(200, "PATCH", "/servers/"+old.Host, map[string]any{"shouldHaveShards": false})
	start := func(via ...*clustertest.Node) {
		e.w = startFoWriters(via, port, pool)
		until(t, time.Minute, "rows acknowledged before the move", func() bool { return e.w.count() > 40 })
	}
	return e, start
}

// mvStartRun starts the rebalance and returns the id of its task.
func mvStartRun(t *testing.T, l *clustertest.Node) any {
	t.Helper()
	return clustertest.TaskID(l.Expect(202, "POST", "/rebalance", nil))
}

// mvOneMoveInFlight waits until the status shows exactly one move in flight and returns it.
func mvOneMoveInFlight(t *testing.T, l *clustertest.Node) map[string]any {
	t.Helper()
	var mv map[string]any
	until(t, time.Minute, "one move in flight in the status", func() bool {
		moves := anyList(rbStatus(l)["inFlightMoves"])
		if len(moves) != 1 {
			return false
		}
		mv, _ = moves[0].(map[string]any)
		return mv != nil
	})
	t.Logf("the move in flight: %v", mv)
	return mv
}

// mvNotRunning waits until no run is pending, running or stopping on the leader (idle, stopped or failed) and returns the status.
func mvNotRunning(t *testing.T, l *clustertest.Node, d time.Duration) map[string]any {
	t.Helper()
	var s map[string]any
	until(t, d, "the rebalance to be neither running nor stopping", func() bool {
		st, body := l.Do("GET", "/rebalance", nil, nil)
		if st != 200 {
			return false // the system database is moving with a failover: ask again
		}
		s = body
		return s["state"] != "running" && s["state"] != "stopping"
	})
	return s
}

// mvFinishRun starts the rebalance again until a run completes with nothing left to move. A start that is refused because the
// cluster is not ready (409 other than "nothing to rebalance", 503) is repeated for up to wait; ok is false when it never started.
func mvFinishRun(t *testing.T, l *clustertest.Node, wait time.Duration) (rep map[string]any, ok bool) {
	t.Helper()
	end := time.Now().Add(wait)
	var last string
	for round := 0; round < 8; {
		st, body := l.Do("POST", "/rebalance", nil, nil)
		switch {
		case st == 409 && body["code"] == "nothing_to_rebalance":
			return rep, true
		case st == 202:
			round++
			id := clustertest.TaskID(body)
			for e2 := time.Now().Add(10 * time.Minute); time.Now().Before(e2); time.Sleep(time.Second) {
				if s := rbStatus(l)["state"]; s == "idle" || s == "stopped" || s == "failed" {
					break
				}
			}
			_, rep = l.Do("GET", fmt.Sprintf("/rebalance/reports/%v", id), nil, nil)
			if rep["outcome"] == "completed" {
				return rep, true
			}
		default:
			last = fmt.Sprintf("%d %v", st, body)
			if time.Now().After(end) {
				t.Logf("the rebalance could not be started again: %s", last)
				return rep, false
			}
			time.Sleep(3 * time.Second)
		}
	}
	t.Fatalf("the rebalance did not complete after repeated starts: %v", rbStatus(l))
	return rep, false
}

// mvReportOutcomes checks that every move of a run's report has an outcome (nothing is left "in flight" in a report of a run that
// is over) and that the report has a final outcome.
func mvReportOutcomes(t *testing.T, l *clustertest.Node, id any) {
	t.Helper()
	st, rep := l.Do("GET", fmt.Sprintf("/rebalance/reports/%v", id), nil, nil)
	if st != 200 {
		t.Errorf("the report of run %v: %d %v", id, st, rep)
		return
	}
	t.Logf("the report of run %v: outcome %v stoppedBy %v moves %d", id, rep["outcome"], rep["stoppedBy"], len(anyList(rep["moves"])))
	if rep["outcome"] == nil {
		t.Errorf("the run %v has no outcome: %v", id, rep)
	}
	for _, m := range anyList(rep["moves"]) {
		if m.(map[string]any)["outcome"] == nil {
			t.Errorf("a move of the report of run %v has no outcome: %v", id, m)
		}
	}
}

// mvSettled waits until no move of the extension is unfinished and check_cluster has nothing that forbids a move, as seen by
// the connection that dial makes to the primary of the map group (the instance that holds the move records).
func mvSettled(t *testing.T, dial func() (*pgx.Conn, error)) {
	t.Helper()
	ctx := context.Background()
	var bad []string
	until(t, 6*time.Minute, "no unfinished move of the extension and nothing in check_cluster that forbids a move", func() bool {
		conn, err := dial()
		if err != nil {
			return false
		}
		defer func() { _ = conn.Close(ctx) }()
		var open int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM kingres.bucket_move WHERE phase NOT IN ('done', 'cleaning', 'failed', 'cancelled')`).Scan(&open); err != nil || open > 0 {
			return false
		}
		bad = nil
		rows, err := conn.Query(ctx, `SELECT check_name || ': ' || coalesce(detail, '') FROM kingres.check_cluster()
			WHERE check_name IN ('stalled_move', 'bucket_moving_without_move', 'move_without_moving_bucket', 'partition_misplaced', 'partition_missing', 'stray_bucket_table', 'bucket_map_incomplete')`)
		if err != nil {
			return false
		}
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			bad = append(bad, s)
		}
		rows.Close()
		return len(bad) == 0
	})
	if len(bad) > 0 {
		t.Errorf("check_cluster: %v", bad)
	}
}

// mvOwners checks that every bucket has exactly one owner and that the catalogs of the servers in up agree on it: the number of
// rows of the map equals the number of distinct buckets, none is moving or orphaned, and the whole map is the same everywhere.
func mvOwners(t *testing.T, up ...*clustertest.Node) {
	t.Helper()
	ctx := context.Background()
	var first string
	for i, n := range up {
		var total, distinct, notActive int
		var digest string
		until(t, 2*time.Minute, "the bucket map of "+n.Host, func() bool {
			conn, err := dialService(n, mvDB)
			if err != nil {
				return false
			}
			defer func() { _ = conn.Close(ctx) }()
			return conn.QueryRow(ctx, `SELECT count(*)::int, count(DISTINCT (colocation_id, bucket))::int, count(*) FILTER (WHERE state <> 'active')::int,
				coalesce(string_agg(colocation_id || '/' || bucket || '=' || group_id, ',' ORDER BY colocation_id, bucket), '')
				FROM kingres.bucket_map`).Scan(&total, &distinct, &notActive, &digest) == nil
		})
		if total == 0 || total != distinct || notActive != 0 {
			t.Errorf("%s: the bucket map has %d rows, %d distinct buckets, %d not active", n.Host, total, distinct, notActive)
		}
		if i == 0 {
			first = digest
		} else if digest != first {
			t.Errorf("the bucket map of %s differs from the one of %s:\n%s\n%s", n.Host, up[0].Host, digest, first)
		}
	}
}

// mvRows checks the data through every server in up: every acknowledged row is there and no id is there twice.
func mvRows(t *testing.T, e mvEnv, acked map[int64]time.Time, up ...*clustertest.Node) {
	t.Helper()
	for _, n := range up {
		have := foReadIDs(t, n, e.port, e.pool)
		lost := 0
		for id := range acked {
			if !have[id] {
				lost++
			}
		}
		if lost > 0 {
			t.Errorf("%d of the %d acknowledged rows are missing when read through %s", lost, len(acked), n.Host)
		}
		var total, distinct int64
		until(t, 2*time.Minute, "the count of the rows through "+n.Host, func() bool {
			conn, err := foConn(context.Background(), n, e.port, e.pool, mvDB)
			if err != nil {
				return false
			}
			defer func() { _ = conn.Close(context.Background()) }()
			return conn.QueryRow(context.Background(), `SELECT count(*), count(DISTINCT id) FROM items`).Scan(&total, &distinct) == nil
		})
		if total != distinct {
			t.Errorf("through %s: %d rows but %d distinct ids: a row is there twice", n.Host, total, distinct)
		}
	}
}

// mvEnd is the common end of the cases: the writers stop after a last stretch of acknowledged writes, and the state is checked
// through the servers in up.
func mvEnd(t *testing.T, e mvEnv, dial func() (*pgx.Conn, error), up ...*clustertest.Node) {
	t.Helper()
	before := e.w.count()
	until(t, 3*time.Minute, "writes after the interplay", func() bool { return e.w.count() > before+30 })
	acked := e.w.Stop()
	mvSettled(t, dial)
	mvOwners(t, up...)
	mvRows(t, e, acked, up...)
}

// T9. The source server of a move is lost while the move is held in a phase, and the automatic failover has to fail it over. The
// failover stops the rebalance first (stopped by the safety rule) and waits for it; the extension settles the move; the failover
// completes; the rebalance can be started again and finishes the job. No acknowledged row is lost, no row is doubled, every
// bucket has one owner. The phases: "copying" (the subscription exists, the copy has not ended) and "switching" (past the point
// of no return: the move is not interrupted and ends within the bounds of the extension).
func TestFailoverDuringBucketMove(t *testing.T) {
	for _, ph := range []struct{ name, point string }{
		{"copying", "move_after_subscription"},
		{"switching", "move_switch_after_marker"},
	} {
		t.Run(ph.name, func(t *testing.T) {
			e, startWriters := mvCluster(t)
			leader, old := e.leader, e.old
			autoEnable(t, leader, 5)
			survivors := []*clustertest.Node{}
			for _, n := range e.c.Nodes {
				if n != old {
					survivors = append(survivors, n)
				}
			}
			startWriters(survivors...)
			defer func() {
				if t.Failed() {
					t.Logf("leader log: %s", leader.LogTail(8000))
				}
			}()
			reached, release, _ := mvGateAt(t, e.c, ph.point)
			defer release()
			id := mvStartRun(t, leader)
			reached(4 * time.Minute)
			mvOneMoveInFlight(t, leader)

			// The source of the move is lost with its server. The loop decides after the silence; the gate is let go when it has.
			foKillServer(old)
			until(t, 4*time.Minute, "the decision of the automatic failover in the log of the leader", func() bool {
				return strings.Contains(leader.LogTail(4000000), `"msg":"automatic failover"`)
			})
			release()
			var rep map[string]any
			until(t, 10*time.Minute, "the automatic failover to end", func() bool {
				r := autoReports(t, leader, "automatic")
				if len(r) == 0 {
					return false
				}
				rep = r[0]
				return rep["outcome"] == "completed" || rep["outcome"] == "completedWithWarnings"
			})

			// The rebalance is over (stopped by the failover, or finished by itself before it); nothing is in flight in its report.
			s := mvNotRunning(t, leader, 5*time.Minute)
			t.Logf("the rebalance after the failover: %v", s["state"])
			if n := len(anyList(s["inFlightMoves"])); n != 0 {
				t.Errorf("%d moves in flight in a rebalance that is not running: %v", n, s)
			}
			mvReportOutcomes(t, leader, id)

			// The old server comes back (as a standby of the group); the extension settles whatever the move left, the rebalance
			// can be started again, and it ends with nothing to move.
			old.Start()
			until(t, 3*time.Minute, "sign-in on the restarted server", func() bool { return old.Login(clustertest.AdminUser, clustertest.AdminPass) })
			mvSettled(t, mvDial(leader))
			if _, ok := mvFinishRun(t, leader, 10*time.Minute); !ok {
				t.Errorf("the rebalance could not be started again after the failover")
			}
			mvEnd(t, *e, mvDial(leader), survivors...)
		})
	}
}

// T9, the planned case. A switchover is refused while a rebalance runs (it names the rebalance and asks for the stop first); the
// stop cancels the move that is held in its copy, the run ends stopped, the switchover then runs with a bucket that is still in
// its old place, and the rebalance started again moves the bucket.
func TestSwitchoverDuringBucketMove(t *testing.T) {
	e, startWriters := mvCluster(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader, old, nw, g := e.leader, e.old, e.nw, e.g
	startWriters(e.c.Nodes...)
	defer func() {
		if t.Failed() {
			t.Logf("leader log: %s", leader.LogTail(8000))
		}
	}()
	reached, release, _ := mvGateAt(t, e.c, "move_after_subscription")
	defer release()
	id := mvStartRun(t, leader)
	reached(4 * time.Minute)
	mv := mvOneMoveInFlight(t, leader)

	// Refused while the rebalance runs; nothing was changed.
	req := map[string]any{"mode": "graceful", "shardGroupIds": []int{g}, "targets": []map[string]any{{"shardGroupId": g, "serverId": nw.Host}}}
	st, p := leader.Do("POST", "/servers/"+old.Host+"/failover", req, nil)
	if st != 409 || p["code"] != "rebalance_in_progress" {
		t.Fatalf("a switchover while a move is in flight: %d %v (the move: %v)", st, p, mv)
	}
	if hosts, _ := primaryOf(t, leader); hosts[g] != old.Host {
		t.Fatalf("the refused switchover changed the primary of group %d: %v", g, hosts)
	}

	// The stop: the loop passes it on to the move, which wakes up when the gate is let go, sees the cancellation and undoes itself.
	stop := leader.Expect(200, "POST", "/rebalance/stop", nil)
	if stop["state"] != "stopping" {
		t.Errorf("state after the stop request: %v", stop["state"])
	}
	release()
	until(t, 5*time.Minute, "the run to be stopped", func() bool { return rbStatus(leader)["state"] == "stopped" })
	if n := len(anyList(rbStatus(leader)["inFlightMoves"])); n != 0 {
		t.Errorf("%d moves in flight in a stopped rebalance", n)
	}
	mvSettled(t, mvDial(leader))

	// The switchover now runs, with writers through every server.
	st, resp := leader.Do("POST", "/servers/"+old.Host+"/failover", req, nil)
	if st != 202 {
		t.Fatalf("the switchover after the stop: %d %v\n%s", st, resp, leader.LogTail(3000))
	}
	body := foWaitTask(t, leader, clustertest.TaskID(resp), 10*time.Minute, "done", "failed")
	if body["state"] != "done" {
		t.Fatalf("the switchover ended %v: %v\n%s", body["state"], body["error"], leader.LogTail(6000))
	}
	if hosts, _ := primaryOf(t, leader); hosts[g] != nw.Host {
		t.Fatalf("the primary of group %d is on %s, want %s", g, hosts[g], nw.Host)
	}
	mvReportOutcomes(t, leader, id)

	// The rebalance started again finishes the job on the new primary of the group.
	until(t, 8*time.Minute, "the old primary to be a ready standby of the group", func() bool {
		for _, s := range topology(t, leader)[g] {
			if s.server == old.Host && s.state != "" && s.state != "rebuilding" {
				return true
			}
		}
		return false
	})
	if _, ok := mvFinishRun(t, leader, 5*time.Minute); !ok {
		t.Errorf("the rebalance could not be started again after the switchover")
	}
	mvEnd(t, *e, mvDial(leader), mvWithout(e.c.Nodes, old)...)
}

// T9, the map group. The leader's server (the primary of the map group, which also runs the rebalance and the extension session
// that makes the move) is lost with a move held in its copy; an operator asks a survivor for the failover of that server; the
// standby of the map group is promoted. The new leader pauses the run (it stopped by the safety rule), the extension's recovery
// settles the move whose session is gone, no acknowledged row is lost or doubled, every bucket has one owner, and the rebalance can
// be started again (resumed) when the cluster allows it. The run's own record of the move is the one the standby replayed; the
// standby of the map group is synchronous, so the record of a move that was recorded before the loss is there.
func TestMapGroupFailoverDuringBucketMove(t *testing.T) {
	m := newMapCluster(t)
	e := mvEnv{c: m.c, port: m.port, pool: m.pool, leader: m.old}
	// The server that is neither the leader's nor the candidate's is drained: the buckets of its group move to the other two.
	m.old.Expect(200, "PATCH", "/servers/"+m.other.Host, map[string]any{"shouldHaveShards": false})
	e.w = startFoWriters(m.survivors, m.port, m.pool)
	until(t, time.Minute, "rows acknowledged before the move", func() bool { return e.w.count() > 40 })
	reached, _, drop := mvGateAt(t, m.c, "move_after_subscription")
	defer drop()
	id := mvStartRun(t, m.old)
	reached(4 * time.Minute)
	mvOneMoveInFlight(t, m.old)

	mapPort := 0 // the port of the standby of the map group, which becomes the primary of the map group
	for _, n := range catalogStandbys(t, m.old) {
		if n.group == 0 {
			mapPort = n.port
		}
	}
	foKillServer(m.old)
	drop() // the instance that waits is gone; this removes the file, so that no later session waits
	m.request(t, m.other)
	task := m.recordedTask(t)
	m.cand.WaitTaskDone(task, 15*time.Minute)

	// The setting of the gate was replicated to the promoted standby: remove it, so that no move waits on the new primary.
	conn, err := tryStandbyConn(m.cand, 0, mapPort, "postgres")
	if err != nil {
		t.Fatalf("connecting to the new primary of the map group: %v", err)
	}
	if _, err = conn.Exec(context.Background(), `ALTER DATABASE `+mvDB+` RESET kingres_shard.fault_injection`); err != nil {
		t.Errorf("resetting the gate on the new leader: %v", err)
	}
	_ = conn.Close(context.Background())

	// The run is paused (or, when the new leader took it over before the pause, running to its end); it is never running on the
	// old leader, and nothing is in flight in it.
	l := m.cand
	s := mvNotRunning(t, l, 5*time.Minute)
	t.Logf("the rebalance after the failover of the map group: %v (%v)", s["state"], s["lastReport"])
	if n := len(anyList(s["inFlightMoves"])); n != 0 {
		t.Errorf("%d moves in flight in a rebalance that is not running: %v", n, s)
	}

	// The run can be started again. The server that was lost is still a member and cannot be reached, so the cluster check may
	// refuse the start (a node that is down forbids a move); that is logged, and the settling below does not depend on it.
	rep, started := mvFinishRun(t, l, 6*time.Minute)
	if started {
		t.Logf("the run after the failover of the map group ended: %v", rep["outcome"])
		mvReportOutcomes(t, l, id)
	}
	mvEnd(t, e, func() (*pgx.Conn, error) { return tryStandbyConn(m.cand, 0, mapPort, mvDB) }, m.survivors...)
}

// mvDial connects to database fo of the instance that the node's own port serves: for the leader, the primary of the map group.
func mvDial(n *clustertest.Node) func() (*pgx.Conn, error) {
	return func() (*pgx.Conn, error) { return dialService(n, mvDB) }
}

// mvWithout returns the nodes except the one named.
func mvWithout(nodes []*clustertest.Node, skip *clustertest.Node) []*clustertest.Node {
	var out []*clustertest.Node
	for _, n := range nodes {
		if n != skip {
			out = append(out, n)
		}
	}
	return out
}
