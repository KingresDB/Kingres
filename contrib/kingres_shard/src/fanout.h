/*-------------------------------------------------------------------------
 *
 * fanout.h
 *	  The "Kingres Fan-out Write" custom scan: one statement executed on several
 *	  nodes in the same distributed transaction.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/fanout.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_FANOUT_H
#define KINGRES_SHARD_FANOUT_H

#include "postgres.h"

#include "nodes/parsenodes.h"
#include "nodes/pg_list.h"
#include "nodes/plannodes.h"

/* One node a statement is executed on; the local node is run through SPI. */
typedef struct KsFanTarget
{
	char	   *host;
	int			port;
	bool		is_local;
} KsFanTarget;

extern void kingres_shard_fanout_init(void);

/*
 * The plan that runs sql on every target, in the order of the list (the same
 * order on every node, so that concurrent statements lock in the same order).
 * all_rows: the result rows and counts of every node are added (a write to a
 * distributed table, whose nodes hold different rows); otherwise the copies of
 * a reference table must agree and the local result is returned.
 */
extern PlannedStmt *kingres_shard_make_fanout_plan(Query *parse, char *sql,
													List *targets, bool all_rows,
													List *relids);

/*
 * True while this backend runs the local part of a fanned-out statement: the
 * planner must then plan the statement for this node only.
 */
extern bool kingres_shard_in_fanout_local(const char *query_string);

#endif							/* KINGRES_SHARD_FANOUT_H */
