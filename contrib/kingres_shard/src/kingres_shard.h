/*-------------------------------------------------------------------------
 *
 * kingres_shard.h
 *	  Declarations shared by all source files of the kingres_shard
 *	  extension: constants, the error code of a stale map and the GUC
 *	  variables.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/kingres_shard.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_KINGRES_SHARD_H
#define KINGRES_SHARD_KINGRES_SHARD_H

#include "fmgr.h"
#include "nodes/params.h"
#include "nodes/parsenodes.h"
#include "nodes/plannodes.h"

#ifndef KINGRES_VERSION
#error "KINGRES_VERSION is not defined: kingres_shard must be built against KingresDB (core patch KP-0001), not against stock PostgreSQL"
#endif

/* The schema that holds the catalog; fixed by the extension control file. */
#define KINGRES_SHARD_SCHEMA "kingres"

/* The map group: the shard group that holds the authoritative catalog. */
#define KINGRES_SHARD_MAP_GROUP 0

/* SQLSTATE of "this bucket is not on this node" (stale map), class KS. */
#define ERRCODE_KINGRES_STALE_MAP MAKE_SQLSTATE('K','S','0','0','1')

/* GUC variables (defined in kingres_shard.c). */
extern PGDLLIMPORT int kingres_shard_default_bucket_count;
extern PGDLLIMPORT bool kingres_shard_enable_routing;
extern PGDLLIMPORT bool kingres_shard_forwarded;
extern PGDLLIMPORT int kingres_shard_node_connect_timeout;

#endif							/* KINGRES_SHARD_KINGRES_SHARD_H */
