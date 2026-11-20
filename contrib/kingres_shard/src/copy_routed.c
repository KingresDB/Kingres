/*-------------------------------------------------------------------------
 *
 * copy_routed.c
 *	  Copy the rows of a table to a destination that is described by its
 *	  caller (design/shard-phase9.md, "Copy"; shard-phase2b.md, 11.9).
 *
 * The batch sender of the row router (batch_sender.c) is given a destination
 * instead of a catalog entry: a placement (which group owns which bucket), every
 * node, or one node, and the name of a table that need not be in the catalog.
 * That is what a copy needs: the table that is being built (a shadow table that
 * no statement can see) receives the rows of the table it replaces.
 *
 * The source is read through a cursor in chunks of insert_batch_rows rows; the
 * sender keeps at most two buffers per group (batch_sender.c), so the memory the
 * copy uses does not depend on the number of rows.  Nothing here decides about
 * the transaction: the writes are in the caller's transaction (the branches that
 * the commit protocol of 2a prepares), and any failure is an error.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/copy_routed.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "catalog/pg_type_d.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "lib/stringinfo.h"
#include "miscadmin.h"
#include "utils/array.h"
#include "utils/builtins.h"
#include "utils/json.h"
#include "utils/jsonb.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/snapmgr.h"

#include "batch_sender.h"
#include "catalog.h"
#include "copy_routed.h"
#include "kingres_shard.h"
#include "node_conn.h"

PG_FUNCTION_INFO_V1(kingres_internal_copy_routed);

int64
kingres_shard_copy_routed(Oid source, const char *dest_schema, const char *dest_table,
						  KsDestKind kind, const KsColocation *placement,
						  const char *key_column, int node_id, int64 *rows_read)
{
	return kingres_shard_copy_routed_ex(source, dest_schema, dest_table, kind, placement,
										key_column, node_id, rows_read, NULL);
}

/*
 * A table has a column of type money: its text depends on lc_monetary, which the transmission
 * modes do not fix.  The nodes that receive such rows get "SET LOCAL lc_monetary = 'C'" in the
 * transaction of the chunk (an online chunk is a transaction of its own; the blocking variant
 * sets it in the transaction that creates the shadow).
 */
static bool
source_has_money(Oid source)
{
	Oid			argtypes[1] = {OIDOID};
	Datum		argvalues[1];
	bool		found;

	argvalues[0] = ObjectIdGetDatum(source);
	if (SPI_execute_with_args("SELECT 1 FROM pg_catalog.pg_attribute a "
							  "WHERE a.attrelid OPERATOR(pg_catalog.=) $1 AND a.attnum OPERATOR(pg_catalog.>) 0 "
							  "AND NOT a.attisdropped AND a.atttypid OPERATOR(pg_catalog.=) 'pg_catalog.money'::pg_catalog.regtype",
							  1, argtypes, argvalues, NULL, true, 1) != SPI_OK_SELECT)
		elog(ERROR, "could not read the columns of table %u", source);
	found = SPI_processed > 0;
	return found;
}

int64
kingres_shard_copy_routed_ex(Oid source, const char *dest_schema, const char *dest_table,
							 KsDestKind kind, const KsColocation *placement,
							 const char *key_column, int node_id, int64 *rows_read,
							 KsCopyOpts *opts)
{
	MemoryContext callercxt = CurrentMemoryContext;
	char	   *src_schema = get_namespace_name(get_rel_namespace(source));
	char	   *src_table = get_rel_name(source);
	Oid			argtypes[1] = {OIDOID};
	Datum		argvalues[1];
	List	   *columns = NIL;
	StringInfoData collist;
	StringInfoData query;
	KsDestination dest;
	KsBatchSpec spec;
	KsBatchSender *sender;
	SPIPlanPtr	plan;
	Portal		portal;
	Datum	   *values;
	bool	   *nulls;
	int			ncols;
	int			key_attno = -1;
	bool		identity = false;
	int64		nread = 0;
	int64		acked;
	int			nest;
	int			guc_nest;
	uint64		i;

	if (src_table == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("relation with OID %u does not exist", source)));

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	/* The columns that carry values: generated columns are computed by the destination. */
	argvalues[0] = ObjectIdGetDatum(source);
	if (SPI_execute_with_args("SELECT a.attname::pg_catalog.text, a.atttypid::pg_catalog.oid, a.atttypmod, "
							  "a.attidentity::pg_catalog.text "
							  "FROM pg_catalog.pg_attribute a "
							  "WHERE a.attrelid OPERATOR(pg_catalog.=) $1 AND a.attnum OPERATOR(pg_catalog.>) 0 "
							  "AND NOT a.attisdropped AND a.attgenerated OPERATOR(pg_catalog.=) '' "
							  "ORDER BY a.attnum",
							  1, argtypes, argvalues, NULL, true, 0) != SPI_OK_SELECT)
		elog(ERROR, "could not read the columns of table \"%s\"", src_table);
	ncols = (int) SPI_processed;
	if (ncols == 0)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("table \"%s\" has no column that holds values", src_table)));
	initStringInfo(&collist);
	for (i = 0; i < SPI_processed; i++)
	{
		HeapTuple	t = SPI_tuptable->vals[i];
		TupleDesc	d = SPI_tuptable->tupdesc;
		bool		isnull;
		KsBatchColumn *c = palloc0(sizeof(KsBatchColumn));
		char	   *ident;

		c->name = SPI_getvalue(t, d, 1);
		c->type = DatumGetObjectId(SPI_getbinval(t, d, 2, &isnull));
		c->typmod = DatumGetInt32(SPI_getbinval(t, d, 3, &isnull));
		ident = SPI_getvalue(t, d, 4);
		if (ident != NULL && ident[0] != '\0')
			identity = true;
		appendStringInfo(&collist, "%s%s", i > 0 ? ", " : "", quote_identifier(c->name));
		if (key_column != NULL && strcmp(c->name, key_column) == 0)
			key_attno = (int) i;
		columns = lappend(columns, c);
	}
	if (kind == KS_DEST_PLACEMENT && key_attno < 0)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_COLUMN),
				 errmsg("column \"%s\" is not a column of table \"%s\" that carries a value",
						key_column ? key_column : "(none)", src_table)));

	memset(&dest, 0, sizeof(dest));
	dest.kind = kind;
	dest.schema = dest_schema;
	dest.table = dest_table;
	dest.relid = InvalidOid;
	dest.placement = placement;
	dest.key_attno = key_attno;
	dest.node_id = node_id;
	dest.expect_placement = 0;
	dest.check_identity = (kind == KS_DEST_PLACEMENT);

	memset(&spec, 0, sizeof(spec));
	spec.columns = columns;
	spec.conflict = (opts != NULL && opts->conflict_nothing) ? KS_CONFLICT_NOTHING : KS_CONFLICT_NONE;
	spec.conflict_sql = "ON CONFLICT DO NOTHING";
	spec.overriding_system_value = identity;

	/*
	 * The text of a money value depends on lc_monetary, which the transmission modes do not
	 * fix: both ends of the copy use "C" (the other nodes get SET LOCAL lc_monetary in the
	 * transaction that creates the destination), for the whole of the copy, local batches included.
	 */
	guc_nest = NewGUCNestLevel();
	(void) set_config_option("lc_monetary", "C", PGC_USERSET, PGC_S_SESSION,
							 GUC_ACTION_SAVE, true, 0, false);
	if (opts != NULL && source_has_money(source))
	{
		/* the transaction of an online chunk is its own: the nodes it writes get the setting here */
		const KsCatalog *cat = kingres_shard_catalog();
		ListCell   *lc;

		foreach(lc, cat->nodes)
		{
			const KsNode *n = (const KsNode *) lfirst(lc);
			KsConn	   *kc;
			PGresult   *res;

			if (n->node_id == cat->local_node_id || !n->is_primary || !n->is_active)
				continue;
			if (kind == KS_DEST_ONE_NODE && n->node_id != node_id)
				continue;
			kc = kingres_shard_get_conn(n->host, n->port, false, NULL);
			kingres_shard_begin_write_xact(kc);
			res = kingres_shard_exec(kc, "SET LOCAL lc_monetary = 'C'");
			PQclear(res);
		}
	}
	sender = kingres_shard_batch_begin(&dest, &spec);

	initStringInfo(&query);
	appendStringInfo(&query, "SELECT %s", collist.data);
	if (opts != NULL)
	{
		int			k;

		for (k = 0; k < opts->nkeys; k++)
			appendStringInfo(&query, ", (%s)::pg_catalog.text AS ks_key_%d", quote_identifier(opts->keycols[k]), k);
	}
	appendStringInfo(&query, " FROM %s", quote_qualified_identifier(src_schema, src_table));
	if (opts != NULL && (opts->where_sql != NULL || opts->after != NULL))
	{
		bool		first = true;

		if (opts->where_sql != NULL)
		{
			appendStringInfo(&query, " WHERE (%s)", opts->where_sql);
			first = false;
		}
		if (opts->after != NULL && opts->nkeys > 0)
		{
			StringInfoData lhs;
			StringInfoData rhs;
			int			k;

			initStringInfo(&lhs);
			initStringInfo(&rhs);
			for (k = 0; k < opts->nkeys; k++)
			{
				Oid			kargtypes[2] = {OIDOID, TEXTOID};
				Datum		kvalues[2];

				kvalues[0] = ObjectIdGetDatum(source);
				kvalues[1] = CStringGetTextDatum(opts->keycols[k]);
				if (SPI_execute_with_args("SELECT pg_catalog.format_type(a.atttypid, a.atttypmod) FROM pg_catalog.pg_attribute a "
										  "WHERE a.attrelid OPERATOR(pg_catalog.=) $1 AND a.attname OPERATOR(pg_catalog.=) $2::pg_catalog.name "
										  "AND NOT a.attisdropped",
										  2, kargtypes, kvalues, NULL, true, 1) != SPI_OK_SELECT || SPI_processed != 1)
					ereport(ERROR,
							(errcode(ERRCODE_UNDEFINED_COLUMN),
							 errmsg("key column \"%s\" is not a column of table \"%s\"", opts->keycols[k], src_table)));
				appendStringInfo(&lhs, "%s%s", k ? ", " : "", quote_identifier(opts->keycols[k]));
				appendStringInfo(&rhs, "%s%s::%s", k ? ", " : "", quote_literal_cstr(opts->after[k]),
								 SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1));
			}
			appendStringInfo(&query, " %s ROW(%s) > ROW(%s)", first ? "WHERE" : "AND", lhs.data, rhs.data);
		}
	}
	if (opts != NULL && opts->nkeys > 0 && (opts->limit > 0 || opts->after != NULL))
	{
		int			k;

		appendStringInfoString(&query, " ORDER BY ");
		for (k = 0; k < opts->nkeys; k++)
			appendStringInfo(&query, "%s%s", k ? ", " : "", quote_identifier(opts->keycols[k]));
	}
	if (opts != NULL && opts->limit > 0)
		appendStringInfo(&query, " LIMIT " INT64_FORMAT, opts->limit);
	plan = SPI_prepare(query.data, 0, NULL);
	if (plan == NULL)
		elog(ERROR, "could not prepare the read of table \"%s\"", src_table);
	/*
	 * A read-only cursor reads with the active snapshot, which a caller that waited for a lock took before the
	 * wait: a row that a transaction committed meanwhile would be missed or read in its old version.  A fresh
	 * snapshot is taken here (READ COMMITTED; the callers refuse the other levels).
	 */
	PushActiveSnapshot(GetTransactionSnapshot());
	portal = SPI_cursor_open(NULL, plan, NULL, NULL, true);
	if (portal == NULL)
		elog(ERROR, "could not open a cursor on table \"%s\"", src_table);

	values = palloc(sizeof(Datum) * ncols);
	nulls = palloc(sizeof(bool) * ncols);
	kingres_shard_fault_point("copy_before_read");
	for (;;)
	{
		SPITupleTable *tuptable;
		uint64		nproc;
		uint64		k;

		CHECK_FOR_INTERRUPTS();
		SPI_cursor_fetch(portal, true, kingres_shard_insert_batch_rows);
		tuptable = SPI_tuptable;
		nproc = SPI_processed;
		if (nproc == 0 || tuptable == NULL)
			break;
		nest = kingres_shard_set_transmission_modes();
		for (k = 0; k < nproc; k++)
		{
			HeapTuple	tup = tuptable->vals[k];
			int			c;

			for (c = 0; c < ncols; c++)
				values[c] = heap_getattr(tup, c + 1, tuptable->tupdesc, &nulls[c]);
			kingres_shard_batch_add(sender, values, nulls);
		}
		kingres_shard_reset_transmission_modes(nest);
		if (opts != NULL && opts->nkeys > 0)
		{
			HeapTuple	lasttup = tuptable->vals[nproc - 1];
			int			k;
			MemoryContext old = MemoryContextSwitchTo(callercxt);

			if (opts->last == NULL)
				opts->last = palloc0(sizeof(char *) * opts->nkeys);
			for (k = 0; k < opts->nkeys; k++)
			{
				char	   *v = SPI_getvalue(lasttup, tuptable->tupdesc, ncols + 1 + k);

				opts->last[k] = v;
			}
			MemoryContextSwitchTo(old);
		}
		nread += (int64) nproc;
		SPI_freetuptable(tuptable);
	}
	SPI_cursor_close(portal);
	PopActiveSnapshot();
	SPI_freeplan(plan);

	acked = kingres_shard_batch_finish(sender);
	kingres_shard_batch_end(sender);
	AtEOXact_GUC(true, guc_nest);
	SPI_finish();
	kingres_shard_fault_point("copy_after_copy");

	if (rows_read != NULL)
		*rows_read = nread;
	return acked;
}

/* The placement of a colocation group from the catalog tables, as of this transaction. */
KsColocation *
kingres_shard_load_placement(int colocation_id)
{
	Oid			argtypes[1] = {INT4OID};
	Datum		argvalues[1];
	KsColocation *c;
	MemoryContext callercxt = CurrentMemoryContext;
	bool		isnull;
	int			nbuckets;
	Oid			dist_type;
	Oid			collation;
	int		   *groups;
	uint64		i;

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	argvalues[0] = Int32GetDatum(colocation_id);
	if (SPI_execute_with_args("SELECT c.bucket_count, c.dist_column_type::pg_catalog.oid, c.dist_collation "
							  "FROM " KINGRES_SHARD_SCHEMA ".colocation c "
							  "WHERE c.colocation_id OPERATOR(pg_catalog.=) $1",
							  1, argtypes, argvalues, NULL, true, 0) != SPI_OK_SELECT ||
		SPI_processed != 1)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_OBJECT),
				 errmsg("colocation group %d does not exist", colocation_id)));
	nbuckets = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull));
	dist_type = DatumGetObjectId(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 2, &isnull));
	collation = DatumGetObjectId(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 3, &isnull));
	/* in the caller's context: the SPI context goes with SPI_finish(), and the layout is made after it */
	groups = MemoryContextAlloc(callercxt, sizeof(int) * Max(nbuckets, 1));
	for (i = 0; i < (uint64) nbuckets; i++)
		groups[i] = -1;
	if (SPI_execute_with_args("SELECT b.bucket, b.group_id FROM " KINGRES_SHARD_SCHEMA ".bucket_map b "
							  "WHERE b.colocation_id OPERATOR(pg_catalog.=) $1",
							  1, argtypes, argvalues, NULL, true, 0) != SPI_OK_SELECT)
		elog(ERROR, "could not read the bucket map of colocation group %d", colocation_id);
	for (i = 0; i < SPI_processed; i++)
	{
		int			b = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1, &isnull));
		int			g = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2, &isnull));

		if (b >= 0 && b < nbuckets)
			groups[b] = g;
	}
	SPI_finish();
	{
		MemoryContext old = MemoryContextSwitchTo(callercxt);

		c = kingres_shard_coloc_make(colocation_id, nbuckets, dist_type, collation, groups);
		MemoryContextSwitchTo(old);
	}
	return c;
}

/*
 * kingres.internal_copy_routed(source regclass, dest_schema name, dest_table name,
 *								dest_kind text, key_column name, colocation_id int,
 *								node_id int) returns bigint
 *
 * Copy the rows of source to the table dest_schema.dest_table of the nodes that
 * dest_kind names ('placement': the groups of the buckets of the colocation
 * group, by key_column; 'every_node'; 'one_node': node_id).  The destination
 * table must exist on those nodes (made in this transaction or before).  The
 * answer is the number of rows the destination acknowledged; it must equal the
 * number of rows read, or the call fails.  Superusers only (the SQL scripts
 * revoke it from PUBLIC), and not for use outside the table operations.
 */
Datum
kingres_internal_copy_routed(PG_FUNCTION_ARGS)
{
	Oid			source = PG_GETARG_OID(0);
	char	   *dschema = pstrdup(NameStr(*PG_GETARG_NAME(1)));
	char	   *dtable = pstrdup(NameStr(*PG_GETARG_NAME(2)));
	char	   *kindname = text_to_cstring(PG_GETARG_TEXT_PP(3));
	char	   *key = PG_ARGISNULL(4) ? NULL : pstrdup(NameStr(*PG_GETARG_NAME(4)));
	KsDestKind	kind;
	KsColocation *placement = NULL;
	int			node_id = PG_ARGISNULL(6) ? 0 : PG_GETARG_INT32(6);
	int64		nread = 0;
	int64		acked;

	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied: this function is for superusers")));
	if (strcmp(kindname, "placement") == 0)
		kind = KS_DEST_PLACEMENT;
	else if (strcmp(kindname, "every_node") == 0)
		kind = KS_DEST_EVERY_NODE;
	else if (strcmp(kindname, "one_node") == 0)
		kind = KS_DEST_ONE_NODE;
	else
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("unknown destination kind \"%s\"", kindname),
				 errhint("Use placement, every_node or one_node.")));
	if (kind == KS_DEST_PLACEMENT)
	{
		if (PG_ARGISNULL(5))
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("a placement destination needs a colocation group")));
		placement = kingres_shard_load_placement(PG_GETARG_INT32(5));
	}
	if (kind == KS_DEST_ONE_NODE && node_id <= 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("a one_node destination needs a node id")));

	acked = kingres_shard_copy_routed(source, dschema, dtable, kind, placement, key, node_id, &nread);
	if (kind == KS_DEST_EVERY_NODE)
	{
		const KsCatalog *cat = kingres_shard_catalog();
		ListCell   *lc;
		int			nodes = 0;

		foreach(lc, cat->nodes)
		{
			const KsNode *n = (const KsNode *) lfirst(lc);

			if (n->is_primary && n->is_active)
				nodes++;
		}
		/* every node acknowledges every row */
		nread *= nodes;
	}
	if (acked != nread)
		ereport(ERROR,
				(errcode(ERRCODE_DATA_EXCEPTION),
				 errmsg("the copy of table \"%s\" read " INT64_FORMAT " rows and the destination acknowledged " INT64_FORMAT,
						get_rel_name(source), nread, acked)));
	PG_RETURN_INT64(acked);
}

PG_FUNCTION_INFO_V1(kingres_internal_copy_chunk);

/* A text[] argument as C strings (NULL elements are not allowed). */
static char **
text_array_args(ArrayType *arr, int *n)
{
	Datum	   *elems;
	bool	   *nulls;
	int			count;
	char	  **out;
	int			i;

	deconstruct_array_builtin(arr, TEXTOID, &elems, &nulls, &count);
	out = palloc(sizeof(char *) * Max(count, 1));
	for (i = 0; i < count; i++)
	{
		if (nulls[i])
			ereport(ERROR,
					(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
					 errmsg("a key value of a chunk cannot be NULL")));
		out[i] = TextDatumGetCString(elems[i]);
	}
	*n = count;
	return out;
}

/*
 * kingres.internal_copy_chunk(source regclass, dest_schema name, dest_table name,
 *							   dest_kind text, key_column name, colocation_id int, node_id int,
 *							   order_cols text[], after_vals text[], batch bigint,
 *							   where_sql text, conflict_nothing bool) returns jsonb
 *
 * One chunk of the copy or of the replay of an online table operation (design, "Copy" and
 * "Replay"): the rows of source that satisfy where_sql, in the order of order_cols, after the key
 * after_vals, at most batch of them, to the destination that dest_kind names (see
 * internal_copy_routed).  The writes are in the caller's transaction, which is the chunk.  The
 * answer is {"read": n, "acked": m, "last": [key values as text] or null}: acked is what the
 * destination stored (less than read when a repeated chunk met rows that were there).
 * Superusers only.
 */
Datum
kingres_internal_copy_chunk(PG_FUNCTION_ARGS)
{
	Oid			source = PG_GETARG_OID(0);
	char	   *dschema = pstrdup(NameStr(*PG_GETARG_NAME(1)));
	char	   *dtable = pstrdup(NameStr(*PG_GETARG_NAME(2)));
	char	   *kindname = text_to_cstring(PG_GETARG_TEXT_PP(3));
	char	   *key = PG_ARGISNULL(4) ? NULL : pstrdup(NameStr(*PG_GETARG_NAME(4)));
	int			node_id = PG_ARGISNULL(6) ? 0 : PG_GETARG_INT32(6);
	KsDestKind	kind;
	KsColocation *placement = NULL;
	KsCopyOpts	opts;
	int64		nread = 0;
	int64		acked;
	int			nafter = 0;
	StringInfoData out;
	int			i;

	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied: this function is for superusers")));
	if (strcmp(kindname, "placement") == 0)
		kind = KS_DEST_PLACEMENT;
	else if (strcmp(kindname, "every_node") == 0)
		kind = KS_DEST_EVERY_NODE;
	else if (strcmp(kindname, "one_node") == 0)
		kind = KS_DEST_ONE_NODE;
	else
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("unknown destination kind \"%s\"", kindname)));
	if (kind == KS_DEST_PLACEMENT)
	{
		if (PG_ARGISNULL(5))
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("a placement destination needs a colocation group")));
		placement = kingres_shard_load_placement(PG_GETARG_INT32(5));
	}
	if (kind == KS_DEST_ONE_NODE && node_id <= 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("a one_node destination needs a node id")));

	memset(&opts, 0, sizeof(opts));
	if (!PG_ARGISNULL(7))
		opts.keycols = text_array_args(PG_GETARG_ARRAYTYPE_P(7), &opts.nkeys);
	if (!PG_ARGISNULL(8))
	{
		opts.after = text_array_args(PG_GETARG_ARRAYTYPE_P(8), &nafter);
		if (nafter != opts.nkeys)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("the key after which a chunk starts has %d values, the key has %d columns",
							nafter, opts.nkeys)));
	}
	opts.limit = PG_ARGISNULL(9) ? 0 : PG_GETARG_INT64(9);
	opts.where_sql = PG_ARGISNULL(10) ? NULL : text_to_cstring(PG_GETARG_TEXT_PP(10));
	opts.conflict_nothing = !PG_ARGISNULL(11) && PG_GETARG_BOOL(11);

	acked = kingres_shard_copy_routed_ex(source, dschema, dtable, kind, placement, key, node_id,
										 &nread, &opts);
	if (!opts.conflict_nothing)
	{
		int64		want = nread;

		if (kind == KS_DEST_EVERY_NODE)
		{
			const KsCatalog *cat = kingres_shard_catalog();
			ListCell   *lc;
			int			nodes = 0;

			foreach(lc, cat->nodes)
			{
				const KsNode *n = (const KsNode *) lfirst(lc);

				if (n->is_primary && n->is_active)
					nodes++;
			}
			want *= nodes;
		}
		if (acked != want)
			ereport(ERROR,
					(errcode(ERRCODE_DATA_EXCEPTION),
					 errmsg("a chunk of the copy of table \"%s\" read " INT64_FORMAT " rows and the destination stored " INT64_FORMAT,
							get_rel_name(source), want, acked)));
	}

	initStringInfo(&out);
	appendStringInfo(&out, "{\"read\": " INT64_FORMAT ", \"acked\": " INT64_FORMAT ", \"last\": ", nread, acked);
	if (opts.last == NULL)
		appendStringInfoString(&out, "null");
	else
	{
		appendStringInfoChar(&out, '[');
		for (i = 0; i < opts.nkeys; i++)
		{
			if (i > 0)
				appendStringInfoString(&out, ", ");
			if (opts.last[i] == NULL)
				appendStringInfoString(&out, "null");
			else
				escape_json(&out, opts.last[i]);
		}
		appendStringInfoChar(&out, ']');
	}
	appendStringInfoChar(&out, '}');
	PG_RETURN_DATUM(DirectFunctionCall1(jsonb_in, CStringGetDatum(out.data)));
}
