// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"math"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// rangeBoundRE reads the bound of a partition of a table that is range-partitioned on one column: FROM ('2026-01-01') TO ('2026-02-01').
var rangeBoundRE = regexp.MustCompile(`^FOR VALUES FROM \('([^']*)'\) TO \('([^']*)'\)$`)

// nativePartitions lists the partitions of an ordinary range- or list-partitioned table of this server's instance (contract 0.17.0: listTablePartitions
// lists them as well as the user partitions of a distributed table). A table that is not partitioned, or is hash-partitioned, lists none. The size and
// the estimated rows are those of this instance; a columnar partition also has its compression ratio.
func (o *Ops) nativePartitions(ctx context.Context, tx pgx.Tx, schema, table, cursor string, limit int, page *api.TablePartitionPage) error {
	var strat string
	err := tx.QueryRow(ctx, `SELECT pt.partstrat::text FROM pg_partitioned_table pt WHERE pt.partrelid = $1::regclass`, qualified(schema, table)).Scan(&strat)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil
		}
		return err
	}
	method := map[string]string{"r": "range", "l": "list"}[strat]
	if method == "" {
		return nil
	}
	var columnar bool
	if err := tx.QueryRow(ctx, `SELECT to_regprocedure('kingres.columnar_info(regclass)') IS NOT NULL`).Scan(&columnar); err != nil {
		return err
	}
	ratio := "NULL::float8"
	if columnar {
		ratio = `CASE WHEN am.amname = 'columnar' THEN (SELECT i.compression_ratio FROM kingres.columnar_info(c.oid) i) END`
	}
	rows, err := tx.Query(ctx, `SELECT c.relname::text, pg_get_expr(c.relpartbound, c.oid), pg_total_relation_size(c.oid), am.amname::text,
			CASE WHEN c.reltuples < 0 THEN NULL ELSE c.reltuples::bigint END, `+ratio+`
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid LEFT JOIN pg_am am ON am.oid = c.relam
		WHERE i.inhparent = $1::regclass AND c.relname::text > $2 ORDER BY c.relname LIMIT $3`, qualified(schema, table), cursor, limit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var x api.TablePartition
		var bound string
		var size int64
		var rt *float64
		if err := rows.Scan(&x.Name, &bound, &size, &x.AccessMethod, &x.EstimatedRows, &rt); err != nil {
			return err
		}
		x.Method, x.Bound, x.IsDefault, x.LeafCount, x.SizeBytes = method, bound, bound == "DEFAULT", 1, &size
		if m := rangeBoundRE.FindStringSubmatch(bound); m != nil {
			from, to := m[1], m[2]
			x.RangeFrom, x.RangeTo = &from, &to
		}
		if rt != nil {
			r := math.Round(*rt*100) / 100
			x.CompressionRatio = &r
		}
		page.Items = append(page.Items, x)
	}
	return rows.Err()
}
