<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Remove the time-partitioning policy of a table (contract 0.18.0, `deletePartitionPolicy`, kingres_admin; 204, not a task).
// The partitions stay as they are; only the scheduler stops looking after the table. 409 `operation_in_progress` while a run goes on.
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import ConfirmDialog from '../ConfirmDialog.vue'
import ProblemNotice from '../ProblemNotice.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import type { Table } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table }>()
const emit = defineEmits<{ close: [] }>()
const client = useQueryClient()
const remove = useMutation({
  mutationFn: () =>
    endpoints.deletePartitionPolicy(props.databaseName, props.table.schema, props.table.name),
  onSuccess: async () => {
    await Promise.all([
      client.invalidateQueries({
        queryKey: keys.partitionPolicy(props.databaseName, props.table.schema, props.table.name),
      }),
      client.invalidateQueries({
        queryKey: keys.table(props.databaseName, props.table.schema, props.table.name),
      }),
      client.invalidateQueries({ queryKey: keys.tables(props.databaseName) }),
    ])
    emit('close')
  },
})
</script>

<template>
  <ConfirmDialog
    :title="`Remove the policy of ${table.schema}.${table.name}`"
    confirm-label="Remove policy"
    danger
    :busy="remove.isPending.value"
    @cancel="emit('close')"
    @confirm="remove.mutate()"
  >
    <p data-testid="remove-policy-text">
      The scheduler stops creating, converting and removing partitions of this table. The partitions
      stay as they are, and so does their data. The history of the runs is kept for a while.
    </p>
    <ProblemNotice v-if="remove.error.value" :error="remove.error.value" />
  </ConfirmDialog>
</template>
