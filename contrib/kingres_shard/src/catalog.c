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
#include "access/skey.h"
#include "catalog/indexing.h"
#include "catalog/namespace.h"
#include "catalog/pg_inherits.h"
#include "commands/trigger.h"
#include "executor/spi.h"
#include "utils/builtins.h"
#include "utils/fmgroids.h"
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
	cs->scan = KS_TABLE_BEGINSCAN(cs->rel, cs->snap, 0, NULL);
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
 * kingres_shard_cluster_secret
 *
 * Read the cluster secret (review finding S2-04) into buf.  False when the
 * table or its row does not exist.  The table is read with the heap API: no
 * role has a privilege on it, and the caller is the extension's own code.
 */
bool
kingres_shard_cluster_secret(unsigned char *buf, int bufsize, int *len)
{
	Oid			relid = kingres_shard_catalog_relid("cluster_secret");
	Relation	rel;
	SysScanDesc scan;
	HeapTuple	tup;
	bool		found = false;

	if (!OidIsValid(relid))
		return false;
	rel = table_open(relid, AccessShareLock);

	/*
	 * A catalog snapshot, not the snapshot of the transaction: the first
	 * statement of a session is checked before anything else runs, and taking
	 * the transaction snapshot there would make a SET TRANSACTION ISOLATION
	 * LEVEL that follows an error.  It sees the changes of this transaction.
	 */
	scan = systable_beginscan(rel, InvalidOid, false, NULL, 0, NULL);
	tup = systable_getnext(scan);
	if (tup != NULL)
	{
		bool		isnull;
		Datum		d = heap_getattr(tup, 1, RelationGetDescr(rel), &isnull);

		if (!isnull)
		{
			bytea	   *b = DatumGetByteaPP(d);
			int			n = VARSIZE_ANY_EXHDR(b);

			if (n > 0 && n <= bufsize)
			{
				memcpy(buf, VARDATA_ANY(b), n);
				*len = n;
				found = true;
			}
		}
	}
	systable_endscan(scan);
	table_close(rel, AccessShareLock);
	return found;
}

/*
 * kingres_shard_local_node_id_raw
 *
 * The id of this node, read like the cluster secret (with a catalog snapshot,
 * without the catalog cache, so that it can run before the transaction
 * snapshot is taken); 0 when there is none.
 */
int
kingres_shard_local_node_id_raw(void)
{
	Oid			relid = kingres_shard_catalog_relid("local_node");
	Relation	rel;
	SysScanDesc scan;
	HeapTuple	tup;
	int			id = 0;

	if (!OidIsValid(relid))
		return 0;
	rel = table_open(relid, AccessShareLock);
	scan = systable_beginscan(rel, InvalidOid, false, NULL, 0, NULL);
	tup = systable_getnext(scan);
	if (tup != NULL)
	{
		bool		isnull;
		Datum		d = heap_getattr(tup, 1, RelationGetDescr(rel), &isnull);

		if (!isnull)
			id = DatumGetInt32(d);
	}
	systable_endscan(scan);
	table_close(rel, AccessShareLock);
	return id;
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
 * The placement of a colocation group, as one number: which shard group holds
 * each bucket.  A distributed scan and a fan-out write compare it between the
 * node that plans them and every node they read or write (design/shard-phase4.md,
 * "The map version rule"); it changes exactly when a bucket moves between groups,
 * not with the other changes of the catalog (a state, another table, another
 * colocation group), which count the map version up too.  The sum of a mixed
 * value per bucket, so that the order of the rows does not matter: the node that
 * asks reads the heap in any order.
 */
static uint64
placement_mix(int bucket, int group)
{
	uint64		x = (((uint64) (uint32) bucket) << 32) | (uint32) group;

	x += UINT64CONST(0x9E3779B97F4A7C15);
	x = (x ^ (x >> 30)) * UINT64CONST(0xBF58476D1CE4E5B9);
	x = (x ^ (x >> 27)) * UINT64CONST(0x94D049BB133111EB);
	return x ^ (x >> 31);
}

/*
 * The latest committed map version of this node, whatever the snapshot of the
 * transaction is; -1 when the table or its row does not exist.
 */
int64
kingres_shard_map_version_now(void)
{
	Oid			relid = kingres_shard_catalog_relid("map_version");
	CatScan		cs;
	HeapTuple	tup;
	int64		version = -1;

	if (!OidIsValid(relid))
		return -1;
	catscan_begin(&cs, relid);
	tup = catscan_next(&cs);
	if (tup != NULL)
	{
		bool		isnull;

		version = DatumGetInt64(heap_getattr(tup, 1, RelationGetDescr(cs.rel), &isnull));
	}
	catscan_end(&cs);
	return version;
}

/*
 * The identity of the set of partitions of a partitioned table: the sum of a
 * mixed value per partition OID, so that the order does not matter.  It is the
 * number a scan compares before and after it declares its cursor on a node
 * (design/shard-phase4.md, "The map version rule"), and the number the plan of
 * the local part of a scan was made with.
 *
 * Why OIDs and not the placement: a bucket that moves away and comes back has
 * the same placement before and after, but the copy that comes back is a new
 * table with a new OID (a move builds a standalone table on its target and the
 * old partition becomes an orphan), so the identity differs after every switch
 * that attached or detached a partition and cannot return to an earlier value
 * unless an OID is handed out twice, which needs 2^32 relation creations while a
 * single statement starts.  Counting the partitions, as 0.3 did, misses a
 * detach and an attach that cancel out.
 */
int64
kingres_shard_partition_set_identity(int n, const Oid *oids)
{
	uint64		h = 0;
	int			i;

	for (i = 0; i < n; i++)
		h += placement_mix((int) oids[i], 0);
	return (int64) h;
}

/*
 * The identity of the partitions attached to a partitioned table now, from the
 * catalog with the latest committed snapshot (the partition list of a relcache
 * entry can be behind a commit whose invalidation has not reached this backend).
 */
int64
kingres_shard_partition_identity_now(Oid parent)
{
	Relation	rel = table_open(InheritsRelationId, AccessShareLock);
	ScanKeyData key;
	SysScanDesc scan;
	Snapshot	snap = RegisterSnapshot(IsInParallelMode() ? GetActiveSnapshot() : GetLatestSnapshot());
	HeapTuple	tup;
	uint64		h = 0;

	ScanKeyInit(&key, Anum_pg_inherits_inhparent, BTEqualStrategyNumber, F_OIDEQ,
				ObjectIdGetDatum(parent));
	scan = systable_beginscan(rel, InheritsParentIndexId, true, snap, 1, &key);
	while ((tup = systable_getnext(scan)) != NULL)
	{
		Oid			child = ((Form_pg_inherits) GETSTRUCT(tup))->inhrelid;

		h += placement_mix((int) child, 0);
	}
	systable_endscan(scan);
	UnregisterSnapshot(snap);
	table_close(rel, AccessShareLock);
	return (int64) h;
}

/* From the cache of this backend. */
int64
kingres_shard_placement_hash(const KsColocation *coloc)
{
	uint64		h = 0;
	int			b;

	if (coloc == NULL)
		return 0;
	for (b = 0; b < coloc->bucket_count; b++)
		if (coloc->bucket_group[b] >= 0)
			h += placement_mix(b, coloc->bucket_group[b]);
	return (int64) h;
}

/*
 * From the catalog table, with the latest committed snapshot whatever the
 * snapshot of the transaction is (the cache of a backend can be behind a commit
 * whose invalidation has not reached it yet).
 */
int64
kingres_shard_placement_hash_now(int colocation_id)
{
	Oid			relid = kingres_shard_catalog_relid("bucket_map");
	CatScan		cs;
	HeapTuple	tup;
	uint64		h = 0;

	if (!OidIsValid(relid))
		return 0;
	catscan_begin(&cs, relid);
	while ((tup = catscan_next(&cs)) != NULL)
	{
		TupleDesc	desc = RelationGetDescr(cs.rel);
		bool		isnull;

		if (DatumGetInt32(heap_getattr(tup, 1, desc, &isnull)) != colocation_id)
			continue;
		h += placement_mix(DatumGetInt32(heap_getattr(tup, 2, desc, &isnull)),
						   DatumGetInt32(heap_getattr(tup, 3, desc, &isnull)));
	}
	catscan_end(&cs);
	return (int64) h;
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
