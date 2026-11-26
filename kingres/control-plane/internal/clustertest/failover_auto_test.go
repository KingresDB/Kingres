// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Automatic failover (increment 6 of ha-phase5-part2.md, section 4): the loop on the manager leader, the counter, the limit and its alert,
// and the candidate monitor of the map group.

func autoSettingsOf(t *testing.T, n *clustertest.Node) map[string]any {
	t.Helper()
	s := n.Expect(200, "GET", "/settings/cluster", nil)
	a, _ := s["autoFailover"].(map[string]any)
	return a
}

func autoEnable(t *testing.T, n *clustertest.Node, max int) {
	t.Helper()
	n.Expect(200, "PATCH", "/settings/cluster", map[string]any{"autoFailover": map[string]any{"enabled": true, "timeoutSeconds": 30, "maxCount": max}})
}

// autoReports returns the reports of failovers with the given trigger.
func autoReports(t *testing.T, n *clustertest.Node, trigger string) []map[string]any {
	t.Helper()
	var out []map[string]any
	// A 503 (the system database is moving with the failover that was just made) is an answer to ask again, not a failure.
	st, body := n.Do("GET", "/failover/reports", nil, nil)
	if st != 200 {
		return nil
	}
	for _, r := range items(t, body) {
		if r["trigger"] == trigger {
			out = append(out, r)
		}
	}
	return out
}

// T13. With the setting off a dead server is left alone, however long it is silent. Turned on, the loop fails it over with the trigger
// automatic, the counter is 1, the limit of 1 raises autoFailoverLimitReached, and the reset clears the counter and the alert. The writers
// lose no acknowledged row.
func TestAutomaticFailoverRulesAndTheCounter(t *testing.T) {
	c, port, pool := foCluster(t)
	leader := c.Leader()
	g, old, _ := foVictim(t, c)
	if a := autoSettingsOf(t, leader); a["enabled"] != false {
		t.Fatalf("automatic failover must start off: %v", a)
	}
	// The timeout is short but the setting stays off: only the switch holds the loop back.
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"autoFailover": map[string]any{"enabled": false, "timeoutSeconds": 30, "maxCount": 1}})
	waitFacts(t, leader, g)
	var survivors []*clustertest.Node
	for _, n := range c.Nodes {
		if n != old {
			survivors = append(survivors, n)
		}
	}
	w := startFoWriters(survivors, port, pool)
	until(t, time.Minute, "rows acknowledged before the loss", func() bool { return w.count() > 40 })
	foKillServer(old)
	// Off: nothing happens for longer than the effective timeout and the lease together.
	time.Sleep(60 * time.Second)
	if r := autoReports(t, leader, "automatic"); len(r) != 0 {
		t.Fatalf("a failover was made with the setting off: %v", r)
	}
	// Not even a decision: the loop logs every decision before it stops a rebalance or starts the task (a refused start would hide a report).
	if strings.Contains(leader.LogTail(4000000), `"msg":"automatic failover"`) {
		t.Fatalf("the loop decided to fail a server over with the setting off: %s", leader.LogTail(3000))
	}
	autoEnable(t, leader, 1)
	until(t, time.Minute, "the decision in the log of the leader", func() bool { return strings.Contains(leader.LogTail(4000000), `"msg":"automatic failover"`) })
	var rep map[string]any
	until(t, 5*time.Minute, "the automatic failover to end", func() bool {
		r := autoReports(t, leader, "automatic")
		if len(r) == 0 {
			return false
		}
		rep = r[0]
		return rep["outcome"] == "completed" || rep["outcome"] == "completedWithWarnings"
	})
	if rep["requestedBy"] != "system" {
		t.Errorf("an automatic failover is requested by system: %v", rep)
	}
	a := autoSettingsOf(t, leader)
	if n, _ := a["eventsSinceReset"].(float64); n != 1 {
		t.Errorf("the counter after one automatic failover: %v", a)
	}
	waitAlert(t, leader, "autoFailoverTriggered", true, 2*time.Minute)
	waitAlert(t, leader, "autoFailoverLimitReached", true, 2*time.Minute)
	before := w.count()
	until(t, 2*time.Minute, "writes after the failover", func() bool { return w.count() > before+30 })
	acked := w.Stop()
	for _, n := range survivors {
		have := foReadIDs(t, n, port, pool)
		for rid := range acked {
			if !have[rid] {
				t.Errorf("acknowledged row %d is missing when read through %s", rid, n.Host)
				break
			}
		}
	}
	out := leader.Expect(200, "POST", "/settings/cluster/auto-failover/reset", nil)
	if af, _ := out["autoFailover"].(map[string]any); af["eventsSinceReset"] != float64(0) {
		t.Errorf("the reset answers the settings: %v", out)
	}
	waitAlert(t, leader, "autoFailoverLimitReached", false, 2*time.Minute)
	until(t, time.Minute, "the audit event of the automatic failover", func() bool {
		return strings.Contains(auditActions(t, leader), `"autoFailover"`)
	})
}

// The map group: the leader and the primary of the map group are lost together, nobody asks, and the candidate monitor on the server
// that holds the standby of the map group starts the bootstrap after the silence and the majority's agreement.
func TestAutomaticFailoverOfTheMapGroup(t *testing.T) {
	m := newMapCluster(t)
	mapLogin(t, m.old)
	autoEnable(t, m.old, 2)
	// The settings reach the replica of the system database that the monitor reads.
	time.Sleep(5 * time.Second)
	w := startFoWriters(m.survivors, m.port, m.pool)
	until(t, time.Minute, "rows acknowledged before the loss", func() bool { return w.count() > 40 })
	foKillServer(m.old)
	id := m.recordedTask(t)
	m.cand.WaitTaskDone(id, 12*time.Minute)
	rep := m.cand.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", id), nil)
	if rep["trigger"] != "automatic" {
		t.Fatalf("report: %v", rep)
	}
	until(t, 3*time.Minute, "the counter", func() bool {
		n, _ := autoSettingsOf(t, m.cand)["eventsSinceReset"].(float64)
		return n == 1
	})
	before := w.count()
	until(t, 2*time.Minute, "writes after the failover", func() bool { return w.count() > before+30 })
	acked := w.Stop()
	for _, n := range m.survivors {
		have := foReadIDs(t, n, m.port, m.pool)
		for rid := range acked {
			if !have[rid] {
				t.Errorf("acknowledged row %d is missing when read through %s", rid, n.Host)
				break
			}
		}
	}
}
