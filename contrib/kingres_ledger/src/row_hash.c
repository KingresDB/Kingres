/*-------------------------------------------------------------------------
 *
 * row_hash.c
 *	  Canonical row encoding and row hash of the ledger, and the SQL
 *	  function kingres.ledger_row_hash(record).
 *
 * The row hash is the RFC 6962 leaf hash of the canonical encoding of a row:
 *
 *	  u8   tag KL_TAG_ROW (0x10)
 *	  u16  hash_version
 *	  u32  table_id
 *	  u16  number of encoded columns
 *	  for each non-dropped, non-NULL column in attribute order:
 *	      u16  length of the column name, then the name in UTF-8
 *	      u32  length of the value, then the value as produced by the type's
 *	           binary send function
 *
 * The text form of a value is never used because it depends on session
 * settings.  Binary send functions are independent of DateStyle,
 * extra_float_digits, bytea_output and IntervalStyle, but the send functions
 * of the text-like types convert from the database encoding to client_encoding.
 * To make the hash independent of client_encoding (and of the database
 * encoding for all convertible text), the client encoding is switched to
 * UTF-8 while the send functions run and restored afterwards.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/row_hash.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "catalog/pg_type.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "lib/stringinfo.h"
#include "mb/pg_wchar.h"
#include "utils/hsearch.h"
#include "utils/lsyscache.h"
#include "utils/rel.h"
#include "utils/memutils.h"
#include "utils/typcache.h"

#include "kingres_ledger.h"
#include "merkle.h"
#include "row_hash.h"

PG_FUNCTION_INFO_V1(kingres_ledger_row_hash);
PG_FUNCTION_INFO_V1(kingres_ledger_history_row_hash);

/* Scratch memory of the hash computation; reset for every row. */
static MemoryContext kl_work_cxt = NULL;

/* Whether PrepareClientEncoding(PG_UTF8) has been done in this backend. */
static bool kl_utf8_prepared = false;

/*
 * The identity that the registry has for a relation, kept for the rest of
 * the transaction in a hash table of its own (one entry per ledger table
 * that the transaction wrote): the capture trigger asks for it on every row,
 * for insert as for update and delete, and rows of several tables may
 * alternate (a trigger of an application that writes a second ledger table,
 * a bulk load through a view).  The first version kept one entry, and an
 * alternation cost an SPI lookup (about 7 microseconds) on every row.
 *
 * Why it cannot serve a stale identity: only a positive answer is stored; a
 * row of the registry is never updated or deleted (the ledger tables are
 * append-only, and a ledger table cannot be dropped, so a relation id is not
 * reused), so an answer that was true once is true for as long as the row
 * can be seen; and everything that could make a stored answer wrong is
 * dropped by the callbacks of capture.c: the end of the transaction, in
 * every way (commit, abort, prepare), and the abort of a subtransaction (a
 * registration that was rolled back to a savepoint, finding L2-05).  A
 * lookup that finds nothing (a table that is not registered yet) is not
 * stored, so a registration made later in the same transaction is found.
 */
typedef struct KlIdEntry
{
	Oid			relid;			/* hash key */
	int32		table_id;
} KlIdEntry;

typedef struct KlHistEntry
{
	Oid			relid;			/* hash key */
	int32		table_id;
	Oid			history;
} KlHistEntry;

static MemoryContext kl_cache_cxt = NULL;
static HTAB *kl_id_hash = NULL;
static HTAB *kl_hist_hash = NULL;

/* The last answer of kl_lookup_table_id, in front of the hash: a run of rows of one table. */
static Oid	kl_last_relid = InvalidOid;
static int32 kl_last_table_id = 0;

/* Saved plan of the table_id lookup. */
static SPIPlanPtr kl_lookup_plan = NULL;

/* Saved plans of the history lookups. */
static SPIPlanPtr kl_hist_plan = NULL;
static SPIPlanPtr kl_hist_by_history_plan = NULL;

static HTAB *
kl_cache_create(const char *name, Size entrysize)
{
	HASHCTL		ctl;

	if (kl_cache_cxt == NULL)
		kl_cache_cxt = AllocSetContextCreate(TopMemoryContext,
											 "kingres_ledger registry cache",
											 ALLOCSET_SMALL_SIZES);
	memset(&ctl, 0, sizeof(ctl));
	ctl.keysize = sizeof(Oid);
	ctl.entrysize = entrysize;
	ctl.hcxt = kl_cache_cxt;
	return hash_create(name, 32, &ctl, HASH_ELEM | HASH_BLOBS | HASH_CONTEXT);
}

/*
 * kl_table_cache_reset
 *		Forget the cached relid to table_id mappings.  Called at transaction
 *		end and at the abort of a subtransaction, so a rolled back
 *		ledger_enable cannot leave a stale entry.
 */
void
kl_table_cache_reset(void)
{
	if (kl_id_hash != NULL || kl_hist_hash != NULL)
		MemoryContextReset(kl_cache_cxt);
	kl_id_hash = NULL;
	kl_hist_hash = NULL;
	kl_last_relid = InvalidOid;
}

/*
 * kl_lookup_table_id
 *		Find the ledger table_id of a relation through kingres.ledger_table.
 *		Returns -1 when the relation is not under ledger control.
 *
 * The query is fully qualified and uses OPERATOR(pg_catalog.=) so that no
 * object of a schema earlier in the search_path can capture it.
 */
int32
kl_lookup_table_id(Oid relid)
{
	Datum		values[1];
	int32		result = -1;
	int			rc;

	if (OidIsValid(relid) && relid == kl_last_relid)
		return kl_last_table_id;
	if (kl_id_hash != NULL)
	{
		KlIdEntry  *hit = hash_search(kl_id_hash, &relid, HASH_FIND, NULL);

		if (hit != NULL)
		{
			kl_last_relid = relid;
			kl_last_table_id = hit->table_id;
			return hit->table_id;
		}
	}

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	if (kl_lookup_plan == NULL)
	{
		Oid			argtypes[1] = {REGCLASSOID};
		SPIPlanPtr	plan;

		plan = SPI_prepare("SELECT table_id FROM kingres.ledger_table "
						   "WHERE relid OPERATOR(pg_catalog.=) $1",
						   1, argtypes);
		if (plan == NULL)
			elog(ERROR, "SPI_prepare failed: %s",
				 SPI_result_code_string(SPI_result));
		if (SPI_keepplan(plan))
			elog(ERROR, "SPI_keepplan failed");
		kl_lookup_plan = plan;
	}

	values[0] = ObjectIdGetDatum(relid);
	rc = SPI_execute_plan(kl_lookup_plan, values, NULL, true, 1);
	if (rc != SPI_OK_SELECT)
		elog(ERROR, "SPI_execute_plan failed: %s", SPI_result_code_string(rc));

	if (SPI_processed == 1)
	{
		bool		isnull;
		Datum		d = SPI_getbinval(SPI_tuptable->vals[0],
									  SPI_tuptable->tupdesc, 1, &isnull);

		if (!isnull)
			result = DatumGetInt32(d);
	}
	SPI_finish();

	if (result >= 0)
	{
		/*
		 * A transaction that writes one table keeps one answer and builds no
		 * hash table (the cost of a table per transaction would show in a
		 * workload of single-row transactions); the second table makes the
		 * hash, which then holds the first answer too.
		 */
		if (OidIsValid(kl_last_relid) && kl_last_relid != relid)
		{
			KlIdEntry  *ent;

			if (kl_id_hash == NULL)
			{
				kl_id_hash = kl_cache_create("kingres_ledger table ids",
											 sizeof(KlIdEntry));
				ent = hash_search(kl_id_hash, &kl_last_relid, HASH_ENTER, NULL);
				ent->table_id = kl_last_table_id;
			}
			ent = hash_search(kl_id_hash, &relid, HASH_ENTER, NULL);
			ent->table_id = result;
		}
		kl_last_relid = relid;
		kl_last_table_id = result;
	}
	return result;
}

/*
 * kl_table_id_for_type
 *		table_id of the ledger table whose row type is "typeid"; raises an
 *		error when the type is not the row type of a ledger table.
 */
int32
kl_table_id_for_type(Oid typeid)
{
	Oid			relid = get_typ_typrelid(typeid);
	int32		table_id;

	if (!OidIsValid(relid))
		ereport(ERROR,
				(errcode(ERRCODE_WRONG_OBJECT_TYPE),
				 errmsg("argument must be a row of a ledger table")));
	table_id = kl_lookup_table_id(relid);
	if (table_id < 0)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("table \"%s\" is not under ledger control",
						get_rel_name(relid))));
	return table_id;
}

/*
 * kl_encode_row
 *		Append the canonical encoding of one row to "buf".
 *
 * Runs in the work context with the client encoding already switched to
 * UTF-8.
 */
static void
kl_encode_row(StringInfo buf, TupleDesc tupdesc, Datum *values, bool *isnull,
			  int32 table_id, bool history)
{
	uint8		hdr[8];
	int			natts = tupdesc->natts;
	int			ncols = 0;
	int			i;

	for (i = 0; i < natts; i++)
	{
		Form_pg_attribute att = TupleDescAttr(tupdesc, i);

		if (!att->attisdropped && !isnull[i] &&
			!(history && kl_is_history_column(NameStr(att->attname))))
			ncols++;
	}

	hdr[0] = KL_TAG_ROW;
	kl_put_u16(hdr + 1, KL_HASH_VERSION);
	kl_put_u32(hdr + 3, (uint32) table_id);
	appendBinaryStringInfo(buf, (char *) hdr, 7);
	kl_put_u16(hdr, (uint16) ncols);
	appendBinaryStringInfo(buf, (char *) hdr, 2);

	for (i = 0; i < natts; i++)
	{
		Form_pg_attribute att = TupleDescAttr(tupdesc, i);
		unsigned char *name;
		int			namelen;
		Oid			sendfn;
		bool		isvarlena;
		bytea	   *sent;

		if (att->attisdropped || isnull[i] ||
			(history && kl_is_history_column(NameStr(att->attname))))
			continue;

		/* The column name in UTF-8, whatever the database encoding is. */
		name = pg_do_encoding_conversion((unsigned char *) NameStr(att->attname),
										 strlen(NameStr(att->attname)),
										 GetDatabaseEncoding(), PG_UTF8);
		namelen = strlen((char *) name);
		if (namelen > PG_UINT16_MAX)
			elog(ERROR, "column name too long");
		kl_put_u16(hdr, (uint16) namelen);
		appendBinaryStringInfo(buf, (char *) hdr, 2);
		appendBinaryStringInfo(buf, (char *) name, namelen);

		getTypeBinaryOutputInfo(att->atttypid, &sendfn, &isvarlena);
		sent = OidSendFunctionCall(sendfn, values[i]);
		kl_put_u32(hdr, (uint32) (VARSIZE(sent) - VARHDRSZ));
		appendBinaryStringInfo(buf, (char *) hdr, 4);
		appendBinaryStringInfo(buf, VARDATA(sent), VARSIZE(sent) - VARHDRSZ);
	}
}

/*
 * kl_row_hash_tuple
 *		Compute the row hash of a heap tuple into "out" (KL_HASH_LEN bytes).
 *
 * All temporary allocations are made in a private context that is reset on
 * every call: an AFTER trigger fires once per inserted row inside a
 * statement-lifetime context, so allocating there would grow without bound
 * during a large COPY.  The client encoding is forced to UTF-8 for the
 * duration of the send functions (see the file header) and restored in
 * PG_FINALLY so that an error cannot leave it changed.
 */
void
kl_row_hash_tuple(TupleDesc tupdesc, HeapTuple tuple, int32 table_id,
				  bool history, uint8 *out)
{
	MemoryContext oldcxt;
	int			saved_encoding;
	volatile bool switched = false;

	if (kl_work_cxt == NULL)
		kl_work_cxt = AllocSetContextCreate(TopMemoryContext,
											"kingres_ledger row hash",
											ALLOCSET_DEFAULT_SIZES);
	else
		MemoryContextReset(kl_work_cxt);

	saved_encoding = pg_get_client_encoding();
	if (saved_encoding != PG_UTF8)
	{
		if (!kl_utf8_prepared)
		{
			if (PrepareClientEncoding(PG_UTF8) < 0)
				ereport(ERROR,
						(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
						 errmsg("cannot convert text to UTF-8 for ledger hashing")));
			kl_utf8_prepared = true;
		}
		if (SetClientEncoding(PG_UTF8) < 0)
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("cannot convert text to UTF-8 for ledger hashing")));
		switched = true;
	}

	oldcxt = MemoryContextSwitchTo(kl_work_cxt);
	PG_TRY();
	{
		Datum	   *values;
		bool	   *isnull;
		StringInfoData buf;

		values = (Datum *) palloc(tupdesc->natts * sizeof(Datum));
		isnull = (bool *) palloc(tupdesc->natts * sizeof(bool));
		heap_deform_tuple(tuple, tupdesc, values, isnull);

		initStringInfo(&buf);
		kl_encode_row(&buf, tupdesc, values, isnull, table_id, history);
		kl_leaf_hash(out, (const uint8 *) buf.data, buf.len);
	}
	PG_FINALLY();
	{
		if (switched)
			SetClientEncoding(saved_encoding);
	}
	PG_END_TRY();
	MemoryContextSwitchTo(oldcxt);
	MemoryContextReset(kl_work_cxt);
}

/*
 * kl_row_hash_composite
 *		Row hash of a composite datum (a whole-row value).  The tuple
 *		descriptor is looked up from the type information stored in the
 *		datum header.
 */
void
kl_row_hash_composite(HeapTupleHeader header, int32 table_id, bool history,
					  uint8 *out)
{
	Oid			tuptype = HeapTupleHeaderGetTypeId(header);
	int32		tupmod = HeapTupleHeaderGetTypMod(header);
	TupleDesc	tupdesc = lookup_rowtype_tupdesc(tuptype, tupmod);
	HeapTupleData tuple;

	tuple.t_len = HeapTupleHeaderGetDatumLength(header);
	ItemPointerSetInvalid(&(tuple.t_self));
	tuple.t_tableOid = InvalidOid;
	tuple.t_data = header;

	kl_row_hash_tuple(tupdesc, &tuple, table_id, history, out);
	ReleaseTupleDesc(tupdesc);
}

/*
 * kingres.ledger_row_hash(record) returns bytea
 *		Leaf hash of a row of a ledger table, for example
 *		SELECT kingres.ledger_row_hash(a) FROM accounts a.
 *		The table_id comes from kingres.ledger_table, found through the row
 *		type of the argument.
 */
Datum
kingres_ledger_row_hash(PG_FUNCTION_ARGS)
{
	HeapTupleHeader header = PG_GETARG_HEAPTUPLEHEADER(0);
	int32		table_id;
	bytea	   *result;

	table_id = kl_table_id_for_type(HeapTupleHeaderGetTypeId(header));

	result = (bytea *) palloc(VARHDRSZ + KL_HASH_LEN);
	SET_VARSIZE(result, VARHDRSZ + KL_HASH_LEN);
	kl_row_hash_composite(header, table_id, false, (uint8 *) VARDATA(result));
	PG_RETURN_BYTEA_P(result);
}

/*
 * kl_is_history_column
 *		Whether a column name is one of the three columns that the history
 *		table of an updatable ledger table adds to the columns of the table.
 *		They are not part of the encoded row.  ledger_enable and the event
 *		trigger reject columns of these names in an updatable table.
 */
bool
kl_is_history_column(const char *name)
{
	return strcmp(name, KL_HIST_COL_TXID) == 0 ||
		strcmp(name, KL_HIST_COL_SEQ) == 0 ||
		strcmp(name, KL_HIST_COL_OP) == 0;
}

/*
 * kl_lookup_history
 *		The history table of the ledger table "relid" and its table_id.
 *		Returns the OID of the history table, or InvalidOid when the table is
 *		append-only.  Raises an error when the table is not registered.  The
 *		query is fully qualified so that no object of a schema earlier in the
 *		search_path can capture it.
 */
Oid
kl_lookup_history(Oid relid, int32 *table_id)
{
	Datum		values[1];
	Oid			history = InvalidOid;
	int32		tid = -1;
	int			rc;

	if (kl_hist_hash != NULL)
	{
		KlHistEntry *hit = hash_search(kl_hist_hash, &relid, HASH_FIND, NULL);

		if (hit != NULL)
		{
			*table_id = hit->table_id;
			return hit->history;
		}
	}

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	if (kl_hist_plan == NULL)
	{
		Oid			argtypes[1] = {REGCLASSOID};
		SPIPlanPtr	plan;

		plan = SPI_prepare("SELECT table_id, history_relid::pg_catalog.oid "
						   "FROM kingres.ledger_table "
						   "WHERE relid OPERATOR(pg_catalog.=) $1",
						   1, argtypes);
		if (plan == NULL)
			elog(ERROR, "SPI_prepare failed: %s",
				 SPI_result_code_string(SPI_result));
		if (SPI_keepplan(plan))
			elog(ERROR, "SPI_keepplan failed");
		kl_hist_plan = plan;
	}
	values[0] = ObjectIdGetDatum(relid);
	rc = SPI_execute_plan(kl_hist_plan, values, NULL, true, 1);
	if (rc != SPI_OK_SELECT)
		elog(ERROR, "SPI_execute_plan failed: %s", SPI_result_code_string(rc));
	if (SPI_processed == 1)
	{
		bool		isnull;
		Datum		d = SPI_getbinval(SPI_tuptable->vals[0],
									  SPI_tuptable->tupdesc, 1, &isnull);

		if (!isnull)
			tid = DatumGetInt32(d);
		d = SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 2,
						  &isnull);
		if (!isnull)
			history = DatumGetObjectId(d);
	}
	SPI_finish();

	if (tid < 0)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("table \"%s\" is not under ledger control",
						get_rel_name(relid))));
	{
		KlHistEntry *ent;

		if (kl_hist_hash == NULL)
			kl_hist_hash = kl_cache_create("kingres_ledger history ids",
										   sizeof(KlHistEntry));
		ent = hash_search(kl_hist_hash, &relid, HASH_ENTER, NULL);
		ent->table_id = tid;
		ent->history = history;
	}
	*table_id = tid;
	return history;
}

/*
 * kingres.ledger_history_row_hash(record) returns bytea
 *		Row hash of a row of the history table of an updatable ledger table:
 *		the leaf hash of the canonical encoding of the columns the history
 *		table has in common with the ledger table (everything except
 *		ledger_txid, ledger_seq and ledger_op), with the table_id of the ledger
 *		table.  It equals the hash that the capture trigger computed for the
 *		old version of the row.
 */
Datum
kingres_ledger_history_row_hash(PG_FUNCTION_ARGS)
{
	HeapTupleHeader header = PG_GETARG_HEAPTUPLEHEADER(0);
	Oid			relid = get_typ_typrelid(HeapTupleHeaderGetTypeId(header));
	int32		table_id = -1;
	bytea	   *result;
	Datum		values[1];
	int			rc;

	if (!OidIsValid(relid))
		ereport(ERROR,
				(errcode(ERRCODE_WRONG_OBJECT_TYPE),
				 errmsg("argument must be a row of a ledger history table")));

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	if (kl_hist_by_history_plan == NULL)
	{
		Oid			argtypes[1] = {REGCLASSOID};
		SPIPlanPtr	plan;

		plan = SPI_prepare("SELECT table_id FROM kingres.ledger_table "
						   "WHERE history_relid OPERATOR(pg_catalog.=) $1",
						   1, argtypes);
		if (plan == NULL)
			elog(ERROR, "SPI_prepare failed: %s",
				 SPI_result_code_string(SPI_result));
		if (SPI_keepplan(plan))
			elog(ERROR, "SPI_keepplan failed");
		kl_hist_by_history_plan = plan;
	}
	values[0] = ObjectIdGetDatum(relid);
	rc = SPI_execute_plan(kl_hist_by_history_plan, values, NULL, true, 1);
	if (rc != SPI_OK_SELECT)
		elog(ERROR, "SPI_execute_plan failed: %s", SPI_result_code_string(rc));
	if (SPI_processed == 1)
	{
		bool		isnull;
		Datum		d = SPI_getbinval(SPI_tuptable->vals[0],
									  SPI_tuptable->tupdesc, 1, &isnull);

		if (!isnull)
			table_id = DatumGetInt32(d);
	}
	SPI_finish();
	if (table_id < 0)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("table \"%s\" is not the history table of a ledger table",
						get_rel_name(relid))));

	result = (bytea *) palloc(VARHDRSZ + KL_HASH_LEN);
	SET_VARSIZE(result, VARHDRSZ + KL_HASH_LEN);
	kl_row_hash_composite(header, table_id, true, (uint8 *) VARDATA(result));
	PG_RETURN_BYTEA_P(result);
}