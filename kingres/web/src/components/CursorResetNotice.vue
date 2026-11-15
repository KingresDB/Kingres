<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Shown after the server refused the position (cursor) a list was reading from, for example because
// the server was updated meanwhile (cursors are valid only for the revision that made them). The
// list started again from the first page; this says so briefly. Announced by usePagedQuery.
defineProps<{ shown: boolean }>()
const emit = defineEmits<{ dismiss: [] }>()
</script>

<template>
  <div v-if="shown" class="notice tone-info" data-testid="cursor-reset-notice">
    <span aria-hidden="true">&#9432;</span>
    <span>
      The list started again from the first page, because the server did not accept the place it was
      reading from (this happens after the server was updated).
    </span>
    <button type="button" class="btn btn-small" @click="emit('dismiss')">Dismiss</button>
  </div>
</template>
