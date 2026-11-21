/*-------------------------------------------------------------------------
 *
 * rebuild.c
 *	  The C part of the rebuild engine of the online table operations
 *	  (design/shard-phase9.md, "The rebuild engine").
 *
 * The orchestration (the record of an operation, its phases, the copy and the
 * replay loops, cancel, resume, cleaning) is SQL, in the extension script, and
 * runs the steps that must commit at once in sessions of their own
 * (kingres.internal_node_exec).  What cannot be SQL is here:
 *
 *	 - the script that creates the shadow of a table on one node, in the layout
 *	   of the destination (the same statements as the blocking variant makes);
 *	 - the swap: in one transaction on the map group primary and, in branches
 *	   that the two-phase commit prepares, on every other node, the old table
 *	   is retired, the shadow takes its name, its sequences, foreign keys,
 *	   owner, privileges and comments, the table is recorded in the catalog and
 *	   the operation moves to "cleaning";
 *	 - the live mark of an operation (a session-level lock that the session
 *	   that runs it holds), and the refusal to run inside a transaction block;
 *	 - the choice of the colocation group of a new table.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/rebuild.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/xact.h"
#include "catalog/pg_type_d.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "lib/stringinfo.h"
#include "miscadmin.h"
#include "storage/lmgr.h"
#include "storage/lock.h"
#include "utils/builtins.h"
#include "utils/json.h"
#include "utils/jsonb.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"

#include "catalog.h"
#include "copy_routed.h"
#include "dist_table.h"
#include "kingres_shard.h"
#include "node_admin.h"
#include "node_conn.h"
#include "utility.h"

PG_FUNCTION_INFO_V1(kingres_internal_choose_colocation);
PG_FUNCTION_INFO_V1(kingres_internal_shadow_script);
PG_FUNCTION_INFO_V1(kingres_internal_swap_table);
PG_FUNCTION_INFO_V1(kingres_internal_op_lock);
PG_FUNCTION_INFO_V1(kingres_internal_op_unlock);
PG_FUNCTION_INFO_V1(kingres_internal_op_alive);
PG_FUNCTION_INFO_V1(kingres_internal_prevent_xact_block);
PG_FUNCTION_INFO_V1(kingres_internal_branch_exec);
PG_FUNCTION_INFO_V1(kingres_internal_branch_scalar);

static void
require_superuser(const char *what)
{
	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for %s", what),
				 errdetail("Table operations are for superusers.")));
}

/*
 * The mark of a live operation: a session-level lock on a tag of its own (an advisory lock tag with
 * a fifth field the SQL advisory functions never use, and one that the move marks do not use).
 */
static void
op_locktag(int64 op_id, LOCKTAG *tag)
{
	SET_LOCKTAG_ADVISORY(*tag, MyDatabaseId,
						 (uint32) (((uint64) op_id) >> 32), (uint32) op_id, 5);
}

/* kingres.internal_op_lock(op_id bigint): take the mark (waits for nobody: false if held) */
Datum
kingres_internal_op_lock(PG_FUNCTION_ARGS)
{
	LOCKTAG		tag;
	LockAcquireResult res;

	require_superuser("kingres.internal_op_lock()");
	op_locktag(PG_GETARG_INT64(0), &tag);
	res = LockAcquire(&tag, ExclusiveLock, true, true);
	PG_RETURN_BOOL(res != LOCKACQUIRE_NOT_AVAIL);
}

Datum
kingres_internal_op_unlock(PG_FUNCTION_ARGS)
{
	LOCKTAG		tag;

	require_superuser("kingres.internal_op_unlock()");
	op_locktag(PG_GETARG_INT64(0), &tag);
	(void) LockRelease(&tag, ExclusiveLock, true);
	PG_RETURN_VOID();
}

/* kingres.internal_op_alive(op_id bigint): whether another session holds the mark */
Datum
kingres_internal_op_alive(PG_FUNCTION_ARGS)
{
	LOCKTAG		tag;
	LockAcquireResult res;

	require_superuser("kingres.internal_op_alive()");
	op_locktag(PG_GETARG_INT64(0), &tag);
	res = LockAcquire(&tag, ExclusiveLock, true, true);
	if (res == LOCKACQUIRE_NOT_AVAIL)
		PG_RETURN_BOOL(true);
	(void) LockRelease(&tag, ExclusiveLock, true);
	PG_RETURN_BOOL(false);
}

/*
 * kingres.internal_prevent_xact_block(what text) returns void
 *
 * The functions that commit in sessions of their own cannot run inside a transaction block
 * (the commits of those sessions would not be the caller's, and the caller could roll the
 * operation's record back by itself).
 */
Datum
kingres_internal_prevent_xact_block(PG_FUNCTION_ARGS)
{
	char	   *what = text_to_cstring(PG_GETARG_TEXT_PP(0));

	PreventInTransactionBlock(true, what);
	PG_RETURN_VOID();
}

/*
 * kingres.internal_choose_colocation(type regtype, collation oid, colocate_with regclass,
 *									   buckets int) returns jsonb
 *
 * What create_distributed_table() decides, for the preparing phase of an online operation: the
 * colocation group of the new table (a new group has its rows made here, in the caller's
 * transaction) and its bucket count; {"colocation_id", "bucket_count", "created"}.
 */
Datum
kingres_internal_choose_colocation(PG_FUNCTION_ARGS)
{
	Oid			coltype = PG_GETARG_OID(0);
	Oid			collation = PG_GETARG_OID(1);
	Oid			colocate_with = PG_ARGISNULL(2) ? InvalidOid : PG_GETARG_OID(2);
	int			requested = PG_ARGISNULL(3) ? 0 : PG_GETARG_INT32(3);
	int			nbuckets = 0;
	bool		created = false;
	int			colocation_id;
	StringInfoData out;

	require_superuser("kingres.internal_choose_colocation()");
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	colocation_id = kingres_shard_choose_colocation(coltype, collation, colocate_with, requested,
													&nbuckets, &created);
	SPI_finish();
	initStringInfo(&out);
	appendStringInfo(&out, "{\"colocation_id\": %d, \"bucket_count\": %d, \"created\": %s}",
					 colocation_id, nbuckets, created ? "true" : "false");
	PG_RETURN_DATUM(DirectFunctionCall1(jsonb_in, CStringGetDatum(out.data)));
}

/*
 * kingres.internal_shadow_script(relid regclass, column name, colocation_id int, group_id int)
 *								  returns text
 *
 * The statements that create the shadow of a table on the node of group_id: the schema, the
 * sequences the columns own, the shadow parent (kingres_shadow_<oid>) with its constraints and
 * indexes, and, for a distributed destination (column not NULL), the partitions of the buckets
 * that the group owns in the colocation group, under their final names.  The old table is not
 * touched: nothing is detached from it before the switch.
 */
Datum
kingres_internal_shadow_script(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	char	   *column = PG_ARGISNULL(1) ? NULL : pstrdup(NameStr(*PG_GETARG_NAME(1)));
	int			colocation_id = PG_ARGISNULL(2) ? 0 : PG_GETARG_INT32(2);
	int			group = PG_ARGISNULL(3) ? 0 : PG_GETARG_INT32(3);
	char	   *partbase = PG_ARGISNULL(4) ? NULL : text_to_cstring(PG_GETARG_TEXT_PP(4));
	MemoryContext callercxt = CurrentMemoryContext;
	char	   *relname;
	char	   *nspname;
	char	   *shadow;
	DdlSet		ddl;
	StringInfoData script;
	char	   *with_clause;

	require_superuser("kingres.internal_shadow_script()");
	relname = get_rel_name(relid);
	if (relname == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("relation with OID %u does not exist", relid)));
	relname = pstrdup(relname);
	nspname = pstrdup(get_namespace_name(get_rel_namespace(relid)));
	shadow = psprintf("kingres_shadow_%u", relid);

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	kingres_shard_fetch_ddl(relid, column, true, &ddl, callercxt);
	with_clause = kingres_shard_with_clause(relid, NULL, callercxt);
	SPI_finish();

	initStringInfo(&script);
	appendStringInfoString(&script, ddl.remote_pre.data);
	appendStringInfoString(&script, ddl.both.data);
	if (column != NULL)
	{
		KsColocation *placement;

		if (colocation_id <= 0)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("a distributed shadow needs a colocation group")));
		placement = kingres_shard_load_placement(colocation_id);
		/* a table that is rebuilt while the old one is in use cannot have its partitions under the final names yet */
		kingres_shard_append_partitions(&script, nspname, partbase != NULL ? partbase : relname, shadow, placement, group, with_clause);
	}
	PG_RETURN_TEXT_P(cstring_to_text(script.data));
}

/*
 * The foreign keys are added to the new table NOT VALID: no scan under the locks of the switch.
 * They are validated in the cleaning phase, which admits writers.  A key that was not valid
 * to begin with already says so.
 */
static char *
fk_not_valid(const char *fk)
{
	StringInfoData out;
	const char *p = fk;

	initStringInfo(&out);
	while (*p != '\0')
	{
		const char *end = strstr(p, ";\n");
		size_t		len;

		if (end == NULL)
			end = p + strlen(p);
		len = end - p;
		if (len > 0)
		{
			char	   *stmt = pnstrdup(p, len);

			appendStringInfoString(&out, stmt);
			if (strstr(stmt, "NOT VALID") == NULL)
				appendStringInfoString(&out, " NOT VALID");
			appendStringInfoString(&out, ";\n");
			pfree(stmt);
		}
		p = (*end == '\0') ? end : end + 2;
	}
	return out.data;
}

/*
 * kingres.internal_swap_table(op_id bigint, relid regclass, column name, colocation_id int,
 *							   cleanup_delay int) returns text
 *
 * The swap of the switch (design, "Switch", step 4), in the caller's transaction, which holds the
 * locks of the switch and has replayed the log to the end.  Returns the name of the retained old
 * copy.  A NULL column makes a reference table.
 */
Datum
kingres_internal_swap_table(PG_FUNCTION_ARGS)
{
	int64		op_id = PG_GETARG_INT64(0);
	Oid			relid = PG_GETARG_OID(1);
	char	   *column = PG_ARGISNULL(2) ? NULL : pstrdup(NameStr(*PG_GETARG_NAME(2)));
	int			colocation_id = PG_ARGISNULL(3) ? 0 : PG_GETARG_INT32(3);
	int			cleanup_s = PG_GETARG_INT32(4);
	MemoryContext callercxt = CurrentMemoryContext;
	char	   *relname;
	Oid			nspoid;
	DdlSet		ddl;
	Oid			argtypes[3];
	Datum		values[3];
	bool		isnull;
	Oid			newrelid;
	Oid			oldrelid;
	char	   *oldname;
	char	   *fk_nv;
	char	   *reloptions;
	List	   *targets;
	ListCell   *lc;
	StringInfoData script;
	StringInfoData xids;
	char	   *local_xid;

	require_superuser("kingres.internal_swap_table()");
	/* the faults of the commit protocol that name this scope fire for the swap, not for the chunks before it */
	kingres_shard_fault_set_scope("tswitch");
	kingres_shard_require_map_group();
	relname = get_rel_name(relid);
	if (relname == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("relation with OID %u does not exist", relid)));
	relname = pstrdup(relname);
	nspoid = get_rel_namespace(relid);

	initStringInfo(&script);
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	/* The statements, read while the old table is whole (sequence states are the final ones). */
	kingres_shard_fetch_ddl(relid, column, true, &ddl, callercxt);
	reloptions = NULL;
	(void) kingres_shard_with_clause(relid, &reloptions, callercxt);
	fk_nv = fk_not_valid(ddl.fk.data);
	{
		MemoryContext old = MemoryContextSwitchTo(callercxt);

		fk_nv = pstrdup(fk_nv);
		MemoryContextSwitchTo(old);
	}
	kingres_shard_fault_point("rebuild_switch_before_swap");

	/* The old table goes: its sequences are let go, then it is renamed and moved out of the way. */
	if (ddl.local_pre.len > 0)
		kingres_shard_run_spi(ddl.local_pre.data, 0, NULL, NULL);
	argtypes[0] = OIDOID;
	argtypes[1] = INT8OID;
	values[0] = ObjectIdGetDatum(relid);
	values[1] = Int64GetDatum(op_id);
	kingres_shard_run_spi("SELECT " KINGRES_SHARD_SCHEMA ".internal_retire_table($1::pg_catalog.oid::pg_catalog.regclass, $2)::pg_catalog.text",
						  2, argtypes, values);
	oldname = MemoryContextStrdup(callercxt,
								  SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1));
	oldrelid = DatumGetObjectId(DirectFunctionCall1(regclassin, CStringGetDatum(oldname)));

	/* The shadow takes its place. */
	if (ddl.swap.len > 0)
		kingres_shard_run_spi(ddl.swap.data, 0, NULL, NULL);
	newrelid = get_relname_relid(relname, nspoid);
	if (!OidIsValid(newrelid))
		elog(ERROR, "could not find the new table \"%s\"", relname);
	if (fk_nv[0] != '\0')
	{
		kingres_shard_internal_ddl_enter();
		PG_TRY();
		{
			kingres_shard_run_spi(fk_nv, 0, NULL, NULL);
		}
		PG_FINALLY();
		{
			kingres_shard_internal_ddl_leave();
		}
		PG_END_TRY();
	}
	if (ddl.post.len > 0)
		kingres_shard_run_spi(ddl.post.data, 0, NULL, NULL);
	if (ddl.post_local.len > 0)
		kingres_shard_run_spi(ddl.post_local.data, 0, NULL, NULL);
	kingres_shard_fault_point("rebuild_switch_after_local_swap");

	/* Record the table, the old copy, and the end of the switch, in this transaction. */
	argtypes[0] = OIDOID;
	argtypes[1] = TEXTOID;
	argtypes[2] = INT4OID;
	values[0] = ObjectIdGetDatum(newrelid);
	if (column != NULL)
	{
		values[1] = CStringGetTextDatum(column);
		values[2] = Int32GetDatum(colocation_id);
		kingres_shard_run_spi(psprintf("INSERT INTO " KINGRES_SHARD_SCHEMA ".dist_table (relid, method, dist_column, colocation_id, reloptions) "
									   "VALUES ($1::pg_catalog.oid::pg_catalog.regclass, 'hash', $2::pg_catalog.name, $3, %s::pg_catalog.text[])",
									   reloptions != NULL ? quote_literal_cstr(reloptions) : "NULL"),
							  3, argtypes, values);
	}
	else
		kingres_shard_run_spi(psprintf("INSERT INTO " KINGRES_SHARD_SCHEMA ".dist_table (relid, method, reloptions) "
									   "VALUES ($1::pg_catalog.oid::pg_catalog.regclass, 'reference', %s::pg_catalog.text[])",
									   reloptions != NULL ? quote_literal_cstr(reloptions) : "NULL"),
							  1, argtypes, values);

	/* the transaction id of the swap on this node: what a later verification compares the rows' xmin with */
	kingres_shard_run_spi("SELECT pg_catalog.pg_current_xact_id()::pg_catalog.text", 0, NULL, NULL);
	local_xid = MemoryContextStrdup(callercxt, SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1));
	SPI_finish();

	/* The other nodes: the same swap, in the branches that the commit protocol prepares. */
	initStringInfo(&xids);
	appendStringInfo(&xids, "{\"swap_xid\": {\"%d\": \"%s\"", kingres_shard_catalog()->local_node_id, local_xid);
	targets = kingres_shard_remote_primaries(callercxt);
	foreach(lc, targets)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		KsConn	   *kc = kingres_shard_get_conn(n->host, n->port, false, NULL);
		PGresult   *xres;

		kingres_shard_begin_write_xact(kc);
		resetStringInfo(&script);
		appendStringInfoString(&script, ddl.swap.data);
		if (fk_nv[0] != '\0')
		{
			appendStringInfoString(&script, "SELECT pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);\n");
			appendStringInfoString(&script, fk_nv);
			appendStringInfoString(&script, "SELECT pg_catalog.set_config('kingres_shard.enable_routing', 'on', true);\n");
		}
		appendStringInfoString(&script, ddl.post.data);
		kingres_shard_exec_on_node(kc, script.data);
		xres = kingres_shard_exec(kc, "SELECT pg_catalog.pg_current_xact_id()::pg_catalog.text");
		if (PQresultStatus(xres) == PGRES_TUPLES_OK && PQntuples(xres) == 1)
			appendStringInfo(&xids, ", \"%d\": \"%s\"", n->node_id, PQgetvalue(xres, 0, 0));
		PQclear(xres);
	}
	appendStringInfoString(&xids, "}}");
	kingres_shard_fault_point("rebuild_switch_after_remote_swap");

	/* The end of the switch is recorded in this transaction: the phase, the old copy, the xids. */
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	argtypes[0] = INT8OID;
	argtypes[1] = INT4OID;
	argtypes[2] = OIDOID;
	values[0] = Int64GetDatum(op_id);
	values[1] = Int32GetDatum(cleanup_s);
	values[2] = ObjectIdGetDatum(oldrelid);
	{
		Oid			xt[2] = {INT8OID, TEXTOID};
		Datum		xv[2];

		xv[0] = Int64GetDatum(op_id);
		xv[1] = CStringGetTextDatum(xids.data);
		kingres_shard_run_spi("UPDATE " KINGRES_SHARD_SCHEMA ".table_operation o "
							  "SET spec = o.spec OPERATOR(pg_catalog.||) $2::pg_catalog.jsonb "
							  "WHERE o.op_id OPERATOR(pg_catalog.=) $1",
							  2, xt, xv);
	}
	kingres_shard_run_spi("UPDATE " KINGRES_SHARD_SCHEMA ".table_operation o "
						  "SET phase = 'cleaning', detail = NULL, "
						  "cleanup_after = pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+) pg_catalog.make_interval(secs => $2), "
						  "updated_at = pg_catalog.clock_timestamp() "
						  "WHERE o.op_id OPERATOR(pg_catalog.=) $1",
						  2, argtypes, values);
	kingres_shard_run_spi("UPDATE " KINGRES_SHARD_SCHEMA ".table_operation_item i "
						  "SET old_name = (SELECT c.relname FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) $3) "
						  "WHERE i.op_id OPERATOR(pg_catalog.=) $1",
						  3, argtypes, values);
	kingres_shard_run_spi("INSERT INTO " KINGRES_SHARD_SCHEMA ".table_old_copy (relid, dropped_after) "
						  "VALUES ($3::pg_catalog.oid::pg_catalog.regclass, "
						  "pg_catalog.clock_timestamp() OPERATOR(pg_catalog.+) pg_catalog.make_interval(secs => $2))",
						  3, argtypes, values);
	(void) isnull;
	SPI_finish();

	/* Make every node know the table. */
	kingres_shard_sync_catalog_internal();
	kingres_shard_fault_point("rebuild_switch_after_prepare");

	PG_RETURN_TEXT_P(cstring_to_text(oldname));
}

/*
 * kingres.internal_branch_scalar(node_id int, sql text) returns text
 *
 * Like internal_branch_exec(), and the first value of the first row that the statement returns (NULL when it
 * returns none): the transaction id of the swap on a node, the OID of a table there.
 */
Datum
kingres_internal_branch_scalar(PG_FUNCTION_ARGS)
{
	int			node_id = PG_GETARG_INT32(0);
	char	   *sql = text_to_cstring(PG_GETARG_TEXT_PP(1));
	const KsNode *n;
	KsConn	   *kc;
	PGresult   *res;
	text	   *result = NULL;

	require_superuser("kingres.internal_branch_scalar()");
	n = kingres_shard_node_by_id(node_id);
	if (n == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_OBJECT),
				 errmsg("node %d does not exist", node_id)));
	if (n->node_id == kingres_shard_catalog()->local_node_id)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("node %d is this node", node_id)));
	kc = kingres_shard_get_conn(n->host, n->port, false, NULL);
	kingres_shard_begin_write_xact(kc);
	res = kingres_shard_exec(kc, sql);
	if (PQresultStatus(res) == PGRES_TUPLES_OK && PQntuples(res) > 0 && !PQgetisnull(res, 0, 0))
		result = cstring_to_text(PQgetvalue(res, 0, 0));
	else if (PQresultStatus(res) != PGRES_TUPLES_OK && PQresultStatus(res) != PGRES_COMMAND_OK)
	{
		PQclear(res);
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("the statement failed on node %d", node_id)));
	}
	PQclear(res);
	if (result == NULL)
		PG_RETURN_NULL();
	PG_RETURN_TEXT_P(result);
}

/*
 * kingres.internal_branch_exec(node_id int, sql text) returns void
 *
 * Run a statement on another node inside the remote branch of the current transaction (the branch
 * that the commit protocol prepares and commits with the rest): the replay of a chunk deletes keys at
 * the destination in the same transaction that inserts their rows, so that a key that two chunks of one
 * transaction handle is removed and written in order, and a chunk is all or nothing.
 */
Datum
kingres_internal_branch_exec(PG_FUNCTION_ARGS)
{
	int			node_id = PG_GETARG_INT32(0);
	char	   *sql = text_to_cstring(PG_GETARG_TEXT_PP(1));
	const KsNode *n;
	KsConn	   *kc;

	require_superuser("kingres.internal_branch_exec()");
	n = kingres_shard_node_by_id(node_id);
	if (n == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_OBJECT),
				 errmsg("node %d does not exist", node_id)));
	if (n->node_id == kingres_shard_catalog()->local_node_id)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("node %d is this node", node_id)));
	kc = kingres_shard_get_conn(n->host, n->port, false, NULL);
	kingres_shard_begin_write_xact(kc);
	kingres_shard_exec_on_node(kc, sql);
	PG_RETURN_VOID();
}
