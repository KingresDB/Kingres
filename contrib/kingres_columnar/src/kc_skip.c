/*-------------------------------------------------------------------------
 *
 * kc_skip.c
 *	  Skipping of stripes and chunk groups with the minimum, maximum and null
 *	  count stored for each column.
 *
 * The restriction clauses of the scan are examined once, when the scan is
 * built.  A clause of the shape  column OP expression  with OP in the btree
 * operator family of the column's default operator class, column IS [NOT]
 * NULL, column = ANY (array expression), and ANDs and ORs of these, becomes a
 * node of a small tree.  The expressions are evaluated once when the scan
 * starts (and again when it is rescanned): constants, external parameters
 * and stable functions are allowed, volatile functions and other columns are
 * not.  A node answers "can no row of this stripe or chunk group satisfy
 * me?" from the statistics.  Skipping is only an optimisation: the clauses
 * stay in the plan as filters, so a clause that is not understood, or a
 * statistic that is missing, costs speed and never an answer.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kc_skip.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/nbtree.h"
#include "access/stratnum.h"
#include "catalog/pg_am.h"
#include "executor/executor.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "optimizer/clauses.h"
#include "optimizer/optimizer.h"
#include "parser/parse_coerce.h"
#include "utils/array.h"
#include "utils/datum.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/rel.h"

#include "kingres_columnar.h"

typedef enum KcSkipKind
{
	KS_AND,
	KS_OR,
	KS_CMP,
	KS_ISNULL,
	KS_NOTNULL,
	KS_ANYEQ
} KcSkipKind;

typedef struct KcSkipNode
{
	KcSkipKind	kind;
	List	   *kids;			/* of KcSkipNode *, for AND and OR */
	int			attidx;			/* 0-based column */
	int			strategy;		/* btree strategy, var on the left */
	FmgrInfo	cmp;			/* cmp(column value, expression value) */
	Oid			collation;
	Expr	   *expr;
	ExprState  *estate;
	Oid			exprtype;
	Datum		val;
	bool		isnull;
	bool		ready;
	Datum	   *elems;
	bool	   *elnulls;
	int			nelems;
} KcSkipNode;

typedef struct KcSkip
{
	KcSkipNode *root;			/* AND of everything usable, or NULL */
	const KcTypeInfo *ti;
	TupleDesc	desc;
	PlanState  *ps;
	MemoryContext cxt;			/* values of the expressions */
	MemoryContext tmp;			/* scratch for one test */
	bool		evaluated;
	List	   *attnames;
} KcSkip;

/* ------------------------------------------------------------------------
 * Building
 * ------------------------------------------------------------------------
 */

static Node *
kc_strip_relabel(Node *n)
{
	while (n && IsA(n, RelabelType))
		n = (Node *) ((RelabelType *) n)->arg;
	return n;
}

/* Is e an expression that can be evaluated once, without any column? */
static bool
kc_const_side(Node *e)
{
	return !contain_var_clause(e) && !contain_volatile_functions(e) &&
		!contain_subplans(e);
}

static int
kc_commute_strategy(int s)
{
	switch (s)
	{
		case BTLessStrategyNumber:
			return BTGreaterStrategyNumber;
		case BTLessEqualStrategyNumber:
			return BTGreaterEqualStrategyNumber;
		case BTGreaterEqualStrategyNumber:
			return BTLessEqualStrategyNumber;
		case BTGreaterStrategyNumber:
			return BTLessStrategyNumber;
	}
	return s;
}

static KcSkipNode *
kc_new_node(KcSkip *sk, KcSkipKind kind)
{
	KcSkipNode *n = MemoryContextAllocZero(sk->cxt, sizeof(KcSkipNode));

	n->kind = kind;
	return n;
}

/* A column of this relation, with a usable type, from an expression. */
static int
kc_column_of(KcSkip *sk, Index relid, Node *e)
{
	Var		   *v;

	e = kc_strip_relabel(e);
	if (e == NULL || !IsA(e, Var))
		return -1;
	v = (Var *) e;
	if (v->varno != relid || v->varlevelsup != 0 || v->varattno <= 0 ||
		v->varattno > sk->ti->natts)
		return -1;
	if (sk->ti->cols[v->varattno - 1].dropped)
		return -1;
	return v->varattno - 1;
}

/* Look up the comparison of the column with a value of the expression's type. */
static bool
kc_find_cmp(KcSkip *sk, KcSkipNode *n, Oid opno, int strategy_in, Oid exprtype,
			bool commuted)
{
	const KcColInfo *ci = &sk->ti->cols[n->attidx];
	List	   *interps;
	ListCell   *lc;

	(void) strategy_in;
	if (!ci->minmax_ok || !OidIsValid(ci->opfamily))
		return false;
	interps = get_op_index_interpretation(opno);
	foreach(lc, interps)
	{
		OpIndexInterpretation *oi = (OpIndexInterpretation *) lfirst(lc);
		Oid			ltype = commuted ? oi->oprighttype : oi->oplefttype;
		Oid			rtype = commuted ? oi->oplefttype : oi->oprighttype;
		Oid			proc;

		if (oi->opfamily_id != ci->opfamily)
			continue;
		(void) exprtype;
		proc = get_opfamily_proc(oi->opfamily_id, ltype, rtype, BTORDER_PROC);
		if (!OidIsValid(proc))
			continue;
		fmgr_info_cxt(proc, &n->cmp, sk->cxt);
		n->strategy = commuted ? kc_commute_strategy((int) oi->cmptype) : (int) oi->cmptype;
		return true;
	}
	return false;
}

static KcSkipNode *
kc_build_node(KcSkip *sk, Index relid, Node *clause)
{
	if (clause == NULL)
		return NULL;

	switch (nodeTag(clause))
	{
		case T_OpExpr:
			{
				OpExpr	   *op = (OpExpr *) clause;
				Node	   *l,
						   *r;
				int			att;
				KcSkipNode *n;
				bool		commuted = false;
				Oid			opno = op->opno;
				Node	   *other;

				if (list_length(op->args) != 2)
					return NULL;
				l = (Node *) linitial(op->args);
				r = (Node *) lsecond(op->args);
				att = kc_column_of(sk, relid, l);
				other = r;
				if (att < 0)
				{
					att = kc_column_of(sk, relid, r);
					other = l;
					commuted = true;
				}
				if (att < 0 || !kc_const_side(other))
					return NULL;
				if (op->inputcollid != sk->ti->cols[att].collation)
					return NULL;
				n = kc_new_node(sk, KS_CMP);
				n->attidx = att;
				n->collation = op->inputcollid;
				n->exprtype = exprType(other);
				if (!kc_find_cmp(sk, n, opno, 0, n->exprtype, commuted))
					return NULL;
				if (n->strategy == BTEqualStrategyNumber ||
					n->strategy == BTLessStrategyNumber ||
					n->strategy == BTLessEqualStrategyNumber ||
					n->strategy == BTGreaterStrategyNumber ||
					n->strategy == BTGreaterEqualStrategyNumber)
					;
				else
					return NULL;
				n->expr = (Expr *) other;
				return n;
			}
		case T_ScalarArrayOpExpr:
			{
				ScalarArrayOpExpr *sa = (ScalarArrayOpExpr *) clause;
				Node	   *l,
						   *r;
				int			att;
				KcSkipNode *n;
				Oid			elemtype;

				if (!sa->useOr || list_length(sa->args) != 2)
					return NULL;
				l = (Node *) linitial(sa->args);
				r = (Node *) lsecond(sa->args);
				att = kc_column_of(sk, relid, l);
				if (att < 0 || !kc_const_side(r))
					return NULL;
				if (sa->inputcollid != sk->ti->cols[att].collation)
					return NULL;
				elemtype = get_element_type(exprType(r));
				if (!OidIsValid(elemtype))
					return NULL;
				n = kc_new_node(sk, KS_ANYEQ);
				n->attidx = att;
				n->collation = sa->inputcollid;
				n->exprtype = exprType(r);
				if (!kc_find_cmp(sk, n, sa->opno, 0, elemtype, false) ||
					n->strategy != BTEqualStrategyNumber)
					return NULL;
				n->expr = (Expr *) r;
				return n;
			}
		case T_NullTest:
			{
				NullTest   *nt = (NullTest *) clause;
				int			att;
				KcSkipNode *n;

				if (nt->argisrow)
					return NULL;
				att = kc_column_of(sk, relid, (Node *) nt->arg);
				if (att < 0)
					return NULL;
				n = kc_new_node(sk, nt->nulltesttype == IS_NULL ? KS_ISNULL : KS_NOTNULL);
				n->attidx = att;
				return n;
			}
		case T_BoolExpr:
			{
				BoolExpr   *be = (BoolExpr *) clause;
				List	   *kids = NIL;
				ListCell   *lc;
				KcSkipNode *n;

				if (be->boolop == NOT_EXPR)
					return NULL;
				foreach(lc, be->args)
				{
					KcSkipNode *k = kc_build_node(sk, relid, (Node *) lfirst(lc));

					if (k != NULL)
						kids = lappend(kids, k);
					else if (be->boolop == OR_EXPR)
						return NULL;	/* an OR skips only if every arm does */
				}
				if (kids == NIL)
					return NULL;
				n = kc_new_node(sk, be->boolop == AND_EXPR ? KS_AND : KS_OR);
				n->kids = kids;
				return n;
			}
		default:
			return NULL;
	}
}

KcSkip *
kc_skip_build(List *quals, Index relid, TupleDesc desc, const KcTypeInfo *ti,
			  PlanState *ps)
{
	KcSkip	   *sk;
	MemoryContext cxt = AllocSetContextCreate(CurrentMemoryContext,
											  "kingres_columnar skip",
											  ALLOCSET_SMALL_SIZES);
	List	   *kids = NIL;
	ListCell   *lc;

	sk = MemoryContextAllocZero(cxt, sizeof(KcSkip));
	sk->cxt = cxt;
	sk->tmp = AllocSetContextCreate(cxt, "kingres_columnar skip test",
									ALLOCSET_SMALL_SIZES);
	sk->ti = ti;
	sk->desc = desc;
	sk->ps = ps;

	foreach(lc, quals)
	{
		KcSkipNode *n = kc_build_node(sk, relid, (Node *) lfirst(lc));

		if (n != NULL)
			kids = lappend(kids, n);
	}
	if (kids != NIL)
	{
		sk->root = kc_new_node(sk, KS_AND);
		sk->root->kids = kids;
	}
	return sk;
}

static void
kc_collect_attnames(KcSkip *sk, KcSkipNode *n, List **names)
{
	ListCell   *lc;

	if (n->kind == KS_AND || n->kind == KS_OR)
	{
		foreach(lc, n->kids)
			kc_collect_attnames(sk, lfirst(lc), names);
		return;
	}
	{
		char	   *nm = NameStr(TupleDescAttr(sk->desc, n->attidx)->attname);
		ListCell   *l2;
		bool		have = false;

		foreach(l2, *names)
			if (strcmp(lfirst(l2), nm) == 0)
				have = true;
		if (!have)
			*names = lappend(*names, nm);
	}
}

/* The columns the skip conditions are on, for EXPLAIN. */
char *
kc_skip_describe(KcSkip *sk)
{
	StringInfoData buf;
	List	   *names = NIL;
	ListCell   *lc;
	bool		first = true;

	if (sk == NULL || sk->root == NULL)
		return NULL;
	kc_collect_attnames(sk, sk->root, &names);
	initStringInfo(&buf);
	foreach(lc, names)
	{
		appendStringInfo(&buf, "%s%s", first ? "" : ", ", (char *) lfirst(lc));
		first = false;
	}
	return buf.data;
}

bool
kc_skip_active(KcSkip *sk)
{
	return sk != NULL && sk->root != NULL && sk->evaluated;
}

/* ------------------------------------------------------------------------
 * Evaluating the expressions
 * ------------------------------------------------------------------------
 */

static void
kc_eval_node(KcSkip *sk, KcSkipNode *n, ExprContext *econtext)
{
	ListCell   *lc;

	if (n->kind == KS_AND || n->kind == KS_OR)
	{
		foreach(lc, n->kids)
			kc_eval_node(sk, lfirst(lc), econtext);
		return;
	}
	if (n->kind != KS_CMP && n->kind != KS_ANYEQ)
		return;

	if (n->estate == NULL)
	{
		MemoryContext old = MemoryContextSwitchTo(sk->cxt);

		n->estate = ExecInitExpr(n->expr, sk->ps);
		MemoryContextSwitchTo(old);
	}
	{
		Datum		d;
		bool		isnull;
		int16		len;
		bool		byval;
		MemoryContext old;

		d = ExecEvalExprSwitchContext(n->estate, econtext, &isnull);
		old = MemoryContextSwitchTo(sk->cxt);
		if (n->kind == KS_CMP)
		{
			get_typlenbyval(n->exprtype, &len, &byval);
			n->isnull = isnull;
			n->val = isnull ? (Datum) 0 : datumCopy(d, byval, len);
		}
		else
		{
			n->isnull = isnull;
			n->nelems = 0;
			n->elems = NULL;
			n->elnulls = NULL;
			if (!isnull)
			{
				ArrayType  *arr = DatumGetArrayTypePCopy(d);
				Oid			et = ARR_ELEMTYPE(arr);
				int16		elen;
				bool		ebyval;
				char		ealign;

				get_typlenbyvalalign(et, &elen, &ebyval, &ealign);
				deconstruct_array(arr, et, elen, ebyval, ealign,
								  &n->elems, &n->elnulls, &n->nelems);
			}
		}
		MemoryContextSwitchTo(old);
	}
}

/* Evaluate the expressions of the conditions; call before the first row. */
void
kc_skip_reevaluate(KcSkip *sk, ExprContext *econtext)
{
	if (sk == NULL || sk->root == NULL)
		return;
	kc_eval_node(sk, sk->root, econtext);
	sk->evaluated = true;
}

/* ------------------------------------------------------------------------
 * Testing a stripe or a chunk group
 * ------------------------------------------------------------------------
 */

static bool kc_node_skips(KcSkip *sk, KcSkipNode *n, const KcColStat *stats,
						  uint64 nrows);

/* Compare the column value of datum form a with the expression value b. */
static int
kc_cmp_with(KcSkipNode *n, Datum a, Datum b)
{
	return DatumGetInt32(FunctionCall2Coll(&n->cmp, n->collation, a, b));
}

/* Can no row with a value in [min, max] satisfy "column OP v"? */
static bool
kc_range_excludes(KcSkipNode *n, int strategy, Datum mn, Datum mx, Datum v)
{
	switch (strategy)
	{
		case BTLessStrategyNumber:	/* col < v */
			return kc_cmp_with(n, mn, v) >= 0;
		case BTLessEqualStrategyNumber:
			return kc_cmp_with(n, mn, v) > 0;
		case BTEqualStrategyNumber:
			return kc_cmp_with(n, mn, v) > 0 || kc_cmp_with(n, mx, v) < 0;
		case BTGreaterEqualStrategyNumber:
			return kc_cmp_with(n, mx, v) < 0;
		case BTGreaterStrategyNumber:
			return kc_cmp_with(n, mx, v) <= 0;
	}
	return false;
}

static bool
kc_node_skips(KcSkip *sk, KcSkipNode *n, const KcColStat *stats, uint64 nrows)
{
	const KcColStat *st = &stats[n->attidx];
	const KcColInfo *ci = &sk->ti->cols[n->attidx];
	ListCell   *lc;

	switch (n->kind)
	{
		case KS_AND:
			foreach(lc, n->kids)
				if (kc_node_skips(sk, lfirst(lc), stats, nrows))
					return true;
			return false;
		case KS_OR:
			foreach(lc, n->kids)
				if (!kc_node_skips(sk, lfirst(lc), stats, nrows))
					return false;
			return true;
		case KS_ISNULL:
			return st->known && st->nnulls == 0;
		case KS_NOTNULL:
			return st->known && st->nnulls >= nrows;
		case KS_CMP:
		case KS_ANYEQ:
			{
				Datum		mn,
							mx;
				int			i;

				if (n->isnull)
					return true;	/* a strict operator on null: never true */
				if (!st->known)
					return false;
				if (st->nnulls >= nrows)
					return true;	/* only nulls: no strict comparison holds */
				if (!st->has_minmax)
					return false;
				mn = kc_stat_datum(ci, st->min, st->min_len);
				mx = kc_stat_datum(ci, st->max, st->max_len);
				if (n->kind == KS_CMP)
					return kc_range_excludes(n, n->strategy, mn, mx, n->val);
				/* = ANY: every non-null element must lie outside the range */
				for (i = 0; i < n->nelems; i++)
				{
					if (n->elnulls[i])
						continue;
					if (!kc_range_excludes(n, BTEqualStrategyNumber, mn, mx,
										   n->elems[i]))
						return false;
				}
				return true;
			}
	}
	return false;
}

/*
 * Can the scan leave out the stripe or chunk group whose statistics are
 * given (one entry per column of the table), which holds nrows rows?
 */
bool
kc_skip_unit(KcSkip *sk, const KcColStat *stats, uint64 nrows)
{
	MemoryContext old;
	bool		res;

	if (!kc_skip_active(sk))
		return false;
	old = MemoryContextSwitchTo(sk->tmp);
	res = kc_node_skips(sk, sk->root, stats, nrows);
	MemoryContextSwitchTo(old);
	MemoryContextReset(sk->tmp);
	return res;
}
