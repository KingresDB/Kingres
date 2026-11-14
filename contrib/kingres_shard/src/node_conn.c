/*-------------------------------------------------------------------------
 *
 * node_conn.c
 *	  libpq connection cache to the other nodes of the cluster, and the
 *	  joining of remote transactions to the local transaction.
 *
 * The structure follows contrib/postgres_fdw/connection.c (PostgreSQL
 * License; Copyright (c) 2012-2025, PostgreSQL Global Development Group),
 * which solves the same problem: a backend-local cache of libpq connections,
 * a remote transaction started on first use, committed in a pre-commit
 * callback and rolled back on abort, and savepoints that mirror local
 * subtransactions.
 *
 * Every connection is opened as the current user, to the current database,
 * with kingres_shard.forwarded = on, so that the remote session never
 * forwards a statement again (design/shard-mvp.md, "Loop prevention").
 *
 * A forwarded statement must mean on the remote node what it means locally,
 * whatever the session settings of either node are (review S-C).  The
 * statement is deparsed under the "transmission modes" below (the technique of
 * contrib/postgres_fdw: ISO dates, postgres-style intervals, shortest-exact
 * floats, standard-conforming strings, hex bytea, search_path = pg_catalog),
 * and the remote session runs under the very same settings, which are
 * forced twice: as connection options, and again with SET LOCAL at the
 * start of every remote transaction (so a function that changed one of them
 * cannot leak into the next statement).  The session's client_encoding is the
 * local database encoding, because the statement text and the result rows are
 * in that encoding.  TimeZone is the one setting that is mirrored instead of
 * fixed: a statement such as "ts::date" depends on it, so the remote
 * transaction uses the TimeZone of the local session at the time of each
 * statement.  Authentication is whatever pg_hba.conf and the password file
 * provide; no credentials are kept in the catalog.
 *
 * Commit is not atomic across nodes: remote transactions are committed in the
 * pre-commit callback one after the other, before the local commit.  Phase 2
 * replaces this with two-phase commit.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/node_conn.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/xact.h"
#include "commands/dbcommands.h"
#include "libpq/libpq-be-fe-helpers.h"
#include "mb/pg_wchar.h"
#include "miscadmin.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/hsearch.h"
#include "utils/memutils.h"
#include "utils/timestamp.h"
#include "utils/wait_classes.h"

#include "kingres_shard.h"
#include "node_conn.h"

#define KS_CONN_KEY_SIZE (offsetof(KsConn, user) + NAMEDATALEN)

/*
 * The settings that every remote transaction runs under, whatever the
 * settings of the local session are; see the file header.  They are the
 * transmission modes below, as SET LOCAL commands.
 */
#define KS_FIXED_SETTINGS_SQL \
	"SET LOCAL search_path = pg_catalog; " \
	"SET LOCAL DateStyle = 'ISO, MDY'; " \
	"SET LOCAL IntervalStyle = 'postgres'; " \
	"SET LOCAL extra_float_digits = 3; " \
	"SET LOCAL standard_conforming_strings = on; " \
	"SET LOCAL bytea_output = 'hex'; "


static HTAB *conn_hash = NULL;

/* The only remote connection a routed write may use in this transaction. */
static KsConn *write_conn = NULL;

/*
 * Local writes that are not the user's: a catalog reload after a stale map
 * assigns the transaction id of the transaction when it was unassigned; that
 * id must not count as a local write by the user.  local_write_seen is set
 * by the executor hook for real local DML, which the id alone cannot tell
 * from our own catalog write.
 */
static TransactionId ignored_xid = InvalidTransactionId;
static bool local_write_seen = false;

static void conn_xact_callback(XactEvent event, void *arg);
static void conn_subxact_callback(SubXactEvent event, SubTransactionId mySubid,
								  SubTransactionId parentSubid, void *arg);
static void exec_internal(KsConn *kc, const char *sql);
static void append_timezone_setting(StringInfo buf);
static void remember_timezone(KsConn *kc);
static void sync_timezone(KsConn *kc);
static void discard_connection(KsConn *kc);

/*
 * kingres_shard_conn_init
 *
 * Called once from _PG_init: registers the transaction callbacks.
 */
void
kingres_shard_conn_init(void)
{
	RegisterXactCallback(conn_xact_callback, NULL);
	RegisterSubXactCallback(conn_subxact_callback, NULL);
}

/*
 * Notices from remote nodes (for example "schema already exists, skipping")
 * are not interesting to the client and libpq would print them to stderr.
 */
static void
ignore_notice(void *arg, const PGresult *res)
{
}

/*
 * connect_to_node
 *
 * Open a new libpq connection to host:port as the current user.  Returns
 * NULL and sets *errmsg (palloc'd) on failure.
 */
static PGconn *
connect_to_node(const char *host, int port, const char *user, char **errmsg_out)
{
	const char *keywords[10];
	const char *values[10];
	char		portstr[16];
	char		timeoutstr[16];
	PGconn	   *conn;
	int			n = 0;

	snprintf(portstr, sizeof(portstr), "%d", port);
	snprintf(timeoutstr, sizeof(timeoutstr), "%d",
			 kingres_shard_node_connect_timeout);

	keywords[n] = "host";
	values[n++] = host;
	keywords[n] = "port";
	values[n++] = portstr;
	keywords[n] = "dbname";
	values[n++] = get_database_name(MyDatabaseId);
	keywords[n] = "user";
	values[n++] = user;
	keywords[n] = "connect_timeout";
	values[n++] = timeoutstr;
	keywords[n] = "application_name";
	values[n++] = "kingres_shard";
	keywords[n] = "client_encoding";
	values[n++] = GetDatabaseEncodingName();
	keywords[n] = "options";
	values[n++] = "-c kingres_shard.forwarded=on -c DateStyle=ISO,MDY "
		"-c IntervalStyle=postgres -c extra_float_digits=3 "
		"-c standard_conforming_strings=on -c bytea_output=hex "
		"-c search_path=pg_catalog";
	keywords[n] = NULL;
	values[n] = NULL;

	conn = PQconnectdbParams(keywords, values, false);
	if (conn == NULL || PQstatus(conn) != CONNECTION_OK)
	{
		*errmsg_out = pstrdup(conn ? PQerrorMessage(conn) : "out of memory");
		if (conn)
			PQfinish(conn);
		return NULL;
	}
	PQsetNoticeReceiver(conn, ignore_notice, NULL);
	return conn;
}

/*
 * kingres_shard_get_conn
 *
 * Return the cached connection to host:port for the current user,
 * connecting (or reconnecting a dead idle connection) when needed.  When the
 * connection cannot be made: with missing_ok NULL is returned and *errmsg
 * describes the reason, otherwise an error is raised.
 *
 * A connection that is part of the current remote transaction is never
 * silently replaced, because that would lose the transaction.
 */
KsConn *
kingres_shard_get_conn(const char *host, int port, bool missing_ok,
						char **errmsg_out)
{
	KsConn		key;
	KsConn	   *kc;
	bool		found;
	char	   *err = NULL;
	const char *user = GetUserNameFromId(GetUserId(), false);

	if (conn_hash == NULL)
	{
		HASHCTL		ctl;

		memset(&ctl, 0, sizeof(ctl));
		ctl.keysize = KS_CONN_KEY_SIZE;
		ctl.entrysize = sizeof(KsConn);
		ctl.hcxt = TopMemoryContext;
		conn_hash = hash_create("kingres_shard connections", 16, &ctl,
								HASH_ELEM | HASH_BLOBS | HASH_CONTEXT);
	}

	memset(&key, 0, sizeof(key));
	strlcpy(key.host, host, sizeof(key.host));
	key.port = port;
	strlcpy(key.user, user, sizeof(key.user));

	kc = hash_search(conn_hash, &key, HASH_ENTER, &found);
	if (!found)
	{
		kc->conn = NULL;
		kc->xact_depth = 0;
		kc->nstatements = 0;
		kc->in_query = false;
		kc->have_error = false;
		kc->tz_applied[0] = '\0';
	}

	/*
	 * An idle cached connection may have been closed by the other side (a
	 * restarted node).  libpq sockets are non-blocking, so reading what is
	 * pending detects the closed socket without waiting.
	 */
	if (kc->conn != NULL && kc->xact_depth == 0 && !kc->in_query)
	{
		/*
		 * A server that shuts down sends a FATAL message and then closes the
		 * socket: the first read takes the message, the second sees the end
		 * of the stream and marks the connection bad.
		 */
		(void) PQconsumeInput(kc->conn);
		if (PQstatus(kc->conn) == CONNECTION_OK)
			(void) PQconsumeInput(kc->conn);
	}

	if (kc->conn != NULL && kc->xact_depth == 0 &&
		(PQstatus(kc->conn) != CONNECTION_OK || kc->have_error ||
		 PQtransactionStatus(kc->conn) != PQTRANS_IDLE))
		discard_connection(kc);

	if (kc->conn == NULL)
	{
		kc->conn = connect_to_node(kc->host, kc->port, kc->user, &err);
		kc->have_error = false;
		if (kc->conn == NULL)
		{
			if (missing_ok)
			{
				if (errmsg_out)
					*errmsg_out = err;
				return NULL;
			}
			ereport(ERROR,
					(errcode(ERRCODE_SQLCLIENT_UNABLE_TO_ESTABLISH_SQLCONNECTION),
					 errmsg("could not connect to node %s:%d", host, port),
					 errdetail_internal("%s", err)));
		}
	}
	return kc;
}

/*
 * Close a connection and forget its transaction state.
 */
static void
discard_connection(KsConn *kc)
{
	if (kc->conn != NULL)
		PQfinish(kc->conn);
	kc->conn = NULL;
	kc->xact_depth = 0;
	kc->nstatements = 0;
	kc->tz_applied[0] = '\0';
	kc->in_query = false;
	kc->have_error = false;
	if (write_conn == kc)
		write_conn = NULL;
}

/*
 * kingres_shard_forget_xact
 *
 * Forget that a remote transaction was open on this connection, after the
 * caller rolled it back itself (for example to retry a statement).
 */
void
kingres_shard_forget_xact(KsConn *kc)
{
	kc->xact_depth = 0;
	kc->nstatements = 0;
	kc->tz_applied[0] = '\0';
	if (write_conn == kc)
		write_conn = NULL;
}

/*
 * kingres_shard_begin_xact
 *
 * Start the remote transaction, if not started, with the isolation level and
 * access mode of the local one, and open savepoints to match the local
 * subtransaction nesting (as postgres_fdw does).
 */
void
kingres_shard_begin_xact(KsConn *kc)
{
	int			curlevel = GetCurrentTransactionNestLevel();

	if (kc->have_error || kc->conn == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("connection to node %s:%d is in an unknown state",
						kc->host, kc->port),
				 errhint("Roll back the transaction and try again.")));

	if (kc->xact_depth <= 0)
	{
		const char *level;
		StringInfoData sql;

		if (IsolationIsSerializable())
			level = "SERIALIZABLE";
		else if (IsolationUsesXactSnapshot())
			level = "REPEATABLE READ";
		else
			level = "READ COMMITTED";

		/*
		 * The settings go with BEGIN in one round trip.  SET LOCAL ends with
		 * the transaction, so the next transaction starts from the
		 * connection's own (identical) settings again.
		 */
		initStringInfo(&sql);
		appendStringInfo(&sql, "BEGIN ISOLATION LEVEL %s%s; %s", level,
						 XactReadOnly ? " READ ONLY" : "",
						 KS_FIXED_SETTINGS_SQL);
		kc->tz_applied[0] = '\0';
		append_timezone_setting(&sql);
		exec_internal(kc, sql.data);
		pfree(sql.data);
		remember_timezone(kc);
		kc->xact_depth = 1;
		kc->nstatements = 0;
	}

	while (kc->xact_depth < curlevel)
	{
		char		sql[64];

		snprintf(sql, sizeof(sql), "SAVEPOINT s%d", kc->xact_depth + 1);
		exec_internal(kc, sql);
		kc->xact_depth++;
	}

	/* The local TimeZone may have changed since the last statement. */
	sync_timezone(kc);
}

/*
 * Append "SET LOCAL TimeZone = '<the local TimeZone>'" to buf.
 */
static void
append_timezone_setting(StringInfo buf)
{
	const char *tz = GetConfigOption("TimeZone", false, false);

	appendStringInfo(buf, "SET LOCAL TimeZone = %s",
					 quote_literal_cstr(tz ? tz : "UTC"));
}

/*
 * Remember which TimeZone the open remote transaction has.  A name that does
 * not fit is not remembered, which makes it be sent again every time.
 */
static void
remember_timezone(KsConn *kc)
{
	const char *tz = GetConfigOption("TimeZone", false, false);

	if (tz != NULL && strlen(tz) < sizeof(kc->tz_applied))
		strlcpy(kc->tz_applied, tz, sizeof(kc->tz_applied));
	else
		kc->tz_applied[0] = '\0';
}

/*
 * sync_timezone
 *
 * Make the remote transaction use the TimeZone of the local session now, if
 * the local session changed it since the last statement (a SET, or a
 * SET LOCAL, in the middle of a transaction).
 */
static void
sync_timezone(KsConn *kc)
{
	const char *tz = GetConfigOption("TimeZone", false, false);
	StringInfoData sql;

	if (tz != NULL && kc->tz_applied[0] != '\0' &&
		strcmp(tz, kc->tz_applied) == 0)
		return;
	initStringInfo(&sql);
	append_timezone_setting(&sql);
	exec_internal(kc, sql.data);
	pfree(sql.data);
	remember_timezone(kc);
}

/*
 * kingres_shard_set_transmission_modes
 *
 * Force the settings that make the text form of values unambiguous to the
 * remote server, as contrib/postgres_fdw's set_transmission_modes() does
 * (PostgreSQL License; Copyright (c) 2012-2025, PostgreSQL Global
 * Development Group), plus standard_conforming_strings and bytea_output,
 * which also change how a literal is written.  search_path is pg_catalog so
 * that the deparser schema-qualifies every name.  Used around the deparse of
 * a statement, around the output of its parameters, and around running a
 * deparsed statement locally again.  Returns the nest level to give to
 * kingres_shard_reset_transmission_modes(); on an error the abort undoes
 * the settings.
 */
int
kingres_shard_set_transmission_modes(void)
{
	int			nestlevel = NewGUCNestLevel();

	(void) set_config_option("datestyle", "ISO", PGC_USERSET, PGC_S_SESSION,
							 GUC_ACTION_SAVE, true, 0, false);
	(void) set_config_option("intervalstyle", "postgres", PGC_USERSET,
							 PGC_S_SESSION, GUC_ACTION_SAVE, true, 0, false);
	(void) set_config_option("extra_float_digits", "3", PGC_USERSET,
							 PGC_S_SESSION, GUC_ACTION_SAVE, true, 0, false);
	(void) set_config_option("standard_conforming_strings", "on", PGC_USERSET,
							 PGC_S_SESSION, GUC_ACTION_SAVE, true, 0, false);
	(void) set_config_option("bytea_output", "hex", PGC_USERSET,
							 PGC_S_SESSION, GUC_ACTION_SAVE, true, 0, false);
	(void) set_config_option("search_path", "pg_catalog", PGC_USERSET,
							 PGC_S_SESSION, GUC_ACTION_SAVE, true, 0, false);
	return nestlevel;
}

/*
 * Undo the effects of kingres_shard_set_transmission_modes().
 */
void
kingres_shard_reset_transmission_modes(int nestlevel)
{
	AtEOXact_GUC(true, nestlevel);
}

/*
 * exec_internal
 *
 * Run a command that returns no rows, with no statement accounting.
 */
static void
exec_internal(KsConn *kc, const char *sql)
{
	PGresult   *res;

	kc->in_query = true;
	if (!PQsendQuery(kc->conn, sql))
	{
		kc->have_error = true;
		kc->in_query = false;
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not send a command to node %s:%d",
						kc->host, kc->port),
				 errdetail_internal("%s", pchomp(PQerrorMessage(kc->conn)))));
	}
	res = libpqsrv_get_result_last(kc->conn, PG_WAIT_EXTENSION);
	kc->in_query = false;
	if (res == NULL || (PQresultStatus(res) != PGRES_COMMAND_OK &&
						PQresultStatus(res) != PGRES_TUPLES_OK))
		kingres_shard_report_error(kc, res, sql);
	PQclear(res);
}

/*
 * kingres_shard_exec
 *
 * Run one command (or several, separated by semicolons) on the node and
 * return the last result, which the caller must PQclear.  Any error from the
 * remote side is raised locally with the remote SQLSTATE and message.
 */
PGresult *
kingres_shard_exec(KsConn *kc, const char *sql)
{
	PGresult   *res;

	kc->in_query = true;
	if (!PQsendQuery(kc->conn, sql))
	{
		kc->have_error = true;
		kc->in_query = false;
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not send a command to node %s:%d",
						kc->host, kc->port),
				 errdetail_internal("%s", pchomp(PQerrorMessage(kc->conn)))));
	}
	res = libpqsrv_get_result_last(kc->conn, PG_WAIT_EXTENSION);
	kc->in_query = false;
	kc->nstatements++;
	if (res == NULL || (PQresultStatus(res) != PGRES_COMMAND_OK &&
						PQresultStatus(res) != PGRES_TUPLES_OK))
		kingres_shard_report_error(kc, res, sql);
	return res;
}

/*
 * kingres_shard_exec_params
 *
 * Run one statement with text-format parameters (types are inferred by the
 * remote parser).  With throw_on_error false the caller gets the result even
 * when it is an error, and must inspect and PQclear it.
 */
PGresult *
kingres_shard_exec_params(KsConn *kc, const char *sql, int nparams,
						   const Oid *types,
						   const char *const *values, bool throw_on_error)
{
	PGresult   *res;

	kc->in_query = true;
	if (!PQsendQueryParams(kc->conn, sql, nparams, types, values, NULL, NULL, 0))
	{
		kc->have_error = true;
		kc->in_query = false;
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not send a command to node %s:%d",
						kc->host, kc->port),
				 errdetail_internal("%s", pchomp(PQerrorMessage(kc->conn)))));
	}
	res = libpqsrv_get_result_last(kc->conn, PG_WAIT_EXTENSION);
	kc->in_query = false;
	kc->nstatements++;
	if (throw_on_error && (res == NULL ||
						   (PQresultStatus(res) != PGRES_COMMAND_OK &&
							PQresultStatus(res) != PGRES_TUPLES_OK)))
		kingres_shard_report_error(kc, res, sql);
	return res;
}

/*
 * kingres_shard_result_sqlstate
 *
 * The SQLSTATE of an error result as an int, or 0 when it has none.
 */
int
kingres_shard_result_sqlstate(PGresult *res)
{
	char	   *s;

	if (res == NULL)
		return 0;
	s = PQresultErrorField(res, PG_DIAG_SQLSTATE);
	if (s == NULL || strlen(s) != 5)
		return 0;
	return MAKE_SQLSTATE(s[0], s[1], s[2], s[3], s[4]);
}

/*
 * kingres_shard_report_error
 *
 * Raise the error held by res (which is cleared) as a local ERROR, keeping
 * the remote SQLSTATE, message, detail and hint, so that an error on a
 * forwarded statement looks like the error of a local one.  A lost
 * connection marks the cache entry so it is not reused.
 */
void
kingres_shard_report_error(KsConn *kc, PGresult *res, const char *sql)
{
	int			sqlstate;
	char	   *primary = NULL;
	char	   *detail = NULL;
	char	   *hint = NULL;
	char	   *context = NULL;
	const char *s;

	if (res != NULL)
	{
		sqlstate = kingres_shard_result_sqlstate(res);
		if ((s = PQresultErrorField(res, PG_DIAG_MESSAGE_PRIMARY)) != NULL)
			primary = pstrdup(s);
		if ((s = PQresultErrorField(res, PG_DIAG_MESSAGE_DETAIL)) != NULL)
			detail = pstrdup(s);
		if ((s = PQresultErrorField(res, PG_DIAG_MESSAGE_HINT)) != NULL)
			hint = pstrdup(s);
		if ((s = PQresultErrorField(res, PG_DIAG_CONTEXT)) != NULL)
			context = pstrdup(s);
		PQclear(res);
	}
	else
		sqlstate = 0;

	if (sqlstate == 0)
		sqlstate = ERRCODE_CONNECTION_FAILURE;
	if (PQstatus(kc->conn) != CONNECTION_OK)
		kc->have_error = true;
	if (primary == NULL)
		primary = pchomp(PQerrorMessage(kc->conn));

	ereport(ERROR,
			(errcode(sqlstate),
			 errmsg_internal("%s", primary),
			 detail ? errdetail_internal("%s", detail) : 0,
			 hint ? errhint("%s", hint) : 0,
			 context ? errcontext("%s", context) : 0,
			 errcontext("remote node %s:%d", kc->host, kc->port)));
	(void) sql;
}

/*
 * kingres_shard_note_remote_write
 *
 * Enforce the phase 1 limits for statements forwarded as writes: a
 * transaction may write on at most one remote shard group and may not write
 * both locally and remotely, because without two-phase commit such a
 * transaction could not be made atomic.
 */
void
kingres_shard_note_remote_write(KsConn *kc)
{
	TransactionId xid = GetTopTransactionIdIfAny();

	if (local_write_seen ||
		(TransactionIdIsValid(xid) && !TransactionIdEquals(xid, ignored_xid)))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("writing locally and on another shard group in one transaction is not supported yet"),
				 errhint("Write on one shard group per transaction until distributed transactions are implemented.")));
	if (write_conn != NULL && write_conn != kc)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("writing on more than one remote shard group in one transaction is not supported yet"),
				 errhint("Write on one shard group per transaction until distributed transactions are implemented.")));
	write_conn = kc;
}

/*
 * kingres_shard_note_local_write
 *
 * The executor hook calls this for local DML: it counts as a local write
 * even if the transaction id was already assigned by a catalog reload.
 */
void
kingres_shard_note_local_write(void)
{
	local_write_seen = true;
}

/*
 * kingres_shard_ignore_current_xid
 *
 * Called after a catalog reload that started with no transaction id: the id
 * the reload assigned does not mean that the user wrote locally.
 */
void
kingres_shard_ignore_current_xid(void)
{
	ignored_xid = GetTopTransactionIdIfAny();
}

/*
 * kingres_shard_remote_write_pending
 *
 * True when this transaction has already written on a remote group; local
 * writes are then refused (see the executor hook in remote_scan.c).
 */
bool
kingres_shard_remote_write_pending(void)
{
	return write_conn != NULL;
}

/*
 * Roll back whatever is open on a connection while the local transaction is
 * aborting.  This runs inside an abort callback, so it must not throw: a
 * connection that cannot be cleaned up cheaply is closed instead.  A query
 * that is still running is cancelled first, so the remote backend stops and
 * rolls back by itself when the socket is closed.
 */
static void
abort_connection(KsConn *kc)
{
	if (kc->conn == NULL)
		return;

	if (kc->in_query)
	{
		(void) libpqsrv_cancel(kc->conn,
							   TimestampTzPlusMilliseconds(GetCurrentTimestamp(), 5000));
		discard_connection(kc);
		return;
	}
	if (kc->have_error || PQstatus(kc->conn) != CONNECTION_OK)
	{
		discard_connection(kc);
		return;
	}
	if (PQtransactionStatus(kc->conn) != PQTRANS_IDLE)
	{
		PGresult   *res = PQexec(kc->conn, "ROLLBACK TRANSACTION");

		if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
		{
			PQclear(res);
			discard_connection(kc);
			return;
		}
		PQclear(res);
	}
	kc->xact_depth = 0;
	kc->nstatements = 0;
	kc->tz_applied[0] = '\0';
}

/*
 * conn_xact_callback
 *
 * Commit remote transactions before the local commit (so that a remote
 * failure aborts the local transaction), roll them back on abort, and refuse
 * PREPARE TRANSACTION while a remote transaction is open.
 */
static void
conn_xact_callback(XactEvent event, void *arg)
{
	HASH_SEQ_STATUS scan;
	KsConn	   *kc;

	/* The per-transaction write tracking is reset whatever the connections. */
	if (event == XACT_EVENT_COMMIT || event == XACT_EVENT_PARALLEL_COMMIT ||
		event == XACT_EVENT_PREPARE || event == XACT_EVENT_ABORT ||
		event == XACT_EVENT_PARALLEL_ABORT)
	{
		write_conn = NULL;
		ignored_xid = InvalidTransactionId;
		local_write_seen = false;
	}

	if (conn_hash == NULL)
		return;

	switch (event)
	{
		case XACT_EVENT_PARALLEL_PRE_COMMIT:
		case XACT_EVENT_PRE_COMMIT:
			hash_seq_init(&scan, conn_hash);
			while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
			{
				if (kc->xact_depth <= 0)
					continue;
				if (kc->xact_depth > 1)
					elog(ERROR, "unexpected nested remote transaction at commit");
				exec_internal(kc, "COMMIT TRANSACTION");
				kc->xact_depth = 0;
				kc->nstatements = 0;
				kc->tz_applied[0] = '\0';
			}
			break;

		case XACT_EVENT_PRE_PREPARE:
			hash_seq_init(&scan, conn_hash);
			while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
			{
				if (kc->xact_depth > 0)
				{
					hash_seq_term(&scan);
					ereport(ERROR,
							(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
							 errmsg("cannot PREPARE a transaction that has operated on remote nodes")));
				}
			}
			break;

		case XACT_EVENT_COMMIT:
		case XACT_EVENT_PARALLEL_COMMIT:
		case XACT_EVENT_PREPARE:
			write_conn = NULL;
			ignored_xid = InvalidTransactionId;
			local_write_seen = false;
			break;

		case XACT_EVENT_ABORT:
		case XACT_EVENT_PARALLEL_ABORT:
			hash_seq_init(&scan, conn_hash);
			while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
				abort_connection(kc);
			write_conn = NULL;
			ignored_xid = InvalidTransactionId;
			local_write_seen = false;
			break;
	}
}

/*
 * conn_subxact_callback
 *
 * Mirror the end of a local subtransaction on every connection that has a
 * savepoint for it: release on commit, roll back to it on abort.
 */
static void
conn_subxact_callback(SubXactEvent event, SubTransactionId mySubid,
					  SubTransactionId parentSubid, void *arg)
{
	HASH_SEQ_STATUS scan;
	KsConn	   *kc;
	int			curlevel;

	if (conn_hash == NULL)
		return;
	if (event != SUBXACT_EVENT_COMMIT_SUB && event != SUBXACT_EVENT_ABORT_SUB)
		return;

	curlevel = GetCurrentTransactionNestLevel();
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		char		sql[64];

		if (kc->xact_depth < curlevel)
			continue;
		if (kc->xact_depth > curlevel)
			elog(ERROR, "unexpected remote savepoint depth");

		if (event == SUBXACT_EVENT_COMMIT_SUB)
		{
			snprintf(sql, sizeof(sql), "RELEASE SAVEPOINT s%d", curlevel);
			exec_internal(kc, sql);
		}
		else
		{
			PGresult   *res;

			/* A SET LOCAL made after the savepoint is undone with it. */
			kc->tz_applied[0] = '\0';
			if (kc->in_query || kc->have_error || kc->conn == NULL ||
				PQstatus(kc->conn) != CONNECTION_OK)
			{
				kc->have_error = true;
				continue;
			}
			snprintf(sql, sizeof(sql), "ROLLBACK TO SAVEPOINT s%d", curlevel);
			res = PQexec(kc->conn, sql);
			if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
				kc->have_error = true;
			PQclear(res);
			if (!kc->have_error)
			{
				snprintf(sql, sizeof(sql), "RELEASE SAVEPOINT s%d", curlevel);
				res = PQexec(kc->conn, sql);
				if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
					kc->have_error = true;
				PQclear(res);
			}
		}
		kc->xact_depth--;
	}
}
