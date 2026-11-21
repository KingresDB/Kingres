/*-------------------------------------------------------------------------
 *
 * schema_ddl.h
 *	  DDL on a distributed schema (phase 11): which statements belong to one,
 *	  which are refused, and the node-local steps that register what they make.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/schema_ddl.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_SCHEMA_DDL_H
#define KINGRES_SHARD_SCHEMA_DDL_H

#include "postgres.h"

#include "nodes/parsenodes.h"
#include "nodes/pg_list.h"

#include "catalog.h"
#include "node_conn.h"

/* What the hook learns about one utility statement (kingres_shard_schema_ddl_classify). */
typedef struct KsSchemaDdl
{
	bool		touches;			/* the objects of the statement are in one distributed schema, or it changes one */
	bool		create_schema;		/* a CREATE SCHEMA whose schema becomes a distributed schema */
	char	   *create_name;		/* ... and its name */
	bool		drops_schema;		/* DROP SCHEMA of a distributed schema */
	bool		may_create_tables;	/* the statement can make a table: the tables of the schema are registered afterwards */
	List	   *schema_names;		/* of char *: the distributed schemas the statement touches */
	List	   *colocations;		/* of int: their colocation groups */
	char	   *refuse_what;		/* the statement kind that is refused in a distributed schema ("" = see refuse_message) */
	char	   *refuse_message;		/* the whole message when refuse_what is NULL and this is not */
	char	   *refuse_hint;
	List	   *verify_tables;		/* of RangeVar *: the tables whose structure is compared across the nodes afterwards */
	int			set_mode;			/* ALTER TABLE ... SET SCHEMA: 1 into a distributed schema, 2 out of one, 3 between two of one owner */
	int			set_owner_group;	/* the shard group that holds the rows (the owner of the schema involved) */
	char	   *set_old_name;		/* the table before the statement, schema-qualified and quoted */
	RangeVar   *set_new_rel;		/* the table after it */
} KsSchemaDdl;

extern bool kingres_shard_enable_schema_routing;

extern void kingres_shard_schema_ddl_init(void);

/* Does this kind of statement have to be looked at at all? */
extern bool kingres_shard_schema_ddl_interesting(Node *parsetree);

/*
 * Fills *sd.  forwarded is true in the session of another node, which never decides to distribute a schema or
 * refuses anything (the origin has done both): such a session only learns which schemas the statement touches.
 */
extern void kingres_shard_schema_ddl_classify(Node *parsetree, const KsCatalog *cat, bool forwarded, KsSchemaDdl *sd);

/* The limit of distributed schemas, the group and the colocation id of a new one (map group primary, under the cluster lock). */
extern void kingres_shard_schema_check_limit(const KsCatalog *cat);
extern int	kingres_shard_schema_choose_group(void);
extern int	kingres_shard_schema_next_colocation(void);

/* Node-local steps, run with the rights of the bootstrap superuser. */
extern void kingres_shard_schema_register_local(const char *name, int colocation_id, int group_id);
extern int	kingres_shard_schema_sync_tables_local(const char *name);
extern char *kingres_shard_schema_fingerprint_local(Oid relid);
extern List *kingres_shard_schema_table_statements_local(Oid relid);
extern int	kingres_shard_schema_reconcile_local(void);
extern void kingres_shard_schema_table_op_local(const char *op, const char *rel);
extern void kingres_shard_schema_verify_remote(KsConn *kc, int node_id, Oid relid, const char *local_fingerprint);

#endif							/* KINGRES_SHARD_SCHEMA_DDL_H */
