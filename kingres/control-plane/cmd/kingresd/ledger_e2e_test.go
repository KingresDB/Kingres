// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// serviceConn opens a connection as the service role (what a DBA with the
// superuser password would do), to set up states the API cannot create.
func (e *cp) serviceConn(t testing.TB, db string) *pgx.Conn {
	t.Helper()
	pw, err := os.ReadFile(filepath.Join(e.dataDir, "service-credentials"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = "127.0.0.1", uint16(e.pgPort), db, "kingresd", strings.TrimSpace(string(pw)) //nolint:gosec // test port
	cfg.TLSConfig, cfg.Fallbacks = nil, nil
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestLedgerDigestsVerificationsAndUpdatableTables(t *testing.T) {
	e := shared(t)
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "ldb", "ledger": map[string]any{"mode": "optIn"}})
	q := func(sql string) map[string]any {
		return e.expect(t, 200, "POST", "/query", map[string]any{"database": "ldb", "sql": sql})
	}
	q("create table public.acc (id bigint primary key, bal int)")
	q("create table public.hist_src (id bigint primary key, v text)")
	// An empty ledger has no digest to build.
	if st, o, _ := e.call("POST", "/databases/ldb/ledger/digests", nil, nil); st != 409 {
		t.Errorf("digest of an empty ledger: %d %v", st, o)
	}
	out := e.expect(t, 200, "PUT", "/databases/ldb/schemas/public/tables/acc/ledger", map[string]any{"mode": "updatable"})
	valid(t, "enableTableLedger", "200", out)
	if l := out["ledger"].(map[string]any); l["mode"] != "updatable" || l["historyTable"] != "acc_ledger_history" {
		t.Errorf("updatable table: %v", l)
	}
	e.expect(t, 200, "PUT", "/databases/ldb/schemas/public/tables/hist_src/ledger", map[string]any{"mode": "appendOnly"})
	q("insert into public.acc values (1, 10), (2, 20)")
	q("update public.acc set bal = 11 where id = 1")
	q("insert into public.hist_src values (1, 'x')")

	// The history table is an attribute of its table, not a table of the user.
	list := e.expect(t, 200, "GET", "/databases/ldb/tables", nil)
	valid(t, "listTables", "200", list)
	for _, it := range list["items"].([]any) {
		if strings.HasSuffix(it.(map[string]any)["name"].(string), "_ledger_history") {
			t.Errorf("history table listed as a table: %v", it)
		}
	}
	// Digests.
	d := e.expect(t, 201, "POST", "/databases/ldb/ledger/digests", nil)
	valid(t, "generateLedgerDigest", "201", d)
	if d["generatedBy"] == nil || len(d["chains"].([]any)) != 1 {
		t.Errorf("digest %v", d)
	}
	dl := e.expect(t, 200, "GET", "/databases/ldb/ledger/digests", nil)
	valid(t, "listLedgerDigests", "200", dl)
	// The scheduler (ledger.digest, a database that never had a digest gets one at its first tick) may record a digest of its own, generatedBy "system", between the
	// inserts above and the POST: it is not an error. The list must hold the digest of the POST once, and any other item must be the scheduler's.
	own := 0
	for _, it := range dl["items"].([]any) {
		m := it.(map[string]any)
		switch {
		case m["id"] == d["id"]:
			own++
		case m["generatedBy"] != "system":
			t.Errorf("a digest that nobody here made: %v", m)
		}
	}
	if own != 1 {
		t.Errorf("the digest of the POST is in the list %d times: %v", own, dl)
	}
	// A verification runs as a task and is recorded.
	task := e.mustTask(t, "POST", "/databases/ldb/ledger/verifications", map[string]any{"scope": "full"})
	if task["kind"] != "ledger.verify" {
		t.Errorf("task %v", task["kind"])
	}
	st := e.expect(t, 200, "GET", "/databases/ldb/ledger", nil)
	valid(t, "getLedgerStatus", "200", st)
	if st["verificationState"] != "passed" || st["ledgerTableCount"] != float64(2) || st["lastVerifiedAt"] == nil {
		t.Errorf("ledger status %v", st)
	}
	// Tampering is reported: change a ledger row behind the ledger's back.
	sc := e.serviceConn(t, "ldb")
	if _, err := sc.Exec(context.Background(), "alter table public.hist_src disable trigger all"); err == nil {
		_, _ = sc.Exec(context.Background(), "update public.hist_src set v = 'tampered'")
	}
	// Whatever the ledger's protections allow, a later verification must not
	// report clean when the row changed.
	var changed string
	_ = sc.QueryRow(context.Background(), "select v from public.hist_src").Scan(&changed)
	if changed == "tampered" {
		q("insert into public.hist_src values (2, 'y')") // new entry so that a block is pending
		e.expect(t, 201, "POST", "/databases/ldb/ledger/digests", nil)
		failed := e.call3(t, "POST", "/databases/ldb/ledger/verifications", map[string]any{"scope": "full"})
		if failed["state"] != "failed" {
			t.Errorf("tampered ledger verified clean: %v", failed)
		}
	}
	// Access: digests and verifications need kingres_ops.
	ro := e.accountWithRole(t, "ledger_ro", "kingres_readonly")
	for _, c := range []struct{ m, p string }{{"POST", "/databases/ldb/ledger/digests"}, {"POST", "/databases/ldb/ledger/verifications"}} {
		if s, _, _ := e.as(ro).call(c.m, c.p, nil, nil); s != 403 {
			t.Errorf("%s %s as readonly: %d", c.m, c.p, s)
		}
	}
	e.expect(t, 204, "DELETE", "/roles/ledger_ro", nil)

	// Run-time detection: a database still on kingres_ledger 0.1.
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "old01"})
	oc := e.serviceConn(t, "old01")
	if _, err := oc.Exec(context.Background(), "create extension kingres_ledger version '0.1'"); err != nil {
		t.Fatal(err)
	}
	if s, o, _ := e.call("POST", "/databases/old01/ledger/digests", nil, nil); s != 422 || problemCode(o) != "unsupported_operation" {
		t.Errorf("digest on 0.1: %d %v", s, o)
	}
	if s, o, _ := e.call("POST", "/databases/old01/ledger/verifications", nil, nil); s != 422 {
		t.Errorf("verification on 0.1: %d %v", s, o)
	}
	dl = e.expect(t, 200, "GET", "/databases/old01/ledger/digests", nil)
	if len(dl["items"].([]any)) != 0 {
		t.Errorf("digests on 0.1: %v", dl)
	}
	ls := e.expect(t, 200, "GET", "/databases/old01/ledger", nil)
	valid(t, "getLedgerStatus", "200", ls)
	if ls["verificationState"] != nil {
		t.Errorf("0.1 status invents a verification state: %v", ls)
	}
	// A database on 0.2 has the records, but the installed library writes
	// the ledger only through the functions of its own version: both calls
	// say what to run. (After the update, below, they are accepted.)
	if _, err := oc.Exec(context.Background(), "alter extension kingres_ledger update to '0.2'"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/databases/old01/ledger/digests", "/databases/old01/ledger/verifications"} {
		s, o, _ := e.call("POST", p, nil, nil)
		if s != 422 || problemCode(o) != "unsupported_operation" || !strings.Contains(fmt.Sprint(o["detail"]), "ALTER EXTENSION kingres_ledger UPDATE") {
			t.Errorf("POST %s on 0.2: %d %v", p, s, o)
		}
	}
	if s, o, _ := e.call("PUT", "/databases/old01/schemas/public/tables/x/ledger", map[string]any{"mode": "updatable"}, nil); s != 404 && s != 422 {
		t.Errorf("updatable before the update: %d %v", s, o)
	}
	// Upgraded in place, the same database gets the features of the current version.
	if _, err := oc.Exec(context.Background(), "alter extension kingres_ledger update"); err != nil {
		t.Fatal(err)
	}
	oq := func(sql string) {
		e.expect(t, 200, "POST", "/query", map[string]any{"database": "old01", "sql": sql})
	}
	oq("create table public.t (id int primary key)")
	e.expect(t, 200, "PUT", "/databases/old01/schemas/public/tables/t/ledger", map[string]any{"mode": "updatable"})
	oq("insert into public.t values (1)")
	e.expect(t, 201, "POST", "/databases/old01/ledger/digests", nil)
	e.mustTask(t, "DELETE", "/databases/old01", nil)
	e.mustTask(t, "DELETE", "/databases/ldb", nil)
}

// call3 starts a verification and returns the last recorded verification state
// read from the ledger itself.
func (e *cp) call3(t testing.TB, method, path string, body any) map[string]any {
	t.Helper()
	st, out, _ := e.call(method, path, body, nil)
	if st != 202 {
		t.Fatalf("%s %s: %d %v", method, path, st, out)
	}
	done := e.waitTask(t, out)
	return map[string]any{"state": map[string]string{"done": "clean", "failed": "failed"}[fmt.Sprint(done["state"])]}
}
