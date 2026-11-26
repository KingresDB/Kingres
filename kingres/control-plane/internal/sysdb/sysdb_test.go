// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sysdb_test

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

func TestEmbeddedMigrationsAreOrdered(t *testing.T) {
	ms, err := sysdb.Embedded()
	if err != nil || len(ms) == 0 {
		t.Fatalf("embedded migrations: %v %d", err, len(ms))
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].Version <= ms[i-1].Version {
			t.Errorf("migration %d has version %d after %d", i, ms[i].Version, ms[i-1].Version)
		}
	}
}

func scalar[T any](t *testing.T, c *pgx.Conn, q string, args ...any) T {
	t.Helper()
	var v T
	if err := c.QueryRow(context.Background(), q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func newDB(t *testing.T) *pgx.Conn {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	c := in.Connect(t, "postgres")
	if _, err := c.Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	return in.Connect(t, "kingres_system")
}

func TestMigrateIdempotent(t *testing.T) {
	c := newDB(t)
	ctx := context.Background()
	ms, err := sysdb.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	last := ms[len(ms)-1].Version
	v, err := sysdb.Migrate(ctx, c)
	if err != nil || v != last {
		t.Fatalf("first run: %d %v", v, err)
	}
	for _, tbl := range []string{"task", "ledger_digest_actor", "database_setting", "cluster_setting", "server", "certificate", "server_group", "alert", "network_rule", "schema_migration"} {
		if !scalar[bool](t, c, "SELECT to_regclass($1) IS NOT NULL", "kingres_cp."+tbl) {
			t.Errorf("table %s missing", tbl)
		}
	}
	v, err = sysdb.Migrate(ctx, c)
	if err != nil || v != last {
		t.Fatalf("second run: %d %v", v, err)
	}
	if n := scalar[int](t, c, "SELECT count(*) FROM kingres_cp.schema_migration"); n != len(ms) {
		t.Errorf("%d migration rows after two runs", n)
	}
	// The unique index enforces one unfinished task per subject.
	ins := "INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ('x', $1, 'database', 'shop', 'a')"
	if _, err := c.Exec(ctx, ins, "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, ins, "pending"); err == nil {
		t.Error("two active tasks for one subject were accepted")
	}
	if _, err := c.Exec(ctx, ins, "done"); err != nil {
		t.Errorf("a finished task was refused: %v", err)
	}
}

func TestMigrationsApplyInOrderAndFailureLeavesNothing(t *testing.T) {
	c := newDB(t)
	ctx := context.Background()
	good := []sysdb.Migration{
		{Version: 1, Name: "0001_a.sql", SQL: "CREATE TABLE kingres_cp.a (id int)"},
		{Version: 2, Name: "0002_b.sql", SQL: "ALTER TABLE kingres_cp.a ADD COLUMN b int"},
	}
	if v, err := sysdb.Run(ctx, c, good); err != nil || v != 2 {
		t.Fatalf("good run: %d %v", v, err)
	}
	// Migration 3 creates a table and then fails: neither the table nor a
	// record of the migration may remain, and migration 2 stays applied.
	bad := append(append([]sysdb.Migration(nil), good...), sysdb.Migration{
		Version: 3, Name: "0003_bad.sql",
		SQL: "CREATE TABLE kingres_cp.half (id int); INSERT INTO kingres_cp.a (nope) VALUES (1)",
	})
	v, err := sysdb.Run(ctx, c, bad)
	if err == nil || v != 2 {
		t.Fatalf("bad run: %d %v", v, err)
	}
	if scalar[bool](t, c, "SELECT to_regclass('kingres_cp.half') IS NOT NULL") {
		t.Error("half-applied migration left a table behind")
	}
	if n := scalar[int](t, c, "SELECT max(version) FROM kingres_cp.schema_migration"); n != 2 {
		t.Errorf("recorded version %d", n)
	}
	// A later, fixed run continues from version 2.
	bad[2].SQL = "CREATE TABLE kingres_cp.half (id int)"
	if v, err := sysdb.Run(ctx, c, bad); err != nil || v != 3 {
		t.Fatalf("fixed run: %d %v", v, err)
	}
}

// A migration whose number lies below one that is already applied (a gap that a later change fills) is
// applied when it is missing, and nothing is applied twice.
func TestAMigrationInAGapIsAppliedLater(t *testing.T) {
	c := newDB(t)
	ctx := context.Background()
	first := []sysdb.Migration{
		{Version: 1, Name: "0001_a.sql", SQL: "CREATE TABLE kingres_cp.a (id int)"},
		{Version: 30, Name: "0030_c.sql", SQL: "ALTER TABLE kingres_cp.a ADD COLUMN c int"},
	}
	if v, err := sysdb.Run(ctx, c, first); err != nil || v != 30 {
		t.Fatalf("first run: %d %v", v, err)
	}
	both := []sysdb.Migration{first[0], {Version: 15, Name: "0015_b.sql", SQL: "ALTER TABLE kingres_cp.a ADD COLUMN b int"}, first[1]}
	if v, err := sysdb.Run(ctx, c, both); err != nil || v != 30 {
		t.Fatalf("second run: %d %v", v, err)
	}
	if !scalar[bool](t, c, "SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'kingres_cp' AND table_name = 'a' AND column_name = 'b')") {
		t.Error("the migration in the gap was not applied")
	}
	if n := scalar[int](t, c, "SELECT count(*) FROM kingres_cp.schema_migration"); n != 3 {
		t.Errorf("%d migration rows", n)
	}
	if v, err := sysdb.Run(ctx, c, both); err != nil || v != 30 {
		t.Fatalf("third run: %d %v", v, err)
	}
	// A binary that lacks an applied migration refuses.
	if _, err := sysdb.Run(ctx, c, first); !errors.Is(err, sysdb.ErrSchemaTooNew) {
		t.Errorf("a binary without migration 15: %v", err)
	}
}

func TestBinaryOlderThanSchemaRefuses(t *testing.T) {
	c := newDB(t)
	ctx := context.Background()
	if _, err := sysdb.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	// Pretend a newer binary applied a migration this one does not have.
	if _, err := c.Exec(ctx, "INSERT INTO kingres_cp.schema_migration (version, name) VALUES (9999, '9999_future.sql')"); err != nil {
		t.Fatal(err)
	}
	if _, err := sysdb.Migrate(ctx, c); !errors.Is(err, sysdb.ErrSchemaTooNew) {
		t.Errorf("Migrate: %v", err)
	}
	if err := sysdb.CheckVersion(ctx, c); !errors.Is(err, sysdb.ErrSchemaTooNew) {
		t.Errorf("CheckVersion: %v", err)
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	// Exercised through the exported helper in export_test.go.
	cases := map[string]fstest.MapFS{
		"badname":   {"m/a.sql": {Data: []byte("x")}},
		"zero":      {"m/0000_a.sql": {Data: []byte("x")}},
		"duplicate": {"m/0002_a.sql": {Data: []byte("x")}, "m/0002_b.sql": {Data: []byte("x")}},
	}
	for name, fsys := range cases {
		if _, err := sysdb.LoadForTest(fsys, "m"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A gap and a first version above 1 are allowed (two lines of work reserve their own ranges).
	if ms, err := sysdb.LoadForTest(fstest.MapFS{"m/0001_a.sql": {Data: []byte("x")}, "m/0030_c.sql": {Data: []byte("x")}}, "m"); err != nil || len(ms) != 2 || ms[1].Version != 30 {
		t.Errorf("gap: %v %v", ms, err)
	}
	if ms, err := sysdb.LoadForTest(fstest.MapFS{"m/0002_b.sql": {Data: []byte("x")}, "m/0001_a.sql": {Data: []byte("y")}}, "m"); err != nil || len(ms) != 2 || ms[0].Name != "0001_a.sql" {
		t.Errorf("ordering: %v %v", ms, err)
	}
}
