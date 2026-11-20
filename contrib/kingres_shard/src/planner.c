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
#include "catalog/namespace.h"
#include "catalog/partition.h"
#include "catalog/pg_namespace_d.h"
#include "catalog/pg_aggregate.h"
#include "catalog/pg_collation_d.h"
#include "catalog/pg_constraint.h"
#include "common/int.h"
#include "catalog/pg_type_d.h"
#include "catalog/pg_language_d.h"
#include "catalog/pg_proc.h"
#include "commands/extension.h"
#include "miscadmin.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "nodes/params.h"
#include "nodes/pathnodes.h"
#include "optimizer/optimizer.h"
#include "optimizer/clauses.h"
#include "optimizer/planner.h"
#include "parser/analyze.h"
#include "parser/parse_relation.h"
#include "parser/parsetree.h"
#include "rewrite/rewriteManip.h"
#include "tcop/tcopprot.h"
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
#include "fragment_scan.h"
#include "forward_auth.h"
#include "hashing.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "pg_compat.h"
#include "planner.h"
#include "pushdown.h"
#include "repartition.h"
#include "remote_scan.h"
#include "row_router.h"
#include "schema_ddl.h"
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
		/*
		 * FROM ONLY a distributed table (review P2D-03): a distributed table is one table to its user, whose rows
		 * are in the partitions of every group, so ONLY has no meaning for it; the statement is refused on every
		 * path instead of reading the rows of the parent alone, or of one group.  The target of an INSERT has no
		 * inheritance flag and is not a scan.
		 */
		if (dt != NULL && !rte->inh && !(q->commandType == CMD_INSERT && rti == (Index) q->resultRelation))
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("FROM ONLY is not supported for distributed table \"%s\"", get_rel_name(rte->relid)),
					 errhint("A distributed table is one table whose rows are in the partitions of every shard group; leave out ONLY.")));
		/*
		 * TABLESAMPLE of a distributed table (review P2E-02): the scan asks every group for its rows and applies no sampling to the rows of
		 * the other groups, so the sample would be that of this node's partitions plus every row of the others.  A sample of a table that is
		 * spread over groups is not one sample of the table, so it is refused on every path instead of answering with another table.
		 */
		if (dt != NULL && rte->tablesample != NULL)
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("TABLESAMPLE is not supported for distributed table \"%s\"", get_rel_name(rte->relid)),
					 errhint("Sample a copy of the rows (for example a table made with CREATE TABLE AS), or filter the rows with a random() condition.")));
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
/*
 * insert_target_has_identity_always
 *
 * The rewriter put the default of a GENERATED ALWAYS identity column into the target list of an INSERT that did
 * not name it, and the deparser prints that default as nextval('sequence').  The node that gets the text takes it
 * for a value the user wrote, and refuses it unless the INSERT says OVERRIDING SYSTEM VALUE.
 */
static bool
insert_target_has_identity_always(Query *query)
{
	RangeTblEntry *rte;
	Relation	rel;
	bool		found = false;

	if (query->resultRelation <= 0)
		return false;
	rte = rt_fetch(query->resultRelation, query->rtable);
	if (rte->rtekind != RTE_RELATION)
		return false;
	rel = table_open(rte->relid, NoLock);
	for (int i = 0; i < rel->rd_att->natts; i++)
	{
		if (TupleDescAttr(rel->rd_att, i)->attidentity == ATTRIBUTE_IDENTITY_ALWAYS)
		{
			found = true;
			break;
		}
	}
	table_close(rel, NoLock);
	return found;
}

static char *
deparse_for_remote(Query *query)
{
	int			nest = kingres_shard_set_transmission_modes();
	char	   *sql;

	if (query->commandType == CMD_INSERT)
	{
		if (query->override == OVERRIDING_NOT_SET && insert_target_has_identity_always(query))
			query->override = OVERRIDING_SYSTEM_VALUE;
		query = wrap_insert_source(query);
	}
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
 * fanout_targets
 *
 * The primary node of every shard group that owns buckets of the colocation group, in
 * group order (the same order on every node, so that concurrent statements take their
 * locks in the same order).  The group of this node is the local one.
 */
static List *
fanout_targets(const KsColocation *coloc, const KsCatalog *cat)
{
	List	   *groups = NIL;
	List	   *targets = NIL;
	ListCell   *lc;
	int			b;

	for (b = 0; b < kingres_shard_coloc_nbuckets(coloc); b++)
	{
		int			g = kingres_shard_bucket_group(coloc, kingres_shard_coloc_bucket(coloc, b));

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
	return targets;
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
	List	   *targets = NIL;
	ListCell   *lc;
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

	targets = fanout_targets(ref->dt->coloc, cat);

	sql = psprintf("%s%s", KS_WRITE_MARKER, deparse_for_remote(parse));
	return kingres_shard_make_fanout_plan(parse, sql, targets, true, relids, NIL);
}

/* The types of the external parameters of a statement: the groups parse its text with them. */
typedef struct ParamTypes
{
	Oid		   *types;
	int			n;
} ParamTypes;

static bool
param_types_walker(Node *node, ParamTypes *pt)
{
	if (node == NULL)
		return false;
	if (IsA(node, Param))
	{
		Param	   *p = (Param *) node;

		if (p->paramkind == PARAM_EXTERN && p->paramid > 0)
		{
			if (p->paramid > pt->n)
			{
				int			i;

				pt->types = pt->types ? repalloc(pt->types, sizeof(Oid) * p->paramid) :
					palloc(sizeof(Oid) * p->paramid);
				for (i = pt->n; i < p->paramid; i++)
					pt->types[i] = InvalidOid;
				pt->n = p->paramid;
			}
			pt->types[p->paramid - 1] = p->paramtype;
		}
		return false;
	}
	if (IsA(node, Query))
		return query_tree_walker((Query *) node, param_types_walker, pt, 0);
	return expression_tree_walker(node, param_types_walker, pt);
}

/*
 * insert_select_text_accepted
 *
 * The decision of a receiving group is made on the parse of the text it receives, not on
 * the tree this node deparsed (design: "the receiving side decides on text that was
 * deparsed and parsed again").  So that a statement that is shipped is never refused
 * there, the text is parsed and rewritten here, under the same settings, and the same
 * function decides on it.  Anything a deparse and a parse change in the meaning of the
 * statement (the shape of a join, a default, a cast) shows up here as a refusal, and the
 * statement goes to the row router.
 */
static bool
insert_select_text_accepted(const char *body, Query *orig, const char **why)
{
	ParamTypes	pt = {NULL, 0};
	List	   *raw;
	List	   *rewritten;
	Query	   *rq;
	KsInsertSelect is2;
	int			nest;
	int			i;

	(void) param_types_walker((Node *) orig, &pt);
	for (i = 0; i < pt.n; i++)
		if (!OidIsValid(pt.types[i]))
			pt.types[i] = TEXTOID;

	/* the window between the deparse and the parse of the text (P2D-02) */
	kingres_shard_fault_point("insel_before_text_parse");
	nest = kingres_shard_set_transmission_modes();
	raw = pg_parse_query(body);
	if (list_length(raw) != 1)
	{
		kingres_shard_reset_transmission_modes(nest);
		*why = "the deparsed text is not one statement";
		return false;
	}
	rq = parse_analyze_fixedparams(linitial_node(RawStmt, raw), body, pt.types, pt.n, NULL);
	rewritten = pg_rewrite_query(rq);
	kingres_shard_reset_transmission_modes(nest);
	if (list_length(rewritten) != 1)
	{
		*why = "the deparsed text is rewritten into several statements";
		return false;
	}
	return kingres_shard_insert_select_analyze(linitial_node(Query, rewritten), &is2, why);
}

/*
 * plan_colocated_insert_select
 *
 * P6 (design 11.7): an INSERT ... SELECT whose target and sources are of one colocation
 * group, joined on their keys so that every row of the result is made on the group that owns
 * it, is shipped whole to every group of the target and run there: no row passes through this
 * node, the counts and the RETURNING rows are added, the groups commit together by the
 * two-phase path of the fan-out, and the placement of the group is compared after the
 * statement ran on every group (fanout.c).  Returns NULL when the statement is not one (the
 * row router or the refusals of 3 and 4 then decide, exactly as without this path).
 */
static PlannedStmt *
plan_colocated_insert_select(Query *parse)
{
	KsInsertSelect is;
	const char *why = NULL;
	char	   *body;
	char	   *sql;
	int			nest;
	int64		place;
	const KsCatalog *cat;
	const KsColocation *coloc;

	if (IsInParallelMode())
		return NULL;
	if (!kingres_shard_insert_select_analyze(parse, &is, &why))
	{
		ereport(DEBUG1, (errmsg_internal("kingres_shard: INSERT ... SELECT is not shipped whole: %s", why)));
		return NULL;
	}

	nest = kingres_shard_set_transmission_modes();
	body = pg_get_querydef(is.insert, false);
	kingres_shard_reset_transmission_modes(nest);

	if (!insert_select_text_accepted(body, parse, &why))
	{
		ereport(DEBUG1, (errmsg_internal("kingres_shard: INSERT ... SELECT is not shipped whole: the groups would not accept its text: %s", why)));
		return NULL;
	}

	/* the analyses read the catalog cache, which a call of theirs may have reloaded */
	cat = kingres_shard_catalog();
	coloc = kingres_shard_lookup_colocation(is.colocation_id);
	if (coloc == NULL)
		return NULL;
	place = kingres_shard_placement_hash(coloc);
	sql = psprintf("%s%s", kingres_shard_insert_select_marker(place), body);
	return kingres_shard_make_insert_select_plan(parse, sql, fanout_targets(coloc, cat), place,
												 is.relids);
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
 * partialize_plan
 *
 * The plan of the fragment of a partial aggregation (design 7.3): the planner made the plan of an ordinary
 * grouping query; it is turned into the first step of a two-step aggregation, which returns the transition state of
 * every aggregate (a state of type internal as bytea, serialized) instead of the final value.  The top node must
 * be the Agg node (or, with no aggregate, a Group node, which has nothing to split); the node and every Aggref of its
 * target list change together (nodeAgg.c asserts that they agree).  Any other shape is an error that names it: the
 * settings of plan_partial_fragment make the plan one Agg over the scan or the join, and the text the sender makes
 * has only bare aggregates and grouping columns in its select list.
 */
static Node *
partial_aggref_mutator(Node *node, void *context)
{
	if (node == NULL)
		return NULL;
	if (IsA(node, Aggref))
	{
		Aggref	   *a = copyObject((Aggref *) node);

		mark_partial_aggref(a, AGGSPLIT_INITIAL_SERIAL);
		return (Node *) a;
	}
	return expression_tree_mutator(node, partial_aggref_mutator, context);
}

static bool
has_aggref_walker(Node *node, void *context)
{
	if (node == NULL)
		return false;
	if (IsA(node, Aggref))
		return true;
	return expression_tree_walker(node, has_aggref_walker, context);
}

/*
 * Is the state of every aggregate of the query its final value (no final function, the state of the type of the result)?  min()
 * and max() are: a query that has only those and no GROUP BY can be planned by the planner as an index probe (an InitPlan
 * under a Result node, no Agg node), whose output columns are then the partial states as they stand.
 */
static bool
aggs_state_is_value_walker(Node *node, void *context)
{
	if (node == NULL)
		return false;
	if (IsA(node, Aggref))
	{
		Aggref	   *a = (Aggref *) node;
		HeapTuple	tup = SearchSysCache1(AGGFNOID, ObjectIdGetDatum(a->aggfnoid));
		bool		bad;

		if (!HeapTupleIsValid(tup))
			return true;
		bad = OidIsValid(((Form_pg_aggregate) GETSTRUCT(tup))->aggfinalfn) ||
			(((Form_pg_aggregate) GETSTRUCT(tup))->aggtranstype != a->aggtype &&
			 !IsPolymorphicType(((Form_pg_aggregate) GETSTRUCT(tup))->aggtranstype));
		ReleaseSysCache(tup);
		return bad;
	}
	return expression_tree_walker(node, aggs_state_is_value_walker, context);
}

static void
partialize_plan(PlannedStmt *pstmt, bool state_is_value)
{
	Plan	   *top = pstmt->planTree;
	ListCell   *lc;

	/*
	 * The planner answered min() and max() of an indexed column by an index probe: a Result over InitPlans, with no Agg
	 * node.  The values it returns are the transition states of those aggregates, which have no final function.
	 */
	if (IsA(top, Result) && top->lefttree == NULL && state_is_value)
		return;

	if (IsA(top, Group))
	{
		if (has_aggref_walker((Node *) top->targetlist, NULL))
			elog(ERROR, "the plan of a partial aggregation has a Group node with aggregates");
		return;
	}
	if (IsA(top, Agg))
	{
		Agg		   *agg = (Agg *) top;

		if (agg->aggsplit != AGGSPLIT_SIMPLE || agg->plan.qual != NIL)
			elog(ERROR, "the plan of a partial aggregation has an Agg node that is already split or has a qualification");
		foreach(lc, agg->plan.targetlist)
		{
			TargetEntry *te = lfirst_node(TargetEntry, lc);

			if (IsA(te->expr, Aggref))
				te->expr = (Expr *) partial_aggref_mutator((Node *) te->expr, NULL);
			else if (has_aggref_walker((Node *) te->expr, NULL))
				elog(ERROR, "the plan of a partial aggregation has an aggregate inside an expression");
		}
		agg->aggsplit = AGGSPLIT_INITIAL_SERIAL;
		return;
	}
	elog(ERROR, "the plan of a partial aggregation does not have an Agg node at its top (node type %d)", (int) nodeTag(top));
}

/*
 * Plan the fragment of a partial aggregation (a group that received it, or this node for its own part): one Agg
 * over the scan or the join (no parallel workers, no partitionwise aggregation, no eager aggregation: 7.3, D1),
 * then partialize.
 */
static PlannedStmt *
plan_partial_fragment(Query *parse, const char *query_string, int cursorOptions,
					  ParamListInfo boundParams, const KsCatalog *cat, int expected, bool local_part)
{
	int			nest = NewGUCNestLevel();
	PlannedStmt *pstmt;
	bool		state_is_value = !aggs_state_is_value_walker((Node *) parse->targetList, NULL);

	(void) set_config_option("max_parallel_workers_per_gather", "0", PGC_SUSET, PGC_S_SESSION,
							 GUC_ACTION_SAVE, true, 0, false);
	(void) set_config_option("enable_partitionwise_aggregate", "off", PGC_SUSET, PGC_S_SESSION,
							 GUC_ACTION_SAVE, true, 0, false);
#ifdef KS_EAGER_AGGREGATE_GUC
	(void) set_config_option(KS_EAGER_AGGREGATE_GUC, "off", PGC_SUSET, PGC_S_SESSION,
							 GUC_ACTION_SAVE, true, 0, false);
#endif
	if (local_part)
		pstmt = pass_through(parse, query_string, cursorOptions, boundParams);
	else
		pstmt = plan_locally(parse, query_string, cursorOptions, boundParams, cat, expected);
	AtEOXact_GUC(true, nest);
	partialize_plan(pstmt, state_is_value);
	return pstmt;
}

/*
 * The extensions named in the marker are installed here at the version the origin has (design 5.5): another version, or none, is KS001
 * with a detail that says the statement is not a retry until the upgrade is finished.  This runs before this node's own analysis, which
 * would refuse an object of a version it does not know with a less useful message.
 */
static void
check_marker_installed_versions(const char *marker_ext)
{
	char	   *copy;
	char	   *tok;

	if (marker_ext == NULL)
		return;
	/* the list is split below at ',' and ':': it is of the exact form, or it is refused (review S13F-02) */
	kingres_shard_ext_list_check(marker_ext);
	copy = pstrdup(marker_ext);
	for (tok = strtok(copy, ","); tok != NULL; tok = strtok(NULL, ","))
	{
		char	   *colon = strchr(tok, ':');
		char	   *name;
		char	   *here;

		if (colon == NULL)
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("this node does not accept the fragment that another node sent it"),
					 errdetail("The marker names an extension without a version.")));
		*colon = '\0';
		name = tok;
		here = kingres_shard_extension_version(name);
		if (here == NULL)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_STALE_MAP),
					 errmsg("extension %s is not installed here and is at version %s at the node that sent the statement", name, colon + 1),
					 errdetail("The statement is shipped whole only between nodes that run the same version of the extension."),
					 errhint("Repeating the statement does not help until the extension is installed on every node; SET kingres_shard.shippable_extensions = 'none' makes the node that runs the statement read the rows instead.")));
		if (strcmp(here, colon + 1) != 0)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_STALE_MAP),
					 errmsg("extension %s is at version %s here and %s at the node that sent the statement", name, here, colon + 1),
					 errdetail("The statement is shipped whole only between nodes that run the same version of the extension."),
					 errhint("Repeating the statement does not help until the upgrade of the extension is finished on every node; SET kingres_shard.shippable_extensions = 'none' makes the node that runs the statement read the rows instead.")));
	}
}

/*
 * The extensions that a fragment names in its marker (design 5.5) are the ones the statement uses, at the version this node has.
 * `used` is what this node's own analysis of the text found.  A version that differs is KS001 with a detail that says it is not
 * a retry until the upgrade is finished; a statement that uses what the marker does not name (a forged one) is 0A000.
 */
static void
check_marker_extensions(const char *marker_ext, const char *used)
{
	List	   *mt = NIL;
	List	   *ut = NIL;
	ListCell   *lc;
	ListCell   *lc2;
	char	   *copy;
	char	   *tok;

	if (marker_ext == NULL && used == NULL)
		return;
	kingres_shard_ext_list_check(marker_ext);
	kingres_shard_ext_list_check(used);
	if (marker_ext != NULL)
	{
		copy = pstrdup(marker_ext);
		for (tok = strtok(copy, ","); tok != NULL; tok = strtok(NULL, ","))
			mt = lappend(mt, pstrdup(tok));
	}
	if (used != NULL)
	{
		copy = pstrdup(used);
		for (tok = strtok(copy, ","); tok != NULL; tok = strtok(NULL, ","))
			ut = lappend(ut, pstrdup(tok));
	}
	foreach(lc, ut)
	{
		char	   *u = (char *) lfirst(lc);
		char	   *colon = strchr(u, ':');
		size_t		nlen = colon ? (size_t) (colon - u) : strlen(u);
		char	   *theirs = NULL;

		foreach(lc2, mt)
		{
			char	   *m = (char *) lfirst(lc2);

			if (strncmp(m, u, nlen) == 0 && m[nlen] == ':')
				theirs = m + nlen + 1;
		}
		if (theirs == NULL)
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("this node does not accept the fragment that another node sent it"),
					 errdetail("The statement uses the extension %.*s, which its marker does not name.", (int) nlen, u),
					 errhint("This is not a change of the bucket map: repeating the statement does not help.")));
		if (strcmp(theirs, colon + 1) != 0)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_STALE_MAP),
					 errmsg("extension %.*s is at version %s here and %s at the node that sent the statement", (int) nlen, u, colon + 1, theirs),
					 errdetail("The statement is shipped whole only between nodes that run the same version of the extension."),
					 errhint("Repeating the statement does not help until the upgrade of the extension is finished on every node; SET kingres_shard.shippable_extensions = 'none' makes the node that runs the statement read the rows instead.")));
	}
	foreach(lc, mt)
	{
		char	   *m = (char *) lfirst(lc);
		char	   *colon = strchr(m, ':');
		size_t		nlen = colon ? (size_t) (colon - m) : strlen(m);
		bool		found = false;

		foreach(lc2, ut)
			if (strncmp((char *) lfirst(lc2), m, nlen) == 0 && ((char *) lfirst(lc2))[nlen] == ':')
				found = true;
		if (!found)
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("this node does not accept the fragment that another node sent it"),
					 errdetail("The marker names the extension %.*s, which the statement does not use.", (int) nlen, m),
					 errhint("This is not a change of the bucket map: repeating the statement does not help.")));
	}
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

	/*
	 * A closed query that another node runs whole (P4, P5 without order or limit; 4.2).  This node
	 * decides on the parse of the text it received, with the function the sender used on the same
	 * text (query_text_accepted), and runs it over its own buckets: the rows are the ones the
	 * buckets of this node make, which is what the sender asks for.  The placement of the colocation
	 * group is compared with the sender's after planning (the parents are locked since parse
	 * analysis, which a DETACH of a bucket waits for).  What no rule accepts is not a stale map.
	 */
	if (parse->commandType == CMD_SELECT && query_string != NULL)
	{
		int64		place;
		char	   *marker_ext = NULL;

		int			marker_rp = 0;

		if (kingres_shard_parse_query_marker_rp(query_string, &place, &marker_ext, &marker_rp))
		{
			KsQueryFragment qf;
			const char *why = NULL;

			if ((check_marker_installed_versions(marker_ext), true) &&
				kingres_shard_query_fragment_analyze(parse, &qf, &why) &&
				(marker_rp == qf.pd.nshipped || (why = "the shipped relations of the text are not the ones the marker counts", false)) &&
				(check_marker_extensions(marker_ext, qf.ext), true))
			{
				PlannedStmt *pstmt = plan_locally(parse, query_string, cursorOptions, boundParams,
												  cat, a->ndist_rtes);

				if (
					kingres_shard_placement_hash_now(qf.colocation_id) != place)
					ereport(ERROR,
							(errcode(ERRCODE_KINGRES_STALE_MAP),
							 errmsg("the bucket map of this node is not the one the statement was shipped with (stale map version " INT64_FORMAT ")",
									cat->map_version),
							 errhint("A bucket moved between shard groups, or the primary of a group changed; repeat the statement.")));
				return pstmt;
			}
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("this node does not accept the fragment that another node sent it"),
					 errdetail("The statement is not a closed query that this node runs: %s.", why),
					 errhint("This is not a change of the bucket map: repeating the statement does not help.")));
		}
	}

	/*
	 * The first step of a partial aggregation that another node asks for (design 7.3): this node plans the
	 * grouping query it received over its own buckets, turns the plan into one that returns the partial states,
	 * and compares the placement as for a closed query.
	 */
	if (parse->commandType == CMD_SELECT && query_string != NULL)
	{
		int64		place;
		char	   *marker_ext = NULL;

		if (kingres_shard_parse_partial_marker(query_string, &place, &marker_ext))
		{
			KsQueryFragment qf;
			const char *why = NULL;

			if ((check_marker_installed_versions(marker_ext), true) &&
				kingres_shard_partial_fragment_analyze(parse, &qf, &why) &&
				(check_marker_extensions(marker_ext, qf.ext), true))
			{
				PlannedStmt *pstmt = plan_partial_fragment(parse, query_string, cursorOptions, boundParams,
														   cat, a->ndist_rtes, false);

				if (kingres_shard_placement_hash_now(qf.colocation_id) != place)
					ereport(ERROR,
							(errcode(ERRCODE_KINGRES_STALE_MAP),
							 errmsg("the bucket map of this node is not the one the statement was shipped with (stale map version " INT64_FORMAT ")",
									cat->map_version),
							 errhint("A bucket moved between shard groups, or the primary of a group changed; repeat the statement.")));
				return pstmt;
			}
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("this node does not accept the fragment that another node sent it"),
					 errdetail("The statement is not a partial aggregation that this node runs: %s.", why),
					 errhint("This is not a change of the bucket map: repeating the statement does not help.")));
		}
	}

	/*
	 * A colocated INSERT ... SELECT that another node ships whole (11.7).  This node decides
	 * on the parse of the text it received, with the function the sender used on the same text
	 * (insert_select_text_accepted), and runs it over its own buckets: each row it reads is
	 * inserted into a partition of this node, or fails with "no partition found for row".
	 */
	if (parse->commandType == CMD_INSERT && query_string != NULL)
	{
		int64		place;

		if (kingres_shard_parse_insert_select_marker(query_string, &place))
		{
			KsInsertSelect is;
			const char *why = NULL;

			if (kingres_shard_insert_select_analyze(parse, &is, &why))
			{
				PlannedStmt *pstmt = plan_locally(parse, query_string, cursorOptions, boundParams,
												  cat, a->ndist_rtes);

				/* after planning (the target is locked ROW EXCLUSIVE, the sources ACCESS SHARE) */
				if (
					kingres_shard_placement_hash_now(is.colocation_id) != place)
					ereport(ERROR,
							(errcode(ERRCODE_KINGRES_STALE_MAP),
							 errmsg("the bucket map of this node is not the one the statement was shipped with (stale map version " INT64_FORMAT ")",
									cat->map_version),
							 errhint("A bucket moved between shard groups, or the primary of a group changed; repeat the statement.")));
				return pstmt;
			}
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("this node does not accept the fragment that another node sent it"),
					 errdetail("The statement is not a colocated INSERT ... SELECT that this node runs: %s.", why),
					 errhint("This is not a change of the bucket map: repeating the statement does not help.")));
		}
	}

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
	if (query_string != NULL &&
		((list_length(a->refs) == 1 &&
		  (strncmp(query_string, KS_FRAGMENT_MARKER, strlen(KS_FRAGMENT_MARKER)) == 0 ||
		   strncmp(query_string, KS_WRITE_MARKER, strlen(KS_WRITE_MARKER)) == 0 ||
		   strncmp(query_string, KS_INSERT_MARKER_PREFIX, strlen(KS_INSERT_MARKER_PREFIX)) == 0)) ||
		 strncmp(query_string, KS_INSERT_SELECT_MARKER_PREFIX, strlen(KS_INSERT_SELECT_MARKER_PREFIX)) == 0 ||
		 strncmp(query_string, KS_QUERY_MARKER_PREFIX, strlen(KS_QUERY_MARKER_PREFIX)) == 0 ||
		 strncmp(query_string, KS_PARTIAL_MARKER_PREFIX, strlen(KS_PARTIAL_MARKER_PREFIX)) == 0))
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
 * query_text_accepted
 *
 * The decision of a receiving group is made on the parse of the text it receives, not on the tree
 * this node deparsed (design: "the receiving side decides on text that was deparsed and parsed
 * again").  So that a query that is shipped is never refused there, the text is parsed and
 * rewritten here, under the same settings, and the same function decides on it.  What a deparse
 * and a parse change in the meaning of the query shows up as a refusal, and the scan answers.  The
 * columns of the text must be those the plan expects: the same number, of the same types.
 */
bool
kingres_shard_text_accepted(const char *body, Query *sent, bool partial, const char **why, char **ext)
{
	ParamTypes	pt = {NULL, 0};
	List	   *raw;
	List	   *rewritten;
	Query	   *rq;
	KsQueryFragment qf;
	ListCell   *la;
	ListCell   *lb;
	int			nest;
	int			i;

	(void) param_types_walker((Node *) sent, &pt);
	for (i = 0; i < pt.n; i++)
		if (!OidIsValid(pt.types[i]))
			pt.types[i] = TEXTOID;

	kingres_shard_fault_point("query_before_text_parse");
	nest = kingres_shard_set_transmission_modes();
	raw = pg_parse_query(body);
	if (list_length(raw) != 1)
	{
		kingres_shard_reset_transmission_modes(nest);
		*why = "the deparsed text is not one statement";
		return false;
	}
	rq = parse_analyze_fixedparams(linitial_node(RawStmt, raw), body, pt.types, pt.n, NULL);
	rewritten = pg_rewrite_query(rq);
	kingres_shard_reset_transmission_modes(nest);
	if (list_length(rewritten) != 1)
	{
		*why = "the deparsed text is rewritten into several statements";
		return false;
	}
	rq = linitial_node(Query, rewritten);
	if (partial ? !kingres_shard_partial_fragment_analyze(rq, &qf, why) : !kingres_shard_query_fragment_analyze(rq, &qf, why))
		return false;
	if (ext != NULL)
		*ext = qf.ext;

	/*
	 * The columns the text returns are the ones that are not resjunk: a partial fragment groups by columns that it does not
	 * return (a resjunk entry of the parse), a closed one has none (every entry was made an ordinary column).
	 */
	{
		List	   *ca = NIL;
		List	   *cb = NIL;

		foreach(la, rq->targetList)
			if (!lfirst_node(TargetEntry, la)->resjunk || !partial)
				ca = lappend(ca, lfirst(la));
		foreach(lb, sent->targetList)
			if (!lfirst_node(TargetEntry, lb)->resjunk || !partial)
				cb = lappend(cb, lfirst(lb));
		if (list_length(ca) != list_length(cb))
		{
			*why = "the deparsed text returns another number of columns";
			return false;
		}
		forboth(la, ca, lb, cb)
		{
			TargetEntry *a = lfirst_node(TargetEntry, la);
			TargetEntry *b = lfirst_node(TargetEntry, lb);

			if (a->resjunk || exprType((Node *) a->expr) != exprType((Node *) b->expr))
			{
				*why = "a column of the deparsed text has another type";
				return false;
			}
		}
	}
	return true;
}

/* An expression that this node can compute when a scan starts: no column, no volatile function, no sub-select, no aggregate. */
static bool
limit_evaluable(Node *e)
{
	if (e == NULL)
		return true;
	return !contain_var_clause(e) && !contain_volatile_functions(e) && !contain_subplans(e) && !checkExprHasSubLink(e) &&
		!contain_agg_clause(e) && !contain_window_function(e);
}

/*
 * closed_sort_limit

 *
 * Design 8: the sort and the limit of a closed query go to the groups.  Every group returns its rows in the final order and this
 * node merges the sorted streams (fragment_scan.c) instead of sorting every row.  What is sent is ORDER BY as the query has it
 * (the select list holds every sort expression: the entries that only order were made ordinary columns) and, when the limit is
 * pushed, LIMIT count + offset with no OFFSET: every group returns its first count + offset rows of the final order, and the node
 * that gathers applies OFFSET and LIMIT to the merged stream.
 *
 * The sort is pushed only when this node can order exactly as the groups do (O1, O2): the ordering operator of every key is a
 * built-in btree operator and the collation is the default or "C".  The limit is pushed only with a sort that is pushed (or none),
 * with constants that fit, without WITH TIES (O4, O5).  *spec is a list of lists of four integers per key: the column, the operator,
 * the collation, nulls first.  Without a pushed sort, the sort clause is taken out of the text.
 */
static void
closed_sort_limit(Query *parse, Query *ship, List **spec, bool *limit_pushed, Node **lcnt, Node **loff)
{
	ListCell   *lc;
	bool		sort_ok = true;

	*spec = NIL;
	*limit_pushed = false;
	*lcnt = NULL;
	*loff = NULL;
	if (!kingres_shard_enable_pushdown_sort)
	{
		ship->sortClause = NIL;
		ship->limitOffset = NULL;
		ship->limitCount = NULL;
		ship->limitOption = LIMIT_OPTION_COUNT;
		return;
	}
	foreach(lc, parse->sortClause)
	{
		SortGroupClause *sgc = lfirst_node(SortGroupClause, lc);
		TargetEntry *tle = get_sortgroupclause_tle(sgc, parse->targetList);
		Oid			coll = exprCollation((Node *) tle->expr);
		Oid			opfamily;
		Oid			opcintype;
		CompareType strategy;

		if ((coll != InvalidOid && coll != DEFAULT_COLLATION_OID && coll != C_COLLATION_OID) ||
			sgc->sortop >= FirstGenbkiObjectId ||
			!get_ordering_op_properties(sgc->sortop, &opfamily, &opcintype, &strategy))
		{
			sort_ok = false;
			break;
		}
		*spec = lappend(*spec, list_make4(makeInteger(tle->resno - 1), makeInteger((int) sgc->sortop),
										  makeInteger((int) coll), makeInteger(sgc->nulls_first ? 1 : 0)));
	}
	if (!sort_ok)
	{
		*spec = NIL;
		ship->sortClause = NIL;
		ship->limitOffset = NULL;
		ship->limitCount = NULL;
		ship->limitOption = LIMIT_OPTION_COUNT;
		return;
	}
	/*
	 * the limit: constants only, count + offset, no WITH TIES.  The parser leaves the cast of the literal to bigint as a call
	 * (the planner folds it later), so the expressions are folded here, as the planner will.
	 */
	ship->limitOffset = NULL;
	ship->limitCount = NULL;
	ship->limitOption = LIMIT_OPTION_COUNT;
	if (parse->limitCount != NULL && parse->limitOption == LIMIT_OPTION_COUNT)
	{
		Node	   *cnt = eval_const_expressions(NULL, parse->limitCount);
		Node	   *off = parse->limitOffset ? eval_const_expressions(NULL, parse->limitOffset) : NULL;

		if (IsA(cnt, Const) && !((Const *) cnt)->constisnull && ((Const *) cnt)->consttype == INT8OID &&
			(off == NULL ||
			 (IsA(off, Const) && !((Const *) off)->constisnull && ((Const *) off)->consttype == INT8OID)))
		{
			int64		count = DatumGetInt64(((Const *) cnt)->constvalue);
			int64		offset = off ? DatumGetInt64(((Const *) off)->constvalue) : 0;
			int64		sum;

			if (count >= 0 && offset >= 0 && !pg_add_s64_overflow(count, offset, &sum))
			{
				ship->limitCount = (Node *) makeConst(INT8OID, -1, InvalidOid, sizeof(int64), Int64GetDatum(sum), false, FLOAT8PASSBYVAL);
				*limit_pushed = true;
			}
		}
		else if (!(IsA(cnt, Const) && ((Const *) cnt)->constisnull) && limit_evaluable(cnt) && (off == NULL || limit_evaluable(off)))
		{
			/*
			 * A parameter, or an expression of parameters and immutable and stable functions: this node computes
			 * count + offset when the scan starts and sends it as one more parameter (design 8.2 O5).
			 */
			*lcnt = cnt;
			*loff = off;
			*limit_pushed = true;
		}
	}
}

/*
 * pushdown_prepare
 *
 * The decision of design 6 for a SELECT that spans shard groups: is it a closed query that every
 * group can run whole on its own buckets (6.1 and 6.4), that gains something from it, that passes
 * the shippability walker, and whose text the groups accept?  Returns what the paths of the
 * statement need (fragment_scan.c) or NULL with *why (a static string, shown by EXPLAIN VERBOSE of
 * the scan the statement gets instead).  ORDER BY and LIMIT are not sent: the node that gathers
 * the rows applies them (the ordered merge and the pushed limit are step 9 of the design).  A column that
 * only orders or groups (a resjunk entry of the select list) is sent as an ordinary column, so
 * the node that sorts has it; a DISTINCT query with such a column is not sent.
 *
 * The statement is not changed.  The work reads the catalog, which can reload the cache of this
 * backend: nothing taken from it before the call is used after it.
 */
static KsPushdownInfo *
pushdown_prepare(Query *parse, const char **why)
{
	KsPushdown	pd;
	const char *reason = NULL;
	Query	   *ship;
	KsQueryFragment qf;
	KsParamRenumber prn = {NIL};
	char	   *body = NULL;
	char	   *pi_ext = NULL;
	int			nest;
	ListCell   *lc;
	List	   *dist = NIL;
	const KsColocation *coloc;
	KsPushdownInfo *pi;
	bool		is_partial = false;
	bool		is_join = false;
	List	   *sortspec = NIL;
	bool		limit_pushed = false;
	Node	   *lcnt = NULL;
	Node	   *loff = NULL;
	int			limit_param = 0;

#define PD_NO(msg) do { *why = (msg); return NULL; } while (0)

	if (IsInParallelMode())
		PD_NO("it is planned in a parallel worker");
	if (!kingres_shard_pushdown_analyze(parse, &pd, &reason))
		PD_NO(reason);
	if (!kingres_shard_pushdown_closed(&pd, &reason))
	{
		const char *preason = NULL;

		/*
		 * Not closed.  An aggregation or a grouping that does not contain a distribution key can still be run in two steps
		 * (design 7): each group aggregates its own rows and this node combines the partial states.  And the join of the level
		 * may be a fragment of its own (design 9): the groups return the rows of the join and this node does the rest.  The
		 * text of both is made by the path, which knows the columns it needs (fragment_scan.c).
		 */
		bool		can_partial = kingres_shard_enable_partial_aggregation &&
			kingres_shard_partial_shape_ok(parse, false, &preason);
		bool		can_join = kingres_shard_enable_pushdown_join && list_length(parse->rtable) >= 2;

		if (!can_partial && !can_join)
			PD_NO(kingres_shard_enable_partial_aggregation && (parse->hasAggs || parse->groupClause != NIL) && preason != NULL ?
				  preason : reason);
		qf.pd = pd;
		qf.colocation_id = pd.colocation_id;
		qf.relids = kingres_shard_pushdown_relids(&pd);
		ship = copyObject(parse);
		ship->sortClause = NIL;
		ship->limitOffset = NULL;
		ship->limitCount = NULL;
		ship->limitOption = LIMIT_OPTION_COUNT;
		is_partial = can_partial;
		is_join = can_join;
		goto assemble;
	}

	if (!kingres_shard_enable_pushdown_closed)
		PD_NO("closed pushdown is switched off");
	ship = copyObject(parse);
	foreach(lc, ship->targetList)
	{
		TargetEntry *te = lfirst_node(TargetEntry, lc);

		if (te->resjunk && ship->distinctClause != NIL)
			PD_NO("it removes duplicates and has a column that only orders or groups");
		te->resjunk = false;
	}
	closed_sort_limit(parse, ship, &sortspec, &limit_pushed, &lcnt, &loff);
	if (!kingres_shard_query_fragment_analyze(ship, &qf, &reason))
		PD_NO(reason);
	if (!parse->hasAggs && parse->groupClause == NIL && parse->distinctClause == NIL && pd.ndist < 2)
		PD_NO("it reads one table and neither groups nor removes duplicates: the groups would return the rows the scan returns");

	ship = query_tree_mutator(ship, kingres_shard_renumber_params, &prn, 0);
	if (lcnt != NULL)
	{
		Param	   *lp = makeNode(Param);

		/* the limit that is computed at the start of the scan: one more parameter, after those of the statement */
		limit_param = list_length(prn.ids) + 1;
		lp->paramkind = PARAM_EXTERN;
		lp->paramid = limit_param;
		lp->paramtype = INT8OID;
		lp->paramtypmod = -1;
		lp->paramcollid = InvalidOid;
		lp->location = -1;
		ship->limitCount = (Node *) lp;
	}
	nest = kingres_shard_set_transmission_modes();
	body = pg_get_querydef(ship, false);
	kingres_shard_reset_transmission_modes(nest);
	if (!kingres_shard_text_accepted(body, ship, false, &reason, &pi_ext))
	{
		ereport(DEBUG1, (errmsg_internal("kingres_shard: a closed query is not shipped: the groups would not accept its text: %s", reason)));
		PD_NO(reason);
	}

assemble:
	/* the analyses read the catalog cache, which a call of theirs may have reloaded */
	coloc = kingres_shard_lookup_colocation(qf.colocation_id);
	if (coloc == NULL)
		PD_NO("its colocation group is not known");
	foreach(lc, qf.pd.rels)
	{
		KsPdRel    *r = (KsPdRel *) lfirst(lc);

		if (!r->reference)
			dist = list_append_unique_oid(dist, r->relid);
	}
	pi = palloc0(sizeof(KsPushdownInfo));
	pi->parse = parse;
	pi->sql = body;
	pi->param_ids = prn.ids;
	pi->colocation_id = qf.colocation_id;
	pi->place = kingres_shard_placement_hash(coloc);
	pi->relids = qf.relids;
	pi->dist_relids = dist;
	pi->kind = (parse->hasAggs || parse->groupClause != NIL) ? "closed aggregate (by key)" :
		(parse->distinctClause != NIL ? "closed distinct (by key)" : "closed join (on key)");
	pi->ncols = list_length(parse->targetList);
	pi->ndist = qf.pd.ndist;
	pi->ext = (is_partial || is_join) ? NULL : (pi_ext != NULL ? pi_ext : qf.ext);
	pi->sort = sortspec;
	pi->limit_cnt = lcnt;
	pi->limit_off = loff;
	pi->limit_param = limit_param;
	pi->limit_pushed = limit_pushed;
	pi->partial = is_partial;
	pi->join = is_join;
	pi->closed = !is_partial && !is_join;
	pi->ship = (is_partial || is_join) ? ship : NULL;
	if (is_partial || is_join)
	{
		pi->sql = NULL;
		pi->kind = is_partial ? "partial aggregate" : "join rows (on key)";
	}
	return pi;
#undef PD_NO
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
			   ParamListInfo boundParams, KsPushdownInfo *pi, const char *pushdown_reason)
{
	PlannedStmt *pstmt;
	int			nest = NewGUCNestLevel();
	KsPushdownInfo *old_pi = kingres_shard_pushdown_info_set(pi);
	const char *old_reason = kingres_shard_pushdown_reason_swap(pushdown_reason);

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
		(void) kingres_shard_pushdown_info_set(old_pi);
		(void) kingres_shard_pushdown_reason_swap(old_reason);
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


/* ---------------------------------------------------------------------------
 * Tables of distributed schemas (phase 11, design/shard-phase11.md 6.1, 6.6, 6.8)
 *
 * A table of a distributed schema is held by one shard group, its owner.  On every
 * other group the same table exists with no rows (a shell).  The standard planner
 * knows nothing of that and would read the empty copy, so a statement that names a
 * shell never goes to it as it is: a read is planned with the distributed scan (one
 * fragment, to the owner, no local part: dist_scan.c), a write is refused until the
 * statement can be run whole on the owner, and a plan that still reads a shell is
 * an error and never an empty answer.
 * ------------------------------------------------------------------------- */

typedef struct SchemaFind
{
	const KsCatalog *cat;
	bool		any;			/* the statement names a table of a distributed schema */
	bool		shell;			/* ... one of which another group holds */
	bool		shell_table;	/* ... and it is a table (not only a sequence that the statement calls) */
	Oid			shell_relid;
	int			shell_group;	/* its owner */
	bool		write_shell;	/* a data-modifying query whose target is a shell */
	Oid			write_relid;
} SchemaFind;

static bool
schema_find_walker(Node *node, void *context)
{
	SchemaFind *sf = (SchemaFind *) context;

	if (node == NULL)
		return false;
	if (IsA(node, Query))
	{
		Query	   *q = (Query *) node;
		ListCell   *lc;
		Index		rti = 0;

		foreach(lc, q->rtable)
		{
			RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);
			const KsDistTable *st;
			int			owner;

			rti++;
			if (rte->rtekind != RTE_RELATION)
				continue;
			st = kingres_shard_lookup_schema_table(sf->cat, rte->relid);
			if (st == NULL)
				continue;
			sf->any = true;
			owner = kingres_shard_schema_table_owner(st);
			if (owner == sf->cat->local_group_id)
				continue;
			if (!sf->shell)
			{
				sf->shell_relid = rte->relid;
				sf->shell_group = owner;
			}
			sf->shell = true;
			sf->shell_table = true;
			if (q->resultRelation == (int) rti &&
				(q->commandType == CMD_INSERT || q->commandType == CMD_UPDATE ||
				 q->commandType == CMD_DELETE || q->commandType == CMD_MERGE) &&
				!sf->write_shell)
			{
				sf->write_shell = true;
				sf->write_relid = rte->relid;
			}
		}
		return query_tree_walker(q, schema_find_walker, context, 0);
	}
	/* a sequence of a distributed schema named by a constant: nextval(), currval(), setval() belong to its owner (design 6.4) */
	if (IsA(node, FuncExpr))
	{
		FuncExpr   *fe = (FuncExpr *) node;

		if (fe->funcid < FirstNormalObjectId && fe->funcresulttype == INT8OID && fe->args != NIL &&
			IsA(linitial(fe->args), Const) && ((Const *) linitial(fe->args))->consttype == REGCLASSOID &&
			!((Const *) linitial(fe->args))->constisnull)
		{
			char	   *fname = get_func_name(fe->funcid);

			if (fname != NULL && (strcmp(fname, "nextval") == 0 || strcmp(fname, "currval") == 0 ||
								  strcmp(fname, "setval") == 0))
			{
				Oid			seq = DatumGetObjectId(((Const *) linitial(fe->args))->constvalue);
				const KsDistSchema *ds = kingres_shard_lookup_dist_schema(sf->cat, get_rel_namespace(seq));

				if (ds != NULL)
				{
					int			owner = kingres_shard_bucket_group(ds->coloc, kingres_shard_coloc_bucket(ds->coloc, 0));

					sf->any = true;
					if (owner != sf->cat->local_group_id)
					{
						if (!sf->shell)
						{
							sf->shell_relid = seq;
							sf->shell_group = owner;
						}
						sf->shell = true;
					}
				}
			}
		}
	}
	return expression_tree_walker(node, schema_find_walker, context);
}

/* What a statement does with the tables of distributed schemas. */

static void
schema_find(Query *parse, const KsCatalog *cat, SchemaFind *sf)
{
	memset(sf, 0, sizeof(*sf));
	sf->cat = cat;
	(void) schema_find_walker((Node *) parse, sf);
}

/*
 * A session that another node opened runs statements for the buckets of this node.  A statement that
 * names a table of a distributed schema that this group does not hold is the statement of a node whose
 * map is older than this one's (or newer: the schema moved): the same retryable error as for a bucket.
 */
static void
schema_forwarded_check(const SchemaFind *sf, const KsCatalog *cat)
{
	if (!sf->shell)
		return;
	ereport(ERROR,
			(errcode(ERRCODE_KINGRES_STALE_MAP),
			 errmsg("this node does not hold table \"%s\" of a distributed schema (stale map version " INT64_FORMAT ")",
					qualified_name(sf->shell_relid), cat->map_version),
			 errdetail("The statement reached shard group %d; the table is held by shard group %d.",
					   cat->local_group_id, sf->shell_group),
			 errhint("The statement can be repeated: it is sent to the group that holds the table.")));
}

/*
 * Path S (design 6.2): a statement that touches the tables of one distributed schema held by another group,
 * and reference tables it only reads, and nothing else, is one closed statement.  It is deparsed and run whole
 * by the owner, which plans it with its own statistics and indexes: subqueries, CTEs (also those that write),
 * windows, aggregates, joins, set operations, FOR UPDATE, ON CONFLICT and RETURNING need nothing of this
 * node.  The plan does not depend on a key, so a generic plan can be routed too.  What the owner checks, and
 * what a stale map means, is the forwarded-session check above (KS001, then the retry of remote_scan.c).
 */
typedef struct PathS
{
	const KsCatalog *cat;
	int			owner;			/* the group that holds the tables, -1 until one is met */
	bool		ok;
	const char *why;			/* the reason when not ok, for the message of a refused write */
	bool		user_function;	/* a function, operator or aggregate that is not built in */
	bool		ship_all;		/* every schema the statement touches ships its functions */
	List	   *relids;			/* the tables and sequences, for the invalidation of the plan */
} PathS;

static void
paths_fail(PathS *p, const char *why)
{
	if (p->ok)
	{
		p->ok = false;
		p->why = why;
	}
}

static void
paths_note_group(PathS *p, int group, bool ship)
{
	if (p->owner < 0)
		p->owner = group;
	else if (p->owner != group)
		paths_fail(p, "it reads tables of distributed schemas that different shard groups hold");
	if (!ship)
		p->ship_all = false;
}

/*
 * A built-in that reads the state of the session that calls it: shipped as text it would be evaluated by the session of the owner (the
 * schema of the origin, a setting of the origin, its backend).  Such a statement is not run whole on the owner (review P11R-04).
 */
static bool
session_function(Oid funcid)
{
	char	   *n = get_func_name(funcid);
	bool		r;

	if (n == NULL)
		return false;
	r = strcmp(n, "current_schema") == 0 || strcmp(n, "current_schemas") == 0 || strcmp(n, "current_setting") == 0 ||
		strcmp(n, "set_config") == 0 || strcmp(n, "pg_backend_pid") == 0 || strcmp(n, "inet_client_addr") == 0 ||
		strcmp(n, "inet_client_port") == 0 || strcmp(n, "current_query") == 0 || strcmp(n, "pg_my_temp_schema") == 0 ||
		strcmp(n, "pg_is_other_temp_schema") == 0 || strcmp(n, "session_user") == 0;
	pfree(n);
	return r;
}

/* does the expression call a function that is not built in? */
static bool
calls_user_function_walker(Node *node, void *context)
{
	if (node == NULL)
		return false;
	if (IsA(node, FuncExpr) && ((FuncExpr *) node)->funcid >= FirstNormalObjectId)
		return true;
	if (IsA(node, OpExpr) || IsA(node, DistinctExpr) || IsA(node, NullIfExpr))
	{
		set_opfuncid((OpExpr *) node);
		if (((OpExpr *) node)->opfuncid >= FirstNormalObjectId)
			return true;
	}
	return expression_tree_walker(node, calls_user_function_walker, context);
}

/*
 * A table of the statement whose triggers, defaults or checks run code that the statement text does not show (review P11R-05): that code runs
 * on the owner with the search_path of the session that issued the statement, as it would on a direct connection.
 */
static bool
table_runs_hidden_code(Oid relid)
{
	Relation	rel;
	bool		hidden = false;

	if (get_rel_relkind(relid) != RELKIND_RELATION && get_rel_relkind(relid) != RELKIND_PARTITIONED_TABLE)
		return false;
	rel = table_open(relid, NoLock);
	if (rel->trigdesc != NULL && rel->trigdesc->numtriggers > 0)
		hidden = true;
	else if (rel->rd_att->constr != NULL)
	{
		TupleConstr *c = rel->rd_att->constr;
		int			i;

		for (i = 0; !hidden && i < c->num_defval; i++)
			hidden = calls_user_function_walker(stringToNode(c->defval[i].adbin), NULL);
		for (i = 0; !hidden && i < c->num_check; i++)
			hidden = calls_user_function_walker(stringToNode(c->check[i].ccbin), NULL);
	}
	table_close(rel, NoLock);
	return hidden;
}

/*
 * A function, operator or aggregate that is not built in.  Only a function of a distributed schema is shipped (it exists on every node);
 * a function of a local schema, of public or of an extension exists where it was made and is never shipped.  A function of a schema that
 * does not ship functions keeps the statement at the origin (the scan), as before.
 */
static void
paths_note_function(PathS *p, Oid funcid)
{
	const KsDistSchema *ds;

	p->user_function = true;
	ds = kingres_shard_lookup_dist_schema(p->cat, get_func_namespace(funcid));
	if (ds == NULL)
		paths_fail(p, "it calls a function that is not in a distributed schema");
	else if (!ds->ship_functions)
		p->ship_all = false;
}

static bool
paths_walker(Node *node, void *context)
{
	PathS	   *p = (PathS *) context;

	if (node == NULL)
		return false;
	if (IsA(node, Query))
	{
		Query	   *q = (Query *) node;
		ListCell   *lc;

		if (q->commandType == CMD_UPDATE)
		{
			/* SET col = DEFAULT of an identity column: the rewriter put nextval() there, which the owner takes for a value (review P11R-10) */
			foreach(lc, q->targetList)
			{
				TargetEntry *te = lfirst_node(TargetEntry, lc);

				if (IsA(te->expr, NextValueExpr))
					paths_fail(p, "it sets an identity column to DEFAULT");
			}
		}
		foreach(lc, q->rtable)
		{
			RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);
			const KsDistTable *st;
			const KsDistTable *dt;

			if (rte->rtekind != RTE_RELATION)
				continue;
			st = kingres_shard_lookup_schema_table(p->cat, rte->relid);
			if (st != NULL)
			{
				const KsDistSchema *ds = kingres_shard_lookup_dist_schema(p->cat, get_rel_namespace(rte->relid));

				kingres_shard_refuse_view_owner(q, rte);
				paths_note_group(p, kingres_shard_schema_table_owner(st), ds != NULL && ds->ship_functions);
				p->relids = list_append_unique_oid(p->relids, rte->relid);
				continue;
			}
			dt = p->cat->ndist_tables > 0 ? hash_search(p->cat->dist_tables, &rte->relid, HASH_FIND, NULL) : NULL;
			if (dt != NULL && dt->is_reference)
			{
				/* a view that another role owns would run with the rights of the session on the other node */
				if (rte->perminfoindex > 0)
				{
					Oid			owner = getRTEPermissionInfo(q->rteperminfos, rte)->checkAsUser;

					if (OidIsValid(owner) && owner != GetUserId())
						paths_fail(p, "it reads a reference table through a view that another role owns");
				}
				p->relids = list_append_unique_oid(p->relids, rte->relid);
				continue;
			}
			paths_fail(p, dt != NULL ? "it reads a hash-distributed table" :
					   "it reads a table that is not in a distributed schema and not a reference table");
		}
		return query_tree_walker(q, paths_walker, context, 0);
	}
	if (IsA(node, FuncExpr))
	{
		FuncExpr   *fe = (FuncExpr *) node;

		if (fe->funcid < FirstNormalObjectId && session_function(fe->funcid))
			paths_fail(p, "it calls a function that reads the state of the session (current_schema, current_setting, ...)");
		if (fe->funcid >= FirstNormalObjectId)
			paths_note_function(p, fe->funcid);
		else if (fe->funcresulttype == INT8OID && fe->args != NIL)
		{
			char	   *fname = get_func_name(fe->funcid);

			if (fname != NULL && strcmp(fname, "lastval") == 0)
				paths_fail(p, "it calls lastval()");
			else if (fname != NULL && (strcmp(fname, "nextval") == 0 || strcmp(fname, "currval") == 0 ||
									   strcmp(fname, "setval") == 0))
			{
				Node	   *a0 = linitial(fe->args);

				if (IsA(a0, Const) && ((Const *) a0)->consttype == REGCLASSOID && !((Const *) a0)->constisnull)
				{
					Oid			seq = DatumGetObjectId(((Const *) a0)->constvalue);
					const KsDistSchema *ds = kingres_shard_lookup_dist_schema(p->cat, get_rel_namespace(seq));

					if (ds != NULL)
					{
						paths_note_group(p, kingres_shard_bucket_group(ds->coloc, kingres_shard_coloc_bucket(ds->coloc, 0)),
										 ds->ship_functions);
						p->relids = list_append_unique_oid(p->relids, seq);
					}
					else
						paths_fail(p, "it calls a sequence that is not in a distributed schema");
				}
				else
					paths_fail(p, "it calls a sequence that is not named by a constant");
			}
		}
	}
	else if (IsA(node, OpExpr))
	{
		OpExpr	   *oe = (OpExpr *) node;

		set_opfuncid(oe);
		if (oe->opfuncid >= FirstNormalObjectId)
			paths_note_function(p, oe->opfuncid);
	}
	else if (IsA(node, DistinctExpr) || IsA(node, NullIfExpr))
	{
		set_opfuncid((OpExpr *) node);
		if (((OpExpr *) node)->opfuncid >= FirstNormalObjectId)
			paths_note_function(p, ((OpExpr *) node)->opfuncid);
	}
	else if (IsA(node, ScalarArrayOpExpr))
	{
		set_sa_opfuncid((ScalarArrayOpExpr *) node);
		if (((ScalarArrayOpExpr *) node)->opfuncid >= FirstNormalObjectId)
			paths_note_function(p, ((ScalarArrayOpExpr *) node)->opfuncid);
	}
	else if (IsA(node, Aggref))
	{
		if (((Aggref *) node)->aggfnoid >= FirstNormalObjectId)
			paths_note_function(p, ((Aggref *) node)->aggfnoid);
	}
	else if (IsA(node, WindowFunc))
	{
		if (((WindowFunc *) node)->winfnoid >= FirstNormalObjectId)
			paths_note_function(p, ((WindowFunc *) node)->winfnoid);
	}
	return expression_tree_walker(node, paths_walker, context);
}

/*
 * The search_path of this session as the owner is to run a function body with it (design 6.4): pg_catalog first, then the schemas of the
 * path that exist here, in order, and never the temporary schema (a temporary schema is private to a backend).  A schema that the owner
 * does not have is ignored by the server there, as on a direct connection.
 */
static char *
search_path_for_functions(void)
{
	List	   *oids = fetch_search_path(false);
	StringInfoData buf;
	ListCell   *lc;

	initStringInfo(&buf);
	appendStringInfoString(&buf, "pg_catalog");
	foreach(lc, oids)
	{
		Oid			nsp = lfirst_oid(lc);
		char	   *name;

		if (isAnyTempNamespace(nsp) || nsp == PG_CATALOG_NAMESPACE)
			continue;
		name = get_namespace_name(nsp);
		if (name == NULL)
			continue;
		appendStringInfo(&buf, ", %s", quote_identifier(name));
	}
	list_free(oids);
	return buf.data;
}

char *
kingres_shard_search_path_for_functions(void)
{
	return search_path_for_functions();
}

bool
kingres_shard_table_runs_hidden_code(Oid relid)
{
	return table_runs_hidden_code(relid);
}

static bool
statement_tables_run_hidden_code(List *relids)
{
	ListCell   *lc;

	foreach(lc, relids)
		if (table_runs_hidden_code(lfirst_oid(lc)))
			return true;
	return false;
}

/*
 * The statement of a session that is not forwarded names a table of a distributed schema that another
 * group holds.  Path S ships it whole to the owner when it can; else a SELECT (and a write whose target is
 * not a shell nor a hash-distributed table) is planned with the distributed scan, and the rest is refused
 * with the reason.
 */
static PlannedStmt *
plan_schema_shell(Query *parse, const char *query_string, int cursorOptions,
				  ParamListInfo boundParams, Analysis *a, const KsCatalog *cat,
				  const SchemaFind *sf)
{
	const char *why = NULL;

	if (kingres_shard_enable_schema_routing)
	{
		PathS		ps;

		memset(&ps, 0, sizeof(ps));
		ps.cat = cat;
		ps.owner = -1;
		ps.ok = true;
		ps.ship_all = true;
		(void) paths_walker((Node *) parse, &ps);
		if (ps.ok && ps.owner < 0)
			paths_fail(&ps, "it names no table of a distributed schema");
		if (ps.ok && ps.user_function && !ps.ship_all)
			paths_fail(&ps, "it calls a function that is not built in, and the schema does not ship functions (kingres.alter_distributed_schema)");
		if (ps.ok && (cursorOptions & CURSOR_OPT_SCROLL) && parse->commandType == CMD_SELECT)
			paths_fail(&ps, "a scrollable cursor needs the distributed scan");
		if (ps.ok && a->has_ref && OidIsValid(reference_write_target(parse, cat)))
			paths_fail(&ps, "it writes a reference table");
		if (ps.ok && ps.owner != cat->local_group_id)
		{
			const KsNode *node = NULL;
			ListCell   *lc;
			bool		is_write;

			if (IsInParallelMode())
				ereport(ERROR,
						(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
						 errmsg("a statement for another shard group cannot be forwarded from a parallel worker"),
						 errhint("Mark the function that runs the statement PARALLEL RESTRICTED or PARALLEL UNSAFE.")));
			foreach(lc, cat->nodes)
			{
				KsNode	   *n = (KsNode *) lfirst(lc);

				if (n->group_id == ps.owner && n->is_primary && n->is_active)
				{
					node = n;
					break;
				}
			}
			if (node == NULL)
				ereport(ERROR,
						(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
						 errmsg("shard group %d has no active primary node", ps.owner)));
			is_write = parse->commandType != CMD_SELECT || parse->rowMarks != NIL ||
				parse->hasModifyingCTE || contain_volatile_functions((Node *) parse);
			return kingres_shard_make_remote_plan_path(parse, deparse_for_remote(parse), node->host, node->port,
													   ps.owner, 0, is_write, ps.relids,
													   (ps.user_function || statement_tables_run_hidden_code(ps.relids)) ? search_path_for_functions() : NULL);
		}
		why = ps.why;
	}
	else
		why = "kingres_shard.enable_schema_routing is off";

	/* INSERT into a shell whose source path S cannot run on the owner (a local table, a hash table, another owner): the rows go to the owner through the row router (design 6.7) */
	if (sf->write_shell && parse->commandType == CMD_INSERT && !IsInParallelMode())
	{
		const KsDistTable *st = kingres_shard_lookup_schema_table(cat, sf->write_relid);

		if (st != NULL && kingres_shard_enable_schema_routing)
			return kingres_shard_plan_row_router(parse, st, list_make1_oid(sf->write_relid));
	}

	if (sf->write_shell)
	{
		const KsNode *owner = kingres_shard_group_primary(sf->shell_group);

		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a write to table \"%s\" of a distributed schema, which shard group %d holds, is not supported from shard group %d yet",
						qualified_name(sf->write_relid), sf->shell_group, cat->local_group_id),
				 errdetail("The statement cannot be run whole on the owner: %s.", why != NULL ? why : "unknown reason"),
				 owner ? errhint("Run the statement on node %d (%s:%d), the primary of shard group %d.",
								 owner->node_id, owner->host, owner->port, sf->shell_group) : 0));
	}
	if (!sf->shell_table)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a sequence of a distributed schema that shard group %d holds cannot be used from shard group %d in this statement",
						sf->shell_group, cat->local_group_id),
				 errdetail("The statement cannot be run whole on the owner: %s.", why != NULL ? why : "unknown reason"),
				 errhint("Call the sequence in a statement of its own.")));
	if (a->has_ref && OidIsValid(reference_write_target(parse, cat)))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a statement that writes a reference table and reads table \"%s\" of a distributed schema of another shard group is not supported yet",
						qualified_name(sf->shell_relid)),
				 errhint("Run them as two statements.")));
	if (!can_distributed_scan(parse, cat, true))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a statement that reads table \"%s\" of a distributed schema of another shard group needs the distributed scan",
						qualified_name(sf->shell_relid)),
				 errdetail("The distributed scan is switched off (kingres_shard.enable_distributed_scan), or the statement writes a distributed table."),
				 errhint("Read the table in a statement of its own, or run the statement on the primary of shard group %d.",
						 sf->shell_group)));
	return plan_with_scan(parse, query_string, cursorOptions, boundParams, NULL, NULL);
}

/*
 * The end-of-planning check (design 6.8, item 1): no scan of the plan reads the empty copy of a table of a
 * distributed schema that another group holds.  The routing above never lets such a statement through; the
 * paths it cannot see (the planner expands the body of a SQL function into the statement, for one) end
 * here, as an error that says what happened, and not as an answer without the rows.
 */
typedef struct SchemaPlanCheck
{
	const KsCatalog *cat;
	PlannedStmt *pstmt;
} SchemaPlanCheck;

static void
schema_plan_check_rti(SchemaPlanCheck *c, Index rti)
{
	RangeTblEntry *rte;
	const KsDistTable *st;

	if (rti == 0 || (int) rti > list_length(c->pstmt->rtable))
		return;
	rte = rt_fetch(rti, c->pstmt->rtable);
	if (rte->rtekind != RTE_RELATION)
		return;
	st = kingres_shard_lookup_schema_table(c->cat, rte->relid);
	if (st == NULL || kingres_shard_schema_table_owner(st) == c->cat->local_group_id)
		return;
	ereport(ERROR,
			(errcode(kingres_shard_session_forwarded() ? ERRCODE_KINGRES_STALE_MAP : ERRCODE_INTERNAL_ERROR),
			 errmsg("internal error: a plan reads the local copy of \"%s\", which this node does not own",
					qualified_name(rte->relid)),
			 errdetail("The table belongs to a distributed schema held by shard group %d; this node is in shard group %d.",
					   kingres_shard_schema_table_owner(st), c->cat->local_group_id),
			 errhint("Run the statement on the primary of shard group %d.", kingres_shard_schema_table_owner(st))));
}

static void
schema_plan_walk(SchemaPlanCheck *c, Plan *plan)
{
	ListCell   *lc;

	if (plan == NULL)
		return;
	check_stack_depth();
	switch (nodeTag(plan))
	{
		case T_SeqScan:
		case T_SampleScan:
		case T_IndexScan:
		case T_IndexOnlyScan:
		case T_BitmapHeapScan:
		case T_TidScan:
		case T_TidRangeScan:
			schema_plan_check_rti(c, ((Scan *) plan)->scanrelid);
			break;
		case T_ModifyTable:
			/* the target of the statement; a schema table is not partitioned, so it is its own nominal relation */
			schema_plan_check_rti(c, ((ModifyTable *) plan)->nominalRelation);
			break;
		case T_Append:
			foreach(lc, ((Append *) plan)->appendplans)
				schema_plan_walk(c, (Plan *) lfirst(lc));
			break;
		case T_MergeAppend:
			foreach(lc, ((MergeAppend *) plan)->mergeplans)
				schema_plan_walk(c, (Plan *) lfirst(lc));
			break;
		case T_BitmapAnd:
			foreach(lc, ((BitmapAnd *) plan)->bitmapplans)
				schema_plan_walk(c, (Plan *) lfirst(lc));
			break;
		case T_BitmapOr:
			foreach(lc, ((BitmapOr *) plan)->bitmapplans)
				schema_plan_walk(c, (Plan *) lfirst(lc));
			break;
		case T_SubqueryScan:
			schema_plan_walk(c, ((SubqueryScan *) plan)->subplan);
			break;
		case T_CustomScan:
			foreach(lc, ((CustomScan *) plan)->custom_plans)
				schema_plan_walk(c, (Plan *) lfirst(lc));
			break;
		default:
			break;
	}
	schema_plan_walk(c, plan->lefttree);
	schema_plan_walk(c, plan->righttree);
}

static void
check_plan_schema_shells(PlannedStmt *pstmt)
{
	SchemaPlanCheck c;
	ListCell   *lc;

	if (pstmt == NULL || pstmt->commandType == CMD_UTILITY || pstmt->planTree == NULL)
		return;
	/* the catalog may have been reloaded while the statement was planned */
	c.cat = kingres_shard_catalog();
	c.pstmt = pstmt;
	if (!c.cat->present || c.cat->nschema_tables == 0 || c.cat->local_group_id == -1)
		return;
	schema_plan_walk(&c, pstmt->planTree);
	foreach(lc, pstmt->subplans)
		schema_plan_walk(&c, (Plan *) lfirst(lc));
}


/*
 * The hook proper.  Whatever it decides for one statement is not inherited by
 * the statements planned while it plans (a function called during constant
 * folding may plan its own): the distributed scan is switched on only around
 * the standard planner of a statement that needs it.
 */
/* Is the statement this call of the planner hook plans the local part that the caller armed? */
static bool this_is_local_part = false;
/* the second pass of planner_hook_body after a reload of the catalog does not try the shipped INSERT ... SELECT again */
static bool insel_attempted = false;

/* the gap in which the plan of the local part can be invalidated: a fault point for the tests (t/131) */
static void
local_part_gap(bool is_local_part)
{
	if (is_local_part)
		kingres_shard_fault_point("local_part_planned");
}

static PlannedStmt *
kingres_shard_planner_hook(Query *parse, const char *query_string,
							int cursorOptions, ParamListInfo boundParams KS_PLANNER_ES_PARAM)
{
	PlannedStmt *result;
	bool		old = kingres_shard_distscan_set_active(false);
	void	   *old_explain = current_explain;
	bool		saved_local_part = this_is_local_part;
	KsPushdownInfo *old_pushdown = kingres_shard_pushdown_info_set(NULL);
	const char *old_reason = kingres_shard_pushdown_reason_swap(NULL);

	current_explain = (void *) KS_PLANNER_ES_VALUE;
	/* consumed first of all: nothing this hook runs can see the mark (fanout.c, P2B-01) */
	this_is_local_part = kingres_shard_local_part_take(query_string);
	PG_TRY();
	{
		result = planner_hook_body(parse, query_string, cursorOptions, boundParams);
		/* no plan may read the empty copy of a table of a distributed schema (design 6.8) */
		if (kingres_shard_enable_routing && !creating_extension)
			check_plan_schema_shells(result);
		/* the gap in which the plan of the local part can be invalidated (t/131) */
		local_part_gap(this_is_local_part);
	}
	PG_FINALLY();
	{
		(void) kingres_shard_distscan_set_active(old);
		current_explain = old_explain;
		this_is_local_part = saved_local_part;
		(void) kingres_shard_pushdown_info_set(old_pushdown);
		(void) kingres_shard_pushdown_reason_swap(old_reason);
	}
	PG_END_TRY();
	return result;
}

/*
 * kingres_shard_planner_hook
 *
 * See the file header.
 */
/*
 * In the forwarded session of a statement that runs code the text does not show (a function of the schema, a trigger, a default), every
 * table that a nested statement reads must be one that this node holds for the schema, a reference table or a catalog: a plain local
 * table would be this node's copy, and the answer would change with the node that runs the statement (design 6.4: KS001).
 */
static bool
shipped_scope_walker(Node *node, void *context)
{
	const KsCatalog *cat = (const KsCatalog *) context;

	if (node == NULL)
		return false;
	if (IsA(node, Query))
	{
		Query	   *q = (Query *) node;
		ListCell   *lc;

		foreach(lc, q->rtable)
		{
			RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);
			char		kind;
			Oid			nsp;
			const char *nspname;
			const KsDistTable *dt;

			if (rte->rtekind != RTE_RELATION)
				continue;
			kind = get_rel_relkind(rte->relid);
			if (kind != RELKIND_RELATION && kind != RELKIND_PARTITIONED_TABLE && kind != RELKIND_FOREIGN_TABLE)
				continue;
			nsp = get_rel_namespace(rte->relid);
			if (isAnyTempNamespace(nsp))
				continue;
			nspname = get_namespace_name(nsp);
			if (nspname != NULL && (strcmp(nspname, "pg_catalog") == 0 || strcmp(nspname, "information_schema") == 0 ||
									strcmp(nspname, "pg_toast") == 0 || strcmp(nspname, "kingres") == 0))
				continue;
			if (kingres_shard_lookup_schema_table(cat, rte->relid) != NULL)
				continue;
			dt = cat->ndist_tables > 0 ? hash_search(cat->dist_tables, &rte->relid, HASH_FIND, NULL) : NULL;
			if (dt != NULL)
				continue;
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_STALE_MAP),
					 errmsg("table \"%s\" is read by code that a statement of another node runs here, and it is not a table of a distributed schema or a reference table",
							qualified_name(rte->relid)),
					 errdetail("This node has its own copy of the table; the node that sent the statement has another.  The statement is refused, not answered from the wrong copy."),
					 errhint("Read the table in a statement of its own, or from a function that is not shipped (kingres.alter_distributed_schema ship_functions => false).")));
		}
		return query_tree_walker(q, shipped_scope_walker, context, 0);
	}
	return expression_tree_walker(node, shipped_scope_walker, context);
}

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
	if (kingres_shard_shipped_scope && kingres_shard_session_forwarded())
	{
		const KsCatalog *sc = kingres_shard_catalog();

		if (sc->present)
			(void) shipped_scope_walker((Node *) parse, (void *) sc);
	}
	if (parse->commandType != CMD_SELECT && parse->commandType != CMD_INSERT &&
		parse->commandType != CMD_UPDATE && parse->commandType != CMD_DELETE &&
		parse->commandType != CMD_MERGE)
		return pass_through(parse, query_string, cursorOptions, boundParams);

	/* The window of shard-phase4.md, "A routed statement on a target"; forced by t/033. */
	kingres_shard_fault_point("route_before_catalog");
	cat = kingres_shard_catalog();
	if (!cat->present || (cat->ndist_tables == 0 && cat->nschema_tables == 0 && cat->ndist_schemas == 0) || cat->local_group_id == -1)
		return pass_through(parse, query_string, cursorOptions, boundParams);

	/* With one shard group every bucket is local. */
	if (cat->ngroups <= 1 && cat->local_group_id >= 0 && !kingres_shard_session_forwarded())
		return pass_through(parse, query_string, cursorOptions, boundParams);

	refuse_bucket_partition_write(parse, cat);

	/*
	 * The analysis and the refresh of the partition lists take locks, which process invalidations
	 * and can make this backend load the catalog cache again (every time under
	 * debug_discard_caches): the cache that `a` points into is then gone.  The analysis is made
	 * again with the new cache; the locks are held by then, and a few rounds are enough.
	 */
	for (int tries = 0;; tries++)
	{
		int64		generation = kingres_shard_catalog_generation();
		int64		map_version = cat->map_version;

		memset(&a, 0, sizeof(a));
		a.params = boundParams;
		a.parse = parse;
		ctx[0] = &a;
		ctx[1] = (void *) cat;
		(void) analyze_walker((Node *) parse, ctx);

		/* The partition lists must be as new as the catalog cache just read. */
		{
			List	   *fresh = NIL;

			foreach(lc, a.refs)
				fresh = lappend_oid(fresh, ((DistRef *) lfirst(lc))->dt->relid);
			foreach(lc, fresh)
				kingres_shard_ensure_fresh_partitions(lfirst_oid(lc), map_version);
		}

		if (kingres_shard_catalog_generation() == generation)
			break;
		if (tries >= 4)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_STALE_MAP),
					 errmsg("the catalog of this node kept changing while the statement was analysed"),
					 errhint("Repeat the statement.")));
		cat = kingres_shard_catalog();
	}

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
	{
		int64		pplace;

		/* this node's own part of a partial aggregation returns partial states as the others do */
		if (parse->commandType == CMD_SELECT && query_string != NULL &&
			kingres_shard_parse_partial_marker(query_string, &pplace, NULL))
			return plan_partial_fragment(parse, query_string, cursorOptions, boundParams, cat, a.ndist_rtes, true);
		return pass_through(parse, query_string, cursorOptions, boundParams);
	}

	/*
	 * A table of a distributed schema that another group holds (phase 11): a session that another node
	 * opened is told its map is not this node's; a read of this node's own session is planned with the
	 * distributed scan, a write is refused (see plan_schema_shell).
	 */
	if (cat->nschema_tables > 0 || cat->ndist_schemas > 0)
	{
		SchemaFind	sf;

		schema_find(parse, cat, &sf);
		if (sf.shell)
		{
			if (kingres_shard_session_forwarded())
				schema_forwarded_check(&sf, cat);
			return plan_schema_shell(parse, query_string, cursorOptions, boundParams, &a, cat, &sf);
		}
	}

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
			return plan_with_scan(parse, query_string, cursorOptions, boundParams, NULL, NULL);
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
	 * A colocated INSERT ... SELECT is shipped whole (P6, design 11.7).  It comes before the
	 * row router, which stays the answer for every statement this path does not take.
	 */
	if (parse->commandType == CMD_INSERT && parse->resultRelation > 0 &&
		kingres_shard_enable_colocated_insert_select)
	{
		DistRef    *target = NULL;

		foreach(lc, a.refs)
		{
			DistRef    *ref = (DistRef *) lfirst(lc);

			if (ref->query == parse && ref->rti == (Index) parse->resultRelation)
				target = ref;
		}
		if (target != NULL && target->bucket < 0 && !target->param_pending && !insel_attempted)
		{
			int64		generation = kingres_shard_catalog_generation();
			PlannedStmt *shipped = plan_colocated_insert_select(parse);

			if (shipped != NULL)
				return shipped;

			/*
			 * The analysis of the statement, and the parse of its text, take locks, which process
			 * invalidations and can make this backend load the catalog cache again: every pointer
			 * that this function took from the old one (the tables of a.refs, the nodes) is then
			 * dangling.  Decide again from the start, with the new cache; the second pass cannot
			 * come back here.
			 */
			if (kingres_shard_catalog_generation() != generation)
			{
				PlannedStmt *again;
				bool		old_attempted = insel_attempted;

				/*
				 * The second pass does not try to ship again (review P2D-04): the first attempt already
				 * found that the statement is not shipped whole, and under debug_discard_caches every pass
				 * reloads the cache, so a loop that waits for a stable generation would never end.  It plans
				 * the statement from the start with the new cache, which is all the dangling pointers need.
				 */
				insel_attempted = true;
				PG_TRY();
				{
					again = planner_hook_body(parse, query_string, cursorOptions, boundParams);
				}
				PG_FINALLY();
				{
					insel_attempted = old_attempted;
				}
				PG_END_TRY();
				return again;
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
		{
			KsPushdownInfo *pi = NULL;
			const char *why = NULL;

			/*
			 * A closed query is one more alternative, with a cost (fragment_scan.c); the scan is
			 * planned beside it and answers whatever the alternative does not take (ADR 0022).
			 * The decision reads the catalog and may reload this backend's cache: nothing taken
			 * from the cache before it is used after it.
			 */
			if (parse->commandType == CMD_SELECT && kingres_shard_enable_pushdown &&
				(kingres_shard_enable_pushdown_closed || kingres_shard_enable_partial_aggregation))
			{
				pi = pushdown_prepare(parse, &why);
				if (pi == NULL && why != NULL)
					ereport(DEBUG1, (errmsg_internal("kingres_shard: no pushdown: %s", why)));
				/* phase 12 (J1): a join on columns other than the key, by moving one table (repartition.c) */
				pi = kingres_shard_repart_attach(parse, pi);
			}
			return plan_with_scan(parse, query_string, cursorOptions, boundParams, pi, why);
		}
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
		return plan_with_scan(parse, query_string, cursorOptions, boundParams, NULL, NULL);
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
