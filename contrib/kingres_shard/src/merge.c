/*-------------------------------------------------------------------------
 *
 * merge.c
 *	  MERGE on distributed tables (design/analytics-phase12-queries.md, section 7): the settings,
 *	  the classification of a statement, and the checks that a receiving group makes of a MERGE
 *	  fragment.
 *
 * Two shapes run on the shard groups:
 *
 *	M1  colocated: the target and every distributed table of USING are of one colocation group
 *		and joined to the target on their distribution keys (or USING reads reference tables
 *		only).  The whole statement runs on every group that owns a bucket of the target, over the
 *		buckets of that group: a target row and every source row that can match it are on one
 *		group.
 *
 *	M3  routed source: ON has a conjunct "target.key = e" where e is an expression of the one
 *		relation of USING.  The origin reads that relation as a SELECT of its own and sends each
 *		row to the group that owns the bucket of e; each group runs the MERGE over the rows it
 *		got (a sub-select over unnest() of array parameters stands for USING, design 3.1).
 *
 * Every other shape is refused with an error that names the condition that is not met.  What a
 * group accepts is decided by kingres_shard_merge_fragment_analyze() on the parse of the text it
 * received, and anything it does not recognise is refused: a forged fragment is never run.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/merge.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/genam.h"
#include "access/htup_details.h"
#include "access/table.h"
#include "catalog/pg_class.h"
#include "catalog/pg_collation_d.h"
#include "catalog/pg_index.h"
#include "catalog/pg_type_d.h"
#include "executor/spi.h"
#include "lib/stringinfo.h"
#include "miscadmin.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "nodes/params.h"
#include "optimizer/optimizer.h"
#include "parser/analyze.h"
#include "parser/parse_node.h"
#include "parser/parse_relation.h"
#include "parser/parsetree.h"
#include "rewrite/rewriteHandler.h"
#include "tcop/tcopprot.h"
#include "utils/acl.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/ruleutils.h"
#include "utils/rel.h"
#include "utils/syscache.h"
#include "utils/typcache.h"

#include "catalog.h"
#include "hashing.h"
#include "kingres_shard.h"
#include "merge.h"
#include "node_conn.h"
#include "pushdown.h"
#include "repartition.h"
#include "shippable.h"

bool		kingres_shard_enable_distributed_merge = true;
int			kingres_shard_merge_max_source_kb = 65536;

void
kingres_shard_merge_init(void)
{
	DefineCustomBoolVariable("kingres_shard.enable_distributed_merge",
							 "Run a MERGE into a distributed table on the shard groups.",
							 "When off, such a MERGE is refused as in phase 2a: \"... is not supported\".",
							 &kingres_shard_enable_distributed_merge,
							 true,
							 PGC_USERSET, 0,
							 NULL, NULL, NULL);
	DefineCustomIntVariable("kingres_shard.merge_max_source_bytes",
							"The most that this node holds of the source rows of a MERGE that it routes to the shard groups.",
							"A MERGE whose source needs more fails before any group runs anything; split the source by key range or raise the setting.",
							&kingres_shard_merge_max_source_kb,
							65536, 1024, 524288,
							PGC_USERSET, GUC_UNIT_KB,
							NULL, NULL, NULL);
}

/* ======================================================================
 * Small helpers
 * ====================================================================== */

static Node *
mg_strip_relabel(Node *n)
{
	while (n != NULL && IsA(n, RelabelType))
		n = (Node *) ((RelabelType *) n)->arg;
	return n;
}

static void
mg_flatten_and(Node *qual, List **out)
{
	if (qual == NULL)
		return;
	if (IsA(qual, BoolExpr) && ((BoolExpr *) qual)->boolop == AND_EXPR)
	{
		ListCell   *lc;

		foreach(lc, ((BoolExpr *) qual)->args)
			mg_flatten_and((Node *) lfirst(lc), out);
		return;
	}
	*out = lappend(*out, qual);
}

static bool
mg_relation_has_rls(Oid relid)
{
	HeapTuple	tup = SearchSysCache1(RELOID, ObjectIdGetDatum(relid));
	bool		rls;

	if (!HeapTupleIsValid(tup))
		return true;
	rls = ((Form_pg_class) GETSTRUCT(tup))->relrowsecurity;
	ReleaseSysCache(tup);
	return rls;
}

/* A trigger of a user, or a rule, on the target would run on the group, in the group's session. */
static bool
mg_target_has_user_code(Oid relid)
{
	Relation	rel = table_open(relid, NoLock);
	bool		found = (rel->rd_rules != NULL);

	if (rel->trigdesc != NULL)
	{
		int			i;

		for (i = 0; i < rel->trigdesc->numtriggers; i++)
			if (!rel->trigdesc->triggers[i].tgisinternal)
				found = true;
	}
	table_close(rel, NoLock);
	return found;
}

/* Every unique index has the distribution column among its keys, under the collation of the distribution. */
static bool
mg_unique_indexes_carry_key(Oid relid, AttrNumber dist_attno, Oid collation)
{
	Relation	rel = table_open(relid, NoLock);
	List	   *indexes = RelationGetIndexList(rel);
	ListCell   *lc;
	bool		ok = true;

	foreach(lc, indexes)
	{
		HeapTuple	tup = SearchSysCache1(INDEXRELID, ObjectIdGetDatum(lfirst_oid(lc)));
		Form_pg_index ix;

		if (!HeapTupleIsValid(tup))
		{
			ok = false;
			break;
		}
		ix = (Form_pg_index) GETSTRUCT(tup);
		if (ix->indisunique || ix->indisexclusion)
		{
			oidvector  *colls = (oidvector *) DatumGetPointer(SysCacheGetAttrNotNull(INDEXRELID, tup,
																					 Anum_pg_index_indcollation));
			int			k;
			bool		found = false;

			for (k = 0; k < ix->indnkeyatts; k++)
			{
				if (ix->indkey.values[k] == dist_attno)
				{
					found = (colls->values[k] == collation);
					break;
				}
			}
			if (!found)
				ok = false;
		}
		ReleaseSysCache(tup);
		if (!ok)
			break;
	}
	list_free(indexes);
	table_close(rel, NoLock);
	return ok;
}

/* ======================================================================
 * The checks that M1 and M3 share (7.2, the refusals of every case)
 * ====================================================================== */

/*
 * The entry of a table of the catalog, copied: every call that reads the catalog (the analyses, the deparse,
 * the parse of a text, a cursor of SPI) can make this backend load the cache again, and a pointer into the
 * old one is then dangling.  The copy and its colocation group stay valid for the statement.
 */
static const KsDistTable *
mg_lookup_stable(Oid relid)
{
	const KsDistTable *dt = kingres_shard_lookup_dist_table(relid);
	KsDistTable *copy;

	if (dt == NULL)
		return NULL;
	copy = palloc(sizeof(KsDistTable));
	memcpy(copy, dt, sizeof(KsDistTable));
	if (dt->coloc != NULL)
		copy->coloc = kingres_shard_coloc_copy(dt->coloc);
	return copy;
}

#define MG_NO(msg) do { *why = (msg); return false; } while (0)

/*
 * The rewriter keeps the entry of a view in the range table of a statement written through it, next to
 * the table it resolved to.  The filter of such a view (a plain view: the jointree; a security_barrier view:
 * securityQuals) is not part of the text a MERGE is deparsed to (a MERGE has no WHERE clause), so a MERGE
 * through a view would change rows the view does not show.  It is refused outright (review P12M-02); the
 * guards below it (the owner of the view, row-level security, CHECK OPTION) stay as the first layer, so that
 * each of them has its own message and its own test.  The name of the view, or NULL.
 */
static const char *
mg_view_in_rtable(Query *q)
{
	ListCell   *lc;

	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		if (rte->rtekind == RTE_RELATION && rte->relkind == RELKIND_VIEW)
			return get_rel_name(rte->relid);
	}
	return NULL;
}

#define MG_NO_VIEW(q) \
	do { \
		const char *vname_ = mg_view_in_rtable(q); \
		if (vname_ != NULL) \
			MG_NO(psprintf("MERGE through a view is not supported (the statement was written through view \"%s\")", vname_)); \
	} while (0)

/*
 * A column that the INSERT action does not list is filled with its default.  A default that is stable
 * (now(), current_user, current_setting()) would be computed by each group for itself, which can differ
 * from group to group; the refusal names the column and the way out (review P12M-06).  Volatile defaults
 * (a sequence, an identity column, random()) are refused by the volatile-value rule of the callers.
 */
static bool
mg_stable_default_insert(Query *q, Oid relid, const char **why)
{
	ListCell   *lc;

	foreach(lc, q->mergeActionList)
	{
		MergeAction *ma = lfirst_node(MergeAction, lc);
		ListCell   *lc2;
		Relation	rel = NULL;
		const char *colname = NULL;

		if (ma->commandType != CMD_INSERT)
			continue;
		foreach(lc2, ma->targetList)
		{
			TargetEntry *te = lfirst_node(TargetEntry, lc2);
			Node	   *def;

			if (te->resjunk || colname != NULL)
				continue;
			if (contain_volatile_functions((Node *) te->expr) || !contain_mutable_functions((Node *) te->expr))
				continue;
			if (rel == NULL)
				rel = table_open(relid, NoLock);
			def = build_column_default(rel, te->resno);
			if (def != NULL && equal(def, te->expr))
				colname = pstrdup(NameStr(TupleDescAttr(rel->rd_att, te->resno - 1)->attname));
		}
		if (rel != NULL)
			table_close(rel, NoLock);
		if (colname != NULL)
			MG_NO(psprintf("column \"%s\" of the target has a default that is stable (now(), current_user, current_setting()), which each group would compute for itself: give it a value in the INSERT list", colname));
	}
	return true;
}

/*
 * What the deparsed text of a MERGE cannot carry about its target: the policies of row-level security and the
 * CHECK OPTION of a view (withCheckOptions), the filter of a row-level security policy or of a security_barrier
 * view (securityQuals), the rights of the owner of a view (checkAsUser), and the view itself (its filter, in
 * the jointree, is not a clause of a MERGE).  Every shape that ships the statement or a text made of it (M1, M3,
 * M4, M5) asks this first, on the statement as the user wrote it: the shapes M4 and M5 are tried one after the
 * other, and a refusal of M4 must not let M5 build a text that has lost the filter (review P12M-02).  Each
 * guard has its own message, so that a test can tell which one refused.
 */
static bool
mg_target_guards(Query *q, RangeTblEntry *trte, const char **why)
{
	RTEPermissionInfo *tperm;

	if (q->withCheckOptions != NIL)
		MG_NO("the rows are checked against a view or a row-level security policy");
	if (trte->securityQuals != NIL || mg_relation_has_rls(trte->relid))
		MG_NO("the target has row-level security");
	tperm = getRTEPermissionInfo(q->rteperminfos, trte);
	if (OidIsValid(tperm->checkAsUser) && tperm->checkAsUser != GetUserId())
		MG_NO("the target is written with the rights of another role (a view of another owner)");
	MG_NO_VIEW(q);
	return true;
}

static bool
merge_common(Query *q, const KsDistTable *tdt, const char **why)
{
	RangeTblEntry *trte = rt_fetch(q->resultRelation, q->rtable);
	ListCell   *lc;

	if (q->cteList != NIL || q->hasRecursive || q->hasModifyingCTE)
		MG_NO("it has a WITH clause");
	if (q->hasSubLinks)
		MG_NO("it has a sub-select in an expression (ON, a WHEN condition, SET, VALUES or RETURNING)");
	if (trte->rtekind != RTE_RELATION || trte->relkind != RELKIND_PARTITIONED_TABLE)
		MG_NO("its target is not a partitioned (distributed) table");
	if (!mg_target_guards(q, trte, why))
		return false;
	if (mg_target_has_user_code(trte->relid))
		MG_NO("the target has a trigger or a rule, which would run on the shard groups");
	if (!mg_unique_indexes_carry_key(trte->relid, tdt->dist_attno, tdt->coloc->collation))
		MG_NO("a unique index of the target does not include the distribution column under the collation of its distribution");
	if (!mg_stable_default_insert(q, trte->relid, why))
		return false;

	foreach(lc, q->mergeActionList)
	{
		MergeAction *ma = lfirst_node(MergeAction, lc);
		ListCell   *lc2;

		if (ma->commandType == CMD_UPDATE)
		{
			/* (updateColnos is made by the planner later: the entries of the target list carry the column numbers) */
			foreach(lc2, ma->targetList)
			{
				TargetEntry *ute = lfirst_node(TargetEntry, lc2);

				if (!ute->resjunk && ute->resno == tdt->dist_attno)
					MG_NO("an UPDATE action assigns the distribution column (the row would have to move to another bucket)");
			}
		}
		if (ma->commandType == CMD_INSERT || ma->commandType == CMD_UPDATE)
		{
			foreach(lc2, ma->targetList)
			{
				TargetEntry *te = lfirst_node(TargetEntry, lc2);

				if (!te->resjunk && contain_volatile_functions((Node *) te->expr))
					MG_NO("an action uses a volatile value (a sequence, an identity column, random()) that each group would compute for itself: give the column a value in the INSERT list, or use a value that is not volatile");
			}
		}
	}
	return true;
}

/* The join the planner makes of a MERGE: inner, left (NOT MATCHED BY SOURCE), right (BY TARGET) or full. */
static JoinType
merge_join_type(Query *q, bool *nmbt, bool *nmbs)
{
	ListCell   *lc;

	*nmbt = false;
	*nmbs = false;
	foreach(lc, q->mergeActionList)
	{
		MergeAction *ma = lfirst_node(MergeAction, lc);

		if (ma->matchKind == MERGE_WHEN_NOT_MATCHED_BY_TARGET)
			*nmbt = true;
		else if (ma->matchKind == MERGE_WHEN_NOT_MATCHED_BY_SOURCE)
			*nmbs = true;
	}
	if (*nmbt && *nmbs)
		return JOIN_FULL;
	if (*nmbt)
		return JOIN_RIGHT;
	if (*nmbs)
		return JOIN_LEFT;
	return JOIN_INNER;
}

/*
 * The SELECT that stands for the join of a MERGE (what transform_MERGE_to_join makes), so that the
 * analysis of a pushdown unit applies: the target and the USING clause, joined on the ON clause.
 */
static Query *
merge_as_select(Query *m)
{
	Query	   *s;
	JoinExpr   *j;
	RangeTblRef *tr;
	bool		nmbt;
	bool		nmbs;

	/*
	 * The filter of a view that the statement was written through is in jointree->quals or in securityQuals,
	 * and this SELECT has neither: it must never be built for such a statement (merge_common refuses it first;
	 * this is the second layer, review P12M-02).
	 */
	if (mg_view_in_rtable(m) != NULL)
		return NULL;
	s = copyObject(m);
	j = makeNode(JoinExpr);
	tr = makeNode(RangeTblRef);
	j->jointype = merge_join_type(m, &nmbt, &nmbs);
	tr->rtindex = m->resultRelation;
	j->larg = (Node *) tr;
	if (list_length(s->jointree->fromlist) != 1)
		return NULL;
	j->rarg = (Node *) linitial(s->jointree->fromlist);
	j->quals = s->mergeJoinCondition;
	j->rtindex = 0;
	s->commandType = CMD_SELECT;
	s->resultRelation = 0;
	s->mergeActionList = NIL;
	s->mergeJoinCondition = NULL;
	s->returningList = NIL;
	s->targetList = NIL;
	s->jointree = makeFromExpr(list_make1(j), NULL);
	return s;
}

/* An INSERT action sets the distribution column from something that decides its group. */
typedef bool (*MergeInsertKeyOk) (Node *expr, void *arg);

static bool
merge_insert_keys(Query *q, AttrNumber dist_attno, MergeInsertKeyOk ok, void *arg, const char **why)
{
	ListCell   *lc;

	foreach(lc, q->mergeActionList)
	{
		MergeAction *ma = lfirst_node(MergeAction, lc);
		ListCell   *lc2;
		TargetEntry *keyte = NULL;

		if (ma->commandType != CMD_INSERT)
			continue;
		foreach(lc2, ma->targetList)
		{
			TargetEntry *te = lfirst_node(TargetEntry, lc2);

			if (!te->resjunk && te->resno == dist_attno)
				keyte = te;
		}
		if (keyte == NULL)
			MG_NO("a WHEN NOT MATCHED INSERT action gives the distribution column no value");
		if (!ok((Node *) keyte->expr, arg))
			MG_NO("the distribution column in a WHEN NOT MATCHED INSERT action is not the key that the statement is colocated or routed by (the inserted row would belong to another shard group)");
	}
	return true;
}

/* ======================================================================
 * M1: colocated
 * ====================================================================== */

typedef struct M1KeyArg
{
	const KsPushdown *pd;
	Index		target_rti;
	Oid			dist_type;
} M1KeyArg;

static bool
m1_insert_key_ok(Node *expr, void *arg)
{
	M1KeyArg   *a = (M1KeyArg *) arg;
	Index		rti = kingres_shard_pushdown_key_rti(a->pd, expr);

	return rti != 0 && rti != a->target_rti && exprType(expr) == a->dist_type;
}

static bool
merge_m1_shape(Query *q, const KsDistTable *tdt, KsMerge *m, const char **why)
{
	Query	   *s;
	KsPushdown	pd;
	ListCell   *lc;
	Index		rti = 0;
	M1KeyArg	ka;
	const char *r = NULL;

	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		rti++;
		if (rti == (Index) q->resultRelation)
			continue;
		if (rte->rtekind != RTE_RELATION && rte->rtekind != RTE_JOIN)
			MG_NO("USING reads something other than tables (a sub-select, a function, a VALUES list)");
	}
	s = merge_as_select(q);
	if (s == NULL)
		MG_NO("USING is not one table or join");
	if (!kingres_shard_pushdown_analyze(s, &pd, &r))
		MG_NO(psprintf("the MERGE is not colocated: %s", r));
	if (pd.colocation_id != tdt->colocation_id)
		MG_NO("USING reads distributed tables of another colocation group than the target");
	ka.pd = &pd;
	ka.target_rti = q->resultRelation;
	ka.dist_type = pd.coloc->dist_type;
	if (!merge_insert_keys(q, tdt->dist_attno, m1_insert_key_ok, &ka, why))
		return false;
	m->kind = KS_MERGE_COLOCATED;
	return true;
}

/* ======================================================================
 * M3: routed source
 * ====================================================================== */

/*
 * The conjunct of ON that equates the distribution column of the target with an expression of
 * the source (7.4 condition 1).  *route is that expression.
 */
static bool
merge_find_route(Query *q, const KsDistTable *tdt, Index src_rti, Node **route)
{
	List	   *conj = NIL;
	ListCell   *lc;
	TypeCacheEntry *tce = lookup_type_cache(tdt->coloc->dist_type, TYPECACHE_HASH_OPFAMILY);

	if (!OidIsValid(tce->hash_opf))
		return false;
	mg_flatten_and(q->mergeJoinCondition, &conj);
	foreach(lc, conj)
	{
		OpExpr	   *op;
		Node	   *l;
		Node	   *r;
		Node	   *e;
		List	   *vars;
		ListCell   *lc2;
		bool		onlysrc = true;

		if (!IsA(lfirst(lc), OpExpr))
			continue;
		op = (OpExpr *) lfirst(lc);
		if (list_length(op->args) != 2 || !op_in_opfamily(op->opno, tce->hash_opf))
			continue;
		if (op->inputcollid != tdt->coloc->collation ||
			(OidIsValid(op->inputcollid) && !get_collation_isdeterministic(op->inputcollid)))
			continue;
		l = mg_strip_relabel((Node *) linitial(op->args));
		r = mg_strip_relabel((Node *) lsecond(op->args));
		if (l != NULL && IsA(l, Var) && ((Var *) l)->varno == (Index) q->resultRelation &&
			((Var *) l)->varlevelsup == 0 && ((Var *) l)->varattno == tdt->dist_attno)
		{
			e = (Node *) lsecond(op->args);
		}
		else if (r != NULL && IsA(r, Var) && ((Var *) r)->varno == (Index) q->resultRelation &&
				 ((Var *) r)->varlevelsup == 0 && ((Var *) r)->varattno == tdt->dist_attno)
		{
			e = (Node *) linitial(op->args);
		}
		else
			continue;
		if (contain_volatile_functions(e))
			continue;
		vars = pull_var_clause(e, 0);
		foreach(lc2, vars)
		{
			Var		   *v = (Var *) lfirst(lc2);

			if (v->varlevelsup != 0 || v->varno != src_rti || v->varattno <= 0)
				onlysrc = false;
		}
		if (!onlysrc)
			continue;
		*route = e;
		return true;
	}
	return false;
}

typedef struct M3KeyArg
{
	Node	   *route;
} M3KeyArg;

static bool
m3_insert_key_ok(Node *expr, void *arg)
{
	M3KeyArg   *a = (M3KeyArg *) arg;

	return equal(mg_strip_relabel(expr), mg_strip_relabel(a->route));
}

/*
 * The shape of M3 that does not depend on what the one relation of USING is: it is one relation of
 * the join tree, the ON clause equates the key with an expression of it, the INSERT actions give
 * the distribution column that expression.  *src_rti the relation.
 */
static bool
merge_m3_common(Query *q, const KsDistTable *tdt, KsMerge *m, const char **why)
{
	Index		src = 0;
	M3KeyArg	ka;
	Node	   *route = NULL;

	if (q->jointree == NULL || list_length(q->jointree->fromlist) != 1 ||
		!IsA(linitial(q->jointree->fromlist), RangeTblRef) ||
		q->jointree->quals != NULL)
		MG_NO("USING is a join or several relations; put it in a sub-select so that the source is one relation");
	src = (Index) ((RangeTblRef *) linitial(q->jointree->fromlist))->rtindex;
	if (src == (Index) q->resultRelation)
		MG_NO("USING is the target");
	if (!merge_find_route(q, tdt, src, &route))
		MG_NO("the ON clause of a MERGE into a distributed table must equate its distribution column with a column or expression of the source");
	ka.route = route;
	if (!merge_insert_keys(q, tdt->dist_attno, m3_insert_key_ok, &ka, why))
		return false;
	m->source_rti = src;
	m->route = route;
	m->kind = KS_MERGE_ROUTED;
	return true;
}

/*
 * M3 as the origin sees the statement: the relation of USING is anything that a SELECT reads; the
 * columns the statement uses are shipped as text, so their types are built in and their
 * collation is the default.
 */
static bool
merge_m3_origin(Query *q, const KsDistTable *tdt, KsMerge *m, const char **why)
{
	RangeTblEntry *srte;
	List	   *vars;
	ListCell   *lc;

	if (!merge_m3_common(q, tdt, m, why))
		return false;
	srte = rt_fetch(m->source_rti, q->rtable);
	if (srte->rtekind != RTE_RELATION && srte->rtekind != RTE_SUBQUERY &&
		srte->rtekind != RTE_VALUES && srte->rtekind != RTE_FUNCTION)
		MG_NO("USING is not a table, a sub-select, a VALUES list or a function");
	if (srte->lateral)
		MG_NO("USING is a LATERAL item");

	vars = pull_var_clause((Node *) q->mergeJoinCondition, 0);
	vars = list_concat(vars, pull_var_clause((Node *) q->mergeActionList, 0));
	vars = list_concat(vars, pull_var_clause((Node *) q->returningList, 0));
	foreach(lc, vars)
	{
		Var		   *v = (Var *) lfirst(lc);

		if (v->varno != m->source_rti || v->varlevelsup != 0)
			continue;
		if (v->varattno <= 0)
			MG_NO("the statement uses a whole row of the source or one of its system columns");
		if (v->vartype >= FirstGenbkiObjectId)
			MG_NO("a column of the source has a type that is not built in");
		if (OidIsValid(v->varcollid) && v->varcollid != DEFAULT_COLLATION_OID)
			MG_NO("a column of the source has a collation other than the default");
	}
	return true;
}

/* ======================================================================
 * The shipped source (design 3.1): the text of M3
 * ====================================================================== */

typedef struct ParamScan
{
	int			maxid;
} ParamScan;

static bool
merge_param_walker(Node *node, ParamScan *ps)
{
	if (node == NULL)
		return false;
	if (IsA(node, Param))
	{
		Param	   *p = (Param *) node;

		if (p->paramkind == PARAM_EXTERN && p->paramid > ps->maxid)
			ps->maxid = p->paramid;
		return false;
	}
	if (IsA(node, Query))
		return query_tree_walker((Query *) node, merge_param_walker, ps, 0);
	return expression_tree_walker(node, merge_param_walker, ps);
}

/* The types of the external parameters of a statement: the parse of its text needs them. */
typedef struct ParamTypes
{
	Oid		   *types;
	int			n;
} ParamTypes;

static bool
merge_param_types_walker(Node *node, ParamTypes *pt)
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
		return query_tree_walker((Query *) node, merge_param_types_walker, pt, 0);
	return expression_tree_walker(node, merge_param_types_walker, pt);
}

/*
 * Parse and rewrite the text of a statement under the transmission modes, the way a receiving group
 * does.  NULL with *why when the text is not one statement.
 */
static Query *
merge_parse_text(const char *text, Oid *ptypes, int nparams, const char **why)
{
	List	   *raw;
	List	   *rewritten;
	Query	   *rq;
	int			nest = kingres_shard_set_transmission_modes();

	raw = pg_parse_query(text);
	if (list_length(raw) != 1)
	{
		kingres_shard_reset_transmission_modes(nest);
		*why = "the deparsed text is not one statement";
		return NULL;
	}
	rq = parse_analyze_fixedparams(linitial_node(RawStmt, raw), text, ptypes, nparams, NULL);
	rewritten = pg_rewrite_query(rq);
	kingres_shard_reset_transmission_modes(nest);
	if (list_length(rewritten) != 1)
	{
		*why = "the deparsed text is rewritten into several statements";
		return NULL;
	}
	return linitial_node(Query, rewritten);
}

/*
 * Make the shipped form of an M3 statement: the one relation of USING becomes the sub-select over
 * unnest() of one text[] parameter per shipped column (the columns the statement uses, in the order
 * of the relation), numbered after the external parameters of the statement; and the SELECT that the
 * origin runs to read the source: those columns and the route expression, last.
 */
bool
kingres_shard_merge_build_ship(Query *q, const KsMerge *m, KsMergeShip *sh, const char **why)
{
	RangeTblEntry *srte = rt_fetch(m->source_rti, q->rtable);
	int			natts = list_length(srte->eref->colnames);
	List	   *vars;
	ListCell   *lc;
	Oid		   *vtype;
	int32	   *vtypmod;
	Oid		   *vcoll;
	bool	   *used;
	int			a;
	int			k = 0;
	ParamScan	ps = {0};
	ParamTypes	pt = {NULL, 0};
	Query	   *sm;
	Query	   *sq;
	RangeTblEntry *rte;
	StringInfoData sel;
	StringInfoData from;
	StringInfoData sql;
	List	   *attnos = NIL;
	List	   *tlist = NIL;
	int			nest;
	Query	   *sub;
	Query	   *reparsed;
	Oid		   *ptypes;
	int			i;

	memset(sh, 0, sizeof(*sh));
	/* the text below is deparsed from the statement, which cannot carry these (the callers ask too; this is the layer under them) */
	if (!mg_target_guards(q, rt_fetch(q->resultRelation, q->rtable), why))
		return false;
	vtype = palloc0(sizeof(Oid) * (natts + 1));
	vtypmod = palloc0(sizeof(int32) * (natts + 1));
	vcoll = palloc0(sizeof(Oid) * (natts + 1));
	used = palloc0(sizeof(bool) * (natts + 1));

	vars = pull_var_clause((Node *) q->mergeJoinCondition, 0);
	vars = list_concat(vars, pull_var_clause((Node *) q->mergeActionList, 0));
	vars = list_concat(vars, pull_var_clause((Node *) q->returningList, 0));
	foreach(lc, vars)
	{
		Var		   *v = (Var *) lfirst(lc);

		if (v->varno != m->source_rti || v->varlevelsup != 0)
			continue;
		if (v->varattno <= 0 || v->varattno > natts)
			MG_NO("the statement uses a whole row of the source or one of its system columns");
		used[v->varattno] = true;
		vtype[v->varattno] = v->vartype;
		vtypmod[v->varattno] = v->vartypmod;
		vcoll[v->varattno] = v->varcollid;
	}
	(void) merge_param_walker((Node *) q, &ps);
	(void) merge_param_types_walker((Node *) q, &pt);
	sh->nparams_ext = ps.maxid;
	sh->first_param = ps.maxid + 1;

	/* the columns shipped; a source of which the statement uses no column ships its first column as a row counter */
	for (a = 1; a <= natts; a++)
		if (used[a])
			attnos = lappend_int(attnos, a);
	if (attnos == NIL)
		attnos = list_make1_int(1);
	k = list_length(attnos);

	/* the SELECT the origin runs */
	sq = copyObject(q);
	sq->commandType = CMD_SELECT;
	sq->resultRelation = 0;
	sq->mergeTargetRelation = 0;
	sq->mergeActionList = NIL;
	sq->mergeJoinCondition = NULL;
	sq->returningList = NIL;
	{
		RangeTblRef *rtr = makeNode(RangeTblRef);
		int			resno = 0;

		rtr->rtindex = m->source_rti;
		sq->jointree = makeFromExpr(list_make1(rtr), NULL);
		foreach(lc, attnos)
		{
			int			att = lfirst_int(lc);

			resno++;
			if (used[att])
				tlist = lappend(tlist,
								makeTargetEntry((Expr *) makeVar(m->source_rti, (AttrNumber) att, vtype[att],
																 vtypmod[att], vcoll[att], 0),
												resno, psprintf("ks_c%d", resno), false));
			else
				tlist = lappend(tlist,
								makeTargetEntry((Expr *) makeConst(INT4OID, -1, InvalidOid, sizeof(int32),
																   Int32GetDatum(1), false, true),
												resno, psprintf("ks_c%d", resno), false));
		}
		if (m->route != NULL)
		{
			resno++;
			tlist = lappend(tlist, makeTargetEntry((Expr *) copyObject(m->route), resno, "ks_route", false));
		}
		sq->targetList = tlist;
		sq->hasSubLinks = false;
	}
	nest = kingres_shard_set_transmission_modes();
	sh->source_sql = pg_get_querydef(sq, false);
	kingres_shard_reset_transmission_modes(nest);
	sh->broadcast = (m->route == NULL);
	sh->route_type = m->route != NULL ? exprType((Node *) m->route) : InvalidOid;

	/* the sub-select that stands for the source in the statement the groups run */
	initStringInfo(&sel);
	initStringInfo(&from);
	appendStringInfoString(&from, "ROWS FROM (");
	for (a = 1; a <= natts; a++)
	{
		const char *name = strVal(list_nth(srte->eref->colnames, a - 1));
		int			pos = -1;
		int			col;
		int			idx = 0;
		ListCell   *plc;

		if (name[0] == '\0')
			name = psprintf("ks_dropped%d", a);
		foreach(plc, attnos)
		{
			if (lfirst_int(plc) == a)
				pos = idx;
			idx++;
		}
		if (a > 1)
			appendStringInfoString(&sel, ", ");
		if (pos < 0)
		{
			appendStringInfo(&sel, "NULL::pg_catalog.text AS %s", quote_identifier(name));
			continue;
		}
		col = pos + 1;
		if (!used[a] || vtype[a] == TEXTOID)
			appendStringInfo(&sel, "u.c%d AS %s", col, quote_identifier(name));
		else
		{
			if (vtype[a] >= FirstGenbkiObjectId)
				MG_NO("a column of the source has a type that is not built in");
			appendStringInfo(&sel, "u.c%d::%s AS %s", col,
							 format_type_extended(vtype[a], -1, FORMAT_TYPE_FORCE_QUALIFY), quote_identifier(name));
		}
	}
	for (i = 1; i <= k; i++)
		appendStringInfo(&from, "%spg_catalog.unnest($%d::pg_catalog.text[])", i > 1 ? ", " : "", sh->first_param + i - 1);
	appendStringInfoString(&from, ") AS u(");
	for (i = 1; i <= k; i++)
		appendStringInfo(&from, "%sc%d", i > 1 ? ", " : "", i);
	appendStringInfoChar(&from, ')');
	initStringInfo(&sql);
	appendStringInfo(&sql, "SELECT %s FROM %s", sel.data, from.data);

	ptypes = palloc0(sizeof(Oid) * (sh->nparams_ext + k + 1));
	for (i = 0; i < sh->nparams_ext; i++)
		ptypes[i] = (i < pt.n && OidIsValid(pt.types[i])) ? pt.types[i] : TEXTOID;
	for (i = 0; i < k; i++)
		ptypes[sh->nparams_ext + i] = TEXTARRAYOID;
	sub = merge_parse_text(sql.data, ptypes, sh->nparams_ext + k, why);
	if (sub == NULL)
		return false;
	if (!kingres_shard_repart_shipped_shape(sub, NULL))
		MG_NO("the sub-select of the shipped source does not have the shape the groups accept");

	sm = copyObject(q);
	rte = rt_fetch(m->source_rti, sm->rtable);
	if (rte->alias == NULL)
		rte->alias = makeAlias(rte->eref->aliasname, NIL);
	rte->rtekind = RTE_SUBQUERY;
	rte->subquery = sub;
	rte->relid = InvalidOid;
	rte->relkind = 0;
	rte->inh = false;
	rte->rellockmode = 0;
	rte->perminfoindex = 0;
	rte->tablesample = NULL;
	rte->securityQuals = NIL;
	rte->lateral = false;
	rte->functions = NIL;
	rte->funcordinality = false;
	rte->values_lists = NIL;
	rte->coltypes = NIL;
	rte->coltypmods = NIL;
	rte->colcollations = NIL;
	rte->security_barrier = false;

	nest = kingres_shard_set_transmission_modes();
	sh->text = pg_get_querydef(sm, false);
	kingres_shard_reset_transmission_modes(nest);

	/* the decision of a group is made on the parse of this text: so is the origin's */
	reparsed = merge_parse_text(sh->text, ptypes, sh->nparams_ext + k, why);
	if (reparsed == NULL)
		return false;
	{
		KsMerge		m2;
		int			nsh = 0;

		if (m->route != NULL)
		{
			if (!kingres_shard_merge_fragment_analyze(reparsed, 1, &m2, why))
				return false;
		}
		else if (!kingres_shard_merge_ref_fragment_analyze(reparsed, &nsh, why) || nsh != 1)
		{
			if (*why == NULL)
				*why = "the text has no shipped source";
			return false;
		}
	}
	sh->shipped = sm;
	sh->ncols = k;
	sh->attnos = attnos;
	return true;
}

/* ======================================================================
 * The decision
 * ====================================================================== */

static List *
merge_relids(Query *q)
{
	List	   *relids = NIL;
	ListCell   *lc;

	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		if (rte->rtekind == RTE_RELATION)
			relids = list_append_unique_oid(relids, rte->relid);
	}
	return relids;
}

static void
merge_fill(Query *q, const KsDistTable *tdt, KsMerge *m)
{
	RangeTblEntry *trte = rt_fetch(q->resultRelation, q->rtable);
	ListCell   *lc;

	m->target_relid = trte->relid;
	m->target_rti = q->resultRelation;
	m->colocation_id = tdt->colocation_id;
	m->dist_attno = tdt->dist_attno;
	m->dist_type = tdt->coloc->dist_type;
	m->relids = merge_relids(q);
	foreach(lc, q->mergeActionList)
	{
		MergeAction *ma = lfirst_node(MergeAction, lc);

		if (ma->matchKind == MERGE_WHEN_NOT_MATCHED_BY_SOURCE)
			m->has_nmbs = true;
		if (ma->commandType == CMD_INSERT)
			m->has_insert = true;
	}
}

bool
kingres_shard_merge_fragment_analyze(Query *q, int rp, KsMerge *m, const char **why)
{
	const KsDistTable *tdt;
	RangeTblEntry *trte;
	ListCell   *lc;
	int			nshipped = 0;
	Index		rti = 0;
	const char *reason = NULL;

	memset(m, 0, sizeof(*m));
	if (q->commandType != CMD_MERGE || q->resultRelation <= 0)
		MG_NO("it is not a MERGE");
	trte = rt_fetch(q->resultRelation, q->rtable);
	if (trte->rtekind != RTE_RELATION)
		MG_NO("its target is not a table");
	tdt = mg_lookup_stable(trte->relid);
	if (tdt == NULL || tdt->is_reference)
		MG_NO("its target is not a distributed table");
	merge_fill(q, tdt, m);
	if (!merge_common(q, tdt, why))
		return false;

	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		rti++;
		if (rti == (Index) q->resultRelation)
			continue;
		if (rte->rtekind == RTE_SUBQUERY && !rte->lateral && !rte->inh &&
			kingres_shard_repart_shipped_shape(rte->subquery, NULL))
			nshipped++;
	}
	if (nshipped != rp)
		MG_NO("the shipped sub-selects of the text are not the ones the marker counts");

	if (rp == 0)
	{
		if (!merge_m1_shape(q, tdt, m, why))
			return false;
		if (!kingres_shard_stmt_shippable_merge(q, tdt->colocation_id, &reason))
			MG_NO(reason ? reason : "it cannot be shipped");
		return true;
	}
	if (rp != 1)
		MG_NO("a MERGE has one shipped source");
	if (!merge_m3_common(q, tdt, m, why))
		return false;
	{
		RangeTblEntry *srte = rt_fetch(m->source_rti, q->rtable);

		if (srte->rtekind != RTE_SUBQUERY || !kingres_shard_repart_shipped_shape(srte->subquery, NULL))
			MG_NO("the source of the text is not the sub-select that stands for a shipped relation");
	}
	/* nothing else is read: the target and the shipped source */
	rti = 0;
	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		rti++;
		if (rti == (Index) q->resultRelation || rti == m->source_rti)
			continue;
		if (rte->rtekind != RTE_RESULT)
			MG_NO("the text reads a relation besides its target and the shipped source");
	}
	if (!kingres_shard_stmt_shippable_merge(q, tdt->colocation_id, &reason))
		MG_NO(reason ? reason : "it cannot be shipped");
	return true;
}

static void
merge_refuse(const KsDistTable *tdt, const char *what)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("this MERGE into distributed table \"%s\" cannot be run on the shard groups",
					get_rel_name(tdt->relid)),
			 errdetail("%s.", what),
			 errhint("A MERGE into a distributed table runs on every group that owns a bucket of the target (USING tables colocated with it and joined on their distribution keys), or reads its source here and sends each row to the group of its key (ON must equate the distribution column with an expression of the source).")));
}

bool
kingres_shard_merge_classify(Query *parse, KsMerge *m)
{
	const KsDistTable *tdt;
	RangeTblEntry *trte;
	const char *why1 = NULL;
	const char *why3 = NULL;
	const char *reason = NULL;
	KsMerge		m1;
	bool		src_plain = true;
	ListCell   *lc;
	Index		rti = 0;

	memset(m, 0, sizeof(*m));
	if (parse->commandType != CMD_MERGE || parse->resultRelation <= 0)
		return false;
	trte = rt_fetch(parse->resultRelation, parse->rtable);
	if (trte->rtekind != RTE_RELATION)
		return false;
	tdt = mg_lookup_stable(trte->relid);
	if (tdt == NULL || tdt->is_reference)
		return false;
	merge_fill(parse, tdt, m);
	if (!merge_common(parse, tdt, &why1))
		merge_refuse(tdt, why1);

	foreach(lc, parse->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		rti++;
		if (rti != (Index) parse->resultRelation && rte->rtekind != RTE_RELATION && rte->rtekind != RTE_JOIN)
			src_plain = false;
	}

	memset(&m1, 0, sizeof(m1));
	why1 = NULL;
	if (merge_m1_shape(parse, tdt, &m1, &why1))
	{
		if (kingres_shard_stmt_shippable_merge(parse, tdt->colocation_id, &reason))
		{
			m1.target_relid = m->target_relid;
			m1.target_rti = m->target_rti;
			m1.colocation_id = m->colocation_id;
			m1.dist_attno = m->dist_attno;
			m1.dist_type = m->dist_type;
			m1.has_nmbs = m->has_nmbs;
			m1.has_insert = m->has_insert;
			m1.relids = m->relids;
			*m = m1;
			return true;
		}
		why1 = reason ? reason : "it cannot be shipped";
	}

	if (merge_m3_origin(parse, tdt, m, &why3))
	{
		KsMergeShip sh;

		/* the statement as the groups would get it, decided as they decide it */
		if (kingres_shard_merge_build_ship(parse, m, &sh, &why3))
			return true;
		m->kind = KS_MERGE_NONE;
	}

	/*
	 * The message names the condition of the shape the statement is nearer to: a source of plain
	 * tables that failed M1 for a reason other than the join of the keys is reported with that
	 * reason, everything else with what M3 lacks.
	 */
	if (src_plain && why1 != NULL && strncmp(why1, "the MERGE is not colocated", 26) != 0)
		merge_refuse(tdt, why1);
	merge_refuse(tdt, why3 != NULL ? why3 : why1);
	return false;				/* not reached */
}

/* ======================================================================
 * The marker of a MERGE fragment, and the text of M1
 * ====================================================================== */

char *
kingres_shard_merge_marker(int64 place, int rp)
{
	return psprintf(KS_MERGE_MARKER_PREFIX "place=" INT64_FORMAT " rp=%d */ ", place, rp);
}

bool
kingres_shard_parse_merge_marker(const char *query_string, int64 *place, int *rp)
{
	const char *p;
	char	   *end;

	if (query_string == NULL ||
		strncmp(query_string, KS_MERGE_MARKER_PREFIX, strlen(KS_MERGE_MARKER_PREFIX)) != 0)
		return false;
	p = query_string + strlen(KS_MERGE_MARKER_PREFIX);
	if (strncmp(p, "place=", 6) != 0)
		return false;
	p += 6;
	*place = strtoll(p, &end, 10);
	if (end == p || strncmp(end, " rp=", 4) != 0)
		return false;
	p = end + 4;
	*rp = (int) strtol(p, &end, 10);
	if (end == p || *rp < 0 || *rp > 1)
		return false;
	return strncmp(end, " */ ", 4) == 0;
}

/*
 * The text of an M1 statement as the groups get it: the statement deparsed qualified under the
 * transmission modes, and decided once more on the parse of that text, as a group decides.  NULL
 * with *why when a group would not accept it.
 */
char *
kingres_shard_merge_text_colocated(Query *parse, const char **why)
{
	ParamTypes	pt = {NULL, 0};
	char	   *body;
	Query	   *rq;
	KsMerge		m2;
	int			nest;
	int			i;

	nest = kingres_shard_set_transmission_modes();
	body = pg_get_querydef(parse, false);
	kingres_shard_reset_transmission_modes(nest);

	(void) merge_param_types_walker((Node *) parse, &pt);
	for (i = 0; i < pt.n; i++)
		if (!OidIsValid(pt.types[i]))
			pt.types[i] = TEXTOID;
	rq = merge_parse_text(body, pt.types, pt.n, why);
	if (rq == NULL)
		return NULL;
	if (!kingres_shard_merge_fragment_analyze(rq, 0, &m2, why))
		return NULL;
	return body;
}

/* ======================================================================
 * M3: the rows of the source, by the group that owns their key
 * ====================================================================== */

typedef struct MgShare
{
	int			group_id;
	int64		nrows;
	StringInfoData *cols;
} MgShare;

struct KsMergeShares
{
	int			ncols;
	int			ngroups;
	MgShare    *g;
	int64		bytes;
	int64		rows;
	bool		broadcast;		/* one share, for every node (a reference table is the target) */
};

static void
mg_append_element(StringInfo buf, bool first, const char *text)
{
	const char *p;

	if (!first)
		appendStringInfoChar(buf, ',');
	if (text == NULL)
	{
		appendStringInfoString(buf, "NULL");
		return;
	}
	appendStringInfoChar(buf, '"');
	for (p = text; *p; p++)
	{
		if (*p == '"' || *p == '\\')
			appendStringInfoChar(buf, '\\');
		appendStringInfoChar(buf, *p);
	}
	appendStringInfoChar(buf, '"');
}

KsMergeShares *
kingres_shard_merge_collect(const char *source_sql, ParamListInfo params, int ncols, int colocation_id,
							List *group_ids, MemoryContext cxt)
{
	KsMergeShares *sh;
	MemoryContext old = MemoryContextSwitchTo(cxt);
	bool		bcast = (colocation_id == 0);
	const KsColocation *coloc = NULL;
	int			n = params ? params->numParams : 0;
	Oid		   *argtypes = palloc(sizeof(Oid) * Max(n, 1));
	Datum	   *values = palloc(sizeof(Datum) * Max(n, 1));
	char	   *nulls = palloc(Max(n, 1));
	int64		limit = (int64) kingres_shard_merge_max_source_kb * 1024;
	FmgrInfo   *outf = NULL;
	Oid			route_type = InvalidOid;
	Portal		portal;
	int			i;
	ListCell   *lc;
	int			nest;
	MemoryContext rowcxt;

	/* a copy: the cursor below can make this backend load the catalog again */
	if (!bcast && kingres_shard_lookup_colocation(colocation_id) != NULL)
		coloc = kingres_shard_coloc_copy(kingres_shard_lookup_colocation(colocation_id));
	if (coloc == NULL && !bcast)
		elog(ERROR, "colocation group %d of a MERGE is not in the catalog", colocation_id);
	sh = palloc0(sizeof(KsMergeShares));
	sh->ncols = ncols;
	sh->broadcast = bcast;
	if (bcast)
		group_ids = list_make1_int(-1);
	sh->ngroups = list_length(group_ids);
	sh->g = palloc0(sizeof(MgShare) * Max(sh->ngroups, 1));
	i = 0;
	foreach(lc, group_ids)
	{
		int			j;

		sh->g[i].group_id = lfirst_int(lc);
		sh->g[i].cols = palloc0(sizeof(StringInfoData) * ncols);
		for (j = 0; j < ncols; j++)
			initStringInfo(&sh->g[i].cols[j]);
		i++;
	}
	for (i = 0; i < n; i++)
	{
		ParamExternData *prm;
		ParamExternData workspace;

		if (params->paramFetch != NULL)
			prm = params->paramFetch(params, i + 1, false, &workspace);
		else
			prm = &params->params[i];
		argtypes[i] = OidIsValid(prm->ptype) ? prm->ptype : TEXTOID;
		values[i] = prm->value;
		nulls[i] = (prm->isnull || !OidIsValid(prm->ptype)) ? 'n' : ' ';
	}
	rowcxt = AllocSetContextCreate(cxt, "kingres_shard merge row", ALLOCSET_SMALL_SIZES);

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	portal = SPI_cursor_open_with_args(NULL, source_sql, n, argtypes, values, nulls, true, CURSOR_OPT_NO_SCROLL);
	if (portal == NULL)
		elog(ERROR, "could not open the cursor of the source of a MERGE");
	nest = kingres_shard_set_transmission_modes();
	PG_TRY();
	{
		for (;;)
		{
			uint64		k;

			SPI_cursor_fetch(portal, true, 256);
			if (SPI_processed == 0)
				break;
			if (outf == NULL)
			{
				TupleDesc	td = SPI_tuptable->tupdesc;
				int			c;

				if (td->natts != ncols + (bcast ? 0 : 1))
					elog(ERROR, "the source of a MERGE returned %d columns, expected %d", td->natts, ncols + (bcast ? 0 : 1));
				outf = MemoryContextAlloc(cxt, sizeof(FmgrInfo) * (ncols + 1));
				for (c = 0; c < td->natts; c++)
				{
					Oid			ofunc;
					bool		isvarlena;

					getTypeOutputInfo(TupleDescAttr(td, c)->atttypid, &ofunc, &isvarlena);
					fmgr_info_cxt(ofunc, &outf[c], cxt);
				}
				if (!bcast)
					route_type = TupleDescAttr(td, ncols)->atttypid;
			}
			for (k = 0; k < SPI_processed; k++)
			{
				HeapTuple	tup = SPI_tuptable->vals[k];
				TupleDesc	td = SPI_tuptable->tupdesc;
				bool		isnull = true;
				Datum		kd = (Datum) 0;
				int			bucket;
				int			g;
				MgShare    *share = NULL;
				int			j;

				CHECK_FOR_INTERRUPTS();
				if (bcast)
					share = &sh->g[0];
				else
				{
					kd = SPI_getbinval(tup, td, ncols + 1, &isnull);
				/* a NULL key hashes like the router's: tuple routing puts the inserted row into the bucket of slot 0 */
				if (!kingres_shard_try_bucket(coloc->dist_type, coloc->collation, coloc, route_type, kd, isnull, &bucket))
					ereport(ERROR,
							(errcode(ERRCODE_DATATYPE_MISMATCH),
							 errmsg("the key expression of a MERGE cannot be placed in the bucket map")));
				g = kingres_shard_bucket_group(coloc, bucket);
				if (g < 0)
					ereport(ERROR,
							(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
							 errmsg("bucket %d of colocation group %d has no owner", bucket, colocation_id)));
				for (j = 0; j < sh->ngroups; j++)
					if (sh->g[j].group_id == g)
					{
						share = &sh->g[j];
						break;
					}
				}
				if (share == NULL)
					continue;
				for (j = 0; j < ncols; j++)
				{
					bool		cnull;
					Datum		d = SPI_getbinval(tup, td, j + 1, &cnull);
					MemoryContext prev = MemoryContextSwitchTo(rowcxt);
					char	   *txt = cnull ? NULL : OutputFunctionCall(&outf[j], d);
					int			before = share->cols[j].len;

					MemoryContextSwitchTo(prev);
					mg_append_element(&share->cols[j], share->nrows == 0, txt);
					sh->bytes += share->cols[j].len - before;
					MemoryContextReset(rowcxt);
				}
				share->nrows++;
				sh->rows++;
				if (sh->bytes > limit)
					ereport(ERROR,
							(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
							 errmsg("the source of this MERGE needs more than kingres_shard.merge_max_source_bytes (%d kB)",
									kingres_shard_merge_max_source_kb),
							 errhint("Split the source by key range (one MERGE for each range), or raise the setting.")));
			}
			SPI_freetuptable(SPI_tuptable);
		}
	}
	PG_FINALLY();
	{
		kingres_shard_reset_transmission_modes(nest);
	}
	PG_END_TRY();
	SPI_cursor_close(portal);
	SPI_finish();
	MemoryContextDelete(rowcxt);
	MemoryContextSwitchTo(old);
	return sh;
}

static MgShare *
mg_find_share(KsMergeShares *sh, int group_id)
{
	int			i;

	if (sh->broadcast)
		return &sh->g[0];

	for (i = 0; i < sh->ngroups; i++)
		if (sh->g[i].group_id == group_id)
			return &sh->g[i];
	return NULL;
}

const char *
kingres_shard_merge_share_array(KsMergeShares *sh, int group_id, int col)
{
	MgShare    *g = mg_find_share(sh, group_id);

	if (g == NULL)
		return "{}";
	return psprintf("{%s}", g->cols[col].data);
}

int64
kingres_shard_merge_share_rows(KsMergeShares *sh, int group_id)
{
	MgShare    *g = mg_find_share(sh, group_id);

	return g == NULL ? 0 : g->nrows;
}

/* ======================================================================
 * M4 and M5: a reference table is the target (7.5)
 * ====================================================================== */

/*
 * The decision about a MERGE into a reference table, as every node makes it on the text it runs: the
 * target is a reference table without code of the users, every relation of the statement is a
 * reference table or the sub-select of a shipped source (at most one), every expression is built in
 * and immutable (the rule of a write that runs on every node: shippable.c), nothing is read that
 * another node does not hold in the same state.  *nshipped is the number of shipped sub-selects.
 */
bool
kingres_shard_merge_ref_fragment_analyze(Query *q, int *nshipped, const char **why)
{
	RangeTblEntry *trte;
	const KsDistTable *tdt;
	ListCell   *lc;
	Index		rti = 0;
	const char *reason = NULL;

	*nshipped = 0;
	*why = NULL;
	if (q->commandType != CMD_MERGE || q->resultRelation <= 0)
		MG_NO("it is not a MERGE");
	trte = rt_fetch(q->resultRelation, q->rtable);
	if (trte->rtekind != RTE_RELATION)
		MG_NO("its target is not a table");
	tdt = mg_lookup_stable(trte->relid);
	if (tdt == NULL || !tdt->is_reference)
		MG_NO("its target is not a reference table");
	if (q->cteList != NIL || q->hasRecursive || q->hasModifyingCTE)
		MG_NO("it has a WITH clause");
	if (!mg_target_guards(q, trte, why))
		return false;
	if (mg_target_has_user_code(trte->relid))
		MG_NO("the target has a trigger or a rule, which would run on every node");
	if (!mg_stable_default_insert(q, trte->relid, why))
		return false;
	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		rti++;
		if (rti == (Index) q->resultRelation)
			continue;
		if (rte->rtekind == RTE_SUBQUERY && !rte->lateral && !rte->inh &&
			kingres_shard_repart_shipped_shape(rte->subquery, NULL))
			(*nshipped)++;
	}
	if (*nshipped > 1)
		MG_NO("a MERGE has one shipped source");
	if (!kingres_shard_stmt_shippable_merge_ref(q, trte->relid, &reason))
		MG_NO(reason ? reason : "it cannot be run on every node");
	return true;
}

/* The text of an M4 statement as every node gets it, decided once more on its parse; NULL with *why when a node would not accept it. */
char *
kingres_shard_merge_text_reference(Query *parse, const char **why)
{
	ParamTypes	pt = {NULL, 0};
	char	   *body;
	Query	   *rq;
	int			nsh;
	int			nest;
	int			i;

	nest = kingres_shard_set_transmission_modes();
	body = pg_get_querydef(parse, false);
	kingres_shard_reset_transmission_modes(nest);

	(void) merge_param_types_walker((Node *) parse, &pt);
	for (i = 0; i < pt.n; i++)
		if (!OidIsValid(pt.types[i]))
			pt.types[i] = TEXTOID;
	rq = merge_parse_text(body, pt.types, pt.n, why);
	if (rq == NULL)
		return NULL;
	if (!kingres_shard_merge_ref_fragment_analyze(rq, &nsh, why) || nsh != 0)
	{
		if (*why == NULL)
			*why = "the text has a shipped source";
		return NULL;
	}
	return body;
}

/*
 * A MERGE into a reference table: M4 when the statement itself can run on every node (it reads
 * reference tables, VALUES and constants only); M5 when its source is one other relation (a
 * distributed table, a local table, a sub-select over them): this node reads the source once and
 * every node gets all of it.  The result has kind KS_MERGE_REFERENCE (M4) or
 * KS_MERGE_REFERENCE_SHIPPED (M5, with source_rti set and route NULL).  False when the target is
 * not a reference table; an error that names the condition when no shape runs.
 */
bool
kingres_shard_merge_classify_ref(Query *parse, KsMerge *m)
{
	RangeTblEntry *trte;
	const KsDistTable *tdt;
	const char *why4 = NULL;
	const char *why5 = NULL;
	int			nsh;
	Index		src;
	RangeTblEntry *srte;

	memset(m, 0, sizeof(*m));
	if (parse->commandType != CMD_MERGE || parse->resultRelation <= 0)
		return false;
	trte = rt_fetch(parse->resultRelation, parse->rtable);
	if (trte->rtekind != RTE_RELATION)
		return false;
	tdt = mg_lookup_stable(trte->relid);
	if (tdt == NULL || !tdt->is_reference)
		return false;
	m->target_relid = trte->relid;
	m->target_rti = parse->resultRelation;
	m->relids = merge_relids(parse);

	/*
	 * What the text cannot carry about the target (a view and its filter, row-level security, CHECK OPTION) is
	 * asked on the statement as written, before M4 and M5 are tried one after the other: a refusal of M4 must
	 * not let M5 build a text that has lost the filter of the view or the policy (review P12M-02).
	 */
	if (!mg_target_guards(parse, trte, &why4))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("this MERGE into reference table \"%s\" cannot be run on every node", get_rel_name(trte->relid)),
				 errdetail("%s.", why4)));

	/* M4: the statement as written runs on every node */
	if (kingres_shard_merge_ref_fragment_analyze(parse, &nsh, &why4) && nsh == 0)
	{
		m->kind = KS_MERGE_REFERENCE;
		return true;
	}

	/* M5: a source that is one relation */
	if (parse->cteList != NIL || parse->hasRecursive || parse->hasModifyingCTE)
		why5 = "it has a WITH clause";
	else if (parse->hasSubLinks)
		why5 = "it has a sub-select in an expression (ON, a WHEN condition, SET, VALUES or RETURNING)";
	else if (parse->jointree == NULL || list_length(parse->jointree->fromlist) != 1 ||
			 !IsA(linitial(parse->jointree->fromlist), RangeTblRef) || parse->jointree->quals != NULL)
		why5 = "USING is a join or several relations; put it in a sub-select so that the source is one relation";
	else
	{
		src = (Index) ((RangeTblRef *) linitial(parse->jointree->fromlist))->rtindex;
		srte = rt_fetch(src, parse->rtable);
		if (src == (Index) parse->resultRelation)
			why5 = "USING is the target";
		else if (srte->rtekind != RTE_RELATION && srte->rtekind != RTE_SUBQUERY &&
				 srte->rtekind != RTE_VALUES && srte->rtekind != RTE_FUNCTION)
			why5 = "USING is not a table, a sub-select, a VALUES list or a function";
		else if (srte->lateral)
			why5 = "USING is a LATERAL item";
		else
		{
			KsMergeShip sh;

			m->source_rti = src;
			m->route = NULL;
			if (kingres_shard_merge_build_ship(parse, m, &sh, &why5))
			{
				m->kind = KS_MERGE_REFERENCE_SHIPPED;
				return true;
			}
		}
	}
	/* the reason of the shape the statement is nearer to: M4 when its source is made of reference tables, else M5 */
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("this MERGE into reference table \"%s\" cannot be run on every node", get_rel_name(trte->relid)),
			 errdetail("%s.", (why4 != NULL && why5 != NULL && strstr(why4, "not a reference table") == NULL &&
							   strstr(why4, "reads a table") == NULL) ? why4 : (why5 != NULL ? why5 : why4)),
			 errhint("A MERGE into a reference table runs on every node, so it may use only constants, parameters and built-in immutable functions (not a sequence, random() or now()); its source is reference tables, VALUES, or one relation that this node reads and sends to every node.")));
	return false;				/* not reached */
}

/* ======================================================================
 * M6: a local target with a distributed source (7.6)
 * ====================================================================== */

typedef struct FenceVars
{
	Index		rti;
	int			sublevels;
	Bitmapset  *attrs;
	bool		whole;
} FenceVars;

static bool
fence_vars_walker(Node *node, FenceVars *fv)
{
	if (node == NULL)
		return false;
	if (IsA(node, Var))
	{
		Var		   *v = (Var *) node;

		if (v->varno == fv->rti && v->varlevelsup == (Index) fv->sublevels)
		{
			if (v->varattno == 0)
				fv->whole = true;
			else if (v->varattno > 0)
				fv->attrs = bms_add_member(fv->attrs, v->varattno);
		}
		return false;
	}
	if (IsA(node, Query))
	{
		bool		r;

		fv->sublevels++;
		r = query_tree_walker((Query *) node, fence_vars_walker, fv, 0);
		fv->sublevels--;
		return r;
	}
	return expression_tree_walker(node, fence_vars_walker, fv);
}

/*
 * Replace the relation of range table entry `rti` of `q`, a distributed table, by the sub-select
 * (SELECT <the columns the query uses; NULL for the others> FROM <table> OFFSET 0): the same
 * alias and column names, at the same range table index.  The OFFSET keeps PostgreSQL from
 * pulling the sub-select up, so the entry stays a sub-select and gets a copy row mark (7.6).
 */
static void
fence_one(Query *q, Index rti)
{
	RangeTblEntry *rte = rt_fetch(rti, q->rtable);
	FenceVars	fv;
	Relation	rel;
	int			natts;
	int			a;
	StringInfoData sql;
	List	   *raw;
	Query	   *sub;
	bool		first = true;
	char	   *relname;

	memset(&fv, 0, sizeof(fv));
	fv.rti = rti;
	(void) query_tree_walker(q, fence_vars_walker, &fv, 0);

	rel = table_open(rte->relid, AccessShareLock);
	natts = RelationGetNumberOfAttributes(rel);
	relname = quote_qualified_identifier(get_namespace_name(RelationGetNamespace(rel)), RelationGetRelationName(rel));
	initStringInfo(&sql);
	appendStringInfoString(&sql, "SELECT ");
	for (a = 1; a <= natts; a++)
	{
		Form_pg_attribute att = TupleDescAttr(rel->rd_att, a - 1);
		const char *name = NameStr(att->attname);

		if (!first)
			appendStringInfoString(&sql, ", ");
		first = false;
		if (att->attisdropped)
			appendStringInfo(&sql, "NULL::pg_catalog.text AS %s", quote_identifier(psprintf("ks_dropped%d", a)));
		else if (fv.whole || bms_is_member(a, fv.attrs))
			appendStringInfo(&sql, "%s", quote_identifier(name));
		else
			appendStringInfo(&sql, "NULL::%s AS %s", format_type_extended(att->atttypid, att->atttypmod,
																		  FORMAT_TYPE_FORCE_QUALIFY | FORMAT_TYPE_TYPEMOD_GIVEN),
							 quote_identifier(name));
	}
	if (natts == 0)
		appendStringInfoString(&sql, "1");
	appendStringInfo(&sql, " FROM %s OFFSET 0", relname);
	table_close(rel, NoLock);

	raw = pg_parse_query(sql.data);
	sub = parse_analyze_fixedparams(linitial_node(RawStmt, raw), sql.data, NULL, 0, NULL);
	if (rte->alias == NULL)
		rte->alias = makeAlias(rte->eref->aliasname, NIL);
	rte->rtekind = RTE_SUBQUERY;
	rte->subquery = sub;
	rte->relid = InvalidOid;
	rte->relkind = 0;
	rte->inh = false;
	rte->rellockmode = 0;
	rte->perminfoindex = 0;
	rte->tablesample = NULL;
	rte->securityQuals = NIL;
	rte->lateral = false;
	rte->security_barrier = false;
}

/* Does the query, at any depth, read a distributed table? */
static bool
reads_dist_walker(Node *node, void *context)
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
			{
				const KsDistTable *dt = kingres_shard_lookup_scan_table(kingres_shard_catalog(), rte->relid);

				if (dt != NULL && !dt->is_reference)
					return true;
			}
		}
		return query_tree_walker(q, reads_dist_walker, context, 0);
	}
	return expression_tree_walker(node, reads_dist_walker, context);
}

/*
 * Only the relations of the top level of the statement are asked for a row identifier (a sub-select is
 * planned as a query of its own): a distributed table there is fenced; a sub-select there that PostgreSQL
 * could pull up (and so turn its tables into relations of the top level) gets OFFSET 0 on itself, so that
 * what the user wrote inside it, its own restrictions, stays inside and reaches the scan.
 */
static int
fence_level(Query *q, Index skip)
{
	ListCell   *lc;
	Index		rti = 0;
	int			n = 0;

	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		rti++;
		if (rti == skip)
			continue;
		if (rte->rtekind == RTE_RELATION && rte->relkind != RELKIND_VIEW)
		{
			const KsDistTable *dt = kingres_shard_lookup_scan_table(kingres_shard_catalog(), rte->relid);

			if (dt != NULL && !dt->is_reference)
			{
				fence_one(q, rti);
				n++;
			}
		}
		else if (rte->rtekind == RTE_SUBQUERY && reads_dist_walker((Node *) rte->subquery, NULL))
		{
			if (rte->subquery->limitOffset == NULL && rte->subquery->limitCount == NULL)
				rte->subquery->limitOffset = (Node *) makeConst(INT8OID, -1, InvalidOid, sizeof(int64),
																Int64GetDatum(0), false, FLOAT8PASSBYVAL);
			n++;
		}
	}
	return n;
}

/*
 * M6: the target of the MERGE is a local table (it keeps its triggers, the capture trigger of a
 * ledger table among them) and USING reads distributed tables.  PostgreSQL asks every relation of
 * the statement other than the target for its row identifier unless the relation is not a table;
 * the distributed scan has none to give, so each distributed table of USING is read through a
 * sub-select with OFFSET 0 (above).  Returns the number of tables fenced; the query is changed.
 */
int
kingres_shard_merge_fence_sources(Query *parse)
{
	if (parse->commandType != CMD_MERGE || parse->resultRelation <= 0)
		return 0;
	return fence_level(parse, (Index) parse->resultRelation);
}
