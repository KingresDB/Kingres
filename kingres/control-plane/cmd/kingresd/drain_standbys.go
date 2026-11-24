// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package main

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/rebalance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/standby"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// drainExcludedKey is the cluster setting that lists the servers a drain has taken out of the ring of the standbys. It is
// cleared when the server is given back to service (ops.Ops.UpdateServer with shouldHaveShards true).
const drainExcludedKey = ops.DrainExcludedKey

// wireStandbyRelocation connects the drain (step 5 of design 11.3) to the standby reconcile: the draining server leaves the ring,
// the reconcile runs now, and the step counts the standbys that remain on the server.
func wireStandbyRelocation(rbs *rebalance.Service, l *standby.Leader, o *ops.Ops, svc *cluster.Service) {
	read := func(ctx context.Context, c *pgx.Conn) map[string]bool {
		out := map[string]bool{}
		var raw []byte
		if err := c.QueryRow(ctx, `SELECT value FROM kingres_cp.cluster_setting WHERE key = $1`, drainExcludedKey).Scan(&raw); err == nil {
			var list []string
			if json.Unmarshal(raw, &list) == nil {
				for _, s := range list {
					out[s] = true
				}
			}
		}
		return out
	}
	l.Excluded = func(ctx context.Context) map[string]bool {
		c, err := svc.ServiceConn(ctx, sysdb.Database)
		if err != nil {
			return nil
		}
		defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
		return read(ctx, c)
	}
	rbs.RelocateStandbys = func(ctx context.Context, server string) (int, error) {
		c, err := svc.ServiceConn(ctx, sysdb.Database)
		if err != nil {
			return 0, err
		}
		defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
		set := read(ctx, c)
		if !set[server] {
			set[server] = true
			list := make([]string, 0, len(set))
			for s := range set {
				list = append(list, s)
			}
			b, _ := json.Marshal(list)
			if _, err := c.Exec(ctx, `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ($1, $2)
				ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, drainExcludedKey, b); err != nil {
				return 0, err
			}
		}
		if eng := o.Engine(); eng != nil {
			if _, err := l.ReconcileNow(ctx, eng.Get); err != nil {
				return 0, err
			}
		} else {
			l.Kick()
		}
		// What is left to do: the standbys still on the server, and the groups that have fewer standbys on the other servers than the
		// best-placed group (a group that lost its standby with the server waits for its replacement: make before break is the
		// reconcile's business, the step waits for the end of it).
		var n int
		err = c.QueryRow(ctx, `WITH per AS (
				SELECT group_id, count(*) FILTER (WHERE role = 'standby' AND host <> $1) AS c FROM kingres.node GROUP BY group_id)
			SELECT (SELECT count(*) FROM kingres.node WHERE host = $1 AND role = 'standby')::int
			     + (SELECT count(*) FROM per WHERE c < (SELECT max(c) FROM per))::int`, server).Scan(&n)
		return n, err
	}
}
