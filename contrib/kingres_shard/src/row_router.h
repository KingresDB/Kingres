/*-------------------------------------------------------------------------
 *
 * row_router.h
 *	  The "Kingres Row Router" custom scan: INSERT and COPY FROM into a distributed
 *	  table of several shard groups (design/shard-phase2b.md, section 11).
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/row_router.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_ROW_ROUTER_H
#define KINGRES_SHARD_ROW_ROUTER_H

#include "postgres.h"

#include "nodes/parsenodes.h"
#include "nodes/pg_list.h"
#include "nodes/plannodes.h"

#include "catalog.h"

extern void kingres_shard_row_router_init(void);

/*
 * The plan of an INSERT into the distributed table dt that is not restricted to one
 * bucket: one custom scan whose source rows are a SELECT run through SPI and whose
 * destination is the batch sender.  relids are the distributed tables of the
 * statement (a cached plan is invalidated when the bucket map changes).  A statement the
 * router does not take raises the error that says why.
 */
extern PlannedStmt *kingres_shard_plan_row_router(Query *parse, const KsDistTable *dt,
												   List *relids);

/*
 * COPY relation FROM ... through the router (11.6).  Does what DoCopy does for COPY FROM
 * (privileges, the read-only rule, the options that are supported) and returns the number
 * of rows.  The caller has decided that the table is distributed over several groups.
 */
extern uint64 kingres_shard_copy_from_routed(CopyStmt *stmt, const char *query_string);

#endif							/* KINGRES_SHARD_ROW_ROUTER_H */
