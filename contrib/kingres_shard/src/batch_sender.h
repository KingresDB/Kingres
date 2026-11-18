/*-------------------------------------------------------------------------
 *
 * batch_sender.h
 *	  The batch sender: buffers rows per destination, sends them as batches of
 *	  one INSERT statement, and checks the placement of every group it wrote
 *	  (design/shard-phase2b.md, 11.3, 11.4 and 11.9).
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/batch_sender.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_BATCH_SENDER_H
#define KINGRES_SHARD_BATCH_SENDER_H

#include "postgres.h"

#include "access/tupdesc.h"
#include "nodes/pg_list.h"
#include "utils/tuplestore.h"

#include "catalog.h"

typedef enum KsDestKind
{
	KS_DEST_PLACEMENT,			/* each row goes to the group that owns its bucket */
	KS_DEST_EVERY_NODE,			/* each row goes to every active primary */
	KS_DEST_ONE_NODE			/* every row goes to one node */
} KsDestKind;

typedef enum KsConflict
{
	KS_CONFLICT_NONE,
	KS_CONFLICT_NOTHING			/* ON CONFLICT ... DO NOTHING; the clause is KsBatchSpec.conflict_sql */
} KsConflict;

/* A column of the rows given to the sender: its name and type on the destination. */
typedef struct KsBatchColumn
{
	const char *name;
	Oid			type;
	int32		typmod;
} KsBatchColumn;

typedef struct KsDestination
{
	KsDestKind	kind;
	const char *schema;			/* the destination table, unquoted */
	const char *table;
	Oid			relid;			/* InvalidOid when the table is not in the catalog */
	const KsColocation *placement;	/* KS_DEST_PLACEMENT: key type, collation and bucket layout (4.7) */
	int			key_attno;		/* position (from 0) of the key column in the rows given to the sender */
	int			node_id;		/* KS_DEST_ONE_NODE */
	int64		expect_placement;	/* the placement number the caller decided with; 0: no check */
	bool		check_identity;	/* a table that is not in the catalog: check its partitions */
} KsDestination;

/*
 * What a batch statement is made of.  The texts are fragments the caller has deparsed
 * (the router does it in the planner hook, under the transmission modes, from the
 * Query); nothing a row contains is ever part of the statement text.
 */
typedef struct KsBatchSpec
{
	List	   *columns;		/* of KsBatchColumn *: names and types of the columns of the rows */
	int			conflict;		/* KS_CONFLICT_NONE | KS_CONFLICT_NOTHING */
	const char *conflict_sql;	/* "ON CONFLICT ... DO NOTHING" for KS_CONFLICT_NOTHING */
	bool		overriding_system_value;
	const char *returning_sql;	/* the RETURNING list as text, or NULL */
	TupleDesc	returning_desc;	/* its columns, when returning_sql is given */
	Tuplestorestate *returning_rows;	/* where the RETURNING rows go, when returning_sql is given */
} KsBatchSpec;

typedef struct KsBatchSender KsBatchSender;

/*
 * kingres_shard_batch_begin makes a memory context of its own below the current one; the
 * sender lives as long as it, and an error that ends the statement lets go of every
 * connection that has a batch in flight (a reset callback of that context).
 * Rows are given to kingres_shard_batch_add() with the transmission modes
 * (kingres_shard_set_transmission_modes) in force, which is how the text of a value is made.
 */
extern KsBatchSender *kingres_shard_batch_begin(const KsDestination *dest, const KsBatchSpec *spec);
extern void kingres_shard_batch_add(KsBatchSender *s, const Datum *values, const bool *nulls);
/* Send what is buffered and wait for every batch in flight. */
extern void kingres_shard_batch_flush(KsBatchSender *s);
/*
 * Flush, ask every group that was written for its placement again, and return the number of
 * rows the groups inserted.  The sender is not usable afterwards; kingres_shard_batch_end()
 * frees it.
 */
extern int64 kingres_shard_batch_finish(KsBatchSender *s);
extern void kingres_shard_batch_end(KsBatchSender *s);

/*
 * The marker that starts the statement of a batch (11.5): the receiving node accepts it only
 * for an INSERT of the shape the sender makes and checks the placement number.
 */
#define KS_INSERT_MARKER_PREFIX "/* kingres:insert-fragment "
extern bool kingres_shard_parse_insert_marker(const char *query_string, int64 *place,
											   bool *has_ident, int64 *ident);

#endif							/* KINGRES_SHARD_BATCH_SENDER_H */
