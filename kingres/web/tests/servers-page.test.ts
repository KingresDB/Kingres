// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { fakeHash } from '@/mocks/state'
import { SERVER_STATUS } from '@/lib/status'
import { renderApp } from './helpers'

describe('Servers page', () => {
  it('renders every status as icon plus text, never colour alone', async () => {
    const s = getState()
    s.servers[1]!.status = 'suspect'
    s.servers[2]!.status = 'pendingRebalance'
    await renderApp('/servers')

    const row1 = await screen.findByTestId('server-row-10.0.0.11')
    const row2 = screen.getByTestId('server-row-10.0.0.12')
    const row3 = screen.getByTestId('server-row-10.0.0.13')
    expect(within(row1).getByText('LIVE')).toBeInTheDocument()
    expect(within(row2).getByText('SUSPECT')).toBeInTheDocument()
    expect(within(row3).getByText('PENDING REBALANCE')).toBeInTheDocument()
    // The icon is decorative (aria-hidden) and the label carries the meaning.
    const badge = within(row3).getByText('PENDING REBALANCE').closest('.badge')
    expect(badge?.querySelector('[aria-hidden="true"]')?.textContent).toBe(
      SERVER_STATUS.pendingRebalance.icon,
    )
  })

  it('has a distinct icon and label for each of the six statuses', () => {
    const labels = Object.values(SERVER_STATUS).map((m) => m.label)
    expect(new Set(labels).size).toBe(6)
    expect(labels).toEqual(
      expect.arrayContaining([
        'LIVE',
        'SUSPECT',
        'DRAINING',
        'DECOMMISSIONING',
        'DEAD',
        'PENDING REBALANCE',
      ]),
    )
    // Tone-only differences would not help colour-blind users: icons differ where tones repeat.
    const infoIcons = Object.values(SERVER_STATUS)
      .filter((m) => m.tone === 'info')
      .map((m) => `${m.icon}${m.label}`)
    expect(new Set(infoIcons).size).toBe(infoIcons.length)
  })

  it('shows instances with role and shard group, bucket counts, size, lag and version', async () => {
    await renderApp('/servers')
    const row1 = await screen.findByTestId('server-row-10.0.0.11')
    expect(within(row1).getByText('primary of group 1')).toBeInTheDocument()
    expect(within(row1).getByText('primary of map group')).toBeInTheDocument()
    // Chained placement: server 1 also holds the standby of group 3.
    expect(within(row1).getByText('standby of group 3')).toBeInTheDocument()
    expect(within(row1).getByText('rack-a')).toBeInTheDocument()
    expect(within(row1).getByText(/manager leader/i)).toBeInTheDocument()
    expect(within(row1).getByText('0.1.0')).toBeInTheDocument()
    expect(within(row1).getByText(/^\d+ \/ \d+$/)).toBeInTheDocument()
  })

  it('asks for confirmation, states the consequence, then follows the failover task', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const row2 = await screen.findByTestId('server-row-10.0.0.12')
    await user.click(within(row2).getByRole('button', { name: /failover/i }))
    const dialog = await screen.findByRole('dialog', { name: /fail over server/i })
    expect(within(dialog).getByText(/hand over to their standbys/i)).toBeInTheDocument()

    await user.click(within(dialog).getByRole('radio', { name: /hard/i }))
    expect(within(dialog).getByRole('alert')).toHaveTextContent(/marks .* as dead/i)

    // Marking a server dead needs its address typed.
    expect(within(dialog).getByRole('button', { name: /hard failover/i })).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.12:8091')
    await user.click(within(dialog).getByRole('button', { name: /hard failover/i }))
    expect(await within(dialog).findByRole('progressbar')).toBeInTheDocument()
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: /^close$/i }))

    await waitFor(() =>
      expect(
        within(screen.getByTestId('server-row-10.0.0.12')).getByText('DEAD'),
      ).toBeInTheDocument(),
    )
    expect(getState().servers[1]?.status).toBe('dead')
  })

  it('adds a server that then shows as pending rebalance and raises the banner', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    await user.click(await screen.findByRole('button', { name: /add server/i }))
    const dialog = await screen.findByRole('dialog', { name: /add server/i })
    await user.type(within(dialog).getByLabelText(/server address/i), '10.0.0.14:8091')
    await user.type(within(dialog).getByLabelText(/setup token/i), 'demo-setup-token')
    await user.type(
      within(dialog).getByLabelText(/expected fingerprint/i),
      fakeHash('bootstrap-10.0.0.14'),
    )
    await user.click(within(dialog).getByRole('button', { name: /^add server$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: /^close$/i }))

    expect(await screen.findByTestId('server-row-10.0.0.14')).toBeInTheDocument()
    expect(
      within(screen.getByTestId('server-row-10.0.0.14')).getByText('PENDING REBALANCE'),
    ).toBeInTheDocument()
    expect(await screen.findByTestId('banner-rebalance')).toHaveTextContent(/rebalance needed/i)
  })

  it('shows a calm message when the server version does not implement the operation', async () => {
    const { mockConfig } = await import('@/mocks/state')
    mockConfig.unsupported.add('listServers')
    await renderApp('/servers')
    expect(await screen.findByText(/not available on this server version yet/i)).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('shows an error with a retry for a transient failure', async () => {
    const { mockServer } = await import('@/mocks/server')
    const { http, HttpResponse } = await import('msw')
    mockServer.use(
      http.get('*/api/v1/servers', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 503, code: 'leader_unavailable' },
          { status: 503 },
        ),
      ),
    )
    await renderApp('/servers')
    expect(await screen.findByText(/no manager leader/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument()
  })
})
