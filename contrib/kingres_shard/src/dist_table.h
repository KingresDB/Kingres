/*-------------------------------------------------------------------------
 *
 * dist_table.h
 *	  Creating distributed tables and their parents on other nodes.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/dist_table.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_DIST_TABLE_H
#define KINGRES_SHARD_DIST_TABLE_H

#include "postgres.h"

#include "lib/stringinfo.h"
#include "nodes/pg_list.h"

#include "catalog.h"
#include "node_conn.h"

/* The statements that recreate a table, by where they run (kingres.internal_table_ddl). */
typedef struct DdlSet
{
	StringInfoData local_pre;
	StringInfoData remote_pre;
	StringInfoData both;
	StringInfoData swap;
	StringInfoData fk;
	StringInfoData post;
	StringInfoData post_local;
} DdlSet;

extern char *kingres_shard_remote_table_script(Oid relid, const char *column);
extern char *kingres_shard_remote_fk_script(Oid relid, const char *column);
extern void kingres_shard_copy_table_rows(Oid relid, List *conns);

/* What the table operations of the rebuild engine (rebuild.c) share with the blocking variant. */
extern void kingres_shard_run_spi(const char *sql, int nargs, Oid *argtypes, Datum *values);
extern void kingres_shard_fetch_ddl(Oid relid, const char *column, bool shadow, DdlSet *ddl,
									MemoryContext cxt);
/* The same for a table the user partitioned: each bucket partition is itself PARTITION BY <partby> with one leaf per user partition. */
extern void kingres_shard_append_partition_tree(StringInfo buf, const char *nsp, const char *prefix, const char *parent,
								  const KsColocation *coloc, int group, const char *partby,
								  List *leafnames, List *leafbounds);
extern void kingres_shard_append_partitions(StringInfo buf, const char *nsp, const char *prefix,
											const char *parent, const KsColocation *coloc, int group, const char *with_clause);
/* the storage parameters, as a WITH (...) clause for CREATE TABLE, of the table (the recorded ones of a distributed table); never NULL */
extern char *kingres_shard_with_clause(Oid relid, char **reloptions_literal, MemoryContext cxt);
extern List *kingres_shard_remote_primaries(MemoryContext callercxt);
extern void kingres_shard_exec_on_node(KsConn *kc, const char *script);
extern char *kingres_shard_remote_tail_script(const DdlSet *ddl);
extern int	kingres_shard_choose_colocation(Oid coltype, Oid collation, Oid colocate_with,
											int requested_buckets, int *bucket_count, bool *created);

#endif							/* KINGRES_SHARD_DIST_TABLE_H */
