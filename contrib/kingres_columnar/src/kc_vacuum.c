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
#include "access/skey.h"
#include "access/table.h"
#include "access/xact.h"
#include "catalog/indexing.h"
#include "catalog/pg_class.h"
#include "catalog/storage.h"
#include "commands/dbcommands.h"
#include "commands/vacuum.h"
#include "executor/executor.h"
#include "miscadmin.h"
#include "pgstat.h"
#include "storage/bufmgr.h"
#include "storage/latch.h"
#include "storage/lmgr.h"
#include "storage/procarray.h"
#include "utils/fmgroids.h"
#include "utils/lsyscache.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"
#include "utils/timestamp.h"
#include "utils/wait_event.h"

#include "kingres_columnar.h"

/*
 * The first block past the last stripe that anything may still need: a
 * stripe whose catalog row is live, in progress, or deleted but not yet
 * invisible to every snapshot.  With the relation held exclusively no writer is
 * in the middle of a flush and the answer is exact; with the share-update lock
 * of the vacuum alone it is an estimate (pages a writer has appended and not yet
 * entered in the catalog look like a tail), good enough to decide whether the
 * exclusive lock is worth asking for, and checked again under it.
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

/*
 * The rule of heap's vacuum (should_attempt_truncation and lazy_truncate_heap
 * in vacuumlazy.c, the same in PostgreSQL 18 and 19), applied to the tail of
 * unreferenced pages:
 *
 *	 - a tail is worth removing when it is not empty and holds at least
 *	   KC_TRUNCATE_MINIMUM pages or a sixteenth of the file;
 *	 - not while the wraparound failsafe is active;
 *	 - the exclusive lock is asked for without waiting, retried every 50 ms for
 *	   5 seconds (checking for interrupts), after which the truncation is given
 *	   up for this visit;
 *	 - under the lock the file is measured again: if it has grown, give up (the
 *	   new pages belong to a writer); else the tail is worked out again, exactly
 *	   now that no writer can be in the middle of a flush, and removed.
 *
 * Where it differs from heap: the decision is made from the stripe catalog and
 * the file size, not from a scan that notes the last page with data, and heap's
 * loop that goes on while a lock waiter interrupted the scan of the tail has no
 * counterpart (finding the tail is one catalog read, which a waiter does not
 * wait behind).  The thresholds are heap's.
 */
#define KC_TRUNCATE_MINIMUM				1000
#define KC_TRUNCATE_FRACTION			16
#define KC_TRUNCATE_LOCK_WAIT_INTERVAL	50	/* ms */

static bool
kc_truncate_tail(Relation rel)
{
	BlockNumber orig;
	BlockNumber need;
	BlockNumber freeable;
	int			retry = 0;
	bool		cut = false;

	orig = RelationGetNumberOfBlocks(rel);
	need = kc_needed_blocks(rel);
	if (need >= orig)
		return false;
	freeable = orig - need;
	if (!(freeable >= KC_TRUNCATE_MINIMUM || freeable >= orig / KC_TRUNCATE_FRACTION))
		return false;

	while (!ConditionalLockRelation(rel, AccessExclusiveLock))
	{
		CHECK_FOR_INTERRUPTS();
		if (++retry > (kc_guc_truncate_lock_timeout / KC_TRUNCATE_LOCK_WAIT_INTERVAL))
		{
			ereport(DEBUG2,
					(errmsg("\"%s\": stopping truncate due to conflicting lock request",
							RelationGetRelationName(rel))));
			return false;
		}
		(void) WaitLatch(MyLatch,
						 WL_LATCH_SET | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH,
						 KC_TRUNCATE_LOCK_WAIT_INTERVAL,
						 WAIT_EVENT_VACUUM_TRUNCATE);
		ResetLatch(MyLatch);
	}

	/* the file grew while the lock was awaited: those pages are a writer's */
	if (RelationGetNumberOfBlocks(rel) == orig)
	{
		need = kc_needed_blocks(rel);
		if (need < orig)
		{
			RelationTruncate(rel, need);
			cut = true;
		}
	}
	UnlockRelation(rel, AccessExclusiveLock);
	return cut;
}

/*
 * The wraparound failsafe, decided for this table's own age the way heap does
 * (vacuum_xid_failsafe_check, which the core also uses for heap: the age of
 * relfrozenxid and relminmxid against vacuum_failsafe_age and
 * vacuum_multixact_failsafe_age, with the floor of 105 percent of the freeze
 * ages) (C11-05).  Nothing but a heap table of the same command set the flag
 * before, so a columnar table past the failsafe age was cut under an exclusive
 * lock, which is WAL-logged and so needs a transaction id, exactly where
 * the server may refuse to assign one.
 */
static bool
kc_failsafe_active(Relation rel)
{
	struct VacuumCutoffs cutoffs;

	memset(&cutoffs, 0, sizeof(cutoffs));
	cutoffs.relfrozenxid = rel->rd_rel->relfrozenxid;
	cutoffs.relminmxid = rel->rd_rel->relminmxid;
	/* a table with no ages yet (nothing recorded) is not in danger */
	if (!TransactionIdIsNormal(cutoffs.relfrozenxid) ||
		!MultiXactIdIsValid(cutoffs.relminmxid))
		return false;
	if (!vacuum_xid_failsafe_check(&cutoffs))
		return false;
	VacuumFailsafeActive = true;
	ereport(WARNING,
			(errmsg("bypassing nonessential maintenance of table \"%s.%s.%s\" as a failsafe",
					get_database_name(MyDatabaseId),
					get_namespace_name(RelationGetNamespace(rel)),
					RelationGetRelationName(rel)),
			 errdetail("The table's relfrozenxid or relminmxid is too far in the past.")));
	return true;
}

/*
 * The relfrozenxid that pg_class holds for the table now: vac_update_relstats
 * only moves it forward, so after a visit it can be later than the id the
 * vacuum passed in, and the log line says what is stored (C10-04).  The change
 * is made in place on the tuple, so a scan sees it at once.
 */
static TransactionId
kc_stored_relfrozenxid(Relation rel)
{
	Relation	pgclass = table_open(RelationRelationId, AccessShareLock);
	ScanKeyData key;
	SysScanDesc scan;
	HeapTuple	tup;
	TransactionId result = InvalidTransactionId;

	ScanKeyInit(&key, Anum_pg_class_oid, BTEqualStrategyNumber, F_OIDEQ,
				ObjectIdGetDatum(RelationGetRelid(rel)));
	scan = systable_beginscan(pgclass, ClassOidIndexId, true, NULL, 1, &key);
	tup = systable_getnext(scan);
	if (HeapTupleIsValid(tup))
		result = ((Form_pg_class) GETSTRUCT(tup))->relfrozenxid;
	systable_endscan(scan);
	table_close(pgclass, AccessShareLock);
	return result;
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
	TransactionId frozenxid;
	MultiXactId minmxid;
	bool		failsafe;

	if (!kc_rel_is_columnar(rel))
		return;

	/*
	 * Deletes catalog rows and shortens the file: nothing where the server
	 * forbids an error.  VACUUM runs outside a transaction block and never at
	 * such a moment, so only a module that calls the access method itself gets
	 * here; a vacuum that is skipped loses nothing, the next one does the same
	 * work.
	 */
	if (kc_entry_refused("vacuum", RelationGetRelationName(rel), false))
		return;

	/* the catalog reads below need a snapshot */
	if (!ActiveSnapshotSet())
	{
		PushActiveSnapshot(GetTransactionSnapshot());
		pushed = true;
	}

	failsafe = kc_failsafe_active(rel);

	kc_am_failed_reset();
	(void) kc_catalog_reconcile(NULL, true);

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
	frozenxid = TransactionIdIsValid(remaining) ? remaining : oldest;
	minmxid = GetOldestMultiXactId();
	vac_update_relstats(rel, nblocks, (double) rows, 0, 0, RelationGetIndexList(rel) != NIL,
						frozenxid, minmxid, NULL, NULL, false);

	/*
	 * Give back the unreferenced tail, if it is worth it (C10-06): the rule of
	 * heap's vacuum, which takes the exclusive lock only for a tail worth
	 * removing, because that lock is WAL-logged and cancels the queries that
	 * hold the table on a hot standby; and not past the failsafe age (C11-05).
	 * It comes after the frozen id is recorded, so that a truncation that fails
	 * (a cancel while the lock is awaited) or is given up does not leave the
	 * table's age where it was: the id is what the failsafe is about.  The
	 * decision is made under the lock the vacuum already holds.
	 */
	if (!failsafe && params->truncate != VACOPTVALUE_DISABLED &&
		kc_truncate_tail(rel))
	{
		nblocks = RelationGetNumberOfBlocks(rel);
		vac_update_relstats(rel, nblocks, (double) rows, 0, 0, RelationGetIndexList(rel) != NIL,
							frozenxid, minmxid, NULL, NULL, false);
	}
	KC_REPORT_VACUUM(rel, (PgStat_Counter) rows, 0, starttime);

	/*
	 * What heap's vacuum does for autovacuum (vacuumlazy.c): one LOG line when
	 * log_autovacuum_min_duration says so, in the same words, so that a visit
	 * of autovacuum to a columnar table can be seen in the server log (C9 follow-up:
	 * the log showed nothing for a columnar table, and the absence of a line
	 * was taken for the absence of a visit).
	 */
	if (AmAutoVacuumWorkerProcess() && KC_VACUUM_LOG_MIN_DURATION(params) >= 0 &&
		TimestampDifferenceExceeds(starttime, GetCurrentTimestamp(),
								   KC_VACUUM_LOG_MIN_DURATION(params)))
	{
		ereport(LOG,
				(errmsg_internal("automatic %svacuum%s of table \"%s.%s.%s\": columnar: %llu rows, %u blocks, relfrozenxid %u, creation id %s",
								 params->is_wraparound ? "aggressive " : "",
								 params->is_wraparound ? " to prevent wraparound" : "",
								 get_database_name(MyDatabaseId),
								 get_namespace_name(RelationGetNamespace(rel)),
								 RelationGetRelationName(rel),
								 (unsigned long long) rows, (unsigned int) nblocks,
								 (unsigned int) kc_stored_relfrozenxid(rel),
								 TransactionIdIsValid(remaining) ? "kept" : "frozen")));
	}
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

	/*
	 * Compaction gives the rows it copies new numbers, and an index names rows by number:
	 * until the rebuild of the indexes is part of it (C6d) a table that has one is not compacted.
	 */
	if (RelationGetIndexList(rel) != NIL)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("columnar_compact is not supported on columnar table \"%s\", which has an index",
						RelationGetRelationName(rel)),
				 errdetail("Compaction gives rows new numbers, and the indexes of the table name rows by number; rebuilding them is not part of it yet."),
				 errhint("Use VACUUM FULL, which rewrites the table and its indexes, or drop the indexes and create them again after the compaction.")));

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
			kc_write_row_values(rel, slot->tts_values, slot->tts_isnull, cid, NULL);
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
									 scan->stripes[i].stripe_id, true) == 0)
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
