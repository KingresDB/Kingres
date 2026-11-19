/*-------------------------------------------------------------------------
 *
 * kingres_columnar.h
 *	  Declarations shared by the files of the columnar table access method:
 *	  the on-disk structures, the in-memory descriptions of a stripe and of
 *	  a column, and the entry points of each module.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kingres_columnar.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_COLUMNAR_H
#define KINGRES_COLUMNAR_H

#include "access/htup_details.h"
#include "access/tableam.h"
#include "access/xlogreader.h"
#include "catalog/pg_class.h"
#include "executor/tuptable.h"
#include "nodes/execnodes.h"
#include "fmgr.h"
#include "storage/bufmgr.h"
#include "storage/lockdefs.h"
#include "utils/relcache.h"
#include "utils/resowner.h"
#include "utils/snapshot.h"
#include "access/tupdesc.h"
#include "port/atomics.h"

#include "pg_compat.h"

/*
 * The resource manager id.  RM_EXPERIMENTAL_ID is for development; an id
 * reserved on the public registry replaces it before a release, and it is
 * part of the WAL format for ever (README, "Resource manager id").
 */
#define KC_RMGR_ID			RM_EXPERIMENTAL_ID
#define KC_EXTENSION_NAME	"kingres_columnar"
#define KC_CATALOG_NAME		"columnar_stripe"
#define KC_CATALOG_PKEY		"columnar_stripe_pkey"

/* Codec ids as stored in a chunk's footer entry.  Never renumber. */
#define KC_CODEC_NONE		0
#define KC_CODEC_PGLZ		1
#define KC_CODEC_LZ4		2
#define KC_CODEC_ZSTD		3
#define KC_CODEC_AUTO		4	/* GUC value only, never stored */

/* ------------------------------------------------------------------------
 * On-disk structures
 * ------------------------------------------------------------------------
 */

#define KC_META_MAGIC		0x4B434D31	/* "KCM1" */

/*
 * The format version of the metapage decides what a table is.  Version 2 is
 * what 0.1 wrote: row numbers are assigned when a stripe is written.  Version
 * 3 (0.2) is a table whose writers may reserve row numbers when a row is
 * inserted and whose stripe catalog may hold a reserved, not yet written, row
 * (increment C6a).  This server reads both and writes a new file as version 3
 * when the stripe catalog has the column that a reserved row needs
 * (kc_catalog_has_reserved), else as version 2, which is byte for byte what 0.1
 * writes.  A reservation is made only for a table of version 3.
 */
#define KC_META_VERSION_BASE	2
#define KC_META_VERSION_RESERVE	3
#define KC_META_VERSION		KC_META_VERSION_RESERVE
#define KC_PAGE_MAGIC		0x4B43		/* "KC" */
#define KC_STRIPE_MAGIC		0x4B435331	/* "KCS1" */
#define KC_STRIPE_VERSION	2

/*
 * Limits of a stripe, defined once.  The writer never writes a stripe of
 * more than KC_MAX_STRIPE_ROWS rows (kc_max_stripe_rows(), which a developer
 * setting can only lower), the option stripe_row_count cannot exceed it, and
 * columnar_check reports a stripe above it.  The reader accepts exactly what
 * the writer can write, KC_STRIPE_ROWS_READ_LIMIT = KC_MAX_STRIPE_ROWS, and
 * refuses a larger row count in a catalog row before it allocates anything
 * from it.  A larger limit would only serve stripes that no released build can
 * have written (0.1 is unreleased, and no build since the limit was introduced
 * writes more); it would let a damaged catalog row ask the reader for memory
 * the writer never needs.
 */
#define KC_MIN_STRIPE_ROWS	1000
#define KC_MAX_STRIPE_ROWS	10000000

/* the most files a commit keeps for the next transaction to clean up after (kc_write.c) */
#define KC_ORPHAN_NOTES_MAX	10000
#define KC_STRIPE_ROWS_READ_LIMIT	KC_MAX_STRIPE_ROWS
StaticAssertDecl(KC_MAX_STRIPE_ROWS <= KC_STRIPE_ROWS_READ_LIMIT,
				 "the reader must accept what the writer writes");
#define KC_MIN_CHUNK_ROWS	100
#define KC_MAX_CHUNK_ROWS	100000
#define KC_MAX_MINMAX_BYTES	64
#define KC_CHUNK_BYTE_LIMIT	(32 * 1024 * 1024)	/* raw bytes: closes a group */

/* Block 0 of the main fork.  Payload of a standard page. */
/*
 * table_xid is the transaction that created the table, as opposed to created_xid,
 * which is that of the file.  TRUNCATE and every rewrite give a table a new file
 * and copy table_xid from the old one, so it follows the content through any
 * chain of replacements.  A table that was not columnar before has it only when
 * the conversion knows the creation for certain (the table was made in the
 * converting transaction, or has no storage); otherwise it is 0, "not known",
 * and the reader asks the catalog what the snapshot saw (kc_check_snapshot_age).
 * A snapshot that cannot see it is older than the table: nothing existed for
 * that snapshot to read, whatever files the table has had since, and the answer
 * is an empty table, not an error.  It is an id in the file like created_xid,
 * and VACUUM freezes it the same way (0 stays 0).
 */
/*
 * The file replaced the content of a relation that existed (TRUNCATE).  A
 * file without this flag, created for the relation it belongs to, is the first
 * file of that relation: nothing older exists that a snapshot taken before the
 * file was created could be meant to see (kc_check_snapshot_age).
 */
#define KC_META_REPLACES_CONTENT	0x01

typedef struct KcMetaData
{
	uint32		magic;
	uint32		version;
	uint32		codec;
	int32		level;
	uint32		stripe_rows;
	uint32		chunk_rows;
	uint64		next_row;
	uint64		next_stripe_id;
	uint64		rel_uid;		/* identity of the stored content, copied with the file */
	uint32		created_xid;	/* transaction that created this file */
	uint32		crc;			/* CRC-32C of this struct with crc zero */
	uint32		flags;			/* KC_META_* */
	uint32		created_relid;	/* relation the file was created for */
	uint32		table_xid;		/* transaction that created the table (see below); 0: unknown */
	uint8		reserved[36];
} KcMetaData;

/* The special area of every page of a stripe: 24 bytes. */
typedef struct KcPageSpecial
{
	uint16		magic;
	uint16		flags;
	uint32		page_no;		/* number of this page within the stripe */
	uint32		page_count;		/* pages of the stripe */
	uint32		reserved;		/* zero; it held a transaction id, which VACUUM would have to freeze */
	uint64		stripe_id;
} KcPageSpecial;

#define KC_PAGE_PAYLOAD \
	(BLCKSZ - SizeOfPageHeaderData - sizeof(KcPageSpecial))

/* The header of a stripe blob (the byte string spread over the pages). */
typedef struct KcStripeHeader
{
	uint32		magic;
	uint16		version;
	uint16		ncols;
	uint32		nrows;
	uint32		ngroups;
	uint64		stripe_id;
	uint64		footer_off;
	uint32		footer_len;
	uint32		footer_crc;
	uint64		blob_len;
	uint8		reserved[16];
} KcStripeHeader;				/* 64 bytes */

/* Flags of a column chunk. */
#define KC_CHUNK_HAS_NULLS		0x01
#define KC_CHUNK_HAS_MINMAX		0x02

/* One column chunk in the footer: fixed part, then min and max bytes. */
typedef struct KcChunkDisk
{
	uint64		off;			/* offset inside the blob */
	uint32		comp_len;
	uint32		raw_len;
	uint32		nnulls;
	uint32		crc;			/* CRC-32C of the stored bytes */
	uint32		typid;			/* what the statistics were computed with */
	uint32		collation;
	uint16		min_len;
	uint16		max_len;
	uint8		codec;
	uint8		flags;
	uint8		pad[2];
} KcChunkDisk;					/* 40 bytes */

/* The same statistic, as the catalog's col_stats keeps it for a stripe. */
typedef struct KcStatDisk
{
	uint32		nnulls;
	uint32		typid;
	uint32		collation;
	uint16		min_len;
	uint16		max_len;
	uint8		flags;
	uint8		pad[3];
} KcStatDisk;					/* 20 bytes */

/* ------------------------------------------------------------------------
 * In-memory descriptions
 * ------------------------------------------------------------------------
 */

/* Everything the encoder and decoder need to know about one column. */
typedef struct KcColInfo
{
	Oid			typid;
	Oid			collation;
	int16		typlen;
	bool		byval;
	char		align;
	bool		dropped;
	bool		minmax_ok;		/* statistics are kept for this column */
	bool		is_array;
	Oid			elemtype;
	int16		elemlen;
	bool		elembyval;
	char		elemalign;
	Oid			opfamily;		/* default btree operator family of the type */
	FmgrInfo	cmp;			/* btree comparison, valid if minmax_ok */
} KcColInfo;

typedef struct KcTypeInfo
{
	int			natts;
	KcColInfo	cols[FLEXIBLE_ARRAY_MEMBER];
} KcTypeInfo;

/* A parsed statistic.  min and max point into the buffer they came from. */
typedef struct KcColStat
{
	bool		known;			/* false: nothing is known (column added later) */
	uint32		nnulls;
	bool		has_minmax;
	uint16		min_len;
	uint16		max_len;
	const char *min;
	const char *max;
} KcColStat;

/* One stripe as the catalog describes it. */
typedef struct KcStripeInfo
{
	uint64		stripe_id;
	uint64		first_row;
	uint32		row_count;
	uint32		chunk_groups;
	int			natts;
	uint64		first_block;
	uint32		block_count;
	uint64		blob_len;
	uint64		raw_bytes;
	uint64		footer_offset;
	bytea	   *cid_points;
	bytea	   *col_stats;
	TransactionId xmin;			/* of the catalog row */
	bool		own;			/* written by the running transaction */
	bool		maintenance;	/* a copy of rows readers see already (compaction) */
	uint32		visible_rows;	/* rows of the stripe the snapshot may see */
} KcStripeInfo;

/* The parsed footer of a stripe. */
typedef struct KcFooter
{
	int			ncols;
	uint32		ngroups;
	uint32	   *group_rows;		/* ngroups */
	KcChunkDisk *chunks;		/* ngroups * ncols */
	const char **mins;			/* ngroups * ncols, into 'raw' */
	const char **maxs;
	char	   *raw;
} KcFooter;

/* The options of a table (metapage), or the defaults for a new one. */
typedef struct KcOptions
{
	int			codec;
	int			level;
	uint32		stripe_rows;
	uint32		chunk_rows;
} KcOptions;

/* ------------------------------------------------------------------------
 * GUCs
 * ------------------------------------------------------------------------
 */
extern int	kc_guc_compression;
extern int	kc_guc_compression_level;
extern int	kc_guc_stripe_rows;
extern int	kc_guc_chunk_rows;
extern int	kc_guc_write_buffer_kb;
extern int	kc_guc_max_stripe_rows;
extern int	kc_guc_orphan_notes_max;
extern int	kc_guc_truncate_lock_timeout;
extern bool kc_guc_reserve_rows;
extern int	kc_guc_reserve_gate;
extern int	kc_guc_reserve_gate_at;

/* how long a vacuum asks for the exclusive lock of its truncation, as heap does (ms) */
#define KC_TRUNCATE_LOCK_TIMEOUT	5000
extern int	kc_max_stripe_rows(void);
extern bool kc_guc_enable_custom_scan;
extern bool kc_guc_enable_chunk_skipping;

/* ------------------------------------------------------------------------
 * kc_encode.c: column chunks and codecs
 * ------------------------------------------------------------------------
 */
extern bool kc_codec_available(int codec);
extern const char *kc_codec_name(int codec);
extern int	kc_codec_from_name(const char *name);
extern int	kc_default_codec(void);
extern KcTypeInfo *kc_typeinfo_build(TupleDesc tupdesc, MemoryContext cxt);
extern void kc_encode_chunk(const KcColInfo *ci, const Datum *vals,
							const bool *nulls, int stride, uint32 nrows,
							int codec, int level, StringInfo out,
							KcChunkDisk *meta, char **minbytes,
							char **maxbytes);
extern void kc_decode_chunk(const KcColInfo *ci, const char *comp,
							const KcChunkDisk *meta, uint32 nrows,
							Datum *vals, bool *nulls, MemoryContext cxt,
							const char *relname, uint64 stripe_id);
extern Datum kc_stat_datum(const KcColInfo *ci, const char *bytes, int len);
extern int	kc_stat_compare(const KcColInfo *ci, Datum a, Datum b);

/* ------------------------------------------------------------------------
 * kc_storage.c: pages, metapage, stripes, WAL
 * ------------------------------------------------------------------------
 */
extern void kc_rmgr_register(void);
extern void kc_require_rmgr(void);
extern bool kc_rmgr_registered;
extern void kc_meta_init_storage(Relation rel, const RelFileLocator *rlocator,
								 const KcOptions *opts, bool replaces_content,
								 TransactionId table_xid);

/* The identity of a relation's stored content, from the metapage (cached). */
typedef struct KcIdent
{
	uint64		uid;
	TransactionId created_xid;
	uint32		flags;
	Oid			created_relid;
	TransactionId table_xid;
	Oid			filenode;
} KcIdent;

extern const KcIdent *kc_rel_ident(Relation rel);
extern void kc_check_snapshot_age(Relation rel, Snapshot snap);
extern void kc_meta_read(Relation rel, KcMetaData *out);
extern void kc_meta_allocate(Relation rel, uint32 nrows, uint64 *stripe_id,
							 uint64 *first_row);
extern bool kc_meta_return(Relation rel, uint64 first_row, uint32 nrows);
extern void kc_meta_set_options(Relation rel, const KcOptions *opts);
extern void kc_meta_reset(Relation rel);
extern TransactionId kc_meta_freeze(Relation rel, TransactionId oldest_xmin);
extern void kc_meta_set_created_xid(Relation rel, TransactionId xid);
extern void kc_stripe_write(Relation rel, uint64 stripe_id, const char *blob,
							uint64 blob_len, uint64 *first_block,
							uint32 *block_count);
extern void kc_stripe_read(Relation rel, const KcStripeInfo *si, uint64 off,
						   uint64 len, char *dst);
extern pg_noreturn void kc_corrupt(Relation rel, uint64 stripe_id, const char *fmt,...) pg_attribute_printf(3, 4);
extern uint64 kc_crc32c(const char *data, uint64 len);

/* ------------------------------------------------------------------------
 * kc_catalog.c: the stripe catalog table
 * ------------------------------------------------------------------------
 */
extern Oid	kc_catalog_oid(void);
extern Oid	kc_catalog_index_oid(void);

/*
 * Is an access method this extension's: decided by its handler, never by its
 * name (C11-02).
 */
typedef enum KcAmClass
{
	KC_AM_COLUMNAR,				/* its handler is ours */
	KC_AM_OTHER,				/* another table access method */
	KC_AM_UNKNOWN				/* cannot be told: no such access method is visible */
} KcAmClass;

typedef struct KcAmMemo
{
	Oid			amoid;
	KcAmClass	cls;
	bool		guard;			/* call a handler that is not cached in a subtransaction (kc_am_class_guarded) */
} KcAmMemo;

extern KcAmClass kc_am_class(Oid amoid);
extern KcAmClass kc_am_class_memo(Oid amoid, KcAmMemo *memo);
extern bool kc_am_is_columnar(Oid amoid);
extern void kc_am_failed_reset(void);
extern void kc_am_cache_init(void);
extern bool kc_rel_is_columnar(Relation rel);

/* the test gates of the check (kingres_columnar.c) */
#define KC_GATE_TABLES		1
#define KC_GATE_ORPHANS		2
#define KC_GATE_CLAIMS		3
extern void kc_check_gate(int which);
extern void kc_gate_wait(int key);
extern Snapshot kc_catalog_snapshot(Snapshot snap, bool *is_mvcc);
extern Snapshot kc_latest_snapshot(void);

/* the relations the check functions lock without waiting (kc_catalog_lock_nowait) */
typedef struct KcCatLocks
{
	int			n;
	Oid			oid[12];
	LOCKMODE	mode[12];
	Oid			failed_oid;		/* the relation that could not be locked, and in what mode */
	LOCKMODE	failed_mode;
} KcCatLocks;
extern bool kc_catalog_lock_nowait(KcCatLocks *l, LOCKMODE mode);
extern void kc_catalog_unlock(KcCatLocks *l);
extern void kc_catalog_snapshot_report_conflicts(Snapshot snap, bool is_mvcc);
extern KcStripeInfo *kc_catalog_load(Relation rel, Snapshot snap, int *nstripes);
extern void kc_catalog_insert(Relation rel, const KcStripeInfo *si,
							  const char *cid_points, int cid_len,
							  const char *col_stats, int stats_len, bool frozen,
							  bool maintenance);
extern bool kc_catalog_has_reserved(void);
extern void kc_catalog_insert_reserved(Relation rel, const KcStripeInfo *si);
extern void kc_catalog_update_reserved(Relation rel, const KcStripeInfo *si,
									   const char *cid_points, int cid_len,
									   const char *col_stats, int stats_len);
extern void kc_catalog_delete_reserved(Relation rel, uint64 stripe_id);
extern int	kc_catalog_count_reserved(Relation rel, Snapshot snap);

/* who holds a row number, and which numbers are live (uniqueness and index vacuum, C6c) */
typedef enum KcRowState
{
	KC_ROW_NONE,				/* nobody: no row, or its writer aborted */
	KC_ROW_FOUND,				/* a committed row or this transaction's own */
	KC_ROW_WAIT					/* a row of a transaction in progress: wait for it */
} KcRowState;
typedef struct KcRowRange
{
	uint64		first;
	uint64		end;			/* exclusive */
} KcRowRange;
extern KcRowState kc_catalog_probe_row(Relation rel, uint64 row, TransactionId *xwait);
extern KcRowRange *kc_catalog_live_ranges(Relation rel, TransactionId oldest, uint64 next_row,
										  int *n_out, uint64 *low);
extern void kc_reserve_lock(Relation rel, LOCKMODE mode);
extern void kc_reserve_unlock(Relation rel, LOCKMODE mode);
extern bool kc_scan_dirty_fetch(Relation rel, ItemPointer tid, Snapshot snap, TupleTableSlot *slot);
extern void kc_index_vacuum(Relation rel, bool skip, double live_rows, BufferAccessStrategy bstrategy);
extern int	kc_catalog_delete_uid(uint64 uid);
extern int	kc_catalog_delete_stripe(uint64 uid, uint64 stripe_id, bool wait);
extern bool kc_catalog_uid_in_use(uint64 uid);
extern uint64 kc_catalog_file_uid(Form_pg_class cl);
extern bool kc_catalog_available(void);
extern bool kc_catalog_all_visible(Relation rel, TransactionId oldest_xmin);
extern uint64 kc_catalog_row_total(Relation rel, Snapshot snap, uint64 *stripes,
								   uint64 *groups, uint64 *raw_bytes);
extern int	kc_catalog_reconcile(const char **why, bool guard);
extern int	kc_catalog_count_orphans(const char **why);
extern void kc_stripe_stats_parse(const KcStripeInfo *si, int ncols,
								  const KcTypeInfo *ti, KcColStat *out);
extern void kc_footer_load(Relation rel, const KcStripeInfo *si, KcFooter *f);
extern void kc_footer_free(KcFooter *f);

/* ------------------------------------------------------------------------
 * kc_write.c: write buffers and transaction callbacks
 * ------------------------------------------------------------------------
 */
extern void kc_write_init(void);

/*
 * The number of a row that has none (a table that does not reserve): its slot
 * gets the meaningless TID kc_am.c has always given, and a scan of the buffer
 * shows the same.
 */
#define KC_NO_ROW	PG_UINT64_MAX

/*
 * The most row numbers a table has: a TID holds a block number below the one the
 * meaningless TID of an unnumbered row uses (MaxBlockNumber - 1) and an offset
 * of MaxOffsetNumber values, about 8.8 x 10^12 rows.  A row number is a TID
 * (kc_row_tid), so the allocation of numbers stops here, where it used to stop at
 * the largest 64-bit number, from which a TID would have wrapped.
 */
#define KC_MAX_ROW_NUMBER	((uint64) (MaxBlockNumber - 1) * MaxOffsetNumber)

/* the TID of a row number (design 5.3), the one every reader and the writer use */
static inline void
kc_row_tid(uint64 row, ItemPointer tid)
{
	ItemPointerSet(tid, (BlockNumber) (row / MaxOffsetNumber),
				   (OffsetNumber) (row % MaxOffsetNumber) + 1);
}

extern uint64 kc_write_row(Relation rel, TupleTableSlot *slot, CommandId cid);
extern void kc_write_rows(Relation rel, TupleTableSlot **slots, int nslots,
						  CommandId cid, uint64 *rownums);
extern bool kc_write_row_values(Relation rel, Datum *vals, bool *nulls,
								CommandId cid, uint64 *rownum);
extern bool kc_entry_refused(const char *what, const char *name, bool fails_txn);
extern bool kc_write_refused(Relation rel, const char *what);
extern bool kc_error_forbidden(void);
extern bool kc_subxact_refused(const char *what);
extern bool kc_subxact_possible(void);
extern int	kc_write_path_count(void);
extern const char *kc_write_path_get(int i, uint64 *hits);
extern void kc_write_paths_reset(void);
extern void kc_write_flush_relation(Relation rel);
extern void kc_write_prepare_for_workers(Relation rel);
extern bool kc_write_parallel_blocked(Relation rel);
extern void kc_write_discard_relation(Oid relid, bool *had_outer);
extern void kc_write_set_frozen(Relation rel, bool frozen);
extern void kc_write_set_maintenance(bool on);
extern void kc_write_note_replaced(Relation rel);
extern void kc_write_note_dropped(Oid relid);
extern int	kc_write_cleanup_replaced(void);
extern void kc_write_maintenance_flush(Relation rel);
extern void kc_write_maintenance_discard(void);

/* the rows [first, nrows) of one buffer: its visible part when the scan began */
typedef struct KcPendingRef
{
	uint32		serial;
	uint32		first;
	uint32		nrows;
} KcPendingRef;

extern uint64 kc_write_hold_acquire(Oid relid, uint64 uid, KcPendingRef **refs,
									int *n);
extern void kc_write_hold_release(uint64 hold);
extern void kc_write_hold_check(uint64 hold, Relation rel);
extern bool kc_write_pending_fetch(Oid relid, const KcPendingRef *ref,
								   uint32 rowno, Datum **vals, bool **nulls,
								   int *ncols, CommandId *cid, uint64 *rownum);

/* ------------------------------------------------------------------------
 * kc_scan.c: the serial scan, shared by the access method and custom scan
 * ------------------------------------------------------------------------
 */
struct KcSkip;
typedef struct KcSkip KcSkip;

typedef struct KcScanStats
{
	uint64		stripes_total;
	uint64		stripes_skipped;
	uint64		groups_total;
	uint64		groups_skipped;
	uint64		bytes_read;
} KcScanStats;

/*
 * The state a parallel columnar scan shares (design 8.4), in the dynamic
 * shared memory segment of the query: the index of the next stripe to take,
 * a fingerprint of the stripe list the leader read (every participant reads
 * the list itself with the same snapshot, and must have read the same one),
 * and one slot of counters per participant (slot 0 is the leader, slot 1 + n
 * is parallel worker n), which only its owner writes.
 */
typedef struct KcParStat
{
	uint64		stripes_total;
	uint64		stripes_skipped;
	uint64		groups_total;
	uint64		groups_skipped;
	uint64		bytes_read;
	uint64		rows;
} KcParStat;

typedef struct KcParallel
{
	pg_atomic_uint64 next;		/* next index into the stripe list to take */
	uint64		fingerprint;	/* of the leader's list of stripes */
	uint32		nstripes;		/* its length */
	int			nslots;
	KcParStat	stats[FLEXIBLE_ARRAY_MEMBER];
} KcParallel;

#define KcParallelSize(nslots) \
	(offsetof(KcParallel, stats) + sizeof(KcParStat) * (Size) (nslots))

typedef struct KcScan
{
	TableScanDescData base;		/* must be first */
	KcParallel *shared;			/* the stripes are taken from here (NULL: all of them) */
	Relation	rel;
	Snapshot	cat_snap;		/* snapshot for catalog reads (may be a copy) */
	CommandId	curcid;
	bool		all_own_rows;	/* ignore the command-id rule */
	TupleDesc	tupdesc;
	KcTypeInfo *ti;
	bool	   *needed;			/* per column; NULL means all */
	int			natts;
	struct KcSkip *skip;

	KcStripeInfo *stripes;
	int			nstripes;
	uint64		list_epoch;		/* kc_flush_epoch when the list was read */
	int			stripe_idx;		/* next stripe to open */

	/* the open stripe */
	bool		in_stripe;
	KcStripeInfo *cur;
	KcFooter	footer;
	KcColStat  *stripe_stats;
	int			group_idx;		/* next group to open */
	uint32		stripe_row_pos;	/* rows of the stripe passed so far */

	/* the open group */
	MemoryContext group_cxt;
	uint32		group_nrows;
	uint32		group_start;	/* row index in the stripe of the group's first row */
	uint32		group_pos;
	Datum	  **colvals;
	bool	  **colnulls;
	bool		have_group;

	/* the pending rows of the transaction */
	bool		pend_started;
	KcPendingRef *pend;
	int			npend;
	int			pend_idx;
	uint32		pend_row;
	uint64		hold;			/* hold on the write buffers (0: none) */
	ResourceOwner owner;	/* the resource owner that releases it on an error */

	/* position for backward steps */
	int64		ordinal;		/* rows returned, the next row's index */
	bool		hit_end;

	/* ANALYZE */
	bool		an_have;
	uint32		an_row;
	uint32		an_end;
	uint32		an_gstart;
	int			an_stripe;
	int			an_group;

	KcScanStats stats;
	MemoryContext scan_cxt;
} KcScan;

/* a scan flag of the extension itself, outside the SO_ bits of PostgreSQL */
#define KC_SCAN_MAINTENANCE	(1 << 30)

/*
 * The scan may begin in a parallel worker: set by the custom scan, whose
 * leader made the rows of its transaction visible to workers before they
 * started (kc_write_prepare_for_workers).  Any other scan in a worker is
 * refused.
 */
#define KC_SCAN_PARALLEL_OK	(1 << 29)

/* the scan is for index fetches by row number: it holds no write buffer (kc_scan_fetch_row) */
#define KC_SCAN_FETCH	(1 << 28)

extern KcScan *kc_scan_begin(Relation rel, Snapshot snapshot,
							 const bool *needed, struct KcSkip *skip,
							 int flags);
extern bool kc_scan_next(KcScan *scan, TupleTableSlot *slot);
extern void kc_scan_rescan(KcScan *scan);
extern void kc_scan_end(KcScan *scan);
extern void kc_scan_drop_hold(KcScan *scan);
extern uint64 kc_scan_stripe_for_block(KcScan *scan, BlockNumber blk,
									   int *idx);
extern uint64 kc_scan_fingerprint(const KcScan *scan);
extern void kc_scan_attach(KcScan *scan, KcParallel *shared);
extern bool kc_scan_analyze_block(KcScan *scan, Buffer buf);
extern bool kc_scan_analyze_next(KcScan *scan, TupleTableSlot *slot);
extern bool kc_scan_fetch_row(KcScan *scan, uint64 row, TupleTableSlot *slot);

/* flushes of stripes in this backend: a scan that fetches by number re-reads its stripe list when it changed */
extern uint64 kc_flush_epoch;
extern bool kc_write_pending_by_number(Oid relid, uint64 uid, uint64 row, Datum **vals,
									   bool **nulls, int *ncols, CommandId *cid);
extern void kc_write_flush_for_index(Relation rel);
extern bool kc_write_has_unwritten(Relation rel);
extern bool kc_rel_reserves(Relation rel);

/* ------------------------------------------------------------------------
 * kc_skip.c: min/max skipping
 * ------------------------------------------------------------------------
 */
extern struct KcSkip *kc_skip_build(List *quals, Index relid, TupleDesc desc,
									const KcTypeInfo *ti, PlanState *ps);
extern void kc_skip_reevaluate(struct KcSkip *skip, ExprContext *econtext);
extern bool kc_skip_unit(struct KcSkip *skip, const KcColStat *stats,
						 uint64 nrows);
extern char *kc_skip_describe(struct KcSkip *skip);
extern bool kc_skip_active(struct KcSkip *skip);

/* ------------------------------------------------------------------------
 * kc_cscan.c: the custom scan
 * ------------------------------------------------------------------------
 */
extern void kc_cscan_init(void);

/* ------------------------------------------------------------------------
 * kc_vacuum.c
 * ------------------------------------------------------------------------
 */
extern void kc_relation_vacuum(KC_VACUUM_PARAMS);
extern int	kc_compact(Relation rel, uint32 min_rows);

/* ------------------------------------------------------------------------
 * kingres_columnar.c
 * ------------------------------------------------------------------------
 */
extern void kc_options_for_new_table(KcOptions *opts);
extern const TableAmRoutine *kc_get_routine(void);
extern void kc_slot_ops_init(void);

#endif							/* KINGRES_COLUMNAR_H */
