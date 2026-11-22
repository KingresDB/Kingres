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
#include "access/table.h"
#include "catalog/pg_index.h"
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
#include "optimizer/clauses.h"
#include "optimizer/cost.h"
#include "optimizer/optimizer.h"
#include "optimizer/pathnode.h"
#include "optimizer/planner.h"
#include "optimizer/prep.h"
#include "optimizer/tlist.h"
#include "optimizer/paths.h"
#include "parser/parsetree.h"
#include "utils/fmgroids.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/portal.h"
#include "utils/rel.h"
#include "common/int.h"
#include "utils/sortsupport.h"
#include "utils/ruleutils.h"
#include "utils/selfuncs.h"
#include "utils/snapmgr.h"
#include "utils/syscache.h"
#include "utils/timestamp.h"
#include "utils/wait_classes.h"

#include "catalog.h"
#include "fanout.h"
#include "fragment_scan.h"
#include "repartition.h"
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
	if (!f->first_seen && f->sent_at != 0)
	{
		f->first_seen = true;
		f->first_ms = (double) (GetCurrentTimestamp() - f->sent_at) / 1000.0;
	}
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
	if (f->sent_at == 0)
		f->sent_at = GetCurrentTimestamp();
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
	ask = kingres_shard_probe_before_sql(st->colocation_id, st->ntables, st->relids, st->ext);
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
		kingres_shard_probe_read_before(f->kc, res, st->ntables, st->plan_placement, f->identity, st->ext);
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
		if (!PQsendQueryParams(f->kc->conn, decl.data, st->nparams, st->ptypes,
							   st->group_values != NULL ? st->group_values(st->arg, f->group_id) : st->pvalues, NULL, NULL, 0))
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
	{
		/* a point of the tests: the map of this node changes after its own cursor is open and the groups were asked again (t/174) */
		if (kingres_shard_fault_spec()[0] != '\0')
			kingres_shard_fault_point(psprintf("%sbefore_own_check", st->fp_declared_prefix));
		st->after_check(st->arg);
	}
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
/* The first FETCH to every group, so that the groups answer at the same time. */
void
kingres_shard_fragset_prime(KsFragSet *fs)
{
	ListCell   *lc;

	foreach(lc, fs->frags)
	{
		KsFrag	   *c = (KsFrag *) lfirst(lc);

		if (c->res == NULL && !c->in_flight && !c->exhausted)
			frag_send_fetch(c);
	}
}

/* The next row of one group: its batch, or the next batch that it has to answer for. */
bool
kingres_shard_fragset_next_of(KsFragSet *fs, KsFrag *f, PGresult **res_out, int *row_out)
{
	for (;;)
	{
		frag_check_failed(f);
		if (f->res != NULL && f->next_row < f->nrows)
		{
			*res_out = f->res;
			*row_out = f->next_row++;
			return true;
		}
		if (f->res != NULL)
		{
			PQclear(f->res);
			f->res = NULL;
			f->nrows = 0;
		}
		if (!f->in_flight)
		{
			if (f->exhausted)
				return false;
			frag_send_fetch(f);
		}
		frag_fault(fs, "wait_batch");
		frag_collect(f, false);
	}
}

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
#define FS_PRIV_PARTIAL		9	/* Integer: 1 for a partial aggregation (the groups return partial states) */
#define FS_PRIV_EXT			10	/* String: the extensions of the text ("name:version,..."), empty for none */
#define FS_PRIV_SORT		11	/* List of List(column, sort operator, collation, nulls first): the groups sort, this node merges */
#define FS_PRIV_LIMIT		12	/* Integer: the number of the parameter that holds count + offset of a LIMIT computed at the start, 0 for none */
#define FS_PRIV_REPART		13	/* List (J1): NIL, or (List of Integer child columns shipped, Integer position of the key among them) */

/* What a custom path of the fragment scan is: the whole statement, a partial aggregation (design 7), the rows of a join (design 9) */
#define FS_PATH_WHOLE		0
#define FS_PATH_PARTIAL		1
#define FS_PATH_JOIN		2
#define FS_PATH_REPART		3		/* J1: the join with one shipped relation (repartition.h) */
#define FS_PATH_REPART_PARTIAL	4	/* J2: a partial aggregation above the repartitioned join */

/* The rows one group returned, for EXPLAIN ANALYZE. */
typedef struct FsGroupRows
{
	int			group_id;		/* -1: this node's own part */
	int64		rows;
} FsGroupRows;

/* One stream of the ordered merge: a group, or this node's own part. */
typedef struct FsMergeSrc
{
	KsFrag	   *frag;			/* NULL: this node's own part */
	int			gid;			/* the group of the stream: the tie-break of the merge (design 8, O3) */
	bool		loaded;			/* the current row is in vals */
	Datum	   *vals;
	bool	   *nulls;
	MemoryContext cxt;
} FsMergeSrc;

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
	bool		partial;
	char	   *ext;
	int			limit_param;
	ExprState  *limit_cnt;
	ExprState  *limit_off;
	bool		limit_null;
	int64		limit_value;
	int			nsort;			/* the keys of the merge; 0: the streams are read as they come */
	int		   *sortcol;
	SortSupportData *ssup;
	FsMergeSrc *msrc;
	int			nmsrc;
	int			mlast;			/* the stream that the last row came from, or -1 */
	bool		merge_started;
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
	TimestampTz local_sent;		/* EXPLAIN ANALYZE: the first fetch of this node's own part, and what it took */
	double		local_first_ms;
	bool		local_first_seen;
	/* J1: the relation that is shipped */
	List	   *rp_cols;		/* of int: the columns of the child's output that are shipped, in the order of the arrays */
	int			rp_keypos;		/* the position of the join key among them */
	KsRepartRows *rp_rows;
	MemoryContext rp_cxt;
	bool		rp_ran;			/* the child was run once (a rescan has to rescan it) */
	const char **rp_base;		/* the values of the statement's parameters, while the groups are being started */
	int			rp_nbase;
	Oid			rp_relid;		/* EXPLAIN: the shipped relation, its key column and the rows estimated */
	int			rp_keyatt;
	int			rp_estrows;
} KsFragScanState;

static create_upper_paths_hook_type prev_create_upper_paths_hook = NULL;
static set_join_pathlist_hook_type prev_set_join_pathlist_hook = NULL;
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
static void fs_partial_paths(PlannerInfo *root, RelOptInfo *input_rel, RelOptInfo *output_rel,
							 GroupPathExtraData *extra);
static void fs_join_paths(PlannerInfo *root, RelOptInfo *joinrel, RelOptInfo *outerrel, RelOptInfo *innerrel,
						   JoinType jointype, JoinPathExtraData *extra);
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

bool
kingres_shard_is_fragment_scan_plan(Plan *plan)
{
	return plan != NULL && IsA(plan, CustomScan) && ((CustomScan *) plan)->methods == &fs_scan_methods;
}

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
	prev_set_join_pathlist_hook = set_join_pathlist_hook;
	set_join_pathlist_hook = fs_join_paths;
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

/*
 * Does a unique index of the table (immediate, valid, without a predicate or an expression) lie within the columns
 * the query groups or removes duplicates by?  Then every group of rows holds one row and the query returns as many
 * rows as it reads whatever the statistics say: the estimates of the planner for the number of groups come from the
 * rows of this node and are not to be trusted here.
 */
static bool
fs_unique_key_covered(Oid relid, Bitmapset *attnos)
{
	Relation	rel = table_open(relid, NoLock);
	List	   *il = RelationGetIndexList(rel);
	ListCell   *lc;
	bool		covered = false;

	foreach(lc, il)
	{
		HeapTuple	tup = SearchSysCache1(INDEXRELID, ObjectIdGetDatum(lfirst_oid(lc)));
		Form_pg_index ix;
		int			k;
		bool		all = true;

		if (!HeapTupleIsValid(tup))
			continue;
		ix = (Form_pg_index) GETSTRUCT(tup);
		if (!ix->indisunique || !ix->indimmediate || !ix->indisvalid || ix->indnkeyatts < 1 ||
			!heap_attisnull(tup, Anum_pg_index_indpred, NULL) ||
			!heap_attisnull(tup, Anum_pg_index_indexprs, NULL))
		{
			ReleaseSysCache(tup);
			continue;
		}
		for (k = 0; k < ix->indnkeyatts; k++)
		{
			int			attno = ix->indkey.values[k];

			if (attno <= 0 || !bms_is_member(attno, attnos))
			{
				all = false;
				break;
			}
		}
		ReleaseSysCache(tup);
		if (all)
		{
			covered = true;
			break;
		}
	}
	list_free(il);
	table_close(rel, NoLock);
	return covered;
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
	if (pi == NULL || parse != pi->parse || root->query_level != 1)
		return;
	if (IsParallelWorker())
		return;
	if (!pi->closed)
	{
		/* a partial aggregation is one more way to make the grouped relation (design 7.4) */
		if (pi->partial && stage == UPPERREL_GROUP_AGG)
			fs_partial_paths(root, input_rel, output_rel, (GroupPathExtraData *) extra);
		return;
	}
	if (stage != UPPERREL_FINAL)
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
	/*
	 * No gain (review of step 6, measurement): one distributed table, and the result has about as many rows
	 * as the table gives after its restrictions (a grouping or DISTINCT by a unique key): the groups would
	 * return the rows that the scan returns, and the scan already ships only the restricted ones.
	 */
	if (list_length(pi->dist_relids) == 1 && pi->ndist == 1)
	{
		int			i;
		double		in_rows = 0;
		Index		dist_rti = 0;

		for (i = 1; i < root->simple_rel_array_size; i++)
		{
			RelOptInfo *r = root->simple_rel_array[i];

			if (r != NULL && r->reloptkind == RELOPT_BASEREL && r->rtekind == RTE_RELATION &&
				list_member_oid(pi->dist_relids, root->simple_rte_array[i]->relid))
			{
				in_rows = Max(in_rows, r->rows);
				dist_rti = (Index) i;
			}
		}
		/* a grouping or DISTINCT by the columns of a unique index returns every row it reads, whatever the estimates say */
		if (dist_rti > 0 && (parse->groupClause != NIL || parse->distinctClause != NIL))
		{
			List	   *clauses = parse->groupClause != NIL ? parse->groupClause : parse->distinctClause;
			Bitmapset  *attnos = NULL;
			ListCell   *lcg;

			foreach(lcg, clauses)
			{
				Node	   *e = get_sortgroupclause_expr(lfirst_node(SortGroupClause, lcg), parse->targetList);

				while (e != NULL && IsA(e, RelabelType))
					e = (Node *) ((RelabelType *) e)->arg;
				if (e != NULL && IsA(e, Var) && ((Var *) e)->varno == dist_rti && ((Var *) e)->varlevelsup == 0)
					attnos = bms_add_member(attnos, ((Var *) e)->varattno);
			}
			if (attnos != NULL && fs_unique_key_covered(root->simple_rte_array[dist_rti]->relid, attnos))
				return;
		}
		ereport(DEBUG1, (errmsg_internal("kingres_shard: fragment scan gain check: the table gives %.0f rows, the query returns %.0f", in_rows, base->rows)));
		if (in_rows > 0 && base->rows >= 0.9 * in_rows)
			return;
	}
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
	if (pi->sort != NIL)
		cpath->path.pathkeys = root->sort_pathkeys;
	else if (root->sort_pathkeys != NIL)
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

/*
 * What a path of a repartitioned join carries to its plan: 0 the child columns shipped, 1 the position of the key among them, 2 the
 * colocation group, 3 its placement, 4 the anchored tables, 5 the shipped relation, 6 its key column, 7 the estimated rows (for EXPLAIN).
 */
static List *
fs_rpinfo(List *child_cols, int keypos, const KsRepartCand *cand, List *dist, RelOptInfo *rrel)
{
	List	   *l = list_make5(child_cols, makeInteger(keypos), makeInteger(cand->colocation_id),
								makeString(psprintf(INT64_FORMAT, cand->place)), dist);

	l = lappend(l, makeInteger((int) cand->routed_relid));
	l = lappend(l, makeInteger((int) cand->key_attno));
	return lappend(l, makeInteger((int) Min(clamp_row_est(rrel->rows), (double) PG_INT32_MAX)));
}

/*
 * The pruning that holds for a repartitioned join: only the anchored tables say which buckets the groups read; a restriction on the shipped table
 * says nothing about the buckets of the others.
 */
static List *
fs_repart_prune(PlannerInfo *root, const KsRepartCand *cand)
{
	List	   *prune = fs_prune_items(root);
	List	   *keep = NIL;
	ListCell   *lc;

	if (list_member_oid(cand->dist_relids, cand->routed_relid))
		return NIL;
	foreach(lc, prune)
		if (((KsFragPrune *) lfirst(lc))->relid != cand->routed_relid)
			keep = lappend(keep, lfirst(lc));
	return keep;
}

/*
 * Does the HAVING clause of the statement as written have a condition that the planner moves into WHERE?  subquery_planner moves every
 * conjunct of HAVING that has no aggregate, no volatile function and no sub-select (it filters rows before the grouping, as the
 * grouping columns are all it can name).  The planner's own final aggregation then no longer has it, and the text of the groups, which is
 * made from the statement before the planner ran, has neither it nor a copy of it in WHERE: the clause would be applied by nobody
 * (review P2E-01).  The partial aggregation is not used for such a statement and the scan answers.  A constant TRUE is moved and
 * dropped; it changes nothing.  What stays in HAVING (an aggregate, a volatile function, a sub-select, also in an OR with the others) is
 * applied by the final aggregation of this node, as before.
 */
static bool
having_has_moved_conjunct(Node *qual)
{
	if (qual == NULL)
		return false;
	if (IsA(qual, BoolExpr) && ((BoolExpr *) qual)->boolop == AND_EXPR)
	{
		ListCell   *lc;

		foreach(lc, ((BoolExpr *) qual)->args)
			if (having_has_moved_conjunct((Node *) lfirst(lc)))
				return true;
		return false;
	}
	if (IsA(qual, Const) && !((Const *) qual)->constisnull && DatumGetBool(((Const *) qual)->constvalue))
		return false;
	return !contain_agg_clause(qual) && !contain_volatile_functions(qual) && !contain_subplans(qual);
}

/*
 * fs_partial_paths
 *
 * Design 7.4.  At the grouped relation of a statement that pushdown_prepare found eligible for a partial aggregation:
 *
 *  1. the partial target, made as make_partial_grouping_target() of the planner makes it (that function is static):
 *     the grouping columns, then every Var and Aggref that the other columns and HAVING need, each Aggref marked as the
 *     first step of a two-step aggregation (it returns the transition state, bytea for an internal one);
 *  2. the text every group runs: the query with the select list of the unmarked partial target, the grouping kept,
 *     HAVING, ORDER BY and LIMIT left to this node;  the origin parses it once more with the rule of the groups;
 *  3. a custom path that returns the partial rows of the groups, and above it the planner's own final aggregation
 *     (AGGSPLIT_FINAL_DESERIAL: it deserializes and combines the states, applies HAVING, runs the final functions).
 *
 * Only when the planner itself finds the aggregation splittable (GROUPING_CAN_PARTIAL_AGG: no DISTINCT or ORDER BY inside
 * a call, a combine function, serialization of internal states, send and receive functions where array_agg needs them).
 */
static void fs_partial_paths_one(PlannerInfo *root, RelOptInfo *input_rel, RelOptInfo *output_rel, GroupPathExtraData *extra,
								   const KsRepartCand *cand);

static void
fs_partial_paths_one(PlannerInfo *root, RelOptInfo *input_rel, RelOptInfo *output_rel, GroupPathExtraData *extra,
					 const KsRepartCand *cand)
{
	KsPushdownInfo *pi = current_info;
	Query	   *parse = root->parse;
	PathTarget *gtarget = output_rel->reltarget;
	PathTarget *ptarget;
	List	   *non_group = NIL;
	List	   *non_group_exprs;
	List	   *unmarked;
	Path	   *base = NULL;
	Path	   *cpath_p;
	CustomPath *cpath;
	AggClauseCosts final_costs;
	Query	   *ship;
	List	   *tlist = NIL;
	KsParamRenumber prn = {NIL};
	const char *why = NULL;
	char	   *body;
	char	   *ext = NULL;
	int			nest;
	int			resno = 1;
	int			i;
	ListCell   *lc;
	const KsCatalog *cat;
	const KsColocation *coloc;
	List	   *prune;
	List	   *frags;
	bool		local = false;
	int			nremote;
	int			ntotal;
	double		startup;
	double		total;
	double		ngroups;
	double		prows;
	List	   *info;
	List	   *pids = NIL;
	RelOptInfo *rrel = NULL;
	Path	   *child = NULL;
	List	   *child_cols = NIL;
	int			keypos = 0;

#define PA_NO(msg) do { ereport(DEBUG1, (errmsg_internal("kingres_shard: no partial aggregation: %s", (msg)))); if (cand != NULL) kingres_shard_repart_note(msg); return; } while (0)

	if (!kingres_shard_enable_partial_aggregation)
		return;
	if (!(extra->flags & GROUPING_CAN_PARTIAL_AGG))
		PA_NO("the planner does not find the aggregation splittable");
	if (parse->groupingSets != NIL || gtarget == NULL)
		PA_NO("grouping sets");
	if (having_has_moved_conjunct(pi->ship->havingQual))
		PA_NO("its HAVING has a condition without an aggregate, which the planner moves into WHERE");

	/* 1. the partial target */
	ptarget = create_empty_pathtarget();
	i = 0;
	foreach(lc, gtarget->exprs)
	{
		Expr	   *expr = (Expr *) lfirst(lc);
		Index		sgref = get_pathtarget_sortgroupref(gtarget, i);

		if (sgref && root->processed_groupClause &&
			get_sortgroupref_clause_noerr(sgref, root->processed_groupClause) != NULL)
			add_column_to_pathtarget(ptarget, expr, sgref);
		else
			non_group = lappend(non_group, expr);
		i++;
	}
	if (extra->havingQual != NULL)
		non_group = lappend(non_group, extra->havingQual);
	non_group_exprs = pull_var_clause((Node *) non_group,
									  PVC_INCLUDE_AGGREGATES | PVC_RECURSE_WINDOWFUNCS | PVC_INCLUDE_PLACEHOLDERS);
	add_new_columns_to_pathtarget(ptarget, non_group_exprs);
	if (ptarget->exprs == NIL)
		PA_NO("the partial target is empty");
	unmarked = list_copy(ptarget->exprs);
	foreach(lc, ptarget->exprs)
	{
		Expr	   *expr = (Expr *) lfirst(lc);

		if (IsA(expr, Aggref))
		{
			Aggref	   *newagg = (Aggref *) palloc(sizeof(Aggref));

			memcpy(newagg, expr, sizeof(Aggref));
			mark_partial_aggref(newagg, AGGSPLIT_INITIAL_SERIAL);
			lfirst(lc) = newagg;
		}
	}
	set_pathtarget_cost_width(root, ptarget);

	/* 2. the text */
	ship = copyObject(pi->ship);
	i = 0;
	foreach(lc, unmarked)
	{
		TargetEntry *te = makeTargetEntry((Expr *) copyObject(lfirst(lc)), (AttrNumber) resno++, NULL, false);

		te->ressortgroupref = get_pathtarget_sortgroupref(ptarget, i);
		tlist = lappend(tlist, te);
		i++;
	}
	/* a column that only groups is not returned, and the text still groups by it */
	foreach(lc, ship->groupClause)
	{
		SortGroupClause *sgc = lfirst_node(SortGroupClause, lc);
		bool		found = false;
		ListCell   *lt;

		foreach(lt, tlist)
			if (lfirst_node(TargetEntry, lt)->ressortgroupref == sgc->tleSortGroupRef)
				found = true;
		if (!found)
		{
			Node	   *ge = get_sortgroupclause_expr(sgc, pi->ship->targetList);
			TargetEntry *te = makeTargetEntry((Expr *) copyObject(ge), (AttrNumber) resno++, NULL, true);

			te->ressortgroupref = sgc->tleSortGroupRef;
			tlist = lappend(tlist, te);
		}
	}
	ship->targetList = tlist;
	ship->havingQual = NULL;
	ship->distinctClause = NIL;
	ship->hasDistinctOn = false;
	ship->windowClause = NIL;
	ship->hasWindowFuncs = false;
	ship->sortClause = NIL;
	ship->limitOffset = NULL;
	ship->limitCount = NULL;
	ship = query_tree_mutator(ship, kingres_shard_renumber_params, &prn, 0);
	if (cand != NULL)
	{
		/* J2: the join below the aggregation is the repartitioned one; one relation is read by its own scan and shipped */
		if (cand->routed_rti >= (Index) root->simple_rel_array_size || (rrel = root->simple_rel_array[cand->routed_rti]) == NULL)
			PA_NO("the shipped relation has no base relation");
		child = rrel->cheapest_total_path;
		if (child == NULL || child->param_info != NULL)
			PA_NO("the shipped relation has no plain path");
		if (!kingres_shard_repart_ship_text(ship, cand, list_length(prn.ids) + 1, rrel->reltarget, &child_cols, &keypos, &why))
			PA_NO(why);
	}
	nest = kingres_shard_set_transmission_modes();
	body = pg_get_querydef(ship, false);
	kingres_shard_reset_transmission_modes(nest);
	if (!kingres_shard_text_accepted(body, ship, true, &why, &ext))
	{
		ereport(DEBUG1, (errmsg_internal("kingres_shard: a partial aggregation is not shipped: the groups would not accept its text: %s", why)));
		return;
	}
	if (cand != NULL && ext != NULL)
		PA_NO("the text uses an extension, which a repartitioned join does not carry yet");
	foreach(lc, prn.ids)
		pids = lappend(pids, makeInteger(lfirst_int(lc)));

	/* 3. the cost, the custom path and the final aggregation */
	foreach(lc, input_rel->pathlist)
	{
		Path	   *c = (Path *) lfirst(lc);

		if (base == NULL || c->total_cost < base->total_cost)
			base = c;
	}
	if (base == NULL)
		PA_NO("the relation has no path");
	cat = kingres_shard_catalog();
	if (!cat->present)
		PA_NO("no catalog");
	coloc = kingres_shard_lookup_colocation(cand != NULL ? cand->colocation_id : pi->colocation_id);
	if (coloc == NULL)
		PA_NO("its colocation group is not known");
	prune = cand != NULL ? fs_repart_prune(root, cand) : fs_prune_items(root);
	frags = kingres_shard_frag_choose(coloc, cat, prune, root->glob->boundParams, &local, NULL, NULL);
	nremote = list_length(frags);
	ntotal = nremote + (local ? 1 : 0);
	if (ntotal == 0)
		PA_NO("no group to ask");

	if (parse->groupClause == NIL)
		ngroups = 1;
	else
		ngroups = estimate_num_groups(root,
									  get_sortgrouplist_exprs(root->processed_groupClause, root->processed_tlist),
									  input_rel->rows, NULL, NULL);
	prows = clamp_row_est(Min(ngroups * ntotal, Max(input_rel->rows, 1.0)));
	startup = 100.0 * nremote;
	total = startup + Max(base->total_cost - startup, 0.0) / ntotal +
		prows * (cpu_tuple_cost * 2.0 + 0.02) * nremote / ntotal;
	if (cand != NULL)
	{
		/* the groups do the join of the other tables, divided among them; this node reads the shipped table and sends its rows once */
		double		moved = clamp_row_est(rrel->rows) * (double) Max(rrel->reltarget->width, 8);

		if (kingres_shard_enable_repartition_join != KS_REPART_FORCE &&
			moved > (double) kingres_shard_repartition_max_kb * 1024.0 / 2.0)
			PA_NO("the shipped side is estimated above half of repartition_max_bytes");
		startup = child->total_cost + 100.0 * nremote;
		total = startup + Max(base->total_cost - child->total_cost, 0.0) / ntotal + clamp_row_est(rrel->rows) * (cpu_tuple_cost * 4.0 + 0.04) +
			prows * (cpu_tuple_cost * 2.0 + 0.02) * nremote / ntotal;
		if (kingres_shard_enable_repartition_join == KS_REPART_FORCE)
		{
			startup = 0.0;
			total = 0.005;
		}
	}

	cpath = makeNode(CustomPath);
	cpath->path.pathtype = T_CustomScan;
	cpath->path.parent = output_rel;
	cpath->path.pathtarget = ptarget;
	cpath->path.param_info = NULL;
	cpath->path.parallel_aware = false;
	cpath->path.parallel_safe = false;
	cpath->path.parallel_workers = 0;
	cpath->path.pathkeys = NIL;
	cpath->path.rows = prows;
	cpath->path.disabled_nodes = 0;
	cpath->path.startup_cost = startup;
	cpath->path.total_cost = total;
	cpath->flags = 0;
	cpath->custom_paths = cand != NULL ? list_make1(child) : NIL;
	cpath->custom_restrictinfo = NIL;
	info = list_make3(makeString(body), pids, makeString(ext != NULL ? ext : pstrdup("")));
	if (cand != NULL)
	{
		List	   *dist = NIL;
		ListCell   *ld;

		foreach(ld, cand->dist_relids)
			dist = lappend(dist, makeInteger((int) lfirst_oid(ld)));
		cpath->custom_private = list_make4(prune, info, makeInteger(FS_PATH_REPART_PARTIAL),
										   fs_rpinfo(child_cols, keypos, cand, dist, rrel));
	}
	else
		cpath->custom_private = list_make3(prune, info, makeInteger(FS_PATH_PARTIAL));
	cpath->methods = &fs_path_methods;
	cpath_p = (Path *) cpath;
	if (cand != NULL)
		kingres_shard_repart_offered();

	if (extra->partial_costs_set)
		final_costs = extra->agg_final_costs;
	else
	{
		MemSet(&final_costs, 0, sizeof(final_costs));
		get_agg_clause_costs(root, AGGSPLIT_FINAL_DESERIAL, &final_costs);
	}
	if (parse->groupClause == NIL)
		add_path(output_rel, (Path *) create_agg_path(root, output_rel, cpath_p, output_rel->reltarget,
													  AGG_PLAIN, AGGSPLIT_FINAL_DESERIAL, NIL,
													  (List *) extra->havingQual, &final_costs, 1));
	else
	{
		if (extra->flags & GROUPING_CAN_USE_HASH)
			add_path(output_rel, (Path *) create_agg_path(root, output_rel, cpath_p, output_rel->reltarget,
														  AGG_HASHED, AGGSPLIT_FINAL_DESERIAL,
														  root->processed_groupClause,
														  (List *) extra->havingQual, &final_costs, ngroups));
		if ((extra->flags & GROUPING_CAN_USE_SORT) && root->group_pathkeys != NIL)
		{
			Path	   *sorted = (Path *) create_sort_path(root, output_rel, cpath_p, root->group_pathkeys, -1.0);

			add_path(output_rel, (Path *) create_agg_path(root, output_rel, sorted, output_rel->reltarget,
														  AGG_SORTED, AGGSPLIT_FINAL_DESERIAL,
														  root->processed_groupClause,
														  (List *) extra->havingQual, &final_costs, ngroups));
		}
	}
}

static void
fs_partial_paths(PlannerInfo *root, RelOptInfo *input_rel, RelOptInfo *output_rel, GroupPathExtraData *extra)
{
	KsPushdownInfo *pi = current_info;

	if (pi->repart != NIL && kingres_shard_enable_repartition_join != KS_REPART_OFF)
	{
		ListCell   *lc;

		foreach(lc, pi->repart)
			fs_partial_paths_one(root, input_rel, output_rel, extra, (const KsRepartCand *) lfirst(lc));
	}
	if (pi->repart == NIL || strcmp(pi->kind, "repartitioned join") != 0)
		fs_partial_paths_one(root, input_rel, output_rel, extra, NULL);
}

static void fs_repart_join_paths(PlannerInfo *root, RelOptInfo *joinrel, RelOptInfo *outerrel, RelOptInfo *innerrel);

/*
 * fs_repart_join_paths (J1)
 *
 * The statement joins distributed tables on columns that are not their distribution key, so that one relation, routed by its join
 * column, aligns the others.  For each candidate (repartition.h) a path is made: the text is the statement's FROM and WHERE with the
 * columns that the planner needs above the join, the routed relation replaced by a sub-select over unnest() of array parameters; the
 * relation is read by its own scan (the child of the path), its rows are hashed here to the group that owns the bucket of their key,
 * and every group joins its rows with its own buckets of the other tables.
 */
static void
fs_repart_join_paths(PlannerInfo *root, RelOptInfo *joinrel, RelOptInfo *outerrel, RelOptInfo *innerrel)
{
	KsPushdownInfo *pi = current_info;
	ListCell   *lcand;
	const KsCatalog *cat = kingres_shard_catalog();

	if (kingres_shard_enable_repartition_join == KS_REPART_OFF || !cat->present)
		return;
	if (joinrel->reltarget == NULL || joinrel->reltarget->exprs == NIL)
		return;
	if (having_has_moved_conjunct(pi->ship->havingQual))
		return;

	foreach(lcand, pi->repart)
	{
		KsRepartCand *cand = (KsRepartCand *) lfirst(lcand);
		Query	   *jq;
		List	   *tl = NIL;
		KsParamRenumber prn = {NIL};
		const char *why = NULL;
		char	   *body;
		char	   *ext = NULL;
		int			nest;
		int			resno = 1;
		ListCell   *lc;
		RelOptInfo *rrel;
		Path	   *child;
		List	   *child_cols = NIL;
		int			keypos = 0;
		List	   *pids = NIL;
		List	   *prune;
		List	   *frags;
		bool		local = false;
		const KsColocation *coloc;
		int			nremote;
		int			ntotal;
		double		startup;
		double		total;
		double		rows;
		double		base_total = 0;
		double		moved_bytes;
		CustomPath *cpath;
		bool		bad = false;
		List	   *dist = NIL;
		List	   *rpinfo;

#define RJ_NO(msg) { ereport(DEBUG1, (errmsg_internal("kingres_shard: no repartitioned join: %s", (msg)))); kingres_shard_repart_note(msg); continue; }

		if (cand->routed_rti >= (Index) root->simple_rel_array_size || (rrel = root->simple_rel_array[cand->routed_rti]) == NULL)
			RJ_NO("the shipped relation has no base relation");
		child = rrel->cheapest_total_path;
		if (child == NULL || child->param_info != NULL)
			RJ_NO("the shipped relation has no plain path");
		foreach(lc, joinrel->reltarget->exprs)
		{
			Node	   *e = (Node *) lfirst(lc);

			if (!IsA(e, Var) || ((Var *) e)->varlevelsup != 0 || ((Var *) e)->varattno <= 0 ||
				((Var *) e)->varno > (Index) list_length(root->parse->rtable))
			{
				bad = true;
				break;
			}
			tl = lappend(tl, makeTargetEntry((Expr *) copyObject(e), (AttrNumber) resno++, NULL, false));
		}
		if (bad)
			RJ_NO("a column above the join is not a column of a table");

		jq = copyObject(pi->ship);
		jq->targetList = tl;
		jq->groupClause = NIL;
		jq->groupingSets = NIL;
		jq->havingQual = NULL;
		jq->distinctClause = NIL;
		jq->hasDistinctOn = false;
		jq->windowClause = NIL;
		jq->hasWindowFuncs = false;
		jq->hasAggs = false;
		jq->sortClause = NIL;
		jq->limitCount = NULL;
		jq->limitOffset = NULL;
		jq->limitOption = LIMIT_OPTION_COUNT;
		jq->rowMarks = NIL;
		jq = query_tree_mutator(jq, kingres_shard_renumber_params, &prn, 0);
		if (!kingres_shard_repart_ship_text(jq, cand, list_length(prn.ids) + 1, rrel->reltarget, &child_cols, &keypos, &why))
			RJ_NO(why);
		nest = kingres_shard_set_transmission_modes();
		body = pg_get_querydef(jq, false);
		kingres_shard_reset_transmission_modes(nest);
		if (!kingres_shard_text_accepted(body, jq, false, &why, &ext))
			RJ_NO(why);
		if (ext != NULL)
			RJ_NO("the text uses an extension, which a repartitioned join does not carry yet");
		foreach(lc, prn.ids)
			pids = lappend(pids, makeInteger(lfirst_int(lc)));

		coloc = kingres_shard_lookup_colocation(cand->colocation_id);
		if (coloc == NULL)
			RJ_NO("its colocation group is not known");
		/* the pruning of the anchored tables only: a restriction on the shipped table says nothing about the buckets of the others */
		prune = fs_repart_prune(root, cand);
		frags = kingres_shard_frag_choose(coloc, cat, prune, root->glob->boundParams, &local, NULL, NULL);
		nremote = list_length(frags);
		ntotal = nremote + (local ? 1 : 0);
		if (ntotal == 0)
			RJ_NO("no group to ask");

		/* the rows that move: the estimate of the shipped side, twice through this node (read, then sent), against the bound */
		moved_bytes = clamp_row_est(rrel->rows) * (double) Max(rrel->reltarget->width, 8);
		if (kingres_shard_enable_repartition_join != KS_REPART_FORCE &&
			moved_bytes > (double) kingres_shard_repartition_max_kb * 1024.0 / 2.0)
			RJ_NO("the shipped side is estimated above half of repartition_max_bytes");

		foreach(lc, cand->dist_relids)
			dist = lappend(dist, makeInteger((int) lfirst_oid(lc)));
		rpinfo = fs_rpinfo(child_cols, keypos, cand, dist, rrel);

		/*
		 * The groups do what the planner would pay to join the other tables with the shipped rows, divided among them; this node
		 * pays for the scan of the shipped table, for sending its rows once and for the rows of the join.
		 */
		if (outerrel->cheapest_total_path != NULL && !bms_is_member((int) cand->routed_rti, outerrel->relids))
			base_total += outerrel->cheapest_total_path->total_cost;
		if (innerrel->cheapest_total_path != NULL && !bms_is_member((int) cand->routed_rti, innerrel->relids))
			base_total += innerrel->cheapest_total_path->total_cost;
		rows = clamp_row_est(joinrel->rows);
		startup = child->total_cost + 100.0 * nremote;
		total = startup + base_total / ntotal + clamp_row_est(rrel->rows) * (cpu_tuple_cost * 4.0 + 0.04) +
			rows * (cpu_tuple_cost * 2.0 + 0.02) * nremote / ntotal;
		if (kingres_shard_enable_repartition_join == KS_REPART_FORCE)
		{
			startup = 0.0;
			total = 0.01;
		}

		cpath = makeNode(CustomPath);
		cpath->path.pathtype = T_CustomScan;
		cpath->path.parent = joinrel;
		cpath->path.pathtarget = joinrel->reltarget;
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
		cpath->custom_paths = list_make1(child);
		cpath->custom_restrictinfo = NIL;
		cpath->custom_private = list_make4(prune, list_make3(makeString(body), pids, makeString(pstrdup(""))),
										   makeInteger(FS_PATH_REPART), rpinfo);
		cpath->methods = &fs_path_methods;
		kingres_shard_repart_offered();
		add_path(joinrel, (Path *) cpath);
#undef RJ_NO
	}
}

/*
 * fs_join_paths
 *
 * Design 9.  A statement that is not closed as a whole (a window function that is not partitioned by the key, an aggregate that
 * cannot be split) and whose tables are joined on their distribution keys (and reference tables) still has a join that every group
 * can compute on its own rows: the join of the whole level is offered as a fragment of its own.  The text is the statement's FROM and
 * WHERE with the columns that the planner needs above the join (joinrel->reltarget) as the select list, so it is a closed query
 * (a join on the key, no aggregate) and the group's rule for the fragment of a closed query accepts it; this node plans what is above
 * the join (the window, the aggregate, the sort) as for any scan.  When the statement is closed, or has a partial aggregation, the
 * paths of fs_upper_paths and fs_partial_paths do more and win by cost.
 *
 * The hook runs for every pair of relations that make the join relation: one path per relation is made.  A grouped relation (19, eager
 * aggregation) has the same relids and is left alone.
 */
static void
fs_join_paths(PlannerInfo *root, RelOptInfo *joinrel, RelOptInfo *outerrel, RelOptInfo *innerrel,
			  JoinType jointype, JoinPathExtraData *extra)
{
	KsPushdownInfo *pi = current_info;
	Query	   *parse = root->parse;
	Query	   *jq;
	List	   *tl = NIL;
	KsParamRenumber prn = {NIL};
	const char *why = NULL;
	char	   *body;
	char	   *ext = NULL;
	int			nest;
	int			resno = 1;
	ListCell   *lc;
	const KsCatalog *cat;
	const KsColocation *coloc;
	List	   *prune;
	List	   *frags;
	List	   *pids = NIL;
	bool		local = false;
	int			nremote;
	int			ntotal;
	double		startup;
	double		total;
	double		rows;
	double		base_total = 0;
	CustomPath *cpath;

	if (prev_set_join_pathlist_hook)
		prev_set_join_pathlist_hook(root, joinrel, outerrel, innerrel, jointype, extra);
	if (pi == NULL || !pi->join || parse != pi->parse || root->query_level != 1 || IsParallelWorker())
		return;
	if (joinrel->reloptkind != RELOPT_JOINREL || KS_IS_GROUPED_REL(joinrel))
		return;
	/* the join of the whole level, and once */
	if (!bms_is_subset(root->all_baserels, joinrel->relids) || pi->join_done == (void *) joinrel)
		return;
	if (pi->repart != NIL)
	{
		/* a statement that is not a unit of 2b: the paths that ship one relation (repartition.h) */
		pi->join_done = (void *) joinrel;
		fs_repart_join_paths(root, joinrel, outerrel, innerrel);
		/* a 2b join of the same statement is tried too */
		if (strcmp(pi->kind, "join rows (on key)") != 0)
			return;
	}
	if (!kingres_shard_enable_pushdown_join)
		return;
	pi->join_done = (void *) joinrel;

#define JN_NO(msg) do { ereport(DEBUG1, (errmsg_internal("kingres_shard: no join fragment: %s", (msg)))); return; } while (0)

	/* the planner has moved such a condition into WHERE of the join or of a table below it; the text is made from the statement before that */
	if (having_has_moved_conjunct(pi->ship->havingQual))
		JN_NO("its HAVING has a condition without an aggregate, which the planner moves into WHERE");
	if (joinrel->reltarget == NULL || joinrel->reltarget->exprs == NIL)
		JN_NO("the join has no column that is needed above it");
	foreach(lc, joinrel->reltarget->exprs)
	{
		Node	   *e = (Node *) lfirst(lc);

		if (!IsA(e, Var) || ((Var *) e)->varlevelsup != 0 || ((Var *) e)->varattno <= 0 ||
			((Var *) e)->varno > (Index) list_length(parse->rtable))
			JN_NO("a column above the join is not a column of a table");
		tl = lappend(tl, makeTargetEntry((Expr *) copyObject(e), (AttrNumber) resno++, NULL, false));
	}

	/* the text: the FROM and WHERE of the statement, the columns above the join */
	jq = copyObject(pi->ship);
	jq->targetList = tl;
	jq->groupClause = NIL;
	jq->groupingSets = NIL;
	jq->havingQual = NULL;
	jq->distinctClause = NIL;
	jq->hasDistinctOn = false;
	jq->windowClause = NIL;
	jq->hasWindowFuncs = false;
	jq->hasAggs = false;
	jq->sortClause = NIL;
	jq->limitCount = NULL;
	jq->limitOffset = NULL;
	jq->limitOption = LIMIT_OPTION_COUNT;
	jq->rowMarks = NIL;
	jq = query_tree_mutator(jq, kingres_shard_renumber_params, &prn, 0);
	nest = kingres_shard_set_transmission_modes();
	body = pg_get_querydef(jq, false);
	kingres_shard_reset_transmission_modes(nest);
	if (!kingres_shard_text_accepted(body, jq, false, &why, &ext))
		JN_NO(why);
	foreach(lc, prn.ids)
		pids = lappend(pids, makeInteger(lfirst_int(lc)));

	cat = kingres_shard_catalog();
	if (!cat->present)
		JN_NO("no catalog");
	coloc = kingres_shard_lookup_colocation(pi->colocation_id);
	if (coloc == NULL)
		JN_NO("its colocation group is not known");
	prune = fs_prune_items(root);
	frags = kingres_shard_frag_choose(coloc, cat, prune, root->glob->boundParams, &local, NULL, NULL);
	nremote = list_length(frags);
	ntotal = nremote + (local ? 1 : 0);
	if (ntotal == 0)
		JN_NO("no group to ask");

	/*
	 * What the groups do is what the planner would pay to join the two sides, divided among the groups that run at the same
	 * time, plus the transfer of the rows of the join.
	 */
	if (outerrel->cheapest_total_path != NULL)
		base_total += outerrel->cheapest_total_path->total_cost;
	if (innerrel->cheapest_total_path != NULL)
		base_total += innerrel->cheapest_total_path->total_cost;
	rows = clamp_row_est(joinrel->rows);
	startup = 100.0 * nremote;
	total = startup + Max(base_total - startup, 0.0) / ntotal +
		rows * (cpu_tuple_cost * 2.0 + 0.02) * nremote / ntotal;

	cpath = makeNode(CustomPath);
	cpath->path.pathtype = T_CustomScan;
	cpath->path.parent = joinrel;
	cpath->path.pathtarget = joinrel->reltarget;
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
	cpath->custom_private = list_make3(prune, list_make3(makeString(body), pids, makeString(ext != NULL ? ext : pstrdup(""))),
									   makeInteger(FS_PATH_JOIN));
	cpath->methods = &fs_path_methods;
	add_path(joinrel, (Path *) cpath);
#undef JN_NO
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
	const char *sql;
	const char *ext;
	int			path_kind = list_length(best_path->custom_private) > 2 ? intVal(lthird(best_path->custom_private)) : FS_PATH_WHOLE;
	bool		is_rp = (path_kind == FS_PATH_REPART || path_kind == FS_PATH_REPART_PARTIAL);

	List	   *rpinfo = NIL;
	int			rp_coloc = 0;
	const char *rp_place = NULL;

	if (pi == NULL || root->parse != pi->parse)
		elog(ERROR, "a fragment scan was planned for a statement that was not decided");

	if (is_rp)
	{
		rpinfo = (List *) lfourth(best_path->custom_private);
		rp_coloc = intVal(lthird(rpinfo));
		rp_place = strVal(lfourth(rpinfo));
	}
	if (path_kind != FS_PATH_WHOLE)
	{
		/* the path made the text: the select list of the partial target (fs_partial_paths) or the columns of the join (fs_join_paths) */
		List	   *info = (List *) lsecond(best_path->custom_private);

		sql = strVal(linitial(info));
		param_ids = (List *) lsecond(info);
		ext = strVal(lthird(info));
	}
	else
	{
		sql = pi->sql;
		ext = pi->ext;
		foreach(lc, pi->param_ids)
			param_ids = lappend(param_ids, makeInteger(lfirst_int(lc)));
	}
	if (is_rp)
		dist = (List *) list_nth(rpinfo, 4);
	else
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
	cscan->custom_plans = is_rp ? custom_plans : NIL;
	cscan->custom_relids = (path_kind == FS_PATH_JOIN || path_kind == FS_PATH_REPART) ? bms_copy(rel->relids) : bms_copy(root->all_baserels);
	cscan->custom_private = list_make5(makeString(pstrdup(sql)), param_ids,
									   makeInteger(is_rp ? rp_coloc : pi->colocation_id),
									   makeString(is_rp ? pstrdup(rp_place) : psprintf(INT64_FORMAT, pi->place)),
									   makeString(relations_signature(pi->relids)));
	cscan->custom_private = lappend(cscan->custom_private, dist);
	cscan->custom_private = lappend(cscan->custom_private, prune);
	cscan->custom_private = lappend(cscan->custom_private, makeString(pstrdup(path_kind == FS_PATH_REPART_PARTIAL ? "repartitioned join, partial aggregate (one table moved by its join key)" : path_kind == FS_PATH_REPART ? "repartitioned join (one table moved by its join key)" : path_kind == FS_PATH_JOIN ? "join rows (on key)" : path_kind == FS_PATH_PARTIAL ? "partial aggregate" : pi->kind)));
	cscan->custom_private = lappend(cscan->custom_private, makeInteger(path_kind != FS_PATH_WHOLE ? list_length(tlist) : pi->ncols));
	cscan->custom_private = lappend(cscan->custom_private, makeInteger((path_kind == FS_PATH_PARTIAL || path_kind == FS_PATH_REPART_PARTIAL) ? 1 : 0));
	cscan->custom_private = lappend(cscan->custom_private, makeString(ext != NULL ? pstrdup(ext) : pstrdup("")));
	cscan->custom_private = lappend(cscan->custom_private, path_kind != FS_PATH_WHOLE ? NIL : pi->sort);
	cscan->custom_private = lappend(cscan->custom_private, makeInteger(path_kind == FS_PATH_WHOLE ? pi->limit_param : 0));
	cscan->custom_private = lappend(cscan->custom_private, is_rp ? list_make5(linitial(rpinfo), lsecond(rpinfo), list_nth(rpinfo, 5), list_nth(rpinfo, 6), list_nth(rpinfo, 7)) : NIL);
	if (path_kind == FS_PATH_WHOLE && pi->limit_param > 0)
		cscan->custom_exprs = list_make2(pi->limit_cnt, pi->limit_off != NULL ? pi->limit_off : (Node *) makeNullConst(INT8OID, -1, InvalidOid));
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
	s->partial = intVal(list_nth(p, FS_PRIV_PARTIAL)) != 0;
	s->ext = strVal(list_nth(p, FS_PRIV_EXT))[0] != '\0' ? pstrdup(strVal(list_nth(p, FS_PRIV_EXT))) : NULL;
	s->limit_param = intVal(list_nth(p, FS_PRIV_LIMIT));
	if (list_length(p) > FS_PRIV_REPART && list_nth(p, FS_PRIV_REPART) != NIL)
	{
		List	   *rp = (List *) list_nth(p, FS_PRIV_REPART);
		ListCell   *rl;

		foreach(rl, (List *) linitial(rp))
			s->rp_cols = lappend_int(s->rp_cols, lfirst_int(rl));
		s->rp_keypos = intVal(lsecond(rp));
		s->rp_relid = (Oid) intVal(lthird(rp));
		s->rp_keyatt = intVal(lfourth(rp));
		s->rp_estrows = intVal(lfirst(list_nth_cell(rp, 4)));
	}
	{
		ListCell   *kl;
		int			k = 0;

		s->nsort = list_length((List *) list_nth(p, FS_PRIV_SORT));
		s->sortcol = palloc(sizeof(int) * Max(s->nsort, 1));
		s->ssup = palloc0(sizeof(SortSupportData) * Max(s->nsort, 1));
		foreach(kl, (List *) list_nth(p, FS_PRIV_SORT))
		{
			List	   *key = (List *) lfirst(kl);
			SortSupport ssup = &s->ssup[k];

			s->sortcol[k] = intVal(linitial(key));
			ssup->ssup_cxt = CurrentMemoryContext;
			ssup->ssup_collation = (Oid) intVal(lthird(key));
			ssup->ssup_nulls_first = intVal(lfourth(key)) != 0;
			PrepareSortSupportFromOrderingOp((Oid) intVal(lsecond(key)), ssup);
			k++;
		}
	}
	s->mlast = -1;
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
	if (s->rp_cols != NIL)
	{
		ListCell   *cl;

		foreach(cl, ((CustomScan *) node->ss.ps.plan)->custom_plans)
			node->custom_ps = lappend(node->custom_ps, ExecInitNode((Plan *) lfirst(cl), estate, eflags));
		s->rp_cxt = AllocSetContextCreate(estate->es_query_cxt, "kingres_shard repartition rows", ALLOCSET_DEFAULT_SIZES);
	}
	if (s->limit_param > 0)
	{
		List	   *lx = ((CustomScan *) node->ss.ps.plan)->custom_exprs;

		s->limit_cnt = ExecInitExpr((Expr *) linitial(lx), &node->ss.ps);
		s->limit_off = ExecInitExpr((Expr *) lsecond(lx), &node->ss.ps);
	}
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
	int			n = list_length(s->param_ids) + (s->limit_param > 0 ? 1 : 0) + list_length(s->rp_cols);
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
	if (s->limit_param > 0)
	{
		argtypes[i] = INT8OID;
		values[i] = Int64GetDatum(s->limit_value);
		nulls[i] = s->limit_null ? 'n' : ' ';
		i++;
	}
	if (s->rp_cols != NIL)
	{
		int			c;
		int			lg = kingres_shard_catalog()->local_group_id;

		/* the rows of the shipped relation that belong to this node's own group */
		for (c = 0; c < list_length(s->rp_cols); c++)
		{
			argtypes[i] = TEXTARRAYOID;
			values[i] = OidInputFunctionCall(F_ARRAY_IN, (char *) kingres_shard_repart_array(s->rp_rows, lg, c), TEXTOID, -1);
			nulls[i] = ' ';
			i++;
		}
	}

	s->ident_before = MemoryContextAlloc(estate->es_query_cxt,
										 sizeof(int64) * Max(list_length(s->dist_relids), 1));
	local_identities(s, s->ident_before);
	kingres_shard_check_origin_placement(s->colocation_id, s->plan_placement, "starting to read it");

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	nest = kingres_shard_set_transmission_modes();
	portal = kingres_shard_spi_local_part_cursor(s->partial ? psprintf("%s%s", kingres_shard_partial_marker_rp(s->plan_placement, s->ext, s->rp_cols != NIL ? 1 : 0), s->sql) : s->sql,
												 n, argtypes, values, nulls);
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
		if (s->local_sent == 0)
		s->local_sent = GetCurrentTimestamp();
	MemoryContextReset(s->lcxt);
	s->lbatch = NULL;
	s->nl = s->next_l = 0;
	s->has_local = false;
}

/* The parameters of one group: the statement's, then the arrays of its own rows of the shipped relation. */
static const char *const *
fs_group_values(void *arg, int group_id)
{
	KsFragScanState *s = (KsFragScanState *) arg;
	int			k = list_length(s->rp_cols);
	const char **v = palloc0(sizeof(char *) * (s->rp_nbase + k));
	int			c;

	memcpy(v, s->rp_base, sizeof(char *) * s->rp_nbase);
	for (c = 0; c < k; c++)
		v[s->rp_nbase + c] = kingres_shard_repart_array(s->rp_rows, group_id, c);
	return v;
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
	if (s->limit_param > 0)
	{
		ExprContext *ec = s->css.ss.ps.ps_ExprContext;
		bool		n1;
		bool		n2;
		Datum		d1 = ExecEvalExprSwitchContext(s->limit_cnt, ec, &n1);
		Datum		d2 = ExecEvalExprSwitchContext(s->limit_off, ec, &n2);
		int64		cnt = n1 ? 0 : DatumGetInt64(d1);
		int64		off = n2 ? 0 : DatumGetInt64(d2);
		int64		sum = 0;
		const char **v2;
		Oid		   *t2;

		/* no limit (NULL) for LIMIT NULL, a negative number (this node's own Limit node says so) and a sum that does not fit */
		s->limit_null = n1 || cnt < 0 || off < 0 || pg_add_s64_overflow(cnt, off, &sum);
		s->limit_value = s->limit_null ? 0 : sum;
		v2 = palloc0(sizeof(char *) * (nparams + 1));
		t2 = palloc0(sizeof(Oid) * (nparams + 1));
		memcpy(v2, values, sizeof(char *) * nparams);
		memcpy(t2, types, sizeof(Oid) * nparams);
		t2[nparams] = INT8OID;
		v2[nparams] = s->limit_null ? NULL : psprintf(INT64_FORMAT, s->limit_value);
		values = v2;
		types = t2;
		nparams++;
	}
	if (s->rp_cols != NIL)
	{
		List	   *groups = NIL;
		int			k = list_length(s->rp_cols);
		const char **v2;
		Oid		   *t2;
		int			c;

		foreach(lc, s->fs.frags)
			groups = lappend_int(groups, ((KsFrag *) lfirst(lc))->group_id);
		if (local)
			groups = lappend_int(groups, cat->local_group_id);
		if (s->rp_ran)
			ExecReScan((PlanState *) linitial(s->css.custom_ps));
		s->rp_ran = true;
		MemoryContextReset(s->rp_cxt);
		s->rp_rows = kingres_shard_repart_collect((PlanState *) linitial(s->css.custom_ps), coloc, s->rp_cols, s->rp_keypos, groups, s->rp_cxt);
		/* design 4.10: a move that commits here is found by the placement check of the groups (KS001), never a wrong answer */
		kingres_shard_fault_point("rp_after_ship");
		/* the arrays come after the statement's own parameters; their values are the group's own */
		v2 = palloc0(sizeof(char *) * (nparams + k));
		t2 = palloc0(sizeof(Oid) * (nparams + k));
		memcpy(v2, values, sizeof(char *) * nparams);
		memcpy(t2, types, sizeof(Oid) * nparams);
		for (c = 0; c < k; c++)
			t2[nparams + c] = TEXTARRAYOID;
		s->rp_base = v2;
		s->rp_nbase = nparams;
		values = v2;
		types = t2;
		nparams += k;
		i = 0;
	}
	relids = palloc(sizeof(Oid) * Max(nrels, 1));
	i = 0;
	foreach(lc, s->dist_relids)
		relids[i++] = lfirst_oid(lc);

	memset(&st, 0, sizeof(st));
	st.colocation_id = s->colocation_id;
	st.ntables = nrels;
	st.relids = relids;
	st.plan_placement = s->plan_placement;
	st.marker = s->partial ? kingres_shard_partial_marker_rp(s->plan_placement, s->ext, s->rp_cols != NIL ? 1 : 0) :
		s->rp_cols != NIL ? kingres_shard_query_marker_rp(s->plan_placement, s->ext, 1) : kingres_shard_query_marker(s->plan_placement, s->ext);
	if (s->rp_cols != NIL)
		st.group_values = fs_group_values;
	st.ext = s->ext;
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
	if (s->rp_cols != NIL)
		kingres_shard_fault_point("rp_before_fragment");
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
	if (!s->local_first_seen)
	{
		s->local_first_seen = true;
		s->local_first_ms = (double) (GetCurrentTimestamp() - s->local_sent) / 1000.0;
	}
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

/* The values of one row of a group, as input functions make them, in the context of its stream. */
static void
merge_load_remote(KsFragScanState *s, FsMergeSrc *src, PGresult *res, int row)
{
	int			j;
	MemoryContext oldcxt;

	if (PQnfields(res) != s->ncols)
		ereport(ERROR,
				(errcode(ERRCODE_DATA_EXCEPTION),
				 errmsg("node %s:%d returned %d columns, expected %d",
						src->frag->host, src->frag->port, PQnfields(res), s->ncols)));
	oldcxt = MemoryContextSwitchTo(src->cxt);
	for (j = 0; j < s->ncols; j++)
	{
		if (PQgetisnull(res, row, j))
		{
			src->vals[j] = (Datum) 0;
			src->nulls[j] = true;
		}
		else
		{
			src->nulls[j] = false;
			src->vals[j] = InputFunctionCall(&s->infuncs[j], PQgetvalue(res, row, j), s->typioparams[j], s->typmods[j]);
		}
	}
	MemoryContextSwitchTo(oldcxt);
}

/* The next row of a stream; loaded is false at its end. */
static void
merge_advance(KsFragScanState *s, FsMergeSrc *src)
{
	MemoryContextReset(src->cxt);
	src->loaded = false;
	if (src->frag == NULL)
	{
		if (s->next_l >= s->nl && !s->lexhausted)
			fs_local_fetch(s);
		if (s->next_l < s->nl)
		{
			HeapTuple	tup = s->lbatch[s->next_l++];

			/* the datums point into the batch, which stays until this stream asks for the next one */
			heap_deform_tuple(tup, s->local_desc, src->vals, src->nulls);
			src->loaded = true;
		}
	}
	else
	{
		PGresult   *res;
		int			row;

		if (kingres_shard_fragset_next_of(&s->fs, src->frag, &res, &row))
		{
			merge_load_remote(s, src, res, row);
			src->loaded = true;
		}
	}
}

static int
merge_cmp(KsFragScanState *s, FsMergeSrc *a, FsMergeSrc *b)
{
	int			k;

	for (k = 0; k < s->nsort; k++)
	{
		int			col = s->sortcol[k];
		int			r = ApplySortComparator(a->vals[col], a->nulls[col], b->vals[col], b->nulls[col], &s->ssup[k]);

		if (r != 0)
			return r;
	}
	return 0;
}

/*
 * The ordered merge (design 8): every stream is sorted by the keys, the row that comes next is the smallest head, and a tie
 * goes to the stream of the group with the lower group id (this node's own part has the id of its own group, so the order of ties is
 * the same whichever node runs the statement: design 8, O3).  A stream
 * is read again only when its head was taken, so a group that answers slowly holds up the merge for as long as it has to
 * and no longer.
 */
static bool
fs_next_row_merged(KsFragScanState *s, TupleTableSlot *slot)
{
	int			i;
	int			best = -1;

	if (!s->merge_started)
	{
		ListCell   *lc;
		int			n = list_length(s->fs.frags) + (s->has_local ? 1 : 0);
		int			k = 0;

		s->msrc = palloc0(sizeof(FsMergeSrc) * Max(n, 1));
		s->nmsrc = n;
		if (s->has_local)
		{
			s->msrc[k].frag = NULL;
			s->msrc[k].gid = kingres_shard_catalog()->local_group_id;
			k++;
		}
		foreach(lc, s->fs.frags)
		{
			s->msrc[k].frag = (KsFrag *) lfirst(lc);
			s->msrc[k].gid = s->msrc[k].frag->group_id;
			k++;
		}
		/* the streams in the order of their group ids (a stable insertion sort: the number of groups is small) */
		for (i = 1; i < n; i++)
		{
			FsMergeSrc	cur = s->msrc[i];
			int			j = i - 1;

			while (j >= 0 && s->msrc[j].gid > cur.gid)
			{
				s->msrc[j + 1] = s->msrc[j];
				j--;
			}
			s->msrc[j + 1] = cur;
		}
		for (i = 0; i < n; i++)
		{
			s->msrc[i].vals = palloc(sizeof(Datum) * Max(s->ncols, 1));
			s->msrc[i].nulls = palloc(sizeof(bool) * Max(s->ncols, 1));
			s->msrc[i].cxt = AllocSetContextCreate(s->css.ss.ps.state->es_query_cxt, "kingres_shard merge row", ALLOCSET_SMALL_SIZES);
		}
		kingres_shard_fragset_prime(&s->fs);
		for (i = 0; i < n; i++)
			merge_advance(s, &s->msrc[i]);
		s->merge_started = true;
		s->mlast = -1;
	}
	else if (s->mlast >= 0)
		merge_advance(s, &s->msrc[s->mlast]);

	for (i = 0; i < s->nmsrc; i++)
	{
		if (!s->msrc[i].loaded)
			continue;
		if (best < 0 || merge_cmp(s, &s->msrc[i], &s->msrc[best]) < 0)
			best = i;
	}
	if (best < 0)
		return false;
	s->mlast = best;
	ExecClearTuple(slot);
	memcpy(slot->tts_values, s->msrc[best].vals, sizeof(Datum) * s->ncols);
	memcpy(slot->tts_isnull, s->msrc[best].nulls, sizeof(bool) * s->ncols);
	ExecStoreVirtualTuple(slot);
	if (s->css.ss.ps.state->es_instrument)
		count_group_row(s, s->msrc[best].frag == NULL ? -1 : s->msrc[best].frag->group_id);
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

		if (s->nsort > 0 ? !fs_next_row_merged(s, scan) : !fs_next_row(s, scan, econtext))
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
	s->rp_rows = NULL;
	if (s->merge_started)
	{
		int			i;

		/* the arrays are made again at the next start (a rescan): let them go with the contexts (review P2E-10) */
		for (i = 0; i < s->nmsrc; i++)
		{
			MemoryContextDelete(s->msrc[i].cxt);
			pfree(s->msrc[i].vals);
			pfree(s->msrc[i].nulls);
		}
		pfree(s->msrc);
		s->msrc = NULL;
		s->merge_started = false;
		s->nmsrc = 0;
		s->mlast = -1;
	}
	s->started = false;
}

static void
fs_end_scan(CustomScanState *node)
{
	ListCell   *lc;

	fs_stop((KsFragScanState *) node);
	foreach(lc, node->custom_ps)
		ExecEndNode((PlanState *) lfirst(lc));
	node->custom_ps = NIL;
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
	if (s->rp_cols != NIL)
	{
		StringInfoData rep;

		initStringInfo(&rep);
		appendStringInfo(&rep, "%s by %s -> placement of colocation %d (%d buckets)",
						 get_rel_name(s->rp_relid) != NULL ? get_rel_name(s->rp_relid) : "?",
						 get_attname(s->rp_relid, (AttrNumber) s->rp_keyatt, true) != NULL ? get_attname(s->rp_relid, (AttrNumber) s->rp_keyatt, true) : "?",
						 s->colocation_id, coloc != NULL ? kingres_shard_coloc_nbuckets(coloc) : 0);
		if (es->costs)
			appendStringInfo(&rep, ", est %d rows", s->rp_estrows);
		ExplainPropertyText("Repartition", rep.data, es);
	}
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
	if (s->nsort > 0)
		ExplainPropertyInteger("Merge keys", NULL, s->nsort, es);

	initStringInfo(&flat);
	flatten_sql(s->sql, &flat);
	ExplainPropertyText("Remote SQL", flat.data, es);

	if (es->analyze && es->timing)
	{
		StringInfoData ft;
		ListCell   *flc;

		initStringInfo(&ft);
		if (s->local_first_seen)
			appendStringInfo(&ft, "local=%.3f ms", s->local_first_ms);
		foreach(flc, s->fs.frags)
		{
			KsFrag	   *ff = (KsFrag *) lfirst(flc);

			if (ff->first_seen)
				appendStringInfo(&ft, "%sgroup %d=%.3f ms", ft.len > 0 ? ", " : "", ff->group_id, ff->first_ms);
		}
		if (ft.len > 0)
			ExplainPropertyText("Time to the first batch", ft.data, es);
	}

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
