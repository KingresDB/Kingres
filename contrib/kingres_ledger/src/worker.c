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
 * Modelled on the logical replication launcher and on autoprewarm.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: PostgreSQL
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

#include "kingres_ledger.h"

/* Every this many rounds the launcher visits all databases (see above). */
#define KL_SWEEP_ROUNDS 6

/* Size of the dirty set; when it is full the next round visits everything. */
#define KL_MAX_DIRTY 128

/* The longest the worker waits for the builder lock, in milliseconds. */
#define KL_LOCK_WAIT_MS 1000

typedef struct KlShared
{
	slock_t		mutex;
	bool		sweep_all;		/* visit every database in the next round */
	int			ndirty;
	Oid			dirty[KL_MAX_DIRTY];
} KlShared;

static KlShared *kl_shared = NULL;
static shmem_request_hook_type prev_shmem_request_hook = NULL;
static shmem_startup_hook_type prev_shmem_startup_hook = NULL;

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
		SpinLockInit(&kl_shared->mutex);
		/* After a start every database is visited once. */
		kl_shared->sweep_all = true;
		kl_shared->ndirty = 0;
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
 * kl_connectable_databases
 *		The OIDs of the databases a worker can connect to.  Runs in its own
 *		transaction; the result is allocated in the caller's context.
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
	return result;
}

/*
 * kl_run_worker
 *		Start a worker for one database and wait until it has finished.
 *		Returns false when no worker could be started.
 */
static bool
kl_run_worker(Oid dboid)
{
	BackgroundWorker worker;
	BackgroundWorkerHandle *handle;
	BgwHandleStatus status;
	pid_t		pid;

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
		ereport(LOG,
				(errmsg("kingres_ledger: no background worker slot free for database %u, will retry",
						dboid),
				 errhint("Increase max_worker_processes.")));
		return false;
	}
	status = WaitForBackgroundWorkerStartup(handle, &pid);
	if (status == BGWH_POSTMASTER_DIED)
		proc_exit(1);
	if (status == BGWH_STARTED)
	{
		status = WaitForBackgroundWorkerShutdown(handle);
		if (status == BGWH_POSTMASTER_DIED)
			proc_exit(1);
	}
	return true;
}

/*
 * kingres_ledger_launcher_main
 *		Main loop of the launcher.
 */
void
kingres_ledger_launcher_main(Datum main_arg)
{
	int			round = 0;

	pqsignal(SIGHUP, SignalHandlerForConfigReload);
	pqsignal(SIGTERM, die);
	BackgroundWorkerUnblockSignals();

	/* No database: the launcher only reads the shared catalog pg_database. */
	BackgroundWorkerInitializeConnection(NULL, NULL, 0);
	pgstat_report_appname("kingres_ledger launcher");

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

		if (kl_block_interval > 0 && !RecoveryInProgress() && kl_shared != NULL)
		{
			Oid			dirty[KL_MAX_DIRTY];
			int			ndirty;
			bool		sweep;
			List	   *targets = NIL;
			ListCell   *lc;

			SpinLockAcquire(&kl_shared->mutex);
			sweep = kl_shared->sweep_all || (round % KL_SWEEP_ROUNDS == 0);
			ndirty = kl_shared->ndirty;
			memcpy(dirty, kl_shared->dirty, sizeof(Oid) * ndirty);
			kl_shared->sweep_all = false;
			kl_shared->ndirty = 0;
			SpinLockRelease(&kl_shared->mutex);
			round++;

			if (sweep || ndirty > 0)
			{
				List	   *all = kl_connectable_databases();

				if (sweep)
					targets = all;
				else
				{
					int			i;

					for (i = 0; i < ndirty; i++)
						if (list_member_oid(all, dirty[i]))
							targets = lappend_oid(targets, dirty[i]);
				}
			}

			foreach(lc, targets)
			{
				CHECK_FOR_INTERRUPTS();
				if (!kl_run_worker(lfirst_oid(lc)))
				{
					/* Try this one and the rest again in the next round. */
					for (; lc != NULL; lc = lnext(targets, lc))
						kl_mark_dirty(lfirst_oid(lc));
					break;
				}
			}
			list_free(targets);
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
 * kingres_ledger_worker_main
 *		The worker of one database: build one block if transactions are
 *		pending.
 */
void
kingres_ledger_worker_main(Datum main_arg)
{
	Oid			dboid = DatumGetObjectId(main_arg);
	Oid			owner;
	char		buf[32];
	volatile bool retry = false;

	pqsignal(SIGTERM, die);
	BackgroundWorkerUnblockSignals();

	BackgroundWorkerInitializeConnectionByOid(dboid, InvalidOid, 0);
	if (RecoveryInProgress())
		proc_exit(0);
	pgstat_report_appname("kingres_ledger worker");

	/*
	 * The builder needs READ COMMITTED whatever the defaults of the database
	 * or the role are, and must not wait long for its lock.
	 */
	SetConfigOption("default_transaction_isolation", "read committed",
					PGC_SUSET, PGC_S_OVERRIDE);
	snprintf(buf, sizeof(buf), "%d",
			 kl_block_interval > 0 && kl_block_interval < KL_LOCK_WAIT_MS ?
			 kl_block_interval : KL_LOCK_WAIT_MS);
	SetConfigOption("lock_timeout", buf, PGC_SUSET, PGC_S_OVERRIDE);
	SetConfigOption("statement_timeout", "0", PGC_SUSET, PGC_S_OVERRIDE);

	SetCurrentStatementStartTimestamp();
	StartTransactionCommand();

	owner = kl_extension_owner_oid();
	if (!OidIsValid(owner))
	{
		/* The extension is not installed here: nothing to do. */
		CommitTransactionCommand();
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

		/* CopyErrorData must not run in ErrorContext. */
		MemoryContextSwitchTo(TopMemoryContext);
		edata = CopyErrorData();
		if (edata->sqlerrcode == ERRCODE_LOCK_NOT_AVAILABLE)
		{
			/* Another builder holds the lock: leave the work to it. */
			FlushErrorState();
			FreeErrorData(edata);
			AbortCurrentTransaction();
			ereport(LOG,
					(errmsg("kingres_ledger: the block builder is busy in database %u, will retry",
							dboid)));
			kl_mark_dirty(dboid);
			proc_exit(0);
		}
		FreeErrorData(edata);
		kl_mark_dirty(dboid);
		PG_RE_THROW();
	}
	PG_END_TRY();

	if (retry)
		kl_mark_dirty(dboid);
	proc_exit(0);
}
