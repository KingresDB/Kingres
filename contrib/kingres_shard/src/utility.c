/*-------------------------------------------------------------------------
 *
 * utility.c
 *	  A ProcessUtility hook that propagates DDL on distributed and reference
 *	  tables to every node, or refuses it.
 *
 * The planner hook sees queries (SELECT, INSERT, UPDATE, DELETE, MERGE), never
 * utility statements, so without this hook TRUNCATE of a distributed table
 * empties the local partitions only, ALTER TABLE changes the table on one node,
 * and COPY FROM writes to the local partitions only (review finding 3).  The
 * hook looks at every utility statement that names a distributed or reference
 * table (design/shard-phase2a.md, section 4):
 *
 *	- Supported statements (ALTER TABLE with ADD/DROP COLUMN, SET/DROP DEFAULT,
 *	  SET/DROP NOT NULL, ADD/DROP/VALIDATE CONSTRAINT and OWNER TO; RENAME of the
 *	  table and of a column; CREATE INDEX and DROP INDEX; DROP TABLE; TRUNCATE;
 *	  GRANT and REVOKE; COMMENT on the table and its columns) are executed on the
 *	  map group primary and then, in the same distributed transaction (two-phase
 *	  commit), on every other node, with the text of the statement and the
 *	  search_path and literal settings of the session.  On any other node they are
 *	  refused with a hint to use the map group primary.  The cluster-wide
 *	  catalog lock serializes them with the other catalog changes.
 *	- VACUUM, ANALYZE, REINDEX and LOCK act on one node by nature and are
 *	  allowed, local.
 *	- COPY <distributed table> TO is run as COPY (SELECT <columns> FROM <table>)
 *	  TO, which the planner hook answers from every shard group.
 *	- Everything else that names such a table is refused with "<statement> is
 *	  not supported on distributed tables yet".
 *
 * A bucket partition is not a table of the user: statements that act on one
 * (rename, drop, attach, detach, create) and statements that change the
 * distribution column are refused whatever the number of nodes.  A cluster with
 * one shard group has no other node to propagate to: the supported statements
 * just run, but renaming a distributed table renames its partitions too, on
 * every node, because the partition names are derived from the table name.
 *
 * The statements that arrive propagated (from the map group primary, in a
 * session with kingres_shard.forwarded on) are executed without propagating
 * again.  That flag can be set by whoever connects, so on a non-map node it is
 * a way around the "run DDL on the map group primary" rule for a user who is
 * allowed to run the statement anyway: it never gives a privilege (every
 * statement is checked against the user's own rights), it only lets the owner
 * of a table make it differ between nodes, which is an inconsistency the owner
 * can cause with enable_routing off, as a superuser, too.
 *
 * The text of the statement is sent as written, so a query string with several
 * statements is refused (the statement text cannot be told apart).  Names
 * resolve identically because the remote transaction runs with the search_path
 * of the session.
 *
 * Drops that cascade (DROP SCHEMA ... CASCADE, DROP TYPE ... CASCADE) are not
 * named by the statement; the event trigger kingres.forget_dropped_tables() is
 * the backstop for those.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/utility.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "access/table.h"
#include "access/tupdesc.h"
#include "access/xact.h"
#include "catalog/index.h"
#include "catalog/namespace.h"
#include "catalog/partition.h"
#include "catalog/pg_class.h"
#include "catalog/pg_authid.h"
#include "catalog/pg_inherits.h"
#include "catalog/pg_namespace.h"
#include "catalog/pg_type.h"
#include "commands/copy.h"
#include "commands/extension.h"
#include "commands/tablecmds.h"
#include "executor/spi.h"
#include "miscadmin.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "nodes/parsenodes.h"
#include "nodes/value.h"
#include "parser/parse_type.h"
#include "storage/lmgr.h"
#include "storage/lockdefs.h"
#include "tcop/cmdtag.h"
#include "tcop/utility.h"
#include "utils/acl.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/rel.h"
#include "utils/ruleutils.h"
#include "utils/syscache.h"
#include "utils/typcache.h"

#include "bucket_move.h"
#include "catalog.h"
#include "forward_auth.h"
#include "kingres_shard.h"
#include "node_admin.h"
#include "node_conn.h"
#include "row_router.h"
#include "schema_ddl.h"
#include "utility.h"

static ProcessUtility_hook_type prev_ProcessUtility = NULL;

PG_FUNCTION_INFO_V1(kingres_internal_ddl_authorized);

/* > 0 while the hook runs a statement that it propagates or that arrived propagated. */
/*
 * The bypass flags are scoped to what the extension itself issues (review
 * finding S2-14), never to the user code that runs while an authorized
 * statement executes (a function in a CHECK constraint or an index
 * expression, a trigger):
 *
 *	ddl_authorized_depth		an authorized DDL statement is executing; only
 *								the statements PostgreSQL generates for it
 *								(the validation join of a foreign key) are
 *								planned without routing (planner.c)
 *	ddl_drop_authorized			the statement that is executing was authorized
 *								by the hook (a DROP or ALTER it runs or
 *								propagates): the drop guard lets what it drops
 *								go.  It is set for each statement the hook
 *								handles and restored afterwards, so a statement
 *								that user code runs inside it does not inherit it
 *	ddl_internal_depth			the extension's own SPI statements (the rename
 *								of bucket partitions) are executing, and the
 *								hook leaves them alone
 */
static int	ddl_internal_depth = 0;
static bool ddl_drop_authorized = false;
static int	ddl_authorized_depth = 0;

/*
 * kingres.internal_ddl_authorized() returns bool
 *
 * Used by the sql_drop event trigger to let a DROP that the extension itself
 * runs (or propagates) through its drop guard.
 */
Datum
kingres_internal_ddl_authorized(PG_FUNCTION_ARGS)
{
	PG_RETURN_BOOL(ddl_drop_authorized);
}

/* What the hook decided about one statement. */
typedef struct UtilDecision
{
	bool		propagate;		/* run on the other nodes too */
	bool		authorize;		/* let the drop guard pass */
	Oid			rename_relid;	/* a hash table renamed: rename its partitions */
	Oid			owner_relid;	/* a hash table whose owner changes: change its partitions' owner too */
	Oid			owner_role;
	List	   *colocations;	/* of int: colocation groups of the hash tables concerned */
	Node	   *replacement;	/* the statement to run instead of the one that came */
	bool		copy_route;		/* COPY FROM into a distributed table: the row router loads it */
} UtilDecision;

/*
 * A statement on a table of a colocation group must not run while a bucket of
 * the group is being moved: the copy on the target is a standalone table that
 * the statement would not reach (kingres.internal_assert_no_move()).
 */
static void
note_colocation(UtilDecision *d, const KsDistTable *dt)
{
	if (dt != NULL && !dt->is_reference)
		d->colocations = list_append_unique_int(d->colocations, dt->colocation_id);
}

/*
 * A statement whose objects are in a distributed schema (schema_ddl.c) is run here, then on every other
 * node in the same transaction, like the DDL on a hash-distributed table; the drop guard lets it go.
 */
static void
merge_schema_decision(UtilDecision *d, const KsSchemaDdl *sd)
{
	ListCell   *lc;

	if (!sd->touches && !sd->create_schema)
		return;
	d->propagate = true;
	d->authorize = true;
	foreach(lc, sd->colocations)
		d->colocations = list_append_unique_int(d->colocations, lfirst_int(lc));
}

/*
 * CREATE TABLE ... AS and SELECT ... INTO a table of a distributed schema (design 6.7): the creation of the empty table, which every node runs
 * like any CREATE TABLE of a distributed schema, then an INSERT ... SELECT of the same query, whose rows go to the owner (by the row router when
 * this node does not hold the schema).  Returns false when the statement is not one of these (the ordinary path decides).
 */
static bool
schema_ctas(CreateTableAsStmt *ctas, QueryCompletion *qc)
{
	IntoClause *into = ctas->into;
	const KsCatalog *cat = kingres_shard_catalog();
	Oid			nsp;
	Query	   *q;
	StringInfoData cols;
	char	   *qname;
	ListCell   *lc;
	int			i = 0;
	uint64		processed = 0;

	if (!cat->present || cat->ndist_schemas == 0 || ctas->objtype != OBJECT_TABLE || into->rel == NULL ||
		into->rel->relpersistence == RELPERSISTENCE_TEMP)
		return false;
	nsp = RangeVarGetCreationNamespace(into->rel);
	if (kingres_shard_lookup_dist_schema(cat, nsp) == NULL)
		return false;
	if (into->rel->relpersistence != RELPERSISTENCE_PERMANENT || into->options != NIL || into->onCommit != ONCOMMIT_NOOP ||
		into->tableSpaceName != NULL || into->accessMethod != NULL)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("CREATE TABLE AS into a distributed schema takes no storage option, tablespace, access method, ON COMMIT or UNLOGGED"),
				 errhint("Create the table with CREATE TABLE, then INSERT ... SELECT.")));
	q = castNode(Query, ctas->query);
	if (q->commandType == CMD_UTILITY)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("CREATE TABLE AS EXECUTE into a distributed schema is not supported"),
				 errhint("Write the query itself: CREATE TABLE ... AS SELECT ...")));
	if (ctas->if_not_exists && OidIsValid(get_relname_relid(into->rel->relname, nsp)))
	{
		ereport(NOTICE,
				(errcode(ERRCODE_DUPLICATE_TABLE),
				 errmsg("relation \"%s\" already exists, skipping", into->rel->relname)));
		if (qc != NULL)
			SetQueryCompletion(qc, CMDTAG_SELECT, 0);
		return true;
	}
	qname = quote_qualified_identifier(get_namespace_name(nsp), into->rel->relname);
	initStringInfo(&cols);
	foreach(lc, q->targetList)
	{
		TargetEntry *te = lfirst_node(TargetEntry, lc);
		const char *name;
		Oid			coll;

		if (te->resjunk)
			continue;
		name = (into->colNames != NIL && i < list_length(into->colNames)) ? strVal(list_nth(into->colNames, i)) : te->resname;
		appendStringInfo(&cols, "%s%s %s", cols.len > 0 ? ", " : "", quote_identifier(name),
						 format_type_with_typemod(exprType((Node *) te->expr), exprTypmod((Node *) te->expr)));
		coll = exprCollation((Node *) te->expr);
		if (OidIsValid(coll) && coll != get_typcollation(exprType((Node *) te->expr)))
			appendStringInfo(&cols, " COLLATE %s", generate_collation_name(coll));
		i++;
	}
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	if (SPI_execute(psprintf("CREATE TABLE %s (%s)", qname, cols.data), false, 0) < 0)
		elog(ERROR, "CREATE TABLE for CREATE TABLE AS failed");
	if (!into->skipData)
	{
		char	   *sel = pg_get_querydef(q, false);

		if (SPI_execute(psprintf("INSERT INTO %s %s", qname, sel), false, 0) != SPI_OK_INSERT)
			elog(ERROR, "INSERT for CREATE TABLE AS failed");
		processed = SPI_processed;
	}
	SPI_finish();
	if (qc != NULL)
		SetQueryCompletion(qc, ctas->is_select_into ? CMDTAG_SELECT : CMDTAG_SELECT, processed);
	return true;
}

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

/* The name of the statement for the error message. */
static const char *
utility_name(Node *parsetree)
{
	if (IsA(parsetree, TruncateStmt))
		return "TRUNCATE";
	if (IsA(parsetree, CopyStmt))
		return ((CopyStmt *) parsetree)->is_from ? "COPY FROM" : "COPY TO";
	return GetCommandTagName(CreateCommandTag(parsetree));
}

/*
 * find_dist
 *
 * The distributed or reference table that a relation belongs to: the table
 * itself, the parent of a bucket partition, or the table of an index of either.
 * NULL when the relation has nothing to do with one.
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

/* The table itself (not a partition or an index) when it is distributed or a reference table. */
static const KsDistTable *
direct_dist(const KsCatalog *cat, Oid relid)
{
	if (!OidIsValid(relid))
		return NULL;
	return hash_search(cat->dist_tables, &relid, HASH_FIND, NULL);
}

/* Is the relation a bucket partition of a distributed table, or an index of one? */
static bool
is_bucket_child(const KsCatalog *cat, Oid relid)
{
	return OidIsValid(relid) &&
		direct_dist(cat, relid) == NULL &&
		find_dist(cat, relid) != NULL;
}

static Oid
rangevar_relid(RangeVar *rv)
{
	if (rv == NULL)
		return InvalidOid;
	return RangeVarGetRelid(rv, NoLock, true);
}

/*
 * refuse_copy_of_schema_shell
 *
 * COPY of the relation itself does not go through the planner hook (design/shard-phase11.md 6.8, item 3).
 * For a table of a distributed schema that another shard group holds, this node has an empty copy:
 * COPY TO would answer with no rows and COPY FROM would write to the copy.  Both are refused with a
 * message that says where the rows are; the form COPY (SELECT ...) TO goes through the planner and reads
 * the table from its owner.
 */
static void
refuse_copy_of_schema_shell(CopyStmt *stmt)
{
	const KsCatalog *cat;
	const KsDistTable *st;
	Oid			relid;
	int			owner;
	const KsNode *node;

	if (stmt->relation == NULL)
		return;
	cat = kingres_shard_catalog();
	if (!cat->present || cat->nschema_tables == 0)
		return;
	relid = rangevar_relid(stmt->relation);
	if (!OidIsValid(relid))
		return;
	st = kingres_shard_lookup_schema_table(cat, relid);
	if (st == NULL)
		return;
	owner = kingres_shard_schema_table_owner(st);
	if (owner == cat->local_group_id)
		return;
	node = kingres_shard_group_primary(owner);
	if (stmt->is_from)
	{
		/* the rows go to the owner through the row router (design 6.8 item 3); the handler is called by the caller */
		if (kingres_shard_enable_copy_routing && stmt->query == NULL)
			return;
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("COPY FROM into table \"%s\" of a distributed schema, which shard group %d holds, is not supported from shard group %d with kingres_shard.enable_copy_routing off",
						get_rel_name(relid), owner, cat->local_group_id),
				 node ? errhint("Run COPY on node %d (%s:%d), the primary of shard group %d.",
								node->node_id, node->host, node->port, owner) : 0));
	}
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("COPY \"%s\" TO would read the empty copy of the table on this node; shard group %d holds the rows",
					get_rel_name(relid), owner),
			 errhint("Use COPY (SELECT * FROM %s) TO, which reads the table from shard group %d.",
					 quote_qualified_identifier(get_namespace_name(get_rel_namespace(relid)), get_rel_name(relid)),
					 owner)));
}

/*
 * reject
 *
 * Raise the error for a statement that is not supported on the distributed or
 * reference table dt.  structural: the statement would damage the distribution
 * (the distribution column or a bucket partition), whatever the cluster size.
 */
static void
reject(Node *parsetree, const KsDistTable *dt, bool structural, const char *detail)
{
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("%s is not supported on %s tables yet",
					utility_name(parsetree),
					dt->is_reference ? "reference" : "distributed"),
			 structural ?
			 errdetail("The statement would change the distribution column or a bucket partition of distributed table \"%s\".",
					   get_rel_name(dt->relid)) :
			 errdetail("%s", detail ? detail :
					   psprintf("The statement would change %s table \"%s\" on this node only, and phase 2a propagates only a fixed set of statements to every node.",
								dt->is_reference ? "reference" : "distributed",
								get_rel_name(dt->relid))),
			 errhint("A superuser can run the statement on every node by hand with kingres_shard.enable_routing = off.")));
}

/*
 * A foreign key from table src (NULL: a table that is not distributed) to the
 * table that con names.  Allowed: distributed -> reference, reference ->
 * reference, and distributed -> colocated distributed on the distribution
 * column.  Everything the nodes could not enforce locally is refused.
 */
static void
check_foreign_key(Node *parsetree, const KsCatalog *cat, const KsDistTable *src,
				  Constraint *con)
{
	Oid			pkrel = rangevar_relid(con->pktable);
	const KsDistTable *pk;

	if (!OidIsValid(pkrel))
		return;					/* the statement reports a missing table */
	pk = direct_dist(cat, pkrel);
	if (pk == NULL)
	{
		if (find_dist(cat, pkrel) != NULL)
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("%s is not supported on distributed tables yet",
							utility_name(parsetree)),
					 errdetail("A foreign key cannot reference a bucket partition.")));
		if (src != NULL)
			reject(parsetree, src, false,
				   "A foreign key of a distributed or reference table can reference only a reference table or a colocated distributed table.");
		return;
	}
	if (pk->is_reference)
		return;
	/* The referenced table is distributed. */
	if (src == NULL || src->is_reference)
		reject(parsetree, pk, false,
			   "A foreign key to a distributed table is possible only from a colocated distributed table.");
	if (src->colocation_id != pk->colocation_id)
		reject(parsetree, src, false,
			   "A foreign key between distributed tables needs both in the same colocation group.");
	{
		ListCell   *l1;
		ListCell   *l2;
		bool		ok = false;

		if (con->pk_attrs == NIL)
			reject(parsetree, src, false,
				   "Name the referenced columns of a foreign key between distributed tables.");
		forboth(l1, con->fk_attrs, l2, con->pk_attrs)
		{
			if (strcmp(strVal(lfirst(l1)), NameStr(src->dist_column)) == 0 &&
				strcmp(strVal(lfirst(l2)), NameStr(pk->dist_column)) == 0)
				ok = true;
		}
		if (!ok)
			reject(parsetree, src, false,
				   "A foreign key between distributed tables must contain the distribution column of both tables.");
	}
}

/*
 * What the text of a constant can mean on one node and not on another.
 *
 * TS_CLOCK: the input function of the type turns some words into the current
 * time or date ('now', 'today', 'tomorrow', 'yesterday', 'allballs'), which
 * PostgreSQL evaluates when ALTER TABLE ... ADD COLUMN runs, on each node at
 * its own moment, so the existing rows would get different values on different
 * nodes (review findings S2-10 and S3-08).
 * TS_OBJECT: the type is an object identifier written as a name (regclass,
 * regtype, ...): the same name is another number on every node.
 */
#define TS_CLOCK	0x01
#define TS_OBJECT	0x02

/*
 * type_text_meaning
 *
 * The flags above for a type, looking through domains and into arrays, ranges,
 * multiranges and composite types, whose input functions call the input
 * functions of their parts.
 */
static int
type_text_meaning(Oid typid, int depth)
{
	Oid			base;
	int			flags = 0;

	if (!OidIsValid(typid) || depth > 4)
		return 0;
	base = getBaseType(typid);
	switch (base)
	{
		case DATEOID:
		case TIMEOID:
		case TIMETZOID:
		case TIMESTAMPOID:
		case TIMESTAMPTZOID:
			return TS_CLOCK;
		case REGPROCOID:
		case REGPROCEDUREOID:
		case REGOPEROID:
		case REGOPERATOROID:
		case REGCLASSOID:
		case REGTYPEOID:
		case REGCOLLATIONOID:
		case REGCONFIGOID:
		case REGDICTIONARYOID:
		case REGNAMESPACEOID:
		case REGROLEOID:
			return TS_OBJECT;
		default:
			break;
	}

	if (OidIsValid(get_element_type(base)))
		return type_text_meaning(get_element_type(base), depth + 1);
	if (type_is_range(base))
		return type_text_meaning(get_range_subtype(base), depth + 1);
	if (type_is_multirange(base))
		return type_text_meaning(get_multirange_range(base), depth + 1);
	if (get_typtype(base) == TYPTYPE_COMPOSITE)
	{
		TupleDesc	tupdesc = lookup_rowtype_tupdesc_noerror(base, -1, true);
		int			i;

		if (tupdesc != NULL)
		{
			for (i = 0; i < tupdesc->natts; i++)
			{
				Form_pg_attribute att = TupleDescAttr(tupdesc, i);

				if (!att->attisdropped)
					flags |= type_text_meaning(att->atttypid, depth + 1);
			}
			ReleaseTupleDesc(tupdesc);
		}
	}
	return flags;
}

static int
typename_text_meaning(TypeName *tn)
{
	Type		tup;
	Oid			typid;

	if (tn == NULL)
		return 0;
	tup = LookupTypeName(NULL, tn, NULL, true);
	if (tup == NULL)
		return 0;				/* the statement itself reports a missing type */
	typid = ((Form_pg_type) GETSTRUCT(tup))->oid;
	ReleaseSysCache(tup);
	return type_text_meaning(typid, 0);
}

/*
 * A string made only of what a date or time written numerically can contain
 * (digits, the signs and separators of ISO 8601 and the common numeric
 * layouts, the letters T and Z) cannot be one of the words that name the
 * current time, because every such word has another letter.  That, and the
 * three words that are the same constant everywhere (epoch, infinity,
 * -infinity, alone), are the only texts of a date or time that are accepted:
 * the test is on what the text can be, so no spelling of a special word (white
 * space around it, other case, a word inside a longer string) gets through.
 */
static bool
is_fixed_datetime_text(const char *s)
{
	static const char *const constants[] = {"epoch", "infinity", "-infinity", NULL};
	const char *p;
	const char *end;
	bool		digit = false;
	int			i;

	/* The whole string without the white space around it, compared as a word. */
	for (p = s; *p != '\0' && isspace((unsigned char) *p); p++)
		;
	for (end = s + strlen(s); end > p && isspace((unsigned char) end[-1]); end--)
		;
	for (i = 0; constants[i] != NULL; i++)
	{
		if ((size_t) (end - p) == strlen(constants[i]) &&
			pg_strncasecmp(p, constants[i], end - p) == 0)
			return true;
	}

	for (p = s; *p != '\0'; p++)
	{
		unsigned char c = (unsigned char) *p;

		if (c >= '0' && c <= '9')
			digit = true;
		else if (strchr("-+:./ ,TtZz", c) == NULL)
			return false;
	}
	return digit;
}

/*
 * Is the default of ADD COLUMN a string whose meaning depends on the node that
 * reads it?  expr is what the statement wrote (a constant, or a constant with a
 * cast), coltype the type of the column.  The literal is read by the input
 * function of the cast type, then converted to the column type, which can read
 * the text again (text to date goes through the input function), so both types
 * count.
 */
static bool
default_text_is_node_dependent(Node *expr, TypeName *coltype)
{
	A_Const    *c;
	int			flags;

	if (expr == NULL)
		return false;
	if (IsA(expr, TypeCast))
	{
		flags = typename_text_meaning(((TypeCast *) expr)->typeName);
		expr = ((TypeCast *) expr)->arg;
	}
	else
		flags = 0;
	if (expr == NULL || !IsA(expr, A_Const))
		return false;
	c = (A_Const *) expr;
	if (c->isnull || nodeTag(&c->val) != T_String)
		return false;
	flags |= typename_text_meaning(coltype);
	if (flags & TS_OBJECT)
		return true;
	return (flags & TS_CLOCK) != 0 && !is_fixed_datetime_text(c->val.sval.sval);
}

static bool
is_serial_type(TypeName *tn)
{
	const char *name;

	if (tn == NULL || tn->names == NIL)
		return false;
	name = strVal(llast(tn->names));
	return strcmp(name, "serial") == 0 || strcmp(name, "bigserial") == 0 ||
		strcmp(name, "smallserial") == 0 || strcmp(name, "serial2") == 0 ||
		strcmp(name, "serial4") == 0 || strcmp(name, "serial8") == 0;
}

/*
 * The supported forms of ALTER TABLE.  Sets *structural for a statement that
 * damages the distribution; calls reject() for a form that is not supported.
 */
static void
check_alter_table(AlterTableStmt *stmt, const KsCatalog *cat,
				  const KsDistTable *dt, bool multi, bool *structural)
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
				*structural = true;
				return;

			case AT_DropColumn:
			case AT_AlterColumnType:
				if (!dt->is_reference && cmd->name != NULL &&
					strcmp(cmd->name, NameStr(dt->dist_column)) == 0)
				{
					*structural = true;
					return;
				}
				break;

			default:
				break;
		}

		if (!multi)
			continue;

		switch (cmd->subtype)
		{
			case AT_AddColumn:
				{
					ColumnDef  *col = (ColumnDef *) cmd->def;
					ListCell   *lc2;

					if (col->identity != 0)
						reject((Node *) stmt, dt, false, "ADD COLUMN with an identity is not supported yet.");
					if (is_serial_type(col->typeName))
						reject((Node *) stmt, dt, false, "ADD COLUMN of a serial type would create a different sequence on every node.");
					foreach(lc2, col->constraints)
					{
						Constraint *c = lfirst_node(Constraint, lc2);

						switch (c->contype)
						{
							case CONSTR_DEFAULT:
								/* A default is evaluated for every row on every node: it must be a constant. */
								if (default_text_is_node_dependent(c->raw_expr, col->typeName))
									reject((Node *) stmt, dt, false, "ADD COLUMN with a default written as a string for a column of a date, time or object-name type is refused: the text ('now', 'today', a table name, ...) can mean another value on each node. Write a date or time numerically (ISO 8601: '2024-01-02 03:04:05+00') or add the column without a default and UPDATE it.");
								if (!(c->raw_expr != NULL &&
									  (IsA(c->raw_expr, A_Const) ||
									   (IsA(c->raw_expr, TypeCast) &&
										IsA(((TypeCast *) c->raw_expr)->arg, A_Const)))))
									reject((Node *) stmt, dt, false, "ADD COLUMN with a default that is not a constant would give different values on different nodes.");
								break;
							case CONSTR_IDENTITY:
								reject((Node *) stmt, dt, false, "ADD COLUMN with an identity is not supported yet.");
								break;
							case CONSTR_FOREIGN:
								check_foreign_key((Node *) stmt, cat, dt, c);
								break;
							case CONSTR_EXCLUSION:
								reject((Node *) stmt, dt, false, "Exclusion constraints are not supported.");
								break;
							default:
								break;
						}
					}
				}
				break;

			case AT_ColumnDefault:
			case AT_DropNotNull:
			case AT_SetNotNull:
			case AT_ChangeOwner:
			case AT_ValidateConstraint:
			case AT_DropConstraint:
				break;

			case AT_DropColumn:
				if (cmd->behavior == DROP_CASCADE)
					reject((Node *) stmt, dt, false, "DROP COLUMN ... CASCADE could drop objects that exist on one node only.");
				break;

			case AT_AddConstraint:
				{
					Constraint *c = (Constraint *) cmd->def;

					switch (c->contype)
					{
						case CONSTR_CHECK:
							break;
						case CONSTR_PRIMARY:
						case CONSTR_UNIQUE:
							if (c->indexname != NULL)
								reject((Node *) stmt, dt, false, "A constraint built on an existing index (USING INDEX) is not supported yet.");
							break;
						case CONSTR_FOREIGN:
							check_foreign_key((Node *) stmt, cat, dt, c);
							break;
						default:
							reject((Node *) stmt, dt, false, "This kind of constraint is not supported yet.");
					}
				}
				break;

			case AT_AlterColumnType:
				reject((Node *) stmt, dt, false, "ALTER COLUMN ... TYPE would rewrite the table on every node.");
				break;

			default:
				reject((Node *) stmt, dt, false, "This form of ALTER TABLE is not supported yet.");
		}
	}
}

/*
 * A CREATE TABLE or ALTER TABLE of a table that is not distributed must not add
 * a foreign key to a distributed table: the nodes could not enforce it.
 */
static void
check_foreign_keys_to_dist(Node *parsetree, const KsCatalog *cat, List *elements,
						   bool are_cmds)
{
	ListCell   *lc;

	foreach(lc, elements)
	{
		Node	   *e = (Node *) lfirst(lc);
		List	   *cons = NIL;
		ListCell   *lc2;

		if (are_cmds)
		{
			AlterTableCmd *cmd = (AlterTableCmd *) e;

			if (cmd->subtype == AT_AddConstraint && cmd->def != NULL &&
				IsA(cmd->def, Constraint))
				cons = list_make1(cmd->def);
			else if (cmd->subtype == AT_AddColumn && cmd->def != NULL &&
					 IsA(cmd->def, ColumnDef))
				cons = ((ColumnDef *) cmd->def)->constraints;
		}
		else if (IsA(e, Constraint))
			cons = list_make1(e);
		else if (IsA(e, ColumnDef))
			cons = ((ColumnDef *) e)->constraints;

		foreach(lc2, cons)
		{
			Constraint *c = (Constraint *) lfirst(lc2);
			const KsDistTable *pk;

			if (!IsA(c, Constraint) || c->contype != CONSTR_FOREIGN)
				continue;
			pk = direct_dist(cat, rangevar_relid(c->pktable));
			if (pk != NULL && !pk->is_reference)
				reject(parsetree, pk, false,
					   "A foreign key to a distributed table is possible only from a colocated distributed table.");
		}
	}
}

/* Refuse a statement on a whole schema if the schema holds a distributed table. */
static void
guard_schema(Node *parsetree, const KsCatalog *cat, const char *nspname, const char *detail)
{
	Oid			nspid = get_namespace_oid(nspname, true);
	HASH_SEQ_STATUS scan;
	KsDistTable *t;

	if (!OidIsValid(nspid))
		return;
	hash_seq_init(&scan, cat->dist_tables);
	while ((t = (KsDistTable *) hash_seq_search(&scan)) != NULL)
	{
		if (get_rel_namespace(t->relid) == nspid)
		{
			hash_seq_term(&scan);
			reject(parsetree, t, false, detail);
		}
	}
}

/*
 * copy_table_as_query
 *
 * COPY [(columns)] <table> TO ... as COPY (SELECT <columns> FROM <table>) TO ...
 * with the same options.  The columns are the ones COPY would write (all of
 * them without a list, except dropped and generated ones, in table order; the
 * same checks of the list), named explicitly so that the SELECT returns exactly
 * what the table form would.
 */
static Node *
copy_table_as_query(CopyStmt *stmt)
{
	CopyStmt   *copy = copyObject(stmt);
	SelectStmt *sel = makeNode(SelectStmt);
	Relation	rel = table_openrv(stmt->relation, AccessShareLock);
	TupleDesc	tupdesc = RelationGetDescr(rel);
	List	   *attnums = CopyGetAttnums(tupdesc, rel, stmt->attlist);
	ListCell   *lc;

	foreach(lc, attnums)
	{
		ColumnRef  *cref = makeNode(ColumnRef);
		ResTarget  *rt = makeNode(ResTarget);

		cref->fields = list_make1(makeString(pstrdup(NameStr(TupleDescAttr(tupdesc, lfirst_int(lc) - 1)->attname))));
		cref->location = -1;
		rt->val = (Node *) cref;
		rt->location = -1;
		sel->targetList = lappend(sel->targetList, rt);
	}
	table_close(rel, NoLock);

	sel->fromClause = list_make1(copyObject(stmt->relation));
	sel->limitOption = LIMIT_OPTION_COUNT;
	copy->query = (Node *) sel;
	copy->relation = NULL;
	copy->attlist = NIL;
	return (Node *) copy;
}

/*
 * decide
 *
 * The decision for one utility statement; see the file header.  Raises an
 * error for a statement that is not allowed; fills *d for one that is.
 */
static void
decide(Node *parsetree, const KsCatalog *cat, UtilDecision *d)
{
	bool		multi = cat->ngroups > 1;
	ListCell   *lc;

	switch (nodeTag(parsetree))
	{
		case T_TruncateStmt:
			{
				int			ndist = 0;
				int			nother = 0;
				const KsDistTable *first = NULL;

				foreach(lc, ((TruncateStmt *) parsetree)->relations)
				{
					Oid			relid = rangevar_relid((RangeVar *) lfirst(lc));
					const KsDistTable *dt = direct_dist(cat, relid);

					if (dt == NULL && is_bucket_child(cat, relid))
						reject(parsetree, find_dist(cat, relid), true, NULL);
					if (dt != NULL)
					{
						ndist++;
						note_colocation(d, dt);
						if (first == NULL)
							first = dt;
					}
					else
						nother++;
				}
				if (ndist > 0 && multi)
				{
					if (nother > 0)
						reject(parsetree, first, false,
							   "Name only distributed or reference tables in a TRUNCATE that includes one.");
					d->propagate = true;
					d->authorize = true;
				}
			}
			break;

		case T_AlterTableStmt:
			{
				AlterTableStmt *stmt = (AlterTableStmt *) parsetree;
				Oid			relid = rangevar_relid(stmt->relation);
				const KsDistTable *dt = direct_dist(cat, relid);
				bool		structural = false;

				if (!OidIsValid(relid))
					break;
				if (stmt->objtype != OBJECT_TABLE)
				{
					/* ALTER INDEX and friends on an object of a distributed table */
					const KsDistTable *any = find_dist(cat, relid);

					if (any != NULL && multi)
						reject(parsetree, any, false, NULL);
					break;
				}
				if (dt == NULL)
				{
					const KsDistTable *any = find_dist(cat, relid);

					if (any != NULL)	/* a bucket partition */
						reject(parsetree, any, true, NULL);
					if (multi)
						check_foreign_keys_to_dist(parsetree, cat, stmt->cmds, true);
					break;
				}
				check_alter_table(stmt, cat, dt, multi, &structural);
				if (structural)
					reject(parsetree, dt, true, NULL);
				if (!dt->is_reference)
				{
					ListCell   *lcc;

					foreach(lcc, stmt->cmds)
					{
						AlterTableCmd *oc = lfirst_node(AlterTableCmd, lcc);

						if (oc->subtype == AT_ChangeOwner)
						{
							d->owner_relid = dt->relid;
							d->owner_role = get_rolespec_oid(oc->newowner, false);
						}
					}
				}
				if (multi)
				{
					note_colocation(d, dt);
					d->propagate = true;
					d->authorize = true;
				}
			}
			break;

		case T_RenameStmt:
			{
				RenameStmt *stmt = (RenameStmt *) parsetree;
				Oid			relid = InvalidOid;
				const KsDistTable *dt;

				switch (stmt->renameType)
				{
					case OBJECT_SCHEMA:
						if (multi)
							guard_schema(parsetree, cat, stmt->subname, NULL);
						return;
					case OBJECT_TABLE:
					case OBJECT_INDEX:
					case OBJECT_COLUMN:
					case OBJECT_TABCONSTRAINT:
					case OBJECT_TRIGGER:
					case OBJECT_POLICY:
					case OBJECT_RULE:
						relid = rangevar_relid(stmt->relation);
						break;
					default:
						return;
				}
				if (!OidIsValid(relid))
					break;
				dt = direct_dist(cat, relid);
				if (dt == NULL)
				{
					const KsDistTable *any = find_dist(cat, relid);

					if (any != NULL && (multi || stmt->renameType == OBJECT_TABLE))
						reject(parsetree, any, stmt->renameType == OBJECT_TABLE, NULL);
					break;
				}
				if (stmt->renameType == OBJECT_TABLE)
				{
					if (!dt->is_reference && strlen(stmt->newname) + 6 > NAMEDATALEN - 1)
						ereport(ERROR,
								(errcode(ERRCODE_INVALID_NAME),
								 errmsg("table name \"%s\" is too long for a distributed table", stmt->newname),
								 errdetail("Bucket partition names append 6 characters (_bNNNN) to the table name.")));
					if (!dt->is_reference)
						d->rename_relid = relid;
				}
				else if (stmt->renameType == OBJECT_COLUMN)
				{
					if (!dt->is_reference && stmt->subname != NULL &&
						strcmp(stmt->subname, NameStr(dt->dist_column)) == 0)
						reject(parsetree, dt, true, NULL);
				}
				else if (multi)
					reject(parsetree, dt, false, NULL);
				if (multi)
				{
					note_colocation(d, dt);
					d->propagate = true;
					d->authorize = true;
				}
			}
			break;

		case T_AlterObjectSchemaStmt:
			{
				AlterObjectSchemaStmt *stmt = (AlterObjectSchemaStmt *) parsetree;

				if ((stmt->objectType == OBJECT_TABLE || stmt->objectType == OBJECT_INDEX) &&
					stmt->relation != NULL)
				{
					Oid			relid = rangevar_relid(stmt->relation);
					const KsDistTable *any = find_dist(cat, relid);

					if (any != NULL && (multi || is_bucket_child(cat, relid)))
						reject(parsetree, any, is_bucket_child(cat, relid), NULL);
				}
			}
			break;

		case T_IndexStmt:
			{
				IndexStmt  *stmt = (IndexStmt *) parsetree;
				Oid			relid = rangevar_relid(stmt->relation);
				const KsDistTable *dt = direct_dist(cat, relid);

				if (dt == NULL)
				{
					const KsDistTable *any = find_dist(cat, relid);

					if (any != NULL)
						reject(parsetree, any, true, NULL);
					break;
				}
				if (multi)
				{
					if (stmt->concurrent)
						reject(parsetree, dt, false, "CREATE INDEX CONCURRENTLY cannot run in the distributed transaction.");
					note_colocation(d, dt);
					d->propagate = true;
					d->authorize = true;
				}
			}
			break;

		case T_DropStmt:
			{
				DropStmt   *stmt = (DropStmt *) parsetree;
				int			ndist = 0;
				int			nother = 0;
				const KsDistTable *first = NULL;

				foreach(lc, stmt->objects)
				{
					Node	   *obj = (Node *) lfirst(lc);
					List	   *names = IsA(obj, List) ? (List *) obj : NIL;
					Oid			relid;
					const KsDistTable *dt;

					switch (stmt->removeType)
					{
						case OBJECT_SCHEMA:
							if (IsA(obj, String) && multi)
								guard_schema(parsetree, cat, strVal(obj), NULL);
							break;
						case OBJECT_TABLE:
						case OBJECT_INDEX:
							if (names == NIL)
								break;
							relid = rangevar_relid(makeRangeVarFromNameList(names));
							if (!OidIsValid(relid))
								break;
							dt = find_dist(cat, relid);
							if (dt == NULL)
							{
								nother++;
								break;
							}
							if (stmt->removeType == OBJECT_TABLE && is_bucket_child(cat, relid))
								reject(parsetree, dt, true, NULL);
							if (stmt->removeType == OBJECT_INDEX &&
								get_rel_relispartition(IndexGetRelation(relid, true)))
								reject(parsetree, dt, true, NULL);
							ndist++;
							note_colocation(d, dt);
							if (first == NULL)
								first = dt;
							break;
						case OBJECT_TRIGGER:
						case OBJECT_POLICY:
						case OBJECT_RULE:
							if (list_length(names) >= 2 && multi)
							{
								Oid			trel = rangevar_relid(makeRangeVarFromNameList(list_copy_head(names, list_length(names) - 1)));
								const KsDistTable *any = find_dist(cat, trel);

								if (any != NULL)
									reject(parsetree, any, false, NULL);
							}
							break;
						default:
							break;
					}
				}
				if (ndist > 0 && multi)
				{
					if (nother > 0)
						reject(parsetree, first, false,
							   "Name only distributed or reference tables (or only their indexes) in a DROP that includes one.");
					if (stmt->concurrent)
						reject(parsetree, first, false, "DROP INDEX CONCURRENTLY cannot run in the distributed transaction.");
					d->propagate = true;
					d->authorize = true;
				}
				else if (ndist > 0 && stmt->removeType == OBJECT_TABLE)
					d->authorize = true;
			}
			break;

		case T_GrantStmt:
			{
				GrantStmt  *stmt = (GrantStmt *) parsetree;
				int			ndist = 0;
				int			nother = 0;
				const KsDistTable *first = NULL;

				if (stmt->objtype != OBJECT_TABLE || !multi)
					break;
				if (stmt->targtype == ACL_TARGET_ALL_IN_SCHEMA)
				{
					foreach(lc, stmt->objects)
						guard_schema(parsetree, cat, strVal(lfirst(lc)),
									 "GRANT ... ON ALL TABLES IN SCHEMA would act on different tables on different nodes; name the tables.");
					break;
				}
				foreach(lc, stmt->objects)
				{
					Oid			relid;
					const KsDistTable *dt;

					if (!IsA(lfirst(lc), RangeVar))
						continue;
					relid = rangevar_relid((RangeVar *) lfirst(lc));
					dt = direct_dist(cat, relid);
					if (dt == NULL && is_bucket_child(cat, relid))
						reject(parsetree, find_dist(cat, relid), true, NULL);
					if (dt != NULL)
					{
						ndist++;
						note_colocation(d, dt);
						if (first == NULL)
							first = dt;
					}
					else
						nother++;
				}
				if (ndist > 0)
				{
					if (nother > 0)
						reject(parsetree, first, false,
							   "Name only distributed or reference tables in a GRANT or REVOKE that includes one.");
					d->propagate = true;
					d->authorize = true;
				}
			}
			break;

		case T_CommentStmt:
			{
				CommentStmt *stmt = (CommentStmt *) parsetree;
				List	   *names = IsA(stmt->object, List) ? (List *) stmt->object : NIL;
				Oid			relid = InvalidOid;

				if (!multi || names == NIL)
					break;
				if (stmt->objtype == OBJECT_TABLE)
					relid = rangevar_relid(makeRangeVarFromNameList(names));
				else if (stmt->objtype == OBJECT_COLUMN && list_length(names) >= 2)
					relid = rangevar_relid(makeRangeVarFromNameList(list_copy_head(names, list_length(names) - 1)));
				if (direct_dist(cat, relid) != NULL)
				{
					note_colocation(d, direct_dist(cat, relid));
					d->propagate = true;
					d->authorize = true;
				}
			}
			break;

		case T_CopyStmt:
			{
				CopyStmt   *stmt = (CopyStmt *) parsetree;
				const KsDistTable *any = find_dist(cat, rangevar_relid(stmt->relation));

				if (any != NULL && multi && stmt->query == NULL && stmt->is_from)
				{
					const KsDistTable *own = direct_dist(cat, rangevar_relid(stmt->relation));

					/*
					 * A hash-distributed table (not a reference table, not a bucket
					 * partition named directly) is loaded through the row router
					 * (row_router.c, design 11.6); everything else, and every case with
					 * the switch off, is refused as before.
					 */
					if (own != NULL && !own->is_reference && kingres_shard_enable_copy_routing &&
						!kingres_shard_session_forwarded())
						d->copy_route = true;
					else
						reject(parsetree, any, false,
							   "COPY FROM would write the rows to the partitions of this node only.");
				}

				/*
				 * COPY <distributed table> TO does not go through the planner
				 * hook.  This core refuses it for a partitioned table ("Try the
				 * COPY (SELECT ...) TO variant"), but a core that accepts it
				 * would read the partitions of this node only and silently leave
				 * out the other shard groups.  So it is run as COPY (SELECT
				 * <columns> FROM <table>) TO, which the planner hook answers from
				 * every group (the distributed scan), on every cluster size.  A
				 * reference table is whole on every node, and a bucket partition
				 * named directly is read as what it is, the rows of that bucket
				 * on this node.
				 */
				if (stmt->query == NULL && !stmt->is_from &&
					stmt->relation != NULL)
				{
					const KsDistTable *dt = direct_dist(cat, rangevar_relid(stmt->relation));

					if (dt != NULL && !dt->is_reference)
						d->replacement = copy_table_as_query(stmt);
				}
			}
			break;

		case KS_T_CLUSTER_STMT:
			{
				const KsDistTable *any = find_dist(cat, rangevar_relid(ks_cluster_stmt_relation(parsetree)));

				if (any != NULL && multi)
					reject(parsetree, any, false, NULL);
			}
			break;

		case T_CreateTrigStmt:
			{
				const KsDistTable *any = find_dist(cat, rangevar_relid(((CreateTrigStmt *) parsetree)->relation));

				if (any != NULL && multi)
					reject(parsetree, any, false, NULL);
			}
			break;

		case T_RuleStmt:
			{
				const KsDistTable *any = find_dist(cat, rangevar_relid(((RuleStmt *) parsetree)->relation));

				if (any != NULL && multi)
					reject(parsetree, any, false, NULL);
			}
			break;

		case T_CreatePolicyStmt:
		case T_AlterPolicyStmt:
			{
				RangeVar   *rv = IsA(parsetree, CreatePolicyStmt) ?
					((CreatePolicyStmt *) parsetree)->table :
					((AlterPolicyStmt *) parsetree)->table;
				const KsDistTable *any = find_dist(cat, rangevar_relid(rv));

				if (any != NULL && multi)
					reject(parsetree, any, false, NULL);
			}
			break;

		case T_CreateStatsStmt:
			foreach(lc, ((CreateStatsStmt *) parsetree)->relations)
			{
				if (IsA(lfirst(lc), RangeVar))
				{
					const KsDistTable *any = find_dist(cat, rangevar_relid((RangeVar *) lfirst(lc)));

					if (any != NULL && multi)
						reject(parsetree, any, false, NULL);
				}
			}
			break;

		case T_CreateStmt:
			{
				CreateStmt *stmt = (CreateStmt *) parsetree;

				/* A new partition of a distributed table is never ours to add. */
				foreach(lc, stmt->inhRelations)
				{
					if (IsA(lfirst(lc), RangeVar))
					{
						const KsDistTable *any = find_dist(cat, rangevar_relid((RangeVar *) lfirst(lc)));

						if (any != NULL && (stmt->partbound != NULL || multi))
							reject(parsetree, any, stmt->partbound != NULL, NULL);
					}
				}
				if (multi)
					check_foreign_keys_to_dist(parsetree, cat, stmt->tableElts, false);
			}
			break;

		default:
			break;
	}
}

/*
 * The text of the statement, for the other nodes.  The query string may hold
 * several statements; the text of one of them cannot be told apart reliably in
 * every case, so such a query string is refused.
 */
static char *
statement_text(PlannedStmt *pstmt, const char *queryString)
{
	int			loc = pstmt->stmt_location;
	int			len = pstmt->stmt_len;
	int			total = strlen(queryString);
	const char *p;

	if (loc < 0)
		loc = 0;
	if (len <= 0 || loc + len > total)
		len = total - loc;
	if (loc != 0)
		ereport(ERROR,
				(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
				 errmsg("a DDL statement on distributed tables cannot be one of several statements in one query string"),
				 errhint("Send each statement on its own.")));
	for (p = queryString + loc + len; *p; p++)
	{
		if (!(*p == ';' || *p == ' ' || *p == '\t' || *p == '\n' || *p == '\r'))
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("a DDL statement on distributed tables cannot be one of several statements in one query string"),
					 errhint("Send each statement on its own.")));
	}
	return pnstrdup(queryString + loc, len);
}

/* A node to propagate to; copied out of the catalog cache. */
typedef struct DdlTarget
{
	int			node_id;
	int			group_id;
	char	   *host;
	int			port;
} DdlTarget;

static int
compare_targets(const ListCell *a, const ListCell *b)
{
	return ((DdlTarget *) lfirst(a))->node_id - ((DdlTarget *) lfirst(b))->node_id;
}

/*
 * After a distributed table was renamed, rename its bucket partitions: their
 * names are <table>_b<NNNN>, derived from the name of the table.
 */
static void
rename_partitions(Oid relid)
{
	char	   *nsp = get_namespace_name(get_rel_namespace(relid));
	char	   *newname = get_rel_name(relid);
	List	   *children = find_inheritance_children(relid, NoLock);
	ListCell   *lc;
	List	   *stmts = NIL;

	foreach(lc, children)
	{
		Oid			child = lfirst_oid(lc);
		char	   *old = get_rel_name(child);
		size_t		len = strlen(old);

		if (len < 6)
			continue;
		stmts = lappend(stmts,
						psprintf("ALTER TABLE %s.%s RENAME TO %s",
								 quote_identifier(nsp), quote_identifier(old),
								 quote_identifier(psprintf("%s%s", newname, old + len - 6))));
	}
	if (stmts == NIL)
		return;
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	foreach(lc, stmts)
	{
		if (SPI_execute((char *) lfirst(lc), false, 0) < 0)
			elog(ERROR, "could not rename a bucket partition");
	}
	SPI_finish();
}

/*
 * After the owner of a distributed table changed, change the owner of its
 * bucket partitions (and their indexes) on this node: PostgreSQL does not
 * recurse OWNER TO to partitions, and the owner of the table could otherwise
 * not run any DDL that touches the partitions (review finding S2-13).  The
 * statement is propagated to every node and each node runs this.
 */
static void
change_partition_owners(Oid relid, Oid newowner)
{
	List	   *children = find_inheritance_children(relid, AccessExclusiveLock);
	ListCell   *lc;

	CommandCounterIncrement();
	foreach(lc, children)
		ATExecChangeOwner(lfirst_oid(lc), newowner, false, AccessExclusiveLock);
}

/*
 * Does the hook have to look at this kind of statement?  The catalog cache
 * takes a snapshot, so it must not be read for statements that never concern
 * it: BEGIN ISOLATION LEVEL ... would fail after a snapshot was taken.
 */
static bool
statement_is_of_interest(Node *parsetree)
{
	switch (nodeTag(parsetree))
	{
		case T_TruncateStmt:
		case T_AlterTableStmt:
		case T_RenameStmt:
		case T_AlterObjectSchemaStmt:
		case T_IndexStmt:
		case T_DropStmt:
		case T_GrantStmt:
		case T_CommentStmt:
		case T_CopyStmt:
		case KS_T_CLUSTER_STMT:
		case T_CreateTrigStmt:
		case T_RuleStmt:
		case T_CreatePolicyStmt:
		case T_AlterPolicyStmt:
		case T_CreateStatsStmt:
		case T_CreateStmt:
			return true;
		default:
			return false;
	}
}

/*
 * refuse_trigger_on_reference_table
 *
 * A trigger on a reference table runs in the write of every node, with whatever
 * its function reads on that node: a count of a distributed table, a clock, a
 * sequence.  Nothing can prove that every copy gets the same row from it (review
 * finding S5-01), so there is none: not with routing off either, which is how
 * the one the review made got there.  The extension creates no trigger on a
 * reference table itself (its foreign keys are internal triggers that do not go
 * through this statement), so there is no exception.
 */
static void
refuse_trigger_on_reference_table(CreateTrigStmt *stmt)
{
	const KsCatalog *cat = kingres_shard_catalog();
	const KsDistTable *dt;

	if (!cat->present || cat->ndist_tables == 0)
		return;
	dt = direct_dist(cat, rangevar_relid(stmt->relation));
	if (dt == NULL || !dt->is_reference)
		return;
	ereport(ERROR,
			(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
			 errmsg("a trigger on reference table \"%s\" is not supported",
					get_rel_name(dt->relid)),
			 errdetail("A write to a reference table runs on every node, and what a trigger reads or computes there (another table, a clock, a sequence) can differ between the copies."),
			 errhint("Put the logic in the statement that writes the table, or in a trigger on a distributed table.")));
}

/*
 * refuse_ddl_on_claimed_table
 *
 * A table that an unfinished online table operation reads (the source of a distribution that
 * runs "concurrently") is copied and followed by a capture trigger; DDL on it would make the copy
 * and the log mean different things.  The statements that change the table or its name are refused
 * with the id of the operation (design/shard-phase9.md, "Capture").  The sessions of the
 * operation itself (forwarded sessions) are not refused.
 */
static void
claimed_relid_check(Oid relid)
{
	Oid			argtypes[1] = {OIDOID};
	Datum		values[1];
	int			ret;
	int64		op_id = 0;
	Oid			save_userid;
	int			save_sec;

	if (!OidIsValid(kingres_shard_catalog_relid("table_operation_item")))
		return;
	if (!OidIsValid(relid))
		return;
	/* an index is the table's: ALTER INDEX, DROP INDEX */
	{
		char		kind = get_rel_relkind(relid);

		if (kind == RELKIND_INDEX || kind == RELKIND_PARTITIONED_INDEX)
		{
			relid = IndexGetRelation(relid, true);
			if (!OidIsValid(relid))
				return;
		}
	}
	GetUserIdAndSecContext(&save_userid, &save_sec);
	SetUserIdAndSecContext(BOOTSTRAP_SUPERUSERID, save_sec | SECURITY_LOCAL_USERID_CHANGE);
	if (SPI_connect() != SPI_OK_CONNECT)
	{
		SetUserIdAndSecContext(save_userid, save_sec);
		return;
	}
	values[0] = ObjectIdGetDatum(relid);
	ret = SPI_execute_with_args("SELECT i.op_id FROM " KINGRES_SHARD_SCHEMA ".table_operation_item i "
								"JOIN " KINGRES_SHARD_SCHEMA ".table_operation o ON o.op_id OPERATOR(pg_catalog.=) i.op_id "
								"WHERE i.relid OPERATOR(pg_catalog.=) $1::pg_catalog.oid::pg_catalog.regclass "
								"AND o.phase IN ('preparing', 'shadow', 'copying', 'catching_up', 'switching') LIMIT 1",
								1, argtypes, values, NULL, true, 1);
	if (ret == SPI_OK_SELECT && SPI_processed > 0)
	{
		bool		isnull;

		op_id = DatumGetInt64(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull));
	}
	SPI_finish();
	SetUserIdAndSecContext(save_userid, save_sec);
	if (op_id != 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("table \"%s\" is being rebuilt by table operation " INT64_FORMAT "; it cannot be changed until the operation finishes",
						get_rel_name(relid), op_id),
				 errhint("Wait for it, or cancel it with kingres.cancel_table_operation(" INT64_FORMAT ").", op_id)));
}

static void
claimed_table_check(RangeVar *rv)
{
	if (rv == NULL)
		return;
	claimed_relid_check(RangeVarGetRelid(rv, NoLock, true));
}

/* the table of an object that DROP names as schema.table.name (a trigger, a rule, a policy) */
static void
claimed_table_of_name(List *names)
{
	if (names == NIL || list_length(names) < 2)
		return;
	claimed_table_check(makeRangeVarFromNameList(list_copy_head(names, list_length(names) - 1)));
}

static void
refuse_ddl_on_claimed_table(Node *parsetree)
{
	switch (nodeTag(parsetree))
	{
		case T_AlterTableStmt:
			claimed_table_check(((AlterTableStmt *) parsetree)->relation);
			break;
		case T_RenameStmt:
			claimed_table_check(((RenameStmt *) parsetree)->relation);
			break;
		case T_IndexStmt:
			claimed_table_check(((IndexStmt *) parsetree)->relation);
			break;
		case T_CreateTrigStmt:
			claimed_table_check(((CreateTrigStmt *) parsetree)->relation);
			break;
		case T_AlterObjectSchemaStmt:
			claimed_table_check(((AlterObjectSchemaStmt *) parsetree)->relation);
			break;
		case T_RuleStmt:
			claimed_table_check(((RuleStmt *) parsetree)->relation);
			break;
		case T_CreatePolicyStmt:
			claimed_table_check(((CreatePolicyStmt *) parsetree)->table);
			break;
		case T_AlterPolicyStmt:
			claimed_table_check(((AlterPolicyStmt *) parsetree)->table);
			break;
		case T_TruncateStmt:
			{
				ListCell   *lc;

				foreach(lc, ((TruncateStmt *) parsetree)->relations)
					claimed_table_check((RangeVar *) lfirst(lc));
				break;
			}
		case T_DropStmt:
			{
				DropStmt   *ds = (DropStmt *) parsetree;
				ListCell   *lc;

				foreach(lc, ds->objects)
				{
					switch (ds->removeType)
					{
						case OBJECT_TABLE:
						case OBJECT_INDEX:
							if (IsA(lfirst(lc), List))
								claimed_table_check(makeRangeVarFromNameList((List *) lfirst(lc)));
							break;
						case OBJECT_TRIGGER:
						case OBJECT_RULE:
						case OBJECT_POLICY:
							if (IsA(lfirst(lc), List))
								claimed_table_of_name((List *) lfirst(lc));
							break;
						default:
							break;
					}
				}
				break;
			}
		default:
			break;
	}
}

/*
 * refuse_inheritance_of_reference_table
 *
 * A write to a reference table also writes the rows of every table that inherits
 * from it, and fires their triggers; a partition is the same.  Those children
 * would be tables that this extension does not know, whose triggers and rows each
 * node would handle for itself, so the copies could differ with no error (review
 * finding S6-03: a trigger on a child, made by hand with routing off).  A
 * reference table is therefore never a parent or a child of anything: creation of
 * a reference table refuses a table that takes part in inheritance
 * (internal_check_referenceable), and from then on these statements are refused,
 * with routing off too:
 *	CREATE TABLE ... INHERITS (r), CREATE TABLE ... PARTITION OF r,
 *	ALTER TABLE r INHERIT p, ALTER TABLE c INHERIT r,
 *	ALTER TABLE r ATTACH PARTITION p ..., ALTER TABLE p ATTACH PARTITION r ...
 */
static void
refuse_inheritance_of_reference_table(Node *parsetree)
{
	const KsCatalog *cat = kingres_shard_catalog();
	List	   *involved = NIL;
	ListCell   *lc;

	if (!cat->present || cat->ndist_tables == 0)
		return;

	if (IsA(parsetree, CreateStmt))
	{
		foreach(lc, ((CreateStmt *) parsetree)->inhRelations)
			if (IsA(lfirst(lc), RangeVar))
				involved = lappend(involved, lfirst(lc));
	}
	else if (IsA(parsetree, AlterTableStmt))
	{
		AlterTableStmt *stmt = (AlterTableStmt *) parsetree;

		foreach(lc, stmt->cmds)
		{
			AlterTableCmd *cmd = lfirst_node(AlterTableCmd, lc);

			if (cmd->subtype == AT_AddInherit && cmd->def != NULL && IsA(cmd->def, RangeVar))
			{
				involved = lappend(involved, stmt->relation);
				involved = lappend(involved, cmd->def);
			}
			else if (cmd->subtype == AT_AttachPartition && cmd->def != NULL && IsA(cmd->def, PartitionCmd))
			{
				involved = lappend(involved, stmt->relation);
				involved = lappend(involved, ((PartitionCmd *) cmd->def)->name);
			}
		}
	}
	foreach(lc, involved)
	{
		RangeVar   *rv = (RangeVar *) lfirst(lc);
		const KsDistTable *dt = direct_dist(cat, rangevar_relid(rv));

		if (dt != NULL && dt->is_reference)
			ereport(ERROR,
					(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					 errmsg("%s is not supported on reference table \"%s\": a reference table has no inheritance children or parents and no partitions",
							utility_name(parsetree), get_rel_name(dt->relid)),
					 errdetail("A write to a reference table would also write the rows of the tables that inherit from it and fire their triggers on every node, and the copies could differ."),
					 errhint("Keep the data in separate tables, or make the table distributed.")));
	}
}

/*
 * Wait for a table that is being distributed before the hook decides about a statement on it.
 *
 * create_distributed_table() and its relatives hold an EXCLUSIVE lock on the table and replace it by a
 * partitioned one.  A statement that was started before the swap decides at its start, when the table is
 * a plain table and nothing is propagated, and then waits for its lock in the standard code: after the
 * swap it would run on the new table as if it were plain.  The lock taken here (ROW EXCLUSIVE, which every
 * writer holds anyway, so that nothing else is serialized) conflicts with that EXCLUSIVE lock, so the
 * statement waits first and decides afterwards, with the catalog as the commit of the call left it.  The
 * name is looked up again after the wait.  Only a role that may do something with the table waits: the
 * lock is no way to stall a table that the role cannot touch.
 *
 * The lock order of the extension is the lock of the catalog first, the table after.  A statement that goes
 * on to propagate takes the lock of the catalog later and so lets go of the locks taken here before it does
 * (the caller); a statement on a plain table never takes it, and keeps the lock, which is what stops a
 * call from starting on the table while the statement runs.
 */
static List *wait_locked = NIL;

static void
wait_for_table_operation(RangeVar *rv, AclMode needed)
{
	Oid			relid;
	MemoryContext old;

	if (rv == NULL || rv->relpersistence == RELPERSISTENCE_TEMP)
		return;
	relid = RangeVarGetRelid(rv, NoLock, true);
	if (!OidIsValid(relid))
		return;
	/*
	 * Only a role that has the right the statement needs waits: the lock is the one the statement would take
	 * anyway, but a statement that fails its permission check (a GRANT of a role that may only read) must not
	 * leave a lock that the role could not take with LOCK TABLE.  needed is the privilege of the statement, 0
	 * for a statement of the owner.
	 */
	if (!object_ownercheck(RelationRelationId, relid, GetUserId()) &&
		(needed == 0 || pg_class_aclmask(relid, GetUserId(), needed, ACLMASK_ANY) == 0))
		return;
	relid = RangeVarGetRelid(rv, RowExclusiveLock, true);
	if (!OidIsValid(relid))
		return;
	old = MemoryContextSwitchTo(TopTransactionContext);
	wait_locked = lappend_oid(wait_locked, relid);
	MemoryContextSwitchTo(old);
}

static void
release_waited_tables(void)
{
	ListCell   *lc;

	foreach(lc, wait_locked)
		UnlockRelationOid(lfirst_oid(lc), RowExclusiveLock);
	wait_locked = NIL;
}

static void
wait_for_statement_tables(Node *parsetree)
{
	ListCell   *lc;

	wait_locked = NIL;

	switch (nodeTag(parsetree))
	{
		case T_AlterTableStmt:
			wait_for_table_operation(((AlterTableStmt *) parsetree)->relation, 0);
			break;
		case T_RenameStmt:
			wait_for_table_operation(((RenameStmt *) parsetree)->relation, 0);
			break;
		case T_AlterObjectSchemaStmt:
			wait_for_table_operation(((AlterObjectSchemaStmt *) parsetree)->relation, 0);
			break;
		case T_IndexStmt:
			wait_for_table_operation(((IndexStmt *) parsetree)->relation, 0);
			break;
		case T_CreateTrigStmt:
			wait_for_table_operation(((CreateTrigStmt *) parsetree)->relation, ACL_TRIGGER);
			break;
		case T_RuleStmt:
			wait_for_table_operation(((RuleStmt *) parsetree)->relation, 0);
			break;
		case T_CreatePolicyStmt:
			wait_for_table_operation(((CreatePolicyStmt *) parsetree)->table, 0);
			break;
		case T_AlterPolicyStmt:
			wait_for_table_operation(((AlterPolicyStmt *) parsetree)->table, 0);
			break;
		case T_CopyStmt:
			if (((CopyStmt *) parsetree)->is_from)
				wait_for_table_operation(((CopyStmt *) parsetree)->relation, ACL_INSERT);
			break;
		case T_TruncateStmt:
			foreach(lc, ((TruncateStmt *) parsetree)->relations)
				wait_for_table_operation((RangeVar *) lfirst(lc), ACL_TRUNCATE);
			break;
		case T_GrantStmt:
			{
				GrantStmt  *g = (GrantStmt *) parsetree;

				if (g->targtype == ACL_TARGET_OBJECT && g->objtype == OBJECT_TABLE)
					foreach(lc, g->objects)
						if (IsA(lfirst(lc), RangeVar))
							/* the owner, or a role that holds a privilege with the grant option */
							wait_for_table_operation((RangeVar *) lfirst(lc), ACL_GRANT_OPTION_FOR(ACL_ALL_RIGHTS_RELATION));
			}
			break;
		case T_DropStmt:
			{
				DropStmt   *ds = (DropStmt *) parsetree;

				if (ds->removeType == OBJECT_TABLE)
					foreach(lc, ds->objects)
						if (IsA(lfirst(lc), List))
							wait_for_table_operation(makeRangeVarFromNameList((List *) lfirst(lc)), 0);
			}
			break;
		default:
			break;
	}
}

/*
 * kingres_shard_process_utility
 *
 * The hook: decide, run, propagate.  Statements that run while the extension
 * itself is created, and all statements while routing is off, are not looked
 * at; nor are statements generated by another statement, nor those that run
 * while an authorized statement runs (the renames of partitions).
 */
/*
 * The statements that name a user partition of a distributed table are not run: they are turned into the calls of the partition
 * functions (design/shard-phase9.md, feature 4), which build their own statements.  The text of a bound is written back from the
 * parse tree for constants (and casts of constants) only; anything else is refused with the hint to call the function.
 */
static void
append_bound_expr(StringInfo buf, Node *e)
{
	if (e == NULL)
		elog(ERROR, "empty partition bound");
	if (IsA(e, A_Const))
	{
		A_Const    *c = (A_Const *) e;

		if (c->isnull)
			appendStringInfoString(buf, "NULL");
		else
			switch (nodeTag(&c->val))
			{
				case T_Integer:
					appendStringInfo(buf, "%d", intVal(&c->val));
					break;
				case T_Float:
					appendStringInfoString(buf, castNode(Float, &c->val)->fval);
					break;
				case T_Boolean:
					appendStringInfoString(buf, boolVal(&c->val) ? "true" : "false");
					break;
				case T_String:
					appendStringInfoString(buf, quote_literal_cstr(strVal(&c->val)));
					break;
				default:
					ereport(ERROR, (errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
									errmsg("this partition bound is not supported in the statement"),
									errhint("Call kingres.create_partition() or kingres.attach_partition() with the bound as text.")));
			}
		return;
	}
	if (IsA(e, TypeCast))
	{
		TypeCast   *tc = (TypeCast *) e;

		Oid			typoid;
		int32		typmod;

		append_bound_expr(buf, tc->arg);
		/* the type with its schema and its modifier: the text is resolved by a function with another search_path (P9C-09, P9F-01) */
		typenameTypeIdAndMod(NULL, tc->typeName, &typoid, &typmod);
		{
			/* a type of another schema than pg_catalog is written with its schema; the built-in types as PostgreSQL writes them (date, numeric(3,0)) */
			int			flags = FORMAT_TYPE_TYPEMOD_GIVEN;
			HeapTuple	tup = SearchSysCache1(TYPEOID, ObjectIdGetDatum(typoid));

			if (!HeapTupleIsValid(tup))
				elog(ERROR, "cache lookup failed for type %u", typoid);
			if (((Form_pg_type) GETSTRUCT(tup))->typnamespace != PG_CATALOG_NAMESPACE)
				flags |= FORMAT_TYPE_FORCE_QUALIFY;
			ReleaseSysCache(tup);
			appendStringInfo(buf, "::%s", format_type_extended(typoid, typmod, flags));
		}
		return;
	}
	if (IsA(e, A_Expr) && ((A_Expr *) e)->kind == AEXPR_OP && list_length(((A_Expr *) e)->name) == 1 &&
		strcmp(strVal(linitial(((A_Expr *) e)->name)), "-") == 0 && ((A_Expr *) e)->lexpr == NULL)
	{
		appendStringInfoChar(buf, '-');
		append_bound_expr(buf, ((A_Expr *) e)->rexpr);
		return;
	}
	ereport(ERROR, (errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
					errmsg("this partition bound is not supported in the statement"),
					errhint("Call kingres.create_partition() or kingres.attach_partition() with the bound as text.")));
}

static char *
partition_bound_text(PartitionBoundSpec *spec)
{
	StringInfoData buf;
	ListCell   *lc;
	bool		first;

	if (spec->is_default)
		return pstrdup("DEFAULT");
	initStringInfo(&buf);
	if (spec->strategy == PARTITION_STRATEGY_RANGE)
	{
		int			side;

		for (side = 0; side < 2; side++)
		{
			List	   *l = side == 0 ? spec->lowerdatums : spec->upperdatums;

			appendStringInfoString(&buf, side == 0 ? "FROM (" : " TO (");
			first = true;
			foreach(lc, l)
			{
				Node	   *d = (Node *) lfirst(lc);

				if (!first)
					appendStringInfoString(&buf, ", ");
				first = false;
				/* the raw tree holds expressions; MINVALUE and MAXVALUE are column references of those names */
				if (IsA(d, ColumnRef) && list_length(((ColumnRef *) d)->fields) == 1 && IsA(linitial(((ColumnRef *) d)->fields), String) &&
					pg_strcasecmp(strVal(linitial(((ColumnRef *) d)->fields)), "minvalue") == 0)
					appendStringInfoString(&buf, "MINVALUE");
				else if (IsA(d, ColumnRef) && list_length(((ColumnRef *) d)->fields) == 1 && IsA(linitial(((ColumnRef *) d)->fields), String) &&
						 pg_strcasecmp(strVal(linitial(((ColumnRef *) d)->fields)), "maxvalue") == 0)
					appendStringInfoString(&buf, "MAXVALUE");
				else if (IsA(d, PartitionRangeDatum))
				{
					PartitionRangeDatum *rd = (PartitionRangeDatum *) d;

					if (rd->kind == PARTITION_RANGE_DATUM_MINVALUE)
						appendStringInfoString(&buf, "MINVALUE");
					else if (rd->kind == PARTITION_RANGE_DATUM_MAXVALUE)
						appendStringInfoString(&buf, "MAXVALUE");
					else
						append_bound_expr(&buf, rd->value);
				}
				else
					append_bound_expr(&buf, d);
			}
			appendStringInfoChar(&buf, ')');
		}
	}
	else if (spec->strategy == PARTITION_STRATEGY_LIST)
	{
		appendStringInfoString(&buf, "IN (");
		first = true;
		foreach(lc, spec->listdatums)
		{
			if (!first)
				appendStringInfoString(&buf, ", ");
			first = false;
			append_bound_expr(&buf, lfirst(lc));
		}
		appendStringInfoChar(&buf, ')');
	}
	else
		ereport(ERROR, (errcode(ERRCODE_FEATURE_NOT_SUPPORTED), errmsg("a hash partition cannot be added to a user-partitioned distributed table")));
	return buf.data;
}

/* Is the table a user-partitioned distributed table? */
static bool
is_user_partitioned(Oid relid)
{
	Oid			argtypes[1] = {OIDOID};
	Datum		values[1];
	bool		result;

	values[0] = ObjectIdGetDatum(relid);
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	if (SPI_execute_with_args("SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".dist_table WHERE relid = $1::pg_catalog.oid::pg_catalog.regclass AND partition_method IS NOT NULL",
							  1, argtypes, values, NULL, true, 1) < 0)
		elog(ERROR, "could not read the catalog");
	result = SPI_processed > 0;
	SPI_finish();
	return result;
}

/* Is there a user partition of that name of the table? */
static bool
user_partition_exists(Oid parent, const char *name)
{
	Oid			argtypes[2] = {OIDOID, TEXTOID};
	Datum		values[2];
	bool		result;

	values[0] = ObjectIdGetDatum(parent);
	values[1] = CStringGetTextDatum(name);
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	if (SPI_execute_with_args("SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".dist_partition WHERE parent = $1::pg_catalog.oid::pg_catalog.regclass AND partition_name = $2",
							  2, argtypes, values, NULL, true, 1) < 0)
		elog(ERROR, "could not read the catalog");
	result = SPI_processed > 0;
	SPI_finish();
	return result;
}

static void
call_partition_function(const char *sql, int nargs, Oid *argtypes, Datum *values)
{
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	if (SPI_execute_with_args(sql, nargs, argtypes, values, NULL, false, 0) < 0)
		elog(ERROR, "partition function failed");
	SPI_finish();
}

static bool
map_partition_statement(Node *parsetree, QueryCompletion *qc)
{
	const KsCatalog *cat = kingres_shard_catalog();
	Oid			argtypes[4];
	Datum		values[4];

	if (IsA(parsetree, CreateStmt))
	{
		CreateStmt *stmt = (CreateStmt *) parsetree;
		Oid			parent;

		if (stmt->partbound == NULL || list_length(stmt->inhRelations) != 1 || !IsA(linitial(stmt->inhRelations), RangeVar))
			return false;
		parent = rangevar_relid((RangeVar *) linitial(stmt->inhRelations));
		if (direct_dist(cat, parent) == NULL || !is_user_partitioned(parent))
			return false;
		if (stmt->partspec != NULL || stmt->options != NIL || stmt->tablespacename != NULL || stmt->accessMethod != NULL || stmt->tableElts != NIL)
			ereport(ERROR, (errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
							errmsg("a partition of a distributed table is created with its bound only"),
							errhint("Sub-partitioning, storage parameters, a tablespace, an access method and columns are not accepted here.")));
		/*
		 * What the statement says and the mapping does not carry is refused, not ignored (P9C-09): the leaves are made in the
		 * schema of the table, as permanent tables; and IF NOT EXISTS is answered as PostgreSQL does, with a notice.
		 */
		if (stmt->relation->relpersistence != RELPERSISTENCE_PERMANENT)
			ereport(ERROR, (errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
							errmsg("a partition of a distributed table is a permanent table"),
							errhint("UNLOGGED and TEMPORARY are not accepted here.")));
		{
			Oid			nsp = stmt->relation->schemaname != NULL ? get_namespace_oid(stmt->relation->schemaname, false)
				: RangeVarGetCreationNamespace(stmt->relation);

			if (nsp != get_rel_namespace(parent))
				ereport(ERROR, (errcode(ERRCODE_INVALID_SCHEMA_NAME),
								errmsg("the partition must be in the schema of the table"),
								errhint("Write the schema of the table in front of the name of the partition.")));
			if (stmt->if_not_exists &&
				(OidIsValid(get_relname_relid(stmt->relation->relname, nsp)) || user_partition_exists(parent, stmt->relation->relname)))
			{
				ereport(NOTICE, (errcode(ERRCODE_DUPLICATE_TABLE),
								 errmsg("relation \"%s\" already exists, skipping", stmt->relation->relname)));
				if (qc != NULL)
					SetQueryCompletion(qc, CMDTAG_CREATE_TABLE, 0);
				return true;
			}
		}
		argtypes[0] = OIDOID;
		argtypes[1] = TEXTOID;
		argtypes[2] = TEXTOID;
		values[0] = ObjectIdGetDatum(parent);
		values[1] = CStringGetTextDatum(stmt->relation->relname);
		values[2] = CStringGetTextDatum(partition_bound_text(stmt->partbound));
		call_partition_function("SELECT " KINGRES_SHARD_SCHEMA ".create_partition($1::pg_catalog.oid::pg_catalog.regclass, $2, $3, false)",
								3, argtypes, values);
		if (qc != NULL)
			SetQueryCompletion(qc, CMDTAG_CREATE_TABLE, 0);
		return true;
	}
	if (IsA(parsetree, AlterTableStmt))
	{
		AlterTableStmt *stmt = (AlterTableStmt *) parsetree;
		AlterTableCmd *cmd;
		PartitionCmd *pc;
		Oid			parent;

		if (stmt->objtype != OBJECT_TABLE || list_length(stmt->cmds) != 1)
			return false;
		cmd = linitial_node(AlterTableCmd, stmt->cmds);
		if (cmd->subtype != AT_AttachPartition && cmd->subtype != AT_DetachPartition)
			return false;
		parent = rangevar_relid(stmt->relation);
		if (direct_dist(cat, parent) == NULL || !is_user_partitioned(parent))
			return false;
		pc = (PartitionCmd *) cmd->def;
		argtypes[0] = OIDOID;
		values[0] = ObjectIdGetDatum(parent);
		if (cmd->subtype == AT_AttachPartition)
		{
			Oid			part = rangevar_relid(pc->name);

			if (direct_dist(cat, part) == NULL)
				ereport(ERROR, (errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
								errmsg("ATTACH PARTITION of a table that is not a distributed table of the group is not supported"),
								errhint("Use kingres.create_partition() and insert the rows into the table.")));
			argtypes[1] = OIDOID;
			argtypes[2] = TEXTOID;
			values[1] = ObjectIdGetDatum(part);
			values[2] = CStringGetTextDatum(partition_bound_text(pc->bound));
			call_partition_function("SELECT " KINGRES_SHARD_SCHEMA ".attach_partition($1::pg_catalog.oid::pg_catalog.regclass, $2::pg_catalog.oid::pg_catalog.regclass, $3)",
									3, argtypes, values);
		}
		else
		{
			if (pc->concurrent)
				ereport(ERROR, (errcode(ERRCODE_FEATURE_NOT_SUPPORTED), errmsg("DETACH PARTITION CONCURRENTLY of a distributed table is not supported")));
			argtypes[1] = TEXTOID;
			values[1] = CStringGetTextDatum(pc->name->relname);
			call_partition_function("SELECT " KINGRES_SHARD_SCHEMA ".detach_partition($1::pg_catalog.oid::pg_catalog.regclass, $2)",
									2, argtypes, values);
		}
		if (qc != NULL)
			SetQueryCompletion(qc, CMDTAG_ALTER_TABLE, 0);
		return true;
	}
	if (IsA(parsetree, DropStmt))
	{
		DropStmt   *stmt = (DropStmt *) parsetree;
		RangeVar   *rv;
		Oid			nsp;

		if (stmt->removeType != OBJECT_TABLE || list_length(stmt->objects) != 1 || stmt->behavior == DROP_CASCADE)
			return false;
		rv = makeRangeVarFromNameList(castNode(List, linitial(stmt->objects)));
		if (OidIsValid(rangevar_relid(rv)))
			return false;
		nsp = rv->schemaname != NULL ? get_namespace_oid(rv->schemaname, true) : RangeVarGetCreationNamespace(rv);
		if (!OidIsValid(nsp))
			return false;
		argtypes[0] = TEXTOID;
		argtypes[1] = OIDOID;
		values[0] = CStringGetTextDatum(rv->relname);
		values[1] = ObjectIdGetDatum(nsp);
		if (SPI_connect() != SPI_OK_CONNECT)
			elog(ERROR, "SPI_connect failed");
		if (SPI_execute_with_args("SELECT p.parent::pg_catalog.oid FROM " KINGRES_SHARD_SCHEMA ".dist_partition p JOIN pg_catalog.pg_class c ON c.oid = p.parent::pg_catalog.oid "
								  "WHERE p.partition_name = $1 AND c.relnamespace = $2",
								  2, argtypes, values, NULL, true, 1) < 0)
			elog(ERROR, "could not read the catalog");
		if (SPI_processed == 0)
		{
			SPI_finish();
			return false;
		}
		{
			bool		isnull;
			Oid			parent = DatumGetObjectId(SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull));
			Oid			a2[3] = {OIDOID, TEXTOID, BOOLOID};
			Datum		v2[3];

			SPI_finish();
			v2[0] = ObjectIdGetDatum(parent);
			v2[1] = CStringGetTextDatum(rv->relname);
			v2[2] = BoolGetDatum(stmt->missing_ok);
			call_partition_function("SELECT " KINGRES_SHARD_SCHEMA ".drop_partition($1::pg_catalog.oid::pg_catalog.regclass, $2, $3)", 3, a2, v2);
		}
		if (qc != NULL)
			SetQueryCompletion(qc, CMDTAG_DROP_TABLE, 0);
		return true;
	}
	return false;
}

static void
kingres_shard_process_utility(PlannedStmt *pstmt, const char *queryString,
							   bool readOnlyTree,
							   ProcessUtilityContext context,
							   ParamListInfo params,
							   QueryEnvironment *queryEnv,
							   DestReceiver *dest, QueryCompletion *qc)
{
	Node	   *parsetree = pstmt->utilityStmt;
	UtilDecision d;
	const KsCatalog *cat;
	List	   *targets = NIL;
	char	   *text = NULL;
	KsSchemaDdl sd;
	bool		have_sd = false;
	bool		authorized;
	bool		saved_drop_authorized;
	volatile bool internal_active = false;
	bool		set_skip_local = false;

	/* A session that claims to be forwarded must prove it before anything runs. */
	(void) kingres_shard_session_forwarded();

	/* CREATE TABLE AS into a distributed schema: the empty table, then the rows (phase 11, design 6.7) */
	if (IsA(parsetree, CreateTableAsStmt) && kingres_shard_enable_routing && !creating_extension && ddl_internal_depth == 0 &&
		context != PROCESS_UTILITY_SUBCOMMAND && !kingres_shard_session_forwarded() &&
		schema_ctas((CreateTableAsStmt *) parsetree, qc))
		return;

	memset(&d, 0, sizeof(d));
	memset(&sd, 0, sizeof(sd));

	/* Whatever the routing setting is: see refuse_trigger_on_reference_table. */
	if (IsA(parsetree, CreateTrigStmt) && !creating_extension && !kingres_shard_session_forwarded())
		refuse_trigger_on_reference_table((CreateTrigStmt *) parsetree);

	/* COPY of a table of a distributed schema that another group holds (phase 11, design 6.8). */
	if (IsA(parsetree, CopyStmt) && kingres_shard_enable_routing && !creating_extension &&
		!kingres_shard_session_forwarded())
	{
		refuse_copy_of_schema_shell((CopyStmt *) parsetree);
		/* COPY FROM into a shell: the rows are sent to the owner */
		if (((CopyStmt *) parsetree)->is_from && ((CopyStmt *) parsetree)->relation != NULL && ((CopyStmt *) parsetree)->query == NULL)
		{
			const KsCatalog *ccat = kingres_shard_catalog();
			Oid			crel = ccat->present && ccat->nschema_tables > 0 ? rangevar_relid(((CopyStmt *) parsetree)->relation) : InvalidOid;
			const KsDistTable *cst = OidIsValid(crel) ? kingres_shard_lookup_schema_table(ccat, crel) : NULL;

			if (cst != NULL && kingres_shard_schema_table_owner(cst) != ccat->local_group_id)
			{
				uint64		processed = kingres_shard_copy_from_routed((CopyStmt *) parsetree, queryString);

				if (qc != NULL)
					SetQueryCompletion(qc, CMDTAG_COPY, processed);
				return;
			}
		}
	}

	/* The source of an online table operation is not changed by anybody else until it ends. */
	if (!creating_extension && !kingres_shard_session_forwarded())
		refuse_ddl_on_claimed_table(parsetree);

	/* And the same for inheritance and partitions of a reference table (S6-03). */
	if ((IsA(parsetree, CreateStmt) || IsA(parsetree, AlterTableStmt)) &&
		!creating_extension && !kingres_shard_session_forwarded())
		refuse_inheritance_of_reference_table(parsetree);

	/* A statement that names a user partition of a distributed table becomes a call of a partition function. */
	if (kingres_shard_enable_routing && !creating_extension && ddl_internal_depth == 0 && context != PROCESS_UTILITY_SUBCOMMAND &&
		(IsA(parsetree, CreateStmt) || IsA(parsetree, AlterTableStmt) || IsA(parsetree, DropStmt)) &&
		!kingres_shard_session_forwarded())
	{
		const KsCatalog *pcat = kingres_shard_catalog();

		if (pcat->present && pcat->ndist_tables > 0 && map_partition_statement(parsetree, qc))
			return;
	}

	if (kingres_shard_enable_routing && !creating_extension &&
		(statement_is_of_interest(parsetree) || kingres_shard_schema_ddl_interesting(parsetree)) &&
		ddl_internal_depth == 0 && context != PROCESS_UTILITY_SUBCOMMAND)
	{
		cat = kingres_shard_catalog();
		if (cat->present && !kingres_shard_session_forwarded())
		{
			wait_for_statement_tables(parsetree);
			cat = kingres_shard_catalog();
		}
		/* an object of a distributed schema, or a new schema that the database distributes (phase 11) */
		if (cat->present && cat->local_node_id != 0 && kingres_shard_schema_ddl_interesting(parsetree) &&
			(cat->ndist_schemas > 0 || IsA(parsetree, CreateSchemaStmt)))
		{
			kingres_shard_schema_ddl_classify(parsetree, cat, kingres_shard_session_forwarded(), &sd);
			have_sd = sd.touches || sd.create_schema;
			if (sd.refuse_what != NULL || sd.refuse_message != NULL)
				ereport(ERROR,
						(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
						 sd.refuse_message != NULL ? errmsg_internal("%s", sd.refuse_message) :
						 errmsg("%s is not supported in a distributed schema yet", sd.refuse_what),
						 sd.refuse_hint != NULL ? errhint("%s", sd.refuse_hint) : 0));
			if (sd.create_schema)
				kingres_shard_schema_check_limit(cat);
			cat = kingres_shard_catalog();
		}
		if (cat->present && (cat->ndist_tables > 0 || have_sd))
		{
			if (cat->ndist_tables > 0)
				decide(parsetree, cat, &d);
			if (have_sd)
				merge_schema_decision(&d, &sd);
			if (d.propagate || d.copy_route)
				release_waited_tables();
			else
				wait_locked = NIL;

			if (d.replacement != NULL)
			{
				/* The statement we were given may be shared (a cached plan): run a copy. */
				PlannedStmt *replaced = makeNode(PlannedStmt);

				memcpy(replaced, pstmt, sizeof(PlannedStmt));
				replaced->utilityStmt = d.replacement;
				pstmt = replaced;
				parsetree = d.replacement;
			}

			if (d.propagate && !kingres_shard_session_forwarded())
			{
				ListCell   *lc;

				if (!kingres_shard_is_recognized_map_primary())
				{
					const KsNode *self = kingres_shard_node_by_id(cat->local_node_id);
					const KsNode *mg = kingres_shard_group_primary(KINGRES_SHARD_MAP_GROUP);

					/* The catalog calls this node the map primary but PostgreSQL promoted it since: say so (S9-02). */
					if (self != NULL && self->group_id == KINGRES_SHARD_MAP_GROUP && self->is_primary)
						kingres_shard_require_map_group();
					ereport(ERROR,
							(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
							 errmsg("%s on a distributed or reference table must run on the map group primary",
									utility_name(parsetree)),
							 mg ? errhint("Run it on node %d (%s:%d).", mg->node_id, mg->host, mg->port) :
							 errhint("Run it on the map group primary.")));
				}
				text = statement_text(pstmt, queryString);
				kingres_shard_lock_catalog();
				foreach(lc, d.colocations)
					kingres_shard_assert_no_move(lfirst_int(lc));
				cat = kingres_shard_catalog();
				foreach(lc, cat->nodes)
				{
					KsNode	   *n = (KsNode *) lfirst(lc);
					DdlTarget  *t;

					if (n->node_id == cat->local_node_id || !n->is_active || !n->is_primary)
						continue;
					t = palloc(sizeof(DdlTarget));
					t->node_id = n->node_id;
					t->group_id = n->group_id;
					t->host = pstrdup(n->host);
					t->port = n->port;
					targets = lappend(targets, t);
				}
				list_sort(targets, compare_targets);
			}
		}
	}

	/*
	 * COPY ... FROM into a distributed table of several groups (11.6).  The handler does what
	 * DoCopy does for COPY FROM but sends the rows to the group that owns each bucket; the
	 * statement is not handed to the next hook, which would write this node's partitions only.
	 */
	if (d.copy_route)
	{
		uint64		processed = kingres_shard_copy_from_routed((CopyStmt *) parsetree, queryString);

		if (qc != NULL)
			SetQueryCompletion(qc, CMDTAG_COPY, processed);
		return;
	}

	/* this node holds a shell of a table that leaves its schema: the shell is dropped, the statement is not run here (the owner runs it) */
	if (have_sd && sd.set_mode == 2 && text != NULL && cat->local_group_id != sd.set_owner_group)
	{
		kingres_shard_schema_table_op_local("assert_empty", sd.set_old_name);
		kingres_shard_schema_table_op_local("drop_shell", sd.set_old_name);
		set_skip_local = true;
	}

	authorized = d.authorize || OidIsValid(d.rename_relid);
	saved_drop_authorized = ddl_drop_authorized;
	ddl_drop_authorized = authorized;
	if (authorized)
		ddl_authorized_depth++;
	PG_TRY();
	{
		if (set_skip_local)
		{
			if (qc != NULL)
				SetQueryCompletion(qc, CMDTAG_ALTER_TABLE, 0);
		}
		else if (prev_ProcessUtility)
			prev_ProcessUtility(pstmt, queryString, readOnlyTree, context, params,
								queryEnv, dest, qc);
		else
			standard_ProcessUtility(pstmt, queryString, readOnlyTree, context,
									params, queryEnv, dest, qc);
		if (OidIsValid(d.rename_relid))
		{
			internal_active = true;
			ddl_internal_depth++;
			rename_partitions(d.rename_relid);
		}
		if (OidIsValid(d.owner_relid))
			change_partition_owners(d.owner_relid, d.owner_role);
	}
	PG_FINALLY();
	{
		if (internal_active)
			ddl_internal_depth--;
		ddl_drop_authorized = saved_drop_authorized;
		if (authorized)
			ddl_authorized_depth--;
	}
	PG_END_TRY();

	if (text != NULL)
	{
		ListCell   *lc;
		int			new_coloc = 0;
		int			new_group = 0;
		List	   *set_in_statements = NIL;

		/* a new distributed schema: its colocation group and its group are chosen here, once, under the cluster lock */
		if (have_sd && sd.create_schema)
		{
			new_coloc = kingres_shard_schema_next_colocation();
			new_group = kingres_shard_schema_choose_group();
			kingres_shard_schema_register_local(sd.create_name, new_coloc, new_group);
			sd.schema_names = list_make1(sd.create_name);
		}
		else if (have_sd && sd.may_create_tables)
		{
			foreach(lc, sd.schema_names)
				(void) kingres_shard_schema_sync_tables_local((char *) lfirst(lc));
		}
		else if (have_sd && sd.set_mode != 0 && !set_skip_local)
			(void) kingres_shard_schema_reconcile_local();

		/* a table that came into a distributed schema: the statements that make it, empty, on the other nodes */
		if (have_sd && sd.set_mode == 1)
		{
			Oid			newid = RangeVarGetRelid(sd.set_new_rel, NoLock, false);

			set_in_statements = kingres_shard_schema_table_statements_local(newid);
		}

		foreach(lc, targets)
		{
			DdlTarget  *t = (DdlTarget *) lfirst(lc);
			KsConn	   *kc = kingres_shard_get_conn(t->host, t->port, false, NULL);

			kingres_shard_begin_write_xact(kc);
			if (have_sd && sd.set_mode == 1)
			{
				ListCell   *ls;

				foreach(ls, set_in_statements)
					kingres_shard_exec_ddl(kc, (char *) lfirst(ls));
			}
			else if (have_sd && sd.set_mode == 2 && t->group_id != sd.set_owner_group)
			{
				/* a shell that leaves the schema: it must be empty, then it goes (nothing is propagated) */
				kingres_shard_exec_ddl(kc, psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_schema_delta('assert_empty', '', NULL, NULL, %s)",
													quote_literal_cstr(sd.set_old_name)));
				kingres_shard_exec_ddl(kc, psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_schema_delta('drop_shell', '', NULL, NULL, %s)",
													quote_literal_cstr(sd.set_old_name)));
			}
			else
				kingres_shard_exec_ddl(kc, text);
			if (have_sd && sd.set_mode != 0 && !(sd.set_mode == 2 && t->group_id != sd.set_owner_group))
				kingres_shard_exec_ddl(kc, "SELECT " KINGRES_SHARD_SCHEMA ".internal_schema_delta('reconcile', '')");
			if (have_sd && sd.create_schema)
				kingres_shard_exec_ddl(kc, psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_schema_delta('register_schema', %s, %d, %d)",
													quote_literal_cstr(sd.create_name), new_coloc, new_group));
			else if (have_sd && sd.may_create_tables)
			{
				ListCell   *lc2;

				foreach(lc2, sd.schema_names)
					kingres_shard_exec_ddl(kc, psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_schema_delta('register_tables', %s)",
														quote_literal_cstr((char *) lfirst(lc2))));
			}
		}

		/*
		 * The structure that the statement made is the same on every node (design 7.1), or the transaction is
		 * rolled back.  The question goes down the connection that ran the statement, in the same branch: the
		 * table is not committed anywhere yet.
		 */
		if (have_sd && sd.verify_tables != NIL && targets != NIL)
		{
			List	   *relids = NIL;
			List	   *fps = NIL;
			ListCell   *lc2;

			foreach(lc, sd.verify_tables)
			{
				Oid			relid = RangeVarGetRelid((RangeVar *) lfirst(lc), NoLock, true);

				if (OidIsValid(relid))
				{
					relids = lappend_oid(relids, relid);
					fps = lappend(fps, kingres_shard_schema_fingerprint_local(relid));
				}
			}
			foreach(lc, targets)
			{
				DdlTarget  *t = (DdlTarget *) lfirst(lc);
				KsConn	   *kc = kingres_shard_get_conn(t->host, t->port, false, NULL);
				ListCell   *lf = list_head(fps);

				foreach(lc2, relids)
				{
					kingres_shard_schema_verify_remote(kc, t->node_id, lfirst_oid(lc2), (const char *) lfirst(lf));
					lf = lnext(fps, lf);
				}
			}
		}
	}
}

void
kingres_shard_internal_ddl_enter(void)
{
	ddl_internal_depth++;
	ddl_authorized_depth++;
}

void
kingres_shard_internal_ddl_leave(void)
{
	ddl_internal_depth--;
	ddl_authorized_depth--;
}

/*
 * kingres_shard_in_authorized_ddl
 *
 * True while a propagated (or arrived) DDL statement runs.  The statements that
 * PostgreSQL runs on its behalf, such as the validation of a new foreign key
 * (a join of the partitions of the referencing table with the referenced
 * table), concern the partitions of this node: the foreign keys that are
 * accepted are on the distribution column of colocated tables or reference a
 * reference table, so every node validates its own buckets.  The planner must
 * not apply the routing checks to them.
 */
bool
kingres_shard_in_authorized_ddl(const char *query_string)
{
	/* Only the validation join of a foreign key, which PostgreSQL generates (ri_triggers.c), not user code. */
	return ddl_authorized_depth > 0 && query_string != NULL &&
		strncmp(query_string, "SELECT fk.", strlen("SELECT fk.")) == 0;
}
