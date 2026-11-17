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
#include "utils/relcache.h"
#include "utils/resowner.h"
#include "utils/snapshot.h"
#include "access/tupdesc.h"

#include "pg_compat.h"

/*
 * The resource manager id.  RM_EXPERIMENTAL_ID is for development; an id
 * reserved on the public registry replaces it before a release, and it is
 * part of the WAL format for ever (README, "Resource manager id").
 */
#define KC_RMGR_ID			RM_EXPERIMENTAL_ID
#define KC_AM_NAME			"columnar"
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
#define KC_META_VERSION		2
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
extern Oid	kc_am_oid(void);
extern bool kc_rel_is_columnar(Relation rel);
extern Snapshot kc_catalog_snapshot(Snapshot snap, bool *is_mvcc);
extern void kc_catalog_snapshot_report_conflicts(Snapshot snap, bool is_mvcc);
extern KcStripeInfo *kc_catalog_load(Relation rel, Snapshot snap, int *nstripes);
extern void kc_catalog_insert(Relation rel, const KcStripeInfo *si,
							  const char *cid_points, int cid_len,
							  const char *col_stats, int stats_len, bool frozen,
							  bool maintenance);
extern int	kc_catalog_delete_uid(uint64 uid);
extern int	kc_catalog_delete_stripe(uint64 uid, uint64 stripe_id);
extern bool kc_catalog_uid_in_use(uint64 uid);
extern uint64 kc_catalog_file_uid(Form_pg_class cl);
extern bool kc_catalog_available(void);
extern bool kc_catalog_all_visible(Relation rel, TransactionId oldest_xmin);
extern uint64 kc_catalog_row_total(Relation rel, Snapshot snap, uint64 *stripes,
								   uint64 *groups, uint64 *raw_bytes);
extern int	kc_catalog_reconcile(void);
extern int	kc_catalog_count_orphans(void);
extern void kc_stripe_stats_parse(const KcStripeInfo *si, int ncols,
								  const KcTypeInfo *ti, KcColStat *out);
extern void kc_footer_load(Relation rel, const KcStripeInfo *si, KcFooter *f);
extern void kc_footer_free(KcFooter *f);

/* ------------------------------------------------------------------------
 * kc_write.c: write buffers and transaction callbacks
 * ------------------------------------------------------------------------
 */
extern void kc_write_init(void);
extern void kc_write_row(Relation rel, TupleTableSlot *slot, CommandId cid);
extern void kc_write_rows(Relation rel, TupleTableSlot **slots, int nslots,
						  CommandId cid);
extern bool kc_write_row_values(Relation rel, Datum *vals, bool *nulls,
								CommandId cid);
extern bool kc_entry_refused(const char *what, const char *name, bool fails_txn);
extern bool kc_write_refused(Relation rel, const char *what);
extern int	kc_write_path_count(void);
extern const char *kc_write_path_get(int i, uint64 *hits);
extern void kc_write_paths_reset(void);
extern void kc_write_flush_relation(Relation rel);
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
								   int *ncols, CommandId *cid);

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

typedef struct KcScan
{
	TableScanDescData base;		/* must be first */
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

extern KcScan *kc_scan_begin(Relation rel, Snapshot snapshot,
							 const bool *needed, struct KcSkip *skip,
							 int flags);
extern bool kc_scan_next(KcScan *scan, TupleTableSlot *slot);
extern void kc_scan_rescan(KcScan *scan);
extern void kc_scan_end(KcScan *scan);
extern void kc_scan_drop_hold(KcScan *scan);
extern uint64 kc_scan_stripe_for_block(KcScan *scan, BlockNumber blk,
									   int *idx);
extern bool kc_scan_analyze_block(KcScan *scan, Buffer buf);
extern bool kc_scan_analyze_next(KcScan *scan, TupleTableSlot *slot);

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
