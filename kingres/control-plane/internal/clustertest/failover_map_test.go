// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"crypto/x509"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// The failover of the map group (increment 5 of ha-phase5-part2.md): the manager leader and the primary of the map group are lost
// together, an operator asks a surviving server, the standby of the map group is promoted by the bootstrap B1 to B8 and the engine
// of the new leader finishes the task.

type mapCluster struct {
	c            *clustertest.Cluster
	port         int
	pool         *x509.CertPool
	old, cand    *clustertest.Node
	other        *clustertest.Node
	survivors    []*clustertest.Node
	standbyGroup int
}

func newMapCluster(t *testing.T) *mapCluster { return newMapClusterWith(t) }

// newMapClusterWith is newMapCluster with more flags for every kingresd.
func newMapClusterWith(t *testing.T, extra ...string) *mapCluster {
	t.Helper()
	c, port, pool := foClusterWith(t, append([]string{"-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s"}, extra...)...)
	m := &mapCluster{c: c, port: port, pool: pool, old: c.Leader()}
	for _, n := range catalogStandbys(t, m.old) {
		if n.group == 0 {
			m.cand = nodeOf(c, n.host)
		}
	}
	if m.cand == nil || m.cand == m.old {
		t.Fatalf("no standby of the map group away from the leader")
	}
	for _, n := range c.Nodes {
		if n != m.old && n != m.cand {
			m.other = n
		}
	}
	m.survivors = []*clustertest.Node{m.cand, m.other}
	waitFacts(t, m.old, 0)
	return m
}

func mapLogin(t *testing.T, n *clustertest.Node) {
	t.Helper()
	defer func() {
		if t.Failed() {
			t.Logf("log of %s: %s", n.Host, n.LogTail(6000))
		}
	}()
	until(t, 2*time.Minute, "sign-in on "+n.Host, func() bool { return n.Login(clustertest.AdminUser, clustertest.AdminPass) })
}

// request asks a surviving server for the failover of the old leader's server and returns the answer.
func (m *mapCluster) request(t *testing.T, via *clustertest.Node) map[string]any {
	t.Helper()
	mapLogin(t, via)
	var st int
	var resp map[string]any
	until(t, 2*time.Minute, "the request to be accepted", func() bool {
		st, resp = via.Do("POST", "/servers/"+m.old.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true, "comment": "map group"}, nil)
		return st != 503
	})
	if st != 202 {
		t.Fatalf("request for the failover of the map group: %d %v", st, resp)
	}
	if id, _ := resp["id"].(float64); id != 0 {
		t.Fatalf("the answer must be the task with id 0 while none is recorded: %v", resp)
	}
	return resp
}

// recordedTask waits until the candidate names the recorded task.
func (m *mapCluster) recordedTask(t *testing.T) any {
	t.Helper()
	mapLogin(t, m.cand)
	defer func() {
		if t.Failed() {
			t.Logf("log of %s: %s", m.cand.Host, m.cand.LogTail(8000))
		}
	}()
	var id any
	until(t, 10*time.Minute, "the task of the failover to be recorded", func() bool {
		st, body := m.cand.Do("GET", "/failover", nil, nil)
		if st == 200 && body["taskId"] != nil {
			id = body["taskId"]
			return true
		}
		return false
	})
	return id
}

// T6. The map group's primary is lost with the leader: the failover goes on, the task is recorded and finishes on the new leader,
// no acknowledged row is lost, and the new leader says that it has no key of the certificate authority.
func TestMapGroupPrimaryLost(t *testing.T) {
	m := newMapCluster(t)
	w := startFoWriters(m.survivors, m.port, m.pool)
	until(t, time.Minute, "rows acknowledged before the loss", func() bool { return w.count() > 40 })
	foKillServer(m.old)
	m.request(t, m.other)
	id := m.recordedTask(t)
	m.cand.WaitTaskDone(id, 12*time.Minute)
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
	rep := m.cand.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", id), nil)
	groups, _ := rep["groups"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["toServerId"] != m.cand.Host {
		t.Fatalf("report: %v", rep)
	}
	// T6c: the key of the certificate authority is not escrowed: the new leader holds none and says so.
	until(t, 2*time.Minute, "the alert about the certificate authority", func() bool {
		body := m.cand.Expect(200, "GET", "/alerts", nil)
		items, _ := body["items"].([]any)
		for _, it := range items {
			if a, _ := it.(map[string]any); a["kind"] == "certificateAuthorityUnavailable" {
				return true
			}
		}
		return false
	})
}

// T5b. kingresd of the candidate ends after the standby was promoted and before the catalog followed: a restart reads the journal and
// goes on (B3 and B5 are done again for the lease, which lived in the memory of the process), without a second promotion.
func TestMapGroupBootstrapSurvivesKingresdEnding(t *testing.T) {
	m := newMapCluster(t)
	m.cand.SetFaults(fault.Spec{FailoverPoint: "map:B6 make the catalog follow:before", FailoverMode: "exit"})
	foKillServer(m.old)
	m.request(t, m.other)
	until(t, 5*time.Minute, "kingresd of the candidate to end at the crash point", func() bool { st, _ := m.cand.Do("GET", "/healthz", nil, nil); return st != 200 })
	m.cand.Kill()
	m.cand.Heal()
	m.cand.Start()
	id := m.recordedTask(t)
	m.cand.WaitTaskDone(id, 12*time.Minute)
	promoted := 0
	for _, e := range foEvents(t, m.cand, id) {
		if msg, _ := e["message"].(string); e["kind"] == "promoted" && strings.Contains(msg, `"status": "promoted"`) {
			promoted++
		}
	}
	if promoted != 1 {
		t.Fatalf("the standby was promoted %d times", promoted)
	}
	rep := m.cand.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", id), nil)
	if groups, _ := rep["groups"].([]any); len(groups) != 1 {
		t.Fatalf("report: %v", rep)
	}
}

// T6b. The engine gate stays closed until B8: while the bootstrap holds after B7, the task is recorded and pending, and the engine of
// the new leader claims nothing; when the hold ends the task runs and finishes.
func TestMapGroupEngineGateStaysClosedUntilTheBootstrapEnds(t *testing.T) {
	m := newMapCluster(t)
	m.cand.SetFaults(fault.Spec{FailoverPoint: "map:B7 make the new leader:after", FailoverMode: "hold"})
	foKillServer(m.old)
	m.request(t, m.other)
	id := m.recordedTask(t)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st, body := m.cand.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		if st == 200 && body["state"] != "pending" {
			t.Fatalf("the engine ran the task while the gate is closed: %v", body)
		}
		time.Sleep(time.Second)
	}
	m.cand.Heal()
	m.cand.WaitTaskDone(id, 12*time.Minute)
}
