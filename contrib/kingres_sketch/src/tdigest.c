/*-------------------------------------------------------------------------
 *
 * tdigest.c
 *	  The type tdigest: a merging t-digest for approximate quantiles.
 *
 * Algorithm.  The merging digest of Dunning and Ertl (arXiv:1902.04023,
 * 2019): a sorted list of centroids (mean, weight) and a buffer of points
 * not yet merged.  When the buffer is full, or a value is read, the buffer is
 * sorted and merged with the centroids in one pass.  The pass walks the
 * sorted items and joins an item to the centroid being built as long as the
 * scale function k(q) = delta / (2 pi) * asin(2 q - 1) (the function k_1 of
 * the paper) grows by at most 1 from the weight to the left of the centroid
 * to its right end.  A centroid near q = 0 or q = 1 is therefore small and one
 * near the median large, which is what makes the rank error small in the
 * tails.  Two consecutive centroids always span more than 1 together, so
 * the list holds at most about delta centroids; the code accepts up to
 * 2 * delta + 2 in a value, which no merge of this code exceeds.
 *
 * The digest keeps the exact minimum, maximum and total weight.  A quantile
 * is read from the piecewise linear curve through (0, min), the centroids
 * at (weight to their left + half their own weight, mean), and (total, max).
 * The rank of a value is the inverse of that curve.
 *
 * A merge is not exact and does not depend on the order of the inputs.  The
 * decisions of the merging pass use the C library's asin, so two platforms
 * can differ in the last digits of a centroid for the same input.
 *
 * Format version 1 (little-endian, doubles as their IEEE 754 bit patterns):
 *	 byte 0..3   header (magic, format 1, kind 3, 0)
 *	 4..7        compression delta (10 to 10000)
 *	 8..11       n, the number of centroids
 *	 12..19      total weight (the sum of the weights)
 *	 20..27      minimum, 28..35 maximum (zero when the digest is empty)
 *	 then n centroids, sorted by mean: f64 mean, i64 weight (at least 1)
 * The state of approx_percentile (never a value of the type) has kind 4 and
 * one more f64, the fraction q, after the centroids.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_sketch/src/tdigest.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include <math.h>

#include "catalog/pg_type.h"
#include "common/int.h"
#include "utils/array.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"

#include "kingres_sketch.h"

PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_in);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_out);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_recv);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_send);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_to_bytea);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_from_bytea);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_empty);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_add);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_union);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_quantile);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_quantiles);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_rank);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_count);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_min);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_max);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_compression);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_agg_add);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_agg_add_c);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_agg_add_wc);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_agg_union);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_agg_pct);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_agg_pct_c);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_agg_combine);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_agg_serial);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_agg_deserial);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_agg_final);
PG_FUNCTION_INFO_V1(kingres_sketch_tdigest_agg_final_pct);

typedef struct TdPoint
{
	double		mean;
	int64		weight;
} TdPoint;

typedef struct TdState
{
	int			compression;
	TdPoint    *cent;			/* merged centroids, sorted by mean */
	int			ncent;
	TdPoint    *buf;			/* points not merged yet, unsorted */
	int			nbuf;
	int			bufcap;
	int			bufmax;
	int64		total;			/* weight of the centroids and the buffer */
	double		min;
	double		max;
	bool		has_q;			/* approx_percentile: the fraction is part of the state */
	double		q;
	MemoryContext cxt;
	KsAcct	   *acct;			/* the account of an aggregate state, else NULL */
} TdState;

static TdState *
td_new(int compression, MemoryContext cxt)
{
	TdState    *st = (TdState *) MemoryContextAllocZero(cxt, sizeof(TdState));

	st->compression = compression;
	st->bufmax = 5 * compression;
	st->cxt = cxt;
	return st;
}

static void
td_free(TdState *st)
{
	if (st->cent)
		pfree(st->cent);
	if (st->buf)
		pfree(st->buf);
	pfree(st);
}

static double
td_scale(double q, double delta)
{
	if (q <= 0.0)
		q = 0.0;
	if (q >= 1.0)
		q = 1.0;
	return delta / (2.0 * M_PI) * asin(2.0 * q - 1.0);
}

/* negative zero: a value that is stored as 0, never as -0, so that bytes are a function of content */
#define TD_NEG_ZERO(x) ((x) == 0.0 && signbit(x))

/*
 * The point at fraction t (0 to 1) between a and b, without overflow when
 * b - a is not finite (values of opposite signs near the double limit), and
 * never outside [a, b].
 */
static double
td_lerp(double a, double b, double t)
{
	double		d = b - a;
	double		r;
	double		lo = Min(a, b);
	double		hi = Max(a, b);

	if (!(t > 0.0))
		t = 0.0;
	if (t > 1.0)
		t = 1.0;
	if (isfinite(d))
		r = a + d * t;
	else
		r = a * (1.0 - t) + b * t;
	if (!(r >= lo))
		r = lo;
	if (r > hi)
		r = hi;
	return r + 0.0;
}

static int
cmp_point(const void *pa, const void *pb)
{
	const TdPoint *a = (const TdPoint *) pa;
	const TdPoint *b = (const TdPoint *) pb;

	if (a->mean != b->mean)
		return a->mean < b->mean ? -1 : 1;
	return (a->weight > b->weight) - (a->weight < b->weight);
}

/*
 * Merge the buffer into the centroids.  The two sorted sequences are
 * merged on the fly: the next item is the smaller of the next centroid and
 * the next buffered point.
 */
static void
td_flush(TdState *st)
{
	TdPoint    *out;
	int			nout = 0;
	int			ci = 0;
	int			bi = 0;
	double		total = (double) st->total;
	double		wsofar = 0.0;
	double		klimit;
	TdPoint		cur;
	bool		have = false;

	if (st->nbuf == 0)
		return;
	qsort(st->buf, st->nbuf, sizeof(TdPoint), cmp_point);
	out = (TdPoint *) palloc((Size) (st->ncent + st->nbuf) * sizeof(TdPoint));
	klimit = td_scale(0.0, st->compression) + 1.0;

	while (ci < st->ncent || bi < st->nbuf)
	{
		TdPoint		it;

		if (bi >= st->nbuf ||
			(ci < st->ncent && cmp_point(&st->cent[ci], &st->buf[bi]) <= 0))
			it = st->cent[ci++];
		else
			it = st->buf[bi++];

		if (!have)
		{
			cur = it;
			have = true;
			continue;
		}
		if (td_scale((wsofar + (double) cur.weight + (double) it.weight) / total,
					 st->compression) <= klimit)
		{
			int64		w = cur.weight + it.weight;	/* total fits, so does this */
			double		mean = td_lerp(cur.mean, it.mean, (double) it.weight / (double) w);

			/* keep the means sorted whatever the rounding does */
			if (mean > it.mean)
				mean = it.mean;
			if (mean < cur.mean)
				mean = cur.mean;
			cur.mean = mean;
			cur.weight = w;
		}
		else
		{
			out[nout++] = cur;
			wsofar += (double) cur.weight;
			klimit = td_scale(wsofar / total, st->compression) + 1.0;
			cur = it;
		}
	}
	out[nout++] = cur;

	if (st->cent)
		pfree(st->cent);
	st->cent = (TdPoint *) MemoryContextAlloc(st->cxt, (Size) nout * sizeof(TdPoint));
	memcpy(st->cent, out, (Size) nout * sizeof(TdPoint));
	st->ncent = nout;
	st->nbuf = 0;
	pfree(out);
}

/* Add a point of the given weight; x is finite and weight at least 1. */
static void
td_add_point(TdState *st, double x, int64 weight)
{
	int64		newtotal;

	/* flush first: the merging pass needs the total without the new point */
	if (st->nbuf >= st->bufmax)
		td_flush(st);
	if (pg_add_s64_overflow(st->total, weight, &newtotal))
		ereport(ERROR,
				(errcode(ERRCODE_NUMERIC_VALUE_OUT_OF_RANGE),
				 errmsg("the total weight of a tdigest overflows bigint")));
	x += 0.0;					/* -0 and 0 are one value */
	if (st->total == 0)
		st->min = st->max = x;
	else
	{
		if (x < st->min)
			st->min = x;
		if (x > st->max)
			st->max = x;
	}
	st->total = newtotal;

	if (st->nbuf == st->bufcap)
	{
		int			newcap = Min(Max(64, st->bufcap * 2), st->bufmax);

		if (st->buf)
			st->buf = (TdPoint *) repalloc(st->buf, (Size) newcap * sizeof(TdPoint));
		else
			st->buf = (TdPoint *) MemoryContextAlloc(st->cxt, (Size) newcap * sizeof(TdPoint));
		st->bufcap = newcap;
	}
	st->buf[st->nbuf].mean = x;
	st->buf[st->nbuf].weight = weight;
	st->nbuf++;
}

/* Merge the digest src into dst (same compression); src is flushed first. */
static void
td_merge(TdState *dst, TdState *src)
{
	int			i;

	td_flush(src);
	for (i = 0; i < src->ncent; i++)
		td_add_point(dst, src->cent[i].mean, src->cent[i].weight);
	if (src->ncent > 0)
	{
		/* the exact extremes of src, which the centroid means do not carry */
		if (src->min < dst->min)
			dst->min = src->min;
		if (src->max > dst->max)
			dst->max = src->max;
	}
	td_flush(dst);
}

static TdState *
td_copy(TdState *src, MemoryContext cxt)
{
	TdState    *c = td_new(src->compression, cxt);

	td_merge(c, src);
	c->has_q = src->has_q;
	c->q = src->q;
	return c;
}

/* ---- serialization ---- */

static bytea *
td_serialize(TdState *st)
{
	Size		len;
	bytea	   *r;
	uint8	   *out;
	int			i;
	bool		pct = st->has_q;

	td_flush(st);
	len = 36 + 16 * (Size) st->ncent + (pct ? 8 : 0);
	r = (bytea *) palloc(VARHDRSZ + len);
	SET_VARSIZE(r, VARHDRSZ + len);
	out = (uint8 *) VARDATA(r);
	out[0] = KS_MAGIC;
	out[1] = KS_FORMAT_VERSION;
	out[2] = pct ? KS_KIND_TDIGEST_PCT : KS_KIND_TDIGEST;
	out[3] = 0;
	ks_put_u32(out + 4, (uint32) st->compression);
	ks_put_u32(out + 8, (uint32) st->ncent);
	ks_put_u64(out + 12, (uint64) st->total);
	ks_put_f64(out + 20, st->ncent > 0 ? st->min + 0.0 : 0.0);
	ks_put_f64(out + 28, st->ncent > 0 ? st->max + 0.0 : 0.0);
	for (i = 0; i < st->ncent; i++)
	{
		ks_put_f64(out + 36 + 16 * (Size) i, st->cent[i].mean + 0.0);
		ks_put_u64(out + 44 + 16 * (Size) i, (uint64) st->cent[i].weight);
	}
	if (pct)
		ks_put_f64(out + 36 + 16 * (Size) st->ncent, st->q + 0.0);
	return r;
}

/*
 * Decode and validate.  Every length is checked against the compression and
 * the byte count before it is used; the means must be finite and sorted, the
 * weights positive and summing to the stored total without overflow, and the
 * exact minimum and maximum must enclose the means.
 */
static TdState *
td_load(const uint8 *p, Size len, bool allow_pct, char *why)
{
	int			kind = KS_KIND_TDIGEST;
	uint32		compression;
	uint32		n;
	int64		total;
	double		min;
	double		max;
	Size		need;
	TdState    *st;
	uint32		i;
	int64		sum = 0;
	double		prev = 0.0;

	if (allow_pct && len >= 3 && p[2] == KS_KIND_TDIGEST_PCT)
		kind = KS_KIND_TDIGEST_PCT;
	if (!ks_check_header(p, len, kind, why))
		return NULL;
	if (p[3] != 0)
	{
		snprintf(why, KS_WHY_LEN, "reserved header byte is not zero");
		return NULL;
	}
	if (len < 36)
	{
		snprintf(why, KS_WHY_LEN, "value is too short (%zu bytes)", len);
		return NULL;
	}
	compression = ks_get_u32(p + 4);
	n = ks_get_u32(p + 8);
	total = (int64) ks_get_u64(p + 12);
	min = ks_get_f64(p + 20);
	max = ks_get_f64(p + 28);
	if (compression < KS_TD_MIN_COMPRESSION || compression > KS_TD_MAX_COMPRESSION)
	{
		snprintf(why, KS_WHY_LEN, "compression %u is outside %d to %d", compression,
				 KS_TD_MIN_COMPRESSION, KS_TD_MAX_COMPRESSION);
		return NULL;
	}
	if (n > 2 * compression + 2)
	{
		snprintf(why, KS_WHY_LEN, "%u centroids are more than compression %u allows", n, compression);
		return NULL;
	}
	need = 36 + 16 * (Size) n + (kind == KS_KIND_TDIGEST_PCT ? 8 : 0);
	if (len != need)
	{
		snprintf(why, KS_WHY_LEN, "length %zu does not match %u centroids", len, n);
		return NULL;
	}
	if (total < 0 || (n == 0) != (total == 0))
	{
		snprintf(why, KS_WHY_LEN, "total weight %lld does not match %u centroids",
				 (long long) total, n);
		return NULL;
	}
	if (n == 0)
	{
		if (ks_get_u64(p + 20) != 0 || ks_get_u64(p + 28) != 0)
		{
			snprintf(why, KS_WHY_LEN, "an empty digest has a minimum or maximum");
			return NULL;
		}
	}
	else if (!isfinite(min) || !isfinite(max) || min > max ||
			 TD_NEG_ZERO(min) || TD_NEG_ZERO(max))
	{
		snprintf(why, KS_WHY_LEN, "minimum or maximum is not valid");
		return NULL;
	}

	st = td_new((int) compression, CurrentMemoryContext);
	if (kind == KS_KIND_TDIGEST_PCT)
	{
		st->q = ks_get_f64(p + 36 + 16 * (Size) n);
		st->has_q = true;
		if (!(st->q >= 0.0 && st->q <= 1.0) || TD_NEG_ZERO(st->q))
		{
			snprintf(why, KS_WHY_LEN, "the fraction is not between 0 and 1, or is negative zero");
			goto bad;
		}
	}
	st->total = total;
	st->min = n > 0 ? min : 0.0;
	st->max = n > 0 ? max : 0.0;
	if (n > 0)
		st->cent = (TdPoint *) MemoryContextAlloc(st->cxt, (Size) n * sizeof(TdPoint));
	for (i = 0; i < n; i++)
	{
		double		mean = ks_get_f64(p + 36 + 16 * (Size) i);
		int64		w = (int64) ks_get_u64(p + 44 + 16 * (Size) i);

		if (!isfinite(mean) || w < 1 || TD_NEG_ZERO(mean))
		{
			snprintf(why, KS_WHY_LEN, "centroid %u has a mean or weight that is not valid", i);
			goto bad;
		}
		if ((i > 0 && mean < prev) || mean < st->min || mean > st->max)
		{
			snprintf(why, KS_WHY_LEN, "centroid %u is out of order or outside the minimum and maximum", i);
			goto bad;
		}
		if (pg_add_s64_overflow(sum, w, &sum))
		{
			snprintf(why, KS_WHY_LEN, "the weights overflow");
			goto bad;
		}
		st->cent[i].mean = mean;
		st->cent[i].weight = w;
		prev = mean;
	}
	st->ncent = (int) n;
	if (sum != total)
	{
		snprintf(why, KS_WHY_LEN, "the weights add up to %lld, not to the total %lld",
				 (long long) sum, (long long) total);
		goto bad;
	}
	return st;

bad:
	td_free(st);
	return NULL;
}

bool
tdigest_validate(const uint8 *p, Size len, char *why)
{
	TdState    *st = td_load(p, len, false, why);

	if (st == NULL)
		return false;
	td_free(st);
	return true;
}

/* The same check for the serialized state of an aggregate (kind 3 or 4). */
bool
tdigest_validate_state(const uint8 *p, Size len, char *why)
{
	TdState    *st = td_load(p, len, true, why);

	if (st == NULL)
		return false;
	td_free(st);
	return true;
}

static TdState *
td_load_arg(FunctionCallInfo fcinfo, int argno)
{
	const uint8 *p;
	Size		len;
	char		why[KS_WHY_LEN];
	TdState    *st;

	p = ks_arg_bytes(fcinfo, argno, &len);
	st = td_load(p, len, false, why);
	if (st == NULL)
		ks_invalid_value("tdigest", why);
	return st;
}

/* ---- quantile and rank ---- */

/*
 * The curve through (0, min), the centroids and (total, max): points[k] has
 * a value and a position.  Filled into palloc'd arrays of ncent + 2.
 */
static void
td_curve(TdState *st, double **pos, double **val)
{
	int			n = st->ncent;
	int			i;
	double		cum = 0.0;

	*pos = (double *) palloc((n + 2) * sizeof(double));
	*val = (double *) palloc((n + 2) * sizeof(double));
	(*pos)[0] = 0.0;
	(*val)[0] = st->min;
	for (i = 0; i < n; i++)
	{
		double		w = (double) st->cent[i].weight;

		(*pos)[i + 1] = cum + w / 2.0;
		(*val)[i + 1] = st->cent[i].mean;
		cum += w;
	}
	(*pos)[n + 1] = (double) st->total;
	(*val)[n + 1] = st->max;
}

static double
curve_quantile(const double *pos, const double *val, int n, double total, double q)
{
	double		target = q * total;
	int			lo = 0;
	int			hi = n + 1;

	if (q <= 0.0)
		return val[0];
	if (q >= 1.0)
		return val[n + 1];
	/* the last point whose position is at most the target */
	while (hi - lo > 1)
	{
		int			mid = (lo + hi) / 2;

		if (pos[mid] <= target)
			lo = mid;
		else
			hi = mid;
	}
	if (pos[hi] == pos[lo])
		return val[lo];
	return td_lerp(val[lo], val[hi], (target - pos[lo]) / (pos[hi] - pos[lo]));
}

static double
td_quantile(TdState *st, double q)
{
	double	   *pos;
	double	   *val;
	double		r;

	td_flush(st);
	td_curve(st, &pos, &val);
	r = curve_quantile(pos, val, st->ncent, (double) st->total, q);
	pfree(pos);
	pfree(val);
	return r;
}

static double
td_rank(TdState *st, double x)
{
	double	   *pos;
	double	   *val;
	int			n;
	int			lo;
	int			hi;
	double		r;

	td_flush(st);
	n = st->ncent;
	if (x < st->min)
		return 0.0;
	if (x >= st->max)
		return 1.0;
	td_curve(st, &pos, &val);
	/* the last point whose value is at most x; val[n + 1] = max > x */
	lo = 0;
	hi = n + 1;
	while (hi - lo > 1)
	{
		int			mid = (lo + hi) / 2;

		if (val[mid] <= x)
			lo = mid;
		else
			hi = mid;
	}
	/* halves, so that the differences cannot overflow near the double limit */
	{
		double		den = val[hi] / 2.0 - val[lo] / 2.0;
		double		t = den > 0.0 ? (x / 2.0 - val[lo] / 2.0) / den : 0.0;

		if (!(t > 0.0))
			t = 0.0;
		if (t > 1.0)
			t = 1.0;
		r = pos[lo] + (pos[hi] - pos[lo]) * t;
	}
	r /= (double) st->total;
	pfree(pos);
	pfree(val);
	return Min(Max(r, 0.0), 1.0);
}

/* ---- self_test ---- */

/*
 * A fixed digest for self_test(): compression 20 (a merging pass every 100
 * points) fed 600 points from a formula, so that the merging pass, which
 * decides with asin() of the C library, runs several times.  Results: a hash
 * of the serialized bytes of the digest, of the union of two such digests
 * (the union is a second merging pass over the centroids of the other), and
 * of the bits of a quantile and a rank read from the union, and of td_scale()
 * at 258 fixed arguments.  Two nodes whose
 * C libraries round asin() differently give different bytes here, and the
 * line of self_test() shows it before they exchange stored digests.
 */
static TdState *
td_self_test_digest(int a, int b, double scale)
{
	TdState    *st = td_new(20, CurrentMemoryContext);
	int			i;

	for (i = 1; i <= 600; i++)
		td_add_point(st, (double) ((i * a) % b) * scale, 1 + (i % 3));
	return st;
}

static uint64
td_self_test_bytes(TdState *st)
{
	bytea	   *r = td_serialize(st);
	uint64		h = ks_hash_bytes64((const uint8 *) VARDATA(r), VARSIZE(r) - VARHDRSZ);

	pfree(r);
	return h;
}

void
tdigest_self_test_values(uint64 *single, uint64 *uni, uint64 *read, uint64 *scale)
{
	TdState    *x = td_self_test_digest(7919, 1009, 0.37);
	TdState    *y = td_self_test_digest(104729, 997, 0.53);
	double		q;
	double		rk;
	uint64		bits[2];
	double		sc[258];
	int			i;

	*single = td_self_test_bytes(x);
	td_merge(x, y);
	*uni = td_self_test_bytes(x);
	q = td_quantile(x, 0.99);
	rk = td_rank(x, 100.0);
	memcpy(&bits[0], &q, sizeof(q));
	memcpy(&bits[1], &rk, sizeof(rk));
	*read = ks_hash_bytes64((const uint8 *) bits, sizeof(bits));
	/*
	 * The scale function itself on fixed arguments: the merging pass decides
	 * on differences of it, and a decision flips only when a difference falls
	 * on the boundary, which is rare; the bits of asin() show any difference of
	 * the C library at once.
	 */
	for (i = 0; i <= 257; i++)
		sc[i] = td_scale((double) i / 257.0, 100.0);
	*scale = ks_hash_bytes64((const uint8 *) sc, sizeof(sc));
	td_free(x);
	td_free(y);
}

/* ---- SQL: the type ---- */

Datum
kingres_sketch_tdigest_in(PG_FUNCTION_ARGS)
{
	return ks_in_common(fcinfo, KS_KIND_TDIGEST, "tdigest", tdigest_validate);
}

Datum
kingres_sketch_tdigest_out(PG_FUNCTION_ARGS)
{
	return ks_out_common(fcinfo);
}

Datum
kingres_sketch_tdigest_recv(PG_FUNCTION_ARGS)
{
	return ks_recv_common(fcinfo, KS_KIND_TDIGEST, "tdigest", tdigest_validate);
}

Datum
kingres_sketch_tdigest_send(PG_FUNCTION_ARGS)
{
	return ks_send_common(fcinfo);
}

Datum
kingres_sketch_tdigest_to_bytea(PG_FUNCTION_ARGS)
{
	return ks_to_bytea_common(fcinfo);
}

Datum
kingres_sketch_tdigest_from_bytea(PG_FUNCTION_ARGS)
{
	return ks_from_bytea_common(fcinfo, KS_KIND_TDIGEST, "tdigest", tdigest_validate);
}

static int
check_compression(int32 c)
{
	if (c < KS_TD_MIN_COMPRESSION || c > KS_TD_MAX_COMPRESSION)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("tdigest compression must be between %d and %d, not %d",
						KS_TD_MIN_COMPRESSION, KS_TD_MAX_COMPRESSION, c)));
	return c;
}

static void
check_value(double x)
{
	if (isnan(x) || isinf(x))
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("tdigest does not take NaN or infinite values")));
}

static void
check_weight(int64 w)
{
	if (w < 1)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("tdigest weight must be at least 1, not %lld", (long long) w)));
}

static void
check_fraction(double q)
{
	if (!(q >= 0.0 && q <= 1.0))
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("the fraction must be between 0 and 1")));
}

Datum
kingres_sketch_tdigest_empty(PG_FUNCTION_ARGS)
{
	TdState    *st = td_new(check_compression(PG_GETARG_INT32(0)), CurrentMemoryContext);

	PG_RETURN_BYTEA_P(td_serialize(st));
}

/* tdigest_add(tdigest, x, weight): a NULL x or weight leaves it unchanged. */
Datum
kingres_sketch_tdigest_add(PG_FUNCTION_ARGS)
{
	TdState    *st;
	double		x;
	int64		w = 1;

	if (PG_ARGISNULL(0))
		PG_RETURN_NULL();
	if (PG_ARGISNULL(1) || PG_ARGISNULL(2))
		PG_RETURN_DATUM(PG_GETARG_DATUM(0));
	x = PG_GETARG_FLOAT8(1);
	w = PG_GETARG_INT64(2);
	check_value(x);
	check_weight(w);
	st = td_load_arg(fcinfo, 0);
	td_add_point(st, x, w);
	PG_RETURN_BYTEA_P(td_serialize(st));
}

static void
check_same_compression(TdState *a, TdState *b)
{
	if (a->compression != b->compression)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("cannot merge digests of compression %d and %d",
						a->compression, b->compression)));
}

Datum
kingres_sketch_tdigest_union(PG_FUNCTION_ARGS)
{
	TdState    *a = td_load_arg(fcinfo, 0);
	TdState    *b = td_load_arg(fcinfo, 1);

	check_same_compression(a, b);
	td_merge(a, b);
	PG_RETURN_BYTEA_P(td_serialize(a));
}

Datum
kingres_sketch_tdigest_quantile(PG_FUNCTION_ARGS)
{
	TdState    *st = td_load_arg(fcinfo, 0);
	double		q = PG_GETARG_FLOAT8(1);

	check_fraction(q);
	if (st->total == 0)
		PG_RETURN_NULL();
	PG_RETURN_FLOAT8(td_quantile(st, q));
}

Datum
kingres_sketch_tdigest_quantiles(PG_FUNCTION_ARGS)
{
	TdState    *st = td_load_arg(fcinfo, 0);
	ArrayType  *qs = PG_GETARG_ARRAYTYPE_P(1);
	Datum	   *elems;
	bool	   *nulls;
	int			nelems;
	int			i;
	double	   *pos;
	double	   *val;
	int			dims[1];
	int			lbs[1];

	deconstruct_array(qs, FLOAT8OID, sizeof(float8), true, TYPALIGN_DOUBLE,
					  &elems, &nulls, &nelems);
	for (i = 0; i < nelems; i++)
		if (!nulls[i])
			check_fraction(DatumGetFloat8(elems[i]));
	if (st->total == 0)
		PG_RETURN_NULL();
	td_flush(st);
	td_curve(st, &pos, &val);
	for (i = 0; i < nelems; i++)
		if (!nulls[i])
			elems[i] = Float8GetDatum(curve_quantile(pos, val, st->ncent,
													 (double) st->total,
													 DatumGetFloat8(elems[i])));
	dims[0] = nelems;
	lbs[0] = 1;
	PG_RETURN_ARRAYTYPE_P(construct_md_array(elems, nulls, 1, dims, lbs, FLOAT8OID,
											 sizeof(float8), true, TYPALIGN_DOUBLE));
}

Datum
kingres_sketch_tdigest_rank(PG_FUNCTION_ARGS)
{
	TdState    *st = td_load_arg(fcinfo, 0);
	double		x = PG_GETARG_FLOAT8(1);

	if (isnan(x))
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("tdigest_rank does not take NaN")));
	if (st->total == 0)
		PG_RETURN_NULL();
	PG_RETURN_FLOAT8(td_rank(st, x));
}

Datum
kingres_sketch_tdigest_count(PG_FUNCTION_ARGS)
{
	TdState    *st = td_load_arg(fcinfo, 0);

	PG_RETURN_INT64(st->total);
}

Datum
kingres_sketch_tdigest_min(PG_FUNCTION_ARGS)
{
	TdState    *st = td_load_arg(fcinfo, 0);

	if (st->total == 0)
		PG_RETURN_NULL();
	PG_RETURN_FLOAT8(st->min);
}

Datum
kingres_sketch_tdigest_max(PG_FUNCTION_ARGS)
{
	TdState    *st = td_load_arg(fcinfo, 0);

	if (st->total == 0)
		PG_RETURN_NULL();
	PG_RETURN_FLOAT8(st->max);
}

Datum
kingres_sketch_tdigest_compression(PG_FUNCTION_ARGS)
{
	TdState    *st = td_load_arg(fcinfo, 0);

	PG_RETURN_INT32(st->compression);
}

/* ---- SQL: the aggregates ---- */

/* A new aggregate state, in a state context of its own, so that what it holds is counted. */
static TdState *
td_agg_new(int compression, MemoryContext aggcontext)
{
	KsAcct	   *acct = ks_acct_create(aggcontext);
	TdState    *st = td_new(compression, ks_acct_context(acct));

	st->acct = acct;
	return st;
}

/*
 * The state of an aggregate, or a new one in the agg context.  The
 * compression is repeated by every row of a group (or NULL, meaning the
 * state's own).
 */
static TdState *
agg_state(FunctionCallInfo fcinfo, int compression, MemoryContext aggcontext)
{
	if (!PG_ARGISNULL(0))
	{
		TdState    *st = (TdState *) PG_GETARG_POINTER(0);

		if (st->compression != compression)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("the compression of the aggregate changed from %d to %d",
							st->compression, compression),
					 errhint("The compression must be the same in every row of a group.")));
		return st;
	}
	return td_agg_new(compression, aggcontext);
}

/* The compression of a call: argument "argno" (never NULL), or, with no such argument, the state's or the default. */
static int
agg_compression(FunctionCallInfo fcinfo, int argno)
{
	if (argno >= 0)
	{
		KS_REQUIRE_PARAM(fcinfo, argno, "compression");
		return check_compression(PG_GETARG_INT32(argno));
	}
	if (!PG_ARGISNULL(0))
		return ((TdState *) PG_GETARG_POINTER(0))->compression;
	return KS_TD_DEFAULT_COMPRESSION;
}

Datum
kingres_sketch_tdigest_agg_add(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	TdState    *st = agg_state(fcinfo, agg_compression(fcinfo, -1), aggcontext);

	if (!PG_ARGISNULL(1))
	{
		check_value(PG_GETARG_FLOAT8(1));
		td_add_point(st, PG_GETARG_FLOAT8(1), 1);
	}
	ks_acct_update(st->acct);
	PG_RETURN_POINTER(st);
}

Datum
kingres_sketch_tdigest_agg_add_c(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	TdState    *st = agg_state(fcinfo, agg_compression(fcinfo, 2), aggcontext);

	if (!PG_ARGISNULL(1))
	{
		check_value(PG_GETARG_FLOAT8(1));
		td_add_point(st, PG_GETARG_FLOAT8(1), 1);
	}
	ks_acct_update(st->acct);
	PG_RETURN_POINTER(st);
}

/* tdigest_weighted_agg(x, weight, compression) */
Datum
kingres_sketch_tdigest_agg_add_wc(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	TdState    *st = agg_state(fcinfo, agg_compression(fcinfo, 3), aggcontext);

	if (!PG_ARGISNULL(1) && !PG_ARGISNULL(2))
	{
		check_value(PG_GETARG_FLOAT8(1));
		check_weight(PG_GETARG_INT64(2));
		td_add_point(st, PG_GETARG_FLOAT8(1), PG_GETARG_INT64(2));
	}
	ks_acct_update(st->acct);
	PG_RETURN_POINTER(st);
}

Datum
kingres_sketch_tdigest_agg_union(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	TdState    *st = NULL;
	TdState    *other;

	if (!PG_ARGISNULL(0))
		st = (TdState *) PG_GETARG_POINTER(0);
	if (PG_ARGISNULL(1))
		KS_RETURN_STATE(st);

	other = td_load_arg(fcinfo, 1);
	if (st == NULL)
		st = td_agg_new(other->compression, aggcontext);
	check_same_compression(st, other);
	td_merge(st, other);
	td_free(other);
	ks_acct_update(st->acct);
	PG_RETURN_POINTER(st);
}

/* The fraction of approx_percentile: from the first row, then the same. */
static void
agg_set_fraction(TdState *st, double q)
{
	check_fraction(q);
	if (!st->has_q)
	{
		st->has_q = true;
		st->q = q + 0.0;
	}
	else if (st->q != q)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("the fraction of approx_percentile changed from %g to %g", st->q, q),
				 errhint("The fraction must be a constant of the call.")));
}

static Datum
agg_pct(FunctionCallInfo fcinfo, int comp_argno)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	TdState    *st = agg_state(fcinfo, agg_compression(fcinfo, comp_argno), aggcontext);

	if (!PG_ARGISNULL(1))
		agg_set_fraction(st, PG_GETARG_FLOAT8(1));
	if (!PG_ARGISNULL(2) && !PG_ARGISNULL(1))
	{
		check_value(PG_GETARG_FLOAT8(2));
		td_add_point(st, PG_GETARG_FLOAT8(2), 1);
	}
	ks_acct_update(st->acct);
	PG_RETURN_POINTER(st);
}

/* approx_percentile(q, x) */
Datum
kingres_sketch_tdigest_agg_pct(PG_FUNCTION_ARGS)
{
	return agg_pct(fcinfo, -1);
}

/* approx_percentile(q, x, compression) */
Datum
kingres_sketch_tdigest_agg_pct_c(PG_FUNCTION_ARGS)
{
	return agg_pct(fcinfo, 3);
}

Datum
kingres_sketch_tdigest_agg_combine(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	TdState    *a = PG_ARGISNULL(0) ? NULL : (TdState *) PG_GETARG_POINTER(0);
	TdState    *b = PG_ARGISNULL(1) ? NULL : (TdState *) PG_GETARG_POINTER(1);

	if (b == NULL)
		KS_RETURN_STATE(a);
	if (a == NULL)
	{
		KsAcct	   *acct = ks_acct_create(aggcontext);

		a = td_copy(b, ks_acct_context(acct));
		a->acct = acct;
		ks_acct_update(acct);
		PG_RETURN_POINTER(a);
	}
	check_same_compression(a, b);
	if (b->has_q)
		agg_set_fraction(a, b->q);
	td_merge(a, b);
	ks_acct_update(a->acct);
	PG_RETURN_POINTER(a);
}

Datum
kingres_sketch_tdigest_agg_serial(PG_FUNCTION_ARGS)
{
	PG_RETURN_BYTEA_P(td_serialize((TdState *) PG_GETARG_POINTER(0)));
}

Datum
kingres_sketch_tdigest_agg_deserial(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	const uint8 *p;
	Size		len;
	char		why[KS_WHY_LEN];
	MemoryContext old;
	TdState    *st;
	KsAcct	   *acct;

	p = ks_arg_bytes(fcinfo, 0, &len);
	if (len > ks_max_value_bytes(KS_KIND_TDIGEST))
		ks_invalid_value("tdigest", "the value is longer than any valid value of the type");
	/* a state of this size is made whole: refuse before it is, not after */
	ks_acct_precheck(len);
	acct = ks_acct_create(aggcontext);
	old = MemoryContextSwitchTo(ks_acct_context(acct));
	st = td_load(p, len, true, why);
	MemoryContextSwitchTo(old);
	if (st == NULL)
	{
		ks_invalid_value("tdigest", why);
	}
	st->acct = acct;
	ks_acct_update(acct);
	PG_RETURN_POINTER(st);
}

Datum
kingres_sketch_tdigest_agg_final(PG_FUNCTION_ARGS)
{
	TdState    *st;

	if (PG_ARGISNULL(0))
		st = td_new(KS_TD_DEFAULT_COMPRESSION, CurrentMemoryContext);
	else
		st = (TdState *) PG_GETARG_POINTER(0);
	PG_RETURN_BYTEA_P(td_serialize(st));
}

Datum
kingres_sketch_tdigest_agg_final_pct(PG_FUNCTION_ARGS)
{
	TdState    *st;

	if (PG_ARGISNULL(0))
		PG_RETURN_NULL();
	st = (TdState *) PG_GETARG_POINTER(0);
	if (st->total == 0 || !st->has_q)
		PG_RETURN_NULL();
	PG_RETURN_FLOAT8(td_quantile(st, st->q));
}
