/*-------------------------------------------------------------------------
 *
 * dist_table.c
 *	  kingres.create_distributed_table(): turn a table, empty or not, into a
 *	  hash-partitioned distributed table placed across the shard groups, and
 *	  kingres.create_reference_table().
 *
 * A bucket is a hash partition (design/shard-mvp.md, "Physical layout"): the
 * table becomes PARTITION BY HASH (distribution column) with one partition
 * per bucket, named <table>_b<NNNN>, and each node holds the partitions of
 * the buckets its group owns.  The statements that recreate the table (its
 * columns, defaults, constraints and indexes) are produced by the SQL
 * function kingres.internal_table_ddl(), which reads the catalogs; this file
 * chooses the colocation group and the bucket placement, runs the statements
 * locally and on every other node, copies the rows, records the table and
 * synchronizes the catalog.
 *
 * The table that holds rows (design/shard-phase9.md, "Blocking variant") is
 * replaced in the caller's transaction, shadow first: the new table is made
 * under the name kingres_shadow_<oid> on every node while the old one stays
 * in place for readers, the rows are copied to their buckets through the batch
 * sender (copy_routed.c), counted against the table under an EXCLUSIVE lock
 * (writers wait for the whole operation, readers do not), and then, in one
 * step that readers wait for, the old table is dropped and the shadow takes its
 * name, its sequences, its foreign keys, its owner, privileges and comments.
 * Everything is in the caller's transaction: a failure anywhere is an error
 * that rolls back every node, and the old table is as it was.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
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
#include "utils/guc.h"
#include "utils/inval.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/snapmgr.h"
#include "utils/typcache.h"

#include "bucket_move.h"
#include "catalog.h"
#include "copy_routed.h"
#include "kingres_shard.h"
#include "dist_table.h"
#include "node_admin.h"
#include "node_conn.h"
#include "utility.h"

PG_FUNCTION_INFO_V1(kingres_create_distributed_table);
PG_FUNCTION_INFO_V1(kingres_internal_has_hash_support);

/*
 * Run a statement through SPI and check that it succeeded.
 */
void
kingres_shard_run_spi(const char *sql, int nargs, Oid *argtypes, Datum *values)
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
 * kingres.internal_has_hash_support(type regtype) returns bool
 *
 * Whether the type has a default hash operator class with an extended hash
 * function: what the bucket of a value needs.
 */
Datum
kingres_internal_has_hash_support(PG_FUNCTION_ARGS)
{
	Oid			type = PG_GETARG_OID(0);
	TypeCacheEntry *tce = lookup_type_cache(type, TYPECACHE_HASH_EXTENDED_PROC);

	PG_RETURN_BOOL(OidIsValid(tce->hash_extended_proc));
}

/*
 * fetch_ddl
 *
 * Ask kingres.internal_table_ddl() for the statements that recreate the
 * table, and sort them by scope into scripts allocated in cxt.  With shadow
 * the new table is made under a name that carries the OID of the old one.
 * Must run before the old table is dropped.
 */
void
kingres_shard_fetch_ddl(Oid relid, const char *column, bool shadow, DdlSet *ddl, MemoryContext cxt)
{
	Oid			argtypes[3] = {OIDOID, TEXTOID, BOOLOID};
	Datum		values[3];
	uint64		i;
	MemoryContext oldcxt = MemoryContextSwitchTo(cxt);

	initStringInfo(&ddl->local_pre);
	initStringInfo(&ddl->remote_pre);
	initStringInfo(&ddl->both);
	initStringInfo(&ddl->swap);
	initStringInfo(&ddl->fk);
	initStringInfo(&ddl->post);
	initStringInfo(&ddl->post_local);
	MemoryContextSwitchTo(oldcxt);

	values[0] = ObjectIdGetDatum(relid);
	values[1] = column != NULL ? CStringGetTextDatum(column) : (Datum) 0;
	values[2] = BoolGetDatum(shadow);
	{
		char		nulls[3] = {' ', column != NULL ? ' ' : 'n', ' '};

		/*
		 * Without the shadow the two-argument form is called: it exists in every version, so that a node
		 * can be added to a cluster whose objects are not upgraded yet.
		 */
		if (SPI_execute_with_args(shadow ?
								  "SELECT scope, stmt FROM " KINGRES_SHARD_SCHEMA ".internal_table_ddl("
								  "$1::pg_catalog.oid::pg_catalog.regclass, $2::pg_catalog.name, $3) ORDER BY ord" :
								  "SELECT scope, stmt FROM " KINGRES_SHARD_SCHEMA ".internal_table_ddl("
								  "$1::pg_catalog.oid::pg_catalog.regclass, $2::pg_catalog.name) ORDER BY ord",
								  shadow ? 3 : 2, argtypes, values, nulls, false, 0) < 0)
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
		else if (strcmp(scope, "swap") == 0)
			target = &ddl->swap;
		else if (strcmp(scope, "post") == 0)
			target = &ddl->post;
		else if (strcmp(scope, "post_local") == 0)
			target = &ddl->post_local;
		else
			target = &ddl->both;

		oldcxt = MemoryContextSwitchTo(cxt);
		appendStringInfo(target, "%s;\n", stmt);
		MemoryContextSwitchTo(oldcxt);
	}
}

/*
 * The functions of the table lifecycle use SQL objects that version 0.7 made.  A database whose
 * extension is older than the library (the library is loaded first, ALTER EXTENSION UPDATE comes
 * after it, on every node) gets a clear refusal instead of a missing-function error.
 */
static void
require_current_sql_objects(void)
{
	if (!OidIsValid(kingres_shard_catalog_relid("table_operation_lock")))
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("the kingres_shard objects of this database are older than the library"),
				 errhint("Run ALTER EXTENSION kingres_shard UPDATE on every node.")));
}

/*
 * The copy reads the rows with a snapshot taken after the lock that stops the writers (copy_routed.c).
 * Only READ COMMITTED gives a new snapshot to take; a transaction that holds one snapshot for its whole
 * life would copy the rows as they were before the lock and lose what a writer committed while the call
 * waited for the lock.
 */
static void
require_read_committed(void)
{
	if (IsolationUsesXactSnapshot())
		ereport(ERROR,
				(errcode(ERRCODE_ACTIVE_SQL_TRANSACTION),
				 errmsg("a table cannot be distributed in a REPEATABLE READ or SERIALIZABLE transaction"),
				 errdetail("The rows are copied as of the moment the writers are stopped, which a transaction snapshot taken earlier does not show."),
				 errhint("Run the call in a READ COMMITTED transaction.")));
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
	kingres_shard_fetch_ddl(relid, column, false, &ddl, callercxt);
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
	kingres_shard_fetch_ddl(relid, column, false, &ddl, callercxt);
	SPI_finish();
	return ddl.fk.data;
}

/*
 * append_partitions
 *
 * Append the CREATE TABLE ... PARTITION OF statements for the buckets that
 * group owns: partition <prefix>_b<NNNN> of table nsp.parent, with the modulus
 * of the bucket (catalog.c).
 */
void
kingres_shard_append_partitions(StringInfo buf, const char *nsp, const char *prefix, const char *parent,
				  const KsColocation *coloc, int group)
{
	int			i;

	for (i = 0; i < kingres_shard_coloc_nbuckets(coloc); i++)
	{
		int			b = kingres_shard_coloc_bucket(coloc, i);
		char	   *part;

		if (kingres_shard_bucket_group(coloc, b) != group)
			continue;
		part = psprintf("%s_b%04d", prefix, b);
		appendStringInfo(buf,
						 "CREATE TABLE %s.%s PARTITION OF %s.%s FOR VALUES WITH (MODULUS %d, REMAINDER %d);\n",
						 quote_identifier(nsp), quote_identifier(part),
						 quote_identifier(nsp), quote_identifier(parent),
						 kingres_shard_bucket_modulus(coloc, b), b);
		pfree(part);
	}
}

/*
 * append_partition_tree
 *
 * As append_partitions, for a table the user partitioned: the bucket partition is created PARTITION BY <partby>
 * and gets one leaf <name>_b<NNNN> per user partition, with the bound of the user partition.  The leaves are
 * created empty (a table is rebuilt into a shadow), so CREATE TABLE ... PARTITION OF is right here.
 */
void
kingres_shard_append_partition_tree(StringInfo buf, const char *nsp, const char *prefix, const char *parent,
									const KsColocation *coloc, int group, const char *partby,
									List *leafnames, List *leafbounds)
{
	int			i;

	for (i = 0; i < kingres_shard_coloc_nbuckets(coloc); i++)
	{
		int			b = kingres_shard_coloc_bucket(coloc, i);
		char	   *part;
		ListCell   *ln;
		ListCell   *lb;

		if (kingres_shard_bucket_group(coloc, b) != group)
			continue;
		part = psprintf("%s_b%04d", prefix, b);
		appendStringInfo(buf,
						 "CREATE TABLE %s.%s PARTITION OF %s.%s FOR VALUES WITH (MODULUS %d, REMAINDER %d) PARTITION BY %s;\n",
						 quote_identifier(nsp), quote_identifier(part),
						 quote_identifier(nsp), quote_identifier(parent),
						 kingres_shard_bucket_modulus(coloc, b), b, partby);
		forboth(ln, leafnames, lb, leafbounds)
		{
			char	   *leaf = psprintf("%s_b%04d", (char *) lfirst(ln), b);

			appendStringInfo(buf, "CREATE TABLE %s.%s PARTITION OF %s.%s %s%s;\n",
							 quote_identifier(nsp), quote_identifier(leaf),
							 quote_identifier(nsp), quote_identifier(part),
							 strcmp((char *) lfirst(lb), "DEFAULT") == 0 ? "" : "FOR VALUES ",
							 (char *) lfirst(lb));
			pfree(leaf);
		}
		pfree(part);
	}
}

/*
 * choose_colocation
 *
 * Decide the colocation group of the new table: the group of colocate_with,
 * else an existing group with the same type, collation and bucket count,
 * else a new group whose buckets are spread round-robin over the groups that
 * should hold shards, in group order.  The id of a new group comes from the
 * sequence kingres.colocation_id_seq, so that an id is never used twice.
 * Returns the colocation id and fills *bucket_count.
 */
int
kingres_shard_choose_colocation(Oid coltype, Oid collation, Oid colocate_with,
								int requested_buckets, int *bucket_count, bool *created)
{
	Oid			argtypes[4];
	Datum		values[4];
	int			colocation_id;
	int			nbuckets;
	bool		isnull;

	if (created != NULL)
		*created = false;
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
	kingres_shard_run_spi("SELECT colocation_id FROM " KINGRES_SHARD_SCHEMA ".colocation "
			"WHERE dist_column_type::pg_catalog.oid = $1 AND dist_collation = $2 "
			"AND bucket_count = $3 "
			"AND NOT EXISTS (SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".table_operation_lock l "
			"WHERE l.colocation_id OPERATOR(pg_catalog.=) colocation.colocation_id) "
			"ORDER BY colocation_id LIMIT 1",
			3, argtypes, values);
	if (SPI_processed > 0)
		return DatumGetInt32(SPI_getbinval(SPI_tuptable->vals[0],
										   SPI_tuptable->tupdesc, 1, &isnull));

	/* A new group: assign the buckets round-robin over the eligible groups. */
	{
		StringInfoData arr;
		uint64		i;

		kingres_shard_run_spi("SELECT DISTINCT group_id FROM " KINGRES_SHARD_SCHEMA ".node "
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

		kingres_shard_run_spi("SELECT pg_catalog.nextval('" KINGRES_SHARD_SCHEMA ".colocation_id_seq')::pg_catalog.int4",
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
		if (created != NULL)
			*created = true;
		kingres_shard_run_spi("INSERT INTO " KINGRES_SHARD_SCHEMA ".colocation "
				"VALUES ($1, $2, $3::pg_catalog.oid::pg_catalog.regtype, $4)",
				4, argtypes, values);

		argtypes[0] = INT4OID;
		argtypes[1] = TEXTOID;
		argtypes[2] = INT4OID;
		values[0] = Int32GetDatum(colocation_id);
		values[1] = CStringGetTextDatum(arr.data);
		values[2] = Int32GetDatum(nbuckets);
		kingres_shard_run_spi("INSERT INTO " KINGRES_SHARD_SCHEMA ".bucket_map (colocation_id, bucket, group_id) "
				"SELECT $1, g, (a.groups)[1 + g % pg_catalog.cardinality(a.groups)] "
				"FROM (SELECT $2::pg_catalog.text::pg_catalog.int4[] AS groups) a, "
				"pg_catalog.generate_series(0, $3 - 1) g",
				3, argtypes, values);
	}
	return colocation_id;
}

/*
 * The nodes other than this one that are active primaries, copied into the
 * caller's context (the cached catalog is replaced when the catalog changes).
 */
List *
kingres_shard_remote_primaries(MemoryContext callercxt)
{
	const KsCatalog *cat = kingres_shard_catalog();
	List	   *targets = NIL;
	ListCell   *lc;

	foreach(lc, cat->nodes)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		KsNode	   *copy;

		if (n->node_id == cat->local_node_id || !n->is_active || !n->is_primary)
			continue;
		copy = MemoryContextAlloc(callercxt, sizeof(KsNode));
		*copy = *n;
		copy->host = MemoryContextStrdup(callercxt, n->host);
		{
			MemoryContext old = MemoryContextSwitchTo(callercxt);

			targets = lappend(targets, copy);
			MemoryContextSwitchTo(old);
		}
	}
	return targets;
}

/* Run a script on a node inside its open transaction branch. */
void
kingres_shard_exec_on_node(KsConn *kc, const char *script)
{
	PGresult   *res;

	if (script[0] == '\0')
		return;
	res = kingres_shard_exec(kc, script);
	PQclear(res);
}

/*
 * The statements that run on another node after the copy: the swap, the foreign
 * keys (with the routing checks of this extension off in that transaction: the
 * report has checked every key, and the table is not in the catalog of that node
 * yet), then sequence values, privileges and comments.
 */
char *
kingres_shard_remote_tail_script(const DdlSet *ddl)
{
	StringInfoData s;

	initStringInfo(&s);
	appendStringInfoString(&s, ddl->swap.data);
	if (ddl->fk.len > 0)
	{
		appendStringInfoString(&s, "SELECT pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);\n");
		appendStringInfoString(&s, ddl->fk.data);
		appendStringInfoString(&s, "SELECT pg_catalog.set_config('kingres_shard.enable_routing', 'on', true);\n");
	}
	appendStringInfoString(&s, ddl->post.data);
	return s.data;
}

/*
 * kingres.create_distributed_table(relation regclass, dist_column name,
 *									 colocate_with regclass, bucket_count int)
 *
 * See the file header.  Everything happens in the caller's transaction: the
 * local DDL is transactional, and the remote nodes' work is in the transaction
 * branches that the commit protocol of phase 2a prepares and commits.
 */
static Datum
kingres_create_distributed_table_body(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	char	   *column = pstrdup(NameStr(*PG_GETARG_NAME(1)));
	Oid			colocate_with = PG_ARGISNULL(2) ? InvalidOid : PG_GETARG_OID(2);
	int			requested = PG_ARGISNULL(3) ? 0 : PG_GETARG_INT32(3);
	MemoryContext callercxt = CurrentMemoryContext;
	char	   *relname;
	char	   *nspname;
	char	   *shadow;
	Oid			nspoid;
	Oid			coltype;
	Oid			collation;
	int			colocation_id;
	int			nbuckets;
	KsColocation *placement;
	int		   *groups;
	int			local_group;
	DdlSet		ddl;
	Oid			argtypes[4];
	Datum		values[4];
	char		nulls[4];
	bool		isnull;
	uint64		i;
	List	   *targets = NIL;
	ListCell   *lc;
	StringInfoData script;
	StringInfoData qrel;
	Oid			newrelid;
	int64		nrows;
	char	   *partby = NULL;
	List	   *leafnames = NIL;
	List	   *leafbounds = NIL;
	int64		nread = 0;
	int64		nacked = 0;

	require_current_sql_objects();
	kingres_shard_lock_catalog();
	kingres_shard_require_map_group();
	local_group = kingres_shard_catalog()->local_group_id;

	if (requested < 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("bucket_count must be positive")));

	/*
	 * Writers wait from here to the commit; readers do not, until the swap.  The
	 * lock is taken first so that the report and the count see what the copy will
	 * read.
	 */
	require_read_committed();
	LockRelationOid(relid, ExclusiveLock);
	relname = get_rel_name(relid);
	if (relname == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("relation with OID %u does not exist", relid)));
	relname = pstrdup(relname);
	nspoid = get_rel_namespace(relid);
	nspname = pstrdup(get_namespace_name(nspoid));
	shadow = psprintf("kingres_shadow_%u", relid);

	/* Allocated in the caller's context: it outlives the SPI connection. */
	initStringInfo(&script);

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	/* 0. Every node must run this version (review finding S4-05). */
	kingres_shard_run_spi("SELECT " KINGRES_SHARD_SCHEMA ".internal_require_version('" KINGRES_SHARD_EXT_VERSION "')",
			0, NULL, NULL);

	/* 1. Validate the table and the column: the blockers of the report. */
	memset(nulls, ' ', sizeof(nulls));
	argtypes[0] = OIDOID;
	argtypes[1] = TEXTOID;
	argtypes[2] = OIDOID;
	argtypes[3] = INT4OID;
	values[0] = ObjectIdGetDatum(relid);
	values[1] = CStringGetTextDatum(column);
	values[2] = ObjectIdGetDatum(colocate_with);
	if (!OidIsValid(colocate_with))
		nulls[2] = 'n';
	values[3] = Int32GetDatum(requested);
	if (requested == 0)
		nulls[3] = 'n';
	if (SPI_execute_with_args("SELECT column_type, column_collation FROM " KINGRES_SHARD_SCHEMA
							  ".internal_check_distributable($1::pg_catalog.oid::pg_catalog.regclass, "
							  "$2::pg_catalog.name, $3::pg_catalog.oid::pg_catalog.regclass, $4)",
							  4, argtypes, values, nulls, false, 0) < 0)
		elog(ERROR, "could not check table \"%s\"", relname);
	coltype = DatumGetObjectId(SPI_getbinval(SPI_tuptable->vals[0],
											 SPI_tuptable->tupdesc, 1, &isnull));
	collation = DatumGetObjectId(SPI_getbinval(SPI_tuptable->vals[0],
											   SPI_tuptable->tupdesc, 2, &isnull));

	/* 2. The colocation group and the placement of its buckets. */
	colocation_id = kingres_shard_choose_colocation(coltype, collation, colocate_with,
												  requested, &nbuckets, NULL);
	/* A table added to a group whose bucket is being moved, or that an operation uses, would be missed. */
	kingres_shard_assert_no_move(colocation_id);

	groups = MemoryContextAlloc(callercxt, sizeof(int) * nbuckets);
	for (i = 0; i < (uint64) nbuckets; i++)
		groups[i] = -1;
	argtypes[0] = INT4OID;
	values[0] = Int32GetDatum(colocation_id);
	kingres_shard_run_spi("SELECT bucket, group_id FROM " KINGRES_SHARD_SCHEMA ".bucket_map "
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
	/* the layout of the group as of this transaction (the cache of the catalog may not have it yet) */
	{
		MemoryContext old = MemoryContextSwitchTo(callercxt);

		placement = kingres_shard_coloc_make(colocation_id, nbuckets, coltype, collation, groups);
		MemoryContextSwitchTo(old);
	}

	/*
	 * 2b. A table the user partitioned: its partitions are recreated as leaves inside every bucket partition
	 * (design, feature 4).  The layout is read now, while the source is whole; the number of leaves is bounded.
	 */
	argtypes[0] = OIDOID;
	values[0] = ObjectIdGetDatum(relid);
	kingres_shard_run_spi("SELECT partby, name::pg_catalog.text, bound FROM " KINGRES_SHARD_SCHEMA
						  ".internal_partition_layout($1::pg_catalog.oid::pg_catalog.regclass) ORDER BY ord",
						  1, argtypes, values);
	{
		MemoryContext old = MemoryContextSwitchTo(callercxt);

		for (i = 0; i < SPI_processed; i++)
		{
			if (partby == NULL)
				partby = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1);
			leafnames = lappend(leafnames, SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2));
			leafbounds = lappend(leafbounds, SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 3));
		}
		if (partby == NULL && get_rel_relkind(relid) == RELKIND_PARTITIONED_TABLE)
		{
			/* partitioned and without a partition: the layout still has the key */
			SPI_execute_with_args("SELECT pg_catalog.pg_get_partkeydef($1::pg_catalog.oid)", 1, argtypes, values, NULL, false, 0);
			partby = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);
		}
		MemoryContextSwitchTo(old);
	}
	if (partby != NULL && (uint64) nbuckets * (uint64) list_length(leafnames) > (uint64) kingres_shard_max_partition_leaves)
		ereport(ERROR,
				(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
				 errmsg("table \"%s\" would have %d leaves (%d buckets, %d partitions); kingres_shard.max_partition_leaves is %d",
						relname, nbuckets * list_length(leafnames), nbuckets, list_length(leafnames),
						kingres_shard_max_partition_leaves)));

	/* 3. The statements that recreate the table (before it is replaced). */
	kingres_shard_fetch_ddl(relid, column, true, &ddl, callercxt);

	/* The rows, counted under the lock: the copy must give exactly this many. */
	initStringInfo(&qrel);
	appendStringInfo(&qrel, "SELECT pg_catalog.count(*) FROM %s", quote_qualified_identifier(nspname, relname));
	kingres_shard_run_spi(qrel.data, 0, NULL, NULL);
	nrows = DatumGetInt64(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull));
	kingres_shard_fault_point("distribute_after_lock");

	/* 4a. The shadow here, with the partitions of this group. */
	if (ddl.local_pre.len > 0)
		kingres_shard_run_spi(ddl.local_pre.data, 0, NULL, NULL);
	resetStringInfo(&script);
	appendStringInfoString(&script, ddl.both.data);
	if (partby != NULL)
		kingres_shard_append_partition_tree(&script, nspname, relname, shadow, placement, local_group, partby, leafnames, leafbounds);
	else
		kingres_shard_append_partitions(&script, nspname, relname, shadow, placement, local_group);
	kingres_shard_run_spi(script.data, 0, NULL, NULL);

	/*
	 * 4b. The other nodes.  The node list is read after the catalog changes
	 * above so that it is current; each node gets the shadow and the
	 * partitions of its own group.
	 */
	targets = kingres_shard_remote_primaries(callercxt);
	foreach(lc, targets)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		KsConn	   *kc = kingres_shard_get_conn(n->host, n->port, false, NULL);

		kingres_shard_begin_write_xact(kc);
		resetStringInfo(&script);
		/* the values of a money column cross under lc_monetary = C (copy_routed.c) */
		appendStringInfoString(&script, "SET LOCAL lc_monetary = 'C';\n");
		appendStringInfoString(&script, ddl.remote_pre.data);
		appendStringInfoString(&script, ddl.both.data);
		if (partby != NULL)
			kingres_shard_append_partition_tree(&script, nspname, relname, shadow, placement, n->group_id, partby, leafnames, leafbounds);
		else
			kingres_shard_append_partitions(&script, nspname, relname, shadow, placement, n->group_id);
		kingres_shard_exec_on_node(kc, script.data);
	}
	kingres_shard_fault_point("distribute_after_shadow");

	/*
	 * 5. The rows: every one to the group that owns its bucket.  The destination is
	 * described, not looked up: the shadow is not in the catalog of any node.
	 */
	if (nrows > 0)
	{
		nacked = kingres_shard_copy_routed(relid, nspname, shadow, KS_DEST_PLACEMENT, placement,
										   column, 0, &nread);
		if (nread != nrows || nacked != nrows)
			ereport(ERROR,
					(errcode(ERRCODE_DATA_EXCEPTION),
					 errmsg("the copy of table \"%s\" is not complete: the table holds " INT64_FORMAT
							" rows, " INT64_FORMAT " were read and " INT64_FORMAT " were stored",
							relname, nrows, nread, nacked)));
	}
	kingres_shard_fault_point("distribute_before_swap");

	/*
	 * 6. The swap.  The old table is dropped (readers wait from here to the
	 * commit) and the shadow takes its name, here and on every other node.
	 */
	LockRelationOid(relid, AccessExclusiveLock);
	resetStringInfo(&script);
	appendStringInfo(&script, "DROP TABLE %s;\n", quote_qualified_identifier(nspname, relname));
	appendStringInfoString(&script, ddl.swap.data);
	kingres_shard_run_spi(script.data, 0, NULL, NULL);

	newrelid = get_relname_relid(relname, nspoid);
	if (!OidIsValid(newrelid))
		elog(ERROR, "could not find the new table \"%s\"", relname);

	/*
	 * 7. Foreign keys, sequence values, privileges and comments here, while the table is
	 * not in the catalog yet: the queries of the helpers read the partitions of this node,
	 * which is what they are for.  The keys were checked by the report; the hook of this
	 * extension leaves the statements alone (as it does for the attach of a bucket) and the
	 * validation of a key runs on the buckets of this node.
	 */
	if (ddl.fk.len > 0)
	{
		kingres_shard_internal_ddl_enter();
		PG_TRY();
		{
			kingres_shard_run_spi(ddl.fk.data, 0, NULL, NULL);
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

	/* 8. Record the table. */
	argtypes[0] = OIDOID;
	argtypes[1] = TEXTOID;
	argtypes[2] = INT4OID;
	values[0] = ObjectIdGetDatum(newrelid);
	values[1] = CStringGetTextDatum(column);
	values[2] = Int32GetDatum(colocation_id);
	kingres_shard_run_spi("INSERT INTO " KINGRES_SHARD_SCHEMA ".dist_table (relid, method, dist_column, colocation_id) "
			"VALUES ($1::pg_catalog.oid::pg_catalog.regclass, 'hash', "
			"$2::pg_catalog.name, $3)",
			3, argtypes, values);
	if (partby != NULL)
	{
		StringInfoData names;
		StringInfoData bounds;
		ListCell   *ln;
		ListCell   *lb;
		bool		first = true;

		initStringInfo(&names);
		initStringInfo(&bounds);
		forboth(ln, leafnames, lb, leafbounds)
		{
			appendStringInfo(&names, "%s%s", first ? "" : ",", quote_literal_cstr((char *) lfirst(ln)));
			appendStringInfo(&bounds, "%s%s", first ? "" : ",", quote_literal_cstr((char *) lfirst(lb)));
			first = false;
		}
		resetStringInfo(&qrel);
		appendStringInfo(&qrel, "SELECT " KINGRES_SHARD_SCHEMA ".internal_set_partitioning($1::pg_catalog.oid::pg_catalog.regclass, %s, "
						 "ARRAY[%s]::pg_catalog.text[], ARRAY[%s]::pg_catalog.text[])",
						 quote_literal_cstr(partby), names.data, bounds.data);
		argtypes[0] = OIDOID;
		values[0] = ObjectIdGetDatum(newrelid);
		kingres_shard_run_spi(qrel.data, 1, argtypes, values);
	}
	SPI_finish();
	kingres_shard_fault_point("distribute_after_swap");


	/* 9. The same on the other nodes, in the branches that are prepared. */
	foreach(lc, targets)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		KsConn	   *kc = kingres_shard_get_conn(n->host, n->port, false, NULL);

		kingres_shard_begin_write_xact(kc);
		kingres_shard_exec_on_node(kc, kingres_shard_remote_tail_script(&ddl));
	}

	/* 10. Make every node know the table. */
	kingres_shard_sync_catalog_internal();

	PG_RETURN_VOID();
}

Datum
kingres_create_distributed_table(PG_FUNCTION_ARGS)
{
	return kingres_shard_control_call(kingres_create_distributed_table_body, fcinfo);
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
	kingres_shard_run_spi("SELECT pg_catalog.quote_ident(attname::pg_catalog.text) FROM pg_catalog.pg_attribute "
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
	/* a fresh snapshot: the caller's was taken before the wait for the lock (copy_routed.c) */
	PushActiveSnapshot(GetTransactionSnapshot());
	portal = SPI_cursor_open(NULL, plan, NULL, NULL, true);

	rows_per_batch = Max(1, 20000 / ncols);
	batchcxt = AllocSetContextCreate(CurrentMemoryContext, "kingres_shard row copy batch",
									 ALLOCSET_DEFAULT_SIZES);
	nest = kingres_shard_set_transmission_modes();
	/* the text of a money value must not depend on lc_monetary (the other ends run under "C") */
	(void) set_config_option("lc_monetary", "C", PGC_USERSET, PGC_S_SESSION,
							 GUC_ACTION_SAVE, true, 0, false);
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
		appendStringInfo(&sql, "%sINSERT INTO %s.%s (%s) OVERRIDING SYSTEM VALUE VALUES ",
						 KS_REFWRITE_MARKER, quote_identifier(nsp), quote_identifier(rel), collist.data);
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
	PopActiveSnapshot();
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
 * The copies carry the owner, privileges and comments of the table, and its
 * foreign keys to other reference tables.
 */
static Datum
kingres_create_reference_table_body(PG_FUNCTION_ARGS)
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

	require_current_sql_objects();
	kingres_shard_lock_catalog();
	kingres_shard_require_map_group();
	require_read_committed();
	LockRelationOid(relid, ExclusiveLock);

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	kingres_shard_run_spi("SELECT " KINGRES_SHARD_SCHEMA ".internal_require_version('" KINGRES_SHARD_EXT_VERSION "')",
			0, NULL, NULL);
	values[0] = ObjectIdGetDatum(relid);
	kingres_shard_run_spi("SELECT " KINGRES_SHARD_SCHEMA ".internal_check_referenceable("
			"$1::pg_catalog.oid::pg_catalog.regclass)", 1, argtypes, values);
	kingres_shard_fetch_ddl(relid, NULL, false, &ddl, callercxt);
	kingres_shard_run_spi("INSERT INTO " KINGRES_SHARD_SCHEMA ".dist_table (relid, method) "
			"VALUES ($1::pg_catalog.oid::pg_catalog.regclass, 'reference')",
			1, argtypes, values);
	/* Plans of statements on the table (prepared, in functions) were made for an ordinary table (S3-02). */
	CacheInvalidateRelcacheByRelid(relid);
	SPI_finish();

	initStringInfo(&script);
	/* the values of a money column cross under lc_monetary = C (copy_routed.c) */
	appendStringInfoString(&script, "SET LOCAL lc_monetary = 'C';\n");
	appendStringInfoString(&script, ddl.remote_pre.data);
	appendStringInfoString(&script, ddl.both.data);

	targets = kingres_shard_remote_primaries(callercxt);
	foreach(lc, targets)
	{
		KsNode	   *n = (KsNode *) lfirst(lc);
		KsConn	   *kc = kingres_shard_get_conn(n->host, n->port, false, NULL);

		kingres_shard_begin_write_xact(kc);
		kingres_shard_exec_on_node(kc, script.data);
		conns = lappend(conns, kc);
	}
	kingres_shard_copy_table_rows(relid, conns);

	/*
	 * The keys to other reference tables, then sequence values, privileges and
	 * comments, on the other nodes (this node's table is the original, and has
	 * them).  The keys were checked by the report.
	 */
	resetStringInfo(&script);
	if (ddl.fk.len > 0)
	{
		appendStringInfoString(&script, "SELECT pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);\n");
		appendStringInfoString(&script, ddl.fk.data);
		appendStringInfoString(&script, "SELECT pg_catalog.set_config('kingres_shard.enable_routing', 'on', true);\n");
	}
	appendStringInfoString(&script, ddl.post.data);
	foreach(lc, conns)
		kingres_shard_exec_on_node((KsConn *) lfirst(lc), script.data);

	kingres_shard_sync_catalog_internal();
	PG_RETURN_VOID();
}

Datum
kingres_create_reference_table(PG_FUNCTION_ARGS)
{
	return kingres_shard_control_call(kingres_create_reference_table_body, fcinfo);
}
