/*-------------------------------------------------------------------------
 *
 * node_conn.h
 *	  libpq connections to other nodes of the cluster and the remote
 *	  transactions that ride on them.
 *
 * SPDX-License-Identifier: PostgreSQL
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

#include "libpq-fe.h"

/* Maximum lengths of the parts of a connection cache key. */
#define KS_CONN_HOST_LEN 256
#define KS_TZ_LEN 96

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
} KsConn;

extern void kingres_shard_conn_init(void);
extern int	kingres_shard_set_transmission_modes(void);
extern void kingres_shard_reset_transmission_modes(int nestlevel);
extern KsConn *kingres_shard_get_conn(const char *host, int port,
									   bool missing_ok, char **errmsg_out);
extern void kingres_shard_begin_xact(KsConn *kc);
extern PGresult *kingres_shard_exec(KsConn *kc, const char *sql);
extern PGresult *kingres_shard_exec_params(KsConn *kc, const char *sql,
											int nparams, const Oid *types,
											const char *const *values,
											bool throw_on_error);
extern void kingres_shard_report_error(KsConn *kc, PGresult *res,
										const char *sql);
extern int	kingres_shard_result_sqlstate(PGresult *res);
extern void kingres_shard_forget_xact(KsConn *kc);

/* Phase 1 limits on routed writes (design/shard-mvp.md, "Transactions"). */
extern void kingres_shard_note_remote_write(KsConn *kc);
extern bool kingres_shard_remote_write_pending(void);
extern void kingres_shard_note_local_write(void);
extern void kingres_shard_ignore_current_xid(void);

#endif							/* KINGRES_SHARD_NODE_CONN_H */
