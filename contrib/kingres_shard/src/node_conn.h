/*-------------------------------------------------------------------------
 *
 * node_conn.h
 *	  libpq connections to other nodes of the cluster and the remote
 *	  transactions that ride on them.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/node_conn.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_NODE_CONN_H
#define KINGRES_SHARD_NODE_CONN_H

#include "postgres.h"

#include "pg_compat.h"
#include "fmgr.h"
#include "nodes/pg_list.h"

/* Maximum lengths of the parts of a connection cache key. */
#define KS_CONN_HOST_LEN 256
#define KS_TZ_LEN 96
#define KS_GID_LEN 160

/*
 * What the owner of an asynchronous fetch does when something else wants the
 * connection (quiet false: read the answer, keep it, raise an error if the
 * answer is one) or when an abort needs it (quiet true: the same, but nothing
 * may be thrown; an error is kept for the owner).
 */
typedef void (*KsPendingFinish) (void *owner, bool quiet);

/*
 * A cached connection to one node, used by one local user.  The pointer is
 * stable for the life of the backend (entries are never removed from the
 * cache, only reconnected).
 */
typedef struct KsConn
{
	/* hash key: must be first and zero padded */
	char		host[KS_CONN_HOST_LEN];
	int			port;
	char		user[NAMEDATALEN];
	/* state */
	PGconn	   *conn;
	int			xact_depth;		/* 0: no remote transaction; n: n nesting levels */
	int			nstatements;	/* statements run in the current remote xact */
	bool		in_query;		/* a query is in flight on conn */
	bool		have_error;		/* connection state is unknown: discard it */
	/* TimeZone sent with SET LOCAL in the open remote transaction, or "" */
	char		tz_applied[KS_TZ_LEN];
	/* two-phase commit state of the current local transaction */
	bool		wrote;			/* the remote transaction has written */
	bool		prepared;		/* PREPARE TRANSACTION succeeded; gid is valid */
	/* COMMIT PREPARED was acknowledged and the durability question is not answered yet (S8-01) */
	bool		commit_sent;
	char		gid[KS_GID_LEN];
	/* the session runs with synchronous_commit raised to at least "on" (S7-07) */
	bool		sync_forced;
	/* the session was opened with the previous cluster secret (S9-03) */
	bool		via_previous;
	/* the participant is a node that is being demoted (S9-05): its standbys are not waited for */
	bool		sync_exempt;
	/*
	 * a statement of a user (outside of every control call) wrote through this session in the
	 * current transaction (S11-01): closing the session loses that write, so the transaction must
	 * not commit afterwards.  The writes of a control call are its own business: it decides per node.
	 */
	bool		wrote_user;
	/* an asynchronous fetch of one scan is in flight on conn */
	void	   *pending_owner;
	KsPendingFinish pending_finish;
} KsConn;

extern bool kingres_shard_previous_secret_used;

/*
 * The scope of a control-plane call (promote_node, add_node, ...), S10-02 and S11-01.  Inside the
 * dynamic extent of such a call every statement it sends to a node is bounded by node_timeout, and
 * the first statement of a new session runs with a lock_timeout of its own, so that a node that
 * accepts the session and does not answer ends the call with an answer that names it.  Nothing is
 * sent to a node when the call is left (S12-01).  The scope is a value that the call saves on entry and
 * restores on every exit (kingres_shard_control_call, or enter and leave around a PG_TRY), never a
 * flag that somebody must remember to clear: a statement of a user, which runs outside of every such
 * extent (the planner's remote statements, scans, fan-out, COPY, forwarded DDL), never reads it.
 */
typedef struct KsControlScope
{
	int			saved_depth;
} KsControlScope;

extern void kingres_shard_control_enter(KsControlScope *scope);
extern void kingres_shard_control_leave(KsControlScope *scope);
extern bool kingres_shard_control_active(void);
extern Datum kingres_shard_control_call(PGFunction body, FunctionCallInfo fcinfo);
extern void kingres_shard_conn_init(void);
extern int	kingres_shard_set_transmission_modes(void);
extern void kingres_shard_reset_transmission_modes(int nestlevel);
extern KsConn *kingres_shard_get_conn(const char *host, int port,
									   bool missing_ok, char **errmsg_out);
extern void kingres_shard_begin_xact(KsConn *kc);
extern PGresult *kingres_shard_exec(KsConn *kc, const char *sql);
extern void kingres_shard_exec_ddl(KsConn *kc, const char *stmt);
extern PGresult *kingres_shard_exec_params(KsConn *kc, const char *sql,
											int nparams, const Oid *types,
											const char *const *values,
											bool throw_on_error);
extern void kingres_shard_report_error(KsConn *kc, PGresult *res,
										const char *sql);
extern int	kingres_shard_result_sqlstate(PGresult *res);
extern void kingres_shard_forget_xact(KsConn *kc);
extern void kingres_shard_conn_drop_stale(List *nodes);

/*
 * Two-phase commit (design/shard-phase2a.md, section 1).  A connection that
 * runs a writing statement is marked with kingres_shard_note_remote_write();
 * at pre-commit a transaction with two or more writers (the local node counts
 * as one when it has a transaction id) prepares every remote writer and logs
 * the decision in kingres.dtx_log.
 */
/*
 * Raise (and later restore) synchronous_commit of the remote session to at
 * least "on", for a COMMIT PREPARED or ROLLBACK PREPARED sent from outside the
 * transaction that prepared (recovery, S7-07).  force returns false when the
 * session is not usable.
 */
extern bool kingres_shard_conn_force_sync(KsConn *kc);
extern char *kingres_shard_conn_error_text(PGconn *conn);
extern void kingres_shard_conn_exempt_sync(KsConn *kc);
extern bool kingres_shard_conn_local_sync(KsConn *kc);
extern void kingres_shard_conn_unforce_sync(KsConn *kc);
extern void kingres_shard_note_remote_write(KsConn *kc);
extern void kingres_shard_begin_write_xact(KsConn *kc);

/* An asynchronous fetch of one scan is in flight on kc. */
extern void kingres_shard_conn_set_pending(KsConn *kc, void *owner,
											KsPendingFinish finish);
extern void kingres_shard_conn_clear_pending(KsConn *kc, void *owner);
extern void kingres_shard_conn_quiesce(KsConn *kc);
extern void kingres_shard_conn_quiesce_quiet(KsConn *kc);
extern void kingres_shard_conn_abandon_pending(KsConn *kc, void *owner);
extern PGresult *kingres_shard_conn_collect_quiet(KsConn *kc);
extern void kingres_shard_conn_drop_inflight(KsConn *kc);

/* The node id of the registered node at host:port, or 0. */
extern int	kingres_shard_conn_node_id(const char *host, int port);

/*
 * Run a command on a connection and wait for its last result for at most
 * kingres_shard.node_timeout seconds (S2-12).  Returns NULL when the wait ran
 * out or the connection failed; the connection is then not usable.  With
 * interruptible false (the commit and abort callbacks) the wait ignores
 * interrupts, which is why it is bounded.
 */
extern PGresult *kingres_shard_bounded_exec(PGconn *conn, const char *sql, bool interruptible);
extern PGresult *kingres_shard_exec_cancel_after_timeout(PGconn *conn, const char *sql);

/* The kingres_shard version host:port reports, asked without a claim (NULL: cannot be asked, "": none). */
extern char *kingres_shard_remote_extension_version(const char *host, int port);

/* The id add_node() is about to give the node at host:port (see forward_auth.c). */
extern void kingres_shard_conn_expect_node(const char *host, int port, int node_id);

/* Failure injection for tests (fault.c). */
extern void kingres_shard_fault_init(void);
extern void kingres_shard_fault_point(const char *point);

/* Ask the node to stop what its session runs; waits for the request for a bounded time only. */
extern void kingres_shard_cancel_with_deadline(PGconn *conn);
extern const char *kingres_shard_fault_spec(void);
extern int	kingres_shard_server_major(void);
extern int	kingres_shard_peer_major(int actual);
extern void kingres_shard_fault_set_scope(const char *scope);

/*
 * A connection that belongs to no transaction of this backend: a session on
 * host:port as the current user, with the options of a node-to-node session.
 * The caller owns it (PQfinish).  Used by the control sessions of a bucket
 * move (bucket_move.c).  NULL and *errmsg_out on failure.
 */
extern int kingres_shard_connect_failure_code(void);
extern int kingres_shard_connect_refusal_sqlstate(void);
extern void kingres_shard_conn_abandon(KsConn *kc);
extern PGconn *kingres_shard_connect_control(const char *host, int port,
											  char **errmsg_out);

/* Text of a connection error with secret paths of node_conninfo removed. */
extern char *kingres_shard_scrub_conn_error(const char *msg);

#endif							/* KINGRES_SHARD_NODE_CONN_H */
