/*-------------------------------------------------------------------------
 *
 * pg_compat.h
 *	  The only place of the extension that knows which PostgreSQL major
 *	  version it is built for.
 *
 * One source tree builds against PostgreSQL 18 and 19 (ADR 0023): no other
 * file tests PG_VERSION_NUM.  The table access method interface changed
 * between the two majors in these places, all absorbed here:
 *
 *	 - 19 types the "options" of the insert callbacks as uint32 (18: int);
 *	 - 19 adds "options" to tuple_delete and tuple_update and drops the
 *	   changingPart argument of tuple_delete;
 *	 - 19 adds a "flags" argument to index_fetch_begin;
 *	 - 19 adds a Snapshot argument to relation_copy_for_cluster;
 *	 - 19 drops the OldestXmin argument of scan_analyze_next_tuple;
 *	 - 19 makes the VacuumParams argument of relation_vacuum const;
 *	 - 19 gives heap_delete an "options" argument in place of changingPart.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/pg_compat.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_COLUMNAR_PG_COMPAT_H
#define KINGRES_COLUMNAR_PG_COMPAT_H

/* Module magic with a name and a version (available since PostgreSQL 18). */
#define KC_MODULE_MAGIC() \
	PG_MODULE_MAGIC_EXT(.name = "kingres_columnar", .version = "0.1")

#if PG_VERSION_NUM >= 190000

typedef uint32 KcInsertOptions;

#define KC_DELETE_PARAMS \
	Relation rel, ItemPointer tid, CommandId cid, uint32 options, \
	Snapshot snapshot, Snapshot crosscheck, bool wait, TM_FailureData *tmfd
#define KC_UPDATE_PARAMS \
	Relation rel, ItemPointer otid, TupleTableSlot *slot, CommandId cid, \
	uint32 options, Snapshot snapshot, Snapshot crosscheck, bool wait, \
	TM_FailureData *tmfd, LockTupleMode *lockmode, \
	TU_UpdateIndexes *update_indexes
#define KC_INDEX_FETCH_BEGIN_PARAMS Relation rel, uint32 flags
#define KC_COPY_FOR_CLUSTER_PARAMS \
	Relation OldTable, Relation NewTable, Relation OldIndex, bool use_sort, \
	TransactionId OldestXmin, Snapshot snapshot, TransactionId *xid_cutoff, \
	MultiXactId *multi_cutoff, double *num_tuples, double *tups_vacuumed, \
	double *tups_recently_dead
#define KC_ANALYZE_NEXT_TUPLE_PARAMS \
	TableScanDesc scan, double *liverows, double *deadrows, \
	TupleTableSlot *slot
#define KC_VACUUM_PARAMS Relation rel, const VacuumParams *params, \
	BufferAccessStrategy bstrategy
#define KC_TABLE_BEGINSCAN(rel, snap) \
	table_beginscan((rel), (snap), 0, NULL, SO_NONE)
#define KC_REPORT_VACUUM(rel, live, dead, start) \
	pgstat_report_vacuum((rel), (live), (dead), (start))
/* the threshold of autovacuum's log line (log_autovacuum_min_duration), in ms */
#define KC_VACUUM_LOG_MIN_DURATION(params) ((params)->log_vacuum_min_duration)
/* delete a row of a heap relation: wait for a deleter in progress or not */
#define KC_HEAP_DELETE(rel, tid, wait, tmfd) \
	heap_delete((rel), (tid), GetCurrentCommandId(true), 0, InvalidSnapshot, \
				(wait), (tmfd))

#else							/* PostgreSQL 18 */

typedef int KcInsertOptions;

#define KC_DELETE_PARAMS \
	Relation rel, ItemPointer tid, CommandId cid, Snapshot snapshot, \
	Snapshot crosscheck, bool wait, TM_FailureData *tmfd, bool changingPart
#define KC_UPDATE_PARAMS \
	Relation rel, ItemPointer otid, TupleTableSlot *slot, CommandId cid, \
	Snapshot snapshot, Snapshot crosscheck, bool wait, \
	TM_FailureData *tmfd, LockTupleMode *lockmode, \
	TU_UpdateIndexes *update_indexes
#define KC_INDEX_FETCH_BEGIN_PARAMS Relation rel
#define KC_COPY_FOR_CLUSTER_PARAMS \
	Relation OldTable, Relation NewTable, Relation OldIndex, bool use_sort, \
	TransactionId OldestXmin, TransactionId *xid_cutoff, \
	MultiXactId *multi_cutoff, double *num_tuples, double *tups_vacuumed, \
	double *tups_recently_dead
#define KC_ANALYZE_NEXT_TUPLE_PARAMS \
	TableScanDesc scan, TransactionId OldestXmin, double *liverows, \
	double *deadrows, TupleTableSlot *slot
#define KC_VACUUM_PARAMS Relation rel, struct VacuumParams *params, \
	BufferAccessStrategy bstrategy
#define KC_TABLE_BEGINSCAN(rel, snap) table_beginscan((rel), (snap), 0, NULL)
#define KC_REPORT_VACUUM(rel, live, dead, start) \
	pgstat_report_vacuum(RelationGetRelid(rel), (rel)->rd_rel->relisshared, \
						 (live), (dead), (start))
/* the threshold of autovacuum's log line (log_autovacuum_min_duration), in ms */
#define KC_VACUUM_LOG_MIN_DURATION(params) ((params)->log_min_duration)

/* delete a row of a heap relation: wait for a deleter in progress or not */
#define KC_HEAP_DELETE(rel, tid, wait, tmfd) \
	heap_delete((rel), (tid), GetCurrentCommandId(true), InvalidSnapshot, \
				(wait), (tmfd), false)

#endif

#endif							/* KINGRES_COLUMNAR_PG_COMPAT_H */
