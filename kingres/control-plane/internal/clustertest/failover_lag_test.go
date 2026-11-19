// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// A standby that lags at the failover (design 2.6, T8): the group has two standbys; one of them cannot receive any more,
// so it holds less than the other, which has every acknowledged row. The failover chooses the one that has most, the
// verdict stays "none" (the synchronous list names both, and the best one was read), no acknowledged row is lost, and the
// standby that lagged follows the new primary afterwards.
func TestAStandbyThatLagsIsNotChosenAndFollowsTheNewPrimary(t *testing.T) {
	c, port, pool := foCluster(t)
	leader := c.Leader()
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 2})
	waitStandbys(t, c, 2)
	g, old, _ := foVictim(t, c)
	until(t, 4*time.Minute, "both standbys of the group registered in the catalog of the database fo", func() bool {
		fc := leader.ServicePG("fo")
		defer func() { _ = fc.Close(context.Background()) }()
		var n int
		return fc.QueryRow(context.Background(), `SELECT count(*) FROM kingres.node WHERE role = 'standby'`).Scan(&n) == nil && n >= 2*len(c.Nodes)
	})
	waitFacts(t, leader, g)
	var lagging, best standbyView
	views := topology(t, leader)[g]
	if len(views) != 2 {
		t.Fatalf("group %d has %d standbys, want 2", g, len(views))
	}
	lagging, best = views[0], views[1]
	lagNode, bestNode := nodeOf(c, lagging.server), nodeOf(c, best.server)
	defer func() {
		if t.Failed() {
			t.Logf("catalog of the leader: %s\nleader log tail:\n%s", foCatalogDump(leader), leader.LogTail(4000))
		}
	}()
	w := startFoWriters([]*clustertest.Node{leader}, port, pool)
	until(t, time.Minute, "rows acknowledged", func() bool { return w.count() > 20 })

	rec := opRecordOf(t, leader, g, lagging.server)
	opCut(t, lagNode, g, 100+g, lagging.port)
	opWaitNotStreaming(t, old, "kingres_standby_"+strconv.FormatInt(rec.id, 10))
	mark := w.count()
	until(t, time.Minute, "rows acknowledged through the other standby while one lags", func() bool { return w.count() > mark+60 })

	old.Kill()
	old.StopInstance()
	old.StopStandbys()
	st, resp := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "comment": "a standby lags"}, nil)
	if st != 202 {
		t.Fatalf("failover request (no allowDataLoss: the verdict must be none): %d %v", st, resp)
	}
	id := clustertest.TaskID(resp)
	// The lagging standby can follow once the new primary is pointed at it: its own receiver is let go when the catalog
	// names the new primary (the step that points the standbys at it comes next and waits for the walreceiver).
	until(t, 8*time.Minute, "the catalog to name the best standby the primary of the group", func() bool {
		hosts, _ := primaryOf(t, leader)
		return hosts[g] == best.server
	})
	if err := opRestore(lagNode, 100+g, lagging.port); err != nil {
		t.Fatal(err)
	}
	if task := leader.WaitTaskDone(id, 8*time.Minute); task["state"] != "done" {
		t.Fatalf("the failover: %v", task)
	}
	rep := leader.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", id), nil)
	gs, _ := rep["groups"].([]any)
	if len(gs) != 1 {
		t.Fatalf("report: %v", rep)
	}
	gr := gs[0].(map[string]any)
	if gr["toServerId"] != best.server {
		t.Errorf("the failover promoted %v, the standby that has every acknowledged row is %s (the lagging one is %s)", gr["toServerId"], best.server, lagging.server)
	}
	if dl, _ := gr["dataLoss"].(map[string]any); dl["verdict"] != "none" {
		t.Errorf("verdict: %v", dl)
	}
	cands, _ := gr["candidates"].([]any)
	if len(cands) != 2 {
		t.Errorf("candidates: %v", cands)
	}
	for _, x := range cands {
		cm := x.(map[string]any)
		if cm["chosen"] == true != (cm["serverId"] == best.server) {
			t.Errorf("candidate %v: chosen is %v", cm["serverId"], cm["chosen"])
		}
	}
	// The one that lagged streams from the new primary.
	until(t, 4*time.Minute, "the standby that lagged to stream from the new primary", func() bool {
		for _, v := range topology(t, leader)[g] {
			if v.server == lagging.server && v.state == "running" && v.syncState != "" {
				return true
			}
		}
		return false
	})
	acked := w.Stop()
	have := foReadIDs(t, leader, port, pool)
	for id := range acked {
		if !have[id] {
			t.Errorf("acknowledged row %d is missing", id)
			break
		}
	}
	_ = bestNode
}

// A second failover is asked for while one is paused (T-extra of the increment 3 list): the paused one holds the others
// off (one failover at a time, cluster-wide, 409 failover_in_progress), the status says it waits, and a repeated request
// for the same server resumes the same task instead of making another.
func TestASecondFailoverWhileOneIsPaused(t *testing.T) {
	c, port, pool := foClusterWith(t, "-lease-duration", "20s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader := c.Leader()
	g, old, _ := foVictim(t, c)
	waitFacts(t, leader, g)
	var other *clustertest.Node
	survivors := []*clustertest.Node{}
	for _, n := range c.Nodes {
		if n != old {
			survivors = append(survivors, n)
		}
		if n != old && n != leader {
			other = n
		}
	}
	if other == nil {
		t.Fatal("no third server")
	}
	defer func() {
		if t.Failed() {
			t.Logf("catalog of the leader: %s\nleader log tail:\n%s", foCatalogDump(leader), leader.LogTail(5000))
		}
	}()
	w := startFoWriters(survivors, port, pool)
	until(t, time.Minute, "rows acknowledged", func() bool { return w.count() > 20 })
	old.Kill()
	old.StopInstance()
	old.StopStandbys()
	var lastSeen time.Time
	{
		sc := leader.ServicePG("kingres_system")
		if err := sc.QueryRow(context.Background(), `SELECT seen_at FROM kingres_cp.group_replication_state WHERE group_id = $1`, g).Scan(&lastSeen); err != nil {
			t.Fatal(err)
		}
		_ = sc.Close(context.Background())
	}
	// The leader's kingresd ends after the lease wait and is started again: its check of the group is then older than the
	// verdict accepts and the task pauses for a decision.
	leader.SetFaults(fault.Spec{TaskExitPoint: "step_recorded", TaskExitStep: 3})
	st, resp := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard"}, nil)
	if st != 202 {
		t.Fatalf("failover request: %d %v", st, resp)
	}
	id := clustertest.TaskID(resp)
	until(t, 5*time.Minute, "the leader's kingresd to end at the fault point", func() bool { st, _ := leader.Do("GET", "/healthz", nil, nil); return st != 200 })
	leader.Kill()
	// The check of the group is older than the verdict accepts (thirty seconds) once the leader has been away long enough: the
	// restarted run does not know how long the primary has been silent and reads the age of the check itself. Wait for that
	// age, not for a fixed time.
	until(t, 3*time.Minute, "the last check of the group to be older than the verdict accepts", func() bool { return time.Since(lastSeen) > 45*time.Second })
	leader.Heal()
	leader.Start()
	until(t, 3*time.Minute, "the restarted leader accepts a sign-in", func() bool { return leader.Login(clustertest.AdminUser, clustertest.AdminPass) })
	until(t, 5*time.Minute, "the task to pause for a decision", func() bool {
		// A member that has not found the manager leader yet answers 503 with Retry-After (the restarted leader is being elected again): wait.
		code, body := leader.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		if code == 503 {
			return false
		}
		if code != 200 {
			t.Fatalf("GET /tasks/%v: status %d %v", id, code, body)
		}
		if body["state"] == "done" || body["state"] == "failed" {
			t.Fatalf("the task ended instead of pausing: %v", body)
		}
		return body["state"] == "paused"
	})
	// The second failover is refused while this one waits.
	st, p := leader.Do("POST", "/servers/"+other.Host+"/failover", map[string]any{"mode": "hard"}, nil)
	if st != 409 || p["code"] != "failover_in_progress" {
		t.Errorf("a second failover while one is paused: %d %v", st, p)
	}
	if status := leader.Expect(200, "GET", "/failover", nil); status["taskId"] == nil || fmt.Sprint(status["taskId"]) != fmt.Sprint(id) {
		t.Errorf("the status resource does not name the paused task %v: %v", id, status)
	}
	// The same request again, accepting the loss, resumes the same task.
	st, resp2 := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true}, nil)
	if st != 202 || fmt.Sprint(clustertest.TaskID(resp2)) != fmt.Sprint(id) {
		t.Fatalf("resuming the paused failover: %d %v (the task was %v)", st, resp2, id)
	}
	if task := leader.WaitTaskDone(id, 8*time.Minute); task["state"] != "done" {
		t.Fatalf("the failover after the resume: %v", task)
	}
	acked := w.Stop()
	have := foReadIDs(t, leader, port, pool)
	for rid := range acked {
		if !have[rid] {
			t.Errorf("acknowledged row %d is missing", rid)
			break
		}
	}
}
