/*-------------------------------------------------------------------------
 *
 * recovery.c
 *	  kingres.recover_prepared_transactions() and the background workers that
 *	  call it (design/shard-phase2a.md, section 1, "Recovery").
 *
 * A node resolves the prepared transactions it originated.  The gid of a
 * prepared transaction is kingres_<origin node>_<origin pid>_<full xid>_<target
 * node>, so each node finds its own on every active primary (itself included)
 * in pg_prepared_xacts, and decides each one:
 *
 *	  - the origin's transaction is still in progress: leave it, it is between
 *		prepare and commit;
 *	  - a row for the gid exists in the local kingres.dtx_log: the local
 *		transaction committed, so COMMIT PREPARED;
 *	  - otherwise the origin never committed (the row would be durable if it
 *		had), so ROLLBACK PREPARED.
 *
 * The order of the two tests matters: the in-progress test comes first and the
 * dtx_log lookup after it, with a fresh snapshot.  If the origin commits
 * between the two, the second one sees the row; the opposite order could miss
 * the row of a transaction that is no longer in progress and roll back a
 * committed one.  For the same reason a dtx_log row is deleted only when it is
 * older than the moment the prepared transactions of its node were listed (a
 * transaction that prepared after the listing would otherwise lose its
 * decision while still prepared).
 *
 * Transaction ids are compared as 64-bit full transaction ids and wraparound
 * cannot confuse them: an id more than 2^31 behind the next one cannot be in
 * progress, and within that window the 32-bit id identifies one transaction.
 *
 * The launcher is a background worker registered at library load.  Every
 * kingres_shard.recovery_interval seconds it starts, one after the other, a
 * short-lived worker for each database that accepts connections (the pattern
 * of src/backend/replication/logical/launcher.c and contrib/pg_prewarm/
 * autoprewarm.c); the worker does nothing unless the extension is installed
 * and initialized there, and nothing on a standby.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/recovery.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/heapam.h"
#include "access/htup_details.h"
#include "access/table.h"
#include "access/tableam.h"
#include "access/transam.h"
#include "access/xact.h"
#include "access/xlog.h"
#include "catalog/pg_database.h"
#include "catalog/pg_type_d.h"
#include "commands/extension.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "libpq/pqsignal.h"
#include "miscadmin.h"
#include "pgstat.h"
#include "postmaster/bgworker.h"
#include "postmaster/interrupt.h"
#include "storage/ipc.h"
#include "storage/latch.h"
#include "storage/procarray.h"
#include "tcop/tcopprot.h"
#include "utils/array.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/memutils.h"
#include "utils/snapmgr.h"
#include "utils/timestamp.h"
#include "utils/wait_classes.h"

#include "catalog.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "recovery.h"

PG_FUNCTION_INFO_V1(kingres_recover_prepared_transactions);

/* A node to look at. */
typedef struct RecNode
{
	int			node_id;
	char	   *host;
	int			port;
} RecNode;

/*
 * Is the transaction with this full id possibly still running on this node?
 */
static bool
xid_possibly_running(uint64 fxid, int pid)
{
	uint64		next = U64FromFullTransactionId(ReadNextFullTransactionId());

	/*
	 * A transaction that is running has an id below the next one, so an id at
	 * or above it belongs to a backend of an earlier life of this node (the
	 * node crashed before the id reached the WAL): it is not running.  An id
	 * that a new transaction has reused since is reported as running, which
	 * only delays the resolution.
	 */
	if (fxid >= next)
		return false;
	if (next - fxid >= ((uint64) 1 << 31))
		return false;
	{
		TransactionId xid = XidFromFullTransactionId(FullTransactionIdFromU64(fxid));

		if (!TransactionIdIsInProgress(xid))
			return false;
		/*
		 * After a crash the id may have been given again to another backend (it
		 * never reached the WAL): the transaction of the gid runs only if the
		 * backend that has the id is the one that made the gid.
		 */
		return BackendXidGetPid(xid) == pid;
	}
}

/*
 * Parse kingres_<origin>_<pid>_<full xid>_<target>[_<n>]; false when the gid is
 * not one of ours.
 */
static bool
parse_gid(const char *gid, int *origin, int *pid, uint64 *fxid)
{
	char	   *copy = pstrdup(gid);
	char	   *tok[8];
	int			n = 0;
	char	   *p = copy;
	bool		ok;

	while (n < 8)
	{
		tok[n++] = p;
		p = strchr(p, '_');
		if (p == NULL)
			break;
		*p++ = '\0';
	}
	/*
	 * kingres_<origin>_<pid>_<full xid>_<target>[_<n>], every field a number:
	 * a prepared transaction of a user that merely starts with "kingres_" is not
	 * ours and is never touched (review finding S2-20).
	 */
	ok = ((n == 5 || n == 6) && strcmp(tok[0], "kingres") == 0);
	{
		int			t;

		for (t = 1; ok && t < n; t++)
		{
			const char *c;

			if (tok[t][0] == '\0')
				ok = false;
			for (c = tok[t]; ok && *c != '\0'; c++)
				if (*c < '0' || *c > '9')
					ok = false;
		}
	}
	if (ok)
	{
		*origin = atoi(tok[1]);
		*pid = atoi(tok[2]);
		*fxid = strtoull(tok[3], NULL, 10);
	}
	pfree(copy);
	return ok;
}

/*
 * Does the local dtx_log hold a row for the gid?
 *
 * The caller has already established that the origin transaction is over (see
 * origin_decision), and the row is looked up with a snapshot taken now, after
 * that fact, never with the snapshot that was active when recovery began: a
 * read-only SPI call would use that older one, which cannot see a decision
 * that became visible in between (review finding S2-01).
 */
static bool
decision_logged(const char *gid)
{
	Oid			argtypes[1] = {TEXTOID};
	Datum		values[1];
	bool		found;

	values[0] = CStringGetTextDatum(gid);
	PushActiveSnapshot(GetLatestSnapshot());
	if (SPI_execute_with_args("SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".dtx_log WHERE gid OPERATOR(pg_catalog.=) $1",
							  1, argtypes, values, NULL, true, 1) != SPI_OK_SELECT)
		elog(ERROR, "could not read kingres.dtx_log");
	PopActiveSnapshot();
	found = (SPI_processed > 0);
	return found;
}

/*
 * origin_decision
 *
 * THE INVARIANT OF RECOVERY.  A prepared transaction may be rolled back only
 * if (a) the origin transaction is no longer in progress, established first,
 * and then (b) a decision row looked up with a snapshot taken after (a) is
 * absent.  The origin makes the row visible by committing; the commit removes
 * its transaction id from the proc array only after the commit record is
 * written, so a snapshot taken after (a) saw "not in progress" sees the row of
 * a transaction that committed.  A snapshot taken before (a) may not, which is
 * why the order is the code's structure here and not two calls a caller could
 * swap.
 *
 * Returns false while the origin transaction may still be running (leave the
 * prepared transaction alone); otherwise true and *commit says what to do.
 */
static bool
origin_decision(const char *gid, uint64 fxid, int pid, bool *commit)
{
	/* (a) */
	if (xid_possibly_running(fxid, pid))
		return false;
	/* (b), strictly after (a) */
	*commit = decision_logged(gid);
	return true;
}

/* Delete the rows of a node that are no longer prepared there. */
static void
purge_log(int node_id, TimestampTz listed_at, List *still_prepared, List *listed)
{
	Oid			argtypes[3] = {INT4OID, TIMESTAMPTZOID, TEXTARRAYOID};
	Datum		values[3];
	Datum	   *elems;
	int			n = list_length(still_prepared);
	int			i = 0;
	ListCell   *lc;

	elems = palloc(sizeof(Datum) * Max(n, 1));
	foreach(lc, still_prepared)
		elems[i++] = CStringGetTextDatum((char *) lfirst(lc));
	values[0] = Int32GetDatum(node_id);
	values[1] = TimestampTzGetDatum(listed_at);
	values[2] = PointerGetDatum(construct_array_builtin(elems, n, TEXTOID));
	if (SPI_execute_with_args("DELETE FROM " KINGRES_SHARD_SCHEMA ".dtx_log "
							  "WHERE node_id OPERATOR(pg_catalog.=) $1 "
							  "AND prepared_at OPERATOR(pg_catalog.<) $2 "
							  "AND NOT (gid OPERATOR(pg_catalog.=) ANY ($3))",
							  3, argtypes, values, NULL, false, 0) != SPI_OK_DELETE)
		elog(ERROR, "could not clean kingres.dtx_log");

	/*
	 * A decision made when the target node was not registered yet (node_id 0,
	 * add_node) is removed when the transaction it names is gone from the node
	 * that is now registered (S2-20).
	 */
	if (node_id != 0)
	{
		Datum	   *lelems;
		int			ln = list_length(listed);
		int			j = 0;
		Oid			a2[2] = {TEXTARRAYOID, TEXTARRAYOID};
		Datum		v2[2];

		lelems = palloc(sizeof(Datum) * Max(ln, 1));
		foreach(lc, listed)
			lelems[j++] = CStringGetTextDatum((char *) lfirst(lc));
		v2[0] = PointerGetDatum(construct_array_builtin(lelems, ln, TEXTOID));
		v2[1] = values[2];
		if (SPI_execute_with_args("DELETE FROM " KINGRES_SHARD_SCHEMA ".dtx_log "
								  "WHERE node_id OPERATOR(pg_catalog.=) 0 "
								  "AND gid OPERATOR(pg_catalog.=) ANY ($1) "
								  "AND NOT (gid OPERATOR(pg_catalog.=) ANY ($2))",
								  2, a2, v2, NULL, false, 0) != SPI_OK_DELETE)
			elog(ERROR, "could not clean kingres.dtx_log");
	}
}

/*
 * kingres_shard_recover_prepared
 *
 * See the file header.  Returns the number of prepared transactions committed
 * or rolled back.
 */
int
kingres_shard_recover_prepared(void)
{
	MemoryContext callercxt = CurrentMemoryContext;
	const KsCatalog *cat = kingres_shard_catalog();
	List	   *nodes = NIL;
	ListCell   *lc;
	int			origin;
	int			resolved = 0;

	if (!cat->present || cat->local_node_id == 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("the cluster is not initialized on this node")));
	if (RecoveryInProgress())
		ereport(ERROR,
				(errcode(ERRCODE_READ_ONLY_SQL_TRANSACTION),
				 errmsg("recovery of prepared transactions cannot run on a standby")));
	if (IsolationUsesXactSnapshot())
		ereport(ERROR,
				(errcode(ERRCODE_ACTIVE_SQL_TRANSACTION),
				 errmsg("recover_prepared_transactions() must run in a READ COMMITTED transaction")));

	origin = cat->local_node_id;
	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		RecNode    *r;

		if (!n->is_active || !n->is_primary)
			continue;
		r = MemoryContextAlloc(callercxt, sizeof(RecNode));
		r->node_id = n->node_id;
		r->host = MemoryContextStrdup(callercxt, n->host);
		r->port = n->port;
		nodes = lappend(nodes, r);
	}

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	/*
	 * Addresses on which add_node() prepared a transaction before it registered
	 * them: if the origin died, the registration was rolled back with the rest
	 * and only this table remembers the node.
	 */
	if (SPI_execute("SELECT p.host, p.port FROM " KINGRES_SHARD_SCHEMA ".pending_node p "
					"WHERE NOT EXISTS (SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".node n "
					"WHERE n.host OPERATOR(pg_catalog.=) p.host AND n.port OPERATOR(pg_catalog.=) p.port)",
					true, 0) != SPI_OK_SELECT)
		elog(ERROR, "could not read kingres.pending_node");
	{
		uint64		i;

		for (i = 0; i < SPI_processed; i++)
		{
			bool		isnull;
			RecNode    *r = MemoryContextAlloc(callercxt, sizeof(RecNode));

			r->node_id = 0;
			r->host = MemoryContextStrdup(callercxt,
										  SPI_getvalue(SPI_tuptable->vals[i],
													   SPI_tuptable->tupdesc, 1));
			r->port = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[i],
												  SPI_tuptable->tupdesc, 2, &isnull));
			nodes = lappend(nodes, r);
		}
	}

	foreach(lc, nodes)
	{
		RecNode    *r = (RecNode *) lfirst(lc);
		char	   *err = NULL;
		KsConn	   *kc;
		PGresult   *res;
		const char *params[1];
		TimestampTz listed_at;
		List	   *gids = NIL;
		List	   *still_prepared = NIL;
		ListCell   *lc2;
		int			i;
		int			ntup;

		kc = kingres_shard_get_conn(r->host, r->port, true, &err);
		if (kc == NULL)
		{
			ereport(LOG,
					(errmsg("kingres_shard recovery: node %d (%s:%d) is not reachable, skipped",
							r->node_id, r->host, r->port),
					 errdetail_internal("%s", err ? err : "")));
			continue;
		}
		if (kc->xact_depth > 0)
			ereport(ERROR,
					(errcode(ERRCODE_ACTIVE_SQL_TRANSACTION),
					 errmsg("recover_prepared_transactions() cannot run in a transaction that has operated on remote nodes")));

		/* In the worker (its connections end with it), not in the session of a user whose connection is reused. */
		if (MyBackendType == B_BG_WORKER)
		{
			char	   *lim = psprintf("SET statement_timeout = %d; SET lock_timeout = %d",
									  kingres_shard_node_timeout * 1000, kingres_shard_node_timeout * 1000);

			PQclear(kingres_shard_exec_params(kc, lim, 0, NULL, NULL, false));
		}
		listed_at = GetCurrentTimestamp();
		params[0] = psprintf("kingres\\_%d\\_%%", origin);
		res = kingres_shard_exec_params(kc,
										"SELECT gid FROM pg_catalog.pg_prepared_xacts "
										"WHERE database OPERATOR(pg_catalog.=) pg_catalog.current_database() "
										"AND gid LIKE $1 ORDER BY prepared",
										1, NULL, params, false);
		if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK)
		{
			ereport(LOG,
					(errmsg("kingres_shard recovery: could not list the prepared transactions of node %d (%s:%d)",
							r->node_id, r->host, r->port),
					 errdetail_internal("%s", kc->conn ? pchomp(PQerrorMessage(kc->conn)) : "")));
			PQclear(res);
			if (kc->conn == NULL || PQstatus(kc->conn) != CONNECTION_OK)
				kc->have_error = true;
			continue;
		}
		ntup = PQntuples(res);
		for (i = 0; i < ntup; i++)
			gids = lappend(gids, pstrdup(PQgetvalue(res, i, 0)));
		PQclear(res);

		foreach(lc2, gids)
		{
			char	   *gid = (char *) lfirst(lc2);
			int			g_origin;
			int			g_pid;
			uint64		fxid;
			bool		commit;
			char	   *sql;

			if (!parse_gid(gid, &g_origin, &g_pid, &fxid) || g_origin != origin)
				continue;

			if (!origin_decision(gid, fxid, g_pid, &commit))
			{
				still_prepared = lappend(still_prepared, gid);
				continue;
			}
			sql = psprintf("%s PREPARED %s", commit ? "COMMIT" : "ROLLBACK",
						   quote_literal_cstr(gid));
			res = kingres_shard_exec_params(kc, sql, 0, NULL, NULL, false);
			if (res != NULL && PQresultStatus(res) == PGRES_COMMAND_OK)
			{
				resolved++;
				ereport(LOG,
						(errmsg("kingres_shard recovery: %s prepared transaction \"%s\" on node %d",
								commit ? "committed" : "rolled back", gid, r->node_id)));
			}
			else if (res != NULL &&
					 kingres_shard_result_sqlstate(res) == ERRCODE_UNDEFINED_OBJECT)
			{
				/* Resolved by someone else in the meantime. */
			}
			else
			{
				ereport(WARNING,
						(errmsg("kingres_shard recovery: could not %s prepared transaction \"%s\" on node %d",
								commit ? "commit" : "roll back", gid, r->node_id),
						 errdetail_internal("%s", kc->conn ? pchomp(PQerrorMessage(kc->conn)) : "")));
				still_prepared = lappend(still_prepared, gid);
			}
			PQclear(res);
		}

		purge_log(r->node_id, listed_at, still_prepared, gids);
	}

	/* Rows of nodes that left the cluster have nothing left to resolve. */
	if (SPI_execute("DELETE FROM " KINGRES_SHARD_SCHEMA ".dtx_log d "
					"WHERE d.node_id OPERATOR(pg_catalog.>) 0 "
					"AND NOT EXISTS (SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".node n "
					"WHERE n.node_id OPERATOR(pg_catalog.=) d.node_id)",
					false, 0) != SPI_OK_DELETE)
		elog(ERROR, "could not clean kingres.dtx_log");

	/*
	 * A pending address is forgotten once the node is registered (the
	 * transaction committed) or when it is so old that no add_node() can still
	 * be running.
	 */
	if (SPI_execute("DELETE FROM " KINGRES_SHARD_SCHEMA ".pending_node p "
					"WHERE p.created_at OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) pg_catalog.make_interval(hours => 1)) "
					"OR EXISTS (SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".node n "
					"WHERE n.host OPERATOR(pg_catalog.=) p.host AND n.port OPERATOR(pg_catalog.=) p.port)",
					false, 0) != SPI_OK_DELETE)
		elog(ERROR, "could not clean kingres.pending_node");

	SPI_finish();
	return resolved;
}

/*
 * kingres.recover_prepared_transactions() returns int
 */
Datum
kingres_recover_prepared_transactions(PG_FUNCTION_ARGS)
{
	PG_RETURN_INT32(kingres_shard_recover_prepared());
}

/* ---------------------------------------------------------------------------
 * Background workers
 * ---------------------------------------------------------------------------
 */

/*
 * kingres_shard_recovery_init
 *
 * Register the launcher.  Called from _PG_init (the library is preloaded).
 */
void
kingres_shard_recovery_init(void)
{
	BackgroundWorker bgw;

	memset(&bgw, 0, sizeof(bgw));
	bgw.bgw_flags = BGWORKER_SHMEM_ACCESS | BGWORKER_BACKEND_DATABASE_CONNECTION;
	bgw.bgw_start_time = BgWorkerStart_RecoveryFinished;
	bgw.bgw_restart_time = 5;
	snprintf(bgw.bgw_library_name, BGW_MAXLEN, "kingres_shard");
	snprintf(bgw.bgw_function_name, BGW_MAXLEN, "kingres_shard_recovery_launcher_main");
	snprintf(bgw.bgw_name, BGW_MAXLEN, "kingres_shard recovery launcher");
	snprintf(bgw.bgw_type, BGW_MAXLEN, "kingres_shard recovery launcher");
	RegisterBackgroundWorker(&bgw);
}

/* The databases that accept connections: (oid, name) pairs, in cxt. */
static List *
list_databases(MemoryContext cxt)
{
	List	   *result = NIL;
	Relation	rel;
	TableScanDesc scan;
	HeapTuple	tup;

	StartTransactionCommand();
	(void) GetTransactionSnapshot();
	rel = table_open(DatabaseRelationId, AccessShareLock);
	scan = table_beginscan_catalog(rel, 0, NULL);
	while ((tup = heap_getnext(scan, ForwardScanDirection)) != NULL)
	{
		Form_pg_database db = (Form_pg_database) GETSTRUCT(tup);
		MemoryContext old;

		if (!db->datallowconn || db->datistemplate)
			continue;
		old = MemoryContextSwitchTo(cxt);
		result = lappend_oid(result, db->oid);
		MemoryContextSwitchTo(old);
	}
	table_endscan(scan);
	table_close(rel, AccessShareLock);
	CommitTransactionCommand();
	return result;
}

/* One pass: a worker per database, one at a time. */
static void
launch_workers(void)
{
	MemoryContext cxt = AllocSetContextCreate(CurrentMemoryContext,
											  "kingres_shard recovery launcher pass",
											  ALLOCSET_DEFAULT_SIZES);
	List	   *dbs = list_databases(cxt);
	ListCell   *lc;

	foreach(lc, dbs)
	{
		BackgroundWorker bgw;
		BackgroundWorkerHandle *handle;
		pid_t		pid;

		memset(&bgw, 0, sizeof(bgw));
		bgw.bgw_flags = BGWORKER_SHMEM_ACCESS | BGWORKER_BACKEND_DATABASE_CONNECTION;
		bgw.bgw_start_time = BgWorkerStart_RecoveryFinished;
		bgw.bgw_restart_time = BGW_NEVER_RESTART;
		snprintf(bgw.bgw_library_name, BGW_MAXLEN, "kingres_shard");
		snprintf(bgw.bgw_function_name, BGW_MAXLEN, "kingres_shard_recovery_worker_main");
		snprintf(bgw.bgw_name, BGW_MAXLEN, "kingres_shard recovery worker");
		snprintf(bgw.bgw_type, BGW_MAXLEN, "kingres_shard recovery worker");
		bgw.bgw_main_arg = ObjectIdGetDatum(lfirst_oid(lc));
		bgw.bgw_notify_pid = MyProcPid;

		if (!RegisterDynamicBackgroundWorker(&bgw, &handle))
		{
			ereport(LOG,
					(errmsg("kingres_shard recovery: no background worker slot is free, will retry")));
			break;
		}
		if (WaitForBackgroundWorkerStartup(handle, &pid) == BGWH_STARTED)
		{
			/*
			 * One worker that does not finish (a node that stopped answering)
			 * must not stall the recovery of the other databases: after a
			 * deadline the worker is terminated and the pass goes on.
			 */
			TimestampTz deadline = TimestampTzPlusMilliseconds(GetCurrentTimestamp(),
															   (int64) 5 * kingres_shard_node_timeout * 1000);

			for (;;)
			{
				pid_t		wpid;

				if (GetBackgroundWorkerPid(handle, &wpid) == BGWH_STOPPED)
					break;
				if (GetCurrentTimestamp() >= deadline)
				{
					ereport(LOG,
							(errmsg("kingres_shard recovery: a worker did not finish in time and is terminated")));
					TerminateBackgroundWorker(handle);
					(void) WaitForBackgroundWorkerShutdown(handle);
					break;
				}
				(void) WaitLatch(MyLatch, WL_LATCH_SET | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH,
								 500, PG_WAIT_EXTENSION);
				ResetLatch(MyLatch);
				CHECK_FOR_INTERRUPTS();
			}
		}
	}
	MemoryContextDelete(cxt);
}

void
kingres_shard_recovery_launcher_main(Datum main_arg)
{
	pqsignal(SIGHUP, SignalHandlerForConfigReload);
	pqsignal(SIGTERM, die);
	BackgroundWorkerUnblockSignals();
	BackgroundWorkerInitializeConnection(NULL, NULL, 0);

	for (;;)
	{
		long		wait_ms;

		if (kingres_shard_recovery_interval > 0)
			wait_ms = (long) kingres_shard_recovery_interval * 1000L;
		else
			wait_ms = 3600 * 1000L;	/* disabled: sleep until a reload */

		(void) WaitLatch(MyLatch,
						 WL_LATCH_SET | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH,
						 wait_ms, PG_WAIT_EXTENSION);
		ResetLatch(MyLatch);
		CHECK_FOR_INTERRUPTS();

		if (ConfigReloadPending)
		{
			ConfigReloadPending = false;
			ProcessConfigFile(PGC_SIGHUP);
		}
		if (kingres_shard_recovery_interval > 0)
			launch_workers();
	}
}

/*
 * Settings of the database or of the role of the worker (ALTER DATABASE ...
 * SET, ALTER ROLE ... SET) must not be able to make a pass fail, stall or
 * behave differently (review finding S2-11): every setting that can is fixed
 * here, at a level (PGC_S_OVERRIDE) that nothing stored can beat.
 */
static void
fix_worker_settings(void)
{
	static const char *const settings[][2] = {
		{"default_transaction_isolation", "read committed"},
		{"default_transaction_read_only", "off"},
		{"default_transaction_deferrable", "off"},
		{"statement_timeout", "0"},
		{"lock_timeout", "0"},
		{"idle_in_transaction_session_timeout", "0"},
		{"idle_session_timeout", "0"},
		{"transaction_timeout", "0"},
		{"session_replication_role", "origin"},
		{"role", "none"},
		{"check_function_bodies", "off"},
		{"synchronous_commit", "on"},
		{"kingres_shard.enable_routing", "on"},
		{"kingres_shard.enable_distributed_scan", "off"},
		{"search_path", "pg_catalog, kingres, pg_temp"},
		{NULL, NULL}
	};
	int			i;

	for (i = 0; settings[i][0] != NULL; i++)
		(void) set_config_option(settings[i][0], settings[i][1],
								 PGC_SUSET, PGC_S_OVERRIDE, GUC_ACTION_SET, true, 0, false);
}

void
kingres_shard_recovery_worker_main(Datum main_arg)
{
	Oid			dboid = DatumGetObjectId(main_arg);

	pqsignal(SIGTERM, die);
	BackgroundWorkerUnblockSignals();
	BackgroundWorkerInitializeConnectionByOid(dboid, InvalidOid, 0);

	/* Before the transaction starts: the isolation level and the access mode are read at its start. */
	fix_worker_settings();
	StartTransactionCommand();
	PushActiveSnapshot(GetTransactionSnapshot());
	if (!RecoveryInProgress() &&
		OidIsValid(get_extension_oid("kingres_shard", true)))
	{
		const KsCatalog *cat = kingres_shard_catalog();

		if (cat->present && cat->local_node_id != 0 &&
			OidIsValid(kingres_shard_catalog_relid("dtx_log")))
			(void) kingres_shard_recover_prepared();
	}
	PopActiveSnapshot();
	CommitTransactionCommand();
	proc_exit(0);
}
