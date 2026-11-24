/*-------------------------------------------------------------------------
 *
 * count_distinct.h
 *	  kingres_shard.count_distinct_mode (design 9.8 of analytics-phase12-queries.md).
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/count_distinct.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_COUNT_DISTINCT_H
#define KINGRES_SHARD_COUNT_DISTINCT_H

#include "nodes/parsenodes.h"

#include "catalog.h"

#define KS_COUNT_DISTINCT_EXACT 0
#define KS_COUNT_DISTINCT_APPROXIMATE 1

extern PGDLLIMPORT int kingres_shard_count_distinct_mode;
extern PGDLLIMPORT double kingres_shard_count_distinct_error_rate;

extern void kingres_shard_count_distinct_init(void);
extern int	kingres_shard_count_distinct_precision(double rate);
extern int	kingres_shard_apply_count_distinct(Query *parse, const KsCatalog *cat);

#endif							/* KINGRES_SHARD_COUNT_DISTINCT_H */
