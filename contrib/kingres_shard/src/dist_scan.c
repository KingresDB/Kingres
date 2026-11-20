/*-------------------------------------------------------------------------
 *
 * dist_scan.c
 *	  The "Kingres Distributed Scan" custom scan (design/shard-phase2a.md,
 *	  section 5).
 *
 * When the routing decision of planner.c is "this SELECT spans shard groups",
 * the standard planner plans the statement as if all the data were here, and a
 * set_rel_pathlist_hook (this file) replaces the paths of every distributed
 * table by one custom path whose plan is a CustomScan with
 *
 *	- a child: the best local path of the table, which reads the local buckets
 *	  (the partitions this node has) and applies every restriction;
 *	- one remote fragment per other shard group that owns buckets of the table:
 *	  SELECT <the columns needed> FROM <table> WHERE <the restrictions that are
 *	  safe to ship>, run in the remote transaction of the local transaction, and
 *	  read through a cursor in batches (kingres_shard.scan_fetch_size rows),
 *	  with the next batch of every group requested before the current one is
 *	  consumed, so that the groups work concurrently and no group's rows have to
 *	  fit in memory.
 *
 * PostgreSQL then joins, aggregates, sorts and limits above the scan as it
 * does above any scan, so every query shape gives a correct answer.  The rows
 * of the local buckets are returned as the child produced them; the rows of
 * the other groups are filtered again here with every restriction of the table
 * (the shipped ones too: a recheck is always correct), so a restriction that
 * cannot be shipped (a function that is not built in, a subquery) costs
 * transfer, not correctness.
 *
 * The scan tuple has the columns of the table's target list first (what the
 * child returns), then the other columns that the restrictions need, which only
 * the remote rows carry.
 *
 * Not supported in this phase (they raise "not supported yet"): system columns
 * of a distributed table (the remote values would mean nothing here), and
 * PlaceHolderVars computed on the table.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/dist_scan.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/parallel.h"
#include "access/htup_details.h"
#include "access/table.h"
#include "access/xact.h"
#include "catalog/pg_type_d.h"
#include "commands/explain.h"
#include "commands/explain_format.h"
#include "executor/executor.h"
#include "executor/tuptable.h"
#include "libpq/libpq-be-fe-helpers.h"
#include "miscadmin.h"
#include "nodes/extensible.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "nodes/pathnodes.h"
#include "optimizer/cost.h"
#include "optimizer/optimizer.h"
#include "optimizer/paths.h"
#include "optimizer/pathnode.h"
#include "optimizer/restrictinfo.h"
#include "partitioning/partdesc.h"
#include "rewrite/rewriteManip.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/ruleutils.h"
#include "utils/wait_classes.h"

#include "catalog.h"
#include "dist_scan.h"
#include "fragment_scan.h"
#include "kingres_shard.h"
#include "planner.h"
#include "node_conn.h"
#include "placement_check.h"
#include "prune.h"
#include "shippable.h"

/* Layout of CustomScan.custom_private. */
#define DS_PRIV_SQL			0	/* String: the fragment */
#define DS_PRIV_RELID		1	/* Integer: the distributed table */
#define DS_PRIV_NCHILD		2	/* Integer: scan columns the child returns */
#define DS_PRIV_PARAMS		3	/* List of Integer: external parameter ids of the fragment, in order */
#define DS_PRIV_PLACEMENT	4	/* String: placement of the colocation group the statement was planned with */
#define DS_PRIV_PARTITIONS	5	/* String: identity of the partitions of the table on this node, in the plan; "" when it is not partitioned */
#define DS_PRIV_PRUNE		6	/* List: the pruning terms of the restrictions (prune.c); NIL: every group is asked */
#define DS_PRIV_REASON		7	/* String: why the statement is not pushed down ("" when the question was not asked) */

/* Rows requested from a remote group at a time. */
int			kingres_shard_scan_fetch_size = 1000;

static set_rel_pathlist_hook_type prev_set_rel_pathlist_hook = NULL;
static bool distscan_active = false;


typedef struct KsDistScanState
{
	CustomScanState css;
	char	   *sql;
	Oid			relid;
	int			nchild;			/* scan columns the child returns */
	int			colocation_id;	/* of the table, from the catalog when the scan starts */
	char	   *reason;			/* why the statement is not pushed (EXPLAIN VERBOSE), "" when it was not asked */
	int64		plan_placement;	/* placement of the colocation group in the plan (the local partitions are those of it) */
	bool		plan_has_parts;	/* the table is partitioned on this node */
	int64		plan_partitions;	/* identity of its partitions in the plan (catalog.c) */
	List	   *prune_terms;	/* pruning terms of the plan (prune.c) */
	int			ncols;			/* scan columns in all */
	List	   *param_ids;
	PlanState  *child;
	bool		started;
	bool		child_done;
	KsFragSet	fs;				/* the fragments (fragment_scan.c) */
	FmgrInfo   *infuncs;
	Oid		   *typioparams;
	int32	   *typmods;
	bool	   *fetched;		/* column j comes from the other groups */
	ExprState **exprs;		/* column j is computed here (else NULL) */
	int			nfetch;
	Datum	   *values;
	bool	   *nulls;
} KsDistScanState;

static void ds_set_rel_pathlist(PlannerInfo *root, RelOptInfo *rel, Index rti,
								RangeTblEntry *rte);
static Plan *ds_plan_custom_path(PlannerInfo *root, RelOptInfo *rel,
								 CustomPath *best_path, List *tlist,
								 List *clauses, List *custom_plans);
static Node *ds_create_scan_state(CustomScan *cscan);
static void ds_begin_scan(CustomScanState *node, EState *estate, int eflags);
static TupleTableSlot *ds_exec_scan(CustomScanState *node);
static void ds_end_scan(CustomScanState *node);
static void ds_rescan(CustomScanState *node);
static void ds_explain_scan(CustomScanState *node, List *ancestors, ExplainState *es);

static const CustomPathMethods ds_path_methods = {
	.CustomName = "Kingres Distributed Scan",
	.PlanCustomPath = ds_plan_custom_path
};

static const CustomScanMethods ds_scan_methods = {
	.CustomName = "Kingres Distributed Scan",
	.CreateCustomScanState = ds_create_scan_state
};

static const CustomExecMethods ds_exec_methods = {
	.CustomName = "Kingres Distributed Scan",
	.BeginCustomScan = ds_begin_scan,
	.ExecCustomScan = ds_exec_scan,
	.EndCustomScan = ds_end_scan,
	.ReScanCustomScan = ds_rescan,
	.ExplainCustomScan = ds_explain_scan
};

void
kingres_shard_dist_scan_init(void)
{
	RegisterCustomScanMethods(&ds_scan_methods);
	prev_set_rel_pathlist_hook = set_rel_pathlist_hook;
	set_rel_pathlist_hook = ds_set_rel_pathlist;

	DefineCustomIntVariable("kingres_shard.scan_fetch_size",
							"Rows fetched at a time from another shard group by the distributed scan.",
							NULL,
							&kingres_shard_scan_fetch_size,
							1000, 1, 1000000,
							PGC_USERSET, 0,
							NULL, NULL, NULL);
}

/* Returns the previous value. */
bool
kingres_shard_distscan_set_active(bool active)
{
	bool		old = distscan_active;

	distscan_active = active;
	return old;
}

/*
 * The groups other than this one that own buckets of a table and can hold rows the statement
 * wants: their primaries, as fragments (fragment_scan.c).  The groups that the restrictions rule out
 * are left out (pruning, design 4.5): terms are the pruning terms of the plan and params the values of
 * its parameters (NULL: a parameter without a value prunes nothing).  *nall, when given, is the
 * number of remote groups before pruning, and *pruned whether a term applied.
 */
static List *
remote_groups_of(const KsDistTable *dt, const KsCatalog *cat, List *terms,
				 ParamListInfo params, int *nall, bool *pruned_out)
{
	KsFragPrune item;
	List	   *prune = NIL;

	if (terms != NIL)
	{
		item.relid = dt->relid;
		item.terms = terms;
		prune = list_make1(&item);
	}
	return kingres_shard_frag_choose(dt->coloc, cat, prune, params, NULL, nall, pruned_out);
}


/* Does a restriction clause list contain a constant false or NULL clause? */
static bool
has_constant_false(List *restrictlist)
{
	ListCell   *lc;

	foreach(lc, restrictlist)
	{
		RestrictInfo *ri = lfirst_node(RestrictInfo, lc);

		if (IsA(ri->clause, Const))
		{
			Const	   *c = (Const *) ri->clause;

			if (c->constisnull || !DatumGetBool(c->constvalue))
				return true;
		}
	}
	return false;
}

/*
 * ds_set_rel_pathlist
 *
 * Replace the paths of a distributed table by the distributed scan.
 */
static void
ds_set_rel_pathlist(PlannerInfo *root, RelOptInfo *rel, Index rti,
					RangeTblEntry *rte)
{
	const KsCatalog *cat;
	const KsDistTable *dt;
	List	   *remote;
	Path	   *best = NULL;
	ListCell   *lc;
	CustomPath *cpath;
	double		local_rows;
	int			local_buckets = 0;
	int			b;
	double		remote_rows;
	double		nremote;
	List	   *prune_terms;

	if (prev_set_rel_pathlist_hook)
		prev_set_rel_pathlist_hook(root, rel, rti, rte);
	if (!distscan_active)
		return;
	/* A member of a UNION ALL is a member rel too; its partitions are not in the catalog. */
	if ((rel->reloptkind != RELOPT_BASEREL && rel->reloptkind != RELOPT_OTHER_MEMBER_REL) ||
		rte->rtekind != RTE_RELATION)
		return;
	cat = kingres_shard_catalog();
	if (!cat->present)
		return;
	dt = hash_search(cat->dist_tables, &rte->relid, HASH_FIND, NULL);
	if (dt == NULL || dt->is_reference)
		return;
	/* A constant false restriction: no row anywhere. */
	if (has_constant_false(rel->baserestrictinfo))
		return;
	remote = remote_groups_of(dt, cat, NIL, NULL, NULL, NULL);
	if (remote == NIL)
		return;					/* every bucket is here: the local paths are right */

	/*
	 * The groups the restrictions leave (design 4.5).  The plan keeps the terms
	 * and the groups are chosen again when the scan starts, with the values of
	 * the parameters; the count here only prices the scan, and a plan that prunes
	 * every remote group keeps its scan node, whose plan number check protects it.
	 */
	prune_terms = kingres_shard_prune_terms(dt, rel->relid, rel->baserestrictinfo);
	if (prune_terms != NIL)
		remote = remote_groups_of(dt, cat, prune_terms, root->glob->boundParams, NULL, NULL);

	/*
	 * From here on the relation is read through connections of this backend
	 * and the snapshots of its remote transactions: no parallel worker may do
	 * it (review finding S2-08), and no plan above it may be parallel.
	 */
	kingres_shard_refuse_view_owner(root->parse, rte);
	if (IsParallelWorker())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("distributed table \"%s\" cannot be read from a parallel worker", get_rel_name(rte->relid)),
				 errhint("Mark the function that runs the statement PARALLEL RESTRICTED or PARALLEL UNSAFE.")));
	rel->consider_parallel = false;

	foreach(lc, rel->pathlist)
	{
		Path	   *p = (Path *) lfirst(lc);

		if (p->param_info != NULL || IsA(p, GatherPath) || IsA(p, GatherMergePath))
			continue;
		if (best == NULL || p->total_cost < best->total_cost)
			best = p;
	}
	if (best == NULL)
		return;

	/*
	 * A node that the catalog does not call the primary of its group (the old
	 * primary after promote_node(), or a node whose group is not known) owns
	 * no bucket, but its partitions of the table may still be there.  They are
	 * a stale copy of what the group's primary holds: the local part of the
	 * scan reads nothing, and every group is asked.
	 */
	if (cat->local_group_id < 0)
		best = (Path *) create_group_result_path(root, rel, rel->reltarget,
										   list_make1(makeBoolConst(false, false)));

	for (b = 0; b < kingres_shard_coloc_nbuckets(dt->coloc); b++)
		if (kingres_shard_bucket_group(dt->coloc, kingres_shard_coloc_bucket(dt->coloc, b)) == cat->local_group_id)
			local_buckets++;
	local_rows = Max(best->rows, 0);
	nremote = list_length(remote);
	if (local_buckets > 0)
		remote_rows = local_rows * (kingres_shard_coloc_nbuckets(dt->coloc) - local_buckets) / local_buckets;
	else
		remote_rows = 1000.0 * nremote;

	cpath = makeNode(CustomPath);
	cpath->path.pathtype = T_CustomScan;
	cpath->path.parent = rel;
	cpath->path.pathtarget = rel->reltarget;
	cpath->path.param_info = NULL;
	cpath->path.parallel_aware = false;
	cpath->path.parallel_safe = false;
	cpath->path.parallel_workers = 0;
	cpath->path.pathkeys = NIL;
	cpath->path.rows = clamp_row_est(local_rows + remote_rows);
	cpath->path.disabled_nodes = best->disabled_nodes;
	cpath->path.startup_cost = best->startup_cost + 100.0 * nremote;
	cpath->path.total_cost = best->total_cost + 100.0 * nremote +
		remote_rows * (cpu_tuple_cost * 2.0 + 0.02);
	cpath->flags = 0;
	cpath->custom_paths = list_make1(best);
	cpath->custom_restrictinfo = NIL;
	{
		/*
		 * The identity of the partitions of the table on this node as the planner saw
		 * them when it expanded the table, or "" when it did not treat the table as
		 * partitioned.  The child path is made of exactly the partitions of the
		 * descriptor that the planner's partition directory pinned for the whole
		 * statement; the descriptor of the relcache can be rebuilt after that by an
		 * invalidation that a lock acquisition processes, so the directory is asked
		 * and not the relcache.  See ds_start.
		 */
		char	   *identity = pstrdup("");

		if (rel->part_scheme != NULL)
		{
			Relation	prel = table_open(rte->relid, NoLock);
			PartitionDesc pd = PartitionDirectoryLookup(root->glob->partition_directory, prel);

			Assert(pd->nparts == rel->nparts);
			identity = psprintf(INT64_FORMAT,
								kingres_shard_partition_set_identity(pd->nparts, pd->oids));
			table_close(prel, NoLock);
		}

		cpath->custom_private = list_make3(makeInteger((int) rte->relid), makeString(identity),
										   prune_terms);
	}
	cpath->methods = &ds_path_methods;

	rel->pathlist = list_make1(cpath);
	rel->partial_pathlist = NIL;
	rel->rows = cpath->path.rows;

	/*
	 * The table must stop looking partitioned to the rest of the planner:
	 * apply_scanjoin_target_to_paths() throws away the paths of a partitioned
	 * relation and rebuilds them from the paths of its partitions, which would
	 * read the local buckets only.  The partitions stay in the child path.
	 */
	rel->part_scheme = NULL;
	rel->nparts = 0;
	rel->consider_partitionwise_join = false;
}


/*
 * A system column of a distributed table was asked for.  In an UPDATE or
 * DELETE ... FROM / USING a distributed table this is the row identifier
 * (ctid) that PostgreSQL wants of every table it does not modify (S2-18).
 */
static void
ds_error_system_columns(PlannerInfo *root, const char *relname)
{
	if (root->parse->commandType == CMD_UPDATE || root->parse->commandType == CMD_DELETE ||
		root->parse->commandType == CMD_MERGE)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("UPDATE, DELETE and MERGE that read distributed table \"%s\" (FROM, USING) are not supported in a statement that spans shard groups",
						relname),
				 errhint("Read the rows with a SELECT first, or restrict the statement by the distribution column.")));
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("system columns of distributed table \"%s\" are not supported in a statement that spans shard groups",
					relname)));
}

/*
 * The placement of the colocation group of a table, from the catalog the planner
 * used (the groups of the fragments, and the local partitions of the child plan,
 * are those of it).
 */
static int64
plan_placement(Oid relid)
{
	const KsDistTable *dt = kingres_shard_lookup_dist_table(relid);

	if (dt == NULL || dt->is_reference)
		return 0;
	return kingres_shard_placement_hash(dt->coloc);
}

/*
 * ds_plan_custom_path
 *
 * Build the CustomScan: its columns, the fragment that the other groups run,
 * and the restrictions that are applied to the rows they return.
 */
static Plan *
ds_plan_custom_path(PlannerInfo *root, RelOptInfo *rel, CustomPath *best_path,
					List *tlist, List *clauses, List *custom_plans)
{
	CustomScan *cscan = makeNode(CustomScan);
	Oid			relid = (Oid) intVal(linitial(best_path->custom_private));
	List	   *quals = extract_actual_clauses(clauses, false);
	List	   *scan_tlist = NIL;
	List	   *attnos = NIL;	/* attnos in scan_tlist order; 0 = whole row */
	int			resno = 0;
	int			nchild;
	ListCell   *lc;
	List	   *extra;
	List	   *nonvar_exprs = NIL;
	StringInfoData sql;
	StringInfoData select;
	List	   *pushed = NIL;
	KsParamRenumber prn = {NIL};
	List	   *dpcontext;
	int			nest;
	char	   *nsp = get_namespace_name(get_rel_namespace(relid));
	char	   *relname = get_rel_name(relid);
	bool		first;
	List	   *param_ids_int = NIL;

	/*
	 * 1. The columns the child returns: its target list, which is what the
	 * planner needs of the table (the order is the child's, not the relation's).  Its
	 * entries are columns, and also expressions that the planner computes on
	 * the table (the child computes them for the local rows; for the rows of
	 * the other groups they are computed here from the columns fetched).
	 */
	foreach(lc, ((Plan *) linitial(custom_plans))->targetlist)
	{
		Node	   *e = (Node *) ((TargetEntry *) lfirst(lc))->expr;


		if (IsA(e, Var))
		{
			Var		   *v = (Var *) e;

			if (v->varattno < 0)
				ds_error_system_columns(root, relname);
			scan_tlist = lappend(scan_tlist,
								 makeTargetEntry((Expr *) copyObject(v), ++resno, NULL, false));
			attnos = lappend_int(attnos, v->varattno);
		}
		else
		{
			scan_tlist = lappend(scan_tlist,
								 makeTargetEntry((Expr *) copyObject(e), ++resno, NULL, false));
			attnos = lappend_int(attnos, -1);
			nonvar_exprs = lappend(nonvar_exprs, copyObject(e));
		}
	}
	nchild = resno;

	/* Every column the statement reads above this node must come from the child. */
	foreach(lc, pull_var_clause((Node *) tlist, PVC_RECURSE_PLACEHOLDERS))
	{
		Var		   *v = (Var *) lfirst(lc);
		ListCell   *lc2;
		bool		have = false;

		if (v->varno != rel->relid || v->varlevelsup != 0)
			continue;
		foreach(lc2, attnos)
			if (lfirst_int(lc2) == v->varattno)
				have = true;
		if (!have)
		{
			/* An expression of the child may produce it: no column of its own. */
			ListCell   *lc3;

			foreach(lc3, scan_tlist)
			{
				TargetEntry *te = lfirst_node(TargetEntry, lc3);

				if (!IsA(te->expr, Var) && equal(te->expr, v))
					have = true;
			}
		}
		if (!have)
			ereport(ERROR,
					(errcode(ERRCODE_INTERNAL_ERROR),
					 errmsg("distributed scan of \"%s\": column %d is needed above the scan but the local scan does not return it",
							relname, (int) v->varattno)));
	}

	/* 2. The other columns that those expressions and the restrictions need. */
	extra = pull_var_clause((Node *) list_concat(nonvar_exprs, copyObject(quals)),
							PVC_RECURSE_PLACEHOLDERS);
	foreach(lc, extra)
	{
		Var		   *v = (Var *) lfirst(lc);
		ListCell   *lc2;
		bool		have = false;

		if (v->varno != rel->relid || v->varlevelsup != 0)
			continue;
		if (v->varattno < 0)
			ds_error_system_columns(root, relname);
		foreach(lc2, attnos)
			if (lfirst_int(lc2) == v->varattno)
				have = true;
		if (have)
			continue;
		scan_tlist = lappend(scan_tlist,
							 makeTargetEntry((Expr *) copyObject(v), ++resno, NULL, false));
		attnos = lappend_int(attnos, v->varattno);
	}

	/* 3. The fragment. */
	nest = kingres_shard_set_transmission_modes();
	dpcontext = deparse_context_for("ks_t", relid);
	initStringInfo(&select);
	first = true;
	foreach(lc, attnos)
	{
		int			attno = lfirst_int(lc);

		if (attno < 0)
			continue;		/* computed here, not fetched */

		appendStringInfoString(&select, first ? "" : ", ");
		first = false;
		if (attno == 0)
			appendStringInfoString(&select, "ks_t");
		else
			appendStringInfoString(&select,
								   quote_identifier(get_attname(relid, attno, false)));
	}
	if (first)
		appendStringInfoString(&select, "1");
	initStringInfo(&sql);
	appendStringInfo(&sql, "SELECT %s FROM %s AS ks_t", select.data,
					 quote_qualified_identifier(nsp, relname));

	foreach(lc, quals)
	{
		Node	   *q = (Node *) lfirst(lc);

		if (kingres_shard_clause_shippable(q, rel->relid))
		{
			Node	   *c = (Node *) copyObject(q);

			ChangeVarNodes(c, rel->relid, 1, 0);
			c = kingres_shard_renumber_params(c, &prn);
			pushed = lappend(pushed, c);
		}
	}
	if (pushed != NIL)
	{
		appendStringInfoString(&sql, " WHERE ");
		first = true;
		foreach(lc, pushed)
		{
			appendStringInfo(&sql, "%s(%s)", first ? "" : " AND ",
							 deparse_expression((Node *) lfirst(lc), dpcontext, false, false));
			first = false;
		}
	}
	kingres_shard_reset_transmission_modes(nest);
	foreach(lc, prn.ids)
		param_ids_int = lappend(param_ids_int, makeInteger(lfirst_int(lc)));

	cscan->scan.plan.targetlist = tlist;
	cscan->scan.plan.qual = quals;
	cscan->scan.scanrelid = 0;
	cscan->flags = best_path->flags;
	cscan->custom_scan_tlist = scan_tlist;
	cscan->custom_plans = custom_plans;
	cscan->custom_relids = bms_make_singleton(rel->relid);
	cscan->custom_private = list_make5(makeString(sql.data), makeInteger((int) relid),
									   makeInteger(nchild), param_ids_int,
									   makeString(psprintf(INT64_FORMAT,
													   plan_placement(relid))));
	cscan->custom_private = lappend(cscan->custom_private, lsecond(best_path->custom_private));
	cscan->custom_private = lappend(cscan->custom_private, lthird(best_path->custom_private));
	cscan->custom_private = lappend(cscan->custom_private,
									makeString(kingres_shard_pushdown_reason() != NULL ? pstrdup(kingres_shard_pushdown_reason()) : pstrdup("")));
	cscan->methods = &ds_scan_methods;
	return &cscan->scan.plan;
}

static Node *
ds_create_scan_state(CustomScan *cscan)
{
	KsDistScanState *s = palloc0(sizeof(KsDistScanState));
	List	   *p = cscan->custom_private;
	ListCell   *lc;

	NodeSetTag(&s->css, T_CustomScanState);
	s->css.methods = &ds_exec_methods;
	s->css.slotOps = &TTSOpsVirtual;
	s->sql = pstrdup(strVal(list_nth(p, DS_PRIV_SQL)));
	s->relid = (Oid) intVal(list_nth(p, DS_PRIV_RELID));
	s->nchild = intVal(list_nth(p, DS_PRIV_NCHILD));
	s->plan_placement = pg_strtoint64(strVal(list_nth(p, DS_PRIV_PLACEMENT)));
	{
		const char *ident = strVal(list_nth(p, DS_PRIV_PARTITIONS));

		s->plan_has_parts = (ident[0] != '\0');
		s->plan_partitions = s->plan_has_parts ? pg_strtoint64(ident) : 0;
	}
	s->prune_terms = (List *) list_nth(p, DS_PRIV_PRUNE);
	s->reason = pstrdup(strVal(list_nth(p, DS_PRIV_REASON)));
	foreach(lc, (List *) list_nth(p, DS_PRIV_PARAMS))
		s->param_ids = lappend_int(s->param_ids, intVal(lfirst(lc)));
	return (Node *) s;
}

/*
 * An expression of the table's target list, ready to be evaluated here on a
 * row of the other groups: its columns become references to the scan tuple
 * (INDEX_VAR), and a PlaceHolderVar is replaced by what it holds.
 */
typedef struct ExprRewrite
{
	List	   *scan_tlist;
} ExprRewrite;

static Node *
rewrite_for_scan(Node *node, ExprRewrite *ctx)
{
	if (node == NULL)
		return NULL;
	if (IsA(node, PlaceHolderVar))
		return rewrite_for_scan((Node *) ((PlaceHolderVar *) node)->phexpr, ctx);
	if (IsA(node, Var))
	{
		Var		   *v = (Var *) node;
		ListCell   *lc;
		int			k = 0;

		foreach(lc, ctx->scan_tlist)
		{
			TargetEntry *te = lfirst_node(TargetEntry, lc);

			k++;
			if (IsA(te->expr, Var) && ((Var *) te->expr)->varattno == v->varattno)
				return (Node *) makeVar(INDEX_VAR, k, v->vartype, v->vartypmod,
										v->varcollid, 0);
		}
		elog(ERROR, "column %d of a distributed scan is not in its scan tuple", v->varattno);
	}
	return expression_tree_mutator(node, rewrite_for_scan, ctx);
}


static void
ds_begin_scan(CustomScanState *node, EState *estate, int eflags)
{
	KsDistScanState *s = (KsDistScanState *) node;
	CustomScan *cscan = (CustomScan *) node->ss.ps.plan;
	TupleDesc	tupdesc = node->ss.ss_ScanTupleSlot->tts_tupleDescriptor;
	int			i;
	ExprRewrite rw;

	if (IsParallelWorker())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a distributed scan cannot run in a parallel worker"),
				 errhint("Mark the function that runs the statement PARALLEL RESTRICTED or PARALLEL UNSAFE.")));
	kingres_shard_fault_point("scan_begin");
	kingres_shard_fragset_init(&s->fs, estate->es_query_cxt, 1);
	s->child = ExecInitNode((Plan *) linitial(cscan->custom_plans), estate, eflags);
	node->custom_ps = list_make1(s->child);

	rw.scan_tlist = cscan->custom_scan_tlist;
	s->ncols = tupdesc->natts;
	s->infuncs = palloc0(sizeof(FmgrInfo) * Max(s->ncols, 1));
	s->typioparams = palloc0(sizeof(Oid) * Max(s->ncols, 1));
	s->typmods = palloc0(sizeof(int32) * Max(s->ncols, 1));
	s->values = palloc0(sizeof(Datum) * Max(s->ncols, 1));
	s->nulls = palloc0(sizeof(bool) * Max(s->ncols, 1));
	s->fetched = palloc0(sizeof(bool) * Max(s->ncols, 1));
	s->exprs = palloc0(sizeof(ExprState *) * Max(s->ncols, 1));
	s->nfetch = 0;
	for (i = 0; i < s->ncols; i++)
	{
		TargetEntry *te = list_nth_node(TargetEntry, cscan->custom_scan_tlist, i);
		Form_pg_attribute att = TupleDescAttr(tupdesc, i);

		if (IsA(te->expr, Var))
		{
			Oid			infunc;

			getTypeInputInfo(att->atttypid, &infunc, &s->typioparams[i]);
			fmgr_info(infunc, &s->infuncs[i]);
			s->typmods[i] = att->atttypmod;
			s->fetched[i] = true;
			s->nfetch++;
		}
		else
			s->exprs[i] = ExecInitExpr((Expr *) rewrite_for_scan((Node *) copyObject(te->expr), &rw),
									   &node->ss.ps);
	}
}


/* ---- remote fragments (the machinery is fragment_scan.c) ---- */

/* The origin's own partitions are compared again when the cursors of the other groups are open. */
static void
ds_after_check(void *arg)
{
	KsDistScanState *s = (KsDistScanState *) arg;

	kingres_shard_check_origin_placement(s->colocation_id, s->plan_placement, "reading it");
	kingres_shard_check_origin_partitions(s->relid, s->plan_has_parts, s->plan_partitions);
}

/*
 * Open the cursors on every group, then ask each for its first batch.  The procedure is the
 * placement check of design 4.3 (placement_check.c, kingres_shard_fragset_start): the plan
 * number, the probe before, the DECLAREs, the probe after, and no FETCH before every node has
 * passed.
 */
static void
ds_start(KsDistScanState *s)
{
	EState	   *estate = s->css.ss.ps.state;
	const KsCatalog *cat = kingres_shard_catalog();
	const KsDistTable *dt = hash_search(cat->dist_tables, &s->relid, HASH_FIND, NULL);
	MemoryContext old;
	MemoryContext startcxt;
	const char **values;
	Oid		   *types;
	int			nparams;
	KsFragStart st;

	if (dt == NULL || dt->is_reference)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("table %u is no longer a distributed table", s->relid)));
	s->colocation_id = dt->colocation_id;

	/*
	 * Step 1.  The partitions the local part of this scan reads are those of the
	 * placement the statement was planned with (their locks are held since the
	 * executor started); the groups asked below are those of the placement as it is
	 * now.  They must be one placement.
	 */
	kingres_shard_placement_check_plan(s->colocation_id, s->plan_placement);

	/*
	 * The partition list the plan was made with must be the one this node has
	 * now: a switch can commit here between the parse analysis of the statement
	 * (which locked the parent, and does not stop an ATTACH) and now, and the list
	 * in the relcache of this backend can be behind it.
	 */
	kingres_shard_check_origin_partitions(s->relid, s->plan_has_parts, s->plan_partitions);

	kingres_shard_fault_point("scan_started");

	old = MemoryContextSwitchTo(estate->es_query_cxt);
	s->fs.frags = remote_groups_of(dt, cat, s->prune_terms, estate->es_param_list_info, NULL, NULL);
	MemoryContextSwitchTo(old);
	s->fs.cur = 0;

	/* The values of the parameters live in a context of their own: a rescan starts the scan again and again. */
	startcxt = AllocSetContextCreate(CurrentMemoryContext, "kingres_shard scan start", ALLOCSET_SMALL_SIZES);
	old = MemoryContextSwitchTo(startcxt);
	values = kingres_shard_fragment_params(estate->es_param_list_info, s->param_ids, &types, &nparams);

	memset(&st, 0, sizeof(st));
	st.colocation_id = s->colocation_id;
	st.ntables = 1;
	st.relids = &s->relid;
	st.plan_placement = s->plan_placement;
	st.marker = KS_FRAGMENT_MARKER;
	st.sql = s->sql;
	st.nparams = nparams;
	st.ptypes = types;
	st.pvalues = values;
	st.after_check = ds_after_check;
	st.arg = s;
	st.fp_after_probe_before = "scan_after_placement";
	st.fp_after_declare = "scan_after_declare";
	st.fp_after_check = "scan_after_check";
	st.fp_declared_prefix = "scan_";
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
	s->started = true;
}

/* Close cursors and free batches. */
static void
ds_stop(KsDistScanState *s, bool close_cursors)
{
	kingres_shard_fragset_stop(&s->fs, close_cursors);
	s->started = false;
}

/*
 * The next row of the other groups into the scan slot, or false at the end.
 */
static bool
ds_next_remote(KsDistScanState *s, TupleTableSlot *slot)
{
	PGresult   *res;
	int			row;
	KsFrag	   *f;
	int			j;
	int			r = 0;
	int			expected = (s->nfetch == 0) ? 1 : s->nfetch;
	MemoryContext oldcxt;
	ExprContext *econtext = s->css.ss.ps.ps_ExprContext;

	if (!kingres_shard_fragset_next(&s->fs, &res, &row, &f))
		return false;
	if (PQnfields(res) != expected)
		ereport(ERROR,
				(errcode(ERRCODE_DATA_EXCEPTION),
				 errmsg("node %s:%d returned %d columns, expected %d",
						f->host, f->port, PQnfields(res), expected)));
	ExecClearTuple(slot);
	/* The values live until the next call, which resets this context. */
	oldcxt = MemoryContextSwitchTo(econtext->ecxt_per_tuple_memory);
	for (j = 0; j < s->ncols; j++)
	{
		if (!s->fetched[j])
		{
			slot->tts_values[j] = (Datum) 0;
			slot->tts_isnull[j] = true;
			continue;
		}
		if (PQgetisnull(res, row, r))
		{
			slot->tts_values[j] = (Datum) 0;
			slot->tts_isnull[j] = true;
		}
		else
		{
			slot->tts_isnull[j] = false;
			slot->tts_values[j] = InputFunctionCall(&s->infuncs[j],
													PQgetvalue(res, row, r),
													s->typioparams[j],
													s->typmods[j]);
		}
		r++;
	}
	ExecStoreVirtualTuple(slot);
	/* The expressions of the target list, from the columns just read. */
	econtext->ecxt_scantuple = slot;
	for (j = 0; j < s->ncols; j++)
	{
		if (s->exprs[j] != NULL)
			slot->tts_values[j] = ExecEvalExpr(s->exprs[j], econtext,
											   &slot->tts_isnull[j]);
	}
	MemoryContextSwitchTo(oldcxt);
	return true;
}


static TupleTableSlot *
ds_exec_scan(CustomScanState *node)
{
	KsDistScanState *s = (KsDistScanState *) node;
	ExprContext *econtext = node->ss.ps.ps_ExprContext;
	TupleTableSlot *scan = node->ss.ss_ScanTupleSlot;

	if (!s->started)
		ds_start(s);

	for (;;)
	{
		bool		remote;

		CHECK_FOR_INTERRUPTS();
		ResetExprContext(econtext);

		if (!s->child_done)
		{
			TupleTableSlot *cs = ExecProcNode(s->child);

			if (!TupIsNull(cs))
			{
				int			i;

				slot_getallattrs(cs);
				ExecClearTuple(scan);
				for (i = 0; i < s->ncols; i++)
				{
					if (i < s->nchild)
					{
						scan->tts_values[i] = cs->tts_values[i];
						scan->tts_isnull[i] = cs->tts_isnull[i];
					}
					else
					{
						scan->tts_values[i] = (Datum) 0;
						scan->tts_isnull[i] = true;
					}
				}
				ExecStoreVirtualTuple(scan);
				econtext->ecxt_scantuple = scan;
				/* The child applied every restriction already. */
				if (node->ss.ps.ps_ProjInfo)
					return ExecProject(node->ss.ps.ps_ProjInfo);
				return scan;
			}
			s->child_done = true;
		}

		remote = ds_next_remote(s, scan);
		if (!remote)
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
ds_end_scan(CustomScanState *node)
{
	KsDistScanState *s = (KsDistScanState *) node;

	ds_stop(s, true);
	if (s->child != NULL)
		ExecEndNode(s->child);
}

static void
ds_rescan(CustomScanState *node)
{
	KsDistScanState *s = (KsDistScanState *) node;

	ds_stop(s, true);
	s->child_done = false;
	if (node->ss.ps.chgParam != NULL)
		UpdateChangedParamSet(s->child, node->ss.ps.chgParam);
	ExecReScan(s->child);
}

static void
ds_explain_scan(CustomScanState *node, List *ancestors, ExplainState *es)
{
	KsDistScanState *s = (KsDistScanState *) node;
	const KsCatalog *cat = kingres_shard_catalog();
	const KsDistTable *dt = hash_search(cat->dist_tables, &s->relid, HASH_FIND, NULL);
	StringInfoData flat;
	const char *p;
	bool		pending_space = false;
	List	   *remote = NIL;
	int			nall = 0;
	bool		pruned = false;

	if (dt != NULL && !dt->is_reference)
		remote = remote_groups_of(dt, cat, s->prune_terms,
								  node->ss.ps.state->es_param_list_info, &nall, &pruned);
	if (pruned && list_length(remote) < nall)
		ExplainPropertyText("Remote groups",
							psprintf("%d of %d (pruned by %s)", list_length(remote), nall,
									 NameStr(dt->dist_column)), es);
	else
		ExplainPropertyInteger("Remote groups", NULL, list_length(remote), es);

	initStringInfo(&flat);
	for (p = s->sql; *p; p++)
	{
		if (*p == '\n' || *p == '\r' || *p == '\t' || *p == ' ')
		{
			pending_space = (flat.len > 0);
			continue;
		}
		if (pending_space)
			appendStringInfoChar(&flat, ' ');
		pending_space = false;
		appendStringInfoChar(&flat, *p);
	}
	ExplainPropertyText("Remote SQL", flat.data, es);
	if (es->verbose && s->reason[0] != '\0')
		ExplainPropertyText("Pushdown", psprintf("not used: %s", s->reason), es);
}
