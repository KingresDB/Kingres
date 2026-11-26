// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// Tests of the fixes of the independent review of phase 14a (docs/reviews/2026-11-22-data-export-import-14a.md). Each fails without its fix.

// DX14-03: an import runs the triggers, defaults and checks of the table as the signed-in role, so a role that bypasses row security is refused at the request.
func TestImportRefusesADangerousRole(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.dr_t (id int)")
	tok := e.accountWithRole(t, "xfer_bypass", "kingres_admin")
	q("grant insert on public.dr_t to xfer_bypass")
	e.serviceExec(t, "alter role xfer_bypass bypassrls")
	as := e.as(tok)
	uid := as.uploadFile(t, []byte("1\n"))
	st, out, _ := as.call("POST", "/databases/db_xfer/schemas/public/tables/dr_t/import", map[string]any{"uploadId": uid, "header": "none"}, nil)
	if st != 403 || !strings.Contains(toJSON(out), "BYPASSRLS") {
		t.Errorf("a role with BYPASSRLS: %d %v", st, out)
	}
	if got := count(t, q, "dr_t"); got != "[[0]]" {
		t.Errorf("rows: %s", got)
	}
	e.serviceExec(t, "alter role xfer_bypass nobypassrls")
	if task := as.startImport(t, "db_xfer", "dr_t", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" {
		t.Errorf("the same role without the attribute: %v", task["error"])
	}
	q("revoke all on public.dr_t from xfer_bypass")
	e.expect(t, 204, "DELETE", "/roles/xfer_bypass", nil)
}

// DX14-10: the byte order mark is removed for UTF-8 only; for another encoding the option is refused, not guessed.
func TestImportByteOrderMark(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.bom_t (a int, b text)")
	q("truncate public.bom_t")
	uid := e.uploadFile(t, append([]byte{0xEF, 0xBB, 0xBF}, []byte("1,x\n2,y\n")...))
	if task := e.startImport(t, "db_xfer", "bom_t", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" || count(t, q, "bom_t") != "[[2]]" {
		t.Fatalf("a UTF-8 file with a mark: %v", task["error"])
	}
	uid = e.uploadFile(t, []byte("3,z\n"))
	st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/bom_t/import", map[string]any{"uploadId": uid, "header": "none", "encoding": "LATIN1", "skipBom": true}, nil)
	if st != 400 || !strings.Contains(toJSON(out), "/skipBom") {
		t.Errorf("skipBom with another encoding: %d %v", st, out)
	}
	// Without the option named, a file in another encoding is read as it is (no byte is dropped).
	if task := e.startImport(t, "db_xfer", "bom_t", map[string]any{"uploadId": uid, "header": "none", "encoding": "LATIN1"}); task["state"] != "done" || count(t, q, "bom_t") != "[[3]]" {
		t.Errorf("a LATIN1 file: %v", task["error"])
	}
}

// DX14-09: the column types of the request (the typed-array cast of a reference table) are compared again when the task runs; a column changed in between ends the task, nothing is stored.
func TestImportColumnTypeChangedBeforeTheStep(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("drop table if exists public.tc_t")
	q("create table public.tc_t (a int primary key, b int)")
	e.mustTask(t, "PUT", "/databases/db_xfer/schemas/public/tables/tc_t/distribution", map[string]any{"method": "reference"})
	uid := e.uploadFile(t, []byte("1,2\n"))
	ops.ImportFault = func(p string, _ uint32) error {
		if p == "beforeCheck" {
			ops.ImportFault = nil
			q("alter table public.tc_t alter column b type text")
		}
		return nil
	}
	defer func() { ops.ImportFault = nil }()
	task := e.startImport(t, "db_xfer", "tc_t", map[string]any{"uploadId": uid, "header": "none"})
	if task["state"] != "failed" || !strings.Contains(taskMessage(task), "type of a column") {
		t.Errorf("a changed column type: %v", task["error"])
	}
	if got := count(t, q, "tc_t"); got != "[[0]]" {
		t.Errorf("rows: %s", got)
	}
	q("drop table public.tc_t")
}

// DX14-08: the cap on a plain file applies at the request; a file that is gzip is not capped by its compressed size.
func TestImportSizeCapLooksAtTheFile(t *testing.T) {
	if testing.Short() {
		t.Skip("stages 520 MiB")
	}
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.cap_ref (a text)")
	e.mustTask(t, "PUT", "/databases/db_xfer/schemas/public/tables/cap_ref/distribution", map[string]any{"method": "reference"})
	noise := make([]byte, 257<<20)
	_, _ = rand.Read(noise)
	uid := e.uploadFile(t, noise)
	st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/cap_ref/import", map[string]any{"uploadId": uid, "header": "none"}, nil)
	if st != 422 || problemCode(out) != "import_too_large" {
		t.Errorf("a plain file above the cap: %d %v", st, out)
	}
	e.expect(t, 204, "DELETE", "/data-uploads/"+uid, nil)
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.NoCompression)
	_, _ = zw.Write(noise)
	_ = zw.Close()
	uid = e.uploadFile(t, gz.Bytes())
	st, out, _ = e.call("POST", "/databases/db_xfer/schemas/public/tables/cap_ref/import", map[string]any{"uploadId": uid, "header": "none"}, nil)
	if st != 202 {
		t.Errorf("a gzip file whose compressed size is above the cap: %d %v", st, out)
	} else {
		e.waitTask(t, out)
	}
	e.expect(t, 204, "DELETE", "/data-uploads/"+uid, nil)
}

// DX14-04: the header cells are under the spreadsheet guard. (The xml part of DX14-11 cannot be run here: the test build of PostgreSQL has no libxml, so no xml
// value can be made; the column kind is decided in readFacts.)
func TestExportHeaderAndXMLCellsAreGuarded(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q(`drop table if exists public.hg_t`)
	q(`create table public.hg_t ("=1+1" int, ok text)`)
	q(`insert into public.hg_t values (1, 'a')`)
	_, id := e.startExport(t, "db_xfer", "hg_t", map[string]any{})
	st, _, body := e.download(t, id, nil)
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if st != 200 || len(lines) != 2 || lines[0] != "'=1+1,ok" || lines[1] != "1,a" {
		t.Errorf("guarded export (%d): %q", st, body)
	}
	e.expect(t, 204, "DELETE", "/data-exports/"+id, nil)
	_, id = e.startExport(t, "db_xfer", "hg_t", map[string]any{"spreadsheetSafe": false})
	_, _, body = e.download(t, id, nil)
	lines = strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 || lines[0] != "=1+1,ok" || lines[1] != "1,a" {
		t.Errorf("unguarded export: %q", body)
	}
	e.expect(t, 204, "DELETE", "/data-exports/"+id, nil)
	q(`drop table public.hg_t`)
}

// DX14-03 (export) and DX14-05: a superuser is a member of every role, so the owner test must compare the owner with the role itself; and a table with forced row
// security is never exported as a shorter table (the read runs with row_security = off and fails).
func TestExportSuperuserAndForcedRowSecurity(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	ownerTok := e.accountWithRole(t, "xfer_so_owner", "kingres_admin")
	suTok := e.accountWithRole(t, "xfer_so_su", "kingres_admin")
	q("create table if not exists public.so_t (id int primary key, v text)")
	q("insert into public.so_t values (1, 'a'), (2, 'b') on conflict do nothing")
	q("create or replace function public.so_f(int) returns boolean language sql immutable as 'select true'")
	q("drop policy if exists so_p on public.so_t")
	q("create policy so_p on public.so_t using (public.so_f(id))")
	q("grant create on schema public to xfer_so_owner")
	q("grant xfer_so_owner to root_admin")
	q("alter function public.so_f(int) owner to xfer_so_owner")
	q("alter table public.so_t owner to xfer_so_owner")
	q("grant select on public.so_t to xfer_so_su")
	e.serviceExec(t, "alter role xfer_so_su superuser")
	st, out, _ := e.as(suTok).call("POST", "/databases/db_xfer/schemas/public/tables/so_t/export", map[string]any{}, nil)
	if st != 422 || problemCode(out) != "export_untrusted_code" {
		t.Errorf("a superuser reading a table of another owner with the owner's function: %d %v", st, out)
	}
	e.serviceExec(t, "alter role xfer_so_su nosuperuser")

	// Forced row security: the owner is subject to its own policy, and the export of it must fail rather than hold one row of two.
	q("drop policy so_p on public.so_t")
	q("alter table public.so_t enable row level security")
	q("alter table public.so_t force row level security")
	q("create policy so_p2 on public.so_t using (id < 2)")
	st, out, _ = e.as(ownerTok).call("POST", "/databases/db_xfer/schemas/public/tables/so_t/export", map[string]any{}, nil)
	if st == 202 {
		done := e.as(ownerTok).waitTask(t, out)
		if done["state"] == "done" {
			t.Errorf("an export of a table with forced row security ended done: %v", done["detail"])
		}
	}
	q("alter table public.so_t no force row level security")
	q("alter table public.so_t disable row level security")
	q("drop policy so_p2 on public.so_t")
	q("alter table public.so_t owner to root_admin")
	q("drop function public.so_f(int)")
	q("revoke create on schema public from xfer_so_owner")
	q("revoke all on public.so_t from xfer_so_su")
	q("revoke xfer_so_owner from root_admin")
	for _, id := range []string{"xfer_so_su", "xfer_so_owner"} {
		e.expect(t, 204, "DELETE", "/roles/"+id, nil)
	}
}

// DX14-12: two requests of one role (for two tables, so that the rule of one task per table does not decide) at the same time start one task, not two.
func TestOneTransferPerRoleUnderRace(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.race_a (id int)")
	q("create table if not exists public.race_b (id int)")
	tok := e.accountWithRole(t, "xfer_race", "kingres_admin")
	q("grant select on public.race_a, public.race_b to xfer_race")
	as := e.as(tok)
	ops.StartGap = func() { time.Sleep(600 * time.Millisecond) }
	defer func() { ops.StartGap = nil }()
	for round := 0; round < 2; round++ {
		var wg sync.WaitGroup
		codes := make([]int, 2)
		bodies := make([]map[string]any, 2)
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i], bodies[i], _ = as.call("POST", "/databases/db_xfer/schemas/public/tables/race_"+string(rune('a'+i))+"/export", map[string]any{}, nil)
			}()
		}
		wg.Wait()
		ok := 0
		for i, c := range codes {
			if c == 202 {
				ok++
				done := as.waitTask(t, bodies[i])
				if done["state"] == "done" {
					if d, _ := done["detail"].(string); strings.HasPrefix(d, "transfer ") {
						as.expect(t, 204, "DELETE", "/data-exports/"+strings.TrimPrefix(d, "transfer "), nil)
					}
				}
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d requests were accepted (%v)", round, ok, codes)
		}
	}
	q("revoke all on public.race_a, public.race_b from xfer_race")
	e.expect(t, 204, "DELETE", "/roles/xfer_race", nil)
}

// DX14-12: a refused export or upload leaves no record behind.
func TestRefusedTransfersLeaveNoRecord(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.nr_t (id int)")
	before := len(e.expect(t, 200, "GET", "/data-exports", nil)["items"].([]any))
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"stagingQuotaBytes": 1 << 20}})
	st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/nr_t/export", map[string]any{}, nil)
	if st != 409 || problemCode(out) != "transfer_quota_exceeded" {
		t.Errorf("a quota that does not fit: %d %v", st, out)
	}
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"stagingQuotaBytes": 1 << 30}})
	if after := len(e.expect(t, 200, "GET", "/data-exports", nil)["items"].([]any)); after != before {
		t.Errorf("a refused export left a record: %d -> %d", before, after)
	}
}

// DX14-12: turning the setting off ends a task that had not begun to move data.
func TestSettingOffEndsAPendingTask(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.so2_t (id int)")
	q("truncate public.so2_t")
	uid := e.uploadFile(t, []byte("1\n"))
	ops.ImportFault = func(p string, _ uint32) error {
		if p == "beforeStart" {
			ops.ImportFault = nil
			e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": false}})
		}
		return nil
	}
	defer func() { ops.ImportFault = nil }()
	task := e.startImport(t, "db_xfer", "so2_t", map[string]any{"uploadId": uid, "header": "none"})
	if task["state"] != "failed" || !strings.Contains(taskMessage(task), "turned off") {
		t.Errorf("a task started before the setting was turned off: %v", task["error"])
	}
	if got := count(t, q, "so2_t"); got != "[[0]]" {
		t.Errorf("rows: %s", got)
	}
}

// serviceExec runs a statement as the service role (the API cannot set the attributes of a role that the tests need).
func (e *cp) serviceExec(t testing.TB, sql string) {
	t.Helper()
	if _, err := e.serviceConn(t, "postgres").Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
