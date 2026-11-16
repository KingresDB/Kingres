/*-------------------------------------------------------------------------
 *
 * kingres_sketch.c
 *	  Module entry, the code the three sketch types share, and self_test().
 *
 * Holds the checks of the common header of a sketch (magic, format version,
 * kind), the text ('\x' and hex digits) and binary input and output of the
 * three types, the casts to and from bytea, the 64-bit hash of an element,
 * and kingres_sketch.self_test(), whose result is compared across the nodes
 * of a cluster.  The algorithms are in hll.c, topn.c and tdigest.c.
 *
 * Every byte string that reaches this extension from outside (a text or
 * binary input, a cast from bytea, the state of a partial aggregate sent by
 * another node) is untrusted: its size is checked against the largest
 * possible value of its kind before anything is decoded or allocated, and
 * the type's validator checks every length, count and parameter before it
 * is used.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_sketch/src/kingres_sketch.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/detoast.h"
#include "access/parallel.h"
#include "catalog/pg_collation.h"
#include "catalog/pg_type.h"
#include "libpq/pqformat.h"
#include "miscadmin.h"
#include "nodes/miscnodes.h"
#include "utils/array.h"
#include "utils/builtins.h"
#include "utils/fmgrprotos.h"
#include "utils/guc.h"
#include "utils/hsearch.h"
#include "utils/lsyscache.h"
#include "utils/typcache.h"

#include "kingres_sketch.h"

KS_MODULE_MAGIC();

PG_FUNCTION_INFO_V1(kingres_sketch_self_test);
PG_FUNCTION_INFO_V1(kingres_sketch_sketch_kind);

/*
 * Largest possible value of each kind, in bytes.  These follow from the
 * parameter limits: a dense hll of precision 18 (3/4 of 2^18 bytes), a top-n
 * of capacity 100000 whose items are 1024 bytes long, a t-digest of
 * compression 10000 with 2 * compression + 2 centroids.  A longer byte
 * string cannot be a valid value and is refused before it is decoded.
 */
Size
ks_max_value_bytes(int kind)
{
	switch (kind)
	{
		case KS_KIND_HLL:
			return KS_HEADER_LEN + (((Size) 3 << KS_HLL_MAX_PRECISION) / 4);
		case KS_KIND_TOPN:
			return 20 + (Size) KS_TOPN_MAX_CAPACITY * (18 + KS_TOPN_MAX_ITEM_BYTES);
		case KS_KIND_TDIGEST:
			return 36 + 16 * ((Size) 2 * KS_TD_MAX_COMPRESSION + 2) + 8;
	}
	return 0;
}

/*
 * Check the common header.  Format version 1 is the only one that exists; a
 * reader never interprets bytes of another version as version 1.
 */
bool
ks_check_header(const uint8 *p, Size len, int kind, char *why)
{
	if (len < KS_HEADER_LEN)
	{
		snprintf(why, KS_WHY_LEN, "value is too short (%zu bytes)", len);
		return false;
	}
	if (p[0] != KS_MAGIC)
	{
		snprintf(why, KS_WHY_LEN, "bad magic byte 0x%02x", p[0]);
		return false;
	}
	if (p[1] != KS_FORMAT_VERSION)
	{
		snprintf(why, KS_WHY_LEN, "unsupported sketch format %d", p[1]);
		return false;
	}
	if (p[2] != kind)
	{
		snprintf(why, KS_WHY_LEN, "wrong kind of sketch %d, expected %d", p[2], kind);
		return false;
	}
	return true;
}

void
ks_invalid_value(const char *typname, const char *why)
{
	ereport(ERROR,
			(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
			 errmsg("invalid %s value", typname),
			 errdetail("%s.", why)));
}

/*
 * The working memory of a call that is not an aggregate is not counted (it
 * is freed with the row), but a value that alone passes the limit is refused
 * before it is detoasted and unpacked: the unpacked form is as large as the
 * value and more (see the README for the measured factors).
 */
void
ks_arg_precheck(FunctionCallInfo fcinfo, int argno)
{
	ks_acct_precheck(toast_raw_datum_size(PG_GETARG_DATUM(argno)) - VARHDRSZ);
}

const uint8 *
ks_arg_bytes(FunctionCallInfo fcinfo, int argno, Size *len)
{
	struct varlena *v;

	ks_arg_precheck(fcinfo, argno);
	v = pg_detoast_datum_packed((struct varlena *) PG_GETARG_POINTER(argno));
	*len = VARSIZE_ANY_EXHDR(v);
	return (const uint8 *) VARDATA_ANY(v);
}

bytea *
ks_make_bytea(const uint8 *buf, Size len)
{
	bytea	   *r = (bytea *) palloc(VARHDRSZ + len);

	SET_VARSIZE(r, VARHDRSZ + len);
	memcpy(VARDATA(r), buf, len);
	return r;
}

static int
hex_digit_value(char c)
{
	if (c >= '0' && c <= '9')
		return c - '0';
	if (c >= 'a' && c <= 'f')
		return c - 'a' + 10;
	if (c >= 'A' && c <= 'F')
		return c - 'A' + 10;
	return -1;
}

/*
 * Text input: "\x" followed by an even number of hex digits.  The size is
 * checked before the bytes are decoded.  An invalid value is reported with
 * SQLSTATE 22P02, softly when the caller asked for that (pg_input_is_valid,
 * COPY ... ON_ERROR ignore).
 */
Datum
ks_in_common(FunctionCallInfo fcinfo, int kind, const char *typname,
			 bool (*validate) (const uint8 *, Size, char *))
{
	char	   *str = PG_GETARG_CSTRING(0);
	Node	   *escontext = fcinfo->context;
	Size		slen = strlen(str);
	Size		nbytes;
	uint8	   *buf;
	Size		i;
	char		why[KS_WHY_LEN];
	bytea	   *r;

	if (slen < 2 || str[0] != '\\' || str[1] != 'x')
		ereturn(escontext, (Datum) 0,
				(errcode(ERRCODE_INVALID_TEXT_REPRESENTATION),
				 errmsg("invalid input syntax for type %s", typname),
				 errdetail("The value must start with \"\\x\" and hold hexadecimal digits.")));
	slen -= 2;
	if (slen % 2 != 0)
		ereturn(escontext, (Datum) 0,
				(errcode(ERRCODE_INVALID_TEXT_REPRESENTATION),
				 errmsg("invalid input syntax for type %s", typname),
				 errdetail("The number of hexadecimal digits is odd.")));
	nbytes = slen / 2;
	if (nbytes > ks_max_value_bytes(kind))
		ereturn(escontext, (Datum) 0,
				(errcode(ERRCODE_INVALID_TEXT_REPRESENTATION),
				 errmsg("invalid input syntax for type %s", typname),
				 errdetail("The value is longer than any valid %s.", typname)));
	ks_acct_precheck(nbytes);
	buf = (uint8 *) palloc(nbytes > 0 ? nbytes : 1);
	for (i = 0; i < nbytes; i++)
	{
		int			hi = hex_digit_value(str[2 + 2 * i]);
		int			lo = hex_digit_value(str[3 + 2 * i]);

		if (hi < 0 || lo < 0)
			ereturn(escontext, (Datum) 0,
					(errcode(ERRCODE_INVALID_TEXT_REPRESENTATION),
					 errmsg("invalid input syntax for type %s", typname),
					 errdetail("A character that is not a hexadecimal digit was found.")));
		buf[i] = (uint8) ((hi << 4) | lo);
	}
	if (!validate(buf, nbytes, why))
		ereturn(escontext, (Datum) 0,
				(errcode(ERRCODE_INVALID_TEXT_REPRESENTATION),
				 errmsg("invalid input syntax for type %s", typname),
				 errdetail("%s.", why)));
	r = ks_make_bytea(buf, nbytes);
	pfree(buf);
	PG_RETURN_BYTEA_P(r);
}

/*
 * Text output: "\x" and lower-case hex digits.  A stored value is not
 * validated again here; every function that reads the content validates it.
 */
Datum
ks_out_common(FunctionCallInfo fcinfo)
{
	const uint8 *p;
	Size		len;
	char	   *out;
	Size		i;
	static const char hex[] = "0123456789abcdef";

	p = ks_arg_bytes(fcinfo, 0, &len);
	out = (char *) palloc(2 + 2 * len + 1);
	out[0] = '\\';
	out[1] = 'x';
	for (i = 0; i < len; i++)
	{
		out[2 + 2 * i] = hex[p[i] >> 4];
		out[3 + 2 * i] = hex[p[i] & 15];
	}
	out[2 + 2 * len] = '\0';
	PG_RETURN_CSTRING(out);
}

/* Binary input: the whole message is the value.  22P03 when invalid. */
Datum
ks_recv_common(FunctionCallInfo fcinfo, int kind, const char *typname,
			   bool (*validate) (const uint8 *, Size, char *))
{
	StringInfo	buf = (StringInfo) PG_GETARG_POINTER(0);
	Size		nbytes = buf->len - buf->cursor;
	const char *data;
	char		why[KS_WHY_LEN];

	if (nbytes > ks_max_value_bytes(kind))
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_BINARY_REPRESENTATION),
				 errmsg("invalid binary input for type %s", typname),
				 errdetail("The value is longer than any valid %s.", typname)));
	ks_acct_precheck(nbytes);
	data = pq_getmsgbytes(buf, nbytes);
	if (!validate((const uint8 *) data, nbytes, why))
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_BINARY_REPRESENTATION),
				 errmsg("invalid binary input for type %s", typname),
				 errdetail("%s.", why)));
	PG_RETURN_BYTEA_P(ks_make_bytea((const uint8 *) data, nbytes));
}

Datum
ks_send_common(FunctionCallInfo fcinfo)
{
	const uint8 *p;
	Size		len;
	StringInfoData buf;

	p = ks_arg_bytes(fcinfo, 0, &len);
	pq_begintypsend(&buf);
	pq_sendbytes(&buf, (const char *) p, len);
	PG_RETURN_BYTEA_P(pq_endtypsend(&buf));
}

/* Cast sketch -> bytea: the bytes of the value (the same as send). */
Datum
ks_to_bytea_common(FunctionCallInfo fcinfo)
{
	const uint8 *p;
	Size		len;

	p = ks_arg_bytes(fcinfo, 0, &len);
	PG_RETURN_BYTEA_P(ks_make_bytea(p, len));
}

/* Cast bytea -> sketch: validating; 22023 when the bytes are not a value. */
Datum
ks_from_bytea_common(FunctionCallInfo fcinfo, int kind, const char *typname,
					 bool (*validate) (const uint8 *, Size, char *))
{
	const uint8 *p;
	Size		len;
	char		why[KS_WHY_LEN];

	p = ks_arg_bytes(fcinfo, 0, &len);
	if (len > ks_max_value_bytes(kind))
		ks_invalid_value(typname, "the value is longer than any valid value of the type");
	if (!validate(p, len, why))
		ks_invalid_value(typname, why);
	PG_RETURN_BYTEA_P(ks_make_bytea(p, len));
}

/* ---- memory of aggregate states ---- */

/*
 * The limit on the bytes that the aggregate states of one backend may hold
 * together (a parallel worker is a backend of its own and has its own count)
 * is the smaller of two settings:
 *
 *	state_memory_ceiling	PGC_SUSET: the administrator's.  PostgreSQL itself
 *		refuses to let a role without the privilege store or set a PGC_SUSET
 *		setting, by SET, set_config, ALTER ROLE, ALTER DATABASE, the SET clause
 *		of a function and the options of a connection, and it refuses the same
 *		for a value stored before the library is loaded (a name that is not yet
 *		known is a placeholder that only a superuser or a holder of GRANT SET
 *		ON PARAMETER may store, and a placeholder set by a plain SET is applied
 *		to the real setting with the rights of the role that set it).
 *	state_memory_limit	PGC_USERSET: any role's own, to lower the limit for a
 *		query of its own.  -1 means the ceiling alone.
 *
 * An earlier version had one USERSET setting and a check hook that judged by
 * the source of the value; a role could store a value for itself with
 * ALTER ROLE, which the hook never saw.  A hook cannot know who stored a
 * value, so the rule is in the context of the setting, which PostgreSQL
 * enforces at every place a value can enter.
 */
#define KS_STATE_CEILING_DEFAULT_KB	(1024 * 1024)

static int	ks_state_ceiling_kb = KS_STATE_CEILING_DEFAULT_KB;
static int	ks_state_limit_kb = -1;

/* The sum of the accounts of the live state contexts of this backend. */
static int64 ks_state_bytes = 0;

/*
 * The ceiling that applies when the administrator's value of the ceiling was
 * lost.  PostgreSQL keeps a value of a setting whose library is not loaded yet
 * as a placeholder.  A role can pass the setting in the options of its
 * connection (a source that wins over the configuration file and over
 * ALTER ROLE and ALTER DATABASE and becomes the reset value of the
 * placeholder); when the library is loaded, PostgreSQL refuses that value
 * because the setting is PGC_SUSET, and it has already dropped the
 * administrator's value for the one it refuses, so the setting is left at its
 * built-in default, which can be higher than what the administrator gave.  The
 * loss is seen in _PG_init and the limit of that backend is the smaller of the
 * ceiling and this failsafe, so that the attempt can only lower the limit of
 * the role that made it.
 */
#define KS_FAILSAFE_KB	1024
static bool ks_ceiling_lost = false;
static bool ks_ceiling_applied = false;

static int64
ks_effective_limit(void)
{
	int			kb = ks_state_ceiling_kb;

	if (ks_ceiling_lost && kb > KS_FAILSAFE_KB)
		kb = KS_FAILSAFE_KB;

	if (ks_state_limit_kb >= 0 && ks_state_limit_kb < kb)
		kb = ks_state_limit_kb;
	return (int64) kb * 1024;
}

/*
 * The account of one aggregate context: the context that holds all states
 * made under it, and the bytes charged to the counter for it.  The struct
 * lives in the context it describes.
 */
struct KsAcct
{
	MemoryContextCallback cb;	/* must stay where it is: a context points to it */
	MemoryContext cxt;
	MemoryContext parent;		/* the aggregate context, the key of the table */
	int64		bytes;			/* what this account has added to ks_state_bytes */
};

typedef struct KsAcctEntry
{
	MemoryContext parent;		/* hash key */
	KsAcct	   *acct;
} KsAcctEntry;

static HTAB *ks_accts = NULL;
static KsAcct *ks_last = NULL;	/* the account used last: a query has one aggregate context for all its groups */

/*
 * The callback of a state context, called when the context is deleted or
 * reset, whatever the reason: it must not raise an error (it can run while a
 * transaction aborts) and does not.
 */
static void
ks_acct_reset_callback(void *arg)
{
	KsAcct	   *acct = (KsAcct *) arg;

	ks_state_bytes -= acct->bytes;
	acct->bytes = 0;
	Assert(ks_state_bytes >= 0);
	if (ks_last == acct)
		ks_last = NULL;
	if (ks_accts != NULL)
		(void) hash_search(ks_accts, &acct->parent, HASH_REMOVE, NULL);
}

static void
ks_state_over_limit(int64 held)
{
	ereport(ERROR,
			(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
			 errmsg("aggregate states of kingres_sketch hold %lld bytes in this backend, over the limit of %lld bytes",
					(long long) held, (long long) ks_effective_limit()),
			 errdetail("Every group, and every parallel worker, has an aggregate state of its own, and work_mem does not limit them."),
			 errhint("Aggregate fewer groups at once, use a smaller capacity, precision or compression, or ask a superuser to raise kingres_sketch.state_memory_ceiling.")));
}

KsAcct *
ks_acct_create(MemoryContext aggcontext)
{
	MemoryContext cxt;
	KsAcct	   *acct;
	KsAcctEntry *ent;
	bool		found;

	if (ks_last != NULL && ks_last->parent == aggcontext)
		return ks_last;
	if (ks_accts == NULL)
	{
		HASHCTL		ctl;

		ctl.keysize = sizeof(MemoryContext);
		ctl.entrysize = sizeof(KsAcctEntry);
		ctl.hcxt = TopMemoryContext;
		ks_accts = hash_create("kingres_sketch aggregate contexts", 8, &ctl,
							   HASH_ELEM | HASH_BLOBS | HASH_CONTEXT);
	}
	ent = (KsAcctEntry *) hash_search(ks_accts, &aggcontext, HASH_FIND, &found);
	if (found)
	{
		ks_last = ent->acct;
		return ent->acct;
	}

	cxt = AllocSetContextCreate(aggcontext, "kingres_sketch aggregate states",
								ALLOCSET_SMALL_MINSIZE, ALLOCSET_SMALL_INITSIZE,
								8 * 1024 * 1024);
	acct = (KsAcct *) MemoryContextAllocZero(cxt, sizeof(KsAcct));
	acct->cxt = cxt;
	acct->parent = aggcontext;
	ent = (KsAcctEntry *) hash_search(ks_accts, &aggcontext, HASH_ENTER, &found);
	ent->acct = acct;
	acct->cb.func = ks_acct_reset_callback;
	acct->cb.arg = acct;
	MemoryContextRegisterResetCallback(cxt, &acct->cb);
	ks_last = acct;
	return acct;
}

MemoryContext
ks_acct_context(KsAcct *acct)
{
	return acct->cxt;
}

/*
 * The counter and the account change together before the check, so that a
 * failed check leaves them agreeing: the callback takes out exactly what the
 * account holds.
 */
void
ks_acct_update(KsAcct *acct)
{
	int64		now = (int64) MemoryContextMemAllocated(acct->cxt, false);

	ks_state_bytes += now - acct->bytes;
	acct->bytes = now;
	if (ks_state_bytes > ks_effective_limit())
		ks_state_over_limit(ks_state_bytes);
}

void
ks_acct_precheck(Size want)
{
	if (ks_state_bytes + (int64) want > ks_effective_limit())
		ks_state_over_limit(ks_state_bytes + (int64) want);
}

/*
 * Check hook of the ceiling: it accepts everything, and notes that a value
 * of some source (not the built-in default) was accepted, which _PG_init
 * needs to know (see ks_ceiling_lost).
 */
static bool
ks_ceiling_check(int *newval, void **extra, GucSource source)
{
	if (source != PGC_S_DEFAULT && source != PGC_S_DYNAMIC_DEFAULT)
		ks_ceiling_applied = true;
	return true;
}

void
_PG_init(void)
{
	bool		had_reset = false;

	/* a placeholder with a reset value: a value that the definition below will try to apply */
	if (GetConfigOption("kingres_sketch.state_memory_ceiling", true, false) != NULL)
	{
		const char *rs = GetConfigOptionResetString("kingres_sketch.state_memory_ceiling");

		had_reset = (rs != NULL && rs[0] != '\0');
	}
	DefineCustomIntVariable("kingres_sketch.state_memory_ceiling",
							"Ceiling of the memory held by the aggregate states of one backend; only a superuser sets it.",
							"The aggregates of kingres_sketch (hll, topn, tdigest) keep one state per group and per parallel worker; work_mem does not limit them. A query that would hold more fails with SQLSTATE 54000. The limit of a backend is the smaller of this setting and kingres_sketch.state_memory_limit.",
							&ks_state_ceiling_kb,
							KS_STATE_CEILING_DEFAULT_KB,
							1,
							MAX_KILOBYTES,
							PGC_SUSET,
							GUC_UNIT_KB,
							ks_ceiling_check,
							NULL,
							NULL);
	if (had_reset && !ks_ceiling_applied)
	{
		ks_ceiling_lost = true;
		ereport(WARNING,
				(errmsg("the value of kingres_sketch.state_memory_ceiling that was set before the library was loaded was refused, and the ceiling is not known"),
				 errdetail("The setting can only be set by a superuser; the limit of this session is at most %d kB.",
						   KS_FAILSAFE_KB),
				 errhint("Preload the library (shared_preload_libraries) so that the administrator's value cannot be lost.")));
	}
	DefineCustomIntVariable("kingres_sketch.state_memory_limit",
							"Own limit of the memory held by the aggregate states of one backend; -1 means the ceiling alone.",
							"Any role may set it, to lower the limit for itself; the limit in force is the smaller of this setting and kingres_sketch.state_memory_ceiling.",
							&ks_state_limit_kb,
							-1,
							-1,
							MAX_KILOBYTES,
							PGC_USERSET,
							GUC_UNIT_KB,
							NULL,
							NULL,
							NULL);
	MarkGUCPrefixReserved("kingres_sketch");
}

PG_FUNCTION_INFO_V1(kingres_sketch_state_bytes);
PG_FUNCTION_INFO_V1(kingres_sketch_state_limit);

/* The bytes held by the aggregate states of this backend now (for tests and monitoring). */
Datum
kingres_sketch_state_bytes(PG_FUNCTION_ARGS)
{
	PG_RETURN_INT64(ks_state_bytes);
}

/* The limit in force in this backend, in bytes: the smaller of the two settings. */
Datum
kingres_sketch_state_limit(PG_FUNCTION_ARGS)
{
	PG_RETURN_INT64(ks_effective_limit());
}

/* ---- the aggregate memory context ---- */

MemoryContext
ks_agg_context(FunctionCallInfo fcinfo)
{
	MemoryContext aggcontext;

	if (!AggCheckCallContext(fcinfo, &aggcontext))
		elog(ERROR, "kingres_sketch aggregate function called in non-aggregate context");
	return aggcontext;
}

/* ---- self_test ---- */

uint64
ks_hash_bytes64(const uint8 *p, Size len)
{
	uint64		h = UINT64CONST(0xcbf29ce484222325);
	Size		i;

	for (i = 0; i < len; i++)
	{
		h ^= p[i];
		h *= UINT64CONST(0x100000001b3);
	}
	return h;
}

static void
selftest_add(StringInfo buf, const char *name, Datum hash)
{
	appendStringInfo(buf, " %s=%016llx", name,
					 (unsigned long long) ks_mix64((uint64) DatumGetInt64(hash)));
}

/*
 * self_test() returns one line that depends on the extension version, the
 * format version, the hash of a fixed value of each type that the sketches
 * accept, an hll estimate over a fixed set, and the bytes of a fixed tdigest
 * and topn built, and united with a second one, from a formula.  Two nodes that give a
 * different line cannot exchange sketches: they differ in encoding, byte
 * order, a hash function or the floating-point behaviour of the estimator.
 * The text hash uses the C collation so the line does not depend on the
 * database's collation.
 */
Datum
kingres_sketch_self_test(PG_FUNCTION_ARGS)
{
	StringInfoData buf;
	Datum		d;

	initStringInfo(&buf);
	appendStringInfo(&buf, "kingres_sketch %s format=%d", KS_EXT_VERSION,
					 KS_FORMAT_VERSION);
	selftest_add(&buf, "int2", DirectFunctionCall2(hashint2extended, Int16GetDatum(-7), Int64GetDatum(0)));
	selftest_add(&buf, "int4", DirectFunctionCall2(hashint4extended, Int32GetDatum(42), Int64GetDatum(0)));
	selftest_add(&buf, "int8", DirectFunctionCall2(hashint8extended, Int64GetDatum(-1), Int64GetDatum(0)));
	selftest_add(&buf, "bool", DirectFunctionCall2(hashcharextended, BoolGetDatum(true), Int64GetDatum(0)));
	selftest_add(&buf, "float8", DirectFunctionCall2(hashfloat8extended, Float8GetDatum(-0.0), Int64GetDatum(0)));
	d = CStringGetTextDatum("kingres");
	selftest_add(&buf, "text", DirectFunctionCall2Coll(hashtextextended, C_COLLATION_OID, d, Int64GetDatum(0)));
	d = DirectFunctionCall3(numeric_in, CStringGetDatum("-12345.6789"), ObjectIdGetDatum(InvalidOid), Int32GetDatum(-1));
	selftest_add(&buf, "numeric", DirectFunctionCall2(hash_numeric_extended, d, Int64GetDatum(0)));
	d = DirectFunctionCall1(uuid_in, CStringGetDatum("123e4567-e89b-12d3-a456-426614174000"));
	selftest_add(&buf, "uuid", DirectFunctionCall2(uuid_hash_extended, d, Int64GetDatum(0)));
	{
		Datum		elems[3] = {Int32GetDatum(1), Int32GetDatum(2), Int32GetDatum(3)};

		d = PointerGetDatum(construct_array_builtin(elems, 3, INT4OID));
		appendStringInfo(&buf, " int4[]=%016llx",
						 (unsigned long long) ks_hash_value_of_type(INT4ARRAYOID, d, C_COLLATION_OID));
	}
	appendStringInfo(&buf, " hll=%lld", (long long) hll_self_test_value());
	{
		uint64		td1,
					td2,
					td3,
					td4,
					tn1,
					tn2;

		tdigest_self_test_values(&td1, &td2, &td3, &td4);
		topn_self_test_values(&tn1, &tn2);
		appendStringInfo(&buf, " tdigest=%016llx tdigest_union=%016llx tdigest_read=%016llx tdigest_scale=%016llx topn=%016llx topn_union=%016llx",
						 (unsigned long long) td1, (unsigned long long) td2,
						 (unsigned long long) td3, (unsigned long long) td4,
						 (unsigned long long) tn1,
						 (unsigned long long) tn2);
	}
	PG_RETURN_TEXT_P(cstring_to_text_with_len(buf.data, buf.len));
}

/* ---- sketch_kind ---- */

/*
 * sketch_kind(bytea) names the kind of a byte string that is a valid sketch
 * value or the serialized state of an aggregate ('hll', 'topn', 'tdigest',
 * 'tdigest_state' for the state of approx_percentile) and raises 22023
 * otherwise.  It runs the same validation as the type input functions and as
 * the deserial functions of the aggregates, so a node can check a state that
 * another node sent without building an aggregate, and the tests can feed it
 * corrupted states.
 */
Datum
kingres_sketch_sketch_kind(PG_FUNCTION_ARGS)
{
	const uint8 *p;
	Size		len;
	char		why[KS_WHY_LEN];
	const char *name;
	int			kind;
	bool		ok;

	p = ks_arg_bytes(fcinfo, 0, &len);
	if (len < 3 || p[0] != KS_MAGIC)
		ks_invalid_value("sketch", "bad magic byte or too short");
	switch (p[2])
	{
		case KS_KIND_HLL:
			name = "hll";
			kind = KS_KIND_HLL;
			break;
		case KS_KIND_TOPN:
			name = "topn";
			kind = KS_KIND_TOPN;
			break;
		case KS_KIND_TDIGEST:
			name = "tdigest";
			kind = KS_KIND_TDIGEST;
			break;
		case KS_KIND_TDIGEST_PCT:
			name = "tdigest_state";
			kind = KS_KIND_TDIGEST;
			break;
		default:
			ks_invalid_value("sketch", "unknown kind of sketch");
			return (Datum) 0;	/* not reached */
	}
	if (len > ks_max_value_bytes(kind))
	{
		snprintf(why, KS_WHY_LEN, "the value is longer than any valid value of the type");
		ok = false;
	}
	else if (kind == KS_KIND_HLL)
		ok = hll_validate(p, len, why);
	else if (kind == KS_KIND_TOPN)
		ok = topn_validate(p, len, why);
	else
		ok = tdigest_validate_state(p, len, why);
	if (!ok)
		ks_invalid_value(name, why);
	PG_RETURN_TEXT_P(cstring_to_text(name));
}
