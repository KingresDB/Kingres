/*-------------------------------------------------------------------------
 *
 * repartition.c
 *	  The repartitioned join, increment J1 (design/analytics-phase12-queries.md, section 4).
 *
 * What is here: the decision of the origin (which relation of the statement can be shipped, and
 * on which column), the text of the shipped relation (a sub-select over unnest() of array
 * parameters, 3.1), the check that a group makes of such a sub-select, and the collecting of the
 * rows of the shipped relation by the group that owns the bucket of their join key.  The paths
 * and the executor of the Fragment Scan (fragment_scan.c) call these functions.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/repartition.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "catalog/pg_attribute.h"
#include "catalog/pg_class.h"
#include "catalog/pg_collation_d.h"
#include "catalog/pg_type_d.h"
#include "executor/executor.h"
#include "lib/stringinfo.h"
#include "miscadmin.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "optimizer/optimizer.h"
#include "parser/analyze.h"
#include "parser/parse_node.h"
#include "parser/parsetree.h"
#include "rewrite/rewriteManip.h"
#include "tcop/tcopprot.h"
#include "utils/builtins.h"
#include "utils/fmgroids.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/syscache.h"

#include "catalog.h"
#include "hashing.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "repartition.h"

static Oid
rp_attcoll(Oid relid, AttrNumber att)
{
	HeapTuple	t = SearchSysCache2(ATTNUM, ObjectIdGetDatum(relid), Int16GetDatum(att));
	Oid			c = InvalidOid;

	if (HeapTupleIsValid(t))
	{
		c = ((Form_pg_attribute) GETSTRUCT(t))->attcollation;
		ReleaseSysCache(t);
	}
	return c;
}

int			kingres_shard_enable_repartition_join = KS_REPART_ON;
int			kingres_shard_repartition_max_kb = 65536;

static const struct config_enum_entry repart_options[] = {
	{"off", KS_REPART_OFF, false},
	{"on", KS_REPART_ON, false},
	{"force", KS_REPART_FORCE, false},
	{"false", KS_REPART_OFF, true},
	{"true", KS_REPART_ON, true},
	{NULL, 0, false}
};

void
kingres_shard_repartition_init(void)
{
	DefineCustomEnumVariable("kingres_shard.enable_repartition_join",
							 "Runs a join of distributed tables on columns other than their distribution column by moving the rows of one table to the groups.",
							 "off: never; on: when the planner finds it cheaper than reading the tables here; force: whenever the statement qualifies (for tests).",
							 &kingres_shard_enable_repartition_join,
							 KS_REPART_ON, repart_options,
							 PGC_USERSET, 0,
							 NULL, NULL, NULL);
	DefineCustomIntVariable("kingres_shard.repartition_max_bytes",
							"The most that this node holds of the rows of a table that a repartitioned join moves.",
							"A statement that would hold more fails (the planner does not choose the path when its estimate is above half of this).",
							&kingres_shard_repartition_max_kb,
							65536, 1024, MAX_KILOBYTES,
							PGC_USERSET, GUC_UNIT_KB,
							NULL, NULL, NULL);
}

/* ======================================================================
 * The shape of the sub-select that stands for a shipped relation
 * ====================================================================== */

static Node *
strip_wrappers(Node *n)
{
	while (n != NULL && (IsA(n, RelabelType) || IsA(n, CoerceViaIO)))
	{
		if (IsA(n, RelabelType))
			n = (Node *) ((RelabelType *) n)->arg;
		else
			n = (Node *) ((CoerceViaIO *) n)->arg;
	}
	return n;
}

bool
kingres_shard_repart_shipped_shape(Query *q, int *narrays)
{
	RangeTblEntry *rte;
	ListCell   *lc;
	int			n = 0;
	RangeTblRef *rtr;

	if (q == NULL || q->commandType != CMD_SELECT || q->cteList != NIL || q->setOperations != NULL ||
		q->hasSubLinks || q->hasAggs || q->hasWindowFuncs || q->hasTargetSRFs || q->hasRecursive ||
		q->groupClause != NIL || q->groupingSets != NIL || q->havingQual != NULL ||
		q->distinctClause != NIL || q->sortClause != NIL || q->limitCount != NULL ||
		q->limitOffset != NULL || q->rowMarks != NIL || q->windowClause != NIL || q->hasForUpdate)
		return false;
	if (list_length(q->rtable) != 1)
		return false;
	rte = (RangeTblEntry *) linitial(q->rtable);
	if (rte->rtekind != RTE_FUNCTION || rte->funcordinality || rte->lateral)
		return false;
	if (q->jointree == NULL || q->jointree->quals != NULL || list_length(q->jointree->fromlist) != 1)
		return false;
	if (!IsA(linitial(q->jointree->fromlist), RangeTblRef))
		return false;
	rtr = (RangeTblRef *) linitial(q->jointree->fromlist);
	if (rtr->rtindex != 1)
		return false;
	foreach(lc, rte->functions)
	{
		RangeTblFunction *rtf = (RangeTblFunction *) lfirst(lc);
		FuncExpr   *fe;
		Param	   *p;

		if (rtf->funcexpr == NULL || !IsA(rtf->funcexpr, FuncExpr) || rtf->funccolcount != 1)
			return false;
		fe = (FuncExpr *) rtf->funcexpr;
		if (fe->funcid != F_UNNEST_ANYARRAY || list_length(fe->args) != 1 || !IsA(linitial(fe->args), Param))
			return false;
		p = (Param *) linitial(fe->args);
		if (p->paramkind != PARAM_EXTERN || p->paramtype != TEXTARRAYOID)
			return false;
		n++;
	}
	if (n == 0)
		return false;
	foreach(lc, q->targetList)
	{
		TargetEntry *tle = lfirst_node(TargetEntry, lc);
		Node	   *e = (Node *) tle->expr;
		Node	   *inner;

		if (tle->resjunk)
			return false;
		if (exprType(e) >= FirstGenbkiObjectId)
			return false;
		inner = strip_wrappers(e);
		if (inner != NULL && IsA(inner, Const) && ((Const *) inner)->constisnull && inner == e)
			continue;
		if (inner == NULL || !IsA(inner, Var))
			return false;
		if (((Var *) inner)->varno != 1 || ((Var *) inner)->varlevelsup != 0 || ((Var *) inner)->vartype != TEXTOID)
			return false;
	}
	if (narrays != NULL)
		*narrays = n;
	return true;
}

/* ======================================================================
 * The decision of the origin
 * ====================================================================== */

typedef struct RpCtx
{
	Query	   *q;
	List	   *conj;			/* of Node *: the conjuncts of the WHERE and of the ON clauses of inner joins */
	Index		outer_rti;		/* J2: the one table that is the nullable side of a LEFT JOIN, 0 for none */
	List	   *outer_on;		/* the conjuncts of the ON clause of that join (they are also in conj) */
	const char *reason;
} RpCtx;

static void
rp_flatten_and(Node *qual, List **out)
{
	if (qual == NULL)
		return;
	if (IsA(qual, BoolExpr) && ((BoolExpr *) qual)->boolop == AND_EXPR)
	{
		ListCell   *lc;

		foreach(lc, ((BoolExpr *) qual)->args)
			rp_flatten_and((Node *) lfirst(lc), out);
		return;
	}
	*out = lappend(*out, qual);
}

/*
 * Does the condition name the columns of the table r and no other table?  (pull_varnos also returns the join entries that a
 * column of the nullable side of an outer join is nulled by, so it is not used here.)
 */
static bool
rp_names_only(Node *qual, Index r)
{
	List	   *vars = pull_var_clause(qual, PVC_RECURSE_AGGREGATES | PVC_RECURSE_WINDOWFUNCS | PVC_RECURSE_PLACEHOLDERS);
	ListCell   *lc;

	if (vars == NIL)
		return false;
	foreach(lc, vars)
	{
		Var		   *v = (Var *) lfirst(lc);

		if (!IsA(v, Var) || v->varlevelsup != 0 || v->varno != r)
			return false;
	}
	return true;
}

/* The conjuncts of the join tree: inner joins, and one LEFT JOIN to a table (J2). */
static bool
rp_collect(RpCtx *c, Node *n)
{
	if (n == NULL)
		return true;
	if (IsA(n, RangeTblRef))
		return true;
	if (IsA(n, FromExpr))
	{
		FromExpr   *f = (FromExpr *) n;
		ListCell   *lc;

		foreach(lc, f->fromlist)
			if (!rp_collect(c, (Node *) lfirst(lc)))
				return false;
		rp_flatten_and(f->quals, &c->conj);
		return true;
	}
	if (IsA(n, JoinExpr))
	{
		JoinExpr   *j = (JoinExpr *) n;

		if (j->jointype == JOIN_LEFT)
		{
			/*
			 * J2: one LEFT JOIN whose nullable side is one table, which can then be the moved one: every row of the
			 * preserved side stays in its own group and finds there every row of the moved table that can match it.
			 */
			if (c->outer_rti != 0 || !IsA(j->rarg, RangeTblRef))
			{
				c->reason = "it has an outer join other than one LEFT JOIN to a table";
				return false;
			}
			c->outer_rti = ((RangeTblRef *) j->rarg)->rtindex;
			if (!rp_collect(c, j->larg))
				return false;
			rp_flatten_and(j->quals, &c->outer_on);
			rp_flatten_and(j->quals, &c->conj);
			return true;
		}
		if (j->jointype != JOIN_INNER)
		{
			c->reason = "it has an outer join that a repartitioned join does not run (only a LEFT JOIN to one table)";
			return false;
		}
		if (!rp_collect(c, j->larg) || !rp_collect(c, j->rarg))
			return false;
		rp_flatten_and(j->quals, &c->conj);
		return true;
	}
	c->reason = "it has a join tree of a kind that is not analysed";
	return false;
}

typedef struct RpNodes
{
	int			n;
	int			cap;
	Index	   *rti;
	AttrNumber *att;
	int		   *uf;
} RpNodes;

static int
rp_node(RpNodes *nd, Index rti, AttrNumber att, bool create)
{
	int			i;

	for (i = 0; i < nd->n; i++)
		if (nd->rti[i] == rti && nd->att[i] == att)
			return i;
	if (!create)
		return -1;
	if (nd->n == nd->cap)
	{
		nd->cap *= 2;
		nd->rti = repalloc(nd->rti, sizeof(Index) * nd->cap);
		nd->att = repalloc(nd->att, sizeof(AttrNumber) * nd->cap);
		nd->uf = repalloc(nd->uf, sizeof(int) * nd->cap);
	}
	nd->rti[nd->n] = rti;
	nd->att[nd->n] = att;
	nd->uf[nd->n] = nd->n;
	return nd->n++;
}

static int
rp_find(RpNodes *nd, int x)
{
	while (nd->uf[x] != x)
	{
		nd->uf[x] = nd->uf[nd->uf[x]];
		x = nd->uf[x];
	}
	return x;
}

static bool
rp_int_family(Oid t)
{
	return t == INT2OID || t == INT4OID || t == INT8OID;
}

typedef struct RpRel
{
	Index		rti;
	Oid			relid;
	AttrNumber	dist_attno;
	int			colocation_id;
	Oid			dist_type;
	Oid			collation;
} RpRel;

static Node *
rp_strip_relabel(Node *n)
{
	while (n != NULL && IsA(n, RelabelType))
		n = (Node *) ((RelabelType *) n)->arg;
	return n;
}

KsPushdownInfo *
kingres_shard_repart_prepare(Query *parse, const char **why)
{
	RpCtx		c;
	RpNodes		nd;
	ListCell   *lc;
	Index		rti = 0;
	List	   *drels = NIL;	/* of RpRel * */
	List	   *relids = NIL;
	List	   *cands = NIL;
	KsPushdownInfo *pi;
	Query	   *ship;
	int			ndist = 0;
	int			ncands;

#define RP_NO(msg) do { *why = (msg); return NULL; } while (0)

	if (kingres_shard_enable_repartition_join == KS_REPART_OFF)
		RP_NO("repartitioned joins are switched off");
	if (IsInParallelMode())
		RP_NO("it is planned in a parallel worker");
	if (parse->commandType != CMD_SELECT)
		RP_NO("it is not a SELECT");
	if (parse->cteList != NIL || parse->hasRecursive || parse->hasModifyingCTE || parse->setOperations != NULL ||
		parse->hasSubLinks || parse->rowMarks != NIL || parse->groupingSets != NIL)
		RP_NO("it has a WITH clause, a set operation, a sub-select in an expression, locked rows or grouping sets");

	foreach(lc, parse->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);
		const KsDistTable *dt;

		rti++;
		switch (rte->rtekind)
		{
			case RTE_JOIN:
			case RTE_GROUP:
				continue;
			case RTE_RELATION:
				break;
			default:
				RP_NO("it reads something other than a table");
		}
		if (rte->tablesample != NULL || (rte->relkind != RELKIND_RELATION && rte->relkind != RELKIND_PARTITIONED_TABLE) ||
			rte->securityQuals != NIL)
			RP_NO("a table is sampled, is not an ordinary table, or has row-level security");
		dt = kingres_shard_lookup_dist_table(rte->relid);
		if (dt == NULL)
			RP_NO("it reads a local table, which the other shard groups do not have");
		relids = list_append_unique_oid(relids, rte->relid);
		if (!dt->is_reference)
		{
			RpRel	   *r = palloc0(sizeof(RpRel));

			r->rti = rti;
			r->relid = rte->relid;
			r->dist_attno = dt->dist_attno;
			r->colocation_id = dt->colocation_id;
			r->dist_type = dt->coloc->dist_type;
			r->collation = dt->coloc->collation;
			drels = lappend(drels, r);
			ndist++;
		}
	}
	if (ndist < 2)
		RP_NO("it reads fewer than two distributed tables");

	memset(&c, 0, sizeof(c));
	c.q = parse;
	if (!rp_collect(&c, (Node *) parse->jointree))
		RP_NO(c.reason);

	/*
	 * A condition of the WHERE that names only the nullable table of the LEFT JOIN cannot be given to its scan (it applies after
	 * the join, to the rows that the join filled with NULLs).
	 */
	if (c.outer_rti != 0)
	{
		foreach(lc, c.conj)
		{
			if (!list_member_ptr(c.outer_on, lfirst(lc)) && rp_names_only((Node *) lfirst(lc), c.outer_rti))
				RP_NO("a condition of the WHERE names only the table on the nullable side of the LEFT JOIN");
		}
	}

	/* the equalities between columns, as classes of (relation, column) */
	nd.n = 0;
	nd.cap = 16;
	nd.rti = palloc(sizeof(Index) * nd.cap);
	nd.att = palloc(sizeof(AttrNumber) * nd.cap);
	nd.uf = palloc(sizeof(int) * nd.cap);
	foreach(lc, c.conj)
	{
		OpExpr	   *op;
		Node	   *l;
		Node	   *r;
		Var		   *vl;
		Var		   *vr;
		Oid			tl;
		Oid			tr;

		if (!IsA(lfirst(lc), OpExpr))
			continue;
		op = (OpExpr *) lfirst(lc);
		if (list_length(op->args) != 2)
			continue;
		l = rp_strip_relabel(flatten_join_alias_vars(NULL, parse, (Node *) linitial(op->args)));
		r = rp_strip_relabel(flatten_join_alias_vars(NULL, parse, (Node *) lsecond(op->args)));
		if (l == NULL || r == NULL || !IsA(l, Var) || !IsA(r, Var))
			continue;
		vl = (Var *) l;
		vr = (Var *) r;
		if (vl->varlevelsup != 0 || vr->varlevelsup != 0 || vl->varattno <= 0 || vr->varattno <= 0 ||
			vl->varno == vr->varno)
			continue;
		tl = vl->vartype;
		tr = vr->vartype;
		if (!op_hashjoinable(op->opno, tl) || !(tl == tr || (rp_int_family(tl) && rp_int_family(tr))))
			continue;
		if (OidIsValid(op->inputcollid) && !get_collation_isdeterministic(op->inputcollid))
			continue;
		{
			int			a = rp_node(&nd, vl->varno, vl->varattno, true);
			int			b = rp_node(&nd, vr->varno, vr->varattno, true);

			a = rp_find(&nd, a);
			b = rp_find(&nd, b);
			if (a != b)
				nd.uf[a] = b;
		}
	}

	/* one candidate for each distributed table that can be the shipped one */
	foreach(lc, drels)
	{
		RpRel	   *rel = (RpRel *) lfirst(lc);
		ListCell   *lo;
		int			anchor_class = -1;
		int			anchor_coloc = 0;
		bool		ok = true;
		bool		first = true;
		int			own;
		AttrNumber	key = InvalidAttrNumber;
		int			i;
		KsRepartCand *cand;
		const KsColocation *coloc;
		List	   *dist = NIL;
		int			nanch = 0;

		/* with a LEFT JOIN, only the table on its nullable side can be moved */
		if (c.outer_rti != 0 && rel->rti != c.outer_rti)
			continue;
		foreach(lo, drels)
		{
			RpRel	   *o = (RpRel *) lfirst(lo);
			int			k;
			int			cls;

			if (o == rel)
				continue;
			k = rp_node(&nd, o->rti, o->dist_attno, false);
			cls = (k >= 0) ? rp_find(&nd, k) : -2 - (int) o->rti;
			if (first)
			{
				anchor_class = cls;
				anchor_coloc = o->colocation_id;
				first = false;
			}
			else if (cls != anchor_class || o->colocation_id != anchor_coloc)
			{
				ok = false;
				break;
			}
			dist = list_append_unique_oid(dist, o->relid);
			nanch++;
		}
		if (!ok || first || anchor_class < 0)
			continue;
		own = rp_node(&nd, rel->rti, rel->dist_attno, false);
		if (own >= 0 && rp_find(&nd, own) == anchor_class)
			continue;			/* it is anchored itself */
		/* the column of the shipped relation that is equated with the key of the anchored tables */
		for (i = 0; i < nd.n; i++)
			if (nd.rti[i] == rel->rti && rp_find(&nd, i) == anchor_class &&
				(key == InvalidAttrNumber || nd.att[i] < key))
				key = nd.att[i];
		if (key == InvalidAttrNumber)
			continue;
		coloc = kingres_shard_lookup_colocation(anchor_coloc);
		if (coloc == NULL)
			continue;
		{
			Oid			kt = get_atttype(rel->relid, key);

			if (!(kt == coloc->dist_type || (rp_int_family(kt) && rp_int_family(coloc->dist_type))))
				continue;
			if (rp_attcoll(rel->relid, key) != coloc->collation)
				continue;
		}
		cand = palloc0(sizeof(KsRepartCand));
		cand->routed_rti = rel->rti;
		cand->routed_relid = rel->relid;
		cand->key_attno = key;
		cand->key_type = get_atttype(rel->relid, key);
		cand->colocation_id = anchor_coloc;
		cand->place = kingres_shard_placement_hash(coloc);
		cand->dist_relids = dist;
		cand->nanchored = 0;
		foreach(lo, drels)
			if (lfirst(lo) != rel)
				cand->nanchored++;
		cands = lappend(cands, cand);
	}
	ncands = list_length(cands);
	if (ncands == 0)
		RP_NO("the distributed tables are not joined so that one repartition aligns them (the other tables must share one key class and the shipped table must equate a column with it)");

	ship = copyObject(parse);
	ship->sortClause = NIL;
	ship->limitOffset = NULL;
	ship->limitCount = NULL;
	ship->limitOption = LIMIT_OPTION_COUNT;

	pi = palloc0(sizeof(KsPushdownInfo));
	pi->parse = parse;
	pi->sql = NULL;
	pi->param_ids = NIL;
	pi->colocation_id = ((KsRepartCand *) linitial(cands))->colocation_id;
	pi->place = ((KsRepartCand *) linitial(cands))->place;
	pi->relids = relids;
	pi->dist_relids = ((KsRepartCand *) linitial(cands))->dist_relids;
	pi->kind = "repartitioned join";
	pi->ncols = 0;
	pi->ndist = ndist - 1;
	pi->ext = NULL;
	pi->join = true;
	pi->closed = false;
	pi->partial = false;
	pi->ship = ship;
	pi->repart = cands;
	return pi;
#undef RP_NO
}

/* ======================================================================
 * The text of the shipped relation
 * ====================================================================== */

/* Remove, from the conjuncts of a qualification, the ones that name the shipped relation only: its own scan applies them. */
static Node *
rp_strip_only(Node *qual, Index r)
{
	List	   *conj = NIL;
	List	   *keep = NIL;
	ListCell   *lc;

	rp_flatten_and(qual, &conj);
	foreach(lc, conj)
	{
		if (rp_names_only((Node *) lfirst(lc), r))
			continue;
		keep = lappend(keep, lfirst(lc));
	}
	return (Node *) make_ands_explicit(keep);
}

static void
rp_strip_tree(Node *n, Index r)
{
	if (n == NULL)
		return;
	if (IsA(n, FromExpr))
	{
		FromExpr   *f = (FromExpr *) n;
		ListCell   *lc;

		foreach(lc, f->fromlist)
			rp_strip_tree((Node *) lfirst(lc), r);
		f->quals = rp_strip_only(f->quals, r);
	}
	else if (IsA(n, JoinExpr))
	{
		JoinExpr   *j = (JoinExpr *) n;

		rp_strip_tree(j->larg, r);
		rp_strip_tree(j->rarg, r);
		j->quals = rp_strip_only(j->quals, r);
	}
}

bool
kingres_shard_repart_ship_text(Query *jq, const KsRepartCand *cand, int first_param, PathTarget *child_target,
							   List **child_cols, int *keypos, const char **why)
{
	RangeTblEntry *rte = rt_fetch(cand->routed_rti, jq->rtable);
	Bitmapset  *attrs = NULL;
	int			natts = list_length(rte->eref->colnames);
	int			a;
	int			k = 0;
	int			i;
	StringInfoData sel;
	StringInfoData from;
	List	   *raw;
	Oid		   *ptypes;
	int			nparams;
	Query	   *sub;
	List	   *cols = NIL;
	int			kp = -1;

#define SH_NO(msg) do { *why = (msg); return false; } while (0)

	rp_strip_tree((Node *) jq->jointree, cand->routed_rti);
	pull_varattnos((Node *) jq->targetList, cand->routed_rti, &attrs);
	pull_varattnos((Node *) jq->jointree, cand->routed_rti, &attrs);
	attrs = bms_add_member(attrs, cand->key_attno - FirstLowInvalidHeapAttributeNumber);

	initStringInfo(&sel);
	initStringInfo(&from);
	appendStringInfoString(&from, "ROWS FROM (");
	for (a = 1; a <= natts; a++)
	{
		const char *name = strVal(list_nth(rte->eref->colnames, a - 1));

		if (a > 1)
			appendStringInfoString(&sel, ", ");
		if (bms_is_member(a - FirstLowInvalidHeapAttributeNumber, attrs))
		{
			Oid			t = get_atttype(cand->routed_relid, (AttrNumber) a);
			Oid			coll = rp_attcoll(cand->routed_relid, (AttrNumber) a);
			int			pos = -1;
			ListCell   *lc;

			if (t >= FirstGenbkiObjectId)
				SH_NO("a column of the shipped table has a type that is not built in");
			if (OidIsValid(coll) && coll != DEFAULT_COLLATION_OID)
				SH_NO("a column of the shipped table has a collation other than the default");
			i = 0;
			foreach(lc, child_target->exprs)
			{
				Node	   *e = (Node *) lfirst(lc);

				if (IsA(e, Var) && ((Var *) e)->varno == cand->routed_rti && ((Var *) e)->varattno == a)
				{
					pos = i;
					break;
				}
				i++;
			}
			if (pos < 0)
				SH_NO("a column of the shipped table that the statement needs is not read by its scan");
			cols = lappend_int(cols, pos);
			if (a == cand->key_attno)
				kp = k;
			if (t == TEXTOID)
				appendStringInfo(&sel, "u.c%d AS %s", k + 1, quote_identifier(name));
			else
				appendStringInfo(&sel, "u.c%d::%s AS %s", k + 1, format_type_extended(t, -1, FORMAT_TYPE_FORCE_QUALIFY), quote_identifier(name));
			if (k > 0)
				appendStringInfoString(&from, ", ");
			appendStringInfo(&from, "pg_catalog.unnest($%d::pg_catalog.text[])", first_param + k);
			k++;
		}
		else
			appendStringInfo(&sel, "NULL::pg_catalog.text AS %s", quote_identifier(name));
	}
	appendStringInfoString(&from, ") AS u(");
	for (i = 1; i <= k; i++)
		appendStringInfo(&from, "%sc%d", i > 1 ? ", " : "", i);
	appendStringInfoChar(&from, ')');
	if (k == 0 || kp < 0)
		SH_NO("the shipped table has no column to ship");

	{
		StringInfoData sql;
		List	   *rawlist;

		initStringInfo(&sql);
		appendStringInfo(&sql, "SELECT %s FROM %s", sel.data, from.data);
		nparams = first_param + k - 1;
		ptypes = palloc0(sizeof(Oid) * Max(nparams, 1));
		for (i = first_param - 1; i < nparams; i++)
			ptypes[i] = TEXTARRAYOID;
		rawlist = pg_parse_query(sql.data);
		raw = rawlist;
		if (list_length(raw) != 1)
			SH_NO("the sub-select of the shipped table did not parse as one statement");
		sub = parse_analyze_fixedparams(linitial_node(RawStmt, raw), sql.data, ptypes, nparams, NULL);
	}
	if (!kingres_shard_repart_shipped_shape(sub, NULL))
		SH_NO("the sub-select of the shipped table does not have the shape the groups accept");

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

	*child_cols = cols;
	*keypos = kp;
	return true;
#undef SH_NO
}

/* ======================================================================
 * The rows of the shipped relation, by group
 * ====================================================================== */

typedef struct RpGroup
{
	int			group_id;
	int64		nrows;
	StringInfoData *cols;
} RpGroup;

struct KsRepartRows
{
	int			ncols;
	int			ngroups;
	RpGroup    *groups;
	int64		bytes;
	int64		rows;
};

static void
rp_append_element(StringInfo buf, bool first, const char *text)
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

KsRepartRows *
kingres_shard_repart_collect(PlanState *child, const KsColocation *coloc, List *cols, int keypos, List *groups,
							 MemoryContext cxt)
{
	KsRepartRows *rows;
	MemoryContext old = MemoryContextSwitchTo(cxt);
	MemoryContext rowcxt;
	TupleDesc	desc = ExecGetResultType(child);
	int			ncols = list_length(cols);
	int			i;
	ListCell   *lc;
	FmgrInfo   *outf;
	int			keycol = list_nth_int(cols, keypos);
	Oid			keytype = TupleDescAttr(desc, keycol)->atttypid;
	int64		limit = (int64) kingres_shard_repartition_max_kb * 1024;
	int			nest;

	rows = palloc0(sizeof(KsRepartRows));
	rows->ncols = ncols;
	rows->ngroups = list_length(groups);
	rows->groups = palloc0(sizeof(RpGroup) * Max(rows->ngroups, 1));
	i = 0;
	foreach(lc, groups)
	{
		int			j;

		rows->groups[i].group_id = lfirst_int(lc);
		rows->groups[i].cols = palloc0(sizeof(StringInfoData) * ncols);
		for (j = 0; j < ncols; j++)
			initStringInfo(&rows->groups[i].cols[j]);
		i++;
	}
	outf = palloc0(sizeof(FmgrInfo) * ncols);
	i = 0;
	foreach(lc, cols)
	{
		Oid			ofunc;
		bool		isvarlena;

		getTypeOutputInfo(TupleDescAttr(desc, lfirst_int(lc))->atttypid, &ofunc, &isvarlena);
		fmgr_info(ofunc, &outf[i++]);
	}
	rowcxt = AllocSetContextCreate(cxt, "kingres_shard repartition row", ALLOCSET_SMALL_SIZES);
	nest = kingres_shard_set_transmission_modes();
	PG_TRY();
	{
		for (;;)
		{
			TupleTableSlot *slot = ExecProcNode(child);
			Datum		kd;
			bool		kn;
			int			bucket;
			int			g;
			RpGroup    *rg = NULL;
			int			j;

			CHECK_FOR_INTERRUPTS();
			if (TupIsNull(slot))
				break;
			kd = slot_getattr(slot, keycol + 1, &kn);
			if (kn)
				continue;		/* a NULL key joins nothing */
			if (!kingres_shard_try_bucket(coloc->dist_type, coloc->collation, coloc, keytype, kd, false, &bucket))
				ereport(ERROR,
						(errcode(ERRCODE_DATATYPE_MISMATCH),
						 errmsg("the join key of a repartitioned join cannot be placed in the bucket map")));
			g = kingres_shard_bucket_group(coloc, bucket);
			if (g < 0)
				ereport(ERROR,
						(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
						 errmsg("bucket %d of colocation group %d has no owner", bucket, coloc->colocation_id)));
			for (j = 0; j < rows->ngroups; j++)
				if (rows->groups[j].group_id == g)
				{
					rg = &rows->groups[j];
					break;
				}
			if (rg == NULL)
				continue;		/* a group the statement does not ask (pruned): its rows are not needed */
			MemoryContextSwitchTo(rowcxt);
			for (j = 0; j < ncols; j++)
			{
				bool		isnull;
				Datum		d = slot_getattr(slot, list_nth_int(cols, j) + 1, &isnull);
				char	   *txt = isnull ? NULL : OutputFunctionCall(&outf[j], d);
				int			before = rg->cols[j].len;

				MemoryContextSwitchTo(cxt);
				rp_append_element(&rg->cols[j], rg->nrows == 0, txt);
				rows->bytes += rg->cols[j].len - before;
				MemoryContextSwitchTo(rowcxt);
			}
			MemoryContextSwitchTo(cxt);
			MemoryContextReset(rowcxt);
			rg->nrows++;
			rows->rows++;
			if (rows->bytes > limit)
				ereport(ERROR,
						(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
						 errmsg("the rows that a repartitioned join moves need more than kingres_shard.repartition_max_bytes (%d kB)",
								kingres_shard_repartition_max_kb),
						 errhint("Raise the setting, or set kingres_shard.enable_repartition_join to off to read the tables here.")));
		}
	}
	PG_FINALLY();
	{
		kingres_shard_reset_transmission_modes(nest);
		MemoryContextSwitchTo(old);
	}
	PG_END_TRY();
	MemoryContextDelete(rowcxt);
	return rows;
}

static RpGroup *
rp_find_group(KsRepartRows *rows, int group_id)
{
	int			i;

	for (i = 0; i < rows->ngroups; i++)
		if (rows->groups[i].group_id == group_id)
			return &rows->groups[i];
	return NULL;
}

const char *
kingres_shard_repart_array(KsRepartRows *rows, int group_id, int col)
{
	RpGroup    *g = rp_find_group(rows, group_id);

	if (g == NULL)
		return "{}";
	return psprintf("{%s}", g->cols[col].data);
}

int64
kingres_shard_repart_row_count(KsRepartRows *rows, int group_id)
{
	RpGroup    *g = rp_find_group(rows, group_id);

	return g == NULL ? 0 : g->nrows;
}

int64
kingres_shard_repart_total_rows(KsRepartRows *rows)
{
	return rows->rows;
}

int64
kingres_shard_repart_total_bytes(KsRepartRows *rows)
{
	return rows->bytes;
}

/*
 * kingres_shard_repart_attach: the hook of planner.c.  A statement of 2b with a closed or partial plan is left alone; a join
 * that 2b cannot run (or runs only on key) gets the candidates for a repartition beside it.
 */
KsPushdownInfo *
kingres_shard_repart_attach(Query *parse, KsPushdownInfo *pi)
{
	KsPushdownInfo *rp;
	const char *why = NULL;

	if (kingres_shard_enable_repartition_join == KS_REPART_OFF || !kingres_shard_enable_pushdown)
		return pi;
	if (pi != NULL && (!pi->join || pi->ship == NULL))
		return pi;
	rp = kingres_shard_repart_prepare(parse, &why);
	if (rp == NULL)
	{
		if (why != NULL)
			ereport(DEBUG1, (errmsg_internal("kingres_shard: no repartitioned join: %s", why)));
		return pi;
	}
	if (pi == NULL)
		return rp;
	pi->repart = rp->repart;
	return pi;
}
