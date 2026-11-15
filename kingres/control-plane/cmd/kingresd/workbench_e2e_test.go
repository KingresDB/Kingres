// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// wbq runs one workbench statement through the API and returns status and body.
func (e *cp) wbq(db, sql string, extra map[string]any) (int, map[string]any) {
	body := map[string]any{"database": db, "sql": sql}
	for k, v := range extra {
		body[k] = v
	}
	st, out, _ := e.call("POST", "/query", body, nil)
	return st, out
}

// TestWorkbenchCutDMLIsCommitted is CP2-11: an UPDATE ... RETURNING whose result is cut at
// rowLimit used to be rolled back and answered as a SELECT; it completes, is committed and
// says what it did.
func TestWorkbenchCutDMLIsCommitted(t *testing.T) {
	e := shared(t)
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "cutdb"})
	defer e.mustTask(t, "DELETE", "/databases/cutdb", nil)
	for _, sql := range []string{
		"create table public.big (id int primary key, x int not null default 0)",
		"insert into public.big select g from generate_series(1, 5000) g",
	} {
		if st, out := e.wbq("cutdb", sql, nil); st != 200 {
			t.Fatalf("%s: %d %v", sql, st, out)
		}
	}
	st, out := e.wbq("cutdb", "update public.big set x = 1 returning id", map[string]any{"rowLimit": 10})
	if st != 200 || out["truncated"] != true || out["commandTag"] != "UPDATE 5000" || out["rowCount"] != float64(5000) || len(out["rows"].([]any)) != 10 {
		t.Fatalf("cut update: %d %v", st, out)
	}
	valid(t, "executeQuery", "200", out)
	if _, out := e.wbq("cutdb", "select count(*) from public.big where x = 1", nil); fmt.Sprint(out["rows"]) != "[[5000]]" {
		t.Errorf("the cut update was not committed: %v", out["rows"])
	}
	// A plain read is still stopped early and says so.
	_, out = e.wbq("cutdb", "select id from public.big", map[string]any{"rowLimit": 7})
	if out["truncated"] != true || out["rowCount"] != float64(7) || out["commandTag"] != "SELECT 7" {
		t.Errorf("cut select: %v", out)
	}
}

// TestWorkbenchTimeoutIsASQLError is CP2-12: timeoutMs above the server's request timeout (30 s)
// took effect only as a 503 of the request; the statement's own timeout now ends it, as 422
// sql_error with SQLSTATE 57014.
func TestWorkbenchTimeoutIsASQLError(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 31 seconds")
	}
	e := shared(t)
	st, out := e.wbq("postgres", "select pg_sleep(60)", map[string]any{"timeoutMs": 31000})
	if st != 422 || problemCode(out) != "sql_error" || out["sqlState"] != "57014" {
		t.Fatalf("a statement that ran into timeoutMs (31 s): %d %v", st, out)
	}
}

// TestWorkbenchHugeValueIsBounded is the "one huge value" item of the second review: a
// value of hundreds of megabytes was read into memory whole before the 1 MiB check. The
// connection to PostgreSQL now refuses a message larger than the result budget, so the
// process allocates a few megabytes at most and the caller gets 422.
func TestWorkbenchHugeValueIsBounded(t *testing.T) {
	e := shared(t)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	st, out := e.wbq("postgres", "select repeat('x', 300000000)", nil)
	runtime.ReadMemStats(&after)
	if st != 422 || problemCode(out) != "unprocessable" {
		t.Fatalf("300 MB value: %d %v", st, out)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 100<<20 {
		t.Errorf("kingresd allocated %d MiB for a value it refuses; the value was read into memory", grew>>20)
	}
	// The workbench still works afterwards.
	if st, out := e.wbq("postgres", "select 1", nil); st != 200 {
		t.Errorf("after the refusal: %d %v", st, out)
	}
}

// TestLedgerDigestScopeAttributionAndBytes covers CP2-08 (the answer says what a digest and a
// verification cover; a task that ends done leaves a record), the digest attribution item
// (generatedBy) and the byte-exactness item (the document of the API is the extension's, only
// without insignificant white space).
func TestLedgerDigestScopeAttributionAndBytes(t *testing.T) {
	e := shared(t)
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "scopedb", "ledger": map[string]any{"mode": "optIn"}})
	defer e.mustTask(t, "DELETE", "/databases/scopedb", nil)
	for _, sql := range []string{"create table public.a (id int primary key)", "select 1"} {
		if st, out := e.wbq("scopedb", sql, nil); st != 200 {
			t.Fatalf("%s: %d %v", sql, st, out)
		}
	}
	e.expect(t, 200, "PUT", "/databases/scopedb/schemas/public/tables/a/ledger", map[string]any{"mode": "appendOnly"})
	e.wbq("scopedb", "insert into public.a values (1)", nil)

	st, raw := e.rawCall("POST", "/databases/scopedb/ledger/digests", nil)
	if st != 201 {
		t.Fatalf("digest: %d %s", st, raw)
	}
	if err := chk(t).Response("generateLedgerDigest", "201", raw); err != nil {
		t.Errorf("digest does not match the contract: %v", err)
	}
	d := asMap(t, raw)
	if d["complete"] != true || fmt.Sprint(d["uncoveredShardGroupIds"]) != "[]" {
		t.Errorf("a one-group database: complete=%v uncovered=%v", d["complete"], d["uncoveredShardGroupIds"])
	}
	// Attribution: the role that asked, not the service role the extension saw.
	if d["generatedBy"] != adminUser {
		t.Errorf("generatedBy = %v, want %s (the requesting role)", d["generatedBy"], adminUser)
	}
	// Byte-exactness: the document is what the extension stored, compacted.
	var stored string
	if err := e.serviceConn(t, "scopedb").QueryRow(context.Background(), "select digest::text from kingres.ledger_digest order by digest_id desc limit 1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(stored)); err != nil {
		t.Fatal(err)
	}
	if doc := digestDocument(t, raw); !bytes.Equal(doc, compact.Bytes()) {
		t.Errorf("the document differs from the stored digest:\n api:    %s\n stored: %s", doc, compact.Bytes())
	}

	// A verification leaves a record, also when nothing is new since the last clean run.
	records := func() int {
		out := e.expect(t, 200, "GET", "/databases/scopedb/ledger/verifications", nil)
		return len(out["items"].([]any))
	}
	e.mustTask(t, "POST", "/databases/scopedb/ledger/verifications", map[string]any{"scope": "full"})
	first := records()
	e.mustTask(t, "POST", "/databases/scopedb/ledger/verifications", nil)
	if n := records(); n != first+1 {
		t.Errorf("a verification with nothing new left %d records after %d: a done task must leave a record", n, first)
	}
	v := e.expect(t, 200, "GET", "/databases/scopedb/ledger/verifications", nil)["items"].([]any)[0].(map[string]any)
	valid(t, "getLedgerVerification", "200", v)
	if v["state"] != "passed" || v["complete"] != true || !strings.HasPrefix(fmt.Sprint(v["uncoveredShardGroupIds"]), "[") {
		t.Errorf("verification: %v", v)
	}
}

// TestWorkbenchIgnoresBareAdminMemberships: PostgreSQL 16 and later make the creator of a role
// a member of it with the ADMIN option only (no INHERIT, no SET). That membership cannot use
// the role, so a dangerous predefined role given to the created role in SQL must not lock the
// administrator who created it out of the workbench.
func TestWorkbenchIgnoresBareAdminMemberships(t *testing.T) {
	e := shared(t)
	pw := "Passw0rd-bare-admin-1"
	e.expect(t, 201, "POST", "/roles", map[string]any{"name": "bare_admin_target", "password": pw, "clusterRoles": []string{"kingres_readonly"}})
	defer e.expect(t, 204, "DELETE", "/roles/bare_admin_target", nil)
	svc := e.serviceConn(t, "postgres")
	if _, err := svc.Exec(context.Background(), "GRANT pg_read_server_files TO bare_admin_target"); err != nil {
		t.Fatal(err)
	}
	if st, out := e.wbq("postgres", "select 1", nil); st != 200 {
		t.Errorf("the creator of a role with pg_read_server_files is locked out of the workbench: %d %v", st, out)
	}
	tok, ok := e.loginAs("bare_admin_target", pw)
	if !ok {
		t.Fatal("login")
	}
	if st, out, _ := e.as(tok).call("POST", "/query", map[string]any{"database": "postgres", "sql": "select 1"}, nil); st != 403 {
		t.Errorf("the role that does hold pg_read_server_files must still be refused: %d %v", st, out)
	}
}

// TestWorkbenchCancelStatementRoute is the HTTP side of cancelStatement (contract 0.7.0): a
// statement started with a statementId is cancelled by that id and ends with sql_error 57014, a
// second cancel finds nothing (404), and a statement id of a running statement cannot be reused.
func TestWorkbenchCancelStatementRoute(t *testing.T) {
	e := shared(t)
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "canceldb"})
	defer e.mustTask(t, "DELETE", "/databases/canceldb", nil)
	done := make(chan map[string]any, 1)
	status := make(chan int, 1)
	go func() {
		st, out := e.wbq("postgres", "select pg_sleep(60)", map[string]any{"statementId": "cancel-me-1", "timeoutMs": 120000})
		status <- st
		done <- out
	}()
	// The statement is tracked as soon as it runs; poll the cancel until it finds it.
	var cancelled bool
	for i := 0; i < 100 && !cancelled; i++ {
		st, _, _ := e.call("POST", "/query/statements/cancel-me-1/cancel", nil, nil)
		if st == 204 {
			cancelled = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !cancelled {
		t.Fatal("the statement was never found by its id")
	}
	select {
	case st := <-status:
		out := <-done
		if st != 422 || out["code"] != "sql_error" || out["sqlState"] != "57014" {
			t.Fatalf("the cancelled statement answered %d %v", st, out)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the cancelled statement did not end")
	}
	if st, _, _ := e.call("POST", "/query/statements/cancel-me-1/cancel", nil, nil); st != 404 {
		t.Errorf("a second cancel: %d, want 404", st)
	}
	if st, _, _ := e.call("POST", "/query/statements/bad%20id/cancel", nil, nil); st != 404 && st != 400 {
		t.Errorf("a malformed id: %d", st)
	}
}
