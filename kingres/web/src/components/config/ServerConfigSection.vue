<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The section "Configuration" of the server page: the server's group, the revision it holds, the overrides that
// apply to it, what waits for a restart (with the restart and its downtime note), what failed and why, and its clock
// and zones. Everything that comes from the server goes through SafeName.
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { useQuery } from '@tanstack/vue-query'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import TableWrap from '@/components/TableWrap.vue'
import ConfigValue from './ConfigValue.vue'
import RestartPendingDialog from './RestartPendingDialog.vue'
import { endpoints } from '@/api/endpoints'
import { fetchAllPages, keys, useClusterTime, useConfigServers } from '@/api/queries'
import {
  CLOCK_STATE,
  CONFIG_SERVER_STATE,
  DOWNTIME_NOTE,
  clockState,
  formatDifference,
  levelWords,
  withinMeasurement,
} from '@/lib/config'

const props = defineProps<{ serverId: string }>()

const servers = useConfigServers()
const time = useClusterTime()
const mine = computed(() => servers.data.value?.items.find((s) => s.serverId === props.serverId))
const clock = computed(() => time.data.value?.servers.find((s) => s.serverId === props.serverId))

/** Every parameter that has an override, to list the ones that apply to this server. */
const changed = useQuery({
  queryKey: [...keys.config, 'overrides'],
  queryFn: () =>
    fetchAllPages((cursor) =>
      endpoints.listConfigParameters({ changed: true, limit: 200, ...(cursor ? { cursor } : {}) }),
    ),
})
const overrides = computed(() => {
  const group = mine.value?.serverGroup ?? null
  return (changed.data.value?.items ?? []).flatMap((p) =>
    p.hidden
      ? []
      : p.overrides
          .filter(
            (o) =>
              o.level === 'cluster' ||
              (o.level === 'serverGroup' && group !== null && o.scopeId === group) ||
              (o.level === 'server' && o.scopeId === props.serverId),
          )
          .map((o) => ({ name: p.name, ...o })),
  )
})
const state = computed(() =>
  clock.value
    ? clockState(
        clock.value.reachable,
        clock.value.differenceMs,
        clock.value.uncertaintyMs,
        time.data.value?.maxDifferenceMs ?? 1000,
      )
    : 'cannotRead',
)
const restarting = ref(false)
</script>

<template>
  <section class="card stack" aria-labelledby="cfg-h" data-testid="server-config">
    <div class="row-between">
      <h2 id="cfg-h">Configuration</h2>
      <RouterLink :to="{ name: 'configuration' }">Open the Configuration page</RouterLink>
    </div>
    <p v-if="servers.isPending.value" class="muted" role="status">Loading.</p>
    <ProblemNotice
      v-else-if="servers.error.value"
      :error="servers.error.value"
      @retry="servers.refetch()"
    />
    <p v-else-if="!mine" class="muted">The configuration state of this server is not known.</p>
    <template v-else>
      <dl class="facts">
        <div>
          <dt>State</dt>
          <dd><StatusBadge :meta="CONFIG_SERVER_STATE[mine.state]" /></dd>
        </div>
        <div>
          <dt>Server group</dt>
          <dd>
            <SafeName v-if="mine.serverGroup" :value="mine.serverGroup" :max="60" />
            <span v-else class="muted">none</span>
          </dd>
        </div>
        <div>
          <dt>Revision held</dt>
          <dd>
            {{ mine.appliedRevision }}
            <span class="muted">of {{ servers.data.value?.revision }}</span>
          </dd>
        </div>
        <div>
          <dt>Overrides that apply</dt>
          <dd>{{ mine.overrideCount }}</dd>
        </div>
      </dl>
      <p v-if="mine.state === 'waiting' && mine.detail" class="notice tone-info" role="status">
        <span aria-hidden="true">&#9719;</span>
        <SafeName :value="mine.detail" kind="text" :max="300" />
      </p>
      <div v-if="mine.failed.length > 0" class="notice tone-danger" data-testid="config-failed">
        <span aria-hidden="true">&#10006;</span>
        <div class="stack">
          <strong>A change failed on this server</strong>
          <ul class="plain">
            <li v-for="f in mine.failed" :key="f.name">
              <code><SafeName :value="f.name" kind="text" :max="60" /></code>:
              <SafeName :value="f.error" kind="text" :max="300" multiline />
            </li>
          </ul>
        </div>
      </div>
      <div
        v-if="mine.pendingRestart.length > 0"
        class="notice tone-warn"
        data-testid="config-pending"
      >
        <span aria-hidden="true">&#9650;</span>
        <div class="stack">
          <strong>Waits for a restart</strong>
          <span>
            <template v-for="(n, i) in mine.pendingRestart" :key="n"
              ><code><SafeName :value="n" kind="text" :max="60" /></code
              ><template v-if="i < mine.pendingRestart.length - 1">, </template></template
            >
          </span>
          <span>{{ DOWNTIME_NOTE }}</span>
          <div class="row">
            <GateButton op="restartPendingInstances" small @click="restarting = true"
              >Restart this server's instances</GateButton
            >
            <span class="muted"
              >One instance of the server you are connected to can also be restarted on the
              <RouterLink :to="{ name: 'this-server' }">This server</RouterLink> page.</span
            >
          </div>
        </div>
      </div>

      <h3>Overrides that apply to this server</h3>
      <p v-if="changed.isPending.value" class="muted" role="status">Loading.</p>
      <ProblemNotice v-else-if="changed.error.value" :error="changed.error.value" />
      <p v-else-if="overrides.length === 0" class="muted">No value is set for this server.</p>
      <TableWrap v-else>
        <table class="table">
          <caption class="sr-only">
            Overrides that apply to this server
          </caption>
          <thead>
            <tr>
              <th scope="col">Level</th>
              <th scope="col">Parameter</th>
              <th scope="col">Value</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="o in overrides" :key="`${o.level}-${o.scopeId}-${o.name}`">
              <td>{{ levelWords(o.level) }}</td>
              <td>
                <code><SafeName :value="o.name" kind="text" :max="60" /></code>
              </td>
              <td><ConfigValue :value="o.value" /></td>
            </tr>
          </tbody>
        </table>
      </TableWrap>

      <h3>Clock and time zones</h3>
      <p v-if="!clock" class="muted">The clock of this server was not measured.</p>
      <dl v-else class="facts" data-testid="server-clock">
        <div>
          <dt>Difference from the leader</dt>
          <dd>
            <span v-if="clock.differenceMs === null" class="muted">cannot be read</span>
            <span v-else-if="withinMeasurement(clock.differenceMs, clock.uncertaintyMs)"
              >within the measurement</span
            >
            <span v-else>{{ formatDifference(clock.differenceMs, clock.uncertaintyMs) }}</span>
            <StatusBadge :meta="CLOCK_STATE[state]" />
          </dd>
        </div>
        <div>
          <dt>Operating system zone</dt>
          <dd><ConfigValue :value="clock.osTimeZone" /></dd>
        </div>
        <div>
          <dt>TimeZone</dt>
          <dd><ConfigValue :value="clock.timeZone" /></dd>
        </div>
        <div>
          <dt>log_timezone</dt>
          <dd><ConfigValue :value="clock.logTimeZone" /></dd>
        </div>
      </dl>
    </template>
  </section>
  <RestartPendingDialog v-if="restarting" :server-ids="[serverId]" @close="restarting = false" />
</template>

<style scoped>
.facts {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: var(--s-3);
  margin: 0;
}
.facts dt {
  color: var(--c-text-muted);
}
.facts dd {
  margin: 0;
}
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
}
</style>
