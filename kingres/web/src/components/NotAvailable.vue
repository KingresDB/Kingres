<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The contract returns `null` for a value the server cannot know yet. It is shown as a neutral
// "n/a", never as 0, and screen readers hear "Not available".
</script>

<template>
  <span class="na" data-testid="na"
    ><span aria-hidden="true">n/a</span><span class="sr-only">Not available</span></span
  >
</template>

<style scoped>
.na {
  color: var(--c-text-muted);
  font-style: italic;
}
</style>
