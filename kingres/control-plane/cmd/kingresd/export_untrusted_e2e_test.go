// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"
)

// A read as the administrator runs the expressions that the owner of the table chose. A table owned by another role whose policy calls a function owned by
// that role is refused (export_untrusted_code) until the exporter says it accepts the risk; the owner reading its own table is not asked.
func TestExportUntrustedFunction(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	ownerTok := e.accountWithRole(t, "xfer_owner", "kingres_admin")
	readerTok := e.accountWithRole(t, "xfer_reader", "kingres_admin")
	q("create table if not exists public.ut_t (id int primary key, v text)")
	q("insert into public.ut_t values (1, 'a'), (2, 'b') on conflict do nothing")
	q("create or replace function public.ut_f(int) returns boolean language sql immutable as 'select true'")
	q("drop policy if exists ut_p on public.ut_t")
	q("create policy ut_p on public.ut_t using (public.ut_f(id))")
	q("grant create on schema public to xfer_owner, xfer_reader")
	q("grant xfer_owner to root_admin")
	q("grant xfer_reader to root_admin")
	q("alter function public.ut_f(int) owner to xfer_owner")
	q("alter table public.ut_t owner to xfer_owner")
	q("grant select on public.ut_t to xfer_reader")
	post := func(tok string, body map[string]any) (int, map[string]any) {
		st, out, _ := e.as(tok).call("POST", "/databases/db_xfer/schemas/public/tables/ut_t/export", body, nil)
		return st, out
	}
	st, out := post(readerTok, map[string]any{})
	if st != 422 || problemCode(out) != "export_untrusted_code" || !strings.Contains(toJSON(out), "ut_f") {
		t.Fatalf("a table with a policy that calls a function of its owner, read by another role: %d %v", st, out)
	}
	if st, out := post(readerTok, map[string]any{"acknowledgeOtherRolesCode": false}); st != 422 {
		t.Errorf("acknowledge false: %d %v", st, out)
	}
	st, out = post(readerTok, map[string]any{"acknowledgeOtherRolesCode": true})
	if st != 202 {
		t.Fatalf("with the risk accepted: %d %v", st, out)
	}
	done := e.as(readerTok).waitTask(t, out)
	if done["state"] != "done" {
		t.Errorf("the accepted export: %v", done["error"])
	}
	// The audit event of the request says that the risk was accepted.
	if ev := e.expect(t, 200, "GET", "/audit?action=exportTable", nil); !strings.Contains(toJSON(ev), "\"acknowledgeOtherRolesCode\":true") {
		t.Errorf("no audit event: %v", ev)
	}
	// The owner is not asked: it reads its own table with its own functions.
	st, out = post(ownerTok, map[string]any{})
	if st != 202 {
		t.Errorf("the owner: %d %v", st, out)
	} else if d := e.as(ownerTok).waitTask(t, out); d["state"] != "done" {
		t.Errorf("the owner's export: %v", d["error"])
	}
	// A function of the reader is not another role's code.
	q("alter function public.ut_f(int) owner to xfer_reader")
	if st, out := post(readerTok, map[string]any{}); st != 202 {
		t.Errorf("a function owned by the reader: %d %v", st, out)
	} else {
		e.as(readerTok).waitTask(t, out)
	}
	q("alter table public.ut_t owner to root_admin")
	q("revoke all on public.ut_t from xfer_reader")
	q("drop policy ut_p on public.ut_t")
	q("drop function public.ut_f(int)")
	q("revoke create on schema public from xfer_owner, xfer_reader")
	q("revoke xfer_owner from root_admin")
	q("revoke xfer_reader from root_admin")
	for _, id := range []string{"xfer_reader", "xfer_owner"} {
		e.expect(t, 204, "DELETE", "/roles/"+id, nil)
	}
}
