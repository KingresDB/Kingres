<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// A parameter value as the server sent it. A value is chosen by whoever may set the parameter (an extension's
// placeholder setting can be set by a database role), so it goes through SafeName: hidden characters become visible,
// a very long value is folded. It is never markup, never an id and never the target of a label.
import NotAvailable from '@/components/NotAvailable.vue'
import SafeName from '@/components/SafeName.vue'

withDefaults(
  defineProps<{
    value: string | null | undefined
    /** PostgreSQL hides this value from ordinary database users: no role is ever shown it. */
    hidden?: boolean
    max?: number
  }>(),
  { hidden: false, max: 80 },
)
</script>

<template>
  <span v-if="hidden" class="muted" data-testid="value-hidden"
    ><span aria-hidden="true">&#128274;</span> hidden</span
  >
  <NotAvailable v-else-if="value === null || value === undefined" />
  <span v-else-if="value === ''" class="muted">(empty)</span>
  <code v-else class="config-value"><SafeName :value="value" kind="text" :max="max" /></code>
</template>

<style scoped>
.config-value {
  overflow-wrap: anywhere;
}
</style>
