<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The settings of anchoring on the Settings page (contract 0.11.0, design 6.1, 6.6, 6.9 and 8.5): how often the newest
// aggregate checkpoint is sent to the targets (60 to 86400 seconds), how many targets must have anchored it before it
// counts as anchored (1 to 8), and whether the absence of an anchor raises a warning instead of a notice. They have a form
// and a button of their own because they are saved by another operation (`updateLedgerSettings`, `kingres_admin`).
import { computed, reactive, ref, watch } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import FormField from '@/components/FormField.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys, useLedgerSettings } from '@/api/queries'
import type { LedgerSettingsUpdate } from '@/api/types'

const client = useQueryClient()
const settings = useLedgerSettings()
const anchor = computed(() => settings.data.value?.anchor)
const form = reactive({ interval: '', minimum: '', required: false })
const dirty = ref(false)
const errors = ref<Record<string, string>>({})
const saved = ref(false)
const unchanged = ref(false)

watch(
  anchor,
  (a) => {
    if (a && !dirty.value) {
      form.interval = String(a.intervalSeconds)
      form.minimum = String(a.minimumAnchored)
      form.required = a.required
    }
  },
  { immediate: true },
)

const save = useMutation({
  mutationFn: (body: LedgerSettingsUpdate) => endpoints.updateLedgerSettings(body),
  onSuccess: async () => {
    dirty.value = false
    saved.value = true
    await Promise.all([
      client.invalidateQueries({ queryKey: keys.ledgerSettings }),
      client.invalidateQueries({ queryKey: keys.ledgerStatus }),
    ])
  },
})
const serverErrors = computed(() =>
  save.error.value ? describeProblem(save.error.value).fieldErrors : {},
)
const errorFor = (field: string): string | undefined =>
  errors.value[field] ?? serverErrors.value[field]

function submit(): void {
  saved.value = false
  unchanged.value = false
  const e: Record<string, string> = {}
  const interval = Number(form.interval)
  const minimum = Number(form.minimum)
  if (!/^\d+$/.test(form.interval) || interval < 60 || interval > 86400)
    e['anchor.intervalSeconds'] = 'Enter a whole number of seconds from 60 to 86400.'
  if (!/^\d+$/.test(form.minimum) || minimum < 1 || minimum > 8)
    e['anchor.minimumAnchored'] = 'Enter a whole number from 1 to 8.'
  errors.value = e
  const a = anchor.value
  if (Object.keys(e).length > 0 || !a) return
  const change: NonNullable<LedgerSettingsUpdate['anchor']> = {}
  if (interval !== a.intervalSeconds) change.intervalSeconds = interval
  if (minimum !== a.minimumAnchored) change.minimumAnchored = minimum
  if (form.required !== a.required) change.required = form.required
  if (Object.keys(change).length === 0) {
    unchanged.value = true
    return
  }
  save.mutate({ anchor: change })
}
</script>

<template>
  <section class="card stack" aria-labelledby="anchor-set-h" data-testid="anchor-settings-form">
    <h2 id="anchor-set-h">Ledger anchoring</h2>
    <p class="muted">
      How the newest aggregate checkpoint of every ledger reaches the anchor targets. The targets
      themselves are on the Anchoring tab of the Ledger page.
    </p>
    <ProblemNotice v-if="settings.error.value" :error="settings.error.value" />
    <form
      v-else-if="anchor"
      class="stack"
      novalidate
      @submit.prevent="submit"
      @input="dirty = true"
    >
      <FormField
        v-slot="slot"
        label="Anchor every (seconds)"
        hint="60 to 86400. The default is 3600. Each used target receives the newest checkpoint this often, and on request."
        :error="errorFor('anchor.intervalSeconds')"
      >
        <input
          v-bind="slot"
          v-model="form.interval"
          class="input num"
          inputmode="numeric"
          autocomplete="off"
        />
      </FormField>
      <FormField
        v-slot="slot"
        label="Targets that must have anchored"
        hint="1 to 8. A checkpoint counts as anchored when at least this many targets have anchored it."
        :error="errorFor('anchor.minimumAnchored')"
      >
        <input
          v-bind="slot"
          v-model="form.minimum"
          class="input num"
          inputmode="numeric"
          autocomplete="off"
        />
      </FormField>
      <label class="check">
        <input v-model="form.required" type="checkbox" data-testid="anchor-required" />
        Anchoring is required
        <span class="muted"
          >The absence of an anchor then raises a warning instead of a notice. Nothing is blocked
          either way.</span
        >
      </label>
      <ProblemNotice v-if="save.error.value" :error="save.error.value" />
      <p v-if="unchanged" class="notice tone-info" role="status">
        <span aria-hidden="true">&#9432;</span> Nothing was changed, so nothing was saved.
      </p>
      <p v-if="saved" class="notice tone-ok" role="status" data-testid="anchor-settings-saved">
        <span aria-hidden="true">&#10003;</span> Anchoring settings saved.
      </p>
      <div class="row">
        <GateButton
          op="updateLedgerSettings"
          type="submit"
          variant="primary"
          :disabled="save.isPending.value"
          >Save anchoring settings</GateButton
        >
      </div>
    </form>
    <p v-else class="muted" role="status">Loading.</p>
  </section>
</template>
