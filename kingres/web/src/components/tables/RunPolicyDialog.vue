<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// "Run now" for a time-partitioning policy (contract 0.18.0, `runPartitionPolicy`, kingres_admin; the task `partition.maintain`).
// The dialog first asks the server what the run would do (`planPartitionPolicy`, a dry run) and lists it, so that a person sees
// which partitions would be dropped before confirming. The run cannot start while the plan is being read, when it could not be
// read, or when it is older than a minute (it is read again). The button is red when the plan drops partitions. The panel never
// sends `asOf`: the run takes the time of the server. A second run while one goes on is 409 `operation_in_progress`.
import { useQuery } from '@tanstack/vue-query'
import { computed } from 'vue'
import TaskActionDialog from '../TaskActionDialog.vue'
import ProblemNotice from '../ProblemNotice.vue'
import PartitionActionsTable from './PartitionActionsTable.vue'
import PlanRemovals from './PlanRemovals.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { formatDateTime } from '@/lib/format'
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
  refetchOnWindowFocus: false,
  // A plan older than a minute is read again, so that the list above the button is what the run would do now.
  refetchInterval: 60_000,
})
const actions = computed(() => plan.data.value?.actions ?? [])
const dropped = computed(() => actions.value.filter((a) => a.kind === 'drop'))
const detached = computed(() => actions.value.filter((a) => a.kind === 'detach'))
const held = computed(
  () => plan.isPending.value || plan.isFetching.value || plan.isError.value || !plan.data.value,
)
const start = () =>
  endpoints.runPartitionPolicy(props.databaseName, props.table.schema, props.table.name)
</script>

<template>
  <TaskActionDialog
    :title="`Run the policy of ${table.schema}.${table.name} now`"
    confirm-label="Run now"
    :danger="dropped.length > 0"
    :start="start"
    :confirm-disabled="held"
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
    <ProblemNotice
      v-else-if="plan.isError.value"
      :error="plan.error.value"
      @retry="plan.refetch()"
    />
    <template v-else-if="plan.data.value">
      <p class="muted" role="status" data-testid="run-plan-asof">
        Plan made at {{ formatDateTime(plan.data.value.asOf)
        }}<template v-if="plan.isFetching.value">; reading it again...</template>.
      </p>
      <PartitionActionsTable
        v-if="actions.length > 0"
        :actions="actions"
        :table="table.name"
        caption="What the run would do"
      />
      <p v-else class="muted" data-testid="run-plan-empty">The run would find nothing to do.</p>
      <PlanRemovals :actions="actions" testid="run-plan-deletes" tail="This cannot be undone." />
      <p v-if="dropped.length === 0 && detached.length > 0" class="muted">
        Detached partitions keep their rows as tables of their own.
      </p>
    </template>
  </TaskActionDialog>
</template>
