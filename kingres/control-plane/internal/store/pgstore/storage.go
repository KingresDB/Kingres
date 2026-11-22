// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"context"
	"math"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// storagePredicate is the SQL condition of the storage filter of listTables: the table, or any partition of it, uses the access method. An empty
// format is no filter. The format is one of the two the handler accepts, never request text.
func storagePredicate(format string) string {
	am := ""
	switch format {
	case "heap":
		am = "heap"
	case "columnar":
		am = "columnar"
	default:
		return "true"
	}
	return `EXISTS (SELECT 1 FROM pg_class x JOIN pg_am xa ON xa.oid = x.relam
	       WHERE xa.amname = '` + am + `' AND (x.oid = c.oid OR (c.relkind = 'p'
	             AND x.oid IN (SELECT pt.relid FROM pg_partition_tree(c.oid) pt WHERE pt.isleaf))))`
}

// addStorage fills Table.Storage (the storage format, contract 0.17.0) and, for a table that is range-partitioned on one time column, the time
// members of Table.Partitioning. Both come from the catalog of this node: a distributed table shows the copies that this node holds. It adds nothing
// when a query fails: the properties are optional.
func (s *Store) addStorage(ctx context.Context, h *dbHandle, ts []api.Table) {
	if len(ts) == 0 {
		return
	}
	schemas, names := make([]string, len(ts)), make([]string, len(ts))
	idx := map[[2]string]int{}
	for i, t := range ts {
		schemas[i], names[i] = t.Schema, t.Name
		idx[[2]string{t.Schema, t.Name}] = i
	}
	rows, err := h.pool.Query(ctx, `SELECT n.nspname, c.relname, c.relkind::text, am.amname,
		coalesce(l.heap, 0)::int, coalesce(l.columnar, 0)::int, coalesce(l.other, 0)::int
	FROM unnest($1::text[], $2::text[]) AS w(s, r)
	JOIN pg_namespace n ON n.nspname = w.s
	JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = w.r
	LEFT JOIN pg_am am ON am.oid = c.relam
	LEFT JOIN LATERAL (SELECT count(*) FILTER (WHERE a.amname = 'heap') AS heap,
	                          count(*) FILTER (WHERE a.amname = 'columnar') AS columnar,
	                          count(*) FILTER (WHERE a.amname NOT IN ('heap', 'columnar')) AS other
	                     FROM pg_partition_tree(c.oid) pt JOIN pg_class lc ON lc.oid = pt.relid JOIN pg_am a ON a.oid = lc.relam
	                    WHERE pt.isleaf AND c.relkind = 'p') l ON true`, schemas, names)
	if err != nil {
		return
	}
	columnarTables := []int{}
	for rows.Next() {
		var sch, name, kind string
		var am *string
		var heap, col, other int
		if rows.Scan(&sch, &name, &kind, &am, &heap, &col, &other) != nil {
			continue
		}
		i := idx[[2]string{sch, name}]
		st := &api.TableStorage{}
		if kind == "p" {
			st.PartitionFormats = &api.PartitionFormats{Heap: heap, Columnar: col, Other: other}
		} else {
			st.AccessMethod = am
		}
		ts[i].Storage = st
		if col > 0 || (am != nil && *am == "columnar") {
			columnarTables = append(columnarTables, i)
		}
	}
	rows.Close()
	s.addColumnarStats(ctx, h, ts, columnarTables)
	s.addTimePartitioning(ctx, h, ts)
}

// addColumnarStats sums kingres.columnar_info over the columnar relations of the tables (the table itself, or its partitions) and reads their options;
// the options are shown when every relation has the same.
func (s *Store) addColumnarStats(ctx context.Context, h *dbHandle, ts []api.Table, which []int) {
	if len(which) == 0 {
		return
	}
	var have bool
	if err := h.pool.QueryRow(ctx, `SELECT to_regprocedure('kingres.columnar_info(regclass)') IS NOT NULL AND to_regprocedure('kingres.columnar_options(regclass)') IS NOT NULL`).Scan(&have); err != nil || !have {
		return
	}
	schemas, names := make([]string, len(which)), make([]string, len(which))
	idx := map[[2]string]int{}
	for k, i := range which {
		schemas[k], names[k] = ts[i].Schema, ts[i].Name
		idx[[2]string{ts[i].Schema, ts[i].Name}] = i
	}
	rows, err := h.pool.Query(ctx, `SELECT n.nspname, c.relname,
		coalesce(sum(i.stripes), 0)::bigint, coalesce(sum(i.rows), 0)::bigint, coalesce(sum(i.bytes_on_disk), 0)::bigint, coalesce(sum(i.bytes_uncompressed), 0)::bigint,
		CASE WHEN count(DISTINCT o.compression) = 1 THEN min(o.compression) END,
		CASE WHEN count(DISTINCT o.compression_level) = 1 THEN min(o.compression_level) END,
		CASE WHEN count(DISTINCT o.stripe_row_count) = 1 THEN min(o.stripe_row_count) END,
		CASE WHEN count(DISTINCT o.chunk_row_count) = 1 THEN min(o.chunk_row_count) END
	FROM unnest($1::text[], $2::text[]) AS w(s, r)
	JOIN pg_namespace n ON n.nspname = w.s
	JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = w.r
	JOIN LATERAL (SELECT lc.oid AS rel FROM pg_class lc JOIN pg_am a ON a.oid = lc.relam
	               WHERE a.amname = 'columnar' AND (lc.oid = c.oid OR (c.relkind = 'p'
	                     AND lc.oid IN (SELECT pt.relid FROM pg_partition_tree(c.oid) pt WHERE pt.isleaf)))) lr ON true
	CROSS JOIN LATERAL kingres.columnar_info(lr.rel) i
	CROSS JOIN LATERAL kingres.columnar_options(lr.rel) o
	GROUP BY n.nspname, c.relname`, schemas, names)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var sch, name string
		var cs api.ColumnarStats
		if rows.Scan(&sch, &name, &cs.Stripes, &cs.Rows, &cs.BytesOnDisk, &cs.BytesUncompressed, &cs.Compression, &cs.CompressionLevel, &cs.StripeRowCount, &cs.ChunkRowCount) != nil {
			continue
		}
		if cs.BytesOnDisk > 0 {
			r := math.Round(float64(cs.BytesUncompressed)/float64(cs.BytesOnDisk)*100) / 100
			cs.CompressionRatio = &r
		}
		if i, ok := idx[[2]string{sch, name}]; ok && ts[i].Storage != nil {
			c := cs
			ts[i].Storage.Columnar = &c
		}
	}
}

// addTimePartitioning sets the time members of Table.Partitioning for the tables of the page that are range-partitioned on one column of a time type
// (the tables a policy of kingres_timepart can work on): the column and its type, the partition count, whether there is a default partition and
// whether a policy exists.
func (s *Store) addTimePartitioning(ctx context.Context, h *dbHandle, ts []api.Table) {
	schemas, names := []string{}, []string{}
	idx := map[[2]string]int{}
	for i, t := range ts {
		if t.Storage != nil && t.Storage.PartitionFormats != nil {
			schemas, names = append(schemas, t.Schema), append(names, t.Name)
			idx[[2]string{t.Schema, t.Name}] = i
		}
	}
	if len(schemas) == 0 {
		return
	}
	var policies bool
	if err := h.pool.QueryRow(ctx, `SELECT to_regclass('kingres.timepart_policy') IS NOT NULL`).Scan(&policies); err != nil {
		return
	}
	policy := "false"
	if policies {
		policy = "EXISTS (SELECT 1 FROM kingres.timepart_policy tp WHERE tp.schema_name = n.nspname AND tp.table_name = c.relname)"
	}
	rows, err := h.pool.Query(ctx, `SELECT n.nspname, c.relname, a.attname::text, a.atttypid::regtype::text,
		(SELECT count(*) FROM pg_inherits i WHERE i.inhparent = c.oid)::int,
		EXISTS (SELECT 1 FROM pg_inherits i JOIN pg_class ch ON ch.oid = i.inhrelid WHERE i.inhparent = c.oid AND pg_get_expr(ch.relpartbound, ch.oid) = 'DEFAULT'),
		`+policy+`
	FROM unnest($1::text[], $2::text[]) AS w(s, r)
	JOIN pg_namespace n ON n.nspname = w.s
	JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = w.r
	JOIN pg_partitioned_table pt ON pt.partrelid = c.oid AND pt.partstrat = 'r' AND pt.partnatts = 1 AND pt.partattrs[0] > 0
	JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = pt.partattrs[0]
	WHERE a.atttypid IN ('date'::regtype, 'timestamp'::regtype, 'timestamptz'::regtype)`, schemas, names)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var sch, name, col, typ string
		var count int
		var def, pol bool
		if rows.Scan(&sch, &name, &col, &typ, &count, &def, &pol) != nil {
			continue
		}
		i := idx[[2]string{sch, name}]
		switch typ {
		case "timestamp without time zone":
			typ = "timestamp"
		case "timestamp with time zone":
			typ = "timestamptz"
		}
		p := ts[i].Partitioning
		if p == nil {
			p = &api.TablePartitioning{Method: "range", Key: col, PartitionCount: count}
		}
		hp, dp := pol, def
		p.ColumnType, p.HasPolicy, p.DefaultPartition = typ, &hp, &dp
		ts[i].Partitioning = p
	}
}
