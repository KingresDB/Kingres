/*-------------------------------------------------------------------------
 *
 * schema_ddl.c
 *	  DDL on a distributed schema (phase 11, design/shard-phase11.md, sections
 *	  5.3, 7.1 to 7.4, 7.8, 7.12 and 12).
 *
 * A distributed schema exists, with its structure, on every node; the rows of its
 * tables are on the one shard group that holds it.  A utility statement whose
 * object is in such a schema is therefore run on every node, in one two-phase
 * transaction, by the ProcessUtility hook (utility.c), exactly as the DDL on a
 * hash-distributed table is.  This file answers the questions the hook asks:
 *
 *	- classify: which distributed schemas does the statement touch (by the
 *	  namespace of every object it names), is it a statement that is refused in
 *	  one (design 7.12), does it make a table that must be registered, does it
 *	  make a new schema that the database distributes by default?
 *	- the node-local steps that register a schema and its tables, which run with
 *	  the rights of the bootstrap superuser (the caller of the DDL is an ordinary
 *	  role), and the comparison of the structure of a table across the nodes.
 *
 * Statements that name no distributed schema are not touched, and the cost for
 * them is the check of the kind of the statement.  A kind of statement that this
 * file does not know is run on this node only, as before: the list in classify()
 * is the list of design 7.1.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/schema_ddl.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "access/relation.h"
#include "catalog/namespace.h"
#include "catalog/objectaddress.h"
#include "catalog/pg_authid.h"
#include "catalog/pg_class.h"
#include "catalog/pg_namespace.h"
#include "catalog/pg_type.h"
#include "commands/extension.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "nodes/makefuncs.h"
#include "nodes/parsenodes.h"
#include "parser/parse_func.h"
#include "parser/parse_type.h"
#include "utils/acl.h"
#include "utils/array.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/rel.h"
#include "utils/syscache.h"

#include "catalog.h"
#include "forward_auth.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "schema_ddl.h"

PG_FUNCTION_INFO_V1(kingres_internal_schema_delta);

/* kingres_shard.max_distributed_schemas, kingres_shard.schema_placement_group, kingres_shard.distribute_new_schemas */
int			kingres_shard_max_distributed_schemas = 10000;
extern int	kingres_shard_add_node_batch_statements;
int			kingres_shard_schema_placement_group = -1;
bool		kingres_shard_enable_schema_routing = true;
bool		kingres_shard_shipped_scope = false;
static int	distribute_new_schemas_setting = 0;	/* 0 default, 1 on, 2 off */

static const struct config_enum_entry distribute_new_schemas_options[] = {
	{"default", 0, false},
	{"on", 1, false},
	{"off", 2, false},
	{NULL, 0, false}
};

void
kingres_shard_schema_ddl_init(void)
{
	DefineCustomIntVariable("kingres_shard.add_node_batch_statements",
							"How many statements one transaction of add_node() runs on the new node when it makes the distributed schemas.",
							"The locks of a transaction grow with its statements; a batch ends before it would pass this number (a schema is never split).",
							&kingres_shard_add_node_batch_statements,
							100, 1, 100000,
							PGC_SUSET,
							0,
							NULL, NULL, NULL);

	DefineCustomBoolVariable("kingres_shard.catalog_incremental",
							 "Invalidate only the distributed tables that a catalog change touches.",
							 "Off: every change of the catalog invalidates every distributed table (the fallback).",
							 &kingres_shard_catalog_incremental,
							 true,
							 PGC_SUSET,
							 0,
							 NULL, NULL, NULL);

	DefineCustomBoolVariable("kingres_shard.enable_schema_routing",
							 "Run a statement on a distributed schema whole on the shard group that holds it.",
							 "When off, such a statement is planned with the distributed scan (single-source form) and a write to a schema of another group is refused; used to compare the two paths.",
							 &kingres_shard_enable_schema_routing,
							 true,
							 PGC_USERSET, 0,
							 NULL, NULL, NULL);

	DefineCustomBoolVariable("kingres_shard.shipped_scope",
							 "Set by the origin of a statement that is run whole on the owner of a distributed schema and that runs code the statement does not show.",
							 "While it is on, a statement planned in a forwarded session may read only tables of distributed schemas, reference tables and catalogs: another table would be this node's own copy, not the origin's (design 6.4).",
							 &kingres_shard_shipped_scope,
							 false,
							 PGC_USERSET, GUC_NO_SHOW_ALL | GUC_NOT_IN_SAMPLE,
							 NULL, NULL, NULL);

	DefineCustomIntVariable("kingres_shard.max_distributed_schemas",
							"Upper bound on the number of distributed schemas of a database.",
							"The number is a guess and not a measured limit; the creation of another distributed schema fails with a clear message.",
							&kingres_shard_max_distributed_schemas,
							10000, 1, 1000000,
							PGC_SIGHUP, 0,
							NULL, NULL, NULL);

	DefineCustomIntVariable("kingres_shard.schema_placement_group",
							"The shard group that receives the next distributed schema this session creates (-1: the group is chosen).",
							NULL,
							&kingres_shard_schema_placement_group,
							-1, -1, INT_MAX,
							PGC_SUSET, 0,
							NULL, NULL, NULL);

	DefineCustomEnumVariable("kingres_shard.distribute_new_schemas",
							 "Whether a new schema of this session becomes a distributed schema: default follows the setting of the database.",
							 "on or off overrides the database setting for the session of a superuser; an ordinary role cannot opt a schema out.",
							 &distribute_new_schemas_setting,
							 0,
							 distribute_new_schemas_options,
							 PGC_SUSET, 0,
							 NULL, NULL, NULL);
}

/* ---------------------------------------------------------------------------
 * Which schemas a statement names
 * ------------------------------------------------------------------------- */

typedef struct NsSet
{
	const KsCatalog *cat;
	List	   *dist;			/* of Oid: namespaces that are distributed schemas */
	List	   *other;			/* of Oid: the others (a temporary schema is none) */
} NsSet;

static void
ns_add(NsSet *s, Oid ns)
{
	if (!OidIsValid(ns) || isAnyTempNamespace(ns))
		return;
	if (kingres_shard_lookup_dist_schema(s->cat, ns) != NULL)
		s->dist = list_append_unique_oid(s->dist, ns);
	else
		s->other = list_append_unique_oid(s->other, ns);
}

/* the schema of an existing relation (InvalidOid when it does not exist) */
static Oid
rv_ns(RangeVar *rv)
{
	Oid			relid;

	if (rv == NULL)
		return InvalidOid;
	relid = RangeVarGetRelid(rv, NoLock, true);
	return OidIsValid(relid) ? get_rel_namespace(relid) : InvalidOid;
}

/* the schema a new relation is created in */
static Oid
rv_creation_ns(RangeVar *rv)
{
	if (rv == NULL || rv->relpersistence == RELPERSISTENCE_TEMP)
		return InvalidOid;
	return RangeVarGetCreationNamespace(rv);
}

static Oid
names_creation_ns(List *names)
{
	char	   *objname;

	if (names == NIL)
		return InvalidOid;
	return QualifiedNameGetCreationNamespace(names, &objname);
}

/* the schema of a type named by a list of names */
static Oid
typename_ns(List *names)
{
	TypeName   *tn;
	Type		tup;
	Oid			ns = InvalidOid;

	if (names == NIL)
		return InvalidOid;
	tn = makeTypeNameFromNameList(names);
	tup = LookupTypeName(NULL, tn, NULL, true);
	if (tup != NULL)
	{
		ns = ((Form_pg_type) GETSTRUCT(tup))->typnamespace;
		ReleaseSysCache(tup);
	}
	return ns;
}

static Oid
func_ns(ObjectType objtype, ObjectWithArgs *owa)
{
	Oid			fn;

	if (owa == NULL)
		return InvalidOid;
	fn = LookupFuncWithArgs(objtype, owa, true);
	return OidIsValid(fn) ? get_func_namespace(fn) : InvalidOid;
}

/*
 * The schema of any object that PostgreSQL can name (DROP, COMMENT, RENAME, OWNER TO ...): the
 * address machinery of the server, which also finds the table of a trigger, a rule or a column.
 */
static Oid
object_ns(ObjectType objtype, Node *object)
{
	Relation	rel = NULL;
	ObjectAddress addr;
	Oid			ns = InvalidOid;

	if (object == NULL)
		return InvalidOid;
	/* ALTER TYPE ... OWNER TO and ALTER DOMAIN ... OWNER TO carry the type as a list of names, not as a TypeName */
	if ((objtype == OBJECT_TYPE || objtype == OBJECT_DOMAIN) && IsA(object, List))
		return typename_ns((List *) object);
	addr = get_object_address(objtype, object, &rel, AccessShareLock, true);
	if (OidIsValid(addr.objectId))
	{
		if (rel != NULL)
			ns = RelationGetNamespace(rel);
		else if (addr.classId == NamespaceRelationId)
			ns = addr.objectId;
		else
			ns = get_object_namespace(&addr);
	}
	if (rel != NULL)
		relation_close(rel, AccessShareLock);
	return ns;
}

/* ---------------------------------------------------------------------------
 * Classification
 * ------------------------------------------------------------------------- */

static void
refuse(KsSchemaDdl *sd, const char *what, const char *hint)
{
	if (sd->refuse_what != NULL || sd->refuse_message != NULL)
		return;
	sd->refuse_what = pstrdup(what);
	if (hint != NULL)
		sd->refuse_hint = pstrdup(hint);
}

static void
refuse_message(KsSchemaDdl *sd, const char *msg, const char *hint)
{
	if (sd->refuse_what != NULL || sd->refuse_message != NULL)
		return;
	sd->refuse_message = pstrdup(msg);
	if (hint != NULL)
		sd->refuse_hint = pstrdup(hint);
}

/* A foreign key of a table of a distributed schema may point to a table of the same schema or to a reference table. */
static void
check_foreign_key(Constraint *con, Oid src_ns, const KsCatalog *cat, KsSchemaDdl *sd)
{
	Oid			pkrel;
	Oid			pkns;
	bool		src_dist;
	bool		pk_dist;

	if (con->contype != CONSTR_FOREIGN || con->pktable == NULL)
		return;
	pkrel = RangeVarGetRelid(con->pktable, NoLock, true);
	if (!OidIsValid(pkrel))
		return;					/* a key of the table to itself while it is created: the same schema */
	pkns = get_rel_namespace(pkrel);
	src_dist = kingres_shard_lookup_dist_schema(cat, src_ns) != NULL;
	pk_dist = kingres_shard_lookup_dist_schema(cat, pkns) != NULL;
	if (!src_dist && !pk_dist)
		return;
	if (src_dist && pkns == src_ns)
		return;
	if (src_dist && cat->dist_tables != NULL && cat->ndist_tables > 0)
	{
		const KsDistTable *dt = hash_search(cat->dist_tables, &pkrel, HASH_FIND, NULL);

		if (dt != NULL && dt->is_reference)
			return;
	}
	refuse_message(sd, "a foreign key between a table of a distributed schema and a table outside the schema is not supported",
				   "A table of a distributed schema may reference a table of the same schema or a reference table.");
}

static void
scan_elements_for_keys(List *elts, Oid src_ns, const KsCatalog *cat, KsSchemaDdl *sd)
{
	ListCell   *lc;

	foreach(lc, elts)
	{
		Node	   *e = (Node *) lfirst(lc);

		if (IsA(e, Constraint))
			check_foreign_key((Constraint *) e, src_ns, cat, sd);
		else if (IsA(e, ColumnDef))
		{
			ListCell   *lc2;

			foreach(lc2, ((ColumnDef *) e)->constraints)
				if (IsA(lfirst(lc2), Constraint))
					check_foreign_key((Constraint *) lfirst(lc2), src_ns, cat, sd);
		}
	}
}

/* what the statement touches, once the schemas are collected */
static void
finish(KsSchemaDdl *sd, NsSet *s, const KsCatalog *cat, const char *what)
{
	ListCell   *lc;

	if (s->dist == NIL)
		return;
	if (s->other != NIL)
		refuse_message(sd, psprintf("%s that names a distributed schema and another schema is not supported", what),
					   "Run one statement for each schema.");
	sd->touches = true;
	foreach(lc, s->dist)
	{
		Oid			ns = lfirst_oid(lc);
		const KsDistSchema *ds = kingres_shard_lookup_dist_schema(cat, ns);
		char	   *name = get_namespace_name(ns);

		if (name != NULL)
		{
			ListCell   *lc2;
			bool		have = false;

			foreach(lc2, sd->schema_names)
				if (strcmp((char *) lfirst(lc2), name) == 0)
					have = true;
			if (!have)
				sd->schema_names = lappend(sd->schema_names, name);
		}
		if (ds != NULL)
			sd->colocations = list_append_unique_int(sd->colocations, ds->colocation_id);
	}
}

/* is a name one that a database never distributes by itself? */
static bool
name_is_reserved(const char *name)
{
	return strncmp(name, "pg_", 3) == 0 || strcmp(name, "information_schema") == 0 ||
		strcmp(name, "kingres") == 0 || strcmp(name, "public") == 0;
}

static bool
effective_distribute_new_schemas(const KsCatalog *cat)
{
	if (cat->sharding_mode == KS_MODE_ROW)
		return false;
	if (distribute_new_schemas_setting == 1)
		return true;
	if (distribute_new_schemas_setting == 2)
		return false;
	return cat->distribute_new_schemas;
}

/* the kinds of statement that classify() looks at */
bool
kingres_shard_schema_ddl_interesting(Node *parsetree)
{
	switch (nodeTag(parsetree))
	{
		case T_CreateSchemaStmt:
		case T_CreateStmt:
		case T_CreateForeignTableStmt:
		case T_CreateTableAsStmt:
		case T_ViewStmt:
		case T_CreateSeqStmt:
		case T_AlterSeqStmt:
		case T_CreateFunctionStmt:
		case T_AlterFunctionStmt:
		case T_CompositeTypeStmt:
		case T_CreateEnumStmt:
		case T_CreateRangeStmt:
		case T_CreateDomainStmt:
		case T_AlterEnumStmt:
		case T_AlterDomainStmt:
		case T_AlterTypeStmt:
		case T_DefineStmt:
		case T_IndexStmt:
		case T_ReassignOwnedStmt:
		case T_CreateExtensionStmt:
		case T_CreateTrigStmt:
		case T_AlterTableStmt:
		case T_RenameStmt:
		case T_AlterObjectSchemaStmt:
		case T_AlterOwnerStmt:
		case T_CommentStmt:
		case T_SecLabelStmt:
		case T_GrantStmt:
		case T_AlterDefaultPrivilegesStmt:
		case T_TruncateStmt:
		case T_DropStmt:
		case T_RuleStmt:
		case T_CreatePolicyStmt:
		case T_AlterPolicyStmt:
		case T_CreateStatsStmt:
			return true;
		default:
			return false;
	}
}

/*
 * ALTER TABLE ... SET SCHEMA of an ordinary table, one of whose two schemas is distributed.  The origin asks kingres.internal_set_schema_check for the
 * kind of move and the reason to refuse it; a session of another node only learns the schemas (the origin has decided).
 */
static int run_as_bootstrap(const char *sql, int nargs, Oid *argtypes, Datum *values, bool want_int);

static void
set_schema_table(Node *parsetree, Oid relid, Oid src_ns, Oid target, const KsCatalog *cat, bool forwarded, KsSchemaDdl *sd, NsSet *s)
{
	AlterObjectSchemaStmt *a = (AlterObjectSchemaStmt *) parsetree;

	/* the border is crossed on purpose: only the distributed side is the statement's schema */
	if (kingres_shard_lookup_dist_schema(cat, src_ns) != NULL)
		ns_add(s, src_ns);
	if (kingres_shard_lookup_dist_schema(cat, target) != NULL)
		ns_add(s, target);
	if (!forwarded)
	{
		Oid			argtypes[2] = {OIDOID, OIDOID};
		Datum		values[2];
		Oid			save_uid;
		int			save_sec;
		int			mode = 0;
		int			owner = -1;
		char	   *err = NULL;
		char	   *hint = NULL;
		MemoryContext callercxt = CurrentMemoryContext;

		values[0] = ObjectIdGetDatum(relid);
		values[1] = ObjectIdGetDatum(target);
		kingres_shard_require_catalog();
		GetUserIdAndSecContext(&save_uid, &save_sec);
		SetUserIdAndSecContext(BOOTSTRAP_SUPERUSERID, save_sec | SECURITY_LOCAL_USERID_CHANGE);
		PG_TRY();
		{
			if (SPI_connect() != SPI_OK_CONNECT)
				elog(ERROR, "SPI_connect failed");
			if (SPI_execute_with_args("SELECT mode, owner_group, err, hint FROM " KINGRES_SHARD_SCHEMA ".internal_set_schema_check($1, $2)",
									  2, argtypes, values, NULL, false, 1) == SPI_OK_SELECT && SPI_processed == 1)
			{
				HeapTuple	tup = SPI_tuptable->vals[0];
				TupleDesc	td = SPI_tuptable->tupdesc;
				bool		isnull;
				MemoryContext cur = MemoryContextSwitchTo(callercxt);
				char	   *v;

				mode = DatumGetInt32(SPI_getbinval(tup, td, 1, &isnull));
				owner = DatumGetInt32(SPI_getbinval(tup, td, 2, &isnull));
				if (isnull)
					owner = -1;
				v = SPI_getvalue(tup, td, 3);
				err = v != NULL ? pstrdup(v) : NULL;
				v = SPI_getvalue(tup, td, 4);
				hint = v != NULL ? pstrdup(v) : NULL;
				MemoryContextSwitchTo(cur);
			}
			SPI_finish();
		}
		PG_FINALLY();
		{
			SetUserIdAndSecContext(save_uid, save_sec);
		}
		PG_END_TRY();
		if (err != NULL)
			refuse_message(sd, psprintf("ALTER TABLE ... SET SCHEMA: %s", err), hint);
		sd->set_mode = mode;
		sd->set_owner_group = owner;
		sd->set_old_name = quote_qualified_identifier(get_namespace_name(src_ns), get_rel_name(relid));
		sd->set_new_rel = makeRangeVar(pstrdup(a->newschema), pstrdup(get_rel_name(relid)), -1);
		if (mode == 1 || mode == 3)
			sd->verify_tables = lappend(sd->verify_tables, sd->set_new_rel);
	}
	finish(sd, s, cat, "ALTER ... SET SCHEMA");
}

void
kingres_shard_schema_ddl_classify(Node *parsetree, const KsCatalog *cat, bool forwarded, KsSchemaDdl *sd)
{

	NsSet		s;
	ListCell   *lc;

	memset(sd, 0, sizeof(*sd));
	memset(&s, 0, sizeof(s));
	s.cat = cat;

	switch (nodeTag(parsetree))
	{
		case T_CreateSchemaStmt:
			{
				CreateSchemaStmt *c = (CreateSchemaStmt *) parsetree;
				char	   *name = c->schemaname;

				if (forwarded || creating_extension)
					break;
				if (name == NULL && c->authrole != NULL)
					name = get_rolespec_name(c->authrole);
				if (name == NULL || name_is_reserved(name))
					break;
				if (!effective_distribute_new_schemas(cat))
					break;
				/* an existing schema: IF NOT EXISTS is a notice, the rest fails in the statement itself */
				if (OidIsValid(get_namespace_oid(name, true)))
					break;
				if (c->schemaElts != NIL)
				{
					refuse_message(sd, "CREATE SCHEMA with elements is not supported in a database that distributes new schemas",
								   "Create the schema, then its tables.");
					break;
				}
				sd->create_schema = true;
				sd->create_name = pstrdup(name);
			}
			break;

		case T_CreateStmt:
			{
				CreateStmt *c = (CreateStmt *) parsetree;
				Oid			ns = rv_creation_ns(c->relation);

				ns_add(&s, ns);
				if (s.dist != NIL)
				{
					foreach(lc, c->inhRelations)
					{
						Oid			pns = rv_ns((RangeVar *) lfirst(lc));

						if (OidIsValid(pns) && pns != ns)
							refuse_message(sd, "a table of a distributed schema cannot inherit from, or be a partition of, a table of another schema",
										   "Create the parent in the same schema.");
					}
					sd->may_create_tables = true;
					sd->verify_tables = lappend(sd->verify_tables, c->relation);
				}
				if (OidIsValid(ns))
					scan_elements_for_keys(c->tableElts, ns, cat, sd);
				finish(sd, &s, cat, "CREATE TABLE");
			}
			break;

		case T_CreateForeignTableStmt:
			{
				CreateStmt *c = &((CreateForeignTableStmt *) parsetree)->base;

				ns_add(&s, rv_creation_ns(c->relation));
				if (s.dist != NIL)
					refuse(sd, "CREATE FOREIGN TABLE", NULL);
				finish(sd, &s, cat, "CREATE FOREIGN TABLE");
			}
			break;

		case T_CreateTableAsStmt:
			{
				CreateTableAsStmt *c = (CreateTableAsStmt *) parsetree;

				if (c->into != NULL)
					ns_add(&s, rv_creation_ns(c->into->rel));
				if (s.dist != NIL)
					refuse(sd, c->objtype == OBJECT_MATVIEW ? "CREATE MATERIALIZED VIEW" : "CREATE TABLE AS or SELECT INTO",
						   c->objtype == OBJECT_MATVIEW ? NULL :
						   "Create the table, then INSERT ... SELECT into it from a node that holds the schema.");
				finish(sd, &s, cat, "CREATE TABLE AS");
			}
			break;

		case T_ViewStmt:
			ns_add(&s, rv_creation_ns(((ViewStmt *) parsetree)->view));
			finish(sd, &s, cat, "CREATE VIEW");
			break;

		case T_CreateSeqStmt:
			ns_add(&s, rv_creation_ns(((CreateSeqStmt *) parsetree)->sequence));
			finish(sd, &s, cat, "CREATE SEQUENCE");
			break;

		case T_AlterSeqStmt:
			ns_add(&s, rv_ns(((AlterSeqStmt *) parsetree)->sequence));
			finish(sd, &s, cat, "ALTER SEQUENCE");
			break;

		case T_CreateFunctionStmt:
			ns_add(&s, names_creation_ns(((CreateFunctionStmt *) parsetree)->funcname));
			finish(sd, &s, cat, "CREATE FUNCTION");
			break;

		case T_AlterFunctionStmt:
			ns_add(&s, func_ns(((AlterFunctionStmt *) parsetree)->objtype, ((AlterFunctionStmt *) parsetree)->func));
			finish(sd, &s, cat, "ALTER FUNCTION");
			break;

		case T_CompositeTypeStmt:
			ns_add(&s, rv_creation_ns(((CompositeTypeStmt *) parsetree)->typevar));
			finish(sd, &s, cat, "CREATE TYPE");
			break;

		case T_CreateEnumStmt:
			ns_add(&s, names_creation_ns(((CreateEnumStmt *) parsetree)->typeName));
			finish(sd, &s, cat, "CREATE TYPE");
			break;

		case T_CreateRangeStmt:
			ns_add(&s, names_creation_ns(((CreateRangeStmt *) parsetree)->typeName));
			if (s.dist != NIL)
				refuse(sd, "CREATE TYPE ... AS RANGE", NULL);
			finish(sd, &s, cat, "CREATE TYPE");
			break;

		case T_CreateDomainStmt:
			ns_add(&s, names_creation_ns(((CreateDomainStmt *) parsetree)->domainname));
			finish(sd, &s, cat, "CREATE DOMAIN");
			break;

		case T_AlterEnumStmt:
			ns_add(&s, typename_ns(((AlterEnumStmt *) parsetree)->typeName));
			finish(sd, &s, cat, "ALTER TYPE");
			break;

		case T_AlterDomainStmt:
			ns_add(&s, typename_ns(((AlterDomainStmt *) parsetree)->typeName));
			finish(sd, &s, cat, "ALTER DOMAIN");
			break;

		case T_AlterTypeStmt:
			ns_add(&s, typename_ns(((AlterTypeStmt *) parsetree)->typeName));
			finish(sd, &s, cat, "ALTER TYPE");
			break;

		case T_DefineStmt:
			ns_add(&s, names_creation_ns(((DefineStmt *) parsetree)->defnames));
			if (s.dist != NIL)
				refuse(sd, "this kind of CREATE (an aggregate, an operator, a collation or a text search object)", NULL);
			finish(sd, &s, cat, "CREATE");
			break;

		case T_IndexStmt:
			{
				IndexStmt  *is = (IndexStmt *) parsetree;

				ns_add(&s, rv_ns(is->relation));
				if (s.dist != NIL)
				{
					if (is->concurrent)
						refuse(sd, "CREATE INDEX CONCURRENTLY", "It cannot run in the two-phase transaction; create the index without CONCURRENTLY.");
					sd->verify_tables = lappend(sd->verify_tables, is->relation);
				}
				finish(sd, &s, cat, "CREATE INDEX");
			}
			break;

		case T_CreateTrigStmt:
			{
				CreateTrigStmt *t = (CreateTrigStmt *) parsetree;

				/* the triggers of kingres_ledger (its two functions) belong to the node that holds the ledger table (design 11.1): not propagated; a user trigger of that name is */
				if (t->trigname != NULL && strncmp(t->trigname, "kingres_ledger_", 15) == 0 && list_length(t->funcname) == 2 &&
					strcmp(strVal(linitial(t->funcname)), "kingres") == 0 &&
					(strcmp(strVal(lsecond(t->funcname)), "ledger_capture") == 0 || strcmp(strVal(lsecond(t->funcname)), "ledger_block_modify") == 0))
					break;
				ns_add(&s, rv_ns(t->relation));
				if (s.dist != NIL)
					sd->verify_tables = lappend(sd->verify_tables, t->relation);
				finish(sd, &s, cat, "CREATE TRIGGER");
			}
			break;

		case T_AlterTableStmt:
			{
				AlterTableStmt *a = (AlterTableStmt *) parsetree;
				Oid			ns = rv_ns(a->relation);

				/* ENABLE ALWAYS TRIGGER of a trigger of kingres_ledger (the last step of ledger_enable) is as node-local as the trigger */
				if (a->cmds != NIL)
				{
					ListCell   *lc_l;
					bool		all_ledger = true;

					foreach(lc_l, a->cmds)
					{
						AlterTableCmd *lcmd = lfirst_node(AlterTableCmd, lc_l);

						if (lcmd->subtype != AT_EnableAlwaysTrig || lcmd->name == NULL || strncmp(lcmd->name, "kingres_ledger_", 15) != 0)
							all_ledger = false;
					}
					if (all_ledger)
						break;
				}

				ns_add(&s, ns);
				/* a key from a table outside a distributed schema to a table of one is refused too */
				if (s.dist == NIL && OidIsValid(ns) && s.other != NIL)
					foreach(lc, a->cmds)
					{
						AlterTableCmd *cmd = (AlterTableCmd *) lfirst(lc);

						if (cmd->subtype == AT_AddConstraint && cmd->def != NULL && IsA(cmd->def, Constraint))
							check_foreign_key((Constraint *) cmd->def, ns, cat, sd);
					}
				if (s.dist != NIL)
				{
					foreach(lc, a->cmds)
					{
						AlterTableCmd *cmd = (AlterTableCmd *) lfirst(lc);

						switch (cmd->subtype)
						{
							case AT_AddConstraint:
								if (cmd->def != NULL && IsA(cmd->def, Constraint))
									check_foreign_key((Constraint *) cmd->def, ns, cat, sd);
								break;
							case AT_AddColumn:
								if (cmd->def != NULL && IsA(cmd->def, ColumnDef))
								{
									ListCell   *lc2;

									foreach(lc2, ((ColumnDef *) cmd->def)->constraints)
										if (IsA(lfirst(lc2), Constraint))
											check_foreign_key((Constraint *) lfirst(lc2), ns, cat, sd);
								}
								break;
							case AT_AttachPartition:
								{
									PartitionCmd *pc = (PartitionCmd *) cmd->def;
									Oid			pns = rv_ns(pc->name);

									if (OidIsValid(pns) && pns != ns)
										refuse_message(sd, "a partition of a table of a distributed schema must be in the same schema",
													   "Move the partition into the schema first.");
								}
								break;
							case AT_AddInherit:
								{
									Oid			pns = rv_ns((RangeVar *) cmd->def);

									if (OidIsValid(pns) && pns != ns)
										refuse_message(sd, "a table of a distributed schema cannot inherit from a table of another schema",
													   NULL);
								}
								break;
							case AT_EnableRowSecurity:
							case AT_ForceRowSecurity:
								refuse(sd, "row-level security", NULL);
								break;
							default:
								break;
						}
					}
					if (a->objtype == OBJECT_TABLE)
					{
						sd->verify_tables = lappend(sd->verify_tables, a->relation);
						sd->may_create_tables = true;	/* an ATTACH PARTITION, for one, makes no table, but nothing is lost */
					}
				}
				finish(sd, &s, cat, "ALTER TABLE");
			}
			break;

		case T_RenameStmt:
			{
				RenameStmt *r = (RenameStmt *) parsetree;

				if (r->relation != NULL)
					ns_add(&s, rv_ns(r->relation));
				else if (r->renameType == OBJECT_SCHEMA)
					ns_add(&s, r->subname != NULL ? get_namespace_oid(r->subname, true) : InvalidOid);
				else
					ns_add(&s, object_ns(r->renameType, r->object));
				finish(sd, &s, cat, "a rename");
			}
			break;

		case T_AlterObjectSchemaStmt:
			{
				AlterObjectSchemaStmt *a = (AlterObjectSchemaStmt *) parsetree;
				Oid			target = a->newschema != NULL ? get_namespace_oid(a->newschema, true) : InvalidOid;

				/* a table across the border of a distributed schema (design 7.9, 8.3) */
				if (a->objectType == OBJECT_TABLE && a->relation != NULL && OidIsValid(target))
				{
					Oid			relid = RangeVarGetRelid(a->relation, NoLock, true);

					if (OidIsValid(relid))
					{
						Oid			src_ns = get_rel_namespace(relid);

						if (src_ns != target &&
							(kingres_shard_lookup_dist_schema(cat, src_ns) != NULL || kingres_shard_lookup_dist_schema(cat, target) != NULL))
						{
							set_schema_table(parsetree, relid, src_ns, target, cat, forwarded, sd, &s);
							break;
						}
					}
				}

				if (a->relation != NULL)
					ns_add(&s, rv_ns(a->relation));
				else
					ns_add(&s, object_ns(a->objectType, a->object));
				if (s.dist != NIL || (OidIsValid(target) && kingres_shard_lookup_dist_schema(cat, target) != NULL))
					refuse_message(sd, "ALTER ... SET SCHEMA into or out of a distributed schema is not supported yet",
								   "Recreate the object in the other schema.");
				finish(sd, &s, cat, "ALTER ... SET SCHEMA");
			}
			break;

		case T_ReassignOwnedStmt:
			{
				/* the owners of a distributed schema and of its tables are the same on every node: this statement is run here only */
				ReassignOwnedStmt *r = (ReassignOwnedStmt *) parsetree;
				ListCell   *lcr;
				List	   *roles = NIL;

				if (forwarded || cat->ndist_schemas == 0)
					break;
				foreach(lcr, r->roles)
				{
					Oid			rid = get_rolespec_oid((RoleSpec *) lfirst(lcr), true);

					if (OidIsValid(rid))
						roles = lappend_oid(roles, rid);
				}
				if (roles != NIL)
				{
					Datum	   *elems = palloc(sizeof(Datum) * list_length(roles));
					int			k = 0;
					ArrayType  *arr;
					Oid			argtypes[1] = {OIDARRAYOID};
					Datum		values[1];
					ListCell   *lcq;
					bool		owns = false;

					foreach(lcq, roles)
						elems[k++] = ObjectIdGetDatum(lfirst_oid(lcq));
					arr = construct_array_builtin(elems, k, OIDOID);
					values[0] = PointerGetDatum(arr);
					owns = run_as_bootstrap("SELECT CASE WHEN " KINGRES_SHARD_SCHEMA ".internal_roles_own_distributed($1) THEN 1 ELSE 0 END",
											1, argtypes, values, true) != 0;
					if (owns)
						refuse_message(sd, "REASSIGN OWNED of a role that owns a distributed schema or one of its tables is not supported",
									   "Change the owner of each object with ALTER ... OWNER TO, which is run on every node.");
				}
			}
			break;

		case T_CreateExtensionStmt:
			{
				CreateExtensionStmt *ce = (CreateExtensionStmt *) parsetree;
				ListCell   *lce;

				foreach(lce, ce->options)
				{
					DefElem    *de = lfirst_node(DefElem, lce);

					if (strcmp(de->defname, "schema") == 0 && de->arg != NULL &&
						kingres_shard_lookup_dist_schema(cat, get_namespace_oid(strVal(de->arg), true)) != NULL)
						refuse_message(sd, "CREATE EXTENSION ... SCHEMA a distributed schema is not supported",
									   "The objects of the extension would exist on this node only.");
				}
			}
			break;

		case T_AlterOwnerStmt:
			ns_add(&s, object_ns(((AlterOwnerStmt *) parsetree)->objectType, ((AlterOwnerStmt *) parsetree)->object));
			finish(sd, &s, cat, "ALTER ... OWNER TO");
			break;

		case T_CommentStmt:
			ns_add(&s, object_ns(((CommentStmt *) parsetree)->objtype, ((CommentStmt *) parsetree)->object));
			finish(sd, &s, cat, "COMMENT");
			break;

		case T_SecLabelStmt:
			ns_add(&s, object_ns(((SecLabelStmt *) parsetree)->objtype, ((SecLabelStmt *) parsetree)->object));
			finish(sd, &s, cat, "SECURITY LABEL");
			break;

		case T_GrantStmt:
			{
				GrantStmt  *g = (GrantStmt *) parsetree;

				if (g->targtype == ACL_TARGET_ALL_IN_SCHEMA ||
					(g->targtype == ACL_TARGET_OBJECT && g->objtype == OBJECT_SCHEMA))
				{
					foreach(lc, g->objects)
						if (IsA(lfirst(lc), String))
							ns_add(&s, get_namespace_oid(strVal(lfirst(lc)), true));
				}
				else if (g->targtype == ACL_TARGET_OBJECT)
				{
					foreach(lc, g->objects)
					{
						Node	   *o = (Node *) lfirst(lc);

						switch (g->objtype)
						{
							case OBJECT_TABLE:
							case OBJECT_SEQUENCE:
								if (IsA(o, RangeVar))
									ns_add(&s, rv_ns((RangeVar *) o));
								break;
							case OBJECT_FUNCTION:
							case OBJECT_PROCEDURE:
							case OBJECT_ROUTINE:
								if (IsA(o, ObjectWithArgs))
									ns_add(&s, func_ns(g->objtype, (ObjectWithArgs *) o));
								break;
							case OBJECT_TYPE:
							case OBJECT_DOMAIN:
								if (IsA(o, TypeName))
									ns_add(&s, typename_ns(((TypeName *) o)->names));
								break;
							default:
								break;
						}
					}
				}
				finish(sd, &s, cat, g->is_grant ? "GRANT" : "REVOKE");
			}
			break;

		case T_AlterDefaultPrivilegesStmt:
			{
				AlterDefaultPrivilegesStmt *a = (AlterDefaultPrivilegesStmt *) parsetree;

				foreach(lc, a->options)
				{
					DefElem    *de = (DefElem *) lfirst(lc);

					if (strcmp(de->defname, "schemas") == 0 && IsA(de->arg, List))
					{
						ListCell   *lc2;

						foreach(lc2, (List *) de->arg)
							if (IsA(lfirst(lc2), String))
								ns_add(&s, get_namespace_oid(strVal(lfirst(lc2)), true));
					}
				}
				finish(sd, &s, cat, "ALTER DEFAULT PRIVILEGES");
			}
			break;

		case T_TruncateStmt:
			{
				TruncateStmt *t = (TruncateStmt *) parsetree;

				foreach(lc, t->relations)
					ns_add(&s, rv_ns((RangeVar *) lfirst(lc)));
				if (s.dist != NIL && t->behavior == DROP_CASCADE)
					refuse(sd, "TRUNCATE ... CASCADE", "Name the tables to truncate.");
				finish(sd, &s, cat, "TRUNCATE");
			}
			break;

		case T_DropStmt:
			{
				DropStmt   *d = (DropStmt *) parsetree;

				foreach(lc, d->objects)
				{
					Node	   *o = (Node *) lfirst(lc);

					if (d->removeType == OBJECT_SCHEMA)
					{
						if (IsA(o, String))
							ns_add(&s, get_namespace_oid(strVal(o), true));
					}
					else if (d->removeType == OBJECT_EXTENSION || d->removeType == OBJECT_PUBLICATION ||
							 d->removeType == OBJECT_SUBSCRIPTION || d->removeType == OBJECT_EVENT_TRIGGER ||
							 d->removeType == OBJECT_ROLE || d->removeType == OBJECT_DATABASE ||
							 d->removeType == OBJECT_TABLESPACE || d->removeType == OBJECT_FDW ||
							 d->removeType == OBJECT_FOREIGN_SERVER || d->removeType == OBJECT_ACCESS_METHOD ||
							 d->removeType == OBJECT_LANGUAGE)
						continue;
					else
						ns_add(&s, object_ns(d->removeType, o));
				}
				if (s.dist != NIL && d->removeType == OBJECT_SCHEMA)
					sd->drops_schema = true;
				finish(sd, &s, cat, "DROP");
			}
			break;

		case T_RuleStmt:
			ns_add(&s, rv_ns(((RuleStmt *) parsetree)->relation));
			if (s.dist != NIL)
				refuse(sd, "CREATE RULE", NULL);
			finish(sd, &s, cat, "CREATE RULE");
			break;

		case T_CreatePolicyStmt:
			ns_add(&s, rv_ns(((CreatePolicyStmt *) parsetree)->table));
			if (s.dist != NIL)
				refuse(sd, "CREATE POLICY", NULL);
			finish(sd, &s, cat, "CREATE POLICY");
			break;

		case T_AlterPolicyStmt:
			ns_add(&s, rv_ns(((AlterPolicyStmt *) parsetree)->table));
			if (s.dist != NIL)
				refuse(sd, "ALTER POLICY", NULL);
			finish(sd, &s, cat, "ALTER POLICY");
			break;

		case T_CreateStatsStmt:
			foreach(lc, ((CreateStatsStmt *) parsetree)->relations)
				if (IsA(lfirst(lc), RangeVar))
					ns_add(&s, rv_ns((RangeVar *) lfirst(lc)));
			if (s.dist != NIL)
				refuse(sd, "CREATE STATISTICS", NULL);
			finish(sd, &s, cat, "CREATE STATISTICS");
			break;

		default:
			break;
	}

	/* a refusal is the origin's to make: the session of another node runs what it was sent */
	if (forwarded)
	{
		sd->refuse_what = NULL;
		sd->refuse_message = NULL;
		sd->refuse_hint = NULL;
	}
}

/* ---------------------------------------------------------------------------
 * The node-local steps (the rights of the bootstrap superuser)
 * ------------------------------------------------------------------------- */

/* Run one statement as the bootstrap superuser; returns the first column of the first row as an int when want_int. */
static int
run_as_bootstrap(const char *sql, int nargs, Oid *argtypes, Datum *values, bool want_int)
{
	Oid			save_uid;
	int			save_sec;
	int			result = 0;

	kingres_shard_require_catalog();
	GetUserIdAndSecContext(&save_uid, &save_sec);
	SetUserIdAndSecContext(BOOTSTRAP_SUPERUSERID, save_sec | SECURITY_LOCAL_USERID_CHANGE);
	PG_TRY();
	{
		int			ret;

		if (SPI_connect() != SPI_OK_CONNECT)
			elog(ERROR, "SPI_connect failed");
		ret = SPI_execute_with_args(sql, nargs, argtypes, values, NULL, false, 0);
		if (ret < 0)
			elog(ERROR, "SPI_execute_with_args failed: %s", SPI_result_code_string(ret));
		if (want_int && SPI_tuptable != NULL && SPI_processed > 0)
		{
			bool		isnull;
			Datum		d = SPI_getbinval(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1, &isnull);

			if (!isnull)
				result = DatumGetInt32(d);
		}
		SPI_finish();
	}
	PG_FINALLY();
	{
		SetUserIdAndSecContext(save_uid, save_sec);
	}
	PG_END_TRY();
	return result;
}

void
kingres_shard_schema_check_limit(const KsCatalog *cat)
{
	if (cat->ndist_schemas >= kingres_shard_max_distributed_schemas)
		ereport(ERROR,
				(errcode(ERRCODE_PROGRAM_LIMIT_EXCEEDED),
				 errmsg("the database already has %d distributed schemas, the limit of kingres_shard.max_distributed_schemas",
						cat->ndist_schemas)));
}

int
kingres_shard_schema_choose_group(void)
{
	int			g;

	if (kingres_shard_schema_placement_group >= 0)
	{
		Oid			argtypes[1] = {INT4OID};
		Datum		values[1];

		values[0] = Int32GetDatum(kingres_shard_schema_placement_group);
		if (run_as_bootstrap("SELECT CASE WHEN EXISTS (SELECT 1 FROM " KINGRES_SHARD_SCHEMA ".node n "
							 "WHERE n.group_id OPERATOR(pg_catalog.=) $1 AND n.role OPERATOR(pg_catalog.=) 'primary' "
							 "AND n.is_active AND n.should_have_shards) THEN 1 ELSE 0 END",
							 1, argtypes, values, true) == 0)
			ereport(ERROR,
					(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
					 errmsg("shard group %d cannot hold a schema: it has no active primary that should have shards",
							kingres_shard_schema_placement_group)));
		return kingres_shard_schema_placement_group;
	}
	g = run_as_bootstrap("SELECT COALESCE(" KINGRES_SHARD_SCHEMA ".internal_choose_schema_group(), -1)", 0, NULL, NULL, true);
	if (g < 0)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("no shard group can hold a schema")));
	return g;
}

int
kingres_shard_schema_next_colocation(void)
{
	return run_as_bootstrap("SELECT pg_catalog.nextval('" KINGRES_SHARD_SCHEMA ".colocation_id_seq')::pg_catalog.int4", 0, NULL, NULL, true);
}

void
kingres_shard_schema_register_local(const char *name, int colocation_id, int group_id)
{
	Oid			argtypes[3] = {NAMEOID, INT4OID, INT4OID};
	Datum		values[3];
	NameData	nm;

	namestrcpy(&nm, name);
	values[0] = NameGetDatum(&nm);
	values[1] = Int32GetDatum(colocation_id);
	values[2] = Int32GetDatum(group_id);
	(void) run_as_bootstrap("SELECT " KINGRES_SHARD_SCHEMA ".internal_register_schema($1, $2, $3)", 3, argtypes, values, false);
}

int
kingres_shard_schema_sync_tables_local(const char *name)
{
	Oid			argtypes[1] = {NAMEOID};
	Datum		values[1];
	NameData	nm;

	namestrcpy(&nm, name);
	values[0] = NameGetDatum(&nm);
	return run_as_bootstrap("SELECT " KINGRES_SHARD_SCHEMA ".internal_register_schema_tables($1)", 1, argtypes, values, true);
}

int
kingres_shard_schema_reconcile_local(void)
{
	return run_as_bootstrap("SELECT " KINGRES_SHARD_SCHEMA ".internal_schema_reconcile()", 0, NULL, NULL, true);
}

/* assert_empty or drop_shell of a shell, by its quoted, schema-qualified name */
void
kingres_shard_schema_table_op_local(const char *op, const char *rel)
{
	Oid			argtypes[1] = {TEXTOID};
	Datum		values[1];

	values[0] = CStringGetTextDatum(rel);
	if (strcmp(op, "assert_empty") == 0)
		(void) run_as_bootstrap("SELECT " KINGRES_SHARD_SCHEMA ".internal_schema_assert_empty($1)", 1, argtypes, values, false);
	else
		(void) run_as_bootstrap("SELECT " KINGRES_SHARD_SCHEMA ".internal_schema_drop_shell($1)", 1, argtypes, values, false);
}

/* The statements that make the table as a shell on another node (a list of char *). */
List *
kingres_shard_schema_table_statements_local(Oid relid)
{
	Oid			argtypes[1] = {OIDOID};
	Datum		values[1];
	List	   *result = NIL;
	Oid			save_uid;
	int			save_sec;
	MemoryContext callercxt = CurrentMemoryContext;

	values[0] = ObjectIdGetDatum(relid);
	kingres_shard_require_catalog();
	GetUserIdAndSecContext(&save_uid, &save_sec);
	SetUserIdAndSecContext(BOOTSTRAP_SUPERUSERID, save_sec | SECURITY_LOCAL_USERID_CHANGE);
	PG_TRY();
	{
		if (SPI_connect() != SPI_OK_CONNECT)
			elog(ERROR, "SPI_connect failed");
		if (SPI_execute_with_args("SELECT s FROM pg_catalog.unnest(" KINGRES_SHARD_SCHEMA ".internal_table_shell_statements($1::pg_catalog.oid::pg_catalog.regclass)) AS s",
								  1, argtypes, values, NULL, false, 0) == SPI_OK_SELECT)
		{
			uint64		i;

			for (i = 0; i < SPI_processed; i++)
			{
				char	   *v = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1);
				MemoryContext cur = MemoryContextSwitchTo(callercxt);

				if (v != NULL)
					result = lappend(result, pstrdup(v));
				MemoryContextSwitchTo(cur);
			}
		}
		SPI_finish();
	}
	PG_FINALLY();
	{
		SetUserIdAndSecContext(save_uid, save_sec);
	}
	PG_END_TRY();
	return result;
}

/*
 * The structure of a table of this node as a fingerprint (kingres.internal_table_fingerprint, 7.1).
  The session
 * is the caller's: the same function runs, in the same transaction branch, on the other nodes (verify_remote), and
 * the function reads the catalogs of PostgreSQL only.
 */
char *
kingres_shard_schema_fingerprint_local(Oid relid)
{
	Oid			argtypes[1] = {OIDOID};
	Datum		values[1];
	char	   *result = NULL;
	int			ret;
	MemoryContext callercxt = CurrentMemoryContext;

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	values[0] = ObjectIdGetDatum(relid);
	ret = SPI_execute_with_args("SELECT " KINGRES_SHARD_SCHEMA ".internal_table_fingerprint($1::pg_catalog.oid::pg_catalog.regclass)",
								1, argtypes, values, NULL, false, 1);
	if (ret == SPI_OK_SELECT && SPI_processed == 1)
	{
		char	   *v = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);

		if (v != NULL)
		{
			MemoryContext cur = MemoryContextSwitchTo(callercxt);

			result = pstrdup(v);
			MemoryContextSwitchTo(cur);
		}
	}
	SPI_finish();
	if (result == NULL)
		elog(ERROR, "could not compute the fingerprint of table %u", relid);
	return result;
}

/* The text the fingerprint is the md5 of (for the message of a difference). */
static char *
kingres_shard_schema_structure_local(Oid relid)
{
	Oid			argtypes[1] = {OIDOID};
	Datum		values[1];
	char	   *result = NULL;
	MemoryContext callercxt = CurrentMemoryContext;

	if (SPI_connect() != SPI_OK_CONNECT)
		return NULL;
	values[0] = ObjectIdGetDatum(relid);
	if (SPI_execute_with_args("SELECT " KINGRES_SHARD_SCHEMA ".internal_table_structure($1::pg_catalog.oid::pg_catalog.regclass)",
							  1, argtypes, values, NULL, false, 1) == SPI_OK_SELECT && SPI_processed == 1)
	{
		char	   *v = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);

		if (v != NULL)
		{
			MemoryContext cur = MemoryContextSwitchTo(callercxt);

			result = pstrdup(v);
			MemoryContextSwitchTo(cur);
		}
	}
	SPI_finish();
	return result;
}

/* The same fingerprint on another node, in the branch of the statement; an error when it differs. */
void
kingres_shard_schema_verify_remote(KsConn *kc, int node_id, Oid relid, const char *local_fingerprint)
{
	char	   *qname = quote_qualified_identifier(get_namespace_name(get_rel_namespace(relid)), get_rel_name(relid));
	char	   *sql = psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_table_fingerprint(%s::pg_catalog.regclass)",
							   quote_literal_cstr(qname));
	PGresult   *res = kingres_shard_exec(kc, sql);
	bool		same = false;

	if (PQresultStatus(res) == PGRES_TUPLES_OK && PQntuples(res) == 1 && !PQgetisnull(res, 0, 0))
		same = strcmp(PQgetvalue(res, 0, 0), local_fingerprint) == 0;
	else
	{
		PQclear(res);
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("could not read the structure of table %s on node %d", qname, node_id)));
	}
	PQclear(res);
	if (!same)
	{
		/* say what differs: the structure text of this node and of that one, cut */
		char	   *theirs = NULL;
		char	   *mine = NULL;
		char	   *sql2 = psprintf("SELECT " KINGRES_SHARD_SCHEMA ".internal_table_structure(%s::pg_catalog.regclass)",
									quote_literal_cstr(qname));
		PGresult   *res2 = kingres_shard_exec(kc, sql2);

		if (PQresultStatus(res2) == PGRES_TUPLES_OK && PQntuples(res2) == 1 && !PQgetisnull(res2, 0, 0))
			theirs = pstrdup(PQgetvalue(res2, 0, 0));
		PQclear(res2);
		mine = kingres_shard_schema_structure_local(relid);
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("the structure of table %s differs on node %d", qname, node_id),
				 errdetail("The statement made a different table there (a schema of the same name earlier in a search path, or an object that exists on one node only).  Here: %.400s  There: %.400s",
						   mine != NULL ? mine : "?", theirs != NULL ? theirs : "?"),
				 errhint("The statement is rolled back on every node.")));
	}
}

/*
 * kingres.internal_schema_delta(op text, nsp name, colocation_id int, group_id int) returns int
 *
 * What the origin of a propagated statement asks every other node to do after the statement, in the same
 * transaction: 'register_schema' (the schema of a CREATE SCHEMA that the database distributes) and
 * 'register_tables' (the tables that the statement made).  Not revoked from PUBLIC because the session that
 * calls it runs as the role of the user whose statement it is; it answers superusers and the sessions
 * that other nodes opened (the proof of forward_auth.c) only.
 */
Datum
kingres_internal_schema_delta(PG_FUNCTION_ARGS)
{
	char	   *op;
	Name		nsp;
	int			result = 0;

	if (!superuser() && !kingres_shard_session_forwarded())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for the internal functions of kingres_shard")));
	if (PG_ARGISNULL(0) || PG_ARGISNULL(1))
		PG_RETURN_NULL();
	op = text_to_cstring(PG_GETARG_TEXT_PP(0));
	nsp = PG_GETARG_NAME(1);
	if (strcmp(op, "register_schema") == 0)
	{
		if (PG_ARGISNULL(2) || PG_ARGISNULL(3))
			ereport(ERROR,
					(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
					 errmsg("register_schema needs the colocation id and the group")));
		kingres_shard_schema_register_local(NameStr(*nsp), PG_GETARG_INT32(2), PG_GETARG_INT32(3));
	}
	else if (strcmp(op, "register_tables") == 0)
		result = kingres_shard_schema_sync_tables_local(NameStr(*nsp));
	else if (strcmp(op, "reconcile") == 0)
		result = kingres_shard_schema_reconcile_local();
	else if (strcmp(op, "assert_empty") == 0 || strcmp(op, "drop_shell") == 0)
	{
		if (PG_NARGS() < 5 || PG_ARGISNULL(4))
			ereport(ERROR,
					(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
					 errmsg("%s needs the table", op)));
		kingres_shard_schema_table_op_local(op, text_to_cstring(PG_GETARG_TEXT_PP(4)));
	}
	else
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("unknown schema delta \"%s\"", op)));
	PG_RETURN_INT32(result);
}
