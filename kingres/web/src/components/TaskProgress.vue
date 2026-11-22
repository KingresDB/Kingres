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
import { hasGroupSteps, stepBlocks, stepName } from '@/lib/failover'
import { keyTaskErrorHint } from '@/lib/ledger-signing'
import ProblemNotice from './ProblemNotice.vue'
import { ApiError } from '@/api/problem'
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
/** The steps of a task with several groups (a failover) come in one block per group (contract 0.10.0), else in one list. */
const blocks = computed(() => {
  const steps = props.task?.progress.steps ?? []
  return hasGroupSteps(steps) ? stepBlocks(steps) : [{ groupId: null, steps }]
})
const grouped = computed(() => hasGroupSteps(props.task?.progress.steps ?? []))
const taskSkip = computed(() => skippedReason(props.task?.detail))
// 0.11.0: a key task of the ledger has words for the error codes it can end with (the code itself is not shown as text).
const errorHint = computed(() =>
  props.task?.kind.startsWith('ledger.key.') ? keyTaskErrorHint(props.task.error?.code) : null,
)
// A schema that became pinned during a task (a ledger table appeared), or that another move holds, ends the task with that code
// (contract 0.16.0); it is shown as the problem notice of the code, as the request would have been refused with it.
const schemaProblem = computed(() => {
  const error = props.task?.error
  if (!error || (error.code !== 'schema_pinned' && error.code !== 'schema_move_in_progress'))
    return null
  return new ApiError({
    type: 'about:blank',
    title: error.code,
    status: 409,
    code: error.code,
    detail: error.message,
  })
})
// A move that failed in its check or its copy, or was cancelled, left the schema where it was, in use (the copy is undone before its
// switch). A move that failed in a later step (the check of the switch) did not: it says nothing about where the schema is.
const moveFailed = computed(() => {
  const t = props.task
  if (t?.kind !== 'schema.move' || t.state !== 'failed') return false
  return (
    t.error?.code === 'cancelled' ||
    t.error?.step === 'Check the move' ||
    t.error?.step === 'Copy the data'
  )
})
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
    <template v-if="task.progress.steps.length">
      <template v-for="(block, b) in blocks" :key="`${b}-${block.groupId}`">
        <h3 v-if="block.groupId !== null" class="step-group" data-testid="step-group">
          Group {{ block.groupId }}
        </h3>
        <ol class="steps">
          <li
            v-for="(step, i) in block.steps"
            :key="`${i}-${step.name}`"
            :data-state="stepState(step)"
          >
            <span aria-hidden="true">{{ stepIcon(step) }}</span>
            <SafeName :value="grouped ? stepName(step) : step.name" kind="text" :max="120" />
            <span class="muted">({{ stepState(step) }})</span>
            <span v-if="step.detail && step.detail !== task.detail" class="muted step-detail"
              ><SafeName :value="step.detail" kind="text" :max="200"
            /></span>
            <span
              v-if="stepSkip(step) !== null"
              class="muted step-detail"
              data-testid="step-skipped"
            >
              Skipped on purpose, not a failure<template v-if="stepSkip(step)"
                >: <SafeName :value="stepSkip(step) ?? ''" kind="text" :max="200" /></template
              >.
            </span>
          </li>
        </ol>
      </template>
    </template>
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
    <ProblemNotice v-if="schemaProblem" :error="schemaProblem" />
    <p v-else-if="task.error" class="tone-danger notice" role="alert">
      <span aria-hidden="true">&#10006;</span>
      <span
        ><SafeName :value="task.error.message" kind="text" :max="400" /><template
          v-if="task.error.step"
        >
          (step: <SafeName :value="task.error.step" kind="text" :max="80" />)</template
        ></span
      >
    </p>
    <p v-if="moveFailed" class="notice tone-info" data-testid="move-failed-note" role="status">
      <span aria-hidden="true">&#9432;</span>
      <span
        >The schema is still on its original group and in use. You can start the move again.</span
      >
    </p>
    <p v-if="errorHint" class="muted" data-testid="task-error-hint">{{ errorHint }}</p>
    <p v-if="pollError" class="muted" role="alert">
      Lost contact with the server while following this task. Reload to see its state.
    </p>
  </section>
</template>

<style scoped>
.step-group {
  font-size: var(--fs-sm);
  margin: var(--s-2) 0 0;
}
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
