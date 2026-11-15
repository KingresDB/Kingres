// SPDX-License-Identifier: PostgreSQL

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
	for i, m := range ms {
		if m.Version != i+1 {
			t.Errorf("migration %d has version %d", i, m.Version)
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
	v, err := sysdb.Migrate(ctx, c)
	if err != nil || v != 4 {
		t.Fatalf("first run: %d %v", v, err)
	}
	for _, tbl := range []string{"task", "database_setting", "cluster_setting", "server", "certificate", "server_group", "alert", "network_rule", "schema_migration"} {
		if !scalar[bool](t, c, "SELECT to_regclass($1) IS NOT NULL", "kingres_cp."+tbl) {
			t.Errorf("table %s missing", tbl)
		}
	}
	v, err = sysdb.Migrate(ctx, c)
	if err != nil || v != 4 {
		t.Fatalf("second run: %d %v", v, err)
	}
	if n := scalar[int](t, c, "SELECT count(*) FROM kingres_cp.schema_migration"); n != 4 {
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

func TestBinaryOlderThanSchemaRefuses(t *testing.T) {
	c := newDB(t)
	ctx := context.Background()
	if _, err := sysdb.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	// Pretend a newer binary applied migration 5.
	if _, err := c.Exec(ctx, "INSERT INTO kingres_cp.schema_migration (version, name) VALUES (5, '0005_future.sql')"); err != nil {
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
		"gap":      {"m/0001_a.sql": {Data: []byte("x")}, "m/0003_c.sql": {Data: []byte("x")}},
		"badname":  {"m/a.sql": {Data: []byte("x")}},
		"zero":     {"m/0000_a.sql": {Data: []byte("x")}},
		"notstart": {"m/0002_a.sql": {Data: []byte("x")}},
	}
	for name, fsys := range cases {
		if _, err := sysdb.LoadForTest(fsys, "m"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if ms, err := sysdb.LoadForTest(fstest.MapFS{"m/0002_b.sql": {Data: []byte("x")}, "m/0001_a.sql": {Data: []byte("y")}}, "m"); err != nil || len(ms) != 2 || ms[0].Name != "0001_a.sql" {
		t.Errorf("ordering: %v %v", ms, err)
	}
}
