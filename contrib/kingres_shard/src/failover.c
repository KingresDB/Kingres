/*-------------------------------------------------------------------------
 *
 * failover.c
 *	  kingres.promote_node(): the change of the sharding catalog that makes a
 *	  promoted standby the primary of its shard group.
 *
 * The control plane promotes the standby in PostgreSQL itself (pg_promote)
 * after the lease of the old primary has run out, then calls this function.  It
 * runs in one two-phase transaction on every node that can still be reached:
 *
 *	  - the standby's node row becomes "primary" and the old primary's "standby",
 *		which raises the map version; every node that applies the new catalog
 *		drops its cached sessions to the old primary (node_conn.c) and sees a
 *		different placement (catalog.c: the placement of a bucket includes the
 *		node that is the primary of its group), so a scan or a write that was
 *		planned against the old primary fails with the retryable KS001;
 *	  - on the promoted node the local_node row (which node this instance is)
 *		is set to its own id: a standby is a physical copy of its primary and
 *		holds the primary's id until now;
 *	  - the old primary, when it can be reached, gets the new catalog too, and
 *		from then on it routes every statement to the new primary and runs
 *		nothing as local (a node whose catalog row is "standby" is in no shard
 *		group, catalog.c).
 *
 * The function runs on the map group primary.  For a group that is not the map
 * group that is the node the catalog is written on.  For the map group it runs
 * on the node that was promoted: it is the only node that can write the catalog
 * then, and the catalogs of the other nodes still name the old map primary.  It
 * proves that it runs on the promoted node (not in recovery, the port of the
 * node row), pushes its catalog to the others with the cluster secret that it
 * holds from replication, and the other nodes find the new map group primary
 * in the catalog it brings.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/failover.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/xact.h"
#include "access/xlog.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "postmaster/postmaster.h"
#include "utils/acl.h"
#include "utils/builtins.h"
#include "utils/guc.h"

#include "catalog.h"
#include "kingres_shard.h"
#include "node_admin.h"
#include "node_conn.h"
#include "recovery.h"

PG_FUNCTION_INFO_V1(kingres_promote_node);

typedef struct Participant
{
	int			node_id;
	char	   *host;
	int			port;
	KsConn	   *kc;
	bool		is_target;
} Participant;

static void
spi_exec_ok(const char *sql, int expected, int nargs, Oid *argtypes, Datum *values)
{
	int			ret;

	if (nargs == 0)
		ret = SPI_execute(sql, false, 0);
	else
		ret = SPI_execute_with_args(sql, nargs, argtypes, values, NULL, false, 0);
	if (ret != expected)
		elog(ERROR, "SPI command failed (%d): %s", ret, sql);
}

/*
 * Ask the node that is to be promoted what it is: out of recovery, the same
 * version of the extension and of the library as this node.  A session of its
 * own (not part of the transaction), so that a refusal leaves nothing behind.
 */
static void
check_target_node(const KsNode *tgt)
{
	char	   *err = NULL;
	PGconn	   *conn = kingres_shard_connect_control(tgt->host, tgt->port, &err);
	PGresult   *res;
	char	   *extver;
	char	   *libver;

	if (conn == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_SQLCLIENT_UNABLE_TO_ESTABLISH_SQLCONNECTION),
				 errmsg("could not connect to node %d (%s:%d), which is to become the primary of group %d",
						tgt->node_id, tgt->host, tgt->port, tgt->group_id),
				 errdetail_internal("%s", err),
				 errhint("Promote the standby with pg_promote() and wait until it accepts connections, then call kingres.promote_node().")));

	res = kingres_shard_bounded_exec(conn,
									 "SELECT pg_catalog.pg_is_in_recovery()::pg_catalog.text, "
									 "COALESCE((SELECT e.extversion FROM pg_catalog.pg_extension e "
									 "WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard'), '')",
									 true);
	if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK || PQntuples(res) != 1)
	{
		PQclear(res);
		PQfinish(conn);
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not ask node %d (%s:%d) whether it is in recovery",
						tgt->node_id, tgt->host, tgt->port)));
	}
	if (strcmp(PQgetvalue(res, 0, 0), "false") != 0)
	{
		PQclear(res);
		PQfinish(conn);
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("node %d (%s:%d) is still in recovery",
						tgt->node_id, tgt->host, tgt->port),
				 errhint("Promote the standby with pg_promote() first; kingres.promote_node() only changes the catalog of the cluster.")));
	}
	extver = pstrdup(PQgetvalue(res, 0, 1));
	PQclear(res);

	res = kingres_shard_bounded_exec(conn, "SELECT " KINGRES_SHARD_SCHEMA ".version()", true);
	if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK || PQntuples(res) != 1)
		libver = pstrdup("(unknown)");
	else
		libver = pstrdup(PQgetvalue(res, 0, 0));
	PQclear(res);
	PQfinish(conn);

	if (strcmp(extver, KINGRES_SHARD_EXT_VERSION) != 0 || strcmp(libver, KINGRES_VERSION) != 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("node %d (%s:%d) runs kingres_shard %s with library %s, this node runs %s with library %s",
						tgt->node_id, tgt->host, tgt->port, extver, libver,
						KINGRES_SHARD_EXT_VERSION, KINGRES_VERSION),
				 errhint("A promoted standby must run the same library as the rest of the cluster; install it on the standby before promoting it.")));
}

/*
 * kingres.promote_node(node_id int) returns void
 *
 * See the file header.  Superusers only.  A repeated call for a node that is
 * already the primary of its group does nothing.
 */
Datum
kingres_promote_node(PG_FUNCTION_ARGS)
{
	int			target_id = PG_GETARG_INT32(0);
	const KsCatalog *cat;
	const KsNode *tgt;
	const KsNode *old;
	bool		map_case;
	int			old_id = 0;
	int			tgt_group;
	int			tgt_port;
	char	   *tgt_host;
	List	   *parts = NIL;
	ListCell   *lc;
	char	   *snapshot;
	Oid			argtypes[1];
	Datum		values[1];
	int			local_id;
	MemoryContext callercxt = CurrentMemoryContext;

	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for function promote_node"),
				 errdetail("Only superusers can promote a node.")));

	kingres_shard_lock_catalog();
	cat = kingres_shard_catalog();
	if (!cat->present || cat->local_node_id == 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("the cluster is not initialized on this node")));
	tgt = kingres_shard_node_by_id(target_id);
	if (tgt == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_OBJECT),
				 errmsg("node %d does not exist", target_id)));
	if (tgt->is_primary)
	{
		ereport(NOTICE,
				(errmsg("node %d is already the primary of group %d", target_id, tgt->group_id)));
		PG_RETURN_VOID();
	}
	tgt_group = tgt->group_id;
	tgt_port = tgt->port;
	tgt_host = pstrdup(tgt->host);
	map_case = (tgt_group == KINGRES_SHARD_MAP_GROUP);
	old = kingres_shard_group_primary(tgt_group);
	if (old != NULL)
		old_id = old->node_id;

	if (!map_case)
	{
		kingres_shard_require_map_group();
		check_target_node(tgt);
	}
	else
	{
		/*
		 * The map group: this node is the promoted standby itself.  What can be
		 * proved here: it is out of recovery, it is a copy of the old map
		 * primary (its local_node row is that node's), and it listens on the port
		 * of the row it is about to become.
		 */
		if (RecoveryInProgress())
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("this node is still in recovery"),
					 errhint("Promote the standby with pg_promote() first, then run kingres.promote_node() on it.")));
		if (old != NULL && cat->local_node_id != old_id && cat->local_node_id != target_id)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("this node is not a copy of the primary of group 0 and cannot become it"),
					 errdetail("Its local node id is %d, the primary of group 0 is node %d.",
							   cat->local_node_id, old_id)));
		if (PostPortNumber != tgt_port)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("node %d is registered on port %d, this server listens on port %d",
							target_id, tgt_port, PostPortNumber),
					 errhint("Run kingres.promote_node() on the standby that was promoted.")));
	}

	/*
	 * What the old primary of the group left prepared (a switch of a bucket move that
	 * was decided, a catalog change of the map group) holds row locks on the catalog
	 * tables that are written below, on the promoted node and, when the group is the
	 * map group, on the others: resolve it first, by the decision log this node holds.
	 * Nothing is open on the sessions yet.
	 */
	(void) kingres_shard_recover_prepared_with(target_id, tgt_group, tgt_host, tgt_port);

	/*
	 * Every session is opened before the catalog changes: the tokens of the
	 * sessions are made for the ids that the nodes hold now, and the catalog
	 * that this transaction is about to write names them differently.
	 */
	local_id = map_case ? target_id : cat->local_node_id;
	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		Participant *p;

		if (n->node_id == local_id || !n->is_active)
			continue;
		if (!(n->is_primary || n->node_id == target_id))
			continue;
		p = palloc0(sizeof(Participant));
		p->node_id = n->node_id;
		p->host = pstrdup(n->host);
		p->port = n->port;
		p->is_target = (n->node_id == target_id);
		if (n->node_id == old_id)
		{
			char	   *err = NULL;

			/* The old primary is told when it can be reached; it is usually down. */
			p->kc = kingres_shard_get_conn(p->host, p->port, true, &err);
			if (p->kc == NULL)
			{
				ereport(WARNING,
						(errmsg("the former primary, node %d (%s:%d), cannot be reached and keeps its old catalog",
								p->node_id, p->host, p->port),
						 errdetail_internal("%s", err ? err : ""),
						 errhint("It must not accept writes until it is rebuilt as a standby of the new primary.")));
				continue;
			}
		}
		else
			p->kc = kingres_shard_get_conn(p->host, p->port, false, NULL);
		/*
		 * The remote transaction is opened now: a session that is idle when the
		 * catalog changes below would be closed as a session to a node that is
		 * no longer a primary.
		 */
		kingres_shard_begin_write_xact(p->kc);
		parts = lappend(parts, p);
	}

	kingres_shard_fault_point("promote_before_change");

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	if (old_id != 0)
	{
		argtypes[0] = INT4OID;
		values[0] = Int32GetDatum(old_id);
		spi_exec_ok("UPDATE " KINGRES_SHARD_SCHEMA ".node SET role = 'standby' WHERE node_id = $1",
					SPI_OK_UPDATE, 1, argtypes, values);
	}
	argtypes[0] = INT4OID;
	values[0] = Int32GetDatum(target_id);
	spi_exec_ok("UPDATE " KINGRES_SHARD_SCHEMA ".node SET role = 'primary', is_active = true WHERE node_id = $1",
				SPI_OK_UPDATE, 1, argtypes, values);
	if (map_case)
		spi_exec_ok(psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_set_local_node(%d)", target_id),
					SPI_OK_SELECT, 0, NULL, NULL);
	spi_exec_ok("SELECT " KINGRES_SHARD_SCHEMA ".internal_catalog_snapshot()::pg_catalog.text",
				SPI_OK_SELECT, 0, NULL, NULL);
	snapshot = MemoryContextStrdup(callercxt,
								   SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1));
	SPI_finish();
	/* The cache of this backend was read between the updates and the local node row. */
	kingres_shard_catalog_invalidate();

	foreach(lc, parts)
	{
		Participant *p = (Participant *) lfirst(lc);
		const char *params[1];
		PGresult   *res;
		char		idbuf[16];

		kingres_shard_begin_write_xact(p->kc);
		params[0] = snapshot;
		res = kingres_shard_exec_params(p->kc,
										"SELECT " KINGRES_SHARD_SCHEMA ".internal_apply_catalog($1::pg_catalog.jsonb)",
										1, NULL, params, true);
		PQclear(res);
		if (p->is_target)
		{
			snprintf(idbuf, sizeof(idbuf), "%d", target_id);
			params[0] = idbuf;
			res = kingres_shard_exec_params(p->kc,
											"SELECT " KINGRES_SHARD_SCHEMA ".internal_set_local_node($1::pg_catalog.int4)",
											1, NULL, params, true);
			PQclear(res);
		}
		kingres_shard_fault_point("promote_after_first_push");
	}
	kingres_shard_fault_point("promote_after_push");
	PG_RETURN_VOID();
}
