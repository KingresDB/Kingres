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
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/shippable.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/transam.h"
#include "catalog/pg_collation_d.h"
#include "catalog/pg_proc.h"
#include "nodes/nodeFuncs.h"
#include "utils/lsyscache.h"

#include "catalog.h"
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
	return type < FirstGenbkiObjectId;
}

/* The default collation and "C" mean the same on every node. */
static bool
collation_ok(Oid coll)
{
	return coll == InvalidOid || coll == DEFAULT_COLLATION_OID ||
		coll == C_COLLATION_OID;
}

static bool
func_ok(Oid funcid)
{
	return funcid < FirstGenbkiObjectId &&
		func_volatile(funcid) == PROVOLATILE_IMMUTABLE;
}

static bool
op_ok(Oid opno)
{
	return opno < FirstGenbkiObjectId &&
		func_volatile(get_opcode(opno)) == PROVOLATILE_IMMUTABLE;
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

	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		rtindex++;
		switch (rte->rtekind)
		{
			case RTE_RELATION:
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
			{
				CoerceViaIO *c = (CoerceViaIO *) node;

				if (!type_ok(c->resulttype) || !collation_ok(c->resultcollid) ||
					!type_ok(exprType((Node *) c->arg)))
					return fail(ctx, "it converts through a type that is not built in");
				break;
			}

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
				break;
			}

		case T_WindowFunc:
			{
				WindowFunc *w = (WindowFunc *) node;

				if (!ctx->statement)
					return fail(ctx, "it has a window function");
				if (!func_ok(w->winfnoid) || !type_ok(w->wintype) ||
					!collation_ok(w->wincollid) || !collation_ok(w->inputcollid))
					return fail(ctx, "it uses a window function that cannot be shipped");
				break;
			}

		case T_SubLink:
		case T_FromExpr:
		case T_JoinExpr:
		case T_RangeTblRef:
		case T_TargetEntry:
		case T_SortGroupClause:
		case T_WindowClause:
		case T_CommonTableExpr:
		case T_OnConflictExpr:
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
