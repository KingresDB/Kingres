/*-------------------------------------------------------------------------
 *
 * kcevents_compat.h
 *	  The differences between PostgreSQL majors that the test module
 *	  kcevents needs.  The module is built by the tests from its own source
 *	  and does not see the headers of kingres_columnar, so it has a header of
 *	  its own; as in the extension, a test of the server version appears in
 *	  this file and nowhere else in the module.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 *-------------------------------------------------------------------------
 */
#ifndef KCEVENTS_COMPAT_H
#define KCEVENTS_COMPAT_H

#include "access/tableam.h"

/*
 * table_tuple_delete and table_tuple_update take an options argument after
 * the command id from PostgreSQL 19 on; table_tuple_delete lost its last
 * argument (changingPart) at the same time.
 */
static inline TM_Result
kce_table_tuple_delete(Relation rel, ItemPointer tid, CommandId cid,
					   bool wait, TM_FailureData *tmfd)
{
#if PG_VERSION_NUM >= 190000
	return table_tuple_delete(rel, tid, cid, 0,
							  InvalidSnapshot, InvalidSnapshot, wait, tmfd);
#else
	return table_tuple_delete(rel, tid, cid,
							  InvalidSnapshot, InvalidSnapshot, wait, tmfd, false);
#endif
}

static inline TM_Result
kce_table_tuple_update(Relation rel, ItemPointer tid, TupleTableSlot *slot,
					   CommandId cid, bool wait, TM_FailureData *tmfd,
					   LockTupleMode *lockmode, TU_UpdateIndexes *idx)
{
#if PG_VERSION_NUM >= 190000
	return table_tuple_update(rel, tid, slot, cid, 0,
							  InvalidSnapshot, InvalidSnapshot, wait, tmfd,
							  lockmode, idx);
#else
	return table_tuple_update(rel, tid, slot, cid,
							  InvalidSnapshot, InvalidSnapshot, wait, tmfd,
							  lockmode, idx);
#endif
}

/* a sequential scan with the default flags: 19 takes the flags as an argument */
static inline TableScanDesc
kce_beginscan(Relation rel, Snapshot snap)
{
#if PG_VERSION_NUM >= 190000
	return table_beginscan(rel, snap, 0, NULL, SO_NONE);
#else
	return table_beginscan(rel, snap, 0, NULL);
#endif
}

/* 19 gives table_index_fetch_begin a flags argument */
static inline IndexFetchTableData *
kce_index_fetch_begin(Relation rel)
{
#if PG_VERSION_NUM >= 190000
	return table_index_fetch_begin(rel, 0);
#else
	return table_index_fetch_begin(rel);
#endif
}

/* 19 gives table_relation_copy_for_cluster a snapshot argument */
static inline void
kce_copy_for_cluster(Relation old_rel, Relation new_rel, TransactionId oldest_xmin,
					 TransactionId *xid_cutoff, MultiXactId *multi_cutoff,
					 double *num_tuples, double *tups_vacuumed,
					 double *tups_recently_dead)
{
#if PG_VERSION_NUM >= 190000
	table_relation_copy_for_cluster(old_rel, new_rel, NULL, false, oldest_xmin,
									InvalidSnapshot, xid_cutoff, multi_cutoff,
									num_tuples, tups_vacuumed, tups_recently_dead);
#else
	table_relation_copy_for_cluster(old_rel, new_rel, NULL, false, oldest_xmin,
									xid_cutoff, multi_cutoff,
									num_tuples, tups_vacuumed, tups_recently_dead);
#endif
}

#endif							/* KCEVENTS_COMPAT_H */
