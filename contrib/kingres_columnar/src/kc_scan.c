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
#include "access/parallel.h"
#include "access/xact.h"
#include "storage/bufmgr.h"
#include "storage/predicate.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"

#include "kingres_columnar.h"

/*
 * A scan that is abandoned by an error is never ended: the executor does not
 * run ExecutorEnd for a failed query or portal.  Its hold on the write
 * buffers is therefore remembered in the resource owner that was current when
 * the scan began, and the owner releases it when the error is cleaned up: the
 * owner of a portal is released when the portal is dropped or its failed
 * state is cleaned up at the abort of the subtransaction that failed it, the
 * owner of a statement outside a portal (a function, COPY, a rewrite) is the
 * one of the (sub)transaction itself.  See kc_write.c for the hold.
 */
static void
kc_hold_release_cb(Datum res)
{
	kc_write_hold_release(DatumGetUInt64(res));
}

static const ResourceOwnerDesc kc_hold_resowner_desc =
{
	.name = "kingres_columnar write buffer hold",
	.release_phase = RESOURCE_RELEASE_BEFORE_LOCKS,
	.release_priority = RELEASE_PRIO_FIRST,
	.ReleaseResource = kc_hold_release_cb,
	.DebugPrint = NULL
};

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

/* the stripes the scan's snapshot sees, with the rows of each that its command may read */
static void
kc_scan_load_stripes(KcScan *scan)
{
	int			i;
	int			nvisible = 0;

	scan->stripes = kc_catalog_load(scan->rel, scan->cat_snap, &scan->nstripes);
	for (i = 0; i < scan->nstripes; i++)
	{
		KcStripeInfo *si = &scan->stripes[i];

		si->visible_rows = kc_visible_rows(si, scan->curcid, scan->all_own_rows);
		if (si->visible_rows > 0)
			scan->stripes[nvisible++] = *si;
	}
	scan->nstripes = nvisible;
	scan->list_epoch = kc_flush_epoch;
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
	bool		maintenance = false;
	bool		parallel_ok = false;
	bool		fetch = false;

	/* a scan for fetches by row number (index scans) reads buffered rows by number, it holds none */
	if (flags & KC_SCAN_FETCH)
	{
		fetch = true;
		flags &= ~KC_SCAN_FETCH;
	}

	/*
	 * Maintenance (compaction) takes part in no serializable bookkeeping: no
	 * predicate lock, no dependency, because it changes nothing logically.  It
	 * reads with the snapshot it is given, or, with none, what is committed
	 * (kc_compact says which, and why).  Its own earlier deletions are past, and
	 * the stripes it wrote itself are read in full.
	 */
	if (flags & KC_SCAN_MAINTENANCE)
	{
		maintenance = true;
		flags &= ~KC_SCAN_MAINTENANCE;
	}

	if (flags & KC_SCAN_PARALLEL_OK)
	{
		parallel_ok = true;
		flags &= ~KC_SCAN_PARALLEL_OK;
	}

	/*
	 * The leader's unflushed rows are invisible to a worker: refuse, never
	 * answer wrongly.  Only the custom scan may run in a worker, because the
	 * leader has flushed the rows its transaction wrote before the workers
	 * started (kc_write_prepare_for_workers), or refused to start them.
	 */
	if (IsParallelWorker() && !parallel_ok)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("columnar tables cannot be read in a parallel worker"),
				 errhint("Declare the function PARALLEL UNSAFE, or run the query without parallelism.")));

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

	kc_check_snapshot_age(rel, snapshot);
	if (snapshot != NULL && !maintenance)
		PredicateLockRelation(rel, snapshot);
	cs = kc_catalog_snapshot(snapshot, &mvcc);
	if (!maintenance)
		kc_catalog_snapshot_report_conflicts(cs, mvcc);
	scan->cat_snap = cs;
	scan->all_own_rows = !mvcc || maintenance;
	scan->curcid = (mvcc && !maintenance) ? snapshot->curcid : InvalidCommandId;

	kc_scan_load_stripes(scan);

	/*
	 * The rows the transaction buffered, pinned so that no flush empties the
	 * buffer under the scan.  An analyze scan has no use for them.
	 */
	if (!(flags & SO_TYPE_ANALYZE) && !fetch)
	{
		ResourceOwner owner = CurrentResourceOwner;

		/* the slot in the owner first: remembering cannot fail afterwards */
		if (owner != NULL)
			ResourceOwnerEnlarge(owner);
		scan->hold = kc_write_hold_acquire(RelationGetRelid(rel),
										   kc_rel_ident(rel)->uid,
										   &scan->pend, &scan->npend);
		if (scan->npend > 0)
			scan->pend_row = scan->pend[0].first;
		if (scan->hold != 0 && owner != NULL)
		{
			ResourceOwnerRemember(owner, UInt64GetDatum(scan->hold),
								  &kc_hold_resowner_desc);
			scan->owner = owner;
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
	/*
	 * A scan that takes its stripes from a parallel state does not restart by
	 * itself: the state is reset by the node that owns it (ReInitializeDSM) and
	 * every participant then takes its share again.
	 */
	scan->stripe_idx = 0;
	scan->have_group = false;
	scan->pend_idx = 0;
	scan->pend_row = scan->npend > 0 ? scan->pend[0].first : 0;
	scan->ordinal = 0;
	scan->hit_end = false;
	MemoryContextReset(scan->group_cxt);
}

/*
 * A fingerprint of the stripes the scan will read (their number, ids, first
 * rows and visible rows), which every participant of a parallel scan must
 * compute alike: they took the list with the same snapshot, so a difference is
 * a defect, and it is reported, never answered with rows read twice or not at
 * all.
 */
uint64
kc_scan_fingerprint(const KcScan *scan)
{
	uint64		h = UINT64CONST(14695981039346656037);
	int			i;

#define KC_FP_MIX(v) \
	do { h ^= (uint64) (v); h *= UINT64CONST(1099511628211); } while (0)
	KC_FP_MIX(scan->nstripes);
	for (i = 0; i < scan->nstripes; i++)
	{
		const KcStripeInfo *si = &scan->stripes[i];

		KC_FP_MIX(si->stripe_id);
		KC_FP_MIX(si->first_row);
		KC_FP_MIX(si->row_count);
		KC_FP_MIX(si->visible_rows);
	}
#undef KC_FP_MIX
	return h;
}

/*
 * Take the stripes from a parallel state from now on (the node that owns the
 * state calls this before the first row is read).  A scan that already read
 * from the state, or a list that differs from the leader's, is an error.
 */
void
kc_scan_attach(KcScan *scan, KcParallel *shared)
{
	if (scan->nstripes != (int) shared->nstripes ||
		kc_scan_fingerprint(scan) != shared->fingerprint)
		ereport(ERROR,
				(errcode(ERRCODE_INTERNAL_ERROR),
				 errmsg("a parallel scan of columnar table \"%s\" found another list of stripes than its leader",
						RelationGetRelationName(scan->rel))));
	scan->shared = shared;
}

/*
 * Give back the scan's hold on the write buffers (it is no longer reading
 * them).  The scan stops reading its pending rows.
 */
void
kc_scan_drop_hold(KcScan *scan)
{
	if (scan->hold != 0)
	{
		if (scan->owner != NULL)
			ResourceOwnerForget(scan->owner, UInt64GetDatum(scan->hold),
								&kc_hold_resowner_desc);
		kc_write_hold_release(scan->hold);
		scan->hold = 0;
		scan->owner = NULL;
	}
	scan->npend = 0;
}

void
kc_scan_end(KcScan *scan)
{
	kc_scan_drop_hold(scan);
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
			/* a chunk of its own checksum, verified before anything decodes it */
			if (kc_crc32c(comp, cd->comp_len) != (uint64) cd->crc)
				kc_corrupt(scan->rel, si->stripe_id, "chunk checksum mismatch (group %u, column %d)",
						   g, c + 1);
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
			out[c].has_minmax = (cd->flags & KC_CHUNK_HAS_MINMAX) != 0 &&
				cd->typid == scan->ti->cols[c].typid &&
				cd->collation == scan->ti->cols[c].collation;
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
	for (;;)
	{
		KcStripeInfo *si;

		if (scan->shared != NULL)
		{
			/* every participant takes the next stripe nobody has taken */
			uint64		idx = pg_atomic_fetch_add_u64(&scan->shared->next, 1);

			if (idx >= (uint64) scan->nstripes)
				return false;
			si = &scan->stripes[idx];
		}
		else
		{
			if (scan->stripe_idx >= scan->nstripes)
				return false;
			si = &scan->stripes[scan->stripe_idx++];
		}

		scan->stats.stripes_total++;
		if (scan->skip && kc_guc_enable_chunk_skipping && kc_skip_active(scan->skip))
		{
			KcColStat  *st = palloc(sizeof(KcColStat) * (scan->natts + 1));
			bool		skipit;

			kc_stripe_stats_parse(si, scan->natts, scan->ti, st);
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

		kc_row_tid(row, &slot->tts_tid);
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
		uint64		rownum;
		int			c;

		kc_write_hold_check(scan->hold, scan->rel);
		if (!kc_write_pending_fetch(RelationGetRelid(scan->rel), ref,
									scan->pend_row, &vals, &nulls, &ncols, &cid, &rownum) ||
			(!scan->all_own_rows && cid >= scan->curcid))
		{
			/* command ids never decrease in a buffer: this one is done */
			scan->pend_idx++;
			scan->pend_row = scan->pend_idx < scan->npend ?
				scan->pend[scan->pend_idx].first : 0;
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
		if (rownum != KC_NO_ROW)
			kc_row_tid(rownum, &slot->tts_tid);	/* a row that has its number (a reservation) */
		else
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

/* ------------------------------------------------------------------------
 * Fetch of one row by its number (index scans, C6b)
 * ------------------------------------------------------------------------
 */

/*
 * The row with this number, if the scan's snapshot and command may see it, into
 * slot.  A row the transaction has buffered and numbered (a buffer that
 * reserves) is taken from the buffer, by the command-id rule; any other row from
 * the stripe whose numbers contain it: one chunk group is decoded (all the
 * columns) and kept, so consecutive fetches in the same group cost one decode.
 * A number that no visible stripe and no buffer of this transaction has (an
 * index entry of an aborted or rolled back writer, of a stripe that was removed,
 * of a writer not committed before the snapshot) returns nothing: that is how
 * every dead entry is filtered, and it is why a row number is never given to
 * another row.  The scan was begun with KC_SCAN_FETCH.
 */
bool
kc_scan_fetch_row(KcScan *scan, uint64 row, TupleTableSlot *slot)
{
	Datum	   *vals;
	bool	   *nulls;
	int			ncols;
	CommandId	cid;
	KcStripeInfo *si = NULL;
	int			lo,
				hi,
				idx = -1,
				i,
				c;
	uint32		off;
	uint32		start = 0;
	uint32		g;
	MemoryContext old;

	if (row >= KC_MAX_ROW_NUMBER)
		return false;

	if (kc_write_pending_by_number(RelationGetRelid(scan->rel), kc_rel_ident(scan->rel)->uid,
								   row, &vals, &nulls, &ncols, &cid))
	{
		if (!scan->all_own_rows && cid >= scan->curcid)
			return false;
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
		kc_row_tid(row, &slot->tts_tid);
		slot->tts_tableOid = RelationGetRelid(scan->rel);
		ExecStoreVirtualTuple(slot);
		return true;
	}

	old = MemoryContextSwitchTo(scan->scan_cxt);
	if (scan->list_epoch != kc_flush_epoch)
	{
		/* this backend wrote a stripe since the list was read: the rows of a buffer may be in it now */
		if (scan->in_stripe)
			kc_footer_free(&scan->footer);
		scan->in_stripe = false;
		scan->have_group = false;
		kc_scan_load_stripes(scan);
	}
	MemoryContextSwitchTo(old);

	lo = 0;
	hi = scan->nstripes - 1;
	while (lo <= hi)
	{
		int			mid = lo + (hi - lo) / 2;

		if (scan->stripes[mid].first_row <= row)
		{
			idx = mid;
			lo = mid + 1;
		}
		else
			hi = mid - 1;
	}
	if (idx >= 0 && row < scan->stripes[idx].first_row + scan->stripes[idx].row_count)
		si = &scan->stripes[idx];
	else
	{
		/* the list is in stripe id order, which is the order of the numbers; do not trust it blindly */
		for (i = 0; i < scan->nstripes; i++)
			if (scan->stripes[i].first_row <= row &&
				row < scan->stripes[i].first_row + scan->stripes[i].row_count)
			{
				si = &scan->stripes[i];
				break;
			}
	}
	if (si == NULL)
		return false;
	off = (uint32) (row - si->first_row);
	if (off >= si->visible_rows)
		return false;

	old = MemoryContextSwitchTo(scan->scan_cxt);
	if (!scan->in_stripe || scan->cur->stripe_id != si->stripe_id)
	{
		if (scan->in_stripe)
			kc_footer_free(&scan->footer);
		scan->cur = si;
		kc_footer_load(scan->rel, si, &scan->footer);
		scan->in_stripe = true;
		scan->have_group = false;
	}
	scan->cur = si;
	for (g = 0; g < scan->footer.ngroups; g++)
	{
		if (off < start + scan->footer.group_rows[g])
			break;
		start += scan->footer.group_rows[g];
	}
	if (g >= scan->footer.ngroups)
		kc_corrupt(scan->rel, si->stripe_id, "the chunk groups do not hold row %u", off);
	if (!scan->have_group || scan->group_start != start)
	{
		scan->group_nrows = kc_load_group(scan, g);
		scan->group_start = start;
		scan->have_group = true;
	}
	MemoryContextSwitchTo(old);

	ExecClearTuple(slot);
	for (c = 0; c < scan->natts; c++)
	{
		if (scan->needed[c] && scan->colvals[c] != NULL)
		{
			slot->tts_values[c] = scan->colvals[c][off - start];
			slot->tts_isnull[c] = scan->colnulls[c][off - start];
		}
		else
		{
			slot->tts_values[c] = (Datum) 0;
			slot->tts_isnull[c] = true;
		}
	}
	kc_row_tid(row, &slot->tts_tid);
	slot->tts_tableOid = RelationGetRelid(scan->rel);
	ExecStoreVirtualTuple(slot);
	return true;
}
