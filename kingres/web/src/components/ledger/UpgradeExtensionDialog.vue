<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The upgrade of the ledger extension of a database (contract 0.11.0, design 9.5): one task that runs ALTER EXTENSION UPDATE in
// every shard group. The consequence is said before the click; a failed group fails the task and its error names the server.
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import SafeName from '@/components/SafeName.vue'
import LedgerRefusal from '@/components/ledger/LedgerRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { UPGRADE_TEXT } from '@/lib/ledger-client'

const props = defineProps<{ databaseName: string }>()
const emit = defineEmits<{ close: [] }>()
</script>

<template>
  <TaskActionDialog
    title="Upgrade the ledger extension"
    confirm-label="Upgrade the extension"
    wide
    :start="() => endpoints.upgradeLedgerExtension(props.databaseName)"
    :invalidate="[keys.ledger(props.databaseName), keys.ledgerStatus, keys.tasks]"
    @close="emit('close')"
  >
    <template #error="{ error }">
      <LedgerRefusal :error="error" action="upgrade" />
    </template>
    <p>
      Database <strong><SafeName :value="databaseName" :max="60" /></strong>.
    </p>
    <p data-testid="upgrade-text">{{ UPGRADE_TEXT }}</p>
  </TaskActionDialog>
</template>
