<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The Time tab: the clock of every server and its difference from the answering server's, the time zones, what to do
// when a clock is off, the cluster time zone, the time zone of each database and the threshold of the alert. KingresDB
// measures and reports; it does not set the system clock (that is the job of NTP or chrony).
import { computed, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import TableWrap from '@/components/TableWrap.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import NotAvailable from '@/components/NotAvailable.vue'
import FormField from '@/components/FormField.vue'
import ConfigValue from './ConfigValue.vue'
import ConfigEditDialog from './ConfigEditDialog.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys, useClusterTime, useTimeZones } from '@/api/queries'
import {
  CLOCK_ADVICE,
  CLOCK_STATE,
  clockState,
  formatDifference,
  formatMs,
  withinMeasurement,
} from '@/lib/config'
import { formatDateTime } from '@/lib/format'
import { isAddressableSegment } from '@/lib/path-segment'
import { firstError, integerBetween } from '@/lib/validators'
import type { ServerTime } from '@/api/types'

const time = useClusterTime()
const zones = useTimeZones()
const client = useQueryClient()
const t = computed(() => time.data.value)

const rows = computed(() =>
  (t.value?.servers ?? []).map((s) => ({
    s,
    state: clockState(
      s.reachable,
      s.differenceMs,
      s.uncertaintyMs,
      t.value?.maxDifferenceMs ?? 1000,
    ),
    reference: s.serverId === t.value?.referenceServerId,
  })),
)
const exceeded = computed(() =>
  rows.value.some((r) => r.state === 'differs' || r.state === 'farOff'),
)

/** The value most servers have, to mark the ones that do not. */
function commonValue(pick: (s: ServerTime) => string | null): string | null {
  const counts = new Map<string, number>()
  for (const r of rows.value) {
    const v = r.s.reachable ? pick(r.s) : null
    if (v !== null) counts.set(v, (counts.get(v) ?? 0) + 1)
  }
  let best: string | null = null
  let n = 0
  for (const [v, c] of counts)
    if (c > n) {
      best = v
      n = c
    }
  return counts.size > 1 ? best : null
}
const common = computed(() => ({
  os: commonValue((s) => s.osTimeZone),
  tz: commonValue((s) => s.timeZone),
  log: commonValue((s) => s.logTimeZone),
}))

function clockText(iso: string | null): string {
  return iso === null ? '' : `${new Date(iso).toISOString().slice(11, 23)} UTC`
}

// --- the cluster time zone ------------------------------------------------------------------------------------

const zone = ref('')
const zoneTouched = ref(false)
watch(
  () => commonValue((s) => s.timeZone) ?? rows.value.find((r) => r.s.timeZone)?.s.timeZone ?? '',
  (v) => {
    if (!zoneTouched.value) zone.value = v
  },
  { immediate: true },
)
const dialog = ref<
  | { kind: 'cluster'; value: string }
  | { kind: 'database'; database: string; value: string | null }
  | null
>(null)
const zoneError = computed(() => {
  if (zone.value.trim() === '') return 'Choose a time zone.'
  const list = zones.data.value?.items
  return list && !list.some((z) => z.name.toLowerCase() === zone.value.toLowerCase())
    ? 'PostgreSQL does not know this time zone: pick one from the list.'
    : undefined
})
const zoneSubmitted = ref(false)
function openCluster(): void {
  zoneSubmitted.value = true
  if (zoneError.value) return
  dialog.value = { kind: 'cluster', value: zone.value }
}

// --- the threshold ------------------------------------------------------------------------------------------------

const limit = ref<number | null>(1000)
const limitTouched = ref(false)
watch(
  () => t.value?.maxDifferenceMs,
  (v) => {
    if (v !== undefined && !limitTouched.value) limit.value = v
  },
  { immediate: true },
)
const limitSubmitted = ref(false)
const limitError = computed(() =>
  firstError<number | null>(limit.value, integerBetween('The threshold', 10, 600000)),
)
const saved = ref(false)
const save = useMutation({
  mutationFn: () =>
    endpoints.updateClusterSettings({ clock: { maxDifferenceMs: limit.value as number } }),
  onSuccess: async () => {
    saved.value = true
    limitTouched.value = false
    await Promise.all([
      client.invalidateQueries({ queryKey: keys.clusterSettings }),
      client.invalidateQueries({ queryKey: keys.configTime }),
      client.invalidateQueries({ queryKey: keys.configSummary }),
      client.invalidateQueries({ queryKey: keys.alerts }),
    ])
  },
})
const serverError = computed(() =>
  save.error.value
    ? describeProblem(save.error.value).fieldErrors['clock.maxDifferenceMs']
    : undefined,
)
function submitLimit(): void {
  limitSubmitted.value = true
  saved.value = false
  if (limitError.value) return
  save.mutate()
}
</script>

<template>
  <div class="stack-lg">
    <QueryBoundary
      :loading="time.isPending.value"
      :error="time.error.value"
      @retry="time.refetch()"
    >
      <template v-if="t">
        <p class="muted" data-testid="measured-at">
          Measured at <time :datetime="t.measuredAt">{{ formatDateTime(t.measuredAt) }}</time>; the
          page measures again every 10 seconds. Differences are relative to server
          <SafeName :value="t.referenceServerId" kind="text" :max="60" />, the one that answered.
          Each difference is an estimate with the uncertainty shown beside it: a smaller difference
          cannot be told from zero.
        </p>
        <TableWrap>
          <table class="table">
            <caption class="sr-only">
              Clocks and time zones of the servers
            </caption>
            <thead>
              <tr>
                <th scope="col">Server</th>
                <th scope="col">Clock</th>
                <th scope="col">Difference</th>
                <th scope="col">State</th>
                <th scope="col">Operating system zone</th>
                <th scope="col">TimeZone</th>
                <th scope="col">log_timezone</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="r in rows" :key="r.s.serverId" :data-testid="`clock-${r.s.serverId}`">
                <th scope="row">
                  <RouterLink
                    v-if="isAddressableSegment(r.s.serverId)"
                    :to="{ name: 'server', params: { id: r.s.serverId } }"
                    ><SafeName :value="r.s.serverId" kind="text" :max="60"
                  /></RouterLink>
                  <SafeName v-else :value="r.s.serverId" kind="text" :max="60" />
                  <div v-if="r.reference" class="muted">answered this request</div>
                </th>
                <td class="mono nowrap">
                  <template v-if="r.s.reachable">{{ clockText(r.s.clock) }}</template>
                  <NotAvailable v-else />
                </td>
                <td>
                  <NotAvailable v-if="r.s.differenceMs === null" />
                  <template v-else-if="r.reference">0 (reference)</template>
                  <template v-else>
                    <span v-if="withinMeasurement(r.s.differenceMs, r.s.uncertaintyMs)"
                      >within the measurement</span
                    >
                    <span v-else class="num nowrap">{{
                      formatDifference(r.s.differenceMs, r.s.uncertaintyMs)
                    }}</span>
                    <div
                      v-if="withinMeasurement(r.s.differenceMs, r.s.uncertaintyMs)"
                      class="muted"
                    >
                      {{ formatMs(r.s.differenceMs, true) }} &plusmn;
                      {{ formatMs(r.s.uncertaintyMs ?? 0) }}
                    </div>
                  </template>
                </td>
                <td><StatusBadge :meta="CLOCK_STATE[r.state]" /></td>
                <td>
                  <ConfigValue :value="r.s.osTimeZone" />
                  <span
                    v-if="r.s.reachable && common.os !== null && r.s.osTimeZone !== common.os"
                    class="badge tone-warn"
                    ><span aria-hidden="true">&#8800;</span> differs from the others</span
                  >
                </td>
                <td>
                  <ConfigValue :value="r.s.timeZone" />
                  <span
                    v-if="r.s.reachable && common.tz !== null && r.s.timeZone !== common.tz"
                    class="badge tone-warn"
                    ><span aria-hidden="true">&#8800;</span> differs from the others</span
                  >
                </td>
                <td>
                  <ConfigValue :value="r.s.logTimeZone" />
                  <span
                    v-if="r.s.reachable && common.log !== null && r.s.logTimeZone !== common.log"
                    class="badge tone-warn"
                    ><span aria-hidden="true">&#8800;</span> differs from the others</span
                  >
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>

        <section
          v-if="exceeded"
          class="notice tone-warn"
          aria-labelledby="clock-advice"
          data-testid="clock-advice"
        >
          <span aria-hidden="true">&#9650;</span>
          <div class="stack">
            <h2 id="clock-advice">What to do about a clock that differs</h2>
            <p>{{ CLOCK_ADVICE.summary }}</p>
            <p>{{ CLOCK_ADVICE.chrony }} {{ CLOCK_ADVICE.checks }}</p>
            <p>{{ CLOCK_ADVICE.step }} {{ CLOCK_ADVICE.after }}</p>
          </div>
        </section>
        <p v-else class="muted" data-testid="clock-note">
          KingresDB does not set the system clock: that needs the rights of the operating system
          administrator and is the job of time synchronization (NTP or chrony).
        </p>

        <section class="stack" aria-labelledby="zone-h">
          <h2 id="zone-h">Cluster time zone</h2>
          <p class="muted">
            TimeZone and log_timezone must be equal on every server, so they are set together for
            the cluster. A change does not need a restart.
          </p>
          <form class="form-row" @submit.prevent="openCluster">
            <FormField
              v-slot="slot"
              label="Time zone"
              :error="zoneSubmitted ? zoneError : undefined"
            >
              <input
                v-bind="slot"
                v-model="zone"
                class="input mono"
                type="text"
                list="time-zone-names"
                autocomplete="off"
                spellcheck="false"
                @input="zoneTouched = true"
              />
            </FormField>
            <datalist id="time-zone-names">
              <option v-for="z in zones.data.value?.items ?? []" :key="z.name" :value="z.name">
                {{ z.utcOffset }}{{ z.isDaylightSaving ? ' (daylight saving time)' : '' }}
              </option>
            </datalist>
            <GateButton op="applyConfigChange" type="submit" variant="primary">Apply</GateButton>
          </form>
        </section>

        <section class="stack" aria-labelledby="dbzone-h">
          <h2 id="dbzone-h">Time zone of each database</h2>
          <p class="muted">
            A database can have a time zone of its own; it applies on every server. A role's own
            setting (ALTER ROLE ... SET TimeZone) beats it for that role, and the console does not
            show or change those.
          </p>
          <TableWrap>
            <table class="table">
              <caption class="sr-only">
                Time zone of each database
              </caption>
              <thead>
                <tr>
                  <th scope="col">Database</th>
                  <th scope="col">Time zone</th>
                  <th scope="col">Action</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="d in t.databases" :key="d.name" :data-testid="`dbzone-${d.name}`">
                  <th scope="row"><SafeName :value="d.name" :max="80" /></th>
                  <td>
                    <ConfigValue v-if="d.timeZone !== null" :value="d.timeZone" />
                    <span v-else class="muted">cluster default</span>
                  </td>
                  <td>
                    <GateButton
                      op="applyConfigChange"
                      small
                      @click="dialog = { kind: 'database', database: d.name, value: d.timeZone }"
                      >Change<span class="sr-only">
                        the time zone of this database</span
                      ></GateButton
                    >
                  </td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
        </section>

        <section class="stack" aria-labelledby="limit-h">
          <h2 id="limit-h">Alert threshold</h2>
          <p class="muted">
            A server whose clock differs from the leader's by more than this, less the uncertainty
            of the measurement, raises the alert "Server clock differs from the leader" (critical at
            ten times the threshold).
          </p>
          <form class="form-row" novalidate @submit.prevent="submitLimit">
            <FormField
              v-slot="slot"
              label="Threshold (milliseconds)"
              hint="10 to 600000. The default is 1000."
              :error="(limitSubmitted ? limitError : undefined) ?? serverError"
            >
              <input
                v-bind="slot"
                v-model.number="limit"
                class="input num"
                type="number"
                min="10"
                max="600000"
                @input="limitTouched = true"
              />
            </FormField>
            <GateButton
              op="updateClusterSettings"
              type="submit"
              variant="primary"
              :disabled="save.isPending.value"
              >Save threshold</GateButton
            >
          </form>
          <ProblemNotice v-if="save.error.value && !serverError" :error="save.error.value" />
          <p v-if="saved" class="notice tone-ok" role="status">
            <span aria-hidden="true">&#10003;</span> Threshold saved.
          </p>
        </section>
      </template>
    </QueryBoundary>
  </div>

  <ConfigEditDialog
    v-if="dialog?.kind === 'cluster'"
    name="TimeZone"
    :also-names="['log_timezone']"
    :preset="{ level: 'cluster', value: dialog.value, lockLevel: true }"
    title="Set the cluster time zone"
    @close="dialog = null"
  />
  <ConfigEditDialog
    v-else-if="dialog?.kind === 'database'"
    name="TimeZone"
    :preset="{
      level: 'database',
      scopeId: dialog.database,
      value: dialog.value ?? '',
      lockLevel: true,
    }"
    title="Set the time zone of a database"
    @close="dialog = null"
  />
</template>
