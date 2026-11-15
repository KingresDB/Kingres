<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts" generic="T">
// Shows a value that may be null ("not available"), formatted with `format`.
import NotAvailable from './NotAvailable.vue'
import SafeName from './SafeName.vue'

const props = defineProps<{
  value: T | null | undefined
  format?: (value: T) => string
  /** The value is a name or text from the database: make hidden characters visible. */
  safe?: boolean
}>()

function text(v: T): string {
  return props.format ? props.format(v) : String(v)
}
</script>

<template>
  <NotAvailable v-if="value === null || value === undefined" />
  <SafeName v-else-if="safe" :value="text(value)" />
  <template v-else>{{ text(value) }}</template>
</template>
