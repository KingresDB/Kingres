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
 *	                  finish (table_finish_bulk_insert)
 *
 *	  kcevents_open(table)   open the table (RowExclusiveLock) and hold it
 *	                         until the end of the transaction, for the writes
 *	                         at the moments above
 *	  kcevents_crit()        write one row inside a critical section (what a
 *	                         caller must never do; shows what the server does)
 *	  kcevents_writes()      how many writes the module has made
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

#include "access/table.h"
#include "access/tableam.h"
#include "access/xact.h"
#include "executor/tuptable.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "storage/ipc.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/resowner.h"

#include "kcevents_compat.h"

PG_MODULE_MAGIC;

PG_FUNCTION_INFO_V1(kcevents_open);
PG_FUNCTION_INFO_V1(kcevents_crit);
PG_FUNCTION_INFO_V1(kcevents_writes);
PG_FUNCTION_INFO_V1(kcevents_write_now);

static char *kce_at = NULL;
static char *kce_op = NULL;
static int	kce_value = 1;
static int	kce_count = 1;

static Relation kce_rel = NULL;
static TupleTableSlot *kce_slot = NULL;
static int64 kce_writes = 0;

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
	if (kce_selected(name))
		kce_write();
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

void
_PG_init(void)
{
	DefineCustomStringVariable("kcevents.at",
							   "The moment at which to write one row.",
							   NULL, &kce_at, "",
							   PGC_USERSET, 0, NULL, NULL, NULL);
	DefineCustomStringVariable("kcevents.op",
							   "What to do at that moment: insert (default), bulk, delete, update, lock, speculative.",
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
	RegisterXactCallback(kce_xact_callback, NULL);
	RegisterSubXactCallback(kce_subxact_callback, NULL);
	RegisterResourceReleaseCallback(kce_release_callback, NULL);
	on_proc_exit(kce_exit_callback, (Datum) 0);
}
