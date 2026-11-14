// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Long-running operations return 202 + a Task. These composables follow the task by polling
// GET /tasks/{id} until it settles, expose its progress, and refresh the affected queries.
import { computed, onScopeDispose, ref, shallowRef } from 'vue'
import { useQueryClient, type QueryKey } from '@tanstack/vue-query'
import { endpoints } from '@/api/endpoints'
import type { Task } from '@/api/types'

/** Poll settings. Tests shorten the interval. */
export const taskPolling = { intervalMs: 1_000, maxConsecutiveFailures: 5 }

/** A task that will not change any more without outside action. `paused` is a stopped rebalance. */
export function isSettled(task: Task): boolean {
  return task.state === 'done' || task.state === 'failed' || task.state === 'paused'
}

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
  let timer: ReturnType<typeof setTimeout> | undefined
  let generation = 0

  const state = computed(() => task.value?.state ?? null)
  const percent = computed(() => task.value?.progress.percent ?? 0)
  const isActive = computed(() => task.value !== null && !isSettled(task.value))
  const failed = computed(() => task.value?.state === 'failed')

  function stop(): void {
    generation += 1
    if (timer !== undefined) clearTimeout(timer)
    timer = undefined
  }

  function settle(t: Task): void {
    options.onSettled?.(t)
  }

  function schedule(id: number, myGeneration: number, failures: number): void {
    timer = setTimeout(async () => {
      if (myGeneration !== generation) return
      try {
        const next = await fetchTask(id)
        if (myGeneration !== generation) return
        task.value = next
        pollError.value = null
        if (isSettled(next)) settle(next)
        else schedule(id, myGeneration, 0)
      } catch (error) {
        if (myGeneration !== generation) return
        // The server may be restarting (leave cluster, restart instance): tolerate a few misses.
        if (failures + 1 >= taskPolling.maxConsecutiveFailures) pollError.value = error
        else schedule(id, myGeneration, failures + 1)
      }
    }, taskPolling.intervalMs)
  }

  function track(initial: Task | number): void {
    stop()
    const myGeneration = generation
    pollError.value = null
    if (typeof initial === 'number') {
      task.value = null
      schedule(initial, myGeneration, 0)
      return
    }
    task.value = initial
    if (isSettled(initial)) settle(initial)
    else schedule(initial.id, myGeneration, 0)
  }

  function reset(): void {
    stop()
    task.value = null
    pollError.value = null
  }

  onScopeDispose(stop)

  return { task, state, percent, isActive, failed, pollError, track, reset }
}

export interface TaskActionOptions<V> {
  /** Query keys to refresh when the task settles (and right after the request is accepted). */
  invalidate?: (variables: V) => QueryKey[]
  onDone?: (task: Task, variables: V) => void
  /** Replaces the function that reads a task (the setup wizard needs a special one). */
  fetchTask?: (id: number) => Promise<Task>
}

/**
 * Starts an operation that answers 202 + Task, follows the task, and refreshes affected queries.
 * Errors from the start request are exposed as `error` (never thrown into the template).
 */
export function useTaskAction<V = void>(
  start: (variables: V) => Promise<Task>,
  options: TaskActionOptions<V> = {},
) {
  const client = useQueryClient()
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

  const isBusy = computed(() => isStarting.value || tracker.isActive.value)

  return { ...tracker, run, reset, error, isStarting, isBusy }
}
