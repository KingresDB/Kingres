<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The download of one export file. It saves to a file the person chooses when the browser can (the bytes are written
// as they arrive, so the size is not limited by memory), from memory for a small file, and by the browser's own
// download for a large file in a browser that cannot. When the network breaks it resumes with `Range`; when the file is
// short or changed it says so and saves nothing. The size and the SHA-256 are shown by the caller, next to this.
import { computed } from 'vue'
import GateButton from '@/components/GateButton.vue'
import { API_BASE_PATH } from '@/api/client'
import type { DataExport } from '@/api/types'
import { useExportDownload, type DownloadHooks } from '@/composables/useExportDownload'
import { formatBytes } from '@/lib/format'
import { describeDownloadFailure } from '@/lib/transfer'

const props = defineProps<{ export: DataExport; hooks?: DownloadHooks }>()
const dl = useExportDownload(() => props.export, props.hooks)

const size = computed(() => props.export.sizeBytes ?? 0)
const percent = computed(() =>
  size.value > 0 ? Math.floor((dl.received.value / size.value) * 100) : 0,
)
const ready = computed(() => props.export.state === 'ready' && props.export.sizeBytes !== null)
/** The address of the file for the browser's own download. It is built from the id the server gave and nothing else. */
const href = computed(() =>
  /^\d{1,20}$/.test(props.export.exportId)
    ? `${API_BASE_PATH}/data-exports/${props.export.exportId}/content`
    : null,
)
const failure = computed(() => {
  const o = dl.outcome.value
  return o && o.kind === 'failed' ? describeDownloadFailure(o.reason, o.received, size.value) : null
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
</script>

<template>
  <div class="stack" data-testid="export-download">
    <p v-if="!ready" class="muted" data-testid="download-unavailable">
      This file cannot be downloaded: it is not ready.
    </p>
    <template v-else-if="dl.method.value === 'link'">
      <p class="muted" data-testid="download-link-note">
        This file is larger than this browser can keep in memory, and the browser cannot save it as
        it arrives. The browser's own download is used; if it breaks, start it again, and compare
        the SHA-256 of the saved file with the one shown.
      </p>
      <a v-if="href" class="btn btn-primary" :href="href" download data-testid="download-link"
        >Download file ({{ formatBytes(size) }})</a
      >
    </template>
    <template v-else>
      <div class="row">
        <GateButton
          v-if="dl.state.value !== 'running'"
          op="downloadDataExport"
          variant="primary"
          data-testid="download-start"
          @click="dl.start()"
        >
          {{ dl.state.value === 'failed' ? 'Try again' : 'Download file' }} ({{
            formatBytes(size)
          }})
        </GateButton>
        <button v-else type="button" class="btn" data-testid="download-cancel" @click="dl.cancel()">
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
    </template>
  </div>
</template>
