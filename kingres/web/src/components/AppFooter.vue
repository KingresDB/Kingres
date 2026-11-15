<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The copyright line at the bottom of every page of the console, with a link to the product's
// site. The link opens in a new tab without giving the site a handle on this page or its address.
</script>

<template>
  <footer class="app-footer">
    <span>Copyright &copy; 2026 RooyeKhat Media Co.</span>
    <a href="https://kingresdb.com" target="_blank" rel="noopener noreferrer"
      >kingresdb.com<span class="sr-only"> (opens in a new tab)</span></a
    >
  </footer>
</template>

<style scoped>
.app-footer {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: center;
  gap: var(--s-1) var(--s-3);
  padding: var(--s-3) var(--s-4);
  font-size: var(--fs-sm);
  color: var(--c-text-muted);
  border-top: 1px solid var(--c-border);
}
</style>
