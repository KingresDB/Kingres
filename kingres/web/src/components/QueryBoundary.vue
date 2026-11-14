<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Wraps the content of a page or card and handles the four non-happy states in one place:
// loading, error (including "not available on this server version yet"), and empty.
import ProblemNotice from './ProblemNotice.vue'

withDefaults(
  defineProps<{
    loading: boolean
    error: unknown
    empty?: boolean
    emptyTitle?: string
    emptyHint?: string
  }>(),
  { empty: false, emptyTitle: 'Nothing here yet', emptyHint: '' },
)
const emit = defineEmits<{ retry: [] }>()
</script>

<template>
  <div v-if="loading" class="skeleton" role="status" aria-live="polite">
    <span class="sr-only">Loading</span>
    <span class="bar" style="width: 60%" />
    <span class="bar" style="width: 90%" />
    <span class="bar" style="width: 75%" />
  </div>
  <ProblemNotice v-else-if="error" :error="error" @retry="emit('retry')" />
  <div v-else-if="empty" class="empty">
    <strong>{{ emptyTitle }}</strong>
    <span v-if="emptyHint" class="muted">{{ emptyHint }}</span>
    <slot name="empty-action" />
  </div>
  <slot v-else />
</template>

<style scoped>
.skeleton {
  display: flex;
  flex-direction: column;
  gap: var(--s-2);
  padding: var(--s-3) 0;
}
.bar {
  display: block;
  height: 14px;
  border-radius: var(--radius-sm);
  background: var(--c-surface-2);
}
.empty {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--s-2);
  padding: var(--s-5) var(--s-4);
  border: 1px dashed var(--c-border-strong);
  border-radius: var(--radius);
  background: var(--c-surface);
}
</style>
