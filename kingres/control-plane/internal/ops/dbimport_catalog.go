// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// A failover leaves out the private database of an import (importdb), so its catalog is not followed: after the failover of a group it still names the old primary
// of that group. The import checks it before it takes the final name, because the database that is renamed is the database of the client from then on, and nothing
// follows its catalog afterwards but the next failover, which then expects the old primary.

// catalogNode is the part of a row of kingres.node that the check compares: the node ids and the group ids of the private database are its own.
type catalogNode struct {
	Host string
	Port int32
	Role string
}

func (n catalogNode) addr() string { return fmt.Sprintf("%s:%d", n.Host, n.Port) }

// staleCatalogPrimaries are the nodes that the private catalog holds as primary and the system catalog (the primary of the map group is its source of truth, as
// for every database) does not: a node that is a standby there, or that is not there any more.
func staleCatalogPrimaries(private, system []catalogNode) []string {
	primary := map[string]bool{}
	for _, n := range system {
		if n.Role == "primary" {
			primary[n.addr()] = true
		}
	}
	var stale []string
	for _, n := range private {
		if n.Role == "primary" && !primary[n.addr()] {
			stale = append(stale, n.addr())
		}
	}
	sort.Strings(stale)
	return stale
}

func readCatalogNodes(ctx context.Context, c *pgx.Conn) ([]catalogNode, error) {
	var has bool
	if err := c.QueryRow(ctx, `SELECT pg_catalog.to_regclass('kingres.node') IS NOT NULL`).Scan(&has); err != nil || !has {
		return nil, err
	}
	rows, err := c.Query(ctx, `SELECT host::pg_catalog.text, port::pg_catalog.int4, role::pg_catalog.text FROM kingres.node`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []catalogNode
	for rows.Next() {
		var n catalogNode
		if err := rows.Scan(&n.Host, &n.Port, &n.Role); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// checkImportCatalog fails the import when the catalog of its private database no longer agrees with the system catalog on the primary of a group: a failover ran
// while the import did (the failure removes the private database, and the import is repeated).
func (o *Ops) checkImportCatalog(ctx context.Context, priv string) error {
	pc, err := o.Service.ServiceConn(ctx, priv)
	if err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to the private database now; repeat the operation")
	}
	pn, err := readCatalogNodes(ctx, pc)
	_ = pc.Close(context.WithoutCancel(ctx))
	if err != nil {
		return transfer.SafeError(err)
	}
	if len(pn) == 0 {
		return nil
	}
	sc, err := o.Service.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to the system database now; repeat the operation")
	}
	sn, err := readCatalogNodes(ctx, sc)
	_ = sc.Close(context.WithoutCancel(ctx))
	if err != nil {
		return transfer.SafeError(err)
	}
	if stale := staleCatalogPrimaries(pn, sn); len(stale) > 0 {
		return transfer.Errorf(transfer.CodeLeaderChanged, true, "a failover changed the primary of a group while the import ran (the catalog of the private database still names %s); the private database is removed, repeat the operation",
			safeSentence(strings.Join(stale, ", ")))
	}
	return nil
}
