// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.7.0, the workbench: each statement carries an id (`QueryRequest.statementId`), "Cancel statement" ends it
// on the server (`cancelStatement`, not a task) and "Stop waiting" only stops the page from waiting. The problem
// `workbench_busy` (429, `Retry-After`) replaces the 503 of 0.6.0.
import { describe, expect, it } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { endpoints } from '@/api/endpoints'
import { ApiError, describeProblem } from '@/api/problem'
import { parseRetryAfter } from '@/api/client'
import { canCall } from '@/api/roles'
import { renderApp } from './helpers'

async function open(user = 'admin') {
  const ui = userEvent.setup()
  await renderApp('/workbench', { user })
  await screen.findByRole('option', { name: 'shop' })
  return ui
}

async function type(sql: string, timeout?: string) {
  if (timeout) await fireEvent.update(screen.getByLabelText(/timeout/i), timeout)
  await fireEvent.update(screen.getByLabelText(/^SQL/), sql)
}

describe('statement ids and cancelling', () => {
  it('sends a statement id with every statement and offers Cancel statement while it runs', async () => {
    let sent: { statementId?: string } = {}
    mockServer.use(
      http.post('*/api/v1/query', async ({ request }) => {
        sent = (await request.clone().json()) as { statementId?: string }
        return HttpResponse.json({
          columns: [{ name: 'one', type: 'int4' }],
          rows: [[1]],
          rowCount: 1,
          truncated: false,
          commandTag: 'SELECT 1',
          durationMs: 1,
        })
      }),
    )
    const user = await open()
    await type('select 1')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    await screen.findByTestId('result')
    expect(sent.statementId).toMatch(/^[0-9a-f]{32}$/)
    // A new statement gets a new id.
    const first = sent.statementId
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    await waitFor(() => expect(sent.statementId).not.toBe(first))
  })

  it('cancels a running statement: the page says it was cancelled by the person, and the server audits it', async () => {
    const user = await open()
    await type('select pg_sleep(30)')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    const cancel = await screen.findByRole('button', { name: /cancel statement/i })
    // Both ways out are explained.
    expect(screen.getByTestId('cancel-help')).toHaveTextContent(/stop waiting.*keeps running/is)
    expect(getState().statements.size).toBe(1)
    await user.click(cancel)
    const hint = await screen.findByTestId('query-error-hint')
    expect(hint).toHaveTextContent(/you cancelled this statement/i)
    expect(hint).toHaveTextContent(/57014/)
    expect(hint).not.toHaveTextContent(/time limit/i)
    expect(getState().statements.size).toBe(0)
    expect(getState().audit.some((e) => e.action === 'cancelStatement')).toBe(true)
    // The run button is back and the cancel button is gone.
    expect(screen.queryByRole('button', { name: /cancel statement/i })).toBeNull()
    expect(screen.getByRole('button', { name: /^run$/i })).toBeEnabled()
  })

  it('"Stop waiting" leaves the statement running and then offers to cancel it on the server', async () => {
    const user = await open()
    await type('select pg_sleep(30)')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    await user.click(await screen.findByRole('button', { name: /stop waiting/i }))
    const notice = await screen.findByTestId('stopped-notice')
    expect(notice).toHaveTextContent(/may still be running on the server/i)
    // The server still runs it and it still holds a slot.
    expect(getState().statements.size).toBe(1)
    await user.click(within(notice).getByRole('button', { name: /cancel it on the server/i }))
    await waitFor(() => expect(getState().statements.size).toBe(0))
    expect(
      await screen.findByText(/cancel requested: the server is ending it/i),
    ).toBeInTheDocument()
  })

  it("cancelStatement answers 404 for an id that is not running, and for another role's statement", async () => {
    getState().statements.set('ops\u0000abc', {
      username: 'ops',
      startedAt: Date.now(),
      cancelled: false,
    })
    await renderApp('/dashboard')
    await expect(endpoints.cancelStatement('abc')).rejects.toMatchObject({ status: 404 })
    await expect(endpoints.cancelStatement('nope')).rejects.toMatchObject({ code: 'not_found' })
    // The other role's statement was not touched.
    expect(getState().statements.get('ops\u0000abc')?.cancelled).toBe(false)
  })

  it('shows the 404 of a cancel in words, with the reason it can happen', async () => {
    const user = await open()
    mockServer.use(
      http.post('*/api/v1/query/statements/:id/cancel', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Not found', status: 404, code: 'not_found' },
          { status: 404 },
        ),
      ),
    )
    await type('select pg_sleep(30)')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    await user.click(await screen.findByRole('button', { name: /cancel statement/i }))
    expect(await screen.findByTestId('cancel-error-hint')).toHaveTextContent(
      /ended already.*another server/is,
    )
    // The statement is still there for the test to end.
    for (const rec of getState().statements.values()) rec.cancelled = true
    await screen.findByTestId('query-error-hint')
  })

  it('a second running statement with the same id is a conflict', async () => {
    await renderApp('/dashboard')
    const first = endpoints.executeQuery({
      database: 'shop',
      sql: 'select pg_sleep(30)',
      statementId: 'same',
    })
    await waitFor(() => expect(getState().statements.size).toBe(1))
    await expect(
      endpoints.executeQuery({ database: 'shop', sql: 'select pg_sleep(30)', statementId: 'same' }),
    ).rejects.toMatchObject({ code: 'conflict' })
    await endpoints.cancelStatement('same')
    await expect(first).rejects.toMatchObject({ code: 'sql_error' })
  })
})

describe('workbench_busy (429)', () => {
  it('the third statement of a role is refused with its own words and the Retry-After of the answer', async () => {
    const s = getState()
    for (const id of ['a', 'b'])
      s.statements.set(`admin\u0000${id}`, {
        username: 'admin',
        startedAt: Date.now(),
        cancelled: false,
      })
    const user = await open()
    await type('select 1')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    const notice = await screen.findByText('The workbench is busy')
    expect(notice.closest('[data-code]')).toHaveAttribute('data-code', 'workbench_busy')
    expect(screen.getByText(/asks you to wait 2 seconds/i)).toBeInTheDocument()
    expect(screen.getByTestId('query-error-hint')).toHaveTextContent(
      /at most two statements at a time/i,
    )
    // Not the words of a server fault.
    expect(screen.queryByText(/service unavailable/i)).toBeNull()
  })

  it('the server counts four statements in all', async () => {
    const s = getState()
    for (const [i, name] of ['ops', 'ops', 'viewer', 'viewer'].entries())
      s.statements.set(`${name}\u0000${i}`, {
        username: name,
        startedAt: Date.now(),
        cancelled: false,
      })
    await renderApp('/dashboard')
    await expect(
      endpoints.executeQuery({ database: 'shop', sql: 'select 1' }),
    ).rejects.toMatchObject({
      code: 'workbench_busy',
    })
  })

  it('the switch workbenchBusy refuses every statement', async () => {
    mockConfig.workbenchBusy = true
    const user = await open()
    await type('select 1')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    expect(await screen.findByText('The workbench is busy')).toBeInTheDocument()
  })

  it('describeProblem has words for it and keeps the Retry-After', () => {
    const view = describeProblem(
      new ApiError(
        { type: 'about:blank', title: 'busy', status: 429, code: 'workbench_busy' },
        false,
        7,
      ),
    )
    expect(view.heading).toBe('The workbench is busy')
    expect(view.retryable).toBe(true)
    expect(view.retryAfterSeconds).toBe(7)
    expect(
      describeProblem(new ApiError({ type: 'x', title: 'x', status: 429, code: 'rate_limited' }))
        .retryAfterSeconds,
    ).toBeNull()
  })

  it('reads Retry-After as whole seconds only', () => {
    expect(parseRetryAfter('5')).toBe(5)
    expect(parseRetryAfter(' 12 ')).toBe(12)
    expect(parseRetryAfter(null)).toBeNull()
    expect(parseRetryAfter('Wed, 21 Oct 2026 07:28:00 GMT')).toBeNull()
    expect(parseRetryAfter('-1')).toBeNull()
  })
})

describe('who may cancel', () => {
  it('every account that may run a statement may cancel one; a stats-only account may do neither', () => {
    for (const roles of [['kingres_readonly'], ['kingres_ops'], ['kingres_admin']] as const) {
      expect(canCall([...roles], 'cancelStatement')).toBe(true)
      expect(canCall([...roles], 'executeQuery')).toBe(true)
    }
    expect(canCall(['kingres_stats'], 'cancelStatement')).toBe(false)
    expect(canCall(['kingres_stats'], 'executeQuery')).toBe(false)
  })

  it('a read-only account sees and can use Cancel statement', async () => {
    const user = await open('viewer')
    await type('select pg_sleep(30)')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    const cancel = await screen.findByRole('button', { name: /cancel statement/i })
    expect(cancel).not.toHaveAttribute('aria-disabled')
    await user.click(cancel)
    await screen.findByTestId('query-error-hint')
  })
})
