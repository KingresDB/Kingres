/*-------------------------------------------------------------------------
 *
 * dist_scan.h
 *	  The "Kingres Distributed Scan" custom scan: the always-correct way to read
 *	  a distributed table in a statement that spans shard groups.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/dist_scan.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_DIST_SCAN_H
#define KINGRES_SHARD_DIST_SCAN_H

#include "postgres.h"

extern void kingres_shard_dist_scan_init(void);

/*
 * While active, the standard planner replaces the paths of every distributed
 * table by the distributed scan (see the file header of dist_scan.c).
 */
extern bool kingres_shard_distscan_set_active(bool active);

#endif							/* KINGRES_SHARD_DIST_SCAN_H */
