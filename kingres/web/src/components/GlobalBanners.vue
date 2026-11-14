<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Cluster-wide banners under the top bar: map group unavailable, open alerts, and
// "rebalance needed / running". Rendered inside one polite live region.
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import { useCluster } from '@/api/queries'
import { REBALANCE_REASON_LABEL } from '@/lib/status'
import { useSessionStore } from '@/stores/session'

const session = useSessionStore()
const cluster = useCluster(() => session.can('getCluster'))
const c = computed(() => cluster.data.value)
const canSeeRebalance = computed(() => session.can('getRebalanceStatus'))
const canSeeAlerts = computed(() => session.can('listAlerts'))
</script>

<template>
  <div aria-live="polite" class="banners">
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
