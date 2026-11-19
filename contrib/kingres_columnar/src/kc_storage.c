/*-------------------------------------------------------------------------
 *
 * kc_storage.c
 *	  The relation file of a columnar table: the metapage, the pages of a
 *	  stripe, and the resource manager that logs them.
 *
 * Block 0 is the metapage (options and the two counters).  Every other page
 * belongs to a stripe: a stripe is a run of consecutive blocks holding the
 * stripe blob.  A page is written once, logged as a whole-page image by the
 * resource manager "KingresColumnar", and never changed again, so redo has no
 * logic of its own: it restores images.  The metapage is changed in place
 * under an exclusive buffer lock and is also logged as a whole-page image.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kc_storage.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/bufmask.h"
#include "access/genam.h"
#include "access/table.h"
#include "access/transam.h"
#include "access/xact.h"
#include "access/xlog_internal.h"
#include "access/xloginsert.h"
#include "access/xlogutils.h"
#include "catalog/indexing.h"
#include "catalog/pg_class.h"
#include "catalog/storage.h"
#include "miscadmin.h"
#include "port/pg_crc32c.h"
#include "storage/bufmgr.h"
#include "storage/bufpage.h"
#include "storage/bulk_write.h"
#include "storage/lmgr.h"
#include "storage/smgr.h"
#include "storage/standby.h"
#include "utils/fmgroids.h"
#include "utils/inval.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"

#include "kingres_columnar.h"

#define XLOG_KC_PAGES		0x00	/* whole-page images of a stripe */
#define XLOG_KC_META		0x10	/* whole-page image of the metapage */
#define XLOG_KC_FREEZE		0x20	/* metapage image, its transaction id frozen */
#define XLOG_KC_RETURN		0x30	/* metapage image, the tail of a reservation given back (0.2) */

typedef struct xl_kc_pages
{
	uint64		stripe_id;
	uint32		nblocks;
} xl_kc_pages;

/* The record that freezes the metapage's transaction id (plus the page image). */
typedef struct xl_kc_freeze
{
	TransactionId horizon;		/* the id that was frozen: standby queries holding it conflict */
} xl_kc_freeze;

/*
 * The record that gives the unused tail of a reservation of row numbers back
 * (plus the page image of the metapage, like every record of this manager).
 * Redo restores the image and ignores the fields; they say what happened to
 * a person who reads the log.
 */
typedef struct xl_kc_return
{
	uint64		first_row;		/* the first number given back */
	uint32		nrows;			/* how many */
} xl_kc_return;

#define KC_MAX_BLOCKS_PER_RECORD 32

/* ------------------------------------------------------------------------
 * Errors and checksums
 * ------------------------------------------------------------------------
 */

void
kc_corrupt(Relation rel, uint64 stripe_id, const char *fmt,...)
{
	char		msg[256];
	va_list		args;

	va_start(args, fmt);
	vsnprintf(msg, sizeof(msg), fmt, args);
	va_end(args);

	ereport(ERROR,
			(errcode(ERRCODE_DATA_CORRUPTED),
			 errmsg("columnar table \"%s\" is corrupt: stripe " UINT64_FORMAT ": %s",
					RelationGetRelationName(rel), stripe_id, msg)));
}

uint64
kc_crc32c(const char *data, uint64 len)
{
	pg_crc32c	crc;

	INIT_CRC32C(crc);
	COMP_CRC32C(crc, data, len);
	FIN_CRC32C(crc);
	return (uint64) crc;
}

/* ------------------------------------------------------------------------
 * The resource manager
 * ------------------------------------------------------------------------
 */

static void
kc_redo(XLogReaderState *record)
{
	uint8		info = XLogRecGetInfo(record) & ~XLR_INFO_MASK;
	int			b;

	if (info != XLOG_KC_PAGES && info != XLOG_KC_META && info != XLOG_KC_FREEZE &&
		info != XLOG_KC_RETURN)
		elog(PANIC, "kingres_columnar_redo: unknown op code %u", info);

	/*
	 * Freezing the id makes the table readable by a snapshot that predates the
	 * rewrite that created the file, which would then see no stripes instead of
	 * getting the serialization failure.  A standby query that could hold the
	 * id is therefore cancelled, as for the removal of a heap tuple.
	 */
	if (info == XLOG_KC_FREEZE && InHotStandby)
	{
		xl_kc_freeze *xlrec = (xl_kc_freeze *) XLogRecGetData(record);
		RelFileLocator rlocator;

		XLogRecGetBlockTag(record, 0, &rlocator, NULL, NULL);
		ResolveRecoveryConflictWithSnapshot(xlrec->horizon, false, rlocator);
	}

	/*
	 * Every block of a record carries a whole-page image, so redo restores
	 * it and has nothing to decide.  A block that needs more than that would
	 * mean a record this version did not write.
	 */
	for (b = 0; b <= XLogRecMaxBlockId(record); b++)
	{
		Buffer		buf;
		XLogRedoAction action;

		if (!XLogRecHasBlockRef(record, b))
			continue;
		action = XLogReadBufferForRedo(record, b, &buf);
		if (action == BLK_NEEDS_REDO)
			elog(PANIC, "kingres_columnar_redo: block %d of a record has no page image", b);
		if (BufferIsValid(buf))
			UnlockReleaseBuffer(buf);
	}
}

static void
kc_desc(StringInfo buf, XLogReaderState *record)
{
	uint8		info = XLogRecGetInfo(record) & ~XLR_INFO_MASK;

	if (info == XLOG_KC_PAGES)
	{
		xl_kc_pages *xlrec = (xl_kc_pages *) XLogRecGetData(record);

		appendStringInfo(buf, "stripe " UINT64_FORMAT ", %u pages",
						 xlrec->stripe_id, xlrec->nblocks);
	}
	else if (info == XLOG_KC_META)
		appendStringInfoString(buf, "metapage");
	else if (info == XLOG_KC_FREEZE)
	{
		xl_kc_freeze *xlrec = (xl_kc_freeze *) XLogRecGetData(record);

		appendStringInfo(buf, "metapage, freeze transaction id %u", xlrec->horizon);
	}
	else if (info == XLOG_KC_RETURN)
	{
		xl_kc_return *xlrec = (xl_kc_return *) XLogRecGetData(record);

		appendStringInfo(buf, "metapage, row numbers " UINT64_FORMAT " and %u more given back",
						 xlrec->first_row, xlrec->nrows);
	}
}

static const char *
kc_identify(uint8 info)
{
	switch (info & ~XLR_INFO_MASK)
	{
		case XLOG_KC_PAGES:
			return "STRIPE_PAGES";
		case XLOG_KC_META:
			return "METAPAGE";
		case XLOG_KC_FREEZE:
			return "FREEZE";
		case XLOG_KC_RETURN:
			return "RETURN";
	}
	return NULL;
}

/* Only the LSN and the checksum may differ between a page and its image. */
static void
kc_mask(char *pagedata, BlockNumber blkno)
{
	mask_page_lsn_and_checksum((Page) pagedata);
}

static const RmgrData kc_rmgr = {
	.rm_name = "KingresColumnar",
	.rm_redo = kc_redo,
	.rm_desc = kc_desc,
	.rm_identify = kc_identify,
	.rm_mask = kc_mask,
};

/*
 * Register the resource manager.  PostgreSQL accepts this only while the
 * shared_preload_libraries are loaded, which is why the extension needs the
 * setting on every node, standbys included.
 */
void
kc_rmgr_register(void)
{
	RegisterCustomRmgr(KC_RMGR_ID, &kc_rmgr);
}

/* ------------------------------------------------------------------------
 * The metapage
 * ------------------------------------------------------------------------
 */

/* The checksum of the metapage payload: a damaged identity would otherwise
 * hide every stripe of the table without an error. */
static void
kc_meta_seal(KcMetaData *m)
{
	m->crc = 0;
	m->crc = (uint32) kc_crc32c((const char *) m, sizeof(KcMetaData));
}

static void
kc_meta_format(Page page, const KcOptions *opts, uint64 next_row,
			   uint64 next_stripe, uint64 uid, TransactionId xid,
			   uint32 flags, Oid relid, TransactionId table_xid,
			   uint32 version)
{
	KcMetaData *m;

	PageInit(page, BLCKSZ, 0);
	m = (KcMetaData *) PageGetContents(page);
	memset(m, 0, sizeof(*m));
	m->magic = KC_META_MAGIC;
	m->version = version;
	m->codec = opts->codec;
	m->level = opts->level;
	m->stripe_rows = opts->stripe_rows;
	m->chunk_rows = opts->chunk_rows;
	m->next_row = next_row;
	m->next_stripe_id = next_stripe;
	m->rel_uid = uid;
	m->created_xid = xid;
	m->flags = flags;
	m->created_relid = relid;
	m->table_xid = table_xid;
	kc_meta_seal(m);
	/* a standard page: the log can drop the unused middle of it */
	((PageHeader) page)->pd_lower =
		(char *) m + sizeof(KcMetaData) - (char *) page;
}

/*
 * Create block 0 of a new relation file.  The file was just created by
 * RelationCreateStorage; the bulk writer logs the page (or syncs the file at
 * commit when wal_level is minimal) as it does for any new relation.
 */
void
kc_meta_init_storage(Relation rel, const RelFileLocator *rlocator,
					 const KcOptions *opts, bool replaces_content,
					 TransactionId table_xid)
{
	SMgrRelation srel = smgropen(*rlocator, INVALID_PROC_NUMBER);
	BulkWriteState *bulk;
	BulkWriteBuffer buf;
	uint64		uid;

	bulk = smgr_bulk_start_smgr(srel, MAIN_FORKNUM, XLogIsNeeded());
	buf = smgr_bulk_get_buf(bulk);
	/*
	 * A fresh identity for every new file (create, TRUNCATE, rewrite).  A
	 * byte-for-byte copy of the file (SET TABLESPACE, a base backup) keeps it,
	 * which is what lets the stripe catalog survive a copy unchanged.
	 */
	for (int tries = 0;; tries++)
	{
		if (!pg_strong_random(&uid, sizeof(uid)))
			elog(ERROR, "could not generate a random identity for a columnar file");
		uid &= PG_INT64_MAX;
		if (uid == 0)
			uid = 1;
		if (!kc_catalog_uid_in_use(uid))
			break;
		if (tries >= 16)
			elog(ERROR, "could not find an unused identity for a columnar file");
	}
	/*
	 * Version 3 only where the stripe catalog can hold what a reservation
	 * leaves in it (0.2); with the catalog of 0.1 the file is what 0.1 writes.
	 */
	kc_meta_format((Page) buf, opts, 0, 1, uid, GetCurrentTransactionId(),
			   replaces_content ? KC_META_REPLACES_CONTENT : 0,
			   RelationGetRelid(rel), table_xid,
			   kc_catalog_has_reserved() ? KC_META_VERSION_RESERVE : KC_META_VERSION_BASE);
	smgr_bulk_write(bulk, 0, buf, true);
	smgr_bulk_finish(bulk);
	smgrclose(srel);
}

static KcMetaData *
kc_meta_check(Relation rel, Page page)
{
	KcMetaData *m;

	if (PageIsNew(page) || PageGetSpecialSize(page) != 0)
		kc_corrupt(rel, 0, "the metapage is missing or malformed");
	m = (KcMetaData *) PageGetContents(page);
	if (m->magic != KC_META_MAGIC)
		kc_corrupt(rel, 0, "the metapage has a bad magic number");
	{
		KcMetaData	copy = *m;

		copy.crc = 0;
		if ((uint32) kc_crc32c((const char *) &copy, sizeof(copy)) != m->crc)
			kc_corrupt(rel, 0, "the metapage checksum does not match");
	}
	if (m->version != KC_META_VERSION_BASE && m->version != KC_META_VERSION_RESERVE)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("columnar table \"%s\" has format version %u, this server reads versions %u and %u",
						RelationGetRelationName(rel), m->version,
						KC_META_VERSION_BASE, KC_META_VERSION_RESERVE)));
	if (m->codec > KC_CODEC_ZSTD ||
		m->stripe_rows < KC_MIN_STRIPE_ROWS || m->stripe_rows > KC_MAX_STRIPE_ROWS ||
		m->chunk_rows < KC_MIN_CHUNK_ROWS || m->chunk_rows > KC_MAX_CHUNK_ROWS)
		kc_corrupt(rel, 0, "the metapage holds options out of range");
	return m;
}

void
kc_meta_read(Relation rel, KcMetaData *out)
{
	Buffer		buf = ReadBuffer(rel, 0);
	Page		page;

	LockBuffer(buf, BUFFER_LOCK_SHARE);
	page = BufferGetPage(buf);
	*out = *kc_meta_check(rel, page);
	UnlockReleaseBuffer(buf);
}

/*
 * Write the changed metapage (the caller holds it exclusively and has not
 * yet entered a critical section).
 */
static void
kc_meta_log_and_release(Relation rel, Buffer buf)
{
	if (RelationNeedsWAL(rel))
	{
		XLogRecPtr	recptr;
		xl_kc_pages xlrec;

		xlrec.stripe_id = 0;
		xlrec.nblocks = 1;
		XLogBeginInsert();
		XLogRegisterData(&xlrec, sizeof(xlrec));
		XLogRegisterBuffer(0, buf, REGBUF_FORCE_IMAGE | REGBUF_STANDARD);
		recptr = XLogInsert(KC_RMGR_ID, XLOG_KC_META);
		PageSetLSN(BufferGetPage(buf), recptr);
	}
}

/*
 * Reserve the identity of a stripe of nrows rows: the next stripe id and the
 * next row numbers.  Row numbers need not be dense: a crash after this and
 * before the stripe is complete only skips numbers.
 */
void
kc_meta_allocate(Relation rel, uint32 nrows, uint64 *stripe_id,
				 uint64 *first_row)
{
	Buffer		buf = ReadBuffer(rel, 0);
	Page		page;
	KcMetaData *m;

	LockBuffer(buf, BUFFER_LOCK_EXCLUSIVE);
	page = BufferGetPage(buf);
	m = kc_meta_check(rel, page);

	*stripe_id = m->next_stripe_id;
	*first_row = m->next_row;
	if (m->next_row > KC_MAX_ROW_NUMBER - nrows || m->next_stripe_id >= PG_INT64_MAX)
		ereport(ERROR,
				(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
				 errmsg("columnar table \"%s\" has no row numbers left",
						RelationGetRelationName(rel))));

	START_CRIT_SECTION();
	m->next_row += nrows;
	m->next_stripe_id++;
	kc_meta_seal(m);
	MarkBufferDirty(buf);
	kc_meta_log_and_release(rel, buf);
	END_CRIT_SECTION();
	UnlockReleaseBuffer(buf);
}

/*
 * Give back the unused tail [first_row, first_row + nrows) of a reservation
 * made by kc_meta_allocate, when it is still the end of the range: nobody has
 * reserved after it, so the next number to hand out goes back to first_row.
 * Otherwise the numbers stay unused, a gap (the answer is false).  Under the
 * exclusive lock of the metapage, so the test and the change are one step.  The
 * caller has already made the claim on the numbers smaller (the catalog row of
 * the stripe says how many rows it has), so that no moment exists at which
 * numbers are free and still claimed.  Not for numbers a row was given: those
 * are never given back (a later index entry may name them).
 */
bool
kc_meta_return(Relation rel, uint64 first_row, uint32 nrows)
{
	Buffer		buf = ReadBuffer(rel, 0);
	Page		page;
	KcMetaData *m;

	LockBuffer(buf, BUFFER_LOCK_EXCLUSIVE);
	page = BufferGetPage(buf);
	m = kc_meta_check(rel, page);

	if (nrows == 0 || m->next_row != first_row + nrows)
	{
		UnlockReleaseBuffer(buf);
		return false;
	}

	START_CRIT_SECTION();
	m->next_row = first_row;
	kc_meta_seal(m);
	MarkBufferDirty(buf);
	if (RelationNeedsWAL(rel))
	{
		XLogRecPtr	recptr;
		xl_kc_return xlrec;

		xlrec.first_row = first_row;
		xlrec.nrows = nrows;
		XLogBeginInsert();
		XLogRegisterData(&xlrec, sizeof(xlrec));
		XLogRegisterBuffer(0, buf, REGBUF_FORCE_IMAGE | REGBUF_STANDARD);
		recptr = XLogInsert(KC_RMGR_ID, XLOG_KC_RETURN);
		PageSetLSN(BufferGetPage(buf), recptr);
	}
	END_CRIT_SECTION();
	UnlockReleaseBuffer(buf);
	return true;
}

void
kc_meta_set_options(Relation rel, const KcOptions *opts)
{
	Buffer		buf = ReadBuffer(rel, 0);
	Page		page;
	KcMetaData *m;

	LockBuffer(buf, BUFFER_LOCK_EXCLUSIVE);
	page = BufferGetPage(buf);
	m = kc_meta_check(rel, page);

	START_CRIT_SECTION();
	m->codec = opts->codec;
	m->level = opts->level;
	m->stripe_rows = opts->stripe_rows;
	m->chunk_rows = opts->chunk_rows;
	kc_meta_seal(m);
	MarkBufferDirty(buf);
	kc_meta_log_and_release(rel, buf);
	END_CRIT_SECTION();
	UnlockReleaseBuffer(buf);
}

/* Back to an empty table: counters at their start, options kept. */
void
kc_meta_reset(Relation rel)
{
	Buffer		buf = ReadBuffer(rel, 0);
	Page		page;
	KcMetaData *m;

	LockBuffer(buf, BUFFER_LOCK_EXCLUSIVE);
	page = BufferGetPage(buf);
	m = kc_meta_check(rel, page);

	START_CRIT_SECTION();
	m->next_row = 0;
	m->next_stripe_id = 1;
	kc_meta_seal(m);
	MarkBufferDirty(buf);
	kc_meta_log_and_release(rel, buf);
	END_CRIT_SECTION();
	UnlockReleaseBuffer(buf);
}

/*
 * Write the metapage after its transaction id changed.  The change is
 * logged as the page image; for a freeze the record also names the id, so
 * that a standby can cancel the queries that might still hold it.
 */
static void
kc_meta_log_xid_change(Relation rel, Buffer buf, TransactionId horizon)
{
	if (RelationNeedsWAL(rel))
	{
		XLogRecPtr	recptr;
		xl_kc_freeze xlrec;

		xlrec.horizon = horizon;
		XLogBeginInsert();
		XLogRegisterData(&xlrec, sizeof(xlrec));
		XLogRegisterBuffer(0, buf, REGBUF_FORCE_IMAGE | REGBUF_STANDARD);
		recptr = XLogInsert(KC_RMGR_ID, XLOG_KC_FREEZE);
		PageSetLSN(BufferGetPage(buf), recptr);
	}
}

/*
 * VACUUM: freeze the id of the transaction that created the file, once no
 * snapshot can have it in progress (it precedes oldest_xmin).  Without this
 * the id would, after a full lap of the transaction id space, name some
 * other running transaction.  Returns the oldest id the metapage still
 * holds, or InvalidTransactionId when it holds none (what the table's
 * relfrozenxid must not pass).
 */
TransactionId
kc_meta_freeze(Relation rel, TransactionId oldest_xmin)
{
	Buffer		buf = ReadBuffer(rel, 0);
	Page		page;
	KcMetaData *m;
	TransactionId cx;
	TransactionId tx;
	TransactionId horizon = InvalidTransactionId;
	TransactionId remaining = InvalidTransactionId;
	bool		freeze_c;
	bool		freeze_t;

	LockBuffer(buf, BUFFER_LOCK_EXCLUSIVE);
	page = BufferGetPage(buf);
	m = kc_meta_check(rel, page);
	cx = m->created_xid;
	tx = m->table_xid;
	freeze_c = TransactionIdIsNormal(cx) && TransactionIdPrecedes(cx, oldest_xmin);
	freeze_t = TransactionIdIsNormal(tx) && TransactionIdPrecedes(tx, oldest_xmin);

	/* what stays: the oldest normal id the page holds afterwards */
	if (TransactionIdIsNormal(cx) && !freeze_c)
		remaining = cx;
	if (TransactionIdIsNormal(tx) && !freeze_t &&
		(!TransactionIdIsValid(remaining) || TransactionIdPrecedes(tx, remaining)))
		remaining = tx;
	if (!freeze_c && !freeze_t)
	{
		UnlockReleaseBuffer(buf);
		return remaining;
	}

	/* the newest id frozen: a standby query that may hold any of them is cancelled */
	if (freeze_c)
		horizon = cx;
	if (freeze_t && (!TransactionIdIsValid(horizon) || TransactionIdFollows(tx, horizon)))
		horizon = tx;

	START_CRIT_SECTION();
	if (freeze_c)
		m->created_xid = FrozenTransactionId;
	if (freeze_t)
		m->table_xid = FrozenTransactionId;
	kc_meta_seal(m);
	MarkBufferDirty(buf);
	kc_meta_log_xid_change(rel, buf, horizon);
	END_CRIT_SECTION();
	UnlockReleaseBuffer(buf);

	/* every backend caches the id with the relation */
	CacheInvalidateRelcache(rel);
	return remaining;
}

/*
 * A rewrite that kept what every snapshot sees (VACUUM FULL, see
 * kc_relation_copy_for_cluster) has no creating transaction to report: an
 * older snapshot reads the new file like the old one.
 */
void
kc_meta_set_created_xid(Relation rel, TransactionId xid)
{
	Buffer		buf = ReadBuffer(rel, 0);
	Page		page;
	KcMetaData *m;

	LockBuffer(buf, BUFFER_LOCK_EXCLUSIVE);
	page = BufferGetPage(buf);
	m = kc_meta_check(rel, page);

	START_CRIT_SECTION();
	m->created_xid = xid;
	kc_meta_seal(m);
	MarkBufferDirty(buf);
	kc_meta_log_and_release(rel, buf);
	END_CRIT_SECTION();
	UnlockReleaseBuffer(buf);
	CacheInvalidateRelcache(rel);
}

/* ------------------------------------------------------------------------
 * Stripe pages
 * ------------------------------------------------------------------------
 */

/*
 * Append the stripe blob to the relation as a run of consecutive pages and
 * log them.  The relation extension lock is held for the whole run so no
 * other backend can extend the relation in between; the first block and the
 * count go to the catalog row, and the reader checks every page against them.
 */
void
kc_stripe_write(Relation rel, uint64 stripe_id, const char *blob,
				uint64 blob_len, uint64 *first_block, uint32 *block_count)
{
	uint64		npages = (blob_len + KC_PAGE_PAYLOAD - 1) / KC_PAGE_PAYLOAD;
	uint64		done = 0;
	BlockNumber expected = InvalidBlockNumber;
	BufferAccessStrategy strategy;

	if (npages == 0)
		npages = 1;
	if (npages >= MaxBlockNumber - 1 || npages > PG_UINT32_MAX)
		ereport(ERROR,
				(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
				 errmsg("columnar stripe of %llu pages is too large",
						(unsigned long long) npages)));

	strategy = GetAccessStrategy(BAS_BULKWRITE);
	LockRelationForExtension(rel, ExclusiveLock);

	while (done < npages)
	{
		Buffer		bufs[KC_MAX_BLOCKS_PER_RECORD];
		uint32		want = (uint32) Min(npages - done, KC_MAX_BLOCKS_PER_RECORD);
		uint32		got = 0;
		BlockNumber first;
		uint32		j;

		first = ExtendBufferedRelBy(BMR_REL(rel), MAIN_FORKNUM, strategy, 0,
									want, bufs, &got);
		if (got == 0)
			elog(ERROR, "could not extend relation \"%s\"",
				 RelationGetRelationName(rel));

		/* a gap between batches would break the stripe's layout */
		if (done > 0 && first != expected)
			elog(ERROR, "pages of a columnar stripe are not contiguous in \"%s\"",
				 RelationGetRelationName(rel));
		if (done == 0)
			*first_block = first;

		for (j = 0; j < got; j++)
		{
			Page		page;
			KcPageSpecial *sp;
			uint64		off = (done + j) * KC_PAGE_PAYLOAD;
			uint64		len = Min((uint64) KC_PAGE_PAYLOAD,
								  blob_len > off ? blob_len - off : 0);

			if (BufferGetBlockNumber(bufs[j]) != first + j)
				elog(ERROR, "extended pages of \"%s\" are not consecutive",
					 RelationGetRelationName(rel));
			LockBuffer(bufs[j], BUFFER_LOCK_EXCLUSIVE);
			page = BufferGetPage(bufs[j]);
			PageInit(page, BLCKSZ, sizeof(KcPageSpecial));
			sp = (KcPageSpecial *) PageGetSpecialPointer(page);
			sp->magic = KC_PAGE_MAGIC;
			sp->flags = 0;
			sp->page_no = (uint32) (done + j);
			sp->page_count = (uint32) npages;
			sp->reserved = 0;
			sp->stripe_id = stripe_id;
			if (len > 0)
				memcpy(PageGetContents(page), blob + off, len);

			/*
			 * The payload sits in what PageInit made the hole.  Close the hole
			 * so that any logger of standard pages (CREATE DATABASE by WAL,
			 * hint-bit images) keeps it instead of dropping it.
			 */
			((PageHeader) page)->pd_lower = ((PageHeader) page)->pd_upper;
		}

		if (RelationNeedsWAL(rel))
			XLogEnsureRecordSpace(got, 1);

		START_CRIT_SECTION();
		for (j = 0; j < got; j++)
			MarkBufferDirty(bufs[j]);
		if (RelationNeedsWAL(rel))
		{
			xl_kc_pages xlrec;
			XLogRecPtr	recptr;

			xlrec.stripe_id = stripe_id;
			xlrec.nblocks = got;
			XLogBeginInsert();
			XLogRegisterData(&xlrec, sizeof(xlrec));
			for (j = 0; j < got; j++)
				XLogRegisterBuffer(j, bufs[j], REGBUF_FORCE_IMAGE);
			recptr = XLogInsert(KC_RMGR_ID, XLOG_KC_PAGES);
			for (j = 0; j < got; j++)
				PageSetLSN(BufferGetPage(bufs[j]), recptr);
		}
		END_CRIT_SECTION();

		for (j = 0; j < got; j++)
			UnlockReleaseBuffer(bufs[j]);

		expected = first + got;
		done += got;
	}

	UnlockRelationForExtension(rel, ExclusiveLock);
	FreeAccessStrategy(strategy);
	*block_count = (uint32) npages;
}

/*
 * Read len bytes at offset off of the stripe blob into dst.  Every page that
 * is touched is checked against the stripe it should belong to.
 */
void
kc_stripe_read(Relation rel, const KcStripeInfo *si, uint64 off, uint64 len,
			   char *dst)
{
	if (off > si->blob_len || len > si->blob_len - off)
		kc_corrupt(rel, si->stripe_id, "read of %llu bytes at offset %llu is outside the stripe",
				   (unsigned long long) len, (unsigned long long) off);

	while (len > 0)
	{
		uint64		pageno = off / KC_PAGE_PAYLOAD;
		uint32		inpage = (uint32) (off % KC_PAGE_PAYLOAD);
		uint32		n = (uint32) Min((uint64) (KC_PAGE_PAYLOAD - inpage), len);
		Buffer		buf;
		Page		page;
		KcPageSpecial *sp;

		if (pageno >= si->block_count)
			kc_corrupt(rel, si->stripe_id, "page %llu is past the end of the stripe",
					   (unsigned long long) pageno);

		buf = ReadBuffer(rel, (BlockNumber) (si->first_block + pageno));
		LockBuffer(buf, BUFFER_LOCK_SHARE);
		page = BufferGetPage(buf);
		if (PageIsNew(page) || PageGetSpecialSize(page) != sizeof(KcPageSpecial))
		{
			UnlockReleaseBuffer(buf);
			kc_corrupt(rel, si->stripe_id, "block %llu is not a stripe page",
					   (unsigned long long) (si->first_block + pageno));
		}
		sp = (KcPageSpecial *) PageGetSpecialPointer(page);
		if (sp->magic != KC_PAGE_MAGIC || sp->stripe_id != si->stripe_id ||
			sp->page_no != pageno || sp->page_count != si->block_count)
		{
			UnlockReleaseBuffer(buf);
			kc_corrupt(rel, si->stripe_id, "block %llu belongs to another stripe or is damaged",
					   (unsigned long long) (si->first_block + pageno));
		}
		memcpy(dst, (char *) PageGetContents(page) + inpage, n);
		UnlockReleaseBuffer(buf);

		dst += n;
		off += n;
		len -= n;
	}
}

/* ------------------------------------------------------------------------
 * Identity of the stored content
 * ------------------------------------------------------------------------
 */

/*
 * The identity of the content of rel's file: a random id and the transaction
 * that created the file, both in the metapage.  The stripe catalog is keyed
 * by the id, so a copy of the file keeps its stripes, and a new file (TRUNCATE,
 * a rewrite) starts with none.  Cached in the relcache entry, and checked
 * against the file number so a swap of files is noticed even if the entry was
 * not rebuilt yet.
 */
const KcIdent *
kc_rel_ident(Relation rel)
{
	KcIdent    *id = (KcIdent *) rel->rd_amcache;

	if (id == NULL || id->filenode != rel->rd_locator.relNumber)
	{
		KcMetaData	m;

		kc_meta_read(rel, &m);
		if (rel->rd_amcache)
			pfree(rel->rd_amcache);
		id = MemoryContextAlloc(CacheMemoryContext, sizeof(KcIdent));
		id->uid = m.rel_uid;
		id->created_xid = m.created_xid;
		id->flags = m.flags;
		id->created_relid = m.created_relid;
		id->table_xid = m.table_xid;
		id->filenode = rel->rd_locator.relNumber;
		rel->rd_amcache = id;
	}
	return id;
}

/*
 * Is there a version of rel's pg_class row that the snapshot sees?  That is
 * what "the table existed for the snapshot" means, and no later change of the
 * row (a GRANT, a new owner, a storage option) can alter the answer, because
 * the version the snapshot sees stays in the catalog while the snapshot lives.
 */
static bool
kc_table_existed_for(Relation rel, Snapshot snap)
{
	Relation	pgclass = table_open(RelationRelationId, AccessShareLock);
	ScanKeyData key;
	SysScanDesc scan;
	bool		found;

	ScanKeyInit(&key, Anum_pg_class_oid, BTEqualStrategyNumber, F_OIDEQ,
				ObjectIdGetDatum(RelationGetRelid(rel)));
	scan = systable_beginscan(pgclass, ClassOidIndexId, true, snap, 1, &key);
	found = HeapTupleIsValid(systable_getnext(scan));
	systable_endscan(scan);
	table_close(pgclass, AccessShareLock);
	return found;
}

/*
 * A snapshot taken before the file was created by a rewrite or a TRUNCATE
 * cannot see the table's stripes (their catalog rows are newer than it).
 * Heap hands such a snapshot an empty table for TRUNCATE and the rewriting
 * forms of ALTER TABLE; here every such command fails the reader with a
 * serialization failure rather than return nothing.  A file that is the
 * first of its table (see below) replaced nothing, and the snapshot reads it
 * as empty, which is the right answer.  A transaction id older
 * than a billion transactions cannot be in any live snapshot, and its
 * comparison would wrap, so it is ignored.
 */
void
kc_check_snapshot_age(Relation rel, Snapshot snap)
{
	const KcIdent *id;
	TransactionId next;

	if (snap == NULL || snap->snapshot_type != SNAPSHOT_MVCC)
		return;
	id = kc_rel_ident(rel);
	if (!TransactionIdIsNormal(id->created_xid) ||
		TransactionIdIsCurrentTransactionId(id->created_xid))
		return;

	/*
	 * The first file of the relation (created for it, and not by TRUNCATE):
	 * no content existed before it, so a snapshot older than the file is
	 * meant to see an empty table, and the stripe catalog shows it exactly
	 * that, every stripe being newer than the snapshot.  That is what heap
	 * answers for a table created after the snapshot (a new partition, CREATE
	 * TABLE AS, a table dropped and created again under the same name).  A
	 * file that came from a rewrite was created for the temporary relation of
	 * the rewrite, not for this one, so it is not first.
	 */
	if (!(id->flags & KC_META_REPLACES_CONTENT) &&
		id->created_relid == RelationGetRelid(rel))
		return;
	next = ReadNextTransactionId();
	if ((uint32) (next - id->created_xid) > (uint32) (1U << 30))
		return;

	/*
	 * A snapshot that cannot see the creation of the table itself is older
	 * than the table: whatever files the table has had since (TRUNCATE, a
	 * rewrite, a change of access method), nothing existed for this snapshot
	 * to read before them, so there is nothing older that was replaced and
	 * the empty table is the right answer, as heap's.  The invariant: an
	 * error is for content that existed for the snapshot and is gone, and an
	 * empty table is never given in place of such content.
	 */
	if (TransactionIdIsNormal(id->table_xid) &&
		(uint32) (next - id->table_xid) <= (uint32) (1U << 30) &&
		XidInMVCCSnapshot(id->table_xid, snap))
		return;

	/*
	 * No creation is recorded: the table came from another access method (or
	 * from a file of an earlier build) and the extension could not tell when
	 * the table was made, nor prove that it was empty.  The creation of the
	 * table cannot be guessed from the transaction that last wrote its
	 * pg_class row, which any GRANT or ALTER TABLE moves forward.  The catalog
	 * itself knows what the snapshot saw: an old version of that row stays
	 * until nothing can see it, so if no version of it is visible to the
	 * snapshot, the table did not exist for it and the empty table is the
	 * right answer; if one is, content may have existed and the error stands.
	 */
	if (id->table_xid == InvalidTransactionId &&
		!kc_table_existed_for(rel, snap))
		return;
	if (XidInMVCCSnapshot(id->created_xid, snap))
		ereport(ERROR,
				(errcode(ERRCODE_T_R_SERIALIZATION_FAILURE),
				 errmsg("could not read columnar table \"%s\": it was rewritten or truncated after this snapshot was taken",
						RelationGetRelationName(rel)),
				 errdetail("VACUUM FULL, TRUNCATE, SET ACCESS METHOD or a rewriting ALTER TABLE replaced the table's storage."),
				 errhint("Retry the transaction.")));
}
