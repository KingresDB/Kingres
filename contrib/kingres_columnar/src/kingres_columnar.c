/*-------------------------------------------------------------------------
 *
 * kingres_columnar.c
 *	  Module entry point of the columnar storage extension: settings, the
 *	  resource manager registration, and the SQL-callable functions.
 *
 * The extension must be in shared_preload_libraries: PostgreSQL accepts the
 * registration of a custom resource manager only while that list is being
 * loaded, and every node that replays columnar WAL (standbys, restores) has to
 * know the resource manager.  Without it the extension still loads, reads
 * existing tables, and refuses to write.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_columnar/src/kingres_columnar.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/genam.h"
#include "access/heapam.h"
#include "access/htup_details.h"
#include "access/relation.h"
#include "access/table.h"
#include "access/xact.h"
#include "catalog/objectaccess.h"
#include "catalog/pg_am.h"
#include "catalog/pg_class.h"
#include "catalog/pg_authid.h"
#include "funcapi.h"
#include "access/xlog.h"
#include "miscadmin.h"
#include "storage/lmgr.h"
#include "utils/acl.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"
#include "utils/tuplestore.h"

#include "kingres_columnar.h"

KC_MODULE_MAGIC();

PG_FUNCTION_INFO_V1(kingres_columnar_handler);
PG_FUNCTION_INFO_V1(kingres_columnar_set_options);
PG_FUNCTION_INFO_V1(kingres_columnar_options);
PG_FUNCTION_INFO_V1(kingres_columnar_info);
PG_FUNCTION_INFO_V1(kingres_columnar_stripes);
PG_FUNCTION_INFO_V1(kingres_columnar_compact);
PG_FUNCTION_INFO_V1(kingres_columnar_check);
PG_FUNCTION_INFO_V1(kingres_columnar_reconcile);
PG_FUNCTION_INFO_V1(kingres_columnar_write_paths);
PG_FUNCTION_INFO_V1(kingres_columnar_note_rewrite);
PG_FUNCTION_INFO_V1(kingres_columnar_finish_rewrites);
PG_FUNCTION_INFO_V1(kingres_columnar_identity);

bool		kc_rmgr_registered = false;

int			kc_guc_compression = KC_CODEC_AUTO;
int			kc_guc_compression_level = 3;
int			kc_guc_stripe_rows = 150000;
int			kc_guc_max_stripe_rows = KC_MAX_STRIPE_ROWS;
int			kc_guc_orphan_notes_max = KC_ORPHAN_NOTES_MAX;
int			kc_guc_chunk_rows = 10000;
int			kc_guc_write_buffer_kb = 131072;
bool		kc_guc_enable_custom_scan = true;
bool		kc_guc_enable_chunk_skipping = true;

static struct config_enum_entry kc_compression_options[8];

/* ------------------------------------------------------------------------
 * Module load
 * ------------------------------------------------------------------------
 */

static object_access_hook_type kc_prev_object_access_hook = NULL;

/*
 * A table access method has no callback when a file goes away, and a rewrite
 * (a type change, VACUUM FULL, a change of access method in either direction)
 * leaves the old file with the transient table that is dropped at the end of
 * it.  The drop of a relation is the one event that every such path goes
 * through, whether or not an event trigger fires, so the access method learns
 * of the files that go away here and the stripe rows of those files are
 * removed by the transaction that dropped them (kc_write_cleanup_replaced).
 * The event triggers of the extension only do it earlier, at the end of the
 * command; correctness does not depend on them.
 */
static void
kc_object_access(ObjectAccessType access, Oid classId, Oid objectId, int subId,
				 void *arg)
{
	if (kc_prev_object_access_hook)
		kc_prev_object_access_hook(access, classId, objectId, subId, arg);
	/*
	 * A drop runs in a live transaction, so the note is made; a module that
	 * calls the hook where an error is forbidden gets a warning and no note (the
	 * leftover rows of the file are removed by VACUUM and columnar_check(true)).
	 */
	if (access == OAT_DROP && classId == RelationRelationId && subId == 0 &&
		!kc_entry_refused("drop note", NULL, false))
		kc_write_note_dropped(objectId);
}

void
_PG_init(void)
{
	int			n = 0;

	/* the codecs this server was built with, plus "auto" */
	kc_compression_options[n++] = (struct config_enum_entry) {"auto", KC_CODEC_AUTO, false};
	kc_compression_options[n++] = (struct config_enum_entry) {"none", KC_CODEC_NONE, false};
	kc_compression_options[n++] = (struct config_enum_entry) {"pglz", KC_CODEC_PGLZ, false};
	if (kc_codec_available(KC_CODEC_LZ4))
		kc_compression_options[n++] = (struct config_enum_entry) {"lz4", KC_CODEC_LZ4, false};
	if (kc_codec_available(KC_CODEC_ZSTD))
		kc_compression_options[n++] = (struct config_enum_entry) {"zstd", KC_CODEC_ZSTD, false};
	kc_compression_options[n] = (struct config_enum_entry) {NULL, 0, false};

	DefineCustomEnumVariable("kingres_columnar.compression",
							 "Codec of a new columnar table.",
							 "Stored in the metapage of the table when it is created.",
							 &kc_guc_compression, KC_CODEC_AUTO,
							 kc_compression_options, PGC_USERSET, 0,
							 NULL, NULL, NULL);
	DefineCustomIntVariable("kingres_columnar.compression_level",
							"Compression level of a new columnar table (zstd 1 to 19).",
							NULL, &kc_guc_compression_level, 3, 1, 19,
							PGC_USERSET, 0, NULL, NULL, NULL);
	DefineCustomIntVariable("kingres_columnar.stripe_row_count",
							"Rows of a full stripe of a new columnar table.",
							NULL, &kc_guc_stripe_rows, 150000,
							KC_MIN_STRIPE_ROWS, KC_MAX_STRIPE_ROWS,
							PGC_USERSET, 0, NULL, NULL, NULL);
	/*
	 * A developer setting, for tests: it only lowers the most rows the writer
	 * puts in a stripe (and the size above which columnar_check reports one),
	 * so the limit can be reached with a few thousand rows.  It cannot raise
	 * anything above the compiled limit and does not touch the reader, so no
	 * value of it can make a table unreadable.
	 */
	DefineCustomIntVariable("kingres_columnar.max_stripe_rows",
							"Most rows the writer puts in one stripe (testing: lowers the compiled limit).",
							NULL, &kc_guc_max_stripe_rows, KC_MAX_STRIPE_ROWS,
							KC_MIN_STRIPE_ROWS, KC_MAX_STRIPE_ROWS,
							PGC_SUSET, GUC_NOT_IN_SAMPLE, NULL, NULL, NULL);
	/*
	 * A developer setting, for tests: it lowers the number of files that the
	 * commit of a transaction keeps for the next transaction to clean up (the
	 * ones another module dropped after the cleanup had run), so that the limit
	 * can be tested with a handful of tables.  It cannot raise the compiled limit.
	 */
	DefineCustomIntVariable("kingres_columnar.orphan_notes_max",
							"Most files kept for the next transaction to clean up after (testing: lowers the compiled limit).",
							NULL, &kc_guc_orphan_notes_max, KC_ORPHAN_NOTES_MAX,
							1, KC_ORPHAN_NOTES_MAX,
							PGC_SUSET, GUC_NOT_IN_SAMPLE, NULL, NULL, NULL);
	DefineCustomIntVariable("kingres_columnar.chunk_row_count",
							"Rows of a chunk group of a new columnar table.",
							NULL, &kc_guc_chunk_rows, 10000,
							KC_MIN_CHUNK_ROWS, KC_MAX_CHUNK_ROWS,
							PGC_USERSET, 0, NULL, NULL, NULL);
	DefineCustomIntVariable("kingres_columnar.write_buffer_memory",
							"Memory of one backend for stripes being built, all tables together.",
							"A soft limit: buffers of an outer subtransaction cannot be flushed early; twice this is a hard ceiling that refuses the write.",
							&kc_guc_write_buffer_kb, 131072, 1024, MAX_KILOBYTES,
							PGC_USERSET, GUC_UNIT_KB, NULL, NULL, NULL);
	DefineCustomBoolVariable("kingres_columnar.enable_custom_scan",
							 "Use the Columnar Scan (projection and skipping).",
							 NULL, &kc_guc_enable_custom_scan, true,
							 PGC_USERSET, 0, NULL, NULL, NULL);
	DefineCustomBoolVariable("kingres_columnar.enable_chunk_skipping",
							 "Skip stripes and chunk groups with their minimum and maximum.",
							 NULL, &kc_guc_enable_chunk_skipping, true,
							 PGC_USERSET, 0, NULL, NULL, NULL);
	MarkGUCPrefixReserved("kingres_columnar");

	if (process_shared_preload_libraries_in_progress)
	{
		kc_rmgr_register();
		kc_rmgr_registered = true;
	}
	kc_slot_ops_init();
	kc_write_init();
	kc_cscan_init();
	kc_prev_object_access_hook = object_access_hook;
	object_access_hook = kc_object_access;
}

void
kc_options_for_new_table(KcOptions *opts)
{
	opts->codec = (kc_guc_compression == KC_CODEC_AUTO) ?
		kc_default_codec() : kc_guc_compression;
	opts->level = kc_guc_compression_level;
	opts->stripe_rows = kc_guc_stripe_rows;
	opts->chunk_rows = kc_guc_chunk_rows;
}

/* The most rows the writer puts in a stripe: the one definition (kingres_columnar.h). */
int
kc_max_stripe_rows(void)
{
	return kc_guc_max_stripe_rows;
}

Datum
kingres_columnar_handler(PG_FUNCTION_ARGS)
{
	PG_RETURN_POINTER(kc_get_routine());
}

/* ------------------------------------------------------------------------
 * Helpers
 * ------------------------------------------------------------------------
 */

static Relation
kc_open_columnar(Oid relid, LOCKMODE lockmode, bool need_owner)
{
	Relation	rel = relation_open(relid, lockmode);

	if (!kc_rel_is_columnar(rel))
		ereport(ERROR,
				(errcode(ERRCODE_WRONG_OBJECT_TYPE),
				 errmsg("\"%s\" is not a columnar table",
						RelationGetRelationName(rel))));
	if (!need_owner &&
		pg_class_aclcheck(relid, GetUserId(), ACL_SELECT) != ACLCHECK_OK)
		aclcheck_error(ACLCHECK_NO_PRIV, get_relkind_objtype(rel->rd_rel->relkind),
					   RelationGetRelationName(rel));
	if (need_owner && !object_ownercheck(RelationRelationId, relid, GetUserId()))
		aclcheck_error(ACLCHECK_NOT_OWNER, get_relkind_objtype(rel->rd_rel->relkind),
					   RelationGetRelationName(rel));
	return rel;
}

/* ------------------------------------------------------------------------
 * Options
 * ------------------------------------------------------------------------
 */

Datum
kingres_columnar_set_options(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	Relation	rel;
	KcMetaData	m;
	KcOptions	opts;

	/* writes the metapage: refused where the server forbids an error */
	if (kc_entry_refused("columnar_set_options", NULL, true))
		PG_RETURN_VOID();
	if (PG_ARGISNULL(0))
		ereport(ERROR,
				(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
				 errmsg("the table must not be null")));

	rel = kc_open_columnar(relid, ShareUpdateExclusiveLock, true);
	kc_meta_read(rel, &m);
	opts.codec = m.codec;
	opts.level = m.level;
	opts.stripe_rows = m.stripe_rows;
	opts.chunk_rows = m.chunk_rows;

	if (!PG_ARGISNULL(1))
	{
		char	   *name = text_to_cstring(PG_GETARG_TEXT_PP(1));
		int			c = kc_codec_from_name(name);

		if (c < 0)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("unknown compression \"%s\"", name),
					 errhint("Use none, pglz, lz4, zstd or auto.")));
		if (c == KC_CODEC_AUTO)
			c = kc_default_codec();
		if (!kc_codec_available(c))
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("compression \"%s\" is not available in this server", name)));
		opts.codec = c;
	}
	if (!PG_ARGISNULL(2))
	{
		int			l = PG_GETARG_INT32(2);

		if (l < 1 || l > 19)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("compression_level must be between 1 and 19")));
		opts.level = l;
	}
	if (!PG_ARGISNULL(3))
	{
		int			n = PG_GETARG_INT32(3);

		if (n < KC_MIN_STRIPE_ROWS || n > KC_MAX_STRIPE_ROWS)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("stripe_row_count must be between %d and %d",
							KC_MIN_STRIPE_ROWS, KC_MAX_STRIPE_ROWS)));
		opts.stripe_rows = n;
	}
	if (!PG_ARGISNULL(4))
	{
		int			n = PG_GETARG_INT32(4);

		if (n < KC_MIN_CHUNK_ROWS || n > KC_MAX_CHUNK_ROWS)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("chunk_row_count must be between %d and %d",
							KC_MIN_CHUNK_ROWS, KC_MAX_CHUNK_ROWS)));
		opts.chunk_rows = n;
	}
	if (opts.chunk_rows > opts.stripe_rows)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("chunk_row_count must not exceed stripe_row_count")));
	kc_require_rmgr();
	kc_meta_set_options(rel, &opts);
	relation_close(rel, NoLock);
	PG_RETURN_VOID();
}

Datum
kingres_columnar_options(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	Relation	rel = kc_open_columnar(relid, AccessShareLock, false);
	KcMetaData	m;
	TupleDesc	tupdesc;
	Datum		values[4];
	bool		nulls[4] = {false, false, false, false};

	if (get_call_result_type(fcinfo, NULL, &tupdesc) != TYPEFUNC_COMPOSITE)
		elog(ERROR, "return type must be a row type");
	kc_meta_read(rel, &m);
	values[0] = CStringGetTextDatum(kc_codec_name(m.codec));
	values[1] = Int32GetDatum(m.level);
	values[2] = Int32GetDatum(m.stripe_rows);
	values[3] = Int32GetDatum(m.chunk_rows);
	relation_close(rel, AccessShareLock);
	PG_RETURN_DATUM(HeapTupleGetDatum(heap_form_tuple(tupdesc, values, nulls)));
}

/* ------------------------------------------------------------------------
 * Information
 * ------------------------------------------------------------------------
 */

Datum
kingres_columnar_info(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	Relation	rel = kc_open_columnar(relid, AccessShareLock, false);
	TupleDesc	tupdesc;
	Datum		values[6];
	bool		nulls[6] = {false, false, false, false, false, false};
	bool		mvcc;
	Snapshot	cs = kc_catalog_snapshot(GetActiveSnapshot(), &mvcc);
	int			n;
	int			i;
	KcStripeInfo *s;
	int64		stripes,
				groups = 0,
				rows = 0,
				disk = 0,
				raw = 0;

	if (get_call_result_type(fcinfo, NULL, &tupdesc) != TYPEFUNC_COMPOSITE)
		elog(ERROR, "return type must be a row type");
	s = kc_catalog_load(rel, cs, &n);
	stripes = n;
	for (i = 0; i < n; i++)
	{
		groups += s[i].chunk_groups;
		rows += s[i].row_count;
		disk += (int64) s[i].block_count * BLCKSZ;
		raw += s[i].raw_bytes;
	}
	values[0] = Int64GetDatum(stripes);
	values[1] = Int64GetDatum(groups);
	values[2] = Int64GetDatum(rows);
	values[3] = Int64GetDatum(disk);
	values[4] = Int64GetDatum(raw);
	if (disk > 0)
		values[5] = Float8GetDatum((double) raw / (double) disk);
	else
		nulls[5] = true;
	relation_close(rel, AccessShareLock);
	PG_RETURN_DATUM(HeapTupleGetDatum(heap_form_tuple(tupdesc, values, nulls)));
}

Datum
kingres_columnar_stripes(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	ReturnSetInfo *rsinfo = (ReturnSetInfo *) fcinfo->resultinfo;
	Relation	rel = kc_open_columnar(relid, AccessShareLock, false);
	bool		mvcc;
	Snapshot	cs = kc_catalog_snapshot(GetActiveSnapshot(), &mvcc);
	int			n;
	int			i;
	KcStripeInfo *s;

	InitMaterializedSRF(fcinfo, 0);
	s = kc_catalog_load(rel, cs, &n);
	for (i = 0; i < n; i++)
	{
		Datum		values[7];
		bool		nulls[7] = {false, false, false, false, false, false, false};

		values[0] = Int64GetDatum((int64) s[i].stripe_id);
		values[1] = Int64GetDatum((int64) s[i].first_row);
		values[2] = Int32GetDatum((int32) s[i].row_count);
		values[3] = Int32GetDatum((int32) s[i].chunk_groups);
		values[4] = Int64GetDatum((int64) s[i].block_count * BLCKSZ);
		values[5] = Int64GetDatum((int64) s[i].raw_bytes);
		values[6] = TransactionIdGetDatum(s[i].xmin);
		tuplestore_putvalues(rsinfo->setResult, rsinfo->setDesc, values, nulls);
	}
	relation_close(rel, AccessShareLock);
	return (Datum) 0;
}

/* ------------------------------------------------------------------------
 * Maintenance
 * ------------------------------------------------------------------------
 */

Datum
kingres_columnar_compact(PG_FUNCTION_ARGS)
{
	Oid			relid;
	Relation	rel;
	uint32		minrows;
	int			n;

	/* writes stripes and catalog rows: refused where the server forbids an error */
	if (kc_entry_refused("columnar_compact", NULL, true))
		PG_RETURN_INT32(0);
	if (PG_ARGISNULL(0))
		ereport(ERROR,
				(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
				 errmsg("the table must not be null")));
	relid = PG_GETARG_OID(0);
	rel = kc_open_columnar(relid, ShareUpdateExclusiveLock, true);

	kc_require_rmgr();
	if (PG_ARGISNULL(1))
	{
		KcMetaData	m;

		kc_meta_read(rel, &m);
		minrows = m.stripe_rows / 4;
	}
	else
	{
		int32		v = PG_GETARG_INT32(1);

		if (v < 1)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("min_rows must be positive")));
		minrows = v;
	}
	n = kc_compact(rel, minrows);
	relation_close(rel, NoLock);
	PG_RETURN_INT32(n);
}

Datum
kingres_columnar_reconcile(PG_FUNCTION_ARGS)
{
	/* deletes catalog rows; skipping it loses nothing, the next call does it */
	if (kc_entry_refused("columnar_reconcile", NULL, false))
		PG_RETURN_INT32(0);
	PG_RETURN_INT32(kc_catalog_reconcile());
}

/*
 * columnar_write_paths(reset): the counters of the write side of this backend
 * (kc_write.c, KcPath), one row each.
 */
Datum
kingres_columnar_write_paths(PG_FUNCTION_ARGS)
{
	bool		reset = PG_GETARG_BOOL(0);
	ReturnSetInfo *rsinfo = (ReturnSetInfo *) fcinfo->resultinfo;
	int			i;

	InitMaterializedSRF(fcinfo, 0);
	for (i = 0; i < kc_write_path_count(); i++)
	{
		uint64		hits;
		Datum		values[2];
		bool		nulls[2] = {false, false};

		values[0] = CStringGetTextDatum(kc_write_path_get(i, &hits));
		values[1] = Int64GetDatum((int64) hits);
		tuplestore_putvalues(rsinfo->setResult, rsinfo->setDesc, values, nulls);
	}
	if (reset)
		kc_write_paths_reset();
	return (Datum) 0;
}

/*
 * columnar_note_rewrite(rel): kept for databases whose install script is the
 * one of an earlier build, which has a table_rewrite event trigger that calls
 * it.  The access method no longer needs the trigger (it notes a file when the
 * relation that holds it is dropped: kc_write_note_dropped); noting a file
 * twice is the same as once.  Whether the file was replaced is decided later,
 * when the command is over (columnar_finish_rewrites), by looking at the file
 * the table has then.
 */
Datum
kingres_columnar_note_rewrite(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	Relation	rel;

	/* a note in memory only, but it reads the catalog: refused where an error is forbidden */
	if (kc_entry_refused("columnar_note_rewrite", NULL, false))
		PG_RETURN_VOID();
	/*
	 * Called from the table_rewrite event trigger of the earlier script, where
	 * the rewrite holds the relation exclusively, and by hand: a lock of our own
	 * (a relation is never opened without one) makes it safe for both, and a
	 * note of a file the table still has deletes nothing.
	 */
	rel = try_relation_open(relid, AccessShareLock);

	if (rel != NULL)
	{
		kc_write_note_replaced(rel);
		relation_close(rel, AccessShareLock);
	}
	PG_RETURN_VOID();
}

/*
 * columnar_finish_rewrites(): remove the stripe rows of the files this
 * transaction replaced, whose table has another file now.  Run at the end of
 * ALTER TABLE; the cost is that of the files the command replaced, none for a
 * command that replaced no columnar file.  Returns the number of rows deleted.
 */
Datum
kingres_columnar_finish_rewrites(PG_FUNCTION_ARGS)
{
	bool		pushed = false;
	int			n;

	/* deletes catalog rows; the commit does the same work, so skipping it loses nothing */
	if (kc_entry_refused("columnar_finish_rewrites", NULL, false))
		PG_RETURN_INT32(0);
	if (!ActiveSnapshotSet())
	{
		PushActiveSnapshot(GetTransactionSnapshot());
		pushed = true;
	}
	n = kc_write_cleanup_replaced();
	if (pushed)
		PopActiveSnapshot();
	PG_RETURN_INT32(n);
}

static void
kc_check_one(Oid relid, bool fix, ReturnSetInfo *rsinfo)
{
		LOCKMODE	lockmode = fix ? ShareUpdateExclusiveLock : AccessShareLock;
		Relation	rel;
		bool		mvcc;
		Snapshot	cs;
		int			n;
		int			i;
		KcStripeInfo *s;
		BlockNumber nblocks;

		/*
		 * Never wait for a lock: a prepared transaction that holds a table
		 * exclusively (a prepared TRUNCATE, a rewrite) keeps it until someone
		 * ends it, and the check would wait with it, without end.  A table that
		 * cannot be locked now is reported as not checked and the rest goes on.
		 */
		if (!ConditionalLockRelationOid(relid, lockmode))
		{
			Datum		values[3];
			bool		nulls[3] = {false, false, false};

			values[0] = CStringGetTextDatum("not_checked");
			values[1] = ObjectIdGetDatum(relid);
			values[2] = CStringGetTextDatum("the table is locked by another transaction, possibly a prepared one, and was not checked; end that transaction and run the check again");
			tuplestore_putvalues(rsinfo->setResult, rsinfo->setDesc, values, nulls);
			return;
		}
		rel = try_relation_open(relid, NoLock);
		if (rel == NULL || !kc_rel_is_columnar(rel))
		{
			if (rel)
				relation_close(rel, lockmode);
			else
				UnlockRelationOid(relid, lockmode);
			return;
		}
		cs = kc_catalog_snapshot(GetActiveSnapshot(), &mvcc);
		s = kc_catalog_load(rel, cs, &n);
		nblocks = RelationGetNumberOfBlocks(rel);
		for (i = 0; i < n; i++)
		{
			const char *problem = NULL;
			char		detail[256];
			Datum		values[3];
			bool		nulls[3] = {false, false, false};

			if (s[i].first_block + s[i].block_count > nblocks)
			{
				problem = "stripe_beyond_file";
				snprintf(detail, sizeof(detail), "stripe " UINT64_FORMAT " lies at blocks %llu to %llu, the file has %u blocks",
						 s[i].stripe_id, (unsigned long long) s[i].first_block,
						 (unsigned long long) (s[i].first_block + s[i].block_count - 1), nblocks);
			}
			else
			{
				Buffer		buf = ReadBuffer(rel, (BlockNumber) s[i].first_block);
				Page		page;
				KcPageSpecial *sp;

				LockBuffer(buf, BUFFER_LOCK_SHARE);
				page = BufferGetPage(buf);
				if (PageIsNew(page) || PageGetSpecialSize(page) != sizeof(KcPageSpecial))
					sp = NULL;
				else
					sp = (KcPageSpecial *) PageGetSpecialPointer(page);
				if (sp == NULL || sp->magic != KC_PAGE_MAGIC ||
					sp->stripe_id != s[i].stripe_id || sp->page_no != 0 ||
					sp->page_count != s[i].block_count)
				{
					problem = "page_header_mismatch";
					snprintf(detail, sizeof(detail), "the first page of stripe " UINT64_FORMAT " (block %llu) does not belong to it",
							 s[i].stripe_id, (unsigned long long) s[i].first_block);
				}
				UnlockReleaseBuffer(buf);
			}
			/*
			 * A stripe over the limit the writer keeps to now.  The reader's limit is
			 * the highest the writer can have (KC_MAX_STRIPE_ROWS) and a stripe over
			 * that is refused as a corrupt catalog row before it gets here, so what is
			 * reported is a stripe that is readable but larger than the developer
			 * setting max_stripe_rows allows today.  A rewrite (VACUUM FULL, SET
			 * ACCESS METHOD) cuts it into stripes within the limit.
			 */
			if (problem == NULL && s[i].row_count > (uint32) kc_max_stripe_rows())
			{
				problem = "stripe_over_row_limit";
				snprintf(detail, sizeof(detail), "stripe " UINT64_FORMAT " has %u rows, more than the %d the setting max_stripe_rows allows; it can be read, and VACUUM FULL or SET ACCESS METHOD writes it again within the limit",
						 s[i].stripe_id, s[i].row_count, kc_max_stripe_rows());
			}
			if (problem == NULL)
				continue;
			values[0] = CStringGetTextDatum(problem);
			values[1] = ObjectIdGetDatum(RelationGetRelid(rel));
			values[2] = CStringGetTextDatum(detail);
			tuplestore_putvalues(rsinfo->setResult, rsinfo->setDesc, values, nulls);
			if (fix && strcmp(problem, "stripe_beyond_file") == 0)
				(void) kc_catalog_delete_stripe(kc_rel_ident(rel)->uid, s[i].stripe_id);
		}
		relation_close(rel, NoLock);
}

/*
 * One relation at a time, each in a subtransaction: a damaged relation is
 * reported as a row, not as an error that hides every other relation.
 */
static void
kc_check_guarded(Oid relid, bool fix, ReturnSetInfo *rsinfo)
{
	MemoryContext oldcxt = CurrentMemoryContext;
	ResourceOwner oldowner = CurrentResourceOwner;

	BeginInternalSubTransaction(NULL);
	MemoryContextSwitchTo(oldcxt);
	PG_TRY();
	{
		kc_check_one(relid, fix, rsinfo);
		ReleaseCurrentSubTransaction();
		MemoryContextSwitchTo(oldcxt);
		CurrentResourceOwner = oldowner;
	}
	PG_CATCH();
	{
		ErrorData  *ed;
		Datum		values[3];
		bool		nulls[3] = {false, false, false};

		MemoryContextSwitchTo(oldcxt);
		ed = CopyErrorData();
		FlushErrorState();
		RollbackAndReleaseCurrentSubTransaction();
		MemoryContextSwitchTo(oldcxt);
		CurrentResourceOwner = oldowner;
		values[0] = CStringGetTextDatum("unreadable");
		values[1] = ObjectIdGetDatum(relid);
		values[2] = CStringGetTextDatum(ed->message);
		tuplestore_putvalues(rsinfo->setResult, rsinfo->setDesc, values, nulls);
	}
	PG_END_TRY();
}

/*
 * columnar_check(fix): the stripe catalog against the files.  Reports rows of
 * files no relation uses (fix deletes them), stripes that run past the end of
 * their file (fix deletes the catalog row: the data is unreadable), and
 * stripes whose first page does not belong to them (reported only).
 */
Datum
kingres_columnar_check(PG_FUNCTION_ARGS)
{
	bool		fix = PG_GETARG_BOOL(0);
	ReturnSetInfo *rsinfo = (ReturnSetInfo *) fcinfo->resultinfo;
	Relation	pgclass;
	TableScanDesc scan;
	HeapTuple	tup;
	List	   *relids = NIL;
	ListCell   *lc;
	Oid			amoid = kc_am_oid();

	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("must be superuser to run columnar_check")));
	if (fix && RecoveryInProgress())
		ereport(ERROR,
				(errcode(ERRCODE_READ_ONLY_SQL_TRANSACTION),
				 errmsg("cannot repair columnar tables during recovery")));
	InitMaterializedSRF(fcinfo, 0);

	/* the repair deletes catalog rows: refused where the server forbids an error */
	if (fix && kc_entry_refused("columnar_check(true)", NULL, false))
		return (Datum) 0;

	pgclass = table_open(RelationRelationId, AccessShareLock);
	scan = table_beginscan_catalog(pgclass, 0, NULL);
	while ((tup = heap_getnext(scan, ForwardScanDirection)) != NULL)
	{
		Form_pg_class cl = (Form_pg_class) GETSTRUCT(tup);

		if (OidIsValid(amoid) && cl->relam == amoid &&
			(cl->relkind == RELKIND_RELATION || cl->relkind == RELKIND_MATVIEW))
			relids = lappend_oid(relids, cl->oid);
	}
	table_endscan(scan);
	table_close(pgclass, AccessShareLock);

	foreach(lc, relids)
	{
		kc_check_guarded(lfirst_oid(lc), fix, rsinfo);
	}

	{
		int			d;
		Datum		values[3];
		bool		nulls[3] = {false, true, false};

		if (fix)
			d = kc_catalog_reconcile();
		else
			d = kc_catalog_count_orphans();
		if (d > 0)
		{
			char		detail[100];

			snprintf(detail, sizeof(detail), "%d stripe catalog rows belong to files no relation uses%s",
					 d, fix ? " (deleted)" : "");
			values[0] = CStringGetTextDatum("orphan_catalog_rows");
			values[1] = (Datum) 0;
			values[2] = CStringGetTextDatum(detail);
			tuplestore_putvalues(rsinfo->setResult, rsinfo->setDesc, values, nulls);
		}
	}
	return (Datum) 0;
}

/*
 * columnar_identity(rel): the identity of the stored content of a columnar
 * table, the key of its rows in the stripe catalog.  For tests and for
 * administrators who look at the catalog; superuser only by privilege.
 */
Datum
kingres_columnar_identity(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	Relation	rel = kc_open_columnar(relid, AccessShareLock, false);
	uint64		uid = kc_rel_ident(rel)->uid;

	relation_close(rel, AccessShareLock);
	PG_RETURN_INT64((int64) uid);
}

/*
 * columnar_creating_xid(rel): the transaction id kept in the metapage, that
 * of the transaction that created the file (2, the frozen id, once VACUUM
 * has frozen it or when a rewrite that kept what every snapshot sees made
 * the file).  The only transaction id a columnar relation file holds.
 */
PG_FUNCTION_INFO_V1(kingres_columnar_creating_xid);

Datum
kingres_columnar_creating_xid(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	Relation	rel = kc_open_columnar(relid, AccessShareLock, false);
	KcMetaData	m;

	kc_meta_read(rel, &m);
	relation_close(rel, AccessShareLock);
	PG_RETURN_TRANSACTIONID(m.created_xid);
}

/*
 * columnar_table_xid(rel): the transaction id of the metapage that stands for
 * the table's creation (see KcMetaData): 0 when it is not known (a table that
 * came from another access method), 2 (frozen) once VACUUM has frozen it.  For
 * tests and for administrators.
 */
PG_FUNCTION_INFO_V1(kingres_columnar_table_xid);

Datum
kingres_columnar_table_xid(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	Relation	rel = kc_open_columnar(relid, AccessShareLock, false);
	KcMetaData	m;

	kc_meta_read(rel, &m);
	relation_close(rel, AccessShareLock);
	PG_RETURN_TRANSACTIONID(m.table_xid);
}
