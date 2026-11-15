<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Cluster-wide banners under the top bar: map group unavailable, open alerts, and
// "rebalance needed / running". Rendered inside one polite live region.
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import { useQueryClient } from '@tanstack/vue-query'
import SafeName from './SafeName.vue'
import { ApiError } from '@/api/problem'
import { keys, useCluster, useClusterSummary } from '@/api/queries'
import { REBALANCE_REASON_LABEL } from '@/lib/status'
import { useSessionStore } from '@/stores/session'
import type { Server } from '@/api/types'

const session = useSessionStore()
const client = useQueryClient()
const cluster = useCluster(() => session.can('getCluster'))
// A stats-only account reads the summary instead; either one answers 503 leader_unavailable when this server has lost
// the manager leader (contract 0.7.0).
const summary = useClusterSummary(() => session.isSignedIn && !session.can('getCluster'))
const isLeaderUnavailable = (e: unknown): boolean =>
  e instanceof ApiError && e.code === 'leader_unavailable'
const leaderLost = computed(
  () => isLeaderUnavailable(cluster.error.value) || isLeaderUnavailable(summary.error.value),
)
/**
 * The response does not say which server to open instead, so the banner names the servers this panel knew from its
 * last answers (kept in the query cache, not asked again: this server cannot answer them now), without this one.
 */
const otherServers = computed<Server[]>(() => {
  if (!leaderLost.value) return []
  const known = client.getQueryData<{ items: Server[] }>(keys.servers)?.items ?? []
  return known.filter((x) => x.id !== session.session?.serverId)
})
const knownLeader = computed(() => cluster.data.value?.managerLeader?.serverId ?? null)
const c = computed(() => cluster.data.value)
const canSeeRebalance = computed(() => session.can('getRebalanceStatus'))
const canSeeAlerts = computed(() => session.can('listAlerts'))
</script>

<template>
  <div aria-live="polite" class="banners">
    <div v-if="leaderLost" class="banner tone-danger" data-testid="banner-leader">
      <span aria-hidden="true">&#10006;</span>
      <span>
        <strong>This server has lost contact with the manager leader.</strong>
        It refuses the requests that need a check of your session until it hears the leader again,
        so nothing can be changed here and what is shown may be old.
        <template v-if="otherServers.length"
          >Open the console on another server of the cluster:
          <template v-for="(x, i) in otherServers" :key="x.id"
            ><template v-if="i > 0">, </template
            ><span class="mono"><SafeName :value="x.address" kind="text" :max="80" /></span
            ><template v-if="x.id === knownLeader">
              (the manager leader, as last known)</template
            ></template
          >.</template
        >
        <template v-else>Open the console on another server of the cluster.</template>
      </span>
    </div>
    <div v-if="c && !c.mapGroupAvailable" class="banner tone-danger" data-testid="banner-map">
      <span aria-hidden="true">&#10006;</span>
      <span>
        <strong>Map group unavailable.</strong> Topology changes, rebalance and distributed DDL are
        paused until it recovers.
      </span>
    </div>
    <div
      v-if="c && canSeeAlerts && (c.openAlerts.critical > 0 || c.openAlerts.warning > 0)"
      class="banner"
      :class="c.openAlerts.critical > 0 ? 'tone-danger' : 'tone-warn'"
      data-testid="banner-alerts"
    >
      <span aria-hidden="true">{{ c.openAlerts.critical > 0 ? '✖' : '▲' }}</span>
      <span>
        <strong>
          {{ c.openAlerts.critical }} critical, {{ c.openAlerts.warning }} warning
          {{ c.openAlerts.critical + c.openAlerts.warning === 1 ? 'alert' : 'alerts' }} open.
        </strong>
        <RouterLink :to="{ name: 'alerts' }">View alerts</RouterLink>
      </span>
    </div>
    <div v-if="c?.rebalance.running" class="banner tone-info" data-testid="banner-rebalance">
      <span aria-hidden="true">&#9680;</span>
      <span>
        <strong>Rebalance running.</strong>
        <RouterLink v-if="canSeeRebalance" :to="{ name: 'rebalance' }">Follow progress</RouterLink>
      </span>
    </div>
    <div v-else-if="c?.rebalance.needed" class="banner tone-warn" data-testid="banner-rebalance">
      <span aria-hidden="true">&#9650;</span>
      <span>
        <strong>Rebalance needed.</strong>
        {{ c.rebalance.reasons.map((r) => REBALANCE_REASON_LABEL[r]).join('; ') }}.
        <RouterLink v-if="canSeeRebalance" :to="{ name: 'rebalance' }">Review the plan</RouterLink>
      </span>
    </div>
  </div>
</template>
