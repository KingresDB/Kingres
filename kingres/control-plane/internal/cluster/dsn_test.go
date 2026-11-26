// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cluster

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
)

// CP-01: a database name taken from a request path must never change the host
// or port of the service connection.
func TestServiceDSNDatabaseNameCannotRedirect(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CredentialsFile), []byte("s3 cret'pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: Config{DataDir: dir, PGPort: 5433}}
	for _, db := range []string{
		"x host=attacker.example port=9999",
		"x' host='attacker.example",
		`x\ host=attacker.example`,
		"x sslmode=disable user=postgres",
	} {
		dsn := s.ServiceDSN(db)
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("%q: %v", db, err)
		}
		if cfg.Host != "127.0.0.1" || cfg.Port != 5433 || cfg.User != "kingresd" || cfg.Database != db || cfg.Password != "s3 cret'pw" {
			t.Fatalf("%q redirected the connection: host %q port %d user %q database %q", db, cfg.Host, cfg.Port, cfg.User, cfg.Database)
		}
	}
	if s.ServiceDSN("a\x00b") != "" {
		t.Fatal("a NUL byte in the database name produced a connection string")
	}
}
