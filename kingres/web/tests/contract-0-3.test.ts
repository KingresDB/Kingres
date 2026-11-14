// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Behaviour added by revision 0.3.0 of the contract, and the panel's answers to what the real
// server does (found by the end-to-end run in scripts/e2e).
import { describe, expect, it } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/vue'
import BucketGrid from '@/components/BucketGrid.vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState, resetMockState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { every } from '@/api/queries'
import { ApiError } from '@/api/problem'
import { renderApp } from './helpers'

const problem = (status: number, code: string) =>
  HttpResponse.json({ type: 'about:blank', title: code, status, code }, { status })

describe('stats-only dashboard uses getClusterSummary', () => {
  it('shows the health, server, database and alert counts it may read', async () => {
    await renderApp('/dashboard', { user: 'metrics' })
    const summary = await screen.findByTestId('cluster-summary')
    expect(within(summary).getByText('Healthy')).toBeInTheDocument()
    expect(within(summary).getByText('prod-eu')).toBeInTheDocument()
    expect(within(summary).getByText('Servers').nextElementSibling).toHaveTextContent('3')
    expect(within(summary).getByText('Databases').nextElementSibling).toHaveTextContent('2')
    expect(within(summary).getByText('Open alerts').nextElementSibling).toHaveTextContent('1')
  })

  it('is not used for accounts that can read the full cluster view', async () => {
    await renderApp('/dashboard', { user: 'viewer' })
    await screen.findByText(/under-replicated buckets/i)
    expect(screen.queryByTestId('cluster-summary')).not.toBeInTheDocument()
  })
})

describe('setup progress from setupTask', () => {
  it('shows the step names of the setup task', async () => {
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await user.type(await screen.findByLabelText(/setup token/i), 'demo-setup-token')
    await user.click(screen.getByRole('button', { name: /continue/i }))
    await user.click(await screen.findByRole('button', { name: /^create new cluster$/i }))
    await user.type(screen.getByLabelText(/cluster name/i), 'prod-eu')
    await user.type(screen.getByLabelText(/^password/i), 'a-very-long-password')
    await user.type(screen.getByLabelText(/repeat password/i), 'a-very-long-password')
    await user.click(screen.getByRole('button', { name: /^create cluster$/i }))
    expect(await screen.findByText(/initialize map group/i)).toBeInTheDocument()
    expect(screen.getByText(/create administrator/i)).toBeInTheDocument()
    await screen.findByRole('button', { name: /continue to sign in/i })
  })
})

describe('distribution columns name the keys that block them', () => {
  it('lists the unique keys that lack the column', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const row = (await screen.findByRole('rowheader', { name: 'public.notes' })).closest(
      'tr',
    ) as HTMLElement
    await user.click(within(row).getByRole('button', { name: /distribute table/i }))
    const dialog = await screen.findByRole('dialog', { name: /distribute public\.notes/i })
    const blocked = await within(dialog).findByTestId('blocked-columns')
    expect(within(blocked).getAllByText('notes_pkey')).toHaveLength(2)
    expect(within(blocked).getAllByText(/keys without it/i)).toHaveLength(2)
  })
})

describe('enable the ledger of a table', () => {
  it('states that it is permanent, needs the table name, and turns the table into a ledger table', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const row = (await screen.findByRole('rowheader', { name: 'public.notes' })).closest(
      'tr',
    ) as HTMLElement
    await user.click(within(row).getByRole('button', { name: /enable ledger/i }))
    const dialog = await screen.findByRole('dialog', {
      name: /enable the ledger for public\.notes/i,
    })
    expect(within(dialog).getByRole('alert')).toHaveTextContent(/permanent/i)
    expect(within(dialog).getByRole('alert')).toHaveTextContent(/never be turned off/i)
    const confirm = within(dialog).getByRole('button', { name: /enable ledger permanently/i })
    expect(confirm).toBeDisabled()
    await user.click(within(dialog).getByRole('radio', { name: /updatable/i }))
    await user.type(within(dialog).getByLabelText(/type notes to confirm/i), 'notes')
    expect(confirm).toBeEnabled()
    await user.click(confirm)

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    const t = getState().databases[0]!.tables.find((x) => x.name === 'notes')
    expect(t?.ledger).toEqual({ mode: 'updatable', historyTable: 'notes_history' })
    const updated = (await screen.findByRole('rowheader', { name: 'public.notes' })).closest(
      'tr',
    ) as HTMLElement
    await waitFor(() => expect(within(updated).getByText('Ledger')).toBeInTheDocument())
    // A table that already has a ledger no longer offers the action.
    expect(
      within(updated).queryByRole('button', { name: /enable ledger/i }),
    ).not.toBeInTheDocument()
  })

  it('is refused for a viewer with the reason, not hidden', async () => {
    await renderApp('/databases/shop', { user: 'viewer' })
    const row = (await screen.findByRole('rowheader', { name: 'public.notes' })).closest(
      'tr',
    ) as HTMLElement
    expect(within(row).getByRole('button', { name: /enable ledger/i })).toHaveAttribute(
      'aria-disabled',
      'true',
    )
  })
})

describe('inclusion proofs have an entry type', () => {
  it('asks for a delete entry and shows which entry the proof is for', async () => {
    const user = userEvent.setup()
    await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    await user.selectOptions(screen.getByLabelText('Entry'), 'D')
    await user.click(screen.getByRole('button', { name: /look up proof/i }))
    const proof = await screen.findByTestId('proof')
    expect(within(proof).getByText('Delete')).toBeInTheDocument()
  })
})

describe('values that became nullable in 0.3.0', () => {
  it('shows n/a for the version of a server that has not reported', async () => {
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.13')
    expect(within(row).getAllByText('Not available').length).toBeGreaterThanOrEqual(4)
    expect(within(row).queryByText('0.1.0')).not.toBeInTheDocument()
    expect(
      within(screen.getByTestId('server-row-10.0.0.11')).getByText('0.1.0'),
    ).toBeInTheDocument()
  })

  it('says so when a database has no creation record', async () => {
    await renderApp('/databases/reports')
    expect(await screen.findByText(/creation record not available/i)).toBeInTheDocument()
  })

  it('shows n/a when the ledger status has no mode or digest interval', async () => {
    mockServer.use(
      http.get('*/api/v1/ledger/status', () =>
        HttpResponse.json({
          items: [
            {
              databaseName: 'shop',
              mode: null,
              digestIntervalSeconds: null,
              verificationState: null,
              lastVerifiedAt: null,
              ledgerTableCount: null,
              chains: [],
            },
          ],
        }),
      ),
    )
    await renderApp('/ledger')
    const status = (await screen.findByText(/status of shop/i)).closest('section') as HTMLElement
    expect(within(status).getAllByText('Not available').length).toBeGreaterThanOrEqual(3)
  })
})

describe('settings send only what changed', () => {
  it('sends nothing when nothing changed, and only the changed value otherwise (no empty groups)', async () => {
    const user = userEvent.setup()
    const bodies: unknown[] = []
    mockServer.events.on('request:start', async ({ request }) => {
      if (request.method === 'PATCH' && request.url.endsWith('/settings/cluster')) {
        bodies.push(await request.clone().json())
      }
    })
    await renderApp('/settings')
    await screen.findByLabelText(/default replica count/i)
    await user.click(screen.getByRole('button', { name: /save settings/i }))
    expect(await screen.findByText(/nothing was changed/i)).toBeInTheDocument()
    expect(bodies).toEqual([])

    const moves = screen.getByLabelText(/concurrent moves/i)
    await user.clear(moves)
    await user.type(moves, '8')
    await user.click(screen.getByRole('button', { name: /save settings/i }))
    expect(await screen.findByText(/settings saved/i)).toBeInTheDocument()
    expect(bodies).toEqual([{ rebalance: { maxConcurrentMoves: 8 } }])
    mockServer.events.removeAllListeners()
  })

  it('the mock refuses an empty group like the contract does', async () => {
    await renderApp('/dashboard')
    const { endpoints } = await import('@/api/endpoints')
    await expect(endpoints.updateClusterSettings({ features: {} })).rejects.toMatchObject({
      code: 'validation_failed',
    })
    await expect(endpoints.updateClusterSettings({})).rejects.toMatchObject({
      code: 'validation_failed',
    })
  })
})

describe('answers of the real server that the panel has to handle', () => {
  it('shows a missing manager leader as "not reported", not as a red failure', async () => {
    // The real server answers managerLeader: null (there is no election yet).
    const { overview } = await import('@/mocks/state')
    const original = overview(getState())
    expect(original.managerLeader).not.toBeNull()
    mockServer.use(
      http.get('*/api/v1/cluster', () => HttpResponse.json({ ...original, managerLeader: null })),
    )
    await renderApp('/dashboard')
    const card = (await screen.findByText('Manager leader')).closest('.card') as HTMLElement
    expect(within(card).getByText('Not reported')).toBeInTheDocument()
    expect(card.querySelector('.tone-danger')).toBeNull()
  })

  it('says once that there are no metrics', async () => {
    mockConfig.unsupported.add('queryMetrics')
    await renderApp('/dashboard')
    const heading = await screen.findByRole('heading', { name: 'Last hour' })
    const section = heading.closest('section') as HTMLElement
    await waitFor(() =>
      expect(
        within(section).getAllByText(/not available on this server version yet/i),
      ).toHaveLength(1),
    )
  })

  it('stops polling a query once the server has said "not supported yet"', () => {
    const unsupported = new ApiError({
      type: 'about:blank',
      title: 'x',
      status: 422,
      code: 'unsupported_operation',
    })
    const failed = new ApiError({
      type: 'about:blank',
      title: 'x',
      status: 503,
      code: 'service_unavailable',
    })
    expect(every(10_000)({ state: { error: unsupported } })).toBe(false)
    expect(every(10_000)({ state: { error: failed } })).toBe(10_000)
    expect(every(10_000)({ state: { error: null } })).toBe(10_000)
  })

  it('still lists the ledger when GET /ledger/status answers 404', async () => {
    mockServer.use(http.get('*/api/v1/ledger/status', () => problem(404, 'not_found')))
    await renderApp('/ledger')
    expect(await screen.findByText(/status of shop/i)).toBeInTheDocument()
    expect(screen.queryByText(/not found/i)).not.toBeInTheDocument()
  })

  it('explains it when the browser does not keep the session cookie', async () => {
    const user = userEvent.setup()
    await renderApp('/login', { user: null })
    // The server accepts the sign-in but the next request is not signed in (cookie dropped).
    mockServer.use(http.get('*/api/v1/auth/session', () => problem(401, 'unauthenticated')))
    await user.type(await screen.findByLabelText('User name'), 'admin')
    await user.type(screen.getByLabelText('Password'), 'demo-password')
    await user.click(screen.getByRole('button', { name: /^sign in$/i }))
    expect(await screen.findByText(/did not keep the session/i)).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: /sign in to kingresdb/i })).toBeInTheDocument()
  })

  it('treats a proxy error page (502 without a problem body) as "service unavailable"', async () => {
    mockServer.use(
      http.get(
        '*/api/v1/servers',
        () =>
          new HttpResponse('<html>Bad Gateway</html>', {
            status: 502,
            headers: { 'Content-Type': 'text/html' },
          }),
      ),
    )
    await renderApp('/servers')
    expect(await screen.findByText('Service unavailable')).toBeInTheDocument()
    expect(screen.queryByText(/unexpected server error/i)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument()
  })

  it('shows n/a, not a dash or 0, for a ledger chain with no blocks yet', async () => {
    mockServer.use(
      http.get('*/api/v1/ledger/status', () =>
        HttpResponse.json({
          items: [
            {
              databaseName: 'shop',
              mode: 'optIn',
              digestIntervalSeconds: 3600,
              verificationState: null,
              lastVerifiedAt: null,
              ledgerTableCount: 1,
              chains: [
                {
                  shardGroupId: 0,
                  latestBlockHeight: null,
                  latestBlockHash: null,
                  latestBlockAt: null,
                  lastVerifiedBlockHeight: null,
                  verificationState: null,
                },
              ],
            },
          ],
        }),
      ),
    )
    await renderApp('/ledger')
    const row = (await screen.findByRole('rowheader', { name: 'Group 0' })).closest(
      'tr',
    ) as HTMLElement
    expect(within(row).getAllByTestId('na')).toHaveLength(5)
    expect(row).not.toHaveTextContent(/^Group 0s*0/)
  })

  it('labels group 0 as the map group on the bucket map', () => {
    render(BucketGrid, {
      props: {
        groupIds: [0],
        buckets: [
          {
            bucket: 0,
            shardGroupId: 0,
            state: 'active',
            primaryServerId: 'a',
            replicaServerIds: [],
            sizeBytes: null,
            movingToShardGroupId: null,
          },
        ],
      },
    })
    expect(screen.getAllByText('Map group (0)').length).toBeGreaterThanOrEqual(2)
    expect(screen.queryByText(/shard group 0/i)).not.toBeInTheDocument()
  })
})
