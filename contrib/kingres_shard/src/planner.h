/*-------------------------------------------------------------------------
 *
 * planner.h
 *	  The routing decision made in the planner hook.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/planner.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_PLANNER_H
#define KINGRES_SHARD_PLANNER_H

#include "postgres.h"

#include "nodes/parsenodes.h"

extern void kingres_shard_planner_init(void);

/* Refuse work for another node on a table read through a view of another owner (S2-15). */
extern void kingres_shard_refuse_view_owner(Query *q, RangeTblEntry *rte);

/* The search_path of the session as the owner of a schema is to run functions with it, and whether the triggers or defaults of a table run code that its statements do not show. */
extern char *kingres_shard_search_path_for_functions(void);
extern bool kingres_shard_table_runs_hidden_code(Oid relid);

#endif							/* KINGRES_SHARD_PLANNER_H */
