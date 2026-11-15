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
 * SPDX-License-Identifier: PostgreSQL
 *
 * IDENTIFICATION
 *	  contrib/kingres_sketch/src/kingres_sketch.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "catalog/pg_collation.h"
#include "catalog/pg_type.h"
#include "libpq/pqformat.h"
#include "nodes/miscnodes.h"
#include "utils/array.h"
#include "utils/builtins.h"
#include "utils/fmgrprotos.h"
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

const uint8 *
ks_arg_bytes(FunctionCallInfo fcinfo, int argno, Size *len)
{
	struct varlena *v = pg_detoast_datum_packed((struct varlena *) PG_GETARG_POINTER(argno));

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

static void
selftest_add(StringInfo buf, const char *name, Datum hash)
{
	appendStringInfo(buf, " %s=%016llx", name,
					 (unsigned long long) ks_mix64((uint64) DatumGetInt64(hash)));
}

/*
 * self_test() returns one line that depends on the extension version, the
 * format version, the hash of a fixed value of each type that the sketches
 * accept, and an hll estimate over a fixed set.  Two nodes that give a
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
