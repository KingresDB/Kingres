/*-------------------------------------------------------------------------
 *
 * row_hash.h
 *	  Canonical row encoding and row hash of the ledger.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
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
							  int32 table_id, bool history, uint8 *out);
extern void kl_row_hash_composite(HeapTupleHeader header, int32 table_id,
								  bool history, uint8 *out);
extern int32 kl_table_id_for_type(Oid typeid);
extern Oid	kl_lookup_history(Oid relid, int32 *table_id);
extern int32 kl_lookup_table_id(Oid relid);
extern bool kl_is_history_column(const char *name);
extern void kl_table_cache_reset(void);

#endif							/* KINGRES_LEDGER_ROW_HASH_H */
