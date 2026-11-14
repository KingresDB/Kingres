/*-------------------------------------------------------------------------
 *
 * node_admin.c
 *	  The node registry: kingres.add_node(), kingres.remove_node() and
 *	  kingres.sync_catalog(), plus the reload of a stale catalog from the
 *	  map group.  (init_cluster() is a plain SQL function.)
 *
 * The catalog is authoritative on the map group primary.  Synchronization
 * (design/shard-mvp.md, "Node registry and catalog synchronization") sends
 * the whole catalog as one jsonb document to every active primary node, and
 * each node replaces its copy in a remote transaction that is committed in
 * the pre-commit callback of the local transaction (node_conn.c).  This is
 * deliberately simple and NOT atomic across nodes: a crash between two remote
 * commits can leave a node with a stale catalog, which a later
 * sync_catalog() repairs.  Phase 2 replaces it with two-phase commit.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/node_admin.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/xact.h"
#include "access/xlog.h"
#include "catalog/pg_authid.h"
#include "catalog/pg_type_d.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "nodes/value.h"
#include "parser/parse_func.h"
#include "storage/lmgr.h"
#include "utils/builtins.h"
#include "utils/memutils.h"

#include "catalog.h"
#include "kingres_shard.h"
#include "dist_table.h"
#include "node_admin.h"
#include "node_conn.h"
#include "remote_scan.h"

PG_FUNCTION_INFO_V1(kingres_add_node);
PG_FUNCTION_INFO_V1(kingres_remove_node);
PG_FUNCTION_INFO_V1(kingres_sync_catalog);

/*
 * kingres_shard_require_map_group
 *
 * Raise an error unless this node is initialized and is the primary of the
 * map group.  Catalog changes are made only there, so that the catalog has a
 * single writer.
 */
void
kingres_shard_require_map_group(void)
{
	const KsCatalog *cat = kingres_shard_catalog();
	const KsNode *self;

	if (!cat->present || cat->local_node_id == 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("the cluster is not initialized on this node"),
				 errhint("Run kingres.init_cluster() on the first node.")));

	self = kingres_shard_node_by_id(cat->local_node_id);
	if (self == NULL || self->group_id != KINGRES_SHARD_MAP_GROUP ||
		!self->is_primary)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("this function can only run on the map group primary"),
				 errdetail("This node is node %d.", cat->local_node_id)));
}

/*
 * kingres_shard_lock_catalog
 *
 * The one cluster-wide lock of every function that changes the catalog
 * (init_cluster, add_node, remove_node, sync_catalog, create_distributed_table;
 * review finding 4): an exclusive lock on kingres.map_version, held to the
 * end of the transaction.  The catalog changes only on the map group primary,
 * so a lock in that database is a lock on the cluster.  The relation is the
 * right thing to lock because the statement trigger of every catalog table
 * updates it: a session that changes a catalog table by hand waits for the
 * lock too, and there are no keys to collide with the advisory locks of an
 * application.  Callers take it before they read anything they are going to
 * base a decision on (the next node id, the primary of a group, the next
 * colocation id), so that the statements after it see what the previous holder
 * committed.
 */
void
kingres_shard_lock_catalog(void)
{
	Oid			relid = kingres_shard_catalog_relid("map_version");

	if (!OidIsValid(relid))
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("the catalog table kingres.map_version does not exist")));
	LockRelationOid(relid, ExclusiveLock);
}

/*
 * Run a statement through SPI, expecting the given result code.
 */
static void
spi_run(const char *sql, int expected, int nargs, Oid *argtypes, Datum *values)
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
 * Fetch the first column of the first row as an int, or the default.
 */
static int
spi_first_int(int dflt)
{
	bool		isnull;
	Datum		d;

	if (SPI_processed == 0)
		return dflt;
	d = SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull);
	return isnull ? dflt : DatumGetInt32(d);
}

/*
 * One node to push the catalog to.
 */
typedef struct SyncTarget
{
	char	   *host;
	int			port;
} SyncTarget;

/*
 * kingres_shard_sync_catalog_internal
 *
 * Push the full catalog to every active primary node except this one.  The
 * remote transactions stay open and are committed in the pre-commit
 * callback.  Called by every catalog-changing function before it returns.
 */
void
kingres_shard_sync_catalog_internal(void)
{
	MemoryContext callercxt = CurrentMemoryContext;
	const KsCatalog *cat;
	char	   *snapshot;
	List	   *targets = NIL;
	ListCell   *lc;

	kingres_shard_lock_catalog();
	kingres_shard_require_map_group();

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	spi_run("SELECT " KINGRES_SHARD_SCHEMA ".internal_catalog_snapshot()::pg_catalog.text",
			SPI_OK_SELECT, 0, NULL, NULL);
	snapshot = MemoryContextStrdup(callercxt,
								   SPI_getvalue(SPI_tuptable->vals[0],
												SPI_tuptable->tupdesc, 1));
	SPI_finish();

	/* The snapshot statement may have been the first read after a change. */
	cat = kingres_shard_catalog();
	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		SyncTarget *t;

		if (n->node_id == cat->local_node_id || !n->is_active || !n->is_primary)
			continue;
		t = palloc(sizeof(SyncTarget));
		t->host = pstrdup(n->host);
		t->port = n->port;
		targets = lappend(targets, t);
	}

	foreach(lc, targets)
	{
		SyncTarget *t = (SyncTarget *) lfirst(lc);
		KsConn	   *kc = kingres_shard_get_conn(t->host, t->port, false, NULL);
		const char *params[1];
		PGresult   *res;

		kingres_shard_begin_write_xact(kc);
		params[0] = snapshot;
		res = kingres_shard_exec_params(kc,
										 "SELECT " KINGRES_SHARD_SCHEMA ".internal_apply_catalog($1::pg_catalog.jsonb)",
										 1, NULL, params, true);
		PQclear(res);
	}
}

/*
 * kingres.sync_catalog() returns void
 *
 * Push the catalog of the map group primary to every active node.
 */
Datum
kingres_sync_catalog(PG_FUNCTION_ARGS)
{
	kingres_shard_sync_catalog_internal();
	PG_RETURN_VOID();
}

/*
 * create_tables_on_node
 *
 * Create the parent of every existing distributed table on a node that joins
 * the cluster.  The catalog sync resolves tables by name, so the parents must
 * exist before it is applied.  The node's group owns no buckets yet, so there
 * are no partitions to create.
 */
static void
create_tables_on_node(KsConn *kc)
{
	MemoryContext callercxt = CurrentMemoryContext;
	List	   *relids = NIL;
	List	   *columns = NIL;
	ListCell   *lc1;
	ListCell   *lc2;
	uint64		i;

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	/* Reference tables have no column: hash tables first, in colocation order. */
	spi_run("SELECT relid::pg_catalog.oid, dist_column::pg_catalog.text FROM "
			KINGRES_SHARD_SCHEMA ".dist_table ORDER BY colocation_id NULLS LAST, relid::pg_catalog.oid",
			SPI_OK_SELECT, 0, NULL, NULL);
	for (i = 0; i < SPI_processed; i++)
	{
		bool		isnull;
		char	   *col;
		MemoryContext old = MemoryContextSwitchTo(callercxt);

		relids = lappend_oid(relids,
							 DatumGetObjectId(SPI_getbinval(SPI_tuptable->vals[i],
															SPI_tuptable->tupdesc, 1, &isnull)));
		col = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2);
		columns = lappend(columns, col ? pstrdup(col) : NULL);
		MemoryContextSwitchTo(old);
	}
	SPI_finish();

	forboth(lc1, relids, lc2, columns)
	{
		char	   *column = (char *) lfirst(lc2);
		char	   *script = kingres_shard_remote_table_script(lfirst_oid(lc1), column);
		PGresult   *res = kingres_shard_exec(kc, script);

		PQclear(res);
		/* A reference table comes with its rows. */
		if (column == NULL)
			kingres_shard_copy_table_rows(lfirst_oid(lc1), list_make1(kc));
	}

	/* The foreign keys, now that every table exists on the node. */
	forboth(lc1, relids, lc2, columns)
	{
		char	   *script = kingres_shard_remote_fk_script(lfirst_oid(lc1),
															(char *) lfirst(lc2));

		if (script[0] != '\0')
		{
			PGresult   *res = kingres_shard_exec(kc, script);

			PQclear(res);
		}
	}
}

/*
 * record_pending_node
 *
 * Remember, in a transaction of its own that commits at once, that add_node()
 * is going to prepare a transaction on host:port.  The registration of the node
 * is part of the transaction that may abort or crash, so without this record
 * recovery could not find the prepared transaction on a node that never got
 * registered.  The record is written through a connection to this node itself,
 * outside the user's transaction.
 */
static void
record_pending_node(const char *host, int port)
{
	const KsCatalog *cat = kingres_shard_catalog();
	const KsNode *self = kingres_shard_node_by_id(cat->local_node_id);
	KsConn	   *kc;
	const char *params[2];
	char		portstr[16];
	PGresult   *res;

	if (self == NULL)
		return;
	kc = kingres_shard_get_conn(self->host, self->port, false, NULL);
	snprintf(portstr, sizeof(portstr), "%d", port);
	params[0] = host;
	params[1] = portstr;
	res = kingres_shard_exec_params(kc,
									 "INSERT INTO " KINGRES_SHARD_SCHEMA ".pending_node (host, port, created_at) "
									 "VALUES ($1::pg_catalog.text, $2::pg_catalog.int4, pg_catalog.clock_timestamp()) "
									 "ON CONFLICT (host, port) DO UPDATE SET created_at = excluded.created_at",
									 2, NULL, params, true);
	PQclear(res);
}

/*
 * kingres.add_node(host text, port int, group_id int, role text) returns int
 *
 * Register a node.  A primary node gets the extension created on it, its
 * local_node row and a copy of the whole catalog, all in a remote transaction
 * that commits with the local one.  A standby is only registered: it is a
 * physical replica, so its catalog arrives by replication.  A new group owns
 * no buckets until a table is created afterwards (or, in phase 4, a
 * rebalance).
 */
Datum
kingres_add_node(PG_FUNCTION_ARGS)
{
	char	   *host;
	int			port;
	bool		group_given = !PG_ARGISNULL(2);
	int			group_id = group_given ? PG_GETARG_INT32(2) : 0;
	char	   *role;
	bool		is_primary;
	int			node_id;
	Oid			argtypes[5];
	Datum		values[5];
	KsConn	   *kc = NULL;
	const char *params[1];
	PGresult   *res;
	char		idbuf[16];

	if (PG_ARGISNULL(0) || PG_ARGISNULL(1))
		ereport(ERROR,
				(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
				 errmsg("host and port must not be null")));
	host = text_to_cstring(PG_GETARG_TEXT_PP(0));
	port = PG_GETARG_INT32(1);
	role = PG_ARGISNULL(3) ? pstrdup("primary") : text_to_cstring(PG_GETARG_TEXT_PP(3));

	kingres_shard_lock_catalog();
	kingres_shard_require_map_group();

	if (strcmp(role, "primary") != 0 && strcmp(role, "standby") != 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("invalid node role \"%s\"", role),
				 errhint("The role must be \"primary\" or \"standby\".")));
	is_primary = (strcmp(role, "primary") == 0);
	if (port < 1 || port > 65535)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("invalid port %d", port)));

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	/* Reject a duplicate address with a clear message. */
	argtypes[0] = TEXTOID;
	argtypes[1] = INT4OID;
	values[0] = CStringGetTextDatum(host);
	values[1] = Int32GetDatum(port);
	spi_run("SELECT node_id FROM " KINGRES_SHARD_SCHEMA ".node WHERE host = $1 AND port = $2",
			SPI_OK_SELECT, 2, argtypes, values);
	if (SPI_processed > 0)
		ereport(ERROR,
				(errcode(ERRCODE_DUPLICATE_OBJECT),
				 errmsg("node %s:%d is already registered as node %d",
						host, port, spi_first_int(0))));

	/* Choose or check the group. */
	if (!group_given)
	{
		if (!is_primary)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("a standby node needs the group_id of its primary")));
		spi_run("SELECT coalesce(max(group_id), 0) + 1 FROM " KINGRES_SHARD_SCHEMA ".node",
				SPI_OK_SELECT, 0, NULL, NULL);
		group_id = spi_first_int(1);
	}
	else
	{
		argtypes[0] = INT4OID;
		values[0] = Int32GetDatum(group_id);
		if (is_primary)
		{
			spi_run("SELECT node_id FROM " KINGRES_SHARD_SCHEMA ".node "
					"WHERE group_id = $1 AND role = 'primary'",
					SPI_OK_SELECT, 1, argtypes, values);
			if (SPI_processed > 0)
				ereport(ERROR,
						(errcode(ERRCODE_DUPLICATE_OBJECT),
						 errmsg("group %d already has a primary (node %d)",
								group_id, spi_first_int(0))));
		}
		else
		{
			spi_run("SELECT node_id FROM " KINGRES_SHARD_SCHEMA ".node "
					"WHERE group_id = $1 AND role = 'primary'",
					SPI_OK_SELECT, 1, argtypes, values);
			if (SPI_processed == 0)
				ereport(ERROR,
						(errcode(ERRCODE_UNDEFINED_OBJECT),
						 errmsg("group %d has no primary node", group_id)));
		}
	}

	spi_run("SELECT coalesce(max(node_id), 0) + 1 FROM " KINGRES_SHARD_SCHEMA ".node",
			SPI_OK_SELECT, 0, NULL, NULL);
	node_id = spi_first_int(1);

	/*
	 * Prepare the remote node before touching the local catalog, so that an
	 * unreachable or unsuitable node fails fast.
	 */
	if (is_primary)
	{
		record_pending_node(host, port);
		kc = kingres_shard_get_conn(host, port, false, NULL);
		kingres_shard_begin_write_xact(kc);
		res = kingres_shard_exec(kc, "CREATE EXTENSION IF NOT EXISTS kingres_shard");
		PQclear(res);
		res = kingres_shard_exec(kc,
								  "SELECT (SELECT count(*) FROM " KINGRES_SHARD_SCHEMA ".local_node) + "
								  "(SELECT count(*) FROM " KINGRES_SHARD_SCHEMA ".dist_table)");
		if (PQntuples(res) != 1 || strcmp(PQgetvalue(res, 0, 0), "0") != 0)
		{
			PQclear(res);
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("node %s:%d already belongs to a cluster", host, port)));
		}
		PQclear(res);
	}

	argtypes[0] = INT4OID;
	argtypes[1] = INT4OID;
	argtypes[2] = TEXTOID;
	argtypes[3] = INT4OID;
	argtypes[4] = TEXTOID;
	values[0] = Int32GetDatum(node_id);
	values[1] = Int32GetDatum(group_id);
	values[2] = CStringGetTextDatum(host);
	values[3] = Int32GetDatum(port);
	values[4] = CStringGetTextDatum(role);
	spi_run("INSERT INTO " KINGRES_SHARD_SCHEMA ".node (node_id, group_id, host, port, role) "
			"VALUES ($1, $2, $3, $4, $5)",
			SPI_OK_INSERT, 5, argtypes, values);
	SPI_finish();

	if (is_primary)
	{
		create_tables_on_node(kc);
		snprintf(idbuf, sizeof(idbuf), "%d", node_id);
		params[0] = idbuf;
		res = kingres_shard_exec_params(kc,
										 "SELECT " KINGRES_SHARD_SCHEMA ".internal_set_local_node($1::pg_catalog.int4)",
										 1, NULL, params, true);
		PQclear(res);
	}

	kingres_shard_sync_catalog_internal();

	PG_RETURN_INT32(node_id);
}

/*
 * kingres.remove_node(node_id int) returns void
 *
 * Unregister a node whose shard group owns no buckets.  The removed node's
 * own catalog is cleared when it can be reached; if it cannot, a warning says
 * so (a dead node must still be removable).
 */
Datum
kingres_remove_node(PG_FUNCTION_ARGS)
{
	int			node_id = PG_GETARG_INT32(0);
	const KsCatalog *cat;
	const KsNode *n;
	Oid			argtypes[1];
	Datum		values[1];
	char	   *host;
	int			port;
	int			group_id;
	bool		reset_remote;
	bool		isnull;
	int			nbuckets;

	kingres_shard_lock_catalog();
	kingres_shard_require_map_group();

	cat = kingres_shard_catalog();
	n = kingres_shard_node_by_id(node_id);
	if (n == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_OBJECT),
				 errmsg("node %d does not exist", node_id)));
	if (node_id == cat->local_node_id)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("the map group primary (this node) cannot be removed")));
	host = pstrdup(n->host);
	port = n->port;
	group_id = n->group_id;
	reset_remote = n->is_primary && n->is_active;

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	argtypes[0] = INT4OID;
	values[0] = Int32GetDatum(group_id);
	spi_run("SELECT count(*) FROM " KINGRES_SHARD_SCHEMA ".bucket_map WHERE group_id = $1",
			SPI_OK_SELECT, 1, argtypes, values);
	nbuckets = (int) DatumGetInt64(SPI_getbinval(SPI_tuptable->vals[0],
												 SPI_tuptable->tupdesc, 1,
												 &isnull));
	if (nbuckets > 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_IN_USE),
				 errmsg("cannot remove node %d: its shard group %d owns %d buckets",
						node_id, group_id, nbuckets),
				 errhint("Move the buckets to other groups first (bucket movement is not available in phase 1).")));

	if (reset_remote)
	{
		char	   *err = NULL;
		KsConn	   *kc = kingres_shard_get_conn(host, port, true, &err);

		if (kc == NULL)
			ereport(WARNING,
					(errmsg("could not reach node %s:%d; its catalog was not cleared", host, port),
					 errdetail_internal("%s", err)));
		else
		{
			PGresult   *res;

			kingres_shard_begin_write_xact(kc);
			res = kingres_shard_exec(kc, "SELECT " KINGRES_SHARD_SCHEMA ".internal_reset_node()");
			PQclear(res);
		}
	}

	argtypes[0] = INT4OID;
	values[0] = Int32GetDatum(node_id);
	spi_run("DELETE FROM " KINGRES_SHARD_SCHEMA ".node WHERE node_id = $1",
			SPI_OK_DELETE, 1, argtypes, values);
	SPI_finish();

	kingres_shard_sync_catalog_internal();
	PG_RETURN_VOID();
}

/*
 * kingres_shard_reload_catalog
 *
 * Replace the local catalog by the one of the map group primary.  Used when
 * a forwarded statement found that this node's bucket map is stale
 * (design/shard-mvp.md, "Stale maps").  Returns false when this node is the
 * map group primary itself, whose catalog is already the authoritative one.
 *
 * The catalog tables belong to the extension owner, so the replacement runs
 * as the bootstrap superuser; the document that is applied comes from the
 * map group, never from the user.
 */
bool
kingres_shard_reload_catalog(void)
{
	const KsCatalog *cat = kingres_shard_catalog();
	const KsNode *mg;
	char	   *host;
	int			port;
	KsConn	   *kc;
	PGresult   *res;
	char	   *snapshot;
	Oid			save_uid;
	int			save_sec;
	Oid			argtypes[1] = {JSONBOID};
	Oid			funcid;
	Datum		doc;

	if (!cat->present)
		return false;
	mg = kingres_shard_group_primary(KINGRES_SHARD_MAP_GROUP);
	if (mg == NULL || mg->node_id == cat->local_node_id)
		return false;

	/*
	 * A hot standby cannot write its catalog; it gets the map of its primary
	 * by replication, which a stale map resolves by itself.
	 */
	if (RecoveryInProgress())
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("the bucket map of this node is stale, and a hot standby cannot reload it"),
				 errhint("The catalog of a standby arrives by replication from its primary; run kingres.sync_catalog() on the map group primary.")));
	if (IsInParallelMode())
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("the bucket map of this node is stale, and a parallel worker cannot reload it")));

	host = pstrdup(mg->host);
	port = mg->port;

	kc = kingres_shard_get_conn(host, port, false, NULL);
	res = kingres_shard_exec(kc,
							  "SELECT " KINGRES_SHARD_SCHEMA ".internal_catalog_snapshot()::pg_catalog.text");
	snapshot = pstrdup(PQgetvalue(res, 0, 0));
	PQclear(res);

	/*
	 * Everything that needs a name is looked up, and the document parsed,
	 * while the session still has the rights of the user: the elevated part
	 * below calls one function by its OID and nothing else.  No statement is
	 * parsed, so no name is resolved through the search_path or the
	 * temporary schema of the session while it runs as the bootstrap
	 * superuser (review S-B).  The function pins its own search_path and
	 * qualifies every name it uses (see the install script).
	 */
	funcid = LookupFuncName(list_make2(makeString(KINGRES_SHARD_SCHEMA),
									   makeString("internal_apply_catalog")),
							1, argtypes, false);
	doc = DirectFunctionCall1(jsonb_in, CStringGetDatum(snapshot));

	GetUserIdAndSecContext(&save_uid, &save_sec);
	SetUserIdAndSecContext(BOOTSTRAP_SUPERUSERID,
						   save_sec | SECURITY_LOCAL_USERID_CHANGE);
	PG_TRY();
	{
		(void) OidFunctionCall1(funcid, doc);
	}
	PG_FINALLY();
	{
		SetUserIdAndSecContext(save_uid, save_sec);
	}
	PG_END_TRY();

	kingres_shard_catalog_invalidate();
	return true;
}
