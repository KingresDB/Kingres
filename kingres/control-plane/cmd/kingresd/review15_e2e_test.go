// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// Tests of the fixes of the review of the fixes of 14a (docs/reviews/2026-11-22-data-export-import-14a-fixes.md).

// DX15-01: the owner of a database plants the schema kingres_transfer with a table and a trigger before the first import. The control plane must not run it as the
// service role: the import fails closed, and nothing the trigger would do happens.
func TestMarkerSchemaPlantedByTheDatabaseOwner(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_plant")
	q("create table if not exists public.pl_t (id int)")
	q("truncate public.pl_t")
	q("create schema kingres_transfer")
	q("create table kingres_transfer.import_marker (nonce text primary key, task_id bigint, state text, rows bigint, at timestamptz default now())")
	q("create function kingres_transfer.boom() returns trigger language plpgsql as $$ begin create role kg_pwned superuser; return new; end $$")
	q("create trigger boom before insert on kingres_transfer.import_marker for each row execute function kingres_transfer.boom()")
	uid := e.uploadFile(t, []byte("1\n"))
	task := e.startImport(t, "db_plant", "pl_t", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true})
	if task["state"] != "failed" || taskCode(task) != "forbidden" || !strings.Contains(taskMessage(task), "kingres_transfer") {
		t.Errorf("an import with a planted schema: %v", task["error"])
	}
	var n int
	if err := e.serviceConn(t, "postgres").QueryRow(context.Background(), "select count(*) from pg_roles where rolname = 'kg_pwned'").Scan(&n); err != nil || n != 0 {
		t.Errorf("the planted trigger ran as the service role: %d %v", n, err)
	}
	if got := count(t, q, "pl_t"); got != "[[0]]" {
		t.Errorf("rows: %s", got)
	}
	// With the schema removed the import works: the control plane makes its own.
	q("drop schema kingres_transfer cascade")
	if task := e.startImport(t, "db_plant", "pl_t", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" {
		t.Errorf("the import after the schema was removed: %v", task["error"])
	}
	e.serviceExec(t, "drop role if exists kg_pwned")
}

// DX15-02: a role cannot write a marker without the proof of the control plane, cannot read the secret or the table.
func TestMarkerCannotBeForgedByARole(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.fg_t (id int)")
	uid := e.uploadFile(t, []byte("1\n"))
	if task := e.startImport(t, "db_xfer", "fg_t", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" {
		t.Fatalf("the first import: %v", task["error"])
	}
	tok := e.accountWithRole(t, "xfer_forge", "kingres_admin")
	as := e.as(tok)
	const nonce = "0123456789abcdef0123456789abcdef"
	st, out, _ := as.call("POST", "/query", map[string]any{"database": "db_xfer", "sql": "select kingres_transfer.mark('" + nonce + "', 1, 1, '\\x00'::bytea)"}, nil)
	if st == 200 {
		t.Errorf("a marker was written with a wrong proof: %v", out)
	}
	for _, sql := range []string{"select * from kingres_transfer.secret", "select * from kingres_transfer.import_marker", "insert into kingres_transfer.import_marker values ('" + nonce + "', 1, 'committed', 1)"} {
		if st, out, _ := as.call("POST", "/query", map[string]any{"database": "db_xfer", "sql": sql}, nil); st == 200 {
			t.Errorf("%s was allowed: %v", sql, out)
		}
	}
	var n int
	if err := e.serviceConn(t, "db_xfer").QueryRow(context.Background(), "select count(*) from kingres_transfer.import_marker where nonce = $1", nonce).Scan(&n); err != nil || n != 0 {
		t.Errorf("a forged marker exists: %d %v", n, err)
	}
	e.expect(t, 204, "DELETE", "/roles/xfer_forge", nil)
}

// DX15-03: markers are keyed by a nonce, not by the task id, so rows of another cluster's tasks (a restored or cloned database) do not collide; old markers are pruned.
func TestMarkerKeysAreNoncesAndOldOnesArePruned(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.nc_t (id int)")
	q("truncate public.nc_t")
	uid := e.uploadFile(t, []byte("1\n"))
	if task := e.startImport(t, "db_xfer", "nc_t", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true}); task["state"] != "done" {
		t.Fatalf("the first import: %v", task["error"])
	}
	sc := e.serviceConn(t, "db_xfer")
	if _, err := sc.Exec(context.Background(), "insert into kingres_transfer.import_marker (nonce, task_id, state, rows) select md5(g::text), g, 'committed', 1 from generate_series(1, 5000) g"); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Exec(context.Background(), "insert into kingres_transfer.import_marker (nonce, task_id, state, rows, at) values (md5('old'), 1, 'committed', 1, now() - interval '60 days')"); err != nil {
		t.Fatal(err)
	}
	if task := e.startImport(t, "db_xfer", "nc_t", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" || count(t, q, "nc_t") != "[[2]]" {
		t.Errorf("an import beside markers of foreign task ids: %v", task["error"])
	}
	var old, recent int
	if err := sc.QueryRow(context.Background(), "select count(*) filter (where nonce = md5('old')), count(*) filter (where nonce = md5('7')) from kingres_transfer.import_marker").Scan(&old, &recent); err != nil || old != 0 || recent != 1 {
		t.Errorf("pruning: old %d recent %d %v", old, recent, err)
	}
}

// DX15-04: the role check of the load step is made again, and a role that gained BYPASSRLS after the request ends the task with nothing stored.
func TestImportRoleChangedBeforeTheLoad(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.rb_t (id int)")
	q("truncate public.rb_t")
	tok := e.accountWithRole(t, "xfer_late", "kingres_admin")
	q("grant insert on public.rb_t to xfer_late")
	as := e.as(tok)
	uid := as.uploadFile(t, []byte("1\n"))
	ops.ImportFault = func(p string, _ uint32) error {
		if p == "beforeCheck" {
			ops.ImportFault = nil
			e.serviceExec(t, "alter role xfer_late bypassrls")
		}
		return nil
	}
	defer func() { ops.ImportFault = nil }()
	task := as.startImport(t, "db_xfer", "rb_t", map[string]any{"uploadId": uid, "header": "none"})
	if task["state"] != "failed" || taskCode(task) != "forbidden" || !strings.Contains(taskMessage(task), "BYPASSRLS") {
		t.Errorf("a role that gained BYPASSRLS before the load: %v", task["error"])
	}
	if got := count(t, q, "rb_t"); got != "[[0]]" {
		t.Errorf("rows: %s", got)
	}
	e.serviceExec(t, "alter role xfer_late nobypassrls")
	q("revoke all on public.rb_t from xfer_late")
	e.expect(t, 204, "DELETE", "/roles/xfer_late", nil)
}

// DX15-07 (from reading): two roles import into a fresh database at the same moment; the creation of the marker objects is serialised.
func TestFirstImportsIntoAFreshDatabaseDoNotCollide(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_fresh")
	q("create table if not exists public.fr_a (id int)")
	q("create table if not exists public.fr_b (id int)")
	names := []string{"xfer_fa", "xfer_fb"}
	var toks [2]string
	var uids [2]string
	for i, n := range names {
		toks[i] = e.accountWithRole(t, n, "kingres_admin")
		q("grant insert on public.fr_" + string(rune('a'+i)) + " to " + n)
		uids[i] = e.as(toks[i]).uploadFile(t, []byte("1\n"))
	}
	var wg sync.WaitGroup
	results := make([]string, 2)
	for i := range toks {
		as := e.as(toks[i])
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, out, _ := as.call("POST", "/databases/db_fresh/schemas/public/tables/fr_"+string(rune('a'+i))+"/import", map[string]any{"uploadId": uids[i], "header": "none"}, nil)
			if st != 202 {
				results[i] = fmt.Sprint(st, out)
				return
			}
			id := fmt.Sprint(int64(out["id"].(float64)))
			for end := time.Now().Add(3 * time.Minute); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
				_, cur, _ := as.call("GET", "/tasks/"+id, nil, nil)
				if s := cur["state"]; s == "done" || s == "failed" {
					results[i] = fmt.Sprint(s, cur["error"])
					return
				}
			}
			results[i] = "timeout"
		}()
	}
	wg.Wait()
	for i, r := range results {
		if !strings.HasPrefix(r, "done") {
			t.Errorf("import %d: %s", i, r)
		}
	}
	for i, n := range names {
		q("revoke all on public.fr_" + string(rune('a'+i)) + " from " + n)
		e.expect(t, 204, "DELETE", "/roles/"+n, nil)
	}
}
