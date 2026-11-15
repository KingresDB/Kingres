// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-12: a task is not polled in a hidden tab, and polling goes on when the tab is shown again.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createTaskPoller, taskPolling } from '@/composables/taskPoller'
import type { Task } from '@/api/types'

const running: Task = {
  id: 1,
  kind: 'database.drop',
  state: 'running',
  progress: { percent: 10, currentStep: null, steps: [] },
  error: null,
  detail: null,
  subject: null,
  cancellable: false,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
  createdBy: 'admin',
}

function setHidden(hidden: boolean): void {
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden })
}

describe('polling in a hidden tab', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    taskPolling.intervalMs = 1000
  })
  afterEach(() => {
    vi.useRealTimers()
    setHidden(false)
  })

  it('does not ask the server while the tab is hidden, and goes on when it is shown', async () => {
    const fetchTask = vi.fn(async () => ({ ...running, state: 'done' as const }))
    const settled = vi.fn()
    const poller = createTaskPoller({
      fetchTask,
      onUpdate: () => undefined,
      onSettled: settled,
      onLost: () => undefined,
    })
    setHidden(true)
    poller.start(1)
    await vi.advanceTimersByTimeAsync(5000)
    expect(fetchTask).not.toHaveBeenCalled()
    setHidden(false)
    await vi.advanceTimersByTimeAsync(1000)
    expect(fetchTask).toHaveBeenCalledTimes(1)
    expect(settled).toHaveBeenCalledTimes(1)
  })
})
