<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Revokes a signing key that was or may have been stolen (contract 0.11.0, design 3.3): the operator states the time since
// which the key is considered compromised and a reason, and types the fingerprint. A revocation says what it means for the
// old signatures, in the words of the design. It never creates a key: when no other key is active the server answers
// 409 `last_signing_key` (the panel does not offer the action then).
import { reactive, ref } from 'vue'
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import FormField from '@/components/FormField.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import {
  fingerprintGroups,
  fromLocalInput,
  REVOKE_MEANING,
  toLocalInput,
  validRevokeReason,
  type SigningKey,
} from '@/lib/ledger-signing'

const props = defineProps<{ keyRow: SigningKey }>()
const emit = defineEmits<{ close: [] }>()

const form = reactive({ since: toLocalInput(new Date()), reason: '' })
const errors = ref<Record<string, string>>({})

function validate(): boolean {
  const e: Record<string, string> = {}
  const iso = fromLocalInput(form.since)
  if (iso === null) e.compromisedSince = 'Give the time as a date and time.'
  else if (Date.parse(iso) > Date.now() + 60_000)
    e.compromisedSince = 'The time cannot be later than now. An earlier time is the safe direction.'
  if (!validRevokeReason(form.reason))
    e.reason = 'Give a reason of 1 to 200 characters, printable ASCII only (it is published).'
  errors.value = e
  return Object.keys(e).length === 0
}
const errorFor = (field: string, server: Record<string, string>): string | undefined =>
  errors.value[field] ?? server[field]

const start = () =>
  endpoints.revokeLedgerSigningKey(props.keyRow.fingerprint, {
    compromisedSince: fromLocalInput(form.since) ?? new Date().toISOString(),
    reason: form.reason,
    confirm: props.keyRow.fingerprint,
  })
</script>

<template>
  <TaskActionDialog
    title="Revoke the signing key"
    confirm-label="Revoke the key"
    danger
    wide
    :type-to-confirm="keyRow.fingerprint"
    :start="start"
    :validate="validate"
    :invalidate="[keys.ledgerSigning, keys.ledgerStatus]"
    @close="emit('close')"
  >
    <template #default="{ fieldErrors }">
      <p>
        Key <code class="mono wrap">{{ fingerprintGroups(keyRow.fingerprint) }}</code>
      </p>
      <div class="notice tone-danger" role="alert">
        <span aria-hidden="true">&#10006;</span>
        <div class="stack" style="gap: var(--s-1)">
          <strong>What a revocation means for old signatures</strong>
          <ul style="margin: 0; padding-left: 1.2em">
            <li v-for="t in REVOKE_MEANING" :key="t">{{ t }}</li>
          </ul>
        </div>
      </div>
      <FormField
        v-slot="slot"
        label="Compromised since"
        hint="Your local time. The default is now; choose an earlier time if you know the key was exposed before."
        :error="errorFor('compromisedSince', fieldErrors)"
        required
      >
        <input v-bind="slot" v-model="form.since" type="datetime-local" class="input" />
      </FormField>
      <FormField
        v-slot="slot"
        label="Reason"
        hint="At most 200 printable ASCII characters. It is published in the event that records the revocation."
        :error="errorFor('reason', fieldErrors)"
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
      <p class="muted">
        To confirm, type the 64 digits of the fingerprint exactly as shown, without the spaces. The
        server checks it too.
      </p>
      <p v-if="fieldErrors.confirm" class="error" role="alert">{{ fieldErrors.confirm }}</p>
    </template>
    <template #result>
      <p class="notice tone-warn" role="status" data-testid="revoke-next">
        <span aria-hidden="true">&#9650;</span>
        <span
          >The key is revoked. An auditor whose only pinned key it was needs a new trust file,
          obtained out of band. Download it again from the Signing tab.</span
        >
      </p>
    </template>
  </TaskActionDialog>
</template>
