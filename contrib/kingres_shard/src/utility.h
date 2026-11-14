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
extern bool kingres_shard_in_authorized_ddl(void);

#endif							/* KINGRES_SHARD_UTILITY_H */
