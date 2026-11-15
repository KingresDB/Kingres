<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { ref } from 'vue'
import TableWrap from '@/components/TableWrap.vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import { endpoints } from '@/api/endpoints'
import { keys, usePagedQuery } from '@/api/queries'
import { alertKindLabel, SEVERITY } from '@/lib/status'
import { formatAgo, formatDateTime } from '@/lib/format'

const client = useQueryClient()
const state = ref<'' | 'open' | 'acknowledged' | 'resolved'>('')
const { query: alerts, items } = usePagedQuery(
  () => [...keys.alerts, 'page'],
  (cursor) => endpoints.listAlerts({ cursor, limit: 100 }),
  10_000,
)
const shown = () => items.value.filter((a) => !state.value || a.state === state.value)
const ack = useMutation({
  mutationFn: (id: number) => endpoints.acknowledgeAlert(id),
  onSettled: () =>
    Promise.all([
      client.invalidateQueries({ queryKey: keys.alerts }),
      client.invalidateQueries({ queryKey: keys.cluster }),
    ]),
})
const STATE_LABEL = { open: 'Open', acknowledged: 'Acknowledged', resolved: 'Resolved' } as const
</script>

<template>
  <PageHeader title="Alerts" subtitle="Conditions that need attention." />
  <div class="field" style="max-width: 14em; margin-bottom: var(--s-3)">
    <label for="a-state">State</label>
    <select id="a-state" v-model="state">
      <option value="">All</option>
      <option value="open">Open</option>
      <option value="acknowledged">Acknowledged</option>
      <option value="resolved">Resolved</option>
    </select>
  </div>
  <ProblemNotice v-if="ack.error.value" :error="ack.error.value" />
  <QueryBoundary
    :loading="alerts.isPending.value"
    :error="alerts.error.value"
    :empty="shown().length === 0"
    empty-title="No alerts"
    empty-hint="Nothing matches this filter."
    @retry="alerts.refetch()"
  >
    <TableWrap>
      <table class="table">
        <caption class="sr-only">
          Alerts
        </caption>
        <thead>
          <tr>
            <th scope="col">Severity</th>
            <th scope="col">Alert</th>
            <th scope="col">State</th>
            <th scope="col">Raised</th>
            <th scope="col">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="a in shown()" :key="a.id">
            <td><StatusBadge :meta="SEVERITY[a.severity]" /></td>
            <td>
              <strong><SafeName :value="a.title" kind="text" :max="200" /></strong>
              <div class="muted alert-kind">
                <SafeName :value="alertKindLabel(a.kind)" kind="text" :max="80" />
              </div>
              <div class="muted">
                <SafeName :value="a.message" kind="text" :max="600" multiline />
              </div>
            </td>
            <td>
              {{ STATE_LABEL[a.state] }}
              <div v-if="a.acknowledgedBy" class="muted">
                by <SafeName :value="a.acknowledgedBy" :max="60" />
              </div>
            </td>
            <td :title="formatDateTime(a.raisedAt)">{{ formatAgo(a.raisedAt) }}</td>
            <td>
              <GateButton
                v-if="a.state === 'open'"
                op="acknowledgeAlert"
                small
                @click="ack.mutate(a.id)"
                >Acknowledge</GateButton
              >
            </td>
          </tr>
        </tbody>
      </table>
    </TableWrap>
  </QueryBoundary>
  <p v-if="alerts.hasNextPage.value" class="row">
    <span class="muted">More alerts exist than are shown.</span>
    <button
      type="button"
      class="btn"
      :disabled="alerts.isFetchingNextPage.value"
      @click="alerts.fetchNextPage()"
    >
      Load more
    </button>
  </p>
</template>
