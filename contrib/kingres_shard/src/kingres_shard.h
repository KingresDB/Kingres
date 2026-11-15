/*-------------------------------------------------------------------------
 *
 * kingres_shard.h
 *	  Declarations shared by all source files of the kingres_shard
 *	  extension: constants, the error code of a stale map and the GUC
 *	  variables.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/kingres_shard.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_KINGRES_SHARD_H
#define KINGRES_SHARD_KINGRES_SHARD_H

/* The differences between the PostgreSQL majors; see the file. */
#include "pg_compat.h"

#include "fmgr.h"

/* The version of the extension this library belongs to (the SQL objects of every node must match). */
#define KINGRES_SHARD_EXT_VERSION "0.5"
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
 * The text that starts the statement of a write to a reference table that is
 * run on every node (fanout.c).  The session that runs it for another node
 * accepts a modification of a reference table only under this marker; a
 * function or trigger that a routed statement runs there may not make one.
 */
#define KS_REFWRITE_MARKER "/* kingres:reference-write */ "

/*
 * The class of the heavyweight locks that serialize the writers of a reference
 * table (fanout.c): the lock tag is an object lock of this class, no class of
 * any catalog, so that no SQL a user can run reaches it.  "KSRF".
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
extern PGDLLIMPORT int kingres_shard_move_switch_attempts;
extern PGDLLIMPORT int kingres_shard_move_cleanup_delay;
extern PGDLLIMPORT int kingres_shard_deadlock_check_interval;
extern PGDLLIMPORT int kingres_shard_deadlock_confirm_delay;

extern bool kingres_shard_check_node_conninfo(char **newval, void **extra,
											   GucSource source);

#endif							/* KINGRES_SHARD_KINGRES_SHARD_H */
