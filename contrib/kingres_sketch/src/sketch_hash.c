/*-------------------------------------------------------------------------
 *
 * sketch_hash.c
 *	  The 64-bit hash of an element of any type that a sketch accepts.
 *
 * A sketch must be a function of the values it was built from, and nothing
 * else: the same values must give the same sketch in another database, on
 * another node, and after a dump and restore.  PostgreSQL's own hash
 * functions do not all have this property: the hash of an enum value is the
 * hash of the OID of its label, and the hash of "oid" and of the "reg*"
 * types is the hash of the number, and an OID is not the same after a
 * restore or on another node.  So the hash is chosen by the kind of type:
 *
 *	 scalar types   the extended hash function of the default hash operator
 *					class of the type (hash_extended_proc), seed 0;
 *	 enum           the hash of the bytes of the label (not of its OID);
 *	 array, range, multirange, record and composite types
 *					a combination of the hashes of the elements (and of the
 *					dimensions of an array, the bound flags of a range),
 *					by the rules here, so that an enum inside a container is
 *					hashed by its label too;
 *	 domain         the hash of the base type;
 *	 oid, reg*, oidvector, or any container of them
 *					refused with SQLSTATE 0A000: their hash depends on an
 *					object identifier.
 *
 * A type for which this cannot be done is refused when the aggregate or
 * function is first called with that type, even if the value is NULL, so
 * that a query does not work or fail depending on the data.  Equal values
 * (under the equality operator of the type) have equal hashes.
 *
 * The hash of a scalar value is finished with a fixed 64-bit mixing
 * function (see ks_mix64).
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_sketch/src/sketch_hash.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "catalog/pg_collation.h"
#include "catalog/pg_type.h"
#include "common/hashfn.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "utils/array.h"
#include "utils/builtins.h"
#include "utils/fmgroids.h"
#include "utils/lsyscache.h"
#include "utils/multirangetypes.h"
#include "utils/rangetypes.h"
#include "utils/typcache.h"

#include "kingres_sketch.h"

PG_FUNCTION_INFO_V1(kingres_sketch_hll_hash);

/*
 * The finishing function of the element hash: the 64-bit finalizer of
 * MurmurHash3 after adding a constant (a bijection).  It spreads the output
 * of the type's own hash so that both the high bits (the register of an hll)
 * and the low bits (its rank) are well mixed; the constant moves the fixed
 * point of the finalizer away from 0.
 */
uint64
ks_mix64(uint64 h)
{
	h += UINT64CONST(0x9e3779b97f4a7c15);
	h ^= h >> 33;
	h *= UINT64CONST(0xff51afd7ed558ccd);
	h ^= h >> 33;
	h *= UINT64CONST(0xc4ceb9fe1a85ec53);
	h ^= h >> 33;
	return h;
}

typedef enum KsHKind
{
	KS_H_SCALAR,
	KS_H_ENUM,
	KS_H_ARRAY,
	KS_H_RANGE,
	KS_H_MULTIRANGE,
	KS_H_RECORD
} KsHKind;

/* How to hash the values of one type. */
typedef struct KsHashPlan
{
	Oid			typid;			/* the base type, after looking through domains */
	KsHKind		kind;
	FmgrInfo	finfo;			/* scalar: the hash function */
	Oid			outfunc;		/* enum: the output function (the label) */
	Oid			elemtype;		/* array element, range subtype */
	int16		elmlen;
	bool		elmbyval;
	char		elmalign;
	TypeCacheEntry *tce;		/* range: its own; multirange: the range's */
} KsHashPlan;

/* The plans of one call site, and the type of its element argument. */
typedef struct KsHashCtx
{
	MemoryContext cxt;
	Oid			argtype;
	Oid			collation;
	KsHashPlan **plans;
	int			nplans;
	int			maxplans;
} KsHashCtx;

static KsHashPlan *ks_plan_for(KsHashCtx *ctx, Oid typid, Oid toptype);
static uint64 ks_hash_value(KsHashCtx *ctx, Oid typid, Datum value, Oid collation);

static void
ks_refuse_oid_type(Oid toptype, Oid typid)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("a sketch does not take type %s", format_type_be(toptype)),
			 errdetail("The hash of type %s depends on an object identifier, which is not the same in another database or after a restore.",
					   format_type_be(typid)),
			 errhint("Cast the value to text.")));
}

static KsHashPlan *
ks_find_plan(KsHashCtx *ctx, Oid typid)
{
	int			i;

	for (i = 0; i < ctx->nplans; i++)
		if (ctx->plans[i]->typid == typid)
			return ctx->plans[i];
	return NULL;
}

/*
 * Build the plan for a type, and the plans of the types inside it, so that a
 * type that cannot be hashed is found now and not at the first value.
 * toptype is the type of the call, named in the error.
 */
static KsHashPlan *
ks_plan_for(KsHashCtx *ctx, Oid typid, Oid toptype)
{
	Oid			base = getBaseType(typid);
	KsHashPlan *plan;
	Oid			elem;
	char		typtype = TYPTYPE_BASE;

	plan = ks_find_plan(ctx, base);
	if (plan)
		return plan;
	check_stack_depth();

	plan = (KsHashPlan *) MemoryContextAllocZero(ctx->cxt, sizeof(KsHashPlan));
	plan->typid = base;

	if (base == RECORDOID)
		plan->kind = KS_H_RECORD;	/* the fields are planned per value */
	else if ((typtype = get_typtype(base)) == TYPTYPE_ENUM)
	{
		bool		isvarlena;

		plan->kind = KS_H_ENUM;
		getTypeOutputInfo(base, &plan->outfunc, &isvarlena);
	}
	else if (typtype == TYPTYPE_RANGE)
	{
		plan->kind = KS_H_RANGE;
		plan->tce = lookup_type_cache(base, TYPECACHE_RANGE_INFO);
		plan->elemtype = plan->tce->rngelemtype->type_id;
		ks_plan_for(ctx, plan->elemtype, toptype);
	}
	else if (typtype == TYPTYPE_MULTIRANGE)
	{
		TypeCacheEntry *mtce = lookup_type_cache(base, TYPECACHE_MULTIRANGE_INFO);

		plan->kind = KS_H_MULTIRANGE;
		plan->tce = mtce->rngtype;
		plan->elemtype = plan->tce->rngelemtype->type_id;
		ks_plan_for(ctx, plan->elemtype, toptype);
	}
	else if (typtype == TYPTYPE_COMPOSITE)
	{
		TupleDesc	tupdesc = lookup_rowtype_tupdesc(base, -1);
		int			i;

		plan->kind = KS_H_RECORD;
		for (i = 0; i < tupdesc->natts; i++)
		{
			Form_pg_attribute att = TupleDescAttr(tupdesc, i);

			if (!att->attisdropped)
				ks_plan_for(ctx, att->atttypid, toptype);
		}
		ReleaseTupleDesc(tupdesc);
	}
	else if (OidIsValid(elem = get_element_type(base)))
	{
		plan->kind = KS_H_ARRAY;
		plan->elemtype = elem;
		get_typlenbyvalalign(elem, &plan->elmlen, &plan->elmbyval, &plan->elmalign);
		ks_plan_for(ctx, elem, toptype);
	}
	else
	{
		TypeCacheEntry *tce = lookup_type_cache(base, TYPECACHE_HASH_EXTENDED_PROC);

		plan->kind = KS_H_SCALAR;
		if (!OidIsValid(tce->hash_extended_proc))
			ereport(ERROR,
					(errcode(ERRCODE_UNDEFINED_FUNCTION),
					 errmsg("type %s has no hash function", format_type_be(base))));
		if (tce->hash_extended_proc == F_HASHOIDEXTENDED ||
			tce->hash_extended_proc == F_HASHOIDVECTOREXTENDED)
			ks_refuse_oid_type(toptype, base);
		fmgr_info_cxt(tce->hash_extended_proc, &plan->finfo, ctx->cxt);
	}

	if (ctx->nplans == ctx->maxplans)
	{
		int			newmax = ctx->maxplans == 0 ? 8 : ctx->maxplans * 2;

		if (ctx->plans)
			ctx->plans = (KsHashPlan **) repalloc(ctx->plans, newmax * sizeof(KsHashPlan *));
		else
			ctx->plans = (KsHashPlan **) MemoryContextAlloc(ctx->cxt, newmax * sizeof(KsHashPlan *));
		ctx->maxplans = newmax;
	}
	ctx->plans[ctx->nplans++] = plan;
	return plan;
}

/* The combination of a running hash and the hash of the next part. */
static inline uint64
ks_comb(uint64 h, uint64 x)
{
	return ks_mix64(h ^ (x * UINT64CONST(0x9e3779b97f4a7c15) + UINT64CONST(0x7f4a7c15)));
}

#define KS_H_NULL_ELEMENT	UINT64CONST(0x6e756c6c656c656d)

static uint64
ks_hash_range_value(KsHashCtx *ctx, KsHashPlan *plan, RangeType *r, Oid collation)
{
	RangeBound	lower;
	RangeBound	upper;
	bool		empty;
	uint64		h = UINT64CONST(0x72616e6765);

	range_deserialize(plan->tce, r, &lower, &upper, &empty);
	if (empty)
		return ks_comb(h, 1);
	h = ks_comb(h, 2);
	h = ks_comb(h, lower.infinite ? 1 : (lower.inclusive ? 2 : 3));
	if (!lower.infinite)
		h = ks_comb(h, ks_hash_value(ctx, plan->elemtype, lower.val, collation));
	h = ks_comb(h, upper.infinite ? 1 : (upper.inclusive ? 2 : 3));
	if (!upper.infinite)
		h = ks_comb(h, ks_hash_value(ctx, plan->elemtype, upper.val, collation));
	return h;
}

/* The hash of one value of a type, by the plan of its kind. */
static uint64
ks_hash_value(KsHashCtx *ctx, Oid typid, Datum value, Oid collation)
{
	KsHashPlan *plan = ks_plan_for(ctx, typid, typid);
	uint64		h;

	switch (plan->kind)
	{
		case KS_H_SCALAR:
			return (uint64) DatumGetInt64(FunctionCall2Coll(&plan->finfo, collation,
															value, Int64GetDatum(0)));

		case KS_H_ENUM:
			{
				char	   *label = OidOutputFunctionCall(plan->outfunc, value);

				h = hash_bytes_extended((const unsigned char *) label, (int) strlen(label), 0);
				pfree(label);
				return h;
			}

		case KS_H_ARRAY:
			{
				ArrayType  *arr = DatumGetArrayTypeP(value);
				Datum	   *elems;
				bool	   *nulls;
				int			n;
				int			i;

				h = UINT64CONST(0x6172726179);
				h = ks_comb(h, (uint64) ARR_NDIM(arr));
				for (i = 0; i < ARR_NDIM(arr); i++)
				{
					h = ks_comb(h, (uint64) ARR_DIMS(arr)[i]);
					h = ks_comb(h, (uint64) (int64) ARR_LBOUND(arr)[i]);
				}
				deconstruct_array(arr, plan->elemtype, plan->elmlen, plan->elmbyval,
								  plan->elmalign, &elems, &nulls, &n);
				for (i = 0; i < n; i++)
					h = ks_comb(h, nulls[i] ? KS_H_NULL_ELEMENT :
								ks_hash_value(ctx, plan->elemtype, elems[i], collation));
				return h;
			}

		case KS_H_RANGE:
			return ks_hash_range_value(ctx, plan, DatumGetRangeTypeP(value), collation);

		case KS_H_MULTIRANGE:
			{
				MultirangeType *mr = DatumGetMultirangeTypeP(value);
				RangeType **ranges;
				int32		count;
				int32		i;

				multirange_deserialize(plan->tce, mr, &count, &ranges);
				h = UINT64CONST(0x6d756c7469);
				h = ks_comb(h, (uint64) count);
				for (i = 0; i < count; i++)
					h = ks_comb(h, ks_hash_range_value(ctx, plan, ranges[i], collation));
				return h;
			}

		case KS_H_RECORD:
			{
				HeapTupleHeader td = DatumGetHeapTupleHeader(value);
				TupleDesc	tupdesc;
				HeapTupleData tmp;
				int			i;

				tupdesc = lookup_rowtype_tupdesc(HeapTupleHeaderGetTypeId(td),
												 HeapTupleHeaderGetTypMod(td));
				tmp.t_len = HeapTupleHeaderGetDatumLength(td);
				ItemPointerSetInvalid(&tmp.t_self);
				tmp.t_tableOid = InvalidOid;
				tmp.t_data = td;
				h = UINT64CONST(0x7265636f7264);
				for (i = 0; i < tupdesc->natts; i++)
				{
					Form_pg_attribute att = TupleDescAttr(tupdesc, i);
					bool		isnull;
					Datum		d;

					if (att->attisdropped)
						continue;
					d = heap_getattr(&tmp, i + 1, tupdesc, &isnull);
					h = ks_comb(h, isnull ? KS_H_NULL_ELEMENT :
								ks_hash_value(ctx, att->atttypid, d,
											  OidIsValid(att->attcollation) ? att->attcollation : collation));
				}
				ReleaseTupleDesc(tupdesc);
				return h;
			}
	}
	return 0;					/* not reached */
}

/* The context of a call site, built (and the type checked) on first use. */
static KsHashCtx *
ks_hash_ctx(FunctionCallInfo fcinfo, int argno)
{
	KsHashCtx  *ctx = (KsHashCtx *) fcinfo->flinfo->fn_extra;
	Oid			typid = get_fn_expr_argtype(fcinfo->flinfo, argno);

	if (!OidIsValid(typid))
		ereport(ERROR,
				(errcode(ERRCODE_INDETERMINATE_DATATYPE),
				 errmsg("could not determine the data type of the element")));
	if (ctx == NULL || ctx->argtype != typid)
	{
		MemoryContext cxt = fcinfo->flinfo->fn_mcxt;

		ctx = (KsHashCtx *) MemoryContextAllocZero(cxt, sizeof(KsHashCtx));
		ctx->cxt = cxt;
		ctx->argtype = typid;
		ctx->collation = OidIsValid(PG_GET_COLLATION()) ? PG_GET_COLLATION() : DEFAULT_COLLATION_OID;
		fcinfo->flinfo->fn_extra = ctx;
		ks_plan_for(ctx, typid, typid);
	}
	return ctx;
}

/* Refuse the type of the element argument now, whatever the value. */
void
ks_hash_check_type(FunctionCallInfo fcinfo, int argno)
{
	(void) ks_hash_ctx(fcinfo, argno);
}

/*
 * The 64-bit hash of an element of the type of argument "argno" of the call:
 * the hash of its plan, finished with ks_mix64.
 */
uint64
ks_hash_datum(FunctionCallInfo fcinfo, int argno, Datum value)
{
	KsHashCtx  *ctx = ks_hash_ctx(fcinfo, argno);

	return ks_mix64(ks_hash_value(ctx, ctx->argtype, value, ctx->collation));
}

/* The same for a value of a given type, outside a call (self_test). */
uint64
ks_hash_value_of_type(Oid typid, Datum value, Oid collation)
{
	KsHashCtx	ctx;
	uint64		h;

	memset(&ctx, 0, sizeof(ctx));
	ctx.cxt = CurrentMemoryContext;
	ctx.argtype = typid;
	ctx.collation = collation;
	h = ks_mix64(ks_hash_value(&ctx, typid, value, collation));
	return h;
}

/* hll_hash(anyelement) RETURNS bigint */
Datum
kingres_sketch_hll_hash(PG_FUNCTION_ARGS)
{
	PG_RETURN_INT64((int64) ks_hash_datum(fcinfo, 0, PG_GETARG_DATUM(0)));
}
