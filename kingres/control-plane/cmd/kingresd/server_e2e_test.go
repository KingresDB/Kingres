// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"testing"
)

func TestThisServer(t *testing.T) {
	e := shared(t)
	info := e.expect(t, 200, "GET", "/this-server", nil)
	valid(t, "getThisServer", "200", info)
	if info["nodeState"] != "clustered" || info["isManagerLeader"] != true || info["postgresVersion"] == nil {
		t.Errorf("this server: %v", info)
	}
	res := e.expect(t, 200, "GET", "/this-server/resources", nil)
	valid(t, "getThisServerResources", "200", res)
	if res["cpu"].(map[string]any)["cores"] == nil || res["memory"].(map[string]any)["totalBytes"] == nil {
		t.Errorf("resources on Linux must be measured: %v", res)
	}
	if d := res["disks"].([]any); len(d) != 1 || d[0].(map[string]any)["totalBytes"] == nil {
		t.Errorf("disks: %v", d)
	}
	// The instance log: entries are parsed, newest first, filters apply, the
	// result is capped by the page size.
	logs := e.expect(t, 200, "GET", "/this-server/logs?limit=5", nil)
	valid(t, "listThisServerLogs", "200", logs)
	items := logs["items"].([]any)
	if len(items) == 0 || len(items) > 5 {
		t.Errorf("log entries: %d", len(items))
	}
	none := e.expect(t, 200, "GET", "/this-server/logs?component=api", nil)
	if len(none["items"].([]any)) != 0 {
		t.Errorf("component filter: %v", none)
	}
	cl := e.expect(t, 200, "GET", "/logs?limit=2", nil)
	valid(t, "listLogs", "200", cl)
	for _, bad := range []string{"/this-server/logs?level=loud", "/this-server/logs?path=/etc/passwd", "/this-server/logs?limit=0"} {
		if st, _, _ := e.call("GET", bad, nil, nil); st != 400 {
			t.Errorf("%s: %d", bad, st)
		}
	}
	// Restart as a task; the control plane keeps working afterwards.
	e.mustTask(t, "POST", "/this-server/instances/1/restart", nil)
	e.expect(t, 200, "GET", "/cluster", nil)
	if st, _, _ := e.call("POST", "/this-server/instances/2/restart", nil, nil); st != 404 {
		t.Errorf("unknown instance: %d", st)
	}
	if st, _, _ := e.call("POST", "/this-server/instances/..%2F..%2Fetc/restart", nil, nil); st < 400 {
		t.Errorf("traversal in the instance id: %d", st)
	}
	ro := e.accountWithRole(t, "srv_ro", "kingres_readonly")
	if st, _, _ := e.as(ro).call("POST", "/this-server/instances/1/restart", nil, nil); st != 403 {
		t.Errorf("restart as readonly: %d", st)
	}
	e.expect(t, 204, "DELETE", "/roles/srv_ro", nil)
}
