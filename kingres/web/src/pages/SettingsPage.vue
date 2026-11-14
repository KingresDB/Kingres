<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Cluster settings: default replica count and durability, auto-failover, rebalance concurrency and
// feature flags (which can be turned on, never off).
import { computed, reactive, ref, watch } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import FormField from '@/components/FormField.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys, useClusterSettings } from '@/api/queries'
import { DURABILITY_LABEL } from '@/lib/status'
import { compact, firstError, integerBetween } from '@/lib/validators'
import type { ClusterSettingsUpdateRequest, Durability } from '@/api/types'

const client = useQueryClient()
const settings = useClusterSettings()
const s = computed(() => settings.data.value)

const form = reactive({
  replicas: 1,
  durability: 'sync' as Durability,
  afEnabled: true,
  afTimeout: 120,
  afMax: 1,
  maxMoves: 4,
  sharding: false,
  ledger: false,
})
const saved = ref(false)
const submitted = ref(false)

function load(): void {
  const d = s.value
  if (!d) return
  Object.assign(form, {
    replicas: d.defaultReplicaCount,
    durability: d.defaultDurability,
    afEnabled: d.autoFailover.enabled,
    afTimeout: d.autoFailover.timeoutSeconds,
    afMax: d.autoFailover.maxCount,
    maxMoves: d.rebalance.maxConcurrentMoves,
    sharding: d.features.sharding,
    ledger: d.features.ledger,
  })
}
watch(s, load, { immediate: true })

const errors = computed(() =>
  compact({
    defaultReplicaCount: firstError<number | null>(
      form.replicas,
      integerBetween('The replica count', 0, 3),
    ),
    'autoFailover.timeoutSeconds': firstError<number | null>(
      form.afTimeout,
      integerBetween('The timeout', 1, 3600),
    ),
    'autoFailover.maxCount': firstError<number | null>(
      form.afMax,
      integerBetween('The maximum count', 1, 100),
    ),
    'rebalance.maxConcurrentMoves': firstError<number | null>(
      form.maxMoves,
      integerBetween('Concurrent moves', 1, 64),
    ),
  }),
)

const save = useMutation({
  mutationFn: () => {
    const d = s.value
    const body: ClusterSettingsUpdateRequest = {
      defaultReplicaCount: form.replicas,
      defaultDurability: form.durability,
      autoFailover: {
        enabled: form.afEnabled,
        timeoutSeconds: form.afTimeout,
        maxCount: form.afMax,
      },
      rebalance: { maxConcurrentMoves: form.maxMoves },
      // Only send features that are being turned on; turning one off is not possible.
      features: {
        ...(form.sharding && !d?.features.sharding ? { sharding: true } : {}),
        ...(form.ledger && !d?.features.ledger ? { ledger: true } : {}),
      },
    }
    return endpoints.updateClusterSettings(body)
  },
  onSuccess: async () => {
    saved.value = true
    await Promise.all([
      client.invalidateQueries({ queryKey: keys.clusterSettings }),
      client.invalidateQueries({ queryKey: keys.cluster }),
    ])
  },
})
const serverErrors = computed(() =>
  save.error.value ? describeProblem(save.error.value).fieldErrors : {},
)
const err = (f: string) => (submitted.value ? errors.value[f] : undefined) ?? serverErrors.value[f]

function submit(): void {
  submitted.value = true
  saved.value = false
  if (Object.keys(errors.value).length === 0) save.mutate()
}
</script>

<template>
  <PageHeader
    title="Settings"
    subtitle="Cluster-wide settings. Changes apply to the whole cluster."
  />
  <QueryBoundary
    :loading="settings.isPending.value"
    :error="settings.error.value"
    @retry="settings.refetch()"
  >
    <form v-if="s" class="stack-lg" style="max-width: 720px" novalidate @submit.prevent="submit">
      <p v-if="s.restart.required" class="notice tone-warn" role="status">
        <span aria-hidden="true">&#9650;</span>
        <span
          >A restart is required on server(s) {{ s.restart.serverIds.join(', ') }} before a newly
          enabled feature is loaded.</span
        >
      </p>
      <fieldset>
        <legend>Replication</legend>
        <FormField
          v-slot="slot"
          label="Default replica count"
          hint="Standby copies of every shard group, 0 to 3. Changing it needs a rebalance to add or remove the copies."
          :error="err('defaultReplicaCount')"
        >
          <input
            v-bind="slot"
            v-model.number="form.replicas"
            class="input num"
            type="number"
            min="0"
            max="3"
          />
        </FormField>
        <FormField v-slot="slot" label="Default durability for new databases">
          <select v-bind="slot" v-model="form.durability">
            <option v-for="(label, value) in DURABILITY_LABEL" :key="value" :value="value">
              {{ label }}
            </option>
          </select>
        </FormField>
      </fieldset>
      <fieldset>
        <legend>Auto-failover</legend>
        <label class="check"
          ><input v-model="form.afEnabled" type="checkbox" /> Fail over automatically when a server
          stops answering</label
        >
        <FormField
          v-slot="slot"
          label="Timeout (seconds)"
          hint="1 to 3600. A server must be silent this long."
          :error="err('autoFailover.timeoutSeconds')"
        >
          <input
            v-bind="slot"
            v-model.number="form.afTimeout"
            class="input num"
            type="number"
            min="1"
            max="3600"
          />
        </FormField>
        <FormField
          v-slot="slot"
          label="Maximum automatic failovers"
          hint="After this many events further failovers need an operator. A rebalance resets the counter."
          :error="err('autoFailover.maxCount')"
        >
          <input
            v-bind="slot"
            v-model.number="form.afMax"
            class="input num"
            type="number"
            min="1"
            max="100"
          />
        </FormField>
        <p class="muted">
          Events since the last reset: <strong>{{ s.autoFailover.eventsSinceReset }}</strong>
        </p>
      </fieldset>
      <fieldset>
        <legend>Rebalance</legend>
        <FormField
          v-slot="slot"
          label="Concurrent moves"
          hint="1 to 64. Default 4."
          :error="err('rebalance.maxConcurrentMoves')"
        >
          <input
            v-bind="slot"
            v-model.number="form.maxMoves"
            class="input num"
            type="number"
            min="1"
            max="64"
          />
        </FormField>
      </fieldset>
      <fieldset>
        <legend>Features</legend>
        <label class="check"
          ><input v-model="form.sharding" type="checkbox" :disabled="s.features.sharding" />
          Sharding
          <span v-if="s.features.sharding" class="muted">(on; cannot be turned off)</span></label
        >
        <label class="check"
          ><input v-model="form.ledger" type="checkbox" :disabled="s.features.ledger" /> Ledger
          <span v-if="s.features.ledger" class="muted">(on; cannot be turned off)</span></label
        >
        <p class="hint muted">The first time a feature is enabled, every server needs a restart.</p>
      </fieldset>
      <ProblemNotice v-if="save.error.value" :error="save.error.value" />
      <p v-if="saved" class="notice tone-ok" role="status">
        <span aria-hidden="true">&#10003;</span> Settings saved.
      </p>
      <div class="row">
        <GateButton
          op="updateClusterSettings"
          type="submit"
          variant="primary"
          :disabled="save.isPending.value"
          >Save settings</GateButton
        >
        <button type="button" class="btn" @click="load">Reset</button>
      </div>
    </form>
  </QueryBoundary>
</template>
