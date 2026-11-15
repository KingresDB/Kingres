// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.4.0: Task.detail and TaskStep.detail are server text (it can quote hostile names) and
// are shown in task progress and on the Tasks page through the safe-text component.
import { describe, expect, it } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import TaskProgress from '@/components/TaskProgress.vue'
import { getState } from '@/mocks/http'
import { HOSTILE, seedHostile } from '@/mocks/hostile'
import type { Task } from '@/api/types'
import { renderApp, settled } from './helpers'

const base: Task = {
  id: 7,
  kind: 'database.create',
  state: 'running',
  progress: {
    percent: 40,
    currentStep: 'Create the database on server 10.0.0.13',
    steps: [
      { name: 'Record settings', state: 'done' },
      { name: 'Create the database on server 10.0.0.13', state: 'running', detail: 'Waiting.' },
    ],
  },
  error: null,
  detail: 'Waiting for server 10.0.0.13, which cannot be reached.',
  subject: null,
  cancellable: false,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
  createdBy: 'admin',
}

describe('TaskProgress shows Task.detail', () => {
  it('shows the task detail and the detail of a step', () => {
    render(TaskProgress, { props: { task: base } })
    expect(screen.getAllByText(/Waiting for server 10\.0\.0\.13/).length).toBeGreaterThan(0)
    expect(screen.getByText('Waiting.')).toBeInTheDocument()
  })

  it('shows nothing extra when the detail is null', () => {
    render(TaskProgress, { props: { task: { ...base, detail: null } } })
    expect(screen.queryByText(/Waiting for server/)).not.toBeInTheDocument()
  })

  it('makes hidden characters in a hostile detail visible and keeps markup as text', () => {
    const detail = `${HOSTILE.markup} ${HOSTILE.reversed} ${HOSTILE.long}`
    const { container } = render(TaskProgress, {
      props: {
        task: {
          ...base,
          detail,
          progress: {
            ...base.progress,
            steps: [{ name: 'x', state: 'running', detail: HOSTILE.reversed }],
          },
        },
      },
    })
    expect(container.querySelector('img')).toBeNull()
    expect(container.textContent).not.toMatch(/[‮]/)
    expect(container.textContent).toContain('<img src=x')
    // Cut with a way to see all of it, not 5000 characters at once.
    expect((container.textContent ?? '').length).toBeLessThan(2000)
  })
})

describe('Tasks page shows Task.detail', () => {
  it('shows the detail of the hostile task safely and of a normal task in the dialog', async () => {
    seedHostile(getState())
    const task = getState().tasks.find((t) => t.task.detail === HOSTILE.markup)
    expect(task).toBeDefined()
    const user = userEvent.setup()
    await renderApp('/tasks')
    await settled()
    await waitFor(() => expect(document.body.textContent).toContain('<img src=x'))
    expect(document.querySelector('img[src="x"]')).toBeNull()
    const row = screen.getAllByRole('row').find((r) => r.textContent?.includes('<img src=x'))
    expect(row).toBeDefined()
    await user.click(within(row as HTMLElement).getByRole('button', { name: 'Details' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getAllByText(/<img src=x/).length).toBeGreaterThan(0)
    expect(dialog.querySelector('.task-detail')?.textContent).toContain('<img src=x')
  })

  it('the mock gives a running task a detail that its running step repeats', async () => {
    const s = getState()
    const { createTask } = await import('@/mocks/state')
    const t = createTask(s, {
      kind: 'database.create',
      subject: { type: 'database', id: 'x' },
      steps: ['One', 'Two'],
      detail: 'Waiting for server 10.0.0.13, which cannot be reached (One).',
      apply: () => undefined,
    })
    expect(t.detail).toMatch(/Waiting for server/)
    expect(t.progress.steps[0]?.detail).toBe(t.detail)
    expect(t.progress.steps[1]?.detail ?? null).toBeNull()
  })
})
