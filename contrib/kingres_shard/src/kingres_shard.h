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
#include "utils/guc.h"

#ifndef KINGRES_VERSION
#error "KINGRES_VERSION is not defined: kingres_shard must be built against KingresDB (core patch KP-0001), not against stock PostgreSQL"
#endif

/* The schema that holds the catalog; fixed by the extension control file. */
#define KINGRES_SHARD_SCHEMA "kingres"

/* The map group: the shard group that holds the authoritative catalog. */
#define KINGRES_SHARD_MAP_GROUP 0

/* SQLSTATE of "this bucket is not on this node" (stale map), class KS. */
#define ERRCODE_KINGRES_STALE_MAP MAKE_SQLSTATE('K','S','0','0','1')

/*
 * SQLSTATE of a switch attempt of a bucket move that gave up waiting (for the
 * lock of the bucket's tables, or for the replay of the copy): the move goes
 * back to catching up and tries again (bucket_move.c).
 */
#define ERRCODE_KINGRES_SWITCH_TIMEOUT MAKE_SQLSTATE('K','S','0','0','4')

/*
 * The text that starts the statement of every fragment of a distributed scan
 * (dist_scan.c).  A forwarded session runs a plain read of a distributed table
 * as the local rows of the table only when the statement carries it; without
 * it such a statement is a routed statement, and a bucket that is not here is
 * the stale-map error.
 */
#define KS_FRAGMENT_MARKER "/* kingres:scan-fragment */ "

/* The same for the statement of a write that is run on every shard group (fanout.c). */
#define KS_WRITE_MARKER "/* kingres:write-fragment */ "

/*
 * The first key of the advisory transaction locks that serialize the writers of
 * a reference table (fanout.c); the second is a hash of the qualified name.
 * "KSRF".
 */
#define KS_REFWRITE_LOCK_CLASS 0x4B535246

/* GUC variables (defined in kingres_shard.c). */
extern PGDLLIMPORT int kingres_shard_default_bucket_count;
extern PGDLLIMPORT bool kingres_shard_enable_routing;
extern PGDLLIMPORT bool kingres_shard_forwarded;
extern PGDLLIMPORT int kingres_shard_node_connect_timeout;
extern PGDLLIMPORT int kingres_shard_node_timeout;
extern PGDLLIMPORT char *kingres_shard_node_conninfo;
extern PGDLLIMPORT int kingres_shard_recovery_interval;
extern PGDLLIMPORT bool kingres_shard_enable_distributed_scan;
extern PGDLLIMPORT int kingres_shard_move_catchup_threshold;
extern PGDLLIMPORT int kingres_shard_move_switch_timeout;
extern PGDLLIMPORT int kingres_shard_move_cleanup_delay;

extern bool kingres_shard_check_node_conninfo(char **newval, void **extra,
											   GucSource source);

#endif							/* KINGRES_SHARD_KINGRES_SHARD_H */
