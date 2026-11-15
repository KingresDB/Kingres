/*-------------------------------------------------------------------------
 *
 * kingres_columnar.h
 *	  Declarations shared by the files of the columnar table access method:
 *	  the on-disk structures, the in-memory descriptions of a stripe and of
 *	  a column, and the entry points of each module.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: PostgreSQL
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
#include "executor/tuptable.h"
#include "nodes/execnodes.h"
#include "fmgr.h"
#include "storage/bufmgr.h"
#include "utils/relcache.h"
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
#define KC_META_VERSION		1
#define KC_PAGE_MAGIC		0x4B43		/* "KC" */
#define KC_STRIPE_MAGIC		0x4B435331	/* "KCS1" */
#define KC_STRIPE_VERSION	1

/* Limits that the readers enforce and the writers respect. */
#define KC_MIN_STRIPE_ROWS	1000
#define KC_MAX_STRIPE_ROWS	10000000
#define KC_MIN_CHUNK_ROWS	100
#define KC_MAX_CHUNK_ROWS	100000
#define KC_MAX_MINMAX_BYTES	64
#define KC_CHUNK_BYTE_LIMIT	(32 * 1024 * 1024)	/* raw bytes: closes a group */

/* Block 0 of the main fork.  Payload of a standard page. */
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
	uint8		reserved[64];
} KcMetaData;

/* The special area of every page of a stripe: 24 bytes. */
typedef struct KcPageSpecial
{
	uint16		magic;
	uint16		flags;
	uint32		page_no;		/* number of this page within the stripe */
	uint32		page_count;		/* pages of the stripe */
	uint32		writer_xid;		/* for the free-extent walk of a later version */
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
	uint16		min_len;
	uint16		max_len;
	uint8		codec;
	uint8		flags;
	uint8		pad[6];
} KcChunkDisk;					/* 32 bytes */

/* The same statistic, as the catalog's col_stats keeps it for a stripe. */
typedef struct KcStatDisk
{
	uint32		nnulls;
	uint16		min_len;
	uint16		max_len;
	uint8		flags;
	uint8		pad[3];
} KcStatDisk;					/* 12 bytes */

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
								 const KcOptions *opts);
extern void kc_meta_read(Relation rel, KcMetaData *out);
extern void kc_meta_allocate(Relation rel, uint32 nrows, uint64 *stripe_id,
							 uint64 *first_row);
extern void kc_meta_set_options(Relation rel, const KcOptions *opts);
extern void kc_meta_reset(Relation rel);
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
extern KcStripeInfo *kc_catalog_load(Relation rel, Snapshot snap, int *nstripes);
extern void kc_catalog_insert(Relation rel, const KcStripeInfo *si,
							  const char *cid_points, int cid_len,
							  const char *col_stats, int stats_len);
extern void kc_catalog_delete_filenode(Oid filenode);
extern int	kc_catalog_delete_stripe(Oid filenode, uint64 stripe_id);
extern void kc_catalog_rekey(Oid old_filenode, Oid new_filenode);
extern uint64 kc_catalog_row_total(Relation rel, Snapshot snap, uint64 *stripes,
								   uint64 *groups, uint64 *raw_bytes);
extern int	kc_catalog_reconcile(void);
extern int	kc_catalog_count_orphans(void);
extern void kc_stripe_stats_parse(const KcStripeInfo *si, int ncols,
								  KcColStat *out);
extern void kc_footer_load(Relation rel, const KcStripeInfo *si, KcFooter *f);
extern void kc_footer_free(KcFooter *f);

/* ------------------------------------------------------------------------
 * kc_write.c: write buffers and transaction callbacks
 * ------------------------------------------------------------------------
 */
extern void kc_write_init(void);
extern void kc_write_row(Relation rel, TupleTableSlot *slot, CommandId cid);
extern void kc_write_row_values(Relation rel, Datum *vals, bool *nulls,
								CommandId cid);
extern void kc_write_flush_relation(Relation rel);
extern void kc_write_discard_relation(Oid relid, bool *had_outer);
extern bool kc_write_has_outer_rows(Oid relid);
extern void kc_write_pin_relation(Oid relid, int delta);
extern int	kc_write_pending_count(Oid relid);
extern bool kc_write_pending_get(Oid relid, int ordinal, uint32 *serial);

typedef struct KcPendingRef
{
	uint32		serial;
	uint32		nrows;			/* rows present when the scan started */
} KcPendingRef;

extern KcPendingRef *kc_write_pending_refs(Oid relid, int *n);
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
	bool		pinned;

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

extern KcScan *kc_scan_begin(Relation rel, Snapshot snapshot,
							 const bool *needed, struct KcSkip *skip,
							 int flags);
extern bool kc_scan_next(KcScan *scan, TupleTableSlot *slot);
extern void kc_scan_rescan(KcScan *scan);
extern void kc_scan_end(KcScan *scan);
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
