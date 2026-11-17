/*-------------------------------------------------------------------------
 *
 * write.c
 *	  The functions that write the ledger tables, and the record of a write
 *	  by hand.
 *
 * Every row of the nine ledger catalog tables is written by this file, by
 * capture.c, or by the script of the extension.  The rule that makes the
 * guard (guard.c) sound is that no SQL-callable function takes the content
 * of a write as an argument: each function below reads the ledger and
 * computes every value it writes, and the statements that write are fixed
 * text with numbered parameters, run with the marker raised (kl_run).  A role
 * that may call these functions, whatever it inherits or owns, can only make
 * them do what they are documented to do:
 *
 *	  ledger_build_block()       close a block from the queue
 *	  ledger_generate_digest()   build a block if needed, record the digest
 *	  ledger_verify_recorded()   run ledger_verify and record the run
 *	  ledger_register_table()    register a table that ledger_enable set up
 *							 and enter the rows it already holds
 *	  ledger_remove_stray()      a superuser's act: needs the setting
 *
 * The block builders and ledger_remove_stray are serialised by a lock on the
 * extension's own object with a sub-object number that no SQL statement
 * uses (KL_BUILDER_LOCK_SUBID), and not by a table lock: a lock on a ledger
 * table can be requested by a role that has MAINTAIN or the right to write
 * every table, and a role that holds one must not be able to stop the
 * builder.
 *
 * The identifiers of the rows that ledger_generate_digest,
 * ledger_verify_recorded, ledger_remove_stray, a manual write and
 * ledger_enable write are taken by these functions, under a lock, and not from
 * the sequences of the identity columns (see kl_follow_sequence below).
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/write.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "access/table.h"
#include "access/transam.h"
#include "access/xact.h"
#include "catalog/namespace.h"
#include "catalog/pg_class.h"
#include "catalog/pg_extension.h"
#include "catalog/pg_trigger.h"
#include "catalog/pg_type.h"
#include "commands/extension.h"
#include "commands/trigger.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "storage/latch.h"
#include "storage/lmgr.h"
#include "utils/acl.h"
#include "utils/array.h"
#include "utils/builtins.h"
#include "utils/datum.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/rel.h"
#include "utils/ruleutils.h"
#include "utils/snapmgr.h"
#include "utils/timeout.h"
#include "utils/timestamp.h"

#include "kingres_ledger.h"

PG_FUNCTION_INFO_V1(kingres_ledger_build_block);
PG_FUNCTION_INFO_V1(kingres_ledger_generate_digest);
PG_FUNCTION_INFO_V1(kingres_ledger_verify_recorded);
PG_FUNCTION_INFO_V1(kingres_ledger_remove_stray);
PG_FUNCTION_INFO_V1(kingres_ledger_register_table);

/*
 * kl_run
 *		Run one statement of fixed text with numbered parameters in the SPI
 *		connection of the caller.  A "guarded" statement is one that writes a
 *		ledger table: it runs with the write marker raised (guard.c), which is
 *		lowered on every way out.  What can run while it is raised: the
 *		statement and nothing else.  The tables it writes have no trigger but
 *		the blocking ones (which raise errors), no policy, no rule and no
 *		default or CHECK expression other than constants, now() and
 *		clock_timestamp(); their indexes have no expression; the types of their
 *		columns are built in; the foreign key checks are system triggers.  The
 *		functions the statements name are pg_catalog's and the extension's,
 *		qualified with the schema, and the calling functions have the
 *		search_path pg_catalog, pg_temp, so nothing a role created is found.
 *		No argument is text that is run.
 */
static int
kl_run(const char *sql, int nargs, const Oid *argtypes, Datum *values,
	   const char *nulls, bool guarded)
{
	int			rc = 0;

	if (guarded)
		kl_write_enter();
	PG_TRY();
	{
		rc = SPI_execute_with_args(sql, nargs, (Oid *) argtypes, values, nulls,
								   false, 0);
	}
	PG_FINALLY();
	{
		if (guarded)
			kl_write_leave();
	}
	PG_END_TRY();
	if (rc < 0)
		elog(ERROR, "ledger statement failed: %s", SPI_result_code_string(rc));
	return rc;
}

/* Column "col" (1-based) of the first row of the last result; false if none or NULL. */
static bool
kl_first(int col, Datum *value)
{
	bool		isnull;

	if (SPI_processed < 1 || SPI_tuptable == NULL)
		return false;
	*value = SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, col,
						   &isnull);
	return !isnull;
}

/*
 * kl_refuse_open_transaction
 *		The lock that serialises the builders is held until the end of the
 *		transaction of the caller, and it has to be: a builder that started
 *		while another one's block was uncommitted would wait on that block's
 *		rows, or build the same height.  A role that may call a builder (an
 *		auditor, the agent of the README) could therefore stop the worker, and
 *		every other builder, for as long as it keeps a transaction open after
 *		the call (L11-03).  A role that is not a superuser may call the
 *		builders only as a statement of its own, so that the lock is held for
 *		the time of that statement and no longer: not inside BEGIN ... COMMIT,
 *		not in a multi-command string (an implicit block), not in a
 *		subtransaction (a savepoint, and also the EXCEPTION block of a PL/pgSQL
 *		function, which looks the same from here: a function that calls a
 *		builder for a role that is not a superuser keeps the call outside such a
 *		block), and not after the first Execute of a pipeline (the flag of the
 *		pipeline reports the rest).
 *		What remains is a statement that runs long, and the first Execute of an
 *		extended-protocol pipeline that the client does not Sync: the server
 *		cannot see either when the call is made, and the settings that look
 *		like a remedy (statement_timeout, idle_in_transaction_session_timeout,
 *		transaction_timeout) are the role's own to change, and do not apply to
 *		a pipeline that is waiting for its Sync (the backend is "active").  The
 *		bound is the extension's own: kl_hold_arm (below) ends the session of a
 *		role that is not a superuser when it holds a lock of the extension
 *		for longer than kingres_ledger.lock_hold_limit, which only a superuser
 *		can change.
 *		A superuser can stop the server anyway, and the workers are superusers.
 */
static void
kl_refuse_open_transaction(const char *fname)
{
	if (superuser_arg(GetOuterUserId()))
		return;
	if (IsTransactionBlock() || (MyXactFlags & XACT_FLAGS_PIPELINING) != 0)
		ereport(ERROR,
				(errcode(ERRCODE_ACTIVE_SQL_TRANSACTION),
				 errmsg("%s cannot run inside a transaction block for a role that is not a superuser",
						fname),
				 errdetail("It holds the lock of the block builders until the end of the transaction, and the block builder of the server would wait for it."),
				 errhint("Call it as a statement of its own, outside BEGIN ... COMMIT, a savepoint and an EXCEPTION block.")));
}

/*
 * The bound on how long a role that is not a superuser holds a lock of the
 * extension (L13-02).  The locks are held to the end of the transaction of the
 * caller, and have to be (see kl_refuse_open_transaction): a block that is not
 * committed must not be invisible to the next builder.  What a caller can do
 * with that, without being a superuser: a pipeline whose client never sends
 * Sync keeps the transaction open with the backend "active", where no timeout
 * of PostgreSQL reaches it; a statement that sleeps after the call runs as
 * long as the role lets it, because every timeout of PostgreSQL that bounds a
 * statement or a transaction can be set to 0 by the role for its own session.
 * So the bound is a timer of our own, armed when a role that is not a
 * superuser has the lock, switched off when the transaction ends, and
 * unreachable from SQL (the limit is a superuser setting).  When it fires, the
 * session ends as if an administrator had terminated it: the transaction rolls
 * back and the lock goes with it.  It is raised again by whatever makes
 * progress (kl_hold_progress: ledger_enable entering the rows of a large
 * table), so that honest work that takes long is not ended, and a call that
 * only waits is.
 */
static int	kl_hold_timeout_id = -1;
static bool kl_hold_armed = false;
static bool kl_hold_callback_registered = false;

/* In the signal handler: end the session the way a terminate request does. */
static void
kl_hold_timeout_handler(void)
{
	ProcDiePending = true;
	InterruptPending = true;
	SetLatch(MyLatch);
}

static void
kl_hold_xact_callback(XactEvent event, void *arg)
{
	switch (event)
	{
		case XACT_EVENT_COMMIT:
		case XACT_EVENT_ABORT:
		case XACT_EVENT_PREPARE:
		case XACT_EVENT_PARALLEL_COMMIT:
		case XACT_EVENT_PARALLEL_ABORT:
			if (kl_hold_armed)
			{
				disable_timeout(kl_hold_timeout_id, false);
				kl_hold_armed = false;
			}
			break;
		default:
			break;
	}
}

static void
kl_hold_arm(void)
{
	if (kl_lock_hold_limit <= 0 || superuser_arg(GetOuterUserId()))
		return;
	if (!kl_hold_callback_registered)
	{
		RegisterXactCallback(kl_hold_xact_callback, NULL);
		kl_hold_callback_registered = true;
	}
	if (kl_hold_timeout_id < 0)
		kl_hold_timeout_id = RegisterTimeout(USER_TIMEOUT, kl_hold_timeout_handler);
	if (!kl_hold_armed)
	{
		enable_timeout_after(kl_hold_timeout_id, kl_lock_hold_limit);
		kl_hold_armed = true;
	}
}

/* Work that makes progress while a lock is held gets a new full period. */
void
kl_hold_progress(void)
{
	if (kl_hold_armed && kl_lock_hold_limit > 0)
		enable_timeout_after(kl_hold_timeout_id, kl_lock_hold_limit);
}

/*
 * Wait for a lock on the extension's own object with a sub-object number that
 * no statement uses (the builders', the registry's, the verifications'; see
 * kingres_ledger.h) and, for a caller that is not a superuser, start the
 * clock of kingres_ledger.lock_hold_limit.  The time spent waiting for the
 * lock is not counted.
 */
void
kl_lock_extension_object(int subid)
{
	LockDatabaseObject(ExtensionRelationId,
					   get_extension_oid(KL_EXTENSION_NAME, false),
					   subid, ExclusiveLock);
	kl_hold_arm();
}

/* Wait for the lock that serialises the block builders (see the file header). */
static void
kl_lock_builders(void)
{
	kl_lock_extension_object(KL_BUILDER_LOCK_SUBID);
}

/*
 * The identifiers of the rows that these functions write are not taken from the
 * sequences of the identity columns.  A role that holds nextval and setval on
 * a sequence (every member of pg_write_all_data does, with no grant to
 * revoke) could move it to the maximum, and no hook sees a call of setval:
 * every writer would then fail for everybody until a superuser repaired it.
 * Each writer takes the next identifier itself, as the maximum of the rows
 * there are plus one, in the statement that inserts the row and under the lock
 * that already serialises the writers of that table (the builders' lock for
 * digests, one of the locks above for the rest), so the value cannot collide
 * and a forged sequence changes nothing.  The columns stay identity columns
 * for the rows that a superuser inserts by hand (README), and each writer sets
 * the sequence to follow the identifier it took (kl_follow_sequence), which
 * also heals a sequence that was moved; no writer reads it.
 */
#define KL_SQL_NEXT_DIGEST_ID \
	"(SELECT GREATEST(COALESCE(pg_catalog.max(x.digest_id), 0), " \
	"COALESCE((SELECT pg_catalog.max((r.row_data OPERATOR(pg_catalog.->>) 'digest_id')::pg_catalog.int8) " \
	"FROM kingres.ledger_removed r " \
	"WHERE r.source_table OPERATOR(pg_catalog.=) 'ledger_digest' " \
	"AND r.action OPERATOR(pg_catalog.=) 'removed'), 0)) + 1 " \
	"FROM kingres.ledger_digest x)"

#define KL_SQL_NEXT_VERIFICATION_ID \
	"(SELECT COALESCE(pg_catalog.max(x.verification_id), 0) + 1 FROM kingres.ledger_verification x)"

/* Set the sequence of an identity column to the identifier that was taken. */
static void
kl_follow_sequence(const char *table, const char *column, int64 id)
{
	Oid			types[3];
	Datum		values[3];

	types[0] = TEXTOID;
	types[1] = TEXTOID;
	types[2] = INT8OID;
	values[0] = CStringGetTextDatum(table);
	values[1] = CStringGetTextDatum(column);
	values[2] = Int64GetDatum(id);
	kl_run("SELECT pg_catalog.setval("
		   "pg_catalog.pg_get_serial_sequence($1, $2)::pg_catalog.regclass, $3)",
		   3, types, values, NULL, false);
}

static char *
kl_ts_text(TimestampTz t)
{
	return DatumGetCString(DirectFunctionCall1(timestamptz_out,
											   TimestampTzGetDatum(t)));
}

static void
kl_outer_user_name(NameData *name)
{
	namestrcpy(name, GetUserNameFromId(GetOuterUserId(), false));
}

/*
 * The statements of a block.  Written in the order the builder runs them.
 */
#define KL_SQL_TX_ROWS \
	"INSERT INTO kingres.ledger_tx " \
	"(ledger_txid, block_height, tx_index, entry_count, merkle_root, user_name) " \
	"SELECT p.ledger_txid, $1, " \
	"(pg_catalog.row_number() OVER (ORDER BY p.ledger_txid) - 1)::pg_catalog.int4, " \
	"r.cnt, r.root, p.user_name " \
	"FROM kingres.ledger_pending p " \
	"CROSS JOIN LATERAL (" \
	"SELECT pg_catalog.count(*)::pg_catalog.int4 AS cnt, " \
	"kingres.ledger_merkle_root(" \
	"kingres.ledger_tx_leaf(e.seq, e.table_id, e.op, e.row_hash) ORDER BY e.seq) AS root " \
	"FROM kingres.ledger_entry e WHERE e.ledger_txid = p.ledger_txid) r " \
	"WHERE p.ledger_txid = ANY ($2)"

#define KL_SQL_BLOCK \
	"INSERT INTO kingres.ledger_blocks " \
	"(height, hash_version, prev_hash, tx_merkle_root, tx_count, closed_at, block_hash) " \
	"VALUES ($1, 1, $2, $3, $4, $5, $6)"

#define KL_SQL_QUEUE_END \
	"DELETE FROM kingres.ledger_pending WHERE ledger_txid = ANY ($1)"

/*
 * kl_build_block_core
 *		Close a block from the queue.  The SPI connection is the caller's.
 *		Returns false when nothing was pending.
 */
static bool
kl_build_block_core(int64 *height_out)
{
	Datum		values[6];
	Oid			types[6];
	Datum		d;
	ArrayType  *txids;
	bytea	   *chain;
	bytea	   *prev;
	bytea	   *txroot;
	bytea	   *bh;
	int64		h;
	int32		n;
	bool		have_prev = false;
	TimestampTz prev_at = 0;
	TimestampTz closed;

	if (XactIsoLevel != XACT_READ_COMMITTED)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("ledger_build_block requires the READ COMMITTED isolation level")));

	kl_lock_builders();

	/* Pending transactions are committed ones: the pending row became visible with its entries. */
	types[0] = INT8OID;
	values[0] = Int64GetDatum((int64) kl_max_block_transactions);
	kl_run("SELECT pg_catalog.array_agg(p.ledger_txid ORDER BY p.ledger_txid) "
		   "FROM (SELECT lp.ledger_txid FROM kingres.ledger_pending lp "
		   "ORDER BY lp.ledger_txid LIMIT $1) p",
		   1, types, values, NULL, false);
	if (!kl_first(1, &d))
		return false;
	txids = (ArrayType *) DatumGetPointer(datumCopy(d, false, -1));

	kl_run("SELECT c.chain_id FROM kingres.ledger_chain c",
		   0, NULL, NULL, NULL, false);
	if (!kl_first(1, &d))
		ereport(ERROR,
				(errcode(ERRCODE_DATA_CORRUPTED),
				 errmsg("the identity of the chain is missing"),
				 errhint("Run kingres.ledger_verify().")));
	chain = (bytea *) DatumGetPointer(datumCopy(d, false, -1));

	kl_run("SELECT b.height + 1, b.block_hash, b.closed_at "
		   "FROM kingres.ledger_blocks b ORDER BY b.height DESC LIMIT 1",
		   0, NULL, NULL, NULL, false);
	if (SPI_processed == 1)
	{
		bool		isnull;
		HeapTuple	row = SPI_tuptable->vals[0];
		TupleDesc	td = SPI_tuptable->tupdesc;

		h = DatumGetInt64(SPI_getbinval(row, td, 1, &isnull));
		prev = (bytea *) DatumGetPointer(datumCopy(SPI_getbinval(row, td, 2, &isnull), false, -1));
		prev_at = DatumGetTimestampTz(SPI_getbinval(row, td, 3, &isnull));
		have_prev = true;
	}
	else
	{
		h = 0;
		types[0] = BYTEAOID;
		values[0] = PointerGetDatum(chain);
		kl_run("SELECT kingres.ledger_genesis_hash($1)", 1, types, values, NULL, false);
		if (!kl_first(1, &d))
			elog(ERROR, "no genesis hash");
		prev = (bytea *) DatumGetPointer(datumCopy(d, false, -1));
	}

	/* The transaction rows of the block. */
	types[0] = INT8OID;
	types[1] = INT8ARRAYOID;
	values[0] = Int64GetDatum(h);
	values[1] = PointerGetDatum(txids);
	kl_run(KL_SQL_TX_ROWS, 2, types, values, NULL, true);

	types[0] = INT8OID;
	values[0] = Int64GetDatum(h);
	kl_run("SELECT pg_catalog.count(*)::pg_catalog.int4, "
		   "kingres.ledger_merkle_root("
		   "kingres.ledger_block_tx_leaf(t.ledger_txid, t.entry_count, t.merkle_root, t.user_name) "
		   "ORDER BY t.tx_index) "
		   "FROM kingres.ledger_tx t WHERE t.block_height = $1",
		   1, types, values, NULL, false);
	if (!kl_first(1, &d))
		elog(ERROR, "no transaction rows for block " INT64_FORMAT, h);
	n = DatumGetInt32(d);
	if (!kl_first(2, &d))
		elog(ERROR, "no transaction root for block " INT64_FORMAT, h);
	txroot = (bytea *) DatumGetPointer(datumCopy(d, false, -1));

	/*
	 * The time of a block is the server's clock, but it never goes back (the
	 * verifier and ledger_verify require it): a clock that was set back a
	 * little, or a standby promoted with a clock that is behind, gives the
	 * time of the previous block.  A previous block that is far ahead of the
	 * clock is not that: every block after it would carry its time, for ever.
	 * The builder refuses (nothing is lost, the transactions stay pending)
	 * until a superuser who has looked at the clock sets
	 * kingres_ledger.max_clock_skew to 0; ledger_verify keeps reporting the
	 * block that is ahead of the clock whatever the setting is.
	 */
	closed = GetCurrentTimestamp();
	if (have_prev && closed < prev_at)
	{
		if (kl_max_clock_skew > 0 &&
			prev_at - closed > (int64) kl_max_clock_skew * 1000)
			ereport(ERROR,
					(errcode(ERRCODE_KL_CLOCK_SKEW),
					 errmsg("block " INT64_FORMAT " was closed at %s, which is ahead of the clock of this server by more than kingres_ledger.max_clock_skew",
							h - 1, kl_ts_text(prev_at)),
					 errdetail("The clock of this server reads %s.", kl_ts_text(closed)),
					 errhint("Check the clock of the server. Every later block would carry the time of that block. A superuser who has checked it can set kingres_ledger.max_clock_skew to 0; ledger_verify keeps reporting the block.")));
		closed = prev_at;
	}

	types[0] = INT8OID;
	types[1] = BYTEAOID;
	types[2] = BYTEAOID;
	types[3] = INT4OID;
	types[4] = TIMESTAMPTZOID;
	values[0] = Int64GetDatum(h);
	values[1] = PointerGetDatum(prev);
	values[2] = PointerGetDatum(txroot);
	values[3] = Int32GetDatum(n);
	values[4] = TimestampTzGetDatum(closed);
	kl_run("SELECT kingres.ledger_block_hash(1::pg_catalog.int2, $1, $2, $3, $4, $5)",
		   5, types, values, NULL, false);
	if (!kl_first(1, &d))
		elog(ERROR, "no block hash for block " INT64_FORMAT, h);
	bh = (bytea *) DatumGetPointer(datumCopy(d, false, -1));

	types[5] = BYTEAOID;
	values[5] = PointerGetDatum(bh);
	kl_run(KL_SQL_BLOCK, 6, types, values, NULL, true);

	types[0] = INT8ARRAYOID;
	values[0] = PointerGetDatum(txids);
	kl_run(KL_SQL_QUEUE_END, 1, types, values, NULL, true);

	*height_out = h;
	return true;
}

/*
 * kingres.ledger_build_block() returns bigint
 *		Close a block from the pending transactions; returns the new height,
 *		or NULL when nothing was pending.  Only one builder runs at a time
 *		(see the file header).  READ COMMITTED is required: the builder must
 *		see the block written by the previous builder, which a REPEATABLE READ
 *		snapshot taken earlier would not.  SECURITY DEFINER, with a fixed
 *		search_path, because it writes the ledger tables, which no other role
 *		may write.
 */
Datum
kingres_ledger_build_block(PG_FUNCTION_ARGS)
{
	int64		h = 0;
	bool		built;

	kl_refuse_open_transaction("ledger_build_block");
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	built = kl_build_block_core(&h);
	SPI_finish();
	if (!built)
		PG_RETURN_NULL();
	PG_RETURN_INT64(h);
}

/*
 * kingres.ledger_generate_digest() returns jsonb
 *		Close a block if needed, record the digest of the latest block and
 *		return it; NULL when the ledger is empty.  One statement builds the
 *		digest from the ledger and records it.
 */
Datum
kingres_ledger_generate_digest(PG_FUNCTION_ARGS)
{
	int64		h;
	Datum		d;
	Datum		result = (Datum) 0;
	bool		have = false;

	kl_refuse_open_transaction("ledger_generate_digest");
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	(void) kl_build_block_core(&h);

	/*
	 * A convenience record, not a root of trust.  The identifier is taken
	 * under the lock of the builders, which kl_build_block_core holds.
	 */
	kl_run("INSERT INTO kingres.ledger_digest "
		   "(digest_id, generated_at, generated_by, height, block_hash, digest) "
		   "OVERRIDING SYSTEM VALUE "
		   "SELECT " KL_SQL_NEXT_DIGEST_ID ", g.ts, kingres.ledger_outer_user(), b.height, b.block_hash, "
		   "pg_catalog.jsonb_build_object("
		   "'database', pg_catalog.current_database(), "
		   "'chain_id', pg_catalog.encode(c.chain_id, 'hex'), "
		   "'hash_version', b.hash_version, "
		   "'height', b.height, "
		   "'block_hash', pg_catalog.encode(b.block_hash, 'hex'), "
		   "'block_closed_at', pg_catalog.to_char(b.closed_at AT TIME ZONE 'UTC', "
		   "'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'), "
		   "'generated_at', pg_catalog.to_char(g.ts AT TIME ZONE 'UTC', "
		   "'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')) "
		   "FROM (SELECT pg_catalog.clock_timestamp() AS ts) g "
		   "CROSS JOIN (SELECT lb.* FROM kingres.ledger_blocks lb "
		   "ORDER BY lb.height DESC LIMIT 1) b "
		   "LEFT JOIN kingres.ledger_chain c ON true "
		   "RETURNING digest, digest_id",
		   0, NULL, NULL, NULL, true);
	if (kl_first(1, &d))
	{
		Datum		idd;

		result = SPI_datumTransfer(d, false, -1);
		have = true;
		if (kl_first(2, &idd))
			kl_follow_sequence("kingres.ledger_digest", "digest_id",
							   DatumGetInt64(idd));
	}
	SPI_finish();
	if (!have)
		PG_RETURN_NULL();
	PG_RETURN_DATUM(result);
}

/*
 * kingres.ledger_verify_recorded(from_height bigint, to_height bigint) returns bigint
 *		Run ledger_verify and record the run in kingres.ledger_verification;
 *		returns the id of the record.  The record is written when the run is
 *		over: PostgreSQL has no autonomous transaction, so a "running" row that
 *		survives a failure of the caller's transaction cannot be written.  A
 *		failure of ledger_verify itself (a cancel included) is caught in a
 *		subtransaction and recorded with the state 'failed' and the error
 *		message; the function then returns normally and warns.  If the
 *		caller's own transaction rolls back, the record is gone with it.
 *		blocks_checked is the number of blocks in the range, rows_checked the
 *		number of ledger entries of the transactions in those blocks; the
 *		problems are the rows of ledger_verify, at most 100 of them (the count
 *		is exact).  started_by is the role that called it.  SECURITY DEFINER
 *		because it writes kingres.ledger_verification, which no other role may
 *		write; the caller needs EXECUTE only, which PUBLIC does not have.
 */
Datum
kingres_ledger_verify_recorded(PG_FUNCTION_ARGS)
{
	MemoryContext oldcxt = CurrentMemoryContext;
	ResourceOwner oldowner = CurrentResourceOwner;
	TimestampTz started = GetCurrentTimestamp();
	NameData	who;
	int64		lo = PG_ARGISNULL(0) ? 0 : PG_GETARG_INT64(0);
	int64		hi = 0;
	bool		to_null = PG_ARGISNULL(1);
	int64		to_arg = to_null ? 0 : PG_GETARG_INT64(1);
	Datum		values[9];
	Oid			types[9];
	char		nulls[9];
	Datum		d;
	ErrorData  *edata = NULL;
	volatile int64 vid = 0;

	kl_outer_user_name(&who);

	if (lo < 0 || (!to_null && to_arg < lo))
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("invalid height range %s .. %s",
						PG_ARGISNULL(0) ? "<NULL>" : psprintf(INT64_FORMAT, lo),
						to_null ? "<NULL>" : psprintf(INT64_FORMAT, to_arg))));

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	types[0] = INT8OID;
	values[0] = Int64GetDatum(to_arg);
	nulls[0] = to_null ? 'n' : ' ';
	kl_run("SELECT COALESCE($1, pg_catalog.max(b.height), -1) FROM kingres.ledger_blocks b",
		   1, types, values, nulls, false);
	if (!kl_first(1, &d))
		elog(ERROR, "no height range");
	hi = DatumGetInt64(d);

	BeginInternalSubTransaction(NULL);
	MemoryContextSwitchTo(oldcxt);
	PG_TRY();
	{
		int32		n_problems;
		int64		n_blocks;
		int64		n_rows;
		Datum		probs = (Datum) 0;
		bool		have_probs;

		types[0] = INT8OID;
		types[1] = INT8OID;
		values[0] = PG_ARGISNULL(0) ? Int64GetDatum(0) : PG_GETARG_DATUM(0);
		nulls[0] = PG_ARGISNULL(0) ? 'n' : ' ';
		values[1] = Int64GetDatum(to_arg);
		nulls[1] = to_null ? 'n' : ' ';
		kl_run("SELECT pg_catalog.count(*)::pg_catalog.int4, "
			   "COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object("
			   "'check', p.check_name, 'object', p.object, 'detail', p.detail)) "
			   "FILTER (WHERE p.rn <= 100), '[]'::pg_catalog.jsonb) "
			   "FROM (SELECT v.check_name, v.object, v.detail, "
			   "pg_catalog.row_number() OVER () AS rn "
			   "FROM kingres.ledger_verify($1, $2) v) p",
			   2, types, values, nulls, false);
		if (!kl_first(1, &d))
			elog(ERROR, "no verification result");
		n_problems = DatumGetInt32(d);
		have_probs = kl_first(2, &d);
		if (have_probs)
			probs = datumCopy(d, false, -1);

		types[0] = INT8OID;
		types[1] = INT8OID;
		values[0] = Int64GetDatum(lo);
		values[1] = Int64GetDatum(hi);
		kl_run("SELECT pg_catalog.count(*) FROM kingres.ledger_blocks b "
			   "WHERE b.height BETWEEN $1 AND $2",
			   2, types, values, NULL, false);
		if (!kl_first(1, &d))
			elog(ERROR, "no block count");
		n_blocks = DatumGetInt64(d);
		kl_run("SELECT COALESCE(pg_catalog.sum(t.entry_count), 0)::pg_catalog.int8 "
			   "FROM kingres.ledger_tx t WHERE t.block_height BETWEEN $1 AND $2",
			   2, types, values, NULL, false);
		if (!kl_first(1, &d))
			elog(ERROR, "no row count");
		n_rows = DatumGetInt64(d);

		types[0] = TIMESTAMPTZOID;
		types[1] = NAMEOID;
		types[2] = INT8OID;
		types[3] = INT8OID;
		types[4] = TEXTOID;
		types[5] = INT4OID;
		types[6] = INT8OID;
		types[7] = INT8OID;
		types[8] = JSONBOID;
		values[0] = TimestampTzGetDatum(started);
		values[1] = NameGetDatum(&who);
		values[2] = Int64GetDatum(lo);
		values[3] = Int64GetDatum(hi);
		values[4] = CStringGetTextDatum(n_problems == 0 ? "clean" : "problems");
		values[5] = Int32GetDatum(n_problems);
		values[6] = Int64GetDatum(n_blocks);
		values[7] = Int64GetDatum(n_rows);
		values[8] = probs;
		memset(nulls, ' ', sizeof(nulls));
		if (n_problems == 0 || !have_probs)
			nulls[8] = 'n';
		/* The identifier is the next free one, taken under a lock of ours. */
		kl_lock_extension_object(KL_VERIFY_LOCK_SUBID);
		kl_run("INSERT INTO kingres.ledger_verification "
			   "(verification_id, started_at, finished_at, started_by, from_height, to_height, state, "
			   "problem_count, blocks_checked, rows_checked, problems) "
			   "OVERRIDING SYSTEM VALUE "
			   "VALUES (" KL_SQL_NEXT_VERIFICATION_ID ", $1, pg_catalog.clock_timestamp(), $2, $3, $4, $5, $6, $7, $8, $9) "
			   "RETURNING verification_id",
			   9, types, values, nulls, true);
		if (!kl_first(1, &d))
			elog(ERROR, "ledger verification record returned no row");
		vid = DatumGetInt64(d);
		kl_follow_sequence("kingres.ledger_verification", "verification_id", vid);

		ReleaseCurrentSubTransaction();
		MemoryContextSwitchTo(oldcxt);
		CurrentResourceOwner = oldowner;
	}
	PG_CATCH();
	{
		/*
		 * A run that was cancelled (statement_timeout, pg_cancel_backend,
		 * Ctrl-C) is recorded as failed too.  What cannot be recorded: a run
		 * that ends the session (pg_terminate_backend, transaction_timeout, a
		 * crash, a lost connection) and a run whose calling transaction rolls
		 * back.
		 */
		MemoryContextSwitchTo(oldcxt);
		edata = CopyErrorData();
		FlushErrorState();
		RollbackAndReleaseCurrentSubTransaction();
		MemoryContextSwitchTo(oldcxt);
		CurrentResourceOwner = oldowner;
	}
	PG_END_TRY();

	if (edata != NULL)
	{
		types[0] = TIMESTAMPTZOID;
		types[1] = NAMEOID;
		types[2] = INT8OID;
		types[3] = INT8OID;
		types[4] = TEXTOID;
		types[5] = TEXTOID;
		values[0] = TimestampTzGetDatum(started);
		values[1] = NameGetDatum(&who);
		values[2] = Int64GetDatum(lo);
		values[3] = Int64GetDatum(hi);
		values[4] = CStringGetTextDatum(edata->message ? edata->message : "");
		values[5] = CStringGetTextDatum(unpack_sql_state(edata->sqlerrcode));
		kl_lock_extension_object(KL_VERIFY_LOCK_SUBID);
		kl_run("INSERT INTO kingres.ledger_verification "
			   "(verification_id, started_at, finished_at, started_by, from_height, to_height, state, "
			   "problem_count, problems) "
			   "OVERRIDING SYSTEM VALUE "
			   "VALUES (" KL_SQL_NEXT_VERIFICATION_ID ", $1, pg_catalog.clock_timestamp(), $2, $3, $4, 'failed', NULL, "
			   "pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object("
			   "'check', 'verification_failed', 'object', NULL::pg_catalog.text, "
			   "'detail', pg_catalog.left($5, 1000), 'sqlstate', $6))) "
			   "RETURNING verification_id",
			   6, types, values, NULL, true);
		if (!kl_first(1, &d))
			elog(ERROR, "ledger verification record returned no row");
		vid = DatumGetInt64(d);
		kl_follow_sequence("kingres.ledger_verification", "verification_id", vid);
		ereport(WARNING,
				(errmsg("ledger verification " INT64_FORMAT " failed: %s",
						(int64) vid, edata->message ? edata->message : "")));
		FreeErrorData(edata);
	}
	SPI_finish();
	PG_RETURN_INT64(vid);
}

/*
 * kl_text_is_blank
 *		Whether a text is empty or only spaces (what btrim(x) = '' says).
 */
static bool
kl_text_is_blank(text *t)
{
	char	   *p = VARDATA_ANY(t);
	int			len = VARSIZE_ANY_EXHDR(t);
	int			i;

	for (i = 0; i < len; i++)
		if (p[i] != ' ')
			return false;
	return true;
}

/*
 * kingres.ledger_remove_stray(reason text) returns bigint
 *		The way out for a row that stops ledger_head and that no honest write
 *		leaves (README, "A row above the head"): a transaction row that names
 *		a block above the head (the end of the chain was removed), and a
 *		recorded digest that names a block that is not there or another hash
 *		than the block has.  What it removes it first copies into
 *		kingres.ledger_removed, as it was; the transactions of the removed
 *		blocks are queued again (their entries are still there), so the next
 *		block is built from them and no entry is left without a block.  It is
 *		an act of a superuser who asked for it: the session must have
 *		kingres_ledger.allow_manual_write on, and the caller must be a
 *		superuser (a superuser acting as another role is not).  No trigger is
 *		switched off and no other session is affected: the setting is the only
 *		thing that opens the tables, for this session.  The statements are
 *		manual writes (the marker is not raised); the hook allows them for the
 *		superuser who turned the setting on, and refuses them for anybody else.
 *
 *		SECURITY DEFINER, with a fixed search_path, so that the owner's
 *		search_path and the lock are the same whoever the superuser is; the
 *		checks are on the caller.
 */
Datum
kingres_ledger_remove_stray(PG_FUNCTION_ARGS)
{
	TimestampTz ts = GetCurrentTimestamp();
	NameData	who;
	Datum		values[4];
	Oid			types[4];
	Datum		d;
	int64		head;
	int64		n_tx;
	int64		n_dg;

	if (PG_ARGISNULL(0) || kl_text_is_blank(PG_GETARG_TEXT_PP(0)))
		ereport(ERROR,
				(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
				 errmsg("say why the rows are removed")));
	kl_outer_user_name(&who);
	if (!superuser_arg(GetOuterUserId()))
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("only a superuser can remove rows of the ledger tables")));
	if (!kl_manual_setting_on())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("removing rows of the ledger tables is a manual write"),
				 errhint("Run SET kingres_ledger.allow_manual_write = on in this session first.")));

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	/* No builder runs while the rows are taken out. */
	kl_lock_builders();
	kl_run("SELECT COALESCE(pg_catalog.max(b.height), -1) FROM kingres.ledger_blocks b",
		   0, NULL, NULL, NULL, false);
	if (!kl_first(1, &d))
		elog(ERROR, "no head");
	head = DatumGetInt64(d);

	types[0] = TIMESTAMPTZOID;
	types[1] = NAMEOID;
	types[2] = TEXTOID;
	types[3] = INT8OID;
	values[0] = TimestampTzGetDatum(ts);
	values[1] = NameGetDatum(&who);
	values[2] = PG_GETARG_DATUM(0);
	values[3] = Int64GetDatum(head);

	/* The identifiers of the records: the next free ones, under a lock of ours. */
	kl_lock_extension_object(KL_REMOVED_LOCK_SUBID);
	kl_run("INSERT INTO kingres.ledger_removed "
		   "(removal_id, removed_at, removed_by, reason, source_table, row_data) "
		   "OVERRIDING SYSTEM VALUE "
		   "SELECT (SELECT COALESCE(pg_catalog.max(x.removal_id), 0) FROM kingres.ledger_removed x) "
		   "+ pg_catalog.row_number() OVER (ORDER BY t.ledger_txid), "
		   "$1, $2, $3, 'ledger_tx', pg_catalog.to_jsonb(t) "
		   "FROM kingres.ledger_tx t WHERE t.block_height > $4 ORDER BY t.ledger_txid",
		   4, types, values, NULL, false);
	n_tx = SPI_processed;
	kl_run("INSERT INTO kingres.ledger_pending (ledger_txid, user_name) "
		   "SELECT t.ledger_txid, t.user_name FROM kingres.ledger_tx t "
		   "WHERE t.block_height > $1 "
		   "AND EXISTS (SELECT 1 FROM kingres.ledger_entry e WHERE e.ledger_txid = t.ledger_txid) "
		   "AND NOT EXISTS (SELECT 1 FROM kingres.ledger_pending p WHERE p.ledger_txid = t.ledger_txid)",
		   1, &types[3], &values[3], NULL, false);
	kl_run("DELETE FROM kingres.ledger_tx t WHERE t.block_height > $1",
		   1, &types[3], &values[3], NULL, false);

	kl_run("INSERT INTO kingres.ledger_removed "
		   "(removal_id, removed_at, removed_by, reason, source_table, row_data) "
		   "OVERRIDING SYSTEM VALUE "
		   "SELECT (SELECT COALESCE(pg_catalog.max(x.removal_id), 0) FROM kingres.ledger_removed x) "
		   "+ pg_catalog.row_number() OVER (ORDER BY d.digest_id), "
		   "$1, $2, $3, 'ledger_digest', pg_catalog.to_jsonb(d) "
		   "FROM kingres.ledger_digest d "
		   "WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_blocks b "
		   "WHERE b.height = d.height AND b.block_hash = d.block_hash) "
		   "ORDER BY d.digest_id",
		   3, types, values, NULL, false);
	n_dg = SPI_processed;
	kl_run("DELETE FROM kingres.ledger_digest d "
		   "WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_blocks b "
		   "WHERE b.height = d.height AND b.block_hash = d.block_hash)",
		   0, NULL, NULL, NULL, false);

	if (n_tx + n_dg > 0)
	{
		kl_run("SELECT pg_catalog.max(x.removal_id) FROM kingres.ledger_removed x",
			   0, NULL, NULL, NULL, false);
		if (kl_first(1, &d))
			kl_follow_sequence("kingres.ledger_removed", "removal_id",
							   DatumGetInt64(d));
	}

	SPI_finish();
	PG_RETURN_INT64(n_tx + n_dg);
}

/*
 * kingres.ledger_register_table(relation regclass) returns void
 *		The last step of ledger_enable: register a table that ledger_enable
 *		has set up (its triggers, and its history table when it is updatable)
 *		and enter the rows that it already contains, in the calling
 *		transaction.  Everything that is written is computed here from the
 *		catalogs: the table_id is the one in the capture trigger, which must
 *		be the next free id of the registry (no sequence), the mode is the
 *		mode of that trigger (UPDATE in its events means updatable), the
 *		history table is the table that ledger_enable names after the table,
 *		and the entries are the rows of the table.  A table that
 *		ledger_table_defects finds anything wrong with is refused, so a call
 *		that is not made by ledger_enable can only register a table that is
 *		exactly what ledger_enable would have made it, and only one that the
 *		caller owns.  Not executable by PUBLIC.
 *
 *		The rows are read by a statement that starts after the table lock is
 *		held (a cursor opened with a new snapshot), so every row committed
 *		before the lock was granted is entered; the rows inserted afterwards
 *		are captured by the trigger.
 */
Datum
kingres_ledger_register_table(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	Relation	rel;
	Trigger    *cap = NULL;
	TriggerDesc *tdesc;
	char	   *endp;
	long		tid;
	bool		updatable;
	Oid			histid = InvalidOid;
	char	   *relname;
	char	   *nspname;
	Datum		values[4];
	Oid			types[4];
	char		nulls[4];
	Datum		d;
	char	   *sql;
	Portal		portal;
	int			i;

	kl_check_owner(relid);
	if (XactIsoLevel != XACT_READ_COMMITTED)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("ledger_register_table requires the READ COMMITTED isolation level")));

	rel = table_open(relid, ShareRowExclusiveLock);
	if (rel->rd_rel->relkind != RELKIND_RELATION ||
		rel->rd_rel->relpersistence != RELPERSISTENCE_PERMANENT ||
		relid < FirstNormalObjectId)
		ereport(ERROR,
				(errcode(ERRCODE_WRONG_OBJECT_TYPE),
				 errmsg("\"%s\" cannot be a ledger table", RelationGetRelationName(rel))));
	relname = pstrdup(RelationGetRelationName(rel));
	nspname = get_namespace_name(RelationGetNamespace(rel));
	if (nspname == NULL || strcmp(nspname, KL_SCHEMA_NAME) == 0)
		ereport(ERROR,
				(errcode(ERRCODE_WRONG_OBJECT_TYPE),
				 errmsg("\"%s\" cannot be a ledger table", relname)));

	tdesc = rel->trigdesc;
	for (i = 0; tdesc != NULL && i < tdesc->numtriggers; i++)
		if (strcmp(tdesc->triggers[i].tgname, KL_TRIGGER_CAPTURE) == 0)
			cap = &tdesc->triggers[i];
	if (cap == NULL || cap->tgnargs != 1 ||
		cap->tgenabled != TRIGGER_FIRES_ALWAYS)
		ereport(ERROR,
				(errcode(ERRCODE_KL_PROTECTED),
				 errmsg("table \"%s\" has no armed capture trigger", relname)));
	errno = 0;
	tid = strtol(cap->tgargs[0], &endp, 10);
	if (errno != 0 || *endp != '\0' || endp == cap->tgargs[0] ||
		tid < 1 || tid > PG_INT32_MAX)
		ereport(ERROR,
				(errcode(ERRCODE_KL_PROTECTED),
				 errmsg("invalid table id \"%s\" in ledger trigger", cap->tgargs[0])));
	updatable = (cap->tgtype & TRIGGER_TYPE_UPDATE) != 0;
	if (updatable)
	{
		histid = get_relname_relid(psprintf("%s_ledger_history", relname),
								   RelationGetNamespace(rel));
		if (!OidIsValid(histid))
			ereport(ERROR,
					(errcode(ERRCODE_KL_PROTECTED),
					 errmsg("table \"%s\" has no history table", relname)));
	}

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	types[0] = REGCLASSOID;
	values[0] = ObjectIdGetDatum(relid);
	kl_run("SELECT 1 FROM kingres.ledger_table WHERE relid OPERATOR(pg_catalog.=) $1",
		   1, types, values, NULL, false);
	if (SPI_processed > 0)
		ereport(ERROR,
				(errcode(ERRCODE_DUPLICATE_OBJECT),
				 errmsg("table \"%s\" is already under ledger control", relname)));

	/*
	 * The id must be the one ledger_enable took: the next free one of the
	 * registry, taken under the registry's lock (ledger_lock_table took it
	 * for ledger_enable; a call that is not made by ledger_enable takes it
	 * here, after the lock of the table).  A larger id would collide with a
	 * table that is enabled later, a smaller one is in use.  No sequence is
	 * read: a role could set it to any value.
	 */
	kl_lock_extension_object(KL_REGISTRY_LOCK_SUBID);
	kl_run("SELECT COALESCE(pg_catalog.max(x.table_id), 0) + 1 FROM kingres.ledger_table x",
		   0, NULL, NULL, NULL, false);
	if (!kl_first(1, &d) || (int64) tid != DatumGetInt64(d))
		ereport(ERROR,
				(errcode(ERRCODE_KL_PROTECTED),
				 errmsg("table id %ld was not issued by the ledger", tid)));

	/* The registration. */
	types[0] = INT4OID;
	types[1] = REGCLASSOID;
	types[2] = TEXTOID;
	types[3] = REGCLASSOID;
	values[0] = Int32GetDatum((int32) tid);
	values[1] = ObjectIdGetDatum(relid);
	values[2] = CStringGetTextDatum(updatable ? "updatable" : "append_only");
	values[3] = ObjectIdGetDatum(histid);
	memset(nulls, ' ', sizeof(nulls));
	if (!updatable)
		nulls[3] = 'n';
	kl_run("INSERT INTO kingres.ledger_table (table_id, relid, mode, history_relid) "
		   "OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4)",
		   4, types, values, nulls, true);
	kl_follow_sequence("kingres.ledger_table", "table_id", (int64) tid);

	/* Whatever ledger_enable was meant to make, this is what it made. */
	types[0] = OIDOID;
	types[1] = INT4OID;
	values[0] = ObjectIdGetDatum(relid);
	values[1] = Int32GetDatum((int32) tid);
	kl_run("SELECT d.check_name || ': ' || d.detail "
		   "FROM kingres.ledger_table_defects($1, $2) d LIMIT 1",
		   2, types, values, NULL, false);
	if (kl_first(1, &d))
		ereport(ERROR,
				(errcode(ERRCODE_KL_PROTECTED),
				 errmsg("table \"%s\" is not completely under ledger control", relname),
				 errdetail("%s", TextDatumGetCString(d))));

	/*
	 * The rows the table already holds, in the order the table returns them.
	 * The lookups of the entry code (the registry, by the row type) run with
	 * the snapshot that is active, and it must see the registration that was
	 * just written: a new snapshot, as every statement of ledger_enable had.
	 * No CommandCounterIncrement is needed first: the statement above that
	 * looks for defects is not read-only for SPI, which advances the counter
	 * before it runs, after the registration was written (L11-04: the call
	 * that was here changed nothing a test could see).
	 */
	PushActiveSnapshot(GetTransactionSnapshot());
	sql = psprintf("SELECT kl_r FROM ONLY %s AS kl_r",
				   quote_qualified_identifier(nspname, relname));
	portal = SPI_cursor_open_with_args(NULL, sql, 0, NULL, NULL, NULL, false, 0);
	if (portal == NULL)
		elog(ERROR, "could not open a cursor on \"%s\"", relname);
	for (;;)
	{
		uint64		k;

		SPI_cursor_fetch(portal, true, 1000);
		if (SPI_processed == 0)
			break;
		/* Entering the rows of a large table is work, not a stall: see kl_hold_progress. */
		kl_hold_progress();
		for (k = 0; k < SPI_processed; k++)
		{
			bool		isnull;
			Datum		row = SPI_getbinval(SPI_tuptable->vals[k],
											SPI_tuptable->tupdesc, 1, &isnull);

			if (!isnull)
				kl_record_existing_row((int32) tid, DatumGetHeapTupleHeader(row));
		}
		SPI_freetuptable(SPI_tuptable);
	}
	SPI_cursor_close(portal);
	PopActiveSnapshot();

	SPI_finish();
	table_close(rel, NoLock);
	PG_RETURN_VOID();
}

/*
 * kl_record_manual_insert
 *		A superuser who turned kingres_ledger.allow_manual_write on has
 *		inserted a row by hand into a table of the ledger.  Leave a record of
 *		it in kingres.ledger_removed (action "inserted"): who, when, which
 *		table and the row as it was, which ledger_verify reports for as long as
 *		it exists (manual_write).  Called by the guard trigger, so the record
 *		is made, and rolled back, with the insert itself.  It is not part of
 *		any hash: the hash formats are untouched.
 */
void
kl_record_manual_insert(Relation rel, HeapTuple tuple)
{
	Datum		row = heap_copy_tuple_as_datum(tuple, RelationGetDescr(rel));
	Datum		js = DirectFunctionCall1(row_to_json, row);
	NameData	who;
	NameData	table;
	Datum		values[4];
	Oid			types[4];

	kl_outer_user_name(&who);
	namestrcpy(&table, RelationGetRelationName(rel));
	types[0] = NAMEOID;
	types[1] = TEXTOID;
	types[2] = NAMEOID;
	types[3] = TEXTOID;
	values[0] = NameGetDatum(&who);
	values[1] = CStringGetTextDatum("manual write (kingres_ledger.allow_manual_write)");
	values[2] = NameGetDatum(&table);
	values[3] = js;

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	/*
	 * Only a superuser gets here (the setting is theirs).  The record of a
	 * manual write into ledger_removed is never made, so the row that is being
	 * inserted cannot be one that this identifier could collide with.
	 */
	kl_lock_extension_object(KL_REMOVED_LOCK_SUBID);
	kl_run("INSERT INTO kingres.ledger_removed "
		   "(removal_id, removed_at, removed_by, reason, source_table, row_data, action) "
		   "OVERRIDING SYSTEM VALUE "
		   "VALUES ((SELECT COALESCE(pg_catalog.max(x.removal_id), 0) + 1 FROM kingres.ledger_removed x), "
		   "pg_catalog.clock_timestamp(), $1, $2, $3, "
		   "$4::pg_catalog.text::pg_catalog.jsonb, 'inserted') "
		   "RETURNING removal_id",
		   4, types, values, NULL, true);
	{
		Datum		idd;

		if (kl_first(1, &idd))
			kl_follow_sequence("kingres.ledger_removed", "removal_id",
							   DatumGetInt64(idd));
	}
	SPI_finish();
}
