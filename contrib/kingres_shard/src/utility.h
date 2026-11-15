/*-------------------------------------------------------------------------
 *
 * utility.h
 *	  The ProcessUtility hook of the kingres_shard extension.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/utility.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_UTILITY_H
#define KINGRES_SHARD_UTILITY_H

#include "postgres.h"

extern void kingres_shard_utility_init(void);
extern bool kingres_shard_in_authorized_ddl(const char *query_string);

/*
 * The attach and detach of a bucket partition by a bucket move
 * (bucket_move.c) are refused by the hook like any other; the two functions
 * that issue them wrap exactly that statement in enter/leave (the hook leaves
 * it alone and the planner treats the foreign key validation it generates as
 * local), and nothing else.
 */
extern void kingres_shard_internal_ddl_enter(void);
extern void kingres_shard_internal_ddl_leave(void);

#endif							/* KINGRES_SHARD_UTILITY_H */
