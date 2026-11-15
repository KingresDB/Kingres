<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// A usage bar. When the server cannot tell (null), no bar is drawn and "n/a" is shown, never 0.
import { computed } from 'vue'
import NotAvailable from './NotAvailable.vue'

const props = defineProps<{
  label: string
  /** Accessible name of the meter. */
  name: string
  /** Fraction source: `used` of `total`, or a percentage when `total` is 100. */
  used: number | null
  total: number | null
  format: (v: number) => string
  /** Show "x of y" (default) or only the used part followed by a percent sign. */
  percent?: boolean
}>()

const known = computed(() => props.used !== null && props.total !== null && props.total > 0)
const ratio = computed(() =>
  known.value ? Math.min(100, ((props.used as number) / (props.total as number)) * 100) : 0,
)
</script>

<template>
  <div>
    <div class="row-between">
      <span
        ><slot name="label">{{ label }}</slot></span
      >
      <span v-if="known" class="num">
        <template v-if="percent">{{ format(used as number) }}</template>
        <template v-else>{{ format(used as number) }} of {{ format(total as number) }}</template>
      </span>
      <NotAvailable v-else />
    </div>
    <div
      v-if="known"
      class="progress"
      role="meter"
      :aria-label="name"
      aria-valuemin="0"
      aria-valuemax="100"
      :aria-valuenow="Math.round(ratio)"
    >
      <span :style="{ width: `${ratio}%` }" />
    </div>
  </div>
</template>
