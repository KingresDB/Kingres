/*-------------------------------------------------------------------------
 *
 * pushdown.c
 *	  The analysis of a pushdown unit, and the decision of the colocated
 *	  INSERT ... SELECT (design/shard-phase2b.md, sections 6, 9.1 and 11.7).
 *
 * What a unit is, in one paragraph.  Every shard group holds a part of each
 * distributed table of a colocation group, chosen by the hash of the
 * distribution key, and a full copy of each reference table.  A SELECT level is
 * a unit when running it on each group over the group's own buckets and adding
 * up the answers gives the answer of the whole: each row of its join is formed
 * from rows of one group (the distributed tables are joined on their
 * distribution keys, 6.3, and an outer join keeps the preserved side distributed,
 * 9.1), and what is computed from the rows (aggregation, DISTINCT) does not mix
 * rows of two groups (the level is closed, 6.4).
 *
 * Nothing here knows what encloses the level, so a later step that ships a SELECT
 * (aggregates, joins, an ordered merge) calls the same three functions:
 * kingres_shard_pushdown_analyze(), _is_key_var() and _closed().
 *
 * The decision of an INSERT ... SELECT (kingres_shard_insert_select_analyze) is
 * made on the rewritten statement, by the origin and by a receiving group alike:
 * the origin on its own tree, and once more on the parse of the text it
 * deparsed (planner.c), the group on the parse of the text it received.  The
 * function must give the same answer on both, which is why it looks only at what
 * a parse of the text restores, and why a default of a column the statement does
 * not name is looked at the same way on both sides (11.2): the group applies its
 * own default for a column that is left out of the column list, so the origin
 * leaves out of the text exactly the columns whose default is the same
 * expression everywhere (constant and immutable), and every other default is a
 * reason to use the row router, which computes it once, at the origin.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/pushdown.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/genam.h"
#include "access/htup_details.h"
#include "access/table.h"
#include "catalog/pg_aggregate.h"
#include "catalog/pg_class.h"
#include "catalog/pg_index.h"
#include "catalog/pg_type_d.h"
#include "miscadmin.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "optimizer/optimizer.h"
#include "optimizer/tlist.h"
#include "parser/parse_relation.h"
#include "parser/parsetree.h"
#include "rewrite/rewriteManip.h"
#include "utils/acl.h"
#include "utils/lsyscache.h"
#include "utils/rel.h"
#include "utils/relcache.h"
#include "utils/syscache.h"
#include "utils/typcache.h"

#include "catalog.h"
#include "pg_compat.h"
#include "pushdown.h"
#include "shippable.h"

/* ======================================================================
 * Small helpers
 * ====================================================================== */

static Node *
strip_relabel(Node *n)
{
	while (n != NULL && IsA(n, RelabelType))
		n = (Node *) ((RelabelType *) n)->arg;
	return n;
}

/* The expression with the references to grouping columns and join aliases replaced by what they are. */
static Node *
resolve_expr(Query *q, Node *e)
{
	if (e == NULL)
		return NULL;
	e = flatten_group_exprs(NULL, q, e);
	return flatten_join_alias_vars(NULL, q, e);
}

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

static bool
relation_has_rls(Oid relid)
{
	HeapTuple	tup = SearchSysCache1(RELOID, ObjectIdGetDatum(relid));
	bool		rls;

	if (!HeapTupleIsValid(tup))
		return true;
	rls = ((Form_pg_class) GETSTRUCT(tup))->relrowsecurity;
	ReleaseSysCache(tup);
	return rls;
}

/* ======================================================================
 * Eligibility and the key class
 * ====================================================================== */

typedef struct PdCtx
{
	Query	   *q;
	KsPushdown *pd;
	const char *reason;
	Oid			hash_opf;		/* the hash operator family of the distribution type */
	KsPdRel   **by_rti;			/* the relation of each range table index, or NULL */
	int			nrtes;
	int		   *uf;				/* union-find over range table indexes (key variables) */
} PdCtx;

static bool
pd_fail(PdCtx *c, const char *reason)
{
	if (c->reason == NULL)
		c->reason = reason;
	return false;
}

static int
uf_find(PdCtx *c, int x)
{
	while (c->uf[x] != x)
	{
		c->uf[x] = c->uf[c->uf[x]];
		x = c->uf[x];
	}
	return x;
}

static void
uf_union(PdCtx *c, int a, int b)
{
	a = uf_find(c, a);
	b = uf_find(c, b);
	if (a != b)
		c->uf[a] = b;
}

/*
 * Is the conjunct an equality of two key variables of the colocation group (6.3)?  The
 * operator is an equality operator of the hash operator family of the group's type, the
 * collation of the comparison is the group's and deterministic, and both sides are bare
 * variables (a relabeling is looked through) of the distribution columns.
 */
static bool
key_equality(PdCtx *c, Node *clause, int *ra, int *rb)
{
	OpExpr	   *op;
	Node	   *l;
	Node	   *r;
	Var		   *vl;
	Var		   *vr;

	if (clause == NULL || !IsA(clause, OpExpr))
		return false;
	op = (OpExpr *) clause;
	if (list_length(op->args) != 2 || !OidIsValid(c->hash_opf) ||
		!op_in_opfamily(op->opno, c->hash_opf))
		return false;
	if (op->inputcollid != c->pd->coloc->collation ||
		(OidIsValid(op->inputcollid) && !get_collation_isdeterministic(op->inputcollid)))
		return false;
	l = strip_relabel(resolve_expr(c->q, (Node *) linitial(op->args)));
	r = strip_relabel(resolve_expr(c->q, (Node *) lsecond(op->args)));
	if (l == NULL || r == NULL || !IsA(l, Var) || !IsA(r, Var))
		return false;
	vl = (Var *) l;
	vr = (Var *) r;
	if (vl->varlevelsup != 0 || vr->varlevelsup != 0 ||
		vl->varno >= (Index) c->nrtes || vr->varno >= (Index) c->nrtes ||
		c->by_rti[vl->varno] == NULL || c->by_rti[vr->varno] == NULL)
		return false;
	if (c->by_rti[vl->varno]->reference || c->by_rti[vr->varno]->reference ||
		c->by_rti[vl->varno]->dist_attno != vl->varattno ||
		c->by_rti[vr->varno]->dist_attno != vr->varattno)
		return false;
	*ra = (int) vl->varno;
	*rb = (int) vr->varno;
	return true;
}

/* The relations of a subtree, and how many of them are distributed. */
static int
count_dist(PdCtx *c, Bitmapset *set)
{
	int			n = 0;
	int			x = -1;

	while ((x = bms_next_member(set, x)) >= 0)
		if (c->by_rti[x] != NULL && !c->by_rti[x]->reference)
			n++;
	return n;
}

/*
 * Walk the join tree of the level.  Returns the set of the relations under the node.  The
 * conjuncts of a WHERE clause and of the ON clause of an inner join unite the key variables
 * they equate; the ON clause of an outer join only those of its two arms (6.3), and an outer
 * join must keep the preserved side distributed (9.1, J2).  The relations on the nullable
 * side of an outer join are marked, so that a key variable of theirs is not taken for the
 * key of an output row.
 */
static Bitmapset *
pd_walk(PdCtx *c, Node *n)
{
	if (n == NULL || c->reason != NULL)
		return NULL;

	if (IsA(n, RangeTblRef))
	{
		int			rti = ((RangeTblRef *) n)->rtindex;

		if (rti <= 0 || rti >= c->nrtes || c->by_rti[rti] == NULL)
		{
			pd_fail(c, "it reads something other than a table");
			return NULL;
		}
		return bms_make_singleton(rti);
	}
	if (IsA(n, FromExpr))
	{
		FromExpr   *f = (FromExpr *) n;
		Bitmapset  *set = NULL;
		ListCell   *lc;
		List	   *conj = NIL;

		foreach(lc, f->fromlist)
			set = bms_union(set, pd_walk(c, (Node *) lfirst(lc)));
		flatten_and(f->quals, &conj);
		foreach(lc, conj)
		{
			int			a;
			int			b;

			if (key_equality(c, (Node *) lfirst(lc), &a, &b))
				uf_union(c, a, b);
		}
		return set;
	}
	if (IsA(n, JoinExpr))
	{
		JoinExpr   *j = (JoinExpr *) n;
		Bitmapset  *lset = pd_walk(c, j->larg);
		Bitmapset  *rset = pd_walk(c, j->rarg);
		int			ld = count_dist(c, lset);
		int			rd = count_dist(c, rset);
		List	   *conj = NIL;
		ListCell   *lc;
		bool		cross = false;

		if (c->reason != NULL)
			return NULL;
		flatten_and(j->quals, &conj);

		if (j->jointype == JOIN_INNER)
		{
			foreach(lc, conj)
			{
				int			a;
				int			b;

				if (key_equality(c, (Node *) lfirst(lc), &a, &b))
					uf_union(c, a, b);
			}
			return bms_union(lset, rset);
		}
		if (j->jointype != JOIN_LEFT && j->jointype != JOIN_RIGHT && j->jointype != JOIN_FULL)
		{
			pd_fail(c, "it has a join that is neither inner nor outer");
			return NULL;
		}

		/* The equalities of the ON clause that join the two arms (6.3). */
		foreach(lc, conj)
		{
			int			a;
			int			b;

			if (key_equality(c, (Node *) lfirst(lc), &a, &b) &&
				((bms_is_member(a, lset) && bms_is_member(b, rset)) ||
				 (bms_is_member(a, rset) && bms_is_member(b, lset))))
			{
				uf_union(c, a, b);
				cross = true;
			}
		}

		if (j->jointype == JOIN_FULL)
		{
			/* both arms distributed with a key equality, or both replicated (J5) */
			if ((ld > 0) != (rd > 0))
			{
				pd_fail(c, "it has a FULL JOIN between a distributed and a replicated side");
				return NULL;
			}
			if (ld > 0 && !cross)
			{
				pd_fail(c, "a FULL JOIN of distributed tables does not equate their distribution keys in its ON clause");
				return NULL;
			}
		}
		else
		{
			int			pd_ = (j->jointype == JOIN_LEFT) ? ld : rd;	/* the preserved side */
			int			nd_ = (j->jointype == JOIN_LEFT) ? rd : ld;	/* the nullable side */

			if (pd_ == 0 && nd_ > 0)
			{
				pd_fail(c, "the preserved side of an outer join is replicated (every group would emit its unmatched rows)");
				return NULL;
			}
			if (pd_ > 0 && nd_ > 0 && !cross)
			{
				pd_fail(c, "an outer join of distributed tables does not equate their distribution keys in its ON clause");
				return NULL;
			}
		}

		/* the nullable arms */
		{
			Bitmapset  *nullable = NULL;
			int			x = -1;

			if (j->jointype == JOIN_LEFT || j->jointype == JOIN_FULL)
				nullable = bms_union(nullable, rset);
			if (j->jointype == JOIN_RIGHT || j->jointype == JOIN_FULL)
				nullable = bms_union(nullable, lset);
			while ((x = bms_next_member(nullable, x)) >= 0)
				c->by_rti[x]->nullable = true;
		}
		return bms_union(lset, rset);
	}

	pd_fail(c, "it has a join tree of a kind that is not analysed");
	return NULL;
}

bool
kingres_shard_pushdown_analyze(Query *level, KsPushdown *pd, const char **reason)
{
	PdCtx		c;
	ListCell   *lc;
	Index		rti = 0;
	int			nrtes = list_length(level->rtable) + 1;
	int			i;
	TypeCacheEntry *tce;

	memset(pd, 0, sizeof(*pd));
	pd->query = level;
	memset(&c, 0, sizeof(c));
	c.q = level;
	c.pd = pd;
	c.nrtes = nrtes;
	c.by_rti = palloc0(sizeof(KsPdRel *) * nrtes);
	c.uf = palloc(sizeof(int) * nrtes);
	for (i = 0; i < nrtes; i++)
		c.uf[i] = i;

#define PD_NO(why) do { *reason = (why); return false; } while (0)

	/* 6.1 rule 2 */
	if (level->commandType != CMD_SELECT)
		PD_NO("it is not a SELECT");
	if (level->cteList != NIL || level->hasRecursive || level->hasModifyingCTE)
		PD_NO("it has a WITH clause");
	if (level->setOperations != NULL)
		PD_NO("it has a set operation");
	if (level->hasSubLinks)
		PD_NO("it has a sub-select in an expression");
	if (level->rowMarks != NIL)
		PD_NO("it locks rows");
	if (level->hasTargetSRFs)
		PD_NO("it has a set-returning function in its select list");
	if (level->groupingSets != NIL)
		PD_NO("it has grouping sets");

	foreach(lc, level->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);
		const KsDistTable *dt;
		KsPdRel    *r;

		rti++;
		switch (rte->rtekind)
		{
			case RTE_JOIN:
			case RTE_GROUP:
				continue;
			case RTE_RELATION:
				break;
			default:
				PD_NO("it reads something other than a table (a sub-select, a view, a function, a VALUES list or a CTE)");
		}
		if (rte->tablesample != NULL)
			PD_NO("it samples a table at random");
		if (rte->relkind != RELKIND_RELATION && rte->relkind != RELKIND_PARTITIONED_TABLE)
			PD_NO("it reads a relation that is not an ordinary or partitioned table");
		/* 6.1 rule 6 and 5.3: row-level security, and rights that are not the session role's */
		if (rte->securityQuals != NIL || relation_has_rls(rte->relid))
			PD_NO("a table has row-level security");
		if (rte->perminfoindex > 0)
		{
			Oid			owner = getRTEPermissionInfo(level->rteperminfos, rte)->checkAsUser;

			if (OidIsValid(owner) && owner != GetUserId())
				PD_NO("a table is read with the rights of another role (a view of another owner)");
		}
		dt = kingres_shard_lookup_dist_table(rte->relid);
		if (dt == NULL)
			PD_NO("it reads a local table, which the other shard groups do not have");

		r = palloc0(sizeof(KsPdRel));
		r->rti = rti;
		r->relid = rte->relid;
		r->reference = dt->is_reference;
		r->dist_attno = dt->is_reference ? InvalidAttrNumber : dt->dist_attno;
		r->keyclass = -1;
		if (!dt->is_reference)
		{
			if (pd->ndist == 0)
			{
				pd->colocation_id = dt->colocation_id;
				pd->coloc = kingres_shard_coloc_copy(dt->coloc);
			}
			else if (pd->colocation_id != dt->colocation_id)
				PD_NO("it reads distributed tables of more than one colocation group");
			pd->ndist++;
		}
		pd->rels = lappend(pd->rels, r);
		c.by_rti[rti] = r;
	}
	if (pd->ndist == 0)
		PD_NO("it reads no distributed table");

	tce = lookup_type_cache(pd->coloc->dist_type, TYPECACHE_HASH_OPFAMILY);
	c.hash_opf = tce->hash_opf;

	/* 6.1 rules 4 and 5, 6.3, 9.1 */
	(void) pd_walk(&c, (Node *) level->jointree);
	if (c.reason != NULL)
		PD_NO(c.reason);

	{
		int		   *roots = palloc(sizeof(int) * nrtes);
		int			nroots = 0;

		foreach(lc, pd->rels)
		{
			KsPdRel    *r = (KsPdRel *) lfirst(lc);
			int			root;
			int			k;

			if (r->reference)
				continue;
			root = uf_find(&c, (int) r->rti);
			for (k = 0; k < nroots; k++)
				if (roots[k] == root)
					break;
			if (k == nroots)
				roots[nroots++] = root;
			r->keyclass = k;
		}
		pd->nclasses = nroots;
		if (nroots != 1)
			PD_NO("the distributed tables are not all joined on their distribution keys (more than one key class)");
	}
	return true;
#undef PD_NO
}

Index
kingres_shard_pushdown_key_rti(const KsPushdown *pd, Node *expr)
{
	Node	   *e;
	Var		   *v;
	ListCell   *lc;

	if (expr == NULL)
		return 0;
	e = strip_relabel(resolve_expr(pd->query, expr));
	if (e == NULL || !IsA(e, Var))
		return 0;
	v = (Var *) e;
	if (v->varlevelsup != 0)
		return 0;
	foreach(lc, pd->rels)
	{
		KsPdRel    *r = (KsPdRel *) lfirst(lc);

		if (r->rti == v->varno)
			return (!r->reference && !r->nullable && r->dist_attno == v->varattno) ? r->rti : 0;
	}
	return 0;
}

bool
kingres_shard_pushdown_is_key_var(const KsPushdown *pd, Node *expr)
{
	return kingres_shard_pushdown_key_rti(pd, expr) != 0;
}

/* Does the list of grouping or partitioning clauses have a key variable of the class? */
static bool
clauses_have_key(const KsPushdown *pd, List *clauses)
{
	ListCell   *lc;

	foreach(lc, clauses)
	{
		SortGroupClause *sgc = (SortGroupClause *) lfirst(lc);
		TargetEntry *tle = get_sortgroupclause_tle(sgc, pd->query->targetList);

		if (kingres_shard_pushdown_is_key_var(pd, (Node *) tle->expr))
			return true;
	}
	return false;
}

bool
kingres_shard_pushdown_closed(const KsPushdown *pd, const char **reason)
{
	Query	   *q = pd->query;
	ListCell   *lc;

	if (q->groupingSets != NIL)
	{
		*reason = "it has grouping sets";
		return false;
	}
	if (q->hasAggs && q->groupClause == NIL)
	{
		*reason = "it has an aggregate without GROUP BY, which adds up rows of every group";
		return false;
	}
	if (q->groupClause != NIL)
	{
		bool		found = false;

		foreach(lc, q->rtable)
		{
			RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);
			ListCell   *lc2;

			if (rte->rtekind != RTE_GROUP)
				continue;
			foreach(lc2, rte->groupexprs)
				if (kingres_shard_pushdown_is_key_var(pd, (Node *) lfirst(lc2)))
					found = true;
		}
		if (!found)
		{
			*reason = "its grouping does not include a distribution key (a group would span shard groups)";
			return false;
		}
	}
	if (q->distinctClause != NIL && !clauses_have_key(pd, q->distinctClause))
	{
		*reason = "its DISTINCT does not cover a distribution key";
		return false;
	}
	foreach(lc, q->windowClause)
	{
		WindowClause *wc = (WindowClause *) lfirst(lc);

		if (!clauses_have_key(pd, wc->partitionClause))
		{
			*reason = "a window is not partitioned by a distribution key";
			return false;
		}
	}
	return true;
}

List *
kingres_shard_pushdown_relids(const KsPushdown *pd)
{
	List	   *result = NIL;
	ListCell   *lc;

	foreach(lc, pd->rels)
		result = list_append_unique_oid(result, ((KsPdRel *) lfirst(lc))->relid);
	return result;
}

/* ======================================================================
 * The marker
 * ====================================================================== */

char *
kingres_shard_insert_select_marker(int64 place)
{
	return psprintf(KS_INSERT_SELECT_MARKER_PREFIX "place=" INT64_FORMAT " */ ", place);
}

bool
kingres_shard_parse_insert_select_marker(const char *query_string, int64 *place)
{
	const char *p;
	char	   *end;

	if (query_string == NULL ||
		strncmp(query_string, KS_INSERT_SELECT_MARKER_PREFIX, strlen(KS_INSERT_SELECT_MARKER_PREFIX)) != 0)
		return false;
	p = query_string + strlen(KS_INSERT_SELECT_MARKER_PREFIX);
	if (strncmp(p, "place=", 6) != 0)
		return false;
	p += 6;
	*place = strtoll(p, &end, 10);
	if (end == p)
		return false;
	return strncmp(end, " */ ", 4) == 0;
}

/* ======================================================================
 * INSERT ... SELECT (P6)
 * ====================================================================== */

/*
 * Every unique index of the table (and every exclusion constraint) has the distribution
 * column among its key columns, compared under the collation the table was distributed
 * with: two rows that conflict then have equal keys, hence the same bucket and the same
 * group (11.7, condition 1).  The utility hook refuses any other index, so this only meets
 * a table that a superuser changed by hand with routing off.
 */
static bool
unique_indexes_carry_key(Oid relid, AttrNumber dist_attno, Oid collation)
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

/* A trigger of a user, or a rule, on the target would run on the group, in the group's session. */
static bool
target_has_user_code(Oid relid)
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

/* Is every Var of the expression of the target, or EXCLUDED (ON CONFLICT DO UPDATE)? */
static bool
vars_of_target_only(Node *e, Index target, Index excluded)
{
	List	   *vars = pull_var_clause(e, 0);
	ListCell   *lc;

	foreach(lc, vars)
	{
		Var		   *v = (Var *) lfirst(lc);

		if (v->varlevelsup != 0 || (v->varno != target && v->varno != excluded))
			return false;
	}
	return true;
}

bool
kingres_shard_insert_select_analyze(Query *insert, KsInsertSelect *is, const char **reason)
{
	Query	   *q;
	RangeTblEntry *trte;
	RangeTblEntry *srte = NULL;
	RTEPermissionInfo *tperm;
	const KsDistTable *tdt;
	Index		srcrti = 0;
	Index		rti = 0;
	Index		excl;
	ListCell   *lc;
	List	   *newtlist = NIL;
	AttrNumber	next = 1;
	AttrNumber	dist_attno;
	int			coloc_id;
	Oid			coloc_collation;
	Oid			target_relid;
	const char *why = NULL;
	TargetEntry *keyte = NULL;
	bool		varfree_named = false;
	OnConflictExpr *oc;

#define IS_NO(r) do { *reason = (r); return false; } while (0)

	memset(is, 0, sizeof(*is));
	if (insert->commandType != CMD_INSERT || insert->resultRelation <= 0)
		IS_NO("it is not an INSERT");
	if (insert->cteList != NIL || insert->hasModifyingCTE)
		IS_NO("it has a WITH clause");
	if (insert->hasSubLinks)
		IS_NO("it has a sub-select in an expression");
	if (insert->rowMarks != NIL)
		IS_NO("it locks rows");
	if (insert->withCheckOptions != NIL)
		IS_NO("the rows are checked against a view or a row-level security policy");

	q = copyObject(insert);
	trte = rt_fetch(q->resultRelation, q->rtable);
	if (trte->rtekind != RTE_RELATION)
		IS_NO("its target is not a table");
	target_relid = trte->relid;
	if (trte->relkind != RELKIND_PARTITIONED_TABLE)
		IS_NO("its target is not a partitioned (distributed) table");
	if (trte->securityQuals != NIL || relation_has_rls(target_relid))
		IS_NO("the target has row-level security");
	tperm = getRTEPermissionInfo(q->rteperminfos, trte);
	if (OidIsValid(tperm->checkAsUser) && tperm->checkAsUser != GetUserId())
		IS_NO("the target is written with the rights of another role (a view of another owner)");

	tdt = kingres_shard_lookup_dist_table(target_relid);
	if (tdt == NULL || tdt->is_reference)
		IS_NO("its target is not a distributed table");
	dist_attno = tdt->dist_attno;
	coloc_id = tdt->colocation_id;
	coloc_collation = tdt->coloc->collation;

	/* The source: the one sub-select the statement reads, and nothing else (the EXCLUDED relation apart). */
	excl = (q->onConflict != NULL) ? (Index) q->onConflict->exclRelIndex : 0;
	foreach(lc, q->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		rti++;
		if (rti == (Index) q->resultRelation || rti == excl)
			continue;
		if (rte->rtekind != RTE_SUBQUERY || srte != NULL)
			IS_NO("its source is not one SELECT (a VALUES list, a function or several relations)");
		srte = rte;
		srcrti = rti;
	}
	if (srte == NULL)
		IS_NO("it has no SELECT as its source");
	if (srte->security_barrier || srte->lateral)
		IS_NO("its source is a security barrier view");
	if (q->jointree == NULL || list_length(q->jointree->fromlist) != 1 ||
		!IsA(linitial(q->jointree->fromlist), RangeTblRef) ||
		((RangeTblRef *) linitial(q->jointree->fromlist))->rtindex != (int) srcrti ||
		q->jointree->quals != NULL)
		IS_NO("its source is not simply the SELECT");

	/* The source is a unit of the target's colocation group, and a closed one (11.7). */
	if (!kingres_shard_pushdown_analyze(srte->subquery, &is->pd, &why))
		IS_NO(why);
	if (is->pd.colocation_id != coloc_id)
		IS_NO("the source reads distributed tables of another colocation group than the target");
	if (!kingres_shard_pushdown_closed(&is->pd, &why))
		IS_NO(why);

	/*
	 * The columns the statement leaves out of its column list: the group applies its own
	 * default for them (11.2), which is the same everywhere only for an expression without
	 * a variable that is immutable and shippable.  A volatile default (nextval, random(),
	 * an identity column), a stable one (now()) and a user function are the router's:
	 * it computes them once per row at the origin.  What the INSERT's target list holds
	 * after this is the columns the source gives.
	 */
	foreach(lc, q->targetList)
	{
		TargetEntry *te = (TargetEntry *) lfirst(lc);
		List	   *vars;

		if (te->resjunk)
		{
			newtlist = lappend(newtlist, te);
			continue;
		}
		vars = pull_var_clause((Node *) te->expr, 0);
		if (vars == NIL)
		{
			if (bms_is_member(te->resno - FirstLowInvalidHeapAttributeNumber, tperm->insertedCols))
			{
				/*
				 * A column the statement names that takes no variable: a literal of the
				 * select list that has no type of its own (the parser puts it, coerced, into
				 * the INSERT's target list).  Position is checked below.
				 */
				varfree_named = true;
				if (te->resno == dist_attno)
					keyte = te;
				newtlist = lappend(newtlist, te);
				continue;
			}
			if (contain_volatile_functions((Node *) te->expr))
				IS_NO("a column that the statement does not name has a volatile default (a sequence, an identity column), which is computed once per row at the origin");
			if (!kingres_shard_clause_shippable((Node *) te->expr, 0))
				IS_NO("a column that the statement does not name has a default that is not a built-in immutable expression");
			continue;				/* the group applies this default itself */
		}
		if (list_length(vars) != 1 || ((Var *) linitial(vars))->varno != srcrti ||
			((Var *) linitial(vars))->varlevelsup != 0)
			IS_NO("a column is computed from something other than one column of the source");
		if (te->resno == dist_attno)
			keyte = te;
		newtlist = lappend(newtlist, te);
	}

	/*
	 * The deparser writes the column list from the target list and the source as it is, so the
	 * i-th column of the list must be the i-th column of the source.  The rewriter sorts the
	 * target list by column of the target: when the statement named its columns in another
	 * order the list is put back in the order of the source (the variables say where each
	 * came from); a literal of the select list is a column that no variable names, so with
	 * one the order must already be the source's.
	 */
	{
		int			n = 0;
		TargetEntry **by_pos;
		int			pos;

		foreach(lc, newtlist)
			if (!((TargetEntry *) lfirst(lc))->resjunk)
				n++;
		by_pos = palloc0(sizeof(TargetEntry *) * (n + 1));
		next = 1;
		foreach(lc, newtlist)
		{
			TargetEntry *te = (TargetEntry *) lfirst(lc);
			List	   *vars;

			if (te->resjunk)
				continue;
			vars = pull_var_clause((Node *) te->expr, 0);
			if (vars == NIL)
			{
				/* a literal without a type of its own, in the position of the next column of the source */
				ListCell   *lc2;
				TargetEntry *src_te = NULL;

				foreach(lc2, srte->subquery->targetList)
				{
					TargetEntry *t = (TargetEntry *) lfirst(lc2);

					if (!t->resjunk && t->resno == next)
						src_te = t;
				}
				if (src_te == NULL || !(IsA(src_te->expr, Const) || IsA(src_te->expr, Param)) ||
					exprType((Node *) src_te->expr) != UNKNOWNOID)
					IS_NO("a column is given a value that is not read from the source");
				pos = next;
			}
			else
			{
				pos = ((Var *) linitial(vars))->varattno;
				if (varfree_named && pos != next)
					IS_NO("the columns are named in another order than the source gives them, next to a literal");
			}
			if (pos < 1 || pos > n || by_pos[pos] != NULL)
				IS_NO("a column of the source is used twice or not at all");
			by_pos[pos] = te;
			next++;
		}
		newtlist = NIL;
		for (pos = 1; pos <= n; pos++)
			newtlist = lappend(newtlist, by_pos[pos]);
		foreach(lc, q->targetList)
			if (((TargetEntry *) lfirst(lc))->resjunk)
				newtlist = lappend(newtlist, lfirst(lc));
	}
	q->targetList = newtlist;

	/* The row's distribution column is a key of the source's class (6.3, 11.7), bare. */
	if (keyte == NULL)
		IS_NO("the distribution column of the target is not given by the source");
	{
		Node	   *e = strip_relabel((Node *) keyte->expr);
		Var		   *v;
		TargetEntry *src_te = NULL;
		ListCell   *lc2;

		if (e == NULL || !IsA(e, Var))
			IS_NO("the distribution column of the target is computed from the source and not the key itself");
		v = (Var *) e;
		foreach(lc2, srte->subquery->targetList)
		{
			TargetEntry *t = (TargetEntry *) lfirst(lc2);

			if (!t->resjunk && t->resno == v->varattno)
				src_te = t;
		}
		if (src_te == NULL || !kingres_shard_pushdown_is_key_var(&is->pd, (Node *) src_te->expr))
			IS_NO("the distribution column of the target is not a distribution key of the source (of a table that is not on the nullable side of an outer join)");
		if (exprType((Node *) src_te->expr) != is->pd.coloc->dist_type ||
			exprType((Node *) keyte->expr) != is->pd.coloc->dist_type)
			IS_NO("the distribution column of the target has another type than the key of the source");
	}

	/* The target: no code of the users, and unique indexes that cannot reach across groups. */
	if (target_has_user_code(target_relid))
		IS_NO("the target has a trigger or a rule, which would run on the shard groups");
	if (!unique_indexes_carry_key(target_relid, dist_attno, coloc_collation))
		IS_NO("a unique index of the target does not include the distribution column under the collation of its distribution");

	/* ON CONFLICT (11.7) */
	oc = q->onConflict;
	if (oc != NULL)
	{
		if (!ks_onconflict_action_shippable(oc->action) || KS_ONCONFLICT_IS_SELECT(oc))
			IS_NO("its ON CONFLICT action cannot be shipped");
		if (oc->action == ONCONFLICT_UPDATE)
		{
			foreach(lc, oc->onConflictSet)
			{
				TargetEntry *te = (TargetEntry *) lfirst(lc);

				if (!te->resjunk && te->resno == dist_attno)
					IS_NO("its ON CONFLICT DO UPDATE assigns the distribution column");
			}
			if (!vars_of_target_only((Node *) oc->onConflictSet, q->resultRelation, excl) ||
				!vars_of_target_only(oc->onConflictWhere, q->resultRelation, excl))
				IS_NO("its ON CONFLICT DO UPDATE uses something other than the target and EXCLUDED");
		}
		if (!vars_of_target_only((Node *) oc->arbiterElems, q->resultRelation, excl) ||
			!vars_of_target_only(oc->arbiterWhere, q->resultRelation, excl))
			IS_NO("the inference clause of its ON CONFLICT uses something other than the target");
	}
	/* RETURNING reads the target only (an INSERT cannot name its source there) */
	if (q->returningList != NIL &&
		!vars_of_target_only((Node *) q->returningList, q->resultRelation, 0))
		IS_NO("its RETURNING uses something other than the target");

	/* Every expression of the whole statement, in the multi-relation mode of the walker (5.1). */
	if (!kingres_shard_stmt_shippable_colocated(q, target_relid, coloc_id, &why))
		IS_NO(why);

	/* The query that is shipped is `q`; `is->pd.query` points into it. */
	is->insert = q;
	is->source = srte->subquery;
	is->target_relid = target_relid;
	is->colocation_id = coloc_id;
	is->dist_attno = dist_attno;
	is->relids = list_make1_oid(target_relid);
	foreach(lc, is->pd.rels)
		is->relids = list_append_unique_oid(is->relids, ((KsPdRel *) lfirst(lc))->relid);
	return true;
#undef IS_NO
}

/* ======================================================================
 * A closed query (P4, P5 without order or limit)
 * ====================================================================== */

char *
kingres_shard_query_marker(int64 place)
{
	return psprintf(KS_QUERY_MARKER_PREFIX "place=" INT64_FORMAT " */ ", place);
}

bool
kingres_shard_parse_query_marker(const char *query_string, int64 *place)
{
	const char *p;
	char	   *end;

	if (query_string == NULL ||
		strncmp(query_string, KS_QUERY_MARKER_PREFIX, strlen(KS_QUERY_MARKER_PREFIX)) != 0)
		return false;
	p = query_string + strlen(KS_QUERY_MARKER_PREFIX);
	if (strncmp(p, "place=", 6) != 0)
		return false;
	p += 6;
	*place = strtoll(p, &end, 10);
	if (end == p)
		return false;
	return strncmp(end, " */ ", 4) == 0;
}

bool
kingres_shard_query_fragment_analyze(Query *query, KsQueryFragment *qf, const char **reason)
{
	memset(qf, 0, sizeof(*qf));
	if (!kingres_shard_pushdown_analyze(query, &qf->pd, reason))
		return false;
	if (!kingres_shard_pushdown_closed(&qf->pd, reason))
		return false;
	if (query->limitCount != NULL || query->limitOffset != NULL)
	{
		*reason = "it has LIMIT or OFFSET, which the node that gathers the rows applies";
		return false;
	}
	if (!kingres_shard_stmt_shippable_colocated(query, InvalidOid, qf->pd.colocation_id, reason))
		return false;
	qf->colocation_id = qf->pd.colocation_id;
	qf->relids = kingres_shard_pushdown_relids(&qf->pd);
	return true;
}

/* ======================================================================
 * A partial aggregation (design 7, step 7)
 * ====================================================================== */

char *
kingres_shard_partial_marker(int64 place)
{
	return psprintf(KS_PARTIAL_MARKER_PREFIX "place=" INT64_FORMAT " */ ", place);
}

bool
kingres_shard_parse_partial_marker(const char *query_string, int64 *place)
{
	const char *p;
	char	   *end;

	if (query_string == NULL ||
		strncmp(query_string, KS_PARTIAL_MARKER_PREFIX, strlen(KS_PARTIAL_MARKER_PREFIX)) != 0)
		return false;
	p = query_string + strlen(KS_PARTIAL_MARKER_PREFIX);
	if (strncmp(p, "place=", 6) != 0)
		return false;
	p += 6;
	*place = strtoll(p, &end, 10);
	if (end == p)
		return false;
	return strncmp(end, " */ ", 4) == 0;
}

/*
 * Can a group return the partial state of this aggregate and this node combine the states?  The
 * catalog says (7.1): a built-in aggregate of the normal kind, with a combine function, and a
 * serialization pair when its state is of type internal; no DISTINCT and no ORDER BY inside the call.
 */
bool
kingres_shard_aggregate_partial_ok(Aggref *a, const char **reason)
{
	HeapTuple	tup;
	Form_pg_aggregate agg;
	bool		ok = true;

	if (a->aggdistinct != NIL || a->aggorder != NIL)
	{
		*reason = "an aggregate has DISTINCT or ORDER BY inside the call, which cannot be split";
		return false;
	}
	if (a->aggfnoid >= FirstGenbkiObjectId)
	{
		*reason = "an aggregate is not built in";
		return false;
	}
	tup = SearchSysCache1(AGGFNOID, ObjectIdGetDatum(a->aggfnoid));
	if (!HeapTupleIsValid(tup))
	{
		*reason = "an aggregate is not in the catalog";
		return false;
	}
	agg = (Form_pg_aggregate) GETSTRUCT(tup);
	if (agg->aggkind != AGGKIND_NORMAL)
	{
		*reason = "an ordered-set or hypothetical-set aggregate cannot be split";
		ok = false;
	}
	else if (!OidIsValid(agg->aggcombinefn))
	{
		*reason = "an aggregate has no combine function";
		ok = false;
	}
	else if (agg->aggtranstype == INTERNALOID &&
			 (!OidIsValid(agg->aggserialfn) || !OidIsValid(agg->aggdeserialfn)))
	{
		*reason = "the state of an aggregate cannot be serialized";
		ok = false;
	}
	ReleaseSysCache(tup);
	return ok;
}

typedef struct PartialAggCtx
{
	const char *reason;
	bool		bad;
} PartialAggCtx;

static bool
partial_agg_walker(Node *node, PartialAggCtx *ctx)
{
	if (node == NULL)
		return false;
	if (IsA(node, Aggref))
	{
		if (!kingres_shard_aggregate_partial_ok((Aggref *) node, &ctx->reason))
		{
			ctx->bad = true;
			return true;
		}
		return false;
	}
	if (IsA(node, Query))
		return false;
	return expression_tree_walker(node, partial_agg_walker, ctx);
}

/*
 * Is the query one that every group can aggregate on its own rows, the partial states being combined
 * by the node that gathers them?  The shape rules of the extension (7.2): an aggregate or a grouping
 * (grouping sets, windows, DISTINCT and a set returning function are the scan's), every aggregate
 * splits (above).  Only the shape; the tables are the business of kingres_shard_pushdown_analyze.
 */
bool
kingres_shard_partial_shape_ok(Query *query, const char **reason)
{
	PartialAggCtx ctx = {NULL, false};

	if (!query->hasAggs && query->groupClause == NIL)
	{
		*reason = "it neither aggregates nor groups";
		return false;
	}
	if (query->groupingSets != NIL)
	{
		*reason = "it has grouping sets";
		return false;
	}
	if (query->hasWindowFuncs || query->windowClause != NIL)
	{
		*reason = "it has a window function";
		return false;
	}
	if (query->distinctClause != NIL)
	{
		*reason = "it removes duplicates after the aggregation";
		return false;
	}
	if (query->hasTargetSRFs)
	{
		*reason = "it has a set-returning function in the select list";
		return false;
	}
	if (partial_agg_walker((Node *) query->targetList, &ctx) ||
		partial_agg_walker(query->havingQual, &ctx))
	{
		*reason = ctx.reason;
		return false;
	}
	return true;
}

/*
 * The fragment of a partial aggregation as a group receives it: a grouping query over the tables of one
 * colocation group (the analysis of pushdown.c), whose aggregates split, with no HAVING, ORDER BY, LIMIT,
 * DISTINCT or window (the node that combines applies them).  A group that is asked for more than that
 * refuses, and that is not a stale map.
 */
bool
kingres_shard_partial_fragment_analyze(Query *query, KsQueryFragment *qf, const char **reason)
{
	memset(qf, 0, sizeof(*qf));
	if (!kingres_shard_pushdown_analyze(query, &qf->pd, reason))
		return false;
	if (!kingres_shard_partial_shape_ok(query, reason))
		return false;
	{
		ListCell   *lt;

		foreach(lt, query->targetList)
		{
			TargetEntry *te = lfirst_node(TargetEntry, lt);

			if (!IsA(te->expr, Aggref) && contain_aggs_of_level((Node *) te->expr, 0))
			{
				*reason = "an aggregate is inside an expression: the groups return the bare states";
				return false;
			}
		}
	}
	if (query->havingQual != NULL || query->sortClause != NIL ||
		query->limitCount != NULL || query->limitOffset != NULL)
	{
		*reason = "it has HAVING, ORDER BY or LIMIT, which the node that combines the states applies";
		return false;
	}
	if (!kingres_shard_stmt_shippable_partial(query, qf->pd.colocation_id, reason))
		return false;
	qf->colocation_id = qf->pd.colocation_id;
	qf->relids = kingres_shard_pushdown_relids(&qf->pd);
	return true;
}
