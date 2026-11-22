<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Attach a distributed table as a user partition (contract 0.14.0, `attachTablePartition`, kingres_admin). The table has to be in
// the colocation group of this table and use the same distribution column; no row moves. Only such tables are offered.
import { computed, reactive, ref } from 'vue'
import { safeText } from '@/lib/safe-text'
import TaskActionDialog from '../TaskActionDialog.vue'
import FormField from '../FormField.vue'
import TableChangePanel from './TableChangePanel.vue'
import TableChangeRefusal from './TableChangeRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { useTableChange } from '@/composables/useTableChange'
import { CANCELLED_TEXT, partitionBoundError } from '@/lib/tables'
import type { Table } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table; candidates: Table[] }>()
const emit = defineEmits<{ close: [] }>()

const tableKey = (t: Table): string => JSON.stringify([t.schema, t.name])
/** The tables that can be attached: distributed in the same colocation group by the same column, and not partitioned themselves. */
const offered = computed(() =>
  props.candidates.filter(
    (t) =>
      !(t.schema === props.table.schema && t.name === props.table.name) &&
      t.distribution?.method === 'hash' &&
      t.distribution.colocationId === props.table.distribution?.colocationId &&
      t.distribution.column === props.table.distribution?.column &&
      !t.partitioning,
  ),
)
const form = reactive({ choice: '', bound: '' })
const submitted = ref(false)
const chosen = computed(() => offered.value.find((t) => tableKey(t) === form.choice))
const tableError = computed(() => (chosen.value ? null : 'Choose the table to attach.'))
const boundError = computed(() => partitionBoundError(form.bound))
const change = useTableChange(
  () => props.databaseName,
  () => props.table.schema,
  () => props.table.name,
  computed(() =>
    chosen.value && !boundError.value
      ? {
          kind: 'attachPartition' as const,
          partition: {
            table: { schema: chosen.value.schema, table: chosen.value.name },
            bound: form.bound.trim(),
          },
        }
      : null,
  ),
)
const validate = (): boolean => {
  submitted.value = true
  return !tableError.value && !boundError.value
}
const start = () => {
  const t = chosen.value
  if (!t) return Promise.reject(new Error('No table chosen.'))
  return endpoints.attachTablePartition(props.databaseName, props.table.schema, props.table.name, {
    table: { schema: t.schema, table: t.name },
    bound: form.bound.trim(),
  })
}
</script>

<template>
  <TaskActionDialog
    :title="`Attach a table to ${table.schema}.${table.name}`"
    confirm-label="Attach table"
    :start="start"
    :validate="validate"
    :confirm-disabled="change.holdsStart.value"
    :invalidate="[
      keys.tables(databaseName),
      keys.table(databaseName, table.schema, table.name),
      keys.tablePartitions(databaseName, table.schema, table.name),
      keys.colocation(databaseName),
    ]"
    cancelable
    :cancelled-text="CANCELLED_TEXT"
    wide
    @close="emit('close')"
  >
    <template #error="{ error }">
      <TableChangeRefusal :error="error" />
    </template>
    <template #default="{ fieldErrors }">
      <p>
        The table you choose becomes a partition of this table. No row moves. It has to be
        distributed in the same colocation group by the same column.
      </p>
      <p
        v-if="offered.length === 0"
        class="notice tone-info"
        role="status"
        data-testid="attach-none"
      >
        <span aria-hidden="true">&#9432;</span>
        <span
          >No table can be attached: none is distributed in this colocation group by the column
          <code>{{ table.distribution?.column }}</code>.</span
        >
      </p>
      <template v-else>
        <FormField
          v-slot="slot"
          label="Table to attach"
          :error="(submitted ? tableError : null) ?? fieldErrors['table'] ?? undefined"
          required
        >
          <select v-bind="slot" v-model="form.choice" data-testid="attach-table">
            <option value="">Choose a table</option>
            <option v-for="t in offered" :key="tableKey(t)" :value="tableKey(t)">
              {{ safeText(`${t.schema}.${t.name}`) }}
            </option>
          </select>
        </FormField>
        <FormField
          v-slot="slot"
          label="Bound"
          hint="The text that follows FOR VALUES, or DEFAULT."
          :error="(submitted ? boundError : null) ?? fieldErrors.bound ?? undefined"
          required
        >
          <textarea
            v-bind="slot"
            v-model="form.bound"
            rows="2"
            autocomplete="off"
            spellcheck="false"
            class="mono"
            data-testid="partition-bound"
          />
        </FormField>
        <TableChangePanel
          v-if="chosen && !boundError"
          :preview="change.preview.value"
          :loading="change.loading.value"
          :error="change.error.value"
          @retry="change.retry()"
        />
      </template>
    </template>
  </TaskActionDialog>
</template>
