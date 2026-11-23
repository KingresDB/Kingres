// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// The parts of the database import that need a real catalog (PostgreSQL 18 or 19, whichever the private installation is): the check of the functions against
// pg_proc, the check that the service role may touch a table, the extended protocol that refuses several statements, the search path that makes an unqualified
// name harmless, and the count of the rows that violate a foreign key.

func pgExecAll(t *testing.T, c *pgx.Conn, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := c.Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestImportFunctionCheckOnARealCatalog(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	pgExecAll(t, c, "create extension citext", "create function public.mine(int) returns int language sql as 'select 1'")
	ctx := context.Background()
	names := []string{"lower", "pg_catalog.lower", "now", "nextval", "abs", "coalesce_is_not_a_function", "pg_sleep", "pg_catalog.pg_sleep", "pg_read_file", "pg_read_binary_file", "pg_ls_dir", "lo_import",
		"set_config", "setval", "pg_terminate_backend", "pg_advisory_lock", "query_to_xml", "dblink", "mine", "public.mine", "public.citext_ne", "a.b.c", "PG_SLEEP", "pg_stat_file", "pg_reload_conf"}
	var got []string
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	got, err = checkFunctions(ctx, tx, names, map[string]string{"citext": "public"})
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]bool{}
	for _, r := range got {
		refused[strings.SplitN(r, ":", 2)[0]] = true
	}
	for _, n := range []string{"lower", "pg_catalog.lower", "now", "nextval", "abs", "public.citext_ne"} {
		if refused[safeName(n)] {
			t.Errorf("%s is refused: %v", n, got)
		}
	}
	for _, n := range []string{"coalesce_is_not_a_function", "pg_sleep", "pg_catalog.pg_sleep", "pg_read_file", "pg_read_binary_file", "pg_ls_dir", "lo_import", "set_config", "setval", "pg_terminate_backend",
		"pg_advisory_lock", "query_to_xml", "dblink", "mine", "public.mine", "a.b.c", "PG_SLEEP", "pg_stat_file", "pg_reload_conf"} {
		if !refused[safeName(n)] {
			t.Errorf("%s is not refused: %v", n, got)
		}
	}
	// The extension that the archive does not list does not make its functions callable.
	got, err = checkFunctions(ctx, tx, []string{"public.citext_ne"}, map[string]string{})
	if err != nil || len(got) != 1 {
		t.Errorf("a function of an extension that the manifest does not list: %v %v", got, err)
	}
}

func TestTableTrustedOnARealCatalog(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	ctx := context.Background()
	pgExecAll(t, c, "create role imp_owner", "create role other_owner",
		"create function public.mine(int) returns int language sql as 'select 1'",
		"create function public.trg() returns trigger language plpgsql as $$ begin return new; end $$",
		"grant create, usage on schema public to imp_owner, other_owner")
	mk := func(role string, qs ...string) {
		t.Helper()
		pgExecAll(t, c, "set role "+role)
		defer pgExecAll(t, c, "reset role")
		pgExecAll(t, c, qs...)
	}
	mk("imp_owner",
		"create table public.ok1 (id int primary key, v text default lower('X'), d timestamptz default now())",
		"create table public.part (id int, d date) partition by range (d)",
		"create table public.with_trigger (id int)",
		"create trigger t1 before insert on public.with_trigger for each row execute function public.trg()",
		"create table public.with_rule (id int)",
		"create rule r1 as on insert to public.with_rule do instead nothing",
		"create table public.with_policy (id int)",
		"create policy p1 on public.with_policy using (true)",
		"create table public.with_rls (id int)",
		"alter table public.with_rls enable row level security",
		"create table public.with_fn (id int, v int default public.mine(1))",
		"create table public.with_fn_idx (id int)",
		"create index on public.with_fn_idx (public.mine(id))",
		"create table public.with_check (id int check (public.mine(id) > 0))",
		"create table public.with_sleep (id int, v text default pg_sleep(0)::text)",
		"create view public.a_view as select 1 as x")
	mk("other_owner", "create table public.foreign_owned (id int)")
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ext := map[string]string{}
	for table, ok := range map[string]bool{"ok1": true, "part": true, "with_trigger": false, "with_rule": false, "with_policy": false, "with_rls": false, "with_fn": false, "with_fn_idx": false,
		"with_check": false, "with_sleep": false, "a_view": false, "foreign_owned": false, "does_not_exist": false} {
		err := tableTrusted(ctx, tx, "public", table, "imp_owner", ext)
		if (err == nil) != ok {
			t.Errorf("table %s: trusted=%v, want %v (%v)", table, err == nil, ok, err)
		}
	}
	// The error names the table and the reason, and never a value.
	if err := tableTrusted(ctx, tx, "public", "with_fn", "imp_owner", ext); err == nil || !strings.Contains(err.Error(), "public.mine") {
		t.Errorf("the reason for with_fn: %v", err)
	}
}

// The text of one statement goes through the extended protocol: a server that reads it as several statements refuses it.
func TestExecOneIsOneStatement(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	ctx := context.Background()
	if err := execOne(ctx, c, "create temp table one_stmt (a int)"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"select 1; select 2", "create temp table two_a (a int); drop table one_stmt", "select 1; set role postgres", "select 1;;select 2"} {
		err := execOne(ctx, c, text)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42601" {
			t.Errorf("%q: %v", text, err)
		}
	}
	var n int
	if err := c.QueryRow(ctx, "select count(*) from pg_tables where tablename in ('one_stmt', 'two_a')").Scan(&n); err != nil || n != 1 {
		t.Errorf("tables after the refusals: %d (%v)", n, err)
	}
}

// An unqualified name finds nothing that the role could have put in front of the system: the search path of the session is pg_catalog, and a role cannot create there.
func TestUnqualifiedNamesDoNotLandAnywhere(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	ctx := context.Background()
	pgExecAll(t, c, "create role imp_user", "grant create, usage on schema public to imp_user", "set role imp_user")
	for _, s := range importSession {
		if _, err := c.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	err := execOne(ctx, c, "CREATE TABLE unqualified (a int)")
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "42501" {
		t.Errorf("an unqualified CREATE TABLE: %v", err)
	}
	if err := execOne(ctx, c, "CREATE TABLE public.qualified (a int)"); err != nil {
		t.Errorf("a qualified CREATE TABLE: %v", err)
	}
	// A function that the role defines in public is not found by an unqualified call.
	pgExecAll(t, c, "create function public.now() returns text language sql as $$ select 'mine' $$")
	var s string
	if err := c.QueryRow(ctx, "select now()::text").Scan(&s); err != nil || strings.Contains(s, "mine") {
		t.Errorf("an unqualified call finds %q (%v)", s, err)
	}
}

func TestForeignKeyViolationsAreCounted(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	ctx := context.Background()
	pgExecAll(t, c, "create table public.p (a int, b int, primary key (a, b))", "create table public.ch (id int, a int, b int)",
		"insert into public.p values (1, 1), (2, 2)",
		"insert into public.ch values (1, 1, 1), (2, 3, 3), (3, 4, 4), (4, null, 9), (5, 2, 2)",
		"alter table public.ch add constraint ch_fk foreign key (a, b) references public.p (a, b) not valid")
	n, err := fkViolations(ctx, c, "ch_fk", "\"public\".\"ch\"")
	if err != nil || n != 2 {
		t.Errorf("violations: %d (%v), want 2 (a null in one column is not a violation under MATCH SIMPLE)", n, err)
	}
	if _, err := fkViolations(ctx, c, "nope", "\"public\".\"ch\""); err == nil {
		t.Error("a constraint that does not exist")
	}
	// The statement with NOT VALID that the importer adds counts the same and rolls back.
	if cnt, err := (&Ops{}).countByRollback(ctx, c, "ALTER TABLE public.ch ADD CONSTRAINT ch_fk2 FOREIGN KEY (a, b) REFERENCES public.p (a, b)", "ch_fk2", "ch", "public"); err != nil || cnt != 2 {
		t.Errorf("the count of the statement that failed: %d (%v)", cnt, err)
	}
	var exists bool
	if err := c.QueryRow(ctx, "select exists (select 1 from pg_constraint where conname = 'ch_fk2')").Scan(&exists); err != nil || exists {
		t.Errorf("the constraint of the count stays: %v %v", exists, err)
	}
}
