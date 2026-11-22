// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// Tests of the fixes of the security review of the marker (docs/reviews/2026-11-22-data-transfer-marker-security.md).

// firstImport makes the marker objects of a database with one import of one row into table (created here) and returns the upload id kept for later imports.
func firstImport(t *testing.T, e *cp, q func(string) map[string]any, db, table string) string {
	t.Helper()
	q("create table if not exists public." + table + " (id int)")
	uid := e.uploadFile(t, []byte("1\n"))
	if task := e.startImport(t, db, table, map[string]any{"uploadId": uid, "header": "none", "keepUpload": true}); task["state"] != "done" {
		t.Fatalf("the first import into %s: %v", table, task["error"])
	}
	return uid
}

func (e *cp) svcRow(t *testing.T, db, sql string, args ...any) string {
	t.Helper()
	rows, err := e.serviceConn(t, db).Query(context.Background(), sql, args...)
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
		out = append(out, fmt.Sprint(vals...))
	}
	return strings.Join(out, ";")
}

// DX16-02: a member of pg_read_all_data and pg_write_all_data bypasses the privileges of the marker tables, but not their row security: it reads no secret and changes no marker.
func TestMarkerRowSecurityStopsAllDataRoles(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	firstImport(t, e, q, "db_xfer", "rs_t")
	tok := e.accountWithRole(t, "xfer_alldata", "kingres_admin")
	e.serviceExec(t, "grant pg_read_all_data, pg_write_all_data to xfer_alldata")
	as := e.as(tok)
	run := func(sql string) map[string]any {
		st, out, _ := as.call("POST", "/query", map[string]any{"database": "db_xfer", "sql": sql}, nil)
		if st != 200 {
			t.Fatalf("%s: %d %v", sql, st, out)
		}
		return out
	}
	if got := fmt.Sprint(run("select count(*) from kingres_transfer.secret")["rows"]); got != "[[0]]" {
		t.Errorf("a member of pg_read_all_data reads the secret: %s", got)
	}
	if got := fmt.Sprint(run("select count(*) from kingres_transfer.import_marker")["rows"]); got != "[[0]]" {
		t.Errorf("a member of pg_read_all_data reads the markers: %s", got)
	}
	const stateSQL = "select (select count(*) from kingres_transfer.secret), (select count(*) from kingres_transfer.import_marker where state = 'aborted'), (select count(*) from kingres_transfer.import_marker where state = 'committed')"
	before := e.svcRow(t, "db_xfer", stateSQL)
	run("update kingres_transfer.import_marker set state = 'aborted'")
	run("delete from kingres_transfer.secret")
	if got := e.svcRow(t, "db_xfer", stateSQL); got != before || !strings.HasPrefix(got, "1 ") {
		t.Errorf("secrets and markers after the writes of a member of pg_write_all_data: %s, before %s", got, before)
	}
	e.serviceExec(t, "revoke pg_read_all_data, pg_write_all_data from xfer_alldata")
	e.expect(t, 204, "DELETE", "/roles/xfer_alldata", nil)
}

// DX16-03: a NULL tag with a missing secret row is invalid; a secret that is not 32 bytes ends the import closed; a missing secret is made again by the next import.
func TestMarkerNullTagAndBadSecret(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	uid := firstImport(t, e, q, "db_xfer", "ns_t")
	tok := e.accountWithRole(t, "xfer_nulltag", "kingres_admin")
	as := e.as(tok)
	const nonce = "abcdefabcdefabcdefabcdefabcdefab"
	e.svcExecDB(t, "db_xfer", "delete from kingres_transfer.secret")
	st, out, _ := as.call("POST", "/query", map[string]any{"database": "db_xfer", "sql": "select kingres_transfer.mark('" + nonce + "', 9, 99, NULL)"}, nil)
	if st == 200 {
		t.Errorf("a NULL tag was accepted without a secret: %v", out)
	}
	if got := e.svcRow(t, "db_xfer", "select count(*) from kingres_transfer.import_marker where nonce = $1", nonce); got != "0" {
		t.Errorf("a marker of a NULL tag exists: %s", got)
	}
	// The next import makes the secret again and works.
	if task := e.startImport(t, "db_xfer", "ns_t", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true}); task["state"] != "done" {
		t.Errorf("the import after the secret was lost: %v", task["error"])
	}
	// A secret of another length is not accepted.
	e.svcExecDB(t, "db_xfer", "update kingres_transfer.secret set k = '\\x00'")
	task := e.startImport(t, "db_xfer", "ns_t", map[string]any{"uploadId": uid, "header": "none"})
	if task["state"] != "failed" || taskCode(task) != "forbidden" {
		t.Errorf("an import with a secret of one byte: %v", task["error"])
	}
	e.svcExecDB(t, "db_xfer", "update kingres_transfer.secret set k = decode(md5('a') || md5('b'), 'hex')")
	e.expect(t, 204, "DELETE", "/roles/xfer_nulltag", nil)
}

// DX16-04: a session that holds the advisory lock that the first version took does not stop a first import into a fresh database.
func TestMarkerCreationTakesNoLockThatARoleCanHold(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_lock")
	q("create table if not exists public.lk_t (id int)")
	holder := e.serviceConn(t, "db_lock")
	if _, err := holder.Exec(context.Background(), "select pg_advisory_lock(7355608)"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = holder.Exec(context.Background(), "select pg_advisory_unlock(7355608)") }()
	uid := e.uploadFile(t, []byte("1\n"))
	st, out, _ := e.call("POST", "/databases/db_lock/schemas/public/tables/lk_t/import", map[string]any{"uploadId": uid, "header": "none"}, nil)
	if st != 202 {
		t.Fatalf("%d %v", st, out)
	}
	id := fmt.Sprint(int64(out["id"].(float64)))
	var state any
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		_, cur, _ := e.call("GET", "/tasks/"+id, nil, nil)
		if state = cur["state"]; state == "done" || state == "failed" {
			break
		}
	}
	if state != "done" {
		t.Errorf("an import beside a held advisory lock: %v", state)
	}
}

// DX16-05: constraints, indexes and grants that the control plane did not make (a restored or cloned database can carry them) end the import closed.
func TestMarkerVerifyRejectsWhatItDidNotMake(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	uid := firstImport(t, e, q, "db_xfer", "vr_t")
	for _, c := range []struct{ name, plant, undo string }{
		{"a check constraint", "alter table kingres_transfer.import_marker add constraint evil check (nonce <> 'zz')", "alter table kingres_transfer.import_marker drop constraint evil"},
		{"an index", "create index evil_i on kingres_transfer.import_marker (task_id)", "drop index kingres_transfer.evil_i"},
		{"a create grant on the schema", "grant create on schema kingres_transfer to public", "revoke create on schema kingres_transfer from public"},
		{"a column grant on the secret", "grant select (k) on kingres_transfer.secret to public", "revoke select (k) on kingres_transfer.secret from public"},
		{"a storage option", "alter table kingres_transfer.import_marker set (fillfactor = 50)", "alter table kingres_transfer.import_marker reset (fillfactor)"},
		{"a column of another type", "alter table kingres_transfer.import_marker alter column rows type numeric", "alter table kingres_transfer.import_marker alter column rows type bigint"},
		{"a select grant on the secret", "grant select on kingres_transfer.secret to public", "revoke select on kingres_transfer.secret from public"},
	} {
		e.svcExecDB(t, "db_xfer", c.plant)
		task := e.startImport(t, "db_xfer", "vr_t", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true})
		if task["state"] != "failed" || taskCode(task) != "forbidden" {
			t.Errorf("%s: %v", c.name, task["error"])
		}
		e.svcExecDB(t, "db_xfer", c.undo)
	}
	if task := e.startImport(t, "db_xfer", "vr_t", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" {
		t.Errorf("the import after the objects were as made: %v", task["error"])
	}
}

// DX16-06: a marker of a task that is still unfinished is not pruned, however old; the old markers of the others are.
func TestMarkerPruningKeepsUnfinishedTasks(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	uid := firstImport(t, e, q, "db_xfer", "pk_t")
	ops.ImportFault = func(p string, _ uint32) error {
		if p == "beforeCheck" {
			ops.ImportFault = nil
			id := e.activeImportID(t)
			e.svcExecDB(t, "db_xfer", "insert into kingres_transfer.import_marker (nonce, task_id, state, rows, at) values (md5('keepme'), "+id+", 'committed', 1, now() - interval '60 days'), (md5('dropme'), 999999, 'committed', 1, now() - interval '60 days')")
		}
		return nil
	}
	defer func() { ops.ImportFault = nil }()
	if task := e.startImport(t, "db_xfer", "pk_t", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" {
		t.Fatalf("the import: %v", task["error"])
	}
	if got := e.svcRow(t, "db_xfer", "select count(*) filter (where nonce = md5('keepme')), count(*) filter (where nonce = md5('dropme')) from kingres_transfer.import_marker"); got != "1 0" {
		t.Errorf("kept and dropped old markers: %s", got)
	}
}

// DX16-07: the objects are verified again where the secret is read, so a change between the request and the load ends the task.
func TestMarkerVerifiedAgainAtTheLoad(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	uid := firstImport(t, e, q, "db_xfer", "va_t")
	q("truncate public.va_t")
	ops.ImportFault = func(p string, _ uint32) error {
		if p == "afterCheck" {
			ops.ImportFault = nil
			e.svcExecDB(t, "db_xfer", "create index evil_i2 on kingres_transfer.import_marker (task_id)")
		}
		return nil
	}
	defer func() { ops.ImportFault = nil }()
	task := e.startImport(t, "db_xfer", "va_t", map[string]any{"uploadId": uid, "header": "none"})
	if task["state"] != "failed" || taskCode(task) != "forbidden" {
		t.Errorf("an import after the marker objects changed: %v", task["error"])
	}
	if got := count(t, q, "va_t"); got != "[[0]]" {
		t.Errorf("rows: %s", got)
	}
	e.svcExecDB(t, "db_xfer", "drop index kingres_transfer.evil_i2")
}

// svcExecDB runs a statement as the service role in database db.
func (e *cp) svcExecDB(t testing.TB, db, sql string) {
	t.Helper()
	if _, err := e.serviceConn(t, db).Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// The two additions to 0.19.0 asked for by the console: DataExport.spreadsheetSafe and the effective cap of one import in the data transfer settings.
func TestExportShowsItsGuardAndSettingsShowTheImportCap(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.cn_t (id int)")
	_, on := e.startExport(t, "db_xfer", "cn_t", map[string]any{})
	_, offID := e.startExport(t, "db_xfer", "cn_t", map[string]any{"spreadsheetSafe": false})
	if got := e.expect(t, 200, "GET", "/data-exports/"+on, nil)["spreadsheetSafe"]; got != true {
		t.Errorf("the export with the guard: %v", got)
	}
	if got := e.expect(t, 200, "GET", "/data-exports/"+offID, nil)["spreadsheetSafe"]; got != false {
		t.Errorf("the export without the guard: %v", got)
	}
	e.expect(t, 204, "DELETE", "/data-exports/"+on, nil)
	e.expect(t, 204, "DELETE", "/data-exports/"+offID, nil)
	st := e.expect(t, 200, "GET", "/settings/cluster", nil)
	dt, _ := st["dataTransfer"].(map[string]any)
	if dt["maxAtomicBytes"] != float64(25165824) {
		t.Errorf("the cap of one import in the settings: %v", dt["maxAtomicBytes"])
	}
}

// DX17-01: a database owner plants the schema, a secret table and a trigger in the window between the owner test and the creation of the objects. The service role must not
// insert into the planted table before the structure is verified: the import fails closed and the trigger never runs.
func TestMarkerPlantedInTheRaceWindow(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_race")
	q("create table if not exists public.rw_t (id int)")
	uid := e.uploadFile(t, []byte("1\n"))
	ops.ImportFault = func(p string, _ uint32) error {
		if p == "beforeMarkerCreate" {
			ops.ImportFault = nil
			q("create schema kingres_transfer")
			q("create table kingres_transfer.secret (singleton boolean primary key default true, k bytea not null)")
			q("create function kingres_transfer.boom() returns trigger language plpgsql as $$ begin create role kg_pwned2 superuser; return new; end $$")
			q("create trigger boom before insert on kingres_transfer.secret for each row execute function kingres_transfer.boom()")
		}
		return nil
	}
	defer func() { ops.ImportFault = nil }()
	task := e.startImport(t, "db_race", "rw_t", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true})
	if task["state"] != "failed" || taskCode(task) != "forbidden" {
		t.Errorf("an import with a schema planted in the race window: %v", task["error"])
	}
	if got := e.svcRow(t, "postgres", "select count(*) from pg_roles where rolname = 'kg_pwned2'"); got != "0" {
		t.Errorf("the planted trigger ran as the service role: %s", got)
	}
	q("drop schema kingres_transfer cascade")
	e.serviceExec(t, "drop role if exists kg_pwned2")
}

// DX17-02: a lock on the secret table that a role with write rights on all data can hold ends the load step after the lock timeout, with a retryable code, instead of waiting for ever.
func TestMarkerLockHeldBeforeTheLoadEndsRetryable(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	uid := firstImport(t, e, q, "db_xfer", "lh_t")
	holder := e.serviceConn(t, "db_xfer")
	var tx interface{ Rollback(context.Context) error }
	ops.ImportFault = func(p string, _ uint32) error {
		if p == "afterCheck" {
			ops.ImportFault = nil
			x, err := holder.Begin(context.Background())
			if err != nil {
				t.Error(err)
				return nil
			}
			tx = x
			if _, err := x.Exec(context.Background(), "lock table kingres_transfer.secret in access exclusive mode"); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	defer func() { ops.ImportFault = nil }()
	task := e.startImport(t, "db_xfer", "lh_t", map[string]any{"uploadId": uid, "header": "none"})
	if tx != nil {
		_ = tx.Rollback(context.Background())
	}
	er, _ := task["error"].(map[string]any)
	if task["state"] != "failed" || taskCode(task) != "transfer_session_lost" || er["retryable"] != true {
		t.Errorf("an import while the secret table is locked: %v", task["error"])
	}
	if task := e.startImport(t, "db_xfer", "lh_t", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" {
		t.Errorf("the import after the lock was released: %v", task["error"])
	}
}
