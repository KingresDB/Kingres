// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cluster

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSafeSessionPinsTheSearchPath(t *testing.T) {
	cfg, err := pgx.ParseConfig("host=127.0.0.1 port=1 dbname=x user=y search_path=evil")
	if err != nil {
		t.Fatal(err)
	}
	SafeSession(cfg)
	if got := cfg.RuntimeParams["search_path"]; got != "pg_catalog" {
		t.Fatalf("search_path %q", got)
	}
	empty := &pgx.ConnConfig{}
	SafeSession(empty) // a config without a map of parameters
	if empty.RuntimeParams["search_path"] != "pg_catalog" {
		t.Fatal("a config without RuntimeParams")
	}
	pc, err := pgxpool.ParseConfig("host=127.0.0.1 port=1 dbname=x user=y")
	if err != nil {
		t.Fatal(err)
	}
	SafeSession(pc.ConnConfig)
	if pc.ConnConfig.RuntimeParams["search_path"] != "pg_catalog" {
		t.Fatal("a pool")
	}
}
