<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Turn the approximate aggregates of one database on or off (contract 0.18.0, `configureApproximateAggregates`, kingres_admin).
// The task installs kingres_sketch on every server, checks that the versions and the hash agree, applies the database settings
// and records them. Turning it off never removes the types: a table may have sketch columns. The request is idempotent.
import { computed, reactive, ref } from 'vue'
import TaskActionDialog from './TaskActionDialog.vue'
import FormField from './FormField.vue'
import SafeName from './SafeName.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import {
  ERROR_RATE_DEFAULT_PERCENT,
  ERROR_RATE_MAX_PERCENT,
  ERROR_RATE_MIN_PERCENT,
  percentToRate,
  rateToPercent,
  type ApproximateAggregates,
  type ApproximateAggregatesRequest,
} from '@/lib/analytics'

const props = defineProps<{ databaseName: string; current: ApproximateAggregates }>()
const emit = defineEmits<{ close: [] }>()

const form = reactive({
  enabled: props.current.enabled,
  mode: props.current.countDistinct as 'exact' | 'approximate',
  percent: (props.current.errorRate !== null
    ? rateToPercent(props.current.errorRate)
    : ERROR_RATE_DEFAULT_PERCENT) as number | string,
})
const submitted = ref(false)
const approximate = computed(() => form.enabled && form.mode === 'approximate')
const percentError = computed(() => {
  if (!approximate.value) return undefined
  const n = typeof form.percent === 'number' ? form.percent : Number.NaN
  return n >= ERROR_RATE_MIN_PERCENT && n <= ERROR_RATE_MAX_PERCENT
    ? undefined
    : `Use a number from ${ERROR_RATE_MIN_PERCENT} to ${ERROR_RATE_MAX_PERCENT}.`
})
const request = computed<ApproximateAggregatesRequest>(() =>
  !form.enabled
    ? { enabled: false }
    : form.mode === 'approximate'
      ? {
          enabled: true,
          countDistinct: 'approximate',
          errorRate: percentToRate(Number(form.percent)),
        }
      : { enabled: true, countDistinct: 'exact' },
)
const validate = (): boolean => {
  submitted.value = true
  return percentError.value === undefined
}
const start = () => endpoints.configureApproximateAggregates(props.databaseName, request.value)
const confirmLabel = computed(() => (form.enabled ? 'Save and install' : 'Turn off'))
</script>

<template>
  <TaskActionDialog
    :title="`Approximate aggregates of ${databaseName}`"
    :confirm-label="confirmLabel"
    :start="start"
    :validate="validate"
    :invalidate="[
      keys.approximateAggregates(databaseName),
      keys.database(databaseName),
      keys.databases,
      keys.tasks,
    ]"
    @close="emit('close')"
  >
    <template #default="{ fieldErrors }">
      <p>
        Database <strong><SafeName :value="databaseName" :max="80" /></strong>. The task installs
        the extension on every server, checks that every server runs the same version and gives the
        same known answer, applies the settings and records them.
      </p>
      <div class="field">
        <label class="check" for="aa-enabled">
          <input id="aa-enabled" v-model="form.enabled" type="checkbox" data-testid="aa-enabled" />
          Enable on this database
        </label>
      </div>
      <FormField
        label="count(DISTINCT)"
        :error="fieldErrors.countDistinct"
        hint="Exact is the default and changes only if you choose the approximate mode."
      >
        <template #default="{ id, ...aria }">
          <select
            :id="id"
            v-model="form.mode"
            data-testid="aa-mode"
            :disabled="!form.enabled"
            v-bind="aria"
          >
            <option value="exact">Exact</option>
            <option value="approximate">Approximate</option>
          </select>
        </template>
      </FormField>
      <FormField
        v-if="approximate"
        label="Error rate (percent)"
        :error="(submitted ? percentError : undefined) ?? fieldErrors.errorRate"
        :hint="`From ${ERROR_RATE_MIN_PERCENT} to ${ERROR_RATE_MAX_PERCENT}. A lower rate needs more memory per sketch.`"
      >
        <template #default="{ id, ...aria }">
          <input
            :id="id"
            v-model.number="form.percent"
            class="input num"
            type="number"
            step="any"
            :min="ERROR_RATE_MIN_PERCENT"
            :max="ERROR_RATE_MAX_PERCENT"
            style="width: 8em"
            data-testid="aa-rate"
            v-bind="aria"
          />
        </template>
      </FormField>
      <p v-if="approximate" class="notice tone-warn" data-testid="aa-consequence">
        <span aria-hidden="true">&#9650;</span>
        <span>
          Approximate counts can differ from exact counts by the error rate. Every
          <code>count(DISTINCT x)</code> of every session on this database is answered from a sketch
          unless the session says <code>SET kingres_shard.count_distinct_mode = 'exact'</code>.
          Views and functions are planned with the setting of the session that runs them.
        </span>
      </p>
      <p v-else-if="!form.enabled" class="notice tone-info" data-testid="aa-off-note">
        <span aria-hidden="true">&#9432;</span>
        <span>
          Turning this off makes <code>count(DISTINCT)</code> exact again and switches the sketch
          pushdown off. It never removes the types hll, topn and tdigest: a table may have columns
          of those types.
        </span>
      </p>
    </template>
  </TaskActionDialog>
</template>
