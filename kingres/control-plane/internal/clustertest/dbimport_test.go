// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive/archivetest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// The import of a database over three servers (contract 0.41.0): the archive of a sharded database is uploaded through a follower and imported through another;
// the private database is made on every server, the tables are distributed over the groups while they are empty, the data is loaded through the row router, the
// sequences are raised on every node, and the private name is renamed to the final name on every server (spike V8: the rename works across groups, with the
// background workers of the extensions connected). A failed import removes the private database from every server.

func TestDatabaseImportOverThreeServers(t *testing.T) {
	c := clustertest.FormTLS(t, 3, nil, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "dbm3", "sharding": map[string]any{"enabled": true, "bucketCount": 12, "mode": "both"},
			"ledger": map[string]any{"mode": "optIn"}})
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
	q := func(db, sql string) map[string]any {
		return leader.Expect(200, "POST", "/query", map[string]any{"database": db, "sql": sql})
	}
	task := func(method, path string, body any) {
		t.Helper()
		out := leader.Expect(202, method, path, body)
		if res := leader.WaitTaskDone(clustertest.TaskID(out), 4*time.Minute); res["state"] != "done" {
			t.Fatalf("%s %s: %v", method, path, res["error"])
		}
	}
	tbl := func(name string) string { return "/databases/dbm3/schemas/public/tables/" + name }
	for _, s := range []string{
		"create table public.t_plain (id serial primary key, name text, n numeric(10,2))",
		"create table public.refdata (id int primary key, v text)",
		"create table public.d_hash (id bigint primary key, tenant int not null, v text)",
		"create table public.d_two (id bigint primary key, d_id bigint, w text)",
		"create table public.d_part (id bigint not null, ts timestamptz not null, v text, primary key (id, ts)) partition by range (ts)",
		"create table public.c_events (id bigint, v text)",
		"create table public.acc (id bigint primary key, bal int)",
		"create schema tn",
		"create table tn.docs (id bigint primary key, v text)",
	} {
		q("dbm3", s)
	}
	// A distributed schema: its tables live together on one group.
	task("PUT", "/databases/dbm3/schemas/tn/distribution", nil)
	task("PUT", tbl("refdata")+"/distribution", map[string]any{"method": "reference"})
	task("PUT", tbl("d_hash")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	task("PUT", tbl("d_two")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id", "colocateWith": map[string]any{"schema": "public", "table": "d_hash"}})
	task("PUT", tbl("d_part")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	task("POST", tbl("d_part")+"/partitions", map[string]any{"name": "d_part_2026", "bound": "FROM ('2026-01-01') TO ('2027-01-01')"})
	task("POST", tbl("d_part")+"/partitions", map[string]any{"name": "d_part_rest", "bound": "DEFAULT"})
	task("PUT", tbl("c_events")+"/storage", map[string]any{"accessMethod": "columnar"})
	leader.Expect(200, "PUT", tbl("acc")+"/ledger", map[string]any{"mode": "updatable"})
	for _, s := range []string{
		"insert into public.t_plain (name, n) values ('Ada', 1.5), ('Bob', 2)",
		"insert into public.refdata values (1, 'a'), (2, 'b')",
		"insert into public.d_hash select g, g % 5, 'v' || g from generate_series(1, 300) g",
		"insert into public.d_two select g, g, 'w' || g from generate_series(1, 100) g",
		"insert into public.d_part select g, '2026-02-01'::timestamptz + (g || ' hours')::interval, 'p' || g from generate_series(1, 90) g",
		"insert into public.c_events select g, 'e' || g from generate_series(1, 120) g",
		"insert into public.acc values (1, 10), (2, 20)",
		"insert into tn.docs select g, 'doc' || g from generate_series(1, 60) g",
	} {
		q("dbm3", s)
	}
	st, out := f1.Do("POST", "/databases/dbm3/export", map[string]any{}, nil)
	if st != 202 {
		t.Fatalf("export: %d %v\n%s", st, out, leader.LogTail(6000)+f1.LogTail(3000))
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
	upload := func(n *clustertest.Node, data []byte) string {
		up := n.Expect(201, "POST", "/data-uploads", map[string]any{"purpose": "databaseArchive", "sizeBytes": len(data)})
		uid := up["uploadId"].(string)
		const mb = 1 << 20
		for off := 0; off < len(data); off += mb {
			end := off + mb
			if end > len(data) {
				end = len(data)
			}
			if st, _ := xfChunk(t, n, uid, off, data[off:end]); st != 204 {
				t.Fatalf("chunk at %d: %d", off, st)
			}
		}
		return uid
	}
	// The importer is a role of kingres_admin as the API makes it, with CREATEROLE and the creator as a member, used as it is.
	leader.Expect(201, "POST", "/roles", map[string]any{"name": "dbi_admin", "password": "Importer-Passw0rd-1", "clusterRoles": []string{"kingres_admin"}})
	for _, n := range []*clustertest.Node{f1, f2} {
		if !n.Login("dbi_admin", "Importer-Passw0rd-1") {
			t.Fatalf("the importer cannot sign in on node %d", n.Index)
		}
	}
	uid := upload(f1, body)
	// Imported through another follower: the work and the files are the leader's.
	st, out = f2.Do("POST", "/database-imports", map[string]any{"uploadId": uid, "database": "dbm3_copy", "acknowledgeLedgerRestart": true}, nil)
	if st != 202 {
		t.Fatalf("import through a follower: %d %v", st, out)
	}
	done := f1.WaitTask(clustertest.TaskID(out), 10*time.Minute)
	if done["state"] != "done" {
		t.Fatalf("import task: %v", done["error"])
	}
	// What the importer owns is read as the service role.
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
	digest := func(db, tb string) string {
		return sq(db, "select count(*), coalesce(md5(string_agg(x::text, '|' order by x::text)), '') from "+tb+" x")
	}
	for _, tb := range []string{"public.t_plain", "public.refdata", "public.d_hash", "public.d_two", "public.d_part", "public.c_events", "public.acc", "tn.docs"} {
		if a, b := digest("dbm3", tb), digest("dbm3_copy", tb); a != b {
			t.Errorf("table %s: source %s, imported %s", tb, a, b)
		}
	}
	// The buckets are spread over the groups, the colocation of the two tables is kept.
	if got := sq("dbm3_copy", "select count(distinct group_id) >= 2 from kingres.bucket_map"); got != "[[true]]" {
		t.Errorf("the buckets of the imported database are in one group: %s", got)
	}
	if got := sq("dbm3_copy", "select count(distinct colocation_id) from kingres.dist_table where method = 'hash'"); got != sq("dbm3", "select count(distinct colocation_id) from kingres.dist_table where method = 'hash'") {
		t.Errorf("the colocation groups differ: %s", got)
	}
	// The schema is distributed here too, and its table is the schema's.
	if a, b := sq("dbm3", "select count(*) from kingres.dist_schema"), sq("dbm3_copy", "select count(*) from kingres.dist_schema"); a != b || a != "[[1]]" {
		t.Errorf("distributed schemas: source %s, imported %s", a, b)
	}
	// The sequence of the serial column continues above the rows on the node that takes the insert.
	if got := sq("dbm3_copy", "insert into public.t_plain (name) values ('z') returning id"); got != "[[3]]" {
		t.Errorf("the serial column continues with %s", got)
	}
	// The ledger of the imported database is consistent: a digest builds the block of the entries of the loaded rows, and the verification reads it.
	leader.Expect(201, "POST", "/databases/dbm3_copy/ledger/digests", nil)
	vt := leader.Expect(202, "POST", "/databases/dbm3_copy/ledger/verifications", map[string]any{"scope": "full"})
	if vres := leader.WaitTaskDone(clustertest.TaskID(vt), 5*time.Minute); vres["state"] != "done" {
		t.Fatalf("ledger verification: %v", vres["error"])
	}
	if st := leader.Expect(200, "GET", "/databases/dbm3_copy/ledger", nil); st["verificationState"] != "passed" {
		t.Errorf("ledger status: %v", st)
	}
	// Every server has the database under its final name and none has a private one.
	for _, n := range c.Nodes {
		r := n.Expect(200, "POST", "/query", map[string]any{"database": "postgres", "sql": "select datname from pg_database where datname like 'dbm3%' or datname like 'kingres_import_%' order by 1"})
		if got := fmt.Sprint(r["rows"]); got != "[[dbm3] [dbm3_copy]]" {
			t.Errorf("node %d has the databases %s", n.Index, got)
		}
	}

	// A failed import removes the private database from every server.
	zv := archivetest.Read(t, body)
	zv.LieAboutRows("public.d_hash", 3)
	bad := upload(f2, zv.Bytes(t))
	st, out = f1.Do("POST", "/database-imports", map[string]any{"uploadId": bad, "database": "dbm3_bad", "acknowledgeLedgerRestart": true}, nil)
	if st != 202 {
		t.Fatalf("second import: %d %v", st, out)
	}
	fail := f2.WaitTask(clustertest.TaskID(out), 10*time.Minute)
	if fail["state"] != "failed" || fmt.Sprint(fail["error"].(map[string]any)["code"]) != "row_count_mismatch" {
		t.Fatalf("a manifest that lies: %v %v", fail["state"], fail["error"])
	}
	for _, n := range c.Nodes {
		deadline := time.Now().Add(2 * time.Minute)
		for {
			r := n.Expect(200, "POST", "/query", map[string]any{"database": "postgres", "sql": "select count(*) from pg_database where datname like 'kingres_import_%' or datname = 'dbm3_bad'"})
			if fmt.Sprint(r["rows"]) == "[[0]]" {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("node %d keeps a database of the failed import: %v", n.Index, r["rows"])
				break
			}
			time.Sleep(time.Second)
		}
	}
}
