<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Each shard group with its primary and standbys, sync state and lag. With chained placement a
// server runs the primary of one group and a standby of a neighbouring group.
import { computed, ref } from 'vue'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useTopology } from '@/api/queries'
import { GROUP_HEALTH, INSTANCE_STATE, SYNC_STATE } from '@/lib/status'
import { groupLabel } from '@/lib/cluster'
import { formatBytes, formatLag } from '@/lib/format'

const topology = useTopology()
const groups = computed(() => topology.data.value?.groups ?? [])
const displaced = computed(() =>
  groups.value.filter((g) => g.primary && !g.primary.isOnPreferredServer),
)
const restoring = ref(false)
</script>

<template>
  <PageHeader title="Replication" subtitle="Primary and standbys of every group.">
    <template #actions>
      <GateButton
        op="restorePreferredPrimaries"
        :disabled="displaced.length === 0"
        @click="restoring = true"
      >
        Restore preferred primaries
      </GateButton>
    </template>
  </PageHeader>

  <QueryBoundary
    :loading="topology.isPending.value"
    :error="topology.error.value"
    :empty="groups.length === 0"
    empty-title="No shard groups"
    @retry="topology.refetch()"
  >
    <div class="stack-lg">
      <p
        v-if="displaced.length"
        class="notice tone-warn"
        role="status"
        data-testid="displaced-note"
      >
        <span aria-hidden="true">&#9650;</span>
        <span>
          {{ displaced.length }} group(s) run their primary away from the preferred server and
          double up the load on a neighbour. Restore the preferred primaries once the server is
          back.
        </span>
      </p>
      <section v-for="g in groups" :key="g.id" class="card stack" :aria-labelledby="`g-${g.id}`">
        <div class="row-between">
          <h2 :id="`g-${g.id}`">{{ groupLabel(g.id, g.isMapGroup) }}</h2>
          <span class="row">
            <span class="muted">Preferred primary: server {{ g.preferredPrimaryServerId }}</span>
            <StatusBadge :meta="GROUP_HEALTH[g.health]" />
          </span>
        </div>
        <div class="table-wrap">
          <table class="table">
            <caption class="sr-only">
              Members of
              {{
                groupLabel(g.id, g.isMapGroup)
              }}
            </caption>
            <thead>
              <tr>
                <th scope="col">Instance</th>
                <th scope="col">Server</th>
                <th scope="col">Role</th>
                <th scope="col">State</th>
                <th scope="col">Sync state</th>
                <th scope="col" class="num">Lag</th>
                <th scope="col" class="num">Timeline</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="m in [...(g.primary ? [g.primary] : []), ...g.standbys]"
                :key="m.instanceId"
              >
                <th scope="row" class="mono">#{{ m.instanceId }}</th>
                <td>
                  Server {{ m.serverId }}
                  <span
                    v-if="m.role === 'primary' && !m.isOnPreferredServer"
                    class="badge tone-warn"
                    ><span aria-hidden="true">&#9650;</span> Not preferred</span
                  >
                </td>
                <td>{{ m.role }}</td>
                <td><StatusBadge :meta="INSTANCE_STATE[m.state]" /></td>
                <td>
                  <StatusBadge v-if="m.syncState" :meta="SYNC_STATE[m.syncState]" /><span
                    v-else
                    class="muted"
                    >-</span
                  >
                </td>
                <td class="num">
                  {{ m.role === 'standby' ? formatLag(m.lagSeconds, m.lagBytes) : '-'
                  }}<span v-if="m.role === 'standby' && m.lagBytes !== null" class="muted">
                    ({{ formatBytes(m.lagBytes) }})</span
                  >
                </td>
                <td class="num">{{ m.timeline }}</td>
              </tr>
              <tr v-if="!g.primary">
                <td colspan="7">
                  <StatusBadge :meta="{ tone: 'danger', icon: '✖', label: 'No primary' }" /> This
                  group has no primary and its buckets are unavailable.
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <details v-if="g.slots.length">
          <summary>Replication slots ({{ g.slots.length }})</summary>
          <ul>
            <li v-for="s in g.slots" :key="s.name">
              <code>{{ s.name }}</code>, {{ s.type }}, {{ s.active ? 'active' : 'inactive' }},
              retains {{ formatBytes(s.retainedWalBytes) }} of WAL
            </li>
          </ul>
        </details>
      </section>
    </div>
  </QueryBoundary>

  <TaskActionDialog
    v-if="restoring"
    title="Restore preferred primaries"
    confirm-label="Restore primaries"
    :start="() => endpoints.restorePreferredPrimaries()"
    :invalidate="[keys.topology, keys.servers]"
    @close="restoring = false"
  >
    <p>
      {{ displaced.length }} shard group(s) switch back to their home server. Connections to those
      primaries are interrupted for a moment.
    </p>
  </TaskActionDialog>
</template>
