<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The refusal of an action of the Client keys tab in the words of its problem code (`clientRefusal`); a code without words of its
// own falls back to the generic notice. The server's own detail is shown below the words, never instead of them.
import { computed } from 'vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import { clientRefusal, type ClientAction } from '@/lib/ledger-client'
import { opsRefusal, type OpsAction } from '@/lib/ledger-ops'
import { ApiError } from '@/api/problem'

// `action` is an action of the Client keys tab; `ops` an action of Console D (bundle, acknowledge, verify): the words differ.
const props = defineProps<{ error: unknown; action?: ClientAction; ops?: OpsAction }>()
const emit = defineEmits<{ retry: [] }>()
const refusal = computed(() =>
  props.ops
    ? opsRefusal(props.error, props.ops)
    : clientRefusal(props.error, props.action ?? 'list'),
)
const detail = computed(() =>
  props.error instanceof ApiError && refusal.value?.withDetail ? props.error.problem.detail : null,
)
const icon = computed(() =>
  refusal.value?.tone === 'info' ? 'ⓘ' : refusal.value?.tone === 'warning' ? '▲' : '✖',
)
</script>

<template>
  <div
    v-if="refusal"
    class="notice"
    :class="`tone-${refusal.tone === 'warning' ? 'warn' : refusal.tone}`"
    :role="refusal.tone === 'info' ? 'status' : 'alert'"
    :data-code="(error as ApiError).code"
    data-testid="ledger-refusal"
  >
    <span aria-hidden="true">{{ icon }}</span>
    <div class="stack" style="gap: var(--s-1)">
      <strong>{{ refusal.heading }}</strong>
      <span>{{ refusal.message }}</span>
      <span v-if="detail" class="muted"
        ><SafeName :value="detail" kind="text" :max="300" multiline
      /></span>
    </div>
  </div>
  <ProblemNotice v-else :error="error" @retry="emit('retry')" />
</template>
