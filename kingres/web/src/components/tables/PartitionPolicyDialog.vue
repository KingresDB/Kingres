<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The time-partitioning policy of a table (contract 0.18.0: `getPartitionPolicy`, `putPartitionPolicy`, `planPartitionPolicy`,
// kingres_admin to save): how long each partition is, how far ahead partitions exist, when old partitions are converted to
// columnar storage, and when they are dropped or detached. "Preview" asks the server what the next run would create, convert and
// drop, with sizes, before anything is saved. Saving a retention, or a shorter one, deletes data at the next run, so it needs a
// preview of the form as it is now and the name of the table typed. The server checks every field and answers with the JSON
// pointer of the ones it refuses; they are shown next to the field. The policy is stored at once and creates no partition: the
// scheduler takes the table at its next tick.
import { computed, reactive, ref, watch } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import AppDialog from '../AppDialog.vue'
import FormField from '../FormField.vue'
import ProblemNotice from '../ProblemNotice.vue'
import QueryBoundary from '../QueryBoundary.vue'
import SafeName from '../SafeName.vue'
import TypeToConfirm from '../TypeToConfirm.vue'
import PartitionActionsTable from './PartitionActionsTable.vue'
import OwnerNotice from './OwnerNotice.vue'
import PlanRemovals from './PlanRemovals.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys, usePartitionPolicy } from '@/api/queries'
import {
  COMPRESSION_LABEL,
  DEFAULT_POLICY,
  DURATION_UNITS,
  durationText,
  type KeptField,
  INTERVAL_PRESETS,
  formOf,
  lowersRetention,
  pointerField,
  requestOf,
  type PartitionPlan,
  type PolicyForm,
} from '@/lib/analytics'
import type { Table } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table }>()
const emit = defineEmits<{ close: [] }>()

const client = useQueryClient()
const hasTimeKey = computed(() => props.table.partitioning?.columnType !== undefined)
const hasDefault = computed(() => props.table.partitioning?.defaultPartition === true)
const usable = computed(() => hasTimeKey.value && !hasDefault.value)
const stored = usePartitionPolicy(
  () => props.databaseName,
  () => props.table.schema,
  () => props.table.name,
  usable,
)

const form = reactive<PolicyForm>({ ...DEFAULT_POLICY })
const loaded = ref(false)
// The form starts from the stored policy once it has been READ (a table with none, a 404, starts from the defaults). A policy
// that could not be read never turns into the defaults: the form stays hidden and Save is off.
watch(
  () => stored.isSuccess.value,
  (ok) => {
    if (!ok || loaded.value) return
    Object.assign(form, formOf(stored.data.value ?? null))
    loaded.value = true
  },
  { immediate: true },
)

const HOURS = [1, 2, 3, 4, 6, 8, 12]
watch(
  () => form.customUnit,
  (u) => {
    if (u === 'hour' && !HOURS.includes(Number(form.customCount))) form.customCount = 6
  },
)
const request = computed(() => requestOf(form))
const KEPT_LABEL: Record<KeptField, string> = {
  partitionEvery: 'interval of a partition',
  createAhead: 'time to create ahead',
  retention: 'retention',
  columnarAfter: 'time to convert after',
  runEvery: 'time between runs',
}
const kept = computed(() =>
  (Object.keys(form.keep) as KeptField[]).map((key) => ({
    key,
    label: KEPT_LABEL[key],
    value: form.keep[key] as string,
  })),
)

const positive = (v: number | string): boolean =>
  typeof v === 'number' && Number.isInteger(v) && v >= 1
const errors = computed<Record<string, string | undefined>>(() => {
  const e: Record<string, string | undefined> = {}
  const whole = 'Use a whole number of 1 or more.'
  if (form.interval === 'custom' && !positive(form.customCount)) e.partitionEvery = whole
  if (!positive(form.aheadCount)) e.createAhead = whole
  if (form.retentionOn && !positive(form.retentionCount)) e.retention = whole
  if (form.columnarOn && !positive(form.columnarCount)) e.columnarAfter = whole
  if (!positive(form.runEveryCount)) e.runEvery = whole
  if (form.columnarOn && form.compression === 'zstd') {
    const n = form.compressionLevel
    if (!(typeof n === 'number' && Number.isInteger(n) && n >= 1 && n <= 19))
      e.compressionLevel = 'Use a whole number from 1 to 19.'
  }
  return e
})
const valid = computed(() => Object.values(errors.value).every((x) => x === undefined))

// --- preview --------------------------------------------------------------------------------------------
const previewed = ref<{ request: string; plan: PartitionPlan } | null>(null)
const preview = useMutation({
  mutationFn: () =>
    endpoints.planPartitionPolicy(
      props.databaseName,
      props.table.schema,
      props.table.name,
      request.value,
    ),
  onSuccess: (plan) => {
    previewed.value = { request: JSON.stringify(request.value), plan }
  },
})
const fresh = computed(
  () => previewed.value !== null && previewed.value.request === JSON.stringify(request.value),
)
const summary = computed(() => {
  const actions = previewed.value?.plan.actions ?? []
  const n = (k: string) => actions.filter((a) => a.kind === k).length
  return {
    create: n('create'),
    convert: n('convert'),
    drop: n('drop'),
    detach: n('detach'),
  }
})

// --- save -----------------------------------------------------------------------------------------------
const typed = ref('')
const removesData = computed(() => lowersRetention(stored.data.value ?? null, request.value))
const typedOk = computed(() => !removesData.value || typed.value === props.table.name)
const previewNeeded = computed(() => removesData.value && !fresh.value)
const save = useMutation({
  mutationFn: () =>
    endpoints.putPartitionPolicy(
      props.databaseName,
      props.table.schema,
      props.table.name,
      request.value,
    ),
  onSuccess: async () => {
    await Promise.all([
      client.invalidateQueries({
        queryKey: keys.partitionPolicy(props.databaseName, props.table.schema, props.table.name),
      }),
      client.invalidateQueries({
        queryKey: keys.table(props.databaseName, props.table.schema, props.table.name),
      }),
      client.invalidateQueries({ queryKey: keys.tables(props.databaseName) }),
      client.invalidateQueries({
        queryKey: keys.partitionRuns(props.databaseName, props.table.schema, props.table.name),
      }),
    ])
    emit('close')
  },
})
const serverErrors = computed<Record<string, string>>(() => {
  const out: Record<string, string> = {}
  if (!save.error.value && !preview.error.value) return out
  const fields = describeProblem(save.error.value ?? preview.error.value).fieldErrors
  for (const [pointer, msg] of Object.entries(fields)) {
    const f = pointerField(pointer)
    if (!(f in out)) out[f] = msg
  }
  return out
})
const err = (field: string): string | undefined => errors.value[field] ?? serverErrors.value[field]
const busy = computed(() => save.isPending.value)
const canSave = computed(
  () =>
    loaded.value &&
    stored.isSuccess.value &&
    valid.value &&
    typedOk.value &&
    !previewNeeded.value &&
    !busy.value,
)
const EXAMPLE = computed(
  () =>
    `CREATE TABLE ${props.table.schema}.${props.table.name} (...) PARTITION BY RANGE (<time column>);`,
)
</script>

<template>
  <AppDialog
    :title="`Time partitioning of ${table.schema}.${table.name}`"
    wide
    :persistent="busy"
    @close="emit('close')"
  >
    <div v-if="!hasTimeKey" class="stack" data-testid="policy-not-partitioned">
      <p class="notice tone-info" role="status">
        <span aria-hidden="true">&#9432;</span>
        <span>
          A policy needs a table that is already partitioned by range on a date or timestamp column.
          This table is not. Create it as a partitioned table with SQL in the query workbench, then
          set the policy here. For example:
        </span>
      </p>
      <pre
        class="mono example"
        data-testid="policy-example"
      ><SafeName :value="EXAMPLE" kind="text" :max="300" /></pre>
    </div>
    <div v-else-if="hasDefault" class="stack" data-testid="policy-default-partition">
      <p class="notice tone-warn" role="alert">
        <span aria-hidden="true">&#9650;</span>
        <span>
          This table has a default partition, so the scheduler cannot create the partitions that
          follow: rows that belong to them would have to move out of it first. Move the rows of the
          default partition into partitions of their own, drop the default partition, and set the
          policy again.
        </span>
      </p>
    </div>
    <QueryBoundary
      v-else
      :loading="stored.isPending.value && !loaded"
      :error="stored.error.value"
      @retry="stored.refetch()"
    >
      <form class="stack" data-testid="policy-form" @submit.prevent>
        <OwnerNotice
          v-if="stored.data.value?.ownerTrusted === false"
          :owner="stored.data.value?.tableOwner"
        />
        <p>
          Partition column
          <code><SafeName :value="table.partitioning?.key ?? ''" :max="60" /></code>
          <span class="muted"> ({{ table.partitioning?.columnType }}, read from the table)</span>
        </p>

        <div v-if="kept.length > 0" class="stack" data-testid="policy-kept">
          <p v-for="k in kept" :key="k.key" class="notice tone-info" role="status">
            <span aria-hidden="true">&#9432;</span>
            <span>
              The stored {{ k.label }} is
              <code><SafeName :value="k.value" kind="text" :max="60" /></code> ({{
                durationText(k.value)
              }}). The form cannot show it, so it is kept as it is and saved unchanged.
              <button type="button" class="btn btn-small" @click="delete form.keep[k.key]">
                Change it
              </button>
            </span>
          </p>
        </div>
        <div class="form-row">
          <FormField
            v-if="!form.keep.partitionEvery"
            label="Each partition covers"
            :error="err('partitionEvery')"
          >
            <template #default="{ id, ...aria }">
              <select :id="id" v-model="form.interval" data-testid="policy-interval" v-bind="aria">
                <option v-for="p in INTERVAL_PRESETS" :key="p.value" :value="p.value">
                  {{ p.label }}
                </option>
              </select>
            </template>
          </FormField>
          <template v-if="form.interval === 'custom'">
            <FormField label="Every" :error="errors.partitionEvery">
              <template #default="{ id, ...aria }">
                <input
                  v-if="form.customUnit === 'day'"
                  :id="id"
                  v-model.number="form.customCount"
                  class="input num"
                  type="number"
                  min="1"
                  max="3660"
                  style="width: 6em"
                  data-testid="policy-custom-count"
                  v-bind="aria"
                />
                <select
                  v-else
                  :id="id"
                  v-model.number="form.customCount"
                  data-testid="policy-custom-count"
                  v-bind="aria"
                >
                  <option v-for="h in HOURS" :key="h" :value="h">{{ h }}</option>
                </select>
              </template>
            </FormField>
            <FormField label="Interval unit">
              <template #default="{ id, ...aria }">
                <select
                  :id="id"
                  v-model="form.customUnit"
                  data-testid="policy-custom-unit"
                  v-bind="aria"
                >
                  <option value="hour">hours</option>
                  <option value="day">days</option>
                </select>
              </template>
            </FormField>
          </template>
        </div>

        <div class="form-row">
          <FormField
            v-if="!form.keep.createAhead"
            label="Create ahead"
            hint="How far in advance partitions exist; keep it larger than a typical bucket move."
            :error="err('createAhead')"
          >
            <template #default="{ id, ...aria }">
              <input
                :id="id"
                v-model.number="form.aheadCount"
                class="input num"
                type="number"
                min="1"
                style="width: 6em"
                data-testid="policy-ahead"
                v-bind="aria"
              />
            </template>
          </FormField>
          <FormField v-if="!form.keep.createAhead" label="Create ahead unit">
            <template #default="{ id, ...aria }">
              <select :id="id" v-model="form.aheadUnit" v-bind="aria">
                <option v-for="u in DURATION_UNITS" :key="u" :value="u">{{ u }}s</option>
              </select>
            </template>
          </FormField>
        </div>

        <fieldset class="stack">
          <legend>Retention</legend>
          <label class="check"
            ><input v-model="form.retentionOn" type="checkbox" data-testid="policy-retention-on" />
            Remove partitions that end before</label
          >
          <div v-if="form.retentionOn" class="form-row">
            <FormField
              v-if="!form.keep.retention"
              label="Remove older than"
              :error="err('retention')"
            >
              <template #default="{ id, ...aria }">
                <input
                  :id="id"
                  v-model.number="form.retentionCount"
                  class="input num"
                  type="number"
                  min="1"
                  style="width: 6em"
                  data-testid="policy-retention"
                  v-bind="aria"
                />
              </template>
            </FormField>
            <FormField v-if="!form.keep.retention" label="Retention unit">
              <template #default="{ id, ...aria }">
                <select :id="id" v-model="form.retentionUnit" v-bind="aria">
                  <option v-for="u in DURATION_UNITS" :key="u" :value="u">{{ u }}s</option>
                </select>
              </template>
            </FormField>
            <FormField label="Action" :error="err('retentionAction')">
              <template #default="{ id, ...aria }">
                <select
                  :id="id"
                  v-model="form.retentionAction"
                  data-testid="policy-retention-action"
                  v-bind="aria"
                >
                  <option value="drop">Drop (the rows are deleted)</option>
                  <option value="detach">Detach (the rows are kept in a table of their own)</option>
                </select>
              </template>
            </FormField>
          </div>
        </fieldset>

        <fieldset class="stack">
          <legend>Convert to columnar</legend>
          <label class="check"
            ><input v-model="form.columnarOn" type="checkbox" data-testid="policy-columnar-on" />
            Convert partitions that end before</label
          >
          <template v-if="form.columnarOn">
            <div class="form-row">
              <FormField
                v-if="!form.keep.columnarAfter"
                label="Convert older than"
                :error="err('columnarAfter')"
              >
                <template #default="{ id, ...aria }">
                  <input
                    :id="id"
                    v-model.number="form.columnarCount"
                    class="input num"
                    type="number"
                    min="1"
                    style="width: 6em"
                    data-testid="policy-columnar"
                    v-bind="aria"
                  />
                </template>
              </FormField>
              <FormField v-if="!form.keep.columnarAfter" label="Convert-after unit">
                <template #default="{ id, ...aria }">
                  <select :id="id" v-model="form.columnarUnit" v-bind="aria">
                    <option v-for="u in DURATION_UNITS" :key="u" :value="u">{{ u }}s</option>
                  </select>
                </template>
              </FormField>
              <FormField label="Compression" :error="err('compression')">
                <template #default="{ id, ...aria }">
                  <select :id="id" v-model="form.compression" v-bind="aria">
                    <option v-for="(label, key) in COMPRESSION_LABEL" :key="key" :value="key">
                      {{ label }}
                    </option>
                  </select>
                </template>
              </FormField>
              <FormField
                v-if="form.compression === 'zstd'"
                label="Level"
                :error="err('compressionLevel')"
              >
                <template #default="{ id, ...aria }">
                  <input
                    :id="id"
                    v-model.number="form.compressionLevel"
                    class="input num"
                    type="number"
                    min="1"
                    max="19"
                    style="width: 6em"
                    v-bind="aria"
                  />
                </template>
              </FormField>
            </div>
            <FormField
              label="Sort rows by (columns, separated by commas)"
              hint="Orders the rows of a columnar partition, which helps the skipping of stripes."
              :error="err('columnarSortBy')"
            >
              <template #default="{ id, ...aria }">
                <input
                  :id="id"
                  v-model="form.sortBy"
                  class="input mono"
                  autocomplete="off"
                  spellcheck="false"
                  v-bind="aria"
                />
              </template>
            </FormField>
          </template>
        </fieldset>

        <div class="form-row">
          <FormField
            label="Time zone"
            hint="The name of a time zone, for example UTC or Europe/Berlin."
            :error="err('timezone')"
          >
            <template #default="{ id, ...aria }">
              <input
                :id="id"
                v-model="form.timezone"
                class="input"
                autocomplete="off"
                spellcheck="false"
                style="width: 14em"
                data-testid="policy-timezone"
                v-bind="aria"
              />
            </template>
          </FormField>
          <FormField v-if="!form.keep.runEvery" label="Run every" :error="err('runEvery')">
            <template #default="{ id, ...aria }">
              <input
                :id="id"
                v-model.number="form.runEveryCount"
                class="input num"
                type="number"
                min="1"
                style="width: 6em"
                v-bind="aria"
              />
            </template>
          </FormField>
          <FormField v-if="!form.keep.runEvery" label="Run every unit">
            <template #default="{ id, ...aria }">
              <select :id="id" v-model="form.runEveryUnit" v-bind="aria">
                <option v-for="u in DURATION_UNITS" :key="u" :value="u">{{ u }}s</option>
              </select>
            </template>
          </FormField>
        </div>
        <label class="check"
          ><input v-model="form.enabled" type="checkbox" data-testid="policy-enabled" /> The policy
          is on (the scheduler runs it)</label
        >

        <div class="stack" data-testid="policy-preview-area">
          <div class="row">
            <button
              type="button"
              class="btn"
              data-testid="policy-preview"
              :disabled="!valid || preview.isPending.value || busy"
              @click="preview.mutate()"
            >
              {{ preview.isPending.value ? 'Reading...' : 'Preview' }}
            </button>
            <span class="muted">Shows what the next run would do. Nothing is changed.</span>
          </div>
          <ProblemNotice v-if="preview.error.value" :error="preview.error.value" />
          <template v-if="previewed">
            <p
              v-if="!fresh"
              class="notice tone-warn"
              role="status"
              data-testid="policy-preview-stale"
            >
              <span aria-hidden="true">&#9650;</span>
              <span
                >The form has changed since this preview. Preview again to see the new plan.</span
              >
            </p>
            <div :class="{ 'is-stale': !fresh }" class="stack" data-testid="policy-preview-result">
              <p data-testid="policy-preview-summary" role="status">
                The next run would create {{ summary.create }}
                {{ summary.create === 1 ? 'partition' : 'partitions' }}, convert
                {{ summary.convert }}, drop {{ summary.drop }} and detach {{ summary.detach }}.
              </p>
              <PlanRemovals
                :actions="previewed.plan.actions"
                testid="policy-preview-deletes"
                tail="Saving the policy deletes nothing now; the next run does."
              />
              <PartitionActionsTable
                v-if="previewed.plan.actions.length > 0"
                :actions="previewed.plan.actions"
                :table="table.name"
                caption="What the next run would do"
              />
              <p v-else class="muted">The next run would find nothing to do.</p>
            </div>
          </template>
        </div>

        <template v-if="removesData">
          <p
            v-if="previewNeeded"
            class="notice tone-warn"
            role="status"
            data-testid="policy-need-preview"
          >
            <span aria-hidden="true">&#9650;</span>
            <span>
              This policy removes old partitions. Preview it first, so that you see the data it
              would delete.
            </span>
          </p>
          <TypeToConfirm v-model="typed" :expected="table.name" />
        </template>
        <ProblemNotice v-if="save.error.value" :error="save.error.value" />
      </form>
    </QueryBoundary>

    <template #footer>
      <button type="button" class="btn" :disabled="busy" @click="emit('close')">
        {{ usable ? 'Cancel' : 'Close' }}
      </button>
      <button
        v-if="usable && !stored.error.value"
        type="button"
        class="btn btn-primary"
        data-testid="policy-save"
        :disabled="!canSave"
        @click="save.mutate()"
      >
        {{ busy ? 'Saving...' : 'Save policy' }}
      </button>
    </template>
  </AppDialog>
</template>

<style scoped>
fieldset {
  border: 1px solid var(--c-border);
  border-radius: var(--radius-sm);
  padding: var(--s-3);
  margin: 0;
}
legend {
  padding-inline: var(--s-1);
}
.example {
  overflow-x: auto;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  margin: 0;
}
.is-stale {
  opacity: 0.6;
}
</style>
