/*-------------------------------------------------------------------------
 *
 * planner.h
 *	  The routing decision made in the planner hook.
 *
 * SPDX-License-Identifier: PostgreSQL
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

extern void kingres_shard_planner_init(void);

#endif							/* KINGRES_SHARD_PLANNER_H */
