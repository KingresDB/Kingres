// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.8.0, the page Configuration (tab Parameters): every state (loading, empty result of a filter, error,
// partial, unsupported), every role, the four states of a server, managed, refused and hidden parameters, the filters
// in the address, paging with a live region, and what a row says.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { renderApp } from './helpers'

const row = async (name: string) => within(await screen.findByTestId(`param-${name}`))

describe('the page and its tabs', () => {
  it('is in the Cluster navigation after Settings, with three tabs', async () => {
    await renderApp('/configuration')
    expect(
      await screen.findByRole('heading', { level: 1, name: 'Configuration' }),
    ).toBeInTheDocument()
    const nav = within(screen.getByRole('navigation', { name: 'Main' }))
    const links = nav.getAllByRole('link').map((a) => a.textContent?.trim())
    expect(links.indexOf('Configuration')).toBe(links.indexOf('Settings') + 1)
    const tabs = screen.getAllByRole('tab').map((t) => t.textContent)
    expect(tabs).toEqual(['Parameters', 'Changes', 'Time'])
    expect(screen.getByRole('tab', { name: 'Parameters' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('tabpanel')).toHaveAttribute('aria-labelledby', 'tab-parameters')
  })

  it('switches tabs by click and by the address', async () => {
    const user = userEvent.setup()
    const { router } = await renderApp('/configuration')
    await user.click(await screen.findByRole('tab', { name: 'Changes' }))
    expect(router.currentRoute.value.query).toEqual({ tab: 'changes' })
    expect(await screen.findByRole('searchbox', { name: /parameter name/i })).toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: 'Time' }))
    expect(await screen.findByTestId('measured-at')).toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: 'Parameters' }))
    expect(router.currentRoute.value.query).toEqual({})
  })

  it('is open to read-only, operator and administrator accounts, and closed to a metrics account', async () => {
    for (const user of ['viewer', 'ops', 'admin']) {
      const { unmount } = await renderApp('/configuration', { user })
      expect(
        await screen.findByRole('heading', { level: 1, name: 'Configuration' }),
      ).toBeInTheDocument()
      unmount()
    }
    await renderApp('/configuration', { user: 'metrics' })
    expect(
      await screen.findByRole('heading', { level: 1, name: /not allowed/i }),
    ).toBeInTheDocument()
    const nav = within(screen.getByRole('navigation', { name: 'Main' }))
    expect(nav.queryByRole('link', { name: 'Configuration' })).toBeNull()
  })
})

describe('a row', () => {
  it('shows the value on every server, where it comes from, and what waits for a restart', async () => {
    await renderApp('/configuration?q=shared_buffers')
    const r = await row('shared_buffers')
    expect(r.getByText('restart')).toBeInTheDocument()
    expect(r.getAllByText(/256MB/).length).toBeGreaterThanOrEqual(3)
    expect(r.getAllByText('(cluster override)').length).toBe(2)
    expect(r.getByText('Pending restart')).toBeInTheDocument()
    expect(r.getByText(/after a restart/)).toBeInTheDocument()
    // The values differ between servers: said in words with a symbol.
    expect(r.getByTestId('differs')).toHaveTextContent('differs between servers')
    expect(r.getByText('Resource Usage / Memory')).toBeInTheDocument()
    expect(r.getAllByText('128MB').length).toBeGreaterThanOrEqual(2)
  })

  it('shows a server override of one server', async () => {
    await renderApp('/configuration?q=work_mem')
    const r = await row('work_mem')
    expect(r.getByText('32MB')).toBeInTheDocument()
    expect(r.getByText('(server override)')).toBeInTheDocument()
    expect(r.getAllByText('8MB').length).toBeGreaterThanOrEqual(2)
    expect(r.getByText('4MB')).toBeInTheDocument()
    expect(r.getByText(/integer in kB/)).toBeInTheDocument()
    expect(r.getByText('64 to 2147483647')).toBeInTheDocument()
  })

  it('shows enum values in the details of the row', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration?q=wal_compression')
    const r = await row('wal_compression')
    await user.click(r.getByText('5 values'))
    expect(r.getByText('zstd')).toBeInTheDocument()
  })

  it('links to the history of the parameter', async () => {
    const user = userEvent.setup()
    const { router } = await renderApp('/configuration?q=work_mem')
    const r = await row('work_mem')
    await user.click(r.getByRole('link', { name: /history of work_mem/i }))
    expect(router.currentRoute.value.query).toEqual({ tab: 'changes', name: 'work_mem' })
    expect(await screen.findByTestId('change-6')).toBeInTheDocument()
    expect(screen.queryByTestId('change-5')).toBeNull()
  })

  it('shows a managed parameter read-only, with the reason and no Edit button', async () => {
    await renderApp('/configuration?kind=managed&q=port')
    const r = await row('port')
    expect(r.getByText('Managed by KingresDB')).toBeInTheDocument()
    expect(r.getByTestId('reason')).toHaveTextContent(/address and port/)
    expect(r.queryByRole('button', { name: /edit/i })).toBeNull()
    expect(r.getAllByText('5432').length).toBeGreaterThanOrEqual(3)
    expect(r.getAllByText('(managed by KingresDB)').length).toBe(3)
  })

  it('shows a refused parameter read-only, with the reason, and a hidden one without its value', async () => {
    await renderApp('/configuration?kind=refused&q=_command')
    const hidden = await row('archive_command')
    expect(hidden.getByText('Not settable from the console')).toBeInTheDocument()
    expect(hidden.getByTestId('reason')).toHaveTextContent(/runs a command/i)
    expect(hidden.getAllByTestId('value-hidden')).toHaveLength(4)
    expect(hidden.queryByRole('button', { name: /edit/i })).toBeNull()
    // Hidden: the three servers and the default.
    expect(hidden.getAllByText(/hidden/).length).toBeGreaterThanOrEqual(4)
  })

  it('marks a parameter that must be equal on every server', async () => {
    await renderApp('/configuration?q=TimeZone')
    const r = await row('TimeZone')
    expect(r.getByText('equal on every server')).toBeInTheDocument()
    expect(r.getAllByText('Asia/Tehran').length).toBe(3)
    expect(r.getAllByText('(set by initdb)').length).toBe(3)
  })

  it('shows each of the four states of a server in the row', async () => {
    const cfg = getState().config
    cfg.servers['10.0.0.11']!.failed.work_mem = 'invalid value for parameter "work_mem"'
    cfg.servers['10.0.0.13']!.waiting = true
    cfg.servers['10.0.0.13']!.waitDetail = 'The instance is not running.'
    cfg.servers['10.0.0.13']!.run.work_mem = { value: '4MB', source: 'default' }
    await renderApp('/configuration?q=work_mem')
    const r = await row('work_mem')
    expect(r.getByText('Failed')).toBeInTheDocument()
    expect(r.getByText('invalid value for parameter "work_mem"')).toBeInTheDocument()
    expect(r.getByText('Waiting for the server')).toBeInTheDocument()
  })
})

describe('the filters live in the address', () => {
  it('shows only the parameters that wait for a restart', async () => {
    await renderApp('/configuration?pendingRestart=1')
    await row('shared_buffers')
    expect(screen.getAllByTestId(/^param-/)).toHaveLength(1)
    expect(screen.getByLabelText('Pending restart')).toBeChecked()
  })

  it('shows only the parameters that differ between servers, and the changed ones', async () => {
    await renderApp('/configuration?differing=1')
    await row('work_mem')
    const names = screen.getAllByTestId(/^param-/).map((e) => e.getAttribute('data-testid'))
    expect(names).toEqual([
      'param-checkpoint_timeout',
      'param-effective_io_concurrency',
      'param-shared_buffers',
      'param-work_mem',
    ])
  })

  it('shows only the parameters that have an override', async () => {
    await renderApp('/configuration?changed=1')
    await row('TimeZone')
    expect(screen.getAllByTestId(/^param-/)).toHaveLength(4)
  })

  it('shows the parameters that are in a state on at least one server', async () => {
    await renderApp('/configuration?state=pendingRestart')
    await row('shared_buffers')
    expect(screen.getAllByTestId(/^param-/)).toHaveLength(1)
    expect(screen.getByTestId('active-filter')).toHaveTextContent('State: pendingRestart')
  })

  it('lists only what can be set by default, and managed and refused ones with the toggle', async () => {
    const user = userEvent.setup()
    const { router } = await renderApp('/configuration?q=ssl_')
    await screen.findByTestId('param-ssl_prefer_server_ciphers')
    expect(screen.queryByTestId('param-ssl_cert_file')).toBeNull()
    await user.click(screen.getByLabelText('Show managed and refused'))
    expect(router.currentRoute.value.query.all).toBe('1')
    expect(await screen.findByTestId('param-ssl_cert_file')).toBeInTheDocument()
  })

  it('puts a search in the address when the typing pauses, and finds by name or description', async () => {
    const user = userEvent.setup()
    const { router } = await renderApp('/configuration')
    await screen.findByTestId('param-autovacuum')
    await user.type(screen.getByRole('searchbox', { name: /name or description/i }), 'effective_io')
    await waitFor(() => expect(router.currentRoute.value.query.q).toBe('effective_io'))
    await waitFor(() => expect(screen.getAllByTestId(/^param-/)).toHaveLength(2))
    expect(screen.getByTestId('param-effective_io_concurrency')).toBeInTheDocument()
    expect(screen.getByTestId('param-maintenance_io_concurrency')).toBeInTheDocument()
  })

  it('takes the category and the toggles from the form and puts them in the address', async () => {
    const user = userEvent.setup()
    const { router } = await renderApp('/configuration')
    await screen.findByTestId('param-autovacuum')
    await user.selectOptions(screen.getByLabelText('Category'), 'Replication')
    await waitFor(() => expect(router.currentRoute.value.query.category).toBe('Replication'))
    await waitFor(() => expect(screen.queryByTestId('param-autovacuum')).toBeNull())
    expect(await screen.findByTestId('param-wal_keep_size')).toBeInTheDocument()
    await user.click(screen.getByLabelText('Changed (has an override)'))
    expect(router.currentRoute.value.query.changed).toBe('1')
    await user.click(screen.getByLabelText('Differing between servers'))
    expect(router.currentRoute.value.query.differing).toBe('1')
  })

  it('keeps the tab when a filter changes', async () => {
    const { router } = await renderApp('/configuration?tab=time')
    await screen.findByTestId('measured-at')
    expect(router.currentRoute.value.query.tab).toBe('time')
  })

  it('says so when a filter matches nothing, and clears the filters', async () => {
    const user = userEvent.setup()
    const { router } = await renderApp('/configuration?q=zzzzzz&changed=1')
    expect(await screen.findByText('No parameter matches these filters')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Clear filters' }))
    expect(router.currentRoute.value.query).toEqual({})
    expect(await screen.findByTestId('param-autovacuum')).toBeInTheDocument()
  })
})

describe('paging', () => {
  it('loads more by cursor and says how many are shown in a live region', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration')
    await screen.findByTestId('param-autovacuum')
    expect(screen.getByTestId('shown-count')).toHaveTextContent('50 parameters shown')
    await user.click(screen.getByRole('button', { name: 'Load more' }))
    await waitFor(() =>
      expect(screen.getByTestId('shown-count')).toHaveTextContent('100 parameters shown'),
    )
    // The announcement goes to the live region of the shell.
    await waitFor(() =>
      expect(document.body.textContent).toMatch(/100 parameters shown; more can be loaded/),
    )
  })

  it('has no Load more on the last page', async () => {
    await renderApp('/configuration?pendingRestart=1')
    await row('shared_buffers')
    expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull()
    expect(screen.getByTestId('shown-count')).toHaveTextContent('1 parameter shown')
  })
})

describe('states', () => {
  it('shows that it is loading', async () => {
    mockConfig.latencyMs = 150
    await renderApp('/configuration')
    expect(await screen.findAllByText('Loading')).not.toHaveLength(0)
    expect(await screen.findByTestId('param-autovacuum')).toBeInTheDocument()
  })

  it('shows the error of the server with a way to try again', async () => {
    const user = userEvent.setup()
    let fail = true
    mockServer.use(
      http.get('*/api/v1/config/parameters', () =>
        fail
          ? HttpResponse.json(
              { type: 'about:blank', title: 'x', status: 503, code: 'service_unavailable' },
              { status: 503 },
            )
          : undefined,
      ),
    )
    await renderApp('/configuration')
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveAttribute('data-code', 'service_unavailable')
    fail = false
    await user.click(within(alert).getByRole('button', { name: 'Try again' }))
    expect(await screen.findByTestId('param-autovacuum')).toBeInTheDocument()
  })

  it('says calmly that the server does not have this yet', async () => {
    mockConfig.unsupported.add('listConfigParameters')
    mockConfig.unsupported.add('listConfigServers')
    await renderApp('/configuration')
    const notices = await screen.findAllByText('Not available on this server version yet')
    expect(notices.length).toBeGreaterThan(0)
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.queryByTestId('pending-note')).toBeNull()
  })

  it('names the servers that could not be read, and shows their values as not known', async () => {
    mockConfig.partial = [
      { serverId: '10.0.0.12', reason: 'timeout' },
      { serverId: '10.0.0.13', reason: 'unreachable' },
    ]
    await renderApp('/configuration?q=work_mem')
    const notice = await screen.findByTestId('partial-notice')
    expect(notice).toHaveTextContent('This list is incomplete')
    expect(notice).toHaveTextContent('10.0.0.12')
    expect(notice).toHaveTextContent('did not answer in time')
    const r = await row('work_mem')
    expect(r.getAllByText('Not known')).toHaveLength(2)
    expect(screen.queryByText(/^0$/)).toBeNull()
  })

  it('does not call an empty partial list empty', async () => {
    mockConfig.partial = [{ serverId: '10.0.0.12', reason: 'timeout' }]
    await renderApp('/configuration?q=zzzzzz')
    expect(
      await screen.findByText('No parameter from the servers that answered'),
    ).toBeInTheDocument()
  })

  it('starts the list again when the server refuses a cursor, and says so', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration')
    await screen.findByTestId('param-autovacuum')
    mockConfig.rejectCursors = true
    await user.click(screen.getByRole('button', { name: 'Load more' }))
    expect(await screen.findByTestId('cursor-reset-notice')).toBeInTheDocument()
  })
})

describe('roles', () => {
  it('lets an administrator edit', async () => {
    await renderApp('/configuration?q=work_mem', { user: 'admin' })
    const r = await row('work_mem')
    expect(r.getByRole('button', { name: 'Edit work_mem' })).not.toHaveAttribute('aria-disabled')
  })

  for (const user of ['ops', 'viewer']) {
    it(`shows ${user} the values and the Edit button disabled with the reason`, async () => {
      await renderApp('/configuration?q=work_mem', { user })
      const r = await row('work_mem')
      const edit = r.getByRole('button', { name: /Edit work_mem/ })
      expect(edit).toHaveAttribute('aria-disabled', 'true')
      expect(edit).toHaveAccessibleDescription(/requires the kingres_admin cluster role/i)
      expect(r.getByText('32MB')).toBeInTheDocument()
      // A disabled button does nothing.
      await userEvent.setup().click(edit)
      expect(screen.queryByRole('dialog')).toBeNull()
      // The restart is the administrator's decision too.
      expect(screen.getByRole('button', { name: /restart pending instances/i })).toHaveAttribute(
        'aria-disabled',
        'true',
      )
    })
  }
})

describe('what waits and what failed, above the table', () => {
  it('says which servers wait for a restart and that nothing is restarted by a change', async () => {
    await renderApp('/configuration')
    const note = await screen.findByTestId('pending-note')
    expect(note).toHaveTextContent('1 server waits for a restart: 10.0.0.12')
    expect(note).toHaveTextContent('Nothing is restarted by a change')
  })

  it('says which servers failed and which wait for the server', async () => {
    getState().config.servers['10.0.0.12']!.failed.work_mem = 'x'
    getState().config.servers['10.0.0.13']!.waiting = true
    getState().config.servers['10.0.0.13']!.waitDetail = 'The instance is not running.'
    await renderApp('/configuration')
    expect(await screen.findByTestId('failed-note')).toHaveTextContent(
      'A change failed on 10.0.0.12',
    )
    expect(screen.getByTestId('waiting-note')).toHaveTextContent('Waiting for 10.0.0.13')
  })
})
