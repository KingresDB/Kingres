<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Change the storage format of a table or of one partition between rows (heap) and columns (contract 0.18.0, `setTableStorage` and
// `setPartitionStorage`, kingres_admin; the task `table.setStorage` with the steps check, convert and analyze). A plain table is
// rewritten under an ACCESS EXCLUSIVE lock, so readers and writers wait until it ends; a partition is built aside and swapped in
// one short step. What the console already knows stops the change before it starts (a ledger table, the parent of partitions, a
// distributed table). What only the server knows (an index, a trigger, a publication) comes back as a 409 `conversion_refused`
// with its reasons in a list. The task can be cancelled until its switch; the table is then as it was.
import { computed, reactive, ref } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import TableBlockerList from './TableBlockerList.vue'
import OwnerNotice from './OwnerNotice.vue'
import TaskActionDialog from '../TaskActionDialog.vue'
import FormField from '../FormField.vue'
import ProblemNotice from '../ProblemNotice.vue'
import SafeName from '../SafeName.vue'
import { endpoints } from '@/api/endpoints'
import { ApiError, isUnsupported } from '@/api/problem'
import { keys } from '@/api/queries'
import { formatBytes } from '@/lib/format'
import {
  CHUNK_ROWS_MIN,
  COMPRESSION_LABEL,
  COMPRESSION_LEVEL_MAX,
  COMPRESSION_LEVEL_MIN,
  STRIPE_ROWS_MIN,
  byPointer,
  refusalReasons,
  type ColumnarCompression,
  type PartitionStorageRequest,
} from '@/lib/analytics'
import type { Table, TablePartition } from '@/api/types'

const props = defineProps<{
  databaseName: string
  table: Table
  /** Set to convert one partition of the table instead of the table. */
  partition?: TablePartition | null
}>()
const emit = defineEmits<{ close: [] }>()

const subject = computed(() => props.partition?.name ?? props.table.name)
const currentMethod = computed(() =>
  props.partition
    ? (props.partition.accessMethod ?? null)
    : (props.table.storage?.accessMethod ?? null),
)
const sizeBytes = computed(() =>
  props.partition ? props.partition.sizeBytes : props.table.sizeBytes,
)

const form = reactive({
  target: (currentMethod.value === 'columnar' ? 'heap' : 'columnar') as 'heap' | 'columnar',
  compression: 'zstd' as ColumnarCompression,
  level: 3 as number | string,
  stripe: '' as number | string,
  chunk: '' as number | string,
  sortBy: '',
})
const submitted = ref(false)

/** What the console knows already and the server would refuse. */
const blockers = computed<string[]>(() => {
  const out: string[] = []
  if (props.table.classes.includes('ledger'))
    out.push('This is a ledger table: its rows are chained, so the format cannot change.')
  if (!props.partition && props.table.partitioning)
    out.push(
      'This table is partitioned. Convert one partition at a time, or set a time-partitioning policy that converts old partitions.',
    )
  if (props.table.classes.includes('distributed') || props.table.classes.includes('reference'))
    out.push('Converting a distributed table is not available in this version.')
  if (props.partition?.isDefault) out.push('A default partition cannot be converted.')
  return out
})
// A table that is columnar already can be asked for columnar again: it sets the options of the new stripes.
const same = computed(
  () => currentMethod.value === form.target && (!!props.partition || form.target === 'heap'),
)
const optionsOnly = computed(
  () => !props.partition && currentMethod.value === 'columnar' && form.target === 'columnar',
)
const columnar = computed(() => form.target === 'columnar')
const levelError = computed(() => {
  if (!columnar.value || form.compression !== 'zstd') return undefined
  const n = typeof form.level === 'number' ? form.level : Number.NaN
  return Number.isInteger(n) && n >= COMPRESSION_LEVEL_MIN && n <= COMPRESSION_LEVEL_MAX
    ? undefined
    : `Use a whole number from ${COMPRESSION_LEVEL_MIN} to ${COMPRESSION_LEVEL_MAX}.`
})
const optional = (v: number | string, min: number, label: string): string | undefined => {
  if (!columnar.value || v === '') return undefined
  return typeof v === 'number' && Number.isInteger(v) && v >= min
    ? undefined
    : `${label}: use a whole number of ${min} or more, or leave it empty.`
}
const stripeError = computed(() => optional(form.stripe, STRIPE_ROWS_MIN, 'Rows per stripe'))
const chunkError = computed(() => optional(form.chunk, CHUNK_ROWS_MIN, 'Rows per chunk group'))
const valid = computed(() => !levelError.value && !stripeError.value && !chunkError.value)
const validate = (): boolean => {
  submitted.value = true
  return valid.value
}

const body = computed<PartitionStorageRequest>(() => {
  if (!columnar.value) return { accessMethod: 'heap' }
  const sort = form.sortBy
    .split(',')
    .map((x) => x.trim())
    .filter((x) => x !== '')
  return {
    accessMethod: 'columnar',
    compression: form.compression,
    ...(form.compression === 'zstd' ? { compressionLevel: Number(form.level) } : {}),
    ...(form.stripe !== '' ? { stripeRowCount: Number(form.stripe) } : {}),
    ...(form.chunk !== '' ? { chunkRowCount: Number(form.chunk) } : {}),
    ...(props.partition && sort.length > 0 ? { sortBy: sort } : {}),
  }
})
const start = () =>
  props.partition
    ? endpoints.setPartitionStorage(
        props.databaseName,
        props.table.schema,
        props.table.name,
        props.partition.name,
        body.value,
      )
    : endpoints.setTableStorage(
        props.databaseName,
        props.table.schema,
        props.table.name,
        body.value,
      )

// The server's own check before anything starts (`dryRun`): the blockers and the notes of this request. A server that cannot
// preview (422 unsupported_operation) does not stop the change: the 409 comes back after the start, with the same blockers.
const askable = computed(() => blockers.value.length === 0 && !same.value && valid.value)
const preview = useQuery({
  queryKey: computed(() => [
    'storagePreview',
    props.databaseName,
    props.table.schema,
    props.table.name,
    props.partition?.name ?? null,
    JSON.stringify(body.value),
  ]),
  queryFn: () =>
    props.partition
      ? endpoints.previewPartitionStorage(
          props.databaseName,
          props.table.schema,
          props.table.name,
          props.partition.name,
          body.value,
        )
      : endpoints.previewTableStorage(
          props.databaseName,
          props.table.schema,
          props.table.name,
          body.value,
        ),
  enabled: askable,
  retry: false,
  staleTime: 0,
  gcTime: 0,
})
const ownerUntrusted = computed(() => askable.value && preview.data.value?.ownerTrusted === false)
const previewUnavailable = computed(() => isUnsupported(preview.error.value))
const serverBlockers = computed(() => (askable.value ? (preview.data.value?.blockers ?? []) : []))
const serverNotes = computed(() => (askable.value ? (preview.data.value?.notes ?? []) : []))
const previewHolds = computed(
  () =>
    askable.value &&
    (preview.isFetching.value ||
      serverBlockers.value.length > 0 ||
      (preview.error.value !== null && !previewUnavailable.value)),
)
// The blockers of a refusal after the start: typed in `Problem.blockers`; `detail` repeats them as text.
const refusalBlockers = (error: unknown) =>
  error instanceof ApiError && error.code === 'conversion_refused'
    ? (error.problem.blockers ?? [])
    : []
const refusal = (error: unknown): string[] =>
  error instanceof ApiError &&
  error.code === 'conversion_refused' &&
  (error.problem.blockers ?? []).length === 0
    ? refusalReasons(error.problem.detail)
    : []
</script>

<template>
  <TaskActionDialog
    :title="`Convert storage of ${partition ? 'partition' : 'table'} ${subject}`"
    confirm-label="Convert"
    :start="start"
    :validate="validate"
    :confirm-disabled="blockers.length > 0 || same || previewHolds"
    :invalidate="[
      keys.tables(databaseName),
      keys.table(databaseName, table.schema, table.name),
      keys.tablePartitions(databaseName, table.schema, table.name),
      keys.tasks,
    ]"
    cancelable
    :cancelled-text="`The task was cancelled. The ${partition ? 'partition' : 'table'} is as it was.`"
    @close="emit('close')"
  >
    <template #error="{ error }">
      <div class="stack" data-testid="storage-refusal">
        <ProblemNotice :error="error" />
        <TableBlockerList
          :lines="refusalBlockers(error)"
          tone="blocker"
          heading="What stops this change"
        />
        <ul v-if="refusal(error).length > 0" class="reasons" data-testid="storage-reasons">
          <li v-for="(r, i) in refusal(error)" :key="i">
            <SafeName :value="r" kind="text" :max="300" />
          </li>
        </ul>
      </div>
    </template>
    <template #default="{ fieldErrors: rawErrors }">
      <p>
        <template v-if="partition"
          >Partition <strong><SafeName :value="partition.name" :max="80" /></strong> of table
          <strong
            ><SafeName :value="`${table.schema}.${table.name}`" :max="120" /></strong></template
        ><template v-else
          >Table <strong><SafeName :value="`${table.schema}.${table.name}`" :max="120" /></strong
        ></template>
        <template v-if="sizeBytes !== null"> ({{ formatBytes(sizeBytes) }})</template>
        is stored as
        <strong>{{
          currentMethod === 'columnar'
            ? 'columns'
            : currentMethod === 'heap'
              ? 'rows'
              : 'an unknown format'
        }}</strong>.
      </p>
      <p v-if="optionsOnly" class="notice tone-info" data-testid="storage-options-only">
        <span aria-hidden="true">&#9432;</span>
        <span
          >The table is stored as columns already. This sets the options for the stripes that are
          written from now on; the stripes that exist stay as they are.</span
        >
      </p>
      <p
        v-for="b in blockers"
        :key="b"
        class="notice tone-danger"
        role="alert"
        data-testid="storage-blocker"
      >
        <span aria-hidden="true">&#10006;</span><span>{{ b }}</span>
      </p>
      <fieldset :disabled="blockers.length > 0">
        <legend>New format</legend>
        <label class="check"
          ><input
            v-model="form.target"
            type="radio"
            value="columnar"
            data-testid="storage-columnar"
          />
          Columnar: stored by column, compressed. Fast to scan, no indexes, no UPDATE or
          DELETE.</label
        >
        <label class="check"
          ><input v-model="form.target" type="radio" value="heap" data-testid="storage-heap" /> Row
          (heap): the usual format.</label
        >
      </fieldset>
      <div v-if="askable" class="stack" data-testid="storage-preview">
        <p
          v-if="preview.isFetching.value"
          class="muted"
          role="status"
          data-testid="storage-preview-loading"
        >
          Checking the change...
        </p>
        <ProblemNotice
          v-else-if="preview.error.value && !previewUnavailable"
          :error="preview.error.value"
          @retry="preview.refetch()"
        />
        <template v-else>
          <OwnerNotice v-if="ownerUntrusted" :owner="preview.data.value?.tableOwner" />
          <TableBlockerList
            :lines="serverBlockers"
            tone="blocker"
            heading="What stops this change"
          />
          <ul v-if="serverNotes.length > 0" class="reasons" data-testid="storage-notes">
            <li v-for="(n, i) in serverNotes" :key="i">
              <SafeName :value="n" kind="text" :max="300" />
            </li>
          </ul>
        </template>
      </div>
      <p v-if="same && blockers.length === 0" class="muted" data-testid="storage-same">
        It is stored in this format already. Choose the other one.
      </p>
      <template v-if="columnar">
        <div class="form-row">
          <FormField label="Compression">
            <template #default="{ id, ...aria }">
              <select :id="id" v-model="form.compression" data-testid="storage-codec" v-bind="aria">
                <option v-for="(label, key) in COMPRESSION_LABEL" :key="key" :value="key">
                  {{ label }}
                </option>
              </select>
            </template>
          </FormField>
          <FormField
            v-if="form.compression === 'zstd'"
            label="Level"
            :error="(submitted ? levelError : undefined) ?? byPointer(rawErrors).compressionLevel"
          >
            <template #default="{ id, ...aria }">
              <input
                :id="id"
                v-model.number="form.level"
                class="input num"
                type="number"
                :min="COMPRESSION_LEVEL_MIN"
                :max="COMPRESSION_LEVEL_MAX"
                style="width: 6em"
                data-testid="storage-level"
                v-bind="aria"
              />
            </template>
          </FormField>
        </div>
        <FormField
          v-if="partition"
          label="Sort rows by (columns, separated by commas)"
          hint="Orders the rows of the partition, which helps the skipping of stripes."
          :error="byPointer(rawErrors).sortBy"
        >
          <template #default="{ id, ...aria }">
            <input
              :id="id"
              v-model="form.sortBy"
              class="input mono"
              autocomplete="off"
              spellcheck="false"
              data-testid="storage-sort"
              v-bind="aria"
            />
          </template>
        </FormField>
        <details>
          <summary>Advanced</summary>
          <div class="form-row">
            <FormField
              label="Rows per stripe"
              :error="(submitted ? stripeError : undefined) ?? byPointer(rawErrors).stripeRowCount"
              :hint="`${STRIPE_ROWS_MIN} or more. Empty keeps the default.`"
            >
              <template #default="{ id, ...aria }">
                <input
                  :id="id"
                  v-model.number="form.stripe"
                  class="input num"
                  type="number"
                  :min="STRIPE_ROWS_MIN"
                  style="width: 9em"
                  data-testid="storage-stripe"
                  v-bind="aria"
                />
              </template>
            </FormField>
            <FormField
              label="Rows per chunk group"
              :error="(submitted ? chunkError : undefined) ?? byPointer(rawErrors).chunkRowCount"
              :hint="`${CHUNK_ROWS_MIN} or more. Empty keeps the default.`"
            >
              <template #default="{ id, ...aria }">
                <input
                  :id="id"
                  v-model.number="form.chunk"
                  class="input num"
                  type="number"
                  :min="CHUNK_ROWS_MIN"
                  style="width: 9em"
                  data-testid="storage-chunk"
                  v-bind="aria"
                />
              </template>
            </FormField>
          </div>
        </details>
      </template>
      <p v-if="partition" class="notice tone-info" data-testid="storage-partition-note">
        <span aria-hidden="true">&#9432;</span>
        <span>
          Writers of this partition wait while it is copied; readers are not blocked. The partition
          is replaced in one short step.
        </span>
      </p>
      <p v-else class="notice tone-warn" data-testid="storage-table-note">
        <span aria-hidden="true">&#9650;</span>
        <span>
          Converting rewrites the table and blocks readers and writers until it finishes. Use time
          partitioning for large tables.
        </span>
      </p>
      <p v-if="!partition && columnar" class="muted" data-testid="storage-late-note">
        An index or a trigger on the table is found when the task runs: the task then fails at the
        step that converts, and the table stays as it was.
      </p>
    </template>
  </TaskActionDialog>
</template>

<style scoped>
.reasons {
  margin: 0;
  padding-left: 1.2em;
}
fieldset {
  border: 0;
  padding: 0;
  margin: 0;
}
</style>
