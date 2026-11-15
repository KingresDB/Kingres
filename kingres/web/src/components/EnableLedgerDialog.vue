<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Turn the ledger on for one table. This is permanent: the ledger of a table cannot be switched off
// again, because that would defeat tamper evidence. The user types the table name to confirm.
import { ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import ConfirmDialog from './ConfirmDialog.vue'
import ProblemNotice from './ProblemNotice.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import type { Table } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table }>()
const emit = defineEmits<{ close: [] }>()

const client = useQueryClient()
const mode = ref<'appendOnly' | 'updatable'>('appendOnly')
const enable = useMutation({
  mutationFn: () =>
    endpoints.enableTableLedger(
      props.databaseName,
      props.table.schema,
      props.table.name,
      mode.value,
    ),
  onSuccess: async () => {
    await Promise.all([
      client.invalidateQueries({ queryKey: keys.tables(props.databaseName) }),
      client.invalidateQueries({ queryKey: keys.databases }),
      client.invalidateQueries({ queryKey: keys.ledgerStatus }),
      client.invalidateQueries({
        queryKey: keys.table(props.databaseName, props.table.schema, props.table.name),
      }),
    ])
    emit('close')
  },
})
</script>

<template>
  <ConfirmDialog
    :title="`Enable the ledger for ${table.schema}.${table.name}`"
    confirm-label="Enable ledger permanently"
    danger
    :type-to-confirm="table.name"
    :busy="enable.isPending.value"
    @cancel="emit('close')"
    @confirm="enable.mutate()"
  >
    <p class="notice tone-danger" role="alert">
      <span aria-hidden="true">&#10006;</span>
      <span>
        This is <strong>permanent</strong>. Once a table has a ledger it can never be turned off
        again, not even by an administrator: every change from now on is recorded in a
        tamper-evident chain.
      </span>
    </p>
    <fieldset>
      <legend>Ledger mode</legend>
      <label class="check"
        ><input v-model="mode" type="radio" value="appendOnly" /> Append-only: rows can be added but
        never changed or deleted.</label
      >
      <label class="check"
        ><input v-model="mode" type="radio" value="updatable" /> Updatable: rows can change; every
        earlier version is kept in a history table.</label
      >
    </fieldset>
    <ProblemNotice v-if="enable.error.value" :error="enable.error.value" />
  </ConfirmDialog>
</template>
