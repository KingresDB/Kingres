<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Rebalance: plan preview, start/stop, live progress and past reports.
import { computed, ref } from 'vue'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import RebalanceReportDialog from '@/components/RebalanceReportDialog.vue'
import NullableValue from '@/components/NullableValue.vue'
import RebalanceAutomaticCard from '@/components/RebalanceAutomaticCard.vue'
import { endpoints } from '@/api/endpoints'
import {
  keys,
  useClusterSettings,
  useDatabases,
  useRebalanceReports,
  useRebalanceStatus,
} from '@/api/queries'
import { useTaskAction } from '@/composables/useTask'
import { REBALANCE_REASON_LABEL, tolerant, type StatusMeta } from '@/lib/status'
import { formatBytes, formatDateTime, formatDuration, formatNumber } from '@/lib/format'
import type { RebalancePlan, RebalanceStatus } from '@/api/types'

const client = useQueryClient()
const status = useRebalanceStatus()
const reports = useRebalanceReports()
const databases = useDatabases()
const settings = useClusterSettings()

const st = computed(() => status.data.value)
const running = computed(() => st.value?.state === 'running' || st.value?.state === 'stopping')

const STATE_META: Record<RebalanceStatus['state'], StatusMeta> = tolerant({
  idle: { tone: 'neutral', icon: '○', label: 'Idle' },
  running: { tone: 'info', icon: '◐', label: 'Running' },
  stopping: { tone: 'warn', icon: '▲', label: 'Stopping' },
  stopped: { tone: 'warn', icon: '▲', label: 'Stopped (start again to go on)' },
  failed: { tone: 'danger', icon: '✖', label: 'Failed' },
})

// Options shared by preview and start.
const selectedDbs = ref<string[]>([])
const maxMoves = ref<number | null>(null)
// Contract 0.16.0: the strategy, the threshold, drain-only and the moves per server. Empty means the cluster setting.
const strategy = ref<'' | 'byBucketCount' | 'byDiskSize'>('')
const threshold = ref<number | null>(null)
const drainOnly = ref(false)
const perServer = ref<number | null>(null)
const options = computed(() => ({
  ...(selectedDbs.value.length ? { databases: selectedDbs.value } : {}),
  ...(maxMoves.value ? { maxConcurrentMoves: maxMoves.value } : {}),
  ...(strategy.value ? { strategy: strategy.value } : {}),
  ...(threshold.value !== null && threshold.value >= 0 ? { threshold: threshold.value } : {}),
  ...(drainOnly.value ? { drainOnly: true } : {}),
  ...(perServer.value ? { maxMovesPerServer: perServer.value } : {}),
}))
const TRIGGER_LABEL: Record<string, string> = {
  manual: 'started by a person',
  automatic: 'started by the automatic mode',
  drain: 'draining a server',
}
const STRATEGY_LABEL: Record<string, string> = {
  byBucketCount: 'by bucket count',
  byDiskSize: 'by disk size',
}
const loadText = (v: number | undefined | null, by: string | undefined): string =>
  v === undefined || v === null ? '-' : by === 'byDiskSize' ? formatBytes(v) : formatNumber(v)

const plan = ref<RebalancePlan | null>(null)
const previewMutation = useMutation({
  mutationFn: () => endpoints.previewRebalancePlan({ ...options.value, moveLimit: 100 }),
  onSuccess: (p) => {
    plan.value = p
  },
})

const start = useTaskAction(() => endpoints.startRebalance(options.value), {
  invalidate: () => [keys.rebalance, keys.rebalanceReports, keys.servers, keys.topology],
})
const confirmStop = ref(false)
const stopMutation = useMutation({
  mutationFn: endpoints.stopRebalance,
  onSettled: () => {
    confirmStop.value = false
    void client.invalidateQueries({ queryKey: keys.rebalance })
    void client.invalidateQueries({ queryKey: keys.tasks })
  },
})

// A stopped run can be ended for good (cancel of its paused task): it is recorded as cancelled, not failed.
const confirmEnd = ref(false)
const endMutation = useMutation({
  mutationFn: () => endpoints.cancelTask(st.value?.taskId ?? 0),
  onSettled: () => {
    confirmEnd.value = false
    void client.invalidateQueries({ queryKey: keys.rebalance })
    void client.invalidateQueries({ queryKey: keys.rebalanceReports })
    void client.invalidateQueries({ queryKey: keys.tasks })
  },
})

const detailId = ref<number | null>(null)

/** The phase of a move in flight, in words (the extension's phases). */
const PHASE_LABEL: Record<string, string> = {
  prepare: 'Preparing',
  copy: 'Copying',
  catchUp: 'Catching up',
  switch: 'Switching',
  cleanup: 'Cleaning up',
}
const stoppedByText = computed(() =>
  st.value?.stoppedBy === 'safetyRule' ? 'a condition of the cluster' : 'an operator',
)
</script>

<template>
  <PageHeader
    title="Rebalance"
    subtitle="Spread buckets evenly over the servers. Adding a server and rebalancing are two steps."
  >
    <template #actions>
      <GateButton
        op="previewRebalancePlan"
        :disabled="previewMutation.isPending.value"
        @click="previewMutation.mutate()"
        >Preview plan</GateButton
      >
      <GateButton
        v-if="st?.state === 'stopped' && st.taskId"
        op="cancelTask"
        variant="danger"
        data-testid="end-run"
        @click="confirmEnd = true"
        >End this run</GateButton
      >
      <GateButton v-if="running" op="stopRebalance" variant="danger" @click="confirmStop = true"
        >Stop rebalance</GateButton
      >
      <GateButton
        v-else
        op="startRebalance"
        variant="primary"
        :disabled="start.isBusy.value"
        @click="start.run()"
      >
        {{ st?.state === 'stopped' ? 'Start again' : 'Start rebalance' }}
      </GateButton>
    </template>
  </PageHeader>

  <!-- Outside the boundary: the answer to an action must show even when the status cannot be read
       (for example while the server answers "not available yet" for every rebalance operation). -->
  <div
    v-if="
      start.error.value ||
      previewMutation.error.value ||
      stopMutation.error.value ||
      endMutation.error.value
    "
    class="stack"
    style="margin-bottom: var(--s-3)"
  >
    <ProblemNotice v-if="start.error.value" :error="start.error.value" />
    <ProblemNotice v-if="previewMutation.error.value" :error="previewMutation.error.value" />
    <ProblemNotice v-if="stopMutation.error.value" :error="stopMutation.error.value" />
    <ProblemNotice v-if="endMutation.error.value" :error="endMutation.error.value" />
  </div>

  <QueryBoundary
    :loading="status.isPending.value"
    :error="status.error.value"
    @retry="status.refetch()"
  >
    <div v-if="st" class="stack-lg">
      <section class="card stack" aria-labelledby="status-h">
        <div class="row-between">
          <h2 id="status-h">Status</h2>
          <StatusBadge :meta="STATE_META[st.state]" />
        </div>
        <p v-if="st.trigger || st.strategy" class="muted" data-testid="run-kind">
          <template v-if="st.state !== 'idle'"
            >This run is {{ TRIGGER_LABEL[st.trigger ?? 'manual'] ?? st.trigger }}, planned
            {{ STRATEGY_LABEL[st.strategy ?? ''] ?? st.strategy }}, threshold
            {{ Math.round((st.threshold ?? 0) * 100) }} percent.</template
          >
        </p>
        <p v-if="st.needed && !running">
          A rebalance is needed:
          {{ st.reasons.map((r) => REBALANCE_REASON_LABEL[r]).join('; ') || 'buckets are uneven' }}.
        </p>
        <p v-else-if="!running" class="muted">No rebalance is needed.</p>
        <p
          v-if="st.state === 'stopped'"
          class="notice tone-warn"
          role="status"
          data-testid="stopped-note"
        >
          <span aria-hidden="true">&#9650;</span>
          <span>
            Stopped by {{ stoppedByText }}.
            <SafeName v-if="st.pausedReason" :value="st.pausedReason" kind="text" :max="300" />
            The buckets moved so far stay where they are; <strong>Start again</strong> goes on with
            the same run, plans the remaining moves again and tries every bucket afresh;
            <strong>End this run</strong> closes it for good.
          </span>
        </p>
        <p
          v-if="st.state === 'stopping'"
          class="notice tone-warn"
          role="status"
          data-testid="stopping-note"
        >
          <span aria-hidden="true">&#9650;</span>
          <span
            >Stopping: the move in flight is cancelled, or finishes if it is already
            switching.</span
          >
        </p>
        <p
          v-if="st.state === 'failed'"
          class="notice tone-danger"
          role="status"
          data-testid="failed-note"
        >
          <span aria-hidden="true">&#10006;</span>
          <span
            >The last run failed<template v-if="st.lastReport?.error"
              >: <SafeName :value="st.lastReport.error" kind="text" :max="300" /></template
            >. Its report lists every move.</span
          >
        </p>
        <p v-if="!running && st.lastReport" data-testid="last-report">
          Last run: <strong>{{ st.lastReport.outcome }}</strong>,
          {{ st.lastReport.totals.movesDone }} of {{ st.lastReport.totals.movesTotal }} moves,
          finished {{ formatDateTime(st.lastReport.finishedAt) }}.
          <button type="button" class="btn btn-link" @click="detailId = st.lastReport.id">
            Show report #{{ st.lastReport.id }}
          </button>
        </p>
        <template v-if="st.progress">
          <div class="row-between">
            <span class="num"
              >{{ st.progress.movesDone }} of {{ st.progress.movesTotal }} moves done,
              {{ st.progress.movesInFlight }} in flight,
              {{ st.progress.movesFailed }} failed<template v-if="st.progress.movesSkipped"
                >, {{ st.progress.movesSkipped }} skipped</template
              ></span
            >
            <span class="num">{{ Math.round(st.progress.percent) }}%</span>
          </div>
          <div
            class="progress"
            role="progressbar"
            aria-label="Rebalance progress"
            aria-valuemin="0"
            aria-valuemax="100"
            :aria-valuenow="Math.round(st.progress.percent)"
          >
            <span :style="{ width: `${st.progress.percent}%` }" />
          </div>
          <p class="muted">{{ formatBytes(st.progress.bytesMoved) }} moved</p>
        </template>
        <TableWrap v-if="st.inFlightMoves?.length">
          <table class="table">
            <caption class="sr-only">
              Moves in flight
            </caption>
            <thead>
              <tr>
                <th scope="col">Database</th>
                <th scope="col" class="num">Bucket</th>
                <th scope="col">From</th>
                <th scope="col">To</th>
                <th scope="col">Phase</th>
                <th scope="col" class="num">Done</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="m in st.inFlightMoves"
                :key="`${m.databaseName}-${m.colocationId}-${m.bucket}`"
              >
                <td><SafeName :value="m.databaseName" :max="60" /></td>
                <td class="num" :data-kind="m.kind ?? 'move'">
                  <template v-if="m.kind === 'schema'"
                    >Schema <SafeName :value="m.schemaName ?? ''" :max="60"
                  /></template>
                  <template v-else
                    >{{ m.bucket
                    }}<span v-if="m.kind === 'split'" class="muted"> (splitting)</span></template
                  >
                </td>
                <td>
                  <SafeName v-if="m.fromServerId" :value="m.fromServerId" :max="80" />
                  <template v-else>group {{ m.fromShardGroupId }}</template>
                </td>
                <td>
                  <SafeName v-if="m.toServerId" :value="m.toServerId" :max="80" />
                  <template v-else>group {{ m.toShardGroupId }}</template>
                </td>
                <td>
                  <span aria-hidden="true">&#9684;</span>
                  <SafeName :value="PHASE_LABEL[m.phase] ?? m.phase" kind="text" :max="40" />
                </td>
                <td class="num">{{ Math.round(m.percent) }}%</td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
        <p class="sr-only" aria-live="polite" role="status">
          {{
            running && st.progress
              ? `Rebalance ${Math.round(st.progress.percent)} percent done.`
              : ''
          }}
        </p>
      </section>

      <section class="card stack" aria-labelledby="opt-h">
        <h2 id="opt-h">Options</h2>
        <fieldset v-if="databases.data.value?.items.some((d) => d.sharding.enabled)">
          <legend>Databases (none selected means all)</legend>
          <label
            v-for="d in databases.data.value.items.filter((x) => x.sharding.enabled)"
            :key="d.name"
            class="check"
          >
            <input v-model="selectedDbs" type="checkbox" :value="d.name" :disabled="running" />
            <SafeName :value="d.name" :max="80" />
          </label>
        </fieldset>
        <div class="field" style="max-width: 20em">
          <label for="rb-strategy">Strategy</label>
          <select id="rb-strategy" v-model="strategy" :disabled="running">
            <option value="">
              The cluster setting ({{ settings.data.value?.rebalance.strategy ?? 'byBucketCount' }})
            </option>
            <option value="byBucketCount">By bucket count</option>
            <option value="byDiskSize">By disk size</option>
          </select>
          <span class="hint"
            >By disk size a bucket counts its bytes, so a few large buckets do not pile up on one
            server.</span
          >
        </div>
        <div class="field" style="max-width: 20em">
          <label for="rb-threshold">Threshold</label>
          <input
            id="rb-threshold"
            v-model.number="threshold"
            class="input num"
            type="number"
            min="0"
            max="1"
            step="0.01"
            placeholder="the strategy's default"
            :disabled="running"
          />
          <span class="hint"
            >The deviation from the ideal load that is accepted: 0.1 is 10 percent. Nothing moves
            while every server is inside it.</span
          >
        </div>
        <label class="check"
          ><input v-model="drainOnly" type="checkbox" :disabled="running" /> Only move buckets off
          servers that should not hold shards</label
        >
        <div class="field" style="max-width: 20em">
          <label for="rb-per">Moves per server</label>
          <input
            id="rb-per"
            v-model.number="perServer"
            class="input num"
            type="number"
            min="1"
            max="16"
            :placeholder="String(settings.data.value?.rebalance.maxMovesPerServer ?? 2)"
            :disabled="running"
          />
        </div>
        <div class="field" style="max-width: 20em">
          <label for="rb-max">Concurrent moves (one at a time in this version)</label>
          <input
            id="rb-max"
            v-model.number="maxMoves"
            class="input num"
            type="number"
            min="1"
            max="64"
            :placeholder="String(settings.data.value?.rebalance.maxConcurrentMoves ?? 4)"
            :disabled="running"
          />
          <span class="hint"
            >1 to 64. Recorded with the run; the server moves one bucket at a time for now.</span
          >
        </div>
      </section>

      <section v-if="plan" class="card stack" aria-labelledby="plan-h" data-testid="plan">
        <h2 id="plan-h">Plan preview</h2>
        <p v-if="plan.strategy" class="muted" data-testid="plan-strategy">
          Planned {{ STRATEGY_LABEL[plan.strategy] ?? plan.strategy
          }}<template v-if="plan.threshold !== undefined"
            >, threshold {{ Math.round(plan.threshold * 100) }} percent</template
          ><template v-if="plan.imbalance !== undefined"
            >; the servers are now up to {{ Math.round(plan.imbalance * 100) }} percent from the
            ideal</template
          >.
        </p>
        <p v-if="!plan.needed" class="muted">Nothing to move: the buckets are already even.</p>
        <template v-else>
          <p>
            <strong>{{ formatNumber(plan.totals.moveCount) }}</strong> moves,
            <NullableValue :value="plan.totals.totalBytes" :format="formatBytes" />, about
            <NullableValue
              :value="plan.totals.estimatedDurationSeconds"
              :format="formatDuration"
            />.
            <span v-if="plan.truncated" class="muted"
              >Only the first {{ plan.moves.length }} moves are listed.</span
            >
          </p>
          <ul v-if="plan.warnings.length">
            <li v-for="w in plan.warnings" :key="w" class="notice tone-warn">
              <span aria-hidden="true">&#9650;</span>
              <SafeName :value="w" kind="text" :max="300" />
            </li>
          </ul>
          <TableWrap>
            <table class="table">
              <caption class="sr-only">
                Primary buckets per server before and after
              </caption>
              <thead>
                <tr>
                  <th scope="col">Server</th>
                  <th scope="col" class="num">Before</th>
                  <th scope="col" class="num">After</th>
                  <th
                    v-if="plan.serverImpacts.some((i) => i.loadBefore !== undefined)"
                    scope="col"
                    class="num"
                  >
                    Load before / after
                  </th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="i in plan.serverImpacts" :key="i.serverId">
                  <th scope="row">Server {{ i.serverId }}</th>
                  <td class="num">{{ i.primaryBucketsBefore }}</td>
                  <td class="num">{{ i.primaryBucketsAfter }}</td>
                  <td v-if="i.loadBefore !== undefined" class="num">
                    {{ loadText(i.loadBefore, plan.strategy) }} /
                    {{ loadText(i.loadAfter, plan.strategy) }}
                  </td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
          <details>
            <summary>Moves ({{ plan.moves.length }})</summary>
            <TableWrap>
              <table class="table">
                <caption class="sr-only">
                  Moves of the plan
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Database</th>
                    <th scope="col" class="num">Bucket</th>
                    <th scope="col">From</th>
                    <th scope="col">To</th>
                    <th scope="col" class="num">Size</th>
                    <th scope="col">Why</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="m in plan.moves"
                    :key="`${m.databaseName}-${m.colocationId}-${m.bucket}`"
                  >
                    <td><SafeName :value="m.databaseName" :max="60" /></td>
                    <td class="num" :data-kind="m.kind ?? 'move'">
                      <template v-if="m.kind === 'schema'"
                        >Schema <SafeName :value="m.schemaName ?? ''" :max="60"
                      /></template>
                      <template v-else
                        >{{ m.bucket
                        }}<span v-if="m.kind === 'split'" class="muted">
                          (splitting)</span
                        ></template
                      >
                    </td>
                    <td>
                      <SafeName v-if="m.fromServerId" :value="m.fromServerId" :max="80" />
                      <template v-else>group {{ m.fromShardGroupId }}</template>
                    </td>
                    <td>
                      <SafeName v-if="m.toServerId" :value="m.toServerId" :max="80" />
                      <template v-else>group {{ m.toShardGroupId }}</template>
                    </td>
                    <td class="num">
                      <NullableValue :value="m.estimatedSizeBytes" :format="formatBytes" />
                    </td>
                    <td>
                      {{
                        m.reason === 'drain'
                          ? 'Drain'
                          : m.reason === 'balance'
                            ? 'Balance'
                            : (m.reason ?? '-')
                      }}
                    </td>
                  </tr>
                </tbody>
              </table>
            </TableWrap>
          </details>
        </template>
      </section>

      <RebalanceAutomaticCard :automatic="st.automatic" />

      <section class="card stack" aria-labelledby="rep-h">
        <h2 id="rep-h">Past reports</h2>
        <QueryBoundary
          :loading="reports.isPending.value"
          :error="reports.error.value"
          :empty="reports.data.value?.items.length === 0"
          empty-title="No reports yet"
          empty-hint="Every rebalance run leaves a report here."
          @retry="reports.refetch()"
        >
          <TableWrap>
            <table class="table">
              <caption class="sr-only">
                Past rebalance reports
              </caption>
              <thead>
                <tr>
                  <th scope="col">Report</th>
                  <th scope="col">Outcome</th>
                  <th scope="col">Started</th>
                  <th scope="col">Run</th>
                  <th scope="col" class="num">Moves</th>
                  <th scope="col" class="num">Data moved</th>
                  <th scope="col" class="num">Duration</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="r in reports.data.value?.items" :key="r.id">
                  <th scope="row">
                    <button type="button" class="btn btn-link" @click="detailId = r.id">
                      #{{ r.id }}
                    </button>
                  </th>
                  <td>
                    <StatusBadge
                      :meta="
                        r.outcome === 'completed'
                          ? { tone: 'ok', icon: '✓', label: 'Completed' }
                          : r.outcome === 'stopped'
                            ? { tone: 'warn', icon: '▲', label: 'Stopped' }
                            : { tone: 'danger', icon: '✖', label: 'Failed' }
                      "
                    />
                  </td>
                  <td>{{ formatDateTime(r.startedAt) }}</td>
                  <td>
                    {{ TRIGGER_LABEL[r.trigger ?? 'manual'] ?? r.trigger
                    }}<template v-if="r.strategy"
                      >, {{ STRATEGY_LABEL[r.strategy] ?? r.strategy }}</template
                    >
                  </td>
                  <td class="num">{{ r.totals.movesDone }} / {{ r.totals.movesTotal }}</td>
                  <td class="num">{{ formatBytes(r.totals.bytesMoved) }}</td>
                  <td class="num">{{ formatDuration(r.totals.durationSeconds) }}</td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
        </QueryBoundary>
        <p v-if="reports.data.value?.moreNotShown" class="notice tone-warn" role="status">
          <span aria-hidden="true">&#9650;</span>
          <span>More reports exist than are shown here; only the latest are listed.</span>
        </p>
      </section>
    </div>
  </QueryBoundary>

  <ConfirmDialog
    v-if="confirmEnd"
    title="End this run for good?"
    confirm-label="End this run"
    danger
    :busy="endMutation.isPending.value"
    @cancel="confirmEnd = false"
    @confirm="endMutation.mutate()"
  >
    <p>
      The stopped run is closed and recorded as <strong>cancelled</strong> in its report; it cannot
      be started again. Buckets already moved stay where they are. Start a new rebalance to plan the
      rest.
    </p>
  </ConfirmDialog>

  <ConfirmDialog
    v-if="confirmStop"
    title="Stop the rebalance?"
    confirm-label="Stop rebalance"
    danger
    :busy="stopMutation.isPending.value"
    @cancel="confirmStop = false"
    @confirm="stopMutation.mutate()"
  >
    <p>
      A move that has not reached its switch is cancelled and undone; a move that is switching
      finishes first. The rebalance then becomes <strong>stopped</strong>;
      <strong>Start again</strong>
      goes on with it. Buckets already moved stay where they are.
    </p>
  </ConfirmDialog>
  <RebalanceReportDialog v-if="detailId !== null" :report-id="detailId" @close="detailId = null" />
</template>

<style scoped>
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
