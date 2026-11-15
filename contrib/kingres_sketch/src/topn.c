/*-------------------------------------------------------------------------
 *
 * topn.c
 *	  The type topn: a Space-Saving summary of the most frequent items.
 *
 * Algorithm.  The summary of Metwally, Agrawal and El Abbadi (ICDT 2005)
 * keeps k counters (item, count, error).  An item that has a counter
 * increments it.  A new item takes a free counter with (1, 0), or, when all k
 * are in use, the counter with the smallest count c: the item is replaced,
 * the error becomes c and the count c + 1.  After N insertions the count of
 * a counter is at least the true frequency of its item and at most that plus
 * its error, and every error is at most N / k.
 *
 * Merge.  The summaries of two streams are merged as in Agarwal et al.
 * (PODS 2012, "Mergeable summaries"): the counts and errors of an item in
 * both are added; an item missing from a summary that is full is given that
 * summary's smallest count as additional count and error (an absent item has
 * at most that frequency there); an item missing from a summary with free
 * counters is added with zero.  The k largest counts are kept.  For a
 * summary with k counters, count - error <= true <= count, every error is at
 * most N / k and the counts add up to at most N hold after any sequence of
 * insertions and merges: an item missing from a summary S that is full has
 * frequency at most min(S); the k kept counts are the largest k of a set in
 * which every count of S1 and S2 appears at most once, so their sum does
 * not exceed the sums of the two summaries.  The theorem of the paper is
 * stated for Misra-Gries summaries; the argument above is the direct one
 * for this summary and the tests check the invariants after random merges.
 *
 * The merge is not associative or commutative in its exact result: only the
 * guarantees hold.
 *
 * Format version 1 (little-endian):
 *	 byte 0..3   header (magic, format 1, kind 2, 0)
 *	 4..7        k, the capacity
 *	 8..15       N, the number of items added (including merged summaries)
 *	 16..19      n, the number of counters
 *	 then n entries, sorted by (count descending, item bytes ascending):
 *	   u16 item length, i64 count, i64 error, the item bytes
 * Items are strings in the database encoding of at most 1024 bytes.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: PostgreSQL
 *
 * IDENTIFICATION
 *	  contrib/kingres_sketch/src/topn.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "catalog/pg_type.h"
#include "common/hashfn.h"
#include "common/int.h"
#include "funcapi.h"
#include "mb/pg_wchar.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"
#include "utils/tuplestore.h"

#include "kingres_sketch.h"

PG_FUNCTION_INFO_V1(kingres_sketch_topn_in);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_out);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_recv);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_send);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_to_bytea);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_from_bytea);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_empty);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_add);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_union);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_list);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_total);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_capacity);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_agg_add);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_agg_add_k);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_agg_union);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_agg_combine);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_agg_serial);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_agg_deserial);
PG_FUNCTION_INFO_V1(kingres_sketch_topn_agg_final);

typedef struct TnCounter
{
	char	   *item;			/* palloc'd, len bytes plus a zero byte */
	uint32		len;
	uint32		hash;
	int64		count;
	int64		err;
	int32		next;			/* next counter in the hash chain, or -1 */
	int32		hpos;			/* position in the heap */
} TnCounter;

/*
 * The in-memory summary: counters in an array that grows as needed up to k
 * (a value of capacity 100000 holding two items takes no more memory than
 * its content), a chained hash table from item to counter, and, once all k
 * counters are in use, a min-heap on the count so that the counter to take
 * over is found in constant time.
 */
typedef struct TnState
{
	uint32		k;
	int64		total;
	int32		n;
	int32		cap;
	TnCounter  *c;
	int32	   *buckets;
	uint32		nbuckets;		/* a power of two, at least 2 * cap */
	int32	   *heap;			/* counter indexes, when heap_ok */
	bool		heap_ok;
	MemoryContext cxt;			/* where the state and its arrays live */
} TnState;

static TnState *
tn_new(uint32 k, MemoryContext cxt)
{
	TnState    *st = (TnState *) MemoryContextAllocZero(cxt, sizeof(TnState));

	st->k = k;
	st->cxt = cxt;
	return st;
}

static void
tn_free(TnState *st)
{
	int32		i;

	for (i = 0; i < st->n; i++)
		pfree(st->c[i].item);
	if (st->c)
		pfree(st->c);
	if (st->buckets)
		pfree(st->buckets);
	if (st->heap)
		pfree(st->heap);
	pfree(st);
}

static void
tn_link(TnState *st, int32 idx)
{
	uint32		b = st->c[idx].hash & (st->nbuckets - 1);

	st->c[idx].next = st->buckets[b];
	st->buckets[b] = idx;
}

static void
tn_unlink(TnState *st, int32 idx)
{
	uint32		b = st->c[idx].hash & (st->nbuckets - 1);
	int32	   *pp = &st->buckets[b];

	while (*pp != idx)
		pp = &st->c[*pp].next;
	*pp = st->c[idx].next;
}

static int32
tn_find(TnState *st, const char *item, uint32 len, uint32 hash)
{
	int32		i;

	if (st->n == 0)
		return -1;
	for (i = st->buckets[hash & (st->nbuckets - 1)]; i >= 0; i = st->c[i].next)
	{
		if (st->c[i].hash == hash && st->c[i].len == len &&
			memcmp(st->c[i].item, item, len) == 0)
			return i;
	}
	return -1;
}

static void
tn_grow(TnState *st)
{
	int32		newcap = st->cap == 0 ? 16 : st->cap * 2;
	int32		i;

	if ((uint32) newcap > st->k)
		newcap = (int32) st->k;
	Assert(newcap > st->cap);
	if (st->c)
		st->c = (TnCounter *) repalloc(st->c, newcap * sizeof(TnCounter));
	else
		st->c = (TnCounter *) MemoryContextAlloc(st->cxt, newcap * sizeof(TnCounter));
	if (st->heap)
		st->heap = (int32 *) repalloc(st->heap, newcap * sizeof(int32));
	st->cap = newcap;

	/* the hash table holds at most half full: rebuild it for the new size */
	if (st->buckets)
		pfree(st->buckets);
	st->nbuckets = 16;
	while (st->nbuckets < 2 * (uint32) newcap)
		st->nbuckets *= 2;
	st->buckets = (int32 *) MemoryContextAlloc(st->cxt, st->nbuckets * sizeof(int32));
	for (i = 0; i < (int32) st->nbuckets; i++)
		st->buckets[i] = -1;
	for (i = 0; i < st->n; i++)
		tn_link(st, i);
}

static void
heap_swap(TnState *st, int32 a, int32 b)
{
	int32		ia = st->heap[a];
	int32		ib = st->heap[b];

	st->heap[a] = ib;
	st->heap[b] = ia;
	st->c[ib].hpos = a;
	st->c[ia].hpos = b;
}

/* The count of a heap entry only grows, so it only moves down. */
static void
heap_sift_down(TnState *st, int32 pos)
{
	for (;;)
	{
		int32		l = 2 * pos + 1;
		int32		r = l + 1;
		int32		small = pos;

		if (l < st->n && st->c[st->heap[l]].count < st->c[st->heap[small]].count)
			small = l;
		if (r < st->n && st->c[st->heap[r]].count < st->c[st->heap[small]].count)
			small = r;
		if (small == pos)
			break;
		heap_swap(st, pos, small);
		pos = small;
	}
}

static void
heap_build(TnState *st)
{
	int32		i;

	if (st->heap == NULL)
		st->heap = (int32 *) MemoryContextAlloc(st->cxt, st->cap * sizeof(int32));
	for (i = 0; i < st->n; i++)
	{
		st->heap[i] = i;
		st->c[i].hpos = i;
	}
	for (i = st->n / 2 - 1; i >= 0; i--)
		heap_sift_down(st, i);
	st->heap_ok = true;
}

/* Append a counter; the item is copied.  The caller checked n < k. */
static int32
tn_append(TnState *st, const char *item, uint32 len, uint32 hash, int64 count, int64 err)
{
	int32		idx;
	TnCounter  *c;

	Assert((uint32) st->n < st->k);
	if (st->n == st->cap)
		tn_grow(st);
	idx = st->n++;
	c = &st->c[idx];
	c->item = (char *) MemoryContextAlloc(st->cxt, len + 1);
	memcpy(c->item, item, len);
	c->item[len] = '\0';
	c->len = len;
	c->hash = hash;
	c->count = count;
	c->err = err;
	c->hpos = -1;
	tn_link(st, idx);
	st->heap_ok = false;
	if ((uint32) st->n == st->k)
		heap_build(st);
	return idx;
}

/* One insertion of an item (the Space-Saving step). */
static void
tn_add(TnState *st, const char *item, uint32 len)
{
	uint32		hash = hash_bytes((const unsigned char *) item, (int) len);
	int32		idx;

	if (st->total == PG_INT64_MAX)
		ereport(ERROR,
				(errcode(ERRCODE_NUMERIC_VALUE_OUT_OF_RANGE),
				 errmsg("topn total count overflows bigint")));
	st->total++;

	idx = tn_find(st, item, len, hash);
	if (idx >= 0)
	{
		st->c[idx].count++;
		if (st->heap_ok)
			heap_sift_down(st, st->c[idx].hpos);
		return;
	}
	if ((uint32) st->n < st->k)
	{
		tn_append(st, item, len, hash, 1, 0);
		return;
	}

	/* all counters in use: take over the one with the smallest count */
	{
		int32		v = st->heap[0];
		TnCounter  *c = &st->c[v];
		int64		min = c->count;

		tn_unlink(st, v);
		pfree(c->item);
		c->item = (char *) MemoryContextAlloc(st->cxt, len + 1);
		memcpy(c->item, item, len);
		c->item[len] = '\0';
		c->len = len;
		c->hash = hash;
		c->err = min;
		c->count = min + 1;
		tn_link(st, v);
		heap_sift_down(st, 0);
	}
}

/* ---- merge ---- */

typedef struct TnEntry
{
	const char *item;
	uint32		len;
	uint32		hash;
	int64		count;
	int64		err;
} TnEntry;

static int
cmp_item_bytes(const char *a, uint32 alen, const char *b, uint32 blen)
{
	int			c = memcmp(a, b, Min(alen, blen));

	if (c != 0)
		return c;
	return (alen > blen) - (alen < blen);
}

/* count descending, then item bytes ascending: the canonical order */
static int
cmp_entry(const void *pa, const void *pb)
{
	const TnEntry *a = (const TnEntry *) pa;
	const TnEntry *b = (const TnEntry *) pb;

	if (a->count != b->count)
		return a->count > b->count ? -1 : 1;
	return cmp_item_bytes(a->item, a->len, b->item, b->len);
}

static int64
tn_min_count(TnState *st)
{
	int64		min;
	int32		i;

	if ((uint32) st->n < st->k || st->n == 0)
		return 0;
	min = st->c[0].count;
	for (i = 1; i < st->n; i++)
		if (st->c[i].count < min)
			min = st->c[i].count;
	return min;
}

static void
add_or_error(int64 a, int64 b, int64 *res)
{
	if (pg_add_s64_overflow(a, b, res))
		ereport(ERROR,
				(errcode(ERRCODE_NUMERIC_VALUE_OUT_OF_RANGE),
				 errmsg("topn count overflows bigint")));
}

/* Merge src into dst (same capacity); src is not changed. */
static void
tn_merge(TnState *dst, TnState *src)
{
	int64		m1 = tn_min_count(dst);
	int64		m2 = tn_min_count(src);
	TnEntry    *e;
	int32		ne = 0;
	int32		i;
	int64		total;
	TnState    *res;

	e = (TnEntry *) palloc(((Size) dst->n + src->n + 1) * sizeof(TnEntry));
	for (i = 0; i < dst->n; i++)
	{
		TnCounter  *a = &dst->c[i];
		int32		j = tn_find(src, a->item, a->len, a->hash);

		e[ne].item = a->item;
		e[ne].len = a->len;
		e[ne].hash = a->hash;
		if (j >= 0)
		{
			add_or_error(a->count, src->c[j].count, &e[ne].count);
			add_or_error(a->err, src->c[j].err, &e[ne].err);
		}
		else
		{
			add_or_error(a->count, m2, &e[ne].count);
			add_or_error(a->err, m2, &e[ne].err);
		}
		ne++;
	}
	for (i = 0; i < src->n; i++)
	{
		TnCounter  *b = &src->c[i];

		if (tn_find(dst, b->item, b->len, b->hash) >= 0)
			continue;
		e[ne].item = b->item;
		e[ne].len = b->len;
		e[ne].hash = b->hash;
		add_or_error(b->count, m1, &e[ne].count);
		add_or_error(b->err, m1, &e[ne].err);
		ne++;
	}
	qsort(e, ne, sizeof(TnEntry), cmp_entry);

	add_or_error(dst->total, src->total, &total);
	res = tn_new(dst->k, dst->cxt);
	res->total = total;
	for (i = 0; i < ne && (uint32) i < dst->k; i++)
		tn_append(res, e[i].item, e[i].len, e[i].hash, e[i].count, e[i].err);
	pfree(e);

	/* move the result into dst, so the caller's pointer stays valid */
	for (i = 0; i < dst->n; i++)
		pfree(dst->c[i].item);
	if (dst->c)
		pfree(dst->c);
	if (dst->buckets)
		pfree(dst->buckets);
	if (dst->heap)
		pfree(dst->heap);
	*dst = *res;
	pfree(res);
}

static TnState *
tn_copy(TnState *st)
{
	TnState    *c = tn_new(st->k, CurrentMemoryContext);
	int32		i;

	for (i = 0; i < st->n; i++)
		tn_append(c, st->c[i].item, st->c[i].len, st->c[i].hash,
				  st->c[i].count, st->c[i].err);
	c->total = st->total;
	return c;
}

/* ---- serialization ---- */

static TnState *sort_state;

static int
cmp_index(const void *pa, const void *pb)
{
	const TnCounter *a = &sort_state->c[*(const int32 *) pa];
	const TnCounter *b = &sort_state->c[*(const int32 *) pb];

	if (a->count != b->count)
		return a->count > b->count ? -1 : 1;
	return cmp_item_bytes(a->item, a->len, b->item, b->len);
}

static bytea *
tn_serialize(TnState *st)
{
	int32	   *idx = (int32 *) palloc((st->n + 1) * sizeof(int32));
	Size		len = 20;
	int32		i;
	bytea	   *r;
	uint8	   *out;

	for (i = 0; i < st->n; i++)
	{
		idx[i] = i;
		len += 18 + st->c[i].len;
	}
	sort_state = st;
	qsort(idx, st->n, sizeof(int32), cmp_index);

	r = (bytea *) palloc(VARHDRSZ + len);
	SET_VARSIZE(r, VARHDRSZ + len);
	out = (uint8 *) VARDATA(r);
	out[0] = KS_MAGIC;
	out[1] = KS_FORMAT_VERSION;
	out[2] = KS_KIND_TOPN;
	out[3] = 0;
	ks_put_u32(out + 4, st->k);
	ks_put_u64(out + 8, (uint64) st->total);
	ks_put_u32(out + 16, (uint32) st->n);
	out += 20;
	for (i = 0; i < st->n; i++)
	{
		TnCounter  *c = &st->c[idx[i]];

		ks_put_u16(out, (uint16) c->len);
		ks_put_u64(out + 2, (uint64) c->count);
		ks_put_u64(out + 10, (uint64) c->err);
		memcpy(out + 18, c->item, c->len);
		out += 18 + c->len;
	}
	pfree(idx);
	return r;
}

/*
 * Decode and validate.  Besides the lengths, the checks are the invariants
 * of a summary: 1 <= n <= k counters (or none), items unique, valid in the
 * database encoding and at most 1024 bytes, 0 <= error < count <= N, the
 * counts adding up to at most N, every error at most N / k, and the entries
 * in the canonical order.
 */
static TnState *
tn_load(const uint8 *p, Size len, char *why)
{
	uint32		k;
	int64		total;
	uint32		n;
	Size		pos;
	TnState    *st;
	uint32		i;
	int64		sum = 0;
	int64		prev_count = 0;
	const char *prev_item = NULL;
	uint32		prev_len = 0;

	if (!ks_check_header(p, len, KS_KIND_TOPN, why))
		return NULL;
	if (p[3] != 0)
	{
		snprintf(why, KS_WHY_LEN, "reserved header byte is not zero");
		return NULL;
	}
	if (len < 20)
	{
		snprintf(why, KS_WHY_LEN, "value is too short (%zu bytes)", len);
		return NULL;
	}
	k = ks_get_u32(p + 4);
	total = (int64) ks_get_u64(p + 8);
	n = ks_get_u32(p + 16);
	if (k < KS_TOPN_MIN_CAPACITY || k > KS_TOPN_MAX_CAPACITY)
	{
		snprintf(why, KS_WHY_LEN, "capacity %u is outside %d to %d", k,
				 KS_TOPN_MIN_CAPACITY, KS_TOPN_MAX_CAPACITY);
		return NULL;
	}
	if (total < 0)
	{
		snprintf(why, KS_WHY_LEN, "negative total count");
		return NULL;
	}
	if (n > k || (uint64) n * 18 > len - 20)
	{
		snprintf(why, KS_WHY_LEN, "counter count %u does not fit the capacity or the length", n);
		return NULL;
	}

	st = tn_new(k, CurrentMemoryContext);
	st->total = total;
	pos = 20;
	for (i = 0; i < n; i++)
	{
		uint32		ilen;
		int64		count;
		int64		err;
		const char *item;

		if (len - pos < 18)
		{
			snprintf(why, KS_WHY_LEN, "counter %u is cut off", i);
			goto bad;
		}
		ilen = ks_get_u16(p + pos);
		count = (int64) ks_get_u64(p + pos + 2);
		err = (int64) ks_get_u64(p + pos + 10);
		pos += 18;
		if (ilen > KS_TOPN_MAX_ITEM_BYTES || ilen > len - pos)
		{
			snprintf(why, KS_WHY_LEN, "item length %u of counter %u is not valid", ilen, i);
			goto bad;
		}
		item = (const char *) p + pos;
		pos += ilen;
		if (count < 1 || err < 0 || err >= count || count > total)
		{
			snprintf(why, KS_WHY_LEN, "count %lld or error %lld of counter %u is not valid",
					 (long long) count, (long long) err, i);
			goto bad;
		}
		if (pg_add_s64_overflow(sum, count, &sum) || sum > total)
		{
			snprintf(why, KS_WHY_LEN, "the counts add up to more than the total");
			goto bad;
		}
		if (err > total / (int64) k)
		{
			snprintf(why, KS_WHY_LEN, "error of counter %u exceeds total / capacity", i);
			goto bad;
		}
		if (!pg_verifymbstr(item, (int) ilen, true))
		{
			snprintf(why, KS_WHY_LEN, "item of counter %u is not valid in the database encoding", i);
			goto bad;
		}
		if (i > 0 && (count > prev_count ||
					  (count == prev_count &&
					   cmp_item_bytes(prev_item, prev_len, item, ilen) >= 0)))
		{
			snprintf(why, KS_WHY_LEN, "counter %u is not in the canonical order", i);
			goto bad;
		}
		if (tn_find(st, item, ilen, hash_bytes((const unsigned char *) item, (int) ilen)) >= 0)
		{
			snprintf(why, KS_WHY_LEN, "item of counter %u is repeated", i);
			goto bad;
		}
		tn_append(st, item, ilen, hash_bytes((const unsigned char *) item, (int) ilen),
				  count, err);
		prev_count = count;
		prev_item = st->c[st->n - 1].item;
		prev_len = ilen;
	}
	if (pos != len)
	{
		snprintf(why, KS_WHY_LEN, "bytes follow the last counter");
		goto bad;
	}
	return st;

bad:
	tn_free(st);
	return NULL;
}

bool
topn_validate(const uint8 *p, Size len, char *why)
{
	TnState    *st = tn_load(p, len, why);

	if (st == NULL)
		return false;
	tn_free(st);
	return true;
}

static TnState *
tn_load_arg(FunctionCallInfo fcinfo, int argno)
{
	const uint8 *p;
	Size		len;
	char		why[KS_WHY_LEN];
	TnState    *st;

	p = ks_arg_bytes(fcinfo, argno, &len);
	st = tn_load(p, len, why);
	if (st == NULL)
		ks_invalid_value("topn", why);
	return st;
}

/* ---- items ---- */

typedef struct TnItemCache
{
	Oid			typid;
	bool		is_text;		/* text or varchar: the bytes are the item */
	Oid			outfunc;
} TnItemCache;

/*
 * The canonical text of an element.  Only types whose output does not
 * depend on a session setting are accepted; the others are refused so that
 * the same value is always the same item.  Returns a palloc'd or detoasted
 * string, not zero-terminated for text.
 */
static const char *
tn_item_text(FunctionCallInfo fcinfo, int argno, Datum value, uint32 *len)
{
	TnItemCache *c = (TnItemCache *) fcinfo->flinfo->fn_extra;
	Oid			typid = get_fn_expr_argtype(fcinfo->flinfo, argno);
	const char *s;
	Size		l;

	if (!OidIsValid(typid))
		ereport(ERROR,
				(errcode(ERRCODE_INDETERMINATE_DATATYPE),
				 errmsg("could not determine the data type of the item")));
	if (c == NULL || c->typid != typid)
	{
		Oid			base = getBaseType(typid);
		bool		isvarlena;

		switch (base)
		{
			case INT2OID:
			case INT4OID:
			case INT8OID:
			case NUMERICOID:
			case TEXTOID:
			case VARCHAROID:
			case BPCHAROID:
			case NAMEOID:
			case UUIDOID:
			case BOOLOID:
				break;
			default:
				ereport(ERROR,
						(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
						 errmsg("topn does not take type %s", format_type_be(typid)),
						 errhint("Cast the value to text with an explicit format.")));
		}
		c = (TnItemCache *) MemoryContextAlloc(fcinfo->flinfo->fn_mcxt, sizeof(TnItemCache));
		c->typid = typid;
		c->is_text = (base == TEXTOID || base == VARCHAROID);
		getTypeOutputInfo(base, &c->outfunc, &isvarlena);
		fcinfo->flinfo->fn_extra = c;
	}

	if (c->is_text)
	{
		struct varlena *v = pg_detoast_datum_packed((struct varlena *) DatumGetPointer(value));

		s = VARDATA_ANY(v);
		l = VARSIZE_ANY_EXHDR(v);
	}
	else
	{
		char	   *o = OidOutputFunctionCall(c->outfunc, value);

		s = o;
		l = strlen(o);
	}
	if (l > KS_TOPN_MAX_ITEM_BYTES)
		ereport(ERROR,
				(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
				 errmsg("topn item is %zu bytes long, the limit is %d bytes",
						l, KS_TOPN_MAX_ITEM_BYTES)));
	*len = (uint32) l;
	return s;
}

static int
check_capacity(int32 k)
{
	if (k < KS_TOPN_MIN_CAPACITY || k > KS_TOPN_MAX_CAPACITY)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("topn capacity must be between %d and %d, not %d",
						KS_TOPN_MIN_CAPACITY, KS_TOPN_MAX_CAPACITY, k)));
	return k;
}

/* ---- SQL: the type ---- */

Datum
kingres_sketch_topn_in(PG_FUNCTION_ARGS)
{
	return ks_in_common(fcinfo, KS_KIND_TOPN, "topn", topn_validate);
}

Datum
kingres_sketch_topn_out(PG_FUNCTION_ARGS)
{
	return ks_out_common(fcinfo);
}

Datum
kingres_sketch_topn_recv(PG_FUNCTION_ARGS)
{
	return ks_recv_common(fcinfo, KS_KIND_TOPN, "topn", topn_validate);
}

Datum
kingres_sketch_topn_send(PG_FUNCTION_ARGS)
{
	return ks_send_common(fcinfo);
}

Datum
kingres_sketch_topn_to_bytea(PG_FUNCTION_ARGS)
{
	return ks_to_bytea_common(fcinfo);
}

Datum
kingres_sketch_topn_from_bytea(PG_FUNCTION_ARGS)
{
	return ks_from_bytea_common(fcinfo, KS_KIND_TOPN, "topn", topn_validate);
}

Datum
kingres_sketch_topn_empty(PG_FUNCTION_ARGS)
{
	TnState    *st = tn_new(check_capacity(PG_GETARG_INT32(0)), CurrentMemoryContext);

	PG_RETURN_BYTEA_P(tn_serialize(st));
}

Datum
kingres_sketch_topn_add(PG_FUNCTION_ARGS)
{
	TnState    *st;
	const char *item;
	uint32		len;

	if (PG_ARGISNULL(0))
		PG_RETURN_NULL();
	if (PG_ARGISNULL(1))
		PG_RETURN_DATUM(PG_GETARG_DATUM(0));
	item = tn_item_text(fcinfo, 1, PG_GETARG_DATUM(1), &len);
	st = tn_load_arg(fcinfo, 0);
	tn_add(st, item, len);
	PG_RETURN_BYTEA_P(tn_serialize(st));
}

static void
check_same_capacity(TnState *a, TnState *b)
{
	if (a->k != b->k)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("cannot merge summaries of capacity %u and %u", a->k, b->k)));
}

Datum
kingres_sketch_topn_union(PG_FUNCTION_ARGS)
{
	TnState    *a = tn_load_arg(fcinfo, 0);
	TnState    *b = tn_load_arg(fcinfo, 1);

	check_same_capacity(a, b);
	tn_merge(a, b);
	PG_RETURN_BYTEA_P(tn_serialize(a));
}

/* topn_list(topn, n): the counters by frequency, at most n of them. */
Datum
kingres_sketch_topn_list(PG_FUNCTION_ARGS)
{
	ReturnSetInfo *rsinfo = (ReturnSetInfo *) fcinfo->resultinfo;
	TnState    *st;
	int32	   *idx;
	int32		i;
	int32		limit;

	InitMaterializedSRF(fcinfo, 0);
	if (PG_ARGISNULL(0))
		return (Datum) 0;
	st = tn_load_arg(fcinfo, 0);
	limit = st->n;
	if (!PG_ARGISNULL(1))
	{
		int32		want = PG_GETARG_INT32(1);

		if (want < 0)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("the number of items must not be negative")));
		limit = Min(limit, want);
	}
	idx = (int32 *) palloc((st->n + 1) * sizeof(int32));
	for (i = 0; i < st->n; i++)
		idx[i] = i;
	sort_state = st;
	qsort(idx, st->n, sizeof(int32), cmp_index);
	for (i = 0; i < limit; i++)
	{
		TnCounter  *c = &st->c[idx[i]];
		Datum		values[3];
		bool		nulls[3] = {false, false, false};

		values[0] = PointerGetDatum(cstring_to_text_with_len(c->item, (int) c->len));
		values[1] = Int64GetDatum(c->count);
		values[2] = Int64GetDatum(c->err);
		tuplestore_putvalues(rsinfo->setResult, rsinfo->setDesc, values, nulls);
	}
	return (Datum) 0;
}

Datum
kingres_sketch_topn_total(PG_FUNCTION_ARGS)
{
	TnState    *st = tn_load_arg(fcinfo, 0);

	PG_RETURN_INT64(st->total);
}

Datum
kingres_sketch_topn_capacity(PG_FUNCTION_ARGS)
{
	TnState    *st = tn_load_arg(fcinfo, 0);

	PG_RETURN_INT32((int32) st->k);
}

/* ---- SQL: the aggregates ---- */

static TnState *
agg_state(FunctionCallInfo fcinfo, uint32 k, MemoryContext aggcontext)
{
	TnState    *st;
	MemoryContext old;

	if (!PG_ARGISNULL(0))
	{
		st = (TnState *) PG_GETARG_POINTER(0);
		if (st->k != k)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("the capacity of the aggregate changed from %u to %u", st->k, k),
					 errhint("The capacity must be the same in every row of a group.")));
		return st;
	}
	old = MemoryContextSwitchTo(aggcontext);
	st = tn_new(k, aggcontext);
	MemoryContextSwitchTo(old);
	return st;
}

static void
agg_add_item(FunctionCallInfo fcinfo, TnState *st, MemoryContext aggcontext)
{
	const char *item;
	uint32		len;
	MemoryContext old;

	if (PG_ARGISNULL(1))
		return;
	item = tn_item_text(fcinfo, 1, PG_GETARG_DATUM(1), &len);
	old = MemoryContextSwitchTo(aggcontext);
	tn_add(st, item, len);
	MemoryContextSwitchTo(old);
}

Datum
kingres_sketch_topn_agg_add(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	TnState    *st = agg_state(fcinfo, KS_TOPN_DEFAULT_CAPACITY, aggcontext);

	agg_add_item(fcinfo, st, aggcontext);
	PG_RETURN_POINTER(st);
}

Datum
kingres_sketch_topn_agg_add_k(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	uint32		k;
	TnState    *st;

	if (!PG_ARGISNULL(2))
		k = (uint32) check_capacity(PG_GETARG_INT32(2));
	else if (!PG_ARGISNULL(0))
		k = ((TnState *) PG_GETARG_POINTER(0))->k;
	else
		k = KS_TOPN_DEFAULT_CAPACITY;
	st = agg_state(fcinfo, k, aggcontext);
	agg_add_item(fcinfo, st, aggcontext);
	PG_RETURN_POINTER(st);
}

Datum
kingres_sketch_topn_agg_union(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	TnState    *st = NULL;
	TnState    *other;
	MemoryContext old;

	if (!PG_ARGISNULL(0))
		st = (TnState *) PG_GETARG_POINTER(0);
	if (PG_ARGISNULL(1))
		KS_RETURN_STATE(st);

	other = tn_load_arg(fcinfo, 1);
	if (st == NULL)
		st = agg_state(fcinfo, other->k, aggcontext);
	check_same_capacity(st, other);
	old = MemoryContextSwitchTo(aggcontext);
	tn_merge(st, other);
	MemoryContextSwitchTo(old);
	tn_free(other);
	PG_RETURN_POINTER(st);
}

Datum
kingres_sketch_topn_agg_combine(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	TnState    *a = PG_ARGISNULL(0) ? NULL : (TnState *) PG_GETARG_POINTER(0);
	TnState    *b = PG_ARGISNULL(1) ? NULL : (TnState *) PG_GETARG_POINTER(1);
	MemoryContext old;

	if (b == NULL)
		KS_RETURN_STATE(a);
	old = MemoryContextSwitchTo(aggcontext);
	if (a == NULL)
		a = tn_copy(b);
	else
	{
		check_same_capacity(a, b);
		tn_merge(a, b);
	}
	MemoryContextSwitchTo(old);
	PG_RETURN_POINTER(a);
}

Datum
kingres_sketch_topn_agg_serial(PG_FUNCTION_ARGS)
{
	PG_RETURN_BYTEA_P(tn_serialize((TnState *) PG_GETARG_POINTER(0)));
}

Datum
kingres_sketch_topn_agg_deserial(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext = ks_agg_context(fcinfo);
	const uint8 *p;
	Size		len;
	char		why[KS_WHY_LEN];
	MemoryContext old;
	TnState    *st;

	p = ks_arg_bytes(fcinfo, 0, &len);
	if (len > ks_max_value_bytes(KS_KIND_TOPN))
		ks_invalid_value("topn", "the value is longer than any valid value of the type");
	old = MemoryContextSwitchTo(aggcontext);
	st = tn_load(p, len, why);
	MemoryContextSwitchTo(old);
	if (st == NULL)
		ks_invalid_value("topn", why);
	PG_RETURN_POINTER(st);
}

Datum
kingres_sketch_topn_agg_final(PG_FUNCTION_ARGS)
{
	TnState    *st;

	if (PG_ARGISNULL(0))
		st = tn_new(KS_TOPN_DEFAULT_CAPACITY, CurrentMemoryContext);
	else
		st = (TnState *) PG_GETARG_POINTER(0);
	PG_RETURN_BYTEA_P(tn_serialize(st));
}
