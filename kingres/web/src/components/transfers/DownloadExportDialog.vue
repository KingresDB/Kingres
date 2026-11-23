<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The download of one staged export file, from the Transfers panel: what the file is, its size and SHA-256 (to compare
// with the file that was saved), and the download itself.
import AppDialog from '@/components/AppDialog.vue'
import SafeName from '@/components/SafeName.vue'
import ExportDownload from './ExportDownload.vue'
import type { DataExport } from '@/api/types'
import { formatBytes, formatDateTime, formatNumber } from '@/lib/format'

defineProps<{ file: DataExport }>()
const emit = defineEmits<{ close: [] }>()
</script>

<template>
  <AppDialog title="Download an export file" @close="emit('close')">
    <dl class="kv" data-testid="download-facts">
      <dt>Table</dt>
      <dd class="mono">
        <SafeName
          :value="`${file.database ?? ''}.${file.schema ?? ''}.${file.table ?? ''}`"
          :max="160"
        />
      </dd>
      <dt>Size</dt>
      <dd>{{ formatBytes(file.sizeBytes) }}</dd>
      <dt>Rows</dt>
      <dd>{{ formatNumber(file.rows) }}</dd>
      <dt>SHA-256</dt>
      <dd>
        <code class="mono wrap" data-testid="download-sha"
          ><SafeName :value="file.sha256 ?? '-'" kind="text" :max="64"
        /></code>
      </dd>
      <dt>Kept until</dt>
      <dd>{{ formatDateTime(file.expiresAt) }}</dd>
    </dl>
    <p
      v-if="file.spreadsheetSafe === false"
      class="notice tone-warn"
      role="alert"
      data-testid="download-spreadsheet-off"
    >
      <span aria-hidden="true">&#9650;</span>
      <span
        >This file was written with the spreadsheet guard <strong>off</strong>: its values are
        exact. Do not open it in a spreadsheet; a value that begins with = + - @ can run as a
        formula there.</span
      >
    </p>
    <p
      v-else-if="file.spreadsheetSafe === true"
      class="muted"
      data-testid="download-spreadsheet-on"
    >
      This file was written with the spreadsheet guard: text values that begin with a formula
      character have an apostrophe in front (importing it puts the apostrophe in). The guard changes
      text columns only; a json or array value is not changed, and it is not a security feature.
    </p>
    <p v-else class="muted" data-testid="download-spreadsheet-note">
      If this file was exported with the spreadsheet guard off, or holds json or array values, do
      not open it in a spreadsheet: a value that begins with = + - @ can run as a formula there. The
      guard changes text columns only and is not a security feature. This list does not say which
      option the export had.
    </p>
    <ExportDownload :export="file" />
    <template #footer>
      <button type="button" class="btn btn-primary" @click="emit('close')">Close</button>
    </template>
  </AppDialog>
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
  min-width: 0;
}
</style>
