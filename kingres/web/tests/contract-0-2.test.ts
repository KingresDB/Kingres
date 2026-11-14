// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Behaviour added or changed by revision 0.2.0 of the contract.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { endpoints } from '@/api/endpoints'
import { renderApp, settled } from './helpers'

describe('kingres_stats account', () => {
  it('lands on a dashboard with the charts it may see, not on "not allowed"', async () => {
    const { router } = await renderApp('/', { user: 'metrics' })
    await waitFor(() => expect(router.currentRoute.value.name).toBe('dashboard'))
    expect(await screen.findByRole('heading', { level: 1, name: 'Dashboard' })).toBeInTheDocument()
    // The shell takes the cluster name from the session, which every role can read.
    expect(await screen.findByText('prod-eu')).toBeInTheDocument()
    expect(await screen.findByRole('img', { name: /cpu usage/i })).toBeInTheDocument()
    // Cards and lists that need kingres_readonly are left out instead of failing.
    expect(screen.queryByText(/under-replicated buckets/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/recent tasks/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/your account cannot open this page/i)).not.toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    const nav = screen.getByRole('navigation', { name: 'Main' })
    expect(within(nav).getAllByRole('link')).toHaveLength(1)
  })
})

describe('null means "not available"', () => {
  it('shows n/a with an accessible label for unknown server values, never 0', async () => {
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.13')
    const na = within(row).getAllByTestId('na')
    expect(na.length).toBeGreaterThanOrEqual(3) // CPU, used memory and disk, uptime
    expect(within(row).getAllByText('Not available').length).toBe(na.length)
    expect(row).not.toHaveTextContent(/CPU 0/)
    // A server that reports everything has no n/a in the same columns.
    expect(within(screen.getByTestId('server-row-10.0.0.11')).queryAllByTestId('na')).toHaveLength(
      0,
    )
  })

  it('shows n/a for database statistics the server cannot provide', async () => {
    await renderApp('/databases')
    const row = (await screen.findByRole('link', { name: 'reports' })).closest('tr') as HTMLElement
    expect(within(row).getAllByTestId('na').length).toBeGreaterThanOrEqual(1)
    expect(within(row).getByText('Not available')).toBeInTheDocument()
  })

  it('shows n/a for a bucket without a size and a table without a row estimate', async () => {
    await renderApp('/databases/reports')
    const exportsRow = (await screen.findByRole('rowheader', { name: 'public.exports' })).closest(
      'tr',
    ) as HTMLElement
    expect(within(exportsRow).getAllByTestId('na')).toHaveLength(1)
  })

  it('shows n/a for a setting and a status the server reports as null', async () => {
    const reports = getState().databases[1]!.info
    reports.ledger.mode = null
    reports.replication.durability = null
    await renderApp('/databases')
    const row = (await screen.findByRole('link', { name: 'reports' })).closest('tr') as HTMLElement
    // durability, ledger mode, transactions per second
    expect(within(row).getAllByTestId('na').length).toBeGreaterThanOrEqual(3)
    expect(row).not.toHaveTextContent('Asynchronous')
  })
})

describe('create-database options from the server', () => {
  it('uses the server defaults and ranges, and filters collations by encoding', async () => {
    const user = userEvent.setup()
    mockServer.use(
      http.get('*/api/v1/database-create-options', () =>
        HttpResponse.json({
          encodings: [
            { name: 'UTF8', isDefault: true },
            { name: 'LATIN1', isDefault: false },
          ],
          collations: [
            { name: 'en_US.UTF-8', encodings: ['UTF8'], isDefault: true },
            { name: 'de_DE.ISO-8859-1', encodings: ['LATIN1'], isDefault: true },
            { name: 'C', encodings: null, isDefault: false },
          ],
          bucketCount: { default: 256, minimum: 8, maximum: 1024 },
          replicaCount: 2,
          defaultDurability: 'syncApply',
          ledgerDigestIntervalSeconds: { default: 600, minimum: 120, maximum: 7200 },
        }),
      ),
    )
    await renderApp('/databases')
    await settled()
    await user.click(await screen.findByRole('button', { name: /create database/i }))
    const dialog = await screen.findByRole('dialog', { name: /create database/i })
    const buckets = await within(dialog).findByLabelText(/bucket count/i)
    expect(buckets).toHaveValue(256)
    expect(buckets).toHaveAttribute('min', '8')
    expect(buckets).toHaveAttribute('max', '1024')
    expect(within(dialog).getByTestId('replica-count')).toHaveTextContent('2')
    expect(within(dialog).getByLabelText(/durability/i)).toHaveValue('syncApply')
    expect(within(dialog).getByLabelText(/digest interval/i)).toHaveValue(600)

    const collation = within(dialog).getByLabelText(/collation/i)
    // The default collation of the default encoding is preselected (not an empty choice).
    expect(collation).toHaveValue('en_US.UTF-8')
    expect(
      within(collation)
        .getAllByRole('option')
        .map((o) => o.textContent?.trim()),
    ).toEqual(['en_US.UTF-8 (default)', 'C'])
    await user.selectOptions(within(dialog).getByLabelText(/encoding/i), 'LATIN1')
    expect(collation).toHaveValue('de_DE.ISO-8859-1')
    expect(
      within(collation)
        .getAllByRole('option')
        .map((o) => o.textContent?.trim()),
    ).toEqual(['de_DE.ISO-8859-1 (default)', 'C'])

    // The range from the server is the one that is validated.
    await user.type(within(dialog).getByLabelText(/^name/i), 'wide')
    await user.click(within(dialog).getByRole('checkbox', { name: /spread this database/i }))
    await user.clear(buckets)
    await user.type(buckets, '4')
    await user.click(within(dialog).getByRole('button', { name: /^create database$/i }))
    expect(
      await within(dialog).findByText(/bucket count must be a whole number between 8 and 1024/i),
    ).toBeInTheDocument()
  })
})

describe('distribute table with columns from the contract', () => {
  it('offers only columns that can be distribution columns and explains the others', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const notes = (await screen.findByRole('rowheader', { name: 'public.notes' })).closest(
      'tr',
    ) as HTMLElement
    await user.click(within(notes).getByRole('button', { name: /distribute table/i }))
    const dialog = await screen.findByRole('dialog', { name: /distribute public\.notes/i })

    const select = await within(dialog).findByLabelText(/distribution column/i)
    expect(
      within(select)
        .getAllByRole('option')
        .map((o) => o.textContent?.trim()),
    ).toEqual(['note_id (bigint)'])
    const blocked = within(dialog).getByTestId('blocked-columns')
    expect(within(blocked).getAllByText(/part of every unique key/i)).toHaveLength(2)
    expect(within(blocked).getByText(/cannot be hashed into buckets/i)).toBeInTheDocument()
    expect(within(blocked).getByText('tags')).toBeInTheDocument()

    await user.click(within(dialog).getByRole('button', { name: /^distribute table$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    const t = getState().databases[0]!.tables.find((x) => x.name === 'notes')
    expect(t?.distribution?.column).toBe('note_id')
  })
})

describe('server groups', () => {
  it('creates a group, assigns a server to it and refuses to delete a group that has servers', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    await user.click(await screen.findByRole('button', { name: /create server group/i }))
    const dialog = await screen.findByRole('dialog', { name: /create server group/i })
    await user.type(within(dialog).getByLabelText(/^name/i), 'bad name!')
    await user.click(within(dialog).getByRole('button', { name: /^create server group$/i }))
    expect(await within(dialog).findByText(/use letters, digits/i)).toBeInTheDocument()
    await user.clear(within(dialog).getByLabelText(/^name/i))
    await user.type(within(dialog).getByLabelText(/^name/i), 'rack-c')
    await user.click(within(dialog).getByRole('button', { name: /^create server group$/i }))
    expect(await screen.findByRole('rowheader', { name: 'rack-c' })).toBeInTheDocument()

    // Assign server 10.0.0.12 on its detail page.
    await user.click(await screen.findByRole('link', { name: '10.0.0.12:8091' }))
    await user.selectOptions(await screen.findByLabelText('Group'), 'rack-c')
    await user.click(screen.getByRole('button', { name: /save group/i }))
    expect(await screen.findByText(/server group saved/i)).toBeInTheDocument()
    expect(getState().servers.find((s) => s.id === '10.0.0.12')?.serverGroup).toBe('rack-c')

    await user.click(screen.getByRole('link', { name: /all servers/i }))
    const row = (await screen.findByRole('rowheader', { name: 'rack-c' })).closest(
      'tr',
    ) as HTMLElement
    expect(within(row).getByRole('link', { name: '10.0.0.12' })).toBeInTheDocument()
    await user.click(within(row).getByRole('button', { name: /delete/i }))
    const confirm = await screen.findByRole('dialog', { name: /delete server group rack-c/i })
    await user.click(within(confirm).getByRole('button', { name: /delete group/i }))
    expect(await within(confirm).findByRole('alert')).toHaveTextContent(/still has servers/i)
    await user.click(within(confirm).getByRole('button', { name: /cancel/i }))

    // An empty group can be deleted.
    await endpoints.updateServer('10.0.0.12', { serverGroup: 'rack-a' })
    await endpoints.deleteServerGroup('rack-c')
    expect((await endpoints.listServerGroups()).items.some((g) => g.name === 'rack-c')).toBe(false)
  })
})

describe('rebalance report detail', () => {
  it('shows the last report and the outcome of every move', async () => {
    const user = userEvent.setup()
    await renderApp('/rebalance')
    const last = await screen.findByTestId('last-report')
    expect(last).toHaveTextContent(/last run: completed/i)
    await user.click(within(last).getByRole('button', { name: /show report #2/i }))
    const dialog = await screen.findByRole('dialog', { name: /rebalance report #2/i })
    const moves = await within(dialog).findByTestId('report-moves')
    expect(within(moves).getAllByRole('row')).toHaveLength(6) // header + 5 moves
    expect(within(moves).getByText('Failed')).toBeInTheDocument()
    expect(within(moves).getByText(/replication slot lagged/i)).toBeInTheDocument()
    expect(within(moves).getByText('Skipped')).toBeInTheDocument()
    // One move has no size: n/a, not 0.
    expect(within(moves).getAllByTestId('na').length).toBeGreaterThanOrEqual(1)
  })
})

describe('audit actions', () => {
  it('filters by the closed list of actions of the contract', async () => {
    const user = userEvent.setup()
    await renderApp('/security')
    await user.click(await screen.findByRole('tab', { name: 'Audit log' }))
    const action = await screen.findByLabelText('Action')
    const names = within(action)
      .getAllByRole('option')
      .map((o) => o.textContent?.trim())
    expect(names).toContain('createDatabase')
    expect(names).toContain('loginFailed')
    expect(names).not.toContain('database.create')
    await user.selectOptions(action, 'createRole')
    await waitFor(() => {
      const cells = screen.getAllByRole('cell', { name: 'createRole' })
      expect(cells.length).toBeGreaterThan(0)
    })
    expect(screen.queryByRole('cell', { name: 'failoverServer' })).not.toBeInTheDocument()
  })
})
