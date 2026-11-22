<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The two actions of a table for the data transfer: Export CSV and Import CSV. Both are for administrators; a person with a
// lower role, a cluster that has the transfer turned off and a table with an unfinished task see them disabled with the
// reason. Mount it where a table has actions (the tables list of a database; the header of the table page).
import { ref } from 'vue'
import GateButton from '@/components/GateButton.vue'
import ExportTableDialog from './ExportTableDialog.vue'
import { useTransferGate } from '@/composables/useTransferGate'
import type { Table } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table }>()
const gate = useTransferGate(
  () => props.databaseName,
  () => props.table.schema,
  () => props.table.name,
)
const open = ref<'export' | null>(null)
</script>

<template>
  <GateButton
    op="exportTable"
    small
    :blocked-reason="gate.reason.value"
    data-testid="export-csv"
    @click="open = 'export'"
  >
    Export CSV
  </GateButton>
  <ExportTableDialog
    v-if="open === 'export'"
    :database-name="databaseName"
    :table="table"
    @close="open = null"
  />
</template>
