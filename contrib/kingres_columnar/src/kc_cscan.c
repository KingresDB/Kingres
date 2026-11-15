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
 * Parallel scans are not offered in this version: the partial paths and the
 * Gather paths over them are removed from a columnar relation.
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
} KcScanState;

static set_rel_pathlist_hook_type prev_set_rel_pathlist = NULL;

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
	Cost		run;
	int			i;

	relation = relation_open(rte->relid, NoLock);
	for (i = 0; i < RelationGetDescr(relation)->natts; i++)
		if (!TupleDescAttr(RelationGetDescr(relation), i)->attisdropped)
			ncols++;
	relation_close(relation, NoLock);

	nneeded = (list_length(needed) == 1 && linitial_int(needed) == -1) ?
		ncols : list_length(needed);
	frac = ncols > 0 ? (double) nneeded / ncols : 1.0;
	if (frac > 1.0)
		frac = 1.0;
	if (frac < 0.05)
		frac = 0.05;

	cost_qual_eval(&qcost, rel->baserestrictinfo, root);
	startup = qcost.startup + rel->reltarget->cost.startup;
	run = seq_page_cost * rel->pages * frac +
		(cpu_tuple_cost * (0.5 + 0.5 * frac) + qcost.per_tuple +
		 rel->reltarget->cost.per_tuple) * rel->tuples;

	p->type = T_CustomPath;
	p->pathtype = T_CustomScan;
	p->parent = rel;
	p->pathtarget = rel->reltarget;
	p->param_info = NULL;
	p->parallel_aware = false;
	p->parallel_safe = false;
	p->parallel_workers = 0;
	p->rows = rel->rows;
	p->startup_cost = startup;
	/* a tie with a sequential scan goes to the scan that can skip */
	p->total_cost = (startup + run) * 0.999;
	p->pathkeys = NIL;

	cpath->flags = 0;
	cpath->custom_paths = NIL;
	cpath->custom_private = needed;
	cpath->methods = &kc_path_methods;
	add_path(rel, p);
}

static void
kc_set_rel_pathlist(PlannerInfo *root, RelOptInfo *rel, Index rti,
					RangeTblEntry *rte)
{
	ListCell   *lc;
	Oid			amoid;

	if (prev_set_rel_pathlist)
		prev_set_rel_pathlist(root, rel, rti, rte);

	if (rte->rtekind != RTE_RELATION || (rel->reloptkind != RELOPT_BASEREL &&
		 rel->reloptkind != RELOPT_OTHER_MEMBER_REL) ||
		(rte->relkind != RELKIND_RELATION && rte->relkind != RELKIND_MATVIEW))
		return;
	amoid = kc_am_oid();
	if (!OidIsValid(amoid) || get_rel_relam(rte->relid) != amoid)
		return;

	/*
	 * Plans that need what only a plain sequential scan gives: a sampled scan,
	 * the target of UPDATE or DELETE, rows to be locked.  They fail in the
	 * access method with the message of the operation.
	 */
	if (rte->tablesample != NULL ||
		root->parse->resultRelation == (int) rti ||
		get_plan_rowmark(root->rowMarks, rti) != NULL)
		return;

	/* no parallel scan in this version: drop partial and Gather paths */
	rel->partial_pathlist = NIL;
	foreach(lc, rel->pathlist)
	{
		Path	   *path = (Path *) lfirst(lc);

		if (IsA(path, GatherPath) || IsA(path, GatherMergePath))
			rel->pathlist = foreach_delete_current(rel->pathlist, lc);
	}

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
		ks->scan = kc_scan_begin(rel, estate->es_snapshot, ks->needed,
								 ks->skip, SO_TYPE_SEQSCAN);
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
		return slot;
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
}

static void
kc_rescan(CustomScanState *node)
{
	KcScanState *ks = (KcScanState *) node;

	if (ks->scan)
		kc_scan_rescan(ks->scan);
	ks->started = false;
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

		initStringInfo(&v);
		appendStringInfo(&v, UINT64_FORMAT " of " UINT64_FORMAT,
						 ks->scan->stats.stripes_skipped,
						 ks->scan->stats.stripes_total);
		ExplainPropertyText("Columnar Stripes Skipped", v.data, es);
		resetStringInfo(&v);
		appendStringInfo(&v, UINT64_FORMAT " of " UINT64_FORMAT,
						 ks->scan->stats.groups_skipped,
						 ks->scan->stats.groups_total);
		ExplainPropertyText("Columnar Chunk Groups Skipped", v.data, es);
		ExplainPropertyInteger("Columnar Compressed Bytes Read", "bytes",
							   (int64) ks->scan->stats.bytes_read, es);
	}
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
	kc_exec_methods.ExplainCustomScan = kc_explain;

	prev_set_rel_pathlist = set_rel_pathlist_hook;
	set_rel_pathlist_hook = kc_set_rel_pathlist;
}
