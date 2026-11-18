/*-------------------------------------------------------------------------
 *
 * fanout.h
 *	  The "Kingres Fan-out Write" custom scan: one statement executed on several
 *	  nodes in the same distributed transaction.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
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
 * The statement that serializes writers of one reference table on a node (see
 * fanout.c); also used by add_node().
 */
extern char *kingres_shard_refwrite_lock_sql(const char *qualified_name);

/* Lock every reference table on every node, as their writers do (add_node, S2-07). */
extern void kingres_shard_lock_all_reference_tables(void);

/*
 * True when the statement text is the local part of a fanned-out statement that
 * this backend is running through SPI (it carries the nonce of that part): the
 * planner must then plan the statement for this node only.
 */
extern bool kingres_shard_local_part_nonce_ok(const char *query_string);

/*
 * Refuse a write that other nodes take part in when the session's
 * session_replication_role is not "origin" (S6-04): the setting turns off
 * triggers, foreign key actions among them, and it is not sent to the other
 * nodes, so the origin would skip what the others do.
 */
extern void kingres_shard_refuse_replica_role(const char *what);

/* A statement that a trigger of a foreign key plans: it runs on this node's rows only (S6-01). */
extern bool kingres_shard_in_ri_action(Query *parse);

/* Take the writer lock of a reference table on this node (until the end of the transaction). */
extern void kingres_shard_lock_reference_table(const char *qualified_name);

#endif							/* KINGRES_SHARD_FANOUT_H */
