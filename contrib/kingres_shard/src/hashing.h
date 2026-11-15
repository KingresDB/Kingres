/*-------------------------------------------------------------------------
 *
 * hashing.h
 *	  The bucket function: which hash bucket a distribution key belongs to.
 *
 * SPDX-License-Identifier: PostgreSQL
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

extern bool kingres_shard_try_bucket(Oid column_type, Oid collation,
									  int bucket_count, Oid value_type,
									  Datum value, bool isnull, int *bucket);
extern int	kingres_shard_bucket(Oid column_type, Oid collation,
								  int bucket_count, Oid value_type,
								  Datum value, bool isnull);

#endif							/* KINGRES_SHARD_HASHING_H */
