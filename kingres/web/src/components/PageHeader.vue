<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
defineProps<{ title: string; subtitle?: string }>()
</script>

<template>
  <header class="row-between page-head">
    <div class="stack" style="gap: var(--s-1)">
      <h1>{{ title }}</h1>
      <p v-if="subtitle" class="muted">{{ subtitle }}</p>
    </div>
    <div class="row"><slot name="actions" /></div>
  </header>
</template>

<style scoped>
.page-head {
  margin-bottom: var(--s-4);
}
</style>
