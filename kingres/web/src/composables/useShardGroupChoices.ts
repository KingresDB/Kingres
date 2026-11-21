// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The shard groups a schema can be placed on or moved to (contract 0.15.0): the groups other than the map group whose primary
// runs on a server that should hold shards. A group has no name of its own; it is told by its number and the server of its
// primary.
import { computed, toValue, type MaybeRefOrGetter } from 'vue'
import { useServers, useTopology } from '@/api/queries'
import { safeText } from '@/lib/safe-text'

export interface GroupChoice {
  id: number
  serverId: string | null
  /** For an option of a select: plain text, hidden characters made visible. */
  label: string
}

/** `exclude` is a group that is not offered (the one the schema is on now). */
export function useShardGroupChoices(exclude: MaybeRefOrGetter<number | null> = null) {
  const topology = useTopology()
  const servers = useServers()
  const choices = computed<GroupChoice[]>(() => {
    const holds = new Map((servers.data.value?.items ?? []).map((s) => [s.id, s.shouldHaveShards]))
    return (topology.data.value?.groups ?? [])
      .filter((g) => !g.isMapGroup && g.id !== toValue(exclude))
      .map((g) => ({ id: g.id, serverId: g.primary?.serverId ?? g.preferredPrimaryServerId }))
      .filter((g) => g.serverId === null || holds.get(g.serverId) !== false)
      .map((g) => ({
        ...g,
        label: g.serverId ? `Group ${g.id} (server ${safeText(g.serverId, 60)})` : `Group ${g.id}`,
      }))
  })
  return {
    choices,
    isPending: computed(() => topology.isPending.value || servers.isPending.value),
    error: computed(() => topology.error.value ?? servers.error.value),
  }
}
