/*-------------------------------------------------------------------------
 *
 * fragment_scan.c
 *	  The fragments of a read that the shard groups run (design/shard-phase2b.md,
 *	  4.3, 4.4 and 6.4), in two parts.
 *
 * The machinery.  A set of fragments is one cursor per group, opened by the
 * procedure of the placement check (the probe before, the DECLAREs, the probe
 * after, no FETCH before every node has passed), read in batches of
 * kingres_shard.scan_fetch_size rows with the next batch of a group requested as
 * soon as the current one is used up, drained when the scan ends early and closed
 * when it stops.  It was part of dist_scan.c; the "Kingres Distributed Scan" and
 * the scan below use the same code, so a fix to one is a fix to both.
 *
 * The "Kingres Fragment Scan".  A closed query (design 6.4: every output row
 * depends on the rows of one group only) is run whole by every group on its own
 * buckets and the rows are concatenated: the groups aggregate, join and remove
 * duplicates, and only the final rows cross the network.  The origin's own group
 * runs the same text through SPI as a cursor (design 4.4), planned for this node
 * only by the planner hook (the one-shot mark of fanout.c).  The path is created
 * at UPPERREL_FINAL when planner_hook_body decided that the statement is closed
 * and shippable (KsPushdownInfo); ORDER BY and LIMIT are not sent, the origin
 * sorts and limits above the rows.  The path has a cost, so the planner takes it
 * only when it is cheaper than what the row-moving scan gives (ADR 0022), and a
 * statement that is not closed gets exactly the plan it got before.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/fragment_scan.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "access/parallel.h"
#include "access/xact.h"
#include "catalog/pg_type_d.h"
#include "commands/explain.h"
#include "commands/explain_format.h"
#include "executor/executor.h"
#include "executor/spi.h"
#include "executor/tuptable.h"
#include "libpq/libpq-be-fe-helpers.h"
#include "miscadmin.h"
#include "nodes/extensible.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "nodes/pathnodes.h"
#include "optimizer/cost.h"
#include "optimizer/optimizer.h"
#include "optimizer/pathnode.h"
#include "optimizer/paths.h"
#include "parser/parsetree.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/portal.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"
#include "utils/wait_classes.h"

#include "catalog.h"
#include "fanout.h"
#include "fragment_scan.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "placement_check.h"
#include "prune.h"
#include "pushdown.h"

/* ======================================================================
 * The machinery
 * ====================================================================== */

static uint64 cursor_serial = 0;

static void frag_cancel_inflight(KsFrag *f);

/* A fault point of this set of fragments: "<prefix><what>", looked up only when a fault is set. */
static void
frag_fault(KsFragSet *fs, const char *what)
{
	if (fs->fp_prefix != NULL && kingres_shard_fault_spec()[0] != '\0')
		kingres_shard_fault_point(psprintf("%s%s", fs->fp_prefix, what));
}

/*
 * The groups a read has to ask (design 4.5).  Pruning items are intersected: a row of a join
 * on the distribution key is made by the group that owns its bucket, whichever table the
 * restriction names.
 */
List *
kingres_shard_frag_choose(const KsColocation *coloc, const KsCatalog *cat, List *prune,
						  ParamListInfo params, bool *local, int *nall, bool *pruned_out)
{
	List	   *groups = NIL;
	List	   *result = NIL;
	List	   *allowed = NIL;
	bool		pruned = false;
	bool		local_chosen = false;
	int			remote_all = 0;
	int			b;
	ListCell   *lc;

	for (b = 0; b < kingres_shard_coloc_nbuckets(coloc); b++)
	{
		int			g = kingres_shard_bucket_group(coloc, kingres_shard_coloc_bucket(coloc, b));

		if (g >= 0 && !list_member_int(groups, g))
			groups = lappend_int(groups, g);
	}
	foreach(lc, groups)
		if (lfirst_int(lc) != cat->local_group_id)
			remote_all++;
	if (nall != NULL)
		*nall = remote_all;

	foreach(lc, prune)
	{
		KsFragPrune *item = (KsFragPrune *) lfirst(lc);
		const KsDistTable *dt = kingres_shard_lookup_dist_table(item->relid);
		bool		p = false;
		List	   *found;
		MemoryContext tmp;
		MemoryContext oldcxt;
		List	   *copy = NIL;
		ListCell   *lc2;

		if (dt == NULL || dt->is_reference || item->terms == NIL)
			continue;

		/*
		 * The terms are evaluated in a context of their own, which is deleted at once: a rescan
		 * starts the scan again and again in the query context, and the arrays of a large IN
		 * list (detoasted, deconstructed) must not stay until the statement ends (review
		 * P2B-04).  Only the list of groups is copied out.
		 */
		tmp = AllocSetContextCreate(CurrentMemoryContext, "kingres_shard pruning", ALLOCSET_DEFAULT_SIZES);
		oldcxt = MemoryContextSwitchTo(tmp);
		found = kingres_shard_prune_groups(dt, item->terms, params, &p);
		MemoryContextSwitchTo(oldcxt);
		if (p)
		{
			foreach(lc2, found)
				copy = lappend_int(copy, lfirst_int(lc2));
			if (!pruned)
				allowed = copy;
			else
			{
				List	   *both = NIL;

				foreach(lc2, allowed)
					if (list_member_int(copy, lfirst_int(lc2)))
						both = lappend_int(both, lfirst_int(lc2));
				allowed = both;
			}
			pruned = true;
		}
		MemoryContextDelete(tmp);
	}
	if (pruned_out != NULL)
		*pruned_out = pruned;

	foreach(lc, groups)
	{
		int			g = lfirst_int(lc);
		const KsNode *n;
		KsFrag	   *f;

		if (pruned && !list_member_int(allowed, g))
			continue;
		if (g == cat->local_group_id)
		{
			local_chosen = true;
			continue;
		}
		n = kingres_shard_group_primary(g);
		if (n == NULL)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("shard group %d has no active primary node", g)));
		f = palloc0(sizeof(KsFrag));
		f->group_id = g;
		f->host = pstrdup(n->host);
		f->port = n->port;
		result = lappend(result, f);
	}
	if (local != NULL)
		*local = local_chosen;
	return result;
}

Node *
kingres_shard_renumber_params(Node *node, KsParamRenumber *ctx)
{
	if (node == NULL)
		return NULL;
	if (IsA(node, Param))
	{
		Param	   *p = (Param *) copyObject(node);
		int			pos = -1;
		int			i = 0;
		ListCell   *lc;

		if (p->paramkind != PARAM_EXTERN)
			return (Node *) p;
		foreach(lc, ctx->ids)
		{
			if (lfirst_int(lc) == p->paramid)
				pos = i;
			i++;
		}
		if (pos < 0)
		{
			ctx->ids = lappend_int(ctx->ids, p->paramid);
			pos = list_length(ctx->ids) - 1;
		}
		p->paramid = pos + 1;
		return (Node *) p;
	}
	return expression_tree_mutator(node, kingres_shard_renumber_params, ctx);
}

/* The value of an external parameter, as text, for the fragment. */
const char **
kingres_shard_fragment_params(ParamListInfo params, List *param_ids, Oid **types_out, int *n_out)
{
	int			n = list_length(param_ids);
	const char **values = palloc0(sizeof(char *) * Max(n, 1));
	Oid		   *types = palloc0(sizeof(Oid) * Max(n, 1));
	int			nest;
	int			i = 0;
	ListCell   *lc;

	nest = kingres_shard_set_transmission_modes();
	foreach(lc, param_ids)
	{
		int			id = lfirst_int(lc);
		ParamExternData *prm;
		ParamExternData workspace;
		Oid			outfunc;
		bool		isvarlena;

		if (params == NULL || id < 1 || id > params->numParams)
			elog(ERROR, "no value for parameter $%d of a distributed scan", id);
		if (params->paramFetch != NULL)
			prm = params->paramFetch(params, id, false, &workspace);
		else
			prm = &params->params[id - 1];
		types[i] = prm->ptype;
		if (!prm->isnull)
		{
			getTypeOutputInfo(prm->ptype, &outfunc, &isvarlena);
			values[i] = OidOutputFunctionCall(outfunc, prm->value);
		}
		i++;
	}
	kingres_shard_reset_transmission_modes(nest);
	*types_out = types;
	*n_out = n;
	return values;
}

/*
 * The batches of a scan that an error ended are libpq memory (malloc), which the memory
 * context machinery does not know: a reset callback of the context of the query clears them
 * (review finding S2-17); an error never reaches ExecEndNode.
 */
static void
fragset_reset_callback(void *arg)
{
	KsFragSet  *fs = (KsFragSet *) arg;
	ListCell   *lc;

	foreach(lc, fs->frags)
	{
		KsFrag	   *f = (KsFrag *) lfirst(lc);

		/*
		 * A connection must not keep pointing at a scan whose memory goes now (S6-02): whatever
		 * way the statement ended, an error, the abort of a subtransaction, a portal that is
		 * dropped.  What the scan had asked for is read and dropped.
		 */
		if (f->kc != NULL && f->in_flight)
		{
			frag_cancel_inflight(f);
			kingres_shard_conn_abandon_pending(f->kc, f);
			f->in_flight = false;
		}

		/*
		 * PostgreSQL 19 wraps every libpq result in an object that the memory context frees
		 * itself, in a reset callback registered after this one, so the results are gone
		 * already (see pg_compat.h).
		 */
		if (!KS_LIBPQ_RESULTS_OWNED_BY_CONTEXT && f->res != NULL)
			PQclear(f->res);
		f->res = NULL;
	}
}

void
kingres_shard_fragset_init(KsFragSet *fs, MemoryContext qcxt, int ntables)
{
	MemoryContextCallback *cb = MemoryContextAlloc(qcxt, sizeof(MemoryContextCallback));

	memset(fs, 0, sizeof(*fs));
	fs->qcxt = qcxt;
	fs->ntables = ntables;
	cb->func = fragset_reset_callback;
	cb->arg = fs;
	MemoryContextRegisterResetCallback(qcxt, cb);
}

static void frag_finish_pending(void *owner, bool quiet);

/*
 * The statement that asked for the batch in flight is ending in an error or a cancel (an abort path, or the
 * wait was interrupted): nobody wants the batch, and the node may be blocked for as long as a lock is
 * held.  It is asked to stop what it runs (for a bounded time), and what it answers is read afterwards.
 * The remote transaction ends with the abort of this one, or is brought back by the rollback to the
 * savepoint of the subtransaction.
 */
static void
frag_cancel_inflight(KsFrag *f)
{
	if (f->in_flight && f->kc != NULL && f->kc->conn != NULL && !f->kc->have_error &&
		PQstatus(f->kc->conn) == CONNECTION_OK)
		kingres_shard_cancel_with_deadline(f->kc->conn);
}

/*
 * Read the answer of the FETCH that is in flight.  With quiet (an abort path asks, and nothing
 * may be thrown) a failure is kept in the fragment and raised when the scan asks for the batch
 * (frag_check_failed).
 */
static void
frag_collect(KsFrag *f, bool quiet)
{
	PGresult   *res;
	MemoryContext oldcxt;

	if (!f->in_flight)
		return;

	/*
	 * The batch outlives this call, and the call can come from another statement that wants the
	 * connection (frag_finish_pending), in whatever memory context that statement is in.
	 * PostgreSQL 19 ties a libpq result to the context it is made in (see pg_compat.h), so make
	 * it in the one of the query, which lives as long as the scan does.
	 *
	 * The scan stays registered on the connection until the answer is read: if the wait is
	 * interrupted, the abort paths find the owner and let go of it (S6-02).
	 */
	oldcxt = MemoryContextSwitchTo(f->set->qcxt);
	if (quiet)
	{
		frag_cancel_inflight(f);
		res = kingres_shard_conn_collect_quiet(f->kc);
	}
	else
	{
		res = libpqsrv_get_result_last(f->kc->conn, PG_WAIT_EXTENSION);
		kingres_shard_conn_clear_pending(f->kc, f);
	}
	MemoryContextSwitchTo(oldcxt);
	f->kc->in_query = false;
	f->in_flight = false;
	if (res == NULL)
	{
		f->kc->have_error = true;
		if (quiet)
		{
			f->failed = true;
			return;
		}
		kingres_shard_fragment_error(f->kc, NULL, "FETCH");
	}
	if (PQresultStatus(res) != PGRES_TUPLES_OK)
	{
		if (quiet)
		{
			f->res = res;
			f->failed = true;
			return;
		}
		kingres_shard_fragment_error(f->kc, res, "FETCH");
	}
	f->res = res;
	f->nrows = PQntuples(res);
	f->next_row = 0;
	if (f->nrows < kingres_shard_scan_fetch_size)
		f->exhausted = true;
}

/* Raise what a quiet read of the answer found (see frag_collect). */
static void
frag_check_failed(KsFrag *f)
{
	PGresult   *res;

	if (!f->failed)
		return;
	f->failed = false;
	res = f->res;
	f->res = NULL;
	f->nrows = f->next_row = 0;
	kingres_shard_fragment_error(f->kc, res, "FETCH");
}

/* Called when something else wants the connection while a FETCH is in flight. */
static void
frag_finish_pending(void *owner, bool quiet)
{
	frag_collect((KsFrag *) owner, quiet);
}

static void
frag_send_fetch(KsFrag *f)
{
	char		sql[96];

	frag_fault(f->set, "before_fetch");
	snprintf(sql, sizeof(sql), "FETCH FORWARD %d FROM %s", kingres_shard_scan_fetch_size,
			 f->cursor);
	/* Another scan of this statement may have a fetch in flight on the same connection. */
	kingres_shard_conn_quiesce(f->kc);
	f->kc->in_query = true;
	if (!PQsendQuery(f->kc->conn, sql))
	{
		f->kc->have_error = true;
		f->kc->in_query = false;
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not send a command to node %s:%d", f->host, f->port),
				 errdetail_internal("%s", kingres_shard_conn_error_text(f->kc->conn))));
	}
	f->in_flight = true;
	kingres_shard_conn_set_pending(f->kc, f, frag_finish_pending);
}

/*
 * The statement fails the way a failed group does (placement_check.c, 4.6) when a command
 * cannot be sent: the connection is gone.
 */
static void
frag_send_failed(KsFrag *f, const char *what)
{
	f->kc->have_error = true;
	f->kc->in_query = false;
	kingres_shard_fragment_error(f->kc, NULL, what);
}

/*
 * Open the cursors on every group, then ask each for its first batch.  The procedure is the
 * placement check of design 4.3 (placement_check.c): the probe before, the DECLAREs, the probe
 * after, and no FETCH before every node has passed.
 */
void
kingres_shard_fragset_start(KsFragSet *fs, const KsFragStart *st)
{
	ListCell   *lc;
	char	   *ask;			/* the probe before */
	char	   *recheck;		/* the probe after the DECLAREs */

	fs->cur = 0;
	fs->fp_prefix = st->fp_declared_prefix;
	frag_fault(fs, "before_probe");

	/*
	 * A transaction uses one node of a group (4.4): checked for every node of the statement
	 * before anything is sent to any.
	 */
	foreach(lc, fs->frags)
	{
		KsFrag	   *f = (KsFrag *) lfirst(lc);

		f->set = fs;
		if (f->identity == NULL)
			f->identity = MemoryContextAllocZero(fs->qcxt, sizeof(int64) * Max(st->ntables, 1));
		kingres_shard_group_rule(f->host, f->port);
	}

	/*
	 * Step 2.  First every group is asked for its placement, the identity of its partitions of
	 * every table and its version of the extension (the question also makes the node's
	 * partition list of the table current, see kingres.internal_placement_hash), then the
	 * cursors are opened, then every group is asked again (step 4).  A node whose answers
	 * differ, or differ from the plan's, had a switch commit in between.
	 */
	ask = kingres_shard_probe_before_sql(st->colocation_id, st->ntables, st->relids);
	recheck = kingres_shard_probe_after_sql(st->colocation_id, st->ntables, st->relids);
	foreach(lc, fs->frags)
	{
		KsFrag	   *f = (KsFrag *) lfirst(lc);

		f->kc = kingres_shard_get_conn(f->host, f->port, false, NULL);
		kingres_shard_begin_xact(f->kc);
		f->kc->in_query = true;
		if (!PQsendQuery(f->kc->conn, ask))
			frag_send_failed(f, "placement");
		f->kc->nstatements++;
		f->in_flight = true;
		kingres_shard_conn_set_pending(f->kc, f, frag_finish_pending);
	}
	foreach(lc, fs->frags)
	{
		KsFrag	   *f = (KsFrag *) lfirst(lc);
		PGresult   *res;

		res = libpqsrv_get_result_last(f->kc->conn, PG_WAIT_EXTENSION);
		kingres_shard_conn_clear_pending(f->kc, f);
		f->kc->in_query = false;
		f->in_flight = false;
		if (res == NULL)
			f->kc->have_error = true;
		kingres_shard_probe_read_before(f->kc, res, st->ntables, st->plan_placement, f->identity);
	}
	kingres_shard_fault_point(st->fp_after_probe_before);

	/* DECLARE on every group first (they plan concurrently), then read the answers. */
	foreach(lc, fs->frags)
	{
		KsFrag	   *f = (KsFrag *) lfirst(lc);
		StringInfoData decl;

		snprintf(f->cursor, sizeof(f->cursor), "ks_scan_" UINT64_FORMAT, ++cursor_serial);
		initStringInfo(&decl);
		appendStringInfo(&decl, "%sDECLARE %s NO SCROLL CURSOR FOR %s", st->marker, f->cursor, st->sql);
		f->kc->in_query = true;
		if (!PQsendQueryParams(f->kc->conn, decl.data, st->nparams, st->ptypes, st->pvalues, NULL, NULL, 0))
			frag_send_failed(f, "DECLARE");
		/* libpq holds its own copy; the statement text of a scan with a large IN list is large (review P2B-04) */
		pfree(decl.data);
		f->kc->nstatements++;
		f->in_flight = true;
		kingres_shard_conn_set_pending(f->kc, f, frag_finish_pending);
	}
	foreach(lc, fs->frags)
	{
		KsFrag	   *f = (KsFrag *) lfirst(lc);
		PGresult   *res;

		/* The answer to DECLARE (not a batch). */
		res = libpqsrv_get_result_last(f->kc->conn, PG_WAIT_EXTENSION);
		kingres_shard_conn_clear_pending(f->kc, f);
		f->kc->in_query = false;
		f->in_flight = false;
		if (res == NULL || PQresultStatus(res) != PGRES_COMMAND_OK)
		{
			if (res == NULL)
				f->kc->have_error = true;
			kingres_shard_fragment_error(f->kc, res, "DECLARE");
		}
		PQclear(res);
		f->declared = true;
		if (kingres_shard_fault_spec()[0] != '\0')
			kingres_shard_fault_point(psprintf("%sdeclared_g%d", st->fp_declared_prefix, f->group_id));
	}
	kingres_shard_fault_point(st->fp_after_declare);

	/* The origin's own part, when there is one, opens its cursor now (design 4.4). */
	if (st->local_open != NULL)
		st->local_open(st->arg);

	/*
	 * Step 4, the map version rule (design/shard-phase4.md, "The map version rule";
	 * design/shard-phase2b.md, 4.3 and 10.2).  A node's bucket partitions of a table and its
	 * placement of the colocation group change only in the transaction of a switch, which
	 * commits on every participant.  The placement and the identity of the node's partitions
	 * of the tables were read before the cursor was opened and are read again now, from the
	 * catalog tables and not from a cache.  The placement of every node must be the plan's;
	 * the identity of a node must be what it was before: a switch that committed in between
	 * changes it, whatever it did to the placement (a bucket that left and came back leaves
	 * the placement as it was, but its partition on the node is a new table).  When both are
	 * unchanged, no switch committed on the node between the two reads, so the cursor was
	 * planned with the partitions of that placement.  What happens on the node later cannot
	 * change what the cursor reads: it holds the parent in ACCESS SHARE, which a detach (the
	 * source) waits for, and a partition attached later (the target) is not in its plan,
	 * which is what the placement says (the bucket is read from the source).  The statement
	 * fails with the retryable KS001; nothing has been returned yet.
	 */
	foreach(lc, fs->frags)
	{
		KsFrag	   *f = (KsFrag *) lfirst(lc);
		PGresult   *res = kingres_shard_exec_params(f->kc, recheck, 0, NULL, NULL, false);

		kingres_shard_probe_read_after(f->kc, res, st->ntables, st->plan_placement, f->identity);
	}
	/* This node's own placement and partitions can have changed in the same window. */
	if (st->after_check != NULL)
		st->after_check(st->arg);
	kingres_shard_fault_point(st->fp_after_check);
	if (st->fp_first_fetch != NULL)
		kingres_shard_fault_point(st->fp_first_fetch);
	foreach(lc, fs->frags)
		frag_send_fetch((KsFrag *) lfirst(lc));
}

/* Read the batch in flight, forgetting an error: the scan is being closed. */
static void
frag_drain(KsFrag *f)
{
	MemoryContext oldcxt = CurrentMemoryContext;

	PG_TRY();
	{
		frag_collect(f, false);
	}
	PG_CATCH();
	{
		MemoryContextSwitchTo(oldcxt);
		FlushErrorState();
		/* An interrupt in the wait leaves the query in flight: read it and let go. */
		if (f->in_flight)
		{
			frag_cancel_inflight(f);
			kingres_shard_conn_abandon_pending(f->kc, f);
		}
		f->in_flight = false;
	}
	PG_END_TRY();
}

/* Close cursors and free batches. */
void
kingres_shard_fragset_stop(KsFragSet *fs, bool close_cursors)
{
	ListCell   *lc;

	foreach(lc, fs->frags)
	{
		KsFrag	   *f = (KsFrag *) lfirst(lc);

		if (f->in_flight && f->kc != NULL && f->kc->conn != NULL && !f->kc->have_error)
		{
			/* Let the batch that was asked for arrive: cancelling would abort the remote transaction. */
			frag_drain(f);
		}
		else if (f->in_flight && f->kc != NULL)
		{
			frag_cancel_inflight(f);
			kingres_shard_conn_abandon_pending(f->kc, f);
			f->in_flight = false;
		}
		f->failed = false;
		if (f->res != NULL)
			PQclear(f->res);
		f->res = NULL;
		if (close_cursors && f->declared && f->kc != NULL && f->kc->conn != NULL &&
			!f->kc->have_error && f->kc->xact_depth > 0 &&
			PQstatus(f->kc->conn) == CONNECTION_OK &&
			PQtransactionStatus(f->kc->conn) == PQTRANS_INTRANS)
		{
			char		sql[64];
			PGresult   *res;

			kingres_shard_conn_quiesce(f->kc);
			snprintf(sql, sizeof(sql), "CLOSE %s", f->cursor);
			res = kingres_shard_bounded_exec(f->kc->conn, sql, false);
			PQclear(res);
		}
		f->declared = false;
	}
	/*
	 * A rescan starts the scan again in the query context: what the fragments of this run took
	 * must not stay until the statement ends (review P2B-04).
	 */
	foreach(lc, fs->frags)
	{
		KsFrag	   *f = (KsFrag *) lfirst(lc);

		if (f->kc != NULL && f->kc->pending_owner == (void *) f)
			continue;
		pfree(f->host);
		if (f->identity != NULL)
			pfree(f->identity);
		pfree(f);
	}
	list_free(fs->frags);
	fs->frags = NIL;
	fs->started = false;
}

bool
kingres_shard_fragset_next(KsFragSet *fs, PGresult **res_out, int *row_out, KsFrag **frag_out)
{
	int			nfrags = list_length(fs->frags);

	if (nfrags == 0)
		return false;
	for (;;)
	{
		int			i;
		bool		any_pending = false;
		KsFrag	   *f = NULL;

		/* An answer that was read in an abort path and was an error (S6-02). */
		for (i = 0; i < nfrags; i++)
			frag_check_failed((KsFrag *) list_nth(fs->frags, i));

		/* A batch with rows left. */
		for (i = 0; i < nfrags; i++)
		{
			KsFrag	   *c = (KsFrag *) list_nth(fs->frags, (fs->cur + i) % nfrags);

			if (c->res != NULL && c->next_row < c->nrows)
			{
				f = c;
				fs->cur = (fs->cur + i) % nfrags;
				break;
			}
		}
		if (f != NULL)
		{
			*res_out = f->res;
			*row_out = f->next_row++;
			*frag_out = f;
			return true;
		}

		/* Batches that are used up: free them and ask for the next ones. */
		for (i = 0; i < nfrags; i++)
		{
			KsFrag	   *c = (KsFrag *) list_nth(fs->frags, i);

			if (c->res != NULL && c->next_row >= c->nrows)
			{
				PQclear(c->res);
				c->res = NULL;
				c->nrows = 0;
			}
			if (c->res == NULL && !c->in_flight && !c->exhausted)
				frag_send_fetch(c);
			if (c->in_flight)
				any_pending = true;
		}
		if (!any_pending)
		{
			bool		anything = false;

			for (i = 0; i < nfrags; i++)
			{
				KsFrag	   *c = (KsFrag *) list_nth(fs->frags, i);

				if (c->res != NULL && c->next_row < c->nrows)
					anything = true;
			}
			if (!anything)
				return false;
			continue;
		}
		/* Wait for the first group that has a batch coming. */
		for (i = 0; i < nfrags; i++)
		{
			KsFrag	   *c = (KsFrag *) list_nth(fs->frags, (fs->cur + i) % nfrags);

			if (c->in_flight)
			{
				frag_fault(fs, "wait_batch");
				frag_collect(c, false);
				break;
			}
		}
	}
}

/* ======================================================================
 * The Kingres Fragment Scan
 * ====================================================================== */

/* Layout of CustomScan.custom_private. */
#define FS_PRIV_SQL			0	/* String: the statement every group runs */
#define FS_PRIV_PARAMS		1	/* List of Integer: external parameter ids, in the order of $1, $2, ... */
#define FS_PRIV_COLOC		2	/* Integer: the colocation group */
#define FS_PRIV_PLACEMENT	3	/* String: the placement of the group the statement was planned with */
#define FS_PRIV_RELS		4	/* String: "relid:colocation," of every relation */
#define FS_PRIV_DIST		5	/* List of Integer: the distributed tables (the tables of the probes) */
#define FS_PRIV_PRUNE		6	/* List of List (Integer relid, terms): the pruning of the restrictions */
#define FS_PRIV_KIND		7	/* String: what is pushed */
#define FS_PRIV_NCOLS		8	/* Integer: columns of a row */

/* The rows one group returned, for EXPLAIN ANALYZE. */
typedef struct FsGroupRows
{
	int			group_id;		/* -1: this node's own part */
	int64		rows;
} FsGroupRows;

typedef struct KsFragScanState
{
	CustomScanState css;
	KsFragSet	fs;
	char	   *sql;
	List	   *param_ids;
	int			colocation_id;
	int64		plan_placement;
	char	   *rels;
	List	   *dist_relids;	/* of Oid */
	List	   *prune;			/* of KsFragPrune * */
	char	   *kind;
	int			ncols;
	FmgrInfo   *infuncs;
	Oid		   *typioparams;
	int32	   *typmods;
	bool		started;
	/* this node's own part */
	bool		has_local;
	char	   *portal_name;
	TupleDesc	local_desc;
	MemoryContext lcxt;
	HeapTuple  *lbatch;
	int			nl;
	int			next_l;
	bool		lexhausted;
	int64	   *ident_before;	/* the identity of this node's partitions of every distributed table before the cursor opens */
	List	   *group_rows;		/* of FsGroupRows * */
} KsFragScanState;

static create_upper_paths_hook_type prev_create_upper_paths_hook = NULL;
static KsPushdownInfo *current_info = NULL;
static const char *current_reason = NULL;

static Plan *fs_plan_custom_path(PlannerInfo *root, RelOptInfo *rel, CustomPath *best_path,
								 List *tlist, List *clauses, List *custom_plans);
static Node *fs_create_scan_state(CustomScan *cscan);
static void fs_begin_scan(CustomScanState *node, EState *estate, int eflags);
static TupleTableSlot *fs_exec_scan(CustomScanState *node);
static void fs_end_scan(CustomScanState *node);
static void fs_rescan(CustomScanState *node);
static void fs_explain_scan(CustomScanState *node, List *ancestors, ExplainState *es);
static void fs_upper_paths(PlannerInfo *root, UpperRelationKind stage, RelOptInfo *input_rel,
						   RelOptInfo *output_rel, void *extra);

static const CustomPathMethods fs_path_methods = {
	.CustomName = "Kingres Fragment Scan",
	.PlanCustomPath = fs_plan_custom_path
};

static const CustomScanMethods fs_scan_methods = {
	.CustomName = "Kingres Fragment Scan",
	.CreateCustomScanState = fs_create_scan_state
};

static const CustomExecMethods fs_exec_methods = {
	.CustomName = "Kingres Fragment Scan",
	.BeginCustomScan = fs_begin_scan,
	.ExecCustomScan = fs_exec_scan,
	.EndCustomScan = fs_end_scan,
	.ReScanCustomScan = fs_rescan,
	.ExplainCustomScan = fs_explain_scan
};

void
kingres_shard_fragment_scan_init(void)
{
	RegisterCustomScanMethods(&fs_scan_methods);
	prev_create_upper_paths_hook = create_upper_paths_hook;
	create_upper_paths_hook = fs_upper_paths;
}

KsPushdownInfo *
kingres_shard_pushdown_info_set(KsPushdownInfo *info)
{
	KsPushdownInfo *old = current_info;

	current_info = info;
	return old;
}

const char *
kingres_shard_pushdown_reason_swap(const char *reason)
{
	const char *old = current_reason;

	current_reason = reason;
	return old;
}

const char *
kingres_shard_pushdown_reason(void)
{
	return current_reason;
}

/* "relid:colocation," for each relation, the colocation 0 for a reference table (as fanout.c writes it). */
static char *
relations_signature(List *relids)
{
	StringInfoData buf;
	ListCell   *lc;

	initStringInfo(&buf);
	foreach(lc, relids)
	{
		const KsDistTable *dt = kingres_shard_lookup_dist_table(lfirst_oid(lc));

		appendStringInfo(&buf, "%u:%d,", lfirst_oid(lc),
						 (dt == NULL || dt->is_reference) ? 0 : dt->colocation_id);
	}
	return buf.data;
}

/*
 * The relations of the plan are what they were when it was planned.  The plan is invalidated by
 * the relation cache when a table is dropped, distributed again or made a reference table; this
 * is the second defence, which holds when an invalidation was missed (design 10.4).
 */
static void
check_relations_unchanged(const char *signature)
{
	const char *p = signature;

	while (*p != '\0')
	{
		char	   *end;
		Oid			relid = (Oid) strtoul(p, &end, 10);
		int			coloc;
		const KsDistTable *dt;
		int			now;

		if (*end != ':')
			elog(ERROR, "malformed relation list in the plan of a fragment scan");
		coloc = (int) strtol(end + 1, &end, 10);
		if (*end != ',')
			elog(ERROR, "malformed relation list in the plan of a fragment scan");
		p = end + 1;

		dt = kingres_shard_lookup_dist_table(relid);
		now = (dt == NULL) ? -1 : (dt->is_reference ? 0 : dt->colocation_id);
		if (now != coloc)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_STALE_MAP),
					 errmsg("table \"%s\" is no longer what it was when this statement was planned",
							get_rel_name(relid) ? get_rel_name(relid) : "(dropped)"),
					 errdetail("The plan runs the statement whole on every shard group, which is right only while the tables keep their colocation group."),
					 errhint("Repeat the statement; it is planned again.")));
	}
}

/* ---- the path ---- */

/*
 * The pruning items of the statement: the terms of the restrictions of every distributed table
 * (baserestrictinfo has the right range table indexes at path time).
 */
static List *
fs_prune_items(PlannerInfo *root)
{
	List	   *items = NIL;
	int			i;

	for (i = 1; i < root->simple_rel_array_size; i++)
	{
		RelOptInfo *rel = root->simple_rel_array[i];
		RangeTblEntry *rte = root->simple_rte_array[i];
		const KsDistTable *dt;
		List	   *terms;
		KsFragPrune *item;

		if (rel == NULL || rte == NULL || rel->reloptkind != RELOPT_BASEREL ||
			rte->rtekind != RTE_RELATION)
			continue;
		dt = kingres_shard_lookup_dist_table(rte->relid);
		if (dt == NULL || dt->is_reference)
			continue;
		terms = kingres_shard_prune_terms(dt, (Index) i, rel->baserestrictinfo);
		if (terms == NIL)
			continue;
		item = palloc(sizeof(KsFragPrune));
		item->relid = rte->relid;
		item->terms = terms;
		items = lappend(items, item);
	}
	return items;
}

static void
fs_upper_paths(PlannerInfo *root, UpperRelationKind stage, RelOptInfo *input_rel,
			   RelOptInfo *output_rel, void *extra)
{
	KsPushdownInfo *pi = current_info;
	Query	   *parse = root->parse;
	PathTarget *target;
	Path	   *base = NULL;
	ListCell   *lc;
	const KsCatalog *cat;
	const KsColocation *coloc;
	List	   *prune;
	List	   *frags;
	bool		local = false;
	int			nremote;
	int			ntotal;
	CustomPath *cpath;
	Path	   *p;
	double		startup;
	double		total;
	double		rows;

	if (prev_create_upper_paths_hook)
		prev_create_upper_paths_hook(root, stage, input_rel, output_rel, extra);
	if (stage != UPPERREL_FINAL || pi == NULL || parse != pi->parse || root->query_level != 1)
		return;
	if (IsParallelWorker())
		return;

	target = root->upper_targets[UPPERREL_FINAL];
	if (target == NULL || list_length(target->exprs) != pi->ncols)
		return;
	foreach(lc, input_rel->pathlist)
	{
		Path	   *c = (Path *) lfirst(lc);

		if (base == NULL || c->total_cost < base->total_cost)
			base = c;
	}
	if (base == NULL)
		return;

	cat = kingres_shard_catalog();
	if (!cat->present)
		return;
	coloc = kingres_shard_lookup_colocation(pi->colocation_id);
	if (coloc == NULL)
		return;
	prune = fs_prune_items(root);
	frags = kingres_shard_frag_choose(coloc, cat, prune, root->glob->boundParams, &local, NULL, NULL);
	nremote = list_length(frags);
	ntotal = nremote + (local ? 1 : 0);

	/*
	 * What the groups do is what the planner priced for the whole statement, divided among the
	 * groups that run at the same time, plus the transfer of the final rows.  A statement that
	 * returns about as many rows as it reads gains little and loses to the scan.
	 */
	startup = 100.0 * nremote;
	if (ntotal == 0)
		total = startup + 1.0;
	else
		total = startup + Max(base->total_cost - startup, 0.0) / ntotal +
			clamp_row_est(base->rows) * (cpu_tuple_cost * 2.0 + 0.02) * nremote / ntotal;
	rows = clamp_row_est(base->rows);

	cpath = makeNode(CustomPath);
	cpath->path.pathtype = T_CustomScan;
	cpath->path.parent = output_rel;
	cpath->path.pathtarget = target;
	cpath->path.param_info = NULL;
	cpath->path.parallel_aware = false;
	cpath->path.parallel_safe = false;
	cpath->path.parallel_workers = 0;
	cpath->path.pathkeys = NIL;
	cpath->path.rows = rows;
	cpath->path.disabled_nodes = 0;
	cpath->path.startup_cost = startup;
	cpath->path.total_cost = total;
	cpath->flags = 0;
	cpath->custom_paths = NIL;
	cpath->custom_restrictinfo = NIL;
	cpath->custom_private = list_make1(prune);
	cpath->methods = &fs_path_methods;
	p = (Path *) cpath;

	/* The origin sorts and limits above the rows the groups return (design 6.4; section 8 of the design is a later step). */
	if (root->sort_pathkeys != NIL)
		p = (Path *) create_sort_path(root, output_rel, p, root->sort_pathkeys, root->limit_tuples);
	if (parse->limitCount != NULL || parse->limitOffset != NULL)
	{
		int64		offset_est = 0;
		int64		count_est = 0;

		if (parse->limitOffset != NULL)
			offset_est = (IsA(parse->limitOffset, Const) && !((Const *) parse->limitOffset)->constisnull &&
						  ((Const *) parse->limitOffset)->consttype == INT8OID) ?
				Max(DatumGetInt64(((Const *) parse->limitOffset)->constvalue), 0) : 0;
		if (parse->limitCount != NULL)
			count_est = (IsA(parse->limitCount, Const) && !((Const *) parse->limitCount)->constisnull &&
						 ((Const *) parse->limitCount)->consttype == INT8OID) ?
				Max(DatumGetInt64(((Const *) parse->limitCount)->constvalue), 1) : -1;
		p = (Path *) create_limit_path(root, output_rel, p, parse->limitOffset, parse->limitCount,
									   parse->limitOption, offset_est, count_est);
	}
	add_path(output_rel, p);
}

static Plan *
fs_plan_custom_path(PlannerInfo *root, RelOptInfo *rel, CustomPath *best_path, List *tlist,
					List *clauses, List *custom_plans)
{
	KsPushdownInfo *pi = current_info;
	CustomScan *cscan = makeNode(CustomScan);
	List	   *prune_items = (List *) linitial(best_path->custom_private);
	List	   *param_ids = NIL;
	List	   *dist = NIL;
	List	   *prune = NIL;
	List	   *scan_tlist = NIL;
	ListCell   *lc;

	if (pi == NULL || root->parse != pi->parse)
		elog(ERROR, "a fragment scan was planned for a statement that was not decided");
	foreach(lc, pi->param_ids)
		param_ids = lappend(param_ids, makeInteger(lfirst_int(lc)));
	foreach(lc, pi->dist_relids)
		dist = lappend(dist, makeInteger((int) lfirst_oid(lc)));
	foreach(lc, prune_items)
	{
		KsFragPrune *item = (KsFragPrune *) lfirst(lc);

		prune = lappend(prune, list_make2(makeInteger((int) item->relid), item->terms));
	}
	foreach(lc, tlist)
	{
		TargetEntry *te = lfirst_node(TargetEntry, lc);

		scan_tlist = lappend(scan_tlist, flatCopyTargetEntry(te));
	}

	cscan->scan.plan.targetlist = tlist;
	cscan->scan.plan.qual = NIL;
	cscan->scan.scanrelid = 0;
	cscan->flags = best_path->flags;
	cscan->custom_scan_tlist = scan_tlist;
	cscan->custom_plans = NIL;
	cscan->custom_relids = bms_copy(root->all_baserels);
	cscan->custom_private = list_make5(makeString(pstrdup(pi->sql)), param_ids,
									   makeInteger(pi->colocation_id),
									   makeString(psprintf(INT64_FORMAT, pi->place)),
									   makeString(relations_signature(pi->relids)));
	cscan->custom_private = lappend(cscan->custom_private, dist);
	cscan->custom_private = lappend(cscan->custom_private, prune);
	cscan->custom_private = lappend(cscan->custom_private, makeString(pstrdup(pi->kind)));
	cscan->custom_private = lappend(cscan->custom_private, makeInteger(pi->ncols));
	cscan->methods = &fs_scan_methods;
	return &cscan->scan.plan;
}

/* ---- the executor ---- */

static Node *
fs_create_scan_state(CustomScan *cscan)
{
	KsFragScanState *s = palloc0(sizeof(KsFragScanState));
	List	   *p = cscan->custom_private;
	ListCell   *lc;

	NodeSetTag(&s->css, T_CustomScanState);
	s->css.methods = &fs_exec_methods;
	s->css.slotOps = &TTSOpsVirtual;
	s->sql = pstrdup(strVal(list_nth(p, FS_PRIV_SQL)));
	foreach(lc, (List *) list_nth(p, FS_PRIV_PARAMS))
		s->param_ids = lappend_int(s->param_ids, intVal(lfirst(lc)));
	s->colocation_id = intVal(list_nth(p, FS_PRIV_COLOC));
	s->plan_placement = pg_strtoint64(strVal(list_nth(p, FS_PRIV_PLACEMENT)));
	s->rels = pstrdup(strVal(list_nth(p, FS_PRIV_RELS)));
	foreach(lc, (List *) list_nth(p, FS_PRIV_DIST))
		s->dist_relids = lappend_oid(s->dist_relids, (Oid) intVal(lfirst(lc)));
	foreach(lc, (List *) list_nth(p, FS_PRIV_PRUNE))
	{
		List	   *pair = (List *) lfirst(lc);
		KsFragPrune *item = palloc(sizeof(KsFragPrune));

		item->relid = (Oid) intVal(linitial(pair));
		item->terms = (List *) lsecond(pair);
		s->prune = lappend(s->prune, item);
	}
	s->kind = pstrdup(strVal(list_nth(p, FS_PRIV_KIND)));
	s->ncols = intVal(list_nth(p, FS_PRIV_NCOLS));
	return (Node *) s;
}

static void
fs_begin_scan(CustomScanState *node, EState *estate, int eflags)
{
	KsFragScanState *s = (KsFragScanState *) node;
	TupleDesc	tupdesc = node->ss.ss_ScanTupleSlot->tts_tupleDescriptor;
	int			i;

	if (IsParallelWorker())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a distributed scan cannot run in a parallel worker"),
				 errhint("Mark the function that runs the statement PARALLEL RESTRICTED or PARALLEL UNSAFE.")));
	if (tupdesc->natts != s->ncols)
		elog(ERROR, "the fragment scan returns %d columns and its plan has %d", s->ncols, tupdesc->natts);
	kingres_shard_fragset_init(&s->fs, estate->es_query_cxt, list_length(s->dist_relids));
	s->lcxt = AllocSetContextCreate(estate->es_query_cxt, "kingres_shard fragment local batch",
									ALLOCSET_DEFAULT_SIZES);
	s->infuncs = palloc0(sizeof(FmgrInfo) * Max(s->ncols, 1));
	s->typioparams = palloc0(sizeof(Oid) * Max(s->ncols, 1));
	s->typmods = palloc0(sizeof(int32) * Max(s->ncols, 1));
	for (i = 0; i < s->ncols; i++)
	{
		Form_pg_attribute att = TupleDescAttr(tupdesc, i);
		Oid			infunc;

		getTypeInputInfo(att->atttypid, &infunc, &s->typioparams[i]);
		fmgr_info(infunc, &s->infuncs[i]);
		s->typmods[i] = att->atttypmod;
	}
}

/* Does a plan (the local part of the statement) hold a scan of this extension anywhere? */
static bool
plan_has_kingres_scan(Plan *plan)
{
	ListCell   *lc;

	if (plan == NULL)
		return false;
	if (IsA(plan, CustomScan))
	{
		CustomScan *c = (CustomScan *) plan;

		if (c->methods != NULL && c->methods->CustomName != NULL &&
			strncmp(c->methods->CustomName, "Kingres", 7) == 0)
			return true;
		foreach(lc, c->custom_plans)
			if (plan_has_kingres_scan((Plan *) lfirst(lc)))
				return true;
	}
	if (plan_has_kingres_scan(plan->lefttree) || plan_has_kingres_scan(plan->righttree))
		return true;
	switch (nodeTag(plan))
	{
		case T_Append:
			foreach(lc, ((Append *) plan)->appendplans)
				if (plan_has_kingres_scan((Plan *) lfirst(lc)))
					return true;
			break;
		case T_MergeAppend:
			foreach(lc, ((MergeAppend *) plan)->mergeplans)
				if (plan_has_kingres_scan((Plan *) lfirst(lc)))
					return true;
			break;
		case T_BitmapAnd:
			foreach(lc, ((BitmapAnd *) plan)->bitmapplans)
				if (plan_has_kingres_scan((Plan *) lfirst(lc)))
					return true;
			break;
		case T_BitmapOr:
			foreach(lc, ((BitmapOr *) plan)->bitmapplans)
				if (plan_has_kingres_scan((Plan *) lfirst(lc)))
					return true;
			break;
		default:
			break;
	}
	return false;
}

/* The identity of this node's own partitions of every distributed table of the statement. */
static void
local_identities(KsFragScanState *s, int64 *out)
{
	int			i = 0;
	ListCell   *lc;

	foreach(lc, s->dist_relids)
		out[i++] = kingres_shard_partition_identity_now(lfirst_oid(lc));
}

/*
 * The origin's own part opens its cursor (design 4.4): the identities of its own partitions are read
 * before, the statement is run through SPI as a cursor, planned for this node only (the mark of
 * fanout.c is consumed by the first planner call of the SPI call), and the identities are read again
 * by local_check, after the other groups were checked.
 */
static void
fs_local_open(void *arg)
{
	KsFragScanState *s = (KsFragScanState *) arg;
	EState	   *estate = s->css.ss.ps.state;
	ParamListInfo params = estate->es_param_list_info;
	int			n = list_length(s->param_ids);
	Oid		   *argtypes = palloc(sizeof(Oid) * Max(n, 1));
	Datum	   *values = palloc(sizeof(Datum) * Max(n, 1));
	char	   *nulls = palloc(Max(n, 1));
	int			i = 0;
	ListCell   *lc;
	Portal		portal;
	int			nest;
	MemoryContext oldcxt;

	if (!s->has_local)
		return;
	kingres_shard_fault_point("frag_local_open");

	foreach(lc, s->param_ids)
	{
		int			id = lfirst_int(lc);
		ParamExternData *prm;
		ParamExternData workspace;

		if (params == NULL || id < 1 || id > params->numParams)
			elog(ERROR, "no value for parameter $%d of a distributed scan", id);
		if (params->paramFetch != NULL)
			prm = params->paramFetch(params, id, false, &workspace);
		else
			prm = &params->params[id - 1];
		argtypes[i] = prm->ptype;
		values[i] = prm->value;
		nulls[i] = prm->isnull ? 'n' : ' ';
		i++;
	}

	s->ident_before = MemoryContextAlloc(estate->es_query_cxt,
										 sizeof(int64) * Max(list_length(s->dist_relids), 1));
	local_identities(s, s->ident_before);
	kingres_shard_check_origin_placement(s->colocation_id, s->plan_placement, "starting to read it");

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	nest = kingres_shard_set_transmission_modes();
	portal = kingres_shard_spi_local_part_cursor(s->sql, n, argtypes, values, nulls);
	kingres_shard_reset_transmission_modes(nest);

	/*
	 * The mark of the local part is consumed by the first call of the planner hook of the SPI
	 * call.  A plan that was invalidated while it was being made is planned again without the
	 * mark, and the planner then plans the statement as a statement of this session: with the
	 * scan, which would read the other groups a second time.  Never run such a plan.
	 */
	if (portal->stmts != NIL)
	{
		foreach(lc, portal->stmts)
		{
			PlannedStmt *pstmt = (PlannedStmt *) lfirst(lc);

			if (IsA(pstmt, PlannedStmt) && plan_has_kingres_scan(pstmt->planTree))
				ereport(ERROR,
						(errcode(ERRCODE_KINGRES_STALE_MAP),
						 errmsg("the statement of this node's own shard group was planned again while it started"),
						 errhint("Repeat the statement.")));
		}
	}
	oldcxt = MemoryContextSwitchTo(estate->es_query_cxt);
	s->portal_name = pstrdup(portal->name);
	s->local_desc = CreateTupleDescCopy(portal->tupDesc);
	MemoryContextSwitchTo(oldcxt);
	SPI_finish();

	if (s->local_desc->natts != s->ncols)
		ereport(ERROR,
				(errcode(ERRCODE_DATA_EXCEPTION),
				 errmsg("this node's own part returned %d columns, expected %d", s->local_desc->natts, s->ncols)));
	for (i = 0; i < s->ncols; i++)
		if (TupleDescAttr(s->local_desc, i)->atttypid !=
			TupleDescAttr(s->css.ss.ss_ScanTupleSlot->tts_tupleDescriptor, i)->atttypid)
			ereport(ERROR,
					(errcode(ERRCODE_DATA_EXCEPTION),
					 errmsg("column %d of this node's own part has another type than the plan expects", i + 1)));
	s->nl = s->next_l = 0;
	s->lexhausted = false;
}

/* After the probes: this node's own placement and partitions are what the plan assumed. */
static void
fs_local_check(void *arg)
{
	KsFragScanState *s = (KsFragScanState *) arg;

	if (!s->has_local)
		return;
	kingres_shard_check_origin_placement(s->colocation_id, s->plan_placement, "reading it");
	{
		int			n = list_length(s->dist_relids);
		int64	   *now = palloc(sizeof(int64) * Max(n, 1));
		int			i;

		local_identities(s, now);
		for (i = 0; i < n; i++)
			if (now[i] != s->ident_before[i])
				ereport(ERROR,
						(errcode(ERRCODE_KINGRES_STALE_MAP),
						 errmsg("the partitions of a table on this node changed while the statement was starting"),
						 errhint("A bucket moved between shard groups; repeat the statement.")));
		pfree(now);
	}
}

static void
fs_local_close(KsFragScanState *s)
{
	if (s->portal_name != NULL)
	{
		Portal		portal = GetPortalByName(s->portal_name);

		if (PortalIsValid(portal))
			SPI_cursor_close(portal);
		pfree(s->portal_name);
		s->portal_name = NULL;
	}
	if (s->local_desc != NULL)
	{
		FreeTupleDesc(s->local_desc);
		s->local_desc = NULL;
	}
	if (s->lcxt != NULL)
		MemoryContextReset(s->lcxt);
	s->lbatch = NULL;
	s->nl = s->next_l = 0;
	s->has_local = false;
}

static void
fs_start(KsFragScanState *s)
{
	EState	   *estate = s->css.ss.ps.state;
	const KsCatalog *cat;
	const KsColocation *coloc;
	MemoryContext startcxt;
	MemoryContext old;
	const char **values;
	Oid		   *types;
	int			nparams;
	Oid		   *relids;
	int			nrels = list_length(s->dist_relids);
	KsFragStart st;
	bool		local = false;
	int			i = 0;
	ListCell   *lc;

	check_relations_unchanged(s->rels);
	cat = kingres_shard_catalog();
	coloc = kingres_shard_lookup_colocation(s->colocation_id);
	if (coloc == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("colocation group %d no longer exists", s->colocation_id),
				 errhint("Repeat the statement; it is planned again.")));

	/* Step 1 of 4.3: the plan's placement is the one this backend knows. */
	kingres_shard_placement_check_plan(s->colocation_id, s->plan_placement);

	old = MemoryContextSwitchTo(estate->es_query_cxt);
	s->fs.frags = kingres_shard_frag_choose(coloc, cat, s->prune, estate->es_param_list_info,
											&local, NULL, NULL);
	MemoryContextSwitchTo(old);
	s->has_local = local;
	s->fs.cur = 0;

	/* The values of the parameters and the lists of the call are made in a context of their own. */
	startcxt = AllocSetContextCreate(CurrentMemoryContext, "kingres_shard fragment start", ALLOCSET_SMALL_SIZES);
	old = MemoryContextSwitchTo(startcxt);
	values = kingres_shard_fragment_params(estate->es_param_list_info, s->param_ids, &types, &nparams);
	relids = palloc(sizeof(Oid) * Max(nrels, 1));
	foreach(lc, s->dist_relids)
		relids[i++] = lfirst_oid(lc);

	memset(&st, 0, sizeof(st));
	st.colocation_id = s->colocation_id;
	st.ntables = nrels;
	st.relids = relids;
	st.plan_placement = s->plan_placement;
	st.marker = kingres_shard_query_marker(s->plan_placement);
	st.sql = s->sql;
	st.nparams = nparams;
	st.ptypes = types;
	st.pvalues = values;
	st.local_open = fs_local_open;
	st.after_check = fs_local_check;
	st.arg = s;
	st.fp_after_probe_before = "frag_after_probe_before";
	st.fp_after_declare = "frag_after_declare";
	st.fp_after_check = "frag_after_probe_after";
	st.fp_first_fetch = "frag_first_fetch";
	st.fp_declared_prefix = "frag_";
	PG_TRY();
	{
		kingres_shard_fragset_start(&s->fs, &st);
	}
	PG_FINALLY();
	{
		MemoryContextSwitchTo(old);
		MemoryContextDelete(startcxt);
	}
	PG_END_TRY();
	s->fs.started = true;
	s->started = true;
}

static void
fs_local_fetch(KsFragScanState *s)
{
	Portal		portal;
	uint64		n;
	uint64		k;
	MemoryContext oldcxt;
	int			nest;

	kingres_shard_fault_point("frag_local_fetch");
	MemoryContextReset(s->lcxt);
	s->lbatch = NULL;
	s->nl = s->next_l = 0;
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	nest = kingres_shard_set_transmission_modes();
	portal = SPI_cursor_find(s->portal_name);
	if (portal == NULL)
		elog(ERROR, "the cursor of this node's own part of a fragment scan is gone");
	SPI_cursor_fetch(portal, true, kingres_shard_scan_fetch_size);
	kingres_shard_reset_transmission_modes(nest);
	n = SPI_processed;
	oldcxt = MemoryContextSwitchTo(s->lcxt);
	s->lbatch = palloc(sizeof(HeapTuple) * Max(n, 1));
	for (k = 0; k < n; k++)
		s->lbatch[k] = heap_copytuple(SPI_tuptable->vals[k]);
	MemoryContextSwitchTo(oldcxt);
	s->nl = (int) n;
	if (n < (uint64) kingres_shard_scan_fetch_size)
		s->lexhausted = true;
	SPI_freetuptable(SPI_tuptable);
	SPI_finish();
}

static void
count_group_row(KsFragScanState *s, int group_id)
{
	ListCell   *lc;
	FsGroupRows *g;

	foreach(lc, s->group_rows)
	{
		g = (FsGroupRows *) lfirst(lc);
		if (g->group_id == group_id)
		{
			g->rows++;
			return;
		}
	}
	g = MemoryContextAlloc(s->css.ss.ps.state->es_query_cxt, sizeof(FsGroupRows));
	g->group_id = group_id;
	g->rows = 1;
	s->group_rows = lappend(s->group_rows, g);
}

/* The next row of this node's own part, or of the other groups, into the scan slot; false at the end. */
static bool
fs_next_row(KsFragScanState *s, TupleTableSlot *slot, ExprContext *econtext)
{
	PGresult   *res;
	int			row;
	KsFrag	   *f;
	int			j;
	MemoryContext oldcxt;

	if (s->has_local)
	{
		if (s->next_l >= s->nl && !s->lexhausted)
			fs_local_fetch(s);
		if (s->next_l < s->nl)
		{
			HeapTuple	tup = s->lbatch[s->next_l++];

			ExecClearTuple(slot);
			heap_deform_tuple(tup, s->local_desc, slot->tts_values, slot->tts_isnull);
			ExecStoreVirtualTuple(slot);
			if (s->css.ss.ps.state->es_instrument)
				count_group_row(s, -1);
			return true;
		}
	}
	if (!kingres_shard_fragset_next(&s->fs, &res, &row, &f))
		return false;
	if (PQnfields(res) != s->ncols)
		ereport(ERROR,
				(errcode(ERRCODE_DATA_EXCEPTION),
				 errmsg("node %s:%d returned %d columns, expected %d",
						f->host, f->port, PQnfields(res), s->ncols)));
	ExecClearTuple(slot);
	/* The values live until the next call, which resets this context. */
	oldcxt = MemoryContextSwitchTo(econtext->ecxt_per_tuple_memory);
	for (j = 0; j < s->ncols; j++)
	{
		if (PQgetisnull(res, row, j))
		{
			slot->tts_values[j] = (Datum) 0;
			slot->tts_isnull[j] = true;
		}
		else
		{
			slot->tts_isnull[j] = false;
			slot->tts_values[j] = InputFunctionCall(&s->infuncs[j], PQgetvalue(res, row, j),
													s->typioparams[j], s->typmods[j]);
		}
	}
	MemoryContextSwitchTo(oldcxt);
	ExecStoreVirtualTuple(slot);
	if (s->css.ss.ps.state->es_instrument)
		count_group_row(s, f->group_id);
	return true;
}

static TupleTableSlot *
fs_exec_scan(CustomScanState *node)
{
	KsFragScanState *s = (KsFragScanState *) node;
	ExprContext *econtext = node->ss.ps.ps_ExprContext;
	TupleTableSlot *scan = node->ss.ss_ScanTupleSlot;

	if (!s->started)
		fs_start(s);

	for (;;)
	{
		CHECK_FOR_INTERRUPTS();
		ResetExprContext(econtext);

		if (!fs_next_row(s, scan, econtext))
			return NULL;
		econtext->ecxt_scantuple = scan;
		if (node->ss.ps.qual != NULL && !ExecQual(node->ss.ps.qual, econtext))
		{
			InstrCountFiltered1(node, 1);
			continue;
		}
		if (node->ss.ps.ps_ProjInfo)
			return ExecProject(node->ss.ps.ps_ProjInfo);
		return scan;
	}
}

static void
fs_stop(KsFragScanState *s)
{
	kingres_shard_fragset_stop(&s->fs, true);
	fs_local_close(s);
	s->started = false;
}

static void
fs_end_scan(CustomScanState *node)
{
	fs_stop((KsFragScanState *) node);
}

static void
fs_rescan(CustomScanState *node)
{
	fs_stop((KsFragScanState *) node);
}

static void
flatten_sql(const char *sql, StringInfo out)
{
	const char *p;
	bool		pending_space = false;

	for (p = sql; *p; p++)
	{
		if (*p == '\n' || *p == '\r' || *p == '\t' || *p == ' ')
		{
			pending_space = (out->len > 0);
			continue;
		}
		if (pending_space)
			appendStringInfoChar(out, ' ');
		pending_space = false;
		appendStringInfoChar(out, *p);
	}
}

static void
fs_explain_scan(CustomScanState *node, List *ancestors, ExplainState *es)
{
	KsFragScanState *s = (KsFragScanState *) node;
	const KsCatalog *cat = kingres_shard_catalog();
	const KsColocation *coloc = kingres_shard_lookup_colocation(s->colocation_id);
	StringInfoData flat;
	StringInfoData groups;
	bool		local = false;
	bool		pruned = false;
	int			nremote_all = 0;
	int			nremote = 0;
	bool		local_possible = false;
	List	   *frags = NIL;

	if (coloc != NULL)
	{
		int			b;

		frags = kingres_shard_frag_choose(coloc, cat, s->prune, node->ss.ps.state->es_param_list_info,
										  &local, &nremote_all, &pruned);
		nremote = list_length(frags);
		for (b = 0; b < kingres_shard_coloc_nbuckets(coloc); b++)
			if (cat->local_group_id >= 0 &&
				kingres_shard_bucket_group(coloc, kingres_shard_coloc_bucket(coloc, b)) == cat->local_group_id)
				local_possible = true;
	}
	ExplainPropertyText("Pushed", s->kind, es);
	initStringInfo(&groups);
	appendStringInfo(&groups, "%d of %d (", nremote + (local ? 1 : 0), nremote_all + (local_possible ? 1 : 0));
	if (local && nremote > 0)
		appendStringInfo(&groups, "local + %d remote", nremote);
	else if (local)
		appendStringInfoString(&groups, "local");
	else if (nremote > 0)
		appendStringInfo(&groups, "%d remote", nremote);
	else
		appendStringInfoString(&groups, "none");
	if (pruned && s->prune != NIL)
	{
		const KsDistTable *dt = kingres_shard_lookup_dist_table(((KsFragPrune *) linitial(s->prune))->relid);

		if (dt != NULL)
			appendStringInfo(&groups, "; pruned by %s", NameStr(dt->dist_column));
	}
	appendStringInfoChar(&groups, ')');
	ExplainPropertyText("Groups", groups.data, es);

	initStringInfo(&flat);
	flatten_sql(s->sql, &flat);
	ExplainPropertyText("Remote SQL", flat.data, es);

	if (es->analyze && s->group_rows != NIL)
	{
		StringInfoData rows;
		ListCell   *lc;
		bool		first = true;

		initStringInfo(&rows);
		foreach(lc, s->group_rows)
		{
			FsGroupRows *g = (FsGroupRows *) lfirst(lc);

			if (g->group_id < 0)
				appendStringInfo(&rows, "%slocal=" INT64_FORMAT, first ? "" : ", ", g->rows);
			else
				appendStringInfo(&rows, "%sgroup %d=" INT64_FORMAT, first ? "" : ", ", g->group_id, g->rows);
			first = false;
		}
		ExplainPropertyText("Rows from groups", rows.data, es);
	}
}
