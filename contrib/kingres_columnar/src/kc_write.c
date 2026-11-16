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
 * is flushed by it, because the catalog row takes the transaction id of
 * whoever inserts it and must disappear with the subtransaction that owns the
 * data.  When a subtransaction commits its buffers are handed to its parent;
 * when it aborts they are dropped.
 *
 * The invariant of the hand-over: the rows of a subtransaction become rows of
 * its parent atomically for every relation, or not at all.  PostgreSQL runs
 * SUBXACT_EVENT_PRE_COMMIT_SUB while the subtransaction is still in progress
 * (an error there aborts it cleanly and runs the abort callbacks), and
 * SUBXACT_EVENT_COMMIT_SUB after the point of no return (an error there still
 * aborts the subtransaction, with the warning "AbortSubTransaction while in
 * COMMIT state", but a hand-over that has begun cannot be taken back).  So
 * everything that can fail (memory, I/O, a flush) is done for all relations in
 * the first callback and left unpublished, and the second callback only
 * assigns pointers and counts, which cannot fail.  See kc_subxact_prepare.
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
#include "utils/guc.h"
#include "utils/hsearch.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"

#include "kingres_columnar.h"

/*
 * The largest block a buffer's memory context takes from the allocator.  The
 * context of a buffer grows by doubling blocks up to this, and what is unused of
 * the last block is held memory the buffer's accounting (kc_charge) does not see,
 * so the size is kept small: the slack of a buffer is at most one block.
 */
#define KC_BUF_MAX_BLOCK	(1024 * 1024)

/* the rows of a buffer that a flush did not write, in memory of their own */
typedef struct KcTail
{
	MemoryContext cxt;
	uint32		nrows;
	uint32		cap;
	Datum	   *vals;
	bool	   *nulls;
	CommandId  *cids;
	uint32	   *sizes;
	Size		bytes;
} KcTail;

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
	uint32	   *sizes;			/* bytes of each by-reference value, else 0 */
	int			refs;			/* scans that hold a reference to this buffer */

	/*
	 * Memory charged for this buffer: the by-reference values (the space their
	 * chunks take, headers and rounding included) and the arrays of row slots
	 * (their capacity, not their fill).  This is what counts against
	 * write_buffer_memory (kc_total_bytes is the sum over all buffers), so the
	 * allowance is about memory and not about payload.
	 */
	Size		bytes;
	KcOptions	opts;

	/*
	 * The visible part of a buffer is the rows [vis_from, nrows) (see
	 * kc_buf_visible_from): every reader takes it from there.  The rows before
	 * vis_from are already in a stripe that a prepared hand-over has written
	 * (kc_merge_flush_prepare) and are read from it; until the hand-over is
	 * published the buffer still holds them, because an abort of the
	 * subtransaction takes the stripe away again.
	 */
	uint32		vis_from;

	/*
	 * A hand-over to the parent that kc_subxact_prepare has made ready and
	 * kc_subxact_publish has not yet made (see kc_subxact_prepare).
	 */
	struct KcWriteBuf *merge_into;	/* the parent's buffer that takes these rows */
	uint32		merge_base;		/* its row count when the rows were copied */
	uint32		merge_rows;		/* these rows' count when they were copied */
	char	   *merge_chunk;	/* their by-reference values, in merge_into's context */
	Size		merge_charge;	/* what merge_chunk is charged when it is published */
	uint32		merge_written;	/* rows of the combined sequence already written as stripes */
	bool		tail_ready;		/* a flush wrote the front; "tail" replaces merge_into's rows */
	KcTail		tail;
} KcWriteBuf;

typedef struct KcRelState
{
	Oid			relid;			/* hash key */
	bool		frozen;			/* stripes get a frozen creator (kc_write_set_frozen) */
	List	   *bufs;			/* KcWriteBuf *, outermost first */
} KcRelState;

static MemoryContext kc_write_cxt = NULL;
static HTAB *kc_states = NULL;
static HTAB *kc_holds = NULL;
static Size kc_total_bytes = 0;
static uint32 kc_next_serial = 1;

/*
 * Bytes written to buffers since the last look for something to flush under
 * memory pressure that found nothing worth writing: the look costs a pass
 * over all buffers, so it is not repeated for every row while the memory that
 * is over the allowance cannot be written by the running subtransaction.
 */
static Size kc_bytes_since_look = 0;

/*
 * Between the two phases of a subtransaction's hand-over (kc_subxact_prepare,
 * kc_subxact_publish) the buffers are in a state that only phase two may
 * change.  Code of another extension that runs between them may read the table
 * (readers use the visible part of a buffer) but not write it.
 */
static bool kc_handover_prepared = false;

/* a hold taken while a hand-over was prepared that its publication broke */
static bool kc_broken_any = false;

/* set while compaction copies rows through the buffers (kc_write_set_maintenance) */
static bool kc_maintenance_write = false;

/*
 * The files this transaction replaced (a rewrite, VACUUM FULL, a change of
 * access method): the table and the identity of the file it had.  The stripe
 * rows of such a file are of no use to anyone once the new file is in place, and
 * the access method has no callback when a file goes away, so the rows are
 * removed by the command that replaced the file or, at the latest, at commit
 * (kc_write_cleanup_replaced), and come back if the transaction aborts.  The
 * list is in the memory of the top-level transaction and starts empty.
 */
typedef struct KcReplaced
{
	Oid			relid;
	uint64		uid;
} KcReplaced;

static List *kc_replaced = NIL;

/*
 * The rows compaction copies: a buffer of its own, never in a relation's list,
 * so no subtransaction hands it over, merges it or mixes it with the rows a
 * user wrote.  Its stripes are therefore all maintenance stripes, whatever
 * else the transaction does (kc_write_maintenance_flush).
 */
static struct KcWriteBuf *kc_maint_buf = NULL;

/* ------------------------------------------------------------------------
 * Fault injection (assertion builds only)
 *
 * A developer facility, to prove that the hand-over of a subtransaction's
 * buffers and the flush of several relations are all or nothing.  Two
 * settings of superusers, compiled only when the server is built with
 * assertions (USE_ASSERT_CHECKING; PostgreSQL's own injection points are not
 * enabled in the installations this extension is tested against):
 *
 *	 kingres_columnar.fault_point  where to fail: 'alloc' (any allocation of
 *								   the write path), 'merge' (the hand-over
 *								   of one relation's buffer to the parent),
 *								   'flush' (the flush of one relation's
 *								   buffer), or 'off'
 *	 kingres_columnar.fault_nth	   the number of the hit that fails
 *
 * Setting either one arms the counter anew.  The failure is raised once, as
 * the error an exhausted allocation raises (out of memory, 53200), and the
 * counter is then spent until the next SET.  In a build without assertions
 * the settings do not exist and every KC_FAULT() is empty.
 * ------------------------------------------------------------------------
 */
#define KC_FAULT_OFF	0
#define KC_FAULT_ALLOC	1
#define KC_FAULT_MERGE	2
#define KC_FAULT_FLUSH	3

#ifdef USE_ASSERT_CHECKING
static int	kc_fault_point = KC_FAULT_OFF;
static int	kc_fault_nth = 0;
static int	kc_fault_left = 0;

static const struct config_enum_entry kc_fault_points[] = {
	{"off", KC_FAULT_OFF, false},
	{"alloc", KC_FAULT_ALLOC, false},
	{"merge", KC_FAULT_MERGE, false},
	{"flush", KC_FAULT_FLUSH, false},
	{NULL, 0, false}
};

static void
kc_fault_assign_point(int newval, void *extra)
{
	kc_fault_left = kc_fault_nth;
}

static void
kc_fault_assign_nth(int newval, void *extra)
{
	kc_fault_left = newval;
}

static void
kc_fault_hit(int point)
{
	if (point != kc_fault_point || kc_fault_left <= 0)
		return;
	if (--kc_fault_left == 0)
		ereport(ERROR,
				(errcode(ERRCODE_OUT_OF_MEMORY),
				 errmsg("out of memory"),
				 errdetail("Injected by kingres_columnar.fault_point.")));
}

#define KC_FAULT(p) kc_fault_hit(p)
#else
#define KC_FAULT(p) ((void) 0)
#endif

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
	kc_holds = NULL;
	kc_maintenance_write = false;
	kc_maint_buf = NULL;
	kc_total_bytes = 0;
	kc_bytes_since_look = 0;
	kc_handover_prepared = false;
	kc_broken_any = false;
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
		st->frozen = false;
		st->bufs = NIL;
	}
	return st;
}

static KcWriteBuf *
kc_buf_make(Relation rel, int ncols)
{
	KcWriteBuf *b;
	MemoryContext cxt;
	KcMetaData meta;

	KC_FAULT(KC_FAULT_ALLOC);
	/* small to start with: a transaction may have a buffer per subtransaction */
	cxt = AllocSetContextCreate(kc_write_cxt, "kingres_columnar rows",
								ALLOCSET_DEFAULT_MINSIZE,
								ALLOCSET_SMALL_INITSIZE,
								KC_BUF_MAX_BLOCK);
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
	return b;
}

static KcWriteBuf *
kc_buf_create(KcRelState *st, Relation rel, int ncols)
{
	KcWriteBuf *b = kc_buf_make(rel, ncols);
	MemoryContext old = MemoryContextSwitchTo(kc_write_cxt);

	st->bufs = lappend(st->bufs, b);
	MemoryContextSwitchTo(old);
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
	b->sizes = NULL;
	b->bytes = 0;
	b->vis_from = 0;
}

/* ------------------------------------------------------------------------
 * Appending rows
 * ------------------------------------------------------------------------
 */

/* The memory of one row slot of a buffer of ncols columns, in the arrays. */
static Size
kc_slot_bytes(int ncols)
{
	return (Size) ncols * (sizeof(Datum) + sizeof(bool) + sizeof(uint32)) +
		sizeof(CommandId);
}

/* Charge (or, negative, give back) memory to a buffer and to the total. */
static void
kc_charge(KcWriteBuf *b, int64 delta)
{
	if (delta >= 0)
	{
		b->bytes += (Size) delta;
		kc_total_bytes += (Size) delta;
	}
	else
	{
		Size		d = Min(b->bytes, (Size) -delta);

		b->bytes -= d;
		kc_total_bytes -= Min(kc_total_bytes, d);
	}
}

static void
kc_buf_reserve(KcWriteBuf *b, uint32 extra)
{
	uint32		need = b->nrows + extra;

	if (need <= b->cap)
		return;
	KC_FAULT(KC_FAULT_ALLOC);
	{
		/*
		 * Start small: a subtransaction that inserts one row must not cost
		 * thousands of row slots.  A bulk load doubles its way up.
		 */
		uint32		ncap = b->cap ? b->cap : 16;
		MemoryContext old = MemoryContextSwitchTo(b->cxt);

		while (ncap < need)
			ncap *= 2;
		if (b->vals == NULL)
		{
			b->vals = palloc_array(Datum, (Size) ncap * b->ncols);
			b->nulls = palloc_array(bool, (Size) ncap * b->ncols);
			b->cids = palloc_array(CommandId, ncap);
			b->sizes = palloc0_array(uint32, (Size) ncap * b->ncols);
		}
		else
		{
			Size		oldn = (Size) b->cap * b->ncols;

			b->vals = repalloc(b->vals, sizeof(Datum) * (Size) ncap * b->ncols);
			b->nulls = repalloc(b->nulls, (Size) ncap * b->ncols);
			b->cids = repalloc(b->cids, sizeof(CommandId) * ncap);
			b->sizes = repalloc(b->sizes, sizeof(uint32) * (Size) ncap * b->ncols);
			memset(&b->sizes[oldn], 0,
				   sizeof(uint32) * ((Size) ncap * b->ncols - oldn));
		}
		kc_charge(b, (int64) (ncap - b->cap) * (int64) kc_slot_bytes(b->ncols));
		b->cap = ncap;
		MemoryContextSwitchTo(old);
	}
}

/*
 * Copy one value into the buffer's memory; returns the datum and its size.
 * The chunk the copy takes is charged to the buffer at once, even if the row
 * is never completed: the memory is held until the buffer is cleared.
 */
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
	KC_FAULT(KC_FAULT_ALLOC);
	old = MemoryContextSwitchTo(b->cxt);
	if (att->attlen > 0)
	{
		char	   *p = palloc(att->attlen);

		memcpy(p, DatumGetPointer(v), att->attlen);
		res = PointerGetDatum(p);
		*sz = att->attlen;
		kc_charge(b, (int64) GetMemoryChunkSpace(p));
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
		kc_charge(b, (int64) GetMemoryChunkSpace(p));
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
	uint32	   *ns;
	uint32		r;
	int			c;

	if (b->nrows == 0)
	{
		/* nothing to keep: the arrays and every value go with the context */
		kc_buf_clear(b);
		b->ncols = new_ncols;
		return;
	}
	KC_FAULT(KC_FAULT_ALLOC);
	old = MemoryContextSwitchTo(b->cxt);
	nv = palloc_array(Datum, (Size) b->cap * new_ncols);
	nn = palloc_array(bool, (Size) b->cap * new_ncols);
	ns = palloc0_array(uint32, (Size) b->cap * new_ncols);
	for (r = 0; r < b->nrows; r++)
	{
		memcpy(&nv[(Size) r * new_ncols], &b->vals[(Size) r * old_ncols],
			   sizeof(Datum) * old_ncols);
		memcpy(&nn[(Size) r * new_ncols], &b->nulls[(Size) r * old_ncols],
			   old_ncols);
		memcpy(&ns[(Size) r * new_ncols], &b->sizes[(Size) r * old_ncols],
			   sizeof(uint32) * old_ncols);
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
			ns[(Size) r * new_ncols + c] = isnull ? 0 : (uint32) sz;
		}
	}
	pfree(b->vals);
	pfree(b->nulls);
	pfree(b->sizes);
	b->vals = nv;
	b->nulls = nn;
	b->sizes = ns;
	kc_charge(b, ((int64) kc_slot_bytes(new_ncols) - (int64) kc_slot_bytes(old_ncols)) *
			  (int64) b->cap);
	b->ncols = new_ncols;
	MemoryContextSwitchTo(old);
}

/*
 * Is a buffer one that the running subtransaction may flush now?  Its rows
 * must be its own (a stripe carries the id of the subtransaction that writes
 * it, and rows of an enclosing one would be lost with an abort of this one)
 * and no scan may be reading it by position (a scan holds the buffers that had
 * rows when it began; rows written after that go to a buffer of their own,
 * which no scan reads, so a scan on a table does not stop the table's later
 * rows from leaving memory).
 */
static bool
kc_buf_flushable(KcRelState *st, KcWriteBuf *b)
{
	return b->refs == 0 && b->subid == GetCurrentSubTransactionId();
}

static void kc_write_stripe(KcRelState *st, KcWriteBuf *b, Relation rel,
						   uint32 first, uint32 count, bool maintenance);
static void kc_flush_buf(KcRelState *st, KcWriteBuf *b, Relation rel);
static void kc_flush_buf_ex(KcRelState *st, KcWriteBuf *b, Relation rel,
							bool keep_tail);
static uint32 kc_stripe_limit(const KcOptions *opts);
static void kc_coalesce(KcRelState *st);

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

/*
 * The fewest rows a flush that only memory pressure asks for may write: a
 * buffer below it is not worth a stripe, however full the memory is, because
 * the stripes a table is made of would then depend on how its rows were
 * grouped into subtransactions and not on its data (a loader with a block
 * per batch would write a stripe per few rows).  A buffer of fewer rows still
 * counts when its rows are so wide that they are a real part of the memory
 * allowance.  The last flush of a transaction (commit, prepare, or a command
 * that needs the rows in the table) is not bound by this.
 */
static uint32
kc_min_flush_rows(const KcOptions *opts)
{
	return Min(kc_stripe_limit(opts), Max(opts->chunk_rows, (uint32) KC_MIN_STRIPE_ROWS));
}

/* The memory allowance of the write buffers, in bytes. */
static Size
kc_allowance(void)
{
	return (Size) kc_guc_write_buffer_kb * 1024;
}

/*
 * The memory that makes a buffer a real part of the allowance: half of it.  A
 * buffer that holds this much is written whatever its row count.
 */
static Size
kc_bytes_worth(void)
{
	return kc_allowance() / 2;
}

/*
 * A buffer is worth a stripe of its own when it has the rows for one
 * (kc_min_flush_rows) or when its memory is a real part of the allowance
 * (kc_bytes_worth).  The row minimum only keeps stripes from being tiny; it
 * never decides about memory: a buffer that is big in bytes is written
 * whatever its row count, so wide rows cannot hide behind a minimum made for
 * narrow ones.
 */
static bool
kc_worth_flushing(KcWriteBuf *b)
{
	return b->nrows >= kc_min_flush_rows(&b->opts) ||
		b->bytes >= kc_bytes_worth();
}

/*
 * The write buffers are over the allowance: write buffers of the running
 * subtransaction as stripes, the largest first, until they are within it or
 * nothing is left that the running subtransaction may write.  A buffer that
 * is not worth a stripe by itself is written only while the memory of all the
 * flushable buffers together is a real part of the allowance (half of it),
 * which is what keeps many tables with small buffers within the allowance
 * without making a tiny stripe of every few rows of one table when it is
 * the rows of an enclosing subtransaction that fill the memory.  What is left
 * over the allowance then is below half of it per open level of
 * subtransaction (kc_subxact_prepare writes the rest when a level commits).
 */
static void
kc_flush_pressure(void)
{
	Size		allowance = kc_allowance();

	if (kc_states == NULL)
		return;
	while (kc_total_bytes > allowance)
	{
		HASH_SEQ_STATUS seq;
		KcRelState *st;
		KcRelState *best_st = NULL;
		KcWriteBuf *best = NULL;
		Size		flushable = 0;
		Relation	rel;

		hash_seq_init(&seq, kc_states);
		while ((st = hash_seq_search(&seq)) != NULL)
		{
			ListCell   *lc;

			foreach(lc, st->bufs)
			{
				KcWriteBuf *b = lfirst(lc);

				if (b->nrows == 0 || !kc_buf_flushable(st, b))
					continue;
				flushable += b->bytes;
				if (best == NULL || b->bytes > best->bytes)
				{
					best = b;
					best_st = st;
				}
			}
		}
		if (best == NULL || (!kc_worth_flushing(best) && flushable < kc_bytes_worth()))
			break;
		rel = try_table_open(best->relid, RowExclusiveLock);
		if (rel == NULL)
		{
			kc_buf_clear(best);
			continue;
		}
		kc_flush_buf(best_st, best, rel);
		table_close(rel, NoLock);
	}
	kc_bytes_since_look = 0;
}

static void kc_buf_free(KcWriteBuf *b);

/* The private buffer of the rows compaction copies, made on first use. */
static KcWriteBuf *
kc_maint_buffer(Relation rel, int ncols)
{
	if (kc_maint_buf != NULL &&
		(kc_maint_buf->relid != RelationGetRelid(rel) ||
		 kc_maint_buf->uid != kc_rel_ident(rel)->uid))
	{
		/* left behind by a compaction that failed: its rows were never stripes */
		kc_buf_free(kc_maint_buf);
		kc_maint_buf = NULL;
	}
	if (kc_maint_buf == NULL)
		kc_maint_buf = kc_buf_make(rel, ncols);
	return kc_maint_buf;
}

/*
 * Write what compaction copied as stripes of maintenance rows, and forget the
 * buffer.  Also drops what a failed compaction left (kc_maint_buf of another
 * relation), so a new compaction never starts with rows it did not copy.
 */
void
kc_write_maintenance_flush(Relation rel)
{
	KcWriteBuf *b = kc_maint_buf;
	KcRelState *st;
	uint32		limit;
	uint32		done = 0;

	if (b == NULL)
		return;
	kc_maint_buf = NULL;
	if (b->relid == RelationGetRelid(rel) && b->uid == kc_rel_ident(rel)->uid &&
		b->nrows > 0)
	{
		st = kc_state_get(b->relid, true);
		limit = kc_stripe_limit(&b->opts);
		while (done < b->nrows)
		{
			uint32		n = Min(limit, b->nrows - done);

			kc_write_stripe(st, b, rel, done, n, true);
			done += n;
		}
	}
	kc_buf_free(b);
}

void
kc_write_maintenance_discard(void)
{
	if (kc_maint_buf != NULL)
	{
		kc_buf_free(kc_maint_buf);
		kc_maint_buf = NULL;
	}
}

/* Has a hand-over that is prepared and not yet published rows of this relation? */
static bool
kc_rel_handover_prepared(KcRelState *st)
{
	ListCell   *lc;

	foreach(lc, st->bufs)
	{
		if (((KcWriteBuf *) lfirst(lc))->merge_into != NULL)
			return true;
	}
	return false;
}

void
kc_write_row_values(Relation rel, Datum *vals, bool *nulls, CommandId cid)
{
	TupleDesc	desc = RelationGetDescr(rel);
	KcRelState *st = kc_state_get(RelationGetRelid(rel), true);
	KcWriteBuf *b = NULL;
	MemoryContext old = CurrentMemoryContext;
	int			c;
	Size		before = kc_total_bytes;

	/*
	 * The two phases of a subtransaction's hand-over leave the buffers in a
	 * state that only the second phase may change.  Nothing in PostgreSQL
	 * writes between them; code of another extension that does gets an error,
	 * and the subtransaction aborts cleanly, rather than rows that are lost or
	 * written twice (kc_subxact_prepare).
	 */
	if (kc_handover_prepared && kc_rel_handover_prepared(st))
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("columnar table \"%s\" cannot be written while a subtransaction is being committed",
						RelationGetRelationName(rel)),
				 errdetail("A callback of another module wrote to the table between the two phases of the commit of a subtransaction.")));

	/*
	 * A write conflicts with transactions that read the table (SSI), except
	 * the copy of rows that readers see already (compaction): that writes
	 * nothing, logically.
	 */
	if (!kc_maintenance_write)
		CheckForSerializableConflictIn(rel, NULL, InvalidBlockNumber);

	if (kc_maintenance_write)
	{
		/* private rows of compaction: never in the list the subtransactions hand over */
		b = kc_maint_buffer(rel, desc->natts);
		MemoryContextSwitchTo(old);
	}
	else if (st->bufs != NIL)
	{
		b = llast(st->bufs);

		/*
		 * A buffer that a scan reads by position takes no more rows: they go
		 * to a buffer of their own, which can be written while the scan lasts.
		 */
		if (b->subid != GetCurrentSubTransactionId() || b->refs > 0 ||
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
			b->sizes[idx] = 0;
			continue;
		}
		{
			Size		sz;

			b->vals[idx] = kc_copy_value(b, att, vals[c], &sz);
			b->nulls[idx] = false;
			b->sizes[idx] = (uint32) sz;
		}
	}
	b->cids[b->nrows] = cid;
	b->nrows++;

	/*
	 * The compaction buffer is private and written as stripes when it holds a
	 * stripe of rows or the allowance, whichever comes first.
	 */
	if (kc_maintenance_write)
	{
		if (b->nrows >= kc_stripe_limit(&b->opts) || b->bytes >= kc_allowance())
			kc_write_maintenance_flush(rel);
		return;
	}

	if (b->nrows >= kc_stripe_limit(&b->opts) && kc_buf_flushable(st, b))
	{
		Relation	r = try_table_open(b->relid, RowExclusiveLock);

		if (r != NULL)
		{
			kc_flush_buf_ex(st, b, r, true);
			table_close(r, NoLock);
		}
	}

	/*
	 * The memory allowance, by bytes: whatever the width of the rows and
	 * however they are grouped into statements and subtransactions, what is
	 * over it is written as soon as the running subtransaction may write it.
	 * The look costs a pass over the buffers, so while it finds nothing to
	 * write (the memory that is over belongs to enclosing subtransactions) it
	 * is repeated only after a sixty-fourth of the allowance in new rows.
	 */
	kc_bytes_since_look += kc_total_bytes > before ? kc_total_bytes - before : 0;
	if (kc_total_bytes > kc_allowance() &&
		(before <= kc_allowance() || kc_bytes_since_look >= kc_allowance() / 64))
		kc_flush_pressure();
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
kc_build_stripe(Relation rel, KcWriteBuf *b, uint32 first, uint32 count,
				const KcOptions *opts, uint64 stripe_id, KcBuf *blob, StringInfo stats,
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

	while (r0 < count)
	{
		uint32		rows = 0;
		Size		acc = 0;

		/* the group ends at the row limit or when its bytes reach the limit */
		while (r0 + rows < count && rows < opts->chunk_rows)
		{
			acc += kc_row_bytes(b, ti, first + r0 + rows);
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
			kc_encode_chunk(&ti->cols[c], &b->vals[(Size) (first + r0) * ncols + c],
							&b->nulls[(Size) (first + r0) * ncols + c], ncols, rows,
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
	for (r = 0; r < count; r++)
	{
		if (r == 0 || b->cids[first + r] != b->cids[first + r - 1])
		{
			uint32		pair[2];

			pair[0] = r;
			pair[1] = (uint32) b->cids[first + r];
			appendBinaryStringInfo(cidpts, (char *) pair, sizeof(pair));
		}
	}

	/* footer, then the header over the reserved first bytes */
	*footer_off = blob->len;
	hdr.magic = KC_STRIPE_MAGIC;
	hdr.version = KC_STRIPE_VERSION;
	hdr.ncols = ncols;
	hdr.nrows = count;
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
 * The most rows one stripe of this buffer may have.  One definition for the
 * writer: the table's stripe size, never above kc_max_stripe_rows() (the
 * reader's own limit, kc_catalog.c, is higher, so what the writer produces is
 * always readable).  Every way rows leave a buffer goes through this: a buffer
 * that grew past it (rows merged in from committed subtransactions) is cut
 * into several stripes.
 */
static uint32
kc_stripe_limit(const KcOptions *opts)
{
	return Min(opts->stripe_rows, (uint32) kc_max_stripe_rows());
}

/*
 * The rows [from, total) of a buffer, copied into memory of their own: what
 * is left of the buffer after the rows before them went into stripes.  The
 * by-reference values are copied, so the memory of the rows that were written
 * is given back when the buffer takes the tail (kc_buf_tail_install) and the
 * byte count is the real one.  This is the part that can fail; it changes
 * nothing of b.
 */
static void
kc_buf_tail_build(KcWriteBuf *b, uint32 from, uint32 total, KcTail *t)
{
	MemoryContext ncxt;
	MemoryContext old;
	uint32		keep = total - from;
	uint32		cap = Max(keep, 16);
	Datum	   *nv;
	bool	   *nn;
	CommandId  *nc;
	uint32	   *ns;
	Size		bytes = 0;
	uint32		r;
	int			c;

	KC_FAULT(KC_FAULT_ALLOC);
	ncxt = AllocSetContextCreate(kc_write_cxt, "kingres_columnar rows",
								 ALLOCSET_DEFAULT_MINSIZE,
								 ALLOCSET_SMALL_INITSIZE,
								 KC_BUF_MAX_BLOCK);
	old = MemoryContextSwitchTo(ncxt);
	PG_TRY();
	{
		nv = palloc_array(Datum, (Size) cap * b->ncols);
		nn = palloc_array(bool, (Size) cap * b->ncols);
		nc = palloc_array(CommandId, cap);
		ns = palloc0_array(uint32, (Size) cap * b->ncols);
		for (r = 0; r < keep; r++)
		{
			Size		src = (Size) (from + r) * b->ncols;
			Size		dst = (Size) r * b->ncols;

			for (c = 0; c < b->ncols; c++)
			{
				Datum		d = b->vals[src + c];
				uint32		sz = b->sizes[src + c];

				if (!b->nulls[src + c] && sz > 0)
				{
					char	   *p = palloc(sz);

					memcpy(p, DatumGetPointer(d), sz);
					d = PointerGetDatum(p);
					bytes += GetMemoryChunkSpace(p);
				}
				nv[dst + c] = d;
				nn[dst + c] = b->nulls[src + c];
				ns[dst + c] = sz;
			}
			nc[r] = b->cids[from + r];
		}
		bytes += (Size) cap * kc_slot_bytes(b->ncols);
	}
	PG_CATCH();
	{
		MemoryContextSwitchTo(old);
		MemoryContextDelete(ncxt);
		PG_RE_THROW();
	}
	PG_END_TRY();
	MemoryContextSwitchTo(old);

	t->cxt = ncxt;
	t->nrows = keep;
	t->cap = cap;
	t->vals = nv;
	t->nulls = nn;
	t->cids = nc;
	t->sizes = ns;
	t->bytes = bytes;
}

/* Make the tail the buffer's rows and give back the rest.  Cannot fail. */
static void
kc_buf_tail_install(KcWriteBuf *b, KcTail *t)
{
	MemoryContext ocxt = b->cxt;

	kc_total_bytes -= Min(kc_total_bytes, b->bytes);
	kc_total_bytes += t->bytes;
	b->cxt = t->cxt;
	b->vals = t->vals;
	b->nulls = t->nulls;
	b->cids = t->cids;
	b->sizes = t->sizes;
	b->nrows = t->nrows;
	b->cap = t->cap;
	b->bytes = t->bytes;
	b->vis_from = 0;
	MemoryContextDelete(ocxt);
}

/*
 * Drop the first n rows of a buffer after they were written as stripes, and
 * keep the rest.
 */
static void
kc_buf_drop_prefix(KcWriteBuf *b, uint32 n)
{
	KcTail		t;

	kc_buf_tail_build(b, n, b->nrows, &t);
	kc_buf_tail_install(b, &t);
}

/*
 * Write rows [first, first + count) of the buffer as one stripe.  The caller
 * is in the subtransaction that owns the buffer and holds a lock on rel.
 *
 * The order is the one the design's crash table relies on: reserve the
 * identity (metapage), write and log the pages, then insert the catalog row.
 * A crash after any step leaves unreferenced pages at worst.
 */
static void
kc_write_stripe(KcRelState *st, KcWriteBuf *b, Relation rel, uint32 first,
				uint32 count, bool maintenance)
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

	Assert(count > 0 && count <= kc_stripe_limit(&b->opts));
	KC_FAULT(KC_FAULT_ALLOC);
	fcxt = AllocSetContextCreate(CurrentMemoryContext, "kingres_columnar flush",
								 ALLOCSET_DEFAULT_SIZES);
	MemoryContextSwitchTo(fcxt);
	initStringInfo(&stats);
	initStringInfo(&cidpts);

	kc_meta_allocate(rel, count, &stripe_id, &first_row);
	kc_build_stripe(rel, b, first, count, &b->opts, stripe_id, &blob, &stats,
					&cidpts, &ngroups, &raw_bytes, &footer_off);

	memset(&si, 0, sizeof(si));
	si.stripe_id = stripe_id;
	si.first_row = first_row;
	si.row_count = count;
	si.chunk_groups = ngroups;
	si.natts = b->ncols;
	si.blob_len = blob.len;
	si.raw_bytes = raw_bytes;
	si.footer_offset = footer_off;
	kc_stripe_write(rel, stripe_id, blob.data, blob.len, &si.first_block,
					&si.block_count);
	kc_catalog_insert(rel, &si, cidpts.data, cidpts.len, stats.data, stats.len,
					  st->frozen, maintenance);

	/*
	 * The stripe can be found by readers from here on.  A reader that took
	 * its relation lock before this point is found now; one that takes it
	 * after this point finds the stripe row (kc_catalog_load) and reports the
	 * dependency itself.  Both happen before the commit-time check of
	 * PostgreSQL, because this runs from the pre-commit callback.  A stripe
	 * of rows that only copy rows readers already see (compaction) changes
	 * nothing logically and takes no part in this.
	 */
	if (!maintenance)
		CheckForSerializableConflictIn(rel, NULL, InvalidBlockNumber);

	MemoryContextSwitchTo(old);
	MemoryContextDelete(fcxt);
}

/*
 * Write the buffer as stripes of at most kc_stripe_limit() rows and empty it.
 * With keep_tail, a last stripe that would be shorter than a full one stays
 * buffered (the flush by size: more rows will come); without it the whole
 * buffer is written (memory, commit, the end of a command that needs the
 * rows in the table).
 */
static void
kc_flush_buf_ex(KcRelState *st, KcWriteBuf *b, Relation rel, bool keep_tail)
{
	uint32		limit;
	uint32		done = 0;

	if (b->nrows == 0)
		return;

	if (!kc_rel_is_columnar(rel) || kc_buf_is_stale(b, rel))
	{
		/* converted to another access method, truncated or rewritten meanwhile */
		kc_buf_clear(b);
		return;
	}

	KC_FAULT(KC_FAULT_FLUSH);
	limit = kc_stripe_limit(&b->opts);
	while (done < b->nrows)
	{
		uint32		n = Min(limit, b->nrows - done);

		if (keep_tail && n < limit)
			break;
		kc_write_stripe(st, b, rel, done, n, false);
		done += n;
	}
	if (done == b->nrows)
		kc_buf_clear(b);
	else if (done > 0)
		kc_buf_drop_prefix(b, done);
}

static void
kc_flush_buf(KcRelState *st, KcWriteBuf *b, Relation rel)
{
	kc_flush_buf_ex(st, b, rel, false);
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
	kc_coalesce(st);
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

/*
 * The stripes written for rel from now on are inserted into the catalog with
 * a frozen creator: a rewrite that keeps what every snapshot sees.
 */
/*
 * While on, rows written through the buffers are copies of rows readers see
 * already (compaction): they take no part in serializable conflict checking
 * and their stripes are marked as maintenance, so that a reader whose snapshot
 * cannot see them does not report a dependency on the compacting transaction.
 * A buffer that also holds a row written for any other reason keeps both.
 */
void
kc_write_set_maintenance(bool on)
{
	kc_maintenance_write = on;
}

/*
 * rel's file is about to be replaced by this transaction (a rewrite): note the
 * file's identity.  Called under the exclusive lock the rewrite holds.  Noting
 * a file twice is the same as once; noting one that is then not replaced
 * (the command fails, or decides against it) does nothing, because the
 * cleanup compares the identity with the file the relation has by then.
 */
void
kc_write_note_replaced(Relation rel)
{
	MemoryContext old;
	KcReplaced *r;
	uint64		uid;
	ListCell   *lc;

	if (!kc_rel_is_columnar(rel))
		return;
	uid = kc_rel_ident(rel)->uid;
	foreach(lc, kc_replaced)
	{
		r = lfirst(lc);

		if (r->relid == RelationGetRelid(rel) && r->uid == uid)
			return;
	}
	old = MemoryContextSwitchTo(TopTransactionContext);
	r = palloc(sizeof(KcReplaced));
	r->relid = RelationGetRelid(rel);
	r->uid = uid;
	kc_replaced = lappend(kc_replaced, r);
	MemoryContextSwitchTo(old);
}

/*
 * Remove the stripe rows of every noted file that its table no longer has:
 * the table has another file now (a rewrite, a rewrite into another access
 * method), or is not columnar any more.  Only these files are looked at, so
 * the cost is that of what the command did and not of the number of columnar
 * tables, and two commands on different tables share no row.  A row that
 * another session deleted meanwhile is gone, which is the purpose; one it is
 * still deleting is left to it.  Needs an active snapshot.  Returns the
 * number of rows deleted.
 */
int
kc_write_cleanup_replaced(void)
{
	List	   *todo = kc_replaced;
	ListCell   *lc;
	int			n = 0;

	kc_replaced = NIL;
	foreach(lc, todo)
	{
		KcReplaced *r = lfirst(lc);
		Relation	rel = try_table_open(r->relid, AccessShareLock);
		bool		replaced = true;

		if (rel != NULL)
		{
			/* a relation this transaction did not give a new file keeps its rows */
			replaced = !kc_rel_is_columnar(rel) || kc_rel_ident(rel)->uid != r->uid;
			table_close(rel, AccessShareLock);
		}
		if (replaced)
			n += kc_catalog_delete_uid(r->uid);
	}
	list_free_deep(todo);
	return n;
}

void
kc_write_set_frozen(Relation rel, bool frozen)
{
	KcRelState *st = kc_state_get(RelationGetRelid(rel), true);

	st->frozen = frozen;
}

/*
 * Holds.  A scan of a relation that has buffered rows reads the buffers
 * directly, and for that it holds the buffers still (no flush, no reset, no
 * rows merged into them): a reference on every buffer it reads.  A buffer that
 * is held takes no more rows (kc_write_row_values starts another), so what a
 * scan reads never changes under it, and the buffers written after it began are
 * not its concern and can be flushed.  The hold is recorded here, under an id, and not only in
 * the scan, because a scan abandoned by an error never runs its end (the
 * executor is not shut down for a failed portal or a failed query).  The scan
 * also remembers the hold in a resource owner (kc_scan.c), and the owner calls
 * kc_write_hold_release when it is released, which is when the error is
 * cleaned up, so a hold cannot outlive the scan whatever way the scan ends.
 * Releasing is idempotent: a hold that is not in the table (the transaction
 * ended and its state was reset, or it was released already) is ignored.
 */
typedef struct KcHold
{
	uint64		id;				/* hash key */
	Oid			relid;
	int			n;
	KcPendingRef *refs;			/* in kc_write_cxt */
	bool		in_window;		/* taken between the two phases of a hand-over */
	bool		broken;			/* ... and the publication changed what it reads */
} KcHold;

/*
 * The visible part of a buffer: the rows [first, end) that a reader may take
 * from it.  The one definition every reader uses (kc_write_hold_acquire names
 * it in the reference a scan keeps, and kc_write_pending_fetch holds the scan
 * to it).  The rows before first are in a stripe a prepared hand-over wrote.
 */
static void
kc_buf_visible(const KcWriteBuf *b, uint32 *first, uint32 *end)
{
	*first = b->vis_from;
	*end = b->nrows;
}

static uint64 kc_next_hold = 1;

/*
 * Take the hold for a scan starting now and return its id, or 0 when the
 * relation has no buffered rows of the file's identity (nothing to hold).
 * *refs is a copy for the scan's own use, in the current memory context.
 * Everything that can fail (allocations) comes first; the counts are raised
 * last, so an error cannot leave a reference behind.
 */
uint64
kc_write_hold_acquire(Oid relid, uint64 uid, KcPendingRef **refs_out, int *n_out)
{
	KcRelState *st = kc_state_get(relid, false);
	KcPendingRef *res;
	KcPendingRef *kept;
	KcHold	   *h;
	ListCell   *lc;
	bool		found;
	uint64		id;
	int			count = 0;
	int			i = 0;

	*refs_out = NULL;
	*n_out = 0;
	if (st == NULL || st->bufs == NIL)
		return 0;
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);
		uint32		first;
		uint32		end;

		kc_buf_visible(b, &first, &end);
		if (end > first && b->uid == uid)
			count++;
	}
	if (count == 0)
		return 0;

	res = palloc(sizeof(KcPendingRef) * count);
	kept = MemoryContextAlloc(kc_write_cxt, sizeof(KcPendingRef) * count);
	if (kc_holds == NULL)
	{
		HASHCTL		ctl;

		memset(&ctl, 0, sizeof(ctl));
		ctl.keysize = sizeof(uint64);
		ctl.entrysize = sizeof(KcHold);
		ctl.hcxt = kc_write_cxt;
		kc_holds = hash_create("kingres_columnar holds", 16, &ctl,
							   HASH_ELEM | HASH_BLOBS | HASH_CONTEXT);
	}
	id = kc_next_hold++;
	h = hash_search(kc_holds, &id, HASH_ENTER, &found);

	/* no failure from here on */
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);
		uint32		first;
		uint32		end;

		kc_buf_visible(b, &first, &end);
		if (end <= first || b->uid != uid)
			continue;
		res[i].serial = b->serial;
		res[i].first = first;
		res[i].nrows = end;
		b->refs++;
		i++;
	}
	Assert(i == count);
	memcpy(kept, res, sizeof(KcPendingRef) * count);
	h->relid = relid;
	h->n = count;
	h->refs = kept;
	h->in_window = kc_handover_prepared;
	h->broken = false;
	*refs_out = res;
	*n_out = count;
	return id;
}

/*
 * Give a hold back.  A buffer that was dropped meanwhile (its subtransaction
 * aborted) is simply not found.
 */
void
kc_write_hold_release(uint64 id)
{
	KcHold	   *h;
	KcRelState *st;
	int			i;

	if (kc_holds == NULL)
		return;
	h = hash_search(kc_holds, &id, HASH_FIND, NULL);
	if (h == NULL)
		return;
	st = kc_state_get(h->relid, false);
	if (st != NULL)
	{
		for (i = 0; i < h->n; i++)
		{
			ListCell   *lc;

			foreach(lc, st->bufs)
			{
				KcWriteBuf *b = lfirst(lc);

				if (b->serial == h->refs[i].serial)
				{
					if (b->refs > 0)
						b->refs--;
					break;
				}
			}
		}
	}
	pfree(h->refs);
	hash_search(kc_holds, &id, HASH_REMOVE, NULL);
}

/*
 * A scan that began between the two phases of a hand-over reads buffers that
 * the second phase rearranges: it cannot go on afterwards.  Reading in that
 * window to the end (a query of a callback) is the use that matters, and a
 * cursor that outlives it gets this error rather than other rows than it
 * began with.
 */
void
kc_write_hold_check(uint64 id, Relation rel)
{
	KcHold	   *h;

	if (!kc_broken_any || kc_holds == NULL)
		return;
	h = hash_search(kc_holds, &id, HASH_FIND, NULL);
	if (h != NULL && h->broken)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("the scan of columnar table \"%s\" cannot go on: it began while a subtransaction was being committed",
						RelationGetRelationName(rel))));
}

/* The second phase of a hand-over is done: holds taken before it cannot go on. */
static void
kc_holds_break_window(void)
{
	HASH_SEQ_STATUS seq;
	KcHold	   *h;

	if (kc_holds == NULL)
		return;
	hash_seq_init(&seq, kc_holds);
	while ((h = hash_seq_search(&seq)) != NULL)
	{
		if (h->in_window)
		{
			h->in_window = false;
			h->broken = true;
			kc_broken_any = true;
		}
	}
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
		if (rowno < ref->first || rowno >= ref->nrows || rowno >= b->nrows)
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

		kc_coalesce(st);
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
			if (kc_replaced != NIL)
			{
				/* the catalog deletes want a snapshot; none is active at pre-commit */
				bool		pushed = false;

				if (!ActiveSnapshotSet())
				{
					PushActiveSnapshot(GetTransactionSnapshot());
					pushed = true;
				}
				(void) kc_write_cleanup_replaced();
				if (pushed)
					PopActiveSnapshot();
			}
			break;
		case XACT_EVENT_COMMIT:
		case XACT_EVENT_PREPARE:
		case XACT_EVENT_ABORT:
			kc_replaced = NIL;
			kc_state_reset();
			break;
		default:
			break;
	}
}

/* ------------------------------------------------------------------------
 * The hand-over of a committing subtransaction's buffers to its parent
 *
 * Invariant: the rows of a subtransaction become rows of its parent for every
 * relation or for none.  Phase one (PRE_COMMIT_SUB, kc_subxact_prepare) does
 * everything that can fail for every relation of the subtransaction (copies
 * the rows into room reserved in the parent's buffer, and, when the parent's
 * buffer is full or the memory allowance is used up, writes it as stripes) and
 * publishes nothing: the parent's row counts, byte counts and buffers are as
 * they were.  An error in any relation, or later in the commit of the
 * subtransaction, aborts it, and ABORT_SUB drops what phase one made.  Phase
 * two (COMMIT_SUB, kc_subxact_publish) only assigns counts and pointers and
 * frees memory: nothing in it allocates, reads a file or can throw.
 *
 * A stripe written in phase one carries the transaction id of the committing
 * subtransaction, though it holds rows of the parent (and of children that
 * committed into it earlier).  That is right, and only because the buffer
 * keeps those rows until phase two: if the subtransaction aborts after the
 * stripe was written, the stripe's id aborts with it and the rows are still in
 * the parent's buffer; if it commits, its id is a committed child of the
 * parent and the stripe stands or falls with the parent, as the rows do.
 * Only buffers of the parent itself may be written this way: those of a
 * grandparent would be lost with the parent's abort.
 *
 * Between the phases the stripe and the buffers both hold those rows.  A
 * reader in that window (a callback of another extension that runs after this
 * one) must see each row once, so the rows of a buffer that are in a stripe
 * are not part of its visible part (vis_from, kc_buf_visible); a write there
 * is refused (kc_handover_prepared).  A scan that began in the window is
 * stopped when phase two has rearranged the buffers it reads
 * (kc_write_hold_check).
 * ------------------------------------------------------------------------
 */

/*
 * Is b, a committing subtransaction's buffer, to be merged into prev?  Neither
 * buffer may be one that a scan reads by position (a scan keeps the rows it
 * began with, and the merged rows would have to move or be written under
 * it); b then stays a buffer of its own and becomes the parent's.
 */
static bool
kc_can_merge(KcWriteBuf *prev, KcWriteBuf *b, SubTransactionId parentSubid)
{
	return prev != NULL && prev->subid == parentSubid &&
		prev->ncols == b->ncols && prev->uid == b->uid &&
		b->refs == 0 && prev->refs == 0;
}

/*
 * Phase one for one buffer: copy the rows of b into the slots of prev past
 * its row count, which no reader and no flush looks at.  The slots are
 * reserved first (kc_buf_reserve either succeeds or leaves prev as it was),
 * and the by-reference values go into one allocation made before any is
 * copied, so the copying itself cannot fail.  Nothing of prev that anyone can
 * see changes: its capacity may have grown, which is no change.
 */
static void
kc_buf_merge_prepare(KcWriteBuf *prev, KcWriteBuf *b)
{
	Size		chunk_bytes = 0;
	Size		off = 0;
	uint32		base = prev->nrows;
	char	   *chunk = NULL;
	uint32		r;
	int			c;

	KC_FAULT(KC_FAULT_MERGE);
	for (r = 0; r < b->nrows; r++)
		for (c = 0; c < b->ncols; c++)
			if (!b->nulls[(Size) r * b->ncols + c])
				chunk_bytes += MAXALIGN((Size) b->sizes[(Size) r * b->ncols + c]);

	kc_buf_reserve(prev, b->nrows);
	if (chunk_bytes > 0)
	{
		KC_FAULT(KC_FAULT_ALLOC);
		chunk = MemoryContextAllocHuge(prev->cxt, chunk_bytes);
	}

	/* no failure from here on until the publish */
	b->merge_into = prev;
	b->merge_base = base;
	b->merge_rows = b->nrows;
	b->merge_chunk = chunk;
	b->merge_charge = chunk ? GetMemoryChunkSpace(chunk) : 0;
	b->merge_written = 0;
	for (r = 0; r < b->nrows; r++)
	{
		Size		src = (Size) r * b->ncols;
		Size		dst = (Size) (base + r) * prev->ncols;

		for (c = 0; c < prev->ncols; c++)
		{
			Datum		d = b->vals[src + c];
			uint32		sz = b->sizes[src + c];

			if (!b->nulls[src + c] && sz > 0)
			{
				memcpy(chunk + off, DatumGetPointer(d), sz);
				d = PointerGetDatum(chunk + off);
				off += MAXALIGN((Size) sz);
			}
			prev->vals[dst + c] = d;
			prev->nulls[dst + c] = b->nulls[src + c];
			prev->sizes[dst + c] = sz;
		}
		prev->cids[base + r] = b->cids[r];
	}
}

/*
 * Write rows [b->merge_written, upto) of the rows a prepared hand-over holds
 * (prev's slots [0, prev->nrows + b->merge_rows): the parent's, then the
 * child's) as stripes, and make the rows from upto on the tail that phase two
 * installs.  This is the part of a hand-over that writes: because the parent
 * cannot flush while the child runs, a committing child writes the stripes
 * that the parent's buffer, now larger, calls for, so that the stripes of a
 * table do not depend on how its rows were grouped into subtransactions.
 * It can be called again for the same pair with a larger upto (memory
 * pressure after the whole stripes); the tail it replaces is dropped only
 * when the new one is built, so an error leaves the earlier state.  The rows
 * written are then hidden from readers of the buffers (vis_from).
 */
static void
kc_merge_flush_prepare(KcRelState *st, KcWriteBuf *prev, KcWriteBuf *b,
					   uint32 upto)
{
	uint32		total = prev->nrows + b->merge_rows;
	uint32		limit = kc_stripe_limit(&prev->opts);
	uint32		done = b->merge_written;
	Relation	rel;
	bool		pushed = false;
	KcTail		tail;

	Assert(b->merge_into == prev);
	if (upto <= done)
		return;

	/* dropped in this transaction: the rows are discarded with the buffer */
	rel = try_table_open(prev->relid, RowExclusiveLock);
	if (rel == NULL)
		return;
	if (!kc_rel_is_columnar(rel) || kc_buf_is_stale(prev, rel))
	{
		table_close(rel, NoLock);
		return;
	}
	if (!ActiveSnapshotSet())
	{
		PushActiveSnapshot(GetTransactionSnapshot());
		pushed = true;
	}
	KC_FAULT(KC_FAULT_FLUSH);
	while (done < upto)
	{
		uint32		n = Min(limit, upto - done);

		kc_write_stripe(st, prev, rel, done, n, false);
		done += n;
	}
	table_close(rel, NoLock);
	if (pushed)
		PopActiveSnapshot();

	/*
	 * The stripes are written; whatever happens next, the rows in them must
	 * not be read from the buffers as well.
	 */
	prev->vis_from = Min(upto, prev->nrows);
	b->vis_from = upto > prev->nrows ? upto - prev->nrows : 0;
	b->merge_written = upto;

	kc_buf_tail_build(prev, upto, total, &tail);
	if (b->tail_ready)
		MemoryContextDelete(b->tail.cxt);
	b->tail = tail;
	b->tail_ready = true;
}

/* Phase two for one buffer: the rows are the parent's now.  Cannot fail. */
static void
kc_buf_merge_publish(KcWriteBuf *b)
{
	KcWriteBuf *prev = b->merge_into;

	if (b->tail_ready)
	{
		/* the parent's rows and the child's, minus what went into stripes */
		kc_total_bytes -= Min(kc_total_bytes, b->bytes);
		b->bytes = 0;
		kc_buf_tail_install(prev, &b->tail);
		b->tail_ready = false;
	}
	else
	{
		prev->nrows = b->merge_base + b->merge_rows;
		kc_charge(prev, (int64) b->merge_charge);
		kc_total_bytes -= Min(kc_total_bytes, b->bytes);
		b->bytes = 0;
	}
	/* the values live in the parent's memory now */
	b->merge_chunk = NULL;
	b->merge_into = NULL;
}

static void
kc_buf_free(KcWriteBuf *b)
{
	/* a hand-over that was prepared and not published: take it back */
	if (b->tail_ready)
		MemoryContextDelete(b->tail.cxt);
	if (b->merge_chunk != NULL)
		pfree(b->merge_chunk);
	kc_buf_clear(b);
	MemoryContextDelete(b->cxt);
	pfree(b);
}

/*
 * Buffers of one relation that belong to the same subtransaction and are not
 * read by a scan become one: the buffers a scan made another buffer necessary
 * for are joined again when rows have to be written, so that the stripes come
 * out as they would have without the scan.  Rows keep their order.  Can fail
 * (memory), before anything is changed for the pair it works on.
 */
static void
kc_coalesce(KcRelState *st)
{
	ListCell   *lc;
	KcWriteBuf *prev = NULL;

	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);

		if (prev != NULL && b->subid == prev->subid && b->ncols == prev->ncols &&
			b->uid == prev->uid && b->refs == 0 && prev->refs == 0 &&
			b->merge_into == NULL && prev->vis_from == 0)
		{
			if (b->nrows > 0)
			{
				kc_buf_merge_prepare(prev, b);
				kc_buf_merge_publish(b);
			}
			st->bufs = foreach_delete_current(st->bufs, lc);
			kc_buf_free(b);
			continue;
		}
		prev = b;
	}
}

/*
 * A buffer of the committing subtransaction that is a real part of the
 * allowance is written by the subtransaction itself, as its own flush: its
 * rows are its own, and a copy of them into the parent's buffer would hold
 * them twice for the length of the commit.
 */
static void
kc_flush_own_buffer(KcRelState *st, KcWriteBuf *b)
{
	Relation	rel;
	bool		pushed = false;

	rel = try_table_open(b->relid, RowExclusiveLock);
	if (rel == NULL)
	{
		kc_buf_clear(b);
		return;
	}
	if (!ActiveSnapshotSet())
	{
		PushActiveSnapshot(GetTransactionSnapshot());
		pushed = true;
	}
	kc_flush_buf(st, b, rel);
	table_close(rel, NoLock);
	if (pushed)
		PopActiveSnapshot();
}

/* One buffer handed over: the child's, the parent's it is merged into. */
typedef struct KcPair
{
	KcRelState *st;
	KcWriteBuf *prev;
	KcWriteBuf *b;
} KcPair;

/* Memory a prepared pair holds once phase two is done. */
static Size
kc_pair_after(const KcPair *p)
{
	return p->b->tail_ready ? p->b->tail.bytes : p->prev->bytes + p->b->merge_charge;
}

/* Rows a prepared pair holds once phase two is done. */
static uint32
kc_pair_rows_after(const KcPair *p)
{
	return p->prev->nrows + p->b->merge_rows - p->b->merge_written;
}

/*
 * Phase one for the whole subtransaction: every relation, all or nothing.
 *
 * First every buffer is made ready: a big one is written by the child (its own
 * flush), a small one is copied into its parent's buffer, which is written as
 * whole stripes when it holds that many rows.  Then, if the buffers are still
 * over the memory allowance, the merged buffers are written, the largest
 * first, until they are within it, by the same rule as kc_flush_pressure: a
 * merged buffer that is not worth a stripe by itself is written only while the
 * merged buffers together are half of the allowance.  The rule makes the
 * memory of a loop of subtransactions, whatever the width of its rows,
 * independent of the number of iterations.
 */
static void
kc_subxact_prepare(SubTransactionId mySubid, SubTransactionId parentSubid)
{
	HASH_SEQ_STATUS seq;
	KcRelState *st;
	KcPair		stackpairs[8];
	KcPair	   *pairs = stackpairs;
	int			npairs = 0;
	int			cap = lengthof(stackpairs);
	Size		allowance = kc_allowance();
	Size		effective;
	int			i;

	hash_seq_init(&seq, kc_states);
	while ((st = hash_seq_search(&seq)) != NULL)
	{
		KcWriteBuf *prev = NULL;
		bool		busy = false;	/* prev already takes a buffer of this child */
		ListCell   *lc;

		foreach(lc, st->bufs)
		{
			KcWriteBuf *b = lfirst(lc);

			if (b->subid != mySubid)
			{
				prev = b;
				busy = false;
				continue;
			}
			if (!busy && kc_can_merge(prev, b, parentSubid))
			{
				if (b->nrows == 0)
					continue;
				if (kc_total_bytes + b->bytes > allowance && kc_worth_flushing(b))
				{
					kc_flush_own_buffer(st, b);
					continue;
				}
				kc_buf_merge_prepare(prev, b);
				busy = true;
				if (npairs == cap)
				{
					/*
					 * Not in the current context: it is the CurTransactionContext of
					 * the committing subtransaction, which PostgreSQL keeps (8 kB a
					 * subtransaction, until the top-level transaction ends) as soon as
					 * anything has been allocated in it, even if it is freed again.
					 */
					cap *= 2;
					if (pairs == stackpairs)
					{
						pairs = MemoryContextAlloc(kc_write_cxt, sizeof(KcPair) * cap);
						memcpy(pairs, stackpairs, sizeof(stackpairs));
					}
					else
						pairs = repalloc(pairs, sizeof(KcPair) * cap);
				}
				pairs[npairs].st = st;
				pairs[npairs].prev = prev;
				pairs[npairs].b = b;
				npairs++;
				{
					uint32		total = prev->nrows + b->nrows;
					uint32		limit = kc_stripe_limit(&prev->opts);

					if (total - total % limit > 0)
						kc_merge_flush_prepare(st, prev, b, total - total % limit);
				}
			}
			else
			{
				prev = b;
				busy = false;
			}
		}
	}

	/* what the memory would be once phase two has installed the tails */
	effective = kc_total_bytes;
	for (i = 0; i < npairs; i++)
	{
		Size		before = pairs[i].prev->bytes + pairs[i].b->bytes;
		Size		after = kc_pair_after(&pairs[i]);

		effective -= Min(effective, before > after ? before - after : 0);
	}
	while (effective > allowance)
	{
		int			best = -1;
		Size		together = 0;

		for (i = 0; i < npairs; i++)
		{
			if (kc_pair_rows_after(&pairs[i]) == 0)
				continue;
			together += kc_pair_after(&pairs[i]);
			if (best < 0 || kc_pair_after(&pairs[i]) > kc_pair_after(&pairs[best]))
				best = i;
		}
		if (best < 0)
			break;
		if (kc_pair_rows_after(&pairs[best]) < kc_min_flush_rows(&pairs[best].prev->opts) &&
			kc_pair_after(&pairs[best]) < kc_bytes_worth() && together < kc_bytes_worth())
			break;
		{
			KcPair	   *p = &pairs[best];
			Size		was = kc_pair_after(p);
			Size		now;

			kc_merge_flush_prepare(p->st, p->prev, p->b,
								   p->prev->nrows + p->b->merge_rows);
			now = kc_pair_after(p);
			effective -= Min(effective, was > now ? was - now : 0);
			if (kc_pair_rows_after(p) != 0)
				break;			/* not written (dropped or converted): no progress */
		}
	}
	if (pairs != stackpairs)
		pfree(pairs);
	if (npairs > 0)
		kc_handover_prepared = true;
}

/* Phase two for the whole subtransaction.  Cannot fail. */
static void
kc_subxact_publish(SubTransactionId mySubid, SubTransactionId parentSubid)
{
	HASH_SEQ_STATUS seq;
	KcRelState *st;

	hash_seq_init(&seq, kc_states);
	while ((st = hash_seq_search(&seq)) != NULL)
	{
		KcWriteBuf *prev = NULL;
		ListCell   *lc;

		/* the list is edited in place: it is not rebuilt for every commit */
		foreach(lc, st->bufs)
		{
			KcWriteBuf *b = lfirst(lc);

			if (b->subid != mySubid)
			{
				prev = b;
				continue;
			}

			/*
			 * The rows now belong to the parent subtransaction.  A buffer that
			 * phase one prepared is merged into the parent's (a scan that
			 * began in between is stopped, see kc_write_hold_check); an empty
			 * one is dropped; any other keeps its identity, because a scan
			 * names it by serial and row count, and is only handed over.
			 */
			if (b->merge_into != NULL)
			{
				Assert(b->merge_into == prev && b->merge_rows == b->nrows &&
					   b->merge_base == prev->nrows);
				kc_buf_merge_publish(b);
				st->bufs = foreach_delete_current(st->bufs, lc);
				kc_buf_free(b);
			}
			else if (b->nrows == 0 && prev != NULL && prev->subid == parentSubid &&
					 prev->ncols == b->ncols && prev->uid == b->uid)
			{
				st->bufs = foreach_delete_current(st->bufs, lc);
				kc_buf_free(b);
			}
			else
			{
				b->subid = parentSubid;
				prev = b;
			}
		}
	}
	if (kc_handover_prepared)
	{
		kc_holds_break_window();
		kc_handover_prepared = false;
	}
}

static void
kc_subxact_abort(SubTransactionId mySubid)
{
	HASH_SEQ_STATUS seq;
	KcRelState *st;

	hash_seq_init(&seq, kc_states);
	while ((st = hash_seq_search(&seq)) != NULL)
	{
		ListCell   *lc;

		foreach(lc, st->bufs)
		{
			KcWriteBuf *b = lfirst(lc);

			if (b->subid == mySubid)
			{
				/* the stripes of a prepared hand-over go with the subtransaction */
				if (b->merge_into != NULL)
					b->merge_into->vis_from = 0;
				st->bufs = foreach_delete_current(st->bufs, lc);
				kc_buf_free(b);
			}
		}
	}
	if (kc_handover_prepared)
	{
		kc_holds_break_window();
		kc_handover_prepared = false;
	}
}

static void
kc_subxact_callback(SubXactEvent event, SubTransactionId mySubid,
					SubTransactionId parentSubid, void *arg)
{
	if (kc_states == NULL)
		return;
	switch (event)
	{
		case SUBXACT_EVENT_PRE_COMMIT_SUB:
			kc_subxact_prepare(mySubid, parentSubid);
			break;
		case SUBXACT_EVENT_COMMIT_SUB:
			kc_subxact_publish(mySubid, parentSubid);
			break;
		case SUBXACT_EVENT_ABORT_SUB:
			kc_subxact_abort(mySubid);
			break;
		default:
			break;
	}
}

void
kc_write_init(void)
{
	RegisterXactCallback(kc_xact_callback, NULL);
	RegisterSubXactCallback(kc_subxact_callback, NULL);

#ifdef USE_ASSERT_CHECKING
	DefineCustomEnumVariable("kingres_columnar.fault_point",
							 "Testing: where the write path fails once (assertion builds only).",
							 NULL, &kc_fault_point, KC_FAULT_OFF,
							 kc_fault_points, PGC_SUSET, GUC_NOT_IN_SAMPLE,
							 NULL, kc_fault_assign_point, NULL);
	DefineCustomIntVariable("kingres_columnar.fault_nth",
							"Testing: the number of the hit at fault_point that fails.",
							NULL, &kc_fault_nth, 0, 0, INT_MAX,
							PGC_SUSET, GUC_NOT_IN_SAMPLE,
							NULL, kc_fault_assign_nth, NULL);
#endif
}
