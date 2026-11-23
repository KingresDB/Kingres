// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/dumpnorm"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlshape"
)

// The database export against a real instance of the build of this tree, in a private installation (KINGRES_PG_BIN): the catalog is read as the service
// role, pg_dump of the same build is run with the arguments of the exporter, and the dump of a database without the project's extensions is normalised and
// accepted by the shape grammar. The cluster tests (cmd/kingresd, internal/clustertest) cover the tables of the extensions.

func pgFacts(t *testing.T, c *pgx.Conn) *dbFacts {
	t.Helper()
	var f *dbFacts
	tx, err := c.BeginTx(context.Background(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if f, err = readDBFacts(context.Background(), tx); err != nil {
		t.Fatalf("readDBFacts: %v", err)
	}
	return f
}

func TestReadFactsAndDumpOfARealDatabase(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	ctx := context.Background()
	exec := func(qs ...string) {
		t.Helper()
		for _, q := range qs {
			if _, err := c.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	exec(
		"create database dbx_pg",
	)
	c = in.Connect(t, "dbx_pg")
	exec(
		"create schema app",
		"create type app.mood as enum ('sad', 'ok')",
		"create domain app.posint as int check (value > 0)",
		"create table public.t_plain (id serial primary key, name text not null default 'x', mood app.mood, qty app.posint, gen int generated always as (id * 2) stored)",
		"create table public.t_child (id int primary key, plain_id int references public.t_plain(id) on delete cascade, note text check (length(note) < 50))",
		"create index t_plain_name on public.t_plain (lower(name))",
		"comment on table public.t_plain is 'the plain table'",
		"create view public.v_plain as select id, name from public.t_plain",
		"create materialized view public.mv_plain as select count(*) as c from public.t_plain",
		"create table public.inh_parent (id int primary key, note text)",
		"create table public.inh_child (extra int) inherits (public.inh_parent)",
		"create table public.lp (id int, d date) partition by range (d)",
		"create table public.lp_2026 partition of public.lp for values from ('2026-01-01') to ('2027-01-01')",
		"create table public.lp_old partition of public.lp default",
		"create table app.items (id bigint generated always as identity primary key, label text)",
		"insert into public.t_plain (name, mood, qty) values ('Ada', 'ok', 3)",
		"select nextval('app.items_id_seq')",
	)
	f := pgFacts(t, c)
	if f.Encoding != "UTF8" || f.PGMajor < 18 {
		t.Errorf("database facts: %s %d", f.Encoding, f.PGMajor)
	}
	byName := map[string]dbTable{}
	for _, tb := range f.Tables {
		byName[tb.qualified()] = tb
	}
	if len(byName) != 8 {
		t.Errorf("tables: %d", len(byName))
	}
	if got := strings.Join(byName["public.t_plain"].Cols, ","); got != "id,name,mood,qty" {
		t.Errorf("the columns of the entry of t_plain are the columns that are not generated: %s", got)
	}
	if len(byName["public.t_plain"].Gen) != 1 || byName["public.t_plain"].Gen[0] != "gen" {
		t.Errorf("generated columns: %v", byName["public.t_plain"].Gen)
	}
	if f.treeRole(byName["public.lp"]) != "root" || f.treeRole(byName["public.lp_2026"]) != "leaf" || f.treeRole(byName["public.inh_parent"]) != "inheritanceParent" || f.treeRole(byName["public.inh_child"]) != "inheritanceChild" {
		t.Error("tree roles")
	}
	if len(f.Seqs) != 2 {
		t.Errorf("sequences: %+v", f.Seqs)
	}
	for _, s := range f.Seqs {
		if s.Name == "items_id_seq" && (s.Last != 1 || !s.Called) {
			t.Errorf("the state of the sequence of the identity column: %+v", s)
		}
		if s.Name == "t_plain_id_seq" && (s.Called && s.Last < 1) {
			t.Errorf("sequence %+v", s)
		}
	}
	if len(f.Unsupported) != 0 || f.LargeObjects != 0 || len(f.OIDColumns) != 0 || len(f.Extensions) != 1 || f.Extensions[0].Name != "plpgsql" {
		t.Errorf("unsupported %v large objects %d oid columns %v extensions %v", f.Unsupported, f.LargeObjects, f.OIDColumns, f.Extensions)
	}
	fp := f.fingerprint()
	exec("alter table public.t_child add column extra2 int")
	if pgFacts(t, c).fingerprint() == fp {
		t.Error("a column added does not change the fingerprint")
	}
	exec("alter table public.t_child drop column extra2")
	if pgFacts(t, c).fingerprint() != fp {
		t.Error("the fingerprint is not stable over a column added and dropped")
	}

	// The dump of the build of the instance, in the snapshot of a read-only transaction.
	o := &Ops{Transfer: &TransferConfig{PGBinDir: pgtest.BinDir(), Dir: t.TempDir()}}
	tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var snap string
	if err := tx.QueryRow(ctx, "select pg_export_snapshot()").Scan(&snap); err != nil {
		t.Fatal(err)
	}
	cfg := c.Config()
	pre, err := o.runDump(ctx, cfg, "pre-data", snap, f.PGMajor)
	if err != nil {
		t.Fatalf("pg_dump pre-data: %v", err)
	}
	post, err := o.runDump(ctx, cfg, "post-data", snap, f.PGMajor)
	if err != nil {
		t.Fatalf("pg_dump post-data: %v", err)
	}
	if !strings.Contains(pre, "\\restrict") && !strings.Contains(pre, "CREATE TABLE public.t_plain") {
		t.Errorf("the dump is not a dump:\n%s", pre)
	}
	norm, err := dumpnorm.Normalise(pre, post, f.normFacts())
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	hints := f.inheritanceChildren()
	for name, text := range map[string]string{"pre-data.sql": norm.Pre, "views.sql": norm.Views, "post-data.sql": norm.Post} {
		plan, err := sqlshape.ParseFile(strings.NewReader(text), name, sqlshape.Hints{InheritanceChildren: hints})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, r := range plan.Refused {
			t.Errorf("%s: the grammar refuses %s: %s", name, r.Kind, r.Reason)
		}
	}
	if !strings.Contains(norm.Views, "CREATE VIEW public.v_plain") || !strings.Contains(norm.Views, "CREATE MATERIALIZED VIEW public.mv_plain") || strings.Contains(norm.Pre, "VIEW") {
		t.Errorf("views are not in the views file:\n%s", norm.Views)
	}
	_ = tx.Rollback(ctx)
	// Anything that an archive does not carry is found by the catalog before a dump is made.
	exec("create function public.f() returns int language sql as 'select 1'", "create unlogged table public.u (a int)", "select lo_create(0)")
	f2 := pgFacts(t, c)
	kinds := strings.Join(f2.Unsupported, "|")
	if !strings.Contains(kinds, "function: public.f") || !strings.Contains(kinds, "unlogged table: u") || f2.LargeObjects != 1 {
		t.Errorf("unsupported %q large objects %d", kinds, f2.LargeObjects)
	}
	// A table with a column of type oid is found too.
	exec("alter table public.t_child add column o oid")
	if f3 := pgFacts(t, c); len(f3.OIDColumns) != 1 || f3.OIDColumns[0] != "public.t_child.o" {
		t.Errorf("oid columns: %v", f3.OIDColumns)
	}
}
