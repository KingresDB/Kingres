/*-------------------------------------------------------------------------
 *
 * row_router.c
 *	  The "Kingres Row Router" custom scan (design/shard-phase2b.md, section 11).
 *
 * An INSERT into a distributed table that is not restricted to one bucket (a multi-row
 * VALUES, an INSERT ... SELECT, a single row whose key is not a constant) cannot be
 * forwarded to one group.  The planner hook (planner.c) gives it a plan with one custom
 * scan, this one, and no ModifyTable:
 *
 *	- the source rows are the rewritten statement turned into a SELECT: one output column
 *	  per column the statement gives a value to, defaults and identity values included, so
 *	  that a default is computed once per row at this node (11.2).  The router runs it as
 *	  an SPI cursor, which the planner hook plans like any statement (a source over
 *	  distributed tables is read through the scan);
 *	- every row goes to the batch sender (batch_sender.c), which finds the group that owns
 *	  the bucket of the row's key and sends the rows in batches of one INSERT statement.
 *
 * Nothing happens before the executor asks for the first row: the whole statement runs
 * then, the rows that RETURNING gives back are kept in a tuplestore (bounded by work_mem)
 * and handed out afterwards, and the count is the sum of what the groups inserted.
 *
 * The statement's range table is the target table alone with its permission entry, so the
 * executor checks INSERT privilege (and the columns) at this node before the router starts,
 * as for any insert; the groups check it again as the same role.
 *
 * COPY FROM goes through the same sender (the second part of this file).
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/row_router.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "access/parallel.h"
#include "access/table.h"
#include "access/tupdesc.h"
#include "catalog/pg_am_d.h"
#include "catalog/pg_authid.h"
#include "catalog/pg_class.h"
#include "catalog/pg_type_d.h"
#include "commands/copy.h"
#include "commands/defrem.h"
#include "commands/explain.h"
#include "commands/explain_format.h"
#include "executor/executor.h"
#include "executor/spi.h"
#include "executor/tuptable.h"
#include "miscadmin.h"
#include "nodes/extensible.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "nodes/params.h"
#include "parser/parse_coerce.h"
#include "parser/parse_node.h"
#include "parser/parse_relation.h"
#include "parser/parsetree.h"
#include "rewrite/rewriteHandler.h"
#include "rewrite/rewriteManip.h"
#include "tcop/tcopprot.h"
#include "tcop/utility.h"
#include "utils/acl.h"
#include "utils/builtins.h"
#include "utils/datum.h"
#include "utils/fmgroids.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/ruleutils.h"
#include "utils/snapmgr.h"
#include "utils/tuplestore.h"

#include "batch_sender.h"
#include "catalog.h"
#include "fanout.h"
#include "forward_auth.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "placement_check.h"
#include "remote_scan.h"
#include "row_router.h"
#include "shippable.h"

/*
 * Layout of CustomScan.custom_private.  Strings and integers only, so that a plan can be
 * copied and read back.
 */
#define RR_SOURCE_SQL		0	/* String: the SELECT that gives the rows */
#define RR_SCHEMA			1	/* String: the target table */
#define RR_TABLE			2	/* String */
#define RR_RELID			3	/* Integer (an Oid) */
#define RR_COLOCATION		4	/* Integer: the colocation group of the table */
#define RR_PLACEMENT		5	/* String: its placement number when the statement was planned */
#define RR_KEY_POS			6	/* Integer: position of the key column among the columns */
#define RR_OVERRIDING		7	/* Integer: OVERRIDING SYSTEM VALUE */
#define RR_CONFLICT_SQL		8	/* String: "" or the ON CONFLICT clause */
#define RR_RETURNING_SQL	9	/* String: "" or the RETURNING list */
#define RR_NCOLS			10	/* Integer */
#define RR_COLUMNS			11	/* then (name String, type Integer, typmod Integer) per column */

typedef struct KsRouterState
{
	CustomScanState css;
	char	   *source_sql;
	char	   *schema;
	char	   *table;
	Oid			relid;
	int			colocation_id;
	int64		plan_placement;
	int			key_pos;
	bool		overriding;
	char	   *conflict_sql;
	char	   *returning_sql;
	List	   *columns;		/* of KsBatchColumn * */
	bool		done;
	bool		counted;
	int64		processed;
	Tuplestorestate *rows;
} KsRouterState;

static Node *rr_create_scan_state(CustomScan *cscan);
static void rr_begin_scan(CustomScanState *node, EState *estate, int eflags);
static TupleTableSlot *rr_exec_scan(CustomScanState *node);
static void rr_end_scan(CustomScanState *node);
static void rr_rescan(CustomScanState *node);
static void rr_explain_scan(CustomScanState *node, List *ancestors, ExplainState *es);

static const CustomScanMethods rr_scan_methods = {
	.CustomName = "Kingres Row Router",
	.CreateCustomScanState = rr_create_scan_state
};

static const CustomExecMethods rr_exec_methods = {
	.CustomName = "Kingres Row Router",
	.BeginCustomScan = rr_begin_scan,
	.ExecCustomScan = rr_exec_scan,
	.EndCustomScan = rr_end_scan,
	.ReScanCustomScan = rr_rescan,
	.ExplainCustomScan = rr_explain_scan
};

void
kingres_shard_row_router_init(void)
{
	RegisterCustomScanMethods(&rr_scan_methods);
}

/* ======================================================================
 * Planning
 * ====================================================================== */

/* The distributed tables a source reads must not hide the rights of a view owner. */
static bool
source_unsafe_walker(Node *node, void *context)
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

			if (rte->rtekind == RTE_SUBQUERY && rte->security_barrier)
				ereport(ERROR,
						(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
						 errmsg("an INSERT into a distributed table of several shard groups cannot read a security barrier view"),
						 errdetail("The statement that gives the rows is sent as text and planned again, which would lose the protection of the view."),
						 errhint("Read the rows into a table first, or insert from the tables the view reads.")));
			if (rte->rtekind == RTE_RELATION && rte->perminfoindex > 0)
			{
				Oid			owner = getRTEPermissionInfo(q->rteperminfos, rte)->checkAsUser;

				if (OidIsValid(owner) && owner != GetUserId())
					ereport(ERROR,
							(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
							 errmsg("an INSERT into a distributed table of several shard groups reads \"%s\" through a view owned by role \"%s\"",
									get_rel_name(rte->relid), GetUserNameFromId(owner, false)),
							 errdetail("The rights on the table are those of the owner of the view, and the statement that gives the rows runs with the rights of the session role."),
							 errhint("Create the view WITH (security_invoker = true), or run the statement as the owner of the view.")));
			}
		}
		return query_tree_walker(q, source_unsafe_walker, context, 0);
	}
	return expression_tree_walker(node, source_unsafe_walker, context);
}

/* A RETURNING that names OLD or NEW (PostgreSQL 18) is not shipped. */
static bool
returning_var_walker(Node *node, void *context)
{
	if (node == NULL)
		return false;
	if (IsA(node, Var) && ((Var *) node)->varreturningtype != VAR_RETURNING_DEFAULT)
		return true;
	return expression_tree_walker(node, returning_var_walker, context);
}

/* A column of the target itself, whatever its type: its text form crosses intact. */
static bool
is_target_column(Node *e, Index rti)
{
	return e != NULL && IsA(e, Var) && ((Var *) e)->varno == rti && ((Var *) e)->varlevelsup == 0 &&
		((Var *) e)->varattno > 0;
}

/* Is the expression something a group can evaluate for this INSERT (5.1)? */
static void
check_shippable_part(Node *e, Index rti, const char *what)
{
	if (is_target_column(e, rti))
		return;
	if (!kingres_shard_clause_shippable(e, rti))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("the %s of this INSERT into a distributed table of several shard groups cannot be run by the shard groups", what),
				 errdetail("It uses a function, operator, type or collation that is not built in and immutable, or something that is not a plain expression."),
				 errhint("Use only columns of the table and built-in immutable expressions in %s, or insert the rows one statement at a time.", what)));
}

/* nextval() for an identity column, as an expression that the deparser can print and a SELECT can run. */
static Node *
next_value_mutator(Node *node, void *context)
{
	if (node == NULL)
		return NULL;
	if (IsA(node, NextValueExpr))
	{
		NextValueExpr *nv = (NextValueExpr *) node;
		Const	   *seq = makeConst(REGCLASSOID, -1, InvalidOid, sizeof(Oid),
									ObjectIdGetDatum(nv->seqid), false, true);
		Node	   *f = (Node *) makeFuncExpr(F_NEXTVAL, INT8OID, list_make1(seq), InvalidOid,
											  InvalidOid, COERCE_EXPLICIT_CALL);

		if (nv->typeId != INT8OID)
			f = coerce_to_target_type(NULL, f, INT8OID, nv->typeId, -1, COERCION_EXPLICIT,
									  COERCE_IMPLICIT_CAST, -1);
		return f;
	}
	return expression_tree_mutator(node, next_value_mutator, context);
}

/* The text of "ON CONFLICT ... DO NOTHING" for the statement's clause. */
static char *
conflict_clause(Query *parse, Index rti, List *dpcontext)
{
	OnConflictExpr *oc = parse->onConflict;
	StringInfoData buf;
	ListCell   *lc;

	if (oc == NULL)
		return NULL;
	if (oc->action == ONCONFLICT_UPDATE)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("ON CONFLICT DO UPDATE is not supported yet for an INSERT that goes to several shard groups"),
				 errdetail("The rows are sent to the groups in batches, and a second row for the same key could update the row of an earlier batch, where one statement raises \"ON CONFLICT DO UPDATE command cannot affect row a second time\"."),
				 errhint("A colocated INSERT ... SELECT is shipped whole and allows it; or use DO NOTHING.")));
	if (oc->action != ONCONFLICT_NOTHING || KS_ONCONFLICT_IS_SELECT(oc))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("this ON CONFLICT action is not supported for an INSERT that goes to several shard groups")));

	initStringInfo(&buf);
	appendStringInfoString(&buf, "ON CONFLICT");
	if (OidIsValid(oc->constraint))
		appendStringInfo(&buf, " ON CONSTRAINT %s", quote_identifier(get_constraint_name(oc->constraint)));
	else if (oc->arbiterElems != NIL)
	{
		bool		first = true;

		appendStringInfoString(&buf, " (");
		foreach(lc, oc->arbiterElems)
		{
			InferenceElem *ie = (InferenceElem *) lfirst(lc);
			Node	   *e = (Node *) ie->expr;
			Oid			opclass;

			check_shippable_part(e, rti, "inference clause of ON CONFLICT");
			/* an explicit collation or operator class is not carried: it would be lost */
			opclass = GetDefaultOpClass(exprType(e), BTREE_AM_OID);
			if ((OidIsValid(ie->infercollid) && ie->infercollid != exprCollation(e)) ||
				(OidIsValid(ie->inferopclass) && ie->inferopclass != opclass))
				ereport(ERROR,
						(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
						 errmsg("ON CONFLICT with a collation or an operator class in its inference clause is not supported yet for an INSERT that goes to several shard groups"),
						 errhint("Use ON CONFLICT ON CONSTRAINT, or no inference clause.")));
			if (!first)
				appendStringInfoString(&buf, ", ");
			first = false;
			if (is_target_column(e, rti))
				appendStringInfoString(&buf, deparse_expression(e, dpcontext, false, false));
			else
				appendStringInfo(&buf, "(%s)", deparse_expression(e, dpcontext, false, false));
		}
		appendStringInfoChar(&buf, ')');
		if (oc->arbiterWhere != NULL)
		{
			check_shippable_part(oc->arbiterWhere, rti, "inference clause of ON CONFLICT");
			appendStringInfo(&buf, " WHERE %s", deparse_expression(oc->arbiterWhere, dpcontext, false, false));
		}
	}
	appendStringInfoString(&buf, " DO NOTHING");
	return buf.data;
}

/*
 * The length coercion of an assignment (varchar(x, n, false), bpchar(x, n, false)) that the parser put into a VALUES
 * list or a select list is an implicit cast, which the deparser does not print: the text of the source would then
 * carry the value unchecked, and the cast that the router writes after it is an explicit one, which cuts a value
 * that is too long instead of failing as a single node does (review P2C-01).  The coercion is made a visible call
 * of its function, which the deparser writes as such.
 */
static Node *
length_coercion_visible(Node *node, void *context)
{
	if (node == NULL)
		return NULL;
	if (IsA(node, Query))
		return (Node *) query_tree_mutator((Query *) node, length_coercion_visible, context, 0);
	if (IsA(node, FuncExpr))
	{
		FuncExpr   *f = (FuncExpr *) expression_tree_mutator(node, length_coercion_visible, context);

		if (f->funcformat == COERCE_IMPLICIT_CAST && list_length(f->args) == 3 &&
			IsA(llast(f->args), Const) && ((Const *) llast(f->args))->consttype == BOOLOID &&
			!((Const *) llast(f->args))->constisnull && !DatumGetBool(((Const *) llast(f->args))->constvalue) &&
			IsA(lsecond(f->args), Const) && ((Const *) lsecond(f->args))->consttype == INT4OID)
			f->funcformat = COERCE_EXPLICIT_CALL;
		return (Node *) f;
	}
	return expression_tree_mutator(node, length_coercion_visible, context);
}

PlannedStmt *
kingres_shard_plan_row_router(Query *parse, const KsDistTable *dt, List *relids)
{
	Index		rti = parse->resultRelation;
	RangeTblEntry *trte = rt_fetch(rti, parse->rtable);
	RTEPermissionInfo *tperm = getRTEPermissionInfo(parse->rteperminfos, trte);
	Relation	rel;
	TupleDesc	desc;
	Query	   *w;
	RangeTblEntry *src = NULL;
	Index		srcrti = 0;
	Index		i;
	ListCell   *lc;
	List	   *cols = NIL;
	List	   *selexprs = NIL;
	int			key_pos = -1;
	bool		overriding = false;
	int			attno;
	char	   *source_sql;
	char	   *conflict_sql;
	char	   *returning_sql = NULL;
	char	   *schema;
	char	   *table;
	List	   *private;
	CustomScan *cscan;
	PlannedStmt *pstmt;
	int			nest;
	List	   *dpcontext;
	int64		placement = kingres_shard_placement_hash(dt->coloc);
	bool		returns_rows = parse->returningList != NIL;

	if (IsInParallelMode())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("an INSERT for several shard groups cannot be planned for a parallel worker")));
	if (parse->cteList != NIL || parse->hasModifyingCTE)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a WITH clause on an INSERT into a distributed table of several shard groups is not supported yet"),
				 errhint("Write the WITH clause inside the SELECT: INSERT INTO t WITH ... SELECT ...")));
	/*
	 * A view that checks its rows (WITH CHECK OPTION) or a policy of row-level security puts a
	 * check on every row that the rewriter has attached to the statement; the rows are sent as
	 * values and the groups do not know the view, so the check would be lost.
	 */
	if (parse->withCheckOptions != NIL)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("an INSERT into a distributed table of several shard groups cannot check the rows against a view or a row-level security policy"),
				 errhint("Insert into the table itself.")));
	(void) source_unsafe_walker((Node *) parse, NULL);

	/* The source of the rows, if the statement has one. */
	i = 0;
	foreach(lc, parse->rtable)
	{
		RangeTblEntry *rte = (RangeTblEntry *) lfirst(lc);

		i++;
		if (i == rti)
			continue;
		if (rte->rtekind == RTE_SUBQUERY || rte->rtekind == RTE_VALUES)
		{
			src = rte;
			srcrti = i;
			break;
		}
	}

	rel = table_open(trte->relid, NoLock);
	desc = RelationGetDescr(rel);
	schema = get_namespace_name(RelationGetNamespace(rel));
	table = pstrdup(RelationGetRelationName(rel));

	for (attno = 1; attno <= desc->natts; attno++)
	{
		Form_pg_attribute att = TupleDescAttr(desc, attno - 1);
		TargetEntry *te = NULL;
		Node	   *expr;
		bool		named;
		KsBatchColumn *c;

		if (att->attisdropped || att->attgenerated)
			continue;
		foreach(lc, parse->targetList)
		{
			TargetEntry *t = (TargetEntry *) lfirst(lc);

			if (!t->resjunk && t->resno == attno)
			{
				te = t;
				break;
			}
		}
		named = bms_is_member(attno - FirstLowInvalidHeapAttributeNumber, tperm->insertedCols);
		if (te == NULL)
		{
			if (attno != dt->dist_attno)
				continue;
			/* the key is always sent: a missing one is NULL, which PostgreSQL puts in bucket 0 */
			expr = (Node *) makeNullConst(att->atttypid, att->atttypmod, att->attcollation);
		}
		else
		{
			expr = (Node *) te->expr;
			/*
			 * A column the statement did not name whose default is a constant is left out, so
			 * that the group applies its own default and the column-level privileges behave as
			 * on one node (11.2).  The key column and identity columns are always sent.
			 */
			if (!named && IsA(expr, Const) && attno != dt->dist_attno && !att->attidentity)
				continue;
		}
		expr = next_value_mutator(copyObject(expr), NULL);
		if (att->attidentity == ATTRIBUTE_IDENTITY_ALWAYS)
			overriding = true;
		/*
		 * The value reaches the router as the table will store it: the assignment cast of the column, with its
		 * type modifier (a length or a scale: timestamp(0), numeric(8,2), char(5), varchar(8)), is applied in the
		 * source select, so the hash of the key is taken on the stored value, and a value that does not fit
		 * fails with the message of a single node (review P2C-01).
		 */
		if (exprType(expr) != att->atttypid || att->atttypmod != -1)
		{
			Node	   *coerced = coerce_to_target_type(NULL, expr, exprType(expr), att->atttypid, att->atttypmod,
														COERCION_ASSIGNMENT, COERCE_IMPLICIT_CAST, -1);

			if (coerced == NULL)
				ereport(ERROR,
						(errcode(ERRCODE_DATATYPE_MISMATCH),
						 errmsg("column \"%s\" is of type %s but the INSERT gives it a value of type %s",
								NameStr(att->attname), format_type_be(att->atttypid),
								format_type_be(exprType(expr)))));
			/*
			 * The length coercion of an assignment (varchar(n), char(n): varchar(x, n, false)) errors on a value that
			 * is too long; the same cast written as a cast in the text of the SELECT is an explicit one, which cuts
			 * the value.  It is therefore kept as a call of the function, which the deparser writes as one.
			 */
			if (IsA(coerced, FuncExpr) && ((FuncExpr *) coerced)->funcformat == COERCE_IMPLICIT_CAST &&
				list_length(((FuncExpr *) coerced)->args) == 3 &&
				IsA(llast(((FuncExpr *) coerced)->args), Const) &&
				!DatumGetBool(((Const *) llast(((FuncExpr *) coerced)->args))->constvalue))
				((FuncExpr *) coerced)->funcformat = COERCE_EXPLICIT_CALL;
			expr = coerced;
		}

		/*
		 * Say the type of the column in the text of the SELECT.  The deparser prints a constant
		 * of a VALUES list without its type when it can (a bigint 500 is "500"), so the SELECT
		 * that is planned again would give an integer where the column is a bigint, and the
		 * router reads every value with the output function of the column's type.
		 */
		expr = (Node *) makeRelabelType((Expr *) expr, att->atttypid, att->atttypmod,
										OidIsValid(att->attcollation) ? exprCollation(expr) : InvalidOid,
										COERCE_EXPLICIT_CAST);
		c = palloc(sizeof(KsBatchColumn));
		c->name = pstrdup(NameStr(att->attname));
		c->type = att->atttypid;
		c->typmod = att->atttypmod;
		if (attno == dt->dist_attno)
			key_pos = list_length(cols);
		cols = lappend(cols, c);
		selexprs = lappend(selexprs, expr);
	}
	if (key_pos < 0)
		elog(ERROR, "the distribution column of \"%s\" is not among the columns of an INSERT", table);

	/* The SELECT that gives the rows: the expressions of the target list, over the source. */
	w = makeNode(Query);
	w->commandType = CMD_SELECT;
	w->querySource = QSRC_ORIGINAL;
	if (src != NULL)
	{
		RangeTblEntry *inner = copyObject(src);
		RangeTblRef *rtr = makeNode(RangeTblRef);

		inner->eref = makeAlias("ks_src", inner->eref->colnames);
		inner->alias = NULL;
		inner->inFromCl = true;
		w->rtable = list_make1(inner);
		rtr->rtindex = 1;
		w->jointree = makeFromExpr(list_make1(rtr), NULL);
	}
	else
		w->jointree = makeFromExpr(NIL, NULL);
	i = 0;
	foreach(lc, selexprs)
	{
		Node	   *expr = (Node *) lfirst(lc);
		KsBatchColumn *c = (KsBatchColumn *) list_nth(cols, i);

		if (src != NULL)
			ChangeVarNodes(expr, srcrti, 1, 0);
		w->targetList = lappend(w->targetList,
								makeTargetEntry((Expr *) expr, (AttrNumber) (i + 1),
												pstrdup(c->name), false));
		i++;
	}
	if (parse->hasSubLinks)
		w->hasSubLinks = true;
	w = query_tree_mutator(w, length_coercion_visible, NULL, 0);

	nest = kingres_shard_set_transmission_modes();
	source_sql = pg_get_querydef(w, false);

	dpcontext = deparse_context_for(RelationGetRelationName(rel), RelationGetRelid(rel));
	conflict_sql = conflict_clause(parse, rti, dpcontext);

	if (returns_rows)
	{
		StringInfoData buf;

		if (returning_var_walker((Node *) parse->returningList, NULL))
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("RETURNING with OLD or NEW is not supported yet for an INSERT that goes to several shard groups")));
		initStringInfo(&buf);
		foreach(lc, parse->returningList)
		{
			TargetEntry *te = (TargetEntry *) lfirst(lc);

			if (te->resjunk)
				continue;
			check_shippable_part((Node *) te->expr, rti, "RETURNING list");
			appendStringInfo(&buf, "%s%s", buf.len > 0 ? ", " : "",
							 deparse_expression((Node *) te->expr, dpcontext, false, false));
		}
		returning_sql = buf.data;
	}
	kingres_shard_reset_transmission_modes(nest);
	table_close(rel, NoLock);

	private = list_make4(makeString(source_sql), makeString(schema), makeString(table),
						 makeInteger((int) trte->relid));
	private = lappend(private, makeInteger(dt->colocation_id));
	private = lappend(private, makeString(psprintf(INT64_FORMAT, placement)));
	private = lappend(private, makeInteger(key_pos));
	private = lappend(private, makeInteger(overriding ? 1 : 0));
	private = lappend(private, makeString(conflict_sql ? conflict_sql : ""));
	private = lappend(private, makeString(returning_sql ? returning_sql : ""));
	private = lappend(private, makeInteger(list_length(cols)));
	foreach(lc, cols)
	{
		KsBatchColumn *c = (KsBatchColumn *) lfirst(lc);

		private = lappend(private, makeString(pstrdup(c->name)));
		private = lappend(private, makeInteger((int) c->type));
		private = lappend(private, makeInteger(c->typmod));
	}

	cscan = kingres_shard_build_scan(parse, private, returns_rows);
	cscan->methods = &rr_scan_methods;
	pstmt = kingres_shard_wrap_plan(parse, cscan, relids, returns_rows);

	/*
	 * The range table of the plan is the target alone with its permission entry, so
	 * the executor checks the privilege of the caller before the router starts.
	 */
	{
		RangeTblEntry *t2 = copyObject(trte);
		RTEPermissionInfo *p2 = copyObject(tperm);

		t2->perminfoindex = 1;
		pstmt->rtable = list_make1(t2);
		pstmt->permInfos = list_make1(p2);
		pstmt->unprunableRelids = bms_make_singleton(1);
	}
	return pstmt;
}

/* ======================================================================
 * Execution
 * ====================================================================== */

static Node *
rr_create_scan_state(CustomScan *cscan)
{
	KsRouterState *s = palloc0(sizeof(KsRouterState));
	List	   *p = cscan->custom_private;
	int			n;
	int			i;

	NodeSetTag(&s->css, T_CustomScanState);
	s->css.methods = &rr_exec_methods;
	s->css.slotOps = &TTSOpsMinimalTuple;

	s->source_sql = pstrdup(strVal(list_nth(p, RR_SOURCE_SQL)));
	s->schema = pstrdup(strVal(list_nth(p, RR_SCHEMA)));
	s->table = pstrdup(strVal(list_nth(p, RR_TABLE)));
	s->relid = (Oid) intVal(list_nth(p, RR_RELID));
	s->colocation_id = intVal(list_nth(p, RR_COLOCATION));
	s->plan_placement = pg_strtoint64(strVal(list_nth(p, RR_PLACEMENT)));
	s->key_pos = intVal(list_nth(p, RR_KEY_POS));
	s->overriding = intVal(list_nth(p, RR_OVERRIDING)) != 0;
	s->conflict_sql = pstrdup(strVal(list_nth(p, RR_CONFLICT_SQL)));
	s->returning_sql = pstrdup(strVal(list_nth(p, RR_RETURNING_SQL)));
	n = intVal(list_nth(p, RR_NCOLS));
	for (i = 0; i < n; i++)
	{
		KsBatchColumn *c = palloc(sizeof(KsBatchColumn));

		c->name = pstrdup(strVal(list_nth(p, RR_COLUMNS + 3 * i)));
		c->type = (Oid) intVal(list_nth(p, RR_COLUMNS + 3 * i + 1));
		c->typmod = intVal(list_nth(p, RR_COLUMNS + 3 * i + 2));
		s->columns = lappend(s->columns, c);
	}
	return (Node *) s;
}

static void
rr_begin_scan(CustomScanState *node, EState *estate, int eflags)
{
	if (eflags & EXEC_FLAG_EXPLAIN_ONLY)
		return;
	if (IsParallelWorker())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("an INSERT for several shard groups cannot run in a parallel worker"),
				 errhint("Mark the function that runs the statement PARALLEL RESTRICTED or PARALLEL UNSAFE.")));
	if (XactReadOnly)
		ereport(ERROR,
				(errcode(ERRCODE_READ_ONLY_SQL_TRANSACTION),
				 errmsg("cannot execute INSERT in a read-only transaction")));
	kingres_shard_refuse_replica_role("a distributed table on several shard groups");
}

/* The parameters of the statement as SPI arguments (the type of an unused one is text). */
static void
spi_params(ParamListInfo params, int *nargs, Oid **types, Datum **values, char **nulls)
{
	int			n = params ? params->numParams : 0;
	int			i;

	*types = palloc(sizeof(Oid) * Max(n, 1));
	*values = palloc(sizeof(Datum) * Max(n, 1));
	*nulls = palloc(Max(n, 1));
	for (i = 0; i < n; i++)
	{
		ParamExternData *prm;
		ParamExternData workspace;

		if (params->paramFetch != NULL)
			prm = params->paramFetch(params, i + 1, false, &workspace);
		else
			prm = &params->params[i];
		(*types)[i] = OidIsValid(prm->ptype) ? prm->ptype : TEXTOID;
		(*values)[i] = prm->value;
		(*nulls)[i] = (prm->isnull || !OidIsValid(prm->ptype)) ? 'n' : ' ';
	}
	*nargs = n;
}

/* The destination and the spec that every user of the sender (INSERT, COPY) builds the same way. */
static void
make_destination(KsDestination *dest, KsBatchSpec *spec, const KsDistTable *dt,
				 const char *schema, const char *table, int key_pos, int64 placement,
				 List *columns, bool overriding, const char *conflict_sql,
				 const char *returning_sql, TupleDesc returning_desc,
				 Tuplestorestate *returning_rows)
{
	memset(dest, 0, sizeof(*dest));
	dest->kind = KS_DEST_PLACEMENT;
	dest->schema = schema;
	dest->table = table;
	dest->relid = dt->relid;
	dest->placement = dt->coloc;
	dest->key_attno = key_pos;
	dest->expect_placement = placement;
	memset(spec, 0, sizeof(*spec));
	spec->columns = columns;
	spec->overriding_system_value = overriding;
	if (conflict_sql != NULL && conflict_sql[0] != '\0')
	{
		spec->conflict = KS_CONFLICT_NOTHING;
		spec->conflict_sql = conflict_sql;
	}
	if (returning_sql != NULL && returning_sql[0] != '\0')
	{
		spec->returning_sql = returning_sql;
		spec->returning_desc = returning_desc;
		spec->returning_rows = returning_rows;
	}
}

/* Run the statement: read the source, feed the sender, finish. */
static void
rr_run(KsRouterState *s)
{
	EState	   *estate = s->css.ss.ps.state;
	const KsCatalog *cat;
	const KsDistTable *dt;
	KsDestination dest;
	KsBatchSpec spec;
	KsBatchSender *sender;
	MemoryContext old;
	int			ncols = list_length(s->columns);
	int			nargs;
	Oid		   *argtypes;
	Datum	   *argvalues;
	char	   *argnulls;
	Portal		portal;
	int			nest;
	Datum	   *values = palloc(sizeof(Datum) * ncols);
	bool	   *nulls = palloc(sizeof(bool) * ncols);
	bool		checked = false;
	Size		widest = 0;
	TupleDesc	retdesc = NULL;

	old = MemoryContextSwitchTo(estate->es_query_cxt);
	if (s->returning_sql[0] != '\0')
	{
		s->rows = tuplestore_begin_heap(false, false, work_mem);
		retdesc = s->css.ss.ss_ScanTupleSlot->tts_tupleDescriptor;
	}
	MemoryContextSwitchTo(old);

	/* The table must still be distributed and the bucket map the one the statement was planned with. */
	cat = kingres_shard_catalog();
	dt = hash_search(cat->dist_tables, &s->relid, HASH_FIND, NULL);
	if (dt == NULL || dt->is_reference)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("table \"%s\" is no longer a hash-distributed table", s->table),
				 errhint("Repeat the statement; it is planned again.")));
	kingres_shard_placement_check_plan(s->colocation_id, s->plan_placement);

	old = MemoryContextSwitchTo(estate->es_query_cxt);
	make_destination(&dest, &spec, dt, s->schema, s->table, s->key_pos, s->plan_placement,
					 s->columns, s->overriding, s->conflict_sql, s->returning_sql, retdesc, s->rows);
	sender = kingres_shard_batch_begin(&dest, &spec);
	MemoryContextSwitchTo(old);

	spi_params(estate->es_param_list_info, &nargs, &argtypes, &argvalues, &argnulls);
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	nest = kingres_shard_set_transmission_modes();
	portal = SPI_cursor_open_with_args(NULL, s->source_sql, nargs, argtypes, argvalues, argnulls,
									   true, CURSOR_OPT_NO_SCROLL);
	kingres_shard_reset_transmission_modes(nest);
	if (portal == NULL)
		elog(ERROR, "could not open the cursor that reads the rows of an INSERT");

	/*
	 * The fetch is bounded in bytes as the batches are (review P2C-02): it starts with a few rows and then
	 * follows the widest row seen so far, so that a batch of the cursor weighs about insert_batch_bytes plus
	 * one row, down to one row at a time.
	 */
	for (;;)
	{
		SPITupleTable *tuptable;
		uint64		nproc;
		uint64		k;
		long		fetch;

		CHECK_FOR_INTERRUPTS();
		fetch = (widest == 0) ? Min(kingres_shard_insert_batch_rows, 16) :
			Max(1, Min((long) kingres_shard_insert_batch_rows, (long) kingres_shard_insert_batch_bytes / (long) widest));
		SPI_cursor_fetch(portal, true, fetch);
		tuptable = SPI_tuptable;
		nproc = SPI_processed;
		if (nproc == 0 || tuptable == NULL)
			break;
		if (!checked)
		{
			int			c;

			if (tuptable->tupdesc->natts != ncols)
				elog(ERROR, "the source of an INSERT returned %d columns, expected %d",
					 tuptable->tupdesc->natts, ncols);
			for (c = 0; c < ncols; c++)
				if (TupleDescAttr(tuptable->tupdesc, c)->atttypid != ((KsBatchColumn *) list_nth(s->columns, c))->type)
					elog(ERROR, "the source of an INSERT returned type %s for column %d, expected %s",
						 format_type_be(TupleDescAttr(tuptable->tupdesc, c)->atttypid), c + 1,
						 format_type_be(((KsBatchColumn *) list_nth(s->columns, c))->type));
			checked = true;
		}
		nest = kingres_shard_set_transmission_modes();
		for (k = 0; k < nproc; k++)
		{
			HeapTuple	tup = tuptable->vals[k];
			int			c;

			if ((Size) tup->t_len > widest)
				widest = tup->t_len;
			for (c = 0; c < ncols; c++)
				values[c] = heap_getattr(tup, c + 1, tuptable->tupdesc, &nulls[c]);
			kingres_shard_batch_add(sender, values, nulls);
		}
		kingres_shard_reset_transmission_modes(nest);
		SPI_freetuptable(tuptable);
	}
	SPI_cursor_close(portal);
	SPI_finish();

	s->processed = kingres_shard_batch_finish(sender);
	kingres_shard_batch_end(sender);
}

static TupleTableSlot *
rr_exec_scan(CustomScanState *node)
{
	KsRouterState *s = (KsRouterState *) node;
	TupleTableSlot *slot = node->ss.ss_ScanTupleSlot;

	if (!s->done)
	{
		rr_run(s);
		s->done = true;
	}
	if (s->returning_sql[0] == '\0')
	{
		if (!s->counted)
		{
			node->ss.ps.state->es_processed += s->processed;
			s->counted = true;
		}
		return NULL;
	}
	ExecClearTuple(slot);
	if (!tuplestore_gettupleslot(s->rows, true, false, slot))
		return NULL;
	node->ss.ps.state->es_processed++;
	return slot;
}

static void
rr_end_scan(CustomScanState *node)
{
	KsRouterState *s = (KsRouterState *) node;

	if (s->rows != NULL)
		tuplestore_end(s->rows);
	s->rows = NULL;
}

/* A write is never run twice. */
static void
rr_rescan(CustomScanState *node)
{
}

static void
rr_explain_scan(CustomScanState *node, List *ancestors, ExplainState *es)
{
	KsRouterState *s = (KsRouterState *) node;
	StringInfoData flat;
	const char *p;
	bool		pending_space = false;

	ExplainPropertyText("Target", quote_qualified_identifier(s->schema, s->table), es);
	ExplainPropertyInteger("Batch rows", NULL, kingres_shard_insert_batch_rows, es);
	if (s->conflict_sql[0] != '\0')
		ExplainPropertyText("Conflict", s->conflict_sql, es);
	initStringInfo(&flat);
	for (p = s->source_sql; *p; p++)
	{
		if (*p == '\n' || *p == '\r' || *p == '\t' || *p == ' ')
		{
			pending_space = (flat.len > 0);
			continue;
		}
		if (pending_space)
			appendStringInfoChar(&flat, ' ');
		pending_space = false;
		appendStringInfoChar(&flat, *p);
	}
	ExplainPropertyText("Source", flat.data, es);
}

/* ======================================================================
 * COPY FROM (design 11.6)
 * ====================================================================== */

/* The options that CopyFrom, not NextCopyFrom, carries out: refused by name. */
static void
refuse_copy_option(const char *name, const char *why)
{
	char	   *upper = pstrdup(name);
	char	   *p;

	for (p = upper; *p; p++)
		*p = pg_toupper((unsigned char) *p);
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("COPY FROM with %s is not supported yet for a distributed table of several shard groups", upper),
			 errdetail("%s", why),
			 errhint("Load the rows with a plain COPY, or into a staging table first and move them with INSERT ... SELECT.")));
}

/* The bytes a value takes in memory, for the bound of a chunk. */
static Size
datum_bytes(Form_pg_attribute att, Datum v)
{
	if (att->attlen > 0)
		return att->attlen;
	if (att->attlen == -1)
		return VARSIZE_ANY(DatumGetPointer(v));
	return strlen(DatumGetCString(v)) + 1;
}

uint64
kingres_shard_copy_from_routed(CopyStmt *stmt, const char *query_string)
{
	Relation	rel;
	ParseState *pstate;
	ParseNamespaceItem *nsitem;
	RTEPermissionInfo *perminfo;
	TupleDesc	desc;
	List	   *attnums;
	ListCell   *lc;
	const KsCatalog *cat;
	const KsDistTable *dt;
	CopyFromState cstate;
	EState	   *estate;
	ExprContext *econtext;
	KsBatchSender *sender;
	KsDestination dest;
	KsBatchSpec spec;
	List	   *columns = NIL;
	List	   *sent_attnos = NIL;
	int			key_pos = -1;
	bool		overriding = false;
	int			nsent;
	int			chunk_max;
	MemoryContext chunkcxt;
	Datum	   *values;
	bool	   *nulls;
	Datum	   *cvalues;
	bool	   *cnulls;
	Datum	   *rowvalues;
	bool	   *rownulls;
	uint64		total = 0;
	int			attno;
	int			j;
	int64		placement;
	ErrorContextCallback errcallback;
	bool		eof = false;

	/* The checks of DoCopy for COPY FROM, which this does not call. */
	if (stmt->filename != NULL)
	{
		if (stmt->is_program)
		{
			if (!has_privs_of_role(GetUserId(), ROLE_PG_EXECUTE_SERVER_PROGRAM))
				ereport(ERROR,
						(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
						 errmsg("permission denied to COPY to or from an external program"),
						 errdetail("Only roles with privileges of the \"%s\" role may COPY to or from an external program.",
								   "pg_execute_server_program"),
						 errhint("Anyone can COPY to stdout or from stdin. "
								 "psql's \\copy command also works for anyone.")));
		}
		else if (!has_privs_of_role(GetUserId(), ROLE_PG_READ_SERVER_FILES))
			ereport(ERROR,
					(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
					 errmsg("permission denied to COPY from a file"),
					 errdetail("Only roles with privileges of the \"%s\" role may COPY from a file.",
							   "pg_read_server_files"),
					 errhint("Anyone can COPY to stdout or from stdin. "
							 "psql's \\copy command also works for anyone.")));
	}
	if (stmt->whereClause != NULL)
		refuse_copy_option("WHERE", "The filter is applied by CopyFrom, which the router does not call.");
	foreach(lc, stmt->options)
	{
		DefElem    *def = (DefElem *) lfirst(lc);

		if (strcmp(def->defname, "freeze") == 0)
			refuse_copy_option("FREEZE", "Rows of a distributed table are written by the shard groups, which cannot freeze a table that this transaction did not create.");
		if (strcmp(def->defname, "on_error") == 0 || strcmp(def->defname, "reject_limit") == 0 ||
			strcmp(def->defname, "log_verbosity") == 0)
			refuse_copy_option(def->defname, "The handling of rows with errors is done by CopyFrom, which the router does not call.");
	}

	rel = table_openrv(stmt->relation, RowExclusiveLock);
	desc = RelationGetDescr(rel);
	if (rel->rd_rel->relrowsecurity)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("COPY FROM not supported with row-level security"),
				 errhint("Use INSERT statements instead.")));
	if (XactReadOnly && !rel->rd_islocaltemp)
		PreventCommandIfReadOnly("COPY FROM");
	kingres_shard_refuse_replica_role("a distributed table on several shard groups");
	if (IsInParallelMode())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("COPY for several shard groups cannot run in a parallel worker")));

	pstate = make_parsestate(NULL);
	pstate->p_sourcetext = query_string;
	nsitem = addRangeTableEntryForRelation(pstate, rel, RowExclusiveLock, NULL, false, false);
	perminfo = nsitem->p_perminfo;
	perminfo->requiredPerms = ACL_INSERT;
	attnums = CopyGetAttnums(desc, rel, stmt->attlist);
	foreach(lc, attnums)
		perminfo->insertedCols = bms_add_member(perminfo->insertedCols,
												lfirst_int(lc) - FirstLowInvalidHeapAttributeNumber);
	ExecCheckPermissions(pstate->p_rtable, list_make1(perminfo), true);

	cat = kingres_shard_catalog();
	dt = hash_search(cat->dist_tables, &rel->rd_id, HASH_FIND, NULL);
	if (dt == NULL || dt->is_reference)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("table \"%s\" is not a hash-distributed table", RelationGetRelationName(rel))));
	placement = kingres_shard_placement_hash(dt->coloc);

	/*
	 * The columns that go to the groups: those the statement lists, and the columns of the
	 * table whose default must be computed here (a function, a sequence).  A constant default
	 * is left to the group (11.2); the key is always sent.
	 */
	for (attno = 1; attno <= desc->natts; attno++)
	{
		Form_pg_attribute att = TupleDescAttr(desc, attno - 1);
		KsBatchColumn *c;
		bool		send;

		if (att->attisdropped || att->attgenerated)
			continue;
		if (list_member_int(attnums, attno) || attno == dt->dist_attno)
			send = true;
		else
		{
			Node	   *def = build_column_default(rel, attno);

			send = (def != NULL && !IsA(def, Const));
			if (att->attidentity)
				send = true;
		}
		if (!send)
			continue;
		if (att->attidentity == ATTRIBUTE_IDENTITY_ALWAYS)
			overriding = true;
		c = palloc(sizeof(KsBatchColumn));
		c->name = pstrdup(NameStr(att->attname));
		c->type = att->atttypid;
		c->typmod = att->atttypmod;
		if (attno == dt->dist_attno)
			key_pos = list_length(columns);
		columns = lappend(columns, c);
		sent_attnos = lappend_int(sent_attnos, attno);
	}
	nsent = list_length(columns);
	if (key_pos < 0)
		elog(ERROR, "the distribution column is not among the columns of a COPY");

	kingres_shard_placement_check_plan(dt->colocation_id, placement);
	make_destination(&dest, &spec, dt, get_namespace_name(RelationGetNamespace(rel)),
					 RelationGetRelationName(rel), key_pos, placement, columns, overriding,
					 NULL, NULL, NULL, NULL);
	sender = kingres_shard_batch_begin(&dest, &spec);

	/* The parsing of every format and option, and the defaults, are PostgreSQL's own. */
	cstate = BeginCopyFrom(pstate, rel, NULL, stmt->filename, stmt->is_program, NULL,
						   stmt->attlist, stmt->options);
	estate = CreateExecutorState();
	econtext = GetPerTupleExprContext(estate);
	values = palloc0(sizeof(Datum) * desc->natts);
	nulls = palloc(sizeof(bool) * desc->natts);
	chunk_max = Min(kingres_shard_insert_batch_rows, 1000);
	chunkcxt = AllocSetContextCreate(CurrentMemoryContext, "kingres_shard copy chunk",
									 ALLOCSET_DEFAULT_SIZES);
	cvalues = palloc(sizeof(Datum) * nsent * chunk_max);
	cnulls = palloc(sizeof(bool) * nsent * chunk_max);
	rowvalues = palloc(sizeof(Datum) * nsent);
	rownulls = palloc(sizeof(bool) * nsent);

	while (!eof)
	{
		int			n = 0;
		int64		bytes = 0;
		int			nest;
		int			r;

		MemoryContextReset(chunkcxt);
		while (n < chunk_max && bytes < kingres_shard_insert_batch_bytes)
		{
			MemoryContext oldcxt;
			bool		ok;

			CHECK_FOR_INTERRUPTS();
			ResetPerTupleExprContext(estate);
			oldcxt = MemoryContextSwitchTo(GetPerTupleMemoryContext(estate));
			errcallback.callback = CopyFromErrorCallback;
			errcallback.arg = (void *) cstate;
			errcallback.previous = error_context_stack;
			error_context_stack = &errcallback;
			ok = NextCopyFrom(cstate, econtext, values, nulls);
			error_context_stack = errcallback.previous;
			if (!ok)
			{
				MemoryContextSwitchTo(oldcxt);
				eof = true;
				break;
			}
			MemoryContextSwitchTo(chunkcxt);
			j = 0;
			foreach(lc, sent_attnos)
			{
				int			a = lfirst_int(lc) - 1;
				Form_pg_attribute att = TupleDescAttr(desc, a);

				if (nulls[a])
				{
					cnulls[n * nsent + j] = true;
					cvalues[n * nsent + j] = (Datum) 0;
				}
				else
				{
					cnulls[n * nsent + j] = false;
					cvalues[n * nsent + j] = datumCopy(values[a], att->attbyval, att->attlen);
					bytes += datum_bytes(att, values[a]);
				}
				j++;
			}
			MemoryContextSwitchTo(oldcxt);
			n++;
		}
		nest = kingres_shard_set_transmission_modes();
		for (r = 0; r < n; r++)
		{
			memcpy(rowvalues, &cvalues[r * nsent], sizeof(Datum) * nsent);
			memcpy(rownulls, &cnulls[r * nsent], sizeof(bool) * nsent);
			kingres_shard_batch_add(sender, rowvalues, rownulls);
		}
		kingres_shard_reset_transmission_modes(nest);
		total += n;
	}

	(void) kingres_shard_batch_finish(sender);
	kingres_shard_batch_end(sender);
	EndCopyFrom(cstate);
	FreeExecutorState(estate);
	MemoryContextDelete(chunkcxt);
	free_parsestate(pstate);
	table_close(rel, NoLock);
	return total;
}
