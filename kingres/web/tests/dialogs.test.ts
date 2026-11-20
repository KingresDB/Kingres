// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-16: a dialog makes the rest of the page inert, and the progress of a task is announced through
// a live region that exists before its text does.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { taskPolling } from '@/composables/useTask'
import { renderApp } from './helpers'

const bodyChildren = () => Array.from(document.body.children) as HTMLElement[]

describe('a dialog makes the page behind it inert (W-16)', () => {
  it('marks everything outside the dialog inert, and undoes it when the dialog closes', async () => {
    const user = userEvent.setup()
    const { container } = await renderApp('/servers')
    await user.click(await screen.findByRole('button', { name: /restore preferred primaries/i }))
    const dialog = await screen.findByRole('dialog')
    const backdrop = dialog.parentElement as HTMLElement
    expect(container).toHaveAttribute('inert')
    expect(backdrop).not.toHaveAttribute('inert')
    // The dialog is still in the page's accessibility tree and can be used.
    expect(within(dialog).getByRole('button', { name: /^cancel$/i })).toBeEnabled()
    await user.click(within(dialog).getByRole('button', { name: /^cancel$/i }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(bodyChildren().some((el) => el.hasAttribute('inert'))).toBe(false)
  })
})

describe('task progress is announced (W-16)', () => {
  it('the live region is there before the first announcement and keeps its identity', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    await user.click(await screen.findByRole('button', { name: /restore preferred primaries/i }))
    const dialog = await screen.findByRole('dialog')
    const region = dialog.querySelector('[aria-live="polite"]') as HTMLElement
    expect(region).not.toBeNull()
    expect(region.textContent).toBe('')
    await user.click(within(dialog).getByRole('button', { name: /^restore primaries$/i }))
    await waitFor(() => expect(region.textContent).toMatch(/finished|percent/i), {
      timeout: 30_000,
    })
    expect(dialog.contains(region)).toBe(true)
  })

  it('"lost contact" is announced as an alert', async () => {
    const user = userEvent.setup()
    const saved = taskPolling.maxConsecutiveFailures
    taskPolling.maxConsecutiveFailures = 2
    try {
      await renderApp('/servers')
      await user.click(await screen.findByRole('button', { name: /restore preferred primaries/i }))
      const dialog = await screen.findByRole('dialog')
      const assertive = dialog.querySelector('[aria-live="assertive"]') as HTMLElement
      expect(assertive).not.toBeNull()
      mockServer.use(
        http.get('*/api/v1/tasks/:id', () =>
          HttpResponse.json(
            { type: 'about:blank', title: 'x', status: 503, code: 'service_unavailable' },
            { status: 503 },
          ),
        ),
      )
      await user.click(within(dialog).getByRole('button', { name: /^restore primaries$/i }))
      await waitFor(() => expect(assertive.textContent).toMatch(/lost contact/i), {
        timeout: 30_000,
      })
      expect(within(dialog).getByRole('alert')).toHaveTextContent(/lost contact/i)
    } finally {
      taskPolling.maxConsecutiveFailures = saved
    }
  })
})
