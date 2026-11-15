// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Tasks that were started in a dialog that has since been closed. The store keeps following each
// of them, so that the end of the task, and above all its failure, is still reported. This is
// server data: it is dropped when the session ends.
import { ref } from 'vue'
import { defineStore } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { registerServerStateReset } from '@/api/server-state'
import type { Task } from '@/api/types'
import { createTaskPoller, isSettled, type TaskPoller } from '@/composables/taskPoller'

export interface TaskNotice {
  /** Task id. */
  id: number
  label: string
  task: Task
  /** True when the task could not be read repeatedly. */
  lost: boolean
}

export const useTaskNoticeStore = defineStore('taskNotices', () => {
  const notices = ref<TaskNotice[]>([])
  const pollers = new Map<number, TaskPoller>()

  function patch(id: number, change: Partial<TaskNotice>): void {
    const i = notices.value.findIndex((n) => n.id === id)
    const current = notices.value[i]
    if (current) notices.value.splice(i, 1, { ...current, ...change })
  }

  /** Follows `task` until it settles; the notice stays until it is dismissed. */
  function follow(
    task: Task,
    options: {
      label: string
      refresh?: () => void
      fetchTask?: (id: number) => Promise<Task>
    },
  ): void {
    if (pollers.has(task.id)) return
    notices.value.push({ id: task.id, label: options.label, task, lost: false })
    if (isSettled(task)) return
    const poller = createTaskPoller({
      fetchTask: options.fetchTask ?? endpoints.getTask,
      onUpdate: (next) => patch(task.id, { task: next, lost: false }),
      onSettled: (next) => {
        pollers.delete(task.id)
        options.refresh?.()
        patch(task.id, { task: next })
      },
      onLost: () => {
        pollers.delete(task.id)
        patch(task.id, { lost: true })
      },
    })
    pollers.set(task.id, poller)
    poller.start(task.id)
  }

  function dismiss(id: number): void {
    pollers.get(id)?.stop()
    pollers.delete(id)
    notices.value = notices.value.filter((n) => n.id !== id)
  }

  /** Stops every follower and forgets every notice (the session ended). */
  function clear(): void {
    for (const poller of pollers.values()) poller.stop()
    pollers.clear()
    notices.value = []
  }
  registerServerStateReset(clear)

  return { notices, follow, dismiss, clear }
})
