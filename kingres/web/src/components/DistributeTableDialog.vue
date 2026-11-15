<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Distribute a plain table. The distribution column is chosen from the columns the server says can
// be one (`canBeDistributionColumn`); the others are listed with the reason from the contract.
import { computed, reactive, ref, watch } from 'vue'
import { safeText } from '@/lib/safe-text'
import SafeName from './SafeName.vue'
import TaskActionDialog from './TaskActionDialog.vue'
import FormField from './FormField.vue'
import QueryBoundary from './QueryBoundary.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useTableDetail } from '@/api/queries'
import type { DistributionBlockedReason, Table } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table; candidates: Table[] }>()
const emit = defineEmits<{ close: [] }>()

/** An unambiguous key for a table: a schema or a table name may itself contain a dot. */
const tableKey = (t: Table): string => JSON.stringify([t.schema, t.name])

const BLOCKED_TEXT: Record<DistributionBlockedReason, string> = {
  shardingDisabled: 'Sharding is turned off for this database.',
  tableNotPlain: 'The table is already distributed or a reference table.',
  unsupportedType: 'The type of this column cannot be hashed into buckets.',
  notInUniqueKey:
    'The column is not part of every unique key; distributing by it could not keep those keys unique.',
}

const detail = useTableDetail(props.databaseName, props.table.schema, props.table.name)
const usable = computed(
  () => detail.data.value?.columns.filter((c) => c.canBeDistributionColumn) ?? [],
)
const blocked = computed(
  () => detail.data.value?.columns.filter((c) => !c.canBeDistributionColumn) ?? [],
)

const form = reactive({ method: 'hash' as 'hash' | 'reference', column: '', colocateWith: '' })
const submitted = ref(false)
watch(
  usable,
  (cols) => {
    if (!cols.some((c) => c.name === form.column)) form.column = cols[0]?.name ?? ''
  },
  { immediate: true },
)
const columnError = computed(() =>
  form.method === 'hash' && !form.column
    ? usable.value.length === 0
      ? 'No column of this table can be the distribution column.'
      : 'Choose the distribution column.'
    : undefined,
)
const validate = (): boolean => {
  submitted.value = true
  return !columnError.value
}

const start = () => {
  const target = props.candidates.find((t) => tableKey(t) === form.colocateWith)
  return endpoints.distributeTable(props.databaseName, props.table.schema, props.table.name, {
    method: form.method,
    ...(form.method === 'hash' ? { distributionColumn: form.column } : {}),
    colocateWith:
      form.method === 'hash' && target ? { schema: target.schema, table: target.name } : null,
  })
}
</script>

<template>
  <TaskActionDialog
    :title="`Distribute ${table.schema}.${table.name}`"
    confirm-label="Distribute table"
    :start="start"
    :validate="validate"
    :invalidate="[keys.tables(databaseName), keys.databases, keys.colocation(databaseName)]"
    @close="emit('close')"
  >
    <template #default="{ fieldErrors }">
      <p>
        The rows are copied to the shard groups and the table is switched over. The table stays
        readable; writes wait briefly during the switch. Distributing cannot be undone.
      </p>
      <QueryBoundary
        :loading="detail.isPending.value"
        :error="detail.error.value"
        @retry="detail.refetch()"
      >
        <fieldset>
          <legend>Method</legend>
          <label class="check"
            ><input v-model="form.method" type="radio" value="hash" /> Hash-distributed by a
            column</label
          >
          <label class="check"
            ><input v-model="form.method" type="radio" value="reference" /> Reference table (a full
            copy on every group)</label
          >
        </fieldset>
        <template v-if="form.method === 'hash'">
          <FormField
            v-slot="slot"
            label="Distribution column"
            hint="Each row goes to the bucket of the hash of this column."
            :error="(submitted ? columnError : undefined) ?? fieldErrors.distributionColumn"
            required
          >
            <select v-bind="slot" v-model="form.column" data-autofocus>
              <option v-for="c in usable" :key="c.name" :value="c.name">
                {{ safeText(c.name) }} ({{ safeText(c.type, 60) }})
              </option>
            </select>
          </FormField>
          <div v-if="blocked.length" data-testid="blocked-columns">
            <p class="muted">Columns that cannot be used:</p>
            <ul class="blocked">
              <li v-for="c in blocked" :key="c.name">
                <code><SafeName :value="c.name" :max="80" /></code> ({{ safeText(c.type, 60) }}):
                {{
                  c.distributionBlockedReason
                    ? BLOCKED_TEXT[c.distributionBlockedReason]
                    : 'Not allowed.'
                }}
                <template v-if="c.blockingKeys.length">
                  Keys without it:
                  <code><SafeName :value="c.blockingKeys.join(', ')" kind="text" :max="200" /></code
                  >.
                </template>
              </li>
            </ul>
          </div>
          <FormField
            v-slot="slot"
            label="Colocate with (optional)"
            hint="Tables in one colocation group can be joined without moving data."
          >
            <select v-bind="slot" v-model="form.colocateWith">
              <option value="">A new colocation group</option>
              <option v-for="t in candidates" :key="tableKey(t)" :value="tableKey(t)">
                {{ safeText(`${t.schema}.${t.name}`) }}
              </option>
            </select>
          </FormField>
        </template>
      </QueryBoundary>
    </template>
  </TaskActionDialog>
</template>

<style scoped>
.blocked {
  margin: 0;
  padding-left: var(--s-4);
  font-size: var(--fs-sm);
}
</style>
