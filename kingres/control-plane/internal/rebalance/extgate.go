// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package rebalance

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extgate"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// The rebalance reads and calls the schema kingres of kingres_shard in every database of the cluster, as the service role (a superuser). The owner of a
// database, or a role with CREATE on it, could make a schema kingres of its own whose views and functions run as that superuser. Every session in a user
// database therefore asks extguard first (internal/extgate gives the refusal): a database whose schema cannot be trusted is not read, not planned and not moved.

// withShard is with for a database of the cluster: the extension must be installed and its schema trusted before f runs. An untrusted schema is a problem
// (extension_unavailable), an absent extension is extension_missing.
func (b *PG) withShard(ctx context.Context, db string, f func(c *pgx.Conn) error) error {
	return b.with(ctx, db, func(c *pgx.Conn) error {
		if err := extgate.Require(ctx, c, extguard.Shard, db); err != nil {
			return err
		}
		return f(c)
	})
}

// isUntrusted reports whether err is the refusal of an untrusted schema.
func isUntrusted(err error) bool {
	var p *problem.Problem
	return errors.As(err, &p) && p.Code == problem.ExtensionUnavailable
}
