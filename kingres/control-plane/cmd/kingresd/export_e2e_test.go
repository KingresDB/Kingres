// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// xferDB makes (once per test) a database with the sharding extension and returns a query function for it.
func xferDB(t testing.TB, e *cp, name string) func(sql string) map[string]any {
	t.Helper()
	if st, _, _ := e.call("GET", "/databases/"+name, nil, nil); st == 404 {
		e.mustTask(t, "POST", "/databases", map[string]any{"name": name, "sharding": map[string]any{"enabled": true, "bucketCount": 8}, "replication": map[string]any{"durability": "async"}, "ledger": map[string]any{"mode": "off"}})
	}
	return func(sql string) map[string]any {
		return e.expect(t, 200, "POST", "/query", map[string]any{"database": name, "sql": sql})
	}
}

// startExport posts exportTable, waits for the task and returns the task and the id of the export it names.
func (e *cp) startExport(t testing.TB, db, table string, req map[string]any) (map[string]any, string) {
	t.Helper()
	st, out, _ := e.call("POST", "/databases/"+db+"/schemas/public/tables/"+table+"/export", req, nil)
	if st != 202 {
		t.Fatalf("exportTable %s: %d %v", table, st, out)
	}
	done := e.waitTask(t, out)
	if done["state"] != "done" {
		t.Fatalf("the export task failed: %v", done["error"])
	}
	d, _ := done["detail"].(string)
	if !strings.HasPrefix(d, "transfer ") {
		t.Fatalf("task detail %q", d)
	}
	return done, strings.TrimPrefix(d, "transfer ")
}

func (e *cp) download(t testing.TB, id string, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	st, h, b, err := e.raw("GET", "/data-exports/"+id+"/content", nil, -1, headers)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	return st, h, b
}

// A table export of a local table: the file equals the table, the record says what is true of it, a download is whole and by range, another
// administrator sees nothing, and the audit log has no value.
func TestExportLocalTableAndDownload(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table public.t_local (id int primary key, name text, note text, amount numeric, at timestamptz, flag bool)")
	q(`insert into public.t_local values
	   (1, 'Ada', '=SUM(A1)', 12.50, '2026-01-02 03:04:05+00', true),
	   (2, 'B,ob', '+1', -5, null, false),
	   (3, 'Cy "q"', '-2', 0, '2026-01-02 03:04:05+00', null),
	   (4, null, '@x', 1e3, null, null),
	   (5, '', E'line\nbreak', 7, null, true),
	   (6, E'\ttab', '|cmd', 8, null, true),
	   (7, 'plain', '%x', 9, null, true)`)

	// With the spreadsheet guard (the default): text cells that start like a formula get an apostrophe, numbers are not touched.
	_, id := e.startExport(t, "db_xfer", "t_local", map[string]any{})
	exp := e.expect(t, 200, "GET", "/data-exports/"+id, nil)
	valid(t, "getDataExport", "200", exp)
	if exp["state"] != "ready" || exp["rows"] != float64(7) || exp["format"] != "csv" || exp["kind"] != "tableExport" || exp["table"] != "t_local" || exp["database"] != "db_xfer" || exp["rlsApplied"] != false {
		t.Fatalf("export: %v", exp)
	}
	cons, _ := exp["consistency"].([]any)
	if len(cons) != 1 || cons[0].(map[string]any)["level"] != "single-snapshot" || cons[0].(map[string]any)["table"] != "public.t_local" {
		t.Errorf("consistency %v", exp["consistency"])
	}
	st, h, body := e.download(t, id, nil)
	if st != 200 || h.Get("Content-Length") != strconv.Itoa(len(body)) || exp["sizeBytes"] != float64(len(body)) || exp["sha256"] != sumHex(body) ||
		h.Get("ETag") != `"`+sumHex(body)+`"` || h.Get("Accept-Ranges") != "bytes" || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(h.Get("Content-Type"), "text/csv; charset=utf8") || !strings.HasPrefix(h.Get("Content-Disposition"), `attachment; filename="db_xfer_t_local_`) {
		t.Fatalf("download: %d %v (%d bytes) export %v", st, h, len(body), exp)
	}
	text := string(body)
	for _, want := range []string{"id,name,note,amount,at,flag\n", "1,Ada,'=SUM(A1),12.50,2026-01-02 03:04:05+00,t\n", `2,"B,ob",'+1,-5,,f` + "\n", `3,"Cy ""q""",'-2,0,`, "4,,'@x,1000,,\n", `5,"",` + "\"line\nbreak\"", "'|cmd", "'%x"} {
		if !strings.Contains(text, want) {
			t.Errorf("the file lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, ",=SUM") {
		t.Errorf("a formula was written as it is:\n%s", text)
	}
	// Ranges: a part, the rest, the last bytes, and what cannot be served.
	if st, h, part := e.download(t, id, map[string]string{"Range": "bytes=3-9"}); st != 206 || string(part) != text[3:10] || h.Get("Content-Range") != "bytes 3-9/"+strconv.Itoa(len(body)) || h.Get("Content-Length") != "7" {
		t.Errorf("range a-b: %d %v %q", st, h, part)
	}
	if st, _, part := e.download(t, id, map[string]string{"Range": "bytes=" + strconv.Itoa(len(body)-5) + "-"}); st != 206 || string(part) != text[len(text)-5:] {
		t.Errorf("open range: %d %q", st, part)
	}
	if st, _, part := e.download(t, id, map[string]string{"Range": "bytes=-4"}); st != 206 || string(part) != text[len(text)-4:] {
		t.Errorf("suffix range: %d %q", st, part)
	}
	if st, h, _ := e.download(t, id, map[string]string{"Range": "bytes=" + strconv.Itoa(len(body)+10) + "-"}); st != 416 || h.Get("Content-Range") != "bytes */"+strconv.Itoa(len(body)) {
		t.Errorf("range past the end: %d %v", st, h)
	}
	if st, _, part := e.download(t, id, map[string]string{"Range": "bytes=0-1,3-4"}); st != 200 || len(part) != len(body) {
		t.Errorf("two ranges are the whole file: %d %d", st, len(part))
	}
	// The creator downloaded the whole file: the record says so.
	if got := e.expect(t, 200, "GET", "/data-exports/"+id, nil); got["downloadedAt"] == nil {
		t.Errorf("downloadedAt is not set after the last byte: %v", got)
	}
	// The list holds the file; another administrator has none, cannot read or download it, and may delete it.
	list := e.expect(t, 200, "GET", "/data-exports", nil)
	valid(t, "listDataExports", "200", list)
	found := false
	for _, it := range list["items"].([]any) {
		found = found || it.(map[string]any)["exportId"] == id
	}
	if !found {
		t.Errorf("the export is not in the list: %v", list)
	}
	other := e.accountWithRole(t, "xfer_admin3", "kingres_admin")
	o2 := e.as(other)
	if l := o2.expect(t, 200, "GET", "/data-exports", nil); len(l["items"].([]any)) != 0 {
		t.Errorf("another administrator lists the file: %v", l)
	}
	if st, _, _ := o2.call("GET", "/data-exports/"+id, nil, nil); st != 404 {
		t.Errorf("get by another administrator: %d", st)
	}
	if st, _, _, _ := o2.raw("GET", "/data-exports/"+id+"/content", nil, -1, nil); st != 404 {
		t.Errorf("download by another administrator: %d", st)
	}
	if st, _, _ := o2.call("DELETE", "/data-exports/"+id, nil, nil); st != 204 {
		t.Errorf("delete by another administrator: %d", st)
	}
	if st, _, _ := e.call("GET", "/data-exports/"+id, nil, nil); st != 404 {
		t.Errorf("a deleted export: %d", st)
	}
	e.expect(t, 204, "DELETE", "/roles/xfer_admin3", nil)

	// The audit log says what happened and holds no value of the data.
	for _, action := range []string{"exportTable", "downloadDataExport", "deleteDataExport"} {
		ev := e.expect(t, 200, "GET", "/audit?action="+action, nil)
		if len(ev["items"].([]any)) == 0 {
			t.Errorf("no audit event %s", action)
		}
		raw := toJSON(ev)
		for _, secret := range []string{"=SUM", "Ada", "B,ob", "plain"} {
			if strings.Contains(raw, secret) {
				t.Errorf("the audit event %s holds data %q", action, secret)
			}
		}
	}
	if ev := e.expect(t, 200, "GET", "/audit?action=downloadDataExport", nil); !strings.Contains(toJSON(ev), "lastByteServed") {
		t.Errorf("the download event does not say whether the last byte was served: %v", ev)
	}

	// Without the guard the file is exact, and the options change the format: a semicolon, a null text, header off, a byte order mark.
	_, id2 := e.startExport(t, "db_xfer", "t_local", map[string]any{"spreadsheetSafe": false, "delimiter": ";", "nullString": "NULL", "header": false, "utf8Bom": true, "columns": []string{"id", "note", "at"}})
	_, _, raw := e.download(t, id2, nil)
	if !bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) || !strings.Contains(string(raw), "1;=SUM(A1);2026-01-02 03:04:05+00\n") || !strings.Contains(string(raw), "2;+1;NULL\n") || strings.Contains(string(raw), "id;note") {
		t.Errorf("options: %q", raw)
	}
	// Compression and another encoding.
	q("create table public.t_enc (id int, w text)")
	q("insert into public.t_enc values (1, 'caf" + string(rune(0xe9)) + "')")
	_, id3 := e.startExport(t, "db_xfer", "t_enc", map[string]any{"encoding": "LATIN1", "compression": "gzip"})
	st, h, gz := e.download(t, id3, nil)
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil || st != 200 || h.Get("Content-Type") != "application/gzip" || !strings.HasSuffix(h.Get("Content-Disposition"), `.csv.gz"`) {
		t.Fatalf("gzip download: %d %v %v", st, h, err)
	}
	plain, _ := io.ReadAll(zr)
	if !bytes.Contains(plain, []byte("1,caf\xe9\n")) {
		t.Errorf("LATIN1 inside gzip: %q", plain)
	}
	for _, i := range []string{id2, id3} {
		e.expect(t, 204, "DELETE", "/data-exports/"+i, nil)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// What is refused before a task exists, and by whom.
func TestExportRefusals(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.t_ok (id int primary key, v text)")
	q("create or replace view public.v_ok as select * from public.t_ok")
	post := func(table string, body any) (int, map[string]any) {
		st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/"+table+"/export", body, nil)
		return st, out
	}
	if st, out := post("v_ok", map[string]any{}); st != 422 || problemCode(out) != "table_not_eligible" {
		t.Errorf("a view: %d %v", st, out)
	}
	if st, out := post("nope", map[string]any{}); st != 404 {
		t.Errorf("a missing table: %d %v", st, out)
	}
	if st, out, _ := e.call("POST", "/databases/db_xfer/schemas/kingres/tables/node/export", map[string]any{}, nil); st != 422 {
		t.Errorf("a table of the extension: %d %v", st, out)
	}
	if st, out, _ := e.call("POST", "/databases/db_xfer/schemas/pg_catalog/tables/pg_class/export", map[string]any{}, nil); st != 422 {
		t.Errorf("a table of the system: %d %v", st, out)
	}
	for name, body := range map[string]any{
		"unknown column":   map[string]any{"columns": []string{"nope"}},
		"duplicate column": map[string]any{"columns": []string{"id", "id"}},
		"bad delimiter":    map[string]any{"delimiter": "#"},
		"bad encoding":     map[string]any{"encoding": "EBCDIC"},
		"null with quote":  map[string]any{"nullString": "a\"b"},
		"long null":        map[string]any{"nullString": strings.Repeat("n", 33)},
		"bad compression":  map[string]any{"compression": "zip"},
		"bad retention":    map[string]any{"retentionHours": 500},
		"bom with latin1":  map[string]any{"utf8Bom": true, "encoding": "LATIN1"},
		"unknown member":   map[string]any{"where": "true"},
		"quote delimiter":  map[string]any{"delimiter": ";", "quote": ";"},
	} {
		if st, out := post("t_ok", body); st != 400 {
			t.Errorf("%s: %d %v", name, st, out)
		}
	}
	// A role below administrator: 403. A role that may not read the table: 422 with the column named, and nothing is staged.
	ro := e.accountWithRole(t, "xfer_ro2", "kingres_ops")
	if st, out, _ := e.as(ro).call("POST", "/databases/db_xfer/schemas/public/tables/t_ok/export", map[string]any{}, nil); st != 403 {
		t.Errorf("ops: %d %v", st, out)
	}
	adm := e.accountWithRole(t, "xfer_noread", "kingres_admin")
	st, out, _ := e.as(adm).call("POST", "/databases/db_xfer/schemas/public/tables/t_ok/export", map[string]any{}, nil)
	if st != 422 || problemCode(out) != "table_not_eligible" || !strings.Contains(toJSON(out["blockers"]), "no_privilege") {
		t.Errorf("a table the role cannot read: %d %v", st, out)
	}
	// Row-level security: refused for a role that neither owns the table nor bypasses it; the owner may export, and the file holds every row.
	q("create table if not exists public.t_rls (id int, v text)")
	q("insert into public.t_rls values (1, 'a'), (2, 'b')")
	q("alter table public.t_rls enable row level security")
	q("create policy only_one on public.t_rls using (id = 1)")
	q("grant select on public.t_rls to xfer_noread")
	st, out, _ = e.as(adm).call("POST", "/databases/db_xfer/schemas/public/tables/t_rls/export", map[string]any{}, nil)
	if st != 422 || !strings.Contains(toJSON(out["blockers"]), "row_level_security") {
		t.Errorf("row-level security for a role that does not own the table: %d %v", st, out)
	}
	_, rid := e.startExport(t, "db_xfer", "t_rls", map[string]any{})
	if got := e.expect(t, 200, "GET", "/data-exports/"+rid, nil); got["rows"] != float64(2) {
		t.Errorf("an export of a table with row-level security by its owner holds %v rows, want 2", got["rows"])
	}
	e.expect(t, 204, "DELETE", "/data-exports/"+rid, nil)
	e.expect(t, 204, "DELETE", "/roles/xfer_ro2", nil)
	// The quota: the estimate of an export is at least 64 MiB; a quota of 1 MiB refuses at the request, 409, and keeps nothing.
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"stagingQuotaBytes": 1 << 20}})
	st, out = post("t_ok", map[string]any{})
	if st != 409 || problemCode(out) != "transfer_quota_exceeded" {
		t.Errorf("a quota that does not fit: %d %v", st, out)
	}
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"stagingQuotaBytes": 1 << 30}})
	if l := e.expect(t, 200, "GET", "/data-exports", nil); len(l["items"].([]any)) != 0 {
		for _, it := range l["items"].([]any) {
			if m := it.(map[string]any); m["kind"] == "tableExport" && (m["state"] == "ready" || m["state"] == "building") {
				t.Errorf("a refused export left a file: %v", it)
			}
		}
	}
	q("revoke select on public.t_rls from xfer_noread")
	e.expect(t, 204, "DELETE", "/roles/xfer_noread", nil)
}
