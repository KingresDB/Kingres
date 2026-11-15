// SPDX-License-Identifier: AGPL-3.0-only

// Package sysdb creates and upgrades the schema kingres_cp of the system
// database kingres_system with versioned SQL migrations embedded in the
// binary (design: control-plane-2.md, D2).
//
// Rules: every migration runs in one transaction together with its record in
// kingres_cp.schema_migration, so a failed migration leaves nothing behind;
// migrations are applied in version order exactly once; a binary that knows
// fewer migrations than the database has applied refuses to run (it would
// not understand the schema).
package sysdb

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Schema is the PostgreSQL schema that holds the control-plane tables.
const Schema = "kingres_cp"

// Database is the name of the system database.
const Database = "kingres_system"

// advisoryLockKey serializes concurrent migrators (two kingresd processes).
const advisoryLockKey int64 = 0x4b494e4752455344 // "KINGRESD" in ASCII

//go:embed migrations/*.sql
var embedded embed.FS

// ErrSchemaTooNew means the database was migrated by a newer binary.
var ErrSchemaTooNew = errors.New("the system database schema is newer than this kingresd")

// Migration is one numbered schema change.
type Migration struct {
	// Version is the migration number, starting at 1 without gaps.
	Version int
	// Name is the file name without directory.
	Name string
	// SQL is the script; it must not contain transaction control.
	SQL string
}

// Embedded returns the migrations compiled into the binary, ordered.
func Embedded() ([]Migration, error) { return load(embedded, "migrations") }

func load(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v < 1 {
			return nil, fmt.Errorf("sysdb: migration file %q must be named <number>_<name>.sql", name)
		}
		b, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: name, SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("sysdb: migration versions must be 1..n without gaps (found %d at position %d)", m.Version, i+1)
		}
	}
	return out, nil
}

// Migrate applies the pending embedded migrations on conn, which must be
// connected to the system database as a role that may create the schema.
// It returns the schema version afterwards.
func Migrate(ctx context.Context, conn *pgx.Conn) (int, error) {
	ms, err := Embedded()
	if err != nil {
		return 0, err
	}
	return Run(ctx, conn, ms)
}

// Run applies migrations (a list that starts at version 1 without gaps).
func Run(ctx context.Context, conn *pgx.Conn, ms []Migration) (int, error) {
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return 0, fmt.Errorf("sysdb: taking the migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
	}()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+Schema); err != nil {
		return 0, fmt.Errorf("sysdb: creating schema: %w", err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+Schema+`.schema_migration (
		version int PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return 0, fmt.Errorf("sysdb: creating schema_migration: %w", err)
	}
	cur, err := currentVersion(ctx, conn)
	if err != nil {
		return 0, err
	}
	if cur > len(ms) {
		return cur, fmt.Errorf("%w (database at version %d, binary knows %d)", ErrSchemaTooNew, cur, len(ms))
	}
	for _, m := range ms[cur:] {
		if err := apply(ctx, conn, m); err != nil {
			return cur, fmt.Errorf("sysdb: migration %s failed: %w", m.Name, err)
		}
		cur = m.Version
	}
	return cur, nil
}

// CheckVersion refuses a database whose schema is newer than the binary.
func CheckVersion(ctx context.Context, conn *pgx.Conn) error {
	ms, err := Embedded()
	if err != nil {
		return err
	}
	cur, err := currentVersion(ctx, conn)
	if err != nil {
		return err
	}
	if cur > len(ms) {
		return fmt.Errorf("%w (database at version %d, binary knows %d)", ErrSchemaTooNew, cur, len(ms))
	}
	return nil
}

func currentVersion(ctx context.Context, conn *pgx.Conn) (int, error) {
	var v int
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM `+Schema+`.schema_migration`).Scan(&v); err != nil {
		return 0, fmt.Errorf("sysdb: reading the schema version: %w", err)
	}
	return v, nil
}

func apply(ctx context.Context, conn *pgx.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	// Rollback after a successful Commit is a no-op.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// The scripts are trusted, embedded text with several statements, so they
	// run with the simple protocol.
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = "+Schema+", pg_catalog"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, m.SQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+Schema+`.schema_migration (version, name) VALUES ($1, $2)`, m.Version, m.Name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
