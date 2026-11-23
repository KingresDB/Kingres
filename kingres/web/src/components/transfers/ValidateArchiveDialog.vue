<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Validate a database archive (contract 0.40.0, a placeholder revision): choose the file, send it with the resumable uploader
// (purpose `databaseArchive`), follow the task `archive.validate`, and read the report. Nothing is imported and no database is
// touched. The dialog says so, and says that importing an archive is not available in this version.
import { computed, ref, watch } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import AppDialog from '@/components/AppDialog.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import SafeName from '@/components/SafeName.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import ArchiveReportView from './ArchiveReport.vue'
import UploadStatus from './UploadStatus.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useClusterSettings } from '@/api/queries'
import { useSessionStore } from '@/stores/session'
import { useTaskAction } from '@/composables/useTask'
import { useUpload } from '@/composables/useUpload'
import { IMPORT_NOT_AVAILABLE_TEXT, isZipName } from '@/lib/archive'
import { formatBytes } from '@/lib/format'
import { findStoredUpload, fingerprintFile } from '@/lib/upload-store'

const emit = defineEmits<{ close: [] }>()

type Step = 'file' | 'send' | 'validate'
const step = ref<Step>('file')
const session = useSessionStore()
const settings = useClusterSettings()
const off = computed(() => settings.data.value?.dataTransfer?.enabled === false)
const maxFileBytes = computed(() => settings.data.value?.dataTransfer?.maxFileBytes ?? null)

const file = ref<File | null>(null)
const fingerprint = ref<string | null>(null)
const readError = ref<string | null>(null)
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

function choose(event: Event): void {
  readError.value = null
  const f = (event.target as HTMLInputElement).files?.[0]
  if (!f) {
    file.value = null
    return
  }
  file.value = f
  uploadId.value = null
  fingerprint.value = null
  void fingerprintFile(f).then((p) => {
    if (file.value === f) fingerprint.value = p
  })
}

const task = useTaskAction<void>(
  () => endpoints.validateDatabaseArchive(uploadId.value as string),
  { invalidate: () => [keys.dataExports], label: () => 'Validate a database archive' },
)
const report = useQuery({
  queryKey: computed(() => ['dataUpload', uploadId.value ?? ''] as const),
  queryFn: () => endpoints.getDataUpload(uploadId.value as string),
  enabled: computed(() => task.state.value === 'done' && uploadId.value !== null),
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
  task.reset()
  await task.run()
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

const busy = computed(() => task.isStarting.value)
</script>

<template>
  <AppDialog title="Validate a database archive" wide :persistent="busy" @close="emit('close')">
    <div v-show="step === 'file'" class="stack">
      <p v-if="off" class="notice tone-info" role="status" data-testid="validate-off">
        <span aria-hidden="true">&#9432;</span>
        <span>Data transfer is turned off for this cluster (Settings, Data transfer).</span>
      </p>
      <p>
        Choose an archive made by Export database. The file is sent to the server and read once from
        end to end: the shape of the zip file, the checksums of every entry, the manifest and the
        catalog, the versions, and every statement of its SQL.
        <strong>No database is touched</strong> and nothing is imported.
        {{ IMPORT_NOT_AVAILABLE_TEXT }}
      </p>
      <label class="field">
        <span>Archive file</span>
        <input
          type="file"
          accept=".zip,application/zip"
          class="input"
          data-testid="archive-file"
          @change="choose"
        />
      </label>
      <p v-if="file" class="muted" data-testid="archive-chosen">
        <SafeName :value="file.name" :max="100" /> ({{ formatBytes(file.size) }})
      </p>
      <p
        v-if="file && !isZipName(file.name)"
        class="notice tone-warn"
        data-testid="archive-not-zip"
      >
        <span aria-hidden="true">&#9650;</span>
        <span
          >The name of the file does not end in .zip. The server reads it anyway and says what it
          is.</span
        >
      </p>
      <p v-if="refusal" class="notice tone-danger" role="alert" data-testid="archive-refused">
        <span aria-hidden="true">&#10006;</span>
        <span>{{ refusal }}</span>
      </p>
      <p v-if="readError" class="error" role="alert">{{ readError }}</p>
      <p v-if="remembered && !refusal" class="notice tone-info" data-testid="archive-remembered">
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

    <div v-if="step === 'validate'" class="stack" data-testid="validate-step">
      <ProblemNotice v-if="task.error.value" :error="task.error.value" />
      <TaskProgress v-else :task="task.task.value" :poll-error="task.pollError.value" />
      <QueryBoundary
        v-if="task.state.value === 'done'"
        :loading="report.isPending.value"
        :error="report.error.value"
        @retry="report.refetch()"
      >
        <ArchiveReportView v-if="report.data.value?.report" :report="report.data.value.report" />
        <p v-else class="notice tone-warn" role="alert" data-testid="validate-no-report">
          <span aria-hidden="true">&#9650;</span>
          <span>The validation is done, but the server did not give a report.</span>
        </p>
      </QueryBoundary>
    </div>

    <template #footer>
      <template v-if="step === 'file'">
        <button type="button" class="btn" @click="emit('close')">Cancel</button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="!file || refusal !== null || off"
          data-testid="start-validation"
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
      <template v-else>
        <button type="button" class="btn btn-primary" @click="emit('close')">
          {{ task.isActive.value ? 'Hide (keeps running)' : 'Close' }}
        </button>
      </template>
    </template>
  </AppDialog>
</template>
