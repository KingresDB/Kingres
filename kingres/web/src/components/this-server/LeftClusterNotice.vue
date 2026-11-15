<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Shown once on the setup wizard after this server left the cluster and reset itself.
import { watch } from 'vue'
import { useRoute } from 'vue-router'
import { availability } from '@/api/availability'

const route = useRoute()
// Any later page than the wizard ends the notice.
watch(
  () => route.name,
  (name) => {
    if (name !== 'setup') availability.leftNotice = false
  },
)
</script>

<template>
  <p
    v-if="availability.leftNotice && route.name === 'setup'"
    class="notice tone-info"
    role="status"
    style="margin: var(--s-4)"
  >
    <span aria-hidden="true">&#9432;</span>
    <span
      >This server left the cluster and was reset. Set it up again to create or join a
      cluster.</span
    >
    <button type="button" class="btn btn-small" @click="availability.leftNotice = false">
      Dismiss
    </button>
  </p>
</template>
