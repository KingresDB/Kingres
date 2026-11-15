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
 *		every error before it is sent to the client.  This depends on the error
 *		being reported to the server log, that is on log_min_messages being
 *		"error" or lower (the default is "warning"); with a higher setting the
 *		victim still ends, with the plain cancel error.
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

#include "fmgr.h"
#include "miscadmin.h"
#include "storage/ipc.h"
#include "storage/latch.h"
#include "storage/shmem.h"
#include "storage/spin.h"
#include "utils/builtins.h"
#include "utils/jsonb.h"
#include "utils/timestamp.h"
#include "utils/wait_classes.h"

#include "ha.h"
#include "kingres_shard.h"
#include "node_conn.h"

PG_FUNCTION_INFO_V1(kingres_internal_restore_barrier);
PG_FUNCTION_INFO_V1(kingres_internal_mark_victim);

#define KS_MAX_VICTIMS 32
/* How long a mark stays valid: the cancel that follows it arrives at once. */
#define KS_VICTIM_TTL_MS 10000

typedef struct HaVictim
{
	int			pid;
	TimestampTz expires;
} HaVictim;

typedef struct HaShared
{
	slock_t		mutex;
	bool		barrier_active;
	TimestampTz barrier_expires;
	int64		barrier_generation;
	int			inflight;
	HaVictim	victims[KS_MAX_VICTIMS];
} HaShared;

static HaShared *ha = NULL;
static bool counted = false;
static shmem_request_hook_type prev_shmem_request_hook = NULL;
static shmem_startup_hook_type prev_shmem_startup_hook = NULL;
static emit_log_hook_type prev_emit_log_hook = NULL;

static void
ha_shmem_request(void)
{
	if (prev_shmem_request_hook)
		prev_shmem_request_hook();
	RequestAddinShmemSpace(MAXALIGN(sizeof(HaShared)));
}

static void
ha_shmem_startup(void)
{
	bool		found;

	if (prev_shmem_startup_hook)
		prev_shmem_startup_hook();
	ha = ShmemInitStruct("kingres_shard ha", sizeof(HaShared), &found);
	if (!found)
	{
		memset(ha, 0, sizeof(HaShared));
		SpinLockInit(&ha->mutex);
	}
}

/* Is this backend marked as a victim?  Removes the mark. */
static bool
consume_victim_mark(int pid)
{
	bool		hit = false;
	TimestampTz now = GetCurrentTimestamp();
	int			i;

	SpinLockAcquire(&ha->mutex);
	for (i = 0; i < KS_MAX_VICTIMS; i++)
	{
		if (ha->victims[i].pid == pid)
		{
			hit = ha->victims[i].expires > now;
			ha->victims[i].pid = 0;
		}
	}
	SpinLockRelease(&ha->mutex);
	return hit;
}

static void
ha_emit_log(ErrorData *edata)
{
	if (ha != NULL && edata->elevel >= ERROR &&
		edata->sqlerrcode == ERRCODE_QUERY_CANCELED &&
		consume_victim_mark(MyProcPid))
	{
		edata->sqlerrcode = ERRCODE_T_R_DEADLOCK_DETECTED;
		edata->message = pstrdup("distributed deadlock detected");
		edata->detail = pstrdup("This transaction waited for a lock in a cycle of waits that spans several nodes, and was chosen as the victim because it is the youngest transaction of the cycle.");
		edata->hint = pstrdup("The transaction was rolled back; repeat it.");
	}
	if (prev_emit_log_hook)
		prev_emit_log_hook(edata);
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
	}
	else if (strcmp(op, "off") == 0)
	{
		held = ha->barrier_active && ha->barrier_generation == gen &&
			now < ha->barrier_expires;
		ha->barrier_active = false;
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
	TimestampTz now = GetCurrentTimestamp();
	int			i;
	int			slot = -1;

	require_super("internal_mark_victim");
	if (ha == NULL)
		PG_RETURN_BOOL(false);
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
	}
	SpinLockRelease(&ha->mutex);
	PG_RETURN_BOOL(slot >= 0);
}
