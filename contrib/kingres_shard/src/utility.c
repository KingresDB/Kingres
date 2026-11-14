/*-------------------------------------------------------------------------
 *
 * utility.c
 *	  A ProcessUtility hook that refuses utility statements that would act on a
 *	  distributed table on this node only.
 *
 * The planner hook sees queries (SELECT, INSERT, UPDATE, DELETE, MERGE), never
 * utility statements, so before this hook TRUNCATE of a distributed table
 * emptied the local partitions and reported success, ALTER TABLE changed the
 * table on one node, and COPY FROM wrote to the local partitions only (review
 * finding 3).  Propagating DDL to every node is a later phase; until then, on
 * a cluster with more than one shard group, every utility statement that
 * would change a distributed table (or one of its partitions or indexes) is
 * refused with a "not supported on distributed tables yet" error.  What is
 * not refused:
 *
 *	- statements on other tables, and statements that only read
 *	  (EXPLAIN, COPY TO, LOCK);
 *	- the per-node maintenance commands VACUUM, ANALYZE, REINDEX and CLUSTER,
 *	  which change no logical content and are run on each node anyway;
 *	- GRANT, REVOKE, COMMENT and SECURITY LABEL: they have to be repeated on
 *	  every node by hand (the privileges and comments of a table are not
 *	  copied), and a node that lacks one fails loudly, never silently.
 *
 * A superuser who runs the statement on every node by hand sets
 * kingres_shard.enable_routing = off, which switches this hook off too.
 * With one shard group every statement works as before, except the few that
 * would destroy the structure of the distribution whatever the number of
 * nodes: dropping, renaming or retyping the distribution column and attaching,
 * detaching, dropping or creating a bucket partition.
 *
 * Drops that cascade (DROP SCHEMA ... CASCADE, DROP TYPE ... CASCADE) are not
 * named by the statement; the event trigger kingres.forget_dropped_tables()
 * is the backstop for those.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/utility.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "catalog/index.h"
#include "catalog/namespace.h"
#include "catalog/partition.h"
#include "catalog/pg_class.h"
#include "commands/extension.h"
#include "nodes/parsenodes.h"
#include "tcop/cmdtag.h"
#include "tcop/utility.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"

#include "catalog.h"
#include "kingres_shard.h"
#include "utility.h"

static ProcessUtility_hook_type prev_ProcessUtility = NULL;

static void kingres_shard_process_utility(PlannedStmt *pstmt,
										   const char *queryString,
										   bool readOnlyTree,
										   ProcessUtilityContext context,
										   ParamListInfo params,
										   QueryEnvironment *queryEnv,
										   DestReceiver *dest,
										   QueryCompletion *qc);

/*
 * kingres_shard_utility_init
 *
 * Install the hook, chaining to a previous one.
 */
void
kingres_shard_utility_init(void)
{
	prev_ProcessUtility = ProcessUtility_hook;
	ProcessUtility_hook = kingres_shard_process_utility;
}

/*
 * The name of the statement for the error message.
 */
static const char *
utility_name(Node *parsetree)
{
	if (IsA(parsetree, TruncateStmt))
		return "TRUNCATE";
	if (IsA(parsetree, CopyStmt))
		return "COPY FROM";
	return GetCommandTagName(CreateCommandTag(parsetree));
}

/*
 * find_dist
 *
 * The distributed table that a relation belongs to: the table itself, the
 * parent of a bucket partition, or the table of an index of either.  NULL when
 * the relation has nothing to do with a distributed table.
 */
static const KsDistTable *
find_dist(const KsCatalog *cat, Oid relid)
{
	int			depth;

	for (depth = 0; depth < 4 && OidIsValid(relid); depth++)
	{
		const KsDistTable *dt = hash_search(cat->dist_tables, &relid,
											HASH_FIND, NULL);
		char		kind;

		if (dt != NULL)
			return dt;
		kind = get_rel_relkind(relid);
		if (kind == RELKIND_INDEX || kind == RELKIND_PARTITIONED_INDEX)
			relid = IndexGetRelation(relid, true);
		else if (get_rel_relispartition(relid))
			relid = get_partition_parent(relid, true);
		else
			return NULL;
	}
	return NULL;
}

/*
 * Is the relation a bucket partition of a distributed table (not the table,
 * not an index)?
 */
static bool
is_bucket_partition(const KsCatalog *cat, Oid relid)
{
	return OidIsValid(relid) &&
		hash_search(cat->dist_tables, &relid, HASH_FIND, NULL) == NULL &&
		get_rel_relkind(relid) == RELKIND_RELATION &&
		find_dist(cat, relid) != NULL;
}

/*
 * reject
 *
 * Raise the error for a statement that would act on distributed table dt on
 * this node only (or, with structural, that would damage its structure).
 */
static void
reject(Node *parsetree, const KsDistTable *dt, bool structural)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("%s is not supported on distributed tables yet",
					utility_name(parsetree)),
			 structural ?
			 errdetail("The statement would change the distribution column or a bucket partition of distributed table \"%s\".",
					   get_rel_name(dt->relid)) :
			 errdetail("The statement would change distributed table \"%s\" on this node only, but the table exists on every node of the cluster; running DDL on every node is a later phase.",
					   get_rel_name(dt->relid)),
			 errhint("A superuser can run the statement on every node by hand with kingres_shard.enable_routing = off.")));
}

/*
 * guard_relid
 *
 * Refuse the statement if relid belongs to a distributed table and the
 * cluster has more than one shard group, or if structural is true.
 */
static void
guard_relid(const KsCatalog *cat, Node *parsetree, Oid relid, bool structural)
{
	const KsDistTable *dt;

	if (!OidIsValid(relid))
		return;
	dt = find_dist(cat, relid);
	if (dt == NULL)
		return;
	if (cat->ngroups <= 1 && !structural)
		return;
	reject(parsetree, dt, structural);
}

/*
 * guard_rangevar
 *
 * guard_relid() for a relation named by a RangeVar; a relation that does not
 * exist is left for the statement itself to report.
 */
static void
guard_rangevar(const KsCatalog *cat, Node *parsetree, RangeVar *rv,
			   bool structural)
{
	if (rv == NULL)
		return;
	guard_relid(cat, parsetree, RangeVarGetRelid(rv, NoLock, true), structural);
}

/*
 * guard_schema
 *
 * Refuse a statement that acts on a whole schema (DROP SCHEMA, ALTER SCHEMA ...
 * RENAME) if the schema holds a distributed table.
 */
static void
guard_schema(const KsCatalog *cat, Node *parsetree, const char *nspname)
{
	Oid			nspid;
	HASH_SEQ_STATUS scan;
	KsDistTable *dt;

	if (cat->ngroups <= 1)
		return;
	nspid = get_namespace_oid(nspname, true);
	if (!OidIsValid(nspid))
		return;
	hash_seq_init(&scan, cat->dist_tables);
	while ((dt = (KsDistTable *) hash_seq_search(&scan)) != NULL)
	{
		if (get_rel_namespace(dt->relid) == nspid)
		{
			hash_seq_term(&scan);
			reject(parsetree, dt, false);
		}
	}
}

/*
 * alter_is_structural
 *
 * Does an ALTER TABLE of distributed table dt change its distribution column
 * or attach or detach a partition?
 */
static bool
alter_is_structural(AlterTableStmt *stmt, const KsDistTable *dt)
{
	ListCell   *lc;

	foreach(lc, stmt->cmds)
	{
		AlterTableCmd *cmd = lfirst_node(AlterTableCmd, lc);

		switch (cmd->subtype)
		{
			case AT_AttachPartition:
			case AT_DetachPartition:
			case AT_DetachPartitionFinalize:
				return true;
			case AT_DropColumn:
			case AT_AlterColumnType:
				if (cmd->name != NULL &&
					strcmp(cmd->name, NameStr(dt->dist_column)) == 0)
					return true;
				break;
			default:
				break;
		}
	}
	return false;
}

/*
 * guard_utility
 *
 * The decision for one utility statement; see the file header.
 */
static void
guard_utility(Node *parsetree)
{
	const KsCatalog *cat;
	ListCell   *lc;

	switch (nodeTag(parsetree))
	{
		case T_TruncateStmt:
		case T_AlterTableStmt:
		case T_RenameStmt:
		case T_AlterObjectSchemaStmt:
		case T_IndexStmt:
		case T_DropStmt:
		case T_CopyStmt:
		case T_CreateTrigStmt:
		case T_RuleStmt:
		case T_CreatePolicyStmt:
		case T_AlterPolicyStmt:
		case T_CreateStatsStmt:
		case T_CreateStmt:
			break;
		default:
			return;
	}

	cat = kingres_shard_catalog();
	if (!cat->present || cat->ndist_tables == 0)
		return;

	switch (nodeTag(parsetree))
	{
		case T_TruncateStmt:
			foreach(lc, ((TruncateStmt *) parsetree)->relations)
				guard_rangevar(cat, parsetree, (RangeVar *) lfirst(lc), false);
			break;

		case T_AlterTableStmt:
			{
				AlterTableStmt *stmt = (AlterTableStmt *) parsetree;
				Oid			relid;
				const KsDistTable *dt;

				relid = stmt->relation == NULL ? InvalidOid :
					RangeVarGetRelid(stmt->relation, NoLock, true);
				dt = OidIsValid(relid) ? find_dist(cat, relid) : NULL;
				if (dt == NULL)
					break;
				guard_relid(cat, parsetree, relid,
							alter_is_structural(stmt, dt) &&
							hash_search(cat->dist_tables, &relid, HASH_FIND, NULL) != NULL);
			}
			break;

		case T_RenameStmt:
			{
				RenameStmt *stmt = (RenameStmt *) parsetree;
				Oid			relid = InvalidOid;
				bool		structural = false;

				switch (stmt->renameType)
				{
					case OBJECT_SCHEMA:
						guard_schema(cat, parsetree, stmt->subname);
						return;
					case OBJECT_TABLE:
					case OBJECT_INDEX:
					case OBJECT_COLUMN:
					case OBJECT_TABCONSTRAINT:
					case OBJECT_TRIGGER:
					case OBJECT_POLICY:
					case OBJECT_RULE:
						if (stmt->relation != NULL)
							relid = RangeVarGetRelid(stmt->relation, NoLock, true);
						break;
					default:
						return;
				}
				if (!OidIsValid(relid))
					break;
				if (stmt->renameType == OBJECT_TABLE)
					structural = is_bucket_partition(cat, relid);
				else if (stmt->renameType == OBJECT_COLUMN)
				{
					const KsDistTable *dt = hash_search(cat->dist_tables, &relid,
														HASH_FIND, NULL);

					structural = dt != NULL && stmt->subname != NULL &&
						strcmp(stmt->subname, NameStr(dt->dist_column)) == 0;
				}
				guard_relid(cat, parsetree, relid, structural);
			}
			break;

		case T_AlterObjectSchemaStmt:
			{
				AlterObjectSchemaStmt *stmt = (AlterObjectSchemaStmt *) parsetree;

				if ((stmt->objectType == OBJECT_TABLE ||
					 stmt->objectType == OBJECT_INDEX) && stmt->relation != NULL)
				{
					Oid			relid = RangeVarGetRelid(stmt->relation, NoLock, true);

					guard_relid(cat, parsetree, relid,
								is_bucket_partition(cat, relid));
				}
			}
			break;

		case T_IndexStmt:
			guard_rangevar(cat, parsetree, ((IndexStmt *) parsetree)->relation,
						   false);
			break;

		case T_DropStmt:
			{
				DropStmt   *stmt = (DropStmt *) parsetree;

				foreach(lc, stmt->objects)
				{
					Node	   *obj = (Node *) lfirst(lc);
					List	   *names = IsA(obj, List) ? (List *) obj : NIL;
					Oid			relid;

					switch (stmt->removeType)
					{
						case OBJECT_SCHEMA:
							if (IsA(obj, String))
								guard_schema(cat, parsetree, strVal(obj));
							break;
						case OBJECT_TABLE:
						case OBJECT_INDEX:
							if (names == NIL)
								break;
							relid = RangeVarGetRelid(makeRangeVarFromNameList(names),
													 NoLock, true);
							guard_relid(cat, parsetree, relid,
										is_bucket_partition(cat, relid));
							break;
						case OBJECT_TRIGGER:
						case OBJECT_POLICY:
						case OBJECT_RULE:
							/* the names are those of the table, then of the object */
							if (list_length(names) < 2)
								break;
							guard_rangevar(cat, parsetree,
										   makeRangeVarFromNameList(list_copy_head(names,
																				   list_length(names) - 1)),
										   false);
							break;
						default:
							break;
					}
				}
			}
			break;

		case T_CopyStmt:
			{
				CopyStmt   *stmt = (CopyStmt *) parsetree;

				if (stmt->is_from)
					guard_rangevar(cat, parsetree, stmt->relation, false);
			}
			break;

		case T_CreateTrigStmt:
			guard_rangevar(cat, parsetree, ((CreateTrigStmt *) parsetree)->relation,
						   false);
			break;

		case T_RuleStmt:
			guard_rangevar(cat, parsetree, ((RuleStmt *) parsetree)->relation,
						   false);
			break;

		case T_CreatePolicyStmt:
			guard_rangevar(cat, parsetree, ((CreatePolicyStmt *) parsetree)->table,
						   false);
			break;

		case T_AlterPolicyStmt:
			guard_rangevar(cat, parsetree, ((AlterPolicyStmt *) parsetree)->table,
						   false);
			break;

		case T_CreateStatsStmt:
			foreach(lc, ((CreateStatsStmt *) parsetree)->relations)
			{
				if (IsA(lfirst(lc), RangeVar))
					guard_rangevar(cat, parsetree, (RangeVar *) lfirst(lc), false);
			}
			break;

		case T_CreateStmt:
			{
				CreateStmt *stmt = (CreateStmt *) parsetree;

				/* A new partition of a distributed table is never ours to add. */
				foreach(lc, stmt->inhRelations)
				{
					if (IsA(lfirst(lc), RangeVar))
						guard_rangevar(cat, parsetree, (RangeVar *) lfirst(lc),
									   stmt->partbound != NULL);
				}
			}
			break;

		default:
			break;
	}
}

/*
 * kingres_shard_process_utility
 *
 * The hook: decide, then hand over to the previous hook or the standard
 * function.  Statements that run while the extension itself is created, and
 * all statements while routing is off, are not looked at.
 */
static void
kingres_shard_process_utility(PlannedStmt *pstmt, const char *queryString,
							   bool readOnlyTree,
							   ProcessUtilityContext context,
							   ParamListInfo params,
							   QueryEnvironment *queryEnv,
							   DestReceiver *dest, QueryCompletion *qc)
{
	if (kingres_shard_enable_routing && !creating_extension)
		guard_utility(pstmt->utilityStmt);

	if (prev_ProcessUtility)
		prev_ProcessUtility(pstmt, queryString, readOnlyTree, context, params,
							queryEnv, dest, qc);
	else
		standard_ProcessUtility(pstmt, queryString, readOnlyTree, context,
								params, queryEnv, dest, qc);
}
