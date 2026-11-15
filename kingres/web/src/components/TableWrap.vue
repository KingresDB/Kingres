<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The scrolling box around a table. A box that scrolls must be reachable and scrollable by
// keyboard and must have a name, so it is a focusable region named after the table's caption.
import { onMounted, onUpdated, ref } from 'vue'

const props = defineProps<{ label?: string }>()
const root = ref<HTMLElement | null>(null)
const name = ref<string | undefined>(props.label)

function readCaption(): void {
  if (props.label) {
    name.value = props.label
    return
  }
  name.value = root.value?.querySelector('caption')?.textContent?.trim() || 'Table'
}
onMounted(readCaption)
onUpdated(readCaption)
</script>

<template>
  <!-- A scrollable region has to be focusable so that the keyboard can scroll it. -->
  <!-- eslint-disable-next-line vuejs-accessibility/no-noninteractive-tabindex -->
  <div ref="root" class="table-wrap" role="group" tabindex="0" :aria-label="name">
    <slot />
  </div>
</template>
