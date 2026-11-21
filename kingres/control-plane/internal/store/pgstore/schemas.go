// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// schemaFacts is what the database has for distributed schemas: kingres_shard 0.8 has the view kingres.schemas
// and the function kingres.schema_inventory; an older extension has neither, and every schema is local.
type schemaFacts struct {
	view      bool
	inventory bool
}

func (s *Store) schemaFacts(ctx context.Context, h *dbHandle) (schemaFacts, error) {
	var f schemaFacts
	err := h.pool.QueryRow(ctx, `SELECT to_regclass('kingres.schemas') IS NOT NULL, to_regprocedure('kingres.schema_inventory(regnamespace)') IS NOT NULL`).Scan(&f.view, &f.inventory)
	return f, s.wrap("checking for distributed schemas", err)
}

// userSchemaPredicate selects the schemas a user made: not the system schemas, not the schemas of the control plane and
// the extensions, not a toast or temporary schema.
const userSchemaPredicate = `n.nspname !~ '^pg_' AND n.nspname NOT IN ('information_schema', 'kingres', 'kingres_cp')
  AND NOT EXISTS (SELECT 1 FROM pg_depend dp WHERE dp.classid = 'pg_namespace'::regclass AND dp.objid = n.oid AND dp.deptype = 'e')`

// schemaQuery lists the schemas of a database in name order; $1 is the name to continue after (NULL: from the start),
// $2 a name to return alone (NULL: all), $3 the number of rows. With the view of the extension a distributed schema
// carries its colocation group, owner group, node, state and move target; without it those columns are NULL.
func schemaQuery(f schemaFacts) string {
	dist := "NULL::int, NULL::int, NULL::int, NULL::text, NULL::int, NULL::bigint"
	join := ""
	if f.view {
		dist = "d.colocation_id, d.group_id, d.node_id, d.state, d.moving_to_group_id, d.size_bytes"
		join = "LEFT JOIN kingres.schemas d ON d.schema_name = n.nspname"
	}
	return `SELECT n.nspname, pg_get_userbyid(n.nspowner),
	(SELECT count(*) FROM pg_class c WHERE c.relnamespace = n.oid AND c.relkind IN ('r', 'p') AND NOT c.relispartition)::int,
	-- the size of the tables, indexes and TOAST of a local schema; a distributed schema has it in the view, where it is known
	(SELECT sum(pg_total_relation_size(c.oid)) FROM pg_class c WHERE c.relnamespace = n.oid AND c.relkind IN ('r', 'p') AND NOT c.relispartition)::bigint,
	-- the planner's estimate; NULL as soon as one table was never analyzed (reltuples is -1)
	(SELECT CASE WHEN bool_or(c.reltuples < 0) THEN NULL ELSE coalesce(sum(c.reltuples), 0)::bigint END
	   FROM pg_class c WHERE c.relnamespace = n.oid AND c.relkind = 'r'),
	` + dist + `
FROM pg_namespace n
` + join + `
WHERE ` + userSchemaPredicate + `
  AND ($1::text IS NULL OR n.nspname > $1) AND ($2::text IS NULL OR n.nspname = $2)
ORDER BY n.nspname
LIMIT $3::int`
}

type schemaRow struct {
	sum       api.SchemaSummary
	colo      *int32
	group     *int32
	node      *int32
	state     *string
	movingTo  *int32
	viewSize  *int64
	localSize *int64
	rows      *int64
}

func (s *Store) readSchemas(ctx context.Context, h *dbHandle, f schemaFacts, after, name *string, limit int) ([]schemaRow, error) {
	rows, err := h.pool.Query(ctx, schemaQuery(f), after, name, limit)
	if err != nil {
		return nil, s.wrap("listing schemas", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (schemaRow, error) {
		var x schemaRow
		var tables int32
		err := r.Scan(&x.sum.Name, &x.sum.Owner, &tables, &x.localSize, &x.rows, &x.colo, &x.group, &x.node, &x.state, &x.movingTo, &x.viewSize)
		x.sum.TableCount = int(tables)
		return x, s.wrap("scanning schemas", err)
	})
}

// schemaSummary turns a row into the contract's summary. A distributed schema is summarized with what this server
// can know: the size from the view (known where the tables are held), the row estimate only on the server of the owner
// group (elsewhere the tables are empty shells whose estimate says 0).
func (s *Store) schemaSummary(x schemaRow, cat *catalog, localGroup int32) api.SchemaSummary {
	out := x.sum
	if x.colo == nil || x.group == nil {
		out.SizeBytes, out.EstimatedRows = x.localSize, x.rows
		return out
	}
	d := &api.SchemaDistribution{State: "active", ColocationID: *x.colo, ShardGroupID: *x.group, MovingToShardGroupID: x.movingTo}
	if x.state != nil && *x.state != "" {
		d.State = *x.state
		if d.State != "active" {
			d.State = "moving"
		}
	}
	if x.movingTo != nil {
		d.State = "moving"
	}
	if x.node != nil {
		if sv := cat.byNode[*x.node]; sv != nil {
			id := sv.ID
			d.ServerID = &id
		}
	}
	out.Distribution = d
	out.SizeBytes = x.viewSize
	if *x.group == localGroup {
		out.EstimatedRows = x.rows
	}
	return out
}

// Schemas implements store.Store. The filters apply to the distribution, so they are applied after the rows are read;
// the page is cut in name order with a keyset cursor, and a read that has to skip rows asks for the next batch.
func (s *Store) Schemas(ctx context.Context, db string, f store.SchemaFilter, p store.Page) (api.SchemaPage, error) {
	h, err := s.requireDatabase(ctx, db)
	if err != nil {
		return api.SchemaPage{}, err
	}
	after, err := store.DecodeTextKey(p.Cursor, 1)
	if err != nil {
		return api.SchemaPage{}, err
	}
	facts, err := s.schemaFacts(ctx, h)
	if err != nil {
		return api.SchemaPage{}, err
	}
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return api.SchemaPage{}, err
	}
	local := s.localGroup(ctx, h)
	var cursor *string
	if after != nil {
		cursor = &after[0]
	}
	out := api.SchemaPage{Items: []api.SchemaSummary{}}
	batch := 200
	for {
		rows, err := s.readSchemas(ctx, h, facts, cursor, nil, batch)
		if err != nil {
			return api.SchemaPage{}, err
		}
		for _, x := range rows {
			sum := s.schemaSummary(x, cat, local)
			if !schemaMatches(sum, f) {
				continue
			}
			if len(out.Items) == p.Limit {
				// one more match exists: the page ends at the last item it holds
				next := store.EncodeTextKey(out.Items[len(out.Items)-1].Name)
				out.NextCursor = &next
				return out, nil
			}
			out.Items = append(out.Items, sum)
		}
		if len(rows) < batch {
			return out, nil
		}
		last := rows[len(rows)-1].sum.Name
		cursor = &last
	}
}

func schemaMatches(sum api.SchemaSummary, f store.SchemaFilter) bool {
	if f.Distributed != nil && (sum.Distribution != nil) != *f.Distributed {
		return false
	}
	if f.ShardGroupID != nil && (sum.Distribution == nil || sum.Distribution.ShardGroupID != *f.ShardGroupID) {
		return false
	}
	if f.State != "" && (sum.Distribution == nil || sum.Distribution.State != f.State) {
		return false
	}
	return true
}

// Schema implements store.Store.
func (s *Store) Schema(ctx context.Context, db, name string) (api.SchemaDetail, error) {
	h, err := s.requireDatabase(ctx, db)
	if err != nil {
		return api.SchemaDetail{}, err
	}
	facts, err := s.schemaFacts(ctx, h)
	if err != nil {
		return api.SchemaDetail{}, err
	}
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return api.SchemaDetail{}, err
	}
	rows, err := s.readSchemas(ctx, h, facts, nil, &name, 1)
	if err != nil {
		return api.SchemaDetail{}, err
	}
	if len(rows) == 0 {
		return api.SchemaDetail{}, store.ErrNotFound
	}
	x := rows[0]
	out := api.SchemaDetail{SchemaSummary: s.schemaSummary(x, cat, s.localGroup(ctx, h)), DistributionBlockers: []api.SchemaBlocker{}}
	if x.colo == nil {
		// a local schema: what keeps it from being distributed
		if facts.inventory {
			rs, err := h.pool.Query(ctx, `SELECT i.object_kind, i.object_name, coalesce(i.reason, '')
				FROM kingres.schema_inventory((SELECT n.oid::regnamespace FROM pg_namespace n WHERE n.nspname = $1)) i
				WHERE NOT i.supported ORDER BY i.object_kind, i.object_name`, name)
			if err != nil {
				return out, s.wrap("reading kingres.schema_inventory", err)
			}
			out.DistributionBlockers, err = pgx.CollectRows(rs, func(r pgx.CollectableRow) (api.SchemaBlocker, error) {
				var b api.SchemaBlocker
				err := r.Scan(&b.ObjectKind, &b.ObjectName, &b.Reason)
				return b, err
			})
			if err != nil {
				return out, s.wrap("reading kingres.schema_inventory", err)
			}
			if out.DistributionBlockers == nil {
				out.DistributionBlockers = []api.SchemaBlocker{}
			}
		}
		return out, nil
	}
	// a distributed schema: pinned by a ledger table, which is on the node of the owner group
	if x.node != nil {
		var reason *string
		err := h.pool.QueryRow(ctx, `SELECT kingres.internal_exec_scalar($1, $2, 10000)`, *x.node,
			"SELECT kingres.internal_schema_pinned("+strconv.Itoa(int(*x.colo))+")").Scan(&reason)
		if err != nil {
			return out, s.wrap("asking the owner whether the schema is pinned", err)
		}
		if reason != nil && *reason != "" {
			out.Pinned = &api.SchemaPinned{Reason: *reason}
		}
	}
	id, err := s.unfinishedSchemaTask(ctx, db, name)
	if err != nil {
		return out, err
	}
	out.MoveInProgress = id
	return out, nil
}

// unfinishedSchemaTask is the id of the unfinished task that moves, or makes local, the schema; nil when none.
func (s *Store) unfinishedSchemaTask(ctx context.Context, db, schema string) (*int64, error) {
	pool, err := s.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	ok, err := s.regclassExists(ctx, pool, "kingres_cp.task")
	if err != nil || !ok {
		return nil, err
	}
	var id *int64
	err = pool.QueryRow(ctx, `SELECT max(id) FROM kingres_cp.task WHERE subject_type = 'schema' AND subject_id = $1
		AND kind IN ('schema.move', 'schema.undistribute') AND state IN ('pending', 'running', 'paused')`, db+"/"+schema).Scan(&id)
	if err != nil {
		return nil, s.wrap(fmt.Sprintf("reading the tasks of schema %q", schema), err)
	}
	return id, nil
}
