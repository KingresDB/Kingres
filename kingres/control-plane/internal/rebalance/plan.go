// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// GroupLoad is one shard group of one database as plan_rebalance sees it (kingres.group_load).
type GroupLoad struct {
	Database string
	Group    int32
	// Node is host:port of the group's primary; Server is its host, the server id.
	Node      string
	Server    string
	Receiving bool
	Weight    float64
	Buckets   int
	Load      float64
	Deviation float64
	Available bool
}

// Loader is what the preview needs beyond the Backend: the load of every group by the strategy. PG implements
// it for an extension of version 0.8; a Backend without it gives a preview without loads.
type Loader interface {
	Loads(ctx context.Context, dbs []string, o Options) ([]GroupLoad, error)
}

// hasPlanRebalance tells whether the database's extension has plan_rebalance (version 0.8).
func hasPlanRebalance(ctx context.Context, c *pgx.Conn) (bool, error) {
	var has bool
	err := c.QueryRow(ctx, `SELECT to_regprocedure('kingres.plan_rebalance(text,double precision,integer,boolean,jsonb,integer[],integer[])') IS NOT NULL`).Scan(&has)
	return has, err
}

// Plan implements Backend: plan_rebalance of the extension on the map group primary of each database, with
// the load of the other databases passed as extra_load (design 6.1 step 2); the older rebalance_plan when the
// extension has no plan_rebalance and the options ask for nothing that only the newer one can do.
func (b *PG) Plan(ctx context.Context, dbs []string, o Options) ([]Move, error) {
	strategy := sqlStrategy(o.Strategy)
	if o.DrainServer != "" {
		o.ExcludeServers = append(append([]string(nil), o.ExcludeServers...), o.DrainServer)
	}
	extra, err := b.extraLoads(ctx, dbs, o)
	if err != nil {
		return nil, err
	}
	var out []Move
	for _, db := range dbs {
		var has bool
		if err := b.withShard(ctx, db, func(c *pgx.Conn) (err error) { has, err = hasPlanRebalance(ctx, c); return }); err != nil {
			return nil, err
		}
		if !has {
			if needsExtension08(o) {
				return nil, problem.New(problem.UnsupportedOp, "The sharding extension of database "+db+" is older than 0.8: it has neither the strategy by disk size, the threshold, nor the drain; update it first.")
			}
			moves, err := b.planLegacy(ctx, []string{db})
			if err != nil {
				return nil, err
			}
			out = append(out, moves...)
			continue
		}
		var excl []int32
		if len(o.ExcludeServers) > 0 {
			excl, err = b.groupsOfServers(ctx, db, o.ExcludeServers)
			if err != nil {
				return nil, err
			}
		}
		var moves []Move
		err = b.withShard(ctx, db, func(c *pgx.Conn) error {
			var threshold any
			if o.Threshold != nil {
				threshold = *o.Threshold
			}
			var extraJSON any
			if e := extra[db]; len(e) > 0 {
				j, _ := json.Marshal(e)
				extraJSON = string(j)
			}
			var exclArg any
			if len(excl) > 0 {
				exclArg = excl
			}
			// the item of a distributed schema is the one bucket of its colocation group (design 9.9); its name comes from dist_schema
			rows, err := c.Query(ctx, `SELECT p.colocation_id, p.bucket, p.source_group, p.target_group, p.cost, p.size_bytes, p.reason, coalesce(ns.nspname::text, '')
				FROM kingres.plan_rebalance($1, $2::float8, NULL, $3, $4::jsonb, $5::int[], NULL) p
				LEFT JOIN kingres.dist_schema s ON s.colocation_id = p.colocation_id
				LEFT JOIN pg_catalog.pg_namespace ns ON ns.oid = s.nsp::oid
				ORDER BY p.seq`,
				strategy, threshold, o.DrainOnly, extraJSON, exclArg)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				m := Move{Database: db}
				if err := rows.Scan(&m.Colocation, &m.Bucket, &m.From, &m.To, &m.Cost, &m.Size, &m.Reason, &m.Schema); err != nil {
					return err
				}
				moves = append(moves, m)
			}
			return rows.Err()
		})
		var pe *pgconn.PgError
		switch {
		case noReceivingGroup(err):
			return nil, problem.New(problem.NoReceivingServer, "No server should hold shards in database "+db+".")
		case errors.As(err, &pe) && pe.Code == "55000" && strings.Contains(pe.Message, "does not answer"):
			return nil, problem.New(problem.ServiceUnavailable, "The size of the buckets cannot be read: "+pe.Message).WithRetryAfter(10)
		case err != nil:
			return nil, err
		}
		out = append(out, moves...)
	}
	return out, nil
}

// groupsOfServers resolves server ids to the groups whose primary runs on them in one database; an id that
// runs no group of the database is an error of the request.
func (b *PG) groupsOfServers(ctx context.Context, db string, servers []string) ([]int32, error) {
	var out []int32
	err := b.withShard(ctx, db, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT DISTINCT group_id FROM kingres.node WHERE role = 'primary' AND is_active AND host = ANY($1) ORDER BY 1`, servers)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[int32])
		return err
	})
	return out, err
}

// extraLoads is, for each database, the load that the other databases of the run put on each group of it
// (design 4.4, extra_load): their group_load, matched to the groups of the database by the primary node
// (host and port), because group numbers belong to one catalog each. It is empty for a run of one database.
func (b *PG) extraLoads(ctx context.Context, dbs []string, o Options) (map[string]map[string]float64, error) {
	if len(dbs) < 2 {
		return nil, nil
	}
	o.ExcludeServers = nil
	loads, err := b.Loads(ctx, dbs, o)
	if err != nil {
		var pe *problem.Problem
		if errors.As(err, &pe) && pe.Code == problem.UnsupportedOp {
			return nil, nil
		}
		return nil, err
	}
	// load per primary node (the server), per database
	perDB := map[string]map[string]float64{}
	for _, l := range loads {
		if perDB[l.Database] == nil {
			perDB[l.Database] = map[string]float64{}
		}
		perDB[l.Database][l.Node] += l.Load
	}
	out := map[string]map[string]float64{}
	for _, db := range dbs {
		groups := map[string]int32{}
		for _, l := range loads {
			if l.Database == db {
				groups[l.Node] = l.Group
			}
		}
		e := map[string]float64{}
		for other, m := range perDB {
			if other == db {
				continue
			}
			for node, v := range m {
				if g, ok := groups[node]; ok {
					e[fmt.Sprint(g)] += v
				}
			}
		}
		out[db] = e
	}
	return out, nil
}

// Loads implements Loader: kingres.group_load for each database.
func (b *PG) Loads(ctx context.Context, dbs []string, o Options) ([]GroupLoad, error) {
	var out []GroupLoad
	for _, db := range dbs {
		err := b.withShard(ctx, db, func(c *pgx.Conn) error {
			var has bool
			if err := c.QueryRow(ctx, `SELECT to_regprocedure('kingres.group_load(text,jsonb,integer[])') IS NOT NULL`).Scan(&has); err != nil {
				return err
			}
			if !has {
				return problem.New(problem.UnsupportedOp, "The sharding extension of database "+db+" is older than 0.8 (no group_load).")
			}
			var excl any
			if len(o.ExcludeServers) > 0 {
				g, err := b.groupsOfServers(ctx, db, o.ExcludeServers)
				if err != nil {
					return err
				}
				excl = g
			}
			rows, err := c.Query(ctx, `SELECT gl.group_id, coalesce(n.host || ':' || n.port, ''), coalesce(n.host, ''), gl.receiving, gl.weight, gl.buckets, gl.load, gl.deviation, gl.available
				FROM kingres.group_load($1, NULL, $2::int[]) gl
				LEFT JOIN kingres.node n ON n.group_id = gl.group_id AND n.role = 'primary' AND n.is_active ORDER BY gl.group_id`, sqlStrategy(o.Strategy), excl)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var l GroupLoad
				var node, host string
				var load int64
				if err := rows.Scan(&l.Group, &node, &host, &l.Receiving, &l.Weight, &l.Buckets, &load, &l.Deviation, &l.Available); err != nil {
					return err
				}
				l.Database, l.Load, l.Node, l.Server = db, float64(load), node, host
				out = append(out, l)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].Group < out[j].Group
	})
	return out, nil
}

// noReceivingGroup tells the refusal of the extension for a database in which no shard group may receive buckets (SQLSTATE 55000). The
// extension words it differently by version and function ("no shard group accepts buckets", "no shard group can receive buckets",
// "no shard group can hold buckets"); all of them are the answer no_receiving_server, not an error of the inspection.
func noReceivingGroup(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55000" {
		return false
	}
	for _, w := range []string{"no shard group accepts", "no shard group can receive", "no shard group can hold"} {
		if strings.Contains(pe.Message, w) {
			return true
		}
	}
	return false
}
