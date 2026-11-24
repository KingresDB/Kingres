// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// rowsText runs a query on a connection and returns the rows as text.
func rowsText(t testing.TB, c *pgx.Conn, sql string) string {
	t.Helper()
	rows, err := c.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, ";")
}

// End-to-end tests of the database import (contract 0.41.0) against a real kingresd and a real PostgreSQL instance: the round trip of a database with every kind of
// table, and the archives that the import must refuse.

// importDB posts importDatabase and returns the status and the body.
func (e *cp) importDB(t testing.TB, req map[string]any) (int, map[string]any) {
	t.Helper()
	st, out, _ := e.call("POST", "/database-imports", req, nil)
	return st, out
}

// importOK imports and waits; the task must be done.
func (e *cp) importOK(t testing.TB, req map[string]any) map[string]any {
	t.Helper()
	st, out := e.importDB(t, req)
	if st != 202 {
		t.Fatalf("importDatabase: %d %v", st, out)
	}
	validTask(t, out)
	done := e.waitTask(t, out)
	if done["state"] != "done" {
		t.Fatalf("the import task failed: %v\nlogs:\n%s", done["error"], tailOf(e.logs.String(), 6000))
	}
	return done
}

// importFails imports and waits; the task must fail with the code.
func (e *cp) importFails(t testing.TB, req map[string]any, code string) map[string]any {
	t.Helper()
	st, out := e.importDB(t, req)
	if st != 202 {
		t.Fatalf("importDatabase: %d %v (wanted a task that fails with %s)", st, out, code)
	}
	done := e.waitTask(t, out)
	if done["state"] != "failed" || taskCode(done) != code {
		t.Fatalf("the import task: state %v code %q (%s), want failed with %s", done["state"], taskCode(done), taskMessage(done), code)
	}
	return done
}

func tailOf(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// dbNames lists the databases of the instance that the control plane did not make for the tests of this binary: used to show that a refusal left nothing behind.
func (e *cp) privateDatabases(t testing.TB) []string {
	t.Helper()
	out := e.expect(t, 200, "POST", "/query", map[string]any{"database": "postgres", "sql": "select datname from pg_database where datname like 'kingres_import_%' order by 1"})
	var names []string
	for _, r := range out["rows"].([]any) {
		names = append(names, r.([]any)[0].(string))
	}
	return names
}

func (e *cp) databaseExists(t testing.TB, name string) bool {
	t.Helper()
	out := e.expect(t, 200, "POST", "/query", map[string]any{"database": "postgres", "sql": "select count(*) from pg_database where datname = '" + name + "'"})
	return fmt.Sprint(out["rows"]) != "[[0]]"
}

var roundTripTables = []string{"public.t_plain", "public.t_child", "public.inh_parent", "public.inh_child", "public.lp", "app.items", "public.refdata", "public.d_hash", "public.d_part",
	"public.c_events", "public.acc", "public.audit_rows", "public.ev"}

// digests of every table of the fixture: the number of rows and the MD5 of the text of the rows in a fixed order.
func digests(t testing.TB, e *cp, db string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tb := range roundTripTables {
		var sql string
		if tb == "public.inh_parent" {
			sql = "select count(*), coalesce(md5(string_agg(x::text, '|' order by x::text)), '') from only " + tb + " x"
		} else {
			sql = "select count(*), coalesce(md5(string_agg(x::text, '|' order by x::text)), '') from " + tb + " x"
		}
		r := e.expect(t, 200, "POST", "/query", map[string]any{"database": db, "sql": sql})
		out[tb] = fmt.Sprint(r["rows"])
	}
	return out
}

// query runs a statement as the service role (the tables belong to the importer, the source to another role) and returns the rows as the workbench prints them.
func query(t testing.TB, e *cp, db, sql string) string {
	t.Helper()
	c := e.serviceConn(t, db)
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

// The importer: a role of the cluster role kingres_admin as the API makes it (it has CREATEROLE, and the role that made it is a member of it), used as it is; it owns what it imports.
const importerName, importerPass = "dbi_admin", "Importer-Passw0rd-1"

func importerOf(t testing.TB, e *cp) *cp {
	t.Helper()
	st, out, _ := e.call("POST", "/roles", map[string]any{"name": importerName, "password": importerPass, "clusterRoles": []string{"kingres_admin"}}, nil)
	if st != 201 && st != 409 {
		t.Fatalf("the importer role: %d %v", st, out)
	}
	tok, ok := e.loginAs(importerName, importerPass)
	if !ok {
		t.Fatal("login of the importer")
	}
	return e.as(tok)
}

// The round trip: a database with every kind of table is exported, imported under a new name and compared table by table.
func TestDatabaseImportRoundTrip(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	imp := importerOf(t, e)
	buildExportFixture(t, e, "dbsrc")
	// A ledger table whose signature policy is required: the import enters its rows first and sets the policy again after the verification.
	ssrc := e.serviceConn(t, "dbsrc")
	if _, err := ssrc.Exec(context.Background(), "select kingres.ledger_set_signature_policy('public.audit_rows'::regclass, 'required')"); err != nil {
		t.Fatal(err)
	}
	_ = ssrc.Close(context.Background())
	id := e.exportDB(t, "dbsrc", map[string]any{})
	st, _, body := e.download(t, id, nil)
	if st != 200 {
		t.Fatalf("download: %d", st)
	}
	// For the test of the other major: the archive of this one is kept with the digests of its source.
	writeArchiveKept(t, e, body, digests(t, e, "dbsrc"))
	up := imp.uploadArchive(t, body)
	// An archive with ledger tables needs the acknowledgement.
	if st, out := imp.importDB(t, map[string]any{"uploadId": up, "database": "dbdst"}); st != 202 {
		t.Fatalf("request: %d %v", st, out)
	} else {
		done := e.waitTask(t, out)
		if done["state"] != "failed" || taskCode(done) != "ledger_restart_not_acknowledged" {
			t.Fatalf("without the acknowledgement: %v %q %s", done["state"], taskCode(done), taskMessage(done))
		}
	}
	if names := e.privateDatabases(t); len(names) != 0 {
		t.Fatalf("a refused import left databases: %v", names)
	}
	done := imp.importOK(t, map[string]any{"uploadId": up, "database": "dbdst", "acknowledgeLedgerRestart": true})
	detail, _ := done["detail"].(string)
	if !strings.Contains(detail, "policy disabled: public.ev") || !strings.Contains(detail, "ledger: public.acc starts a new chain") {
		t.Errorf("the detail of the import: %q", detail)
	}
	if names := e.privateDatabases(t); len(names) != 0 {
		t.Errorf("databases with a private name after the import: %v", names)
	}
	if !e.databaseExists(t, "dbdst") {
		t.Fatal("the final database does not exist")
	}
	// Every table, row for row.
	src, dst := digests(t, e, "dbsrc"), digests(t, imp, "dbdst")
	for _, tb := range roundTripTables {
		if src[tb] != dst[tb] {
			t.Errorf("table %s: source %s, imported %s", tb, src[tb], dst[tb])
		}
	}
	// The distribution: the same tables, the same method and column, the same bucket count and the same colocation sets.
	dist := "select d.relid::text, d.method, coalesce(d.dist_column::text, ''), coalesce(c.bucket_count, 0) from kingres.dist_table d left join kingres.colocation c using (colocation_id) order by 1"
	if a, b := query(t, e, "dbsrc", dist), query(t, imp, "dbdst", dist); a != b {
		t.Errorf("distribution: source %s, imported %s", a, b)
	}
	sets := "select string_agg(relid::text, ',' order by relid::text) from kingres.dist_table where colocation_id is not null group by colocation_id order by 1"
	if a, b := query(t, e, "dbsrc", sets), query(t, imp, "dbdst", sets); a != b {
		t.Errorf("colocation sets: source %s, imported %s", a, b)
	}
	// The ledger: enabled for the same tables, a new and consistent chain, the entries of the loaded rows.
	led := "select relid::text, mode, signature_policy from kingres.ledger_table order by 1"
	ssc := e.serviceConn(t, "dbsrc")
	defer func() { _ = ssc.Close(context.Background()) }()
	dsc := e.serviceConn(t, "dbdst")
	defer func() { _ = dsc.Close(context.Background()) }()
	if a, b := rowsText(t, ssc, led), rowsText(t, dsc, led); a != b || !strings.Contains(a, "required") {
		t.Errorf("ledger tables: source %s, imported %s (the policy of audit_rows must be required in both)", a, b)
	}
	sc := e.serviceConn(t, "dbdst")
	defer func() { _ = sc.Close(context.Background()) }()
	var findings, entries int
	if err := sc.QueryRow(context.Background(), "select count(*) from kingres.ledger_verify()").Scan(&findings); err != nil || findings != 0 {
		t.Errorf("ledger_verify on the imported database: %d findings (%v)", findings, err)
	}
	// The rows that were loaded are the first entries of the new chain: two rows of acc and one of audit_rows.
	if err := sc.QueryRow(context.Background(), "select count(*) from kingres.ledger_entry").Scan(&entries); err != nil || entries != 3 {
		t.Errorf("entries of the new chain: %d (%v), want 3", entries, err)
	}
	// The time-partition policy is there and disabled.
	var tp, enabled = "", true
	if err := sc.QueryRow(context.Background(), "select table_name::text, enabled from kingres.timepart_policy").Scan(&tp, &enabled); err != nil || tp != "ev" || enabled {
		t.Errorf("time-partition policies after the import: %q enabled=%v (%v)", tp, enabled, err)
	}
	// A materialized view is made WITH NO DATA and refreshed last, so its content is the one of a refresh now; the source is refreshed to compare with it.
	query(t, e, "dbsrc", "refresh materialized view public.mv_plain")
	// Constraints are valid, comments, views, the materialized view, the enum, the domain and the sequence are there.
	if got := query(t, imp, "dbdst", "select count(*) from pg_constraint where not convalidated"); got != "[[0]]" {
		t.Errorf("invalid constraints: %s", got)
	}
	for _, c := range []string{
		"select obj_description('public.t_plain'::regclass), col_description('public.t_plain'::regclass, 2)",
		"select * from public.v_plain order by id", "select * from public.mv_plain",
		"select enum_range(null::app.mood)::text", "select typname from pg_type where typname = 'posint'",
	} {
		if a, b := query(t, e, "dbsrc", c), query(t, imp, "dbdst", c); a != b {
			t.Errorf("%s: source %s, imported %s", c, a, b)
		}
	}
	// A sequence continues above the largest value of its column.
	if got := query(t, imp, "dbdst", "insert into app.items (label) values ('z') returning id"); got != "[[3]]" {
		t.Errorf("the identity sequence of app.items continues with %s", got)
	}
	if got := query(t, imp, "dbdst", "insert into public.t_plain (name) values ('n') returning id"); got != "[[5]]" {
		t.Errorf("the serial sequence of t_plain continues with %s", got)
	}
	// The importing role owns what it made, and the database is owned by it.
	if got := query(t, imp, "dbdst", "select distinct pg_get_userbyid(relowner) from pg_class where relname in ('t_plain', 'd_hash', 'refdata')"); got != "[[dbi_admin]]" {
		t.Errorf("owners: %s", got)
	}
	if got := query(t, e, "postgres", "select pg_get_userbyid(datdba) from pg_database where datname = 'dbdst'"); got != "[[dbi_admin]]" {
		t.Errorf("owner of the database: %s", got)
	}
	// The database is in the list of the databases.
	if st, _, _ := e.call("GET", "/databases/dbdst", nil, nil); st != 200 {
		t.Errorf("getDatabase: %d", st)
	}
	// A second import under the same name is refused at the request.
	if st, out := imp.importDB(t, map[string]any{"uploadId": up, "database": "dbdst", "acknowledgeLedgerRestart": true}); st != 409 || problemCode(out) != "already_exists" {
		t.Errorf("a second import under the same name: %d %v", st, out)
	}
}

// writeArchiveKept keeps the archive and the digests of the source for the cross-major test (KINGRES_ARCHIVE_DIR): the other major imports them.
func writeArchiveKept(t testing.TB, e *cp, body []byte, src map[string]string) {
	dir := os.Getenv("KINGRES_ARCHIVE_DIR")
	if dir == "" {
		return
	}
	major := strings.Trim(query(t, e, "postgres", "select current_setting('server_version_num')::int / 10000"), "[]")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Logf("the directory of the archives: %v", err)
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "archive-"+major+".zip"), body, 0o600)
	b, _ := json.Marshal(src)
	_ = os.WriteFile(filepath.Join(dir, "digests-"+major+".json"), b, 0o600)
}

// An archive made on another major: one of an older major is imported and equals what its source was; one of a later major is refused as incompatible and nothing is
// made. The archives are made by TestDatabaseImportRoundTrip of the other major (KINGRES_ARCHIVE_DIR names the directory both runs share).
func TestDatabaseImportOfAnArchiveFromAnotherMajor(t *testing.T) {
	dir := os.Getenv("KINGRES_ARCHIVE_DIR")
	if dir == "" {
		t.Skip("KINGRES_ARCHIVE_DIR is not set; the archives of the other major are not there")
	}
	e, off := transferOn(t)
	defer off()
	imp := importerOf(t, e)
	here := 0
	fmt.Sscan(strings.Trim(query(t, e, "postgres", "select current_setting('server_version_num')::int / 10000"), "[]"), &here)
	found := 0
	for _, major := range []int{18, 19} {
		body, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("archive-%d.zip", major)))
		if err != nil {
			continue
		}
		found++
		var want map[string]string
		if b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("digests-%d.json", major))); err == nil {
			_ = json.Unmarshal(b, &want)
		}
		up := imp.uploadArchive(t, body)
		name := fmt.Sprintf("dbfrom%d", major)
		req := map[string]any{"uploadId": up, "database": name, "acknowledgeLedgerRestart": true}
		if major > here {
			imp.importFails(t, req, "archive_incompatible")
			if e.databaseExists(t, name) || len(e.waitNoPrivate(t)) != 0 {
				t.Errorf("an archive of major %d on %d left something", major, here)
			}
			continue
		}
		imp.importOK(t, req)
		got := digests(t, imp, name)
		for _, tb := range roundTripTables {
			if want[tb] == "" || got[tb] != want[tb] {
				t.Errorf("the archive of major %d imported on %d: table %s is %s, its source was %s", major, here, tb, got[tb], want[tb])
			}
		}
		if q := query(t, imp, name, "select count(*) from kingres.dist_table"); q == "[[0]]" {
			t.Errorf("no distributed table after the import of the archive of major %d", major)
		}
	}
	if found == 0 {
		t.Fatalf("no archive in %s: run TestDatabaseImportRoundTrip of a major first", dir)
	}
}
