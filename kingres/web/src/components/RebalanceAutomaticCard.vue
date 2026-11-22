<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The automatic rebalance (contract 0.16.0): off by default, changed by an administrator only (the button is a GateButton),
// shown to everyone who can read the cluster settings. The card says what the mode needs before it is switched on.
import { computed, reactive, ref, watch } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import GateButton from './GateButton.vue'
import FormField from './FormField.vue'
import ProblemNotice from './ProblemNotice.vue'
import SafeName from './SafeName.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useClusterSettings } from '@/api/queries'
import { formatDateTime } from '@/lib/format'
import type { RebalanceStatus } from '@/api/types'

defineProps<{ automatic?: RebalanceStatus['automatic'] }>()
const client = useQueryClient()
const settings = useClusterSettings()
const cur = computed(() => settings.data.value?.rebalance.automatic)

const editing = ref(false)
const form = reactive({
  strategy: 'byBucketCount',
  threshold: 0.15,
  settleSeconds: 300,
  minIntervalSeconds: 3600,
  windowOn: false,
  from: '22:00',
  to: '06:00',
  timeZone: 'UTC',
})
watch(
  cur,
  (a) => {
    if (!a) return
    form.strategy = a.strategy
    form.threshold = a.threshold
    form.settleSeconds = a.settleSeconds
    form.minIntervalSeconds = a.minIntervalSeconds
    form.windowOn = a.window !== null
    if (a.window)
      Object.assign(form, { from: a.window.from, to: a.window.to, timeZone: a.window.timeZone })
  },
  { immediate: true },
)

const save = useMutation({
  mutationFn: (enabled: boolean | undefined) =>
    endpoints.updateClusterSettings({
      rebalance: {
        automatic: {
          ...(enabled === undefined ? {} : { enabled }),
          strategy: form.strategy as 'byBucketCount' | 'byDiskSize',
          threshold: form.threshold,
          settleSeconds: form.settleSeconds,
          minIntervalSeconds: form.minIntervalSeconds,
          window: form.windowOn ? { from: form.from, to: form.to, timeZone: form.timeZone } : null,
        },
      },
    }),
  onSuccess: () => {
    editing.value = false
    void client.invalidateQueries({ queryKey: keys.clusterSettings })
    void client.invalidateQueries({ queryKey: keys.rebalance })
  },
})

const OUTCOME: Record<string, string> = {
  started: 'A run was started.',
  nothingToDo: 'The buckets are balanced, or a run is under way.',
  blocked: 'Held back by a rule.',
  settling: 'The imbalance is too young or too soon after a change.',
}
const RULE: Record<string, string> = {
  window: 'outside the time window',
  failover: 'a failover is running or has just ended',
  groupDegraded: 'a shard group has fewer healthy standbys than the replica count',
  groupUnavailable: 'a shard group has no primary',
  servers: 'a server is not live, or a change of the servers is unfinished',
  moveFailed: 'an earlier automatic run failed and nobody has looked at it',
  consistency: 'the cluster check reports a problem',
  operatorStopped: 'an operator stopped the run',
  running: 'a run is under way',
}
</script>

<template>
  <section class="card stack" aria-labelledby="auto-h" data-testid="automatic-card">
    <div class="row-between">
      <h2 id="auto-h">Automatic rebalance</h2>
      <span
        class="badge"
        :class="cur?.enabled ? 'tone-ok' : 'tone-neutral'"
        data-testid="auto-state"
      >
        {{ cur?.enabled ? 'On' : 'Off' }}
      </span>
    </div>
    <p>
      When on, the manager starts a rebalance by itself once the buckets have been uneven for a
      while and nothing makes a move unsafe. It never starts during a failover, while a server is
      not live, while a shard group lacks a healthy standby, or after a failed run until an operator
      has looked. It is off by default.
    </p>
    <template v-if="cur?.enabled && automatic">
      <p v-if="automatic.lastEvaluation" data-testid="auto-last">
        Last look {{ formatDateTime(automatic.lastEvaluation.at) }}:
        {{ OUTCOME[automatic.lastEvaluation.outcome] ?? automatic.lastEvaluation.outcome }}
        <template v-if="automatic.lastEvaluation.blockedBy.length">
          ({{ automatic.lastEvaluation.blockedBy.map((r) => RULE[r] ?? r).join('; ') }})
        </template>
        <template v-if="automatic.nextCheckAt">
          Next look {{ formatDateTime(automatic.nextCheckAt) }}.</template
        >
      </p>
    </template>
    <dl v-if="cur" class="kv">
      <dt>Strategy</dt>
      <dd>{{ cur.strategy === 'byDiskSize' ? 'By disk size' : 'By bucket count' }}</dd>
      <dt>Threshold</dt>
      <dd>{{ Math.round(cur.threshold * 100) }} percent from the ideal</dd>
      <dt>Settle time</dt>
      <dd>
        {{ cur.settleSeconds }} s ({{ cur.settleAfterTopologySeconds }} s after a change of the
        servers)
      </dd>
      <dt>Time window</dt>
      <dd>
        <template v-if="cur.window"
          >{{ cur.window.from }} to {{ cur.window.to }}
          <SafeName :value="cur.window.timeZone" :max="60" kind="text"
        /></template>
        <template v-else>Any time</template>
      </dd>
    </dl>
    <div class="row">
      <GateButton op="updateClusterSettings" data-testid="auto-edit" @click="editing = !editing">
        {{ editing ? 'Hide settings' : 'Settings' }}
      </GateButton>
      <GateButton
        op="updateClusterSettings"
        :variant="cur?.enabled ? 'default' : 'primary'"
        :disabled="save.isPending.value || !cur"
        data-testid="auto-toggle"
        @click="save.mutate(!cur?.enabled)"
      >
        {{ cur?.enabled ? 'Turn off' : 'Turn on' }}
      </GateButton>
    </div>
    <form
      v-if="editing"
      class="stack"
      data-testid="auto-form"
      @submit.prevent="save.mutate(undefined)"
    >
      <FormField v-slot="slot" label="Strategy">
        <select v-bind="slot" v-model="form.strategy">
          <option value="byBucketCount">By bucket count</option>
          <option value="byDiskSize">By disk size</option>
        </select>
      </FormField>
      <FormField v-slot="slot" label="Threshold" hint="0.15 is 15 percent; above 0, at most 1.">
        <input
          v-bind="slot"
          v-model.number="form.threshold"
          class="input num"
          type="number"
          min="0.01"
          max="1"
          step="0.01"
        />
      </FormField>
      <FormField v-slot="slot" label="Settle time (seconds)">
        <input
          v-bind="slot"
          v-model.number="form.settleSeconds"
          class="input num"
          type="number"
          min="0"
        />
      </FormField>
      <FormField v-slot="slot" label="Least time between runs (seconds)">
        <input
          v-bind="slot"
          v-model.number="form.minIntervalSeconds"
          class="input num"
          type="number"
          min="0"
        />
      </FormField>
      <label class="check"
        ><input v-model="form.windowOn" type="checkbox" /> Only start inside a time window</label
      >
      <template v-if="form.windowOn">
        <FormField v-slot="slot" label="From (HH:MM)"
          ><input v-bind="slot" v-model="form.from" class="input"
        /></FormField>
        <FormField v-slot="slot" label="To (HH:MM)"
          ><input v-bind="slot" v-model="form.to" class="input"
        /></FormField>
        <FormField v-slot="slot" label="Time zone"
          ><input v-bind="slot" v-model="form.timeZone" class="input"
        /></FormField>
      </template>
      <ProblemNotice v-if="save.error.value" :error="save.error.value" />
      <div class="row">
        <button type="submit" class="btn btn-primary" :disabled="save.isPending.value">
          Save settings
        </button>
      </div>
    </form>
  </section>
</template>

<style scoped>
.kv {
  display: grid;
  grid-template-columns: fit-content(45%) minmax(0, 1fr);
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dd {
  margin: 0;
}
</style>
