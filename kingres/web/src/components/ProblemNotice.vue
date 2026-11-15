<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Shows an API error. The wording comes from describeProblem (keyed on Problem.code).
// "Not available on this server version yet" is shown as calm information, not as a failure.
import { computed } from 'vue'
import SafeName from './SafeName.vue'
import { safeText } from '@/lib/safe-text'
import { describeProblem } from '@/api/problem'

const props = defineProps<{ error: unknown }>()
const emit = defineEmits<{ retry: [] }>()

const view = computed(() => describeProblem(props.error))
const icon = computed(() =>
  view.value.tone === 'info' ? 'ⓘ' : view.value.tone === 'warning' ? '▲' : '✖',
)
const toneClass = computed(() => `tone-${view.value.tone === 'warning' ? 'warn' : view.value.tone}`)
</script>

<template>
  <div
    class="notice"
    :class="toneClass"
    :role="view.tone === 'info' ? 'status' : 'alert'"
    :data-code="view.code"
  >
    <span aria-hidden="true">{{ icon }}</span>
    <div class="stack" style="gap: var(--s-1)">
      <strong>{{ view.heading }}</strong>
      <span><SafeName :value="view.message" kind="text" :max="600" multiline /></span>
      <span v-if="view.retryAfterSeconds !== null" data-retry-after>
        The server asks you to wait {{ view.retryAfterSeconds }}
        {{ view.retryAfterSeconds === 1 ? 'second' : 'seconds' }} before you try again.
      </span>
      <span v-if="view.sqlState" class="mono">SQLSTATE {{ safeText(view.sqlState, 10) }}</span>
      <ul v-if="Object.keys(view.fieldErrors).length" style="margin: 0; padding-left: 1.2em">
        <li v-for="(msg, field) in view.fieldErrors" :key="field">
          <code><SafeName :value="String(field)" kind="text" :max="80" /></code>:
          <SafeName :value="msg" kind="text" :max="300" />
        </li>
      </ul>
      <span v-if="view.requestId" class="muted"
        >Request id: <code><SafeName :value="view.requestId" kind="text" :max="80" /></code
      ></span>
      <div v-if="view.retryable">
        <button type="button" class="btn btn-small" @click="emit('retry')">Try again</button>
      </div>
    </div>
  </div>
</template>
