/*-------------------------------------------------------------------------
 *
 * bucket_move.c
 *	  The C primitives of moving a bucket from one shard group to another
 *	  (design/shard-phase4.md).  The orchestration itself (the phases, the
 *	  waits, the undo, the resumption) is PL/pgSQL in the install script; what
 *	  needs C is here:
 *
 *	  kingres.internal_node_exec(node_id, sql, timeout_ms) returns jsonb
 *		Run SQL on a node in a session of its own, outside every transaction of
 *		this backend, and return the rows as a JSON array of arrays of text.  A
 *		move has to commit its bookkeeping at once (so that other sessions and
 *		a restart see it), create slots and subscriptions (which cannot run in
 *		a transaction block) and wait for other nodes; all of that goes through
 *		this function.  Every wait is bounded and can be interrupted: the
 *		remote statement is cancelled and the session closed.  The session is
 *		a node-to-node session (node_conn.c): same options and token.
 *
 *	  kingres.internal_switch_bucket(move_id, switch_timeout_ms, cleanup_delay_s)
 *		The switch (design, "switching"): in one distributed two-phase
 *		transaction, lock the bucket's tables on the source, prove that the
 *		copy has replayed everything, attach the copies on the target, detach
 *		the partitions on the source, change the bucket map on every node.  It
 *		runs in a session of its own (the caller reaches it through
 *		internal_node_exec on this node), so that the ordinary commit protocol
 *		of node_conn.c (prepare everywhere, decision in kingres.dtx_log, commit
 *		prepared, recovery) decides the outcome.
 *
 *	  kingres.internal_attach_bucket / internal_detach_bucket
 *		The ATTACH and DETACH of one bucket partition, run on the node that
 *		attaches or detaches it.  The ProcessUtility hook refuses those
 *		statements for everyone else; these functions are the one exception
 *		and wrap exactly that statement in a scope that tells the hook (and
 *		the planner, for the validation join of a foreign key) to leave it
 *		alone.
 *
 *	  kingres.internal_subscription_conninfo(host, port)
 *		The connection string of the subscription of a move, built on the
 *		target from its own kingres_shard.node_conninfo, so that key paths
 *		never leave that node.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/bucket_move.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/heapam.h"
#include "access/htup_details.h"
#include "access/table.h"
#include "access/tableam.h"
#include "access/xact.h"
#include "catalog/pg_type_d.h"
#include "commands/dbcommands.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "lib/stringinfo.h"
#include "libpq-fe.h"
#include "miscadmin.h"
#include "storage/latch.h"
#include "storage/lock.h"
#include "utils/builtins.h"
#include "utils/json.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/relcache.h"
#include "utils/snapmgr.h"
#include "utils/timestamp.h"
#include "utils/wait_classes.h"

#include "bucket_move.h"
#include "catalog.h"
#include "kingres_shard.h"
#include "node_admin.h"
#include "node_conn.h"
#include "utility.h"

PG_FUNCTION_INFO_V1(kingres_internal_node_exec);
PG_FUNCTION_INFO_V1(kingres_internal_switch_bucket);
PG_FUNCTION_INFO_V1(kingres_internal_attach_bucket);
PG_FUNCTION_INFO_V1(kingres_internal_detach_bucket);
PG_FUNCTION_INFO_V1(kingres_internal_subscription_conninfo);

/* ---------------------------------------------------------------------------
 * Control sessions
 * ---------------------------------------------------------------------------
 */

#define KS_MAX_CTL 16
#define KS_CTL_FAULT_LEN 512

typedef struct CtlConn
{
	char		host[KS_CONN_HOST_LEN];
	int			port;
	char		user[NAMEDATALEN];
	PGconn	   *conn;
	char		fault[KS_CTL_FAULT_LEN];	/* kingres_shard.fault_injection last sent */
} CtlConn;

static CtlConn ctl_cache[KS_MAX_CTL];
static int	nctl = 0;

static void
ctl_discard(CtlConn *cc)
{
	if (cc->conn != NULL)
		PQfinish(cc->conn);
	cc->conn = NULL;
	cc->fault[0] = '\0';
}

/* Ask the node to stop the running statement; the answer is not awaited. */
static void
ctl_cancel(CtlConn *cc)
{
	PGcancel   *cancel;
	char		errbuf[256];

	if (cc->conn == NULL)
		return;
	cancel = PQgetCancel(cc->conn);
	if (cancel != NULL)
	{
		(void) PQcancel(cancel, errbuf, sizeof(errbuf));
		PQfreeCancel(cancel);
	}
}

/*
 * The cached control session to host:port for the current user, connected (or
 * reconnected: a session that is closed, or left inside a transaction, is
 * replaced).
 */
static CtlConn *
ctl_get(const char *host, int port)
{
	const char *user = GetUserNameFromId(GetUserId(), false);
	CtlConn    *cc = NULL;
	char	   *err = NULL;
	int			i;

	for (i = 0; i < nctl; i++)
	{
		if (ctl_cache[i].port == port &&
			strcmp(ctl_cache[i].host, host) == 0 &&
			strcmp(ctl_cache[i].user, user) == 0)
		{
			cc = &ctl_cache[i];
			break;
		}
	}
	if (cc == NULL)
	{
		if (nctl == KS_MAX_CTL)
		{
			cc = &ctl_cache[0];
			ctl_discard(cc);
		}
		else
			cc = &ctl_cache[nctl++];
		memset(cc, 0, sizeof(*cc));
		strlcpy(cc->host, host, sizeof(cc->host));
		cc->port = port;
		strlcpy(cc->user, user, sizeof(cc->user));
	}

	if (cc->conn != NULL)
	{
		/* A restarted node closed the session; reading detects it. */
		(void) PQconsumeInput(cc->conn);
		if (PQstatus(cc->conn) == CONNECTION_OK)
			(void) PQconsumeInput(cc->conn);
		if (PQstatus(cc->conn) != CONNECTION_OK ||
			PQtransactionStatus(cc->conn) != PQTRANS_IDLE)
			ctl_discard(cc);
	}
	if (cc->conn == NULL)
	{
		cc->conn = kingres_shard_connect_control(host, port, &err);
		cc->fault[0] = '\0';
		if (cc->conn == NULL)
			ereport(ERROR,
					(errcode(ERRCODE_SQLCLIENT_UNABLE_TO_ESTABLISH_SQLCONNECTION),
					 errmsg("could not connect to node %s:%d", host, port),
					 errdetail_internal("%s", err)));
	}
	return cc;
}

/*
 * Run sql on the control session and return its last result (the caller
 * PQclears it, and looks at its status).  Waits at most timeout_ms, and can be
 * interrupted: in both cases the running statement is cancelled and the
 * session is closed.
 */
static PGresult *
ctl_run(CtlConn *cc, const char *sql, int timeout_ms)
{
	TimestampTz deadline = TimestampTzPlusMilliseconds(GetCurrentTimestamp(), timeout_ms);
	PGresult *volatile last = NULL;
	volatile bool ok = false;

	if (!PQsendQuery(cc->conn, sql))
	{
		char	   *msg = pchomp(PQerrorMessage(cc->conn));

		ctl_discard(cc);
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not send a command to node %s:%d", cc->host, cc->port),
				 errdetail_internal("%s", msg)));
	}

	PG_TRY();
	{
		for (;;)
		{
			PGresult   *res;

			while (PQisBusy(cc->conn))
			{
				long		secs;
				int			usecs;
				long		ms;
				int			rc;

				TimestampDifference(GetCurrentTimestamp(), deadline, &secs, &usecs);
				ms = secs * 1000L + usecs / 1000;
				if (GetCurrentTimestamp() >= deadline || ms <= 0)
					ereport(ERROR,
							(errcode(ERRCODE_QUERY_CANCELED),
							 errmsg("timed out after %d ms waiting for node %s:%d",
									timeout_ms, cc->host, cc->port)));
				rc = WaitLatchOrSocket(MyLatch,
									   WL_LATCH_SET | WL_SOCKET_READABLE | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH,
									   PQsocket(cc->conn), Min(ms, 1000L), PG_WAIT_EXTENSION);
				if (rc & WL_LATCH_SET)
				{
					ResetLatch(MyLatch);
					CHECK_FOR_INTERRUPTS();
				}
				if (!PQconsumeInput(cc->conn))
					ereport(ERROR,
							(errcode(ERRCODE_CONNECTION_FAILURE),
							 errmsg("lost the connection to node %s:%d", cc->host, cc->port),
							 errdetail_internal("%s", pchomp(PQerrorMessage(cc->conn)))));
			}
			res = PQgetResult(cc->conn);
			if (res == NULL)
				break;
			PQclear(last);
			last = res;
			if (PQresultStatus(res) == PGRES_COPY_IN ||
				PQresultStatus(res) == PGRES_COPY_OUT ||
				PQresultStatus(res) == PGRES_COPY_BOTH)
				ereport(ERROR,
						(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
						 errmsg("COPY is not supported on a control session")));
		}
		ok = true;
	}
	PG_FINALLY();
	{
		if (!ok)
		{
			ctl_cancel(cc);
			ctl_discard(cc);
			PQclear(last);
		}
	}
	PG_END_TRY();
	return last;
}

/* Raise the error held by res (which is cleared) with the remote SQLSTATE. */
static void
ctl_raise(CtlConn *cc, PGresult *res)
{
	int			sqlstate = kingres_shard_result_sqlstate(res);
	const char *s;
	char	   *primary = NULL;
	char	   *detail = NULL;
	char	   *hint = NULL;

	if (res != NULL)
	{
		if ((s = PQresultErrorField(res, PG_DIAG_MESSAGE_PRIMARY)) != NULL)
			primary = pstrdup(s);
		if ((s = PQresultErrorField(res, PG_DIAG_MESSAGE_DETAIL)) != NULL)
			detail = pstrdup(s);
		if ((s = PQresultErrorField(res, PG_DIAG_MESSAGE_HINT)) != NULL)
			hint = pstrdup(s);
		PQclear(res);
	}
	if (sqlstate == 0)
		sqlstate = ERRCODE_CONNECTION_FAILURE;
	if (PQstatus(cc->conn) != CONNECTION_OK)
		ctl_discard(cc);
	if (primary == NULL)
		primary = pstrdup("no result");

	ereport(ERROR,
			(errcode(sqlstate),
			 errmsg_internal("%s", primary),
			 detail ? errdetail_internal("%s", detail) : 0,
			 hint ? errhint("%s", hint) : 0,
			 errcontext("remote node %s:%d", cc->host, cc->port)));
}

/*
 * Copy the fault injection setting of this session to the control session, so
 * that a point inside a function that runs there fires there.
 */
static void
ctl_sync_fault(CtlConn *cc)
{
	const char *spec = kingres_shard_fault_spec();
	PGresult   *res;
	char	   *sql;

	if (strcmp(spec, cc->fault) == 0)
		return;
	if (strlen(spec) >= sizeof(cc->fault))
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("kingres_shard.fault_injection is too long to copy to a control session")));
	if (spec[0] == '\0')
		sql = pstrdup("RESET kingres_shard.fault_injection");
	else
		sql = psprintf("SET kingres_shard.fault_injection = %s", quote_literal_cstr(spec));
	res = ctl_run(cc, sql, kingres_shard_node_timeout * 1000);
	if (PQresultStatus(res) != PGRES_COMMAND_OK)
		ctl_raise(cc, res);
	PQclear(res);
	strlcpy(cc->fault, spec, sizeof(cc->fault));
}

/* The rows of a result as a JSON array of arrays of text (null for NULL). */
static Datum
result_to_jsonb(PGresult *res)
{
	StringInfoData buf;
	int			nrows = PQntuples(res);
	int			ncols = PQnfields(res);
	int			r;
	int			c;

	initStringInfo(&buf);
	appendStringInfoChar(&buf, '[');
	for (r = 0; r < nrows; r++)
	{
		if (r > 0)
			appendStringInfoChar(&buf, ',');
		appendStringInfoChar(&buf, '[');
		for (c = 0; c < ncols; c++)
		{
			if (c > 0)
				appendStringInfoChar(&buf, ',');
			if (PQgetisnull(res, r, c))
				appendStringInfoString(&buf, "null");
			else
				escape_json(&buf, PQgetvalue(res, r, c));
		}
		appendStringInfoChar(&buf, ']');
	}
	appendStringInfoChar(&buf, ']');
	return DirectFunctionCall1(jsonb_in, CStringGetDatum(buf.data));
}

static void
require_superuser(const char *what)
{
	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for %s", what),
				 errdetail("Moving buckets is for superusers: it creates publications, subscriptions and replication slots on the nodes.")));
}

/*
 * kingres.internal_node_exec(node_id int, sql text, timeout_ms int) returns jsonb
 */
Datum
kingres_internal_node_exec(PG_FUNCTION_ARGS)
{
	int			node_id = PG_GETARG_INT32(0);
	char	   *sql = text_to_cstring(PG_GETARG_TEXT_PP(1));
	int			timeout_ms = PG_ARGISNULL(2) ? kingres_shard_node_timeout * 1000 : PG_GETARG_INT32(2);
	const KsNode *n;
	char	   *host;
	int			port;
	CtlConn    *cc;
	PGresult   *res;
	Datum		result;

	require_superuser("kingres.internal_node_exec()");
	if (timeout_ms < 1)
		timeout_ms = 1;
	n = kingres_shard_node_by_id(node_id);
	if (n == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_OBJECT),
				 errmsg("node %d does not exist", node_id)));
	host = pstrdup(n->host);
	port = n->port;

	cc = ctl_get(host, port);
	ctl_sync_fault(cc);
	res = ctl_run(cc, sql, timeout_ms);
	if (res == NULL ||
		(PQresultStatus(res) != PGRES_COMMAND_OK &&
		 PQresultStatus(res) != PGRES_TUPLES_OK &&
		 PQresultStatus(res) != PGRES_EMPTY_QUERY))
		ctl_raise(cc, res);
	result = result_to_jsonb(res);
	PQclear(res);
	PG_RETURN_DATUM(result);
}

/*
 * Run a statement on a control session; any error is raised.  Returns the
 * result, which the caller clears.
 */
static PGresult *
ctl_must(const char *host, int port, const char *sql, int timeout_ms)
{
	CtlConn    *cc = ctl_get(host, port);
	PGresult   *res;

	ctl_sync_fault(cc);
	res = ctl_run(cc, sql, timeout_ms);
	if (res == NULL ||
		(PQresultStatus(res) != PGRES_COMMAND_OK &&
		 PQresultStatus(res) != PGRES_TUPLES_OK))
		ctl_raise(cc, res);
	return res;
}

/* ---------------------------------------------------------------------------
 * The mark of a live move
 * ---------------------------------------------------------------------------
 */

/*
 * The session that runs a move holds a session-level lock on a tag of its own
 * (an advisory lock tag with a fourth field that the SQL advisory functions
 * never use, so no application key can collide with it).  Whoever finds it held
 * knows the move is alive: resume_bucket_moves() leaves it alone.  The lock is
 * released when the session ends, however it ends.  A row lock on the record
 * of the move would do the same, but it gives the running session a transaction
 * id, and the slot of the move waits for every transaction that has one.
 */
static void
move_locktag(int64 move_id, LOCKTAG *tag)
{
	SET_LOCKTAG_ADVISORY(*tag, MyDatabaseId,
						 (uint32) (((uint64) move_id) >> 32), (uint32) move_id, 3);
}

PG_FUNCTION_INFO_V1(kingres_internal_move_lock);
PG_FUNCTION_INFO_V1(kingres_internal_move_unlock);
PG_FUNCTION_INFO_V1(kingres_internal_move_alive);

Datum
kingres_internal_move_lock(PG_FUNCTION_ARGS)
{
	LOCKTAG		tag;

	require_superuser("kingres.internal_move_lock()");
	move_locktag(PG_GETARG_INT64(0), &tag);
	(void) LockAcquire(&tag, ExclusiveLock, true, false);
	PG_RETURN_VOID();
}

Datum
kingres_internal_move_unlock(PG_FUNCTION_ARGS)
{
	LOCKTAG		tag;

	require_superuser("kingres.internal_move_unlock()");
	move_locktag(PG_GETARG_INT64(0), &tag);
	(void) LockRelease(&tag, ExclusiveLock, true);
	PG_RETURN_VOID();
}

Datum
kingres_internal_move_alive(PG_FUNCTION_ARGS)
{
	LOCKTAG		tag;
	LockAcquireResult res;

	require_superuser("kingres.internal_move_alive()");
	move_locktag(PG_GETARG_INT64(0), &tag);
	res = LockAcquire(&tag, ExclusiveLock, true, true);
	if (res == LOCKACQUIRE_NOT_AVAIL)
		PG_RETURN_BOOL(true);
	(void) LockRelease(&tag, ExclusiveLock, true);
	PG_RETURN_BOOL(false);
}

/*
 * kingres.internal_map_version() returns bigint
 *
 * The latest committed version of the bucket map on this node, whatever the
 * snapshot of the transaction is.  (The distributed scan and the fan-out write
 * compare the placement of the colocation group instead, below: the version
 * counts every change of the catalog.)
 */
PG_FUNCTION_INFO_V1(kingres_internal_map_version);

Datum
kingres_internal_map_version(PG_FUNCTION_ARGS)
{
	int64		version = kingres_shard_map_version_now();

	if (version < 0)
		PG_RETURN_NULL();
	PG_RETURN_INT64(version);
}

/*
 * kingres.internal_placement_hash(colocation_id int, parent regclass default null)
 * returns bigint
 *
 * The placement of the colocation group on this node as one number (which group
 * holds each bucket), from the latest committed state of the catalog whatever the
 * snapshot of the transaction is.  A fragment of a distributed scan asks for it
 * before it opens its cursor and again after (design/shard-phase4.md, "The map
 * version rule"): the partitions of a node change only in the transaction that
 * also changes its placement, so when the two answers and the origin's are equal,
 * no switch committed on this node in between, and the cursor reads the partitions
 * of that placement.
 *
 * With the parent table, the first call also makes sure that the partition list
 * the cursor will plan with is not older than the answer: a backend that has not
 * seen this placement of the table before forgets its relcache entry of the
 * parent.  (A commit becomes visible a moment before its invalidation messages
 * reach the other backends; without this a long-lived session could read the new
 * placement and plan with the old partitions.)
 */
PG_FUNCTION_INFO_V1(kingres_internal_placement_hash);

#define KS_SEEN_PARENTS 16

Datum
kingres_internal_placement_hash(PG_FUNCTION_ARGS)
{
	static struct
	{
		Oid			parent;
		int64		hash;
	}			seen[KS_SEEN_PARENTS];
	static int	next_slot = 0;
	int64		h = kingres_shard_placement_hash_now(PG_GETARG_INT32(0));

	if (!PG_ARGISNULL(1))
	{
		Oid			parent = PG_GETARG_OID(1);
		int			i;
		bool		known = false;

		for (i = 0; i < KS_SEEN_PARENTS; i++)
		{
			if (seen[i].parent == parent)
			{
				known = (seen[i].hash == h);
				seen[i].hash = h;
				break;
			}
		}
		if (i == KS_SEEN_PARENTS)
		{
			seen[next_slot].parent = parent;
			seen[next_slot].hash = h;
			next_slot = (next_slot + 1) % KS_SEEN_PARENTS;
		}
		if (!known)
		{
			/* The rebuild must not use a catalog snapshot older than the answer. */
			InvalidateCatalogSnapshot();
			RelationCacheInvalidateEntry(parent);
		}
	}
	PG_RETURN_INT64(h);
}

/* ---------------------------------------------------------------------------
 * The cluster lock and the guard of DDL
 * ---------------------------------------------------------------------------
 */

/*
 * kingres_shard_assert_no_move
 *
 * See bucket_move.h.
 */
void
kingres_shard_assert_no_move(int colocation_id)
{
	Oid			argtypes[1] = {INT4OID};
	Datum		values[1];
	int			found = 0;

	if (!OidIsValid(kingres_shard_catalog_relid("bucket_move")))
		return;
	values[0] = Int32GetDatum(colocation_id);
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	if (SPI_execute_with_args("SELECT m.move_id FROM " KINGRES_SHARD_SCHEMA ".bucket_move m "
							  "WHERE m.colocation_id OPERATOR(pg_catalog.=) $1 "
							  "AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching') "
							  "LIMIT 1",
							  1, argtypes, values, NULL, true, 0) != SPI_OK_SELECT)
		elog(ERROR, "could not look for bucket moves");
	if (SPI_processed > 0)
	{
		bool		isnull;

		found = (int) DatumGetInt64(SPI_getbinval(SPI_tuptable->vals[0],
												   SPI_tuptable->tupdesc, 1, &isnull));
	}
	SPI_finish();
	if (found > 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_IN_USE),
				 errmsg("a bucket of colocation group %d is being moved (move %d)",
						colocation_id, found),
				 errdetail("The statement would change the tables of the group while the copy of the bucket on the target does not see it."),
				 errhint("Wait for the move to finish, or cancel it with kingres.cancel_bucket_move(); see kingres.bucket_move.")));
}

/* ---------------------------------------------------------------------------
 * The switch
 * ---------------------------------------------------------------------------
 */

typedef struct MoveTable
{
	char	   *qualified;		/* quoted schema.table of the parent */
} MoveTable;

static int
remaining_ms(TimestampTz deadline)
{
	long		secs;
	int			usecs;
	long		ms;

	if (GetCurrentTimestamp() >= deadline)
		return 1;
	TimestampDifference(GetCurrentTimestamp(), deadline, &secs, &usecs);
	ms = secs * 1000L + usecs / 1000;
	return ms < 1 ? 1 : (int) Min(ms, 3600000L);
}

/* The statement that locks the parents of the tables of the move. */
static char *
lock_statement(List *tables, const char *mode)
{
	StringInfoData buf;
	ListCell   *lc;
	bool		first = true;

	initStringInfo(&buf);
	appendStringInfoString(&buf, "LOCK TABLE ");
	foreach(lc, tables)
	{
		MoveTable  *t = (MoveTable *) lfirst(lc);

		appendStringInfo(&buf, "%sONLY %s", first ? "" : ", ", t->qualified);
		first = false;
	}
	appendStringInfo(&buf, " IN %s MODE", mode);
	return buf.data;
}

/*
 * A participant of the switch: a node whose tables are locked and whose bucket
 * partition is attached or detached.  When the node is this one, the work is done
 * in this transaction (the coordinator's): its partitions then change in the same
 * transaction as its catalog, which is what lets a statement of this node trust
 * both (design/shard-phase4.md, "The map version rule").  A session of this
 * backend to its own node would commit its part before this transaction does, and
 * the node would show the new catalog with the old partitions in between.
 */
typedef struct Part
{
	KsConn	   *kc;				/* NULL for this node */
	bool		local;
} Part;

static void
part_exec(Part *p, const char *sql)
{
	if (p->local)
	{
		if (SPI_connect() != SPI_OK_CONNECT)
			elog(ERROR, "SPI_connect failed");
		if (SPI_execute(sql, false, 0) < 0)
			elog(ERROR, "statement failed on this node: %s", sql);
		SPI_finish();
	}
	else
	{
		PGresult   *res = kingres_shard_exec(p->kc, sql);

		PQclear(res);
	}
}

/*
 * The settings of the transaction on a participant of the switch: how long it
 * may wait for a lock (the switch gives up and the move goes back to catching
 * up), and a short deadlock timeout so that if this session is part of a
 * deadlock it is the one that finds it and fails, never an application
 * transaction.
 */
static Part
part_begin(const char *host, int port, bool local, TimestampTz deadline, int lock_ms)
{
	Part		p;
	char	   *sql;

	p.local = local;
	p.kc = NULL;
	if (p.local)
	{
		sql = psprintf("SET LOCAL lock_timeout = '%dms'; SET LOCAL deadlock_timeout = '100ms'",
					   Min(lock_ms, remaining_ms(deadline)));
		part_exec(&p, sql);
		return p;
	}
	p.kc = kingres_shard_get_conn(host, port, false, NULL);
	kingres_shard_begin_write_xact(p.kc);
	if (kingres_shard_fault_spec()[0] != '\0')
	{
		sql = psprintf("SET LOCAL kingres_shard.fault_injection = %s",
					   quote_literal_cstr(kingres_shard_fault_spec()));
		part_exec(&p, sql);
	}
	sql = psprintf("SET LOCAL lock_timeout = '%dms'; SET LOCAL statement_timeout = '%dms'; "
				   "SET LOCAL deadlock_timeout = '100ms'",
				   Min(lock_ms, remaining_ms(deadline)), remaining_ms(deadline));
	part_exec(&p, sql);
	return p;
}

/*
 * kingres.internal_switch_bucket(move_id bigint, switch_timeout_ms int,
 *								   cleanup_delay_s int) returns void
 */
Datum
kingres_internal_switch_bucket(PG_FUNCTION_ARGS)
{
	int64		move_id = PG_GETARG_INT64(0);
	int			switch_ms = PG_GETARG_INT32(1);
	int			cleanup_delay = PG_GETARG_INT32(2);
	MemoryContext callercxt = CurrentMemoryContext;
	TimestampTz deadline;
	Oid			argtypes[3];
	Datum		values[3];
	bool		isnull;
	int			colocation_id;
	int			bucket;
	int			source_group;
	int			target_group;
	int			bucket_count;
	char	   *phase;
	List	   *tables = NIL;
	const KsNode *src;
	const KsNode *tgt;
	char	   *srchost;
	char	   *tgthost;
	int			srcport;
	int			tgtport;
	bool		src_local;
	bool		tgt_local;
	Part		S;
	Part		T;
	PGresult   *res;
	char	   *sql;
	int64		seq;
	uint64		i;
	ListCell   *lc;
	int			ord;
	int			lock_ms;

	require_superuser("kingres.internal_switch_bucket()");
	kingres_shard_fault_set_scope("switch");
	kingres_shard_lock_catalog();
	kingres_shard_require_map_group();

	/* 1. The move and its tables, read under the cluster lock. */
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	argtypes[0] = INT8OID;
	values[0] = Int64GetDatum(move_id);
	if (SPI_execute_with_args("SELECT colocation_id, bucket, source_group, target_group, phase "
							  "FROM " KINGRES_SHARD_SCHEMA ".bucket_move WHERE move_id OPERATOR(pg_catalog.=) $1 FOR NO KEY UPDATE",
							  1, argtypes, values, NULL, false, 0) != SPI_OK_SELECT ||
		SPI_processed != 1)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_OBJECT),
				 errmsg("bucket move " INT64_FORMAT " does not exist", move_id)));
	colocation_id = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull));
	bucket = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 2, &isnull));
	source_group = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 3, &isnull));
	target_group = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 4, &isnull));
	phase = MemoryContextStrdup(callercxt, SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 5));
	if (strcmp(phase, "switching") != 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("bucket move " INT64_FORMAT " is in phase \"%s\", not \"switching\"", move_id, phase)));

	argtypes[0] = INT4OID;
	argtypes[1] = INT4OID;
	values[0] = Int32GetDatum(colocation_id);
	values[1] = Int32GetDatum(bucket);
	if (SPI_execute_with_args("SELECT group_id, state FROM " KINGRES_SHARD_SCHEMA ".bucket_map "
							  "WHERE colocation_id OPERATOR(pg_catalog.=) $1 AND bucket OPERATOR(pg_catalog.=) $2",
							  2, argtypes, values, NULL, true, 0) != SPI_OK_SELECT ||
		SPI_processed != 1 ||
		DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull)) != source_group ||
		strcmp(SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 2), "moving") != 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("bucket %d of colocation group %d is not moving from group %d",
						bucket, colocation_id, source_group)));

	argtypes[0] = INT4OID;
	values[0] = Int32GetDatum(colocation_id);
	if (SPI_execute_with_args("SELECT bucket_count FROM " KINGRES_SHARD_SCHEMA ".colocation "
							  "WHERE colocation_id OPERATOR(pg_catalog.=) $1",
							  1, argtypes, values, NULL, true, 0) != SPI_OK_SELECT || SPI_processed != 1)
		elog(ERROR, "colocation group %d does not exist", colocation_id);
	bucket_count = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull));

	if (SPI_execute_with_args("SELECT schema_name, table_name FROM " KINGRES_SHARD_SCHEMA ".internal_move_tables($1) ORDER BY ord",
							  1, argtypes, values, NULL, true, 0) != SPI_OK_SELECT || SPI_processed == 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("colocation group %d has no distributed table", colocation_id)));
	for (i = 0; i < SPI_processed; i++)
	{
		MemoryContext old = MemoryContextSwitchTo(callercxt);
		MoveTable  *t = palloc(sizeof(MoveTable));
		char	   *nsp = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1);
		char	   *rel = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2);

		t->qualified = psprintf("%s.%s", quote_identifier(nsp), quote_identifier(rel));
		tables = lappend(tables, t);
		MemoryContextSwitchTo(old);
	}
	SPI_finish();

	src = kingres_shard_group_primary(source_group);
	tgt = kingres_shard_group_primary(target_group);
	if (src == NULL || tgt == NULL || !src->is_active || !tgt->is_active)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("shard group %d or %d has no active primary", source_group, target_group)));
	srchost = pstrdup(src->host);
	srcport = src->port;
	tgthost = pstrdup(tgt->host);
	tgtport = tgt->port;
	src_local = (src->node_id == kingres_shard_catalog()->local_node_id);
	tgt_local = (tgt->node_id == kingres_shard_catalog()->local_node_id);

	deadline = TimestampTzPlusMilliseconds(GetCurrentTimestamp(), switch_ms);

	/*
	 * Each lock is waited for much less than the whole attempt: the writers of
	 * one node that wait for the lock on the other (they hold their own node's
	 * lock while they wait) and this transaction that waits for them form a
	 * cycle that no node can see; the lock timeout ends it, the attempt is
	 * abandoned, and the writers go on.  The next attempt starts a moment later.
	 */
	lock_ms = Max(200, switch_ms / 10);

	/*
	 * 1b. The target first: a lock on the parents of the tables that the attach
	 * needs anyway (attaching a table with a foreign key locks the referenced
	 * table in SHARE ROW EXCLUSIVE).  Taken now, before anything on the source is
	 * locked, it waits only for statements that finish by themselves; taken at
	 * the attach it would wait for a statement of this node that is itself
	 * waiting for the lock on the source, which nothing detects.  From here to the
	 * commit the writers of the tables wait on the target too.
	 */
	T = part_begin(tgthost, tgtport, tgt_local, deadline, lock_ms);
	part_exec(&T, lock_statement(tables, "SHARE ROW EXCLUSIVE"));

	/*
	 * 2. The source: lock the parents of the tables.  Writers of the group's
	 * tables on this node wait here, readers go on; every transaction that
	 * wrote the bucket before has finished when the lock is granted, or the
	 * lock times out (lock_not_available) and the move goes back to catching
	 * up.  Locking the parents, and not only the partitions of the bucket, is
	 * what makes the detach below possible without a deadlock: a writer that
	 * is queued for a partition holds a lock on its parent, which the detach
	 * needs exclusively.
	 */
	S = part_begin(srchost, srcport, src_local, deadline, lock_ms);
	part_exec(&S, lock_statement(tables, "SHARE"));
	kingres_shard_fault_point("move_switch_after_lock");

	/*
	 * 3. The proof that the copy has everything: a marker row committed on the
	 * source after the lock is replicated after every transaction that wrote
	 * the bucket before it; when it is on the target, so are they.
	 */
	sql = psprintf("INSERT INTO " KINGRES_SHARD_SCHEMA ".move_marker (move_id, seq) "
				   "VALUES (" INT64_FORMAT ", (extract(epoch FROM pg_catalog.clock_timestamp()) * 1000000)::pg_catalog.int8) "
				   "RETURNING seq", move_id);
	res = ctl_must(srchost, srcport, sql, remaining_ms(deadline));
	seq = pg_strtoint64(PQgetvalue(res, 0, 0));
	PQclear(res);
	kingres_shard_fault_point("move_switch_after_marker");
	sql = psprintf("SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".move_marker "
				   "WHERE move_id OPERATOR(pg_catalog.=) " INT64_FORMAT " AND seq OPERATOR(pg_catalog.=) " INT64_FORMAT,
				   move_id, seq);
	for (;;)
	{
		bool		seen;

		res = ctl_must(tgthost, tgtport, sql, remaining_ms(deadline));
		seen = PQntuples(res) > 0;
		PQclear(res);
		if (seen)
			break;
		if (GetCurrentTimestamp() >= deadline)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_SWITCH_TIMEOUT),
					 errmsg("the copy of bucket %d did not replay up to the switch point within %d ms",
							bucket, switch_ms),
					 errhint("The move returns to catching up and tries again.")));
		CHECK_FOR_INTERRUPTS();
		pg_usleep(10000L);
	}

	/*
	 * 4. The target: attach the copies, the referenced tables first.  The locks
	 * the attach needs are held since step 1b.  A reader that straddles the switch
	 * is caught by the map version rule instead of by a lock (dist_scan.c: every
	 * fragment compares the version of its node with its origin's after it holds
	 * its locks).
	 */	foreach(lc, tables)
	{
		MoveTable  *t = (MoveTable *) lfirst(lc);

		sql = psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_attach_bucket(%s::pg_catalog.regclass, %d, %d)",
					   quote_literal_cstr(t->qualified), bucket, bucket_count);
		part_exec(&T, sql);
	}
	kingres_shard_fault_point("move_switch_after_attach");

	/* 5. The source: detach, the referencing tables first. */
	ord = list_length(tables);
	for (i = ord; i > 0; i--)
	{
		MoveTable  *t = (MoveTable *) list_nth(tables, i - 1);

		sql = psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_detach_bucket(%s::pg_catalog.regclass, %d, 'orphan_" INT64_FORMAT "_%d')",
					   quote_literal_cstr(t->qualified), bucket, move_id, (int) i);
		part_exec(&S, sql);
	}
	kingres_shard_fault_point("move_switch_after_detach");

	/* 6. The map, on this node and, through the synchronization, on every other. */
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	argtypes[0] = INT4OID;
	argtypes[1] = INT4OID;
	argtypes[2] = INT4OID;
	values[0] = Int32GetDatum(colocation_id);
	values[1] = Int32GetDatum(target_group);
	values[2] = Int32GetDatum(bucket);
	if (SPI_execute_with_args("UPDATE " KINGRES_SHARD_SCHEMA ".bucket_map SET group_id = $2, state = 'active' "
							  "WHERE colocation_id OPERATOR(pg_catalog.=) $1 AND bucket OPERATOR(pg_catalog.=) $3",
							  3, argtypes, values, NULL, false, 0) != SPI_OK_UPDATE)
		elog(ERROR, "could not update the bucket map");
	argtypes[0] = INT8OID;
	argtypes[1] = INT4OID;
	values[0] = Int64GetDatum(move_id);
	values[1] = Int32GetDatum(cleanup_delay);
	if (SPI_execute_with_args("UPDATE " KINGRES_SHARD_SCHEMA ".bucket_move SET phase = 'cleaning', detail = NULL, "
							  "updated_at = pg_catalog.clock_timestamp(), "
							  "cleanup_after = pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+) pg_catalog.make_interval(secs => $2) "
							  "WHERE move_id OPERATOR(pg_catalog.=) $1",
							  2, argtypes, values, NULL, false, 0) != SPI_OK_UPDATE)
		elog(ERROR, "could not update the bucket move");
	SPI_finish();
	kingres_shard_sync_catalog_internal();
	kingres_shard_fault_point("move_switch_after_local");

	PG_RETURN_VOID();
}

/* ---------------------------------------------------------------------------
 * Attach and detach of one bucket partition
 * ---------------------------------------------------------------------------
 */

/* Run one statement through SPI; errors are raised. */
static void
spi_stmt(const char *sql)
{
	if (SPI_execute(sql, false, 0) < 0)
		elog(ERROR, "statement failed: %s", sql);
}

/* The distributed table that a regclass argument must be, and its names. */
static const KsDistTable *
move_parent(Oid parent, char **nsp, char **rel, char **part, int bucket)
{
	const KsDistTable *dt = kingres_shard_lookup_dist_table(parent);

	if (dt == NULL || dt->is_reference)
		ereport(ERROR,
				(errcode(ERRCODE_WRONG_OBJECT_TYPE),
				 errmsg("\"%s\" is not a distributed table", get_rel_name(parent))));
	*nsp = pstrdup(get_namespace_name(get_rel_namespace(parent)));
	*rel = pstrdup(get_rel_name(parent));
	*part = psprintf("%s_b%04d", *rel, bucket);
	return dt;
}

/*
 * kingres.internal_attach_bucket(parent regclass, bucket int, bucket_count int)
 *
 * Attach the standalone table <parent>_b<bbbb> as the partition of the bucket.
 */
Datum
kingres_internal_attach_bucket(PG_FUNCTION_ARGS)
{
	Oid			parent = PG_GETARG_OID(0);
	int			bucket = PG_GETARG_INT32(1);
	int			bucket_count = PG_GETARG_INT32(2);
	char	   *nsp;
	char	   *rel;
	char	   *part;
	StringInfoData sql;

	require_superuser("kingres.internal_attach_bucket()");
	(void) move_parent(parent, &nsp, &rel, &part, bucket);
	kingres_shard_fault_point("move_attach");

	initStringInfo(&sql);
	appendStringInfo(&sql, "ALTER TABLE %s.%s ATTACH PARTITION %s.%s FOR VALUES WITH (MODULUS %d, REMAINDER %d)",
					 quote_identifier(nsp), quote_identifier(rel),
					 quote_identifier(nsp), quote_identifier(part),
					 bucket_count, bucket);
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	kingres_shard_internal_ddl_enter();
	PG_TRY();
	{
		spi_stmt(sql.data);
		/*
		 * The check that spared the validation scan has done its work.  The
		 * table is a bucket partition now, so this statement is one for the
		 * hook to leave alone as well.
		 */
		resetStringInfo(&sql);
		appendStringInfo(&sql, "ALTER TABLE %s.%s DROP CONSTRAINT IF EXISTS ks_bucket_check",
						 quote_identifier(nsp), quote_identifier(part));
		spi_stmt(sql.data);
	}
	PG_FINALLY();
	{
		kingres_shard_internal_ddl_leave();
	}
	PG_END_TRY();
	SPI_finish();
	PG_RETURN_VOID();
}

/*
 * kingres.internal_detach_bucket(parent regclass, bucket int, orphan text)
 *
 * Detach the partition of the bucket and turn it into the orphan table
 * kingres.<orphan>.  The foreign keys that the table kept are dropped: they
 * would still make the referenced tables look for rows in it.
 */
Datum
kingres_internal_detach_bucket(PG_FUNCTION_ARGS)
{
	Oid			parent = PG_GETARG_OID(0);
	int			bucket = PG_GETARG_INT32(1);
	char	   *orphan = text_to_cstring(PG_GETARG_TEXT_PP(2));
	char	   *nsp;
	char	   *rel;
	char	   *part;
	StringInfoData sql;
	Oid			partoid;
	uint64		i;
	List	   *fks = NIL;
	ListCell   *lc;

	require_superuser("kingres.internal_detach_bucket()");
	(void) move_parent(parent, &nsp, &rel, &part, bucket);
	kingres_shard_fault_point("move_detach");

	initStringInfo(&sql);
	appendStringInfo(&sql, "ALTER TABLE %s.%s DETACH PARTITION %s.%s",
					 quote_identifier(nsp), quote_identifier(rel),
					 quote_identifier(nsp), quote_identifier(part));
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	kingres_shard_internal_ddl_enter();
	PG_TRY();
	{
		spi_stmt(sql.data);
	}
	PG_FINALLY();
	{
		kingres_shard_internal_ddl_leave();
	}
	PG_END_TRY();

	partoid = get_relname_relid(part, get_rel_namespace(parent));
	if (!OidIsValid(partoid))
		elog(ERROR, "could not find the detached partition \"%s\"", part);
	{
		Oid			argtypes[1] = {OIDOID};
		Datum		values[1];

		values[0] = ObjectIdGetDatum(partoid);
		if (SPI_execute_with_args("SELECT c.conname FROM pg_catalog.pg_constraint c "
								  "WHERE c.conrelid OPERATOR(pg_catalog.=) $1 AND c.contype OPERATOR(pg_catalog.=) 'f' "
								  "ORDER BY c.conname",
								  1, argtypes, values, NULL, true, 0) != SPI_OK_SELECT)
			elog(ERROR, "could not list the foreign keys of \"%s\"", part);
		for (i = 0; i < SPI_processed; i++)
			fks = lappend(fks, pstrdup(SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1)));
	}
	foreach(lc, fks)
	{
		resetStringInfo(&sql);
		appendStringInfo(&sql, "ALTER TABLE %s.%s DROP CONSTRAINT IF EXISTS %s",
						 quote_identifier(nsp), quote_identifier(part),
						 quote_identifier((char *) lfirst(lc)));
		spi_stmt(sql.data);
	}
	resetStringInfo(&sql);
	appendStringInfo(&sql, "ALTER TABLE %s.%s RENAME TO %s",
					 quote_identifier(nsp), quote_identifier(part), quote_identifier(orphan));
	spi_stmt(sql.data);
	resetStringInfo(&sql);
	appendStringInfo(&sql, "ALTER TABLE %s.%s SET SCHEMA " KINGRES_SHARD_SCHEMA,
					 quote_identifier(nsp), quote_identifier(orphan));
	spi_stmt(sql.data);
	SPI_finish();
	PG_RETURN_VOID();
}

/* ---------------------------------------------------------------------------
 * The connection string of the subscription
 * ---------------------------------------------------------------------------
 */

/* A libpq connection string value: in single quotes, backslash and quote escaped. */
static void
append_conninfo_value(StringInfo buf, const char *key, const char *value)
{
	const char *p;

	appendStringInfo(buf, " %s='", key);
	for (p = value; *p; p++)
	{
		if (*p == '\'' || *p == '\\')
			appendStringInfoChar(buf, '\\');
		appendStringInfoChar(buf, *p);
	}
	appendStringInfoChar(buf, '\'');
}

/*
 * kingres.internal_subscription_conninfo(host text, port int) returns text
 */
Datum
kingres_internal_subscription_conninfo(PG_FUNCTION_ARGS)
{
	char	   *host = text_to_cstring(PG_GETARG_TEXT_PP(0));
	int			port = PG_GETARG_INT32(1);
	StringInfoData buf;
	char		portstr[16];
	char		timeoutstr[16];

	require_superuser("kingres.internal_subscription_conninfo()");
	snprintf(portstr, sizeof(portstr), "%d", port);
	snprintf(timeoutstr, sizeof(timeoutstr), "%d", kingres_shard_node_connect_timeout);
	initStringInfo(&buf);
	appendStringInfoString(&buf, "application_name=kingres_shard_move");
	append_conninfo_value(&buf, "host", host);
	append_conninfo_value(&buf, "port", portstr);
	append_conninfo_value(&buf, "dbname", get_database_name(MyDatabaseId));
	append_conninfo_value(&buf, "user", GetUserNameFromId(GetUserId(), false));
	append_conninfo_value(&buf, "connect_timeout", timeoutstr);
	/*
	 * The administrator's options (sslmode, sslcert, ...), as for every
	 * node-to-node connection; the setting cannot name a keyword used above.
	 */
	if (kingres_shard_node_conninfo != NULL && *kingres_shard_node_conninfo != '\0')
		appendStringInfo(&buf, " %s", kingres_shard_node_conninfo);
	PG_RETURN_TEXT_P(cstring_to_text(buf.data));
}
