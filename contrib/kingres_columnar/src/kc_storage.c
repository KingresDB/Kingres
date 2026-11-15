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
#include "access/transam.h"
#include "access/xact.h"
#include "access/xlog_internal.h"
#include "access/xloginsert.h"
#include "access/xlogutils.h"
#include "catalog/storage.h"
#include "miscadmin.h"
#include "port/pg_crc32c.h"
#include "storage/bufmgr.h"
#include "storage/bufpage.h"
#include "storage/bulk_write.h"
#include "storage/lmgr.h"
#include "storage/smgr.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"

#include "kingres_columnar.h"

#define XLOG_KC_PAGES		0x00	/* whole-page images of a stripe */
#define XLOG_KC_META		0x10	/* whole-page image of the metapage */

typedef struct xl_kc_pages
{
	uint64		stripe_id;
	uint32		nblocks;
} xl_kc_pages;

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

	if (info != XLOG_KC_PAGES && info != XLOG_KC_META)
		elog(PANIC, "kingres_columnar_redo: unknown op code %u", info);

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
			   uint64 next_stripe, uint64 uid, TransactionId xid)
{
	KcMetaData *m;

	PageInit(page, BLCKSZ, 0);
	m = (KcMetaData *) PageGetContents(page);
	memset(m, 0, sizeof(*m));
	m->magic = KC_META_MAGIC;
	m->version = KC_META_VERSION;
	m->codec = opts->codec;
	m->level = opts->level;
	m->stripe_rows = opts->stripe_rows;
	m->chunk_rows = opts->chunk_rows;
	m->next_row = next_row;
	m->next_stripe_id = next_stripe;
	m->rel_uid = uid;
	m->created_xid = xid;
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
					 const KcOptions *opts)
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
	if (!pg_strong_random(&uid, sizeof(uid)))
		elog(ERROR, "could not generate a random identity for a columnar file");
	uid &= PG_INT64_MAX;
	if (uid == 0)
		uid = 1;
	kc_meta_format((Page) buf, opts, 0, 1, uid, GetCurrentTransactionId());
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
	if (m->version != KC_META_VERSION)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("columnar table \"%s\" has format version %u, this server reads version %u",
						RelationGetRelationName(rel), m->version,
						KC_META_VERSION)));
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
	if (m->next_row > PG_INT64_MAX - nrows || m->next_stripe_id >= PG_INT64_MAX)
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
	TransactionId xid = GetCurrentTransactionId();

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
			sp->writer_xid = xid;
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
		id->filenode = rel->rd_locator.relNumber;
		rel->rd_amcache = id;
	}
	return id;
}

/*
 * A snapshot taken before the file was created by a rewrite or a TRUNCATE
 * cannot see the table's stripes (their catalog rows are newer than it).
 * Heap hands such a snapshot an empty table for TRUNCATE and the rewriting
 * forms of ALTER TABLE; here every such command fails the reader with a
 * serialization failure rather than return nothing.  A transaction id older
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
	next = ReadNextTransactionId();
	if ((uint32) (next - id->created_xid) > (uint32) (1U << 30))
		return;
	if (XidInMVCCSnapshot(id->created_xid, snap))
		ereport(ERROR,
				(errcode(ERRCODE_T_R_SERIALIZATION_FAILURE),
				 errmsg("could not read columnar table \"%s\": it was rewritten or truncated after this snapshot was taken",
						RelationGetRelationName(rel)),
				 errdetail("VACUUM FULL, TRUNCATE, SET ACCESS METHOD or a rewriting ALTER TABLE replaced the table's storage."),
				 errhint("Retry the transaction.")));
}
