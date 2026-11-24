// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/spooftest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// TestSchemaSpoofIsNeverExecutedByFailover: a failover reads and calls the catalog (kingres.node, kingres.promote_node, kingres.check_cluster) of every database as
// the service role (a superuser). A database whose schema kingres is a tenant's is not one that has a catalog, and no call is made in it.
func TestSchemaSpoofIsNeverExecutedByFailover(t *testing.T) {
	env := spooftest.New(t)
	ctx := context.Background()
	c, err := env.ServiceConn(ctx, spooftest.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	if has, err := hasCatalog(ctx, c, spooftest.Database); err != nil || has {
		t.Fatalf("hasCatalog: %v %v", has, err)
	}
	env.NoMarkers(t, "hasCatalog")
	err = requireCatalog(ctx, c, spooftest.Database)
	if p, ok := err.(*problem.Problem); !ok || p.Code != problem.ExtensionMissing {
		t.Fatalf("requireCatalog: %v", err)
	}
	env.NoMarkers(t, "requireCatalog")
	// the system database is the service role's own and is not asked
	sc, err := env.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sc.Close(ctx) }()
	if err := requireCatalog(ctx, sc, sysdb.Database); err != nil {
		t.Fatalf("requireCatalog on the system database: %v", err)
	}
}

func TestIsUntrustedErr(t *testing.T) {
	if !isUntrustedErr(problem.New(problem.ExtensionUnavailable, "x")) || isUntrustedErr(problem.New(problem.ExtensionMissing, "x")) || isUntrustedErr(nil) {
		t.Fatal("isUntrustedErr does not tell the refusal of an untrusted schema from other errors")
	}
}
