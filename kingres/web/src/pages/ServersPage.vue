<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import ServerActions from '@/components/ServerActions.vue'
import ServerGroupsPanel from '@/components/ServerGroupsPanel.vue'
import NullableValue from '@/components/NullableValue.vue'
import { useServers, keys } from '@/api/queries'
import { SERVER_STATUS } from '@/lib/status'
import { instanceLabel } from '@/lib/cluster'
import { formatBytes, formatDuration, formatLag, percent } from '@/lib/format'
import type { Server } from '@/api/types'

const servers = useServers()
const rows = computed(() => servers.data.value?.items ?? [])

/** The dialog being shown: which action on which server. */
const dialog = ref<{ kind: 'add' | 'restore' | 'failover' | 'remove'; server?: Server } | null>(
  null,
)
</script>

<template>
  <PageHeader title="Servers" subtitle="Every server in the cluster, its instances and its load.">
    <template #actions>
      <GateButton op="restorePreferredPrimaries" @click="dialog = { kind: 'restore' }">
        Restore preferred primaries
      </GateButton>
      <GateButton op="addServer" variant="primary" @click="dialog = { kind: 'add' }"
        >Add server</GateButton
      >
    </template>
  </PageHeader>

  <QueryBoundary
    :loading="servers.isPending.value"
    :error="servers.error.value"
    :empty="rows.length === 0"
    empty-title="No servers"
    @retry="servers.refetch()"
  >
    <div class="table-wrap">
      <table class="table">
        <caption class="sr-only">
          Servers
        </caption>
        <thead>
          <tr>
            <th scope="col">Server</th>
            <th scope="col">Status</th>
            <th scope="col">Server group</th>
            <th scope="col">Instances</th>
            <th scope="col" class="num">Buckets (primary / replica)</th>
            <th scope="col" class="num">Size</th>
            <th scope="col" class="num">Max lag</th>
            <th scope="col">Resources</th>
            <th scope="col">Version</th>
            <th scope="col">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="s in rows" :key="s.id" :data-testid="`server-row-${s.id}`">
            <th scope="row">
              <RouterLink :to="{ name: 'server', params: { id: s.id } }">{{
                s.address
              }}</RouterLink>
              <div class="muted">{{ s.hostname }}</div>
              <div v-if="s.isManagerLeader" class="muted">Manager leader</div>
            </th>
            <td><StatusBadge :meta="SERVER_STATUS[s.status]" /></td>
            <td>{{ s.serverGroup ?? '-' }}</td>
            <td>
              <ul class="plain">
                <li v-for="i in s.instances" :key="i.id" class="nowrap">
                  {{ instanceLabel(i)
                  }}<span v-if="i.state !== 'running'" class="muted"> ({{ i.state }})</span>
                </li>
                <li v-if="s.instances.length === 0" class="muted">None</li>
              </ul>
            </td>
            <td class="num">{{ s.buckets.primary }} / {{ s.buckets.replica }}</td>
            <td class="num nowrap"><NullableValue :value="s.sizeBytes" :format="formatBytes" /></td>
            <td class="num nowrap">
              <NullableValue :value="s.replicationLagMaxSeconds" :format="(v) => formatLag(v)" />
            </td>
            <td>
              <div>
                CPU <NullableValue :value="s.resources.cpuUsagePercent" :format="percent" />
              </div>
              <div class="muted">
                Mem <NullableValue :value="s.resources.memoryUsedBytes" :format="formatBytes" /> /
                <NullableValue :value="s.resources.memoryTotalBytes" :format="formatBytes" />
              </div>
              <div class="muted">
                Disk <NullableValue :value="s.resources.diskUsedBytes" :format="formatBytes" /> /
                <NullableValue :value="s.resources.diskTotalBytes" :format="formatBytes" />
              </div>
            </td>
            <td>
              <NullableValue :value="s.version" />
              <div class="muted">
                up <NullableValue :value="s.uptimeSeconds" :format="formatDuration" />
              </div>
            </td>
            <td>
              <div class="row">
                <GateButton
                  op="failoverServer"
                  small
                  @click="dialog = { kind: 'failover', server: s }"
                  >Failover</GateButton
                >
                <GateButton
                  op="removeServer"
                  small
                  variant="danger"
                  @click="dialog = { kind: 'remove', server: s }"
                  >Remove</GateButton
                >
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </QueryBoundary>

  <div style="margin-top: var(--s-5)"><ServerGroupsPanel /></div>

  <ServerActions
    v-if="dialog"
    :kind="dialog.kind"
    :server="dialog.server"
    :invalidate="[keys.servers, keys.topology]"
    @close="dialog = null"
  />
</template>

<style scoped>
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
}
</style>
