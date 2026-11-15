/*-------------------------------------------------------------------------
 *
 * bucket_move.h
 *	  The C primitives of moving a bucket between shard groups.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/bucket_move.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_BUCKET_MOVE_H
#define KINGRES_SHARD_BUCKET_MOVE_H

#include "postgres.h"

/*
 * Raise an error if a bucket of the colocation group is being moved (a move
 * that has not reached "cleaning").  Called under the cluster lock by the
 * statements that must not run then: DDL on a table of the group and the
 * creation of a table in it.
 */
extern void kingres_shard_assert_no_move(int colocation_id);

#endif							/* KINGRES_SHARD_BUCKET_MOVE_H */
