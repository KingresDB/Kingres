<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import ServerActions from '@/components/ServerActions.vue'
import NullableValue from '@/components/NullableValue.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useServer, useServerGroups } from '@/api/queries'
import { INSTANCE_STATE, SERVER_STATUS } from '@/lib/status'
import { instanceLabel } from '@/lib/cluster'
import { formatBytes, formatDateTime, formatDuration, formatLag, percent } from '@/lib/format'

const props = defineProps<{ id: string }>()
const client = useQueryClient()
const server = useServer(() => props.id)
const groups = useServerGroups()
const s = computed(() => server.data.value)
const dialog = ref<'failover' | 'remove' | null>(null)

// Server group assignment: PATCH /servers/{id} with serverGroup (null removes the server from its group).
const group = ref('')
const groupSaved = ref(false)
watch(
  () => s.value?.serverGroup,
  (g) => {
    group.value = g ?? ''
  },
  { immediate: true },
)
const assign = useMutation({
  mutationFn: () => endpoints.updateServer(props.id, { serverGroup: group.value || null }),
  onSuccess: () => {
    groupSaved.value = true
  },
  onSettled: () =>
    Promise.all([
      client.invalidateQueries({ queryKey: keys.server(props.id) }),
      client.invalidateQueries({ queryKey: keys.servers }),
      client.invalidateQueries({ queryKey: keys.serverGroups }),
      client.invalidateQueries({ queryKey: keys.cluster }),
    ]),
})
function saveGroup(): void {
  groupSaved.value = false
  assign.mutate()
}
</script>

<template>
  <PageHeader :title="s ? `Server ${s.address}` : `Server ${id}`" :subtitle="s?.hostname">
    <template #actions>
      <GateButton op="failoverServer" @click="dialog = 'failover'">Failover</GateButton>
      <GateButton op="removeServer" variant="danger" @click="dialog = 'remove'">Remove</GateButton>
    </template>
  </PageHeader>
  <p><RouterLink :to="{ name: 'servers' }">&larr; All servers</RouterLink></p>

  <QueryBoundary
    :loading="server.isPending.value"
    :error="server.error.value"
    @retry="server.refetch()"
  >
    <div v-if="s" class="stack-lg">
      <section class="grid-cards" aria-label="Summary">
        <div class="card kpi">
          <span class="label">Status</span>
          <span class="value"><StatusBadge :meta="SERVER_STATUS[s.status]" /></span>
          <span class="muted">Server group: {{ s.serverGroup ?? 'none' }}</span>
        </div>
        <div class="card kpi">
          <span class="label">Buckets (primary / replica)</span>
          <span class="value">{{ s.buckets.primary }} / {{ s.buckets.replica }}</span>
          <RouterLink :to="{ name: 'shards', query: { server: s.id } }">Show in Shards</RouterLink>
        </div>
        <div class="card kpi">
          <span class="label">Data size</span>
          <span class="value"><NullableValue :value="s.sizeBytes" :format="formatBytes" /></span>
          <span class="muted"
            >Max lag
            <NullableValue :value="s.replicationLagMaxSeconds" :format="(v) => formatLag(v)"
          /></span>
        </div>
        <div class="card kpi">
          <span class="label">Uptime</span>
          <span class="value"
            ><NullableValue :value="s.uptimeSeconds" :format="formatDuration"
          /></span>
          <span class="muted"
            >Joined <NullableValue :value="s.joinedAt" :format="formatDateTime"
          /></span>
          <span class="muted"
            >Last heartbeat <NullableValue :value="s.lastHeartbeatAt" :format="formatDateTime"
          /></span>
        </div>
      </section>

      <section class="card stack" aria-labelledby="grp-h">
        <h2 id="grp-h">Server group</h2>
        <form
          class="row"
          style="align-items: flex-end; gap: var(--s-4)"
          @submit.prevent="saveGroup"
        >
          <div class="field">
            <label for="srv-group">Group</label>
            <select id="srv-group" v-model="group">
              <option value="">None</option>
              <option v-for="g in groups.data.value?.items" :key="g.name" :value="g.name">
                {{ g.name }}
              </option>
            </select>
          </div>
          <GateButton op="updateServer" type="submit" :disabled="assign.isPending.value">
            Save group
          </GateButton>
        </form>
        <p class="hint muted">
          Changing the group can make the current placement invalid; a rebalance is then suggested.
        </p>
        <ProblemNotice v-if="assign.error.value" :error="assign.error.value" />
        <p v-if="groupSaved" class="notice tone-ok" role="status">
          <span aria-hidden="true">&#10003;</span> Server group saved.
        </p>
      </section>

      <section class="card" aria-labelledby="inst-h">
        <h2 id="inst-h">Instances</h2>
        <div class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th scope="col">Instance</th>
                <th scope="col">Role</th>
                <th scope="col">State</th>
                <th scope="col" class="num">Port</th>
                <th scope="col" class="num">Size</th>
                <th scope="col" class="num">Lag</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="i in s.instances" :key="i.id">
                <th scope="row" class="mono">#{{ i.id }}</th>
                <td>{{ instanceLabel(i) }}</td>
                <td><StatusBadge :meta="INSTANCE_STATE[i.state]" /></td>
                <td class="num">{{ i.port }}</td>
                <td class="num"><NullableValue :value="i.sizeBytes" :format="formatBytes" /></td>
                <td class="num">
                  <template v-if="i.role === 'standby'">
                    <NullableValue
                      :value="i.replicationLagSeconds ?? i.replicationLagBytes"
                      :format="() => formatLag(i.replicationLagSeconds, i.replicationLagBytes)"
                    />
                  </template>
                  <template v-else>-</template>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <section class="card" aria-labelledby="db-h">
        <h2 id="db-h">Buckets by database</h2>
        <p v-if="s.bucketsByDatabase.length === 0" class="muted">No sharded databases.</p>
        <div v-else class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th scope="col">Database</th>
                <th scope="col" class="num">Primary buckets</th>
                <th scope="col" class="num">Primary size</th>
                <th scope="col" class="num">Replica buckets</th>
                <th scope="col" class="num">Replica size</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="d in s.bucketsByDatabase" :key="d.databaseName">
                <th scope="row">
                  <RouterLink
                    :to="{ name: 'shards', query: { db: d.databaseName, server: s.id } }"
                    >{{ d.databaseName }}</RouterLink
                  >
                </th>
                <td class="num">{{ d.primaryBuckets }}</td>
                <td class="num">
                  <NullableValue :value="d.primarySizeBytes" :format="formatBytes" />
                </td>
                <td class="num">{{ d.replicaBuckets }}</td>
                <td class="num">
                  <NullableValue :value="d.replicaSizeBytes" :format="formatBytes" />
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <section class="card" aria-labelledby="res-h">
        <h2 id="res-h">Resources</h2>
        <dl class="kv">
          <dt>CPU</dt>
          <dd><NullableValue :value="s.resources.cpuUsagePercent" :format="percent" /></dd>
          <dt>Memory</dt>
          <dd>
            <NullableValue :value="s.resources.memoryUsedBytes" :format="formatBytes" /> of
            <NullableValue :value="s.resources.memoryTotalBytes" :format="formatBytes" />
          </dd>
          <dt>Disk</dt>
          <dd>
            <NullableValue :value="s.resources.diskUsedBytes" :format="formatBytes" /> of
            <NullableValue :value="s.resources.diskTotalBytes" :format="formatBytes" />
          </dd>
          <dt>Version</dt>
          <dd><NullableValue :value="s.version" /></dd>
        </dl>
      </section>
    </div>
  </QueryBoundary>

  <ServerActions
    v-if="dialog && s"
    :kind="dialog"
    :server="s"
    :invalidate="[keys.servers, keys.server(id), keys.topology]"
    @close="dialog = null"
  />
</template>

<style scoped>
.kv {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dt {
  color: var(--c-text-muted);
}
.kv dd {
  margin: 0;
  font-variant-numeric: tabular-nums;
}
</style>
