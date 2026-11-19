/*-------------------------------------------------------------------------
 *
 * kc_cscan.c
 *	  The "Columnar Scan": a custom scan that reads only the columns a query
 *	  needs and skips stripes and chunk groups with the stored statistics.
 *
 * A plain table access method scan has no way to be told which columns are
 * needed or which rows are wanted, so a hook on path generation
 * (set_rel_pathlist_hook) adds a custom path for every base relation stored
 * with the columnar access method.  The ordinary sequential scan path stays in
 * the list and wins when it is cheaper or when
 * kingres_columnar.enable_custom_scan is off.
 *
 * Parallel scan (design 8.4).  The scan has a partial path, a "Parallel
 * Columnar Scan" under a Gather: the unit of work is the stripe, and every
 * participant takes the next stripe nobody has taken from one atomic counter
 * in the dynamic shared memory of the query (kc_scan.c reads the stripe list
 * itself, with the snapshot every participant shares, and checks that its list
 * is the leader's).  Rows the transaction has written and not flushed exist
 * only in the leader's memory, so the leader flushes them into stripes before
 * the executor enters parallel mode (kc_write_prepare_for_workers, called from
 * BeginCustomScan when the plan has a Gather), or fails the plan when it
 * cannot.  Every path of a columnar relation that is not this scan is marked
 * not parallel-safe: the access method scan has no such step and is refused in
 * a worker.  The non-parallel custom path is parallel-safe: in a worker it
 * reads the whole table, as the inner side of a partial join or the single copy
 * of the plan that debug_parallel_query runs.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kc_cscan.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/parallel.h"
#include "access/relation.h"
#include "access/table.h"
#include "catalog/pg_class.h"
#include "commands/explain.h"
#include "commands/explain_format.h"
#include "executor/executor.h"
#include "nodes/extensible.h"
#include "nodes/makefuncs.h"
#include "nodes/pathnodes.h"
#include "optimizer/clauses.h"
#include "optimizer/cost.h"
#include "optimizer/optimizer.h"
#include "optimizer/pathnode.h"
#include "optimizer/paths.h"
#include "optimizer/planmain.h"
#include "optimizer/prep.h"
#include "optimizer/restrictinfo.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/rel.h"

#include "kingres_columnar.h"

typedef struct KcScanState
{
	CustomScanState css;
	KcScan	   *scan;
	KcSkip	   *skip;
	bool	   *needed;
	KcTypeInfo *ti;
	bool		started;

	/* parallel execution */
	KcParallel *pstate;			/* the shared state, while the segment exists */
	KcParStat  *acc;			/* totals of the loops before the last rescan */
	KcParStat  *shut;			/* the shared counters, copied at shutdown */
	int			nslots;
	KcScanStats base;			/* the scan's counters when the state was (re)set */
	uint64		rows;			/* rows returned since then */
	uint64		published;		/* stripes_total at the last publication */
} KcScanState;

static set_rel_pathlist_hook_type prev_set_rel_pathlist = NULL;
static ExecutorStart_hook_type prev_executor_start = NULL;

/*
 * The plan a parallel worker was started to run: the first query the worker
 * starts (ParallelQueryMain), before any query of a function it calls.
 * Compared by address with the plan of a scan that begins in the worker.
 */
static PlannedStmt *kc_worker_top = NULL;

static CustomPathMethods kc_path_methods;
static CustomScanMethods kc_scan_methods;
static CustomExecMethods kc_exec_methods;

/* ------------------------------------------------------------------------
 * Planning
 * ------------------------------------------------------------------------
 */

static Plan *
kc_plan_custom_path(PlannerInfo *root, RelOptInfo *rel, CustomPath *best_path,
					List *tlist, List *clauses, List *custom_plans)
{
	CustomScan *cscan = makeNode(CustomScan);

	cscan->scan.plan.targetlist = tlist;
	cscan->scan.plan.qual = extract_actual_clauses(clauses, false);
	cscan->scan.scanrelid = rel->relid;
	cscan->flags = best_path->flags;
	cscan->custom_plans = custom_plans;
	cscan->custom_exprs = list_copy(cscan->scan.plan.qual);
	cscan->custom_private = best_path->custom_private;
	cscan->custom_scan_tlist = NIL;
	cscan->custom_relids = NULL;
	cscan->methods = &kc_scan_methods;
	return &cscan->scan.plan;
}

/*
 * The columns the scan has to produce: those of the target list and of the
 * restrictions.  A whole-row reference needs them all (-1 in the list).
 */
static List *
kc_needed_columns(RelOptInfo *rel)
{
	Bitmapset  *attrs = NULL;
	ListCell   *lc;
	List	   *res = NIL;
	int			x;

	pull_varattnos((Node *) rel->reltarget->exprs, rel->relid, &attrs);
	foreach(lc, rel->baserestrictinfo)
		pull_varattnos((Node *) ((RestrictInfo *) lfirst(lc))->clause,
					   rel->relid, &attrs);

	x = -1;
	while ((x = bms_next_member(attrs, x)) >= 0)
	{
		AttrNumber	attno = x + FirstLowInvalidHeapAttributeNumber;

		if (attno == InvalidAttrNumber)
			return list_make1_int(-1);
		if (attno > 0)
			res = lappend_int(res, attno);
	}
	return res;
}

/*
 * The divisor a partial path's per-row cost is divided by: the workers, and the
 * leader's share when it takes part (the leader also reads the tuples the workers
 * send, so its share shrinks by 0.3 per worker, as the server's own scans assume).
 */
static double
kc_parallel_divisor(int workers)
{
	double		divisor = workers;

	if (parallel_leader_participation)
	{
		double		leader = 1.0 - 0.3 * workers;

		if (leader > 0)
			divisor += leader;
	}
	return divisor;
}

static void
kc_fill_path(CustomPath *cpath, RelOptInfo *rel, List *needed)
{
	Path	   *p = &cpath->path;

	p->type = T_CustomPath;
	p->pathtype = T_CustomScan;
	p->parent = rel;
	p->pathtarget = rel->reltarget;
	p->param_info = NULL;
	p->pathkeys = NIL;
	cpath->flags = 0;
	cpath->custom_paths = NIL;
	cpath->custom_private = needed;
	cpath->methods = &kc_path_methods;
}

static void
kc_add_custom_path(PlannerInfo *root, RelOptInfo *rel, RangeTblEntry *rte)
{
	CustomPath *cpath = makeNode(CustomPath);
	Path	   *p = &cpath->path;
	List	   *needed = kc_needed_columns(rel);
	QualCost	qcost;
	Relation	relation;
	int			ncols = 0;
	int			nneeded;
	double		frac;
	Cost		startup;
	Cost		cpu_run;
	Cost		disk_run;
	int			workers = 0;
	int			i;

	relation = relation_open(rte->relid, NoLock);
	for (i = 0; i < RelationGetDescr(relation)->natts; i++)
		if (!TupleDescAttr(RelationGetDescr(relation), i)->attisdropped)
			ncols++;

	nneeded = (list_length(needed) == 1 && linitial_int(needed) == -1) ?
		ncols : list_length(needed);
	frac = ncols > 0 ? (double) nneeded / ncols : 1.0;
	if (frac > 1.0)
		frac = 1.0;
	if (frac < 0.05)
		frac = 0.05;

	cost_qual_eval(&qcost, rel->baserestrictinfo, root);
	startup = qcost.startup + rel->reltarget->cost.startup;
	disk_run = seq_page_cost * rel->pages * frac;
	cpu_run = (cpu_tuple_cost * (0.5 + 0.5 * frac) + qcost.per_tuple +
			   rel->reltarget->cost.per_tuple) * rel->tuples;

	kc_fill_path(cpath, rel, needed);
	/*
	 * Rows this backend holds that workers could not be given (an enclosing
	 * subtransaction's): no plan with workers reads this relation.
	 */
	if (rel->consider_parallel && kc_write_parallel_blocked(relation))
		rel->consider_parallel = false;
	p->parallel_aware = false;
	p->parallel_safe = rel->consider_parallel;
	p->parallel_workers = 0;
	p->rows = rel->rows;
	p->startup_cost = startup;
	/* a tie with a sequential scan goes to the scan that can skip */
	p->total_cost = (startup + disk_run + cpu_run) * 0.999;
	add_path(rel, p);

	/*
	 * The partial path: one stripe is the unit of work, so a table of S stripes
	 * has no use for more than S - 1 workers besides the leader.  The number of
	 * workers follows the size of the file (min_parallel_table_scan_size) or the
	 * table's parallel_workers option, as for a heap table.
	 */
	if (rel->consider_parallel && max_parallel_workers_per_gather > 0)
	{
		workers = compute_parallel_worker(rel, rel->pages, -1,
										  max_parallel_workers_per_gather);
		if (workers > 0)
		{
			uint64		stripes = 0;
			uint64		most;

			(void) kc_catalog_row_total(relation, NULL, &stripes, NULL, NULL);
			most = stripes > 0 ? stripes - 1 : 0;
			if ((uint64) workers > most)
				workers = (int) most;
		}
	}
	relation_close(relation, NoLock);

	if (workers > 0)
	{
		CustomPath *ppath = makeNode(CustomPath);
		Path	   *pp = &ppath->path;
		double		divisor = kc_parallel_divisor(workers);

		kc_fill_path(ppath, rel, list_copy(needed));
		pp->parallel_aware = true;
		pp->parallel_safe = true;
		pp->parallel_workers = workers;
		pp->rows = clamp_row_est(rel->rows / divisor);
		pp->startup_cost = startup;
		/* the disk part is not divided, as for a sequential scan: the cache is shared */
		pp->total_cost = (startup + disk_run + cpu_run / divisor) * 0.999;
		add_partial_path(rel, pp);
	}
}

static void
kc_set_rel_pathlist(PlannerInfo *root, RelOptInfo *rel, Index rti,
					RangeTblEntry *rte)
{
	ListCell   *lc;

	if (prev_set_rel_pathlist)
		prev_set_rel_pathlist(root, rel, rti, rte);

	if (rte->rtekind != RTE_RELATION || (rel->reloptkind != RELOPT_BASEREL &&
		 rel->reloptkind != RELOPT_OTHER_MEMBER_REL) ||
		(rte->relkind != RELKIND_RELATION && rte->relkind != RELKIND_MATVIEW))
		return;
	/* by the handler of the access method, not by its name (C11-02) */
	if (!kc_am_is_columnar(get_rel_relam(rte->relid)))
		return;

	/*
	 * Only the custom scan may run in a worker.  Rows the transaction has
	 * written and not flushed live in the leader's memory, and the leader makes
	 * them visible to workers only where it starts a plan that has a Gather and
	 * runs this node (BeginCustomScan); the plain sequential scan has no such
	 * step.  So the partial paths the server made (a Parallel Seq Scan), and the
	 * Gather paths over them, are removed, and every other path made so far is
	 * marked not parallel-safe: not a Parallel Append child, not the inner side of
	 * a partitioned join, not under a Gather.
	 */
	rel->partial_pathlist = NIL;
	foreach(lc, rel->pathlist)
	{
		Path	   *path = (Path *) lfirst(lc);

		if (IsA(path, GatherPath) || IsA(path, GatherMergePath))
			rel->pathlist = foreach_delete_current(rel->pathlist, lc);
		else
			path->parallel_safe = false;
	}

	/*
	 * Plans that need what only a plain sequential scan gives: a sampled scan,
	 * the target of UPDATE or DELETE, rows to be locked.  They fail in the
	 * access method with the message of the operation.
	 */
	if (rte->tablesample != NULL ||
		root->parse->resultRelation == (int) rti ||
		get_plan_rowmark(root->rowMarks, rti) != NULL)
		return;

	if (kc_guc_enable_custom_scan)
		kc_add_custom_path(root, rel, rte);
}

/* ------------------------------------------------------------------------
 * Execution
 * ------------------------------------------------------------------------
 */

static Node *
kc_create_scan_state(CustomScan *cscan)
{
	KcScanState *ks = (KcScanState *) newNode(sizeof(KcScanState), T_CustomScanState);

	ks->css.methods = &kc_exec_methods;
	ks->css.flags = cscan->flags;
	return (Node *) ks;
}

static void
kc_begin_scan(CustomScanState *node, EState *estate, int eflags)
{
	KcScanState *ks = (KcScanState *) node;
	CustomScan *cscan = (CustomScan *) node->ss.ps.plan;
	Relation	rel = node->ss.ss_currentRelation;
	TupleDesc	desc = RelationGetDescr(rel);
	ListCell   *lc;
	bool		all = false;

	ks->ti = kc_typeinfo_build(desc, estate->es_query_cxt);
	ks->needed = palloc0(sizeof(bool) * (desc->natts + 1));
	foreach(lc, cscan->custom_private)
	{
		int			a = lfirst_int(lc);

		if (a == -1)
			all = true;
		else if (a > 0 && a <= desc->natts)
			ks->needed[a - 1] = true;
	}
	if (all)
		memset(ks->needed, true, sizeof(bool) * desc->natts);

	ks->skip = kc_skip_build(cscan->custom_exprs, cscan->scan.scanrelid, desc,
							 ks->ti, &node->ss.ps);
	if (!(eflags & EXEC_FLAG_EXPLAIN_ONLY))
	{
		/*
		 * A plan with a Gather may run this scan in a worker, which cannot see
		 * the rows this backend has buffered.  They are written as stripes now,
		 * here, because the executor enters parallel mode later and a catalog row
		 * cannot be written in it (design 8.4).  A worker's own start of the
		 * plan has nothing to do: it has no buffers.
		 */
		if (!IsParallelWorker() && estate->es_plannedstmt->parallelModeNeeded)
			kc_write_prepare_for_workers(rel);
		/*
		 * In a worker only the scans of the plan the leader started are allowed: a
		 * query that a function of the plan runs in the worker was never seen by
		 * the leader, which has flushed nothing for it.
		 */
		ks->scan = kc_scan_begin(rel, estate->es_snapshot, ks->needed, ks->skip,
								 SO_TYPE_SEQSCAN |
								 ((!IsParallelWorker() ||
								   estate->es_plannedstmt == kc_worker_top) ?
								  KC_SCAN_PARALLEL_OK : 0));
	}
}

/*
 * Write this participant's counters to its slot of the shared state, where the
 * leader reads them for EXPLAIN.  Counters of a rescan are counted from the
 * reset of the state (ks->base).
 */
static void
kc_publish(KcScanState *ks)
{
	KcParStat  *s;
	int			slot = ParallelWorkerNumber + 1;

	if (ks->pstate == NULL || ks->scan == NULL || slot >= ks->pstate->nslots)
		return;
	s = &ks->pstate->stats[slot];
	s->stripes_total = ks->scan->stats.stripes_total - ks->base.stripes_total;
	s->stripes_skipped = ks->scan->stats.stripes_skipped - ks->base.stripes_skipped;
	s->groups_total = ks->scan->stats.groups_total - ks->base.groups_total;
	s->groups_skipped = ks->scan->stats.groups_skipped - ks->base.groups_skipped;
	s->bytes_read = ks->scan->stats.bytes_read - ks->base.bytes_read;
	s->rows = ks->rows;
	ks->published = ks->scan->stats.stripes_total;
}

static TupleTableSlot *
kc_next(ScanState *ss)
{
	KcScanState *ks = (KcScanState *) ss;
	TupleTableSlot *slot = ss->ss_ScanTupleSlot;

	if (!ks->started)
	{
		kc_skip_reevaluate(ks->skip, ss->ps.ps_ExprContext);
		ks->started = true;
	}
	if (kc_scan_next(ks->scan, slot))
	{
		ks->rows++;
		if (ks->pstate != NULL && ks->scan->stats.stripes_total != ks->published)
			kc_publish(ks);
		return slot;
	}
	kc_publish(ks);
	ExecClearTuple(slot);
	return NULL;
}

static bool
kc_recheck(ScanState *ss, TupleTableSlot *slot)
{
	return true;
}

static TupleTableSlot *
kc_exec_scan(CustomScanState *node)
{
	return ExecScan(&node->ss, kc_next, kc_recheck);
}

static void
kc_end_scan(CustomScanState *node)
{
	KcScanState *ks = (KcScanState *) node;

	if (ks->scan)
		kc_scan_end(ks->scan);
	ks->scan = NULL;
	ks->pstate = NULL;
}

static void
kc_rescan(CustomScanState *node)
{
	KcScanState *ks = (KcScanState *) node;

	if (ks->scan)
		kc_scan_rescan(ks->scan);
	ks->started = false;
}

/* ------------------------------------------------------------------------
 * Parallel execution
 * ------------------------------------------------------------------------
 */

static Size
kc_estimate_dsm(CustomScanState *node, ParallelContext *pcxt)
{
	return KcParallelSize(pcxt->nworkers + 1);
}

/* Remember where the scan's counters stand: the slots count from here. */
static void
kc_mark_base(KcScanState *ks)
{
	if (ks->scan != NULL)
		ks->base = ks->scan->stats;
	ks->rows = 0;
	ks->published = 0;
}

static void
kc_initialize_dsm(CustomScanState *node, ParallelContext *pcxt, void *coordinate)
{
	KcScanState *ks = (KcScanState *) node;
	KcParallel *ps = (KcParallel *) coordinate;

	pg_atomic_init_u64(&ps->next, 0);
	ps->nslots = pcxt->nworkers + 1;
	memset(ps->stats, 0, sizeof(KcParStat) * ps->nslots);
	ps->nstripes = 0;
	ps->fingerprint = 0;
	if (ks->scan != NULL)
	{
		ps->nstripes = (uint32) ks->scan->nstripes;
		ps->fingerprint = kc_scan_fingerprint(ks->scan);
		ks->scan->shared = ps;
	}
	ks->pstate = ps;
	ks->nslots = ps->nslots;
	kc_mark_base(ks);
}

/* Add the slots of the finished loop to the totals, before the state is reset. */
static void
kc_fold_slots(KcScanState *ks, const KcParStat *slots, int nslots)
{
	int			i;

	if (ks->acc == NULL)
		ks->acc = MemoryContextAllocZero(ks->css.ss.ps.state->es_query_cxt,
										 sizeof(KcParStat) * nslots);
	for (i = 0; i < nslots; i++)
	{
		ks->acc[i].stripes_total += slots[i].stripes_total;
		ks->acc[i].stripes_skipped += slots[i].stripes_skipped;
		ks->acc[i].groups_total += slots[i].groups_total;
		ks->acc[i].groups_skipped += slots[i].groups_skipped;
		ks->acc[i].bytes_read += slots[i].bytes_read;
		ks->acc[i].rows += slots[i].rows;
	}
}

static void
kc_reinitialize_dsm(CustomScanState *node, ParallelContext *pcxt, void *coordinate)
{
	KcScanState *ks = (KcScanState *) node;
	KcParallel *ps = (KcParallel *) coordinate;

	/* the leader's slot is up to date only up to its last publication */
	kc_publish(ks);
	kc_fold_slots(ks, ps->stats, ps->nslots);
	pg_atomic_write_u64(&ps->next, 0);
	memset(ps->stats, 0, sizeof(KcParStat) * ps->nslots);
	ks->pstate = ps;
	if (ks->scan != NULL)
		ks->scan->shared = ps;
	kc_mark_base(ks);
}

static void
kc_initialize_worker(CustomScanState *node, shm_toc *toc, void *coordinate)
{
	KcScanState *ks = (KcScanState *) node;
	KcParallel *ps = (KcParallel *) coordinate;

	ks->pstate = ps;
	ks->nslots = ps->nslots;
	if (ks->scan != NULL)
		kc_scan_attach(ks->scan, ps);
	kc_mark_base(ks);
}

/*
 * Before the segment goes away.  A worker writes its final counters; the leader
 * keeps a copy of everybody's for EXPLAIN, and goes on without the shared state
 * (a scan that runs after this one reads everything itself).
 */
static void
kc_shutdown(CustomScanState *node)
{
	KcScanState *ks = (KcScanState *) node;

	if (ks->pstate == NULL)
		return;
	kc_publish(ks);
	if (!IsParallelWorker())
	{
		if (ks->shut == NULL)
			ks->shut = MemoryContextAlloc(ks->css.ss.ps.state->es_query_cxt,
										  sizeof(KcParStat) * ks->pstate->nslots);
		memcpy(ks->shut, ks->pstate->stats, sizeof(KcParStat) * ks->pstate->nslots);
		ks->nslots = ks->pstate->nslots;
		if (ks->scan != NULL)
			ks->scan->shared = NULL;
		ks->pstate = NULL;
	}
}

/*
 * The counters of every participant over all loops: those of the loops before
 * the last rescan, and the live shared state or its copy.  Returns the number
 * of slots (0: the scan never ran in parallel), and fills a palloc'd array.
 */
static int
kc_slot_totals(KcScanState *ks, KcParStat **out)
{
	const KcParStat *cur = ks->pstate != NULL ? ks->pstate->stats : ks->shut;
	KcParStat  *res;
	int			i;

	if (ks->nslots == 0 || (cur == NULL && ks->acc == NULL))
	{
		*out = NULL;
		return 0;
	}
	res = palloc0(sizeof(KcParStat) * ks->nslots);
	for (i = 0; i < ks->nslots; i++)
	{
		if (ks->acc != NULL)
			res[i] = ks->acc[i];
		if (cur != NULL)
		{
			res[i].stripes_total += cur[i].stripes_total;
			res[i].stripes_skipped += cur[i].stripes_skipped;
			res[i].groups_total += cur[i].groups_total;
			res[i].groups_skipped += cur[i].groups_skipped;
			res[i].bytes_read += cur[i].bytes_read;
			res[i].rows += cur[i].rows;
		}
	}
	*out = res;
	return ks->nslots;
}

static void
kc_explain(CustomScanState *node, List *ancestors, ExplainState *es)
{
	KcScanState *ks = (KcScanState *) node;
	Relation	rel = node->ss.ss_currentRelation;
	TupleDesc	desc = RelationGetDescr(rel);
	StringInfoData names;
	int			nneeded = 0;
	int			ncols = 0;
	int			i;
	char	   *skipdesc;

	initStringInfo(&names);
	for (i = 0; i < desc->natts; i++)
	{
		Form_pg_attribute att = TupleDescAttr(desc, i);

		if (att->attisdropped)
			continue;
		ncols++;
		if (ks->needed[i])
		{
			appendStringInfo(&names, "%s%s", nneeded ? ", " : "",
							 NameStr(att->attname));
			nneeded++;
		}
	}
	{
		StringInfoData v;

		initStringInfo(&v);
		appendStringInfo(&v, "%d of %d", nneeded, ncols);
		if (nneeded > 0)
			appendStringInfo(&v, " (%s)", names.data);
		ExplainPropertyText("Columnar Projected Columns", v.data, es);
	}

	skipdesc = kc_skip_describe(ks->skip);
	if (skipdesc != NULL && kc_guc_enable_chunk_skipping)
		ExplainPropertyText("Columnar Skip Conditions", skipdesc, es);

	if (es->analyze && ks->scan != NULL)
	{
		StringInfoData v;
		KcScanStats st = ks->scan->stats;
		KcParStat  *slots;
		int			nslots = kc_slot_totals(ks, &slots);

		/*
		 * With workers the totals are everybody's: the leader's own counters
		 * (which the leader's slot repeats) and the workers' slots.
		 */
		if (nslots > 0)
		{
			memset(&st, 0, sizeof(st));
			for (i = 0; i < nslots; i++)
			{
				st.stripes_total += slots[i].stripes_total;
				st.stripes_skipped += slots[i].stripes_skipped;
				st.groups_total += slots[i].groups_total;
				st.groups_skipped += slots[i].groups_skipped;
				st.bytes_read += slots[i].bytes_read;
			}
		}

		initStringInfo(&v);
		appendStringInfo(&v, UINT64_FORMAT " of " UINT64_FORMAT,
						 st.stripes_skipped, st.stripes_total);
		ExplainPropertyText("Columnar Stripes Skipped", v.data, es);
		resetStringInfo(&v);
		appendStringInfo(&v, UINT64_FORMAT " of " UINT64_FORMAT,
						 st.groups_skipped, st.groups_total);
		ExplainPropertyText("Columnar Chunk Groups Skipped", v.data, es);
		ExplainPropertyInteger("Columnar Compressed Bytes Read", "bytes",
							   (int64) st.bytes_read, es);

		/* who read what: the leader, and every worker that read a stripe */
		if (nslots > 0)
		{
			resetStringInfo(&v);
			for (i = 0; i < nslots; i++)
			{
				if (i > 0 && slots[i].stripes_total == 0)
					continue;
				if (i == 0)
					appendStringInfo(&v, "leader " UINT64_FORMAT " (" UINT64_FORMAT " rows)",
									 slots[i].stripes_total, slots[i].rows);
				else
					appendStringInfo(&v, ", worker %d " UINT64_FORMAT " (" UINT64_FORMAT " rows)",
									 i - 1, slots[i].stripes_total, slots[i].rows);
			}
			ExplainPropertyText("Columnar Stripes Read By", v.data, es);
		}
	}
}

static void
kc_executor_start(QueryDesc *queryDesc, int eflags)
{
	if (IsParallelWorker() && kc_worker_top == NULL)
		kc_worker_top = queryDesc->plannedstmt;
	if (prev_executor_start)
		prev_executor_start(queryDesc, eflags);
	else
		standard_ExecutorStart(queryDesc, eflags);
}

void
kc_cscan_init(void)
{
	kc_path_methods.CustomName = "Columnar Scan";
	kc_path_methods.PlanCustomPath = kc_plan_custom_path;

	kc_scan_methods.CustomName = "Columnar Scan";
	kc_scan_methods.CreateCustomScanState = kc_create_scan_state;
	RegisterCustomScanMethods(&kc_scan_methods);

	kc_exec_methods.CustomName = "Columnar Scan";
	kc_exec_methods.BeginCustomScan = kc_begin_scan;
	kc_exec_methods.ExecCustomScan = kc_exec_scan;
	kc_exec_methods.EndCustomScan = kc_end_scan;
	kc_exec_methods.ReScanCustomScan = kc_rescan;
	kc_exec_methods.EstimateDSMCustomScan = kc_estimate_dsm;
	kc_exec_methods.InitializeDSMCustomScan = kc_initialize_dsm;
	kc_exec_methods.ReInitializeDSMCustomScan = kc_reinitialize_dsm;
	kc_exec_methods.InitializeWorkerCustomScan = kc_initialize_worker;
	kc_exec_methods.ShutdownCustomScan = kc_shutdown;
	kc_exec_methods.ExplainCustomScan = kc_explain;

	prev_set_rel_pathlist = set_rel_pathlist_hook;
	set_rel_pathlist_hook = kc_set_rel_pathlist;
	prev_executor_start = ExecutorStart_hook;
	ExecutorStart_hook = kc_executor_start;
}
