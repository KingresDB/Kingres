// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"strings"
	"testing"
)

// Databases that a hostile tenant made: the export reads and calls nothing that the tenant could have planted, and refuses with a clear problem. The tenant is a
// role that owns objects of the database; the export runs as the service role (a superuser) for the catalog and as the signed-in role for the data.

func hostileDB(t *testing.T, e *cp, name string) func(sql string) {
	t.Helper()
	e.mustTask(t, "POST", "/databases", map[string]any{"name": name})
	return func(sql string) {
		t.Helper()
		e.expect(t, 200, "POST", "/query", map[string]any{"database": name, "sql": sql})
	}
}

func markerRows(t *testing.T, e *cp, db string) int {
	t.Helper()
	out := e.expect(t, 200, "POST", "/query", map[string]any{"database": db, "sql": "select count(*) from public.marker"})
	return int(out["rows"].([]any)[0].([]any)[0].(float64))
}

// A schema kingres that the tenant made, with a table the control plane would read and a function that would write a marker, is never read or called.
func TestExportRefusesAFakeKingresSchema(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	// A database made without the extensions (the control plane's own databases always have the schema kingres), with a schema kingres owned by the tenant.
	admin := e.serviceConn(t, "postgres")
	for _, sql := range []string{"create role dbx_tenant", "create database dbx_fake"} {
		if _, err := admin.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
	svc := e.serviceConn(t, "dbx_fake")
	for _, sql := range []string{
		"create table public.marker (t text)", "create table public.t (id int primary key)", "grant all on all tables in schema public to public",
		"create schema kingres authorization dbx_tenant",
		"create function kingres.evil() returns bigint language plpgsql as $$ begin insert into public.marker values ('ran'); return 1; end $$",
		"create view kingres.dist_table as select kingres.evil() as relid, 'hash'::text as method, 'id'::text as dist_column, 1 as colocation_id",
	} {
		if _, err := svc.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	st, out := e.startDBExport(t, "dbx_fake", map[string]any{})
	if st != 422 || out["code"] != "schema_unrecognised" || !strings.Contains(out["detail"].(string), "kingres") || !strings.Contains(out["detail"].(string), "cannot be trusted") {
		t.Fatalf("a fake schema kingres: %d %v", st, out)
	}
	var n int
	if err := svc.QueryRow(context.Background(), "select count(*) from public.marker").Scan(&n); err != nil || n != 0 {
		t.Errorf("the function of the tenant ran %d times (%v)", n, err)
	}
	// An empty schema kingres of the tenant is refused too (the owner is what counts, not what is in it).
	for _, sql := range []string{"create database dbx_fake2"} {
		if _, err := admin.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
	svc2 := e.serviceConn(t, "dbx_fake2")
	if _, err := svc2.Exec(context.Background(), "create schema kingres authorization dbx_tenant"); err != nil {
		t.Fatal(err)
	}
	if st, out := e.startDBExport(t, "dbx_fake2", map[string]any{}); st != 422 || out["code"] != "schema_unrecognised" {
		t.Errorf("an empty schema kingres of the tenant: %d %v", st, out)
	}
	// With the schema of the tenant gone the same database is exported.
	if _, err := svc.Exec(context.Background(), "drop schema kingres cascade"); err != nil {
		t.Fatal(err)
	}
	if id := e.exportDB(t, "dbx_fake", map[string]any{}); id == "" {
		t.Fatal("an export of the database without the fake schema")
	}
}

// A schema kingres_transfer that the service role did not make is refused.
func TestExportRefusesAFakeTransferSchema(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := hostileDB(t, e, "dbx_faket")
	q("create table public.t (id int primary key)")
	q("create schema kingres_transfer")
	st, out := e.startDBExport(t, "dbx_faket", map[string]any{})
	if st != 422 || out["code"] != "schema_unrecognised" || !strings.Contains(out["detail"].(string), "kingres_transfer") {
		t.Fatalf("a fake schema kingres_transfer: %d %v", st, out)
	}
}

// ALTER DATABASE ... SET search_path to a schema of the tenant that shadows functions of the catalog: nothing the control plane asks of the catalog goes there.
func TestExportIgnoresASearchPathOfTheDatabase(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := hostileDB(t, e, "dbx_path")
	q("create table public.marker (t text)")
	q("create table public.t (id int primary key, v text)")
	q("create schema evil")
	for _, fn := range []string{"to_regclass(text) returns regclass", "pg_get_userbyid(oid) returns name", "format_type(oid, integer) returns text", "has_table_privilege(oid, text) returns boolean",
		"has_schema_privilege(oid, text) returns boolean", "pg_encoding_to_char(integer) returns name", "current_setting(text) returns text", "pg_total_relation_size(oid) returns bigint"} {
		q("create function evil." + fn + " language plpgsql as $$ begin insert into public.marker values ('ran'); raise exception 'the function of the tenant ran'; end $$")
	}
	c := e.serviceConn(t, "dbx_path")
	if _, err := c.Exec(context.Background(), "alter database dbx_path set search_path = evil, pg_catalog, public"); err != nil {
		t.Fatal(err)
	}
	// The functions of the tenant are objects that an archive does not carry: the export is refused for that, and for nothing else. Had a catalog function been
	// resolved to one of the tenant's, the answer would be the exception that they raise.
	st, out := e.startDBExport(t, "dbx_path", map[string]any{})
	if st != 422 || out["code"] != "schema_unrecognised" || !strings.Contains(out["detail"].(string), "function") {
		t.Fatalf("a search path of the database: %d %v", st, out)
	}
	if n := markerRows(t, e, "dbx_path"); n != 0 {
		t.Errorf("a function of the tenant ran %d times", n)
	}
}

// A sequence that the role cannot read refuses the export, and the role that is given it exports.
func TestExportRefusesASequenceTheRoleCannotRead(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := hostileDB(t, e, "dbx_seq")
	q("create table public.t (id serial primary key, v text)")
	pw := "Reader-Passw0rd-2"
	e.expect(t, 201, "POST", "/roles", map[string]any{"name": "dbx_seqrole", "password": pw, "clusterRoles": []string{"kingres_admin"}})
	q("grant select on all tables in schema public to dbx_seqrole")
	tok, ok := e.loginAs("dbx_seqrole", pw)
	if !ok {
		t.Fatal("sign in")
	}
	other := e.as(tok)
	st, out, _ := other.call("POST", "/databases/dbx_seq/export", map[string]any{}, nil)
	if st != 422 || out["code"] != "export_not_readable" || !strings.Contains(out["detail"].(string), "public.t_id_seq") {
		t.Fatalf("a sequence that the role cannot read: %d %v", st, out)
	}
	q("grant select on sequence public.t_id_seq to dbx_seqrole")
	if st, out, _ := other.call("POST", "/databases/dbx_seq/export", map[string]any{}, nil); st != 202 {
		t.Fatalf("with the privilege on the sequence: %d %v", st, out)
	} else {
		other.waitTask(t, out)
	}
}
