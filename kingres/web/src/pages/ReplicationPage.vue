<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Each shard group with its primary and standbys: state, sync state and lag of every member, whether a commit now
// needs a standby, and the lease of the group (0.7.0). With chained placement a server runs the primary of one group
// and a standby of a neighbouring group. An administrator changes the replica count here; the manager leader then
// adds or removes standbys in a task.
import { computed, nextTick, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import GroupLeaseInfo from '@/components/GroupLeaseInfo.vue'
import ReplicaCountDialog from '@/components/ReplicaCountDialog.vue'
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import { endpoints } from '@/api/endpoints'
import {
  keys,
  useAlerts,
  useClusterSettings,
  useServers,
  useStandbyTask,
  useTopology,
} from '@/api/queries'
import { GROUP_HEALTH, INSTANCE_STATE, SYNC_STATE } from '@/lib/status'
import { groupLabel } from '@/lib/cluster'
import { commitRule, managedStandbyId } from '@/lib/standby'
import { formatBytes, formatLag } from '@/lib/format'
import { useSessionStore } from '@/stores/session'

const session = useSessionStore()
const route = useRoute()
const topology = useTopology()
const groups = computed(() => topology.data.value?.groups ?? [])
const settings = useClusterSettings()
const servers = useServers()
const canAlerts = computed(() => session.can('listAlerts'))
const alerts = useAlerts('open', canAlerts)
const standbyTask = useStandbyTask()
const lastTask = computed(() => standbyTask.data.value?.items[0] ?? null)
const displaced = computed(() =>
  groups.value.filter((g) => g.primary && !g.primary.isOnPreferredServer),
)
const restoring = ref(false)
const changingCount = ref(false)

const replicaCount = computed(() => settings.data.value?.defaultReplicaCount ?? null)
const serverCount = computed(() => servers.data.value?.items.length ?? 0)
/** Groups with fewer standbys than the replica count asks for. */
const underReplicated = computed(() =>
  replicaCount.value === null
    ? 0
    : groups.value.filter((g) => g.standbys.length < (replicaCount.value as number)).length,
)

function membersOf(g: (typeof groups.value)[number]) {
  return [...(g.primary ? [g.primary] : []), ...g.standbys]
}
const rule = (g: (typeof groups.value)[number]) =>
  commitRule(g, canAlerts.value ? (alerts.data.value?.items ?? null) : null)

// A link from an alert (`/replication#g-2`) lands on its group once the groups are drawn.
watch(
  [() => route.hash, groups],
  async ([hash]) => {
    if (!hash || groups.value.length === 0) return
    await nextTick()
    const el = document.getElementById(hash.slice(1))
    if (el) el.scrollIntoView({ block: 'start' })
  },
  { immediate: true },
)
</script>

<template>
  <PageHeader
    title="Replication"
    subtitle="Primary, standbys, leases and commit rule of every group."
  >
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

  <section class="card stack" style="margin-bottom: var(--s-4)" aria-labelledby="rc-h">
    <h2 id="rc-h">Standbys per shard group</h2>
    <div class="row">
      <span>
        Replica count:
        <strong class="num" data-testid="replica-count">
          <template v-if="replicaCount !== null">{{ replicaCount }}</template>
          <template v-else-if="settings.isPending.value">...</template>
          <template v-else>not available</template>
        </strong>
      </span>
      <GateButton
        op="updateClusterSettings"
        :disabled="replicaCount === null"
        @click="changingCount = true"
      >
        Change replica count
      </GateButton>
    </div>
    <p class="muted">
      Every shard group gets this many standbys, as far as the servers allow. The manager leader
      adds or removes standbys in the task "Add or remove standbys", one group at a time, and checks
      every ten seconds that they still match.
      <span v-if="underReplicated > 0" data-testid="under-replicated-note"
        >{{ underReplicated }} group(s) have fewer standbys than that now.</span
      >
    </p>
    <div v-if="lastTask" class="stack" data-testid="standby-task">
      <h3>{{ lastTask.state === 'running' ? 'Standby task running' : 'Last standby task' }}</h3>
      <TaskProgress :task="lastTask" />
    </div>
  </section>

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
      <section
        v-for="g in groups"
        :id="`g-${g.id}`"
        :key="g.id"
        class="card stack"
        :aria-labelledby="`g-${g.id}-h`"
        :data-testid="`group-${g.id}`"
      >
        <div class="row-between">
          <h2 :id="`g-${g.id}-h`">{{ groupLabel(g.id, g.isMapGroup) }}</h2>
          <span class="row">
            <span class="muted">Preferred primary: server {{ g.preferredPrimaryServerId }}</span>
            <StatusBadge :meta="GROUP_HEALTH[g.health]" />
          </span>
        </div>
        <div class="grid-2">
          <div class="stack" data-testid="commit-rule" :data-state="rule(g).state">
            <h3>Does a commit wait for a standby?</h3>
            <p class="row">
              <StatusBadge
                :meta="{ tone: rule(g).tone, icon: rule(g).icon, label: rule(g).label }"
              />
            </p>
            <p>{{ rule(g).text }}</p>
          </div>
          <div class="stack">
            <h3>Lease of the primary</h3>
            <GroupLeaseInfo :lease="g.lease" />
          </div>
        </div>
        <TableWrap>
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
              <tr v-for="m in membersOf(g)" :key="m.instanceId">
                <th scope="row" class="mono">
                  #{{ m.instanceId }}
                  <div
                    v-if="managedStandbyId(m) !== null"
                    class="muted"
                    data-testid="managed-standby"
                  >
                    standby {{ managedStandbyId(m) }}, managed by kingresd
                  </div>
                </th>
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
                  <template v-if="m.syncState">
                    <StatusBadge :meta="SYNC_STATE[m.syncState]" />
                    <div class="muted">
                      {{
                        m.syncState === 'sync' || m.syncState === 'quorum'
                          ? 'A commit can wait for it'
                          : 'A commit does not wait for it'
                      }}
                    </div>
                  </template>
                  <span v-else class="muted">-</span>
                </td>
                <td class="num">
                  <template v-if="m.role === 'standby'">
                    <template v-if="m.lagSeconds === null && m.lagBytes === null"
                      ><span class="muted">not streaming</span></template
                    >
                    <template v-else>
                      {{ formatLag(m.lagSeconds, m.lagBytes)
                      }}<span v-if="m.lagBytes !== null" class="muted">
                        ({{ formatBytes(m.lagBytes) }})</span
                      >
                    </template>
                  </template>
                  <template v-else>-</template>
                </td>
                <td class="num">{{ m.timeline }}</td>
              </tr>
              <tr v-if="!g.primary">
                <td colspan="7">
                  <StatusBadge :meta="{ tone: 'danger', icon: '✖', label: 'No primary' }" /> This
                  group has no primary and its buckets are unavailable.
                </td>
              </tr>
              <tr v-if="g.primary && g.standbys.length === 0">
                <td colspan="7">
                  <StatusBadge :meta="{ tone: 'neutral', icon: '○', label: 'No standby' }" /> This
                  group has no standby.
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
        <details v-if="g.slots.length">
          <summary>Replication slots ({{ g.slots.length }})</summary>
          <ul>
            <li v-for="s in g.slots" :key="s.name">
              <code><SafeName :value="s.name" kind="text" :max="80" /></code>, {{ s.type }},
              {{ s.active ? 'active' : 'inactive' }}, retains
              {{ formatBytes(s.retainedWalBytes) }} of WAL
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
  <ReplicaCountDialog
    v-if="changingCount && replicaCount !== null"
    :current="replicaCount"
    :server-count="serverCount"
    @close="changingCount = false"
  />
</template>
