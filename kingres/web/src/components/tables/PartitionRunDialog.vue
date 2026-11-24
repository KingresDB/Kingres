<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// One run of a time-partitioning policy with its actions (contract 0.18.0, `getPartitionPolicyRun`): when it ran, why, who
// started it, how it ended, the error, and what each action did. A failed action says what happened in words when the scheduler's
// code is known. Read only.
import AppDialog from '../AppDialog.vue'
import NullableValue from '../NullableValue.vue'
import QueryBoundary from '../QueryBoundary.vue'
import SafeName from '../SafeName.vue'
import StatusBadge from '../StatusBadge.vue'
import PartitionActionsTable from './PartitionActionsTable.vue'
import { usePartitionRun } from '@/api/queries'
import { formatDateTime } from '@/lib/format'
import { RUN_STATE, TRIGGER_LABEL } from '@/lib/analytics'
import type { Table } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table; runId: number }>()
const emit = defineEmits<{ close: [] }>()
const run = usePartitionRun(
  () => props.databaseName,
  () => props.table.schema,
  () => props.table.name,
  () => props.runId,
)
</script>

<template>
  <AppDialog :title="`Run ${runId} of ${table.schema}.${table.name}`" wide @close="emit('close')">
    <QueryBoundary :loading="run.isPending.value" :error="run.error.value" @retry="run.refetch()">
      <div v-if="run.data.value" class="stack" data-testid="run-details">
        <dl class="kv">
          <dt>State</dt>
          <dd><StatusBadge :meta="RUN_STATE[run.data.value.state]" /></dd>
          <dt>Started</dt>
          <dd>{{ formatDateTime(run.data.value.startedAt) }}</dd>
          <dt>Finished</dt>
          <dd><NullableValue :value="run.data.value.finishedAt" :format="formatDateTime" /></dd>
          <dt>Why</dt>
          <dd>{{ TRIGGER_LABEL[run.data.value.trigger] ?? run.data.value.trigger }}</dd>
          <dt>Started by</dt>
          <dd><SafeName :value="run.data.value.startedBy" kind="text" :max="60" /></dd>
          <dt>Planned for</dt>
          <dd>{{ formatDateTime(run.data.value.asOf) }}</dd>
        </dl>
        <p
          v-if="run.data.value.error"
          class="notice tone-danger"
          role="alert"
          data-testid="run-error"
        >
          <span aria-hidden="true">&#10006;</span>
          <span><SafeName :value="run.data.value.error" kind="text" :max="400" multiline /></span>
        </p>
        <PartitionActionsTable
          v-if="run.data.value.actions.length > 0"
          :actions="run.data.value.actions"
          :table="table.name"
          caption="Actions of the run"
        />
        <p v-else class="muted">The run found nothing to do.</p>
      </div>
    </QueryBoundary>
    <template #footer>
      <button type="button" class="btn btn-primary" @click="emit('close')">Close</button>
    </template>
  </AppDialog>
</template>

<style scoped>
.kv {
  display: grid;
  grid-template-columns: fit-content(45%) minmax(0, 1fr);
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dt {
  color: var(--c-text-muted);
}
.kv dd {
  margin: 0;
}
</style>
