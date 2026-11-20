/*-------------------------------------------------------------------------
 *
 * placement_check.h
 *	  The placement check that every statement which reads or writes the groups
 *	  of a colocation group makes (design/shard-phase2b.md, 4.3, 4.4 and 4.6):
 *	  the plan number, the probes with the identity of the partitions and the
 *	  extension version, the rule "one node per group in a transaction", and the
 *	  error that a failed group becomes.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/placement_check.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_PLACEMENT_CHECK_H
#define KINGRES_SHARD_PLACEMENT_CHECK_H

#include "postgres.h"

#include "node_conn.h"

/*
 * Whether the probes carry the identity of the partitions.  Phase 10 turns it
 * off when the placement epoch makes it redundant (design 4.3, "What 2b needs
 * from phase 10"); the probe functions below are the only places that read it.
 */
#define KS_PROBE_IDENTITY 1

/*
 * Step 1 of 4.3.  The placement number of the colocation group in this
 * backend's cache must be the one of the plan (a cached or generic plan, a plan
 * made before a move or a failover).  KS001 otherwise; nothing has been sent.
 */
extern void kingres_shard_placement_check_plan(int colocation_id, int64 plan_placement);

/*
 * The probe statements of steps 2 and 4, for the tables (distributed tables of
 * the colocation group, in any order: the statement lists them in OID order).
 * Before: placement and identity of every table, and the extension version of
 * the node.  After: the placement and the identity of every table.
 */
extern char *kingres_shard_probe_before_sql(int colocation_id, int ntables, const Oid *relids, const char *ext);
extern char *kingres_shard_probe_after_sql(int colocation_id, int ntables, const Oid *relids);

/*
 * Read the answer of a probe (res is cleared).  The order of identities is the
 * order of the sorted tables, which is what kingres_shard_probe_tables() gives.
 * Before: the placement must be the plan's, the node must run this version of
 * the extension, and the identities are stored.  After: the placement must be
 * the plan's and every identity must be the stored one.  KS001 for a difference
 * of placement or identity, an ordinary error that is not retryable for another
 * version of the extension.
 */
extern void kingres_shard_probe_read_before(KsConn *kc, PGresult *res, int ntables,
											 int64 plan_placement, int64 *identities, const char *ext);
extern void kingres_shard_probe_read_after(KsConn *kc, PGresult *res, int ntables,
											int64 plan_placement, const int64 *identities);

/* The tables of a probe in the order its answer has them: a sorted copy of relids. */
extern Oid *kingres_shard_probe_tables(int ntables, const Oid *relids);

/*
 * The origin's own part: the placement is recomputed from the catalog tables, and
 * the identity of the table's own partitions is compared with the one the plan
 * was made with.  KS001 on a difference.
 */
extern void kingres_shard_check_origin_placement(int colocation_id, int64 plan_placement,
												  const char *when);
extern void kingres_shard_check_origin_partitions(Oid relid, bool plan_has_parts,
												   int64 plan_partitions);

/*
 * The placement of the colocation group on a node, asked alone (a fan-out write
 * checks it after the statement ran); a failed node is reported by
 * kingres_shard_fragment_error().
 */
extern int64 kingres_shard_probe_placement(KsConn *kc, int colocation_id);

/*
 * 4.4: a statement reads and writes each shard group through one node.  KS001
 * if this transaction has an open remote transaction on another node of the
 * group of host:port (a node of the same group that the catalog knows: the old
 * primary after promote_node, a standby).  Not applied inside a control call,
 * which talks to the nodes of a group on purpose.
 */
extern void kingres_shard_group_rule(const char *host, int port);

/*
 * 4.6: raise the error for a failed group.  res is what the node answered, or
 * NULL when the connection failed; it is cleared.  A failure that a change of
 * primary causes, on a group that has a registered standby (or whose node is no
 * longer its primary in the cache), is KS001 with a detail that names the node;
 * every other failure is the plain error (the remote one, or a lost
 * connection).  Never returns.
 */
extern pg_noreturn void kingres_shard_fragment_error(KsConn *kc, PGresult *res,
													  const char *what);

#endif							/* KINGRES_SHARD_PLACEMENT_CHECK_H */
