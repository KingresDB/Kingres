// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// One smoke and accessibility check per page: the page renders its heading with data from the mock
// API, logs no Vue warnings or errors, and has no axe violations. (Colour contrast cannot be
// computed in jsdom; it is covered by tokens-contrast.test.ts.)
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { configureAxe } from 'vitest-axe'
import { resetMockState } from '@/mocks/http'
import { renderApp, settled } from './helpers'

const axe = configureAxe({ rules: { 'color-contrast': { enabled: false } } })

interface PageCase {
  path: string
  heading: string
  /** Text that proves data was loaded and rendered. */
  data: RegExp
  user?: string
}

const PAGES: PageCase[] = [
  { path: '/dashboard', heading: 'Dashboard', data: /prod-eu|Cluster health/ },
  { path: '/servers', heading: 'Servers', data: /10\.0\.0\.12:8091/ },
  { path: '/servers/10.0.0.11', heading: 'Server 10.0.0.11:8091', data: /Buckets by database/ },
  { path: '/databases', heading: 'Databases', data: /reports/ },
  { path: '/databases/shop', heading: 'Database shop', data: /public\.orders/ },
  { path: '/shards', heading: 'Shards', data: /Bucket map/ },
  { path: '/replication', heading: 'Replication', data: /Shard group 3/ },
  { path: '/rebalance', heading: 'Rebalance', data: /No rebalance is needed/ },
  { path: '/security', heading: 'Security', data: /analyst/ },
  { path: '/ledger', heading: 'Ledger', data: /Status of shop/ },
  { path: '/workbench', heading: 'Query workbench', data: /Run/ },
  { path: '/tasks', heading: 'Tasks', data: /No tasks|database\./ },
  { path: '/alerts', heading: 'Alerts', data: /Replication lag is high/ },
  { path: '/logs', heading: 'Logs', data: /heartbeat from server 2 received/ },
  { path: '/settings', heading: 'Settings', data: /Default replica count/ },
  { path: '/this-server', heading: 'This server', data: /Local settings/ },
  { path: '/this-server/logs', heading: 'Local logs', data: /heartbeat|checkpoint|leader lease/ },
]

describe('pages', () => {
  let errors: ReturnType<typeof vi.spyOn>
  let warnings: ReturnType<typeof vi.spyOn>

  beforeEach(() => {
    errors = vi.spyOn(console, 'error').mockImplementation(() => {})
    warnings = vi.spyOn(console, 'warn').mockImplementation(() => {})
  })
  afterEach(() => {
    errors.mockRestore()
    warnings.mockRestore()
  })

  for (const page of PAGES) {
    it(`${page.path} renders data, logs nothing and passes axe`, async () => {
      const { container } = await renderApp(page.path, { user: page.user })
      await screen.findByRole('heading', { level: 1, name: page.heading })
      await waitFor(() => expect(screen.getAllByText(page.data).length).toBeGreaterThan(0), {
        timeout: 4000,
      })
      await settled()
      expect(await axe(container)).toHaveNoViolations()
      expect(errors).not.toHaveBeenCalled()
      expect(warnings).not.toHaveBeenCalled()
    })
  }

  it('the create-database dialog passes axe', async () => {
    const user = userEvent.setup()
    const { container } = await renderApp('/databases')
    await user.click(await screen.findByRole('button', { name: /create database/i }))
    const dialog = await screen.findByRole('dialog', { name: /create database/i })
    await within(dialog).findByLabelText(/bucket count/i)
    expect(await axe(dialog)).toHaveNoViolations()
    expect(container).toBeTruthy()
    expect(errors).not.toHaveBeenCalled()
  })

  it('the distribute-table dialog passes axe', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const row = (await screen.findByRole('rowheader', { name: 'public.notes' })).closest('tr')
    await user.click(within(row as HTMLElement).getByRole('button', { name: /distribute table/i }))
    const dialog = await screen.findByRole('dialog', { name: /distribute/i })
    await within(dialog).findByLabelText(/distribution column/i)
    expect(await axe(dialog)).toHaveNoViolations()
  })

  it('/login passes axe', async () => {
    const { container } = await renderApp('/login', { user: null })
    await screen.findByRole('heading', { name: /sign in to kingresdb/i })
    expect(await axe(container)).toHaveNoViolations()
    expect(errors).not.toHaveBeenCalled()
  })

  it('/setup passes axe in every step', async () => {
    resetMockState({ uninitialized: true })
    const { container } = await renderApp('/setup', { user: null })
    await screen.findByLabelText(/setup token/i)
    expect(await axe(container)).toHaveNoViolations()
    expect(errors).not.toHaveBeenCalled()
  })
})
