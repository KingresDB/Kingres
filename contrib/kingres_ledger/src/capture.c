/*-------------------------------------------------------------------------
 *
 * capture.c
 *	  Write capture of the ledger: the insert trigger, the blocking triggers
 *	  and the per-transaction state.
 *
 * kingres.ledger_capture() is an AFTER INSERT ... FOR EACH ROW trigger
 * installed by kingres.ledger_enable.  For every inserted row it computes
 * the row hash and appends a row to kingres.ledger_entry; on the first
 * entry of a transaction it also queues the transaction in
 * kingres.ledger_pending.  It runs as the extension owner (SECURITY
 * DEFINER) because ordinary roles have no write privilege on the ledger
 * tables.
 *
 * Per-transaction state (the next sequence number and whether the pending
 * row has been written) lives in backend memory.  Two kinds of callback keep
 * it right: a transaction callback resets it at transaction end, and a
 * subtransaction callback undoes the "pending row written" flag when the
 * subtransaction that wrote the pending row is rolled back, because the row
 * disappears with it.  The sequence counter is deliberately not rewound: gaps
 * in seq are harmless, reuse would not be.
 *
 * kingres.ledger_block_modify() is the trigger function that makes the
 * ledger tables and the user's ledger tables append-only.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/capture.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "access/table.h"
#include "access/tableam.h"
#include "access/xact.h"
#include "catalog/pg_class.h"
#include "catalog/pg_trigger.h"
#include "catalog/pg_type.h"
#include "executor/tuptable.h"
#include "commands/trigger.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "utils/builtins.h"
#include "utils/rel.h"

#include "kingres_ledger.h"
#include "row_hash.h"

PG_FUNCTION_INFO_V1(kingres_ledger_capture);
PG_FUNCTION_INFO_V1(kingres_ledger_block_modify);
PG_FUNCTION_INFO_V1(kingres_ledger_record_row);

/*
 * State of the transaction that is currently writing entries.  txid is zero
 * when no entry has been written in the current top-level transaction.
 */
typedef struct KlTxState
{
	uint64		txid;			/* top-level transaction id (xid8) */
	int32		next_seq;		/* sequence number of the next entry */
	bool		pending_written;	/* ledger_pending row inserted? */
	SubTransactionId pending_subid; /* subtransaction that inserted it */
} KlTxState;

static KlTxState kl_state = {0, 0, false, InvalidSubTransactionId};

/* Saved SPI plans of the two inserts, prepared on first use. */
static SPIPlanPtr kl_entry_plan = NULL;
static SPIPlanPtr kl_pending_plan = NULL;

/*
 * kl_xact_callback
 *		Forget the per-transaction state when the top-level transaction ends,
 *		whichever way.  The cached relid to table_id mapping is dropped too,
 *		because a rolled back ledger_enable must not leave it behind.
 */
static void
kl_xact_callback(XactEvent event, void *arg)
{
	switch (event)
	{
		case XACT_EVENT_COMMIT:
			/* Tell the launcher that this database has pending transactions. */
			if (kl_state.pending_written)
				kl_mark_dirty(MyDatabaseId);
			pg_fallthrough;
		case XACT_EVENT_ABORT:
		case XACT_EVENT_PREPARE:
			kl_state.txid = 0;
			kl_state.next_seq = 0;
			kl_state.pending_written = false;
			kl_state.pending_subid = InvalidSubTransactionId;
			kl_write_reset();
			kl_table_cache_reset();
			break;
		default:
			break;
	}
}

/*
 * kl_subxact_callback
 *		Keep "pending_written" true only while the ledger_pending row is
 *		still alive.
 *
 * When the subtransaction that inserted the pending row commits, the row now
 * belongs to its parent.  When it aborts, the row is gone and the next entry
 * of the transaction must insert it again; otherwise the transaction would
 * have entries that no block ever picks up.
 */
static void
kl_subxact_callback(SubXactEvent event, SubTransactionId mySubid,
					SubTransactionId parentSubid, void *arg)
{
	/*
	 * A rolled back subtransaction may have registered a table
	 * (ledger_enable) or removed a registration, and the caches map the OID
	 * of a table to what the registry said while it was in force; a table
	 * that is registered again after a ROLLBACK TO SAVEPOINT or an exception
	 * block gets another table_id.  Whatever was cached is dropped (finding
	 * L2-05); the next lookup reads the registry again.
	 */
	if (event == SUBXACT_EVENT_ABORT_SUB)
		kl_table_cache_reset();

	if (!kl_state.pending_written || kl_state.pending_subid != mySubid)
		return;

	if (event == SUBXACT_EVENT_COMMIT_SUB)
		kl_state.pending_subid = parentSubid;
	else if (event == SUBXACT_EVENT_ABORT_SUB)
	{
		kl_state.pending_written = false;
		kl_state.pending_subid = InvalidSubTransactionId;
	}
}

/*
 * kl_capture_init
 *		Register the transaction callbacks.  Called from _PG_init.
 */
void
kl_capture_init(void)
{
	RegisterXactCallback(kl_xact_callback, NULL);
	RegisterSubXactCallback(kl_subxact_callback, NULL);
}

/*
 * kl_prepare_plan
 *		Prepare and keep a plan for a fixed INSERT statement.  The statements
 *		name their target with the schema so that the search_path of the
 *		session cannot redirect them.
 */
static SPIPlanPtr
kl_prepare_plan(const char *sql, int nargs, Oid *argtypes)
{
	SPIPlanPtr	plan = SPI_prepare(sql, nargs, argtypes);

	if (plan == NULL)
		elog(ERROR, "SPI_prepare failed: %s", SPI_result_code_string(SPI_result));
	if (SPI_keepplan(plan))
		elog(ERROR, "SPI_keepplan failed");
	return plan;
}

/*
 * kl_execute_guarded
 *		Run one of the two inserts with the write marker raised (guard.c).  The
 *		statements are fixed, their arguments are values, and the tables they
 *		write have no code of a user that could run inside them (see the
 *		comment at kl_run in write.c); the marker is lowered on
 *		every way out.
 */
static int
kl_execute_guarded(SPIPlanPtr plan, Datum *values)
{
	int			rc = 0;

	kl_write_enter();
	PG_TRY();
	{
		rc = SPI_execute_plan(plan, values, NULL, false, 0);
	}
	PG_FINALLY();
	{
		kl_write_leave();
	}
	PG_END_TRY();
	return rc;
}

/*
 * kl_record_entry
 *		Append one entry for the current transaction.
 *
 * Writes the ledger_pending row when this is the first live entry of the
 * transaction, then the ledger_entry row.  The user name recorded is the
 * session's current user as seen outside this SECURITY DEFINER function.
 */
static void
kl_record_entry(int32 table_id, char op, const uint8 *row_hash,
				uint64 *txid_out, int32 *seq_out)
{
	uint64		txid = U64FromFullTransactionId(GetTopFullTransactionId());
	Datum		values[5];
	bytea	   *hash_datum;
	int			rc;

	/* A new transaction whose end callback did not run cannot happen; be safe. */
	if (kl_state.txid != txid)
	{
		kl_state.txid = txid;
		kl_state.next_seq = 0;
		kl_state.pending_written = false;
		kl_state.pending_subid = InvalidSubTransactionId;
	}

	if (kl_state.next_seq == PG_INT32_MAX)
		ereport(ERROR,
				(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
				 errmsg("too many ledger entries in one transaction")));

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	if (!kl_state.pending_written)
	{
		NameData	uname;

		if (kl_pending_plan == NULL)
		{
			Oid			argtypes[2] = {INT8OID, NAMEOID};

			kl_pending_plan = kl_prepare_plan("INSERT INTO kingres.ledger_pending "
											  "(ledger_txid, user_name) VALUES ($1, $2)",
											  2, argtypes);
		}
		namestrcpy(&uname, GetUserNameFromId(GetOuterUserId(), false));
		values[0] = Int64GetDatum((int64) txid);
		values[1] = NameGetDatum(&uname);
		rc = kl_execute_guarded(kl_pending_plan, values);
		if (rc != SPI_OK_INSERT)
			elog(ERROR, "could not queue ledger transaction: %s",
				 SPI_result_code_string(rc));
		kl_state.pending_written = true;
		kl_state.pending_subid = GetCurrentSubTransactionId();
	}

	if (kl_entry_plan == NULL)
	{
		Oid			argtypes[5] = {INT8OID, INT4OID, INT4OID, CHAROID, BYTEAOID};

		kl_entry_plan = kl_prepare_plan("INSERT INTO kingres.ledger_entry "
										"(ledger_txid, seq, table_id, op, row_hash) "
										"VALUES ($1, $2, $3, $4, $5)",
										5, argtypes);
	}
	hash_datum = (bytea *) palloc(VARHDRSZ + KL_HASH_LEN);
	SET_VARSIZE(hash_datum, VARHDRSZ + KL_HASH_LEN);
	memcpy(VARDATA(hash_datum), row_hash, KL_HASH_LEN);

	values[0] = Int64GetDatum((int64) txid);
	values[1] = Int32GetDatum(kl_state.next_seq);
	values[2] = Int32GetDatum(table_id);
	values[3] = CharGetDatum(op);
	values[4] = PointerGetDatum(hash_datum);
	rc = kl_execute_guarded(kl_entry_plan, values);
	if (rc != SPI_OK_INSERT)
		elog(ERROR, "could not write ledger entry: %s",
			 SPI_result_code_string(rc));
	*txid_out = txid;
	*seq_out = kl_state.next_seq;
	kl_state.next_seq++;

	/* hash_datum lives in the SPI procedure context, freed here. */
	SPI_finish();
}

/*
 * kl_insert_history
 *		Write the old version of a row into the history table of its ledger
 *		table, with the transaction id and the sequence number of the D entry
 *		that recorded its removal.
 *
 * The columns are matched by name (the history table gets a column appended
 * when ADD COLUMN is applied to the table, so the positions differ), and the
 * types must be the same; anything else is an error, because a history row
 * that does not carry every column would not hash like the entry.  The row is
 * stored with the table access method directly: the history table has no
 * triggers, rules or indexes (the protection rejects them), and what goes in
 * is the exact old tuple, not a copy through text.  Values stored out of line
 * in the table are fetched by the toaster, which copes with external values
 * of another table.
 */
static void
kl_insert_history(Oid histrelid, Relation rel, HeapTuple oldtup,
				  uint64 txid, int32 seq)
{
	Relation	hist = table_open(histrelid, RowExclusiveLock);
	TupleDesc	td = RelationGetDescr(rel);
	TupleDesc	hd = RelationGetDescr(hist);
	Datum	   *src_values = (Datum *) palloc(td->natts * sizeof(Datum));
	bool	   *src_nulls = (bool *) palloc(td->natts * sizeof(bool));
	TupleTableSlot *slot;
	int			assigned = 0;
	int			expected = 0;
	int			i;
	int			j;

	if (hist->rd_rel->relkind != RELKIND_RELATION)
		ereport(ERROR,
				(errcode(ERRCODE_WRONG_OBJECT_TYPE),
				 errmsg("history table of \"%s\" is not an ordinary table",
						RelationGetRelationName(rel))));

	heap_deform_tuple(oldtup, td, src_values, src_nulls);
	slot = MakeSingleTupleTableSlot(hd, &TTSOpsVirtual);
	ExecClearTuple(slot);
	memset(slot->tts_values, 0, hd->natts * sizeof(Datum));
	memset(slot->tts_isnull, true, hd->natts * sizeof(bool));

	for (j = 0; j < hd->natts; j++)
	{
		Form_pg_attribute ha = TupleDescAttr(hd, j);

		if (ha->attisdropped)
			continue;
		expected++;
		if (strcmp(NameStr(ha->attname), KL_HIST_COL_TXID) == 0)
		{
			if (ha->atttypid != INT8OID)
				elog(ERROR, "column %s of the history table must be bigint", KL_HIST_COL_TXID);
			slot->tts_values[j] = Int64GetDatum((int64) txid);
			slot->tts_isnull[j] = false;
			assigned++;
		}
		else if (strcmp(NameStr(ha->attname), KL_HIST_COL_SEQ) == 0)
		{
			if (ha->atttypid != INT4OID)
				elog(ERROR, "column %s of the history table must be integer", KL_HIST_COL_SEQ);
			slot->tts_values[j] = Int32GetDatum(seq);
			slot->tts_isnull[j] = false;
			assigned++;
		}
		else if (strcmp(NameStr(ha->attname), KL_HIST_COL_OP) == 0)
		{
			if (ha->atttypid != CHAROID)
				elog(ERROR, "column %s of the history table must be \"char\"", KL_HIST_COL_OP);
			slot->tts_values[j] = CharGetDatum(KL_OP_DELETE);
			slot->tts_isnull[j] = false;
			assigned++;
		}
	}

	for (i = 0; i < td->natts; i++)
	{
		Form_pg_attribute ta = TupleDescAttr(td, i);
		bool		found = false;

		if (ta->attisdropped)
			continue;
		for (j = 0; j < hd->natts; j++)
		{
			Form_pg_attribute ha = TupleDescAttr(hd, j);

			if (ha->attisdropped ||
				strcmp(NameStr(ha->attname), NameStr(ta->attname)) != 0)
				continue;
			if (ha->atttypid != ta->atttypid || ha->atttypmod != ta->atttypmod)
				ereport(ERROR,
						(errcode(ERRCODE_DATATYPE_MISMATCH),
						 errmsg("column \"%s\" of the history table of \"%s\" has another type",
								NameStr(ta->attname), RelationGetRelationName(rel))));
			slot->tts_values[j] = src_values[i];
			slot->tts_isnull[j] = src_nulls[i];
			assigned++;
			found = true;
			break;
		}
		if (!found)
			ereport(ERROR,
					(errcode(ERRCODE_UNDEFINED_COLUMN),
					 errmsg("the history table of \"%s\" has no column \"%s\"",
							RelationGetRelationName(rel), NameStr(ta->attname))));
	}
	if (assigned != expected)
		ereport(ERROR,
				(errcode(ERRCODE_DATATYPE_MISMATCH),
				 errmsg("the history table of \"%s\" has columns that the table does not have",
						RelationGetRelationName(rel))));

	ExecStoreVirtualTuple(slot);
	table_tuple_insert(hist, slot, GetCurrentCommandId(true), 0, NULL);
	ExecDropSingleTupleTableSlot(slot);
	table_close(hist, NoLock);
	pfree(src_values);
	pfree(src_nulls);
}
/*
 * kingres.ledger_capture() returns trigger
 *		AFTER INSERT ... FOR EACH ROW trigger of a ledger table.  The single
 *		trigger argument is the table_id, written by ledger_enable.  Using the
 *		trigger argument instead of a catalog lookup keeps the per-row cost
 *		low.
 */
Datum
kingres_ledger_capture(PG_FUNCTION_ARGS)
{
	TriggerData *trigdata;
	Trigger    *trigger;
	char	   *endp;
	long		table_id;
	uint8		hash[KL_HASH_LEN];
	uint64		txid;
	int32		seq;
	int32		registered;
	Oid			history;

	if (!CALLED_AS_TRIGGER(fcinfo))
		ereport(ERROR,
				(errcode(ERRCODE_E_R_I_E_TRIGGER_PROTOCOL_VIOLATED),
				 errmsg("ledger_capture must be called as a trigger")));
	trigdata = (TriggerData *) fcinfo->context;
	trigger = trigdata->tg_trigger;

	if (!TRIGGER_FIRED_AFTER(trigdata->tg_event) ||
		!TRIGGER_FIRED_FOR_ROW(trigdata->tg_event))
		ereport(ERROR,
				(errcode(ERRCODE_E_R_I_E_TRIGGER_PROTOCOL_VIOLATED),
				 errmsg("ledger_capture must be an AFTER ... FOR EACH ROW trigger")));
	if (trigger->tgnargs != 1)
		ereport(ERROR,
				(errcode(ERRCODE_E_R_I_E_TRIGGER_PROTOCOL_VIOLATED),
				 errmsg("ledger_capture requires exactly one argument, the table id")));

	errno = 0;
	table_id = strtol(trigger->tgargs[0], &endp, 10);
	if (errno != 0 || *endp != '\0' || endp == trigger->tgargs[0] ||
		table_id < 0 || table_id > PG_INT32_MAX)
		ereport(ERROR,
				(errcode(ERRCODE_E_R_I_E_TRIGGER_PROTOCOL_VIOLATED),
				 errmsg("invalid table id \"%s\" in ledger trigger",
						trigger->tgargs[0])));

	/*
	 * The trigger argument must be the table_id that the registry has for this
	 * relation, for an insert as for an update or a delete: the trigger is the
	 * only thing that says which table the entries are for, and a role that
	 * may create the trigger on a table of its own (the owner of the extension
	 * and the roles that inherit its privileges) must not be able to record
	 * entries under the identity of another table.
	 */
	if (TRIGGER_FIRED_BY_INSERT(trigdata->tg_event))
	{
		/* The registry of 0.1 has no history table: ask for the identity only. */
		history = InvalidOid;
		registered = kl_lookup_table_id(RelationGetRelid(trigdata->tg_relation));
		if (registered < 0)
			ereport(ERROR,
					(errcode(ERRCODE_UNDEFINED_TABLE),
					 errmsg("table \"%s\" is not under ledger control",
							RelationGetRelationName(trigdata->tg_relation))));
	}
	else
		history = kl_lookup_history(RelationGetRelid(trigdata->tg_relation), &registered);
	if (registered != (int32) table_id)
		ereport(ERROR,
				(errcode(ERRCODE_E_R_I_E_TRIGGER_PROTOCOL_VIOLATED),
				 errmsg("ledger trigger of table \"%s\" has table id %ld, the ledger has %d",
						RelationGetRelationName(trigdata->tg_relation),
						table_id, registered)));

	if (TRIGGER_FIRED_BY_INSERT(trigdata->tg_event))
	{
		kl_row_hash_tuple(RelationGetDescr(trigdata->tg_relation),
						  trigdata->tg_trigtuple, (int32) table_id, false, hash);
		kl_record_entry((int32) table_id, KL_OP_INSERT, hash, &txid, &seq);
	}
	else
	{
		/*
		 * UPDATE and DELETE: only an updatable table may see them (an
		 * append-only table has the blocking trigger and a capture trigger
		 * that is AFTER INSERT only; this check is the second line).
		 */
		if (!OidIsValid(history))
			ereport(ERROR,
					(errcode(ERRCODE_KL_APPEND_ONLY),
					 errmsg("table \"%s\" is an append-only ledger table",
							RelationGetRelationName(trigdata->tg_relation))));

		/* The old version of the row leaves: entry D and a history row. */
		kl_row_hash_tuple(RelationGetDescr(trigdata->tg_relation),
						  trigdata->tg_trigtuple, (int32) table_id, false, hash);
		kl_record_entry((int32) table_id, KL_OP_DELETE, hash, &txid, &seq);
		kl_insert_history(history, trigdata->tg_relation, trigdata->tg_trigtuple,
						  txid, seq);

		/* UPDATE: the new version enters. */
		if (TRIGGER_FIRED_BY_UPDATE(trigdata->tg_event))
		{
			kl_row_hash_tuple(RelationGetDescr(trigdata->tg_relation),
							  trigdata->tg_newtuple, (int32) table_id, false, hash);
			kl_record_entry((int32) table_id, KL_OP_INSERT, hash, &txid, &seq);
		}
	}

	return PointerGetDatum(NULL);
}

/*
 * kingres.ledger_block_modify() returns trigger
 *		BEFORE UPDATE OR DELETE ... FOR EACH ROW and BEFORE TRUNCATE trigger
 *		that rejects the operation.  The same function serves both, so the
 *		message does not depend on how the trigger was declared.
 */
Datum
kingres_ledger_block_modify(PG_FUNCTION_ARGS)
{
	TriggerData *trigdata;
	TriggerDesc *tdesc;
	bool		updatable = false;
	int			i;

	if (!CALLED_AS_TRIGGER(fcinfo))
		ereport(ERROR,
				(errcode(ERRCODE_E_R_I_E_TRIGGER_PROTOCOL_VIOLATED),
				 errmsg("ledger_block_modify must be called as a trigger")));
	trigdata = (TriggerData *) fcinfo->context;

	/*
	 * The guard trigger of the ledger tables (kingres_ledger_guard_write):
	 * the tables take an insert, and the queue a delete, only from the code
	 * of the extension (guard.c).  ExecutorCheckPerms_hook refuses the others
	 * before a row is built; this trigger is for the writers that never call
	 * it, the logical replication apply worker and the table synchronisation
	 * worker.
	 */
	if (strcmp(trigdata->tg_trigger->tgname, KL_TRIGGER_GUARD_WRITE) == 0)
	{
		HeapTuple	rettup = trigdata->tg_trigtuple;

		if (TRIGGER_FIRED_BY_INSERT(trigdata->tg_event))
		{
			if (!kl_insert_allowed())
				kl_refuse_write_rel(trigdata->tg_relation, true);
			/*
			 * A superuser's write by hand takes the locks of the writers
			 * first, so that it cannot interleave with them (L15-05), and
			 * leaves a record (README).
			 */
			if (kl_insert_by_hand())
			{
				kl_lock_for_manual_write(trigdata->tg_relation);
				rettup = kl_manual_write_identifier(trigdata->tg_relation, rettup);
			}
			if (kl_insert_is_manual(trigdata->tg_relation))
				kl_record_manual_insert(trigdata->tg_relation, rettup);
		}
		else if (!kl_pending_delete_allowed(trigdata->tg_relation))
			kl_refuse_write_rel(trigdata->tg_relation, false);
		return PointerGetDatum(rettup);
	}

	/*
	 * A superuser who turned allow_manual_write on may delete the rows that
	 * ledger_remove_stray removes (guard.c); nothing else.
	 */
	if (TRIGGER_FIRED_BY_DELETE(trigdata->tg_event) &&
		TRIGGER_FIRED_FOR_ROW(trigdata->tg_event) &&
		kl_manual_delete_allowed(trigdata->tg_relation))
		return PointerGetDatum(trigdata->tg_trigtuple);

	/* INSERT: the only other trigger that fires on an insert is the one of a history table. */
	if (TRIGGER_FIRED_BY_INSERT(trigdata->tg_event))
		ereport(ERROR,
				(errcode(ERRCODE_KL_APPEND_ONLY),
				 errmsg("table \"%s\" is a history table of the ledger; only the ledger writes it",
						RelationGetRelationName(trigdata->tg_relation))));

	/* TRUNCATE of an updatable table: say what the table is. */
	tdesc = trigdata->tg_relation->trigdesc;
	for (i = 0; tdesc != NULL && i < tdesc->numtriggers; i++)
	{
		Trigger    *t = &tdesc->triggers[i];

		if (strcmp(t->tgname, KL_TRIGGER_CAPTURE) == 0 &&
			(t->tgtype & TRIGGER_TYPE_UPDATE) != 0)
			updatable = true;
	}
	if (updatable)
		ereport(ERROR,
				(errcode(ERRCODE_KL_APPEND_ONLY),
				 errmsg("table \"%s\" is an updatable ledger table and cannot be truncated",
						RelationGetRelationName(trigdata->tg_relation))));

	ereport(ERROR,
			(errcode(ERRCODE_KL_APPEND_ONLY),
			 errmsg("table \"%s\" is an append-only ledger table",
					RelationGetRelationName(trigdata->tg_relation))));
	PG_RETURN_NULL();			/* not reached */
}

/*
 * kingres.ledger_record_row(table_id int, row_data record) returns void
 *		Only for the SQL of versions 0.1 and 0.2, whose ledger_enable calls it
 *		(the update to 0.3 drops it, and ledger_enable then enters the rows
 *		through kingres.ledger_register_table).  It takes a table_id and a row,
 *		so it would write entries of its caller's choice, and it is therefore
 *		refused unless the caller is a superuser acting as itself with
 *		kingres_ledger.allow_manual_write on: the one thing that may write the
 *		ledger by hand.  A role that is not a superuser cannot call it in any
 *		case (EXECUTE is the owner's alone, and the protection hook refuses
 *		every GRANT on it).
 */
Datum
kingres_ledger_record_row(PG_FUNCTION_ARGS)
{
	int32		table_id = PG_GETARG_INT32(0);
	HeapTupleHeader header = PG_GETARG_HEAPTUPLEHEADER(1);

	if (!kl_manual_write_ok())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("kingres.ledger_record_row belongs to the SQL of versions 0.1 and 0.2 of extension \"%s\" and enters the row it is given",
						KL_EXTENSION_NAME),
				 errhint("Run ALTER EXTENSION %s UPDATE. A superuser can call it only with kingres_ledger.allow_manual_write on.",
						 KL_EXTENSION_NAME)));
	kl_record_existing_row(table_id, header);
	PG_RETURN_VOID();
}

/*
 * kl_record_existing_row
 *		Enter one existing row of a ledger table into the ledger.  Called by
 *		kingres_ledger_register_table (write.c) for the rows a table already
 *		contains when it is put under ledger control; it goes through the same
 *		code as the insert trigger so both share the sequence counter of the
 *		transaction.  No SQL function does this: a function that took a row
 *		and a table_id as arguments would write entries of the caller's choice.
 *		The row type must be the row type of the table that has this table_id.
 */
void
kl_record_existing_row(int32 table_id, HeapTupleHeader header)
{
	uint8		hash[KL_HASH_LEN];
	uint64		txid;
	int32		seq;

	if (kl_table_id_for_type(HeapTupleHeaderGetTypeId(header)) != table_id)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("row does not belong to ledger table %d", table_id)));

	kl_row_hash_composite(header, table_id, false, hash);
	kl_record_entry(table_id, KL_OP_INSERT, hash, &txid, &seq);
}
