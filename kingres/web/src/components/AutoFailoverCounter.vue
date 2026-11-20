<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The counter of automatic failovers (design 8.4): "Events since the last reset: n of m", the time of the last event,
// the line that says the limit is reached, and the button that resets the counter (kingres_admin; a confirmation says
// what it allows again). A server that does not know the reset yet answers `unsupported_operation`: the dialog says so
// calmly and nothing changes. The counter itself is only read here; the numbers come from `ClusterSettings`.
import { computed, ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import GateButton from './GateButton.vue'
import ConfirmDialog from './ConfirmDialog.vue'
import ProblemNotice from './ProblemNotice.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { autoFailoverCounterWords } from '@/lib/failover'
import type { ClusterSettings } from '@/api/types'

const props = defineProps<{ autoFailover: ClusterSettings['autoFailover'] }>()
const client = useQueryClient()
const words = computed(() => autoFailoverCounterWords(props.autoFailover))
const confirming = ref(false)
const done = ref(false)

const reset = useMutation({
  mutationFn: () => endpoints.resetAutoFailoverCounter(),
  onSuccess: async () => {
    confirming.value = false
    done.value = true
    await Promise.all([
      client.invalidateQueries({ queryKey: keys.clusterSettings }),
      client.invalidateQueries({ queryKey: keys.cluster }),
    ])
  },
})
function ask(): void {
  done.value = false
  reset.reset()
  confirming.value = true
}
function close(): void {
  confirming.value = false
  reset.reset()
}
</script>

<template>
  <div class="stack" data-testid="auto-failover-counter">
    <p>
      Events since the last reset:
      <strong data-testid="af-counter">{{ words.counter }}</strong>
    </p>
    <p class="muted" data-testid="af-last">{{ words.last }}</p>
    <p v-if="words.limitReached" class="notice tone-warn" role="status" data-testid="af-limit">
      <span aria-hidden="true">&#9650;</span>
      <span>The limit is reached: further failures need an operator.</span>
    </p>
    <p v-if="done" class="notice tone-ok" role="status" data-testid="af-reset-done">
      <span aria-hidden="true">&#10003;</span> The counter was reset.
    </p>
    <div class="row">
      <GateButton
        op="resetAutoFailoverCounter"
        :blocked-reason="words.empty ? 'The counter is at 0 already.' : ''"
        @click="ask"
        >Reset counter</GateButton
      >
    </div>
  </div>
  <ConfirmDialog
    v-if="confirming"
    title="Reset the counter of automatic failovers?"
    confirm-label="Reset counter"
    :busy="reset.isPending.value"
    @cancel="close"
    @confirm="reset.mutate()"
  >
    <p data-testid="af-reset-text">
      Automatic failover will be allowed again up to {{ autoFailover.maxCount }}
      {{ autoFailover.maxCount === 1 ? 'time' : 'times' }}. A failover that was made by a person is
      not counted and stays as it is.
    </p>
    <ProblemNotice v-if="reset.error.value" :error="reset.error.value" />
  </ConfirmDialog>
</template>
