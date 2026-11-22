<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The Transfers panel: the staged export files of the signed-in role (size, SHA-256, consistency, expiry), with Download
// and Delete. The panel says why it shows nothing when data transfer is turned off or the role is too low, and never asks
// the server for what it would refuse. A file of another role is never listed (the server answers only for the caller).
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import TableWrap from '@/components/TableWrap.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import NullableValue from '@/components/NullableValue.vue'
import DownloadExportDialog from './DownloadExportDialog.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useClusterSettings, useDataExports } from '@/api/queries'
import { denialReason } from '@/api/roles'
import { useSessionStore } from '@/stores/session'
import type { DataExport } from '@/api/types'
import { EXPORT_STATE } from '@/lib/status'
import { formatBytes, formatDateTime, formatNumber } from '@/lib/format'
import { formatLabel } from '@/lib/transfer'

const props = defineProps<{
  /** Only the files of this database (the page of a database); the panel says how many others exist. */
  database?: string
}>()

const session = useSessionStore()
const client = useQueryClient()
const settings = useClusterSettings()
const off = computed(() => settings.data.value?.dataTransfer?.enabled === false)
const allowed = computed(() => session.can('listDataExports'))
const exports = useDataExports(() => allowed.value && !off.value)

const all = computed(() => exports.data.value?.items ?? [])
const shown = computed(() =>
  props.database === undefined ? all.value : all.value.filter((e) => e.database === props.database),
)
const others = computed(() => all.value.length - shown.value.length)

const downloading = ref<DataExport | null>(null)
const deleting = ref<DataExport | null>(null)
const remove = useMutation({
  mutationFn: (id: string) => endpoints.deleteDataExport(id),
  onSuccess: async () => {
    deleting.value = null
    await client.invalidateQueries({ queryKey: keys.dataExports })
  },
})

function nameOf(e: DataExport): string {
  return [e.database, e.schema, e.table].filter((p) => p !== null && p !== undefined).join('.')
}
function levels(e: DataExport): string {
  if (!e.consistency || e.consistency.length === 0) return ''
  return e.consistency
    .map((c) => (c.level === 'single-snapshot' ? 'one snapshot' : `per group (${c.groups})`))
    .join(', ')
}
</script>

<template>
  <section aria-labelledby="transfers-h" class="stack" data-testid="transfers-panel">
    <h2 id="transfers-h">Transfers</h2>
    <p v-if="!allowed" class="muted" data-testid="transfers-role">
      {{ denialReason('listDataExports') }} Export files are listed only for administrators.
    </p>
    <p v-else-if="off" class="notice tone-info" role="status" data-testid="transfers-off">
      <span aria-hidden="true">&#9432;</span>
      <span
        >Data transfer is turned off for this cluster, so tables cannot be exported or imported. An
        administrator can turn it on under
        <RouterLink :to="{ name: 'settings' }">Settings, Data transfer</RouterLink>.</span
      >
    </p>
    <QueryBoundary
      v-else
      :loading="exports.isPending.value"
      :error="exports.error.value"
      :empty="shown.length === 0"
      empty-title="No export files"
      empty-hint="A table you export is kept here until its retention time ends. Only your own files are listed."
      @retry="exports.refetch()"
    >
      <TableWrap>
        <table class="table">
          <caption class="sr-only">
            Export files of the signed-in role
          </caption>
          <thead>
            <tr>
              <th scope="col">Table</th>
              <th scope="col">State</th>
              <th scope="col" class="num">Size</th>
              <th scope="col" class="num">Rows</th>
              <th scope="col">Consistency</th>
              <th scope="col">SHA-256</th>
              <th scope="col">Kept until</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="e in shown" :key="e.exportId" :data-export="e.exportId">
              <th scope="row" class="mono"><SafeName :value="nameOf(e)" :max="120" /></th>
              <td>
                <StatusBadge :meta="EXPORT_STATE[e.state]" />
                <span class="muted"> {{ formatLabel(e.format) }}</span>
              </td>
              <td class="num">
                <NullableValue :value="e.sizeBytes ?? null" :format="formatBytes" />
              </td>
              <td class="num"><NullableValue :value="e.rows ?? null" :format="formatNumber" /></td>
              <td>{{ levels(e) }}</td>
              <td>
                <template v-if="e.sha256">
                  <span class="mono" aria-hidden="true" :title="e.sha256"
                    >{{ e.sha256.slice(0, 12) }}&hellip;</span
                  >
                  <span class="sr-only">{{ e.sha256 }}</span>
                </template>
                <span v-else class="muted">-</span>
              </td>
              <td>{{ formatDateTime(e.expiresAt) }}</td>
              <td>
                <div class="row">
                  <GateButton
                    op="downloadDataExport"
                    small
                    :blocked-reason="
                      e.state === 'ready' ? '' : 'Only a file that is ready can be downloaded.'
                    "
                    @click="downloading = e"
                  >
                    Download
                  </GateButton>
                  <GateButton op="deleteDataExport" small variant="danger" @click="deleting = e">
                    Delete
                  </GateButton>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
      <p v-if="others > 0" class="muted" data-testid="transfers-others">
        {{ others }} more {{ others === 1 ? 'file' : 'files' }} of yours are for other databases.
      </p>
      <p v-if="exports.data.value?.moreNotShown" class="notice tone-warn" role="status">
        <span aria-hidden="true">&#9650;</span>
        <span>More files exist than are shown here.</span>
      </p>
    </QueryBoundary>
  </section>

  <DownloadExportDialog v-if="downloading" :file="downloading" @close="downloading = null" />
  <ConfirmDialog
    v-if="deleting"
    title="Delete an export file"
    confirm-label="Delete the file"
    danger
    :busy="remove.isPending.value"
    @cancel="deleting = null"
    @confirm="remove.mutate(deleting.exportId)"
  >
    <p>
      The file of <strong><SafeName :value="nameOf(deleting)" :max="120" /></strong> is deleted from
      the server and its room is freed. It cannot be downloaded again; export the table again if you
      need it.
    </p>
    <ProblemNotice v-if="remove.error.value" :error="remove.error.value" />
  </ConfirmDialog>
</template>
