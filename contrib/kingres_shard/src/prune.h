/*-------------------------------------------------------------------------
 *
 * prune.h
 *	  Which shard groups a read of a distributed table has to ask: the pruning
 *	  of design/shard-phase2b.md, 4.5.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/prune.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_PRUNE_H
#define KINGRES_SHARD_PRUNE_H

#include "postgres.h"

#include "nodes/params.h"
#include "nodes/pg_list.h"

#include "catalog.h"

/*
 * The pruning terms of the restriction clauses of a base relation (the planner's
 * baserestrictinfo, whose range table indexes are right at path time).  A term
 * is one clause that says which keys the rows can have:
 *
 *	  dist_col = <constant or external parameter>
 *	  dist_col IN (<constants>), dist_col = ANY (<array constant or parameter>)
 *	  an OR of such clauses
 *
 * with the operator an equality operator of the hash operator family of the
 * column and the input collation the one the table was distributed with (and
 * deterministic).  The terms are Nodes (lists, integers, constants, parameters)
 * so that they live in a plan.  NIL means "prunes nothing".
 */
extern List *kingres_shard_prune_terms(const KsDistTable *dt, Index rti, List *restrictinfo);

/*
 * The groups a read can find rows in, from the terms and the values of the
 * parameters (params may be NULL: a parameter that has no value prunes nothing).
 * *pruned is false when no term applies (every group is possible); otherwise
 * the result is the list of group numbers, possibly empty.  The bucket of a
 * value is found by the one bucket lookup (hashing.c).
 */
extern List *kingres_shard_prune_groups(const KsDistTable *dt, List *terms,
										 ParamListInfo params, bool *pruned);

#endif							/* KINGRES_SHARD_PRUNE_H */
