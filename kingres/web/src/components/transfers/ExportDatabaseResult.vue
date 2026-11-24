<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The result of a finished database export: the archive's size, rows, SHA-256, expiry, how every table was read, the warnings
// of the manifest (what the archive does not carry), the sentence about comparing the SHA-256 with a value from another channel,
// and the download. The id of the file comes from the task's detail (`transfer <id>`) and everything else from `getDataExport`.
import { computed } from 'vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import SafeName from '@/components/SafeName.vue'
import ExportDownload from './ExportDownload.vue'
import { useDataExport } from '@/api/queries'
import type { Task } from '@/api/types'
import { formatBytes, formatDateTime, formatNumber } from '@/lib/format'
import { consistencySentence, formatLabel } from '@/lib/transfer'

const props = defineProps<{ task: Task }>()

const exportId = computed(() => /^transfer (\d{1,20})$/.exec(props.task.detail ?? '')?.[1] ?? null)
const file = useDataExport(exportId)
const levels = computed(() => file.data.value?.consistency ?? [])
/** Tables that were read per group: each of them is named, the others are summed up. */
const skewed = computed(() =>
  levels.value.filter(
    (c) => c.level !== 'archive-single-snapshot' && c.level !== 'single-snapshot',
  ),
)
const oneSnapshot = computed(
  () => levels.value.length > 0 && levels.value.every((c) => c.level === 'archive-single-snapshot'),
)
</script>

<template>
  <div class="stack" data-testid="export-db-result">
    <p v-if="exportId === null" class="notice tone-warn" role="alert" data-testid="export-db-no-id">
      <span aria-hidden="true">&#9650;</span>
      <span
        >The export is done, but the server did not say which file it made. Find it in the Transfers
        panel of this page.</span
      >
    </p>
    <QueryBoundary
      v-else
      :loading="file.isPending.value"
      :error="file.error.value"
      @retry="file.refetch()"
    >
      <template v-if="file.data.value">
        <dl class="kv" data-testid="export-db-facts">
          <dt>Database</dt>
          <dd class="mono"><SafeName :value="file.data.value.database ?? ''" :max="160" /></dd>
          <dt>Format</dt>
          <dd>{{ formatLabel(file.data.value.format) }}</dd>
          <dt>Size</dt>
          <dd>{{ formatBytes(file.data.value.sizeBytes) }}</dd>
          <dt>Tables</dt>
          <dd>{{ formatNumber(levels.length) }}</dd>
          <dt>Rows</dt>
          <dd>{{ formatNumber(file.data.value.rows) }}</dd>
          <dt>SHA-256</dt>
          <dd>
            <code class="mono wrap" data-testid="export-db-sha"
              ><SafeName :value="file.data.value.sha256 ?? '-'" kind="text" :max="64"
            /></code>
          </dd>
          <dt>Kept until</dt>
          <dd>{{ formatDateTime(file.data.value.expiresAt) }}</dd>
        </dl>
        <p
          v-if="oneSnapshot"
          class="notice tone-info"
          data-testid="export-db-consistency"
          data-level="archive-single-snapshot"
        >
          <span aria-hidden="true">ⓘ</span>
          <span
            >Every table was read in one transaction: the archive is one snapshot of the database.
            Every table is local, a reference table or on one group, so this archive needed no
            global snapshot.</span
          >
        </p>
        <div v-else class="stack" data-testid="export-db-consistency" data-level="per-table">
          <p class="notice tone-warn">
            <span aria-hidden="true">▲</span>
            <span
              >The tables were read one after another, so tables that refer to each other may have
              been read at different moments. {{ skewed.length }} of {{ levels.length }}
              {{ levels.length === 1 ? 'table was' : 'tables were' }} read per group. There is no
              global snapshot.</span
            >
          </p>
          <ul class="stack" style="gap: var(--s-1)">
            <li v-for="c in skewed" :key="c.table" data-testid="export-db-skewed-table">
              <code><SafeName :value="c.table" :max="100" /></code>:
              {{ consistencySentence(c) }}
            </li>
          </ul>
        </div>
        <section v-if="file.data.value.warnings?.length" aria-labelledby="export-db-warn-h">
          <h3 id="export-db-warn-h">Warnings of the archive</h3>
          <ul class="stack" style="gap: var(--s-1)" data-testid="export-db-warnings">
            <li v-for="w in file.data.value.warnings" :key="w">
              <SafeName :value="w" kind="text" :max="240" />
            </li>
          </ul>
        </section>
        <p class="muted" data-testid="export-db-compare">
          The checksums of an archive find damage and a cut file; they do not prove who made it. If
          you got a SHA-256 for this archive from another channel, compare it with the value above.
          To see whether the data changed while it was read, export the database a second time: if
          the two SHA-256 values are equal, nothing changed during either run.
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
