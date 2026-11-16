/*-------------------------------------------------------------------------
 *
 * ha.c
 *	  The parts of the high-availability functions that need shared memory:
 *
 *	  - The restore-point barrier (kingres.create_restore_point).  Every
 *		transaction that commits with two-phase commit counts itself in from
 *		before its first PREPARE TRANSACTION until its second phase is over.
 *		While the barrier of a node is up, no new transaction counts in (it
 *		waits), so once the count reaches zero it stays zero, and every
 *		distributed transaction is either completely before the restore point
 *		of this node or completely after it.  The barrier expires by itself, so
 *		that a coordinator that died cannot stop commits for ever.
 *
 *	  - The marks of distributed-deadlock victims.  The detector cancels the
 *		session that waits for a lock; a cancel is reported as "canceling
 *		statement due to user request" (57014), which a client cannot tell from
 *		a cancel of its own.  A backend that was marked reports 40P01 and says
 *		why instead: the error is rewritten in emit_log_hook, which runs for
 *		every error before it is sent to the client.  The hook is skipped only
 *		when the error is not reported to the server log at all (log_min_messages
 *		above "error"): the default "warning"
 *		works; the victim still ends, with the plain cancel error.  A mark
 *		belongs to the wait it was made for (S7-10).
 *
 *	  - The identity of the distributed transaction that a remote session
 *		works for, one slot per backend, written only by a session that proved
 *		to be a node session (S7-02, S7-03); and the question whether a commit
 *		record has been confirmed by the synchronous standbys, which the second
 *		phase of two-phase commit and recovery ask (S7-01).
 *
 * Locks: mutex protects the barrier and the marks, ident_mutex the identity
 * slots; they are spinlocks held for the copy of a few words, never together
 * and never while anything is called.  SyncRepLock is taken alone, shared.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/ha.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/parallel.h"
#include "access/xact.h"
#include "access/xlog.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "replication/message.h"
#include "replication/syncrep.h"
#include "replication/walsender.h"
#include "replication/walsender_private.h"
#include "storage/ipc.h"
#include "storage/lwlock.h"
#include "storage/proc.h"
#include "storage/procarray.h"
#include "storage/latch.h"
#include "storage/shmem.h"
#include "storage/spin.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/jsonb.h"
#include "utils/timestamp.h"
#include "utils/wait_classes.h"

#include "forward_auth.h"
#include "ha.h"
#include "kingres_shard.h"
#include "node_conn.h"

PG_FUNCTION_INFO_V1(kingres_internal_restore_barrier);
PG_FUNCTION_INFO_V1(kingres_internal_mark_victim);
PG_FUNCTION_INFO_V1(kingres_internal_dtx_identity);
PG_FUNCTION_INFO_V1(kingres_internal_unmark_victim);
PG_FUNCTION_INFO_V1(kingres_internal_sync_confirm);
PG_FUNCTION_INFO_V1(kingres_internal_sync_probe);

#define KS_MAX_VICTIMS 32
/* How long a mark stays valid: the cancel that follows it arrives at once. */
#define KS_VICTIM_TTL_MS 10000

typedef struct HaVictim
{
	int			pid;
	TimestampTz expires;
	TimestampTz waitstart;	/* the lock wait the mark was made for */
} HaVictim;

/*
 * The identity that a node session registered for its remote transaction (the
 * origin node, the origin backend and the start of the origin transaction),
 * kept per backend slot.  Only the backend that owns a slot writes it, and only
 * after it proved to come from a node of the cluster; a lookup by pid checks
 * that the slot still belongs to that pid.
 */
typedef struct HaIdent
{
	int			pid;			/* 0: none */
	int			origin_node;
	int			origin_pid;
	int64		origin_start;	/* microseconds since the Unix epoch */
} HaIdent;

typedef struct HaShared
{
	slock_t		mutex;
	bool		barrier_active;
	TimestampTz barrier_expires;
	int64		barrier_generation;
	int			barrier_owner;	/* backend that raised the barrier; 0 none (S7-08) */
	int			inflight;
	/* the newest catalog term this node was refused with while it acted as the map primary (term.c); 0 none */
	int64		superseded_term;
	HaVictim	victims[KS_MAX_VICTIMS];

	/*
	 * The identities, one slot per backend (the proc number is the index).
	 * ident_mutex is a lock of its own: it is held only for the copy of one
	 * slot, never together with mutex, and nothing is called while it is held.
	 */
	slock_t		ident_mutex;
	int			nidents;
	HaIdent		idents[FLEXIBLE_ARRAY_MEMBER];
} HaShared;

/* The slots: one per possible backend and auxiliary process; bounded by max_connections and friends. */
static Size
ha_shmem_size(void)
{
	return add_size(offsetof(HaShared, idents),
					mul_size(sizeof(HaIdent), MaxBackends + NUM_AUXILIARY_PROCS));
}

static HaShared *ha = NULL;
static void require_super(const char *what);
static bool counted = false;
static shmem_request_hook_type prev_shmem_request_hook = NULL;
static shmem_startup_hook_type prev_shmem_startup_hook = NULL;
static emit_log_hook_type prev_emit_log_hook = NULL;

static void
ha_shmem_request(void)
{
	if (prev_shmem_request_hook)
		prev_shmem_request_hook();
	RequestAddinShmemSpace(MAXALIGN(ha_shmem_size()));
}

static void
ha_shmem_startup(void)
{
	bool		found;

	if (prev_shmem_startup_hook)
		prev_shmem_startup_hook();
	ha = ShmemInitStruct("kingres_shard ha", ha_shmem_size(), &found);
	if (!found)
	{
		memset(ha, 0, ha_shmem_size());
		SpinLockInit(&ha->mutex);
		SpinLockInit(&ha->ident_mutex);
		ha->nidents = MaxBackends + NUM_AUXILIARY_PROCS;
	}
}

/*
 * The wait that this backend was marked as a victim for, or 0 when it has no
 * mark or the mark has run out.  Removes the mark.  The hook below reads the
 * mark only for an error that reports a cancel, and rewrites the error only
 * when the backend is still in the very wait the mark was made for (S7-10): a
 * mark that missed its wait (the backend left it before the cancel arrived)
 * can never turn another cancel into a deadlock error, and runs out by
 * itself.
 */
static TimestampTz
consume_victim_mark(int pid)
{
	TimestampTz hit = 0;
	TimestampTz now = GetCurrentTimestamp();
	int			i;

	SpinLockAcquire(&ha->mutex);
	for (i = 0; i < KS_MAX_VICTIMS; i++)
	{
		if (ha->victims[i].pid == pid)
		{
			if (ha->victims[i].expires > now)
				hit = ha->victims[i].waitstart;
			ha->victims[i].pid = 0;
		}
	}
	SpinLockRelease(&ha->mutex);
	return hit;
}

/*
 * The error hook.  It runs for every error of every backend: no catalog
 * access, no lock but the spinlock of consume_victim_mark (held for a copy of
 * a few words), and an allocation only for the message that it rewrites.
 */
static void
ha_emit_log(ErrorData *edata)
{
	if (ha != NULL && edata->elevel >= ERROR &&
		edata->sqlerrcode == ERRCODE_QUERY_CANCELED)
	{
		TimestampTz mark = consume_victim_mark(MyProcPid);

		/*
		 * The wait of the mark began during the statement that this error ends:
		 * a mark for an earlier statement of this backend (the wait is over, the
		 * cancel hit an idle backend or was meant for something else) is
		 * dropped without effect.  The lock wait itself is already cleaned up when
		 * a cancel is reported, so its state cannot be asked here.
		 */
		if (mark != 0 && mark >= GetCurrentStatementStartTimestamp() &&
			mark <= GetCurrentTimestamp())
		{
			edata->sqlerrcode = ERRCODE_T_R_DEADLOCK_DETECTED;
			edata->message = pstrdup("distributed deadlock detected");
			edata->detail = pstrdup("This transaction waited for a lock in a cycle of waits that spans several nodes, and was chosen as the victim because it is the youngest transaction of the cycle.");
			edata->hint = pstrdup("The transaction was rolled back; repeat it.");
		}
	}
	if (prev_emit_log_hook)
		prev_emit_log_hook(edata);
}

/* ---------------------------------------------------------------------------
 * The identity of a distributed transaction (S7-02, S7-03)
 *
 * A remote session that a node opens for a distributed transaction says whose
 * transaction it works for with SET LOCAL kingres_shard.forward_dtx = 'node:pid:start'
 * in the first command of its remote transaction.  The setting is taken into
 * the slot of the backend only if the session proved to be a node session
 * (forward_auth.c); the SET LOCAL goes with the transaction, which removes the
 * identity again.  The deadlock detector reads the slot of a backend through
 * kingres.internal_dtx_identity().  Nothing a client sets, application_name
 * included, is ever read as an identity.
 * ---------------------------------------------------------------------------
 */
typedef struct DtxClaim
{
	bool		set;
	int			node;
	int			pid;
	int64		start;
} DtxClaim;

static char *forward_dtx_guc = NULL;
static bool ident_exit_registered = false;

static bool
check_forward_dtx(char **newval, void **extra, GucSource source)
{
	DtxClaim   *c;
	int			node;
	int			pid;
	long long	start;
	char		tail;

	c = (DtxClaim *) guc_malloc(LOG, sizeof(DtxClaim));
	if (c == NULL)
		return false;
	memset(c, 0, sizeof(DtxClaim));
	if (*newval != NULL && (*newval)[0] != '\0')
	{
		/* Only a session that was opened by a node may carry one. */
		if (!kingres_shard_forwarded)
		{
			GUC_check_errmsg("kingres_shard.forward_dtx can only be set by a session opened by another node");
			guc_free(c);
			return false;
		}
		if (sscanf(*newval, "%d:%d:%lld%c", &node, &pid, &start, &tail) != 3 ||
			node <= 0 || pid <= 0 || start < 0)
		{
			guc_free(c);
			return false;
		}
		c->set = true;
		c->node = node;
		c->pid = pid;
		c->start = (int64) start;
	}
	*extra = c;
	return true;
}

static void
clear_ident_at_exit(int code, Datum arg)
{
	if (ha != NULL && MyProcNumber != INVALID_PROC_NUMBER && MyProcNumber < ha->nidents)
	{
		SpinLockAcquire(&ha->ident_mutex);
		ha->idents[MyProcNumber].pid = 0;
		SpinLockRelease(&ha->ident_mutex);
	}
}

/*
 * Publish (or remove) the identity of this backend.  No allocation, no catalog
 * access; the spinlock is held for the copy of one slot.
 */
static void
assign_forward_dtx(const char *newval, void *extra)
{
	DtxClaim   *c = (DtxClaim *) extra;
	HaIdent    *slot;

	if (ha == NULL || MyProcNumber == INVALID_PROC_NUMBER || MyProcNumber >= ha->nidents ||
		IsParallelWorker())
		return;
	slot = &ha->idents[MyProcNumber];
	if (!ident_exit_registered)
	{
		before_shmem_exit(clear_ident_at_exit, (Datum) 0);
		ident_exit_registered = true;
	}
	SpinLockAcquire(&ha->ident_mutex);
	if (c != NULL && c->set && kingres_shard_session_verified())
	{
		slot->pid = MyProcPid;
		slot->origin_node = c->node;
		slot->origin_pid = c->pid;
		slot->origin_start = c->start;
	}
	else
		slot->pid = 0;
	SpinLockRelease(&ha->ident_mutex);
}

/*
 * kingres.internal_dtx_identity(pid int) returns text
 *
 * The label "ks-dtx:<origin node>:<origin pid>:<origin start>" that the backend
 * pid registered as a verified node session, or NULL.  Superusers only.
 */
Datum
kingres_internal_dtx_identity(PG_FUNCTION_ARGS)
{
	int			pid = PG_GETARG_INT32(0);
	PGPROC	   *proc;
	HaIdent		copy;
	int			idx;
	char		buf[96];

	require_super("internal_dtx_identity");
	if (ha == NULL)
		PG_RETURN_NULL();
	proc = BackendPidGetProc(pid);
	if (proc == NULL)
		PG_RETURN_NULL();
	idx = GetNumberFromPGProc(proc);
	if (idx < 0 || idx >= ha->nidents)
		PG_RETURN_NULL();
	SpinLockAcquire(&ha->ident_mutex);
	copy = ha->idents[idx];
	SpinLockRelease(&ha->ident_mutex);
	if (copy.pid != pid)
		PG_RETURN_NULL();
	snprintf(buf, sizeof(buf), "ks-dtx:%d:%d:" INT64_FORMAT,
			 copy.origin_node, copy.origin_pid, copy.origin_start);
	PG_RETURN_TEXT_P(cstring_to_text(buf));
}

void
kingres_shard_ha_init(void)
{
	prev_shmem_request_hook = shmem_request_hook;
	shmem_request_hook = ha_shmem_request;
	prev_shmem_startup_hook = shmem_startup_hook;
	shmem_startup_hook = ha_shmem_startup;
	prev_emit_log_hook = emit_log_hook;
	emit_log_hook = ha_emit_log;

	DefineCustomStringVariable("kingres_shard.forward_dtx",
							   "Set by the router on the remote transactions of a node; not for users.",
							   "The distributed transaction that the remote transaction works for: origin node, origin backend, start.",
							   &forward_dtx_guc,
							   "",
							   PGC_USERSET,
							   GUC_NOT_IN_SAMPLE | GUC_NO_SHOW_ALL | GUC_NO_RESET_ALL,
							   check_forward_dtx, assign_forward_dtx, NULL);
}

void
kingres_shard_ha_commit_enter(void)
{
	if (ha == NULL || counted)
		return;
	for (;;)
	{
		TimestampTz now = GetCurrentTimestamp();

		SpinLockAcquire(&ha->mutex);
		if (!ha->barrier_active || now >= ha->barrier_expires)
		{
			ha->inflight++;
			counted = true;
			SpinLockRelease(&ha->mutex);
			return;
		}
		SpinLockRelease(&ha->mutex);
		kingres_shard_fault_point("commit_waits_for_barrier");
		(void) WaitLatch(MyLatch, WL_LATCH_SET | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH,
						 10, PG_WAIT_EXTENSION);
		ResetLatch(MyLatch);
		CHECK_FOR_INTERRUPTS();
	}
}

void
kingres_shard_ha_commit_leave(void)
{
	if (ha == NULL || !counted)
		return;
	SpinLockAcquire(&ha->mutex);
	if (ha->inflight > 0)
		ha->inflight--;
	SpinLockRelease(&ha->mutex);
	counted = false;
}

/*
 * Has the WAL up to lsn been confirmed by the synchronous standbys, as the
 * configuration of this node requires?
 *
 * PostgreSQL does not say whether a commit that returned was acknowledged:
 * when the backend that waits for its synchronous standby is cancelled or
 * terminated, SyncRepWaitForLSN() ends the wait with a WARNING and the commit
 * completes (syncrep.c, "the transaction has already committed locally, but
 * might not have been replicated to the standby").  The only state a
 * finished wait leaves behind is the one the walsenders keep: the position up
 * to which each kind of wait is released, WalSndCtl->lsn[mode], under
 * SyncRepLock.  This function asks the same question the wait asked, with the
 * same two cases (the status of the synchronous standby data may not be
 * initialized yet at start-up).
 *
 * mode is SYNC_REP_WAIT_FLUSH or SYNC_REP_WAIT_APPLY.  No synchronous standby
 * defined, or no walsender possible, means that the configuration requires
 * nothing, and the answer is true: that is what a commit does then, too.
 *
 * Lock order: SyncRepLock is taken alone, shared, and released before the
 * function returns; no other lock is held or taken inside.
 */
bool
kingres_shard_sync_confirmed(XLogRecPtr lsn, int mode)
{
	bool		ok;

	if (max_wal_senders == 0 || WalSndCtl == NULL)
		return true;
	Assert(mode >= SYNC_REP_WAIT_WRITE && mode < NUM_SYNC_REP_WAIT_MODE);

	LWLockAcquire(SyncRepLock, LW_SHARED);
	if (lsn <= WalSndCtl->lsn[mode])
		ok = true;
	else if (WalSndCtl->sync_standbys_status & SYNC_STANDBY_INIT)
		ok = (WalSndCtl->sync_standbys_status & SYNC_STANDBY_DEFINED) == 0;
	else
		ok = (SyncRepStandbyNames == NULL || SyncRepStandbyNames[0] == '\0');
	LWLockRelease(SyncRepLock);
	return ok;
}

/*
 * The kind of wait a commit of this session does for its synchronous standby
 * once the extension has raised the setting to "on": flush, or apply when the
 * session asks for remote_apply.
 */
int
kingres_shard_sync_wait_mode(void)
{
	return (synchronous_commit >= SYNCHRONOUS_COMMIT_REMOTE_APPLY) ?
		SYNC_REP_WAIT_APPLY : SYNC_REP_WAIT_FLUSH;
}

/*
 * Wait, for at most timeout_ms, until everything this node has flushed so
 * far is confirmed by the synchronous standbys.  Used by recovery, which acts
 * on a decision that it read from a committed row: the commit record of that
 * row is below the flush position taken here, because the row became visible
 * only after its record was flushed.  A target fixed first does not move
 * while waiting, so a node under load cannot starve the wait.  Returns false
 * when the standbys have not confirmed it in time (the caller leaves the
 * transaction prepared and tries again in its next pass).
 */
bool
kingres_shard_wait_decision_durable(int timeout_ms)
{
	if (RecoveryInProgress() || max_wal_senders == 0)
		return true;
	return kingres_shard_wait_confirmed(GetFlushRecPtr(NULL), SYNC_REP_WAIT_FLUSH, timeout_ms);
}

/*
 * Wait, for at most timeout_ms, until the synchronous standbys have confirmed
 * everything up to target, the way the configuration of this node requires.
 * The target is fixed by the caller and does not move while waiting.
 * Interruptible: an error here is the caller's to deal with.
 */
bool
kingres_shard_wait_confirmed(XLogRecPtr target, int mode, int timeout_ms)
{
	TimestampTz deadline = TimestampTzPlusMilliseconds(GetCurrentTimestamp(), timeout_ms);

	for (;;)
	{
		long		left;

		if (kingres_shard_sync_confirmed(target, mode))
			return true;
		left = TimestampDifferenceMilliseconds(GetCurrentTimestamp(), deadline);
		if (left <= 0)
			return false;
		(void) WaitLatch(MyLatch, WL_LATCH_SET | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH,
						 Min(left, 20), PG_WAIT_EXTENSION);
		ResetLatch(MyLatch);
		CHECK_FOR_INTERRUPTS();
	}
}

/*
 * kingres.internal_sync_confirm(timeout_ms int) returns boolean
 *
 * The participant's answer to "is what you just did durable?" (S8-01).  The
 * origin cannot see the warning with which PostgreSQL ends the wait of a
 * PREPARE TRANSACTION or a COMMIT PREPARED for a cancel, a termination or a
 * shutdown: the statement answers as done.  So the origin asks, in the same
 * session and right after, whether the position this node has flushed so far
 * is confirmed by its synchronous standbys as its own configuration requires.
 * That position is at or beyond the record of the statement, so "yes" means
 * the record is there.  "No" is answered after timeout_ms.  The target is taken
 * once, so a busy node waits only for the few milliseconds that its standby
 * needs to catch up with the writes of other sessions.  The configuration that
 * requires nothing (no synchronous standby named) answers yes, as a commit
 * does.  Only a node session or a superuser may ask; the session's own
 * synchronous_commit decides between flush and apply.
 */
Datum
kingres_internal_sync_confirm(PG_FUNCTION_ARGS)
{
	int			timeout_ms = PG_GETARG_INT32(0);

	if (!superuser() && !kingres_shard_session_forwarded())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for function internal_sync_confirm")));
	if (RecoveryInProgress())
		PG_RETURN_BOOL(true);
	PG_RETURN_BOOL(kingres_shard_wait_confirmed(GetFlushRecPtr(NULL),
												kingres_shard_sync_wait_mode(),
												Max(timeout_ms, 0)));
}


/*
 * Can this node commit at all?  A commit waits for the synchronous standbys that
 * synchronous_standby_names names, and a name that nobody serves makes every
 * commit wait for ever (a promoted standby inherits the list of the primary it
 * was copied from, S8-02).  A new WAL record is written and flushed, and the
 * standbys get timeout_ms to confirm it, so that a node that has been idle
 * since its last standby left is found out as well.  True when nothing is required
 * of the node (no list, no walsender possible) or the standbys confirmed.
 */
bool
kingres_shard_probe_sync(int timeout_ms)
{
	XLogRecPtr	lsn;

	if (RecoveryInProgress() || max_wal_senders == 0)
		return true;
	lsn = LogLogicalMessage("kingres_shard", "p", 1, false, true);
	return kingres_shard_wait_confirmed(lsn, SYNC_REP_WAIT_FLUSH, Max(timeout_ms, 0));
}

/*
 * kingres.internal_sync_probe(timeout_ms int) returns boolean: the SQL form of
 * kingres_shard_probe_sync(), for promote_node() to ask a node that is about
 * to be promoted.
 */
Datum
kingres_internal_sync_probe(PG_FUNCTION_ARGS)
{
	if (!superuser() && !kingres_shard_session_forwarded())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for function internal_sync_probe")));
	PG_RETURN_BOOL(kingres_shard_probe_sync(PG_GETARG_INT32(0)));
}

/*
 * A barrier belongs to the backend that raised it, which is the session of the node
 * that create_restore_point() opened: when that session ends (the caller was
 * terminated, its connection was lost, the server of the caller stopped) the
 * barrier falls at once, without waiting for its time to live (S7-08).
 */
static bool barrier_exit_registered = false;

static void
lower_barrier_at_exit(int code, Datum arg)
{
	if (ha == NULL)
		return;
	SpinLockAcquire(&ha->mutex);
	if (ha->barrier_owner == MyProcPid)
	{
		ha->barrier_active = false;
		ha->barrier_owner = 0;
	}
	SpinLockRelease(&ha->mutex);
}

static void
require_super(const char *what)
{
	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for function %s", what)));
}

/*
 * kingres.internal_restore_barrier(op text, ttl_ms int, generation bigint) returns jsonb
 *
 *	  'on'	  raise the barrier for ttl_ms milliseconds; the answer carries the
 *			  generation of this barrier and the number of transactions in flight
 *	  'state' the same, changing nothing
 *	  'off'   lower the barrier; "held" says whether the barrier of the given
 *			  generation was up, unexpired, for the whole time since 'on'
 */
Datum
kingres_internal_restore_barrier(PG_FUNCTION_ARGS)
{
	char	   *op;
	int			ttl_ms = PG_ARGISNULL(1) ? 30000 : PG_GETARG_INT32(1);
	int64		gen = PG_ARGISNULL(2) ? 0 : PG_GETARG_INT64(2);
	bool		active;
	bool		held = false;
	bool		barrier_raised_here = false;
	int64		cur_gen;
	int			inflight;
	TimestampTz now = GetCurrentTimestamp();
	char		buf[160];

	require_super("internal_restore_barrier");
	if (PG_ARGISNULL(0))
		ereport(ERROR, (errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED), errmsg("op must not be null")));
	op = text_to_cstring(PG_GETARG_TEXT_PP(0));
	if (ha == NULL)
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("kingres_shard shared memory is not available")));
	if (ttl_ms < 1)
		ttl_ms = 1;

	SpinLockAcquire(&ha->mutex);
	if (strcmp(op, "on") == 0)
	{
		ha->barrier_active = true;
		ha->barrier_expires = TimestampTzPlusMilliseconds(now, ttl_ms);
		ha->barrier_generation++;
		ha->barrier_owner = MyProcPid;
		barrier_raised_here = true;
	}
	else if (strcmp(op, "off") == 0)
	{
		held = ha->barrier_active && ha->barrier_generation == gen &&
			now < ha->barrier_expires;
		/* A late lowering by the caller of an earlier barrier must not lower a newer one. */
		if (gen == 0 || ha->barrier_generation == gen)
		{
			ha->barrier_active = false;
			ha->barrier_owner = 0;
		}
	}
	else if (strcmp(op, "state") != 0)
	{
		SpinLockRelease(&ha->mutex);
		ereport(ERROR,
				(errcode(ERRCODE_INVALID_PARAMETER_VALUE),
				 errmsg("invalid barrier operation \"%s\"", op)));
	}
	active = ha->barrier_active && now < ha->barrier_expires;
	cur_gen = ha->barrier_generation;
	inflight = ha->inflight;
	SpinLockRelease(&ha->mutex);
	if (barrier_raised_here && !barrier_exit_registered)
	{
		before_shmem_exit(lower_barrier_at_exit, (Datum) 0);
		barrier_exit_registered = true;
	}

	snprintf(buf, sizeof(buf),
			 "{\"active\": %s, \"held\": %s, \"generation\": " INT64_FORMAT ", \"inflight\": %d}",
			 active ? "true" : "false", held ? "true" : "false", cur_gen, inflight);
	PG_RETURN_DATUM(DirectFunctionCall1(jsonb_in, CStringGetDatum(buf)));
}

/*
 * kingres.internal_mark_victim(pid int) returns bool
 *
 * Mark a backend of this node as the victim of a distributed deadlock: its
 * next query cancel is reported as 40P01.  The caller cancels it right after.
 */
Datum
kingres_internal_mark_victim(PG_FUNCTION_ARGS)
{
	int			pid = PG_GETARG_INT32(0);
	int64		unix_us = PG_GETARG_INT64(1);
	TimestampTz now = GetCurrentTimestamp();
	TimestampTz ws;
	int			i;
	int			slot = -1;

	require_super("internal_mark_victim");
	if (ha == NULL)
		PG_RETURN_BOOL(false);
	/* The wait as PostgreSQL keeps it (microseconds since 2000), from the Unix time of pg_locks. */
	ws = (TimestampTz) (unix_us - ((int64) (POSTGRES_EPOCH_JDATE - UNIX_EPOCH_JDATE)) * SECS_PER_DAY * USECS_PER_SEC);
	SpinLockAcquire(&ha->mutex);
	for (i = 0; i < KS_MAX_VICTIMS; i++)
	{
		if (ha->victims[i].pid == pid)
		{
			slot = i;
			break;
		}
		if (slot < 0 && (ha->victims[i].pid == 0 || ha->victims[i].expires <= now))
			slot = i;
	}
	if (slot >= 0)
	{
		ha->victims[slot].pid = pid;
		ha->victims[slot].expires = TimestampTzPlusMilliseconds(now, KS_VICTIM_TTL_MS);
		ha->victims[slot].waitstart = ws;
	}
	SpinLockRelease(&ha->mutex);
	PG_RETURN_BOOL(slot >= 0);
}

/*
 * The newer catalog term that this node, acting as the map group primary,
 * has been refused with (term.c): remembered at once, before the worker has
 * written it to the catalog.  The spinlock is held only for the copy.
 */
void
kingres_shard_ha_note_superseded(int64 term)
{
	if (ha == NULL)
		return;
	SpinLockAcquire(&ha->mutex);
	if (term > ha->superseded_term)
		ha->superseded_term = term;
	SpinLockRelease(&ha->mutex);
}

int64
kingres_shard_ha_superseded_term(void)
{
	int64		t;

	if (ha == NULL)
		return 0;
	SpinLockAcquire(&ha->mutex);
	t = ha->superseded_term;
	SpinLockRelease(&ha->mutex);
	return t;
}

/*
 * kingres.internal_unmark_victim(pid int) returns bool
 *
 * Remove the mark of a backend of this node; true when there was one.  The
 * detector calls it after the cancel: a mark that is still there was not
 * consumed by an error of the backend, so the cancel did not reach a wait.
 */
Datum
kingres_internal_unmark_victim(PG_FUNCTION_ARGS)
{
	int			pid = PG_GETARG_INT32(0);
	bool		had = false;
	int			i;

	require_super("internal_unmark_victim");
	if (ha == NULL)
		PG_RETURN_BOOL(false);
	SpinLockAcquire(&ha->mutex);
	for (i = 0; i < KS_MAX_VICTIMS; i++)
	{
		if (ha->victims[i].pid == pid)
		{
			ha->victims[i].pid = 0;
			had = true;
		}
	}
	SpinLockRelease(&ha->mutex);
	PG_RETURN_BOOL(had);
}
