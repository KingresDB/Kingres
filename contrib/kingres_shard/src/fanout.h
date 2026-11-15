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
													List *relids, List *locknames);

/*
 * True while this backend runs the local part of a fanned-out statement: the
 * planner must then plan the statement for this node only.
 */
/*
 * The statement that serializes writers of one reference table on a node (see
 * fanout.c); also used by add_node().
 */
extern char *kingres_shard_refwrite_lock_sql(const char *qualified_name);

/* Lock every reference table on every node, as their writers do (add_node, S2-07). */
extern void kingres_shard_lock_all_reference_tables(void);

extern bool kingres_shard_in_fanout_local(const char *query_string);

/* A statement planned inside a reference-table write that runs as one copy of it (foreign key actions). */
extern bool kingres_shard_in_refwrite_nested(const char *query_string);

/* Take the writer lock of a reference table on this node (until the end of the transaction). */
extern void kingres_shard_lock_reference_table(const char *qualified_name);

#endif							/* KINGRES_SHARD_FANOUT_H */
