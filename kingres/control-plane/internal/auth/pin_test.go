// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package auth

import "testing"

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
