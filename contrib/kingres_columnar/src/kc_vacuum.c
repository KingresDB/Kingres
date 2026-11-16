/*-------------------------------------------------------------------------
 *
 * kc_vacuum.c
 *	  VACUUM of a columnar table and the compaction of small stripes.
 *
 * There is nothing to prune or freeze (no dead rows, no row transaction
 * ids), so VACUUM does four things: it records the page and row counts, it
 * gives the unreferenced tail of the file back to the operating system, it
 * removes the stripe catalog rows of files no relation uses any more, and it
 * reports itself to the statistics collector, which resets the counter that
 * triggers autovacuum after inserts.  Space in the middle of the file is not
 * reused (README, "Limits"); VACUUM FULL rewrites the table and returns it.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kc_vacuum.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/genam.h"
#include "access/heapam.h"
#include "access/htup_details.h"
#include "access/multixact.h"
#include "access/table.h"
#include "access/xact.h"
#include "catalog/storage.h"
#include "commands/vacuum.h"
#include "executor/executor.h"
#include "miscadmin.h"
#include "pgstat.h"
#include "storage/bufmgr.h"
#include "storage/lmgr.h"
#include "storage/procarray.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"
#include "utils/timestamp.h"

#include "kingres_columnar.h"

/*
 * The first block past the last stripe that anything may still need: a
 * stripe whose catalog row is live, in progress, or deleted but not yet
 * invisible to every snapshot.  The caller holds the relation exclusively,
 * so no writer is in the middle of a flush.
 */
static BlockNumber
kc_needed_blocks(Relation rel)
{
	Relation	cat = table_open(kc_catalog_oid(), AccessShareLock);
	TableScanDesc scan;
	HeapTuple	tup;
	TransactionId oldest = GetOldestNonRemovableTransactionId(rel);
	uint64		maxend = 1;

	scan = KC_TABLE_BEGINSCAN(cat, SnapshotAny);
	while ((tup = heap_getnext(scan, ForwardScanDirection)) != NULL)
	{
		HeapScanDesc hscan = (HeapScanDesc) scan;
		Buffer		buf = hscan->rs_cbuf;
		HTSV_Result res;
		bool		isnull;
		Datum		d;

		LockBuffer(buf, BUFFER_LOCK_SHARE);
		res = HeapTupleSatisfiesVacuum(tup, oldest, buf);
		LockBuffer(buf, BUFFER_LOCK_UNLOCK);
		if (res == HEAPTUPLE_DEAD)
			continue;
		d = heap_getattr(tup, 1, RelationGetDescr(cat), &isnull);
		if (isnull || (uint64) DatumGetInt64(d) != kc_rel_ident(rel)->uid)
			continue;
		{
			uint64		first = DatumGetInt64(heap_getattr(tup, 7, RelationGetDescr(cat), &isnull));
			uint32		cnt = DatumGetInt32(heap_getattr(tup, 8, RelationGetDescr(cat), &isnull));

			if (first + cnt > maxend)
				maxend = first + cnt;
		}
	}
	table_endscan(scan);
	table_close(cat, AccessShareLock);
	return (BlockNumber) Min(maxend, (uint64) MaxBlockNumber);
}

void
kc_relation_vacuum(KC_VACUUM_PARAMS)
{
	BlockNumber nblocks;
	uint64		rows;
	TimestampTz starttime = GetCurrentTimestamp();
	bool		pushed = false;
	TransactionId oldest;
	TransactionId remaining;

	if (!kc_rel_is_columnar(rel))
		return;

	/* the catalog reads below need a snapshot */
	if (!ActiveSnapshotSet())
	{
		PushActiveSnapshot(GetTransactionSnapshot());
		pushed = true;
	}

	/*
	 * Give back the unreferenced tail, if no one is using the table.  The
	 * lock is conditional, as lazy_truncate_heap does, so VACUUM never waits
	 * behind a query; on a standby the lock is WAL-logged and cancels
	 * conflicting queries.
	 */
	if (params->truncate != VACOPTVALUE_DISABLED &&
		ConditionalLockRelation(rel, AccessExclusiveLock))
	{
		BlockNumber need = kc_needed_blocks(rel);

		nblocks = RelationGetNumberOfBlocks(rel);
		if (need < nblocks)
			RelationTruncate(rel, need);
		UnlockRelation(rel, AccessExclusiveLock);
	}

	(void) kc_catalog_reconcile();

	nblocks = RelationGetNumberOfBlocks(rel);
	rows = kc_catalog_row_total(rel, NULL, NULL, NULL, NULL);

	/*
	 * The only transaction id a columnar file holds is the one in its
	 * metapage (the stripes are visible through the catalog, whose rows are
	 * frozen by the vacuum of the catalog).  Freeze it when it is old enough,
	 * and tell PostgreSQL the oldest id that is left, so that the table takes
	 * part in the protection against transaction id wraparound as a heap
	 * table does.
	 */
	oldest = GetOldestNonRemovableTransactionId(rel);
	remaining = kc_meta_freeze(rel, oldest);
	vac_update_relstats(rel, nblocks, (double) rows, 0, 0, false,
						TransactionIdIsValid(remaining) ? remaining : oldest,
						GetOldestMultiXactId(), NULL, NULL, false);
	KC_REPORT_VACUUM(rel, (PgStat_Counter) rows, 0, starttime);
	if (pushed)
		PopActiveSnapshot();
}

/*
 * Merge the stripes of fewer than min_rows rows into full ones.  The rows
 * are read from the small stripes, written through the ordinary write buffer,
 * and the old stripes' catalog rows are deleted in the same transaction:
 * readers see the old stripes or the new ones, never both.  The pages of the
 * old stripes stay allocated until the tail is truncated or the table is
 * rewritten.  Returns the number of stripes written.
 *
 * Invariant: no operation changes which rows a transaction's snapshot sees, for
 * that transaction or any other.  The stripes this writes are the compacting
 * transaction's own, and a transaction sees all of its own stripes, so the rows
 * it copies are exactly the rows it sees afterwards: they must be the rows
 * its snapshot sees now.
 *
 *	 - READ COMMITTED: every statement takes a new snapshot, so the rows seen
 *	   now are the rows committed now, and the sources are the stripes that are
 *	   committed (a fresh snapshot, the maintenance rule).  A command that is
 *	   already running reads the stripes it replaced and not the copies (the
 *	   copies carry the command id of the compaction).
 *	 - REPEATABLE READ and SERIALIZABLE: the transaction keeps the snapshot of
 *	   its first statement, and the copies would be read with it.  Only the
 *	   stripes that snapshot sees are copied and deleted; a stripe committed
 *	   after it is neither read nor touched, so it stays invisible to this
 *	   transaction, as before, and stays for every other snapshot, which sees
 *	   the sources until this transaction commits and the copies after.  A
 *	   source that another transaction deleted since (it committed after the
 *	   snapshot) cannot be deleted again: a serialization failure, as heap
 *	   gives for a row updated concurrently.
 *
 * Other snapshots cannot tell the difference at any time: before this commits
 * they see the sources and not the copies (the copies' creator is in progress
 * or newer); after it they see the copies and not the sources.  The copies hold
 * the rows of the sources and no others, because the sources are only the
 * stripes the transaction's own snapshot sees.
 */
int
kc_compact(Relation rel, uint32 min_rows)
{
	KcScan	   *scan;
	TupleTableSlot *slot;
	int			i;
	int			nsmall = 0;
	uint64		rows = 0;
	KcMetaData	m;
	CommandId	cid = GetCurrentCommandId(true);

	kc_meta_read(rel, &m);
	scan = kc_scan_begin(rel, IsolationUsesXactSnapshot() ? GetTransactionSnapshot() : NULL,
						 NULL, NULL, KC_SCAN_MAINTENANCE);

	/* keep the small stripes of other transactions, in the scan's list */
	for (i = 0; i < scan->nstripes; i++)
	{
		KcStripeInfo *si = &scan->stripes[i];

		if (!si->own && si->row_count < min_rows)
			scan->stripes[nsmall++] = *si;
	}
	scan->nstripes = nsmall;
	kc_scan_drop_hold(scan);

	if (nsmall < 2)
	{
		kc_scan_end(scan);
		return 0;
	}

	/*
	 * The rows are copied through the ordinary write buffer, as maintenance:
	 * they are rows that readers see in the stripes being replaced, so the
	 * copy is no write for the serializable machinery (a reader needs no
	 * dependency on it, and heap's own VACUUM FULL and CLUSTER take none).
	 * Compaction that does not change the logical content cannot create a
	 * dependency, and one that is made together with a real write by the same
	 * transaction keeps that write's checks (kc_write.c).
	 */
	kc_write_maintenance_discard();
	slot = table_slot_create(rel, NULL);
	PG_TRY();
	{
		kc_write_set_maintenance(true);
		while (kc_scan_next(scan, slot))
		{
			slot_getallattrs(slot);
			kc_write_row_values(rel, slot->tts_values, slot->tts_isnull, cid);
			rows++;
			CHECK_FOR_INTERRUPTS();
		}
	}
	PG_FINALLY();
	{
		kc_write_set_maintenance(false);
	}
	PG_END_TRY();
	ExecDropSingleTupleTableSlot(slot);

	for (i = 0; i < nsmall; i++)
	{
		/* a source another transaction replaced since: its rows are copied twice, so stop */
		if (kc_catalog_delete_stripe(kc_rel_ident(rel)->uid,
									 scan->stripes[i].stripe_id) == 0)
			ereport(ERROR,
					(errcode(ERRCODE_T_R_SERIALIZATION_FAILURE),
					 errmsg("could not serialize access due to concurrent update"),
					 errdetail("A stripe of \"%s\" was replaced by another transaction while it was being compacted.",
							   RelationGetRelationName(rel))));
	}
	kc_scan_end(scan);
	kc_write_maintenance_flush(rel);
	return (int) ((rows + m.stripe_rows - 1) / m.stripe_rows);
}
