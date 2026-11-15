<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Label, hint and error around one control. The control receives its id and aria attributes
// through the slot scope, so the label is always programmatically associated.
import { computed } from 'vue'

const props = defineProps<{ label: string; hint?: string; error?: string; required?: boolean }>()
const id = `f-${Math.random().toString(36).slice(2, 9)}`
const hintId = computed(() => (props.hint ? `${id}-hint` : undefined))
const errorId = computed(() => (props.error ? `${id}-err` : undefined))
const describedBy = computed(
  () => [errorId.value, hintId.value].filter(Boolean).join(' ') || undefined,
)
</script>

<template>
  <div class="field">
    <label :for="id">{{ label }}<span v-if="required" aria-hidden="true"> *</span></label>
    <slot
      :id="id"
      :aria-describedby="describedBy"
      :aria-invalid="error ? 'true' : undefined"
      :aria-required="required ? 'true' : undefined"
    />
    <span v-if="hint" :id="hintId" class="hint">{{ hint }}</span>
    <span v-if="error" :id="errorId" class="error" role="alert">{{ error }}</span>
  </div>
</template>
