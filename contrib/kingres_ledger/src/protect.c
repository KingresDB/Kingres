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
 *		the functions and the event triggers of the extension, and dropping,
 *		replacing or altering the status view of the extension;
 *	  - the history table of an updatable ledger table (registered as
 *		history_relid in kingres.ledger_table) is treated like a table of the
 *		ledger catalog, without exception: the ADD COLUMN that follows an ADD
 *		COLUMN of the ledger table is made by kingres.ledger_history_sync
 *		through the catalog API (history.c), not as a statement, so no switch
 *		exists that would let a statement through; every GRANT on it is
 *		rejected (its privileges follow the owner of the ledger table), and
 *		ADD COLUMN of a column with the name of a history column is rejected.
 *
 *	  - the second invariant (README, "What a role that is not a superuser can
 *		stop or remove"): LOCK of a ledger catalog table or of a view that
 *		reaches one, TRUNCATE of one, a foreign key that references one, ALTER
 *		... DEPENDS ON EXTENSION of a ledger object, and, for a role that is not
 *		a superuser, GRANT and REVOKE on the schema of the extension, ALTER
 *		SCHEMA ... OWNER TO and ALTER SEQUENCE of its sequences.  A ledger
 *		catalog table is never locked by this hook, whoever owns it: the request
 *		for ACCESS EXCLUSIVE of a statement that is going to be rejected anyway
 *		would queue behind any reader (pg_dump, ledger_verify), and every
 *		ROW EXCLUSIVE request after it (the builder, the capture) would wait
 *		behind the queued one.
 *
 * A second hook, object_access_hook (OAT_DROP, kl_object_access), is called for
 * every object that any DROP removes, through any dependency; while tables are
 * under ledger control it rejects the drop of anything that belongs to the
 * ledger, which a statement hook cannot do for a cascade.
 *
 * The hook has no "inside our own code" state that relaxes a check, with one
 * exception that is scoped to the statement text: the statements of the
 * extension's own install or upgrade script (see kl_is_own_script).
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
 * SPDX-License-Identifier: AGPL-3.0-only
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
#include "catalog/objectaccess.h"
#include "catalog/objectaddress.h"
#include "catalog/pg_class.h"
#include "catalog/pg_extension.h"
#include "catalog/pg_language.h"
#include "catalog/pg_namespace.h"
#include "catalog/pg_proc.h"
#include "catalog/pg_trigger.h"
#include "catalog/pg_type.h"
#include "commands/defrem.h"
#include "commands/event_trigger.h"
#include "commands/extension.h"
#include "commands/tablecmds.h"
#include "commands/trigger.h"
#include "miscadmin.h"
#include "nodes/makefuncs.h"
#include "nodes/nodeFuncs.h"
#include "nodes/parsenodes.h"
#include "parser/parse_func.h"
#include "parser/parse_type.h"
#include "rewrite/rewriteHandler.h"
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
#include "row_hash.h"

/* What kind of protected relation a relation is. */
typedef enum KlKind
{
	KL_NOT_LEDGER,				/* not protected */
	KL_USER_LEDGER,				/* a table under ledger control */
	KL_CATALOG_LEDGER			/* a kingres.ledger_* table of the extension */
} KlKind;

static ProcessUtility_hook_type prev_ProcessUtility = NULL;
static object_access_hook_type prev_object_access_hook = NULL;

static void kl_ProcessUtility(PlannedStmt *pstmt, const char *queryString,
							  bool readOnlyTree, ProcessUtilityContext context,
							  ParamListInfo params, QueryEnvironment *queryEnv,
							  DestReceiver *dest, QueryCompletion *qc);
static void kl_object_access(ObjectAccessType access, Oid classId,
							 Oid objectId, int subId, void *arg);
static void kl_check_fk_elements(List *elements, Oid extoid);

/*
 * kl_protect_init
 *		Install the hooks, chaining to previously installed ones.  Called
 *		from _PG_init when the library is preloaded.
 */
void
kl_protect_init(void)
{
	prev_ProcessUtility = ProcessUtility_hook;
	ProcessUtility_hook = kl_ProcessUtility;
	prev_object_access_hook = object_access_hook;
	object_access_hook = kl_object_access;
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
 * kl_reject_public
 *		The refusal of a REVOKE of the privileges of PUBLIC on a table of the
 *		catalog: the hint says why (the one of kl_reject is about switching the
 *		ledger off, which this is not; L15-10).
 */
static void
kl_reject_public(const char *name)
{
	ereport(ERROR,
			(errcode(ERRCODE_KL_PROTECTED),
			 errmsg("cannot revoke the privileges of PUBLIC on table \"%s\": it is part of the ledger", name),
			 errhint("The read access that PUBLIC has on the catalog tables is withdrawn by a superuser; a role that is not a superuser, a member of the owner role included, cannot.")));
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
		kl_is_ledger_function(funcoid, extoid, KL_SYM_REGISTER_TABLE) ||
		kl_is_ledger_function(funcoid, extoid, KL_SYM_RECORD_ROW) ||
		kl_is_ledger_function(funcoid, extoid, KL_SYM_LOCK_TABLE) ||
		kl_is_ledger_function(funcoid, extoid, KL_SYM_HISTORY_SYNC) ||
		kl_is_ledger_function(funcoid, extoid, KL_SYM_HISTORY_ACL_SYNC);
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
 * kl_is_history
 *		Whether the table is the history table of an updatable ledger table,
 *		that is, whether kingres.ledger_table names it as history_relid.  Like
 *		kl_is_registered it uses the latest snapshot.  The history table is part
 *		of the ledger: it is treated like a table of the ledger catalog.  In a
 *		database that still has version 0.1 of the extension there is no such
 *		column and no table is a history table.
 */
static bool
kl_is_history(Oid relid)
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
	idxoid = get_relname_relid("ledger_table_history_relid_key", nspoid);
	if (!OidIsValid(tableoid) || !OidIsValid(idxoid))
		return false;

	rel = table_open(tableoid, AccessShareLock);
	snap = RegisterSnapshot(GetLatestSnapshot());
	ScanKeyInit(&key, KL_LEDGER_TABLE_HISTORY_ATTNO, BTEqualStrategyNumber,
				F_OIDEQ, ObjectIdGetDatum(relid));
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
	/*
	 * The status view and the sequences of the extension are part of the
	 * ledger too (the registry's sequence issues the identity of every table).
	 */
	if (relkind == RELKIND_VIEW || relkind == RELKIND_SEQUENCE)
		return getExtensionOfObject(RelationRelationId, relid) == extoid ?
			KL_CATALOG_LEDGER : KL_NOT_LEDGER;
	if (relkind != RELKIND_RELATION)
		return KL_NOT_LEDGER;
	if (getExtensionOfObject(RelationRelationId, relid) == extoid ||
		kl_is_history(relid))
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
 *
 *		A table of the ledger catalog is never locked either, whoever owns it:
 *		the statement is rejected whatever it is, and the request for a lock
 *		made here (the owner of the extension, and every role that inherits
 *		from it, owns the tables) waits in the queue of the table behind
 *		whoever reads it (pg_dump, ledger_verify), and everybody who asks for
 *		ROW EXCLUSIVE after it (the block builder, every capture trigger) waits
 *		behind the request for as long as the reader keeps its lock (the second
 *		invariant, README "What a role that is not a superuser can stop or
 *		remove").  A statement that fails releases its locks at once, so
 *		nothing is kept; the harm is the wait.  The names of these tables cannot move (renaming them,
 *		replacing them and moving them are rejected), so nothing is lost.  If
 *		the name moved to a ledger catalog table while the lock was
 *		requested, the lock is given back.
 */
static Oid
kl_rv_relid(const RangeVar *rv, LOCKMODE lockmode)
{
	Oid			relid;
	Oid			locked;
	Oid			extoid;

	if (rv == NULL)
		return InvalidOid;
	relid = RangeVarGetRelid(rv, NoLock, true);
	if (lockmode == NoLock || !OidIsValid(relid))
		return relid;
	extoid = kl_extension_oid();
	if (kl_classify(relid, extoid) == KL_CATALOG_LEDGER)
		return relid;
	if (!object_ownercheck(RelationRelationId, relid, GetUserId()))
		return relid;
	locked = RangeVarGetRelidExtended(rv, lockmode, RVR_MISSING_OK, NULL, NULL);
	if (locked != relid && OidIsValid(locked) &&
		kl_classify(locked, extoid) == KL_CATALOG_LEDGER)
		UnlockRelationOid(locked, lockmode);
	return locked;
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
		/* The history table of an updatable table is dropped with it. */
		if (HeapTupleHeaderGetNatts(tup->t_data) >= KL_LEDGER_TABLE_HISTORY_ATTNO)
		{
			d = heap_getattr(tup, KL_LEDGER_TABLE_HISTORY_ATTNO,
							 RelationGetDescr(rel), &isnull);
			if (!isnull)
				result = lappend_oid(result, DatumGetObjectId(d));
		}
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

	if (col->colname != NULL && kl_is_history_column(col->colname))
		kl_reject("add a column named like a column of the history table to table",
				  relname, kind);
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

		if ((cmd->subtype == AT_AddConstraint || cmd->subtype == AT_AddColumn) &&
			cmd->def != NULL)
		{
			List	   *one = list_make1(cmd->def);

			kl_check_fk_elements(one, extoid);
			list_free(one);
		}

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
		case OBJECT_VIEW:
		case OBJECT_INDEX:
			foreach(lc, stmt->objects)
			{
				Oid			relid = kl_name_list_relid((List *) lfirst(lc),
													   stmt->removeType != OBJECT_INDEX ?
													   AccessExclusiveLock : NoLock);
				KlKind		kind = kl_classify(relid, extoid);

				if (kind == KL_NOT_LEDGER)
					continue;
				/* Indexes of user ledger tables may be dropped. */
				if (stmt->removeType == OBJECT_INDEX && kind == KL_USER_LEDGER)
					continue;
				kl_reject(stmt->removeType != OBJECT_INDEX ?
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
kl_check_catalog_grant(GrantStmt *stmt, const char *relname, Oid extoid,
					   bool history)
{
	ListCell   *lc;

	if (stmt->is_grant)
	{
		/*
		 * The history table holds old row versions, which bypass the row
		 * security and the column privileges of the table: who may read it
		 * is decided by the ownership of the ledger table alone (history.c),
		 * not by a grant.
		 */
		if (history)
			ereport(ERROR,
					(errcode(ERRCODE_KL_PROTECTED),
					 errmsg("cannot grant privileges on table \"%s\": it is a history table of the ledger",
							relname),
					 errhint("Only the owner of the ledger table can read its history table; make a role a member of that owner to give it access.")));
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
			RoleSpec   *spec = lfirst_node(RoleSpec, lc);
			Oid			roleid;

			/*
			 * PUBLIC is not a role and never the owner.  get_rolespec_oid
			 * raises "role "public" does not exist" for it, which made
			 * REVOKE ... FROM PUBLIC fail with a message that blames the
			 * statement.  The read access of PUBLIC to the catalog tables
			 * (the registered tables, the identity of the chain, the block
			 * headers) is the administrator's to withdraw: a role that is
			 * not a superuser (a member of the owner role) could take it
			 * from every application role, and the publication of the chain
			 * by plain roles would stop (L13-06; the same class as the
			 * USAGE of the schema, L11-05).  The functions that publish the
			 * chain run with the rights of the extension owner and need only
			 * EXECUTE, and the builder and the capture do not read with the
			 * rights of PUBLIC at all.  A history table has no grants for
			 * PUBLIC to lose.
			 */
			if (spec->roletype == ROLESPEC_PUBLIC)
			{
				if (!history && !superuser())
					kl_reject_public(relname);
				continue;
			}
			roleid = get_rolespec_oid(spec, true);
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
		case OBJECT_SCHEMA:
			/*
			 * The privileges on the schema of the extension are the
			 * administrator's: a role that is not a superuser (a member of the
			 * owner role) could take USAGE from PUBLIC and every role would
			 * lose the functions.
			 */
			if (stmt->targtype == ACL_TARGET_OBJECT && !superuser())
				foreach(lc, stmt->objects)
				{
					if (IsA(lfirst(lc), String) &&
						strcmp(strVal(lfirst(lc)), KL_SCHEMA_NAME) == 0)
						kl_reject("change the privileges on schema", KL_SCHEMA_NAME,
								  KL_CATALOG_LEDGER);
				}
			break;

		case OBJECT_SEQUENCE:
			/* The sequences of the extension are read-only for everybody else. */
			foreach(lc, stmt->objects)
			{
				if (stmt->targtype == ACL_TARGET_ALL_IN_SCHEMA)
				{
					if (strcmp(strVal(lfirst(lc)), KL_SCHEMA_NAME) == 0)
						kl_check_catalog_grant(stmt, KL_SCHEMA_NAME, extoid, false);
				}
				else
				{
					Oid			relid = kl_rv_relid((RangeVar *) lfirst(lc), NoLock);

					if (kl_classify(relid, extoid) == KL_CATALOG_LEDGER)
						kl_check_catalog_grant(stmt, get_rel_name(relid), extoid,
											   false);
				}
			}
			break;

		case OBJECT_TABLE:
			foreach(lc, stmt->objects)
			{
				if (stmt->targtype == ACL_TARGET_ALL_IN_SCHEMA)
				{
					if (strcmp(strVal(lfirst(lc)), KL_SCHEMA_NAME) == 0)
						kl_check_catalog_grant(stmt, KL_SCHEMA_NAME, extoid, false);
				}
				else
				{
					Oid			relid = kl_rv_relid((RangeVar *) lfirst(lc), NoLock);

					if (kl_classify(relid, extoid) == KL_CATALOG_LEDGER)
						kl_check_catalog_grant(stmt, get_rel_name(relid), extoid,
											   kl_is_history(relid));
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

					/*
					 * What PUBLIC may execute (the hash primitives, ledger_row_hash,
					 * ledger_verify_proof, ledger_block_preimage) is for every role
					 * to publish and check proofs with; taking it away is the
					 * administrator's, like the SELECT of PUBLIC on the catalog
					 * tables (L15-10).  REVOKE from any other role is harmless.
					 */
					if (OidIsValid(funcoid) && !stmt->is_grant && !superuser() &&
						getExtensionOfObject(ProcedureRelationId, funcoid) == extoid)
					{
						ListCell   *gc;

						foreach(gc, stmt->grantees)
						{
							if (lfirst_node(RoleSpec, gc)->roletype == ROLESPEC_PUBLIC)
								ereport(ERROR,
										(errcode(ERRCODE_KL_PROTECTED),
										 errmsg("cannot revoke the privileges of PUBLIC on a function of the ledger"),
										 errhint("The right to execute that PUBLIC has on the functions of the ledger is withdrawn by a superuser; a role that is not a superuser, a member of the owner role included, cannot.")));
						}
					}
				}
			}
			break;

		default:
			break;
	}
}

/*
 * The query text of the script of this extension that is being run by
 * CREATE EXTENSION or ALTER EXTENSION UPDATE, or NULL.  execute_sql_string
 * passes the text of the whole script to ProcessUtility for each of its
 * statements, so the statements of the script share one pointer, and a
 * statement that anything else runs meanwhile (an event trigger, a function)
 * has a text of its own.  The first statement that the hook sees while the
 * script runs is a statement of the script; see kl_note_script.
 */
static const char *kl_script_query = NULL;

/*
 * kl_note_script
 *		Called for every utility statement, before it is checked.  Remembers
 *		the text of the script of this extension at its first statement, and
 *		forgets it as soon as no script of this extension is running.
 *		A script that is followed by another one in the same command (a chain
 *		of upgrade steps) would need this to learn the text of the next one;
 *		today there is a single step, and a second one would be rejected
 *		rather than trusted.
 */
static void
kl_note_script(const char *queryString)
{
	if (!creating_extension)
	{
		kl_script_query = NULL;
		return;
	}
	if (kl_script_query == NULL &&
		CurrentExtensionObject == get_extension_oid(KL_EXTENSION_NAME, true))
		kl_script_query = queryString;
}

/*
 * kl_is_own_script
 *		Whether the statement is a statement of the install or upgrade script
 *		of the extension, which has to be able to do what the hook rejects
 *		everywhere else.  Nothing else is trusted while the script runs: not a
 *		statement of an event trigger, not one of a function.
 */
static bool
kl_is_own_script(Oid extoid, const char *queryString)
{
	return creating_extension && CurrentExtensionObject == extoid &&
		kl_script_query != NULL && queryString == kl_script_query;
}

/*
 * kl_check_fk_constraint, kl_check_fk_elements
 *		A foreign key that references a table of the ledger catalog.  Creating
 *		one takes SHARE ROW EXCLUSIVE on the referenced table (the validation
 *		of the existing rows) until the transaction ends, which stops every
 *		capture trigger; once it exists, its system triggers run on every
 *		insert, update and delete of the catalog table (the block builder's
 *		DELETE of the queue) and every row of the referencing table that
 *		points at a row of the ledger holds a lock on it.  Only a role that
 *		holds REFERENCES on a catalog table can do it: the owner of the
 *		extension and the roles that inherit its privileges.  Rejected for
 *		every role, in every form: the column constraint of CREATE TABLE and
 *		of ADD COLUMN, the table constraint of both, and ADD CONSTRAINT.
 */
static void
kl_check_fk_constraint(Constraint *con, Oid extoid)
{
	Oid			relid;

	if (con->contype != CONSTR_FOREIGN || con->pktable == NULL)
		return;
	relid = RangeVarGetRelid(con->pktable, NoLock, true);
	if (OidIsValid(relid) && kl_classify(relid, extoid) == KL_CATALOG_LEDGER)
		kl_reject("create a foreign key that references table", get_rel_name(relid),
				  KL_CATALOG_LEDGER);
}

static void
kl_check_fk_elements(List *elements, Oid extoid)
{
	ListCell   *lc;

	foreach(lc, elements)
	{
		Node	   *n = (Node *) lfirst(lc);

		if (IsA(n, Constraint))
			kl_check_fk_constraint((Constraint *) n, extoid);
		else if (IsA(n, ColumnDef))
		{
			ListCell   *lcc;

			foreach(lcc, ((ColumnDef *) n)->constraints)
			{
				Node	   *c = (Node *) lfirst(lcc);

				if (IsA(c, Constraint))
					kl_check_fk_constraint((Constraint *) c, extoid);
			}
		}
	}
}

/*
 * kl_lock_walker, kl_lock_reach
 *		Whether a view (or a view over a view, with its sublinks and
 *		subqueries) reaches a table of the ledger catalog: the tables that LOCK
 *		TABLE of a view locks are the ones in the stored query of the view, with
 *		the privileges of the view's owner (LockViewRecurse in lockcmds.c).  The
 *		views are locked ACCESS SHARE while they are read, so that the owner of a
 *		view cannot redefine it between this check and the LOCK that follows.
 */
typedef struct KlLockWalk
{
	Oid			extoid;
	List	   *views;			/* views already visited */
	Oid			hit;			/* the catalog table that was reached */
} KlLockWalk;

static bool kl_lock_walker(Node *node, KlLockWalk *ctx);

static void
kl_lock_reach(Oid relid, KlLockWalk *ctx)
{
	if (kl_classify(relid, ctx->extoid) == KL_CATALOG_LEDGER)
	{
		ctx->hit = relid;
		return;
	}
	if (get_rel_relkind(relid) == RELKIND_VIEW &&
		!list_member_oid(ctx->views, relid))
	{
		Relation	rel;

		ctx->views = lappend_oid(ctx->views, relid);
		rel = relation_open(relid, AccessShareLock);
		(void) kl_lock_walker((Node *) get_view_query(rel), ctx);
		relation_close(rel, NoLock);
	}
}

static bool
kl_lock_walker(Node *node, KlLockWalk *ctx)
{
	if (node == NULL)
		return false;
	if (IsA(node, Query))
	{
		Query	   *query = (Query *) node;
		ListCell   *lc;

		foreach(lc, query->rtable)
		{
			RangeTblEntry *rte = lfirst_node(RangeTblEntry, lc);

			if (rte->rtekind != RTE_RELATION)
				continue;
			kl_lock_reach(rte->relid, ctx);
			if (OidIsValid(ctx->hit))
				return true;
		}
		return query_tree_walker(query, kl_lock_walker, ctx,
								 QTW_IGNORE_JOINALIASES);
	}
	return expression_tree_walker(node, kl_lock_walker, ctx);
}

/*
 * kl_check_lock
 *		LOCK TABLE above ACCESS SHARE by a role that is not a superuser.  pg_maintain
 *		may take any lock on any table, ACCESS EXCLUSIVE included, and a role that
 *		may write every table may take ROW SHARE and ROW EXCLUSIVE; a lock that is
 *		held until the transaction ends makes the capture triggers (ROW EXCLUSIVE
 *		on the queue, the entries and the registry) and the block builder wait.
 *		The builder takes no table lock of its own (see write.c), so what conflicts
 *		with it is whatever conflicts with the writes, and every mode above ACCESS
 *		SHARE is refused.  The ledger's own code takes no lock with a LOCK
 *		statement, and a superuser can stop the server anyway.
 *
 *		The refusal is about the relation that is locked, not the one that the
 *		statement names: LOCK TABLE of a view locks the tables of its query, so a
 *		view is followed, and a view over a view, to the end (L11-01).  Inheritance
 *		and partitioning cannot lead to a table of the ledger (it can have no
 *		children and cannot be one), and ONLY changes nothing for the same reason.
 */
static void
kl_check_lock(LockStmt *stmt, Oid extoid)
{
	ListCell   *rl;

	if (stmt->mode <= AccessShareLock || superuser())
		return;
	foreach(rl, stmt->relations)
	{
		Oid			relid = RangeVarGetRelid(lfirst_node(RangeVar, rl), NoLock, true);

		if (!OidIsValid(relid))
			continue;
		if (kl_classify(relid, extoid) == KL_CATALOG_LEDGER)
		{
			/*
			 * Only a role that the lock would be allowed to: any other gets
			 * the ordinary "permission denied" from the command itself.
			 * pg_maintain and pg_write_all_data are such roles.
			 */
			if (pg_class_aclmask(relid, GetUserId(),
								 ACL_UPDATE | ACL_DELETE | ACL_TRUNCATE | ACL_MAINTAIN,
								 ACLMASK_ANY) != 0)
				kl_reject("lock with a mode that blocks readers or writers table",
						  get_rel_name(relid), KL_CATALOG_LEDGER);
		}
		else if (get_rel_relkind(relid) == RELKIND_VIEW)
		{
			KlLockWalk	ctx;

			ctx.extoid = extoid;
			ctx.views = NIL;
			ctx.hit = InvalidOid;
			kl_lock_reach(relid, &ctx);
			if (OidIsValid(ctx.hit))
				kl_reject("lock with a mode that blocks readers or writers a view over table",
						  get_rel_name(ctx.hit), KL_CATALOG_LEDGER);
		}
	}
}

/*
 * kl_check_truncate
 *		TRUNCATE of a table of the ledger catalog by a role that is not a
 *		superuser.  The BEFORE TRUNCATE trigger refuses it for every role, but
 *		only after the table has been locked
 *		ACCESS EXCLUSIVE, together with every table that references it, and a
 *		role that has the TRUNCATE privilege (the owner of the extension and the
 *		roles that inherit from it) would queue for that lock behind any reader,
 *		with every writer behind it.  So it is refused before anything is locked,
 *		with the same message.
 */
static void
kl_check_truncate(TruncateStmt *stmt, Oid extoid)
{
	ListCell   *lc;

	/* A superuser is stopped by the trigger, as it always was. */
	if (superuser())
		return;
	foreach(lc, stmt->relations)
	{
		Oid			relid = RangeVarGetRelid(lfirst_node(RangeVar, lc), NoLock, true);

		if (OidIsValid(relid) && kl_classify(relid, extoid) == KL_CATALOG_LEDGER)
			ereport(ERROR,
					(errcode(ERRCODE_KL_APPEND_ONLY),
					 errmsg("table \"%s\" is an append-only ledger table",
							get_rel_name(relid))));
	}
}

/*
 * kl_check_depends
 *		ALTER ... DEPENDS ON EXTENSION of an object of the ledger would make the
 *		object a dependent of another extension, and DROP EXTENSION of that one
 *		would drop it (a capture trigger, for one).  Rejected for the functions
 *		and event triggers of the extension, for the ledger triggers of every
 *		ledger table and for the indexes of the ledger catalog.
 */
static void
kl_check_depends(AlterObjectDependsStmt *stmt, Oid extoid)
{
	if (stmt->relation != NULL)
	{
		Oid			relid = kl_rv_relid(stmt->relation, NoLock);
		KlKind		kind = kl_classify(relid, extoid);

		if (kind == KL_NOT_LEDGER)
			return;
		if (stmt->objectType == OBJECT_TRIGGER)
		{
			const char *trigname = (stmt->object != NULL && IsA(stmt->object, List)) ?
				strVal(linitial((List *) stmt->object)) : NULL;

			if (kind == KL_CATALOG_LEDGER || kl_is_ledger_trigger_name(trigname))
				kl_reject("change the dependencies of a trigger of table",
						  get_rel_name(relid), kind);
		}
		else if (kind == KL_CATALOG_LEDGER)
			kl_reject("change the dependencies of an object of table",
					  get_rel_name(relid), kind);
		return;
	}
	if (stmt->object != NULL &&
		(stmt->objectType == OBJECT_FUNCTION ||
		 stmt->objectType == OBJECT_PROCEDURE ||
		 stmt->objectType == OBJECT_ROUTINE ||
		 stmt->objectType == OBJECT_AGGREGATE) &&
		kl_object_is_member(stmt->objectType, stmt->object, extoid))
		kl_reject_object("cannot make a function of the ledger depend on an extension");
}

/*
 * kl_check_maintenance
 *		CLUSTER and REINDEX (not CONCURRENTLY) of a ledger catalog table by a
 *		role that is not a superuser (pg_maintain).  They lock the table in a
 *		mode that conflicts with the writes of the capture triggers and of the
 *		block builder (SHARE, ACCESS EXCLUSIVE), and inside a transaction block
 *		or a function the lock is held until that transaction ends, for as long
 *		as the role likes.  They are allowed as a statement of their own, which
 *		holds the lock for the time the statement takes and no longer.  VACUUM
 *		cannot run in a transaction block, so VACUUM FULL needs no check.
 */
static void
kl_check_maintenance(Node *parsetree, bool is_top_level, Oid extoid)
{
	RangeVar   *rv;
	Oid			relid;
	const char *what;
	bool		is_index = false;

	if (superuser())
		return;
	if (nodeTag(parsetree) == KL_T_CLUSTER_STMT)
	{
		rv = kl_cluster_stmt_relation(parsetree);
		what = "CLUSTER of a ledger catalog table";
	}
	else
	{
		ReindexStmt *stmt = (ReindexStmt *) parsetree;
		ListCell   *lc;

		if (stmt->kind != REINDEX_OBJECT_TABLE && stmt->kind != REINDEX_OBJECT_INDEX)
			return;
		foreach(lc, stmt->params)
		{
			DefElem    *opt = lfirst_node(DefElem, lc);

			if (strcmp(opt->defname, "concurrently") == 0 && defGetBoolean(opt))
				return;
		}
		rv = stmt->relation;
		is_index = (stmt->kind == REINDEX_OBJECT_INDEX);
		what = "REINDEX of a ledger catalog table";
	}
	if (rv == NULL)
		return;
	relid = kl_rv_relid(rv, NoLock);
	if (is_index && OidIsValid(relid))
		relid = IndexGetRelation(relid, true);
	if (!OidIsValid(relid) || kl_classify(relid, extoid) != KL_CATALOG_LEDGER)
		return;
	PreventInTransactionBlock(is_top_level, what);
}

/*
 * kl_check_statement
 *		Dispatch on the statement type.  Unlisted statements are not
 *		inspected and cost nothing.
 */
static void
kl_check_statement(Node *parsetree, const char *queryString, bool is_top_level)
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
		case T_ViewStmt:
		case T_LockStmt:
		case T_TruncateStmt:
		case T_AlterObjectDependsStmt:
		case T_AlterSeqStmt:
		case KL_T_CLUSTER_STMT:
		case T_ReindexStmt:
			break;
		default:
			return;
	}

	/*
	 * pg_upgrade restores the schema of the old cluster in binary upgrade mode:
	 * the extension is created empty and every member is added with ALTER
	 * EXTENSION ... ADD, which this hook refuses everywhere else.  Only the
	 * upgrade can reach a server in that mode (it is started with -b by
	 * pg_upgrade, for a superuser, on a new cluster nobody else is using).
	 */
	if (IsBinaryUpgrade)
		return;

	/* Nothing to protect where the extension is not installed. */
	extoid = get_extension_oid(KL_EXTENSION_NAME, true);
	if (!OidIsValid(extoid))
		return;
	/* The statements of the extension's own install and upgrade script. */
	if (kl_is_own_script(extoid, queryString))
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
					case OBJECT_SCHEMA:
						/* The owner of the schema is the owner of the extension. */
						if (!superuser() && stmt->object != NULL &&
							IsA(stmt->object, String) &&
							strcmp(strVal(stmt->object), KL_SCHEMA_NAME) == 0)
							kl_reject("change the owner of schema", KL_SCHEMA_NAME,
									  KL_CATALOG_LEDGER);
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
			kl_check_fk_elements(((CreateStmt *) parsetree)->tableElts, extoid);
			kl_check_fk_elements(((CreateStmt *) parsetree)->constraints, extoid);
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

		case T_ViewStmt:
			{
				ViewStmt   *stmt = (ViewStmt *) parsetree;

				/* CREATE OR REPLACE VIEW of the status view of the extension. */
				if (stmt->replace)
				{
					relid = kl_rv_relid(stmt->view, NoLock);
					if (kl_classify(relid, extoid) != KL_NOT_LEDGER)
						kl_reject("replace view", get_rel_name(relid), KL_CATALOG_LEDGER);
				}
			}
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

		case KL_T_CLUSTER_STMT:
		case T_ReindexStmt:
			kl_check_maintenance(parsetree, is_top_level, extoid);
			break;

		case T_LockStmt:
			kl_check_lock((LockStmt *) parsetree, extoid);
			break;

		case T_TruncateStmt:
			kl_check_truncate((TruncateStmt *) parsetree, extoid);
			break;

		case T_AlterObjectDependsStmt:
			kl_check_depends((AlterObjectDependsStmt *) parsetree, extoid);
			break;

		case T_AlterSeqStmt:
			/*
			 * ALTER SEQUENCE of the registry's sequence: a role that is not a
			 * superuser could restart it (ledger_enable then fails on a key that
			 * exists) or make it run out.  A superuser may.
			 */
			if (!superuser())
			{
				relid = kl_rv_relid(((AlterSeqStmt *) parsetree)->sequence, NoLock);
				kind = kl_classify(relid, extoid);
				if (kind != KL_NOT_LEDGER)
					kl_reject("alter sequence", get_rel_name(relid), kind);
			}
			break;

		default:
			break;
	}
}

/*
 * kl_may_change_owner
 *		Whether a statement can change the owner of a table: REASSIGN OWNED, or
 *		ALTER TABLE with an OWNER TO subcommand.
 */
static bool
kl_may_change_owner(Node *parsetree)
{
	if (parsetree == NULL)
		return false;
	if (IsA(parsetree, ReassignOwnedStmt))
		return true;
	if (IsA(parsetree, AlterTableStmt))
	{
		ListCell   *lc;

		foreach(lc, ((AlterTableStmt *) parsetree)->cmds)
		{
			if (lfirst_node(AlterTableCmd, lc)->subtype == AT_ChangeOwner)
				return true;
		}
	}
	return false;
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
	bool		owner_change;

	kl_note_script(queryString);
	kl_check_statement(pstmt->utilityStmt, queryString,
					   context == PROCESS_UTILITY_TOPLEVEL);
	owner_change = kl_may_change_owner(pstmt->utilityStmt);

	if (prev_ProcessUtility)
		prev_ProcessUtility(pstmt, queryString, readOnlyTree, context, params,
							queryEnv, dest, qc);
	else
		standard_ProcessUtility(pstmt, queryString, readOnlyTree, context,
								params, queryEnv, dest, qc);

	/*
	 * A history table is readable by the owner of its ledger table only.  The
	 * event trigger moves the privileges after an ALTER TABLE ... OWNER TO,
	 * but REASSIGN OWNED fires none, and event triggers can be off.
	 */
	if (owner_change && !creating_extension)
	{
		Oid			extoid = get_extension_oid(KL_EXTENSION_NAME, true);

		if (OidIsValid(extoid))
			kl_sync_all_history_acls(kl_extension_owner(extoid));
	}
}

/*
 * kl_drop_protected
 *		Whether the object that is being dropped is part of the ledger: the
 *		extension, its schema, a member of it (a table, a sequence, the view, a
 *		function, an event trigger), a table under ledger control and its
 *		history table (or a column of any of them), an index of a table of the
 *		ledger catalog, and a trigger of the ledger on a ledger table (the
 *		capture and blocking triggers; a trigger of the application is not).
 *		Indexes of a user ledger table may be dropped, as the statement
 *		hook has always allowed.
 */
static bool
kl_drop_protected(Oid classId, Oid objectId, int subId, Oid extoid)
{
	switch (classId)
	{
		case ExtensionRelationId:
			return objectId == extoid;

		case NamespaceRelationId:
			return objectId == get_namespace_oid(KL_SCHEMA_NAME, true);

		case RelationRelationId:
			{
				char		relkind = get_rel_relkind(objectId);
				KlKind		kind;

				if (relkind == '\0')
					return false;
				kind = kl_classify(objectId, extoid);
				/*
				 * An index of a ledger table is not protected by itself: the
				 * statement hook rejects DROP INDEX on a catalog table, and an
				 * index goes with its table, which is protected;
				 * REINDEX CONCURRENTLY drops the old index of a catalog table, and
				 * that is an honest operation.
				 */
				if (relkind == RELKIND_INDEX || relkind == RELKIND_PARTITIONED_INDEX)
					return false;
				if (kind == KL_CATALOG_LEDGER)
					return true;
				if (kind == KL_USER_LEDGER)
					return relkind != RELKIND_INDEX &&
						relkind != RELKIND_PARTITIONED_INDEX;
				/* what kl_classify does not look at: toast tables, and so on */
				return getExtensionOfObject(classId, objectId) == extoid;
			}

		case ProcedureRelationId:
		case EventTriggerRelationId:
			return getExtensionOfObject(classId, objectId) == extoid;

		case TriggerRelationId:
			{
				HeapTuple	tup;
				Relation	rel;
				ScanKeyData key;
				SysScanDesc scan;
				bool		result = false;

				rel = table_open(TriggerRelationId, AccessShareLock);
				ScanKeyInit(&key, Anum_pg_trigger_oid, BTEqualStrategyNumber,
							F_OIDEQ, ObjectIdGetDatum(objectId));
				scan = systable_beginscan(rel, TriggerOidIndexId, true, NULL, 1,
										  &key);
				tup = systable_getnext(scan);
				if (HeapTupleIsValid(tup))
				{
					Form_pg_trigger trig = (Form_pg_trigger) GETSTRUCT(tup);
					KlKind		kind = kl_classify(trig->tgrelid, extoid);

					if (kind == KL_CATALOG_LEDGER)
						result = true;
					else if (kind == KL_USER_LEDGER)
						result = kl_is_ledger_trigger_name(NameStr(trig->tgname));
				}
				systable_endscan(scan);
				table_close(rel, AccessShareLock);
				return result;
			}

		default:
			return false;
	}
}

/*
 * kl_object_access
 *		object_access_hook.  Every object that a DROP removes, whatever the
 *		statement and the dependency path that led to it, is announced here
 *		before it goes (OAT_DROP, from deleteOneObject): DROP EXTENSION of
 *		anything the extension depends on (plpgsql, which the SQL and PL/pgSQL
 *		functions of 0.3 are written in), DROP SCHEMA ... CASCADE, DROP OWNED,
 *		DROP TYPE ... CASCADE, a dependency that ALTER ... DEPENDS ON EXTENSION
 *		added.  While tables are under ledger control, nothing of the ledger
 *		may be dropped, by any role, superusers included; the statements of the
 *		extension's own install and upgrade script are the only exception (they
 *		replace functions), and nothing is refused when no table is under ledger
 *		control (DROP EXTENSION is then an ordinary one: there is no ledger to
 *		lose).  The statement hook and the event triggers say it earlier, with
 *		a message that fits the statement; this is the check that does not
 *		depend on the shape of the statement (L11-02).
 */
static void
kl_object_access(ObjectAccessType access, Oid classId, Oid objectId,
				 int subId, void *arg)
{
	Oid			extoid;

	if (prev_object_access_hook)
		prev_object_access_hook(access, classId, objectId, subId, arg);
	if (access != OAT_DROP || IsBinaryUpgrade)
		return;
	extoid = kl_extension_oid();
	if (!OidIsValid(extoid))
		return;
	if (creating_extension && CurrentExtensionObject == extoid)
		return;
	if (!kl_drop_protected(classId, objectId, subId, extoid))
		return;
	if (kl_ledger_relids() == NIL)
		return;

	{
		ObjectAddress addr;
		char	   *desc;

		ObjectAddressSubSet(addr, classId, objectId, subId);
		desc = getObjectDescription(&addr, true);
		ereport(ERROR,
				(errcode(ERRCODE_KL_PROTECTED),
				 errmsg("cannot drop %s: it belongs to the ledger, and tables are under ledger control",
						desc ? desc : "an object of the ledger"),
				 errhint("Ledger tables are append-only; the ledger can only be switched off outside the database. This statement drops the object through a dependency.")));
	}
}
