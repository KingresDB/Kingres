/*-------------------------------------------------------------------------
 *
 * node_admin.h
 *	  Cluster administration: catalog synchronization and reload.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/node_admin.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_NODE_ADMIN_H
#define KINGRES_SHARD_NODE_ADMIN_H

#include "postgres.h"

extern void kingres_shard_require_map_group(void);
extern bool kingres_shard_is_recognized_map_primary(void);
extern void kingres_shard_lock_catalog(void);
extern void kingres_shard_sync_catalog_internal(void);
extern bool kingres_shard_reload_catalog(void);

#endif							/* KINGRES_SHARD_NODE_ADMIN_H */
