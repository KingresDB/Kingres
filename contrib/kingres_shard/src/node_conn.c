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
 * Commit (design/shard-phase2a.md, section 1).  A transaction with at most one
 * writer commits as in phase 1: remote transactions are committed in the
 * pre-commit callback, before the local commit.  A transaction with two or
 * more writers (each remote connection that ran a writing statement, plus the
 * local node when the local transaction has a transaction id) uses two-phase
 * commit:
 *
 *	1. pre-commit: every remote writer runs PREPARE TRANSACTION '<gid>';
 *	2. pre-commit: one row per prepared remote is inserted into the node-local
 *	   table kingres.dtx_log by the local transaction itself, so the row is
 *	   durable exactly when the local commit is; its presence is the decision
 *	   to commit;
 *	3. after the local commit: COMMIT PREPARED on every remote.  A failure here
 *	   cannot abort anything: it is a warning, and recovery.c finishes the job.
 *
 * If anything fails before the local commit, the abort callback rolls back
 * what was prepared (ROLLBACK PREPARED) and what was not (ROLLBACK); a
 * prepared transaction whose origin died is resolved by recovery.c.
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

#include "access/htup_details.h"
#include "access/table.h"
#include "access/xact.h"
#include "catalog/indexing.h"
#include "commands/dbcommands.h"
#include "libpq/libpq-be-fe-helpers.h"
#include "storage/latch.h"
#include "mb/pg_wchar.h"
#include "miscadmin.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/hsearch.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"
#include "utils/timestamp.h"
#include "utils/wait_classes.h"

#include "catalog.h"
#include "forward_auth.h"
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

/* Connection keywords the extension controls; node_conninfo may not set them. */
static const char *const forbidden_conninfo_keys[] = {
	"host", "hostaddr", "port", "dbname", "user", "password", "options",
	"service", "client_encoding", "application_name", "connect_timeout",
	"replication", "sslpassword", NULL
};

/*
 * Connection keywords whose values are paths of key material or secrets: they
 * are removed from the text of a connection error.
 */
static const char *const secret_conninfo_keys[] = {
	"sslcert", "sslkey", "sslrootcert", "sslcrl", "sslcrldir", "sslpassword",
	"sslkeylogfile", "passfile", "ssl_min_protocol_version", NULL
};

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
 * kingres_shard_check_node_conninfo
 *
 * Check hook of kingres_shard.node_conninfo: the value must parse as a
 * connection string and must not set a keyword that the extension controls.
 * The messages never repeat the value (it may name key files); an invalid
 * value is also replaced by a placeholder, because the generic GUC error
 * message prints it.
 */
bool
kingres_shard_check_node_conninfo(char **newval, void **extra, GucSource source)
{
	PQconninfoOption *opts;
	PQconninfoOption *o;
	const char *bad = NULL;

	if (*newval == NULL || **newval == '\0')
		return true;

	opts = PQconninfoParse(*newval, NULL);
	if (opts == NULL)
	{
		/*
		 * A string that does not parse (for example a service that does not
		 * exist here) may still name a forbidden keyword: say which.
		 */
		int			k;

		bad = "";
		for (k = 0; forbidden_conninfo_keys[k] != NULL; k++)
		{
			const char *p = *newval;
			size_t		len = strlen(forbidden_conninfo_keys[k]);

			while ((p = strstr(p, forbidden_conninfo_keys[k])) != NULL)
			{
				const char *after = p + len;

				while (*after == ' ')
					after++;
				if ((p == *newval || p[-1] == ' ' || p[-1] == 0x09) && *after == '=')
				{
					bad = forbidden_conninfo_keys[k];
					break;
				}
				p += len;
			}
			if (bad[0] != 0)
				break;
		}
	}
	else
	{
		for (o = opts; o->keyword != NULL && bad == NULL; o++)
		{
			int			k;

			if (o->val == NULL || o->val[0] == '\0')
				continue;
			for (k = 0; forbidden_conninfo_keys[k] != NULL; k++)
				if (strcmp(o->keyword, forbidden_conninfo_keys[k]) == 0)
					bad = forbidden_conninfo_keys[k];
		}
		PQconninfoFree(opts);
	}

	if (bad == NULL)
		return true;

	if (bad[0] == '\0')
		GUC_check_errdetail("The value is not a valid connection string.");
	else
		GUC_check_errdetail("The keyword \"%s\" is controlled by the extension and cannot be set here.", bad);
	GUC_check_errhint("Use keywords such as sslmode, sslcert, sslkey and sslrootcert; host, port, dbname, user, password and options are not allowed.");
	/* The generic error message prints the value: do not let it. */
	guc_free(*newval);
	*newval = guc_strdup(LOG, "<hidden>");
	return false;
}

/*
 * kingres_shard_scrub_conn_error
 *
 * The text of a libpq connection error with the values of the key and
 * certificate paths of node_conninfo replaced by "<hidden>", so that a path
 * to a private key never reaches an error message or the log.  The result is
 * palloc'd.
 */
char *
kingres_shard_scrub_conn_error(const char *msg)
{
	char	   *result = pstrdup(msg ? msg : "");
	size_t		rlen = strlen(result);
	PQconninfoOption *opts;
	PQconninfoOption *o;

	/* libpq ends its messages with a newline. */
	while (rlen > 0 && (result[rlen - 1] == '\n' || result[rlen - 1] == '\r'))
		result[--rlen] = '\0';
	if (kingres_shard_node_conninfo == NULL || *kingres_shard_node_conninfo == '\0')
		return result;
	opts = PQconninfoParse(kingres_shard_node_conninfo, NULL);
	if (opts == NULL)
		return result;
	for (o = opts; o->keyword != NULL; o++)
	{
		int			k;

		if (o->val == NULL || o->val[0] == '\0')
			continue;
		for (k = 0; secret_conninfo_keys[k] != NULL; k++)
		{
			if (strcmp(o->keyword, secret_conninfo_keys[k]) == 0)
			{
				StringInfoData buf;
				const char *p = result;
				const char *hit;

				initStringInfo(&buf);
				while ((hit = strstr(p, o->val)) != NULL)
				{
					appendBinaryStringInfo(&buf, p, hit - p);
					appendStringInfoString(&buf, "<hidden>");
					p = hit + strlen(o->val);
				}
				appendStringInfoString(&buf, p);
				pfree(result);
				result = buf.data;
			}
		}
	}
	PQconninfoFree(opts);
	return result;
}

/* The nodes that add_node() is about to register: their ids are not in the catalog yet. */
#define KS_MAX_EXPECTED 4
static struct
{
	char		host[KS_CONN_HOST_LEN];
	int			port;
	int			node_id;
} expected_nodes[KS_MAX_EXPECTED];
static int	nexpected = 0;

void
kingres_shard_conn_expect_node(const char *host, int port, int node_id)
{
	int			i;

	for (i = 0; i < nexpected; i++)
		if (expected_nodes[i].port == port && strcmp(expected_nodes[i].host, host) == 0)
			break;
	if (i == nexpected)
	{
		if (nexpected == KS_MAX_EXPECTED)
			i = --nexpected;
		nexpected++;
	}
	strlcpy(expected_nodes[i].host, host, sizeof(expected_nodes[i].host));
	expected_nodes[i].port = port;
	expected_nodes[i].node_id = node_id;
}

/* The id of the node a connection goes to, as the node knows itself. */
static int
target_node_id(const char *host, int port)
{
	int			id = kingres_shard_conn_node_id(host, port);
	int			i;

	if (id != 0)
		return id;
	for (i = 0; i < nexpected; i++)
		if (expected_nodes[i].port == port && strcmp(expected_nodes[i].host, host) == 0)
			return expected_nodes[i].node_id;
	return 0;
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
	const char *keywords[64];
	const char *values[64];
	char		portstr[16];
	char		timeoutstr[16];
	char		keepidlestr[16];
	char		keepintstr[16];
	char		usertimeoutstr[32];
	PGconn	   *conn;
	int			n = 0;
	PQconninfoOption *extra = NULL;
	char	   *token;
	char	   *options;

	snprintf(portstr, sizeof(portstr), "%d", port);
	snprintf(timeoutstr, sizeof(timeoutstr), "%d",
			 kingres_shard_node_connect_timeout);

	/*
	 * A node that stops answering without closing the connection (a partition,
	 * a stopped machine) is found out by the keepalives and by the limit on
	 * unacknowledged data, within kingres_shard.node_timeout (S2-12).
	 */
	snprintf(keepidlestr, sizeof(keepidlestr), "%d", Max(kingres_shard_node_timeout / 3, 1));
	snprintf(keepintstr, sizeof(keepintstr), "%d", Max(kingres_shard_node_timeout / 6, 1));
	snprintf(usertimeoutstr, sizeof(usertimeoutstr), "%d", kingres_shard_node_timeout * 1000);

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
	keywords[n] = "keepalives";
	values[n++] = "1";
	keywords[n] = "keepalives_idle";
	values[n++] = keepidlestr;
	keywords[n] = "keepalives_interval";
	values[n++] = keepintstr;
	keywords[n] = "keepalives_count";
	values[n++] = "3";
	keywords[n] = "tcp_user_timeout";
	values[n++] = usertimeoutstr;
	keywords[n] = "application_name";
	values[n++] = "kingres_shard";
	keywords[n] = "client_encoding";
	values[n++] = GetDatabaseEncodingName();
	/*
	 * The token proves to the other node that this session comes from a node
	 * of the cluster (forward_auth.c): it is made for this role and for the id
	 * of the node that receives it.
	 */
	token = kingres_shard_forward_token(user, target_node_id(host, port));
	options = psprintf("-c kingres_shard.forwarded=on%s%s -c DateStyle=ISO,MDY "
					   "-c IntervalStyle=postgres -c extra_float_digits=3 "
					   "-c standard_conforming_strings=on -c bytea_output=hex "
					   "-c search_path=pg_catalog",
					   token ? " -c kingres_shard.forward_token=" : "", token ? token : "");
	keywords[n] = "options";
	values[n++] = options;

	/*
	 * The administrator's connection options (sslmode, sslcert, ...) come
	 * last.  The setting was validated when it was set: it cannot name a
	 * keyword used above, so nothing the extension controls is overridden.
	 */
	if (kingres_shard_node_conninfo != NULL && *kingres_shard_node_conninfo != '\0')
	{
		PQconninfoOption *o;

		extra = PQconninfoParse(kingres_shard_node_conninfo, NULL);
		if (extra != NULL)
		{
			for (o = extra; o->keyword != NULL; o++)
			{
				int			k;
				bool		forbidden = false;

				if (o->val == NULL || o->val[0] == '\0')
					continue;
				for (k = 0; forbidden_conninfo_keys[k] != NULL; k++)
					if (strcmp(o->keyword, forbidden_conninfo_keys[k]) == 0)
						forbidden = true;
				if (forbidden || n >= 62)
					continue;
				keywords[n] = o->keyword;
				values[n++] = o->val;
			}
		}
	}
	keywords[n] = NULL;
	values[n] = NULL;

	conn = PQconnectdbParams(keywords, values, false);
	if (token != NULL)
		explicit_bzero(options, strlen(options));
	if (extra != NULL)
		PQconninfoFree(extra);
	if (conn == NULL || PQstatus(conn) != CONNECTION_OK)
	{
		*errmsg_out = conn ? kingres_shard_scrub_conn_error(PQerrorMessage(conn))
			: pstrdup("out of memory");
		if (conn)
			PQfinish(conn);
		return NULL;
	}
	PQsetNoticeReceiver(conn, ignore_notice, NULL);
	return conn;
}

/*
 * kingres_shard_connect_control
 *
 * See node_conn.h.
 */
PGconn *
kingres_shard_connect_control(const char *host, int port, char **errmsg_out)
{
	return connect_to_node(host, port, GetUserNameFromId(GetUserId(), false),
						   errmsg_out);
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
		kc->wrote = false;
		kc->prepared = false;
		kc->gid[0] = '\0';
		kc->pending_owner = NULL;
		kc->pending_finish = NULL;
	}

	/* An asynchronous fetch of a scan must finish before the connection is used. */
	kingres_shard_conn_quiesce(kc);

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
	{
		/*
		 * PQfinish() sends a Terminate message; on a connection to a node that
		 * does not read, in the default blocking mode, flushing it waits until
		 * the node reads (S3-07).
		 */
		(void) PQsetnonblocking(kc->conn, 1);
		PQfinish(kc->conn);
	}
	kc->conn = NULL;
	kc->xact_depth = 0;
	kc->nstatements = 0;
	kc->tz_applied[0] = '\0';
	kc->in_query = false;
	kc->have_error = false;
	kc->wrote = false;
	kc->prepared = false;
	kc->pending_owner = NULL;
	kc->pending_finish = NULL;
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
	kc->wrote = false;
}

/*
 * Asynchronous fetches.  A scan that has sent a FETCH and has not read the
 * answer registers itself on the connection; anything else that wants to use
 * the connection first asks the owner to read the answer (it keeps the result
 * for later), so that two statements never overlap on one libpq connection.
 */
void
kingres_shard_conn_set_pending(KsConn *kc, void *owner, void (*finish) (void *owner))
{
	kc->pending_owner = owner;
	kc->pending_finish = finish;
}

void
kingres_shard_conn_clear_pending(KsConn *kc, void *owner)
{
	if (kc->pending_owner == owner)
	{
		kc->pending_owner = NULL;
		kc->pending_finish = NULL;
	}
}

void
kingres_shard_conn_quiesce(KsConn *kc)
{
	if (kc->pending_owner != NULL)
	{
		void	   *owner = kc->pending_owner;
		void		(*finish) (void *) = kc->pending_finish;

		kc->pending_owner = NULL;
		kc->pending_finish = NULL;
		finish(owner);
	}
}

/*
 * kingres_shard_conn_node_id
 *
 * The id of the registered node at host:port, or 0.
 */
int
kingres_shard_conn_node_id(const char *host, int port)
{
	const KsCatalog *cat = kingres_shard_catalog();
	ListCell   *lc;

	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);

		if (n->port == port && strcmp(n->host, host) == 0)
			return n->node_id;
	}
	return 0;
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

	kingres_shard_conn_quiesce(kc);

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
 * Ask the node to stop what it is doing, waiting for the cancel request to be
 * delivered for a bounded time only (review finding S3-07).  The legacy
 * PQcancel() connects and then waits for the postmaster to close the
 * connection with no timeout of its own: a node whose postmaster is stopped
 * (a frozen machine whose kernel still accepts connections) held the
 * committing backend for as long as it stayed stopped, although it had given
 * up on the node already.  This is the non-blocking cancel of libpq polled
 * against a deadline (as libpqsrv_cancel() does, which cannot be used here
 * because it checks for interrupts, and the callers run where an error is not
 * possible: the commit and abort callbacks).  The wait is a third of
 * node_timeout, at most node_connect_timeout, at least a second.
 */
void
kingres_shard_cancel_with_deadline(PGconn *conn)
{
	PGcancelConn *cc = PQcancelCreate(conn);
	TimestampTz endtime;

	if (cc == NULL)
		return;
	endtime = TimestampTzPlusMilliseconds(GetCurrentTimestamp(),
										  (int64) 1000 *
										  Max(1, Min(kingres_shard_node_connect_timeout,
													 kingres_shard_node_timeout / 3)));
	if (PQcancelStart(cc))
	{
		for (;;)
		{
			PostgresPollingStatusType pollres = PQcancelPoll(cc);
			long		remaining;
			int			events = WL_LATCH_SET | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH;

			if (pollres == PGRES_POLLING_OK || pollres == PGRES_POLLING_FAILED)
				break;
			remaining = TimestampDifferenceMilliseconds(GetCurrentTimestamp(), endtime);
			if (remaining <= 0)
				break;
			events |= (pollres == PGRES_POLLING_READING) ? WL_SOCKET_READABLE : WL_SOCKET_WRITEABLE;
			(void) WaitLatchOrSocket(MyLatch, events, PQcancelSocket(cc), remaining,
									 PG_WAIT_EXTENSION);
			ResetLatch(MyLatch);
		}
	}
	PQcancelFinish(cc);
}

/*
 * kingres_shard_bounded_exec
 *
 * PQexec without the unbounded wait: send the command, then wait on the latch
 * and the socket until the result is complete or kingres_shard.node_timeout
 * seconds have passed.  On a timeout the running command is cancelled
 * (best effort) and NULL is returned; the caller closes the connection.
 */
PGresult *
kingres_shard_bounded_exec(PGconn *conn, const char *sql, bool interruptible)
{
	TimestampTz deadline = TimestampTzPlusMilliseconds(GetCurrentTimestamp(),
													   (int64) kingres_shard_node_timeout * 1000);
	PGresult   *last = NULL;

	if (conn == NULL || PQstatus(conn) != CONNECTION_OK || !PQsendQuery(conn, sql))
		return NULL;
	for (;;)
	{
		PGresult   *res;

		while (PQisBusy(conn))
		{
			long		secs;
			int			usecs;
			long		ms;
			int			rc;

			TimestampDifference(GetCurrentTimestamp(), deadline, &secs, &usecs);
			ms = secs * 1000L + usecs / 1000;
			if (GetCurrentTimestamp() >= deadline || ms <= 0)
			{
				kingres_shard_cancel_with_deadline(conn);
				PQclear(last);
				return NULL;
			}
			rc = WaitLatchOrSocket(MyLatch,
								   WL_LATCH_SET | WL_SOCKET_READABLE | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH,
								   PQsocket(conn), Min(ms, 1000L), PG_WAIT_EXTENSION);
			if (rc & WL_LATCH_SET)
			{
				ResetLatch(MyLatch);
				if (interruptible)
					CHECK_FOR_INTERRUPTS();
			}
			if (!PQconsumeInput(conn))
			{
				PQclear(last);
				return NULL;
			}
		}
		res = PQgetResult(conn);
		if (res == NULL)
			break;
		PQclear(last);
		last = res;
		if (PQresultStatus(res) == PGRES_COPY_IN || PQresultStatus(res) == PGRES_COPY_OUT ||
			PQresultStatus(res) == PGRES_COPY_BOTH)
			break;
	}
	return last;
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

	kingres_shard_conn_quiesce(kc);
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

	kingres_shard_conn_quiesce(kc);
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

	kingres_shard_conn_quiesce(kc);
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

	/*
	 * A helper function of the extension that the other node does not have: its
	 * SQL objects are older than this node's library (a rolling upgrade that is
	 * not finished; review finding S4-05).  Say that, instead of passing on
	 * "function does not exist".
	 */
	if (sqlstate == ERRCODE_UNDEFINED_FUNCTION &&
		strstr(primary, "function " KINGRES_SHARD_SCHEMA ".internal_") != NULL)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("node %s:%d runs an older version of the kingres_shard SQL objects",
						kc->host, kc->port),
				 errdetail("%s", primary),
				 errhint("Run ALTER EXTENSION kingres_shard UPDATE on that node; every node of a cluster must run the same version.")));

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
 * The remote transaction of kc has run (or is about to run) a statement that
 * writes.  A transaction with two or more writers commits with two-phase
 * commit; there is no longer a limit on the number of writers.
 */
void
kingres_shard_note_remote_write(KsConn *kc)
{
	kc->wrote = true;
}

/*
 * kingres_shard_begin_write_xact
 *
 * kingres_shard_begin_xact() for a caller that will write.
 */
void
kingres_shard_begin_write_xact(KsConn *kc)
{
	kingres_shard_begin_xact(kc);
	kc->wrote = true;
}

/* Forget everything about the transaction that just ended on kc. */
static void
reset_xact_state(KsConn *kc)
{
	kc->xact_depth = 0;
	kc->nstatements = 0;
	kc->tz_applied[0] = '\0';
	kc->wrote = false;
	kc->prepared = false;
	kc->gid[0] = '\0';
}

/*
 * Roll back whatever is open on a connection while the local transaction is
 * aborting.  This runs inside an abort callback, so it must not throw: a
 * connection that cannot be cleaned up cheaply is closed instead.  A query
 * that is still running is cancelled first, so the remote backend stops and
 * rolls back by itself when the socket is closed.  A transaction that was
 * prepared is rolled back with ROLLBACK PREPARED; if that cannot be done now,
 * recovery (recovery.c) does it later, because no dtx_log row exists.
 */
static void
abort_connection(KsConn *kc)
{
	if (kc->conn == NULL)
	{
		reset_xact_state(kc);
		return;
	}

	if (kc->in_query)
	{
		(void) libpqsrv_cancel(kc->conn,
							   TimestampTzPlusMilliseconds(GetCurrentTimestamp(), 5000));
		if (kc->prepared)
			ereport(WARNING,
					(errmsg("could not roll back prepared transaction \"%s\" on node %s:%d; recovery will resolve it",
							kc->gid, kc->host, kc->port)));
		discard_connection(kc);
		reset_xact_state(kc);
		return;
	}
	if (kc->have_error || PQstatus(kc->conn) != CONNECTION_OK)
	{
		if (kc->prepared)
			ereport(WARNING,
					(errmsg("could not roll back prepared transaction \"%s\" on node %s:%d; recovery will resolve it",
							kc->gid, kc->host, kc->port)));
		discard_connection(kc);
		reset_xact_state(kc);
		return;
	}
	if (kc->prepared)
	{
		char		sql[KS_GID_LEN + 32];
		PGresult   *res;

		snprintf(sql, sizeof(sql), "ROLLBACK PREPARED '%s'", kc->gid);
		res = kingres_shard_bounded_exec(kc->conn, sql, false);
		if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
		{
			ereport(WARNING,
					(errmsg("could not roll back prepared transaction \"%s\" on node %s:%d; recovery will resolve it",
							kc->gid, kc->host, kc->port),
					 errdetail_internal("%s", res ? pchomp(PQerrorMessage(kc->conn)) : "no result")));
			PQclear(res);
			discard_connection(kc);
			reset_xact_state(kc);
			return;
		}
		PQclear(res);
	}
	else if (PQtransactionStatus(kc->conn) != PQTRANS_IDLE)
	{
		PGresult   *res = kingres_shard_bounded_exec(kc->conn, "ROLLBACK TRANSACTION", false);

		if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
		{
			PQclear(res);
			discard_connection(kc);
			reset_xact_state(kc);
			return;
		}
		PQclear(res);
	}
	reset_xact_state(kc);
}

/*
 * prepare_one
 *
 * PREPARE TRANSACTION on one remote writer.  A failure (a deferred constraint
 * that fails at prepare time, prepared transactions disabled, the limit
 * reached) aborts the local transaction; the abort callback rolls back the
 * others.
 */
static void
prepare_one(KsConn *kc)
{
	char		sql[KS_GID_LEN + 32];
	PGresult   *res;

	snprintf(sql, sizeof(sql), "PREPARE TRANSACTION '%s'", kc->gid);
	kingres_shard_conn_quiesce(kc);
	kc->in_query = true;
	if (!PQsendQuery(kc->conn, sql))
	{
		kc->have_error = true;
		kc->in_query = false;
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not send PREPARE TRANSACTION to node %s:%d",
						kc->host, kc->port),
				 errdetail_internal("%s", pchomp(PQerrorMessage(kc->conn)))));
	}
	res = libpqsrv_get_result_last(kc->conn, PG_WAIT_EXTENSION);
	kc->in_query = false;
	if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
	{
		int			sqlstate = kingres_shard_result_sqlstate(res);
		char	   *remote = (res != NULL && PQresultErrorField(res, PG_DIAG_MESSAGE_PRIMARY) != NULL) ?
			pstrdup(PQresultErrorField(res, PG_DIAG_MESSAGE_PRIMARY)) :
			pchomp(PQerrorMessage(kc->conn));

		if (PQstatus(kc->conn) != CONNECTION_OK)
			kc->have_error = true;
		PQclear(res);
		ereport(ERROR,
				(errcode(sqlstate != 0 ? sqlstate : ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not prepare the transaction on node %s:%d: %s",
						kc->host, kc->port, remote),
				 (sqlstate == ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE ||
				  sqlstate == ERRCODE_OUT_OF_MEMORY) ?
				 errhint("Two-phase commit needs max_prepared_transactions greater than zero on every node of the cluster, and enough of them for the transactions in flight.") : 0));
	}
	PQclear(res);
	kc->prepared = true;
	kc->xact_depth = 0;
}

/*
 * log_decision
 *
 * Insert one kingres.dtx_log row per prepared remote.  The local transaction
 * does it itself, so the rows become durable exactly when the local commit
 * does; their presence is the decision to commit.  The tuples are inserted
 * with the heap API, so no privilege on the table is needed by the user.
 */
static void
log_decision(void)
{
	Oid			relid = kingres_shard_catalog_relid("dtx_log");
	Relation	rel;
	HASH_SEQ_STATUS scan;
	KsConn	   *kc;
	TimestampTz now = GetCurrentTimestamp();

	if (!OidIsValid(relid))
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("the table kingres.dtx_log does not exist"),
				 errhint("Run ALTER EXTENSION kingres_shard UPDATE.")));
	/*
	 * The participants commit as soon as this transaction has, and their
	 * COMMIT PREPARED is always flushed: the local commit record, which makes
	 * the rows below durable, must be flushed first whatever
	 * synchronous_commit says (review finding S2-02).  ForceSyncCommit() flushes
	 * the record locally, but does not make the commit wait for synchronous
	 * standbys: the core asks for that only when synchronous_commit is above
	 * "local" (SyncRepRequested()).  The decision must survive a failover of
	 * this node as well as a crash of it, so for a transaction that prepared
	 * remote participants "off" and "local" are raised to "on" until it ends
	 * (S3-15).  Without synchronous standbys that costs nothing more than the
	 * flush.
	 */
	ForceSyncCommit();
	if (synchronous_commit <= SYNCHRONOUS_COMMIT_LOCAL_FLUSH)
		(void) set_config_option("synchronous_commit", "on", PGC_USERSET,
								 PGC_S_SESSION, GUC_ACTION_LOCAL, true, 0, false);

	/* The unique-index check of the insert needs a snapshot. */
	PushActiveSnapshot(GetTransactionSnapshot());
	rel = table_open(relid, RowExclusiveLock);
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		Datum		values[3];
		bool		nulls[3] = {false, false, false};
		HeapTuple	tup;

		if (!kc->prepared)
			continue;
		values[0] = CStringGetTextDatum(kc->gid);
		values[1] = Int32GetDatum(kingres_shard_conn_node_id(kc->host, kc->port));
		values[2] = TimestampTzGetDatum(now);
		tup = heap_form_tuple(RelationGetDescr(rel), values, nulls);
		CatalogTupleInsert(rel, tup);
		heap_freetuple(tup);
	}
	table_close(rel, NoLock);
	PopActiveSnapshot();
}

/*
 * precommit
 *
 * The first phase of the commit; see the file header.
 */
static void
precommit(bool parallel)
{
	HASH_SEQ_STATUS scan;
	KsConn	   *kc;
	int			nremote_writers = 0;
	bool		use_2pc;
	bool		local_writer;
	int			nprepared = 0;
	FullTransactionId fxid;
	int			origin;
	List	   *used_gids = NIL;

	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		if (kc->xact_depth > 0 && kc->wrote)
			nremote_writers++;
	}
	local_writer = !parallel && TransactionIdIsValid(GetTopTransactionIdIfAny());
	use_2pc = !parallel && nremote_writers >= 1 &&
		(nremote_writers + (local_writer ? 1 : 0)) >= 2;

	/*
	 * Read-only participants are committed first; so is the only writer when
	 * the transaction does not need two-phase commit.
	 */
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		if (kc->xact_depth <= 0)
			continue;
		if (kc->xact_depth > 1)
			elog(ERROR, "unexpected nested remote transaction at commit");
		if (kc->wrote)
			continue;
		exec_internal(kc, "COMMIT TRANSACTION");
		reset_xact_state(kc);
	}
	if (!use_2pc)
	{
		/*
		 * The one writer is committed after every reader: if a reader's commit
		 * fails, the client gets an error and nothing is durable (S2-19).
		 */
		hash_seq_init(&scan, conn_hash);
		while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
		{
			if (kc->xact_depth <= 0 || !kc->wrote)
				continue;
			exec_internal(kc, "COMMIT TRANSACTION");
			reset_xact_state(kc);
		}
		return;
	}

	/* The global transaction id needs the local transaction id. */
	(void) GetTopTransactionId();
	fxid = GetTopFullTransactionId();
	origin = kingres_shard_catalog()->local_node_id;

	kingres_shard_fault_point("before_prepare");
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		char	   *gid;
		int			target;

		if (kc->xact_depth <= 0 || !kc->wrote)
			continue;
		target = kingres_shard_conn_node_id(kc->host, kc->port);
		gid = psprintf("kingres_%d_%d_" UINT64_FORMAT "_%d", origin, MyProcPid,
					   U64FromFullTransactionId(fxid), target);
		/* Two connections (two roles) to one node need two names. */
		{
			ListCell   *lc;
			bool		dup = true;
			int			n = 0;

			while (dup)
			{
				dup = false;
				foreach(lc, used_gids)
				{
					if (strcmp((char *) lfirst(lc), gid) == 0)
					{
						dup = true;
						gid = psprintf("kingres_%d_%d_" UINT64_FORMAT "_%d_%d", origin,
									   MyProcPid, U64FromFullTransactionId(fxid),
									   target, ++n);
						break;
					}
				}
			}
		}
		used_gids = lappend(used_gids, gid);
		strlcpy(kc->gid, gid, sizeof(kc->gid));
		prepare_one(kc);
		nprepared++;
		if (nprepared == 1)
			kingres_shard_fault_point("after_first_prepare");
	}
	kingres_shard_fault_point("after_prepare");
	log_decision();
	kingres_shard_fault_point("after_log");
}

/*
 * postcommit
 *
 * The second phase: the local transaction committed, so the decision is
 * "commit"; tell every prepared remote.  Nothing may throw here: a remote that
 * cannot be reached keeps its prepared transaction, and recovery commits it
 * because the dtx_log row is durable.
 */
static void
postcommit(void)
{
	HASH_SEQ_STATUS scan;
	KsConn	   *kc;
	bool		first = true;

	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		char		sql[KS_GID_LEN + 32];
		PGresult   *res;

		if (!kc->prepared)
		{
			reset_xact_state(kc);
			continue;
		}
		if (first)
			kingres_shard_fault_point("after_commit");
		else
			kingres_shard_fault_point("between_commit_prepared");
		first = false;

		if (kc->conn == NULL || kc->have_error || PQstatus(kc->conn) != CONNECTION_OK)
		{
			ereport(WARNING,
					(errmsg("could not commit prepared transaction \"%s\" on node %s:%d; recovery will commit it",
							kc->gid, kc->host, kc->port)));
			discard_connection(kc);
			reset_xact_state(kc);
			continue;
		}
		snprintf(sql, sizeof(sql), "COMMIT PREPARED '%s'", kc->gid);
		res = kingres_shard_bounded_exec(kc->conn, sql, false);
		if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
		{
			ereport(WARNING,
					(errmsg("could not commit prepared transaction \"%s\" on node %s:%d; recovery will commit it",
							kc->gid, kc->host, kc->port),
					 errdetail_internal("%s", res ? pchomp(PQerrorMessage(kc->conn)) : "no result")));
			PQclear(res);
			discard_connection(kc);
		}
		else
			PQclear(res);
		reset_xact_state(kc);
	}
}

/*
 * conn_xact_callback
 *
 * Commit remote transactions before the local commit (so that a remote
 * failure aborts the local transaction), finish two-phase commit after it,
 * roll back on abort, and refuse PREPARE TRANSACTION while a remote
 * transaction is open.
 */
static void
conn_xact_callback(XactEvent event, void *arg)
{
	HASH_SEQ_STATUS scan;
	KsConn	   *kc;

	if (conn_hash == NULL)
		return;

	switch (event)
	{
		case XACT_EVENT_PARALLEL_PRE_COMMIT:
			precommit(true);
			break;

		case XACT_EVENT_PRE_COMMIT:
			precommit(false);
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
			postcommit();
			break;

		case XACT_EVENT_PARALLEL_COMMIT:
		case XACT_EVENT_PREPARE:
			hash_seq_init(&scan, conn_hash);
			while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
				reset_xact_state(kc);
			break;

		case XACT_EVENT_ABORT:
		case XACT_EVENT_PARALLEL_ABORT:
			hash_seq_init(&scan, conn_hash);
			while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
				abort_connection(kc);
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
			res = kingres_shard_bounded_exec(kc->conn, sql, false);
			if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
				kc->have_error = true;
			PQclear(res);
			if (!kc->have_error)
			{
				snprintf(sql, sizeof(sql), "RELEASE SAVEPOINT s%d", curlevel);
				res = kingres_shard_bounded_exec(kc->conn, sql, false);
				if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
					kc->have_error = true;
				PQclear(res);
			}
		}
		kc->xact_depth--;
	}
}

/*
 * kingres_shard_exec_ddl
 *
 * Run one DDL statement on a node in its open transaction, as the statement
 * means in this session: the names in it are resolved with this session's
 * search_path, and its literals are read with this session's DateStyle,
 * IntervalStyle, bytea_output, extra_float_digits and
 * standard_conforming_strings.  The settings are SET LOCAL equivalents
 * (set_config with is_local) and are put back to the fixed settings of the
 * connection afterwards, so that the statements that follow are not affected.
 */
void
kingres_shard_exec_ddl(KsConn *kc, const char *stmt)
{
	const char *params[6];
	PGresult   *res;

	params[0] = GetConfigOption("search_path", false, false);
	params[1] = GetConfigOption("DateStyle", false, false);
	params[2] = GetConfigOption("IntervalStyle", false, false);
	params[3] = GetConfigOption("standard_conforming_strings", false, false);
	params[4] = GetConfigOption("bytea_output", false, false);
	params[5] = GetConfigOption("extra_float_digits", false, false);
	res = kingres_shard_exec_params(kc,
									 "SELECT pg_catalog.set_config('search_path', $1, true), "
									 "pg_catalog.set_config('DateStyle', $2, true), "
									 "pg_catalog.set_config('IntervalStyle', $3, true), "
									 "pg_catalog.set_config('standard_conforming_strings', $4, true), "
									 "pg_catalog.set_config('bytea_output', $5, true), "
									 "pg_catalog.set_config('extra_float_digits', $6, true)",
									 6, NULL, params, true);
	PQclear(res);
	kc->tz_applied[0] = '\0';
	res = kingres_shard_exec(kc, stmt);
	PQclear(res);
	exec_internal(kc, KS_FIXED_SETTINGS_SQL "SELECT 1");
	kc->tz_applied[0] = '\0';
	sync_timezone(kc);
}

PG_FUNCTION_INFO_V1(kingres_internal_scrub_conn_error);

/*
 * kingres.internal_scrub_conn_error(message text) returns text
 *
 * kingres_shard_scrub_conn_error() as a function, so that the rule can be
 * tested without a server that fails in a way that names a key file.
 */
Datum
kingres_internal_scrub_conn_error(PG_FUNCTION_ARGS)
{
	char	   *msg = text_to_cstring(PG_GETARG_TEXT_PP(0));

	PG_RETURN_TEXT_P(cstring_to_text(kingres_shard_scrub_conn_error(msg)));
}
