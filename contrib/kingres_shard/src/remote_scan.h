/*-------------------------------------------------------------------------
 *
 * remote_scan.h
 *	  The "Kingres Remote Statement" custom scan and the plan that wraps it.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
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

#include "nodes/params.h"
#include "nodes/parsenodes.h"
#include "nodes/pg_list.h"
#include "nodes/plannodes.h"

extern void kingres_shard_remote_scan_init(void);
extern CustomScan *kingres_shard_build_scan(Query *parse, List *private,
											 bool returns_rows);
extern PlannedStmt *kingres_shard_wrap_plan(Query *parse, CustomScan *cscan,
											 List *relids, bool returns_rows);
extern const char **kingres_shard_params_to_text(ParamListInfo params,
												  Oid **types_out, int *nparams_out);
extern PlannedStmt *kingres_shard_make_remote_plan(Query *parse, char *sql,
													const char *host, int port,
													int group_id, int bucket,
													bool is_write,
													List *relids);
extern PlannedStmt *kingres_shard_make_deferred_plan(Query *parse,
													  List *relids);

#endif							/* KINGRES_SHARD_REMOTE_SCAN_H */
