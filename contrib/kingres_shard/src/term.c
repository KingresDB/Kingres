/*-------------------------------------------------------------------------
 *
 * term.c
 *	  The catalog term (review S7-06 and S7-04).
 *
 * The catalog has one writer, the primary of the map group, and a promotion
 * of the map group moves that role to another node.  The old primary cannot
 * always be told (it is down, or cut off), and when it is restarted it still
 * holds the old catalog and still believes it is the writer.  Nothing in a
 * version number can tell the two apart: both count changes.  So the catalog
 * has a term (kingres.map_version.term), an integer that every promotion
 * raises, and that travels with everything a node says to another:
 *
 *	  - every catalog push carries the term of its snapshot, and
 *		internal_apply_catalog() refuses (KS002) a snapshot whose term is older
 *		than the term of the node that receives it;
 *	  - every remote transaction a node opens on behalf of a statement says
 *		which term the sender's catalog has (kingres_shard.forward_term, set
 *		with SET LOCAL in the first command of the transaction, like the label
 *		of a distributed transaction), and the receiving node refuses (KS002)
 *		a statement of a sender whose term is older than its own.  The claim
 *		can only be made by a session that proved to be a node session
 *		(forward_auth.c); a plain client that sets it has no effect.
 *
 * A refusal says which term the refusing node has.  The sender that gets it
 * learns what it needs: a node that is the map group primary according to its
 * own catalog has been superseded; it refuses every catalog change from then
 * on (kingres_shard_refuse_if_superseded) until it is rebuilt as a standby
 * of the new primary.  Any other node was only unreachable when the
 * promotion was made: it fetches the newer catalog from the node that refused
 * it, before it does anything else.  The same happens to a node that receives
 * a claim with a term newer than its own.
 *
 * Learning writes to the catalog in a transaction that must survive the
 * failure of the statement that found out, so it is done by a short-lived
 * background worker (recovery.c runs it, mode "learn"), as the superuser, and
 * the backend waits for it unless it holds the locks of a catalog change
 * itself (the map primary that is refused in the middle of one).
 *
 * Shared memory: none of its own.  The immediate effect of a refusal on the
 * map primary that got it is a number kept in ha.c (the worker records it in
 * the catalog a moment later).
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/term.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include <ctype.h>

#include "access/parallel.h"
#include "access/xact.h"
#include "commands/extension.h"
#include "executor/spi.h"
#include "miscadmin.h"
#include "postmaster/bgworker.h"
#include "storage/latch.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/snapmgr.h"
#include "utils/timestamp.h"
#include "utils/wait_classes.h"

#include "catalog.h"
#include "ha.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "term.h"

/* What the sender of this transaction said: -1 when it said nothing. */
static int64 claim_term = -1;
static int	claim_origin = 0;

/* The GUC storage (the values are only a vehicle for the hooks). */
static char *forward_term_guc = NULL;
static int	forward_origin_guc = 0;

/* The transaction and claim that were checked last. */
static TimestampTz checked_xact_ts = 0;
static int64 checked_claim = -2;

/*
 * Only a node session may make a claim: a session that did not ask to be one
 * (kingres_shard.forwarded) cannot set it, not even to add it to a role or a
 * database.  The check of the proof itself is the one of every forwarded
 * statement (forward_auth.c); the claim is read only after it.
 */
static bool
check_forward_term(char **newval, void **extra, GucSource source)
{
	int64	   *v;
	const char *s = *newval;
	int64		n = -1;

	if (s != NULL && s[0] != '\0')
	{
		const char *p;

		if (!kingres_shard_forwarded)
		{
			GUC_check_errmsg("kingres_shard.forward_term can only be set by a session opened by another node");
			return false;
		}
		if (strlen(s) > 18)
			return false;
		for (p = s; *p != '\0'; p++)
			if (!isdigit((unsigned char) *p))
				return false;
		n = (int64) strtoll(s, NULL, 10);
	}
	v = (int64 *) guc_malloc(LOG, sizeof(int64));
	if (v == NULL)
		return false;
	*v = n;
	*extra = v;
	return true;
}

static void
assign_forward_term(const char *newval, void *extra)
{
	claim_term = extra != NULL ? *(int64 *) extra : -1;
}

static bool
check_forward_origin(int *newval, void **extra, GucSource source)
{
	if (*newval != 0 && !kingres_shard_forwarded)
	{
		GUC_check_errmsg("kingres_shard.forward_origin can only be set by a session opened by another node");
		return false;
	}
	return true;
}

static void
assign_forward_origin(int newval, void *extra)
{
	claim_origin = newval;
}

void
kingres_shard_term_init(void)
{
	DefineCustomStringVariable("kingres_shard.forward_term",
							   "Set by the router on the remote transactions of a node; not for users.",
							   "The term of the catalog of the node that sends the statement.",
							   &forward_term_guc,
							   "",
							   PGC_USERSET,
							   GUC_NOT_IN_SAMPLE | GUC_NO_SHOW_ALL | GUC_NO_RESET_ALL,
							   check_forward_term, assign_forward_term, NULL);
	DefineCustomIntVariable("kingres_shard.forward_origin",
							"Set by the router on the remote transactions of a node; not for users.",
							"The id of the node that sends the statement.",
							&forward_origin_guc,
							0, 0, INT_MAX,
							PGC_USERSET,
							GUC_NOT_IN_SAMPLE | GUC_NO_SHOW_ALL | GUC_NO_RESET_ALL,
							check_forward_origin, assign_forward_origin, NULL);
}

/*
 * "; SET LOCAL kingres_shard.forward_term = ...; SET LOCAL
 * kingres_shard.forward_origin = ..." for the first command of a remote
 * transaction.
 */
void
kingres_shard_append_term_claim(StringInfo buf)
{
	int64		term = kingres_shard_term_now();
	int			node = kingres_shard_local_node_id_raw();

	/*
	 * Read without the catalog cache: loading the cache can close the cached
	 * sessions (node_conn.c), and this runs while one of them is being set up.
	 */
	if (term < 0 || node == 0)
		return;
	/* The origin first: the term is checked at the statement that follows it. */
	appendStringInfo(buf, "; SET LOCAL kingres_shard.forward_origin = '%d'"
					 "; SET LOCAL kingres_shard.forward_term = '" INT64_FORMAT "'",
					 node, term);
}

/*
 * The number after "kingres-term=" in the detail of a refusal.
 */
bool
kingres_shard_parse_term_detail(const char *detail, int64 *term)
{
	const char *p;

	if (detail == NULL)
		return false;
	p = strstr(detail, "kingres-term=");
	if (p == NULL)
		return false;
	p += strlen("kingres-term=");
	if (!isdigit((unsigned char) *p))
		return false;
	*term = (int64) strtoll(p, NULL, 10);
	return true;
}

/*
 * Is this node the map group primary according to its own catalog?
 */
static bool
is_map_primary(const KsCatalog *cat)
{
	const KsNode *self = kingres_shard_node_by_id(cat->local_node_id);

	return self != NULL && self->group_id == KINGRES_SHARD_MAP_GROUP && self->is_primary;
}

/*
 * kingres_shard_refuse_if_superseded
 *
 * A map group primary that has been told of a newer term does not change the
 * catalog (S7-06).
 */
void
kingres_shard_refuse_if_superseded(void)
{
	const KsCatalog *cat = kingres_shard_catalog();
	int64		seen = Max(cat->superseded_by_term, kingres_shard_ha_superseded_term());

	if (seen > cat->term)
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_WRONG_NODE),
				 errmsg("this node has been superseded as the map group primary"),
				 errdetail("Its catalog has term " INT64_FORMAT ", the cluster is at term " INT64_FORMAT ".",
						   cat->term, seen),
				 errhint("Fence it and rebuild it as a standby of the current map group primary; it changes nothing in the catalog until then.")));
}

/*
 * Ask a background worker to take what this node has learned into the
 * catalog.  See the file header.
 */
void
kingres_shard_learn_term(int64 term, int peer_node_id, bool wait)
{
	const KsCatalog *cat = kingres_shard_catalog();
	BackgroundWorker bgw;
	BackgroundWorkerHandle *handle;
	pid_t		pid;
	bool		mapnode;

	if (!cat->present || cat->local_node_id == 0 || term <= cat->term)
		return;
	mapnode = is_map_primary(cat);
	if (mapnode)
		kingres_shard_ha_note_superseded(term);
	/* The map primary may hold the locks of a catalog change that the worker needs. */
	if (mapnode)
		wait = false;
	if (RecoveryInProgress() || IsParallelWorker() || MyDatabaseId == InvalidOid)
		return;

	memset(&bgw, 0, sizeof(bgw));
	bgw.bgw_flags = BGWORKER_SHMEM_ACCESS | BGWORKER_BACKEND_DATABASE_CONNECTION;
	bgw.bgw_start_time = BgWorkerStart_RecoveryFinished;
	bgw.bgw_restart_time = BGW_NEVER_RESTART;
	snprintf(bgw.bgw_library_name, BGW_MAXLEN, "kingres_shard");
	snprintf(bgw.bgw_function_name, BGW_MAXLEN, "kingres_shard_recovery_worker_main");
	snprintf(bgw.bgw_name, BGW_MAXLEN, "kingres_shard learn worker");
	snprintf(bgw.bgw_type, BGW_MAXLEN, "kingres_shard learn worker");
	bgw.bgw_main_arg = ObjectIdGetDatum(MyDatabaseId);
	snprintf(bgw.bgw_extra, BGW_EXTRALEN, "learn %d %d " INT64_FORMAT " %d",
			 kingres_shard_node_timeout, kingres_shard_node_connect_timeout,
			 term, peer_node_id);
	bgw.bgw_notify_pid = MyProcPid;
	if (!RegisterDynamicBackgroundWorker(&bgw, &handle))
	{
		ereport(LOG,
				(errmsg("kingres_shard: no background worker slot is free to take catalog term " INT64_FORMAT " into the catalog",
						term)));
		return;
	}
	if (!wait)
		return;
	if (WaitForBackgroundWorkerStartup(handle, &pid) == BGWH_STARTED)
	{
		TimestampTz deadline = TimestampTzPlusMilliseconds(GetCurrentTimestamp(),
														   (int64) kingres_shard_node_timeout * 1000);

		for (;;)
		{
			pid_t		wpid;

			if (GetBackgroundWorkerPid(handle, &wpid) == BGWH_STOPPED)
				break;
			if (GetCurrentTimestamp() >= deadline)
			{
				TerminateBackgroundWorker(handle);
				(void) WaitForBackgroundWorkerShutdown(handle);
				break;
			}
			(void) WaitLatch(MyLatch, WL_LATCH_SET | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH,
							 20, PG_WAIT_EXTENSION);
			ResetLatch(MyLatch);
			CHECK_FOR_INTERRUPTS();
		}
	}
	kingres_shard_catalog_invalidate();
}

/*
 * kingres_shard_check_claimed_term
 *
 * See term.h.
 */
void
kingres_shard_check_claimed_term(void)
{
	const KsCatalog *cat;
	TimestampTz ts;

	if (claim_term < 0 || IsParallelWorker() || !IsTransactionState())
		return;
	ts = GetCurrentTransactionStartTimestamp();
	if (ts == checked_xact_ts && claim_term == checked_claim)
		return;
	cat = kingres_shard_catalog();
	if (!cat->present)
		return;
	if (claim_term > cat->term)
	{
		/* This node missed a promotion: bring it up to date before anything else. */
		kingres_shard_learn_term(claim_term, claim_origin, true);
		cat = kingres_shard_catalog();
		if (claim_term > cat->term)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_STALE_MAP),
					 errmsg("the catalog of this node (term " INT64_FORMAT ") is older than the sender's (term " INT64_FORMAT ")",
							cat->term, claim_term),
					 errhint("The node could not fetch the newer catalog; repeat the statement, or run kingres.sync_catalog() on the map group primary.")));
	}
	if (claim_term < cat->term)
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_TERM),
				 errmsg("the catalog of the sending node (term " INT64_FORMAT ") has been superseded by term " INT64_FORMAT,
						claim_term, cat->term),
				 errdetail_internal("kingres-term=" INT64_FORMAT, cat->term),
				 errhint("The sender must reload its catalog from the current map group primary; a map group primary that was replaced must be rebuilt as a standby.")));
	checked_xact_ts = ts;
	checked_claim = claim_term;
}

/* ---------------------------------------------------------------------------
 * The worker
 * ---------------------------------------------------------------------------
 */

/* The term of a snapshot document, or -1. */
static int64
snapshot_term(const char *snapshot)
{
	Oid			argtypes[1] = {TEXTOID};
	Datum		values[1];
	int64		term = -1;

	values[0] = CStringGetTextDatum(snapshot);
	if (SPI_execute_with_args("SELECT (($1::pg_catalog.jsonb) OPERATOR(pg_catalog.->>) 'term')::pg_catalog.int8",
							  1, argtypes, values, NULL, true, 1) == SPI_OK_SELECT &&
		SPI_processed == 1)
	{
		bool		isnull;
		Datum		d = SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull);

		if (!isnull)
			term = DatumGetInt64(d);
	}
	return term;
}

/* A node to fetch the catalog from, copied out of the catalog cache (which an apply invalidates). */
typedef struct PullNode
{
	int			node_id;
	char	   *host;
	int			port;
} PullNode;

static bool
pull_from(const PullNode *peer, int64 want)
{
	char	   *err = NULL;
	KsConn	   *kc = kingres_shard_get_conn(peer->host, peer->port, true, &err);
	PGresult   *res;
	char	   *snapshot;
	Oid			argtypes[1] = {TEXTOID};
	Datum		values[1];

	if (kc == NULL)
		return false;
	res = kingres_shard_exec_params(kc,
									"SELECT " KINGRES_SHARD_SCHEMA ".internal_catalog_snapshot()::pg_catalog.text",
									0, NULL, NULL, false);
	if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK || PQntuples(res) != 1)
	{
		PQclear(res);
		return false;
	}
	snapshot = pstrdup(PQgetvalue(res, 0, 0));
	PQclear(res);
	if (snapshot_term(snapshot) < want)
		return false;
	values[0] = CStringGetTextDatum(snapshot);
	if (SPI_execute_with_args("SELECT " KINGRES_SHARD_SCHEMA ".internal_apply_catalog($1::pg_catalog.jsonb)",
							  1, argtypes, values, NULL, false, 0) != SPI_OK_SELECT)
		return false;
	return true;
}

/*
 * What the worker does; it runs as the bootstrap superuser in one transaction
 * of its own.  A failure is logged and ends the worker; the caller notices
 * that the term did not arrive and reports a retryable error.  Everything it
 * needs from the catalog cache is copied first: applying a catalog
 * invalidates the cache.
 */
void
kingres_shard_term_worker_run(const char *extra)
{
	int			nt;
	int			ct;
	long long	term;
	int			peer;
	MemoryContext oldcxt = CurrentMemoryContext;

	if (sscanf(extra, "%*s %d %d %lld %d", &nt, &ct, &term, &peer) != 4)
		return;

	StartTransactionCommand();
	PushActiveSnapshot(GetTransactionSnapshot());
	PG_TRY();
	{
		if (!RecoveryInProgress() && OidIsValid(get_extension_oid("kingres_shard", true)))
		{
			const KsCatalog *cat = kingres_shard_catalog();

			if (cat->present && cat->local_node_id != 0 && cat->term < term)
			{
				int64		have = cat->term;
				bool		mapnode = is_map_primary(cat);
				List	   *cands = NIL;
				ListCell   *lc;
				bool		done = false;

				/* The peer first, then every other active primary. */
				foreach(lc, cat->nodes)
				{
					const KsNode *n = (const KsNode *) lfirst(lc);
					PullNode   *pn;

					if (n->node_id == cat->local_node_id || !n->is_active || !n->is_primary)
						continue;
					pn = palloc(sizeof(PullNode));
					pn->node_id = n->node_id;
					pn->host = pstrdup(n->host);
					pn->port = n->port;
					if (n->node_id == peer)
						cands = lcons(pn, cands);
					else
						cands = lappend(cands, pn);
				}
				cat = NULL;		/* not valid any more once anything is applied */

				if (SPI_connect() != SPI_OK_CONNECT)
					elog(ERROR, "SPI_connect failed");
				if (mapnode)
				{
					Oid			argtypes[1] = {INT8OID};
					Datum		values[1];

					/* Superseded: recorded, nothing is pulled; see the file header. */
					values[0] = Int64GetDatum((int64) term);
					if (SPI_execute_with_args("UPDATE " KINGRES_SHARD_SCHEMA ".map_version SET superseded_by_term = $1 "
											  "WHERE superseded_by_term OPERATOR(pg_catalog.<) $1 AND term OPERATOR(pg_catalog.<) $1",
											  1, argtypes, values, NULL, false, 0) != SPI_OK_UPDATE)
						elog(ERROR, "could not record the newer term");
					ereport(LOG,
							(errmsg("kingres_shard: this node was the map group primary of catalog term " INT64_FORMAT " and has been superseded by term %lld",
									have, term)));
				}
				else
				{
					foreach(lc, cands)
					{
						if (pull_from((const PullNode *) lfirst(lc), (int64) term))
						{
							done = true;
							break;
						}
					}
					if (done)
						ereport(LOG,
								(errmsg("kingres_shard: took the catalog of term %lld from the cluster", term)));
					else
						ereport(LOG,
								(errmsg("kingres_shard: could not fetch the catalog of term %lld", term)));
				}
				SPI_finish();
			}
		}
		PopActiveSnapshot();
		CommitTransactionCommand();
	}
	PG_CATCH();
	{
		MemoryContextSwitchTo(oldcxt);
		EmitErrorReport();
		FlushErrorState();
		AbortCurrentTransaction();
	}
	PG_END_TRY();
}
