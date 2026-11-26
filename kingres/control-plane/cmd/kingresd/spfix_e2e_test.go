// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"testing"
)

// DX16-01: the owner of a database sets the search path of the database to a schema of its own that holds an operator, and the service role (a superuser) must not
// run it. The service path used is the distribution of a table as a reference table, whose check compares a regclass with a regclass.
func TestServiceSessionIgnoresTheSearchPathOfTheDatabase(t *testing.T) {
	e := shared(t)
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "db_sp", "sharding": map[string]any{"enabled": true, "bucketCount": 8}, "replication": map[string]any{"durability": "async"}, "ledger": map[string]any{"mode": "off"}})
	q := func(sql string) map[string]any {
		return e.expect(t, 200, "POST", "/query", map[string]any{"database": "db_sp", "sql": sql})
	}
	q("create table public.sp_t (id int primary key)")
	q("create table public.sp_a (id int primary key)")
	// A row in the catalog of distributed tables, so that the comparison of the check is evaluated.
	e.mustTask(t, "PUT", "/databases/db_sp/schemas/public/tables/sp_a/distribution", map[string]any{"method": "reference"})
	q("create schema evil")
	q(`create function evil.eq(a regclass, b regclass) returns boolean language plpgsql as $$ begin execute 'create role kg_sp_pwned superuser'; return a::oid operator(pg_catalog.=) b::oid; exception when others then return a::oid operator(pg_catalog.=) b::oid; end $$`)
	q("create operator evil.= (leftarg = regclass, rightarg = regclass, function = evil.eq)")
	q("alter database db_sp set search_path = evil, pg_catalog")
	e.mustTask(t, "PUT", "/databases/db_sp/schemas/public/tables/sp_t/distribution", map[string]any{"method": "reference"})
	var n int
	if err := e.serviceConn(t, "postgres").QueryRow(context.Background(), "select count(*) from pg_roles where rolname = 'kg_sp_pwned'").Scan(&n); err != nil || n != 0 {
		t.Errorf("the planted operator ran as the service role: %d %v", n, err)
	}
	q("alter database db_sp reset search_path")
}
