// SPDX-License-Identifier: AGPL-3.0-only

package membership

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extgate"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// catalogDB is the database whose kingres.node table is read for the
// membership facts: every server holds a copy of the catalog in it.
const catalogDB = sysdb.Database

// holds lists, per sharded database, the number of buckets the server owns: the
// buckets of the shard groups whose primary it runs. A standby it hosts for another
// group owns nothing a rebalance could move, and the standbys of a removed server
// are removed with it (standby.Leader.PurgeServer), so they are not counted.
func (m *Manager) holds(ctx context.Context, server string) ([]bucketHold, error) {
	dbs, err := m.shardedDatabases(ctx)
	if err != nil {
		return nil, err
	}
	var out []bucketHold
	for _, db := range dbs {
		c, err := m.Svc.ServiceConn(ctx, db)
		if err != nil {
			return nil, err
		}
		n, err := bucketsOwned(ctx, c, db, server)
		_ = c.Close(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, bucketHold{DB: db, Buckets: n})
		}
	}
	return out, nil
}

// catalogFacts reads what the local copy of the catalog says about a server.
func (m *Manager) catalogFacts(ctx context.Context, id string) (removalFacts, error) {
	var f removalFacts
	c, err := m.Svc.ServiceConn(ctx, catalogDB)
	if err != nil {
		return f, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, `SELECT group_id, is_active, role FROM kingres.node WHERE host = $1`, id)
	if err != nil {
		return f, err
	}
	nodes, active := 0, 0
	for rows.Next() {
		var g int32
		var up bool
		var role string
		if err := rows.Scan(&g, &up, &role); err != nil {
			rows.Close()
			return f, err
		}
		nodes++
		if up {
			active++
		}
		// a standby of the map group does not make its server the manager leader
		if g == 0 && role == "primary" {
			f.HostsMapGroup = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return f, err
	}
	f.Known = nodes > 0
	f.Dead = nodes > 0 && active == 0
	if err := c.QueryRow(ctx, `SELECT count(DISTINCT host) FROM kingres.node`).Scan(&f.Servers); err != nil {
		return f, err
	}
	f.Holds, err = m.holds(ctx, id)
	return f, err
}

// leaderFacts is catalogFacts plus what only the leader knows: the server
// record (a server that is registered but has no catalog entry yet is a
// member too) and the unfinished tasks.
func (m *Manager) leaderFacts(ctx context.Context, id string) (removalFacts, error) {
	f, err := m.catalogFacts(ctx, id)
	if err != nil {
		return f, err
	}
	if row, rerr := m.getServer(ctx, id); rerr == nil {
		if row.Status != StatusPending {
			f.Known = true
		}
	} else if !errors.Is(rerr, errNoServer) {
		return f, rerr
	}
	err = m.sys(ctx, func(c *pgx.Conn) error {
		if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres_cp.task
			WHERE kind = 'rebalance' AND state IN ('pending', 'running'))`).Scan(&f.RebalanceRunning); err != nil {
			return err
		}
		return c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres_cp.task
			WHERE subject_type = 'server' AND subject_id = $1 AND state IN ('pending', 'running', 'paused'))`, id).Scan(&f.OperationRunning)
	})
	return f, err
}

// InstanceBelongsTo tells whether the instance with this id in the shard catalog
// is one of the server's (what a peer that records the task of an instance
// must prove, CP2-20).
func (m *Manager) InstanceBelongsTo(ctx context.Context, instanceID, server string) (bool, error) {
	id, err := strconv.ParseInt(instanceID, 10, 32)
	if err != nil {
		return false, nil
	}
	c, err := m.Svc.ServiceConn(ctx, catalogDB)
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var own bool
	err = c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.node WHERE node_id = $1 AND host = $2)`, id, server).Scan(&own)
	return own, err
}

// bucketsOwned counts the buckets of the shard groups whose primary the server runs, in the catalog of database db that c is open to. The count is read as a
// superuser from the schema kingres of a user database: it must be the extension's (the owner of the database could have made a schema of its own), or the
// removal of a server is refused.
func bucketsOwned(ctx context.Context, c *pgx.Conn, db, server string) (int, error) {
	if err := extgate.Require(ctx, c, extguard.Shard, db); err != nil {
		return 0, err
	}
	var n int
	err := c.QueryRow(ctx, `SELECT count(*) FROM kingres.bucket_map b
		WHERE b.group_id IN (SELECT n.group_id FROM kingres.node n WHERE n.host = $1 AND n.role = 'primary')`, server).Scan(&n)
	return n, err
}
