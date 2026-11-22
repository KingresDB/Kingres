<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Change the distribution of a distributed table (contract 0.14.0, `alterTableDistribution`, kingres_admin): the distribution
// column, the bucket count, the colocation group. At least one has to change. A change that moves no row (the catalog only) is done
// in one step. The tables of the colocation group can change together, which is blocking only. A user-partitioned table cannot be
// altered in this version, and the preview says so.
import { computed, reactive, ref, watch } from 'vue'
import { safeText } from '@/lib/safe-text'
import TaskActionDialog from '../TaskActionDialog.vue'
import FormField from '../FormField.vue'
import QueryBoundary from '../QueryBoundary.vue'
import SafeName from '../SafeName.vue'
import TableChangeOptions from './TableChangeOptions.vue'
import TableChangePanel from './TableChangePanel.vue'
import TableChangeRefusal from './TableChangeRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useTableDetail } from '@/api/queries'
import { useTableChange } from '@/composables/useTableChange'
import { bucketCountError } from '@/lib/tables'
import type {
  AlterTableDistributionRequest,
  DependentViews,
  Table,
  TableChangeMode,
} from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table; candidates: Table[] }>()
const emit = defineEmits<{ close: [] }>()

const tableKey = (t: Table): string => JSON.stringify([t.schema, t.name])
const current = computed(() => props.table.distribution)
const detail = useTableDetail(props.databaseName, props.table.schema, props.table.name)
const columns = computed(() => detail.data.value?.columns ?? [])

const form = reactive({
  column: '',
  bucketCount: '',
  colocate: 'keep' as string,
  cascade: false,
  mode: 'auto' as TableChangeMode,
  views: 'refuse' as DependentViews,
})
const submitted = ref(false)
watch(
  () => form.cascade,
  (on) => {
    // A change of the tables of a colocation group together is blocking only.
    if (on) form.mode = 'blocking'
  },
)

const bucketError = computed(() => bucketCountError(form.bucketCount))
const bucketNumber = computed(() =>
  form.bucketCount.trim() !== '' && !bucketError.value ? Number(form.bucketCount) : null,
)
/** What the dialog asks to change; the parts that stay as they are are left out. */
function request(): AlterTableDistributionRequest {
  const colocate =
    form.colocate === 'keep'
      ? undefined
      : form.colocate === 'default' || form.colocate === 'none'
        ? form.colocate
        : (() => {
            const t = props.candidates.find((x) => tableKey(x) === form.colocate)
            return t ? { schema: t.schema, table: t.name } : undefined
          })()
  return {
    ...(form.column && form.column !== current.value?.column
      ? { distributionColumn: form.column }
      : {}),
    ...(bucketNumber.value !== null && bucketNumber.value !== current.value?.bucketCount
      ? { bucketCount: bucketNumber.value }
      : {}),
    ...(colocate !== undefined ? { colocateWith: colocate } : {}),
    ...(form.cascade ? { cascadeToColocated: true } : {}),
    ...(form.mode !== 'auto' ? { mode: form.mode } : {}),
    ...(form.views !== 'refuse' ? { dependentViews: form.views } : {}),
  }
}
const changesSomething = computed(() => {
  const r = request()
  return (
    r.distributionColumn !== undefined ||
    r.bucketCount !== undefined ||
    r.colocateWith !== undefined
  )
})
const nothingError = computed(() =>
  changesSomething.value
    ? undefined
    : 'Choose what to change: the column, the buckets or the colocation.',
)
/** A multiple of the old bucket count moves no row between servers (contract). */
const multiple = computed(
  () =>
    bucketNumber.value !== null &&
    current.value?.bucketCount != null &&
    bucketNumber.value !== current.value.bucketCount &&
    bucketNumber.value % current.value.bucketCount === 0,
)

const change = useTableChange(
  () => props.databaseName,
  () => props.table.schema,
  () => props.table.name,
  computed(() =>
    changesSomething.value && !bucketError.value
      ? { kind: 'alter' as const, alter: request() }
      : null,
  ),
)
const onlineAvailable = computed(() => change.preview.value?.estimate.onlineAvailable ?? true)
watch(onlineAvailable, (available) => {
  if (!available && form.mode === 'online') form.mode = 'auto'
})
const validate = (): boolean => {
  submitted.value = true
  return changesSomething.value && !bucketError.value
}
const start = () =>
  endpoints.alterTableDistribution(
    props.databaseName,
    props.table.schema,
    props.table.name,
    request(),
  )
</script>

<template>
  <TaskActionDialog
    :title="`Change the distribution of ${table.schema}.${table.name}`"
    confirm-label="Change the distribution"
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
    cancelled-text="The change was cancelled. The table is as it was before."
    wide
    @close="emit('close')"
  >
    <template #error="{ error }">
      <TableChangeRefusal :error="error" />
    </template>
    <template #default="{ fieldErrors }">
      <p>
        The rows are copied to their new buckets and the table is switched over. A change that moves
        no row is done at once. Reads go on; for writers see the choice below.
      </p>
      <dl class="kv" data-testid="alter-current">
        <dt>Now</dt>
        <dd>
          distributed by
          <code><SafeName :value="current?.column ?? '-'" :max="60" /></code>,
          {{ current?.bucketCount ?? '-' }} buckets, colocation {{ current?.colocationId ?? '-' }}
        </dd>
      </dl>
      <QueryBoundary
        :loading="detail.isPending.value"
        :error="detail.error.value"
        @retry="detail.refetch()"
      >
        <FormField
          v-slot="slot"
          label="Distribution column"
          hint="The new column has to be part of every unique key of the table."
          :error="fieldErrors.distributionColumn"
        >
          <select v-bind="slot" v-model="form.column" data-testid="alter-column">
            <option value="">Keep {{ safeText(current?.column ?? '') }}</option>
            <option
              v-for="c in columns.filter((x) => x.name !== current?.column)"
              :key="c.name"
              :value="c.name"
            >
              {{ safeText(c.name) }} ({{ safeText(c.type, 60) }})
            </option>
          </select>
        </FormField>
        <FormField
          v-slot="slot"
          label="Buckets"
          :hint="`From 1 to 4096. Empty: keep ${current?.bucketCount ?? 'the number'}. A multiple of the old number moves no row between servers.`"
          :error="bucketError ?? fieldErrors.bucketCount ?? undefined"
        >
          <input
            v-bind="slot"
            v-model="form.bucketCount"
            inputmode="numeric"
            autocomplete="off"
            data-testid="alter-buckets"
          />
        </FormField>
        <p v-if="multiple" class="muted" data-testid="alter-multiple">
          This is a multiple of the old number: no row moves between servers.
        </p>
        <FormField v-slot="slot" label="Colocation group" :error="fieldErrors.colocateWith">
          <select v-bind="slot" v-model="form.colocate" data-testid="alter-colocate">
            <option value="keep">Keep the group it is in</option>
            <option value="default">A group of the same type and bucket count</option>
            <option value="none">A group of its own</option>
            <option v-for="t in candidates" :key="tableKey(t)" :value="tableKey(t)">
              Join the group of {{ safeText(`${t.schema}.${t.name}`) }}
            </option>
          </select>
        </FormField>
        <label class="check">
          <input v-model="form.cascade" type="checkbox" data-testid="alter-cascade" />
          Change the tables of its colocation group together (blocking only)
        </label>
        <p v-if="submitted && nothingError" class="error" role="alert" data-testid="alter-nothing">
          {{ nothingError }}
        </p>
        <TableChangeOptions
          v-model:mode="form.mode"
          v-model:views="form.views"
          :online-available="onlineAvailable"
          :online-reason="change.preview.value?.estimate.onlineUnavailableReason ?? null"
          :blocking-only="form.cascade"
        />
        <TableChangePanel
          v-if="changesSomething"
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
.kv {
  display: grid;
  grid-template-columns: fit-content(45%) minmax(0, 1fr);
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dt {
  color: var(--c-text-muted);
}
.kv dd {
  margin: 0;
}
</style>
