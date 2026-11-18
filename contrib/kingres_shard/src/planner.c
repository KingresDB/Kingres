/*-------------------------------------------------------------------------
 *
 * planner.c
 *	  The routing decision of kingres_shard, made in the planner hook.
 *
 * For every statement the hook looks for distributed tables in the query
 * tree (design/shard-mvp.md, "Routing").  Statements without distributed
 * tables, and every statement on a cluster with a single shard group, go to
 * the standard planner untouched.  Otherwise each distributed table must be
 * restricted to one key, either by "dist_column = constant or parameter" in
 * the top-level AND of the WHERE clause (or of an inner JOIN's ON clause)
 * or, for a single-row INSERT, by the value written to the distribution
 * column; all keys must hash to the same bucket of one colocation group.
 * The bucket map then names the owning shard group: if that is this node's
 * group the standard planner runs, if it is another group the statement is
 * deparsed and wrapped in the "Kingres Remote Statement" custom scan
 * (remote_scan.c).
 *
 * Phase 2a (design/shard-phase2a.md) handles what used to be refused:
 *	- a SELECT that spans groups (and a write to an ordinary table that reads a
 *	  distributed one) is planned by the standard planner with the distributed
 *	  scan (dist_scan.c) replacing the paths of the distributed tables;
 *	- an UPDATE or DELETE of one distributed table that spans groups and can be
 *	  shipped whole runs on every group that owns buckets (fanout.c);
 *	- a write to a reference table runs on every node (fanout.c);
 *	- the rest (a multi-row INSERT or INSERT ... SELECT across groups, a write
 *	  that cannot be shipped) is refused with a "not supported yet" error.
 * The decision is made per statement; a function called while a statement is
 * planned decides for its own statements, and the distributed scan is switched
 * on only around the standard planner of the statement that needs it.
 *
 * A forwarded session (kingres_shard.forwarded, opened by another node) never
 * forwards, and does not take the origin's word for it either: it runs a
 * statement locally only if every bucket the statement can touch is provably
 * on this node (plan_forwarded()), and raises the stale-map error KS001
 * otherwise.  A restriction by key counts only if the comparison is made
 * under the collation the key is hashed with.  After the standard planner has
 * run, the plan is checked for distributed tables that the analysis did not
 * see, which is what the planner does when it expands a SQL-language function
 * into the statement (check_plan_dist_rtes()).
 *
 * Parameters.  A parameter's value is known at planning time only in a
 * custom plan.  When a decision depends on a parameter that is not known (a
 * generic plan), the hook returns a plan that fails if it is ever executed
 * and is given a huge cost, so the plan cache always prefers a custom plan;
 * see kingres_shard_make_deferred_plan().
 *
 * The catalog cache pointers used here are valid only until the next call of
 * kingres_shard_catalog(), so the hook calls it once and reads everything
 * from that one result.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/planner.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/genam.h"
#include "access/hash.h"
#include "access/htup_details.h"
#include "access/table.h"
#include "access/xact.h"
#include "catalog/partition.h"
#include "catalog/pg_constraint.h"
#include "catalog/pg_language_d.h"
#include "catalog/pg_proc.h"
#include "commands/extension.h"
#include "miscadmin.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "nodes/params.h"
#include "nodes/pathnodes.h"
#include "optimizer/optimizer.h"
#include "optimizer/planner.h"
#include "parser/parse_relation.h"
#include "parser/parsetree.h"
#include "rewrite/rewriteManip.h"
#include "utils/acl.h"
#include "utils/builtins.h"
#include "utils/fmgroids.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/ruleutils.h"
#include "utils/syscache.h"
#include "utils/typcache.h"

#include "batch_sender.h"
#include "bucket_move.h"
#include "catalog.h"
#include "dist_scan.h"
#include "fanout.h"
#include "forward_auth.h"
#include "hashing.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "planner.h"
#include "remote_scan.h"
#include "row_router.h"
#include "shippable.h"
#include "utility.h"

static planner_hook_type prev_planner_hook = NULL;

/*
 * The ExplainState that the planner hook was called with, for the next planner
 * in the chain; NULL on a major whose hook has none.  Saved and restored
 * around each call, because a statement planned while another is planned
 * (see kingres_shard_planner_hook) has its own.
 */
static void *current_explain = NULL;

/* One distributed table reference found in the query tree. */
typedef struct DistRef
{
	Query	   *query;
	Index		rti;
	const KsDistTable *dt;
	int			bucket;			/* -1 when not restricted to one key */
	bool		param_pending;	/* restricted by a parameter of unknown value */
} DistRef;

typedef struct Analysis
{
	ParamListInfo params;
	Query	   *parse;
	PlannerInfo *root;		/* stand-in that carries the bound parameters */
	List	   *refs;			/* of DistRef * */
	bool		has_nondist;	/* the query also reads other relations */
	bool		has_merge;
	int			ndist_rtes;		/* every range table entry of a hash-distributed table */
	bool		has_ref;		/* the query reads or writes a reference table */
} Analysis;

static PlannedStmt *kingres_shard_planner_hook(Query *parse,
												const char *query_string,
												int cursorOptions,
												ParamListInfo boundParams
												KS_PLANNER_ES_PARAM);
static bool is_scan_fragment(Query *q, const KsCatalog *cat);
static bool is_insert_fragment(Query *q, const KsCatalog *cat);
static PlannedStmt *planner_hook_body(Query *parse, const char *query_string,
									   int cursorOptions, ParamListInfo boundParams);
static PlannedStmt *pass_through(Query *parse, const char *query_string,
								 int cursorOptions, ParamListInfo boundParams);

/*
 * kingres_shard_planner_init
 *
 * Install the planner hook, chaining to a previous one.
 */
void
kingres_shard_planner_init(void)
{
	prev_planner_hook = planner_hook;
	planner_hook = kingres_shard_planner_hook;
}

/*
 * The statement is not ours: behave as if the hook were not installed.
 */
static PlannedStmt *
pass_through(Query *parse, const char *query_string, int cursorOptions,
			 ParamListInfo boundParams)
{
	if (prev_planner_hook)
		return prev_planner_hook(parse, query_string, cursorOptions, boundParams
								KS_PLANNER_ES_ARG(current_explain));
	return standard_planner(parse, query_string, cursorOptions, boundParams
							KS_PLANNER_ES_ARG(current_explain));
}

/* Remove binary-compatible relabeling, which hides a Var or a constant. */
static Node *
strip_relabel(Node *n)
{
	while (n != NULL && IsA(n, RelabelType))
		n = (Node *) ((RelabelType *) n)->arg;
	return n;
}

/*
 * Flatten the top-level AND of a qualification into a list of conjuncts.
 */
static void
flatten_and(Node *qual, List **out)
{
	if (qual == NULL)
		return;
	if (IsA(qual, BoolExpr) && ((BoolExpr *) qual)->boolop == AND_EXPR)
	{
		ListCell   *lc;

		foreach(lc, ((BoolExpr *) qual)->args)
			flatten_and((Node *) lfirst(lc), out);
		return;
	}
	*out = lappend(*out, qual);
}

/*
 * Collect the conjuncts that restrict the rows of the whole query level: the
 * WHERE clause and the ON clauses of inner joins.  The ON clause of an outer
 * join does not restrict the preserved side, so outer joins are not entered.
 */
static void
collect_conjuncts(Node *jt, List **out)
{
	ListCell   *lc;

	if (jt == NULL)
		return;
	if (IsA(jt, FromExpr))
	{
		FromExpr   *f = (FromExpr *) jt;

		flatten_and(f->quals, out);
		foreach(lc, f->fromlist)
			collect_conjuncts((Node *) lfirst(lc), out);
	}
	else if (IsA(jt, JoinExpr))
	{
		JoinExpr   *j = (JoinExpr *) jt;

		if (j->jointype == JOIN_INNER)
		{
			flatten_and(j->quals, out);
			collect_conjuncts(j->larg, out);
			collect_conjuncts(j->rarg, out);
		}
	}
}

/*
 * Try to restrict a distributed table reference to one bucket with the
 * expression "key", the value that the distribution column must have.
 *
 * The expression is first simplified the way the planner would (constant
 * folding, and substitution of parameters whose value is known, which the
 * caller's ParamListInfo marks PARAM_FLAG_CONST in a custom plan), so that
 * "id = 5::bigint" and "id = $1" are recognized.  What is left must be a
 * constant, which hashes to a bucket, or a parameter of unknown value, which
 * defers the decision.
 */
static void
restrict_to_value(Analysis *a, DistRef *ref, Node *key)
{
	const KsColocation *coloc = ref->dt->coloc;
	int			bucket;

	if (key == NULL)
		return;
	if (a->root == NULL)
	{
		/*
		 * eval_const_expressions() takes the bound parameters from the
		 * PlannerGlobal of a PlannerInfo; this one exists only for that, and
		 * is built on first use because most statements never need it.
		 */
		PlannerGlobal *glob = makeNode(PlannerGlobal);

		glob->boundParams = a->params;
		a->root = makeNode(PlannerInfo);
		a->root->parse = a->parse;
		a->root->glob = glob;
	}
	key = strip_relabel(eval_const_expressions(a->root, key));

	if (key != NULL && IsA(key, Const))
	{
		Const	   *c = (Const *) key;

		if (c->constisnull)
			return;
		if (kingres_shard_try_bucket(coloc->dist_type, coloc->collation,
									  coloc, c->consttype,
									  c->constvalue, false, &bucket))
			ref->bucket = bucket;
	}
	else if (key != NULL && IsA(key, Param) &&
			 ((Param *) key)->paramkind == PARAM_EXTERN)
		ref->param_pending = true;
}

/*
 * Look at one conjunct: "dist_column = key" (either order) restricts the
 * matching reference.  The operator must be an equality operator of the hash
 * operator family of the column, so that equal values hash equally.
 */
static void
restrict_from_conjunct(Analysis *a, List *refs, Node *clause)
{
	OpExpr	   *op;
	Node	   *args[2];
	int			i;
	ListCell   *lc;

	if (!IsA(clause, OpExpr))
		return;
	op = (OpExpr *) clause;
	if (list_length(op->args) != 2)
		return;
	args[0] = strip_relabel((Node *) linitial(op->args));
	args[1] = strip_relabel((Node *) lsecond(op->args));

	for (i = 0; i < 2; i++)
	{
		Node	   *var = args[i];

		if (var == NULL || !IsA(var, Var) || ((Var *) var)->varlevelsup != 0)
			continue;
		foreach(lc, refs)
		{
			DistRef    *ref = (DistRef *) lfirst(lc);
			TypeCacheEntry *tce;

			if (ref->rti != ((Var *) var)->varno ||
				ref->dt->dist_attno != ((Var *) var)->varattno ||
				ref->bucket >= 0 || ref->param_pending)
				continue;
			tce = lookup_type_cache(ref->dt->coloc->dist_type,
									TYPECACHE_HASH_OPFAMILY);
			if (!OidIsValid(tce->hash_opf) ||
				!op_in_opfamily(op->opno, tce->hash_opf))
				continue;

			/*
			 * The bucket of the key is computed with the collation the table
			 * was distributed with, and PostgreSQL chooses the partition of a
			 * row the same way.  The comparison restricts the statement to
			 * that bucket only if it is made under the same collation, and
			 * that collation is deterministic (equal values hash equally);
			 * under another collation, or a nondeterministic one such as a
			 * case-insensitive ICU collation, values that compare equal can
			 * live in other buckets (review finding 2).
			 */
			if (op->inputcollid != ref->dt->coloc->collation ||
				(OidIsValid(op->inputcollid) &&
				 !get_collation_isdeterministic(op->inputcollid)))
				continue;
			restrict_to_value(a, ref, args[1 - i]);
		}
	}
}

/*
 * Examine the range table of one query level and its restrictions.
 */
static void
analyze_query(Query *q, Analysis *a, const KsCatalog *cat)
{
	ListCell   *lc;
	List	   *mine = NIL;
	List	   *conjuncts = NIL;
	Index		rti = 0;

	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);
		KsDistTable *dt;

		rti++;
		if (rte->rtekind != RTE_RELATION)
			continue;

		/*
		 * The "excluded" pseudo-relation of ON CONFLICT DO UPDATE is a second
		 * range table entry for the target table; it holds the row being
		 * inserted, not a scan, and takes part in no routing decision.
		 */
		dt = hash_search(cat->dist_tables, &rte->relid, HASH_FIND, NULL);
		if (dt != NULL && dt->is_reference)
		{
			/* every node has the whole table: it takes no part in routing */
			a->has_ref = true;
			continue;
		}
		if (dt != NULL)
			a->ndist_rtes++;
		if (q->onConflict != NULL && q->onConflict->exclRelIndex == (int) rti)
			continue;
		if (dt != NULL)
		{
			DistRef    *ref = palloc0(sizeof(DistRef));

			ref->query = q;
			ref->rti = rti;
			ref->dt = dt;
			ref->bucket = -1;
			mine = lappend(mine, ref);
			a->refs = lappend(a->refs, ref);
		}
		else
			a->has_nondist = true;
	}
	if (mine == NIL)
		return;
	if (q->commandType == CMD_MERGE)
		a->has_merge = true;

	collect_conjuncts((Node *) q->jointree, &conjuncts);
	foreach(lc, conjuncts)
		restrict_from_conjunct(a, mine, (Node *) lfirst(lc));

	/* A single-row INSERT: the value written to the distribution column. */
	if (q->commandType == CMD_INSERT && q->resultRelation > 0)
	{
		ListCell   *lc2;

		foreach(lc, mine)
		{
			DistRef    *ref = (DistRef *) lfirst(lc);

			if (ref->rti != (Index) q->resultRelation)
				continue;
			foreach(lc2, q->targetList)
			{
				TargetEntry *te = (TargetEntry *) lfirst(lc2);

				if (!te->resjunk && te->resno == ref->dt->dist_attno)
					restrict_to_value(a, ref, (Node *) te->expr);
			}
		}
	}
}

static bool
analyze_walker(Node *node, void *context)
{
	Analysis   *a = ((void **) context)[0];
	const KsCatalog *cat = ((void **) context)[1];

	if (node == NULL)
		return false;
	if (IsA(node, Query))
	{
		analyze_query((Query *) node, a, cat);
		return query_tree_walker((Query *) node, analyze_walker, context, 0);
	}
	return expression_tree_walker(node, analyze_walker, context);
}

/*
 * The name of a table for messages, schema-qualified.
 */
static char *
qualified_name(Oid relid)
{
	return quote_qualified_identifier(get_namespace_name(get_rel_namespace(relid)),
									  get_rel_name(relid));
}

/*
 * insert_source_needs_wrapping
 *
 * The rewritten INSERT ... SELECT (or multi-row INSERT ... VALUES) keeps the
 * source as a range table entry and puts into the target list what is written
 * to each column: a Var of the source, or an expression of its own, such as the
 * default of a column the statement did not name.  The deparser prints the
 * column list from the target list and the source as it is, so a default (or a
 * column list in another order than the source's) gives a column list longer
 * than the source: "INSERT has more target columns than expressions" (review
 * finding S3-10).  True when some column does not simply take the next column
 * of the source.
 */
static bool
insert_source_needs_wrapping(Query *query, Index srcrti)
{
	ListCell   *lc;
	AttrNumber	next = 1;

	foreach(lc, query->targetList)
	{
		TargetEntry *te = lfirst_node(TargetEntry, lc);
		List	   *vars;

		if (te->resjunk)
			continue;
		vars = pull_var_clause((Node *) te->expr, 0);
		if (list_length(vars) != 1 ||
			((Var *) linitial(vars))->varno != srcrti ||
			((Var *) linitial(vars))->varattno != next)
			return true;
		next++;
	}
	return false;
}

/*
 * wrap_insert_source
 *
 * Rewrite such an INSERT so that its source is a SELECT that returns exactly
 * the value of each column of the target list, in that order:
 *
 *	INSERT INTO t (a, d, b) SELECT ks_src.x, 7, ks_src.y FROM (<source>) ks_src
 *
 * The expressions are moved from the target list into the SELECT, the
 * target list keeps one Var per column.  Defaults come from this node's
 * catalog and are sent as the values they have, not looked up again on the
 * other node.
 */
static Query *
wrap_insert_source(Query *query)
{
	Query	   *q = copyObject(query);
	RangeTblEntry *src = NULL;
	Index		srcrti = 0;
	Index		rti = 0;
	Oid			target_relid;
	Query	   *w;
	RangeTblEntry *inner;
	RangeTblEntry *outer;
	RangeTblRef *rtr;
	List	   *colnames = NIL;
	AttrNumber	pos = 0;
	ListCell   *lc;

	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = lfirst_node(RangeTblEntry, lc);

		rti++;
		if (rte->rtekind == RTE_SUBQUERY || rte->rtekind == RTE_VALUES)
		{
			src = rte;
			srcrti = rti;
			break;
		}
	}
	if (src == NULL || !insert_source_needs_wrapping(q, srcrti))
		return query;
	target_relid = rt_fetch(q->resultRelation, q->rtable)->relid;

	w = makeNode(Query);
	w->commandType = CMD_SELECT;
	w->querySource = QSRC_ORIGINAL;
	inner = copyObject(src);
	inner->eref = makeAlias("ks_src", inner->eref->colnames);
	inner->alias = NULL;
	inner->inFromCl = true;
	w->rtable = list_make1(inner);
	rtr = makeNode(RangeTblRef);
	rtr->rtindex = 1;
	w->jointree = makeFromExpr(list_make1(rtr), NULL);

	foreach(lc, q->targetList)
	{
		TargetEntry *te = lfirst_node(TargetEntry, lc);
		Node	   *expr;
		char	   *name;

		if (te->resjunk)
			continue;
		pos++;
		expr = copyObject((Node *) te->expr);
		ChangeVarNodes(expr, srcrti, 1, 0);
		name = get_attname(target_relid, te->resno, false);
		w->targetList = lappend(w->targetList,
								makeTargetEntry((Expr *) expr, pos, pstrdup(name), false));
		colnames = lappend(colnames, makeString(pstrdup(name)));
		te->expr = (Expr *) makeVar(srcrti, pos, exprType(expr), exprTypmod(expr),
									exprCollation(expr), 0);
	}

	outer = makeNode(RangeTblEntry);
	outer->rtekind = RTE_SUBQUERY;
	outer->subquery = w;
	outer->eref = makeAlias("*SELECT*", colnames);
	outer->inFromCl = src->inFromCl;
	outer->rellockmode = 0;
	lfirst(list_nth_cell(q->rtable, srcrti - 1)) = outer;
	return q;
}

/*
 * deparse_for_remote
 *
 * Turn the query back into SQL text for another node.  The text must mean the
 * same on the remote node whatever the settings of this session are (review
 * S-C): the transmission modes (node_conn.c) make constants come out as ISO
 * dates, postgres-style intervals, shortest-exact floats, standard-conforming
 * strings and hex bytea, and a search_path of pg_catalog makes the deparser
 * schema-qualify every relation, function and type.  The remote session runs
 * under the same settings.
 */
static char *
deparse_for_remote(Query *query)
{
	int			nest = kingres_shard_set_transmission_modes();
	char	   *sql;

	if (query->commandType == CMD_INSERT)
		query = wrap_insert_source(query);
	sql = pg_get_querydef(query, false);
	kingres_shard_reset_transmission_modes(nest);
	return sql;
}

/*
 * Refuse a statement that is outside the phase 1 boundary.  The hint names
 * the distribution column of the first table that is not restricted.
 */
static void
error_spans_groups(DistRef *ref, const char *detail)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("statements that span shard groups are not supported yet"),
			 detail ? errdetail("%s", detail) : 0,
			 errhint("Restrict every distributed table of the statement to one key, for example with WHERE %s = <value> on table \"%s\".",
					 NameStr(ref->dt->dist_column),
					 get_rel_name(ref->dt->relid))));
}

/*
 * kingres_shard_refuse_view_owner
 *
 * PostgreSQL checks the rights on the table of a view with the owner of the
 * view (the range table entry carries it in checkAsUser), unless the view is a
 * security_invoker view.  A fragment that is run on another node runs as the
 * role of the session, so the owner's rights would be lost there: the case is
 * refused with a clear message instead of failing with a permission error on
 * the other node or, worse, running with the wrong rights (review finding
 * S2-15).  Called wherever work is sent to another node.
 */
void
kingres_shard_refuse_view_owner(Query *q, RangeTblEntry *rte)
{
	Oid			owner = InvalidOid;

	if (rte->perminfoindex > 0)
		owner = getRTEPermissionInfo(q->rteperminfos, rte)->checkAsUser;
	if (OidIsValid(owner) && owner != GetUserId())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("distributed table \"%s\" is read through a view owned by role \"%s\" and the statement needs another shard group",
						get_rel_name(rte->relid), GetUserNameFromId(owner, false)),
				 errdetail("The rights on the table are those of the owner of the view, and a statement run on another node uses the rights of the session role."),
				 errhint("Create the view WITH (security_invoker = true), or run the statement as the owner of the view.")));
}

/*
 * error_write_spans_groups
 *
 * An UPDATE or DELETE that spans shard groups and cannot be run group by group.
 */
static void
error_write_spans_groups(Query *parse, DistRef *ref, const char *detail)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("this %s spans shard groups and cannot be shipped",
					parse->commandType == CMD_UPDATE ? "UPDATE" : "DELETE"),
			 detail ? errdetail("%s", detail) : 0,
			 errhint("Restrict it by the distribution column, for example with WHERE %s = <value>, or use only built-in immutable functions on the one table (and reference tables).",
					 NameStr(ref->dt->dist_column))));
}

/*
 * plan_distributed_write
 *
 * An UPDATE or DELETE of one distributed table that is not restricted to one
 * bucket (design/shard-phase2a.md, section 6): when the whole statement can be
 * shipped (shippable.c: it reads no other distributed table, uses only built-in
 * immutable functions and operators) it is executed on every shard group that
 * owns buckets of the table, in one distributed transaction, and the counts
 * and the RETURNING rows of the groups are added.  A statement that assigns the
 * distribution column is refused: the row would have to move between groups.
 * The text carries KS_WRITE_MARKER, which tells the forwarded sessions of the
 * other groups that this is such a fragment.
 */
static PlannedStmt *
plan_distributed_write(Query *parse, DistRef *ref, const KsCatalog *cat, List *relids)
{
	const char *reason = NULL;
	List	   *groups = NIL;
	List	   *targets = NIL;
	ListCell   *lc;
	int			b;
	char	   *sql;

	if (parse->commandType == CMD_UPDATE)
	{
		foreach(lc, parse->targetList)
		{
			TargetEntry *te = lfirst_node(TargetEntry, lc);

			if (!te->resjunk && te->resno == ref->dt->dist_attno)
				ereport(ERROR,
						(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
						 errmsg("an UPDATE of the distribution column \"%s\" of \"%s\" is not supported",
								NameStr(ref->dt->dist_column), get_rel_name(ref->dt->relid)),
						 errdetail("The row would have to move to another bucket, possibly of another shard group."),
						 errhint("DELETE the row and INSERT it with the new key.")));
		}
	}
	if (parse->hasModifyingCTE || parse->rowMarks != NIL ||
		!kingres_shard_stmt_shippable(parse, ref->dt->relid, &reason))
		error_write_spans_groups(parse, ref,
								 reason ? psprintf("The statement is not shippable: %s.", reason) : NULL);
	kingres_shard_refuse_view_owner(ref->query, rt_fetch(ref->rti, ref->query->rtable));
	if (IsInParallelMode())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a statement for several shard groups cannot be run from a parallel worker")));

	for (b = 0; b < kingres_shard_coloc_nbuckets(ref->dt->coloc); b++)
	{
		int			g = kingres_shard_bucket_group(ref->dt->coloc,
												   kingres_shard_coloc_bucket(ref->dt->coloc, b));

		if (g >= 0 && !list_member_int(groups, g))
			groups = lappend_int(groups, g);
	}
	list_sort(groups, list_int_cmp);
	foreach(lc, groups)
	{
		int			g = lfirst_int(lc);
		const KsNode *n = NULL;
		ListCell   *lc2;
		KsFanTarget *t;

		foreach(lc2, cat->nodes)
		{
			KsNode	   *c = (KsNode *) lfirst(lc2);

			if (c->group_id == g && c->is_primary && c->is_active)
			{
				n = c;
				break;
			}
		}
		if (n == NULL)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("shard group %d has no active primary node", g)));
		t = palloc(sizeof(KsFanTarget));
		t->host = n->host;
		t->port = n->port;
		t->is_local = (g == cat->local_group_id);
		targets = lappend(targets, t);
	}

	sql = psprintf("%s%s", KS_WRITE_MARKER, deparse_for_remote(parse));
	return kingres_shard_make_fanout_plan(parse, sql, targets, true, relids, NIL);
}

/*
 * all_buckets_local
 *
 * True when every bucket of the colocation group belongs to this node's
 * shard group, so that whatever a statement on a table of the group touches
 * is local.
 */
static bool
all_buckets_local(const KsColocation *coloc, int local_group)
{
	int			b;

	for (b = 0; b < kingres_shard_coloc_nbuckets(coloc); b++)
	{
		if (kingres_shard_bucket_group(coloc, kingres_shard_coloc_bucket(coloc, b)) != local_group)
			return false;
	}
	return true;
}

/*
 * error_not_provably_local
 *
 * A forwarded session was asked to run a statement that is not provably
 * confined to buckets of this node (review finding 1).  It raises KS001, the
 * error of "this is not on this node", which the originating node treats as a
 * stale bucket map.
 */
static void
error_not_provably_local(const KsDistTable *dt, int local_group,
						 const char *detail)
{
	ereport(ERROR,
			(errcode(ERRCODE_KINGRES_STALE_MAP),
			 errmsg("cannot prove that the statement touches only buckets of this node"),
			 errdetail("%s", detail ? detail :
					   psprintf("Distributed table %s is not restricted to one bucket, and shard group %d does not own every bucket of its colocation group.",
								qualified_name(dt->relid), local_group)),
			 errhint("A session opened by another node runs a statement locally only when every bucket the statement can touch is on this node.")));
}

/*
 * check_plan_dist_rtes
 *
 * The routing decision is made on the query tree before planning, but the
 * planner can add relations to a statement afterwards: it expands the body of
 * a SQL-language function into the calling statement (review S-D).  The
 * expanded statement would read the local partitions only, unrestricted, and
 * return a wrong answer without an error.  The analysis counted the
 * distributed tables it saw (expected); a plan that has more of them than
 * that came from an expansion the analysis could not see.
 *
 * Only a SQL-language function can be expanded, so the count means something only when the
 * statement calls one (review P2B-03).  The planner also copies range table entries of its own
 * accord: a UNION, INTERSECT or EXCEPT with an arm that cannot match anything has one more
 * entry of the table in its plan than the statement has references, and counting that as an
 * expansion refused such a statement with a message that was not true.
 */
static bool
func_is_sql(Oid funcid)
{
	HeapTuple	tup = SearchSysCache1(PROCOID, ObjectIdGetDatum(funcid));
	bool		result;

	if (!HeapTupleIsValid(tup))
		return false;
	result = ((Form_pg_proc) GETSTRUCT(tup))->prolang == SQLlanguageId;
	ReleaseSysCache(tup);
	return result;
}

static bool
sql_function_walker(Node *node, void *context)
{
	if (node == NULL)
		return false;
	if (IsA(node, FuncExpr))
	{
		if (func_is_sql(((FuncExpr *) node)->funcid))
			return true;
	}
	else if (IsA(node, OpExpr) || IsA(node, DistinctExpr) || IsA(node, NullIfExpr))
	{
		OpExpr	   *o = (OpExpr *) node;

		set_opfuncid(o);
		if (func_is_sql(o->opfuncid))
			return true;
	}
	else if (IsA(node, Query))
		return query_tree_walker((Query *) node, sql_function_walker, context, 0);
	return expression_tree_walker(node, sql_function_walker, context);
}

static void
check_plan_dist_rtes(bool calls_sql_function, PlannedStmt *pstmt, int expected)
{
	/* Planning may have run a function that reloaded the catalog cache. */
	const KsCatalog *cat = kingres_shard_catalog();
	ListCell   *lc;
	int			found = 0;
	Oid			first = InvalidOid;

	/* The empty catalog that a nested call sees while the cache loads. */
	if (!cat->present || cat->dist_tables == NULL)
		return;

	/* nothing the planner could have expanded */
	if (!calls_sql_function)
		return;

	foreach(lc, pstmt->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		if (rte->rtekind != RTE_RELATION)
			continue;
		{
			KsDistTable *d2 = hash_search(cat->dist_tables, &rte->relid, HASH_FIND, NULL);

			if (d2 == NULL || d2->is_reference)
				continue;
		}
		if (++found == 1)
			first = rte->relid;
	}
	if (found <= expected)
		return;

	if (kingres_shard_session_forwarded())
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("cannot prove that the statement touches only buckets of this node"),
				 errdetail("The planner expanded a SQL-language function that reads distributed table %s.",
						   qualified_name(first)),
				 errhint("A session opened by another node runs a statement locally only when every bucket the statement can touch is on this node.")));
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("statements that reach distributed tables through a SQL-language function are not supported yet"),
			 errdetail("The planner expanded a SQL-language function into the statement, and the body of the function reads distributed table %s without a key the router could see.",
					   qualified_name(first)),
			 errhint("Write the function in PL/pgSQL, which is never expanded and whose statements are routed one by one, or put the key in the statement itself.")));
}

/*
 * plan_locally
 *
 * Let the standard planner plan a statement that runs on this node, and check
 * that the planner did not add distributed tables the routing decision did not
 * see.
 */
static PlannedStmt *
plan_locally(Query *parse, const char *query_string, int cursorOptions,
			 ParamListInfo boundParams, const KsCatalog *cat, int expected)
{
	/* looked for before planning: the planner replaces the call of a SQL function by its body */
	bool		calls_sql_function = sql_function_walker((Node *) parse, NULL);
	PlannedStmt *pstmt = pass_through(parse, query_string, cursorOptions,
									  boundParams);

	check_plan_dist_rtes(calls_sql_function, pstmt, expected);
	return pstmt;
}

/*
 * plan_forwarded
 *
 * The statement of a forwarded session (kingres_shard.forwarded is on): the
 * originating node decided that it belongs to this node, but this node does
 * not rely on that.  It runs the statement only if every bucket it can touch
 * is provably local: a table restricted to one bucket needs that bucket to be
 * ours, and an unrestricted table needs the whole colocation group to be ours
 * (a single shard group).  Anything else raises KS001, so that a wrong answer
 * is never silent whoever set the flag.  A statement whose routing depends on
 * a parameter that is not known yet becomes the deferred plan, as anywhere.
 */
static PlannedStmt *
plan_forwarded(Query *parse, const char *query_string, int cursorOptions,
			   ParamListInfo boundParams, Analysis *a, const KsCatalog *cat)
{
	ListCell   *lc;
	List	   *relids = NIL;
	bool		pending = false;

	/*
	 * A node that the catalog does not call the primary of its group (the old primary
	 * after promote_node(), a standby) answers no fragment of any kind: its partitions
	 * are a stale copy.  The node that sent the fragment reloads its catalog and goes to
	 * the new primary.
	 */
	if (cat->local_group_id < 0 && cat->local_node_id != 0)
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("this node is no longer the primary of its shard group (stale map version " INT64_FORMAT ")",
						cat->map_version),
				 errhint("The statement can be repeated: it is sent to the current primary of the group.")));

	/* The fragment of a distributed scan of another node: the local buckets only. */
	if (list_length(a->refs) == 1 && query_string != NULL &&
		strncmp(query_string, KS_FRAGMENT_MARKER, strlen(KS_FRAGMENT_MARKER)) == 0 &&
		is_scan_fragment(parse, cat))
		return plan_locally(parse, query_string, cursorOptions, boundParams, cat,
							a->ndist_rtes);

	/* A batch of rows that the row router of another node sends here (11.5). */
	if (list_length(a->refs) == 1 && parse->commandType == CMD_INSERT)
	{
		int64		place;
		int64		ident;
		bool		has_ident;
		DistRef    *only = (DistRef *) linitial(a->refs);

		if (kingres_shard_parse_insert_marker(query_string, &place, &has_ident, &ident) &&
			only->query == parse && only->rti == (Index) parse->resultRelation &&
			is_insert_fragment(parse, cat))
		{
			PlannedStmt *pstmt = plan_locally(parse, query_string, cursorOptions, boundParams,
											  cat, a->ndist_rtes);

			/*
			 * The check of 11.5, after planning (the parent is locked ROW EXCLUSIVE since
			 * parse analysis, which a DETACH of a bucket waits for): the placement number the
			 * sender decided with, or the identity of the partitions of a table that is not
			 * in the catalog.  A fast failure for a stale origin or a changed primary; it is
			 * not what keeps a row out of a wrong bucket, the tuple routing does.
			 */
			if (place != 0 &&
				kingres_shard_placement_hash_now(only->dt->colocation_id) != place)
				ereport(ERROR,
						(errcode(ERRCODE_KINGRES_STALE_MAP),
						 errmsg("the bucket map of this node is not the one the rows were routed with (stale map version " INT64_FORMAT ")",
								cat->map_version),
						 errhint("A bucket moved between shard groups, or the primary of a group changed; repeat the statement.")));
			if (has_ident && kingres_shard_partition_identity_now(only->dt->relid) != ident)
				ereport(ERROR,
						(errcode(ERRCODE_KINGRES_STALE_MAP),
						 errmsg("the partitions of table \"%s\" on this node are not the ones the rows were routed with",
								get_rel_name(only->dt->relid)),
						 errhint("Repeat the statement.")));
			return pstmt;
		}
	}

	/* The part of an UPDATE or DELETE that another node runs on every group. */
	if (list_length(a->refs) == 1 && query_string != NULL &&
		strncmp(query_string, KS_WRITE_MARKER, strlen(KS_WRITE_MARKER)) == 0 &&
		(parse->commandType == CMD_UPDATE || parse->commandType == CMD_DELETE) &&
		!parse->hasModifyingCTE && parse->rowMarks == NIL)
	{
		DistRef    *only = (DistRef *) linitial(a->refs);

		if (only->query == parse && only->rti == (Index) parse->resultRelation &&
			kingres_shard_stmt_shippable(parse, only->dt->relid, NULL))
			return plan_locally(parse, query_string, cursorOptions, boundParams, cat,
								a->ndist_rtes);
	}

	/*
	 * A statement that claims to be a fragment and that none of the rules above accepted: the
	 * node that sent it and this node decide on the same tree, so a retry gives the same
	 * answer and the retryable error KS001 would be a lie (review P2B-02).
	 */
	if (query_string != NULL && list_length(a->refs) == 1 &&
		(strncmp(query_string, KS_FRAGMENT_MARKER, strlen(KS_FRAGMENT_MARKER)) == 0 ||
		 strncmp(query_string, KS_WRITE_MARKER, strlen(KS_WRITE_MARKER)) == 0 ||
		 strncmp(query_string, KS_INSERT_MARKER_PREFIX, strlen(KS_INSERT_MARKER_PREFIX)) == 0))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("this node does not accept the fragment that another node sent it"),
				 errdetail("The statement does not have the shape of a fragment that this node runs, or uses an expression that is not built in and immutable."),
				 errhint("This is not a change of the bucket map: repeating the statement does not help.")));

	foreach(lc, a->refs)
	{
		DistRef    *ref = (DistRef *) lfirst(lc);

		relids = list_append_unique_oid(relids, ref->dt->relid);
		if (ref->param_pending)
		{
			pending = true;
			continue;
		}
		if (ref->bucket >= 0)
		{
			if (kingres_shard_bucket_group(ref->dt->coloc, ref->bucket) != cat->local_group_id)
				ereport(ERROR,
						(errcode(ERRCODE_KINGRES_STALE_MAP),
						 errmsg("bucket %d of %s is not on this node (stale map version " INT64_FORMAT ")",
								ref->bucket, qualified_name(ref->dt->relid),
								cat->map_version)));
			continue;
		}
		if (!all_buckets_local(ref->dt->coloc, cat->local_group_id))
			error_not_provably_local(ref->dt, cat->local_group_id, NULL);
	}

	if (pending)
		return kingres_shard_make_deferred_plan(parse, relids);
	return plan_locally(parse, query_string, cursorOptions, boundParams, cat,
						a->ndist_rtes);
}

/*
 * Count the data-modifying queries below the top one that write a reference
 * table (a WITH clause may hide one).
 */
static bool
nested_reference_write_walker(Node *node, void *context)
{
	const KsCatalog *cat = ((void **) context)[0];
	Query	   *top = ((void **) context)[1];

	if (node == NULL)
		return false;
	if (IsA(node, Query))
	{
		Query	   *q = (Query *) node;

		if (q != top && q->resultRelation > 0 &&
			(q->commandType == CMD_INSERT || q->commandType == CMD_UPDATE ||
			 q->commandType == CMD_DELETE || q->commandType == CMD_MERGE))
		{
			RangeTblEntry *rte = rt_fetch(q->resultRelation, q->rtable);
			KsDistTable *dt = hash_search(cat->dist_tables, &rte->relid, HASH_FIND, NULL);

			if (dt != NULL && dt->is_reference)
				return true;
		}
		return query_tree_walker(q, nested_reference_write_walker, context, 0);
	}
	return expression_tree_walker(node, nested_reference_write_walker, context);
}

/* The reference table the statement writes, or InvalidOid. */
static Oid
reference_write_target(Query *parse, const KsCatalog *cat)
{
	void	   *ctx[2];

	if ((parse->commandType == CMD_INSERT || parse->commandType == CMD_UPDATE ||
		 parse->commandType == CMD_DELETE || parse->commandType == CMD_MERGE) &&
		parse->resultRelation > 0)
	{
		RangeTblEntry *rte = rt_fetch(parse->resultRelation, parse->rtable);
		KsDistTable *dt = hash_search(cat->dist_tables, &rte->relid, HASH_FIND, NULL);

		if (dt != NULL && dt->is_reference)
			return rte->relid;
	}

	ctx[0] = (void *) cat;
	ctx[1] = parse;
	if (query_tree_walker(parse, nested_reference_write_walker, ctx, 0))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a write to a reference table inside a WITH clause or a subquery is not supported yet"),
				 errhint("Run the write to the reference table as a statement of its own.")));
	return InvalidOid;
}

static int
compare_nodes_by_id(const ListCell *a, const ListCell *b)
{
	const KsNode *na = (const KsNode *) lfirst(a);
	const KsNode *nb = (const KsNode *) lfirst(b);

	return na->node_id - nb->node_id;
}

/* The relations a statement reads or writes, in nested queries too. */
static bool
collect_relids_walker(Node *node, List **relids)
{
	if (node == NULL)
		return false;
	if (IsA(node, Query))
	{
		Query	   *q = (Query *) node;
		ListCell   *lc;

		foreach(lc, q->rtable)
		{
			RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

			if (rte->rtekind == RTE_RELATION)
				*relids = list_append_unique_oid(*relids, rte->relid);
		}
		return query_tree_walker(q, collect_relids_walker, relids, 0);
	}
	return expression_tree_walker(node, collect_relids_walker, relids);
}

static int
compare_cstrings(const ListCell *a, const ListCell *b)
{
	return strcmp((char *) lfirst(a), (char *) lfirst(b));
}

/*
 * add_foreign_key_linked
 *
 * Add to the list of relations the reference tables that a write to one of
 * them can reach through the actions of foreign keys (ON DELETE CASCADE, SET
 * NULL, ON UPDATE CASCADE, and the checks that read the other table), in
 * either direction and through any number of keys.  A write that cascades into
 * a second table and a concurrent writer of that table alone would otherwise
 * hold different locks and could interleave differently on different nodes
 * (review finding S3-13).
 */
static List *
add_foreign_key_linked(List *relids, const KsCatalog *cat)
{
	Relation	conrel;
	SysScanDesc scan;
	ScanKeyData key;
	HeapTuple	tup;
	List	   *children = NIL;
	List	   *parents = NIL;
	bool		changed = true;

	conrel = table_open(ConstraintRelationId, AccessShareLock);
	ScanKeyInit(&key, Anum_pg_constraint_contype, BTEqualStrategyNumber,
				F_CHAREQ, CharGetDatum(CONSTRAINT_FOREIGN));
	scan = systable_beginscan(conrel, InvalidOid, false, NULL, 1, &key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		Form_pg_constraint con = (Form_pg_constraint) GETSTRUCT(tup);

		children = lappend_oid(children, con->conrelid);
		parents = lappend_oid(parents, con->confrelid);
	}
	systable_endscan(scan);
	table_close(conrel, AccessShareLock);

	while (changed)
	{
		ListCell   *c;
		ListCell   *p;

		changed = false;
		forboth(c, children, p, parents)
		{
			Oid			child = lfirst_oid(c);
			Oid			parent = lfirst_oid(p);
			bool		has_child = list_member_oid(relids, child);
			bool		has_parent = list_member_oid(relids, parent);
			Oid			other = has_child && !has_parent ? parent :
				(has_parent && !has_child ? child : InvalidOid);
			KsDistTable *dt;

			if (!OidIsValid(other))
				continue;
			dt = hash_search(cat->dist_tables, &other, HASH_FIND, NULL);
			if (dt != NULL && dt->is_reference)
			{
				relids = lappend_oid(relids, other);
				changed = true;
			}
		}
	}
	return relids;
}

/*
 * The qualified names of the reference tables a statement uses and can reach
 * through foreign keys, sorted: the writers of each of them are serialized on
 * every node (S2-06, S3-13).
 */
static List *
reference_lock_names(Query *parse, const KsCatalog *cat)
{
	List	   *relids = NIL;
	List	   *names = NIL;
	ListCell   *lc;

	(void) collect_relids_walker((Node *) parse, &relids);
	relids = add_foreign_key_linked(relids, cat);
	foreach(lc, relids)
	{
		Oid			relid = lfirst_oid(lc);

		names = lappend(names,
						quote_qualified_identifier(get_namespace_name(get_rel_namespace(relid)),
												   get_rel_name(relid)));
	}
	list_sort(names, compare_cstrings);
	return names;
}

/*
 * plan_reference_write
 *
 * A write to a reference table runs on every active primary node, in one
 * distributed transaction (design/shard-phase2a.md, section 3), so it must
 * produce the same rows everywhere: it is shipped only if every expression in
 * it is built in and immutable (no volatile function, no sequence default, and
 * also no now(), which differs between nodes), and it reads no table that is
 * not a reference table.
 */
static PlannedStmt *
plan_reference_write(Query *parse, Oid target, const KsCatalog *cat)
{
	const char *reason = NULL;
	List	   *nodes = NIL;
	List	   *targets = NIL;
	ListCell   *lc;
	char	   *sql;

	if (parse->commandType == CMD_MERGE)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("MERGE into a reference table is not supported yet")));
	if (!kingres_shard_stmt_shippable(parse, target, &reason))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("this write to reference table \"%s\" cannot be run on every node",
						get_rel_name(target)),
				 errdetail("The statement is not shippable: %s.", reason),
				 errhint("A write to a reference table runs on every node, so it may use only constants, parameters and built-in immutable functions (not a sequence, random() or now()), and other reference tables.")));
	if (IsInParallelMode())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a write to a reference table cannot be run from a parallel worker")));

	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);

		if (n->is_active && n->is_primary)
			nodes = lappend(nodes, n);
	}
	list_sort(nodes, compare_nodes_by_id);
	foreach(lc, nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		KsFanTarget *t = palloc(sizeof(KsFanTarget));

		t->host = n->host;
		t->port = n->port;
		t->is_local = (n->node_id == cat->local_node_id);
		targets = lappend(targets, t);
	}

	sql = psprintf("%s%s", KS_REFWRITE_MARKER, deparse_for_remote(parse));
	return kingres_shard_make_fanout_plan(parse, sql, targets, false,
										   list_make1_oid(target),
										   reference_lock_names(parse, cat));
}

/*
 * can_distributed_scan
 *
 * May a statement that spans shard groups be planned with the distributed scan?
 * Only a SELECT without row locking and without a data-modifying WITH clause.
 */
static bool
writes_hash_dist_walker(Node *node, void *context)
{
	const KsCatalog *cat = (const KsCatalog *) context;

	if (node == NULL)
		return false;
	if (IsA(node, Query))
	{
		Query	   *q = (Query *) node;

		if (q->resultRelation > 0 &&
			(q->commandType == CMD_INSERT || q->commandType == CMD_UPDATE ||
			 q->commandType == CMD_DELETE || q->commandType == CMD_MERGE))
		{
			RangeTblEntry *rte = rt_fetch(q->resultRelation, q->rtable);
			KsDistTable *dt = hash_search(cat->dist_tables, &rte->relid, HASH_FIND, NULL);

			if (dt != NULL && !dt->is_reference)
				return true;
		}
		return query_tree_walker(q, writes_hash_dist_walker, context, 0);
	}
	return expression_tree_walker(node, writes_hash_dist_walker, context);
}

static bool
can_distributed_scan(Query *parse, const KsCatalog *cat, bool spans)
{
	if (!kingres_shard_enable_distributed_scan)
		return false;
	/* A statement that writes a distributed table is not planned by reading it all. */
	if (writes_hash_dist_walker((Node *) parse, (void *) cat))
		return false;
	if (parse->rowMarks != NIL)
	{
		if (spans)
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("row-level locking on a statement that spans shard groups is not supported yet"),
					 errhint("Restrict every distributed table of the statement to one key, or remove FOR UPDATE / FOR SHARE.")));
		/* review P2B-07: the statement names one key, but a local table cannot be sent to the other group */
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a local table cannot be read in a statement that locks rows of a distributed table of another shard group"),
				 errhint("Read the local table in a statement of its own, or remove FOR UPDATE / FOR SHARE.")));
	}
	return true;
}

/*
 * plan_with_scan
 *
 * Plan a statement with the standard planner while the set_rel_pathlist_hook of
 * dist_scan.c replaces the paths of the distributed tables by the distributed
 * scan.  The planner is not allowed to join or aggregate partition by
 * partition: that would read the local partitions only.
 */
static PlannedStmt *
plan_with_scan(Query *parse, const char *query_string, int cursorOptions,
			   ParamListInfo boundParams)
{
	PlannedStmt *pstmt;
	int			nest = NewGUCNestLevel();

	(void) set_config_option("enable_partitionwise_join", "off", PGC_SUSET,
							 PGC_S_SESSION, GUC_ACTION_SAVE, true, 0, false);
	(void) set_config_option("enable_partitionwise_aggregate", "off", PGC_SUSET,
							 PGC_S_SESSION, GUC_ACTION_SAVE, true, 0, false);
	(void) kingres_shard_distscan_set_active(true);
	PG_TRY();
	{
		pstmt = pass_through(parse, query_string, cursorOptions, boundParams);
	}
	PG_FINALLY();
	{
		(void) kingres_shard_distscan_set_active(false);
	}
	PG_END_TRY();
	AtEOXact_GUC(true, nest);
	return pstmt;
}

/*
 * is_scan_fragment
 *
 * Is the statement of a forwarded session the fragment of a distributed scan:
 * a plain read of one distributed table, returning columns of it, restricted
 * only by clauses that are safe to ship?  Such a statement means "the rows of
 * the buckets of this node that satisfy the restriction" and nothing else, so
 * it is correct to run it here whatever buckets it names, and it is how the
 * other groups read their part of the table.
 */
static bool
is_scan_fragment(Query *q, const KsCatalog *cat)
{
	RangeTblEntry *rte;
	KsDistTable *dt;
	FromExpr   *f = q->jointree;
	ListCell   *lc;

	if (q->commandType != CMD_SELECT || q->cteList != NIL || q->setOperations != NULL ||
		q->hasSubLinks || q->hasAggs || q->hasWindowFuncs || q->hasTargetSRFs ||
		q->hasRecursive || q->groupClause != NIL || q->groupingSets != NIL ||
		q->havingQual != NULL || q->distinctClause != NIL || q->sortClause != NIL ||
		q->limitCount != NULL || q->limitOffset != NULL || q->rowMarks != NIL)
		return false;
	if (list_length(q->rtable) != 1)
		return false;
	rte = (RangeTblEntry *) linitial(q->rtable);
	if (rte->rtekind != RTE_RELATION)
		return false;
	dt = hash_search(cat->dist_tables, &rte->relid, HASH_FIND, NULL);
	if (dt == NULL || dt->is_reference)
		return false;
	if (f == NULL || list_length(f->fromlist) != 1 || !IsA(linitial(f->fromlist), RangeTblRef))
		return false;
	foreach(lc, q->targetList)
	{
		TargetEntry *te = lfirst_node(TargetEntry, lc);
		Node	   *e = (Node *) te->expr;

		if (IsA(e, Const))
			continue;
		if (IsA(e, Var) && ((Var *) e)->varno == 1 && ((Var *) e)->varlevelsup == 0 &&
			((Var *) e)->varattno >= 0)
			continue;
		return false;
	}
	if (f->quals != NULL && !kingres_shard_clause_shippable(f->quals, 1))
		return false;
	return true;
}

/*
 * An expression of the SELECT of an insert fragment: a column of the unnest, read through
 * the casts from text that the sender writes.
 */
static bool
insert_src_expr_ok(Node *e)
{
	if (e == NULL)
		return false;
	switch (nodeTag(e))
	{
		case T_Var:
			return ((Var *) e)->varno == 1 && ((Var *) e)->varlevelsup == 0 && ((Var *) e)->varattno > 0;
		case T_CoerceViaIO:
			return insert_src_expr_ok((Node *) ((CoerceViaIO *) e)->arg);
		case T_RelabelType:
			return insert_src_expr_ok((Node *) ((RelabelType *) e)->arg);
		case T_CoerceToDomain:
			return insert_src_expr_ok((Node *) ((CoerceToDomain *) e)->arg);
		case T_FuncExpr:
			{
				/* a cast function of the catalog (text to char(n) is one), on what is allowed */
				FuncExpr   *fe = (FuncExpr *) e;
				ListCell   *lc;

				if (fe->funcid >= FirstNormalObjectId)
					return false;
				foreach(lc, fe->args)
				{
					Node	   *a = (Node *) lfirst(lc);

					if (!IsA(a, Const) && !insert_src_expr_ok(a))
						return false;
				}
				return true;
			}
		default:
			return false;
	}
}

/* Does the query contain a Var of the first relation whose RETURNING kind is not the default? */
static bool
frag_returning_var_walker(Node *node, void *context)
{
	if (node == NULL)
		return false;
	if (IsA(node, Var) && ((Var *) node)->varreturningtype != VAR_RETURNING_DEFAULT)
		return true;
	return expression_tree_walker(node, frag_returning_var_walker, context);
}

/*
 * is_insert_fragment
 *
 * Is the statement of a forwarded session the batch of the row router of another node (11.3,
 * 11.5): an INSERT into one distributed table whose only other relation is the multi-argument
 * unnest of external parameters, with ON CONFLICT DO NOTHING or nothing, and a RETURNING list
 * that passes the shippability walker?  A statement of this shape means "put these rows in the
 * buckets of this node", the rows that do not belong fail in the tuple routing, so it is
 * correct to run it here whatever keys the rows have.  The group computes no default of its
 * own for it that is not a constant (a volatile one would be computed on the wrong node).
 */
static bool
is_insert_fragment(Query *q, const KsCatalog *cat)
{
	RangeTblEntry *trte;
	RangeTblEntry *srte;
	KsDistTable *dt;
	Query	   *sub;
	RangeTblEntry *frte;
	ListCell   *lc;

	if (q->commandType != CMD_INSERT || q->resultRelation != 1 || q->cteList != NIL ||
		q->hasModifyingCTE || q->hasSubLinks || q->rowMarks != NIL ||
		list_length(q->rtable) != 2)
		return false;
	trte = (RangeTblEntry *) linitial(q->rtable);
	srte = (RangeTblEntry *) lsecond(q->rtable);
	if (trte->rtekind != RTE_RELATION || srte->rtekind != RTE_SUBQUERY)
		return false;
	dt = hash_search(cat->dist_tables, &trte->relid, HASH_FIND, NULL);
	if (dt == NULL || dt->is_reference)
		return false;

	sub = srte->subquery;
	if (sub->commandType != CMD_SELECT || sub->cteList != NIL || sub->setOperations != NULL ||
		sub->hasSubLinks || sub->hasAggs || sub->hasWindowFuncs || sub->hasTargetSRFs ||
		sub->groupClause != NIL || sub->havingQual != NULL || sub->distinctClause != NIL ||
		sub->sortClause != NIL || sub->limitCount != NULL || sub->limitOffset != NULL ||
		sub->rowMarks != NIL || list_length(sub->rtable) != 1 ||
		sub->jointree == NULL || sub->jointree->quals != NULL)
		return false;
	frte = (RangeTblEntry *) linitial(sub->rtable);
	if (frte->rtekind != RTE_FUNCTION || frte->funcordinality || frte->functions == NIL)
		return false;
	foreach(lc, frte->functions)
	{
		RangeTblFunction *rtf = (RangeTblFunction *) lfirst(lc);
		FuncExpr   *fe = (FuncExpr *) rtf->funcexpr;
		Node	   *arg;

		if (fe == NULL || !IsA(fe, FuncExpr) || fe->funcid != F_UNNEST_ANYARRAY ||
			list_length(fe->args) != 1)
			return false;
		arg = strip_relabel((Node *) linitial(fe->args));
		if (arg == NULL || !IsA(arg, Param) || ((Param *) arg)->paramkind != PARAM_EXTERN)
			return false;
	}
	foreach(lc, sub->targetList)
	{
		TargetEntry *te = lfirst_node(TargetEntry, lc);

		if (!insert_src_expr_ok((Node *) te->expr))
			return false;
	}
	/* What the INSERT does with them: Vars of the source and constants (the defaults of the group), nothing volatile. */
	foreach(lc, q->targetList)
	{
		TargetEntry *te = lfirst_node(TargetEntry, lc);
		List	   *vars = pull_var_clause((Node *) te->expr, 0);
		ListCell   *lc2;

		if (contain_volatile_functions((Node *) te->expr))
			return false;
		foreach(lc2, vars)
			if (((Var *) lfirst(lc2))->varno != 2 || ((Var *) lfirst(lc2))->varlevelsup != 0)
				return false;
	}
	if (q->onConflict != NULL)
	{
		OnConflictExpr *oc = q->onConflict;

		if (oc->action != ONCONFLICT_NOTHING || KS_ONCONFLICT_IS_SELECT(oc))
			return false;
		foreach(lc, oc->arbiterElems)
		{
			InferenceElem *ie = (InferenceElem *) lfirst(lc);
			Node	   *e = (Node *) ie->expr;

			if (!(IsA(e, Var) && ((Var *) e)->varno == 1 && ((Var *) e)->varlevelsup == 0) &&
				!kingres_shard_clause_shippable(e, 1))
				return false;
		}
		if (oc->arbiterWhere != NULL && !kingres_shard_clause_shippable(oc->arbiterWhere, 1))
			return false;
	}
	if (q->returningList != NIL)
	{
		if (frag_returning_var_walker((Node *) q->returningList, NULL))
			return false;
		foreach(lc, q->returningList)
		{
			TargetEntry *te = lfirst_node(TargetEntry, lc);
			Node	   *e = (Node *) te->expr;

			if (!(IsA(e, Var) && ((Var *) e)->varno == 1 && ((Var *) e)->varlevelsup == 0 &&
				  ((Var *) e)->varattno > 0) &&
				!kingres_shard_clause_shippable(e, 1))
				return false;
		}
	}
	return true;
}

/*
 * A data-modifying query below (or at) the top that writes a bucket partition
 * itself: the partition of a hash-distributed table, named directly.
 */
static bool
bucket_partition_write_walker(Node *node, void *context)
{
	const KsCatalog *cat = (const KsCatalog *) context;

	if (node == NULL)
		return false;
	if (IsA(node, Query))
	{
		Query	   *q = (Query *) node;

		if (q->resultRelation > 0 &&
			(q->commandType == CMD_INSERT || q->commandType == CMD_UPDATE ||
			 q->commandType == CMD_DELETE || q->commandType == CMD_MERGE))
		{
			RangeTblEntry *rte = rt_fetch(q->resultRelation, q->rtable);

			if (rte->rtekind == RTE_RELATION && get_rel_relispartition(rte->relid))
			{
				Oid			parent = get_partition_parent(rte->relid, true);

				if (OidIsValid(parent))
				{
					KsDistTable *dt = hash_search(cat->dist_tables, &parent, HASH_FIND, NULL);

					if (dt != NULL && !dt->is_reference)
						return true;
				}
			}
		}
		return query_tree_walker(q, bucket_partition_write_walker, context, 0);
	}
	return expression_tree_walker(node, bucket_partition_write_walker, context);
}

/*
 * refuse_bucket_partition_write
 *
 * A write that names a bucket partition (INSERT INTO orders_b0002 ...) is not a
 * statement of the user's table: the partition is the extension's storage.  It
 * takes no lock on the parent, so it is not routed by key, is not seen by the
 * switch of a bucket move as a writer of the table, and a row written that way
 * to a bucket that is leaving ends in the orphan table.  The move itself is safe
 * against it (the switch locks the partitions of the bucket, bucket_move.c), but a
 * write that is refused here fails with a reason instead of waiting for, or being
 * undone by, a move.  Only a superuser who has switched kingres_shard.enable_routing
 * off (to repeat a statement on every node by hand) gets past this check; the
 * lock of the switch holds for that case as well (review finding S4-03).
 */
static void
refuse_bucket_partition_write(Query *parse, const KsCatalog *cat)
{
	if (bucket_partition_write_walker((Node *) parse, (void *) cat))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("cannot write a bucket partition of a distributed table directly"),
				 errdetail("The partitions of a distributed table are the storage of its buckets; a write to one skips the routing by key and the locking of a bucket move."),
				 errhint("Write the distributed table itself.")));
}


/*
 * The hook proper.  Whatever it decides for one statement is not inherited by
 * the statements planned while it plans (a function called during constant
 * folding may plan its own): the distributed scan is switched on only around
 * the standard planner of a statement that needs it.
 */
/* Is the statement this call of the planner hook plans the local part that the caller armed? */
static bool this_is_local_part = false;

static PlannedStmt *
kingres_shard_planner_hook(Query *parse, const char *query_string,
							int cursorOptions, ParamListInfo boundParams KS_PLANNER_ES_PARAM)
{
	PlannedStmt *result;
	bool		old = kingres_shard_distscan_set_active(false);
	void	   *old_explain = current_explain;
	bool		saved_local_part = this_is_local_part;

	current_explain = (void *) KS_PLANNER_ES_VALUE;
	/* consumed first of all: nothing this hook runs can see the mark (fanout.c, P2B-01) */
	this_is_local_part = kingres_shard_local_part_take(query_string);
	PG_TRY();
	{
		result = planner_hook_body(parse, query_string, cursorOptions, boundParams);
	}
	PG_FINALLY();
	{
		(void) kingres_shard_distscan_set_active(old);
		current_explain = old_explain;
		this_is_local_part = saved_local_part;
	}
	PG_END_TRY();
	return result;
}

/*
 * kingres_shard_planner_hook
 *
 * See the file header.
 */
static PlannedStmt *
planner_hook_body(Query *parse, const char *query_string,
							int cursorOptions, ParamListInfo boundParams)
{
	const KsCatalog *cat;
	Analysis	a;
	void	   *ctx[2];
	ListCell   *lc;
	DistRef    *first;
	int			colocation_id;
	int			bucket = -1;
	bool		pending = false;
	int			group;
	bool		spans = false;
	DistRef    *span_ref = NULL;
	const char *span_detail = NULL;
	List	   *relids = NIL;
	bool		is_write;

	/* A session that claims to be forwarded must prove it before anything runs. */
	(void) kingres_shard_session_forwarded();

	/*
	 * A statement of a trigger of a foreign key (S6-01) is planned for this node
	 * only.  (The local part of a fanned-out statement is too, but it is
	 * recognized further down, after the analysis and after the partition lists
	 * were made as new as the catalog: design 4.4.)
	 */
	if (kingres_shard_enable_routing && !creating_extension &&
		kingres_shard_in_ri_action(parse))
	{
		/*
		 * The partition list of the relcache must be as new as the catalog before the
		 * local part is planned: after a move or a promotion a plan made on an older
		 * list writes to the wrong set of partitions.
		 */
		const KsCatalog *fc = kingres_shard_catalog();
		ListCell   *flc;

		if (fc->present && fc->dist_tables != NULL)
			foreach(flc, parse->rtable)
			{
				RangeTblEntry *frte = (RangeTblEntry *) lfirst(flc);
				KsDistTable *fd;

				if (frte->rtekind != RTE_RELATION)
					continue;
				fd = hash_search(fc->dist_tables, &frte->relid, HASH_FIND, NULL);
				if (fd != NULL && !fd->is_reference)
					kingres_shard_ensure_fresh_partitions(frte->relid, fc->map_version);
			}
		return pass_through(parse, query_string, cursorOptions, boundParams);
	}
	/* The statements PostgreSQL runs for a propagated DDL work on this node's buckets. */
	if (kingres_shard_in_authorized_ddl(query_string))
		return pass_through(parse, query_string, cursorOptions, boundParams);
	if (!kingres_shard_enable_routing || creating_extension)
		return pass_through(parse, query_string, cursorOptions, boundParams);
	if (parse->commandType != CMD_SELECT && parse->commandType != CMD_INSERT &&
		parse->commandType != CMD_UPDATE && parse->commandType != CMD_DELETE &&
		parse->commandType != CMD_MERGE)
		return pass_through(parse, query_string, cursorOptions, boundParams);

	/* The window of shard-phase4.md, "A routed statement on a target"; forced by t/033. */
	kingres_shard_fault_point("route_before_catalog");
	cat = kingres_shard_catalog();
	if (!cat->present || cat->ndist_tables == 0 || cat->local_group_id == -1)
		return pass_through(parse, query_string, cursorOptions, boundParams);

	/* With one shard group every bucket is local. */
	if (cat->ngroups <= 1 && cat->local_group_id >= 0 && !kingres_shard_session_forwarded())
		return pass_through(parse, query_string, cursorOptions, boundParams);

	refuse_bucket_partition_write(parse, cat);

	memset(&a, 0, sizeof(a));
	a.params = boundParams;
	a.parse = parse;
	ctx[0] = &a;
	ctx[1] = (void *) cat;
	(void) analyze_walker((Node *) parse, ctx);

	/* The partition lists must be as new as the catalog cache just read. */
	foreach(lc, a.refs)
		kingres_shard_ensure_fresh_partitions(((DistRef *) lfirst(lc))->dt->relid,
											  cat->map_version);

	/*
	 * The local part of a fanned-out statement or of a batch, which this backend runs
	 * through SPI after arming the one-shot mark (fanout.c), is planned for this node
	 * only, without routing and without the scan.  The mark was consumed at the entry
	 * of the hook; it is acted on here and not earlier: the partition lists of its
	 * tables are now as new as the catalog (above).  Nothing in the text of the
	 * statement says it is a local part, so a function that the local part calls
	 * cannot make one (review P2B-01).
	 */
	if (this_is_local_part)
		return pass_through(parse, query_string, cursorOptions, boundParams);

	/*
	 * A write to a reference table runs on every node; it takes no part in the
	 * routing by key, and it cannot be combined with a distributed table.
	 */
	if (a.has_ref && !kingres_shard_session_forwarded())
	{
		Oid			reftarget = reference_write_target(parse, cat);

		if (OidIsValid(reftarget))
		{
			if (a.refs != NIL)
				ereport(ERROR,
						(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
						 errmsg("a statement that writes reference table \"%s\" and reads a distributed table is not supported yet",
								get_rel_name(reftarget))));
			return plan_reference_write(parse, reftarget, cat);
		}
	}

	if (a.refs == NIL)
	{
		/*
		 * No distributed table in the statement as written: the planner may
		 * still find one (it expands a SQL-language function into the
		 * statement).  The distributed scan reads such a table correctly.
		 */
		if (!kingres_shard_session_forwarded() && kingres_shard_enable_distributed_scan)
			return plan_with_scan(parse, query_string, cursorOptions, boundParams);
		return plan_locally(parse, query_string, cursorOptions, boundParams,
							cat, a.ndist_rtes);
	}

	if (kingres_shard_session_forwarded())
		return plan_forwarded(parse, query_string, cursorOptions, boundParams,
							  &a, cat);

	/* A row cannot change its key: it might have to move to another shard group. */
	if (parse->commandType == CMD_UPDATE && parse->resultRelation > 0)
	{
		foreach(lc, a.refs)
		{
			DistRef    *ref = (DistRef *) lfirst(lc);
			ListCell   *lc2;

			if (ref->query != parse || ref->rti != (Index) parse->resultRelation)
				continue;
			foreach(lc2, parse->targetList)
			{
				TargetEntry *te = lfirst_node(TargetEntry, lc2);

				if (!te->resjunk && te->resno == ref->dt->dist_attno)
					ereport(ERROR,
							(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
							 errmsg("an UPDATE of the distribution column \"%s\" of \"%s\" is not supported",
									NameStr(ref->dt->dist_column), get_rel_name(ref->dt->relid)),
							 errdetail("The row would have to move to another bucket, possibly of another shard group."),
							 errhint("DELETE the row and INSERT it with the new key.")));
			}
		}
	}

	/*
	 * An INSERT into a distributed table whose key is not one constant (design 11.1): a
	 * multi-row VALUES, INSERT ... SELECT, a row whose key is computed.  It is run by the row
	 * router, which sends each row to the group that owns its bucket.  A single row with a
	 * constant key (or a parameter whose value the plan does not know yet) stays on the
	 * paths below, and with enable_bulk_routing off the statement is refused as in 2a.
	 */
	if (parse->commandType == CMD_INSERT && parse->resultRelation > 0 &&
		kingres_shard_enable_bulk_routing)
	{
		DistRef    *target = NULL;
		List	   *rrelids = NIL;

		foreach(lc, a.refs)
		{
			DistRef    *ref = (DistRef *) lfirst(lc);

			rrelids = list_append_unique_oid(rrelids, ref->dt->relid);
			if (ref->query == parse && ref->rti == (Index) parse->resultRelation)
				target = ref;
		}
		if (target != NULL && target->bucket < 0 && !target->param_pending)
			return kingres_shard_plan_row_router(parse, target->dt, rrelids);
	}

	first = (DistRef *) linitial(a.refs);
	colocation_id = first->dt->colocation_id;

	foreach(lc, a.refs)
	{
		DistRef    *ref = (DistRef *) lfirst(lc);

		relids = list_append_unique_oid(relids, ref->dt->relid);
		if (spans)
			continue;
		if (ref->dt->colocation_id != colocation_id)
		{
			spans = true;
			span_ref = ref;
			span_detail = "The statement uses distributed tables of different colocation groups.";
		}
		else if (ref->bucket < 0 && !ref->param_pending)
		{
			spans = true;
			span_ref = ref;
		}
		else if (ref->param_pending)
			pending = true;
		else if (bucket < 0)
			bucket = ref->bucket;
		else if (bucket != ref->bucket)
		{
			spans = true;
			span_ref = ref;
			span_detail = "The keys of the statement hash to different buckets.";
		}
	}

	if (spans)
	{
		/*
		 * The statement spans shard groups.  A SELECT is planned as if all the
		 * data were here, with the distributed scan fetching the rows of the
		 * other groups (dist_scan.c); a write cannot be (a multi-row INSERT,
		 * INSERT ... SELECT) or is handled by the planner of the writes below.
		 */
		if (can_distributed_scan(parse, cat, true))
			return plan_with_scan(parse, query_string, cursorOptions, boundParams);
		if (parse->commandType == CMD_UPDATE || parse->commandType == CMD_DELETE)
		{
			if (list_length(a.refs) == 1 && span_ref->query == parse &&
				span_ref->rti == (Index) parse->resultRelation && span_detail == NULL)
				return plan_distributed_write(parse, span_ref, cat, relids);
			error_write_spans_groups(parse, span_ref,
									 span_detail ? span_detail :
									 "The statement reads a distributed table other than as its target (a subquery, a join, a self join).");
		}
		error_spans_groups(span_ref, span_detail);
	}

	if (pending)
	{
		/* The routing depends on a parameter whose value is not known yet. */
		return kingres_shard_make_deferred_plan(parse, relids);
	}

	group = kingres_shard_bucket_group(first->dt->coloc, bucket);
	if (group < 0)
		elog(ERROR, "bucket %d of colocation group %d has no owner",
			 bucket, colocation_id);

	if (group == cat->local_group_id)
		return plan_locally(parse, query_string, cursorOptions, boundParams,
							cat, a.ndist_rtes);

	/* The statement belongs to another group: forward it. */
	if (a.has_merge)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("MERGE on distributed tables of another shard group is not supported yet")));

	/*
	 * A read that mixes a distributed table of another group with a local table,
	 * and a scrollable cursor on a statement of another group, are answered by the
	 * distributed scan (design 6.5, ADR 0022): the local table does not exist on
	 * the other node, so the statement cannot be forwarded whole, but the scan
	 * reads the distributed table (asking only the group the restriction names,
	 * 4.5) and PostgreSQL joins it with the local table here.  Every restriction is
	 * applied again at the origin.  A scrollable cursor gets a Material node above
	 * the scan, which cannot be read backwards.  A statement that writes a
	 * distributed table stays refused below.
	 */
	if ((a.has_nondist || (cursorOptions & CURSOR_OPT_SCROLL)) &&
		can_distributed_scan(parse, cat, false))
		return plan_with_scan(parse, query_string, cursorOptions, boundParams);
	if (a.has_nondist)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("statements that combine distributed tables of another shard group with local tables are not supported yet")));
	if (cursorOptions & CURSOR_OPT_SCROLL)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("scrollable cursors on statements of another shard group are not supported yet")));

	/*
	 * A parallel worker has a snapshot and a transaction of its own on the
	 * remote node, which would not see what the leader's remote transaction
	 * did, so a statement is forwarded only from the leader.
	 */
	if (IsInParallelMode())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a statement for another shard group cannot be forwarded from a parallel worker"),
				 errhint("Mark the function that runs the statement PARALLEL RESTRICTED or PARALLEL UNSAFE.")));

	{
		const KsNode *node = NULL;
		char	   *sql;

		foreach(lc, cat->nodes)
		{
			KsNode	   *n = (KsNode *) lfirst(lc);

			if (n->group_id == group && n->is_primary && n->is_active)
			{
				node = n;
				break;
			}
		}
		if (node == NULL)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("shard group %d has no active primary node", group)));

		foreach(lc, a.refs)
		{
			DistRef    *vr = (DistRef *) lfirst(lc);

			kingres_shard_refuse_view_owner(vr->query, rt_fetch(vr->rti, vr->query->rtable));
		}
		/* A routed SELECT that calls a volatile function may write on the other node: it is a writer (S2-19). */
		is_write = parse->commandType != CMD_SELECT || parse->rowMarks != NIL ||
			parse->hasModifyingCTE ||
			contain_volatile_functions((Node *) parse);
		sql = deparse_for_remote(parse);
		return kingres_shard_make_remote_plan(parse, sql, node->host,
											   node->port, group, bucket,
											   is_write, relids);
	}
}
