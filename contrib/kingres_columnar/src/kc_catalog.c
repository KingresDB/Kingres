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
#include "storage/lmgr.h"
#include "storage/predicate.h"
#include "storage/procarray.h"
#include "storage/smgr.h"
#include "utils/builtins.h"
#include "utils/fmgroids.h"
#include "utils/inval.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/relcache.h"
#include "utils/snapmgr.h"
#include "utils/syscache.h"

#include "kingres_columnar.h"

#define Natts_kc_stripe_v01			14	/* the catalog of 0.1 */
#define Natts_kc_stripe				15	/* the catalog of 0.2: + reserved */
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
#define Anum_kc_reserved			15

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

/*
 * Whether an access method is this extension's, decided the way the relation
 * cache decides it for a table (rd_tableam == kc_get_routine()): by what the
 * handler of the access method returns, not by the name of the access method.
 * A superuser may create a second access method over the handler
 * (CREATE ACCESS METHOD columnar2 TYPE TABLE HANDLER kingres.columnar_handler),
 * and a table of it is a columnar table for every read and write; the name
 * "columnar" would leave it out of the check, the cleanup of the stripe
 * catalog and the planner's path (C11-02).
 *
 * KC_AM_UNKNOWN: there is no such access method for this transaction's
 * catalog snapshot (a relation of a transaction that has not committed, the
 * invalid oid).  A caller that deletes things on the strength of the answer
 * must treat it as "cannot say" and delete nothing.
 */
/*
 * The answers are kept for the planner's sake (every base relation of every
 * query asks), a few access methods at most, and dropped whenever the access
 * method or the function catalog changes (a function can be replaced under
 * the same oid, an access method dropped and its oid used again).  Only
 * definite answers are kept: "unknown" depends on what this transaction can
 * see.
 */
#define KC_AM_CACHE_SIZE 8
static struct
{
	Oid			amoid;
	KcAmClass	cls;
}			kc_am_cache[KC_AM_CACHE_SIZE];
static int	kc_am_cache_n = 0;
static int	kc_am_cache_next = 0;

static void
kc_am_cache_inval(Datum arg, int cacheid, uint32 hashvalue)
{
	kc_am_cache_n = 0;
	kc_am_cache_next = 0;
}

void
kc_am_cache_init(void)
{
	CacheRegisterSyscacheCallback(AMOID, kc_am_cache_inval, (Datum) 0);
	CacheRegisterSyscacheCallback(PROCOID, kc_am_cache_inval, (Datum) 0);
}

static bool
kc_am_class_cached(Oid amoid, KcAmClass *cls)
{
	int			i;

	for (i = 0; i < kc_am_cache_n; i++)
		if (kc_am_cache[i].amoid == amoid)
		{
			*cls = kc_am_cache[i].cls;
			return true;
		}
	return false;
}

KcAmClass
kc_am_class(Oid amoid)
{
	HeapTuple	tup;
	Form_pg_am	am;
	KcAmClass	result;

	if (!OidIsValid(amoid))
		return KC_AM_UNKNOWN;
	if (kc_am_class_cached(amoid, &result))
		return result;
	tup = SearchSysCache1(AMOID, ObjectIdGetDatum(amoid));
	if (!HeapTupleIsValid(tup))
		return KC_AM_UNKNOWN;
	am = (Form_pg_am) GETSTRUCT(tup);
	if (am->amtype != AMTYPE_TABLE || !OidIsValid(am->amhandler))
		result = KC_AM_OTHER;
	else
		result = (GetTableAmRoutine(am->amhandler) == kc_get_routine()) ?
			KC_AM_COLUMNAR : KC_AM_OTHER;
	ReleaseSysCache(tup);
	kc_am_cache[kc_am_cache_next].amoid = amoid;
	kc_am_cache[kc_am_cache_next].cls = result;
	kc_am_cache_next = (kc_am_cache_next + 1) % KC_AM_CACHE_SIZE;
	if (kc_am_cache_n < KC_AM_CACHE_SIZE)
		kc_am_cache_n++;
	return result;
}

/*
 * kc_am_class for the loops over pg_class of the orphan step and the check:
 * the handler of an access method that is not ours is code of somebody else,
 * and an error it raises must not end the call.  The handler is called in a
 * subtransaction (only when the answer is not cached, and where the server
 * lets code begin one); an error other than a cancel, a shutdown or running
 * out of memory leaves the access method "unknown", which makes the orphan
 * step stand down and say so (C12-01) and leaves the relation out of the list
 * of tables.
 */
/*
 * The access methods whose handler raised an error since the entry point of
 * the running call (the check, the reconcile, a vacuum) began: each is called
 * once per call, however many tables of it the passes over pg_class meet, and
 * its error is logged once.  Cleared by kc_am_failed_reset at the start of every
 * entry point, and when an access method or a function changes.  More than
 * KC_AM_FAILED_MAX broken access methods are called again when they come up.
 */
#define KC_AM_FAILED_MAX 16
static Oid	kc_am_failed[KC_AM_FAILED_MAX];
static int	kc_am_failed_n = 0;

void
kc_am_failed_reset(void)
{
	kc_am_failed_n = 0;
}

static KcAmClass
kc_am_class_guarded(Oid amoid)
{
	KcAmClass	cls;
	MemoryContext oldcxt = CurrentMemoryContext;
	ResourceOwner oldowner = CurrentResourceOwner;

	int			i;

	if (!OidIsValid(amoid))
		return KC_AM_UNKNOWN;
	if (kc_am_class_cached(amoid, &cls))
		return cls;
	if (!kc_subxact_possible())
		return kc_am_class(amoid);

	/* a handler that failed during this call is not called again (C13-01) */
	for (i = 0; i < kc_am_failed_n; i++)
		if (kc_am_failed[i] == amoid)
			return KC_AM_UNKNOWN;

	BeginInternalSubTransaction(NULL);
	MemoryContextSwitchTo(oldcxt);
	PG_TRY();
	{
		cls = kc_am_class(amoid);
		ReleaseCurrentSubTransaction();
		MemoryContextSwitchTo(oldcxt);
		CurrentResourceOwner = oldowner;
	}
	PG_CATCH();
	{
		ErrorData  *ed;

		MemoryContextSwitchTo(oldcxt);
		ed = CopyErrorData();
		FlushErrorState();
		RollbackAndReleaseCurrentSubTransaction();
		MemoryContextSwitchTo(oldcxt);
		CurrentResourceOwner = oldowner;
		if (ERRCODE_TO_CATEGORY(ed->sqlerrcode) == ERRCODE_TO_CATEGORY(ERRCODE_OPERATOR_INTERVENTION) ||
			ed->sqlerrcode == ERRCODE_OUT_OF_MEMORY)
			ReThrowError(ed);
		ereport(LOG,
				(errmsg("columnar: the handler of access method %u raised an error and the access method cannot be classified: %s",
						amoid, ed->message)));
		FreeErrorData(ed);
		cls = KC_AM_UNKNOWN;
		if (kc_am_failed_n < KC_AM_FAILED_MAX)
			kc_am_failed[kc_am_failed_n++] = amoid;
	}
	PG_END_TRY();
	return cls;
}

/*
 * The answer for the last access method kept, for a loop over pg_class.  With
 * memo->guard a handler that is not cached is called in a subtransaction
 * (kc_am_class_guarded); a caller says so only where a subtransaction may be
 * begun: the check (which refuses where it cannot), a statement at the top
 * level.  A function called at the start of a subtransaction, which this module
 * cannot see, must not begin one.
 */
KcAmClass
kc_am_class_memo(Oid amoid, KcAmMemo *memo)
{
	if (!OidIsValid(memo->amoid) || memo->amoid != amoid)
	{
		memo->cls = memo->guard ? kc_am_class_guarded(amoid) : kc_am_class(amoid);
		memo->amoid = amoid;
	}
	return memo->cls;
}

bool
kc_am_is_columnar(Oid amoid)
{
	return kc_am_class(amoid) == KC_AM_COLUMNAR;
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
/*
 * A private copy of a snapshot, with its own copy of the lists of running
 * transactions.  The snapshot GetLatestSnapshot() hands out is a static one
 * whose arrays the next call overwrites: a shallow copy kept across two calls
 * (the orphan step takes one before the scan of pg_class and one after it)
 * would see the second one's running transactions with the first one's
 * bounds, and call a transaction that committed in between "still running"
 * for one snapshot and "committed" for the other (C11-01).
 */
static KcCatSnap *
kc_catsnap_copy(const SnapshotData *base)
{
	Size		nx = (Size) (base->xip ? base->xcnt : 0) * sizeof(TransactionId);
	Size		ns = (Size) (base->subxip ? base->subxcnt : 0) * sizeof(TransactionId);
	KcCatSnap  *cs = palloc0(sizeof(KcCatSnap) + nx + ns);
	char	   *p = (char *) (cs + 1);

	cs->snap = *base;
	if (nx > 0)
	{
		cs->snap.xip = (TransactionId *) p;
		memcpy(p, base->xip, nx);
		p += nx;
	}
	if (ns > 0)
	{
		cs->snap.subxip = (TransactionId *) p;
		memcpy(p, base->subxip, ns);
	}
	return cs;
}

Snapshot
kc_catalog_snapshot(Snapshot snap, bool *is_mvcc)
{
	KcCatSnap  *cs;
	Snapshot	copy;

	if (snap != NULL && snap->snapshot_type == SNAPSHOT_MVCC)
	{
		cs = kc_catsnap_copy(snap);
		copy = &cs->snap;
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
		cs = kc_catsnap_copy(base);
		copy = &cs->snap;
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

/*
 * A fresh snapshot of everything committed, plus the running transaction: the
 * maintenance rule, whatever the isolation level of the caller.  The check
 * functions (columnar_check, columnar_check_clean) read the stripe catalog with
 * it (C10-01).
 */
Snapshot
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
 * Version 0.1 is not released and its format changes in place; the one earlier
 * shape that databases may still have is the catalog of the build before the one
 * that added the column "maintenance", with 13 columns.  Nothing is read or
 * written through a catalog of another shape, but the error says how to repair
 * it without losing anything: adding the column with its default makes every
 * table readable at once, the old rows being, by definition, rows that are not
 * copies made by compaction.  A hint to drop and create the extension again
 * would drop the tables with their data (their access method is part of the
 * extension), so it is never given.
 */
static int
kc_catalog_check_shape(Relation cat)
{
	int			natts = RelationGetDescr(cat)->natts;

	/*
	 * Both shapes are served: the catalog of 0.2 (the column "reserved" at the
	 * end), and the catalog of 0.1, which a database has between the
	 * replacement of the library and ALTER EXTENSION kingres_columnar UPDATE.
	 * With the catalog of 0.1 every row is a written stripe and nothing is ever
	 * reserved (kc_catalog_has_reserved is false).
	 */
	if (natts == Natts_kc_stripe || natts == Natts_kc_stripe_v01)
		return natts;
	if (natts == Natts_kc_stripe_v01 - 1)
		ereport(ERROR,
				(errcode(ERRCODE_DATA_CORRUPTED),
				 errmsg("the stripe catalog has %d columns, this build uses %d (0.1) or %d (0.2)",
						natts, Natts_kc_stripe_v01, Natts_kc_stripe),
				 errhint("The catalog was made by an earlier build of version 0.1. As the owner of the extension, run: ALTER TABLE %s.%s ADD COLUMN maintenance boolean NOT NULL DEFAULT false. Nothing is lost.",
						 quote_identifier(get_namespace_name(RelationGetNamespace(cat))),
						 quote_identifier(RelationGetRelationName(cat)))));
	ereport(ERROR,
			(errcode(ERRCODE_DATA_CORRUPTED),
			 errmsg("the stripe catalog has %d columns, this build uses %d (0.1) or %d (0.2)",
					natts, Natts_kc_stripe_v01, Natts_kc_stripe),
			 errhint("The catalog does not belong to this build and cannot be repaired by it; restore it from a backup of the database.")));
	return natts;				/* not reached */
}

/*
 * Does the stripe catalog of this database have the column "reserved", that
 * is, is the SQL side of the extension at version 0.2?  Only then may a file
 * be made as format version 3 and a row number be reserved.
 */
bool
kc_catalog_has_reserved(void)
{
	Relation	cat = table_open(kc_catalog_oid(), AccessShareLock);
	int			natts = RelationGetDescr(cat)->natts;

	table_close(cat, AccessShareLock);
	return natts == Natts_kc_stripe;
}

/* Is this catalog row a reservation that no stripe has been written for? */
static bool
kc_tuple_is_reserved(Relation cat, HeapTuple tup)
{
	bool		isnull;
	Datum		d;

	if (RelationGetDescr(cat)->natts < Natts_kc_stripe)
		return false;
	d = heap_getattr(tup, Anum_kc_reserved, RelationGetDescr(cat), &isnull);
	return !isnull && DatumGetBool(d);
}

static void
kc_stripe_from_tuple(Relation cat, HeapTuple tup, KcStripeInfo *si)
{
	TupleDesc	desc = RelationGetDescr(cat);
	Datum		v[Natts_kc_stripe];
	bool		n[Natts_kc_stripe];
	int			i;
	int			natts = kc_catalog_check_shape(cat);

	heap_deform_tuple(tup, desc, v, n);
	for (i = 0; i < natts; i++)
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

		/*
		 * A reserved row is a claim on row numbers, not a stripe: nothing is
		 * written for it yet, and when the writer flushes it the row is replaced
		 * by the stripe's.  No reader takes it for a stripe, whoever it belongs to.
		 */
		if (kc_tuple_is_reserved(cat, tup))
			continue;
		if (n == cap)
		{
			cap = cap ? cap * 2 : 16;
			res = res ? repalloc(res, sizeof(KcStripeInfo) * cap)
				: palloc(sizeof(KcStripeInfo) * cap);
		}
		kc_stripe_from_tuple(cat, tup, &res[n]);
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

/*
 * The values of a catalog row.  cp and cs are the cid_points and col_stats
 * bytea (made by the caller).  The array has room for the 0.2 shape; with the
 * catalog of 0.1 the tuple is formed from its first 14 entries.
 */
static void
kc_catalog_values(Relation rel, const KcStripeInfo *si, bytea *cp, bytea *cs,
				  bool maintenance, bool reserved, Datum *v, bool *n)
{
	memset(n, 0, sizeof(bool) * Natts_kc_stripe);
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
	v[Anum_kc_reserved - 1] = BoolGetDatum(reserved);
}

static bytea *
kc_make_bytea(const char *data, int len)
{
	bytea	   *b = palloc(VARHDRSZ + len);

	SET_VARSIZE(b, VARHDRSZ + len);
	if (len > 0)
		memcpy(VARDATA(b), data, len);
	return b;
}

/*
 * The reserved row of a stripe that is about to be written (design 7.8,
 * 5.4): inserted when the first row goes into a buffer that reserves, by the
 * transaction (or subtransaction) that inserts that row, with the numbers it
 * claims in first_row and row_count and nothing else yet.  Readers skip it;
 * the transaction's flush replaces it with the stripe's row
 * (kc_catalog_update_reserved), and the heap rules decide what is left of it:
 * a transaction that aborts, or a backend that dies, leaves a row nobody sees.
 * The row's place is not kept: the writer holds no lock on the catalog between
 * the insert and its flush, so a VACUUM FULL or CLUSTER of the catalog may move
 * the row meanwhile, and the flush finds it again by its key (kc_find_reserved).
 */
void
kc_catalog_insert_reserved(Relation rel, const KcStripeInfo *si)
{
	Relation	cat = table_open(kc_catalog_oid(), RowExclusiveLock);
	Datum		v[Natts_kc_stripe];
	bool		n[Natts_kc_stripe];
	bytea	   *cp = kc_make_bytea(NULL, 0);
	bytea	   *cs = kc_make_bytea(NULL, 0);
	HeapTuple	tup;

	if (kc_catalog_check_shape(cat) != Natts_kc_stripe)
		elog(ERROR, "cannot reserve row numbers: the stripe catalog is not the one of version 0.2");
	kc_catalog_values(rel, si, cp, cs, false, true, v, n);
	tup = heap_form_tuple(RelationGetDescr(cat), v, n);
	CatalogTupleInsert(cat, tup);
	heap_freetuple(tup);
	pfree(cp);
	pfree(cs);
	table_close(cat, RowExclusiveLock);
}

/*
 * The place of the reserved row of this stripe, found by its key: the version
 * that is reserved, was inserted by this transaction or one of its subtransactions
 * (committed into it or running) and that nothing of ours has deleted or updated
 * (a deletion or update by a transaction that aborted does not count).  An
 * error if there is none: the writer says it holds a reservation, and it does.
 */
static void
kc_find_reserved(Relation cat, uint64 uid, uint64 stripe_id, ItemPointer tid)
{
	ScanKeyData key[2];
	SysScanDesc scan;
	HeapTuple	tup;
	bool		found = false;

	ScanKeyInit(&key[0], Anum_kc_uid, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) uid));
	ScanKeyInit(&key[1], Anum_kc_stripe_id, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) stripe_id));
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true, SnapshotAny, 2, key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		HeapTupleHeader th = tup->t_data;

		if (!kc_tuple_is_reserved(cat, tup) || (th->t_infomask & HEAP_XMIN_INVALID) ||
			!TransactionIdIsCurrentTransactionId(HeapTupleHeaderGetRawXmin(th)))
			continue;
		if (!(th->t_infomask & HEAP_XMAX_INVALID) &&
			!HEAP_XMAX_IS_LOCKED_ONLY(th->t_infomask))
		{
			TransactionId xmax = HeapTupleHeaderGetUpdateXid(th);

			if (TransactionIdIsCurrentTransactionId(xmax) ||
				TransactionIdIsInProgress(xmax) || TransactionIdDidCommit(xmax))
				continue;
		}
		*tid = tup->t_self;
		found = true;
		break;
	}
	systable_endscan(scan);
	if (!found)
		elog(ERROR, "the reserved stripe row " UINT64_FORMAT " of this transaction is missing from the stripe catalog",
			 stripe_id);
}

/*
 * Replace a reserved row by the stripe's row: the same key, the real counts,
 * place and statistics, no longer reserved.  The reserved row was inserted by
 * this transaction or one of its committed subtransactions, so the update is
 * ours to make; the caller has made the command counter pass the command that
 * inserted it (an update of a row of the same command is "invisible" to
 * heap_update).
 */
void
kc_catalog_update_reserved(Relation rel, const KcStripeInfo *si,
						   const char *cid_points, int cid_len,
						   const char *col_stats, int stats_len)
{
	Relation	cat = table_open(kc_catalog_oid(), RowExclusiveLock);
	Datum		v[Natts_kc_stripe];
	bool		n[Natts_kc_stripe];
	bytea	   *cp = kc_make_bytea(cid_points, cid_len);
	bytea	   *cs = kc_make_bytea(col_stats, stats_len);
	HeapTuple	tup;
	ItemPointerData tid;

	if (kc_catalog_check_shape(cat) != Natts_kc_stripe)
		elog(ERROR, "cannot update a reserved row: the stripe catalog is not the one of version 0.2");
	kc_find_reserved(cat, kc_rel_ident(rel)->uid, si->stripe_id, &tid);
	kc_catalog_values(rel, si, cp, cs, false, false, v, n);
	tup = heap_form_tuple(RelationGetDescr(cat), v, n);
	CatalogTupleUpdate(cat, &tid, tup);
	heap_freetuple(tup);
	pfree(cp);
	pfree(cs);
	table_close(cat, RowExclusiveLock);
}

/* Delete the reserved row of a reservation that is given up (no row went in). */
/*
 * The reserved row of this transaction claims more numbers now (C13-02: a reservation that grows
 * in place while its buffer fills): the same row, with the larger row_count.  The caller has
 * taken the numbers from the metapage under the reservation lock and passes the command first
 * (the row must not have been made by the running command).
 */
void
kc_catalog_resize_reserved(Relation rel, const KcStripeInfo *si)
{
	Relation	cat = table_open(kc_catalog_oid(), RowExclusiveLock);
	Datum		v[Natts_kc_stripe];
	bool		n[Natts_kc_stripe];
	bytea	   *cp = kc_make_bytea(NULL, 0);
	bytea	   *cs = kc_make_bytea(NULL, 0);
	HeapTuple	tup;
	ItemPointerData tid;

	kc_find_reserved(cat, kc_rel_ident(rel)->uid, si->stripe_id, &tid);
	kc_catalog_values(rel, si, cp, cs, false, true, v, n);
	tup = heap_form_tuple(RelationGetDescr(cat), v, n);
	CatalogTupleUpdate(cat, &tid, tup);
	heap_freetuple(tup);
	pfree(cp);
	pfree(cs);
	table_close(cat, RowExclusiveLock);
}

void
kc_catalog_delete_reserved(Relation rel, uint64 stripe_id)
{
	Relation	cat = table_open(kc_catalog_oid(), RowExclusiveLock);
	ItemPointerData tid;

	kc_find_reserved(cat, kc_rel_ident(rel)->uid, stripe_id, &tid);
	CatalogTupleDelete(cat, &tid);
	table_close(cat, RowExclusiveLock);
}

/*
 * The reserved rows of rel that this snapshot sees and that belong to no
 * transaction of this backend: such a row survives only if the transaction that
 * inserted it committed without writing the stripe, which no code path does.
 * Rows of transactions in progress are not seen by the snapshot (the check's is
 * one of everything committed), and those of this backend are its own writers'
 * live claims.
 */
int
kc_catalog_count_reserved(Relation rel, Snapshot snap)
{
	Relation	cat = table_open(kc_catalog_oid(), AccessShareLock);
	ScanKeyData key;
	SysScanDesc scan;
	HeapTuple	tup;
	int			n = 0;

	ScanKeyInit(&key, Anum_kc_uid, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) kc_rel_ident(rel)->uid));
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true, SnapshotAny, 1, &key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		if (!kc_tuple_visible(tup, snap, NULL) || !kc_tuple_is_reserved(cat, tup) ||
			TransactionIdIsCurrentTransactionId(HeapTupleHeaderGetXmin(tup->t_data)))
			continue;
		n++;
	}
	systable_endscan(scan);
	table_close(cat, AccessShareLock);
	return n;
}

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

	kc_catalog_check_shape(cat);
	cp = kc_make_bytea(cid_points, cid_len);
	cs = kc_make_bytea(col_stats, stats_len);
	kc_catalog_values(rel, si, cp, cs, maintenance, false, v, n);

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

/*
 * Delete a stripe row.  A row that is already gone is what the caller wanted:
 * another session (VACUUM, columnar_reconcile, a cleanup of its own) deleted
 * it, or an earlier command of this transaction did, and the answer is false,
 * not an error.  With wait, a deletion that another transaction has begun
 * and not finished is waited for; without it the row is left to that
 * transaction (false), so a cleanup never makes anyone wait.
 */
static bool
kc_catalog_delete_tuple(Relation cat, ItemPointer tid, bool wait)
{
	TM_FailureData tmfd;
	TM_Result	res;

	res = KC_HEAP_DELETE(cat, tid, wait, &tmfd);
	switch (res)
	{
		case TM_Ok:
			return true;
		case TM_SelfModified:
		case TM_Deleted:
		case TM_Updated:
		case TM_BeingModified:
			return false;
		default:
			elog(ERROR, "unexpected result %d deleting a stripe catalog row", (int) res);
	}
	return false;
}

/* Delete the rows of one stored content that are visible (to us, now); returns how many. */
int
kc_catalog_delete_uid(uint64 uid)
{
	Relation	cat = table_open(kc_catalog_oid(), RowExclusiveLock);
	ScanKeyData key;
	SysScanDesc scan;
	HeapTuple	tup;
	Snapshot	vsnap;
	int			n = 0;

	ScanKeyInit(&key, Anum_kc_uid, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) uid));
	vsnap = kc_latest_snapshot();
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true,
							  SnapshotAny, 1, &key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
		if (kc_tuple_visible(tup, vsnap, NULL) &&
			kc_catalog_delete_tuple(cat, &tup->t_self, true))
			n++;
	systable_endscan(scan);
	table_close(cat, RowExclusiveLock);
	return n;
}

/*
 * Delete one stripe's row; returns the number of rows deleted.  With wait, a
 * deletion that another transaction has begun and not finished is waited for
 * (compaction needs the answer); without it that row is left to the other
 * transaction and the answer is 0, which is what the check needs, because it
 * never waits.
 */
int
kc_catalog_delete_stripe(uint64 uid, uint64 stripe_id, bool wait)
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
		if (kc_catalog_delete_tuple(cat, &tup->t_self, wait))
			n++;
	}
	systable_endscan(scan);
	table_close(cat, RowExclusiveLock);
	return n;
}

/* ------------------------------------------------------------------------
 * Locks of the check functions, taken without waiting (C10-02)
 * ------------------------------------------------------------------------
 */

static bool
kc_catlocks_add1(KcCatLocks *l, Oid relid, LOCKMODE mode)
{
	if (l->n >= lengthof(l->oid))
		elog(ERROR, "too many relations to lock for the check");
	if (!ConditionalLockRelationOid(relid, mode))
	{
		l->failed_oid = relid;
		l->failed_mode = mode;
		return false;
	}
	l->oid[l->n] = relid;
	l->mode[l->n] = mode;
	l->n++;
	return true;
}

/*
 * The share lock a scan takes, and mode as well when it is stronger: every
 * mode the work will ask for is held beforehand, so that asking again is
 * granted at once and never queues behind a waiter.
 */
static bool
kc_catlocks_add(KcCatLocks *l, Oid relid, LOCKMODE mode)
{
	if (!kc_catlocks_add1(l, relid, AccessShareLock))
		return false;
	return mode == AccessShareLock || kc_catlocks_add1(l, relid, mode);
}

/*
 * Release what kc_catalog_lock_nowait took (all of it, also after a failure
 * part of the way).
 */
void
kc_catalog_unlock(KcCatLocks *l)
{
	while (l->n > 0)
	{
		l->n--;
		UnlockRelationOid(l->oid[l->n], l->mode[l->n]);
	}
}

/*
 * Lock, without waiting, every relation the check functions read by themselves
 * besides the tables they check: pg_class (the list of tables, the files of the
 * orphan step), the stripe catalog, its primary key index, its TOAST table and
 * the indexes of that (the stripe rows have bytea columns).  A lock that another
 * transaction holds in a conflicting mode (LOCK TABLE ... ACCESS EXCLUSIVE, the
 * REINDEX or VACUUM FULL of an administrator, a prepared transaction that holds
 * one for ever) makes this return false with nothing held, and the caller says
 * "not checked" instead of waiting behind it.  mode is the mode the work needs
 * on the catalog and its TOAST table: AccessShareLock to read, RowExclusiveLock
 * to delete rows.  Once these are held by this backend, every later open of
 * the same relations by the same code is granted at once, so the scans need no
 * conditional variant of their own.  What remains outside, and is read as any
 * statement reads it, is the relation cache's own reading of the system
 * catalogs (pg_attribute, pg_index, ...).
 */
bool
kc_catalog_lock_nowait(KcCatLocks *l, LOCKMODE mode)
{
	Oid			catoid = kc_catalog_oid();
	Oid			idxoid = kc_catalog_index_oid();
	Oid			toastoid;
	Relation	rel;

	l->n = 0;
	l->failed_oid = InvalidOid;
	l->failed_mode = NoLock;
	if (!kc_catlocks_add(l, RelationRelationId, AccessShareLock) ||
		!kc_catlocks_add(l, catoid, mode) ||
		!kc_catlocks_add(l, idxoid, AccessShareLock))
		goto fail;

	rel = table_open(catoid, NoLock);
	toastoid = rel->rd_rel->reltoastrelid;
	table_close(rel, NoLock);
	if (OidIsValid(toastoid))
	{
		List	   *idx;
		ListCell   *lc;

		if (!kc_catlocks_add(l, toastoid, mode))
			goto fail;
		rel = table_open(toastoid, NoLock);
		idx = RelationGetIndexList(rel);
		table_close(rel, NoLock);
		foreach(lc, idx)
		{
			if (!kc_catlocks_add(l, lfirst_oid(lc), mode))
			{
				list_free(idx);
				goto fail;
			}
		}
		list_free(idx);
	}
	return true;

fail:
	kc_catalog_unlock(l);
	return false;
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
 * The identity of the file a pg_class row names, or 0 when it has none or the
 * first block is not a metapage of ours.  For the object access hook, which
 * sees a relation that is being dropped (a table, or the transient table that
 * holds the old file of a rewrite) and must note which stored content goes
 * with it.
 */
uint64
kc_catalog_file_uid(Form_pg_class cl)
{
	bool		unknown = false;

	return kc_uid_of_file(cl, &unknown);
}

/*
 * Is the stripe catalog there?  The cleanup of replaced files runs at commit,
 * and a transaction that drops the extension has no catalog left to clean:
 * its rows went with the table.
 */
bool
kc_catalog_available(void)
{
	Oid			extoid = get_extension_oid(KC_EXTENSION_NAME, true);
	Oid			nspoid;

	if (!OidIsValid(extoid))
		return false;
	nspoid = get_extension_schema(extoid);
	return OidIsValid(get_relname_relid(KC_CATALOG_NAME, nspoid)) &&
		OidIsValid(get_relname_relid(KC_CATALOG_PKEY, nspoid));
}
/*
 * A table access method has no callback at DROP, and a rewrite leaves the old
 * file's rows behind, so the rows whose stored content no relation owns any
 * more are removed here: after a drop, at the end of VACUUM, and by
 * columnar_check.
 *
 * What owns a row is read from pg_class with a dirty snapshot, so the
 * relations of transactions that have not committed yet (a table being
 * created, or rewritten, by another session) keep their rows.  The two reads,
 * the stripe catalog and pg_class, cannot be made at one instant, and each
 * order has a race; the steps below close both (C11-01):
 *
 *   1. a snapshot of the catalog, then the gate for tests;
 *   2. the files that are live, from pg_class (the first claim);
 *   3. a second snapshot, taken after that scan;
 *   4. the stripe rows: a candidate only if both snapshots see it and no
 *      live file of step 2 owns it;
 *   5. the files that are live again (the second claim), and a candidate that
 *      is claimed now is dropped.
 *
 * A table that is dropped, or given a new file (VACUUM FULL, SET ACCESS
 * METHOD), by a transaction that commits before the scan of step 2 and after
 * the first snapshot deletes the stripe rows of the old file in that very
 * transaction: the first snapshot sees them and the scan finds no file for
 * them, and they were called orphans (a false alarm in 10 to 30 percent of
 * checks while DDL ran); the second snapshot does not see them.  Rows that
 * a rewrite inserts frozen (visible to every snapshot as soon as they are
 * written, even before the transaction ends) are seen by both snapshots
 * whenever they were written; the file they belong to is claimed by the
 * second scan (the transient table exists before its first row, and the new
 * file of the table once the rewrite commits), so they are not orphans
 * either, and, in a deleting call, are never deleted under a rewrite that has
 * not finished.
 *
 * Which relations are columnar is decided by the access method's handler
 * (kc_am_class), and a relation of a table-like kind whose access method
 * cannot be classified makes the whole cleanup stand down: rows are deleted
 * only when every relation that could own them has been classified (C11-02).
 *
 * Returns the number of rows deleted (or, dry, that would be), or -1 when the
 * cleanup stood down and did nothing (C12-01): the answer "0" of a step that did
 * not run must not be the answer "0" of a step that ran and found nothing.  *why
 * (when the caller gives one) is then the reason, a string that lives on.
 */

/*
 * The identities of the live files, sorted, from pg_class with a dirty
 * snapshot.  False, with nothing returned, when the cleanup must stand down:
 * a columnar relation whose metapage cannot be read, or one whose access
 * method cannot be classified.
 */
static bool
kc_live_files(uint64 **live_out, int *nlive_out, const char **why, bool guard)
{
	Relation	pgclass;
	SysScanDesc scan;
	HeapTuple	tup;
	SnapshotData dirty;
	uint64	   *live = NULL;
	int			nlive = 0;
	int			caplive = 0;
	bool		unknown = false;
	bool		unclassified = false;
	KcAmMemo	memo = {InvalidOid, KC_AM_UNKNOWN, false};

	memo.guard = guard;
	pgclass = table_open(RelationRelationId, AccessShareLock);
	InitDirtySnapshot(dirty);
	scan = systable_beginscan(pgclass, InvalidOid, false, &dirty, 0, NULL);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		Form_pg_class cl = (Form_pg_class) GETSTRUCT(tup);
		uint64		uid;
		KcAmClass	cls;

		if ((cl->relkind != RELKIND_RELATION && cl->relkind != RELKIND_MATVIEW) ||
			!OidIsValid(cl->relfilenode))
			continue;
		cls = kc_am_class_memo(cl->relam, &memo);
		if (cls == KC_AM_UNKNOWN)
		{
			unclassified = true;
			continue;
		}
		if (cls != KC_AM_COLUMNAR)
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
	if (unknown || unclassified)
	{
		*why = unknown ?
			"a columnar relation has an unreadable metapage" :
			"a relation has an access method that cannot be classified (one that a transaction has made and not committed, or whose handler raises an error)";
		ereport(LOG,
				(errmsg(unknown ?
						"columnar stripe catalog cleanup skipped: a columnar relation has an unreadable metapage" :
						"columnar stripe catalog cleanup skipped: a relation has an access method that cannot be classified")));
		if (live)
			pfree(live);
		return false;
	}
	if (nlive > 1)
		qsort(live, nlive, sizeof(uint64), kc_u64_cmp);
	*live_out = live;
	*nlive_out = nlive;
	return true;
}

static bool
kc_uid_is_live(uint64 uid, const uint64 *live, int nlive)
{
	return nlive > 0 && bsearch(&uid, live, nlive, sizeof(uint64), kc_u64_cmp) != NULL;
}

typedef struct KcOrphanCand
{
	ItemPointerData tid;
	uint64		uid;
} KcOrphanCand;

static int
kc_reconcile_ex(bool dry, bool guard, const char **why_out)
{
	const char *why = NULL;
	SysScanDesc scan;
	HeapTuple	tup;
	uint64	   *live = NULL;
	int			nlive = 0;
	uint64	   *live2 = NULL;
	int			nlive2 = 0;
	Relation	cat;
	ScanKeyData key;
	int			ndel = 0;
	Snapshot	catsnap = kc_latest_snapshot();
	Snapshot	catsnap2;
	KcOrphanCand *cand = NULL;
	int			ncand = 0;
	int			capcand = 0;
	int			i;
	bool		stood_down = false;

	/* the test gate (t/036): after the first snapshot, before the scan of pg_class */
	kc_check_gate(KC_GATE_ORPHANS);

	if (!kc_live_files(&live, &nlive, &why, guard))
	{
		if (why_out)
			*why_out = why;
		return -1;
	}

	/* the test gate (t/036): after the first claim, before the second snapshot */
	kc_check_gate(KC_GATE_CLAIMS);
	catsnap2 = kc_latest_snapshot();

	cat = table_open(kc_catalog_oid(), dry ? AccessShareLock : RowExclusiveLock);
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true,
							  SnapshotAny, 0, &key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		bool		isnull;
		uint64		uid = (uint64) DatumGetInt64(heap_getattr(tup, Anum_kc_uid,
															  RelationGetDescr(cat),
															  &isnull));

		if (isnull || !kc_tuple_visible(tup, catsnap, NULL) ||
			!kc_tuple_visible(tup, catsnap2, NULL) ||
			kc_uid_is_live(uid, live, nlive))
			continue;
		if (ncand == capcand)
		{
			capcand = capcand ? capcand * 2 : 64;
			cand = cand ? repalloc(cand, sizeof(KcOrphanCand) * capcand)
				: palloc(sizeof(KcOrphanCand) * capcand);
		}
		cand[ncand].tid = tup->t_self;
		cand[ncand].uid = uid;
		ncand++;
	}
	systable_endscan(scan);

	/*
	 * The second claim: what is live now.  A candidate that a table owns
	 * after all (its table was made, or rewritten, since the first scan) is
	 * not an orphan.  A cleanup that cannot classify now stands down.
	 */
	if (ncand > 0 && !kc_live_files(&live2, &nlive2, &why, guard))
	{
		ncand = 0;
		stood_down = true;
	}
	for (i = 0; i < ncand; i++)
	{
		if (kc_uid_is_live(cand[i].uid, live2, nlive2))
			continue;
		/* a row that another session is deleting or deleted is not ours to count */
		if (dry || kc_catalog_delete_tuple(cat, &cand[i].tid, false))
			ndel++;
	}
	table_close(cat, dry ? AccessShareLock : RowExclusiveLock);
	if (live)
		pfree(live);
	if (live2)
		pfree(live2);
	if (cand)
		pfree(cand);
	if (stood_down)
	{
		if (why_out)
			*why_out = why;
		return -1;
	}
	return ndel;
}


int
kc_catalog_reconcile(const char **why, bool guard)
{
	return kc_reconcile_ex(false, guard, why);
}

/* only the check calls it, and the check begins subtransactions anyway */
int
kc_catalog_count_orphans(const char **why)
{
	return kc_reconcile_ex(true, true, why);
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

/* ------------------------------------------------------------------------
 * Who holds a row number (uniqueness, C6c) and which numbers are live (index vacuum)
 * ------------------------------------------------------------------------
 */

/* What one version of a stripe catalog row says about its writer. */
typedef enum KcVer
{
	KC_VER_DEAD,				/* aborted, or replaced/deleted: holds nothing */
	KC_VER_OWN,					/* written by this transaction or one of its subtransactions */
	KC_VER_COMMITTED,
	KC_VER_RUNNING				/* a transaction in progress, a prepared one included */
} KcVer;

static KcVer
kc_version_status(HeapTupleHeader th, TransactionId *xmin_out)
{
	TransactionId xmin = HeapTupleHeaderGetRawXmin(th);

	*xmin_out = InvalidTransactionId;
	if (HeapTupleHeaderXminFrozen(th))
		return KC_VER_COMMITTED;
	if (th->t_infomask & HEAP_XMIN_INVALID)
		return KC_VER_DEAD;
	if (TransactionIdIsCurrentTransactionId(xmin))
		return KC_VER_OWN;
	if (TransactionIdIsInProgress(xmin))
	{
		*xmin_out = xmin;
		return KC_VER_RUNNING;
	}
	if ((th->t_infomask & HEAP_XMIN_COMMITTED) || TransactionIdDidCommit(xmin))
		return KC_VER_COMMITTED;
	return KC_VER_DEAD;
}

/* Has this version been replaced or deleted (by anyone, whatever became of that)? */
static bool
kc_version_superseded(HeapTupleHeader th)
{
	return !(th->t_infomask & HEAP_XMAX_INVALID) && !HEAP_XMAX_IS_LOCKED_ONLY(th->t_infomask);
}

/*
 * Does this version hold nothing for a unique check, by what became of its replacement or
 * deletion?  A reserved row is replaced by the stripe row of its own writer, which covers only
 * the rows the stripe has: whatever the replacer's state, the reserved row holds nothing
 * (its numbers beyond the stripe may be reserved again by another writer).  A stripe row that
 * a transaction deleted (compaction replaces stripes with copies that have new numbers) holds
 * nothing once the deleter is this transaction or has committed; while the deleter runs, and
 * if it aborted, the rows are still the table's rows.
 */
static bool
kc_version_gone(Relation cat, HeapTuple tup)
{
	HeapTupleHeader th = tup->t_data;
	TransactionId xmax;

	if (!kc_version_superseded(th))
		return false;
	if (kc_tuple_is_reserved(cat, tup))
		return true;
	xmax = HeapTupleHeaderGetUpdateXid(th);
	if (TransactionIdIsCurrentTransactionId(xmax))
		return true;
	if (TransactionIdIsInProgress(xmax))
		return false;
	return TransactionIdDidCommit(xmax);
}

/*
 * The state of one row number of rel for a dirty read (a unique index asks, with
 * SnapshotDirty): KC_ROW_NONE (no row: nobody holds it, or its writer aborted),
 * KC_ROW_FOUND (a row that is committed or this transaction's own), or KC_ROW_WAIT
 * (the row belongs to a transaction in progress, *xwait): the caller waits for it
 * and asks again.  The answer comes from the stripe catalog alone, with its own
 * visibility rules (a stripe is its catalog row, the claim of a writer is its
 * reserved row), so a row that is only in the memory of another backend is found by
 * the claim.  A version that was replaced (a reserved row by its stripe's row, which
 * covers only the rows the stripe has) holds nothing: its numbers beyond the stripe
 * may be reserved again by another writer after the tail was given back.
 */
KcRowState
kc_catalog_probe_row(Relation rel, uint64 row, TransactionId *xwait)
{
	Relation	cat = table_open(kc_catalog_oid(), AccessShareLock);
	ScanKeyData key;
	SysScanDesc scan;
	HeapTuple	tup;
	KcRowState	res = KC_ROW_NONE;

	*xwait = InvalidTransactionId;
	ScanKeyInit(&key, Anum_kc_uid, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) kc_rel_ident(rel)->uid));
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true, SnapshotAny, 1, &key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		bool		isnull;
		uint64		first = (uint64) DatumGetInt64(heap_getattr(tup, Anum_kc_first_row,
																  RelationGetDescr(cat), &isnull));
		uint32		cnt = (uint32) DatumGetInt32(heap_getattr(tup, Anum_kc_row_count,
															  RelationGetDescr(cat), &isnull));
		TransactionId x;
		KcVer		v;

		if (row < first || row - first >= cnt)
			continue;
		if (kc_version_gone(cat, tup))
			continue;
		v = kc_version_status(tup->t_data, &x);
		if (v == KC_VER_OWN || v == KC_VER_COMMITTED)
		{
			res = KC_ROW_FOUND;
			break;
		}
		if (v == KC_VER_RUNNING && res == KC_ROW_NONE)
		{
			res = KC_ROW_WAIT;
			*xwait = x;
		}
	}
	systable_endscan(scan);
	table_close(cat, AccessShareLock);
	return res;
}

static int
kc_range_cmp(const void *a, const void *b)
{
	const KcRowRange *x = a;
	const KcRowRange *y = b;

	return (x->first > y->first) - (x->first < y->first);
}

/*
 * The row numbers an index entry may still need (index vacuum): the ranges of
 * every stripe row version whose writer did not abort, except versions deleted by a
 * transaction that committed before the oldest transaction any snapshot may still
 * have, merged into disjoint ranges sorted by their first number.  *low is the
 * number below which the answer holds: a number at or above it is never to be
 * deleted (the claims of writers in progress and every number the metapage may hand
 * out from now on).  The caller holds the reservation lock (kc_reserve_lock), which
 * keeps the metapage and the catalog in step: no writer is between taking numbers and
 * inserting its row for them.
 */
KcRowRange *
kc_catalog_live_ranges(Relation rel, TransactionId oldest, uint64 next_row,
					   int *n_out, uint64 *low)
{
	Relation	cat = table_open(kc_catalog_oid(), AccessShareLock);
	ScanKeyData key;
	SysScanDesc scan;
	HeapTuple	tup;
	KcRowRange *r = NULL;
	int			n = 0;
	int			cap = 0;
	int			i;
	int			m;

	*low = next_row;
	ScanKeyInit(&key, Anum_kc_uid, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) kc_rel_ident(rel)->uid));
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true, SnapshotAny, 1, &key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		bool		isnull;
		uint64		first = (uint64) DatumGetInt64(heap_getattr(tup, Anum_kc_first_row,
																  RelationGetDescr(cat), &isnull));
		uint32		cnt = (uint32) DatumGetInt32(heap_getattr(tup, Anum_kc_row_count,
															  RelationGetDescr(cat), &isnull));
		HeapTupleHeader th = tup->t_data;
		TransactionId x;
		KcVer		v = kc_version_status(th, &x);

		if (v == KC_VER_DEAD)
			continue;
		if (v == KC_VER_RUNNING || v == KC_VER_OWN)
			*low = Min(*low, first);
		if (kc_version_superseded(th))
		{
			TransactionId xmax = HeapTupleHeaderGetUpdateXid(th);

			if (!TransactionIdIsCurrentTransactionId(xmax) && !TransactionIdIsInProgress(xmax) &&
				TransactionIdDidCommit(xmax) && TransactionIdPrecedes(xmax, oldest))
				continue;
		}
		if (n == cap)
		{
			cap = cap ? cap * 2 : 64;
			r = r ? repalloc(r, sizeof(KcRowRange) * cap) : palloc(sizeof(KcRowRange) * cap);
		}
		r[n].first = first;
		r[n].end = first + cnt;
		n++;
	}
	systable_endscan(scan);
	table_close(cat, AccessShareLock);

	if (n > 1)
		qsort(r, n, sizeof(KcRowRange), kc_range_cmp);
	/* merge into disjoint ranges */
	m = 0;
	for (i = 0; i < n; i++)
	{
		if (m > 0 && r[i].first <= r[m - 1].end)
			r[m - 1].end = Max(r[m - 1].end, r[i].end);
		else
			r[m++] = r[i];
	}
	*n_out = m;
	return r;
}

/*
 * The blocks of the file that some stripe row version still needs (increment C8), as disjoint
 * ranges [first, end) sorted by their first block: the blocks of every version of this table's
 * stripes whose writer did not abort, except versions deleted by a transaction that committed
 * before the oldest transaction any snapshot may still have (compaction's old stripes, once
 * nobody can see them).  A version with no blocks (a reserved row) needs none.  Everything else
 * in the file, past the metapage, is free, provided no writer is running (the caller's lock
 * sees to that).  *horizon is the newest transaction among those deleters, which a standby
 * query may still hold: the record that lists the blocks cancels it.
 */
KcRowRange *
kc_catalog_live_blocks(Relation rel, TransactionId oldest, int *n_out, TransactionId *horizon)
{
	Relation	cat = table_open(kc_catalog_oid(), AccessShareLock);
	ScanKeyData key;
	SysScanDesc scan;
	HeapTuple	tup;
	KcRowRange *r = NULL;
	int			n = 0;
	int			cap = 0;
	int			i;
	int			m;

	*horizon = InvalidTransactionId;
	ScanKeyInit(&key, Anum_kc_uid, BTEqualStrategyNumber, F_INT8EQ,
				Int64GetDatum((int64) kc_rel_ident(rel)->uid));
	scan = systable_beginscan(cat, kc_catalog_index_oid(), true, SnapshotAny, 1, &key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		bool		isnull;
		uint64		first = (uint64) DatumGetInt64(heap_getattr(tup, Anum_kc_first_block,
																  RelationGetDescr(cat), &isnull));
		uint32		cnt = (uint32) DatumGetInt32(heap_getattr(tup, Anum_kc_block_count,
															  RelationGetDescr(cat), &isnull));
		HeapTupleHeader th = tup->t_data;
		TransactionId x;
		KcVer		v = kc_version_status(th, &x);

		if (v == KC_VER_DEAD || cnt == 0)
			continue;
		if (kc_version_superseded(th))
		{
			TransactionId xmax = HeapTupleHeaderGetUpdateXid(th);

			if (!TransactionIdIsCurrentTransactionId(xmax) && !TransactionIdIsInProgress(xmax) &&
				TransactionIdDidCommit(xmax) && TransactionIdPrecedes(xmax, oldest))
			{
				if (!TransactionIdIsValid(*horizon) || TransactionIdFollows(xmax, *horizon))
					*horizon = xmax;
				continue;
			}
		}
		if (n == cap)
		{
			cap = cap ? cap * 2 : 64;
			r = r ? repalloc(r, sizeof(KcRowRange) * cap) : palloc(sizeof(KcRowRange) * cap);
		}
		r[n].first = first;
		r[n].end = first + cnt;
		n++;
	}
	systable_endscan(scan);
	table_close(cat, AccessShareLock);

	if (n > 1)
		qsort(r, n, sizeof(KcRowRange), kc_range_cmp);
	m = 0;
	for (i = 0; i < n; i++)
	{
		if (m > 0 && r[i].first <= r[m - 1].end)
			r[m - 1].end = Max(r[m - 1].end, r[i].end);
		else
			r[m++] = r[i];
	}
	*n_out = m;
	return r;
}

/*
 * The stripes that an older snapshot may still see and a current one does not (C13-01): versions
 * whose writer committed and whose deleter (a compaction) is this transaction or committed but
 * not before the oldest transaction any snapshot may have, so they are not removable yet.  An
 * index that is built or rebuilt now must have an entry for the rows of these stripes as well as
 * for those of the stripes visible now, or a snapshot older than the compaction that reads
 * through the index finds none of its rows (the sequential scan of the same snapshot does).  Heap
 * indexes recently dead tuples for the same reason.  The caller holds a lock that keeps a
 * compaction and a vacuum of the table out.
 */
KcStripeInfo *
kc_catalog_load_dead(Relation rel, TransactionId oldest, int *nstripes)
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
		HeapTupleHeader th = tup->t_data;
		TransactionId x;
		TransactionId xmax;
		KcVer		v;

		if (kc_tuple_is_reserved(cat, tup))
			continue;
		v = kc_version_status(th, &x);
		if (v != KC_VER_COMMITTED || !kc_version_superseded(th))
			continue;
		xmax = HeapTupleHeaderGetUpdateXid(th);
		if (TransactionIdIsCurrentTransactionId(xmax))
			;					/* deleted by this transaction: other snapshots still see it */
		else if (TransactionIdIsInProgress(xmax) || !TransactionIdDidCommit(xmax))
			continue;			/* its deleter is running elsewhere (a lock keeps that out) or aborted: it is visible, not dead */
		else if (TransactionIdPrecedes(xmax, oldest))
			continue;			/* nobody can see it any more */
		if (n == cap)
		{
			cap = cap ? cap * 2 : 16;
			res = res ? repalloc(res, sizeof(KcStripeInfo) * cap)
				: palloc(sizeof(KcStripeInfo) * cap);
		}
		kc_stripe_from_tuple(cat, tup, &res[n]);
		if (res[n].block_count == 0)
			continue;
		n++;
	}
	systable_endscan(scan);
	table_close(cat, AccessShareLock);
	*nstripes = n;
	return res;
}
