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
 * A node that cannot be reached is skipped and reported (review S7-04); it
 * cannot act on its stale catalog once it meets a node that knows the new
 * catalog term, which every promotion raises (term.c, S7-06).  The promoted node
 * itself must be reachable, and for the map group a majority of the groups.  The
 * function can be repeated after any failure and finishes what it finds left
 * (S7-05): the answer says whether it promoted, completed or had nothing to do.
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
#include "utils/jsonb.h"

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
 * What a node says about itself for a promotion (kingres.internal_promotion_state()).
 */
typedef struct PromoState
{
	bool		ok;
	int			local_node;
	int64		term;
	int64		version;
	bool		in_recovery;
	int			pending;		/* prepared catalog changes that are still unresolved */
} PromoState;

static bool
read_state(KsConn *kc, PromoState *st)
{
	PGresult   *res;

	memset(st, 0, sizeof(*st));
	res = kingres_shard_exec_params(kc,
									"SELECT local_node, term, version, in_recovery::pg_catalog.text, pending "
									"FROM " KINGRES_SHARD_SCHEMA ".internal_promotion_state()",
									0, NULL, NULL, false);
	if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK || PQntuples(res) != 1)
	{
		PQclear(res);
		return false;
	}
	st->local_node = PQgetisnull(res, 0, 0) ? 0 : atoi(PQgetvalue(res, 0, 0));
	st->term = strtoll(PQgetvalue(res, 0, 1), NULL, 10);
	st->version = strtoll(PQgetvalue(res, 0, 2), NULL, 10);
	st->in_recovery = strcmp(PQgetvalue(res, 0, 3), "true") == 0;
	st->pending = atoi(PQgetvalue(res, 0, 4));
	st->ok = true;
	PQclear(res);
	return true;
}

/*
 * Has the promotion of target (the catalog already calls it the primary of its
 * group) been carried through on every node that can be reached?  The node
 * must know itself by its id, be out of recovery, hold the catalog of this
 * node (term and version) and have no catalog change left prepared; the other
 * primaries must hold the same catalog.  Nodes that cannot be reached do not
 * count (they are reported by the caller).  A node that cannot answer the
 * question is treated as incomplete only when it is the target.
 */
static bool
promotion_complete(const KsCatalog *cat, const KsNode *tgt, bool map_case, List **unreached)
{
	ListCell   *lc;

	if (map_case)
	{
		if (cat->local_node_id != tgt->node_id || RecoveryInProgress())
			return false;
	}
	foreach(lc, cat->nodes)
	{
		const KsNode *n = (const KsNode *) lfirst(lc);
		char	   *err = NULL;
		KsConn	   *kc;
		PromoState	st;
		bool		is_self = map_case ? (n->node_id == tgt->node_id) : (n->node_id == cat->local_node_id);

		if (is_self || !n->is_active || !(n->is_primary || n->node_id == tgt->node_id))
			continue;
		kc = kingres_shard_get_conn(n->host, n->port, true, &err);
		if (kc == NULL || !read_state(kc, &st))
		{
			if (n->node_id == tgt->node_id)
				return false;
			*unreached = lappend_int(*unreached, n->node_id);
			continue;
		}
		if (st.in_recovery || st.pending > 0 || st.term != cat->term || st.version != cat->map_version)
			return false;
		if (n->node_id == tgt->node_id && st.local_node != tgt->node_id)
			return false;
	}
	return true;
}

static char *
unreached_json(List *unreached)
{
	StringInfoData buf;
	ListCell   *lc;
	bool		first = true;

	initStringInfo(&buf);
	appendStringInfoChar(&buf, '[');
	foreach(lc, unreached)
	{
		appendStringInfo(&buf, "%s%d", first ? "" : ", ", lfirst_int(lc));
		first = false;
	}
	appendStringInfoChar(&buf, ']');
	return buf.data;
}

static Datum
promote_result(const char *status, int64 term, List *unreached)
{
	char	   *list = unreached_json(unreached);
	char	   *doc = psprintf("{\"status\": \"%s\", \"term\": " INT64_FORMAT ", \"unreached\": %s}",
							   status, term, list);

	return DirectFunctionCall1(jsonb_in, CStringGetDatum(doc));
}

/*
 * kingres.promote_node(node_id int) returns jsonb
 *
 * See the file header.  Superusers only.  The answer says what was done:
 * {"status": "promoted" | "completed" | "already", "term": <catalog term>,
 * "unreached": [<ids of the nodes that were not told>]}.
 *
 *	  promoted	 the node was a standby in the catalog and is the primary now;
 *	  completed	 the catalog already called it the primary, but the promotion
 *				 was not carried through (a prepared catalog change was left,
 *				 or a node did not hold the catalog) and this call finished it;
 *	  already	 nothing was left to do.
 *
 * A node that cannot be reached is skipped and listed; it does not hold up
 * the promotion, and it cannot act on its old catalog once it meets a node
 * that knows the new term (term.c).  The ones that cannot be skipped: the
 * promoted node itself, and, when the map group is promoted, a majority of
 * the groups (the promoted node counting for its own).  A call is resumable:
 * repeat it after a failure and it finishes what the failed call left.
 */
Datum
kingres_promote_node(PG_FUNCTION_ARGS)
{
	int			target_id = PG_GETARG_INT32(0);
	const KsCatalog *cat;
	const KsNode *tgt;
	const KsNode *old;
	bool		map_case;
	bool		resume = false;
	int			old_id = 0;
	int			tgt_group;
	int			tgt_port;
	char	   *tgt_host;
	List	   *parts = NIL;
	List	   *unreached = NIL;
	ListCell   *lc;
	char	   *snapshot;
	Oid			argtypes[2];
	Datum		values[2];
	int			local_id;
	int64		new_term;
	int			resolved;
	int			reached_groups;
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
	tgt_group = tgt->group_id;
	tgt_port = tgt->port;
	tgt_host = pstrdup(tgt->host);
	map_case = (tgt_group == KINGRES_SHARD_MAP_GROUP);
	old = kingres_shard_group_primary(tgt_group);
	if (old != NULL && !tgt->is_primary)
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
		if (!tgt->is_primary && old != NULL && cat->local_node_id != old_id && cat->local_node_id != target_id)
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
	 * Nothing is open on the sessions yet.  The same pass finishes a promotion that
	 * an earlier call left prepared on the promoted node (S7-05).
	 */
	resolved = kingres_shard_recover_prepared_with(target_id, tgt_group, tgt_host, tgt_port);
	kingres_shard_fault_point("promote_after_recover");

	if (tgt->is_primary)
	{
		/* A repeated call: is there anything left to do? */
		cat = kingres_shard_catalog();
		tgt = kingres_shard_node_by_id(target_id);
		if (promotion_complete(cat, tgt, map_case, &unreached))
		{
			if (resolved == 0)
			{
				ereport(NOTICE,
						(errmsg("node %d is already the primary of group %d", target_id, tgt_group)));
				PG_RETURN_DATUM(promote_result("already", cat->term, unreached));
			}
			PG_RETURN_DATUM(promote_result("completed", cat->term, unreached));
		}
		list_free(unreached);
		unreached = NIL;
		resume = true;
		ereport(NOTICE,
				(errmsg("the promotion of node %d is not complete; finishing it", target_id)));
	}

	/*
	 * Every session is opened before the catalog changes: the tokens of the
	 * sessions are made for the ids that the nodes hold now, and the catalog
	 * that this transaction is about to write names them differently.  A node that
	 * cannot be reached is skipped (S7-04), except the promoted node.
	 */
	local_id = map_case ? target_id : cat->local_node_id;
	reached_groups = 1;
	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		Participant *p;
		char	   *err = NULL;

		if (n->node_id == local_id || !n->is_active)
			continue;
		if (!(n->is_primary || n->node_id == target_id))
			continue;
		p = palloc0(sizeof(Participant));
		p->node_id = n->node_id;
		p->host = pstrdup(n->host);
		p->port = n->port;
		p->is_target = (n->node_id == target_id);
		p->kc = kingres_shard_get_conn(p->host, p->port, !p->is_target, &err);
		if (p->kc == NULL)
		{
			if (n->node_id == old_id)
				ereport(WARNING,
						(errmsg("the former primary, node %d (%s:%d), cannot be reached and keeps its old catalog",
								p->node_id, p->host, p->port),
						 errdetail_internal("%s", err ? err : ""),
						 errhint("It must not accept writes until it is rebuilt as a standby of the new primary.")));
			else
				ereport(WARNING,
						(errmsg("node %d (%s:%d) cannot be reached and was not given the new catalog",
								p->node_id, p->host, p->port),
						 errdetail_internal("%s", err ? err : ""),
						 errhint("Run kingres.sync_catalog() on the map group primary when it is back; until then every other node refuses what it sends (catalog term).")));
			unreached = lappend_int(unreached, p->node_id);
			continue;
		}
		/*
		 * The remote transaction is opened now: a session that is idle when the
		 * catalog changes below would be closed as a session to a node that is
		 * no longer a primary.
		 */
		kingres_shard_begin_write_xact(p->kc);
		parts = lappend(parts, p);
		if (n->group_id != KINGRES_SHARD_MAP_GROUP && n->node_id != old_id && n->is_primary)
			reached_groups++;
	}

	/*
	 * The map group is promoted by the node that is promoted, which cannot know
	 * whether the old primary is alive on the other side of a partition: it
	 * proceeds only when it can reach a majority of the groups (itself for its
	 * own).  A minority changes nothing; the nodes it cannot reach are not
	 * affected.
	 */
	if (map_case && reached_groups * 2 <= cat->ngroups)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("only %d of the %d shard groups can be reached from this node; the map group cannot be promoted from a minority",
						reached_groups, cat->ngroups),
				 errhint("Promote it from a node that can reach more than half of the groups, or bring the unreachable nodes back first.")));

	kingres_shard_fault_point("promote_after_sessions");
	kingres_shard_fault_point("promote_before_change");

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	/*
	 * The new term is above every term that this node or a node it reaches
	 * holds: a standby of the map group that lagged cannot give the catalog a
	 * term that a lost promotion already used.  A resumed promotion keeps the
	 * term of the call that began it, and refuses to go on when a reachable
	 * node holds a newer one.
	 */
	new_term = cat->term;
	foreach(lc, parts)
	{
		Participant *p = (Participant *) lfirst(lc);
		PGresult   *res = kingres_shard_exec_params(p->kc,
													"SELECT term FROM " KINGRES_SHARD_SCHEMA ".map_version",
													0, NULL, NULL, true);
		int64		t = (PQntuples(res) == 1) ? strtoll(PQgetvalue(res, 0, 0), NULL, 10) : 0;

		PQclear(res);
		if (t > new_term)
			new_term = t;
	}
	if (resume)
	{
		if (new_term > cat->term)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("a node holds catalog term " INT64_FORMAT ", newer than the " INT64_FORMAT " of this node",
							new_term, cat->term),
					 errhint("This node's catalog is stale; call kingres.promote_node() on the current map group primary.")));
	}
	else
		new_term++;

	if (!resume)
	{
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
		argtypes[0] = INT8OID;
		values[0] = Int64GetDatum(new_term);
		spi_exec_ok("UPDATE " KINGRES_SHARD_SCHEMA ".map_version SET term = $1, superseded_by_term = 0",
					SPI_OK_UPDATE, 1, argtypes, values);
	}
	if (map_case)
		spi_exec_ok(psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_set_local_node(%d)", target_id),
					SPI_OK_SELECT, 0, NULL, NULL);
	kingres_shard_fault_point("promote_after_local_change");
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
	PG_RETURN_DATUM(promote_result(resume ? "completed" : "promoted", new_term, unreached));
}
