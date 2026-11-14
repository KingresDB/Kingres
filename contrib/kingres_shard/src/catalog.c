/*-------------------------------------------------------------------------
 *
 * catalog.c
 *	  The per-backend cache of the kingres catalog tables.
 *
 * The planner hook runs for every statement, so it cannot read the catalog
 * tables each time.  Each backend keeps the node list, the colocation groups
 * with their bucket maps and the distributed tables in a private memory
 * context and rebuilds it lazily after an invalidation.
 *
 * Invalidation.  DML on a table does not invalidate its relcache entry, so
 * the statement trigger that increments kingres.map_version (see
 * kingres_map_version_bump() below) also calls
 * CacheInvalidateRelcacheByRelid() for map_version; every backend of the
 * database then sees a relcache callback for that relation and drops its
 * cache.  While the extension is not installed the cache holds only the fact
 * that it is absent, and any relcache invalidation drops that, so a
 * CREATE EXTENSION is noticed.  See design/shard-mvp.md, "Catalog cache".
 *
 * The catalog is read with plain heap scans instead of SPI so that loading
 * it from inside the planner cannot recurse into the planner hook.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/catalog.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/genam.h"
#include "access/heapam.h"
#include "access/htup_details.h"
#include "access/table.h"
#include "access/tableam.h"
#include "access/xact.h"
#include "catalog/namespace.h"
#include "commands/trigger.h"
#include "executor/spi.h"
#include "utils/builtins.h"
#include "utils/inval.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/snapmgr.h"

#include "catalog.h"
#include "kingres_shard.h"

PG_FUNCTION_INFO_V1(kingres_map_version_bump);

/* Catalog tables whose relcache invalidation drops the cache. */
static const char *const catalog_tables[] = {
	"node", "colocation", "dist_table", "bucket_map", "map_version",
	"local_node"
};
#define NUM_CATALOG_TABLES lengthof(catalog_tables)

static struct
{
	bool		valid;
	bool		loading;
	Oid			relids[NUM_CATALOG_TABLES];
	MemoryContext cxt;
	KsCatalog	cat;
}			ks;

static void catalog_inval_callback(Datum arg, Oid relid);
static void catalog_load(void);

/*
 * kingres_shard_catalog_init
 *
 * Called once from _PG_init: registers the relcache callback.
 */
void
kingres_shard_catalog_init(void)
{
	CacheRegisterRelcacheCallback(catalog_inval_callback, (Datum) 0);
}

/*
 * catalog_inval_callback
 *
 * Relcache callbacks may run at any time, so this only sets a flag; the
 * memory is released by the next catalog_load().
 */
static void
catalog_inval_callback(Datum arg, Oid relid)
{
	int			i;

	if (!ks.valid)
		return;
	if (relid == InvalidOid || !ks.cat.present)
	{
		ks.valid = false;
		return;
	}
	for (i = 0; i < (int) NUM_CATALOG_TABLES; i++)
	{
		if (ks.relids[i] == relid)
		{
			ks.valid = false;
			return;
		}
	}
}

/*
 * kingres_shard_catalog_invalidate
 *
 * Drop the cache of this backend, for example after the catalog was
 * rewritten by a catalog sync.
 */
void
kingres_shard_catalog_invalidate(void)
{
	ks.valid = false;
}

/*
 * kingres_shard_catalog
 *
 * Return the cache, loading it first if it is missing or invalid.  While the
 * catalog is being loaded, a nested call sees an empty "not present" catalog,
 * so that code reached from the loader cannot recurse.
 */
const KsCatalog *
kingres_shard_catalog(void)
{
	static KsCatalog empty;

	if (ks.loading)
		return &empty;
	if (!ks.valid)
		catalog_load();
	return &ks.cat;
}

/*
 * kingres_shard_catalog_relid
 *
 * The OID of one of the catalog tables, or InvalidOid when the extension is
 * not installed.
 */
Oid
kingres_shard_catalog_relid(const char *relname)
{
	Oid			nsp = get_namespace_oid(KINGRES_SHARD_SCHEMA, true);

	if (!OidIsValid(nsp))
		return InvalidOid;
	return get_relname_relid(relname, nsp);
}

/*
 * Begin a scan of one catalog table with a snapshot that sees this
 * transaction's own earlier changes.
 */
typedef struct CatScan
{
	Relation	rel;
	TableScanDesc scan;
	Snapshot	snap;
} CatScan;

static void
catscan_begin(CatScan *cs, Oid relid)
{
	cs->rel = table_open(relid, AccessShareLock);
	/*
	 * GetLatestSnapshot() cannot be used in parallel mode (a parallel worker,
	 * or the leader while workers run): it raises "cannot update
	 * SecondarySnapshot during a parallel operation" for every statement a
	 * parallel worker plans.  The active snapshot is the right one there: no
	 * catalog write can happen during a parallel operation.
	 */
	cs->snap = RegisterSnapshot(IsInParallelMode() ? GetActiveSnapshot() :
								GetLatestSnapshot());
	cs->scan = table_beginscan(cs->rel, cs->snap, 0, NULL);
}

static HeapTuple
catscan_next(CatScan *cs)
{
	return heap_getnext(cs->scan, ForwardScanDirection);
}

static void
catscan_end(CatScan *cs)
{
	table_endscan(cs->scan);
	UnregisterSnapshot(cs->snap);
	table_close(cs->rel, AccessShareLock);
}

/*
 * catalog_load
 *
 * Rebuild the cache from the catalog tables.  Everything is allocated in a
 * fresh context; the old one is deleted only after the new one is complete,
 * so an error during the load leaves the backend without a valid cache but
 * with no half-built state in use.
 */
static void
catalog_load(void)
{
	MemoryContext newcxt;
	MemoryContext oldcxt;
	MemoryContext prevcxt;
	KsCatalog	cat;
	Oid			relids[NUM_CATALOG_TABLES];
	Oid			nsp;
	int			i;
	bool		complete = true;
	CatScan		cs;
	HeapTuple	tup;
	HASHCTL		ctl;
	List	   *groups = NIL;
	ListCell   *lc;

	memset(&cat, 0, sizeof(cat));
	cat.local_group_id = -1;

	nsp = get_namespace_oid(KINGRES_SHARD_SCHEMA, true);
	for (i = 0; i < (int) NUM_CATALOG_TABLES; i++)
	{
		relids[i] = OidIsValid(nsp) ?
			get_relname_relid(catalog_tables[i], nsp) : InvalidOid;
		if (!OidIsValid(relids[i]))
			complete = false;
	}

	newcxt = AllocSetContextCreate(CacheMemoryContext,
								   "kingres_shard catalog cache",
								   ALLOCSET_DEFAULT_SIZES);
	prevcxt = ks.cxt;
	ks.loading = true;

	PG_TRY();
	{
		oldcxt = MemoryContextSwitchTo(newcxt);

		memset(&ctl, 0, sizeof(ctl));
		ctl.keysize = sizeof(Oid);
		ctl.entrysize = sizeof(KsDistTable);
		ctl.hcxt = newcxt;
		cat.dist_tables = hash_create("kingres_shard dist tables", 64, &ctl,
									  HASH_ELEM | HASH_BLOBS | HASH_CONTEXT);

		if (complete)
		{
			cat.present = true;

			/* map_version */
			catscan_begin(&cs, relids[4]);
			while ((tup = catscan_next(&cs)) != NULL)
			{
				bool		isnull;

				cat.map_version =
					DatumGetInt64(heap_getattr(tup, 1,
											   RelationGetDescr(cs.rel),
											   &isnull));
			}
			catscan_end(&cs);

			/* node(node_id, group_id, host, port, role, is_active, should_have_shards) */
			catscan_begin(&cs, relids[0]);
			while ((tup = catscan_next(&cs)) != NULL)
			{
				TupleDesc	desc = RelationGetDescr(cs.rel);
				bool		isnull;
				KsNode	   *n = palloc0(sizeof(KsNode));
				char	   *role;

				n->node_id = DatumGetInt32(heap_getattr(tup, 1, desc, &isnull));
				n->group_id = DatumGetInt32(heap_getattr(tup, 2, desc, &isnull));
				n->host = TextDatumGetCString(heap_getattr(tup, 3, desc, &isnull));
				n->port = DatumGetInt32(heap_getattr(tup, 4, desc, &isnull));
				role = TextDatumGetCString(heap_getattr(tup, 5, desc, &isnull));
				n->is_primary = (strcmp(role, "primary") == 0);
				n->is_active = DatumGetBool(heap_getattr(tup, 6, desc, &isnull));
				n->should_have_shards =
					DatumGetBool(heap_getattr(tup, 7, desc, &isnull));
				pfree(role);
				cat.nodes = lappend(cat.nodes, n);
				if (n->is_active && n->is_primary &&
					!list_member_int(groups, n->group_id))
					groups = lappend_int(groups, n->group_id);
			}
			catscan_end(&cs);
			cat.ngroups = list_length(groups);

			/* local_node(node_id) */
			catscan_begin(&cs, relids[5]);
			while ((tup = catscan_next(&cs)) != NULL)
			{
				bool		isnull;

				cat.local_node_id =
					DatumGetInt32(heap_getattr(tup, 1,
											   RelationGetDescr(cs.rel),
											   &isnull));
			}
			catscan_end(&cs);
			foreach(lc, cat.nodes)
			{
				KsNode	   *n = (KsNode *) lfirst(lc);

				if (n->node_id == cat.local_node_id)
					cat.local_group_id = n->group_id;
			}

			/* colocation(colocation_id, bucket_count, dist_column_type, dist_collation) */
			catscan_begin(&cs, relids[1]);
			while ((tup = catscan_next(&cs)) != NULL)
			{
				TupleDesc	desc = RelationGetDescr(cs.rel);
				bool		isnull;
				KsColocation *c = palloc0(sizeof(KsColocation));
				int			b;

				c->colocation_id = DatumGetInt32(heap_getattr(tup, 1, desc, &isnull));
				c->bucket_count = DatumGetInt32(heap_getattr(tup, 2, desc, &isnull));
				c->dist_type = DatumGetObjectId(heap_getattr(tup, 3, desc, &isnull));
				c->collation = DatumGetObjectId(heap_getattr(tup, 4, desc, &isnull));
				c->bucket_group = palloc(sizeof(int) * c->bucket_count);
				for (b = 0; b < c->bucket_count; b++)
					c->bucket_group[b] = -1;
				cat.colocations = lappend(cat.colocations, c);
			}
			catscan_end(&cs);

			/* bucket_map(colocation_id, bucket, group_id, state) */
			catscan_begin(&cs, relids[3]);
			while ((tup = catscan_next(&cs)) != NULL)
			{
				TupleDesc	desc = RelationGetDescr(cs.rel);
				bool		isnull;
				int			cid = DatumGetInt32(heap_getattr(tup, 1, desc, &isnull));
				int			bucket = DatumGetInt32(heap_getattr(tup, 2, desc, &isnull));
				int			group = DatumGetInt32(heap_getattr(tup, 3, desc, &isnull));

				foreach(lc, cat.colocations)
				{
					KsColocation *c = (KsColocation *) lfirst(lc);

					if (c->colocation_id == cid)
					{
						if (bucket >= 0 && bucket < c->bucket_count)
							c->bucket_group[bucket] = group;
						break;
					}
				}
			}
			catscan_end(&cs);

			/* dist_table(relid, method, dist_column, colocation_id) */
			catscan_begin(&cs, relids[2]);
			while ((tup = catscan_next(&cs)) != NULL)
			{
				TupleDesc	desc = RelationGetDescr(cs.rel);
				bool		isnull;
				Oid			relid = DatumGetObjectId(heap_getattr(tup, 1, desc, &isnull));
				char	   *method = TextDatumGetCString(heap_getattr(tup, 2, desc, &isnull));
				Datum		coldatum = heap_getattr(tup, 3, desc, &isnull);
				bool		col_null = isnull;
				Datum		ciddatum = heap_getattr(tup, 4, desc, &isnull);
				bool		cid_null = isnull;
				KsDistTable *dt;
				bool		found;
				AttrNumber	attno = InvalidAttrNumber;
				bool		is_ref = (strcmp(method, "reference") == 0);

				/* A relation that vanished is cleaned up by the drop trigger. */
				if (get_rel_name(relid) == NULL)
					continue;
				if (!is_ref)
				{
					if (col_null || cid_null)
						continue;
					attno = get_attnum(relid, NameStr(*DatumGetName(coldatum)));
					if (attno == InvalidAttrNumber)
						continue;
				}

				dt = hash_search(cat.dist_tables, &relid, HASH_ENTER, &found);
				dt->relid = relid;
				dt->dist_attno = attno;
				dt->colocation_id = is_ref ? 0 : DatumGetInt32(ciddatum);
				dt->coloc = NULL;
				dt->is_reference = is_ref;
				if (is_ref)
					memset(&dt->dist_column, 0, sizeof(dt->dist_column));
				else
				{
					namestrcpy(&dt->dist_column, NameStr(*DatumGetName(coldatum)));
					foreach(lc, cat.colocations)
					{
						KsColocation *c = (KsColocation *) lfirst(lc);

						if (c->colocation_id == dt->colocation_id)
							dt->coloc = c;
					}
				}
				if (!is_ref && dt->coloc == NULL)
					hash_search(cat.dist_tables, &relid, HASH_REMOVE, NULL);
				else
					cat.ndist_tables++;
			}
			catscan_end(&cs);
		}
		MemoryContextSwitchTo(oldcxt);
	}
	PG_CATCH();
	{
		ks.loading = false;
		MemoryContextDelete(newcxt);
		PG_RE_THROW();
	}
	PG_END_TRY();

	ks.loading = false;
	ks.cxt = newcxt;
	ks.cat = cat;
	memcpy(ks.relids, relids, sizeof(relids));
	if (prevcxt != NULL)
		MemoryContextDelete(prevcxt);
	ks.valid = true;
}

/*
 * kingres_shard_lookup_dist_table
 *
 * The cached entry of a distributed table, or NULL when the relation is not
 * distributed (or the extension is not installed).  This is one hash lookup,
 * the cost every statement pays per range-table entry.
 */
const KsDistTable *
kingres_shard_lookup_dist_table(Oid relid)
{
	const KsCatalog *cat = kingres_shard_catalog();

	if (!cat->present || cat->ndist_tables == 0)
		return NULL;
	return hash_search(cat->dist_tables, &relid, HASH_FIND, NULL);
}

/*
 * kingres_shard_lookup_colocation
 *
 * The cached colocation group with the given id, or NULL.
 */
const KsColocation *
kingres_shard_lookup_colocation(int colocation_id)
{
	const KsCatalog *cat = kingres_shard_catalog();
	ListCell   *lc;

	foreach(lc, cat->colocations)
	{
		KsColocation *c = (KsColocation *) lfirst(lc);

		if (c->colocation_id == colocation_id)
			return c;
	}
	return NULL;
}

/*
 * kingres_shard_group_primary
 *
 * The active primary node of a shard group, or NULL.
 */
const KsNode *
kingres_shard_group_primary(int group_id)
{
	const KsCatalog *cat = kingres_shard_catalog();
	ListCell   *lc;

	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);

		if (n->group_id == group_id && n->is_primary && n->is_active)
			return n;
	}
	return NULL;
}

/*
 * kingres_shard_node_by_id
 *
 * The cached node row with the given id, or NULL.
 */
const KsNode *
kingres_shard_node_by_id(int node_id)
{
	const KsCatalog *cat = kingres_shard_catalog();
	ListCell   *lc;

	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);

		if (n->node_id == node_id)
			return n;
	}
	return NULL;
}

/*
 * kingres_map_version_bump
 *
 * Statement-level trigger on the four synchronized catalog tables.  It
 * increments kingres.map_version and makes every backend of the database
 * drop its catalog cache, by invalidating the relcache entry of map_version.
 * It also invalidates the relcache entries of all distributed tables, so
 * that cached plans that depend on the old bucket map are replanned.
 */
Datum
kingres_map_version_bump(PG_FUNCTION_ARGS)
{
	Oid			mvrelid;
	int			ret;
	uint64		i;

	if (!CALLED_AS_TRIGGER(fcinfo))
		ereport(ERROR,
				(errcode(ERRCODE_E_R_I_E_TRIGGER_PROTOCOL_VIOLATED),
				 errmsg("kingres.map_version_bump() must be called as a trigger")));

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	ret = SPI_execute("UPDATE " KINGRES_SHARD_SCHEMA ".map_version SET version = version + 1",
					  false, 0);
	if (ret != SPI_OK_UPDATE)
		elog(ERROR, "could not update map_version");

	mvrelid = kingres_shard_catalog_relid("map_version");
	if (OidIsValid(mvrelid))
		CacheInvalidateRelcacheByRelid(mvrelid);

	ret = SPI_execute("SELECT d.relid::pg_catalog.oid FROM " KINGRES_SHARD_SCHEMA ".dist_table d "
					  "JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) d.relid::pg_catalog.oid",
					  true, 0);
	if (ret != SPI_OK_SELECT)
		elog(ERROR, "could not list distributed tables");
	for (i = 0; i < SPI_processed; i++)
	{
		bool		isnull;
		Datum		d = SPI_getbinval(SPI_tuptable->vals[i],
									  SPI_tuptable->tupdesc, 1, &isnull);

		if (!isnull)
			CacheInvalidateRelcacheByRelid(DatumGetObjectId(d));
	}

	SPI_finish();
	ks.valid = false;
	return PointerGetDatum(NULL);
}
