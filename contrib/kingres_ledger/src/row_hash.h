/*-------------------------------------------------------------------------
 *
 * row_hash.h
 *	  Canonical row encoding and row hash of the ledger.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: PostgreSQL
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/row_hash.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_LEDGER_ROW_HASH_H
#define KINGRES_LEDGER_ROW_HASH_H

#include "access/htup.h"
#include "access/tupdesc.h"

extern void kl_row_hash_tuple(TupleDesc tupdesc, HeapTuple tuple,
							  int32 table_id, uint8 *out);
extern void kl_row_hash_composite(HeapTupleHeader header, int32 table_id,
								  uint8 *out);
extern int32 kl_table_id_for_type(Oid typeid);
extern void kl_table_cache_reset(void);

#endif							/* KINGRES_LEDGER_ROW_HASH_H */
