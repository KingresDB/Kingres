<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Tabs with the WAI-ARIA pattern: roving tabindex, arrow keys, Home and End.
// The parent renders the panel and gives it role="tabpanel" with aria-labelledby="tab-<id>".
import { ref } from 'vue'

const props = defineProps<{ tabs: { id: string; label: string }[]; label: string }>()
const model = defineModel<string>({ required: true })
const buttons = ref<HTMLButtonElement[]>([])

function onKey(event: KeyboardEvent, index: number): void {
  const last = props.tabs.length - 1
  let next: number
  if (event.key === 'ArrowRight') next = index === last ? 0 : index + 1
  else if (event.key === 'ArrowLeft') next = index === 0 ? last : index - 1
  else if (event.key === 'Home') next = 0
  else if (event.key === 'End') next = last
  else return
  event.preventDefault()
  const tab = props.tabs[next]
  if (!tab) return
  model.value = tab.id
  buttons.value[next]?.focus()
}
</script>

<template>
  <div class="tabs" role="tablist" :aria-label="label">
    <button
      v-for="(t, i) in tabs"
      :id="`tab-${t.id}`"
      :key="t.id"
      ref="buttons"
      type="button"
      role="tab"
      class="tab"
      :aria-selected="model === t.id"
      :aria-controls="`panel-${t.id}`"
      :tabindex="model === t.id ? 0 : -1"
      @click="model = t.id"
      @keydown="onKey($event, i)"
    >
      {{ t.label }}
    </button>
  </div>
</template>

<style scoped>
.tabs {
  display: flex;
  gap: var(--s-1);
  border-bottom: 1px solid var(--c-border);
  margin-bottom: var(--s-4);
  overflow-x: auto;
}
.tab {
  font: inherit;
  font-weight: 500;
  padding: var(--s-2) var(--s-3);
  background: none;
  color: var(--c-text-muted);
  border: 0;
  border-bottom: 3px solid transparent;
  cursor: pointer;
  white-space: nowrap;
}
.tab[aria-selected='true'] {
  color: var(--c-text);
  border-bottom-color: var(--c-accent);
}
.tab:hover {
  color: var(--c-text);
}
</style>
