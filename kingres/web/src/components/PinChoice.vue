<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The pin of "Add server" and "Join cluster" (contract 0.6.0): the fingerprint of the other server's
// certificate is the normal path and is required. Skipping it is an explicit choice that is never
// pre-selected, worded as what it means, and it is sent as `allowUnpinned: true`.
import FormField from './FormField.vue'
import FingerprintHelp from './FingerprintHelp.vue'

defineProps<{
  purpose: 'add' | 'join'
  /** The message to show under the fingerprint field (client-side check or the server's 400). */
  error?: string
  /** The hint under the field. */
  hint: string
}>()
const fingerprint = defineModel<string>('fingerprint', { required: true })
const unpinned = defineModel<boolean>('unpinned', { required: true })
</script>

<template>
  <div class="stack" style="gap: var(--s-2)">
    <FormField
      v-slot="slot"
      label="Expected fingerprint"
      :hint="hint"
      :error="error"
      :required="!unpinned"
    >
      <input
        v-bind="slot"
        v-model="fingerprint"
        class="input mono"
        autocomplete="off"
        spellcheck="false"
        :disabled="unpinned"
        data-testid="pin-fingerprint"
      />
    </FormField>
    <FingerprintHelp :purpose="purpose" />
    <fieldset class="stack" style="gap: var(--s-1)" data-testid="pin-skip">
      <legend>If you do not have the fingerprint</legend>
      <label class="check"
        ><input v-model="unpinned" type="checkbox" data-testid="allow-unpinned" /> Do not check the
        certificate: trust whatever answers at this address</label
      >
      <p v-if="unpinned" class="notice tone-warn" role="status" data-testid="unpinned-warning">
        <span aria-hidden="true">&#9650;</span>
        <span v-if="purpose === 'join'"
          >Nothing will prove that the machine at this address is the server you mean. Whatever
          answers there is trusted, and
          <strong>the administrator password you type here is sent to it</strong>. A machine on the
          network path, or a wrong address, would receive it. Choose this only on a network you
          control.</span
        >
        <span v-else
          >Nothing will prove that the machine at this address is the server you mean. Whatever
          answers there is trusted and receives the setup token you typed. A machine on the network
          path, or a wrong address, would answer in the new server's place. Choose this only on a
          network you control.</span
        >
      </p>
    </fieldset>
  </div>
</template>
