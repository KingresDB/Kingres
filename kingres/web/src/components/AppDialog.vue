<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Modal dialog: traps Tab focus, closes on Escape, and returns focus to the element that opened it.
// Mount it with v-if; it is open for as long as it exists.
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

const props = withDefaults(
  defineProps<{
    title: string
    /** Blocks Escape and the close button, for example while a request is running. */
    persistent?: boolean
    wide?: boolean
  }>(),
  { persistent: false, wide: false },
)
const emit = defineEmits<{ close: [] }>()

const panel = ref<HTMLElement | null>(null)
const titleId = `dlg-${Math.random().toString(36).slice(2, 9)}`
let opener: HTMLElement | null = null

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'

function focusables(): HTMLElement[] {
  return Array.from(panel.value?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? []).filter(
    (el) => el.getAttribute('aria-hidden') !== 'true',
  )
}

function requestClose(): void {
  if (!props.persistent) emit('close')
}

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') {
    event.stopPropagation()
    requestClose()
    return
  }
  if (event.key !== 'Tab') return
  const items = focusables()
  if (items.length === 0) {
    event.preventDefault()
    panel.value?.focus()
    return
  }
  const first = items[0] as HTMLElement
  const last = items[items.length - 1] as HTMLElement
  const active = document.activeElement
  if (event.shiftKey && (active === first || active === panel.value)) {
    event.preventDefault()
    last.focus()
  } else if (!event.shiftKey && active === last) {
    event.preventDefault()
    first.focus()
  }
}

onMounted(async () => {
  opener = document.activeElement instanceof HTMLElement ? document.activeElement : null
  await nextTick()
  // Prefer the first form control; fall back to the first focusable, then the panel itself.
  const target =
    panel.value?.querySelector<HTMLElement>('[data-autofocus], input:not([disabled]), select') ??
    focusables().find((el) => !el.hasAttribute('data-dialog-close')) ??
    panel.value
  target?.focus()
})

onBeforeUnmount(() => {
  opener?.focus()
})
</script>

<template>
  <Teleport to="body">
    <!-- A click on the dimmed area closes the dialog; keyboard users have Escape and the close button. -->
    <!-- eslint-disable-next-line vuejs-accessibility/no-static-element-interactions -->
    <div class="dlg-backdrop" @mousedown.self="requestClose">
      <!-- Focus is trapped inside the dialog, so Tab/Escape are handled on its root. -->
      <!-- eslint-disable-next-line vuejs-accessibility/no-static-element-interactions -->
      <div
        ref="panel"
        class="dlg"
        :class="{ wide }"
        role="dialog"
        aria-modal="true"
        :aria-labelledby="titleId"
        tabindex="-1"
        @keydown="onKeydown"
      >
        <div class="dlg-head">
          <h2 :id="titleId">{{ title }}</h2>
          <button
            type="button"
            class="btn btn-small"
            data-dialog-close
            :disabled="persistent"
            aria-label="Close dialog"
            @click="requestClose"
          >
            <span aria-hidden="true">&#10005;</span>
          </button>
        </div>
        <div class="dlg-body"><slot /></div>
        <div v-if="$slots.footer" class="dlg-foot"><slot name="footer" /></div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.dlg-backdrop {
  position: fixed;
  inset: 0;
  z-index: 100;
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding: var(--s-6) var(--s-4);
  background: var(--c-overlay);
  overflow-y: auto;
}
.dlg {
  width: min(520px, 100%);
  background: var(--c-surface);
  color: var(--c-text);
  border: 1px solid var(--c-border-strong);
  border-radius: var(--radius);
  box-shadow: var(--shadow);
}
.dlg.wide {
  width: min(760px, 100%);
}
.dlg:focus {
  outline: none;
}
.dlg-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--s-3);
  padding: var(--s-3) var(--s-4);
  border-bottom: 1px solid var(--c-border);
}
.dlg-head h2 {
  font-size: var(--fs-lg);
}
.dlg-body {
  padding: var(--s-4);
  display: flex;
  flex-direction: column;
  gap: var(--s-3);
}
.dlg-foot {
  display: flex;
  justify-content: flex-end;
  gap: var(--s-2);
  padding: var(--s-3) var(--s-4);
  border-top: 1px solid var(--c-border);
}
</style>
