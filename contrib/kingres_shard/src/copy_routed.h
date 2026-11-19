/*-------------------------------------------------------------------------
 *
 * copy_routed.h
 *	  Copy the rows of a table to a destination that is described, not looked
 *	  up in the catalog (design/shard-phase9.md, "Copy").
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/copy_routed.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_COPY_ROUTED_H
#define KINGRES_SHARD_COPY_ROUTED_H

#include "postgres.h"

#include "batch_sender.h"
#include "catalog.h"

/*
 * Read every row of the table source (all columns that are not generated) and
 * give it to the batch sender of the destination dest_schema.dest_table: each
 * row to the group that owns its bucket in placement (KS_DEST_PLACEMENT, the key
 * is key_column), to every active primary (KS_DEST_EVERY_NODE) or to node_id
 * (KS_DEST_ONE_NODE).  The destination need not be in the catalog.  The rows are
 * read through a cursor in chunks and sent in batches, so the memory used does
 * not depend on the number of rows.  The transmission modes of the transaction
 * are set around the conversion of the values.  Returns the number of rows the
 * destination groups acknowledged and sets *rows_read to the number of rows
 * read; the caller compares them.  The caller's transaction carries the writes:
 * a failure is an error, which rolls everything back.
 */
extern int64 kingres_shard_copy_routed(Oid source, const char *dest_schema, const char *dest_table,
									   KsDestKind kind, const KsColocation *placement,
									   const char *key_column, int node_id, int64 *rows_read);

/*
 * One chunk of an online copy or replay: the rows of the source that satisfy where_sql (a
 * predicate text made by the extension, or NULL), taken in the order of the key columns after the
 * key given in after (NULL: from the start), at most limit of them (0: all).  The last key read
 * is returned as text in *last (nkeys values, in the caller's memory context; NULL when no row was
 * read).  With conflict_nothing the insert has ON CONFLICT DO NOTHING (a chunk that is repeated
 * stores nothing twice); without it a conflict is an error.
 */
typedef struct KsCopyOpts
{
	const char *where_sql;
	int			nkeys;
	char	  **keycols;
	char	  **after;
	int64		limit;
	bool		conflict_nothing;
	char	  **last;			/* out: nkeys strings, or NULL */
} KsCopyOpts;

/* The placement of a colocation group from the catalog tables, as of this transaction (needs no SPI connection of its own). */
extern KsColocation *kingres_shard_load_placement(int colocation_id);

extern int64 kingres_shard_copy_routed_ex(Oid source, const char *dest_schema, const char *dest_table,
										  KsDestKind kind, const KsColocation *placement,
										  const char *key_column, int node_id, int64 *rows_read,
										  KsCopyOpts *opts);

#endif							/* KINGRES_SHARD_COPY_ROUTED_H */
