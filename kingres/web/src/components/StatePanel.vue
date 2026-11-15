<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// A page-sized state (not found, not allowed): the mark, a heading and an explanation.
// The mark is decorative: the shell's top bar already names the product on these pages.
import BrandLogo from './BrandLogo.vue'

defineProps<{ title: string }>()
</script>

<template>
  <section class="state-panel" :aria-labelledby="'sp-title'">
    <BrandLogo variant="mark" :size="72" decorative />
    <h1 id="sp-title">{{ title }}</h1>
    <div class="stack" style="align-items: center"><slot /></div>
  </section>
</template>

<style scoped>
.state-panel {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--s-3);
  padding: var(--s-6) var(--s-4);
  text-align: center;
}
</style>
