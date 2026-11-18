/*-------------------------------------------------------------------------
 *
 * prune.c
 *	  Which shard groups a read of a distributed table has to ask (design
 *	  /shard-phase2b.md, 4.5).
 *
 * The set is computed from the restriction clauses the planner attaches to the
 * relation, at path time, and evaluated when the scan starts, so that a generic
 * plan prunes by the value of its parameters too.  The rule is the one of
 * restrict_from_conjunct() in planner.c, which routes a statement that every
 * distributed table restricts to one key (review finding 2): the operator is an
 * equality operator of the hash operator family of the distribution column and
 * the clause's input collation is the collation the table was distributed with
 * and is deterministic, because under another collation values that compare
 * equal can live in other buckets.  A clause that is not understood prunes
 * nothing, which is always correct: the scan then asks every group, and the rows
 * it gets are filtered by every restriction again.
 *
 * The bucket of a value is found by the one bucket lookup (hashing.c) and the
 * group of a bucket by the accessors of catalog.c: nothing here computes
 * hash % bucket_count.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/prune.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "nodes/pathnodes.h"
#include "nodes/primnodes.h"
#include "utils/array.h"
#include "utils/lsyscache.h"
#include "utils/typcache.h"

#include "hashing.h"
#include "kingres_shard.h"
#include "prune.h"

/*
 * An alternative of a term is a two-element list: (kind, expression).  The
 * expression is a Const or an external Param; its kind says whether it is one key
 * or an array of keys.
 */
#define PRUNE_VALUE 0
#define PRUNE_ARRAY 1

static Node *
strip_relabel(Node *n)
{
	while (n != NULL && IsA(n, RelabelType))
		n = (Node *) ((RelabelType *) n)->arg;
	return n;
}

static bool
is_key_var(const KsDistTable *dt, Index rti, Node *n)
{
	Var		   *v;

	n = strip_relabel(n);
	if (n == NULL || !IsA(n, Var))
		return false;
	v = (Var *) n;
	return v->varno == rti && v->varlevelsup == 0 && v->varattno == dt->dist_attno;
}

/* An equality operator of the key's hash family, under the table's own collation. */
static bool
equality_ok(const KsDistTable *dt, Oid opno, Oid inputcollid)
{
	TypeCacheEntry *tce = lookup_type_cache(dt->coloc->dist_type, TYPECACHE_HASH_OPFAMILY);

	if (!OidIsValid(tce->hash_opf) || !op_in_opfamily(opno, tce->hash_opf))
		return false;
	if (inputcollid != dt->coloc->collation ||
		(OidIsValid(inputcollid) && !get_collation_isdeterministic(inputcollid)))
		return false;
	return true;
}

static bool
is_value_node(Node *n)
{
	return n != NULL &&
		(IsA(n, Const) || (IsA(n, Param) && ((Param *) n)->paramkind == PARAM_EXTERN));
}

static List *
alternative(int kind, Node *expr)
{
	return list_make2(makeInteger(kind), copyObject(expr));
}

/*
 * The alternatives of one clause (a list of alternatives), or NIL when the clause
 * does not limit the keys.  An AND limits them by any one of its conjuncts (the
 * keys of the others are a subset, so the keys of one are a safe superset); an OR
 * limits them only when every arm does.
 */
static List *
clause_alternatives(const KsDistTable *dt, Index rti, Node *clause)
{
	if (clause == NULL)
		return NIL;

	if (IsA(clause, RestrictInfo))
		clause = (Node *) ((RestrictInfo *) clause)->clause;

	if (IsA(clause, OpExpr))
	{
		OpExpr	   *op = (OpExpr *) clause;
		Node	   *args[2];
		int			i;

		if (list_length(op->args) != 2)
			return NIL;
		args[0] = strip_relabel((Node *) linitial(op->args));
		args[1] = strip_relabel((Node *) lsecond(op->args));
		for (i = 0; i < 2; i++)
		{
			if (is_key_var(dt, rti, args[i]) && is_value_node(args[1 - i]) &&
				equality_ok(dt, op->opno, op->inputcollid))
				return list_make1(alternative(PRUNE_VALUE, args[1 - i]));
		}
		return NIL;
	}
	if (IsA(clause, ScalarArrayOpExpr))
	{
		ScalarArrayOpExpr *sa = (ScalarArrayOpExpr *) clause;
		Node	   *arr;

		if (!sa->useOr || list_length(sa->args) != 2 ||
			!is_key_var(dt, rti, (Node *) linitial(sa->args)) ||
			!equality_ok(dt, sa->opno, sa->inputcollid))
			return NIL;
		arr = strip_relabel((Node *) lsecond(sa->args));
		if (arr == NULL)
			return NIL;
		if (is_value_node(arr))
			return list_make1(alternative(PRUNE_ARRAY, arr));
		if (IsA(arr, ArrayExpr))
		{
			List	   *alts = NIL;
			ListCell   *lc;

			if (((ArrayExpr *) arr)->multidims)
				return NIL;
			foreach(lc, ((ArrayExpr *) arr)->elements)
			{
				Node	   *e = strip_relabel((Node *) lfirst(lc));

				if (!is_value_node(e))
					return NIL;
				alts = lappend(alts, alternative(PRUNE_VALUE, e));
			}
			return alts;
		}
		return NIL;
	}
	if (IsA(clause, BoolExpr))
	{
		BoolExpr   *b = (BoolExpr *) clause;
		ListCell   *lc;

		if (b->boolop == AND_EXPR)
		{
			foreach(lc, b->args)
			{
				List	   *alts = clause_alternatives(dt, rti, (Node *) lfirst(lc));

				if (alts != NIL)
					return alts;
			}
			return NIL;
		}
		if (b->boolop == OR_EXPR)
		{
			List	   *all = NIL;

			foreach(lc, b->args)
			{
				List	   *alts = clause_alternatives(dt, rti, (Node *) lfirst(lc));

				if (alts == NIL)
					return NIL;
				all = list_concat(all, alts);
			}
			return all;
		}
	}
	return NIL;
}

List *
kingres_shard_prune_terms(const KsDistTable *dt, Index rti, List *restrictinfo)
{
	List	   *terms = NIL;
	ListCell   *lc;

	foreach(lc, restrictinfo)
	{
		List	   *alts = clause_alternatives(dt, rti, (Node *) lfirst(lc));

		if (alts != NIL)
			terms = lappend(terms, alts);
	}
	return terms;
}

/* The value of a Const or an external parameter; false when it has none yet. */
static bool
fetch_value(Node *n, ParamListInfo params, Datum *value, bool *isnull, Oid *type)
{
	if (IsA(n, Const))
	{
		Const	   *c = (Const *) n;

		*value = c->constvalue;
		*isnull = c->constisnull;
		*type = c->consttype;
		return true;
	}
	if (IsA(n, Param))
	{
		Param	   *p = (Param *) n;
		ParamExternData *prm;
		ParamExternData workspace;

		if (p->paramkind != PARAM_EXTERN || params == NULL ||
			p->paramid < 1 || p->paramid > params->numParams)
			return false;
		if (params->paramFetch != NULL)
			prm = params->paramFetch(params, p->paramid, false, &workspace);
		else
			prm = &params->params[p->paramid - 1];
		if (!OidIsValid(prm->ptype))
			return false;
		*value = prm->value;
		*isnull = prm->isnull;
		*type = prm->ptype;
		return true;
	}
	return false;
}

/*
 * The group of the bucket of one key value is added to *groups.  False when the
 * value cannot be placed (the type has no hash function, the bucket has no
 * owner): the term then prunes nothing.  A NULL is equal to nothing.
 */
static bool
add_group_of(const KsDistTable *dt, Oid type, Datum value, bool isnull, List **groups)
{
	int			bucket;
	int			g;

	if (isnull)
		return true;
	if (!kingres_shard_try_bucket(dt->coloc->dist_type, dt->coloc->collation, dt->coloc,
								  type, value, false, &bucket))
		return false;
	g = kingres_shard_bucket_group(dt->coloc, bucket);
	if (g < 0)
		return false;
	*groups = list_append_unique_int(*groups, g);
	return true;
}

static bool
alternative_groups(const KsDistTable *dt, List *alt, ParamListInfo params, List **groups)
{
	int			kind = intVal(linitial(alt));
	Node	   *n = (Node *) lsecond(alt);
	Datum		value;
	bool		isnull;
	Oid			type;

	if (!fetch_value(n, params, &value, &isnull, &type))
		return false;
	if (kind == PRUNE_VALUE)
		return add_group_of(dt, type, value, isnull, groups);

	/* An array: every non-NULL element is a possible key. */
	{
		ArrayType  *arr;
		Oid			elemtype;
		int16		typlen;
		bool		typbyval;
		char		typalign;
		Datum	   *elems;
		bool	   *nulls;
		int			nelems;
		int			i;

		if (isnull)
			return true;
		arr = DatumGetArrayTypeP(value);
		elemtype = ARR_ELEMTYPE(arr);
		get_typlenbyvalalign(elemtype, &typlen, &typbyval, &typalign);
		deconstruct_array(arr, elemtype, typlen, typbyval, typalign, &elems, &nulls, &nelems);
		for (i = 0; i < nelems; i++)
			if (!add_group_of(dt, elemtype, elems[i], nulls[i], groups))
				return false;
	}
	return true;
}

List *
kingres_shard_prune_groups(const KsDistTable *dt, List *terms, ParamListInfo params,
						   bool *pruned)
{
	List	   *allowed = NIL;
	bool		have = false;
	ListCell   *lc;

	foreach(lc, terms)
	{
		List	   *term = (List *) lfirst(lc);
		List	   *tg = NIL;
		bool		known = true;
		ListCell   *lc2;

		foreach(lc2, term)
		{
			if (!alternative_groups(dt, (List *) lfirst(lc2), params, &tg))
			{
				known = false;
				break;
			}
		}
		if (!known)
			continue;
		if (!have)
		{
			allowed = tg;
			have = true;
		}
		else
			allowed = list_intersection_int(allowed, tg);
	}
	*pruned = have;
	return allowed;
}
