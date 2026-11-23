// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"archive/zip"
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlshape"
)

// The export of a database over three servers (contract 0.40.0): sharded, reference, ledger, columnar and time-partitioned tables, a user-partitioned
// distributed table, read through the row router over several groups. The archive is made through a follower, downloaded through another, validated as it
// is and then uploaded through a follower and validated by the task.

func TestDatabaseExportOverThreeServers(t *testing.T) {
	c := clustertest.FormTLS(t, 3, nil, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "dbx3", "sharding": map[string]any{"enabled": true, "bucketCount": 12},
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
	q := func(sql string) map[string]any {
		return leader.Expect(200, "POST", "/query", map[string]any{"database": "dbx3", "sql": sql})
	}
	task := func(method, path string, body any) {
		t.Helper()
		out := leader.Expect(202, method, path, body)
		if res := leader.WaitTaskDone(clustertest.TaskID(out), 4*time.Minute); res["state"] != "done" {
			t.Fatalf("%s %s: %v", method, path, res["error"])
		}
	}
	tbl := func(name string) string { return "/databases/dbx3/schemas/public/tables/" + name }
	for _, s := range []string{
		"create table public.t_plain (id serial primary key, name text, n numeric(10,2))",
		"create table public.refdata (id int primary key, v text)",
		"create table public.d_hash (id bigint primary key, tenant int not null, v text)",
		"create table public.d_part (id bigint not null, ts timestamptz not null, v text, primary key (id, ts)) partition by range (ts)",
		"create table public.c_events (id bigint, v text)",
		"create table public.acc (id bigint primary key, bal int)",
		"create table public.ev (id bigint not null, at timestamptz not null, v text, primary key (id, at)) partition by range (at)",
	} {
		q(s)
	}
	task("PUT", tbl("refdata")+"/distribution", map[string]any{"method": "reference"})
	task("PUT", tbl("d_hash")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	task("PUT", tbl("d_part")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	task("POST", tbl("d_part")+"/partitions", map[string]any{"name": "d_part_2026", "bound": "FROM ('2026-01-01') TO ('2027-01-01')"})
	task("POST", tbl("d_part")+"/partitions", map[string]any{"name": "d_part_rest", "bound": "DEFAULT"})
	task("PUT", tbl("c_events")+"/storage", map[string]any{"accessMethod": "columnar"})
	leader.Expect(200, "PUT", tbl("acc")+"/ledger", map[string]any{"mode": "updatable"})
	leader.Expect(200, "PUT", tbl("ev")+"/partition-policy", map[string]any{"partitionEvery": "P1D", "createAhead": "P3D", "retention": "P30D", "enabled": true})
	q("create view public.v_hash as select tenant, count(*) as c from public.d_hash group by tenant")
	for _, s := range []string{
		"insert into public.t_plain (name, n) values ('Ada', 1.5), ('Bob', 2)",
		"insert into public.refdata values (1, 'a'), (2, 'b')",
		"insert into public.d_hash select g, g % 5, 'v' || g from generate_series(1, 300) g",
		"insert into public.d_part select g, '2026-02-01'::timestamptz + (g || ' hours')::interval, 'p' || g from generate_series(1, 90) g",
		"insert into public.c_events select g, 'e' || g from generate_series(1, 120) g",
		"insert into public.acc values (1, 10), (2, 20)",
	} {
		q(s)
	}

	// The export through a follower; the files are the leader's.
	st, out := f1.Do("POST", "/databases/dbx3/export", map[string]any{}, nil)
	if st != 202 {
		t.Fatalf("export through a follower: %d %v", st, out)
	}
	res := f2.WaitTask(clustertest.TaskID(out), 6*time.Minute)
	if res["state"] != "done" || !strings.HasPrefix(res["detail"].(string), "transfer ") {
		t.Fatalf("export task: %v %v", res["state"], res["error"])
	}
	id := strings.TrimPrefix(res["detail"].(string), "transfer ")
	exp := f2.Expect(200, "GET", "/data-exports/"+id, nil)
	levels := map[string]string{}
	groups := map[string]float64{}
	for _, cc := range exp["consistency"].([]any) {
		m := cc.(map[string]any)
		levels[m["table"].(string)] = m["level"].(string)
		groups[m["table"].(string)] = m["groups"].(float64)
	}
	// With buckets in several groups a hash table is read per group; the others are one snapshot of their own; the archive is not one snapshot.
	if levels["public.d_hash"] != "per-group" || groups["public.d_hash"] < 2 || levels["public.d_part"] != "per-group" || levels["public.refdata"] != "single-snapshot" || levels["public.c_events"] != "single-snapshot" {
		t.Errorf("consistency labels: %v groups %v", levels, groups)
	}
	for tb, lv := range levels {
		if lv == "archive-single-snapshot" {
			t.Errorf("%s is labelled as one snapshot of the archive although tables have buckets in several groups", tb)
		}
	}
	var body []byte
	for _, n := range []*clustertest.Node{f1, leader, f2} {
		st, h, b, err := n.DoBody("GET", "/data-exports/"+id+"/content", nil, nil)
		if err != nil || st != 200 || h.Get("Content-Type") != "application/zip" || h.Get("Content-Length") != strconv.Itoa(len(b)) {
			t.Fatalf("node %d download: %d %v err %v", n.Index, st, h, err)
		}
		if body == nil {
			body = b
		} else if !bytes.Equal(body, b) {
			t.Errorf("node %d got other bytes", n.Index)
		}
	}
	if exp["sha256"] != xfSum(body) {
		t.Errorf("sha256 %v, file %s", exp["sha256"], xfSum(body))
	}
	r, err := archive.Validate(bytes.NewReader(body), int64(len(body)), archive.Limits{MaxImportBytes: 1 << 30, SQLFor: func(cat *archive.Catalog) archive.SQLChecker {
		return func(name string, rd io.Reader) (archive.SQLResult, error) {
			plan, err := sqlshape.ParseFile(rd, name, sqlshape.Hints{})
			if err != nil {
				return archive.SQLResult{}, err
			}
			o := archive.SQLResult{ByKind: plan.ByKind}
			for _, rf := range plan.Refused {
				o.Refused = append(o.Refused, archive.RefusedStatement{Kind: rf.Kind, Position: rf.Position, Fingerprint: rf.Fingerprint, Reason: rf.Reason})
			}
			return o, nil
		}
	}})
	if err != nil {
		t.Fatalf("the archive is not valid: %v", err)
	}
	for n, sr := range r.SQL {
		if len(sr.Refused) > 0 {
			t.Errorf("%s: refused %v", n, sr.Refused)
		}
	}
	rows := map[string]int64{}
	for _, e := range r.Manifest.Entries {
		if e.Table != "" && !e.LedgerHistory {
			rows[e.Table] = e.Rows
		}
	}
	for tb, want := range map[string]int64{"public.t_plain": 2, "public.refdata": 2, "public.d_hash": 300, "public.d_part": 90, "public.c_events": 120, "public.acc": 2, "public.ev": 0} {
		if rows[tb] != want {
			t.Errorf("table %s: %d rows, want %d", tb, rows[tb], want)
		}
	}
	zr, _ := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	schema := ""
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "schema/") {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			schema += string(b)
		}
	}
	for _, bad := range []string{"_b0", "PARTITION BY HASH", "CREATE EXTENSION", "kingres_ledger_", "acc_ledger_history"} {
		if strings.Contains(schema, bad) {
			t.Errorf("the schema files hold %q", bad)
		}
	}
	warned := false
	for _, w := range r.Manifest.Warnings {
		warned = warned || strings.Contains(w, "d_hash was read per group")
	}
	if !warned {
		t.Errorf("warnings: %v", r.Manifest.Warnings)
	}

	// The archive, uploaded through a follower and validated through another.
	up := f1.Expect(201, "POST", "/data-uploads", map[string]any{"purpose": "databaseArchive", "sizeBytes": len(body)})
	uid := up["uploadId"].(string)
	const mb = 1 << 20
	for off := 0; off < len(body); off += mb {
		end := off + mb
		if end > len(body) {
			end = len(body)
		}
		if st, _ := xfChunk(t, f1, uid, off, body[off:end]); st != 204 {
			t.Fatalf("chunk at %d: %d", off, st)
		}
	}
	st, vt := f2.Do("POST", "/data-uploads/"+uid+"/validation", nil, nil)
	if st != 202 {
		t.Fatalf("validation through a follower: %d %v", st, vt)
	}
	vres := f1.WaitTask(clustertest.TaskID(vt), 5*time.Minute)
	if vres["state"] != "done" {
		t.Fatalf("validation task: %v", vres["error"])
	}
	got := f2.Expect(200, "GET", "/data-uploads/"+uid, nil)
	rep, _ := got["report"].(map[string]any)
	if got["state"] != "valid" || rep["valid"] != true || rep["sha256"] != xfSum(body) {
		t.Errorf("the upload after the validation: %v", got)
	}
}
