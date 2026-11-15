/*-------------------------------------------------------------------------
 *
 * remote_scan.c
 *	  The "Kingres Remote Statement" custom scan: a plan with a single
 *	  CustomScan node that sends the deparsed statement to the primary of
 *	  the owning shard group and returns its result.
 *
 * The planner hook (planner.c) builds the plan with
 * kingres_shard_make_remote_plan() without running the standard planner:
 * the statement is deparsed with pg_get_querydef() and the plan carries only
 * the SQL text, the target node and the shape of the result.  At execution
 * the scan sends the statement with its parameters in text form over the
 * cached connection of node_conn.c, which joins the local transaction, and
 * converts the result rows with the input functions of the result column
 * types.  For INSERT, UPDATE and DELETE without RETURNING it reports the
 * affected row count.
 *
 * Stale maps.  If the remote node answers KS001 ("this bucket is not on
 * this node"), our bucket map was stale.  When the remote transaction has
 * run nothing else, the scan rolls it back, reloads the catalog from the map
 * group and runs the original statement again through SPI, which plans it
 * with the new map, once (design/shard-mvp.md, "Stale maps").  When the
 * transaction has run other statements the remote side cannot be rolled back
 * to just before this one, so the error is passed on.
 *
 * Plans that depend on a parameter whose value was not known at planning
 * time (a generic plan) become a "deferred" scan that raises an error when
 * started and has a huge cost, so that the plan cache only ever uses custom
 * plans for such statements.
 *

 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/remote_scan.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/parallel.h"
#include "access/xact.h"
#include "catalog/pg_type_d.h"
#include "commands/explain.h"
#include "commands/explain_format.h"
#include "executor/executor.h"
#include "executor/spi.h"
#include "executor/tuptable.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "nodes/extensible.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "nodes/params.h"
#include "optimizer/optimizer.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/tuplestore.h"

#include "catalog.h"
#include "kingres_shard.h"
#include "node_admin.h"
#include "node_conn.h"
#include "remote_scan.h"

/* Layout of CustomScan.custom_private. */
#define KS_PRIV_SQL			0	/* String: the statement to send */
#define KS_PRIV_HOST		1	/* String: target host */
#define KS_PRIV_PORT		2	/* Integer: target port */
#define KS_PRIV_GROUP		3	/* Integer: shard group */
#define KS_PRIV_BUCKET		4	/* Integer: bucket */
#define KS_PRIV_IS_WRITE	5	/* Integer: the statement writes */
#define KS_PRIV_DEFERRED	6	/* Integer: fails when started */
#define KS_PRIV_RETURNS_ROWS 7	/* Integer: result rows are returned */


/* Depth of retries in progress: a retried statement is not retried again. */
static int	retry_depth = 0;


typedef struct KsRemoteScanState
{
	CustomScanState css;
	char	   *sql;
	char	   *host;
	int			port;
	int			group_id;
	int			bucket;
	bool		is_write;
	bool		deferred;
	bool		returns_rows;
	bool		fetched;
	bool		counted;
	uint64		processed;
	int			ncols;
	Tuplestorestate *rows;
	MemoryContext rowcxt;
	FmgrInfo   *infuncs;
	Oid		   *typioparams;
} KsRemoteScanState;

static Node *ks_create_scan_state(CustomScan *cscan);
static void ks_begin_scan(CustomScanState *node, EState *estate, int eflags);
static TupleTableSlot *ks_exec_scan(CustomScanState *node);
static void ks_end_scan(CustomScanState *node);
static void ks_rescan(CustomScanState *node);
static void ks_explain_scan(CustomScanState *node, List *ancestors,
							ExplainState *es);

static const CustomScanMethods ks_scan_methods = {
	.CustomName = "Kingres Remote Statement",
	.CreateCustomScanState = ks_create_scan_state
};

static const CustomExecMethods ks_exec_methods = {
	.CustomName = "Kingres Remote Statement",
	.BeginCustomScan = ks_begin_scan,
	.ExecCustomScan = ks_exec_scan,
	.EndCustomScan = ks_end_scan,
	.ReScanCustomScan = ks_rescan,
	.ExplainCustomScan = ks_explain_scan
};

/*
 * kingres_shard_remote_scan_init
 *
 * Register the custom scan methods (needed to copy and read plans) and
 * nothing else is installed.
 */
void
kingres_shard_remote_scan_init(void)
{
	RegisterCustomScanMethods(&ks_scan_methods);
}

/*
 * Build the CustomScan node with the given private data and the result
 * columns of the statement.  The scan tuple and the output tuple are the
 * same: one column per non-junk entry of the target list, typed from the
 * query, so no projection is needed.
 */
CustomScan *
kingres_shard_build_scan(Query *parse, List *private, bool returns_rows)
{
	CustomScan *cscan = makeNode(CustomScan);
	List	   *src;
	List	   *scan_tlist = NIL;
	List	   *out_tlist = NIL;
	ListCell   *lc;
	int			resno = 0;

	src = parse->commandType == CMD_SELECT ? parse->targetList :
		parse->returningList;
	if (returns_rows)
	{
		foreach(lc, src)
		{
			TargetEntry *te = (TargetEntry *) lfirst(lc);
			Oid			type = exprType((Node *) te->expr);
			int32		typmod = exprTypmod((Node *) te->expr);
			Oid			coll = exprCollation((Node *) te->expr);

			if (te->resjunk)
				continue;
			resno++;
			/* A typed placeholder: only the type of the scan column counts. */
			scan_tlist = lappend(scan_tlist,
								 makeTargetEntry((Expr *) makeNullConst(type, typmod, coll),
												 resno,
												 te->resname ? pstrdup(te->resname) : NULL,
												 false));
			out_tlist = lappend(out_tlist,
								makeTargetEntry((Expr *) makeVar(INDEX_VAR, resno,
																 type, typmod, coll, 0),
												resno,
												te->resname ? pstrdup(te->resname) : NULL,
												false));
		}
	}

	cscan->scan.plan.targetlist = out_tlist;
	cscan->scan.plan.qual = NIL;
	cscan->scan.plan.startup_cost = 0;
	cscan->scan.plan.total_cost = 1000;
	cscan->scan.plan.plan_rows = 1000;
	cscan->scan.plan.plan_width = 32;
	cscan->scan.scanrelid = 0;
	cscan->flags = 0;
	cscan->custom_scan_tlist = scan_tlist;
	cscan->custom_private = private;
	cscan->methods = &ks_scan_methods;
	return cscan;
}

/*
 * Wrap a CustomScan into a PlannedStmt that has no range table: the remote
 * node checks permissions, and no local relation is opened.  The tables are
 * listed in relationOids so that a cached plan is invalidated when the bucket
 * map changes (the map version trigger invalidates their relcache entries).
 */
PlannedStmt *
kingres_shard_wrap_plan(Query *parse, CustomScan *cscan, List *relids, bool returns_rows)
{
	PlannedStmt *pstmt = makeNode(PlannedStmt);

	pstmt->commandType = parse->commandType;
	pstmt->queryId = parse->queryId;
	pstmt->hasReturning = (parse->returningList != NIL);
	pstmt->hasModifyingCTE = false;
	pstmt->canSetTag = parse->canSetTag;
	pstmt->transientPlan = false;
	pstmt->dependsOnRole = false;
	pstmt->parallelModeNeeded = false;
	pstmt->planTree = (Plan *) cscan;
	pstmt->rtable = NIL;
	pstmt->permInfos = NIL;
	KS_CLEAR_RESULT_RELATIONS(pstmt);
	pstmt->subplans = NIL;
	pstmt->relationOids = relids;
	pstmt->invalItems = NIL;
	pstmt->paramExecTypes = NIL;
	pstmt->utilityStmt = NULL;
	pstmt->stmt_location = parse->stmt_location;
	pstmt->stmt_len = parse->stmt_len;
	(void) returns_rows;
	return pstmt;
}

/*
 * kingres_shard_make_remote_plan
 *
 * The plan that forwards a statement to the primary of shard group group_id
 * at host:port.  sql is the deparsed statement.
 */
PlannedStmt *
kingres_shard_make_remote_plan(Query *parse, char *sql, const char *host,
								int port, int group_id, int bucket,
								bool is_write, List *relids)
{
	bool		returns_rows = (parse->commandType == CMD_SELECT ||
								parse->returningList != NIL);
	List	   *private;

	private = list_make4(makeString(sql), makeString(pstrdup(host)),
						 makeInteger(port), makeInteger(group_id));
	private = lappend(private, makeInteger(bucket));
	private = lappend(private, makeInteger(is_write ? 1 : 0));
	private = lappend(private, makeInteger(0));
	private = lappend(private, makeInteger(returns_rows ? 1 : 0));

	return kingres_shard_wrap_plan(parse, kingres_shard_build_scan(parse, private, returns_rows), relids,
					 returns_rows);
}

/*
 * kingres_shard_make_deferred_plan
 *
 * The plan for a statement whose routing depends on parameter values that are
 * not known (a generic plan).  It is never meant to run: starting it raises
 * an error, and its cost is so high that the plan cache keeps choosing a
 * custom plan, which has the values.
 */
PlannedStmt *
kingres_shard_make_deferred_plan(Query *parse, List *relids)
{
	List	   *private;
	CustomScan *cscan;

	private = list_make4(makeString(pstrdup("")), makeString(pstrdup("")),
						 makeInteger(0), makeInteger(0));
	private = lappend(private, makeInteger(0));
	private = lappend(private, makeInteger(0));
	private = lappend(private, makeInteger(1));
	private = lappend(private, makeInteger(0));

	cscan = kingres_shard_build_scan(parse, private, false);
	cscan->scan.plan.total_cost = 1.0e30;
	return kingres_shard_wrap_plan(parse, cscan, relids, false);
}

static Node *
ks_create_scan_state(CustomScan *cscan)
{
	KsRemoteScanState *s = palloc0(sizeof(KsRemoteScanState));
	List	   *p = cscan->custom_private;

	NodeSetTag(&s->css, T_CustomScanState);
	s->css.methods = &ks_exec_methods;
	/* Rows go through a tuplestore, which hands out minimal tuples. */
	s->css.slotOps = &TTSOpsMinimalTuple;

	s->sql = pstrdup(strVal(list_nth(p, KS_PRIV_SQL)));
	s->host = pstrdup(strVal(list_nth(p, KS_PRIV_HOST)));
	s->port = intVal(list_nth(p, KS_PRIV_PORT));
	s->group_id = intVal(list_nth(p, KS_PRIV_GROUP));
	s->bucket = intVal(list_nth(p, KS_PRIV_BUCKET));
	s->is_write = intVal(list_nth(p, KS_PRIV_IS_WRITE)) != 0;
	s->deferred = intVal(list_nth(p, KS_PRIV_DEFERRED)) != 0;
	s->returns_rows = intVal(list_nth(p, KS_PRIV_RETURNS_ROWS)) != 0;
	return (Node *) s;
}

/*
 * ks_begin_scan
 *
 * Nothing is sent yet; this checks that the statement may run and prepares
 * the input functions of the result columns.
 */
static void
ks_begin_scan(CustomScanState *node, EState *estate, int eflags)
{
	KsRemoteScanState *s = (KsRemoteScanState *) node;
	TupleDesc	tupdesc;
	int			i;

	if (eflags & EXEC_FLAG_EXPLAIN_ONLY)
		return;

	if (IsParallelWorker())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a statement for another shard group cannot run in a parallel worker"),
				 errhint("Mark the function that runs the statement PARALLEL RESTRICTED or PARALLEL UNSAFE.")));

	if (s->deferred)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a generic plan cannot route a statement on distributed tables"),
				 errhint("Set plan_cache_mode to force_custom_plan or auto, so that parameter values are known when the statement is planned.")));

	if (s->is_write && XactReadOnly)
		ereport(ERROR,
				(errcode(ERRCODE_READ_ONLY_SQL_TRANSACTION),
				 errmsg("cannot execute a write statement in a read-only transaction")));

	tupdesc = node->ss.ss_ScanTupleSlot->tts_tupleDescriptor;
	s->ncols = tupdesc->natts;
	s->infuncs = palloc0(sizeof(FmgrInfo) * Max(s->ncols, 1));
	s->typioparams = palloc0(sizeof(Oid) * Max(s->ncols, 1));
	for (i = 0; i < s->ncols; i++)
	{
		Oid			infunc;

		getTypeInputInfo(TupleDescAttr(tupdesc, i)->atttypid, &infunc,
						 &s->typioparams[i]);
		fmgr_info(infunc, &s->infuncs[i]);
	}
}

/*
 * The parameters of the statement in text form.  Types the remote side can
 * resolve by OID (built-in ones) are sent as such; the others are left for
 * the remote parser to infer.
 */
const char **
kingres_shard_params_to_text(ParamListInfo params, Oid **types_out, int *nparams_out)
{
	int			n = params ? params->numParams : 0;
	const char **values = palloc0(sizeof(char *) * Max(n, 1));
	Oid		   *types = palloc0(sizeof(Oid) * Max(n, 1));
	int			nest;
	int			i;

	nest = kingres_shard_set_transmission_modes();
	for (i = 0; i < n; i++)
	{
		ParamExternData *prm;
		ParamExternData workspace;
		Oid			outfunc;
		bool		isvarlena;

		if (params->paramFetch != NULL)
			prm = params->paramFetch(params, i + 1, false, &workspace);
		else
			prm = &params->params[i];

		/*
		 * A parameter that the statement does not use may come with no type
		 * (PL/pgSQL offers every variable of the function as a parameter and
		 * returns a dummy for those the statement does not mention); the
		 * remote parser cannot infer the type of an unreferenced parameter,
		 * so give it one.  The value is NULL.
		 */
		if (!OidIsValid(prm->ptype))
		{
			types[i] = TEXTOID;
			continue;
		}
		if (prm->ptype < FirstNormalObjectId)
			types[i] = prm->ptype;
		if (prm->isnull)
			continue;
		getTypeOutputInfo(prm->ptype, &outfunc, &isvarlena);
		values[i] = OidOutputFunctionCall(outfunc, prm->value);
	}
	kingres_shard_reset_transmission_modes(nest);

	*types_out = types;
	*nparams_out = n;
	return values;
}

/*
 * Convert the rows of a remote result into the tuplestore.  The PGresult is
 * cleared whether or not a value fails to convert.
 */
static void
store_result(KsRemoteScanState *s, PGresult *res)
{
	TupleDesc	tupdesc = s->css.ss.ss_ScanTupleSlot->tts_tupleDescriptor;
	Datum	   *values = palloc(sizeof(Datum) * Max(s->ncols, 1));
	bool	   *nulls = palloc(sizeof(bool) * Max(s->ncols, 1));

	PG_TRY();
	{
		int			ntup = PQntuples(res);
		int			i;
		int			j;

		if (PQnfields(res) != s->ncols)
			ereport(ERROR,
					(errcode(ERRCODE_DATA_EXCEPTION),
					 errmsg("remote node returned %d columns, expected %d",
							PQnfields(res), s->ncols)));

		for (i = 0; i < ntup; i++)
		{
			MemoryContext old;

			MemoryContextReset(s->rowcxt);
			old = MemoryContextSwitchTo(s->rowcxt);
			for (j = 0; j < s->ncols; j++)
			{
				if (PQgetisnull(res, i, j))
				{
					values[j] = (Datum) 0;
					nulls[j] = true;
				}
				else
				{
					nulls[j] = false;
					values[j] = InputFunctionCall(&s->infuncs[j],
												  PQgetvalue(res, i, j),
												  s->typioparams[j],
												  TupleDescAttr(tupdesc, j)->atttypmod);
				}
			}
			MemoryContextSwitchTo(old);
			tuplestore_putvalues(s->rows, tupdesc, values, nulls);
		}
		s->processed = strtoull(PQcmdTuples(res), NULL, 10);
	}
	PG_FINALLY();
	{
		PQclear(res);
	}
	PG_END_TRY();
}

/*
 * retry_through_spi
 *
 * Run the statement again from this node, after the catalog was reloaded, so
 * that it is planned with the new bucket map (which may forward it to
 * another group, or run it locally).  Rows and the processed count are
 * stored as if they had come from the remote node.
 */
static void
retry_through_spi(KsRemoteScanState *s, ParamListInfo params)
{
	int			n = params ? params->numParams : 0;
	Oid		   *argtypes = palloc(sizeof(Oid) * Max(n, 1));
	Datum	   *values = palloc(sizeof(Datum) * Max(n, 1));
	char	   *nulls = palloc(Max(n, 1));
	int			i;
	int			ret;
	int			nest;

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

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	retry_depth++;

	/*
	 * The statement text was deparsed under the transmission modes (ISO
	 * dates, standard-conforming strings, ...), so it must be parsed under
	 * them again, whatever this session's settings are.
	 */
	nest = kingres_shard_set_transmission_modes();
	PG_TRY();
	{
		ret = SPI_execute_with_args(s->sql, n, argtypes, values, nulls,
									!s->is_write, 0);
	}
	PG_FINALLY();
	{
		retry_depth--;
	}
	PG_END_TRY();
	kingres_shard_reset_transmission_modes(nest);
	if (ret < 0)
		elog(ERROR, "retry of a forwarded statement failed (%d)", ret);

	s->processed = SPI_processed;
	if (s->returns_rows)
	{
		uint64		k;

		if (SPI_tuptable == NULL || SPI_tuptable->tupdesc->natts != s->ncols)
			elog(ERROR, "unexpected result shape on retry of a forwarded statement");
		for (k = 0; k < SPI_processed; k++)
			tuplestore_puttuple(s->rows, SPI_tuptable->vals[k]);
	}
	SPI_finish();
}

/*
 * fetch_remote
 *
 * Send the statement and collect the result.  See the file header for the
 * stale-map retry.
 */
static void
fetch_remote(KsRemoteScanState *s)
{
	EState	   *estate = s->css.ss.ps.state;
	ParamListInfo params = estate->es_param_list_info;
	KsConn	   *kc;
	bool		fresh;
	const char **values;
	Oid		   *types;
	int			nparams;
	PGresult   *res;
	MemoryContext old;

	old = MemoryContextSwitchTo(estate->es_query_cxt);
	s->rows = tuplestore_begin_heap(false, false, work_mem);
	s->rowcxt = AllocSetContextCreate(estate->es_query_cxt,
									  "kingres_shard remote rows",
									  ALLOCSET_DEFAULT_SIZES);
	MemoryContextSwitchTo(old);

	kc = kingres_shard_get_conn(s->host, s->port, false, NULL);
	if (s->is_write)
		kingres_shard_note_remote_write(kc);
	kingres_shard_begin_xact(kc);
	fresh = (kc->nstatements == 0);

	values = kingres_shard_params_to_text(params, &types, &nparams);
	res = kingres_shard_exec_params(kc, s->sql, nparams, types, values, false);

	if (res != NULL && PQresultStatus(res) != PGRES_COMMAND_OK &&
		PQresultStatus(res) != PGRES_TUPLES_OK)
	{
		if (kingres_shard_result_sqlstate(res) == ERRCODE_KINGRES_STALE_MAP &&
			fresh && retry_depth == 0)
		{
			PGresult   *rb;

			PQclear(res);
			/* Nothing ran before this statement: undo the remote transaction. */
			rb = kingres_shard_exec(kc, "ROLLBACK TRANSACTION");
			PQclear(rb);
			kingres_shard_forget_xact(kc);

			if (kingres_shard_reload_catalog())
			{
				retry_through_spi(s, params);
				return;
			}
			/*
			 * The map group primary has the authoritative catalog, but a switch of a
			 * bucket move commits here before the group that gave the bucket up has
			 * heard of it, and a statement planned just before the commit finds the
			 * old group already changed (design/shard-phase4.md, the map version
			 * rule).  The catalog has moved on: plan the statement again, once.
			 */
			kingres_shard_catalog_invalidate();
			retry_through_spi(s, params);
			return;
		}
		kingres_shard_report_error(kc, res, s->sql);
	}

	if (s->returns_rows)
		store_result(s, res);
	else
	{
		s->processed = strtoull(PQcmdTuples(res), NULL, 10);
		PQclear(res);
	}
}

static TupleTableSlot *
ks_exec_scan(CustomScanState *node)
{
	KsRemoteScanState *s = (KsRemoteScanState *) node;
	TupleTableSlot *slot = node->ss.ss_ScanTupleSlot;

	if (!s->fetched)
	{
		fetch_remote(s);
		s->fetched = true;
	}

	if (!s->returns_rows)
	{
		/* The executor only counts rows it sends; report the affected rows. */
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

	/*
	 * The executor counts the rows of a SELECT itself; for INSERT, UPDATE
	 * and DELETE ... RETURNING the node that does the writing must count
	 * them (see ExecutePlan), and here that is this node.
	 */
	if (node->ss.ps.state->es_plannedstmt->commandType != CMD_SELECT)
		node->ss.ps.state->es_processed++;
	return slot;
}

static void
ks_end_scan(CustomScanState *node)
{
	KsRemoteScanState *s = (KsRemoteScanState *) node;

	if (s->rows != NULL)
		tuplestore_end(s->rows);
	s->rows = NULL;
	if (s->rowcxt != NULL)
		MemoryContextDelete(s->rowcxt);
	s->rowcxt = NULL;
}

/*
 * A rescan of a read runs the statement again, because the parameters may
 * have changed.  A write is never rescanned.
 */
static void
ks_rescan(CustomScanState *node)
{
	KsRemoteScanState *s = (KsRemoteScanState *) node;

	if (s->is_write)
		return;
	ks_end_scan(node);
	s->fetched = false;
}

static void
ks_explain_scan(CustomScanState *node, List *ancestors, ExplainState *es)
{
	KsRemoteScanState *s = (KsRemoteScanState *) node;
	char	   *target;

	if (s->deferred)
	{
		ExplainPropertyText("Routing", "deferred until parameter values are known", es);
		return;
	}
	target = psprintf("%s:%d (group %d, bucket %d)", s->host, s->port,
					  s->group_id, s->bucket);
	ExplainPropertyText("Node", target, es);

	/*
	 * The deparsed statement has line breaks; show it on one line.  This is
	 * for display only, the statement that is sent is unchanged.
	 */
	{
		StringInfoData flat;
		const char *p;
		bool		pending_space = false;

		initStringInfo(&flat);
		for (p = s->sql; *p; p++)
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
		ExplainPropertyText("Remote SQL", flat.data, es);
	}
}
