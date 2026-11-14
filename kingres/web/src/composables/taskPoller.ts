// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// The polling loop that follows a task through GET /tasks/{id}. It has no dependency on components
// or stores, so that dialogs and the task notice store can both use it.
import { registerServerStateReset } from '@/api/server-state'
import type { Task } from '@/api/types'

/** Poll settings. Tests shorten the interval. */
export const taskPolling = { intervalMs: 1_000, maxConsecutiveFailures: 5 }

/** A task that will not change any more without outside action. `paused` is a stopped rebalance. */
export function isSettled(task: Task): boolean {
  return task.state === 'done' || task.state === 'failed' || task.state === 'paused'
}

export interface TaskPoller {
  /** Starts following `id`; a previous follow of this poller ends. */
  start: (id: number) => void
  stop: () => void
}

/** Every poller that is running. They all stop when the session ends. */
const running = new Set<TaskPoller>()
registerServerStateReset(() => {
  for (const poller of [...running]) poller.stop()
})

/**
 * The polling loop shared by the dialogs (`useTask`) and the task notices. Late answers of a
 * poll that was stopped are discarded.
 */
export function createTaskPoller(handlers: {
  fetchTask: (id: number) => Promise<Task>
  onUpdate: (task: Task) => void
  onSettled: (task: Task) => void
  /** The task could not be read several times in a row. */
  onLost: (error: unknown) => void
}): TaskPoller {
  let timer: ReturnType<typeof setTimeout> | undefined
  let generation = 0

  function stop(): void {
    generation += 1
    if (timer !== undefined) clearTimeout(timer)
    timer = undefined
    running.delete(poller)
  }

  function schedule(id: number, myGeneration: number, failures: number): void {
    timer = setTimeout(async () => {
      if (myGeneration !== generation) return
      // A hidden tab does not poll; it asks again as soon as the timer runs while visible.
      if (typeof document !== 'undefined' && document.hidden) {
        schedule(id, myGeneration, failures)
        return
      }
      try {
        const next = await handlers.fetchTask(id)
        if (myGeneration !== generation) return
        handlers.onUpdate(next)
        if (isSettled(next)) {
          running.delete(poller)
          handlers.onSettled(next)
        } else schedule(id, myGeneration, 0)
      } catch (error) {
        if (myGeneration !== generation) return
        // The server may be restarting (leave cluster, restart instance): tolerate a few misses.
        if (failures + 1 >= taskPolling.maxConsecutiveFailures) {
          running.delete(poller)
          handlers.onLost(error)
        } else schedule(id, myGeneration, failures + 1)
      }
    }, taskPolling.intervalMs)
  }

  const poller: TaskPoller = {
    start(id) {
      stop()
      const myGeneration = generation
      running.add(poller)
      schedule(id, myGeneration, 0)
    },
    stop,
  }
  return poller
}
