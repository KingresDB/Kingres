<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Detach a user partition as a table of its own, or drop it (contract 0.14.0, `detachTablePartition` and `dropTablePartition`,
// kingres_admin). A detach moves no row and keeps the data; a drop removes the partition with its leaves on every server and the
// rows of the partition are gone, so it asks for the name of the partition to be typed. The preview says what stops the change
// (a table with foreign keys cannot detach a partition).
import { computed } from 'vue'
import SafeName from '../SafeName.vue'
import TaskActionDialog from '../TaskActionDialog.vue'
import TableChangePanel from './TableChangePanel.vue'
import TableChangeRefusal from './TableChangeRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { useTableChange } from '@/composables/useTableChange'
import { formatBytes } from '@/lib/format'
import type { Table, TablePartition } from '@/api/types'

const props = defineProps<{
  databaseName: string
  table: Table
  partition: TablePartition
  action: 'detach' | 'drop'
}>()
const emit = defineEmits<{ close: [] }>()

const drop = computed(() => props.action === 'drop')
const change = useTableChange(
  () => props.databaseName,
  () => props.table.schema,
  () => props.table.name,
  computed(() => ({
    kind: drop.value ? ('dropPartition' as const) : ('detachPartition' as const),
    partition: { name: props.partition.name },
  })),
)
const start = () =>
  drop.value
    ? endpoints.dropTablePartition(
        props.databaseName,
        props.table.schema,
        props.table.name,
        props.partition.name,
      )
    : endpoints.detachTablePartition(
        props.databaseName,
        props.table.schema,
        props.table.name,
        props.partition.name,
      )
</script>

<template>
  <TaskActionDialog
    :title="`${drop ? 'Drop' : 'Detach'} partition ${partition.name}`"
    :confirm-label="drop ? 'Drop partition' : 'Detach partition'"
    :danger="drop"
    :type-to-confirm="drop ? partition.name : undefined"
    :start="start"
    :confirm-disabled="change.holdsStart.value"
    :invalidate="[
      keys.tables(databaseName),
      keys.table(databaseName, table.schema, table.name),
      keys.tablePartitions(databaseName, table.schema, table.name),
      keys.colocation(databaseName),
    ]"
    cancelable
    cancelled-text="The task was cancelled. The partition is as it was."
    @close="emit('close')"
  >
    <template #error="{ error }">
      <TableChangeRefusal :error="error" />
    </template>
    <template #default>
      <p v-if="drop" class="notice tone-danger" role="alert" data-testid="drop-partition-warning">
        <span aria-hidden="true">&#10006;</span>
        <span>
          The partition <strong><SafeName :value="partition.name" :max="80" /></strong>
          <template v-if="partition.sizeBytes !== null"
            >({{ formatBytes(partition.sizeBytes) }} on this server)</template
          >
          is removed with its leaves on every server, and <strong>its rows are deleted</strong>.
          This cannot be undone. To keep the rows, detach the partition instead.
        </span>
      </p>
      <p v-else data-testid="detach-partition-text">
        The partition <strong><SafeName :value="partition.name" :max="80" /></strong> becomes a
        distributed table of its own in the same colocation group and on the same column. No row
        moves and none is deleted.
      </p>
      <TableChangePanel
        :preview="change.preview.value"
        :loading="change.loading.value"
        :error="change.error.value"
        @retry="change.retry()"
      />
    </template>
  </TaskActionDialog>
</template>
