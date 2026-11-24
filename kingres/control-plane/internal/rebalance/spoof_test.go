// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package rebalance

import (
	"context"
	"errors"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/spooftest"
)

func wantProblem(t *testing.T, what string, err error, code problem.Code) {
	t.Helper()
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != code {
		t.Fatalf("%s: %v, want problem %s", what, err, code)
	}
}

// TestSchemaSpoofIsNeverExecutedByRebalance: the rebalance reads and calls kingres.* in every database as the service role (a superuser). A database that a tenant
// owns, with no extension and a schema kingres of the tenant, is never read: it is not a database with distributed tables, a plan or a check of it is refused, and
// none of the views and functions of the tenant runs.
func TestSchemaSpoofIsNeverExecutedByRebalance(t *testing.T) {
	env := spooftest.New(t)
	ctx := context.Background()
	db := spooftest.Database
	b := &PG{Conns: env}

	dbs, err := b.Databases(ctx, Options{})
	if err != nil {
		t.Fatalf("Databases: %v", err)
	}
	for _, d := range dbs {
		if d == db {
			t.Fatalf("the database of the tenant is listed as one with distributed tables")
		}
	}
	env.NoMarkers(t, "Databases")
	if dbs, err = b.Databases(ctx, Options{Databases: []string{db}}); err != nil || len(dbs) != 0 {
		t.Fatalf("Databases that names the database of the tenant: %v %v", dbs, err)
	}
	env.NoMarkers(t, "Databases (named)")

	wantProblem(t, "Check", b.Check(ctx, []string{db}), problem.ExtensionMissing)
	env.NoMarkers(t, "Check")
	_, err = b.Plan(ctx, []string{db}, Options{})
	wantProblem(t, "Plan", err, problem.ExtensionMissing)
	env.NoMarkers(t, "Plan")
	_, err = b.Loads(ctx, []string{db}, Options{})
	wantProblem(t, "Loads", err, problem.ExtensionMissing)
	env.NoMarkers(t, "Loads")
	_, err = b.Placement(ctx, []string{db})
	wantProblem(t, "Placement", err, problem.ExtensionMissing)
	env.NoMarkers(t, "Placement")
	if h := b.ServerOf(ctx, db, 1); h != "" {
		t.Fatalf("ServerOf answered %q from the schema of the tenant", h)
	}
	env.NoMarkers(t, "ServerOf")
	m := Move{Database: db, Colocation: 1, Bucket: 0, From: 1, To: 2}
	wantProblem(t, "CheckMove", b.CheckMove(ctx, m), problem.ExtensionMissing)
	env.NoMarkers(t, "CheckMove")
	_, err = b.Mark(ctx, m)
	wantProblem(t, "Mark", err, problem.ExtensionMissing)
	env.NoMarkers(t, "Mark")
	_, _, err = b.Recover(ctx, MoveRow{Move: m})
	wantProblem(t, "Recover", err, problem.ExtensionMissing)
	env.NoMarkers(t, "Recover")

	// the drain preview and the split preview ask the same databases
	s := &Service{Conns: env, Backend: b}
	if n, err := s.bucketsOn(ctx, "10.0.0.1"); err != nil || n != 0 {
		t.Fatalf("bucketsOn: %d %v", n, err)
	}
	if got := s.localData(ctx, "10.0.0.1"); len(got) != 0 {
		t.Fatalf("localData: %+v", got)
	}
	_, err = s.SplitPreview(ctx, db, 1, 0, SplitRequest{})
	if err == nil {
		t.Fatalf("SplitPreview of the database of the tenant was not refused")
	}
	env.NoMarkers(t, "the drain and split previews")
}

// TestUntrustedSchemaIsRefusedByRebalance: kingres_shard is installed in a schema kingres that the tenant owns. A rebalance that does not name the database leaves
// it out (one tenant cannot stop the rebalance of the others); one that names it, and every call on it, is refused.
func TestUntrustedSchemaIsRefusedByRebalance(t *testing.T) {
	env := spooftest.NewUntrusted(t)
	ctx := context.Background()
	db := spooftest.Database
	b := &PG{Conns: env}
	dbs, err := b.Databases(ctx, Options{})
	if err != nil {
		t.Fatalf("Databases: %v", err)
	}
	for _, d := range dbs {
		if d == db {
			t.Fatal("the database of the tenant is planned")
		}
	}
	_, err = b.Databases(ctx, Options{Databases: []string{db}})
	wantProblem(t, "Databases that names the database", err, problem.ExtensionUnavailable)
	wantProblem(t, "Check", b.Check(ctx, []string{db}), problem.ExtensionUnavailable)
	_, err = b.Plan(ctx, []string{db}, Options{})
	wantProblem(t, "Plan", err, problem.ExtensionUnavailable)
	_, err = b.Placement(ctx, []string{db})
	wantProblem(t, "Placement", err, problem.ExtensionUnavailable)
	env.NoMarkers(t, "rebalance")
}
