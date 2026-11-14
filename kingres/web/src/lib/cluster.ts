// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
import type { Instance } from '@/api/types'

/** "primary of group 3", "standby of map group". */
export function instanceLabel(i: Pick<Instance, 'role' | 'isMapGroup' | 'shardGroupId'>): string {
  return `${i.role} of ${i.isMapGroup ? 'map group' : `group ${i.shardGroupId}`}`
}

export function groupLabel(id: number, isMapGroup: boolean): string {
  return isMapGroup ? 'Map group' : `Shard group ${id}`
}
