/*-------------------------------------------------------------------------
 *
 * kcevents.c
 *	  A module for tests of kingres_columnar only (never installed, never part
 *	  of a release).  It registers a callback for every event of the
 *	  transaction and the subtransaction callbacks, a callback for each phase
 *	  of the release of a resource owner and a callback for the exit of the
 *	  process, and writes one row into a table at the event a setting selects,
 *	  with table_tuple_insert on a relation that it holds open.  What a table
 *	  access method does for a write at each of those moments is what the
 *	  tests look at; run on a heap table the same module shows what PostgreSQL
 *	  itself does there.
 *
 *	  kcevents.at     the moment to write at: one of pre_commit, commit,
 *	                  pre_prepare, prepare, abort, start_sub, pre_commit_sub,
 *	                  commit_sub, abort_sub, res_before_locks, res_locks,
 *	                  res_after_locks, proc_exit, or empty (never)
 *	  kcevents.value  the value written (column "a", an int; the table may
 *	                  have more columns, they are null)
 *	  kcevents.count  how many rows to write at that moment (default 1)
 *	  kcevents.op     what to do there: insert (default), bulk (a batch with
 *	                  table_multi_insert), delete, update, lock, speculative,
 *	                  finish (table_finish_bulk_insert), and one operation for
 *	                  every other entry point of the table access method:
 *	                  ntrunc (relation_nontransactional_truncate), setnew
 *	                  (relation_set_new_filelocator), copydata
 *	                  (relation_copy_data), cluster (relation_copy_for_cluster),
 *	                  vacuum (relation_vacuum), scan (scan_begin,
 *	                  scan_getnextslot, scan_end), size (relation_size),
 *	                  estimate (relation_estimate_size), idxfetch (the index
 *	                  fetch callbacks), fetchrow (tuple_fetch_row_version),
 *	                  tidvalid (tuple_tid_valid), idxdel (index_delete_tuples),
 *	                  idxbuild (index_build_range_scan), idxvalidate
 *	                  (index_validate_scan), sample (the TABLESAMPLE callbacks);
 *	                  and, for the other entry points of the extension: objhook
 *	                  (the object access hook, for a drop of the table), fn
 *	                  (the SQL functions registered with kcevents_fn)
 *	  kcevents.crit   on: run the operation inside a critical section, which
 *	                  makes a build with assertions trap any allocation that the
 *	                  access method makes on the way (the refusal path of the
 *	                  extension allocates nothing)
 *	  kcevents.once   on: act at the first moment the setting names and not again
 *	                  in the same transaction (a script with several savepoints
 *	                  then makes one call and not one for each of them)
 *	  kcevents.hook_raise
 *	                  on: an emit_log_hook that raises an ERROR for a message
 *	                  that contains "refused on columnar" (what a hook of
 *	                  another module may do to any WARNING)
 *
 *	  kcevents_open(table)   open the table (RowExclusiveLock) and hold it
 *	                         until the end of the transaction, for the writes
 *	                         at the moments above
 *	  kcevents_crit()        write one row inside a critical section (what a
 *	                         caller must never do; shows what the server does)
 *	  kcevents_writes()      how many writes the module has made
 *	  kcevents_fn(name, nargs)
 *	                         register a C function of the extension (the name of
 *	                         its symbol, and how many arguments it takes) to be
 *	                         called by the op fn, the way another module calls it
 *	                         directly: the first argument is the table, the others
 *	                         null; the list is forgotten at the end of the
 *	                         transaction
 *	  kcevents_walk(table)   one row for every callback of the table's access
 *	                         method routine: its offset, its name and the
 *	                         decision the extension has recorded for it (null:
 *	                         none)
 *	  kcevents_write_now(table, value)
 *	                         write one row now (declared PARALLEL SAFE by the
 *	                         tests: what a table does when a worker writes)
 *
 *	  The module logs every event it sees (LOG, "kcevents: event <name>"), so
 *	  a test can read the order in which the callbacks ran.
 *
 *	  Callbacks run last-registered first.  Loaded before kingres_columnar in
 *	  shared_preload_libraries this module's callbacks run after the access
 *	  method's; loaded after it, before.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/relscan.h"
#include "access/table.h"
#include "access/tableam.h"
#include "access/xact.h"
#include "catalog/objectaccess.h"
#include "catalog/pg_class.h"
#include "commands/vacuum.h"
#include "executor/tuptable.h"
#include "fmgr.h"
#include "funcapi.h"
#include "miscadmin.h"
#include "storage/ipc.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/resowner.h"
#include "utils/snapmgr.h"
#include "utils/tuplestore.h"

#include "kcevents_compat.h"

PG_MODULE_MAGIC;

PG_FUNCTION_INFO_V1(kcevents_open);
PG_FUNCTION_INFO_V1(kcevents_crit);
PG_FUNCTION_INFO_V1(kcevents_writes);
PG_FUNCTION_INFO_V1(kcevents_write_now);
PG_FUNCTION_INFO_V1(kcevents_fn);
PG_FUNCTION_INFO_V1(kcevents_walk);

static char *kce_at = NULL;
static char *kce_op = NULL;
static int	kce_value = 1;
static int	kce_count = 1;
static bool kce_crit = false;
static bool kce_once = false;
static bool kce_fired = false;
static bool kce_hook_raise = false;

#define KCE_MAX_FNS 8
static PGFunction kce_fns[KCE_MAX_FNS];
static int	kce_fn_nargs[KCE_MAX_FNS];
static int	kce_nfns = 0;
static Snapshot kce_snap = NULL;
static bool kce_released = false;
static emit_log_hook_type kce_prev_emit_log_hook = NULL;

static Relation kce_rel = NULL;
static TupleTableSlot *kce_slot = NULL;
static int64 kce_writes = 0;

/* a number for a file that no relation has; the callbacks that make a file are given it */
static RelFileLocator
kce_new_locator(void)
{
	RelFileLocator nl = kce_rel->rd_locator;

	nl.relNumber = kce_rel->rd_locator.relNumber + 1000000;
	return nl;
}

/*
 * The operations for the entry points of the table access method other than the
 * writes (and for those of the extension itself).  Each logs "kcevents: <op>
 * done" with what it saw, so that a test can tell a call that returned from one
 * that raised.  Returns true when op named one of them.
 */
static bool
kce_entry_op(const char *op)
{
	ItemPointerData tid;

	ItemPointerSet(&tid, 0, 1);
	if (strcmp(op, "ntrunc") == 0)
	{
		table_relation_nontransactional_truncate(kce_rel);
		ereport(LOG, (errmsg("kcevents: ntrunc done")));
	}
	else if (strcmp(op, "setnew") == 0)
	{
		RelFileLocator nl = kce_new_locator();
		TransactionId fx = 12345;
		MultiXactId mm = 12345;

		table_relation_set_new_filelocator(kce_rel, &nl, kce_rel->rd_rel->relpersistence, &fx, &mm);
		ereport(LOG, (errmsg("kcevents: setnew done newfile=%u freeze=%u", nl.relNumber, fx)));
	}
	else if (strcmp(op, "copydata") == 0)
	{
		RelFileLocator nl = kce_new_locator();

		table_relation_copy_data(kce_rel, &nl);
		ereport(LOG, (errmsg("kcevents: copydata done newfile=%u", nl.relNumber)));
	}
	else if (strcmp(op, "cluster") == 0)
	{
		TransactionId xc = 0;
		MultiXactId mc = 0;
		double		nt = -1,
					tv = -1,
					td = -1;

		kce_copy_for_cluster(kce_rel, kce_rel, 0, &xc, &mc, &nt, &tv, &td);
		ereport(LOG, (errmsg("kcevents: cluster done tuples=%.0f", nt)));
	}
	else if (strcmp(op, "vacuum") == 0)
	{
		VacuumParams vp;

		memset(&vp, 0, sizeof(vp));
		table_relation_vacuum(kce_rel, &vp, NULL);
		ereport(LOG, (errmsg("kcevents: vacuum done")));
	}
	else if (strcmp(op, "scan") == 0)
	{
		TableScanDesc scan = kce_beginscan(kce_rel, kce_snap);
		TupleTableSlot *slot = table_slot_create(kce_rel, NULL);
		int			n = 0;

		while (table_scan_getnextslot(scan, ForwardScanDirection, slot))
			n++;
		ExecDropSingleTupleTableSlot(slot);
		table_endscan(scan);
		ereport(LOG, (errmsg("kcevents: scan done rows=%d", n)));
	}
	else if (strcmp(op, "size") == 0)
	{
		ereport(LOG, (errmsg("kcevents: size done bytes=%lld",
							 (long long) table_relation_size(kce_rel, MAIN_FORKNUM))));
	}
	else if (strcmp(op, "estimate") == 0)
	{
		int32		widths[MaxHeapAttributeNumber];
		BlockNumber pages = 0;
		double		tuples = -1,
					allvis = -1;

		memset(widths, 0, sizeof(widths));
		table_relation_estimate_size(kce_rel, widths, &pages, &tuples, &allvis);
		ereport(LOG, (errmsg("kcevents: estimate done pages=%u tuples=%.0f", pages, tuples)));
	}
	else if (strcmp(op, "idxfetch") == 0)
	{
		IndexFetchTableData *f = kce_index_fetch_begin(kce_rel);
		bool		call_again = true,
					all_dead = true;
		bool		found;

		found = table_index_fetch_tuple(f, &tid, kce_snap, kce_slot, &call_again, &all_dead);
		table_index_fetch_reset(f);
		table_index_fetch_end(f);
		ereport(LOG, (errmsg("kcevents: idxfetch done found=%d", (int) found)));
	}
	else if (strcmp(op, "fetchrow") == 0)
	{
		bool		found = table_tuple_fetch_row_version(kce_rel, &tid, kce_snap, kce_slot);

		ereport(LOG, (errmsg("kcevents: fetchrow done found=%d", (int) found)));
	}
	else if (strcmp(op, "tidvalid") == 0)
	{
		/* a descriptor that names only the relation: the callback is refused or an error before it reads anything */
		TableScanDescData fake;
		bool		valid;

		memset(&fake, 0, sizeof(fake));
		fake.rs_rd = kce_rel;
		valid = table_tuple_tid_valid(&fake, &tid);
		ereport(LOG, (errmsg("kcevents: tidvalid done valid=%d", (int) valid)));
	}
	else if (strcmp(op, "idxdel") == 0)
	{
		TM_IndexDeleteOp del;
		TransactionId x;

		memset(&del, 0, sizeof(del));
		x = table_index_delete_tuples(kce_rel, &del);
		ereport(LOG, (errmsg("kcevents: idxdel done xid=%u", x)));
	}
	else if (strcmp(op, "idxbuild") == 0)
	{
		double		n = kce_rel->rd_tableam->index_build_range_scan(kce_rel, NULL, NULL, false, false, false,
																	0, InvalidBlockNumber, NULL, NULL, NULL);

		ereport(LOG, (errmsg("kcevents: idxbuild done tuples=%.0f", n)));
	}
	else if (strcmp(op, "idxvalidate") == 0)
	{
		table_index_validate_scan(kce_rel, NULL, NULL, kce_snap, NULL);
		ereport(LOG, (errmsg("kcevents: idxvalidate done")));
	}
	else if (strcmp(op, "sample") == 0)
	{
		TableScanDescData fake;
		bool		b;
		bool		t;

		memset(&fake, 0, sizeof(fake));
		fake.rs_rd = kce_rel;
		b = kce_rel->rd_tableam->scan_sample_next_block(&fake, NULL);
		t = kce_rel->rd_tableam->scan_sample_next_tuple(&fake, NULL, kce_slot);
		ereport(LOG, (errmsg("kcevents: sample done block=%d tuple=%d", (int) b, (int) t)));
	}
	else if (strcmp(op, "objhook") == 0)
	{
		if (object_access_hook)
			object_access_hook(OAT_DROP, RelationRelationId, RelationGetRelid(kce_rel), 0, NULL);
		ereport(LOG, (errmsg("kcevents: objhook done")));
	}
	else if (strcmp(op, "fn") == 0)
	{
		int			i;

		for (i = 0; i < kce_nfns; i++)
		{
			LOCAL_FCINFO(fcinfo, KCE_MAX_FNS);
			int			a;

			InitFunctionCallInfoData(*fcinfo, NULL, kce_fn_nargs[i],
									 InvalidOid, NULL, NULL);
			for (a = 0; a < kce_fn_nargs[i]; a++)
			{
				fcinfo->args[a].value = (Datum) 0;
				fcinfo->args[a].isnull = true;
			}
			if (kce_fn_nargs[i] > 0)
			{
				fcinfo->args[0].value = ObjectIdGetDatum(RelationGetRelid(kce_rel));
				fcinfo->args[0].isnull = false;
			}
			(void) kce_fns[i] (fcinfo);
			ereport(LOG, (errmsg("kcevents: fn %d done", i)));
		}
	}
	else
		return false;
	return true;
}

/*
 * The other things a module can do to a table through the access method: a
 * batch of rows (table_multi_insert), a delete, an update, a row lock, a
 * speculative insert.  They report the result in the log; what a columnar
 * table does for them where an error is allowed is an error (append-only), and
 * where one is forbidden a warning and "not found".
 */
static void
kce_other(const char *op)
{
	ItemPointerData tid;
	TM_FailureData tmfd;
	TM_Result	res = TM_Ok;

	ItemPointerSet(&tid, 0, 1);
	if (kce_entry_op(op))
		return;
	if (strcmp(op, "bulk") == 0)
	{
		TupleTableSlot **slots = palloc(sizeof(TupleTableSlot *) * kce_count);
		int			i;

		for (i = 0; i < kce_count; i++)
		{
			slots[i] = MakeSingleTupleTableSlot(kce_slot->tts_tupleDescriptor, &TTSOpsVirtual);
			memset(slots[i]->tts_isnull, true, slots[i]->tts_tupleDescriptor->natts);
			slots[i]->tts_values[0] = Int32GetDatum(kce_value + i);
			slots[i]->tts_isnull[0] = false;
			ExecStoreVirtualTuple(slots[i]);
		}
		table_multi_insert(kce_rel, slots, kce_count, GetCurrentCommandId(true), 0, NULL);
		kce_writes += kce_count;
		return;
	}
	if (strcmp(op, "finish") == 0)
	{
		/* the end of a bulk load: a table made in this transaction is flushed */
		table_finish_bulk_insert(kce_rel, 0);
		return;
	}
	if (strcmp(op, "speculative") == 0)
	{
		ExecClearTuple(kce_slot);
		memset(kce_slot->tts_isnull, true, kce_slot->tts_tupleDescriptor->natts);
		kce_slot->tts_values[0] = Int32GetDatum(kce_value);
		kce_slot->tts_isnull[0] = false;
		ExecStoreVirtualTuple(kce_slot);
		table_tuple_insert_speculative(kce_rel, kce_slot, GetCurrentCommandId(true), 0, NULL, 1);
		return;
	}
	if (strcmp(op, "delete") == 0)
	{
		res = kce_table_tuple_delete(kce_rel, &tid, GetCurrentCommandId(true),
									 true, &tmfd);
	}
	else if (strcmp(op, "update") == 0)
	{
		LockTupleMode lockmode;
		TU_UpdateIndexes idx;

		ExecClearTuple(kce_slot);
		memset(kce_slot->tts_isnull, true, kce_slot->tts_tupleDescriptor->natts);
		kce_slot->tts_values[0] = Int32GetDatum(kce_value);
		kce_slot->tts_isnull[0] = false;
		ExecStoreVirtualTuple(kce_slot);
		res = kce_table_tuple_update(kce_rel, &tid, kce_slot, GetCurrentCommandId(true),
									 true, &tmfd, &lockmode, &idx);
	}
	else if (strcmp(op, "lock") == 0)
	{
		ExecClearTuple(kce_slot);
		res = table_tuple_lock(kce_rel, &tid, InvalidSnapshot, kce_slot,
							   GetCurrentCommandId(true), LockTupleExclusive,
							   LockWaitBlock, 0, &tmfd);
	}
	ereport(LOG, (errmsg("kcevents: %s result %d", op, (int) res)));
}

static void
kce_write(void)
{
	int			i;

	if (kce_rel == NULL || kce_slot == NULL)
		return;
	if (kce_op != NULL && kce_op[0] != '\0' && strcmp(kce_op, "insert") != 0)
	{
		kce_other(kce_op);
		return;
	}
	for (i = 0; i < kce_count; i++)
	{
		int			c;

		ExecClearTuple(kce_slot);
		for (c = 0; c < kce_slot->tts_tupleDescriptor->natts; c++)
		{
			kce_slot->tts_values[c] = (Datum) 0;
			kce_slot->tts_isnull[c] = true;
		}
		kce_slot->tts_values[0] = Int32GetDatum(kce_value + i);
		kce_slot->tts_isnull[0] = false;
		ExecStoreVirtualTuple(kce_slot);
		table_tuple_insert(kce_rel, kce_slot, GetCurrentCommandId(true), 0, NULL);
		kce_writes++;
	}
}

static bool
kce_selected(const char *name)
{
	return kce_at != NULL && strcmp(kce_at, name) == 0;
}

static void
kce_event(const char *name)
{
	ereport(LOG, (errmsg("kcevents: event %s", name)));
	if (kce_selected(name) && !(kce_once && kce_fired))
	{
		kce_fired = true;
		if (kce_crit)
			START_CRIT_SECTION();
		kce_write();
		if (kce_crit)
			END_CRIT_SECTION();
	}
}

/*
 * Forget the held relation: the transaction that opened it is over.  The
 * relation is not closed (a test module; the leak warning of the resource
 * owner is part of the noise), so that the release phases can still write.
 */
static void
kce_forget(void)
{
	kce_rel = NULL;
	kce_fired = false;
	kce_nfns = 0;
	if (kce_snap != NULL)
	{
		/* registered with the transaction's owner, which is still there at the events (not at the release phases) */
		if (!kce_released)
			UnregisterSnapshotFromOwner(kce_snap, TopTransactionResourceOwner);
		kce_snap = NULL;
	}
	if (kce_slot != NULL)
	{
		ExecDropSingleTupleTableSlot(kce_slot);
		kce_slot = NULL;
	}
}

/* at the end of a transaction: forget, unless a later moment still writes */
static void
kce_end_of_transaction(void)
{
	if (kce_at != NULL && strncmp(kce_at, "res_", 4) == 0)
		return;
	kce_forget();
}

static void
kce_xact_callback(XactEvent event, void *arg)
{
	switch (event)
	{
		case XACT_EVENT_COMMIT:
			kce_event("commit");
			kce_end_of_transaction();
			break;
		case XACT_EVENT_PARALLEL_COMMIT:
			kce_event("parallel_commit");
			break;
		case XACT_EVENT_ABORT:
			kce_event("abort");
			kce_end_of_transaction();
			break;
		case XACT_EVENT_PARALLEL_ABORT:
			kce_event("parallel_abort");
			break;
		case XACT_EVENT_PREPARE:
			kce_event("prepare");
			kce_end_of_transaction();
			break;
		case XACT_EVENT_PRE_COMMIT:
			kce_event("pre_commit");
			break;
		case XACT_EVENT_PARALLEL_PRE_COMMIT:
			kce_event("parallel_pre_commit");
			break;
		case XACT_EVENT_PRE_PREPARE:
			kce_event("pre_prepare");
			break;
	}
}

static void
kce_subxact_callback(SubXactEvent event, SubTransactionId mySubid,
					 SubTransactionId parentSubid, void *arg)
{
	switch (event)
	{
		case SUBXACT_EVENT_START_SUB:
			kce_event("start_sub");
			break;
		case SUBXACT_EVENT_COMMIT_SUB:
			kce_event("commit_sub");
			break;
		case SUBXACT_EVENT_ABORT_SUB:
			kce_event("abort_sub");
			break;
		case SUBXACT_EVENT_PRE_COMMIT_SUB:
			kce_event("pre_commit_sub");
			break;
	}
}

static void
kce_release_callback(ResourceReleasePhase phase, bool isCommit, bool isTopLevel,
					 void *arg)
{
	if (!isTopLevel)
		return;
	switch (phase)
	{
		case RESOURCE_RELEASE_BEFORE_LOCKS:
			/* the owner has released the snapshot already */
			kce_released = true;
			kce_event("res_before_locks");
			break;
		case RESOURCE_RELEASE_LOCKS:
			kce_event("res_locks");
			break;
		case RESOURCE_RELEASE_AFTER_LOCKS:
			kce_event("res_after_locks");
			kce_forget();
			break;
	}
}

static void
kce_exit_callback(int code, Datum arg)
{
	kce_event("proc_exit");
}

Datum
kcevents_open(PG_FUNCTION_ARGS)
{
	text	   *name = PG_GETARG_TEXT_PP(0);
	Oid			relid = DatumGetObjectId(DirectFunctionCall1(regclassin,
															 CStringGetDatum(text_to_cstring(name))));
	MemoryContext old;

	kce_forget();
	kce_rel = table_open(relid, RowExclusiveLock);
	old = MemoryContextSwitchTo(TopMemoryContext);
	/* a snapshot of its own, for the operations that read (they run where none may be taken) */
	kce_released = false;
	kce_snap = RegisterSnapshotOnOwner(GetTransactionSnapshot(), TopTransactionResourceOwner);
	/* a copy of the descriptor: not reference counted, so nothing to unpin later */
	kce_slot = MakeSingleTupleTableSlot(CreateTupleDescCopy(RelationGetDescr(kce_rel)),
										&TTSOpsVirtual);
	MemoryContextSwitchTo(old);
	PG_RETURN_VOID();
}

Datum
kcevents_crit(PG_FUNCTION_ARGS)
{
	START_CRIT_SECTION();
	kce_write();
	END_CRIT_SECTION();
	PG_RETURN_VOID();
}

Datum
kcevents_writes(PG_FUNCTION_ARGS)
{
	PG_RETURN_INT64(kce_writes);
}

/*
 * kcevents_write_now(table, value): write one row into the table now, with
 * table_tuple_insert.  Declared PARALLEL SAFE in the tests, so that a query
 * can run it in a parallel worker, where a table access method must refuse to
 * write (heap does: "cannot insert tuples during a parallel operation").
 */
Datum
kcevents_write_now(PG_FUNCTION_ARGS)
{
	text	   *name = PG_GETARG_TEXT_PP(0);
	int32		value = PG_GETARG_INT32(1);
	Oid			relid = DatumGetObjectId(DirectFunctionCall1(regclassin,
															 CStringGetDatum(text_to_cstring(name))));
	Relation	rel = table_open(relid, RowExclusiveLock);
	TupleTableSlot *slot = table_slot_create(rel, NULL);
	int			c;

	ExecClearTuple(slot);
	for (c = 0; c < slot->tts_tupleDescriptor->natts; c++)
	{
		slot->tts_values[c] = (Datum) 0;
		slot->tts_isnull[c] = true;
	}
	slot->tts_values[0] = Int32GetDatum(value);
	slot->tts_isnull[0] = false;
	ExecStoreVirtualTuple(slot);
	/* not "used": a worker may not mark the command id as used */
	table_tuple_insert(rel, slot, GetCurrentCommandId(false), 0, NULL);
	ExecDropSingleTupleTableSlot(slot);
	table_close(rel, NoLock);
	PG_RETURN_INT32(value);
}

Datum
kcevents_fn(PG_FUNCTION_ARGS)
{
	char	   *name = text_to_cstring(PG_GETARG_TEXT_PP(0));

	if (kce_nfns >= KCE_MAX_FNS)
		elog(ERROR, "too many functions");
	kce_fns[kce_nfns] = (PGFunction) load_external_function("kingres_columnar", name, true, NULL);
	kce_fn_nargs[kce_nfns] = PG_GETARG_INT32(1);
	kce_nfns++;
	PG_RETURN_VOID();
}

/*
 * The members of the routine of a table's access method, the way the test
 * that every one has a decision walks them: every slot after the node tag that
 * is not null, asked of the extension (kc_am_decision, exported by it).
 */
typedef const char *(*kce_decision_fn) (Size offset, const char **member, const char **why);

Datum
kcevents_walk(PG_FUNCTION_ARGS)
{
	text	   *name = PG_GETARG_TEXT_PP(0);
	Oid			relid = DatumGetObjectId(DirectFunctionCall1(regclassin,
															 CStringGetDatum(text_to_cstring(name))));
	ReturnSetInfo *rsinfo = (ReturnSetInfo *) fcinfo->resultinfo;
	Relation	rel = table_open(relid, AccessShareLock);
	const char *base = (const char *) rel->rd_tableam;
	kce_decision_fn decide = (kce_decision_fn) load_external_function("kingres_columnar", "kc_am_decision", true, NULL);
	Size		off;

	InitMaterializedSRF(fcinfo, 0);
	for (off = offsetof(TableAmRoutine, slot_callbacks); off + sizeof(void *) <= sizeof(TableAmRoutine); off += sizeof(void *))
	{
		void	   *fn;
		const char *member = NULL;
		const char *why = NULL;
		const char *decision;
		Datum		values[4];
		bool		nulls[4] = {false, false, false, false};

		memcpy(&fn, base + off, sizeof(fn));
		if (fn == NULL)
			continue;
		decision = decide(off, &member, &why);
		values[0] = Int64GetDatum((int64) off);
		if (member != NULL)
			values[1] = CStringGetTextDatum(member);
		else
			nulls[1] = true;
		if (decision != NULL)
			values[2] = CStringGetTextDatum(decision);
		else
			nulls[2] = true;
		if (why != NULL)
			values[3] = CStringGetTextDatum(why);
		else
			nulls[3] = true;
		tuplestore_putvalues(rsinfo->setResult, rsinfo->setDesc, values, nulls);
	}
	table_close(rel, AccessShareLock);
	return (Datum) 0;
}

/* a hook that raises an error for a refusal of the extension, as a hook of another module may for any message */
static void
kce_emit_log_hook(ErrorData *edata)
{
	if (kce_hook_raise && edata->elevel == WARNING && edata->message != NULL &&
		strstr(edata->message, "refused on columnar") != NULL)
		elog(ERROR, "kcevents: the log hook raises for a refusal of the extension");
	if (kce_prev_emit_log_hook)
		kce_prev_emit_log_hook(edata);
}

void
_PG_init(void)
{
	DefineCustomStringVariable("kcevents.at",
							   "The moment at which to write one row.",
							   NULL, &kce_at, "",
							   PGC_USERSET, 0, NULL, NULL, NULL);
	DefineCustomStringVariable("kcevents.op",
							   "What to do at that moment: insert (default), bulk, delete, update, lock, speculative, and the entry-point operations named in the header of the module.",
							   NULL, &kce_op, "insert",
							   PGC_USERSET, 0, NULL, NULL, NULL);
	DefineCustomIntVariable("kcevents.value",
							"The value of column a of the row written.",
							NULL, &kce_value, 1, INT_MIN, INT_MAX,
							PGC_USERSET, 0, NULL, NULL, NULL);
	DefineCustomIntVariable("kcevents.count",
							"How many rows to write at that moment.",
							NULL, &kce_count, 1, 1, 1000000,
							PGC_USERSET, 0, NULL, NULL, NULL);
	DefineCustomBoolVariable("kcevents.crit",
							 "Run the operation inside a critical section.",
							 NULL, &kce_crit, false,
							 PGC_USERSET, 0, NULL, NULL, NULL);
	DefineCustomBoolVariable("kcevents.once",
							 "Act only once per transaction at the moment named.",
							 NULL, &kce_once, false,
							 PGC_USERSET, 0, NULL, NULL, NULL);
	DefineCustomBoolVariable("kcevents.hook_raise",
							 "Raise an error from the log hook for a refusal of the extension.",
							 NULL, &kce_hook_raise, false,
							 PGC_USERSET, 0, NULL, NULL, NULL);
	kce_prev_emit_log_hook = emit_log_hook;
	emit_log_hook = kce_emit_log_hook;
	RegisterXactCallback(kce_xact_callback, NULL);
	RegisterSubXactCallback(kce_subxact_callback, NULL);
	RegisterResourceReleaseCallback(kce_release_callback, NULL);
	on_proc_exit(kce_exit_callback, (Datum) 0);
}
