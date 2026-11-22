<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Create a partition of a user-partitioned distributed table (contract 0.14.0, `createTablePartition`, kingres_admin): one leaf in
// every bucket partition on every server, in one transaction. The bound is the text that follows FOR VALUES (or DEFAULT).
// The name is at most 57 bytes, because the name of each leaf adds a suffix.
import { computed, reactive, ref } from 'vue'
import TaskActionDialog from '../TaskActionDialog.vue'
import FormField from '../FormField.vue'
import TableChangePanel from './TableChangePanel.vue'
import TableChangeRefusal from './TableChangeRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { useTableChange } from '@/composables/useTableChange'
import {
  PARTITION_NAME_MAX_BYTES,
  partitionBoundError,
  partitionNameError,
  utf8Length,
} from '@/lib/tables'
import type { Table } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table }>()
const emit = defineEmits<{ close: [] }>()

const form = reactive({ name: '', bound: '', ifNotExists: false })
const submitted = ref(false)
const nameError = computed(() => partitionNameError(form.name))
const boundError = computed(() => partitionBoundError(form.bound))
const method = computed(() => props.table.partitioning?.method ?? null)
const example = computed(() =>
  method.value === 'list' ? "IN ('eu', 'uk')" : "FROM ('2026-01-01') TO ('2026-02-01')",
)
const change = useTableChange(
  () => props.databaseName,
  () => props.table.schema,
  () => props.table.name,
  computed(() =>
    !nameError.value && !boundError.value
      ? {
          kind: 'createPartition' as const,
          partition: { name: form.name, bound: form.bound.trim() },
        }
      : null,
  ),
)
const validate = (): boolean => {
  submitted.value = true
  return !nameError.value && !boundError.value
}
const start = () =>
  endpoints.createTablePartition(props.databaseName, props.table.schema, props.table.name, {
    name: form.name,
    bound: form.bound.trim(),
    ...(form.ifNotExists ? { ifNotExists: true } : {}),
  })
</script>

<template>
  <TaskActionDialog
    :title="`Create a partition of ${table.schema}.${table.name}`"
    confirm-label="Create partition"
    :start="start"
    :validate="validate"
    :confirm-disabled="change.holdsStart.value"
    :invalidate="[
      keys.tables(databaseName),
      keys.table(databaseName, table.schema, table.name),
      keys.tablePartitions(databaseName, table.schema, table.name),
    ]"
    cancelable
    cancelled-text="The task was cancelled. No partition was created."
    wide
    @close="emit('close')"
  >
    <template #error="{ error }">
      <TableChangeRefusal :error="error" />
    </template>
    <template #default="{ fieldErrors }">
      <p>
        The partition is created on every server in one step, with one leaf for each bucket. No row
        moves.
        <template v-if="table.partitioning"
          >The table is partitioned by {{ table.partitioning.method.toUpperCase() }} on
          <code>{{ table.partitioning.key }}</code>.</template
        >
      </p>
      <FormField
        v-slot="slot"
        label="Name of the partition"
        :hint="`At most ${PARTITION_NAME_MAX_BYTES} bytes (${utf8Length(form.name)} used).`"
        :error="(submitted ? nameError : null) ?? fieldErrors.name ?? undefined"
        required
      >
        <input
          v-bind="slot"
          v-model="form.name"
          autocomplete="off"
          data-autofocus
          data-testid="partition-name"
        />
      </FormField>
      <FormField
        v-slot="slot"
        label="Bound"
        :hint="`The text that follows FOR VALUES, for example ${example}, or DEFAULT.`"
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
      <label class="check">
        <input v-model="form.ifNotExists" type="checkbox" />
        Do nothing when a partition of this name exists
      </label>
      <TableChangePanel
        v-if="!nameError && !boundError"
        :preview="change.preview.value"
        :loading="change.loading.value"
        :error="change.error.value"
        @retry="change.retry()"
      />
    </template>
  </TaskActionDialog>
</template>
