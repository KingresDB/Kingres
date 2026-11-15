<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The dashboard card "Configuration": the servers that wait for a restart, the parameters that differ inside a server
// group, the largest clock difference, and the servers where a change failed or waits. Every number links to the
// Configuration page filtered accordingly. A zero is a state ("none"), and a number the server cannot know is "not
// available", never 0.
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import SafeName from '@/components/SafeName.vue'
import NotAvailable from '@/components/NotAvailable.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import { useConfigSummary } from '@/api/queries'
import { CLOCK_STATE, clockState, formatMs } from '@/lib/config'

const summary = useConfigSummary()
const s = computed(() => summary.data.value)
const clock = computed(() => {
  const c = s.value?.largestClockDifference
  if (!s.value || !c || c.differenceMs === null || c.differenceMs === undefined) return null
  return {
    ms: c.differenceMs,
    serverId: c.serverId ?? null,
    state: clockState(true, c.differenceMs, 0, s.value.maxDifferenceMs),
  }
})
</script>

<template>
  <section class="card stack" aria-labelledby="cfgcard-h" data-testid="config-card">
    <div class="row-between">
      <h2 id="cfgcard-h">Configuration</h2>
      <RouterLink :to="{ name: 'configuration' }">Open</RouterLink>
    </div>
    <p v-if="summary.isPending.value" class="muted" role="status">Loading.</p>
    <ProblemNotice
      v-else-if="summary.error.value"
      :error="summary.error.value"
      @retry="summary.refetch()"
    />
    <ul v-else-if="s" class="plain">
      <li data-testid="cfg-pending">
        <span class="label">Servers waiting for a restart</span>
        <span v-if="s.serversPendingRestart.length === 0" class="value"
          ><span aria-hidden="true">&#10003;</span> none</span
        >
        <RouterLink
          v-else
          class="value"
          :to="{ name: 'configuration', query: { pendingRestart: '1' } }"
          :aria-label="`Servers waiting for a restart: ${s.serversPendingRestart.length}. Show the parameters.`"
          ><span aria-hidden="true">&#9650;</span> {{ s.serversPendingRestart.length }}</RouterLink
        >
      </li>
      <li data-testid="cfg-differing">
        <span class="label">Parameters that differ inside a server group</span>
        <span v-if="s.differingInGroup.length === 0" class="value"
          ><span aria-hidden="true">&#10003;</span> none</span
        >
        <RouterLink
          v-else
          class="value"
          :to="{ name: 'configuration', query: { differing: '1' } }"
          :aria-label="`Parameters that differ inside a server group: ${s.differingInGroup.length}. Show the parameters.`"
          ><span aria-hidden="true">&#8800;</span> {{ s.differingInGroup.length }}</RouterLink
        >
      </li>
      <li data-testid="cfg-clock">
        <span class="label">Largest clock difference</span>
        <NotAvailable v-if="clock === null" class="value" />
        <RouterLink
          v-else
          class="value"
          :to="{ name: 'configuration', query: { tab: 'time' } }"
          :aria-label="`Largest clock difference: ${formatMs(Math.abs(clock.ms))}, ${CLOCK_STATE[clock.state].label}. Show the clocks.`"
        >
          <span aria-hidden="true">{{ CLOCK_STATE[clock.state].icon }}</span>
          {{ formatMs(Math.abs(clock.ms)) }}
          <span v-if="clock.serverId" class="muted"
            >(<SafeName :value="clock.serverId" kind="text" :max="40" />)</span
          >
          <span class="muted">{{ CLOCK_STATE[clock.state].label }}</span>
        </RouterLink>
      </li>
      <li v-if="s.failedServerIds.length > 0" data-testid="cfg-failed">
        <span class="label">Servers where a change failed</span>
        <RouterLink
          class="value"
          :to="{ name: 'configuration', query: { state: 'failed' } }"
          :aria-label="`Servers where a change failed: ${s.failedServerIds.length}. Show the parameters.`"
          ><span aria-hidden="true">&#10006;</span> {{ s.failedServerIds.length }}</RouterLink
        >
      </li>
      <li v-if="s.waitingServerIds.length > 0" data-testid="cfg-waiting">
        <span class="label">Servers that wait to get a change</span>
        <RouterLink
          class="value"
          :to="{ name: 'configuration', query: { state: 'waiting' } }"
          :aria-label="`Servers that wait to get a change: ${s.waitingServerIds.length}. Show the parameters.`"
          ><span aria-hidden="true">&#9719;</span> {{ s.waitingServerIds.length }}</RouterLink
        >
      </li>
    </ul>
    <p v-if="s && s.failedServerIds.length === 0 && s.waitingServerIds.length === 0" class="muted">
      No failed or waiting server.
    </p>
  </section>
</template>

<style scoped>
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--s-2);
}
.plain li {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  gap: var(--s-1) var(--s-3);
}
.value {
  font-variant-numeric: tabular-nums;
  font-weight: 600;
}
</style>
