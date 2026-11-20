<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Creates a bundle for offline verification (contract 0.11.0, design 7.3): which logs, since which size, whether the anchor
// evidence goes in, and optionally the inclusion proof of one row (with the warning of what it reveals). The bundle is made by a
// task; when it is done the dialog offers the download (the archive is kept for 24 hours). The id of the bundle is read from the
// detail of the task, as the contract has no member for the result of a task.
import { computed, reactive, ref } from 'vue'
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import FormField from '@/components/FormField.vue'
import SafeName from '@/components/SafeName.vue'
import LedgerRefusal from '@/components/ledger/LedgerRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useLedgerPublication } from '@/api/queries'
import { saveBlobFile } from '@/lib/download'
import { BUNDLE_TEXT, bundleIdOf, PROOFS_WARNING } from '@/lib/ledger-ops'
import type { Task } from '@/api/types'

const props = defineProps<{ databaseName: string }>()
const emit = defineEmits<{ close: [] }>()
const publication = useLedgerPublication()
const logs = computed(() => publication.data.value?.logs ?? [])
const form = reactive({
  all: true,
  chosen: {} as Record<string, boolean>,
  since: '',
  anchors: true,
  proof: false,
  schema: 'public',
  table: '',
  key: '',
})
const errors = ref<Record<string, string>>({})

function validate(): boolean {
  const e: Record<string, string> = {}
  if (form.since.trim() !== '' && !/^\d+$/.test(form.since.trim()))
    e.since = 'Give a whole number of entries, or leave it empty.'
  if (!form.all && !logs.value.some((l) => form.chosen[l.origin]))
    e.logs = 'Choose at least one log, or take all of them.'
  if (form.proof) {
    if (form.table.trim() === '') e.table = 'Name the table.'
    try {
      const k: unknown = JSON.parse(form.key)
      if (typeof k !== 'object' || k === null || Array.isArray(k)) throw new Error('x')
    } catch {
      e.key = 'Give the whole primary key as a JSON object, for example {"id": 42}.'
    }
  }
  errors.value = e
  return Object.keys(e).length === 0
}
const start = () =>
  endpoints.createLedgerBundle(props.databaseName, {
    includeAnchors: form.anchors,
    ...(form.all
      ? {}
      : { logs: logs.value.filter((l) => form.chosen[l.origin]).map((l) => l.origin) }),
    ...(form.since.trim() !== '' ? { sinceSize: Number(form.since) } : {}),
    ...(form.proof
      ? {
          proofs: [
            {
              schema: form.schema.trim(),
              table: form.table.trim(),
              primaryKey: JSON.parse(form.key) as Record<string, string | number | boolean>,
            },
          ],
        }
      : {}),
  })

const downloadError = ref<unknown>(null)
const downloading = ref(false)
async function download(task: Task): Promise<void> {
  const id = bundleIdOf(task.detail)
  if (id === null) return
  downloading.value = true
  downloadError.value = null
  try {
    saveBlobFile(
      `ledger-bundle-${id}.tar.gz`,
      await endpoints.getLedgerBundle(props.databaseName, id),
    )
  } catch (e) {
    downloadError.value = e
  } finally {
    downloading.value = false
  }
}
</script>

<template>
  <TaskActionDialog
    title="Create a bundle for offline verification"
    confirm-label="Create the bundle"
    wide
    :start="start"
    :validate="validate"
    :invalidate="[keys.ledger(databaseName), keys.tasks]"
    @close="emit('close')"
  >
    <template #error="{ error }"><LedgerRefusal :error="error" ops="bundle" /></template>
    <template #default>
      <p>
        Database <strong><SafeName :value="databaseName" :max="60" /></strong>.
      </p>
      <p data-testid="bundle-text">{{ BUNDLE_TEXT }}</p>
      <fieldset class="stack">
        <legend>Logs</legend>
        <label class="check"
          ><input v-model="form.all" type="checkbox" data-testid="bundle-all" /> All the published
          logs</label
        >
        <template v-if="!form.all">
          <p v-if="logs.length === 0" class="muted">No log is published yet.</p>
          <label v-for="l in logs" :key="l.origin" class="check">
            <input v-model="form.chosen[l.origin]" type="checkbox" />
            <SafeName :value="l.origin" kind="text" :max="120" />
          </label>
          <span v-if="errors.logs" class="error" role="alert">{{ errors.logs }}</span>
        </template>
      </fieldset>
      <FormField
        v-slot="slot"
        label="Only the part after this size (optional)"
        hint="The number of entries a log already had. Empty: from the start."
        :error="errors.since"
      >
        <input
          v-bind="slot"
          v-model="form.since"
          class="input"
          inputmode="numeric"
          autocomplete="off"
        />
      </FormField>
      <label class="check"
        ><input v-model="form.anchors" type="checkbox" /> Include the evidence of the anchors</label
      >
      <label class="check"
        ><input v-model="form.proof" type="checkbox" data-testid="bundle-proof" /> Include the
        inclusion proof of a row</label
      >
      <template v-if="form.proof">
        <p class="notice tone-warn" role="status" data-testid="proofs-warning">
          <span aria-hidden="true">&#9650;</span><span>{{ PROOFS_WARNING }}</span>
        </p>
        <FormField v-slot="slot" label="Schema"
          ><input v-bind="slot" v-model="form.schema" class="input mono" autocomplete="off"
        /></FormField>
        <FormField v-slot="slot" label="Table" :error="errors.table"
          ><input v-bind="slot" v-model="form.table" class="input mono" autocomplete="off"
        /></FormField>
        <FormField v-slot="slot" label="Primary key (JSON)" :error="errors.key"
          ><input v-bind="slot" v-model="form.key" class="input mono" autocomplete="off"
        /></FormField>
      </template>
    </template>
    <template #result="{ task }">
      <p class="notice tone-ok" role="status" data-testid="bundle-ready">
        <span aria-hidden="true">&#10003;</span
        ><span>The bundle is ready and is kept for 24 hours.</span>
      </p>
      <div class="row">
        <button
          v-if="task && bundleIdOf(task.detail) !== null"
          type="button"
          class="btn btn-primary"
          :disabled="downloading"
          data-testid="bundle-download"
          @click="download(task)"
        >
          Download the bundle
        </button>
        <span v-else class="muted"
          >The server did not say which bundle it made; find it on the Tasks page.</span
        >
      </div>
      <LedgerRefusal v-if="downloadError" :error="downloadError" ops="download" />
    </template>
  </TaskActionDialog>
</template>
