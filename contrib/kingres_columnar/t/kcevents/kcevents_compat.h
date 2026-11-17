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

#endif							/* KCEVENTS_COMPAT_H */
