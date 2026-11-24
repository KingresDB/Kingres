// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extgate"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// isUntrustedErr reports whether err is the refusal of extgate for a schema that cannot be trusted (problem extension_unavailable).
func isUntrustedErr(err error) bool {
	var p *problem.Problem
	return errors.As(err, &p) && p.Code == problem.ExtensionUnavailable
}

// requireCatalog is the gate of a session of the service role to database db before the catalog (kingres.*) is read or called in it: the catalog of a database of a
// client is read as a superuser, so its schema must be the extension's. The system database is the service role's own and is not asked.
func requireCatalog(ctx context.Context, c *pgx.Conn, db string) error {
	if db == sysdb.Database {
		return nil
	}
	return extgate.Require(ctx, c, extguard.Shard, db)
}

// hasCatalog tells whether database db has a sharding catalog. A schema kingres that extguard does not accept is not a catalog: the database is left out (the
// error is the refusal, for the caller to log), so that one client cannot stop the failover of the others; the monitor raises the alert and the calls on the
// database refuse (requireCatalog).
func hasCatalog(ctx context.Context, c *pgx.Conn, db string) (bool, error) {
	has, err := extgate.Usable(ctx, c, extguard.Shard, db)
	if err != nil || !has {
		return false, err
	}
	err = c.QueryRow(ctx, `SELECT to_regclass('kingres.node') IS NOT NULL`).Scan(&has)
	return has, err
}
