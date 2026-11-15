<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Tasks whose dialog was closed while they were running. A running task shows its progress, a
// finished one says so, and a failed one stays (with the reason) until it is dismissed.
import { RouterLink } from 'vue-router'
import SafeName from './SafeName.vue'
import { useTaskNoticeStore } from '@/stores/taskNotices'
import { safeText } from '@/lib/safe-text'
import { isSettled } from '@/composables/taskPoller'

const store = useTaskNoticeStore()
</script>

<template>
  <div v-if="store.notices.length" class="task-notices">
    <section
      v-for="n in store.notices"
      :key="n.id"
      class="banner"
      :class="
        n.task.state === 'failed'
          ? 'tone-danger'
          : n.task.state === 'done'
            ? 'tone-ok'
            : 'tone-info'
      "
      :role="n.task.state === 'failed' || n.lost ? 'alert' : 'status'"
      :aria-label="`${safeText(n.label)}: ${n.task.state}`"
    >
      <span aria-hidden="true">{{
        n.task.state === 'failed' ? '✖' : n.task.state === 'done' ? '✓' : '◐'
      }}</span>
      <span class="task-notice-text">
        <strong><SafeName :value="n.label" kind="text" :max="120" /></strong>:
        <template v-if="n.task.state === 'failed'"
          >failed<template v-if="n.task.error">
            (<SafeName :value="n.task.error.message" kind="text" :max="300" />)</template
          >.</template
        >
        <template v-else-if="n.task.state === 'done'">finished.</template>
        <template v-else-if="n.task.state === 'paused'">paused.</template>
        <template v-else
          >still running, {{ Math.round(n.task.progress.percent) }} percent<template
            v-if="n.task.progress.currentStep"
          >
            (<SafeName :value="n.task.progress.currentStep" kind="text" :max="80" />)</template
          >.</template
        >
        <template v-if="n.lost">
          Lost contact with the server; the state of the task is not known.</template
        >
        <RouterLink :to="{ name: 'tasks' }"> All tasks</RouterLink>
      </span>
      <button
        v-if="isSettled(n.task) || n.lost"
        type="button"
        class="btn btn-small"
        @click="store.dismiss(n.id)"
      >
        Dismiss
      </button>
    </section>
  </div>
</template>

<style scoped>
.task-notice-text {
  flex: 1;
  min-width: 0;
  overflow-wrap: anywhere;
}
</style>
