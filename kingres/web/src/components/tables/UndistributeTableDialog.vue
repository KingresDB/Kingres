<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Turn a distributed or reference table back into a plain table (contract 0.14.0, `undistributeTable`, kingres_admin). The table
// goes to the primary of one shard group (the map group by default); the tables that foreign keys link to it are refused unless
// they are undistributed with it. The preview names the tables the change touches and what stops it; the free space of the
// target server is checked by the task, and a failure there leaves the table as it was.
import { computed, reactive, watch } from 'vue'
import TaskActionDialog from '../TaskActionDialog.vue'
import FormField from '../FormField.vue'
import TableChangeOptions from './TableChangeOptions.vue'
import TableChangePanel from './TableChangePanel.vue'
import TableChangeRefusal from './TableChangeRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useTopology } from '@/api/queries'
import { useTableChange } from '@/composables/useTableChange'
import { formatNumber } from '@/lib/format'
import { CANCELLED_TEXT } from '@/lib/tables'
import type { DependentViews, Table, TableChangeMode } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table }>()
const emit = defineEmits<{ close: [] }>()

const topology = useTopology()
const groups = computed(() =>
  (topology.data.value?.groups ?? [])
    .map((g) => ({
      id: g.id,
      label: g.isMapGroup ? `Group ${g.id} (the map group)` : `Group ${g.id}`,
    }))
    .sort((a, b) => a.id - b.id),
)
const form = reactive({
  cascade: false,
  targetGroupId: 0,
  mode: 'auto' as TableChangeMode,
  views: 'refuse' as DependentViews,
})

/** The options, the defaults of the contract left out. */
function query() {
  return {
    ...(form.cascade ? { cascadeViaForeignKeys: true } : {}),
    ...(form.targetGroupId !== 0 ? { targetGroupId: form.targetGroupId } : {}),
    ...(form.mode !== 'auto' ? { mode: form.mode } : {}),
    ...(form.views !== 'refuse' ? { dependentViews: form.views } : {}),
  }
}
const change = useTableChange(
  () => props.databaseName,
  () => props.table.schema,
  () => props.table.name,
  computed(() => ({ kind: 'undistribute' as const, undistribute: query() })),
)
const onlineAvailable = computed(() => change.preview.value?.estimate.onlineAvailable ?? true)
watch(onlineAvailable, (available) => {
  if (!available && form.mode === 'online') form.mode = 'auto'
})
/** The rows of the table go to one server; with rows (or an unknown number) the name has to be typed. */
const confirmName = computed(() => (props.table.estimatedRows === 0 ? undefined : props.table.name))
const start = () =>
  endpoints.undistributeTable(props.databaseName, props.table.schema, props.table.name, query())
</script>

<template>
  <TaskActionDialog
    :title="`Undistribute ${table.schema}.${table.name}`"
    confirm-label="Undistribute table"
    danger
    :type-to-confirm="confirmName"
    :start="start"
    :confirm-disabled="change.holdsStart.value"
    :invalidate="[
      keys.tables(databaseName),
      keys.table(databaseName, table.schema, table.name),
      keys.databases,
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
        The table becomes an ordinary table on one server: its rows are copied there from every
        shard group, and the shards are removed. The server must have room for the whole table, and
        that is checked first. Reads go on; for writers see the choice below.
      </p>
      <p class="notice tone-danger" role="alert" data-testid="undistribute-warning">
        <span aria-hidden="true">&#10006;</span>
        <span
          >The table will live on one server. Its data must fit there. The copy can take hours for a
          large table.</span
        >
      </p>
      <p v-if="table.estimatedRows !== null" data-testid="undistribute-rows">
        This table has about {{ formatNumber(table.estimatedRows) }} rows.
      </p>
      <FormField
        v-slot="slot"
        label="The table goes to"
        hint="The primary of this shard group holds the table afterwards."
        :error="fieldErrors.targetGroupId"
      >
        <select v-bind="slot" v-model.number="form.targetGroupId" data-testid="undistribute-group">
          <option v-if="groups.length === 0" :value="0">Group 0 (the map group)</option>
          <option v-for="g in groups" :key="g.id" :value="g.id">{{ g.label }}</option>
        </select>
      </FormField>
      <label class="check">
        <input v-model="form.cascade" type="checkbox" data-testid="undistribute-cascade" />
        Undistribute the tables that foreign keys link to this one as well
      </label>
      <p class="muted">
        Without this a table that is linked to others is refused, and the preview names the linked
        tables.
      </p>
      <TableChangeOptions
        v-model:mode="form.mode"
        v-model:views="form.views"
        :online-available="onlineAvailable"
        :online-reason="change.preview.value?.estimate.onlineUnavailableReason ?? null"
      />
      <TableChangePanel
        :preview="change.preview.value"
        :loading="change.loading.value"
        :error="change.error.value"
        @retry="change.retry()"
      />
    </template>
  </TaskActionDialog>
</template>
