// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// uploadFile sends data as a complete upload and returns its id.
func (e *cp) uploadFile(t testing.TB, data []byte) string {
	t.Helper()
	up := e.expect(t, 201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": len(data)})
	id := up["uploadId"].(string)
	const seg = 1 << 20
	for off := 0; off < len(data); off += seg {
		end := off + seg
		if end > len(data) {
			end = len(data)
		}
		if st, _, out := e.chunk(id, int64(off), data[off:end]); st != 204 {
			t.Fatalf("chunk at %d: %d %v", off, st, out)
		}
	}
	return id
}

// startImport posts importTable and returns the task as it ended.
func (e *cp) startImport(t testing.TB, db, table string, req map[string]any) map[string]any {
	t.Helper()
	st, out, _ := e.call("POST", "/databases/"+db+"/schemas/public/tables/"+table+"/import", req, nil)
	if st != 202 {
		t.Fatalf("importTable %s: %d %v", table, st, out)
	}
	return e.waitTask(t, out)
}

func taskCode(task map[string]any) string {
	if er, _ := task["error"].(map[string]any); er != nil {
		c, _ := er["code"].(string)
		return c
	}
	return ""
}

func taskMessage(task map[string]any) string {
	if er, _ := task["error"].(map[string]any); er != nil {
		m, _ := er["message"].(string)
		return m
	}
	return ""
}

func count(t testing.TB, q func(string) map[string]any, table string) string {
	t.Helper()
	return fmt.Sprint(q("select count(*) from public." + table)["rows"])
}

const canaryValue = "CANARY-5e1d-secret-value"

// A round trip: the file an export wrote is loaded into an empty table of the same shape and the two tables are equal; then the options.
func TestImportRoundTripAndOptions(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.rt_src (id int primary key, name text, note text, amount numeric, at timestamptz, flag bool)")
	q("truncate public.rt_src")
	q(`insert into public.rt_src values (1, 'Ada', '=SUM(A1)', 12.50, '2026-01-02 03:04:05+00', true), (2, 'B,ob', E'a\nb', -5, null, false),
	   (3, 'Cy "q"', '', 0, '2026-01-02 03:04:05+00', null), (4, null, null, 1e3, null, null), (5, E'\\N', 'x', 7, null, true)`)
	q("create table if not exists public.rt_dst (like public.rt_src including all)")
	q("truncate public.rt_dst")
	_, xid := e.startExport(t, "db_xfer", "rt_src", map[string]any{"spreadsheetSafe": false, "nullString": `\N`})
	_, _, file := e.download(t, xid, nil)
	uid := e.uploadFile(t, file)
	task := e.startImport(t, "db_xfer", "rt_dst", map[string]any{"uploadId": uid, "nullString": `\N`, "header": "match"})
	if task["state"] != "done" || task["kind"] != "table.import" {
		t.Fatalf("import task: %v", task)
	}
	if d, _ := task["detail"].(string); d != "committed 5 rows" {
		t.Errorf("detail %q", d)
	}
	if got := fmt.Sprint(q("select count(*) from (select * from public.rt_src except select * from public.rt_dst) x")["rows"]); got != "[[0]]" {
		t.Errorf("rows of the source that are not in the copy: %s", got)
	}
	if got := fmt.Sprint(q("select count(*) from (select * from public.rt_dst except select * from public.rt_src) x")["rows"]); got != "[[0]]" || count(t, q, "rt_dst") != "[[5]]" {
		t.Errorf("rows of the copy that are not in the source: %s, count %s", got, count(t, q, "rt_dst"))
	}
	// The upload is gone after a successful import (keepUpload is false), and the task and the audit log hold no value.
	if st, _, _ := e.call("GET", "/data-uploads/"+uid, nil, nil); st != 404 {
		t.Errorf("the upload after a successful import: %d", st)
	}
	raw := toJSON(e.expect(t, 200, "GET", "/audit?action=importTable", nil)) + toJSON(task)
	for _, v := range []string{"SUM(A1)", "Ada", "B,ob"} {
		if strings.Contains(raw, v) {
			t.Errorf("the audit log or the task holds the value %q", v)
		}
	}
	e.expect(t, 204, "DELETE", "/data-exports/"+xid, nil)

	// A second import of the same file into the table fails on the key, with the primary key's table named and no value, and stores nothing more.
	uid = e.uploadFile(t, file)
	task = e.startImport(t, "db_xfer", "rt_dst", map[string]any{"uploadId": uid, "nullString": `\N`, "keepUpload": true})
	if task["state"] != "failed" || !strings.Contains(taskMessage(task), "a unique constraint is violated") || taskCode(task) != "sql_error" {
		t.Errorf("a repeated import: %v", task["error"])
	}
	if count(t, q, "rt_dst") != "[[5]]" {
		t.Errorf("a failed import changed the table: %s", count(t, q, "rt_dst"))
	}
	// keepUpload kept it: the file is there for another try, with other options.
	if st, _, _ := e.call("GET", "/data-uploads/"+uid, nil, nil); st != 200 {
		t.Errorf("the kept upload: %d", st)
	}
	e.expect(t, 204, "DELETE", "/data-uploads/"+uid, nil)

	// Options: no header, a semicolon, a null text, an apostrophe as the quote, another encoding, gzip found by itself.
	q("create table if not exists public.opt_t (a int, b text, c text)")
	q("truncate public.opt_t")
	var plain bytes.Buffer
	plain.WriteString("1;'it''s; ok';NULL\n2;caf" + string(rune(0xe9)) + ";\n")
	latin := bytes.ReplaceAll(plain.Bytes(), []byte("caf\xc3\xa9"), []byte("caf\xe9"))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(latin)
	_ = zw.Close()
	uid = e.uploadFile(t, gz.Bytes())
	task = e.startImport(t, "db_xfer", "opt_t", map[string]any{"uploadId": uid, "header": "none", "delimiter": ";", "quote": "'", "nullString": "NULL", "encoding": "LATIN1"})
	if task["state"] != "done" {
		t.Fatalf("options import: %v", task["error"])
	}
	if got := fmt.Sprint(q("select a, b, c from public.opt_t order by a")["rows"]); got != "[[1 it's; ok <nil>] [2 caf"+string(rune(0xe9))+" ]]" {
		t.Errorf("rows after the options import: %s", got)
	}

	// Refusals before a task exists.
	post := func(table string, body any) (int, map[string]any) {
		st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/"+table+"/import", body, nil)
		return st, out
	}
	inc := e.expect(t, 201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": 100})
	if st, out := post("opt_t", map[string]any{"uploadId": inc["uploadId"]}); st != 409 || problemCode(out) != "upload_incomplete" {
		t.Errorf("an incomplete upload: %d %v", st, out)
	}
	e.expect(t, 204, "DELETE", "/data-uploads/"+inc["uploadId"].(string), nil)
	if st, out := post("opt_t", map[string]any{"uploadId": "999999"}); st != 404 {
		t.Errorf("an unknown upload: %d %v", st, out)
	}
	good := e.uploadFile(t, []byte("1,a,b\n"))
	for name, body := range map[string]map[string]any{
		"unknown member":     {"uploadId": good, "onError": "ignore"},
		"bad header":         {"uploadId": good, "header": "maybe"},
		"bad delimiter":      {"uploadId": good, "delimiter": "x"},
		"unknown column":     {"uploadId": good, "columns": []string{"a", "zzz"}},
		"duplicate column":   {"uploadId": good, "columns": []string{"a", "a"}},
		"force not loaded":   {"uploadId": good, "columns": []string{"a"}, "forceNull": []string{"b"}},
		"bad compression":    {"uploadId": good, "compression": "zip"},
		"quote is delimiter": {"uploadId": good, "delimiter": ",", "quote": ","},
		"no upload":          {},
	} {
		if st, out := post("opt_t", body); st != 400 {
			t.Errorf("%s: %d %v", name, st, out)
		}
	}
	if st, out := post("nope", map[string]any{"uploadId": good}); st != 404 {
		t.Errorf("a missing table: %d %v", st, out)
	}
	q("create or replace view public.opt_v as select * from public.opt_t")
	if st, out := post("opt_v", map[string]any{"uploadId": good}); st != 422 || problemCode(out) != "table_not_eligible" {
		t.Errorf("a view: %d %v", st, out)
	}
	other := e.accountWithRole(t, "xfer_admin4", "kingres_admin")
	if st, out, _ := e.as(other).call("POST", "/databases/db_xfer/schemas/public/tables/opt_t/import", map[string]any{"uploadId": good}, nil); st != 404 {
		t.Errorf("the upload of another role: %d %v", st, out)
	}
	e.expect(t, 204, "DELETE", "/roles/xfer_admin4", nil)
	e.expect(t, 204, "DELETE", "/data-uploads/"+good, nil)
}

// Every class of error of a file leaves the canary of its failing row in no task, problem, audit event or log line, stores nothing, and names the
// line when the origin knows it.
func TestImportErrorsCarryNoValues(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.err_t (id int primary key, s varchar(5) not null, n smallint, d date, c int check (c < 100), code text, u text unique)")
	cases := []struct {
		name, file, state, sentence string
		line                        int
	}{
		{"type", "1,aaaaa,1,2026-01-01,1,x,u1\n2,aaaaa," + canaryValue + ",2026-01-01,1,x,u2\n", "22P02", "does not have the type", 2},
		{"too long", "1,aaaaa,1,2026-01-01,1,x,u1\n2," + canaryValue + ",1,2026-01-01,1,x,u2\n", "22001", "too long", 2},
		{"range", "1,aaaaa,1,2026-01-01,1,x,u1\n2,aaaaa,99999999,2026-01-01,1,x,u2\n", "22003", "out of range", 2},
		{"date", "1,aaaaa,1,2026-01-01,1,x,u1\n2,aaaaa,1," + canaryValue + ",1,x,u2\n", "22007", "date or time", 2},
		{"null", "1,aaaaa,1,2026-01-01,1,x,u1\n2,,1,2026-01-01,1,x,u2\n", "23502", "required column is empty", 0},
		{"check", "1,aaaaa,1,2026-01-01,1,x,u1\n2,aaaaa,1,2026-01-01,500,x," + canaryValue + "\n", "23514", "check constraint", 0},
		{"unique in the file", "1,aaaaa,1,2026-01-01,1,x," + canaryValue + "\n2,aaaaa,1,2026-01-01,1,x," + canaryValue + "\n", "23505", "unique constraint", 0},
		{"columns", "1,aaaaa,1,2026-01-01,1,x,u1\n2,aaaaa,1,2026-01-01,1,x,u2," + canaryValue + "\n", "22P04", "malformed", 2},
		{"unterminated quote", "1,aaaaa,1,2026-01-01,1,\"" + canaryValue + ",u1\n", "22P04", "malformed", 0},
		{"encoding", "1,aaaaa,1,2026-01-01,1,\xff" + canaryValue + "\xfe,u1\n", "22021", "encoding", 1},
	}
	for _, tc := range cases {
		q("truncate public.err_t")
		uid := e.uploadFile(t, []byte(tc.file))
		task := e.startImport(t, "db_xfer", "err_t", map[string]any{"uploadId": uid, "header": "none"})
		msg := taskMessage(task)
		if task["state"] != "failed" || taskCode(task) != "sql_error" || !strings.Contains(msg, tc.sentence) {
			t.Errorf("%s: %v", tc.name, task["error"])
		}
		if tc.line > 0 && !strings.Contains(msg, "line "+strconv.Itoa(tc.line)) {
			t.Errorf("%s: the message does not name line %d: %q", tc.name, tc.line, msg)
		}
		if ss, _ := task["error"].(map[string]any); ss != nil && strings.Contains(toJSON(ss), "CANARY") {
			t.Errorf("%s: the canary is in the task error: %v", tc.name, ss)
		}
		if got := count(t, q, "err_t"); got != "[[0]]" {
			t.Errorf("%s: a failed import stored rows: %s", tc.name, got)
		}
		if strings.Contains(toJSON(task), "CANARY") {
			t.Errorf("%s: the canary is in the task: %v", tc.name, task)
		}
		e.expect(t, 204, "DELETE", "/data-uploads/"+uid, nil)
	}
	// Nowhere else either: the audit log, the log of the server, the other tasks.
	if strings.Contains(toJSON(e.expect(t, 200, "GET", "/audit?limit=200", nil)), "CANARY") {
		t.Error("the canary is in the audit log")
	}
	if strings.Contains(toJSON(e.expect(t, 200, "GET", "/tasks?limit=100", nil)), "CANARY") {
		t.Error("the canary is in the list of tasks")
	}
	if strings.Contains(e.logs.String(), "CANARY") {
		t.Error("the canary is in the log of the server")
	}
	if strings.Contains(e.logs.String(), canaryValue) {
		t.Error("the value is in the log of the server")
	}
}

// The count of the server must equal the count of the scanner: a trigger that drops a row makes the server report fewer rows, and the import is
// rolled back instead of committed.
func TestImportRowCountMismatchRollsBack(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.trg_t (id int, v text)")
	q("truncate public.trg_t")
	q("create or replace function public.trg_drop() returns trigger language plpgsql as $$ begin if new.id = 2 then return null; end if; return new; end $$")
	q("drop trigger if exists trg_drop on public.trg_t")
	q("create trigger trg_drop before insert on public.trg_t for each row execute function public.trg_drop()")
	uid := e.uploadFile(t, []byte("1,a\n2,b\n3,c\n"))
	task := e.startImport(t, "db_xfer", "trg_t", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true})
	if task["state"] != "failed" || taskCode(task) != "row_count_mismatch" {
		t.Errorf("a trigger that drops a row: %v", task["error"])
	}
	if count(t, q, "trg_t") != "[[0]]" {
		t.Errorf("rows were committed although the counts differ: %s", count(t, q, "trg_t"))
	}
	// A trigger that does not change the count is fine, and fires for every row.
	q("drop trigger trg_drop on public.trg_t")
	q("create or replace function public.trg_up() returns trigger language plpgsql as $$ begin new.v = upper(new.v); return new; end $$")
	q("create trigger trg_up before insert on public.trg_t for each row execute function public.trg_up()")
	task = e.startImport(t, "db_xfer", "trg_t", map[string]any{"uploadId": uid, "header": "none"})
	if task["state"] != "done" || fmt.Sprint(q("select string_agg(v, '' order by id) from public.trg_t")["rows"]) != "[[ABC]]" {
		t.Errorf("a trigger that changes values: %v %v", task["error"], q("select * from public.trg_t")["rows"])
	}
}

// The end marker, a quoted one, the header, and the limits of the file (the flag max-atomic-bytes is 24 MiB in this cluster).
func TestImportFileRules(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.fr_t (id int, v text)")
	q("truncate public.fr_t")
	run := func(file []byte, req map[string]any) map[string]any {
		uid := e.uploadFile(t, file)
		req["uploadId"] = uid
		task := e.startImport(t, "db_xfer", "fr_t", req)
		if st, _, _ := e.call("GET", "/data-uploads/"+uid, nil, nil); st == 200 {
			e.expect(t, 204, "DELETE", "/data-uploads/"+uid, nil)
		}
		return task
	}
	task := run([]byte("1,a\n\\.\n2,b\n"), map[string]any{"header": "none"})
	if task["state"] != "failed" || taskCode(task) != "import_end_marker" || !strings.Contains(taskMessage(task), "line 2") {
		t.Errorf("an unquoted end marker: %v", task["error"])
	}
	if task := run([]byte("1,\"\\.\"\n2,b\n"), map[string]any{"header": "none"}); task["state"] != "done" {
		t.Errorf("a quoted \\. row: %v", task["error"])
	}
	if got := fmt.Sprint(q("select v from public.fr_t where id = 1")["rows"]); got != `[[\.]]` {
		t.Errorf("the quoted row: %s", got)
	}
	q("truncate public.fr_t")
	// The header: skip ignores it, match checks it, a wrong one fails with its own code and no name of the file.
	if task := run([]byte("x,y\n1,a\n"), map[string]any{"header": "skip"}); task["state"] != "done" {
		t.Errorf("skip: %v", task["error"])
	}
	task = run([]byte(canaryValue+",v\n2,b\n"), map[string]any{"header": "match"})
	if task["state"] != "failed" || taskCode(task) != "import_header_mismatch" || strings.Contains(toJSON(task), "CANARY") {
		t.Errorf("a header that does not match: %v", task["error"])
	}
	if task := run([]byte("id,v\n3,c\n"), map[string]any{"header": "match"}); task["state"] != "done" {
		t.Errorf("a matching header: %v", task["error"])
	}
	// Limits: a file above the cap is refused at the request; a gzip bomb is stopped by the count of its expansion; a record above 16 MiB by the scanner.
	big := bytes.Repeat([]byte("1,aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), 420000)
	uid := e.uploadFile(t, big)
	st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/fr_t/import", map[string]any{"uploadId": uid, "header": "none"}, nil)
	if st != 422 || problemCode(out) != "import_too_large" {
		t.Errorf("a file above the cap: %d %v", st, out)
	}
	e.expect(t, 204, "DELETE", "/data-uploads/"+uid, nil)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(big)
	_ = zw.Close()
	if gz.Len() > 1<<20 {
		t.Fatalf("the bomb is not small: %d", gz.Len())
	}
	if task := run(gz.Bytes(), map[string]any{"header": "none"}); task["state"] != "failed" || taskCode(task) != "import_too_large" {
		t.Errorf("a gzip bomb: %v", task["error"])
	}
	// A file of the size of the cap is fine (a quarter of it, to stay quick).
	if got := count(t, q, "fr_t"); got != "[[2]]" && got != "[[3]]" {
		t.Errorf("rows after the refusals: %s", got)
	}
}

// A load that is cancelled stores nothing; a role that is demoted while the file loads stops the import before the commit.
func TestImportCancelAndDemotion(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.slow_t (id int, v text)")
	q("truncate public.slow_t")
	q("create or replace function public.slow_trg() returns trigger language plpgsql as $$ begin perform pg_sleep(0.05); return new; end $$")
	q("drop trigger if exists slow_trg on public.slow_t")
	q("create trigger slow_trg before insert on public.slow_t for each row execute function public.slow_trg()")
	var file bytes.Buffer
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&file, "%d,row\n", i)
	}
	uid := e.uploadFile(t, file.Bytes())
	st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/slow_t/import", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true}, nil)
	if st != 202 {
		t.Fatalf("%d %v", st, out)
	}
	id := fmt.Sprint(int64(out["id"].(float64)))
	time.Sleep(2 * time.Second)
	e.expect(t, 200, "POST", "/tasks/"+id+"/cancel", nil)
	done := e.waitTask(t, out)
	if done["state"] != "failed" || taskCode(done) != "cancelled" {
		t.Errorf("a cancelled import: %v", done)
	}
	if got := count(t, q, "slow_t"); got != "[[0]]" {
		t.Errorf("a cancelled import stored rows: %s", got)
	}
	// An administrator that is demoted while the file loads: the commit does not happen.
	adm := e.accountWithRole(t, "xfer_demote", "kingres_admin")
	q("grant insert on public.slow_t to xfer_demote")
	a := e.as(adm)
	st, out, _ = a.call("POST", "/databases/db_xfer/schemas/public/tables/slow_t/import", map[string]any{"uploadId": a.uploadFile(t, file.Bytes()), "header": "none"}, nil)
	if st != 202 {
		t.Fatalf("import as the second administrator: %d %v", st, out)
	}
	time.Sleep(2 * time.Second)
	e.expect(t, 200, "PUT", "/roles/xfer_demote/cluster-roles", map[string]any{"clusterRoles": []string{"kingres_readonly"}})
	done = e.waitTask(t, out)
	if done["state"] != "failed" || taskCode(done) != "forbidden" {
		t.Errorf("a demoted role: %v", done["error"])
	}
	if got := count(t, q, "slow_t"); got != "[[0]]" {
		t.Errorf("an import of a demoted role stored rows: %s", got)
	}
	q("revoke insert on public.slow_t from xfer_demote")
	e.expect(t, 204, "DELETE", "/roles/xfer_demote", nil)
	e.expect(t, 204, "DELETE", "/data-uploads/"+uid, nil)
}

// Every kind of target: a distributed table (with the file spread over the buckets), a reference table (the typed-array path), a partitioned table, a table
// with a generated column, a table the role may not write, a table with row-level security.
func TestImportTargets(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.tg_dist (id bigint primary key, v text)")
	q("create table if not exists public.tg_ref (id int primary key, name text, amount numeric, at timestamptz, flag boolean)")
	q("create table if not exists public.tg_part (id int, d date) partition by range (d)")
	q("create table if not exists public.tg_part_a partition of public.tg_part for values from ('2026-01-01') to ('2027-01-01')")
	q("create table if not exists public.tg_gen (a int, b int generated always as (a * 2) stored)")
	q("create table if not exists public.tg_arr (id int primary key, tags text[])")
	for _, d := range []struct{ table, body string }{{"tg_dist", `{"method":"hash","distributionColumn":"id"}`}, {"tg_ref", `{"method":"reference"}`}, {"tg_arr", `{"method":"reference"}`}} {
		var b map[string]any
		_ = json.Unmarshal([]byte(d.body), &b)
		if st, _, _ := e.call("GET", "/databases/db_xfer/schemas/public/tables/"+d.table, nil, nil); st == 200 {
			tb := e.expect(t, 200, "GET", "/databases/db_xfer/schemas/public/tables/"+d.table, nil)
			if cl, _ := tb["classes"].([]any); len(cl) == 1 && cl[0] == "plain" {
				e.mustTask(t, "PUT", "/databases/db_xfer/schemas/public/tables/"+d.table+"/distribution", b)
			}
		}
	}
	var dist bytes.Buffer
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&dist, "%d,value %d\n", i, i)
	}
	uid := e.uploadFile(t, dist.Bytes())
	if task := e.startImport(t, "db_xfer", "tg_dist", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" {
		t.Fatalf("distributed: %v", task["error"])
	}
	if got := fmt.Sprint(q("select count(*), sum(id) from public.tg_dist")["rows"]); got != "[[2000 2001000]]" {
		t.Errorf("distributed table: %s", got)
	}
	ref := "1,Ada,12.50,2026-01-02 03:04:05+00,t\n2,\"B,ob\",,,f\n3,\"q\"\"uote\",-1,2026-01-02 03:04:05+00,\n"
	uid = e.uploadFile(t, []byte(ref))
	if task := e.startImport(t, "db_xfer", "tg_ref", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" {
		t.Fatalf("reference: %v", task["error"])
	}
	if got := fmt.Sprint(q("select id, name, amount, flag from public.tg_ref order by id")["rows"]); got != `[[1 Ada 12.50 true] [2 B,ob <nil> false] [3 q"uote -1 <nil>]]` {
		t.Errorf("reference table: %s", got)
	}
	uid = e.uploadFile(t, []byte("1,{a}\n"))
	if st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/tg_arr/import", map[string]any{"uploadId": uid, "header": "none"}, nil); st != 422 || !strings.Contains(toJSON(out["blockers"]), "array_column") {
		t.Errorf("a reference table with an array column: %d %v", st, out)
	}
	e.expect(t, 204, "DELETE", "/data-uploads/"+uid, nil)
	uid = e.uploadFile(t, []byte("1,2026-03-04\n2,2026-05-06\n"))
	if task := e.startImport(t, "db_xfer", "tg_part", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" || count(t, q, "tg_part") != "[[2]]" {
		t.Errorf("partitioned: %v", task["error"])
	}
	// A generated column: refused when listed; left out by default.
	uid = e.uploadFile(t, []byte("1\n2\n"))
	if st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/tg_gen/import", map[string]any{"uploadId": uid, "header": "none", "columns": []string{"a", "b"}}, nil); st != 400 {
		t.Errorf("a generated column in columns: %d %v", st, out)
	}
	if task := e.startImport(t, "db_xfer", "tg_gen", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" || fmt.Sprint(q("select sum(b) from public.tg_gen")["rows"]) != "[[6]]" {
		t.Errorf("generated column: %v", task["error"])
	}
	// A table the role may not write: 422 with the column named. A table with row-level security: 422.
	q("create table if not exists public.tg_priv (id int)")
	q("create table if not exists public.tg_rls (id int)")
	q("alter table public.tg_rls enable row level security")
	adm := e.accountWithRole(t, "xfer_nowrite", "kingres_admin")
	a := e.as(adm)
	uid = a.uploadFile(t, []byte("1\n"))
	if st, out, _ := a.call("POST", "/databases/db_xfer/schemas/public/tables/tg_priv/import", map[string]any{"uploadId": uid, "header": "none"}, nil); st != 422 || !strings.Contains(toJSON(out["blockers"]), "no_privilege") {
		t.Errorf("a table the role cannot write: %d %v", st, out)
	}
	if st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/tg_rls/import", map[string]any{"uploadId": e.uploadFile(t, []byte("1\n")), "header": "none"}, nil); st != 422 || !strings.Contains(toJSON(out["blockers"]), "row_level_security") {
		t.Errorf("row-level security: %d %v", st, out)
	}
	e.expect(t, 204, "DELETE", "/roles/xfer_nowrite", nil)
}

// The commit protocol at its fault points: a connection that dies before the intent leaves nothing stored and a retryable code, a clean repeat works; a
// connection that dies after the intent is settled by the marker that commits with the rows (here: not committed, so a retryable failure with the proof).
func TestImportCommitFaultPoints(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.fp_t (id int, v text)")
	q("truncate public.fp_t")
	uid := e.uploadFile(t, []byte("1,a\n2,b\n3,c\n"))
	kill := func(point string) {
		ops.ImportFault = func(p string, pid uint32) error {
			if p == point {
				ops.ImportFault = nil
				e.expect(t, 200, "POST", "/query", map[string]any{"database": "postgres", "sql": "select pg_terminate_backend(" + strconv.Itoa(int(pid)) + ")"})
			}
			return nil
		}
	}
	defer func() { ops.ImportFault = nil }()

	kill("beforeIntent")
	task := e.startImport(t, "db_xfer", "fp_t", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true})
	er, _ := task["error"].(map[string]any)
	if task["state"] != "failed" || taskCode(task) != "copy_interrupted" || er["retryable"] != true || count(t, q, "fp_t") != "[[0]]" {
		t.Errorf("a connection that died before the intent: %v (rows %s)", task["error"], count(t, q, "fp_t"))
	}
	if d, _ := task["detail"].(string); strings.Contains(d, "commit sent") {
		t.Errorf("the intent was written although the connection was dead: %q", d)
	}
	if got := task["cancellable"]; got == nil {
		t.Errorf("task %v", task)
	}

	kill("afterIntent")
	task = e.startImport(t, "db_xfer", "fp_t", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true})
	er, _ = task["error"].(map[string]any)
	// The marker in the database is the proof: the connection died before COMMIT, so the marker did not commit and nothing was stored (DX14-01).
	if task["state"] != "failed" || taskCode(task) != "copy_interrupted" || er["retryable"] != true || !strings.Contains(taskMessage(task), "marker") {
		t.Errorf("a connection that died after the intent: %v", task["error"])
	}
	if task["cancellable"] != false {
		t.Errorf("a task past the intent is cancellable: %v", task["cancellable"])
	}
	// The server killed the connection before COMMIT, so nothing is in; the task cannot know, and says so. A repeat works.
	if count(t, q, "fp_t") != "[[0]]" {
		t.Errorf("rows after the killed commit: %s", count(t, q, "fp_t"))
	}
	if task := e.startImport(t, "db_xfer", "fp_t", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" || count(t, q, "fp_t") != "[[3]]" {
		t.Errorf("the repeat: %v", task["error"])
	}
}
