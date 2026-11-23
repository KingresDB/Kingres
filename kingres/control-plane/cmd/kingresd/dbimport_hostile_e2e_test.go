// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// The hostile archives of the database import. Every one is made from a good archive by changing a part of it, and every one must be refused, with nothing left
// behind: no database, no role, no setting, no private database.

// variant is the content of an archive that a test may change before it is written again.
type variant struct {
	Cat   archive.Catalog
	Man   archive.Manifest
	Files map[string]string // every entry but the manifest, by name
	Names []string          // the entries, in order, without the manifest
	// Patch changes the manifest as it is about to be written (after the sizes and sums were recomputed), so that it can lie.
	Patch func(m *archive.Manifest)
}

func readVariant(t testing.TB, b []byte) *variant {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	v := &variant{Files: map[string]string{}}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		switch f.Name {
		case archive.ManifestName:
			if err := json.Unmarshal(data, &v.Man); err != nil {
				t.Fatal(err)
			}
		case archive.CatalogName:
			if err := json.Unmarshal(data, &v.Cat); err != nil {
				t.Fatal(err)
			}
			v.Names = append(v.Names, f.Name)
		default:
			v.Files[f.Name] = string(data)
			v.Names = append(v.Names, f.Name)
		}
	}
	return v
}

// bytes writes the archive again: the catalog as JSON, the sizes and sums of the manifest recomputed.
func (v *variant) bytes(t testing.TB) []byte {
	t.Helper()
	var parts []archive.Part
	m := v.Man
	m.Entries = nil
	var oldEntries = map[string]archive.ManifestEntry{}
	for _, e := range v.Man.Entries {
		oldEntries[e.Name] = e
	}
	m.Totals = archive.Totals{}
	for _, n := range v.Names {
		var data []byte
		if n == archive.CatalogName {
			var err error
			if data, err = json.Marshal(v.Cat); err != nil {
				t.Fatal(err)
			}
		} else {
			data = []byte(v.Files[n])
		}
		parts = append(parts, archive.Part{Name: n, Data: data})
		sum := sha256.Sum256(data)
		e := oldEntries[n]
		e.Name, e.Size, e.SHA256 = n, int64(len(data)), hex.EncodeToString(sum[:])
		if _, ok := archive.IsData(n); ok && !e.LedgerHistory {
			m.Totals.Tables++
			m.Totals.Rows += e.Rows
		}
		m.Totals.UncompressedBytes += e.Size
		m.Entries = append(m.Entries, e)
	}
	if v.Patch != nil {
		v.Patch(&m)
	}
	mj, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := archive.Assemble(&buf, parts, mj); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// addTable lists a table in the catalog and in the options as a table without data (so that no data entry is needed).
func (v *variant) addTable(schema, name string, cols ...string) {
	v.Cat.Tables = append(v.Cat.Tables, archive.CatalogTable{Schema: schema, Name: name, Kind: "local", TreeRole: "standalone", Columns: cols, AccessMethod: "heap"})
	v.Man.Options.ExcludedTables = append(v.Man.Options.ExcludedTables, schema+"."+name)
}

// snapshot is what a refused import must leave exactly as it was: the databases, the roles and the per-database settings.
func snapshot(t testing.TB, e *cp) string {
	t.Helper()
	sc := e.serviceConn(t, "postgres")
	defer func() { _ = sc.Close(context.Background()) }()
	return rowsText(t, sc, `select 'db', datname::text, pg_get_userbyid(datdba)::text, datacl::text from pg_database
		union all select 'role', rolname::text, rolsuper::text || rolcreaterole::text || rolcanlogin::text, '' from pg_roles
		union all select 'setting', setdatabase::text, setrole::text, setconfig::text from pg_db_role_setting
		union all select 'member', roleid::text, member::text, '' from pg_auth_members order by 1, 2, 3`)
}

var smallOnce sync.Once

// smallArchive builds a small database (a victim table and one hash table) and exports it.
func smallArchive(t testing.TB, e *cp, db string) []byte {
	t.Helper()
	e.mustTask(t, "POST", "/databases", map[string]any{"name": db, "sharding": map[string]any{"enabled": true, "bucketCount": 8}, "replication": map[string]any{"durability": "async"}})
	q := func(sql string) { e.expect(t, 200, "POST", "/query", map[string]any{"database": db, "sql": sql}) }
	q("create table public.victim (id int primary key, v text)")
	q("insert into public.victim values (1, 'a'), (2, 'b')")
	q("create table public.dh (id bigint primary key, v text)")
	e.mustTask(t, "PUT", "/databases/"+db+"/schemas/public/tables/dh/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	q("insert into public.dh select g, 'v' || g from generate_series(1, 40) g")
	id := e.exportDB(t, db, map[string]any{})
	_, _, body := e.download(t, id, nil)
	return body
}

func TestDatabaseImportRefusesHostileArchives(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	base := smallArchive(t, e, "dbhostile")
	// The good archive imports, so every refusal below is the work of what was changed.
	good := e.uploadArchive(t, base)
	e.importOK(t, map[string]any{"uploadId": good, "database": "dbhostile_ok"})
	if got := query(t, e, "dbhostile_ok", "select count(*) from public.victim"); got != "[[2]]" {
		t.Fatalf("the good archive: %s", got)
	}
	// A statement of 1 MiB + 1 byte that does not compress (a bomb is refused by the ratio before it is read as SQL).
	var pad strings.Builder
	for h := sha256.Sum256([]byte("pad")); pad.Len() <= 1<<20; h = sha256.Sum256(h[:]) {
		pad.WriteString(hex.EncodeToString(h[:]))
	}
	big := "CREATE TABLE public.big (a int) /* " + pad.String() + " */;\n"
	cases := []struct {
		name string
		mod  func(v *variant)
		code string // the code of the failed task
	}{
		{"set role", func(v *variant) { v.Files[archive.PreDataName] = "SET ROLE postgres;\n" + v.Files[archive.PreDataName] }, "archive_statement_refused"},
		{"set session authorization", func(v *variant) {
			v.Files[archive.PreDataName] = "SET SESSION AUTHORIZATION postgres;\n" + v.Files[archive.PreDataName]
		}, "archive_statement_refused"},
		{"set in a later file", func(v *variant) { v.Files[archive.PostDataName] += "SET search_path = public;\n" }, "archive_statement_refused"},
		{"alter database", func(v *variant) {
			v.Files[archive.PostDataName] += "ALTER DATABASE postgres SET search_path = public, pg_temp;\n"
		}, "archive_statement_refused"},
		{"alter database reset", func(v *variant) { v.Files[archive.PostDataName] += "ALTER DATABASE postgres RESET ALL;\n" }, "archive_statement_refused"},
		{"grant to public on a system object", func(v *variant) {
			v.Files[archive.PostDataName] += "GRANT ALL ON ALL TABLES IN SCHEMA pg_catalog TO PUBLIC;\n"
		}, "archive_statement_refused"},
		{"grant a role", func(v *variant) { v.Files[archive.PostDataName] += "GRANT pg_execute_server_program TO root_admin;\n" }, "archive_statement_refused"},
		{"alter system", func(v *variant) { v.Files[archive.PostDataName] += "ALTER SYSTEM SET archive_command = 'x';\n" }, "archive_statement_refused"},
		{"create role", func(v *variant) { v.Files[archive.PreDataName] += "CREATE ROLE evil SUPERUSER LOGIN;\n" }, "archive_statement_refused"},
		{"comment on role", func(v *variant) { v.Files[archive.PostDataName] += "COMMENT ON ROLE root_admin IS 'x';\n" }, "archive_statement_refused"},
		{"create extension", func(v *variant) { v.Files[archive.PreDataName] += "CREATE EXTENSION dblink;\n" }, "archive_statement_refused"},
		{"copy program", func(v *variant) { v.Files[archive.PostDataName] += "COPY public.victim TO PROGRAM 'id';\n" }, "archive_statement_refused"},
		{"do block", func(v *variant) { v.Files[archive.PostDataName] += "DO $$ BEGIN PERFORM pg_sleep(1); END $$;\n" }, "archive_statement_refused"},
		{"create function", func(v *variant) {
			v.Files[archive.PreDataName] += "CREATE FUNCTION public.f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;\n"
		}, "archive_statement_refused"},
		{"create trigger", func(v *variant) {
			v.Files[archive.PostDataName] += "CREATE TRIGGER t BEFORE INSERT ON public.victim FOR EACH ROW EXECUTE FUNCTION pg_catalog.suppress_redundant_updates_trigger();\n"
		}, "archive_statement_refused"},
		{"a second action in one alter table", func(v *variant) {
			v.Files[archive.PostDataName] += "ALTER TABLE public.victim ENABLE ROW LEVEL SECURITY, OWNER TO postgres;\n"
		}, "archive_statement_refused"},
		{"create table as", func(v *variant) { v.Files[archive.PreDataName] += "CREATE TABLE public.ctas AS SELECT * FROM pg_authid;\n" }, "archive_statement_refused"},
		{"schema element", func(v *variant) {
			v.Files[archive.PreDataName] += "CREATE SCHEMA s2 CREATE TABLE t (a int) CREATE TRIGGER x BEFORE INSERT ON t EXECUTE FUNCTION f();\n"
		}, "archive_statement_refused"},
		{"schema kingres", func(v *variant) { v.Files[archive.PreDataName] += "CREATE SCHEMA kingres;\n" }, "archive_statement_refused"},
		{"table in pg_catalog", func(v *variant) { v.Files[archive.PreDataName] += "CREATE TABLE pg_catalog.x (a int);\n" }, "archive_statement_refused"},
		{"drop", func(v *variant) { v.Files[archive.PostDataName] += "DROP TABLE public.victim;\n" }, "archive_statement_refused"},
		{"a statement after a carriage return comment", func(v *variant) {
			v.Files[archive.PostDataName] += "COMMENT ON TABLE public.victim IS 'x' -- c\r; DROP TABLE public.victim;\n"
		}, "archive_statement_refused"},
		{"a backslash command", func(v *variant) { v.Files[archive.PostDataName] += "\\! id\n" }, "archive_statement_refused"},
		{"an oversized statement", func(v *variant) { v.Files[archive.PreDataName] += big }, "archive_statement_refused"},
		{"pg_sleep in a default", func(v *variant) {
			v.Files[archive.PreDataName] += "CREATE TABLE public.f1 (a int DEFAULT pg_sleep(100)::text::int);\n"
			v.addTable("public", "f1", "a")
		}, "archive_statement_refused"},
		{"pg_read_file in a generated column", func(v *variant) {
			v.Files[archive.PreDataName] += "CREATE TABLE public.f2 (a text GENERATED ALWAYS AS (pg_read_file('/etc/passwd')) STORED);\n"
			v.addTable("public", "f2", "a")
		}, "archive_statement_refused"},
		{"pg_sleep in a view", func(v *variant) { v.Files[archive.ViewsName] += "CREATE VIEW public.v1 AS SELECT pg_sleep(100);\n" }, "archive_statement_refused"},
		{"a function that does not exist", func(v *variant) {
			v.Files[archive.ViewsName] += "CREATE VIEW public.v2 AS SELECT public.mine(1);\n"
		}, "archive_statement_refused"},
		{"a management function of the extension", func(v *variant) {
			v.Files[archive.ViewsName] += "CREATE VIEW public.v3 AS SELECT kingres.ledger_enable('public.victim'::regclass, 'updatable');\n"
		}, "archive_statement_refused"},
		{"set_config in a check", func(v *variant) {
			v.Files[archive.PreDataName] += "CREATE TABLE public.f3 (a int CHECK (set_config('search_path', 'public', false) <> ''));\n"
			v.addTable("public", "f3", "a")
		}, "archive_statement_refused"},
		{"a table the catalog does not list", func(v *variant) { v.Files[archive.PreDataName] += "CREATE TABLE public.sneaky (a int);\n" }, "archive_invalid"},
		{"a table of the catalog that the files do not create", func(v *variant) { v.addTable("public", "ghost", "a") }, "archive_invalid"},
		{"an extension outside the closed list", func(v *variant) {
			v.Man.Extensions = append(v.Man.Extensions, archive.ManifestExtension{Name: "dblink", Version: "1.2", Schema: "public"})
		}, "archive_invalid"},
		{"file_fdw in the manifest", func(v *variant) {
			v.Man.Extensions = append(v.Man.Extensions, archive.ManifestExtension{Name: "file_fdw", Version: "1.0", Schema: "public"})
		}, "archive_invalid"},
		{"a zip bomb in a data entry", func(v *variant) {
			n := archive.DataName(1)
			v.Files[n] = strings.SplitN(v.Files[n], "\n", 2)[0] + "\n\"1\",\"" + strings.Repeat("a", 12<<20) + "\"\n"
		}, "archive_invalid"},
		{"a manifest that lies about the rows", func(v *variant) {
			v.Patch = func(m *archive.Manifest) {
				for i := range m.Entries {
					if m.Entries[i].Table == "public.victim" {
						m.Entries[i].Rows = 99
						m.Totals.Rows += 97
					}
				}
			}
		}, "row_count_mismatch"},
		{"a manifest that lies about the columns", func(v *variant) {
			v.Patch = func(m *archive.Manifest) {
				for i := range m.Entries {
					if m.Entries[i].Table == "public.victim" {
						m.Entries[i].Columns = []string{"v", "id"}
					}
				}
			}
		}, "archive_invalid"},
		{"a manifest that lies about a checksum", func(v *variant) {
			v.Patch = func(m *archive.Manifest) { m.Entries[len(m.Entries)-1].SHA256 = strings.Repeat("0", 64) }
		}, "archive_invalid"},
		{"a manifest that lies about the totals", func(v *variant) {
			v.Patch = func(m *archive.Manifest) { m.Totals.Tables += 3 }
		}, "archive_invalid"},
		{"a manifest from a later major", func(v *variant) { v.Man.Source.PostgresMajor += 5 }, "archive_incompatible"},
		{"a format that is not known", func(v *variant) { v.Patch = func(m *archive.Manifest) { m.Format = "kingres-export/2" } }, "archive_incompatible"},
	}
	e.waitNoPrivate(t)
	before := snapshot(t, e)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			v := readVariant(t, base)
			tc.mod(v)
			up := e.uploadArchive(t, v.bytes(t))
			st, out := e.importDB(t, map[string]any{"uploadId": up, "database": "dbhostile_x"})
			if st != 202 {
				// A refusal at the request is as good: nothing was started.
				if st != 422 {
					t.Fatalf("importDatabase: %d %v", st, out)
				}
			} else {
				done := e.waitTask(t, out)
				if done["state"] != "failed" || taskCode(done) != tc.code {
					t.Fatalf("task: state %v code %q (%s), want failed with %s", done["state"], taskCode(done), taskMessage(done), tc.code)
				}
				if msg := taskMessage(done); strings.Contains(msg, "evil") || strings.Contains(msg, "pg_authid") || strings.Contains(msg, "/etc/passwd") {
					t.Errorf("the message of the task holds text of the archive: %q", msg)
				}
			}
			if names := e.waitNoPrivate(t); len(names) != 0 {
				t.Errorf("a private database is left: %v", names)
			}
			if e.databaseExists(t, "dbhostile_x") {
				t.Errorf("the final database exists")
			}
			if after := snapshot(t, e); after != before {
				t.Errorf("a refused import changed the cluster:\n before %s\n after  %s", before, after)
			}
		})
	}
}

// A statement that is hidden in a comment, in a dollar quote or in a string is data of the statement that holds it: it is not run, and the import succeeds.
func TestDatabaseImportHiddenTextIsNotRun(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	base := smallArchive(t, e, "dbhidden")
	v := readVariant(t, base)
	v.Files[archive.PreDataName] += "CREATE TABLE public.h_comment (a int) /* ; DROP TABLE public.victim ; GRANT ALL ON ALL TABLES IN SCHEMA pg_catalog TO PUBLIC */;\n" +
		"CREATE TABLE public.h_dollar (a text DEFAULT $x$; DROP TABLE public.victim; ALTER DATABASE postgres SET search_path = x; $x$);\n" +
		"CREATE TABLE public.h_string (a text DEFAULT 'it''s; DROP TABLE public.victim;');\n" +
		"CREATE TABLE public.h_line (a int) -- ; DROP TABLE public.victim\n;\n" +
		"CREATE TABLE public.h_e (a text DEFAULT E'\\'; DROP TABLE public.victim; --');\n"
	for _, n := range []string{"h_comment", "h_dollar", "h_string", "h_line", "h_e"} {
		v.addTable("public", n, "a")
	}
	up := e.uploadArchive(t, v.bytes(t))
	before := snapshot(t, e)
	e.importOK(t, map[string]any{"uploadId": up, "database": "dbhidden_ok"})
	if got := query(t, e, "dbhidden_ok", "select count(*) from public.victim"); got != "[[2]]" {
		t.Errorf("victim after the import: %s", got)
	}
	if got := query(t, e, "dbhidden_ok", "select count(*) from pg_tables where schemaname = 'public' and tablename like 'h\\_%'"); got != "[[5]]" {
		t.Errorf("tables made: %s", got)
	}
	// The new database is the only change: no setting of a database carries a search path.
	if got := query(t, e, "postgres", "select count(*) from pg_db_role_setting where setconfig::text like '%search_path%'"); got != "[[0]]" {
		t.Errorf("settings of databases with a search path: %s", got)
	}
	if after := snapshot(t, e); after == before {
		t.Errorf("the database was not made")
	}
}

// A role with an attribute that makes the statements of an archive dangerous is refused at the request.
func TestDatabaseImportRefusesADangerousRole(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	base := smallArchive(t, e, "dbdanger")
	pw := "Danger-Passw0rd-1"
	e.expect(t, 201, "POST", "/roles", map[string]any{"name": "dbx_danger", "password": pw, "clusterRoles": []string{"kingres_admin"}})
	sc := e.serviceConn(t, "postgres")
	defer func() { _ = sc.Close(context.Background()) }()
	if _, err := sc.Exec(context.Background(), "alter role dbx_danger superuser"); err != nil {
		t.Fatal(err)
	}
	tok, ok := e.loginAs("dbx_danger", pw)
	if !ok {
		t.Fatal("login of the role")
	}
	d := e.as(tok)
	up := d.uploadArchive(t, base)
	st, out, _ := d.call("POST", "/database-imports", map[string]any{"uploadId": up, "database": "dbdanger_x"}, nil)
	if st != 403 || problemCode(out) != "forbidden" {
		t.Fatalf("a superuser role: %d %v", st, out)
	}
	if _, err := sc.Exec(context.Background(), "alter role dbx_danger nosuperuser; grant pg_read_server_files to dbx_danger"); err != nil {
		t.Fatal(err)
	}
	if st, out, _ := d.call("POST", "/database-imports", map[string]any{"uploadId": up, "database": "dbdanger_x"}, nil); st != 403 || problemCode(out) != "forbidden" {
		t.Fatalf("a role that reads server files: %d %v", st, out)
	}
	if e.databaseExists(t, "dbdanger_x") || len(e.waitNoPrivate(t)) != 0 {
		t.Error("something was made")
	}
}

// The role cannot connect to the private database while the service role works in it, and can while it loads: the access follows the steps.
func TestDatabaseImportWithdrawsTheAccessOfTheRole(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	base := smallArchive(t, e, "dbaccess")
	up := e.uploadArchive(t, base)
	results := map[string]string{}
	var mu sync.Mutex
	try := func(step string, id string) {
		cfg, _ := pgx.ParseConfig("")
		cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = "127.0.0.1", uint16(e.pgPort), id, adminUser, adminPass //nolint:gosec // test port
		cfg.TLSConfig, cfg.Fallbacks = nil, nil
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c, err := pgx.ConnectConfig(ctx, cfg)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			results[step] = "refused"
			return
		}
		_ = c.Close(ctx)
		results[step] = "connected"
	}
	var taskID string
	ops.ImportHook = func(ctx context.Context, step string) error {
		mu.Lock()
		id := taskID
		mu.Unlock()
		if id == "" || (step != "Create the tables" && step != "Distribute the tables" && step != "Load the data" && step != "Enable the ledger and the policies" && step != "Verify") {
			return nil
		}
		try(step, "kingres_import_"+id)
		return nil
	}
	defer func() { ops.ImportHook = nil }()
	st, out := e.importDB(t, map[string]any{"uploadId": up, "database": "dbaccess_ok"})
	if st != 202 {
		t.Fatalf("%d %v", st, out)
	}
	mu.Lock()
	taskID = fmt.Sprint(int64(out["id"].(float64)))
	mu.Unlock()
	done := e.waitTask(t, out)
	if done["state"] != "done" {
		t.Fatalf("import: %v", done["error"])
	}
	// Before the tables are made the role cannot connect yet (the step gives the access when it starts: the hook runs just before); the service role's steps are closed.
	want := map[string]string{"Distribute the tables": "connected", "Load the data": "connected", "Enable the ledger and the policies": "refused", "Verify": "refused"}
	for step, w := range want {
		if results[step] != w {
			t.Errorf("at %q the role %s, want %s (%v)", step, results[step], w, results)
		}
	}
}

// A failed import removes the private database; a cancelled one too; keepOnFailure keeps it (and it can be dropped).
func TestDatabaseImportCleansUp(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	base := smallArchive(t, e, "dbclean")
	// A failure in the middle of the work: the data step fails (the manifest lies about the rows).
	v := readVariant(t, base)
	v.Patch = func(m *archive.Manifest) {
		for i := range m.Entries {
			if m.Entries[i].Table == "public.dh" {
				m.Totals.Rows += 7 - m.Entries[i].Rows
				m.Entries[i].Rows = 7
			}
		}
	}
	bad := e.uploadArchive(t, v.bytes(t))
	e.waitNoPrivate(t)
	before := snapshot(t, e)
	e.importFails(t, map[string]any{"uploadId": bad, "database": "dbclean_x"}, "row_count_mismatch")
	if names := e.waitNoPrivate(t); len(names) != 0 || e.databaseExists(t, "dbclean_x") {
		t.Errorf("after a failure: %v", names)
	}
	if after := snapshot(t, e); after != before {
		t.Errorf("a failed import changed the cluster:\n%s\n%s", before, after)
	}
	// keepOnFailure: the private database stays, under its private name, and nothing of the final name exists.
	e.importFails(t, map[string]any{"uploadId": bad, "database": "dbclean_x", "keepOnFailure": true}, "row_count_mismatch")
	time.Sleep(2 * time.Second)
	names := e.privateDatabases(t)
	if len(names) != 1 || e.databaseExists(t, "dbclean_x") {
		t.Fatalf("keepOnFailure: %v", names)
	}
	sc := e.serviceConn(t, "postgres")
	if _, err := sc.Exec(context.Background(), `drop database "`+names[0]+`" with (force)`); err != nil {
		t.Errorf("dropping the kept database: %v", err)
	}
	_ = sc.Close(context.Background())
	// A cancel while the data is loaded.
	good := e.uploadArchive(t, base)
	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	ops.ImportHook = func(ctx context.Context, step string) error {
		if step == "Load the data" {
			once.Do(func() { close(reached) })
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	defer func() { ops.ImportHook = nil }()
	st, out := e.importDB(t, map[string]any{"uploadId": good, "database": "dbclean_c"})
	if st != 202 {
		t.Fatalf("%d %v", st, out)
	}
	select {
	case <-reached:
	case <-time.After(3 * time.Minute):
		t.Fatal("the import did not reach the data")
	}
	if names := e.privateDatabases(t); len(names) != 1 {
		t.Errorf("while the data is loaded: %v", names)
	}
	e.expect(t, 200, "POST", "/tasks/"+idOf(out)+"/cancel", nil)
	close(release)
	done := e.waitTask(t, out)
	if done["state"] != "failed" || taskCode(done) != "cancelled" {
		t.Errorf("a cancelled import: %v %q", done["state"], taskCode(done))
	}
	if names := e.waitNoPrivate(t); len(names) != 0 || e.databaseExists(t, "dbclean_c") {
		t.Errorf("after a cancel: %v", names)
	}
}

// waitNoPrivate waits until no database with a private name is left: the compensation of a failed task runs after the task is recorded as failed.
func (e *cp) waitNoPrivate(t testing.TB) []string {
	t.Helper()
	var names []string
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if names = e.privateDatabases(t); len(names) == 0 {
			return nil
		}
	}
	return names
}
