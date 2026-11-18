<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The answer to a failover request that was refused, in words of its own for each problem code (design 8.2, contract
// 0.10.0). What the server says in `detail` is shown below the words and never replaces them. A code without words of
// its own falls back to the general notice. "Not available on this server version yet" is calm information.
import { computed } from 'vue'
import ProblemNotice from './ProblemNotice.vue'
import SafeName from './SafeName.vue'
import { ApiError } from '@/api/problem'
import { failoverRefusal, type RefusalContext } from '@/lib/failover'

const props = defineProps<{ error: unknown; context: RefusalContext }>()
const view = computed(() => failoverRefusal(props.error, props.context))
const detail = computed(() =>
  view.value?.withDetail && props.error instanceof ApiError
    ? (props.error.problem.detail ?? '')
    : '',
)
const icon = computed(() =>
  view.value?.tone === 'info' ? 'ⓘ' : view.value?.tone === 'warning' ? '▲' : '✖',
)
const toneClass = computed(() =>
  view.value?.tone === 'warning' ? 'tone-warn' : `tone-${view.value?.tone ?? 'danger'}`,
)
</script>

<template>
  <div
    v-if="view"
    class="notice"
    :class="toneClass"
    :role="view.tone === 'info' ? 'status' : 'alert'"
    :data-code="view.code"
    data-testid="failover-refusal"
  >
    <span aria-hidden="true">{{ icon }}</span>
    <div class="stack" style="gap: var(--s-1)">
      <strong>{{ view.heading }}</strong>
      <span>{{ view.message }}</span>
      <span v-if="detail" class="muted" data-testid="refusal-detail">
        The server says: <SafeName :value="detail" kind="text" :max="600" multiline />
      </span>
    </div>
  </div>
  <ProblemNotice v-else :error="error" />
</template>
