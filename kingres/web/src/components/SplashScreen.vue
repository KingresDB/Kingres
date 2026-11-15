<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Shown while the application finds out where to go (setup state, session). The mark identifies the
// product, so it keeps its accessible name.
import BrandLogo from './BrandLogo.vue'
</script>

<template>
  <div class="splash" role="status" aria-live="polite">
    <BrandLogo variant="mark" :size="96" />
    <span class="sr-only">Loading</span>
  </div>
</template>

<style scoped>
.splash {
  min-height: 100vh;
  display: grid;
  place-items: center;
}
</style>
