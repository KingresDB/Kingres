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
 * SPDX-License-Identifier: AGPL-3.0-only
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
#include "utils/array.h"
#include "utils/resowner.h"
#include "storage/lmgr.h"
#include "storage/proc.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"

#include "catalog.h"
#include "kingres_shard.h"
#include "dist_table.h"
#include "fanout.h"
#include "node_admin.h"
#include "node_conn.h"
#include "remote_scan.h"
#include "term.h"

PG_FUNCTION_INFO_V1(kingres_add_node);
PG_FUNCTION_INFO_V1(kingres_remove_node);
PG_FUNCTION_INFO_V1(kingres_sync_catalog);
PG_FUNCTION_INFO_V1(kingres_rotate_secret);
PG_FUNCTION_INFO_V1(kingres_internal_require_map_group);

/*
 * Is this node the map group primary as far as PostgreSQL says as well as the
 * catalog?  THE INVARIANT OF EVERY CATALOG CHANGE (S9-02): none is accepted from a
 * node that is not the map primary by a claim that a majority of the electorate
 * supports.  A standby of the map group that was promoted in PostgreSQL still
 * holds the old primary's catalog and the old primary's node id, so the catalog
 * calls it the map primary; what tells it apart is the WAL timeline, which the
 * node records in local_node when it becomes the map primary in the catalog
 * (promote_node, with the majority rule) and which a promotion in PostgreSQL
 * changes.  A node that has no timeline recorded (a catalog older than the
 * library) is taken as recognised.
 */
bool
kingres_shard_is_recognized_map_primary(void)
{
	const KsCatalog *cat = kingres_shard_catalog();
	const KsNode *self;

	if (!cat->present || cat->local_node_id == 0)
		return false;
	self = kingres_shard_node_by_id(cat->local_node_id);
	if (self == NULL || self->group_id != KINGRES_SHARD_MAP_GROUP || !self->is_primary)
		return false;
	if (RecoveryInProgress())
		return false;
	return cat->local_timeline == 0 || cat->local_timeline == (int) GetWALInsertionTimeLine();
}

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
				(errcode(ERRCODE_KINGRES_WRONG_NODE),
				 errmsg("this function can only run on the map group primary"),
				 errdetail("This node is node %d.", cat->local_node_id)));
	if (!kingres_shard_is_recognized_map_primary())
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_WRONG_NODE),
				 errmsg("this node was promoted in PostgreSQL after the catalog named it the map group primary, and the catalog has not recognised the promotion"),
				 errdetail("The catalog of this node is the old map primary's.  Nothing that changes a catalog is accepted here until kingres.promote_node(%d) has run on this node: it needs the primaries of a strict majority of the other groups.",
						   cat->local_node_id),
				 errhint("Call kingres.promote_node(%d) on this node.", cat->local_node_id)));
	kingres_shard_refuse_if_superseded();
}

/*
 * S9-08: a catalog change commits on every primary, and a primary whose list of
 * synchronous standbys names a standby that is not there makes the PREPARE of its part
 * wait until node_timeout, then fail with KS005 two timeouts and five seconds later (125 s
 * by default).  The wrong order (remove_node before the name is taken out of the list, a
 * typo in a name) is found out at once instead: every other primary is asked whether it
 * can commit, as promote_node asks the node it promotes, and the call is refused before it
 * changes anything.
 */
static void
require_primaries_can_commit(void)
{
	const KsCatalog *cat = kingres_shard_catalog();
	ListCell   *lc;
	int			ms = kingres_shard_node_timeout >= 2 ? Min(kingres_shard_node_timeout, 5) * 1000 - 1000 : 500;
	List	   *targets = NIL;

	/*
	 * Connecting takes locks and reads settings, which processes invalidations and loads the
	 * cache again (every time under debug_discard_caches), and the cache that was walked is
	 * gone.  What the loop needs is copied out of the cache first.
	 */
	foreach(lc, cat->nodes)
	{
		const KsNode *n = (const KsNode *) lfirst(lc);
		KsNode	   *t;

		if (n->node_id == cat->local_node_id || !n->is_active || !n->is_primary)
			continue;
		t = palloc(sizeof(KsNode));
		*t = *n;
		t->host = pstrdup(n->host);
		targets = lappend(targets, t);
	}

	foreach(lc, targets)
	{
		const KsNode *n = (const KsNode *) lfirst(lc);
		char	   *err = NULL;
		PGconn	   *conn;
		PGresult   *res;
		char		sql[160];
		bool		ok;

		conn = kingres_shard_connect_control(n->host, n->port, &err);
		if (conn == NULL)
			continue;			/* a node that cannot be reached is the business of the call itself */
		snprintf(sql, sizeof(sql), "SELECT " KINGRES_SHARD_SCHEMA ".internal_sync_probe(%d)", ms);
		res = kingres_shard_bounded_exec(conn, sql, true);
		ok = (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK || PQntuples(res) != 1 ||
			  strcmp(PQgetvalue(res, 0, 0), "t") == 0);
		PQclear(res);
		PQfinish(conn);
		if (!ok)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_CANNOT_COMMIT),
					 errmsg("node %d cannot commit: its synchronous standbys do not confirm", n->node_id),
					 errdetail("The change would have to commit on that node, and its synchronous_standby_names names a standby that is not connected to it."),
					 errhint("On node %d set synchronous_standby_names to the standbys that exist (or to an empty string) and reload the configuration, then repeat the call.  Nothing was changed.",
							 n->node_id)));
	}
}

/*
 * kingres.internal_require_map_group(): the check of every SQL function that changes the
 * catalog, the same as the C ones make (S9-02).
 */
Datum
kingres_internal_require_map_group(PG_FUNCTION_ARGS)
{
	kingres_shard_require_map_group();
	PG_RETURN_VOID();
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

	/*
	 * Not for ever (S10-07): a second caller behind a first that is stuck would hang as long
	 * as the first.  It waits at most node_timeout (or its own lock_timeout when that is
	 * shorter), then is refused with KS008, having changed nothing.
	 */
	if (ConditionalLockRelationOid(relid, ExclusiveLock))
		return;
	{
		int			nest = NewGUCNestLevel();
		char		buf[32];
		bool		busy = false;
		MemoryContext oldcxt = CurrentMemoryContext;
		ResourceOwner oldowner = CurrentResourceOwner;

		if (LockTimeout == 0 || LockTimeout > kingres_shard_node_timeout * 1000)
		{
			snprintf(buf, sizeof(buf), "%d", kingres_shard_node_timeout * 1000);
			(void) set_config_option("lock_timeout", buf, PGC_USERSET, PGC_S_SESSION,
									 GUC_ACTION_SAVE, true, 0, false);
		}
		/*
		 * In a subtransaction, as PL/pgSQL does for an exception block: an error that is caught
		 * must not leave the lock wait half undone.  The lock, once granted, belongs to the
		 * outer transaction when the subtransaction is released.
		 */
		BeginInternalSubTransaction(NULL);
		PG_TRY();
		{
			LockRelationOid(relid, ExclusiveLock);
			ReleaseCurrentSubTransaction();
			MemoryContextSwitchTo(oldcxt);
			CurrentResourceOwner = oldowner;
		}
		PG_CATCH();
		{
			ErrorData  *edata;

			MemoryContextSwitchTo(oldcxt);
			edata = CopyErrorData();
			FlushErrorState();
			RollbackAndReleaseCurrentSubTransaction();
			MemoryContextSwitchTo(oldcxt);
			CurrentResourceOwner = oldowner;
			/* the lock timeout is the only error that is ours to translate */
			if (edata->sqlerrcode != ERRCODE_LOCK_NOT_AVAILABLE)
				ReThrowError(edata);
			busy = true;
		}
		PG_END_TRY();
		AtEOXact_GUC(true, nest);
		if (busy)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_BUSY),
					 errmsg("another call that changes the catalog is running on this node, and did not end within %d seconds",
							kingres_shard_node_timeout),
					 errdetail("Calls that change the catalog (add_node, remove_node, promote_node, sync_catalog, rotate_secret, create_distributed_table and others) run one at a time; this one waited for the lock on kingres.map_version and changed nothing."),
					 errhint("Repeat the call later.  If the first call does not end, find its session in pg_stat_activity and the node that it waits for (its answer names it).")));
	}
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
 * push_secret
 *
 * Install the cluster secret on the node of kc, in the remote transaction that
 * commits with the local one.  The value is an argument of a statement, which
 * a server may log with its arguments, in the log line of a statement and in
 * the context of an error: the session is told not to
 * (log_parameter_max_length and log_parameter_max_length_on_error are 0 for
 * this transaction), so the secret reaches no log and no error message, also
 * when the statement fails (S3-06).  Each setting is a command of its own: the
 * extended protocol refuses several commands in one statement.
 */
static void
push_secret(KsConn *kc)
{
	unsigned char secret[128];
	int			len = 0;
	char		hex[2 * sizeof(secret) + 1];
	const char *params[1];
	PGresult   *res;

	if (!kingres_shard_cluster_secret(secret, sizeof(secret), &len))
		return;
	(void) hex_encode((const char *) secret, len, hex);
	hex[2 * len] = '\0';
	params[0] = hex;
	kingres_shard_begin_write_xact(kc);
	res = kingres_shard_exec(kc, "SET LOCAL log_parameter_max_length = 0");
	PQclear(res);
	res = kingres_shard_exec(kc, "SET LOCAL log_parameter_max_length_on_error = 0");
	PQclear(res);
	res = kingres_shard_exec_params(kc,
									 "SELECT " KINGRES_SHARD_SCHEMA ".internal_apply_secret($1::pg_catalog.text)",
									 1, NULL, params, true);
	PQclear(res);
	explicit_bzero(secret, sizeof(secret));
	explicit_bzero(hex, sizeof(hex));
}

/*
 * kingres.sync_catalog() returns void
 *
 * Push the catalog of the map group primary to every active node, and the
 * cluster secret with it (made first if this node has none: the step that
 * secures a cluster upgraded from 0.1).
 */
static Datum
kingres_sync_catalog_body(PG_FUNCTION_ARGS)
{
	const KsCatalog *cat;
	ListCell   *lc;
	List	   *targets = NIL;

	kingres_shard_lock_catalog();
	kingres_shard_require_map_group();
	require_primaries_can_commit();
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	spi_run("SELECT " KINGRES_SHARD_SCHEMA ".internal_ensure_secret()", SPI_OK_SELECT, 0, NULL, NULL);
	SPI_finish();

	kingres_shard_sync_catalog_internal();

	cat = kingres_shard_catalog();
	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);

		if (n->node_id == cat->local_node_id || !n->is_active || !n->is_primary)
			continue;
		targets = lappend(targets, n);
	}
	foreach(lc, targets)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		KsConn	   *kc = kingres_shard_get_conn(n->host, n->port, false, NULL);
		char		idbuf[16];
		const char *params[1];

		/*
		 * A node that was restored from a logical dump has the extension and its
		 * tables but no catalog and no idea which node it is (the dump holds
		 * neither, review finding S6-08): the catalog is the authority on who
		 * lives at an address, so tell it, once, in the same transaction.  A node
		 * that knows its id is not touched.  The statement runs before the
		 * secret arrives, while the node accepts the session of a superuser.
		 */
		snprintf(idbuf, sizeof(idbuf), "%d", n->node_id);
		params[0] = idbuf;
		kingres_shard_begin_write_xact(kc);
		PQclear(kingres_shard_exec_params(kc,
										   "SELECT " KINGRES_SHARD_SCHEMA ".internal_set_local_node($1::pg_catalog.int4) "
										   "WHERE NOT EXISTS (SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".local_node)",
										   1, NULL, params, true));
		push_secret(kc);
	}
	PG_RETURN_VOID();
}

Datum
kingres_sync_catalog(PG_FUNCTION_ARGS)
{
	return kingres_shard_control_call(kingres_sync_catalog_body, fcinfo);
}

/*
 * kingres.rotate_secret() returns void
 *
 * Replace the cluster secret on every active primary node, in the one
 * two-phase transaction that every catalog change uses.  Every connection is
 * opened first: a new connection is authenticated with the secret the other
 * node holds now, and the origin's own copy changes in the next step.  Sessions
 * that other nodes open after their own commit use the new secret; during the
 * commit itself (milliseconds) a session opened toward a node that has not yet
 * committed is refused and the statement has to be repeated.  The old secret
 * stops working at once on the nodes that have committed: this is how a leaked
 * secret, or a copy of a node that left the control of the operator, is made
 * useless.
 */
static Datum
kingres_rotate_secret_body(PG_FUNCTION_ARGS)
{
	const KsCatalog *cat;
	ListCell   *lc;
	List	   *conns = NIL;
	uint8		raw[32];
	char		hex[2 * sizeof(raw) + 1];
	Datum		values[1];
	Oid			argtypes[1] = {TEXTOID};

	kingres_shard_lock_catalog();
	kingres_shard_require_map_group();
	require_primaries_can_commit();

	cat = kingres_shard_catalog();
	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);

		if (n->node_id == cat->local_node_id || !n->is_active || !n->is_primary)
			continue;
		conns = lappend(conns, kingres_shard_get_conn(n->host, n->port, false, NULL));
	}

	/*
	 * A transaction that is in doubt carries a tag made with the secret of today, and
	 * recovery resolves only the names whose tag verifies (S7-09): after the
	 * rotation it would hold its locks for ever and be called foreign (S8-03).  So
	 * the rotation waits for nothing and refuses while any node holds one.  Names
	 * whose tag does not verify are somebody else's and do not count (a role could
	 * otherwise stop rotations by preparing a transaction of that form).
	 */
	{
		static const char in_doubt_sql[] =
			"SELECT pg_catalog.count(*)::pg_catalog.text FROM pg_catalog.pg_prepared_xacts p "
			"WHERE p.database OPERATOR(pg_catalog.=) pg_catalog.current_database() "
			"AND p.gid LIKE 'kingres\\_h%' AND pg_catalog.length(p.gid) OPERATOR(pg_catalog.>) 17 "
			"AND pg_catalog.right(p.gid, 16) OPERATOR(pg_catalog.=) "
			KINGRES_SHARD_SCHEMA ".internal_gid_tag(pg_catalog.left(p.gid, pg_catalog.length(p.gid) OPERATOR(pg_catalog.-) 17))";
		StringInfoData where;
		int			total = 0;

		initStringInfo(&where);
		if (SPI_connect() != SPI_OK_CONNECT)
			elog(ERROR, "SPI_connect failed");
		if (SPI_execute(in_doubt_sql, true, 0) != SPI_OK_SELECT || SPI_processed != 1)
			elog(ERROR, "could not look for distributed transactions in doubt");
		{
			int			c = atoi(SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1));

			if (c > 0)
			{
				total += c;
				appendStringInfo(&where, "node %d (%d)", cat->local_node_id, c);
			}
		}
		SPI_finish();
		foreach(lc, conns)
		{
			KsConn	   *kc = (KsConn *) lfirst(lc);
			PGresult   *res = kingres_shard_exec_params(kc, in_doubt_sql, 0, NULL, NULL, true);
			int			c;

			if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK || PQntuples(res) != 1)
				ereport(ERROR,
						(errcode(ERRCODE_CONNECTION_FAILURE),
						 errmsg("could not look for distributed transactions in doubt on node %s:%d",
								kc->host, kc->port)));
			c = atoi(PQgetvalue(res, 0, 0));
			PQclear(res);
			if (c > 0)
			{
				total += c;
				appendStringInfo(&where, "%snode %d (%d)", where.len > 0 ? ", " : "",
								 kingres_shard_conn_node_id(kc->host, kc->port), c);
			}
		}
		if (total > 0)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_IN_DOUBT),
					 errmsg("the cluster secret cannot be rotated while distributed transactions are in doubt"),
					 errdetail("%d prepared transaction(s) of this cluster: %s.", total, where.data),
					 errhint("Run kingres.recover_prepared_transactions() on the origin of each, wait until kingres.check_cluster() shows none, and rotate again.")));
	}

	/*
	 * A node that missed the last rotation can be reached only with the secret that the rotation
	 * replaced, which is kept as "previous" for exactly that reason (S11-04).  A second rotation
	 * would replace it again: the node would hold a secret that nobody has, could not be reached,
	 * and could not be promoted.  So while the previous secret is kept, every standby of the
	 * catalog must be reached with the current one first.
	 */
	{
		unsigned char pbuf[128];
		int			plen = 0;

		if (kingres_shard_previous_secret(pbuf, sizeof(pbuf), &plen))
		{
			explicit_bzero(pbuf, sizeof(pbuf));
			foreach(lc, cat->nodes)
			{
				KsNode	   *n = (KsNode *) lfirst(lc);
				char	   *serr = NULL;
				KsConn	   *skc;

				if (n->node_id == cat->local_node_id || !n->is_active || n->is_primary)
					continue;
				skc = kingres_shard_get_conn(n->host, n->port, true, &serr);
				if (skc == NULL || skc->via_previous)
					ereport(ERROR,
							(errcode(ERRCODE_KINGRES_SECRET_UNKNOWN),
							 errmsg("the cluster secret cannot be rotated again yet: standby %d (%s:%d) has not confirmed the last rotation",
									n->node_id, n->host, n->port),
							 errdetail("%s", skc == NULL ? (serr ? serr : "it cannot be reached") :
									   "it accepts only the previous secret, which a second rotation would drop"),
							 errhint("Start the standby and let it replay the WAL, run kingres.recover_prepared_transactions() until it has confirmed the secret, and rotate again; or remove the standby from the cluster with kingres.remove_node().")));
			}
		}
	}

	if (!pg_strong_random(raw, sizeof(raw)))
		ereport(ERROR,
				(errcode(ERRCODE_INTERNAL_ERROR),
				 errmsg("could not generate a random secret")));
	(void) hex_encode((const char *) raw, sizeof(raw), hex);
	hex[2 * sizeof(raw)] = '\0';
	values[0] = CStringGetTextDatum(hex);
	explicit_bzero(raw, sizeof(raw));

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	spi_run("SELECT " KINGRES_SHARD_SCHEMA ".internal_apply_secret($1)", SPI_OK_SELECT,
			1, argtypes, values);
	SPI_finish();
	explicit_bzero(hex, sizeof(hex));

	foreach(lc, conns)
		push_secret((KsConn *) lfirst(lc));
	PG_RETURN_VOID();
}

Datum
kingres_rotate_secret(PG_FUNCTION_ARGS)
{
	return kingres_shard_control_call(kingres_rotate_secret_body, fcinfo);
}

/*
 * The objects whose owner and privileges a new node must get (review finding
 * S3-11): every distributed and reference table, the sequences that belong
 * to a column of one, and the schemas that hold the tables.  The sequences
 * follow the owner of their table (OWNER TO of the table changes them), their
 * privileges are copied.
 */
#define ACL_OBJECTS_CTE \
	"WITH obj AS (" \
	"SELECT c.oid, 'TABLE' AS kind, " \
	"       pg_catalog.quote_ident(n.nspname) || '.' || pg_catalog.quote_ident(c.relname) AS q, " \
	"       c.relacl, c.relowner " \
	"  FROM " KINGRES_SHARD_SCHEMA ".dist_table d " \
	"  JOIN pg_class c ON c.oid = d.relid::pg_catalog.oid " \
	"  JOIN pg_namespace n ON n.oid = c.relnamespace " \
	" UNION ALL " \
	"SELECT s.oid, 'SEQUENCE', " \
	"       pg_catalog.quote_ident(sn.nspname) || '.' || pg_catalog.quote_ident(s.relname), " \
	"       s.relacl, s.relowner " \
	"  FROM pg_depend dp " \
	"  JOIN pg_class s ON s.oid = dp.objid AND s.relkind = 'S' " \
	"  JOIN pg_namespace sn ON sn.oid = s.relnamespace " \
	" WHERE dp.classid = 'pg_class'::pg_catalog.regclass " \
	"   AND dp.refclassid = 'pg_class'::pg_catalog.regclass " \
	"   AND dp.deptype IN ('a', 'i') " \
	"   AND dp.refobjid IN (SELECT d2.relid::pg_catalog.oid FROM " KINGRES_SHARD_SCHEMA ".dist_table d2) " \
	" UNION ALL " \
	"SELECT ns.oid, 'SCHEMA', pg_catalog.quote_ident(ns.nspname), ns.nspacl, ns.nspowner " \
	"  FROM pg_namespace ns " \
	" WHERE ns.oid IN (SELECT c3.relnamespace FROM " KINGRES_SHARD_SCHEMA ".dist_table d3 " \
	"                   JOIN pg_class c3 ON c3.oid = d3.relid::pg_catalog.oid)) "

/*
 * The statements, in order, that give the new node the privileges of the
 * objects.  The ACL of an object that has one is rebuilt from nothing (the
 * default privileges of a new object are not the ones the object has), then
 * granted entry by entry, with grant options.  The grantor is not kept: the
 * statements run as a superuser, so the grantor is the owner, which is what
 * the effective rights depend on.
 */
#define ACL_STATEMENTS_SQL \
	ACL_OBJECTS_CTE \
	"SELECT stmt FROM (" \
	" SELECT o.oid AS oid, 0 AS ord, '' AS k, " \
	"        pg_catalog.format('ALTER SCHEMA %s OWNER TO %I', o.q, r0.rolname) AS stmt " \
	"   FROM obj o JOIN pg_roles r0 ON r0.oid = o.relowner WHERE o.kind = 'SCHEMA' " \
	" UNION ALL " \
	" SELECT o.oid, 1, '', pg_catalog.format('REVOKE ALL ON %s %s FROM PUBLIC', o.kind, o.q) " \
	"   FROM obj o WHERE o.relacl IS NOT NULL " \
	" UNION ALL " \
	" SELECT o.oid, 2, '', pg_catalog.format('REVOKE ALL ON %s %s FROM %I', o.kind, o.q, r.rolname) " \
	"   FROM obj o JOIN pg_roles r ON r.oid = o.relowner WHERE o.relacl IS NOT NULL " \
	" UNION ALL " \
	" SELECT o.oid, 3, '', pg_catalog.format('GRANT %s ON %s %s TO %s%s', " \
	"          pg_catalog.string_agg(x.privilege_type, ', ' ORDER BY x.privilege_type), o.kind, o.q, " \
	"          CASE WHEN x.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.quote_ident(g.rolname) END, " \
	"          CASE WHEN x.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END) " \
	"   FROM obj o CROSS JOIN LATERAL pg_catalog.aclexplode(o.relacl) x " \
	"   LEFT JOIN pg_roles g ON g.oid = x.grantee " \
	"  WHERE o.relacl IS NOT NULL " \
	"  GROUP BY o.oid, o.kind, o.q, x.grantee, g.rolname, x.is_grantable " \
	" UNION ALL " \
	" SELECT o.oid, 4, a.attname::pg_catalog.text, " \
	"          pg_catalog.format('GRANT %s (%I) ON TABLE %s TO %s%s', " \
	"          pg_catalog.string_agg(x.privilege_type, ', ' ORDER BY x.privilege_type), a.attname, o.q, " \
	"          CASE WHEN x.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.quote_ident(g.rolname) END, " \
	"          CASE WHEN x.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END) " \
	"   FROM obj o JOIN pg_attribute a ON a.attrelid = o.oid AND a.attacl IS NOT NULL AND NOT a.attisdropped " \
	"   CROSS JOIN LATERAL pg_catalog.aclexplode(a.attacl) x " \
	"   LEFT JOIN pg_roles g ON g.oid = x.grantee " \
	"  WHERE o.kind = 'TABLE' " \
	"  GROUP BY o.oid, a.attname, o.q, x.grantee, g.rolname, x.is_grantable) s " \
	"ORDER BY oid, ord, k, stmt"

/* The names of the roles those statements and the owners of the tables need. */
#define ACL_ROLES_SQL \
	ACL_OBJECTS_CTE \
	"SELECT DISTINCT rolname FROM (" \
	" SELECT r.rolname::pg_catalog.text AS rolname FROM obj o JOIN pg_roles r ON r.oid = o.relowner " \
	"  WHERE o.kind IN ('TABLE', 'SCHEMA') " \
	" UNION ALL " \
	" SELECT g.rolname::pg_catalog.text FROM obj o CROSS JOIN LATERAL pg_catalog.aclexplode(o.relacl) x " \
	"   JOIN pg_roles g ON g.oid = x.grantee " \
	" UNION ALL " \
	" SELECT g.rolname::pg_catalog.text FROM obj o " \
	"   JOIN pg_attribute a ON a.attrelid = o.oid AND a.attacl IS NOT NULL " \
	"   CROSS JOIN LATERAL pg_catalog.aclexplode(a.attacl) x " \
	"   JOIN pg_roles g ON g.oid = x.grantee) s " \
	"ORDER BY 1"

/*
 * acl_query
 *
 * The first column of every row of one of the queries above, as strings in the
 * caller's memory context.  The query runs with a search_path that holds
 * nothing but the system catalogs.
 */
static List *
acl_query(const char *sql)
{
	MemoryContext callercxt = CurrentMemoryContext;
	List	   *result = NIL;
	int			nest;
	uint64		i;

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	nest = NewGUCNestLevel();
	(void) set_config_option("search_path", "pg_catalog, pg_temp", PGC_USERSET,
							 PGC_S_SESSION, GUC_ACTION_SAVE, true, 0, false);
	spi_run(sql, SPI_OK_SELECT, 0, NULL, NULL);
	for (i = 0; i < SPI_processed; i++)
	{
		MemoryContext old = MemoryContextSwitchTo(callercxt);

		result = lappend(result,
						 pstrdup(SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1)));
		MemoryContextSwitchTo(old);
	}
	AtEOXact_GUC(true, nest);
	SPI_finish();
	return result;
}

/*
 * check_acl_roles_on_node
 *
 * A node that joins the cluster gets the owners and privileges of the existing
 * tables; a role they name that does not exist there would fail the statement
 * in the middle of the work.  Say which roles are missing before anything is
 * changed, so that the operator can create them (roles are not copied: they
 * hold passwords and cluster-wide settings of their own).
 */
static void
check_acl_roles_on_node(KsConn *kc, const char *host, int port)
{
	List	   *roles = acl_query(ACL_ROLES_SQL);
	StringInfoData array;
	PGresult   *res;
	ListCell   *lc;

	if (roles == NIL)
		return;
	initStringInfo(&array);
	foreach(lc, roles)
		appendStringInfo(&array, "%s%s", array.len ? ", " : "", quote_literal_cstr((char *) lfirst(lc)));
	res = kingres_shard_exec(kc,
							 psprintf("SELECT pg_catalog.string_agg(r, ', ' ORDER BY r) "
									  "FROM pg_catalog.unnest(ARRAY[%s]::pg_catalog.text[]) AS r "
									  "WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles x "
									  "WHERE x.rolname OPERATOR(pg_catalog.=) r)", array.data));
	if (!PQgetisnull(res, 0, 0))
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_OBJECT),
				 errmsg("cannot add node %s:%d: the roles %s own tables or hold privileges on them in this cluster and do not exist on the new node",
						host, port, PQgetvalue(res, 0, 0)),
				 errhint("Create the roles on the new node (CREATE ROLE, with the same names), then repeat add_node().")));
	PQclear(res);
}

/*
 * copy_acls_to_node
 *
 * Give the tables (and the sequences of their columns) that were just created
 * on the new node the privileges they have here.  Without them a role that
 * may write a table everywhere else gets "permission denied" on the new node,
 * from the first statement that reaches it.  The owners of the tables are set
 * by the script that creates them.
 */
static void
copy_acls_to_node(KsConn *kc)
{
	List	   *stmts = acl_query(ACL_STATEMENTS_SQL);
	StringInfoData script;
	ListCell   *lc;

	if (stmts == NIL)
		return;
	initStringInfo(&script);
	foreach(lc, stmts)
		appendStringInfo(&script, "%s;\n", (char *) lfirst(lc));
	PQclear(kingres_shard_exec(kc, script.data));
}

/* How many statements a batch of schemas holds at most (a schema is never split: a single bigger schema is a batch of its own). */
int			kingres_shard_add_node_batch_statements = 100;

/*
 * The errors that say a schema reads something that the new node does not have yet (a reference table, which is made in the transaction
 * of the registration): the schema is made there, after them.
 */
static bool
stage_error_is_dependency(int sqlstate, const char *msg)
{
	/* a role that the node lacks is undefined_object too, and no later transaction makes it: that is an error of the call (P12R-05) */
	if (sqlstate == ERRCODE_UNDEFINED_OBJECT && msg != NULL && strstr(msg, "role \"") != NULL)
		return false;
	return sqlstate == ERRCODE_UNDEFINED_TABLE || sqlstate == ERRCODE_UNDEFINED_OBJECT ||
		sqlstate == ERRCODE_INVALID_SCHEMA_NAME || sqlstate == ERRCODE_UNDEFINED_FUNCTION;
}

static void
stage_exec_ok(PGconn *conn, const char *sql, const char *what)
{
	PGresult   *res = kingres_shard_bounded_exec(conn, sql, true);

	if (res == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("the new node stopped answering while %s", what),
				 errhint("Repeat add_node(): the schemas that are complete on the node are kept.")));
	if (PQresultStatus(res) != PGRES_COMMAND_OK && PQresultStatus(res) != PGRES_TUPLES_OK)
	{
		char	   *msg = pstrdup(PQresultErrorMessage(res));
		int			state = kingres_shard_result_sqlstate(res);

		PQclear(res);
		ereport(ERROR,
				(errcode(state != 0 ? state : ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("the new node refused a statement while %s", what),
				 errdetail_internal("%s", kingres_shard_scrub_conn_error(msg)),
				 errhint("Fix the cause and repeat add_node(): the schemas that are complete on the node are kept.")));
	}
	PQclear(res);
}

/*
 * stage_schemas_on_node
 *
 * The distributed schemas of the cluster are made on the new node before it is registered, in transactions of their own, a batch of schemas
 * each, so that no transaction holds more locks than kingres_shard.add_node_batch_statements allows (a transaction that made every schema
 * ran out of lock table slots, max_locks_per_transaction, from about a thousand schemas on).  The node is not in the catalog yet, so no
 * statement can reach what is half done; what a batch completed is in kingres.add_node_progress on the node, with the digest of the
 * statements, so that a repeated call skips it and the registration (create_tables_on_node, the last transaction) checks that every schema
 * is there and removes the rows.  A schema that cannot be made yet because it reads a reference table is returned in *late_scripts and made
 * in the last transaction.  Returns the number of schemas that are complete on the node.
 */
static int
stage_schemas_on_node(const char *host, int port, MemoryContext keepcxt, List **late_scripts, int *nlate, bool *has_progress)
{
	MemoryContext callercxt = CurrentMemoryContext;
	List	   *names = NIL;
	List	   *scripts = NIL;
	List	   *digests = NIL;
	List	   *weights = NIL;
	ListCell   *l1;
	ListCell   *l2;
	ListCell   *l3;
	ListCell   *l4;
	uint64		i;
	PGconn	   *conn;
	char	   *err = NULL;
	PGresult   *res;
	HTAB	   *done;
	HASHCTL		ctl;
	int			staged = 0;
	int			skipped = 0;
	int			nbatches = 0;
	int			weight = 0;
	bool		open = false;

	*late_scripts = NIL;
	*nlate = 0;
	*has_progress = false;
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	spi_run("SELECT nsp::pg_catalog.text, script, digest, n FROM " KINGRES_SHARD_SCHEMA ".internal_add_node_schemas()",
			SPI_OK_SELECT, 0, NULL, NULL);
	for (i = 0; i < SPI_processed; i++)
	{
		MemoryContext old = MemoryContextSwitchTo(callercxt);
		bool		isnull;

		names = lappend(names, SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1));
		scripts = lappend(scripts, SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2));
		digests = lappend(digests, SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 3));
		weights = lappend_int(weights, DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 4, &isnull)));
		MemoryContextSwitchTo(old);
	}
	SPI_finish();

	/*
	 * With no distributed schema the node is still asked: an earlier call may have left schemas and progress rows on it, which the registration
	 * must not carry into the cluster (P12R-04).
	 */
	conn = kingres_shard_connect_control(host, port, &err);
	if (conn == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not connect to the new node %s:%d to make the distributed schemas", host, port),
				 errdetail_internal("%s", err ? err : "")));

	memset(&ctl, 0, sizeof(ctl));
	ctl.keysize = NAMEDATALEN;
	ctl.entrysize = NAMEDATALEN;
	ctl.hcxt = callercxt;
	done = hash_create("add_node staged schemas", 256, &ctl, HASH_ELEM | HASH_STRINGS | HASH_CONTEXT);

	PG_TRY();
	{
		if (names == NIL)
		{
			res = kingres_shard_bounded_exec(conn, "SELECT pg_catalog.to_regclass('" KINGRES_SHARD_SCHEMA ".add_node_progress') IS NOT NULL", true);
			if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK || PQntuples(res) != 1)
				ereport(ERROR,
						(errcode(ERRCODE_CONNECTION_FAILURE),
						 errmsg("the new node %s:%d could not be asked what it holds", host, port)));
			if (strcmp(PQgetvalue(res, 0, 0), "t") != 0)
			{
				PQclear(res);
				goto stage_done;
			}
			PQclear(res);
		}
		*has_progress = true;
		/* The extension is made in a transaction of its own: the batches need its table, and they commit before the registration. */
		stage_exec_ok(conn, "CREATE EXTENSION IF NOT EXISTS kingres_shard", "making the extension");
		res = kingres_shard_bounded_exec(conn,
										 "SELECT (SELECT count(*) FROM " KINGRES_SHARD_SCHEMA ".local_node) + "
										 "(SELECT count(*) FROM " KINGRES_SHARD_SCHEMA ".dist_table)", true);
		if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK || PQntuples(res) != 1)
			ereport(ERROR,
					(errcode(ERRCODE_CONNECTION_FAILURE),
					 errmsg("the new node %s:%d could not be asked what it holds", host, port),
					 errhint("The extension on the node may be another version: run ALTER EXTENSION kingres_shard UPDATE there.")));
		if (strcmp(PQgetvalue(res, 0, 0), "0") != 0)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("node %s:%d already belongs to a cluster", host, port)));
		PQclear(res);
		res = kingres_shard_bounded_exec(conn, "SELECT nsp::pg_catalog.text, digest FROM " KINGRES_SHARD_SCHEMA ".add_node_progress", true);
		if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("the new node %s:%d has no table " KINGRES_SHARD_SCHEMA ".add_node_progress", host, port),
					 errhint("The node runs another version of kingres_shard: run ALTER EXTENSION kingres_shard UPDATE there, or DROP EXTENSION kingres_shard.")));
		/*
		 * What an earlier call made: kept when the schema is still a distributed schema with the same statements; a schema that went, or
		 * that changed meanwhile, is dropped here (the progress row proves that this call made it) and made again below.
		 */
		{
			HASHCTL		cctl;
			HTAB	   *cur;

			memset(&cctl, 0, sizeof(cctl));
			cctl.keysize = NAMEDATALEN;
			cctl.entrysize = NAMEDATALEN + 72;
			cctl.hcxt = callercxt;
			cur = hash_create("add_node current schemas", 256, &cctl, HASH_ELEM | HASH_STRINGS | HASH_CONTEXT);
			forboth(l1, names, l3, digests)
			{
				bool		found;
				char	   *e = hash_search(cur, lfirst(l1), HASH_ENTER, &found);

				strlcpy(e + NAMEDATALEN, (char *) lfirst(l3), 72);
			}
			for (i = 0; i < (uint64) PQntuples(res); i++)
			{
				char	   *pn = PQgetvalue(res, (int) i, 0);
				char	   *e = hash_search(cur, pn, HASH_FIND, NULL);

				if (e != NULL && strcmp(e + NAMEDATALEN, PQgetvalue(res, (int) i, 1)) == 0)
				{
					bool		found;

					hash_search(done, pn, HASH_ENTER, &found);
				}
				else
				{
					StringInfoData drop;

					initStringInfo(&drop);
					appendStringInfo(&drop, "DROP SCHEMA IF EXISTS %s CASCADE;\nDELETE FROM " KINGRES_SHARD_SCHEMA ".add_node_progress WHERE nsp OPERATOR(pg_catalog.=) %s",
									 quote_identifier(pn), quote_literal_cstr(pn));
					stage_exec_ok(conn, drop.data, "dropping a schema that changed since an earlier call");
				}
			}
		}
		PQclear(res);

		forfour(l1, names, l2, scripts, l3, digests, l4, weights)
		{
			char	   *nm = (char *) lfirst(l1);
			char	   *script = (char *) lfirst(l2);
			char	   *digest = (char *) lfirst(l3);
			int			w = lfirst_int(l4);
			bool		have = hash_search(done, nm, HASH_FIND, NULL) != NULL;
			StringInfoData sql;
			int			sqlstate;

			CHECK_FOR_INTERRUPTS();
			if (have)
			{
				skipped++;
				staged++;
				continue;
			}
			if (open && weight + w > kingres_shard_add_node_batch_statements)
			{
				stage_exec_ok(conn, "COMMIT", "completing a batch of schemas");
				kingres_shard_fault_point("add_node_batch_done");
				open = false;
				weight = 0;
				nbatches++;
			}
			if (!open)
			{
				stage_exec_ok(conn, "BEGIN", "starting a batch of schemas");
				open = true;
			}
			stage_exec_ok(conn, "SAVEPOINT ks_stage", "making a schema");
			initStringInfo(&sql);
			appendStringInfo(&sql, "%s;\nINSERT INTO " KINGRES_SHARD_SCHEMA ".add_node_progress (nsp, digest) VALUES (%s, %s)",
							 script, quote_literal_cstr(nm), quote_literal_cstr(digest));
			res = kingres_shard_bounded_exec(conn, sql.data, true);
			if (res == NULL)
				ereport(ERROR,
						(errcode(ERRCODE_CONNECTION_FAILURE),
						 errmsg("the new node stopped answering while it made the schema \"%s\"", nm),
						 errhint("Repeat add_node(): the schemas that are complete on the node are kept.")));
			if (PQresultStatus(res) == PGRES_COMMAND_OK || PQresultStatus(res) == PGRES_TUPLES_OK)
			{
				PQclear(res);
				stage_exec_ok(conn, "RELEASE SAVEPOINT ks_stage", "making a schema");
				weight += w;
				staged++;
				continue;
			}
			sqlstate = kingres_shard_result_sqlstate(res);
			if (stage_error_is_dependency(sqlstate, PQresultErrorMessage(res)))
			{
				/* Not yet: it reads a table that the registration makes.  What the statements did is undone with the savepoint. */
				PQclear(res);
				stage_exec_ok(conn, "ROLLBACK TO SAVEPOINT ks_stage", "making a schema");
				{
					MemoryContext old = MemoryContextSwitchTo(keepcxt);

					*late_scripts = lappend(*late_scripts, pstrdup(script));
					MemoryContextSwitchTo(old);
				}
				(*nlate)++;
				continue;
			}
			{
				char	   *msg = pstrdup(PQresultErrorMessage(res));

				PQclear(res);
				ereport(ERROR,
						(errcode(sqlstate != 0 ? sqlstate : ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
						 errmsg("the new node refused a statement of the schema \"%s\"", nm),
						 errdetail_internal("%s", kingres_shard_scrub_conn_error(msg)),
						 errhint("Fix the cause (a role that the schema names may be missing on the node) and repeat add_node(): the schemas that are complete on the node are kept.")));
			}
		}
		if (open)
		{
			stage_exec_ok(conn, "COMMIT", "completing a batch of schemas");
			kingres_shard_fault_point("add_node_batch_done");
			nbatches++;
		}
stage_done:
		;
	}
	PG_CATCH();
	{
		PQfinish(conn);
		PG_RE_THROW();
	}
	PG_END_TRY();
	PQfinish(conn);

	if (names != NIL)
	ereport(NOTICE,
			(errmsg("add_node: %d distributed schemas are on the node in %d batches (%d were made by an earlier call, %d are made in the last transaction)",
					staged, nbatches, skipped, *nlate)));
	return staged;
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
create_tables_on_node(KsConn *kc, List *late_scripts)
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
			KINGRES_SHARD_SCHEMA ".dist_table WHERE method OPERATOR(pg_catalog.<>) 'schema' ORDER BY colocation_id NULLS LAST, relid::pg_catalog.oid",
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

	/*
	 * The schemas that could not be made before (they read a reference table), now that the reference tables exist in this transaction.
	 */
	foreach(lc1, late_scripts)
		PQclear(kingres_shard_exec(kc, (char *) lfirst(lc1)));

	/* Owners were set with the tables; the privileges follow now that every table exists. */
	copy_acls_to_node(kc);
}

/* The SQLSTATE the node gave to the last refused ask_node(), or 0 (S12-05). */
static int	ask_node_sqlstate = 0;

/*
 * One value of a one-row result from a session on host:port, or NULL when the
 * node cannot be asked.  Used on throwaway sessions, outside the transaction.
 */
static char *
ask_node(const char *host, int port, const char *sql, char **err_out)
{
	char	   *local_err = NULL;
	PGconn	   *conn;
	PGresult   *res;
	char	   *value = NULL;

	if (err_out == NULL)
		err_out = &local_err;
	ask_node_sqlstate = 0;
	conn = kingres_shard_connect_control(host, port, err_out);
	if (conn == NULL)
	{
		ask_node_sqlstate = kingres_shard_connect_refusal_sqlstate();
		return NULL;
	}
	res = kingres_shard_bounded_exec(conn, sql, true);
	if (res != NULL && PQresultStatus(res) == PGRES_TUPLES_OK && PQntuples(res) == 1 &&
		!PQgetisnull(res, 0, 0))
		value = pstrdup(PQgetvalue(res, 0, 0));
	else
	{
		ask_node_sqlstate = kingres_shard_result_sqlstate(res);
		*err_out = kingres_shard_conn_error_text(conn);
	}
	PQclear(res);
	PQfinish(conn);
	return value;
}

/*
 * check_standby_node
 *
 * A standby is registered as a node that owns nothing and is skipped by
 * routing and synchronization, so nothing breaks on the primaries whatever its
 * state; what is checked is that the address is the right kind of node when the
 * node can be asked.  A standby that is not reachable (it is being built, or it
 * is down) is registered with a warning: "registered but not streaming" is a
 * state the cluster has to live with (kingres.check_cluster() reports it).  A
 * node that answers must be in recovery, run the PostgreSQL major of this node,
 * and be a copy of the primary of its group (the same system identifier).
 */
static void
check_standby_node(const char *host, int port, int group_id)
{
	const KsNode *primary = kingres_shard_group_primary(group_id);
	char	   *err = NULL;
	char	   *state;
	char	   *sysid_sb;
	char	   *sysid_pr;
	char	   *major;

	/*
	 * The session proves itself with a token made for the node id the standby
	 * holds as its own, which is the id of its primary until it is promoted.
	 */
	if (primary != NULL)
		kingres_shard_conn_expect_node(host, port, primary->node_id);
	state = ask_node(host, port, "SELECT pg_catalog.pg_is_in_recovery()::pg_catalog.text", &err);
	if (state == NULL && ask_node_sqlstate == ERRCODE_KINGRES_SECRET_UNKNOWN)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("node %s:%d is not a copy of the primary of group %d",
						host, port, group_id),
				 errdetail("The node did not accept a session of this cluster: it holds another cluster secret, or another node id.")));
	if (state == NULL)
	{
		ereport(WARNING,
				(errmsg("standby %s:%d could not be reached; it is registered anyway", host, port),
				 errdetail_internal("%s", err ? err : ""),
				 errhint("kingres.check_cluster() reports a standby that is not reachable or not streaming.")));
		return;
	}
	if (strcmp(state, "true") != 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("node %s:%d is not in recovery, so it is not a standby", host, port),
				 errhint("Register a standby that replicates from the primary of group %d.", group_id)));
	major = ask_node(host, port, "SELECT (pg_catalog.current_setting('server_version_num')::pg_catalog.int4 / 10000)::pg_catalog.text", NULL);
	if (major != NULL && atoi(major) != kingres_shard_server_major())
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("node %s:%d runs PostgreSQL %s, this node runs PostgreSQL %d",
						host, port, major, kingres_shard_server_major())));
	if (primary != NULL)
	{
		sysid_sb = ask_node(host, port, "SELECT system_identifier::pg_catalog.text FROM pg_catalog.pg_control_system()", NULL);
		sysid_pr = ask_node(primary->host, primary->port,
							"SELECT system_identifier::pg_catalog.text FROM pg_catalog.pg_control_system()", NULL);
		if (sysid_sb != NULL && sysid_pr != NULL && strcmp(sysid_sb, sysid_pr) != 0)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("node %s:%d is not a copy of the primary of group %d (node %d)",
							host, port, group_id, primary->node_id),
					 errdetail("The system identifier of the node is %s, that of the primary %s.",
							   sysid_sb, sysid_pr)));
	}
}

/*
 * kingres_shard_require_pending_node_id
 *
 * kingres.pending_node.node_id was added to the 0.5 scripts in place (S12-06; 0.5 is not
 * released).  A database that installed 0.5 before the column existed has no update path to a
 * version that has it, and every statement that lists the column fails with "column does not
 * exist": say what to run instead.
 */
void
kingres_shard_require_pending_node_id(void)
{
	Oid			relid = kingres_shard_catalog_relid("pending_node");

	if (!OidIsValid(relid))
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("the table kingres.pending_node does not exist"),
				 errhint("Run ALTER EXTENSION kingres_shard UPDATE.")));
	if (get_attnum(relid, "node_id") == InvalidAttrNumber)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_COLUMN),
				 errmsg("the table kingres.pending_node has no column node_id"),
				 errdetail("This database installed an earlier build of kingres_shard 0.5, from before the column existed; ALTER EXTENSION UPDATE does nothing for the same version."),
				 errhint("On this node run: ALTER TABLE kingres.pending_node ADD COLUMN node_id pg_catalog.int4;  then repeat the call.")));
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
record_pending_node(const char *host, int port, int node_id)
{
	const KsCatalog *cat = kingres_shard_catalog();
	const KsNode *self = kingres_shard_node_by_id(cat->local_node_id);
	KsConn	   *kc;
	const char *params[3];
	char		portstr[16];
	char		idstr[16];
	PGresult   *res;

	kingres_shard_require_pending_node_id();

	if (self == NULL)
		return;
	kc = kingres_shard_get_conn(self->host, self->port, false, NULL);
	snprintf(portstr, sizeof(portstr), "%d", port);
	snprintf(idstr, sizeof(idstr), "%d", node_id);
	params[0] = host;
	params[1] = portstr;
	params[2] = node_id > 0 ? idstr : NULL;
	res = kingres_shard_exec_params(kc,
									 "INSERT INTO " KINGRES_SHARD_SCHEMA ".pending_node (host, port, created_at, node_id) "
									 "VALUES ($1::pg_catalog.text, $2::pg_catalog.int4, pg_catalog.clock_timestamp(), $3::pg_catalog.int4) "
									 "ON CONFLICT (host, port) DO UPDATE SET created_at = excluded.created_at, node_id = excluded.node_id",
									 3, NULL, params, true);
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
static Datum
kingres_add_node_body(PG_FUNCTION_ARGS)
{
	char	   *host;
	int			port;
	bool		group_given = !PG_ARGISNULL(2);
	int			group_id = group_given ? PG_GETARG_INT32(2) : 0;
	char	   *role;
	bool		is_primary;
	int			node_id;
	MemoryContext fcxt = CurrentMemoryContext;	/* the context of the call: what the staging returns must outlive the SPI connection of this function */
	int			staged_schemas = 0;
	bool		has_progress = false;
	List	   *late_scripts = NIL;
	int			nlate = 0;
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
	require_primaries_can_commit();

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

	if (!is_primary)
		check_standby_node(host, port, group_id);

	spi_run("SELECT coalesce(max(node_id), 0) + 1 FROM " KINGRES_SHARD_SCHEMA ".node",
			SPI_OK_SELECT, 0, NULL, NULL);
	node_id = spi_first_int(1);
	kingres_shard_conn_expect_node(host, port, node_id);

	/*
	 * Writers of the reference tables must neither miss the rows copied to the
	 * new node nor write to a list of nodes without it (review finding S2-07):
	 * take their lock on every existing node until this transaction ends.
	 */
	if (is_primary)
		kingres_shard_lock_all_reference_tables();

	/*
	 * Prepare the remote node before touching the local catalog, so that an
	 * unreachable or unsuitable node fails fast.
	 */
	if (is_primary)
	{
		record_pending_node(host, port, 0);
		staged_schemas = stage_schemas_on_node(host, port, fcxt, &late_scripts, &nlate, &has_progress);
		kingres_shard_fault_point("add_node_staged");
		kc = kingres_shard_get_conn(host, port, false, NULL);
		kingres_shard_begin_write_xact(kc);
		check_acl_roles_on_node(kc, host, port);
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
		/*
		 * The nodes must agree on the encoding, the default collation and its
		 * library version, and run the same version of the extension: shipped
		 * comparisons assume it (review finding S2-20).
		 */
		{
			static const char *const idq =
				"SELECT COALESCE(d.datcollate, '') OPERATOR(pg_catalog.||) '/' OPERATOR(pg_catalog.||) "
				"COALESCE(d.datctype, '') OPERATOR(pg_catalog.||) '/' OPERATOR(pg_catalog.||) "
				"d.datlocprovider::pg_catalog.text OPERATOR(pg_catalog.||) '/' OPERATOR(pg_catalog.||) "
				"COALESCE(d.datcollversion, '') "
				"FROM pg_catalog.pg_database d WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database()";
			static const char *const encq =
				"SELECT pg_catalog.pg_encoding_to_char(d.encoding) FROM pg_catalog.pg_database d "
				"WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database()";
			static const char *const verq =
				"SELECT COALESCE((SELECT e.extversion FROM pg_catalog.pg_extension e "
				"WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard'), '')";
			char	   *remote_id;
			char	   *local_id;
			char	   *remote_ver;
			char	   *local_ver;
			char	   *remote_enc;
			char	   *local_enc;

			res = kingres_shard_exec(kc, idq);
			remote_id = pstrdup(PQgetvalue(res, 0, 0));
			PQclear(res);
			res = kingres_shard_exec(kc, encq);
			remote_enc = pstrdup(PQgetvalue(res, 0, 0));
			PQclear(res);
			res = kingres_shard_exec(kc, verq);
			remote_ver = pstrdup(PQgetvalue(res, 0, 0));
			PQclear(res);
			spi_run(idq, SPI_OK_SELECT, 0, NULL, NULL);
			local_id = pstrdup(SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1));
			spi_run(encq, SPI_OK_SELECT, 0, NULL, NULL);
			local_enc = pstrdup(SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1));
			spi_run(verq, SPI_OK_SELECT, 0, NULL, NULL);
			local_ver = pstrdup(SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1));
			/* One PostgreSQL major per cluster (ADR 0023). */
			if (PQserverVersion(kc->conn) / 10000 != kingres_shard_server_major())
				ereport(ERROR,
						(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
						 errmsg("node %s:%d runs PostgreSQL %d, this node runs PostgreSQL %d",
								host, port, PQserverVersion(kc->conn) / 10000,
								kingres_shard_server_major()),
						 errdetail("All nodes of a cluster must run the same major version of PostgreSQL."),
						 errhint("Install the major version of this node on the new node.")));
			if (strcmp(remote_ver, local_ver) != 0)
				ereport(ERROR,
						(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
						 errmsg("node %s:%d has kingres_shard version \"%s\", this node has \"%s\"",
								host, port, remote_ver, local_ver),
						 errhint("Run ALTER EXTENSION kingres_shard UPDATE on the node, or DROP EXTENSION kingres_shard there, before add_node().")));
			if (strcmp(remote_id, local_id) != 0)
				ereport(ERROR,
						(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
						 errmsg("node %s:%d differs from this node in default collation or its library version",
								host, port),
						 errdetail("This node: %s; the new node: %s (datcollate, datctype, provider, collation version).",
								   local_id, remote_id),
						 errhint("Create the database of the new node with the same encoding and collation as this one.")));
			if (strcmp(remote_enc, local_enc) != 0)
				ereport(WARNING,
						(errmsg("node %s:%d has database encoding %s, this node has %s",
								host, port, remote_enc, local_enc),
						 errdetail("Statements are converted between the encodings, but comparisons of text outside ASCII can differ between nodes.")));
		}
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
		if (staged_schemas > 0 || has_progress)
		{
			/*
			 * Every schema that the batches made is on the node, or the node is not registered (a schema that went missing after its batch, or
			 * a progress that does not match what is there).
			 */
			res = kingres_shard_exec(kc,
									 "SELECT (SELECT count(*) FROM " KINGRES_SHARD_SCHEMA ".add_node_progress), "
									 "(SELECT count(*) FROM pg_catalog.pg_namespace n JOIN " KINGRES_SHARD_SCHEMA ".add_node_progress p ON p.nsp OPERATOR(pg_catalog.=) n.nspname)");
			if (PQntuples(res) != 1 || atoi(PQgetvalue(res, 0, 0)) != staged_schemas ||
				atoi(PQgetvalue(res, 0, 1)) != staged_schemas)
			{
				PQclear(res);
				ereport(ERROR,
						(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
						 errmsg("the new node %s:%d does not hold every distributed schema of the cluster", host, port),
						 errhint("Repeat add_node(): the batches that are complete are kept.")));
			}
			PQclear(res);
		}
		create_tables_on_node(kc, late_scripts);
		if (staged_schemas > 0 || has_progress)
			PQclear(kingres_shard_exec(kc, "DELETE FROM " KINGRES_SHARD_SCHEMA ".add_node_progress"));
		snprintf(idbuf, sizeof(idbuf), "%d", node_id);
		params[0] = idbuf;
		res = kingres_shard_exec_params(kc,
										 "SELECT " KINGRES_SHARD_SCHEMA ".internal_set_local_node($1::pg_catalog.int4)",
										 1, NULL, params, true);
		PQclear(res);
	}

	kingres_shard_sync_catalog_internal();
	if (is_primary)
		push_secret(kc);

	PG_RETURN_INT32(node_id);
}

Datum
kingres_add_node(PG_FUNCTION_ARGS)
{
	return kingres_shard_control_call(kingres_add_node_body, fcinfo);
}

/*
 * S13-06: a node that was let go without clearing its catalog still holds the cluster secret, and the nodes of
 * the cluster accept its sessions until the secret is rotated.
 */
#define REMOVED_NODE_HINT \
	"The removed node still holds its catalog and the cluster secret, so the nodes of the cluster still accept its sessions: " \
	"run kingres.rotate_secret() (it no longer includes this node) to shut it out, and reset the node when it is back."

/*
 * clear_removed_node
 *
 * The removed node's own catalog is cleared in the transaction of the removal.  A node that
 * accepted the session and then stops answering (or refuses a statement) is let go (S12-04): a
 * node that is stuck must be removable, as a node that cannot be connected to is.  Its session is
 * abandoned, so nothing of this call is left open on it, and the call says so with a WARNING that
 * names the node and carries the SQLSTATE the failure had (KS007 for a node that did not answer).
 * What can end this function: the statements succeed (the session stays in the transaction and is
 * prepared and committed with the others, so a node that stops answering later fails the call at
 * its PREPARE, with nothing changed); a statement fails (a WARNING, the session is closed, the
 * removal goes on without the node); the user cancels the call or the backend is told to stop
 * (the error goes on, as for every call).  The errors that are caught are raised by
 * node_conn.c on its own state only, which abandoning the session resets.
 */
static void
clear_removed_node(KsConn *kc, int node_id, const char *host, int port)
{
	MemoryContext oldcxt = CurrentMemoryContext;

	PG_TRY();
	{
		PGresult   *res;

		kingres_shard_begin_write_xact(kc);
		res = kingres_shard_exec(kc, "SELECT " KINGRES_SHARD_SCHEMA ".internal_reset_node()");
		PQclear(res);
	}
	PG_CATCH();
	{
		ErrorData  *edata;

		MemoryContextSwitchTo(oldcxt);
		edata = CopyErrorData();
		/*
		 * The user's cancel and the time of the call as a whole (KS015, S13-02) are not a node that stopped
		 * answering: the call goes on with them as an error, it does not let the node go.
		 */
		if (ERRCODE_TO_CATEGORY(edata->sqlerrcode) == ERRCODE_OPERATOR_INTERVENTION ||
			edata->sqlerrcode == ERRCODE_KINGRES_CALL_TIMEOUT)
			PG_RE_THROW();
		FlushErrorState();
		kingres_shard_conn_abandon(kc);
		ereport(WARNING,
				(errcode(edata->sqlerrcode),
				 errmsg("could not reach node %d (%s:%d); its catalog was not cleared", node_id, host, port),
				 errdetail_internal("%s", edata->message),
				 errhint("%s", REMOVED_NODE_HINT)));
		FreeErrorData(edata);
	}
	PG_END_TRY();
}

/*
 * kingres.remove_node(node_id int) returns void
 *
 * Unregister a node whose shard group owns no buckets.  The removed node's
 * own catalog is cleared when it can be reached; if it cannot, a warning says
 * so (a dead node must still be removable).
 */
static Datum
kingres_remove_node_body(PG_FUNCTION_ARGS)
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
	bool		node_is_primary;
	bool		isnull;
	int			nbuckets;

	kingres_shard_lock_catalog();
	kingres_shard_require_map_group();
	require_primaries_can_commit();

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
	node_is_primary = n->is_primary;
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
	/*
	 * A standby owns nothing: it can be removed whatever its group holds, and
	 * its removal changes nothing for routing.  A primary that still has
	 * standbys registered cannot go before them.
	 */
	if (node_is_primary)
	{
		spi_run("SELECT node_id FROM " KINGRES_SHARD_SCHEMA ".node "
				"WHERE group_id = $1 AND role = 'standby' LIMIT 1",
				SPI_OK_SELECT, 1, argtypes, values);
		if (SPI_processed > 0)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_IN_USE),
					 errmsg("cannot remove node %d: shard group %d still has the standby node %d",
							node_id, group_id, spi_first_int(0)),
					 errhint("Remove the standbys of the group first.")));
	}
	if (node_is_primary && nbuckets > 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_IN_USE),
				 errmsg("cannot remove node %d: its shard group %d owns %d buckets",
						node_id, group_id, nbuckets),
				 errhint("Move the buckets to other groups first with kingres.move_bucket(); kingres.rebalance_plan() lists the moves that empty a group when its should_have_shards is false.")));

	/* A group that takes part in an unfinished move of a bucket cannot go. */
	if (node_is_primary && OidIsValid(kingres_shard_catalog_relid("bucket_move")))
		spi_run("SELECT m.move_id FROM " KINGRES_SHARD_SCHEMA ".bucket_move m "
				"WHERE (m.source_group = $1 OR m.target_group = $1) "
				"AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching', 'cleaning') "
				"LIMIT 1",
				SPI_OK_SELECT, 1, argtypes, values);
	else
		spi_run("SELECT 1 WHERE false", SPI_OK_SELECT, 0, NULL, NULL);
	if (SPI_processed > 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_IN_USE),
				 errmsg("cannot remove node %d: shard group %d takes part in bucket move %d",
						node_id, group_id, spi_first_int(0)),
				 errhint("Wait for the move to finish (kingres.bucket_move), cancel it, or run kingres.cleanup_moved_buckets().")));

	if (reset_remote)
	{
		char	   *err = NULL;
		KsConn	   *kc;

		/*
		 * The node leaves the catalog in this transaction, and recovery visits only the nodes of
		 * the catalog: a call killed after its commit would leave the part prepared on the removed
		 * node for ever, with its locks (S11-05).  Recovery visits the nodes that are listed as
		 * pending too, so the address is written down first, outside this transaction.
		 */
		SPI_finish();
		record_pending_node(host, port, node_id);
		if (SPI_connect() != SPI_OK_CONNECT)
			elog(ERROR, "SPI_connect failed");
		kc = kingres_shard_get_conn(host, port, true, &err);

		if (kc == NULL)
			ereport(WARNING,
					(errmsg("could not reach node %d (%s:%d); its catalog was not cleared", node_id, host, port),
					 errdetail_internal("%s", err),
					 errhint("%s", REMOVED_NODE_HINT)));
		else
			clear_removed_node(kc, node_id, host, port);
	}

	argtypes[0] = INT4OID;
	values[0] = Int32GetDatum(node_id);
	spi_run("DELETE FROM " KINGRES_SHARD_SCHEMA ".node WHERE node_id = $1",
			SPI_OK_DELETE, 1, argtypes, values);
	SPI_finish();

	kingres_shard_sync_catalog_internal();
	PG_RETURN_VOID();
}

Datum
kingres_remove_node(PG_FUNCTION_ARGS)
{
	return kingres_shard_control_call(kingres_remove_node_body, fcinfo);
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
	Oid			mvrelid;

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

	/*
	 * A catalog change of the cluster (a switch of a bucket move, for one) may be
	 * in flight on this node: its transaction is prepared here, and the map group
	 * primary has committed it, which is why this node's map is found stale.  It
	 * holds ROW EXCLUSIVE on the version table until it is over; wait for it, so
	 * that the document applied below is not written over a change that is just
	 * committing (the writes would wait for its rows, and then run against the
	 * rows it left: a foreign key violation or a deadlock where the caller expects
	 * a retry).  The lock is taken before the first statement of the document runs, so
	 * that every statement of it sees the committed change.  Reloaders serialize on
	 * the same lock; it is released when the document is applied.
	 */
	mvrelid = kingres_shard_catalog_relid("map_version");
	if (OidIsValid(mvrelid))
		LockRelationOid(mvrelid, ShareRowExclusiveLock);

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
	if (OidIsValid(mvrelid))
		UnlockRelationOid(mvrelid, ShareRowExclusiveLock);

	kingres_shard_catalog_invalidate();
	return true;
}
