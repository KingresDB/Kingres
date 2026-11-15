<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Live view of a Task: state, percent, steps and error. Announced through an aria-live region.
import { computed, watch } from 'vue'
import StatusBadge from './StatusBadge.vue'
import SafeName from './SafeName.vue'
import { useAnnounce } from '@/composables/useAnnounce'
import { TASK_STATE } from '@/lib/status'
import { skippedReason } from '@/lib/standby'
import type { Task, TaskStep } from '@/api/types'

const props = defineProps<{ task: Task | null; pollError?: unknown }>()
const announce = useAnnounce()

const summary = computed(() => {
  const t = props.task
  if (!t) return ''
  if (t.state === 'failed') return `Task ${t.id} failed: ${t.error?.message ?? 'unknown error'}`
  if (t.state === 'done') return `Task ${t.id} finished.`
  return `Task ${t.id}: ${t.progress.currentStep ?? t.state}, ${Math.round(t.progress.percent)} percent.${t.detail ? ` ${t.detail}` : ''}`
})
// What is read aloud changes only when the state, the step or the next tenth of the progress
// changes, not every second.
const announcementKey = computed(() => {
  const t = props.task
  return t
    ? `${t.state}|${t.progress.currentStep ?? ''}|${Math.floor(t.progress.percent / 10)}`
    : ''
})
/** The reason a step was skipped on purpose, or null. A skipped step is not a failure. */
function stepSkip(step: TaskStep): string | null {
  // The contract has the state `skipped`; kingresd 0.7.0 finishes the catalog step and records the reason in the
  // detail of the task ("skipped: needs kingres_shard 0.5"). Both are shown the same way.
  if (step.state === 'skipped') {
    const reason = skippedReason(step.detail) ?? skippedReason(props.task?.detail)
    if (reason !== null) return reason
    // A step that was not run because an earlier step failed is not "on purpose".
    return props.task?.state === 'failed' ? null : ''
  }
  if (step.name === 'standby.register_in_catalog' && step.state === 'done')
    return skippedReason(props.task?.detail)
  return null
}
/** The state as it is read out: a skipped step says so in words, never by symbol alone. */
function stepState(step: TaskStep): string {
  return stepSkip(step) !== null ? 'skipped' : step.state
}
function stepIcon(step: TaskStep): string {
  if (stepSkip(step) !== null) return '↷'
  return step.state === 'done'
    ? '✓'
    : step.state === 'failed'
      ? '✖'
      : step.state === 'running'
        ? '◐'
        : '○'
}
const taskSkip = computed(() => skippedReason(props.task?.detail))
watch(announcementKey, (key) => {
  if (key) announce(summary.value, props.task?.state === 'failed' ? 'assertive' : 'polite')
})
watch(
  () => props.pollError,
  (error) => {
    if (error) announce('Lost contact with the server while following this task.', 'assertive')
  },
)
</script>

<template>
  <section v-if="task" class="stack task-progress" :aria-label="`Task ${task.id} progress`">
    <div class="row-between">
      <span class="row">
        <StatusBadge :meta="TASK_STATE[task.state]" />
        <span class="mono"><SafeName :value="task.kind" kind="text" :max="40" /></span>
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
      <li v-for="step in task.progress.steps" :key="step.name" :data-state="stepState(step)">
        <span aria-hidden="true">{{ stepIcon(step) }}</span>
        <SafeName :value="step.name" kind="text" :max="120" />
        <span class="muted">({{ stepState(step) }})</span>
        <span v-if="step.detail && step.detail !== task.detail" class="muted step-detail"
          ><SafeName :value="step.detail" kind="text" :max="200"
        /></span>
        <span v-if="stepSkip(step) !== null" class="muted step-detail" data-testid="step-skipped">
          Skipped on purpose, not a failure<template v-if="stepSkip(step)"
            >: <SafeName :value="stepSkip(step) ?? ''" kind="text" :max="200" /></template
          >.
        </span>
      </li>
    </ol>
    <p v-if="task.detail" class="muted task-detail">
      <SafeName :value="task.detail" kind="text" :max="300" multiline />
    </p>
    <p
      v-if="taskSkip !== null && task.state !== 'failed'"
      class="muted"
      data-testid="task-skip-note"
    >
      A skipped step is not a failure: the task did what it could. Here the standbys work (they
      replicate and are supervised) without being recorded in the sharding catalog, which needs the
      sharding extension 0.4.
    </p>
    <p v-if="task.error" class="tone-danger notice" role="alert">
      <span aria-hidden="true">&#10006;</span>
      <span
        ><SafeName :value="task.error.message" kind="text" :max="400" /><template
          v-if="task.error.step"
        >
          (step: <SafeName :value="task.error.step" kind="text" :max="80" />)</template
        ></span
      >
    </p>
    <p v-if="pollError" class="muted" role="alert">
      Lost contact with the server while following this task. Reload to see its state.
    </p>
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
