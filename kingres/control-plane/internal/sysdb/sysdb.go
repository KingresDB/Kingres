// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package sysdb creates and upgrades the schema kingres_cp of the system
// database kingres_system with versioned SQL migrations embedded in the
// binary (design: control-plane-2.md, D2).
//
// Rules: every migration runs in one transaction together with its record in
// kingres_cp.schema_migration, so a failed migration leaves nothing behind;
// every migration the database has not recorded is applied, in version order,
// exactly once (version numbers are unique and increasing but may have gaps,
// so that two lines of work can reserve their own ranges: a migration whose
// number lies below one that is already applied is still applied when it is
// missing); a binary that does not know a migration the database has applied
// refuses to run (it would not understand the schema).
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
	// Version is the migration number: unique and increasing, and it may have gaps (see the rules above).
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
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("sysdb: two migration files have the version %d (%s and %s)", out[i].Version, out[i-1].Name, out[i].Name)
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

// Run applies the migrations the database has not recorded, in version order (a list of unique,
// increasing versions; gaps are allowed). It returns the highest version applied.
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
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return 0, err
	}
	cur := maxVersion(applied)
	if unknown := unknownVersion(applied, ms); unknown != 0 {
		return cur, fmt.Errorf("%w (database has applied version %d, binary does not know it)", ErrSchemaTooNew, unknown)
	}
	for _, m := range ms {
		if applied[m.Version] {
			continue
		}
		if err := apply(ctx, conn, m); err != nil {
			return cur, fmt.Errorf("sysdb: migration %s failed: %w", m.Name, err)
		}
		if m.Version > cur {
			cur = m.Version
		}
	}
	return cur, nil
}

// CheckVersion refuses a database whose schema is newer than the binary.
func CheckVersion(ctx context.Context, conn *pgx.Conn) error {
	ms, err := Embedded()
	if err != nil {
		return err
	}
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}
	if unknown := unknownVersion(applied, ms); unknown != 0 {
		return fmt.Errorf("%w (database has applied version %d, binary does not know it)", ErrSchemaTooNew, unknown)
	}
	return nil
}

// appliedVersions is the set of versions recorded in schema_migration.
func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM `+Schema+`.schema_migration`)
	if err != nil {
		return nil, fmt.Errorf("sysdb: reading the schema version: %w", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("sysdb: reading the schema version: %w", err)
		}
		out[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sysdb: reading the schema version: %w", err)
	}
	return out, nil
}

func maxVersion(applied map[int]bool) int {
	m := 0
	for v := range applied {
		if v > m {
			m = v
		}
	}
	return m
}

// unknownVersion returns the highest applied version that no embedded migration has, 0 when none.
func unknownVersion(applied map[int]bool, ms []Migration) int {
	known := map[int]bool{}
	for _, m := range ms {
		known[m.Version] = true
	}
	u := 0
	for v := range applied {
		if !known[v] && v > u {
			u = v
		}
	}
	return u
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
