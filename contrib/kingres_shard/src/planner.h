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

#endif							/* KINGRES_SHARD_PLANNER_H */
