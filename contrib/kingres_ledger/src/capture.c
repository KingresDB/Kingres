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
 * SPDX-License-Identifier: PostgreSQL
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/capture.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "access/xact.h"
#include "catalog/pg_type.h"
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
		case XACT_EVENT_ABORT:
		case XACT_EVENT_PREPARE:
			kl_state.txid = 0;
			kl_state.next_seq = 0;
			kl_state.pending_written = false;
			kl_state.pending_subid = InvalidSubTransactionId;
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
 * kl_record_entry
 *		Append one entry for the current transaction.
 *
 * Writes the ledger_pending row when this is the first live entry of the
 * transaction, then the ledger_entry row.  The user name recorded is the
 * session's current user as seen outside this SECURITY DEFINER function.
 */
static void
kl_record_entry(int32 table_id, const uint8 *row_hash)
{
	uint64		txid = U64FromFullTransactionId(GetTopFullTransactionId());
	Datum		values[4];
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
		rc = SPI_execute_plan(kl_pending_plan, values, NULL, false, 0);
		if (rc != SPI_OK_INSERT)
			elog(ERROR, "could not queue ledger transaction: %s",
				 SPI_result_code_string(rc));
		kl_state.pending_written = true;
		kl_state.pending_subid = GetCurrentSubTransactionId();
	}

	if (kl_entry_plan == NULL)
	{
		Oid			argtypes[4] = {INT8OID, INT4OID, INT4OID, BYTEAOID};

		kl_entry_plan = kl_prepare_plan("INSERT INTO kingres.ledger_entry "
										"(ledger_txid, seq, table_id, op, row_hash) "
										"VALUES ($1, $2, $3, 'I', $4)",
										4, argtypes);
	}
	hash_datum = (bytea *) palloc(VARHDRSZ + KL_HASH_LEN);
	SET_VARSIZE(hash_datum, VARHDRSZ + KL_HASH_LEN);
	memcpy(VARDATA(hash_datum), row_hash, KL_HASH_LEN);

	values[0] = Int64GetDatum((int64) txid);
	values[1] = Int32GetDatum(kl_state.next_seq);
	values[2] = Int32GetDatum(table_id);
	values[3] = PointerGetDatum(hash_datum);
	rc = SPI_execute_plan(kl_entry_plan, values, NULL, false, 0);
	if (rc != SPI_OK_INSERT)
		elog(ERROR, "could not write ledger entry: %s",
			 SPI_result_code_string(rc));
	kl_state.next_seq++;

	/* hash_datum lives in the SPI procedure context, freed here. */
	SPI_finish();
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

	if (!CALLED_AS_TRIGGER(fcinfo))
		ereport(ERROR,
				(errcode(ERRCODE_E_R_I_E_TRIGGER_PROTOCOL_VIOLATED),
				 errmsg("ledger_capture must be called as a trigger")));
	trigdata = (TriggerData *) fcinfo->context;
	trigger = trigdata->tg_trigger;

	if (!TRIGGER_FIRED_AFTER(trigdata->tg_event) ||
		!TRIGGER_FIRED_FOR_ROW(trigdata->tg_event) ||
		!TRIGGER_FIRED_BY_INSERT(trigdata->tg_event))
		ereport(ERROR,
				(errcode(ERRCODE_E_R_I_E_TRIGGER_PROTOCOL_VIOLATED),
				 errmsg("ledger_capture must be an AFTER INSERT FOR EACH ROW trigger")));
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

	kl_row_hash_tuple(RelationGetDescr(trigdata->tg_relation),
					  trigdata->tg_trigtuple, (int32) table_id, hash);
	kl_record_entry((int32) table_id, hash);

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

	if (!CALLED_AS_TRIGGER(fcinfo))
		ereport(ERROR,
				(errcode(ERRCODE_E_R_I_E_TRIGGER_PROTOCOL_VIOLATED),
				 errmsg("ledger_block_modify must be called as a trigger")));
	trigdata = (TriggerData *) fcinfo->context;

	ereport(ERROR,
			(errcode(ERRCODE_KL_APPEND_ONLY),
			 errmsg("table \"%s\" is an append-only ledger table",
					RelationGetRelationName(trigdata->tg_relation))));
	PG_RETURN_NULL();			/* not reached */
}

/*
 * kingres.ledger_record_row(table_id int, row record) returns void
 *		Enter one existing row into the ledger.  Used by ledger_enable for
 *		the rows a table already contains when it is put under ledger
 *		control; it goes through the same code as the insert trigger so both
 *		share the sequence counter of the transaction.
 *
 * SECURITY DEFINER and not executable by PUBLIC: it writes the ledger.  It
 * refuses a table_id that does not belong to the row's type, so even a
 * caller with access cannot record a row under another table's identity.
 */
Datum
kingres_ledger_record_row(PG_FUNCTION_ARGS)
{
	int32		table_id = PG_GETARG_INT32(0);
	HeapTupleHeader header = PG_GETARG_HEAPTUPLEHEADER(1);
	uint8		hash[KL_HASH_LEN];

	if (kl_table_id_for_type(HeapTupleHeaderGetTypeId(header)) != table_id)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("row does not belong to ledger table %d", table_id)));

	kl_row_hash_composite(header, table_id, hash);
	kl_record_entry(table_id, hash);
	PG_RETURN_VOID();
}
