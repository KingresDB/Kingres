<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts" generic="T">
// Shows a value that may be null ("not available"), formatted with `format`.
import NotAvailable from './NotAvailable.vue'

const props = defineProps<{ value: T | null | undefined; format?: (value: T) => string }>()

function text(v: T): string {
  return props.format ? props.format(v) : String(v)
}
</script>

<template>
  <NotAvailable v-if="value === null || value === undefined" />
  <template v-else>{{ text(value) }}</template>
</template>
