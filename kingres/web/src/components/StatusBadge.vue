<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// A status is always shown as icon + text, never as colour alone.
import type { StatusMeta } from '@/lib/status'

defineProps<{ meta: StatusMeta; label?: string }>()
</script>

<template>
  <span class="badge" :class="`tone-${meta.tone}`" :data-tone="meta.tone" :title="meta.hint">
    <span aria-hidden="true">{{ meta.icon }}</span>
    <span>{{ label ?? meta.label }}</span>
  </span>
</template>
