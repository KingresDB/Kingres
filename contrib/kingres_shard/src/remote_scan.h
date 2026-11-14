/*-------------------------------------------------------------------------
 *
 * remote_scan.h
 *	  The "Kingres Remote Statement" custom scan and the plan that wraps it.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/remote_scan.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_REMOTE_SCAN_H
#define KINGRES_SHARD_REMOTE_SCAN_H

#include "postgres.h"

#include "nodes/parsenodes.h"
#include "nodes/pg_list.h"
#include "nodes/plannodes.h"

extern PGDLLIMPORT bool kingres_shard_in_catalog_write;

extern void kingres_shard_remote_scan_init(void);
extern PlannedStmt *kingres_shard_make_remote_plan(Query *parse, char *sql,
													const char *host, int port,
													int group_id, int bucket,
													bool is_write,
													List *relids);
extern PlannedStmt *kingres_shard_make_deferred_plan(Query *parse,
													  List *relids);

#endif							/* KINGRES_SHARD_REMOTE_SCAN_H */
