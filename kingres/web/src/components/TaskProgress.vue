<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Live view of a Task: state, percent, steps and error. Announced through an aria-live region.
import { computed } from 'vue'
import StatusBadge from './StatusBadge.vue'
import { TASK_STATE } from '@/lib/status'
import type { Task } from '@/api/types'

const props = defineProps<{ task: Task | null; pollError?: unknown }>()
const summary = computed(() => {
  const t = props.task
  if (!t) return ''
  if (t.state === 'failed') return `Task ${t.id} failed: ${t.error?.message ?? 'unknown error'}`
  if (t.state === 'done') return `Task ${t.id} finished.`
  return `Task ${t.id}: ${t.progress.currentStep ?? t.state}, ${Math.round(t.progress.percent)} percent.`
})
</script>

<template>
  <section v-if="task" class="stack task-progress" :aria-label="`Task ${task.id} progress`">
    <div class="row-between">
      <span class="row">
        <StatusBadge :meta="TASK_STATE[task.state]" />
        <span class="mono">{{ task.kind }}</span>
      </span>
      <span class="num">{{ Math.round(task.progress.percent) }}%</span>
    </div>
    <div
      class="progress"
      role="progressbar"
      aria-label="Task progress"
      aria-valuemin="0"
      aria-valuemax="100"
      :aria-valuenow="Math.round(task.progress.percent)"
    >
      <span :style="{ width: `${task.progress.percent}%` }" />
    </div>
    <ol v-if="task.progress.steps.length" class="steps">
      <li v-for="step in task.progress.steps" :key="step.name" :data-state="step.state">
        <span aria-hidden="true">{{
          step.state === 'done'
            ? '✓'
            : step.state === 'failed'
              ? '✖'
              : step.state === 'running'
                ? '◐'
                : '○'
        }}</span>
        {{ step.name }}
        <span class="muted">({{ step.state }})</span>
      </li>
    </ol>
    <p v-if="task.error" class="tone-danger notice" role="alert">
      <span aria-hidden="true">&#10006;</span>
      <span
        >{{ task.error.message
        }}<template v-if="task.error.step"> (step: {{ task.error.step }})</template></span
      >
    </p>
    <p v-if="pollError" class="muted">
      Lost contact with the server while following this task. Reload to see its state.
    </p>
    <p class="sr-only" aria-live="polite" role="status">{{ summary }}</p>
  </section>
</template>

<style scoped>
.steps {
  margin: 0;
  padding-left: var(--s-4);
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: var(--fs-sm);
}
</style>
