<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Confirmation for consequential actions. The default slot must state the consequence in plain
// words. With `typeToConfirm` the user has to type that text before the button is enabled.
import { computed, ref } from 'vue'
import AppDialog from './AppDialog.vue'
import TypeToConfirm from './TypeToConfirm.vue'

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
const ready = computed(
  () => props.typeToConfirm === undefined || typed.value === props.typeToConfirm,
)
</script>

<template>
  <AppDialog :title="title" :persistent="busy" @close="emit('cancel')">
    <slot />
    <TypeToConfirm v-if="typeToConfirm !== undefined" v-model="typed" :expected="typeToConfirm" />
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
