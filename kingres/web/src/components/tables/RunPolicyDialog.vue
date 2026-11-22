<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// "Run now" for a time-partitioning policy (contract 0.18.0, `runPartitionPolicy`, kingres_admin; the task `partition.maintain`).
// The dialog first asks the server what the run would do (`planPartitionPolicy`, a dry run) and lists it, so that a person sees
// what would be dropped before confirming. A second run while one goes on is 409 `operation_in_progress`.
import { useQuery } from '@tanstack/vue-query'
import { computed } from 'vue'
import TaskActionDialog from '../TaskActionDialog.vue'
import ProblemNotice from '../ProblemNotice.vue'
import PartitionActionsTable from './PartitionActionsTable.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { formatBytes } from '@/lib/format'
import type { Table } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table }>()
const emit = defineEmits<{ close: [] }>()
const plan = useQuery({
  queryKey: computed(() => [
    'partitionPlan',
    props.databaseName,
    props.table.schema,
    props.table.name,
  ]),
  queryFn: () =>
    endpoints.planPartitionPolicy(props.databaseName, props.table.schema, props.table.name),
  retry: false,
  staleTime: 0,
  gcTime: 0,
})
const actions = computed(() => plan.data.value?.actions ?? [])
const removed = computed(() =>
  actions.value.filter((a) => a.kind === 'drop' || a.kind === 'detach'),
)
const dropped = computed(() => actions.value.filter((a) => a.kind === 'drop'))
const droppedBytes = computed(() => dropped.value.reduce((a, x) => a + (x.bytesBefore ?? 0), 0))
const start = () =>
  endpoints.runPartitionPolicy(props.databaseName, props.table.schema, props.table.name)
</script>

<template>
  <TaskActionDialog
    :title="`Run the policy of ${table.schema}.${table.name} now`"
    confirm-label="Run now"
    :start="start"
    :confirm-disabled="plan.isPending.value"
    :invalidate="[
      keys.partitionPolicy(databaseName, table.schema, table.name),
      keys.partitionRuns(databaseName, table.schema, table.name),
      keys.tablePartitions(databaseName, table.schema, table.name),
      keys.table(databaseName, table.schema, table.name),
      keys.tables(databaseName),
      keys.tasks,
    ]"
    cancelable
    cancelled-text="The run was cancelled. Actions that were finished stay; the rest is tried again at the next run."
    wide
    @close="emit('close')"
  >
    <p>The run does what the policy says, once, now. This is what it would do:</p>
    <div v-if="plan.isPending.value" class="muted" role="status" data-testid="run-plan-loading">
      Reading the plan...
    </div>
    <ProblemNotice v-else-if="plan.error.value" :error="plan.error.value" @retry="plan.refetch()" />
    <template v-else>
      <PartitionActionsTable
        v-if="actions.length > 0"
        :actions="actions"
        :table="table.name"
        caption="What the run would do"
      />
      <p v-else class="muted" data-testid="run-plan-empty">The run would find nothing to do.</p>
      <p
        v-if="dropped.length > 0"
        class="notice tone-danger"
        role="alert"
        data-testid="run-plan-deletes"
      >
        <span aria-hidden="true">&#10006;</span>
        <span>
          The run drops {{ dropped.length }}
          {{ dropped.length === 1 ? 'partition' : 'partitions' }} and deletes
          {{ formatBytes(droppedBytes) }} of data. This cannot be undone.
        </span>
      </p>
      <p v-else-if="removed.length > 0" class="muted">
        Detached partitions keep their rows as tables of their own.
      </p>
    </template>
  </TaskActionDialog>
</template>
