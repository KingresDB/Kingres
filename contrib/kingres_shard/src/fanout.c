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

#include "access/parallel.h"
#include "access/xact.h"
#include "catalog/namespace.h"
#include "catalog/pg_type_d.h"
#include "commands/extension.h"
#include "catalog.h"
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
#include "parser/parsetree.h"
#include "storage/lmgr.h"
#include "storage/lock.h"
#include "storage/proc.h"
#include "tcop/tcopprot.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/inval.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/tuplestore.h"
#include "utils/varlena.h"

#include "fanout.h"
#include "forward_auth.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "remote_scan.h"

/* Layout of CustomScan.custom_private: three fields, then (host, port, local) per target. */
#define FO_PRIV_SQL			0
#define FO_PRIV_RETURNS		1
#define FO_PRIV_ALL_ROWS	2
#define FO_PRIV_COLOC		3	/* Integer: colocation group of the table (0: a reference table) */
#define FO_PRIV_PLACEMENT	4	/* String: its placement when the statement was planned */
#define FO_PRIV_NLOCKS		5
/* then the lock names, then (host, port, local) per target */

/* The statement text whose local execution is in progress, or NULL. */
static const char *fanout_local_sql = NULL;

/*
 * How many local parts of a fanned-out reference-table write are running in
 * this backend.  The statements those run (the actions of foreign keys, and
 * whatever else the write fires) are part of the write that every node runs
 * for itself, so they act on this node's copy only.
 */
static int	refwrite_local_depth = 0;

static ExecutorStart_hook_type prev_ExecutorStart = NULL;

static void fo_executor_start(QueryDesc *queryDesc, int eflags);

typedef struct KsFanoutState
{
	CustomScanState css;
	char	   *sql;
	bool		returns_rows;
	bool		all_rows;
	int			colocation_id;	/* of the table, 0 for a reference table */
	int64		plan_placement;	/* its placement when the targets were chosen */
	List	   *targets;		/* of KsFanTarget * */
	List	   *locknames;		/* reference tables to lock on every node first */
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
	prev_ExecutorStart = ExecutorStart_hook;
	ExecutorStart_hook = fo_executor_start;
}

bool
kingres_shard_in_fanout_local(const char *query_string)
{
	return fanout_local_sql != NULL && query_string != NULL &&
		strcmp(fanout_local_sql, query_string) == 0;
}

/*
 * True when the statement of the top-level query text of this backend is the
 * fragment of a reference-table write that another node runs on every node.
 */
static bool
in_refwrite_fragment(void)
{
	return kingres_shard_session_forwarded() && debug_query_string != NULL &&
		strncmp(debug_query_string, KS_REFWRITE_MARKER, strlen(KS_REFWRITE_MARKER)) == 0;
}

/*
 * Is the text one of the statements that the triggers of foreign keys run
 * (ri_triggers.c: every one of them names its table with ONLY)?  Only these are
 * left to this node's copy while a reference-table write runs; any other
 * statement that the write makes run (review finding S5-01: a trigger or a
 * function) is planned like a statement of its own, routed and checked.  There
 * is no trigger on a reference table by hand (utility.c); the rest is the
 * foreign keys.
 */
static bool
is_ri_action(const char *query_string)
{
	return strncmp(query_string, "SELECT 1 FROM ONLY ", 19) == 0 ||
		strncmp(query_string, "UPDATE ONLY ", 12) == 0 ||
		strncmp(query_string, "DELETE FROM ONLY ", 17) == 0;
}

/*
 * kingres_shard_in_refwrite_nested
 *
 * True for a statement planned while a reference-table write runs on this node
 * as one of its copies, other than that write itself: the action of a foreign
 * key, a trigger.  It must run on this node's copy only, because the write is
 * run on every node and each node does its own part of it.  (The planner then
 * leaves the statement alone.)
 */
bool
kingres_shard_in_refwrite_nested(const char *query_string)
{
	if (query_string == NULL || !is_ri_action(query_string))
		return false;
	if (refwrite_local_depth > 0)
		return true;
	return in_refwrite_fragment() &&
		debug_query_string != NULL && strcmp(query_string, debug_query_string) != 0;
}

/*
 * fo_executor_start
 *
 * The rule of reference tables: every copy is written, or none.  Whatever path
 * a write takes (a function called by a statement, a trigger, a rule, a cached
 * plan, a statement that runs for another node), the planner hook is not
 * always the one that decides: a plan can be older than the table's being a
 * reference table, and a function that a routed statement calls runs on
 * another node, in a session that never fans a write out.  So the decision is
 * checked where it cannot be bypassed, when a plan that modifies a table starts
 * to run (review findings S3-01 and S3-02).  A modification of a reference
 * table is allowed only
 *
 *	- in the local part of a write this node fanned out (refwrite_local_depth),
 *	  or in a session another node opened to run its fragment of such a write
 *	  (the marker that begins the statement text), where each node does its
 *	  own part;
 *	- when the cluster has a single shard group and the session is not
 *	  forwarded: there is no other copy;
 *	- when routing is switched off (superuser, by hand), or while the extension
 *	  is created.
 *
 * Everything else fails before the first row is changed.  A plan of a
 * statement that fans the write out has no ModifyTable node (it is a custom
 * scan that runs the statement on the nodes), so it never gets here.
 */
static void
fo_executor_start(QueryDesc *queryDesc, int eflags)
{
	PlannedStmt *pstmt = queryDesc->plannedstmt;
	List	   *result_rels = ks_result_relations(pstmt);

	if (result_rels != NIL && !(eflags & EXEC_FLAG_EXPLAIN_ONLY) &&
		kingres_shard_enable_routing && !creating_extension &&
		refwrite_local_depth == 0 && !IsParallelWorker() && !in_refwrite_fragment())
	{
		const KsCatalog *cat = kingres_shard_catalog();
		ListCell   *lc;

		if (cat->present && cat->dist_tables != NULL && cat->ndist_tables > 0 &&
			(cat->ngroups > 1 || kingres_shard_session_forwarded()))
		{
			foreach(lc, result_rels)
			{
				RangeTblEntry *rte = rt_fetch(lfirst_int(lc), pstmt->rtable);
				KsDistTable *dt;

				if (rte->rtekind != RTE_RELATION)
					continue;
				dt = hash_search(cat->dist_tables, &rte->relid, HASH_FIND, NULL);
				if (dt == NULL || !dt->is_reference)
					continue;
				if (kingres_shard_session_forwarded())
					ereport(ERROR,
							(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
							 errmsg("reference table \"%s\" cannot be written from a statement that is run for another node",
									get_rel_name(rte->relid)),
							 errdetail("A function, trigger or rule that a routed statement runs on this node would change this node's copy only, and the copies of a reference table must stay equal."),
							 errhint("Run the write to the reference table as a statement of its own, from the session that started the work.")));
				ereport(ERROR,
						(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
						 errmsg("reference table \"%s\" cannot be written by this plan, which would change this node's copy only",
								get_rel_name(rte->relid)),
						 errdetail("The plan was made before the table became a reference table or before the cluster had several nodes."),
						 errhint("Repeat the statement; it is planned again.")));
			}
		}
	}
	if (prev_ExecutorStart)
		prev_ExecutorStart(queryDesc, eflags);
	else
		standard_ExecutorStart(queryDesc, eflags);
}

PlannedStmt *
kingres_shard_make_fanout_plan(Query *parse, char *sql, List *targets,
								bool all_rows, List *relids, List *locknames)
{
	bool		returns_rows = (parse->returningList != NIL);
	List	   *private;
	ListCell   *lc;
	CustomScan *cscan;
	int			colocation_id = 0;

	/* The colocation group of the distributed table the statement writes (none for a reference table). */
	foreach(lc, relids)
	{
		const KsDistTable *dt = kingres_shard_lookup_dist_table(lfirst_oid(lc));

		if (dt != NULL && !dt->is_reference)
		{
			colocation_id = dt->colocation_id;
			break;
		}
	}

	private = list_make3(makeString(sql), makeInteger(returns_rows ? 1 : 0),
						 makeInteger(all_rows ? 1 : 0));
	private = lappend(private, makeInteger(colocation_id));
	private = lappend(private, makeString(psprintf(INT64_FORMAT,
												   kingres_shard_placement_hash(kingres_shard_lookup_colocation(colocation_id)))));
	private = lappend(private, makeInteger(list_length(locknames)));
	foreach(lc, locknames)
		private = lappend(private, makeString(pstrdup((char *) lfirst(lc))));
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
	int			nlocks;

	NodeSetTag(&s->css, T_CustomScanState);
	s->css.methods = &fo_exec_methods;
	s->css.slotOps = &TTSOpsMinimalTuple;

	s->sql = pstrdup(strVal(list_nth(p, FO_PRIV_SQL)));
	s->returns_rows = intVal(list_nth(p, FO_PRIV_RETURNS)) != 0;
	s->all_rows = intVal(list_nth(p, FO_PRIV_ALL_ROWS)) != 0;
	s->colocation_id = intVal(list_nth(p, FO_PRIV_COLOC));
	s->plan_placement = pg_strtoint64(strVal(list_nth(p, FO_PRIV_PLACEMENT)));
	nlocks = intVal(list_nth(p, FO_PRIV_NLOCKS));
	for (i = 0; i < nlocks; i++)
		s->locknames = lappend(s->locknames, pstrdup(strVal(list_nth(p, FO_PRIV_NLOCKS + 1 + i))));
	for (i = FO_PRIV_NLOCKS + 1 + nlocks; i + 2 < list_length(p); i += 3)
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
	if (IsParallelWorker())
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a write to several nodes cannot run in a parallel worker"),
				 errhint("Mark the function that runs the statement PARALLEL RESTRICTED or PARALLEL UNSAFE.")));
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
	if (!s->all_rows)
		refwrite_local_depth++;
	PG_TRY();
	{
		ret = SPI_execute_with_args(s->sql, n, argtypes, values, nulls, false, 0);
	}
	PG_FINALLY();
	{
		fanout_local_sql = saved;
		if (!s->all_rows)
			refwrite_local_depth--;
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

static int
compare_node_ids(const ListCell *a, const ListCell *b)
{
	return ((KsNode *) lfirst(a))->node_id - ((KsNode *) lfirst(b))->node_id;
}

static int
compare_names(const ListCell *a, const ListCell *b)
{
	return strcmp((char *) lfirst(a), (char *) lfirst(b));
}

/* The active primary nodes of the catalog, in node id order. */
static List *
active_primaries(const KsCatalog *cat)
{
	List	   *result = NIL;
	ListCell   *lc;

	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);

		if (n->is_active && n->is_primary)
			result = lappend(result, n);
	}
	list_sort(result, compare_node_ids);
	return result;
}

/*
 * kingres_shard_lock_all_reference_tables
 *
 * For add_node() (review finding S2-07): take, on every active primary node
 * and for every reference table, the lock that writers of the table take (see
 * lock_reference_tables), so that the copies made on the new node include
 * every committed write and a writer that has planned its statement with the
 * old list of nodes waits until the registration is committed (and then finds
 * that the list changed, see check_node_set).  Held to the end of the
 * transaction.
 */
void
kingres_shard_lock_all_reference_tables(void)
{
	const KsCatalog *cat = kingres_shard_catalog();
	List	   *names = NIL;
	List	   *nodes;
	ListCell   *lc;
	HASH_SEQ_STATUS scan;
	KsDistTable *dt;

	if (cat->dist_tables == NULL)
		return;
	hash_seq_init(&scan, cat->dist_tables);
	while ((dt = (KsDistTable *) hash_seq_search(&scan)) != NULL)
	{
		if (dt->is_reference)
			names = lappend(names,
							quote_qualified_identifier(get_namespace_name(get_rel_namespace(dt->relid)),
													   get_rel_name(dt->relid)));
	}
	if (names == NIL)
		return;
	list_sort(names, compare_names);
	nodes = active_primaries(cat);
	foreach(lc, nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		ListCell   *lc2;

		if (n->node_id == cat->local_node_id)
		{
			foreach(lc2, names)
				kingres_shard_lock_reference_table((char *) lfirst(lc2));
		}
		else
		{
			KsConn	   *kc = kingres_shard_get_conn(n->host, n->port, false, NULL);

			kingres_shard_begin_write_xact(kc);
			foreach(lc2, names)
				PQclear(kingres_shard_exec(kc, kingres_shard_refwrite_lock_sql((char *) lfirst(lc2))));
		}
	}
}

/*
 * After the locks of a reference-table write are held, the nodes the statement
 * was planned for must still be the nodes of the cluster: if an add_node()
 * committed while the statement waited, a write to the old list would miss the
 * new node.
 */
static void
check_node_set(KsFanoutState *s)
{
	List	   *current = active_primaries(kingres_shard_catalog());
	ListCell   *lc;
	bool		same = (list_length(current) == list_length(s->targets));

	foreach(lc, current)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		ListCell   *lc2;
		bool		found = false;

		foreach(lc2, s->targets)
		{
			KsFanTarget *t = (KsFanTarget *) lfirst(lc2);

			if (t->port == n->port && strcmp(t->host, n->host) == 0)
				found = true;
		}
		if (!found)
			same = false;
	}
	if (!same)
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("the set of nodes changed while a write to a reference table waited"),
				 errhint("Repeat the statement.")));
}

/*
 * The context of the error that ends a wait for the lock (lock_timeout, the
 * deadlock detector, a cancel): which table the writer waited for, and what
 * can hold it.
 */
static void
lock_wait_error_context(void *arg)
{
	errcontext("while waiting for the writer lock of reference table \"%s\", which another writer (possibly a prepared transaction that waits for its origin) holds on this node",
			   (const char *) arg);
}

/*
 * kingres_shard_lock_reference_table
 *
 * Make a writer of the reference table named by the qualified name wait for
 * the other writers of it, on this node, until the end of the transaction.
 *
 * The lock is a heavyweight lock on an object of a class of its own
 * (KS_REFWRITE_LOCK_CLASS, which no catalog has) and the OID of the table on
 * this node.  No role can take it: SQL reaches advisory locks, relation locks
 * and the locks of the objects that DDL names, never a lock of this class, and
 * the function that takes it for another node answers only superusers and
 * sessions that proved they come from a node of the cluster.  (An advisory
 * lock, as 0.2 used, is in a key space every role shares: any role could hold
 * the key of a table and stop all writers of it, review finding S3-03.)  Not
 * LOCK TABLE either: a mode that conflicts with itself needs the UPDATE,
 * DELETE or TRUNCATE privilege, which a role that may only INSERT does not
 * have.  Readers are never blocked.
 *
 * The wait is bounded: by the session's lock_timeout when it has one, and in
 * any case by kingres_shard.node_timeout, so that a writer that waits behind
 * a prepared transaction of a crashed origin (which keeps its locks until it is
 * resolved) gets a clear error instead of waiting for ever.
 */
void
kingres_shard_lock_reference_table(const char *qualified_name)
{
	RangeVar   *rv = makeRangeVarFromNameList(textToQualifiedNameList(cstring_to_text(qualified_name)));
	Oid			relid = RangeVarGetRelid(rv, NoLock, true);
	LOCKTAG		tag;
	int			nest;
	char		buf[32];
	ErrorContextCallback errcallback;

	if (!OidIsValid(relid))
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("reference table \"%s\" does not exist on this node", qualified_name)));
	SET_LOCKTAG_OBJECT(tag, MyDatabaseId, KS_REFWRITE_LOCK_CLASS, relid, 0);

	nest = NewGUCNestLevel();
	if (LockTimeout == 0 || LockTimeout > kingres_shard_node_timeout * 1000)
	{
		snprintf(buf, sizeof(buf), "%d", kingres_shard_node_timeout * 1000);
		(void) set_config_option("lock_timeout", buf, PGC_USERSET, PGC_S_SESSION,
								 GUC_ACTION_SAVE, true, 0, false);
	}
	errcallback.callback = lock_wait_error_context;
	errcallback.arg = (void *) qualified_name;
	errcallback.previous = error_context_stack;
	error_context_stack = &errcallback;
	(void) LockAcquire(&tag, ExclusiveLock, false, false);
	error_context_stack = errcallback.previous;
	AtEOXact_GUC(true, nest);
}

/*
 * kingres.internal_lock_reference_table(name text) returns void
 *
 * The same for another node's writer, which sends it in its forwarded session.
 */
PG_FUNCTION_INFO_V1(kingres_internal_lock_reference_table);

Datum
kingres_internal_lock_reference_table(PG_FUNCTION_ARGS)
{
	if (!superuser() && !kingres_shard_session_forwarded())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for the internal functions of kingres_shard")));
	kingres_shard_lock_reference_table(text_to_cstring(PG_GETARG_TEXT_PP(0)));
	PG_RETURN_VOID();
}

/*
 * kingres.internal_invalidate_relation(rel regclass) returns void
 *
 * Called when the catalog of the cluster changes: the plans of statements on a
 * table that was planned with the old catalog (a write that did not fan out,
 * because the table was not yet a reference table) must be made again.
 */
PG_FUNCTION_INFO_V1(kingres_internal_invalidate_relation);

Datum
kingres_internal_invalidate_relation(PG_FUNCTION_ARGS)
{
	CacheInvalidateRelcacheByRelid(PG_GETARG_OID(0));
	PG_RETURN_VOID();
}

/*
 * kingres_shard_refwrite_lock_sql
 *
 * The statement that takes the lock of kingres_shard_lock_reference_table on
 * the node it is sent to.
 */
char *
kingres_shard_refwrite_lock_sql(const char *qualified_name)
{
	return psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_lock_reference_table(%s)",
					quote_literal_cstr(qualified_name));
}

/*
 * Serialize the writers of the reference tables a statement uses (review
 * finding S2-06): on every target, in the order of the list (node id order),
 * take the lock of each table in name order.  The same order everywhere makes
 * such writers wait for each other without a deadlock, and a writer that gets
 * all its locks runs after the commit of every earlier writer on every node,
 * so the statement sees the same rows on each node.  The locks are held to the
 * end of the distributed transaction.
 */
static void
lock_reference_tables(KsFanoutState *s)
{
	ListCell   *lc;

	foreach(lc, s->targets)
	{
		KsFanTarget *t = (KsFanTarget *) lfirst(lc);
		ListCell   *lc2;

		if (t->is_local)
		{
			foreach(lc2, s->locknames)
				kingres_shard_lock_reference_table((char *) lfirst(lc2));
		}
		else
		{
			KsConn	   *kc = kingres_shard_get_conn(t->host, t->port, false, NULL);

			kingres_shard_begin_write_xact(kc);
			foreach(lc2, s->locknames)
				PQclear(kingres_shard_exec(kc, kingres_shard_refwrite_lock_sql((char *) lfirst(lc2))));
		}
	}
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
	int64		origin_placement = 0;

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

	/*
	 * The groups of this statement are those of the placement it was planned with
	 * (a cached or generic plan can be older than the catalog this node holds
	 * now); a placement that changed since then is a different set of groups, and
	 * nothing has run yet.
	 */
	if (s->all_rows)
	{
		origin_placement = kingres_shard_placement_hash(kingres_shard_lookup_colocation(s->colocation_id));
		if (origin_placement != s->plan_placement)
			ereport(ERROR,
					(errcode(ERRCODE_KINGRES_STALE_MAP),
					 errmsg("the bucket map changed after the statement was planned"),
					 errhint("A bucket moved between shard groups; nothing was changed.  Repeat the statement.")));
	}

	if (s->locknames != NIL)
	{
		lock_reference_tables(s);
		check_node_set(s);
	}

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
	kingres_shard_fault_point("fanout_after_run");

	/*
	 * The map version rule (design/shard-phase4.md): a write to a distributed
	 * table is run on the groups one after the other; a bucket that moved
	 * between two of those statements would be missed.  Every group is still
	 * locked by this transaction (the statement holds ROW EXCLUSIVE on the parent
	 * of the table there, which the switch of a move cannot get past), so its
	 * partitions cannot have changed since the statement ran on it; when every
	 * group (and this node) has the placement the groups were chosen with, no
	 * bucket moved while the statement ran.
	 */
	if (s->all_rows)
	{
		foreach(lc, s->targets)
		{
			KsFanTarget *t = (KsFanTarget *) lfirst(lc);
			int64		theirs;

			if (t->is_local)
				theirs = kingres_shard_placement_hash_now(s->colocation_id);
			else
			{
				KsConn	   *kc = kingres_shard_get_conn(t->host, t->port, false, NULL);
				PGresult   *res = kingres_shard_exec(kc,
													 psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_placement_hash(%d)",
															  s->colocation_id));

				theirs = PQgetisnull(res, 0, 0) ? 0 : pg_strtoint64(PQgetvalue(res, 0, 0));
				PQclear(res);
			}
			if (theirs != origin_placement)
				ereport(ERROR,
						(errcode(ERRCODE_KINGRES_STALE_MAP),
						 errmsg("the bucket map changed while the statement was running (node %s:%d has another placement of the buckets)",
								t->host, t->port),
						 errhint("A bucket moved between shard groups; nothing was changed.  Repeat the statement.")));
		}
	}

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
