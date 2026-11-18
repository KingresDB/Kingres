/*-------------------------------------------------------------------------
 *
 * sign.c
 *	  Client signatures: the key registry, the attestation of one ledger entry,
 *	  the signature policy of a table, and the registration of the tables that
 *	  hold them (kingres/docs/design/ledger-phase7.md, 3.6 to 3.8 and 8.1).
 *
 * A client attestation is an Ed25519 signature, made with a key the client
 * holds, over one ledger entry of one row version: "the holder of this key
 * wrote (or removed) this row version in this chain".  The server never signs
 * and never sees a private key.  The registry of keys, the revocations and the
 * attestations are rows of three tables of the extension that are ledger
 * tables themselves (kingres.ledger_client_key, ledger_client_key_revocation,
 * ledger_attestation): they are registered in kingres.ledger_table by the
 * script of the extension (ledger_register_internal below), and the capture
 * trigger enters their rows into the chain like those of any ledger table, so
 * the blocks, the proofs and ledger_verify cover them and no new hash tag
 * exists.  They are written by this file and by nothing else: the guard
 * (guard.c) refuses every other statement that writes them, whatever the role
 * inherits, because their names start with "ledger_" and they belong to the
 * extension; every function here that writes one runs a fixed statement with
 * the marker of the guard raised (kl_run) and computes or checks every value it
 * writes, and no function takes the content of a row as an argument that it
 * would store unchecked.
 *
 * None of the functions of this file takes a lock of the extension: the rows
 * have no identifier that has to be allocated (the keys are identified by the
 * hash of the public key, an attestation by the entry it attests), so the
 * rule that a role that is not a superuser calls a lock-taking function only
 * as a statement of its own does not apply to them, and ledger_attest must run
 * inside the transaction that wrote the entry.  ledger_register_internal takes
 * the lock of the registry, as ledger_register_table does, and only the script
 * of the extension (a superuser) can call it.
 *
 * Without OpenSSL the extension builds and everything but the verification of a
 * signature works: ledger_attest raises 0A000.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/sign.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/transam.h"
#include "access/xact.h"
#include "catalog/dependency.h"
#include "catalog/namespace.h"
#include "catalog/pg_class.h"
#include "catalog/pg_type.h"
#include "commands/extension.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "mb/pg_wchar.h"
#include "miscadmin.h"
#include "utils/acl.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"
#include "utils/syscache.h"

#include "kingres_ledger.h"
#include "merkle.h"
#include "row_hash.h"

/*
 * KL_NO_OPENSSL is for building the extension as if the server had no OpenSSL
 * (the test of the 0A000 refusal, in the README): the macro is not set in any
 * real build.
 */
#if defined(USE_OPENSSL) && !defined(KL_NO_OPENSSL)
#define KL_HAVE_SIGN 1
#include <openssl/err.h>
#include <openssl/evp.h>
#endif

PG_FUNCTION_INFO_V1(kingres_ledger_attestation_message);
PG_FUNCTION_INFO_V1(kingres_ledger_client_key_register);
PG_FUNCTION_INFO_V1(kingres_ledger_client_key_revoke);
PG_FUNCTION_INFO_V1(kingres_ledger_attest);
PG_FUNCTION_INFO_V1(kingres_ledger_set_signature_policy);
PG_FUNCTION_INFO_V1(kingres_ledger_register_internal);

/*
 * The domain tag of the message a client signs, with the zero byte that ends
 * it (the sizeof of the array counts that byte): "kingres-ledger/client-
 * signature/v1" || 0x00.  A signature made for another purpose, or by another
 * version of this format, cannot be mistaken for one of these.
 */
static const char kl_msg_tag[] = "kingres-ledger/client-signature/v1";

/* Length of the message: tag and zero, chain, table, op, row hash, key, nonce. */
#define KL_MSG_LEN (sizeof(kl_msg_tag) + KL_CHAIN_ID_LEN + 4 + 1 + KL_HASH_LEN + \
					KL_CLIENT_KEY_ID_LEN + KL_CLIENT_NONCE_LEN)

/* The text of the CREATE TRIGGER and ALTER TABLE that ledger_register_internal runs. */
static char *kl_internal_stmt = NULL;

/*
 * kl_is_internal_table_name
 *		Whether the name is one of the three tables of client signatures.
 */
bool
kl_is_internal_table_name(const char *name)
{
	return strcmp(name, KL_TABLE_CLIENT_KEY) == 0 ||
		strcmp(name, KL_TABLE_CLIENT_REVOKE) == 0 ||
		strcmp(name, KL_TABLE_ATTESTATION) == 0;
}

/*
 * kl_internal_stmt_active
 *		Whether this is the text of the statement that ledger_register_internal
 *		is running now (protect.c lets exactly that statement through, and only
 *		while the script of the extension runs).
 */
bool
kl_internal_stmt_active(const char *queryString)
{
	return kl_internal_stmt != NULL && queryString != NULL &&
		strcmp(queryString, kl_internal_stmt) == 0;
}

/* Raise 22023 unless the argument has exactly "len" bytes. */
static const uint8 *
kl_exact(bytea *b, int len, const char *what)
{
	if (VARSIZE_ANY_EXHDR(b) != len)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("%s must be %d bytes, not %d", what, len,
						(int) VARSIZE_ANY_EXHDR(b))));
	return (const uint8 *) VARDATA_ANY(b);
}

/* Raise 22004 when an argument is NULL: a call that does nothing must not look done. */
static void
kl_not_null(FunctionCallInfo fcinfo, int nargs)
{
	int			i;

	for (i = 0; i < nargs; i++)
		if (PG_ARGISNULL(i))
			ereport(ERROR,
					(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
					 errmsg("argument %d must not be NULL", i + 1)));
}

/*
 * kl_build_message
 *		The bytes a client signs (README, "Client signatures"): the tag, a zero
 *		byte, the chain identity, u32 table_id, the operation byte, the row
 *		hash, the key identifier and the nonce; integers big-endian.
 */
static void
kl_build_message(uint8 *out, const uint8 *chain_id, int32 table_id, char op,
				 const uint8 *row_hash, const uint8 *key_id, const uint8 *nonce)
{
	uint8	   *p = out;

	memcpy(p, kl_msg_tag, sizeof(kl_msg_tag));
	p += sizeof(kl_msg_tag);
	memcpy(p, chain_id, KL_CHAIN_ID_LEN);
	p += KL_CHAIN_ID_LEN;
	kl_put_u32(p, (uint32) table_id);
	p += 4;
	*p++ = (uint8) op;
	memcpy(p, row_hash, KL_HASH_LEN);
	p += KL_HASH_LEN;
	memcpy(p, key_id, KL_CLIENT_KEY_ID_LEN);
	p += KL_CLIENT_KEY_ID_LEN;
	memcpy(p, nonce, KL_CLIENT_NONCE_LEN);
	p += KL_CLIENT_NONCE_LEN;
	Assert(p - out == (ptrdiff_t) KL_MSG_LEN);
}

/*
 * kingres.ledger_attestation_message(chain_id bytea, table_id int, op "char",
 *									  row_hash bytea, key_id bytea,
 *									  nonce bytea) returns bytea
 *		The message a client signs, for a client that wants to compare its own
 *		encoding with the server's.  A pure function of its arguments.
 */
Datum
kingres_ledger_attestation_message(PG_FUNCTION_ARGS)
{
	const uint8 *chain = kl_exact(PG_GETARG_BYTEA_PP(0), KL_CHAIN_ID_LEN, "chain_id");
	int32		table_id = PG_GETARG_INT32(1);
	char		op = PG_GETARG_CHAR(2);
	const uint8 *row_hash = kl_exact(PG_GETARG_BYTEA_PP(3), KL_HASH_LEN, "row_hash");
	const uint8 *key_id = kl_exact(PG_GETARG_BYTEA_PP(4), KL_CLIENT_KEY_ID_LEN, "key_id");
	const uint8 *nonce = kl_exact(PG_GETARG_BYTEA_PP(5), KL_CLIENT_NONCE_LEN, "nonce");
	bytea	   *result;

	if (table_id < 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("table_id must not be negative")));
	if (op != KL_OP_INSERT && op != KL_OP_DELETE)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("operation must be I or D")));
	result = (bytea *) palloc(VARHDRSZ + KL_MSG_LEN);
	SET_VARSIZE(result, VARHDRSZ + KL_MSG_LEN);
	kl_build_message((uint8 *) VARDATA(result), chain, table_id, op, row_hash,
					 key_id, nonce);
	PG_RETURN_BYTEA_P(result);
}

#ifdef KL_HAVE_SIGN
/*
 * kl_ed25519_verify
 *		Plain Ed25519 (RFC 8032) over the message, with OpenSSL's EVP_DigestVerify.
 *		A public key that OpenSSL cannot load, or a signature that does not
 *		verify, gives false.
 */
static bool
kl_ed25519_verify(const uint8 *pub, const uint8 *msg, size_t msglen,
				  const uint8 *sig)
{
	EVP_PKEY   *pkey;
	EVP_MD_CTX *ctx;
	bool		ok = false;

	ERR_clear_error();
	pkey = EVP_PKEY_new_raw_public_key(EVP_PKEY_ED25519, NULL, pub,
									   KL_CLIENT_PUBKEY_LEN);
	if (pkey == NULL)
	{
		ERR_clear_error();
		return false;
	}
	ctx = EVP_MD_CTX_new();
	if (ctx == NULL)
	{
		EVP_PKEY_free(pkey);
		ereport(ERROR,
				(errcode(ERRCODE_OUT_OF_MEMORY),
				 errmsg("out of memory")));
	}
	if (EVP_DigestVerifyInit(ctx, NULL, NULL, NULL, pkey) == 1)
		ok = (EVP_DigestVerify(ctx, sig, KL_CLIENT_SIG_LEN, msg, msglen) == 1);
	EVP_MD_CTX_free(ctx);
	EVP_PKEY_free(pkey);
	ERR_clear_error();
	return ok;
}
#endif							/* KL_HAVE_SIGN */

/* Raise an error unless the text has at most KL_CLIENT_TEXT_MAX characters. */
static void
kl_check_text(text *t, const char *what)
{
	int			chars = pg_mbstrlen_with_len(VARDATA_ANY(t), VARSIZE_ANY_EXHDR(t));

	if (chars > KL_CLIENT_TEXT_MAX)
		ereport(ERROR,
				(errcode(ERRCODE_STRING_DATA_RIGHT_TRUNCATION),
				 errmsg("%s is %d characters long; the limit is %d", what, chars,
						KL_CLIENT_TEXT_MAX)));
}

/* The name of the role that called the SECURITY DEFINER function. */
static void
kl_caller_name(NameData *name)
{
	namestrcpy(name, GetUserNameFromId(GetOuterUserId(), false));
}

#ifdef KL_HAVE_SIGN
/*
 * The statements of ledger_attest, prepared on first use and kept: the call is
 * on the path of a write, and the parse and plan of four statements cost more
 * than the signature (the first version ran them as text: about 400 microseconds
 * a call, measured).  Saved plans are re-planned when the tables change.
 */
static SPIPlanPtr kl_plan_chain = NULL;
static SPIPlanPtr kl_plan_entry = NULL;
static SPIPlanPtr kl_plan_key = NULL;
static SPIPlanPtr kl_plan_insert = NULL;

/*
 * kl_exec_prepared
 *		Run a saved plan in the SPI connection of the caller; a "guarded" one
 *		writes a ledger table and runs with the write marker raised (guard.c),
 *		which is lowered on every way out, as kl_run does for statements of text.
 */
static void
kl_exec_prepared(SPIPlanPtr *slot, const char *sql, int nargs, Oid *types,
				 Datum *values, bool read_only, bool guarded)
{
	int			rc = 0;

	if (*slot == NULL)
	{
		SPIPlanPtr	plan = SPI_prepare(sql, nargs, types);

		if (plan == NULL)
			elog(ERROR, "SPI_prepare failed: %s", SPI_result_code_string(SPI_result));
		if (SPI_keepplan(plan))
			elog(ERROR, "SPI_keepplan failed");
		*slot = plan;
	}
	if (guarded)
		kl_write_enter();
	PG_TRY();
	{
		rc = SPI_execute_plan(*slot, values, NULL, read_only, 0);
	}
	PG_FINALLY();
	{
		if (guarded)
			kl_write_leave();
	}
	PG_END_TRY();
	if (rc < 0)
		elog(ERROR, "ledger statement failed: %s", SPI_result_code_string(rc));
}
#endif							/* KL_HAVE_SIGN */

/*
 * kingres.ledger_client_key_register(public_key bytea, role_name name,
 *									  label text) returns bytea
 *		Register a client key: the identifier is the first 16 bytes of the
 *		SHA-256 of the public key.  role_name NULL lets any role attest with the
 *		key, a role name only that role.  The same key cannot be registered
 *		twice, and a revoked key is not registered again.  The registration is
 *		a row of a ledger table, entered into the chain by the capture trigger
 *		in this transaction.  Not executable by PUBLIC.
 */
Datum
kingres_ledger_client_key_register(PG_FUNCTION_ARGS)
{
	const uint8 *pub;
	uint8		digest[KL_HASH_LEN];
	bytea	   *key_id;
	NameData	by;
	Datum		values[5];
	Oid			types[5];
	char		nulls[5];
	text	   *label;

	if (PG_ARGISNULL(0) || PG_ARGISNULL(2))
		ereport(ERROR,
				(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
				 errmsg("the public key and the label must not be NULL")));
	pub = kl_exact(PG_GETARG_BYTEA_PP(0), KL_CLIENT_PUBKEY_LEN, "public_key");
	label = PG_GETARG_TEXT_PP(2);
	kl_check_text(label, "label");
	if (!PG_ARGISNULL(1) &&
		!OidIsValid(get_role_oid(NameStr(*PG_GETARG_NAME(1)), true)))
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_OBJECT),
				 errmsg("role \"%s\" does not exist", NameStr(*PG_GETARG_NAME(1)))));

	kl_sha256(digest, pub, KL_CLIENT_PUBKEY_LEN);
	key_id = (bytea *) palloc(VARHDRSZ + KL_CLIENT_KEY_ID_LEN);
	SET_VARSIZE(key_id, VARHDRSZ + KL_CLIENT_KEY_ID_LEN);
	memcpy(VARDATA(key_id), digest, KL_CLIENT_KEY_ID_LEN);
	kl_caller_name(&by);

	memset(nulls, ' ', sizeof(nulls));
	types[0] = BYTEAOID;
	values[0] = PointerGetDatum(key_id);
	types[1] = BYTEAOID;
	values[1] = PG_GETARG_DATUM(0);
	types[2] = NAMEOID;
	if (PG_ARGISNULL(1))
	{
		values[2] = (Datum) 0;
		nulls[2] = 'n';
	}
	else
		values[2] = PG_GETARG_DATUM(1);
	types[3] = TEXTOID;
	values[3] = PG_GETARG_DATUM(2);
	types[4] = NAMEOID;
	values[4] = NameGetDatum(&by);

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	kl_run("INSERT INTO kingres.ledger_client_key "
		   "(key_id, algorithm, public_key, role_name, label, registered_by, registered_at) "
		   "VALUES ($1, 'ed25519', $2, $3, $4, $5, pg_catalog.clock_timestamp()) "
		   "ON CONFLICT (key_id) DO NOTHING",
		   5, types, values, nulls, true);
	if (SPI_processed == 0)
		ereport(ERROR,
				(errcode(ERRCODE_UNIQUE_VIOLATION),
				 errmsg("this client key is registered already")));
	SPI_finish();
	PG_RETURN_BYTEA_P(key_id);
}

/*
 * kingres.ledger_client_key_revoke(key_id bytea, reason text) returns void
 *		Revoke a client key: a row of a ledger table.  The attestations made
 *		before the block that holds the revocation stay valid (README).  An
 *		unknown key and a key that is revoked already are KL012.
 */
Datum
kingres_ledger_client_key_revoke(PG_FUNCTION_ARGS)
{
	bytea	   *key_id;
	text	   *reason;
	NameData	by;
	Datum		values[3];
	Oid			types[3];

	if (PG_ARGISNULL(0) || PG_ARGISNULL(1))
		ereport(ERROR,
				(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
				 errmsg("the key identifier and the reason must not be NULL")));
	key_id = PG_GETARG_BYTEA_PP(0);
	(void) kl_exact(key_id, KL_CLIENT_KEY_ID_LEN, "key_id");
	reason = PG_GETARG_TEXT_PP(1);
	kl_check_text(reason, "reason");
	kl_caller_name(&by);

	types[0] = BYTEAOID;
	values[0] = PointerGetDatum(key_id);
	types[1] = TEXTOID;
	values[1] = PointerGetDatum(reason);
	types[2] = NAMEOID;
	values[2] = NameGetDatum(&by);

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	kl_run("SELECT 1 FROM kingres.ledger_client_key k WHERE k.key_id OPERATOR(pg_catalog.=) $1",
		   1, types, values, NULL, false);
	if (SPI_processed == 0)
		ereport(ERROR,
				(errcode(ERRCODE_KL_KEY),
				 errmsg("no client key with this identifier is registered")));
	kl_run("INSERT INTO kingres.ledger_client_key_revocation "
		   "(key_id, reason, revoked_by, revoked_at) "
		   "VALUES ($1, $2, $3, pg_catalog.clock_timestamp()) "
		   "ON CONFLICT (key_id) DO NOTHING",
		   3, types, values, NULL, true);
	if (SPI_processed == 0)
		ereport(ERROR,
				(errcode(ERRCODE_KL_KEY),
				 errmsg("this client key is revoked already")));
	SPI_finish();
	PG_RETURN_VOID();
}

/*
 * kingres.ledger_attest(relation regclass, row_hash bytea, op "char",
 *						 key_id bytea, nonce bytea, signature bytea) returns void
 *		Accept the client's attestation of one entry of the current
 *		transaction: the entry is found (the table, the hash and the operation,
 *		the first one of this transaction that has no attestation yet), the key
 *		must be registered and not revoked and, when it names a role, the role
 *		that called must be that one, the Ed25519 signature must verify over
 *		the message of kl_build_message, and the (key, nonce) pair must not have
 *		been used before.  The attestation is then a row of a ledger table, and
 *		the capture trigger enters it into the chain in this transaction.
 *
 *		KL011  no such entry in this transaction (or all of them are attested)
 *		KL012  the key is unknown, revoked, or not allowed for this role
 *		KL013  the signature does not verify
 *		KL014  the nonce was used with this key before
 *
 *		The key and its revocation are read with the latest snapshot, whatever
 *		the isolation level of the caller: a key that was revoked and committed
 *		is not accepted by a transaction that began before it.
 */
Datum
kingres_ledger_attest(PG_FUNCTION_ARGS)
{
#ifndef KL_HAVE_SIGN
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("client signatures need a server built with OpenSSL")));
	PG_RETURN_VOID();
#else
	Oid			relid;
	const uint8 *row_hash;
	char		op;
	const uint8 *key_id;
	const uint8 *nonce;
	const uint8 *sig;
	bytea	   *b_hash;
	bytea	   *b_key;
	bytea	   *b_nonce;
	bytea	   *b_sig;
	int32		table_id;
	bool		required;
	FullTransactionId fxid;
	int64		txid;
	Datum		values[5];
	Oid			types[5];
	Datum		d;
	uint8		chain[KL_CHAIN_ID_LEN];
	uint8		pub[KL_CLIENT_PUBKEY_LEN];
	uint8		msg[KL_MSG_LEN];
	int32		seq;
	char	   *relname;
	char	   *keyrole = NULL;
	bool		revoked;
	bool		isnull;

	kl_not_null(fcinfo, 6);
	relid = PG_GETARG_OID(0);
	b_hash = PG_GETARG_BYTEA_PP(1);
	row_hash = kl_exact(b_hash, KL_HASH_LEN, "row_hash");
	op = PG_GETARG_CHAR(2);
	b_key = PG_GETARG_BYTEA_PP(3);
	key_id = kl_exact(b_key, KL_CLIENT_KEY_ID_LEN, "key_id");
	b_nonce = PG_GETARG_BYTEA_PP(4);
	nonce = kl_exact(b_nonce, KL_CLIENT_NONCE_LEN, "nonce");
	b_sig = PG_GETARG_BYTEA_PP(5);
	if (op != KL_OP_INSERT && op != KL_OP_DELETE)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("operation must be I or D")));

	relname = get_rel_name(relid);
	if (relname == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("relation with OID %u does not exist", relid)));
	table_id = kl_lookup_table_id_policy(relid, &required);
	if (table_id < 0)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("table \"%s\" is not under ledger control", relname)));
	if (get_rel_namespace(relid) == get_namespace_oid(KL_SCHEMA_NAME, true) &&
		kl_is_internal_table_name(relname))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("the rows of table \"%s\", a table of the ledger itself, are not attested",
						relname)));

	fxid = GetTopFullTransactionIdIfAny();
	if (!FullTransactionIdIsValid(fxid))
		ereport(ERROR,
				(errcode(ERRCODE_KL_NO_ENTRY),
				 errmsg("this transaction has written no ledger entry")));
	txid = (int64) U64FromFullTransactionId(fxid);

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");

	/* The identity of the chain: a signature is useless in another chain. */
	kl_exec_prepared(&kl_plan_chain, "SELECT c.chain_id FROM kingres.ledger_chain c",
					 0, NULL, NULL, false, false);
	if (!kl_first(1, &d) || VARSIZE_ANY_EXHDR(DatumGetByteaPP(d)) != KL_CHAIN_ID_LEN)
		ereport(ERROR,
				(errcode(ERRCODE_DATA_CORRUPTED),
				 errmsg("the identity of the chain is missing or malformed")));
	memcpy(chain, VARDATA_ANY(DatumGetByteaPP(d)), KL_CHAIN_ID_LEN);

	/* The entry: the first one of this transaction with this hash and no attestation. */
	types[0] = INT8OID;
	values[0] = Int64GetDatum(txid);
	types[1] = INT4OID;
	values[1] = Int32GetDatum(table_id);
	types[2] = BYTEAOID;
	values[2] = PointerGetDatum(b_hash);
	types[3] = CHAROID;
	values[3] = CharGetDatum(op);
	kl_exec_prepared(&kl_plan_entry,
					 "SELECT e.seq FROM kingres.ledger_entry e "
					 "WHERE e.table_id OPERATOR(pg_catalog.=) $2 "
					 "AND e.row_hash OPERATOR(pg_catalog.=) $3 "
					 "AND e.op OPERATOR(pg_catalog.=) $4 "
					 "AND e.ledger_txid OPERATOR(pg_catalog.=) $1 "
					 "AND NOT EXISTS (SELECT 1 FROM kingres.ledger_attestation a "
					 "WHERE a.ledger_txid OPERATOR(pg_catalog.=) e.ledger_txid "
					 "AND a.seq OPERATOR(pg_catalog.=) e.seq) "
					 "ORDER BY e.seq LIMIT 1",
					 4, types, values, false, false);
	if (!kl_first(1, &d))
	{
		kl_run("SELECT 1 FROM kingres.ledger_entry e "
			   "WHERE e.table_id OPERATOR(pg_catalog.=) $2 "
			   "AND e.row_hash OPERATOR(pg_catalog.=) $3 "
			   "AND e.op OPERATOR(pg_catalog.=) $4 "
			   "AND e.ledger_txid OPERATOR(pg_catalog.=) $1",
			   4, types, values, NULL, false);
		if (SPI_processed > 0)
			ereport(ERROR,
					(errcode(ERRCODE_KL_NO_ENTRY),
					 errmsg("every ledger entry of this transaction for this row version has an attestation already")));
		ereport(ERROR,
				(errcode(ERRCODE_KL_NO_ENTRY),
				 errmsg("this transaction has written no ledger entry of operation %c with this row hash for table \"%s\"",
						op, relname),
				 errhint("The entry exists only after the statement that writes the row; call ledger_attest after it, in the same transaction, with the hash of the row as the server computes it.")));
	}
	seq = DatumGetInt32(d);

	/* The key and its revocation, as of now. */
	types[0] = BYTEAOID;
	values[0] = PointerGetDatum(b_key);
	PushActiveSnapshot(GetLatestSnapshot());
	kl_exec_prepared(&kl_plan_key,
					 "SELECT k.public_key, k.role_name::pg_catalog.text, "
					 "EXISTS (SELECT 1 FROM kingres.ledger_client_key_revocation r "
					 "WHERE r.key_id OPERATOR(pg_catalog.=) k.key_id) "
					 "FROM kingres.ledger_client_key k "
					 "WHERE k.key_id OPERATOR(pg_catalog.=) $1",
					 1, types, values, true, false);
	PopActiveSnapshot();
	if (SPI_processed == 0)
		ereport(ERROR,
				(errcode(ERRCODE_KL_KEY),
				 errmsg("no client key with this identifier is registered")));
	d = SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull);
	if (isnull || VARSIZE_ANY_EXHDR(DatumGetByteaPP(d)) != KL_CLIENT_PUBKEY_LEN)
		ereport(ERROR,
				(errcode(ERRCODE_DATA_CORRUPTED),
				 errmsg("the registered public key is missing or malformed")));
	memcpy(pub, VARDATA_ANY(DatumGetByteaPP(d)), KL_CLIENT_PUBKEY_LEN);
	d = SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 2, &isnull);
	if (!isnull)
		keyrole = TextDatumGetCString(d);
	d = SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 3, &isnull);
	revoked = !isnull && DatumGetBool(d);
	if (revoked)
		ereport(ERROR,
				(errcode(ERRCODE_KL_KEY),
				 errmsg("this client key is revoked")));
	if (keyrole != NULL &&
		strcmp(keyrole, GetUserNameFromId(GetOuterUserId(), false)) != 0)
		ereport(ERROR,
				(errcode(ERRCODE_KL_KEY),
				 errmsg("this client key may be used only by role \"%s\"", keyrole)));

	/* The signature. */
	if (VARSIZE_ANY_EXHDR(b_sig) != KL_CLIENT_SIG_LEN)
		ereport(ERROR,
				(errcode(ERRCODE_KL_SIGNATURE),
				 errmsg("the signature must be %d bytes, not %d", KL_CLIENT_SIG_LEN,
						(int) VARSIZE_ANY_EXHDR(b_sig))));
	sig = (const uint8 *) VARDATA_ANY(b_sig);
	kl_build_message(msg, chain, table_id, op, row_hash, key_id, nonce);
	if (!kl_ed25519_verify(pub, msg, sizeof(msg), sig))
		ereport(ERROR,
				(errcode(ERRCODE_KL_SIGNATURE),
				 errmsg("the signature does not verify"),
				 errhint("kingres.ledger_attestation_message gives the bytes that are signed.")));

	/* The row; a nonce that was used with this key before conflicts. */
	types[0] = INT8OID;
	values[0] = Int64GetDatum(txid);
	types[1] = INT4OID;
	values[1] = Int32GetDatum(seq);
	types[2] = BYTEAOID;
	values[2] = PointerGetDatum(b_key);
	types[3] = BYTEAOID;
	values[3] = PointerGetDatum(b_nonce);
	types[4] = BYTEAOID;
	values[4] = PointerGetDatum(b_sig);
	kl_exec_prepared(&kl_plan_insert,
					 "INSERT INTO kingres.ledger_attestation "
					 "(ledger_txid, seq, key_id, nonce, signature) "
					 "VALUES ($1, $2, $3, $4, $5) ON CONFLICT (key_id, nonce) DO NOTHING",
					 5, types, values, false, true);
	if (SPI_processed == 0)
		ereport(ERROR,
				(errcode(ERRCODE_KL_NONCE),
				 errmsg("this nonce was used with this client key before"),
				 errhint("A nonce is 16 random bytes that the client chooses for each attestation.")));
	SPI_finish();

	/* The entry is no longer owed an attestation (a table with the policy "required"). */
	kl_attest_mark(seq);
	PG_RETURN_VOID();
#endif
}

/*
 * kingres.ledger_set_signature_policy(relation regclass, policy text) returns void
 *		Switch a ledger table from the policy "optional" to "required": from
 *		then on a transaction that writes an entry of the table and does not
 *		attest it fails at commit (KL010, capture.c).  There is no way back, as
 *		there is none for the mode of a table.  The caller owns the table (or is
 *		a member of its owner).  The row of the registry that carries the policy
 *		is the one row of a ledger catalog table that the code of the extension
 *		updates (guard.c, kl_policy_update_allowed), and the transaction that
 *		switches it is recorded in the row, so that ledger_verify asks for an
 *		attestation only of the entries from then on.  The tables of the ledger
 *		itself have no policy.
 */
Datum
kingres_ledger_set_signature_policy(PG_FUNCTION_ARGS)
{
	Oid			relid;
	char	   *policy;
	char	   *relname;
	int32		table_id;
	bool		required;
	Datum		values[2];
	Oid			types[2];

	kl_not_null(fcinfo, 2);
	relid = PG_GETARG_OID(0);
	policy = text_to_cstring(PG_GETARG_TEXT_PP(1));
	if (strcmp(policy, "optional") != 0 && strcmp(policy, "required") != 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("signature policy \"%s\" is not supported", policy),
				 errhint("The policies are \"optional\" and \"required\".")));
	kl_check_owner(relid);
	relname = get_rel_name(relid);
	table_id = kl_lookup_table_id_policy(relid, &required);
	if (table_id < 0)
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("table \"%s\" is not under ledger control", relname)));
	if (get_rel_namespace(relid) == get_namespace_oid(KL_SCHEMA_NAME, true) &&
		kl_is_internal_table_name(relname))
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("table \"%s\" is a table of the ledger itself and has no signature policy",
						relname)));
	if (strcmp(policy, "optional") == 0)
	{
		if (required)
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("table \"%s\" has the signature policy required, and the policy of a table cannot be relaxed",
							relname),
					 errhint("Like the mode of a ledger table, the policy is permanent.")));
		PG_RETURN_VOID();
	}
	if (required)
		PG_RETURN_VOID();

	types[0] = INT4OID;
	values[0] = Int32GetDatum(table_id);
	types[1] = INT8OID;
	values[1] = Int64GetDatum((int64) U64FromFullTransactionId(GetTopFullTransactionId()));
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	kl_run("UPDATE kingres.ledger_table "
		   "SET signature_policy = 'required', signature_required_from = $2 "
		   "WHERE table_id OPERATOR(pg_catalog.=) $1 AND signature_policy OPERATOR(pg_catalog.=) 'optional'",
		   2, types, values, NULL, true);
	SPI_finish();
	/* The answer that this transaction cached for the table is out of date. */
	kl_table_cache_reset();
	PG_RETURN_VOID();
}

/*
 * kl_run_internal
 *		Run one fixed statement for ledger_register_internal.  protect.c lets
 *		that text through, and no other, while this function runs inside the
 *		script of the extension (kl_internal_stmt_active).
 */
static void
kl_run_internal(const char *sql)
{
	int			rc = 0;

	kl_internal_stmt = pstrdup(sql);
	PG_TRY();
	{
		rc = SPI_execute(sql, false, 0);
	}
	PG_FINALLY();
	{
		kl_internal_stmt = NULL;
	}
	PG_END_TRY();
	if (rc != SPI_OK_UTILITY)
		elog(ERROR, "ledger statement failed: %s", SPI_result_code_string(rc));
}

/*
 * kingres.ledger_register_internal(relation regclass) returns void
 *		For the install and upgrade scripts of the extension only: register one
 *		of the three tables of client signatures, which the script has created
 *		with their blocking triggers, as an append-only ledger table.  It takes
 *		the next identifier of the registry under the registry's lock (no
 *		sequence is read), arms the capture trigger with that identifier, and
 *		writes the row of the registry; then it checks the table with
 *		ledger_table_defects like ledger_register_table does.  The table holds
 *		no row yet, so no entry is written, and the upgrade of a ledger writes
 *		none.  The identifier cannot be a literal of the script: a database that
 *		upgrades from 0.2 has tables that use the first ones.
 *
 *		Everything it writes it computes: it refuses any relation but those
 *		three tables of the extension, and any caller outside the script.
 */
Datum
kingres_ledger_register_internal(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	Oid			extoid = kl_extension_oid();
	char	   *relname;
	Datum		values[4];
	Oid			types[4];
	Datum		d;
	int64		tid;
	char	   *sql;

	if (!OidIsValid(extoid) || !creating_extension || CurrentExtensionObject != extoid)
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("only the install or upgrade script of extension \"%s\" can register its own tables",
						KL_EXTENSION_NAME)));
	relname = get_rel_name(relid);
	if (relname == NULL ||
		get_rel_namespace(relid) != get_namespace_oid(KL_SCHEMA_NAME, true) ||
		get_rel_relkind(relid) != RELKIND_RELATION ||
		!kl_is_internal_table_name(relname) ||
		getExtensionOfObject(RelationRelationId, relid) != extoid)
		ereport(ERROR,
				(errcode(ERRCODE_WRONG_OBJECT_TYPE),
				 errmsg("relation with OID %u is not a table of client signatures of the extension",
						relid)));

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	types[0] = REGCLASSOID;
	values[0] = ObjectIdGetDatum(relid);
	kl_run("SELECT 1 FROM kingres.ledger_table WHERE relid OPERATOR(pg_catalog.=) $1",
		   1, types, values, NULL, false);
	if (SPI_processed > 0)
		ereport(ERROR,
				(errcode(ERRCODE_DUPLICATE_OBJECT),
				 errmsg("table \"%s\" is registered already", relname)));

	kl_lock_extension_object(KL_REGISTRY_LOCK_SUBID);
	kl_run("SELECT COALESCE(pg_catalog.max(x.table_id), 0) + 1 FROM kingres.ledger_table x",
		   0, NULL, NULL, NULL, false);
	if (!kl_first(1, &d))
		elog(ERROR, "could not compute the next table identifier");
	tid = DatumGetInt64(d);
	if (tid > PG_INT32_MAX)
		ereport(ERROR,
				(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
				 errmsg("no table identifier is left")));

	/* The capture trigger, armed last (the hook treats the table as protected from then on). */
	sql = psprintf("CREATE TRIGGER %s AFTER INSERT ON %s.%s FOR EACH ROW "
				   "EXECUTE FUNCTION %s.ledger_capture('%d')",
				   KL_TRIGGER_CAPTURE, KL_SCHEMA_NAME, relname, KL_SCHEMA_NAME,
				   (int) tid);
	kl_run_internal(sql);
	sql = psprintf("ALTER TABLE %s.%s ENABLE ALWAYS TRIGGER %s",
				   KL_SCHEMA_NAME, relname, KL_TRIGGER_CAPTURE);
	kl_run_internal(sql);

	types[0] = INT4OID;
	types[1] = REGCLASSOID;
	types[2] = TEXTOID;
	values[0] = Int32GetDatum((int32) tid);
	values[1] = ObjectIdGetDatum(relid);
	values[2] = CStringGetTextDatum("append_only");
	kl_run("INSERT INTO kingres.ledger_table (table_id, relid, mode, history_relid) "
		   "OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, NULL)",
		   3, types, values, NULL, true);
	kl_follow_sequence("kingres.ledger_table", "table_id", tid);

	/* Whatever the script was meant to make, this is what it made. */
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
	SPI_finish();
	PG_RETURN_VOID();
}
