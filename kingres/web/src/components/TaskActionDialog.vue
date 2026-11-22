<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// A dialog for an operation that answers 202 + Task: it first states the consequence (default
// slot) and asks for confirmation, then follows the task and shows its progress.
// Field errors returned by the server are passed to the slot so forms can show them inline.
import { computed, ref } from 'vue'
import type { QueryKey } from '@tanstack/vue-query'
import AppDialog from './AppDialog.vue'
import TypeToConfirm from './TypeToConfirm.vue'
import ProblemNotice from './ProblemNotice.vue'
import TaskProgress from './TaskProgress.vue'
import GateButton from './GateButton.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import type { Task } from '@/api/types'
import { useTaskAction } from '@/composables/useTask'

const props = withDefaults(
  defineProps<{
    title: string
    confirmLabel: string
    /** Starts the operation and returns the accepted task. */
    start: () => Promise<Task>
    danger?: boolean
    /** The user must type this text before the button is enabled. */
    typeToConfirm?: string
    /** Return false to stop the submit (client-side validation). */
    validate?: () => boolean
    invalidate?: QueryKey[]
    wide?: boolean
    /** True while the confirm button must stay disabled for a reason the dialog itself explains. */
    confirmDisabled?: boolean
    /** Offers "Cancel the task" while a task that can be cancelled runs, and says what a cancelled task leaves. */
    cancelable?: boolean
    cancelledText?: string
  }>(),
  {
    danger: false,
    typeToConfirm: undefined,
    validate: undefined,
    invalidate: () => [],
    wide: false,
    confirmDisabled: false,
    cancelable: false,
    cancelledText: 'The task was cancelled.',
  },
)
const emit = defineEmits<{ close: []; done: [task: Task] }>()

const typed = ref('')
const action = useTaskAction<void>(() => props.start(), {
  invalidate: () => props.invalidate,
  onDone: (task) => emit('done', task),
  label: () => props.title,
})

const cancelling = ref(false)
const cancelError = ref<unknown>(null)
const canCancel = computed(
  () =>
    props.cancelable &&
    action.task.value !== null &&
    action.isActive.value &&
    action.task.value.cancellable,
)
const wasCancelled = computed(
  () =>
    props.cancelable &&
    action.task.value?.state === 'failed' &&
    action.task.value.error?.code === 'cancelled',
)
async function cancelTask(): Promise<void> {
  const t = action.task.value
  if (!t) return
  cancelling.value = true
  cancelError.value = null
  try {
    await endpoints.cancelTask(t.id)
  } catch (e) {
    cancelError.value = e
  } finally {
    cancelling.value = false
  }
}

const fieldErrors = computed(() =>
  action.error.value ? describeProblem(action.error.value).fieldErrors : {},
)
const started = computed(() => action.task.value !== null)
const typedOk = computed(
  () => props.typeToConfirm === undefined || typed.value === props.typeToConfirm,
)

async function confirm(): Promise<void> {
  if (props.validate && !props.validate()) return
  await action.run()
}
</script>

<template>
  <AppDialog
    :title="title"
    :wide="wide"
    :persistent="action.isStarting.value"
    @close="emit('close')"
  >
    <template v-if="!started">
      <slot :field-errors="fieldErrors" :error="action.error.value" />
      <TypeToConfirm v-if="typeToConfirm !== undefined" v-model="typed" :expected="typeToConfirm" />
      <template v-if="action.error.value">
        <!-- A dialog can say a refusal in words of its own (the failover dialog does); the general notice is the default. -->
        <slot name="error" :error="action.error.value">
          <ProblemNotice :error="action.error.value" />
        </slot>
      </template>
    </template>
    <template v-else>
      <TaskProgress :task="action.task.value" :poll-error="action.pollError.value" />
      <p v-if="action.state.value === 'done'" class="tone-ok notice" role="status">
        <span aria-hidden="true">&#10003;</span> Finished.
      </p>
      <p v-if="wasCancelled" class="tone-info notice" role="status" data-testid="task-cancelled">
        <span aria-hidden="true">&#9432;</span> {{ cancelledText }}
      </p>
      <ProblemNotice v-if="cancelError" :error="cancelError" />
      <slot v-if="action.state.value === 'done'" name="result" :task="action.task.value" />
      <!-- A task that waits for a person (a failover that needs a decision) says what to do. -->
      <slot v-if="action.state.value === 'paused'" name="paused" :task="action.task.value" />
    </template>

    <template #footer>
      <template v-if="!started">
        <button
          type="button"
          class="btn"
          :disabled="action.isStarting.value"
          @click="emit('close')"
        >
          Cancel
        </button>
        <button
          type="button"
          class="btn"
          :class="danger ? 'btn-danger' : 'btn-primary'"
          :disabled="!typedOk || confirmDisabled || action.isStarting.value"
          @click="confirm"
        >
          {{ confirmLabel }}
        </button>
      </template>
      <GateButton
        v-if="started && canCancel"
        op="cancelTask"
        data-testid="cancel-task"
        :disabled="cancelling"
        @click="cancelTask"
      >
        Cancel the task
      </GateButton>
      <button v-if="started" type="button" class="btn btn-primary" @click="emit('close')">
        {{ action.isActive.value ? 'Hide (keeps running)' : 'Close' }}
      </button>
    </template>
  </AppDialog>
</template>
