<!-- SPDX-License-Identifier: AGPL-3.0-only -->
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
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import AutoFailoverCounter from '@/components/AutoFailoverCounter.vue'
import AnchorSettings from '@/components/ledger/AnchorSettings.vue'
import { AUTO_FAILOVER_REQUIREMENTS } from '@/lib/failover'
import { vBlockEnterSubmit } from '@/lib/forms'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys, useClusterSettings } from '@/api/queries'
import { DURABILITY_LABEL } from '@/lib/status'
import { fromGib, MAX_ATOMIC_BYTES, toGib } from '@/lib/transfer'
import { formatBytes } from '@/lib/format'
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
  // Data transfer (0.19.0). The sizes are in gibibytes in the form and in bytes on the wire.
  dtEnabled: false,
  dtRetention: 24,
  dtMaxFile: 20,
  dtQuota: 100,
  dtConcurrent: 2,
})
const saved = ref(false)
const submitted = ref(false)
/** True once the person has edited a field; a refresh from the server must not overwrite edits. */
const dirty = ref(false)
const confirmingFeatures = ref<string[] | null>(null)

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
  if (d.dataTransfer)
    Object.assign(form, {
      dtEnabled: d.dataTransfer.enabled,
      dtRetention: d.dataTransfer.retentionHours,
      dtMaxFile: toGib(d.dataTransfer.maxFileBytes),
      dtQuota: toGib(d.dataTransfer.stagingQuotaBytes),
      dtConcurrent: d.dataTransfer.maxConcurrent,
    })
  dirty.value = false
}
watch(
  s,
  () => {
    if (!dirty.value) load()
  },
  { immediate: true },
)

const errors = computed(() =>
  compact({
    defaultReplicaCount: firstError<number | null>(
      form.replicas,
      integerBetween('The replica count', 0, 3),
    ),
    'autoFailover.timeoutSeconds': firstError<number | null>(
      form.afTimeout,
      integerBetween('The timeout', 30, 3600),
    ),
    'autoFailover.maxCount': firstError<number | null>(
      form.afMax,
      integerBetween('The maximum count', 1, 100),
    ),
    'rebalance.maxConcurrentMoves': firstError<number | null>(
      form.maxMoves,
      integerBetween('Concurrent moves', 1, 64),
    ),
    ...(s.value?.dataTransfer
      ? {
          'dataTransfer.retentionHours': firstError<number | null>(
            form.dtRetention,
            integerBetween('The retention time', 1, 168),
          ),
          'dataTransfer.maxFileBytes': firstError<number | null>(
            form.dtMaxFile,
            minimumGib('The largest file'),
          ),
          'dataTransfer.stagingQuotaBytes': firstError<number | null>(
            form.dtQuota,
            minimumGib('The staging quota'),
          ),
          'dataTransfer.maxConcurrent': firstError<number | null>(
            form.dtConcurrent,
            integerBetween('The number of transfers', 1, 16),
          ),
        }
      : {}),
  }),
)

/** A size in gibibytes that is at least 1 MiB (the contract's minimum), a number with at most three decimals. */
function minimumGib(label: string): (value: number | null) => string | undefined {
  return (value) =>
    typeof value !== 'number' || !Number.isFinite(value) || value < 0.001
      ? `${label} is at least 0.001 GiB (1 MiB).`
      : undefined
}

/**
 * Only what the user changed. A PATCH is a partial update: the contract refuses an empty group
 * (`features: {}`) and an empty body, so unchanged groups are left out entirely.
 */
function changes(): ClusterSettingsUpdateRequest {
  const d = s.value
  const body: ClusterSettingsUpdateRequest = {}
  if (!d) return body
  if (form.replicas !== d.defaultReplicaCount) body.defaultReplicaCount = form.replicas
  if (form.durability !== d.defaultDurability) body.defaultDurability = form.durability
  const af: NonNullable<ClusterSettingsUpdateRequest['autoFailover']> = {}
  if (form.afEnabled !== d.autoFailover.enabled) af.enabled = form.afEnabled
  if (form.afTimeout !== d.autoFailover.timeoutSeconds) af.timeoutSeconds = form.afTimeout
  if (form.afMax !== d.autoFailover.maxCount) af.maxCount = form.afMax
  if (Object.keys(af).length) body.autoFailover = af
  if (form.maxMoves !== d.rebalance.maxConcurrentMoves)
    body.rebalance = { maxConcurrentMoves: form.maxMoves }
  // A feature can be turned on, never off.
  const features: NonNullable<ClusterSettingsUpdateRequest['features']> = {}
  if (form.sharding && !d.features.sharding) features.sharding = true
  if (form.ledger && !d.features.ledger) features.ledger = true
  if (Object.keys(features).length) body.features = features
  const dt = d.dataTransfer
  if (dt) {
    const update: NonNullable<ClusterSettingsUpdateRequest['dataTransfer']> = {}
    if (form.dtEnabled !== dt.enabled) update.enabled = form.dtEnabled
    if (form.dtRetention !== dt.retentionHours) update.retentionHours = form.dtRetention
    // A size is sent only when the person changed what the field shows (the field rounds to three decimals).
    if (form.dtMaxFile !== toGib(dt.maxFileBytes)) update.maxFileBytes = fromGib(form.dtMaxFile)
    if (form.dtQuota !== toGib(dt.stagingQuotaBytes))
      update.stagingQuotaBytes = fromGib(form.dtQuota)
    if (form.dtConcurrent !== dt.maxConcurrent) update.maxConcurrent = form.dtConcurrent
    if (Object.keys(update).length) body.dataTransfer = update
  }
  return body
}

const unchanged = ref(false)
const save = useMutation({
  mutationFn: () => endpoints.updateClusterSettings(changes()),
  onSuccess: async () => {
    saved.value = true
    dirty.value = false
    confirmingFeatures.value = null
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
  unchanged.value = false
  if (Object.keys(errors.value).length > 0) return
  const body = changes()
  if (Object.keys(body).length === 0) {
    unchanged.value = true
    return
  }
  // A feature cannot be turned off again, so turning one on is confirmed first.
  const turnedOn = Object.keys(body.features ?? {})
  if (turnedOn.length > 0) {
    confirmingFeatures.value = turnedOn
    return
  }
  save.mutate()
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
    <form
      v-if="s"
      v-block-enter-submit
      class="stack-lg"
      style="max-width: 720px"
      novalidate
      @submit.prevent="submit"
      @input="dirty = true"
      @change="dirty = true"
    >
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
          hint="Standby copies of every shard group, 0 to 3. Saving a replica count starts the task that adds or removes standbys, one group at a time (it is not a rebalance); follow it on the Replication page."
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
          hint="30 to 3600. A server must be silent this long. A shorter time cannot tell a failure from a pause, and the cluster waits at least three times the time it takes to call a server failed, so a failover can start later than this."
          :error="err('autoFailover.timeoutSeconds')"
        >
          <input
            v-bind="slot"
            v-model.number="form.afTimeout"
            class="input num"
            type="number"
            min="30"
            max="3600"
          />
        </FormField>
        <FormField
          v-slot="slot"
          label="Maximum automatic failovers"
          hint="1 to 100. After this many events further failovers need an operator. A rebalance that completes resets the counter."
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
        <AutoFailoverCounter :auto-failover="s.autoFailover" />
        <div data-testid="af-requirements">
          <p class="muted">Automatic failover needs:</p>
          <ul class="muted">
            <li v-for="r in AUTO_FAILOVER_REQUIREMENTS" :key="r">{{ r }}</li>
          </ul>
        </div>
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
      <fieldset v-if="s.dataTransfer" data-testid="data-transfer-settings">
        <legend>Data transfer</legend>
        <label class="check"
          ><input v-model="form.dtEnabled" type="checkbox" /> Allow table export and import (CSV
          files)</label
        >
        <p class="hint muted" data-testid="data-transfer-state">
          <template v-if="s.dataTransfer.enabled"
            >On. Administrators can export a table to a CSV file and import a CSV file into a table.
            Files are kept on the manager leader's disk until their retention time ends.</template
          >
          <template v-else
            >Off. Every transfer operation is refused, and the Export CSV and Import CSV actions are
            disabled with this reason. Turning it on is recorded in the audit log.</template
          >
          A transfer works only over HTTPS. An import is all or nothing and takes at most
          {{ formatBytes(s.dataTransfer.maxAtomicBytes ?? MAX_ATOMIC_BYTES) }} of data (256 MiB for
          a reference table); split a larger file.
        </p>
        <FormField
          v-slot="slot"
          label="Retention (hours)"
          hint="1 to 168. How long an export file is kept by default. An upload that receives nothing for 48 hours is deleted."
          :error="err('dataTransfer.retentionHours')"
        >
          <input
            v-bind="slot"
            v-model.number="form.dtRetention"
            class="input num"
            type="number"
            min="1"
            max="168"
          />
        </FormField>
        <FormField
          v-slot="slot"
          label="Largest file (GiB)"
          hint="No staged file, upload or export, is larger. At least 0.001 (1 MiB)."
          :error="err('dataTransfer.maxFileBytes')"
        >
          <input
            v-bind="slot"
            v-model.number="form.dtMaxFile"
            class="input num"
            type="number"
            min="0.001"
            step="any"
          />
        </FormField>
        <FormField
          v-slot="slot"
          label="Staging quota (GiB)"
          hint="The room all staged files together may take on the disk. A transfer that does not fit is refused."
          :error="err('dataTransfer.stagingQuotaBytes')"
        >
          <input
            v-bind="slot"
            v-model.number="form.dtQuota"
            class="input num"
            type="number"
            min="0.001"
            step="any"
          />
        </FormField>
        <FormField
          v-slot="slot"
          label="Transfers at once"
          hint="1 to 16. Transfers that run in the cluster at the same time; one per role."
          :error="err('dataTransfer.maxConcurrent')"
        >
          <input
            v-bind="slot"
            v-model.number="form.dtConcurrent"
            class="input num"
            type="number"
            min="1"
            max="16"
          />
        </FormField>
      </fieldset>
      <ProblemNotice v-if="save.error.value" :error="save.error.value" />
      <p v-if="unchanged" class="notice tone-info" role="status">
        <span aria-hidden="true">&#9432;</span> Nothing was changed, so nothing was saved.
      </p>
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
  <div style="max-width: 720px; margin-top: var(--s-5)">
    <AnchorSettings />
  </div>
  <ConfirmDialog
    v-if="confirmingFeatures"
    :title="`Turn on ${confirmingFeatures.join(' and ')} for the whole cluster?`"
    confirm-label="Turn on permanently"
    danger
    :busy="save.isPending.value"
    @cancel="confirmingFeatures = null"
    @confirm="save.mutate()"
  >
    <p>
      This cannot be turned off again. The first time a feature is enabled, every server needs a
      restart before it is loaded.
    </p>
    <ProblemNotice v-if="save.error.value" :error="save.error.value" />
  </ConfirmDialog>
</template>
