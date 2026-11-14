<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Confirmation for consequential actions. The default slot must state the consequence in plain
// words. With `typeToConfirm` the user has to type that text before the button is enabled.
import { computed, ref } from 'vue'
import AppDialog from './AppDialog.vue'

const props = withDefaults(
  defineProps<{
    title: string
    confirmLabel: string
    danger?: boolean
    typeToConfirm?: string
    busy?: boolean
  }>(),
  { danger: false, typeToConfirm: undefined, busy: false },
)
const emit = defineEmits<{ confirm: []; cancel: [] }>()

const typed = ref('')
const inputId = `cf-${Math.random().toString(36).slice(2, 9)}`
const ready = computed(
  () => props.typeToConfirm === undefined || typed.value === props.typeToConfirm,
)
</script>

<template>
  <AppDialog :title="title" :persistent="busy" @close="emit('cancel')">
    <slot />
    <div v-if="typeToConfirm !== undefined" class="field">
      <label :for="inputId">
        Type <code>{{ typeToConfirm }}</code> to confirm
      </label>
      <input
        :id="inputId"
        v-model="typed"
        class="input mono"
        autocomplete="off"
        spellcheck="false"
      />
    </div>
    <template #footer>
      <button type="button" class="btn" :disabled="busy" @click="emit('cancel')">Cancel</button>
      <button
        type="button"
        class="btn"
        :class="danger ? 'btn-danger' : 'btn-primary'"
        :disabled="!ready || busy"
        @click="emit('confirm')"
      >
        {{ confirmLabel }}
      </button>
    </template>
  </AppDialog>
</template>
