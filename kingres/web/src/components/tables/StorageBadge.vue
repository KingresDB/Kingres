<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The storage format of a table as a badge (contract 0.18.0, `Table.storage`): Row, Columnar, or Mixed (3 columnar of 31) for a
// partitioned table; for a columnar table the compression ratio (`6.2 x`) and the bytes on disk, or `n/a` when the server does not
// know the ratio (never 0). A table whose storage is null is "Not available".
import { computed } from 'vue'
import NotAvailable from '../NotAvailable.vue'
import StatusBadge from '../StatusBadge.vue'
import { formatBytes } from '@/lib/format'
import { STORAGE_COLUMNAR, STORAGE_ROW, ratioText, storageKind, storageText } from '@/lib/analytics'
import type { Table } from '@/api/types'

const props = defineProps<{ table: Pick<Table, 'storage'> }>()
const kind = computed(() => storageKind(props.table))
const columnar = computed(() => props.table.storage?.columnar ?? null)
const ratio = computed(() => ratioText(columnar.value?.compressionRatio))
</script>

<template>
  <span class="stack-tight" data-testid="storage-cell" :data-storage="kind">
    <NotAvailable v-if="kind === 'unknown'" />
    <StatusBadge v-else-if="kind === 'row'" :meta="STORAGE_ROW" />
    <StatusBadge v-else-if="kind === 'columnar'" :meta="STORAGE_COLUMNAR" />
    <StatusBadge
      v-else
      :meta="{ tone: 'info', icon: '◐', label: storageText(table) }"
      data-testid="storage-mixed"
    />
    <span v-if="columnar" class="muted" data-testid="storage-ratio">
      <template v-if="ratio">{{ ratio }}</template><template v-else>n/a</template>
      <template v-if="columnar.bytesOnDisk >= 0"
        >, {{ formatBytes(columnar.bytesOnDisk) }}</template
      >
    </span>
  </span>
</template>

<style scoped>
.stack-tight {
  display: inline-flex;
  flex-direction: column;
  gap: 2px;
  align-items: flex-start;
}
</style>
