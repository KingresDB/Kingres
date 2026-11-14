/*-------------------------------------------------------------------------
 *
 * proof.c
 *	  Offline check of a ledger inclusion proof:
 *	  kingres.ledger_verify_proof(proof jsonb, digest jsonb).
 *
 * The check reads no table.  It recomputes, from the content of the proof
 * only:
 *
 *	  0. that the proof and the digest name the same chain (chain_id), and, if
 *		 the caller passes them, that the table id and the row hash are the
 *		 ones the caller computed from the row it holds;
 *	  1. the leaf hash of the entry from (seq, table_id, op, row_hash) and the
 *		 transaction root from the entry's audit path;
 *	  2. the leaf hash of the transaction from (ledger_txid, entry_count,
 *		 transaction root, user_name) and the block's transaction root from
 *		 the transaction's audit path;
 *	  3. the block hash from the block header (for block 0 also that its
 *		 prev_hash is derived from chain_id);
 *	  4. the chain of block headers from that block up to the block named by
 *		 the trusted digest, and compares the final hash with the digest.
 *
 * Every number the verifier relies on is bound by a hash: the entry count
 * by the transaction leaf, the transaction count by the block hash, and the
 * chain by the prev_hash links.  The check proves that an entry with this
 * row hash and table id is in the ledger; it cannot know the row.  The
 * caller has to recompute the row hash from the row it holds.  A proof that
 * is malformed in any way is rejected by returning false, never by raising
 * an error, so callers can use the function on untrusted input.  Numbers
 * must be integers in range; a number that is not an integer in value is
 * rejected.  "format" must be 1, "op" must be "I" or "D" (and the one the
 * caller expects, for the forms that take it; the four-argument form expects
 * "I"), a block header must have hash_version 1, and the headers of "chain"
 * after the one the digest names are parsed under the same rules but not
 * used.  The user name is covered in UTF-8: the name in the proof is in the
 * database encoding and is converted, as ledger_block_tx_leaf does.
 *
 * Proof format (hash_version 1):
 *	  {"format":1, "chain_id":hex, "relation":text, "table_id":N,
 *	   "ledger_txid":N, "seq":N, "op":"I", "row_hash":hex, "entry_index":N,
 *	   "entry_count":N, "entry_path":[hex..], "tx_root":hex,
 *	   "user_name":text, "tx_index":N, "tx_path":[hex..],
 *	   "block":{"height","hash_version","prev_hash","tx_merkle_root",
 *				"tx_count","closed_at_us","block_hash"},
 *	   "chain":[{"height","hash_version","prev_hash","tx_merkle_root",
 *				 "tx_count","closed_at_us"}, ...]}
 * The digest is {"chain_id":hex, "height":N, "block_hash":hex, ...}; other
 * keys are ignored.  "relation" is informational and not authenticated.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: PostgreSQL
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/proof.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "fmgr.h"
#include "mb/pg_wchar.h"
#include "miscadmin.h"
#include "utils/builtins.h"
#include "utils/fmgrprotos.h"
#include "utils/jsonb.h"
#include "utils/numeric.h"
#include "utils/timestamp.h"

#include "kingres_ledger.h"
#include "merkle.h"

PG_FUNCTION_INFO_V1(kingres_ledger_verify_proof);
PG_FUNCTION_INFO_V1(kingres_ledger_timestamptz_us);

/* A block header as it appears in a proof. */
typedef struct KlHeader
{
	int64		height;
	int64		hash_version;
	uint8		prev_hash[KL_HASH_LEN];
	uint8		tx_root[KL_HASH_LEN];
	int64		tx_count;
	int64		closed_at_us;
	uint8		block_hash[KL_HASH_LEN];	/* only when present */
	bool		has_block_hash;
} KlHeader;

/* Value of one hexadecimal digit, or -1. */
static int
kl_hex_digit(char c)
{
	if (c >= '0' && c <= '9')
		return c - '0';
	if (c >= 'a' && c <= 'f')
		return c - 'a' + 10;
	if (c >= 'A' && c <= 'F')
		return c - 'A' + 10;
	return -1;
}

/* Decode exactly "nbytes" bytes from a hex string of 2 * nbytes digits. */
static bool
kl_hex_decode(const char *s, int len, uint8 *out, int nbytes)
{
	int			i;

	if (len != 2 * nbytes)
		return false;
	for (i = 0; i < nbytes; i++)
	{
		int			hi = kl_hex_digit(s[2 * i]);
		int			lo = kl_hex_digit(s[2 * i + 1]);

		if (hi < 0 || lo < 0)
			return false;
		out[i] = (uint8) (hi * 16 + lo);
	}
	return true;
}

/* Fetch member "key" of a JSON object, or NULL when absent. */
static JsonbValue *
kl_json_member(JsonbContainer *obj, const char *key, JsonbValue *scratch)
{
	if (!JsonContainerIsObject(obj))
		return NULL;
	return getKeyJsonValueFromContainer(obj, key, strlen(key), scratch);
}

/*
 * An integer member that fits in int64.  The number must be an integer: a
 * value with a fractional part is rejected, not rounded (the conversion to
 * int8 rounds, so the result is converted back and compared).
 */
static bool
kl_json_int(JsonbContainer *obj, const char *key, int64 *out)
{
	JsonbValue	scratch;
	JsonbValue *v = kl_json_member(obj, key, &scratch);
	bool		have_error = false;
	int64		n;

	if (v == NULL || v->type != jbvNumeric)
		return false;
	n = numeric_int8_opt_error(v->val.numeric, &have_error);
	if (have_error)
		return false;
	if (!DatumGetBool(DirectFunctionCall2(numeric_eq,
										  NumericGetDatum(v->val.numeric),
										  NumericGetDatum(int64_to_numeric(n)))))
		return false;
	*out = n;
	return true;
}

/* A 32-byte hash given as 64 hexadecimal digits. */
static bool
kl_json_hash(JsonbContainer *obj, const char *key, uint8 *out)
{
	JsonbValue	scratch;
	JsonbValue *v = kl_json_member(obj, key, &scratch);

	if (v == NULL || v->type != jbvString)
		return false;
	return kl_hex_decode(v->val.string.val, v->val.string.len, out,
						 KL_HASH_LEN);
}

/* A nested object or array member; returns its container or NULL. */
static JsonbContainer *
kl_json_container(JsonbContainer *obj, const char *key, JsonbValue *scratch)
{
	JsonbValue *v = kl_json_member(obj, key, scratch);

	if (v == NULL || v->type != jbvBinary)
		return NULL;
	return v->val.binary.data;
}

/*
 * kl_json_path
 *		Read an array of hex hashes into "path" (room for KL_MAX_DEPTH
 *		hashes); returns the number of hashes or -1 when malformed.
 */
static int
kl_json_path(JsonbContainer *obj, const char *key, uint8 *path)
{
	JsonbValue	scratch;
	JsonbContainer *arr = kl_json_container(obj, key, &scratch);
	JsonbIterator *it;
	JsonbValue	v;
	JsonbIteratorToken tok;
	int			n = 0;

	if (arr == NULL || !JsonContainerIsArray(arr) || JsonContainerIsScalar(arr))
		return -1;
	it = JsonbIteratorInit(arr);
	tok = JsonbIteratorNext(&it, &v, true);
	if (tok != WJB_BEGIN_ARRAY)
		return -1;
	while ((tok = JsonbIteratorNext(&it, &v, true)) == WJB_ELEM)
	{
		if (n >= KL_MAX_DEPTH || v.type != jbvString ||
			!kl_hex_decode(v.val.string.val, v.val.string.len,
						   path + (size_t) n * KL_HASH_LEN, KL_HASH_LEN))
			return -1;
		n++;
	}
	return tok == WJB_END_ARRAY ? n : -1;
}

/* Read a block header from a JSON object. */
static bool
kl_json_header(JsonbContainer *obj, bool need_block_hash, KlHeader *h)
{
	memset(h, 0, sizeof(KlHeader));
	if (!kl_json_int(obj, "height", &h->height) ||
		!kl_json_int(obj, "hash_version", &h->hash_version) ||
		!kl_json_hash(obj, "prev_hash", h->prev_hash) ||
		!kl_json_hash(obj, "tx_merkle_root", h->tx_root) ||
		!kl_json_int(obj, "tx_count", &h->tx_count) ||
		!kl_json_int(obj, "closed_at_us", &h->closed_at_us))
		return false;
	/*
	 * block_hash is required in the header of the proof's own block and
	 * optional in the elements of "chain"; when it is there it must be a
	 * well-formed hash (a malformed one is an error, not an absent member).
	 */
	{
		JsonbValue	bscratch;

		if (kl_json_member(obj, "block_hash", &bscratch) != NULL)
		{
			if (!kl_json_hash(obj, "block_hash", h->block_hash))
				return false;
			h->has_block_hash = true;
		}
	}
	if (need_block_hash && !h->has_block_hash)
		return false;
	if (h->height < 0 || h->tx_count < 0 || h->tx_count > PG_INT32_MAX ||
		h->hash_version != KL_HASH_VERSION)
		return false;
	return true;
}

/* Block hash of a header, recomputed. */
static void
kl_header_hash(const KlHeader *h, uint8 *out)
{
	kl_block_hash(out, (uint16) h->hash_version, (uint64) h->height,
				  h->prev_hash, h->tx_root, (uint32) h->tx_count,
				  h->closed_at_us);
}

/*
 * kl_chain_reaches_digest
 *		Starting from the block with hash "start_hash" at height
 *		"start_height", follow the "chain" headers and report whether the
 *		block at "digest_height" has hash "digest_hash".
 */
static bool
kl_chain_reaches_digest(JsonbContainer *proof, int64 start_height,
						const uint8 *start_hash, int64 start_closed_at_us,
						int64 digest_height, const uint8 *digest_hash)
{
	JsonbValue	scratch;
	JsonbContainer *arr = kl_json_container(proof, "chain", &scratch);
	JsonbIterator *it;
	JsonbValue	v;
	JsonbIteratorToken tok;
	uint8		prev[KL_HASH_LEN];
	int64		height = start_height;
	int64		closed_at_us = start_closed_at_us;
	bool		reached = false;
	bool		matches = false;

	if (arr == NULL || !JsonContainerIsArray(arr) || JsonContainerIsScalar(arr))
		return false;
	memcpy(prev, start_hash, KL_HASH_LEN);

	it = JsonbIteratorInit(arr);
	if (JsonbIteratorNext(&it, &v, true) != WJB_BEGIN_ARRAY)
		return false;
	while ((tok = JsonbIteratorNext(&it, &v, true)) == WJB_ELEM)
	{
		KlHeader	h;
		uint8		calc[KL_HASH_LEN];

		check_stack_depth();
		if (v.type != jbvBinary ||
			!kl_json_header(v.val.binary.data, false, &h))
			return false;

		/*
		 * Headers after the one the digest names are parsed strictly (a
		 * proof made after the digest was taken has them) but not used: they
		 * say nothing about the digest.
		 */
		if (reached)
			continue;

		if (h.height != height + 1 || memcmp(h.prev_hash, prev, KL_HASH_LEN) != 0)
			return false;
		/* Block time never goes backwards along the chain. */
		if (h.closed_at_us < closed_at_us)
			return false;
		kl_header_hash(&h, calc);
		if (h.has_block_hash && memcmp(h.block_hash, calc, KL_HASH_LEN) != 0)
			return false;
		height = h.height;
		closed_at_us = h.closed_at_us;
		memcpy(prev, calc, KL_HASH_LEN);
		if (height == digest_height)
		{
			reached = true;
			matches = (memcmp(calc, digest_hash, KL_HASH_LEN) == 0);
		}
		else if (height > digest_height)
			return false;
	}
	return tok == WJB_END_ARRAY && reached && matches;
}

/*
 * kingres.ledger_verify_proof(proof jsonb, digest jsonb) returns boolean
 *		See the file header.  Reads no table and has no side effects.
 */
Datum
kingres_ledger_verify_proof(PG_FUNCTION_ARGS)
{
	Jsonb	   *proof = PG_GETARG_JSONB_P(0);
	Jsonb	   *digest = PG_GETARG_JSONB_P(1);
	bool		have_expected = (PG_NARGS() >= 4);
	int32		expected_table_id = 0;
	const uint8 *expected_row_hash = NULL;
	uint8		expected_op = KL_OP_INSERT;	/* the four-argument form: an I entry */
	int64		format;
	char	   *uname_utf8;
	int			uname_len;
	JsonbContainer *p = &proof->root;
	JsonbContainer *d = &digest->root;
	JsonbValue	scratch;
	JsonbContainer *blk;
	JsonbValue *opv;
	JsonbValue *unamev;
	JsonbValue	uscratch;
	KlHeader	header;
	int64		table_id;
	int64		ledger_txid;
	int64		seq;
	int64		entry_index;
	int64		entry_count;
	int64		tx_index;
	int64		digest_height;
	uint8		row_hash[KL_HASH_LEN];
	uint8		tx_root[KL_HASH_LEN];
	uint8		digest_hash[KL_HASH_LEN];
	uint8		digest_chain[KL_CHAIN_ID_LEN];
	uint8		proof_chain[KL_CHAIN_ID_LEN];
	uint8		genesis[KL_HASH_LEN];
	uint8		entry_path[KL_MAX_DEPTH * KL_HASH_LEN];
	uint8		tx_path[KL_MAX_DEPTH * KL_HASH_LEN];
	uint8		leaf[KL_HASH_LEN];
	uint8		block_hash[KL_HASH_LEN];
	int			n_entry_path;
	int			n_tx_path;
	uint8		op;

	if (have_expected)
	{
		bytea	   *eh = PG_GETARG_BYTEA_PP(3);

		expected_table_id = PG_GETARG_INT32(2);
		if (VARSIZE_ANY_EXHDR(eh) != KL_HASH_LEN)
			PG_RETURN_BOOL(false);
		expected_row_hash = (const uint8 *) VARDATA_ANY(eh);
		if (PG_NARGS() == 5)
			expected_op = (uint8) PG_GETARG_CHAR(4);
	}

	if (!JsonContainerIsObject(p) || !JsonContainerIsObject(d))
		PG_RETURN_BOOL(false);

	/* The proof format; "format" is required and must be 1. */
	if (!kl_json_int(p, "format", &format) || format != 1)
		PG_RETURN_BOOL(false);

	/* The trusted digest, and the chain it belongs to. */
	if (!kl_json_int(d, "height", &digest_height) || digest_height < 0 ||
		!kl_json_hash(d, "block_hash", digest_hash) ||
		!kl_json_hash(d, "chain_id", digest_chain))
		PG_RETURN_BOOL(false);

	/* A proof of another chain is not a proof for this digest. */
	if (!kl_json_hash(p, "chain_id", proof_chain) ||
		memcmp(proof_chain, digest_chain, KL_CHAIN_ID_LEN) != 0)
		PG_RETURN_BOOL(false);

	/* The entry. */
	if (!kl_json_int(p, "table_id", &table_id) ||
		!kl_json_int(p, "ledger_txid", &ledger_txid) ||
		!kl_json_int(p, "seq", &seq) ||
		!kl_json_int(p, "entry_index", &entry_index) ||
		!kl_json_int(p, "entry_count", &entry_count) ||
		!kl_json_int(p, "tx_index", &tx_index) ||
		!kl_json_hash(p, "row_hash", row_hash) ||
		!kl_json_hash(p, "tx_root", tx_root))
		PG_RETURN_BOOL(false);
	if (table_id < 0 || table_id > PG_INT32_MAX ||
		seq < 0 || seq > PG_INT32_MAX ||
		ledger_txid < 0 ||
		entry_count < 1 || entry_count > PG_INT32_MAX ||
		entry_index < 0 || entry_index >= entry_count ||
		tx_index < 0)
		PG_RETURN_BOOL(false);

	opv = kl_json_member(p, "op", &scratch);
	if (opv == NULL || opv->type != jbvString || opv->val.string.len != 1)
		PG_RETURN_BOOL(false);
	op = (uint8) opv->val.string.val[0];
	/* Only the two operations of the ledger exist; the caller may ask for one. */
	if ((op != KL_OP_INSERT && op != KL_OP_DELETE) ||
		(have_expected && op != expected_op))
		PG_RETURN_BOOL(false);

	unamev = kl_json_member(p, "user_name", &uscratch);
	if (unamev == NULL || unamev->type != jbvString ||
		unamev->val.string.len > KL_MAX_USER_NAME_BYTES)
		PG_RETURN_BOOL(false);

	n_entry_path = kl_json_path(p, "entry_path", entry_path);
	n_tx_path = kl_json_path(p, "tx_path", tx_path);
	if (n_entry_path < 0 || n_tx_path < 0)
		PG_RETURN_BOOL(false);

	blk = kl_json_container(p, "block", &scratch);
	if (blk == NULL || !JsonContainerIsObject(blk) ||
		!kl_json_header(blk, true, &header))
		PG_RETURN_BOOL(false);
	if (tx_index >= header.tx_count)
		PG_RETURN_BOOL(false);

	/*
	 * The caller's own view of the row: the row hash is recomputed by the
	 * caller from the row it holds, and the entry leaf below covers the table
	 * id, so a proof for another table or another row is rejected here.
	 */
	if (have_expected &&
		(table_id != expected_table_id ||
		 memcmp(row_hash, expected_row_hash, KL_HASH_LEN) != 0))
		PG_RETURN_BOOL(false);

	/* The first block of a chain starts from the identity of the chain. */
	if (header.height == 0)
	{
		kl_genesis_hash(genesis, proof_chain);
		if (memcmp(header.prev_hash, genesis, KL_HASH_LEN) != 0)
			PG_RETURN_BOOL(false);
	}

	/* 1. entry -> transaction root */
	kl_tx_leaf_hash(leaf, (uint32) seq, (uint32) table_id, op, row_hash);
	if (!kl_merkle_verify_path(leaf, entry_index, entry_count, entry_path,
							   n_entry_path, tx_root))
		PG_RETURN_BOOL(false);

	/* 2. transaction -> block transaction root */
	/*
	 * The name in the proof is in the database encoding; the hash covers it
	 * in UTF-8, as ledger_block_tx_leaf computes it.
	 */
	uname_utf8 = (char *) pg_do_encoding_conversion((unsigned char *) pnstrdup(unamev->val.string.val,
																			   unamev->val.string.len),
													unamev->val.string.len,
													GetDatabaseEncoding(), PG_UTF8);
	uname_len = strlen(uname_utf8);
	if (uname_len > KL_MAX_USER_NAME_BYTES)
		PG_RETURN_BOOL(false);
	kl_block_tx_leaf_hash(leaf, (uint64) ledger_txid, (uint32) entry_count,
						  tx_root, (const uint8 *) uname_utf8, uname_len);
	if (!kl_merkle_verify_path(leaf, tx_index, header.tx_count, tx_path,
							   n_tx_path, header.tx_root))
		PG_RETURN_BOOL(false);

	/* 3. block header -> block hash */
	kl_header_hash(&header, block_hash);
	if (memcmp(block_hash, header.block_hash, KL_HASH_LEN) != 0)
		PG_RETURN_BOOL(false);

	/* 4. block -> trusted digest */
	if (digest_height == header.height)
		PG_RETURN_BOOL(memcmp(block_hash, digest_hash, KL_HASH_LEN) == 0);
	if (digest_height < header.height)
		PG_RETURN_BOOL(false);
	PG_RETURN_BOOL(kl_chain_reaches_digest(p, header.height, block_hash,
										   header.closed_at_us,
										   digest_height, digest_hash));
}

/*
 * kingres.ledger_timestamptz_us(timestamptz) returns bigint
 *		The microseconds since 2000-01-01 00:00:00 UTC that the block hash
 *		covers, so proofs can carry the exact value without rounding.
 */
Datum
kingres_ledger_timestamptz_us(PG_FUNCTION_ARGS)
{
	TimestampTz ts = PG_GETARG_TIMESTAMPTZ(0);

	if (TIMESTAMP_NOT_FINITE(ts))
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("timestamp must be finite")));
	PG_RETURN_INT64((int64) ts);
}
