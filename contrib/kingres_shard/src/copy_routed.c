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
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"

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
	spec.conflict = KS_CONFLICT_NONE;
	spec.overriding_system_value = identity;

	/*
	 * The text of a money value depends on lc_monetary, which the transmission modes do not
	 * fix: both ends of the copy use "C" (the other nodes get SET LOCAL lc_monetary in the
	 * transaction that creates the destination), for the whole of the copy, local batches included.
	 */
	guc_nest = NewGUCNestLevel();
	(void) set_config_option("lc_monetary", "C", PGC_USERSET, PGC_S_SESSION,
							 GUC_ACTION_SAVE, true, 0, false);
	sender = kingres_shard_batch_begin(&dest, &spec);

	initStringInfo(&query);
	appendStringInfo(&query, "SELECT %s FROM %s", collist.data,
					 quote_qualified_identifier(src_schema, src_table));
	plan = SPI_prepare(query.data, 0, NULL);
	if (plan == NULL)
		elog(ERROR, "could not prepare the read of table \"%s\"", src_table);
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
		nread += (int64) nproc;
		SPI_freetuptable(tuptable);
	}
	SPI_cursor_close(portal);
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
static KsColocation *
load_placement(int colocation_id)
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
	groups = palloc(sizeof(int) * Max(nbuckets, 1));
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
		placement = load_placement(PG_GETARG_INT32(5));
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
