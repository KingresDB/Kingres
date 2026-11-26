// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// The restore of the preferred primaries and the plan of a switchover (increment 7, round 2 of ha-phase5-part2.md, section 5).

// T12. A group is handed to the standby that was named and handed back by the restore, while writers run through every entry point: nothing
// acknowledged is lost, the plan of each request is answered without a change, the restore finds the one group that is not at home, and a
// second restore has nothing to do.
func TestSwitchoverAndBack(t *testing.T) {
	e := swCluster(t)
	c, port, pool, leader, g, old, nw, w := e.c, e.port, e.pool, e.leader, e.g, e.old, e.nw, e.w
	var plan map[string]any
	// The warnings about the lease of every server are logged on every end of the test (the release of the repeated stop that answers refused is one of them),
	// so that a run that passes shows them as well.
	defer func() {
		for _, n := range c.Nodes {
			t.Logf("lease warnings of %s: %s", n.Host, leaseLines(n.LogTail(8000000)))
		}
	}()
	defer func() {
		if t.Failed() {
			t.Logf("the last plan: %v", plan)
			t.Logf("the catalog and the standby records: %s", foCatalogDump(leader))
			t.Logf("log of the old primary's server: %s", old.LogTail(9000))
			for _, f := range []string{"pgdata/pg_hba.conf", "pgdata/pg_ident.conf", "kingres-role.json"} {
				b, _ := os.ReadFile(filepath.Join(old.DataDir, "instances", "1", f))
				t.Logf("%s of the old primary: %s", f, b)
			}
		}
	}()
	// The plan of the switchover: the steps, the chosen standby and the pause of the writes, and nothing changes.
	st, plan := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "graceful", "shardGroupIds": []int{g}, "dryRun": true,
		"targets": []map[string]any{{"shardGroupId": g, "serverId": nw.Host}}}, nil)
	if st != 200 || plan["mode"] != "graceful" || plan["canProceed"] != true {
		t.Fatalf("the plan of the switchover: %d %v", st, plan)
	}
	groups, _ := plan["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("the plan has %d groups: %v", len(groups), plan)
	}
	gp := groups[0].(map[string]any)
	if gp["toServerId"] != nw.Host || gp["writesPausedSeconds"] == nil {
		t.Fatalf("the plan of the group: %v", gp)
	}
	if status := leader.Expect(200, "GET", "/failover", nil); status["state"] != "idle" {
		t.Fatalf("a dry run left a task: %v", status)
	}
	// Nothing is at home to restore yet.
	st, p := leader.Do("POST", "/replication/restore-preferred-primaries", nil, nil)
	if st != 422 {
		t.Fatalf("a restore with every group at home: %d %v", st, p)
	}
	id := swRequest(t, leader, old, nw, g)
	swWait(t, e, id, 8*time.Minute)
	if host := swPrimaryHost(t, leader, g); host != nw.Host {
		t.Fatalf("the primary of group %d is on %s, want %s", g, host, nw.Host)
	}
	// The old primary must be a standby of the group again before it can be restored to.
	until(t, 8*time.Minute, "the old primary to be a ready standby of the group", func() bool {
		for _, s := range topology(t, leader)[g] {
			if s.server == old.Host && s.state != "" && s.state != "rebuilding" {
				return true
			}
		}
		return false
	})
	// The plan of the restore. The old primary is a ready standby that is registered in the catalog only after its rebuild has been
	// noticed: until then the group is skipped (preferredNotReady), so the plan is asked until it lists the group.
	until(t, 5*time.Minute, "the plan of the restore to list the group, to be possible and to show the standby caught up", func() bool {
		st, plan = leader.Do("POST", "/replication/restore-preferred-primaries", map[string]any{"dryRun": true}, nil)
		gs, _ := plan["groups"].([]any)
		if st != 200 || len(gs) != 1 || plan["canProceed"] != true {
			return false
		}
		// The old primary has only just become a standby: the restore stops the new primary and waits for the standby to replay its position (a few
		// seconds); a standby that is still behind makes the switchover of the group refuse (standby_behind), the task ends done with the outcome partial and
		// the primary stays where it is. So the restore is requested only when the plan shows no lag.
		cands, _ := gs[0].(map[string]any)["candidates"].([]any)
		for _, c := range cands {
			cm, _ := c.(map[string]any)
			if cm["chosen"] == true {
				lag, _ := cm["lagBytes"].(float64)
				return cm["readable"] == true && lag == 0
			}
		}
		return false
	})
	if plan["kind"] != "restorePreferredPrimaries" || plan["mode"] != "restore" || plan["serverId"] != nil || plan["canProceed"] != true {
		t.Fatalf("the plan of the restore: %v", plan)
	}
	groups, _ = plan["groups"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["fromServerId"] != nw.Host || groups[0].(map[string]any)["toServerId"] != old.Host {
		t.Fatalf("the plan of the restore names %v", groups)
	}
	// The restore.
	var rid any
	until(t, 5*time.Minute, "the restore to be accepted", func() bool {
		st, resp := leader.Do("POST", "/replication/restore-preferred-primaries", nil, nil)
		if st == 202 {
			rid = clustertest.TaskID(resp)
			return true
		}
		if st != 409 && st != 503 {
			t.Fatalf("restore request: %d %v\n%s", st, resp, leader.LogTail(3000))
		}
		return false
	})
	defer func() {
		if t.Failed() {
			// A restore whose group switchover was refused or rolled back ends done with the outcome partial: the task and the status say which code.
			_, body := leader.Do("GET", fmt.Sprintf("/tasks/%v", rid), nil, nil)
			t.Logf("the restore task %v: %v", rid, body)
			t.Logf("the failover status after the restore: %v", leader.Expect(200, "GET", "/failover", nil))
			_, rep := leader.Do("GET", fmt.Sprintf("/failover/reports/%v", rid), nil, nil)
			t.Logf("the report of the restore (the code of each group that failed): %v", rep)
			for _, n := range c.Nodes {
				t.Logf("the lease and stop lines of %s: %s", n.Host, leaseLines(n.LogTail(8000000)))
			}
		}
	}()
	swWait(t, e, rid, 8*time.Minute)
	if host := swPrimaryHost(t, leader, g); host != old.Host {
		t.Fatalf("after the restore the primary of group %d is on %s, want %s", g, host, old.Host)
	}
	before := w.count()
	until(t, 2*time.Minute, "writes after the restore", func() bool { return w.count() > before+30 })
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
	rep := leader.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", rid), nil)
	rg, _ := rep["groups"].([]any)
	if rep["kind"] != "restorePreferredPrimaries" || rep["mode"] != "restore" || len(rg) != 1 || rg[0].(map[string]any)["toServerId"] != old.Host || rep["serverId"] != nil {
		t.Fatalf("report: %v", rep)
	}
	task := leader.Expect(200, "GET", fmt.Sprintf("/tasks/%v", rid), nil)
	if task["kind"] != "replication.restorePreferredPrimaries" {
		t.Fatalf("the kind of the task: %v", task["kind"])
	}
	// Everything is at home again.
	st, p = leader.Do("POST", "/replication/restore-preferred-primaries", nil, nil)
	if st != 422 {
		t.Fatalf("a second restore: %d %v", st, p)
	}
}

// T12 (status). While a switchover stands at a step, GET /failover names the phase of the group, and the time since its writes stopped.
func TestSwitchoverStatusNamesThePhaseAndThePause(t *testing.T) {
	e := swCluster(t)
	leader, g, old, nw := e.leader, e.g, e.old, e.nw
	leader.SetFaults(fault.Spec{FailoverPoint: fmt.Sprintf("Group %d: %s:before", g, failover.StepStopClean), FailoverMode: "pause"})
	id := swRequest(t, leader, old, nw, g)
	task := foWaitTask(t, leader, id, 8*time.Minute, "paused", "failed", "done")
	if task["state"] != "paused" {
		t.Fatalf("the task did not pause before the clean stop: %v", task)
	}
	status := leader.Expect(200, "GET", "/failover", nil)
	groups, _ := status["groups"].([]any)
	if status["state"] != "paused" || len(groups) != 1 {
		t.Fatalf("status: %v", status)
	}
	gp := groups[0].(map[string]any)
	if gp["phase"] != "stopping" || gp["writesPausedSince"] == nil {
		t.Fatalf("the group while the writes are stopped: %v", gp)
	}
	// The repeated request goes on; the phase and the pause are gone when the group is done.
	leader.Heal()
	st, resp := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "graceful", "shardGroupIds": []int{g}}, nil)
	if st != 202 && st != 200 {
		t.Fatalf("the repeated request: %d %v", st, resp)
	}
	swWait(t, e, id, 8*time.Minute)
	status = leader.Expect(200, "GET", "/failover", nil)
	if status["state"] != "idle" {
		t.Fatalf("status after the switchover: %v", status)
	}
	e.w.Stop()
}
