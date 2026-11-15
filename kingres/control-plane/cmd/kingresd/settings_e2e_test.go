// SPDX-License-Identifier: AGPL-3.0-only

package main

import "testing"

func TestClusterSettingsUpdate(t *testing.T) {
	e := shared(t)
	before := e.expect(t, 200, "GET", "/settings/cluster", nil)
	out := e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{
		"defaultReplicaCount": 2, "defaultDurability": "syncApply",
		"autoFailover": map[string]any{"enabled": true, "timeoutSeconds": 90},
		"rebalance":    map[string]any{"maxConcurrentMoves": 8},
	})
	valid(t, "updateClusterSettings", "200", out)
	af := out["autoFailover"].(map[string]any)
	if out["defaultReplicaCount"] != float64(2) || out["defaultDurability"] != "syncApply" || af["enabled"] != true ||
		af["timeoutSeconds"] != float64(90) || af["maxCount"] != float64(1) || out["rebalance"].(map[string]any)["maxConcurrentMoves"] != float64(8) {
		t.Errorf("settings %v", out)
	}
	got := e.expect(t, 200, "GET", "/settings/cluster", nil)
	valid(t, "getClusterSettings", "200", got)
	if got["defaultDurability"] != "syncApply" {
		t.Errorf("not persisted: %v", got)
	}
	for _, bad := range []map[string]any{
		{}, {"defaultReplicaCount": 9}, {"defaultDurability": "fast"}, {"autoFailover": map[string]any{}},
		{"autoFailover": map[string]any{"timeoutSeconds": 0}}, {"rebalance": map[string]any{"maxConcurrentMoves": 99}},
		{"rebalance": map[string]any{}}, {"features": map[string]any{"ledger": false}}, {"unknown": 1},
	} {
		if st, o, _ := e.call("PATCH", "/settings/cluster", bad, nil); st < 400 || st >= 500 {
			t.Errorf("%v: %d %v", bad, st, o)
		}
	}
	ops := e.accountWithRole(t, "set_ops", "kingres_ops")
	if st, _, _ := e.as(ops).call("PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 0}, nil); st != 403 {
		t.Errorf("settings update as ops: %d", st)
	}
	e.expect(t, 204, "DELETE", "/roles/set_ops", nil)
	// Put the defaults back for the other tests.
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{
		"defaultReplicaCount": before["defaultReplicaCount"], "defaultDurability": before["defaultDurability"],
		"autoFailover": map[string]any{"enabled": false, "timeoutSeconds": 120},
		"rebalance":    map[string]any{"maxConcurrentMoves": 4},
	})
}
