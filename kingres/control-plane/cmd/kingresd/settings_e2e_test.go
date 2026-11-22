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

// The data transfer setting (contract 0.19.0): off by default, changed by an administrator only, validated as a whole, persisted, and
// audited with the old and the new value.
func TestClusterSettingsDataTransfer(t *testing.T) {
	e := shared(t)
	before := e.expect(t, 200, "GET", "/settings/cluster", nil)
	valid(t, "getClusterSettings", "200", before)
	dt, _ := before["dataTransfer"].(map[string]any)
	if dt == nil || dt["enabled"] != false || dt["retentionHours"] != float64(24) || dt["maxConcurrent"] != float64(2) || dt["maxFileBytes"] != float64(20<<30) {
		t.Fatalf("defaults: %v", before["dataTransfer"])
	}
	out := e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": true, "retentionHours": 48, "stagingQuotaBytes": 5 << 30}})
	valid(t, "updateClusterSettings", "200", out)
	dt = out["dataTransfer"].(map[string]any)
	if dt["enabled"] != true || dt["retentionHours"] != float64(48) || dt["stagingQuotaBytes"] != float64(5<<30) || dt["maxConcurrent"] != float64(2) {
		t.Errorf("after the update: %v", dt)
	}
	got := e.expect(t, 200, "GET", "/settings/cluster", nil)
	valid(t, "getClusterSettings", "200", got)
	if got["dataTransfer"].(map[string]any)["retentionHours"] != float64(48) {
		t.Errorf("not persisted: %v", got["dataTransfer"])
	}
	// The other settings were not touched by it.
	if got["defaultDurability"] != before["defaultDurability"] {
		t.Errorf("another setting changed: %v", got)
	}
	for _, bad := range []map[string]any{
		{"dataTransfer": map[string]any{}}, {"dataTransfer": map[string]any{"retentionHours": 0}}, {"dataTransfer": map[string]any{"retentionHours": 169}},
		{"dataTransfer": map[string]any{"maxFileBytes": 10}}, {"dataTransfer": map[string]any{"maxConcurrent": 17}},
		{"dataTransfer": map[string]any{"enabled": "yes"}}, {"dataTransfer": map[string]any{"unknown": 1}},
		// A request with one valid and one invalid member stores nothing.
		{"dataTransfer": map[string]any{"enabled": false, "maxConcurrent": 0}},
	} {
		if st, o, _ := e.call("PATCH", "/settings/cluster", bad, nil); st != 400 {
			t.Errorf("%v: %d %v", bad, st, o)
		}
	}
	if again := e.expect(t, 200, "GET", "/settings/cluster", nil); again["dataTransfer"].(map[string]any)["enabled"] != true {
		t.Errorf("a refused request changed the setting: %v", again["dataTransfer"])
	}
	for _, role := range []string{"kingres_ops", "kingres_readonly"} {
		acct := e.accountWithRole(t, "dt_"+role[len("kingres_"):], role)
		if st, _, _ := e.as(acct).call("PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": false}}, nil); st != 403 {
			t.Errorf("%s turned the data transfer off: %d", role, st)
		}
		// Reading the setting needs only kingres_readonly.
		if st, _, _ := e.as(acct).call("GET", "/settings/cluster", nil, nil); st != 200 {
			t.Errorf("%s reads the settings: %d", role, st)
		}
		e.expect(t, 204, "DELETE", "/roles/dt_"+role[len("kingres_"):], nil)
	}
	// The audit event names the old and the new value, and nothing else of the data transfer.
	ev := e.expect(t, 200, "GET", "/audit?action=updateClusterSettings", nil)
	items, _ := ev["items"].([]any)
	found := false
	for _, it := range items {
		d, _ := it.(map[string]any)["details"].(map[string]any)
		if x, ok := d["dataTransfer"].(map[string]any); ok {
			found = true
			if x["old"].(map[string]any)["enabled"] != false || x["new"].(map[string]any)["enabled"] != true {
				t.Errorf("audit details: %v", x)
			}
			break
		}
	}
	if !found {
		t.Errorf("no audit event with the data transfer values: %v", items)
	}
	// Put the defaults back for the other tests.
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": false, "retentionHours": 24, "maxFileBytes": 20 << 30, "stagingQuotaBytes": 100 << 30, "maxConcurrent": 2}})
}
