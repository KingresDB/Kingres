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
