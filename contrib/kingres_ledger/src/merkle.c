/*-------------------------------------------------------------------------
 *
 * merkle.c
 *	  SHA-256, RFC 6962 Merkle trees and the block hash of the ledger.
 *
 * This file holds every hash computation of the ledger except the row
 * encoding (row_hash.c): the domain-separated leaf and node hashes of
 * RFC 6962, an incremental root computation used by the aggregate
 * kingres.ledger_merkle_root, the audit path generator and checker, and
 * the leaf layouts of the transaction tree and the block tree.  The SQL
 * functions that expose these primitives are defined here as well.  The
 * formats are specified in kingres/docs/design/ledger-mvp.md ("Hashing")
 * and in the README of the extension.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/merkle.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "catalog/pg_type.h"
#include "common/cryptohash.h"
#include "fmgr.h"
#include "mb/pg_wchar.h"
#include "miscadmin.h"
#include "utils/array.h"
#include "utils/builtins.h"
#include "utils/timestamp.h"

#include "kingres_ledger.h"
#include "merkle.h"

PG_FUNCTION_INFO_V1(kingres_ledger_leaf_hash);
PG_FUNCTION_INFO_V1(kingres_ledger_node_hash);
PG_FUNCTION_INFO_V1(kingres_ledger_merkle_accum);
PG_FUNCTION_INFO_V1(kingres_ledger_merkle_final);
PG_FUNCTION_INFO_V1(kingres_ledger_merkle_path);
PG_FUNCTION_INFO_V1(kingres_ledger_tx_leaf);
PG_FUNCTION_INFO_V1(kingres_ledger_block_tx_leaf);
PG_FUNCTION_INFO_V1(kingres_ledger_block_hash);
PG_FUNCTION_INFO_V1(kingres_ledger_block_preimage);
PG_FUNCTION_INFO_V1(kingres_ledger_genesis_hash);

/*
 * kl_sha256_parts
 *		SHA-256 of the concatenation of "nparts" byte ranges.
 *
 * All hashing of the extension goes through this function, which uses the
 * pg_cryptohash API (OpenSSL when available, the built-in implementation
 * otherwise).  Failure of the hash library is reported as an error and
 * never as a wrong hash.
 */
static void
kl_sha256_parts(uint8 *out, int nparts, const uint8 *const *parts,
				const size_t *lens)
{
	pg_cryptohash_ctx *ctx;
	bool		ok;
	int			i;

	ctx = pg_cryptohash_create(PG_SHA256);
	if (ctx == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_OUT_OF_MEMORY),
				 errmsg("out of memory")));

	ok = (pg_cryptohash_init(ctx) == 0);
	for (i = 0; ok && i < nparts; i++)
		ok = (pg_cryptohash_update(ctx, parts[i], lens[i]) == 0);
	if (ok)
		ok = (pg_cryptohash_final(ctx, out, KL_HASH_LEN) == 0);

	if (!ok)
	{
		const char *msg = pg_cryptohash_error(ctx);
		char	   *copy = pstrdup(msg ? msg : "unknown error");

		pg_cryptohash_free(ctx);
		ereport(ERROR,
				(errcode(ERRCODE_INTERNAL_ERROR),
				 errmsg("could not compute SHA-256: %s", copy)));
	}
	pg_cryptohash_free(ctx);
}

/* SHA-256 of a single byte range; len = 0 gives the hash of the empty string. */
void
kl_sha256(uint8 *out, const uint8 *data, size_t len)
{
	const uint8 *parts[1];
	size_t		lens[1];

	parts[0] = data;
	lens[0] = len;
	kl_sha256_parts(out, len > 0 ? 1 : 0, parts, lens);
}

/*
 * kl_leaf_hash
 *		RFC 6962 leaf hash: SHA-256(0x00 || data).  The 0x00 prefix separates
 *		leaves from inner nodes so that an inner node can never be presented
 *		as a leaf (second-preimage protection).
 */
void
kl_leaf_hash(uint8 *out, const uint8 *data, size_t len)
{
	static const uint8 prefix[1] = {0x00};
	const uint8 *parts[2];
	size_t		lens[2];

	parts[0] = prefix;
	lens[0] = 1;
	parts[1] = data;
	lens[1] = len;
	kl_sha256_parts(out, 2, parts, lens);
}

/* RFC 6962 inner node hash: SHA-256(0x01 || left || right). */
void
kl_node_hash(uint8 *out, const uint8 *left, const uint8 *right)
{
	static const uint8 prefix[1] = {0x01};
	const uint8 *parts[3];
	size_t		lens[3];

	parts[0] = prefix;
	lens[0] = 1;
	parts[1] = left;
	lens[1] = KL_HASH_LEN;
	parts[2] = right;
	lens[2] = KL_HASH_LEN;
	kl_sha256_parts(out, 3, parts, lens);
}

/*
 * kl_merkle_accum_add
 *		Add one leaf hash to the incremental root computation.
 *
 * The state is a binary counter: bit i of "count" is set when a perfect
 * subtree of 2^i leaves is pending, with its root in stack[i].  Adding a leaf
 * carries like binary addition.  This needs O(log n) memory, so the root
 * aggregate does not have to keep all leaves.
 */
void
kl_merkle_accum_add(KlMerkleAccum *acc, const uint8 *leaf_hash)
{
	uint8		carry[KL_HASH_LEN];
	int			level = 0;

	memcpy(carry, leaf_hash, KL_HASH_LEN);
	while ((acc->count >> level) & 1)
	{
		kl_node_hash(carry, acc->stack[level], carry);
		level++;
	}
	memcpy(acc->stack[level], carry, KL_HASH_LEN);
	acc->count++;
}

/*
 * kl_merkle_accum_root
 *		Root of the tree of the leaves added so far.
 *
 * RFC 6962 splits a tree of n leaves at the largest power of two smaller than
 * n, so the pending perfect subtrees are combined from the smallest (right
 * most) to the largest.  Zero leaves give SHA-256 of the empty string.
 */
void
kl_merkle_accum_root(const KlMerkleAccum *acc, uint8 *out)
{
	bool		have = false;
	int			level;

	if (acc->count == 0)
	{
		kl_sha256(out, NULL, 0);
		return;
	}
	for (level = 0; level < KL_MAX_DEPTH; level++)
	{
		if (((acc->count >> level) & 1) == 0)
			continue;
		if (!have)
		{
			memcpy(out, acc->stack[level], KL_HASH_LEN);
			have = true;
		}
		else
			kl_node_hash(out, acc->stack[level], out);
	}
}

/* Largest power of two strictly smaller than n; n must be at least 2. */
static int64
kl_split_point(int64 n)
{
	int64		k = 1;

	while (k * 2 < n)
		k *= 2;
	return k;
}

/*
 * kl_mth_range
 *		Merkle tree hash of leaves[lo, hi) where "leaves" holds leaf hashes.
 *
 * Direct transcription of the recursive definition of RFC 6962 section 2.1;
 * the recursion depth is at most log2(n).
 */
static void
kl_mth_range(const uint8 *leaves, int64 lo, int64 hi, uint8 *out)
{
	int64		n = hi - lo;
	int64		k;
	uint8		left[KL_HASH_LEN];
	uint8		right[KL_HASH_LEN];

	check_stack_depth();
	if (n == 0)
	{
		kl_sha256(out, NULL, 0);
		return;
	}
	if (n == 1)
	{
		memcpy(out, leaves + lo * KL_HASH_LEN, KL_HASH_LEN);
		return;
	}
	k = kl_split_point(n);
	kl_mth_range(leaves, lo, lo + k, left);
	kl_mth_range(leaves, lo + k, hi, right);
	kl_node_hash(out, left, right);
}

/*
 * kl_merkle_path
 *		Audit path of leaf "index" in a tree of "n" leaf hashes, RFC 6962
 *		section 2.1.1, written to "path" (at most KL_MAX_DEPTH hashes).
 *		Returns the number of hashes.  The path starts with the sibling
 *		closest to the leaf.
 */
int
kl_merkle_path(const uint8 *leaves, int64 n, int64 index, uint8 *path)
{
	uint8		tmp[KL_MAX_DEPTH][KL_HASH_LEN];
	int			count = 0;
	int64		lo = 0;
	int64		hi = n;
	int			i;

	Assert(index >= 0 && index < n);
	while (hi - lo > 1)
	{
		int64		k = kl_split_point(hi - lo);

		if (index < lo + k)
		{
			kl_mth_range(leaves, lo + k, hi, tmp[count]);
			hi = lo + k;
		}
		else
		{
			kl_mth_range(leaves, lo, lo + k, tmp[count]);
			lo += k;
		}
		count++;
	}
	/* The recursion of the RFC puts the deepest sibling first: reverse. */
	for (i = 0; i < count; i++)
		memcpy(path + i * KL_HASH_LEN, tmp[count - 1 - i], KL_HASH_LEN);
	return count;
}

/*
 * kl_merkle_verify_path
 *		Check an audit path (RFC 9162 section 2.1.3.2): recompute the root
 *		from the leaf hash, its index and the tree size, and compare with the
 *		expected root.
 *
 * The tree size takes part in the computation, so a path cannot be replayed
 * against a tree of a different size, and a path of the wrong length is
 * rejected.
 */
bool
kl_merkle_verify_path(const uint8 *leaf_hash, int64 index, int64 size,
					  const uint8 *path, int npath, const uint8 *root)
{
	uint8		r[KL_HASH_LEN];
	uint64		fn;
	uint64		sn;
	int			i;

	if (index < 0 || size <= 0 || index >= size)
		return false;
	fn = (uint64) index;
	sn = (uint64) size - 1;
	memcpy(r, leaf_hash, KL_HASH_LEN);

	for (i = 0; i < npath; i++)
	{
		const uint8 *p = path + (size_t) i * KL_HASH_LEN;

		if (sn == 0)
			return false;
		if ((fn & 1) == 1 || fn == sn)
		{
			kl_node_hash(r, p, r);
			if ((fn & 1) == 0)
			{
				while ((fn & 1) == 0 && fn != 0)
				{
					fn >>= 1;
					sn >>= 1;
				}
			}
		}
		else
			kl_node_hash(r, r, p);
		fn >>= 1;
		sn >>= 1;
	}
	return sn == 0 && memcmp(r, root, KL_HASH_LEN) == 0;
}

/*
 * kl_tx_leaf_hash
 *		Leaf hash of one entry in the transaction tree.
 *		Leaf data: u8 KL_TAG_TX_ENTRY || u32 seq || u32 table_id || u8 op ||
 *		row_hash.
 */
void
kl_tx_leaf_hash(uint8 *out, uint32 seq, uint32 table_id, uint8 op,
				const uint8 *row_hash)
{
	uint8		buf[1 + 4 + 4 + 1 + KL_HASH_LEN];

	buf[0] = KL_TAG_TX_ENTRY;
	kl_put_u32(buf + 1, seq);
	kl_put_u32(buf + 5, table_id);
	buf[9] = op;
	memcpy(buf + 10, row_hash, KL_HASH_LEN);
	kl_leaf_hash(out, buf, sizeof(buf));
}

/*
 * kl_block_tx_leaf_hash
 *		Leaf hash of one transaction in the block tree.
 *		Leaf data: u8 KL_TAG_BLOCK_TX || u64 ledger_txid || u32 entry_count ||
 *		merkle_root || u16 length of user_name || user_name in UTF-8.  The
 *		fixed-width fields come first and the name is length prefixed, so the
 *		encoding is unambiguous.  Covering the name here puts it under the
 *		block's transaction root and therefore under the block hash.  "namelen"
 *		must be at most KL_MAX_USER_NAME_BYTES.
 */
void
kl_block_tx_leaf_hash(uint8 *out, uint64 ledger_txid, uint32 entry_count,
					  const uint8 *root, const uint8 *name, size_t namelen)
{
	uint8		buf[1 + 8 + 4 + KL_HASH_LEN + 2 + KL_MAX_USER_NAME_BYTES];

	Assert(namelen <= KL_MAX_USER_NAME_BYTES);
	buf[0] = KL_TAG_BLOCK_TX;
	kl_put_u64(buf + 1, ledger_txid);
	kl_put_u32(buf + 9, entry_count);
	memcpy(buf + 13, root, KL_HASH_LEN);
	kl_put_u16(buf + 13 + KL_HASH_LEN, (uint16) namelen);
	if (namelen > 0)
		memcpy(buf + 15 + KL_HASH_LEN, name, namelen);
	kl_leaf_hash(out, buf, 15 + KL_HASH_LEN + namelen);
}

/*
 * kl_block_preimage
 *		The data a block hash covers: u8 KL_TAG_BLOCK || u16 hash_version ||
 *		u64 height || prev_hash || tx_merkle_root || u32 tx_count ||
 *		i64 closed_at_us, KL_BLOCK_PREIMAGE_LEN bytes into buf.
 *		closed_at_us is microseconds since 2000-01-01 00:00:00 UTC, the
 *		internal representation of timestamptz.
 *
 *		This is the one place that lays the bytes out: kl_block_hash hashes
 *		it, and the SQL function ledger_block_preimage returns it as the leaf
 *		of the chain log, so the two cannot drift apart.
 */
void
kl_block_preimage(uint8 *buf, uint16 version, uint64 height,
				  const uint8 *prev_hash, const uint8 *tx_root,
				  uint32 tx_count, int64 closed_at_us)
{
	buf[0] = KL_TAG_BLOCK;
	kl_put_u16(buf + 1, version);
	kl_put_u64(buf + 3, height);
	memcpy(buf + 11, prev_hash, KL_HASH_LEN);
	memcpy(buf + 11 + KL_HASH_LEN, tx_root, KL_HASH_LEN);
	kl_put_u32(buf + 11 + 2 * KL_HASH_LEN, tx_count);
	kl_put_u64(buf + 15 + 2 * KL_HASH_LEN, (uint64) closed_at_us);
}

/*
 * kl_block_hash
 *		block_hash = SHA-256(kl_block_preimage(...)).
 */
void
kl_block_hash(uint8 *out, uint16 version, uint64 height,
			  const uint8 *prev_hash, const uint8 *tx_root, uint32 tx_count,
			  int64 closed_at_us)
{
	uint8		buf[KL_BLOCK_PREIMAGE_LEN];

	kl_block_preimage(buf, version, height, prev_hash, tx_root, tx_count,
					  closed_at_us);
	kl_sha256(out, buf, sizeof(buf));
}

/*
 * kl_genesis_hash
 *		The prev_hash of block 0: SHA-256(u8 KL_TAG_CHAIN || chain_id).  It
 *		ties the first block, and through the hash links every later block, to
 *		the random identity of the chain.
 */
void
kl_genesis_hash(uint8 *out, const uint8 *chain_id)
{
	uint8		buf[1 + KL_CHAIN_ID_LEN];

	buf[0] = KL_TAG_CHAIN;
	memcpy(buf + 1, chain_id, KL_CHAIN_ID_LEN);
	kl_sha256(out, buf, sizeof(buf));
}

/* Build a bytea datum from a byte range. */
static bytea *
kl_make_bytea(const uint8 *data, size_t len)
{
	bytea	   *result = (bytea *) palloc(VARHDRSZ + len);

	SET_VARSIZE(result, VARHDRSZ + len);
	if (len > 0)
		memcpy(VARDATA(result), data, len);
	return result;
}

/* Fetch a 32-byte hash argument, rejecting any other length. */
static const uint8 *
kl_get_hash_arg(FunctionCallInfo fcinfo, int argno, const char *what)
{
	bytea	   *b = PG_GETARG_BYTEA_PP(argno);

	if (VARSIZE_ANY_EXHDR(b) != KL_HASH_LEN)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("%s must be exactly %d bytes", what, KL_HASH_LEN)));
	return (const uint8 *) VARDATA_ANY(b);
}

/*
 * kingres.ledger_leaf_hash(data bytea) returns bytea
 *		RFC 6962 leaf hash of arbitrary data.
 */
Datum
kingres_ledger_leaf_hash(PG_FUNCTION_ARGS)
{
	bytea	   *data = PG_GETARG_BYTEA_PP(0);
	uint8		out[KL_HASH_LEN];

	kl_leaf_hash(out, (const uint8 *) VARDATA_ANY(data),
				 VARSIZE_ANY_EXHDR(data));
	PG_RETURN_BYTEA_P(kl_make_bytea(out, KL_HASH_LEN));
}

/*
 * kingres.ledger_node_hash(left bytea, right bytea) returns bytea
 *		RFC 6962 inner node hash of two 32-byte hashes.
 */
Datum
kingres_ledger_node_hash(PG_FUNCTION_ARGS)
{
	const uint8 *left = kl_get_hash_arg(fcinfo, 0, "left hash");
	const uint8 *right = kl_get_hash_arg(fcinfo, 1, "right hash");
	uint8		out[KL_HASH_LEN];

	kl_node_hash(out, left, right);
	PG_RETURN_BYTEA_P(kl_make_bytea(out, KL_HASH_LEN));
}

/*
 * Transition function of the aggregate kingres.ledger_merkle_root(bytea).
 * The input values are leaf hashes (32 bytes) and the aggregate is sensitive
 * to input order: callers write ledger_merkle_root(x ORDER BY ...).  The
 * state lives in the aggregate memory context.
 */
Datum
kingres_ledger_merkle_accum(PG_FUNCTION_ARGS)
{
	MemoryContext aggcontext;
	KlMerkleAccum *acc;
	bytea	   *leaf;

	if (!AggCheckCallContext(fcinfo, &aggcontext))
		elog(ERROR, "kingres_ledger_merkle_accum called in non-aggregate context");

	if (PG_ARGISNULL(0))
		acc = (KlMerkleAccum *) MemoryContextAllocZero(aggcontext,
													   sizeof(KlMerkleAccum));
	else
		acc = (KlMerkleAccum *) PG_GETARG_POINTER(0);

	if (PG_ARGISNULL(1))
		ereport(ERROR,
				(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
				 errmsg("ledger_merkle_root does not accept NULL leaf hashes")));
	leaf = PG_GETARG_BYTEA_PP(1);
	if (VARSIZE_ANY_EXHDR(leaf) != KL_HASH_LEN)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("leaf hash must be exactly %d bytes", KL_HASH_LEN)));

	kl_merkle_accum_add(acc, (const uint8 *) VARDATA_ANY(leaf));
	PG_RETURN_POINTER(acc);
}

/*
 * Final function of kingres.ledger_merkle_root.  Not strict: an aggregate
 * over zero rows has a NULL state and must return the root of the empty
 * tree, SHA-256 of the empty string.
 */
Datum
kingres_ledger_merkle_final(PG_FUNCTION_ARGS)
{
	uint8		out[KL_HASH_LEN];

	if (PG_ARGISNULL(0))
		kl_sha256(out, NULL, 0);
	else
		kl_merkle_accum_root((KlMerkleAccum *) PG_GETARG_POINTER(0), out);
	PG_RETURN_BYTEA_P(kl_make_bytea(out, KL_HASH_LEN));
}

/*
 * kingres.ledger_merkle_path(leaves bytea[], idx int) returns bytea[]
 *		Audit path of the leaf with zero-based index idx in the tree whose
 *		leaf hashes are given in order.
 */
Datum
kingres_ledger_merkle_path(PG_FUNCTION_ARGS)
{
	ArrayType  *arr = PG_GETARG_ARRAYTYPE_P(0);
	int32		index = PG_GETARG_INT32(1);
	Datum	   *elems;
	bool	   *nulls;
	int			n;
	uint8	   *flat;
	uint8		path[KL_MAX_DEPTH * KL_HASH_LEN];
	Datum		out[KL_MAX_DEPTH];
	int			npath;
	int			i;

	deconstruct_array(arr, BYTEAOID, -1, false, TYPALIGN_INT,
					  &elems, &nulls, &n);
	if (index < 0 || index >= n)
		ereport(ERROR,
				(errcode(ERRCODE_ARRAY_SUBSCRIPT_ERROR),
				 errmsg("leaf index %d is out of range for %d leaves",
						index, n)));

	flat = (uint8 *) palloc((Size) n * KL_HASH_LEN);
	for (i = 0; i < n; i++)
	{
		bytea	   *b;

		if (nulls[i])
			ereport(ERROR,
					(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
					 errmsg("leaf hashes must not be NULL")));
		b = DatumGetByteaPP(elems[i]);
		if (VARSIZE_ANY_EXHDR(b) != KL_HASH_LEN)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("leaf hash must be exactly %d bytes",
							KL_HASH_LEN)));
		memcpy(flat + (Size) i * KL_HASH_LEN, VARDATA_ANY(b), KL_HASH_LEN);
	}

	npath = kl_merkle_path(flat, n, index, path);
	for (i = 0; i < npath; i++)
		out[i] = PointerGetDatum(kl_make_bytea(path + i * KL_HASH_LEN,
											   KL_HASH_LEN));
	PG_RETURN_ARRAYTYPE_P(construct_array(out, npath, BYTEAOID, -1, false,
										  TYPALIGN_INT));
}

/*
 * kingres.ledger_tx_leaf(seq int, table_id int, op "char", row_hash bytea)
 *		Leaf hash of an entry in the transaction tree.
 */
Datum
kingres_ledger_tx_leaf(PG_FUNCTION_ARGS)
{
	int32		seq = PG_GETARG_INT32(0);
	int32		table_id = PG_GETARG_INT32(1);
	char		op = PG_GETARG_CHAR(2);
	const uint8 *row_hash = kl_get_hash_arg(fcinfo, 3, "row hash");
	uint8		out[KL_HASH_LEN];

	if (seq < 0 || table_id < 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("seq and table_id must not be negative")));
	kl_tx_leaf_hash(out, (uint32) seq, (uint32) table_id, (uint8) op,
					row_hash);
	PG_RETURN_BYTEA_P(kl_make_bytea(out, KL_HASH_LEN));
}

/*
 * kingres.ledger_block_tx_leaf(ledger_txid bigint, entry_count int,
 *								 merkle_root bytea, user_name name)
 *		Leaf hash of a transaction in the block tree.  The user name is
 *		converted from the database encoding to UTF-8.
 */
Datum
kingres_ledger_block_tx_leaf(PG_FUNCTION_ARGS)
{
	int64		txid = PG_GETARG_INT64(0);
	int32		entry_count = PG_GETARG_INT32(1);
	const uint8 *root = kl_get_hash_arg(fcinfo, 2, "transaction root");
	Name		uname = PG_GETARG_NAME(3);
	unsigned char *utf8;
	size_t		namelen;
	uint8		out[KL_HASH_LEN];

	utf8 = pg_do_encoding_conversion((unsigned char *) NameStr(*uname),
									 strlen(NameStr(*uname)),
									 GetDatabaseEncoding(), PG_UTF8);
	namelen = strlen((char *) utf8);
	if (namelen > KL_MAX_USER_NAME_BYTES)
		ereport(ERROR,
				(errcode(ERRCODE_NAME_TOO_LONG),
				 errmsg("user name is too long")));

	if (txid < 0 || entry_count < 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("ledger_txid and entry_count must not be negative")));
	kl_block_tx_leaf_hash(out, (uint64) txid, (uint32) entry_count, root,
						  utf8, namelen);
	PG_RETURN_BYTEA_P(kl_make_bytea(out, KL_HASH_LEN));
}

/*
 * kingres.ledger_block_hash(hash_version smallint, height bigint,
 *							  prev_hash bytea, tx_merkle_root bytea,
 *							  tx_count int, closed_at timestamptz)
 *		Block hash.  Only hash_version 1 is defined.
 */
Datum
kingres_ledger_block_hash(PG_FUNCTION_ARGS)
{
	int16		version = PG_GETARG_INT16(0);
	int64		height = PG_GETARG_INT64(1);
	const uint8 *prev = kl_get_hash_arg(fcinfo, 2, "previous block hash");
	const uint8 *root = kl_get_hash_arg(fcinfo, 3, "transaction root");
	int32		tx_count = PG_GETARG_INT32(4);
	TimestampTz closed_at = PG_GETARG_TIMESTAMPTZ(5);
	uint8		out[KL_HASH_LEN];

	if (version != KL_HASH_VERSION)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("hash version %d is not supported", version)));
	if (height < 0 || tx_count < 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("height and tx_count must not be negative")));
	if (TIMESTAMP_NOT_FINITE(closed_at))
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("closed_at must be a finite timestamp")));
	kl_block_hash(out, (uint16) version, (uint64) height, prev, root,
				  (uint32) tx_count, (int64) closed_at);
	PG_RETURN_BYTEA_P(kl_make_bytea(out, KL_HASH_LEN));
}

/*
 * kingres.ledger_block_preimage(hash_version smallint, height bigint,
 *								  prev_hash bytea, tx_merkle_root bytea,
 *								  tx_count int, closed_at timestamptz)
 *		The 87 bytes ledger_block_hash hashes, with the same checks on the
 *		arguments.
 */
Datum
kingres_ledger_block_preimage(PG_FUNCTION_ARGS)
{
	int16		version = PG_GETARG_INT16(0);
	int64		height = PG_GETARG_INT64(1);
	const uint8 *prev = kl_get_hash_arg(fcinfo, 2, "previous block hash");
	const uint8 *root = kl_get_hash_arg(fcinfo, 3, "transaction root");
	int32		tx_count = PG_GETARG_INT32(4);
	TimestampTz closed_at = PG_GETARG_TIMESTAMPTZ(5);
	uint8		out[KL_BLOCK_PREIMAGE_LEN];

	if (version != KL_HASH_VERSION)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("hash version %d is not supported", version)));
	if (height < 0 || tx_count < 0)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("height and tx_count must not be negative")));
	if (TIMESTAMP_NOT_FINITE(closed_at))
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("closed_at must be a finite timestamp")));
	kl_block_preimage(out, (uint16) version, (uint64) height, prev, root,
					  (uint32) tx_count, (int64) closed_at);
	PG_RETURN_BYTEA_P(kl_make_bytea(out, sizeof(out)));
}

/*
 * kingres.ledger_genesis_hash(chain_id bytea) returns bytea
 *		The prev_hash of block 0 of the chain with this identity.
 */
Datum
kingres_ledger_genesis_hash(PG_FUNCTION_ARGS)
{
	const uint8 *chain_id = kl_get_hash_arg(fcinfo, 0, "chain id");
	uint8		out[KL_HASH_LEN];

	kl_genesis_hash(out, chain_id);
	PG_RETURN_BYTEA_P(kl_make_bytea(out, KL_HASH_LEN));
}
