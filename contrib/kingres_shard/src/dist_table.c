/*-------------------------------------------------------------------------
 *
 * dist_table.c
 *	  kingres.create_distributed_table(): turn an empty table into a
 *	  hash-partitioned distributed table placed across the shard groups.
 *
 * A bucket is a hash partition (design/shard-mvp.md, "Physical layout"): the
 * table becomes PARTITION BY HASH (distribution column) with one partition
 * per bucket, named <table>_b<NNNN>, and each node holds the partitions of
 * the buckets its group owns.  The statements that recreate the table (its
 * columns, defaults, constraints and indexes) are produced by the SQL
 * function kingres.internal_table_ddl(), which reads the catalogs; this file
 * chooses the colocation group and the bucket placement, runs the statements
 * locally and on every other node, records the table and synchronizes the
 * catalog.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/dist_table.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "catalog/pg_type_d.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "lib/stringinfo.h"
#include "miscadmin.h"
#include "storage/lmgr.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/typcache.h"

#include "catalog.h"
#include "kingres_shard.h"
#include "dist_table.h"
#include "node_admin.h"
#include "node_conn.h"

PG_FUNCTION_INFO_V1(kingres_create_distributed_table);

/* The statements that recreate the table, by where they run. */
typedef struct DdlSet
{
	StringInfoData local_pre;
	StringInfoData remote_pre;
	StringInfoData both;
	StringInfoData fk;
} DdlSet;

/*
 * Run a statement through SPI and check that it succeeded.
 */
static void
run_spi(const char *sql, int nargs, Oid *argtypes, Datum *values)
{
	int			ret;

	if (nargs == 0)
		ret = SPI_execute(sql, false, 0);
	else
		ret = SPI_execute_with_args(sql, nargs, argtypes, values, NULL, false, 0);
	if (ret < 0)
		elog(ERROR, "SPI command failed (%d): %s", ret, sql);
}

/*
 * fetch_ddl
 *
 * Ask kingres.internal_table_ddl() for the statements that recreate the
 * table, and sort them by scope into three scripts allocated in cxt.  Must
 * run before the old table is dropped.
 */
static void
fetch_ddl(Oid relid, const char *column, DdlSet *ddl, MemoryContext cxt)
{
	Oid			argtypes[2] = {OIDOID, TEXTOID};
	Datum		values[2];
	uint64		i;
	MemoryContext oldcxt = MemoryContextSwitchTo(cxt);

	initStringInfo(&ddl->local_pre);
	initStringInfo(&ddl->remote_pre);
	initStringInfo(&ddl->both);
	initStringInfo(&ddl->fk);
	MemoryContextSwitchTo(oldcxt);

	values[0] = ObjectIdGetDatum(relid);
	values[1] = column != NULL ? CStringGetTextDatum(column) : (Datum) 0;
	{
		char		nulls[2] = {' ', column != NULL ? ' ' : 'n'};

		if (SPI_execute_with_args("SELECT scope, stmt FROM " KINGRES_SHARD_SCHEMA ".internal_table_ddl("
								  "$1::pg_catalog.oid::pg_catalog.regclass, $2::pg_catalog.name) ORDER BY ord",
								  2, argtypes, values, nulls, false, 0) < 0)
			elog(ERROR, "could not read the statements that recreate table %u", relid);
	}

	for (i = 0; i < SPI_processed; i++)
	{
		char	   *scope = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1);
		char	   *stmt = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2);
		StringInfo	target;

		if (strcmp(scope, "local_pre") == 0)
			target = &ddl->local_pre;
		else if (strcmp(scope, "fk") == 0)
			target = &ddl->fk;
		else if (strcmp(scope, "remote_pre") == 0)
			target = &ddl->remote_pre;
		else
			target = &ddl->both;

		oldcxt = MemoryContextSwitchTo(cxt);
		appendStringInfo(target, "%s;\n", stmt);
		MemoryContextSwitchTo(oldcxt);
	}
}

/*
 * kingres_shard_remote_table_script
 *
 * The script that creates the parent of an existing distributed table (with
 * its columns, constraints and indexes, but no partitions) on a node that
 * does not have it, for example a node that joins the cluster after the
 * table was distributed.  Returned in the caller's memory context.
 */
char *
kingres_shard_remote_table_script(Oid relid, const char *column)
{
	MemoryContext callercxt = CurrentMemoryContext;
	DdlSet		ddl;
	StringInfoData script;

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	fetch_ddl(relid, column, &ddl, callercxt);
	SPI_finish();

	initStringInfo(&script);
	appendStringInfoString(&script, ddl.remote_pre.data);
	appendStringInfoString(&script, ddl.both.data);
	return script.data;
}

/*
 * kingres_shard_remote_fk_script
 *
 * The statements that add the foreign keys of a table on a node that got its
 * tables from kingres_shard_remote_table_script(): they run after every table
 * exists there.  An empty string when the table has none.
 */
char *
kingres_shard_remote_fk_script(Oid relid, const char *column)
{
	MemoryContext callercxt = CurrentMemoryContext;
	DdlSet		ddl;

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	fetch_ddl(relid, column, &ddl, callercxt);
	SPI_finish();
	return ddl.fk.data;
}

/*
 * append_partitions
 *
 * Append the CREATE TABLE ... PARTITION OF statements for the buckets that
 * group owns.
 */
static void
append_partitions(StringInfo buf, const char *nsp, const char *rel,
				  int nbuckets, const int *groups, int group)
{
	int			b;

	for (b = 0; b < nbuckets; b++)
	{
		char	   *part;

		if (groups[b] != group)
			continue;
		part = psprintf("%s_b%04d", rel, b);
		appendStringInfo(buf,
						 "CREATE TABLE %s.%s PARTITION OF %s.%s FOR VALUES WITH (MODULUS %d, REMAINDER %d);\n",
						 quote_identifier(nsp), quote_identifier(part),
						 quote_identifier(nsp), quote_identifier(rel),
						 nbuckets, b);
		pfree(part);
	}
}

/*
 * choose_colocation
 *
 * Decide the colocation group of the new table: the group of colocate_with,
 * else an existing group with the same type, collation and bucket count,
 * else a new group whose buckets are spread round-robin over the groups that
 * should hold shards, in group order.  Returns the colocation id and fills
 * *bucket_count.
 */
static int
choose_colocation(Oid coltype, Oid collation, Oid colocate_with,
				  int requested_buckets, int *bucket_count)
{
	Oid			argtypes[4];
	Datum		values[4];
	int			colocation_id;
	int			nbuckets;
	bool		isnull;

	if (OidIsValid(colocate_with))
	{
		const KsDistTable *other = kingres_shard_lookup_dist_table(colocate_with);

		if (other == NULL || other->is_reference)
			ereport(ERROR,
					(errcode(ERRCODE_WRONG_OBJECT_TYPE),
					 errmsg("table \"%s\" is not a distributed table",
							get_rel_name(colocate_with))));
		if (other->coloc->dist_type != coltype ||
			other->coloc->collation != collation)
			ereport(ERROR,
					(errcode(ERRCODE_DATATYPE_MISMATCH),
					 errmsg("cannot colocate with table \"%s\": the distribution column types differ",
							get_rel_name(colocate_with))));
		if (requested_buckets > 0 &&
			requested_buckets != other->coloc->bucket_count)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("bucket_count %d differs from the %d buckets of table \"%s\"",
							requested_buckets, other->coloc->bucket_count,
							get_rel_name(colocate_with))));
		*bucket_count = other->coloc->bucket_count;
		return other->coloc->colocation_id;
	}

	nbuckets = requested_buckets > 0 ? requested_buckets :
		kingres_shard_default_bucket_count;
	if (nbuckets < 1 || nbuckets > 4096)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("bucket_count must be between 1 and 4096")));
	*bucket_count = nbuckets;

	argtypes[0] = OIDOID;
	argtypes[1] = OIDOID;
	argtypes[2] = INT4OID;
	values[0] = ObjectIdGetDatum(coltype);
	values[1] = ObjectIdGetDatum(collation);
	values[2] = Int32GetDatum(nbuckets);
	run_spi("SELECT colocation_id FROM " KINGRES_SHARD_SCHEMA ".colocation "
			"WHERE dist_column_type::pg_catalog.oid = $1 AND dist_collation = $2 "
			"AND bucket_count = $3 ORDER BY colocation_id LIMIT 1",
			3, argtypes, values);
	if (SPI_processed > 0)
		return DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[0],
										   SPI_tuptable->tupdesc, 1, &isnull));

	/* A new group: assign the buckets round-robin over the eligible groups. */
	{
		StringInfoData arr;
		uint64		i;

		run_spi("SELECT DISTINCT group_id FROM " KINGRES_SHARD_SCHEMA ".node "
				"WHERE role = 'primary' AND is_active AND should_have_shards "
				"ORDER BY group_id",
				0, NULL, NULL);
		if (SPI_processed == 0)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("no shard group can hold buckets"),
					 errhint("Register a node with kingres.add_node() or set should_have_shards.")));
		initStringInfo(&arr);
		appendStringInfoChar(&arr, '{');
		for (i = 0; i < SPI_processed; i++)
			appendStringInfo(&arr, "%s%d", i ? "," : "",
							 DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[i],
														 SPI_tuptable->tupdesc, 1,
														 &isnull)));
		appendStringInfoChar(&arr, '}');

		run_spi("SELECT coalesce(max(colocation_id), 0) + 1 FROM " KINGRES_SHARD_SCHEMA ".colocation",
				0, NULL, NULL);
		colocation_id = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[0],
													SPI_tuptable->tupdesc, 1,
													&isnull));

		argtypes[0] = INT4OID;
		argtypes[1] = INT4OID;
		argtypes[2] = OIDOID;
		argtypes[3] = OIDOID;
		values[0] = Int32GetDatum(colocation_id);
		values[1] = Int32GetDatum(nbuckets);
		values[2] = ObjectIdGetDatum(coltype);
		values[3] = ObjectIdGetDatum(collation);
		run_spi("INSERT INTO " KINGRES_SHARD_SCHEMA ".colocation "
				"VALUES ($1, $2, $3::pg_catalog.oid::pg_catalog.regtype, $4)",
				4, argtypes, values);

		argtypes[0] = INT4OID;
		argtypes[1] = TEXTOID;
		argtypes[2] = INT4OID;
		values[0] = Int32GetDatum(colocation_id);
		values[1] = CStringGetTextDatum(arr.data);
		values[2] = Int32GetDatum(nbuckets);
		run_spi("INSERT INTO " KINGRES_SHARD_SCHEMA ".bucket_map (colocation_id, bucket, group_id) "
				"SELECT $1, g, (a.groups)[1 + g % pg_catalog.cardinality(a.groups)] "
				"FROM (SELECT $2::pg_catalog.text::pg_catalog.int4[] AS groups) a, "
				"pg_catalog.generate_series(0, $3 - 1) g",
				3, argtypes, values);
	}
	return colocation_id;
}

/*
 * kingres.create_distributed_table(relation regclass, dist_column name,
 *									 colocate_with regclass, bucket_count int)
 *
 * See the file header.  Everything happens in the caller's transaction: the
 * local DDL is transactional, and the remote nodes' work is committed in the
 * pre-commit callback (not atomically, see node_admin.c).
 */
Datum
kingres_create_distributed_table(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	char	   *column = pstrdup(NameStr(*PG_GETARG_NAME(1)));
	Oid			colocate_with = PG_ARGISNULL(2) ? InvalidOid : PG_GETARG_OID(2);
	int			requested = PG_ARGISNULL(3) ? 0 : PG_GETARG_INT32(3);
	MemoryContext callercxt = CurrentMemoryContext;
	char	   *relname;
	char	   *nspname;
	Oid			nspoid;
	Oid			coltype;
	Oid			collation;
	TypeCacheEntry *tce;
	int			colocation_id;
	int			nbuckets;
	int		   *groups;
	int			local_group;
	DdlSet		ddl;
	Oid			argtypes[3];
	Datum		values[3];
	bool		isnull;
	uint64		i;
	List	   *targets = NIL;
	ListCell   *lc;
	StringInfoData script;
	Oid			newrelid;

	kingres_shard_lock_catalog();
	kingres_shard_require_map_group();
	local_group = kingres_shard_catalog()->local_group_id;

	if (requested < 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("bucket_count must be positive")));

	LockRelationOid(relid, AccessExclusiveLock);
	relname = pstrdup(get_rel_name(relid));
	nspoid = get_rel_namespace(relid);
	nspname = pstrdup(get_namespace_name(nspoid));

	/* Allocated in the caller's context: it outlives the SPI connection. */
	initStringInfo(&script);

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	/* 1. Validate the table and the column. */
	argtypes[0] = OIDOID;
	argtypes[1] = TEXTOID;
	values[0] = ObjectIdGetDatum(relid);
	values[1] = CStringGetTextDatum(column);
	run_spi("SELECT column_type, column_collation FROM " KINGRES_SHARD_SCHEMA
			".internal_check_distributable($1::pg_catalog.oid::pg_catalog.regclass, "
			"$2::pg_catalog.name)",
			2, argtypes, values);
	coltype = DatumGetObjectId(SPI_getbinval(SPI_tuptable->vals[0],
											 SPI_tuptable->tupdesc, 1, &isnull));
	collation = DatumGetObjectId(SPI_getbinval(SPI_tuptable->vals[0],
											   SPI_tuptable->tupdesc, 2, &isnull));

	tce = lookup_type_cache(coltype, TYPECACHE_HASH_EXTENDED_PROC);
	if (!OidIsValid(tce->hash_extended_proc))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("column \"%s\" of type %s cannot be a distribution column",
						column, format_type_be(coltype)),
				 errdetail("The type has no default hash operator class with an extended hash function.")));

	/* 2. The colocation group and the placement of its buckets. */
	colocation_id = choose_colocation(coltype, collation, colocate_with,
									  requested, &nbuckets);

	groups = MemoryContextAlloc(callercxt, sizeof(int) * nbuckets);
	for (i = 0; i < (uint64) nbuckets; i++)
		groups[i] = -1;
	argtypes[0] = INT4OID;
	values[0] = Int32GetDatum(colocation_id);
	run_spi("SELECT bucket, group_id FROM " KINGRES_SHARD_SCHEMA ".bucket_map "
			"WHERE colocation_id = $1 ORDER BY bucket",
			1, argtypes, values);
	for (i = 0; i < SPI_processed; i++)
	{
		int			b = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[i],
													SPI_tuptable->tupdesc, 1, &isnull));
		int			g = DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[i],
													SPI_tuptable->tupdesc, 2, &isnull));

		if (b >= 0 && b < nbuckets)
			groups[b] = g;
	}

	/* 3. The statements that recreate the table (before it is dropped). */
	fetch_ddl(relid, column, &ddl, callercxt);

	/* 4a. Recreate the table here with the partitions of this group. */
	if (ddl.local_pre.len > 0)
		run_spi(ddl.local_pre.data, 0, NULL, NULL);
	{
		StringInfoData drop;

		initStringInfo(&drop);
		appendStringInfo(&drop, "DROP TABLE %s.%s",
						 quote_identifier(nspname), quote_identifier(relname));
		run_spi(drop.data, 0, NULL, NULL);
	}
	resetStringInfo(&script);
	appendStringInfoString(&script, ddl.both.data);
	append_partitions(&script, nspname, relname, nbuckets, groups, local_group);
	run_spi(script.data, 0, NULL, NULL);

	newrelid = get_relname_relid(relname, nspoid);
	if (!OidIsValid(newrelid))
		elog(ERROR, "could not find the new table \"%s\"", relname);

	/* 5. Record the table. */
	argtypes[0] = OIDOID;
	argtypes[1] = TEXTOID;
	argtypes[2] = INT4OID;
	values[0] = ObjectIdGetDatum(newrelid);
	values[1] = CStringGetTextDatum(column);
	values[2] = Int32GetDatum(colocation_id);
	run_spi("INSERT INTO " KINGRES_SHARD_SCHEMA ".dist_table "
			"VALUES ($1::pg_catalog.oid::pg_catalog.regclass, 'hash', "
			"$2::pg_catalog.name, $3)",
			3, argtypes, values);
	SPI_finish();

	/*
	 * 4b. The other nodes.  The node list is read after the catalog changes
	 * above so that it is current; each node gets the table and the
	 * partitions of its own group.
	 */
	{
		const KsCatalog *cat = kingres_shard_catalog();

		foreach(lc, cat->nodes)
		{
			KsNode	   *n = (KsNode *) lfirst(lc);
			KsNode	   *copy;

			if (n->node_id == cat->local_node_id || !n->is_active || !n->is_primary)
				continue;
			copy = MemoryContextAlloc(callercxt, sizeof(KsNode));
			*copy = *n;
			copy->host = MemoryContextStrdup(callercxt, n->host);
			targets = lappend(targets, copy);
		}
	}
	foreach(lc, targets)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		KsConn	   *kc = kingres_shard_get_conn(n->host, n->port, false, NULL);
		PGresult   *res;

		kingres_shard_begin_write_xact(kc);
		resetStringInfo(&script);
		appendStringInfoString(&script, ddl.remote_pre.data);
		appendStringInfoString(&script, ddl.both.data);
		append_partitions(&script, nspname, relname, nbuckets, groups, n->group_id);
		res = kingres_shard_exec(kc, script.data);
		PQclear(res);
	}

	/* 6. Make every node know the table. */
	kingres_shard_sync_catalog_internal();

	PG_RETURN_VOID();
}

PG_FUNCTION_INFO_V1(kingres_create_reference_table);

/*
 * kingres_shard_copy_table_rows
 *
 * Copy every row of a table of this node to the tables of the same name on
 * the nodes of conns (a list of KsConn *), whose transactions are open.  The
 * rows are read through a cursor and written in multi-row INSERTs with
 * parameters in text form, under the transmission modes, so that every value
 * means the same on the other side; generated columns are left to be computed
 * there, identity columns are copied with their values.
 */
void
kingres_shard_copy_table_rows(Oid relid, List *conns)
{
	char	   *nsp = get_namespace_name(get_rel_namespace(relid));
	char	   *rel = get_rel_name(relid);
	StringInfoData collist;
	StringInfoData query;
	Oid			argtypes[1] = {OIDOID};
	Datum		values[1];
	int			ncols;
	uint64		i;
	Portal		portal;
	int			rows_per_batch;
	int			nest;
	SPIPlanPtr	plan;
	MemoryContext batchcxt;
	MemoryContext oldcxt;

	if (conns == NIL)
		return;
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	values[0] = ObjectIdGetDatum(relid);
	run_spi("SELECT pg_catalog.quote_ident(attname::pg_catalog.text) FROM pg_catalog.pg_attribute "
			"WHERE attrelid OPERATOR(pg_catalog.=) $1 AND attnum OPERATOR(pg_catalog.>) 0 "
			"AND NOT attisdropped AND attgenerated OPERATOR(pg_catalog.=) '' ORDER BY attnum",
			1, argtypes, values);
	ncols = (int) SPI_processed;
	if (ncols == 0)
	{
		SPI_finish();
		return;
	}
	initStringInfo(&collist);
	for (i = 0; i < SPI_processed; i++)
		appendStringInfo(&collist, "%s%s", i ? ", " : "",
						 SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1));

	initStringInfo(&query);
	appendStringInfo(&query, "SELECT %s FROM %s.%s", collist.data,
					 quote_identifier(nsp), quote_identifier(rel));
	plan = SPI_prepare(query.data, 0, NULL);
	if (plan == NULL)
		elog(ERROR, "could not prepare the copy of table \"%s\"", rel);
	portal = SPI_cursor_open(NULL, plan, NULL, NULL, true);

	rows_per_batch = Max(1, 20000 / ncols);
	batchcxt = AllocSetContextCreate(CurrentMemoryContext, "kingres_shard row copy batch",
									 ALLOCSET_DEFAULT_SIZES);
	nest = kingres_shard_set_transmission_modes();
	for (;;)
	{
		StringInfoData sql;
		const char **params;
		int			nrows;
		int			r;
		int			c;
		ListCell   *lc;

		SPI_cursor_fetch(portal, true, rows_per_batch);
		if (SPI_processed == 0)
			break;
		nrows = (int) SPI_processed;

		oldcxt = MemoryContextSwitchTo(batchcxt);
		params = palloc(sizeof(char *) * nrows * ncols);
		initStringInfo(&sql);
		appendStringInfo(&sql, "INSERT INTO %s.%s (%s) OVERRIDING SYSTEM VALUE VALUES ",
						 quote_identifier(nsp), quote_identifier(rel), collist.data);
		for (r = 0; r < nrows; r++)
		{
			appendStringInfo(&sql, "%s(", r ? ", " : "");
			for (c = 0; c < ncols; c++)
			{
				appendStringInfo(&sql, "%s$%d", c ? ", " : "", r * ncols + c + 1);
				params[r * ncols + c] = SPI_getvalue(SPI_tuptable->vals[r],
													 SPI_tuptable->tupdesc, c + 1);
			}
			appendStringInfoChar(&sql, ')');
		}
		MemoryContextSwitchTo(oldcxt);

		foreach(lc, conns)
		{
			PGresult   *res = kingres_shard_exec_params((KsConn *) lfirst(lc), sql.data,
														 nrows * ncols, NULL, params, true);

			PQclear(res);
		}
		MemoryContextReset(batchcxt);
		SPI_freetuptable(SPI_tuptable);
	}
	kingres_shard_reset_transmission_modes(nest);
	SPI_cursor_close(portal);
	MemoryContextDelete(batchcxt);
	SPI_finish();
}

/*
 * kingres.create_reference_table(relation regclass) returns void
 *
 * Make an ordinary table a reference table: every node gets a full copy, in
 * the same distributed transaction as the catalog change, and from then on
 * writes to it run on every node.  Map group primary only.  Writers are
 * blocked while the rows are copied (an EXCLUSIVE lock); readers are not.
 */
Datum
kingres_create_reference_table(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	MemoryContext callercxt = CurrentMemoryContext;
	Oid			argtypes[1] = {OIDOID};
	Datum		values[1];
	DdlSet		ddl;
	List	   *targets = NIL;
	List	   *conns = NIL;
	ListCell   *lc;
	StringInfoData script;

	kingres_shard_lock_catalog();
	kingres_shard_require_map_group();
	LockRelationOid(relid, ExclusiveLock);

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	values[0] = ObjectIdGetDatum(relid);
	run_spi("SELECT " KINGRES_SHARD_SCHEMA ".internal_check_referenceable("
			"$1::pg_catalog.oid::pg_catalog.regclass)", 1, argtypes, values);
	fetch_ddl(relid, NULL, &ddl, callercxt);
	run_spi("INSERT INTO " KINGRES_SHARD_SCHEMA ".dist_table (relid, method) "
			"VALUES ($1::pg_catalog.oid::pg_catalog.regclass, 'reference')",
			1, argtypes, values);
	SPI_finish();

	initStringInfo(&script);
	appendStringInfoString(&script, ddl.remote_pre.data);
	appendStringInfoString(&script, ddl.both.data);

	{
		const KsCatalog *cat = kingres_shard_catalog();

		foreach(lc, cat->nodes)
		{
			KsNode	   *n = (KsNode *) lfirst(lc);
			KsNode	   *copy;

			if (n->node_id == cat->local_node_id || !n->is_active || !n->is_primary)
				continue;
			copy = MemoryContextAlloc(callercxt, sizeof(KsNode));
			*copy = *n;
			copy->host = MemoryContextStrdup(callercxt, n->host);
			targets = lappend(targets, copy);
		}
	}
	foreach(lc, targets)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		KsConn	   *kc = kingres_shard_get_conn(n->host, n->port, false, NULL);
		PGresult   *res;

		kingres_shard_begin_write_xact(kc);
		res = kingres_shard_exec(kc, script.data);
		PQclear(res);
		conns = lappend(conns, kc);
	}
	kingres_shard_copy_table_rows(relid, conns);

	kingres_shard_sync_catalog_internal();
	PG_RETURN_VOID();
}
