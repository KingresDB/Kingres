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
 * (remote_scan.c).  Any other statement is refused with a "not supported
 * yet" error: that is the phase 1 boundary.
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
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/planner.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/hash.h"
#include "access/xact.h"
#include "commands/extension.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "nodes/params.h"
#include "nodes/pathnodes.h"
#include "optimizer/optimizer.h"
#include "optimizer/planner.h"
#include "parser/parsetree.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/ruleutils.h"
#include "utils/typcache.h"

#include "catalog.h"
#include "hashing.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "planner.h"
#include "remote_scan.h"

static planner_hook_type prev_planner_hook = NULL;

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
	int			ndist_rtes;		/* every range table entry of a distributed table */
} Analysis;

static PlannedStmt *kingres_shard_planner_hook(Query *parse,
												const char *query_string,
												int cursorOptions,
												ParamListInfo boundParams);
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
		return prev_planner_hook(parse, query_string, cursorOptions, boundParams);
	return standard_planner(parse, query_string, cursorOptions, boundParams);
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
									  coloc->bucket_count, c->consttype,
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

	for (b = 0; b < coloc->bucket_count; b++)
	{
		if (coloc->bucket_group[b] != local_group)
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
 */
static void
check_plan_dist_rtes(PlannedStmt *pstmt, int expected)
{
	/* Planning may have run a function that reloaded the catalog cache. */
	const KsCatalog *cat = kingres_shard_catalog();
	ListCell   *lc;
	int			found = 0;
	Oid			first = InvalidOid;

	/* The empty catalog that a nested call sees while the cache loads. */
	if (!cat->present || cat->dist_tables == NULL)
		return;

	foreach(lc, pstmt->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		if (rte->rtekind != RTE_RELATION)
			continue;
		if (hash_search(cat->dist_tables, &rte->relid, HASH_FIND, NULL) == NULL)
			continue;
		if (++found == 1)
			first = rte->relid;
	}
	if (found <= expected)
		return;

	if (kingres_shard_forwarded)
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
	PlannedStmt *pstmt = pass_through(parse, query_string, cursorOptions,
									  boundParams);

	check_plan_dist_rtes(pstmt, expected);
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
			if (ref->dt->coloc->bucket_group[ref->bucket] != cat->local_group_id)
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
 * kingres_shard_planner_hook
 *
 * See the file header.
 */
static PlannedStmt *
kingres_shard_planner_hook(Query *parse, const char *query_string,
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
	List	   *relids = NIL;
	bool		is_write;

	if (!kingres_shard_enable_routing || creating_extension)
		return pass_through(parse, query_string, cursorOptions, boundParams);
	if (parse->commandType != CMD_SELECT && parse->commandType != CMD_INSERT &&
		parse->commandType != CMD_UPDATE && parse->commandType != CMD_DELETE &&
		parse->commandType != CMD_MERGE)
		return pass_through(parse, query_string, cursorOptions, boundParams);

	cat = kingres_shard_catalog();
	if (!cat->present || cat->ndist_tables == 0 || cat->local_group_id < 0)
		return pass_through(parse, query_string, cursorOptions, boundParams);

	/* With one shard group every bucket is local. */
	if (cat->ngroups <= 1 && !kingres_shard_forwarded)
		return pass_through(parse, query_string, cursorOptions, boundParams);

	memset(&a, 0, sizeof(a));
	a.params = boundParams;
	a.parse = parse;
	ctx[0] = &a;
	ctx[1] = (void *) cat;
	(void) analyze_walker((Node *) parse, ctx);

	if (a.refs == NIL)
		return plan_locally(parse, query_string, cursorOptions, boundParams,
							cat, a.ndist_rtes);

	if (kingres_shard_forwarded)
		return plan_forwarded(parse, query_string, cursorOptions, boundParams,
							  &a, cat);

	first = (DistRef *) linitial(a.refs);
	colocation_id = first->dt->colocation_id;

	foreach(lc, a.refs)
	{
		DistRef    *ref = (DistRef *) lfirst(lc);

		relids = list_append_unique_oid(relids, ref->dt->relid);
		if (ref->dt->colocation_id != colocation_id)
			error_spans_groups(ref, "The statement uses distributed tables of different colocation groups.");
		if (ref->bucket < 0 && !ref->param_pending)
			error_spans_groups(ref, NULL);
		if (ref->param_pending)
			pending = true;
		else if (bucket < 0)
			bucket = ref->bucket;
		else if (bucket != ref->bucket)
			error_spans_groups(ref, "The keys of the statement hash to different buckets.");
	}

	if (pending)
	{
		/* The routing depends on a parameter whose value is not known yet. */
		return kingres_shard_make_deferred_plan(parse, relids);
	}

	group = first->dt->coloc->bucket_group[bucket];
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

		is_write = parse->commandType != CMD_SELECT || parse->rowMarks != NIL ||
			parse->hasModifyingCTE;
		sql = deparse_for_remote(parse);
		return kingres_shard_make_remote_plan(parse, sql, node->host,
											   node->port, group, bucket,
											   is_write, relids);
	}
}
