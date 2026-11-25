// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package importdb is the one rule that says which database is the private database of a running import, and the one list of the databases that a walk over the
// databases of the cluster (the failover and the registration of the standbys in the catalogs, the weight and the drain of a server, the rebalance) visits.
//
// A database is the private database of an import only when all three of these hold: its name starts with Prefix, its comment is the marker that is made from its
// name (Marker), and its owner is the service role, the role of the connection that asks (the role that makes the database and the only one that may drop it). The
// name and the marker are made from the id of the task and a nonce that is in the name, so a role with CREATEDB can write both; it cannot be the owner.
package importdb

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Prefix is the start of the name of the database that a task works in; the rest is the id of the task and the nonce.
const Prefix = "kingres_import_"

// Marker is the comment that marks a private database as the one that a task made: its name already says which task it is.
func Marker(priv string) string {
	return "kingres-import:" + strings.TrimPrefix(priv, Prefix)
}

// IsPrivate says whether a database is the private database of an import: the name, the marker as its comment and the owner (ownedByService: the owner is the
// service role) all match. A database that a client made under the prefix, or that carries the comment of a task under another name, is a database like any other.
func IsPrivate(name, comment string, ownedByService bool) bool {
	return ownedByService && strings.HasPrefix(name, Prefix) && comment == Marker(name)
}

// ListSQL lists, for every database that is not a template and accepts connections, its name, its comment and whether the connection's role owns it.
const ListSQL = `SELECT d.datname::pg_catalog.text, coalesce(pg_catalog.shobj_description(d.oid, 'pg_database'), ''), pg_catalog.pg_get_userbyid(d.datdba) = current_user
	FROM pg_catalog.pg_database d WHERE NOT d.datistemplate AND d.datallowconn ORDER BY d.datname`

// List returns the databases that a walk visits, in the order of their names: every database that is not a template and accepts connections, except the private
// databases of running imports. c must be the connection of the service role.
func List(ctx context.Context, c *pgx.Conn) ([]string, error) {
	rows, err := c.Query(ctx, ListSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n, comment string
		var mine bool
		if err := rows.Scan(&n, &comment, &mine); err != nil {
			return nil, err
		}
		if IsPrivate(n, comment, mine) {
			continue
		}
		names = append(names, n)
	}
	return names, rows.Err()
}
