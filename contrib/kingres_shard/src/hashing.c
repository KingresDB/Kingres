/*-------------------------------------------------------------------------
 *
 * hashing.c
 *	  The bucket function and its SQL interface, kingres.bucket_for().
 *
 * The bucket of a key is compute_partition_hash_value(key) % bucket_count.
 * That is exactly the value PostgreSQL itself computes to choose the
 * partition of a row for PARTITION BY HASH (key) with MODULUS bucket_count,
 * so routing and storage cannot disagree (design/shard-mvp.md, "The bucket
 * function").  The hash support function is the extended hash function of
 * the default hash operator class of the distribution column type, the same
 * one a partition key uses.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/hashing.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/hash.h"
#include "fmgr.h"
#include "parser/parse_coerce.h"
#include "partitioning/partbounds.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"
#include "utils/typcache.h"

#include "catalog.h"
#include "hashing.h"
#include "kingres_shard.h"

PG_FUNCTION_INFO_V1(kingres_bucket_for_relation);
PG_FUNCTION_INFO_V1(kingres_bucket_for_colocation);

/*
 * kingres_shard_try_bucket
 *
 * Compute the bucket of one value.  column_type is the type of the
 * distribution column (which decides the hash operator family),
 * value_type the type of the datum.  They are equal or binary coercible in
 * the common case; otherwise the hash operator family of the column must
 * contain a hash function for the value type (for example int4 against an
 * int8 column), which then hashes equal values equally.  A NULL key hashes to
 * 0, as it does for partition routing.
 *
 * Returns false when no suitable hash function exists; the caller decides
 * whether that is an error.  Errors raised by the hash function itself
 * propagate.
 */
bool
kingres_shard_try_bucket(Oid column_type, Oid collation, int bucket_count,
						  Oid value_type, Datum value, bool isnull,
						  int *bucket)
{
	TypeCacheEntry *tce;
	FmgrInfo	finfo;
	FmgrInfo   *finfop;
	uint64		hash;

	tce = lookup_type_cache(column_type,
							TYPECACHE_HASH_EXTENDED_PROC_FINFO |
							TYPECACHE_HASH_OPFAMILY);
	if (!OidIsValid(tce->hash_extended_proc))
		return false;

	if (value_type == column_type || value_type == InvalidOid ||
		IsBinaryCoercible(value_type, column_type))
		finfop = &tce->hash_extended_proc_finfo;
	else
	{
		Oid			proc;

		proc = get_opfamily_proc(tce->hash_opf, value_type, value_type,
								 HASHEXTENDED_PROC);
		if (!OidIsValid(proc))
			return false;
		fmgr_info(proc, &finfo);
		finfop = &finfo;
	}

	hash = compute_partition_hash_value(1, finfop, &collation, &value,
										&isnull);
	*bucket = (int) (hash % (uint64) bucket_count);
	return true;
}

/*
 * kingres_shard_bucket
 *
 * Like kingres_shard_try_bucket(), but a value that cannot be hashed for the
 * column is an error.
 */
int
kingres_shard_bucket(Oid column_type, Oid collation, int bucket_count,
					  Oid value_type, Datum value, bool isnull)
{
	int			bucket;

	if (!kingres_shard_try_bucket(column_type, collation, bucket_count,
								   value_type, value, isnull, &bucket))
		ereport(ERROR,
				(errcode(ERRCODE_DATATYPE_MISMATCH),
				 errmsg("cannot compute a bucket for a value of type %s with a distribution column of type %s",
						format_type_be(value_type),
						format_type_be(column_type))));
	return bucket;
}

/*
 * bucket_for_colocation_entry
 *
 * Shared part of the two SQL functions: argument 1 is the key value, whose
 * type comes from the call expression because the SQL type is anyelement.
 */
static Datum
bucket_for_colocation_entry(FunctionCallInfo fcinfo,
							const KsColocation *coloc)
{
	Oid			value_type = get_fn_expr_argtype(fcinfo->flinfo, 1);
	bool		isnull = PG_ARGISNULL(1);
	Datum		value = isnull ? (Datum) 0 : PG_GETARG_DATUM(1);

	if (!OidIsValid(value_type))
		ereport(ERROR,
				(errcode(ERRCODE_INDETERMINATE_DATATYPE),
				 errmsg("could not determine the type of the key value")));

	PG_RETURN_INT32(kingres_shard_bucket(coloc->dist_type, coloc->collation,
										  coloc->bucket_count, value_type,
										  value, isnull));
}

/*
 * kingres.bucket_for(relation regclass, value anyelement) returns int
 *
 * The bucket of a key of a distributed table.  Not strict: a NULL key is
 * stored in bucket 0 by PostgreSQL, so that is what is returned.
 */
Datum
kingres_bucket_for_relation(PG_FUNCTION_ARGS)
{
	Oid			relid;
	const KsDistTable *dt;

	if (PG_ARGISNULL(0))
		PG_RETURN_NULL();
	relid = PG_GETARG_OID(0);

	dt = kingres_shard_lookup_dist_table(relid);
	if (dt == NULL)
	{
		char	   *relname = get_rel_name(relid);

		if (relname != NULL)
			ereport(ERROR,
					(errcode(ERRCODE_UNDEFINED_TABLE),
					 errmsg("table \"%s\" is not a distributed table",
							relname)));
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("relation with OID %u is not a distributed table",
						relid)));
	}
	return bucket_for_colocation_entry(fcinfo, dt->coloc);
}

/*
 * kingres.bucket_for(colocation_id int, value anyelement) returns int
 *
 * The bucket of a key in a colocation group.
 */
Datum
kingres_bucket_for_colocation(PG_FUNCTION_ARGS)
{
	const KsColocation *coloc;

	if (PG_ARGISNULL(0))
		PG_RETURN_NULL();

	coloc = kingres_shard_lookup_colocation(PG_GETARG_INT32(0));
	if (coloc == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_OBJECT),
				 errmsg("colocation group %d does not exist",
						PG_GETARG_INT32(0))));
	return bucket_for_colocation_entry(fcinfo, coloc);
}
