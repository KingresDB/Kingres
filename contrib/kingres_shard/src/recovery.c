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
 * SPDX-License-Identifier: AGPL-3.0-only
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
	int			group_id;
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
 * A gid of ours, split.  Two formats exist:
 *
 *	  kingres_g<origin group>_<timeline>_<pid>_<full xid>_<target>[_<n>]
 *	  kingres_<origin node>_<pid>_<full xid>_<target>[_<n>]	(0.4 and older)
 *
 * The first names the group of the origin, so that the node that became the
 * primary of that group after a failover resolves what the old one started,
 * and the timeline the origin prepared on.  The second names the origin node;
 * transactions prepared by 0.4 and still pending at an upgrade carry it, and
 * the group is looked up from the node row.
 */
typedef struct ParsedGid
{
	bool		is_group_format;
	int			origin;			/* a group id, or (old format) a node id */
	uint32		tli;			/* 0 in the old format */
	int			pid;
	uint64		fxid;
} ParsedGid;

/* false when the gid is not one of ours. */
static bool
parse_gid(const char *gid, ParsedGid *out)
{
	char	   *copy = pstrdup(gid);
	char	   *tok[10];
	int			n = 0;
	char	   *p = copy;
	bool		ok;
	bool		grp;
	int			first;			/* the first numeric token */
	int			nnum;

	while (n < 10)
	{
		tok[n++] = p;
		p = strchr(p, '_');
		if (p == NULL)
			break;
		*p++ = '\0';
	}
	/*
	 * Every field a number (the group with a "g" in front): a prepared
	 * transaction of a user that merely starts with "kingres_" is not ours and
	 * is never touched (review finding S2-20).
	 */
	ok = (n >= 5 && strcmp(tok[0], "kingres") == 0);
	grp = ok && tok[1][0] == 'g';
	if (ok)
	{
		int			t;

		/* group format: 6 or 7 tokens; old format: 5 or 6 */
		if (grp)
			ok = (n == 6 || n == 7);
		else
			ok = (n == 5 || n == 6);
		first = 1;
		nnum = n;
		for (t = first; ok && t < nnum; t++)
		{
			const char *c = tok[t];

			if (t == 1 && grp)
				c++;
			if (*c == '\0')
				ok = false;
			for (; ok && *c != '\0'; c++)
				if (*c < '0' || *c > '9')
					ok = false;
		}
	}
	if (ok)
	{
		out->is_group_format = grp;
		if (grp)
		{
			out->origin = atoi(tok[1] + 1);
			out->tli = (uint32) strtoul(tok[2], NULL, 10);
			out->pid = atoi(tok[3]);
			out->fxid = strtoull(tok[4], NULL, 10);
		}
		else
		{
			out->origin = atoi(tok[1]);
			out->tli = 0;
			out->pid = atoi(tok[2]);
			out->fxid = strtoull(tok[3], NULL, 10);
		}
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
origin_decision(const char *gid, uint64 fxid, int pid, bool earlier_timeline, bool *commit)
{
	/*
	 * (a)  A transaction of an earlier timeline is over by definition: the
	 * backend that ran it was on the primary that lost the group (or on this
	 * node before it was promoted, and every transaction of that time ended
	 * with the promotion).  Its transaction id says nothing here: the new
	 * timeline may have used the same number for another transaction.
	 */
	if (!earlier_timeline && xid_possibly_running(fxid, pid))
		return false;
	/* (b), strictly after (a) */
	*commit = decision_logged(gid);
	return true;
}

/*
 * Delete the rows of a node that are no longer prepared there.  node_ids are
 * the ids of the node and of every other node row of its shard group: the rows
 * of a decision name the node that was the primary when the transaction
 * prepared, and after a failover the transaction is looked for (and gone) on
 * the group's new primary.
 */
static void
purge_log(int node_id, const int *node_ids, int nids, TimestampTz listed_at, List *still_prepared, List *listed)
{
	Oid			argtypes[3] = {INT4ARRAYOID, TIMESTAMPTZOID, TEXTARRAYOID};
	Datum		values[3];
	Datum	   *elems;
	int			n = list_length(still_prepared);
	int			i = 0;
	ListCell   *lc;

	elems = palloc(sizeof(Datum) * Max(n, 1));
	foreach(lc, still_prepared)
		elems[i++] = CStringGetTextDatum((char *) lfirst(lc));
	{
		Datum	   *ids = palloc(sizeof(Datum) * Max(nids, 1));
		int			k;

		for (k = 0; k < nids; k++)
			ids[k] = Int32GetDatum(node_ids[k]);
		values[0] = PointerGetDatum(construct_array_builtin(ids, nids, INT4OID));
	}
	values[1] = TimestampTzGetDatum(listed_at);
	values[2] = PointerGetDatum(construct_array_builtin(elems, n, TEXTOID));
	if (SPI_execute_with_args("DELETE FROM " KINGRES_SHARD_SCHEMA ".dtx_log "
							  "WHERE node_id OPERATOR(pg_catalog.=) ANY ($1) "
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
 * Remember, in kingres.dtx_incident, that a prepared transaction of an earlier
 * timeline had no commit decision and was rolled back.  The origin that was lost
 * may have committed it and sent COMMIT PREPARED to some participants before its
 * decision reached the standby that was promoted (asynchronous replication): this
 * is the one case in which a transaction can end up committed on some nodes and
 * rolled back on others, and the row is how an operator finds out which
 * transactions to look at.  With a synchronous standby the decision is on the
 * promoted node before any participant is told to commit, and the rows are
 * only the rollbacks of transactions that never committed.
 */
static void
record_incident(const char *gid, int node_id, uint32 gid_tli, TimeLineID current_tli)
{
	Oid			argtypes[4] = {TEXTOID, INT4OID, INT4OID, INT4OID};
	Datum		values[4];

	if (!OidIsValid(kingres_shard_catalog_relid("dtx_incident")))
		return;
	values[0] = CStringGetTextDatum(gid);
	values[1] = Int32GetDatum(node_id);
	values[2] = Int32GetDatum((int32) gid_tli);
	values[3] = Int32GetDatum((int32) current_tli);
	if (SPI_execute_with_args("INSERT INTO " KINGRES_SHARD_SCHEMA ".dtx_incident "
							  "(gid, node_id, gid_timeline, current_timeline, detected_at, kind) "
							  "VALUES ($1, $2, $3, $4, pg_catalog.clock_timestamp(), 'rolled_back_after_failover') "
							  "ON CONFLICT (gid) DO NOTHING",
							  4, argtypes, values, NULL, false, 0) != SPI_OK_INSERT)
		elog(ERROR, "could not record in kingres.dtx_incident");
	ereport(WARNING,
			(errmsg("kingres_shard recovery: prepared transaction \"%s\" on node %d belongs to timeline %u and has no commit decision on this node (timeline %u); it was rolled back",
					gid, node_id, gid_tli, current_tli),
			 errdetail("If the group lost its primary and the commit of this transaction had not reached this node, other nodes may have committed it."),
			 errhint("Look at the row of this gid in kingres.dtx_incident.")));
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
	return kingres_shard_recover_prepared_with(0, 0, NULL, 0);
}

/*
 * The same, and also on one more node that the catalog does not list as a
 * primary (extra_host is not NULL): the standby that promote_node() is about to
 * make the primary of its group.  What its group's old primary left prepared on it
 * (a switch of a bucket move that was decided, a catalog change) holds row locks
 * on the catalog tables that promote_node() writes there, so it is resolved first.
 */
int
kingres_shard_recover_prepared_with(int extra_id, int extra_group, const char *extra_host, int extra_port)
{
	MemoryContext callercxt = CurrentMemoryContext;
	const KsCatalog *cat = kingres_shard_catalog();
	List	   *nodes = NIL;
	ListCell   *lc;
	int			my_group;
	TimeLineID	current_tli;
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

	/*
	 * The node resolves what its shard group originated, and only while the
	 * catalog calls it the primary of that group: a node that was demoted by
	 * promote_node() (group -2) is not the origin of anything any more.
	 */
	current_tli = GetWALInsertionTimeLine();
	my_group = cat->local_group_id;
	if (my_group < 0)
		return 0;
	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		RecNode    *r;

		if (!n->is_active || !n->is_primary)
			continue;
		r = MemoryContextAlloc(callercxt, sizeof(RecNode));
		r->node_id = n->node_id;
		r->group_id = n->group_id;
		r->host = MemoryContextStrdup(callercxt, n->host);
		r->port = n->port;
		nodes = lappend(nodes, r);
	}
	if (extra_host != NULL)
	{
		RecNode    *r = MemoryContextAlloc(callercxt, sizeof(RecNode));

		r->node_id = extra_id;
		r->group_id = extra_group;
		r->host = MemoryContextStrdup(callercxt, extra_host);
		r->port = extra_port;
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
			r->group_id = -1;
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
			/*
			 * One command per message: the extended protocol refuses several
			 * commands in one statement, and a refused SET would leave the
			 * remote database's own settings (possibly hostile ones) in
			 * force for this session without anybody noticing.
			 */
			static const char *const limited[] = {"statement_timeout", "lock_timeout"};
			bool		bounded = true;

			for (int s = 0; s < lengthof(limited) && bounded; s++)
			{
				char	   *set = psprintf("SET %s = %d", limited[s], kingres_shard_node_timeout * 1000);
				PGresult   *sres = kingres_shard_exec_params(kc, set, 0, NULL, NULL, false);

				bounded = (sres != NULL && PQresultStatus(sres) == PGRES_COMMAND_OK);
				if (!bounded)
					ereport(LOG,
							(errmsg("kingres_shard recovery: could not bound the session on node %d, skipped",
									r->node_id),
							 errdetail_internal("%s", sres ? PQresultErrorMessage(sres) : "no result")));
				PQclear(sres);
				pfree(set);
			}
			if (!bounded)
				continue;
		}
		listed_at = GetCurrentTimestamp();
		/* Both formats; parse_gid() and the group of the origin say which are ours. */
		params[0] = "kingres\\_%";
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
			ParsedGid	pg;
			int			origin_group;
			bool		earlier;
			bool		commit;
			char	   *sql;

			if (!parse_gid(gid, &pg))
				continue;
			/* The old format names a node: the group is the one of that node's row. */
			if (pg.is_group_format)
				origin_group = pg.origin;
			else
			{
				const KsNode *on = kingres_shard_node_by_id(pg.origin);

				origin_group = on ? on->group_id : -1;
			}
			if (origin_group != my_group)
				continue;
			earlier = pg.is_group_format && (TimeLineID) pg.tli < current_tli;

			if (!origin_decision(gid, pg.fxid, pg.pid, earlier, &commit))
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
				if (!commit && earlier)
					record_incident(gid, r->node_id, pg.tli, current_tli);
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

		{
			int		   *ids = palloc(sizeof(int) * (list_length(cat->nodes) + 1));
			int			nids = 0;
			ListCell   *lcn;

			ids[nids++] = r->node_id;
			if (r->node_id != 0)
				foreach(lcn, cat->nodes)
				{
					KsNode	   *gn = (KsNode *) lfirst(lcn);

					if (gn->group_id == r->group_id && gn->node_id != r->node_id)
						ids[nids++] = gn->node_id;
				}
			purge_log(r->node_id, ids, nids, listed_at, still_prepared, gids);
		}
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

	if (OidIsValid(kingres_shard_catalog_relid("dtx_incident")) &&
		SPI_execute("DELETE FROM " KINGRES_SHARD_SCHEMA ".dtx_incident "
					"WHERE detected_at OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) pg_catalog.make_interval(days => 30))",
					false, 0) != SPI_OK_DELETE)
		elog(ERROR, "could not clean kingres.dtx_incident");

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

/* One pass: a worker per database, one at a time.  mode: "recovery" or "deadlock". */
static void
launch_workers(const char *mode)
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
		snprintf(bgw.bgw_name, BGW_MAXLEN, "kingres_shard %s worker", mode);
		snprintf(bgw.bgw_type, BGW_MAXLEN, "kingres_shard %s worker", mode);
		bgw.bgw_main_arg = ObjectIdGetDatum(lfirst_oid(lc));
		/*
		 * The values the node-to-node settings have in the server configuration:
		 * the worker forces them (fix_worker_settings), so that a setting stored
		 * for its database or role cannot change how long a pass waits (S3-16).
		 */
		snprintf(bgw.bgw_extra, BGW_EXTRALEN, "%s %d %d", mode,
				 kingres_shard_node_timeout, kingres_shard_node_connect_timeout);
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
	TimestampTz next_recovery;
	TimestampTz next_deadlock;

	pqsignal(SIGHUP, SignalHandlerForConfigReload);
	pqsignal(SIGTERM, die);
	BackgroundWorkerUnblockSignals();
	BackgroundWorkerInitializeConnection(NULL, NULL, 0);

	next_recovery = TimestampTzPlusMilliseconds(GetCurrentTimestamp(),
												Max(kingres_shard_recovery_interval, 1) * 1000L);
	next_deadlock = TimestampTzPlusMilliseconds(GetCurrentTimestamp(),
												Max(kingres_shard_deadlock_check_interval, 1) * 1000L);

	for (;;)
	{
		TimestampTz now = GetCurrentTimestamp();
		TimestampTz due = TimestampTzPlusMilliseconds(now, 3600 * 1000L);
		long		wait_ms;

		/* A setting of 0 disables the pass: sleep until a reload. */
		if (kingres_shard_recovery_interval > 0 && next_recovery < due)
			due = next_recovery;
		if (kingres_shard_deadlock_check_interval > 0 && next_deadlock < due)
			due = next_deadlock;
		wait_ms = Max(0, (long) ((due - now) / 1000));

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
		now = GetCurrentTimestamp();
		if (kingres_shard_recovery_interval > 0 && now >= next_recovery)
		{
			launch_workers("recovery");
			now = GetCurrentTimestamp();
			next_recovery = TimestampTzPlusMilliseconds(now, kingres_shard_recovery_interval * 1000L);
		}
		if (kingres_shard_deadlock_check_interval > 0 && now >= next_deadlock)
		{
			launch_workers("deadlock");
			now = GetCurrentTimestamp();
			next_deadlock = TimestampTzPlusMilliseconds(now, kingres_shard_deadlock_check_interval * 1000L);
		}
		/* After a reload that switched a pass on, it is due one interval from now. */
		if (kingres_shard_recovery_interval > 0 && next_recovery > TimestampTzPlusMilliseconds(now, kingres_shard_recovery_interval * 1000L))
			next_recovery = TimestampTzPlusMilliseconds(now, kingres_shard_recovery_interval * 1000L);
		if (kingres_shard_deadlock_check_interval > 0 && next_deadlock > TimestampTzPlusMilliseconds(now, kingres_shard_deadlock_check_interval * 1000L))
			next_deadlock = TimestampTzPlusMilliseconds(now, kingres_shard_deadlock_check_interval * 1000L);
	}
}

/*
 * Settings of the database or of the role of the worker (ALTER DATABASE ...
 * SET, ALTER ROLE ... SET) must not be able to make a pass fail, stall or
 * behave differently (review finding S2-11): every setting that can is fixed
 * here, at a level (PGC_S_OVERRIDE) that nothing stored can beat.
 */
static void
fix_worker_settings(const char *extra)
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
	int			node_timeout;
	int			node_connect_timeout;

	for (i = 0; settings[i][0] != NULL; i++)
		(void) set_config_option(settings[i][0], settings[i][1],
								 PGC_SUSET, PGC_S_OVERRIDE, GUC_ACTION_SET, true, 0, false);

	/*
	 * node_timeout and node_connect_timeout can be stored with ALTER DATABASE
	 * or ALTER ROLE (S3-16): a worker that took such a value would bound its
	 * statements, its connections and the launcher's deadline by it.  The
	 * launcher passes the values of the server configuration.
	 */
	if (sscanf(extra, "%*s %d %d", &node_timeout, &node_connect_timeout) == 2)
	{
		char		buf[16];

		snprintf(buf, sizeof(buf), "%d", node_timeout);
		(void) set_config_option("kingres_shard.node_timeout", buf,
								 PGC_SUSET, PGC_S_OVERRIDE, GUC_ACTION_SET, true, 0, false);
		snprintf(buf, sizeof(buf), "%d", node_connect_timeout);
		(void) set_config_option("kingres_shard.node_connect_timeout", buf,
								 PGC_SUSET, PGC_S_OVERRIDE, GUC_ACTION_SET, true, 0, false);
	}
}

/*
 * run_move_step
 *
 * Run one statement of the bucket-move upkeep (resume the interrupted moves,
 * drop the orphan tables of the finished ones) in a transaction of its own.
 * Both functions do their work in sessions that they open themselves and that
 * wait for the rows this transaction would hold, and each is independent of
 * the other, so a failure is logged and ends only its own step.
 */
static void
run_move_step(const char *sql)
{
	MemoryContext oldcxt = CurrentMemoryContext;

	StartTransactionCommand();
	PushActiveSnapshot(GetTransactionSnapshot());
	PG_TRY();
	{
		if (SPI_connect() != SPI_OK_CONNECT)
			elog(ERROR, "SPI_connect failed");
		if (SPI_execute(sql, false, 0) < 0)
			elog(ERROR, "could not run \"%s\"", sql);
		SPI_finish();
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

void
kingres_shard_recovery_worker_main(Datum main_arg)
{
	Oid			dboid = DatumGetObjectId(main_arg);
	bool		run_moves = false;
	bool		run_deadlock = false;
	char		mode[16] = "recovery";

	pqsignal(SIGTERM, die);
	BackgroundWorkerUnblockSignals();
	BackgroundWorkerInitializeConnectionByOid(dboid, InvalidOid, 0);

	/* Before the transaction starts: the isolation level and the access mode are read at its start. */
	(void) sscanf(MyBgworkerEntry->bgw_extra, "%15s", mode);
	fix_worker_settings(MyBgworkerEntry->bgw_extra);
	StartTransactionCommand();
	PushActiveSnapshot(GetTransactionSnapshot());
	if (!RecoveryInProgress() &&
		OidIsValid(get_extension_oid("kingres_shard", true)))
	{
		const KsCatalog *cat = kingres_shard_catalog();

		if (strcmp(mode, "deadlock") == 0)
		{
			/*
			 * The detector runs on the map group primary only, and only where the
			 * SQL objects of 0.5 exist (a library newer than its SQL objects has
			 * no such function).
			 */
			run_deadlock = cat->present && cat->local_group_id == KINGRES_SHARD_MAP_GROUP &&
				OidIsValid(kingres_shard_catalog_relid("dtx_incident"));
		}
		else if (cat->present && cat->local_node_id != 0 &&
				 OidIsValid(kingres_shard_catalog_relid("dtx_log")))
			(void) kingres_shard_recover_prepared();

		/*
		 * Moves are recorded on the map group primary only, and only where the
		 * SQL objects of 0.3 exist (a node whose library is newer than its SQL
		 * objects has no such table: review finding S4-05).
		 */
		run_moves = strcmp(mode, "recovery") == 0 &&
			cat->present && cat->local_group_id == KINGRES_SHARD_MAP_GROUP &&
			OidIsValid(kingres_shard_catalog_relid("bucket_move"));
	}
	PopActiveSnapshot();
	CommitTransactionCommand();

	if (run_deadlock)
		run_move_step("SELECT " KINGRES_SHARD_SCHEMA ".detect_distributed_deadlocks()");

	/*
	 * Moves (review finding S4-02).  A move whose session died keeps its slot,
	 * its "moving" bucket and the DDL block until somebody undoes it; a move
	 * that finished keeps a full second copy of the bucket until its orphan
	 * tables are dropped.  Both functions leave a move alone whose session
	 * still runs it (its live mark is held) and bound every call to another
	 * node; the pass as a whole is bounded by the launcher's deadline.
	 */
	if (run_moves)
	{
		run_move_step("SELECT " KINGRES_SHARD_SCHEMA ".resume_bucket_moves()");
		run_move_step("SELECT " KINGRES_SHARD_SCHEMA ".cleanup_moved_buckets()");
	}
	proc_exit(0);
}
