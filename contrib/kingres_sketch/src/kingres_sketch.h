/*-------------------------------------------------------------------------
 *
 * kingres_sketch.h
 *	  Declarations shared by the files of the kingres_sketch extension.
 *
 * The extension provides three approximate-aggregate sketches, written from
 * the published algorithms:
 *
 *	 hll      HyperLogLog: P. Flajolet, E. Fusy, O. Gandouet, F. Meunier,
 *			  "HyperLogLog: the analysis of a near-optimal cardinality
 *			  estimation algorithm", AofA 2007; S. Heule, M. Nunkesser,
 *			  A. Hall, "HyperLogLog in practice", EDBT 2013 (the sparse
 *			  representation); O. Ertl, "New cardinality estimation
 *			  algorithms for HyperLogLog sketches", arXiv:1702.01284, 2017
 *			  (the estimator and the folding of registers).
 *	 topn     Space-Saving: A. Metwally, D. Agrawal, A. El Abbadi,
 *			  "Efficient computation of frequent and top-k elements in data
 *			  streams", ICDT 2005; P. Agarwal, G. Cormode, Z. Huang,
 *			  J. Phillips, Z. Wei, K. Yi, "Mergeable summaries", PODS 2012.
 *	 tdigest  merging t-digest: T. Dunning, O. Ertl, "Computing extremely
 *			  accurate quantiles using t-digests", arXiv:1902.04023, 2019.
 *
 * This header holds the format of the byte strings (the header common to
 * the three, and the little-endian accessors), the limits, and the entry
 * points that are shared between files.  Format version 1 is specified in
 * the README; every value is a function of the content of the sketch
 * (canonical bytes), and no value depends on the byte order of the machine.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_sketch/src/kingres_sketch.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SKETCH_KINGRES_SKETCH_H
#define KINGRES_SKETCH_KINGRES_SKETCH_H

#include "fmgr.h"
#include "utils/memutils.h"
#include "varatt.h"

#include "pg_compat.h"

/* Extension version, reported by kingres_sketch.self_test(). */
#define KS_EXT_VERSION "1.0"

/* The header of every sketch: magic, format version, kind, one parameter. */
#define KS_MAGIC			0x4B
#define KS_FORMAT_VERSION	1
#define KS_HEADER_LEN		4

#define KS_KIND_HLL			1
#define KS_KIND_TOPN		2
#define KS_KIND_TDIGEST		3
/* The state of approx_percentile: a t-digest followed by the fraction. */
#define KS_KIND_TDIGEST_PCT 4

/* Limits of the parameters (SQL: 22023 outside). */
#define KS_HLL_MIN_PRECISION	4
#define KS_HLL_MAX_PRECISION	18
#define KS_HLL_DEFAULT_PRECISION 14

#define KS_TOPN_MIN_CAPACITY	1
#define KS_TOPN_MAX_CAPACITY	100000
#define KS_TOPN_DEFAULT_CAPACITY 1000
#define KS_TOPN_MAX_ITEM_BYTES	1024

#define KS_TD_MIN_COMPRESSION	10
#define KS_TD_MAX_COMPRESSION	10000
#define KS_TD_DEFAULT_COMPRESSION 100

/* Message buffer for the validators: why a value is not valid. */
#define KS_WHY_LEN 160

/* ---- little-endian accessors; the format does not depend on the machine ---- */

static inline void
ks_put_u16(uint8 *p, uint16 v)
{
	p[0] = (uint8) v;
	p[1] = (uint8) (v >> 8);
}

static inline void
ks_put_u32(uint8 *p, uint32 v)
{
	int			i;

	for (i = 0; i < 4; i++)
		p[i] = (uint8) (v >> (8 * i));
}

static inline void
ks_put_u64(uint8 *p, uint64 v)
{
	int			i;

	for (i = 0; i < 8; i++)
		p[i] = (uint8) (v >> (8 * i));
}

static inline uint16
ks_get_u16(const uint8 *p)
{
	return (uint16) p[0] | ((uint16) p[1] << 8);
}

static inline uint32
ks_get_u32(const uint8 *p)
{
	uint32		v = 0;
	int			i;

	for (i = 3; i >= 0; i--)
		v = (v << 8) | p[i];
	return v;
}

static inline uint64
ks_get_u64(const uint8 *p)
{
	uint64		v = 0;
	int			i;

	for (i = 7; i >= 0; i--)
		v = (v << 8) | p[i];
	return v;
}

/* IEEE 754 binary64, stored as its bit pattern in little-endian order. */
static inline void
ks_put_f64(uint8 *p, double v)
{
	uint64		bits;

	memcpy(&bits, &v, sizeof(bits));
	ks_put_u64(p, bits);
}

static inline double
ks_get_f64(const uint8 *p)
{
	uint64		bits = ks_get_u64(p);
	double		v;

	memcpy(&v, &bits, sizeof(v));
	return v;
}

/* ---- common code (kingres_sketch.c) ---- */

/* How big a value of this kind can be at most (the check before any decode). */
extern Size ks_max_value_bytes(int kind);

/* Check the common header; false and a reason in why[] when it is wrong. */
extern bool ks_check_header(const uint8 *p, Size len, int kind, char *why);

/* The bytes of a sketch argument (detoasted, possibly packed header). */
/* Refuse an argument that alone passes the limit, before it is detoasted and unpacked. */
extern void ks_arg_precheck(FunctionCallInfo fcinfo, int argno);
extern const uint8 *ks_arg_bytes(FunctionCallInfo fcinfo, int argno, Size *len);

/* A new varlena holding a copy of the bytes. */
extern bytea *ks_make_bytea(const uint8 *buf, Size len);

/* Report an invalid value found in a stored or passed sketch: hard 22023. */
extern void ks_invalid_value(const char *typname, const char *why);

/* Text input and output of sketches: '\x' and hex digits. */
extern Datum ks_in_common(FunctionCallInfo fcinfo, int kind, const char *typname,
						  bool (*validate) (const uint8 *, Size, char *));
extern Datum ks_out_common(FunctionCallInfo fcinfo);
extern Datum ks_recv_common(FunctionCallInfo fcinfo, int kind, const char *typname,
							bool (*validate) (const uint8 *, Size, char *));
extern Datum ks_send_common(FunctionCallInfo fcinfo);
extern Datum ks_to_bytea_common(FunctionCallInfo fcinfo);
extern Datum ks_from_bytea_common(FunctionCallInfo fcinfo, int kind,
								  const char *typname,
								  bool (*validate) (const uint8 *, Size, char *));

/*
 * The 64-bit hash of an element: the extended hash function of the default
 * hash operator class of the argument's type, seed 0, finished with a fixed
 * mixing function.  The type is taken from the call expression.
 */
extern uint64 ks_hash_datum(FunctionCallInfo fcinfo, int argno, Datum value);
extern uint64 ks_mix64(uint64 h);

/* Refuse the type of the element argument of the call now, whatever the value. */
extern void ks_hash_check_type(FunctionCallInfo fcinfo, int argno);

/* The hash of a value of a given type outside a call (self_test). */
extern uint64 ks_hash_value_of_type(Oid typid, Datum value, Oid collation);

/*
 * The parameter argument of an aggregate (precision, capacity, compression)
 * must not be NULL in any row: one rule, so that the answer does not depend
 * on the order of the rows, the plan or the number of workers.
 */
#define KS_REQUIRE_PARAM(fcinfo, argno, what) \
	do { \
		if (PG_ARGISNULL(argno)) \
			ereport(ERROR, \
					(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED), \
					 errmsg("the %s argument of the aggregate must not be NULL", what), \
					 errhint("Give the same non-NULL %s in every row.", what))); \
	} while (0)

/* Return a state pointer, or SQL NULL when there is no state (a NULL pointer is not a NULL datum). */
#define KS_RETURN_STATE(p) \
	do { \
		if ((p) == NULL) \
			PG_RETURN_NULL(); \
		PG_RETURN_POINTER(p); \
	} while (0)

/* The memory context of an aggregate's state; an error outside aggregates. */
extern MemoryContext ks_agg_context(FunctionCallInfo fcinfo);

/*
 * Memory of aggregate states.  All the states of the aggregates of one
 * aggregate context (of all three types) live in ONE memory context, a child
 * of that aggregate context, that carries an account: the bytes the context
 * has taken from the system.  One context per state cost several times the
 * memory of the states themselves when a query has a million small groups.
 * The sum of the accounts of a backend is one counter, bounded by the
 * effective limit (the smaller of the settings kingres_sketch.state_memory_
 * ceiling and kingres_sketch.state_memory_limit).  The context goes away with
 * the aggregate's context on every path (the end of a group of a sorted
 * aggregate, the end of the query, an error, the abort of a subtransaction),
 * and a reset callback of the context takes its account out of the counter,
 * so the counter cannot leak.
 */
typedef struct KsAcct KsAcct;

/* The account (and context) of aggcontext, made on first use. */
extern KsAcct *ks_acct_create(MemoryContext aggcontext);

/* The context in which the state of the account allocates everything. */
extern MemoryContext ks_acct_context(KsAcct *acct);

/* Bring the account up to what the context holds now; error 54000 over the limit. */
extern void ks_acct_update(KsAcct *acct);

/* Refuse now when "want" more bytes would pass the limit (before a large load). */
extern void ks_acct_precheck(Size want);

/* Validators of the three types, used by the type functions and by tests. */
extern bool hll_validate(const uint8 *p, Size len, char *why);
extern bool topn_validate(const uint8 *p, Size len, char *why);
extern bool tdigest_validate(const uint8 *p, Size len, char *why);
extern bool tdigest_validate_state(const uint8 *p, Size len, char *why);

/* The sketch state of hll.c, used by self_test() in kingres_sketch.c. */
extern int64 hll_self_test_value(void);
extern void tdigest_self_test_values(uint64 *single, uint64 *uni, uint64 *read, uint64 *scale);
extern void topn_self_test_values(uint64 *single, uint64 *uni);

/* A fixed hash of a byte string (FNV-1a), for self_test(): no dependency on a library. */
extern uint64 ks_hash_bytes64(const uint8 *p, Size len);

#endif							/* KINGRES_SKETCH_KINGRES_SKETCH_H */
