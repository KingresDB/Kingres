<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The two actions of a database for the data transfer (contract 0.29.0): Export database and Validate an archive. Both are for
// administrators; a person with a lower role, a cluster that has the transfer turned off and a database that has an unfinished
// export or table task (export only) see them disabled with the reason. Mount it in the header of the database page.
import { computed, ref } from 'vue'
import GateButton from '@/components/GateButton.vue'
import ExportDatabaseDialog from './ExportDatabaseDialog.vue'
import ValidateArchiveDialog from './ValidateArchiveDialog.vue'
import { useClusterSettings, useTasks } from '@/api/queries'

const props = defineProps<{ databaseName: string }>()
const settings = useClusterSettings()
const tasks = useTasks(100)
const off = computed(() => settings.data.value?.dataTransfer?.enabled === false)
const running = computed(() =>
  (tasks.data.value?.items ?? []).filter(
    (t) => t.state === 'running' || t.state === 'pending' || t.state === 'paused',
  ),
)
const exportReason = computed(() => {
  if (off.value) return 'Data transfer is turned off for this cluster (Settings, Data transfer).'
  const mine = running.value.some(
    (t) =>
      (t.kind === 'database.export' && t.subject?.id === props.databaseName) ||
      (t.subject?.type === 'table' && t.subject.id.startsWith(`${props.databaseName}/`)),
  )
  return mine ? 'Waiting for the operation on this database to end.' : ''
})
const validateReason = computed(() =>
  off.value ? 'Data transfer is turned off for this cluster (Settings, Data transfer).' : '',
)
const open = ref<'export' | 'validate' | null>(null)
</script>

<template>
  <GateButton
    op="exportDatabase"
    :blocked-reason="exportReason"
    data-testid="export-database"
    @click="open = 'export'"
  >
    Export database
  </GateButton>
  <GateButton
    op="validateDatabaseArchive"
    :blocked-reason="validateReason"
    data-testid="validate-archive"
    @click="open = 'validate'"
  >
    Validate an archive
  </GateButton>
  <ExportDatabaseDialog
    v-if="open === 'export'"
    :database-name="databaseName"
    @close="open = null"
  />
  <ValidateArchiveDialog v-if="open === 'validate'" @close="open = null" />
</template>
