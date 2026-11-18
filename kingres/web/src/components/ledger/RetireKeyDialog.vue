<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Retires a signing key now (contract 0.11.0, design 3.3): the key stops signing, a `key-retired` event signed by another
// key records it and the private file is deleted from every server that holds it. What it signed stays valid.
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { fingerprintGroups, RETIRE_MEANING, type SigningKey } from '@/lib/ledger-signing'

const props = defineProps<{ keyRow: SigningKey }>()
const emit = defineEmits<{ close: [] }>()
const start = () => endpoints.retireLedgerSigningKey(props.keyRow.fingerprint)
</script>

<template>
  <TaskActionDialog
    title="Retire the signing key"
    confirm-label="Retire the key"
    danger
    :start="start"
    :invalidate="[keys.ledgerSigning, keys.ledgerStatus]"
    @close="emit('close')"
  >
    <p>
      Key <code class="mono wrap">{{ fingerprintGroups(keyRow.fingerprint) }}</code>
    </p>
    <ul>
      <li v-for="t in RETIRE_MEANING" :key="t">{{ t }}</li>
    </ul>
  </TaskActionDialog>
</template>
