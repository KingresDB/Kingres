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
#include "catalog/dependency.h"
#include "catalog/pg_extension.h"
#include "catalog/pg_language_d.h"
#include "catalog/pg_operator_d.h"
#include "commands/extension.h"
#include "lib/stringinfo.h"
#include "utils/builtins.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "optimizer/optimizer.h"
#include "utils/rel.h"
#include "utils/relcache.h"
#include "utils/syscache.h"
#include "utils/rangetypes.h"
#include "access/relation.h"
#include "catalog/pg_type.h"
#include "utils/lsyscache.h"

#include "catalog.h"
#include "pg_compat.h"
#include "shippable.h"
#include "repartition.h"

typedef struct ShipCtx
{
	bool		allow_top_limit;	/* the closed query: the top level may carry the LIMIT that was sent with it (design 8) */
	bool		allow_order_aggs;	/* a partial aggregation: float and order-sensitive aggregates are combined (design 7.6, A3) */
	Index		varno;			/* clause mode: the only table; else 0 */
	bool		statement;		/* a whole statement, not one clause */
	Oid			shipped_relid;	/* statement mode: the table being written */
	Query	   *top;			/* statement mode: the statement itself */
	bool		merge_ok;		/* the top level may be a MERGE (design 7): its ON clause and actions are walked */
	int			colocation_id;	/* multi-relation mode (design 5, 11.7): every distributed table of
								 * this colocation group may be read, at the one source level the
								 * analysis of pushdown.c found; else 0 */
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

/* ------------------------------------------------------------------------
 * Objects of extensions (design 5.5): a list compiled into the library
 * ---------------------------------------------------------------------- */

typedef struct KsShippableExtension
{
	const char *name;
	const char *const *versions;	/* the installed versions that share one state format, NULL terminated */
} KsShippableExtension;

static const char *const sketch_versions[] = {"1.0", NULL};

static const KsShippableExtension shippable_extensions[] = {
	{"kingres_sketch", sketch_versions},
	{NULL, NULL}
};

/* kingres_shard.shippable_extensions: "all", "none" or a list of names; it narrows, it never adds */
char	   *kingres_shard_shippable_extensions = NULL;

/* Is the extension in the compiled-in list?  (the check of the setting) */
bool
kingres_shard_shippable_extension_known(const char *name)
{
	int			i;

	for (i = 0; shippable_extensions[i].name != NULL; i++)
		if (strcmp(shippable_extensions[i].name, name) == 0)
			return true;
	return false;
}

/*
 * The list of extensions that a marker and a probe carry is "name:version[,name:version...]", split at ',' and at ':'.  The format
 * is exact only if no name and no version has either character, so a name or a version is made of letters, digits, '_', '.' and '-'
 * and nothing else (review S13F-02).  The extensions of the compiled-in list have such names; an object of an extension whose
 * name or version has another character is not shippable (the statement then reads the rows, as for any object that is not on the
 * list), and a list that does not have the exact form is refused wherever it is split (kingres_shard_ext_list_check).
 */
bool
kingres_shard_ext_token_ok(const char *s, size_t len)
{
	size_t		i;

	if (len == 0)
		return false;
	for (i = 0; i < len; i++)
	{
		char		c = s[i];

		if (!((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-'))
			return false;
	}
	return true;
}

/* Is the list exactly "name:version" items joined by ','?  NULL (no extension) is. */
bool
kingres_shard_ext_list_valid(const char *list)
{
	const char *p = list;

	if (list == NULL)
		return true;
	for (;;)
	{
		const char *colon = strchr(p, ':');
		const char *comma = strchr(p, ',');
		const char *end = comma != NULL ? comma : p + strlen(p);

		if (colon == NULL || colon >= end)
			return false;
		if (!kingres_shard_ext_token_ok(p, (size_t) (colon - p)) ||
			!kingres_shard_ext_token_ok(colon + 1, (size_t) (end - colon - 1)))
			return false;
		if (comma == NULL)
			return true;
		p = comma + 1;
	}
}

/* The list of a marker or of a statement that is not of the exact form is refused before it is split. */
void
kingres_shard_ext_list_check(const char *list)
{
	if (!kingres_shard_ext_list_valid(list))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("the list of extensions of a shipped statement is not of the form name:version[,name:version...]"),
				 errdetail("Names and versions are made of letters, digits, '_', '.' and '-'; the list is \"%s\".", list),
				 errhint("This is not a change of the bucket map: repeating the statement does not help.")));
}

/* The statement modes that carry the extensions they use in their marker allow them; the others never do. */
static bool ship_allow_ext = false;
static List *ship_used_ext = NIL;	/* of char *: "name:version" of every extension the walk used */

static bool
ext_setting_allows(const char *name)
{
	const char *s = kingres_shard_shippable_extensions;
	size_t		len = strlen(name);

	if (s == NULL || strcmp(s, "all") == 0)
		return true;
	if (strcmp(s, "none") == 0)
		return false;
	while (*s != '\0')
	{
		const char *end;

		while (*s == ' ' || *s == ',')
			s++;
		end = s;
		while (*end != '\0' && *end != ',' && *end != ' ')
			end++;
		if ((size_t) (end - s) == len && strncmp(s, name, len) == 0)
			return true;
		s = end;
	}
	return false;
}

/* A function or aggregate of an extension: the code is the extension's library, fixed, immutable, no SET clause. */
static bool type_ok(Oid type);

static bool
ext_proc_ok(Oid funcid)
{
	HeapTuple	tup = SearchSysCache1(PROCOID, ObjectIdGetDatum(funcid));
	Form_pg_proc p;
	bool		ok;
	int			i;

	if (!HeapTupleIsValid(tup))
		return false;
	p = (Form_pg_proc) GETSTRUCT(tup);
	ok = (p->prolang == ClanguageId || p->prolang == INTERNALlanguageId) &&
		p->provolatile == PROVOLATILE_IMMUTABLE && !p->prosecdef &&
		heap_attisnull(tup, Anum_pg_proc_proconfig, NULL) && type_ok(p->prorettype);
	for (i = 0; ok && i < p->pronargs; i++)
		ok = type_ok(p->proargtypes.values[i]);
	ReleaseSysCache(tup);
	return ok;
}

/* The extension that an object belongs to, when it is in the list, in the setting and at a version the list accepts. */
static bool
ext_object_ok(Oid classid, Oid objid)
{
	Oid			extoid;
	char	   *extname;
	HeapTuple	etup;
	Datum		d;
	bool		isnull;
	char	   *version;
	const KsShippableExtension *e = NULL;
	bool		version_ok = false;
	int			i;

	if (!ship_allow_ext)
		return false;
	extoid = getExtensionOfObject(classid, objid);
	if (!OidIsValid(extoid))
		return false;
	extname = get_extension_name(extoid);
	if (extname == NULL)
		return false;
	for (i = 0; shippable_extensions[i].name != NULL; i++)
		if (strcmp(shippable_extensions[i].name, extname) == 0)
			e = &shippable_extensions[i];
	if (e == NULL || !ext_setting_allows(extname))
		return false;
	etup = SearchSysCache1(EXTENSIONOID, ObjectIdGetDatum(extoid));
	if (!HeapTupleIsValid(etup))
		return false;
	d = SysCacheGetAttr(EXTENSIONOID, etup, Anum_pg_extension_extversion, &isnull);
	version = isnull ? NULL : TextDatumGetCString(d);
	ReleaseSysCache(etup);
	if (version == NULL)
		return false;
	/* a name or a version that the format of the list cannot carry (S13F-02) */
	if (!kingres_shard_ext_token_ok(extname, strlen(extname)) || !kingres_shard_ext_token_ok(version, strlen(version)))
		return false;
	for (i = 0; e->versions[i] != NULL; i++)
		if (strcmp(e->versions[i], version) == 0)
			version_ok = true;
	if (!version_ok)
		return false;

	if (classid == ProcedureRelationId)
	{
		if (!ext_proc_ok(objid))
			return false;
	}
	else if (classid == TypeRelationId)
	{
		HeapTuple	tup = SearchSysCache1(TYPEOID, ObjectIdGetDatum(objid));
		Form_pg_type t;
		bool		ok;

		if (!HeapTupleIsValid(tup))
			return false;
		t = (Form_pg_type) GETSTRUCT(tup);
		/* a base type of the extension, whose four conversion functions are the extension's too */
		ok = t->typtype == TYPTYPE_BASE &&
			OidIsValid(t->typinput) && OidIsValid(t->typoutput) && OidIsValid(t->typsend) && OidIsValid(t->typreceive) &&
			getExtensionOfObject(ProcedureRelationId, t->typinput) == extoid &&
			getExtensionOfObject(ProcedureRelationId, t->typoutput) == extoid &&
			getExtensionOfObject(ProcedureRelationId, t->typsend) == extoid &&
			getExtensionOfObject(ProcedureRelationId, t->typreceive) == extoid;
		ReleaseSysCache(tup);
		if (!ok)
			return false;
	}
	else if (classid == OperatorRelationId)
	{
		if (!ext_proc_ok(get_opcode(objid)) ||
			getExtensionOfObject(ProcedureRelationId, get_opcode(objid)) != extoid)
			return false;
	}
	else
		return false;

	{
		char	   *used = psprintf("%s:%s", extname, version);
		ListCell   *lc;
		bool		have = false;

		foreach(lc, ship_used_ext)
			if (strcmp((char *) lfirst(lc), used) == 0)
				have = true;
		if (!have)
			ship_used_ext = lappend(ship_used_ext, used);
	}
	return true;
}

/*
 * For the analysis of pushdown.c: is this aggregate of an extension one the list allows?  (Its call is accepted
 * or refused by the walker as any other; this only lets the shape rule see it.)
 */
bool
kingres_shard_extension_aggregate_ok(Oid aggfnoid)
{
	bool		old = ship_allow_ext;
	List	   *old_used = ship_used_ext;
	bool		ok;

	/*
	 * The list of the last walk lives in the memory of that statement, which may be gone: this call comes from the shape
	 * analysis, before the walk of this statement starts and resets the list.  It must not read the old list (a freed list
	 * is a crash in a group that runs a second fragment of an extension in one session).  It starts its own and drops it.
	 */
	ship_allow_ext = true;
	ship_used_ext = NIL;
	ok = ext_object_ok(ProcedureRelationId, aggfnoid);
	ship_allow_ext = old;
	ship_used_ext = old_used;
	return ok;
}

/* "name:version,name:version" of what the last statement walk used, or NULL */
char *
kingres_shard_shippable_ext_used(void)
{
	StringInfoData buf;
	ListCell   *lc;

	if (ship_used_ext == NIL)
		return NULL;
	initStringInfo(&buf);
	foreach(lc, ship_used_ext)
	{
		if (buf.len > 0)
			appendStringInfoChar(&buf, ',');
		appendStringInfoString(&buf, (char *) lfirst(lc));
	}
	return buf.data;
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
	if (type < FirstGenbkiObjectId)
		return true;
	if (ship_allow_ext)
	{
		Oid			elem = get_element_type(type);

		if (OidIsValid(elem))
			return type_ok(elem);
		return ext_object_ok(TypeRelationId, type);
	}
	return false;
}

/*
 * A scalar type whose value is the OID of a catalog object (regclass, regproc, ..., and the types a later
 * major adds, regdatabase of 19): decided by the server's own data, not by a list.  Every such type is built in,
 * and its input function is named reg<something>in.
 */
static bool
is_reg_scalar(Oid type)
{
	HeapTuple	tup;
	bool		result = false;

	if (type >= FirstGenbkiObjectId)
		return false;
	tup = SearchSysCache1(TYPEOID, ObjectIdGetDatum(type));
	if (HeapTupleIsValid(tup))
	{
		Oid			infunc = ((Form_pg_type) GETSTRUCT(tup))->typinput;
		char	   *name = get_func_name(infunc);

		result = name != NULL && strncmp(name, "reg", 3) == 0 &&
			strlen(name) > 5 && strcmp(name + strlen(name) - 2, "in") == 0;
		ReleaseSysCache(tup);
	}
	return result;
}

/* Does the type, looked into through domains, arrays, ranges and the fields of a row, hold a reg* value? */
static bool
type_mentions_reg(Oid type, int depth)
{
	Oid			base;
	Oid			elem;
	Oid			relid;

	if (depth > 8)
		return true;
	base = getBaseType(type);
	if (is_reg_scalar(base))
		return true;
	elem = get_element_type(base);
	if (OidIsValid(elem))
		return type_mentions_reg(elem, depth + 1);
	if (type_is_range(base))
		return type_mentions_reg(get_range_subtype(base), depth + 1);
	if (type_is_multirange(base))
		return type_mentions_reg(get_multirange_range(base), depth + 1);
	relid = get_typ_typrelid(base);
	if (OidIsValid(relid))
	{
		Relation	rel = relation_open(relid, AccessShareLock);
		TupleDesc	desc = RelationGetDescr(rel);
		bool		found = false;
		int			i;

		for (i = 0; i < desc->natts && !found; i++)
			if (!TupleDescAttr(desc, i)->attisdropped)
				found = type_mentions_reg(TupleDescAttr(desc, i)->atttypid, depth + 1);
		relation_close(rel, AccessShareLock);
		return found;
	}
	return false;
}

/* The types whose value is the OID of a catalog object (a scalar of such a type). */
static bool
is_reg_type(Oid type)
{
	return is_reg_scalar(getBaseType(type));
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
	if (funcid >= FirstGenbkiObjectId)
		return ext_object_ok(ProcedureRelationId, funcid);
	return func_volatile(funcid) == PROVOLATILE_IMMUTABLE &&
		!libm_dependent(funcid);
}

static bool
op_ok(Oid opno)
{
	if (opno >= FirstGenbkiObjectId)
		return ext_object_ok(OperatorRelationId, opno);
	return func_ok(get_opcode(opno));
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
	 * Multi-relation mode: the tables of the colocation group are all read by the
	 * statement that each group runs on its own buckets.  Whether they are joined so
	 * that this is right is decided by the analysis of pushdown.c, not here; this walker
	 * only accepts the relation, and refuses what could read another table behind that
	 * analysis (a sub-select, a function, a CTE: see query_ok and ship_walker).
	 */
	if (ctx->colocation_id != 0)
		return dt != NULL && !dt->is_reference && dt->colocation_id == ctx->colocation_id;
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

	/* the sub-select that stands for a relation another node shipped (repartition.h): its shape is fixed and reads no table (false: nothing wrong) */
	if (q != ctx->top && kingres_shard_repart_shipped_shape(q, NULL))
		return false;
	if (q != ctx->top && q->commandType != CMD_SELECT)
		return fail(ctx, "it has a data-modifying statement inside a subquery or a WITH clause");
	if (q->commandType == CMD_UTILITY || (q->commandType == CMD_MERGE && !(ctx->merge_ok && q == ctx->top)))
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
	/* the one LIMIT of the closed-query text (count + offset, folded, checked by pushdown.c) is the top level's, and only in that mode */
	if ((q->limitCount != NULL || q->limitOffset != NULL) && !(ctx->allow_top_limit && q == ctx->top))
		return fail(ctx, "it has LIMIT or OFFSET, which picks rows by their order");
	if (q->hasDistinctOn)
		return fail(ctx, "it has DISTINCT ON, which picks an arbitrary row of each group");
	if (q->hasWindowFuncs)
		return fail(ctx, "it has a window function, whose result can depend on the order of the rows");

	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		rtindex++;

		/*
		 * Multi-relation mode reads the relations of the level directly: below the
		 * statement itself only plain tables, joins and the grouping entry; the one
		 * sub-select it accepts is the source of the statement (the statement's own
		 * level), which pushdown.c has found to be the one unit.  A function, a
		 * VALUES list, a CTE or a deeper sub-select could read a table of the
		 * group without the key the analysis relies on.
		 */
		if (ctx->colocation_id != 0 && rte->rtekind != RTE_RELATION && rte->rtekind != RTE_JOIN &&
			rte->rtekind != RTE_GROUP && !(rte->rtekind == RTE_SUBQUERY && q == ctx->top))
			return fail(ctx, "it reads something other than the tables of the colocation group (a sub-select, a function, a VALUES list or a CTE)");

		switch (rte->rtekind)
		{
			case RTE_RELATION:
				/* the EXCLUDED pseudo relation of ON CONFLICT DO UPDATE holds the row being inserted */
				if (q == ctx->top && q->onConflict != NULL &&
					rtindex == q->onConflict->exclRelIndex)
					break;
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
				/*
				 * ON CONFLICT DO UPDATE has a whole-row entry of EXCLUDED in its target list (the
				 * parser adds it, nothing refers to it unless the statement does, and a use of
				 * it is a function or a field selection that the walker judges on its own).
				 */
				if (ctx->colocation_id != 0 && v->varattno == 0 &&
					!(ctx->top != NULL && ctx->top->onConflict != NULL &&
					  v->varlevelsup == 0 && v->varno == (Index) ctx->top->onConflict->exclRelIndex))
					return fail(ctx, "it uses a whole-row reference, whose text form carries the types of every column");
				if (ctx->colocation_id != 0 && v->varattno == 0)
					return false;
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
				/*
				 * Rule 6 of 5.1: the OID of a table, function, type, role or schema that is not
				 * built in names another object (or none) on another node.  'my_table'::regclass is
				 * folded into such a constant at parse time.
				 */
				if (!c->constisnull && is_reg_type(c->consttype) &&
					DatumGetObjectId(c->constvalue) >= FirstGenbkiObjectId)
					return fail(ctx, "it uses the OID of an object that is not built in, which names another object on another node");
				/* an array, a range or a row that holds reg* values: each group would resolve the names itself (review P2D-01) */
				if (!c->constisnull && !is_reg_type(c->consttype) && type_mentions_reg(c->consttype, 0))
					return fail(ctx, "it uses a constant that holds the OID of an object, which names another object on another node");
				return false;
			}

		case T_Param:
			{
				Param	   *p = (Param *) node;

				if (p->paramkind != PARAM_EXTERN)
					return fail(ctx, "it uses a parameter that is computed during execution");
				if (!type_ok(p->paramtype) || !collation_ok(p->paramcollid))
					return fail(ctx, "it uses a parameter of a type or collation that is not built in");
				if (type_mentions_reg(p->paramtype, 0))
					return fail(ctx, "it uses a parameter that holds the OID of an object, which names another object on another node");
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

		case T_CollateExpr:
			{
				/*
				 * The deparser writes the collation of a constant of type name (and of an
				 * explicit COLLATE) as COLLATE, which the receiving group reads back as this
				 * node: the origin and the group decide on the same tree (review P2B-02).  Only
				 * the collations that mean the same on every node pass.
				 */
				CollateExpr *ce = (CollateExpr *) node;

				if (!collation_ok(ce->collOid))
					return fail(ctx, "it uses a collation that is not built in");
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
				if (!ctx->allow_order_aggs && agg_order_sensitive(a))
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

		case T_MergeAction:
			if (!ctx->statement || !ctx->merge_ok)
				return fail(ctx, "it has the structure of a whole statement");
			break;

		case T_MergeSupportFunc:
			{
				MergeSupportFunc *mf = (MergeSupportFunc *) node;

				if (!ctx->statement || !ctx->merge_ok)
					return fail(ctx, "it has the structure of a whole statement");
				if (!type_ok(mf->msftype) || !collation_ok(mf->msfcollid))
					return fail(ctx, "it uses a type or collation that is not built in");
				break;
			}

		case T_OnConflictExpr:
			if (!ctx->statement)
				return fail(ctx, "it has the structure of a whole statement");
			if (!ks_onconflict_action_shippable(((OnConflictExpr *) node)->action))
				return fail(ctx, psprintf("its ON CONFLICT action (%d) is not one that can be shipped", (int) ((OnConflictExpr *) node)->action));
			/*
			 * Multi-relation mode: the description of the EXCLUDED relation (a Var for every column of
			 * the target, a column of a type that is not shippable included) says nothing the
			 * statement uses; what it does use (the SET list, the WHERE and the inference clause) is
			 * walked on its own.
			 */
			if (ctx->colocation_id != 0)
			{
				OnConflictExpr *oc = (OnConflictExpr *) node;

				return ship_walker((Node *) oc->arbiterElems, ctx) ||
					ship_walker(oc->arbiterWhere, ctx) ||
					ship_walker((Node *) oc->onConflictSet, ctx) ||
					ship_walker(oc->onConflictWhere, ctx);
			}
			break;

		case T_SubLink:
			/* a sub-select reads tables without the key that ties it to a group (multi-relation mode) */
			if (ctx->colocation_id != 0)
				return fail(ctx, "it has a sub-select in an expression");
			if (!ctx->statement)
				return fail(ctx, "it has the structure of a whole statement");
			break;

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

	ship_allow_ext = false;
	ship_used_ext = NIL;
	memset(&ctx, 0, sizeof(ctx));
	ctx.varno = varno;
	return !ship_walker(clause, &ctx);
}

bool
kingres_shard_stmt_shippable(Query *query, Oid shipped_relid, const char **reason)
{
	ShipCtx		ctx;
	bool		bad;

	ship_allow_ext = false;
	ship_used_ext = NIL;
	memset(&ctx, 0, sizeof(ctx));
	ctx.statement = true;
	ctx.shipped_relid = shipped_relid;
	ctx.top = query;
	bad = query_ok(query, &ctx);
	if (bad && reason != NULL)
		*reason = ctx.reason ? ctx.reason : "it cannot be shipped";
	return !bad;
}

/*
 * The multi-relation mode (design 5 and 17, step 5): an INSERT whose source reads
 * several distributed tables of one colocation group (and reference tables).
 */
bool
kingres_shard_stmt_shippable_colocated(Query *query, Oid target_relid, int colocation_id,
									   const char **reason)
{
	ShipCtx		ctx;
	bool		bad;

	Assert(colocation_id != 0);
	ship_allow_ext = false;
	ship_used_ext = NIL;
	memset(&ctx, 0, sizeof(ctx));
	ctx.statement = true;
	ctx.shipped_relid = target_relid;
	ctx.colocation_id = colocation_id;
	ctx.top = query;
	bad = query_ok(query, &ctx);
	if (bad && reason != NULL)
		*reason = ctx.reason ? ctx.reason : "it cannot be shipped";
	return !bad;
}

/*
 * A MERGE of a distributed table (design 7): the statement a group runs, whose relations are the
 * target, the distributed tables of the colocation group, reference tables and the sub-select of
 * a shipped source.  Whether the tables are joined so that this is right is the analysis of
 * merge.c; this walker judges the expressions (the ON clause, the conditions, SET, VALUES, RETURNING).
 */
bool
kingres_shard_stmt_shippable_merge(Query *query, int colocation_id, const char **reason)
{
	ShipCtx		ctx;
	bool		bad;

	Assert(colocation_id != 0);
	ship_allow_ext = false;
	ship_used_ext = NIL;
	memset(&ctx, 0, sizeof(ctx));
	ctx.statement = true;
	ctx.merge_ok = true;
	ctx.shipped_relid = InvalidOid;
	ctx.colocation_id = colocation_id;
	ctx.top = query;
	bad = query_ok(query, &ctx);
	if (bad && reason != NULL)
		*reason = ctx.reason ? ctx.reason : "it cannot be shipped";
	return !bad;
}

/*
 * A MERGE into a reference table (design 7.5, M4 and M5): the rule of a write that runs on every node (the target
 * is the one table written, the other relations are reference tables or the sub-select of a shipped source)
 * applied to the MERGE node kinds.
 */
bool
kingres_shard_stmt_shippable_merge_ref(Query *query, Oid target_relid, const char **reason)
{
	ShipCtx		ctx;
	bool		bad;

	ship_allow_ext = false;
	ship_used_ext = NIL;
	memset(&ctx, 0, sizeof(ctx));
	ctx.statement = true;
	ctx.merge_ok = true;
	ctx.shipped_relid = target_relid;
	ctx.top = query;
	bad = query_ok(query, &ctx);
	if (bad && reason != NULL)
		*reason = ctx.reason ? ctx.reason : "it cannot be shipped";
	return !bad;
}

/*
 * The fragment of a partial aggregation (design 7): the same tests as the colocated mode, except that
 * an aggregate whose result depends on the order or the grouping of the rows (a floating point sum,
 * array_agg, string_agg) is accepted: the partial states are combined in the order the groups
 * answer, as in a parallel plan, and the README says so (design 7.6, A3).
 */
bool
kingres_shard_stmt_shippable_partial(Query *query, int colocation_id, const char **reason)
{
	ShipCtx		ctx;
	bool		bad;

	Assert(colocation_id != 0);
	ship_allow_ext = true;
	ship_used_ext = NIL;
	memset(&ctx, 0, sizeof(ctx));
	ctx.statement = true;
	ctx.shipped_relid = InvalidOid;
	ctx.colocation_id = colocation_id;
	ctx.top = query;
	ctx.allow_order_aggs = true;
	bad = query_ok(query, &ctx);
	if (bad && reason != NULL)
		*reason = ctx.reason ? ctx.reason : "it cannot be shipped";
	return !bad;
}

/*
 * The fragment of a closed query that every group runs whole: the colocated mode, and the objects of the extensions
 * of the list are allowed (they are named in the marker, and the group compares them with its own catalog).
 */
bool
kingres_shard_stmt_shippable_query(Query *query, int colocation_id, const char **reason)
{
	ShipCtx		ctx;
	bool		bad;

	Assert(colocation_id != 0);
	ship_allow_ext = true;
	ship_used_ext = NIL;
	memset(&ctx, 0, sizeof(ctx));
	ctx.statement = true;
	ctx.shipped_relid = InvalidOid;
	ctx.colocation_id = colocation_id;
	ctx.top = query;
	ctx.allow_top_limit = true;
	bad = query_ok(query, &ctx);
	if (bad && reason != NULL)
		*reason = ctx.reason ? ctx.reason : "it cannot be shipped";
	return !bad;
}

/* The installed version of an extension of this database, or NULL when it is not installed. */
char *
kingres_shard_extension_version(const char *name)
{
	Oid			extoid = get_extension_oid(name, true);
	HeapTuple	etup;
	Datum		d;
	bool		isnull;
	char	   *version = NULL;

	if (!OidIsValid(extoid))
		return NULL;
	etup = SearchSysCache1(EXTENSIONOID, ObjectIdGetDatum(extoid));
	if (!HeapTupleIsValid(etup))
		return NULL;
	d = SysCacheGetAttr(EXTENSIONOID, etup, Anum_pg_extension_extversion, &isnull);
	if (!isnull)
		version = TextDatumGetCString(d);
	ReleaseSysCache(etup);
	return version;
}
