// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package auth

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The connections of the authenticator fix the search path in their startup parameters: a database owner can run ALTER DATABASE ... SET search_path, and a
// setting of the database never outranks a startup parameter.
func TestConnectionConfigPinsTheSearchPath(t *testing.T) {
	p, err := NewPostgres("host=127.0.0.1 port=1 dbname=kingres_system user=x search_path=evil", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.config().RuntimeParams["search_path"]; got != "pg_catalog" {
		t.Fatalf("NewPostgres: search_path %q", got)
	}
	if err := p.SetDSN("host=127.0.0.1 port=2 dbname=kingres_system options='-c search_path=evil'"); err != nil {
		t.Fatal(err)
	}
	if got := p.config().RuntimeParams["search_path"]; got != "pg_catalog" {
		t.Fatalf("SetDSN: search_path %q", got)
	}
}

// A role without CONNECT on the system database (it is in no cluster role) is answered like a wrong password, not as an unavailable server.
func TestNoConnectPrivilegeIsInvalidCredentials(t *testing.T) {
	err := classifyConnectError(&pgconn.PgError{Code: "42501", Message: "permission denied for database kingres_system"})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("%v", err)
	}
	if err := classifyConnectError(&pgconn.PgError{Code: "53300"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("too many connections stays unavailable: %v", err)
	}
}
