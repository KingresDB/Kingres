<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { ref } from 'vue'
import SafeName from '../SafeName.vue'
import TableWrap from '../TableWrap.vue'
import QueryBoundary from '../QueryBoundary.vue'
import { endpoints } from '@/api/endpoints'
import { keys, usePagedQuery } from '@/api/queries'
import { formatDateTime } from '@/lib/format'
import { auditActions } from '@/api/operations.generated'
import type { AuditAction } from '@/api/types'

const actor = ref('')
const outcome = ref<'' | 'success' | 'failure' | 'denied'>('')
// The action filter is the closed AuditAction enum of the contract.
const action = ref<'' | AuditAction>('')
const { query, items } = usePagedQuery(
  () => [...keys.audit, actor.value, outcome.value, action.value],
  (cursor) =>
    endpoints.listAuditEvents({
      cursor,
      limit: 50,
      actor: actor.value || undefined,
      outcome: outcome.value || undefined,
      action: action.value || undefined,
    }),
  15_000,
)
const OUTCOME = {
  success: { tone: 'ok', icon: '✓', label: 'Success' },
  failure: { tone: 'danger', icon: '✖', label: 'Failure' },
  denied: { tone: 'warn', icon: '▲', label: 'Denied' },
} as const
</script>

<template>
  <div class="stack">
    <div class="row" style="gap: var(--s-4)">
      <div class="field">
        <label for="au-actor">Actor</label><input id="au-actor" v-model="actor" class="input" />
      </div>
      <div class="field">
        <label for="au-act">Action</label>
        <select id="au-act" v-model="action">
          <option value="">All</option>
          <option v-for="a in auditActions" :key="a" :value="a">{{ a }}</option>
        </select>
      </div>
      <div class="field">
        <label for="au-out">Outcome</label>
        <select id="au-out" v-model="outcome">
          <option value="">All</option>
          <option value="success">Success</option>
          <option value="failure">Failure</option>
          <option value="denied">Denied</option>
        </select>
      </div>
    </div>
    <QueryBoundary
      :loading="query.isPending.value"
      :error="query.error.value"
      :empty="items.length === 0"
      empty-title="No audit events"
      empty-hint="Sign-ins and administrative actions are recorded here."
      @retry="query.refetch()"
    >
      <TableWrap>
        <table class="table">
          <caption class="sr-only">
            Audit log
          </caption>
          <thead>
            <tr>
              <th scope="col">Time</th>
              <th scope="col">Actor</th>
              <th scope="col">Source</th>
              <th scope="col">Action</th>
              <th scope="col">Resource</th>
              <th scope="col">Outcome</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="e in items" :key="e.id">
              <td>{{ formatDateTime(e.timestamp) }}</td>
              <td>
                <SafeName :value="e.actor" :max="60" />
                <div v-if="e.actorAddress" class="muted mono">
                  <SafeName :value="e.actorAddress" kind="text" :max="60" />
                </div>
              </td>
              <td>{{ e.source }}</td>
              <td class="mono">{{ e.action }}</td>
              <td>
                <SafeName
                  v-if="e.resourceType"
                  :value="`${e.resourceType} ${e.resourceId ?? ''}`"
                  :max="120"
                />
                <template v-else>-</template>
              </td>
              <td>
                <span class="badge" :class="`tone-${OUTCOME[e.outcome].tone}`"
                  ><span aria-hidden="true">{{ OUTCOME[e.outcome].icon }}</span>
                  {{ OUTCOME[e.outcome].label }}</span
                >
              </td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
      <button
        v-if="query.hasNextPage.value"
        type="button"
        class="btn"
        :disabled="query.isFetchingNextPage.value"
        @click="query.fetchNextPage()"
      >
        Load more
      </button>
    </QueryBoundary>
  </div>
</template>
