/*-------------------------------------------------------------------------
 *
 * hashing.h
 *	  The bucket function: which hash bucket a distribution key belongs to.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/hashing.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_HASHING_H
#define KINGRES_SHARD_HASHING_H

#include "postgres.h"

#include "catalog.h"

/*
 * The one place that turns a key value into a bucket (design/shard-phase2b.md,
 * section 4.7).  No other code computes a hash of a key or a remainder of a
 * hash: it calls these.
 *
 * The hash of one key value, with the extended hash function of the default
 * hash operator class of the distribution column's type; false when the type
 * has no suitable hash function.  A NULL key hashes to 0.
 */
extern bool kingres_shard_hash_of_value(Oid column_type, Oid collation,
										 Oid value_type, Datum value,
										 bool isnull, uint64 *hash);

/* The number of the bucket that holds a hash value in a colocation group. */
extern int	kingres_shard_bucket_for_hash(const KsColocation *coloc,
										   uint64 hash);

/* The two above in one call; false when the value cannot be hashed. */
extern bool kingres_shard_try_bucket(Oid column_type, Oid collation,
									  const KsColocation *coloc, Oid value_type,
									  Datum value, bool isnull, int *bucket);
extern int	kingres_shard_bucket(Oid column_type, Oid collation,
								  const KsColocation *coloc, Oid value_type,
								  Datum value, bool isnull);

#endif							/* KINGRES_SHARD_HASHING_H */
