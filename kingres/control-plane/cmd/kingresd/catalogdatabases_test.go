// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/importdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The catalog follow-up of a failover and the registration of the standbys visit the databases that catalogDatabases lists, and the private database of an import must
// not be one of them (the failover of the map group would never end). The test runs the function itself on a real instance, with databases that each have a table
// kingres.node: the private database that the service role made and marked is left out, and so is nothing else (the one that a role with CREATEDB made under the same
// name and marker, the one that has the prefix and no marker, an ordinary one). A mutant that lists every database fails it.
func TestCatalogDatabasesLeavesOutThePrivateDatabaseOfAnImport(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	c := in.Connect(t, "postgres")
	priv := importdb.Prefix + "31_0123456789ab"
	forged := importdb.Prefix + "32_0123456789ab"
	unmarked := importdb.Prefix + "33_0123456789ab"
	dbs := []string{sysdb.Database, priv, forged, unmarked, "shop"}
	for _, q := range []string{
		"create role tenant login createdb",
		"create database " + sysdb.Database, "create database " + priv, "create database " + forged + " owner tenant", "create database " + unmarked, "create database shop",
		"comment on database " + priv + " is '" + importdb.Marker(priv) + "'",
		"comment on database " + forged + " is '" + importdb.Marker(forged) + "'",
	} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, db := range dbs {
		d := in.Connect(t, db)
		for _, q := range []string{"create schema kingres", "create table kingres.node (node_id int)"} {
			if _, err := d.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	conn := func(ctx context.Context, db string) (*pgx.Conn, error) { return pgx.Connect(ctx, in.DSN(db)) }
	has := func(ctx context.Context, c *pgx.Conn, _ string) (bool, error) {
		var h bool
		err := c.QueryRow(ctx, `SELECT to_regclass('kingres.node') IS NOT NULL`).Scan(&h)
		return h, err
	}
	got, err := catalogDatabasesOf(ctx, conn, has, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] != sysdb.Database {
		t.Fatalf("the system database is not first: %v", got)
	}
	want := []string{forged, unmarked, "shop"}
	rest := append([]string(nil), got[1:]...)
	sort.Strings(rest)
	sort.Strings(want)
	if !reflect.DeepEqual(rest, want) {
		t.Fatalf("catalogDatabases = %v, want the system database first and then %v", got, want)
	}
	// without the system database (the registration of the standbys)
	got, err = catalogDatabasesOf(ctx, conn, has, false)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalogDatabases without the system database = %v, want %v", got, want)
	}
}
