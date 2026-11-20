// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.7.0: the audit actions of leases, standbys and `cancelStatement` have readable labels and are in the
// filter; the 16 `client*` metrics of the client entry point appear on the dashboard only when the server reports them.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { mockConfig, startStandbyReconcile } from '@/mocks/state'
import { auditActions } from '@/api/operations.generated'
import { AUDIT_ACTION_LABEL, auditActionLabel } from '@/lib/status'
import { applySwitches } from '@/mocks/switches'
import { renderApp } from './helpers'

const NEW_ACTIONS = [
  'cancelStatement',
  'leaseLost',
  'leaseFenced',
  'leaseAcquired',
  'standbyCreated',
  'standbyRemoved',
  'standbyRebuilt',
  'standbyInstanceStarted',
  'standbyInstanceRemoved',
  'standbyInstanceRebuilt',
  'synchronousStandbyNamesChanged',
  'replicationSlotDropped',
] as const

describe('audit actions of 0.7.0', () => {
  it('every action of the contract has words, and so do the twelve new ones', () => {
    for (const a of auditActions) expect(AUDIT_ACTION_LABEL[a]).toBeTruthy()
    for (const a of NEW_ACTIONS) {
      expect(auditActions).toContain(a)
      expect(auditActionLabel(a)).not.toBe(a)
    }
    // An action of a newer server is shown as sent.
    expect(auditActionLabel('somethingNew')).toBe('somethingNew')
  })

  it('the filter lists them with their words and filters by one', async () => {
    const s = getState()
    s.settings.defaultReplicaCount = 2
    startStandbyReconcile(s, 'admin')
    const user = userEvent.setup()
    await renderApp('/security')
    await user.click(await screen.findByRole('tab', { name: 'Audit log' }))
    const select = await screen.findByLabelText('Action')
    const names = within(select)
      .getAllByRole('option')
      .map((o) => o.textContent?.replace(/\s+/g, ' ').trim())
    for (const a of NEW_ACTIONS) expect(names).toContain(`${AUDIT_ACTION_LABEL[a]} (${a})`)
    await user.selectOptions(select, 'standbyCreated')
    await waitFor(
      () => expect(screen.getAllByTestId('audit-action')[0]).toHaveTextContent('Standby created'),
      { timeout: 30_000 },
    )
    expect(screen.getAllByTestId('audit-action')[0]).toHaveTextContent('standbyCreated')
  })

  it('shows lease events written by the system, and the cancel of a statement', async () => {
    const s = getState()
    s.audit.unshift(
      {
        id: 900,
        timestamp: new Date().toISOString(),
        actor: 'system',
        source: 'system',
        action: 'leaseFenced',
        resourceType: 'shardGroup',
        resourceId: '1',
        outcome: 'success',
        serverId: '10.0.0.11',
      },
      {
        id: 901,
        timestamp: new Date().toISOString(),
        actor: 'admin',
        source: 'panel',
        action: 'cancelStatement',
        resourceType: 'statement',
        resourceId: 'abc',
        outcome: 'success',
        serverId: '10.0.0.11',
      },
    )
    const user = userEvent.setup()
    await renderApp('/security')
    await user.click(await screen.findByRole('tab', { name: 'Audit log' }))
    const fenced = (await screen.findByText('Primary stopped because its lease was lost')).closest(
      'tr',
    )!
    expect(within(fenced).getByText('leaseFenced')).toBeInTheDocument()
    expect(within(fenced).getByText('shardGroup 1')).toBeInTheDocument()
    expect(screen.getByText('Cancel a workbench statement')).toBeInTheDocument()
  })
})

describe('the client entry point on the dashboard', () => {
  it('shows nothing, not even a heading, while the entry point is off (the default)', async () => {
    await renderApp('/dashboard')
    await screen.findByText('Memory used')
    await waitFor(() => expect(screen.queryByTestId('client-entry-point')).toBeNull())
    expect(screen.queryByText('Client entry point')).toBeNull()
    expect(screen.queryByTestId('metric-clientConnectionsActive')).toBeNull()
  })

  it('shows the eight charts when the entry point is on, one series per server', async () => {
    mockConfig.clientEntryPoint = 'on'
    await renderApp('/dashboard')
    const section = within(await screen.findByTestId('client-entry-point', {}, { timeout: 30_000 }))
    expect(section.getByRole('heading', { name: 'Client entry point' })).toBeInTheDocument()
    await waitFor(() => expect(section.getAllByTestId(/^metric-client/)).toHaveLength(8))
    expect(section.getByText('Connections open now')).toBeInTheDocument()
    await waitFor(() =>
      expect(section.getByTestId('metric-clientBytesOut')).toHaveTextContent(
        /bytes sent to clients/i,
      ),
    )
    // No standby entry point, no heading for it.
    expect(section.queryByText('Standby entry point')).toBeNull()
    // The legend is one line per series: a server each.
    const chart = section.getByTestId('metric-clientConnectionsActive')
    await waitFor(() =>
      expect(within(chart).getAllByText(/^Server 10\.0\.0\.1[123]$/).length).toBeGreaterThanOrEqual(
        3,
      ),
    )
  })

  it('shows the standby entry point as a second group when it is on too', async () => {
    mockConfig.clientEntryPoint = 'standby'
    await renderApp('/dashboard')
    const section = within(await screen.findByTestId('client-entry-point', {}, { timeout: 30_000 }))
    expect(await section.findByRole('heading', { name: 'Standby entry point' })).toBeInTheDocument()
    await waitFor(() => expect(section.getAllByTestId(/^metric-client/)).toHaveLength(16))
  })

  it('the URL switch turns it on', () => {
    applySwitches(new URLSearchParams('client=on'), getState())
    expect(mockConfig.clientEntryPoint).toBe('on')
  })

  it('an older server that refuses the metric names (400) shows nothing', async () => {
    mockServer.use(
      http.get('*/api/v1/metrics/query', ({ request }) => {
        const metric = new URL(request.url).searchParams.get('metric') ?? ''
        if (!metric.startsWith('client')) return undefined
        return HttpResponse.json(
          { type: 'about:blank', title: 'Bad', status: 400, code: 'validation_failed' },
          { status: 400 },
        )
      }),
    )
    await renderApp('/dashboard')
    await screen.findByText('Memory used')
    expect(screen.queryByTestId('client-entry-point')).toBeNull()
  })

  it('a server error of the probe is shown, not mistaken for "off"', async () => {
    mockServer.use(
      http.get('*/api/v1/metrics/query', ({ request }) => {
        const metric = new URL(request.url).searchParams.get('metric') ?? ''
        if (!metric.startsWith('client')) return undefined
        return HttpResponse.json(
          { type: 'about:blank', title: 'Boom', status: 500, code: 'internal_error' },
          { status: 500 },
        )
      }),
    )
    await renderApp('/dashboard')
    const section = await screen.findByTestId('client-entry-point', {}, { timeout: 30_000 })
    expect(within(section).getByText(/unexpected server error/i)).toBeInTheDocument()
  }, 90_000)

  it('a stats-only account sees them too (it may read metrics)', async () => {
    mockConfig.clientEntryPoint = 'on'
    await renderApp('/dashboard', { user: 'metrics' })
    expect(
      await screen.findByTestId('client-entry-point', {}, { timeout: 30_000 }),
    ).toBeInTheDocument()
  })
})
