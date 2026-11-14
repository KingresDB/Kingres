// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { effectScope } from 'vue'
import { isSettled, taskPolling, useTask } from '@/composables/useTask'
import type { Task, TaskState } from '@/api/types'

function task(id: number, state: TaskState, percent: number): Task {
  return {
    id,
    kind: 'database.create',
    state,
    progress: { percent, currentStep: null, steps: [] },
    error: state === 'failed' ? { code: 'x', message: 'disk full' } : null,
    subject: null,
    cancellable: false,
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
    createdBy: 'admin',
  }
}

function make(options: Parameters<typeof useTask>[0]) {
  const scope = effectScope()
  const t = scope.run(() => useTask(options))
  if (!t) throw new Error('scope did not run')
  return { t, scope }
}

describe('useTask', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    taskPolling.intervalMs = 1000
    taskPolling.maxConsecutiveFailures = 5
  })
  afterEach(() => vi.useRealTimers())

  it('polls until the task is done and exposes progress', async () => {
    const answers = [task(7, 'running', 40), task(7, 'running', 80), task(7, 'done', 100)]
    const fetchTask = vi.fn(async () => answers.shift() as Task)
    const onSettled = vi.fn()
    const { t, scope } = make({ fetchTask, onSettled })

    t.track(task(7, 'pending', 0))
    expect(t.isActive.value).toBe(true)
    expect(t.percent.value).toBe(0)

    await vi.advanceTimersByTimeAsync(1000)
    expect(t.percent.value).toBe(40)
    await vi.advanceTimersByTimeAsync(1000)
    expect(t.percent.value).toBe(80)
    expect(onSettled).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1000)

    expect(t.state.value).toBe('done')
    expect(t.isActive.value).toBe(false)
    expect(onSettled).toHaveBeenCalledTimes(1)
    // No further polling after the task settled.
    await vi.advanceTimersByTimeAsync(5000)
    expect(fetchTask).toHaveBeenCalledTimes(3)
    scope.stop()
  })

  it('reports a failed task with its error and stops', async () => {
    const fetchTask = vi.fn(async () => task(8, 'failed', 50))
    const { t, scope } = make({ fetchTask })
    t.track(task(8, 'running', 10))
    await vi.advanceTimersByTimeAsync(1000)
    expect(t.failed.value).toBe(true)
    expect(t.task.value?.error?.message).toBe('disk full')
    scope.stop()
  })

  it('treats a paused task (stopped rebalance) as settled', () => {
    expect(isSettled(task(1, 'paused', 30))).toBe(true)
    expect(isSettled(task(1, 'running', 30))).toBe(false)
  })

  it('accepts a bare task id and tolerates a few failed polls', async () => {
    let calls = 0
    const fetchTask = vi.fn(async () => {
      calls += 1
      if (calls <= 2) throw new Error('restarting')
      return task(9, 'done', 100)
    })
    const { t, scope } = make({ fetchTask })
    t.track(9)
    await vi.advanceTimersByTimeAsync(3000)
    expect(t.state.value).toBe('done')
    expect(t.pollError.value).toBeNull()
    scope.stop()
  })

  it('gives up after repeated poll failures and exposes the error', async () => {
    const fetchTask = vi.fn(async () => {
      throw new Error('offline')
    })
    const { t, scope } = make({ fetchTask })
    t.track(task(10, 'running', 1))
    await vi.advanceTimersByTimeAsync(
      taskPolling.intervalMs * taskPolling.maxConsecutiveFailures + 10,
    )
    expect(t.pollError.value).toBeInstanceOf(Error)
    expect(fetchTask).toHaveBeenCalledTimes(taskPolling.maxConsecutiveFailures)
    scope.stop()
  })

  it('stops polling when reset or when its scope ends', async () => {
    const fetchTask = vi.fn(async () => task(11, 'running', 5))
    const { t, scope } = make({ fetchTask })
    t.track(task(11, 'running', 0))
    t.reset()
    await vi.advanceTimersByTimeAsync(3000)
    expect(fetchTask).not.toHaveBeenCalled()
    t.track(task(11, 'running', 0))
    scope.stop()
    await vi.advanceTimersByTimeAsync(3000)
    expect(fetchTask).not.toHaveBeenCalled()
  })

  it('does not poll a task that is already settled when tracked', async () => {
    const fetchTask = vi.fn()
    const onSettled = vi.fn()
    const { t, scope } = make({ fetchTask, onSettled })
    t.track(task(12, 'done', 100))
    await vi.advanceTimersByTimeAsync(3000)
    expect(onSettled).toHaveBeenCalledTimes(1)
    expect(fetchTask).not.toHaveBeenCalled()
    scope.stop()
  })
})
