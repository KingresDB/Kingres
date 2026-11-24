<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The result of a finished export task: the file's size, rows, SHA-256, expiry and what it guarantees (the consistency label
// of the table), the sentence about exporting twice, and the download. The id of the file comes from the task's detail
// (`transfer <id>`, the convention of the contract), and everything else from `getDataExport`.
import { computed } from 'vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import SafeName from '@/components/SafeName.vue'
import ExportDownload from './ExportDownload.vue'
import { useDataExport } from '@/api/queries'
import type { Task } from '@/api/types'
import { formatBytes, formatDateTime, formatNumber } from '@/lib/format'
import { consistencySentence, formatLabel } from '@/lib/transfer'

const props = defineProps<{ task: Task; spreadsheetSafe: boolean }>()

/** The id of the export file: only what the contract says the detail is, and nothing else is trusted as an id. */
const exportId = computed(() => /^transfer (\d{1,20})$/.exec(props.task.detail ?? '')?.[1] ?? null)
const file = useDataExport(exportId)
</script>

<template>
  <div class="stack" data-testid="export-result">
    <p v-if="exportId === null" class="notice tone-warn" role="alert" data-testid="export-no-id">
      <span aria-hidden="true">&#9650;</span>
      <span
        >The export is done, but the server did not say which file it made. Find it in the Transfers
        panel of the databases page.</span
      >
    </p>
    <QueryBoundary
      v-else
      :loading="file.isPending.value"
      :error="file.error.value"
      @retry="file.refetch()"
    >
      <template v-if="file.data.value">
        <dl class="kv" data-testid="export-facts">
          <dt>Table</dt>
          <dd class="mono">
            <SafeName
              :value="`${file.data.value.schema ?? ''}.${file.data.value.table ?? ''}`"
              :max="160"
            />
          </dd>
          <dt>Format</dt>
          <dd>{{ formatLabel(file.data.value.format) }}</dd>
          <dt>Size</dt>
          <dd>{{ formatBytes(file.data.value.sizeBytes) }}</dd>
          <dt>Rows</dt>
          <dd>{{ formatNumber(file.data.value.rows) }}</dd>
          <dt>SHA-256</dt>
          <dd>
            <code class="mono wrap" data-testid="export-sha"
              ><SafeName :value="file.data.value.sha256 ?? '-'" kind="text" :max="64"
            /></code>
          </dd>
          <dt>Kept until</dt>
          <dd>{{ formatDateTime(file.data.value.expiresAt) }}</dd>
        </dl>
        <div
          v-for="c in file.data.value.consistency ?? []"
          :key="c.table"
          class="notice"
          :class="c.level === 'single-snapshot' ? 'tone-info' : 'tone-warn'"
          data-testid="export-consistency"
          :data-level="c.level"
        >
          <span aria-hidden="true">{{ c.level === 'single-snapshot' ? 'ⓘ' : '▲' }}</span>
          <span>{{ consistencySentence(c) }}</span>
        </div>
        <p
          v-if="file.data.value.spreadsheetSafe ?? props.spreadsheetSafe"
          class="muted"
          data-testid="export-lossy"
        >
          Text values that begin with a formula character were changed with an apostrophe in front
          (a header cell too). Importing this file puts the apostrophe into the table; import it
          with the first line ignored. The guard looks at the type of a column: a json or an array
          value that begins with such a character is not changed, so it is a convenience and not a
          security feature.
        </p>
        <p v-else class="notice tone-warn" data-testid="export-not-for-spreadsheet">
          <span aria-hidden="true">&#9650;</span>
          <span
            >This file holds the values exactly. Do not open it in a spreadsheet: a value that
            begins with = + - @ can run as a formula there.</span
          >
        </p>
        <p class="muted" data-testid="export-twice">
          To see whether the data changed while it was read, export the table a second time and
          compare the two SHA-256 values: if they are equal, nothing changed during either run.
        </p>
        <ExportDownload :export="file.data.value" />
      </template>
    </QueryBoundary>
  </div>
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
