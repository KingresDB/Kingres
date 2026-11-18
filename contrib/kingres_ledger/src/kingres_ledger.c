/*-------------------------------------------------------------------------
 *
 * kingres_ledger.c
 *	  Module load, GUCs and small helper functions of kingres_ledger.
 *
 * _PG_init defines the GUC, registers the transaction callbacks of the
 * capture code and, when the library is loaded through
 * shared_preload_libraries, installs the ProcessUtility protection hook.
 * Loading the library any other way (for example by CREATE EXTENSION in a
 * session of a server that does not preload it) still makes the SQL
 * functions work, but kingres.ledger_enable refuses to run because nothing
 * would then stop a privileged user from switching the ledger off.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/kingres_ledger.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/table.h"
#include "catalog/namespace.h"
#include "catalog/objectaddress.h"
#include "catalog/pg_class.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "lib/stringinfo.h"
#include "miscadmin.h"
#include "storage/lmgr.h"
#include "utils/acl.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/lsyscache.h"
#include "utils/rel.h"
#include "utils/syscache.h"

#include "kingres_ledger.h"

PG_MODULE_MAGIC;

PG_FUNCTION_INFO_V1(kingres_ledger_hook_active);
PG_FUNCTION_INFO_V1(kingres_ledger_binary_upgrade);
PG_FUNCTION_INFO_V1(kingres_ledger_outer_user);
PG_FUNCTION_INFO_V1(kingres_ledger_lock_table);

/* GUC: most transactions that one block may contain. */
int			kl_max_block_transactions = 100000;

/* GUC: interval of the block-building launcher in milliseconds; 0 = off. */
int			kl_block_interval = 10000;

/* GUC: the longest one worker may run for one database, in ms; 0 = no limit. */
int			kl_worker_timeout = 60000;

/* GUC: how far the previous block may be ahead of the clock, in ms; 0 = no limit. */
int			kl_max_clock_skew = 3600000;

/* GUC: how long a role that is not a superuser may hold a lock of the extension, in ms; 0 = no limit. */
int			kl_lock_hold_limit = 60000;

/*
 * GUC: most time one transaction may add to the hold limit for ledger_enable, in ms;
 * 0 = no bound.  The default is one more period of the hold limit (L21-06): a role
 * that is not a superuser holds the registry's lock for two minutes at most with the
 * defaults; a table that needs longer is entered by a superuser.
 */
int			kl_enable_credit_limit = 60000;

/* GUC (developer, for the tests): ms that ledger_register_table waits after each batch. */
int			kl_enable_batch_delay = 0;

/* Set in _PG_init when loaded through shared_preload_libraries. */
bool		kl_preloaded = false;

/*
 * _PG_init
 *		Entry point of the library.  This is the only place that defines
 *		GUCs and installs hooks.
 */
void
_PG_init(void)
{
	DefineCustomIntVariable("kingres_ledger.max_block_transactions",
							"Most transactions that one ledger block may contain.",
							"Transactions beyond the limit wait for the next block.",
							&kl_max_block_transactions,
							100000,
							1,
							100000000,
							PGC_SUSET,
							0,
							NULL,
							NULL,
							NULL);

	DefineCustomIntVariable("kingres_ledger.block_interval",
							"How often the ledger launcher looks for pending transactions.",
							"0 switches the block-building workers off.",
							&kl_block_interval,
							10000,
							0,
							86400000,
							PGC_SIGHUP,
							GUC_UNIT_MS,
							NULL,
							NULL,
							NULL);

	DefineCustomIntVariable("kingres_ledger.worker_timeout",
							"The longest a block-building worker may work on one database.",
							"A worker that is still running after this time is cancelled; the database is then retried later with a growing delay. 0 means no limit.",
							&kl_worker_timeout,
							60000,
							0,
							86400000,
							PGC_SIGHUP,
							GUC_UNIT_MS,
							NULL,
							NULL,
							NULL);

	DefineCustomIntVariable("kingres_ledger.max_clock_skew",
							"How far the previous block may be ahead of the clock of the server when a block is built.",
							"The builder refuses to build a block whose predecessor was closed later than this; the chain keeps block times that never go back, so a block closed in the future would otherwise give its time to every later block. 0 means no limit.",
							&kl_max_clock_skew,
							3600000,
							0,
							2147483647,
							PGC_SUSET,
							GUC_UNIT_MS,
							NULL,
							NULL,
							NULL);

	DefineCustomIntVariable("kingres_ledger.lock_hold_limit",
							"How long a role that is not a superuser may hold a lock of the ledger.",
							"The block builders, ledger_enable and ledger_verify_recorded hold a lock of the extension until the end of the transaction of the caller, and a role that is not a superuser may call them only as a statement of its own. A session of such a role that is still in that statement this long after it got the lock is ended, so that no such role can stop the builders, or the capture that depends on them, for longer than the administrator allows (ledger_enable may add the time it spends entering rows, up to kingres_ledger.enable_credit_limit). The setting cannot be changed by the role it limits. 0 means no limit.",
							&kl_lock_hold_limit,
							60000,
							0,
							2147483647,
							PGC_SUSET,
							GUC_UNIT_MS,
							NULL,
							NULL,
							NULL);

	DefineCustomIntVariable("kingres_ledger.enable_credit_limit",
							"The most time that ledger_enable may add to the hold limit of a role that is not a superuser in one transaction.",
							"While ledger_enable enters the rows that a table holds, the time it spends is added to kingres_ledger.lock_hold_limit, so that a table of honest size is not ended by the limit; this is the most that one transaction may add, for all the tables it enters (the default is one minute, so that the registry's lock is held for two minutes at most). A table that needs longer is enabled by a superuser. 0 means no bound. The setting cannot be changed by the role it limits.",
							&kl_enable_credit_limit,
							60000,
							0,
							2147483647,
							PGC_SUSET,
							GUC_UNIT_MS,
							NULL,
							NULL,
							NULL);

	DefineCustomIntVariable("kingres_ledger.enable_batch_delay",
							"Developer setting for the tests: how long ledger_register_table waits after each batch of rows.",
							"The wait counts as time spent entering rows. It is 0 in every real use.",
							&kl_enable_batch_delay,
							0,
							0,
							60000,
							PGC_SUSET,
							GUC_UNIT_MS | GUC_NOT_IN_SAMPLE,
							NULL,
							NULL,
							NULL);

	kl_guard_init();

	MarkGUCPrefixReserved("kingres_ledger");

	kl_capture_init();

	if (process_shared_preload_libraries_in_progress)
	{
		kl_preloaded = true;
		kl_protect_init();
		kl_guard_install();
		kl_worker_init();
	}
}

/*
 * kingres.ledger_hook_active() returns boolean
 *		True when the protection hook is installed in this server, that is,
 *		when the library was loaded through shared_preload_libraries.
 */
Datum
kingres_ledger_hook_active(PG_FUNCTION_ARGS)
{
	PG_RETURN_BOOL(kl_preloaded);
}

/*
 * kingres.ledger_binary_upgrade() returns boolean
 *		True in a server that pg_upgrade started for the restore of a schema
 *		(binary upgrade mode).  The event triggers of the extension are created
 *		one at a time there, and the first one would see the others missing.
 */
Datum
kingres_ledger_binary_upgrade(PG_FUNCTION_ARGS)
{
	PG_RETURN_BOOL(IsBinaryUpgrade);
}

/*
 * kingres.ledger_outer_user() returns name
 *		The current user as seen outside any SECURITY DEFINER function.
 *		The SQL orchestration functions run as the extension owner; they use
 *		this to check privileges of the role that called them.
 */
Datum
kingres_ledger_outer_user(PG_FUNCTION_ARGS)
{
	Name		result = (Name) palloc0(NAMEDATALEN);

	namestrcpy(result, GetUserNameFromId(GetOuterUserId(), false));
	PG_RETURN_NAME(result);
}

/*
 * kl_check_owner
 *		Raise the usual "must be owner" error unless the role that called the
 *		SECURITY DEFINER function is the owner of the relation (directly or
 *		through role membership), and "does not exist" when the relation is
 *		gone.
 */
void
kl_check_owner(Oid relid)
{
	if (!SearchSysCacheExists1(RELOID, ObjectIdGetDatum(relid)))
		ereport(ERROR,
				(errcode(ERRCODE_UNDEFINED_TABLE),
				 errmsg("relation with OID %u does not exist", relid)));
	if (!object_ownercheck(RelationRelationId, relid, GetOuterUserId()))
		aclcheck_error(ACLCHECK_NOT_OWNER, get_relkind_objtype(get_rel_relkind(relid)),
					   get_rel_name(relid));
}

/*
 * kingres.ledger_lock_table(relation regclass) returns void
 *		Lock a table in SHARE ROW EXCLUSIVE mode, by OID, on behalf of the role
 *		that called ledger_enable.  The caller must own the table.  Locking by
 *		OID, before anything else is read, means the table cannot be renamed or
 *		changed under ledger_enable, and no name has to be resolved again.
 *		The ownership is checked before the lock is requested, so a role that
 *		does not own the table cannot make anybody wait, and again after the
 *		lock is granted, because the owner may have changed while waiting.
 *		Then it takes the lock of the registry (see below), under which
 *		ledger_enable takes the next identifier of the table.
 */
Datum
kingres_ledger_lock_table(PG_FUNCTION_ARGS)
{
	Oid			relid = PG_GETARG_OID(0);

	kl_require_own_statement("ledger_enable");
	kl_check_owner(relid);
	/*
	 * The order of the locks is checked before the wait for the table (L19-04):
	 * a caller that holds a later lock of the extension and then waits for a
	 * table that another session holds, while that session waits for the later
	 * lock, would be a deadlock; the call is refused instead.
	 */
	kl_check_lock_order(KL_REGISTRY_LOCK_SUBID);
	LockRelationOid(relid, ShareRowExclusiveLock);
	kl_check_owner(relid);

	/*
	 * The identifier of the table is the next free one of the registry, taken
	 * under this lock (ledger_enable, and ledger_register_table which checks
	 * it); no sequence is read.  The lock comes after the table lock, never
	 * before: a role that holds a lock on its own table and calls
	 * ledger_enable from another session would otherwise hold this lock while
	 * it waits for itself.
	 */
	kl_lock_extension_object(KL_REGISTRY_LOCK_SUBID);
	PG_RETURN_VOID();
}
