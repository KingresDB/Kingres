/*-------------------------------------------------------------------------
 *
 * guard.c
 *	  The write guard: a row of a ledger catalog table exists only because
 *	  the code of the extension wrote it.
 *
 * The problem.  The ledger tables (kingres.ledger_*) cannot be updated or
 * deleted by any role (BEFORE triggers), and no role has been granted INSERT
 * on them.  A member of pg_write_all_data bypasses table privileges, though,
 * and could insert a block, a transaction row, a digest or a pending row by
 * hand; every later honest block would then follow the forged one.
 *
 * The guard.  ExecutorCheckPerms_hook is called by ExecCheckPermissions for
 * every plan that is started (INSERT, UPDATE, DELETE, MERGE, INSERT ... ON
 * CONFLICT, writes through views and rules, writes of SECURITY DEFINER
 * functions and triggers, foreign key actions, the plans that SPI runs) and by
 * COPY FROM, and it is called whatever the role is, a superuser included:
 * the check that a superuser passes happens before the hook runs.  The hook
 * refuses every statement that inserts, deletes or updates a row of a ledger
 * catalog table, unless one of three things holds:
 *
 *	  1. The code of the extension is writing: a backend-local depth counter,
 *		 raised and lowered by kl_write_enter and kl_write_leave around the
 *		 statements that this library itself executes (the capture code in
 *		 capture.c and kingres.ledger_internal_write below).  SQL cannot raise
 *		 it: the only SQL-callable function that does is
 *		 ledger_internal_write, whose EXECUTE is held by the extension owner
 *		 alone (nobody can be granted it: the ProcessUtility hook refuses the
 *		 GRANT), and it runs one of a fixed list of statements, each of which
 *		 touches nothing but ledger tables and functions of the extension and
 *		 their arguments: no trigger, default, check constraint, index
 *		 expression, policy or operator of a user runs inside it (see the
 *		 comment at ledger_internal_write).  The counter is lowered in a
 *		 PG_FINALLY block, and zeroed by the transaction callback, so an error
 *		 or a rolled back subtransaction cannot leave it raised.
 *	  2. The script of the extension itself is running (CREATE EXTENSION or
 *		 ALTER EXTENSION UPDATE, creating_extension for this extension): the
 *		 script writes the identity of the chain.
 *	  3. A superuser asked for it: the setting kingres_ledger.allow_manual_write
 *		 is on, and both the current user and the session user are superusers
 *		 (so SET ROLE, and a SECURITY DEFINER function of a superuser called by
 *		 a role that was given the right to set the parameter, do not count).
 *		 The setting can be changed in a session only (see the check hook): not
 *		 in postgresql.conf, not with ALTER SYSTEM, ALTER ROLE or ALTER
 *		 DATABASE, not as a start-up option of the connection.
 *
 * What the hook does not see.  TRUNCATE and ALTER TABLE are utility
 * statements (TRUNCATE is stopped by its trigger, ALTER TABLE by the
 * ProcessUtility hook), large objects are not rows of these tables, the
 * logical replication apply worker and the table synchronisation worker do
 * not call ExecCheckPermissions (they check the privileges themselves), and
 * C code of another extension that writes the heap directly is not an
 * executor statement.  The README lists each case.
 *
 * ExecutorCheckPerms_hook has the same signature on PostgreSQL 18 and 19, and
 * 19 also calls ExecCheckPermissions for the foreign key checks that bypass
 * the executor; they only lock the referenced row (ACL_SELECT_FOR_UPDATE,
 * which is the bit ACL_UPDATE, with no updated column), and the hook ignores a
 * statement whose only write bit is that one.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/guard.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/htup_details.h"
#include "access/xact.h"
#include "catalog/dependency.h"
#include "catalog/namespace.h"
#include "catalog/pg_class.h"
#include "catalog/pg_extension.h"
#include "catalog/pg_type.h"
#include "commands/extension.h"
#include "executor/executor.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "nodes/parsenodes.h"
#include "utils/acl.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/inval.h"
#include "utils/lsyscache.h"
#include "utils/rel.h"
#include "utils/syscache.h"

#include "kingres_ledger.h"

PG_FUNCTION_INFO_V1(kingres_ledger_internal_write);

/* Raised by the code of the extension while it writes; see the file header. */
static int	kl_write_depth = 0;

/* GUC kingres_ledger.allow_manual_write. */
static bool kl_allow_manual_write = false;

static ExecutorCheckPerms_hook_type prev_ExecutorCheckPerms_hook = NULL;

/*
 * kl_write_enter, kl_write_leave
 *		Raise and lower the marker.  Callers lower it in a PG_FINALLY block.
 */
void
kl_write_enter(void)
{
	kl_write_depth++;
}

void
kl_write_leave(void)
{
	if (kl_write_depth > 0)
		kl_write_depth--;
}

/* Called at the end of every transaction: nothing stays raised. */
void
kl_write_reset(void)
{
	kl_write_depth = 0;
}

/*
 * kl_check_manual_write
 *		Check hook of kingres_ledger.allow_manual_write.  The setting may be
 *		turned on only by a command of a session (SET, SET LOCAL, set_config):
 *		a value from postgresql.conf (and ALTER SYSTEM), from ALTER ROLE or
 *		ALTER DATABASE, from a connection option or from an environment
 *		variable would switch the guard off for sessions that never asked for
 *		it.  PGC_S_TEST is the source ALTER ROLE ... SET validates with.
 */
static bool
kl_check_manual_write(bool *newval, void **extra, GucSource source)
{
	if (*newval && source != PGC_S_SESSION)
	{
		GUC_check_errmsg("kingres_ledger.allow_manual_write can be turned on only with SET in a session");
		GUC_check_errhint("It cannot be set in postgresql.conf, with ALTER SYSTEM, ALTER ROLE, ALTER DATABASE or as a connection option.");
		return false;
	}
	return true;
}

/*
 * kl_manual_write_allowed
 *		A superuser, in a session where the superuser turned the setting on,
 *		acting as that superuser.
 */
static bool
kl_manual_write_allowed(void)
{
	return kl_allow_manual_write &&
		superuser_arg(GetUserId()) && superuser_arg(GetOuterUserId());
}

/*
 * The OID of the extension, cached: the triggers of the ledger tables ask for
 * it once per row.  Forgotten when pg_extension changes (a DROP or a CREATE).
 */
static Oid	kl_extoid_cache = InvalidOid;
static bool kl_extoid_callback_registered = false;

static void
kl_extoid_invalidate(Datum arg, int cacheid, uint32 hashvalue)
{
	kl_extoid_cache = InvalidOid;
}

static Oid
kl_extension_oid(void)
{
	if (!kl_extoid_callback_registered)
	{
		CacheRegisterSyscacheCallback(EXTENSIONOID, kl_extoid_invalidate, (Datum) 0);
		kl_extoid_callback_registered = true;
	}
	if (!OidIsValid(kl_extoid_cache))
		kl_extoid_cache = get_extension_oid(KL_EXTENSION_NAME, true);
	return kl_extoid_cache;
}

/*
 * kl_write_allowed
 *		The three cases of the file header.
 */
static bool
kl_write_allowed(Oid extoid)
{
	if (kl_write_depth > 0)
		return true;
	if (creating_extension && CurrentExtensionObject == extoid)
		return true;
	return kl_manual_write_allowed();
}

/*
 * kl_is_catalog_table
 *		Whether the relation is a table of the extension in schema kingres.
 *		Called for every relation that a statement writes, so the cheap tests
 *		come first: only a relation of that schema whose name starts with
 *		"ledger_" is looked up in pg_depend.
 */
static bool
kl_is_catalog_table(Oid relid, Oid extoid)
{
	Oid			nspoid = get_rel_namespace(relid);
	char	   *name;
	bool		result;

	if (!OidIsValid(nspoid) || nspoid != get_namespace_oid(KL_SCHEMA_NAME, true))
		return false;
	if (get_rel_relkind(relid) != RELKIND_RELATION)
		return false;
	name = get_rel_name(relid);
	if (name == NULL)
		return false;
	result = strncmp(name, "ledger_", 7) == 0 &&
		getExtensionOfObject(RelationRelationId, relid) == extoid;
	pfree(name);
	return result;
}

/*
 * kl_extension_version
 *		The installed version of the extension (palloc'd), or NULL.
 */
static char *
kl_extension_version(Oid extoid)
{
	HeapTuple	tup = SearchSysCache1(EXTENSIONOID, ObjectIdGetDatum(extoid));
	char	   *result = NULL;

	if (HeapTupleIsValid(tup))
	{
		bool		isnull;
		Datum		d = SysCacheGetAttr(EXTENSIONOID, tup,
										Anum_pg_extension_extversion, &isnull);

		if (!isnull)
			result = TextDatumGetCString(d);
		ReleaseSysCache(tup);
	}
	return result;
}

/*
 * kl_refuse_write
 *		Raise the error.  When the scripts of the extension are older than the
 *		library the old functions are the ones refused: say so, and say what
 *		to run.
 */
static void
kl_refuse_write(Oid relid, Oid extoid, AclMode writes)
{
	char	   *version = kl_extension_version(extoid);
	char	   *relname = get_rel_name(relid);

	/*
	 * An UPDATE or DELETE has always been refused with the message of the
	 * blocking trigger; the guard keeps saying so, so that nobody who handles
	 * that error has to learn another.  Only the queue (DELETE of ledger_pending
	 * is what the block builder does) and the inserts are the guard's own.
	 */
	if ((writes & ACL_INSERT) == 0 &&
		(relname == NULL || strcmp(relname, "ledger_pending") != 0 || (writes & ACL_DELETE) == 0))
		ereport(ERROR,
				(errcode(ERRCODE_KL_APPEND_ONLY),
				 errmsg("table \"%s\" is an append-only ledger table",
						relname ? relname : "?")));

	if (version != NULL && strcmp(version, KL_SCRIPT_VERSION) != 0)
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("cannot write to table \"%s\": the SQL of extension \"%s\" is version %s, and this library writes the ledger only through the functions of version %s",
						get_rel_name(relid), KL_EXTENSION_NAME, version,
						KL_SCRIPT_VERSION),
				 errhint("Run ALTER EXTENSION %s UPDATE.", KL_EXTENSION_NAME)));
	ereport(ERROR,
			(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
			 errmsg("cannot write to table \"%s\": only the code of the ledger writes its tables",
					get_rel_name(relid)),
			 errdetail("The rows of the ledger tables are written by the extension (the capture triggers, ledger_build_block and the other functions of the extension) and by nothing else."),
			 errhint("A superuser can write by hand for one session: SET kingres_ledger.allow_manual_write = on.")));
}

/*
 * kl_ExecutorCheckPerms
 *		The hook.  Every statement that writes a ledger catalog table is
 *		refused unless the write is allowed (see the file header).
 */
static bool
kl_ExecutorCheckPerms(List *rangeTable, List *rtePermInfos,
					  bool ereport_on_violation)
{
	ListCell   *lc;
	Oid			extoid = InvalidOid;
	bool		extoid_known = false;

	if (prev_ExecutorCheckPerms_hook &&
		!prev_ExecutorCheckPerms_hook(rangeTable, rtePermInfos,
									  ereport_on_violation))
		return false;

	foreach(lc, rtePermInfos)
	{
		RTEPermissionInfo *perminfo = lfirst_node(RTEPermissionInfo, lc);
		AclMode		writes = perminfo->requiredPerms &
			(ACL_INSERT | ACL_UPDATE | ACL_DELETE | ACL_TRUNCATE);

		if (writes == 0)
			continue;
		/* SELECT ... FOR UPDATE and the foreign key checks: a row lock. */
		if (writes == ACL_UPDATE && bms_is_empty(perminfo->updatedCols))
			continue;
		if (!extoid_known)
		{
			extoid = kl_extension_oid();
			extoid_known = true;
		}
		if (!OidIsValid(extoid) || !kl_is_catalog_table(perminfo->relid, extoid))
			continue;
		if (kl_write_allowed(extoid))
			continue;
		if (!ereport_on_violation)
			return false;
		kl_refuse_write(perminfo->relid, extoid, writes);
	}
	return true;
}

/*
 * kl_guard_init
 *		Define the setting.  Called from _PG_init in every process, before the
 *		prefix is reserved.
 */
void
kl_guard_init(void)
{
	DefineCustomBoolVariable("kingres_ledger.allow_manual_write",
							 "Lets a superuser write the ledger tables by hand in this session.",
							 "Off by default and settable in a session only; the rows of the ledger tables are otherwise written by the extension alone.",
							 &kl_allow_manual_write,
							 false,
							 PGC_SUSET,
							 GUC_NOT_IN_SAMPLE,
							 kl_check_manual_write,
							 NULL,
							 NULL);
}

/*
 * kl_guard_install
 *		Install the executor hook.  Called when the library is preloaded.
 */
void
kl_guard_install(void)
{
	prev_ExecutorCheckPerms_hook = ExecutorCheckPerms_hook;
	ExecutorCheckPerms_hook = kl_ExecutorCheckPerms;
}

/*
 * What kingres.ledger_internal_write runs.  Each statement is fixed text with
 * numbered parameters; the arguments are values and never text that is run.
 * The statements name the schema, and the function has a fixed search_path
 * (pg_catalog, pg_temp), so nothing a session sets changes what they mean.
 */
typedef struct KlWriteOp
{
	int			nargs;
	Oid			argtypes[9];
	bool		returns_id;		/* RETURNING one bigint */
	const char *sql;
} KlWriteOp;

static const KlWriteOp kl_write_ops[] = {
	/* 1: register a table */
	{4, {INT4OID, REGCLASSOID, TEXTOID, REGCLASSOID}, false,
		"INSERT INTO kingres.ledger_table (table_id, relid, mode, history_relid) "
	"OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4)"},
	/* 2: the transaction rows of a block */
	{2, {INT8OID, INT8ARRAYOID}, false,
		"INSERT INTO kingres.ledger_tx "
		"(ledger_txid, block_height, tx_index, entry_count, merkle_root, user_name) "
		"SELECT p.ledger_txid, $1, "
		"(pg_catalog.row_number() OVER (ORDER BY p.ledger_txid) - 1)::pg_catalog.int4, "
		"r.cnt, r.root, p.user_name "
		"FROM kingres.ledger_pending p "
		"CROSS JOIN LATERAL ("
		"SELECT pg_catalog.count(*)::pg_catalog.int4 AS cnt, "
		"kingres.ledger_merkle_root("
		"kingres.ledger_tx_leaf(e.seq, e.table_id, e.op, e.row_hash) ORDER BY e.seq) AS root "
		"FROM kingres.ledger_entry e WHERE e.ledger_txid = p.ledger_txid) r "
		"WHERE p.ledger_txid = ANY ($2)"},
	/* 3: the block */
	{6, {INT8OID, BYTEAOID, BYTEAOID, INT4OID, TIMESTAMPTZOID, BYTEAOID}, false,
		"INSERT INTO kingres.ledger_blocks "
		"(height, hash_version, prev_hash, tx_merkle_root, tx_count, closed_at, block_hash) "
	"VALUES ($1, 1, $2, $3, $4, $5, $6)"},
	/* 4: the transactions leave the queue */
	{1, {INT8ARRAYOID}, false,
	"DELETE FROM kingres.ledger_pending WHERE ledger_txid = ANY ($1)"},
	/* 5: a recorded digest */
	{5, {TIMESTAMPTZOID, NAMEOID, INT8OID, BYTEAOID, JSONBOID}, false,
		"INSERT INTO kingres.ledger_digest "
		"(generated_at, generated_by, height, block_hash, digest) "
	"VALUES ($1, $2, $3, $4, $5)"},
	/* 6: a recorded verification that ran */
	{9, {TIMESTAMPTZOID, NAMEOID, INT8OID, INT8OID, TEXTOID, INT4OID, INT8OID, INT8OID, JSONBOID}, true,
		"INSERT INTO kingres.ledger_verification "
		"(started_at, finished_at, started_by, from_height, to_height, state, "
		"problem_count, blocks_checked, rows_checked, problems) "
		"VALUES ($1, pg_catalog.clock_timestamp(), $2, $3, $4, $5, $6, $7, $8, $9) "
	"RETURNING verification_id"},
	/* 7: a recorded verification that failed */
	{5, {TIMESTAMPTZOID, NAMEOID, INT8OID, INT8OID, JSONBOID}, true,
		"INSERT INTO kingres.ledger_verification "
		"(started_at, finished_at, started_by, from_height, to_height, state, "
		"problem_count, problems) "
		"VALUES ($1, pg_catalog.clock_timestamp(), $2, $3, $4, 'failed', NULL, $5) "
	"RETURNING verification_id"}
};

/*
 * kingres.ledger_internal_write(op int, VARIADIC args "any") returns bigint
 *		Run one of the fixed statements above with the marker raised, and
 *		return the number of rows it changed (the new id for the two
 *		statements that return one).
 *
 * Who can call it.  Nobody but the extension owner has EXECUTE, and the
 * ProcessUtility hook refuses every GRANT on it, so a role that is not a
 * superuser reaches it only through a SECURITY DEFINER function of the
 * extension.  A superuser who calls it directly writes the ledger without
 * the setting; a superuser can change the catalogs anyway, and this is no
 * way for anybody else.
 *
 * What can run while the marker is raised.  Only the statement above, in an
 * SPI connection of its own.  The tables it writes have no trigger but the
 * blocking ones (which raise errors), no policy, no rule, and no default or
 * check expression other than constant ones, now() and clock_timestamp();
 * their indexes have no expression; the types of the columns are built in;
 * the foreign key checks are system triggers.  The functions the statements
 * name are pg_catalog's and the extension's, found by schema qualification
 * and by a search_path that has nothing in front of pg_catalog.  A role
 * cannot add a trigger, a rule, a policy or an index to these tables (the
 * ProcessUtility hook and the event triggers refuse), and no user code is
 * an argument: the arguments are values.  The one thing that could run user
 * code is a statement-level event trigger on the statement; event triggers
 * fire on DDL, not on these statements, and only a superuser can create one.
 */
Datum
kingres_ledger_internal_write(PG_FUNCTION_ARGS)
{
	int32		op = PG_GETARG_INT32(0);
	const KlWriteOp *spec;
	Datum		values[9];
	char		nulls[9];
	int			nargs = PG_NARGS() - 1;
	int			i;
	int			rc = 0;
	int64		result = 0;

	if (op < 1 || op > (int32) lengthof(kl_write_ops))
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("unknown ledger write %d", op)));
	spec = &kl_write_ops[op - 1];
	if (nargs != spec->nargs)
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("ledger write %d takes %d arguments, not %d",
						op, spec->nargs, nargs)));
	for (i = 0; i < nargs; i++)
	{
		if (get_fn_expr_argtype(fcinfo->flinfo, i + 1) != spec->argtypes[i])
			ereport(ERROR,
					(errcode(ERRCODE_DATATYPE_MISMATCH),
					 errmsg("argument %d of ledger write %d has the wrong type",
							i + 1, op)));
		values[i] = PG_GETARG_DATUM(i + 1);
		nulls[i] = PG_ARGISNULL(i + 1) ? 'n' : ' ';
	}

	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	kl_write_enter();
	PG_TRY();
	{
		rc = SPI_execute_with_args(spec->sql, nargs, (Oid *) spec->argtypes,
								   values, nulls, false, 0);
	}
	PG_FINALLY();
	{
		kl_write_leave();
	}
	PG_END_TRY();
	if (rc < 0)
		elog(ERROR, "ledger write %d failed: %s", op, SPI_result_code_string(rc));
	if (spec->returns_id)
	{
		bool		isnull;

		if (SPI_processed != 1 || SPI_tuptable == NULL)
			elog(ERROR, "ledger write %d returned no row", op);
		result = DatumGetInt64(SPI_getbinval(SPI_tuptable->vals[0],
											 SPI_tuptable->tupdesc, 1, &isnull));
	}
	else
		result = (int64) SPI_processed;
	SPI_finish();
	PG_RETURN_INT64(result);
}

/*
 * kl_manual_delete_allowed
 *		Whether a DELETE of a row of this table is allowed to a superuser who
 *		turned allow_manual_write on: the transaction rows and the recorded
 *		digests, which are what ledger_remove_stray takes away (the README,
 *		"A row above the head").  The blocking trigger asks; every other
 *		table and every other statement stays refused.
 */
bool
kl_manual_delete_allowed(Relation rel)
{
	const char *name = RelationGetRelationName(rel);
	Oid			extoid;

	if (strcmp(name, "ledger_tx") != 0 && strcmp(name, "ledger_digest") != 0)
		return false;
	if (!kl_manual_write_allowed())
		return false;
	extoid = kl_extension_oid();
	return OidIsValid(extoid) &&
		kl_is_catalog_table(RelationGetRelid(rel), extoid);
}

/*
 * kl_pending_delete_allowed
 *		Whether a DELETE of a row of kingres.ledger_pending is allowed: the
 *		block builder takes the transactions it put in a block off the queue
 *		through ledger_internal_write; nobody else does.
 */
bool
kl_pending_delete_allowed(Relation rel)
{
	Oid			extoid = kl_extension_oid();

	return OidIsValid(extoid) &&
		strcmp(RelationGetRelationName(rel), "ledger_pending") == 0 &&
		kl_is_catalog_table(RelationGetRelid(rel), extoid) &&
		kl_write_allowed(extoid);
}

/*
 * kl_insert_allowed
 *		Whether a row may be inserted into this table of the ledger now: asked
 *		by the BEFORE INSERT trigger of the ledger tables, which is what stops
 *		the writers that do not call ExecCheckPermissions (the logical
 *		replication apply worker and the table synchronisation worker).
 */
bool
kl_insert_allowed(void)
{
	Oid			extoid = kl_extension_oid();

	return OidIsValid(extoid) && kl_write_allowed(extoid);
}

/*
 * kl_refuse_write_rel
 *		The error of a refused insert or delete, for the guard trigger.
 */
void
kl_refuse_write_rel(Relation rel, bool is_insert)
{
	kl_refuse_write(RelationGetRelid(rel), kl_extension_oid(),
					is_insert ? ACL_INSERT : ACL_DELETE);
}
