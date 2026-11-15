/*-------------------------------------------------------------------------
 *
 * worker.c
 *	  The block-building background workers of the ledger: one launcher that
 *	  runs for the whole server, and a short-lived worker per database.
 *
 * The launcher is registered at library load (it needs shared_preload_libraries).
 * Every kingres_ledger.block_interval (default 10 seconds; 0 switches the
 * workers off) it decides which databases to look at, and for each of them
 * starts a worker, waits for it to finish and goes on to the next one, so at
 * most one worker of the ledger exists at a time.  A worker connects to its
 * database as the owner of the extension, does nothing when the extension is
 * not installed or no transaction is pending, and otherwise calls
 * kingres.ledger_build_block() once and exits.  It does nothing on a standby
 * (the launcher is only started when recovery has finished, and the worker
 * checks again), and it never waits long for the builder lock: it sets a
 * lock timeout of one second (or the interval, if that is shorter), gives up
 * quietly and leaves the work to the next round.
 *
 * Which databases.  A launcher cannot look into a database, so it keeps a
 * small set of "dirty" databases in shared memory: a backend adds its database
 * to the set when a transaction that wrote ledger entries commits.  The
 * launcher visits the dirty databases only, so an idle cluster with many
 * databases costs nothing.  The set is not a source of truth: it is lost at
 * a restart, a transaction that was prepared and is committed later does not
 * add to it, and the set can overflow.  Therefore the launcher visits every
 * connectable database in its first round after the server starts (or is
 * promoted) and again every KL_SWEEP_ROUNDS rounds, and a worker puts its
 * database back into the set when it could not finish or when pending
 * transactions remain after its block.  A committed ledger transaction is in a
 * block within about one interval; a prepared transaction that is committed
 * later within KL_SWEEP_ROUNDS intervals.
 *
 * What a sweep costs, and what is known cheaply.  What the launcher cannot
 * know without connecting is whether the extension is installed in a database
 * and whether work is pending there.  It remembers the outcome of every visit
 * (in shared memory, so that ledger_status can show it): a database where a
 * worker found no extension is left out of the following KL_NOEXT_SWEEPS - 1
 * sweeps (without the extension nothing can become pending, and a database
 * that gets it and a ledger transaction is marked dirty by the commit, which
 * is visited at once); a database whose worker failed is not visited again
 * until a delay has passed that doubles with every failure, up to an hour.
 * The first sweep after a start still connects once to every database.
 *
 * Fairness.  A worker is cancelled by itself after kingres_ledger.worker_timeout
 * (and terminated by the launcher a little later if it does not react), the
 * databases of a sweep are visited in an order that continues where the
 * previous sweep stopped, and a round that has used its time budget leaves the
 * rest of its databases to the next round, so one slow or failing database
 * cannot keep the others waiting for more than about one timeout.
 *
 * What the worker does not trust.  A database owner or a role owner can attach
 * settings to the database or the role (ALTER DATABASE ... SET); the worker
 * connects, and the settings are applied during the connection, before any
 * code of the extension runs.  The worker then overrides everything that can
 * make it fail or stall (kl_harden_settings) with the highest priority
 * before it starts its transaction.  A setting that kills the worker during
 * the connection itself cannot be overridden: such a failure is recorded like
 * any other, with the delay above, and shown in kingres.ledger_status.
 *
 * Modelled on the logical replication launcher and on autoprewarm.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/worker.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/genam.h"
#include "access/heapam.h"
#include "access/htup_details.h"
#include "access/table.h"
#include "access/xact.h"
#include "access/xlog.h"
#include "catalog/pg_database.h"
#include "catalog/pg_extension.h"
#include "commands/extension.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "funcapi.h"
#include "miscadmin.h"
#include "pgstat.h"
#include "postmaster/bgworker.h"
#include "postmaster/interrupt.h"
#include "storage/ipc.h"
#include "storage/latch.h"
#include "storage/proc.h"
#include "storage/shmem.h"
#include "storage/spin.h"
#include "tcop/tcopprot.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/memutils.h"
#include "utils/snapmgr.h"
#include "utils/syscache.h"
#include "utils/timeout.h"
#include "utils/timestamp.h"

#include "kingres_ledger.h"

PG_FUNCTION_INFO_V1(kingres_ledger_worker_state);

/* Every this many rounds the launcher visits all databases (see above). */
#define KL_SWEEP_ROUNDS 6

/* A database without the extension is left out of this many sweeps - 1. */
#define KL_NOEXT_SWEEPS 10

/* Size of the dirty set; when it is full the next round visits everything. */
#define KL_MAX_DIRTY 128

/* Databases whose outcome is remembered; the least useful entry is recycled. */
#define KL_MAX_DB_STATE 256

/* The longest the worker waits for the builder lock, in milliseconds. */
#define KL_LOCK_WAIT_MS 1000

/* The longest delay after a failure, in milliseconds. */
#define KL_MAX_BACKOFF_MS 3600000

/* Time budget of a round when kingres_ledger.worker_timeout is 0. */
#define KL_DEFAULT_ROUND_BUDGET_MS 60000

/* How long after its own timeout a worker is terminated by the launcher. */
#define KL_TERMINATE_GRACE_MS 5000

/* Longest error text kept per database, and room for a SQLSTATE. */
#define KL_ERROR_LEN 200
#define KL_SQLSTATE_LEN 6

/* The outcome of the last visit to a database. */
typedef enum KlDbResult
{
	KL_R_NONE = 0,				/* never visited */
	KL_R_OK,					/* worker ran and finished */
	KL_R_NO_EXTENSION,			/* the extension is not installed there */
	KL_R_BUSY,					/* the builder lock was taken, will retry */
	KL_R_FAILED					/* the worker failed */
} KlDbResult;

typedef struct KlDbState
{
	Oid			dboid;			/* InvalidOid: the slot is free */
	int			result;			/* KlDbResult */
	int			consecutive_failures;
	int			skipped_sweeps;
	int64		visits;			/* workers started for this database */
	int64		failures;		/* failures since the entry exists */
	TimestampTz last_attempt;
	TimestampTz last_success;
	TimestampTz retry_after;	/* no worker before this time (failures) */
	char		last_error[KL_ERROR_LEN];
	char		last_sqlstate[KL_SQLSTATE_LEN];
} KlDbState;

typedef struct KlShared
{
	slock_t		mutex;
	bool		sweep_all;		/* visit every database in the next round */
	int			ndirty;
	Oid			dirty[KL_MAX_DIRTY];
	KlDbState	state[KL_MAX_DB_STATE];
} KlShared;

static KlShared *kl_shared = NULL;
static shmem_request_hook_type prev_shmem_request_hook = NULL;
static shmem_startup_hook_type prev_shmem_startup_hook = NULL;

/* Launcher: databases that did not get their turn in the last round. */
static MemoryContext kl_backlog_cxt = NULL;
static List *kl_backlog = NIL;

/* Launcher: rounds in a row in which no worker slot was free. */
static int	kl_slot_failures = 0;

/* Launcher: a missing worker slot is logged at the first failure and then every so many. */
#define KL_SLOT_LOG_EVERY 60

/* Launcher: the database visited last; the next sweep starts after it. */
static Oid	kl_cursor = InvalidOid;

/* Worker: what the exit callback needs to know. */
static Oid	kl_worker_dboid = InvalidOid;
static bool kl_worker_reported = false;
static volatile sig_atomic_t kl_worker_timed_out = false;
static bool kl_have_error = false;
static char kl_error_text[KL_ERROR_LEN];
static char kl_error_state[KL_SQLSTATE_LEN];
static emit_log_hook_type prev_emit_log_hook = NULL;

PGDLLEXPORT void kingres_ledger_launcher_main(Datum main_arg);
PGDLLEXPORT void kingres_ledger_worker_main(Datum main_arg);

static void
kl_shmem_request(void)
{
	if (prev_shmem_request_hook)
		prev_shmem_request_hook();
	RequestAddinShmemSpace(MAXALIGN(sizeof(KlShared)));
}

static void
kl_shmem_startup(void)
{
	bool		found;

	if (prev_shmem_startup_hook)
		prev_shmem_startup_hook();

	LWLockAcquire(AddinShmemInitLock, LW_EXCLUSIVE);
	kl_shared = ShmemInitStruct("kingres_ledger", sizeof(KlShared), &found);
	if (!found)
	{
		memset(kl_shared, 0, sizeof(KlShared));
		SpinLockInit(&kl_shared->mutex);
		/* After a start every database is visited once. */
		kl_shared->sweep_all = true;
	}
	LWLockRelease(AddinShmemInitLock);
}

/*
 * kl_mark_dirty
 *		Remember that a database may have pending ledger transactions.  Called
 *		from the commit callback of a backend: it must not fail and does not
 *		allocate.
 */
void
kl_mark_dirty(Oid dboid)
{
	int			i;

	if (kl_shared == NULL)
		return;
	SpinLockAcquire(&kl_shared->mutex);
	if (!kl_shared->sweep_all)
	{
		for (i = 0; i < kl_shared->ndirty; i++)
			if (kl_shared->dirty[i] == dboid)
				break;
		if (i == kl_shared->ndirty)
		{
			if (kl_shared->ndirty < KL_MAX_DIRTY)
				kl_shared->dirty[kl_shared->ndirty++] = dboid;
			else
				kl_shared->sweep_all = true;
		}
	}
	SpinLockRelease(&kl_shared->mutex);
}

/*
 * kl_state_slot
 *		The remembered state of a database; with "create", a slot is made (and
 *		an old one recycled when all are in use: first a database that is fine
 *		or has no extension, the one visited longest ago).  The caller holds
 *		the spinlock.
 */
static KlDbState *
kl_state_slot(Oid dboid, bool create)
{
	int			i;
	int			free_slot = -1;
	int			victim = -1;
	int			oldest = 0;
	KlDbState  *s;

	for (i = 0; i < KL_MAX_DB_STATE; i++)
	{
		s = &kl_shared->state[i];
		if (s->dboid == dboid && OidIsValid(dboid))
			return s;
		if (!OidIsValid(s->dboid))
		{
			if (free_slot < 0)
				free_slot = i;
			continue;
		}
		if ((s->result == KL_R_OK || s->result == KL_R_NO_EXTENSION) &&
			(victim < 0 || s->last_attempt < kl_shared->state[victim].last_attempt))
			victim = i;
		if (s->last_attempt < kl_shared->state[oldest].last_attempt)
			oldest = i;
	}
	if (!create)
		return NULL;
	i = free_slot >= 0 ? free_slot : (victim >= 0 ? victim : oldest);
	s = &kl_shared->state[i];
	memset(s, 0, sizeof(KlDbState));
	s->dboid = dboid;
	return s;
}

/* kl_state_visit: a worker is about to be started for the database. */
static void
kl_state_visit(Oid dboid)
{
	KlDbState  *s;

	SpinLockAcquire(&kl_shared->mutex);
	s = kl_state_slot(dboid, true);
	s->visits++;
	s->last_attempt = GetCurrentTimestamp();
	SpinLockRelease(&kl_shared->mutex);
}

/*
 * kl_state_report
 *		Record the outcome of a visit.  Called by the worker (also from its
 *		exit callback, so it only copies and takes the spinlock) and by nobody
 *		else.
 */
static void
kl_state_report(Oid dboid, KlDbResult result, const char *error,
				const char *sqlstate)
{
	KlDbState  *s;
	TimestampTz now = GetCurrentTimestamp();

	if (kl_shared == NULL || !OidIsValid(dboid))
		return;
	SpinLockAcquire(&kl_shared->mutex);
	s = kl_state_slot(dboid, true);
	s->result = result;
	switch (result)
	{
		case KL_R_OK:
			s->consecutive_failures = 0;
			s->last_success = now;
			s->retry_after = 0;
			break;
		case KL_R_NO_EXTENSION:
			s->consecutive_failures = 0;
			s->skipped_sweeps = 0;
			s->retry_after = 0;
			break;
		case KL_R_BUSY:
			break;
		case KL_R_FAILED:
			{
				int64		delay = kl_block_interval > 0 ? kl_block_interval : 10000;
				int			shift = Min(s->consecutive_failures, 12);

				delay = Min(delay << shift, KL_MAX_BACKOFF_MS);
				s->consecutive_failures++;
				s->failures++;
				s->retry_after = TimestampTzPlusMilliseconds(now, delay);
				if (error != NULL)
					strlcpy(s->last_error, error, sizeof(s->last_error));
				if (sqlstate != NULL)
					strlcpy(s->last_sqlstate, sqlstate, sizeof(s->last_sqlstate));
			}
			break;
		default:
			break;
	}
	SpinLockRelease(&kl_shared->mutex);
}

/*
 * kl_state_skip
 *		Whether the launcher leaves a database out of this round: it failed
 *		and its delay has not passed, or (for a sweep) it had no extension at
 *		the last visit and fewer than KL_NOEXT_SWEEPS sweeps have passed.
 *		A database that is dirty is never left out for lack of an extension.
 */
static bool
kl_state_skip(Oid dboid, bool dirty)
{
	KlDbState  *s;
	bool		skip = false;
	TimestampTz now = GetCurrentTimestamp();

	SpinLockAcquire(&kl_shared->mutex);
	s = kl_state_slot(dboid, false);
	if (s != NULL)
	{
		if (s->result == KL_R_FAILED && s->retry_after > now)
			skip = true;
		else if (s->result == KL_R_NO_EXTENSION && !dirty)
		{
			if (++s->skipped_sweeps < KL_NOEXT_SWEEPS)
				skip = true;
			else
				s->skipped_sweeps = 0;
		}
	}
	SpinLockRelease(&kl_shared->mutex);
	return skip;
}

/*
 * kl_state_prune
 *		Forget databases that no longer exist (or cannot be connected to).
 */
static void
kl_state_prune(List *all)
{
	int			i;

	for (i = 0; i < KL_MAX_DB_STATE; i++)
	{
		Oid			oid;

		SpinLockAcquire(&kl_shared->mutex);
		oid = kl_shared->state[i].dboid;
		SpinLockRelease(&kl_shared->mutex);
		if (OidIsValid(oid) && !list_member_oid(all, oid))
		{
			SpinLockAcquire(&kl_shared->mutex);
			if (kl_shared->state[i].dboid == oid)
				kl_shared->state[i].dboid = InvalidOid;
			SpinLockRelease(&kl_shared->mutex);
		}
	}
}

/*
 * kl_worker_init
 *		Request the shared memory and register the launcher.  Called from
 *		_PG_init when the library is preloaded.
 */
void
kl_worker_init(void)
{
	BackgroundWorker worker;

	prev_shmem_request_hook = shmem_request_hook;
	shmem_request_hook = kl_shmem_request;
	prev_shmem_startup_hook = shmem_startup_hook;
	shmem_startup_hook = kl_shmem_startup;

	memset(&worker, 0, sizeof(worker));
	worker.bgw_flags = BGWORKER_SHMEM_ACCESS | BGWORKER_BACKEND_DATABASE_CONNECTION;
	worker.bgw_start_time = BgWorkerStart_RecoveryFinished;
	worker.bgw_restart_time = 10;
	snprintf(worker.bgw_library_name, BGW_MAXLEN, "kingres_ledger");
	snprintf(worker.bgw_function_name, BGW_MAXLEN, "kingres_ledger_launcher_main");
	snprintf(worker.bgw_name, BGW_MAXLEN, "kingres_ledger launcher");
	snprintf(worker.bgw_type, BGW_MAXLEN, "kingres_ledger launcher");
	RegisterBackgroundWorker(&worker);
}

/*
 * kl_harden_settings
 *		Override, with the highest priority, every setting that a database
 *		owner or a role owner can attach to the database or to the role and
 *		that can make the worker fail, stall or run other code than the one it
 *		is meant to run.  Called by the worker (and the launcher) after the
 *		connection and before the first transaction; the settings of the
 *		database and the role were applied during the connection.  The list was
 *		made by reading the settings of guc_tables.c that an ordinary role can
 *		set (USERSET) or that are applied at the start of a transaction:
 *
 *		  - timeouts that end a session or a transaction: transaction_timeout,
 *			idle_in_transaction_session_timeout, idle_session_timeout; and
 *			statement_timeout (not armed in a worker, but 0 anyway);
 *		  - the mode of the transaction: default_transaction_isolation (the
 *			builder needs READ COMMITTED), default_transaction_read_only,
 *			default_transaction_deferrable, transaction_read_only;
 *		  - what code can see and do: search_path, session_replication_role,
 *			row_security, check_function_bodies, standard_conforming_strings
 *			(the SQL of the extension has no backslash in a literal, but the
 *			setting is fixed all the same), the settings of PL/pgSQL that
 *			turn a harmless construct into an error or change how a name is
 *			resolved (plpgsql.variable_conflict, plpgsql.extra_errors,
 *			plpgsql.extra_warnings);
 *		  - things that fail for reasons outside the ledger: jit (a missing
 *			or broken provider), debug_parallel_query, temp_tablespaces (a
 *			tablespace that was dropped or has no space).
 *
 *		Not overridden: kingres_ledger.max_block_transactions (superusers
 *		only, and a database setting of it is how an administrator bounds the
 *		blocks of a database), synchronous_commit and work_mem (they change
 *		speed, not outcome; a worker that waits for a synchronous standby is
 *		cancelled by the worker timeout).
 */
static void
kl_harden_settings(void)
{
	static const char *const settings[][2] = {
		{"transaction_timeout", "0"},
		{"idle_in_transaction_session_timeout", "0"},
		{"idle_session_timeout", "0"},
		{"statement_timeout", "0"},
		{"default_transaction_isolation", "read committed"},
		{"default_transaction_read_only", "off"},
		{"default_transaction_deferrable", "off"},
		{"transaction_read_only", "off"},
		{"search_path", "pg_catalog"},
		{"session_replication_role", "origin"},
		{"row_security", "off"},
		{"check_function_bodies", "on"},
		{"standard_conforming_strings", "on"},
		{"plpgsql.variable_conflict", "error"},
		{"plpgsql.extra_errors", "none"},
		{"plpgsql.extra_warnings", "none"},
		{"jit", "off"},
		{"debug_parallel_query", "off"},
		{"temp_tablespaces", ""}
	};
	int			i;
	char		buf[32];

	for (i = 0; i < lengthof(settings); i++)
		SetConfigOption(settings[i][0], settings[i][1], PGC_SUSET, PGC_S_OVERRIDE);

	snprintf(buf, sizeof(buf), "%d",
			 kl_block_interval > 0 && kl_block_interval < KL_LOCK_WAIT_MS ?
			 kl_block_interval : KL_LOCK_WAIT_MS);
	SetConfigOption("lock_timeout", buf, PGC_SUSET, PGC_S_OVERRIDE);
}

/*
 * kl_connectable_databases
 *		The OIDs of the databases a worker can connect to, sorted.  Runs in its
 *		own transaction; the result is allocated in the current memory context
 *		(the caller's round context, which it resets).
 */
static List *
kl_connectable_databases(void)
{
	List	   *result = NIL;
	MemoryContext cxt = CurrentMemoryContext;
	Relation	rel;
	TableScanDesc scan;
	HeapTuple	tup;

	StartTransactionCommand();
	(void) GetTransactionSnapshot();
	rel = table_open(DatabaseRelationId, AccessShareLock);
	scan = table_beginscan_catalog(rel, 0, NULL);
	while (HeapTupleIsValid(tup = heap_getnext(scan, ForwardScanDirection)))
	{
		Form_pg_database db = (Form_pg_database) GETSTRUCT(tup);
		MemoryContext old;

		if (!db->datallowconn || db->datistemplate)
			continue;
		old = MemoryContextSwitchTo(cxt);
		result = lappend_oid(result, db->oid);
		MemoryContextSwitchTo(old);
	}
	table_endscan(scan);
	table_close(rel, AccessShareLock);
	CommitTransactionCommand();
	list_sort(result, list_oid_cmp);
	return result;
}

/*
 * kl_run_worker
 *		Start a worker for one database and wait until it has finished, for as
 *		long as kingres_ledger.worker_timeout plus a short grace allows; then
 *		the worker is terminated.  Returns false when no worker could be
 *		started.
 */
static bool
kl_run_worker(Oid dboid)
{
	BackgroundWorker worker;
	BackgroundWorkerHandle *handle;
	BgwHandleStatus status;
	pid_t		pid;
	bool		terminated = false;
	TimestampTz deadline = 0;

	memset(&worker, 0, sizeof(worker));
	worker.bgw_flags = BGWORKER_SHMEM_ACCESS | BGWORKER_BACKEND_DATABASE_CONNECTION;
	worker.bgw_start_time = BgWorkerStart_RecoveryFinished;
	worker.bgw_restart_time = BGW_NEVER_RESTART;
	snprintf(worker.bgw_library_name, BGW_MAXLEN, "kingres_ledger");
	snprintf(worker.bgw_function_name, BGW_MAXLEN, "kingres_ledger_worker_main");
	snprintf(worker.bgw_name, BGW_MAXLEN, "kingres_ledger block worker for database %u", dboid);
	snprintf(worker.bgw_type, BGW_MAXLEN, "kingres_ledger worker");
	worker.bgw_main_arg = ObjectIdGetDatum(dboid);
	worker.bgw_notify_pid = MyProcPid;

	if (!RegisterDynamicBackgroundWorker(&worker, &handle))
	{
		/*
		 * Every round fails the same way while max_worker_processes is used
		 * up; say it the first time and then once in a while, not at every
		 * interval for ever.
		 */
		if (kl_slot_failures++ % KL_SLOT_LOG_EVERY == 0)
			ereport(LOG,
					(errmsg("kingres_ledger: no background worker slot free for database %u, will retry",
							dboid),
					 errdetail("This message is repeated every %d rounds while it lasts.",
							   KL_SLOT_LOG_EVERY),
					 errhint("Increase max_worker_processes.")));
		return false;
	}
	kl_slot_failures = 0;
	kl_state_visit(dboid);
	if (kl_worker_timeout > 0)
		deadline = TimestampTzPlusMilliseconds(GetCurrentTimestamp(),
											   (int64) kl_worker_timeout + KL_TERMINATE_GRACE_MS);

	status = WaitForBackgroundWorkerStartup(handle, &pid);
	if (status == BGWH_POSTMASTER_DIED)
		proc_exit(1);
	if (status == BGWH_STARTED)
	{
		for (;;)
		{
			status = GetBackgroundWorkerPid(handle, &pid);
			if (status == BGWH_STOPPED)
				break;
			if (status == BGWH_POSTMASTER_DIED)
				proc_exit(1);
			CHECK_FOR_INTERRUPTS();
			if (deadline != 0 && !terminated && GetCurrentTimestamp() >= deadline)
			{
				ereport(LOG,
						(errmsg("kingres_ledger: the worker for database %u did not stop within kingres_ledger.worker_timeout, terminating it",
								dboid)));
				TerminateBackgroundWorker(handle);
				terminated = true;
			}
			(void) WaitLatch(MyLatch,
							 WL_LATCH_SET | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH,
							 200, PG_WAIT_EXTENSION);
			ResetLatch(MyLatch);
		}
	}
	/* The handle is allocated in TopMemoryContext by the postmaster API. */
	pfree(handle);
	return true;
}

/*
 * kl_collect_targets
 *		The databases this round visits, in the order of visit: first those
 *		that did not get their turn in the last round, then the dirty ones,
 *		then, in a sweep, all the others starting after the database visited
 *		last.  Databases that are in their delay (or, in a sweep, have no
 *		extension) are left out; a dirty one that is left out is marked dirty
 *		again so that it comes back.  Allocated in the current context.
 */
static List *
kl_collect_targets(List *all, const Oid *dirty, int ndirty, bool sweep)
{
	List	   *targets = NIL;
	ListCell   *lc;
	int			i;

	foreach(lc, kl_backlog)
	{
		Oid			oid = lfirst_oid(lc);

		if (list_member_oid(all, oid))
			targets = list_append_unique_oid(targets, oid);
	}
	for (i = 0; i < ndirty; i++)
	{
		if (!list_member_oid(all, dirty[i]))
			continue;
		if (kl_state_skip(dirty[i], true))
		{
			kl_mark_dirty(dirty[i]);
			continue;
		}
		targets = list_append_unique_oid(targets, dirty[i]);
	}
	if (sweep)
	{
		/* After the cursor first, then the databases up to it. */
		foreach(lc, all)
		{
			Oid			oid = lfirst_oid(lc);

			if (oid > kl_cursor && !kl_state_skip(oid, false))
				targets = list_append_unique_oid(targets, oid);
		}
		foreach(lc, all)
		{
			Oid			oid = lfirst_oid(lc);

			if (oid <= kl_cursor && !kl_state_skip(oid, false))
				targets = list_append_unique_oid(targets, oid);
		}
	}
	return targets;
}

/*
 * kl_keep_backlog
 *		Remember the databases from "from" on for the next round.
 */
static void
kl_keep_backlog(List *targets, ListCell *from)
{
	MemoryContext old;

	MemoryContextReset(kl_backlog_cxt);
	kl_backlog = NIL;
	old = MemoryContextSwitchTo(kl_backlog_cxt);
	for (; from != NULL; from = lnext(targets, from))
		kl_backlog = lappend_oid(kl_backlog, lfirst_oid(from));
	MemoryContextSwitchTo(old);
}

/*
 * kingres_ledger_launcher_main
 *		Main loop of the launcher.
 */
void
kingres_ledger_launcher_main(Datum main_arg)
{
	int			round = 0;
	MemoryContext round_cxt;

	pqsignal(SIGHUP, SignalHandlerForConfigReload);
	pqsignal(SIGTERM, die);
	BackgroundWorkerUnblockSignals();

	/* No database: the launcher only reads the shared catalog pg_database. */
	BackgroundWorkerInitializeConnection(NULL, NULL, 0);
	pgstat_report_appname("kingres_ledger launcher");
	kl_harden_settings();

	/*
	 * Everything a round allocates is in round_cxt, which is reset at the
	 * start of the next one; what must outlive a round is the backlog.
	 */
	round_cxt = AllocSetContextCreate(TopMemoryContext,
									  "kingres_ledger launcher round",
									  ALLOCSET_DEFAULT_SIZES);
	kl_backlog_cxt = AllocSetContextCreate(TopMemoryContext,
										   "kingres_ledger launcher backlog",
										   ALLOCSET_SMALL_SIZES);

	for (;;)
	{
		long		timeout = -1;

		ResetLatch(MyLatch);
		CHECK_FOR_INTERRUPTS();
		if (ConfigReloadPending)
		{
			ConfigReloadPending = false;
			ProcessConfigFile(PGC_SIGHUP);
		}
		MemoryContextReset(round_cxt);

		if (kl_block_interval > 0 && !RecoveryInProgress() && kl_shared != NULL)
		{
			Oid			dirty[KL_MAX_DIRTY];
			int			ndirty;
			bool		sweep;
			List	   *targets = NIL;
			ListCell   *lc;
			MemoryContext old;
			TimestampTz started = GetCurrentTimestamp();
			long		budget = kl_worker_timeout > 0 ?
				kl_worker_timeout : KL_DEFAULT_ROUND_BUDGET_MS;

			SpinLockAcquire(&kl_shared->mutex);
			sweep = kl_shared->sweep_all || (round % KL_SWEEP_ROUNDS == 0);
			ndirty = kl_shared->ndirty;
			memcpy(dirty, kl_shared->dirty, sizeof(Oid) * ndirty);
			kl_shared->sweep_all = false;
			kl_shared->ndirty = 0;
			SpinLockRelease(&kl_shared->mutex);
			round++;

			if (sweep || ndirty > 0 || kl_backlog != NIL)
			{
				List	   *all;

				old = MemoryContextSwitchTo(round_cxt);
				all = kl_connectable_databases();
				if (sweep)
					kl_state_prune(all);
				targets = kl_collect_targets(all, dirty, ndirty, sweep);
				MemoryContextSwitchTo(old);
			}

			/* The backlog is consumed: what is not done is kept again. */
			kl_keep_backlog(NIL, NULL);

			foreach(lc, targets)
			{
				CHECK_FOR_INTERRUPTS();
				kl_cursor = lfirst_oid(lc);
				if (!kl_run_worker(lfirst_oid(lc)))
				{
					/* Try this one and the rest again in the next round. */
					kl_keep_backlog(targets, lc);
					break;
				}
				if (lnext(targets, lc) != NULL &&
					TimestampDifferenceMilliseconds(started, GetCurrentTimestamp()) >= budget)
				{
					/* The time of the round is used; the others wait. */
					kl_keep_backlog(targets, lnext(targets, lc));
					break;
				}
			}
		}

		if (kl_block_interval > 0)
			timeout = kl_block_interval;
		(void) WaitLatch(MyLatch,
						 WL_LATCH_SET | WL_EXIT_ON_PM_DEATH |
						 (timeout >= 0 ? WL_TIMEOUT : 0),
						 timeout, PG_WAIT_EXTENSION);
	}
}

/*
 * kl_extension_owner_oid
 *		Owner of the extension in the current database, or InvalidOid when it
 *		is not installed.  Needs a transaction.
 */
static Oid
kl_extension_owner_oid(void)
{
	Oid			extoid = get_extension_oid(KL_EXTENSION_NAME, true);
	HeapTuple	tup;
	Oid			owner;

	if (!OidIsValid(extoid))
		return InvalidOid;
	tup = SearchSysCache1(EXTENSIONOID, ObjectIdGetDatum(extoid));
	if (!HeapTupleIsValid(tup))
		return InvalidOid;
	owner = ((Form_pg_extension) GETSTRUCT(tup))->extowner;
	ReleaseSysCache(tup);
	return owner;
}

/*
 * kl_pending_exists
 *		Whether kingres.ledger_pending has a row.
 */
static bool
kl_pending_exists(void)
{
	int			rc = SPI_execute("SELECT 1 FROM kingres.ledger_pending LIMIT 1",
								 false, 1);

	if (rc != SPI_OK_SELECT)
		elog(ERROR, "could not look at kingres.ledger_pending: %s",
			 SPI_result_code_string(rc));
	return SPI_processed > 0;
}

/*
 * kl_emit_log
 *		Remember the first error of this worker, for the state that the exit
 *		callback records (a FATAL error, such as a database setting that kills
 *		the session, never reaches a PG_CATCH).
 */
static void
kl_emit_log(ErrorData *edata)
{
	if (prev_emit_log_hook)
		prev_emit_log_hook(edata);
	if (edata->elevel >= ERROR && !kl_have_error)
	{
		kl_have_error = true;
		strlcpy(kl_error_text, edata->message != NULL ? edata->message : "",
				sizeof(kl_error_text));
		strlcpy(kl_error_state, unpack_sql_state(edata->sqlerrcode),
				sizeof(kl_error_state));
	}
}

/*
 * kl_worker_exit
 *		Exit callback of a worker: when it ended without having reported its
 *		outcome, it failed.  Only copies and takes a spinlock.
 */
static void
kl_worker_exit(int code, Datum arg)
{
	if (kl_worker_reported)
		return;
	kl_worker_reported = true;
	kl_state_report(kl_worker_dboid, KL_R_FAILED,
					kl_have_error ? kl_error_text : "the worker stopped before it finished",
					kl_have_error ? kl_error_state : "");
}

/*
 * kl_worker_timeout_handler
 *		Timeout handler of the worker's own time limit: cancel what it runs,
 *		which raises "canceling statement" at the next interrupt check.
 */
static void
kl_worker_timeout_handler(void)
{
	kl_worker_timed_out = true;
	QueryCancelPending = true;
	InterruptPending = true;
	SetLatch(MyLatch);
}

/*
 * kl_worker_report
 *		Report the outcome once.
 */
static void
kl_worker_report(KlDbResult result, const char *error, const char *sqlstate)
{
	kl_worker_reported = true;
	kl_state_report(kl_worker_dboid, result, error, sqlstate);
}

/*
 * kingres_ledger_worker_main
 *		The worker of one database: build one block if transactions are
 *		pending.
 */
void
kingres_ledger_worker_main(Datum main_arg)
{
	Oid			dboid = DatumGetObjectId(main_arg);
	Oid			owner;
	volatile bool retry = false;

	kl_worker_dboid = dboid;
	pqsignal(SIGTERM, die);
	BackgroundWorkerUnblockSignals();

	prev_emit_log_hook = emit_log_hook;
	emit_log_hook = kl_emit_log;
	on_shmem_exit(kl_worker_exit, (Datum) 0);

	BackgroundWorkerInitializeConnectionByOid(dboid, InvalidOid, 0);
	if (RecoveryInProgress())
	{
		kl_worker_reported = true;
		proc_exit(0);
	}
	pgstat_report_appname("kingres_ledger worker");

	/*
	 * The settings of the database and of the role were applied by the
	 * connection; nothing of them may reach the transaction below.
	 */
	kl_harden_settings();
	RegisterTimeout(USER_TIMEOUT, kl_worker_timeout_handler);

	SetCurrentStatementStartTimestamp();
	StartTransactionCommand();
	if (kl_worker_timeout > 0)
		enable_timeout_after(USER_TIMEOUT, kl_worker_timeout);

	owner = kl_extension_owner_oid();
	if (!OidIsValid(owner))
	{
		/* The extension is not installed here: nothing to do. */
		CommitTransactionCommand();
		kl_worker_report(KL_R_NO_EXTENSION, NULL, NULL);
		proc_exit(0);
	}
	SetUserIdAndSecContext(owner, 0);

	PG_TRY();
	{
		if (SPI_connect() != SPI_OK_CONNECT)
			elog(ERROR, "SPI_connect failed");
		PushActiveSnapshot(GetTransactionSnapshot());

		if (kl_pending_exists())
		{
			int			rc = SPI_execute("SELECT kingres.ledger_build_block()",
										 false, 0);

			if (rc != SPI_OK_SELECT)
				elog(ERROR, "kingres.ledger_build_block failed: %s",
					 SPI_result_code_string(rc));
			/* More than one block's worth was pending: come back. */
			retry = kl_pending_exists();
		}
		SPI_finish();
		PopActiveSnapshot();
		CommitTransactionCommand();
	}
	PG_CATCH();
	{
		ErrorData  *edata;
		bool		busy;
		char		message[KL_ERROR_LEN];
		char		sqlstate[KL_SQLSTATE_LEN];

		/* CopyErrorData must not run in ErrorContext. */
		MemoryContextSwitchTo(TopMemoryContext);
		edata = CopyErrorData();
		busy = (edata->sqlerrcode == ERRCODE_LOCK_NOT_AVAILABLE);
		strlcpy(message, kl_worker_timed_out ?
				"the worker ran longer than kingres_ledger.worker_timeout" :
				(edata->message != NULL ? edata->message : ""),
				sizeof(message));
		strlcpy(sqlstate, unpack_sql_state(edata->sqlerrcode), sizeof(sqlstate));
		FlushErrorState();
		FreeErrorData(edata);
		AbortCurrentTransaction();
		kl_mark_dirty(dboid);

		if (busy)
		{
			/* Another builder holds the lock: leave the work to it. */
			ereport(LOG,
					(errmsg("kingres_ledger: the block builder is busy in database %u, will retry",
							dboid)));
			kl_worker_report(KL_R_BUSY, NULL, NULL);
		}
		else
		{
			/*
			 * One line, and the launcher's delay before the next attempt: a
			 * database that cannot be served does not fill the log.
			 */
			ereport(LOG,
					(errmsg("kingres_ledger: the block worker for database %u failed: %s",
							dboid, message),
					 errdetail("SQLSTATE %s. The next attempt follows after a delay that grows with every failure; see kingres.ledger_status.",
							   sqlstate)));
			kl_worker_report(KL_R_FAILED, message, sqlstate);
		}
		proc_exit(0);
	}
	PG_END_TRY();

	disable_timeout(USER_TIMEOUT, false);
	if (retry)
		kl_mark_dirty(dboid);
	kl_worker_report(KL_R_OK, NULL, NULL);
	proc_exit(0);
}

/*
 * kingres.ledger_worker_state(OUT ...) returns record
 *		What the launcher and the workers know about the current database:
 *		the outcome of the last visit, how many workers were started for it,
 *		how many failed, when, and the last error.  Read by the view
 *		ledger_status; not executable by PUBLIC (the error text can name
 *		settings).
 */
Datum
kingres_ledger_worker_state(PG_FUNCTION_ARGS)
{
	TupleDesc	tupdesc;
	Datum		values[8];
	bool		nulls[8];
	KlDbState	copy;
	bool		found = false;
	const char *state;

	if (get_call_result_type(fcinfo, NULL, &tupdesc) != TYPEFUNC_COMPOSITE)
		elog(ERROR, "return type must be a row type");
	memset(&copy, 0, sizeof(copy));
	if (kl_shared != NULL)
	{
		KlDbState  *s;

		SpinLockAcquire(&kl_shared->mutex);
		s = kl_state_slot(MyDatabaseId, false);
		if (s != NULL)
		{
			copy = *s;
			found = true;
		}
		SpinLockRelease(&kl_shared->mutex);
	}

	memset(nulls, true, sizeof(nulls));
	if (kl_shared == NULL)
		state = "not_running";
	else if (!found)
		state = "never_visited";
	else
		switch (copy.result)
		{
			case KL_R_OK:
				state = "ok";
				break;
			case KL_R_NO_EXTENSION:
				state = "no_extension";
				break;
			case KL_R_BUSY:
				state = "busy";
				break;
			case KL_R_FAILED:
				state = "failed";
				break;
			default:
				state = "never_visited";
				break;
		}
	values[0] = CStringGetTextDatum(state);
	nulls[0] = false;
	if (found)
	{
		values[1] = Int64GetDatum(copy.visits);
		values[2] = Int64GetDatum(copy.failures);
		values[3] = Int32GetDatum(copy.consecutive_failures);
		nulls[1] = nulls[2] = nulls[3] = false;
		if (copy.last_attempt != 0)
		{
			values[4] = TimestampTzGetDatum(copy.last_attempt);
			nulls[4] = false;
		}
		if (copy.last_success != 0)
		{
			values[5] = TimestampTzGetDatum(copy.last_success);
			nulls[5] = false;
		}
		if (copy.result == KL_R_FAILED && copy.retry_after != 0)
		{
			values[6] = TimestampTzGetDatum(copy.retry_after);
			nulls[6] = false;
		}
		if (copy.last_error[0] != '\0')
		{
			values[7] = CStringGetTextDatum(copy.last_error);
			nulls[7] = false;
		}
	}
	PG_RETURN_DATUM(HeapTupleGetDatum(heap_form_tuple(tupdesc, values, nulls)));
}
