// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// Phase 10, step 4 of the drain (the hand-over of the map group) and the standby-lag rule of the automatic mode, on real clusters.

// p10d4Cluster forms three servers with standbys, database rb with table items (12 buckets, rows rows) and the lease settings that the
// tests of the switchover of the map group use; the buckets are spread over the servers by a first rebalance.
func p10d4Cluster(t *testing.T, rows int) *clustertest.Cluster {
	t.Helper()
	extra := append([]string{"-rebalance-retry-backoff", "1s", "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s"}, standbyArgs...)
	c := p10Cluster(t, "rb", 12, rows, extra)
	waitStandbys(t, c, 1)
	l := c.Leader()
	until(t, 3*time.Minute, "every standby to be registered in the catalog", func() bool {
		var n int
		err := l.ServicePG("rb").QueryRow(context.Background(), `SELECT count(*) FROM kingres.node WHERE role = 'standby' AND is_active`).Scan(&n)
		return err == nil && n >= len(c.Nodes)
	})
	p10Cleanup(t, c, "3s")
	return c
}

// p10d4Leader finds the node that is the manager leader now, asking every server that answers.
func p10d4Leader(t *testing.T, c *clustertest.Cluster, not string) *clustertest.Node {
	t.Helper()
	var found *clustertest.Node
	until(t, 5*time.Minute, "a manager leader other than "+not, func() bool {
		for _, n := range c.Nodes {
			if n.Host == not {
				continue
			}
			st, body := n.Do("GET", "/cluster", nil, nil)
			if st != 200 {
				continue
			}
			if ml, _ := body["managerLeader"].(map[string]any); ml != nil && ml["serverId"] == n.Host {
				found = n
				return true
			}
		}
		return false
	})
	return found
}

// quietLogWithout is quietLog without the lines that contain skip (the polling of the test and a repeated warning).
func quietLogWithout(log, skip string) string {
	var out []string
	for _, l := range strings.Split(quietLog(log), "\n") {
		if !strings.Contains(l, skip) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func taskSteps(body map[string]any) map[string]string {
	out := map[string]string{}
	prog, _ := body["progress"].(map[string]any)
	for _, s := range anyList(prog["steps"]) {
		m := s.(map[string]any)
		out[fmt.Sprint(m["name"])] = fmt.Sprint(m["state"])
	}
	return out
}

// The server that runs the primary of the map group is drained: the drain pauses itself, the map group is switched over to a standby on another
// server, the new manager leader resumes the drain, and the drain ends with the server holding neither a bucket nor an instance of any group.
// Writers run through all of it; nothing they were told is lost. (Removing the server afterwards is not part of the test: the first server holds the key of the certificate authority of the cluster, which is its own matter.)
func TestDrainHandsOverTheMapGroup(t *testing.T) {
	const rows = 400
	c := p10d4Cluster(t, rows)
	old := c.Leader()
	wantSum := int64(rows) * int64(rows+1) / 2
	rep := waitReport(t, old, clustertest.TaskID(old.Expect(202, "POST", "/rebalance", nil)), 10*time.Minute)
	if rep["outcome"] != "completed" {
		t.Fatalf("the first rebalance: %v", rep)
	}
	if primaryBuckets(t, old)[old.Host] == 0 {
		t.Fatalf("the map group server owns no bucket: %v", primaryBuckets(t, old))
	}
	if hosts, _ := primaryOf(t, old); hosts[0] != old.Host {
		t.Fatalf("the primary of the map group is on %s, not on the leader %s", hosts[0], old.Host)
	}
	pv := old.Expect(200, "GET", "/servers/"+old.Host+"/drain-preview", nil)
	if pv["canDrain"] != true {
		t.Fatalf("the preview of the map group server: %v", pv)
	}

	w := p10WriteTries(t, c.Nodes[1], "rb", 3_000_000, 1800)
	var nl *clustertest.Node
	var id any
	defer func() {
		if t.Failed() {
			t.Logf("old leader log: %s", quietLogWithout(old.LogTail(60000), "checking the instance"))
			if nl != nil {
				_, tk := nl.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
				_, rb := nl.Do("GET", "/rebalance", nil, nil)
				t.Logf("task %v on the new leader: %v\nrebalance status: %v", id, tk, rb)
				t.Logf("new leader log: %s", quietLogWithout(nl.LogTail(90000), "\"msg\":\"request\""))
			}
		}
	}()
	body := old.Expect(202, "POST", "/servers/"+old.Host+"/drain", map[string]any{})
	id = clustertest.TaskID(body)

	nl = p10d4Leader(t, c, old.Host)
	var task map[string]any
	until(t, 25*time.Minute, "the drain to end on the new manager leader", func() bool {
		st, b := nl.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		if st != 200 {
			return false
		}
		if s, _ := b["state"].(string); s == "done" || s == "failed" || s == "cancelled" {
			task = b
			return true
		}
		return false
	})
	if task["state"] != "done" {
		t.Fatalf("the drain ended %v: %v\nnew leader log: %s", task["state"], task["error"], quietLog(nl.LogTail(60000)))
	}
	if got := taskSteps(task)[stepMapGroup]; got != "done" {
		t.Errorf("the step %q is %q: %v", stepMapGroup, got, taskSteps(task))
	}
	if hosts, _ := primaryOf(t, nl); hosts[0] == old.Host || hosts[0] == "" {
		t.Errorf("the primary of the map group is on %q after the drain", hosts[0])
	}
	if st := nl.Expect(200, "GET", "/cluster", nil); st["managerLeader"].(map[string]any)["serverId"] == old.Host {
		t.Errorf("the drained server is still the manager leader")
	}
	extra, extraSum := w.finish(t)
	for _, n := range c.Nodes {
		if n == old {
			continue // the drained server runs no instance of any group any more
		}
		if cnt, sum := rowsOf(t, n); cnt != rows+extra || sum != wantSum+extraSum {
			t.Errorf("node %d sees %d rows (sum %d), want %d (sum %d)", n.Index, cnt, sum, rows+extra, wantSum+extraSum)
		}
	}
	if got := primaryBuckets(t, nl)[old.Host]; got != 0 {
		t.Errorf("the drained server owns %d buckets", got)
	}
	var onOld int
	if err := nl.ServicePG("rb").QueryRow(context.Background(), `SELECT count(*) FROM kingres.node WHERE host = $1`, old.Host).Scan(&onOld); err != nil {
		t.Fatal(err)
	}
	if onOld != 0 {
		t.Errorf("the drained server still hosts %d instances of the groups", onOld)
	}
	// the planned switchover is in the failover reports: one failover of the old leader's server, finished
	reports := nl.Expect(200, "GET", "/failover/reports", nil)
	found := false
	for _, r := range anyList(reports["items"]) {
		m := r.(map[string]any)
		if m["serverId"] == old.Host && m["kind"] == "failover" && strings.HasPrefix(fmt.Sprint(m["outcome"]), "completed") {
			found = true
		}
	}
	if !found {
		t.Errorf("no finished failover of %s in the reports: %v", old.Host, reports)
	}
}

const stepMapGroup = "Hand over the map group"

// The planned switchover is given up after the old leader stopped (the candidate cannot promote, an operator abandons the bootstrap): the old
// leader holds the map group again, and the drain, which went on under the old leader, fails with the reason; the server stays draining and the
// drain can be started again.
func TestDrainWhoseSwitchoverOfTheMapGroupIsGivenUpFailsAndLeavesTheServerDraining(t *testing.T) {
	c := p10d4Cluster(t, 300)
	old := c.Leader()
	views := topology(t, old)[0]
	if len(views) == 0 {
		t.Fatalf("the map group has no standby: %v", topology(t, old))
	}
	nw := nodeOf(c, views[0].server)
	if nw == nil || nw == old {
		t.Fatalf("the standby of the map group is on %q", views[0].server)
	}
	waitReport(t, old, clustertest.TaskID(old.Expect(202, "POST", "/rebalance", nil)), 10*time.Minute)
	nw.SetFaults(fault.Spec{FailoverPoint: "promote:unavailable", FailoverMode: "unavailable"})
	defer nw.Heal()
	body := old.Expect(202, "POST", "/servers/"+old.Host+"/drain", map[string]any{})
	id := clustertest.TaskID(body)
	mapStopped(t, old)
	until(t, 3*time.Minute, "the bootstrap to be given up", func() bool {
		st, b := nw.Do("POST", "/failover/map/abandon", map[string]any{"reason": "the candidate cannot promote"}, nil)
		return st == 200 && b["abandoned"] == true
	})
	var task map[string]any
	until(t, 20*time.Minute, "the drain to end on the old leader once its instance is back", func() bool {
		st, b := old.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		if st != 200 {
			return false
		}
		if s, _ := b["state"].(string); s == "done" || s == "failed" || s == "cancelled" {
			task = b
			return true
		}
		return false
	})
	if task["state"] != "failed" {
		t.Fatalf("the drain ended %v: %v", task["state"], task["error"])
	}
	if e, _ := task["error"].(map[string]any); e == nil || !strings.Contains(fmt.Sprint(e["message"]), "server_hosts_map_group") {
		t.Errorf("the error of the drain: %v", task["error"])
	}
	nw.Heal()
	until(t, 5*time.Minute, "the old leader to hold the map group again", func() bool {
		hosts, _ := primaryOf(t, old)
		return hosts[0] == old.Host
	})
	if s := old.Servers()[old.Host]; s["shouldHaveShards"] != false {
		t.Errorf("the flag of the server: %v", s["shouldHaveShards"])
	}
	// the failed drain is over: a rebalance can run (the lock is not held)
	if st, p := old.Do("POST", "/rebalance", nil, nil); st != 202 && st != 409 {
		t.Errorf("a rebalance after the failed drain: %d %v", st, p)
	} else if st == 409 && p["code"] == "rebalance_in_progress" {
		t.Errorf("the failed drain still holds the rebalance: %v", p)
	}
}

// The lag rule of the automatic mode (design 8.3): a standby that is behind its primary by more than maxStandbyLagBytes holds the automatic run
// back (blockedBy groupDegraded) although the group has all its standbys; once the standby has replayed the log the run starts and completes.
func TestAutomaticRebalanceWaitsForAStandbyThatLags(t *testing.T) {
	const rows = 300
	c := p10Cluster(t, "rb", 12, rows, append([]string{"-rebalance-retry-backoff", "1s"}, standbyArgs...))
	waitStandbys(t, c, 1)
	l := c.Leader()
	until(t, 3*time.Minute, "every standby to be registered in the catalog", func() bool {
		var n int
		err := l.ServicePG("rb").QueryRow(context.Background(), `SELECT count(*) FROM kingres.node WHERE role = 'standby' AND is_active`).Scan(&n)
		return err == nil && n >= len(c.Nodes)
	})
	// the group that owns the buckets (the table was loaded on the first server): its standby stops replaying, and the group is written to
	var g int
	if err := l.ServicePG("rb").QueryRow(context.Background(), `SELECT group_id FROM kingres.bucket_map GROUP BY group_id ORDER BY count(*) DESC LIMIT 1`).Scan(&g); err != nil {
		t.Fatal(err)
	}
	views := topology(t, l)[g]
	if len(views) == 0 {
		t.Fatalf("group %d has no standby: %v", g, topology(t, l))
	}
	sn := nodeOf(c, views[0].server)
	sc := standbyConn(t, sn, g, views[0].port, "postgres")
	if _, err := sc.Exec(context.Background(), `SELECT pg_wal_replay_pause()`); err != nil {
		t.Fatal(err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			if _, err := sc.Exec(context.Background(), `SELECT pg_wal_replay_resume()`); err != nil {
				t.Errorf("resuming the replay: %v", err)
			}
		}
	}
	defer resume()
	l.Expect(200, "PATCH", "/settings/cluster", map[string]any{"rebalance": map[string]any{"maxStandbyLagBytes": 1 << 20}})
	if _, err := l.ServicePG("rb").Exec(context.Background(), `INSERT INTO items SELECT g, repeat('x', 800) FROM generate_series(100000, 108000) g`); err != nil {
		t.Fatal(err)
	}
	l.Expect(200, "PATCH", "/settings/cluster", map[string]any{"rebalance": map[string]any{"automatic": map[string]any{
		"enabled": true, "settleSeconds": 0, "settleAfterTopologySeconds": 0, "minIntervalSeconds": 0, "checkIntervalSeconds": 5, "threshold": 0.15}}})
	blocked := func() bool {
		a, _ := rbStatus(l)["automatic"].(map[string]any)
		ev, _ := a["lastEvaluation"].(map[string]any)
		if ev == nil || ev["outcome"] != "blocked" {
			return false
		}
		for _, r := range anyList(ev["blockedBy"]) {
			if r == "groupDegraded" {
				return true
			}
		}
		return false
	}
	until(t, 3*time.Minute, "the automatic mode to say it is blocked by the standby that lags", blocked)
	// it stays blocked, and no run starts, while the standby is behind
	time.Sleep(15 * time.Second)
	if st := rbStatus(l); st["state"] != "idle" || st["lastReport"] != nil {
		t.Fatalf("a run started although a standby lags: %v", st)
	}
	if !blocked() {
		t.Fatalf("the rule let go while the standby still lags: %v", rbStatus(l)["automatic"])
	}

	resume()
	until(t, 10*time.Minute, "the automatic run to end", func() bool {
		st := rbStatus(l)
		lr, _ := st["lastReport"].(map[string]any)
		return st["state"] == "idle" && lr != nil && lr["trigger"] == "automatic" && lr["outcome"] == "completed"
	})
	var total int
	var sum int64
	if err := l.ServicePG("rb").QueryRow(context.Background(), `SELECT count(*), coalesce(sum(id), 0)::bigint FROM items`).Scan(&total, &sum); err != nil {
		t.Fatal(err)
	}
	if want := rows + 8001; total != want {
		t.Errorf("%d rows after the run, want %d", total, want)
	}
}
