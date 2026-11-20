<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Registers the public key of a client (contract 0.11.0, design 3.7): 32 bytes as standard base64, an optional role, a label. A
// private key is never asked for: there is no field for one, and the text says where the key pair is made.
import { computed, reactive, ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import AppDialog from '@/components/AppDialog.vue'
import FormField from '@/components/FormField.vue'
import LedgerRefusal from '@/components/ledger/LedgerRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys } from '@/api/queries'
import { REGISTER_TEXT, registerBody, validateRegister } from '@/lib/ledger-client'

const props = defineProps<{ databaseName: string }>()
const emit = defineEmits<{ close: []; saved: [keyId: string] }>()
const client = useQueryClient()
const form = reactive({ publicKey: '', roleName: '', label: '' })
const errors = ref<Record<string, string>>({})

const save = useMutation({
  mutationFn: () => endpoints.registerLedgerClientKey(props.databaseName, registerBody(form)),
  onSuccess: async (key) => {
    await client.invalidateQueries({ queryKey: keys.ledger(props.databaseName) })
    emit('saved', key.keyId)
    emit('close')
  },
})
const serverErrors = computed(() =>
  save.error.value ? describeProblem(save.error.value).fieldErrors : {},
)
const errorFor = (f: string): string | undefined => errors.value[f] ?? serverErrors.value[f]

function submit(): void {
  save.reset()
  errors.value = validateRegister(form)
  if (Object.keys(errors.value).length === 0) save.mutate()
}
</script>

<template>
  <AppDialog
    title="Register a client key"
    wide
    :persistent="save.isPending.value"
    @close="emit('close')"
  >
    <form
      id="client-key-form"
      class="stack"
      novalidate
      data-testid="client-key-form"
      @submit.prevent="submit"
    >
      <p>{{ REGISTER_TEXT }}</p>
      <FormField
        v-slot="slot"
        label="Public key (base64)"
        hint="The public half of the key pair. Never paste a private key here."
        :error="errorFor('publicKey')"
        required
      >
        <input
          v-bind="slot"
          v-model="form.publicKey"
          class="input mono"
          autocomplete="off"
          spellcheck="false"
        />
      </FormField>
      <FormField
        v-slot="slot"
        label="Role (optional)"
        hint="Only this role may attest with the key. Empty: any role may."
        :error="errorFor('roleName')"
      >
        <input v-bind="slot" v-model="form.roleName" class="input" autocomplete="off" />
      </FormField>
      <FormField
        v-slot="slot"
        label="Label"
        hint="1 to 100 characters: whose key this is, for the people who read the list."
        :error="errorFor('label')"
        required
      >
        <input
          v-bind="slot"
          v-model="form.label"
          class="input"
          autocomplete="off"
          maxlength="100"
        />
      </FormField>
      <LedgerRefusal v-if="save.error.value" :error="save.error.value" action="register" />
    </form>
    <template #footer>
      <button type="button" class="btn" :disabled="save.isPending.value" @click="emit('close')">
        Cancel
      </button>
      <button
        type="submit"
        form="client-key-form"
        class="btn btn-primary"
        :disabled="save.isPending.value"
      >
        Register the key
      </button>
    </template>
  </AppDialog>
</template>
