<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Revokes a client key (contract 0.11.0): a reason of 1 to 200 characters, and the words about what a revocation means for the
// entries the key signed before. A key that is revoked already is told, not shown as a failure.
import { ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import AppDialog from '@/components/AppDialog.vue'
import FormField from '@/components/FormField.vue'
import SafeName from '@/components/SafeName.vue'
import LedgerRefusal from '@/components/ledger/LedgerRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { clientRefusal, keyRole, REVOKE_TEXT } from '@/lib/ledger-client'
import type { LedgerClientKey } from '@/api/types'

const props = defineProps<{ databaseName: string; clientKey: LedgerClientKey }>()
const emit = defineEmits<{ close: [] }>()
const client = useQueryClient()
const reason = ref('')
const error = ref('')

const revoke = useMutation({
  mutationFn: () =>
    endpoints.revokeLedgerClientKey(props.databaseName, props.clientKey.keyId, reason.value.trim()),
  onSuccess: async () => {
    await client.invalidateQueries({ queryKey: keys.ledger(props.databaseName) })
    emit('close')
  },
  onError: async (e) => {
    // A key that is revoked already is as the person wanted: the list is read again and says so.
    if (clientRefusal(e, 'revoke')?.alreadyDone)
      await client.invalidateQueries({ queryKey: keys.ledger(props.databaseName) })
  },
})
function submit(): void {
  revoke.reset()
  const r = reason.value.trim()
  error.value = r.length < 1 || r.length > 200 ? 'Give a reason of 1 to 200 characters.' : ''
  if (!error.value) revoke.mutate()
}
</script>

<template>
  <AppDialog
    title="Revoke the client key"
    :persistent="revoke.isPending.value"
    @close="emit('close')"
  >
    <form id="revoke-client-key" class="stack" novalidate @submit.prevent="submit">
      <p>
        <strong><SafeName :value="clientKey.label" kind="text" :max="100" /></strong>
        <span class="muted">
          ({{ keyRole(clientKey) }}, key <code class="mono">{{ clientKey.keyId }}</code>)</span
        >
      </p>
      <p>{{ REVOKE_TEXT }}</p>
      <FormField
        v-slot="slot"
        label="Reason"
        hint="Up to 200 characters. It is kept with the revocation."
        :error="error"
        required
      >
        <input v-bind="slot" v-model="reason" class="input" autocomplete="off" maxlength="200" />
      </FormField>
      <LedgerRefusal v-if="revoke.error.value" :error="revoke.error.value" action="revoke" />
    </form>
    <template #footer>
      <button type="button" class="btn" :disabled="revoke.isPending.value" @click="emit('close')">
        Cancel
      </button>
      <button
        type="submit"
        form="revoke-client-key"
        class="btn btn-danger"
        :disabled="revoke.isPending.value"
      >
        Revoke the key
      </button>
    </template>
  </AppDialog>
</template>
