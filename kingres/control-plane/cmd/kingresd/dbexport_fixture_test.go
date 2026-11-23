// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"testing"
)

// buildExportFixture makes, through the API and the workbench, a database that has every kind of table the database export carries: local tables with
// keys, sequences, comments, an enum, a domain, an inheritance pair, a local partitioned table, views and a materialized view, a reference table, hash
// tables (one user-partitioned), a columnar table, an updatable and an append-only ledger table and a time-partitioned table. It returns a query
// function of the workbench.
func buildExportFixture(t testing.TB, e *cp, db string) func(sql string) map[string]any {
	t.Helper()
	if st, _, _ := e.call("GET", "/databases/"+db, nil, nil); st != 404 {
		t.Fatalf("database %s exists", db)
	}
	e.mustTask(t, "POST", "/databases", map[string]any{"name": db, "sharding": map[string]any{"enabled": true, "bucketCount": 8},
		"replication": map[string]any{"durability": "async"}, "ledger": map[string]any{"mode": "optIn"}})
	// The analytics extensions refuse a table that a role other than a superuser owns unless the setting says that the owners are trusted.
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"analytics": map[string]any{"allowNonSuperuserOwners": true}})
	q := func(sql string) map[string]any {
		t.Helper()
		return e.expect(t, 200, "POST", "/query", map[string]any{"database": db, "sql": sql})
	}
	tbl := func(name string) string { return "/databases/" + db + "/schemas/public/tables/" + name }
	for _, s := range []string{
		"create schema app",
		"create type app.mood as enum ('sad', 'ok', 'happy')",
		"create domain app.posint as int check (value > 0)",
		"create table public.t_plain (id serial primary key, name text not null default 'x', mood app.mood, qty app.posint, tags text[], at timestamptz default now(), n numeric(10,2))",
		"create index t_plain_name on public.t_plain (lower(name))",
		"comment on table public.t_plain is 'the plain table'",
		"comment on column public.t_plain.name is 'the name'",
		"create table public.t_child (id int primary key, plain_id int references public.t_plain(id) on delete cascade, note text check (length(note) < 50), gen int generated always as (id * 2) stored)",
		"create view public.v_plain as select id, name from public.t_plain",
		"create materialized view public.mv_plain as select count(*) as c from public.t_plain",
		"create table public.inh_parent (id int primary key, note text)",
		"create table public.inh_child (extra int) inherits (public.inh_parent)",
		"create table public.lp (id int, d date, v text) partition by range (d)",
		"create table public.lp_2026 partition of public.lp for values from ('2026-01-01') to ('2027-01-01')",
		"create table public.lp_old partition of public.lp default",
		"create table app.items (id bigint generated always as identity primary key, label text)",
		"create table public.refdata (id int primary key, v text)",
		"create table public.d_hash (id bigint primary key, tenant int not null, v text)",
		"create index d_hash_tenant on public.d_hash (tenant)",
		"create table public.d_part (id bigint not null, ts timestamptz not null, v text, primary key (id, ts)) partition by range (ts)",
		"create table public.c_events (id bigint, v text)",
		"create table public.acc (id bigint primary key, bal int)",
		"create table public.audit_rows (id bigint primary key, what text)",
		"create table public.ev (id bigint not null, at timestamptz not null, v text, primary key (id, at)) partition by range (at)",
	} {
		q(s)
	}
	e.mustTask(t, "PUT", tbl("refdata")+"/distribution", map[string]any{"method": "reference"})
	e.mustTask(t, "PUT", tbl("d_hash")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	e.mustTask(t, "PUT", tbl("d_part")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	e.mustTask(t, "POST", tbl("d_part")+"/partitions", map[string]any{"name": "d_part_2026", "bound": "FROM ('2026-01-01') TO ('2027-01-01')"})
	e.mustTask(t, "POST", tbl("d_part")+"/partitions", map[string]any{"name": "d_part_rest", "bound": "DEFAULT"})
	e.mustTask(t, "PUT", tbl("c_events")+"/storage", map[string]any{"accessMethod": "columnar"})
	e.expect(t, 200, "PUT", tbl("acc")+"/ledger", map[string]any{"mode": "updatable"})
	e.expect(t, 200, "PUT", tbl("audit_rows")+"/ledger", map[string]any{"mode": "appendOnly"})
	e.expect(t, 200, "PUT", tbl("ev")+"/partition-policy", map[string]any{"partitionEvery": "P1D", "createAhead": "P3D", "retention": "P30D", "retentionAction": "drop", "enabled": true})
	for _, s := range []string{
		"insert into public.t_plain (name, mood, qty, tags, n) values ('Ada', 'ok', 3, '{a,b}', 12.50), ('', null, null, null, null), ('\\N', 'happy', 1, '{}', -0.01), ('line' || chr(10) || 'break, \"q\"', 'sad', 2, null, 7)",
		"insert into public.t_child (id, plain_id, note) values (1, 1, 'one'), (2, 1, ''), (3, null, null)",
		"insert into public.inh_parent values (1, 'p')",
		"insert into public.inh_child values (2, 'c', 9)",
		"insert into public.lp values (1, '2026-03-01', 'a'), (2, '2020-01-01', 'old')",
		"insert into app.items (label) values ('x'), ('y')",
		"insert into public.refdata values (1, 'a'), (2, 'b')",
		"insert into public.d_hash select g, g % 5, 'v' || g from generate_series(1, 200) g",
		"insert into public.d_part select g, '2026-02-01'::timestamptz + (g || ' hours')::interval, 'p' || g from generate_series(1, 60) g",
		"insert into public.d_part values (1000, '2030-01-01', 'late')",
		"insert into public.c_events select g, 'e' || g from generate_series(1, 300) g",
		"insert into public.acc values (1, 10), (2, 20)",
		"insert into public.audit_rows values (1, 'first')",
	} {
		q(s)
	}
	return q
}
