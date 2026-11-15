// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// tableQuery builds the table listing query for the catalog objects that
// exist in the database. Missing kingres tables contribute NULL columns.
// classPredicate is the SQL condition of a table class (the same classes the Go code
// derives for a row), so that the filter can run in the same query as the paging.
func classPredicate(class string, hasShard, hasLedger bool) string {
	dist, ledger := "false", "false"
	if hasShard {
		dist = "(dt.method IS NOT NULL AND dt.colocation_id IS NOT NULL)"
	}
	if hasLedger {
		ledger = "(lt.table_id IS NOT NULL)"
	}
	switch class {
	case "distributed":
		if hasShard {
			return "(dt.method IS NOT NULL AND dt.colocation_id IS NOT NULL AND dt.method <> 'reference')"
		}
		return "false"
	case "reference":
		if hasShard {
			return "(dt.method = 'reference' AND dt.colocation_id IS NOT NULL)"
		}
		return "false"
	case "ledger":
		return ledger
	case "plain":
		return "(NOT " + dist + " AND NOT " + ledger + ")"
	}
	return "true"
}

// tableQuery builds the table listing query for the catalog objects that
// exist in the database; $1 and $2 filter the schema and the name, $3 and $4 are
// the position to continue after (NULL: from the start), $5 the number of rows.
func tableQuery(hasShard, hasLedger, hasUpdatable bool, class string) string {
	dist := "NULL::text, NULL::text, NULL::int, NULL::int"
	distJoin := ""
	if hasShard {
		dist = "dt.method, dt.dist_column::text, dt.colocation_id, co.bucket_count"
		distJoin = `LEFT JOIN kingres.dist_table dt ON dt.relid::oid = c.oid
		LEFT JOIN kingres.colocation co ON co.colocation_id = dt.colocation_id`
	}
	ledger := "false, NULL::text, NULL::text"
	ledgerJoin, hideHistory := "", ""
	if hasLedger {
		ledger = "(lt.table_id IS NOT NULL), lt.mode, NULL::text"
		ledgerJoin = "LEFT JOIN kingres.ledger_table lt ON lt.relid::oid = c.oid"
	}
	if hasUpdatable {
		// 0.2: the registry has history_relid; history tables are an attribute of
		// their table, not tables of the user.
		ledger = "(lt.table_id IS NOT NULL), lt.mode, (SELECT hc.relname::text FROM pg_class hc WHERE hc.oid = lt.history_relid::oid)"
		hideHistory = "AND NOT EXISTS (SELECT 1 FROM kingres.ledger_table hl WHERE hl.history_relid::oid = c.oid)"
	}
	return `SELECT n.nspname, c.relname,
	-- pg_partition_tree returns no rows for a table that is not partitioned.
	-- reltuples is -1 until the table was analyzed: the estimate is then NULL.
	CASE WHEN c.relkind = 'p'
	     THEN (SELECT CASE WHEN bool_or(ch.reltuples < 0) THEN NULL ELSE sum(ch.reltuples)::bigint END
	           FROM pg_partition_tree(c.oid) pt JOIN pg_class ch ON ch.oid = pt.relid WHERE pt.isleaf)
	     WHEN c.reltuples < 0 THEN NULL
	     ELSE c.reltuples::bigint END,
	CASE WHEN c.relkind = 'p'
	     THEN coalesce((SELECT sum(pg_total_relation_size(pt.relid)) FROM pg_partition_tree(c.oid) pt WHERE pt.isleaf), 0)
	     ELSE pg_total_relation_size(c.oid) END::bigint,
	` + dist + `, ` + ledger + `
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	` + distJoin + `
	` + ledgerJoin + `
WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'kingres', 'kingres_cp')
  AND n.nspname !~ '^pg_(toast|temp)'
  ` + hideHistory + `
  AND ($1 = '' OR n.nspname = $1) AND ($2 = '' OR c.relname = $2)
  AND ($3::text IS NULL OR (n.nspname, c.relname) > ($3, $4))
  AND ` + classPredicate(class, hasShard, hasLedger) + `
ORDER BY n.nspname, c.relname
LIMIT $5::int`
}

// readTables reads the tables of a database; schema and name filter when
// non-empty.
func (s *Store) readTables(ctx context.Context, h *dbHandle, schema, name, class string, after []string, limit int) ([]api.Table, error) {
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return nil, err
	}
	// A distributed or reference table has data on every group; this node only
	// sees its own partitions, so with several groups the totals are unknown.
	multiGroup := len(cat.groups) > 1
	hasShard, err := s.regclassExists(ctx, h.pool, "kingres.dist_table")
	if err != nil {
		return nil, err
	}
	if hasShard {
		// dist_table is useless without colocation.
		if hasShard, err = s.regclassExists(ctx, h.pool, "kingres.colocation"); err != nil {
			return nil, err
		}
	}
	hasLedger, err := s.regclassExists(ctx, h.pool, "kingres.ledger_table")
	if err != nil {
		return nil, err
	}
	hasUpdatable := false
	if hasLedger {
		if err := h.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'kingres.ledger_table'::regclass AND attname = 'history_relid' AND NOT attisdropped)`).Scan(&hasUpdatable); err != nil {
			return nil, s.wrap("checking the ledger version", err)
		}
	}
	home := s.localGroup(ctx, h)
	var as, an any
	if after != nil {
		as, an = after[0], after[1]
	}
	rows, err := h.pool.Query(ctx, tableQuery(hasShard, hasLedger, hasUpdatable, class), schema, name, as, an, limit)
	if err != nil {
		return nil, s.wrap("listing tables", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.Table, error) {
		var (
			t                  api.Table
			method, column     *string
			colocation, bucket *int32
			isLedger           bool
			lmode, lhistory    *string
		)
		if err := r.Scan(&t.Schema, &t.Name, &t.EstimatedRows, &t.SizeBytes, &method, &column, &colocation, &bucket, &isLedger, &lmode, &lhistory); err != nil {
			return t, s.wrap("scanning tables", err)
		}
		t.Classes = []string{"plain"}
		if method != nil && colocation != nil {
			class := "distributed"
			if *method == "reference" {
				class = "reference"
			}
			t.Classes = []string{class}
			bc := 0
			if bucket != nil {
				bc = int(*bucket)
			}
			t.Distribution = &api.TableDistribution{Method: *method, Column: column, ColocationID: *colocation, BucketCount: bc}
			if multiGroup {
				t.EstimatedRows, t.SizeBytes = nil, nil
			}
			if *method == "reference" {
				t.Distribution.Column = nil
			}
		} else {
			g := home
			t.HomeShardGroupID = &g
		}
		if isLedger {
			// plain is never combined with another class.
			if len(t.Classes) == 1 && t.Classes[0] == "plain" {
				t.Classes = []string{"ledger"}
			} else {
				t.Classes = append(t.Classes, "ledger")
			}
			t.Ledger = &api.TableLedger{Mode: "appendOnly", HistoryTable: lhistory}
			if lmode != nil && *lmode == "updatable" {
				t.Ledger.Mode = "updatable"
			}
		}
		return t, nil
	})
}

// localGroup is the catalog group of this node (from local_node), used as the
// home group of plain tables; the map group (0) when the node is not in the
// catalog, which is the contract's default home.
func (s *Store) localGroup(ctx context.Context, h *dbHandle) int32 {
	ok, err := s.regclassExists(ctx, h.pool, "kingres.local_node")
	if err != nil || !ok {
		return 0
	}
	var g int32
	err = h.pool.QueryRow(ctx, `SELECT n.group_id FROM kingres.local_node l
		JOIN kingres.node n ON n.node_id = l.node_id LIMIT 1`).Scan(&g)
	if err != nil {
		return 0
	}
	return g
}

// Tables implements store.Store.
func (s *Store) Tables(ctx context.Context, db string, f store.TableFilter, p store.Page) (api.TablePage, error) {
	h, err := s.requireDatabase(ctx, db)
	if err != nil {
		return api.TablePage{}, err
	}
	after, err := store.DecodeTextKey(p.Cursor, 2)
	if err != nil {
		return api.TablePage{}, err
	}
	// Keyset paging in SQL on (schema, name): the database builds one more row than
	// the page, never the whole list of tables.
	all, err := s.readTables(ctx, h, f.Schema, "", f.Class, after, p.Limit+1)
	if err != nil {
		return api.TablePage{}, err
	}
	items, next := store.KeysetPage(all, p, func(t api.Table) string { return store.EncodeTextKey(t.Schema, t.Name) })
	if items == nil {
		items = []api.Table{}
	}
	return api.TablePage{Items: items, NextCursor: next}, nil
}

// Table implements store.Store.
func (s *Store) Table(ctx context.Context, db, schema, name string) (api.TableDetail, error) {
	h, err := s.requireDatabase(ctx, db)
	if err != nil {
		return api.TableDetail{}, err
	}
	all, err := s.readTables(ctx, h, schema, name, "", nil, 1)
	if err != nil {
		return api.TableDetail{}, err
	}
	if len(all) == 0 {
		return api.TableDetail{}, store.ErrNotFound
	}
	cols, err := s.readColumns(ctx, h, all[0])
	if err != nil {
		return api.TableDetail{}, s.wrap("reading columns", err)
	}
	return api.TableDetail{Table: all[0], Columns: cols}, nil
}

// ColocationGroups implements store.Store.
func (s *Store) ColocationGroups(ctx context.Context, db string) ([]api.ColocationGroup, error) {
	h, err := s.requireDatabase(ctx, db)
	if err != nil {
		return nil, err
	}
	out := []api.ColocationGroup{}
	ok, err := s.regclassExists(ctx, h.pool, "kingres.colocation")
	if err != nil || !ok {
		return out, err
	}
	hasDist, err := s.regclassExists(ctx, h.pool, "kingres.dist_table")
	if err != nil {
		return nil, err
	}
	count, size := "0", "0"
	if hasDist {
		count = "(SELECT count(*) FROM kingres.dist_table dt WHERE dt.colocation_id = co.colocation_id)"
		size = `(SELECT coalesce(sum(CASE WHEN rc.relkind = 'p'
		           THEN (SELECT coalesce(sum(pg_total_relation_size(pt.relid)), 0) FROM pg_partition_tree(dt.relid::oid) pt WHERE pt.isleaf)
		           ELSE pg_total_relation_size(dt.relid::oid) END), 0)
		         FROM kingres.dist_table dt JOIN pg_class rc ON rc.oid = dt.relid::oid
		         WHERE dt.colocation_id = co.colocation_id)`
	}
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return nil, err
	}
	multiGroup := len(cat.groups) > 1
	rows, err := h.pool.Query(ctx, `SELECT co.colocation_id, co.bucket_count, co.dist_column_type::text,
		`+count+`::int, `+size+`::bigint FROM kingres.colocation co ORDER BY co.colocation_id`)
	if err != nil {
		return nil, s.wrap("reading kingres.colocation", err)
	}
	out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.ColocationGroup, error) {
		var g api.ColocationGroup
		err := r.Scan(&g.ColocationID, &g.BucketCount, &g.DistributionColumnType, &g.TableCount, &g.SizeBytes)
		if multiGroup {
			g.SizeBytes = nil // this node sees only its own partitions
		}
		return g, err
	})
	return out, s.wrap("reading kingres.colocation", err)
}

// serversByGroup resolves, for every catalog group, the server of its active
// primary and the servers of its active standbys.
func serversByGroup(cat *catalog) (primary map[int32]*string, replicas map[int32][]string) {
	primary = map[int32]*string{}
	replicas = map[int32][]string{}
	for g, nodes := range cat.groups {
		for _, n := range nodes {
			if !n.Active {
				continue
			}
			id := cat.byNode[n.ID].ID
			if n.Role == "primary" {
				if primary[g] == nil {
					primary[g] = &id
				}
			} else {
				replicas[g] = append(replicas[g], id)
			}
		}
	}
	return primary, replicas
}

// Buckets implements store.Store.
func (s *Store) Buckets(ctx context.Context, db string, colocationID int32, f store.BucketFilter, p store.Page) (api.BucketPage, error) {
	h, err := s.requireDatabase(ctx, db)
	if err != nil {
		return api.BucketPage{}, err
	}
	ok, err := s.regclassExists(ctx, h.pool, "kingres.bucket_map")
	if err != nil {
		return api.BucketPage{}, err
	}
	if !ok {
		return api.BucketPage{}, store.ErrNotFound
	}
	var exists bool
	if err := h.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM kingres.colocation WHERE colocation_id = $1)", colocationID).Scan(&exists); err != nil {
		return api.BucketPage{}, s.wrap("reading kingres.colocation", err)
	}
	if !exists {
		return api.BucketPage{}, store.ErrNotFound
	}
	version, err := s.mapVersion(ctx, h.pool)
	if err != nil {
		return api.BucketPage{}, err
	}
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return api.BucketPage{}, err
	}
	primary, replicas := serversByGroup(cat)
	rows, err := h.pool.Query(ctx, `SELECT bucket, group_id, state FROM kingres.bucket_map
		WHERE colocation_id = $1 ORDER BY bucket`, colocationID)
	if err != nil {
		return api.BucketPage{}, s.wrap("reading kingres.bucket_map", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.Bucket, error) {
		var b api.Bucket
		var g int32
		if err := r.Scan(&b.Bucket, &g, &b.State); err != nil {
			return b, err
		}
		b.ShardGroupID = g
		b.PrimaryServerID = primary[g]
		b.ReplicaServerIDs = append([]string{}, replicas[g]...)
		return b, nil
	})
	if err != nil {
		return api.BucketPage{}, s.wrap("reading kingres.bucket_map", err)
	}
	sel := all[:0]
	for _, b := range all {
		if f.ShardGroupID != nil && b.ShardGroupID != *f.ShardGroupID {
			continue
		}
		if f.State != "" && b.State != f.State {
			continue
		}
		if f.ServerID != nil && !bucketOnServer(b, *f.ServerID) {
			continue
		}
		sel = append(sel, b)
	}
	items, next, err := store.Paginate(sel, p)
	return api.BucketPage{MapVersion: version, Items: items, NextCursor: next}, err
}

func bucketOnServer(b api.Bucket, id string) bool {
	if b.PrimaryServerID != nil && *b.PrimaryServerID == id {
		return true
	}
	for _, r := range b.ReplicaServerIDs {
		if r == id {
			return true
		}
	}
	return false
}

// BucketDistribution implements store.Store. Sizes are null: per-bucket sizes
// need the agent, which does not exist yet.
func (s *Store) BucketDistribution(ctx context.Context, db string, colocationID *int32) (api.BucketDistribution, error) {
	h, err := s.requireDatabase(ctx, db)
	if err != nil {
		return api.BucketDistribution{}, err
	}
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return api.BucketDistribution{}, err
	}
	stats, version, err := s.bucketStats(ctx, h.name, colocationID)
	if err != nil {
		return api.BucketDistribution{}, err
	}
	per := map[string]*api.ServerBucketCount{}
	for _, sv := range cat.servers {
		per[sv.ID] = &api.ServerBucketCount{ServerID: sv.ID}
	}
	for g, gb := range stats {
		for _, n := range cat.groups[g] {
			if !n.Active {
				continue
			}
			c := per[cat.byNode[n.ID].ID]
			if n.Role == "primary" {
				c.PrimaryBuckets += gb.held()
			} else {
				c.ReplicaBuckets += gb.held()
			}
		}
	}
	out := api.BucketDistribution{MapVersion: version, Servers: make([]api.ServerBucketCount, 0, len(per))}
	// In catalog order (smallest node id first); the sizes stay null until the
	// agent measures them.
	for _, sv := range cat.servers {
		out.Servers = append(out.Servers, *per[sv.ID])
	}
	return out, nil
}

// columnSQL reads the columns of a table with the facts that decide whether a
// column can be the distribution column of a hash-distributed table:
//
//   - hashable: the column type (after resolving a domain to its base type)
//     has a default hash operator class with an extended hash function, which
//     is what the bucket function needs. varchar is hashed as text and cidr as
//     inet, as PostgreSQL does through binary coercion; arrays, enums and
//     ranges use their polymorphic operator classes.
//   - outsideUniqueKey: the table has a primary or unique index that does not
//     contain the column among its key columns. A partitioned table requires
//     every unique index to contain the partition key.
const columnSQL = `
SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
  EXISTS (
    SELECT 1 FROM pg_opclass oc
      JOIN pg_am am ON am.oid = oc.opcmethod AND am.amname = 'hash'
      JOIN pg_amproc ap ON ap.amprocfamily = oc.opcfamily AND ap.amproclefttype = oc.opcintype AND ap.amprocnum = 2
    WHERE oc.opcdefault AND oc.opcintype IN (
      CASE WHEN bt.oid = 'varchar'::regtype THEN 'text'::regtype
           WHEN bt.oid = 'cidr'::regtype THEN 'inet'::regtype ELSE bt.oid END,
      CASE WHEN bt.typcategory = 'A' THEN 'anyarray'::regtype
           WHEN bt.typtype = 'e' THEN 'anyenum'::regtype
           WHEN bt.typtype = 'r' THEN 'anyrange'::regtype
           WHEN bt.typtype = 'm' THEN 'anymultirange'::regtype END)),
  ARRAY(
    SELECT ic.relname::text FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
    WHERE i.indrelid = c.oid AND i.indisunique
      AND NOT EXISTS (SELECT 1 FROM generate_series(0, i.indnkeyatts - 1) k WHERE i.indkey[k] = a.attnum)
    ORDER BY 1)
FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_attribute a ON a.attrelid = c.oid
  JOIN pg_type t ON t.oid = a.atttypid
  JOIN pg_type bt ON bt.oid = CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE t.oid END
WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`

// readColumns reads the columns of a table and decides, for each, whether it
// can be the distribution column; the first applicable reason of the
// contract's DistributionBlockedReason wins.
func (s *Store) readColumns(ctx context.Context, h *dbHandle, t api.Table) ([]api.TableColumn, error) {
	shardingOn := false
	if ok, err := s.regclassExists(ctx, h.pool, "kingres.dist_table"); err != nil {
		return nil, err
	} else if ok {
		shardingOn, err = s.regclassExists(ctx, h.pool, "kingres.colocation")
		if err != nil {
			return nil, err
		}
	}
	rows, err := h.pool.Query(ctx, columnSQL, t.Schema, t.Name)
	if err != nil {
		return nil, s.wrap("reading columns", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.TableColumn, error) {
		var c api.TableColumn
		var hashable bool
		var outside []string
		if err := r.Scan(&c.Name, &c.Type, &c.Nullable, &hashable, &outside); err != nil {
			return c, err
		}
		reason := ""
		c.BlockingKeys = []string{}
		switch {
		case !shardingOn:
			reason = "shardingDisabled"
		case t.Distribution != nil:
			reason = "tableNotPlain"
		case !hashable:
			reason = "unsupportedType"
		case len(outside) > 0:
			reason = "notInUniqueKey"
		}
		if reason == "notInUniqueKey" {
			c.BlockingKeys = outside
		}
		if reason == "" {
			c.CanBeDistributionColumn = true
		} else {
			c.DistributionBlockedReason = &reason
		}
		return c, nil
	})
}
