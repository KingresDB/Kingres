/*-------------------------------------------------------------------------
 *
 * count_distinct.c
 *	  kingres_shard.count_distinct_mode: count(DISTINCT x) made approximate.
 *
 * Design: kingres/docs/design/analytics-phase12-queries.md, section 9.8.  With the setting 'approximate' the planner hook rewrites, in
 * every query level that reads a distributed table (a hash-distributed one: not a reference table, not a table of a distributed
 * schema), each
 *
 *		count(DISTINCT e) [FILTER (WHERE ...)]
 *
 * whose argument is of a built-in scalar type that has an extended hash function, into
 * kingres_sketch.approx_count_distinct(e, p), the same Aggref with the function replaced, the DISTINCT clause cleared and the
 * precision p added as a constant.  The new aggregate has a combine function and serial functions, so the statement is planned as a
 * partial aggregation: every shard group returns one sketch per group, not its rows.
 *
 * Left alone: count(DISTINCT a, b), a type without a hash function or that is not built in (a user type, an array, a range, an enum, a
 * record, oid and the reg types, whose hash depends on an object identifier), an aggregate with ORDER BY, a level that has no
 * distributed table, and a database where kingres_sketch is not installed at a version the list of the extension accepts (one
 * warning per session).  The setting is USERSET: it changes results, so it is never on by default.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/count_distinct.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include <math.h>

#include "access/transam.h"
#include "catalog/dependency.h"
#include "catalog/pg_aggregate.h"
#include "catalog/pg_namespace_d.h"
#include "catalog/pg_proc.h"
#include "catalog/pg_type.h"
#include "commands/extension.h"
#include "miscadmin.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "parser/parse_func.h"
#include "utils/fmgroids.h"
#include "utils/guc.h"
#include "utils/acl.h"
#include "utils/lsyscache.h"
#include "utils/plancache.h"
#include "utils/typcache.h"

#include "catalog.h"
#include "count_distinct.h"
#include "shippable.h"

#define KS_SKETCH_EXTENSION "kingres_sketch"
#define KS_HLL_MIN_P 4
#define KS_HLL_MAX_P 18

int			kingres_shard_count_distinct_mode = KS_COUNT_DISTINCT_EXACT;
double		kingres_shard_count_distinct_error_rate = 0.01;

static const struct config_enum_entry count_distinct_modes[] = {
	{"exact", KS_COUNT_DISTINCT_EXACT, false},
	{"approximate", KS_COUNT_DISTINCT_APPROXIMATE, false},
	{NULL, 0, false}
};

typedef struct CdContext
{
	const KsCatalog *cat;
	Oid			fnoid;			/* approx_count_distinct(anyelement, int), resolved when the first eligible aggregate is met */
	bool		resolved;
	bool		usable;
	int			precision;
	int			nrewritten;
} CdContext;

static bool warned_missing = false;
static bool warned_privilege = false;

/*
 * The function of the rewrite, looked up when the first eligible count(DISTINCT) is met, never for a statement that has none (P12S-01):
 * an ordinary role must not be locked out of the database by a setting.  The role needs USAGE on the schema of kingres_sketch and EXECUTE
 * on the aggregate; without either, or without the extension at a version that is accepted, the statement stays exact and one warning
 * is raised for the session.
 */
static bool
cd_resolve(CdContext *cx)
{
	Oid			argtypes[2] = {ANYELEMENTOID, INT4OID};
	Oid			extoid;
	Oid			nspoid;

	if (cx->resolved)
		return cx->usable;
	cx->resolved = true;
	cx->usable = false;
	extoid = get_extension_oid(KS_SKETCH_EXTENSION, true);
	if (!OidIsValid(extoid) || !kingres_shard_extension_usable(KS_SKETCH_EXTENSION))
	{
		if (!warned_missing)
		{
			warned_missing = true;
			ereport(WARNING,
					(errmsg("count_distinct_mode is approximate but kingres_sketch is not installed in this database, or its version is not one that kingres_shard accepts"),
					 errhint("count(DISTINCT x) stays exact.")));
		}
		return false;
	}
	nspoid = get_extension_schema(extoid);
	if (object_aclcheck(NamespaceRelationId, nspoid, GetUserId(), ACL_USAGE) == ACLCHECK_OK)
	{
		cx->fnoid = LookupFuncName(list_make2(makeString(get_namespace_name(nspoid)), makeString("approx_count_distinct")),
								   2, argtypes, true);
		if (OidIsValid(cx->fnoid) && getExtensionOfObject(ProcedureRelationId, cx->fnoid) != extoid)
			cx->fnoid = InvalidOid;
		if (OidIsValid(cx->fnoid) && object_aclcheck(ProcedureRelationId, cx->fnoid, GetUserId(), ACL_EXECUTE) != ACLCHECK_OK)
			cx->fnoid = InvalidOid;
	}
	if (!OidIsValid(cx->fnoid))
	{
		if (!warned_privilege)
		{
			warned_privilege = true;
			ereport(WARNING,
					(errmsg("count_distinct_mode is approximate but this role may not use kingres_sketch (it needs USAGE on its schema and EXECUTE on approx_count_distinct)"),
					 errhint("count(DISTINCT x) stays exact for this role.")));
		}
		return false;
	}
	cx->usable = true;
	return true;
}

/* a cached plan was made with the setting that was in force: a change of the setting, in either direction, drops the cached plans (P12S-02) */
static void
assign_count_distinct_mode(int newval, void *extra)
{
	if (newval != kingres_shard_count_distinct_mode)
		ResetPlanCache();
}

static void
assign_count_distinct_error_rate(double newval, void *extra)
{
	if (newval != kingres_shard_count_distinct_error_rate)
		ResetPlanCache();
}

void
kingres_shard_count_distinct_init(void)
{
	DefineCustomEnumVariable("kingres_shard.count_distinct_mode",
							 "Whether count(DISTINCT x) on a distributed table is exact or approximate.",
							 "exact (the default) counts exactly; approximate rewrites count(DISTINCT x), for an x of a built-in type with a hash function, into kingres_sketch.approx_count_distinct(x, p), which every shard group answers with a small sketch.  It changes the results of the statements it applies to (a relative standard error of count_distinct_error_rate), so it is never on by default.  It needs kingres_sketch.",
							 &kingres_shard_count_distinct_mode,
							 KS_COUNT_DISTINCT_EXACT,
							 count_distinct_modes,
							 PGC_USERSET, 0,
							 NULL, assign_count_distinct_mode, NULL);
	DefineCustomRealVariable("kingres_shard.count_distinct_error_rate",
							 "The relative standard error asked of count_distinct_mode = approximate.",
							 "The precision of the sketch is the smallest p from 4 to 18 with 1.04 / sqrt(2^p) not above this rate: 0.01 gives p = 14, 0.005 gives p = 16.",
							 &kingres_shard_count_distinct_error_rate,
							 0.01, 0.0005, 0.1,
							 PGC_USERSET, 0,
							 NULL, assign_count_distinct_error_rate, NULL);
}

/* The precision of a sketch with at most this relative standard error. */
int
kingres_shard_count_distinct_precision(double rate)
{
	int			p = (int) ceil(2.0 * log2(1.04 / rate));

	if (p < KS_HLL_MIN_P)
		p = KS_HLL_MIN_P;
	if (p > KS_HLL_MAX_P)
		p = KS_HLL_MAX_P;
	return p;
}

/* Is the argument type one the sketch hashes the same way on every node, and the planner may hand it to the sketch? */
static bool
type_hashable(Oid typid)
{
	Oid			base = getBaseType(typid);
	TypeCacheEntry *tce;

	if (base >= FirstGenbkiObjectId || get_typtype(base) != TYPTYPE_BASE)
		return false;
	if (OidIsValid(get_element_type(base)))
		return false;
	tce = lookup_type_cache(base, TYPECACHE_HASH_EXTENDED_PROC_FINFO);
	if (!OidIsValid(tce->hash_extended_proc_finfo.fn_oid))
		return false;
	if (tce->hash_extended_proc_finfo.fn_oid == F_HASHOIDEXTENDED || tce->hash_extended_proc_finfo.fn_oid == F_HASHOIDVECTOREXTENDED)
		return false;
	return true;
}

static bool
agg_eligible(Aggref *a, Oid *argtype)
{
	TargetEntry *te;

	if (a->aggfnoid != F_COUNT_ANY || a->aggdistinct == NIL || a->aggorder != NIL || a->aggstar ||
		a->aggvariadic || a->agglevelsup != 0 || a->aggkind != AGGKIND_NORMAL || a->aggdirectargs != NIL ||
		list_length(a->args) != 1)
		return false;
	te = (TargetEntry *) linitial(a->args);
	*argtype = exprType((Node *) te->expr);
	return type_hashable(*argtype);
}

static Node *
agg_mutator(Node *node, void *context)
{
	CdContext  *cx = (CdContext *) context;
	Oid			argtype;

	if (node == NULL)
		return NULL;
	if (IsA(node, Aggref) && agg_eligible((Aggref *) node, &argtype))
	{
		Aggref	   *n;
		TargetEntry *te;
		Const	   *p;

		if (!cd_resolve(cx))
			return node;			/* stays exact */
		n = (Aggref *) copyObject(node);
		te = (TargetEntry *) copyObject(linitial(n->args));
		p = makeConst(INT4OID, -1, InvalidOid, sizeof(int32), Int32GetDatum(cx->precision), false, true);

		te->ressortgroupref = 0;	/* it named the DISTINCT clause, which is gone */
		n->aggfnoid = cx->fnoid;
		n->aggtype = INT8OID;
		n->aggargtypes = list_make2_oid(argtype, INT4OID);
		n->args = list_make2(te, makeTargetEntry((Expr *) p, 2, NULL, false));
		n->aggdistinct = NIL;
		n->aggorder = NIL;
		cx->nrewritten++;
		return (Node *) n;
	}
	return expression_tree_mutator(node, agg_mutator, context);
}

static bool level_reads_distributed(Query *q, const KsCatalog *cat);
static bool cd_walker(Node *node, void *context);

static void
rewrite_level(Query *q, CdContext *cx)
{
	if (level_reads_distributed(q, cx->cat))
	{
		q->targetList = (List *) expression_tree_mutator((Node *) q->targetList, agg_mutator, cx);
		q->havingQual = expression_tree_mutator(q->havingQual, agg_mutator, cx);
	}
	/* the sub-selects, CTEs and sub-links of this level, each a level of its own */
	(void) query_tree_walker(q, cd_walker, cx, 0);
}

static bool
cd_walker(Node *node, void *context)
{
	if (node == NULL)
		return false;
	if (IsA(node, Query))
	{
		rewrite_level((Query *) node, (CdContext *) context);
		return false;
	}
	return expression_tree_walker(node, cd_walker, context);
}

static bool
level_reads_distributed(Query *q, const KsCatalog *cat)
{
	ListCell   *lc;

	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);
		KsDistTable *dt;

		if (rte->rtekind != RTE_RELATION)
			continue;
		dt = hash_search(cat->dist_tables, &rte->relid, HASH_FIND, NULL);
		if (dt != NULL && !dt->is_reference)
			return true;
	}
	return false;
}

/*
 * kingres_shard_apply_count_distinct
 *
 * Called by the planner hook with the statement before it is analyzed.  Returns the number of aggregates rewritten.
 */
int
kingres_shard_apply_count_distinct(Query *parse, const KsCatalog *cat)
{
	CdContext	cx;

	if (kingres_shard_count_distinct_mode != KS_COUNT_DISTINCT_APPROXIMATE ||
		cat == NULL || !cat->present || cat->dist_tables == NULL)
		return 0;

	cx.fnoid = InvalidOid;
	cx.resolved = false;
	cx.usable = false;
	cx.cat = cat;
	cx.precision = kingres_shard_count_distinct_precision(kingres_shard_count_distinct_error_rate);
	cx.nrewritten = 0;
	rewrite_level(parse, &cx);
	return cx.nrewritten;
}
