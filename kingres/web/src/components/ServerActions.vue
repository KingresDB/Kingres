<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The four server operations, each with a confirmation that states the consequence:
// add server, remove server, failover (hard or graceful), restore preferred primaries.
import { computed, reactive, ref } from 'vue'
import type { QueryKey } from '@tanstack/vue-query'
import TaskActionDialog from './TaskActionDialog.vue'
import FormField from './FormField.vue'
import { endpoints } from '@/api/endpoints'
import { useServerGroups } from '@/api/queries'
import { firstError, nodeAddress, required, compact } from '@/lib/validators'
import type { Server } from '@/api/types'

const props = defineProps<{
  kind: 'add' | 'remove' | 'failover' | 'restore'
  server?: Server
  invalidate?: QueryKey[]
}>()
const emit = defineEmits<{ close: [] }>()

const form = reactive({ address: '', setupToken: '', serverGroup: '' })
const mode = ref<'hard' | 'graceful'>('graceful')
const submitted = ref(false)
const groups = useServerGroups()

const errors = computed(() =>
  compact({
    address: firstError(form.address, nodeAddress()),
    setupToken: firstError(form.setupToken, required('The setup token')),
  }),
)

function validate(): boolean {
  submitted.value = true
  return props.kind !== 'add' || Object.keys(errors.value).length === 0
}

const primaries = computed(() => props.server?.instances.filter((i) => i.role === 'primary') ?? [])

const config = computed(() => {
  const id = props.server?.id ?? ''
  switch (props.kind) {
    case 'add':
      return {
        title: 'Add server',
        confirmLabel: 'Add server',
        danger: false,
        start: () =>
          endpoints.addServer({
            address: form.address,
            setupToken: form.setupToken,
            serverGroup: form.serverGroup || null,
          }),
      }
    case 'remove':
      return {
        title: `Remove server ${props.server?.address ?? ''}`,
        confirmLabel: 'Remove server',
        danger: true,
        start: () => endpoints.removeServer(id),
      }
    case 'failover':
      return {
        title: `Fail over server ${props.server?.address ?? ''}`,
        confirmLabel: mode.value === 'hard' ? 'Hard failover' : 'Graceful failover',
        danger: mode.value === 'hard',
        start: () => endpoints.failoverServer(id, mode.value),
      }
    default:
      return {
        title: 'Restore preferred primaries',
        confirmLabel: 'Restore primaries',
        danger: false,
        start: () => endpoints.restorePreferredPrimaries(),
      }
  }
})
</script>

<template>
  <TaskActionDialog
    :title="config.title"
    :confirm-label="config.confirmLabel"
    :danger="config.danger"
    :start="config.start"
    :validate="validate"
    :invalidate="invalidate"
    @close="emit('close')"
  >
    <template #default="{ fieldErrors }">
      <template v-if="kind === 'add'">
        <p>
          The server joins the cluster and shows as <strong>pending rebalance</strong>. No data
          moves until you start a rebalance.
        </p>
        <FormField
          v-slot="slot"
          label="Server address"
          hint="host:port of the new server's panel port."
          :error="(submitted ? errors.address : undefined) ?? fieldErrors.address"
          required
        >
          <input v-bind="slot" v-model="form.address" class="input mono" data-autofocus />
        </FormField>
        <FormField
          v-slot="slot"
          label="Setup token of the new server"
          :error="(submitted ? errors.setupToken : undefined) ?? fieldErrors.setupToken"
          required
        >
          <input v-bind="slot" v-model="form.setupToken" class="input mono" autocomplete="off" />
        </FormField>
        <FormField v-slot="slot" label="Server group (optional)">
          <select v-bind="slot" v-model="form.serverGroup">
            <option value="">None</option>
            <option v-for="g in groups.data.value?.items" :key="g.name" :value="g.name">
              {{ g.name }}
            </option>
          </select>
        </FormField>
      </template>

      <template v-else-if="kind === 'remove'">
        <p>
          Server <strong>{{ server?.address }}</strong> is drained first: its
          <strong>{{ server?.buckets.primary }}</strong> primary buckets move to the other servers,
          then it leaves the cluster. This runs a rebalance and can take a while; reads and writes
          continue.
        </p>
        <p class="muted">Remaining servers must have room for the data.</p>
      </template>

      <template v-else-if="kind === 'failover'">
        <fieldset>
          <legend>Failover mode</legend>
          <label class="check"
            ><input v-model="mode" type="radio" value="graceful" /> Graceful: wait for standbys to
            catch up, then switch roles. No data loss.</label
          >
          <label class="check"
            ><input v-model="mode" type="radio" value="hard" /> Hard: promote standbys now. Writes
            not yet replicated can be lost.</label
          >
        </fieldset>
        <p v-if="mode === 'hard'" class="notice tone-danger" role="alert">
          <span aria-hidden="true">&#10006;</span>
          <span>
            Hard failover marks <strong>{{ server?.address }}</strong> as dead. Its
            {{ primaries.length }} primary instance(s) are replaced by their standbys, and their
            former neighbours carry double write load until you restore the preferred primaries.
          </span>
        </p>
        <p v-else>
          The {{ primaries.length }} primary instance(s) on
          <strong>{{ server?.address }}</strong> hand over to their standbys. The server stays in
          the cluster as a standby.
        </p>
      </template>

      <template v-else>
        <p>
          Every shard group whose primary runs on a server other than its preferred one is switched
          back to its home server. Connections to those primaries are interrupted for a moment.
        </p>
      </template>
    </template>
  </TaskActionDialog>
</template>
