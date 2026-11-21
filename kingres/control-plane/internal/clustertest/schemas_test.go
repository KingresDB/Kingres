// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Phase 11, step 13: the schema operations through the real extension (0.8).
func TestDistributedSchemasThroughTheAPI(t *testing.T) {
	c := clustertest.FormWithExtra(t, 3, nil, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "sc", "sharding": map[string]any{"enabled": true, "mode": "both", "bucketCount": 8}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		sc := leader.ServicePG("sc")
		for _, q := range []string{`CREATE SCHEMA tenant1`, `CREATE TABLE tenant1.items (id bigint PRIMARY KEY, v text)`, `INSERT INTO tenant1.items SELECT g, 'x' FROM generate_series(1, 200) g`,
			`CREATE SCHEMA nokey`, `CREATE TABLE nokey.log (a int)`, `CREATE SCHEMA bad`, `CREATE MATERIALIZED VIEW bad.mv AS SELECT 1 AS one`} {
			if _, err := sc.Exec(context.Background(), q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	})
	l := c.Leader()

	db := l.Expect(200, "GET", "/databases/sc", nil)
	if sh := db["sharding"].(map[string]any); sh["mode"] != "both" || sh["distributeNewSchemas"] != false {
		t.Fatalf("sharding settings: %v", sh)
	}
	page := l.Expect(200, "GET", "/databases/sc/schemas?distributed=false", nil)
	names := map[string]bool{}
	for _, s := range anyList(page["items"]) {
		names[s.(map[string]any)["name"].(string)] = true
	}
	if !names["tenant1"] || !names["bad"] || names["kingres"] {
		t.Fatalf("local schemas: %v", names)
	}

	t.Run("a schema with an unsupported object is refused with the blockers", func(t *testing.T) {
		st, p := l.Do("PUT", "/databases/sc/schemas/bad/distribution", nil, nil)
		if st != 422 || p["code"] != "schema_not_distributable" || len(anyList(p["errors"])) == 0 {
			t.Fatalf("%d %v", st, p)
		}
		d := l.Expect(200, "GET", "/databases/sc/schemas/bad", nil)
		if len(anyList(d["distributionBlockers"])) == 0 {
			t.Errorf("blockers: %v", d)
		}
	})

	var groups []int32
	rows, err := l.ServicePG("sc").Query(context.Background(), `SELECT group_id FROM kingres.node WHERE role = 'primary' ORDER BY group_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var g int32
		_ = rows.Scan(&g)
		groups = append(groups, g)
	}
	rows.Close()
	if len(groups) < 2 {
		t.Fatalf("groups: %v", groups)
	}

	t.Run("distribute, move, make local", func(t *testing.T) {
		body := l.Expect(202, "PUT", "/databases/sc/schemas/tenant1/distribution", map[string]any{})
		task := l.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
		if task["kind"] != "schema.distribute" {
			t.Errorf("task: %v", task)
		}
		d := l.Expect(200, "GET", "/databases/sc/schemas/tenant1", nil)
		dist, _ := d["distribution"].(map[string]any)
		if dist == nil || dist["state"] != "active" {
			t.Fatalf("after distribute: %v", d)
		}
		if st, p := l.Do("PUT", "/databases/sc/schemas/tenant1/distribution", nil, nil); st != 409 {
			t.Errorf("distribute twice: %d %v", st, p)
		}
		from := int32(dist["shardGroupId"].(float64))
		var to int32 = -1
		for _, g := range groups {
			if g != from {
				to = g
				break
			}
		}
		if st, p := l.Do("POST", "/databases/sc/schemas/tenant1/moves", map[string]any{"targetShardGroupId": from}, nil); st != 422 {
			t.Errorf("move to the same group: %d %v", st, p)
		}
		body = l.Expect(202, "POST", "/databases/sc/schemas/tenant1/moves", map[string]any{"targetShardGroupId": to})
		l.WaitTaskDone(clustertest.TaskID(body), 6*time.Minute)
		d = l.Expect(200, "GET", "/databases/sc/schemas/tenant1", nil)
		if got := d["distribution"].(map[string]any)["shardGroupId"]; got != float64(to) {
			t.Errorf("schema is on %v, want %d", got, to)
		}
		var n int
		if err := c.Nodes[0].ServicePG("sc").QueryRow(context.Background(), `SELECT count(*) FROM tenant1.items`).Scan(&n); err != nil || n != 200 {
			t.Errorf("rows after the move: %d %v", n, err)
		}
		body = l.Expect(202, "DELETE", "/databases/sc/schemas/tenant1/distribution", nil)
		l.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
		if d = l.Expect(200, "GET", "/databases/sc/schemas/tenant1", nil); d["distribution"] != nil {
			t.Errorf("after make local: %v", d)
		}
	})

	t.Run("a table without a key needs the replica identity", func(t *testing.T) {
		body := l.Expect(202, "PUT", "/databases/sc/schemas/nokey/distribution", nil)
		l.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
		d := l.Expect(200, "GET", "/databases/sc/schemas/nokey", nil)
		from := int32(d["distribution"].(map[string]any)["shardGroupId"].(float64))
		var to int32
		for _, g := range groups {
			if g != from {
				to = g
			}
		}
		st, p := l.Do("POST", "/databases/sc/schemas/nokey/moves", map[string]any{"targetShardGroupId": to}, nil)
		if st != 422 || len(anyList(p["errors"])) == 0 {
			t.Errorf("move without a key: %d %v", st, p)
		}
	})

	t.Run("the mode cannot go back to rows while a schema is distributed", func(t *testing.T) {
		st, p := l.Do("PATCH", "/databases/sc", map[string]any{"sharding": map[string]any{"mode": "row"}}, nil)
		if st != 409 || p["code"] != "sharding_mode_in_use" {
			t.Errorf("%d %v", st, p)
		}
	})
}
