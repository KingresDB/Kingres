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

/* Rows requested from a remote group at a time. */
int			kingres_shard_scan_fetch_size = 1000;

static set_rel_pathlist_hook_type prev_set_rel_pathlist_hook = NULL;
static bool distscan_active = false;
static uint64 cursor_serial = 0;

/* A fragment: the query of one remote group, read through a cursor. */
typedef struct DsFrag
{
	struct KsDistScanState *scan;
	KsConn	   *kc;
	char		cursor[40];
	bool		declared;
	bool		exhausted;
	bool		in_flight;		/* a FETCH was sent and not yet read */
	bool		failed;			/* the answer was read where no error may be raised: raise it when the batch is needed */
	PGresult   *res;			/* the batch being consumed */
	int			nrows;
	int			next_row;
	char	   *host;
	int			port;
	int			group_id;
	int64		identity;		/* identity of the node's partitions when it was first asked */
} DsFrag;

typedef struct KsDistScanState
{
	CustomScanState css;
	char	   *sql;
	Oid			relid;
	int			nchild;			/* scan columns the child returns */
	int64		plan_placement;	/* placement of the colocation group in the plan (the local partitions are those of it) */
	bool		plan_has_parts;	/* the table is partitioned on this node */
	int64		plan_partitions;	/* identity of its partitions in the plan (catalog.c) */
	List	   *prune_terms;	/* pruning terms of the plan (prune.c) */
	int			ncols;			/* scan columns in all */
	List	   *param_ids;
	PlanState  *child;
	bool		started;
	bool		child_done;
	List	   *frags;			/* of DsFrag * */
	int			cur;			/* round-robin position */
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
static void ds_finish_pending(void *owner, bool quiet);
static void ds_stop(KsDistScanState *s, bool close_cursors);

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
 * The shard groups other than this one that own buckets of a table and can hold
 * rows the statement wants: their primaries, copied into the current memory
 * context.  The groups that the restrictions rule out are left out (pruning,
 * design 4.5): terms are the pruning terms of the plan and params the values of
 * its parameters (NULL: a parameter without a value prunes nothing).  *nall, when
 * given, is the number of remote groups before pruning, and *pruned whether a term
 * applied.
 */
static List *
remote_groups_of(const KsDistTable *dt, const KsCatalog *cat, List *terms,
				 ParamListInfo params, int *nall, bool *pruned_out)
{
	List	   *groups = NIL;
	List	   *result = NIL;
	List	   *allowed = NIL;
	bool		pruned = false;
	int			b;
	ListCell   *lc;

	for (b = 0; b < kingres_shard_coloc_nbuckets(dt->coloc); b++)
	{
		int			g = kingres_shard_bucket_group(dt->coloc,
												   kingres_shard_coloc_bucket(dt->coloc, b));

		if (g >= 0 && g != cat->local_group_id && !list_member_int(groups, g))
			groups = lappend_int(groups, g);
	}
	if (nall != NULL)
		*nall = list_length(groups);
	if (terms != NIL)
	{
		/*
		 * The terms are evaluated in a context of their own, which is deleted at once: a
		 * rescan starts the scan again and again in the query context, and the arrays of a
		 * large IN list (detoasted, deconstructed) must not stay until the statement ends
		 * (review P2B-04).  Only the list of groups is copied out.
		 */
		MemoryContext tmp = AllocSetContextCreate(CurrentMemoryContext, "kingres_shard pruning",
												  ALLOCSET_DEFAULT_SIZES);
		MemoryContext oldcxt = MemoryContextSwitchTo(tmp);
		List	   *found = kingres_shard_prune_groups(dt, terms, params, &pruned);

		MemoryContextSwitchTo(oldcxt);
		foreach(lc, found)
			allowed = lappend_int(allowed, lfirst_int(lc));
		MemoryContextDelete(tmp);
	}
	if (pruned_out != NULL)
		*pruned_out = pruned;
	foreach(lc, groups)
	{
		int			g = lfirst_int(lc);
		const KsNode *n;
		DsFrag	   *f;

		if (pruned && !list_member_int(allowed, g))
			continue;
		n = kingres_shard_group_primary(g);

		if (n == NULL)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("shard group %d has no active primary node", g)));
		f = palloc0(sizeof(DsFrag));
		f->group_id = g;
		f->host = pstrdup(n->host);
		f->port = n->port;
		result = lappend(result, f);
	}
	return result;
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
 * Replace the external parameters of a clause by consecutively numbered ones,
 * recording the original ids in order, for a fragment that carries only the
 * parameters it uses.
 */
typedef struct ParamRenumber
{
	List	   *ids;			/* original ids, position = new id - 1 */
} ParamRenumber;

static Node *
renumber_params(Node *node, ParamRenumber *ctx)
{
	if (node == NULL)
		return NULL;
	if (IsA(node, Param))
	{
		Param	   *p = (Param *) copyObject(node);
		int			pos = -1;
		int			i = 0;
		ListCell   *lc;

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
	return expression_tree_mutator(node, renumber_params, ctx);
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
	ParamRenumber prn = {NIL};
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
			c = renumber_params(c, &prn);
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

/*
 * The batches of a scan that an error ended are libpq memory (malloc), which
 * the memory context machinery does not know: a reset callback of the context
 * of the query clears them (review finding S2-17); an error never reaches
 * ExecEndNode.
 */
static void
ds_reset_callback(void *arg)
{
	KsDistScanState *s = (KsDistScanState *) arg;
	ListCell   *lc;

	foreach(lc, s->frags)
	{
		DsFrag	   *f = (DsFrag *) lfirst(lc);

		/*
		 * A connection must not keep pointing at a scan whose memory goes now
		 * (S6-02): whatever way the statement ended, an error, the abort of a
		 * subtransaction, a portal that is dropped.  What the scan had asked
		 * for is read and dropped.
		 */
		if (f->kc != NULL && f->in_flight)
		{
			kingres_shard_conn_abandon_pending(f->kc, f);
			f->in_flight = false;
		}

		/*
		 * PostgreSQL 19 wraps every libpq result in an object that the memory
		 * context frees itself, in a reset callback registered after this one,
		 * so the results are gone already (see pg_compat.h).
		 */
		if (!KS_LIBPQ_RESULTS_OWNED_BY_CONTEXT && f->res != NULL)
			PQclear(f->res);
		f->res = NULL;
	}
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
	{
		MemoryContextCallback *cb = MemoryContextAlloc(estate->es_query_cxt, sizeof(MemoryContextCallback));

		cb->func = ds_reset_callback;
		cb->arg = s;
		MemoryContextRegisterResetCallback(estate->es_query_cxt, cb);
	}
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

/* ---- remote fragments ---- */

/* The value of an external parameter, as text, for the fragment. */
static const char **
fragment_params(KsDistScanState *s, Oid **types_out, int *n_out)
{
	ParamListInfo params = s->css.ss.ps.state->es_param_list_info;
	int			n = list_length(s->param_ids);
	const char **values = palloc0(sizeof(char *) * Max(n, 1));
	Oid		   *types = palloc0(sizeof(Oid) * Max(n, 1));
	int			nest;
	int			i = 0;
	ListCell   *lc;

	nest = kingres_shard_set_transmission_modes();
	foreach(lc, s->param_ids)
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
 * Read the answer of the FETCH that is in flight.  With quiet (an abort path
 * asks, and nothing may be thrown) a failure is kept in the fragment and
 * raised when the scan asks for the batch (frag_check_failed).
 */
static void
frag_collect(DsFrag *f, bool quiet)
{
	PGresult   *res;
	MemoryContext oldcxt;

	if (!f->in_flight)
		return;

	/*
	 * The batch outlives this call, and the call can come from another
	 * statement that wants the connection (ds_finish_pending), in whatever
	 * memory context that statement is in.  PostgreSQL 19 ties a libpq result
	 * to the context it is made in (see pg_compat.h), so make it in the one of
	 * the query, which lives as long as the scan does.
	 *
	 * The scan stays registered on the connection until the answer is read: if
	 * the wait is interrupted, the abort paths find the owner and let go of it
	 * (S6-02).
	 */
	oldcxt = MemoryContextSwitchTo(f->scan->css.ss.ps.state->es_query_cxt);
	if (quiet)
		res = kingres_shard_conn_collect_quiet(f->kc);
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
frag_check_failed(DsFrag *f)
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
ds_finish_pending(void *owner, bool quiet)
{
	DsFrag	   *f = (DsFrag *) owner;

	frag_collect(f, quiet);
}

static void
frag_send_fetch(DsFrag *f)
{
	char		sql[96];

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
	kingres_shard_conn_set_pending(f->kc, f, ds_finish_pending);
}

/*
 * The statement fails the way a failed group does (placement_check.c, 4.6) when
 * a command cannot be sent: the connection is gone.
 */
static void
frag_send_failed(DsFrag *f, const char *what)
{
	f->kc->have_error = true;
	f->kc->in_query = false;
	kingres_shard_fragment_error(f->kc, NULL, what);
}

/*
 * Open the cursors on every group, then ask each for its first batch.  The
 * procedure is the placement check of design 4.3 (placement_check.c): the plan
 * number, the probe before, the DECLAREs, the probe after, and no FETCH before
 * every node has passed.
 */
static void
ds_start(KsDistScanState *s)
{
	EState	   *estate = s->css.ss.ps.state;
	const KsCatalog *cat = kingres_shard_catalog();
	const KsDistTable *dt = hash_search(cat->dist_tables, &s->relid, HASH_FIND, NULL);
	MemoryContext old;
	const char **values;
	Oid		   *types;
	int			nparams;
	ListCell   *lc;
	int			colocation_id;
	char	   *ask;			/* the probe before */
	char	   *recheck;		/* the probe after the DECLAREs */

	if (dt == NULL || dt->is_reference)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("table %u is no longer a distributed table", s->relid)));
	colocation_id = dt->colocation_id;

	/*
	 * Step 1.  The partitions the local part of this scan reads are those of the
	 * placement the statement was planned with (their locks are held since the
	 * executor started); the groups asked below are those of the placement as it is
	 * now.  They must be one placement.
	 */
	kingres_shard_placement_check_plan(colocation_id, s->plan_placement);

	/*
	 * The partition list the plan was made with must be the one this node has
	 * now: a switch can commit here between the parse analysis of the statement
	 * (which locked the parent, and does not stop an ATTACH) and now, and the list
	 * in the relcache of this backend can be behind it.
	 */
	kingres_shard_check_origin_partitions(s->relid, s->plan_has_parts, s->plan_partitions);

	kingres_shard_fault_point("scan_started");

	old = MemoryContextSwitchTo(estate->es_query_cxt);
	s->frags = remote_groups_of(dt, cat, s->prune_terms, estate->es_param_list_info, NULL, NULL);
	MemoryContextSwitchTo(old);
	s->cur = 0;
	values = fragment_params(s, &types, &nparams);

	/*
	 * A transaction uses one node of a group (4.4): checked for every node of the
	 * statement before anything is sent to any.
	 */
	foreach(lc, s->frags)
	{
		DsFrag	   *f = (DsFrag *) lfirst(lc);

		kingres_shard_group_rule(f->host, f->port);
	}

	/*
	 * Step 2.  First every group is asked for its placement, the identity of its
	 * partitions of the table and its version of the extension (the question also
	 * makes the node's partition list of the table current, see
	 * kingres.internal_placement_hash), then the cursors are opened, then every
	 * group is asked again (step 4).  A node whose answers differ, or differ from
	 * the plan's, had a switch commit in between.
	 */
	ask = kingres_shard_probe_before_sql(colocation_id, 1, &s->relid);
	recheck = kingres_shard_probe_after_sql(colocation_id, 1, &s->relid);
	foreach(lc, s->frags)
	{
		DsFrag	   *f = (DsFrag *) lfirst(lc);

		f->scan = s;
		f->kc = kingres_shard_get_conn(f->host, f->port, false, NULL);
		kingres_shard_begin_xact(f->kc);
		f->kc->in_query = true;
		if (!PQsendQuery(f->kc->conn, ask))
			frag_send_failed(f, "placement");
		f->kc->nstatements++;
		f->in_flight = true;
		kingres_shard_conn_set_pending(f->kc, f, ds_finish_pending);
	}
	foreach(lc, s->frags)
	{
		DsFrag	   *f = (DsFrag *) lfirst(lc);
		PGresult   *res;

		res = libpqsrv_get_result_last(f->kc->conn, PG_WAIT_EXTENSION);
		kingres_shard_conn_clear_pending(f->kc, f);
		f->kc->in_query = false;
		f->in_flight = false;
		if (res == NULL)
			f->kc->have_error = true;
		kingres_shard_probe_read_before(f->kc, res, 1, s->plan_placement, &f->identity);
	}
	kingres_shard_fault_point("scan_after_placement");

	/* DECLARE on every group first (they plan concurrently), then read the answers. */
	foreach(lc, s->frags)
	{
		DsFrag	   *f = (DsFrag *) lfirst(lc);
		StringInfoData decl;

		snprintf(f->cursor, sizeof(f->cursor), "ks_scan_" UINT64_FORMAT, ++cursor_serial);
		initStringInfo(&decl);
		appendStringInfo(&decl, KS_FRAGMENT_MARKER "DECLARE %s NO SCROLL CURSOR FOR %s", f->cursor, s->sql);
		f->kc->in_query = true;
		if (!PQsendQueryParams(f->kc->conn, decl.data, nparams, types, values, NULL, NULL, 0))
			frag_send_failed(f, "DECLARE");
		/* libpq holds its own copy; the statement text of a scan with a large IN list is large (review P2B-04) */
		pfree(decl.data);
		f->kc->nstatements++;
		f->in_flight = true;
		kingres_shard_conn_set_pending(f->kc, f, ds_finish_pending);
	}
	foreach(lc, s->frags)
	{
		DsFrag	   *f = (DsFrag *) lfirst(lc);
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
			kingres_shard_fault_point(psprintf("scan_declared_g%d", f->group_id));
	}
	kingres_shard_fault_point("scan_after_declare");

	/*
	 * Step 4, the map version rule (design/shard-phase4.md, "The map version
	 * rule"; design/shard-phase2b.md, 4.3 and 10.2).  A node's bucket partitions of a
	 * table and its placement of the colocation group change only in the
	 * transaction of a switch, which commits on every participant.  The placement
	 * and the identity of the node's partitions of the table were read before the
	 * cursor was opened and are read again now, from the catalog tables and not
	 * from a cache.  The placement of every node must be the plan's; the identity
	 * of a node must be what it was before: a switch that committed in between
	 * changes it, whatever it did to the placement (a bucket that left and came
	 * back leaves the placement as it was, but its partition on the node is a new
	 * table).  When both are unchanged, no switch committed on the node between
	 * the two reads, so the cursor was planned with the partitions of that
	 * placement.  What happens on the node later cannot change what the cursor
	 * reads: it holds the parent in ACCESS SHARE, which a detach (the source) waits
	 * for, and a partition attached later (the target) is not in its plan, which is
	 * what the placement says (the bucket is read from the source).  The statement
	 * fails with the retryable KS001; nothing has been returned yet.
	 */
	foreach(lc, s->frags)
	{
		DsFrag	   *f = (DsFrag *) lfirst(lc);
		PGresult   *res = kingres_shard_exec_params(f->kc, recheck, 0, NULL, NULL, false);

		kingres_shard_probe_read_after(f->kc, res, 1, s->plan_placement, &f->identity);
	}
	/* This node's own placement and partitions can have changed in the same window. */
	kingres_shard_check_origin_placement(colocation_id, s->plan_placement, "reading it");
	kingres_shard_check_origin_partitions(s->relid, s->plan_has_parts, s->plan_partitions);
	kingres_shard_fault_point("scan_after_check");
	foreach(lc, s->frags)
		frag_send_fetch((DsFrag *) lfirst(lc));
	s->started = true;
}

/* Read the batch in flight, forgetting an error: the scan is being closed. */
static void
frag_drain(DsFrag *f)
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
			kingres_shard_conn_abandon_pending(f->kc, f);
		f->in_flight = false;
	}
	PG_END_TRY();
}

/* Close cursors and free batches. */
static void
ds_stop(KsDistScanState *s, bool close_cursors)
{
	ListCell   *lc;

	foreach(lc, s->frags)
	{
		DsFrag	   *f = (DsFrag *) lfirst(lc);

		if (f->in_flight && f->kc != NULL && f->kc->conn != NULL && !f->kc->have_error)
		{
			/* Let the batch that was asked for arrive: cancelling would abort the remote transaction. */
			frag_drain(f);
		}
		else if (f->in_flight && f->kc != NULL)
		{
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
	foreach(lc, s->frags)
	{
		DsFrag	   *f = (DsFrag *) lfirst(lc);

		if (f->kc != NULL && f->kc->pending_owner == (void *) f)
			continue;
		pfree(f->host);
		pfree(f);
	}
	list_free(s->frags);
	s->frags = NIL;
	s->started = false;
}

/*
 * The next row of the other groups into the scan slot, or false at the end.
 * Batches are taken in turn, and the next batch of a group is requested as soon
 * as the current one is used up.
 */
static bool
ds_next_remote(KsDistScanState *s, TupleTableSlot *slot)
{
	int			nfrags = list_length(s->frags);

	if (nfrags == 0)
		return false;
	for (;;)
	{
		int			i;
		bool		any_pending = false;
		DsFrag	   *f = NULL;

		/* An answer that was read in an abort path and was an error (S6-02). */
		for (i = 0; i < nfrags; i++)
			frag_check_failed((DsFrag *) list_nth(s->frags, i));

		/* A batch with rows left. */
		for (i = 0; i < nfrags; i++)
		{
			DsFrag	   *c = (DsFrag *) list_nth(s->frags, (s->cur + i) % nfrags);

			if (c->res != NULL && c->next_row < c->nrows)
			{
				f = c;
				s->cur = (s->cur + i) % nfrags;
				break;
			}
		}
		if (f != NULL)
		{
			int			row = f->next_row++;
			int			j;
			int			r = 0;
			int			expected = (s->nfetch == 0) ? 1 : s->nfetch;
			MemoryContext oldcxt;
			ExprContext *econtext = s->css.ss.ps.ps_ExprContext;

			if (PQnfields(f->res) != expected)
				ereport(ERROR,
						(errcode(ERRCODE_DATA_EXCEPTION),
						 errmsg("node %s:%d returned %d columns, expected %d",
								f->host, f->port, PQnfields(f->res), expected)));
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
				if (PQgetisnull(f->res, row, r))
				{
					slot->tts_values[j] = (Datum) 0;
					slot->tts_isnull[j] = true;
				}
				else
				{
					slot->tts_isnull[j] = false;
					slot->tts_values[j] = InputFunctionCall(&s->infuncs[j],
															PQgetvalue(f->res, row, r),
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

		/* Batches that are used up: free them and ask for the next ones. */
		for (i = 0; i < nfrags; i++)
		{
			DsFrag	   *c = (DsFrag *) list_nth(s->frags, i);

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
				DsFrag	   *c = (DsFrag *) list_nth(s->frags, i);

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
			DsFrag	   *c = (DsFrag *) list_nth(s->frags, (s->cur + i) % nfrags);

			if (c->in_flight)
			{
				frag_collect(c, false);
				break;
			}
		}
	}
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
}
