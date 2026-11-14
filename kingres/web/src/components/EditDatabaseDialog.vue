<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Edit the mutable settings of a database. Fixed settings (name, encoding, collation, bucket
// count) are listed read-only with the reason. PATCH answers 200 for instant changes and 202 + Task
// when background work is needed (turning on sharding or the ledger).
import { computed, reactive, ref } from 'vue'
import SafeName from './SafeName.vue'
import TaskActionDialog from './TaskActionDialog.vue'
import FormField from './FormField.vue'
import NullableValue from './NullableValue.vue'
import HiddenPreview from './HiddenPreview.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import type { Database, DatabaseUpdateRequest, Durability, LedgerMode, Task } from '@/api/types'
import { DURABILITY_LABEL, LEDGER_MODE_LABEL } from '@/lib/status'
import { compact, firstError, integerBetween } from '@/lib/validators'

const props = defineProps<{ database: Database }>()
const emit = defineEmits<{ close: [] }>()

const d = props.database
// A setting the server reports as not available (null) starts from the contract default and is only
// sent when the user changes it.
const orig = {
  owner: d.general.owner ?? '',
  connectionLimit: d.general.connectionLimit ?? -1,
  durability: d.replication.durability ?? 'sync',
  ledgerMode: d.ledger.mode ?? 'off',
  digestInterval: d.ledger.digestIntervalSeconds ?? 3600,
} as const
const form = reactive({
  owner: orig.owner as string,
  connectionLimit: orig.connectionLimit as number,
  durability: orig.durability as Durability,
  ledgerMode: orig.ledgerMode as LedgerMode,
  digestInterval: orig.digestInterval as number,
  enableSharding: false,
})
const submitted = ref(false)

const body = computed<DatabaseUpdateRequest>(() => {
  const out: DatabaseUpdateRequest = {}
  const general: NonNullable<DatabaseUpdateRequest['general']> = {}
  if (form.owner !== orig.owner) general.owner = form.owner
  if (form.connectionLimit !== orig.connectionLimit) general.connectionLimit = form.connectionLimit
  if (Object.keys(general).length) out.general = general
  if (form.durability !== orig.durability) out.replication = { durability: form.durability }
  const ledger: NonNullable<DatabaseUpdateRequest['ledger']> = {}
  if (form.ledgerMode !== orig.ledgerMode) ledger.mode = form.ledgerMode
  if (form.digestInterval !== orig.digestInterval)
    ledger.digestIntervalSeconds = form.digestInterval
  if (Object.keys(ledger).length) out.ledger = ledger
  if (form.enableSharding) out.sharding = { enabled: true }
  return out
})

const errors = computed(() =>
  compact({
    changes: Object.keys(body.value).length === 0 ? 'Change at least one setting.' : undefined,
    'general.connectionLimit': firstError<number | null>(
      form.connectionLimit,
      integerBetween('The connection limit', -1, 1_000_000),
    ),
    'ledger.digestIntervalSeconds': firstError<number | null>(
      form.digestInterval,
      integerBetween('The digest interval', 60, 86400),
    ),
  }),
)

function validate(): boolean {
  submitted.value = true
  return Object.keys(errors.value).length === 0
}

/** Instant changes answer with the Database; present them as an already finished task. */
async function start(): Promise<Task> {
  const result = await endpoints.updateDatabase(d.name, body.value)
  if ('progress' in result) return result
  const now = new Date().toISOString()
  return {
    id: 0,
    kind: 'database.update',
    state: 'done',
    progress: {
      percent: 100,
      currentStep: null,
      steps: [{ name: 'Apply settings', state: 'done' }],
    },
    error: null,
    subject: { type: 'database', id: d.name },
    cancellable: false,
    createdAt: now,
    updatedAt: now,
    createdBy: '',
  }
}
</script>

<template>
  <TaskActionDialog
    :title="`Edit ${d.name}`"
    confirm-label="Save changes"
    wide
    :start="start"
    :validate="validate"
    :invalidate="[keys.databases, keys.database(d.name)]"
    @close="emit('close')"
  >
    <template #default="{ fieldErrors }">
      <p v-if="submitted && errors.changes" class="error" role="alert">{{ errors.changes }}</p>
      <fieldset>
        <legend>Can be changed</legend>
        <FormField v-slot="slot" label="Owner role" :error="fieldErrors['general.owner']">
          <input v-bind="slot" v-model="form.owner" class="input mono" />
          <HiddenPreview :value="form.owner" />
        </FormField>
        <FormField
          v-slot="slot"
          label="Connection limit"
          hint="-1 means unlimited."
          :error="
            (submitted ? errors['general.connectionLimit'] : undefined) ??
            fieldErrors['general.connectionLimit']
          "
        >
          <input
            v-bind="slot"
            v-model.number="form.connectionLimit"
            class="input num"
            type="number"
            min="-1"
          />
        </FormField>
        <FormField v-slot="slot" label="Durability">
          <select v-bind="slot" v-model="form.durability">
            <option v-for="(label, value) in DURABILITY_LABEL" :key="value" :value="value">
              {{ label }}
            </option>
          </select>
        </FormField>
        <FormField
          v-slot="slot"
          label="Ledger mode"
          :hint="
            orig.ledgerMode === 'off'
              ? 'Can be turned on, but never off again.'
              : 'The ledger cannot be turned off once it is on; you can only change how new tables behave.'
          "
        >
          <select v-bind="slot" v-model="form.ledgerMode">
            <option
              v-for="(label, value) in LEDGER_MODE_LABEL"
              :key="value"
              :value="value"
              :disabled="value === 'off' && orig.ledgerMode !== 'off'"
            >
              {{ label }}
            </option>
          </select>
        </FormField>
        <FormField
          v-slot="slot"
          label="Digest interval (seconds)"
          :error="
            (submitted ? errors['ledger.digestIntervalSeconds'] : undefined) ??
            fieldErrors['ledger.digestIntervalSeconds']
          "
        >
          <input
            v-bind="slot"
            v-model.number="form.digestInterval"
            class="input num"
            type="number"
            min="60"
            max="86400"
          />
        </FormField>
        <label v-if="!d.sharding.enabled" class="check">
          <input v-model="form.enableSharding" type="checkbox" />
          Turn on sharding (cannot be turned off again; existing tables stay plain until you
          distribute them)
        </label>
      </fieldset>
      <fieldset>
        <legend>Fixed after creation</legend>
        <dl class="fixed">
          <dt>Name</dt>
          <dd class="mono"><SafeName :value="d.name" :max="80" /></dd>
          <dt>Encoding</dt>
          <dd class="mono"><NullableValue :value="d.general.encoding" /></dd>
          <dt>Collation</dt>
          <dd class="mono">
            <NullableValue :value="d.general.lcCollate" /> /
            <NullableValue :value="d.general.lcCtype" />
          </dd>
          <dt>Bucket count</dt>
          <dd><NullableValue :value="d.sharding.bucketCount" /></dd>
        </dl>
        <p class="hint muted">
          The replica count is a cluster setting and is changed in cluster settings.
        </p>
      </fieldset>
    </template>
  </TaskActionDialog>
</template>

<style scoped>
.fixed {
  display: grid;
  grid-template-columns: fit-content(45%) minmax(0, 1fr);
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.fixed dd {
  margin: 0;
}
</style>
