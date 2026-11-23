<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Import a CSV file into a table, in four steps: choose the file (the first 64 KiB are previewed in the browser and the
// delimiter, header and encoding are guessed for the person to correct), set the options (the import is all or nothing, and
// the dialog says what that means and what the 2 GiB limit means), send the file (resumable: pause, resume, a break, a
// session that ends), and follow the import as a task. A failed import shows the whitelisted sentence of the server and the
// line number, never a value; `import_outcome_unknown` says to check the table and offers no repeat.
import { computed, reactive, ref, watch } from 'vue'
import AppDialog from '@/components/AppDialog.vue'
import FormField from '@/components/FormField.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import SafeName from '@/components/SafeName.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import ImportFileStep from './ImportFileStep.vue'
import CsvPreview from './CsvPreview.vue'
import UploadStatus from './UploadStatus.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys, useClusterSettings, useTableDetail } from '@/api/queries'
import type { CsvDelimiter, CsvEncoding, Table } from '@/api/types'
import { useSessionStore } from '@/stores/session'
import { useTaskAction } from '@/composables/useTask'
import { useUpload } from '@/composables/useUpload'
import type { CsvGuess } from '@/lib/csv-guess'
import { formatBytes, formatNumber } from '@/lib/format'
import { atomicCapFor, DELIMITER_LABEL, ENCODING_LABEL } from '@/lib/transfer'
import {
  defaultImportForm,
  importRequest,
  nullStringError,
  quoteError,
  utf8Length,
} from '@/lib/transfer-form'
import { findStoredUpload, fingerprintFile, forgetUpload, markUpload } from '@/lib/upload-store'

const props = defineProps<{ databaseName: string; table: Table }>()
const emit = defineEmits<{ close: [] }>()

type Step = 'file' | 'options' | 'send' | 'import'
const step = ref<Step>('file')
const session = useSessionStore()
const settings = useClusterSettings()
const detail = useTableDetail(props.databaseName, props.table.schema, props.table.name)
const off = computed(() => settings.data.value?.dataTransfer?.enabled === false)
const maxFileBytes = computed(() => settings.data.value?.dataTransfer?.maxFileBytes ?? null)
/** The most data one import of this table takes: 2 GiB, and 256 MiB for a reference table (written on every group). */
const cap = computed(() =>
  atomicCapFor(props.table.classes, settings.data.value?.dataTransfer?.maxAtomicBytes),
)
const allColumns = computed(() => detail.data.value?.columns.map((c) => c.name) ?? [])

const form = reactive(defaultImportForm())
const file = ref<File | null>(null)
const bytes = ref<Uint8Array>(new Uint8Array())
const guess = ref<CsvGuess | null>(null)
const refused = ref<string | null>(null)
const chooseColumns = ref(false)
const picked = ref<string[]>([])
const submitted = ref(false)
/** A hash of the beginning and the end of the chosen file, so that an upload is continued only for the same content. */
const fingerprint = ref<string | null>(null)
/** The file cannot be previewed (compressed), so whether its first line is names is for the person to say. */
const headerPending = ref(false)
/** The person confirmed that an earlier import of this very file was checked (see `remembered.imported`). */
const acknowledgedRepeat = ref(false)

const target = {
  database: props.databaseName,
  schema: props.table.schema,
  table: props.table.name,
}
const upload = useUpload(target)
/** The id of a complete upload that the import uses; kept after a failed import so the file is not sent again. */
const completeUpload = ref<string | null>(null)
const signingIn = ref(false)
const signInError = ref<unknown>(null)

function onChosen(p: {
  file: File
  bytes: Uint8Array
  guess: CsvGuess
  refused: string | null
}): void {
  file.value = p.file
  bytes.value = p.bytes
  guess.value = p.guess
  refused.value = p.refused
  completeUpload.value = null
  acknowledgedRepeat.value = false
  fingerprint.value = null
  void fingerprintFile(p.file).then((f) => {
    if (file.value === p.file) fingerprint.value = f
  })
  headerPending.value = p.guess.compressed
  // The guess is the starting point; the person corrects it in the next step.
  form.encoding = p.guess.encoding
  form.delimiter = p.guess.delimiter
  // A file that cannot be previewed gets no default: whether its first line is names is for the person to say.
  form.header = p.guess.compressed ? ('' as typeof form.header) : p.guess.header ? 'skip' : 'none'
  form.skipBom = true
  form.compression = p.guess.compressed ? 'gzip' : null
}
function onCleared(): void {
  file.value = null
  guess.value = null
  refused.value = null
}

const errors = computed(() => {
  const e: Record<string, string> = {}
  const nul = nullStringError(form.nullString, form.delimiter, form.quote)
  if (nul) e.nullString = nul
  const q = quoteError(form.quote, form.encoding)
  if (q) e.quote = q
  if (chooseColumns.value && picked.value.length === 0) e.columns = 'Choose at least one column.'
  if (headerPending.value)
    e.header = 'This file cannot be previewed here. Say whether its first line holds column names.'
  if (remembered.value?.imported && !acknowledgedRepeat.value)
    e.repeat = 'Say that you checked the table, or choose another file.'
  return e
})

const task = useTaskAction<void>(
  () =>
    endpoints.importTable(
      props.databaseName,
      props.table.schema,
      props.table.name,
      importRequest(completeUpload.value as string, {
        ...form,
        columns: chooseColumns.value ? [...picked.value] : null,
      }),
    ),
  {
    invalidate: () => [keys.tables(props.databaseName), keys.databases, keys.dataExports],
    onDone: () => {
      if (completeUpload.value && !form.keepUpload) forgetUpload(completeUpload.value)
    },
    label: () => `Import into ${props.table.schema}.${props.table.name}`,
  },
)

const outcomeUnknown = computed(() => task.task.value?.error?.code === 'import_outcome_unknown')
const importFailed = computed(() => task.state.value === 'failed')
const importError = computed(() => task.error.value)
const rowEstimate = computed(() => props.table.estimatedRows)

/** The upload of this file that this tab remembers, if any. */
const remembered = computed(() =>
  file.value
    ? findStoredUpload(file.value, target, session.username ?? '', fingerprint.value)
    : null,
)

function toOptions(): void {
  if (file.value && refused.value === null) step.value = 'options'
}

async function startSending(): Promise<void> {
  submitted.value = true
  if (Object.keys(errors.value).length > 0 || file.value === null) return
  if (completeUpload.value !== null) return startImport()
  step.value = 'send'
  await upload.start(file.value, {
    existing: remembered.value?.uploadId,
    compressed: guess.value?.compressed ?? false,
    maxFileBytes: maxFileBytes.value,
    maxAtomicBytes: cap.value,
    fingerprint: fingerprint.value,
  })
}

async function startImport(): Promise<void> {
  if (completeUpload.value === null) return
  step.value = 'import'
  task.reset()
  await task.run()
  // The upload is gone (expired or deleted): it has to be sent again.
  if (task.error.value && describeProblem(task.error.value).code === 'not_found') {
    completeUpload.value = null
  }
}

// What became of an import is remembered with the upload: after an unknown outcome, and after a success that kept the
// file, choosing the same file again does not import it again without a word.
watch(
  () => task.state.value,
  (state) => {
    const id = completeUpload.value
    if (!id) return
    if (state === 'failed' && task.task.value?.error?.code === 'import_outcome_unknown')
      markUpload(id, 'unknown')
    if (state === 'done' && form.keepUpload) markUpload(id, 'done')
  },
)

// When the whole file is on the server, the import starts by itself: the options were confirmed before the upload.
watch(
  () => upload.snapshot.value.phase,
  (phase) => {
    if (phase === 'complete' && step.value === 'send' && upload.snapshot.value.uploadId) {
      completeUpload.value = upload.snapshot.value.uploadId
      void startImport()
    }
  },
)

async function signInAgain(password: string): Promise<void> {
  signingIn.value = true
  signInError.value = null
  try {
    await session.signIn(session.username ?? '', password)
    await upload.continueAfterSignIn()
  } catch (error) {
    signInError.value = error
  } finally {
    signingIn.value = false
  }
}

async function cancelUpload(): Promise<void> {
  await upload.cancel()
  step.value = 'options'
}

function backToOptions(): void {
  task.reset()
  step.value = 'options'
}

/** Closing while the file is being sent stops the request; the server and the tab keep what they have. */
function close(): void {
  emit('close')
}

const titleText = computed(() => `Import CSV into ${props.table.schema}.${props.table.name}`)
const busy = computed(() => task.isStarting.value)
const headerIsNames = computed(() => form.header !== 'none')
</script>

<template>
  <AppDialog :title="titleText" wide :persistent="busy" @close="close">
    <!-- Step 1 -->
    <div v-show="step === 'file'" class="stack">
      <p v-if="off" class="notice tone-info" role="status" data-testid="import-off">
        <span aria-hidden="true">&#9432;</span>
        <span>Data transfer is turned off for this cluster (Settings, Data transfer).</span>
      </p>
      <p>
        The rows of the file are <strong>added</strong> to
        <code><SafeName :value="`${table.schema}.${table.name}`" :max="100" /></code>; nothing is
        replaced.
        <template v-if="rowEstimate !== null && rowEstimate !== undefined"
          >The table has about {{ formatNumber(rowEstimate) }} rows now (an estimate).</template
        >
      </p>
      <ImportFileStep
        :max-file-bytes="maxFileBytes"
        :max-atomic-bytes="cap"
        @chosen="onChosen"
        @cleared="onCleared"
      />
      <template v-if="file && guess">
        <p
          v-if="remembered && refused === null"
          class="notice tone-info"
          data-testid="import-remembered"
        >
          <span aria-hidden="true">&#9432;</span>
          <span
            >This tab started to upload this file earlier. The upload is continued from where the
            server has it.</span
          >
        </p>
        <div v-if="!guess.compressed && !guess.empty" class="stack">
          <p class="muted" data-testid="import-guess">
            Guessed from the first 64 KiB: {{ ENCODING_LABEL[guess.encoding] }},
            {{ DELIMITER_LABEL[guess.delimiter] ?? 'a delimiter' }},
            {{ guess.header ? 'a header line' : 'no header line' }}. You can correct this in the
            next step.
          </p>
          <CsvPreview
            :bytes="bytes"
            :encoding="guess.encoding"
            :delimiter="guess.delimiter"
            quote='"'
            :partial="guess.partial"
            :header="guess.header"
          />
        </div>
      </template>
    </div>

    <!-- Step 2 -->
    <form v-if="step === 'options'" class="stack" novalidate @submit.prevent="startSending">
      <div v-if="remembered?.imported" class="stack" data-testid="repeat-warning">
        <p class="notice tone-danger" role="alert">
          <span aria-hidden="true">&#10006;</span>
          <span
            ><template v-if="remembered.imported === 'unknown'"
              >An import of this very file ended without a known outcome: some of its rows may be in
              the table already.</template
            ><template v-else
              >This file was imported before and the upload was kept on the server: its rows are in
              the table already.</template
            >
            Importing it again adds the rows again (a table with a unique key answers with the key's
            own error).</span
          >
        </p>
        <label class="check"
          ><input v-model="acknowledgedRepeat" type="checkbox" data-testid="ack-repeat" /> I checked
          the table and I want to import this file again</label
        >
        <span v-if="submitted && errors.repeat" class="error" role="alert">{{
          errors.repeat
        }}</span>
      </div>
      <p
        v-if="upload.snapshot.value.deletePending"
        class="notice tone-warn"
        role="alert"
        data-testid="delete-pending"
      >
        <span aria-hidden="true">&#9650;</span>
        <span
          >The upload you cancelled could not be deleted on the server (the session ended or the
          server could not be reached). It still holds its room. Delete it from the Transfers
          panel.</span
        >
      </p>
      <p class="notice tone-warn" data-testid="all-or-nothing">
        <span aria-hidden="true">&#9650;</span>
        <span
          ><strong>All or nothing.</strong> The file is loaded in one transaction. If a row is
          wrong, nothing is stored, and the error says which line and column. A file larger than
          {{ formatBytes(cap) }} cannot be imported in one piece: split it. The rows are locked
          until the end, so a large import slows writers to the table.</span
        >
      </p>
      <p v-if="file" class="muted">
        <SafeName :value="file.name" :max="100" /> ({{ formatBytes(file.size) }})
      </p>
      <div v-if="file && guess && !guess.compressed && !guess.empty">
        <CsvPreview
          :bytes="bytes"
          :encoding="form.encoding"
          :delimiter="form.delimiter"
          :quote="form.quote"
          :partial="guess.partial"
          :header="headerIsNames"
        />
      </div>
      <div class="grid-2">
        <FormField
          v-slot="slot"
          label="First line"
          hint="A file made by Export CSV with spreadsheet-safe values: choose to ignore the names (an apostrophe may stand before one)."
          :error="submitted ? errors.header : undefined"
        >
          <select
            v-bind="slot"
            v-model="form.header"
            data-testid="import-header"
            @change="headerPending = false"
          >
            <option v-if="headerPending" value="" disabled>Choose ...</option>
            <option value="skip">Has column names; ignore them</option>
            <option value="match">Has column names; they must match the columns</option>
            <option value="none">Has no column names</option>
          </select>
        </FormField>
        <FormField v-slot="slot" label="Delimiter">
          <select v-bind="slot" v-model="form.delimiter" data-testid="import-delimiter">
            <option v-for="(label, d) in DELIMITER_LABEL" :key="d" :value="d as CsvDelimiter">
              {{ label }}
            </option>
          </select>
        </FormField>
        <FormField v-slot="slot" label="Encoding">
          <select v-bind="slot" v-model="form.encoding" data-testid="import-encoding">
            <option v-for="(label, e) in ENCODING_LABEL" :key="e" :value="e as CsvEncoding">
              {{ label }}
            </option>
          </select>
        </FormField>
        <FormField
          v-slot="slot"
          label="Quote character"
          :error="submitted ? errors.quote : undefined"
        >
          <input
            v-bind="slot"
            v-model="form.quote"
            class="input mono"
            maxlength="4"
            autocomplete="off"
          />
        </FormField>
        <FormField
          v-slot="slot"
          label="Text of a null"
          :hint="`${utf8Length(form.nullString)} of 32 bytes. Empty: an empty field with no quotes is a null.`"
          :error="submitted ? errors.nullString : undefined"
        >
          <input v-bind="slot" v-model="form.nullString" class="input mono" autocomplete="off" />
        </FormField>
      </div>
      <label v-if="form.encoding === 'UTF8'" class="check"
        ><input v-model="form.skipBom" type="checkbox" /> Remove a byte order mark at the start of
        the file (spreadsheets write one)</label
      >
      <label class="check"
        ><input v-model="form.keepUpload" type="checkbox" /> Keep the uploaded file after a
        successful import, until its retention time ends</label
      >

      <fieldset>
        <legend>Columns</legend>
        <label class="check"
          ><input v-model="chooseColumns" type="radio" :value="false" /> The columns of the table,
          in table order (a generated column is skipped)</label
        >
        <label class="check"
          ><input v-model="chooseColumns" type="radio" :value="true" /> The columns of the file, in
          the order they appear in it</label
        >
        <QueryBoundary
          v-if="chooseColumns"
          :loading="detail.isPending.value"
          :error="detail.error.value"
          @retry="detail.refetch()"
        >
          <p class="hint muted">
            Click the columns in the order they appear in the file. A file made by Export CSV also
            holds the generated columns: leave those out.
          </p>
          <div
            class="stack"
            style="gap: var(--s-1); max-height: 12rem; overflow: auto"
            tabindex="0"
            role="group"
            aria-label="Columns of the file"
          >
            <label v-for="c in allColumns" :key="c" class="check"
              ><input v-model="picked" type="checkbox" :value="c" />
              <code><SafeName :value="c" :max="80" /></code
            ></label>
          </div>
          <p class="muted" data-testid="picked-order">
            Order in the file:
            <template v-if="picked.length"
              ><span v-for="(c, i) in picked" :key="c"
                ><template v-if="i > 0">, </template
                ><SafeName :value="c" :max="40" /></span></template
            ><template v-else>none yet</template>
            <button v-if="picked.length" type="button" class="btn btn-small" @click="picked = []">
              Clear
            </button>
          </p>
          <span v-if="submitted && errors.columns" class="error" role="alert">{{
            errors.columns
          }}</span>
        </QueryBoundary>
      </fieldset>
      <details v-if="allColumns.length">
        <summary>
          Columns where an empty field is an empty text, or a quoted null text is a null
        </summary>
        <fieldset>
          <legend>An empty unquoted field is an empty text (not a null) in</legend>
          <label v-for="c in allColumns" :key="`nn-${c}`" class="check"
            ><input v-model="form.forceNotNull" type="checkbox" :value="c" />
            <code><SafeName :value="c" :max="80" /></code
          ></label>
        </fieldset>
        <fieldset>
          <legend>A quoted null text is a null in</legend>
          <label v-for="c in allColumns" :key="`fn-${c}`" class="check"
            ><input v-model="form.forceNull" type="checkbox" :value="c" />
            <code><SafeName :value="c" :max="80" /></code
          ></label>
        </fieldset>
      </details>
    </form>

    <!-- Step 3 -->
    <div v-if="step === 'send'" class="stack">
      <UploadStatus
        :snapshot="upload.snapshot.value"
        :percent="upload.percent.value"
        :signing-in="signingIn"
        :sign-in-error="signInError"
        @pause="upload.pause()"
        @resume="upload.resume()"
        @cancel="cancelUpload"
        @retry="step = 'options'"
        @sign-in="signInAgain"
      />
    </div>

    <!-- Step 4 -->
    <div v-if="step === 'import'" class="stack" data-testid="import-step">
      <p v-if="importError" class="stack">
        <ProblemNotice :error="importError" />
      </p>
      <TaskProgress v-else :task="task.task.value" :poll-error="task.pollError.value" />
      <p
        v-if="task.state.value === 'done'"
        class="notice tone-ok"
        role="status"
        data-testid="import-done"
      >
        <span aria-hidden="true">&#10003;</span>
        <span
          >The rows were added in one transaction. The row count of the table in the list is an
          estimate and catches up within a few minutes.</span
        >
      </p>
      <p
        v-if="outcomeUnknown"
        class="notice tone-danger"
        role="alert"
        data-testid="outcome-unknown"
      >
        <span aria-hidden="true">&#10006;</span>
        <span
          >Check the table before you repeat the import. The commit was sent and the server could
          not see whether it was stored, so some of the rows may have been loaded. Repeating the
          import could load them twice (a table with a unique key answers with the key's own
          error).</span
        >
      </p>
      <p v-else-if="importFailed" class="notice tone-info" data-testid="import-failed-note">
        <span aria-hidden="true">&#9432;</span>
        <span
          >Nothing was stored. The file is still on the server: you can change the options and try
          again without sending it again.</span
        >
      </p>
    </div>

    <template #footer>
      <template v-if="step === 'file'">
        <button type="button" class="btn" @click="close">Cancel</button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="!file || refused !== null || (guess?.empty ?? false) || off"
          data-testid="to-options"
          @click="toOptions"
        >
          Next: options
        </button>
      </template>
      <template v-else-if="step === 'options'">
        <button type="button" class="btn" @click="step = 'file'">Back</button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="off"
          data-testid="start-import"
          @click="startSending"
        >
          {{ completeUpload ? 'Import again' : 'Upload and import' }}
        </button>
      </template>
      <template v-else-if="step === 'send'">
        <button type="button" class="btn" @click="close">
          {{ upload.running.value ? 'Close (stops the upload)' : 'Close' }}
        </button>
      </template>
      <template v-else>
        <button
          v-if="(importFailed || importError) && !outcomeUnknown"
          type="button"
          class="btn"
          data-testid="change-options"
          @click="backToOptions"
        >
          Change the options
        </button>
        <button type="button" class="btn btn-primary" @click="close">
          {{ task.isActive.value ? 'Hide (keeps running)' : 'Close' }}
        </button>
      </template>
    </template>
  </AppDialog>
</template>
