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
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/kingres_shard.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "fmgr.h"
#include "miscadmin.h"
#include "utils/builtins.h"
#include "utils/guc.h"

#include "catalog.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "planner.h"
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

	DefineCustomIntVariable("kingres_shard.node_connect_timeout",
							"Connection timeout between nodes.",
							NULL,
							&kingres_shard_node_connect_timeout,
							5, 1, 3600,
							PGC_USERSET, GUC_UNIT_S,
							NULL, NULL, NULL);

	MarkGUCPrefixReserved("kingres_shard");

	kingres_shard_catalog_init();
	kingres_shard_conn_init();
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
