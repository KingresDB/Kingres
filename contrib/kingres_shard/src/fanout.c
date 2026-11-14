/*-------------------------------------------------------------------------
 *
 * fanout.c
 *	  The "Kingres Fan-out Write" custom scan.
 *
 * Two kinds of statement are executed on several nodes at once, in the
 * distributed transaction of the session (two-phase commit makes them atomic):
 *
 *	- a write to a reference table (every node holds the whole table), executed
 *	  on every active primary node (design/shard-phase2a.md, section 3);
 *	- an UPDATE or DELETE of a distributed table that is not restricted to one
 *	  bucket and can be shipped whole, executed on every shard group that owns
 *	  buckets of the table (section 6).
 *
 * The planner (planner.c) has already checked that the statement can be
 * shipped (shippable.c) and deparsed it under the transmission modes.  This
 * node runs the text on each target in a fixed order, node after node, the
 * local one through SPI: a fixed order makes concurrent statements take their
 * row locks in the same order on every node, so they cannot deadlock on each
 * other across nodes (a deadlock of several statements in several transactions
 * is for the distributed deadlock detection of phase 5).
 *
 * While the local part runs through SPI the planner is told to plan the
 * statement for this node only (kingres_shard_in_fanout_local()); a remote
 * node runs it in a forwarded session, where the same holds.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/fanout.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

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
#include "nodes/params.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/tuplestore.h"

#include "fanout.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "remote_scan.h"

/* Layout of CustomScan.custom_private: three fields, then (host, port, local) per target. */
#define FO_PRIV_SQL			0
#define FO_PRIV_RETURNS		1
#define FO_PRIV_ALL_ROWS	2
#define FO_PRIV_TARGETS		3

/* The statement text whose local execution is in progress, or NULL. */
static const char *fanout_local_sql = NULL;

typedef struct KsFanoutState
{
	CustomScanState css;
	char	   *sql;
	bool		returns_rows;
	bool		all_rows;
	List	   *targets;		/* of KsFanTarget * */
	bool		done;
	bool		counted;
	uint64		processed;
	int			ncols;
	Tuplestorestate *rows;
	MemoryContext rowcxt;
	FmgrInfo   *infuncs;
	Oid		   *typioparams;
} KsFanoutState;

static Node *fo_create_scan_state(CustomScan *cscan);
static void fo_begin_scan(CustomScanState *node, EState *estate, int eflags);
static TupleTableSlot *fo_exec_scan(CustomScanState *node);
static void fo_end_scan(CustomScanState *node);
static void fo_rescan(CustomScanState *node);
static void fo_explain_scan(CustomScanState *node, List *ancestors,
							ExplainState *es);

static const CustomScanMethods fo_scan_methods = {
	.CustomName = "Kingres Fan-out Write",
	.CreateCustomScanState = fo_create_scan_state
};

static const CustomExecMethods fo_exec_methods = {
	.CustomName = "Kingres Fan-out Write",
	.BeginCustomScan = fo_begin_scan,
	.ExecCustomScan = fo_exec_scan,
	.EndCustomScan = fo_end_scan,
	.ReScanCustomScan = fo_rescan,
	.ExplainCustomScan = fo_explain_scan
};

void
kingres_shard_fanout_init(void)
{
	RegisterCustomScanMethods(&fo_scan_methods);
}

bool
kingres_shard_in_fanout_local(const char *query_string)
{
	return fanout_local_sql != NULL && query_string != NULL &&
		strcmp(fanout_local_sql, query_string) == 0;
}

PlannedStmt *
kingres_shard_make_fanout_plan(Query *parse, char *sql, List *targets,
								bool all_rows, List *relids)
{
	bool		returns_rows = (parse->returningList != NIL);
	List	   *private;
	ListCell   *lc;
	CustomScan *cscan;

	private = list_make3(makeString(sql), makeInteger(returns_rows ? 1 : 0),
						 makeInteger(all_rows ? 1 : 0));
	foreach(lc, targets)
	{
		KsFanTarget *t = (KsFanTarget *) lfirst(lc);

		private = lappend(private, makeString(pstrdup(t->host)));
		private = lappend(private, makeInteger(t->port));
		private = lappend(private, makeInteger(t->is_local ? 1 : 0));
	}
	cscan = kingres_shard_build_scan(parse, private, returns_rows);
	cscan->methods = &fo_scan_methods;
	return kingres_shard_wrap_plan(parse, cscan, relids, returns_rows);
}

static Node *
fo_create_scan_state(CustomScan *cscan)
{
	KsFanoutState *s = palloc0(sizeof(KsFanoutState));
	List	   *p = cscan->custom_private;
	int			i;

	NodeSetTag(&s->css, T_CustomScanState);
	s->css.methods = &fo_exec_methods;
	s->css.slotOps = &TTSOpsMinimalTuple;

	s->sql = pstrdup(strVal(list_nth(p, FO_PRIV_SQL)));
	s->returns_rows = intVal(list_nth(p, FO_PRIV_RETURNS)) != 0;
	s->all_rows = intVal(list_nth(p, FO_PRIV_ALL_ROWS)) != 0;
	for (i = FO_PRIV_TARGETS; i + 2 < list_length(p); i += 3)
	{
		KsFanTarget *t = palloc(sizeof(KsFanTarget));

		t->host = pstrdup(strVal(list_nth(p, i)));
		t->port = intVal(list_nth(p, i + 1));
		t->is_local = intVal(list_nth(p, i + 2)) != 0;
		s->targets = lappend(s->targets, t);
	}
	return (Node *) s;
}

static void
fo_begin_scan(CustomScanState *node, EState *estate, int eflags)
{
	KsFanoutState *s = (KsFanoutState *) node;
	TupleDesc	tupdesc;
	int			i;

	if (eflags & EXEC_FLAG_EXPLAIN_ONLY)
		return;
	if (XactReadOnly)
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

/* Rows of a remote result into the tuplestore; the result is cleared. */
static void
store_remote_rows(KsFanoutState *s, PGresult *res)
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
	}
	PG_FINALLY();
	{
		PQclear(res);
	}
	PG_END_TRY();
}

/*
 * Run the statement on this node through SPI, with the planner told that the
 * statement is already the local part of a fanned-out one.  The rows are kept
 * when keep_rows; the number of rows affected is returned.
 */
static uint64
run_local(KsFanoutState *s, ParamListInfo params, bool keep_rows)
{
	int			n = params ? params->numParams : 0;
	Oid		   *argtypes = palloc(sizeof(Oid) * Max(n, 1));
	Datum	   *values = palloc(sizeof(Datum) * Max(n, 1));
	char	   *nulls = palloc(Max(n, 1));
	int			i;
	int			ret;
	int			nest;
	uint64		processed;
	const char *saved = fanout_local_sql;

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
	nest = kingres_shard_set_transmission_modes();
	fanout_local_sql = s->sql;
	PG_TRY();
	{
		ret = SPI_execute_with_args(s->sql, n, argtypes, values, nulls, false, 0);
	}
	PG_FINALLY();
	{
		fanout_local_sql = saved;
	}
	PG_END_TRY();
	kingres_shard_reset_transmission_modes(nest);
	if (ret < 0)
		elog(ERROR, "local part of a fanned-out statement failed (%d)", ret);

	processed = SPI_processed;
	if (keep_rows && s->returns_rows)
	{
		uint64		k;

		if (SPI_tuptable == NULL || SPI_tuptable->tupdesc->natts != s->ncols)
			elog(ERROR, "unexpected result shape of a fanned-out statement");
		for (k = 0; k < SPI_processed; k++)
			tuplestore_puttuple(s->rows, SPI_tuptable->vals[k]);
	}
	SPI_finish();
	return processed;
}

static void
run_all(KsFanoutState *s)
{
	EState	   *estate = s->css.ss.ps.state;
	ParamListInfo params = estate->es_param_list_info;
	MemoryContext old;
	ListCell   *lc;
	uint64		first_count = 0;
	bool		have_first = false;
	uint64		local_count = 0;
	bool		have_local = false;
	uint64		total = 0;

	if (IsInParallelMode())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a statement for several nodes cannot run in a parallel worker")));

	old = MemoryContextSwitchTo(estate->es_query_cxt);
	s->rows = tuplestore_begin_heap(false, false, work_mem);
	s->rowcxt = AllocSetContextCreate(estate->es_query_cxt,
									  "kingres_shard fanout rows",
									  ALLOCSET_DEFAULT_SIZES);
	MemoryContextSwitchTo(old);

	foreach(lc, s->targets)
	{
		KsFanTarget *t = (KsFanTarget *) lfirst(lc);
		uint64		count;

		if (t->is_local)
		{
			count = run_local(s, params, true);
			local_count = count;
			have_local = true;
		}
		else
		{
			KsConn	   *kc = kingres_shard_get_conn(t->host, t->port, false, NULL);
			const char **values;
			Oid		   *types;
			int			nparams;
			PGresult   *res;

			kingres_shard_begin_write_xact(kc);
			values = kingres_shard_params_to_text(params, &types, &nparams);
			res = kingres_shard_exec_params(kc, s->sql, nparams, types, values, true);
			count = strtoull(PQcmdTuples(res), NULL, 10);
			if (s->all_rows && s->returns_rows)
				store_remote_rows(s, res);
			else
				PQclear(res);
		}

		if (!have_first)
		{
			first_count = count;
			have_first = true;
		}
		else if (!s->all_rows && count != first_count)
			ereport(ERROR,
					(errcode(ERRCODE_DATA_CORRUPTED),
					 errmsg("the copies of a reference table differ"),
					 errdetail("The statement affected %llu rows on one node and %llu on another.",
							   (unsigned long long) first_count,
							   (unsigned long long) count),
					 errhint("Compare the table on the nodes; kingres.create_reference_table() copies it again only on a new node.")));
		total += count;
	}
	(void) have_local;

	/* A reference table's result is the local one; a distributed one's is the sum. */
	s->processed = s->all_rows ? total : (have_local ? local_count : first_count);
}

static TupleTableSlot *
fo_exec_scan(CustomScanState *node)
{
	KsFanoutState *s = (KsFanoutState *) node;
	TupleTableSlot *slot = node->ss.ss_ScanTupleSlot;

	if (!s->done)
	{
		run_all(s);
		s->done = true;
	}

	if (!s->returns_rows)
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
fo_end_scan(CustomScanState *node)
{
	KsFanoutState *s = (KsFanoutState *) node;

	if (s->rows != NULL)
		tuplestore_end(s->rows);
	s->rows = NULL;
	if (s->rowcxt != NULL)
		MemoryContextDelete(s->rowcxt);
	s->rowcxt = NULL;
}

/* A write is never run twice. */
static void
fo_rescan(CustomScanState *node)
{
}

static void
fo_explain_scan(CustomScanState *node, List *ancestors, ExplainState *es)
{
	KsFanoutState *s = (KsFanoutState *) node;
	StringInfoData flat;
	const char *p;
	bool		pending_space = false;

	ExplainPropertyInteger("Nodes", NULL, list_length(s->targets), es);
	ExplainPropertyText("Result", s->all_rows ? "sum over the nodes" : "reference table: copies must agree", es);

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
