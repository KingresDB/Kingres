// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.8.0: the Time tab (the clocks, their states, what to do, the cluster time zone, the time zone of a database,
// the alert threshold), the dashboard card and its links, the section Configuration of the server page, and the alert
// clockDifference.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { setClockOffset, setClockReachable } from '@/mocks/config'
import { calmClocks, renderApp } from './helpers'

const clock = async (id: string) => within(await screen.findByTestId(`clock-${id}`))

describe('the Time tab: the servers', () => {
  it('shows the clock, the difference with its uncertainty, a state and the zones of every server', async () => {
    await renderApp('/configuration?tab=time')
    const a = await clock('10.0.0.11')
    expect(a.getByText('answered this request')).toBeInTheDocument()
    expect(a.getByText('0 (reference)')).toBeInTheDocument()
    expect(a.getByText('In sync')).toBeInTheDocument()
    expect(a.getAllByText('Asia/Tehran')).toHaveLength(3)
    const c = await clock('10.0.0.13')
    expect(c.getByText('Differs')).toBeInTheDocument()
    expect(c.getByText('+1.82 s ± 0.4 ms')).toBeInTheDocument()
    expect(c.getByText('UTC')).toBeInTheDocument()
    expect(screen.getByTestId('measured-at')).toHaveTextContent(
      /Measured at .*UTC; the page measures again every 10 seconds/,
    )
  })

  it('shows a difference that is smaller than its uncertainty as within the measurement', async () => {
    await renderApp('/configuration?tab=time')
    const b = await clock('10.0.0.12')
    expect(b.getByText('within the measurement')).toBeInTheDocument()
    expect(b.getByText(/\+0\.3 ms\s*±\s*0\.4 ms/)).toBeInTheDocument()
    expect(b.getByText('In sync')).toBeInTheDocument()
  })

  it('marks the server whose zone differs from the others', async () => {
    await renderApp('/configuration?tab=time')
    const c = await clock('10.0.0.13')
    expect(c.getByText('differs from the others')).toBeInTheDocument()
    expect((await clock('10.0.0.11')).queryByText('differs from the others')).toBeNull()
  })

  it('says far off beyond ten times the threshold', async () => {
    setClockOffset(getState(), '10.0.0.12', -20000)
    await renderApp('/configuration?tab=time')
    const b = await clock('10.0.0.12')
    expect(b.getByText('Far off')).toBeInTheDocument()
    expect(b.getByText('-20.00 s ± 0.4 ms')).toBeInTheDocument()
  })

  it('says a clock cannot be read, with no number', async () => {
    setClockReachable(getState(), '10.0.0.12', false)
    await renderApp('/configuration?tab=time')
    const b = await clock('10.0.0.12')
    expect(b.getByText('Cannot be read')).toBeInTheDocument()
    expect(b.getAllByTestId('na').length).toBeGreaterThanOrEqual(4)
    expect(b.queryByText(/^0/)).toBeNull()
  })

  it('shows the state from the threshold of the cluster', async () => {
    mockConfig.latencyMs = 0
    getState().settings.clock = { maxDifferenceMs: 5000 }
    await renderApp('/configuration?tab=time')
    expect((await clock('10.0.0.13')).getByText('In sync')).toBeInTheDocument()
    expect(screen.queryByTestId('clock-advice')).toBeNull()
  })
})

describe('the Time tab: what to do', () => {
  it('says that KingresDB does not set the clock and what to run instead, when a clock differs', async () => {
    await renderApp('/configuration?tab=time')
    const advice = await screen.findByTestId('clock-advice')
    expect(advice).toHaveTextContent('KingresDB does not set the system clock')
    expect(advice).toHaveTextContent('rights of the operating system administrator')
    expect(advice).toHaveTextContent('NTP or chrony')
    expect(advice).toHaveTextContent('chronyc tracking')
    expect(advice).toHaveTextContent('timedatectl timesync-status')
    expect(advice).toHaveTextContent('not by hand with date')
    expect(
      screen.getByRole('heading', { level: 2, name: /what to do about a clock that differs/i }),
    ).toBeInTheDocument()
  })

  it('shows only a short note when every clock is within the threshold', async () => {
    calmClocks()
    await renderApp('/configuration?tab=time')
    await clock('10.0.0.11')
    expect(screen.queryByTestId('clock-advice')).toBeNull()
    expect(screen.getByTestId('clock-note')).toHaveTextContent('does not set the system clock')
  })
})

describe('the Time tab: the cluster time zone', () => {
  it('starts with the zone the servers have, and a zone that is not in the list is refused before anything is asked', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration?tab=time')
    const input = await screen.findByLabelText('Time zone')
    await waitFor(() => expect(input).toHaveValue('Asia/Tehran'))
    await user.clear(input)
    await user.type(input, 'Mars/Olympus')
    await user.click(screen.getByRole('button', { name: /^Apply/ }))
    expect(await screen.findByText(/does not know this time zone/)).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.querySelector('#time-zone-names option[value="Europe/Paris"]')).not.toBeNull()
  })

  it('sets TimeZone and log_timezone together through the same dialog, preview and task', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration?tab=time')
    const input = await screen.findByLabelText('Time zone')
    await user.clear(input)
    await user.type(input, 'Europe/Paris')
    await user.click(screen.getByRole('button', { name: /^Apply/ }))
    const d = within(await screen.findByRole('dialog', { name: 'Set the cluster time zone' }))
    // The level cannot be moved: the pair is a cluster setting.
    expect(d.getByRole('radio', { name: 'Cluster' })).toBeChecked()
    expect(d.getByRole('radio', { name: 'Database' })).toBeDisabled()
    expect(d.getByLabelText('Time zone')).toHaveValue('Europe/Paris')
    expect(await d.findByTestId('preview-sentence')).toHaveTextContent(
      '3 of 3 servers will change. 0 need a restart; nothing is restarted by this change.',
    )
    expect(d.getAllByText('TimeZone')).not.toHaveLength(0)
    expect(d.getAllByText('log_timezone')).not.toHaveLength(0)
    await waitFor(() =>
      expect(d.getByRole('button', { name: /^Apply/ })).not.toHaveAttribute('aria-disabled'),
    )
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    await d.findByText('Applied on every server.')
    await user.click(d.getByRole('button', { name: /^(Close|Hide \(keeps running\))$/ }))
    const c = await clock('10.0.0.13')
    await waitFor(() => expect(c.getAllByText('Europe/Paris')).toHaveLength(2))
  })

  it('is for administrators', async () => {
    await renderApp('/configuration?tab=time', { user: 'viewer' })
    const apply = await screen.findByRole('button', { name: /^Apply/ })
    expect(apply).toHaveAttribute('aria-disabled', 'true')
    expect(apply).toHaveAccessibleDescription(/requires the kingres_admin cluster role/i)
  })
})

describe('the Time tab: the time zone of each database', () => {
  it('lists the databases with their zone or the cluster default', async () => {
    await renderApp('/configuration?tab=time')
    const shop = within(await screen.findByTestId('dbzone-shop'))
    expect(shop.getByText('cluster default')).toBeInTheDocument()
    expect(
      within(screen.getByTestId('dbzone-reports')).getByText('Europe/Berlin'),
    ).toBeInTheDocument()
  })

  it('changes the zone of one database through the dialog', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration?tab=time')
    const shop = within(await screen.findByTestId('dbzone-shop'))
    await user.click(shop.getByRole('button', { name: /^Change/ }))
    const d = within(await screen.findByRole('dialog', { name: 'Set the time zone of a database' }))
    expect(d.getByRole('radio', { name: 'Database' })).toBeChecked()
    expect(d.getByRole('combobox', { name: 'Database' })).toHaveValue('shop')
    expect(d.getByRole('combobox', { name: 'Database' })).toBeDisabled()
    const zone = d.getByLabelText('Time zone')
    await user.clear(zone)
    await user.type(zone, 'Europe/Paris')
    expect(await d.findByTestId('preview-sentence')).toBeInTheDocument()
    await waitFor(() =>
      expect(d.getByRole('button', { name: /^Apply/ })).not.toHaveAttribute('aria-disabled'),
    )
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    await d.findByText('Applied on every server.')
    expect(getState().config.overrides.find((o) => o.scopeId === 'shop')?.value).toBe(
      'Europe/Paris',
    )
  })

  it('removes the zone of a database so that it follows the cluster again', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration?tab=time')
    const reports = within(await screen.findByTestId('dbzone-reports'))
    await user.click(reports.getByRole('button', { name: /^Change/ }))
    const d = within(await screen.findByRole('dialog', { name: 'Set the time zone of a database' }))
    expect(d.getByLabelText('Time zone')).toHaveValue('Europe/Berlin')
    await user.click(d.getByRole('button', { name: 'Remove this override' }))
    await d.findByTestId('preview-sentence')
    await waitFor(() =>
      expect(d.getByRole('button', { name: /^Apply/ })).not.toHaveAttribute('aria-disabled'),
    )
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    await d.findByText('Applied on every server.')
    expect(getState().config.overrides.some((o) => o.scopeId === 'reports')).toBe(false)
  })
})

describe('the Time tab: the threshold', () => {
  it('saves a new threshold with the cluster settings and the states follow', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration?tab=time')
    const input = await screen.findByLabelText('Threshold (milliseconds)')
    await waitFor(() => expect(input).toHaveValue(1000))
    await user.clear(input)
    await user.type(input, '5000')
    await user.click(screen.getByRole('button', { name: /^Save threshold/ }))
    expect(await screen.findByText('Threshold saved.')).toBeInTheDocument()
    expect(getState().settings.clock).toEqual({ maxDifferenceMs: 5000 })
    await waitFor(() => expect(screen.queryByTestId('clock-advice')).toBeNull())
    expect((await clock('10.0.0.13')).getByText('In sync')).toBeInTheDocument()
  })

  it('checks the range before it asks, and shows the refusal of the server', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration?tab=time')
    const input = await screen.findByLabelText('Threshold (milliseconds)')
    await waitFor(() => expect(input).toHaveValue(1000))
    await user.clear(input)
    await user.type(input, '5')
    await user.click(screen.getByRole('button', { name: /^Save threshold/ }))
    expect(await screen.findByText(/whole number between 10 and 600000/)).toBeInTheDocument()
    expect(getState().settings.clock).toEqual({ maxDifferenceMs: 1000 })
    mockServer.use(
      http.patch('*/api/v1/settings/cluster', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'x',
            status: 400,
            code: 'validation_failed',
            errors: [
              { field: 'clock.maxDifferenceMs', message: 'Between 10 and 600000 milliseconds.' },
            ],
          },
          { status: 400 },
        ),
      ),
    )
    await user.clear(input)
    await user.type(input, '50')
    await user.click(screen.getByRole('button', { name: /^Save threshold/ }))
    expect(await screen.findByText('Between 10 and 600000 milliseconds.')).toBeInTheDocument()
  })

  it('is for administrators', async () => {
    await renderApp('/configuration?tab=time', { user: 'ops' })
    expect(await screen.findByRole('button', { name: /^Save threshold/ })).toHaveAttribute(
      'aria-disabled',
      'true',
    )
  })
})

describe('the Time tab: states', () => {
  it('shows an error and a way to try again', async () => {
    mockServer.use(
      http.get('*/api/v1/config/time', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 503, code: 'service_unavailable' },
          { status: 503 },
        ),
      ),
    )
    await renderApp('/configuration?tab=time')
    expect((await screen.findByRole('alert')).getAttribute('data-code')).toBe('service_unavailable')
  })

  it('says calmly that the server does not have it yet', async () => {
    mockConfig.unsupported.add('getClusterTime')
    await renderApp('/configuration?tab=time')
    expect(await screen.findByText('Not available on this server version yet')).toBeInTheDocument()
  })

  it('shows loading first', async () => {
    mockConfig.latencyMs = 150
    await renderApp('/configuration?tab=time')
    expect((await screen.findAllByText('Loading')).length).toBeGreaterThan(0)
    await clock('10.0.0.11')
  })
})

describe('the dashboard card', () => {
  it('counts what waits, what differs and the clock, each as a link to the filtered page', async () => {
    await renderApp('/dashboard')
    const card = within(await screen.findByTestId('config-card'))
    expect(card.getByRole('heading', { level: 2, name: 'Configuration' })).toBeInTheDocument()
    await waitFor(() =>
      expect(card.getByTestId('cfg-pending')).toHaveTextContent('Servers waiting for a restart'),
    )
    const pending = within(card.getByTestId('cfg-pending')).getByRole('link')
    expect(pending).toHaveAccessibleName(/Servers waiting for a restart: 1/)
    expect(pending).toHaveAttribute('href', '/configuration?pendingRestart=1')
    const differing = within(card.getByTestId('cfg-differing')).getByRole('link')
    expect(differing).toHaveTextContent('2')
    expect(differing).toHaveAttribute('href', '/configuration?differing=1')
    const clockLink = within(card.getByTestId('cfg-clock')).getByRole('link')
    expect(clockLink).toHaveTextContent('1.82 s')
    expect(clockLink).toHaveTextContent('10.0.0.13')
    expect(clockLink).toHaveTextContent('Differs')
    expect(clockLink).toHaveAttribute('href', '/configuration?tab=time')
    // Nothing failed or waits: no row for it, and a sentence.
    expect(card.queryByTestId('cfg-failed')).toBeNull()
    expect(card.getByText('No failed or waiting server.')).toBeInTheDocument()
  })

  it('opens the page with the filter of the link', async () => {
    const user = userEvent.setup()
    const { router } = await renderApp('/dashboard')
    const card = within(await screen.findByTestId('config-card'))
    await user.click(await within(await card.findByTestId('cfg-pending')).findByRole('link'))
    expect(router.currentRoute.value.fullPath).toBe('/configuration?pendingRestart=1')
    expect(await screen.findByTestId('param-shared_buffers')).toBeInTheDocument()
  })

  it('shows the failed and the waiting servers when there are some, linked to their state', async () => {
    getState().config.servers['10.0.0.12']!.failed.work_mem = 'x'
    getState().config.servers['10.0.0.13']!.waiting = true
    getState().config.servers['10.0.0.13']!.waitDetail = 'The instance is not running.'
    await renderApp('/dashboard')
    const failed = await screen.findByTestId('cfg-failed')
    expect(within(failed).getByRole('link')).toHaveAttribute('href', '/configuration?state=failed')
    expect(within(failed).getByRole('link')).toHaveAccessibleName(
      /Servers where a change failed: 1/,
    )
    expect(within(screen.getByTestId('cfg-waiting')).getByRole('link')).toHaveAttribute(
      'href',
      '/configuration?state=waiting',
    )
  })

  it('says none for a zero, in words with a symbol', async () => {
    getState().config.servers['10.0.0.12']!.pending = {}
    getState().config.servers['10.0.0.12']!.run.shared_buffers = {
      value: '256MB',
      source: 'cluster',
    }
    getState().config.servers['10.0.0.12']!.run.checkpoint_timeout = {
      value: '5min',
      source: 'default',
    }
    calmClocks()
    await renderApp('/dashboard')
    const card = within(await screen.findByTestId('config-card'))
    await waitFor(() => expect(card.getByTestId('cfg-pending')).toHaveTextContent('none'))
    expect(card.getByTestId('cfg-differing')).toHaveTextContent('none')
    expect(card.queryAllByRole('link', { name: /Servers waiting/ })).toHaveLength(0)
    expect(card.getByTestId('cfg-clock')).toHaveTextContent('0 ms')
  })

  it('says not available for a clock that could not be measured, never 0', async () => {
    setClockReachable(getState(), '10.0.0.12', false)
    setClockReachable(getState(), '10.0.0.13', false)
    await renderApp('/dashboard')
    const card = within(await screen.findByTestId('config-card'))
    await waitFor(() => expect(card.getByTestId('cfg-clock')).toHaveTextContent('Not available'))
    expect(card.getByTestId('cfg-clock')).not.toHaveTextContent('0 ms')
  })

  it('is not there for an account with only the metrics role', async () => {
    await renderApp('/dashboard', { user: 'metrics' })
    await screen.findByTestId('cluster-summary')
    expect(screen.queryByTestId('config-card')).toBeNull()
  })

  it('shows the error of the server, calmly when it does not have this yet', async () => {
    mockConfig.unsupported.add('getConfigSummary')
    await renderApp('/dashboard')
    const card = within(await screen.findByTestId('config-card'))
    expect(await card.findByText('Not available on this server version yet')).toBeInTheDocument()
  })
})

describe('the section Configuration of the server page', () => {
  it('shows the group, the revision, the overrides, what waits for a restart and the clock', async () => {
    await renderApp('/servers/10.0.0.12')
    const section = within(await screen.findByTestId('server-config'))
    expect(await section.findByText('Pending restart')).toBeInTheDocument()
    expect(section.getByText('rack-a')).toBeInTheDocument()
    expect(screen.getByTestId('server-config')).toHaveTextContent(/6\s*of 6/)
    const pending = section.getByTestId('config-pending')
    expect(pending).toHaveTextContent('shared_buffers')
    expect(pending).toHaveTextContent('downtime for its shard group')
    // The overrides that apply to this server: the cluster's two, the group's one.
    await waitFor(() => expect(section.getAllByRole('row').length).toBeGreaterThan(3))
    expect(section.getByText('effective_io_concurrency')).toBeInTheDocument()
    expect(section.getAllByText('Server group').length).toBeGreaterThan(0)
    expect(section.queryByText('32MB')).toBeNull()
    expect(section.getByTestId('server-clock')).toHaveTextContent('within the measurement')
  })

  it('shows the overrides of the server itself and a clock that is off', async () => {
    await renderApp('/servers/10.0.0.13')
    const section = within(await screen.findByTestId('server-config'))
    await waitFor(() => expect(section.getByText('32MB')).toBeInTheDocument())
    expect(section.getByTestId('server-clock')).toHaveTextContent('+1.82 s ± 0.4 ms')
    expect(section.getByTestId('server-clock')).toHaveTextContent('Differs')
    expect(section.getByTestId('server-clock')).toHaveTextContent('UTC')
  })

  it('restarts the instances of this server after the downtime is acknowledged', async () => {
    const user = userEvent.setup()
    await renderApp('/servers/10.0.0.12')
    const section = within(await screen.findByTestId('server-config'))
    await user.click(
      await section.findByRole('button', { name: "Restart this server's instances" }),
    )
    const d = within(await screen.findByRole('dialog', { name: 'Restart pending instances' }))
    expect(await d.findByTestId('waiting-servers')).toHaveTextContent('10.0.0.12')
    await user.click(d.getByRole('checkbox', { name: /I understand/ }))
    await user.click(d.getByRole('button', { name: /^Restart now/ }))
    expect(await d.findByText(/Every instance was restarted/)).toBeInTheDocument()
  })

  it('shows what failed with the text of PostgreSQL', async () => {
    getState().config.servers['10.0.0.12']!.failed.work_mem =
      'invalid value for parameter "work_mem"'
    await renderApp('/servers/10.0.0.12')
    expect(await screen.findByTestId('config-failed')).toHaveTextContent(
      'invalid value for parameter "work_mem"',
    )
  })

  it('is not there for a role that cannot read it, and calm when the server does not have it', async () => {
    mockConfig.unsupported.add('listConfigServers')
    await renderApp('/servers/10.0.0.12')
    const section = within(await screen.findByTestId('server-config'))
    expect(await section.findByText('Not available on this server version yet')).toBeInTheDocument()
  })
})

describe('the alert clockDifference', () => {
  it('has words, an explanation, the server and a link to the Time tab', async () => {
    await renderApp('/alerts')
    const title = await screen.findByText(/Clock of server 10\.0\.0\.13 differs from the leader/)
    expect(title).toBeInTheDocument()
    expect(screen.getAllByText('Server clock differs from the leader').length).toBeGreaterThan(0)
    expect(
      screen.getAllByText(/does not set the system clock: run time synchronization/)[0]!,
    ).toBeInTheDocument()
    const link = screen.getByRole('link', { name: 'Clocks and what to do' })
    expect(link).toHaveAttribute('href', '/configuration?tab=time')
    expect(
      within(
        screen.getByTestId('alert-clock').closest('[data-testid="alert-subject"]') as HTMLElement,
      ).getByRole('link', { name: '10.0.0.13' }),
    ).toBeInTheDocument()
  })

  it('is on the dashboard among the open alerts', async () => {
    await renderApp('/dashboard')
    expect(
      await screen.findByText(/Clock of server 10\.0\.0\.13 differs from the leader/),
    ).toBeInTheDocument()
  })
})
