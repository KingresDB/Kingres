/*-------------------------------------------------------------------------
 *
 * shippable.h
 *	  Deciding whether an expression or a whole statement can be sent to
 *	  another node and mean the same there.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/shippable.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_SHIPPABLE_H
#define KINGRES_SHARD_SHIPPABLE_H

#include "postgres.h"

#include "nodes/parsenodes.h"

/*
 * kingres_shard_clause_shippable
 *
 * A restriction clause on one table (range table index varno of the query it
 * belongs to) that may be sent to the other shard groups with the fragment
 * that scans the table: it references only that table's columns, constants and
 * external parameters, and uses only built-in immutable functions and
 * operators on built-in types with default (or C) collations.
 */
extern bool kingres_shard_clause_shippable(Node *clause, Index varno);

/*
 * kingres_shard_stmt_shippable
 *
 * A whole INSERT, UPDATE or DELETE statement that may be executed on every
 * node (a reference table write) or on every shard group (a write that spans
 * groups): every expression passes the same test and every relation is a
 * reference table or is shipped_relid itself.  On failure *reason says why
 * (a static string).
 */
extern bool kingres_shard_stmt_shippable(Query *query, Oid shipped_relid,
										  const char **reason);

/*
 * kingres_shard_stmt_shippable_colocated
 *
 * The multi-relation mode of the walker (design 5 and 11.7): a whole INSERT
 * whose source reads distributed tables of the colocation group colocation_id
 * (and reference tables) and writes target_relid.  Every expression passes the
 * same tests; the relations of the group are accepted wherever the statement's
 * own level and its one source level read them, and everything that could read a
 * table behind them (a sub-select in an expression, a function, a VALUES list, a
 * CTE, a whole-row reference) is refused.  That the tables are joined so that each
 * group can answer for its own rows is not decided here but by pushdown.c.
 */
extern bool kingres_shard_stmt_shippable_colocated(Query *query, Oid target_relid,
													int colocation_id,
													const char **reason);

extern bool kingres_shard_stmt_shippable_merge(Query *query, int colocation_id, const char **reason);

extern bool kingres_shard_stmt_shippable_query(Query *query, int colocation_id, const char **reason);

/* The extensions of the compiled-in list (design 5.5) */
extern PGDLLIMPORT char *kingres_shard_shippable_extensions;
extern bool kingres_shard_shippable_extension_known(const char *name);
extern bool kingres_shard_extension_aggregate_ok(Oid aggfnoid);
extern char *kingres_shard_shippable_ext_used(void);
extern bool kingres_shard_ext_token_ok(const char *s, size_t len);
extern bool kingres_shard_ext_list_valid(const char *list);
extern void kingres_shard_ext_list_check(const char *list);
extern char *kingres_shard_extension_version(const char *name);

extern bool kingres_shard_stmt_shippable_partial(Query *query, int colocation_id,
												  const char **reason);

#endif							/* KINGRES_SHARD_SHIPPABLE_H */
