// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import "testing"

// Regression tests for the mismatches between the server and the contract that
// the web panel's end-to-end run found against the real server. Each runs
// against a real one-server cluster, where the system database kingres_system
// exists.

func TestLedgerStatusListAndUnfilteredGrants(t *testing.T) {
	e := shared(t)
	out := e.expect(t, 200, "GET", "/ledger/status", nil)
	valid(t, "listLedgerStatus", "200", out)
	g := e.expect(t, 200, "GET", "/grants", nil)
	valid(t, "listGrants", "200", g)
}

func TestDatabaseCountMatchesTheList(t *testing.T) {
	e := shared(t)
	list := e.expect(t, 200, "GET", "/databases", nil)
	sum := e.expect(t, 200, "GET", "/cluster/summary", nil)
	valid(t, "getClusterSummary", "200", sum)
	n := len(list["items"].([]any))
	for _, it := range list["items"].([]any) {
		if it.(map[string]any)["name"] == "kingres_system" {
			t.Error("the system database is listed")
		}
	}
	cl := e.expect(t, 200, "GET", "/cluster", nil)
	if got := int(cl["databaseCount"].(float64)); got != n {
		t.Errorf("cluster.databaseCount = %d, GET /databases lists %d", got, n)
	}
	if sc, ok := sum["databaseCount"]; ok {
		if got := int(sc.(float64)); got != n {
			t.Errorf("summary.databaseCount = %d, GET /databases lists %d", got, n)
		}
	}
}

func TestLedgerTableClassIsNotCombinedWithPlain(t *testing.T) {
	e := shared(t)
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "clsdb", "sharding": map[string]any{"enabled": false}, "ledger": map[string]any{"mode": "optIn"}})
	e.expect(t, 200, "POST", "/query", map[string]any{"database": "clsdb", "sql": "create table public.led (id bigint primary key)"})
	e.expect(t, 200, "PUT", "/databases/clsdb/schemas/public/tables/led/ledger", map[string]any{"mode": "appendOnly"})
	tbl := e.expect(t, 200, "GET", "/databases/clsdb/schemas/public/tables/led", nil)
	valid(t, "getTable", "200", tbl)
	classes := tbl["classes"].([]any)
	if len(classes) != 1 || classes[0] != "ledger" {
		t.Errorf("classes = %v, want [ledger]", classes)
	}
}

func TestOneServerClusterWithoutDistributedTablesIsHealthy(t *testing.T) {
	e := shared(t)
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "emptyshard", "sharding": map[string]any{"enabled": true, "bucketCount": 8}})
	cl := e.expect(t, 200, "GET", "/cluster", nil)
	servers := e.expect(t, 200, "GET", "/servers", nil)
	for _, it := range servers["items"].([]any) {
		if st := it.(map[string]any)["status"]; st != "live" {
			t.Errorf("server status = %v, want live", st)
		}
	}
	if cl["health"] != "healthy" {
		t.Errorf("cluster health = %v, want healthy", cl["health"])
	}
}

func TestManagerLeaderAgreesInOneServerCluster(t *testing.T) {
	e := shared(t)
	cl := e.expect(t, 200, "GET", "/cluster", nil)
	this := e.expect(t, 200, "GET", "/this-server", nil)
	servers := e.expect(t, 200, "GET", "/servers", nil)
	ml, _ := cl["managerLeader"].(map[string]any)
	if ml == nil {
		t.Fatalf("managerLeader is null: %v", cl)
	}
	leaders := 0
	for _, it := range servers["items"].([]any) {
		sv := it.(map[string]any)
		if sv["isManagerLeader"] == true {
			leaders++
			if sv["id"] != ml["serverId"] {
				t.Errorf("server %v is leader, managerLeader = %v", sv["id"], ml["serverId"])
			}
		}
	}
	if leaders != 1 || this["isManagerLeader"] != true || this["serverId"] != ml["serverId"] {
		t.Errorf("leaders=%d this=%v managerLeader=%v", leaders, this, ml)
	}
}
