<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import SafeName from './SafeName.vue'

defineProps<{ title: string; subtitle?: string }>()
</script>

<template>
  <header class="row-between page-head">
    <div class="stack" style="gap: var(--s-1)">
      <!-- eslint-disable-next-line vuejs-accessibility/heading-has-content -->
      <h1><SafeName :value="title" kind="text" :max="120" /></h1>
      <p v-if="subtitle" class="muted"><SafeName :value="subtitle" kind="text" :max="300" /></p>
    </div>
    <div class="row"><slot name="actions" /></div>
  </header>
</template>

<style scoped>
.page-head {
  margin-bottom: var(--s-4);
}
</style>
