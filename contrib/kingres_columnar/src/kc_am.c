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
 * SPDX-License-Identifier: PostgreSQL
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kc_am.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include <math.h>

#include "access/heapam.h"
#include "access/multixact.h"
#include "access/relscan.h"
#include "access/tableam.h"
#include "access/xact.h"
#include "catalog/storage.h"
#include "commands/vacuum.h"
#include "executor/executor.h"
#include "miscadmin.h"
#include "tcop/pquery.h"
#include "storage/bufmgr.h"
#include "storage/read_stream.h"
#include "storage/smgr.h"
#include "utils/rel.h"
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

static TableScanDesc
kc_scan_begin_cb(Relation rel, Snapshot snapshot, int nkeys, struct ScanKeyData *key,
				 ParallelTableScanDesc pscan, uint32 flags)
{
	KcScan	   *scan;

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

static Size
kc_parallelscan_estimate(Relation rel)
{
	return sizeof(ParallelTableScanDescData);
}

static Size
kc_parallelscan_initialize(Relation rel, ParallelTableScanDesc pscan)
{
	pscan->phs_locator = rel->rd_locator;
	pscan->phs_syncscan = false;
	pscan->phs_snapshot_any = false;
	pscan->phs_snapshot_off = 0;
	return sizeof(ParallelTableScanDescData);
}

static void
kc_parallelscan_reinitialize(Relation rel, ParallelTableScanDesc pscan)
{
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

static IndexFetchTableData *
kc_index_fetch_begin(KC_INDEX_FETCH_BEGIN_PARAMS)
{
	kc_no_indexes();
}

static void
kc_index_fetch_reset(IndexFetchTableData *scan)
{
}

static void
kc_index_fetch_end(IndexFetchTableData *scan)
{
}

static bool
kc_index_fetch_tuple(IndexFetchTableData *scan, ItemPointer tid,
					 Snapshot snapshot, TupleTableSlot *slot, bool *call_again,
					 bool *all_dead)
{
	kc_no_indexes();
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
kc_set_insert_tid(TupleTableSlot *slot)
{
	ItemPointerSet(&slot->tts_tid, MaxBlockNumber - 1, 1);
}

static void
kc_tuple_insert(Relation rel, TupleTableSlot *slot, CommandId cid,
				KcInsertOptions options, struct BulkInsertStateData *bistate)
{
	kc_require_rmgr();
	kc_write_row(rel, slot, cid);
	slot->tts_tableOid = RelationGetRelid(rel);
	kc_set_insert_tid(slot);
}

static void
kc_tuple_insert_speculative(Relation rel, TupleTableSlot *slot, CommandId cid,
							KcInsertOptions options, struct BulkInsertStateData *bistate,
							uint32 specToken)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("INSERT ... ON CONFLICT DO UPDATE is not supported on columnar tables")));
}

static void
kc_tuple_complete_speculative(Relation rel, TupleTableSlot *slot,
							  uint32 specToken, bool succeeded)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("INSERT ... ON CONFLICT DO UPDATE is not supported on columnar tables")));
}

static void
kc_multi_insert(Relation rel, TupleTableSlot **slots, int nslots, CommandId cid,
				KcInsertOptions options, struct BulkInsertStateData *bistate)
{
	int			i;

	kc_require_rmgr();
	for (i = 0; i < nslots; i++)
	{
		kc_write_row(rel, slots[i], cid);
		slots[i]->tts_tableOid = RelationGetRelid(rel);
		kc_set_insert_tid(slots[i]);
	}
}

static TM_Result
kc_tuple_delete(KC_DELETE_PARAMS)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("columnar tables are append-only: UPDATE and DELETE are not supported")));
	return TM_Ok;
}

static TM_Result
kc_tuple_update(KC_UPDATE_PARAMS)
{
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

static void
kc_relation_set_new_filelocator(Relation rel, const RelFileLocator *newrlocator,
								char persistence, TransactionId *freezeXid,
								MultiXactId *minmulti)
{
	KcOptions	opts;
	SMgrRelation srel;
	bool		truncating = false;

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
		bool		outer;

		truncating = true;
		kc_meta_read(rel, &m);
		opts.codec = m.codec;
		opts.level = m.level;
		opts.stripe_rows = m.stripe_rows;
		opts.chunk_rows = m.chunk_rows;

		if (kc_write_has_outer_rows(RelationGetRelid(rel)))
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("cannot truncate columnar table \"%s\" in a subtransaction after rows were inserted in an outer one",
							RelationGetRelationName(rel))));
		kc_write_discard_relation(RelationGetRelid(rel), &outer);
		kc_catalog_delete_filenode(rel->rd_locator.relNumber);
	}
	(void) truncating;

	*freezeXid = InvalidTransactionId;
	*minmulti = InvalidMultiXactId;

	srel = RelationCreateStorage(*newrlocator, persistence, true);
	smgrclose(srel);
	kc_meta_init_storage(rel, newrlocator, &opts);
}

static void
kc_relation_nontransactional_truncate(Relation rel)
{
	bool		outer;

	kc_write_discard_relation(RelationGetRelid(rel), &outer);
	kc_catalog_delete_filenode(rel->rd_locator.relNumber);
	RelationTruncate(rel, 1);
	kc_meta_reset(rel);
}

static void
kc_relation_copy_data(Relation rel, const RelFileLocator *newrlocator)
{
	SMgrRelation dstrel;

	/* the copy reads the file, so the cached pages go to it first */
	FlushRelationBuffers(rel);
	dstrel = RelationCreateStorage(*newrlocator, rel->rd_rel->relpersistence, true);
	RelationCopyStorage(RelationGetSmgr(rel), dstrel, MAIN_FORKNUM,
						rel->rd_rel->relpersistence);
	smgrclose(dstrel);
	RelationDropStorage(rel);

	/* the stripes keep their blocks: their rows follow the new file */
	kc_catalog_rekey(rel->rd_locator.relNumber, newrlocator->relNumber);
}

static void
kc_relation_copy_for_cluster(KC_COPY_FOR_CLUSTER_PARAMS)
{
	KcMetaData	m;
	KcOptions	opts;
	KcScan	   *scan;
	TupleTableSlot *slot;
	double		n = 0;

	if (OldIndex != NULL || use_sort)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("CLUSTER needs an index; columnar tables have none in this version")));
	kc_require_rmgr();

	*xid_cutoff = InvalidTransactionId;
	*multi_cutoff = InvalidMultiXactId;

	/* the new table keeps the options of the old one */
	kc_meta_read(OldTable, &m);
	opts.codec = m.codec;
	opts.level = m.level;
	opts.stripe_rows = m.stripe_rows;
	opts.chunk_rows = m.chunk_rows;
	kc_meta_set_options(NewTable, &opts);

	scan = kc_scan_begin(OldTable, NULL, NULL, NULL, 0);
	slot = table_slot_create(OldTable, NULL);
	while (kc_scan_next(scan, slot))
	{
		slot_getallattrs(slot);
		kc_write_row_values(NewTable, slot->tts_values, slot->tts_isnull,
							GetCurrentCommandId(true));
		n++;
		CHECK_FOR_INTERRUPTS();
	}
	ExecDropSingleTupleTableSlot(slot);
	kc_scan_end(scan);

	/* the swap of the files comes before commit, so the stripes go now */
	kc_write_flush_relation(NewTable);

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
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("indexes on columnar tables are not supported in this version")));
	return 0;
}

static void
kc_index_validate_scan(Relation table_rel, Relation index_rel,
					   struct IndexInfo *index_info, Snapshot snapshot,
					   struct ValidateIndexState *state)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("indexes on columnar tables are not supported in this version")));
}

static bool
kc_relation_needs_toast_table(Relation rel)
{
	return false;
}

static bool
kc_scan_sample_next_block(TableScanDesc scan, struct SampleScanState *scanstate)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("TABLESAMPLE is not supported on columnar tables")));
	return false;
}

static bool
kc_scan_sample_next_tuple(TableScanDesc scan, struct SampleScanState *scanstate,
						  TupleTableSlot *slot)
{
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

	.relation_estimate_size = kc_relation_estimate_size,

	.scan_sample_next_block = kc_scan_sample_next_block,
	.scan_sample_next_tuple = kc_scan_sample_next_tuple,
};

const TableAmRoutine *
kc_get_routine(void)
{
	return &kc_methods;
}
