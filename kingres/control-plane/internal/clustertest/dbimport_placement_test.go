// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// An import on a cluster whose servers have a weight or no place for shards, and the exclusion while it runs. The private database has a catalog of its own that no
// drain and no weight change follows, so (1) the import gives it the flags and the weights of the cluster when it registers the servers, and the final database has
// no bucket on the server that should not hold any; (2) while it runs a rebalance, a drain, the removal of a server and a change of the weight or of the flag of a server
// are refused with 409 operation_in_progress; and (3) they are allowed again when it has ended.
func TestDatabaseImportKeepsThePlacementOfTheClusterAndExcludesDrainAndWeight(t *testing.T) {
	c := clustertest.FormTLS(t, 3, nil, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "dbw", "sharding": map[string]any{"enabled": true, "bucketCount": 12, "mode": "both"}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader, f1, f2 := c.Leader(), c.Nodes[1], c.Nodes[2]
	if leader == f1 {
		f1 = c.Nodes[0]
	}
	if leader == f2 {
		f2 = c.Nodes[0]
	}
	for _, n := range c.Nodes {
		n.Login(clustertest.AdminUser, clustertest.AdminPass)
	}
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": true, "maxConcurrent": 4}, "analytics": map[string]any{"allowNonSuperuserOwners": true}})
	q := func(db, sql string) {
		leader.Expect(200, "POST", "/query", map[string]any{"database": db, "sql": sql})
	}
	task := func(method, path string, body any) {
		t.Helper()
		out := leader.Expect(202, method, path, body)
		if res := leader.WaitTaskDone(clustertest.TaskID(out), 4*time.Minute); res["state"] != "done" {
			t.Fatalf("%s %s: %v", method, path, res["error"])
		}
	}
	q("dbw", "create table public.d_hash (id bigint primary key, v text)")
	task("PUT", "/databases/dbw/schemas/public/tables/d_hash/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	q("dbw", "insert into public.d_hash select g, 'v' || g from generate_series(1, 200) g")
	// the placement of the cluster: one server has a weight, another should hold no shard (a drain that has not moved its buckets yet, as far as the catalogs go)
	leader.Expect(200, "PATCH", "/servers/"+f2.Host, map[string]any{"weight": 3})
	leader.Expect(200, "PATCH", "/servers/"+f1.Host, map[string]any{"shouldHaveShards": false})

	st, out := f1.Do("POST", "/databases/dbw/export", map[string]any{}, nil)
	if st != 202 {
		t.Fatalf("export: %d %v", st, out)
	}
	res := f2.WaitTask(clustertest.TaskID(out), 6*time.Minute)
	if res["state"] != "done" {
		t.Fatalf("export task: %v", res["error"])
	}
	id := strings.TrimPrefix(res["detail"].(string), "transfer ")
	_, _, body, err := leader.DoBody("GET", "/data-exports/"+id+"/content", nil, nil)
	if err != nil || len(body) == 0 {
		t.Fatalf("download: %v", err)
	}
	up := f1.Expect(201, "POST", "/data-uploads", map[string]any{"purpose": "databaseArchive", "sizeBytes": len(body)})
	uid := up["uploadId"].(string)
	const mb = 1 << 20
	for off := 0; off < len(body); off += mb {
		end := min(off+mb, len(body))
		if st, _ := xfChunk(t, f1, uid, off, body[off:end]); st != 204 {
			t.Fatalf("chunk at %d: %d", off, st)
		}
	}
	st, out = leader.Do("POST", "/database-imports", map[string]any{"uploadId": uid, "database": "dbw_copy"}, nil)
	if st != 202 {
		t.Fatalf("import: %d %v", st, out)
	}
	imp := clustertest.TaskID(out)
	// while the import runs
	if _, tk := leader.Do("GET", fmt.Sprintf("/tasks/%v", imp), nil, nil); tk["state"] == "done" || tk["state"] == "failed" {
		t.Fatalf("the import ended before the refusals were asked: %v", tk)
	}
	refused := func(what, method, path string, body any) {
		t.Helper()
		st, resp := leader.Do(method, path, body, nil)
		if st != 409 || resp["code"] != "operation_in_progress" {
			t.Errorf("%s while an import runs: %d %v, want 409 operation_in_progress", what, st, resp)
		}
	}
	refused("a rebalance", "POST", "/rebalance", map[string]any{})
	refused("a drain", "POST", "/servers/"+f2.Host+"/drain", map[string]any{"acceptFewerReplicas": true, "acknowledgeLocalData": true})
	refused("a change of the weight", "PATCH", "/servers/"+f2.Host, map[string]any{"weight": 5})
	refused("a change of the flag", "PATCH", "/servers/"+f2.Host, map[string]any{"shouldHaveShards": false})
	if st, resp := leader.Do("DELETE", "/servers/"+f2.Host, nil, nil); st != 409 {
		t.Errorf("the removal of a server while an import runs: %d %v, want 409", st, resp)
	}
	done := leader.WaitTask(imp, 10*time.Minute)
	if done["state"] != "done" {
		t.Fatalf("import task: %v", done["error"])
	}
	sq := func(db, sql string) string {
		c := leader.ServicePG(db)
		defer func() { _ = c.Close(context.Background()) }()
		rows, err := c.Query(context.Background(), sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		defer rows.Close()
		var out [][]any
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			out = append(out, vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return fmt.Sprint(out)
	}
	const nodes = "select host, should_have_shards, weight, is_active from kingres.node where role = 'primary' order by host"
	if a, b := sq("dbw", nodes), sq("dbw_copy", nodes); a != b || !strings.Contains(a, "false") || !strings.Contains(a, "3") {
		t.Errorf("the placement flags of the catalogs: source %s, imported %s", a, b)
	}
	if got := sq("dbw_copy", "select count(*) from kingres.bucket_map m join kingres.node n on n.group_id = m.group_id and n.role = 'primary' where n.host = '"+f1.Host+"'"); got != "[[0]]" {
		t.Errorf("the imported database has buckets on the server that should hold none: %s", got)
	}
	if a, b := sq("dbw", "select count(*), sum(id) from public.d_hash"), sq("dbw_copy", "select count(*), sum(id) from public.d_hash"); a != b {
		t.Errorf("the rows: source %s, imported %s", a, b)
	}
	// the import has ended: the weight can change again
	leader.Expect(200, "PATCH", "/servers/"+f2.Host, map[string]any{"weight": 1})
}
