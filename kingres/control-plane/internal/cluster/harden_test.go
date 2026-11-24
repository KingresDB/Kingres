// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cluster

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// The system database after HardenSystemDatabase: only the cluster roles can connect (so a console sign-in, which connects as the signing-in role, still
// works for them), nobody else can create in the schema public, and extguard finds nothing wrong with it. A second run changes nothing.
func TestHardenSystemDatabase(t *testing.T) {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	admin := in.Connect(t, "postgres")
	for _, s := range []string{
		"CREATE ROLE kingres_admin NOLOGIN", "CREATE ROLE kingres_ops NOLOGIN", "CREATE ROLE kingres_readonly NOLOGIN", // kingres_stats is left out: the roles that exist are the ones granted
		"CREATE ROLE alice LOGIN PASSWORD 'pw-a' IN ROLE kingres_admin",
		"CREATE ROLE bob LOGIN PASSWORD 'pw-b' IN ROLE kingres_readonly",
		"CREATE ROLE mallory LOGIN PASSWORD 'pw-m'",
		"CREATE DATABASE kingres_system",
	} {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	connect := func(user, pw string) error {
		cfg, err := pgx.ParseConfig(in.DSN("kingres_system"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.User, cfg.Password = user, pw
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err == nil {
			_ = c.Close(ctx)
		}
		return err
	}
	if err := connect("mallory", "pw-m"); err != nil {
		t.Fatalf("before the hardening PUBLIC can connect: %v", err)
	}
	sys := in.Connect(t, "kingres_system")
	for i := 0; i < 2; i++ {
		if err := HardenSystemDatabase(ctx, sys); err != nil {
			t.Fatal(err)
		}
	}
	for user, pw := range map[string]string{"alice": "pw-a", "bob": "pw-b"} {
		if err := connect(user, pw); err != nil {
			t.Errorf("%s (a member of a cluster role) must connect: %v", user, err)
		}
	}
	err := connect("mallory", "pw-m")
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe == nil || pe.Code != "42501" {
		t.Errorf("a role without a cluster role cannot connect: %v", err)
	}
	var can bool
	if err := sys.QueryRow(ctx, "SELECT has_schema_privilege('mallory', 'public', 'CREATE') OR has_database_privilege('mallory', 'kingres_system', 'TEMP')").Scan(&can); err != nil || can {
		t.Errorf("PUBLIC keeps a privilege: %v %v", can, err)
	}
	if probs, err := extguard.SystemDatabaseProblems(ctx, sys); err != nil || len(probs) != 0 {
		t.Errorf("%v %v", probs, err)
	}
}
