/*-------------------------------------------------------------------------
 *
 * kc_scan.c
 *	  The serial scan of a columnar table, shared by the table access method
 *	  (every plain scan, ANALYZE, COPY TO, rewrites) and by the custom scan
 *	  (projection and skipping).
 *
 * A scan reads, in this order: the stripes whose catalog rows the snapshot
 * sees, and then the rows the running transaction has written and not yet
 * flushed.  For a stripe the running transaction wrote itself, only the rows
 * written by earlier commands are returned (a stripe's cid_points say where
 * the command id changes); the same rule applies to buffered rows.  So
 * INSERT INTO t SELECT FROM t does not read its own output.
 *
 * A stripe is read footer first; a chunk group is decoded whole, for the
 * needed columns only, when it is entered.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kc_scan.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "access/xact.h"
#include "storage/bufmgr.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"

#include "kingres_columnar.h"

/* ------------------------------------------------------------------------
 * Set-up
 * ------------------------------------------------------------------------
 */

/* How many of the stripe's rows may a scan of command curcid read? */
static uint32
kc_visible_rows(const KcStripeInfo *si, CommandId curcid, bool all_rows)
{
	const char *p;
	Size		n;
	Size		i;

	if (!si->own || all_rows)
		return si->row_count;
	p = VARDATA_ANY(si->cid_points);
	n = VARSIZE_ANY_EXHDR(si->cid_points) / 8;
	for (i = 0; i < n; i++)
	{
		uint32		pair[2];

		memcpy(pair, p + i * 8, 8);
		if (pair[1] >= (uint32) curcid)
			return Min(pair[0], si->row_count);
	}
	return si->row_count;
}

KcScan *
kc_scan_begin(Relation rel, Snapshot snapshot, const bool *needed,
			  struct KcSkip *skip, int flags)
{
	KcScan	   *scan;
	MemoryContext cxt;
	MemoryContext old;
	bool		mvcc;
	Snapshot	cs;
	int			i;
	int			nvisible;

	cxt = AllocSetContextCreate(CurrentMemoryContext, "kingres_columnar scan",
								ALLOCSET_DEFAULT_SIZES);
	old = MemoryContextSwitchTo(cxt);
	scan = palloc0(sizeof(KcScan));
	scan->scan_cxt = cxt;
	scan->base.rs_rd = rel;
	scan->base.rs_snapshot = snapshot;
	scan->base.rs_flags = flags;
	scan->rel = rel;
	scan->tupdesc = RelationGetDescr(rel);
	scan->natts = scan->tupdesc->natts;
	scan->ti = kc_typeinfo_build(scan->tupdesc, cxt);
	scan->skip = skip;
	scan->group_cxt = AllocSetContextCreate(cxt, "kingres_columnar group",
											ALLOCSET_DEFAULT_SIZES);
	scan->an_stripe = -1;
	scan->an_group = -1;

	scan->needed = palloc(sizeof(bool) * (scan->natts + 1));
	for (i = 0; i < scan->natts; i++)
		scan->needed[i] = !scan->ti->cols[i].dropped &&
			(needed == NULL || needed[i]);

	cs = kc_catalog_snapshot(snapshot, &mvcc);
	scan->cat_snap = cs;
	scan->all_own_rows = !mvcc;
	scan->curcid = mvcc ? snapshot->curcid : InvalidCommandId;

	scan->stripes = kc_catalog_load(rel, cs, &scan->nstripes);
	nvisible = 0;
	for (i = 0; i < scan->nstripes; i++)
	{
		KcStripeInfo *si = &scan->stripes[i];

		si->visible_rows = kc_visible_rows(si, scan->curcid, scan->all_own_rows);
		if (si->visible_rows > 0)
			scan->stripes[nvisible++] = *si;
	}
	scan->nstripes = nvisible;

	/*
	 * The rows the transaction buffered, pinned so that no flush empties the
	 * buffer under the scan.  An analyze scan has no use for them.
	 */
	if (!(flags & SO_TYPE_ANALYZE))
	{
		scan->pend = kc_write_pending_refs(RelationGetRelid(rel), &scan->npend);
		if (scan->npend > 0)
		{
			kc_write_pin_relation(RelationGetRelid(rel), 1);
			scan->pinned = true;
		}
	}
	MemoryContextSwitchTo(old);
	return scan;
}

void
kc_scan_rescan(KcScan *scan)
{
	if (scan->in_stripe)
	{
		kc_footer_free(&scan->footer);
		scan->in_stripe = false;
	}
	scan->stripe_idx = 0;
	scan->have_group = false;
	scan->pend_idx = 0;
	scan->pend_row = 0;
	scan->ordinal = 0;
	scan->hit_end = false;
	MemoryContextReset(scan->group_cxt);
}

void
kc_scan_end(KcScan *scan)
{
	if (scan->pinned)
		kc_write_pin_relation(RelationGetRelid(scan->rel), -1);
	MemoryContextDelete(scan->scan_cxt);
}

/* ------------------------------------------------------------------------
 * Chunk groups
 * ------------------------------------------------------------------------
 */

/*
 * The value of a column that a stripe written before the column existed
 * does not have: the column's default as of ADD COLUMN, or null.
 */
static void
kc_missing_value(KcScan *scan, int c, Datum *val, bool *isnull)
{
	if (TupleDescAttr(scan->tupdesc, c)->attisdropped)
	{
		*val = (Datum) 0;
		*isnull = true;
		return;
	}
	*val = getmissingattr(scan->tupdesc, c + 1, isnull);
}

/*
 * Decode chunk group g of the open stripe into colvals/colnulls (the needed
 * columns only).  Returns its row count.
 */
static uint32
kc_load_group(KcScan *scan, uint32 g)
{
	KcStripeInfo *si = scan->cur;
	uint32		rows = scan->footer.group_rows[g];
	int			c;

	MemoryContextReset(scan->group_cxt);
	scan->colvals = MemoryContextAllocZero(scan->group_cxt, sizeof(Datum *) * scan->natts);
	scan->colnulls = MemoryContextAllocZero(scan->group_cxt, sizeof(bool *) * scan->natts);

	for (c = 0; c < scan->natts; c++)
	{
		if (!scan->needed[c])
			continue;
		if (c >= si->natts)
		{
			/* column added after the stripe: its default for every row */
			Datum		v;
			bool		isnull;
			uint32		r;

			kc_missing_value(scan, c, &v, &isnull);
			scan->colvals[c] = MemoryContextAlloc(scan->group_cxt, sizeof(Datum) * rows);
			scan->colnulls[c] = MemoryContextAlloc(scan->group_cxt, rows);
			for (r = 0; r < rows; r++)
			{
				scan->colvals[c][r] = v;
				scan->colnulls[c][r] = isnull;
			}
		}
		else
		{
			const KcChunkDisk *cd = &scan->footer.chunks[(Size) g * scan->footer.ncols + c];
			char	   *comp;

			comp = MemoryContextAllocHuge(scan->group_cxt, cd->comp_len ? cd->comp_len : 1);
			kc_stripe_read(scan->rel, si, cd->off, cd->comp_len, comp);
			scan->stats.bytes_read += cd->comp_len;
			scan->colvals[c] = MemoryContextAlloc(scan->group_cxt, sizeof(Datum) * rows);
			scan->colnulls[c] = MemoryContextAlloc(scan->group_cxt, rows);
			kc_decode_chunk(&scan->ti->cols[c], comp, cd, rows,
							scan->colvals[c], scan->colnulls[c],
							scan->group_cxt, RelationGetRelationName(scan->rel),
							si->stripe_id);
			pfree(comp);
		}
	}
	return rows;
}

/* The statistics of group g of the open stripe, for the skipping test. */
static void
kc_group_stats(KcScan *scan, uint32 g, KcColStat *out)
{
	int			c;
	KcStripeInfo *si = scan->cur;

	for (c = 0; c < scan->natts; c++)
	{
		memset(&out[c], 0, sizeof(KcColStat));
		if (c >= si->natts)
			continue;			/* unknown */
		{
			const KcChunkDisk *cd = &scan->footer.chunks[(Size) g * scan->footer.ncols + c];

			out[c].known = true;
			out[c].nnulls = cd->nnulls;
			out[c].has_minmax = (cd->flags & KC_CHUNK_HAS_MINMAX) != 0;
			out[c].min_len = cd->min_len;
			out[c].max_len = cd->max_len;
			out[c].min = scan->footer.mins[(Size) g * scan->footer.ncols + c];
			out[c].max = scan->footer.maxs[(Size) g * scan->footer.ncols + c];
		}
	}
}

/* ------------------------------------------------------------------------
 * Returning rows
 * ------------------------------------------------------------------------
 */

static bool
kc_open_next_stripe(KcScan *scan)
{
	while (scan->stripe_idx < scan->nstripes)
	{
		KcStripeInfo *si = &scan->stripes[scan->stripe_idx++];

		scan->stats.stripes_total++;
		if (scan->skip && kc_guc_enable_chunk_skipping && kc_skip_active(scan->skip))
		{
			KcColStat  *st = palloc(sizeof(KcColStat) * (scan->natts + 1));
			bool		skipit;

			kc_stripe_stats_parse(si, scan->natts, st);
			skipit = kc_skip_unit(scan->skip, st, si->row_count);
			pfree(st);
			if (skipit)
			{
				scan->stats.stripes_skipped++;
				/* the groups of a skipped stripe count as skipped too */
				scan->stats.groups_total += si->chunk_groups;
				scan->stats.groups_skipped += si->chunk_groups;
				continue;
			}
		}
		scan->cur = si;
		kc_footer_load(scan->rel, si, &scan->footer);
		scan->in_stripe = true;
		scan->group_idx = 0;
		scan->stripe_row_pos = 0;
		return true;
	}
	return false;
}

static bool
kc_open_next_group(KcScan *scan)
{
	while (scan->group_idx < (int) scan->footer.ngroups)
	{
		uint32		g = scan->group_idx++;
		uint32		rows = scan->footer.group_rows[g];
		uint32		start = scan->stripe_row_pos;

		scan->stripe_row_pos += rows;
		if (start >= scan->cur->visible_rows)
			return false;		/* the rest of the stripe is not visible */
		scan->stats.groups_total++;
		if (scan->skip && kc_guc_enable_chunk_skipping && kc_skip_active(scan->skip))
		{
			KcColStat  *st = palloc(sizeof(KcColStat) * (scan->natts + 1));
			bool		skipit;

			kc_group_stats(scan, g, st);
			skipit = kc_skip_unit(scan->skip, st, rows);
			pfree(st);
			if (skipit)
			{
				scan->stats.groups_skipped++;
				continue;
			}
		}
		scan->group_nrows = Min(rows, scan->cur->visible_rows - start);
		scan->group_start = start;
		(void) kc_load_group(scan, g);
		scan->group_pos = 0;
		scan->have_group = true;
		return true;
	}
	return false;
}

static void
kc_store_group_row(KcScan *scan, TupleTableSlot *slot)
{
	int			c;

	ExecClearTuple(slot);
	for (c = 0; c < scan->natts; c++)
	{
		if (scan->needed[c] && scan->colvals[c] != NULL)
		{
			slot->tts_values[c] = scan->colvals[c][scan->group_pos];
			slot->tts_isnull[c] = scan->colnulls[c][scan->group_pos];
		}
		else
		{
			slot->tts_values[c] = (Datum) 0;
			slot->tts_isnull[c] = true;
		}
	}
	/*
	 * A TID from the row number (design 5.3): ANALYZE sorts its sample by it
	 * and the executor asserts it is valid.  Nothing looks a row up by it.
	 */
	{
		uint64		row = scan->cur->first_row + scan->group_start + scan->group_pos;

		ItemPointerSet(&slot->tts_tid, (BlockNumber) (row / MaxOffsetNumber),
					   (OffsetNumber) (row % MaxOffsetNumber) + 1);
	}
	slot->tts_tableOid = RelationGetRelid(scan->rel);
	scan->group_pos++;
	ExecStoreVirtualTuple(slot);
}

static bool
kc_next_pending(KcScan *scan, TupleTableSlot *slot)
{
	while (scan->pend_idx < scan->npend)
	{
		KcPendingRef *ref = &scan->pend[scan->pend_idx];
		Datum	   *vals;
		bool	   *nulls;
		int			ncols;
		CommandId	cid;
		int			c;

		if (!kc_write_pending_fetch(RelationGetRelid(scan->rel), ref,
									scan->pend_row, &vals, &nulls, &ncols, &cid) ||
			(!scan->all_own_rows && cid >= scan->curcid))
		{
			/* command ids never decrease in a buffer: this one is done */
			scan->pend_idx++;
			scan->pend_row = 0;
			continue;
		}
		ExecClearTuple(slot);
		for (c = 0; c < scan->natts; c++)
		{
			if (c < ncols)
			{
				slot->tts_values[c] = vals[c];
				slot->tts_isnull[c] = nulls[c];
			}
			else
				kc_missing_value(scan, c, &slot->tts_values[c], &slot->tts_isnull[c]);
		}
		ItemPointerSet(&slot->tts_tid, MaxBlockNumber - 1,
					   (OffsetNumber) (scan->pend_row % MaxOffsetNumber) + 1);
		slot->tts_tableOid = RelationGetRelid(scan->rel);
		scan->pend_row++;
		ExecStoreVirtualTuple(slot);
		return true;
	}
	return false;
}

/*
 * The next row into slot; false at the end.  Everything the scan allocates
 * lives in its own contexts, whatever the caller's current one is (the
 * caller may be running in a per-tuple context that is reset between rows).
 */
static bool
kc_scan_next_inner(KcScan *scan, TupleTableSlot *slot)
{
	for (;;)
	{
		if (scan->have_group)
		{
			if (scan->group_pos < scan->group_nrows)
			{
				kc_store_group_row(scan, slot);
				return true;
			}
			scan->have_group = false;
		}
		if (scan->in_stripe)
		{
			if (kc_open_next_group(scan))
				continue;
			kc_footer_free(&scan->footer);
			scan->in_stripe = false;
			continue;
		}
		if (kc_open_next_stripe(scan))
			continue;
		break;
	}
	return kc_next_pending(scan, slot);
}

bool
kc_scan_next(KcScan *scan, TupleTableSlot *slot)
{
	MemoryContext old;
	bool		res;

	old = MemoryContextSwitchTo(scan->scan_cxt);

	/*
	 * A table rewrite (ALTER COLUMN TYPE, ADD COLUMN with a volatile default)
	 * scans with the descriptor the table had before the change, which is the
	 * one the stripes were written with; the relation already carries the new
	 * one.  Follow the slot.
	 */
	if (slot->tts_tupleDescriptor != scan->tupdesc && !scan->have_group &&
		!scan->in_stripe)
	{
		int			i;

		scan->tupdesc = slot->tts_tupleDescriptor;
		scan->natts = scan->tupdesc->natts;
		scan->ti = kc_typeinfo_build(scan->tupdesc, scan->scan_cxt);
		scan->needed = palloc(sizeof(bool) * (scan->natts + 1));
		for (i = 0; i < scan->natts; i++)
			scan->needed[i] = !scan->ti->cols[i].dropped;
	}

	res = kc_scan_next_inner(scan, slot);
	MemoryContextSwitchTo(old);
	return res;
}

/* ------------------------------------------------------------------------
 * ANALYZE
 * ------------------------------------------------------------------------
 */

/*
 * The sampler picked the block in buf.  A block of a stripe stands for the
 * rows in the same relative position of the stripe: page j of P pages holds
 * the rows from R*j/P up to R*(j+1)/P.  The metapage, pages of no stripe the
 * snapshot sees, and pages that do not look like stripe pages stand for no
 * rows.  (The design's modulo scheme would force every sampled page to
 * decode every chunk group of its stripe; contiguous ranges let consecutive
 * sampled pages share the decoded group.)
 */
bool
kc_scan_analyze_block(KcScan *scan, Buffer buf)
{
	Page		page;
	KcPageSpecial *sp;
	int			lo,
				hi;
	uint64		R;
	uint64		P;
	uint64		j;

	scan->an_have = false;
	LockBuffer(buf, BUFFER_LOCK_SHARE);
	page = BufferGetPage(buf);
	if (PageIsNew(page) || PageGetSpecialSize(page) != sizeof(KcPageSpecial))
	{
		LockBuffer(buf, BUFFER_LOCK_UNLOCK);
		return false;
	}
	sp = (KcPageSpecial *) PageGetSpecialPointer(page);
	if (sp->magic != KC_PAGE_MAGIC || sp->page_count == 0 ||
		sp->page_no >= sp->page_count)
	{
		LockBuffer(buf, BUFFER_LOCK_UNLOCK);
		return false;
	}
	{
		uint64		sid = sp->stripe_id;

		j = sp->page_no;
		P = sp->page_count;
		LockBuffer(buf, BUFFER_LOCK_UNLOCK);

		/* stripes are in stripe id order */
		lo = 0;
		hi = scan->nstripes - 1;
		while (lo <= hi)
		{
			int			mid = (lo + hi) / 2;

			if (scan->stripes[mid].stripe_id == sid)
			{
				lo = mid;
				break;
			}
			if (scan->stripes[mid].stripe_id < sid)
				lo = mid + 1;
			else
				hi = mid - 1;
		}
		if (lo > hi)
			return false;
	}
	if (scan->stripes[lo].block_count != P)
		return false;
	R = scan->stripes[lo].visible_rows;
	scan->an_stripe = lo;
	scan->an_row = (uint32) (R * j / P);
	scan->an_end = (uint32) (R * (j + 1) / P);
	scan->an_have = true;
	return true;
}

/* The next row of the sampled block, or false when it has no more. */
static bool
kc_scan_analyze_next_inner(KcScan *scan, TupleTableSlot *slot)
{
	KcStripeInfo *si;
	uint32		pos;
	uint32		g;

	if (!scan->an_have || scan->an_row >= scan->an_end)
		return false;
	si = &scan->stripes[scan->an_stripe];

	/* (re)open the stripe if the footer in memory is another one */
	if (!scan->in_stripe || scan->cur != si)
	{
		if (scan->in_stripe)
			kc_footer_free(&scan->footer);
		scan->cur = si;
		kc_footer_load(scan->rel, si, &scan->footer);
		scan->in_stripe = true;
		scan->have_group = false;
		scan->an_group = -1;
	}

	/* find the group that holds the row */
	pos = 0;
	for (g = 0; g < scan->footer.ngroups; g++)
	{
		if (scan->an_row < pos + scan->footer.group_rows[g])
			break;
		pos += scan->footer.group_rows[g];
	}
	if (g >= scan->footer.ngroups)
		return false;
	if (scan->an_group != (int) g)
	{
		(void) kc_load_group(scan, g);
		scan->an_group = (int) g;
		scan->an_gstart = pos;
		scan->group_start = pos;
	}
	scan->group_pos = scan->an_row - scan->an_gstart;
	kc_store_group_row(scan, slot);
	scan->an_row++;
	return true;
}

bool
kc_scan_analyze_next(KcScan *scan, TupleTableSlot *slot)
{
	MemoryContext old = MemoryContextSwitchTo(scan->scan_cxt);
	bool		res = kc_scan_analyze_next_inner(scan, slot);

	MemoryContextSwitchTo(old);
	return res;
}
