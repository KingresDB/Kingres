/*-------------------------------------------------------------------------
 *
 * hll.c
 *	  The type hll: a HyperLogLog sketch for approximate distinct counts.
 *
 * Algorithm.  A sketch of precision p has m = 2^p registers.  An element's
 * 64-bit hash h picks register j = h >> (64 - p), and the register keeps the
 * largest rank seen, where the rank is 1 plus the number of leading zero
 * bits of the remaining 64 - p bits (Flajolet et al. 2007).  The estimate is
 * Ertl's improved estimator (arXiv:1702.01284, algorithm 6), which needs no
 * bias tables and no switch between linear counting and the raw estimate.
 * The sparse form of Heule et al. 2013 keeps a short list of the non-zero
 * registers while the sketch is small.  Folding to a lower precision follows
 * Ertl's section on reducing the number of registers.
 *
 * Format version 1 (all integers little-endian):
 *	 byte 0..2  magic, format version, kind (1)
 *	 byte 3     precision p (4 to 18)
 *	 then, by the length of the rest of the value:
 *	   0 bytes            an empty sketch
 *	   3 * 2^p / 4 bytes  dense: the m registers of 6 bits each, register i
 *						  in bits 6i to 6i+5 of the bit string whose bit b
 *						  is bit (b mod 8) of byte (b div 8)
 *	   4 * n bytes        sparse: n entries (j << 6) | rank of the non-zero
 *						  registers, strictly increasing in j
 * A value is sparse if and only if 4 * (non-zero registers) is smaller than
 * the dense size, so the bytes are a function of the registers alone and the
 * length tells the encoding.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_sketch/src/hll.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include <math.h>

#include "port/pg_bitutils.h"
#include "utils/builtins.h"

#include "kingres_sketch.h"

PG_FUNCTION_INFO_V1(kingres_sketch_hll_in);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_out);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_recv);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_send);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_to_bytea);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_from_bytea);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_empty);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_add);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_add_hash);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_union);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_fold);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_cardinality);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_precision);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_is_empty);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_agg_add);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_agg_add_p);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_agg_union);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_agg_combine);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_agg_serial);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_agg_deserial);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_agg_final);
PG_FUNCTION_INFO_V1(kingres_sketch_hll_agg_final_card);

/*
 * The in-memory sketch.  While it is small it is a list of entries
 * (j << 6) | rank that may hold several entries for one register; it becomes
 * a plain array of m one-byte registers when the list would no longer be
 * smaller than the dense form.  Which form the state is in is only a matter
 * of speed and memory: the serialized bytes depend on the registers alone.
 */
typedef struct HllState
{
	int			p;
	uint32		m;				/* number of registers, 2^p */
	uint32		sparse_max;		/* longest list that is smaller than dense */
	bool		dense;
	uint8	   *regs;			/* m registers, when dense */
	uint32	   *list;			/* entries, when not dense */
	uint32		nlist;
	uint32		listcap;
	MemoryContext cxt;			/* where the state and its arrays live */
	KsAcct	   *acct;			/* the account of an aggregate state, else NULL */
} HllState;

static inline uint32
hll_dense_bytes(int p)
{
	return (uint32) (((uint64) 3 << p) / 4);
}

static inline int
hll_max_rank(int p)
{
	return 64 - p + 1;
}

static HllState *
hll_new(int p)
{
	HllState   *st = (HllState *) palloc0(sizeof(HllState));

	st->p = p;
	st->cxt = CurrentMemoryContext;
	st->m = (uint32) 1 << p;
	/* the list is smaller than the dense form while 4 * n < dense bytes */
	st->sparse_max = (hll_dense_bytes(p) - 1) / 4;
	return st;
}

static void
hll_free(HllState *st)
{
	if (st->regs)
		pfree(st->regs);
	if (st->list)
		pfree(st->list);
	pfree(st);
}

static int
cmp_u32(const void *a, const void *b)
{
	uint32		x = *(const uint32 *) a;
	uint32		y = *(const uint32 *) b;

	return (x > y) - (x < y);
}

/* Sort the list and keep one entry, the highest rank, per register. */
static void
hll_compact(HllState *st)
{
	uint32		i;
	uint32		out = 0;

	if (st->nlist < 2)
		return;
	qsort(st->list, st->nlist, sizeof(uint32), cmp_u32);
	for (i = 0; i < st->nlist; i++)
	{
		/* entries sort by register and then by rank: the last one wins */
		if (i + 1 < st->nlist && (st->list[i + 1] >> 6) == (st->list[i] >> 6))
			continue;
		st->list[out++] = st->list[i];
	}
	st->nlist = out;
}

static void
hll_densify(HllState *st)
{
	uint32		i;

	if (st->dense)
		return;
	st->regs = (uint8 *) MemoryContextAllocZero(st->cxt, st->m);
	for (i = 0; i < st->nlist; i++)
	{
		uint32		j = st->list[i] >> 6;
		uint8		r = (uint8) (st->list[i] & 63);

		if (st->regs[j] < r)
			st->regs[j] = r;
	}
	if (st->list)
		pfree(st->list);
	st->list = NULL;
	st->nlist = st->listcap = 0;
	st->dense = true;
}

/* Raise register j to at least rank. */
static void
hll_add_entry(HllState *st, uint32 j, uint8 rank)
{
	if (st->dense)
	{
		if (st->regs[j] < rank)
			st->regs[j] = rank;
		return;
	}
	if (st->nlist == st->listcap)
	{
		if (st->listcap < st->sparse_max)
		{
			uint32		newcap = Min(Max(16, st->listcap * 2), st->sparse_max);

			if (st->list == NULL)
				st->list = (uint32 *) MemoryContextAlloc(st->cxt, newcap * sizeof(uint32));
			else
				st->list = (uint32 *) repalloc(st->list, newcap * sizeof(uint32));
			st->listcap = newcap;
		}
		else
		{
			/* The list is at its longest: drop duplicates, or go dense. */
			hll_compact(st);
			if (st->nlist > st->sparse_max / 2)
			{
				hll_densify(st);
				if (st->regs[j] < rank)
					st->regs[j] = rank;
				return;
			}
		}
	}
	st->list[st->nlist++] = (j << 6) | rank;
}

static void
hll_add_hash_value(HllState *st, uint64 h)
{
	uint32		j = (uint32) (h >> (64 - st->p));
	uint64		w = h << st->p;
	int			rank;

	if (w == 0)
		rank = hll_max_rank(st->p);
	else
		rank = 64 - pg_leftmost_one_pos64(w);	/* leading zeros + 1 */
	hll_add_entry(st, j, (uint8) rank);
}

/* Registers as a freshly allocated array of m bytes. */
static uint8 *
hll_registers(HllState *st)
{
	uint8	   *regs = (uint8 *) palloc0(st->m);
	uint32		i;

	if (st->dense)
		memcpy(regs, st->regs, st->m);
	else
		for (i = 0; i < st->nlist; i++)
		{
			uint32		j = st->list[i] >> 6;
			uint8		r = (uint8) (st->list[i] & 63);

			if (regs[j] < r)
				regs[j] = r;
		}
	return regs;
}

/* dst := the register-wise maximum of dst and src; same precision. */
static void
hll_merge(HllState *dst, HllState *src)
{
	uint32		i;

	if (src->dense)
	{
		hll_densify(dst);
		for (i = 0; i < dst->m; i++)
			if (dst->regs[i] < src->regs[i])
				dst->regs[i] = src->regs[i];
	}
	else
	{
		for (i = 0; i < src->nlist; i++)
			hll_add_entry(dst, src->list[i] >> 6, (uint8) (src->list[i] & 63));
	}
}

static HllState *
hll_copy(HllState *st)
{
	HllState   *c = hll_new(st->p);

	hll_merge(c, st);
	return c;
}

/* ---- serialization ---- */

/*
 * The canonical bytes of the registers: sparse when 4 * n is smaller than
 * the dense size, else dense.
 */
static bytea *
hll_serialize(HllState *st)
{
	uint32		dense_bytes = hll_dense_bytes(st->p);
	bytea	   *r;
	uint8	   *out;
	uint32		n;
	uint32		i;

	if (!st->dense)
	{
		hll_compact(st);
		n = st->nlist;
		r = (bytea *) palloc(VARHDRSZ + KS_HEADER_LEN + 4 * (Size) n);
		SET_VARSIZE(r, VARHDRSZ + KS_HEADER_LEN + 4 * (Size) n);
		out = (uint8 *) VARDATA(r);
		out[0] = KS_MAGIC;
		out[1] = KS_FORMAT_VERSION;
		out[2] = KS_KIND_HLL;
		out[3] = (uint8) st->p;
		for (i = 0; i < n; i++)
			ks_put_u32(out + KS_HEADER_LEN + 4 * (Size) i, st->list[i]);
		return r;
	}

	n = 0;
	for (i = 0; i < st->m; i++)
		if (st->regs[i] != 0)
			n++;

	if ((uint64) 4 * n < dense_bytes)
	{
		uint32		w = 0;

		r = (bytea *) palloc(VARHDRSZ + KS_HEADER_LEN + 4 * (Size) n);
		SET_VARSIZE(r, VARHDRSZ + KS_HEADER_LEN + 4 * (Size) n);
		out = (uint8 *) VARDATA(r);
		out[0] = KS_MAGIC;
		out[1] = KS_FORMAT_VERSION;
		out[2] = KS_KIND_HLL;
		out[3] = (uint8) st->p;
		for (i = 0; i < st->m; i++)
			if (st->regs[i] != 0)
				ks_put_u32(out + KS_HEADER_LEN + 4 * (Size) w++, (i << 6) | st->regs[i]);
		return r;
	}

	r = (bytea *) palloc0(VARHDRSZ + KS_HEADER_LEN + dense_bytes);
	SET_VARSIZE(r, VARHDRSZ + KS_HEADER_LEN + dense_bytes);
	out = (uint8 *) VARDATA(r);
	out[0] = KS_MAGIC;
	out[1] = KS_FORMAT_VERSION;
	out[2] = KS_KIND_HLL;
	out[3] = (uint8) st->p;
	for (i = 0; i < st->m; i++)
	{
		uint64		bit = (uint64) 6 * i;
		uint32		byte = (uint32) (bit >> 3);
		int			shift = (int) (bit & 7);
		uint32		v = (uint32) st->regs[i] << shift;

		/* a 6-bit register straddles at most two bytes */
		out[KS_HEADER_LEN + byte] |= (uint8) (v & 0xFF);
		if (shift > 2)
			out[KS_HEADER_LEN + byte + 1] |= (uint8) (v >> 8);
	}
	return r;
}

/*
 * Decode and validate a value.  Returns NULL, and a reason in why[], when
 * the bytes are not a sketch of format version 1.  Every length is checked
 * against the precision before it is used, the registers of a dense value
 * must not exceed the largest rank, the entries of a sparse value must be
 * in range and strictly increasing, and the encoding must be the one that
 * the canonical rule picks, so one set of registers has one byte string.
 */
static HllState *
hll_load(const uint8 *p, Size len, char *why)
{
	int			prec;
	Size		payload;
	Size		dense_bytes;
	HllState   *st;
	uint32		i;

	if (!ks_check_header(p, len, KS_KIND_HLL, why))
		return NULL;
	prec = p[3];
	if (prec < KS_HLL_MIN_PRECISION || prec > KS_HLL_MAX_PRECISION)
	{
		snprintf(why, KS_WHY_LEN, "precision %d is outside %d to %d", prec,
				 KS_HLL_MIN_PRECISION, KS_HLL_MAX_PRECISION);
		return NULL;
	}
	payload = len - KS_HEADER_LEN;
	dense_bytes = hll_dense_bytes(prec);

	st = hll_new(prec);
	if (payload == 0)
		return st;

	if (payload == dense_bytes)
	{
		uint32		nz = 0;
		int			maxrank = hll_max_rank(prec);

		st->regs = (uint8 *) palloc(st->m);
		st->dense = true;
		for (i = 0; i < st->m; i++)
		{
			uint64		bit = (uint64) 6 * i;
			uint32		byte = (uint32) (bit >> 3);
			int			shift = (int) (bit & 7);
			uint32		v = p[KS_HEADER_LEN + byte];

			if (shift > 2)
				v |= (uint32) p[KS_HEADER_LEN + byte + 1] << 8;
			v = (v >> shift) & 63;
			if ((int) v > maxrank)
			{
				snprintf(why, KS_WHY_LEN, "register %u holds %u, more than the largest rank %d",
						 i, v, maxrank);
				hll_free(st);
				return NULL;
			}
			st->regs[i] = (uint8) v;
			if (v != 0)
				nz++;
		}
		if ((uint64) 4 * nz < dense_bytes)
		{
			snprintf(why, KS_WHY_LEN, "dense form with only %u non-zero registers; it must be sparse", nz);
			hll_free(st);
			return NULL;
		}
		return st;
	}

	if (payload > dense_bytes || payload % 4 != 0)
	{
		snprintf(why, KS_WHY_LEN, "length of %zu bytes does not fit precision %d", len, prec);
		hll_free(st);
		return NULL;
	}
	/* sparse: the payload is shorter than the dense form, by the test above */
	st->nlist = st->listcap = (uint32) (payload / 4);
	st->list = (uint32 *) palloc(st->listcap * sizeof(uint32));
	for (i = 0; i < st->nlist; i++)
	{
		uint32		e = ks_get_u32(p + KS_HEADER_LEN + 4 * (Size) i);
		uint32		j = e >> 6;
		uint32		r = e & 63;

		if (j >= st->m || r < 1 || (int) r > hll_max_rank(prec))
		{
			snprintf(why, KS_WHY_LEN, "sparse entry %u (register %u, rank %u) is out of range", i, j, r);
			hll_free(st);
			return NULL;
		}
		if (i > 0 && j <= (st->list[i - 1] >> 6))
		{
			snprintf(why, KS_WHY_LEN, "sparse entry %u is not in increasing register order", i);
			hll_free(st);
			return NULL;
		}
		st->list[i] = e;
	}
	return st;
}

bool
hll_validate(const uint8 *p, Size len, char *why)
{
	HllState   *st = hll_load(p, len, why);

	if (st == NULL)
		return false;
	hll_free(st);
	return true;
}

static HllState *
hll_load_arg(FunctionCallInfo fcinfo, int argno)
{
	const uint8 *p;
	Size		len;
	char		why[KS_WHY_LEN];
	HllState   *st;

	p = ks_arg_bytes(fcinfo, argno, &len);
	st = hll_load(p, len, why);
	if (st == NULL)
		ks_invalid_value("hll", why);
	return st;
}

/* ---- the estimator ---- */

/* sigma(x) of Ertl's paper: x + sum_{k>=1} x^(2^k) 2^(k-1). */
static double
hll_sigma(double x)
{
	double		y = 1.0;
	double		z = x;
	double		zprev;

	if (x == 1.0)
		return INFINITY;
	do
	{
		x = x * x;
		zprev = z;
		z += x * y;
		y += y;
	} while (zprev != z);
	return z;
}

/* tau(x) of Ertl's paper. */
static double
hll_tau(double x)
{
	double		y = 1.0;
	double		z = 1.0 - x;
	double		zprev;

	if (x == 0.0 || x == 1.0)
		return 0.0;
	do
	{
		x = sqrt(x);
		zprev = z;
		y *= 0.5;
		z -= (1.0 - x) * (1.0 - x) * y;
	} while (zprev != z);
	return z / 3.0;
}

/*
 * The estimate of the number of distinct elements: from the histogram C_k
 * of the register values (k = 0 to q + 1, q = 64 - p),
 *	 n = alpha_inf * m^2 / (m * sigma(C_0 / m)
 *						   + sum_{k=1..q} C_k * 2^-k
 *						   + m * tau(1 - C_{q+1} / m) * 2^-q),
 * evaluated by Horner's scheme as in the paper.  An empty sketch gives 0.
 */
static double
hll_estimate(HllState *st)
{
	uint8	   *regs = hll_registers(st);
	uint32		counts[66];
	int			q = 64 - st->p;
	double		m = (double) st->m;
	double		z;
	uint32		i;
	int			k;
	double		est;

	memset(counts, 0, sizeof(counts));
	for (i = 0; i < st->m; i++)
		counts[regs[i]]++;
	pfree(regs);
	if (counts[0] == st->m)
		return 0.0;

	z = m * hll_tau(1.0 - (double) counts[q + 1] / m);
	for (k = q; k >= 1; k--)
		z = 0.5 * (z + (double) counts[k]);
	z += m * hll_sigma((double) counts[0] / m);
	est = 0.7213475204444817 * m * m / z;	/* alpha_inf = 1 / (2 ln 2) */
	return est;
}

static int64
hll_estimate_int64(HllState *st)
{
	double		est = hll_estimate(st);

	if (est >= 9.0e18)
		return PG_INT64_MAX;
	return (int64) floor(est + 0.5);
}

/*
 * A fixed estimate for self_test(): the elements 1 to 1000 hashed by the
 * finishing function alone, at precision 14.
 */
int64
hll_self_test_value(void)
{
	HllState   *st = hll_new(14);
	uint64		i;
	int64		r;

	for (i = 1; i <= 1000; i++)
		hll_add_hash_value(st, ks_mix64(i));
	r = hll_estimate_int64(st);
	hll_free(st);
	return r;
}

/* ---- folding ---- */

/*
 * Reduce to precision p2 < p.  Register j of the old sketch holds the rank
 * of the elements whose top p bits are j; with p2 bits the register is the
 * top p2 bits of j, and the next d = p - p2 bits of the hash are the low d
 * bits of j, so the new rank is read from them when they are not all zero,
 * and is d plus the old rank when they are.  The result is exactly the sketch
 * that would have been built at precision p2 from the same elements.
 */
static HllState *
hll_fold_state(HllState *st, int p2)
{
	HllState   *res = hll_new(p2);
	uint8	   *regs = hll_registers(st);
	int			d = st->p - p2;
	uint32		j;

	for (j = 0; j < st->m; j++)
	{
		uint32		low;
		int			rank;

		if (regs[j] == 0)
			continue;
		low = j & (((uint32) 1 << d) - 1);
		if (low != 0)
			rank = d - pg_leftmost_one_pos32(low);
		else
			rank = d + regs[j];
		hll_add_entry(res, j >> d, (uint8) rank);
	}
	pfree(regs);
	return res;
}

/* ---- SQL: the type ---- */

Datum
kingres_sketch_hll_in(PG_FUNCTION_ARGS)
{
	return ks_in_common(fcinfo, KS_KIND_HLL, "hll", hll_validate);
}

Datum
kingres_sketch_hll_out(PG_FUNCTION_ARGS)
{
	return ks_out_common(fcinfo);
}

Datum
kingres_sketch_hll_recv(PG_FUNCTION_ARGS)
{
	return ks_recv_common(fcinfo, KS_KIND_HLL, "hll", hll_validate);
}

Datum
kingres_sketch_hll_send(PG_FUNCTION_ARGS)
{
	return ks_send_common(fcinfo);
}

Datum
kingres_sketch_hll_to_bytea(PG_FUNCTION_ARGS)
{
	return ks_to_bytea_common(fcinfo);
}

Datum
kingres_sketch_hll_from_bytea(PG_FUNCTION_ARGS)
{
	return ks_from_bytea_common(fcinfo, KS_KIND_HLL, "hll", hll_validate);
}

static int
check_precision(int32 p)
{
	if (p < KS_HLL_MIN_PRECISION || p > KS_HLL_MAX_PRECISION)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("hll precision must be between %d and %d, not %d",
						KS_HLL_MIN_PRECISION, KS_HLL_MAX_PRECISION, p)));
	return p;
}

Datum
kingres_sketch_hll_empty(PG_FUNCTION_ARGS)
{
	uint8		buf[KS_HEADER_LEN];

	buf[0] = KS_MAGIC;
	buf[1] = KS_FORMAT_VERSION;
	buf[2] = KS_KIND_HLL;
	buf[3] = (uint8) check_precision(PG_GETARG_INT32(0));
	PG_RETURN_BYTEA_P(ks_make_bytea(buf, KS_HEADER_LEN));
}

/* hll_add(hll, anyelement): a NULL element leaves the sketch unchanged. */
Datum
kingres_sketch_hll_add(PG_FUNCTION_ARGS)
{
	HllState   *st;

	/* the type of the element is checked whatever the value, even NULL */
	ks_hash_check_type(fcinfo, 1);
	if (PG_ARGISNULL(0))
		PG_RETURN_NULL();
	if (PG_ARGISNULL(1))
		PG_RETURN_DATUM(PG_GETARG_DATUM(0));
	st = hll_load_arg(fcinfo, 0);
	hll_add_hash_value(st, ks_hash_datum(fcinfo, 1, PG_GETARG_DATUM(1)));
	PG_RETURN_BYTEA_P(hll_serialize(st));
}

Datum
kingres_sketch_hll_add_hash(PG_FUNCTION_ARGS)
{
	HllState   *st;

	if (PG_ARGISNULL(0))
		PG_RETURN_NULL();
	if (PG_ARGISNULL(1))
		PG_RETURN_DATUM(PG_GETARG_DATUM(0));
	st = hll_load_arg(fcinfo, 0);
	hll_add_hash_value(st, (uint64) PG_GETARG_INT64(1));
	PG_RETURN_BYTEA_P(hll_serialize(st));
}

static void
check_same_precision(HllState *a, HllState *b)
{
	if (a->p != b->p)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("cannot merge sketches of precision %d and %d", a->p, b->p),
				 errhint("Use hll_fold to bring the sketches to the same precision.")));
}

Datum
kingres_sketch_hll_union(PG_FUNCTION_ARGS)
{
	HllState   *a = hll_load_arg(fcinfo, 0);
	HllState   *b = hll_load_arg(fcinfo, 1);

	check_same_precision(a, b);
	hll_merge(a, b);
	PG_RETURN_BYTEA_P(hll_serialize(a));
}

Datum
kingres_sketch_hll_fold(PG_FUNCTION_ARGS)
{
	HllState   *st = hll_load_arg(fcinfo, 0);
	int			p2 = check_precision(PG_GETARG_INT32(1));

	if (p2 > st->p)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("cannot fold a sketch of precision %d to the higher precision %d",
						st->p, p2)));
	if (p2 < st->p)
		st = hll_fold_state(st, p2);
	PG_RETURN_BYTEA_P(hll_serialize(st));
}

Datum
kingres_sketch_hll_cardinality(PG_FUNCTION_ARGS)
{
	HllState   *st = hll_load_arg(fcinfo, 0);

	PG_RETURN_INT64(hll_estimate_int64(st));
}

Datum
kingres_sketch_hll_precision(PG_FUNCTION_ARGS)
{
	const uint8 *p;
	Size		len;
	char		why[KS_WHY_LEN];

	p = ks_arg_bytes(fcinfo, 0, &len);
	if (!ks_check_header(p, len, KS_KIND_HLL, why) ||
		p[3] < KS_HLL_MIN_PRECISION || p[3] > KS_HLL_MAX_PRECISION)
		ks_invalid_value("hll", "the header is not valid");
	PG_RETURN_INT32(p[3]);
}

Datum
kingres_sketch_hll_is_empty(PG_FUNCTION_ARGS)
{
	HllState   *st = hll_load_arg(fcinfo, 0);

	PG_RETURN_BOOL(!st->dense && st->nlist == 0);
}

/* ---- SQL: the aggregates ---- */

/* The state of an aggregate, or a new one of precision p in the agg context. */
static HllState *
agg_state(FunctionCallInfo fcinfo, int p, MemoryContext aggcontext)
{
	HllState   *st;
	MemoryContext old;
	KsAcct	   *acct;

	if (!PG_ARGISNULL(0))
	{
		st = (HllState *) PG_GETARG_POINTER(0);
		if (st->p != p)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("the precision of the aggregate changed from %d to %d", st->p, p),
					 errhint("The precision must be the same in every row of a group.")));
		return st;
	}
	/* a state context of its own, so that what the state holds is counted */
	acct = ks_acct_create(aggcontext);
	old = MemoryContextSwitchTo(ks_acct_context(acct));
	st = hll_new(p);
	MemoryContextSwitchTo(old);
	st->acct = acct;
	return st;
}

/* hll_add_agg(anyelement), approx_count_distinct(anyelement): precision 14. */
Datum
kingres_sketch_hll_agg_add(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	HllState   *st;

	ks_hash_check_type(fcinfo, 1);
	st = agg_state(fcinfo, KS_HLL_DEFAULT_PRECISION, aggcontext);
	if (!PG_ARGISNULL(1))
		hll_add_hash_value(st, ks_hash_datum(fcinfo, 1, PG_GETARG_DATUM(1)));
	ks_acct_update(st->acct);
	PG_RETURN_POINTER(st);
}

/* The same with a precision argument, which every row of a group repeats. */
Datum
kingres_sketch_hll_agg_add_p(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	int			p;
	HllState   *st;

	ks_hash_check_type(fcinfo, 1);
	KS_REQUIRE_PARAM(fcinfo, 2, "precision");
	p = check_precision(PG_GETARG_INT32(2));
	st = agg_state(fcinfo, p, aggcontext);
	if (!PG_ARGISNULL(1))
		hll_add_hash_value(st, ks_hash_datum(fcinfo, 1, PG_GETARG_DATUM(1)));
	ks_acct_update(st->acct);
	PG_RETURN_POINTER(st);
}

/* hll_union_agg(hll): the sketches must have one precision. */
Datum
kingres_sketch_hll_agg_union(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	HllState   *st = NULL;
	HllState   *other;

	if (!PG_ARGISNULL(0))
		st = (HllState *) PG_GETARG_POINTER(0);
	if (PG_ARGISNULL(1))
		KS_RETURN_STATE(st);

	other = hll_load_arg(fcinfo, 1);
	if (st == NULL)
		st = agg_state(fcinfo, other->p, aggcontext);
	check_same_precision(st, other);
	hll_merge(st, other);
	hll_free(other);
	ks_acct_update(st->acct);
	PG_RETURN_POINTER(st);
}

Datum
kingres_sketch_hll_agg_combine(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	HllState   *a = PG_ARGISNULL(0) ? NULL : (HllState *) PG_GETARG_POINTER(0);
	HllState   *b = PG_ARGISNULL(1) ? NULL : (HllState *) PG_GETARG_POINTER(1);

	if (b == NULL)
		KS_RETURN_STATE(a);
	if (a == NULL)
	{
		KsAcct	   *acct = ks_acct_create(aggcontext);
		MemoryContext old = MemoryContextSwitchTo(ks_acct_context(acct));

		a = hll_copy(b);
		MemoryContextSwitchTo(old);
		a->acct = acct;
		ks_acct_update(acct);
		PG_RETURN_POINTER(a);
	}
	check_same_precision(a, b);
	hll_merge(a, b);
	ks_acct_update(a->acct);
	PG_RETURN_POINTER(a);
}

/* The serialized state is the same byte string as the value of the type. */
Datum
kingres_sketch_hll_agg_serial(PG_FUNCTION_ARGS)
{
	PG_RETURN_BYTEA_P(hll_serialize((HllState *) PG_GETARG_POINTER(0)));
}

Datum
kingres_sketch_hll_agg_deserial(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	const uint8 *p;
	Size		len;
	char		why[KS_WHY_LEN];
	MemoryContext old;
	HllState   *st;
	KsAcct	   *acct;

	p = ks_arg_bytes(fcinfo, 0, &len);
	if (len > ks_max_value_bytes(KS_KIND_HLL))
		ks_invalid_value("hll", "the value is longer than any valid value of the type");
	/* a state of this size is made whole: refuse before it is, not after */
	ks_acct_precheck(len);
	acct = ks_acct_create(aggcontext);
	old = MemoryContextSwitchTo(ks_acct_context(acct));
	st = hll_load(p, len, why);
	MemoryContextSwitchTo(old);
	if (st == NULL)
	{
		ks_invalid_value("hll", why);
	}
	st->acct = acct;
	ks_acct_update(acct);
	PG_RETURN_POINTER(st);
}

/* The final function is not strict: an aggregate over no rows is empty. */
Datum
kingres_sketch_hll_agg_final(PG_FUNCTION_ARGS)
{
	HllState   *st;
	uint8		buf[KS_HEADER_LEN];

	if (PG_ARGISNULL(0))
	{
		buf[0] = KS_MAGIC;
		buf[1] = KS_FORMAT_VERSION;
		buf[2] = KS_KIND_HLL;
		buf[3] = KS_HLL_DEFAULT_PRECISION;
		PG_RETURN_BYTEA_P(ks_make_bytea(buf, KS_HEADER_LEN));
	}
	st = (HllState *) PG_GETARG_POINTER(0);
	PG_RETURN_BYTEA_P(hll_serialize(st));
}

Datum
kingres_sketch_hll_agg_final_card(PG_FUNCTION_ARGS)
{
	if (PG_ARGISNULL(0))
		PG_RETURN_INT64(0);
	PG_RETURN_INT64(hll_estimate_int64((HllState *) PG_GETARG_POINTER(0)));
}
