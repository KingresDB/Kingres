/*-------------------------------------------------------------------------
 *
 * shippable.h
 *	  Deciding whether an expression or a whole statement can be sent to
 *	  another node and mean the same there.
 *
 * SPDX-License-Identifier: PostgreSQL
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

#endif							/* KINGRES_SHARD_SHIPPABLE_H */
