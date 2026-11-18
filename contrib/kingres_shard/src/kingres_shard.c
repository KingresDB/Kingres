/*-------------------------------------------------------------------------
 *
 * kingres_shard.c
 *	  Module load of the kingres_shard extension: GUCs, the version
 *	  function and the installation of the hooks.
 *
 * kingres_shard must be loaded through shared_preload_libraries because it
 * installs a planner hook, an executor hook and transaction callbacks (see
 * design/shard-mvp.md, "Settings").  The rest of the extension lives in:
 *
 *	catalog.c	 per-backend cache of the catalog tables
 *	hashing.c	 the bucket function
 *	node_conn.c	 libpq connections to other nodes and remote transactions
 *	node_admin.c init_cluster, add_node, remove_node, sync_catalog
 *	dist_table.c create_distributed_table
 *	planner.c	 the routing decision in the planner hook
 *	utility.c	 the ProcessUtility hook that refuses statements that would act
 *				 on a distributed table on one node only
 *	remote_scan.c the "Kingres Remote Statement" custom scan
 *	fanout.c	 the "Kingres Fan-out Write" custom scan: reference table writes
 *				 and UPDATE/DELETE that span shard groups
 *	dist_scan.c	 the "Kingres Distributed Scan" custom scan and its path hook
 *	shippable.c	 what may be sent to another node and mean the same there
 *	recovery.c	 recover_prepared_transactions() and the background workers
 *	fault.c		 fault injection for the tests of two-phase commit
 *	bucket_move.c the primitives of moving a bucket between shard groups:
 *				 control connections, the switch, attach and detach
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/kingres_shard.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "fmgr.h"
#include "access/twophase.h"
#include "miscadmin.h"
#include "utils/builtins.h"
#include "utils/guc.h"

#include "catalog.h"
#include "dist_scan.h"
#include "fanout.h"
#include "forward_auth.h"
#include "bucket_move.h"
#include "ha.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "term.h"
#include "planner.h"
#include "recovery.h"
#include "remote_scan.h"
#include "utility.h"

PG_MODULE_MAGIC_EXT(
					.name = "kingres_shard",
					.version = KINGRES_VERSION
);

PG_FUNCTION_INFO_V1(kingres_version);

/* GUC variables */
int			kingres_shard_default_bucket_count = 128;
bool		kingres_shard_enable_routing = true;
bool		kingres_shard_forwarded = false;
int			kingres_shard_node_connect_timeout = 5;
int			kingres_shard_node_timeout = 60;
int			kingres_shard_control_call_timeout = 0;
char	   *kingres_shard_node_conninfo = NULL;
int			kingres_shard_recovery_interval = 30;
bool		kingres_shard_enable_distributed_scan = true;
int			kingres_shard_move_catchup_threshold = 1024;	/* kB */
int			kingres_shard_move_switch_timeout = 10000;	/* ms */
int			kingres_shard_move_switch_attempts = 20;
int			kingres_shard_move_cleanup_delay = 600;	/* s */
int			kingres_shard_deadlock_check_interval = 2;
int			kingres_shard_deadlock_confirm_delay = 200;	/* ms */
int			kingres_shard_decision_retention = 300;	/* s */

/*
 * _PG_init
 *
 * Define the GUCs and install the hooks.  Every hook saves the previous
 * value and the hook functions call it (coding-standards.md, 1.7).
 */
void
_PG_init(void)
{
	if (!process_shared_preload_libraries_in_progress)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("kingres_shard must be loaded via shared_preload_libraries")));

	DefineCustomIntVariable("kingres_shard.default_bucket_count",
							"Number of buckets of a new colocation group.",
							"The count is fixed per colocation group when the group is created.",
							&kingres_shard_default_bucket_count,
							128, 1, 4096,
							PGC_USERSET, 0,
							NULL, NULL, NULL);

	/*
	 * PGC_SUSET: with routing off nothing checks a statement on a distributed
	 * table any more (a SELECT reads the local partitions only, a TRUNCATE
	 * clears this node only).  That is what an administrator who repeats a
	 * statement on every node by hand wants, and not what an ordinary user
	 * can be allowed to switch on for their own session (review finding 1).
	 */
	DefineCustomBoolVariable("kingres_shard.enable_routing",
							 "Route statements on distributed tables to the owning shard group.",
							 "When off, the planner and utility hooks do nothing; superusers only.",
							 &kingres_shard_enable_routing,
							 true,
							 PGC_SUSET, 0,
							 NULL, NULL, NULL);

	/*
	 * "This session was opened by another node to run one statement for a
	 * bucket of this node": such a session never forwards a statement, and
	 * runs one locally only when every bucket it can touch is provably local
	 * (planner.c), so the flag can only make a statement fail, never make a
	 * wrong answer silent.  It is still not meant to change in the middle of a
	 * session, so it is PGC_BACKEND: it is read from the connection options
	 * that connect_to_node() sends (and from nowhere a running session can
	 * reach), and SET, set_config() and ALTER ROLE ... SET at run time cannot
	 * toggle it.  An application can still connect with it on, which only
	 * gives it a stricter session.
	 */
	DefineCustomBoolVariable("kingres_shard.forwarded",
							 "Set by the router on forwarded sessions; not for users.",
							 "Fixed at connection time.  In such a session statements are never forwarded again, and run only when every bucket they touch is local.",
							 &kingres_shard_forwarded,
							 false,
							 PGC_BACKEND, GUC_NOT_IN_SAMPLE,
							 NULL, NULL, NULL);

	kingres_shard_forward_auth_init();
	kingres_shard_term_init();

	/*
	 * The longest a wait on a node may last where nothing may be left waiting
	 * (the second phase of two-phase commit, the cleanup of an aborted
	 * transaction, the recovery worker), and the time after which the
	 * connection to a node that does not answer at all is declared dead
	 * (tcp_user_timeout, keepalives).  Review findings S2-11 and S2-12.
	 */
	DefineCustomIntVariable("kingres_shard.node_timeout",
							"Seconds after which a node that does not answer is given up on.",
							"Bounds COMMIT PREPARED, the cleanup of aborted transactions and every wait of the recovery worker; also the time a connection to an unresponsive node survives.",
							&kingres_shard_node_timeout,
							60, 1, 3600,
							PGC_USERSET, GUC_UNIT_S,
							NULL, NULL, NULL);

	/*
	 * S13-02: an opt-in bound for the whole of a control-plane call.  0 (the default) means none: a call
	 * that is working is bounded per statement by node_timeout and nothing else.
	 */
	DefineCustomIntVariable("kingres_shard.control_call_timeout",
							"Seconds after which a control-plane call that is still working is ended as a whole (0: no limit).",
							"Counted from the start of the call; checked between the statements that the call sends to nodes, and every wait is cut to what is left.  The error (KS015) does not say that a node is dead.",
							&kingres_shard_control_call_timeout,
							0, 0, 86400,
							PGC_USERSET, GUC_UNIT_S,
							NULL, NULL, NULL);

	DefineCustomIntVariable("kingres_shard.node_connect_timeout",
							"Connection timeout between nodes.",
							NULL,
							&kingres_shard_node_connect_timeout,
							5, 1, 3600,
							PGC_USERSET, GUC_UNIT_S,
							NULL, NULL, NULL);

	/*
	 * Appended to every node-to-node connection string (sslmode, sslcert,
	 * sslkey, sslrootcert, ...).  Server configuration only (postgresql.conf, ALTER SYSTEM; PGC_SIGHUP): a value set per role or per database would be stored in pg_db_role_setting, which every role can read (review finding S2-16).  It can name key files.
	 * The keywords the extension controls are rejected when it is set, and
	 * the value never appears in the messages of the extension.
	 */
	DefineCustomStringVariable("kingres_shard.node_conninfo",
							   "Connection options appended to every node-to-node connection.",
							   "A libpq connection string such as \"sslmode=verify-full sslrootcert=...\"; host, port, dbname, user, password and options are not allowed.",
							   &kingres_shard_node_conninfo,
							   "",
							   PGC_SIGHUP, GUC_SUPERUSER_ONLY,
							   kingres_shard_check_node_conninfo, NULL, NULL);

	DefineCustomIntVariable("kingres_shard.recovery_interval",
							"Seconds between runs of the recovery of prepared transactions; 0 disables.",
							NULL,
							&kingres_shard_recovery_interval,
							30, 0, 86400,
							PGC_SIGHUP, GUC_UNIT_S,
							NULL, NULL, NULL);

	DefineCustomBoolVariable("kingres_shard.enable_distributed_scan",
							 "Let SELECT statements that span shard groups run through the distributed scan.",
							 "When off, such statements fail with \"statements that span shard groups are not supported yet\".",
							 &kingres_shard_enable_distributed_scan,
							 true,
							 PGC_USERSET, 0,
							 NULL, NULL, NULL);

	/* Moving a bucket (design/shard-phase4.md, "Settings added"). */
	DefineCustomIntVariable("kingres_shard.move_catchup_threshold",
							"How close the copy of a bucket move must be to the source before the switch is attempted.",
							"Lag of the replication slot behind the current WAL position of the source; a replay proven by a marker row counts as caught up.",
							&kingres_shard_move_catchup_threshold,
							1024, 0, MAX_KILOBYTES,
							PGC_USERSET, GUC_UNIT_KB,
							NULL, NULL, NULL);

	DefineCustomIntVariable("kingres_shard.move_switch_timeout",
							"Longest time the tables of a moving bucket are locked by one switch attempt.",
							"After it the switch is abandoned, the locks are released and the move returns to catching up.",
							&kingres_shard_move_switch_timeout,
							10000, 100, 3600000,
							PGC_USERSET, GUC_UNIT_MS,
							NULL, NULL, NULL);

	DefineCustomIntVariable("kingres_shard.move_switch_attempts",
							"How many times a move tries to switch before it gives up.",
							"A try that cannot get the locks of the bucket's tables (a long transaction holds one) is abandoned and repeated after a pause that doubles up to 10 seconds; when the tries are used up the move fails and names the sessions that were in the way.",
							&kingres_shard_move_switch_attempts,
							20, 1, 1000,
							PGC_USERSET, 0,
							NULL, NULL, NULL);

	DefineCustomIntVariable("kingres_shard.move_cleanup_delay",
							"How long the orphan tables of a moved bucket are kept on the old group.",
							"Statements that were reading the old partitions have finished by then.",
							&kingres_shard_move_cleanup_delay,
							600, 0, 31536000,
							PGC_USERSET, GUC_UNIT_S,
							NULL, NULL, NULL);

	/* Distributed deadlock detection (ha-phase5.md, part C). */
	DefineCustomIntVariable("kingres_shard.deadlock_check_interval",
							"Seconds between passes of the distributed deadlock detector; 0 disables.",
							"The recovery worker of the map group primary collects the wait-for edges of every primary this often and cancels the youngest transaction of a cycle that spans nodes.",
							&kingres_shard_deadlock_check_interval,
							2, 0, 3600,
							PGC_SIGHUP, GUC_UNIT_S,
							NULL, NULL, NULL);

	DefineCustomIntVariable("kingres_shard.deadlock_confirm_delay",
							"Time between the two readings of the wait-for graph that confirm a distributed deadlock.",
							"A cycle is cancelled only if the same waits are found again this long after they were first seen.",
							&kingres_shard_deadlock_confirm_delay,
							200, 10, 60000,
							PGC_SIGHUP, GUC_UNIT_MS,
							NULL, NULL, NULL);

	/* Failover of a participant (review S7-07). */
	DefineCustomIntVariable("kingres_shard.decision_retention",
							"Seconds that the commit decisions of a transaction that is over are kept when the participant's group has a standby.",
							"If the participant's group fails over in that time, the origin compares its decisions with the participant's own record and reports a committed transaction that the new primary has no trace of; 0 keeps them no longer than recovery needs them.",
							&kingres_shard_decision_retention,
							300, 0, 86400,
							PGC_SIGHUP, GUC_UNIT_S,
							NULL, NULL, NULL);

	MarkGUCPrefixReserved("kingres_shard");

	/* Two-phase commit needs prepared transactions on every node. */
	if (max_prepared_xacts == 0)
		ereport(WARNING,
				(errmsg("kingres_shard: max_prepared_transactions is 0"),
				 errhint("Transactions that write on several nodes need max_prepared_transactions greater than zero on every node.")));

	kingres_shard_fault_init();
	kingres_shard_ha_init();
	kingres_shard_catalog_init();
	kingres_shard_conn_init();
	kingres_shard_ctl_init();
	kingres_shard_recovery_init();
	kingres_shard_fanout_init();
	kingres_shard_dist_scan_init();
	kingres_shard_planner_init();
	kingres_shard_utility_init();
	kingres_shard_remote_scan_init();
}

/*
 * kingres.version() returns text
 *
 * The product version, compiled in from KINGRES_VERSION (core patch
 * KP-0001).  Building against stock PostgreSQL fails in kingres_shard.h.
 */
Datum
kingres_version(PG_FUNCTION_ARGS)
{
	PG_RETURN_TEXT_P(cstring_to_text(KINGRES_VERSION));
}
