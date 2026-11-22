/*-------------------------------------------------------------------------
 *
 * repartition.h
 *	  The repartitioned join (design/analytics-phase12-queries.md, section 4): a join of
 *	  distributed tables on columns that are not their common distribution column, run by the
 *	  groups after the rows of one of the tables were moved to the group that owns the
 *	  matching rows of the others.
 *
 * Increment J1: one routed relation (a distributed table whose rows go, each to the group
 * that owns the bucket of its join key in the colocation group of the anchored tables),
 * inner joins, the rows of the join returned to this node, which plans what is above them.
 * The routed relation is read by its own ordinary scan (the child of the Fragment Scan), its
 * rows are hashed here and each group receives its rows as array parameters of the
 * statement: a sub-select over unnest() takes the place of the relation in the text.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/repartition.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_REPARTITION_H
#define KINGRES_SHARD_REPARTITION_H

#include "postgres.h"

#include "access/attnum.h"
#include "nodes/execnodes.h"
#include "nodes/parsenodes.h"
#include "nodes/pathnodes.h"
#include "nodes/pg_list.h"

#include "catalog.h"
#include "fragment_scan.h"

/* kingres_shard.enable_repartition_join: off, on (by cost), force (always, for the tests) */
#define KS_REPART_OFF 0
#define KS_REPART_ON 1
#define KS_REPART_FORCE 2
extern PGDLLIMPORT int kingres_shard_enable_repartition_join;
/* kingres_shard.repartition_max_bytes (kB): the rows of the moved side that one statement may hold at this node */
extern PGDLLIMPORT int kingres_shard_repartition_max_kb;

extern void kingres_shard_repartition_init(void);

/*
 * One way to repartition the statement: the relation `routed_rti` is the shipped one, `key_attno`
 * its column that is equated with the distribution key of the anchored tables.
 */
typedef struct KsRepartCand
{
	Index		routed_rti;
	Oid			routed_relid;
	AttrNumber	key_attno;
	Oid			key_type;
	int			colocation_id;	/* of the anchored tables */
	int64		place;			/* its placement number */
	List	   *dist_relids;	/* the anchored tables (relation OIDs): what the groups read */
	int			nanchored;		/* the references to them in the statement */
} KsRepartCand;

/*
 * Is the query the sub-select that stands for a shipped relation in a statement (design 3.1): a
 * SELECT of casts of the columns of one unnest() of text[] parameters, and NULL constants?  The
 * groups accept a sub-select of this shape and no other.  *narrays: the number of arrays.
 */
extern bool kingres_shard_repart_shipped_shape(Query *q, int *narrays);

/*
 * The decision of the origin, when the statement is not a unit of 2b (pushdown_prepare): can it be
 * run by repartitioning one relation?  The result has the candidates, the paths of
 * fragment_scan.c make the text; NULL with *why (a static string) when it cannot.
 */
extern KsPushdownInfo *kingres_shard_repart_prepare(Query *parse, const char **why);

/*
 * What EXPLAIN (VERBOSE) says of a statement that was a candidate and got no repartitioned plan (design 4.12).  The reasons are
 * collected between kingres_shard_repart_attach and the end of plan_with_scan; the text is NULL when there is nothing to say.
 */
extern void kingres_shard_repart_note_reset(void);
extern void kingres_shard_repart_note(const char *msg);
extern void kingres_shard_repart_offered(void);
extern const char *kingres_shard_repart_explain_reason(void);

/* The hook of planner.c: add the candidates to what pushdown_prepare found (pi may be NULL); pi when none. */
extern KsPushdownInfo *kingres_shard_repart_attach(Query *parse, KsPushdownInfo *pi);

/*
 * The path side: replace the routed relation of the text `jq` (a copy of the statement made for the
 * join: its select list is the columns the plan needs above the join) by the sub-select over
 * unnest.  `first_param` is the number of the first array parameter.  On success *child_cols is the
 * list of the positions, in the output of the child scan of the relation (child_target), of the
 * columns shipped, in the order of the arrays, and *keypos the position of the join key among them.
 * False with *why when the relation cannot be shipped.
 */
extern bool kingres_shard_repart_ship_text(Query *jq, const KsRepartCand *cand, int first_param,
										   PathTarget *child_target, List **child_cols, int *keypos,
										   const char **why);

/* ------------------------------------------------------------------------
 * The executor side
 * ---------------------------------------------------------------------- */

typedef struct KsRepartRows KsRepartRows;

/*
 * Read the rows of the child plan to its end and sort them by the group that owns the bucket of
 * the key.  `cols` are the positions in the child's output (0-based) of the columns to ship, in
 * the order of the arrays; `keypos` the position of the key among them.  `groups` the groups whose
 * rows are kept.  An error when the rows need more than the setting allows.
 */
extern KsRepartRows *kingres_shard_repart_collect(PlanState *child, const KsColocation *coloc,
												  List *cols, int keypos, List *groups,
												  MemoryContext cxt);

/* The array literal (text[] in text form) of one shipped column for one group. */
extern const char *kingres_shard_repart_array(KsRepartRows *rows, int group_id, int col);
extern int64 kingres_shard_repart_row_count(KsRepartRows *rows, int group_id);
extern int64 kingres_shard_repart_total_rows(KsRepartRows *rows);
extern int64 kingres_shard_repart_total_bytes(KsRepartRows *rows);

#endif							/* KINGRES_SHARD_REPARTITION_H */
