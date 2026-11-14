// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Long-running operations return 202 + a Task. These composables follow the task by polling
// GET /tasks/{id} until it settles, expose its progress, and refresh the affected queries.
// A task whose dialog is closed while it still runs is handed to the task notice store, so its
// end (and its failure) is still reported.
import { computed, getCurrentScope, onScopeDispose, ref, shallowRef } from 'vue'
import { useQueryClient, type QueryKey } from '@tanstack/vue-query'
import { endpoints } from '@/api/endpoints'
import type { Task } from '@/api/types'
import { useTaskNoticeStore } from '@/stores/taskNotices'
import { createTaskPoller, isSettled, taskPolling } from './taskPoller'

export { createTaskPoller, isSettled, taskPolling }

export interface UseTaskOptions {
  /** Called once when the task settles (done, failed or paused). */
  onSettled?: (task: Task) => void
  /** Replaceable for tests. */
  fetchTask?: (id: number) => Promise<Task>
}

/**
 * Follows one task at a time. Call `track()` with the Task from a 202 response (or just its id).
 * Polling stops when the task settles, when `reset()` is called, and when the scope is disposed.
 */
export function useTask(options: UseTaskOptions = {}) {
  const fetchTask = options.fetchTask ?? endpoints.getTask
  const task = shallowRef<Task | null>(null)
  const pollError = ref<unknown>(null)

  const state = computed(() => task.value?.state ?? null)
  const percent = computed(() => task.value?.progress.percent ?? 0)
  const isActive = computed(() => task.value !== null && !isSettled(task.value))
  const failed = computed(() => task.value?.state === 'failed')

  const poller = createTaskPoller({
    fetchTask,
    onUpdate: (next) => {
      task.value = next
      pollError.value = null
    },
    onSettled: (t) => options.onSettled?.(t),
    onLost: (error) => {
      pollError.value = error
    },
  })

  function track(initial: Task | number): void {
    poller.stop()
    pollError.value = null
    if (typeof initial === 'number') {
      task.value = null
      poller.start(initial)
      return
    }
    task.value = initial
    if (isSettled(initial)) options.onSettled?.(initial)
    else poller.start(initial.id)
  }

  function reset(): void {
    poller.stop()
    task.value = null
    pollError.value = null
  }

  if (getCurrentScope()) onScopeDispose(poller.stop)

  return { task, state, percent, isActive, failed, pollError, track, reset }
}

export interface TaskActionOptions<V> {
  /** Query keys to refresh when the task settles (and right after the request is accepted). */
  invalidate?: (variables: V) => QueryKey[]
  onDone?: (task: Task, variables: V) => void
  /** Replaces the function that reads a task (the setup wizard needs a special one). */
  fetchTask?: (id: number) => Promise<Task>
  /** Names the task in the notice that outlives the dialog, for example the dialog title. */
  label?: () => string
}

/**
 * Starts an operation that answers 202 + Task, follows the task, and refreshes affected queries.
 * Errors from the start request are exposed as `error` (never thrown into the template).
 * If the owner goes away while the task still runs (a closed dialog), a task notice takes over.
 */
export function useTaskAction<V = void>(
  start: (variables: V) => Promise<Task>,
  options: TaskActionOptions<V> = {},
) {
  const client = useQueryClient()
  const notices = useTaskNoticeStore()
  const error = ref<unknown>(null)
  const isStarting = ref(false)
  let lastVariables: V

  const refresh = () => {
    const list = options.invalidate?.(lastVariables) ?? []
    for (const queryKey of [...list, ['tasks'], ['cluster']]) {
      void client.invalidateQueries({ queryKey })
    }
  }

  const tracker = useTask({
    fetchTask: options.fetchTask,
    onSettled: (t) => {
      refresh()
      if (t.state === 'done') options.onDone?.(t, lastVariables)
    },
  })

  /** Returns the accepted Task, or null if the server refused the request. */
  async function run(variables: V): Promise<Task | null> {
    error.value = null
    lastVariables = variables
    isStarting.value = true
    try {
      const accepted = await start(variables)
      tracker.track(accepted)
      refresh()
      return accepted
    } catch (e) {
      error.value = e
      return null
    } finally {
      isStarting.value = false
    }
  }

  function reset(): void {
    error.value = null
    tracker.reset()
  }

  // The dialog was closed while the task still runs: keep following it in the notice store.
  if (getCurrentScope()) {
    onScopeDispose(() => {
      const t = tracker.task.value
      if (t && !isSettled(t)) {
        notices.follow(t, {
          label: options.label?.() ?? t.kind,
          refresh,
          fetchTask: options.fetchTask,
        })
      }
    })
  }

  const isBusy = computed(() => isStarting.value || tracker.isActive.value)

  return { ...tracker, run, reset, error, isStarting, isBusy }
}
