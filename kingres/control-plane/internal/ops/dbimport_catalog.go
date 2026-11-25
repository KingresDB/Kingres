// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// A failover leaves out the private database of an import (importdb), so its catalog is not followed: after the failover of a group it still names the old primary
// of that group. The import checks it before it takes the final name, because the database that is renamed is the database of the client from then on, and nothing
// follows its catalog afterwards but the next failover, which then expects the old primary.
//
// A drain, a change of the flag or the weight of a server, the removal of a server and a rebalance are refused while an import runs (the exclusion of the tasks,
// migration 0045, and UpdateServer), so the check is the second line: it finds the changes that those do not see (a failover, a planned switchover, a server that
// joined).

// catalogNode is the part of a row of kingres.node that the check compares. The node ids and the group ids of the private database are its own, so a node is
// known by its host and port.
type catalogNode struct {
	ID     int32
	Host   string
	Port   int32
	Role   string
	Active bool
	Should bool
	Weight *float64
}

func (n catalogNode) addr() string { return fmt.Sprintf("%s:%d", n.Host, n.Port) }

func sameWeight(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func weightText(w *float64) string {
	if w == nil {
		return "none"
	}
	return strconv.FormatFloat(*w, 'g', -1, 64)
}

// catalogDifferences lists, in words, how the private catalog differs from the system catalog (the primary of the map group is its source of truth, as for every
// database): the set of the primaries of the shard groups (a node that is a standby in the system catalog or is not there any more, and a group of the system
// catalog that the private catalog lacks) and, for each node that is a primary in the private catalog (the nodes that hold buckets), its flag should_have_shards,
// its weight and its flag is_active.
func catalogDifferences(private, system []catalogNode) []string {
	sysByAddr := map[string]catalogNode{}
	sysPrimary := map[string]bool{}
	for _, n := range system {
		sysByAddr[n.addr()] = n
		if n.Role == "primary" {
			sysPrimary[n.addr()] = true
		}
	}
	privPrimary := map[string]bool{}
	var out []string
	for _, n := range private {
		if n.Role != "primary" {
			continue
		}
		a := n.addr()
		privPrimary[a] = true
		s, ok := sysByAddr[a]
		if !ok || s.Role != "primary" {
			out = append(out, "the private catalog names "+a+" as a primary and the cluster does not")
			continue
		}
		if n.Should != s.Should {
			out = append(out, fmt.Sprintf("%s has should_have_shards %t (the cluster: %t)", a, n.Should, s.Should))
		}
		if !sameWeight(n.Weight, s.Weight) {
			out = append(out, fmt.Sprintf("%s has weight %s (the cluster: %s)", a, weightText(n.Weight), weightText(s.Weight)))
		}
		if n.Active != s.Active {
			out = append(out, fmt.Sprintf("%s has is_active %t (the cluster: %t)", a, n.Active, s.Active))
		}
	}
	for a := range sysPrimary {
		if !privPrimary[a] {
			out = append(out, "the primary "+a+" of the cluster is not in the private catalog")
		}
	}
	sort.Strings(out)
	return out
}

// readCatalogNodes reads kingres.node; has is false when the database has no such table (a database without sharded tables).
func readCatalogNodes(ctx context.Context, c *pgx.Conn) (nodes []catalogNode, has bool, err error) {
	if err := c.QueryRow(ctx, `SELECT pg_catalog.to_regclass('kingres.node') IS NOT NULL`).Scan(&has); err != nil || !has {
		return nil, false, err
	}
	// the column weight is the one of the extension 0.8 and later; a catalog without it has no weights, in either database
	var hasWeight bool
	if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid = 'kingres.node'::pg_catalog.regclass AND attname = 'weight' AND NOT attisdropped)`).Scan(&hasWeight); err != nil {
		return nil, true, err
	}
	w := "NULL::pg_catalog.float8"
	if hasWeight {
		w = "weight::pg_catalog.float8"
	}
	rows, err := c.Query(ctx, `SELECT node_id::pg_catalog.int4, host::pg_catalog.text, port::pg_catalog.int4, role::pg_catalog.text, is_active, should_have_shards, `+w+` FROM kingres.node`)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	for rows.Next() {
		var n catalogNode
		if err := rows.Scan(&n.ID, &n.Host, &n.Port, &n.Role, &n.Active, &n.Should, &n.Weight); err != nil {
			return nil, true, err
		}
		nodes = append(nodes, n)
	}
	return nodes, true, rows.Err()
}

// failoverUnfinishedSQL is the reading of the failover state that GET /failover makes (failover.Service.Status): the state is not idle when a task of kind
// server.failover or of the restore of the preferred primaries is pending, running or paused.
const failoverUnfinishedSQL = `SELECT count(*) FROM kingres_cp.task WHERE kind IN ('server.failover', 'replication.restorePreferredPrimaries') AND state IN ('pending', 'running', 'paused')`

// checkImportCatalog fails the import when the catalog of its private database no longer agrees with the system catalog (catalogDifferences), or when a failover is
// not over: a standby that is promoted before the system catalog says so would pass the comparison. The failure goes the way of every failure of the task: the
// private database is removed and the operation is repeated.
func (o *Ops) checkImportCatalog(ctx context.Context, priv string) error {
	pc, err := o.Service.ServiceConn(ctx, priv)
	if err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to the private database now; repeat the operation")
	}
	pn, hasPriv, err := readCatalogNodes(ctx, pc)
	_ = pc.Close(context.WithoutCancel(ctx))
	if err != nil {
		return transfer.SafeError(err)
	}
	if !hasPriv {
		return nil // no sharded tables: no catalog follows anything
	}
	sc, err := o.Service.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to the system database now; repeat the operation")
	}
	sn, hasSys, err := readCatalogNodes(ctx, sc)
	var failovers int
	if err == nil {
		err = sc.QueryRow(ctx, failoverUnfinishedSQL).Scan(&failovers)
	}
	_ = sc.Close(context.WithoutCancel(ctx))
	if err != nil {
		return transfer.SafeError(err)
	}
	if !hasSys {
		return transfer.Errorf(transfer.CodeVerification, false, "the system database has no shard catalog, so the catalog of the private database cannot be checked; the private database is removed")
	}
	if failovers > 0 {
		return transfer.Errorf(transfer.CodeLeaderChanged, true, "a failover or a planned switchover is not over, so the primaries of the cluster are not settled; the private database is removed, repeat the operation when it has ended")
	}
	if d := catalogDifferences(pn, sn); len(d) > 0 {
		return transfer.Errorf(transfer.CodeLeaderChanged, true, "a primary of the cluster changed during the import (%s); the private database is removed, repeat the operation",
			safeSentence(strings.Join(d, "; ")))
	}
	return nil
}

// seedImportCatalog gives the nodes of the private catalog the flag should_have_shards and the weight that the system catalog has for the same host and port. The
// registration of a server (kingres.add_node) makes a node with the defaults, so without this a server that was drained, or given a weight, before the import started
// would be a receiver of the buckets of the private database (the final database would have buckets on a server that the cluster means to empty), and the check before
// the final name would fail every import on such a cluster. The flag is_active is not changed here: a node that the cluster has not got active is a reason to fail
// at the check, not one to hide.
func (o *Ops) seedImportCatalog(ctx context.Context, priv string) error {
	pc, err := o.Service.ServiceConn(ctx, priv)
	if err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to the private database now; repeat the operation")
	}
	defer func() { _ = pc.Close(context.WithoutCancel(ctx)) }()
	pn, hasPriv, err := readCatalogNodes(ctx, pc)
	if err != nil {
		return transfer.SafeError(err)
	}
	if !hasPriv {
		return nil
	}
	sc, err := o.Service.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to the system database now; repeat the operation")
	}
	sn, hasSys, err := readCatalogNodes(ctx, sc)
	_ = sc.Close(context.WithoutCancel(ctx))
	if err != nil {
		return transfer.SafeError(err)
	}
	if !hasSys {
		return transfer.Errorf(transfer.CodeVerification, false, "the system database has no shard catalog, so the private database cannot be given the placement flags of the cluster")
	}
	sys := map[string]catalogNode{}
	for _, n := range sn {
		sys[n.addr()] = n
	}
	for _, n := range pn {
		s, ok := sys[n.addr()]
		if !ok {
			continue // a node that the cluster does not know: the check before the final name reports it
		}
		if n.Should != s.Should {
			if err := setNodeProperty(ctx, pc, priv, []int32{n.ID}, s.Should); err != nil {
				return transfer.SafeError(err)
			}
		}
		if !sameWeight(n.Weight, s.Weight) && s.Weight != nil {
			if _, err := pc.Exec(ctx, `SELECT kingres.set_node_property($1, NULL, $2)`, n.ID, *s.Weight); err != nil {
				return transfer.SafeError(err)
			}
		}
	}
	return nil
}
