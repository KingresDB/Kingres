/*-------------------------------------------------------------------------
 *
 * kc_catalog.c
 *	  The stripe catalog: the heap table kingres.columnar_stripe, one row per
 *	  stripe, keyed by the identity of the stored content (an id in the metapage, copied with the file).
 *
 * A row of a columnar table is visible to a snapshot exactly when its stripe
 * is, and a stripe is visible when its catalog row is visible under the
 * ordinary heap rules.  So rollback, crash recovery, replication and hot
 * standby reads follow from PostgreSQL's own transaction machinery.  The
 * table is read and written here with the generic heap and index interfaces,
 * which check no privileges: nobody but the extension owner has any on it.
 *
 * Also here: the parsing, with validation, of the per-stripe statistics and
 * of a stripe's footer, both of which come from disk.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kc_catalog.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/genam.h"
#include "access/heapam.h"
#include "access/htup_details.h"
#include "access/subtrans.h"
#include "access/table.h"
#include "access/multixact.h"
#include "access/transam.h"
#include "access/xact.h"
#include "catalog/indexing.h"
#include "catalog/index.h"
#include "catalog/namespace.h"
#include "catalog/pg_am.h"
#include "catalog/pg_class.h"
#include "catalog/pg_extension.h"
#include "commands/extension.h"
#include "miscadmin.h"
#include "storage/bufmgr.h"
#include "storage/predicate.h"
#include "storage/procarray.h"
#include "storage/smgr.h"
#include "utils/builtins.h"
#include "utils/fmgroids.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"
#include "utils/syscache.h"

#include "kingres_columnar.h"

#define Natts_kc_stripe				14
#define Anum_kc_uid					1
#define Anum_kc_stripe_id			2
#define Anum_kc_first_row			3
#define Anum_kc_row_count			4
#define Anum_kc_chunk_groups		5
#define Anum_kc_natts				6
#define Anum_kc_first_block			7
#define Anum_kc_block_count			8
#define Anum_kc_blob_len			9
#define Anum_kc_raw_bytes			10
#define Anum_kc_footer_offset		11
#define Anum_kc_cid_points			12
#define Anum_kc_col_stats			13
#define Anum_kc_maintenance			14

/*
 * The snapshot the catalog is read with: the caller's snapshot copied, with
 * what the visibility check below needs besides.
 */
typedef struct KcCatSnap
{
	SnapshotData snap;			/* must be first: callers pfree() it */
	CommandId	real_cid;		/* the reader's command id (see kc_tuple_visible) */
	bool		ssi;			/* report conflicts to the serializable machinery */
} KcCatSnap;

static Oid	cached_catalog_oid = InvalidOid;
static Oid	cached_index_oid = InvalidOid;
static Oid	cached_am_oid = InvalidOid;

/* ------------------------------------------------------------------------
 * Finding the catalog objects
 * ------------------------------------------------------------------------
 */

static void
kc_resolve_catalog(void)
{
	Oid			extoid;
	Oid			nspoid;

	if (OidIsValid(cached_catalog_oid) &&
		SearchSysCacheExists1(RELOID, ObjectIdGetDatum(cached_catalog_oid)) &&
		OidIsValid(cached_index_oid) &&
		SearchSysCacheExists1(RELOID, ObjectIdGetDatum(cached_index_oid)))
		return;

	extoid = get_extension_oid(KC_EXTENSION_NAME, true);
	if (!OidIsValid(extoid))
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_OBJECT),
				 errmsg("extension \"%s\" is not installed in this database",
						KC_EXTENSION_NAME)));
	nspoid = get_extension_schema(extoid);
	cached_catalog_oid = get_relname_relid(KC_CATALOG_NAME, nspoid);
	cached_index_oid = get_relname_relid(KC_CATALOG_PKEY, nspoid);
	if (!OidIsValid(cached_catalog_oid) || !OidIsValid(cached_index_oid))
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("the stripe catalog of \"%s\" is missing",
						KC_EXTENSION_NAME)));
}

Oid
kc_catalog_oid(void)
{
	kc_resolve_catalog();
	return cached_catalog_oid;
}

Oid
kc_catalog_index_oid(void)
{
	kc_resolve_catalog();
	return cached_index_oid;
}

Oid
kc_am_oid(void)
{
	if (OidIsValid(cached_am_oid) &&
		SearchSysCacheExists1(AMOID, ObjectIdGetDatum(cached_am_oid)))
		return cached_am_oid;
	cached_am_oid = GetSysCacheOid1(AMNAME, Anum_pg_am_oid,
									CStringGetDatum(KC_AM_NAME));
	return cached_am_oid;
}

bool
kc_rel_is_columnar(Relation rel)
{
	return rel->rd_tableam == kc_get_routine();
}

/* ------------------------------------------------------------------------
 * Snapshots
 * ------------------------------------------------------------------------
 */

/*
 * The snapshot to read the catalog with, for a scan that was given snap.
 * An MVCC snapshot is copied with its command id raised so that every catalog
 * row of the running transaction is seen (a stripe is flushed at some later
 * command than the one that wrote its rows; the scan applies the finer
 * command-id rule itself, from the stripe's cid_points).  Anything else
 * (SnapshotAny of ANALYZE, say) is replaced by a current snapshot, so that
 * stripes of aborted or running writers are never read.  *is_mvcc tells
 * whether the caller's snapshot was an MVCC one.
 */
Snapshot
kc_catalog_snapshot(Snapshot snap, bool *is_mvcc)
{
	KcCatSnap  *cs = palloc0(sizeof(KcCatSnap));
	Snapshot	copy = &cs->snap;

	if (snap != NULL && snap->snapshot_type == SNAPSHOT_MVCC)
	{
		*copy = *snap;
		*is_mvcc = true;
		cs->real_cid = snap->curcid;
		cs->ssi = false;
	}
	else
	{
		Snapshot	base;

		/*
		 * A fresh snapshot, not the active one: VACUUM FULL takes its snapshot
		 * before it waits for the table lock, and a writer that committed
		 * meanwhile must not be missed by the rewrite.
		 */
		if (IsInParallelMode())
			base = GetActiveSnapshot();
		else
			base = GetLatestSnapshot();
		*copy = *base;
		*is_mvcc = false;

		/*
		 * A maintenance read (vacuum, compaction, a rewrite) treats every
		 * deletion by the running transaction as past, whatever the command.
		 */
		cs->real_cid = InvalidCommandId;
		cs->ssi = false;
	}
	copy->curcid = InvalidCommandId;

	/*
	 * Visibility checks assert that the snapshot is registered or active.
	 * The copy is private and never enters the snapshot manager, so when the
	 * original was neither (GetLatestSnapshot hands out a static one) it is
	 * marked as registered for the assertion only.
	 */
	if (copy->regd_count == 0 && copy->active_count == 0)
		copy->regd_count = 1;
	return copy;
}

/*
 * Make loads with a snapshot from kc_catalog_snapshot report the stripes they
 * skip to the serializable machinery.  Only a scan of the table (kc_scan.c)
 * asks for it: the inspection functions, the planner's estimates and the
 * maintenance paths read the stripe catalog but do not read the table, and
 * must neither create dependencies nor be affected by them.  A snapshot that
 * was not an MVCC one never reports.
 */
void
kc_catalog_snapshot_report_conflicts(Snapshot snap, bool is_mvcc)
{
	((KcCatSnap *) snap)->ssi = is_mvcc;
}

/* A fresh snapshot of everything committed, plus the running transaction. */
static Snapshot
kc_latest_snapshot(void)
{
	bool		dummy;

	return kc_catalog_snapshot(GetLatestSnapshot(), &dummy);
}

/*
 * Is a catalog row visible to the snapshot (a copy prepared by
 * kc_catalog_snapshot, with its command id raised)?  The catalog is scanned
 * with SnapshotAny and the heap visibility rules are applied here, because a
 * scan with an MVCC snapshot would take predicate locks on the pages of the
 * one index all columnar tables share, and under SERIALIZABLE make
 * transactions on unrelated tables abort each other.  The table itself has
 * its own relation-level predicate lock (kc_scan.c) and conflict checks
 * (kc_write.c, and kc_catalog_load for the stripes this check hides).
 *
 * The own transaction's insertions are all visible: the scan applies the
 * finer command-id rule from the stripe's cid_points.  The own transaction's
 * deletions follow HeapTupleSatisfiesMVCC: a stripe deleted by an earlier
 * command is gone, one deleted by the reader's own command or a later one is
 * still there.  *concurrent is set to the transaction whose stripe row was
 * hidden because that transaction had not committed before the snapshot, or
 * to InvalidTransactionId.
 */
static bool
kc_tuple_visible(HeapTuple tup, Snapshot snap, TransactionId *concurrent)
{
	HeapTupleHeader th = tup->t_data;
	TransactionId xmin = HeapTupleHeaderGetRawXmin(th);
	CommandId	real_cid = ((KcCatSnap *) snap)->real_cid;

	if (concurrent)
		*concurrent = InvalidTransactionId;
	if (!HeapTupleHeaderXminFrozen(th))
	{
		if (th->t_infomask & HEAP_XMIN_INVALID)
			return false;
		if (!TransactionIdIsCurrentTransactionId(xmin))
		{
			if (XidInMVCCSnapshot(xmin, snap))
			{
				if (concurrent)
					*concurrent = xmin;
				return false;
			}
			if (!(th->t_infomask & HEAP_XMIN_COMMITTED) &&
				!TransactionIdDidCommit(xmin))
				return false;
		}
	}
	if ((th->t_infomask & HEAP_XMAX_INVALID) ||
		HEAP_XMAX_IS_LOCKED_ONLY(th->t_infomask))
		return true;
	{
		TransactionId xmax = HeapTupleHeaderGetUpdateXid(th);

		if (TransactionIdIsCurrentTransactionId(xmax))
			return HeapTupleHeaderGetCmax(th) >= real_cid;
		if (XidInMVCCSnapshot(xmax, snap))
			return true;
		if (th->t_infomask & HEAP_XMAX_COMMITTED)
			return false;
		return !TransactionIdDidCommit(xmax);
	}
}

/* ------------------------------------------------------------------------
 * Reading
 * ------------------------------------------------------------------------
 */

static bytea *
kc_copy_bytea(Datum d)
{
	return (bytea *) PG_DETOAST_DATUM_COPY(d);
}

/*
 * The stripe catalog has the columns this build writes and reads, all of them.
 * Version 0.1 is not released and its format changes in place, so a catalog
 * made by an earlier build of it (it had one column fewer) is not read: it
 * is an error that says so, and the extension must be created again in that
 * database.  No supported database can have one.
 */
static void
kc_catalog_check_shape(TupleDesc desc)
{
	if (desc->natts != Natts_kc_stripe)
		ereport(ERROR,
				(errcode(ERRCODE_DATA_CORRUPTED),
				 errmsg("the stripe catalog has %d columns, this build uses %d",
						desc->natts, Natts_kc_stripe),
				 errhint("The catalog was made by an earlier build of version 0.1; drop and create the extension again.")));
}

static void
kc_stripe_from_tuple(TupleDesc desc, HeapTuple tup, KcStripeInfo *si)
{
	Datum		v[Natts_kc_stripe];
	bool		n[Natts_kc_stripe];
	int			i;

	kc_catalog_check_shape(desc);
	heap_deform_tuple(tup, desc, v, n);
	for (i = 0; i < Natts_kc_stripe; i++)
		if (n[i])
			ereport(ERROR,
					(errcode(ERRCODE_DATA_CORRUPTED),
					 errmsg("the stripe catalog has a null in column %d", i + 1)));

	memset(si, 0, sizeof(*si));
	si->stripe_id = (uint64) DatumGetInt64(v[Anum_kc_stripe_id - 1]);
	si->first_row = (uint64) DatumGetInt64(v[Anum_kc_first_row - 1]);
	si->row_count = (uint32) DatumGetInt32(v[Anum_kc_row_count - 1]);
	si->chunk_groups = (uint32) DatumGetInt32(v[Anum_kc_chunk_groups - 1]);
	si->natts = DatumGetInt16(v[Anum_kc_natts - 1]);
	si->first_block = (uint64) DatumGetInt64(v[Anum_kc_first_block - 1]);
	si->block_count = (uint32) DatumGetInt32(v[Anum_kc_block_count - 1]);
	si->blob_len = (uint64) DatumGetInt64(v[Anum_kc_blob_len - 1]);
	si->raw_bytes = (uint64) DatumGetInt64(v[Anum_kc_raw_bytes - 1]);
	si->footer_offset = (uint64) DatumGetInt64(v[Anum_kc_footer_offset - 1]);
	si->cid_points = kc_copy_bytea(v[Anum_kc_cid_points - 1]);
	si->col_stats = kc_copy_bytea(v[Anum_kc_col_stats - 1]);
	si->xmin = HeapTupleHeaderGetXmin(tup->t_data);
	si->own = TransactionIdIsCurrentTransactionId(HeapTupleHeaderGetXmin(tup->t_data));
	si->maintenance = DatumGetBool(v[Anum_kc_maintenance - 1]);
	si->visible_rows = si->row_count;

	/* the numbers must be self-consistent before anything is allocated */
	if ((int64) si->stripe_id <= 0 || si->row_count == 0 ||
		si->row_count > KC_STRIPE_ROWS_READ_LIMIT || si->natts < 0 ||
		si->block_count == 0 || si->blob_len == 0 ||
		si->blob_len > (uint64) si->block_count * KC_PAGE_PAYLOAD ||
		si->blob_len <= (uint64) (si->block_count - 1) * KC_PAGE_PAYLOAD ||
		si->footer_offset < sizeof(KcStripeHeader) ||
		si->footer_offset >= si->blob_len ||
		si->first_block == 0 || si->first_block >= MaxBlockNumber ||
		si->first_block + si->block_count > MaxBlockNumber ||
		VARSIZE_ANY_EXHDR(si->cid_points) % 8 != 0)
		ereport(ERROR,
				(errcode(ERRCODE_DATA_CORRUPTED),
				 errmsg("the stripe catalog row of stripe " UINT64_FORMAT " is corrupt",
						si->stripe_id)));
}

/* Is the stripe row one that compaction wrote (kc_compact)? */
static bool
kc_tuple_is_maintenance(TupleDesc desc, HeapTuple tup)
{
	bool		isnull;
	Datum		d;

	d = heap_getattr(tup, Anum_kc_maintenance, desc, &isnull);
	return !isnull && DatumGetBool(d);
}

/*
 * Report to the serializable machinery that the reader skipped a stripe
 * written by the transaction xid (the xmin of the stripe row), as heap does
 * for a tuple it skips (HeapCheckForSerializableConflictOut).  The xid in the
 * row is the id of whatever subtransaction inserted it, and the top-level id
 * only when the stripe was flushed at commit.  The serializable machinery
 * knows top-level transactions only (a subtransaction id is not in its
 * table, and the dependency would be dropped silently), so the id is mapped
 * to its top-level transaction first.  The cases:
 *
 * - an id older than the oldest snapshot xmin of this backend cannot belong
 *   to a concurrent writer, and its pg_subtrans entry may be gone (asking
 *   for it is an error): nothing to report, as in heap;
 * - the reader's own transaction is no dependency; an own subtransaction that
 *   aborted can be newer than the snapshot and so looks concurrent;
 * - a writer subtransaction that aborted left nothing to depend on (heap
 *   sees its tuple as dead).  The test is made on the id that wrote the row,
 *   before the mapping, because the top-level transaction goes on.
 */
static void
kc_report_conflict_out(Relation rel, TransactionId xid, Snapshot snap)
{
	TransactionId top;

	if (!TransactionIdIsNormal(xid) || TransactionIdPrecedes(xid, TransactionXmin))
		return;
	if (TransactionIdEquals(xid, GetTopTransactionIdIfAny()))
		return;
	if (!TransactionIdIsInProgress(xid) && !TransactionIdDidCommit(xid))
		return;
	top = SubTransGetTopmostTransaction(xid);
	if (TransactionIdPrecedes(top, TransactionXmin) ||
		TransactionIdEquals(top, GetTopTransactionIdIfAny()))
		return;
	CheckForSerializableConflictOut(rel, top, snap);
}

/*
 * All stripes of rel visible to snap (already prepared by
 * kc_catalog_snapshot), in stripe id order.
 */
KcStripeInfo *
kc_catalog_load(Relation rel, Snapshot snap, int *nstripes)
{
	Relation	cat = table_open(kc_catalog_oid(), AccessShareLock);
	ScanKeyData key;
	SysScanDesc scan;
	HeapTuple	tup;
	KcStripeInfo *res = NULL;
	int			n = 0;
	int			cap = 0;

	ScanKeyInit(&key, Anum_kc_uid, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) kc_rel_ident(rel)->uid));
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true, SnapshotAny, 1, &key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		TransactionId concurrent;

		if (!kc_tuple_visible(tup, snap, &concurrent))
		{
			/*
			 * A stripe hidden because its writer had not committed before
			 * the snapshot is a write the reader did not see: under
			 * SERIALIZABLE the reader is before that writer, as heap records
			 * for a tuple it skips.  The writer's own check
			 * (kc_flush_buf) covers the readers whose lock came later.
			 *
			 * A stripe that only copies rows (compaction) hides no row from the
			 * reader: the rows are in the stripes it replaced, which the reader
			 * still sees, so there is no dependency on its writer.
			 */
			if (((KcCatSnap *) snap)->ssi && TransactionIdIsValid(concurrent) &&
				!kc_tuple_is_maintenance(RelationGetDescr(cat), tup))
				kc_report_conflict_out(rel, concurrent, snap);
			continue;
		}
		if (n == cap)
		{
			cap = cap ? cap * 2 : 16;
			res = res ? repalloc(res, sizeof(KcStripeInfo) * cap)
				: palloc(sizeof(KcStripeInfo) * cap);
		}
		kc_stripe_from_tuple(RelationGetDescr(cat), tup, &res[n]);
		n++;
	}
	systable_endscan(scan);
	table_close(cat, AccessShareLock);
	*nstripes = n;
	return res;
}


/*
 * Does every stripe row of rel's content read the same to every snapshot
 * that exists or will exist?  True when each row is either dead (written by a
 * transaction that aborted, or deleted by one that committed before
 * oldest_xmin) or live and written by a transaction that committed before
 * oldest_xmin.  A rewrite that keeps the content (VACUUM FULL) may then give
 * its stripe rows a frozen creator, which every snapshot sees, so that
 * snapshots taken before the rewrite read the new file like the old one.
 * A row in progress, newer, or deleted recently makes the answer no: some
 * older snapshot may be meant to see a different set.
 */
bool
kc_catalog_all_visible(Relation rel, TransactionId oldest_xmin)
{
	Relation	cat = table_open(kc_catalog_oid(), AccessShareLock);
	ScanKeyData key;
	SysScanDesc scan;
	HeapTuple	tup;
	bool		result = true;

	ScanKeyInit(&key, Anum_kc_uid, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) kc_rel_ident(rel)->uid));
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true, SnapshotAny, 1, &key);
	while (result && HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		HeapTupleHeader th = tup->t_data;
		TransactionId xmin = HeapTupleHeaderGetRawXmin(th);

		if (!HeapTupleHeaderXminFrozen(th))
		{
			if (th->t_infomask & HEAP_XMIN_INVALID)
				continue;
			if (TransactionIdIsCurrentTransactionId(xmin) ||
				TransactionIdIsInProgress(xmin))
			{
				result = false;
				break;
			}
			if (!TransactionIdDidCommit(xmin))
				continue;		/* aborted: dead */
			if (!TransactionIdPrecedes(xmin, oldest_xmin))
			{
				result = false;
				break;
			}
		}
		if (!(th->t_infomask & HEAP_XMAX_INVALID) &&
			!HEAP_XMAX_IS_LOCKED_ONLY(th->t_infomask))
		{
			TransactionId xmax = HeapTupleHeaderGetUpdateXid(th);

			if (TransactionIdIsCurrentTransactionId(xmax) ||
				TransactionIdIsInProgress(xmax))
				result = false;
			else if (TransactionIdDidCommit(xmax))
			{
				/* deleted: dead only when nobody can still see it */
				if (!TransactionIdPrecedes(xmax, oldest_xmin))
					result = false;
			}
		}
	}
	systable_endscan(scan);
	table_close(cat, AccessShareLock);
	return result;
}
/*
 * Totals over the stripes visible to snap (any snapshot: it is prepared
 * here).  Used by vacuum, the estimates and columnar_info.
 */
uint64
kc_catalog_row_total(Relation rel, Snapshot snap, uint64 *stripes,
					 uint64 *groups, uint64 *raw_bytes)
{
	bool		mvcc;
	Snapshot	cs = kc_catalog_snapshot(snap, &mvcc);
	int			n;
	int			i;
	KcStripeInfo *s = kc_catalog_load(rel, cs, &n);
	uint64		rows = 0;

	if (stripes)
		*stripes = n;
	if (groups)
		*groups = 0;
	if (raw_bytes)
		*raw_bytes = 0;
	for (i = 0; i < n; i++)
	{
		rows += s[i].row_count;
		if (groups)
			*groups += s[i].chunk_groups;
		if (raw_bytes)
			*raw_bytes += s[i].raw_bytes;
	}
	pfree(cs);
	if (s)
		pfree(s);
	return rows;
}

/* ------------------------------------------------------------------------
 * Writing
 * ------------------------------------------------------------------------
 */

void
kc_catalog_insert(Relation rel, const KcStripeInfo *si, const char *cid_points,
				  int cid_len, const char *col_stats, int stats_len, bool frozen,
				  bool maintenance)
{
	Relation	cat = table_open(kc_catalog_oid(), RowExclusiveLock);
	Datum		v[Natts_kc_stripe];
	bool		n[Natts_kc_stripe];
	bytea	   *cp;
	bytea	   *cs;
	HeapTuple	tup;

	kc_catalog_check_shape(RelationGetDescr(cat));
	cp = palloc(VARHDRSZ + cid_len);
	SET_VARSIZE(cp, VARHDRSZ + cid_len);
	memcpy(VARDATA(cp), cid_points, cid_len);
	cs = palloc(VARHDRSZ + stats_len);
	SET_VARSIZE(cs, VARHDRSZ + stats_len);
	memcpy(VARDATA(cs), col_stats, stats_len);

	memset(n, 0, sizeof(n));
	v[Anum_kc_uid - 1] = Int64GetDatum((int64) kc_rel_ident(rel)->uid);
	v[Anum_kc_stripe_id - 1] = Int64GetDatum((int64) si->stripe_id);
	v[Anum_kc_first_row - 1] = Int64GetDatum((int64) si->first_row);
	v[Anum_kc_row_count - 1] = Int32GetDatum((int32) si->row_count);
	v[Anum_kc_chunk_groups - 1] = Int32GetDatum((int32) si->chunk_groups);
	v[Anum_kc_natts - 1] = Int16GetDatum((int16) si->natts);
	v[Anum_kc_first_block - 1] = Int64GetDatum((int64) si->first_block);
	v[Anum_kc_block_count - 1] = Int32GetDatum((int32) si->block_count);
	v[Anum_kc_blob_len - 1] = Int64GetDatum((int64) si->blob_len);
	v[Anum_kc_raw_bytes - 1] = Int64GetDatum((int64) si->raw_bytes);
	v[Anum_kc_footer_offset - 1] = Int64GetDatum((int64) si->footer_offset);
	v[Anum_kc_cid_points - 1] = PointerGetDatum(cp);
	v[Anum_kc_col_stats - 1] = PointerGetDatum(cs);
	v[Anum_kc_maintenance - 1] = BoolGetDatum(maintenance);

	tup = heap_form_tuple(RelationGetDescr(cat), v, n);
	if (!frozen)
		CatalogTupleInsert(cat, tup);
	else
	{
		/*
		 * A row of a rewrite that kept what every snapshot sees: its creator
		 * is frozen, so snapshots older than the rewrite see the stripe too.
		 * Only the new file refers to this identity until the rewrite commits,
		 * and a rewrite that aborts leaves rows that reconciliation removes.
		 */
		Relation	idx = index_open(kc_catalog_index_oid(), RowExclusiveLock);
		IndexInfo  *ii = BuildIndexInfo(idx);
		Datum		iv[2];
		bool		inull[2] = {false, false};

		heap_insert(cat, tup, GetCurrentCommandId(true), HEAP_INSERT_FROZEN, NULL);
		iv[0] = v[Anum_kc_uid - 1];
		iv[1] = v[Anum_kc_stripe_id - 1];
		(void) index_insert(idx, iv, inull, &tup->t_self, cat, UNIQUE_CHECK_YES,
							false, ii);
		index_close(idx, RowExclusiveLock);
	}
	heap_freetuple(tup);
	pfree(cp);
	pfree(cs);
	table_close(cat, RowExclusiveLock);
}

/* Delete the rows of one stored content that are visible (to us, now). */
void
kc_catalog_delete_uid(uint64 uid)
{
	Relation	cat = table_open(kc_catalog_oid(), RowExclusiveLock);
	ScanKeyData key;
	SysScanDesc scan;
	HeapTuple	tup;
	Snapshot	vsnap;

	ScanKeyInit(&key, Anum_kc_uid, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) uid));
	vsnap = kc_latest_snapshot();
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true,
							  SnapshotAny, 1, &key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
		if (kc_tuple_visible(tup, vsnap, NULL))
			CatalogTupleDelete(cat, &tup->t_self);
	systable_endscan(scan);
	table_close(cat, RowExclusiveLock);
}

/* Delete one stripe's row; returns the number of rows deleted. */
int
kc_catalog_delete_stripe(uint64 uid, uint64 stripe_id)
{
	Relation	cat = table_open(kc_catalog_oid(), RowExclusiveLock);
	ScanKeyData key[2];
	SysScanDesc scan;
	HeapTuple	tup;
	Snapshot	vsnap = kc_latest_snapshot();
	int			n = 0;

	ScanKeyInit(&key[0], Anum_kc_uid, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) uid));
	ScanKeyInit(&key[1], Anum_kc_stripe_id, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) stripe_id));
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true,
							  SnapshotAny, 2, key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		if (!kc_tuple_visible(tup, vsnap, NULL))
			continue;
		CatalogTupleDelete(cat, &tup->t_self);
		n++;
	}
	systable_endscan(scan);
	table_close(cat, RowExclusiveLock);
	return n;
}

/* ------------------------------------------------------------------------
 * Reconciliation: remove the rows of files that no relation uses any more
 * ------------------------------------------------------------------------
 */

static int
kc_u64_cmp(const void *a, const void *b)
{
	uint64		x = *(const uint64 *) a;
	uint64		y = *(const uint64 *) b;

	return (x > y) - (x < y);
}

/*
 * The identity stored in block 0 of the file a pg_class row names, or 0 when
 * the file does not exist yet or is not one of ours.  Read through the buffer
 * manager without a lock on the relation: the relation may belong to a
 * transaction that has not committed.
 */
static uint64
kc_uid_of_file(Form_pg_class cl, bool *unknown)
{
	RelFileLocator loc;
	SMgrRelation srel;
	PGIOAlignedBlock buf;
	KcMetaData *m;
	KcMetaData	copy;

	loc.spcOid = OidIsValid(cl->reltablespace) ? cl->reltablespace : MyDatabaseTableSpace;
	loc.dbOid = MyDatabaseId;
	loc.relNumber = cl->relfilenode;
	srel = smgropen(loc, INVALID_PROC_NUMBER);
	if (!smgrexists(srel, MAIN_FORKNUM) || smgrnblocks(srel, MAIN_FORKNUM) == 0)
		return 0;

	/*
	 * Read the file itself, not through the buffer manager: the identity is
	 * written once, when the file is created, straight to the file, and a
	 * damaged page of one table must not make a VACUUM or a DROP of another
	 * one fail.  A page that is not a valid metapage makes the identities
	 * unknown, and the caller then deletes nothing.
	 */
	smgrread(srel, MAIN_FORKNUM, 0, buf.data);
	m = (KcMetaData *) PageGetContents((Page) buf.data);
	copy = *m;
	copy.crc = 0;
	if (PageIsNew((Page) buf.data) || PageGetSpecialSize((Page) buf.data) != 0 ||
		m->magic != KC_META_MAGIC ||
		(uint32) kc_crc32c((const char *) &copy, sizeof(copy)) != m->crc)
	{
		*unknown = true;
		return 0;
	}
	return m->rel_uid;
}

/*
 * A table access method has no callback at DROP, and a rewrite leaves the old
 * file's rows behind, so the rows whose stored content no relation owns any
 * more are removed here: after a drop, at the end of VACUUM, and by
 * columnar_check.
 *
 * The catalog is read first, with a current snapshot; the identities of the
 * live files are then read from pg_class with a dirty snapshot, so the
 * relations of transactions that have not committed yet (a table being
 * created, or rewritten, by another session) keep their rows, and a table
 * that commits after the catalog snapshot cannot lose rows it was not seen to
 * have.  Returns the number of rows deleted.
 */
static int
kc_reconcile_ex(bool dry)
{
	Relation	pgclass;
	SysScanDesc scan;
	HeapTuple	tup;
	SnapshotData dirty;
	uint64	   *live = NULL;
	int			nlive = 0;
	int			caplive = 0;
	Oid			amoid = kc_am_oid();
	Relation	cat;
	ScanKeyData key;
	int			ndel = 0;
	Snapshot	catsnap = kc_latest_snapshot();
	bool		unknown = false;

	pgclass = table_open(RelationRelationId, AccessShareLock);
	InitDirtySnapshot(dirty);
	scan = systable_beginscan(pgclass, InvalidOid, false, &dirty, 0, NULL);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		Form_pg_class cl = (Form_pg_class) GETSTRUCT(tup);
		uint64		uid;

		if (!OidIsValid(amoid) || cl->relam != amoid ||
			!OidIsValid(cl->relfilenode) ||
			(cl->relkind != RELKIND_RELATION && cl->relkind != RELKIND_MATVIEW))
			continue;
		uid = kc_uid_of_file(cl, &unknown);
		if (uid == 0)
			continue;
		if (nlive == caplive)
		{
			caplive = caplive ? caplive * 2 : 64;
			live = live ? repalloc(live, sizeof(uint64) * caplive)
				: palloc(sizeof(uint64) * caplive);
		}
		live[nlive++] = uid;
	}
	systable_endscan(scan);
	table_close(pgclass, AccessShareLock);
	if (unknown)
	{
		ereport(LOG,
				(errmsg("columnar stripe catalog cleanup skipped: a columnar relation has an unreadable metapage")));
		if (live)
			pfree(live);
		return 0;
	}
	if (nlive > 1)
		qsort(live, nlive, sizeof(uint64), kc_u64_cmp);

	cat = table_open(kc_catalog_oid(), RowExclusiveLock);
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true,
							  SnapshotAny, 0, &key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		bool		isnull;
		uint64		uid = (uint64) DatumGetInt64(heap_getattr(tup, Anum_kc_uid,
															  RelationGetDescr(cat),
															  &isnull));

		if (isnull || !kc_tuple_visible(tup, catsnap, NULL))
			continue;
		if (nlive == 0 || bsearch(&uid, live, nlive, sizeof(uint64), kc_u64_cmp) == NULL)
		{
			if (!dry)
				CatalogTupleDelete(cat, &tup->t_self);
			ndel++;
		}
	}
	systable_endscan(scan);
	table_close(cat, RowExclusiveLock);
	if (live)
		pfree(live);
	return ndel;
}


int
kc_catalog_reconcile(void)
{
	return kc_reconcile_ex(false);
}

int
kc_catalog_count_orphans(void)
{
	return kc_reconcile_ex(true);
}

/* ------------------------------------------------------------------------
 * Statistics of a stripe, and the footer
 * ------------------------------------------------------------------------
 */

static pg_noreturn void
kc_stats_corrupt(uint64 stripe_id, const char *what)
{
	ereport(ERROR,
			(errcode(ERRCODE_DATA_CORRUPTED),
			 errmsg("the statistics of columnar stripe " UINT64_FORMAT " are corrupt: %s",
					stripe_id, what)));
}

/*
 * Parse the col_stats column of a stripe: for each of the ncols columns a
 * KcStatDisk, then the minimum and maximum bytes.  Pointers in out refer to
 * the catalog row's copy, which lives as long as the stripe list.
 */
void
kc_stripe_stats_parse(const KcStripeInfo *si, int ncols, const KcTypeInfo *ti,
						 KcColStat *out)
{
	const char *p = VARDATA_ANY(si->col_stats);
	Size		left = VARSIZE_ANY_EXHDR(si->col_stats);
	int			c;

	for (c = 0; c < ncols; c++)
	{
		KcStatDisk	d;

		if (c >= si->natts)
		{
			/* a column added after the stripe was written: nothing known */
			memset(&out[c], 0, sizeof(KcColStat));
			continue;
		}
		if (left < sizeof(KcStatDisk))
			kc_stats_corrupt(si->stripe_id, "truncated");
		memcpy(&d, p, sizeof(d));
		p += sizeof(d);
		left -= sizeof(d);
		if ((d.flags & ~(KC_CHUNK_HAS_NULLS | KC_CHUNK_HAS_MINMAX)) != 0 ||
			d.min_len > KC_MAX_MINMAX_BYTES || d.max_len > KC_MAX_MINMAX_BYTES ||
			d.nnulls > si->row_count ||
			(uint32) d.min_len + d.max_len > left)
			kc_stats_corrupt(si->stripe_id, "bad entry");
		out[c].known = true;
		out[c].nnulls = d.nnulls;
		/*
		 * Minimum and maximum are valid only for the ordering they were
		 * computed with: the column's type and collation must be the same.
		 * (A binary-coercible ALTER COLUMN TYPE keeps old stripes.)  The null
		 * count does not depend on either.
		 */
		out[c].has_minmax = (d.flags & KC_CHUNK_HAS_MINMAX) != 0 &&
			c < ti->natts && d.typid == ti->cols[c].typid &&
			d.collation == ti->cols[c].collation;
		out[c].min_len = d.min_len;
		out[c].max_len = d.max_len;
		out[c].min = p;
		out[c].max = p + d.min_len;
		p += d.min_len + d.max_len;
		left -= d.min_len + d.max_len;
	}
}

/*
 * Read and validate the header and footer of a stripe.  Everything that is
 * later used as an offset, a length or a count is checked here.
 */
void
kc_footer_load(Relation rel, const KcStripeInfo *si, KcFooter *f)
{
	KcStripeHeader h;
	char	   *raw;
	uint64		flen;
	const char *p;
	const char *end;
	uint32		g;
	int			c;
	int			ncols = si->natts;
	uint64		nrows_sum = 0;

	if (si->first_block + si->block_count > (uint64) RelationGetNumberOfBlocks(rel))
		kc_corrupt(rel, si->stripe_id, "the stripe lies beyond the end of the file");

	kc_stripe_read(rel, si, 0, sizeof(h), (char *) &h);
	if (h.magic != KC_STRIPE_MAGIC || h.version != KC_STRIPE_VERSION)
		kc_corrupt(rel, si->stripe_id, "bad stripe header");
	if (h.stripe_id != si->stripe_id || h.ncols != ncols ||
		h.nrows != si->row_count || h.ngroups != si->chunk_groups ||
		h.footer_off != si->footer_offset || h.blob_len != si->blob_len)
		kc_corrupt(rel, si->stripe_id, "the stripe header disagrees with the catalog");
	if (h.footer_off > h.blob_len || h.footer_len != h.blob_len - h.footer_off)
		kc_corrupt(rel, si->stripe_id, "the footer does not end the stripe");
	flen = h.footer_len;
	if (flen < (uint64) h.ngroups * 4 || flen > (uint64) 1 << 30)
		kc_corrupt(rel, si->stripe_id, "implausible footer size");

	/*
	 * Every group takes its row count and one entry per column in the footer:
	 * the arrays below are sized from what the footer really holds, never from
	 * a count that a damaged stripe header could inflate.
	 */
	if ((uint64) h.ngroups * (4 + (uint64) ncols * sizeof(KcChunkDisk)) > flen)
		kc_corrupt(rel, si->stripe_id, "the footer is too small for its chunk groups");

	raw = MemoryContextAllocHuge(CurrentMemoryContext, flen);
	kc_stripe_read(rel, si, h.footer_off, flen, raw);
	if (kc_crc32c(raw, flen) != (uint64) h.footer_crc)
		kc_corrupt(rel, si->stripe_id, "footer checksum mismatch");

	f->ncols = ncols;
	f->ngroups = h.ngroups;
	f->raw = raw;
	f->group_rows = palloc(sizeof(uint32) * (h.ngroups ? h.ngroups : 1));
	f->chunks = palloc(sizeof(KcChunkDisk) * ((Size) h.ngroups * ncols + 1));
	f->mins = palloc(sizeof(char *) * ((Size) h.ngroups * ncols + 1));
	f->maxs = palloc(sizeof(char *) * ((Size) h.ngroups * ncols + 1));

	p = raw;
	end = raw + flen;
	for (g = 0; g < h.ngroups; g++)
	{
		uint32		gr;

		if (end - p < 4)
			kc_corrupt(rel, si->stripe_id, "footer is truncated");
		memcpy(&gr, p, 4);
		p += 4;
		if (gr == 0 || gr > KC_MAX_CHUNK_ROWS)
			kc_corrupt(rel, si->stripe_id, "chunk group with a bad row count");
		f->group_rows[g] = gr;
		nrows_sum += gr;
		for (c = 0; c < ncols; c++)
		{
			KcChunkDisk *cd = &f->chunks[(Size) g * ncols + c];

			if ((uint64) (end - p) < sizeof(KcChunkDisk))
				kc_corrupt(rel, si->stripe_id, "footer is truncated");
			memcpy(cd, p, sizeof(KcChunkDisk));
			p += sizeof(KcChunkDisk);
			if (cd->min_len > KC_MAX_MINMAX_BYTES ||
				cd->max_len > KC_MAX_MINMAX_BYTES ||
				(uint64) cd->min_len + cd->max_len > (uint64) (end - p))
				kc_corrupt(rel, si->stripe_id, "chunk statistics do not fit in the footer");
			if (cd->off < sizeof(KcStripeHeader) ||
				cd->off > h.footer_off ||
				cd->comp_len > h.footer_off - cd->off)
				kc_corrupt(rel, si->stripe_id, "chunk lies outside the stripe body");
			if (cd->codec > KC_CODEC_ZSTD)
				kc_corrupt(rel, si->stripe_id, "unknown codec id");
			f->mins[(Size) g * ncols + c] = p;
			f->maxs[(Size) g * ncols + c] = p + cd->min_len;
			p += cd->min_len + cd->max_len;
		}
	}
	if (p != end)
		kc_corrupt(rel, si->stripe_id, "footer has trailing bytes");
	if (nrows_sum != si->row_count)
		kc_corrupt(rel, si->stripe_id, "chunk group rows do not add up to the stripe's rows");
}

void
kc_footer_free(KcFooter *f)
{
	if (f->raw)
	{
		pfree(f->raw);
		pfree(f->group_rows);
		pfree(f->chunks);
		pfree(f->mins);
		pfree(f->maxs);
	}
	memset(f, 0, sizeof(*f));
}

/*
 * Does the catalog hold any row, live or dead, for this identity?  A new
 * file draws its identity at random; the draw is repeated when it hits one
 * that is in use, so that two stored contents never share a key.
 */
bool
kc_catalog_uid_in_use(uint64 uid)
{
	Relation	cat = table_open(kc_catalog_oid(), AccessShareLock);
	ScanKeyData key;
	SysScanDesc scan;
	bool		used;

	ScanKeyInit(&key, Anum_kc_uid, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) uid));
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true, SnapshotAny, 1, &key);
	used = HeapTupleIsValid(systable_getnext(scan));
	systable_endscan(scan);
	table_close(cat, AccessShareLock);
	return used;
}
