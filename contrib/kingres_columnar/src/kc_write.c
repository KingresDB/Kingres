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
 * The invariant of a commit: a row that an insert accepted without an error is
 * committed with its transaction.  This callback writes every buffer at
 * pre-commit, and callbacks of other modules that were registered earlier run
 * after it and may write: from the flush on, a row goes straight to a stripe
 * (kc_write_late) and no buffer is touched; the same holds between the two
 * phases of a hand-over, where the buffers are prepared and must not be
 * changed.  A write at a moment where PostgreSQL forbids an error (after the
 * commit or abort of a transaction or subtransaction has begun, in a critical
 * section) is refused with a warning and never with an error, which the
 * server would make a PANIC (kc_write_refused, README "Writes while a
 * transaction is ending").
 *
 * The invariant of memory: the memory of the buffers of one backend is bounded
 * by a number that follows from write_buffer_memory (A) alone: the buffers are
 * written, the largest first, when they are over A and the running
 * subtransaction may write them, and a write that would go on from above 2A,
 * where nothing can be written (rows of enclosing subtransactions, rows a
 * cursor reads), is refused (kc_write_row_values, kc_subxact_prepare).  Every
 * buffer is charged for its own memory, empty or not, and an empty buffer that
 * a flush leaves is freed, so the number of buffers is part of the bound.
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
#include "access/xlog.h"
#include "access/parallel.h"
#include "access/relation.h"
#include "access/table.h"
#include "access/xact.h"
#include "catalog/storage.h"
#include "miscadmin.h"
#include "pgstat.h"
#include "storage/lock.h"
#include "storage/predicate.h"
#include "utils/guc.h"
#include "utils/hsearch.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"
#include "utils/syscache.h"

#include "kingres_columnar.h"

/*
 * The largest block a buffer's memory context takes from the allocator.  The
 * context of a buffer grows by doubling blocks up to this, and what is unused of
 * the last block is held memory the buffer's accounting (kc_charge) does not see,
 * so the size is kept small: the slack of a buffer is at most one block.
 */
#define KC_BUF_MAX_BLOCK	(1024 * 1024)

/*
 * What a write buffer costs before it holds a row: its header, its memory
 * context and the first block the context takes.  Charged to the accounting
 * for as long as the buffer exists, so that the number of buffers (levels of
 * subtransaction, relations, partitions, buffers that cursors keep apart) is
 * part of the bound on memory and not beside it.
 */
#define KC_BUF_OVERHEAD		2048

/*
 * The hard ceiling on the memory of the write buffers, as a multiple of the
 * allowance (kingres_columnar.write_buffer_memory).  The allowance is what the
 * writer keeps to by writing stripes; the ceiling is what it never exceeds
 * where writing cannot help (rows of enclosing subtransactions, rows that a
 * cursor reads): a write that would go beyond it is refused with an error.
 */
#define KC_CEILING_FACTOR	2

/*
 * How often each path of the write side was taken in this backend, for tests
 * and for diagnosis (kingres.columnar_write_paths).  A test that claims to
 * reach a path asserts that its count is not zero; the counters cost an
 * increment and are kept in every build.
 */
typedef enum KcPath
{
	KC_PATH_LATE_COMMIT,		/* write-through after the commit-time flush */
	KC_PATH_LATE_WINDOW,		/* write-through between the two phases of a hand-over */
	KC_PATH_PRESSURE_FLUSH,		/* memory pressure wrote a buffer */
	KC_PATH_OWN_FLUSH,			/* a committing subtransaction wrote its own big buffer */
	KC_PATH_HANDOVER_FLUSH,		/* a hand-over wrote stripes (parent's buffer over the stripe size or the allowance) */
	KC_PATH_SIZE_FLUSH,			/* a buffer was written because it held a full stripe */
	KC_PATH_PREPARED_SKIP,		/* a flush was refused because a hand-over was prepared or the commit flush was done */
	KC_PATH_CEILING_WRITE,		/* a write was refused at the ceiling */
	KC_PATH_CEILING_HANDOVER,	/* a hand-over was refused at the ceiling */
	KC_PATH_REFUSED_ENDING,		/* a write was refused: the transaction state forbids an error */
	KC_PATH_ORPHANS_CLEANED,	/* stripe rows of a file dropped after the cleanup, removed later */
	KC_PATH_REFUSED_LIVE,		/* ... of those refusals, the ones whose top-level transaction was still alive (it then fails at its next pre-commit) */
	KC_PATH_LOST_RAISED,		/* the error raised for a refusal of that kind */
	KC_PATH_RESERVED,			/* a buffer reserved a stripe's worth of row numbers (0.2) */
	KC_PATH_RETURNED,			/* the unused tail of a reservation was given back */
	KC_PATH_GAP,				/* ... could not be, and stays a gap in the numbers */
	KC_PATH_COUNT
} KcPath;

static const char *const kc_path_names[KC_PATH_COUNT] = {
	"late_commit", "late_window", "pressure_flush", "own_flush", "handover_flush",
	"size_flush", "prepared_skip", "ceiling_write", "ceiling_handover",
	"refused_ending", "orphans_cleaned", "refused_live", "lost_raised",
	"reserved", "tail_returned", "tail_gap"
};

static uint64 kc_path_hits[KC_PATH_COUNT];

#define KC_PATH(p) (kc_path_hits[p]++)

static Size kc_total_bytes_for_report(void);
static int	kc_orphans_kept_for_report(void);

/*
 * The rows the SQL function reports: one per path, then two that are not
 * counts and are never reset: "bytes_held", the memory the write buffers are
 * charged for right now (the number the allowance and the ceiling are about, so a
 * test can measure what a buffer, a level of subtransaction or a row costs), and
 * "orphans_kept", the number of files noted at the commit of this backend's last
 * transaction that the next transaction deletes the stripe rows of.
 */
int
kc_write_path_count(void)
{
	return KC_PATH_COUNT + 2;
}

const char *
kc_write_path_get(int i, uint64 *hits)
{
	if (i == KC_PATH_COUNT)
	{
		*hits = kc_total_bytes_for_report();
		return "bytes_held";
	}
	if (i == KC_PATH_COUNT + 1)
	{
		*hits = (uint64) kc_orphans_kept_for_report();
		return "orphans_kept";
	}
	*hits = kc_path_hits[i];
	return kc_path_names[i];
}

void
kc_write_paths_reset(void)
{
	memset(kc_path_hits, 0, sizeof(kc_path_hits));
}

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
	bool		uncounted;		/* a buffer of one write-through call: not in kc_total_bytes */

	/*
	 * Memory charged for this buffer: the by-reference values (the space their
	 * chunks take, headers and rounding included), the arrays of row slots
	 * (their capacity, not their fill) and a fixed charge for the buffer
	 * itself (KC_BUF_OVERHEAD: its memory context and header, which a buffer
	 * with no rows still holds).  This is what counts against
	 * write_buffer_memory (kc_total_bytes is the sum over all buffers), so the
	 * allowance is about memory and not about payload, and a thousand empty
	 * buffers are not free.
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

	/*
	 * Row numbers reserved at insert (increment C6a, design 5.3 and 7.8).  A
	 * buffer that reserves takes a whole stripe's worth of numbers from the
	 * metapage when its first row goes in, and inserts the stripe's catalog row
	 * in the state "reserved" at once: row i of the buffer has the number
	 * res_first + i, and so the TID the insert returns, whatever happens to the
	 * buffer (a flush by size, by memory, at a savepoint, at commit or prepare)
	 * until the transaction ends.  A buffer that reserves holds at most
	 * res_count rows, is written as one stripe (the front of the reservation;
	 * the unused tail is given back when it is the end of the range), is never
	 * merged into another buffer, and is written, not merged, by a committing
	 * subtransaction (decision 3 of C6a).
	 *
	 *	 reserve_asked	the setting was on when the buffer was made, and the
	 *					table is one that may reserve: what decides which buffer
	 *					a row goes to (a row never goes into a buffer made under
	 *					the other setting)
	 *	 reserving		this buffer reserves (reserve_asked and the table is of
	 *					format 3 and the catalog has the column)
	 *	 res_valid		the reservation is held: the numbers are taken and the
	 *					reserved catalog row is in the catalog (found by its key)
	 *	 res_cmin		the command id that inserted that row: it is "invisible"
	 *					to an update of the same command, which then waits for the
	 *					command counter to pass (kc_pass_command)
	 */
	bool		reserve_asked;
	bool		reserving;
	bool		res_valid;
	uint64		res_stripe_id;
	uint64		res_first;
	uint32		res_count;
	CommandId	res_cmin;
} KcWriteBuf;

/* the developer settings of C6a (kc_write_init) */
bool		kc_guc_reserve_rows = false;
int			kc_guc_reserve_gate = 0;
int			kc_guc_reserve_gate_at = 0;

/* where kingres_columnar.reserve_gate_at stops a writer (testing) */
#define KC_RES_AFTER_ALLOC		1	/* the numbers are taken from the metapage */
#define KC_RES_AFTER_EARLY_ROW	2	/* the reserved catalog row is inserted */
#define KC_RES_AFTER_PAGES		3	/* the stripe's pages are written, its row is not yet updated */
#define KC_RES_AFTER_UPDATE		4	/* the row is updated, the tail not yet given back */

static void
kc_reserve_gate(int point)
{
	if (kc_guc_reserve_gate != 0 && kc_guc_reserve_gate_at == point)
		kc_gate_wait(kc_guc_reserve_gate);
}

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

static Size
kc_total_bytes_for_report(void)
{
	return kc_total_bytes;
}

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

/*
 * Set when the pre-commit (or pre-prepare) callback of this module has written
 * every buffer.  Callbacks of other modules that were registered before this
 * one run after it and may run SQL: a row written from then on is written to a
 * stripe at once (kc_write_late), because nothing would write a buffer
 * afterwards.  The same holds between the two phases of a hand-over.
 */
static bool kc_commit_flushed = false;

/* a hold taken while a hand-over was prepared that its publication broke */
static bool kc_broken_any = false;

/* set while compaction copies rows through the buffers (kc_write_set_maintenance) */
static bool kc_maintenance_write = false;

/* the first row number of each stripe that compaction wrote in this transaction (kc_write_maintenance_ranges) */
static uint64 *kc_maint_first = NULL;
static int	kc_maint_nfirst = 0;
static int	kc_maint_cap = 0;

/*
 * The files this transaction replaced (a rewrite, VACUUM FULL, a change of
 * access method): the table and the identity of the file it had.  The stripe
 * rows of such a file are of no use to anyone once the new file is in place.  The
 * access method has no callback when a file goes away, but every way a file goes
 * away ends in the drop of a relation that holds it, which an object access hook
 * sees (kc_write_note_dropped); the rows are removed by the command that replaced
 * the file (the event triggers call kc_write_cleanup_replaced) or, at the latest,
 * at commit, and come back if the transaction aborts.  Correctness does not
 * depend on an event trigger.  A note follows the subtransaction that made it
 * (kc_replaced_subxact).  The list is in the memory of the top-level transaction
 * and starts empty.
 */
typedef struct KcReplaced
{
	Oid			relid;
	uint64		uid;
	SubTransactionId subid;		/* the subtransaction that noted it (see kc_replaced_subxact) */
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
	kc_maint_first = NULL;
	kc_maint_nfirst = 0;
	kc_maint_cap = 0;
	kc_total_bytes = 0;
	kc_bytes_since_look = 0;
	kc_handover_prepared = false;
	kc_commit_flushed = false;
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

/*
 * Do the rows written now ask for row numbers at insert?  The developer setting
 * is on, and the rows are not the copies of compaction nor the rows of a rewrite
 * (their stripes are written with frozen catalog rows, and a rewrite builds the
 * table's indexes again; neither promises a TID to anyone).  Whether the table can
 * is another matter (the format of its file and the shape of the stripe catalog:
 * kc_buf_make).  Nor in parallel mode (a CREATE TABLE AS whose query is parallel
 * writes its rows in the leader while the executor is in parallel mode): a reserved
 * row is replaced by an update, and heap forbids an update, and the command counter
 * passing it, in parallel mode, so such a buffer could never be written by size or
 * by memory.  The rows of such a statement have their numbers when their stripes
 * are written, as without the setting; nothing can have an index yet that would
 * need them earlier (the target of the statement is new).
 */
/* Has the table an index?  Then every row it takes needs its number when it is inserted (C6b). */
bool
kc_rel_reserves(Relation rel)
{
	return rel->rd_rel->relhasindex && RelationGetIndexList(rel) != NIL;
}

uint64		kc_flush_epoch = 0;

static bool
kc_reserve_asked(KcRelState *st, Relation rel)
{
	return (kc_guc_reserve_rows || kc_rel_reserves(rel)) && !kc_maintenance_write &&
		!st->frozen && !IsInParallelMode();
}

/*
 * A new buffer, in the memory of parent.  A counted buffer (the buffers of the
 * transaction's state) is charged KC_BUF_OVERHEAD from here on; an uncounted
 * one is the private buffer of a single write-through call (kc_write_late),
 * which lives in the caller's memory and is gone when the call ends.
 */
static KcWriteBuf *
kc_buf_make(Relation rel, int ncols, MemoryContext parent, bool counted,
		   bool reserve_asked)
{
	KcWriteBuf *b;
	MemoryContext cxt;
	KcMetaData meta;

	KC_FAULT(KC_FAULT_ALLOC);
	/* small to start with: a transaction may have a buffer per subtransaction */
	cxt = AllocSetContextCreate(parent, "kingres_columnar rows",
								ALLOCSET_DEFAULT_MINSIZE,
								ALLOCSET_SMALL_INITSIZE,
								KC_BUF_MAX_BLOCK);
	b = MemoryContextAllocZero(parent, sizeof(KcWriteBuf));
	b->serial = kc_next_serial++;
	b->relid = RelationGetRelid(rel);
	b->subid = GetCurrentSubTransactionId();
	b->uid = kc_rel_ident(rel)->uid;
	b->cxt = cxt;
	b->ncols = ncols;
	b->uncounted = !counted;

	kc_meta_read(rel, &meta);
	b->opts.codec = meta.codec;
	b->opts.level = meta.level;
	b->opts.stripe_rows = meta.stripe_rows;
	b->opts.chunk_rows = meta.chunk_rows;
	b->reserve_asked = reserve_asked;
	b->reserving = reserve_asked && meta.version >= KC_META_VERSION_RESERVE &&
		kc_catalog_has_reserved();
	if (counted)
	{
		/* last: nothing after this point can fail, so the charge is never orphaned */
		b->bytes = KC_BUF_OVERHEAD;
		kc_total_bytes += KC_BUF_OVERHEAD;
	}
	return b;
}

static KcWriteBuf *
kc_buf_create(KcRelState *st, Relation rel, int ncols)
{
	KcWriteBuf *b = kc_buf_make(rel, ncols, kc_write_cxt, true, kc_reserve_asked(st, rel));
	MemoryContext old = MemoryContextSwitchTo(kc_write_cxt);

	st->bufs = lappend(st->bufs, b);
	MemoryContextSwitchTo(old);
	return b;
}

/* What a counted buffer is charged when it holds no row. */
static Size
kc_buf_base(const KcWriteBuf *b)
{
	return b->uncounted ? 0 : KC_BUF_OVERHEAD;
}

/* Forget every row of the buffer; the buffer itself stays (and stays charged). */
static void
kc_buf_clear(KcWriteBuf *b)
{
	kc_total_bytes -= Min(kc_total_bytes, b->bytes);
	kc_total_bytes += kc_buf_base(b);
	MemoryContextReset(b->cxt);
	b->nrows = 0;
	b->cap = 0;
	b->vals = NULL;
	b->nulls = NULL;
	b->cids = NULL;
	b->sizes = NULL;
	b->bytes = kc_buf_base(b);
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
	if (b->uncounted)
		return;
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
 *
 * The invariant of a prepared hand-over is kept here for every caller at once:
 * from the moment kc_subxact_prepare has prepared a buffer until phase two has
 * published or dropped it, no buffer may be flushed by anything but the
 * hand-over itself (kc_merge_flush_prepare), because the prepared buffers
 * hold rows that a stripe holds as well, and a second stripe of them would
 * be a doubled row.  The prepared state is a property of the buffer
 * (merge_into, tail_ready) and of the whole backend (kc_handover_prepared:
 * the parent's buffer that takes the rows is prepared too).  Nothing that
 * chooses a victim or flushes a relation can see a buffer as flushable
 * while it is.
 */
static bool
kc_buf_flushable(KcRelState *st, KcWriteBuf *b)
{
	if (kc_handover_prepared || kc_commit_flushed ||
		b->merge_into != NULL || b->tail_ready)
	{
		KC_PATH(KC_PATH_PREPARED_SKIP);
		return false;
	}
	/* a reserved row is updated by the flush: heap forbids that in parallel mode */
	if (b->res_valid && IsInParallelMode())
		return false;
	return b->refs == 0 && b->subid == GetCurrentSubTransactionId();
}

static void kc_write_stripe(KcRelState *st, KcWriteBuf *b, Relation rel,
						   uint32 first, uint32 count, bool maintenance,
						   uint64 *first_row_out);
static void kc_flush_buf(KcRelState *st, KcWriteBuf *b, Relation rel);
static void kc_flush_buf_ex(KcRelState *st, KcWriteBuf *b, Relation rel,
							bool keep_tail);
static uint32 kc_stripe_limit(const KcOptions *opts);
static void kc_coalesce(KcRelState *st);
static void kc_buf_free(KcWriteBuf *b);
static void kc_buf_forget_reservation(KcWriteBuf *b);
static void kc_buf_visible(const KcWriteBuf *b, uint32 *first, uint32 *end);

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

	/*
	 * Not while a hand-over is prepared, nor after the commit-time flush: no
	 * buffer may be written then except by the hand-over (kc_buf_flushable
	 * says so for each buffer; this says it for the whole look).
	 */
	if (kc_states == NULL)
		return;
	if (kc_handover_prepared || kc_commit_flushed)
	{
		KC_PATH(KC_PATH_PREPARED_SKIP);
		return;
	}
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
			kc_buf_forget_reservation(best);
			kc_buf_clear(best);
		}
		else
		{
			kc_flush_buf(best_st, best, rel);
			table_close(rel, NoLock);
			KC_PATH(KC_PATH_PRESSURE_FLUSH);
		}

		/*
		 * An empty buffer still costs its context and header, and a transaction
		 * that touches many relations would hold one for each: under pressure
		 * the buffer goes away with its rows, and the state of the relation
		 * with it unless the state says something (stripes of a rewrite are
		 * frozen).  The next row of the relation makes them again.  Only the
		 * buffer that was just written is looked at, and nobody holds it (no
		 * scan reads it, no hand-over is prepared: it was flushable).
		 */
		if (best->nrows == 0 && best->refs == 0 && best->merge_into == NULL &&
			!best->tail_ready)
		{
			best_st->bufs = list_delete_ptr(best_st->bufs, best);
			kc_buf_free(best);
			if (best_st->bufs == NIL && !best_st->frozen)
				hash_search(kc_states, &best_st->relid, HASH_REMOVE, NULL);
		}
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
		kc_maint_buf = kc_buf_make(rel, ncols, kc_write_cxt, true, false);
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

			uint64		fr = KC_NO_ROW;

			kc_write_stripe(st, b, rel, done, n, true, &fr);
			if (kc_maint_nfirst == kc_maint_cap)
			{
				kc_maint_cap = kc_maint_cap ? kc_maint_cap * 2 : 16;
				kc_maint_first = kc_maint_first ?
					repalloc(kc_maint_first, sizeof(uint64) * kc_maint_cap) :
					MemoryContextAlloc(kc_write_cxt, sizeof(uint64) * kc_maint_cap);
			}
			kc_maint_first[kc_maint_nfirst++] = fr;
			done += n;
		}
	}
	kc_buf_free(b);
}

/*
 * The first row numbers of the stripes that compaction has written since it began: the rows
 * it copied have these numbers, and the indexes of the table need entries for them.
 */
uint64 *
kc_write_maintenance_ranges(int *n)
{
	*n = kc_maint_nfirst;
	return kc_maint_first;
}

void
kc_write_maintenance_discard(void)
{
	kc_maint_nfirst = 0;
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

/*
 * Is a row written now one that no later callback of this module will write?
 * After the commit-time flush (callbacks of other modules that run after it)
 * and between the two phases of a subtransaction's hand-over (callbacks that
 * run after the first phase) the buffers are not ours to change any more, and
 * the access method has no later point at which it could write a new one.
 */
static bool
kc_write_is_late(void)
{
	return !kc_maintenance_write && (kc_commit_flushed || kc_handover_prepared);
}

/*
 * The moments at which PostgreSQL forbids an error.  An ERROR raised there is
 * made a PANIC (a critical section; the commit path after the commit record,
 * "cannot abort transaction ... it was already committed"; the abort path, "error
 * stack"; a failed assertion in the callbacks of PREPARE TRANSACTION), so the
 * access method must never raise one for a call that reaches it there; and
 * it cannot do the work either, because writing a stripe or changing the
 * catalog reads the catalogs and allocates memory, which PostgreSQL forbids
 * there as well (a write into a heap table at those moments stops a server
 * built with assertions; the README says which).  What it can do is refuse
 * without an error: kc_entry_refused is the one place that decides "may this
 * entry point touch storage or the catalog now", and every entry point that
 * does (all callbacks of the table access method, the SQL functions that
 * write, the object access hook) asks it first, so that the next entry point
 * has one function to call and the test that walks the routine (t/kcevents)
 * fails when one has no decision recorded (kc_am_decisions in kc_am.c).
 *
 * What a refusal means depends on what is still possible, and the answer is
 * one of two:
 *
 *	 KC_END_DECIDED  the fate of the (sub)transaction is settled: the commit
 *	   or prepare of the top-level transaction has begun (XACT_EVENT_COMMIT,
 *	   XACT_EVENT_PREPARE), or the transaction or subtransaction is aborting
 *	   (XACT_EVENT_ABORT, SUBXACT_EVENT_ABORT_SUB), or no transaction runs (the
 *	   exit of the process).  Nothing better than a WARNING exists: an aborting
 *	   transaction loses the data anyway, and a committed one cannot be changed
 *	   any more.
 *
 *	 KC_END_LIVE  the top-level transaction is still alive and can still abort
 *	   cleanly: the commit of a subtransaction (SUBXACT_EVENT_COMMIT_SUB, and
 *	   the release of its resources) and a critical section entered by a
 *	   transaction in progress BEFORE this module's own pre-commit or
 *	   pre-prepare callback has run.  A critical section entered after it (a
 *	   callback of another module that runs later) is DECIDED, not LIVE:
 *	   no transaction callback of this module runs again for the top-level
 *	   transaction before the commit record, so no error can be raised for a
 *	   remembered refusal, and the warning says so (C9-01).  Inside a
 *	   subtransaction that such a callback began it is LIVE again (C10-03): the
 *	   callbacks of this module for the subtransaction do run, and its
 *	   pre-commit raises.
 *	   The refusal is remembered (statics, so that
 *	   nothing is allocated) and raised as an ERROR at the next moment an error
 *	   is allowed: the pre-commit of the subtransaction that would have owned
 *	   the lost data, the pre-commit or pre-prepare of the top-level
 *	   transaction, or the next write to a columnar table.  The transaction
 *	   then cannot commit having lost a row or an operation.  The WARNING
 *	   still comes first, because it names the cause at the moment it happens.
 *
 * The states: IsTransactionState() is true only while the transaction is in
 * progress; the callbacks at pre-commit, pre-prepare, the start of a
 * subtransaction and the first phase of its commit are in that state, and a
 * call there is served (kc_write_late and the buffers).  The commit or the
 * abort of a subtransaction is told apart by what this module saw: it saw
 * PRE_COMMIT_SUB before every COMMIT_SUB, so a subtransaction that passed it
 * and has not ended is committing, whichever module's callback runs first;
 * once this module's own callback has run it knows the end (kc_sub_ended).
 * An abort after the pre-commit (another module's error between the two) is
 * taken for a commit until this module's ABORT_SUB callback runs, which
 * forgets the refusal that belonged to the subtransaction.
 */
typedef enum KcEnding
{
	KC_END_NONE,				/* an error is allowed */
	KC_END_DECIDED,				/* the fate of the transaction is settled */
	KC_END_LIVE					/* the top-level transaction can still abort */
} KcEnding;

/* the subtransaction whose PRE_COMMIT_SUB was seen and whose end was not */
static SubTransactionId kc_sub_precommitted = InvalidSubTransactionId;

/* the end of a subtransaction this module's callback saw; valid while it is the current one */
static SubTransactionId kc_sub_ended = InvalidSubTransactionId;
static SubTransactionId kc_sub_ended_parent = InvalidSubTransactionId;
static bool kc_sub_ended_committed = false;

/*
 * A call refused while the top-level transaction was alive: how many, the
 * outermost subtransaction that owns what was lost (it and every one around it
 * cannot commit; one that aborts forgets it), and the first one's name, in
 * static memory because it is set where nothing may be allocated.
 */
static int	kc_lost_count = 0;
static SubTransactionId kc_lost_owner = InvalidSubTransactionId;
static char kc_lost_what[48];
static char kc_lost_name[NAMEDATALEN];

/*
 * Whether a critical section is open at a moment at which this module's own
 * pre-commit (or pre-prepare) callback has already run: a callback of another
 * module that runs after it entered the critical section.  Nothing of this
 * extension's transaction callback runs between that moment and the commit
 * record (PostgreSQL's own work after the pre-commit callbacks,
 * CommitTransaction and PrepareTransaction, has no hook that an extension can
 * use, and an error is allowed there only for PostgreSQL's own checks), so a
 * call refused then cannot be remembered and raised later: the refusal is
 * final, like one at XACT_EVENT_COMMIT.  That holds only at the top level
 * (C10-03): a callback of a later module can begin a subtransaction, and the
 * callbacks of this module for the subtransaction (its start, its pre-commit,
 * its commit) do run; a call refused in a critical section inside it is LIVE,
 * remembered for that subtransaction, and its pre-commit raises the error, as
 * for any subtransaction.
 */
static bool
kc_crit_after_own_precommit(void)
{
	return IsTransactionState() && CritSectionCount > 0 && kc_commit_flushed &&
		GetCurrentTransactionNestLevel() == 1;
}

static KcEnding
kc_ending(void)
{
	if (IsTransactionState())
	{
		if (CritSectionCount == 0)
			return KC_END_NONE;
		return kc_crit_after_own_precommit() ? KC_END_DECIDED : KC_END_LIVE;
	}
	if (GetCurrentTransactionNestLevel() > 1)
	{
		SubTransactionId cur = GetCurrentSubTransactionId();

		if (kc_sub_ended == cur)
			return kc_sub_ended_committed ? KC_END_LIVE : KC_END_DECIDED;
		if (kc_sub_precommitted == cur)
			return KC_END_LIVE;
	}
	return KC_END_DECIDED;
}

static bool
kc_write_state_forbids_error(void)
{
	return kc_ending() != KC_END_NONE;
}

static void
kc_lost_clear(void)
{
	kc_lost_count = 0;
	kc_lost_owner = InvalidSubTransactionId;
}

/* the end of a top-level transaction: nothing of the subtransactions is left */
static void
kc_ending_reset(void)
{
	kc_lost_clear();
	kc_sub_precommitted = InvalidSubTransactionId;
	kc_sub_ended = InvalidSubTransactionId;
}

/* the error that a refusal of the LIVE kind turns into where one is allowed */
static pg_noreturn void
kc_lost_raise(void)
{
	KC_PATH(KC_PATH_LOST_RAISED);
	ereport(ERROR,
			(errcode(ERRCODE_INVALID_TRANSACTION_STATE),
			 kc_lost_name[0] != 0
			 ? errmsg("a call on columnar table \"%s\" was refused earlier in this transaction, so it cannot commit",
					  kc_lost_name)
			 : errmsg("a call to %s was refused earlier in this transaction, so it cannot commit",
					  kc_lost_what),
			 errdetail("Code of another module (not a statement of this session) called the table at a moment where the server forbids an error: %d call(s) were refused while the transaction was still open (the commit of a subtransaction, or a critical section); the first was \"%s\".  What they were to do was not done.",
					   kc_lost_count, kc_lost_what),
			 errhint("Roll back, and write only at XACT_EVENT_PRE_COMMIT or XACT_EVENT_PRE_PREPARE or earlier, outside a critical section.")));
}

/*
 * Whether the call named what, on the columnar table (or other object) named
 * name (NULL: none), must be refused now (see above); warns and counts when it
 * must.  Never raises an error where one is forbidden.  fails_txn says that
 * what the call was to do cannot be skipped without the transaction being wrong
 * (a write, a truncate, a new file): a refusal in a live transaction makes it
 * fail at its next pre-commit, and a call of this kind after such a refusal
 * raises at once.  A call whose skipping loses nothing (a vacuum, a note about a
 * dropped file, the clean-up of leftovers, which later transactions repeat)
 * passes false.  The refusal path allocates nothing of its own (static
 * counters and strings; the message is formatted by ereport in ErrorContext, which
 * PostgreSQL allows in a critical section).
 */
bool
kc_entry_refused(const char *what, const char *name, bool fails_txn)
{
	KcEnding	ending = kc_ending();

	if (ending == KC_END_NONE)
	{
		if (fails_txn && kc_lost_count > 0)
			kc_lost_raise();
		return false;
	}
	KC_PATH(KC_PATH_REFUSED_ENDING);
	/*
	 * The warning first: a hook of another module (emit_log_hook) can raise an
	 * error from it, which at the commit of a subtransaction aborts that
	 * subtransaction, and then nothing is lost that needs remembering.
	 */
	/*
	 * The text of each cell is true of that cell (C9-01, C9-05): what happens to
	 * the transaction depends on whether the call may be skipped (fails_txn),
	 * and on whether an error can still be raised for it later (ending).
	 */
	ereport(WARNING,
			(errcode(ERRCODE_INVALID_TRANSACTION_STATE),
			 errmsg("%s refused%s%s%s: %s",
					what,
					name ? " on columnar table \"" : "", name ? name : "", name ? "\"" : "",
					CritSectionCount > 0 ? "inside a critical section" :
					ending == KC_END_LIVE ? "at the commit of a subtransaction" :
					"the transaction is not in progress"),
			 errdetail(!fails_txn
					   ? "Code of another module called the table at a moment where the server forbids an error, and the table cannot be changed then.  Nothing was done; skipping this call loses nothing of the transaction (VACUUM, the commit or a later call does the same work)."
					   : ending == KC_END_LIVE
					   ? "Code of another module called the table at a moment where the server forbids an error, and the table cannot be changed then: in a critical section, or at the commit of a subtransaction.  Nothing was done; the transaction is still open and fails with an error at its next pre-commit."
					   : kc_crit_after_own_precommit()
					   ? "Code of another module called the table inside a critical section that it entered after this extension's own pre-commit (or pre-prepare) callback had run.  Nothing of this extension runs again before the commit record, so no error can be raised for this call.  Nothing was done; if the transaction commits (or prepares) it does so without this call."
					   : "Code of another module called the table at a moment where the server forbids an error, and the table cannot be changed then: in a critical section, or after the commit or abort of the transaction or subtransaction has begun (XACT_EVENT_COMMIT, XACT_EVENT_PREPARE, XACT_EVENT_ABORT, SUBXACT_EVENT_ABORT_SUB, the release of resources).  Nothing was done; the transaction cannot be failed for it any more: if it commits or prepares it does so without this call, and an aborting transaction or subtransaction loses its data anyway."),
			 errhint("Write at XACT_EVENT_PRE_COMMIT or XACT_EVENT_PRE_PREPARE, outside a critical section, or in a transaction of its own.")));
	if (ending == KC_END_LIVE && fails_txn)
	{
		SubTransactionId owner = GetCurrentSubTransactionId();

		KC_PATH(KC_PATH_REFUSED_LIVE);
		if (kc_sub_ended == owner && kc_sub_ended_committed)
			owner = kc_sub_ended_parent;
		if (kc_lost_count == 0)
		{
			strlcpy(kc_lost_what, what, sizeof(kc_lost_what));
			strlcpy(kc_lost_name, name ? name : "", sizeof(kc_lost_name));
			kc_lost_owner = owner;
		}
		else if (owner < kc_lost_owner)
			kc_lost_owner = owner;
		kc_lost_count++;
	}
	return true;
}

/* the same, for a call on a relation */
bool
kc_write_refused(Relation rel, const char *what)
{
	return kc_entry_refused(what, RelationGetRelationName(rel), true);
}

/* whether an error is forbidden now (the decision of kc_ending, for the SQL functions that read) */
bool
kc_error_forbidden(void)
{
	return kc_ending() != KC_END_NONE;
}

/*
 * Whether a subtransaction cannot be started now (C9-03).  A function that
 * begins one (columnar_check does, for each table) can only do so where the
 * server lets its own code do it: BeginInternalSubTransaction ends the session
 * with a FATAL for a subtransaction that is starting, committing or aborting,
 * and the call changes the state of a transaction that is not in progress, so
 * the moments are:
 *
 *	 - no transaction in progress (IsTransactionState: the commit, prepare and
 *	   abort of the top-level transaction, the abort and the commit of a
 *	   subtransaction, the release of a resource owner, the exit of the process);
 *	 - a critical section (a subtransaction allocates);
 *	 - a subtransaction between its PRE_COMMIT_SUB and its end, as this module's
 *	   own callback saw it (kc_sub_precommitted).
 *
 * What this module cannot see is the start of a subtransaction
 * (SUBXACT_EVENT_START_SUB: the state is "in progress" and nothing marks the
 * end of the event), and the pre-commit of a subtransaction when another module's
 * callback runs before this module's own; there the server's own FATAL for a
 * subtransaction begun in that state applies, as it does to any code, heap's
 * PL/pgSQL exception blocks among them (README).  The pre-commit and the
 * pre-prepare of the top-level transaction are fine.
 *
 * Nothing is raised and nothing is allocated; the caller answers "nothing was
 * checked".
 */
static const char *
kc_subxact_blocked(void)
{
	if (CritSectionCount > 0)
		return "inside a critical section";
	if (!IsTransactionState())
		return "the transaction is not in progress";
	if (GetCurrentTransactionNestLevel() > 1 &&
		kc_sub_precommitted == GetCurrentSubTransactionId())
		return "the subtransaction is committing";
	return NULL;
}

/* the same decision, quietly, for code that has another way out (kc_am_class_guarded) */
bool
kc_subxact_possible(void)
{
	return kc_subxact_blocked() == NULL && !IsInParallelMode() && !kc_error_forbidden();
}

bool
kc_subxact_refused(const char *what)
{
	const char *why = kc_subxact_blocked();

	if (why == NULL)
		return false;
	KC_PATH(KC_PATH_REFUSED_ENDING);
	ereport(WARNING,
			(errcode(ERRCODE_INVALID_TRANSACTION_STATE),
			 errmsg("%s refused: %s", what, why),
			 errdetail("The call starts a subtransaction for each table it looks at, which the server cannot do at this moment (it ends the session, or allocates where that is forbidden).  Nothing was checked, and the empty answer of a call that was refused is not an answer that the tables are clean."),
			 errhint("Call it from a statement, or at XACT_EVENT_PRE_COMMIT or XACT_EVENT_PRE_PREPARE.")));
	return true;
}

/*
 * Whether a row may be written at all now, after kc_write_refused said no to
 * refusing: a parallel worker must not write (an error: the state allows it).
 */
static void
kc_write_check_state(Relation rel)
{
	/*
	 * A relation that has the callbacks of this access method but is not
	 * recognised by it (the handler of its access method returns a copy of the
	 * routine, C12-04) cannot keep what is written: the flush at commit takes
	 * it for another kind of table and discards the buffer.  A table that
	 * cannot keep its rows does not take them.
	 */
	if (!kc_rel_is_columnar(rel))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("cannot insert into table \"%s\": its access method uses the columnar callbacks but its handler does not return the routine of kingres_columnar itself",
						RelationGetRelationName(rel)),
				 errhint("Create the access method over kingres.columnar_handler, or over a handler that returns the pointer that function returns.")));
	if (IsParallelWorker())
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_TRANSACTION_STATE),
				 errmsg("cannot insert into columnar table \"%s\" in a parallel worker",
						RelationGetRelationName(rel))));
}

/* Put one row into the arrays of a buffer.  May fail (memory), before it changes nrows. */
static void
kc_buf_put_row(KcWriteBuf *b, TupleDesc desc, Datum *vals, bool *nulls, CommandId cid)
{
	int			c;

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
}

/* ------------------------------------------------------------------------
 * Reserved row numbers (increment C6a)
 *
 * The life of a reservation, for a buffer that reserves (KcWriteBuf):
 *
 *	 1. the first row goes in: the stripe's whole worth of numbers is taken
 *	    from the metapage (kc_meta_allocate, the ALLOC record), and the
 *	    stripe's catalog row is inserted as "reserved", by the transaction or
 *	    subtransaction that inserts the row;
 *	 2. every later row of the buffer has the next number;
 *	 3. a flush writes the pages of the stripe (for the rows that are there),
 *	    replaces the reserved catalog row by the stripe's row, and gives the
 *	    unused tail of the numbers back when it is still the end of the range;
 *	 4. a transaction or subtransaction that ends in an abort drops the buffer:
 *	    the reserved row is a dead row nobody sees (the heap rules clean it), the
 *	    numbers are not given back (the abort callback may not touch the file),
 *	    and no row ever had them in a stripe.
 *
 * Numbers a row was handed are never given back (a later index entry may name
 * them); only the tail beyond the last row of a stripe that was written is.
 * ------------------------------------------------------------------------
 */

/*
 * The update (or the delete) of the reserved row is made by a command later
 * than the one that inserted it, because heap_update calls a row of its own
 * command "inserted after the scan started" and refuses it.  A flush in the
 * middle of the statement that wrote the first row (by size, or by memory), and
 * the flush at the commit of a statement that was the last of its transaction,
 * are the same command: the counter is passed first.  It is allowed where the
 * flush is: the executor of a statement, the pre-commit and pre-prepare
 * callbacks, the pre-commit of a subtransaction (never in a parallel mode, where
 * nothing is written).
 */
static void
kc_pass_command(KcWriteBuf *b)
{
	if (GetCurrentCommandId(false) <= b->res_cmin)
		CommandCounterIncrement();
}

static void
kc_give_back_tail(Relation rel, uint64 first, uint32 count)
{
	if (count == 0)
		return;
	if (kc_meta_return(rel, first, count))
		KC_PATH(KC_PATH_RETURNED);
	else
		KC_PATH(KC_PATH_GAP);
}

/*
 * The reservation lock (C6c).  Between the moment a writer takes row numbers from the
 * metapage and the moment the catalog row that claims them is inserted, the numbers
 * belong to nobody the catalog knows, and an index vacuum that looked at the catalog
 * in that gap would call the index entries the writer is about to make dead.  A writer
 * holds this lock in SHARE mode across the gap (an advisory-class lock of its own,
 * field 4 = 7, that no user lock uses), and the index vacuum takes it in EXCLUSIVE
 * mode while it reads the metapage and the catalog (kc_index_vacuum), so each of them
 * sees every claim that was made before it and the metapage that comes after.  Writers
 * do not wait for each other; one waits only for the short look of a vacuum.
 */
void
kc_reserve_lock(Relation rel, LOCKMODE mode)
{
	LOCKTAG		tag;

	SET_LOCKTAG_ADVISORY(tag, MyDatabaseId, 0x4B43, RelationGetRelid(rel), 7);
	(void) LockAcquire(&tag, mode, false, false);
}

void
kc_reserve_unlock(Relation rel, LOCKMODE mode)
{
	LOCKTAG		tag;

	SET_LOCKTAG_ADVISORY(tag, MyDatabaseId, 0x4B43, RelationGetRelid(rel), 7);
	(void) LockRelease(&tag, mode, false);
}

/* Take the numbers and insert the reserved catalog row; the buffer has none. */
static void
kc_buf_reserve_numbers(KcWriteBuf *b, Relation rel)
{
	KcStripeInfo si;
	uint64		stripe_id;
	uint64		first_row;
	uint32		count = kc_stripe_limit(&b->opts);

	Assert(b->reserving && !b->res_valid);
	kc_reserve_lock(rel, ShareLock);
	kc_meta_allocate(rel, count, &stripe_id, &first_row);
	KC_PATH(KC_PATH_RESERVED);
	kc_reserve_gate(KC_RES_AFTER_ALLOC);

	memset(&si, 0, sizeof(si));
	si.stripe_id = stripe_id;
	si.first_row = first_row;
	si.row_count = count;
	si.natts = b->ncols;
	kc_catalog_insert_reserved(rel, &si);
	b->res_cmin = GetCurrentCommandId(true);
	b->res_stripe_id = stripe_id;
	b->res_first = first_row;
	b->res_count = count;
	b->res_valid = true;
	kc_reserve_unlock(rel, ShareLock);
	kc_reserve_gate(KC_RES_AFTER_EARLY_ROW);
}

/*
 * Give a reservation up, for a buffer that holds no row: the reserved catalog
 * row goes, and all its numbers are given back when they are still the end of
 * the range.
 */
static void
kc_buf_release_reservation(KcWriteBuf *b, Relation rel)
{
	if (!b->res_valid)
		return;
	kc_pass_command(b);
	kc_catalog_delete_reserved(rel, b->res_stripe_id);
	b->res_valid = false;
	kc_give_back_tail(rel, b->res_first, b->res_count);
}

/*
 * The rows of the buffer are discarded and the file they were for is gone or
 * replaced (a stale buffer, a table dropped or truncated meanwhile): the claim
 * stays as it is, a row of the old file that its drop or rewrite removes with the
 * rest, and the buffer does not hold it any more.
 */
static void
kc_buf_forget_reservation(KcWriteBuf *b)
{
	b->res_valid = false;
}

/*
 * Write rows as stripes at once (write-through): the rows of a callback that
 * runs after the access method's last chance to write its buffers.  The rows
 * go through a buffer of this call alone, in the caller's memory, outside the
 * accounting, and the buffer is written as stripes of at most the stripe size
 * before the call returns.  Neither the buffers of the transaction nor the
 * prepared hand-over are touched, so the rows can be neither lost nor written
 * twice.  The stripes carry the id of the running (sub)transaction, which is
 * the one that wrote the rows; if a later callback fails and the transaction
 * aborts, they go with it.  A row written one at a time is a stripe of one
 * row: correct, and rare (only code that runs after the access method's
 * callback gets here).  Only columnar_compact() joins small stripes (VACUUM
 * does not): the cost of one stripe per row, measured, is in the README.
 */
static void
kc_write_late(Relation rel, Datum **vv, bool **nn, int nrows, CommandId cid,
			  uint64 *rownums)
{
	TupleDesc	desc = RelationGetDescr(rel);
	KcRelState *st = kc_state_get(RelationGetRelid(rel), true);
	KcWriteBuf *b;
	uint32		limit;
	uint32		done = 0;
	bool		pushed = false;
	int			i;

	KC_PATH(kc_handover_prepared ? KC_PATH_LATE_WINDOW : KC_PATH_LATE_COMMIT);
	b = kc_buf_make(rel, desc->natts, CurrentMemoryContext, false, kc_reserve_asked(st, rel));
	for (i = 0; i < nrows; i++)
		kc_buf_put_row(b, desc, vv[i], nn[i], cid);

	if (!ActiveSnapshotSet())
	{
		PushActiveSnapshot(GetTransactionSnapshot());
		pushed = true;
	}
	limit = kc_stripe_limit(&b->opts);
	while (done < b->nrows)
	{
		uint32		n = Min(limit, b->nrows - done);
		uint64		first_row;
		uint32		j;

		kc_write_stripe(st, b, rel, done, n, false, &first_row);

		/*
		 * A table that reserves hands the numbers back to the caller (the TID of
		 * the row that was just inserted): a stripe written at once gets its
		 * numbers from the metapage, exactly n of them, nothing to give back.
		 */
		if (b->reserving && rownums != NULL)
			for (j = 0; j < n; j++)
				rownums[done + j] = first_row + j;
		done += n;
	}
	if (pushed)
		PopActiveSnapshot();
	MemoryContextDelete(b->cxt);
	pfree(b);
}

/*
 * Rows of INSERT and COPY.  A multi-insert batch is one write-through call
 * when it is late, so that a COPY run by a late callback makes stripes of its
 * batches and not one stripe per row.
 */
void
kc_write_rows(Relation rel, TupleTableSlot **slots, int nslots, CommandId cid,
			  uint64 *rownums)
{
	int			i;

	if (nslots > 0 && kc_write_refused(rel, "multi insert"))
		return;
	if (nslots > 0 && kc_write_is_late())
	{
		Datum	  **vv = palloc(sizeof(Datum *) * nslots);
		bool	  **nn = palloc(sizeof(bool *) * nslots);

		kc_write_check_state(rel);
		CheckForSerializableConflictIn(rel, NULL, InvalidBlockNumber);
		for (i = 0; i < nslots; i++)
		{
			slot_getallattrs(slots[i]);
			vv[i] = slots[i]->tts_values;
			nn[i] = slots[i]->tts_isnull;
		}
		kc_write_late(rel, vv, nn, nslots, cid, rownums);
		pgstat_count_heap_insert(rel, nslots);
		pfree(vv);
		pfree(nn);
		return;
	}
	for (i = 0; i < nslots; i++)
	{
		uint64		rn = kc_write_row(rel, slots[i], cid);

		if (rownums != NULL)
			rownums[i] = rn;
	}
}

/* The most memory the write buffers may hold, whatever the write cannot free. */
static Size
kc_ceiling(void)
{
	return kc_allowance() * KC_CEILING_FACTOR;
}

/* Returns false, after a warning, when the row was refused (kc_write_refused). */
bool
kc_write_row_values(Relation rel, Datum *vals, bool *nulls, CommandId cid,
					uint64 *rownum_out)
{
	TupleDesc	desc = RelationGetDescr(rel);
	KcRelState *st;
	KcWriteBuf *b = NULL;
	MemoryContext old = CurrentMemoryContext;
	Size		before = kc_total_bytes;
	uint64		rownum = KC_NO_ROW;

	if (rownum_out != NULL)
		*rownum_out = KC_NO_ROW;
	if (kc_write_refused(rel, "insert"))
		return false;
	kc_write_check_state(rel);

	/*
	 * A write conflicts with transactions that read the table (SSI), except
	 * the copy of rows that readers see already (compaction): that writes
	 * nothing, logically.
	 */
	if (!kc_maintenance_write)
		CheckForSerializableConflictIn(rel, NULL, InvalidBlockNumber);

	/*
	 * Late: written at once, not buffered (kc_write_late).  The buffers are
	 * not looked at, so nothing here can reach a buffer that a prepared
	 * hand-over holds, flush one, or add to one.
	 */
	if (kc_write_is_late())
	{
		kc_write_late(rel, &vals, &nulls, 1, cid, rownum_out);
		return true;
	}

	st = kc_state_get(RelationGetRelid(rel), true);

	/*
	 * The ceiling.  What the writer cannot write (rows of enclosing
	 * subtransactions, rows that an open cursor reads) is not bounded by the
	 * allowance, and neither is the number of such levels and cursors a role can
	 * open; so the memory of the write buffers is bounded here instead, by
	 * refusing the write that would go on from above the ceiling.  The memory
	 * is as much as the ceiling plus the row being written.
	 */
	if (!kc_maintenance_write && kc_total_bytes > kc_ceiling())
	{
		kc_flush_pressure();
		/* the look may have dropped the state of a relation, this one's too */
		st = kc_state_get(RelationGetRelid(rel), true);
		if (kc_total_bytes > kc_ceiling())
		{
			KC_PATH(KC_PATH_CEILING_WRITE);
			ereport(ERROR,
					(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
					 errmsg("the write buffers of columnar tables hold " UINT64_FORMAT " bytes, more than the limit of " UINT64_FORMAT " bytes",
							(uint64) kc_total_bytes, (uint64) kc_ceiling()),
					 errdetail("The limit is twice kingres_columnar.write_buffer_memory.  Rows of enclosing subtransactions and rows that an open cursor reads cannot be written yet."),
					 errhint("Release or commit subtransactions, close cursors, or raise kingres_columnar.write_buffer_memory.")));
		}
	}

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

		/*
		 * A row goes only into a buffer made under the same setting (one that
		 * reserves gives its rows numbers, the others never do), and a buffer
		 * that reserves is full when it has used the numbers it took.
		 */
		else if (b->reserve_asked != kc_reserve_asked(st, rel) ||
				 (b->reserving && b->res_valid && b->nrows >= b->res_count))
			b = NULL;
	}
	if (b == NULL)
		b = kc_buf_create(st, rel, desc->natts);
	else if (b->ncols != desc->natts)
		kc_buf_widen(b, rel);
	MemoryContextSwitchTo(old);

	/*
	 * The first row of a buffer that reserves takes the numbers of a stripe and
	 * inserts the stripe's catalog row, "reserved"; this row has the next
	 * number, which is the TID the insert returns (design 5.3).  Before the row
	 * is put: if the reservation fails the transaction or subtransaction ends
	 * and the buffer with it, and if the put fails the buffer holds a
	 * reservation and no row, which the flush at the end gives back
	 * (kc_buf_release_reservation).
	 */
	if (b->reserving)
	{
		if (!b->res_valid)
			kc_buf_reserve_numbers(b, rel);
		rownum = b->res_first + b->nrows;
	}

	if (!b->reserving && !kc_maintenance_write && kc_rel_reserves(rel))
		ereport(ERROR,
				(errcode(ERRCODE_INTERNAL_ERROR),
				 errmsg("a row of columnar table \"%s\", which has an index, cannot be given a row number now",
						RelationGetRelationName(rel)),
				 errdetail("The file is of format version 2, or the catalog is not the one of version 0.2.")));
	kc_buf_put_row(b, desc, vals, nulls, cid);
	if (rownum_out != NULL)
		*rownum_out = rownum;

	/*
	 * The compaction buffer is private and written as stripes when it holds a
	 * stripe of rows or the allowance, whichever comes first.
	 */
	if (kc_maintenance_write)
	{
		if (b->nrows >= kc_stripe_limit(&b->opts) || b->bytes >= kc_allowance())
			kc_write_maintenance_flush(rel);
		return true;
	}

	if (b->nrows >= (b->reserving ? b->res_count : kc_stripe_limit(&b->opts)) &&
		kc_buf_flushable(st, b))
	{
		Relation	r = try_table_open(b->relid, RowExclusiveLock);

		if (r != NULL)
		{
			kc_flush_buf_ex(st, b, r, true);
			table_close(r, NoLock);
			KC_PATH(KC_PATH_SIZE_FLUSH);
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
	return true;
}

uint64
kc_write_row(Relation rel, TupleTableSlot *slot, CommandId cid)
{
	uint64		rownum = KC_NO_ROW;

	slot_getallattrs(slot);
	if (kc_write_row_values(rel, slot->tts_values, slot->tts_isnull, cid, &rownum))
		pgstat_count_heap_insert(rel, 1);
	return rownum;
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
	Size		bytes = KC_BUF_OVERHEAD;	/* the tail is a buffer of its own once installed */
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

/*
 * Make the tail the buffer's rows and give back the rest.  Cannot fail.  The
 * tail of a prepared hand-over has been charged since it was built (it holds
 * memory from then on, next to the rows it was made from); any other tail is
 * charged here.
 */
static void
kc_buf_tail_install(KcWriteBuf *b, KcTail *t, bool charged)
{
	MemoryContext ocxt = b->cxt;

	kc_total_bytes -= Min(kc_total_bytes, b->bytes);
	if (!charged)
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
	kc_buf_tail_install(b, &t, false);
}


/*
 * The rows [first, first + count) of a buffer in the form of heap tuples for logical decoding
 * (increment C7; see kc_decode in kc_storage.c).  Written by the transaction that writes the
 * stripe, a few hundred rows or about a megabyte to a record.  Not for the copies compaction makes
 * (maintenance), which are no new rows.
 */
static void
kc_log_logical_rows(Relation rel, KcWriteBuf *b, uint32 first, uint32 count)
{
	TupleDesc	desc = RelationGetDescr(rel);
	int			natts = desc->natts;
	Datum	   *v = palloc(sizeof(Datum) * Max(natts, 1));
	bool	   *n = palloc(sizeof(bool) * Max(natts, 1));
	StringInfoData sb;
	uint32		nt = 0;
	uint32		r;
	int			c;

	/* the transaction has an id: the stripe's catalog row was written by it */
	(void) GetCurrentTransactionId();
	initStringInfo(&sb);
	for (r = first; r < first + count; r++)
	{
		HeapTuple	tup;

		for (c = 0; c < natts; c++)
		{
			if (c < b->ncols)
			{
				Size		idx = (Size) r * b->ncols + c;

				v[c] = b->vals[idx];
				n[c] = b->nulls[idx];
			}
			else
			{
				v[c] = (Datum) 0;
				n[c] = true;
			}
		}
		tup = heap_form_tuple(desc, v, n);
		kc_logical_append_tuple(&sb, tup);
		heap_freetuple(tup);
		nt++;
		if (nt >= 512 || sb.len >= 1024 * 1024)
		{
			kc_xlog_logical(rel, nt, sb.data, sb.len);
			resetStringInfo(&sb);
			nt = 0;
		}
	}
	if (nt > 0)
		kc_xlog_logical(rel, nt, sb.data, sb.len);
	pfree(sb.data);
	pfree(v);
	pfree(n);
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
				uint32 count, bool maintenance, uint64 *first_row_out)
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
	bool		reserved = b->res_valid && !maintenance;
	bool		locked = false;

	/*
	 * A stripe of a buffer that holds a reservation is the front of the numbers
	 * it took: the rows [0, count) of the buffer, which is the whole buffer (it
	 * never holds more than it reserved).  Any other stripe takes its numbers
	 * from the metapage now.
	 */
	Assert(count > 0 &&
		   count <= (reserved ? b->res_count : kc_stripe_limit(&b->opts)));
	Assert(!reserved || (first == 0 && count == b->nrows));
	KC_FAULT(KC_FAULT_ALLOC);
	fcxt = AllocSetContextCreate(CurrentMemoryContext, "kingres_columnar flush",
								 ALLOCSET_DEFAULT_SIZES);
	MemoryContextSwitchTo(fcxt);
	initStringInfo(&stats);
	initStringInfo(&cidpts);

	if (reserved)
	{
		stripe_id = b->res_stripe_id;
		first_row = b->res_first;
	}
	else
	{
		kc_reserve_lock(rel, ShareLock);
		kc_meta_allocate(rel, count, &stripe_id, &first_row);
		locked = true;
	}
	if (first_row_out != NULL)
		*first_row_out = first_row;
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
	if (!reserved)
		kc_catalog_insert(rel, &si, cidpts.data, cidpts.len, stats.data, stats.len,
						  st->frozen, maintenance);
	if (locked)
		kc_reserve_unlock(rel, ShareLock);
	else
	{
		uint64		res_first = b->res_first;
		uint32		res_count = b->res_count;

		/*
		 * The pages are written; the reserved row becomes the stripe's row.  Its
		 * claim shrinks to the rows that are there (row_count) before the unused
		 * numbers are offered back, so no number is ever free while the catalog
		 * still says a writer holds it.
		 */
		kc_reserve_gate(KC_RES_AFTER_PAGES);
		kc_pass_command(b);
		kc_catalog_update_reserved(rel, &si, cidpts.data, cidpts.len,
								   stats.data, stats.len);
		b->res_valid = false;
		kc_reserve_gate(KC_RES_AFTER_UPDATE);
		kc_give_back_tail(rel, res_first + count, res_count - count);
	}
	kc_flush_epoch++;

	/* the rows for logical decoding (C7), by the transaction that writes the stripe */
	if (!maintenance && XLogLogicalInfoActive() && RelationIsLogicallyLogged(rel))
		kc_log_logical_rows(rel, b, first, count);

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
		kc_buf_forget_reservation(b);
		kc_buf_clear(b);
		return;
	}

	KC_FAULT(KC_FAULT_FLUSH);

	/*
	 * A buffer that reserves holds one stripe, at most the numbers it took: it
	 * is written whole, as the front of them, or (with keep_tail, the flush by
	 * size) left alone until it has used them all.
	 */
	if (b->res_valid)
	{
		if (keep_tail && b->nrows < b->res_count)
			return;
		kc_write_stripe(st, b, rel, 0, b->nrows, false, NULL);
		kc_buf_clear(b);
		return;
	}
	limit = kc_stripe_limit(&b->opts);
	while (done < b->nrows)
	{
		uint32		n = Min(limit, b->nrows - done);

		if (keep_tail && n < limit)
			break;
		kc_write_stripe(st, b, rel, done, n, false, NULL);
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

	/* where an error is forbidden nothing is written (kc_write_state_forbids_error) */
	if (st == NULL || kc_write_state_forbids_error())
		return;
	kc_coalesce(st);
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);

		if (b->nrows > 0 && kc_buf_flushable(st, b))
			kc_flush_buf(st, b, rel);
	}
}

/*
 * A plan that has a Gather runs the scans of columnar tables in parallel
 * workers, which cannot see rows that exist only in this backend's memory.
 * Called by the custom scan of the leader when it starts such a plan (before
 * the executor enters parallel mode, where a catalog row could not be written):
 * the running subtransaction's buffers of the relation become stripes (design
 * 7.1; the rows of the commands that ran before are then visible by the
 * stripe's command ids exactly as in a serial scan), and what cannot be
 * written (a buffer of an enclosing subtransaction, which the stripe row's
 * transaction id would not follow; a buffer another scan of this backend
 * reads) makes the plan fail with a message, never answer without those rows.
 */
void
kc_write_prepare_for_workers(Relation rel)
{
	KcRelState *st;
	ListCell   *lc;
	uint64		uid;

	kc_write_flush_relation(rel);
	st = kc_state_get(RelationGetRelid(rel), false);
	if (st == NULL)
		return;
	uid = kc_rel_ident(rel)->uid;
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);
		uint32		first;
		uint32		end;

		kc_buf_visible(b, &first, &end);
		if (end > first && b->uid == uid)
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("columnar table \"%s\" has rows that this transaction wrote and parallel workers cannot read",
							RelationGetRelationName(rel)),
					 errdetail("The rows were written by an enclosing subtransaction, or a scan of this session is still reading them."),
					 errhint("Run the query with max_parallel_workers_per_gather = 0, or after the subtransaction has ended.")));
	}
}

/*
 * Does this backend hold rows of rel that kc_write_prepare_for_workers could
 * not make visible to workers?  The planner asks, and offers no parallel path
 * for such a relation (a plan prepared earlier and run later can still meet the
 * rows, and then fails with the message of kc_write_prepare_for_workers).
 */
bool
kc_write_parallel_blocked(Relation rel)
{
	KcRelState *st = kc_state_get(RelationGetRelid(rel), false);
	ListCell   *lc;
	uint64		uid;

	if (st == NULL)
		return false;
	uid = kc_rel_ident(rel)->uid;
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);
		uint32		first;
		uint32		end;

		kc_buf_visible(b, &first, &end);
		if (end > first && b->uid == uid &&
			(kc_write_state_forbids_error() || !kc_buf_flushable(st, b)))
			return true;
	}
	return false;
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

	/*
	 * The rows a prepared hand-over holds are not forgotten from here: the
	 * hand-over publishes them (or the abort drops them) and nothing else.  A
	 * callback of another module that truncates the table in that window is
	 * refused; the subtransaction aborts cleanly.
	 */
	if (kc_handover_prepared && kc_rel_handover_prepared(st))
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("columnar table cannot be truncated while a subtransaction is being committed"),
				 errdetail("A callback of another module truncated the table between the two phases of the commit of a subtransaction.")));
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);

		if (b->subid == GetCurrentSubTransactionId())
		{
			kc_buf_forget_reservation(b);
			kc_buf_clear(b);
		}
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
static void
kc_write_note_file(Oid relid, uint64 uid)
{
	MemoryContext old;
	KcReplaced *r;
	ListCell   *lc;

	foreach(lc, kc_replaced)
	{
		r = lfirst(lc);

		if (r->relid == relid && r->uid == uid &&
			r->subid == GetCurrentSubTransactionId())
			return;
	}
	old = MemoryContextSwitchTo(TopTransactionContext);
	r = palloc(sizeof(KcReplaced));
	r->relid = relid;
	r->uid = uid;
	r->subid = GetCurrentSubTransactionId();
	kc_replaced = lappend(kc_replaced, r);
	MemoryContextSwitchTo(old);
}

/*
 * What a note says (this file is going away) is true only as long as the
 * subtransaction that noted it is: a drop or a rewrite that a rollback to a
 * savepoint undoes leaves the file in use, and the relation that was dropped
 * (the transient table of a rewrite) does not exist any more either, so the
 * cleanup could not tell the two apart.  A note therefore follows the
 * subtransaction that made it: it goes with an abort and becomes the parent's
 * with a commit.
 */
static void
kc_replaced_subxact(SubXactEvent event, SubTransactionId mySubid,
					SubTransactionId parentSubid)
{
	ListCell   *lc;

	if (kc_replaced == NIL ||
		(event != SUBXACT_EVENT_COMMIT_SUB && event != SUBXACT_EVENT_ABORT_SUB))
		return;
	foreach(lc, kc_replaced)
	{
		KcReplaced *r = lfirst(lc);

		if (r->subid != mySubid)
			continue;
		if (event == SUBXACT_EVENT_COMMIT_SUB)
			r->subid = parentSubid;
		else
		{
			kc_replaced = foreach_delete_current(kc_replaced, lc);
			pfree(r);
		}
	}
}

void
kc_write_note_replaced(Relation rel)
{
	if (!kc_rel_is_columnar(rel))
		return;
	kc_write_note_file(RelationGetRelid(rel), kc_rel_ident(rel)->uid);
}

/*
 * A relation is being dropped (the object access hook, before the catalog
 * rows go): if it holds a columnar file, note the file.  This is how the
 * access method itself learns that a file is going away, whatever the command
 * (DROP TABLE, DROP SCHEMA, DROP OWNED, DROP EXTENSION, and the drop of the
 * transient table at the end of a rewrite, which by then holds the table's
 * OLD file, whatever the access method it is rewritten to): no event trigger
 * is involved, so cleanup does not depend on one firing.  Noting is not
 * deleting: a drop that is rolled back (a savepoint, an error) leaves the
 * relation with the file, and the cleanup compares (kc_write_cleanup_replaced).
 * Only reads, nothing here can fail for a reason of the relation.
 */
void
kc_write_note_dropped(Oid relid)
{
	HeapTuple	tup;

	tup = SearchSysCache1(RELOID, ObjectIdGetDatum(relid));
	if (HeapTupleIsValid(tup))
	{
		Form_pg_class cl = (Form_pg_class) GETSTRUCT(tup);

		/* by the handler of the access method, not by its name (C11-02) */
		if (kc_am_is_columnar(cl->relam) && OidIsValid(cl->relfilenode) &&
			(cl->relkind == RELKIND_RELATION || cl->relkind == RELKIND_MATVIEW))
		{
			uint64		uid = kc_catalog_file_uid(cl);

			if (uid != 0)
				kc_write_note_file(relid, uid);
		}
		ReleaseSysCache(tup);
	}
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

	/* a transaction that dropped the extension has no catalog to clean */
	if (todo != NIL && !kc_catalog_available())
	{
		list_free_deep(todo);
		return 0;
	}
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
					   Datum **vals, bool **nulls, int *ncols, CommandId *cid,
					   uint64 *rownum)
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
		/* the number a row of a buffer that reserves was given when it was inserted */
		*rownum = (b->reserving && b->res_valid) ? b->res_first + rowno : KC_NO_ROW;
		return true;
	}
	return false;
}

/*
 * The row with this number in a buffer of this transaction that reserved it
 * (C6b: an index scan fetches a row its own transaction inserted and has not
 * written).  Only a buffer that reserves has numbers.  The pointers are valid
 * until the next write to the relation.
 */
bool
kc_write_pending_by_number(Oid relid, uint64 uid, uint64 row, Datum **vals,
						   bool **nulls, int *ncols, CommandId *cid)
{
	KcRelState *st = kc_state_get(relid, false);
	ListCell   *lc;

	if (st == NULL)
		return false;
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);
		uint32		first;
		uint32		end;

		if (!b->reserving || !b->res_valid || b->uid != uid)
			continue;
		kc_buf_visible(b, &first, &end);
		if (row < b->res_first + first || row >= b->res_first + end)
			continue;
		*vals = &b->vals[(Size) (row - b->res_first) * b->ncols];
		*nulls = &b->nulls[(Size) (row - b->res_first) * b->ncols];
		*ncols = b->ncols;
		*cid = b->cids[row - b->res_first];
		return true;
	}
	return false;
}

/* Does this transaction hold rows of the table that are only in memory? */
bool
kc_write_has_unwritten(Relation rel)
{
	KcRelState *st = kc_state_get(RelationGetRelid(rel), false);
	ListCell   *lc;
	uint64		uid = kc_rel_ident(rel)->uid;

	if (st == NULL)
		return false;
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);

		if (b->nrows > 0 && b->uid == uid)
			return true;
	}
	return false;
}

/*
 * A first index on a table whose transaction holds rows in buffers: those rows
 * have no numbers (their buffers were made before the table had an index) or
 * are still only in memory, and the build scan reads stripes.  The buffers of the
 * running subtransaction are written first, so the build sees every row with the
 * number it has.  What cannot be written (a buffer of an enclosing subtransaction,
 * one that a scan reads) makes the statement fail, never build an index that
 * misses rows.
 */
void
kc_write_flush_for_index(Relation rel)
{
	KcRelState *st;
	ListCell   *lc;
	uint64		uid;

	kc_write_flush_relation(rel);
	st = kc_state_get(RelationGetRelid(rel), false);
	if (st == NULL)
		return;
	uid = kc_rel_ident(rel)->uid;
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);
		uint32		first;
		uint32		end;

		kc_buf_visible(b, &first, &end);
		if (end > first && b->uid == uid)
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("cannot build an index on columnar table \"%s\": this transaction holds rows of it that cannot be written yet",
							RelationGetRelationName(rel)),
					 errdetail("The rows were written by an enclosing subtransaction, or a scan of this session is still reading them."),
					 errhint("Build the index in the subtransaction that wrote the rows, or after the savepoint is released, or close the cursor.")));
	}
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

			/* an empty buffer that still holds a reservation gives it up below */
			if (b->nrows == 0 && !b->res_valid)
				continue;
			if (b->subid != GetCurrentSubTransactionId())
				elog(ERROR, "columnar write buffer of an inner subtransaction survived to commit");
			if (rel == NULL)
			{
				rel = try_table_open(st->relid, RowExclusiveLock);
				if (rel == NULL)
				{
					/* dropped in this transaction */
					kc_buf_forget_reservation(b);
					kc_buf_clear(b);
					continue;
				}
			}
			if (b->nrows == 0)
			{
				if (!kc_buf_is_stale(b, rel))
					kc_buf_release_reservation(b, rel);
				kc_buf_forget_reservation(b);
				continue;
			}
			kc_flush_buf(st, b, rel);
		}
		if (rel != NULL)
			table_close(rel, NoLock);
	}
	if (pushed)
		PopActiveSnapshot();
}

/*
 * Files noted as going away after the cleanup of the transaction had run (a
 * callback of another module that drops or rewrites a table at pre-commit with
 * event triggers off): the transaction commits, and nothing in it can delete the
 * stripe rows any more, because it is not in progress.  They are kept, as
 * (relation, identity) pairs, and the next transaction of this backend that
 * reaches its pre-commit deletes the rows of those that are gone (the same
 * comparison as the cleanup of the transaction itself, so a file still in use
 * keeps its rows).  A backend that exits first leaves them to VACUUM and
 * columnar_check(true), which find every such row.  At most KC_ORPHAN_MAX files
 * are kept; any beyond are left to the same two.
 */
#define KC_ORPHAN_MAX	KC_ORPHAN_NOTES_MAX

/*
 * The kept files live in static memory, an array that is never allocated or
 * freed: they are noted at XACT_EVENT_COMMIT, where an allocation that fails is
 * a PANIC, so that moment moves 24 bytes per file and nothing else.  The array
 * is bss: pages that are never written are never made by the system.
 */
static KcReplaced kc_orphans[KC_ORPHAN_MAX];
static int	kc_orphans_n = 0;
static bool kc_orphans_adopted = false;

static int
kc_orphans_kept_for_report(void)
{
	return kc_orphans_n;
}

static void
kc_orphans_keep(void)
{
	ListCell   *lc;

	if (kc_replaced == NIL || !kc_commit_flushed)
		return;
#ifdef USE_ASSERT_CHECKING
	/* a build with assertions traps an allocation here, as it would in the critical section it stands for */
	START_CRIT_SECTION();
#endif
	foreach(lc, kc_replaced)
	{
		if (kc_orphans_n >= kc_guc_orphan_notes_max)
			break;
		kc_orphans[kc_orphans_n++] = *((KcReplaced *) lfirst(lc));
	}
#ifdef USE_ASSERT_CHECKING
	END_CRIT_SECTION();
#endif
}

/* make the kept files part of this transaction's notes, to be cleaned with them */
static void
kc_orphans_adopt(void)
{
	int			i;
	MemoryContext old;

	if (kc_orphans_n == 0)
		return;
	old = MemoryContextSwitchTo(TopTransactionContext);
	for (i = 0; i < kc_orphans_n; i++)
	{
		KcReplaced *c = palloc(sizeof(KcReplaced));

		*c = kc_orphans[i];
		c->subid = GetCurrentSubTransactionId();
		kc_replaced = lappend(kc_replaced, c);
	}
	MemoryContextSwitchTo(old);
	kc_orphans_adopted = true;
}

/* the transaction that adopted the kept files has committed: their rows are deleted */
static void
kc_orphans_done(void)
{
	if (kc_orphans_adopted)
		kc_orphans_n = 0;
	kc_orphans_adopted = false;
}

static void
kc_xact_callback(XactEvent event, void *arg)
{
	switch (event)
	{
		case XACT_EVENT_PRE_COMMIT:
		case XACT_EVENT_PRE_PREPARE:
			/*
			 * A call was refused while this transaction was alive (the commit
			 * of a subtransaction, a critical section): it cannot commit.  An
			 * error is allowed here, and it is the last moment that one is.
			 */
			if (kc_lost_count > 0)
				kc_lost_raise();
			/*
			 * Callbacks of other modules that run after this one can still
			 * write: from here a write goes to a stripe at once
			 * (kc_write_late), because nothing writes a buffer after this
			 * callback.  Set first, so that the flush below is the last
			 * thing that touches the buffers.
			 */
			kc_commit_flushed = true;
			kc_flush_all();
			if (event == XACT_EVENT_PRE_COMMIT)
				kc_orphans_adopt();
			if (kc_replaced != NIL)
			{
				/* the catalog deletes want a snapshot; none is active at pre-commit */
				bool		pushed = false;
				int			n;

				if (!ActiveSnapshotSet())
				{
					PushActiveSnapshot(GetTransactionSnapshot());
					pushed = true;
				}
				n = kc_write_cleanup_replaced();
				if (kc_orphans_adopted)
					kc_path_hits[KC_PATH_ORPHANS_CLEANED] += n;
				if (pushed)
					PopActiveSnapshot();
			}
			break;
		case XACT_EVENT_COMMIT:
			/* files dropped after the cleanup: kept for the next transaction */
			kc_orphans_done();
			kc_orphans_keep();
			kc_replaced = NIL;
			kc_state_reset();
			kc_ending_reset();
			break;
		case XACT_EVENT_PREPARE:
		case XACT_EVENT_ABORT:
			kc_orphans_adopted = false;
			kc_replaced = NIL;
			kc_state_reset();
			kc_ending_reset();
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
 * goes to a stripe at once (kc_write_late) and touches no buffer.  A scan that began in the window is
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
	/*
	 * A buffer that reserves is never merged into another, nor another into it
	 * (decision 3 of C6a): its rows have numbers, one stripe is its life.
	 */
	return prev != NULL && prev->subid == parentSubid &&
		prev->ncols == b->ncols && prev->uid == b->uid &&
		b->refs == 0 && prev->refs == 0 && !b->reserving && !prev->reserving;
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
	/* real memory from now until phase two, next to the rows it copies */
	kc_total_bytes += b->merge_charge;
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

		kc_write_stripe(st, prev, rel, done, n, false, NULL);
		done += n;
	}
	table_close(rel, NoLock);
	if (pushed)
		PopActiveSnapshot();
	KC_PATH(KC_PATH_HANDOVER_FLUSH);

	/*
	 * The stripes are written; whatever happens next, the rows in them must
	 * not be read from the buffers as well.
	 */
	prev->vis_from = Min(upto, prev->nrows);
	b->vis_from = upto > prev->nrows ? upto - prev->nrows : 0;
	b->merge_written = upto;

	kc_buf_tail_build(prev, upto, total, &tail);
	if (b->tail_ready)
	{
		kc_total_bytes -= Min(kc_total_bytes, b->tail.bytes);
		MemoryContextDelete(b->tail.cxt);
	}
	/* the tail is memory from here on, next to the rows it was made from */
	kc_total_bytes += tail.bytes;
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
		/* the copy of the values dies with the context the tail replaces */
		kc_total_bytes -= Min(kc_total_bytes, b->merge_charge);
		kc_buf_tail_install(prev, &b->tail, true);
		b->tail_ready = false;
	}
	else
	{
		prev->nrows = b->merge_base + b->merge_rows;
		/* counted since phase one; now it is the parent buffer's */
		kc_total_bytes -= Min(kc_total_bytes, b->merge_charge);
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
	{
		kc_total_bytes -= Min(kc_total_bytes, b->tail.bytes);
		MemoryContextDelete(b->tail.cxt);
	}
	if (b->merge_chunk != NULL)
	{
		kc_total_bytes -= Min(kc_total_bytes, b->merge_charge);
		pfree(b->merge_chunk);
	}
	kc_total_bytes -= Min(kc_total_bytes, b->bytes);
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

	/*
	 * A buffer that takes part in a prepared hand-over (as the child or as the
	 * parent that takes the rows) is not joined to another: its address and
	 * its rows are named by the prepared state.
	 */
	if (kc_handover_prepared)
		return;
	foreach(lc, st->bufs)
	{
		KcWriteBuf *b = lfirst(lc);

		if (prev != NULL && b->subid == prev->subid && b->ncols == prev->ncols &&
			b->uid == prev->uid && b->refs == 0 && prev->refs == 0 &&
			b->merge_into == NULL && prev->vis_from == 0 &&
			!b->reserving && !prev->reserving)
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
		kc_buf_forget_reservation(b);
		kc_buf_clear(b);
		return;
	}
	if (b->nrows == 0)
	{
		/* a reservation and no row (a failed insert): give it back */
		if (!kc_buf_is_stale(b, rel))
			kc_buf_release_reservation(b, rel);
		kc_buf_forget_reservation(b);
		table_close(rel, NoLock);
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
	KC_PATH(KC_PATH_OWN_FLUSH);
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

			/*
			 * A buffer that reserves is written by the subtransaction that
			 * inserted into it, as a stripe of its own, and never merged into the
			 * parent's (decision 3 of C6a): its rows have numbers and a catalog
			 * row of their own, inserted by this subtransaction, which the parent
			 * could not write on its behalf.  One that a scan reads cannot be
			 * written; it stays as it is and becomes the parent's, and the parent
			 * writes it later.  Cost: one stripe per savepoint that wrote.
			 */
			if (b->reserving)
			{
				if ((b->nrows > 0 && b->refs == 0) || (b->nrows == 0 && b->res_valid))
					kc_flush_own_buffer(st, b);
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
				/*
				 * From here a buffer is prepared: nothing may flush, free,
				 * move or add to a buffer until phase two or the abort of the
				 * subtransaction (kc_buf_flushable, kc_write_is_late).
				 */
				kc_handover_prepared = true;
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
		Size		before = pairs[i].prev->bytes + pairs[i].b->bytes +
			pairs[i].b->merge_charge +
			(pairs[i].b->tail_ready ? pairs[i].b->tail.bytes : 0);
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

	/*
	 * The ceiling (kc_write_row_values) holds for the hand-over too: what the
	 * parent level would hold once the child's rows are its own.  An error
	 * here aborts the committing subtransaction, which drops what phase one
	 * made.
	 */
	if (effective > kc_ceiling())
	{
		KC_PATH(KC_PATH_CEILING_HANDOVER);
		ereport(ERROR,
				(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
				 errmsg("the write buffers of columnar tables would hold " UINT64_FORMAT " bytes after this subtransaction, more than the limit of " UINT64_FORMAT " bytes",
						(uint64) effective, (uint64) kc_ceiling()),
				 errdetail("The limit is twice kingres_columnar.write_buffer_memory.  Rows of enclosing subtransactions and rows that an open cursor reads cannot be written yet."),
				 errhint("Release or commit subtransactions, close cursors, or raise kingres_columnar.write_buffer_memory.")));
	}
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
			else if ((b->nrows == 0 && prev != NULL && prev->subid == parentSubid &&
					  prev->ncols == b->ncols && prev->uid == b->uid) ||
					 (b->reserving && b->nrows == 0 && !b->res_valid && b->refs == 0))
			{
				/* (a buffer that reserves and holds nothing, not even a claim, is of no use to the parent) */
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
	/*
	 * What the refusal rule needs to know (kc_ending), kept even when this
	 * module has no buffers: the first thing, so that a callback of another
	 * module that runs after this one finds the end of the subtransaction
	 * recorded.
	 */
	switch (event)
	{
		case SUBXACT_EVENT_PRE_COMMIT_SUB:
			/* a refusal that this subtransaction owns: it cannot commit */
			if (kc_lost_count > 0 && kc_lost_owner == mySubid)
				kc_lost_raise();
			kc_sub_precommitted = mySubid;
			break;
		case SUBXACT_EVENT_COMMIT_SUB:
			kc_sub_ended = mySubid;
			kc_sub_ended_parent = parentSubid;
			kc_sub_ended_committed = true;
			kc_sub_precommitted = InvalidSubTransactionId;
			/* what it lost is now the parent's, which cannot commit either */
			if (kc_lost_count > 0 && kc_lost_owner == mySubid)
				kc_lost_owner = parentSubid;
			break;
		case SUBXACT_EVENT_ABORT_SUB:
			kc_sub_ended = mySubid;
			kc_sub_ended_parent = parentSubid;
			kc_sub_ended_committed = false;
			kc_sub_precommitted = InvalidSubTransactionId;
			/* what it lost goes with it */
			if (kc_lost_count > 0 && kc_lost_owner == mySubid)
				kc_lost_clear();
			break;
		default:
			break;
	}
	kc_replaced_subxact(event, mySubid, parentSubid);
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

	/*
	 * Developer settings of C6a, superuser only, for tests (README).  Until a
	 * table has an index (C6b makes a table that has one reserve by itself) no
	 * table needs row numbers at insert, so the tests switch the reservation on
	 * for a session.  Off, nothing of the write path changes: the same stripes,
	 * the same row numbers.  reserve_gate and reserve_gate_at stop a writer at a
	 * point of its reservation, the way check_gate stops the check: the writer
	 * waits while another session of a superuser holds the advisory lock (key,
	 * 0), so that a test can stop the server at a moment it can name.
	 */
	DefineCustomBoolVariable("kingres_columnar.reserve_row_numbers",
							 "Reserve row numbers when a row is inserted, so that its TID exists then (testing, until a table can have an index).",
							 NULL, &kc_guc_reserve_rows, false,
							 PGC_SUSET, GUC_NOT_IN_SAMPLE, NULL, NULL, NULL);
	DefineCustomIntVariable("kingres_columnar.reserve_gate",
							"Advisory lock key that a writer that reserves waits on at the point reserve_gate_at names (testing).",
							NULL, &kc_guc_reserve_gate, 0, 0, 1000000,
							PGC_SUSET, GUC_NOT_IN_SAMPLE, NULL, NULL, NULL);
	DefineCustomIntVariable("kingres_columnar.reserve_gate_at",
							"The point of the reservation where reserve_gate waits: 1 numbers taken, 2 reserved row inserted, 3 pages written, 4 row updated (testing).",
							NULL, &kc_guc_reserve_gate_at, 0, 0, 5,
							PGC_SUSET, GUC_NOT_IN_SAMPLE, NULL, NULL, NULL);

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
