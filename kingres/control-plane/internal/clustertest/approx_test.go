// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Approximate aggregates (contract 0.18.0, task database.approximateAggregates): the extension kingres_sketch is installed in the database on every
// server of a cluster of three, the version and the known-answer hash are checked, the setting is recorded and read back, a server that is down
// makes the task wait, a disabled setting leaves the extension installed, and the roles see what the contract says.

func apSketchVersion(t *testing.T, n *clustertest.Node, db string) string {
	t.Helper()
	var v *string
	err := n.ServicePG(db).QueryRow(context.Background(), `SELECT (SELECT extversion FROM pg_extension WHERE extname = 'kingres_sketch')`).Scan(&v)
	if err != nil {
		t.Fatalf("node %d: %v", n.Index, err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func apStepNames(task map[string]any) []string {
	prog, _ := task["progress"].(map[string]any)
	steps, _ := prog["steps"].([]any)
	var names []string
	for _, s := range steps {
		names = append(names, s.(map[string]any)["name"].(string))
	}
	return names
}

func TestApproximateAggregates(t *testing.T) {
	c := clustertest.FormWith(t, 3, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "ap", "sharding": map[string]any{"enabled": true, "bucketCount": 12}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader := c.Leader()
	path := "/databases/ap/approximate-aggregates"

	t.Run("a database that never had it reads as off, on every server", func(t *testing.T) {
		got := leader.Expect(200, "GET", path, nil)
		if got["enabled"] != false || got["countDistinct"] != "exact" || got["errorRate"] != nil || got["extensionVersion"] != nil {
			t.Fatalf("%v", got)
		}
		servers, _ := got["servers"].([]any)
		if len(servers) != 3 {
			t.Fatalf("servers: %v", got["servers"])
		}
		for _, s := range servers {
			if st := s.(map[string]any)["state"]; st != "notInstalled" {
				t.Errorf("server %v: %v", s.(map[string]any)["serverId"], st)
			}
		}
		db := leader.Expect(200, "GET", "/databases/ap", nil)
		if aa, _ := db["approximateAggregates"].(map[string]any); aa["enabled"] != false || aa["countDistinct"] != "exact" {
			t.Errorf("Database.approximateAggregates: %v", db["approximateAggregates"])
		}
	})

	t.Run("the request is validated before a task exists", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"enabled": false, "countDistinct": "approximate"},
			{"enabled": true, "countDistinct": "approximate", "errorRate": 0.5},
			{"enabled": true, "countDistinct": "exact", "errorRate": 0.01},
		} {
			st, p := leader.Do("PUT", path, body, nil)
			if st != 400 || p["code"] != "validation_failed" {
				t.Errorf("%v: %d %v", body, st, p)
			}
		}
		if st, p := leader.Do("PUT", "/databases/nope/approximate-aggregates", map[string]any{"enabled": true}, nil); st != 404 {
			t.Errorf("an unknown database: %d %v", st, p)
		}
	})

	t.Run("enabling installs the extension on every server and records the setting", func(t *testing.T) {
		body := leader.Expect(202, "PUT", path, map[string]any{"enabled": true, "countDistinct": "approximate", "errorRate": 0.02})
		got := leader.WaitTask(clustertest.TaskID(body), 5*time.Minute)
		if got["state"] != "done" {
			t.Fatalf("%v\n%s", got["error"], leader.LogTail(3000))
		}
		names := apStepNames(got)
		if len(names) != 3+4 || names[0] != "Check the servers" || names[len(names)-1] != "Record the settings" || !strings.HasPrefix(names[1], "Install the extension on server ") {
			t.Errorf("steps %v", names)
		}
		for _, n := range c.Nodes {
			if v := apSketchVersion(t, n, "ap"); v != "1.0" {
				t.Errorf("node %d: kingres_sketch %q", n.Index, v)
			}
			// kingres_shard of this installation may not know the settings yet; then the step skipped them. When it knows them they are stored.
			var known, stored int
			c2 := n.ServicePG("ap")
			if err := c2.QueryRow(context.Background(), `SELECT count(*) FROM pg_settings WHERE name = 'kingres_shard.count_distinct_mode'`).Scan(&known); err != nil {
				t.Fatal(err)
			}
			if err := c2.QueryRow(context.Background(), `SELECT count(*) FROM pg_db_role_setting s JOIN pg_database d ON d.oid = s.setdatabase
				WHERE d.datname = 'ap' AND s.setrole = 0 AND EXISTS (SELECT 1 FROM unnest(s.setconfig) c WHERE c LIKE 'kingres_shard.count_distinct_mode=approximate')`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if known == 1 && stored != 1 {
				t.Errorf("node %d: count_distinct_mode is not in pg_db_role_setting", n.Index)
			}
			if known == 0 && stored != 0 {
				t.Errorf("node %d: a setting the server does not know was stored", n.Index)
			}
		}
		read := leader.Expect(200, "GET", path, nil)
		if read["enabled"] != true || read["countDistinct"] != "approximate" || read["errorRate"] != 0.02 || read["extensionVersion"] != "1.0" {
			t.Fatalf("%v", read)
		}
		for _, s := range read["servers"].([]any) {
			m := s.(map[string]any)
			if m["state"] != "ok" || m["installedVersion"] != "1.0" {
				t.Errorf("server %v: %v", m["serverId"], m)
			}
		}
		db := leader.Expect(200, "GET", "/databases/ap", nil)
		if aa, _ := db["approximateAggregates"].(map[string]any); aa["enabled"] != true || aa["countDistinct"] != "approximate" {
			t.Errorf("Database.approximateAggregates: %v", db["approximateAggregates"])
		}
		// the same read from another server answers the same (the read is the leader's)
		if got := c.Nodes[1].Expect(200, "GET", path, nil); got["extensionVersion"] != "1.0" {
			t.Errorf("from a follower: %v", got)
		}
	})

	t.Run("a second request while one runs is operation_in_progress, a repeat after it is fine", func(t *testing.T) {
		first := leader.Expect(202, "PUT", path, map[string]any{"enabled": true, "countDistinct": "approximate", "errorRate": 0.02})
		st, p := leader.Do("PUT", path, map[string]any{"enabled": true}, nil)
		leader.WaitTaskDone(clustertest.TaskID(first), 5*time.Minute)
		if st != 409 || p["code"] != "operation_in_progress" {
			t.Errorf("%d %v", st, p)
		}
		again := leader.Expect(202, "PUT", path, map[string]any{"enabled": true, "countDistinct": "approximate", "errorRate": 0.02})
		leader.WaitTaskDone(clustertest.TaskID(again), 5*time.Minute)
		for _, n := range c.Nodes {
			if v := apSketchVersion(t, n, "ap"); v != "1.0" {
				t.Errorf("node %d: %q", n.Index, v)
			}
		}
	})

	t.Run("disabling keeps the extension", func(t *testing.T) {
		body := leader.Expect(202, "PUT", path, map[string]any{"enabled": false})
		leader.WaitTaskDone(clustertest.TaskID(body), 5*time.Minute)
		read := leader.Expect(200, "GET", path, nil)
		if read["enabled"] != false || read["countDistinct"] != "exact" || read["errorRate"] != nil || read["extensionVersion"] != "1.0" {
			t.Fatalf("%v", read)
		}
		for _, n := range c.Nodes {
			if v := apSketchVersion(t, n, "ap"); v != "1.0" {
				t.Errorf("node %d: the extension was dropped (%q)", n.Index, v)
			}
		}
	})

	t.Run("roles and the audit log", func(t *testing.T) {
		tokens := map[string]string{}
		for name, role := range map[string]string{"apview": "kingres_readonly", "apstats": "kingres_stats", "apops": "kingres_ops"} {
			mkRole(t, leader, name, markerPassword, role)
			st, o := leader.Do("POST", "/auth/login", map[string]any{"username": name, "password": markerPassword, "issueToken": true}, map[string]string{"Authorization": ""})
			if st != 200 {
				t.Fatalf("login %s: %d %v", name, st, o)
			}
			tokens[name] = o["token"].(string)
		}
		as := func(name, method string, body any) int {
			st, _ := leader.Do(method, path, body, map[string]string{"Authorization": "Bearer " + tokens[name]})
			return st
		}
		if st := as("apview", "GET", nil); st != 200 {
			t.Errorf("a read-only role reads: %d", st)
		}
		for _, name := range []string{"apview", "apops"} {
			if st := as(name, "PUT", map[string]any{"enabled": true}); st != 403 {
				t.Errorf("%s PUT: %d, want 403", name, st)
			}
		}
		for _, m := range []string{"GET", "PUT"} {
			if st := as("apstats", m, map[string]any{"enabled": true}); st != 403 {
				t.Errorf("a stats role %s: %d, want 403", m, st)
			}
		}
		audit := leader.Expect(200, "GET", "/audit?action=configureApproximateAggregates", nil)
		items, _ := audit["items"].([]any)
		var ok, denied int
		for _, it := range items {
			switch it.(map[string]any)["outcome"] {
			case "success":
				ok++
			case "denied":
				denied++
			}
		}
		if ok < 2 || denied < 2 {
			t.Errorf("the audit log has %d successes and %d denied for configureApproximateAggregates, want at least 2 and 2 (%v)", ok, denied, items)
		}
	})

	t.Run("a server that is down makes the task wait, and it finishes when the server is back", func(t *testing.T) {
		down := c.Nodes[2]
		down.Stop()
		t.Cleanup(func() {
			if !down.Running() {
				down.Start()
			}
		})
		body := leader.Expect(202, "PUT", path, map[string]any{"enabled": true, "countDistinct": "approximate", "errorRate": 0.05})
		id := clustertest.TaskID(body)
		waiting := false
		for end := time.Now().Add(2 * time.Minute); time.Now().Before(end) && !waiting; time.Sleep(200 * time.Millisecond) {
			_, task := leader.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
			if d, _ := task["detail"].(string); strings.Contains(d, "Waiting for server "+down.Host) {
				waiting = true
			}
		}
		if !waiting {
			t.Fatalf("the task does not say that it waits for %s\n%s", down.Host, leader.LogTail(3000))
		}
		_, task := leader.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		if task["state"] != "running" {
			t.Fatalf("the task is %v while a server is down; it must wait", task["state"])
		}
		// The read says which server cannot be reached.
		read := leader.Expect(200, "GET", path, nil)
		var unreachable int
		for _, s := range read["servers"].([]any) {
			if s.(map[string]any)["state"] == "unreachable" {
				unreachable++
			}
		}
		if unreachable != 1 {
			t.Errorf("%d servers unreachable in %v", unreachable, read["servers"])
		}
		down.Start()
		signedIn := false
		for end := time.Now().Add(90 * time.Second); time.Now().Before(end) && !signedIn; time.Sleep(time.Second) {
			signedIn = down.Login(clustertest.AdminUser, clustertest.AdminPass)
		}
		if !signedIn {
			t.Fatalf("cannot sign in on the restarted server\n%s", down.LogTail(2000))
		}
		final := leader.WaitTask(id, 4*time.Minute)
		if final["state"] != "done" {
			t.Fatalf("the task ended %v: %v\n%s", final["state"], final["error"], leader.LogTail(3000))
		}
		for _, n := range c.Nodes {
			if v := apSketchVersion(t, n, "ap"); v != "1.0" {
				t.Errorf("node %d: %q", n.Index, v)
			}
		}
		if read := leader.Expect(200, "GET", path, nil); read["errorRate"] != 0.05 {
			t.Errorf("the recorded rate: %v", read["errorRate"])
		}
	})
}
