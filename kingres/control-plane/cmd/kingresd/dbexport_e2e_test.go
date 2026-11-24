// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlshape"
)

// End-to-end tests of the database export and the archive validation (contract 0.29.0) against a real kingresd and a real PostgreSQL instance.

// startDBExport posts exportDatabase and returns the status, the body and, when it was accepted, nothing more.
func (e *cp) startDBExport(t testing.TB, db string, req map[string]any) (int, map[string]any) {
	t.Helper()
	st, out, _ := e.call("POST", "/databases/"+db+"/export", req, nil)
	return st, out
}

// exportDB exports a database and returns the id of the export.
func (e *cp) exportDB(t testing.TB, db string, req map[string]any) string {
	t.Helper()
	st, out := e.startDBExport(t, db, req)
	if st != 202 {
		t.Fatalf("exportDatabase %s: %d %v", db, st, out)
	}
	validTask(t, out)
	done := e.waitTask(t, out)
	if done["state"] != "done" {
		t.Fatalf("the export task failed: %v", done["error"])
	}
	d, _ := done["detail"].(string)
	if !strings.HasPrefix(d, "transfer ") {
		t.Fatalf("task detail %q", d)
	}
	return strings.TrimPrefix(d, "transfer ")
}

// checkArchive reads an archive through the validator of the importer, with the shape grammar, and returns the result.
func checkArchive(t testing.TB, b []byte) *archive.Result {
	t.Helper()
	res, err := archive.Validate(bytes.NewReader(b), int64(len(b)), archive.Limits{MaxImportBytes: 1 << 30, SQLFor: func(cat *archive.Catalog) archive.SQLChecker {
		hints := map[string]bool{}
		for _, ct := range cat.Tables {
			if ct.Parent != "" && (ct.TreeRole == "inheritanceChild" || ct.TreeRole == "inheritanceParent") {
				hints[ct.Qualified()] = true
			}
		}
		return func(name string, r io.Reader) (archive.SQLResult, error) {
			plan, err := sqlshape.ParseFile(r, name, sqlshape.Hints{InheritanceChildren: hints})
			if err != nil {
				return archive.SQLResult{}, err
			}
			out := archive.SQLResult{ByKind: plan.ByKind}
			for _, rf := range plan.Refused {
				out.Refused = append(out.Refused, archive.RefusedStatement{Kind: rf.Kind, Position: rf.Position, Fingerprint: rf.Fingerprint, Reason: rf.Reason})
			}
			return out, nil
		}
	}})
	if err != nil {
		t.Fatalf("the archive is not valid: %v", err)
	}
	for n, sr := range res.SQL {
		if len(sr.Refused) > 0 {
			t.Errorf("%s: the grammar refuses %v", n, sr.Refused)
		}
	}
	return res
}

func zipEntry(t testing.TB, b []byte, name string) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rc.Close() }()
			out, _ := io.ReadAll(rc)
			return string(out)
		}
	}
	t.Fatalf("the archive has no entry %s", name)
	return ""
}

// uploadArchive sends a file as an upload for a database archive and returns the id.
func (e *cp) uploadArchive(t testing.TB, data []byte) string {
	t.Helper()
	up := e.expect(t, 201, "POST", "/data-uploads", map[string]any{"purpose": "databaseArchive", "sizeBytes": len(data)})
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

func (e *cp) validateArchive(t testing.TB, uploadID string) map[string]any {
	t.Helper()
	st, out, _ := e.call("POST", "/data-uploads/"+uploadID+"/validation", nil, nil)
	if st != 202 {
		t.Fatalf("validateDatabaseArchive: %d %v", st, out)
	}
	validTask(t, out)
	done := e.waitTask(t, out)
	if done["state"] != "done" {
		t.Fatalf("the validation task failed: %v", done["error"])
	}
	return e.expect(t, 200, "GET", "/data-uploads/"+uploadID, nil)
}

func TestDatabaseExportOfEveryTableKindAndItsValidation(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	buildExportFixture(t, e, "dbexp")
	id := e.exportDB(t, "dbexp", map[string]any{})
	exp := e.expect(t, 200, "GET", "/data-exports/"+id, nil)
	valid(t, "getDataExport", "200", exp)
	if exp["state"] != "ready" || exp["kind"] != "databaseExport" || exp["format"] != "archive/1" || exp["database"] != "dbexp" || exp["table"] != nil {
		t.Fatalf("export: %v", exp)
	}
	cons, _ := exp["consistency"].([]any)
	if len(cons) < 10 {
		t.Fatalf("consistency %v", exp["consistency"])
	}
	for _, c := range cons {
		// One server, one group: every table is read in one transaction.
		if lv := c.(map[string]any)["level"]; lv != "archive-single-snapshot" {
			t.Errorf("label %v", c)
		}
	}
	st, h, body := e.download(t, id, nil)
	if st != 200 || h.Get("Content-Type") != "application/zip" || !strings.HasPrefix(h.Get("Content-Disposition"), `attachment; filename="dbexp_`) || !strings.HasSuffix(h.Get("Content-Disposition"), `.kdx.zip"`) ||
		exp["sha256"] != sumHex(body) || exp["sizeBytes"] != float64(len(body)) {
		t.Fatalf("download: %d %v (%d bytes) %v", st, h, len(body), exp)
	}
	res := checkArchive(t, body)
	m := res.Manifest
	if m.Source.Database != "dbexp" || m.Format != "kingres-export/1" || m.Source.PostgresMajor < 18 || m.Fingerprint == "" {
		t.Errorf("manifest source: %+v", m.Source)
	}
	// The rows of every table, the partition roots only, the history of the updatable ledger table for the record.
	want := map[string]int64{"public.t_plain": 4, "public.t_child": 3, "public.inh_parent": 1, "public.inh_child": 1, "public.lp": 2, "app.items": 2, "public.refdata": 2, "public.d_hash": 200,
		"public.d_part": 61, "public.c_events": 300, "public.acc": 2, "public.audit_rows": 1, "public.ev": 1}
	got := map[string]int64{}
	history := 0
	for _, en := range m.Entries {
		if en.Table != "" && !en.LedgerHistory {
			got[en.Table] = en.Rows
		}
		if en.LedgerHistory {
			history++
		}
	}
	for tb, rows := range want {
		if r, ok := got[tb]; !ok || r != rows {
			t.Errorf("table %s: %d rows (%v), want %d", tb, r, ok, rows)
		}
	}
	for tb := range got {
		if _, ok := want[tb]; !ok {
			t.Errorf("unexpected table %s in the archive", tb)
		}
	}
	if history != 1 {
		t.Errorf("history entries: %d", history)
	}
	kinds := map[string]string{}
	roles := map[string]string{}
	for _, ct := range res.Catalog.Tables {
		kinds[ct.Qualified()] = ct.Kind + "/" + ct.AccessMethod
		roles[ct.Qualified()] = ct.TreeRole
	}
	for tb, k := range map[string]string{"public.d_hash": "hash/heap", "public.d_part": "hash/heap", "public.refdata": "reference/heap", "public.c_events": "local/columnar", "public.t_plain": "local/heap"} {
		if kinds[tb] != k {
			t.Errorf("catalog: %s is %q, want %q", tb, kinds[tb], k)
		}
	}
	if roles["public.lp"] != "root" || roles["public.lp_2026"] != "leaf" || roles["public.inh_parent"] != "inheritanceParent" || roles["public.inh_child"] != "inheritanceChild" || roles["public.d_part"] != "root" {
		t.Errorf("tree roles: %v", roles)
	}
	for _, ct := range res.Catalog.Tables {
		if ct.Qualified() == "public.d_part" && (ct.UserPartitioning == nil || len(ct.UserPartitioning.Partitions) != 2 || ct.UserPartitioning.Method != "range" || ct.UserPartitioning.Key != "ts") {
			t.Errorf("user partitioning of d_part: %+v", ct.UserPartitioning)
		}
		if ct.Qualified() == "public.acc" && (ct.Ledger == nil || ct.Ledger.Mode != "updatable" || len(ct.Ledger.HistoryColumns) == 0) {
			t.Errorf("ledger of acc: %+v", ct.Ledger)
		}
	}
	if len(res.Catalog.TimepartPolicies) != 1 || res.Catalog.TimepartPolicies[0].Table != "ev" || len(res.Catalog.Sequences) < 2 {
		t.Errorf("policies %+v sequences %+v", res.Catalog.TimepartPolicies, res.Catalog.Sequences)
	}
	// A null is the unquoted marker, an empty string is "" and a text that is \N is quoted.
	var plainEntry string
	for _, en := range m.Entries {
		if en.Table == "public.t_plain" {
			plainEntry = en.Name
		}
	}
	csvText := zipEntry(t, body, plainEntry)
	for _, wantText := range []string{"id,name,mood,qty,tags,at,n\n", `"2","",\N,\N,\N,`, `"3","\N","happy","1","{}"`, "\"line\nbreak, \"\"q\"\"\""} {
		if !strings.Contains(csvText, wantText) {
			t.Errorf("the CSV of t_plain lacks %q:\n%s", wantText, csvText)
		}
	}
	schema := zipEntry(t, body, "schema/pre-data.sql") + zipEntry(t, body, "schema/views.sql") + zipEntry(t, body, "schema/post-data.sql")
	for _, bad := range []string{"_b0", "CREATE EXTENSION", "kingres_ledger", "acc_ledger_history", "PARTITION BY HASH", "restrict"} {
		if strings.Contains(schema, bad) {
			t.Errorf("the schema files hold %q", bad)
		}
	}
	// The same archive, uploaded and validated: valid, with the report of the contract.
	up := e.uploadArchive(t, body)
	rep := e.validateArchive(t, up)
	valid(t, "getDataUpload", "200", rep)
	if rep["state"] != "valid" {
		t.Fatalf("upload after the validation: %v", rep)
	}
	r, _ := rep["report"].(map[string]any)
	tabs, _ := r["tables"].(map[string]any)
	needs, _ := r["needs"].(map[string]any)
	if r["valid"] != true || r["sha256"] != sumHex(body) || tabs["hash"] != float64(2) || tabs["reference"] != float64(1) || tabs["ledger"] != float64(2) || tabs["columnar"] != float64(1) || needs["ledgerRestart"] != true {
		t.Errorf("report: %v", r)
	}
	// The archive is not a CSV: an import refuses it.
	if st, out, _ := e.call("POST", "/databases/dbexp/schemas/public/tables/refdata/import", map[string]any{"uploadId": up}, nil); st != 400 {
		t.Errorf("import of an archive: %d %v", st, out)
	}
	// A second validation of the same bytes is answered from the cache of the first.
	rep2 := e.validateArchive(t, up)
	if rep2["state"] != "valid" {
		t.Errorf("second validation: %v", rep2)
	}
	// A CSV upload cannot be validated as an archive.
	csvUp := e.uploadFile(t, []byte("a,b\n1,2\n"))
	if st, out, _ := e.call("POST", "/data-uploads/"+csvUp+"/validation", nil, nil); st != 400 && st != 422 {
		t.Errorf("validation of a CSV upload: %d %v", st, out)
	}
	// A damaged archive is a finished task with a report that says why.
	bad := append([]byte{}, body...)
	bad[len(bad)/2] ^= 0xff
	badUp := e.uploadArchive(t, bad)
	brep := e.validateArchive(t, badUp)
	valid(t, "getDataUpload", "200", brep)
	if brep["state"] != "invalid" {
		t.Fatalf("damaged archive: %v", brep)
	}
	if rr, _ := brep["report"].(map[string]any); rr["valid"] != false || len(rr["reasons"].([]any)) == 0 {
		t.Errorf("damaged archive report: %v", brep["report"])
	}
	// The audit log names the operation and holds no value.
	audit := e.expect(t, 200, "GET", "/audit?action=exportDatabase", nil)
	if items, _ := audit["items"].([]any); len(items) == 0 {
		t.Errorf("no audit event of exportDatabase: %v", audit)
	}
}

func TestDatabaseExportRefusals(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	// An unknown database and bad options.
	if st, out := e.startDBExport(t, "no_such_db", map[string]any{}); st != 404 {
		t.Errorf("unknown database: %d %v", st, out)
	}
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "dbx_plain"})
	q := func(db, sql string) map[string]any {
		return e.expect(t, 200, "POST", "/query", map[string]any{"database": db, "sql": sql})
	}
	q("dbx_plain", "create table public.t (id int primary key)")
	for _, bad := range []map[string]any{{"dataMode": "some"}, {"retentionHours": 0}, {"excludeTables": []any{map[string]any{"schema": "public", "table": "nope"}}}, {"sql": "select 1"}} {
		if st, out := e.startDBExport(t, "dbx_plain", bad); st != 400 {
			t.Errorf("request %v: %d %v", bad, st, out)
		}
	}
	// Objects that an archive does not carry: a function.
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "dbx_fn"})
	q("dbx_fn", "create function public.f() returns int language sql as 'select 1'")
	if st, out := e.startDBExport(t, "dbx_fn", map[string]any{}); st != 422 || out["code"] != "schema_unrecognised" || !strings.Contains(out["detail"].(string), "function") {
		t.Errorf("a function: %d %v", st, out)
	}
	// Large objects.
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "dbx_lo"})
	q("dbx_lo", "create table public.t (id int primary key)")
	c := e.serviceConn(t, "dbx_lo")
	if _, err := c.Exec(context.Background(), "select lo_create(0)"); err != nil {
		t.Fatal(err)
	}
	if st, out := e.startDBExport(t, "dbx_lo", map[string]any{}); st != 422 || out["code"] != "large_objects_present" {
		t.Errorf("large objects: %d %v", st, out)
	}
	id := e.exportDB(t, "dbx_lo", map[string]any{"acknowledgeLargeObjectsLost": true})
	_, _, body := e.download(t, id, nil)
	res := checkArchive(t, body)
	found := false
	for _, w := range res.Manifest.Warnings {
		found = found || strings.Contains(w, "large objects")
	}
	if !found {
		t.Errorf("the warnings do not say that large objects are lost: %v", res.Manifest.Warnings)
	}
	// A role that cannot read: the first 20 objects are named.
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "dbx_priv"})
	for i := 0; i < 25; i++ {
		q("dbx_priv", "create table public.t"+string(rune('a'+i%26))+string(rune('a'+i/26))+" (id int)")
	}
	pw := "Viewer-Passw0rd-1"
	e.expect(t, 201, "POST", "/roles", map[string]any{"name": "dbx_noread", "password": pw, "clusterRoles": []string{"kingres_admin"}})
	tok, ok := e.loginAs("dbx_noread", pw)
	if !ok {
		t.Fatal("sign in of the role without privileges")
	}
	other := e.as(tok)
	st, out, _ := other.call("POST", "/databases/dbx_priv/export", map[string]any{}, nil)
	if st != 422 || out["code"] != "export_not_readable" {
		t.Fatalf("a role that cannot read: %d %v", st, out)
	}
	if rs, _ := out["reasons"].([]any); len(rs) != 20 || !strings.Contains(out["detail"].(string), "25 objects") {
		t.Errorf("reasons %d, detail %v", len(rs), out["detail"])
	}
	// Excluding a table leaves its rows out and keeps its structure.
	q("dbx_plain", "insert into public.t values (1), (2)")
	id = e.exportDB(t, "dbx_plain", map[string]any{"excludeTables": []any{map[string]any{"schema": "public", "table": "t"}}})
	_, _, body = e.download(t, id, nil)
	res = checkArchive(t, body)
	if len(res.Manifest.Options.ExcludedTables) != 1 || res.Manifest.Totals.Tables != 0 || !strings.Contains(zipEntry(t, body, "schema/pre-data.sql"), "CREATE TABLE public.t") {
		t.Errorf("excluded table: %+v", res.Manifest.Options)
	}
	id = e.exportDB(t, "dbx_plain", map[string]any{"dataMode": "none"})
	_, _, body = e.download(t, id, nil)
	res = checkArchive(t, body)
	if res.Manifest.Options.DataMode != "none" || res.Manifest.Totals.Tables != 0 {
		t.Errorf("data mode none: %+v", res.Manifest)
	}
}

// A table task of the database waits while an export runs (and the reverse), a cancel leaves no file, and a column added while the export runs fails it with
// schema_changed.
func TestDatabaseExportBlocksTableTasksAndNoticesChanges(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "dbx_race", "sharding": map[string]any{"enabled": true, "bucketCount": 8}})
	q := func(sql string) {
		e.expect(t, 200, "POST", "/query", map[string]any{"database": "dbx_race", "sql": sql})
	}
	q("create table public.a (id bigint primary key, v text)")
	q("create table public.b (id bigint primary key, v text)")
	q("insert into public.a values (1, 'x'), (2, 'y')")

	var mu sync.Mutex
	reached := make(chan struct{}, 4)
	release := make(chan struct{})
	var stepToHold string
	ops.DBExportHook = func(ctx context.Context, step string) error {
		mu.Lock()
		hold := step == stepToHold
		mu.Unlock()
		if hold {
			reached <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	defer func() { ops.DBExportHook = nil }()
	hold := func(step string) {
		mu.Lock()
		stepToHold = step
		mu.Unlock()
	}

	// 1. While the data is written, a table task of the database is refused; the export goes on.
	hold("Write the data")
	st, out := e.startDBExport(t, "dbx_race", map[string]any{})
	if st != 202 {
		t.Fatalf("export: %d %v", st, out)
	}
	select {
	case <-reached:
	case <-time.After(2 * time.Minute):
		t.Fatal("the export did not reach the data step")
	}
	if st, o, _ := e.call("PUT", "/databases/dbx_race/schemas/public/tables/b/distribution", map[string]any{"method": "hash", "distributionColumn": "id"}, nil); st != http.StatusConflict || o["code"] != "operation_in_progress" {
		t.Errorf("a table task during the export: %d %v", st, o)
	}
	// 2. The cancel ends it; no file is kept.
	if st, o, _ := e.call("POST", "/tasks/"+idOf(out)+"/cancel", nil, nil); st >= 300 {
		t.Fatalf("cancel: %d %v", st, o)
	}
	close(release)
	done := e.waitTask(t, out)
	if done["state"] != "failed" {
		t.Fatalf("the cancelled export: %v", done["state"])
	}
	list := e.expect(t, 200, "GET", "/data-exports", nil)
	for _, it := range list["items"].([]any) {
		m := it.(map[string]any)
		if m["database"] == "dbx_race" && m["state"] != "failed" {
			t.Errorf("an export of the cancelled task is %v", m["state"])
		}
		if m["database"] == "dbx_race" {
			if st, _, _, _ := e.raw("GET", "/data-exports/"+m["exportId"].(string)+"/content", nil, -1, nil); st != 404 {
				t.Errorf("download of a cancelled export: %d", st)
			}
		}
	}
	// With the export over, the table task is allowed.
	e.mustTask(t, "PUT", "/databases/dbx_race/schemas/public/tables/b/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})

	// 3. A column added while the export runs: the fingerprint differs at the end.
	release = make(chan struct{})
	hold("Write the data")
	st, out = e.startDBExport(t, "dbx_race", map[string]any{})
	if st != 202 {
		t.Fatalf("export: %d %v", st, out)
	}
	<-reached
	c := e.serviceConn(t, "dbx_race")
	if _, err := c.Exec(context.Background(), "alter table public.a add column z int"); err != nil {
		t.Fatal(err)
	}
	close(release)
	done = e.waitTask(t, out)
	if done["state"] != "failed" || done["error"].(map[string]any)["code"] != "schema_changed" {
		t.Fatalf("the export after a change of the structure: %v %v", done["state"], done["error"])
	}
	hold("")
	// The next export sees the new structure and succeeds.
	id := e.exportDB(t, "dbx_race", map[string]any{})
	_, _, body := e.download(t, id, nil)
	checkArchive(t, body)
}

func idOf(task map[string]any) string {
	if v, ok := task["id"].(float64); ok {
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

// A database whose archive grows beyond the largest staged file stops with export_too_large and leaves no file; the same database passes under a larger limit.
func TestDatabaseExportTooLarge(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "dbx_big"})
	q := func(sql string) {
		e.expect(t, 200, "POST", "/query", map[string]any{"database": "dbx_big", "sql": sql})
	}
	q("create table public.big (id int primary key, a text, b text)")
	q("insert into public.big select g, md5(g::text) || md5((g + 1)::text), md5((g + 2)::text) || md5((g + 3)::text) from generate_series(1, 60000) g")
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"maxFileBytes": 1 << 20}})
	st, out := e.startDBExport(t, "dbx_big", map[string]any{})
	if st != 202 {
		t.Fatalf("export: %d %v", st, out)
	}
	done := e.waitTask(t, out)
	if done["state"] != "failed" || done["error"].(map[string]any)["code"] != "export_too_large" {
		t.Fatalf("the export of a database larger than the largest file: %v %v", done["state"], done["error"])
	}
	list := e.expect(t, 200, "GET", "/data-exports", nil)
	for _, it := range list["items"].([]any) {
		m := it.(map[string]any)
		if m["database"] == "dbx_big" && m["state"] != "failed" {
			t.Errorf("the export that was too large is %v", m["state"])
		}
	}
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"maxFileBytes": 1 << 30}})
	id := e.exportDB(t, "dbx_big", map[string]any{})
	_, _, body := e.download(t, id, nil)
	res := checkArchive(t, body)
	if res.Manifest.Totals.Rows != 60000 {
		t.Errorf("rows: %d", res.Manifest.Totals.Rows)
	}
}
