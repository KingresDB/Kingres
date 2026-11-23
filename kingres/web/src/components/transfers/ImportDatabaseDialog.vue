<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Import a database from an archive (contract 0.41.0, a placeholder revision): choose the file, send it with the resumable
// uploader (purpose `databaseArchive`), validate it (the report page), choose the options and the name of the new database,
// and follow the task `database.import`. The archive is hostile input and the server reads it again at import; this dialog
// only helps a person see what the import will do. The text of a statement of an archive is never shown: a refused
// statement is a kind, a position and a fingerprint. The statements run as the signed-in role, so a dangerous role is told
// to sign in as another administrator.
import { computed, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { useQuery } from '@tanstack/vue-query'
import AppDialog from '@/components/AppDialog.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import SafeName from '@/components/SafeName.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import ArchiveReportView from './ArchiveReport.vue'
import UploadStatus from './UploadStatus.vue'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { keys, useClusterSettings, useDatabases } from '@/api/queries'
import { useSessionStore } from '@/stores/session'
import { useTaskAction } from '@/composables/useTask'
import { useUpload } from '@/composables/useUpload'
import {
  checksumError,
  databaseNameError,
  DANGEROUS_ROLE_TEXT,
  importDatabaseRequest,
  importResultLines,
  isZipName,
  LEDGER_NEW_CHAIN_TEXT,
  POLICIES_DISABLED_TEXT,
  privateDatabaseSentence,
  suggestDatabaseName,
} from '@/lib/archive'
import { formatBytes } from '@/lib/format'
import { isAddressableSegment } from '@/lib/path-segment'
import { findStoredUpload, fingerprintFile } from '@/lib/upload-store'

const emit = defineEmits<{ close: [] }>()

type Step = 'file' | 'send' | 'validate' | 'import'
const step = ref<Step>('file')
const session = useSessionStore()
const settings = useClusterSettings()
const databases = useDatabases()
const off = computed(() => settings.data.value?.dataTransfer?.enabled === false)
const maxFileBytes = computed(() => settings.data.value?.dataTransfer?.maxFileBytes ?? null)

const file = ref<File | null>(null)
const fingerprint = ref<string | null>(null)
/** An archive has no table to belong to: the tab remembers it by an empty target. */
const target = { database: '', schema: '', table: '' }
const upload = useUpload(target)
const uploadId = ref<string | null>(null)
const signingIn = ref(false)
const signInError = ref<unknown>(null)

const refusal = computed(() => {
  const f = file.value
  if (!f) return null
  if (f.size === 0) return 'The file is empty.'
  if (maxFileBytes.value !== null && f.size > maxFileBytes.value)
    return `The file is larger than the ${formatBytes(maxFileBytes.value)} this cluster stages.`
  return null
})
const remembered = computed(() =>
  file.value
    ? findStoredUpload(file.value, target, session.username ?? '', fingerprint.value)
    : null,
)

// The form of step 3.
const name = ref('')
const nameTouched = ref(false)
const expectedSha256 = ref('')
const acknowledge = ref(false)
const onForeignKeyViolation = ref<'fail' | 'keepNotValid'>('fail')
const keepOnFailure = ref(false)

function choose(event: Event): void {
  const f = (event.target as HTMLInputElement).files?.[0]
  if (!f) {
    file.value = null
    return
  }
  file.value = f
  uploadId.value = null
  fingerprint.value = null
  if (!nameTouched.value) name.value = suggestDatabaseName(f.name)
  void fingerprintFile(f).then((p) => {
    if (file.value === f) fingerprint.value = p
  })
}

const validation = useTaskAction<void>(
  () => endpoints.validateDatabaseArchive(uploadId.value as string),
  { invalidate: () => [keys.dataExports], label: () => 'Validate a database archive' },
)
const uploadQuery = useQuery({
  queryKey: computed(() => ['dataUpload', uploadId.value ?? ''] as const),
  queryFn: () => endpoints.getDataUpload(uploadId.value as string),
  enabled: computed(() => validation.state.value === 'done' && uploadId.value !== null),
})
const report = computed(() => uploadQuery.data.value?.report ?? null)

const request = computed(() =>
  importDatabaseRequest({
    uploadId: uploadId.value ?? '',
    database: name.value,
    expectedSha256: expectedSha256.value,
    acknowledgeLedgerRestart: acknowledge.value,
    onForeignKeyViolation: onForeignKeyViolation.value,
    keepOnFailure: keepOnFailure.value,
  }),
)
const importTask = useTaskAction<void>(() => endpoints.importDatabase(request.value), {
  invalidate: () => [keys.databases, keys.dataExports],
  label: () => `Import database ${name.value}`,
})

async function startSending(): Promise<void> {
  const f = file.value
  if (f === null || refusal.value !== null) return
  step.value = 'send'
  await upload.start(f, {
    existing: remembered.value?.uploadId,
    purpose: 'databaseArchive',
    maxFileBytes: maxFileBytes.value,
    fingerprint: fingerprint.value,
  })
}

async function startValidation(): Promise<void> {
  if (uploadId.value === null) return
  step.value = 'validate'
  validation.reset()
  await validation.run()
}

watch(
  () => upload.snapshot.value.phase,
  (phase) => {
    if (phase === 'complete' && step.value === 'send' && upload.snapshot.value.uploadId) {
      uploadId.value = upload.snapshot.value.uploadId
      void startValidation()
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
  step.value = 'file'
}

// What blocks the confirmation, in the order a person fixes it.
const existing = computed(() => (databases.data.value?.items ?? []).map((d) => d.name))
const nameError = computed(() => databaseNameError(name.value, existing.value))
const shaError = computed(() => checksumError(expectedSha256.value, report.value?.sha256))
const refusedCount = computed(() => report.value?.statements?.refused?.length ?? 0)
const needsLedgerAck = computed(() => report.value?.needs?.ledgerRestart === true)
const blockers = computed(() => {
  const out: string[] = []
  if (off.value) out.push('Data transfer is turned off for this cluster (Settings, Data transfer).')
  if (report.value === null) out.push('The archive has not been validated.')
  else if (!report.value.valid) out.push('The archive is not valid, so it cannot be imported.')
  if (refusedCount.value > 0)
    out.push('The archive has statements that are not allowed; it cannot be imported.')
  if (nameError.value) out.push(nameError.value)
  if (shaError.value) out.push(shaError.value)
  if (needsLedgerAck.value && !acknowledge.value)
    out.push('Accept that the ledger tables start a new chain.')
  return out
})

async function startImport(): Promise<void> {
  if (blockers.value.length > 0 || uploadId.value === null) return
  step.value = 'import'
  importTask.reset()
  await importTask.run()
}

const dangerous = computed(
  () =>
    importTask.error.value instanceof ApiError &&
    importTask.error.value.code === 'forbidden' &&
    step.value === 'import',
)
const reasons = computed(() => {
  const e = importTask.error.value
  return e instanceof ApiError ? (e.problem.reasons ?? []) : []
})
const resultLines = computed(() => importResultLines(importTask.task.value?.detail))
const failedStep = computed(() => importTask.task.value?.error?.step ?? null)
const finalName = computed(() => name.value)
const busy = computed(() => validation.isStarting.value || importTask.isStarting.value)
const importing = computed(() => importTask.isActive.value)
</script>

<template>
  <AppDialog
    title="Import a database from an archive"
    wide
    :persistent="busy"
    @close="emit('close')"
  >
    <div v-show="step === 'file'" class="stack">
      <p v-if="off" class="notice tone-info" role="status" data-testid="import-off">
        <span aria-hidden="true">&#9432;</span>
        <span>Data transfer is turned off for this cluster (Settings, Data transfer).</span>
      </p>
      <p>
        Choose an archive made by Export database. It is sent to the server, validated, and loaded
        into a <strong>new</strong> database. The import works in a private database and gives it
        its name as the last step; if anything fails, the private database is removed. The
        statements of the archive run as <strong>your role</strong>, so you need the privileges to
        create the objects, and a role that is a superuser or reads server files is refused.
      </p>
      <label class="field">
        <span>Archive file</span>
        <input
          type="file"
          accept=".zip,application/zip"
          class="input"
          data-testid="import-file"
          @change="choose"
        />
      </label>
      <p v-if="file" class="muted" data-testid="import-chosen">
        <SafeName :value="file.name" :max="100" /> ({{ formatBytes(file.size) }})
      </p>
      <p v-if="file && !isZipName(file.name)" class="notice tone-warn" data-testid="import-not-zip">
        <span aria-hidden="true">&#9650;</span>
        <span
          >The name of the file does not end in .zip. The server reads it anyway and says what it
          is.</span
        >
      </p>
      <p v-if="refusal" class="notice tone-danger" role="alert" data-testid="import-refused">
        <span aria-hidden="true">&#10006;</span>
        <span>{{ refusal }}</span>
      </p>
      <p v-if="remembered && !refusal" class="notice tone-info" data-testid="import-remembered">
        <span aria-hidden="true">&#9432;</span>
        <span
          >This tab started to upload this file earlier. The upload is continued from where the
          server has it.</span
        >
      </p>
    </div>

    <div v-if="step === 'send'" class="stack">
      <UploadStatus
        :snapshot="upload.snapshot.value"
        :percent="upload.percent.value"
        :signing-in="signingIn"
        :sign-in-error="signInError"
        @pause="upload.pause()"
        @resume="upload.resume()"
        @cancel="cancelUpload"
        @retry="step = 'file'"
        @sign-in="signInAgain"
      />
    </div>

    <div v-if="step === 'validate'" class="stack" data-testid="import-validate-step">
      <ProblemNotice v-if="validation.error.value" :error="validation.error.value" />
      <TaskProgress v-else :task="validation.task.value" :poll-error="validation.pollError.value" />
      <QueryBoundary
        v-if="validation.state.value === 'done'"
        :loading="uploadQuery.isPending.value"
        :error="uploadQuery.error.value"
        @retry="uploadQuery.refetch()"
      >
        <template v-if="report">
          <ArchiveReportView :report="report" :show-import-hint="false" />

          <form
            v-if="report.valid && refusedCount === 0"
            class="stack"
            data-testid="import-options"
            @submit.prevent="startImport"
          >
            <h3>The new database</h3>
            <label class="field">
              <span>Name of the new database</span>
              <input
                v-model="name"
                class="input mono"
                data-testid="import-name"
                autocomplete="off"
                spellcheck="false"
                :aria-invalid="nameError ? 'true' : undefined"
                aria-describedby="import-name-error"
                @input="nameTouched = true"
              />
              <span
                id="import-name-error"
                class="error"
                role="alert"
                data-testid="import-name-error"
                >{{ nameError ?? '' }}</span
              >
            </label>
            <label class="field">
              <span>SHA-256 of the archive (optional)</span>
              <input
                v-model="expectedSha256"
                class="input mono"
                data-testid="import-sha"
                autocomplete="off"
                spellcheck="false"
                :aria-invalid="shaError ? 'true' : undefined"
                aria-describedby="import-sha-help"
              />
              <span id="import-sha-help" class="muted"
                >Compare it with the value the export showed or an operator gave you. A different
                file is refused. The archive that was sent has
                <code class="wrap"
                  ><SafeName :value="report.sha256 ?? '-'" kind="text" :max="64" /></code
                >.</span
              >
              <span v-if="shaError" class="error" role="alert" data-testid="import-sha-error">{{
                shaError
              }}</span>
            </label>

            <div v-if="needsLedgerAck" class="notice tone-warn" data-testid="import-ledger">
              <span aria-hidden="true">&#9650;</span>
              <div class="stack" style="gap: var(--s-1)">
                <span>{{ LEDGER_NEW_CHAIN_TEXT }}</span>
                <label class="check">
                  <input v-model="acknowledge" type="checkbox" data-testid="import-ack" />
                  <span>I understand that the ledger tables start a new chain</span>
                </label>
              </div>
            </div>
            <p class="muted" data-testid="import-policies">{{ POLICIES_DISABLED_TEXT }}</p>

            <fieldset class="field">
              <legend>A foreign key that the loaded rows violate</legend>
              <label class="check">
                <input
                  v-model="onForeignKeyViolation"
                  type="radio"
                  value="fail"
                  name="fk-mode"
                  data-testid="import-fk-fail"
                />
                <span>Fail the import and remove the database</span>
              </label>
              <label class="check">
                <input
                  v-model="onForeignKeyViolation"
                  type="radio"
                  value="keepNotValid"
                  name="fk-mode"
                  data-testid="import-fk-keep"
                />
                <span
                  >Keep the constraint as NOT VALID, keep the database and report the
                  violations</span
                >
              </label>
            </fieldset>
            <label class="check">
              <input v-model="keepOnFailure" type="checkbox" data-testid="import-keep" />
              <span>Keep the private database if the import fails, to look at it</span>
            </label>

            <ul
              v-if="blockers.length"
              class="stack"
              style="gap: var(--s-1)"
              data-testid="import-blockers"
            >
              <li v-for="b in blockers" :key="b">{{ b }}</li>
            </ul>
          </form>
          <p v-else class="notice tone-danger" role="alert" data-testid="import-cannot">
            <span aria-hidden="true">&#10006;</span>
            <span>This archive cannot be imported. The reasons are above.</span>
          </p>
        </template>
        <p v-else class="notice tone-warn" role="alert" data-testid="import-no-report">
          <span aria-hidden="true">&#9650;</span>
          <span>The validation is done, but the server did not give a report.</span>
        </p>
      </QueryBoundary>
    </div>

    <div v-if="step === 'import'" class="stack" data-testid="import-step">
      <template v-if="importTask.error.value">
        <div
          v-if="dangerous"
          class="notice tone-danger"
          role="alert"
          data-testid="import-dangerous"
        >
          <span aria-hidden="true">&#10006;</span>
          <div class="stack" style="gap: var(--s-1)">
            <strong>Your role cannot import a database</strong>
            <span>{{ DANGEROUS_ROLE_TEXT }}</span>
          </div>
        </div>
        <ProblemNotice v-else :error="importTask.error.value" />
        <ul
          v-if="reasons.length"
          class="stack"
          style="gap: var(--s-1)"
          data-testid="import-reasons"
        >
          <li v-for="(r, i) in reasons" :key="i">
            <code><SafeName :value="r.object" :max="80" /></code>:
            <SafeName :value="r.message" kind="text" :max="240" />
          </li>
        </ul>
      </template>
      <TaskProgress v-else :task="importTask.task.value" :poll-error="importTask.pollError.value" />
      <div
        v-if="importTask.state.value === 'failed'"
        class="notice tone-danger"
        role="alert"
        data-testid="import-failed"
      >
        <span aria-hidden="true">&#10006;</span>
        <div class="stack" style="gap: var(--s-1)">
          <span v-if="failedStep"
            >The import failed at the step
            <strong><SafeName :value="failedStep" kind="text" :max="80" /></strong>.</span
          >
          <span>{{ privateDatabaseSentence(keepOnFailure) }}</span>
        </div>
      </div>
      <div
        v-if="importTask.state.value === 'done'"
        class="notice tone-ok"
        role="status"
        data-testid="import-done"
      >
        <span aria-hidden="true">&#10003;</span>
        <div class="stack" style="gap: var(--s-1)">
          <span
            >The database
            <RouterLink
              v-if="isAddressableSegment(finalName)"
              :to="{ name: 'database', params: { name: finalName } }"
              data-testid="import-open"
              ><SafeName :value="finalName" /></RouterLink
            ><SafeName v-else :value="finalName" /> was imported.</span
          >
          <ul
            v-if="resultLines.length"
            class="stack"
            style="gap: var(--s-1)"
            data-testid="import-lines"
          >
            <li v-for="(l, i) in resultLines" :key="i">
              <SafeName :value="l" kind="text" :max="300" />
            </li>
          </ul>
        </div>
      </div>
    </div>

    <template #footer>
      <template v-if="step === 'file'">
        <button type="button" class="btn" @click="emit('close')">Cancel</button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="!file || refusal !== null || off"
          data-testid="import-start"
          @click="startSending"
        >
          Upload and validate
        </button>
      </template>
      <template v-else-if="step === 'send'">
        <button type="button" class="btn" @click="emit('close')">
          {{ upload.running.value ? 'Close (stops the upload)' : 'Close' }}
        </button>
      </template>
      <template v-else-if="step === 'validate'">
        <button type="button" class="btn" @click="emit('close')">
          {{ validation.isActive.value ? 'Hide (keeps running)' : 'Close' }}
        </button>
        <button
          v-if="report && report.valid && refusedCount === 0"
          type="button"
          class="btn btn-primary"
          :disabled="blockers.length > 0"
          data-testid="import-confirm"
          @click="startImport"
        >
          Import the database
        </button>
      </template>
      <template v-else>
        <button type="button" class="btn btn-primary" @click="emit('close')">
          {{ importing ? 'Hide (keeps running)' : 'Close' }}
        </button>
      </template>
    </template>
  </AppDialog>
</template>
