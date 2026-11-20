<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Switches the signature policy of a ledger table to "required" (contract 0.11.0, extension 0.3, design 3.7). It cannot go back
// and it waits for the open writers of the table, so the dialog says both, and what a write without an attestation meets, before
// the click; the name of the table is typed. Each refusal has its own words. A policy that is "required" already is reported to
// the page, which then shows the table as required.
import { ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import AppDialog from '@/components/AppDialog.vue'
import SafeName from '@/components/SafeName.vue'
import TypeToConfirm from '@/components/TypeToConfirm.vue'
import LedgerRefusal from '@/components/ledger/LedgerRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { clientRefusal, POLICY_BEFORE_TEXT } from '@/lib/ledger-client'
import type { Table } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table }>()
const emit = defineEmits<{ close: []; required: [] }>()
const client = useQueryClient()
const typed = ref('')
const qualified = `${props.table.schema}.${props.table.name}`

const set = useMutation({
  mutationFn: () =>
    endpoints.setTableSignaturePolicy(props.databaseName, props.table.schema, props.table.name),
  onSuccess: async () => {
    await client.invalidateQueries({ queryKey: keys.tables(props.databaseName) })
    emit('required')
    emit('close')
  },
  onError: (e) => {
    if (clientRefusal(e, 'policy')?.alreadyDone) emit('required')
  },
})
</script>

<template>
  <AppDialog
    title="Require client signatures"
    wide
    :persistent="set.isPending.value"
    @close="emit('close')"
  >
    <div class="stack" data-testid="policy-dialog">
      <p>
        Table
        <strong
          ><code class="mono"><SafeName :value="qualified" kind="name" :max="120" /></code
        ></strong>
        will require a client signature on every entry that is written to it.
      </p>
      <ul class="stack" data-testid="policy-consequences">
        <li v-for="t in POLICY_BEFORE_TEXT" :key="t">{{ t }}</li>
      </ul>
      <p
        v-if="set.isPending.value"
        class="notice tone-info"
        role="status"
        data-testid="policy-waiting"
      >
        <span aria-hidden="true">&#9680;</span>
        <span>Waiting for the open writers of the table. This can take a while.</span>
      </p>
      <TypeToConfirm v-model="typed" :expected="qualified" />
      <LedgerRefusal v-if="set.error.value" :error="set.error.value" action="policy" />
    </div>
    <template #footer>
      <button type="button" class="btn" :disabled="set.isPending.value" @click="emit('close')">
        Cancel
      </button>
      <button
        type="button"
        class="btn btn-danger"
        :disabled="typed !== qualified || set.isPending.value"
        @click="set.mutate()"
      >
        Require signatures
      </button>
    </template>
  </AppDialog>
</template>
