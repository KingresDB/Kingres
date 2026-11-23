// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/spooftest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// TestSchemaSpoofIsNeverExecutedByStandby: the registrar records a standby in the catalog of every database of the clients (kingres.node, kingres.add_node) as the
// service role (a superuser). The schema kingres of a tenant is never read or called: the registration in that database is refused and nothing of the tenant runs.
func TestSchemaSpoofIsNeverExecutedByStandby(t *testing.T) {
	env := spooftest.New(t)
	ctx := context.Background()
	r := &PGRegistrar{
		Conn:      func(ctx context.Context) (*pgx.Conn, error) { return env.ServiceConn(ctx, sysdb.Database) },
		Databases: func(context.Context) ([]string, error) { return []string{spooftest.Database}, nil },
		DBConn:    env.ServiceConn,
	}
	e := Entry{ID: 1, Group: 1, Server: "10.0.0.2", PrimaryServer: "10.0.0.1", PrimaryPort: 5432, Port: 5433}
	err := r.EnsureEverywhere(ctx, e)
	var p *problem.Problem
	if err == nil || !errors.As(err, &p) || p.Code != problem.ExtensionMissing {
		t.Fatalf("EnsureEverywhere in a database whose schema kingres is the tenant's: %v", err)
	}
	env.NoMarkers(t, "EnsureEverywhere")
	e.CatalogNode = 7
	err = r.in(spooftest.Database).unregister(ctx, e)
	if err == nil || !errors.As(err, &p) || p.Code != problem.ExtensionMissing {
		t.Fatalf("unregister in a database whose schema kingres is the tenant's: %v", err)
	}
	env.NoMarkers(t, "unregister")
}
