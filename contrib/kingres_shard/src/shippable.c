/*-------------------------------------------------------------------------
 *
 * shippable.c
 *	  Deciding whether an expression or a whole statement can be sent to
 *	  another node and mean the same there.
 *
 * The question is the one contrib/postgres_fdw answers in is_foreign_expr()
 * (deparse.c; PostgreSQL License, Copyright (c) 2012-2025, PostgreSQL Global
 * Development Group).  The answer here is stricter, because the other nodes
 * are not foreign servers but peers that must agree with this node:
 *
 *	- only built-in functions and operators (objects that exist, with the same
 *	  meaning, on every node), and only immutable ones;
 *	- only built-in types, so that a constant, a parameter or a result is
 *	  written and read the same on both sides;
 *	- only the default collation (of the database, the same on every node by
 *	  construction of the cluster) or "C", whose comparisons do not depend on a
 *	  locale library;
 *	- in a clause: only columns of the one table, constants and external
 *	  parameters (no subqueries, no executor parameters, no aggregates).
 *
 * A clause that fails the test is evaluated on this node, on the rows that
 * were fetched, so a refusal costs speed, never correctness.  A statement
 * that fails the test is not shipped at all (the caller refuses it).
 *
 * SPDX-License-Identifier: AGPL-3.0-only AND PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/shippable.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "access/table.h"
#include "access/transam.h"
#include "catalog/pg_class.h"
#include "catalog/pg_index.h"
#include "catalog/pg_type_d.h"
#include "catalog/pg_collation_d.h"
#include "catalog/pg_proc.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "optimizer/optimizer.h"
#include "utils/rel.h"
#include "utils/relcache.h"
#include "utils/syscache.h"
#include "utils/lsyscache.h"

#include "catalog.h"
#include "pg_compat.h"
#include "shippable.h"

typedef struct ShipCtx
{
	Index		varno;			/* clause mode: the only table; else 0 */
	bool		statement;		/* a whole statement, not one clause */
	Oid			shipped_relid;	/* statement mode: the table being written */
	Query	   *top;			/* statement mode: the statement itself */
	const char *reason;
} ShipCtx;

static bool ship_walker(Node *node, ShipCtx *ctx);

static bool
fail(ShipCtx *ctx, const char *reason)
{
	if (ctx->reason == NULL)
		ctx->reason = reason;
	return true;
}

/* A type that exists with the same meaning on every node. */
static bool
type_ok(Oid type)
{
	/*
	 * money (and arrays of it) is not shippable: its text form depends on
	 * lc_monetary of the session that reads or writes it, which can differ between
	 * nodes, so a value or a restriction of that type could mean another amount
	 * there.
	 */
	if (type == CASHOID || get_element_type(type) == CASHOID)
		return false;
	return type < FirstGenbkiObjectId;
}

/* The default collation and "C" mean the same on every node. */
static bool
collation_ok(Oid coll)
{
	return coll == InvalidOid || coll == DEFAULT_COLLATION_OID ||
		coll == C_COLLATION_OID;
}

/*
 * Does the function compute its floating point result with the C library of the
 * node (review finding S3-14)?  The basic operations and the square root are
 * correctly rounded by IEEE 754 and give the same bits everywhere, but
 * exp, ln, pow, the trigonometric and the gamma functions are not: two nodes
 * with another libm (another CPU architecture or distribution) can differ in
 * the last bit, and a write that applies such a function on every node would
 * make the copies of a reference table differ without an error.  The
 * numeric versions of the same names (sqrt(numeric), exp(numeric), ...) are
 * implemented by PostgreSQL itself and give the same digits everywhere, so the
 * test is on the name and a floating point result.
 */
static bool
libm_dependent(Oid funcid)
{
	static const char *const names[] = {
		"cbrt", "dcbrt", "exp", "dexp", "ln", "log", "dlog1", "log10", "dlog10",
		"pow", "power", "dpow", "sin", "cos", "tan", "cot", "asin", "acos", "atan", "atan2", "sinh", "cosh", "tanh",
		"asinh", "acosh", "atanh", "sind", "cosd", "tand", "cotd", "asind",
		"acosd", "atand", "atan2d", "erf", "erfc", "gamma", "lgamma", NULL
	};
	Oid			rettype = get_func_rettype(funcid);
	char	   *name;
	int			i;

	if (rettype != FLOAT8OID && rettype != FLOAT4OID)
		return false;
	name = get_func_name(funcid);
	if (name == NULL)
		return false;
	for (i = 0; names[i] != NULL; i++)
	{
		if (strcmp(name, names[i]) == 0)
			return true;
	}
	return false;
}

static bool
func_ok(Oid funcid)
{
	return funcid < FirstGenbkiObjectId &&
		func_volatile(funcid) == PROVOLATILE_IMMUTABLE &&
		!libm_dependent(funcid);
}

static bool
op_ok(Oid opno)
{
	return opno < FirstGenbkiObjectId &&
		func_ok(get_opcode(opno));
}

/*
 * A relation of a statement that is shipped: a reference table (every node has
 * the full table) or the one table the statement writes.
 */
static bool
relation_ok(Oid relid, ShipCtx *ctx, Query *q, int rtindex)
{
	const KsDistTable *dt = kingres_shard_lookup_dist_table(relid);

	if (dt != NULL && dt->is_reference)
		return true;
	/*
	 * The distributed table that is written appears once, as the target: any
	 * other reading of it (a subquery, a self join) would need the rows of the
	 * other shard groups, which a statement run group by group does not see.
	 */
	if (relid == ctx->shipped_relid)
		return q == ctx->top && rtindex == q->resultRelation;
	return false;
}

/*
 * An aggregate whose result can differ between two nodes that hold the same
 * rows in a different physical order: the ones that build a list or pick a row
 * in input order, and those that add floating point numbers (the sum depends
 * on the order of the additions).
 */
static bool
agg_order_sensitive(Aggref *a)
{
	static const char *const names[] = {
		"array_agg", "string_agg", "json_agg", "jsonb_agg", "json_object_agg",
		"jsonb_object_agg", "json_object_agg_unique", "jsonb_object_agg_unique",
		"json_object_agg_strict", "jsonb_object_agg_strict",
		"json_object_agg_unique_strict", "jsonb_object_agg_unique_strict",
		"xmlagg", "any_value", NULL
	};
	char	   *name = get_func_name(a->aggfnoid);
	ListCell   *lc;
	int			i;

	if (name != NULL)
	{
		for (i = 0; names[i] != NULL; i++)
			if (strcmp(name, names[i]) == 0)
				return true;
	}
	if (a->aggtype == FLOAT4OID || a->aggtype == FLOAT8OID)
		return true;
	foreach(lc, a->args)
	{
		TargetEntry *te = lfirst_node(TargetEntry, lc);
		Oid			t = exprType((Node *) te->expr);

		if (t == FLOAT4OID || t == FLOAT8OID)
			return true;
	}
	return false;
}

/* The top-level conditions of a join tree that every result row satisfies. */
static void
collect_inner_quals(Node *n, List **quals)
{
	if (n == NULL)
		return;
	if (IsA(n, FromExpr))
	{
		FromExpr   *f = (FromExpr *) n;
		ListCell   *lc;

		if (f->quals != NULL)
			*quals = list_concat(*quals, make_ands_implicit((Expr *) f->quals));
		foreach(lc, f->fromlist)
			collect_inner_quals((Node *) lfirst(lc), quals);
	}
	else if (IsA(n, JoinExpr))
	{
		JoinExpr   *j = (JoinExpr *) n;

		if (j->jointype == JOIN_INNER)
		{
			if (j->quals != NULL)
				*quals = list_concat(*quals, make_ands_implicit((Expr *) j->quals));
			collect_inner_quals(j->larg, quals);
			collect_inner_quals(j->rarg, quals);
		}
	}
}

/*
 * Does the relation (range table index rti) match at most one row for each
 * combination of the other relations, because the conditions equate all the
 * columns of one of its unique indexes with expressions that do not use the
 * relation itself?
 */
static bool
matches_at_most_one(int rti, List *quals, Oid relid)
{
	Bitmapset  *cols = NULL;
	ListCell   *lc;
	Relation	rel;
	List	   *indexes;
	bool		ok = false;

	foreach(lc, quals)
	{
		Node	   *qual = (Node *) lfirst(lc);
		OpExpr	   *op;
		int			side;

		if (!IsA(qual, OpExpr))
			continue;
		op = (OpExpr *) qual;
		if (list_length(op->args) != 2 ||
			!op_mergejoinable(op->opno, exprType(linitial(op->args))))
			continue;
		for (side = 0; side < 2; side++)
		{
			Node	   *mine = (Node *) list_nth(op->args, side);
			Node	   *other = (Node *) list_nth(op->args, 1 - side);

			while (IsA(mine, RelabelType))
				mine = (Node *) ((RelabelType *) mine)->arg;
			if (IsA(mine, Var) && ((Var *) mine)->varno == (Index) rti &&
				((Var *) mine)->varlevelsup == 0 && ((Var *) mine)->varattno > 0 &&
				!bms_is_member(rti, pull_varnos(NULL, other)))
				cols = bms_add_member(cols, ((Var *) mine)->varattno);
		}
	}
	if (bms_is_empty(cols))
		return false;

	rel = table_open(relid, AccessShareLock);
	indexes = RelationGetIndexList(rel);
	foreach(lc, indexes)
	{
		HeapTuple	tup = SearchSysCache1(INDEXRELID, ObjectIdGetDatum(lfirst_oid(lc)));
		Form_pg_index ix;
		int			k;
		bool		all = true;

		if (!HeapTupleIsValid(tup))
			continue;
		ix = (Form_pg_index) GETSTRUCT(tup);
		if (ix->indisunique && ix->indisvalid && ix->indimmediate &&
			heap_attisnull(tup, Anum_pg_index_indpred, NULL) &&
			heap_attisnull(tup, Anum_pg_index_indexprs, NULL))
		{
			for (k = 0; k < ix->indnkeyatts; k++)
			{
				if (ix->indkey.values[k] <= 0 || !bms_is_member(ix->indkey.values[k], cols))
				{
					all = false;
					break;
				}
			}
			if (all)
				ok = true;
		}
		ReleaseSysCache(tup);
		if (ok)
			break;
	}
	list_free(indexes);
	table_close(rel, NoLock);
	return ok;
}

static bool
query_ok(Query *q, ShipCtx *ctx)
{
	ListCell   *lc;
	int			rtindex = 0;

	if (q != ctx->top && q->commandType != CMD_SELECT)
		return fail(ctx, "it has a data-modifying statement inside a subquery or a WITH clause");
	if (q->commandType == CMD_MERGE || q->commandType == CMD_UTILITY)
		return fail(ctx, "it is a MERGE or a utility statement");
	if (q->rowMarks != NIL)
		return fail(ctx, "it has a row locking clause");
	/*
	 * The rule of review finding S2-05, in one place: nothing whose result can
	 * differ between two nodes that hold the same set of rows.  LIMIT and
	 * OFFSET (which rows come first depends on the physical order unless the
	 * order is total, and that cannot be proved here) and DISTINCT ON (an
	 * arbitrary row of each group) are refused outright, so are window
	 * functions, system columns, order-sensitive aggregates, table sampling and
	 * an UPDATE ... FROM that can match several rows to one target row.
	 */
	if (q->limitCount != NULL || q->limitOffset != NULL)
		return fail(ctx, "it has LIMIT or OFFSET, which picks rows by their order");
	if (q->hasDistinctOn)
		return fail(ctx, "it has DISTINCT ON, which picks an arbitrary row of each group");
	if (q->hasWindowFuncs)
		return fail(ctx, "it has a window function, whose result can depend on the order of the rows");

	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		rtindex++;
		switch (rte->rtekind)
		{
			case RTE_RELATION:
				if (rte->tablesample != NULL)
					return fail(ctx, "it samples a table at random");

				/*
				 * A write through an updatable view (review finding S3-18).  The
				 * rewriter turns it into a statement on the base table, but that
				 * statement is not one the other nodes can run: it reads the
				 * base table a second time to find the rows (the join on the row
				 * identifier is added by the planner), so the text that is sent
				 * would be wrong, and WITH CHECK OPTION and the rights of the
				 * view's owner cannot be put into it.  Only a view that is written
				 * has such an entry, a view that is read is expanded.
				 */
				if (rte->relkind == RELKIND_VIEW)
					return fail(ctx, "it writes through a view; write the table itself");
				if (!relation_ok(rte->relid, ctx, q, rtindex))
					return fail(ctx, "it reads a table that is not a reference table");
				break;
			case RTE_SUBQUERY:
			case RTE_JOIN:
			case RTE_FUNCTION:
			case RTE_VALUES:
			case RTE_CTE:
			case RTE_RESULT:
			case RTE_GROUP:
				break;
			default:
				return fail(ctx, "it uses a table function or another kind of relation that cannot be shipped");
		}
	}
	/*
	 * UPDATE ... FROM (and DELETE ... USING ... RETURNING): when several rows
	 * of the FROM relations match one target row, which of them supplies the
	 * values is arbitrary and depends on the physical order.  Accepted only if
	 * every FROM relation is a table matched through a unique key.
	 */
	if (q == ctx->top &&
		(q->commandType == CMD_UPDATE ||
		 (q->commandType == CMD_DELETE && q->returningList != NIL)))
	{
		List	   *quals = NIL;
		int			i = 0;

		collect_inner_quals((Node *) q->jointree, &quals);
		foreach(lc, q->rtable)
		{
			RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

			i++;
			if (i == q->resultRelation || !rte->inFromCl)
				continue;
			if (rte->rtekind == RTE_JOIN || rte->rtekind == RTE_GROUP ||
				rte->rtekind == RTE_RESULT)
				continue;
			if (rte->rtekind != RTE_RELATION || !matches_at_most_one(i, quals, rte->relid))
				return fail(ctx, "it joins a table that can match several rows to one row of the target (the one that is used is arbitrary)");
		}
	}
	return query_tree_walker(q, ship_walker, ctx, 0);
}

static bool
ship_walker(Node *node, ShipCtx *ctx)
{
	if (node == NULL)
		return false;

	switch (nodeTag(node))
	{
		case T_Var:
			{
				Var		   *v = (Var *) node;

				/*
				 * A system column (ctid, xmin, tableoid and the others) names the
				 * physical layout of one node, which differs between two nodes
				 * holding the same rows (review finding S2-05).
				 */
				if (v->varattno < 0)
					return fail(ctx, "it uses a system column, whose value depends on the physical layout of one node");

				if (!ctx->statement &&
					(v->varno != ctx->varno || v->varlevelsup != 0 || v->varattno <= 0))
					return fail(ctx, "it uses a column of another table or a system column");
				/* The whole-row column of an allowed table (ON CONFLICT DO UPDATE has one). */
				if (ctx->statement && v->varattno == 0 &&
					OidIsValid(get_typ_typrelid(v->vartype)) &&
					relation_ok(get_typ_typrelid(v->vartype), ctx, ctx->top, ctx->top->resultRelation))
					return false;
				if (!type_ok(v->vartype) || !collation_ok(v->varcollid))
					return fail(ctx, psprintf("it uses a column of a type (%u) or collation (%u) that is not built in", v->vartype, v->varcollid));
				return false;
			}

		case T_Const:
			{
				Const	   *c = (Const *) node;

				if (!type_ok(c->consttype) || !collation_ok(c->constcollid))
					return fail(ctx, "it uses a constant of a type or collation that is not built in");
				return false;
			}

		case T_Param:
			{
				Param	   *p = (Param *) node;

				if (p->paramkind != PARAM_EXTERN)
					return fail(ctx, "it uses a parameter that is computed during execution");
				if (!type_ok(p->paramtype) || !collation_ok(p->paramcollid))
					return fail(ctx, "it uses a parameter of a type or collation that is not built in");
				return false;
			}

		case T_FuncExpr:
			{
				FuncExpr   *f = (FuncExpr *) node;

				if (!func_ok(f->funcid))
					return fail(ctx, "it uses a function that is not built in and immutable");
				if (!type_ok(f->funcresulttype) || !collation_ok(f->funccollid) ||
					!collation_ok(f->inputcollid))
					return fail(ctx, "it uses a type or collation that is not built in");
				break;
			}

		case T_OpExpr:
		case T_DistinctExpr:
		case T_NullIfExpr:
			{
				OpExpr	   *o = (OpExpr *) node;

				if (!op_ok(o->opno))
					return fail(ctx, "it uses an operator that is not built in and immutable");
				if (!type_ok(o->opresulttype) || !collation_ok(o->opcollid) ||
					!collation_ok(o->inputcollid))
					return fail(ctx, "it uses a type or collation that is not built in");
				break;
			}

		case T_ScalarArrayOpExpr:
			{
				ScalarArrayOpExpr *o = (ScalarArrayOpExpr *) node;

				if (!op_ok(o->opno) || !collation_ok(o->inputcollid))
					return fail(ctx, "it uses an operator that is not built in and immutable");
				break;
			}

		case T_RelabelType:
			{
				RelabelType *r = (RelabelType *) node;

				if (!type_ok(r->resulttype) || !collation_ok(r->resultcollid))
					return fail(ctx, "it uses a type or collation that is not built in");
				break;
			}

		case T_CoerceViaIO:
			/*
			 * A conversion through the text input and output functions of the
			 * two types: those functions of date, time, interval, float, bytea
			 * and others depend on session settings (DateStyle, IntervalStyle,
			 * extra_float_digits, bytea_output, TimeZone), which are not the
			 * settings of the node that receives the fragment.  postgres_fdw
			 * refuses it too (review finding S2-03).
			 */
			return fail(ctx, "it converts a value through its text form, which depends on session settings");

		case T_ArrayExpr:
			{
				ArrayExpr  *a = (ArrayExpr *) node;

				if (!type_ok(a->array_typeid) || !type_ok(a->element_typeid) ||
					!collation_ok(a->array_collid))
					return fail(ctx, "it uses an array of a type that is not built in");
				break;
			}

		case T_CaseExpr:
			{
				CaseExpr   *c = (CaseExpr *) node;

				if (!type_ok(c->casetype) || !collation_ok(c->casecollid))
					return fail(ctx, "it uses a type or collation that is not built in");
				break;
			}

		case T_CoalesceExpr:
			{
				CoalesceExpr *c = (CoalesceExpr *) node;

				if (!type_ok(c->coalescetype) || !collation_ok(c->coalescecollid))
					return fail(ctx, "it uses a type or collation that is not built in");
				break;
			}

		case T_MinMaxExpr:
			{
				MinMaxExpr *m = (MinMaxExpr *) node;

				if (!type_ok(m->minmaxtype) || !collation_ok(m->minmaxcollid) ||
					!collation_ok(m->inputcollid))
					return fail(ctx, "it uses a type or collation that is not built in");
				break;
			}

		case T_CaseWhen:
		case T_CaseTestExpr:
		case T_BoolExpr:
		case T_NullTest:
		case T_BooleanTest:
		case T_List:
			break;

		case T_Query:
			if (!ctx->statement)
				return fail(ctx, "it has a subquery");
			return query_ok((Query *) node, ctx);

		/* The rest is statement structure; only a whole statement has it. */
		case T_Aggref:
			{
				Aggref	   *a = (Aggref *) node;

				if (!ctx->statement)
					return fail(ctx, "it has an aggregate");
				if (!func_ok(a->aggfnoid) || !type_ok(a->aggtype) ||
					!collation_ok(a->aggcollid) || !collation_ok(a->inputcollid) ||
					a->aggorder != NIL)
					return fail(ctx, "it uses an aggregate that cannot be shipped");
				if (agg_order_sensitive(a))
					return fail(ctx, "it uses an aggregate whose result can depend on the order of the rows");
				break;
			}

		case T_WindowFunc:
			{
				WindowFunc *w = (WindowFunc *) node;

				if (!ctx->statement)
					return fail(ctx, "it has a window function");
				/* row_number() and friends depend on the order of the rows (S2-05) */
				return fail(ctx, "it has a window function, whose result can depend on the order of the rows");
				if (!func_ok(w->winfnoid) || !type_ok(w->wintype) ||
					!collation_ok(w->wincollid) || !collation_ok(w->inputcollid))
					return fail(ctx, "it uses a window function that cannot be shipped");
				break;
			}

		case T_OnConflictExpr:
			if (!ctx->statement)
				return fail(ctx, "it has the structure of a whole statement");
			if (!ks_onconflict_action_shippable(((OnConflictExpr *) node)->action))
				return fail(ctx, psprintf("its ON CONFLICT action (%d) is not one that can be shipped", (int) ((OnConflictExpr *) node)->action));
			break;

		case T_SubLink:
		case T_FromExpr:
		case T_JoinExpr:
		case T_RangeTblRef:
		case T_TargetEntry:
		case T_SortGroupClause:
		case T_WindowClause:
		case T_CommonTableExpr:
		case T_InferenceElem:
		case T_RangeTblFunction:
			if (!ctx->statement)
				return fail(ctx, "it has the structure of a whole statement");
			break;

		default:
			return fail(ctx, psprintf("it has an expression of kind %d that cannot be shipped", (int) nodeTag(node)));
	}

	return expression_tree_walker(node, ship_walker, ctx);
}

bool
kingres_shard_clause_shippable(Node *clause, Index varno)
{
	ShipCtx		ctx;

	memset(&ctx, 0, sizeof(ctx));
	ctx.varno = varno;
	return !ship_walker(clause, &ctx);
}

bool
kingres_shard_stmt_shippable(Query *query, Oid shipped_relid, const char **reason)
{
	ShipCtx		ctx;
	bool		bad;

	memset(&ctx, 0, sizeof(ctx));
	ctx.statement = true;
	ctx.shipped_relid = shipped_relid;
	ctx.top = query;
	bad = query_ok(query, &ctx);
	if (bad && reason != NULL)
		*reason = ctx.reason ? ctx.reason : "it cannot be shipped";
	return !bad;
}
