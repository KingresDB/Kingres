// SPDX-License-Identifier: PostgreSQL

package clustertest_test

import (
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Defect 1: what the control plane recorded about a database (bucket count,
// ledger mode and digest interval, who created it) is the same whichever server
// answers; a follower used to answer null for what only the leader's system
// database holds.
func TestDatabaseRecordedFieldsAreTheSameOnEveryServer(t *testing.T) {
	c := sharedCluster(t)
	var want map[string]any
	for _, n := range c.Nodes {
		db := n.Expect(200, "GET", "/databases/before_db", nil)
		sharding, _ := db["sharding"].(map[string]any)
		ledger, _ := db["ledger"].(map[string]any)
		if sharding["bucketCount"] != float64(32) {
			t.Errorf("node %d: sharding = %v, want bucketCount 32", n.Index, sharding)
		}
		if ledger["mode"] != "optIn" || ledger["digestIntervalSeconds"] == nil {
			t.Errorf("node %d: ledger = %v, want optIn with a digest interval", n.Index, ledger)
		}
		if db["createdBy"] == nil || db["createdAt"] == nil {
			t.Errorf("node %d: createdBy/createdAt = %v / %v", n.Index, db["createdBy"], db["createdAt"])
		}
		if want == nil {
			want = map[string]any{"sharding": sharding, "ledger": ledger}
		}
		if sharding["bucketCount"] != want["sharding"].(map[string]any)["bucketCount"] || ledger["mode"] != want["ledger"].(map[string]any)["mode"] {
			t.Errorf("node %d disagrees with node 0", n.Index)
		}
		list := n.Expect(200, "GET", "/databases", nil)
		for _, d := range items(t, list) {
			if d["name"] == "before_db" {
				if s, _ := d["sharding"].(map[string]any); s["bucketCount"] != float64(32) {
					t.Errorf("node %d: the list says %v", n.Index, s)
				}
			}
		}
		st := n.Expect(200, "GET", "/databases/before_db/ledger", nil)
		if st["mode"] != "optIn" {
			t.Errorf("node %d: ledger status mode = %v", n.Index, st["mode"])
		}
	}
}

// CP2-13: the administrative actions are written by the leader whichever server
// received them; every server answers the audit log of the whole cluster.
func TestAuditShowsTheLeadersActionsOnEveryServer(t *testing.T) {
	c := sharedCluster(t)
	follower, leader := c.Nodes[1], c.Leader()
	listed := func(n *clustertest.Node) []map[string]any {
		body := n.Expect(200, "GET", "/audit?action=createRole&limit=200", nil)
		if body["partial"] != false {
			t.Errorf("node %d: partial = %v", n.Index, body["partial"])
		}
		return items(t, body)
	}
	before := len(listed(follower))
	mkRole(t, follower, "audit_probe", "Passw0rd-audit-probe-1", "kingres_readonly")
	ids := map[float64]bool{}
	for _, n := range c.Nodes {
		got := listed(n)
		if len(got) != before+1 {
			t.Errorf("node %d lists %d createRole events, want %d", n.Index, len(got), before+1)
			continue
		}
		newest := got[0] // newest first
		ids[newest["id"].(float64)] = true
		if newest["serverId"] != leader.Host || newest["actor"] == nil {
			t.Errorf("node %d: the newest createRole event is %v, the leader is %s", n.Index, newest, leader.Host)
		}
	}
	if len(ids) != 1 {
		t.Errorf("the servers disagree on the id of the event: %v", ids)
	}
	// Sign-ins are recorded by the server that took them and are listed everywhere.
	servers := map[any]bool{}
	for _, e := range items(t, follower.Expect(200, "GET", "/audit?action=login&limit=200", nil)) {
		servers[e["serverId"]] = true
	}
	if len(servers) < 1 {
		t.Error("no sign-ins listed")
	}
}
