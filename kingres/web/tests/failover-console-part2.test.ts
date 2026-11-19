// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The failover console, part 2 (design 8.3 banner, 8.4 settings and counter, 8.5 alerts, 8.6 roles, 8.7 mock states): the
// banner in words for every state of a run, the counter and its reset, the settings that follow the contract (30 seconds
// at least), the limit on the dashboard and in the alerts, what each role sees and does, and the mock states.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { axe } from 'vitest-axe'
import { createPinia } from 'pinia'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { endpoints } from '@/api/endpoints'
import type { FailoverStatus } from '@/api/types'
import { failoverBanner, autoFailoverCounterWords } from '@/lib/failover'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { ok } from './schema-check'
import { calmClocks, renderApp, signInAs } from './helpers'

const sw = (query: string) => applySwitches(new URLSearchParams(query), getState())

const status = (over: Partial<FailoverStatus> = {}): FailoverStatus => ({
  state: 'running',
  phase: 'promoting',
  kind: 'failover',
  trigger: 'manual',
  taskId: 7,
  serverId: '10.0.0.12',
  startedAt: '2026-11-19T10:00:00Z',
  pausedReason: null,
  needsDecision: [],
  groups: [
    { shardGroupId: 1, state: 'done', fromServerId: '10.0.0.12', toServerId: '10.0.0.13' },
    { shardGroupId: 2, state: 'frozen', fromServerId: '10.0.0.12', toServerId: '10.0.0.13' },
    { shardGroupId: 3, state: 'pending', fromServerId: '10.0.0.12', toServerId: '10.0.0.11' },
  ],
  cancellable: true,
  lastReport: null,
  ...over,
})

describe('the banner in words', () => {
  it('says which group of how many and what is done, for a run that goes on', () => {
    expect(failoverBanner(status())).toEqual({
      tone: 'info',
      icon: '◐',
      heading: 'Failover of server 10.0.0.12 is running: group 2 of 3.',
      detail: 'Promoting the standby',
    })
  })
  it('names an automatic run and the restore of the preferred primaries', () => {
    expect(failoverBanner(status({ trigger: 'automatic' }))?.heading).toMatch(
      /^Automatic failover of server 10\.0\.0\.12 is running/,
    )
    expect(failoverBanner(status({ kind: 'restorePreferredPrimaries' }))?.heading).toMatch(
      /^Restoring the preferred primaries is running/,
    )
  })
  it('is danger and says "waiting for you" when a decision is needed', () => {
    const b = failoverBanner(
      status({
        state: 'paused',
        needsDecision: [
          { code: 'failoverNeedsDecision', shardGroupId: 2, message: 'm' },
        ] as FailoverStatus['needsDecision'],
      }),
    )
    expect(b).toMatchObject({ tone: 'danger', heading: expect.stringMatching(/waiting for you/) })
    expect(b?.detail).toBe('A decision is needed for 1 group.')
  })
  it('says where the manager moves to while the map group is moved', () => {
    const b = failoverBanner(
      status({
        phase: 'bootstrap',
        taskId: null,
        groups: [
          {
            shardGroupId: 0,
            state: 'promoted',
            fromServerId: '10.0.0.11',
            toServerId: '10.0.0.12',
          },
        ],
      }),
    )
    expect(b?.heading).toBe('The manager is moving to 10.0.0.12.')
  })
  it('shows nothing for an idle, a failed or an absent run', () => {
    expect(failoverBanner(status({ state: 'idle' }))).toBeNull()
    expect(failoverBanner(status({ state: 'failed' }))).toBeNull()
    expect(failoverBanner(null)).toBeNull()
  })
})

describe('the banner in the shell', () => {
  it('appears for an automatic run and links to the progress', async () => {
    mockConfig.taskDurationMs = 60_000
    sw('failover=auto:10.0.0.12')
    await renderApp('/dashboard', { user: 'ops' })
    const banner = await screen.findByTestId('banner-failover')
    expect(banner).toHaveTextContent(/Automatic failover of server 10\.0\.0\.12 is running/)
    expect(within(banner).getByRole('link', { name: /follow the failover/i })).toHaveAttribute(
      'href',
      '/replication#failover-top',
    )
  })
  it('is not shown to kingres_stats, which may not read the status', async () => {
    mockConfig.taskDurationMs = 60_000
    sw('failover=auto:10.0.0.12')
    await renderApp('/dashboard', { user: 'metrics' })
    await screen.findByTestId('cluster-summary')
    expect(screen.queryByTestId('banner-failover')).not.toBeInTheDocument()
  })
  it('is not shown when the server does not know the status yet', async () => {
    mockConfig.taskDurationMs = 60_000
    sw('failover=auto:10.0.0.12')
    mockConfig.unsupported.add('getFailoverStatus')
    await renderApp('/dashboard')
    await screen.findByText(/auto-failover/i)
    expect(screen.queryByTestId('banner-failover')).not.toBeInTheDocument()
  })
  it('is danger and says "waiting for you" for a run that needs a decision', async () => {
    mockConfig.taskDurationMs = 400
    sw('failover=needs-decision')
    const app = await renderApp('/dashboard')
    await endpoints.failoverServer('10.0.0.12', 'hard')
    await app.queryClient.invalidateQueries()
    await waitFor(
      () => expect(screen.getByTestId('banner-failover')).toHaveTextContent(/waiting for you/i),
      { timeout: 5000 },
    )
    expect(screen.getByTestId('banner-failover')).toHaveClass('tone-danger')
  })
})

describe('the words of the counter', () => {
  it('counts n of m, names the last event and finds the limit from the numbers', () => {
    expect(
      autoFailoverCounterWords({
        eventsSinceReset: 1,
        maxCount: 3,
        lastEventAt: '2026-11-19T10:00:00Z',
      }),
    ).toEqual({
      counter: '1 of 3',
      last: 'Last automatic failover: 19 Nov 2026, 10:00:00 UTC.',
      limitReached: false,
      empty: false,
    })
    expect(autoFailoverCounterWords({ eventsSinceReset: 3, maxCount: 3 }).limitReached).toBe(true)
    expect(autoFailoverCounterWords({ eventsSinceReset: 0, maxCount: 3 })).toMatchObject({
      empty: true,
      last: expect.stringMatching(/^No automatic failover has been made/),
    })
  })
})

describe('the Settings page: automatic failover', () => {
  it('shows the counter, the last event and the requirements in words', async () => {
    sw('failover=count:2:5')
    await renderApp('/settings')
    expect(await screen.findByTestId('af-counter')).toHaveTextContent('2 of 5')
    expect(screen.getByTestId('af-last')).toHaveTextContent(/^Last automatic failover: /)
    expect(screen.queryByTestId('af-limit')).not.toBeInTheDocument()
    const req = screen.getByTestId('af-requirements')
    expect(req).toHaveTextContent(/at least three servers/i)
    expect(req).toHaveTextContent(/a standby for every shard group/i)
    expect(req).toHaveTextContent(/synchronous standby/i)
  })

  it('says that the limit is reached', async () => {
    sw('failover=limit')
    await renderApp('/settings')
    expect(await screen.findByTestId('af-limit')).toHaveTextContent(
      'The limit is reached: further failures need an operator.',
    )
  })

  it('asks for at least 30 seconds, as the contract does', async () => {
    const user = userEvent.setup()
    await renderApp('/settings')
    const timeout = await screen.findByLabelText(/timeout \(seconds\)/i)
    await user.clear(timeout)
    await user.type(timeout, '10')
    await user.click(screen.getByRole('button', { name: /save settings/i }))
    expect(
      await screen.findByText('The timeout must be a whole number between 30 and 3600.'),
    ).toBeInTheDocument()
    expect(getState().settings.autoFailover.timeoutSeconds).toBe(120)
  })

  it('resets the counter after the confirmation, and the limit line and the alert go away', async () => {
    const user = userEvent.setup()
    sw('failover=limit')
    await renderApp('/settings')
    await screen.findByTestId('af-limit')
    await user.click(screen.getByRole('button', { name: /^reset counter$/i }))
    const dialog = await screen.findByRole('dialog', { name: /reset the counter/i })
    expect(within(dialog).getByTestId('af-reset-text')).toHaveTextContent(
      'Automatic failover will be allowed again up to 1 time.',
    )
    // Nothing was reset by opening the dialog.
    expect(getState().settings.autoFailover.eventsSinceReset).toBe(1)
    await user.click(within(dialog).getByRole('button', { name: /^reset counter$/i }))
    await waitFor(() => expect(screen.getByTestId('af-counter')).toHaveTextContent('0 of 1'))
    expect(screen.queryByTestId('af-limit')).not.toBeInTheDocument()
    expect(screen.getByTestId('af-reset-done')).toBeInTheDocument()
    expect(
      getState().alerts.filter(
        (a) => a.kind === 'autoFailoverLimitReached' && a.state !== 'resolved',
      ),
    ).toHaveLength(0)
  })

  it('cancelling the confirmation resets nothing', async () => {
    const user = userEvent.setup()
    sw('failover=limit')
    await renderApp('/settings')
    await user.click(await screen.findByRole('button', { name: /^reset counter$/i }))
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: /^cancel$/i }),
    )
    expect(getState().settings.autoFailover.eventsSinceReset).toBe(1)
    expect(screen.getByTestId('af-counter')).toHaveTextContent('1 of 1')
  })

  it('a server that does not know the reset: calm information, nothing changed', async () => {
    const user = userEvent.setup()
    sw('failover=limit')
    mockConfig.unsupported.add('resetAutoFailoverCounter')
    await renderApp('/settings')
    await user.click(await screen.findByRole('button', { name: /^reset counter$/i }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /^reset counter$/i }))
    expect(
      await within(dialog).findByText(/not available on this server version yet/i),
    ).toBeInTheDocument()
    expect(getState().settings.autoFailover.eventsSinceReset).toBe(1)
    expect(screen.queryByTestId('af-reset-done')).not.toBeInTheDocument()
  })

  it('a counter at 0 has nothing to reset: the button says so', async () => {
    await renderApp('/settings')
    const button = await screen.findByRole('button', { name: /^reset counter/i })
    expect(button).toHaveAttribute('aria-disabled', 'true')
    expect(button).toHaveTextContent('The counter is at 0 already.')
  })

  it('only kingres_admin can reset: ops and read-only see the button disabled with the reason', async () => {
    sw('failover=limit')
    for (const user of ['ops', 'viewer']) {
      const app = await renderApp('/settings', { user })
      const button = await screen.findByRole('button', { name: /^reset counter/i })
      expect(button).toHaveAttribute('aria-disabled', 'true')
      expect(button).toHaveTextContent(/kingres_admin/)
      app.unmount()
    }
    await renderApp('/settings', { user: 'admin' })
    expect(await screen.findByRole('button', { name: /^reset counter/i })).not.toHaveAttribute(
      'aria-disabled',
    )
  })

  it('has no accessibility violations, with the limit and with the dialog open', async () => {
    const user = userEvent.setup()
    sw('failover=limit')
    const { container } = await renderApp('/settings')
    await screen.findByTestId('af-limit')
    expect(await axe(container)).toHaveNoViolations()
    await user.click(screen.getByRole('button', { name: /^reset counter$/i }))
    const dialog = await screen.findByRole('dialog')
    expect(await axe(dialog)).toHaveNoViolations()
  })
})

describe('the dashboard, the alerts and the counter in the mock', () => {
  it('the dashboard card says that the limit is reached and links to Settings', async () => {
    sw('failover=limit')
    await renderApp('/dashboard')
    const card = await screen.findByTestId('kpi-auto-failover')
    expect(within(card).getByTestId('kpi-auto-failover-limit')).toHaveTextContent(
      /the limit is reached/i,
    )
    expect(within(card).getByRole('link', { name: /settings/i })).toHaveAttribute(
      'href',
      '/settings',
    )
  })

  it('the alert of the limit links to the counter on Settings', async () => {
    sw('failover=limit')
    calmClocks()
    await renderApp('/alerts')
    const links = await screen.findAllByTestId('alert-auto-failover-settings')
    expect(links[0]?.querySelector('a')).toHaveAttribute('href', '/settings')
  })

  it('failover=count:n:m, failover=off and failover=limit reach their states', async () => {
    await signInAs(createPinia())
    sw('failover=count:2:5,off')
    let s = await endpoints.getClusterSettings()
    ok(s, 'ClusterSettings')
    expect(s.autoFailover).toMatchObject({ enabled: false, eventsSinceReset: 2, maxCount: 5 })
    expect(s.autoFailover.lastEventAt).toBeTruthy()
    expect(getState().alerts.some((a) => a.kind === 'autoFailoverLimitReached')).toBe(false)
    sw('failover=count:5:5')
    s = await endpoints.getClusterSettings()
    expect(s.autoFailover.eventsSinceReset).toBe(5)
    expect(getState().alerts.some((a) => a.kind === 'autoFailoverLimitReached')).toBe(true)
  })

  it('a failover started by a person is not counted', async () => {
    mockConfig.taskDurationMs = 60
    await signInAs(createPinia())
    const manual = await endpoints.failoverServer('10.0.0.12', 'hard')
    await waitFor(async () => expect((await endpoints.getTask(manual.id)).state).toBe('done'))
    expect(getState().settings.autoFailover.eventsSinceReset).toBe(0)
    expect(getState().alerts.some((a) => a.kind === 'autoFailoverTriggered')).toBe(false)
  })

  it('an automatic run that ends well raises the info alert and, at the limit, the warning', async () => {
    mockConfig.taskDurationMs = 60
    await signInAs(createPinia())
    sw('failover=auto:10.0.0.12')
    await waitFor(
      async () => {
        await endpoints.getFailoverStatus()
        expect(getState().settings.autoFailover.eventsSinceReset).toBe(1)
      },
      { timeout: 5000 },
    )
    const kinds = getState().alerts.map((a) => a.kind)
    expect(kinds).toContain('autoFailoverTriggered')
    expect(kinds).toContain('autoFailoverLimitReached')
    // The reset clears the limit alert and keeps the time of the last event.
    const after = await endpoints.resetAutoFailoverCounter()
    expect(after.autoFailover.eventsSinceReset).toBe(0)
    expect(after.autoFailover.lastEventAt).toBeTruthy()
    expect(
      getState().alerts.some(
        (a) => a.kind === 'autoFailoverLimitReached' && a.state !== 'resolved',
      ),
    ).toBe(false)
  })
})

describe('the reset of the counter, refused', () => {
  const CASES: [string, number, RegExp][] = [
    ['forbidden', 403, /does not have the cluster role/i],
    ['leader_unavailable', 503, /has not heard from the cluster manager leader/i],
    ['unsupported_operation', 422, /not available on this server version yet/i],
  ]
  for (const [code, status, words] of CASES) {
    it(`${code} is told in its own words and the counter stays`, async () => {
      const user = userEvent.setup()
      sw('failover=limit')
      mockServer.use(
        http.post('*/api/v1/settings/cluster/auto-failover/reset', () =>
          HttpResponse.json(
            { type: 'about:blank', title: 'x', status, code, detail: 'd' },
            { status },
          ),
        ),
      )
      await renderApp('/settings')
      await user.click(await screen.findByRole('button', { name: /^reset counter$/i }))
      const dialog = await screen.findByRole('dialog')
      await user.click(within(dialog).getByRole('button', { name: /^reset counter$/i }))
      expect(await within(dialog).findByText(words)).toBeInTheDocument()
      expect(screen.queryByTestId('af-reset-done')).not.toBeInTheDocument()
      expect(getState().settings.autoFailover.eventsSinceReset).toBe(1)
    })
  }
})

describe('hostile text in the banner', () => {
  it('shows a hostile server id as visible marks, not as markup or direction control', () => {
    const b = failoverBanner(status({ serverId: 'x\u202Eevil<img src=x>' }))
    expect(b?.heading).not.toContain('\u202E')
    expect(b?.heading).toContain('U+202E')
  })
})
