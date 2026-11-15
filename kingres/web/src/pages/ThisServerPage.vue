<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// "This server": the node that serves this panel. These pages keep working when the node is
// standalone or cut off from the rest of the cluster.
import { ref, watch } from 'vue'
import { safeText } from '@/lib/safe-text'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import { RouterLink } from 'vue-router'
import { useQuery } from '@tanstack/vue-query'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import NullableValue from '@/components/NullableValue.vue'
import OwnFingerprint from '@/components/OwnFingerprint.vue'
import ResourceMeter from '@/components/ResourceMeter.vue'
import LocalSettingsPanel from '@/components/this-server/LocalSettingsPanel.vue'
import InstanceActionDialog from '@/components/this-server/InstanceActionDialog.vue'
import LeaveClusterDialog from '@/components/this-server/LeaveClusterDialog.vue'
import { rememberInstances } from '@/api/availability'
import { endpoints } from '@/api/endpoints'
import { every, keys, POLL, useThisServer, useThisServerInstances } from '@/api/queries'
import { INSTANCE_STATE, SERVICE_STATE } from '@/lib/status'
import { instanceLabel } from '@/lib/cluster'
import { formatBytes, formatDuration, percent } from '@/lib/format'
import type { Instance } from '@/api/types'

const info = useThisServer()
const resources = useQuery({
  queryKey: [...keys.thisServer, 'resources'],
  queryFn: endpoints.getThisServerResources,
  refetchInterval: every(POLL.fast),
})
const instances = useThisServerInstances()
const action = ref<
  { kind: 'restart' | 'stop' | 'start'; instance: Instance } | { kind: 'leave' } | null
>(null)

// The instance that hosts the system database must be known when it is stopped: after that no
// list can be read any more, and "start" needs its id.
watch(
  () => instances.data.value?.items,
  (items) => {
    if (items) rememberInstances(items)
  },
  { immediate: true },
)
const percentText = (v: number) => percent(v)
</script>

<template>
  <PageHeader
    title="This server"
    :subtitle="
      info.data.value ? `${info.data.value.hostname} (${info.data.value.address})` : undefined
    "
  >
    <template #actions>
      <RouterLink class="btn" :to="{ name: 'this-server-logs' }">Local logs</RouterLink>
      <GateButton op="leaveCluster" variant="danger" @click="action = { kind: 'leave' }"
        >Leave cluster</GateButton
      >
    </template>
  </PageHeader>

  <QueryBoundary :loading="info.isPending.value" :error="info.error.value" @retry="info.refetch()">
    <div v-if="info.data.value" class="stack-lg">
      <section class="grid-cards" aria-label="Summary">
        <div class="card kpi">
          <span class="label">Node state</span
          ><span class="value"
            ><SafeName :value="info.data.value.nodeState" kind="text" :max="40" /></span
          ><span class="muted"
            >Server id <SafeName :value="info.data.value.serverId ?? 'none'" kind="text" :max="60"
          /></span>
        </div>
        <div class="card kpi">
          <span class="label">Version</span
          ><span class="value"
            ><SafeName :value="info.data.value.version" kind="text" :max="40" /></span
          ><span class="muted"
            >PostgreSQL <NullableValue :value="info.data.value.postgresVersion"
          /></span>
        </div>
        <div class="card kpi">
          <span class="label">Uptime</span
          ><span class="value">{{ formatDuration(info.data.value.uptimeSeconds) }}</span
          ><span class="muted">{{
            info.data.value.isManagerLeader ? 'Manager leader' : 'Not the manager leader'
          }}</span>
        </div>
      </section>

      <section class="card stack" aria-labelledby="fp-h" data-testid="this-server-fingerprint">
        <h2 id="fp-h">Fingerprint of this server</h2>
        <OwnFingerprint
          :fingerprint="info.data.value.internalFingerprint"
          purpose="The SHA-256 fingerprint of the certificate that the internal API of this server presents now. An administrator who joins a new server to this cluster through this server (the join form of the new server) pins it; give it to them by a way other than the network between the servers."
          fallback="This server does not report its fingerprint. Read it as the SHA-256 of the file pki/server.crt in its data directory."
        />
      </section>

      <section class="card stack" aria-labelledby="svc-h">
        <h2 id="svc-h">Services</h2>
        <ul class="plain-list">
          <li v-for="s in info.data.value.services" :key="s.name" class="row">
            <span class="mono" style="min-width: 7em"
              ><SafeName :value="s.name" kind="text" :max="60" /></span
            ><StatusBadge :meta="SERVICE_STATE[s.state]" /><span v-if="s.detail" class="muted"
              ><SafeName :value="s.detail" kind="text" :max="300"
            /></span>
          </li>
        </ul>
      </section>

      <section class="card stack" aria-labelledby="inst-h">
        <h2 id="inst-h">Instances</h2>
        <QueryBoundary
          :loading="instances.isPending.value"
          :error="instances.error.value"
          :empty="instances.data.value?.items.length === 0"
          empty-title="No instances on this server"
        >
          <TableWrap>
            <table class="table">
              <caption class="sr-only">
                Local instances
              </caption>
              <thead>
                <tr>
                  <th scope="col">Instance</th>
                  <th scope="col">Role</th>
                  <th scope="col">State</th>
                  <th scope="col" class="num">Port</th>
                  <th scope="col" class="num">Size</th>
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="i in instances.data.value?.items" :key="i.id">
                  <th scope="row" class="mono">#{{ i.id }}</th>
                  <td>{{ instanceLabel(i) }}</td>
                  <td><StatusBadge :meta="INSTANCE_STATE[i.state]" /></td>
                  <td class="num">{{ i.port }}</td>
                  <td class="num"><NullableValue :value="i.sizeBytes" :format="formatBytes" /></td>
                  <td>
                    <div class="row">
                      <GateButton
                        v-if="i.state === 'stopped' || i.state === 'failed'"
                        op="startThisServerInstance"
                        small
                        @click="action = { kind: 'start', instance: i }"
                        >Start</GateButton
                      >
                      <span v-else-if="i.state !== 'running'" class="muted" role="status"
                        >{{ INSTANCE_STATE[i.state].label }}...</span
                      >
                      <template v-else>
                        <GateButton
                          op="restartThisServerInstance"
                          small
                          @click="action = { kind: 'restart', instance: i }"
                          >Restart</GateButton
                        >
                        <GateButton
                          op="stopThisServerInstance"
                          small
                          variant="danger"
                          @click="action = { kind: 'stop', instance: i }"
                          >Stop</GateButton
                        >
                      </template>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
        </QueryBoundary>
      </section>

      <section class="card stack" aria-labelledby="res-h">
        <h2 id="res-h">Resources</h2>
        <QueryBoundary :loading="resources.isPending.value" :error="resources.error.value">
          <div v-if="resources.data.value" class="stack">
            <ResourceMeter
              label="CPU"
              name="CPU usage"
              percent
              :used="resources.data.value.cpu.usagePercent"
              :total="resources.data.value.cpu.usagePercent === null ? null : 100"
              :format="(v) => percentText(v)"
            >
              <template #label
                >CPU (<NullableValue :value="resources.data.value.cpu.cores" /> cores)</template
              >
            </ResourceMeter>
            <ResourceMeter
              label="Memory"
              name="Memory usage"
              :used="resources.data.value.memory.usedBytes"
              :total="resources.data.value.memory.totalBytes"
              :format="formatBytes"
            />
            <ResourceMeter
              v-for="d in resources.data.value.disks"
              :key="d.path"
              :label="`Disk ${safeText(d.path)} (${d.purpose})`"
              :name="`Disk usage of ${safeText(d.path)}`"
              :used="d.usedBytes"
              :total="d.totalBytes"
              :format="formatBytes"
            />
          </div>
        </QueryBoundary>
      </section>

      <LocalSettingsPanel
        :instances="instances.data.value?.items ?? []"
        @restart="(i) => (action = { kind: 'restart', instance: i })"
      />
    </div>
  </QueryBoundary>

  <LeaveClusterDialog
    v-if="action?.kind === 'leave'"
    :server-name="info.data.value?.hostname ?? ''"
    @close="action = null"
  />
  <InstanceActionDialog
    v-else-if="action"
    :kind="action.kind"
    :instance="action.instance"
    @close="action = null"
  />
</template>

<style scoped>
.plain-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--s-2);
}
.kv {
  display: grid;
  grid-template-columns: fit-content(45%) minmax(0, 1fr);
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dd {
  margin: 0;
}
</style>
