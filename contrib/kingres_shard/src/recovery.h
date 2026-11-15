/*-------------------------------------------------------------------------
 *
 * recovery.h
 *	  Recovery of prepared transactions left by two-phase commit, and the
 *	  background workers that run it.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/recovery.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_RECOVERY_H
#define KINGRES_SHARD_RECOVERY_H

#include "postgres.h"

extern void kingres_shard_recovery_init(void);
extern int	kingres_shard_recover_prepared(void);
extern int	kingres_shard_recover_prepared_with(int extra_id, int extra_group,
														const char *extra_host, int extra_port);

PGDLLEXPORT void kingres_shard_recovery_launcher_main(Datum main_arg);
PGDLLEXPORT void kingres_shard_recovery_worker_main(Datum main_arg);

#endif							/* KINGRES_SHARD_RECOVERY_H */
