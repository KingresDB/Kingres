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
 * SPDX-License-Identifier: AGPL-3.0-only
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
#include "funcapi.h"
#include "utils/builtins.h"
#include "utils/fmgroids.h"
#include "utils/inval.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/snapmgr.h"

#include "catalog.h"
#include "kingres_shard.h"
#include "node_conn.h"

PG_FUNCTION_INFO_V1(kingres_map_version_bump);
PG_FUNCTION_INFO_V1(kingres_internal_catalog_stats);

/* What this backend did for the cache (kingres.internal_catalog_stats(), for the tests of step 12). */
static int64 stat_lazy_lookups = 0;
static int64 stat_invalidations = 0;

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
	HTAB	   *cohash;			/* colocation id -> KsColocation *, of the cached list */
}			ks;

typedef struct ColocEntry
{
	int			cid;
	KsColocation *coloc;
} ColocEntry;

static void catalog_inval_callback(Datum arg, Oid relid);
static void catalog_load(void);
static void catalog_retire(MemoryContext cxt);
static void catalog_xact_callback(XactEvent event, void *arg);

/*
 * kingres_shard_catalog_init
 *
 * Called once from _PG_init: registers the relcache callback.
 */
void
kingres_shard_catalog_init(void)
{
	CacheRegisterRelcacheCallback(catalog_inval_callback, (Datum) 0);
	RegisterXactCallback(catalog_xact_callback, NULL);
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
 * A cache that was replaced is not freed at once: code that is in the middle of a statement
 * holds pointers into it (a distributed table, a colocation group, a node) and the load can
 * happen under any call that takes a lock, and under debug_discard_caches under every one of
 * them.  The pointers then keep describing the catalog as the statement began with it, and
 * the memory goes when the transaction ends.
 */
static List *retired_cxts = NIL;

static void
catalog_retire(MemoryContext cxt)
{
	MemoryContext old = MemoryContextSwitchTo(TopMemoryContext);

	retired_cxts = lappend(retired_cxts, cxt);
	MemoryContextSwitchTo(old);
}

static void
catalog_xact_callback(XactEvent event, void *arg)
{
	ListCell   *lc;

	if (event != XACT_EVENT_COMMIT && event != XACT_EVENT_ABORT && event != XACT_EVENT_PREPARE)
		return;
	foreach(lc, retired_cxts)
		MemoryContextDelete((MemoryContext) lfirst(lc));
	list_free(retired_cxts);
	retired_cxts = NIL;
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
 * kingres_shard_catalog_generation
 *
 * How many times this backend has loaded the cache.  Everything a caller took from the cache is
 * dangling once the number changed: code that calls something that may load the cache again
 * (parse analysis takes locks, which process invalidations) compares it before it goes on with
 * pointers of its own.
 */
static int64 catalog_generation = 0;

int64
kingres_shard_catalog_generation(void)
{
	return catalog_generation;
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
static bool
read_secret_attr(int attno, bool committed_view, unsigned char *buf, int bufsize, int *len)
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
		bool		isnull = true;
		Datum		d = (Datum) 0;

		/*
		 * The committed view (S9-03): when this transaction wrote the row (a rotation
		 * of the secret that has not committed), the secret that counts for the name
		 * of a prepared transaction is the one it replaced: the names are verified by
		 * the origin after the rotation committed (the replaced secret is then its
		 * previous one) or after it rolled back (it is the current one).
		 */
		if (committed_view && attno == 1 &&
			TransactionIdIsCurrentTransactionId(HeapTupleHeaderGetXmin(tup->t_data)) &&
			2 <= RelationGetDescr(rel)->natts)
		{
			bool		pnull;

			(void) heap_getattr(tup, 2, RelationGetDescr(rel), &pnull);
			if (!pnull)
				attno = 2;
		}

		/* The previous secret (attribute 2) does not exist in a table older than the library. */
		if (attno <= RelationGetDescr(rel)->natts)
			d = heap_getattr(tup, attno, RelationGetDescr(rel), &isnull);
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

bool
kingres_shard_cluster_secret(unsigned char *buf, int bufsize, int *len)
{
	return read_secret_attr(1, false, buf, bufsize, len);
}

/*
 * The secret that the one in force replaced (S9-03): a node that holds a rotation
 * prepared and was not told it committed still has it, and the node that can finish
 * the rotation proves itself to that node with it.  Never used to accept a session.
 */
bool
kingres_shard_previous_secret(unsigned char *buf, int bufsize, int *len)
{
	return read_secret_attr(2, false, buf, bufsize, len);
}

/* The secret that names of prepared transactions are made with: the current one as other sessions see it. */
bool
kingres_shard_cluster_secret_for_names(unsigned char *buf, int bufsize, int *len)
{
	return read_secret_attr(1, true, buf, bufsize, len);
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
	HTAB	   *cohash = NULL;

	memset(&cat, 0, sizeof(cat));
	cat.local_group_id = -1;
	catalog_generation++;

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
		cat.schema_tables = hash_create("kingres_shard schema tables", 64, &ctl,
										HASH_ELEM | HASH_BLOBS | HASH_CONTEXT);
		{
			HASHCTL		cctl;

			memset(&cctl, 0, sizeof(cctl));
			cctl.keysize = sizeof(int);
			cctl.entrysize = sizeof(ColocEntry);
			cctl.hcxt = newcxt;
			cohash = hash_create("kingres_shard colocations", 256, &cctl, HASH_ELEM | HASH_BLOBS | HASH_CONTEXT);
		}

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
				/* the columns that review S7 added; an older SQL script has only the version */
				if (RelationGetDescr(cs.rel)->natts >= 3)
				{
					cat.term = DatumGetInt64(heap_getattr(tup, 2, RelationGetDescr(cs.rel), &isnull));
					cat.superseded_by_term = DatumGetInt64(heap_getattr(tup, 3, RelationGetDescr(cs.rel), &isnull));
				}
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
				cat.local_timeline = 0;
				if (RelationGetDescr(cs.rel)->natts >= 2)
				{
					Datum		d = heap_getattr(tup, 2, RelationGetDescr(cs.rel), &isnull);

					if (!isnull)
						cat.local_timeline = DatumGetInt32(d);
				}
			}
			catscan_end(&cs);
			foreach(lc, cat.nodes)
			{
				KsNode	   *n = (KsNode *) lfirst(lc);

				if (n->node_id == cat.local_node_id)
				{
					/*
					 * A node that the catalog does not call the primary of its
					 * group (it was demoted by promote_node(), or it is a
					 * standby) is in no group as far as routing goes: -2,
					 * which no bucket has, so that nothing is run here as
					 * local and statements go to the primary.  -1 stays "not
					 * initialized".
					 */
					cat.local_group_id = n->is_primary ? n->group_id : -2;
				}
			}

			/*
			 * colocation(colocation_id, bucket_count, dist_column_type, dist_collation,
			 * max_modulus, placement_epoch): the last two exist from 0.8; a catalog that is not
			 * upgraded yet has the layout of a group that was never split.
			 */
			catscan_begin(&cs, relids[1]);
			while ((tup = catscan_next(&cs)) != NULL)
			{
				TupleDesc	desc = RelationGetDescr(cs.rel);
				bool		isnull;
				int			cid = DatumGetInt32(heap_getattr(tup, 1, desc, &isnull));
				int			count = DatumGetInt32(heap_getattr(tup, 2, desc, &isnull));
				Oid			dtype = DatumGetObjectId(heap_getattr(tup, 3, desc, &isnull));
				Oid			coll = DatumGetObjectId(heap_getattr(tup, 4, desc, &isnull));
				int			maxmod = count;
				int64		epoch = 0;

				if (desc->natts >= 5)
				{
					Datum		d = heap_getattr(tup, 5, desc, &isnull);

					if (!isnull)
						maxmod = DatumGetInt32(d);
				}
				if (desc->natts >= 6)
				{
					Datum		d = heap_getattr(tup, 6, desc, &isnull);

					if (!isnull)
						epoch = DatumGetInt64(d);
				}
				{
					KsColocation *nc = kingres_shard_coloc_begin(cid, count, maxmod, epoch, dtype, coll);
					ColocEntry *ce;
					bool		cfound;

					cat.colocations = lappend(cat.colocations, nc);
					ce = hash_search(cohash, &cid, HASH_ENTER, &cfound);
					ce->coloc = nc;
				}
			}
			catscan_end(&cs);

			/* bucket_map(colocation_id, bucket, group_id, state, modulus): modulus exists from 0.8 */
			catscan_begin(&cs, relids[3]);
			while ((tup = catscan_next(&cs)) != NULL)
			{
				TupleDesc	desc = RelationGetDescr(cs.rel);
				bool		isnull;
				int			cid = DatumGetInt32(heap_getattr(tup, 1, desc, &isnull));
				int			bucket = DatumGetInt32(heap_getattr(tup, 2, desc, &isnull));
				int			group = DatumGetInt32(heap_getattr(tup, 3, desc, &isnull));

				{
					ColocEntry *ce = hash_search(cohash, &cid, HASH_FIND, NULL);

					if (ce != NULL)
					{
						KsColocation *c = ce->coloc;
						int			modulus = c->bucket_count;

						if (desc->natts >= 5)
						{
							Datum		d = heap_getattr(tup, 5, desc, &isnull);

							if (!isnull)
								modulus = DatumGetInt32(d);
						}
						kingres_shard_coloc_add(c, bucket, modulus, group);
					}
				}
			}
			catscan_end(&cs);
			foreach(lc, cat.colocations)
				kingres_shard_coloc_finish((KsColocation *) lfirst(lc));

			/* dist_table(relid, method, dist_column, colocation_id) */
			catscan_begin(&cs, relids[2]);
			while ((tup = catscan_next(&cs)) != NULL)
			{
				TupleDesc	desc = RelationGetDescr(cs.rel);
				bool		isnull;
				Oid			relid = DatumGetObjectId(heap_getattr(tup, 1, desc, &isnull));
				Datum		mdat = heap_getattr(tup, 2, desc, &isnull);
				bool		m_null = isnull;
				char	   *method;
				Datum		coldatum = heap_getattr(tup, 3, desc, &isnull);
				bool		col_null = isnull;
				Datum		ciddatum = heap_getattr(tup, 4, desc, &isnull);
				bool		cid_null = isnull;
				KsDistTable *dt;
				bool		found;
				AttrNumber	attno = InvalidAttrNumber;
				bool		is_ref;

				/*
				 * A table of a distributed schema (phase 11) is only counted here; its entry is made when a statement asks for the
				 * table (kingres_shard_lookup_schema_table), so that a catalog of many schemas is not read whole by every backend
				 * after every change of it (step 12).
				 */
				if (!m_null && VARSIZE_ANY_EXHDR(DatumGetPointer(mdat)) == 6 && memcmp(VARDATA_ANY(DatumGetPointer(mdat)), "schema", 6) == 0)
				{
					cat.nschema_tables++;
					continue;
				}
				method = TextDatumGetCString(mdat);
				is_ref = (strcmp(method, "reference") == 0);

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
				dt->is_schema = false;
				if (is_ref)
					memset(&dt->dist_column, 0, sizeof(dt->dist_column));
				else
				{
					namestrcpy(&dt->dist_column, NameStr(*DatumGetName(coldatum)));
					{
						ColocEntry *ce = hash_search(cohash, &dt->colocation_id, HASH_FIND, NULL);

						if (ce != NULL)
							dt->coloc = ce->coloc;
					}
				}
				if (!is_ref && dt->coloc == NULL)
					hash_search(cat.dist_tables, &relid, HASH_REMOVE, NULL);
				else
					cat.ndist_tables++;
			}
			catscan_end(&cs);

			/*
			 * The distributed schemas and the sharding mode (phase 11, version 0.8).  The two tables are not
			 * in the list that decides whether the extension is installed: a database that still has the
			 * 0.7 objects (the library was replaced before ALTER EXTENSION UPDATE) works as it did, with no
			 * distributed schema and the mode "row".
			 */
			{
				Oid			ds_rel = OidIsValid(nsp) ? get_relname_relid("dist_schema", nsp) : InvalidOid;
				Oid			ss_rel = OidIsValid(nsp) ? get_relname_relid("sharding_settings", nsp) : InvalidOid;

				cat.sharding_mode = KS_MODE_ROW;
				if (OidIsValid(ss_rel))
				{
					catscan_begin(&cs, ss_rel);
					while ((tup = catscan_next(&cs)) != NULL)
					{
						TupleDesc	desc = RelationGetDescr(cs.rel);
						bool		isnull;
						char	   *mode = TextDatumGetCString(heap_getattr(tup, 1, desc, &isnull));

						cat.sharding_mode = strcmp(mode, "schema") == 0 ? KS_MODE_SCHEMA :
							strcmp(mode, "both") == 0 ? KS_MODE_BOTH : KS_MODE_ROW;
						cat.distribute_new_schemas = DatumGetBool(heap_getattr(tup, 2, desc, &isnull));
					}
					catscan_end(&cs);
				}
				if (OidIsValid(ds_rel))
				{
					HASHCTL		dctl;

					memset(&dctl, 0, sizeof(dctl));
					dctl.keysize = sizeof(Oid);
					dctl.entrysize = sizeof(KsDistSchema);
					dctl.hcxt = newcxt;
					cat.dist_schemas = hash_create("kingres_shard dist schemas", 64, &dctl,
												   HASH_ELEM | HASH_BLOBS | HASH_CONTEXT);
					/* only counted: the entries are made when a statement asks for the schema (step 12) */
					catscan_begin(&cs, ds_rel);
					while ((tup = catscan_next(&cs)) != NULL)
						cat.ndist_schemas++;
					catscan_end(&cs);
				}
			}
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
	ks.cohash = cohash;
	memcpy(ks.relids, relids, sizeof(relids));
	if (prevcxt != NULL)
		catalog_retire(prevcxt);
	ks.valid = true;

	/* Sessions to nodes that are no longer primaries are of no use any more. */
	if (ks.cat.present)
		kingres_shard_conn_drop_stale(ks.cat.nodes);
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
 *
 * Since 0.8 the number also holds the placement epoch of the group, a counter that only the
 * switch of a move or of a split raises, in the transaction that changes the partitions.  A
 * bucket that moves away and comes back inside the window of a statement restores the sum of
 * the bucket terms, but never the epoch (review finding S4-01).
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

/* The term of the placement epoch of a group in the placement hash. */
static uint64
epoch_term(int64 epoch)
{
	return placement_mix((int) (epoch & 0x7fffffff), (int) ((uint64) epoch >> 31)) ^ UINT64CONST(0x5bd1e9955bd1e995);
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

/*
 * The primary of every shard group, as (group, node id) pairs.  The placement of
 * a bucket is its group AND the node that is that group's primary: when
 * promote_node() changes the primary, a statement that planned against the old
 * one fails with the retryable KS001 instead of answering from it.
 */
typedef struct GroupPrimary
{
	int			group;
	int			node;
} GroupPrimary;

static int
primary_of(const GroupPrimary *gp, int n, int group)
{
	int			i;

	for (i = 0; i < n; i++)
		if (gp[i].group == group)
			return gp[i].node;
	return 0;
}

static uint64
bucket_term(int bucket, int group, int primary)
{
	uint64		t = placement_mix(bucket, group);

	if (primary > 0)
		t ^= placement_mix(primary, 0x2f1d);
	return t;
}

/* From the cache of this backend. */
int64
kingres_shard_placement_hash(const KsColocation *coloc)
{
	const KsCatalog *cat = kingres_shard_catalog();
	GroupPrimary *gp;
	int			ngp = 0;
	uint64		h = 0;
	int			b;
	ListCell   *lc;

	if (coloc == NULL)
		return 0;
	h = epoch_term(coloc->placement_epoch);
	gp = palloc(sizeof(GroupPrimary) * Max(list_length(cat->nodes), 1));
	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);

		if (n->is_primary && n->is_active)
		{
			gp[ngp].group = n->group_id;
			gp[ngp].node = n->node_id;
			ngp++;
		}
	}
	for (b = 0; b < kingres_shard_coloc_nbuckets(coloc); b++)
	{
		int			bucket = kingres_shard_coloc_bucket(coloc, b);
		int			group = kingres_shard_bucket_group(coloc, bucket);

		if (group >= 0)
			h += bucket_term(bucket, group, primary_of(gp, ngp, group));
	}
	pfree(gp);
	return (int64) h;
}

/*
 * The placement epoch of a colocation group from the catalog table, with the latest committed
 * snapshot; 0 for a group that is not there or a catalog of a version without the column.
 */
static int64
placement_epoch_now(int colocation_id)
{
	Oid			relid = kingres_shard_catalog_relid("colocation");
	Relation	rel;
	Snapshot	snap;
	Oid			pkey;
	ScanKeyData key;
	SysScanDesc scan;
	HeapTuple	tup;
	int64		epoch = 0;

	if (!OidIsValid(relid))
		return 0;
	rel = table_open(relid, AccessShareLock);
	if (RelationGetDescr(rel)->natts < 6)
	{
		table_close(rel, AccessShareLock);
		return 0;
	}
	snap = RegisterSnapshot(IsInParallelMode() ? GetActiveSnapshot() : GetLatestSnapshot());
	pkey = get_relname_relid("colocation_pkey", RelationGetNamespace(rel));
	ScanKeyInit(&key, 1, BTEqualStrategyNumber, F_INT4EQ, Int32GetDatum(colocation_id));
	scan = systable_beginscan(rel, pkey, OidIsValid(pkey), snap, 1, &key);
	while ((tup = systable_getnext(scan)) != NULL)
	{
		bool		isnull;
		bool		a;
		Datum		d;

		a = DatumGetInt32(heap_getattr(tup, 1, RelationGetDescr(rel), &isnull)) == colocation_id;
		if (!a)
			continue;
		d = heap_getattr(tup, 6, RelationGetDescr(rel), &isnull);
		if (!isnull)
			epoch = DatumGetInt64(d);
		break;
	}
	systable_endscan(scan);
	UnregisterSnapshot(snap);
	table_close(rel, AccessShareLock);
	return epoch;
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
	Oid			noderel = kingres_shard_catalog_relid("node");
	CatScan		cs;
	HeapTuple	tup;
	uint64		h = 0;
	GroupPrimary *gp = NULL;
	int			ngp = 0;
	int			cap = 0;

	if (!OidIsValid(relid))
		return 0;
	if (OidIsValid(noderel))
	{
		catscan_begin(&cs, noderel);
		while ((tup = catscan_next(&cs)) != NULL)
		{
			TupleDesc	desc = RelationGetDescr(cs.rel);
			bool		isnull;
			char	   *role = TextDatumGetCString(heap_getattr(tup, 5, desc, &isnull));
			bool		active = DatumGetBool(heap_getattr(tup, 6, desc, &isnull));

			if (strcmp(role, "primary") == 0 && active)
			{
				if (ngp == cap)
				{
					cap = cap ? cap * 2 : 8;
					gp = gp ? repalloc(gp, sizeof(GroupPrimary) * cap)
						: palloc(sizeof(GroupPrimary) * cap);
				}
				gp[ngp].node = DatumGetInt32(heap_getattr(tup, 1, desc, &isnull));
				gp[ngp].group = DatumGetInt32(heap_getattr(tup, 2, desc, &isnull));
				ngp++;
			}
			pfree(role);
		}
		catscan_end(&cs);
	}
	/*
	 * Only the buckets of this colocation group are read: through the primary key
	 * (colocation_id, bucket) when it is there, so that the cost of a check does not grow
	 * with the number of buckets of the other groups (review P2B-06); a scan of the whole
	 * table with the same key otherwise.
	 */
	{
		Relation	maprel = table_open(relid, AccessShareLock);
		Snapshot	snap = RegisterSnapshot(IsInParallelMode() ? GetActiveSnapshot() :
											GetLatestSnapshot());
		Oid			pkey = get_relname_relid("bucket_map_pkey", RelationGetNamespace(maprel));
		ScanKeyData key;
		SysScanDesc mscan;

		ScanKeyInit(&key, 1, BTEqualStrategyNumber, F_INT4EQ, Int32GetDatum(colocation_id));
		mscan = systable_beginscan(maprel, pkey, OidIsValid(pkey), snap, 1, &key);
		while ((tup = systable_getnext(mscan)) != NULL)
		{
			TupleDesc	desc = RelationGetDescr(maprel);
			bool		isnull;
			int			bucket;
			int			group;

			if (DatumGetInt32(heap_getattr(tup, 1, desc, &isnull)) != colocation_id)
				continue;
			bucket = DatumGetInt32(heap_getattr(tup, 2, desc, &isnull));
			group = DatumGetInt32(heap_getattr(tup, 3, desc, &isnull));
			h += bucket_term(bucket, group, primary_of(gp, ngp, group));
		}
		systable_endscan(mscan);
		UnregisterSnapshot(snap);
		table_close(maprel, AccessShareLock);
	}
	if (gp != NULL)
		pfree(gp);
	return (int64) (h + epoch_term(placement_epoch_now(colocation_id)));
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
 * kingres_shard_lookup_schema_table
 *
 * The cached entry of a table of a distributed schema, or NULL.  The entry is not in
 * dist_tables, so code that works on hash-distributed tables never sees it.
 */
/*
 * The entries of schema_tables and dist_schemas are made when they are asked for: one index scan of the catalog table for the
 * relation (or the schema), the answer cached until the next reload, "no" included.  A statement pays for the tables it names, not
 * for the number of schemas of the database (step 12).  The scans use the snapshot of the loader, so they see what this
 * transaction changed.
 */
static bool lazy_no_pk_warned = false;

static HeapTuple
lazy_fetch(Oid heapoid, Oid key, SysScanDesc *sd, Relation *rel, Snapshot *snap)
{
	ScanKeyData skey;
	Oid			pkoid;

	stat_lazy_lookups++;
	*rel = table_open(heapoid, AccessShareLock);
	/*
	 * The index is the one that the open relation has now, not an OID kept from the load: REINDEX CONCURRENTLY replaces it without a write to the
	 * catalog table (P12R-03).  A table that has lost its primary key is scanned with no index: the answer stays right (it is never "this is a
	 * local table"), only slower, and the statements that restore the key (ALTER TABLE ... ADD PRIMARY KEY, CREATE UNIQUE INDEX) and every
	 * unrelated statement still run, which an error here would forbid (P12F-01).  The cache is not touched: it holds the answers, not the way
	 * they were found.  The warning comes once for a backend, and again if the key came back and went again.
	 */
	pkoid = RelationGetPrimaryKeyIndex(*rel, true);
	if (!OidIsValid(pkoid))
	{
		if (!lazy_no_pk_warned)
		{
			lazy_no_pk_warned = true;
			ereport(WARNING,
					(errcode(ERRCODE_UNDEFINED_OBJECT),
					 errmsg("the table \"%s\" of the extension has no primary key; it is read without an index, which is slower",
							RelationGetRelationName(*rel)),
					 errhint("Restore the primary key, for example ALTER TABLE kingres.%s ADD PRIMARY KEY (%s).",
							 RelationGetRelationName(*rel),
							 heapoid == ks.relids[2] ? "relid" : "nsp")));
		}
	}
	else
		lazy_no_pk_warned = false;
	*snap = RegisterSnapshot(IsInParallelMode() ? GetActiveSnapshot() : GetLatestSnapshot());
	ScanKeyInit(&skey, 1, BTEqualStrategyNumber, F_OIDEQ, ObjectIdGetDatum(key));
	*sd = systable_beginscan(*rel, pkoid, OidIsValid(pkoid), *snap, 1, &skey);
	return systable_getnext(*sd);
}

static void
lazy_done(SysScanDesc sd, Relation rel, Snapshot snap)
{
	systable_endscan(sd);
	UnregisterSnapshot(snap);
	table_close(rel, AccessShareLock);
}

static const KsDistTable *
lazy_schema_table(Oid relid)
{
	KsDistTable *st;
	bool		found;
	SysScanDesc sd;
	Relation	rel;
	Snapshot	snap;
	HeapTuple	tup;
	bool		is_schema = false;
	int			cid = 0;

	if (!OidIsValid(ks.relids[2]))
		return NULL;
	tup = lazy_fetch(ks.relids[2], relid, &sd, &rel, &snap);
	if (tup != NULL)
	{
		TupleDesc	desc = RelationGetDescr(rel);
		bool		isnull;
		Datum		mdat = heap_getattr(tup, 2, desc, &isnull);
		Datum		cdat = heap_getattr(tup, 4, desc, &isnull);

		if (VARSIZE_ANY_EXHDR(DatumGetPointer(mdat)) == 6 && memcmp(VARDATA_ANY(DatumGetPointer(mdat)), "schema", 6) == 0 && !isnull)
		{
			is_schema = true;
			cid = DatumGetInt32(cdat);
		}
	}
	lazy_done(sd, rel, snap);
	st = hash_search(ks.cat.schema_tables, &relid, HASH_ENTER, &found);
	memset(st, 0, sizeof(*st));
	st->relid = relid;
	st->dist_attno = InvalidAttrNumber;
	st->colocation_id = cid;
	st->is_schema = true;
	if (is_schema)
	{
		ColocEntry *ce = hash_search(ks.cohash, &cid, HASH_FIND, NULL);

		st->coloc = ce != NULL ? ce->coloc : NULL;
	}
	st->negative = (st->coloc == NULL);
	return st->negative ? NULL : st;
}

const KsDistTable *
kingres_shard_lookup_schema_table(const KsCatalog *cat, Oid relid)
{
	KsDistTable *st;

	if (!cat->present || cat->nschema_tables == 0 || cat->schema_tables == NULL)
		return NULL;
	st = hash_search(cat->schema_tables, &relid, HASH_FIND, NULL);
	if (st != NULL)
		return st->negative ? NULL : st;
	if (cat != &ks.cat || ks.loading)
		return NULL;
	return lazy_schema_table(relid);
}

/*
 * The distributed schema with this namespace OID (a node-local OID), or NULL.
 */
const KsDistSchema *
kingres_shard_lookup_dist_schema(const KsCatalog *cat, Oid nsp)
{
	KsDistSchema *s;
	bool		found;
	SysScanDesc sd;
	Relation	rel;
	Snapshot	snap;
	HeapTuple	tup;
	int			cid = 0;
	bool		ship = false;
	bool		have = false;

	if (!cat->present || cat->ndist_schemas == 0 || cat->dist_schemas == NULL)
		return NULL;
	s = hash_search(cat->dist_schemas, &nsp, HASH_FIND, NULL);
	if (s != NULL)
		return s->negative ? NULL : s;
	if (cat != &ks.cat || ks.loading)
		return NULL;
	{
		Oid			ds_rel = kingres_shard_catalog_relid("dist_schema");

		if (!OidIsValid(ds_rel))
			return NULL;
		tup = lazy_fetch(ds_rel, nsp, &sd, &rel, &snap);
		if (tup != NULL)
		{
			TupleDesc	desc = RelationGetDescr(rel);
			bool		isnull;

			cid = DatumGetInt32(heap_getattr(tup, 2, desc, &isnull));
			ship = DatumGetBool(heap_getattr(tup, 4, desc, &isnull));
			have = true;
		}
		lazy_done(sd, rel, snap);
	}
	s = hash_search(cat->dist_schemas, &nsp, HASH_ENTER, &found);
	memset(s, 0, sizeof(*s));
	s->nsp = nsp;
	if (have)
	{
		ColocEntry *ce = hash_search(ks.cohash, &cid, HASH_FIND, NULL);

		s->colocation_id = cid;
		s->ship_functions = ship;
		s->coloc = ce != NULL ? ce->coloc : NULL;
	}
	s->negative = (s->coloc == NULL);
	return s->negative ? NULL : s;
}

/*
 * The shard group that holds the rows of a schema table: the group of its one bucket
 * (-1 when the bucket is not mapped).
 */
int
kingres_shard_schema_table_owner(const KsDistTable *st)
{
	return kingres_shard_bucket_group(st->coloc, kingres_shard_coloc_bucket(st->coloc, 0));
}

/*
 * The table the distributed scan reads: a distributed table (not a reference table) or a table of a
 * distributed schema, or NULL.
 */
const KsDistTable *
kingres_shard_lookup_scan_table(const KsCatalog *cat, Oid relid)
{
	const KsDistTable *dt;

	if (!cat->present)
		return NULL;
	if (cat->dist_tables != NULL && cat->ndist_tables > 0)
	{
		dt = hash_search(cat->dist_tables, &relid, HASH_FIND, NULL);
		if (dt != NULL)
			return dt->is_reference ? NULL : dt;
	}
	return kingres_shard_lookup_schema_table(cat, relid);
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
	ColocEntry *ce;

	if (!cat->present || ks.cohash == NULL)
		return NULL;
	ce = hash_search(ks.cohash, &colocation_id, HASH_FIND, NULL);
	return ce != NULL ? ce->coloc : NULL;
}

/*
 * The buckets of a colocation group (catalog.h).  The layout is kept in the arrays of
 * KsColocation; these functions and the three that make a layout are the only code that
 * reads or writes them.
 */
int
kingres_shard_coloc_nbuckets(const KsColocation *coloc)
{
	return coloc->nbucket;
}

int
kingres_shard_coloc_bucket(const KsColocation *coloc, int i)
{
	Assert(i >= 0 && i < coloc->nbucket);
	return coloc->bucket_list[i];
}

/*
 * The modulus of the hash partition that is a bucket: FOR VALUES WITH (MODULUS m,
 * REMAINDER bucket).  Every bucket of a group that was never split has the modulus
 * bucket_count; a bucket that was split has children of a larger modulus
 * (design/shard-phase10.md).  The code that creates or attaches a bucket partition asks here.
 */
int
kingres_shard_bucket_modulus(const KsColocation *coloc, int bucket)
{
	Assert(bucket >= 0 && bucket < coloc->max_modulus && coloc->bucket_mod[bucket] > 0);
	return coloc->bucket_mod[bucket];
}

/*
 * A copy of a colocation group's bucket layout that stays valid when the catalog cache is
 * reloaded (the batch sender keeps one for the whole of a statement, which plans other
 * statements while it runs).  Made in the current memory context.
 */
KsColocation *
kingres_shard_coloc_copy(const KsColocation *coloc)
{
	KsColocation *c = palloc(sizeof(KsColocation));
	Size		n = sizeof(int) * Max(coloc->max_modulus, 1);

	*c = *coloc;
	c->bucket_list = palloc(sizeof(int) * Max(coloc->nbucket, 1));
	memcpy(c->bucket_list, coloc->bucket_list, sizeof(int) * coloc->nbucket);
	c->bucket_group = palloc(n);
	memcpy(c->bucket_group, coloc->bucket_group, n);
	c->bucket_mod = palloc(n);
	memcpy(c->bucket_mod, coloc->bucket_mod, n);
	c->slot_bucket = palloc(n);
	memcpy(c->slot_bucket, coloc->slot_bucket, n);
	return c;
}

int
kingres_shard_bucket_group(const KsColocation *coloc, int bucket)
{
	if (bucket < 0 || bucket >= coloc->max_modulus)
		return -1;
	return coloc->bucket_group[bucket];
}

/*
 * Making a layout: kingres_shard_coloc_begin() allocates it with no bucket, kingres_shard_coloc_add()
 * records one row of bucket_map, kingres_shard_coloc_finish() lists the buckets.  A row that cannot
 * belong (modulus that is not a divisor of max_modulus, remainder not below the modulus, a number
 * twice, slots that another bucket covers) is dropped: the layout then has a gap, which the routing
 * code treats as "no owner" and check_cluster() names.
 */
KsColocation *
kingres_shard_coloc_begin(int colocation_id, int bucket_count, int max_modulus,
						  int64 placement_epoch, Oid dist_type, Oid collation)
{
	KsColocation *c = palloc0(sizeof(KsColocation));
	int			i;

	if (max_modulus < 1)
		max_modulus = Max(bucket_count, 1);
	c->colocation_id = colocation_id;
	c->bucket_count = bucket_count;
	c->placement_epoch = placement_epoch;
	c->dist_type = dist_type;
	c->collation = collation;
	c->max_modulus = max_modulus;
	c->bucket_group = palloc(sizeof(int) * max_modulus);
	c->bucket_mod = palloc0(sizeof(int) * max_modulus);
	c->slot_bucket = palloc(sizeof(int) * max_modulus);
	for (i = 0; i < max_modulus; i++)
	{
		c->bucket_group[i] = -1;
		c->slot_bucket[i] = -1;
	}
	return c;
}

void
kingres_shard_coloc_add(KsColocation *c, int bucket, int modulus, int group)
{
	int			s;

	if (modulus < 1 || bucket < 0 || bucket >= modulus || c->max_modulus % modulus != 0 ||
		c->bucket_mod[bucket] != 0)
		return;
	for (s = bucket; s < c->max_modulus; s += modulus)
		if (c->slot_bucket[s] >= 0)
			return;
	for (s = bucket; s < c->max_modulus; s += modulus)
		c->slot_bucket[s] = bucket;
	c->bucket_mod[bucket] = modulus;
	c->bucket_group[bucket] = group;
}

void
kingres_shard_coloc_finish(KsColocation *c)
{
	int			b;
	int			n = 0;

	for (b = 0; b < c->max_modulus; b++)
		if (c->bucket_mod[b] > 0)
			n++;
	/*
	 * A group with no row in bucket_map at all (a catalog that is being made, or a broken one) has
	 * the buckets of a group that was never split, none of them mapped: the routing then says "no
	 * owner" instead of failing to find a bucket, as it always did.
	 */
	if (n == 0 && c->max_modulus == c->bucket_count)
	{
		for (b = 0; b < c->max_modulus; b++)
		{
			c->slot_bucket[b] = b;
			c->bucket_mod[b] = c->bucket_count;
		}
		n = c->max_modulus;
	}
	c->nbucket = n;
	c->bucket_list = palloc(sizeof(int) * Max(n, 1));
	n = 0;
	for (b = 0; b < c->max_modulus; b++)
		if (c->bucket_mod[b] > 0)
			c->bucket_list[n++] = b;
}

/*
 * The layout of the buckets 0 .. nbuckets - 1, all of modulus nbuckets (a group that was never
 * split), made by the caller: groups[b] is the group that owns bucket b, or -1.
 */
KsColocation *
kingres_shard_coloc_make(int colocation_id, int nbuckets, Oid dist_type, Oid collation,
						 const int *groups)
{
	KsColocation *c = kingres_shard_coloc_begin(colocation_id, nbuckets, nbuckets, 0, dist_type, collation);
	int			b;

	for (b = 0; b < nbuckets; b++)
		kingres_shard_coloc_add(c, b, nbuckets, groups[b]);
	kingres_shard_coloc_finish(c);
	return c;
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

bool		kingres_shard_catalog_incremental = true;

/*
 * The rows of a catalog table that this transaction wrote: the first column that is wanted of each, as an int (the colocation id)
 * or as an OID (the relation).  Returns the number of rows found.
 */
static Bitmapset *
written_ids(Oid relid, int attno, bool as_oid, List **oids)
{
	CatScan		cs;
	HeapTuple	tup;
	Bitmapset  *ids = NULL;

	catscan_begin(&cs, relid);
	while ((tup = catscan_next(&cs)) != NULL)
	{
		bool		isnull;
		Datum		d;

		if (!TransactionIdIsCurrentTransactionId(HeapTupleHeaderGetRawXmin(tup->t_data)))
			continue;
		d = heap_getattr(tup, attno, RelationGetDescr(cs.rel), &isnull);
		if (isnull)
			continue;
		if (as_oid)
			*oids = lappend_oid(*oids, DatumGetObjectId(d));
		else if (DatumGetInt32(d) >= 0)
			ids = bms_add_member(ids, DatumGetInt32(d));
	}
	catscan_end(&cs);
	return ids;
}

/*
 * Invalidate the relcache entries that the statement of this trigger can change; false when the statement is one that touches
 * every distributed table (the caller then does that).
 */
static bool
bump_invalidate_changed(FunctionCallInfo fcinfo)
{
	TriggerData *td = (TriggerData *) fcinfo->context;
	const char *rel = RelationGetRelationName(td->tg_relation);
	Oid			relid;
	Bitmapset  *cids = NULL;
	List	   *oids = NIL;
	ListCell   *lc;
	Oid			dtrel;

	if (TRIGGER_FIRED_BY_TRUNCATE(td->tg_event) ||
		RelationGetNamespace(td->tg_relation) != get_namespace_oid(KINGRES_SHARD_SCHEMA, true))
		return false;
	relid = RelationGetRelid(td->tg_relation);
	dtrel = kingres_shard_catalog_relid("dist_table");
	if (!OidIsValid(dtrel))
		return false;
	if (strcmp(rel, "dist_table") == 0)
		(void) written_ids(relid, 1, true, &oids);
	else if (strcmp(rel, "colocation") == 0 || strcmp(rel, "bucket_map") == 0)
		cids = written_ids(relid, 1, false, NULL);
	else if (strcmp(rel, "dist_schema") == 0)
		cids = written_ids(relid, 2, false, NULL);
	else
		return false;

	foreach(lc, oids)
	{
		stat_invalidations++;
		CacheInvalidateRelcacheByRelid(lfirst_oid(lc));
	}
	if (!bms_is_empty(cids))
	{
		CatScan		cs;
		HeapTuple	tup;

		catscan_begin(&cs, dtrel);
		while ((tup = catscan_next(&cs)) != NULL)
		{
			bool		isnull;
			Datum		d = heap_getattr(tup, 4, RelationGetDescr(cs.rel), &isnull);

			if (!isnull && DatumGetInt32(d) >= 0 && bms_is_member(DatumGetInt32(d), cids))
			{
				stat_invalidations++;
				CacheInvalidateRelcacheByRelid(DatumGetObjectId(heap_getattr(tup, 1, RelationGetDescr(cs.rel), &isnull)));
			}
		}
		catscan_end(&cs);
	}
	return true;
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

	/*
	 * The relcache entries of the distributed tables that the change can touch (step 12).  The statements that change one table
	 * or one group (a DDL of a schema, a move) name the rows they wrote: a row that this transaction wrote has this transaction as
	 * the inserting transaction, so one pass of the catalog table finds them without a query and without a message for every
	 * table of the database.  Everything else (a node, the settings, a truncate, or the setting turned off) invalidates every
	 * distributed table, as before.
	 */
	if (kingres_shard_catalog_incremental && bump_invalidate_changed(fcinfo))
	{
		/* done */
	}
	else
	{
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
			{
				stat_invalidations++;
				CacheInvalidateRelcacheByRelid(DatumGetObjectId(d));
			}
		}
	}

	SPI_finish();
	ks.valid = false;
	return PointerGetDatum(NULL);
}

/*
 * kingres.internal_catalog_stats(): how often this backend loaded the cache, asked the catalog for one schema table or schema, and sent a
 * relcache invalidation for a distributed table from the trigger of a catalog change.
 */
Datum
kingres_internal_catalog_stats(PG_FUNCTION_ARGS)
{
	TupleDesc	tupdesc;
	Datum		values[3];
	bool		nulls[3] = {false, false, false};

	if (get_call_result_type(fcinfo, NULL, &tupdesc) != TYPEFUNC_COMPOSITE)
		elog(ERROR, "return type must be a row type");
	values[0] = Int64GetDatum(catalog_generation);
	values[1] = Int64GetDatum(stat_lazy_lookups);
	values[2] = Int64GetDatum(stat_invalidations);
	PG_RETURN_DATUM(HeapTupleGetDatum(heap_form_tuple(tupdesc, values, nulls)));
}

/*
 * The latest committed catalog term of this node (the same read as
 * kingres_shard_map_version_now(): no cache, so no reload of the catalog and no
 * side effect on the cached sessions); -1 when there is none.
 */
int64
kingres_shard_term_now(void)
{
	Oid			relid = kingres_shard_catalog_relid("map_version");
	CatScan		cs;
	HeapTuple	tup;
	int64		term = -1;

	if (!OidIsValid(relid))
		return -1;
	catscan_begin(&cs, relid);
	tup = catscan_next(&cs);
	if (tup != NULL && RelationGetDescr(cs.rel)->natts >= 3)
	{
		bool		isnull;

		term = DatumGetInt64(heap_getattr(tup, 2, RelationGetDescr(cs.rel), &isnull));
	}
	catscan_end(&cs);
	return term;
}
