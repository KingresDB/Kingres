<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The two screen-reader regions of createAnnouncer(). Always rendered, so they exist before text
// is put into them.
defineProps<{ polite: string; assertive: string }>()
</script>

<template>
  <div class="sr-only" aria-live="polite" aria-atomic="true">{{ polite }}</div>
  <div class="sr-only" aria-live="assertive" aria-atomic="true">{{ assertive }}</div>
</template>
