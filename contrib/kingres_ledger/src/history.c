/*-------------------------------------------------------------------------
 *
 * history.c
 *	  Maintenance of the history table of an updatable ledger table: the
 *	  columns that follow ADD COLUMN, and the privileges that follow the
 *	  owner of the table.
 *
 * Both are done through the catalog API of the server and not through SQL,
 * so that no statement is run in the middle of a privileged operation: no
 * event trigger, no function of a user and no default expression can run
 * while the extension changes the history table.  The protection hook
 * (protect.c) therefore has no "internal statement" switch: it treats every
 * statement that reaches it the same way, and the operations of this file
 * never reach it.
 *
 * Columns.  kingres.ledger_history_sync adds to the history table the columns
 * that the ledger table gained, with AlterTableInternal on exactly that
 * table and exactly ADD COLUMN subcommands, with the collection of commands
 * for event triggers switched off for its duration, so that no event trigger
 * sees the change as a statement of its own.
 *
 * Privileges.  The history table holds the old versions of the rows of the
 * ledger table.  Reading it is allowed to the extension owner and to the
 * owner of the ledger table, and to nobody else: kl_sync_history_acl sets the
 * access control list to exactly that, whatever default privileges said when
 * the table was created and whoever owned the ledger table before.  It is
 * called when the history table is created, by the event trigger after every
 * DDL command, and by the protection hook after the statements that change
 * an owner but fire no event trigger (REASSIGN OWNED) or may be run with
 * event triggers off (ALTER TABLE ... OWNER TO).
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/history.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/genam.h"
#include "access/htup_details.h"
#include "access/table.h"
#include "access/xact.h"
#include "catalog/namespace.h"
#include "catalog/pg_attribute.h"
#include "catalog/pg_class.h"
#include "commands/event_trigger.h"
#include "commands/tablecmds.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "nodes/makefuncs.h"
#include "nodes/parsenodes.h"
#include "utils/acl.h"
#include "utils/array.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"
#include "utils/rel.h"
#include "utils/snapmgr.h"
#include "utils/syscache.h"

#include "kingres_ledger.h"

PG_FUNCTION_INFO_V1(kingres_ledger_history_sync);
PG_FUNCTION_INFO_V1(kingres_ledger_history_acl_sync);

/*
 * kingres.ledger_history_sync(relation regclass, history regclass) returns boolean
 *		Add to the history table the columns that the ledger table has gained
 *		(ADD COLUMN), in one ALTER TABLE so that the history table matches the
 *		table again when the statement ends.  Called by the event trigger
 *		kingres.ledger_guard after every DDL command, for every updatable
 *		ledger table.  Returns true when it changed something.
 *
 * Only the extension may call this: it is not executable by PUBLIC and the
 * protection hook rejects a grant on it.  The change is made with
 * AlterTableInternal, which does not go through ProcessUtility, so the hook
 * and the user's event triggers do not see it, and the only thing that can
 * happen is what is built here: an ADD COLUMN of a column that exists in the
 * ledger table, with the type and modifier of that column, on the history
 * table whose OID the registry gave.
 */
Datum
kingres_ledger_history_sync(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);
	Oid			histid = PG_GETARG_OID(1);
	Relation	rel = table_open(relid, AccessShareLock);
	Relation	hist = table_open(histid, AccessShareLock);
	TupleDesc	td = RelationGetDescr(rel);
	TupleDesc	hd = RelationGetDescr(hist);
	List	   *cmds = NIL;
	int			i;
	int			j;

	if (hist->rd_rel->relkind != RELKIND_RELATION)
		ereport(ERROR,
				(errcode(ERRCODE_WRONG_OBJECT_TYPE),
				 errmsg("history table of \"%s\" is not an ordinary table",
						RelationGetRelationName(rel))));

	for (i = 0; i < td->natts; i++)
	{
		Form_pg_attribute ta = TupleDescAttr(td, i);
		bool		found = false;
		AlterTableCmd *cmd;

		if (ta->attisdropped)
			continue;
		for (j = 0; j < hd->natts; j++)
		{
			Form_pg_attribute ha = TupleDescAttr(hd, j);

			if (!ha->attisdropped &&
				strcmp(NameStr(ha->attname), NameStr(ta->attname)) == 0)
			{
				found = true;
				break;
			}
		}
		if (found)
			continue;

		cmd = makeNode(AlterTableCmd);
		cmd->subtype = AT_AddColumn;
		cmd->def = (Node *) makeColumnDef(NameStr(ta->attname), ta->atttypid,
										  ta->atttypmod, InvalidOid);
		cmd->behavior = DROP_RESTRICT;
		cmds = lappend(cmds, cmd);
	}
	table_close(rel, NoLock);
	table_close(hist, NoLock);

	if (cmds == NIL)
		PG_RETURN_BOOL(false);

	/*
	 * The statement that is being run (usually an ALTER TABLE of the ledger
	 * table) is being collected for the event triggers; this change must not
	 * become part of it.
	 */
	EventTriggerInhibitCommandCollection();
	PG_TRY();
	{
		AlterTableInternal(histid, cmds, false);
	}
	PG_FINALLY();
	{
		EventTriggerUndoInhibitCommandCollection();
	}
	PG_END_TRY();
	CommandCounterIncrement();
	PG_RETURN_BOOL(true);
}

/*
 * kl_acl_collect
 *		Look at one ACL (of the table, or of a column) of the history table and
 *		add to "bad" the OIDs of the roles that have anything there that the
 *		rule does not give them (0 stands for PUBLIC).  The rule: the owner of
 *		the history table has what an owner has, the owner of the ledger table
 *		has SELECT, granted by the owner of the history table, without grant
 *		option, and nobody else has anything.  Only the table ACL may hold that
 *		SELECT; a column ACL holds nothing.  Sets *have_owner_item when the
 *		SELECT of the owner of the ledger table is there in that form.
 */
static void
kl_acl_collect(Datum acldatum, Oid histowner, Oid tableowner,
			   bool table_level, List **bad, bool *have_owner_item)
{
	Acl		   *acl = DatumGetAclP(acldatum);
	AclItem    *items = ACL_DAT(acl);
	int			n = ACL_NUM(acl);
	int			i;

	for (i = 0; i < n; i++)
	{
		AclItem    *ai = &items[i];

		if (ai->ai_grantee == histowner)
			continue;
		if (table_level && tableowner != histowner &&
			ai->ai_grantee == tableowner && ai->ai_grantor == histowner &&
			ACLITEM_GET_PRIVS(*ai) == ACL_SELECT &&
			ACLITEM_GET_GOPTIONS(*ai) == 0)
		{
			*have_owner_item = true;
			continue;
		}
		*bad = list_append_unique_oid(*bad, ai->ai_grantee);
	}
}

/*
 * kl_history_acl_state
 *		Compare the privileges on the history table with the rule above.
 *		Returns the list of roles whose privileges must be removed in "bad" and
 *		whether the owner of the ledger table still has to be granted SELECT in
 *		*need_grant, and the owner of the ledger table.
 */
static void
kl_history_acl_state(Oid relid, Oid histid, List **bad, bool *need_grant,
					 Oid *tableowner_out)
{
	HeapTuple	reltup = SearchSysCache1(RELOID, ObjectIdGetDatum(relid));
	HeapTuple	histtup = SearchSysCache1(RELOID, ObjectIdGetDatum(histid));
	Oid			tableowner;
	Oid			histowner;
	Form_pg_class histform;
	bool		have_owner_item = false;
	bool		isnull;
	Datum		d;
	int			natts;
	int			i;

	if (!HeapTupleIsValid(reltup) || !HeapTupleIsValid(histtup))
		elog(ERROR, "cache lookup failed for a ledger table or its history table");
	tableowner = ((Form_pg_class) GETSTRUCT(reltup))->relowner;
	histform = (Form_pg_class) GETSTRUCT(histtup);
	histowner = histform->relowner;
	natts = histform->relnatts;

	*bad = NIL;
	d = SysCacheGetAttr(RELOID, histtup, Anum_pg_class_relacl, &isnull);
	if (!isnull)
		kl_acl_collect(d, histowner, tableowner, true, bad, &have_owner_item);

	for (i = 1; i <= natts; i++)
	{
		HeapTuple	atttup = SearchSysCache2(ATTNUM, ObjectIdGetDatum(histid),
											 Int16GetDatum(i));

		if (!HeapTupleIsValid(atttup))
			continue;
		d = SysCacheGetAttr(ATTNUM, atttup, Anum_pg_attribute_attacl, &isnull);
		if (!isnull)
			kl_acl_collect(d, histowner, tableowner, false, bad, &have_owner_item);
		ReleaseSysCache(atttup);
	}

	*need_grant = (tableowner != histowner) &&
		(!have_owner_item || list_member_oid(*bad, tableowner));
	*tableowner_out = tableowner;
	ReleaseSysCache(reltup);
	ReleaseSysCache(histtup);
}

/*
 * kl_role_spec
 *		A RoleSpec for a role OID (0 is PUBLIC).
 */
static RoleSpec *
kl_role_spec(Oid roleid)
{
	RoleSpec   *rs = makeNode(RoleSpec);

	rs->location = -1;
	if (!OidIsValid(roleid))
	{
		rs->roletype = ROLESPEC_PUBLIC;
		rs->rolename = NULL;
	}
	else
	{
		rs->roletype = ROLESPEC_CSTRING;
		rs->rolename = pstrdup(GetUserNameFromId(roleid, false));
	}
	return rs;
}

/*
 * kl_run_grant
 *		GRANT SELECT to, or REVOKE ALL from, a list of roles on the history
 *		table, through ExecuteGrantStmt (which fires no event trigger and does
 *		not go through the protection hook).  Run as the owner of the history
 *		table.
 */
static void
kl_run_grant(bool is_grant, Oid histid, List *grantees)
{
	GrantStmt  *g = makeNode(GrantStmt);
	ListCell   *lc;

	g->is_grant = is_grant;
	g->targtype = ACL_TARGET_OBJECT;
	g->objtype = OBJECT_TABLE;
	g->objects = list_make1(makeRangeVar(get_namespace_name(get_rel_namespace(histid)),
										 get_rel_name(histid), -1));
	if (is_grant)
	{
		AccessPriv *ap = makeNode(AccessPriv);

		ap->priv_name = pstrdup("select");
		ap->cols = NIL;
		g->privileges = list_make1(ap);
	}
	else
		g->privileges = NIL;	/* ALL */
	g->grantees = NIL;
	foreach(lc, grantees)
		g->grantees = lappend(g->grantees, kl_role_spec(lfirst_oid(lc)));
	g->grant_option = false;
	g->grantor = NULL;
	g->behavior = DROP_CASCADE;
	ExecuteGrantStmt(g);
}

/*
 * kl_sync_history_acl
 *		Make the privileges on the history table what the rule says (see the
 *		file header).  Returns true when something was changed.  Does nothing
 *		(and is cheap: syscache lookups) when the privileges are right.
 *		The caller runs as the owner of the history table.
 */
static bool
kl_sync_history_acl(Oid relid, Oid histid)
{
	List	   *bad;
	bool		need_grant;
	Oid			tableowner;

	if (!SearchSysCacheExists1(RELOID, ObjectIdGetDatum(relid)) ||
		!SearchSysCacheExists1(RELOID, ObjectIdGetDatum(histid)) ||
		get_rel_relkind(histid) != RELKIND_RELATION)
		return false;

	kl_history_acl_state(relid, histid, &bad, &need_grant, &tableowner);
	if (bad == NIL && !need_grant)
		return false;

	EventTriggerInhibitCommandCollection();
	PG_TRY();
	{
		if (bad != NIL)
			kl_run_grant(false, histid, bad);
		if (need_grant)
			kl_run_grant(true, histid, list_make1_oid(tableowner));
	}
	PG_FINALLY();
	{
		EventTriggerUndoInhibitCommandCollection();
	}
	PG_END_TRY();
	CommandCounterIncrement();
	return true;
}

/*
 * kingres.ledger_history_acl_sync(relation regclass, history regclass) returns boolean
 *		SQL entry of kl_sync_history_acl, called by ledger_enable and by the
 *		event trigger; the calling function runs as the extension owner.  Not
 *		executable by PUBLIC, and the hook rejects a grant on it.
 */
Datum
kingres_ledger_history_acl_sync(PG_FUNCTION_ARGS)
{
	PG_RETURN_BOOL(kl_sync_history_acl(PG_GETARG_OID(0), PG_GETARG_OID(1)));
}

/*
 * kl_sync_all_history_acls
 *		Apply the rule to the history tables of all updatable ledger tables, as
 *		the extension owner "extowner".  Called by the protection hook after a
 *		statement that can change the owner of a ledger table.  The function
 *		runs only these fixed catalog operations with the owner's rights, and
 *		restores the user id afterwards, also on error.
 */
void
kl_sync_all_history_acls(Oid extowner)
{
	List	   *relids = NIL;
	List	   *histids = NIL;
	Oid			nspoid = get_namespace_oid(KL_SCHEMA_NAME, true);
	Oid			tableoid;
	Relation	rel;
	SysScanDesc scan;
	HeapTuple	tup;
	Snapshot	snap;
	ListCell   *l1;
	ListCell   *l2;
	Oid			saved_uid;
	int			saved_sec;

	if (!OidIsValid(nspoid) || !OidIsValid(extowner))
		return;
	tableoid = get_relname_relid("ledger_table", nspoid);
	if (!OidIsValid(tableoid))
		return;

	CommandCounterIncrement();
	rel = table_open(tableoid, AccessShareLock);
	snap = RegisterSnapshot(GetLatestSnapshot());
	scan = systable_beginscan(rel, InvalidOid, false, snap, 0, NULL);
	while (HeapTupleIsValid(tup = systable_getnext(scan)))
	{
		bool		isnull;
		Datum		d;

		if (HeapTupleHeaderGetNatts(tup->t_data) < KL_LEDGER_TABLE_HISTORY_ATTNO)
			continue;
		d = heap_getattr(tup, 2, RelationGetDescr(rel), &isnull);
		if (isnull)
			continue;
		relids = lappend_oid(relids, DatumGetObjectId(d));
		d = heap_getattr(tup, KL_LEDGER_TABLE_HISTORY_ATTNO, RelationGetDescr(rel),
						 &isnull);
		histids = lappend_oid(histids, isnull ? InvalidOid : DatumGetObjectId(d));
	}
	systable_endscan(scan);
	UnregisterSnapshot(snap);
	table_close(rel, AccessShareLock);

	GetUserIdAndSecContext(&saved_uid, &saved_sec);
	SetUserIdAndSecContext(extowner, saved_sec | SECURITY_LOCAL_USERID_CHANGE);
	PG_TRY();
	{
		forboth(l1, relids, l2, histids)
		{
			if (OidIsValid(lfirst_oid(l2)))
				(void) kl_sync_history_acl(lfirst_oid(l1), lfirst_oid(l2));
		}
	}
	PG_FINALLY();
	{
		SetUserIdAndSecContext(saved_uid, saved_sec);
	}
	PG_END_TRY();
}
