// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The findings of the independent review of the console of increment 9.7 (C97R-001 to C97R-007): type-to-confirm for the changes that move
// rows, what a cancelled or failed task says, the paused, verification-failed and pending states of a table task, the preview that holds the
// start while it loads, radio groups that the keyboard can walk, the 57 bytes of a partition name in UTF-8, and the pending task that holds
// the buttons of the page.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { endpoints } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { applySwitches } from '@/mocks/switches'
import { renderApp } from './helpers'

const sw = (q: string) => applySwitches(new URLSearchParams(q), getState())
type User = ReturnType<typeof userEvent.setup>
const page = (db: string, table: string) => `/databases/${db}/tables/public/${table}`
const dialogOf = async (name: RegExp) => within(await screen.findByRole('dialog', { name }))
const enabled = async (button: HTMLElement) => waitFor(() => expect(button).toBeEnabled())
/** A letter of two bytes in UTF-8, written without a non-ASCII character in the source. */
const E_ACUTE = String.fromCharCode(0xe9)
const LONG = { timeout: 30_000 }

describe('C97R-001: type-to-confirm and the danger tone', () => {
  async function openAlter(user: User, table = 'orders') {
    await renderApp(page('shop', table))
    await user.click(await screen.findByRole('button', { name: /^change the distribution/i }))
    return dialogOf(/^change the distribution of public\./i)
  }

  it('a change that moves rows needs the name of the table typed and warns', async () => {
    const user = userEvent.setup()
    const dialog = await openAlter(user)
    await user.type(await dialog.findByTestId('alter-buckets'), '100')
    await dialog.findByTestId('table-change-estimate')
    expect(dialog.getByTestId('alter-moves-warning')).toHaveClass('tone-danger')
    const start = dialog.getByRole('button', { name: /^change the distribution$/i })
    expect(start).toBeDisabled()
    await user.type(dialog.getByLabelText(/to confirm/i), 'orders')
    await enabled(start)
  })

  it('a change that moves no row (a multiple of the buckets) needs no typing', async () => {
    const user = userEvent.setup()
    const dialog = await openAlter(user, 'events')
    await user.type(await dialog.findByTestId('alter-buckets'), '256')
    await dialog.findByTestId('table-change-estimate')
    expect(dialog.queryByLabelText(/to confirm/i)).not.toBeInTheDocument()
    expect(dialog.queryByTestId('alter-moves-warning')).not.toBeInTheDocument()
    await enabled(dialog.getByRole('button', { name: /^change the distribution$/i }))
  })
})

describe('C97R-002 and C97R-003: cancelled, paused and failed tasks say what is known', () => {
  it('a task that waits for a transaction is shown paused with the blockers, and Cancel works while it is paused', async () => {
    const user = userEvent.setup()
    sw('tables=slow,blockme,undo:500')
    await renderApp(page('shop', 'events'))
    await screen.findByTestId('table-distribution')
    const task = await endpoints.undistributeTable('shop', 'public', 'events')
    const rec = getState().tasks.find((t) => t.task.id === task.id)!
    rec.startedAt = Date.now() - 0.6 * rec.durationMs
    const running = await screen.findByTestId('table-task-running', {}, LONG)
    await waitFor(() => expect(running).toHaveTextContent(/paused/i), LONG)
    expect(running).toHaveTextContent(/pid 4123 \(role app\) holds a lock/i)
    const alter = screen.getByRole('button', { name: /^change the distribution/i })
    expect(alter).toHaveAttribute('aria-disabled', 'true')
    const cancel = within(running).getByTestId('table-task-cancel')
    expect(cancel).not.toHaveAttribute('aria-disabled', 'true')
    await user.click(cancel)
    const failed = await screen.findByTestId('table-task-failed', {}, LONG)
    expect(failed).toHaveTextContent(/cancelled/i)
    expect(failed).not.toHaveTextContent(/as it was/i)
    await waitFor(() => expect(failed).toHaveTextContent(/was undone on every server/i), LONG)
  })

  it('an undo that could not reach a server says which one is cleaned up when it returns, and never "as it was"', async () => {
    const user = userEvent.setup()
    sw('tables=slow,undo:200,undo:fail')
    await renderApp(page('shop', 'events'))
    await screen.findByTestId('table-distribution')
    await endpoints.undistributeTable('shop', 'public', 'events')
    const running = await screen.findByTestId('table-task-running', {}, LONG)
    await user.click(within(running).getByTestId('table-task-cancel'))
    const failed = await screen.findByTestId('table-task-failed', {}, LONG)
    await waitFor(
      () => expect(failed).toHaveTextContent(/10\.0\.0\.13.*cleaned up when that server returns/i),
      LONG,
    )
    expect(failed).not.toHaveTextContent(/as it was/i)
  })

  it('a verification failure is told in the dialog and on the page, with the old copy kept', async () => {
    const user = userEvent.setup()
    sw('tables=diffme')
    mockConfig.taskDurationMs = 80
    await renderApp(page('shop', 'notes'))
    await user.click(await screen.findByRole('button', { name: /^distribute table/i }))
    const dialog = await dialogOf(/^distribute public\./i)
    const start = dialog.getByRole('button', { name: /^distribute table$/i })
    await dialog.findByTestId('table-change-eligible')
    await enabled(start)
    await user.click(start)
    const note = await dialog.findByTestId('task-verification-failed', {}, LONG)
    expect(note).toHaveTextContent(/old copy was kept for inspection; contact an administrator/i)
    expect(dialog.getAllByText(/the new table differs from the old copy/i).length).toBeGreaterThan(
      0,
    )
    await user.click(dialog.getByRole('button', { name: /^close$/i }))
    const failed = await screen.findByTestId('table-task-failed', {}, LONG)
    expect(failed).toHaveTextContent(/old copy was kept for inspection/i)
    expect(failed).not.toHaveTextContent(/as it was/i)
  })
})

describe('C97R-004: the start waits for the preview of the current request', () => {
  it('stays disabled while the first preview loads, and while a changed request is not yet previewed', async () => {
    const user = userEvent.setup()
    let release: () => void = () => {}
    let gate = new Promise<void>((r) => (release = r))
    let calls = 0
    mockServer.use(
      http.post('*/distribution/preview', async () => {
        calls += 1
        await gate
        return HttpResponse.json({
          blockers: [],
          warnings: [],
          notes: [],
          affectedTables: [],
          estimate: {
            rows: 10,
            bytes: 100,
            modeRecommended: 'blocking',
            onlineAvailable: true,
            onlineUnavailableReason: null,
            writersBlocked: 'switchOnly',
          },
        })
      }),
    )
    await renderApp(page('shop', 'notes'))
    await user.click(await screen.findByRole('button', { name: /^distribute table/i }))
    const dialog = await dialogOf(/^distribute public\./i)
    const start = dialog.getByRole('button', { name: /^distribute table$/i })
    await waitFor(() => expect(calls).toBe(1))
    expect(start).toBeDisabled() // the first preview is on its way
    release()
    await dialog.findByTestId('table-change-eligible')
    await enabled(start)
    // A change of the request: inside the delay of the preview and while the new one loads, the button stays off.
    gate = new Promise<void>((r) => (release = r))
    await user.click(dialog.getByRole('radio', { name: /^blocking/i }))
    expect(start).toBeDisabled()
    await waitFor(() => expect(calls).toBe(2))
    expect(start).toBeDisabled()
    release()
    await enabled(start)
  })
})

describe('C97R-005: radios that belong together share a name', () => {
  it('the choices of the mode have one name, those of the views and of the method others', async () => {
    const user = userEvent.setup()
    await renderApp(page('shop', 'notes'))
    await user.click(await screen.findByRole('button', { name: /^distribute table/i }))
    const dialog = await dialogOf(/^distribute public\./i)
    const names = (labels: RegExp) =>
      new Set(dialog.getAllByRole('radio', { name: labels }).map((r) => r.getAttribute('name')))
    const mode = names(/^(automatic|online|blocking)/i)
    const views = names(/views/i)
    const method = names(/^(hash|reference)/i)
    for (const group of [mode, views, method]) {
      expect(group.size).toBe(1)
      expect([...group][0]).toBeTruthy()
    }
    expect(new Set([...mode, ...views, ...method]).size).toBe(3)
  })
})

describe('C97R-006: the name of a partition counts bytes, not characters', () => {
  it('a name of 30 two-byte letters (60 bytes) is refused, 28 of them (56 bytes) is accepted', async () => {
    const user = userEvent.setup()
    sw('tables=partitions')
    await renderApp(page('shop', 'measurements'))
    await user.click(await screen.findByTestId('create-partition'))
    const dialog = await dialogOf(/^create a partition of/i)
    await user.click(dialog.getByRole('button', { name: /^create partition$/i }))
    await user.type(dialog.getByTestId('partition-name'), E_ACUTE.repeat(30))
    expect(
      await dialog.findByText(/at most 57 bytes: the name/i, { selector: '.error' }),
    ).toBeInTheDocument()
    await user.clear(dialog.getByTestId('partition-name'))
    await user.type(dialog.getByTestId('partition-name'), E_ACUTE.repeat(28))
    await waitFor(() =>
      expect(
        dialog.queryByText(/at most 57 bytes: the name/i, { selector: '.error' }),
      ).not.toBeInTheDocument(),
    )
  })
})

describe('C97R-007: a pending task holds the buttons, and the page reads enough tasks', () => {
  it('holds the change buttons while a change waits to start, and asks for 200 tasks', async () => {
    sw('tables=pending')
    const limits: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      const u = new URL(request.url)
      if (u.pathname.endsWith('/tasks')) limits.push(u.searchParams.get('limit') ?? '')
    })
    await renderApp(page('shop', 'events'))
    await screen.findByTestId('table-distribution')
    const task = await endpoints.undistributeTable('shop', 'public', 'events')
    expect(task.state).toBe('pending')
    const alter = screen.getByRole('button', { name: /^change the distribution/i })
    await waitFor(() => expect(alter).toHaveAttribute('aria-disabled', 'true'), LONG)
    expect(alter).toHaveTextContent(new RegExp(`task ${task.id}`))
    expect(await screen.findByTestId('table-task-running', {}, LONG)).toBeInTheDocument()
    expect(limits).toContain('200')
  })
})
