// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.6.0 on the audit log: the answer is cluster-wide (a server column, the "this list is incomplete"
// notice), event ids are opaque numbers that are not a sequence, a cursor of an earlier revision answers 400
// and the list starts again, and an audited change answers 503 when the audit log cannot be written.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { HOSTILE, HOSTILE_MISSING, seedHostile } from '@/mocks/hostile'
import { mockConfig } from '@/mocks/state'
import type { AuditEvent } from '@/api/types'
import { renderApp } from './helpers'

/** Opens the audit tab. `prepare` runs after the sign-in (which writes its own event) and before the tab opens. */
async function openAudit(prepare: () => void = () => undefined) {
  const user = userEvent.setup()
  const app = await renderApp('/security')
  prepare()
  await user.click(await screen.findByRole('tab', { name: 'Audit log' }))
  return { user, ...app }
}

const event = (over: Partial<AuditEvent>): AuditEvent => ({
  id: 1,
  timestamp: new Date().toISOString(),
  actor: 'someone',
  source: 'panel',
  action: 'createRole',
  outcome: 'success',
  ...over,
})

describe('audit log: server column and partial answers (0.6.0)', () => {
  it('shows which server wrote each event, and a dash for an event that names none', async () => {
    getState().audit = [
      event({ id: 5, actor: 'on-twelve', serverId: '10.0.0.12' }),
      event({ id: 4, actor: 'on-none' }),
    ]
    await openAudit()
    const row12 = (await screen.findByText('on-twelve')).closest('tr') as HTMLElement
    expect(within(row12).getByTestId('audit-server')).toHaveTextContent('10.0.0.12')
    const rowNone = screen.getByText('on-none').closest('tr') as HTMLElement
    expect(within(rowNone).getByTestId('audit-server')).toHaveTextContent('-')
    expect(screen.getByRole('columnheader', { name: 'Server' })).toBeInTheDocument()
    expect(screen.queryByTestId('partial-notice')).toBeNull()
  })

  it('says the list is incomplete, names every missing server, and never calls it empty', async () => {
    mockConfig.partial = [
      { serverId: '10.0.0.12', reason: 'timeout' },
      { serverId: '10.0.0.13', reason: 'unreachable' },
    ]
    await openAudit(() => {
      getState().audit = []
    })
    const notice = await screen.findByTestId('partial-notice')
    expect(notice).toHaveTextContent(/this list is incomplete/i)
    expect(notice).toHaveTextContent(/Server 10\.0\.0\.12 did not answer in time/)
    expect(notice).toHaveTextContent(/Server 10\.0\.0\.13 could not be reached/)
    expect(notice).toHaveTextContent(/events written by these servers are not shown/i)
    expect(await screen.findByText(/no audit events from the servers that answered/i)).toBeVisible()
    expect(document.body).toHaveTextContent(/does not mean there are none/i)
  })

  it('shows hostile server ids and event servers as text', async () => {
    seedHostile(getState())
    mockConfig.partial = HOSTILE_MISSING
    await openAudit()
    const notice = await screen.findByTestId('partial-notice')
    expect(notice.textContent).toContain('U+202E')
    expect(notice.textContent).toContain(HOSTILE.markup)
    // The event whose server id is the reversed-text trick shows it with the code, not reversed.
    await waitFor(() =>
      expect(
        screen.getAllByTestId('audit-server').some((c) => (c.textContent ?? '').includes('U+202E')),
      ).toBe(true),
    )
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined()
    expect(document.body.textContent).not.toMatch(/‮/)
  })
})

describe('audit ids are opaque (0.6.0)', () => {
  it('keeps the order the server sent: rows are not sorted by id, and big ids do not matter', async () => {
    const big = 2 ** 53 - 1
    getState().audit = [
      event({ id: 2 ** 32 * 3 + 7, actor: 'first-row' }),
      event({ id: big, actor: 'second-row' }),
      event({ id: 5, actor: 'third-row' }),
      event({ id: 2 ** 32 + 1, actor: 'fourth-row' }),
    ]
    await openAudit()
    await screen.findByText('first-row')
    const order = ['first-row', 'second-row', 'third-row', 'fourth-row']
    const rows = screen.getAllByRole('row').map((r) => r.textContent ?? '')
    const positions = order.map((a) => rows.findIndex((t) => t.includes(a)))
    expect(positions).toEqual([...positions].sort((a, b) => a - b))
    expect(positions.every((p) => p > 0)).toBe(true)
  })

  it('pages through events whose ids are large and out of order, by cursor only', async () => {
    getState().audit = Array.from({ length: 120 }, (_, i) =>
      event({ id: 2 ** 40 + (i % 2 === 0 ? i : 1000 - i), actor: `actor-${i}` }),
    )
    const { user } = await openAudit()
    await screen.findByText('actor-0')
    await user.click(screen.getByRole('button', { name: /load more/i }))
    await screen.findByText('actor-60')
    expect(screen.getAllByRole('row').length).toBeGreaterThan(100)
  })
})

describe('audit log cursors (refused after an update)', () => {
  it('a 400 for a cursor starts again from the first page, with the short notice and no error', async () => {
    getState().audit = Array.from({ length: 120 }, (_, i) =>
      event({ id: 1000 + i, actor: `a-${i}` }),
    )
    const { user } = await openAudit()
    await screen.findByText('a-0')
    mockConfig.rejectCursors = true
    await user.click(screen.getByRole('button', { name: /load more/i }))
    const notice = await screen.findByTestId('cursor-reset-notice')
    expect(notice).toHaveTextContent(/started again from the first page/i)
    await screen.findByText('a-0')
    expect(screen.queryByText('a-60')).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

describe('an audited change while the audit log cannot be written (503)', () => {
  it('is explained, says nothing was changed, and nothing was', async () => {
    const user = userEvent.setup()
    await renderApp('/security')
    await user.click(await screen.findByRole('tab', { name: 'Network access' }))
    await user.type(await screen.findByLabelText('Rule 1 comment'), ' x')
    await user.click(screen.getByRole('button', { name: /review and apply/i }))
    const dialog = await screen.findByRole('dialog', { name: /network access rules/i })
    await within(dialog).findByText(/accepts them/i) // a dry run is not audited, so it still works
    mockConfig.auditUnwritable = true
    await user.click(within(dialog).getByRole('button', { name: /^apply on all servers$/i }))
    const notice = await waitFor(() => {
      const el = dialog.querySelector('[data-code="service_unavailable"]')
      if (!el) throw new Error('no notice yet')
      return el as HTMLElement
    })
    expect(notice).toHaveTextContent(/audit log cannot be written/i)
    expect(notice).toHaveTextContent(/nothing was changed/i)
    expect(getState().network.revision).toBe(7)
    expect(getState().network.rules[0]?.comment).toBe('cluster network')
  })
})
