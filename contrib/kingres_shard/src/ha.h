/*-------------------------------------------------------------------------
 *
 * ha.h
 *	  Shared state of the high-availability functions: the restore-point
 *	  barrier and the marks of distributed-deadlock victims.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/ha.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_HA_H
#define KINGRES_SHARD_HA_H

#include "access/xlogdefs.h"

extern void kingres_shard_ha_init(void);

/*
 * A transaction that is about to run two-phase commit enters before it
 * prepares anything and leaves when its second phase is over (or it aborts).
 * While a restore-point barrier is up, entering waits.
 */
extern void kingres_shard_ha_commit_enter(void);
extern void kingres_shard_ha_commit_leave(void);

/*
 * Durability of a decision on the synchronous standbys (review S7-01): see
 * ha.c.
 */
extern bool kingres_shard_sync_confirmed(XLogRecPtr lsn, int mode);
extern int	kingres_shard_sync_wait_mode(void);
extern bool kingres_shard_wait_decision_durable(int timeout_ms);

/* The newer catalog term this node was refused with as the map group primary (term.c). */
extern void kingres_shard_ha_note_superseded(int64 term);
extern int64 kingres_shard_ha_superseded_term(void);

#endif							/* KINGRES_SHARD_HA_H */
