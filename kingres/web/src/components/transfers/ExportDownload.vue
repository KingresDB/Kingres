<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The download of one export file. It saves to a file the person chooses when the browser can (the bytes are written
// as they arrive, so the size is not limited by memory), from memory for a small file, and by the browser's own
// download for a large file in a browser that cannot (then the person checks the SHA-256 themselves). When the network
// breaks it resumes with `Range`; when the file is short or changed it says so and saves nothing; when the session ends it
// asks for the password and goes on from the bytes it kept. The size and the SHA-256 are shown here.
import { computed, ref, watch } from 'vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import { API_BASE_PATH } from '@/api/client'
import type { DataExport } from '@/api/types'
import { useAnnounce } from '@/composables/useAnnounce'
import { useExportDownload, type DownloadHooks } from '@/composables/useExportDownload'
import { useSessionStore } from '@/stores/session'
import { formatBytes } from '@/lib/format'
import { describeDownloadFailure } from '@/lib/transfer'

const props = defineProps<{ export: DataExport; hooks?: DownloadHooks }>()
const dl = useExportDownload(() => props.export, props.hooks)
const session = useSessionStore()
const announce = useAnnounce()

const size = computed(() => props.export.sizeBytes ?? 0)
const percent = computed(() =>
  size.value > 0 ? Math.floor((dl.received.value / size.value) * 100) : 0,
)
const what = computed(() =>
  props.export.kind === 'databaseExport'
    ? `${props.export.database ?? 'database'} archive`
    : `${props.export.table ?? 'export'} file`,
)
/** The text of the button; its accessible name starts with it (so that the visible text is part of the name). */
const startText = computed(
  () =>
    `${dl.state.value === 'failed' ? 'Try again' : 'Download file'} (${formatBytes(size.value)})`,
)
const ready = computed(() => props.export.state === 'ready' && props.export.sizeBytes !== null)
/** The address of the file for the browser's own download. It is built from the id the server gave and nothing else. */
const href = computed(() =>
  /^\d{1,20}$/.test(props.export.exportId)
    ? `${API_BASE_PATH}/data-exports/${props.export.exportId}/content`
    : null,
)
const mayLink = computed(() => session.can('downloadDataExport'))
const failure = computed(() => {
  const o = dl.outcome.value
  return o && o.kind === 'failed' ? describeDownloadFailure(o.reason, o.received, size.value) : null
})
const needsPassword = computed(() => {
  const o = dl.outcome.value
  return o?.kind === 'failed' && o.reason === 'session' && dl.canResume.value
})
const success = computed(() => {
  const o = dl.outcome.value
  if (!o || o.kind !== 'complete') return null
  const resumed =
    o.resumes > 0 ? ` after ${o.resumes} ${o.resumes === 1 ? 'resume' : 'resumes'}` : ''
  const check =
    o.verified === true
      ? 'Its SHA-256 matches the export.'
      : 'Its SHA-256 was not checked here (compare it with the value above).'
  return `${formatBytes(o.bytes)} saved${resumed}. ${check}`
})

// The progress is read out every ten percent, as the progress of a task is.
watch(
  () => Math.floor(percent.value / 10),
  (tenth) => {
    if (dl.state.value === 'running' && tenth > 0) announce(`Download ${tenth * 10} percent.`)
  },
)

const password = ref('')
const signingIn = ref(false)
const signInError = ref<unknown>(null)
async function signInAgain(): Promise<void> {
  if (password.value === '') return
  signingIn.value = true
  signInError.value = null
  try {
    await session.signIn(session.username ?? '', password.value)
    password.value = ''
    await dl.resume()
  } catch (error) {
    signInError.value = error
  } finally {
    signingIn.value = false
  }
}
</script>

<template>
  <div class="stack" data-testid="export-download">
    <p v-if="!ready" class="muted" data-testid="download-unavailable">
      This file cannot be downloaded: it is not ready.
    </p>
    <template v-else-if="dl.method.value === 'link'">
      <p class="muted" data-testid="download-link-note">
        This file is larger than this browser can keep in memory, and the browser cannot save it as
        it arrives. The browser's own download is used. Nothing here checks the file, so check it
        yourself: its size must be {{ formatBytes(size) }} ({{ size }} bytes) and its SHA-256 must
        be
        <code class="mono wrap" data-testid="link-sha"
          ><SafeName :value="props.export.sha256 ?? '-'" kind="text" :max="64"
        /></code>
        . Compute it with <code>sha256sum file</code> (Linux),
        <code>shasum -a 256 file</code> (macOS) or
        <code>certutil -hashfile file SHA256</code> (Windows). If the download breaks, start it
        again.
      </p>
      <a
        v-if="href && mayLink"
        class="btn btn-primary"
        :href="href"
        download
        data-testid="download-link"
        >Download file ({{ formatBytes(size) }})</a
      >
      <p v-else-if="!mayLink" class="muted" data-testid="download-link-role">
        Downloading needs the kingres_admin cluster role.
      </p>
    </template>
    <template v-else>
      <div class="row">
        <GateButton
          v-if="dl.state.value !== 'running' && !needsPassword"
          op="downloadDataExport"
          variant="primary"
          data-testid="download-start"
          :aria-label="`${startText} of the ${what}`"
          @click="dl.start()"
        >
          {{ startText }}
        </GateButton>
        <button
          v-else-if="dl.state.value === 'running'"
          type="button"
          class="btn"
          data-testid="download-cancel"
          @click="dl.cancel()"
        >
          Cancel the download
        </button>
      </div>
      <div v-if="dl.state.value === 'running'" class="stack" style="gap: var(--s-1)">
        <div
          class="progress"
          role="progressbar"
          aria-label="Download progress"
          aria-valuemin="0"
          aria-valuemax="100"
          :aria-valuenow="percent"
        >
          <span :style="{ width: `${percent}%` }" />
        </div>
        <span class="muted num" data-testid="download-progress"
          >{{ formatBytes(dl.received.value) }} of {{ formatBytes(size) }}</span
        >
      </div>
      <p v-if="success" class="notice tone-ok" role="status" data-testid="download-done">
        <span aria-hidden="true">&#10003;</span> <span>{{ success }}</span>
      </p>
      <p v-if="failure" class="notice tone-danger" role="alert" data-testid="download-failed">
        <span aria-hidden="true">&#10006;</span> <span>{{ failure }}</span>
      </p>
      <form
        v-if="needsPassword"
        class="stack"
        data-testid="download-password"
        @submit.prevent="signInAgain"
      >
        <label for="download-password-input">Password</label>
        <input
          id="download-password-input"
          v-model="password"
          class="input"
          type="password"
          autocomplete="current-password"
          data-autofocus
        />
        <ProblemNotice v-if="signInError" :error="signInError" />
        <div class="row">
          <button type="submit" class="btn btn-primary" :disabled="signingIn || password === ''">
            Sign in and continue the download
          </button>
        </div>
      </form>
    </template>
  </div>
</template>
