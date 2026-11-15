// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// The server (0.4.0) answers 422 `unsupported_operation` for rebalance, failover and restoring the
// preferred primaries. The panel says "not available on this server version yet" only because of that
// answer, stops polling after it, keeps the actions visible and works as soon as the server does.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { mockConfig } from '@/mocks/state'
import { keys } from '@/api/queries'
import { renderApp } from './helpers'

const REBALANCE_OPS = [
  'getRebalanceStatus',
  'startRebalance',
  'previewRebalancePlan',
  'listRebalanceReports',
  'getRebalanceReport',
  'stopRebalance',
] as const

function allUnsupported(ops: readonly string[]): void {
  for (const op of ops) mockConfig.unsupported.add(op as never)
}

describe('rebalance page against a server that does not implement it', () => {
  it('shows the calm notice from the 422 answer and stops polling', async () => {
    allUnsupported(REBALANCE_OPS)
    const app = await renderApp('/rebalance')
    const notice = await screen.findByText(/not available on this server version yet/i)
    expect(notice.closest('[data-code]')).toHaveAttribute('data-code', 'unsupported_operation')
    expect(screen.getByRole('button', { name: /start rebalance/i })).toBeEnabled()
    const query = app.queryClient.getQueryCache().find({ queryKey: keys.rebalance })
    expect(query).toBeDefined()
    const interval = (
      query?.options as { refetchInterval: (q: unknown) => number | false }
    ).refetchInterval(query)
    expect(interval).toBe(false)
  })

  it('shows the answer to a clicked action even though the status could not be read', async () => {
    allUnsupported(REBALANCE_OPS)
    const user = userEvent.setup()
    await renderApp('/rebalance')
    await screen.findByText(/not available on this server version yet/i)
    const before = screen.getAllByText(/not available on this server version yet/i).length
    await user.click(screen.getByRole('button', { name: /preview plan/i }))
    await waitFor(() =>
      expect(screen.getAllByText(/not available on this server version yet/i).length).toBe(
        before + 1,
      ),
    )
    await user.click(screen.getByRole('button', { name: /start rebalance/i }))
    await waitFor(() =>
      expect(screen.getAllByText(/not available on this server version yet/i).length).toBe(
        before + 2,
      ),
    )
  })

  it('works without a panel change once the server implements it', async () => {
    const user = userEvent.setup()
    await renderApp('/rebalance')
    await screen.findByRole('heading', { name: 'Status' })
    expect(screen.queryByText(/not available on this server version yet/i)).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /preview plan/i }))
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument())
  })

  it('takes the wording from the answer, not from a list of operations', async () => {
    // Another operation answering 422 with another code is not described as "not available yet".
    mockServer.use(
      http.get('*/api/v1/rebalance', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 422, code: 'unprocessable', detail: 'Nope.' },
          { status: 422 },
        ),
      ),
    )
    await renderApp('/rebalance')
    await screen.findByText(/cannot be done/i)
    expect(screen.queryByText(/not available on this server version yet/i)).not.toBeInTheDocument()
  })
})

describe('failover and restoring primaries', () => {
  it('failover: the dialog shows the 422 as calm information, and works when supported', async () => {
    const user = userEvent.setup()
    mockConfig.unsupported.add('failoverServer')
    await renderApp('/servers')
    const failover = (await screen.findAllByRole('button', { name: /^failover$/i }))[0]
    await user.click(failover as HTMLElement)
    let dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /^graceful failover$/i }))
    const notice = await within(dialog).findByText(/not available on this server version yet/i)
    expect(notice.closest('[data-code]')?.getAttribute('role')).toBe('status')
    expect(within(dialog).getByRole('button', { name: /^graceful failover$/i })).toBeEnabled()
    await user.click(within(dialog).getByRole('button', { name: /^cancel$/i }))

    mockConfig.unsupported.delete('failoverServer')
    await user.click(
      (await screen.findAllByRole('button', { name: /^failover$/i }))[0] as HTMLElement,
    )
    dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /^graceful failover$/i }))
    await within(dialog).findByRole('progressbar')
  })

  it('restore preferred primaries: 422 is shown in the dialog, then it works when supported', async () => {
    const user = userEvent.setup()
    mockConfig.unsupported.add('restorePreferredPrimaries')
    await renderApp('/servers')
    await user.click(await screen.findByRole('button', { name: /restore preferred primaries/i }))
    let dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /^restore primaries$/i }))
    await within(dialog).findByText(/not available on this server version yet/i)
    await user.click(within(dialog).getByRole('button', { name: /^cancel$/i }))
    mockConfig.unsupported.delete('restorePreferredPrimaries')
    await user.click(screen.getByRole('button', { name: /restore preferred primaries/i }))
    dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /^restore primaries$/i }))
    await within(dialog).findByRole('progressbar')
  })
})
