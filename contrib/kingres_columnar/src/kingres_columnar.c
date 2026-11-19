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

#include "catalog/index.h"
#include "catalog/namespace.h"
#include "commands/defrem.h"
#include "nodes/parsenodes.h"
#include "tcop/utility.h"
#include "utils/lsyscache.h"

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
#include "storage/lock.h"
#include "storage/latch.h"
#include "storage/proc.h"
#include "storage/procarray.h"
#include "utils/wait_event.h"
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
PG_FUNCTION_INFO_V1(kingres_columnar_check_clean);
PG_FUNCTION_INFO_V1(kingres_columnar_reconcile);
PG_FUNCTION_INFO_V1(kingres_columnar_write_paths);
PG_FUNCTION_INFO_V1(kingres_columnar_note_rewrite);
PG_FUNCTION_INFO_V1(kingres_columnar_finish_rewrites);
PG_FUNCTION_INFO_V1(kingres_columnar_identity);
PG_FUNCTION_INFO_V1(kingres_columnar_is_columnar_am);

bool		kc_rmgr_registered = false;

int			kc_guc_compression = KC_CODEC_AUTO;
int			kc_guc_compression_level = 3;
int			kc_guc_stripe_rows = 150000;
int			kc_guc_max_stripe_rows = KC_MAX_STRIPE_ROWS;
int			kc_guc_orphan_notes_max = KC_ORPHAN_NOTES_MAX;
int			kc_guc_truncate_lock_timeout = KC_TRUNCATE_LOCK_TIMEOUT;
static int	kc_guc_check_gate = 0;
static int	kc_guc_check_gate_orphans = 0;
static int	kc_guc_check_gate_claims = 0;
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

/*
 * Utility statements that touch an index of a columnar table (C6b): the concurrent
 * forms are refused before they begin (their first transaction would commit an
 * invalid index that the build then fails to make).  The write of the rows this
 * transaction holds, before a build reads stripes, is the build's own
 * (kc_index_build_range_scan).
 */
static ProcessUtility_hook_type kc_prev_process_utility = NULL;

static Oid
kc_columnar_table_of(Oid relid)
{
	char		kind;

	if (!OidIsValid(relid))
		return InvalidOid;
	kind = get_rel_relkind(relid);
	if (kind == RELKIND_INDEX)
		relid = IndexGetRelation(relid, true);
	if (!OidIsValid(relid))
		return InvalidOid;
	kind = get_rel_relkind(relid);
	if ((kind == RELKIND_RELATION || kind == RELKIND_MATVIEW) &&
		kc_am_is_columnar(get_rel_relam(relid)))
		return relid;
	return InvalidOid;
}

static void
kc_process_utility(PlannedStmt *pstmt, const char *queryString, bool readOnlyTree,
				   ProcessUtilityContext context, ParamListInfo params,
				   QueryEnvironment *queryEnv, DestReceiver *dest,
				   QueryCompletion *qc)
{
	Node	   *parsetree = pstmt->utilityStmt;
	Oid			tbl = InvalidOid;
	bool		concurrent = false;

	if (IsA(parsetree, IndexStmt))
	{
		IndexStmt  *stmt = (IndexStmt *) parsetree;

		if (stmt->relation != NULL)
			tbl = kc_columnar_table_of(RangeVarGetRelid(stmt->relation, NoLock, true));
		concurrent = stmt->concurrent;
	}
	else if (IsA(parsetree, ReindexStmt))
	{
		ReindexStmt *stmt = (ReindexStmt *) parsetree;
		ListCell   *lc;

		foreach(lc, stmt->params)
		{
			DefElem    *opt = (DefElem *) lfirst(lc);

			if (strcmp(opt->defname, "concurrently") == 0)
				concurrent = defGetBoolean(opt);
		}
		if ((stmt->kind == REINDEX_OBJECT_INDEX || stmt->kind == REINDEX_OBJECT_TABLE) &&
			stmt->relation != NULL)
			tbl = kc_columnar_table_of(RangeVarGetRelid(stmt->relation, NoLock, true));
	}
	if (OidIsValid(tbl) && concurrent)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("CREATE INDEX CONCURRENTLY and REINDEX CONCURRENTLY are not supported on columnar tables"),
				 errhint("Use CREATE INDEX or REINDEX, which lock the table against writes while they run.")));
	if (kc_prev_process_utility)
		kc_prev_process_utility(pstmt, queryString, readOnlyTree, context, params,
								queryEnv, dest, qc);
	else
		standard_ProcessUtility(pstmt, queryString, readOnlyTree, context, params,
								queryEnv, dest, qc);
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
	/*
	 * Developer settings, for tests: a key (the first of the two integers of
	 * pg_advisory_lock(int, int), the second being 0) that columnar_check and
	 * columnar_check_clean wait on, at the start of the look at each table (after
	 * the list of tables is made), until no other session holds that advisory
	 * lock, so that a test can drop or make a table, or cancel the call, at that
	 * moment and not at a moment it can only hope for; and a second key for the
	 * moment of the orphan step between its first snapshot and its read of pg_class.  0 (the default) is no
	 * gate.  They exist in every build, because the tests that use them must run
	 * on production builds; so that they cannot be used to hold up a check, the
	 * wait is only for an advisory lock that a session of a superuser holds (an
	 * ordinary role can take any advisory lock), and it is bounded (two
	 * minutes) and can be cancelled (C11-07).
	 */
	DefineCustomIntVariable("kingres_columnar.check_gate",
							"Advisory lock key that the check functions wait on after listing the tables (testing).",
							NULL, &kc_guc_check_gate, 0, 0, 1000000,
							PGC_SUSET, GUC_NOT_IN_SAMPLE, NULL, NULL, NULL);
	DefineCustomIntVariable("kingres_columnar.check_gate_orphans",
							"Advisory lock key that the orphan step waits on after its first snapshot, before it reads pg_class (testing).",
							NULL, &kc_guc_check_gate_orphans, 0, 0, 1000000,
							PGC_SUSET, GUC_NOT_IN_SAMPLE, NULL, NULL, NULL);
	DefineCustomIntVariable("kingres_columnar.check_gate_claims",
							"Advisory lock key that the orphan step waits on after its first read of pg_class, before its second snapshot (testing).",
							NULL, &kc_guc_check_gate_claims, 0, 0, 1000000,
							PGC_SUSET, GUC_NOT_IN_SAMPLE, NULL, NULL, NULL);
	/*
	 * A developer setting, for tests: how long the truncation of a VACUUM asks
	 * for its exclusive lock before it gives up for this visit.  The default
	 * is heap's 5 seconds; a test raises it so that a verdict does not depend
	 * on how fast the machine is (a reader that goes away "while the vacuum
	 * waits"), or lowers it to see the give-up without a long wait.
	 */
	DefineCustomIntVariable("kingres_columnar.vacuum_truncate_lock_timeout",
							"How long VACUUM waits for the lock to truncate a table before giving up (testing; heap uses 5 seconds).",
							NULL, &kc_guc_truncate_lock_timeout, KC_TRUNCATE_LOCK_TIMEOUT,
							50, 600000, PGC_SUSET, GUC_NOT_IN_SAMPLE | GUC_UNIT_MS,
							NULL, NULL, NULL);
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
	kc_am_cache_init();
	kc_cscan_init();
	kc_prev_object_access_hook = object_access_hook;
	object_access_hook = kc_object_access;
	kc_prev_process_utility = ProcessUtility_hook;
	ProcessUtility_hook = kc_process_utility;
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

/*
 * The table of a function that only reads, opened with AccessShareLock.  Where
 * the server forbids an error (the end of a transaction, a critical section: a
 * module of another extension calls the function there) a lock that has to be
 * waited for is not taken: the wait can end in a deadlock error, which is a
 * PANIC there, and the transaction cannot be cancelled.  The lock is asked for
 * without waiting; a table that is locked exclusively gets a WARNING and the
 * answer "nothing" (NULL, or no rows), which the caller sees (C9-03).  Where an
 * error is allowed this is kc_open_columnar.
 */
static Relation
kc_open_columnar_read(Oid relid, const char *what)
{
	if (kc_error_forbidden())
	{
		Relation	rel;

		if (!ConditionalLockRelationOid(relid, AccessShareLock))
		{
			ereport(WARNING,
					(errcode(ERRCODE_LOCK_NOT_AVAILABLE),
					 errmsg("%s refused: the table is locked exclusively by another transaction and the server forbids an error here, where a wait could end in one", what),
					 errdetail("Nothing was read; the answer is empty (null or no rows).")));
			return NULL;
		}
		/* the lock is ours now: opening takes it again without waiting, and the extra one goes at once */
		rel = kc_open_columnar(relid, AccessShareLock, false);
		UnlockRelationOid(relid, AccessShareLock);
		return rel;
	}
	return kc_open_columnar(relid, AccessShareLock, false);
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
	Relation	rel = kc_open_columnar_read(relid, "columnar_options");
	KcMetaData	m;
	TupleDesc	tupdesc;
	Datum		values[4];
	bool		nulls[4] = {false, false, false, false};

	if (rel == NULL)
		PG_RETURN_NULL();
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
	Relation	rel = kc_open_columnar_read(relid, "columnar_info");
	TupleDesc	tupdesc;
	Datum		values[7];
	bool		nulls[7] = {false, false, false, false, false, false, false};
	bool		mvcc;
	Snapshot	cs;
	int			n;
	int			i;
	KcStripeInfo *s;
	int64		stripes,
				groups = 0,
				rows = 0,
				disk = 0,
				raw = 0;

	if (rel == NULL)
		PG_RETURN_NULL();
	cs = kc_catalog_snapshot(GetActiveSnapshot(), &mvcc);
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

	/*
	 * unreferenced_bytes (C11-06): what the file holds that no stripe visible
	 * now uses: the pages of loads that were rolled back, of stripes that
	 * compaction replaced, and of a load that is still in progress (an
	 * estimate at any moment).  VACUUM gives back such pages at the end of the
	 * file when they are at least 1000 pages or a sixteenth of it (heap's rule),
	 * and nothing reuses them in the middle of the file, so a file whose number
	 * stays high after a VACUUM is waiting for VACUUM FULL.  Read with a
	 * snapshot of this moment, whatever the caller's snapshot is.
	 */
	{
		Snapshot	ls = kc_latest_snapshot();
		int			ln;
		KcStripeInfo *lsi = kc_catalog_load(rel, ls, &ln);
		int64		used = BLCKSZ;	/* the metapage */
		int64		filebytes = (int64) RelationGetNumberOfBlocks(rel) * BLCKSZ;
		int			k;

		for (k = 0; k < ln; k++)
			used += (int64) lsi[k].block_count * BLCKSZ;
		values[6] = Int64GetDatum(filebytes > used ? filebytes - used : 0);
		pfree(ls);
	}
	relation_close(rel, AccessShareLock);
	PG_RETURN_DATUM(HeapTupleGetDatum(heap_form_tuple(tupdesc, values, nulls)));
}

Datum
kingres_columnar_stripes(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	ReturnSetInfo *rsinfo = (ReturnSetInfo *) fcinfo->resultinfo;
	Relation	rel;
	bool		mvcc;
	Snapshot	cs;
	int			n;
	int			i;
	KcStripeInfo *s;

	InitMaterializedSRF(fcinfo, 0);
	rel = kc_open_columnar_read(relid, "columnar_stripes");
	if (rel == NULL)
		return (Datum) 0;
	cs = kc_catalog_snapshot(GetActiveSnapshot(), &mvcc);
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

/*
 * columnar_reconcile(): the number of stripe catalog rows deleted, or NULL (with
 * a WARNING that says why) when the clean-up stood down and looked at nothing
 * (C12-01): a relation whose access method cannot be classified, or a columnar
 * relation with an unreadable metapage.  Zero is "looked, found nothing".
 */
Datum
kingres_columnar_reconcile(PG_FUNCTION_ARGS)
{
	const char *why = NULL;
	int			n;

	/* deletes catalog rows; skipping it loses nothing, the next call does it */
	if (kc_entry_refused("columnar_reconcile", NULL, false))
		PG_RETURN_INT32(0);
	/* at the start of a subtransaction (not visible from here) none may be begun: only at the top level */
	kc_am_failed_reset();
	n = kc_catalog_reconcile(&why, GetCurrentTransactionNestLevel() == 1);
	if (n < 0)
	{
		ereport(WARNING,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("columnar_reconcile did nothing: %s", why ? why : "it could not run"),
				 errdetail("Nothing was deleted and no orphan row was looked for; the answer is NULL, which is not \"nothing to delete\"."),
				 errhint("End or wait for the transaction that made the access method, or repair the relation, and call it again.")));
		PG_RETURN_NULL();
	}
	PG_RETURN_INT32(n);
}

/*
 * columnar_is_columnar_am(amoid): whether the access method is this
 * extension's, by what its handler returns (kc_am_class), the one definition
 * that every C path uses.  The DDL guard of the extension asks it, so that an
 * access method over any SQL function with the extension's C entry point is
 * guarded like the one over kingres.columnar_handler (C12-03).
 */
Datum
kingres_columnar_is_columnar_am(PG_FUNCTION_ARGS)
{
	PG_RETURN_BOOL(kc_am_class(PG_GETARG_OID(0)) == KC_AM_COLUMNAR);
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

/*
 * Where the rows of a check go: into the result set of columnar_check, or only
 * counted (columnar_check_clean, which answers true when there is none).
 */
typedef struct KcCheckSink
{
	ReturnSetInfo *rsinfo;		/* NULL: count only */
	int64		rows;
	LockData   *ld;				/* the lock data the not_checked rows read, see kc_lock_blockers */
	int			ld_uses;
} KcCheckSink;

static void
kc_check_row(KcCheckSink *sink, const char *name, Oid relid, const char *detail)
{
	Datum		values[3];
	bool		nulls[3] = {false, false, false};

	sink->rows++;
	if (sink->rsinfo == NULL)
		return;
	values[0] = CStringGetTextDatum(name);
	if (OidIsValid(relid))
		values[1] = ObjectIdGetDatum(relid);
	else
	{
		values[1] = (Datum) 0;
		nulls[1] = true;
	}
	values[2] = CStringGetTextDatum(detail);
	tuplestore_putvalues(sink->rsinfo->setResult, sink->rsinfo->setDesc, values, nulls);
	pfree(DatumGetPointer(values[0]));
	pfree(DatumGetPointer(values[2]));
}

/* a copy of the server's lock data, freed (GetLockStatusData has no function of its own for it) */
static void
kc_free_lockdata(LockData *ld)
{
	pfree(ld->locks);
	pfree(ld);
}

/*
 * Is the advisory lock held by a session of a superuser (another session than
 * this one)?  The gate waits only for such a holder (C11-07): the settings
 * that name the lock are administrator settings, but an ordinary role can
 * take any advisory lock, and must not be able to hold up a check that an
 * administrator's session runs.  A holder that is a prepared transaction (no
 * process) does not count either.
 */
static bool
kc_gate_held_by_superuser(const LOCKTAG *tag)
{
	LockData   *ld = GetLockStatusData();
	int			i;
	bool		found = false;

	for (i = 0; i < ld->nelements; i++)
	{
		LockInstanceData *l = &ld->locks[i];
		PGPROC	   *proc;

		if (l->holdMask == 0 || l->pid == 0 || l->pid == MyProcPid ||
			l->locktag.locktag_lockmethodid != tag->locktag_lockmethodid ||
			l->locktag.locktag_type != tag->locktag_type ||
			l->locktag.locktag_field1 != tag->locktag_field1 ||
			l->locktag.locktag_field2 != tag->locktag_field2 ||
			l->locktag.locktag_field3 != tag->locktag_field3 ||
			l->locktag.locktag_field4 != tag->locktag_field4)
			continue;
		proc = BackendPidGetProc(l->pid);
		if (proc != NULL && superuser_arg(proc->roleId))
		{
			found = true;
			break;
		}
	}
	kc_free_lockdata(ld);
	return found;
}

/*
 * The test gates (settings kingres_columnar.check_gate and
 * kingres_columnar.check_gate_orphans and kingres_columnar.check_gate_claims): wait while another session of a
 * superuser holds the advisory lock (key, 0).  Never taken by anything but a
 * test.  KC_GATE_TABLES is called at the start of the look at each table,
 * inside the per-table guard, so that a test can also cancel or time out the
 * call at that point; KC_GATE_ORPHANS in the orphan step (kc_catalog.c), after
 * its first snapshot and before its read of pg_class, KC_GATE_CLAIMS between
 * that read and the second snapshot.  The waits are bounded
 * (two minutes) and can be cancelled.
 */
void
kc_check_gate(int which)
{
	kc_gate_wait((which == KC_GATE_ORPHANS) ? kc_guc_check_gate_orphans :
				 (which == KC_GATE_CLAIMS) ? kc_guc_check_gate_claims : kc_guc_check_gate);
}

/* The wait of every gate, also those of the writer (kingres_columnar.reserve_gate). */
void
kc_gate_wait(int key)
{
	LOCKTAG		tag;
	int			i;

	if (key == 0)
		return;
	SET_LOCKTAG_ADVISORY(tag, MyDatabaseId, (uint32) key, 0, 2);
	for (i = 0; i < 12000; i++)
	{
		if (LockAcquire(&tag, ShareLock, false, true) != LOCKACQUIRE_NOT_AVAIL)
		{
			(void) LockRelease(&tag, ShareLock, false);
			return;
		}
		if (!kc_gate_held_by_superuser(&tag))
			return;
		CHECK_FOR_INTERRUPTS();
		(void) WaitLatch(MyLatch, WL_LATCH_SET | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH, 10, WAIT_EVENT_PG_SLEEP);
		ResetLatch(MyLatch);
	}
}

/*
 * Who holds a lock that conflicts with mode on a relation, for the
 * not_checked rows (C11-09): the process ids of the sessions, and whether a
 * prepared transaction (no process) holds one, so that an operator is told what
 * to end.  Read from the lock manager's own tables without taking any lock
 * of the relation, so it cannot wait; the answer is a moment's view.  The text
 * starts with a space and is empty when nobody was found (the holder let go
 * in the meantime).
 *
 * The lock data of the whole server is copied once for up to KC_LOCKDATA_USES
 * rows of a call (and freed at the end of it): a copy for every row costs the
 * size of the lock table times the number of rows, in memory and in time with
 * every lock partition held, which grew with the square of the number of
 * tables that a large DDL locks (C12-02).  The view is a moment's view anyway.
 */
#define KC_LOCKDATA_USES 64

static void
kc_lock_blockers(KcCheckSink *sink, Oid relid, LOCKMODE mode, char *out, size_t len)
{
	LOCKTAG		tag;
	LockData   *ld;
	LockMethod	lm;
	int			i;
	int			npids = 0;
	bool		prepared = false;
	char		pids[160] = "";
	size_t		used = 0;

	out[0] = '\0';
	if (!OidIsValid(relid) || mode == NoLock)
		return;
	SET_LOCKTAG_RELATION(tag, MyDatabaseId, relid);
	lm = GetLockTagsMethodTable(&tag);
	if (sink->ld != NULL && sink->ld_uses >= KC_LOCKDATA_USES)
	{
		kc_free_lockdata(sink->ld);
		sink->ld = NULL;
	}
	if (sink->ld == NULL)
	{
		sink->ld = GetLockStatusData();
		sink->ld_uses = 0;
	}
	sink->ld_uses++;
	ld = sink->ld;
	for (i = 0; i < ld->nelements; i++)
	{
		LockInstanceData *l = &ld->locks[i];

		if (l->holdMask == 0 || l->pid == MyProcPid ||
			l->locktag.locktag_type != LOCKTAG_RELATION ||
			l->locktag.locktag_field1 != tag.locktag_field1 ||
			l->locktag.locktag_field2 != tag.locktag_field2 ||
			(l->holdMask & lm->conflictTab[mode]) == 0)
			continue;
		if (l->pid == 0)
			prepared = true;
		else if (npids < 5 && used < sizeof(pids) - 16)
		{
			used += snprintf(pids + used, sizeof(pids) - used, "%s%d", npids ? ", " : "", l->pid);
			npids++;
		}
	}
	if (npids > 0 && prepared)
		snprintf(out, len, " It is held by process %s and by a prepared transaction.", pids);
	else if (npids > 0)
		snprintf(out, len, " It is held by process %s.", pids);
	else if (prepared)
		snprintf(out, len, " It is held by a prepared transaction (see pg_prepared_xacts).");
}

static void
kc_check_one(Oid relid, bool fix, KcCheckSink *sink)
{
	LOCKMODE	lockmode = fix ? ShareUpdateExclusiveLock : AccessShareLock;
	Relation	rel;
	Snapshot	cs;
	int			n;
	int			i;
	KcStripeInfo *s;
	BlockNumber nblocks;

	kc_check_gate(KC_GATE_TABLES);

	/*
	 * Never wait for a lock: a prepared transaction that holds a table
	 * exclusively (a prepared TRUNCATE, a rewrite) keeps it until someone
	 * ends it, and the check would wait with it, without end.  A table that
	 * cannot be locked now is reported as not checked, which is a row like
	 * any other (C9-04: a caller that tests for the absence of rows, or
	 * asks columnar_check_clean, cannot mistake it for a clean table),
	 * and the rest goes on.
	 */
	if (!ConditionalLockRelationOid(relid, lockmode))
	{
		char		who[240];
		char		detail[640];

		kc_lock_blockers(sink, relid, lockmode, who, sizeof(who));
		snprintf(detail, sizeof(detail), "%s%s",
				 "the table is locked by another transaction (a prepared one, or ordinary DDL or VACUUM FULL that is running) and was not checked; this is not a clean result: end or wait for that transaction and run the check again.",
				 who);
		kc_check_row(sink, "not_checked", relid, detail);
		return;
	}
	rel = try_relation_open(relid, NoLock);
	if (rel == NULL || !kc_rel_is_columnar(rel))
	{
		/*
		 * Dropped, or made another kind of table, since the list was made
		 * (C10-01): it is not a table this call looked at, which is not a clean
		 * result either.
		 */
		bool		dropped = (rel == NULL);

		if (rel)
			relation_close(rel, lockmode);
		else
			UnlockRelationOid(relid, lockmode);
		kc_check_row(sink, "not_checked", relid,
					 dropped ? "the table was dropped during the call and was not checked"
					 : "the table is no longer a columnar table (its access method was changed during the call) and was not checked");
		return;
	}

	/*
	 * The stripes as of now, whatever the isolation level of the caller
	 * (C10-01): the caller's own snapshot, in REPEATABLE READ or SERIALIZABLE,
	 * does not see a stripe committed after it, nor any stripe of a table made
	 * after it, and would call a damaged table clean.  The orphan step has
	 * always read the catalog this way.
	 */
	cs = kc_latest_snapshot();
	s = kc_catalog_load(rel, cs, &n);
	pfree(cs);
	nblocks = RelationGetNumberOfBlocks(rel);
	for (i = 0; i < n; i++)
	{
		const char *problem = NULL;
		char		detail[256];

		CHECK_FOR_INTERRUPTS();
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
		kc_check_row(sink, problem, RelationGetRelid(rel), detail);
		if (fix && strcmp(problem, "stripe_beyond_file") == 0)
			(void) kc_catalog_delete_stripe(kc_rel_ident(rel)->uid, s[i].stripe_id, false);
	}
	if (s)
	{
		/* the stripe list of this table is not kept for the rest of the call */
		for (i = 0; i < n; i++)
		{
			pfree(s[i].cid_points);
			pfree(s[i].col_stats);
		}
		pfree(s);
	}

	/*
	 * A reserved stripe row that a committed transaction left (C6a): a writer
	 * that reserved numbers replaces the row by its stripe's before it commits or
	 * prepares, so a row that is still "reserved" and is seen by everything
	 * committed is damage.  What this does not report, on purpose: the reserved
	 * row of a transaction that is in progress (its writer is alive, and the
	 * snapshot does not see it), of one that aborted, of a backend that died (the
	 * heap's own rules make such a row dead, and a vacuum of the catalog takes it
	 * away), of a prepared transaction (it has written its stripes), and those of
	 * this session's own transaction.
	 */
	{
		Snapshot	rs = kc_latest_snapshot();
		int			nres = kc_catalog_count_reserved(rel, rs);

		pfree(rs);
		if (nres > 0)
		{
			char		detail[160];

			snprintf(detail, sizeof(detail), "%d reserved stripe row(s) of committed transactions: row numbers were reserved and no stripe was written for them",
					 nres);
			kc_check_row(sink, "reserved_row_left", RelationGetRelid(rel), detail);
		}
	}

	/*
	 * Let go of the table's lock now (C11-03).  A read leaves nothing that has
	 * to be protected until the end of the transaction, and a lock kept for
	 * every table would need an entry in the shared lock table for each of
	 * them at once ("out of shared memory" with more tables than it has room
	 * for, and entries taken from the other sessions meanwhile).  The lock
	 * was taken by this subtransaction, so what the caller's transaction held
	 * on the table before is not touched.
	 */
	relation_close(rel, lockmode);
}

/*
 * Whether an error raised while a table was read is about that table: damage
 * it shows (corrupt data, an unreadable or missing file, a format this server
 * does not read, a value that cannot be decoded).  Only those become an
 * `unreadable` row.  Everything else is not the table's: a query cancel and
 * statement_timeout (57014), the shutdown of the server (57P01 and its class),
 * a lock timeout or deadlock, running out of memory, and any error of code
 * that is not about a table at all.  Those end the call (C10-02).
 */
static bool
kc_check_error_is_about_table(int sqlerrcode)
{
	int			cat = ERRCODE_TO_CATEGORY(sqlerrcode);

	return cat == ERRCODE_TO_CATEGORY(ERRCODE_INTERNAL_ERROR) ||	/* XX: corrupt data and index, internal */
		cat == ERRCODE_TO_CATEGORY(ERRCODE_IO_ERROR) ||	/* 58: I/O error, file missing */
		cat == ERRCODE_TO_CATEGORY(ERRCODE_DATA_EXCEPTION) ||	/* 22 */
		sqlerrcode == ERRCODE_FEATURE_NOT_SUPPORTED;
}

/*
 * One relation at a time, each in a subtransaction: a damaged relation is
 * reported as a row, not as an error that hides every other relation.
 */
static void
kc_check_guarded(Oid relid, bool fix, KcCheckSink *sink)
{
	MemoryContext oldcxt = CurrentMemoryContext;
	ResourceOwner oldowner = CurrentResourceOwner;

	BeginInternalSubTransaction(NULL);
	MemoryContextSwitchTo(oldcxt);
	PG_TRY();
	{
		kc_check_one(relid, fix, sink);
		ReleaseCurrentSubTransaction();
		MemoryContextSwitchTo(oldcxt);
		CurrentResourceOwner = oldowner;
	}
	PG_CATCH();
	{
		ErrorData  *ed;

		MemoryContextSwitchTo(oldcxt);
		ed = CopyErrorData();
		FlushErrorState();
		RollbackAndReleaseCurrentSubTransaction();
		MemoryContextSwitchTo(oldcxt);
		CurrentResourceOwner = oldowner;
		if (!kc_check_error_is_about_table(ed->sqlerrcode))
			ReThrowError(ed);
		kc_check_row(sink, "unreadable", relid, ed->message);
		FreeErrorData(ed);
	}
	PG_END_TRY();
}

/* The oids of the columnar tables and materialized views, as pg_class lists them now. */
static List *
kc_check_list_tables(void)
{
	Relation	pgclass;
	TableScanDesc scan;
	HeapTuple	tup;
	List	   *relids = NIL;
	KcAmMemo	memo = {InvalidOid, KC_AM_UNKNOWN, true};

	/* a catalog snapshot of this moment, not one kept from an earlier statement */
	InvalidateCatalogSnapshot();
	pgclass = table_open(RelationRelationId, AccessShareLock);
	scan = table_beginscan_catalog(pgclass, 0, NULL);
	while ((tup = heap_getnext(scan, ForwardScanDirection)) != NULL)
	{
		Form_pg_class cl = (Form_pg_class) GETSTRUCT(tup);

		/* by the handler of the access method, as everywhere (C11-02) */
		if ((cl->relkind == RELKIND_RELATION || cl->relkind == RELKIND_MATVIEW) &&
			kc_am_class_memo(cl->relam, &memo) == KC_AM_COLUMNAR)
			relids = lappend_oid(relids, cl->oid);
	}
	table_endscan(scan);
	table_close(pgclass, AccessShareLock);
	return relids;
}

/*
 * columnar_check(fix): the stripe catalog against the files.  Reports rows of
 * files no relation uses (fix deletes them), stripes that run past the end of
 * their file (fix deletes the catalog row: the data is unreadable), and
 * stripes whose first page does not belong to them (reported only).
 *
 * Every lock it takes is taken without waiting (kc_catalog_lock_nowait for the
 * relations it reads itself, ConditionalLockRelationOid for each table), and
 * what cannot be locked is a not_checked row.  The list of tables is made first
 * and again at the end: a table that appears in the second list only was made
 * during the call and was not looked at, which is a not_checked row, never a
 * clean result (C10-01).
 */
static void
kc_check_run(bool fix, KcCheckSink *sink)
{
	KcCatLocks	locks;
	List	   *relids;
	List	   *later;
	ListCell   *lc;

	kc_am_failed_reset();
	if (!kc_catalog_lock_nowait(&locks, fix ? RowExclusiveLock : AccessShareLock))
	{
		char		who[240];
		char		detail[800];

		char	   *rn = OidIsValid(locks.failed_oid) ? get_rel_name(locks.failed_oid) : NULL;

		kc_lock_blockers(sink, locks.failed_oid, locks.failed_mode, who, sizeof(who));
		snprintf(detail, sizeof(detail), "%s%s%s%s%s",
				 "pg_class or the stripe catalog (its index or its TOAST table) is locked by another transaction (a prepared one, or an administrator's LOCK TABLE, REINDEX or VACUUM FULL); no table was checked, and this is not a clean result: end or wait for that transaction and run the check again.",
				 rn ? " The lock that could not be had is on \"" : "",
				 rn ? rn : "",
				 rn ? "\"." : "",
				 who);
		kc_check_row(sink, "not_checked", InvalidOid, detail);
		if (sink->ld != NULL)
		{
			kc_free_lockdata(sink->ld);
			sink->ld = NULL;
		}
		return;
	}

	relids = kc_check_list_tables();
	foreach(lc, relids)
	{
		kc_check_guarded(lfirst_oid(lc), fix, sink);
	}

	later = kc_check_list_tables();
	foreach(lc, later)
	{
		if (!list_member_oid(relids, lfirst_oid(lc)))
			kc_check_row(sink, "not_checked", lfirst_oid(lc),
						 "the table was made during the call and was not checked; this is not a clean result: run the check again");
	}
	list_free(later);
	list_free(relids);

	{
		int			d;
		const char *why = NULL;

		if (fix)
			d = kc_catalog_reconcile(&why, true);
		else
			d = kc_catalog_count_orphans(&why);
		if (d < 0)
		{
			/* the step did not run: never the same answer as a step that ran and found nothing (C12-01) */
			char		detail[400];

			snprintf(detail, sizeof(detail), "the orphan step could not be done: %s; stripe catalog rows of files no relation uses were not looked for, and this is not a clean result: end or wait for the cause and run the check again.",
					 why ? why : "it stood down");
			kc_check_row(sink, "not_checked", InvalidOid, detail);
		}
		else if (d > 0)
		{
			char		detail[100];

			snprintf(detail, sizeof(detail), "%d stripe catalog rows belong to files no relation uses%s",
					 d, fix ? " (deleted)" : "");
			kc_check_row(sink, "orphan_catalog_rows", InvalidOid, detail);
		}
	}
	kc_catalog_unlock(&locks);
	if (sink->ld != NULL)
	{
		kc_free_lockdata(sink->ld);
		sink->ld = NULL;
	}
}

/*
 * columnar_check(fix) is a read of the catalog and the files, except that it
 * begins a subtransaction for each table, and its own decision follows from
 * that (C9-03): where the server cannot begin one it is refused with a WARNING
 * and the answer is empty (kc_subxact_refused), which is not an answer that
 * the tables are clean, and the warning says so.  The superuser test comes
 * after the refusal because an error is not allowed at those moments.
 */
Datum
kingres_columnar_check(PG_FUNCTION_ARGS)
{
	bool		fix = PG_GETARG_BOOL(0);
	ReturnSetInfo *rsinfo = (ReturnSetInfo *) fcinfo->resultinfo;
	KcCheckSink sink;

	if (kc_subxact_refused(fix ? "columnar_check(true)" : "columnar_check(false)"))
		return (Datum) 0;
	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("must be superuser to run columnar_check")));
	if (fix && RecoveryInProgress())
		ereport(ERROR,
				(errcode(ERRCODE_READ_ONLY_SQL_TRANSACTION),
				 errmsg("cannot repair columnar tables during recovery")));
	InitMaterializedSRF(fcinfo, 0);
	sink.rsinfo = rsinfo;
	sink.rows = 0;
	sink.ld = NULL;
	sink.ld_uses = 0;
	kc_check_run(fix, &sink);
	return (Datum) 0;
}

/*
 * columnar_check_clean(): the strict form (C9-04).  True only when every
 * columnar table was checked and nothing was found: it is false for any row
 * columnar_check() would return, a table that could not be locked
 * (not_checked) included, so that a script that asks "is it clean" cannot pass
 * on a table that was not looked at.  Refused where a subtransaction cannot be
 * begun (false, with the warning).
 */
Datum
kingres_columnar_check_clean(PG_FUNCTION_ARGS)
{
	KcCheckSink sink;

	if (kc_subxact_refused("columnar_check_clean"))
		PG_RETURN_BOOL(false);
	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("must be superuser to run columnar_check_clean")));
	sink.rsinfo = NULL;
	sink.rows = 0;
	sink.ld = NULL;
	sink.ld_uses = 0;
	kc_check_run(false, &sink);
	PG_RETURN_BOOL(sink.rows == 0);
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
	Relation	rel = kc_open_columnar_read(relid, "columnar_identity");
	uint64		uid;

	if (rel == NULL)
		PG_RETURN_NULL();
	uid = kc_rel_ident(rel)->uid;
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
	Relation	rel = kc_open_columnar_read(relid, "columnar_creating_xid");
	KcMetaData	m;

	if (rel == NULL)
		PG_RETURN_NULL();
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
	Relation	rel = kc_open_columnar_read(relid, "columnar_table_xid");
	KcMetaData	m;

	if (rel == NULL)
		PG_RETURN_NULL();
	kc_meta_read(rel, &m);
	relation_close(rel, AccessShareLock);
	PG_RETURN_TRANSACTIONID(m.table_xid);
}
