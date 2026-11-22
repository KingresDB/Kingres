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
 * SPDX-License-Identifier: AGPL-3.0-only AND PostgreSQL
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
#include "access/xlog.h"
#include "catalog/indexing.h"
#include "commands/dbcommands.h"
#include "libpq/libpq-be-fe-helpers.h"
#include "storage/latch.h"
#include "utils/acl.h"
#include "mb/pg_wchar.h"
#include "miscadmin.h"
#include "replication/syncrep.h"
#include "portability/instr_time.h"
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
#include "ha.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "term.h"

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
	"SET LOCAL bytea_output = 'hex'; " \
	"RESET check_function_bodies; "


static HTAB *conn_hash = NULL;

/* The kind of standby wait the decision of this transaction required (log_decision). */
static int	decision_wait_mode = SYNC_REP_WAIT_FLUSH;

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
static void exec_internal_ext(KsConn *kc, const char *sql, bool force_bound);
static char *abbreviate_sql(const char *sql);
static char *conn_label(KsConn *kc);
static void append_timezone_setting(StringInfo buf);
static void append_dtx_label(StringInfo buf);
static void remember_timezone(KsConn *kc);
static void sync_timezone(KsConn *kc);
static void discard_connection(KsConn *kc);
static void end_of_xact(void);
static int64 control_time_left_ms(void);
static PGresult *bounded_get_last_limit(PGconn *conn, bool interruptible, int64 limit_ms, bool *failed);
static void call_timeout_error(const char *waiting, const char *sql);

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
ignore_notice(void *arg, const KsRawResult *res)
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
	{
		/*
		 * A standby is a physical copy of the primary of its group, so the node
		 * row it holds for itself (local_node) is the primary's until
		 * promote_node() changes it: the token of a session to a standby is made
		 * for that id.
		 */
		const KsNode *n = kingres_shard_node_by_id(id);

		if (n != NULL && !n->is_primary)
		{
			const KsNode *p = kingres_shard_group_primary(n->group_id);

			if (p != NULL)
				return p->node_id;
		}
		return id;
	}
	for (i = 0; i < nexpected; i++)
		if (expected_nodes[i].port == port && strcmp(expected_nodes[i].host, host) == 0)
			return expected_nodes[i].node_id;
	return 0;
}

/*
 * S9-03: a node that holds a rotation of the cluster secret prepared and has
 * not been told that it committed still holds the old secret and refuses the
 * sessions made with the new one, which is what the node that can finish the
 * rotation (the origin, by recovery) has.  Such a session is opened with the
 * previous secret, which every node keeps when it applies a new one; it is
 * never accepted from anybody, it only lets this node prove itself to a node
 * that has not switched yet.  Set only around the retry in connect_to_node.
 */
static bool connect_with_previous = false;

/* Did this backend need the previous secret to reach a node?  Recovery keeps the secret while so. */
bool		kingres_shard_previous_secret_used = false;

/*
 * A node that accepts the session and does not answer (S10-02).  In a control-plane call the
 * first statement of a new session is sent after a lock_timeout of the node's own; when it is
 * not answered within node_timeout (KS007) or the node gives up a lock wait (55P03), the reason is
 * kept here for the caller of connect_to_node() (get_conn raises it with its SQLSTATE), and the
 * node is remembered for the rest of the transaction, so that a call does not wait node_timeout
 * again for every session it would open to the same node.
 */
static int	connect_failure_code = 0;
static char *connect_failure_msg = NULL;

/*
 * The SQLSTATE of the refusal of the last probe of a session (S12-05): what the node itself said,
 * so that a caller decides by a fact the node reports and not by the words of its message.  0 when
 * the node did not refuse (it answered, or no session was made).
 */
static int	connect_refusal_sqlstate = 0;
typedef struct StuckNode
{
	char		key[KS_CONN_HOST_LEN + 16];
	int			code;
	char	   *msg;
} StuckNode;

static List *stuck_nodes = NIL;	/* the nodes that did not answer in this transaction */

/* How many control-plane calls are running, one inside the other (kingres_shard_control_enter) */
static int	control_depth = 0;

static int
control_lock_timeout_ms(void)
{
	return Max(kingres_shard_node_timeout * 750, 250);
}

/*
 * The time that one control-plane call gives one node (S12-03).  There is no budget for the call:
 * every statement of a call waits for its node at most node_timeout, and nothing else is counted.
 * A budget for the whole call (2 x node_timeout + 2 seconds, the S11-07 round) ended calls against a
 * node that answered every statement late, but in time, with the code of a node that does not
 * answer, which tells a control plane to fence a working node.  What replaces it is a deadline per
 * statement and a bound that is stated per call (number of statements x node_timeout): KS007 means
 * one statement was not answered within node_timeout, and nothing else.  A node that did not answer
 * is not asked again in the same call (stuck_nodes), so a silent node costs a call one wait, not
 * one wait per statement.
 */

static StuckNode *
node_is_stuck(const char *host, int port)
{
	ListCell   *lc;
	char		key[KS_CONN_HOST_LEN + 16];

	snprintf(key, sizeof(key), "%s:%d", host, port);
	foreach(lc, stuck_nodes)
		if (strcmp(((StuckNode *) lfirst(lc))->key, key) == 0)
			return (StuckNode *) lfirst(lc);
	return NULL;
}

/* Remember what the first failure to get an answer from the node said, for the rest of the transaction. */
static void
note_stuck(const char *host, int port, int code, const char *msg)
{
	MemoryContext old;
	StuckNode  *s;

	if (node_is_stuck(host, port))
		return;
	old = MemoryContextSwitchTo(TopMemoryContext);
	s = palloc0(sizeof(StuckNode));
	snprintf(s->key, sizeof(s->key), "%s:%d", host, port);
	s->code = code;
	s->msg = pstrdup(msg);
	stuck_nodes = lappend(stuck_nodes, s);
	MemoryContextSwitchTo(old);
}

static char *
node_label(const char *host, int port)
{
	int			id = kingres_shard_conn_node_id(host, port);

	return id > 0 ? psprintf("%d (%s:%d)", id, host, port) : psprintf("%s:%d", host, port);
}

/*
 * How a node is named in an error (N1 of S13): its id and the address as registered, which an
 * operator can look up, also for a node registered by a socket directory.  The id is the one
 * found when the session was got, so no catalog is read here (the callers include the callback
 * of a subtransaction that commits, where the catalog is not safe to read).
 */
static char *
conn_label(KsConn *kc)
{
	return kc->node_id > 0 ? psprintf("%d (%s:%d)", kc->node_id, kc->host, kc->port) :
		psprintf("%s:%d", kc->host, kc->port);
}

/*
 * S11-01: a session that carried the writes of the user's transaction was closed.  Its writes
 * are rolled back by the node, the other nodes of the transaction would commit: the transaction
 * must not.  The note lives until the transaction ends (a savepoint that is rolled back does not
 * clear it: the writes of an outer level are lost with the session), and is looked at before
 * anything is committed or prepared (conn_xact_callback).
 */
static char *session_lost_msg = NULL;
static bool in_xact_end = false;	/* inside the callback of the end of a transaction */

static void
note_session_lost(KsConn *kc)
{
	MemoryContext old;
	char	   *label;

	if (session_lost_msg != NULL)
		return;
	/*
	 * The label is made in the current (short-lived) context, and only the final text in the long-lived
	 * one: a temporary made in TopMemoryContext would stay for ever (review S14-03).
	 * The catalog is not read in an aborting transaction.
	 */
	label = IsTransactionState() ? node_label(kc->host, kc->port) : psprintf("%s:%d", kc->host, kc->port);
	old = MemoryContextSwitchTo(TopMemoryContext);
	session_lost_msg = psprintf("the session to node %s was closed while this transaction had written through it",
								label);
	MemoryContextSwitchTo(old);
	pfree(label);
}

/*
 * S13-01: a statement that ends the work of a node session (RELEASE SAVEPOINT, ROLLBACK TO SAVEPOINT) did not
 * complete within node_timeout, or failed.  The session is closed and the transaction must not commit: other
 * sessions may have released the work of the same subtransaction already, and the closed one took with it
 * what it held.  Said once, at the commit or the prepare of the transaction (an abort callback cannot raise
 * an error), with an error that names the node.  The note lives as long as the one of a lost session.
 */
static void
note_session_failed(KsConn *kc, const char *what)
{
	MemoryContext old;
	char	   *label;

	if (session_lost_msg != NULL)
		return;
	label = conn_label(kc);		/* not in the long-lived context (review S14-03) */
	old = MemoryContextSwitchTo(TopMemoryContext);
	session_lost_msg = psprintf("node %s did not complete %s, and its session was closed",
								label, what);
	MemoryContextSwitchTo(old);
	pfree(label);
}

static void
check_session_lost(void)
{
	if (session_lost_msg != NULL)
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("%s", session_lost_msg),
				 errdetail("What the transaction did on that node is rolled back with the session, or no longer follows the statement that failed, so committing the rest could apply the transaction on some nodes only."),
				 errhint("Roll the transaction back and repeat it.")));
}

static void
forget_session_lost(void)
{
	if (session_lost_msg != NULL)
	{
		pfree(session_lost_msg);
		session_lost_msg = NULL;
	}
}

/*
 * connect_to_node
 *
 * Open a new libpq connection to host:port as the current user.  Returns
 * NULL and sets *errmsg (palloc'd) on failure.
 */
static PGconn *
connect_to_node_as(const char *host, int port, const char *user, int claim_id,
				   char **errmsg_out)
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
	{
		/* in a control call the connect is cut to what is left of the time of the call (review S14-02) */
		int64		left = control_time_left_ms();
		int			ct = kingres_shard_node_connect_timeout;

		if (left >= 0 && left < (int64) ct * 1000)
			ct = (int) Max(1, (left + 999) / 1000);
		snprintf(timeoutstr, sizeof(timeoutstr), "%d", ct);
	}

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
	token = connect_with_previous ?
		kingres_shard_forward_token_previous(user, claim_id >= 0 ? claim_id : target_node_id(host, port)) :
		kingres_shard_forward_token(user, claim_id >= 0 ? claim_id : target_node_id(host, port));
	if (connect_with_previous && token == NULL)
	{
		*errmsg_out = pstrdup("this node holds no previous cluster secret");
		return NULL;
	}
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
		*errmsg_out = conn ? kingres_shard_scrub_conn_error(kingres_shard_conn_error_text(conn))
			: pstrdup("out of memory");
		if (conn)
			PQfinish(conn);
		return NULL;
	}
	PQsetNoticeReceiver(conn, ignore_notice, NULL);
	return conn;
}

/*
 * The proof of a session is checked by the other node at its first statement,
 * not when it connects.  Send one that costs nothing; false and the text of
 * the error when the node refuses the session.
 */
static bool
probe_session(PGconn *conn, const char *host, int port, char **errmsg_out)
{
	PGresult   *res;
	bool		ok;
	bool		control = kingres_shard_control_active();

	/*
	 * In a control-plane call the node's own lock wait is bounded for this one statement, so
	 * that a node whose first query waits for a lock of another session says so instead of
	 * being cancelled from far away (S10-02).  The three commands are one simple query, so one
	 * implicit transaction: the setting is gone when it ends, whichever way it ends, and the
	 * session that the caller gets carries nothing (S11-02).  A command that is not answered
	 * while the connection is still up is a node that does not answer; a connection that is
	 * gone (the node refused the session, which is what the callers below look for) is not.
	 */
	if (control)
	{
		char		sql[160];

		int64		left = control_time_left_ms();

		snprintf(sql, sizeof(sql), "SET lock_timeout = %d; SELECT 1; RESET lock_timeout",
				 control_lock_timeout_ms());
		if (left >= 0 && left < (int64) kingres_shard_node_timeout * 1000)
		{
			/* the probe waits for what is left of the call's time, and no longer (review S14-02) */
			bool		failed = false;

			res = NULL;
			if (PQstatus(conn) == CONNECTION_OK && PQsendQuery(conn, sql))
			{
				res = bounded_get_last_limit(conn, false, Max(left, 1), &failed);
				if (failed)
				{
					bool		still_up = (PQstatus(conn) == CONNECTION_OK);

					res = NULL;
					if (still_up)
					{
						PQfinish(conn);
						call_timeout_error(node_label(host, port), "the first command of a new session");
					}
				}
			}
		}
		else
			res = kingres_shard_bounded_exec(conn, sql, false);
	}
	else
		res = kingres_shard_bounded_exec(conn, "SELECT 1", false);
	ok = (res != NULL && PQresultStatus(res) == (control ? PGRES_COMMAND_OK : PGRES_TUPLES_OK));
	connect_refusal_sqlstate = ok ? 0 : kingres_shard_result_sqlstate(res);
	if (!ok && control)
	{
		if (res == NULL && PQstatus(conn) == CONNECTION_OK)
		{
			connect_failure_code = ERRCODE_KINGRES_NODE_TIMEOUT;
			*errmsg_out = psprintf("node %s did not answer within %d seconds: it accepted the session and did not answer its first command",
								   node_label(host, port), kingres_shard_node_timeout);
			connect_failure_msg = *errmsg_out;
			note_stuck(host, port, connect_failure_code, connect_failure_msg);
			return false;
		}
		if (res != NULL && kingres_shard_result_sqlstate(res) == ERRCODE_LOCK_NOT_AVAILABLE)
		{
			connect_failure_code = ERRCODE_LOCK_NOT_AVAILABLE;
			*errmsg_out = psprintf("lock not available on node %s: its first query did not get a lock within its lock timeout (another session holds a lock on a catalog table there)",
								   node_label(host, port));
			PQclear(res);
			connect_failure_msg = *errmsg_out;
			note_stuck(host, port, connect_failure_code, connect_failure_msg);
			return false;
		}
	}
	if (!ok)
		*errmsg_out = kingres_shard_scrub_conn_error(kingres_shard_conn_error_text(conn));
	PQclear(res);
	return ok;
}

/*
 * connect_to_node
 *
 * connect_to_node_as() for the id that this node's catalog gives the target.
 * A node that holds a catalog change prepared (a promotion that was left
 * half-done, S7-05) does not yet know itself by the id the catalog now gives
 * it: a standby is a copy of its primary and carries the primary's id until
 * promote_node() commits.  So when the node does not accept the proof, the same
 * session is tried with the id of every other node of that node's group: one
 * of them is what the node believes it is.  The first statement of a new
 * session is a probe for that; if no id is accepted the session of the first
 * attempt is returned, so that the statement that follows fails with the
 * message it always had (version differences are explained there).
 */
static PGconn *connect_to_node_inner(const char *host, int port, const char *user, char **errmsg_out);

/*
 * A node that could not be connected to is not tried again in the same call (S11-07): one call
 * pays node_connect_timeout once for it, not for every session it would open.
 */
static PGconn *
connect_to_node(const char *host, int port, const char *user, char **errmsg_out)
{
	PGconn	   *conn = connect_to_node_inner(host, port, user, errmsg_out);

	if (conn == NULL && control_depth > 0 && connect_failure_code == 0 && *errmsg_out != NULL)
		note_stuck(host, port, 0, *errmsg_out);
	return conn;
}

static PGconn *
connect_to_node_inner(const char *host, int port, const char *user, char **errmsg_out)
{
	PGconn	   *conn;
	char	   *perr = NULL;
	int			tried;
	int			group = -1;
	List	   *alts = NIL;
	ListCell   *lc;
	const KsCatalog *cat;

	connect_failure_code = 0;
	connect_refusal_sqlstate = 0;
	if (control_depth > 0)
	{
		StuckNode  *s = node_is_stuck(host, port);

		/* The node did not answer earlier in this call: it is not asked again, and says what it said then. */
		if (s != NULL)
		{
			connect_failure_code = s->code;
			*errmsg_out = connect_failure_msg = s->msg;
			return NULL;
		}
	}
	conn = connect_to_node_as(host, port, user, -1, errmsg_out);
	if (conn == NULL)
		return NULL;
	if (probe_session(conn, host, port, &perr))
		return conn;
	PQfinish(conn);
	conn = NULL;
	if (connect_failure_code != 0)
	{
		*errmsg_out = connect_failure_msg;
		return NULL;
	}
	/* the node's own SQLSTATE says whether it did not accept our proof (S12-05), not the words of its message */
	if (connect_refusal_sqlstate != ERRCODE_KINGRES_SECRET_UNKNOWN)
		return connect_to_node_as(host, port, user, -1, errmsg_out);
	(void) perr;

	tried = target_node_id(host, port);
	cat = kingres_shard_catalog();
	foreach(lc, cat->nodes)
	{
		const KsNode *n = (const KsNode *) lfirst(lc);

		if (n->port == port && strcmp(n->host, host) == 0)
			group = n->group_id;
	}
	if (group >= 0)
	{
		foreach(lc, cat->nodes)
		{
			const KsNode *n = (const KsNode *) lfirst(lc);

			if (n->group_id == group && n->node_id != tried)
				alts = lappend_int(alts, n->node_id);
		}
	}
	foreach(lc, alts)
	{
		char	   *err2 = NULL;

		conn = connect_to_node_as(host, port, user, lfirst_int(lc), &err2);
		if (conn == NULL)
			continue;
		if (probe_session(conn, host, port, &err2))
			return conn;
		PQfinish(conn);
		conn = NULL;
		if (connect_failure_code != 0)
		{
			*errmsg_out = connect_failure_msg;
			return NULL;
		}
	}

	/*
	 * The node may be one that missed the commit of a rotation of the secret:
	 * try the sessions again with the previous secret, for the ids of the node
	 * as above.
	 */
	{
		unsigned char probe_buf[128];
		int			probe_len = 0;

		if (kingres_shard_previous_secret(probe_buf, sizeof(probe_buf), &probe_len))
		{
			List	   *ids = lcons_int(tried, list_copy(alts));
			ListCell   *lcp;

			explicit_bzero(probe_buf, sizeof(probe_buf));
			PG_TRY();
			{
				connect_with_previous = true;
				foreach(lcp, ids)
				{
					char	   *err3 = NULL;

					conn = connect_to_node_as(host, port, user, lfirst_int(lcp), &err3);
					if (conn == NULL)
						continue;
					if (probe_session(conn, host, port, &err3))
					{
						kingres_shard_previous_secret_used = true;
						break;
					}
					PQfinish(conn);
					conn = NULL;
					if (connect_failure_code != 0)
						break;
				}
			}
			PG_FINALLY();
			{
				connect_with_previous = false;
			}
			PG_END_TRY();
			if (conn != NULL)
				return conn;
			if (connect_failure_code != 0)
			{
				*errmsg_out = connect_failure_msg;
				return NULL;
			}
		}
	}
	return connect_to_node_as(host, port, user, -1, errmsg_out);
}

/* The SQLSTATE of the last refusal that kingres_shard_connect_control() could name (KS007, 55P03), or 0. */
int
kingres_shard_connect_failure_code(void)
{
	return connect_failure_code;
}

/* The SQLSTATE the node itself gave when it refused the proof of the last session (S12-05), or 0. */
int
kingres_shard_connect_refusal_sqlstate(void)
{
	return connect_refusal_sqlstate;
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
 * Is host:port a node that add_node() is registering in this backend?  add_node()
 * checks the major of that node itself, with its own message.
 */
static bool
is_expected_node(const char *host, int port)
{
	int			i;

	for (i = 0; i < nexpected; i++)
		if (expected_nodes[i].port == port && strcmp(expected_nodes[i].host, host) == 0)
			return true;
	return false;
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
		kc->sync_forced = false;
		kc->via_previous = false;
		kc->sync_exempt = false;
		kc->new_node = false;

		kc->wrote_user = false;
		kc->control_work = false;
		kc->gid[0] = '\0';
		kc->pending_owner = NULL;
		kc->pending_finish = NULL;
	}
	kc->node_id = kingres_shard_conn_node_id(host, port);

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
		kingres_shard_previous_secret_used = false;
		kc->conn = connect_to_node(kc->host, kc->port, kc->user, &err);
		kc->via_previous = kingres_shard_previous_secret_used;
		kc->have_error = false;

		/* The node is up and did not answer, or gave up a lock wait (S10-02): say so, with its own SQLSTATE. */
		if (kc->conn == NULL && connect_failure_code != 0 && !missing_ok)
			ereport(ERROR,
					(errcode(connect_failure_code),
					 errmsg_internal("%s", err),
					 connect_failure_code == ERRCODE_KINGRES_NODE_TIMEOUT ?
					 errhint("The node is up but does not answer: look at its locks (pg_locks) and its backends (pg_stat_activity), or fence it.  Repeating the call does not help while it stays so.") :
					 errhint("Another session on that node holds a lock that this call needs (see pg_locks there); repeat the call when it is gone.")));

		/*
		 * One PostgreSQL major per cluster (review finding S6-09).  A node that
		 * was upgraded in place to another major and restarted is reachable and
		 * answers, but statements that span groups would run on two majors
		 * (another libm, another planner, other defaults): it is refused here,
		 * where a connection is made, with an error that says what to do.  This
		 * is the one place where a node learns what major another node runs:
		 * the catalog does not record it.  check_cluster() reports such a node
		 * (it asks over control sessions, which are not refused).
		 */
		if (kc->conn != NULL &&
			kingres_shard_peer_major(PQserverVersion(kc->conn) / 10000) != KS_SERVER_MAJOR &&
			!is_expected_node(host, port))
		{
			int			theirs = kingres_shard_peer_major(PQserverVersion(kc->conn) / 10000);

			discard_connection(kc);
			if (missing_ok)
			{
				if (errmsg_out)
					*errmsg_out = psprintf("node %s:%d runs PostgreSQL %d, this node runs PostgreSQL %d",
										   host, port, theirs, KS_SERVER_MAJOR);
				return NULL;
			}
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("node %s:%d runs PostgreSQL %d, this node runs PostgreSQL %d",
							host, port, theirs, KS_SERVER_MAJOR),
					 errdetail("A cluster runs one major version of PostgreSQL, and a statement that needs another node would run on two."),
					 errhint("Upgrade every node of the cluster to the same major version; kingres.check_cluster() lists the nodes of another major.")));
		}
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
 *
 * S12-01: closing a session whose remote transaction holds work that is not durable loses that
 * work without a trace (the commit looks only at sessions with xact_depth > 0).  So a session that
 * holds the writes of a transaction is closed only where the loss is told:
 *	- control_deadline(): the statement of a call got no answer; an ERROR ends the call (a caller
 *	  that wants to go on without the node uses kingres_shard_conn_abandon() below and says so with
 *	  a WARNING of its own);
 *	- the end of a transaction (commit, abort), where what was prepared is resolved by recovery and
 *	  every failure is a WARNING that names the node;
 *	- a session that is idle (xact_depth 0): nothing is lost (get_conn, drop_stale).
 * Nothing else closes a session: the call sends no statement after its last piece of work (no
 * RESET at the end of a call), and the lost-session note covers the writes of a user.
 */
static void
discard_connection(KsConn *kc)
{
	/*
	 * A session that carried the writes of the user's transaction (S11-01) cannot be closed
	 * without the transaction knowing: whatever the reason, the transaction must not commit.
	 * Every closing of a session goes through here.  The callbacks of the end of the transaction
	 * are exempt (the transaction is ending, and they clear the note).
	 */
	if (kc->conn != NULL && !in_xact_end &&
		(kc->wrote_user || (kc->xact_depth > 0 && IsolationUsesXactSnapshot())))
		note_session_lost(kc);
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
	kc->wrote_user = false;
	kc->control_work = false;
	kc->prepared = false;
	kc->sync_forced = false;
	kc->sync_exempt = false;
	kc->new_node = false;

	kc->pending_owner = NULL;
	kc->pending_finish = NULL;
}

/*
 * kingres_shard_conn_abandon
 *
 * A control call gives up on the session of one node and goes on without it (remove_node of a node
 * that stopped answering).  The remote transaction ends with the session, so nothing of the call
 * is left on that node; the caller says so itself, with a WARNING that names the node.  Nothing is
 * sent to the node.
 */
void
kingres_shard_conn_abandon(KsConn *kc)
{
	discard_connection(kc);
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
	kc->wrote_user = false;
	kc->control_work = false;

}

/*
 * kingres_shard_conn_drop_stale
 *
 * Close the idle cached sessions to nodes that the catalog knows and that are
 * not active primaries (the old primary of a group after promote_node(), a
 * standby): nothing routes to them any more, and a session that stays open to a
 * demoted node keeps the node's connection slot and, if the node is later
 * promoted again, would be used with its old state.  Sessions that are in a
 * transaction are left; they end with it.  Called when the catalog was loaded.
 */
void
kingres_shard_conn_drop_stale(List *nodes)
{
	HASH_SEQ_STATUS scan;
	KsConn	   *kc;

	if (conn_hash == NULL)
		return;
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		ListCell   *lc;
		bool		known = false;
		bool		primary = false;

		/* a session whose transaction is prepared (xact_depth is 0 then) is not idle: its second phase is coming (S12-01) */
		if (kc->conn == NULL || kc->xact_depth > 0 || kc->in_query ||
			kc->pending_owner != NULL || kc->prepared)
			continue;
		foreach(lc, nodes)
		{
			KsNode	   *n = (KsNode *) lfirst(lc);

			if (n->port == kc->port && strcmp(n->host, kc->host) == 0)
			{
				known = true;
				if (n->is_primary && n->is_active)
					primary = true;
			}
		}
		if (known && !primary)
			discard_connection(kc);
	}
}

/*
 * kingres_shard_conn_open_in_group
 *
 * A cached session of this backend that has a remote transaction open on a node of
 * shard group group_id other than host:port (the catalog says which nodes belong to
 * the group: the primary, the old primary after promote_node(), the standbys); NULL
 * when there is none.  A statement reads and writes a group through one node only
 * (design/shard-phase2b.md, 4.4; placement_check.c makes the rule).
 */
KsConn *
kingres_shard_conn_open_in_group(int group_id, const char *host, int port)
{
	const KsCatalog *cat = kingres_shard_catalog();
	HASH_SEQ_STATUS scan;
	KsConn	   *kc;

	if (conn_hash == NULL)
		return NULL;
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		ListCell   *lc;

		if (kc->conn == NULL || kc->xact_depth <= 0 ||
			(kc->port == port && strcmp(kc->host, host) == 0))
			continue;
		foreach(lc, cat->nodes)
		{
			KsNode	   *n = (KsNode *) lfirst(lc);

			if (n->group_id == group_id && n->port == kc->port && strcmp(n->host, kc->host) == 0)
			{
				hash_seq_term(&scan);
				return kc;
			}
		}
	}
	return NULL;
}

/*
 * Asynchronous fetches.  A scan that has sent a FETCH and has not read the
 * answer registers itself on the connection; anything else that wants to use
 * the connection first asks the owner to read the answer (it keeps the result
 * for later), so that two statements never overlap on one libpq connection.
 *
 * The owner is a pointer into the memory of a statement, which an error can
 * free without the owner ever being asked to let go (review finding S6-02).
 * So the rule is: the connection points at its owner for exactly as long as a
 * query of the owner is in flight, and every way that the owner can go away
 * lets go first:
 *	- the owner reads the answer and clears the registration itself
 *	  (kingres_shard_conn_clear_pending), before it looks at what it got;
 *	- the memory context of the owner has a reset callback that calls
 *	  kingres_shard_conn_abandon_pending (dist_scan.c), which reads and drops the
 *	  answer, whichever way the context goes (an error, a subtransaction abort,
 *	  a portal that is dropped, the end of the backend);
 *	- the abort of a subtransaction reads the answer through the owner while the
 *	  owner still exists, without raising an error (conn_subxact_callback), and
 *	  the abort of the transaction closes the connection (abort_connection),
 *	  which forgets the owner.
 * The finish function gets quiet = true from the abort paths: it must not
 * throw, and it keeps an error it finds for the owner to raise when the owner
 * asks for the batch.
 */
void
kingres_shard_conn_set_pending(KsConn *kc, void *owner, KsPendingFinish finish)
{
	Assert(kc->pending_owner == NULL || kc->pending_owner == owner);
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

/* Ask the owner of the query in flight to read its answer (it may raise an error). */
void
kingres_shard_conn_quiesce(KsConn *kc)
{
	if (kc->pending_owner != NULL)
		kc->pending_finish(kc->pending_owner, false);
}

/* The same where an error is not possible: an abort callback. */
void
kingres_shard_conn_quiesce_quiet(KsConn *kc)
{
	if (kc->pending_owner != NULL)
		kc->pending_finish(kc->pending_owner, true);
}

/*
 * kingres_shard_conn_abandon_pending
 *
 * The owner is going away.  If the connection still has it registered, the
 * query it sent is read and dropped (bounded, nothing thrown) and the
 * registration is gone, so the connection can be used by whoever comes next.
 * A connection that is broken has nothing to read; it is only forgotten.
 */
void
kingres_shard_conn_abandon_pending(KsConn *kc, void *owner)
{
	if (kc->pending_owner != owner)
		return;
	if (kc->conn == NULL || kc->have_error || PQstatus(kc->conn) != CONNECTION_OK)
	{
		kc->pending_owner = NULL;
		kc->pending_finish = NULL;
		kc->in_query = false;
		kc->have_error = true;
		return;
	}
	kingres_shard_conn_drop_inflight(kc);
}

/*
 * kingres_shard_conn_drop_inflight
 *
 * Read and drop the answer of the query in flight on kc (and forget its owner).
 * The memory the result wrapper of PostgreSQL 19 needs is taken in a context that
 * outlives the caller: the caller may be the reset callback of a memory context
 * that is being deleted, or an abort callback.
 */
void
kingres_shard_conn_drop_inflight(KsConn *kc)
{
	MemoryContext oldcxt = MemoryContextSwitchTo(TopMemoryContext);

	PQclear(kingres_shard_conn_collect_quiet(kc));
	MemoryContextSwitchTo(oldcxt);
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
				 errmsg("connection to node %s is in an unknown state",
						conn_label(kc)),
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
		appendStringInfo(&sql, "BEGIN ISOLATION LEVEL %s%s; %s%s", level,
						 XactReadOnly ? " READ ONLY" : "",
						 kc->sync_forced ? "RESET synchronous_commit; " : "",
						 KS_FIXED_SETTINGS_SQL);
		kc->tz_applied[0] = '\0';
		append_timezone_setting(&sql);
		append_dtx_label(&sql);
		kingres_shard_append_term_claim(&sql);
		exec_internal(kc, sql.data);
		kc->sync_forced = false;
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
 * The identity of the distributed transaction a remote session works for, as
 * the setting kingres_shard.forward_dtx for the length of the remote
 * transaction: "<origin node>:<origin pid>:<start of the origin transaction,
 * in microseconds since the Unix epoch>".  The node that receives it keeps it
 * in shared memory only when the session proved to come from a node of the
 * cluster (ha.c), and the deadlock detector reads it from there
 * (kingres.internal_dtx_identity), so that a wait of this session counts as a
 * wait of the origin transaction.  It is not the application name, which any
 * client can set (S7-02).  SET LOCAL ends with the transaction.
 */
static void
append_dtx_label(StringInfo buf)
{
	int			node = kingres_shard_local_node_id_raw();
	int64		us;

	if (node == 0)
		return;
	us = (int64) GetCurrentTransactionStartTimestamp() +
		((int64) (POSTGRES_EPOCH_JDATE - UNIX_EPOCH_JDATE)) * SECS_PER_DAY * USECS_PER_SEC;
	appendStringInfo(buf, "; SET LOCAL kingres_shard.forward_dtx = '%d:%d:" INT64_FORMAT "'",
					 node, MyProcPid, us);
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
 * The text of the last error of a connection, never empty (S9: "could not prepare the
 * transaction on node H:P: " with nothing after the colon).  libpq has no text when the
 * wait ran out and the command was cancelled, or when the answer never came.
 */
char *
kingres_shard_conn_error_text(PGconn *conn)
{
	char	   *msg;

	if (conn == NULL)
		return pstrdup("there is no connection to the node");
	msg = pchomp(PQerrorMessage(conn));
	if (msg[0] == '\0')
		return pstrdup(PQstatus(conn) == CONNECTION_OK ?
					   "the node did not answer (the wait ran out, or the answer was not read)" :
					   "the connection to the node was lost");
	return msg;
}

/*
 * Wait for the answer of the query that is in flight on conn, for at most
 * kingres_shard.node_timeout seconds, and return its last result (NULL when it
 * has none).  *failed is set when the wait ran out (the running command is
 * cancelled, best effort) or the connection broke; the caller closes the
 * connection or marks it unusable then.  Never throws; interrupts are checked
 * only when asked (the abort callbacks must not).
 */
static PGresult *
bounded_get_last_limit(PGconn *conn, bool interruptible, int64 limit_ms, bool *failed)
{
	/*
	 * The wait is measured with the monotonic clock: a step of the system clock (a time
	 * synchronisation of a virtual machine, a correction by ntpd) must not end the wait of a node that
	 * has answered in time (S12-03).
	 */
	instr_time	start;
	PGresult   *last = NULL;

	INSTR_TIME_SET_CURRENT(start);
	*failed = false;
	for (;;)
	{
		PGresult   *res;

		while (PQisBusy(conn))
		{
			instr_time	now;
			long		ms;
			int			rc;

			INSTR_TIME_SET_CURRENT(now);
			INSTR_TIME_SUBTRACT(now, start);
			ms = (long) (limit_ms - (int64) INSTR_TIME_GET_MILLISEC(now));
			if (ms <= 0)
			{
				/*
				 * This process may not have been scheduled for a while: an answer that is already
				 * in the socket is an answer, not a node that did not answer (S12-03).
				 */
				if (PQconsumeInput(conn) && !PQisBusy(conn))
					break;
				kingres_shard_cancel_with_deadline(conn);
				PQclear(last);
				*failed = true;
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
				/*
				 * A node that answers with a FATAL error (it refuses the proof of the session) sends the
				 * error and closes the connection: the error is the answer, with the SQLSTATE the node
				 * gave it (S12-05), and the end of the stream that follows is not a second failure.
				 */
				if (last != NULL && PQresultStatus(last) == PGRES_FATAL_ERROR)
					return last;
				PQclear(last);
				*failed = true;
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

/* The wait of node_timeout seconds that every statement of a node session gets. */
static PGresult *
bounded_get_last(PGconn *conn, bool interruptible, bool *failed)
{
	return bounded_get_last_limit(conn, interruptible, (int64) kingres_shard_node_timeout * 1000, failed);
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
	bool		failed;
	PGresult   *res;

	if (conn == NULL || PQstatus(conn) != CONNECTION_OK || !PQsendQuery(conn, sql))
		return NULL;
	res = bounded_get_last(conn, interruptible, &failed);
	return failed ? NULL : res;
}

/*
 * Like kingres_shard_bounded_exec(), for a command that waits for a standby (COMMIT PREPARED,
 * ROLLBACK PREPARED): when node_timeout runs out the command is cancelled, which ends the wait
 * with a warning, and its result (completed on the node, its standby not confirmed) is read
 * within another node_timeout.  NULL only when the connection broke or even that did not come.
 */
PGresult *
kingres_shard_exec_cancel_after_timeout(PGconn *conn, const char *sql)
{
	bool		failed;
	PGresult   *res;

	if (conn == NULL || PQstatus(conn) != CONNECTION_OK || !PQsendQuery(conn, sql))
		return NULL;
	res = bounded_get_last(conn, true, &failed);
	if (!failed)
		return res;
	if (PQstatus(conn) != CONNECTION_OK)
		return NULL;
	res = bounded_get_last(conn, true, &failed);
	return failed ? NULL : res;
}

/*
 * kingres_shard_conn_collect_quiet
 *
 * Read the answer of the query in flight on kc, as an abort path may: the wait
 * is bounded and interrupts are not checked, and nothing is thrown.  Returns
 * the last result (the caller clears it; NULL when there is none or the
 * connection failed, in which case have_error is set).  The query is no longer
 * in flight afterwards, and nobody is registered as waiting for it.
 */
PGresult *
kingres_shard_conn_collect_quiet(KsConn *kc)
{
	PGresult   *res = NULL;
	bool		failed = false;

	if (kc->conn == NULL || PQstatus(kc->conn) != CONNECTION_OK)
		failed = true;
	else
		res = bounded_get_last(kc->conn, false, &failed);
	if (failed)
		kc->have_error = true;
	kc->in_query = false;
	kc->pending_owner = NULL;
	kc->pending_finish = NULL;
	return failed ? NULL : res;
}

/*
 * kingres_shard_conn_cancel_and_drop
 *
 * The query in flight on kc is a write whose statement is ending in an error, a cancel or the
 * abort of a subtransaction (the batch of the batch sender): nobody wants its answer, so the node
 * is asked to stop it, and what it answers is read for at most the time a cancel request gets
 * (a third of node_timeout, at most node_connect_timeout).  Reading the answer for node_timeout
 * instead (kingres_shard_conn_collect_quiet) would make a cancel of the statement take that long
 * when the node is frozen or slow.  A node that does not answer in that time loses the session
 * (have_error): the remote transaction ends with it, and the transaction cannot commit
 * (precommit), which is what a lost write must mean.  Nothing is thrown.
 */
void
kingres_shard_conn_cancel_and_drop(KsConn *kc)
{
	bool		failed = false;
	MemoryContext oldcxt;

	if (kc->conn == NULL || kc->have_error || PQstatus(kc->conn) != CONNECTION_OK)
	{
		kc->have_error = true;
		kc->in_query = false;
		kc->pending_owner = NULL;
		kc->pending_finish = NULL;
		return;
	}
	kingres_shard_cancel_with_deadline(kc->conn);
	/* the result wrapper of PostgreSQL 19 needs a context that outlives a context being reset */
	oldcxt = MemoryContextSwitchTo(TopMemoryContext);
	PQclear(bounded_get_last_limit(kc->conn, false,
								   (int64) 1000 * Max(1, Min(kingres_shard_node_connect_timeout,
															 kingres_shard_node_timeout / 3)),
								   &failed));
	MemoryContextSwitchTo(oldcxt);
	if (failed)
		kc->have_error = true;
	kc->in_query = false;
	kc->pending_owner = NULL;
	kc->pending_finish = NULL;
}

/*
 * The dynamic extent of the control-plane calls that are running (S11-01).  A control call is a
 * function of the SQL interface that runs inside kingres_shard_control_call() (or between
 * kingres_shard_control_enter() and kingres_shard_control_leave() in a PG_TRY): the depth is
 * raised on entry and put back to the value it had on every way out, including an error that a
 * PL/pgSQL block catches, so that no state outlives the call; and when the depth is back at zero
 * the call's other state goes with it (the nodes it found stuck).  Nothing else changes it, and
 * the end of a transaction forces it to zero as a last resort (conn_xact_callback).
 *
 * S12-01: leaving a call sends nothing to any node.  Until the round of S12 the outermost leave
 * sent "RESET lock_timeout" to every session that carried the call's SET LOCAL lock_timeout, and a
 * node that did not answer that one statement lost its session, in the middle of the remote
 * transaction that held the call's own writes: the commit then skipped it, and the call ended
 * with success.  The call sets no lock_timeout in a remote transaction any more (a lock wait that
 * the deadline of node_timeout cuts off ends with KS007, or with the node's own 55P03 when the
 * administrator gave the node a lock_timeout), so there is nothing to give back, and no statement
 * is sent after the last statement of the call's own work.
 */

bool
kingres_shard_control_active(void)
{
	return control_depth > 0;
}

/*
 * The time of the call as a whole (S13-02).  kingres_shard.control_call_timeout is read when the
 * outermost call starts (a change inside the call does not count), 0 means no limit, and the
 * clock is the monotonic one.  The statements of the call are bounded by it only while the call
 * runs (control_depth > 0): the commit of the transaction that follows is not part of the call.
 */
static instr_time control_started;
static int64 control_budget_ms = 0;

/* What is left of the time of the call in milliseconds (never negative), or -1 when it has no limit. */
static int64
control_time_left_ms(void)
{
	instr_time	now;

	if (control_depth == 0 || control_budget_ms <= 0)
		return -1;
	INSTR_TIME_SET_CURRENT(now);
	INSTR_TIME_SUBTRACT(now, control_started);
	return Max(control_budget_ms - (int64) INSTR_TIME_GET_MILLISEC(now), 0);
}

/*
 * The call took longer than control_call_timeout as a whole (S13-02): its own SQLSTATE, and words
 * that do not say a node is dead.  waiting says which node the call was waiting for when the time
 * ran out (NULL: it ran out between two statements).
 */
static void
call_timeout_error(const char *waiting, const char *sql)
{
	ereport(ERROR,
			(errcode(ERRCODE_KINGRES_CALL_TIMEOUT),
			 errmsg("the call took longer than kingres_shard.control_call_timeout (%d seconds) as a whole",
					kingres_shard_control_call_timeout),
			 waiting != NULL ?
			 errdetail("The time ran out while it waited for node %s to answer: %s.  That wait was cut short by what was left of the call's time: nothing says the node is dead or slow.",
					   waiting, abbreviate_sql(sql)) :
			 errdetail("The time ran out between two statements that it sends to nodes.  Nothing says that a node is dead or slow."),
			 errhint("The state is the one of any call that failed: nothing of it was kept.  Do not fence a node because of this error.  Repeat the call, with a longer control_call_timeout or none.")));
}

/* Called before a statement of a call is sent: the time of the call may be used up already. */
static void
check_call_budget(void)
{
	if (control_time_left_ms() == 0)
		call_timeout_error(NULL, "");
}

void
kingres_shard_control_enter(KsControlScope *scope)
{
	scope->saved_depth = control_depth;
	if (control_depth == 0)
	{
		INSTR_TIME_SET_CURRENT(control_started);
		control_budget_ms = (int64) kingres_shard_control_call_timeout * 1000;
	}
	control_depth++;
}

/* The nodes a call found stuck are forgotten with the call, with the text each one carries (S13 by reading). */
static void
free_stuck_nodes(void)
{
	ListCell   *lc;

	foreach(lc, stuck_nodes)
		pfree(((StuckNode *) lfirst(lc))->msg);
	list_free_deep(stuck_nodes);
	stuck_nodes = NIL;
}

void
kingres_shard_control_leave(KsControlScope *scope)
{
	control_depth = scope->saved_depth;
	if (control_depth == 0)
		free_stuck_nodes();
}

/* A function of the SQL interface that is a control-plane call: its body runs in the scope. */
Datum
kingres_shard_control_call(PGFunction body, FunctionCallInfo fcinfo)
{
	KsControlScope scope;
	Datum		result;

	kingres_shard_control_enter(&scope);
	PG_TRY();
	{
		result = body(fcinfo);
	}
	PG_FINALLY();
	{
		kingres_shard_control_leave(&scope);
	}
	PG_END_TRY();
	return result;
}

/* The first words of a statement, for a message. */
static char *
abbreviate_sql(const char *sql)
{
	char	   *s = pstrdup(sql);
	char	   *p;

	for (p = s; *p; p++)
		if (*p == '\n' || *p == '\r' || *p == '\t')
			*p = ' ';
	if (strlen(s) > 90)
	{
		s[87] = '\0';
		return psprintf("%s...", s);
	}
	return s;
}

/*
 * The node did not answer within node_timeout (S10-02).  The statement has been cancelled;
 * the session is closed, which ends whatever the node was doing for it (a wait for a lock,
 * a transaction that was open).  Says which node did not answer and what it was asked.
 * With elevel WARNING it returns, and the caller gets no result.
 */
static void
control_deadline(KsConn *kc, const char *sql, int elevel, bool cut_by_call)
{
	char	   *what = abbreviate_sql(sql);
	char	   *who = conn_label(kc);
	char	   *msg = psprintf("node %s did not answer within %d seconds", who, kingres_shard_node_timeout);

	kc->in_query = false;
	/*
	 * A statement that the end of a call sends (RELEASE, COMMIT) after the call has been left is
	 * not remembered: stuck_nodes belongs to the call, and the next call in the same transaction
	 * (a retry in a PL/pgSQL loop) must ask the node again.
	 */
	if (!cut_by_call && control_depth > 0)
		note_stuck(kc->host, kc->port, ERRCODE_KINGRES_NODE_TIMEOUT, msg);
	discard_connection(kc);
	if (cut_by_call)
		call_timeout_error(who, sql);
	ereport(elevel,
			(errcode(ERRCODE_KINGRES_NODE_TIMEOUT),
			 errmsg_internal("%s", msg),
			 errdetail("It accepted the session and was waiting for its answer to: %s%s", what,
					   strncmp(sql, "COMMIT TRANSACTION", 18) == 0 ?
					   ".  Whether it committed is not known: the session was closed" : ""),
			 errhint("The node is up but does not answer: look at its locks (pg_locks) and its backends (pg_stat_activity), or fence it.  Repeating the call does not help while it stays so.")));
}

/*
 * The answer of the statement in flight on kc.  In a control-plane call the wait is bounded by
 * node_timeout (and by what is left of control_call_timeout, S13-02) and *timed_out says that the
 * wait ran out (the caller reports it, after kc is closed; *cut_by_call says that it was the time
 * of the call that ran out and not node_timeout); otherwise it waits for ever, as the statements of
 * a user do (they can be cancelled by the user).  force_bound is the statements that the end of a
 * node session needs (S13-01): RELEASE, ROLLBACK TO, and the COMMIT of a session that carried the
 * work of a call; they wait at most node_timeout whatever the control depth is.
 */
static PGresult *
control_get_result(KsConn *kc, bool *timed_out, bool *cut_by_call, bool force_bound)
{
	bool		failed;
	PGresult   *res;
	int64		limit = (int64) kingres_shard_node_timeout * 1000;
	int64		left = control_time_left_ms();

	*timed_out = false;
	*cut_by_call = false;
	if (control_depth == 0 && !force_bound)
		return libpqsrv_get_result_last(kc->conn, PG_WAIT_EXTENSION);
	if (left >= 0 && left < limit)
	{
		limit = left;
		*cut_by_call = true;
	}
	res = bounded_get_last_limit(kc->conn, true, limit, &failed);
	if (failed)
	{
		*timed_out = (PQstatus(kc->conn) == CONNECTION_OK);
		return NULL;
	}
	return res;
}

/*
 * The end of a top-level transaction (commit, prepare or abort): nothing of a control call or of
 * a lost session survives it.  The scopes have restored themselves long before this on every way
 * out of a call, so this is the last resort that makes "left set" impossible (S11-02).
 */
static void
end_of_xact(void)
{
	control_depth = 0;
	free_stuck_nodes();
	forget_session_lost();
}

/*
 * exec_internal
 *
 * Run a command that returns no rows, with no statement accounting.
 */
static void
exec_internal(KsConn *kc, const char *sql)
{
	exec_internal_ext(kc, sql, false);
}

static void
exec_internal_ext(KsConn *kc, const char *sql, bool force_bound)
{
	PGresult   *res;
	bool		timed_out;
	bool		cut_by_call;

	kingres_shard_conn_quiesce(kc);
	if (control_depth > 0)
	{
		check_call_budget();
		kc->control_work = true;
	}
	kc->in_query = true;
	if (!PQsendQuery(kc->conn, sql))
	{
		kc->have_error = true;
		kc->in_query = false;
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not send a command to node %s",
						conn_label(kc)),
				 errdetail_internal("%s", kingres_shard_conn_error_text(kc->conn))));
	}
	res = control_get_result(kc, &timed_out, &cut_by_call, force_bound);
	if (timed_out)
		control_deadline(kc, sql, ERROR, cut_by_call);
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
	bool		timed_out;
	bool		cut_by_call;

	kingres_shard_conn_quiesce(kc);
	if (control_depth > 0)
	{
		check_call_budget();
		kc->control_work = true;
	}
	kc->in_query = true;
	if (!PQsendQuery(kc->conn, sql))
	{
		kc->have_error = true;
		kc->in_query = false;
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not send a command to node %s",
						conn_label(kc)),
				 errdetail_internal("%s", kingres_shard_conn_error_text(kc->conn))));
	}
	res = control_get_result(kc, &timed_out, &cut_by_call, false);
	if (timed_out)
		control_deadline(kc, sql, ERROR, cut_by_call);
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
	bool		timed_out;
	bool		cut_by_call;

	/*
	 * A session that a deadline closed earlier in this call (S10-02): a caller that tolerates
	 * failure gets "no answer", as for any node that cannot be asked.
	 */
	if (kc->conn == NULL && !throw_on_error && control_depth > 0)
		return NULL;
	kingres_shard_conn_quiesce(kc);
	if (control_depth > 0)
	{
		check_call_budget();
		kc->control_work = true;
	}
	kc->in_query = true;
	if (!PQsendQueryParams(kc->conn, sql, nparams, types, values, NULL, NULL, 0))
	{
		kc->have_error = true;
		kc->in_query = false;
		/* a caller that tolerates failure (recovery, S12-01) gets "no answer", as when a deadline closed the session */
		if (!throw_on_error)
			return NULL;
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not send a command to node %s",
						conn_label(kc)),
				 errdetail_internal("%s", kingres_shard_conn_error_text(kc->conn))));
	}
	res = control_get_result(kc, &timed_out, &cut_by_call, false);
	if (timed_out)
	{
		control_deadline(kc, sql, throw_on_error ? ERROR : WARNING, cut_by_call);
		return NULL;
	}
	kc->in_query = false;
	kc->nstatements++;
	if (throw_on_error && (res == NULL ||
						   (PQresultStatus(res) != PGRES_COMMAND_OK &&
							PQresultStatus(res) != PGRES_TUPLES_OK)))
		kingres_shard_report_error(kc, res, sql);
	return res;
}

/*
 * kingres_shard_conn_send_params
 *
 * Send one statement with text-format parameters (the types come from the casts in the
 * statement) and return without waiting for the answer, which the caller reads with
 * libpqsrv_get_result_last() after registering as the owner of the query in flight
 * (kingres_shard_conn_set_pending).  This is the send of the batch sender (design 11.4,
 * 11.9): the batch can be a megabyte, and a node that does not read (stuck, or slow) must
 * not leave this backend in a write that nothing can end, so the connection is made
 * non-blocking for the send and the wait for room in the socket is a latch wait that a
 * cancel, a statement timeout and the death of the postmaster end (S13: every wait on a node
 * has a bound or is a statement of the user that a cancel ends).  The statement is in
 * flight (kc->in_query) from the first byte on, so every path that leaves it half sent
 * finds the session marked: the error path of this function closes the session (it cannot
 * be used for another statement after a half-sent message), which rolls back what the
 * transaction did there; the commit then fails (note_session_lost).
 *
 * Returns false when the statement could not be sent (the connection is gone); the caller
 * raises the error of the group (kingres_shard_fragment_error).
 */
bool
kingres_shard_conn_send_params(KsConn *kc, const char *sql, int nparams,
							   const char *const *values)
{
	bool		ok = true;

	kingres_shard_conn_quiesce(kc);
	if (kc->conn == NULL || PQstatus(kc->conn) != CONNECTION_OK)
	{
		kc->have_error = true;
		return false;
	}
	if (control_depth > 0)
	{
		check_call_budget();
		kc->control_work = true;
	}
	kc->in_query = true;
	PG_TRY();
	{
		if (PQsetnonblocking(kc->conn, 1) != 0 ||
			!PQsendQueryParams(kc->conn, sql, nparams, NULL, values, NULL, NULL, 0))
			ok = false;
		while (ok)
		{
			int			rc = PQflush(kc->conn);

			if (rc < 0)
			{
				ok = false;
				break;
			}
			if (rc == 0)
				break;
			(void) WaitLatchOrSocket(MyLatch,
									 WL_LATCH_SET | WL_SOCKET_READABLE | WL_SOCKET_WRITEABLE |
									 WL_EXIT_ON_PM_DEATH,
									 PQsocket(kc->conn), -1L, PG_WAIT_EXTENSION);
			ResetLatch(MyLatch);
			CHECK_FOR_INTERRUPTS();
			/* the node may be sending its answer or a notice: read it so that it can go on reading ours */
			if (!PQconsumeInput(kc->conn))
				ok = false;
		}
		if (ok && PQsetnonblocking(kc->conn, 0) != 0)
			ok = false;
	}
	PG_CATCH();
	{
		kc->in_query = false;
		kc->have_error = true;
		PG_RE_THROW();
	}
	PG_END_TRY();
	if (!ok)
	{
		kc->in_query = false;
		kc->have_error = true;
		return false;
	}
	kc->nstatements++;
	return true;
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
 * kingres_shard_remote_extension_version
 *
 * The version of the extension that host:port has, asked on a connection that
 * claims nothing (the node refused the claim of this one), as the current role;
 * NULL when it cannot be asked, an empty string when the extension is not
 * installed there.  Used to explain a refusal, and by check_cluster() and the
 * check before a move for a node that runs another version (S6-06).
 */
char *
kingres_shard_remote_extension_version(const char *host, int port)
{
	const char *keywords[8];
	const char *values[8];
	char		portstr[16];
	char		timeoutstr[16];
	PGconn	   *conn;
	PGresult   *res;
	char	   *version = NULL;
	int			n = 0;

	snprintf(portstr, sizeof(portstr), "%d", port);
	snprintf(timeoutstr, sizeof(timeoutstr), "%d", kingres_shard_node_connect_timeout);
	keywords[n] = "host";
	values[n++] = host;
	keywords[n] = "port";
	values[n++] = portstr;
	keywords[n] = "dbname";
	values[n++] = get_database_name(MyDatabaseId);
	keywords[n] = "user";
	values[n++] = GetUserNameFromId(GetUserId(), false);
	keywords[n] = "connect_timeout";
	values[n++] = timeoutstr;
	keywords[n] = NULL;
	values[n] = NULL;
	conn = PQconnectdbParams(keywords, values, false);
	if (conn == NULL)
		return NULL;
	if (PQstatus(conn) == CONNECTION_OK)
	{
		res = kingres_shard_bounded_exec(conn,
										 "SELECT extversion FROM pg_catalog.pg_extension WHERE extname = 'kingres_shard'",
										 false);
		if (res != NULL && PQresultStatus(res) == PGRES_TUPLES_OK)
			version = pstrdup(PQntuples(res) == 1 ? PQgetvalue(res, 0, 0) : "");
		PQclear(res);
	}
	PQfinish(conn);
	return version;
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
	bool		no_result;

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

	/* no SQLSTATE: no result, or the error that libpq makes of a connection that was closed */
	no_result = (sqlstate == 0);
	if (sqlstate == 0)
		sqlstate = ERRCODE_CONNECTION_FAILURE;
	if (PQstatus(kc->conn) != CONNECTION_OK)
		kc->have_error = true;
	if (primary == NULL)
		primary = kingres_shard_conn_error_text(kc->conn);

	/*
	 * The other node holds a newer catalog term than this one (S7-06): this node
	 * has been superseded (the map group primary) or missed a promotion (any
	 * other).  Learn it before the error leaves, so that the next statement
	 * starts from the right catalog.
	 */
	if (sqlstate == ERRCODE_KINGRES_STALE_TERM && IsTransactionState())
	{
		int64		newer;

		if (kingres_shard_parse_term_detail(detail, &newer))
			kingres_shard_learn_term(newer, kingres_shard_conn_node_id(kc->host, kc->port), true);
	}

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

	/*
	 * A node that does not accept the claim of this session to come from a
	 * node of the cluster: with another version of the extension the proof is
	 * made another way (0.3 and 0.4 differ), and "cannot prove it" says nothing
	 * about that (review finding S5-02).  Ask for the version and say it.
	 */
	/*
	 * The node's own SQLSTATE says that it refused the proof (S12-05).  A statement of a user waits
	 * with libpq's own reader, which drops the error of a node that closes the connection after a
	 * FATAL error: there is no result then, only the text of the connection, and the text is all
	 * there is to explain the refusal with (it decides nothing: the version is asked and said).
	 */
	if (sqlstate == ERRCODE_KINGRES_SECRET_UNKNOWN ||
		sqlstate == ERRCODE_INVALID_AUTHORIZATION_SPECIFICATION ||
		(no_result && strstr(primary, "claims to have been opened by another node of the cluster") != NULL))
	{
		char	   *theirs = kingres_shard_remote_extension_version(kc->host, kc->port);

		if (theirs != NULL && theirs[0] == '\0')
			theirs = NULL;
		if (theirs == NULL || strcmp(theirs, KINGRES_SHARD_EXT_VERSION) != 0)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 theirs ?
					 errmsg("node %s:%d runs kingres_shard %s, this node runs %s", kc->host, kc->port,
							theirs, KINGRES_SHARD_EXT_VERSION) :
					 errmsg("node %s:%d did not accept the session of this node, and its kingres_shard version could not be read",
							kc->host, kc->port),
					 errdetail("The node said: %s", primary),
					 errhint("Every node of a cluster must run the same version of kingres_shard; run ALTER EXTENSION kingres_shard UPDATE on the nodes that run an older one.")));
	}

	/*
	 * A lock wait that ended by the node's own lock_timeout (the first statement of a session of a
	 * control call runs with one, S10-02, and so does every statement when the administrator gave
	 * the node's database or role a lock_timeout): say which node, and what it was waiting for,
	 * instead of a bare "canceling statement due to lock timeout" that the caller cannot place.
	 */
	if (sqlstate == ERRCODE_LOCK_NOT_AVAILABLE && control_depth > 0)
	{
		int			id = kingres_shard_conn_node_id(kc->host, kc->port);

		ereport(ERROR,
				(errcode(sqlstate),
				 errmsg("lock not available on node %s: the node did not get a lock within its lock timeout",
						id > 0 ? psprintf("%d (%s:%d)", id, kc->host, kc->port) :
						psprintf("%s:%d", kc->host, kc->port)),
				 errdetail("The node said: %s", primary),
				 errhint("Another session on that node holds a lock that this call needs (see pg_locks there); repeat the call when it is gone."),
				 context ? errcontext("%s", context) : 0,
				 errcontext("remote node %s:%d", kc->host, kc->port)));
	}

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
	if (control_depth == 0)
		kc->wrote_user = true;
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
	if (control_depth == 0)
		kc->wrote_user = true;
}

/*
 * kingres.internal_dtx_applied(gid text) returns void
 *
 * Runs in the remote transaction of a participant, just before its PREPARE
 * TRANSACTION (S7-07): it writes the identifier of the distributed transaction to
 * kingres.dtx_applied.  The row commits with the participant's part, replicates
 * with it, and is what the origin looks for when the participant has failed over.
 * Only a node session (or a superuser) can write one.  The row is made with the heap API, so the role of the
 * transaction needs no privilege on the table.
 */
PG_FUNCTION_INFO_V1(kingres_internal_dtx_applied);

Datum
kingres_internal_dtx_applied(PG_FUNCTION_ARGS)
{
	char	   *gid = text_to_cstring(PG_GETARG_TEXT_PP(0));
	Oid			relid;
	Relation	rel;
	Datum		values[2];
	bool		nulls[2] = {false, false};
	HeapTuple	tup;

	if (!superuser() && !kingres_shard_session_node_proven())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for function internal_dtx_applied")));
	/*
	 * The tag is not checked here: the node that is being removed has just lost
	 * the cluster secret in the same transaction, and only a node session or a
	 * superuser gets this far.
	 */
	if (strncmp(gid, "kingres_h", 9) != 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("not an identifier of a distributed transaction")));
	relid = kingres_shard_catalog_relid("dtx_applied");
	if (!OidIsValid(relid))
		PG_RETURN_VOID();
	values[0] = CStringGetTextDatum(gid);
	values[1] = TimestampTzGetDatum(GetCurrentTimestamp());
	PushActiveSnapshot(GetTransactionSnapshot());
	rel = table_open(relid, RowExclusiveLock);
	tup = heap_form_tuple(RelationGetDescr(rel), values, nulls);
	CatalogTupleInsert(rel, tup);
	heap_freetuple(tup);
	table_close(rel, NoLock);
	PopActiveSnapshot();
	PG_RETURN_VOID();
}

/*
 * kingres_shard_conn_force_sync
 *
 * Raise synchronous_commit of an idle remote session to at least "on" (the
 * participant's standby must acknowledge a COMMIT PREPARED or ROLLBACK PREPARED
 * that recovery sends, whatever the setting of the participant's role is;
 * S7-07).  "remote_apply" is kept.  Nothing is thrown.
 */
bool
kingres_shard_conn_force_sync(KsConn *kc)
{
	PGresult   *res;
	bool		ok;

	if (kc->sync_forced)
		return true;
	res = kingres_shard_exec_params(kc,
									"SELECT set_config('synchronous_commit', CASE WHEN current_setting('synchronous_commit') = 'remote_apply' THEN 'remote_apply' ELSE 'on' END, false)",
									0, NULL, NULL, false);
	ok = (res != NULL && PQresultStatus(res) == PGRES_TUPLES_OK);
	PQclear(res);
	if (ok)
		kc->sync_forced = true;
	return ok;
}

/*
 * Mark the session of a node that is being demoted: its PREPARE and COMMIT PREPARED do
 * not wait for its standbys and are not asked about them (S9-05).  Cleared when the
 * transaction ends.
 */
void
kingres_shard_conn_exempt_sync(KsConn *kc)
{
	kc->sync_exempt = true;
}

/* synchronous_commit = local on an idle session: recovery commits what a demoted node left, without waiting for the standby it names. */
bool
kingres_shard_conn_local_sync(KsConn *kc)
{
	PGresult   *res;
	bool		ok;

	res = kingres_shard_exec_params(kc, "SELECT set_config('synchronous_commit', 'local', false)",
									0, NULL, NULL, false);
	ok = (res != NULL && PQresultStatus(res) == PGRES_TUPLES_OK);
	PQclear(res);
	if (ok)
		kc->sync_forced = true;
	return ok;
}

/* Undo kingres_shard_conn_force_sync() on an idle session; a failure only leaves the stricter setting. */
void
kingres_shard_conn_unforce_sync(KsConn *kc)
{
	PGresult   *res;

	if (!kc->sync_forced || kc->conn == NULL || kc->xact_depth > 0)
		return;
	res = kingres_shard_exec_params(kc, "RESET synchronous_commit", 0, NULL, NULL, false);
	if (res != NULL && PQresultStatus(res) == PGRES_COMMAND_OK)
		kc->sync_forced = false;
	PQclear(res);
}

/* Forget everything about the transaction that just ended on kc. */
static void
reset_xact_state(KsConn *kc)
{
	kc->xact_depth = 0;
	kc->sync_exempt = false;
	kc->nstatements = 0;
	kc->tz_applied[0] = '\0';
	kc->wrote = false;
	kc->wrote_user = false;
	kc->control_work = false;

	kc->prepared = false;
	kc->commit_sent = false;
	kc->gid[0] = '\0';
	/* Nothing that was in flight outlives the transaction. */
	kc->pending_owner = NULL;
	kc->pending_finish = NULL;
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
					 errdetail_internal("%s", res ? kingres_shard_conn_error_text(kc->conn) : "no result")));
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
 * prepare_collect
 *
 * Read the answers of the batch that prepare_one() sent, one result at a time,
 * so that the answer of PREPARE TRANSACTION and the answer of the question
 * after it (is the record durable on the participant's standby?) are told
 * apart.  The wait is interruptible (the client may cancel) and bounded by
 * kingres_shard.node_timeout (S8-08): when it runs out the participant's
 * backend is cancelled, which ends a wait for its synchronous standby with a
 * warning; the batch then goes on to the question, whose answer is "no", and
 * gets one more node_timeout to arrive.  *prepared says that PREPARE
 * TRANSACTION itself succeeded; *confirmed is -1 when no answer to the
 * question arrived, 1 for yes and 0 for no.  Returns the last result (an error
 * result when something after the start failed), or NULL when the connection
 * broke.
 */
static PGresult *
prepare_collect(KsConn *kc, bool *prepared, int *confirmed, bool *timed_out)
{
	TimestampTz deadline = TimestampTzPlusMilliseconds(GetCurrentTimestamp(),
													   (int64) kingres_shard_node_timeout * 1000);
	PGresult   *last = NULL;
	bool		cancelled = false;

	*prepared = false;
	*confirmed = -1;
	*timed_out = false;
	for (;;)
	{
		PGresult   *res;

		while (PQisBusy(kc->conn))
		{
			long		ms = TimestampDifferenceMilliseconds(GetCurrentTimestamp(), deadline);
			int			rc;

			if (ms <= 0)
			{
				if (cancelled)
				{
					kc->have_error = true;
					PQclear(last);
					return NULL;
				}
				kingres_shard_cancel_with_deadline(kc->conn);
				cancelled = true;
				*timed_out = true;
				/*
				 * After the cancel the participant still answers its own question (internal_sync_confirm,
				 * at most min(node_timeout, 5) seconds) before the batch ends: wait for that answer, or
				 * the refusal "did not confirm" and "no answer" are a race of two equal timeouts.
				 */
				deadline = TimestampTzPlusMilliseconds(GetCurrentTimestamp(),
													   (int64) Max(kingres_shard_node_timeout, Min(kingres_shard_node_timeout, 5) + 2) * 1000);
				continue;
			}
			rc = WaitLatchOrSocket(MyLatch,
								   WL_LATCH_SET | WL_SOCKET_READABLE | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH,
								   PQsocket(kc->conn), Min(ms, 1000L), PG_WAIT_EXTENSION);
			if (rc & WL_LATCH_SET)
			{
				ResetLatch(MyLatch);
				CHECK_FOR_INTERRUPTS();
			}
			if (!PQconsumeInput(kc->conn))
			{
				kc->have_error = true;
				PQclear(last);
				return NULL;
			}
		}
		res = PQgetResult(kc->conn);
		if (res == NULL)
			break;
		if (PQresultStatus(res) == PGRES_COMMAND_OK &&
			strcmp(PQcmdStatus(res), "PREPARE TRANSACTION") == 0)
			*prepared = true;
		else if (PQresultStatus(res) == PGRES_TUPLES_OK && PQnfields(res) == 1 &&
				 PQntuples(res) == 1 && *prepared)
			*confirmed = (strcmp(PQgetvalue(res, 0, 0), "t") == 0) ? 1 : 0;
		PQclear(last);
		last = res;
	}
	return last;
}

/*
 * prepare_one
 *
 * PREPARE TRANSACTION on one remote writer.  A failure (a deferred constraint
 * that fails at prepare time, prepared transactions disabled, the limit
 * reached) aborts the local transaction; the abort callback rolls back the
 * others.
 *
 * The prepare counts as done only when the participant says that the record is
 * durable by its own rule (S8-01).  PostgreSQL ends the wait of PREPARE
 * TRANSACTION for a cancel, a termination or a shutdown with a warning and an
 * answer of success, which this session cannot see; so the same batch asks, after
 * the PREPARE, internal_sync_confirm(): is everything this node has flushed so
 * far confirmed by its synchronous standbys as its configuration requires?  The
 * answer is a result of the batch, not a warning.  "No" (or no answer) before
 * the decision is a failed prepare: the transaction aborts, and the prepared
 * transactions are rolled back (by the abort callback, or by recovery, which finds
 * no decision row).
 */
static void
prepare_one(KsConn *kc)
{
	char		sql[2 * KS_GID_LEN + 420];
	PGresult   *res;
	bool		prepared;
	bool		timed_out;
	int			confirmed;
	bool		schema_missing;

	/*
	 * The participant acknowledges its PREPARE (and, on this session, the
	 * COMMIT PREPARED or ROLLBACK PREPARED that follows) to its synchronous
	 * standby whatever its own session, role or database setting says: the
	 * setting is raised to at least "on" right before the PREPARE, at session
	 * level so that it still holds for the second phase on this connection
	 * (S7-07).  "remote_apply" is kept.  The next remote transaction on the
	 * connection resets it (kingres_shard_begin_xact).  The question at the end
	 * waits at most five seconds (or node_timeout, if shorter) for a standby that
	 * is behind with the few records written since the PREPARE.
	 */
	/*
	 * A participant that is being demoted (S9-05: the old primary of a promotion) does
	 * not wait for its standbys and is not asked: it is about to stop being a primary,
	 * its confirmation is not what makes the promotion durable, and the standby it
	 * names is the node that was promoted in its place.
	 */
	if (kc->sync_exempt)
		snprintf(sql, sizeof(sql),
				 "SELECT " KINGRES_SHARD_SCHEMA ".internal_dtx_applied('%s'); "
				 "SELECT set_config('synchronous_commit', 'local', false); "
				 "PREPARE TRANSACTION '%s'; "
				 "SELECT true",
				 kc->gid, kc->gid);
	else
		snprintf(sql, sizeof(sql),
				 "SELECT " KINGRES_SHARD_SCHEMA ".internal_dtx_applied('%s'); "
				 "SELECT set_config('synchronous_commit', CASE WHEN current_setting('synchronous_commit') = 'remote_apply' THEN 'remote_apply' ELSE 'on' END, false); "
				 "PREPARE TRANSACTION '%s'; "
				 "SELECT " KINGRES_SHARD_SCHEMA ".internal_sync_confirm(%d)",
				 kc->gid, kc->gid, Min(kingres_shard_node_timeout, 5) * 1000);
	kingres_shard_conn_quiesce(kc);
	kc->in_query = true;
	if (!PQsendQuery(kc->conn, sql))
	{
		kc->have_error = true;
		kc->in_query = false;
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not send PREPARE TRANSACTION to node %s",
						conn_label(kc)),
				 errdetail_internal("%s", kingres_shard_conn_error_text(kc->conn))));
	}
	res = prepare_collect(kc, &prepared, &confirmed, &timed_out);
	kc->in_query = false;
	if (prepared)
	{
		/* From here on the transaction exists on the participant, whatever follows. */
		kc->sync_forced = true;
		kc->prepared = true;
		kc->xact_depth = 0;
	}
	/*
	 * A node that gets the extension in this very transaction (add_node) has no
	 * schema to ask after the PREPARE: the objects are in the prepared transaction.
	 * It is not part of the cluster yet, so no group of it has a standby that
	 * could matter; the error "schema does not exist" is then the answer "nothing
	 * to confirm".  The schema "kingres" is shared with kingres_columnar and kingres_ledger: a node that has one of them
	 * has the schema and not the function, which is the same case (undefined_function) -- but only for the node that add_node is
	 * making part of the cluster (new_node): a participant of a cluster that lacks the function must fail, not skip the confirmation.
	 */
	schema_missing = (prepared && res != NULL && PQresultStatus(res) == PGRES_FATAL_ERROR &&
					  (kingres_shard_result_sqlstate(res) == ERRCODE_INVALID_SCHEMA_NAME ||
					   (kc->new_node && kingres_shard_result_sqlstate(res) == ERRCODE_UNDEFINED_FUNCTION)));
	if (!schema_missing &&
		(res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK || confirmed != 1))
	{
		int			sqlstate = kingres_shard_result_sqlstate(res);
		char	   *remote = (res != NULL && PQresultErrorField(res, PG_DIAG_MESSAGE_PRIMARY) != NULL) ?
			pstrdup(PQresultErrorField(res, PG_DIAG_MESSAGE_PRIMARY)) :
			(res == NULL && timed_out) ?
			psprintf("the node did not answer within kingres_shard.node_timeout (%d s), nor in the time it gets after the command was cancelled", kingres_shard_node_timeout) :
			kingres_shard_conn_error_text(kc->conn);
		bool		said_no = (prepared && res != NULL &&
							   PQresultStatus(res) == PGRES_TUPLES_OK && confirmed == 0);

		if (PQstatus(kc->conn) != CONNECTION_OK)
			kc->have_error = true;
		PQclear(res);
		if (said_no)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_NOT_DURABLE),
					 errmsg("node %s did not confirm that its part of the transaction is durable on its synchronous standby",
							conn_label(kc)),
					 errdetail("The PREPARE TRANSACTION of the node was acknowledged, but its synchronous standby has not confirmed the record: the wait was cancelled or ended early, or the standby is down or too slow%s.",
							   timed_out ? " (the node did not answer within kingres_shard.node_timeout)" : ""),
					 errhint("The transaction was not committed anywhere.  Repeat it when the standby of the node is back, or remove the standby from synchronous_standby_names of the node.")));
		if (prepared)
			ereport(ERROR,
					(errcode(sqlstate != 0 ? sqlstate : ERRCODE_CONNECTION_FAILURE),
					 errmsg("could not confirm the prepared transaction on node %s: %s",
							conn_label(kc), remote),
					 errdetail("The transaction was prepared there, but the node did not say that the record is durable on its synchronous standby; nothing was committed.")));
		ereport(ERROR,
				(errcode(sqlstate != 0 ? sqlstate : ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not prepare the transaction on node %s: %s",
						conn_label(kc), remote),
				 (sqlstate == ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE ||
				  sqlstate == ERRCODE_OUT_OF_MEMORY) ?
				 errhint("Two-phase commit needs max_prepared_transactions greater than zero on every node of the cluster, and enough of them for the transactions in flight.") : 0));
	}
	PQclear(res);
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
	 * remote participants "off", "local" and "remote_write" are raised to "on"
	 * until it ends (S3-15; a write that the standby has not flushed is not a
	 * decision that survives its failover either).  Without synchronous
	 * standbys that costs nothing more than the flush.  The wait can still end
	 * without the standby's answer, when the backend is cancelled or
	 * terminated: the second phase checks for that (postcommit, S7-01).
	 */
	ForceSyncCommit();
	if (synchronous_commit < SYNCHRONOUS_COMMIT_ON)
		(void) set_config_option("synchronous_commit", "on", PGC_USERSET,
								 PGC_S_SESSION, GUC_ACTION_LOCAL, true, 0, false);
	decision_wait_mode = kingres_shard_sync_wait_mode();

	/* The unique-index check of the insert needs a snapshot. */
	PushActiveSnapshot(GetTransactionSnapshot());
	rel = table_open(relid, RowExclusiveLock);
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		Datum		values[5];
		bool		nulls[5] = {false, false, false, false, false};
		HeapTuple	tup;

		if (!kc->prepared)
			continue;
		values[0] = CStringGetTextDatum(kc->gid);
		values[1] = Int32GetDatum(kingres_shard_conn_node_id(kc->host, kc->port));
		values[2] = TimestampTzGetDatum(now);
		/* Not durable until the participant's standbys confirm its second phase (recovery.c). */
		values[3] = BoolGetDatum(kc->sync_exempt);
		/* A demoted participant is not checked for the loss of its part either (S9-06). */
		values[4] = BoolGetDatum(kc->sync_exempt);
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
	int			origin_group;
	TimeLineID	tli;
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
		exec_internal_ext(kc, "COMMIT TRANSACTION", kc->control_work);
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
			exec_internal_ext(kc, "COMMIT TRANSACTION", kc->control_work);
			reset_xact_state(kc);
		}
		return;
	}

	/* The global transaction id needs the local transaction id. */
	(void) GetTopTransactionId();
	fxid = GetTopFullTransactionId();
	/*
	 * The gid names the origin shard group and the timeline of the origin, not
	 * the origin node: after a failover the promoted standby of the group
	 * resolves what the old primary started (recovery.c).  A restore-point
	 * barrier is waited out before anything is prepared; the count of
	 * transactions in flight ends in the commit or abort callback.
	 */
	origin_group = kingres_shard_catalog()->local_group_id;
	if (origin_group < 0)
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("this node is not the primary of a shard group, so it cannot commit a transaction that writes on several nodes"),
				 errhint("The node was demoted by kingres.promote_node(); repeat the statement on the new primary.")));
	tli = GetWALInsertionTimeLine();
	kingres_shard_ha_commit_enter();

	kingres_shard_fault_point("before_prepare");
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		char	   *gid;
		int			target;

		if (kc->xact_depth <= 0 || !kc->wrote)
			continue;
		target = kingres_shard_conn_node_id(kc->host, kc->port);
		gid = psprintf("kingres_h%d_%u_%d_" UINT64_FORMAT "_%d", origin_group, tli,
					   MyProcPid, U64FromFullTransactionId(fxid), target);
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
						gid = psprintf("kingres_h%d_%u_%d_" UINT64_FORMAT "_%d_%d", origin_group,
									   tli, MyProcPid, U64FromFullTransactionId(fxid),
									   target, ++n);
						break;
					}
				}
			}
		}
		used_gids = lappend(used_gids, gid);
		/*
		 * The tag makes the name the extension's own: any role can prepare a transaction
		 * under any name, and recovery touches only the names whose tag verifies
		 * (recovery.c, S7-09).
		 */
		{
			char		tag[17];

			if (!kingres_shard_gid_tag(gid, tag))
				ereport(ERROR,
						(errcode(ERRCODE_INTERNAL_ERROR),
						 errmsg("could not compute the tag of a transaction identifier")));
			gid = psprintf("%s_%s", gid, tag);
		}
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
	bool		deferred = false;

	/*
	 * The second phase is sent only when the decision is known to be where the
	 * configuration of the group says it must be (S7-01).  The commit above
	 * waited for the synchronous standbys, but a cancel or a termination of
	 * this backend ends that wait with a warning and the commit completes
	 * all the same.  Telling the participants to commit then would let them
	 * keep a transaction that a standby promoted now knows nothing about.  So
	 * ask what the standbys have confirmed; if they have not confirmed this
	 * commit record, leave every participant prepared.  Recovery commits them
	 * once the decision is confirmed (or rolls them back, if the node that
	 * holds the decision is lost: what a promoted standby has is what counts).
	 */
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		if (kc->prepared)
		{
			hash_seq_term(&scan);
			if (!kingres_shard_sync_confirmed(XactLastCommitEnd, decision_wait_mode))
				deferred = true;
			break;
		}
	}
	if (deferred)
	{
		int			nleft = 0;

		hash_seq_init(&scan, conn_hash);
		while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
		{
			if (kc->prepared)
				nleft++;
			reset_xact_state(kc);
		}
		ereport(WARNING,
				(errmsg("the commit of this distributed transaction was not confirmed by the synchronous standby; the second phase was not sent"),
				 errdetail("The transaction is committed on this node, but the standby has not acknowledged the decision (the wait was cancelled or the backend is terminating), so the %d prepared transaction(s) on the other nodes stay prepared.", nleft),
				 errhint("Recovery commits them once the decision is on the standby, or rolls them back if a standby that does not have it is promoted; the outcome is decided by recovery, not by this session.")));
		return;
	}

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
					 errdetail_internal("%s", res ? kingres_shard_conn_error_text(kc->conn) : "no result")));
			PQclear(res);
			discard_connection(kc);
		}
		else
		{
			/* The participant's standby is asked below, for every participant at once (not a demoted one). */
			PQclear(res);
			if (kc->sync_exempt)
				reset_xact_state(kc);
			else
				kc->commit_sent = true;
			continue;
		}
		reset_xact_state(kc);
	}

	/*
	 * Is the commit record of each participant durable on its own standby?  The
	 * wait of COMMIT PREPARED ends for a cancel or a shutdown exactly as the one
	 * of PREPARE TRANSACTION does, and the statement answers as done (S8-01).  The
	 * transaction is committed everywhere and cannot be undone; what is at stake
	 * is that a failover of the participant right now brings its part back as
	 * prepared.  Recovery commits it again as long as the decision row exists
	 * (kingres_shard.decision_retention), and the client is told here.  The
	 * questions are sent to all participants first and answered afterwards, so
	 * that the commit does not wait for them one after another.
	 */
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		char		sql[96];

		if (!kc->commit_sent)
			continue;
		snprintf(sql, sizeof(sql), "SELECT " KINGRES_SHARD_SCHEMA ".internal_sync_confirm(%d)",
				 Min(kingres_shard_node_timeout, 5) * 1000);
		if (kc->conn == NULL || PQstatus(kc->conn) != CONNECTION_OK || !PQsendQuery(kc->conn, sql))
		{
			kc->have_error = true;
			kc->commit_sent = false;
			ereport(WARNING,
					(errmsg("could not ask node %s:%d whether the commit of \"%s\" is durable on its standby",
							kc->host, kc->port, kc->gid)));
			discard_connection(kc);
			reset_xact_state(kc);
		}
	}
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		bool		failed;
		PGresult   *res;
		bool		durable;

		if (!kc->commit_sent)
			continue;
		kc->commit_sent = false;
		res = bounded_get_last(kc->conn, false, &failed);
		durable = (!failed && res != NULL && PQresultStatus(res) == PGRES_TUPLES_OK &&
				   PQntuples(res) == 1 && strcmp(PQgetvalue(res, 0, 0), "t") == 0);
		if (!durable)
		{
			ereport(WARNING,
					(errmsg("node %s:%d has committed its part of the distributed transaction, but its standby has not confirmed the commit",
							kc->host, kc->port),
					 errdetail("The transaction is committed on every node.  If this node fails over to a standby that lacks the commit, its part comes back prepared and recovery commits it again, as long as the decision is kept (kingres_shard.decision_retention, %d s).",
							   kingres_shard_decision_retention)));
			if (failed || res == NULL)
				discard_connection(kc);
		}
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
	{
		/* No session was ever opened, but the state of a control call may still be set (S11-02). */
		switch (event)
		{
			case XACT_EVENT_COMMIT:
			case XACT_EVENT_PARALLEL_COMMIT:
			case XACT_EVENT_PREPARE:
			case XACT_EVENT_ABORT:
			case XACT_EVENT_PARALLEL_ABORT:
				end_of_xact();
				break;
			default:
				break;
		}
		return;
	}

	switch (event)
	{
		case XACT_EVENT_PARALLEL_PRE_COMMIT:
			check_session_lost();
			precommit(true);
			break;

		case XACT_EVENT_PRE_COMMIT:
			check_session_lost();
			precommit(false);
			break;

		case XACT_EVENT_PRE_PREPARE:
			/* A session that was closed under writes leaves xact_depth 0: look at the note too. */
			check_session_lost();
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
			in_xact_end = true;
			postcommit();
			end_of_xact();
			in_xact_end = false;
			kingres_shard_ha_commit_leave();
			break;

		case XACT_EVENT_PARALLEL_COMMIT:
		case XACT_EVENT_PREPARE:
			in_xact_end = true;
			hash_seq_init(&scan, conn_hash);
			while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
				reset_xact_state(kc);
			end_of_xact();
			in_xact_end = false;
			break;

		case XACT_EVENT_ABORT:
		case XACT_EVENT_PARALLEL_ABORT:
			in_xact_end = true;
			hash_seq_init(&scan, conn_hash);
			while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
				abort_connection(kc);
			end_of_xact();
			in_xact_end = false;
			kingres_shard_ha_commit_leave();
			break;
	}
}

/*
 * A statement that undoes a savepoint in the abort of a subtransaction (ROLLBACK TO SAVEPOINT, then
 * RELEASE SAVEPOINT) got no answer within node_timeout, or an error (S13-01).  An abort callback cannot
 * raise an error, so: a WARNING that names the node, the session is closed as every abandoned session
 * is (the node rolls the transaction back with it), and the transaction can no longer commit: the
 * commit or prepare of it raises an error that names the node (note_session_failed).
 */
static void
abort_savepoint_failed(KsConn *kc, const char *what, bool no_answer)
{
	bool		timed_out = no_answer && kc->conn != NULL && PQstatus(kc->conn) == CONNECTION_OK;
	/* what the session held that closing it loses (review S14-04); read before discard_connection clears it */
	bool		lost_work = kc->wrote || kc->wrote_user || kc->control_work || IsolationUsesXactSnapshot();

	ereport(WARNING,
			(errcode(timed_out ? ERRCODE_KINGRES_NODE_TIMEOUT : ERRCODE_CONNECTION_FAILURE),
			 errmsg("node %s did not complete %s%s; its session was closed",
					conn_label(kc), what,
					timed_out ? psprintf(" within %d seconds", kingres_shard_node_timeout) : ""),
			 errdetail("What this transaction did on that node is rolled back with the session; the transaction cannot be committed.")));
	if (lost_work)
		note_session_failed(kc, what);
	discard_connection(kc);
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
	if (event != SUBXACT_EVENT_PRE_COMMIT_SUB && event != SUBXACT_EVENT_COMMIT_SUB &&
		event != SUBXACT_EVENT_ABORT_SUB)
		return;

	curlevel = GetCurrentTransactionNestLevel();
	hash_seq_init(&scan, conn_hash);
	while ((kc = (KsConn *) hash_seq_search(&scan)) != NULL)
	{
		char		sql[64];

		if (kc->xact_depth < curlevel)
			continue;

		/*
		 * Review S14-01: the callback of the abort or the commit of a subtransaction must never raise
		 * an error where PostgreSQL cannot take one (an error in the abort of a subtransaction is
		 * aborted again, until the error stack overflows).  A session whose depth is deeper than the
		 * level that ends was left there by a level that could not undo it (the connection broke, or
		 * its statement was cut short): it is repaired here, closed, and the transaction is marked so
		 * that it cannot commit when the session held work; every level above skips it.
		 */
		if (kc->xact_depth > curlevel)
		{
			if (event != SUBXACT_EVENT_PRE_COMMIT_SUB)
			{
				if (kc->wrote || kc->wrote_user || kc->control_work || IsolationUsesXactSnapshot())
					note_session_failed(kc, "ROLLBACK TO SAVEPOINT");
				discard_connection(kc);
				continue;
			}
		}

		if (event == SUBXACT_EVENT_COMMIT_SUB)
		{
			/* the RELEASE was sent at PRE_COMMIT_SUB, where an error is legal: nothing can fail here */
			kc->xact_depth = curlevel - 1;
			continue;
		}

		if (event == SUBXACT_EVENT_PRE_COMMIT_SUB)
		{
			if (kc->xact_depth > curlevel)
				continue;			/* released by the level above; repaired at the commit or the abort */
			snprintf(sql, sizeof(sql), "RELEASE SAVEPOINT s%d", curlevel);

			/*
			 * S13-01: bounded by node_timeout whatever the control depth is (a call has been left
			 * when its subtransaction commits), and an error that names the node, not a wait that
			 * nobody can end.  Review S14-05: sent at PRE_COMMIT_SUB, as postgres_fdw does, so that
			 * an error aborts a subtransaction that is still in progress and nothing of a rolled-back
			 * subtransaction is merged into its parent.  A failure also keeps this transaction from
			 * committing: the sessions that were released before this one keep the work of a
			 * subtransaction that the error is about to roll back (the doomed note, which is the
			 * second protection: the abort then finds the savepoint released and says so).
			 */
			PG_TRY();
			{
				exec_internal_ext(kc, sql, true);
			}
			PG_CATCH();
			{
				/* a session that is still up and was only interrupted is rolled back normally */
				if (kc->conn == NULL || kc->have_error || PQstatus(kc->conn) != CONNECTION_OK)
					note_session_failed(kc, "RELEASE SAVEPOINT");
				PG_RE_THROW();
			}
			PG_END_TRY();
			continue;
		}
		else
		{
			PGresult   *res;

			/* A SET LOCAL made after the savepoint is undone with it. */
			kc->tz_applied[0] = '\0';

			/*
			 * An error ended the statement of this subtransaction while a
			 * fetch of its scan was in flight (S6-02), or interrupted a
			 * command of ours.  The answer must be read before the rollback
			 * can be sent.  The owner of the fetch still exists here (its
			 * memory goes later, and its reset callback lets go of the
			 * connection if that comes first): it reads the answer, and an
			 * error in it is kept for the owner, not raised.  What is left in
			 * flight after that has no owner (a command that an interrupt cut
			 * short): read and dropped.  A batch of the batch sender in flight is
			 * cancelled there, not waited for.
			 */
			if (kc->in_query)
			{
				kingres_shard_conn_quiesce_quiet(kc);
				if (kc->in_query)
					kingres_shard_conn_drop_inflight(kc);
			}
			if (kc->have_error || kc->conn == NULL ||
				PQstatus(kc->conn) != CONNECTION_OK)
			{
				/*
				 * A broken session at this level: every level above skips it (review S14-01).
				 * What it held is lost with it; a session that wrote for the user is noted when
				 * the connection is closed (discard_connection).
				 */
				/*
				 * xact_depth is not lowered here (review S14-01, P2C): the session is closed, and the callback of the
				 * next level up finds xact_depth above its own level and closes it the same way, noting it when it
				 * held work.  Lowering it made no test fail (t/118, t/119 with the line removed).
				 */
				kc->have_error = true;
				continue;
			}
			snprintf(sql, sizeof(sql), "ROLLBACK TO SAVEPOINT s%d", curlevel);
			res = kingres_shard_bounded_exec(kc->conn, sql, false);
			if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
			{
				bool		no_answer = (res == NULL);

				PQclear(res);
				abort_savepoint_failed(kc, "ROLLBACK TO SAVEPOINT", no_answer);
				continue;
			}
			PQclear(res);
			snprintf(sql, sizeof(sql), "RELEASE SAVEPOINT s%d", curlevel);
			res = kingres_shard_bounded_exec(kc->conn, sql, false);
			if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
			{
				bool		no_answer = (res == NULL);

				PQclear(res);
				abort_savepoint_failed(kc, "RELEASE SAVEPOINT", no_answer);
				continue;
			}
			PQclear(res);
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
	const char *params[7];
	PGresult   *res;

	params[0] = GetConfigOption("search_path", false, false);
	params[1] = GetConfigOption("DateStyle", false, false);
	params[2] = GetConfigOption("IntervalStyle", false, false);
	params[3] = GetConfigOption("standard_conforming_strings", false, false);
	params[4] = GetConfigOption("bytea_output", false, false);
	params[5] = GetConfigOption("extra_float_digits", false, false);
	params[6] = GetConfigOption("check_function_bodies", false, false);
	res = kingres_shard_exec_params(kc,
									 "SELECT pg_catalog.set_config('search_path', $1, true), "
									 "pg_catalog.set_config('DateStyle', $2, true), "
									 "pg_catalog.set_config('IntervalStyle', $3, true), "
									 "pg_catalog.set_config('standard_conforming_strings', $4, true), "
									 "pg_catalog.set_config('bytea_output', $5, true), "
									 "pg_catalog.set_config('extra_float_digits', $6, true), "
									 "pg_catalog.set_config('check_function_bodies', $7, true)",
									 7, NULL, params, true);
	PQclear(res);
	kc->tz_applied[0] = '\0';
	res = kingres_shard_exec(kc, stmt);
	PQclear(res);
	exec_internal(kc, KS_FIXED_SETTINGS_SQL "SELECT 1");
	kc->tz_applied[0] = '\0';
	sync_timezone(kc);
}

PG_FUNCTION_INFO_V1(kingres_internal_remote_extension_version);

/*
 * kingres.internal_remote_extension_version(host text, port int) returns text
 *
 * kingres_shard_remote_extension_version() for PL/pgSQL.  Superusers only.
 */
Datum
kingres_internal_remote_extension_version(PG_FUNCTION_ARGS)
{
	char	   *host;
	char	   *version;

	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for the internal functions of kingres_shard")));
	if (PG_ARGISNULL(0) || PG_ARGISNULL(1))
		PG_RETURN_NULL();
	host = text_to_cstring(PG_GETARG_TEXT_PP(0));
	version = kingres_shard_remote_extension_version(host, PG_GETARG_INT32(1));
	if (version == NULL)
		PG_RETURN_NULL();
	PG_RETURN_TEXT_P(cstring_to_text(version));
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
