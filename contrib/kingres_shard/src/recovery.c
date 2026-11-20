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
#include "replication/syncrep.h"
#include "storage/ipc.h"
#include "storage/latch.h"
#include "storage/procarray.h"
#include "tcop/tcopprot.h"
#include "utils/acl.h"
#include "utils/array.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/memutils.h"
#include "utils/snapmgr.h"
#include "utils/timestamp.h"
#include "utils/wait_classes.h"

#include "catalog.h"
#include "forward_auth.h"
#include "ha.h"
#include "kingres_shard.h"
#include "node_admin.h"
#include "node_conn.h"
#include "recovery.h"
#include "term.h"

PG_FUNCTION_INFO_V1(kingres_recover_prepared_transactions);

/* A node to look at. */
typedef struct RecNode
{
	int			node_id;
	int			group_id;
	char	   *host;
	int			port;
	bool		standby;		/* a standby of the catalog: looked at only when it is out of recovery */
	int			claim_id;		/* the id a node that remove_node() took out of the catalog still answers to, or 0 */
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
 * A gid of ours, split.  Three forms exist:
 *
 *	  kingres_h<origin group>_<timeline>_<pid>_<full xid>_<target>[_<n>]_<tag>
 *	  kingres_g<origin group>_<timeline>_<pid>_<full xid>_<target>[_<n>]
 *	  kingres_<origin node>_<pid>_<full xid>_<target>[_<n>]	(0.4 and older)
 *
 * The first is the one that 0.5 makes: it names the group of the origin, so
 * that the node that became the primary of that group after a failover
 * resolves what the old one started, and the timeline the origin prepared on,
 * and it ends with a tag, 16 hex digits of an HMAC of everything before it with
 * the cluster secret (forward_auth.c).  Any role can PREPARE TRANSACTION under
 * any name, so a name of this form is the extension's own only when its tag
 * verifies (S7-09); the identifier is at most 100 bytes (a three-digit group,
 * a ten-digit timeline, pid, node and a twenty-digit transaction number),
 * which fits PostgreSQL's limit of 200 for it.  The other two are the names
 * that 0.4 and the first 0.5 made, without tag; a transaction that carries one
 * and was prepared before this node came to the version that tags them is
 * still resolved, by the group of its origin (looked up from the node row in
 * the 0.4 form).
 */
#define KS_GID_TAG_LEN 16

typedef enum GidKind
{
	GID_TAGGED,					/* the tag verifies */
	GID_FORGED,					/* the form of a tagged name, and the tag does not verify */
	GID_UNTAGGED				/* an older form, without tag */
} GidKind;

typedef struct ParsedGid
{
	GidKind		kind;
	bool		is_group_format;
	int			origin;			/* a group id, or (old format) a node id */
	uint32		tli;			/* 0 in the old format */
	int			pid;
	uint64		fxid;
} ParsedGid;

/* Is every character of s (starting at from) a decimal digit, and is there at least one? */
static bool
all_digits(const char *s)
{
	if (*s == '\0')
		return false;
	for (; *s != '\0'; s++)
		if (*s < '0' || *s > '9')
			return false;
	return true;
}

/* false when the gid is not one of ours, in any form. */
static bool
parse_gid(const char *gid, ParsedGid *out)
{
	char	   *copy = pstrdup(gid);
	char	   *tok[12];
	int			n = 0;
	char	   *p = copy;
	bool		ok;
	char		form;			/* 'h', 'g' or 'n' */
	int			t;

	while (n < 12)
	{
		tok[n++] = p;
		p = strchr(p, '_');
		if (p == NULL)
			break;
		*p++ = '\0';
	}
	if (p != NULL)
	{
		pfree(copy);
		return false;			/* more tokens than any form has */
	}
	ok = (n >= 5 && strcmp(tok[0], "kingres") == 0);
	form = 'n';
	if (ok && (tok[1][0] == 'g' || tok[1][0] == 'h'))
		form = tok[1][0];
	if (ok)
	{
		/* tagged: 7 or 8 numbers and the tag; group, untagged: 6 or 7; node: 5 or 6 */
		if (form == 'h')
			ok = (n == 7 || n == 8);
		else if (form == 'g')
			ok = (n == 6 || n == 7);
		else
			ok = (n == 5 || n == 6);
		for (t = 1; ok && t < n - (form == 'h' ? 1 : 0); t++)
			ok = all_digits(form != 'n' && t == 1 ? tok[t] + 1 : tok[t]);
		if (ok && form == 'h')
		{
			const char *tag = tok[n - 1];

			ok = (strlen(tag) == KS_GID_TAG_LEN);
			for (; ok && *tag != '\0'; tag++)
				ok = ((*tag >= '0' && *tag <= '9') || (*tag >= 'a' && *tag <= 'f'));
		}
	}
	if (ok)
	{
		out->is_group_format = (form != 'n');
		out->kind = GID_UNTAGGED;
		if (form == 'n')
		{
			out->origin = atoi(tok[1]);
			out->tli = 0;
			out->pid = atoi(tok[2]);
			out->fxid = strtoull(tok[3], NULL, 10);
		}
		else
		{
			out->origin = atoi(tok[1] + 1);
			out->tli = (uint32) strtoul(tok[2], NULL, 10);
			out->pid = atoi(tok[3]);
			out->fxid = strtoull(tok[4], NULL, 10);
		}
		if (form == 'h')
		{
			char		want[KS_GID_TAG_LEN + 1];
			size_t		blen = strlen(gid) - (KS_GID_TAG_LEN + 1);
			char	   *body = pnstrdup(gid, blen);

			out->kind = ((kingres_shard_gid_tag(body, want) &&
						  strcmp(want, tok[n - 1]) == 0) ||
						 (kingres_shard_gid_tag_previous(body, want) &&
						  strcmp(want, tok[n - 1]) == 0)) ? GID_TAGGED : GID_FORGED;
			pfree(body);
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
	Oid			argtypes[6] = {INT4ARRAYOID, TIMESTAMPTZOID, TEXTARRAYOID, INT4OID, INT4OID, BOOLOID};
	Datum		values[6];
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
	/*
	 * The rows of the node that is the primary of its group are kept for
	 * decision_retention seconds after the transaction is over when the group has a
	 * standby: if the group fails over in that time, the origin looks at them
	 * (check_participant_parts, S7-07).  The rows of the nodes that are not the
	 * primary any more were looked at before this and go at once.
	 */
	values[3] = Int32GetDatum(node_id);
	values[4] = Int32GetDatum(nids > 1 ? kingres_shard_decision_retention : 0);
	values[5] = BoolGetDatum(nids > 1);	/* the group has a standby: only durable rows are forgotten */
	/*
	 * THE INVARIANT OF THE DECISION LOG (S9-01): a decision is never forgotten
	 * while the participant that it names might still come back with the
	 * transaction prepared.  That is possible as long as the participant's
	 * synchronous standbys have not confirmed what the participant flushed,
	 * the commit of its part included: a failover to a standby that has the
	 * PREPARE and not the COMMIT PREPARED brings the part back prepared, and
	 * without the row recovery would roll back a committed transaction.  So the
	 * rows of the current primary are subject to the retention only once they are
	 * marked durable (asked of the participant after the prepared transactions
	 * were listed, see participant_confirmed); the rows of an unconfirmed
	 * participant are kept without limit and reported by check_cluster().  The rows of
	 * the nodes that are not the primary any more were looked at before this.
	 */
	if (SPI_execute_with_args("DELETE FROM " KINGRES_SHARD_SCHEMA ".dtx_log "
							  "WHERE node_id OPERATOR(pg_catalog.=) ANY ($1) "
							  "AND prepared_at OPERATOR(pg_catalog.<) $2 "
							  "AND NOT (gid OPERATOR(pg_catalog.=) ANY ($3)) "
							  "AND (node_id OPERATOR(pg_catalog.<>) $4 "
							  "OR ((durable OR NOT $6) AND prepared_at OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) pg_catalog.make_interval(secs => $5))))",
							  6, argtypes, values, NULL, false, 0) != SPI_OK_DELETE)
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
record_incident(const char *gid, int node_id, uint32 gid_tli, TimeLineID current_tli,
				const char *kind)
{
	Oid			argtypes[5] = {TEXTOID, INT4OID, INT4OID, INT4OID, TEXTOID};
	Datum		values[5];

	if (!OidIsValid(kingres_shard_catalog_relid("dtx_incident")))
		return;
	values[0] = CStringGetTextDatum(gid);
	values[1] = Int32GetDatum(node_id);
	values[2] = Int32GetDatum((int32) gid_tli);
	values[3] = Int32GetDatum((int32) current_tli);
	values[4] = CStringGetTextDatum(kind);
	if (SPI_execute_with_args("INSERT INTO " KINGRES_SHARD_SCHEMA ".dtx_incident "
							  "(gid, node_id, gid_timeline, current_timeline, detected_at, kind) "
							  "VALUES ($1, $2, $3, $4, pg_catalog.clock_timestamp(), $5) "
							  "ON CONFLICT (gid) DO NOTHING",
							  5, argtypes, values, NULL, false, 0) != SPI_OK_INSERT)
		elog(ERROR, "could not record in kingres.dtx_incident");
	if (strcmp(kind, "participant_lost") == 0)
	{
		ereport(WARNING,
				(errmsg("kingres_shard recovery: the transaction \"%s\" was committed here, but node %d, the primary of the group that took part in it, has no trace of it",
						gid, node_id),
				 errdetail("The participant failed over to a standby that did not have the commit of its part (asynchronous replication)."),
				 errhint("Look at the row of this gid in kingres.dtx_incident; the changes of the transaction on that group are lost.")));
		return;
	}
	if (strcmp(kind, "rolled_back_after_retention") == 0)
	{
		ereport(WARNING,
				(errmsg("kingres_shard recovery: prepared transaction \"%s\" on node %d is older than decision_retention and has no commit decision on this node; it was rolled back",
						gid, node_id),
				 errdetail("Either its origin never decided (it was lost long ago and this participant was out of reach until now), or it committed and its decision was forgotten after the retention, which happens only for a participant whose standbys cannot confirm a commit (none required)."),
				 errhint("Look at the row of this gid in kingres.dtx_incident.")));
		return;
	}
	ereport(WARNING,
			(errmsg("kingres_shard recovery: prepared transaction \"%s\" on node %d belongs to timeline %u and has no commit decision on this node (timeline %u); it was rolled back",
					gid, node_id, gid_tli, current_tli),
			 errdetail("If the group lost its primary and the commit of this transaction had not reached this node, other nodes may have committed it."),
			 errhint("Look at the row of this gid in kingres.dtx_incident.")));
}

/*
 * A transaction that this node decided to commit and sent COMMIT PREPARED for may
 * still be lost on a participant: the participant failed over to a standby that
 * was behind (asynchronous replication), and the standby has neither the prepared
 * transaction nor its commit (S7-07).  Every participant writes a row to its
 * kingres.dtx_applied in the transaction that it prepares, so the row is there
 * exactly when the participant's part is committed, and replicates with it.  When
 * the decision rows of a participant that is no longer the primary of its group
 * are about to be dropped (the transaction is not prepared on the new primary),
 * the rows that the new primary has no mark for are recorded in
 * kingres.dtx_incident as "participant_lost".  This costs nothing until a group has
 * failed over: only rows of a node that is not the group's primary any more are
 * looked at.  Returns false when the new primary could not be asked: the rows are
 * kept and the look is repeated at the next pass.
 */
static bool
check_participant_parts(KsConn *kc, int node_id, const int *node_ids, int nids,
						TimestampTz listed_at, List *still_prepared, TimeLineID current_tli)
{
	Oid			argtypes[4] = {INT4ARRAYOID, TIMESTAMPTZOID, TEXTARRAYOID, INT4OID};
	Datum		values[4];
	Datum	   *elems;
	Datum	   *ids;
	int			n = list_length(still_prepared);
	int			i = 0;
	int			k;
	ListCell   *lc;
	List	   *cands = NIL;
	StringInfoData arr;
	const char *params[1];
	PGresult   *res;
	bool		first = true;

	if (!OidIsValid(kingres_shard_catalog_relid("dtx_applied")) || node_id == 0)
		return true;
	elems = palloc(sizeof(Datum) * Max(n, 1));
	foreach(lc, still_prepared)
		elems[i++] = CStringGetTextDatum((char *) lfirst(lc));
	ids = palloc(sizeof(Datum) * Max(nids, 1));
	for (k = 0; k < nids; k++)
		ids[k] = Int32GetDatum(node_ids[k]);
	values[0] = PointerGetDatum(construct_array_builtin(ids, nids, INT4OID));
	values[1] = TimestampTzGetDatum(listed_at);
	values[2] = PointerGetDatum(construct_array_builtin(elems, n, TEXTOID));
	values[3] = Int32GetDatum(node_id);
	if (SPI_execute_with_args("SELECT gid FROM " KINGRES_SHARD_SCHEMA ".dtx_log "
							  "WHERE node_id OPERATOR(pg_catalog.=) ANY ($1) "
							  "AND NOT exempt "
							  "AND node_id OPERATOR(pg_catalog.<>) $4 "
							  "AND prepared_at OPERATOR(pg_catalog.<) $2 "
							  "AND NOT (gid OPERATOR(pg_catalog.=) ANY ($3))",
							  4, argtypes, values, NULL, true, 0) != SPI_OK_SELECT)
		elog(ERROR, "could not read kingres.dtx_log");
	for (i = 0; i < (int) SPI_processed; i++)
		cands = lappend(cands, MemoryContextStrdup(CurrentMemoryContext,
												   SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1)));
	if (cands == NIL)
		return true;

	initStringInfo(&arr);
	appendStringInfoChar(&arr, '{');
	foreach(lc, cands)
	{
		const char *g = (const char *) lfirst(lc);

		appendStringInfo(&arr, "%s\"", first ? "" : ",");
		for (; *g != '\0'; g++)
		{
			if (*g == '"' || *g == '\\')
				appendStringInfoChar(&arr, '\\');
			appendStringInfoChar(&arr, *g);
		}
		appendStringInfoChar(&arr, '"');
		first = false;
	}
	appendStringInfoChar(&arr, '}');
	params[0] = arr.data;
	res = kingres_shard_exec_params(kc,
									"SELECT gid FROM " KINGRES_SHARD_SCHEMA ".dtx_applied "
									"WHERE gid OPERATOR(pg_catalog.=) ANY ($1::pg_catalog.text[])",
									1, NULL, params, false);
	if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK)
	{
		/* A node whose SQL objects are older than this library has no marks: nothing to compare. */
		bool		undefined = (res != NULL && kingres_shard_result_sqlstate(res) == ERRCODE_UNDEFINED_TABLE);

		PQclear(res);
		if (kc->conn == NULL || PQstatus(kc->conn) != CONNECTION_OK)
			kc->have_error = true;
		return undefined;
	}
	foreach(lc, cands)
	{
		const char *g = (const char *) lfirst(lc);
		bool		present = false;

		for (i = 0; i < PQntuples(res); i++)
			if (strcmp(PQgetvalue(res, i, 0), g) == 0)
			{
				present = true;
				break;
			}
		if (!present)
			record_incident(g, node_id, current_tli, current_tli, "participant_lost");
	}
	PQclear(res);
	return true;
}

/*
 * Are the decisions of this participant safe to forget (S9-01)?  They are when
 * the participant's synchronous standbys have confirmed everything it has
 * flushed, asked now, after the prepared transactions of this pass were listed:
 * every commit whose gid was absent from that list is below the position the
 * participant flushes at this moment.  A participant that has no decision to
 * forget, or whose group has no registered standby, needs no question.  The
 * answer is also written to kingres.dtx_unconfirmed, which check_cluster()
 * reads, so that a decision that is kept for lack of a confirmation is visible.
 */
static bool
participant_confirmed(KsConn *kc, int node_id, int nids, TimestampTz listed_at, List *still_prepared,
						 bool demoted)
{
	Oid			argtypes[3] = {INT4OID, TIMESTAMPTZOID, TEXTARRAYOID};
	Datum		values[3];
	Datum	   *elems;
	int			n = list_length(still_prepared);
	int			i = 0;
	ListCell   *lc;
	int64		kept;
	bool		confirmed = true;
	bool		have_table = OidIsValid(kingres_shard_catalog_relid("dtx_unconfirmed"));

	if (node_id == 0 || nids <= 1)
		return true;
	elems = palloc(sizeof(Datum) * Max(n, 1));
	foreach(lc, still_prepared)
		elems[i++] = CStringGetTextDatum((char *) lfirst(lc));
	values[0] = Int32GetDatum(node_id);
	values[1] = TimestampTzGetDatum(listed_at);
	values[2] = PointerGetDatum(construct_array_builtin(elems, n, TEXTOID));
	if (SPI_execute_with_args("SELECT count(*) FROM " KINGRES_SHARD_SCHEMA ".dtx_log "
							  "WHERE node_id OPERATOR(pg_catalog.=) $1 AND NOT durable "
							  "AND prepared_at OPERATOR(pg_catalog.<) $2 "
							  "AND NOT (gid OPERATOR(pg_catalog.=) ANY ($3))",
							  3, argtypes, values, NULL, true, 1) != SPI_OK_SELECT || SPI_processed != 1)
		elog(ERROR, "could not read kingres.dtx_log");
	{
		bool		isnull;

		kept = DatumGetInt64(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull));
	}
	/* A node the catalog does not call the primary (the old primary of a promotion) has no standby to ask. */
	if (kept > 0 && !demoted)
	{
		char	   *qsql = psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_sync_confirm(%d)",
									Min(kingres_shard_node_timeout, 2) * 1000);
		PGresult   *qres = kingres_shard_exec_params(kc, qsql, 0, NULL, NULL, false);

		confirmed = (qres != NULL && PQresultStatus(qres) == PGRES_TUPLES_OK &&
					 PQntuples(qres) == 1 && strcmp(PQgetvalue(qres, 0, 0), "t") == 0);
		if (qres == NULL || PQresultStatus(qres) != PGRES_TUPLES_OK)
			if (kc->conn == NULL || PQstatus(kc->conn) != CONNECTION_OK)
				kc->have_error = true;
		PQclear(qres);
		pfree(qsql);
	}
	if (confirmed && kept > 0 &&
		SPI_execute_with_args("UPDATE " KINGRES_SHARD_SCHEMA ".dtx_log SET durable = true "
								  "WHERE node_id OPERATOR(pg_catalog.=) $1 AND NOT durable "
								  "AND prepared_at OPERATOR(pg_catalog.<) $2 "
								  "AND NOT (gid OPERATOR(pg_catalog.=) ANY ($3))",
								  3, argtypes, values, NULL, false, 0) != SPI_OK_UPDATE)
		elog(ERROR, "could not mark kingres.dtx_log");
	if (have_table)
	{
		Oid			a2[2] = {INT4OID, INT8OID};
		Datum		v2[2];

		v2[0] = Int32GetDatum(node_id);
		v2[1] = Int64GetDatum(kept);
		if (confirmed)
		{
			if (SPI_execute_with_args("DELETE FROM " KINGRES_SHARD_SCHEMA ".dtx_unconfirmed "
									  "WHERE node_id OPERATOR(pg_catalog.=) $1",
									  1, a2, v2, NULL, false, 0) != SPI_OK_DELETE)
				elog(ERROR, "could not clean kingres.dtx_unconfirmed");
		}
		else if (SPI_execute_with_args("INSERT INTO " KINGRES_SHARD_SCHEMA ".dtx_unconfirmed (node_id, since, kept) "
									   "VALUES ($1, pg_catalog.clock_timestamp(), $2) "
									   "ON CONFLICT (node_id) DO UPDATE SET kept = EXCLUDED.kept",
									   2, a2, v2, NULL, false, 0) != SPI_OK_INSERT)
			elog(ERROR, "could not record in kingres.dtx_unconfirmed");
	}
	if (!confirmed)
		ereport(LOG,
				(errmsg("kingres_shard recovery: the standbys of node %d have not confirmed what it flushed; its %lld decision(s) are kept until they do",
						node_id, (long long) kept)));
	return confirmed;
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
static int
recover_prepared_with_body(int extra_id, int extra_group, const char *extra_host, int extra_port)
{
	MemoryContext callercxt = CurrentMemoryContext;
	const KsCatalog *cat = kingres_shard_catalog();
	List	   *nodes = NIL;
	ListCell   *lc;
	int			my_group;
	TimeLineID	current_tli;
	int			resolved = 0;
	bool		sync_wait_failed = false;
	List	   *all_prepared = NIL;	/* every prepared gid seen on any node in this pass */
	int			expected_primaries = 0;
	int			purged_primaries = 0;
	bool		any_via_previous = false;	/* a node needed the previous cluster secret (S9-03) */
	int			unconfirmed_standbys = 0;	/* a standby that could not be asked: it may hold an older secret (S11-04) */
	TimestampTz pass_start = GetCurrentTimestamp();
	const char *old_format_until = "-infinity";

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

		/*
		 * A standby is looked at too, once it is out of recovery: the standby that
		 * promote_node() is promoting holds the prepared catalog change of a
		 * promotion that stopped (S7-05), and it is not a primary of the catalog
		 * until the change commits.  A standby that is still in recovery, or that
		 * cannot be reached, is skipped without a word.
		 */
		if (!n->is_active || (!n->is_primary && n->node_id == cat->local_node_id))
			continue;
		r = MemoryContextAlloc(callercxt, sizeof(RecNode));
		r->standby = !n->is_primary;
		r->claim_id = 0;
		r->node_id = n->node_id;
		r->group_id = n->group_id;
		r->host = MemoryContextStrdup(callercxt, n->host);
		r->port = n->port;
		nodes = lappend(nodes, r);
		if (!r->standby)
			expected_primaries++;
	}
	if (extra_host != NULL)
	{
		RecNode    *r = MemoryContextAlloc(callercxt, sizeof(RecNode));

		r->standby = false;
		r->claim_id = 0;
		r->node_id = extra_id;
		r->group_id = extra_group;
		r->host = MemoryContextStrdup(callercxt, extra_host);
		r->port = extra_port;
		nodes = lappend(nodes, r);
		expected_primaries++;
	}

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	/*
	 * The names of the form that 0.4 used carry no tag.  They are ours when they
	 * were prepared before this node came to the version that tags them (S7-09),
	 * and not after: the time is in kingres.dtx_epoch (-infinity when the table is
	 * not there, which is an SQL script older than the library).
	 */
	if (OidIsValid(kingres_shard_catalog_relid("dtx_epoch")) &&
		SPI_execute("SELECT old_format_until::pg_catalog.text FROM " KINGRES_SHARD_SCHEMA ".dtx_epoch",
					true, 1) == SPI_OK_SELECT && SPI_processed == 1)
		old_format_until = MemoryContextStrdup(callercxt,
											   SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1));

	/*
	 * Addresses on which add_node() prepared a transaction before it registered
	 * them: if the origin died, the registration was rolled back with the rest
	 * and only this table remembers the node.
	 */
	kingres_shard_require_pending_node_id();
	if (SPI_execute("SELECT p.host, p.port, p.node_id FROM " KINGRES_SHARD_SCHEMA ".pending_node p "
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

			r->standby = false;
			r->node_id = 0;
			r->group_id = -1;
			r->host = MemoryContextStrdup(callercxt,
										  SPI_getvalue(SPI_tuptable->vals[i],
													   SPI_tuptable->tupdesc, 1));
			r->port = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[i],
												  SPI_tuptable->tupdesc, 2, &isnull));
			/*
			 * A node that remove_node() takes out of the catalog still holds the catalog and the
			 * secret it had until the removal commits, and accepts only the session that claims
			 * its old id (S11-05).  Its id is not used for anything else: the decision log is
			 * found by the gid.
			 */
			{
				int			old_id = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[i],
																 SPI_tuptable->tupdesc, 3, &isnull));

				r->claim_id = (!isnull && old_id > 0) ? old_id : 0;
			}
			nodes = lappend(nodes, r);
		}
	}

	foreach(lc, nodes)
	{
		RecNode    *r = (RecNode *) lfirst(lc);
		char	   *err = NULL;
		KsConn	   *kc;
		PGresult   *res;
		const char *params[3];
		TimestampTz listed_at;
		List	   *gids = NIL;
		List	   *old_flags = NIL;
		List	   *still_prepared = NIL;
		ListCell   *lc2;
		ListCell   *lc3;
		int			i;
		int			ntup;

		if (r->claim_id > 0)
			kingres_shard_conn_expect_node(r->host, r->port, r->claim_id);
		kc = kingres_shard_get_conn(r->host, r->port, true, &err);
		if (kc == NULL)
		{
			if (r->standby)
			{
				unconfirmed_standbys++;
				continue;
			}
			ereport((MyBackendType == B_BG_WORKER || r->node_id == 0) ? LOG : WARNING,
					(errmsg("kingres_shard recovery: node %d (%s:%d) is not reachable, skipped",
							r->node_id, r->host, r->port),
					 errdetail_internal("%s", err ? err : "")));
			continue;
		}
		if (kc->xact_depth > 0)
			ereport(ERROR,
					(errcode(ERRCODE_ACTIVE_SQL_TRANSACTION),
					 errmsg("recover_prepared_transactions() cannot run in a transaction that has operated on remote nodes")));
		if (kc->via_previous)
			any_via_previous = true;
		if (r->standby)
		{
			PGresult   *rres = kingres_shard_exec_params(kc, "SELECT pg_catalog.pg_is_in_recovery()::pg_catalog.text",
														0, NULL, NULL, false);
			bool		in_recovery = (rres == NULL || PQresultStatus(rres) != PGRES_TUPLES_OK ||
									strcmp(PQgetvalue(rres, 0, 0), "false") != 0);

			PQclear(rres);
			if (in_recovery)
				continue;
		}

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
							 errdetail_internal("%s", (sres != NULL && PQresultErrorMessage(sres)[0] != '\0') ?
											PQresultErrorMessage(sres) : kingres_shard_conn_error_text(kc->conn))));
				PQclear(sres);
				pfree(set);
			}
			if (!bounded)
				continue;
		}
		/* The COMMIT PREPARED and ROLLBACK PREPARED below are acknowledged by this node's standby (S7-07). */
		/*
		 * A node that the catalog calls a standby and that is out of recovery is the one being
		 * promoted or the old primary that was demoted: its standbys are not waited for (S9-05;
		 * the old primary names the node that replaced it).
		 */
		if (!(r->standby ? kingres_shard_conn_local_sync(kc) : kingres_shard_conn_force_sync(kc)))
		{
			ereport(MyBackendType == B_BG_WORKER ? LOG : WARNING,
					(errmsg("kingres_shard recovery: could not set up the session on node %d (%s:%d), skipped",
							r->node_id, r->host, r->port),
					 errdetail_internal("%s", kingres_shard_conn_error_text(kc->conn))));
			continue;
		}
		listed_at = GetCurrentTimestamp();
		/* Both formats; parse_gid() and the group of the origin say which are ours. */
		params[0] = "kingres\\_%";
		params[1] = old_format_until;
		params[2] = psprintf("%d", kingres_shard_decision_retention);
		res = kingres_shard_exec_params(kc,
										"SELECT gid, prepared OPERATOR(pg_catalog.<) $2::pg_catalog.timestamptz, "
										"prepared OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) pg_catalog.make_interval(secs => $3::pg_catalog.int4)) "
										"FROM pg_catalog.pg_prepared_xacts "
										"WHERE database OPERATOR(pg_catalog.=) pg_catalog.current_database() "
										"AND gid LIKE $1 ORDER BY prepared",
										3, NULL, params, false);
		if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK)
		{
			ereport(MyBackendType == B_BG_WORKER ? LOG : WARNING,
					(errmsg("kingres_shard recovery: could not list the prepared transactions of node %d (%s:%d)",
							r->node_id, r->host, r->port),
					 errdetail_internal("%s", kingres_shard_conn_error_text(kc->conn))));
			PQclear(res);
			if (kc->conn == NULL || PQstatus(kc->conn) != CONNECTION_OK)
				kc->have_error = true;
			continue;
		}
		ntup = PQntuples(res);
		for (i = 0; i < ntup; i++)
		{
			gids = lappend(gids, pstrdup(PQgetvalue(res, i, 0)));
			old_flags = lappend_int(old_flags, (strcmp(PQgetvalue(res, i, 1), "t") == 0 ? 1 : 0) |
									(strcmp(PQgetvalue(res, i, 2), "t") == 0 ? 2 : 0));
		}
		PQclear(res);
		all_prepared = list_concat(all_prepared, list_copy(gids));

		forboth(lc2, gids, lc3, old_flags)
		{
			char	   *gid = (char *) lfirst(lc2);
			bool		before_cutoff = (lfirst_int(lc3) & 1) != 0;
			bool		past_retention = (lfirst_int(lc3) & 2) != 0;
			ParsedGid	pg;
			int			origin_group;
			bool		earlier;
			bool		commit;
			char	   *sql;

			if (!parse_gid(gid, &pg))
				continue;

			/*
			 * A name of the right form that does not carry a valid tag was not made
			 * by a node of this cluster, and a name of an older form (no tag) is ours
			 * only if it was prepared before this node came to the version that tags
			 * them: neither is touched (S7-09; check_cluster() reports the first).
			 */
			if (pg.kind == GID_FORGED || (pg.kind == GID_UNTAGGED && !before_cutoff))
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
			/*
			 * A decision is acted on only once it is known to be where the
			 * configuration of the group says it must be (S7-01): the commit of
			 * the row has been flushed, and the synchronous standbys must have
			 * confirmed everything flushed so far, or a failover could promote a
			 * node that does not have the decision after the participants
			 * committed.  Rolling back needs nothing of the kind.
			 */
			if (commit)
			{
				bool		durable;

				if (sync_wait_failed)
					durable = kingres_shard_sync_confirmed(GetFlushRecPtr(NULL),
														   SYNC_REP_WAIT_FLUSH);
				else
				{
					durable = kingres_shard_wait_decision_durable(Min(kingres_shard_node_timeout, 5) * 1000);
					sync_wait_failed = !durable;
				}
				if (!durable)
				{
					ereport(LOG,
							(errmsg("kingres_shard recovery: the decision for prepared transaction \"%s\" on node %d is not yet confirmed by the synchronous standby; left prepared",
									gid, r->node_id)));
					still_prepared = lappend(still_prepared, gid);
					continue;
				}
			}
			kingres_shard_fault_point("recovery_before_resolve");
			sql = psprintf("%s PREPARED %s", commit ? "COMMIT" : "ROLLBACK",
						   quote_literal_cstr(gid));
			/*
			 * COMMIT PREPARED and ROLLBACK PREPARED wait for the participant's standbys, and a session
			 * of a user has no timeout for that wait (the worker's statement_timeout does not apply to
			 * a commit): bounded by node_timeout, then cancelled, as every other wait on a node (S9).
			 * The cancelled command has completed on the participant; its standby is asked below.
			 */
			res = kingres_shard_exec_cancel_after_timeout(kc->conn, sql);
			if (res == NULL)
				kc->have_error = true;
			if (res != NULL && PQresultStatus(res) == PGRES_COMMAND_OK)
			{
				resolved++;
				ereport(LOG,
						(errmsg("kingres_shard recovery: %s prepared transaction \"%s\" on node %d",
								commit ? "committed" : "rolled back", gid, r->node_id)));
				/*
				 * The same rule as the origin's second phase (S8-01): the answer of
				 * COMMIT PREPARED does not say that the participant's standby has the
				 * record (its wait may have ended early).  Ask; when the standby has
				 * not confirmed it, the decision row is kept for this pass at least, and
				 * the retention keeps it after that (purge_log).
				 */
				if (commit && !r->standby)
				{
					char	   *qsql = psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_sync_confirm(%d)",
												Min(kingres_shard_node_timeout, 5) * 1000);
					PGresult   *qres = kingres_shard_exec_params(kc, qsql, 0, NULL, NULL, false);

					if (qres == NULL || PQresultStatus(qres) != PGRES_TUPLES_OK ||
						PQntuples(qres) != 1 || strcmp(PQgetvalue(qres, 0, 0), "t") != 0)
					{
						ereport(LOG,
								(errmsg("kingres_shard recovery: node %d committed \"%s\", but its synchronous standby has not confirmed the commit; the decision is kept",
										r->node_id, gid)));
						still_prepared = lappend(still_prepared, gid);
						if (qres == NULL || PQresultStatus(qres) != PGRES_TUPLES_OK)
							if (kc->conn == NULL || PQstatus(kc->conn) != CONNECTION_OK)
								kc->have_error = true;
					}
					PQclear(qres);
				}
				if (!commit && earlier)
					record_incident(gid, r->node_id, pg.tli, current_tli, "rolled_back_after_failover");
				else if (!commit && past_retention)
					record_incident(gid, r->node_id, pg.tli, current_tli, "rolled_back_after_retention");
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
						 errdetail_internal("%s", kingres_shard_conn_error_text(kc->conn))));
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
			/* Asked in the session that is still forced to wait for the standbys. */
			(void) participant_confirmed(kc, r->node_id, nids, listed_at, still_prepared, r->standby);
			kingres_shard_conn_unforce_sync(kc);
			/* A participant that failed over and lost its part is found out before the decision is forgotten. */
			/* A node that the catalog does not call the primary is not where lost parts are looked for (S9-06). */
			if (!r->standby &&
				!check_participant_parts(kc, r->node_id, ids, nids, listed_at, still_prepared, current_tli))
				continue;
			purge_log(r->node_id, ids, nids, listed_at, still_prepared, gids);
			if (!r->standby && r->node_id != 0)
				purged_primaries++;
			/* A session that needed the previous secret is not kept: the next one is made with the current one. */
			if (kc->via_previous)
				kc->have_error = true;
		}
	}

	/*
	 * Rows of nodes that left the cluster have nothing left to resolve, but only
	 * when this pass looked at every primary and none of them holds the
	 * transaction prepared: a node that was removed may have been the primary
	 * of a group whose new primary still has the part prepared (S9-01).
	 */
	if (purged_primaries >= expected_primaries)
	{
		Oid			argtypes[2] = {TIMESTAMPTZOID, TEXTARRAYOID};
		Datum		values[2];
		Datum	   *elems;
		int			i = 0;
		ListCell   *lc4;

		elems = palloc(sizeof(Datum) * Max(list_length(all_prepared), 1));
		foreach(lc4, all_prepared)
			elems[i++] = CStringGetTextDatum((char *) lfirst(lc4));
		values[0] = TimestampTzGetDatum(pass_start);
		values[1] = PointerGetDatum(construct_array_builtin(elems, i, TEXTOID));
		if (SPI_execute_with_args("DELETE FROM " KINGRES_SHARD_SCHEMA ".dtx_log d "
								  "WHERE d.node_id OPERATOR(pg_catalog.>) 0 "
								  "AND d.prepared_at OPERATOR(pg_catalog.<) $1 "
								  "AND NOT (d.gid OPERATOR(pg_catalog.=) ANY ($2)) "
								  "AND NOT EXISTS (SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".node n "
								  "WHERE n.node_id OPERATOR(pg_catalog.=) d.node_id)",
								  2, argtypes, values, NULL, false, 0) != SPI_OK_DELETE)
			elog(ERROR, "could not clean kingres.dtx_log");
	}
	/*
	 * The previous cluster secret is kept for the nodes that missed the commit of a
	 * rotation (S9-03).  When this pass reached every primary with the current one
	 * nobody needs it any more: no node holds a rotation prepared that it has not
	 * been told about, and a session that needed it would have said so.
	 */
	{
		unsigned char pbuf[128];
		int			plen = 0;

		if (purged_primaries >= expected_primaries && !any_via_previous && unconfirmed_standbys == 0 &&
			kingres_shard_previous_secret(pbuf, sizeof(pbuf), &plen))
		{
			bool		all_told = true;
			ListCell   *lcw;

			explicit_bzero(pbuf, sizeof(pbuf));

			/*
			 * Every primary accepted a session made with the current secret, so every primary
			 * has it and no rotation is in doubt anywhere: tell them to forget the previous
			 * one too (S10-04).  A node that cannot be told keeps it, and this node keeps its
			 * own until the pass that reaches all of them.
			 */
			foreach(lcw, nodes)
			{
				RecNode    *w = (RecNode *) lfirst(lcw);
				char	   *werr = NULL;
				KsConn	   *wkc;
				PGresult   *wres;

				if (w->standby || w->node_id == cat->local_node_id)
					continue;
				wkc = kingres_shard_get_conn(w->host, w->port, true, &werr);
				if (wkc == NULL || wkc->xact_depth > 0)
				{
					all_told = false;
					continue;
				}
				wres = kingres_shard_exec_params(wkc, "SELECT " KINGRES_SHARD_SCHEMA ".internal_drop_previous_secret()",
												 0, NULL, NULL, false);
				if (wres == NULL || PQresultStatus(wres) != PGRES_TUPLES_OK)
					all_told = false;
				PQclear(wres);
			}
			if (all_told &&
				SPI_execute("UPDATE " KINGRES_SHARD_SCHEMA ".cluster_secret SET previous = NULL, previous_at = NULL "
							"WHERE previous IS NOT NULL", false, 0) != SPI_OK_UPDATE)
				elog(ERROR, "could not clear the previous cluster secret");
		}
	}
	if (OidIsValid(kingres_shard_catalog_relid("dtx_unconfirmed")) &&
		SPI_execute("DELETE FROM " KINGRES_SHARD_SCHEMA ".dtx_unconfirmed u "
					"WHERE NOT EXISTS (SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".dtx_log l "
					"WHERE l.node_id OPERATOR(pg_catalog.=) u.node_id)",
					false, 0) != SPI_OK_DELETE)
		elog(ERROR, "could not clean kingres.dtx_unconfirmed");

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

	/*
	 * The marks of participants (S7-07) are kept as long as the origin keeps the
	 * decision they are compared with: decision_retention (set the same on every node),
	 * and a minute more.  An index on applied_at serves the delete (S8-06).
	 */
	if (OidIsValid(kingres_shard_catalog_relid("dtx_applied")))
	{
		char	   *sql = psprintf("DELETE FROM " KINGRES_SHARD_SCHEMA ".dtx_applied "
								   "WHERE applied_at OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() "
								   "OPERATOR(pg_catalog.-) pg_catalog.make_interval(secs => %d))",
								   kingres_shard_decision_retention + 60);

		if (SPI_execute(sql, false, 0) != SPI_OK_DELETE)
			elog(ERROR, "could not clean kingres.dtx_applied");
	}

	if (OidIsValid(kingres_shard_catalog_relid("dtx_incident")) &&
		SPI_execute("DELETE FROM " KINGRES_SHARD_SCHEMA ".dtx_incident "
					"WHERE detected_at OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) pg_catalog.make_interval(days => 30))",
					false, 0) != SPI_OK_DELETE)
		elog(ERROR, "could not clean kingres.dtx_incident");

	SPI_finish();
	return resolved;
}

/*
 * The recovery pass runs in the scope of a control-plane call (S11-01), whoever calls it: the SQL
 * function, promote_node() and the recovery worker.
 */
int
kingres_shard_recover_prepared_with(int extra_id, int extra_group, const char *extra_host, int extra_port)
{
	KsControlScope scope;
	volatile int result = 0;

	kingres_shard_control_enter(&scope);
	PG_TRY();
	{
		result = recover_prepared_with_body(extra_id, extra_group, extra_host, extra_port);
	}
	PG_FINALLY();
	{
		kingres_shard_control_leave(&scope);
	}
	PG_END_TRY();
	return result;
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
	bool		run_table_ops = false;
	bool		run_splits = false;
	bool		run_deadlock = false;
	char		mode[16] = "recovery";

	pqsignal(SIGTERM, die);
	BackgroundWorkerUnblockSignals();
	BackgroundWorkerInitializeConnectionByOid(dboid, InvalidOid, 0);

	/* Before the transaction starts: the isolation level and the access mode are read at its start. */
	(void) sscanf(MyBgworkerEntry->bgw_extra, "%15s", mode);
	fix_worker_settings(MyBgworkerEntry->bgw_extra);
	if (strcmp(mode, "learn") == 0)
	{
		/* A node learned of a newer catalog term (term.c). */
		kingres_shard_term_worker_run(MyBgworkerEntry->bgw_extra);
		proc_exit(0);
	}
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

		/* The same for the records of table operations (design/shard-phase9.md, "Crashes and lost nodes"). */
		run_table_ops = strcmp(mode, "recovery") == 0 &&
			cat->present && cat->local_group_id == KINGRES_SHARD_MAP_GROUP &&
			OidIsValid(kingres_shard_catalog_relid("table_operation"));

		/* The same for the records of bucket splits (0.8). */
		run_splits = strcmp(mode, "recovery") == 0 &&
			cat->present && cat->local_group_id == KINGRES_SHARD_MAP_GROUP &&
			OidIsValid(kingres_shard_catalog_relid("bucket_split"));
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

	/*
	 * Table operations (design, "Crashes and lost nodes", S4-02): an operation whose session died and that did not
	 * switch is undone, one that switched is finished, an undone one that missed a node is cleaned again; the old
	 * copies whose time has come are dropped.  The functions leave an operation alone whose session holds its live mark.
	 */
	/* Splits (0.8): one whose session died is undone, or finished when its switch committed; the orphans of finished ones are dropped. */
	if (run_splits)
	{
		run_move_step("SELECT " KINGRES_SHARD_SCHEMA ".resume_bucket_splits()");
		run_move_step("SELECT " KINGRES_SHARD_SCHEMA ".cleanup_bucket_splits()");
	}

	if (run_table_ops)
	{
		run_move_step("SELECT " KINGRES_SHARD_SCHEMA ".resume_table_operations()");
		run_move_step("SELECT " KINGRES_SHARD_SCHEMA ".cleanup_table_operations()");
	}
	proc_exit(0);
}

/*
 * kingres.internal_gid_class(gid text) returns text
 *
 * 'tagged', 'forged', 'untagged' or 'none': see parse_gid().  Called by
 * kingres.internal_local_state() for check_cluster().  Superusers only.
 */
PG_FUNCTION_INFO_V1(kingres_internal_gid_class);

Datum
kingres_internal_gid_class(PG_FUNCTION_ARGS)
{
	char	   *gid = text_to_cstring(PG_GETARG_TEXT_PP(0));
	ParsedGid	pg;
	const char *what;

	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for function internal_gid_class")));
	if (!parse_gid(gid, &pg))
		what = "none";
	else if (pg.kind == GID_TAGGED)
		what = "tagged";
	else if (pg.kind == GID_FORGED)
		what = "forged";
	else
		what = "untagged";
	PG_RETURN_TEXT_P(cstring_to_text(what));
}
