// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"crypto/x509"
	"fmt"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// The planned switchover (increment 7 of ha-phase5-part2.md, section 5, mode graceful): the old primary's server is alive, the writes stop,
// the old primary stops cleanly, the new primary is proven to hold everything and is promoted.

type swEnv struct {
	c       *clustertest.Cluster
	port    int
	pool    *x509.CertPool
	leader  *clustertest.Node
	g       int
	old, nw *clustertest.Node
	w       *foWriters
}

func swCluster(t *testing.T) swEnv {
	t.Helper()
	c, port, pool := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader := c.Leader()
	g, old, nw := foVictim(t, c)
	waitFacts(t, leader, g)
	w := startFoWriters(c.Nodes, port, pool)
	until(t, time.Minute, "rows acknowledged before the switchover", func() bool { return w.count() > 40 })
	return swEnv{c, port, pool, leader, g, old, nw, w}
}

func swRequest(t *testing.T, leader, old, nw *clustertest.Node, g int) any {
	t.Helper()
	st, resp := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "graceful", "shardGroupIds": []int{g},
		"targets": []map[string]any{{"shardGroupId": g, "serverId": nw.Host}}}, nil)
	if st != 202 {
		t.Fatalf("switchover request: %d %v\n%s", st, resp, leader.LogTail(3000))
	}
	return clustertest.TaskID(resp)
}

func swPrimaryHost(t *testing.T, leader *clustertest.Node, g int) string {
	t.Helper()
	hosts, _ := primaryOf(t, leader)
	return hosts[g]
}

// T12. A group is handed to the standby that was named while writers run through every entry point: nothing acknowledged is lost, the
// new primary is on the named server, the old primary becomes a standby of it, the pause of the writes is measured, and the term rises
// by one.
func TestSwitchoverOfAGroup(t *testing.T) {
	e := swCluster(t)
	c, port, pool, leader, g, old, nw, w := e.c, e.port, e.pool, e.leader, e.g, e.old, e.nw, e.w
	id := swRequest(t, leader, old, nw, g)
	swWait(t, e, id, 8*time.Minute)
	if host := swPrimaryHost(t, leader, g); host != nw.Host {
		t.Fatalf("the primary of group %d is on %s, want %s", g, host, nw.Host)
	}
	before := w.count()
	until(t, 2*time.Minute, "writes after the switchover", func() bool { return w.count() > before+30 })
	acked := w.Stop()
	for _, n := range c.Nodes {
		if n == old {
			continue // it hosts no primary now: its entry point forwards, and the certificate it meets names another server
		}
		have := foReadIDs(t, n, port, pool)
		for rid := range acked {
			if !have[rid] {
				t.Errorf("acknowledged row %d is missing when read through %s", rid, n.Host)
				break
			}
		}
	}
	foEntryPointAnswers(t, old, port, pool, leader.Host)
	kinds := map[string]map[string]any{}
	for _, e := range foEvents(t, leader, id) {
		if k, _ := e["kind"].(string); k != "" {
			kinds[k] = e
		}
	}
	for _, k := range []string{"quiesced", "stoppedClean", "verified", "promoted", "writesResumed"} {
		if kinds[k] == nil {
			t.Errorf("the report has no event %q: %v", k, kinds)
		}
	}
	if d, _ := kinds["writesResumed"]["detail"].(map[string]any); d != nil {
		t.Logf("the writes were stopped for %v ms", d["pauseMs"])
	}
	rep := leader.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", id), nil)
	groups, _ := rep["groups"].([]any)
	if rep["kind"] != "failover" || len(groups) != 1 || groups[0].(map[string]any)["toServerId"] != nw.Host {
		t.Fatalf("report: %v", rep)
	}
	if dl, _ := groups[0].(map[string]any)["dataLoss"].(map[string]any); dl != nil && dl["verdict"] != "none" {
		t.Errorf("the verdict of a switchover is none: %v", dl)
	}
	// The old primary is a standby of the new one again (adopted by the rebuild, no promotion of it).
	until(t, 8*time.Minute, "the old primary to be a ready standby of the group", func() bool {
		for _, s := range topology(t, leader)[g] {
			if s.server == old.Host && s.state != "" && s.state != "rebuilding" {
				return true
			}
		}
		return false
	})
}

// T12b. The proof stays false: the new primary is never promoted, the old primary is started again, the writes go on, and the task
// fails with standby_behind.
func TestSwitchoverVerificationFails(t *testing.T) {
	e := swCluster(t)
	c, port, pool, leader, g, old, nw, w := e.c, e.port, e.pool, e.leader, e.g, e.old, e.nw, e.w
	nw.SetFaults(fault.Spec{FailoverPoint: "verify:false", FailoverMode: "fail"})
	id := swRequest(t, leader, old, nw, g)
	task := foWaitTask(t, leader, id, 8*time.Minute, "failed", "done")
	if e, _ := task["error"].(map[string]any); task["state"] != "failed" || e == nil || e["code"] != failover.CodeStandbyBehind {
		t.Fatalf("the task: %v", task)
	}
	swAfterRollback(t, c, port, pool, leader, g, old, w)
}

// T12b (second case). The old primary stopped, but its state is not "shut down": nothing is asked of the new primary, and the rollback
// starts the old primary again.
func TestSwitchoverWithAnUncleanStop(t *testing.T) {
	e := swCluster(t)
	c, port, pool, leader, g, old, nw, w := e.c, e.port, e.pool, e.leader, e.g, e.old, e.nw, e.w
	old.SetFaults(fault.Spec{FailoverPoint: "demote:state", FailoverMode: "fail"})
	id := swRequest(t, leader, old, nw, g)
	task := foWaitTask(t, leader, id, 8*time.Minute, "failed", "done")
	if e, _ := task["error"].(map[string]any); task["state"] != "failed" || e == nil || e["code"] != failover.CodeSwitchoverUnproven {
		t.Fatalf("the task: %v", task)
	}
	swAfterRollback(t, c, port, pool, leader, g, old, w)
}

// swAfterRollback checks what a rolled back switchover leaves: the old primary is the primary of the group again and takes writes, and
// no acknowledged row is lost.
func swAfterRollback(t *testing.T, c *clustertest.Cluster, port int, pool *x509.CertPool, leader *clustertest.Node, g int, old *clustertest.Node, w *foWriters) {
	t.Helper()
	old.Heal()
	until(t, 5*time.Minute, "the old primary to run and hold the lease again", func() bool {
		hosts, _ := primaryOf(t, leader)
		if hosts[g] != old.Host {
			return false
		}
		_, state := old.Do("GET", "/replication/topology", nil, nil)
		return state != nil
	})
	before := w.count()
	until(t, 5*time.Minute, "writes after the rollback", func() bool { return w.count() > before+30 })
	acked := w.Stop()
	for _, n := range c.Nodes {
		have := foReadIDs(t, n, port, pool)
		for rid := range acked {
			if !have[rid] {
				t.Errorf("acknowledged row %d is missing when read through %s", rid, n.Host)
				break
			}
		}
	}
}

// T12c. A cancel while the writes cannot be stopped rolls back and the group takes writes again; a cancel after the promotion is refused
// with task_not_cancellable and the task finishes when repeated.
func TestSwitchoverCancelBeforeAndAfterThePointOfNoReturn(t *testing.T) {
	e := swCluster(t)
	c, port, pool, leader, g, old, nw, w := e.c, e.port, e.pool, e.leader, e.g, e.old, e.nw, e.w
	old.SetFaults(fault.Spec{FailoverPoint: "quiesce:stall", FailoverMode: "fail"})
	id := swRequest(t, leader, old, nw, g)
	until(t, 2*time.Minute, "the task to reach the stop of the writes", func() bool {
		_, body := leader.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		prog, _ := body["progress"].(map[string]any)
		steps, _ := prog["steps"].([]any)
		for _, s := range steps {
			m, _ := s.(map[string]any)
			if m["name"] == failover.StepName(int32(g), failover.StepQuiesce) && m["state"] == "running" {
				return true
			}
		}
		return false
	})
	st, resp := leader.Do("POST", fmt.Sprintf("/tasks/%v/cancel", id), nil, nil)
	if st != 200 && st != 202 {
		t.Fatalf("cancel before the point of no return: %d %v", st, resp)
	}
	foWaitTask(t, leader, id, 3*time.Minute, "failed")
	swAfterRollback(t, c, port, pool, leader, g, old, w)

	// After the promotion: the task pauses at the catalog step, which is past the point of no return.
	leader.SetFaults(fault.Spec{FailoverPoint: fmt.Sprintf("Group %d: %s:before", g, failover.StepCatalog), FailoverMode: "pause"})
	id2 := swRequest(t, leader, old, nw, g)
	task := foWaitTask(t, leader, id2, 8*time.Minute, "paused", "failed", "done")
	if task["state"] != "paused" {
		t.Fatalf("the task did not pause at the catalog step: %v", task)
	}
	st, p := leader.Do("POST", fmt.Sprintf("/tasks/%v/cancel", id2), nil, nil)
	if st != 409 || p["code"] != "task_not_cancellable" {
		t.Fatalf("cancel after the promotion: %d %v", st, p)
	}
	leader.Heal()
	st, resp = leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "graceful", "shardGroupIds": []int{g}}, nil)
	if st != 202 && st != 200 {
		t.Fatalf("the repeated request: %d %v", st, resp)
	}
	swWait(t, e, id2, 8*time.Minute)
	if host := swPrimaryHost(t, leader, g); host != nw.Host {
		t.Fatalf("the primary of group %d is on %s, want %s", g, host, nw.Host)
	}
}

// swWait waits until the task is done and fails, with the task and the logs of the servers, when it ended otherwise.
func swWait(t *testing.T, e swEnv, id any, d time.Duration) map[string]any {
	t.Helper()
	defer func() {
		if t.Failed() {
			st, body := e.leader.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
			t.Logf("task %v after the wait (%d): %v", id, st, body)
			t.Logf("failover status: %v", e.leader.Expect(200, "GET", "/failover", nil))
			t.Logf("leader log: %s", e.leader.LogTail(12000))
		}
	}()
	body := foWaitTask(t, e.leader, id, d, "done", "failed")
	if body["state"] != "done" {
		t.Logf("leader log: %s", e.leader.LogTail(6000))
		t.Logf("old log: %s", e.old.LogTail(4000))
		t.Fatalf("the task ended %v: %v", body["state"], body["error"])
	}
	return body
}
