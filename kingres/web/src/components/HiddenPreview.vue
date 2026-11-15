<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// A text field cannot draw hidden characters as escapes, and a bidirectional control inside it
// reorders what the person reads. When the value of a field holds such characters, this shows the
// same value with every one of them made visible, below the field.
import SafeName from './SafeName.vue'
import { hasHidden } from '@/lib/safe-text'

defineProps<{ value: string }>()
</script>

<template>
  <span v-if="hasHidden(value)" class="hint muted">
    With hidden characters shown: <SafeName :value="value" kind="text" :max="120" />
  </span>
</template>
