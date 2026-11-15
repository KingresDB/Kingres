/*-------------------------------------------------------------------------
 *
 * merkle.h
 *	  SHA-256 helpers and RFC 6962 Merkle tree primitives of the ledger.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/merkle.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_LEDGER_MERKLE_H
#define KINGRES_LEDGER_MERKLE_H

#include "kingres_ledger.h"

/* Longest path or stack the code supports: trees of up to 2^63 leaves. */
#define KL_MAX_DEPTH 64

/*
 * State of the incremental root computation: the roots of the perfect
 * subtrees that make up the tree so far, one per set bit of "count".
 */
typedef struct KlMerkleAccum
{
	uint64		count;
	uint8		stack[KL_MAX_DEPTH][KL_HASH_LEN];
} KlMerkleAccum;

extern void kl_sha256(uint8 *out, const uint8 *data, size_t len);
extern void kl_leaf_hash(uint8 *out, const uint8 *data, size_t len);
extern void kl_node_hash(uint8 *out, const uint8 *left, const uint8 *right);

extern void kl_merkle_accum_add(KlMerkleAccum *acc, const uint8 *leaf_hash);
extern void kl_merkle_accum_root(const KlMerkleAccum *acc, uint8 *out);

extern int	kl_merkle_path(const uint8 *leaves, int64 n, int64 index,
						   uint8 *path);
extern bool kl_merkle_verify_path(const uint8 *leaf_hash, int64 index,
								  int64 size, const uint8 *path, int npath,
								  const uint8 *root);

extern void kl_tx_leaf_hash(uint8 *out, uint32 seq, uint32 table_id, uint8 op,
							const uint8 *row_hash);
extern void kl_block_tx_leaf_hash(uint8 *out, uint64 ledger_txid,
								  uint32 entry_count, const uint8 *root,
								  const uint8 *name, size_t namelen);
/* Length of the data a block hash covers (and of a chain-log leaf). */
#define KL_BLOCK_PREIMAGE_LEN (1 + 2 + 8 + KL_HASH_LEN + KL_HASH_LEN + 4 + 8)

extern void kl_block_preimage(uint8 *buf, uint16 version, uint64 height,
							  const uint8 *prev_hash, const uint8 *tx_root,
							  uint32 tx_count, int64 closed_at_us);
extern void kl_block_hash(uint8 *out, uint16 version, uint64 height,
						  const uint8 *prev_hash, const uint8 *tx_root,
						  uint32 tx_count, int64 closed_at_us);
extern void kl_genesis_hash(uint8 *out, const uint8 *chain_id);

#endif							/* KINGRES_LEDGER_MERKLE_H */
