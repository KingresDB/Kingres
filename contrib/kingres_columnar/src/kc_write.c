/*-------------------------------------------------------------------------
 *
 * kc_write.c
 *	  The write path: row buffers, the construction of a stripe from them, the
 *	  flush, and the transaction callbacks that decide when a flush happens.
 *
 * Rows of INSERT and COPY are appended to a buffer of raw rows kept per
 * relation and per subtransaction.  A buffer becomes a stripe when it holds
 * a full stripe of rows, when the backend's buffers exceed their memory
 * allowance, or when the transaction commits or prepares.  A rollback just
 * forgets the buffers: nothing was written.  A flush writes the pages of the
 * stripe (logged as whole-page images) and then inserts the stripe's catalog
 * row; the row, written by the transaction that wrote the rows, is what makes
 * them visible or not.
 *
 * Subtransactions: a buffer belongs to the subtransaction that wrote it and
 * can only be flushed by it, because the catalog row takes the transaction id
 * of whoever inserts it and must disappear with the subtransaction that owns
 * the data.  When a subtransaction commits its buffers are handed to its
 * parent; when it aborts they are dropped.  The memory allowance is
 * therefore a soft limit while rows of an outer level are buffered.
 *
 * A scan of a relation that has buffered rows reads them directly (see
 * kc_scan.c), so a scan never flushes.  While a scan reads a relation's
 * buffers they are pinned and not flushed or reset.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kc_write.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/detoast.h"
#include "access/htup_details.h"
#include "access/relation.h"
#include "access/table.h"
#include "access/xact.h"
#include "catalog/storage.h"
#include "miscadmin.h"
#include "pgstat.h"
#include "storage/predicate.h"
#include "utils/hsearch.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"

#include "kingres_columnar.h"

typedef struct KcWriteBuf
{
	uint32		serial;
	Oid			relid;
	SubTransactionId subid;
	uint64		uid;			/* identity of the file the rows were written for */
	MemoryContext cxt;
	int			ncols;
	uint32		nrows;
	uint32		cap;
	Datum	   *vals;
	bool	   *nulls;
	CommandId  *cids;
	Size		bytes;
	KcOptions	opts;
} KcWriteBuf;

typedef struct KcRelState
{
	Oid			relid;			/* hash key */
	int			pins;
	List	   *bufs;			/* KcWriteBuf *, outermost first */
} KcRelState;

static MemoryContext kc_write_cxt = NULL;
static HTAB *kc_states = NULL;
static Size kc_total_bytes = 0;
static uint32 kc_next_serial = 1;

/* a growable buffer that may exceed 1 GB */
typedef struct KcBuf
{
	char	   *data;
	uint64		len;
	uint64		cap;
} KcBuf;

static void
kc_buf_append(KcBuf *b, const void *src, uint64 n)
{
	if (b->len + n > b->cap)
	{
		uint64		ncap = b->cap ? b->cap : 8192;

		while (ncap < b->len + n)
			ncap *= 2;
		if (ncap > (uint64) MaxAllocHugeSize)
			ereport(ERROR,
					(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
					 errmsg("columnar stripe is too large")));
		b->data = b->data ? repalloc_huge(b->data, ncap)
			: MemoryContextAllocHuge(CurrentMemoryContext, ncap);
		b->cap = ncap;
	}
	if (n > 0)
		memcpy(b->data + b->len, src, n);
	b->len += n;
}

/* ------------------------------------------------------------------------
 * State
 * ------------------------------------------------------------------------
 */

static void
kc_state_init(void)
{
	HASHCTL		ctl;

	if (kc_states != NULL)
		return;
	kc_write_cxt = AllocSetContextCreate(TopMemoryContext, "kingres_columnar write buffers",
										 ALLOCSET_DEFAULT_SIZES);
	memset(&ctl, 0, sizeof(ctl));
	ctl.keysize = sizeof(Oid);
	ctl.entrysize = sizeof(KcRelState);
	ctl.hcxt = kc_write_cxt;
	kc_states = hash_create("kingres_columnar relation states", 16, &ctl,
							HASH_ELEM | HASH_BLOBS | HASH_CONTEXT);
	kc_total_bytes = 0;
}

static void
kc_state_reset(void)
{
	if (kc_write_cxt)
		MemoryContextDelete(kc_write_cxt);
	kc_write_cxt = NULL;
	kc_states = NULL;
	kc_total_bytes = 0;
}

static KcRelState *
kc_state_get(Oid relid, bool create)
{
	KcRelState *st;
	bool		found;

	if (kc_states == NULL)
	{
		if (!create)
			return NULL;
		kc_state_init();
	}
	st = hash_search(kc_states, &relid, create ? HASH_ENTER : HASH_FIND, &found);
	if (st && !found)
	{
		st->pins = 0;
		st->bufs = NIL;
	}
	return st;
}

static KcWriteBuf *
kc_buf_create(KcRelState *st, Relation rel, int ncols)
{
	KcWriteBuf *b;
	MemoryContext cxt;
	KcMetaData meta;

	cxt = AllocSetContextCreate(kc_write_cxt, "kingres_columnar rows",
								ALLOCSET_DEFAULT_SIZES);
	b = MemoryContextAllocZero(kc_write_cxt, sizeof(KcWriteBuf));
	b->serial = kc_next_serial++;
	b->relid = RelationGetRelid(rel);
	b->subid = GetCurrentSubTransactionId();
	b->uid = kc_rel_ident(rel)->uid;
	b->cxt = cxt;
	b->ncols = ncols;

	kc_meta_read(rel, &meta);
	b->opts.codec = meta.codec;
	b->opts.level = meta.level;
	b->opts.stripe_rows = meta.stripe_rows;
	b->opts.chunk_rows = meta.chunk_rows;

	{
		MemoryContext old = MemoryContextSwitchTo(kc_write_cxt);

		st->bufs = lappend(st->bufs, b);
		MemoryContextSwitchTo(old);
	}
	return b;
}

static void
kc_buf_clear(KcWriteBuf *b)
{
	kc_total_bytes -= Min(kc_total_bytes, b->bytes);
	MemoryContextReset(b->cxt);
	b->nrows = 0;
	b->cap = 0;
	b->vals = NULL;
	b->nulls = NULL;
	b->cids = NULL;
	b->bytes = 0;
}

/* ------------------------------------------------------------------------
 * Appending rows
 * ------------------------------------------------------------------------
 */

static void
kc_buf_reserve(KcWriteBuf *b, uint32 extra)
{
	uint32		need = b->nrows + extra;

	if (need <= b->cap)
		return;
	{
		uint32		ncap = b->cap ? b->cap : 1024;
		MemoryContext old = MemoryContextSwitchTo(b->cxt);

		while (ncap < need)
			ncap *= 2;
		if (b->vals == NULL)
		{
			b->vals = palloc_array(Datum, (Size) ncap * b->ncols);
			b->nulls = palloc_array(bool, (Size) ncap * b->ncols);
			b->cids = palloc_array(CommandId, ncap);
		}
		else
		{
			b->vals = repalloc(b->vals, sizeof(Datum) * (Size) ncap * b->ncols);
			b->nulls = repalloc(b->nulls, (Size) ncap * b->ncols);
			b->cids = repalloc(b->cids, sizeof(CommandId) * ncap);
		}
		b->cap = ncap;
		MemoryContextSwitchTo(old);
	}
}

/* Copy one value into the buffer's memory; returns the datum and its size. */
static Datum
kc_copy_value(KcWriteBuf *b, Form_pg_attribute att, Datum v, Size *sz)
{
	MemoryContext old;
	Datum		res;

	if (att->attbyval)
	{
		*sz = 0;
		return v;
	}
	old = MemoryContextSwitchTo(b->cxt);
	if (att->attlen > 0)
	{
		char	   *p = palloc(att->attlen);

		memcpy(p, DatumGetPointer(v), att->attlen);
		res = PointerGetDatum(p);
		*sz = att->attlen;
	}
	else
	{
		struct varlena *src = (struct varlena *) DatumGetPointer(v);
		struct varlena *flat = pg_detoast_datum_packed(src);
		Size		len = VARSIZE_ANY(flat);
		char	   *p = palloc(len);

		memcpy(p, flat, len);
		if (flat != src)
			pfree(flat);
		res = PointerGetDatum(p);
		*sz = len;
	}
	MemoryContextSwitchTo(old);
	return res;
}

/*
 * The table got more columns since the buffer was started (ADD COLUMN in the
 * same transaction): give the buffered rows the value the new columns have
 * for rows that predate them, as the readers do for older stripes.
 */
static void
kc_buf_widen(KcWriteBuf *b, Relation rel)
{
	TupleDesc	desc = RelationGetDescr(rel);
	int			old_ncols = b->ncols;
	int			new_ncols = desc->natts;
	MemoryContext old;
	Datum	   *nv;
	bool	   *nn;
	uint32		r;
	int			c;

	if (b->nrows == 0)
	{
		b->ncols = new_ncols;
		b->vals = NULL;
		b->nulls = NULL;
		b->cids = NULL;
		b->cap = 0;
		return;
	}
	old = MemoryContextSwitchTo(b->cxt);
	nv = palloc_array(Datum, (Size) b->cap * new_ncols);
	nn = palloc_array(bool, (Size) b->cap * new_ncols);
	for (r = 0; r < b->nrows; r++)
	{
		memcpy(&nv[(Size) r * new_ncols], &b->vals[(Size) r * old_ncols],
			   sizeof(Datum) * old_ncols);
		memcpy(&nn[(Size) r * new_ncols], &b->nulls[(Size) r * old_ncols],
			   old_ncols);
		for (c = old_ncols; c < new_ncols; c++)
		{
			Form_pg_attribute att = TupleDescAttr(desc, c);
			bool		isnull;
			Datum		d = getmissingattr(desc, c + 1, &isnull);
			Size		sz = 0;

			if (att->attisdropped)
				isnull = true;
			if (!isnull)
				d = kc_copy_value(b, att, d, &sz);
			nv[(Size) r * new_ncols + c] = isnull ? (Datum) 0 : d;
			nn[(Size) r * new_ncols + c] = isnull;
		}
	}
	b->vals = nv;
	b->nulls = nn;
	b->ncols = new_ncols;
	MemoryContextSwitchTo(old);
}

/* Is a buffer one that the running subtransaction may flush now? */
static bool
kc_buf_flushable(KcRelState *st, KcWriteBuf *b)
{
	return st->pins == 0 && b->subid == GetCurrentSubTransactionId();
}

static void kc_flush_buf(KcRelState *st, KcWriteBuf *b, Relation rel);

/*
 * Rows buffered for a file the relation no longer has (it was truncated or
 * rewritten after they were written: a rewrite reads them and writes them
 * into the new file, TRUNCATE discards them) are not the table's any more.  They are
 * kept, hidden, until the transaction ends, because a rollback of the
 * subtransaction that replaced the file brings the old file back.
 */
static bool
kc_buf_is_stale(KcWriteBuf *b, Relation rel)
{
	return b->uid != kc_rel_ident(rel)->uid;
}

/* Flush the largest flushable buffer of any relation; false if none. */
static bool
kc_flush_largest(void)
{
	HASH_SEQ_STATUS seq;
	KcRelState *st;
	KcRelState *best_st = NULL;
	KcWriteBuf *best = NULL;
	Relation	rel;

	hash_seq_init(&seq, kc_states);
	while ((st = hash_seq_search(&seq)) != NULL)
	{
		ListCell   *lc;

		foreach(lc, st->bufs)
		{
			KcWriteBuf *b = lfirst(lc);

			if (b->nrows > 0 && kc_buf_flushable(st, b) &&
				(best == NULL || b->bytes > best->bytes))
			{
				best = b;
				best_st = st;
			}
		}
	}
	if (best == NULL)
		return false;
	rel = try_table_open(best->relid, RowExclusiveLock);
	if (rel == NULL)
	{
		kc_buf_clear(best);
		return true;
	}
	kc_flush_buf(best_st, best, rel);
	table_close(rel, NoLock);
	return true;
}

void
kc_write_row_values(Relation rel, Datum *vals, bool *nulls, CommandId cid)
{
	TupleDesc	desc = RelationGetDescr(rel);
	KcRelState *st = kc_state_get(RelationGetRelid(rel), true);
	KcWriteBuf *b = NULL;
	MemoryContext old = CurrentMemoryContext;
	int			c;
	Size		rowbytes = 0;

	/* a write conflicts with transactions that read the table (SSI) */
	CheckForSerializableConflictIn(rel, NULL, InvalidBlockNumber);

	if (st->bufs != NIL)
	{
		b = llast(st->bufs);
		if (b->subid != GetCurrentSubTransactionId() ||
			b->uid != kc_rel_ident(rel)->uid)
			b = NULL;
	}
	if (b == NULL)
		b = kc_buf_create(st, rel, desc->natts);
	else if (b->ncols != desc->natts)
		kc_buf_widen(b, rel);
	MemoryContextSwitchTo(old);

	kc_buf_reserve(b, 1);
	for (c = 0; c < b->ncols; c++)
	{
		Form_pg_attribute att = TupleDescAttr(desc, c);
		Size		idx = (Size) b->nrows * b->ncols + c;

		if (att->attisdropped || nulls[c])
		{
			b->vals[idx] = (Datum) 0;
			b->nulls[idx] = true;
			rowbytes += 1;
			continue;
		}
		{
			Size		sz;

			b->vals[idx] = kc_copy_value(b, att, vals[c], &sz);
			b->nulls[idx] = false;
			rowbytes += sizeof(Datum) + 1 + sz;
		}
	}
	b->cids[b->nrows] = cid;
	b->nrows++;
	b->bytes += rowbytes + sizeof(CommandId);
	kc_total_bytes += rowbytes + sizeof(CommandId);

	if (b->nrows >= b->opts.stripe_rows && kc_buf_flushable(st, b))
	{
		Relation	r = try_table_open(b->relid, RowExclusiveLock);

		if (r != NULL)
		{
			kc_flush_buf(st, b, r);
			table_close(r, NoLock);
		}
	}
	else if (kc_total_bytes > (Size) kc_guc_write_buffer_kb * 1024)
	{
		/* soft limit: when only outer-level or pinned buffers exist, go on */
		(void) kc_flush_largest();
	}
}

void
kc_write_row(Relation rel, TupleTableSlot *slot, CommandId cid)
{
	slot_getallattrs(slot);
	kc_write_row_values(rel, slot->tts_values, slot->tts_isnull, cid);
	pgstat_count_heap_insert(rel, 1);
}

/* ------------------------------------------------------------------------
 * Building and writing a stripe
 * ------------------------------------------------------------------------
 */

/* The size of a row, the way the chunk byte limit counts it. */
static Size
kc_row_bytes(KcWriteBuf *b, const KcTypeInfo *ti, uint32 r)
{
	Size		sz = 0;
	int			c;

	for (c = 0; c < b->ncols; c++)
	{
		Size		idx = (Size) r * b->ncols + c;

		if (b->nulls[idx])
			continue;
		if (ti->cols[c].typlen > 0)
			sz += ti->cols[c].typlen;
		else
			sz += 4 + VARSIZE_ANY_EXHDR(DatumGetPointer(b->vals[idx]));
	}
	return sz;
}

/* per-column running statistics of a whole stripe */
typedef struct KcStripeColStat
{
	uint64		nnulls;
	bool		any_values;		/* some chunk had non-null values */
	bool		all_mm;			/* every such chunk had min and max */
	bool		have;
	Datum		min;
	Datum		max;
} KcStripeColStat;

/*
 * Turn the rows of a buffer into the stripe blob, and compute everything the
 * catalog row needs: the chunk-group count, the raw size, the column
 * statistics and the command-id change points.
 */
static void
kc_build_stripe(Relation rel, KcWriteBuf *b, const KcOptions *opts,
				uint64 stripe_id, KcBuf *blob, StringInfo stats,
				StringInfo cidpts, uint32 *ngroups_out, uint64 *raw_bytes,
				uint64 *footer_off)
{
	MemoryContext tmp = AllocSetContextCreate(CurrentMemoryContext,
											  "kingres_columnar stripe build",
											  ALLOCSET_DEFAULT_SIZES);
	MemoryContext old = MemoryContextSwitchTo(tmp);
	KcTypeInfo *ti = kc_typeinfo_build(RelationGetDescr(rel), tmp);
	KcStripeHeader hdr;
	StringInfoData footer;
	KcStripeColStat *cs;
	uint32		r0 = 0;
	uint32		ngroups = 0;
	int			ncols = b->ncols;
	int			codec = opts->codec;
	int			c;
	uint32		r;

	if (!kc_codec_available(codec))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("the codec \"%s\" of columnar table \"%s\" is not available in this server",
						kc_codec_name(codec), RelationGetRelationName(rel))));

	/* the blob is built in the caller's context so it outlives the temp one */
	memset(&hdr, 0, sizeof(hdr));
	MemoryContextSwitchTo(old);
	kc_buf_append(blob, &hdr, sizeof(hdr));
	MemoryContextSwitchTo(tmp);

	initStringInfo(&footer);
	cs = palloc0(sizeof(KcStripeColStat) * ncols);
	for (c = 0; c < ncols; c++)
		cs[c].all_mm = true;
	*raw_bytes = 0;

	while (r0 < b->nrows)
	{
		uint32		rows = 0;
		Size		acc = 0;

		/* the group ends at the row limit or when its bytes reach the limit */
		while (r0 + rows < b->nrows && rows < opts->chunk_rows)
		{
			acc += kc_row_bytes(b, ti, r0 + rows);
			rows++;
			if (acc >= KC_CHUNK_BYTE_LIMIT)
				break;
		}

		appendBinaryStringInfo(&footer, (char *) &rows, sizeof(rows));
		for (c = 0; c < ncols; c++)
		{
			KcChunkDisk meta;
			char	   *mn;
			char	   *mx;
			char	   *data;
			uint32		dlen;
			StringInfoData sbuf;

			initStringInfo(&sbuf);
			kc_encode_chunk(&ti->cols[c], &b->vals[(Size) r0 * ncols + c],
							&b->nulls[(Size) r0 * ncols + c], ncols, rows,
							codec, opts->level, &sbuf, &meta, &mn, &mx);
			data = sbuf.data;
			dlen = sbuf.len;
			meta.off = blob->len;
			*raw_bytes += meta.raw_len;

			/* stripe-level statistics of the column */
			cs[c].nnulls += meta.nnulls;
			if (meta.nnulls < rows)
			{
				cs[c].any_values = true;
				if (!(meta.flags & KC_CHUNK_HAS_MINMAX))
					cs[c].all_mm = false;
				else
				{
					Datum		dmin = kc_stat_datum(&ti->cols[c], mn, meta.min_len);
					Datum		dmax = kc_stat_datum(&ti->cols[c], mx, meta.max_len);

					if (!cs[c].have)
					{
						cs[c].min = dmin;
						cs[c].max = dmax;
						cs[c].have = true;
					}
					else
					{
						if (kc_stat_compare(&ti->cols[c], dmin, cs[c].min) < 0)
							cs[c].min = dmin;
						if (kc_stat_compare(&ti->cols[c], dmax, cs[c].max) > 0)
							cs[c].max = dmax;
					}
				}
			}

			MemoryContextSwitchTo(old);
			kc_buf_append(blob, data, dlen);
			MemoryContextSwitchTo(tmp);

			appendBinaryStringInfo(&footer, (char *) &meta, sizeof(meta));
			if (meta.flags & KC_CHUNK_HAS_MINMAX)
			{
				appendBinaryStringInfo(&footer, mn, meta.min_len);
				appendBinaryStringInfo(&footer, mx, meta.max_len);
			}
			pfree(data);
		}
		ngroups++;
		r0 += rows;
	}

	/* the stripe statistics in their stored form */
	for (c = 0; c < ncols; c++)
	{
		KcStatDisk	d;
		bool		mm = cs[c].have && cs[c].all_mm && cs[c].any_values;
		Size		mnl = 0,
					mxl = 0;
		char	   *mnb = NULL,
				   *mxb = NULL;
		const KcColInfo *ci = &ti->cols[c];

		if (mm)
		{
			mnl = (ci->byval || ci->typlen > 0) ? ci->typlen : VARSIZE_ANY(DatumGetPointer(cs[c].min));
			mxl = (ci->byval || ci->typlen > 0) ? ci->typlen : VARSIZE_ANY(DatumGetPointer(cs[c].max));
			if (mnl > KC_MAX_MINMAX_BYTES || mxl > KC_MAX_MINMAX_BYTES)
				mm = false;
		}
		memset(&d, 0, sizeof(d));
		d.typid = ci->typid;
		d.collation = ci->collation;
		d.nnulls = (uint32) cs[c].nnulls;
		d.flags = (cs[c].nnulls > 0 ? KC_CHUNK_HAS_NULLS : 0) |
			(mm ? KC_CHUNK_HAS_MINMAX : 0);
		d.min_len = mm ? (uint16) mnl : 0;
		d.max_len = mm ? (uint16) mxl : 0;
		appendBinaryStringInfo(stats, (char *) &d, sizeof(d));
		if (mm)
		{
			if (ci->byval)
			{
				mnb = palloc(8);
				mxb = palloc(8);
				switch (ci->typlen)
				{
					case 1:
						*mnb = DatumGetChar(cs[c].min);
						*mxb = DatumGetChar(cs[c].max);
						break;
					case 2:
						{
							int16		x = DatumGetInt16(cs[c].min);
							int16		y = DatumGetInt16(cs[c].max);

							memcpy(mnb, &x, 2);
							memcpy(mxb, &y, 2);
							break;
						}
					case 4:
						{
							int32		x = DatumGetInt32(cs[c].min);
							int32		y = DatumGetInt32(cs[c].max);

							memcpy(mnb, &x, 4);
							memcpy(mxb, &y, 4);
							break;
						}
					default:
						{
							int64		x = DatumGetInt64(cs[c].min);
							int64		y = DatumGetInt64(cs[c].max);

							memcpy(mnb, &x, 8);
							memcpy(mxb, &y, 8);
							break;
						}
				}
			}
			else
			{
				mnb = DatumGetPointer(cs[c].min);
				mxb = DatumGetPointer(cs[c].max);
			}
			appendBinaryStringInfo(stats, mnb, mnl);
			appendBinaryStringInfo(stats, mxb, mxl);
		}
	}

	/* command-id change points: (row index, command id) pairs */
	for (r = 0; r < b->nrows; r++)
	{
		if (r == 0 || b->cids[r] != b->cids[r - 1])
		{
			uint32		pair[2];

			pair[0] = r;
			pair[1] = (uint32) b->cids[r];
			appendBinaryStringInfo(cidpts, (char *) pair, sizeof(pair));
		}
	}

	/* footer, then the header over the reserved first bytes */
	*footer_off = blob->len;
	hdr.magic = KC_STRIPE_MAGIC;
	hdr.version = KC_STRIPE_VERSION;
	hdr.ncols = ncols;
	hdr.nrows = b->nrows;
	hdr.ngroups = ngroups;
	hdr.stripe_id = stripe_id;
	hdr.footer_off = *footer_off;
	hdr.footer_len = footer.len;
	hdr.footer_crc = (uint32) kc_crc32c(footer.data, footer.len);
	hdr.blob_len = *footer_off + footer.len;
	MemoryContextSwitchTo(old);
	kc_buf_append(blob, footer.data, footer.len);
	memcpy(blob->data, &hdr, sizeof(hdr));

	*ngroups_out = ngroups;
	MemoryContextDelete(tmp);
}

/*
 * Write the buffer as one stripe and empty it.  The caller is in the
 * subtransaction that owns the buffer and holds a lock on rel.
 *
 * The order is the one the design's crash table relies on: reserve the
 * identity (metapage), write and log the pages, then insert the catalog row.
 * A crash after any step leaves unreferenced pages at worst.
 */
static void
kc_flush_buf(KcRelState *st, KcWriteBuf *b, Relation rel)
{
	KcStripeInfo si;
	KcBuf		blob = {NULL, 0, 0};
	StringInfoData stats;
	StringInfoData cidpts;
	uint64		stripe_id;
	uint64		first_row;
	uint32		ngroups;
	uint64		raw_bytes;
	uint64		footer_off;
	MemoryContext old = CurrentMemoryContext;
	MemoryContext fcxt;

	if (b->nrows == 0)
		return;

	if (!kc_rel_is_columnar(rel) || kc_buf_is_stale(b, rel))
	{
		/* converted to another access method, truncated or rewritten meanwhile */
		kc_buf_clear(b);
		return;
	}

	fcxt = AllocSetContextCreate(CurrentMemoryContext, "kingres_columnar flush",
								 ALLOCSET_DEFAULT_SIZES);
	MemoryContextSwitchTo(fcxt);
	initStringInfo(&stats);
	initStringInfo(&cidpts);

	kc_meta_allocate(rel, b->nrows, &stripe_id, &first_row);
	kc_build_stripe(rel, b, &b->opts, stripe_id, &blob, &stats, &cidpts,
					&ngroups, &raw_bytes, &footer_off);

	memset(&si, 0, sizeof(si));
	si.stripe_id = stripe_id;
	si.first_row = first_row;
	si.row_count = b->nrows;
	si.chunk_groups = ngroups;
	si.natts = b->ncols;
	si.blob_len = blob.len;
	si.raw_bytes = raw_bytes;
	si.footer_offset = footer_off;
	kc_stripe_write(rel, stripe_id, blob.data, blob.len, &si.first_block,
					&si.block_count);
	kc_catalog_insert(rel, &si, cidpts.data, cidpts.len, stats.data, stats.len);

	MemoryContextSwitchTo(old);
	MemoryContextDelete(fcxt);
	kc_buf_clear(b);
}

/* ------------------------------------------------------------------------
 * Entry points for the access method
 * ------------------------------------------------------------------------
 */

/* Flush the running subtransaction's buffers of rel, if they may be. */
void
kc_write_flush_relation(Relation rel)
{
	KcRelState *st = kc_state_get(RelationGetRelid(rel), false);
	ListCell   *lc;

	if (st == NULL)
		return;
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);

		if (b->nrows > 0 && kc_buf_flushable(st, b))
			kc_flush_buf(st, b, rel);
	}
}

/* Forget the running subtransaction's buffered rows of a relation. */
void
kc_write_discard_relation(Oid relid, bool *had_outer)
{
	KcRelState *st = kc_state_get(relid, false);
	ListCell   *lc;

	if (had_outer)
		*had_outer = false;
	if (st == NULL)
		return;
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);

		if (b->subid == GetCurrentSubTransactionId())
			kc_buf_clear(b);
		else if (had_outer && b->nrows > 0)
			*had_outer = true;
	}
}

void
kc_write_pin_relation(Oid relid, int delta)
{
	KcRelState *st = kc_state_get(relid, delta > 0);

	if (st == NULL)
		return;
	st->pins += delta;
	if (st->pins < 0)
		st->pins = 0;
}

/* The buffers a scan starting now has to read, with their current sizes. */
KcPendingRef *
kc_write_pending_refs(Oid relid, uint64 uid, int *n)
{
	KcRelState *st = kc_state_get(relid, false);
	KcPendingRef *res;
	ListCell   *lc;
	int			i = 0;

	*n = 0;
	if (st == NULL || st->bufs == NIL)
		return NULL;
	res = palloc(sizeof(KcPendingRef) * list_length(st->bufs));
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);

		if (b->nrows == 0 || b->uid != uid)
			continue;
		res[i].serial = b->serial;
		res[i].nrows = b->nrows;
		i++;
	}
	*n = i;
	return res;
}

/*
 * Row rowno of the buffer the reference names.  The pointers stay valid until
 * the next insert into the relation (the by-reference values much longer).
 */
bool
kc_write_pending_fetch(Oid relid, const KcPendingRef *ref, uint32 rowno,
					   Datum **vals, bool **nulls, int *ncols, CommandId *cid)
{
	KcRelState *st = kc_state_get(relid, false);
	ListCell   *lc;

	if (st == NULL)
		return false;
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);

		if (b->serial != ref->serial)
			continue;
		if (rowno >= ref->nrows || rowno >= b->nrows)
			return false;
		*vals = &b->vals[(Size) rowno * b->ncols];
		*nulls = &b->nulls[(Size) rowno * b->ncols];
		*ncols = b->ncols;
		*cid = b->cids[rowno];
		return true;
	}
	return false;
}

/* ------------------------------------------------------------------------
 * Transaction callbacks
 * ------------------------------------------------------------------------
 */

/* At pre-commit and pre-prepare every remaining buffer becomes a stripe. */
static void
kc_flush_all(void)
{
	HASH_SEQ_STATUS seq;
	KcRelState *st;
	bool		pushed = false;

	if (kc_states == NULL)
		return;

	/*
	 * The heap insert of the catalog row wants a snapshot; at pre-commit the
	 * statement is over and none is active.
	 */
	if (!ActiveSnapshotSet())
	{
		PushActiveSnapshot(GetTransactionSnapshot());
		pushed = true;
	}

	/*
	 * Flushing inserts into other tables and never into the hash itself, but
	 * can open relations; restart the scan is not needed because no entry is
	 * added or removed here.
	 */
	hash_seq_init(&seq, kc_states);
	while ((st = hash_seq_search(&seq)) != NULL)
	{
		ListCell   *lc;
		Relation	rel = NULL;

		foreach(lc, st->bufs)
		{
			KcWriteBuf *b = lfirst(lc);

			if (b->nrows == 0)
				continue;
			if (b->subid != GetCurrentSubTransactionId())
				elog(ERROR, "columnar write buffer of an inner subtransaction survived to commit");
			if (rel == NULL)
			{
				rel = try_table_open(st->relid, RowExclusiveLock);
				if (rel == NULL)
				{
					/* dropped in this transaction */
					kc_buf_clear(b);
					continue;
				}
			}
			kc_flush_buf(st, b, rel);
		}
		if (rel != NULL)
			table_close(rel, NoLock);
	}
	if (pushed)
		PopActiveSnapshot();
}

static void
kc_xact_callback(XactEvent event, void *arg)
{
	switch (event)
	{
		case XACT_EVENT_PRE_COMMIT:
		case XACT_EVENT_PRE_PREPARE:
			kc_flush_all();
			break;
		case XACT_EVENT_COMMIT:
		case XACT_EVENT_PREPARE:
		case XACT_EVENT_ABORT:
			kc_state_reset();
			break;
		default:
			break;
	}
}

static void
kc_subxact_callback(SubXactEvent event, SubTransactionId mySubid,
					SubTransactionId parentSubid, void *arg)
{
	HASH_SEQ_STATUS seq;
	KcRelState *st;

	if (kc_states == NULL)
		return;
	if (event != SUBXACT_EVENT_COMMIT_SUB && event != SUBXACT_EVENT_ABORT_SUB)
		return;

	hash_seq_init(&seq, kc_states);
	while ((st = hash_seq_search(&seq)) != NULL)
	{
		List	   *keep = NIL;
		ListCell   *lc;
		MemoryContext old = MemoryContextSwitchTo(kc_write_cxt);

		foreach(lc, st->bufs)
		{
			KcWriteBuf *b = lfirst(lc);
			KcWriteBuf *prev = keep ? llast(keep) : NULL;

			if (b->subid != mySubid)
			{
				keep = lappend(keep, b);
				continue;
			}
			if (event == SUBXACT_EVENT_ABORT_SUB)
			{
				kc_buf_clear(b);
				MemoryContextDelete(b->cxt);
				pfree(b);
				continue;
			}

			/* commit: the rows now belong to the parent subtransaction */
			if (prev != NULL && prev->subid == parentSubid &&
				prev->ncols == b->ncols && prev->uid == b->uid &&
				st->pins == 0)
			{
				uint32		r;

				kc_buf_reserve(prev, b->nrows);
				for (r = 0; r < b->nrows; r++)
				{
					Size		idx = (Size) prev->nrows * prev->ncols;

					memcpy(&prev->vals[idx], &b->vals[(Size) r * b->ncols],
						   sizeof(Datum) * prev->ncols);
					memcpy(&prev->nulls[idx], &b->nulls[(Size) r * b->ncols],
						   prev->ncols);
					prev->cids[prev->nrows] = b->cids[r];
					prev->nrows++;
				}
				prev->bytes += b->bytes;

				/* the by-reference values stay in b's context, now prev's child */
				MemoryContextSetParent(b->cxt, prev->cxt);
				b->bytes = 0;
				b->nrows = 0;
			}
			else
			{
				b->subid = parentSubid;
				keep = lappend(keep, b);
			}
		}
		MemoryContextSwitchTo(old);
		st->bufs = keep;
	}
}

void
kc_write_init(void)
{
	RegisterXactCallback(kc_xact_callback, NULL);
	RegisterSubXactCallback(kc_subxact_callback, NULL);
}
