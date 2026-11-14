/*-------------------------------------------------------------------------
 *
 * protect.c
 *	  ProcessUtility hook that stops utility statements from switching the
 *	  ledger off.
 *
 * What is protected.  A "ledger table" is an ordinary table that is
 * registered in kingres.ledger_table, or whose capture trigger (a trigger
 * of the extension's C function kingres_ledger_capture) is ENABLE ALWAYS;
 * ledger_enable arms that trigger before it registers the table, so a table
 * is protected from the moment it is fully set up.  The "ledger catalog
 * tables" are the kingres.ledger_* tables that belong to the extension.
 * For every role, superusers included, the hook rejects with SQLSTATE KL002:
 *
 *	  - on ledger tables: DROP TABLE, DROP COLUMN, ALTER COLUMN TYPE, RENAME
 *		COLUMN, ATTACH/DETACH PARTITION, SET SCHEMA, DROP TRIGGER of a ledger
 *		trigger, every ALTER TABLE subcommand that changes how triggers fire
 *		(DISABLE/ENABLE ... TRIGGER), changes the persistence, the
 *		inheritance or the generated values of the table, and ADD COLUMN
 *		unless the new column is nullable and has no default;
 *	  - every statement that would make a table a child of a ledger table, or
 *		a ledger table a child (CREATE TABLE and CREATE FOREIGN TABLE with
 *		INHERITS or PARTITION OF, ALTER TABLE INHERIT, ATTACH PARTITION);
 *	  - on the ledger catalog tables: every ALTER, RENAME, CREATE INDEX,
 *		CREATE TRIGGER, CREATE RULE, CREATE POLICY, DROP and SET SCHEMA, and
 *		every GRANT or REVOKE other than of SELECT;
 *	  - DROP EXTENSION kingres_ledger while any ledger table exists, and the
 *		DROP SCHEMA and DROP OWNED statements that would drop ledger tables;
 *	  - renaming, replacing, altering, re-owning or changing the privileges of
 *		the functions and the event triggers of the extension.
 *
 * Name resolution.  The statement itself resolves the names in it again
 * after the hook has looked at them.  If the hook resolved a name without a
 * lock, a concurrent rename could make the name point to another table in
 * between (the review finding L-03), and the hook would have checked the
 * wrong table.  For the statements that name a table, the hook therefore
 * resolves the name once, takes the lock that the statement will take
 * (as core does, by RangeVarGetRelidExtended), and decides on the OID; while
 * the lock is held the name cannot move.  A role that does not own the table
 * takes no lock, so it cannot make anybody wait; such a statement fails in
 * core for lack of ownership, except where a second role cooperates, and
 * for that case, and for every statement where the hook does not resolve
 * names reliably (DROP SCHEMA, DROP OWNED, cascades, and so on), the event
 * trigger kingres.ledger_guard enforces the rules at the point of action,
 * from the state the statement left behind, and raises an error that rolls
 * the statement back.
 *
 * What is not protected: the hook sees utility statements only.  It does not
 * stop direct modification of the catalogs, a superuser who switches event
 * triggers off, removal of the library from shared_preload_libraries, or
 * file-level tampering; verification against an external digest detects
 * those.  See the README of the extension.
 *
 * The hook never changes a statement: it either raises an error or passes
 * the statement on to the next hook, or to standard_ProcessUtility.  It does
 * nothing in a database where the extension is not installed, and nothing
 * while the extension's own install or upgrade script runs.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: PostgreSQL
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/protect.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/genam.h"
#include "access/htup_details.h"
#include "access/relation.h"
#include "access/table.h"
#include "catalog/dependency.h"
#include "catalog/index.h"
#include "catalog/namespace.h"
#include "catalog/objectaddress.h"
#include "catalog/pg_class.h"
#include "catalog/pg_extension.h"
#include "catalog/pg_language.h"
#include "catalog/pg_namespace.h"
#include "catalog/pg_proc.h"
#include "catalog/pg_trigger.h"
#include "catalog/pg_type.h"
#include "commands/event_trigger.h"
#include "commands/extension.h"
#include "commands/tablecmds.h"
#include "commands/trigger.h"
#include "miscadmin.h"
#include "nodes/makefuncs.h"
#include "nodes/parsenodes.h"
#include "parser/parse_func.h"
#include "parser/parse_type.h"
#include "storage/lmgr.h"
#include "tcop/utility.h"
#include "utils/acl.h"
#include "utils/builtins.h"
#include "utils/fmgroids.h"
#include "utils/lsyscache.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"
#include "utils/syscache.h"

#include "kingres_ledger.h"

/* What kind of protected relation a relation is. */
typedef enum KlKind
{
	KL_NOT_LEDGER,				/* not protected */
	KL_USER_LEDGER,				/* a table under ledger control */
	KL_CATALOG_LEDGER			/* a kingres.ledger_* table of the extension */
} KlKind;

static ProcessUtility_hook_type prev_ProcessUtility = NULL;

static void kl_ProcessUtility(PlannedStmt *pstmt, const char *queryString,
							  bool readOnlyTree, ProcessUtilityContext context,
							  ParamListInfo params, QueryEnvironment *queryEnv,
							  DestReceiver *dest, QueryCompletion *qc);

/*
 * kl_protect_init
 *		Install the hook, chaining to a previously installed one.  Called
 *		from _PG_init when the library is preloaded.
 */
void
kl_protect_init(void)
{
	prev_ProcessUtility = ProcessUtility_hook;
	ProcessUtility_hook = kl_ProcessUtility;
}

/*
 * kl_reject
 *		Raise the protection error.  "action" completes "cannot ...".
 */
static void
kl_reject(const char *action, const char *name, KlKind kind)
{
	ereport(ERROR,
			(errcode(ERRCODE_KL_PROTECTED),
			 errmsg("cannot %s \"%s\": it is %s", action, name,
					kind == KL_CATALOG_LEDGER ?
					"part of the ledger" : "a ledger table"),
			 errhint("Ledger tables are append-only; the ledger can only be switched off outside the database.")));
}

/*
 * kl_reject_object
 *		Raise the protection error for an object that is not a table.
 */
static void
kl_reject_object(const char *message)
{
	ereport(ERROR,
			(errcode(ERRCODE_KL_PROTECTED),
			 errmsg("%s", message),
			 errhint("The functions, event triggers and tables of the extension cannot be changed.")));
}

/*
 * kl_member_function_symbol
 *		If the function is a C function that belongs to the extension, return
 *		its C symbol (prosrc, palloc'd), else NULL.  The SQL name of a
 *		function is not used: renaming a function does not change its symbol,
 *		and membership of the extension cannot be faked by a function that
 *		only has the same name.
 */
static char *
kl_member_function_symbol(Oid funcoid, Oid extoid)
{
	HeapTuple	tup;
	char	   *result = NULL;

	tup = SearchSysCache1(PROCOID, ObjectIdGetDatum(funcoid));
	if (!HeapTupleIsValid(tup))
		return NULL;
	if (((Form_pg_proc) GETSTRUCT(tup))->prolang == ClanguageId &&
		getExtensionOfObject(ProcedureRelationId, funcoid) == extoid)
	{
		Datum		d = SysCacheGetAttrNotNull(PROCOID, tup, Anum_pg_proc_prosrc);

		result = TextDatumGetCString(d);
	}
	ReleaseSysCache(tup);
	return result;
}

/*
 * kl_is_ledger_function
 *		Whether a function is the extension's C function with the given C
 *		symbol.
 */
static bool
kl_is_ledger_function(Oid funcoid, Oid extoid, const char *symbol)
{
	char	   *found = kl_member_function_symbol(funcoid, extoid);
	bool		result = (found != NULL && strcmp(found, symbol) == 0);

	if (found != NULL)
		pfree(found);
	return result;
}

/*
 * kl_is_internal_function
 *		Functions of the extension that only the extension itself may call:
 *		nobody is ever granted EXECUTE on them.
 */
static bool
kl_is_internal_function(Oid funcoid, Oid extoid)
{
	return kl_is_ledger_function(funcoid, extoid, KL_SYM_CAPTURE) ||
		kl_is_ledger_function(funcoid, extoid, KL_SYM_BLOCK_MODIFY) ||
		kl_is_ledger_function(funcoid, extoid, KL_SYM_RECORD_ROW) ||
		kl_is_ledger_function(funcoid, extoid, KL_SYM_LOCK_TABLE);
}

/*
 * kl_has_armed_capture_trigger
 *		Whether the table has the capture trigger of the extension, enabled
 *		ALWAYS.  Looks at pg_trigger through its index so that no lock on the
 *		table is needed and the answer follows changes made earlier in the
 *		same transaction.
 */
static bool
kl_has_armed_capture_trigger(Oid relid, Oid extoid)
{
	Relation	tgrel;
	SysScanDesc scan;
	ScanKeyData key;
	HeapTuple	tup;
	bool		found = false;

	tgrel = table_open(TriggerRelationId, AccessShareLock);
	ScanKeyInit(&key, Anum_pg_trigger_tgrelid, BTEqualStrategyNumber,
				F_OIDEQ, ObjectIdGetDatum(relid));
	scan = systable_beginscan(tgrel, TriggerRelidNameIndexId, true, NULL, 1,
							  &key);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		Form_pg_trigger trig = (Form_pg_trigger) GETSTRUCT(tup);

		if (trig->tgenabled == TRIGGER_FIRES_ALWAYS &&
			kl_is_ledger_function(trig->tgfoid, extoid, KL_SYM_CAPTURE))
		{
			found = true;
			break;
		}
	}
	systable_endscan(scan);
	table_close(tgrel, AccessShareLock);
	return found;
}

/*
 * kl_is_registered
 *		Whether the table is registered in kingres.ledger_table.  Uses the
 *		latest snapshot so that a table registered by the current transaction
 *		is seen.  Registration is what makes a table a ledger table even
 *		after its triggers were switched off by a bypass.
 */
static bool
kl_is_registered(Oid relid)
{
	Oid			nspoid = get_namespace_oid(KL_SCHEMA_NAME, true);
	Oid			tableoid;
	Oid			idxoid;
	Relation	rel;
	SysScanDesc scan;
	ScanKeyData key;
	Snapshot	snap;
	bool		found;

	if (!OidIsValid(nspoid))
		return false;
	tableoid = get_relname_relid("ledger_table", nspoid);
	idxoid = get_relname_relid("ledger_table_relid_key", nspoid);
	if (!OidIsValid(tableoid) || !OidIsValid(idxoid))
		return false;

	rel = table_open(tableoid, AccessShareLock);
	snap = RegisterSnapshot(GetLatestSnapshot());
	ScanKeyInit(&key, 2, BTEqualStrategyNumber, F_OIDEQ,
				ObjectIdGetDatum(relid));
	scan = systable_beginscan(rel, idxoid, true, snap, 1, &key);
	found = HeapTupleIsValid(systable_getnext(scan));
	systable_endscan(scan);
	UnregisterSnapshot(snap);
	table_close(rel, AccessShareLock);
	return found;
}

/*
 * kl_classify
 *		Classify a relation.  An index is classified as its table.
 */
static KlKind
kl_classify(Oid relid, Oid extoid)
{
	char		relkind;

	if (!OidIsValid(relid))
		return KL_NOT_LEDGER;
	relkind = get_rel_relkind(relid);
	if (relkind == RELKIND_INDEX || relkind == RELKIND_PARTITIONED_INDEX)
	{
		relid = IndexGetRelation(relid, true);
		if (!OidIsValid(relid))
			return KL_NOT_LEDGER;
		relkind = get_rel_relkind(relid);
	}
	if (relkind != RELKIND_RELATION)
		return KL_NOT_LEDGER;
	if (getExtensionOfObject(RelationRelationId, relid) == extoid)
		return KL_CATALOG_LEDGER;
	if (kl_is_registered(relid) || kl_has_armed_capture_trigger(relid, extoid))
		return KL_USER_LEDGER;
	return KL_NOT_LEDGER;
}

/*
 * kl_rv_relid
 *		Resolve a RangeVar once, and lock the relation with the lock the
 *		statement is going to take, so that the name cannot be moved to
 *		another table between this check and the statement (see the file
 *		header).  Pass NoLock for statements where the lock is not worth it.
 *		A relation that the current role does not own is not locked: nobody
 *		may be made to wait by a statement that is going to fail, and a
 *		role that does not own a table cannot change it anyway.
 */
static Oid
kl_rv_relid(const RangeVar *rv, LOCKMODE lockmode)
{
	Oid			relid;

	if (rv == NULL)
		return InvalidOid;
	relid = RangeVarGetRelid(rv, NoLock, true);
	if (lockmode == NoLock || !OidIsValid(relid))
		return relid;
	if (!object_ownercheck(RelationRelationId, relid, GetUserId()))
		return relid;
	return RangeVarGetRelidExtended(rv, lockmode, RVR_MISSING_OK, NULL, NULL);
}

/*
 * kl_ledger_relids
 *		The relids of all tables registered in kingres.ledger_table.
 *		Uses the latest snapshot so that a table registered by the current
 *		transaction is seen.
 */
static List *
kl_ledger_relids(void)
{
	List	   *result = NIL;
	Oid			nspoid = get_namespace_oid(KL_SCHEMA_NAME, true);
	Oid			tableoid;
	Relation	rel;
	SysScanDesc scan;
	HeapTuple	tup;
	Snapshot	snap;

	if (!OidIsValid(nspoid))
		return NIL;
	tableoid = get_relname_relid("ledger_table", nspoid);
	if (!OidIsValid(tableoid))
		return NIL;

	rel = table_open(tableoid, AccessShareLock);
	snap = RegisterSnapshot(GetLatestSnapshot());
	scan = systable_beginscan(rel, InvalidOid, false, snap, 0, NULL);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		bool		isnull;
		Datum		d = heap_getattr(tup, 2, RelationGetDescr(rel), &isnull);

		if (!isnull)
			result = lappend_oid(result, DatumGetObjectId(d));
	}
	systable_endscan(scan);
	UnregisterSnapshot(snap);
	table_close(rel, AccessShareLock);
	return result;
}

/*
 * kl_name_list_relid
 *		Relation named by a list of String nodes, as used in DropStmt.
 */
static Oid
kl_name_list_relid(List *names, LOCKMODE lockmode)
{
	if (names == NIL)
		return InvalidOid;
	return kl_rv_relid(makeRangeVarFromNameList(names), lockmode);
}

/*
 * Prefix of the names of the triggers the extension creates on a ledger
 * table.  The length used in comparisons is derived from the literal, so the
 * two cannot drift apart.
 */
#define KL_TRIGGER_NAME_PREFIX "kingres_ledger_"

/* True when the name starts with the prefix of the extension's own objects. */
static bool
kl_is_ledger_trigger_name(const char *name)
{
	return name != NULL &&
		strncmp(name, KL_TRIGGER_NAME_PREFIX,
				sizeof(KL_TRIGGER_NAME_PREFIX) - 1) == 0;
}

/*
 * kl_object_is_member
 *		Whether the object that a statement names (a function, an aggregate,
 *		an event trigger) is a member of the extension.  Nothing is raised for
 *		an object that does not exist: the statement itself will complain.
 */
static bool
kl_object_is_member(ObjectType objtype, Node *object, Oid extoid)
{
	Relation	rel = NULL;
	ObjectAddress addr;

	if (object == NULL)
		return false;
	addr = get_object_address(objtype, object, &rel, AccessShareLock, true);
	if (rel != NULL)
		relation_close(rel, NoLock);
	return OidIsValid(addr.objectId) &&
		getExtensionOfObject(addr.classId, addr.objectId) == extoid;
}

/*
 * kl_check_add_column
 *		ADD COLUMN on a ledger table is allowed only when the new column is
 *		nullable, has no default and is not generated or an identity column:
 *		then every existing row reads the new column as NULL and, because
 *		NULL columns are left out of the row encoding, its hash does not
 *		change.  A domain with a default would turn the NULLs into values.
 *		The type of the column is checked by the event trigger, with the same
 *		function that ledger_enable uses.
 */
static void
kl_check_add_column(ColumnDef *col, const char *relname, KlKind kind)
{
	ListCell   *lc;
	Oid			typid;

	if (col->raw_default != NULL || col->cooked_default != NULL ||
		col->identity != 0 || col->generated != 0 || col->is_not_null)
		kl_reject("add a column with a default, NOT NULL, identity or generated value to table",
				  relname, kind);
	foreach(lc, col->constraints)
	{
		Constraint *con = lfirst_node(Constraint, lc);

		if (con->contype != CONSTR_NULL)
			kl_reject("add a column with a default or a constraint to table", relname, kind);
	}
	if (col->typeName != NULL)
	{
		typid = LookupTypeNameOid(NULL, col->typeName, true);
		if (OidIsValid(typid) && get_typtype(typid) == TYPTYPE_DOMAIN &&
			get_typdefault(typid) != NULL)
			kl_reject("add a column of a domain with a default to table",
					  relname, kind);
	}
}

/*
 * kl_check_inherit_parents
 *		A list of parent tables of a new table (INHERITS or PARTITION OF).
 */
static void
kl_check_inherit_parents(List *parents, Oid extoid)
{
	ListCell   *lc;

	foreach(lc, parents)
	{
		Oid			relid = kl_rv_relid((RangeVar *) lfirst(lc),
										ShareUpdateExclusiveLock);
		KlKind		kind = kl_classify(relid, extoid);

		if (kind != KL_NOT_LEDGER)
			kl_reject("inherit from table", get_rel_name(relid), kind);
	}
}

/*
 * kl_check_alter_table
 *		ALTER TABLE on a protected table, and ATTACH/DETACH PARTITION that
 *		names a protected table as the partition.
 */
static void
kl_check_alter_table(AlterTableStmt *stmt, Oid extoid)
{
	LOCKMODE	lockmode = (stmt->objtype == OBJECT_TABLE) ?
		AlterTableGetLockLevel(stmt->cmds) : NoLock;
	Oid			relid = kl_rv_relid(stmt->relation, lockmode);
	KlKind		kind = kl_classify(relid, extoid);
	char	   *relname = relid != InvalidOid ? get_rel_name(relid) : NULL;
	ListCell   *lc;

	if (kind == KL_CATALOG_LEDGER)
		kl_reject("alter table", relname, kind);

	foreach(lc, stmt->cmds)
	{
		AlterTableCmd *cmd = lfirst_node(AlterTableCmd, lc);

		switch (cmd->subtype)
		{
			case AT_AttachPartition:
			case AT_DetachPartition:
			case AT_DetachPartitionFinalize:
				{
					PartitionCmd *pc = (PartitionCmd *) cmd->def;
					Oid			partid = kl_rv_relid(pc->name,
													 cmd->subtype == AT_AttachPartition ?
													 AccessExclusiveLock :
													 ShareUpdateExclusiveLock);
					KlKind		pkind = kl_classify(partid, extoid);

					if (pkind != KL_NOT_LEDGER)
						kl_reject("attach or detach partition",
								  get_rel_name(partid), pkind);
				}
				if (kind != KL_NOT_LEDGER)
					kl_reject("attach or detach a partition of table",
							  relname, kind);
				break;

			case AT_AddInherit:
				{
					/* Making another table a child of a ledger table. */
					Oid			parentid = kl_rv_relid((RangeVar *) cmd->def,
													   ShareUpdateExclusiveLock);
					KlKind		pkind = kl_classify(parentid, extoid);

					if (pkind != KL_NOT_LEDGER)
						kl_reject("inherit from table", get_rel_name(parentid),
								  pkind);
				}
				break;

			default:
				break;
		}
		if (kind != KL_USER_LEDGER)
			continue;

		switch (cmd->subtype)
		{
			case AT_AddColumn:
				kl_check_add_column((ColumnDef *) cmd->def, relname, kind);
				break;
			case AT_DropColumn:
				kl_reject("drop a column of table", relname, kind);
				break;
			case AT_AlterColumnType:
				kl_reject("change the type of a column of table", relname, kind);
				break;
			case AT_SetExpression:
				kl_reject("change the generation expression of a column of table",
						  relname, kind);
				break;
			case AT_EnableTrig:
			case AT_EnableAlwaysTrig:
			case AT_EnableReplicaTrig:
			case AT_DisableTrig:
			case AT_EnableTrigAll:
			case AT_DisableTrigAll:
			case AT_EnableTrigUser:
			case AT_DisableTrigUser:
				kl_reject("change the triggers of table", relname, kind);
				break;
			case AT_EnableRule:
			case AT_EnableAlwaysRule:
			case AT_EnableReplicaRule:
			case AT_DisableRule:
				kl_reject("change the rules of table", relname, kind);
				break;
			case AT_SetLogged:
			case AT_SetUnLogged:
				kl_reject("change the persistence of table", relname, kind);
				break;
			case AT_AddInherit:
			case AT_DropInherit:
			case AT_AddOf:
			case AT_DropOf:
				kl_reject("change the inheritance or the type of table",
						  relname, kind);
				break;
			default:
				break;
		}
	}
}

/*
 * kl_check_rename
 *		RENAME COLUMN and RENAME of triggers on ledger tables, every rename
 *		of an object of a ledger catalog table, RENAME of the functions and
 *		event triggers of the extension, and RENAME of the schema of the
 *		extension, whose name the functions use.
 */
static void
kl_check_rename(RenameStmt *stmt, Oid extoid)
{
	Oid			relid;
	KlKind		kind;
	LOCKMODE	lockmode = NoLock;

	if (stmt->renameType == OBJECT_SCHEMA)
	{
		if (stmt->subname != NULL &&
			strcmp(stmt->subname, KL_SCHEMA_NAME) == 0)
			kl_reject("rename schema", KL_SCHEMA_NAME, KL_CATALOG_LEDGER);
		return;
	}
	switch (stmt->renameType)
	{
		case OBJECT_FUNCTION:
		case OBJECT_PROCEDURE:
		case OBJECT_ROUTINE:
		case OBJECT_AGGREGATE:
		case OBJECT_EVENT_TRIGGER:
			if (kl_object_is_member(stmt->renameType, stmt->object, extoid))
				kl_reject_object("cannot rename a function or an event trigger of the ledger");
			return;
		case OBJECT_TABLE:
		case OBJECT_COLUMN:
		case OBJECT_TRIGGER:
			lockmode = AccessExclusiveLock;
			break;
		default:
			break;
	}
	if (stmt->relation == NULL)
		return;
	relid = kl_rv_relid(stmt->relation, lockmode);
	kind = kl_classify(relid, extoid);
	if (kind == KL_NOT_LEDGER)
		return;

	switch (stmt->renameType)
	{
		case OBJECT_COLUMN:
			kl_reject("rename a column of table", get_rel_name(relid), kind);
			break;
		case OBJECT_TRIGGER:
			kl_reject("rename a trigger of table", get_rel_name(relid), kind);
			break;
		default:
			/* Renaming a user ledger table or its indexes does not matter. */
			if (kind == KL_CATALOG_LEDGER)
				kl_reject("rename an object of table", get_rel_name(relid), kind);
			break;
	}
}

/*
 * kl_type_used_by_ledger
 *		Whether a column of one of the ledger tables has the type "typid" (or
 *		an array of it), or, when "extid" is valid, a type that belongs to
 *		that extension (or an array of such a type).  Used for DROP TYPE,
 *		DOMAIN and EXTENSION ... CASCADE, which would silently drop the
 *		column, and for ALTER TYPE ... RENAME VALUE, which changes the binary
 *		form of existing enum values.  This is only an early, best effort
 *		check with a precise message: a domain or composite built on the type
 *		is not followed here.  The event trigger, which sees what a statement
 *		really dropped, covers every dependency path.
 */
static bool
kl_type_used_by_ledger(Oid typid, Oid extid, List *relids)
{
	ListCell   *lc;
	Oid			arrid = OidIsValid(typid) ? get_array_type(typid) : InvalidOid;

	foreach(lc, relids)
	{
		Oid			relid = lfirst_oid(lc);
		HeapTuple	reltup = SearchSysCache1(RELOID, ObjectIdGetDatum(relid));
		int			natts;
		int			i;

		if (!HeapTupleIsValid(reltup))
			continue;
		natts = ((Form_pg_class) GETSTRUCT(reltup))->relnatts;
		ReleaseSysCache(reltup);

		for (i = 1; i <= natts; i++)
		{
			Oid			atttype = get_atttype(relid, (AttrNumber) i);
			Oid			elemtype;

			if (!OidIsValid(atttype))
				continue;
			if (OidIsValid(typid) &&
				(atttype == typid || (OidIsValid(arrid) && atttype == arrid)))
				return true;
			if (OidIsValid(extid))
			{
				elemtype = get_element_type(atttype);
				if (getExtensionOfObject(TypeRelationId, atttype) == extid ||
					(OidIsValid(elemtype) &&
					 getExtensionOfObject(TypeRelationId, elemtype) == extid))
					return true;
			}
		}
	}
	return false;
}

/*
 * kl_check_drop
 *		DROP statements that would remove a ledger table, a ledger trigger,
 *		the extension or the schema it lives in while ledger tables exist.
 */
static void
kl_check_drop(DropStmt *stmt, Oid extoid)
{
	ListCell   *lc;
	List	   *ledgers = NIL;
	bool		have_ledgers = false;

	switch (stmt->removeType)
	{
		case OBJECT_TABLE:
		case OBJECT_INDEX:
			foreach(lc, stmt->objects)
			{
				Oid			relid = kl_name_list_relid((List *) lfirst(lc),
													   stmt->removeType == OBJECT_TABLE ?
													   AccessExclusiveLock : NoLock);
				KlKind		kind = kl_classify(relid, extoid);

				if (kind == KL_NOT_LEDGER)
					continue;
				/* Indexes of user ledger tables may be dropped. */
				if (stmt->removeType == OBJECT_INDEX && kind == KL_USER_LEDGER)
					continue;
				kl_reject(stmt->removeType == OBJECT_TABLE ?
						  "drop table" : "drop an index of table",
						  get_rel_name(stmt->removeType == OBJECT_TABLE ?
									   relid : IndexGetRelation(relid, true)),
						  kind);
			}
			break;

		case OBJECT_TRIGGER:
		case OBJECT_RULE:
		case OBJECT_POLICY:
			foreach(lc, stmt->objects)
			{
				List	   *names = (List *) lfirst(lc);
				char	   *objname;
				Oid			relid;
				KlKind		kind;

				if (list_length(names) < 2)
					continue;
				objname = strVal(llast(names));
				relid = kl_name_list_relid(list_copy_head(names,
														  list_length(names) - 1),
										   AccessExclusiveLock);
				kind = kl_classify(relid, extoid);
				if (kind == KL_CATALOG_LEDGER ||
					(kind == KL_USER_LEDGER && stmt->removeType == OBJECT_TRIGGER &&
					 kl_is_ledger_trigger_name(objname)))
					kl_reject("drop a trigger, rule or policy of table",
							  get_rel_name(relid), kind);
			}
			break;

		case OBJECT_SCHEMA:
			ledgers = kl_ledger_relids();
			foreach(lc, stmt->objects)
			{
				Oid			nspoid = get_namespace_oid(strVal(lfirst(lc)), true);
				ListCell   *lr;

				if (!OidIsValid(nspoid))
					continue;
				if (ledgers != NIL &&
					nspoid == get_namespace_oid(KL_SCHEMA_NAME, true))
					kl_reject("drop schema", KL_SCHEMA_NAME, KL_CATALOG_LEDGER);
				foreach(lr, ledgers)
				{
					if (get_rel_namespace(lfirst_oid(lr)) == nspoid)
						kl_reject("drop schema containing table",
								  get_rel_name(lfirst_oid(lr)), KL_USER_LEDGER);
				}
			}
			break;

		case OBJECT_EXTENSION:
			ledgers = kl_ledger_relids();
			foreach(lc, stmt->objects)
			{
				char	   *name = strVal(lfirst(lc));
				Oid			dropped;

				if (strcmp(name, KL_EXTENSION_NAME) == 0 && ledgers != NIL)
					ereport(ERROR,
							(errcode(ERRCODE_KL_PROTECTED),
							 errmsg("cannot drop extension \"%s\" while ledger tables exist",
									KL_EXTENSION_NAME),
							 errhint("Ledger tables are append-only and cannot be removed from the database.")));

				/* An extension that provides the type of a ledger column. */
				if (stmt->behavior == DROP_CASCADE && ledgers != NIL)
				{
					dropped = get_extension_oid(name, true);
					if (OidIsValid(dropped) &&
						kl_type_used_by_ledger(InvalidOid, dropped, ledgers))
						ereport(ERROR,
								(errcode(ERRCODE_KL_PROTECTED),
								 errmsg("cannot drop extension \"%s\" with CASCADE: a ledger table has a column of a type it provides",
										name),
								 errhint("Dropping the extension would drop the column.")));
				}
			}
			break;

		case OBJECT_TYPE:
		case OBJECT_DOMAIN:
			if (stmt->behavior != DROP_CASCADE)
				break;
			ledgers = kl_ledger_relids();
			have_ledgers = (ledgers != NIL);
			if (!have_ledgers)
				break;
			foreach(lc, stmt->objects)
			{
				Oid			typid = LookupTypeNameOid(NULL, lfirst_node(TypeName, lc), true);

				if (OidIsValid(typid) && kl_type_used_by_ledger(typid, InvalidOid, ledgers))
					ereport(ERROR,
							(errcode(ERRCODE_KL_PROTECTED),
							 errmsg("cannot drop type \"%s\" with CASCADE: a ledger table has a column of this type",
									format_type_be(typid)),
							 errhint("Dropping the type would drop the column.")));
			}
			break;

		default:
			break;
	}
}

/*
 * kl_check_create_function
 *		CREATE OR REPLACE FUNCTION on a function of the extension.  Replacing
 *		kingres.ledger_capture would switch capture off without touching a
 *		trigger.  The statement does not say which function it replaces until
 *		the argument types are resolved, so every replacement of a function
 *		with a name of the extension in its schema is rejected; the event
 *		trigger checks, by membership and C symbol, that the trigger
 *		functions are still the ones the extension installed.
 */
static void
kl_check_create_function(CreateFunctionStmt *stmt, Oid extoid)
{
	Oid			nspoid;
	char	   *fname;

	if (!stmt->replace)
		return;
	nspoid = QualifiedNameGetCreationNamespace(stmt->funcname, &fname);
	if (nspoid == get_namespace_oid(KL_SCHEMA_NAME, true) &&
		strncmp(fname, "ledger_", 7) == 0 &&
		OidIsValid(extoid))
		ereport(ERROR,
				(errcode(ERRCODE_KL_PROTECTED),
				 errmsg("cannot replace function \"%s\": it belongs to the ledger", fname)));
}

static void
kl_check_alter_function(AlterFunctionStmt *stmt, Oid extoid)
{
	if (kl_object_is_member(stmt->objtype, (Node *) stmt->func, extoid))
		ereport(ERROR,
				(errcode(ERRCODE_KL_PROTECTED),
				 errmsg("cannot alter a function of the ledger")));
}

/*
 * kl_check_drop_owned
 *		DROP OWNED BY would drop the ledger tables of the listed roles.
 */
static void
kl_check_drop_owned(DropOwnedStmt *stmt)
{
	List	   *ledgers = kl_ledger_relids();
	ListCell   *lr;

	foreach(lr, ledgers)
	{
		Oid			relid = lfirst_oid(lr);
		Oid			owner = InvalidOid;
		HeapTuple	tup = SearchSysCache1(RELOID, ObjectIdGetDatum(relid));
		ListCell   *lc;

		if (!HeapTupleIsValid(tup))
			continue;
		owner = ((Form_pg_class) GETSTRUCT(tup))->relowner;
		ReleaseSysCache(tup);

		foreach(lc, stmt->roles)
		{
			Oid			roleid = get_rolespec_oid(lfirst_node(RoleSpec, lc), true);

			if (OidIsValid(roleid) && roleid == owner)
				kl_reject("drop objects owned by the owner of table",
						  get_rel_name(relid), KL_USER_LEDGER);
		}
	}
}

/*
 * kl_extension_owner
 *		The role that owns the extension, and with it every table and
 *		function of it.
 */
static Oid
kl_extension_owner(Oid extoid)
{
	HeapTuple	tup = SearchSysCache1(EXTENSIONOID, ObjectIdGetDatum(extoid));
	Oid			owner = InvalidOid;

	if (HeapTupleIsValid(tup))
	{
		owner = ((Form_pg_extension) GETSTRUCT(tup))->extowner;
		ReleaseSysCache(tup);
	}
	return owner;
}

/*
 * kl_check_catalog_grant
 *		GRANT and REVOKE on a ledger catalog table.  Readers are granted
 *		SELECT (the README lists the grants of an auditor role); granting any
 *		other privilege would let a role write the ledger, and revoking from
 *		the owner would stop the ledger functions.  Revoking from anybody else
 *		is harmless.
 */
static void
kl_check_catalog_grant(GrantStmt *stmt, const char *relname, Oid extoid)
{
	ListCell   *lc;

	if (stmt->is_grant)
	{
		if (stmt->privileges == NIL)
			kl_reject("grant all privileges on table", relname,
					  KL_CATALOG_LEDGER);
		foreach(lc, stmt->privileges)
		{
			AccessPriv *priv = lfirst_node(AccessPriv, lc);

			if (priv->priv_name == NULL ||
				pg_strcasecmp(priv->priv_name, "select") != 0)
				kl_reject("grant any privilege other than SELECT on table",
						  relname, KL_CATALOG_LEDGER);
		}
	}
	else
	{
		Oid			owner = kl_extension_owner(extoid);

		foreach(lc, stmt->grantees)
		{
			Oid			roleid = get_rolespec_oid(lfirst_node(RoleSpec, lc), true);

			if (OidIsValid(roleid) && roleid == owner)
				kl_reject("revoke privileges of the owner on table", relname,
						  KL_CATALOG_LEDGER);
		}
	}
}

/*
 * kl_check_grant
 *		GRANT and REVOKE on the tables and functions of the extension.
 */
static void
kl_check_grant(GrantStmt *stmt, Oid extoid)
{
	ListCell   *lc;

	if (stmt->targtype == ACL_TARGET_DEFAULTS)
		return;

	switch (stmt->objtype)
	{
		case OBJECT_TABLE:
			foreach(lc, stmt->objects)
			{
				if (stmt->targtype == ACL_TARGET_ALL_IN_SCHEMA)
				{
					if (strcmp(strVal(lfirst(lc)), KL_SCHEMA_NAME) == 0)
						kl_check_catalog_grant(stmt, KL_SCHEMA_NAME, extoid);
				}
				else
				{
					Oid			relid = kl_rv_relid((RangeVar *) lfirst(lc), NoLock);

					if (kl_classify(relid, extoid) == KL_CATALOG_LEDGER)
						kl_check_catalog_grant(stmt, get_rel_name(relid), extoid);
				}
			}
			break;

		case OBJECT_FUNCTION:
		case OBJECT_PROCEDURE:
		case OBJECT_ROUTINE:
			foreach(lc, stmt->objects)
			{
				if (stmt->targtype == ACL_TARGET_ALL_IN_SCHEMA)
				{
					if (strcmp(strVal(lfirst(lc)), KL_SCHEMA_NAME) == 0)
						ereport(ERROR,
								(errcode(ERRCODE_KL_PROTECTED),
								 errmsg("cannot grant or revoke privileges on all functions of schema \"%s\"",
										KL_SCHEMA_NAME),
								 errhint("Some functions of the ledger must never be callable by anybody else; grant EXECUTE on the public functions one by one.")));
				}
				else
				{
					Oid			funcoid = LookupFuncWithArgs(stmt->objtype,
															 lfirst_node(ObjectWithArgs, lc),
															 true);

					if (OidIsValid(funcoid) && kl_is_internal_function(funcoid, extoid))
						kl_reject_object("cannot grant or revoke privileges on an internal function of the ledger");
				}
			}
			break;

		default:
			break;
	}
}

/*
 * kl_check_statement
 *		Dispatch on the statement type.  Unlisted statements are not
 *		inspected and cost nothing.
 */
static void
kl_check_statement(Node *parsetree)
{
	Oid			extoid;
	Oid			relid;
	KlKind		kind;

	if (parsetree == NULL)
		return;

	switch (nodeTag(parsetree))
	{
		case T_AlterTableStmt:
		case T_RenameStmt:
		case T_AlterObjectSchemaStmt:
		case T_AlterOwnerStmt:
		case T_AlterEventTrigStmt:
		case T_GrantStmt:
		case T_DropStmt:
		case T_CreateTrigStmt:
		case T_IndexStmt:
		case T_RuleStmt:
		case T_CreatePolicyStmt:
		case T_AlterPolicyStmt:
		case T_CreateStmt:
		case T_CreateForeignTableStmt:
		case T_DropOwnedStmt:
		case T_AlterExtensionContentsStmt:
		case T_CreateFunctionStmt:
		case T_AlterFunctionStmt:
		case T_AlterEnumStmt:
			break;
		default:
			return;
	}

	/* Nothing to protect where the extension is not installed. */
	extoid = get_extension_oid(KL_EXTENSION_NAME, true);
	if (!OidIsValid(extoid))
		return;
	/* The extension's own install and upgrade scripts are trusted. */
	if (creating_extension && CurrentExtensionObject == extoid)
		return;

	switch (nodeTag(parsetree))
	{
		case T_AlterTableStmt:
			kl_check_alter_table((AlterTableStmt *) parsetree, extoid);
			break;

		case T_RenameStmt:
			kl_check_rename((RenameStmt *) parsetree, extoid);
			break;

		case T_AlterObjectSchemaStmt:
			{
				AlterObjectSchemaStmt *stmt = (AlterObjectSchemaStmt *) parsetree;

				if (stmt->objectType == OBJECT_EXTENSION &&
					stmt->object != NULL &&
					strcmp(strVal(stmt->object), KL_EXTENSION_NAME) == 0)
					ereport(ERROR,
							(errcode(ERRCODE_KL_PROTECTED),
							 errmsg("cannot move extension \"%s\" to another schema",
									KL_EXTENSION_NAME)));
				if (stmt->relation != NULL)
				{
					relid = kl_rv_relid(stmt->relation,
										stmt->objectType == OBJECT_TABLE ?
										AccessExclusiveLock : NoLock);
					kind = kl_classify(relid, extoid);
					if (kind != KL_NOT_LEDGER)
						kl_reject("move to another schema table",
								  get_rel_name(relid), kind);
				}
				else if (stmt->object != NULL &&
						 (stmt->objectType == OBJECT_FUNCTION ||
						  stmt->objectType == OBJECT_PROCEDURE ||
						  stmt->objectType == OBJECT_ROUTINE ||
						  stmt->objectType == OBJECT_AGGREGATE))
				{
					if (kl_object_is_member(stmt->objectType, stmt->object, extoid))
						ereport(ERROR,
								(errcode(ERRCODE_KL_PROTECTED),
								 errmsg("cannot move a function of the ledger to another schema")));
				}
			}
			break;

		case T_AlterOwnerStmt:
			{
				AlterOwnerStmt *stmt = (AlterOwnerStmt *) parsetree;

				switch (stmt->objectType)
				{
					case OBJECT_FUNCTION:
					case OBJECT_PROCEDURE:
					case OBJECT_ROUTINE:
					case OBJECT_AGGREGATE:
					case OBJECT_EVENT_TRIGGER:
						if (kl_object_is_member(stmt->objectType, stmt->object, extoid))
							kl_reject_object("cannot change the owner of a function or an event trigger of the ledger");
						break;
					default:
						break;
				}
			}
			break;

		case T_AlterEventTrigStmt:
			{
				AlterEventTrigStmt *stmt = (AlterEventTrigStmt *) parsetree;
				Oid			evtoid = get_event_trigger_oid(stmt->trigname, true);

				if (OidIsValid(evtoid) &&
					getExtensionOfObject(EventTriggerRelationId, evtoid) == extoid)
					kl_reject_object("cannot enable or disable an event trigger of the ledger");
			}
			break;

		case T_GrantStmt:
			kl_check_grant((GrantStmt *) parsetree, extoid);
			break;

		case T_DropStmt:
			kl_check_drop((DropStmt *) parsetree, extoid);
			break;

		case T_CreateTrigStmt:
			{
				CreateTrigStmt *stmt = (CreateTrigStmt *) parsetree;

				relid = kl_rv_relid(stmt->relation, ShareRowExclusiveLock);
				kind = kl_classify(relid, extoid);
				if (kind == KL_CATALOG_LEDGER ||
					(kind == KL_USER_LEDGER &&
					 (stmt->replace || kl_is_ledger_trigger_name(stmt->trigname))))
					kl_reject("create a trigger on table", get_rel_name(relid), kind);
			}
			break;

		case T_IndexStmt:
			relid = kl_rv_relid(((IndexStmt *) parsetree)->relation, NoLock);
			kind = kl_classify(relid, extoid);
			if (kind == KL_CATALOG_LEDGER)
				kl_reject("create an index on table", get_rel_name(relid), kind);
			break;

		case T_RuleStmt:
			relid = kl_rv_relid(((RuleStmt *) parsetree)->relation, NoLock);
			kind = kl_classify(relid, extoid);
			if (kind != KL_NOT_LEDGER)
				kl_reject("create a rule on table", get_rel_name(relid), kind);
			break;

		case T_CreatePolicyStmt:
			relid = kl_rv_relid(((CreatePolicyStmt *) parsetree)->table, NoLock);
			kind = kl_classify(relid, extoid);
			if (kind == KL_CATALOG_LEDGER)
				kl_reject("create a policy on table", get_rel_name(relid), kind);
			break;

		case T_AlterPolicyStmt:
			relid = kl_rv_relid(((AlterPolicyStmt *) parsetree)->table, NoLock);
			kind = kl_classify(relid, extoid);
			if (kind == KL_CATALOG_LEDGER)
				kl_reject("alter a policy on table", get_rel_name(relid), kind);
			break;

		case T_CreateStmt:
			kl_check_inherit_parents(((CreateStmt *) parsetree)->inhRelations,
									 extoid);
			break;

		case T_CreateForeignTableStmt:
			kl_check_inherit_parents(((CreateForeignTableStmt *) parsetree)->base.inhRelations,
									 extoid);
			break;

		case T_DropOwnedStmt:
			kl_check_drop_owned((DropOwnedStmt *) parsetree);
			break;

		case T_AlterExtensionContentsStmt:
			if (strcmp(((AlterExtensionContentsStmt *) parsetree)->extname,
					   KL_EXTENSION_NAME) == 0)
				ereport(ERROR,
						(errcode(ERRCODE_KL_PROTECTED),
						 errmsg("cannot change the members of extension \"%s\"",
								KL_EXTENSION_NAME)));
			break;

		case T_CreateFunctionStmt:
			kl_check_create_function((CreateFunctionStmt *) parsetree, extoid);
			break;

		case T_AlterFunctionStmt:
			kl_check_alter_function((AlterFunctionStmt *) parsetree, extoid);
			break;

		case T_AlterEnumStmt:
			{
				AlterEnumStmt *stmt = (AlterEnumStmt *) parsetree;
				Oid			typid;

				/* ADD VALUE is harmless; RENAME VALUE changes the hashed label. */
				if (stmt->oldVal == NULL)
					break;
				typid = LookupTypeNameOid(NULL,
										  makeTypeNameFromNameList(stmt->typeName),
										  true);
				if (OidIsValid(typid) &&
					kl_type_used_by_ledger(typid, InvalidOid, kl_ledger_relids()))
					ereport(ERROR,
							(errcode(ERRCODE_KL_PROTECTED),
							 errmsg("cannot rename a value of enum type \"%s\": a ledger table has a column of this type",
									format_type_be(typid)),
							 errhint("The binary form of an enum value is its label, which is part of the row hash.")));
			}
			break;

		default:
			break;
	}
}

/*
 * kl_ProcessUtility
 *		The hook: check the statement, then run it through the next hook or
 *		the standard implementation.
 */
static void
kl_ProcessUtility(PlannedStmt *pstmt, const char *queryString,
				  bool readOnlyTree, ProcessUtilityContext context,
				  ParamListInfo params, QueryEnvironment *queryEnv,
				  DestReceiver *dest, QueryCompletion *qc)
{
	kl_check_statement(pstmt->utilityStmt);

	if (prev_ProcessUtility)
		prev_ProcessUtility(pstmt, queryString, readOnlyTree, context, params,
							queryEnv, dest, qc);
	else
		standard_ProcessUtility(pstmt, queryString, readOnlyTree, context,
								params, queryEnv, dest, qc);
}
