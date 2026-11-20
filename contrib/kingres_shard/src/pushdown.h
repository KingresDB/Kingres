/*-------------------------------------------------------------------------
 *
 * pushdown.h
 *	  The analysis of a pushdown unit (design/shard-phase2b.md, section 6): which
 *	  statements can be run whole by every shard group on its own buckets.
 *
 * The module answers three questions about one SELECT level, and nothing in it
 * depends on the statement that wraps that level (an INSERT today, a plain
 * SELECT, an aggregate or a join of later steps):
 *
 *	- kingres_shard_pushdown_analyze(): is it a unit (eligibility, 6.1 rules 2 to
 *	  6; the key class, 6.3; the join types, 9.1)?
 *	- kingres_shard_pushdown_is_key_var(): is an expression of its output a
 *	  distribution key of the class, not on the nullable side of an outer join?
 *	- kingres_shard_pushdown_closed(): does every output row depend on the rows of
 *	  one group only (6.4)?
 *
 * The expressions of the level (functions, types, collations) are the business
 * of the shippability walker (shippable.c); the caller runs it on the whole
 * statement it ships.  Rule 1 of 6.1 (the switches, the level of the query, the
 * session) belongs to the caller too.
 *
 * The first user is the colocated INSERT ... SELECT (P6, section 11.7): the
 * part of this file after "INSERT ... SELECT" decides it, on the origin and, on
 * the text that the origin sent, on the receiving group.  Both run the same
 * function on the Query they have (the origin on its own tree, checked once
 * more on the parse of the text it deparsed, so that the two decide alike).
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/pushdown.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_PUSHDOWN_H
#define KINGRES_SHARD_PUSHDOWN_H

#include "postgres.h"

#include "nodes/bitmapset.h"
#include "nodes/parsenodes.h"
#include "nodes/pg_list.h"

#include "catalog.h"

/*
 * One relation of the level.  Everything is copied by value: the catalog cache
 * can be reloaded by any call that reads it, and a pointer into it must not be
 * kept.
 */
typedef struct KsPdRel
{
	Index		rti;			/* range table index in the analysed query */
	Oid			relid;
	bool		reference;		/* a reference table: a full copy on every group */
	AttrNumber	dist_attno;		/* distributed table: the distribution column */
	bool		nullable;		/* on the nullable side of an outer join of the level */
	int			keyclass;		/* distributed table: the class of its key variable (6.3) */
} KsPdRel;

/* What the analysis found out about one level. */
typedef struct KsPushdown
{
	Query	   *query;			/* the level that was analysed */
	int			colocation_id;	/* the one colocation group of its distributed tables */
	KsColocation *coloc;		/* a copy of it */
	List	   *rels;			/* of KsPdRel *, in range table order */
	int			ndist;			/* distributed tables in the level */
	int			nclasses;		/* key classes: 1 for a unit */
} KsPushdown;

/*
 * Eligibility (6.1 rules 2 to 6 and 9.1) and the key class (6.3).  True when
 * the level is a unit, with *pd filled; false with *reason a static string that
 * says which rule failed.  The level is a SELECT of the statement's own range
 * table (an INSERT's source, a query, ...); the analysis does not look at what
 * surrounds it.
 */
extern bool kingres_shard_pushdown_analyze(Query *level, KsPushdown *pd, const char **reason);

/*
 * Is the expression (of the output of the analysed level: a target entry, a
 * grouping expression) a bare distribution key of the key class, of a table
 * that is not on the nullable side of an outer join?  A join alias, a
 * grouping column and a relabeling are looked through.  A row is produced by
 * the group that owns the bucket of such a value (6.3, 6.4).
 */
extern bool kingres_shard_pushdown_is_key_var(const KsPushdown *pd, Node *expr);

/* The range table index of the key variable `expr` is, or 0 when it is none. */
extern Index kingres_shard_pushdown_key_rti(const KsPushdown *pd, Node *expr);

/*
 * Is the level closed (6.4): every aggregate, DISTINCT and grouping is by a key
 * of the class, so each output row depends on the rows of one group only.  A
 * level without aggregate, DISTINCT or grouping is closed.  false with *reason.
 */
extern bool kingres_shard_pushdown_closed(const KsPushdown *pd, const char **reason);

/* The OIDs of the relations of the level (distributed and reference), without repeats. */
extern List *kingres_shard_pushdown_relids(const KsPushdown *pd);

/* ------------------------------------------------------------------------
 * INSERT ... SELECT (P6, 11.7)
 * ---------------------------------------------------------------------- */

/* The marker that starts the statement text of a colocated INSERT ... SELECT. */
#define KS_INSERT_SELECT_MARKER_PREFIX "/* kingres:insert-select-fragment "

/* "marker text" of the fragment for a placement number: the text, ending with a blank. */
extern char *kingres_shard_insert_select_marker(int64 place);

/* Parse the marker at the start of a statement text; false when it is not one. */
extern bool kingres_shard_parse_insert_select_marker(const char *query_string, int64 *place);

typedef struct KsInsertSelect
{
	Query	   *insert;			/* the INSERT to ship: a copy, without the defaults the groups apply */
	Query	   *source;			/* its source SELECT (inside insert) */
	KsPushdown	pd;				/* the analysis of the source */
	Oid			target_relid;
	int			colocation_id;
	AttrNumber	dist_attno;
	List	   *relids;			/* every relation of the statement, target included */
} KsInsertSelect;

/*
 * Decide a colocated INSERT ... SELECT: the target and every distributed table
 * of the source are of one colocation group, the source is a closed unit, the
 * row's distribution column is a key of the source's class, the statement is
 * shippable as a whole and the ON CONFLICT clause (none, DO NOTHING or DO UPDATE
 * under the rules of 11.7) cannot reach across groups.  `insert` is the
 * rewritten statement (the origin's, or the receiving group's parse of the
 * text); the result is a copy in *is, and the original is not changed.
 * False with *reason (a static string) when the statement is not one.
 */
extern bool kingres_shard_insert_select_analyze(Query *insert, KsInsertSelect *is,
												const char **reason);

/* ------------------------------------------------------------------------
 * A closed query (P4, P5 without order or limit; 6.4)
 * ---------------------------------------------------------------------- */

/* The marker that starts the statement text of a closed query that every group runs whole. */
#define KS_QUERY_MARKER_PREFIX "/* kingres:query-fragment "

/* "marker text" of the fragment for a placement number: the text, ending with a blank. */
extern char *kingres_shard_query_marker(int64 place);

/* Parse the marker at the start of a statement text; false when it is not one. */
extern bool kingres_shard_parse_query_marker(const char *query_string, int64 *place);

/* A partial aggregation (design 7): every group aggregates its own rows and returns the partial states. */
#define KS_PARTIAL_MARKER_PREFIX "/* kingres:partial-fragment "
extern char *kingres_shard_partial_marker(int64 place);
extern bool kingres_shard_parse_partial_marker(const char *query_string, int64 *place);

typedef struct KsQueryFragment
{
	KsPushdown	pd;				/* the analysis of the query */
	int			colocation_id;
	List	   *relids;			/* every relation of the query */
} KsQueryFragment;

extern bool kingres_shard_text_accepted(const char *body, Query *sent, bool partial, const char **why);
extern bool kingres_shard_aggregate_partial_ok(Aggref *a, const char **reason);
extern bool kingres_shard_partial_shape_ok(Query *query, const char **reason);
extern bool kingres_shard_partial_fragment_analyze(Query *query, KsQueryFragment *qf, const char **reason);

/*
 * Decide a closed query that every group runs whole on its own buckets and whose rows are
 * concatenated: it is a unit (6.1), closed (6.4), has no LIMIT or OFFSET (they are applied by the
 * node that gathers the rows), and passes the shippability walker as a whole statement (5).  The
 * origin runs this function on the query it ships (and again on the parse of its text), and a
 * receiving group on the parse of the text it received, so that both decide on the same shape.
 * False with *reason (a static string) when the query is not one.
 */
extern bool kingres_shard_query_fragment_analyze(Query *query, KsQueryFragment *qf,
												 const char **reason);

#endif							/* KINGRES_SHARD_PUSHDOWN_H */
