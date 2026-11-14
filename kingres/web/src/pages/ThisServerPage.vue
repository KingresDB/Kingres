<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// "This server": the node that serves this panel. These pages keep working when the node is
// standalone or cut off from the rest of the cluster.
import { computed, reactive, ref, watch } from 'vue'
import { safeText } from '@/lib/safe-text'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import { RouterLink } from 'vue-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import NullableValue from '@/components/NullableValue.vue'
import ResourceMeter from '@/components/ResourceMeter.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import { vBlockEnterSubmit } from '@/lib/forms'
import { endpoints } from '@/api/endpoints'
import { every, keys, POLL, useThisServer } from '@/api/queries'
import { INSTANCE_STATE, SERVICE_STATE } from '@/lib/status'
import { instanceLabel } from '@/lib/cluster'
import { formatBytes, formatDuration, percent } from '@/lib/format'
import type { Instance, LogLevel } from '@/api/types'
import type { components } from '@/api/schema'

type S = components['schemas']

const client = useQueryClient()
const info = useThisServer()
const resources = useQuery({
  queryKey: [...keys.thisServer, 'resources'],
  queryFn: endpoints.getThisServerResources,
  refetchInterval: every(POLL.fast),
})
const instances = useQuery({
  queryKey: [...keys.thisServer, 'instances'],
  queryFn: endpoints.listThisServerInstances,
  refetchInterval: every(POLL.fast),
})
const local = useQuery({
  queryKey: [...keys.thisServer, 'settings'],
  queryFn: endpoints.getThisServerSettings,
})

const action = ref<
  { kind: 'restart' | 'stop' | 'start'; instance: Instance } | { kind: 'leave' } | null
>(null)
const force = ref(false)
// "Force" belongs to the action it was ticked for: every new dialog starts unticked.
watch(action, () => {
  force.value = false
})

const form = reactive({ memory: 4096, logLevel: 'info' as LogLevel })
watch(
  () => local.data.value,
  (d) => {
    if (d) {
      form.memory = d.memoryQuota.totalMegabytes
      form.logLevel = d.logLevel
    }
  },
  { immediate: true },
)
const memoryError = computed(() =>
  form.memory >= 512 ? undefined : 'The memory quota must be at least 512 MB.',
)
/** Only what was changed; an untouched field is not sent back. */
function localChanges(): S['LocalSettingsUpdate'] {
  const d = local.data.value
  const body: S['LocalSettingsUpdate'] = {}
  if (!d || form.memory !== d.memoryQuota.totalMegabytes)
    body.memoryQuota = { totalMegabytes: form.memory }
  if (!d || form.logLevel !== d.logLevel) body.logLevel = form.logLevel
  return body
}
const saveSettings = useMutation({
  mutationFn: () => endpoints.updateThisServerSettings(localChanges()),
  onSuccess: () => client.invalidateQueries({ queryKey: keys.thisServer }),
})

const startAction = () => {
  const a = action.value
  if (!a) throw new Error('No action selected.')
  return a.kind === 'leave'
    ? endpoints.leaveCluster(force.value)
    : endpoints.instanceAction(a.instance.id, a.kind, force.value)
}
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
          ><span class="value">{{ info.data.value.nodeState }}</span
          ><span class="muted">Server id {{ info.data.value.serverId ?? 'none' }}</span>
        </div>
        <div class="card kpi">
          <span class="label">Version</span><span class="value">{{ info.data.value.version }}</span
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

      <section class="card stack" aria-labelledby="set-h">
        <h2 id="set-h">Local settings</h2>
        <QueryBoundary :loading="local.isPending.value" :error="local.error.value">
          <form
            v-if="local.data.value"
            v-block-enter-submit
            class="stack"
            style="max-width: 32em"
            @submit.prevent="saveSettings.mutate()"
          >
            <p v-if="local.data.value.restartRequired" class="notice tone-warn" role="status">
              <span aria-hidden="true">&#9650;</span> A restart of this server is required for the
              memory quota to take effect.
            </p>
            <dl class="kv">
              <dt>Data directory</dt>
              <dd class="mono">
                <SafeName
                  :value="local.data.value.dataPaths.dataDirectory"
                  kind="text"
                  :max="200"
                />
              </dd>
              <dt>Client ports</dt>
              <dd>
                read-write {{ local.data.value.clientPorts.readWritePort
                }}<template v-if="local.data.value.clientPorts.readOnlyPort"
                  >, read-only {{ local.data.value.clientPorts.readOnlyPort }}</template
                >
              </dd>
            </dl>
            <div class="field">
              <label for="ls-mem">Memory quota (MB)</label
              ><input
                id="ls-mem"
                v-model.number="form.memory"
                class="input num"
                type="number"
                min="512"
                :aria-invalid="memoryError ? 'true' : undefined"
              /><span v-if="memoryError" class="error" role="alert">{{ memoryError }}</span>
            </div>
            <div class="field">
              <label for="ls-level">Log level</label
              ><select id="ls-level" v-model="form.logLevel">
                <option value="debug">debug</option>
                <option value="info">info</option>
                <option value="warning">warning</option>
                <option value="error">error</option>
              </select>
            </div>
            <ProblemNotice v-if="saveSettings.error.value" :error="saveSettings.error.value" />
            <div>
              <GateButton
                op="updateThisServerSettings"
                type="submit"
                variant="primary"
                :disabled="!!memoryError || saveSettings.isPending.value"
                >Save local settings</GateButton
              >
            </div>
          </form>
        </QueryBoundary>
      </section>
    </div>
  </QueryBoundary>

  <TaskActionDialog
    v-if="action"
    :title="
      action.kind === 'leave'
        ? 'Leave the cluster'
        : `${action.kind[0]?.toUpperCase()}${action.kind.slice(1)} instance #${action.instance.id}`
    "
    :confirm-label="
      action.kind === 'leave'
        ? 'Leave cluster'
        : `${action.kind[0]?.toUpperCase()}${action.kind.slice(1)} instance`
    "
    :danger="action.kind !== 'start'"
    :type-to-confirm="action.kind === 'leave' ? 'leave' : undefined"
    :start="startAction"
    :invalidate="[keys.thisServer, keys.servers]"
    @close="action = null"
  >
    <template v-if="action.kind === 'leave'">
      <p class="notice tone-danger" role="alert">
        <span aria-hidden="true">&#10006;</span>
        <span
          >This server drains its data, leaves the cluster and becomes a standalone server. Its
          panel keeps working, but it no longer serves the cluster.</span
        >
      </p>
    </template>
    <template v-else-if="action.kind === 'stop'">
      <p>
        Instance #{{ action.instance.id }} ({{ instanceLabel(action.instance) }}) stops.
        <template v-if="action.instance.role === 'primary'"
          >Its group has no primary until a standby is promoted; its buckets are unavailable
          meanwhile.</template
        >
      </p>
    </template>
    <template v-else-if="action.kind === 'restart'">
      <p>
        Instance #{{ action.instance.id }} ({{ instanceLabel(action.instance) }}) restarts.
        Connections to it are dropped for a short time.
      </p>
    </template>
    <template v-else
      ><p>Start instance #{{ action.instance.id }}.</p></template
    >
    <label v-if="action.kind !== 'start'" class="check"
      ><input v-model="force" type="checkbox" /> Force (do not wait for clients or
      replication)</label
    >
  </TaskActionDialog>
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
