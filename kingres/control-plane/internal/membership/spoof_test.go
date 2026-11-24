// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package membership

import (
	"context"
	"errors"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/spooftest"
)

// TestSchemaSpoofIsNeverExecutedByMembership: the facts that decide whether a server can be removed, and the registration and removal of its nodes, read and call
// kingres.* in the databases of the clients as the service role (a superuser). The schema kingres of a tenant is never read or called: each call is refused and none
// of the views and functions of the tenant runs.
func TestSchemaSpoofIsNeverExecutedByMembership(t *testing.T) {
	env := spooftest.New(t)
	ctx := context.Background()
	db := spooftest.Database
	c, err := env.ServiceConn(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	want := func(what string, err error) {
		t.Helper()
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != problem.ExtensionMissing {
			t.Fatalf("%s: %v, want problem extension_missing", what, err)
		}
		env.NoMarkers(t, what)
	}
	_, err = bucketsOwned(ctx, c, db, "10.0.0.1")
	want("bucketsOwned", err)
	want("registerNodeOn", registerNodeOn(ctx, c, db, "10.0.0.9", 5432))
	m := &Manager{}
	want("unregisterNodeOn", m.unregisterNodeOn(ctx, c, db, "10.0.0.9"))
}

// TestUntrustedSchemaIsRefusedByMembership: kingres_shard is installed in a schema kingres that the tenant owns; the removal facts are refused.
func TestUntrustedSchemaIsRefusedByMembership(t *testing.T) {
	env := spooftest.NewUntrusted(t)
	ctx := context.Background()
	c, err := env.ServiceConn(ctx, spooftest.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	_, err = bucketsOwned(ctx, c, spooftest.Database, "10.0.0.1")
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != problem.ExtensionUnavailable {
		t.Fatalf("bucketsOwned: %v", err)
	}
	if err := registerNodeOn(ctx, c, spooftest.Database, "10.0.0.9", 5432); !errors.As(err, &p) || p.Code != problem.ExtensionUnavailable {
		t.Fatalf("registerNodeOn: %v", err)
	}
	env.NoMarkers(t, "membership")
}
