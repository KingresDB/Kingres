<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// One rebalance report with the outcome of every move (GET /rebalance/reports/{id}).
import { useQuery } from '@tanstack/vue-query'
import SafeName from './SafeName.vue'
import TableWrap from './TableWrap.vue'
import AppDialog from './AppDialog.vue'
import QueryBoundary from './QueryBoundary.vue'
import NullableValue from './NullableValue.vue'
import { computed } from 'vue'
import { endpoints } from '@/api/endpoints'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'

const props = defineProps<{ reportId: number }>()
const emit = defineEmits<{ close: [] }>()

const report = useQuery({
  queryKey: ['rebalanceReport', props.reportId],
  queryFn: () => endpoints.getRebalanceReport(props.reportId),
})
/** Why a move was skipped; a reason a newer server sends is shown as it is. */
const SKIP_REASON: Record<string, string> = {
  stopped: 'cancelled by the stop',
  targetDiskLow: 'the target server has little disk space',
  groupDegraded: 'a shard group needs a synchronous standby it does not have',
  groupLeaseLost: 'a shard group lost its lease',
}
/** Before and after side by side, one row per database and server. */
const placement = computed(() => {
  const r = report.data.value
  if (!r) return []
  const rows = new Map<
    string,
    { databaseName: string; serverId: string; before: number; after: number | null }
  >()
  for (const p of r.placementBefore ?? [])
    rows.set(`${p.databaseName}|${p.serverId}`, { ...p, before: p.primaryBuckets, after: null })
  const ended = (r.placementAfter ?? []).length > 0
  for (const p of r.placementAfter ?? []) {
    const k = `${p.databaseName}|${p.serverId}`
    const cur = rows.get(k)
    if (cur) cur.after = p.primaryBuckets
    else
      rows.set(k, {
        databaseName: p.databaseName,
        serverId: p.serverId,
        before: 0,
        after: p.primaryBuckets,
      })
  }
  if (ended) for (const v of rows.values()) if (v.after === null) v.after = 0
  return [...rows.values()]
})
const OUTCOME: Record<string, { icon: string; cls: string; label: string }> = {
  done: { icon: '✓', cls: 'tone-ok', label: 'Done' },
  failed: { icon: '✖', cls: 'tone-danger', label: 'Failed' },
  skipped: { icon: '○', cls: 'tone-neutral', label: 'Skipped' },
}
/** An outcome a newer server sends is shown as it is, not as a crash (clients tolerate unknown values). */
function outcomeOf(o: string): { icon: string; cls: string; label: string } {
  return OUTCOME[o] ?? { icon: '?', cls: 'tone-neutral', label: o }
}
/** How the run ended, in words. */
const RUN_OUTCOME: Record<string, string> = {
  completed: 'completed',
  stopped: 'stopped',
  failed: 'failed',
  cancelled: 'cancelled by an operator',
}
</script>

<template>
  <AppDialog :title="`Rebalance report #${reportId}`" wide @close="emit('close')">
    <QueryBoundary
      :loading="report.isPending.value"
      :error="report.error.value"
      @retry="report.refetch()"
    >
      <template v-if="report.data.value">
        <dl class="kv">
          <dt>Outcome</dt>
          <dd data-testid="report-outcome">
            {{ RUN_OUTCOME[report.data.value.outcome] ?? report.data.value.outcome }}
          </dd>
          <dt>Started</dt>
          <dd>{{ formatDateTime(report.data.value.startedAt) }}</dd>
          <dt>Finished</dt>
          <dd>{{ formatDateTime(report.data.value.finishedAt) }}</dd>
          <template v-if="report.data.value.outcome === 'stopped'">
            <dt>Stopped by</dt>
            <dd data-testid="report-stopped-by">
              {{ report.data.value.stoppedBy === 'safetyRule' ? 'a safety rule' : 'an operator' }}
            </dd>
          </template>
          <template v-if="report.data.value.error">
            <dt>Reason</dt>
            <dd><SafeName :value="report.data.value.error" kind="text" :max="300" /></dd>
          </template>
          <dt>Moves</dt>
          <dd>
            {{ report.data.value.totals.movesDone }} done,
            {{ report.data.value.totals.movesFailed }} failed,
            {{ report.data.value.totals.movesSkipped ?? 0 }} skipped,
            {{ report.data.value.totals.movesTotal }} planned
          </dd>
          <dt>Data moved</dt>
          <dd>{{ formatBytes(report.data.value.totals.bytesMoved) }}</dd>
        </dl>
        <TableWrap v-if="placement.length" data-testid="report-placement">
          <table class="table">
            <caption class="sr-only">
              Primary buckets per server before and after the run
            </caption>
            <thead>
              <tr>
                <th scope="col">Database</th>
                <th scope="col">Server</th>
                <th scope="col" class="num">Before</th>
                <th scope="col" class="num">After</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="p in placement" :key="`${p.databaseName}-${p.serverId}`">
                <td><SafeName :value="p.databaseName" :max="60" /></td>
                <td><SafeName :value="p.serverId" :max="80" /></td>
                <td class="num">{{ p.before }}</td>
                <td class="num">{{ p.after === null ? 'n/a' : p.after }}</td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
        <p v-if="report.data.value.moves.length === 0" class="muted">
          This report has no per-move detail.
        </p>
        <TableWrap v-else data-testid="report-moves">
          <table class="table">
            <caption class="sr-only">
              Moves of the report
            </caption>
            <thead>
              <tr>
                <th scope="col">Database</th>
                <th scope="col" class="num">Bucket</th>
                <th scope="col">From</th>
                <th scope="col">To</th>
                <th scope="col" class="num">Tries</th>
                <th scope="col" class="num">Size</th>
                <th scope="col" class="num">Duration</th>
                <th scope="col">Outcome</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="(m, i) in report.data.value.moves"
                :key="`${i}-${m.databaseName}-${m.colocationId}-${m.bucket}`"
              >
                <td><SafeName :value="m.databaseName" :max="60" /></td>
                <td class="num" :data-kind="m.kind ?? 'move'">
                  <template v-if="m.kind === 'schema'"
                    >Schema <SafeName :value="m.schemaName ?? ''" :max="60"
                  /></template>
                  <template v-else>{{ m.bucket }}</template>
                </td>
                <td>
                  <SafeName v-if="m.fromServerId" :value="m.fromServerId" :max="80" />
                  <template v-else>group {{ m.fromShardGroupId }}</template>
                </td>
                <td>
                  <SafeName v-if="m.toServerId" :value="m.toServerId" :max="80" />
                  <template v-else>group {{ m.toShardGroupId }}</template>
                </td>
                <td class="num">{{ m.attempts ?? 'n/a' }}</td>
                <td class="num"><NullableValue :value="m.sizeBytes" :format="formatBytes" /></td>
                <td class="num">
                  <NullableValue :value="m.durationSeconds" :format="formatDuration" />
                </td>
                <td>
                  <span class="badge" :class="outcomeOf(m.outcome).cls"
                    ><span aria-hidden="true">{{ outcomeOf(m.outcome).icon }}</span>
                    {{ outcomeOf(m.outcome).label }}</span
                  >
                  <span v-if="m.skipReason" class="muted" data-testid="skip-reason">
                    ({{ SKIP_REASON[m.skipReason] ?? m.skipReason }})</span
                  >
                  <div v-if="m.error" class="muted">
                    <SafeName :value="m.error" kind="text" :max="300" />
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </template>
    </QueryBoundary>
  </AppDialog>
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
