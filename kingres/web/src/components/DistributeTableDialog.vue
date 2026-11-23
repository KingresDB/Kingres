<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Distribute a plain table (contract 0.14.0: a table with rows can be distributed). The distribution column is chosen from the
// columns the server says can be one (`canBeDistributionColumn`); the others are listed with the reason from the contract. The
// rows to copy, the mode of the copy, the bucket count of a new colocation group and what happens to the views that use the
// table are asked here, and the preview says what the sharding extension finds before anything starts: a blocker keeps the
// start button off, and a refusal (422 `table_not_eligible`) lists the same lines.
import { computed, reactive, ref, useId, watch } from 'vue'
import { safeText } from '@/lib/safe-text'
import SafeName from './SafeName.vue'
import TaskActionDialog from './TaskActionDialog.vue'
import FormField from './FormField.vue'
import QueryBoundary from './QueryBoundary.vue'
import TableChangeOptions from './tables/TableChangeOptions.vue'
import TableChangePanel from './tables/TableChangePanel.vue'
import TableChangeRefusal from './tables/TableChangeRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useTableDetail } from '@/api/queries'
import { useTableChange } from '@/composables/useTableChange'
import { bucketCountError, CANCELLED_TEXT, ONLINE_ROWS_THRESHOLD } from '@/lib/tables'
import { formatNumber } from '@/lib/format'
import type {
  DependentViews,
  DistributeTableRequest,
  DistributionBlockedReason,
  Table,
  TableChangeMode,
} from '@/api/types'

/** One name for the two radios of the method, so that the arrow keys move between them. */
const methodGroup = `${useId()}-method`
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

const form = reactive({
  method: 'hash' as 'hash' | 'reference',
  column: '',
  colocateWith: '',
  bucketCount: '',
  mode: 'auto' as TableChangeMode,
  views: 'refuse' as DependentViews,
})
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
const target = computed(() => props.candidates.find((t) => tableKey(t) === form.colocateWith))
/** The bucket count belongs to a new colocation group: it is asked only when the table does not join one. */
const asksBuckets = computed(() => form.method === 'hash' && !target.value)
const bucketError = computed(() => (asksBuckets.value ? bucketCountError(form.bucketCount) : null))
const rows = computed(() => props.table.estimatedRows)

/** The request; the defaults of the contract (`auto`, `refuse`) are left out. */
function request(): DistributeTableRequest {
  return {
    method: form.method,
    ...(form.method === 'hash' ? { distributionColumn: form.column } : {}),
    colocateWith:
      form.method === 'hash' && target.value
        ? { schema: target.value.schema, table: target.value.name }
        : null,
    ...(asksBuckets.value && form.bucketCount.trim() !== '' && !bucketError.value
      ? { bucketCount: Number(form.bucketCount) }
      : {}),
    ...(form.mode !== 'auto' ? { mode: form.mode } : {}),
    ...(form.views !== 'refuse' ? { dependentViews: form.views } : {}),
  }
}
const previewRequest = computed(() =>
  (form.method === 'reference' || form.column) && !bucketError.value
    ? { kind: 'distribute' as const, distribute: request() }
    : null,
)
const change = useTableChange(
  () => props.databaseName,
  () => props.table.schema,
  () => props.table.name,
  previewRequest,
)
const onlineAvailable = computed(() => change.preview.value?.estimate.onlineAvailable ?? true)
// An online copy that the preview says is impossible cannot stay chosen.
watch(onlineAvailable, (available) => {
  if (!available && form.mode === 'online') form.mode = 'auto'
})

const validate = (): boolean => {
  submitted.value = true
  return !columnError.value && !bucketError.value
}
const start = () =>
  endpoints.distributeTable(props.databaseName, props.table.schema, props.table.name, request())
</script>

<template>
  <TaskActionDialog
    :title="`Distribute ${table.schema}.${table.name}`"
    confirm-label="Distribute table"
    :start="start"
    :validate="validate"
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
        The rows are copied to the shard groups and the table is switched over. In the online mode
        writers go on while the rows are copied; in the blocking mode they wait for the whole copy.
        Reads go on in both. The table can be made plain again with "Undistribute".
      </p>
      <p data-testid="distribute-rows">
        <template v-if="rows === null">The number of rows of this table is not known.</template>
        <template v-else-if="rows === 0">This table is empty: nothing is copied.</template>
        <template v-else>
          This table has about {{ formatNumber(rows) }} rows, which are copied.
          <span v-if="rows > ONLINE_ROWS_THRESHOLD" class="muted"
            >That is a lot: the automatic mode copies online.</span
          >
        </template>
      </p>
      <QueryBoundary
        :loading="detail.isPending.value"
        :error="detail.error.value"
        @retry="detail.refetch()"
      >
        <fieldset>
          <legend>Method</legend>
          <label class="check"
            ><input v-model="form.method" type="radio" :name="methodGroup" value="hash" />
            Hash-distributed by a column</label
          >
          <label class="check"
            ><input v-model="form.method" type="radio" :name="methodGroup" value="reference" />
            Reference table (a full copy on every group)</label
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
          <FormField
            v-if="asksBuckets"
            v-slot="slot"
            label="Buckets of the new colocation group (optional)"
            hint="From 1 to 4096. Empty: the number set for the database."
            :error="bucketError ?? fieldErrors.bucketCount ?? undefined"
          >
            <input
              v-bind="slot"
              v-model="form.bucketCount"
              inputmode="numeric"
              autocomplete="off"
              data-testid="distribute-buckets"
            />
          </FormField>
        </template>
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
