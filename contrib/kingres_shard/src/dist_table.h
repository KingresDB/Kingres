/*-------------------------------------------------------------------------
 *
 * dist_table.h
 *	  Creating distributed tables and their parents on other nodes.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/dist_table.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_DIST_TABLE_H
#define KINGRES_SHARD_DIST_TABLE_H

#include "postgres.h"

extern char *kingres_shard_remote_table_script(Oid relid, const char *column);

#endif							/* KINGRES_SHARD_DIST_TABLE_H */
