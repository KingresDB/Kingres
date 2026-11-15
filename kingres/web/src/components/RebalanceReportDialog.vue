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
import { endpoints } from '@/api/endpoints'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'

const props = defineProps<{ reportId: number }>()
const emit = defineEmits<{ close: [] }>()

const report = useQuery({
  queryKey: ['rebalanceReport', props.reportId],
  queryFn: () => endpoints.getRebalanceReport(props.reportId),
})
const OUTCOME = {
  done: { icon: '✓', cls: 'tone-ok', label: 'Done' },
  failed: { icon: '✖', cls: 'tone-danger', label: 'Failed' },
  skipped: { icon: '○', cls: 'tone-neutral', label: 'Skipped' },
} as const
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
          <dd>{{ report.data.value.outcome }}</dd>
          <dt>Started</dt>
          <dd>{{ formatDateTime(report.data.value.startedAt) }}</dd>
          <dt>Finished</dt>
          <dd>{{ formatDateTime(report.data.value.finishedAt) }}</dd>
          <dt>Moves</dt>
          <dd>
            {{ report.data.value.totals.movesDone }} done,
            {{ report.data.value.totals.movesFailed }} failed,
            {{ report.data.value.totals.movesTotal }} planned
          </dd>
          <dt>Data moved</dt>
          <dd>{{ formatBytes(report.data.value.totals.bytesMoved) }}</dd>
        </dl>
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
                <th scope="col">From group</th>
                <th scope="col">To group</th>
                <th scope="col" class="num">Size</th>
                <th scope="col" class="num">Duration</th>
                <th scope="col">Outcome</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="m in report.data.value.moves"
                :key="`${m.databaseName}-${m.colocationId}-${m.bucket}`"
              >
                <td><SafeName :value="m.databaseName" :max="60" /></td>
                <td class="num">{{ m.bucket }}</td>
                <td>{{ m.fromShardGroupId }}</td>
                <td>{{ m.toShardGroupId }}</td>
                <td class="num"><NullableValue :value="m.sizeBytes" :format="formatBytes" /></td>
                <td class="num">
                  <NullableValue :value="m.durationSeconds" :format="formatDuration" />
                </td>
                <td>
                  <span class="badge" :class="OUTCOME[m.outcome].cls"
                    ><span aria-hidden="true">{{ OUTCOME[m.outcome].icon }}</span>
                    {{ OUTCOME[m.outcome].label }}</span
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
