/*-------------------------------------------------------------------------
 *
 * kc_am.c
 *	  The table access method callbacks of the columnar storage.
 *
 * Implemented: scans, INSERT and COPY (through the write buffers of
 * kc_write.c), TRUNCATE, rewrites (ALTER TABLE, VACUUM FULL), SET TABLESPACE,
 * VACUUM, ANALYZE.  Refused with a clear error: everything that needs a row
 * identity or a way to change or lock a row (UPDATE, DELETE, row locks,
 * indexes, TABLESAMPLE, speculative insertion).  A refused callback raises
 * ERRCODE_FEATURE_NOT_SUPPORTED with the text the design gives.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kc_am.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include <math.h>

#include "access/heapam.h"
#include "access/parallel.h"
#include "access/multixact.h"
#include "access/relscan.h"
#include "access/tableam.h"
#include "access/xact.h"
#include "catalog/pg_am.h"
#include "catalog/index.h"
#include "catalog/pg_class.h"
#include "catalog/storage.h"
#include "commands/vacuum.h"
#include "executor/executor.h"
#include "miscadmin.h"
#include "tcop/pquery.h"
#include "storage/bufmgr.h"
#include "storage/read_stream.h"
#include "storage/smgr.h"
#include "storage/lmgr.h"
#include "utils/rel.h"
#include "utils/syscache.h"
#include "utils/snapmgr.h"

#include "kingres_columnar.h"

void
kc_require_rmgr(void)
{
	if (!kc_rmgr_registered)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("kingres_columnar must be listed in shared_preload_libraries to write columnar tables"),
				 errhint("Add kingres_columnar to shared_preload_libraries and restart the server.")));
}

/* ------------------------------------------------------------------------
 * Scans
 * ------------------------------------------------------------------------
 */

/*
 * Rows are produced as virtual tuples, with one change: a heap tuple copied
 * from the slot carries the slot's TID.  ANALYZE keeps its sample rows as
 * heap tuples and sorts them by TID, which the plain virtual slot does not
 * set.  The TID is a number made from the row's position (kc_scan.c); nothing
 * can fetch a row by it.
 */
static TupleTableSlotOps kc_slot_ops;

static HeapTuple
kc_slot_copy_heap_tuple(TupleTableSlot *slot)
{
	HeapTuple	tup = TTSOpsVirtual.copy_heap_tuple(slot);

	tup->t_self = slot->tts_tid;
	tup->t_tableOid = slot->tts_tableOid;
	return tup;
}

void
kc_slot_ops_init(void)
{
	kc_slot_ops = TTSOpsVirtual;
	kc_slot_ops.copy_heap_tuple = kc_slot_copy_heap_tuple;
}

static const TupleTableSlotOps *
kc_slot_callbacks(Relation rel)
{
	return &kc_slot_ops;
}

#define KC_PSCAN_HEAD	MAXALIGN(sizeof(ParallelTableScanDescData))

static TableScanDesc
kc_scan_begin_cb(Relation rel, Snapshot snapshot, int nkeys, struct ScanKeyData *key,
				 ParallelTableScanDesc pscan, uint32 flags)
{
	KcScan	   *scan;

	if (pscan != NULL)
	{
		/* the scan of an index build, shared by stripes (see kc_parallelscan_initialize) */
		scan = kc_scan_begin(rel, NULL, NULL, NULL,
							 (int) flags | KC_SCAN_MAINTENANCE | KC_SCAN_PARALLEL_OK);
		scan->shared = (KcParallel *) ((char *) pscan + KC_PSCAN_HEAD);
		return &scan->base;
	}
	/* the leader's unflushed rows are invisible to a worker: refuse, never answer wrongly */
	if (IsParallelWorker())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("columnar tables cannot be read in a parallel worker"),
				 errhint("Declare the function PARALLEL UNSAFE, or run the query without parallelism.")));
	if (nkeys > 0)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("scan keys are not supported on columnar tables")));
	if (pscan != NULL)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("parallel scans of columnar tables are not supported in this version")));
	scan = kc_scan_begin(rel, snapshot, NULL, NULL, (int) flags);
	return &scan->base;
}

static void
kc_scan_end_cb(TableScanDesc sscan)
{
	kc_scan_end((KcScan *) sscan);
}

static void
kc_scan_rescan_cb(TableScanDesc sscan, struct ScanKeyData *key, bool set_params,
				  bool allow_strat, bool allow_sync, bool allow_pagemode)
{
	kc_scan_rescan((KcScan *) sscan);
}

/*
 * A backward step (a SCROLL cursor) re-reads from the start and skips: slow
 * but correct, and the rows are the same ones in the same order.
 */
static bool
kc_getnextslot(TableScanDesc sscan, ScanDirection direction, TupleTableSlot *slot)
{
	KcScan	   *scan = (KcScan *) sscan;

	if (ScanDirectionIsForward(direction))
	{
		if (kc_scan_next(scan, slot))
		{
			scan->ordinal++;
			return true;
		}
		scan->hit_end = true;
		return false;
	}
	else
	{
		int64		target = scan->hit_end ? scan->ordinal - 1 : scan->ordinal - 2;
		int64		i;

		if (target < 0)
		{
			kc_scan_rescan(scan);
			ExecClearTuple(slot);
			return false;
		}
		kc_scan_rescan(scan);
		for (i = 0; i <= target; i++)
		{
			if (!kc_scan_next(scan, slot))
				return false;
		}
		scan->ordinal = target + 1;
		scan->hit_end = false;
		return true;
	}
}

/*
 * The only parallel table scan there is: the one a B-tree build starts (a plan never
 * has one, kc_cscan.c).  The descriptor is followed by the counter the participants
 * take the next stripe from (KcParallel); every participant reads the stripes
 * with the maintenance rule (all rows of the committed stripes and of this
 * transaction's own), and the rows of this transaction were written before the
 * workers started.
 */
static Size
kc_parallelscan_estimate(Relation rel)
{
	return KC_PSCAN_HEAD + KcParallelSize(1);
}

static Size
kc_parallelscan_initialize(Relation rel, ParallelTableScanDesc pscan)
{
	KcParallel *sh = (KcParallel *) ((char *) pscan + KC_PSCAN_HEAD);

	/* rows of this transaction that are only in memory are invisible to workers */
	if (kc_write_has_unwritten(rel))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("cannot build an index of columnar table \"%s\" in parallel: this transaction holds rows of it that are not written",
						RelationGetRelationName(rel)),
				 errhint("Set max_parallel_maintenance_workers to 0 for this statement.")));
	pscan->phs_locator = rel->rd_locator;
	pscan->phs_syncscan = false;
	pscan->phs_snapshot_any = false;
	pscan->phs_snapshot_off = 0;
	memset(sh, 0, KcParallelSize(1));
	pg_atomic_init_u64(&sh->next, 0);
	sh->nslots = 1;
	return KC_PSCAN_HEAD + KcParallelSize(1);
}

static void
kc_parallelscan_reinitialize(Relation rel, ParallelTableScanDesc pscan)
{
	KcParallel *sh = (KcParallel *) ((char *) pscan + KC_PSCAN_HEAD);

	pg_atomic_write_u64(&sh->next, 0);
}

/* ------------------------------------------------------------------------
 * Index fetches and row versions: refused
 * ------------------------------------------------------------------------
 */

static pg_noreturn void
kc_no_indexes(void)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("columnar tables have no indexes in this version")));
}

/*
 * What the entry points that always raise an error do where an error is
 * forbidden: the one helper decides (kc_entry_refused); where it says no the
 * entry point answers "nothing" and the transaction cannot commit if it is
 * still open.  No index can exist on a columnar table (an index build is an
 * error), so PostgreSQL itself never calls these at all; a module that does
 * gets the answer a table without rows would give, and a WARNING.
 */
/*
 * Index fetches (C6b).  An index scan hands the access method a TID, and the
 * TID is a row number (kc_row_tid): the row is taken from a buffer of this
 * transaction that reserved it, else from the visible stripe whose numbers hold
 * it, one chunk group decoded at a time and kept (kc_scan_fetch_row).  What a scan
 * cannot see (a stripe of a writer that is in progress or aborted, a number that
 * no stripe has) returns nothing, which is how every dead index entry is filtered.
 * Where the server forbids an error the answer is "nothing" and a warning
 * (kc_write_refused).
 */
typedef struct KcIndexFetch
{
	IndexFetchTableData base;
	KcScan	   *scan;			/* made at the first fetch, with the snapshot of that call */
	Snapshot	snap;
	MemoryContext cxt;			/* the scan lives as long as the fetch does */
} KcIndexFetch;

static IndexFetchTableData kc_refused_fetch;

static IndexFetchTableData *
kc_index_fetch_begin(KC_INDEX_FETCH_BEGIN_PARAMS)
{
	KcIndexFetch *f;

	if (kc_write_refused(rel, "index fetch"))
	{
		/* static memory: nothing is allocated where the server forbids it */
		kc_refused_fetch.rel = rel;
		return &kc_refused_fetch;
	}
	f = palloc0(sizeof(KcIndexFetch));
	f->base.rel = rel;
	f->cxt = CurrentMemoryContext;
	return &f->base;
}

static void
kc_index_fetch_reset(IndexFetchTableData *scan)
{
}

static void
kc_index_fetch_end(IndexFetchTableData *scan)
{
	KcIndexFetch *f = (KcIndexFetch *) scan;

	if (scan == &kc_refused_fetch)
		return;
	if (f->scan != NULL)
		kc_scan_end(f->scan);
	pfree(f);
}

static bool
kc_index_fetch_tuple(IndexFetchTableData *scan, ItemPointer tid,
					 Snapshot snapshot, TupleTableSlot *slot, bool *call_again,
					 bool *all_dead)
{
	KcIndexFetch *f = (KcIndexFetch *) scan;
	OffsetNumber off;
	MemoryContext old;

	*call_again = false;
	*all_dead = false;
	if (scan == &kc_refused_fetch || kc_write_refused(scan->rel, "index fetch"))
		return false;
	off = ItemPointerGetOffsetNumber(tid);
	if (off < 1 || off > MaxOffsetNumber)
		return false;
	if (f->scan != NULL && f->snap != snapshot)
	{
		kc_scan_end(f->scan);
		f->scan = NULL;
	}
	if (f->scan == NULL)
	{
		old = MemoryContextSwitchTo(f->cxt);
		f->scan = kc_scan_begin(scan->rel, snapshot, NULL, NULL, KC_SCAN_FETCH);
		f->snap = snapshot;
		MemoryContextSwitchTo(old);
	}
	return kc_scan_fetch_row(f->scan,
							 (uint64) ItemPointerGetBlockNumber(tid) * MaxOffsetNumber + (off - 1),
							 slot);
}

/*
 * Reached by AFTER ROW triggers (the event holds the row's TID and the
 * executor re-reads the row at the end of the statement), by TID scans and by
 * foreign key checks.  A row of a columnar table has no TID before it is
 * flushed, so none of these can work in this version.
 */
static bool
kc_fetch_row_version(Relation rel, ItemPointer tid, Snapshot snapshot,
					 TupleTableSlot *slot)
{
	if (kc_write_refused(rel, "fetch by TID"))
		return false;
	/* UPDATE, DELETE and MERGE read the old row first: say what they want */
	if (ActivePortal != NULL &&
		(ActivePortal->commandTag == CMDTAG_UPDATE ||
		 ActivePortal->commandTag == CMDTAG_DELETE ||
		 ActivePortal->commandTag == CMDTAG_MERGE))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("columnar tables are append-only: UPDATE and DELETE are not supported")));
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("columnar tables do not support fetching a row by its TID"),
			 errdetail("AFTER ROW triggers, TID scans and foreign keys need it."),
			 errhint("Use statement-level triggers, or keep the table in row storage.")));
	return false;
}

static bool
kc_tuple_tid_valid(TableScanDesc scan, ItemPointer tid)
{
	if (kc_write_refused(scan->rs_rd, "TID check"))
		return false;
	/* a TID scan asks this first: refuse it here, not by finding no rows */
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("columnar tables do not support fetching a row by its TID"),
			 errdetail("AFTER ROW triggers, TID scans and foreign keys need it.")));
	return false;
}

static void
kc_tuple_get_latest_tid(TableScanDesc scan, ItemPointer tid)
{
}

static bool
kc_tuple_satisfies_snapshot(Relation rel, TupleTableSlot *slot, Snapshot snapshot)
{
	return true;
}

static TransactionId
kc_index_delete_tuples(Relation rel, TM_IndexDeleteOp *delstate)
{
	if (kc_write_refused(rel, "index delete"))
		return InvalidTransactionId;
	kc_no_indexes();
}

/* ------------------------------------------------------------------------
 * Writes
 * ------------------------------------------------------------------------
 */

/*
 * An inserted row has no identity until its stripe is written.  The slot gets
 * a valid but meaningless TID: the executor queues AFTER ROW trigger events
 * and constraint checks with it, and the event's attempt to read the row back
 * then fails in kc_fetch_row_version with a clear message, where an invalid
 * TID would make it run the trigger without a row.
 */
static void
kc_set_insert_tid(TupleTableSlot *slot, uint64 rownum)
{
	/*
	 * A row that was given its number when it was inserted (the buffer holds a
	 * reservation of row numbers, or the row was written at once) has its real
	 * TID, the one a scan shows after the commit; every other row gets the
	 * meaningless one.
	 */
	if (rownum != KC_NO_ROW)
		kc_row_tid(rownum, &slot->tts_tid);
	else
		ItemPointerSet(&slot->tts_tid, MaxBlockNumber - 1, 1);
}

static void
kc_tuple_insert(Relation rel, TupleTableSlot *slot, CommandId cid,
				KcInsertOptions options, struct BulkInsertStateData *bistate)
{
	/*
	 * Where the server forbids an error (the end of a transaction, a critical
	 * section) the write is refused with a warning, here and in every callback
	 * below that would otherwise raise one (kc_write_refused).
	 */
	uint64		rownum = KC_NO_ROW;

	if (!kc_write_refused(rel, "insert"))
	{
		kc_require_rmgr();
		rownum = kc_write_row(rel, slot, cid);
	}
	slot->tts_tableOid = RelationGetRelid(rel);
	kc_set_insert_tid(slot, rownum);
}

static void
kc_tuple_insert_speculative(Relation rel, TupleTableSlot *slot, CommandId cid,
							KcInsertOptions options, struct BulkInsertStateData *bistate,
							uint32 specToken)
{
	if (kc_write_refused(rel, "speculative insert"))
		return;
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("INSERT ... ON CONFLICT DO UPDATE is not supported on columnar tables")));
}

static void
kc_tuple_complete_speculative(Relation rel, TupleTableSlot *slot,
							  uint32 specToken, bool succeeded)
{
	if (kc_write_refused(rel, "speculative insert completion"))
		return;
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("INSERT ... ON CONFLICT DO UPDATE is not supported on columnar tables")));
}

static void
kc_multi_insert(Relation rel, TupleTableSlot **slots, int nslots, CommandId cid,
				KcInsertOptions options, struct BulkInsertStateData *bistate)
{
	int			i;
	uint64	   *rownums = palloc(sizeof(uint64) * Max(nslots, 1));

	for (i = 0; i < nslots; i++)
		rownums[i] = KC_NO_ROW;
	if (!kc_write_refused(rel, "multi insert"))
	{
		kc_require_rmgr();
		kc_write_rows(rel, slots, nslots, cid, rownums);
	}
	for (i = 0; i < nslots; i++)
	{
		slots[i]->tts_tableOid = RelationGetRelid(rel);
		kc_set_insert_tid(slots[i], rownums[i]);
	}
	pfree(rownums);
}

static TM_Result
kc_tuple_delete(KC_DELETE_PARAMS)
{
	/* nothing was deleted: the row is not there for this table (and a warning says why) */
	if (kc_write_refused(rel, "delete"))
		return TM_Invisible;
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("columnar tables are append-only: UPDATE and DELETE are not supported")));
	return TM_Ok;
}

static TM_Result
kc_tuple_update(KC_UPDATE_PARAMS)
{
	if (kc_write_refused(rel, "update"))
		return TM_Invisible;
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("columnar tables are append-only: UPDATE and DELETE are not supported")));
	return TM_Ok;
}

static TM_Result
kc_tuple_lock(Relation rel, ItemPointer tid, Snapshot snapshot,
			  TupleTableSlot *slot, CommandId cid, LockTupleMode mode,
			  LockWaitPolicy wait_policy, uint8 flags, TM_FailureData *tmfd)
{
	if (kc_write_refused(rel, "row lock"))
		return TM_Invisible;
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("columnar tables do not support row locks")));
	return TM_Ok;
}

/*
 * A table created, or rewritten, in this transaction must have its rows in
 * stripes before the rewrite swaps the files, which happens before
 * pre-commit; any other relation keeps filling its buffer across commands.
 */
static void
kc_finish_bulk_insert(Relation rel, KcInsertOptions options)
{
	if (rel->rd_createSubid != InvalidSubTransactionId ||
		rel->rd_firstRelfilelocatorSubid != InvalidSubTransactionId)
		kc_write_flush_relation(rel);
}

/* ------------------------------------------------------------------------
 * DDL support
 * ------------------------------------------------------------------------
 */

/*
 * The creation of the table the new file is for.  The new file replaces older
 * content in three ways and the answer is the same in each: the table's own
 * creation is carried over, so that a snapshot older than the table, which
 * never saw any of it, is not told that something was replaced
 * (kc_check_snapshot_age).
 *
 *	 - a table made now: this transaction;
 *	 - TRUNCATE: the old file's (the caller passes it);
 *	 - a rewrite (ALTER TABLE, VACUUM FULL, SET ACCESS METHOD, REFRESH
 *	   MATERIALIZED VIEW): PostgreSQL makes a transient table named
 *	   "pg_temp_<oid of the table>" in the table's schema, holds the table
 *	   locked exclusively, and swaps the files in afterwards.  A columnar table
 *	   gives its own creation (and its file's rows are removed, once the swap
 *	   is done: kc_write_note_replaced).  A table of another access method
 *	   gives its creation only when that is known for certain: it was made in
 *	   this transaction, or it has no storage at all, so that no snapshot can
 *	   have seen content of it that the rewrite takes away.  The transaction
 *	   that wrote its pg_class row is no evidence: any GRANT, new owner or
 *	   storage option writes the row again, later than the creation, and a
 *	   snapshot between the two would then be told the table was empty while it
 *	   held rows.  Otherwise the creation is left unknown (InvalidTransactionId)
 *	   and the reader asks the catalog what the snapshot saw
 *	   (kc_check_snapshot_age), which gives the rows or the error, never a
 *	   guess.
 *
 * The name is only a hint, checked against the lock and the schema, so a
 * table that a user names like that gets the creation of an unrelated table at
 * worst, which makes the check stricter or equal to heap's and never lets a
 * snapshot read a row it should not.
 */
static TransactionId
kc_table_creation_xid(Relation rel)
{
	const char *name = RelationGetRelationName(rel);
	char	   *end;
	unsigned long oid;
	HeapTuple	tup;
	TransactionId xid;

	if (strncmp(name, "pg_temp_", 8) != 0 || name[8] < '1' || name[8] > '9')
		return GetCurrentTransactionId();
	errno = 0;
	oid = strtoul(name + 8, &end, 10);
	if (errno != 0 || *end != 0 || oid > PG_UINT32_MAX ||
		!CheckRelationOidLockedByMe((Oid) oid, AccessExclusiveLock, false))
		return GetCurrentTransactionId();

	tup = SearchSysCache1(RELOID, ObjectIdGetDatum((Oid) oid));
	if (!HeapTupleIsValid(tup))
		return GetCurrentTransactionId();
	if (((Form_pg_class) GETSTRUCT(tup))->relnamespace != RelationGetNamespace(rel))
	{
		ReleaseSysCache(tup);
		return GetCurrentTransactionId();
	}
	if (kc_am_is_columnar(((Form_pg_class) GETSTRUCT(tup))->relam))
	{
		Relation	old = relation_open((Oid) oid, NoLock);

		xid = kc_rel_ident(old)->table_xid;
		kc_write_note_replaced(old);
		relation_close(old, NoLock);
	}
	else
	{
		Relation	old = relation_open((Oid) oid, NoLock);

		if (old->rd_createSubid != InvalidSubTransactionId ||
			table_relation_size(old, MAIN_FORKNUM) == 0)
			xid = GetCurrentTransactionId();
		else
			xid = InvalidTransactionId;
		relation_close(old, NoLock);
	}
	ReleaseSysCache(tup);
	return xid;
}

static void
kc_relation_set_new_filelocator(Relation rel, const RelFileLocator *newrlocator,
								char persistence, TransactionId *freezeXid,
								MultiXactId *minmulti)
{
	KcOptions	opts;
	SMgrRelation srel;
	bool		truncating = false;
	TransactionId table_xid = InvalidTransactionId;

	/*
	 * A new file means catalog rows, a file and WAL: nothing PostgreSQL allows
	 * where an error is forbidden.  Refused, the caller gets no file and ids that
	 * name none; no command of PostgreSQL runs at such a moment (the executor and
	 * the catalog code need a transaction in progress), so only a module that
	 * calls the access method itself gets here.
	 */
	if (kc_write_refused(rel, "new file"))
	{
		*freezeXid = InvalidTransactionId;
		*minmulti = InvalidMultiXactId;
		return;
	}
	if (persistence != RELPERSISTENCE_PERMANENT)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("columnar tables cannot be temporary or unlogged")));
	kc_require_rmgr();

	kc_options_for_new_table(&opts);

	/*
	 * TRUNCATE gives an existing table a new file: keep its options, drop
	 * the old file's stripe rows (an abort of the transaction brings them
	 * back with the old file), and forget the rows buffered for the old
	 * file.  Rows buffered by an outer subtransaction would have to come
	 * back if this subtransaction aborts, which this version does not track.
	 */
	if (rel->rd_locator.relNumber != newrlocator->relNumber &&
		smgrexists(RelationGetSmgr(rel), MAIN_FORKNUM) &&
		RelationGetNumberOfBlocks(rel) > 0)
	{
		KcMetaData	m;

		truncating = true;
		table_xid = kc_rel_ident(rel)->table_xid;
		kc_meta_read(rel, &m);
		opts.codec = m.codec;
		opts.level = m.level;
		opts.stripe_rows = m.stripe_rows;
		opts.chunk_rows = m.chunk_rows;

		kc_catalog_delete_uid(kc_rel_ident(rel)->uid);
	}

	/* a table with a relfrozenxid has a relminmxid; it holds no multixact */
	*minmulti = GetOldestMultiXactId();

	srel = RelationCreateStorage(*newrlocator, persistence, true);
	smgrclose(srel);
	if (!truncating)
		table_xid = kc_table_creation_xid(rel);
	kc_meta_init_storage(rel, newrlocator, &opts, truncating, table_xid);

	/*
	 * The new file holds up to two transaction ids in its metapage: that of the
	 * creating transaction, and the table's own (table_xid, which a truncate or
	 * a rewrite keeps from the old file, so it can be older).  relfrozenxid must
	 * not pass either of them, or it would claim more than the file holds (C10-04;
	 * VACUUM freezes both and moves relfrozenxid on, kc_vacuum.c).
	 */
	*freezeXid = GetCurrentTransactionId();
	if (TransactionIdIsNormal(table_xid) && TransactionIdPrecedes(table_xid, *freezeXid))
		*freezeXid = table_xid;
}

/*
 * Empties the table's current file in place (PostgreSQL calls this for a table
 * created or given a new file in the current subtransaction, and for a temporary
 * table, which a columnar table cannot be; it is refused for any other table,
 * below).  The order of the steps is the one that leaves a readable table when
 * one of them fails: the buffered rows are forgotten, then the stripe catalog
 * rows are deleted (an ordinary catalog write, which an error undoes with the
 * transaction and which is where an error is likely), then the file is
 * shortened to its metapage and the metapage reset, which only fail on an I/O
 * error.  Where an error is forbidden none of it is done (a half-done truncate
 * is a table that no reader accepts: the file shortened and the rows still in
 * the catalog, or the other way round).
 */
static void
kc_relation_nontransactional_truncate(Relation rel)
{
	bool		outer;

	if (kc_write_refused(rel, "truncate"))
		return;

	/*
	 * Only for a relation whose file an abort removes anyway (C10-05): one
	 * created, or given a new file, in the CURRENT SUBTRANSACTION (C11-04: a table
	 * of an outer subtransaction is not enough, an abort of this one would not
	 * remove its file).  That is the only case PostgreSQL itself calls this for
	 * (ExecuteTruncateGuts compares rd_createSubid and rd_newRelfilelocatorSubid
	 * with the current subtransaction id; a columnar table cannot be temporary).  For any other
	 * table the steps below destroy committed data outside the transaction: the
	 * file is cut and the metapage reset for good, while an abort brings the
	 * catalog rows back, and the table is then unreadable.  Where an error is
	 * allowed that is an error; at the moments where it is not, the refusal
	 * above has already warned and nothing is changed.
	 */
	if (rel->rd_createSubid != GetCurrentSubTransactionId() &&
		rel->rd_newRelfilelocatorSubid != GetCurrentSubTransactionId())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("cannot truncate columnar table \"%s\" in place: it was not created or given a new file in the current (sub)transaction",
						RelationGetRelationName(rel)),
				 errdetail("An in-place truncate cannot be rolled back; the table would lose committed data for good if the transaction aborted."),
				 errhint("Use TRUNCATE, which gives the table a new file.")));
	kc_write_discard_relation(RelationGetRelid(rel), &outer);

	/*
	 * Stripe rows that this very command wrote (the commit-time flush, at
	 * pre-commit, runs in the command that called us) are not visible to a
	 * delete of the same command: "attempted to delete invisible tuple".
	 */
	CommandCounterIncrement();
	kc_catalog_delete_uid(kc_rel_ident(rel)->uid);
	RelationTruncate(rel, 1);
	kc_meta_reset(rel);
}

static void
kc_relation_copy_data(Relation rel, const RelFileLocator *newrlocator)
{
	SMgrRelation dstrel;

	/* a new file and the drop of the old: nothing where an error is forbidden */
	if (kc_write_refused(rel, "file copy"))
		return;

	/* the copy reads the file, so the cached pages go to it first */
	FlushRelationBuffers(rel);
	dstrel = RelationCreateStorage(*newrlocator, rel->rd_rel->relpersistence, true);
	RelationCopyStorage(RelationGetSmgr(rel), dstrel, MAIN_FORKNUM,
						rel->rd_rel->relpersistence);
	smgrclose(dstrel);
	RelationDropStorage(rel);

	/*
	 * The copy keeps the identity in its metapage, so the stripes follow the
	 * file with nothing to update, and a snapshot older than the move still
	 * sees them.
	 */
	;
}

static void
kc_relation_copy_for_cluster(KC_COPY_FOR_CLUSTER_PARAMS)
{
	KcMetaData	m;
	KcOptions	opts;
	KcScan	   *scan;
	TupleTableSlot *slot;
	bool		keep_visibility;
	double		n = 0;

	/* a rewrite writes stripes, catalog rows and a file: refused, nothing was copied */
	if (kc_write_refused(NewTable, "rewrite"))
	{
		*xid_cutoff = OldestXmin;
		*multi_cutoff = InvalidMultiXactId;
		*num_tuples = 0;
		*tups_vacuumed = 0;
		*tups_recently_dead = 0;
		return;
	}
	if (OldIndex != NULL || use_sort)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("CLUSTER is not supported on columnar tables: it would reorder the rows, and an index names rows by their number")));
	kc_require_rmgr();

	/*
	 * VACUUM FULL does not change what the table contains.  When every
	 * stripe row is old enough that every snapshot sees it (or is dead), the
	 * new stripes get frozen catalog rows and the new file no creating
	 * transaction, so a snapshot that predates the rewrite reads the new file
	 * exactly as it read the old one, as it does for heap.  Otherwise some
	 * older snapshot is meant to see a different set of stripes and the
	 * ordinary rule applies: it fails with a serialization failure.
	 */
	keep_visibility = kc_catalog_all_visible(OldTable, OldestXmin);
	*xid_cutoff = OldestXmin;
	*multi_cutoff = GetOldestMultiXactId();

	/* the new table keeps the options of the old one */
	kc_meta_read(OldTable, &m);
	opts.codec = m.codec;
	opts.level = m.level;
	opts.stripe_rows = m.stripe_rows;
	opts.chunk_rows = m.chunk_rows;
	kc_meta_set_options(NewTable, &opts);
	if (keep_visibility)
		kc_write_set_frozen(NewTable, true);

	/*
	 * The stripe rows of the old file are removed when the transaction
	 * commits (kc_write_cleanup_replaced).  The old table was noted when the
	 * transient table got its file (kc_table_creation_xid); noting it again
	 * costs nothing and does not depend on the name of the transient table.
	 */
	kc_write_note_replaced(OldTable);

	scan = kc_scan_begin(OldTable, NULL, NULL, NULL, 0);
	slot = table_slot_create(OldTable, NULL);
	while (kc_scan_next(scan, slot))
	{
		slot_getallattrs(slot);
		kc_write_row_values(NewTable, slot->tts_values, slot->tts_isnull,
							GetCurrentCommandId(true), NULL);
		n++;
		CHECK_FOR_INTERRUPTS();
	}
	ExecDropSingleTupleTableSlot(slot);
	kc_scan_end(scan);

	/* the swap of the files comes before commit, so the stripes go now */
	kc_write_flush_relation(NewTable);
	if (keep_visibility)
		kc_meta_set_created_xid(NewTable, FrozenTransactionId);

	/*
	 * The rewritten table's relfrozenxid is the id returned here (the swap of
	 * the files copies it).  The new metapage keeps the table's own id of the
	 * old file, which can be older than OldestXmin, so the id handed back is
	 * not newer than it (C10-04, the same rule as for a new file): relfrozenxid
	 * must not claim more than the file holds.  The creating id of the new file
	 * is the id of this transaction, which is not older than OldestXmin.
	 */
	{
		TransactionId tx = kc_rel_ident(NewTable)->table_xid;

		if (TransactionIdIsNormal(tx) && TransactionIdPrecedes(tx, *xid_cutoff))
			*xid_cutoff = tx;
	}

	*num_tuples = n;
	*tups_vacuumed = 0;
	*tups_recently_dead = 0;
}

/* ------------------------------------------------------------------------
 * ANALYZE
 * ------------------------------------------------------------------------
 */

/*
 * Take the next block the sampler chose.  Returning false means the sample
 * is exhausted; a block that stands for no rows (the metapage, a page of an
 * unreferenced stripe) is returned and simply yields no tuples.
 */
static bool
kc_scan_analyze_next_block(TableScanDesc sscan, ReadStream *stream)
{
	KcScan	   *scan = (KcScan *) sscan;
	Buffer		buf = read_stream_next_buffer(stream, NULL);

	if (!BufferIsValid(buf))
		return false;
	(void) kc_scan_analyze_block(scan, buf);
	ReleaseBuffer(buf);
	return true;
}

static bool
kc_scan_analyze_next_tuple(KC_ANALYZE_NEXT_TUPLE_PARAMS)
{
	if (kc_scan_analyze_next((KcScan *) scan, slot))
	{
		*liverows += 1;
		return true;
	}
	return false;
}

/* ------------------------------------------------------------------------
 * Index builds, sampling, sizes and estimates
 * ------------------------------------------------------------------------
 */

static double
kc_index_build_range_scan(Relation table_rel, Relation index_rel,
						  struct IndexInfo *index_info, bool allow_sync, bool anyvisible,
						  bool progress, BlockNumber start_blockno,
						  BlockNumber numblocks, IndexBuildCallback callback,
						  void *callback_state, TableScanDesc scan)
{
	KcMetaData	meta;
	EState	   *estate;
	ExprContext *econtext;
	TupleTableSlot *slot;
	ExprState  *predicate;
	KcScan	   *kscan;
	Datum		values[INDEX_MAX_KEYS];
	bool		isnull[INDEX_MAX_KEYS];
	double		reltuples = 0;

	if (kc_write_refused(table_rel, "index build"))
		return 0;
	if (index_rel == NULL || index_info == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("an index build needs an index: columnar tables are indexed by CREATE INDEX only")));

	/*
	 * What a build can serve in this version (C6b): a B-tree, not unique, not
	 * concurrent, of the whole table, on a file of format version 3.  Everything
	 * else is refused here, before anything is written, in the statement itself.
	 */
	if (index_rel->rd_rel->relam != BTREE_AM_OID)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("columnar tables support B-tree indexes only"),
				 errdetail("The index \"%s\" is of another access method.", RelationGetRelationName(index_rel)),
				 errhint("Use a B-tree index, or keep the table in row storage.")));
	if (index_info->ii_Unique)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("unique indexes, primary keys and unique constraints are not supported on columnar tables yet"),
				 errdetail("The index \"%s\" on \"%s\" is unique.  Uniqueness needs the wait on a running writer (increment C6c).",
						   RelationGetRelationName(index_rel), RelationGetRelationName(table_rel)),
				 errhint("Create a non-unique index, or keep the table in row storage.")));
	if (index_info->ii_ExclusionOps != NULL)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("exclusion constraints are not supported on columnar tables"),
				 errdetail("The index \"%s\" on \"%s\" enforces one; that needs a fetch with a dirty snapshot (increment C6c).",
						   RelationGetRelationName(index_rel), RelationGetRelationName(table_rel))));
	if (index_info->ii_Concurrent)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("CREATE INDEX CONCURRENTLY and REINDEX CONCURRENTLY are not supported on columnar tables"),
				 errhint("Use CREATE INDEX or REINDEX, which lock the table against writes while they run.")));
	if (start_blockno != 0 || numblocks != InvalidBlockNumber)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("an index of a columnar table is built over the whole table")));
	kc_meta_read(table_rel, &meta);
	if (meta.version < KC_META_VERSION_RESERVE)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("columnar table \"%s\" cannot have an index: its file was written by version 0.1 (format version %u)",
						RelationGetRelationName(table_rel), meta.version),
				 errhint("Rewrite the table first: VACUUM FULL, or ALTER TABLE ... SET ACCESS METHOD columnar (a new file of format version 3).")));

	/* the rows of this transaction are written, with numbers, before the scan reads stripes */
	if (scan == NULL)
		kc_write_flush_for_index(table_rel);

	estate = CreateExecutorState();
	econtext = GetPerTupleExprContext(estate);
	slot = table_slot_create(table_rel, NULL);
	econtext->ecxt_scantuple = slot;
	predicate = ExecPrepareQual(index_info->ii_Predicate, estate);

	if (scan != NULL)
		kscan = (KcScan *) scan;	/* a parallel build: the stripes are shared out */
	else
		kscan = kc_scan_begin(table_rel, NULL, NULL, NULL, KC_SCAN_MAINTENANCE);
	while (kc_scan_next(kscan, slot))
	{
		CHECK_FOR_INTERRUPTS();
		MemoryContextReset(econtext->ecxt_per_tuple_memory);
		reltuples += 1;
		if (predicate != NULL && !ExecQual(predicate, econtext))
			continue;
		FormIndexDatum(index_info, slot, estate, values, isnull);
		callback(index_rel, &slot->tts_tid, values, isnull, true, callback_state);
	}
	if (scan == NULL)
		kc_scan_end(kscan);
	ExecDropSingleTupleTableSlot(slot);
	FreeExecutorState(estate);
	index_info->ii_ExpressionsState = NIL;
	index_info->ii_PredicateState = NULL;
	return reltuples;
}

static void
kc_index_validate_scan(Relation table_rel, Relation index_rel,
					   struct IndexInfo *index_info, Snapshot snapshot,
					   struct ValidateIndexState *state)
{
	if (kc_write_refused(table_rel, "index validation"))
		return;
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("indexes on columnar tables are not supported in this version")));
}

/*
 * A columnar table keeps nothing in a TOAST table: values are detoasted
 * when a row is written, and stripes hold them whole.  The table still gets
 * an (always empty) TOAST table when it has a column that heap would
 * toast, because a rewrite creates the new table's TOAST table only when
 * the old one had one, and ALTER TABLE ... SET ACCESS METHOD heap must
 * give the heap table the means to store a row over 8 kB.
 */
static bool
kc_relation_needs_toast_table(Relation rel)
{
	TupleDesc	desc = RelationGetDescr(rel);
	int			i;

	for (i = 0; i < desc->natts; i++)
	{
		Form_pg_attribute att = TupleDescAttr(desc, i);

		if (!att->attisdropped && att->attlen < 0 &&
			att->attstorage != TYPSTORAGE_PLAIN)
			return true;
	}
	return false;
}

/* The TOAST table of a columnar table is an ordinary heap table. */
static Oid
kc_relation_toast_am(Relation rel)
{
	return HEAP_TABLE_AM_OID;
}

static bool
kc_scan_sample_next_block(TableScanDesc scan, struct SampleScanState *scanstate)
{
	if (kc_write_refused(scan->rs_rd, "sampling"))
		return false;
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("TABLESAMPLE is not supported on columnar tables")));
	return false;
}

static bool
kc_scan_sample_next_tuple(TableScanDesc scan, struct SampleScanState *scanstate,
						  TupleTableSlot *slot)
{
	if (kc_write_refused(scan->rs_rd, "sampling"))
		return false;
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("TABLESAMPLE is not supported on columnar tables")));
	return false;
}

/*
 * The file holds compressed bytes, so the width of a row says nothing about
 * the number of rows.  With statistics, use their density as heap does; with
 * none (never vacuumed or analyzed) count the rows in the stripe catalog.
 */
static void
kc_relation_estimate_size(Relation rel, int32 *attr_widths, BlockNumber *pages,
						  double *tuples, double *allvisfrac)
{
	BlockNumber curpages = RelationGetNumberOfBlocks(rel);
	BlockNumber relpages = (BlockNumber) rel->rd_rel->relpages;
	double		reltuples = (double) rel->rd_rel->reltuples;

	*pages = curpages;
	*allvisfrac = 0;
	if (curpages == 0)
	{
		*tuples = 0;
		return;
	}
	if (relpages > 0 && reltuples >= 0)
	{
		*tuples = rint(reltuples / (double) relpages * curpages);
		return;
	}
	*tuples = (double) kc_catalog_row_total(rel, NULL, NULL, NULL, NULL);
}

/* ------------------------------------------------------------------------
 * What each callback does where an error is forbidden
 * ------------------------------------------------------------------------
 *
 * Every member of the routine the extension sets has a decision here, one of:
 *
 *	 guarded    asks kc_entry_refused first and, where the server forbids an
 *	            error (the end of a transaction, a critical section), does
 *	            nothing but warn and answer "nothing" (README, "Writes while a
 *	            transaction is ending");
 *	 reads      reads the table or the catalog and changes nothing; it works
 *	            wherever PostgreSQL lets a heap table be read (a release phase of
 *	            a resource owner is not one: the buffer pin of any table access
 *	            method fails there), so there is nothing to refuse;
 *	 nostorage  touches neither the table nor the catalog: pure, or a no-op.
 *
 * The table is data on purpose.  The test module of the tests (t/kcevents,
 * t/031_entry_points.pl) walks the members of the routine that the server
 * gets from the handler and fails for a member that has no row here, so a
 * callback that is added to the routine cannot be forgotten.  The offset is
 * what ties a row to a member of the struct.
 */
typedef struct KcAmDecision
{
	const char *member;
	Size		offset;
	const char *decision;
	const char *why;
} KcAmDecision;

#define KC_AMD(member, decision, why) \
	{ #member, offsetof(TableAmRoutine, member), decision, why }

static const KcAmDecision kc_am_decisions[] = {
	KC_AMD(slot_callbacks, "nostorage", "returns the slot ops"),
	KC_AMD(scan_begin, "reads", "reads like heap; refuses nothing"),
	KC_AMD(scan_end, "reads", "releases what the scan held"),
	KC_AMD(scan_rescan, "reads", "restarts a read"),
	KC_AMD(scan_getnextslot, "reads", "reads like heap"),
	KC_AMD(parallelscan_estimate, "nostorage", "a constant"),
	KC_AMD(parallelscan_initialize, "nostorage", "fills a descriptor in memory"),
	KC_AMD(parallelscan_reinitialize, "nostorage", "does nothing"),
	KC_AMD(index_fetch_begin, "guarded", "no index can exist; an error, or a warning where none is allowed"),
	KC_AMD(index_fetch_reset, "nostorage", "does nothing"),
	KC_AMD(index_fetch_end, "nostorage", "does nothing"),
	KC_AMD(index_fetch_tuple, "guarded", "no index can exist; an error, or a warning where none is allowed"),
	KC_AMD(tuple_fetch_row_version, "guarded", "a row has no TID before its stripe is written"),
	KC_AMD(tuple_tid_valid, "guarded", "a TID scan is refused"),
	KC_AMD(tuple_get_latest_tid, "nostorage", "does nothing"),
	KC_AMD(tuple_satisfies_snapshot, "nostorage", "answers true"),
	KC_AMD(index_delete_tuples, "guarded", "no index can exist"),
	KC_AMD(tuple_insert, "guarded", "writes rows"),
	KC_AMD(tuple_insert_speculative, "guarded", "an error where one is allowed"),
	KC_AMD(tuple_complete_speculative, "guarded", "an error where one is allowed"),
	KC_AMD(multi_insert, "guarded", "writes rows"),
	KC_AMD(tuple_delete, "guarded", "append-only: an error, or TM_Invisible and a warning"),
	KC_AMD(tuple_update, "guarded", "append-only: an error, or TM_Invisible and a warning"),
	KC_AMD(tuple_lock, "guarded", "no row locks: an error, or TM_Invisible and a warning"),
	KC_AMD(finish_bulk_insert, "guarded", "flushes buffers; where an error is forbidden the flush is left to the pre-commit"),
	KC_AMD(relation_set_new_filelocator, "guarded", "creates a file and catalog rows"),
	KC_AMD(relation_nontransactional_truncate, "guarded", "deletes catalog rows and shortens the file"),
	KC_AMD(relation_copy_data, "guarded", "creates a file"),
	KC_AMD(relation_copy_for_cluster, "guarded", "writes stripes into a new file"),
	KC_AMD(relation_vacuum, "guarded", "deletes catalog rows and shortens the file"),
	KC_AMD(scan_analyze_next_block, "reads", "reads a block"),
	KC_AMD(scan_analyze_next_tuple, "reads", "decodes a row"),
	KC_AMD(index_build_range_scan, "guarded", "no index can be built"),
	KC_AMD(index_validate_scan, "guarded", "no index can be built"),
	KC_AMD(relation_size, "reads", "the size of the file (table_block_relation_size)"),
	KC_AMD(relation_needs_toast_table, "nostorage", "looks at the row type"),
	KC_AMD(relation_toast_am, "nostorage", "a constant"),
	KC_AMD(relation_estimate_size, "reads", "the size of the file and the catalog row counts"),
	KC_AMD(scan_sample_next_block, "guarded", "TABLESAMPLE is refused"),
	KC_AMD(scan_sample_next_tuple, "guarded", "TABLESAMPLE is refused"),
};

/*
 * The decision recorded for the member of the routine at this offset, or NULL
 * when there is none; *member names it.  Exported for the test module.
 */
PGDLLEXPORT const char *kc_am_decision(Size offset, const char **member, const char **why);

const char *
kc_am_decision(Size offset, const char **member, const char **why)
{
	int			i;

	for (i = 0; i < lengthof(kc_am_decisions); i++)
	{
		if (kc_am_decisions[i].offset == offset)
		{
			*member = kc_am_decisions[i].member;
			*why = kc_am_decisions[i].why;
			return kc_am_decisions[i].decision;
		}
	}
	return NULL;
}

/* ------------------------------------------------------------------------
 * The routine
 * ------------------------------------------------------------------------
 */

static const TableAmRoutine kc_methods = {
	.type = T_TableAmRoutine,

	.slot_callbacks = kc_slot_callbacks,

	.scan_begin = kc_scan_begin_cb,
	.scan_end = kc_scan_end_cb,
	.scan_rescan = kc_scan_rescan_cb,
	.scan_getnextslot = kc_getnextslot,

	.parallelscan_estimate = kc_parallelscan_estimate,
	.parallelscan_initialize = kc_parallelscan_initialize,
	.parallelscan_reinitialize = kc_parallelscan_reinitialize,

	.index_fetch_begin = kc_index_fetch_begin,
	.index_fetch_reset = kc_index_fetch_reset,
	.index_fetch_end = kc_index_fetch_end,
	.index_fetch_tuple = kc_index_fetch_tuple,

	.tuple_fetch_row_version = kc_fetch_row_version,
	.tuple_tid_valid = kc_tuple_tid_valid,
	.tuple_get_latest_tid = kc_tuple_get_latest_tid,
	.tuple_satisfies_snapshot = kc_tuple_satisfies_snapshot,
	.index_delete_tuples = kc_index_delete_tuples,

	.tuple_insert = kc_tuple_insert,
	.tuple_insert_speculative = kc_tuple_insert_speculative,
	.tuple_complete_speculative = kc_tuple_complete_speculative,
	.multi_insert = kc_multi_insert,
	.tuple_delete = kc_tuple_delete,
	.tuple_update = kc_tuple_update,
	.tuple_lock = kc_tuple_lock,
	.finish_bulk_insert = kc_finish_bulk_insert,

	.relation_set_new_filelocator = kc_relation_set_new_filelocator,
	.relation_nontransactional_truncate = kc_relation_nontransactional_truncate,
	.relation_copy_data = kc_relation_copy_data,
	.relation_copy_for_cluster = kc_relation_copy_for_cluster,
	.relation_vacuum = kc_relation_vacuum,
	.scan_analyze_next_block = kc_scan_analyze_next_block,
	.scan_analyze_next_tuple = kc_scan_analyze_next_tuple,
	.index_build_range_scan = kc_index_build_range_scan,
	.index_validate_scan = kc_index_validate_scan,

	.relation_size = table_block_relation_size,
	.relation_needs_toast_table = kc_relation_needs_toast_table,
	.relation_toast_am = kc_relation_toast_am,

	.relation_estimate_size = kc_relation_estimate_size,

	.scan_sample_next_block = kc_scan_sample_next_block,
	.scan_sample_next_tuple = kc_scan_sample_next_tuple,
};

const TableAmRoutine *
kc_get_routine(void)
{
	return &kc_methods;
}
