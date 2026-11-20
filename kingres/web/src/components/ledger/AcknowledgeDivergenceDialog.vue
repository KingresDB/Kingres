<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Acknowledges a diverged chain and starts a new epoch (contract 0.11.0, design 9.4). A dangerous action: the dialog says what it
// means for the history before anything is typed, asks what happened and why, and needs the name of the database typed.
import { reactive, ref } from 'vue'
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import FormField from '@/components/FormField.vue'
import SafeName from '@/components/SafeName.vue'
import LedgerRefusal from '@/components/ledger/LedgerRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { ACK_KIND, ACK_TEXT } from '@/lib/ledger-ops'

const props = defineProps<{ databaseName: string; shardGroupId: number }>()
const emit = defineEmits<{ close: [] }>()
const form = reactive({ kind: '', reason: '' })
const errors = ref<Record<string, string>>({})
function validate(): boolean {
  const e: Record<string, string> = {}
  if (!(form.kind in ACK_KIND)) e.kind = 'Choose what happened.'
  const r = form.reason.trim()
  if (r.length < 1 || r.length > 200) e.reason = 'Give a reason of 1 to 200 characters.'
  errors.value = e
  return Object.keys(e).length === 0
}
const start = () =>
  endpoints.acknowledgeLedgerDivergence(props.databaseName, props.shardGroupId, {
    kind: form.kind as 'restore' | 'failoverLoss' | 'other',
    reason: form.reason.trim(),
    confirm: props.databaseName,
  })
</script>

<template>
  <TaskActionDialog
    :title="`Acknowledge the divergence of shard group ${shardGroupId}`"
    confirm-label="Acknowledge and start a new epoch"
    danger
    wide
    :type-to-confirm="databaseName"
    :start="start"
    :validate="validate"
    :invalidate="[keys.ledger(databaseName), keys.ledgerStatus, keys.ledgerPublication, keys.tasks]"
    @close="emit('close')"
  >
    <template #error="{ error }"><LedgerRefusal :error="error" ops="acknowledge" /></template>
    <template #default>
      <p>
        Database <strong><SafeName :value="databaseName" :max="60" /></strong>, shard group
        {{ shardGroupId }}.
      </p>
      <ul class="stack" data-testid="ack-text">
        <li v-for="t in ACK_TEXT" :key="t">{{ t }}</li>
      </ul>
      <FormField v-slot="slot" label="What happened" :error="errors.kind" required>
        <select v-bind="slot" v-model="form.kind">
          <option value="">Choose one</option>
          <option v-for="(label, k) in ACK_KIND" :key="k" :value="k">{{ label }}</option>
        </select>
      </FormField>
      <FormField
        v-slot="slot"
        label="Reason"
        hint="Up to 200 characters. It is part of the history."
        :error="errors.reason"
        required
      >
        <input
          v-bind="slot"
          v-model="form.reason"
          class="input"
          maxlength="200"
          autocomplete="off"
        />
      </FormField>
    </template>
  </TaskActionDialog>
</template>
