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
#define KINGRES_SHARD_EXT_VERSION "0.9"
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
 * SQLSTATE of "the catalog of the sender has been superseded": the refusing node
 * holds a newer catalog term (term.c).  The detail carries "kingres-term=<n>".
 */
#define ERRCODE_KINGRES_STALE_TERM MAKE_SQLSTATE('K','S','0','0','2')

/*
 * SQLSTATE of a switch attempt of a bucket move that gave up waiting (for the
 * lock of the bucket's tables, or for the replay of the copy): the move goes
 * back to catching up and tries again (bucket_move.c).
 */
#define ERRCODE_KINGRES_SWITCH_TIMEOUT MAKE_SQLSTATE('K','S','0','0','4')

/*
 * SQLSTATE of "a participant could not confirm that its part of a distributed
 * transaction is durable on its own synchronous standby" (node_conn.c, S8-01): the
 * commit of the transaction is refused and nothing is committed anywhere.
 */
#define ERRCODE_KINGRES_NOT_DURABLE MAKE_SQLSTATE('K','S','0','0','5')

/*
 * SQLSTATE of "this switchover cannot be proven loss-free" (ha.c, S10-01): the stopped
 * old primary was not shut down cleanly, or the position given is not a shutdown
 * checkpoint that this node has replayed as the last record of the old primary.  The
 * caller treats the promotion as a failover (README, planned switchover).
 */
#define ERRCODE_KINGRES_SWITCHOVER_UNPROVEN MAKE_SQLSTATE('K','S','0','0','6')

/*
 * SQLSTATE of "a node accepted the session and did not answer within kingres_shard.node_timeout"
 * (node_conn.c, S10-02).  The node is named in the message.  A caller must not repeat the call
 * against that node: it probes the node and fences or restarts it.
 */
#define ERRCODE_KINGRES_NODE_TIMEOUT MAKE_SQLSTATE('K','S','0','0','7')

/*
 * SQLSTATE of "another call that changes the catalog is running here and did not end within
 * node_timeout" (node_admin.c, S10-07): nothing was changed; repeat the call later.
 */
#define ERRCODE_KINGRES_BUSY MAKE_SQLSTATE('K','S','0','0','8')

/*
 * SQLSTATEs of the refusals of the high-availability calls that a control plane reacts to differently
 * (S10-08).  Their texts are unchanged.  One code per reaction, not per sentence:
 *   KS009  the node cannot commit: its synchronous standbys do not confirm; fix the list or the
 *          standby, then repeat;
 *   KS010  the call went to the wrong node (not the map group primary, superseded, another port,
 *          not a copy of the map group); send it where the message and hint say;
 *   KS011  the node is still in recovery: promote it in PostgreSQL first, then repeat;
 *   KS012  the promotion would not be safe (a newer catalog term exists, or only a minority of the
 *          groups can be reached): not a retry, a decision of the operator;
 *   KS013  distributed transactions are in doubt: run recovery, wait until check_cluster() shows
 *          none, repeat.
 * The other refusals stay 55000: they are mistakes of the person who calls (a node that is not a
 * standby, a node of another version) and no control plane reacts to them automatically.
 */
#define ERRCODE_KINGRES_CANNOT_COMMIT MAKE_SQLSTATE('K','S','0','0','9')
#define ERRCODE_KINGRES_WRONG_NODE MAKE_SQLSTATE('K','S','0','1','0')
#define ERRCODE_KINGRES_IN_RECOVERY MAKE_SQLSTATE('K','S','0','1','1')
#define ERRCODE_KINGRES_UNSAFE_PROMOTION MAKE_SQLSTATE('K','S','0','1','2')
#define ERRCODE_KINGRES_IN_DOUBT MAKE_SQLSTATE('K','S','0','1','3')
/*
 *   KS014  two nodes hold cluster secrets that do not match (S11-04, S13-04): a standby that missed a
 *          rotation, and was promoted or is asked about.  It does not say which of the two is behind:
 *          the node that refused, or the node that asked (a map standby that missed a rotation and
 *          runs promote_node itself).  Not a transient refusal: do not repeat; ask a third node
 *          (the node that every other node refuses is the stale one), and rebuild that one from a
 *          node that has the current secret.
 */
#define ERRCODE_KINGRES_SECRET_UNKNOWN MAKE_SQLSTATE('K','S','0','1','4')

/*
 * SQLSTATE of "the call took longer than kingres_shard.control_call_timeout as a whole" (node_conn.c,
 * S13-02).  It says nothing about any node: every node that was asked answered within node_timeout,
 * or the wait that was cut off was cut off by what was left of the call's time.  Not a reason to
 * fence a node.  The setting is 0 (no limit) unless the caller sets it.
 */
#define ERRCODE_KINGRES_CALL_TIMEOUT MAKE_SQLSTATE('K','S','0','1','5')

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
extern PGDLLIMPORT int kingres_shard_min_bucket_cost;
extern PGDLLIMPORT bool kingres_shard_enable_routing;
extern PGDLLIMPORT bool kingres_shard_forwarded;
extern PGDLLIMPORT int kingres_shard_node_connect_timeout;
extern PGDLLIMPORT int kingres_shard_node_timeout;
extern PGDLLIMPORT int kingres_shard_control_call_timeout;
extern PGDLLIMPORT char *kingres_shard_node_conninfo;
extern PGDLLIMPORT int kingres_shard_recovery_interval;
extern PGDLLIMPORT bool kingres_shard_enable_distributed_scan;
extern PGDLLIMPORT bool kingres_shard_enable_pushdown;
extern PGDLLIMPORT bool kingres_shard_enable_pushdown_closed;
extern PGDLLIMPORT bool kingres_shard_enable_partial_aggregation;
extern PGDLLIMPORT bool kingres_shard_enable_pushdown_sort;
extern PGDLLIMPORT bool kingres_shard_enable_pushdown_join;
extern PGDLLIMPORT bool kingres_shard_enable_bulk_routing;
extern PGDLLIMPORT bool kingres_shard_enable_colocated_insert_select;
extern PGDLLIMPORT bool kingres_shard_enable_copy_routing;
extern PGDLLIMPORT int kingres_shard_insert_batch_rows;
extern PGDLLIMPORT int kingres_shard_insert_batch_bytes;
extern PGDLLIMPORT int kingres_shard_move_catchup_threshold;
extern PGDLLIMPORT int kingres_shard_move_switch_timeout;
extern PGDLLIMPORT int kingres_shard_move_switch_attempts;
extern PGDLLIMPORT int kingres_shard_move_cleanup_delay;
extern PGDLLIMPORT int kingres_shard_rebuild_batch_rows;
extern PGDLLIMPORT int kingres_shard_max_partition_leaves;
extern PGDLLIMPORT int kingres_shard_dependent_views;
extern PGDLLIMPORT int kingres_shard_rebuild_drain_threshold;
extern PGDLLIMPORT int kingres_shard_rebuild_lock_timeout;
extern PGDLLIMPORT int kingres_shard_rebuild_switch_timeout;
extern PGDLLIMPORT int kingres_shard_rebuild_switch_max_attempts;
extern PGDLLIMPORT int kingres_shard_table_cleanup_delay;
extern PGDLLIMPORT bool kingres_shard_rebuild_verify;
extern PGDLLIMPORT int kingres_shard_deadlock_check_interval;
extern PGDLLIMPORT int kingres_shard_deadlock_confirm_delay;
extern PGDLLIMPORT int kingres_shard_decision_retention;

extern bool kingres_shard_check_node_conninfo(char **newval, void **extra,
											   GucSource source);

#endif							/* KINGRES_SHARD_KINGRES_SHARD_H */
