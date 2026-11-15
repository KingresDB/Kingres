/*-------------------------------------------------------------------------
 *
 * kc_encode.c
 *	  Column chunks: the encoding of the values of one column of one chunk
 *	  group, the codecs (none, pglz, lz4, zstd), the minimum and maximum of
 *	  a chunk, and the validated decoding of a chunk read from disk.
 *
 * A chunk before compression is: a null bitmap when any value is null (bit
 * set means "not null"), then the non-null values densely, in the byte order
 * of the server.  Fixed-length values are their bytes; variable-length values
 * are an array of 32-bit lengths followed by the bytes without the varlena
 * header, which the reader rebuilds with a four-byte header.  The whole
 * buffer is compressed as one unit; when that does not make it smaller, the
 * chunk is kept uncompressed.
 *
 * Everything the decoder reads comes from disk, so every length, count and
 * size is checked before it is used, and a mismatch is reported as
 * ERRCODE_DATA_CORRUPTED (README, "Reading corrupt files").
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kc_encode.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/detoast.h"
#include "catalog/pg_collation.h"
#include "catalog/pg_type.h"
#include "common/pg_lzcompress.h"
#include "lib/stringinfo.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/pg_locale.h"
#include "utils/typcache.h"

#ifdef USE_LZ4
#include <lz4.h>
#endif
#ifdef USE_ZSTD
#include <zstd.h>
#endif

#include "kingres_columnar.h"

/* ------------------------------------------------------------------------
 * Codecs
 * ------------------------------------------------------------------------
 */

bool
kc_codec_available(int codec)
{
	switch (codec)
	{
		case KC_CODEC_NONE:
		case KC_CODEC_PGLZ:
			return true;
		case KC_CODEC_LZ4:
#ifdef USE_LZ4
			return true;
#else
			return false;
#endif
		case KC_CODEC_ZSTD:
#ifdef USE_ZSTD
			return true;
#else
			return false;
#endif
		default:
			return false;
	}
}

const char *
kc_codec_name(int codec)
{
	switch (codec)
	{
		case KC_CODEC_NONE:
			return "none";
		case KC_CODEC_PGLZ:
			return "pglz";
		case KC_CODEC_LZ4:
			return "lz4";
		case KC_CODEC_ZSTD:
			return "zstd";
		case KC_CODEC_AUTO:
			return "auto";
	}
	return "unknown";
}

int
kc_codec_from_name(const char *name)
{
	if (pg_strcasecmp(name, "none") == 0)
		return KC_CODEC_NONE;
	if (pg_strcasecmp(name, "pglz") == 0)
		return KC_CODEC_PGLZ;
	if (pg_strcasecmp(name, "lz4") == 0)
		return KC_CODEC_LZ4;
	if (pg_strcasecmp(name, "zstd") == 0)
		return KC_CODEC_ZSTD;
	if (pg_strcasecmp(name, "auto") == 0)
		return KC_CODEC_AUTO;
	return -1;
}

/* The codec "auto" stands for: zstd if built in, else lz4, else pglz. */
int
kc_default_codec(void)
{
	if (kc_codec_available(KC_CODEC_ZSTD))
		return KC_CODEC_ZSTD;
	if (kc_codec_available(KC_CODEC_LZ4))
		return KC_CODEC_LZ4;
	return KC_CODEC_PGLZ;
}

/*
 * Compress src into a new buffer.  Returns the buffer and its length in
 * *clen, or NULL when the codec failed or the result is not smaller than the
 * input (the caller then stores the chunk with codec none).
 */
static char *
kc_compress(int codec, int level, const char *src, uint32 slen, uint32 *clen)
{
	char	   *dst = NULL;
	int64		n = -1;

	if (slen < 16)
		return NULL;

	switch (codec)
	{
		case KC_CODEC_PGLZ:
			{
				/* pglz works on int32 sizes; PGLZ_MAX_OUTPUT adds a margin */
				if (slen > (uint32) (INT_MAX / 2))
					return NULL;
				dst = MemoryContextAllocHuge(CurrentMemoryContext,
											 (Size) PGLZ_MAX_OUTPUT(slen));
				n = pglz_compress(src, (int32) slen, dst,
								  PGLZ_strategy_always);
				break;
			}
#ifdef USE_LZ4
		case KC_CODEC_LZ4:
			{
				int			bound;

				if (slen > (uint32) LZ4_MAX_INPUT_SIZE)
					return NULL;
				bound = LZ4_compressBound((int) slen);
				dst = MemoryContextAllocHuge(CurrentMemoryContext, bound);
				n = LZ4_compress_default(src, dst, (int) slen, bound);
				if (n <= 0)
					n = -1;
				break;
			}
#endif
#ifdef USE_ZSTD
		case KC_CODEC_ZSTD:
			{
				size_t		bound = ZSTD_compressBound(slen);
				size_t		r;

				dst = MemoryContextAllocHuge(CurrentMemoryContext, bound);
				r = ZSTD_compress(dst, bound, src, slen, level);
				n = ZSTD_isError(r) ? -1 : (int64) r;
				break;
			}
#endif
		default:
			return NULL;
	}

	if (n < 0 || n >= (int64) slen)
	{
		if (dst)
			pfree(dst);
		return NULL;
	}
	*clen = (uint32) n;
	return dst;
}

/* Decompress into dst of exactly rawlen bytes; false if anything is off. */
static bool
kc_decompress(int codec, const char *src, uint32 clen, char *dst, uint32 rawlen)
{
	switch (codec)
	{
		case KC_CODEC_PGLZ:
			if (clen > (uint32) INT_MAX || rawlen > (uint32) INT_MAX)
				return false;
			return pglz_decompress(src, (int32) clen, dst, (int32) rawlen,
								   true) == (int32) rawlen;
#ifdef USE_LZ4
		case KC_CODEC_LZ4:
			if (clen > (uint32) INT_MAX || rawlen > (uint32) INT_MAX)
				return false;
			return LZ4_decompress_safe(src, dst, (int) clen,
									   (int) rawlen) == (int) rawlen;
#endif
#ifdef USE_ZSTD
		case KC_CODEC_ZSTD:
			{
				size_t		r = ZSTD_decompress(dst, rawlen, src, clen);

				return !ZSTD_isError(r) && r == rawlen;
			}
#endif
		default:
			return false;
	}
}

/* ------------------------------------------------------------------------
 * Column descriptions
 * ------------------------------------------------------------------------
 */

/*
 * Is the ordering of this collation independent of any library?  Only then a
 * minimum and a maximum stored in a file stay valid when libc or ICU change.
 */
static bool
kc_collation_is_c(Oid collid)
{
	pg_locale_t locale;

	if (collid == C_COLLATION_OID)
		return true;
	if (!OidIsValid(collid))
		return false;
	locale = pg_newlocale_from_collation(collid);
	return locale != NULL && locale->collate_is_c;
}

KcTypeInfo *
kc_typeinfo_build(TupleDesc tupdesc, MemoryContext cxt)
{
	KcTypeInfo *ti;
	MemoryContext old = MemoryContextSwitchTo(cxt);
	int			i;

	ti = palloc0(offsetof(KcTypeInfo, cols) + sizeof(KcColInfo) * tupdesc->natts);
	ti->natts = tupdesc->natts;

	for (i = 0; i < tupdesc->natts; i++)
	{
		Form_pg_attribute att = TupleDescAttr(tupdesc, i);
		KcColInfo  *ci = &ti->cols[i];

		ci->typid = att->atttypid;
		ci->collation = att->attcollation;
		ci->typlen = att->attlen;
		ci->byval = att->attbyval;
		ci->align = att->attalign;
		ci->dropped = att->attisdropped;
		ci->minmax_ok = false;

		if (ci->dropped)
			continue;

		/*
		 * Statistics only for plain base types, so that the comparison is
		 * the one of a btree index and cannot depend on a sub-type's
		 * collation, an enum's catalog order or a domain's constraint.
		 */
		if (get_typtype(att->atttypid) == TYPTYPE_BASE &&
			!OidIsValid(get_element_type(att->atttypid)) &&
			att->atttypid != RECORDOID)
		{
			TypeCacheEntry *tce;

			tce = lookup_type_cache(att->atttypid, TYPECACHE_CMP_PROC_FINFO | TYPECACHE_BTREE_OPFAMILY);
			if (OidIsValid(tce->cmp_proc) &&
				(!OidIsValid(att->attcollation) ||
				 kc_collation_is_c(att->attcollation)))
			{
				fmgr_info_copy(&ci->cmp, &tce->cmp_proc_finfo, cxt);
				ci->opfamily = tce->btree_opf;
				ci->minmax_ok = true;
			}
		}
	}
	MemoryContextSwitchTo(old);
	return ti;
}

int
kc_stat_compare(const KcColInfo *ci, Datum a, Datum b)
{
	return DatumGetInt32(FunctionCall2Coll((FmgrInfo *) &ci->cmp,
										   ci->collation, a, b));
}

/* The stored form of a minimum or maximum, back to a datum (palloc'd). */
Datum
kc_stat_datum(const KcColInfo *ci, const char *bytes, int len)
{
	if (ci->byval)
	{
		if (len != ci->typlen)
			ereport(ERROR,
					(errcode(ERRCODE_DATA_CORRUPTED),
					 errmsg("columnar statistics are corrupt: bad length %d for a by-value column", len)));
		switch (ci->typlen)
		{
			case 1:
				return CharGetDatum(*(const char *) bytes);
			case 2:
				{
					int16		v;

					memcpy(&v, bytes, sizeof(v));
					return Int16GetDatum(v);
				}
			case 4:
				{
					int32		v;

					memcpy(&v, bytes, sizeof(v));
					return Int32GetDatum(v);
				}
			default:
				{
					int64		v;

					memcpy(&v, bytes, sizeof(v));
					return Int64GetDatum(v);
				}
		}
	}
	else
	{
		char	   *p;

		if (ci->typlen > 0)
		{
			if (len != ci->typlen)
				ereport(ERROR,
						(errcode(ERRCODE_DATA_CORRUPTED),
						 errmsg("columnar statistics are corrupt: bad length %d for a fixed-length column", len)));
		}
		else
		{
			/*
			 * Only a plain value: an external, indirect or compressed header
			 * would be followed by the type's comparison function.
			 */
			if (len < 1 || VARATT_IS_EXTERNAL(bytes) || VARATT_IS_COMPRESSED(bytes) ||
				(!VARATT_IS_SHORT(bytes) && len < VARHDRSZ) ||
				(Size) len != VARSIZE_ANY(bytes))
				ereport(ERROR,
						(errcode(ERRCODE_DATA_CORRUPTED),
						 errmsg("columnar statistics are corrupt: bad variable-length value")));
		}
		p = palloc(len);
		memcpy(p, bytes, len);
		return PointerGetDatum(p);
	}
}

/* ------------------------------------------------------------------------
 * Encoding
 * ------------------------------------------------------------------------
 */

static inline void
kc_put_byval(char *dst, Datum v, int len)
{
	switch (len)
	{
		case 1:
			{
				char		c = DatumGetChar(v);

				memcpy(dst, &c, 1);
				break;
			}
		case 2:
			{
				int16		x = DatumGetInt16(v);

				memcpy(dst, &x, 2);
				break;
			}
		case 4:
			{
				int32		x = DatumGetInt32(v);

				memcpy(dst, &x, 4);
				break;
			}
		default:
			{
				int64		x = DatumGetInt64(v);

				memcpy(dst, &x, 8);
				break;
			}
	}
}

/* The bytes of a minimum or maximum, and their count. */
static inline Size
kc_stat_size(const KcColInfo *ci, Datum v)
{
	if (ci->byval || ci->typlen > 0)
		return ci->typlen;
	return VARSIZE_ANY(DatumGetPointer(v));
}

static char *
kc_stat_bytes(const KcColInfo *ci, Datum v, Size len)
{
	char	   *p = palloc(len);

	if (ci->byval)
		kc_put_byval(p, v, ci->typlen);
	else
		memcpy(p, DatumGetPointer(v), len);
	return p;
}

/*
 * Encode and compress one column chunk of nrows rows.  vals and nulls are
 * the first entry of the column in row-major arrays with the given stride.
 * Variable-length values must already be detoasted (packed form is fine).
 * The compressed bytes are appended to out; meta describes them (the caller
 * sets meta->off).  *minbytes and *maxbytes are palloc'd serialized values,
 * or NULL when the chunk has no statistics.
 */
void
kc_encode_chunk(const KcColInfo *ci, const Datum *vals, const bool *nulls,
				int stride, uint32 nrows, int codec, int level,
				StringInfo out, KcChunkDisk *meta, char **minbytes,
				char **maxbytes)
{
	uint32		i;
	uint32		nnulls = 0;
	uint64		rawsz;
	uint32		nonnull;
	char	   *raw;
	char	   *p;
	char	   *comp;
	uint32		clen = 0;
	bool		have_mm = false;
	Datum		mn = 0,
				mx = 0;
	char	   *mnb = NULL,
			   *mxb = NULL;
	Size		mnl = 0,
				mxl = 0;

	for (i = 0; i < nrows; i++)
		if (nulls[(Size) i * stride])
			nnulls++;
	nonnull = nrows - nnulls;

	/* the size of the raw buffer */
	rawsz = (nnulls > 0) ? (nrows + 7) / 8 : 0;
	if (ci->byval || ci->typlen > 0)
		rawsz += (uint64) nonnull * ci->typlen;
	else
	{
		rawsz += (uint64) nonnull * sizeof(uint32);
		for (i = 0; i < nrows; i++)
			if (!nulls[(Size) i * stride])
				rawsz += VARSIZE_ANY_EXHDR(DatumGetPointer(vals[(Size) i * stride]));
	}
	if (rawsz > (uint64) PG_UINT32_MAX)
		ereport(ERROR,
				(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
				 errmsg("columnar chunk of %llu bytes is too large",
						(unsigned long long) rawsz)));

	raw = MemoryContextAllocHuge(CurrentMemoryContext, rawsz > 0 ? rawsz : 1);
	p = raw;

	if (nnulls > 0)
	{
		memset(p, 0, (nrows + 7) / 8);
		for (i = 0; i < nrows; i++)
			if (!nulls[(Size) i * stride])
				p[i / 8] |= (1 << (i % 8));
		p += (nrows + 7) / 8;
	}

	if (ci->byval)
	{
		for (i = 0; i < nrows; i++)
		{
			if (nulls[(Size) i * stride])
				continue;
			kc_put_byval(p, vals[(Size) i * stride], ci->typlen);
			p += ci->typlen;
		}
	}
	else if (ci->typlen > 0)
	{
		for (i = 0; i < nrows; i++)
		{
			if (nulls[(Size) i * stride])
				continue;
			memcpy(p, DatumGetPointer(vals[(Size) i * stride]), ci->typlen);
			p += ci->typlen;
		}
	}
	else
	{
		char	   *lens = p;
		char	   *data = p + (Size) nonnull * sizeof(uint32);
		uint32		k = 0;

		for (i = 0; i < nrows; i++)
		{
			struct varlena *v;
			uint32		l;

			if (nulls[(Size) i * stride])
				continue;
			v = (struct varlena *) DatumGetPointer(vals[(Size) i * stride]);
			l = VARSIZE_ANY_EXHDR(v);
			memcpy(lens + (Size) k * sizeof(uint32), &l, sizeof(uint32));
			memcpy(data, VARDATA_ANY(v), l);
			data += l;
			k++;
		}
		p = data;
	}
	Assert((uint64) (p - raw) == rawsz);

	/* minimum and maximum */
	if (ci->minmax_ok && nonnull > 0)
	{
		bool		first = true;

		for (i = 0; i < nrows; i++)
		{
			Datum		v;

			if (nulls[(Size) i * stride])
				continue;
			v = vals[(Size) i * stride];
			if (first)
			{
				mn = mx = v;
				first = false;
			}
			else
			{
				if (kc_stat_compare(ci, v, mn) < 0)
					mn = v;
				if (kc_stat_compare(ci, v, mx) > 0)
					mx = v;
			}
		}
		mnl = kc_stat_size(ci, mn);
		mxl = kc_stat_size(ci, mx);
		if (mnl <= KC_MAX_MINMAX_BYTES && mxl <= KC_MAX_MINMAX_BYTES)
		{
			mnb = kc_stat_bytes(ci, mn, mnl);
			mxb = kc_stat_bytes(ci, mx, mxl);
			have_mm = true;
		}
	}

	memset(meta, 0, sizeof(*meta));
	meta->raw_len = (uint32) rawsz;
	meta->nnulls = nnulls;
	meta->flags = (nnulls > 0 ? KC_CHUNK_HAS_NULLS : 0) |
		(have_mm ? KC_CHUNK_HAS_MINMAX : 0);
	meta->min_len = have_mm ? (uint16) mnl : 0;
	meta->max_len = have_mm ? (uint16) mxl : 0;

	comp = (codec != KC_CODEC_NONE) ?
		kc_compress(codec, level, raw, (uint32) rawsz, &clen) : NULL;
	if (comp)
	{
		meta->codec = (uint8) codec;
		meta->comp_len = clen;
		appendBinaryStringInfo(out, comp, clen);
		pfree(comp);
	}
	else
	{
		meta->codec = KC_CODEC_NONE;
		meta->comp_len = (uint32) rawsz;
		appendBinaryStringInfo(out, raw, rawsz);
	}
	pfree(raw);

	*minbytes = mnb;
	*maxbytes = mxb;
}

/* ------------------------------------------------------------------------
 * Decoding
 * ------------------------------------------------------------------------
 */

static pg_noreturn void
kc_chunk_corrupt(const char *relname, uint64 stripe_id, const char *what)
{
	ereport(ERROR,
			(errcode(ERRCODE_DATA_CORRUPTED),
			 errmsg("columnar table \"%s\" is corrupt: stripe " UINT64_FORMAT ": %s",
					relname, stripe_id, what)));
}

/*
 * Decode one chunk into vals/nulls (nrows entries each).  Memory for by-
 * reference values is allocated in cxt, which the caller resets per group.
 * comp points at meta->comp_len bytes already read from the stripe.
 */
void
kc_decode_chunk(const KcColInfo *ci, const char *comp, const KcChunkDisk *meta,
				uint32 nrows, Datum *vals, bool *nulls, MemoryContext cxt,
				const char *relname, uint64 stripe_id)
{
	MemoryContext old;
	const char *raw;
	char	   *rawbuf = NULL;
	uint32		rawlen = meta->raw_len;
	uint32		nonnull;
	const unsigned char *bitmap = NULL;
	uint64		bmlen = 0;
	uint32		i;
	const char *p;
	const char *end;

	if (!kc_codec_available(meta->codec))
	{
		if (meta->codec > KC_CODEC_ZSTD)
			kc_chunk_corrupt(relname, stripe_id, "unknown codec id");
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("columnar table \"%s\" uses the codec \"%s\", which this server was built without",
						relname, kc_codec_name(meta->codec))));
	}
	if ((meta->flags & ~(KC_CHUNK_HAS_NULLS | KC_CHUNK_HAS_MINMAX)) != 0)
		kc_chunk_corrupt(relname, stripe_id, "unknown chunk flags");
	if (meta->nnulls > nrows)
		kc_chunk_corrupt(relname, stripe_id, "more nulls than rows in a chunk");
	if (((meta->flags & KC_CHUNK_HAS_NULLS) != 0) != (meta->nnulls > 0))
		kc_chunk_corrupt(relname, stripe_id, "null flag and null count disagree");
	nonnull = nrows - meta->nnulls;

	/*
	 * Bound the size the chunk claims before anything is allocated for it: a
	 * fixed-width column has exactly one size; a compressed stream cannot
	 * expand by more than its codec allows.
	 */
	if (ci->typlen > 0)
	{
		uint64		maxraw = ((uint64) nrows + 7) / 8 + (uint64) nonnull * ci->typlen;

		if (rawlen > maxraw)
			kc_chunk_corrupt(relname, stripe_id, "chunk is larger than its rows can be");
	}
	if (meta->codec == KC_CODEC_PGLZ && (uint64) rawlen > (uint64) meta->comp_len * 300 + 1024)
		kc_chunk_corrupt(relname, stripe_id, "chunk expands more than the codec allows");
	if (meta->codec == KC_CODEC_LZ4 && (uint64) rawlen > (uint64) meta->comp_len * 260 + 1024)
		kc_chunk_corrupt(relname, stripe_id, "chunk expands more than the codec allows");
#ifdef USE_ZSTD
	if (meta->codec == KC_CODEC_ZSTD)
	{
		unsigned long long fs = ZSTD_getFrameContentSize(comp, meta->comp_len);

		if (fs != (unsigned long long) rawlen)
			kc_chunk_corrupt(relname, stripe_id, "chunk frame size disagrees with its recorded size");
	}
#endif

	old = MemoryContextSwitchTo(cxt);

	if (meta->codec == KC_CODEC_NONE)
	{
		if (meta->comp_len != rawlen)
			kc_chunk_corrupt(relname, stripe_id, "uncompressed chunk length mismatch");
		raw = comp;
	}
	else
	{
		rawbuf = MemoryContextAllocHuge(cxt, rawlen > 0 ? rawlen : 1);
		if (!kc_decompress(meta->codec, comp, meta->comp_len, rawbuf, rawlen))
			kc_chunk_corrupt(relname, stripe_id, "chunk does not decompress to its recorded size");
		raw = rawbuf;
	}
	p = raw;
	end = raw + rawlen;

	if (meta->nnulls > 0)
	{
		bmlen = ((uint64) nrows + 7) / 8;
		if (bmlen > rawlen)
			kc_chunk_corrupt(relname, stripe_id, "null bitmap does not fit in the chunk");
		bitmap = (const unsigned char *) p;
		p += bmlen;
		/* the bitmap must agree with the null count */
		{
			uint32		set = 0;

			for (i = 0; i < nrows; i++)
				if (bitmap[i / 8] & (1 << (i % 8)))
					set++;
			if (set != nonnull)
				kc_chunk_corrupt(relname, stripe_id, "null bitmap disagrees with the null count");
		}
	}

	if (ci->byval)
	{
		if ((uint64) nonnull * ci->typlen != (uint64) (end - p))
			kc_chunk_corrupt(relname, stripe_id, "value bytes do not match the row count");
		for (i = 0; i < nrows; i++)
		{
			if (bitmap && !(bitmap[i / 8] & (1 << (i % 8))))
			{
				nulls[i] = true;
				vals[i] = (Datum) 0;
				continue;
			}
			nulls[i] = false;
			switch (ci->typlen)
			{
				case 1:
					vals[i] = CharGetDatum(*p);
					break;
				case 2:
					{
						int16		v;

						memcpy(&v, p, 2);
						vals[i] = Int16GetDatum(v);
						break;
					}
				case 4:
					{
						int32		v;

						memcpy(&v, p, 4);
						vals[i] = Int32GetDatum(v);
						break;
					}
				default:
					{
						int64		v;

						memcpy(&v, p, 8);
						vals[i] = Int64GetDatum(v);
						break;
					}
			}
			p += ci->typlen;
		}
	}
	else if (ci->typlen > 0)
	{
		Size		stride = MAXALIGN(ci->typlen);
		char	   *area;

		if ((uint64) nonnull * ci->typlen != (uint64) (end - p))
			kc_chunk_corrupt(relname, stripe_id, "value bytes do not match the row count");
		area = MemoryContextAllocHuge(cxt, (Size) nonnull * stride + 1);
		for (i = 0; i < nrows; i++)
		{
			if (bitmap && !(bitmap[i / 8] & (1 << (i % 8))))
			{
				nulls[i] = true;
				vals[i] = (Datum) 0;
				continue;
			}
			nulls[i] = false;
			memcpy(area, p, ci->typlen);
			vals[i] = PointerGetDatum(area);
			area += stride;
			p += ci->typlen;
		}
	}
	else if (ci->typlen == -1)
	{
		const char *lens = p;
		const char *data;
		uint64		total = 0;
		uint32		k;
		char	   *area;

		if ((uint64) nonnull * sizeof(uint32) > (uint64) (end - p))
			kc_chunk_corrupt(relname, stripe_id, "length array does not fit in the chunk");
		data = p + (Size) nonnull * sizeof(uint32);

		/* the sum of the lengths must be exactly what is left */
		for (k = 0; k < nonnull; k++)
		{
			uint32		l;

			memcpy(&l, lens + (Size) k * sizeof(uint32), sizeof(uint32));
			if (l > MaxAllocSize - VARHDRSZ - 8)
				kc_chunk_corrupt(relname, stripe_id, "value length exceeds the limit");
			total += l;
		}
		if (total != (uint64) (end - data))
			kc_chunk_corrupt(relname, stripe_id, "value lengths do not match the chunk size");

		area = MemoryContextAllocHuge(cxt, (Size) total + (Size) nonnull * (VARHDRSZ + MAXIMUM_ALIGNOF) + 1);
		k = 0;
		for (i = 0; i < nrows; i++)
		{
			uint32		l;

			if (bitmap && !(bitmap[i / 8] & (1 << (i % 8))))
			{
				nulls[i] = true;
				vals[i] = (Datum) 0;
				continue;
			}
			nulls[i] = false;
			memcpy(&l, lens + (Size) k * sizeof(uint32), sizeof(uint32));
			k++;
			area = (char *) MAXALIGN(area);
			SET_VARSIZE(area, l + VARHDRSZ);
			memcpy(VARDATA(area), data, l);
			data += l;
			vals[i] = PointerGetDatum(area);
			area += l + VARHDRSZ;
		}
	}
	else
		kc_chunk_corrupt(relname, stripe_id, "column type has an unsupported length");

	if (rawbuf)
		pfree(rawbuf);
	MemoryContextSwitchTo(old);
}
