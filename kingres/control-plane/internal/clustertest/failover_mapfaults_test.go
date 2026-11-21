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

// Cluster tests of the review of failover round 2 (F5-01 to F5-07): faults of the candidate of the map group and of the clean stop.

// mapStopped waits for the old leader's system database to be stopped (the clean stop of the map group's primary), the point from which the rows of the
// old leader cannot be read and the verdict is waited for.
func mapStopped(t *testing.T, leader *clustertest.Node) {
	t.Helper()
	until(t, 5*time.Minute, "the old leader's system database to be stopped", func() bool {
		st, _ := leader.Do("GET", "/cluster", nil, nil)
		return st != 200
	})
}

// mapAcked stops the writers and checks that every acknowledged row can be read through every server.
func mapAcked(t *testing.T, c *clustertest.Cluster, port int, pool *x509.CertPool, w *foWriters) {
	t.Helper()
	before := w.count()
	until(t, 5*time.Minute, "writes after the end", func() bool { return w.count() > before+30 })
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

// mapRolledBack waits for the planned switchover of the map group to be rolled back and checks what a rollback leaves: the task ends failed with the code,
// read from the old leader once its instance is back; the old leader holds the map group again; the standby never became a primary; the writers go on and
// nothing acknowledged is lost.
func mapRolledBack(t *testing.T, c *clustertest.Cluster, port int, pool *x509.CertPool, leader, nw *clustertest.Node, w *foWriters, id any, wait time.Duration) {
	t.Helper()
	var task map[string]any
	defer func() {
		if t.Failed() {
			t.Logf("task %v: %v", id, task)
			t.Logf("old leader log: %s", quietLog(leader.LogTail(60000)))
			t.Logf("candidate log: %s", quietLog(nw.LogTail(60000)))
		}
	}()
	until(t, wait, "the task to end on the old leader once its instance is back", func() bool {
		st, body := leader.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		if st != 200 {
			return false
		}
		if s, _ := body["state"].(string); s == "failed" || s == "done" {
			task = body
			return true
		}
		return false
	})
	if e, _ := task["error"].(map[string]any); task["state"] != "failed" || e == nil {
		t.Fatalf("the task: %v", task)
	}
	nw.Heal()
	until(t, 5*time.Minute, "the old leader to hold the map group again", func() bool {
		hosts, _ := primaryOf(t, leader)
		return hosts[0] == leader.Host
	})
	var st failover.State
	if code, err := leader.InternalCall(nw, "GET", "/failover/state?group=0", nil, &st); err != nil || code != 200 || len(st.OutOfRecovery()) != 0 {
		t.Errorf("the standby of the map group is a primary: %d %v %+v", code, err, st)
	}
	mapAcked(t, c, port, pool, w)
}

// F5-01. The candidate never promotes (its promoting call answers unavailable for as long as the fault lasts) and says nothing: after the old leader's wait
// for the verdict the task rolls back although its rows are unreadable (the system database is the stopped instance): the candidate is let go of and
// given up, and the old leader is started again.
func TestSwitchoverOfTheMapGroupWhoseCandidateNeverPromotes(t *testing.T) {
	c, port, pool, leader, nw, w := mapSwitchoverEnv(t)
	nw.SetFaults(fault.Spec{FailoverPoint: "promote:unavailable", FailoverMode: "unavailable"})
	id := mapSwitchoverRequest(t, leader, nw)
	mapRolledBack(t, c, port, pool, leader, nw, w, id, 16*time.Minute)
}

// F5-02, F5-08. An operator gives up the bootstrap of the candidate that cannot promote: it lets go of its lease and tells the old leader, whose task rolls
// back at once (well before its own wait for a verdict of six minutes), and the old primary is started again.
func TestAbandoningTheBootstrapOfASwitchoverRestartsTheOldLeader(t *testing.T) {
	c, port, pool, leader, nw, w := mapSwitchoverEnv(t)
	nw.SetFaults(fault.Spec{FailoverPoint: "promote:unavailable", FailoverMode: "unavailable"})
	id := mapSwitchoverRequest(t, leader, nw)
	mapStopped(t, leader)
	until(t, 2*time.Minute, "the bootstrap to be given up", func() bool {
		st, body := nw.Do("POST", "/failover/map/abandon", map[string]any{"reason": "the candidate cannot promote"}, nil)
		return st == 200 && body["abandoned"] == true
	})
	mapRolledBack(t, c, port, pool, leader, nw, w, id, 5*time.Minute)
}

// F5-03. An abandon that meets the promoting call in flight (the call holds for 20 s) is either refused (409: it promoted) or given up before anything was
// promoted; in both cases there is exactly one primary of the map group at the end and nothing acknowledged is lost.
func TestAnAbandonWhilePromoteIsInFlightLeavesExactlyOnePrimary(t *testing.T) {
	c, port, pool, leader, nw, w := mapSwitchoverEnv(t)
	nw.SetFaults(fault.Spec{FailoverPoint: "promote:slow", FailoverMode: "slow"})
	id := mapSwitchoverRequest(t, leader, nw)
	mapStopped(t, leader)
	time.Sleep(8 * time.Second) // the proof is sent, the promoting call (held for 20 s) is in flight
	st, body := nw.Do("POST", "/failover/map/abandon", map[string]any{"reason": "while the promotion is in flight"}, nil)
	t.Logf("the abandon during the promotion: %d %v", st, body)
	if st != 200 && st != 409 {
		t.Fatalf("the abandon answered %d %v", st, body)
	}
	defer func() {
		if t.Failed() {
			t.Logf("task %v", id)
			t.Logf("old leader log: %s", quietLog(leader.LogTail(60000)))
			t.Logf("candidate log: %s", quietLog(nw.LogTail(60000)))
		}
	}()
	// the task ends on whichever server holds the system database at the end
	until(t, 12*time.Minute, "the task to end", func() bool {
		for _, n := range []*clustertest.Node{nw, leader} {
			stt, b := n.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
			if stt == 200 {
				if s, _ := b["state"].(string); s == "failed" || s == "done" {
					return true
				}
			}
		}
		return false
	})
	nw.Heal()
	var oldPrimary, newPrimary bool
	until(t, 5*time.Minute, "one primary of the map group", func() bool {
		// the state call is answered to the manager leader only: whichever server leads at the end asks (a server may ask itself)
		stateOf := func(target *clustertest.Node) (failover.State, bool) {
			for _, from := range []*clustertest.Node{nw, leader} {
				var s failover.State
				if code, err := from.InternalCall(target, "GET", "/failover/state?group=0", nil, &s); err == nil && code == 200 {
					return s, true
				}
			}
			return failover.State{}, false
		}
		so, ok1 := stateOf(leader)
		sn, ok2 := stateOf(nw)
		if !ok1 || !ok2 {
			return false
		}
		oldPrimary, newPrimary = len(so.OutOfRecovery()) > 0, len(sn.OutOfRecovery()) > 0
		return oldPrimary != newPrimary
	})
	mapAcked(t, c, port, pool, w)
}

// F5-04. The standby has not replayed the shutdown checkpoint when the promoting call arrives (the fault answers standby_behind five times, ten seconds): the
// bootstrap waits for it as the ordinary groups do and the switchover succeeds.
func TestSwitchoverOfTheMapGroupWaitsForAStandbyThatIsBehind(t *testing.T) {
	_, port, pool, leader, nw, w := mapSwitchoverEnv(t)
	nw.SetFaults(fault.Spec{FailoverPoint: "promote:behind", FailoverMode: "fail", FailoverTimes: 5})
	id := mapSwitchoverRequest(t, leader, nw)
	var task map[string]any
	defer func() {
		if t.Failed() {
			t.Logf("task %v: %v", id, task)
			t.Logf("candidate log: %s", quietLog(nw.LogTail(60000)))
		}
	}()
	until(t, 10*time.Minute, "the task to end on the new leader", func() bool {
		st, body := nw.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		if st != 200 {
			return false
		}
		if s, _ := body["state"].(string); s == "failed" || s == "done" {
			task = body
			return true
		}
		return false
	})
	if task["state"] != "done" {
		t.Fatalf("the task ended %v: %v", task["state"], task["error"])
	}
	hosts, _ := primaryOf(t, nw)
	if hosts[0] != nw.Host {
		t.Fatalf("the primary of the map group is on %s, want %s", hosts[0], nw.Host)
	}
	before := w.count()
	until(t, 3*time.Minute, "writes after the switchover", func() bool { return w.count() > before+30 })
	acked := w.Stop()
	have := foReadIDs(t, nw, port, pool)
	for rid := range acked {
		if !have[rid] {
			t.Errorf("acknowledged row %d is missing when read through %s", rid, nw.Host)
			break
		}
	}
}

// F5-07. pg_controldata cannot be read after the clean stop of an ordinary group: the old primary is stopped and no answer can be given, the renewal of its
// lease ends, the task rolls back, and the old primary is started again by its lease manager and takes writes.
func TestSwitchoverWhoseControlDataCannotBeReadRollsBack(t *testing.T) {
	e := swCluster(t)
	c, port, pool, leader, g, old, nw, w := e.c, e.port, e.pool, e.leader, e.g, e.old, e.nw, e.w
	old.SetFaults(fault.Spec{FailoverPoint: "demote:controldata", FailoverMode: "fail"})
	id := swRequest(t, leader, old, nw, g)
	task := foWaitTask(t, leader, id, 8*time.Minute, "failed", "done")
	if e, _ := task["error"].(map[string]any); task["state"] != "failed" || e == nil {
		t.Fatalf("the task: %v", task)
	}
	swAfterRollback(t, c, port, pool, leader, g, old, w)
}
