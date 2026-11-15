// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-03: the matrix shows what the server has, also after a failed change.
// W-04: a grant template is confirmed, with what it grants, before it is applied.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { renderApp } from './helpers'

const forbidden = () =>
  HttpResponse.json(
    { type: 'about:blank', title: 'Forbidden', status: 403, code: 'forbidden' },
    { status: 403 },
  )

async function openAccess() {
  const user = userEvent.setup()
  const app = await renderApp('/security')
  await user.click(await screen.findByRole('tab', { name: 'Access' }))
  await screen.findByRole('option', { name: 'app_rw' })
  await user.selectOptions(screen.getByLabelText('Role'), 'app_rw')
  await user.selectOptions(screen.getByLabelText('Database'), 'shop')
  await screen.findByRole('checkbox', { name: /delete on table public\.orders/i })
  return { user, ...app }
}

describe('the grant matrix after a failed change (W-03)', () => {
  it('a grant that failed is not shown as granted, and the person is told', async () => {
    const { user } = await openAccess()
    mockServer.use(http.post('*/api/v1/grants', forbidden))
    const box = screen.getByRole('checkbox', { name: /delete on table public\.orders/i })
    expect(box).not.toBeChecked()
    await user.click(box)
    expect(await screen.findByText(/was not applied/i)).toBeInTheDocument()
    await waitFor(() =>
      expect(
        screen.getByRole('checkbox', { name: /delete on table public\.orders/i }),
      ).not.toBeChecked(),
    )
  })

  it('a revoke that failed is still shown as granted', async () => {
    const { user } = await openAccess()
    mockServer.use(http.post('*/api/v1/grants/revocations', forbidden))
    const box = screen.getByRole('checkbox', { name: /select on table public\.orders/i })
    expect(box).toBeChecked()
    await user.click(box)
    expect(await screen.findByText(/was not applied/i)).toBeInTheDocument()
    await waitFor(() =>
      expect(
        screen.getByRole('checkbox', { name: /select on table public\.orders/i }),
      ).toBeChecked(),
    )
  })
})

describe('grant templates need a confirmation (W-04)', () => {
  it('shows what the owner template grants and asks for the database name', async () => {
    const { user } = await openAccess()
    const requests: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      if (request.method === 'POST') requests.push(new URL(request.url).pathname)
    })
    await user.click(screen.getByRole('radio', { name: /database owner/i }))
    await user.click(screen.getByRole('button', { name: /apply template to app_rw/i }))
    const dialog = await screen.findByRole('dialog', { name: /app_rw.*shop/i })
    expect(requests.filter((p) => p.endsWith('/template-applications'))).toHaveLength(0)
    expect(within(dialog).getByText(/can change the structure/i)).toBeInTheDocument()
    const apply = within(dialog).getByRole('button', { name: /^apply template$/i })
    expect(apply).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/to confirm/i), 'shop')
    await user.click(apply)
    await waitFor(() =>
      expect(requests.filter((p) => p.endsWith('/template-applications'))).toHaveLength(1),
    )
    await waitFor(() =>
      expect(
        getState().grants.find((g) => g.grantee === 'app_rw' && g.level === 'table')?.privileges,
      ).toContain('delete'),
    )
  })

  it('cancelling applies nothing', async () => {
    const { user } = await openAccess()
    const requests: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      if (request.method === 'POST') requests.push(new URL(request.url).pathname)
    })
    await user.click(screen.getByRole('button', { name: /apply template to app_rw/i }))
    const dialog = await screen.findByRole('dialog', { name: /app_rw.*shop/i })
    expect(within(dialog).getByText(/select/i)).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: /^cancel$/i }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(requests.filter((p) => p.endsWith('/template-applications'))).toHaveLength(0)
  })
})
