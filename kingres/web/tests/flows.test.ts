// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// End-to-end flows through the real app against the mock API.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { mockServer } from '@/mocks/server'
import { http, HttpResponse } from 'msw'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { renderApp } from './helpers'

describe('session', () => {
  it('signs in with the form and signs out again', async () => {
    const user = userEvent.setup()
    const { router } = await renderApp('/dashboard', { user: null })
    await user.type(await screen.findByLabelText('User name'), 'viewer')
    await user.type(screen.getByLabelText('Password'), 'wrong')
    await user.click(screen.getByRole('button', { name: /^sign in$/i }))
    expect(await screen.findByText(/user name or password is not correct/i)).toBeInTheDocument()

    await user.type(screen.getByLabelText('Password'), 'demo-password')
    await user.click(screen.getByRole('button', { name: /^sign in$/i }))
    await waitFor(() => expect(router.currentRoute.value.name).toBe('dashboard'))
    // The top bar shows the user and the highest cluster role.
    expect(await screen.findByText('kingres_readonly')).toBeInTheDocument()
    expect(screen.getByText('viewer')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /sign out/i }))
    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'))
    expect(getState().session).toBeNull()
  })

  it('sends the CSRF token on changes and the server rejects a missing one', async () => {
    await renderApp('/dashboard')
    const seen: (string | null)[] = []
    mockServer.use(
      http.post('*/api/v1/rebalance/plan', ({ request }) => {
        seen.push(request.headers.get('X-CSRF-Token'))
        return HttpResponse.json({
          needed: false,
          totals: { moveCount: 0, totalBytes: 0, estimatedDurationSeconds: 0 },
          truncated: false,
          moves: [],
          serverImpacts: [],
          warnings: [],
        })
      }),
    )
    await endpoints.previewRebalancePlan({})
    expect(seen[0]).toBe(getState().session?.csrfToken)

    // Without the header the mock (like the real server) answers csrf_token_invalid.
    mockServer.resetHandlers()
    const response = await fetch(`${location.origin}/api/v1/rebalance/plan`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    })
    expect(response.status).toBe(403)
    expect(((await response.json()) as { code: string }).code).toBe('csrf_token_invalid')
  })

  it('returns to the sign-in page when the session has expired', async () => {
    const { router } = await renderApp('/servers')
    await screen.findByTestId('server-row-10.0.0.11')
    mockServer.use(
      http.get('*/api/v1/servers', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 401, code: 'session_expired' },
          { status: 401 },
        ),
      ),
    )
    await router.push('/dashboard')
    await router.push('/servers')
    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'), { timeout: 3000 })
  })
})

describe('add server, then rebalance', () => {
  it('moves buckets to the new server, with progress and a report', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    // Add a server through the API, wait for the task, then use the panel for the rebalance.
    const task = await endpoints.addServer({
      address: '10.0.0.14:8091',
      setupToken: 'demo-setup-token',
      allowUnpinned: true,
    })
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('done'))

    const { router } = await renderApp('/rebalance')
    expect(
      await screen.findByText(/a rebalance is needed: a server was added/i),
    ).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /preview plan/i }))
    const plan = await screen.findByTestId('plan')
    expect(within(plan).getByText(/moves, /)).toBeInTheDocument()
    expect(within(plan).getByRole('row', { name: /server 10[.]0[.]0[.]14/i })).toBeInTheDocument()

    mockConfig.taskDurationMs = 400
    await user.click(screen.getByRole('button', { name: /start rebalance/i }))
    expect(
      await screen.findByRole('progressbar', { name: /rebalance progress/i }),
    ).toBeInTheDocument()
    await waitFor(
      () => expect(screen.getByRole('button', { name: /start rebalance/i })).toBeInTheDocument(),
      { timeout: 5000 },
    )
    await waitFor(() => expect(getState().rebalance.needed).toBe(false), { timeout: 5000 })

    const s = getState()
    const group4 = s.groups.find((g) => g.preferredPrimaryServerId === '10.0.0.14')
    const counts = s.databases[0]!.buckets.get(1)!.filter((b) => b.group === group4?.id).length
    expect(counts).toBe(32)
    expect(s.servers.find((x) => x.id === '10.0.0.14')?.status).toBe('live')
    expect(s.rebalance.reports[0]?.outcome).toBe('completed')
    await router.push('/rebalance')
    expect(
      await screen.findByRole('button', { name: `#${s.rebalance.reports[0]!.id}` }),
    ).toBeInTheDocument()
  })

  it('stops a running rebalance after confirmation', async () => {
    const user = userEvent.setup()
    const { router } = await renderApp('/dashboard')
    const added = await endpoints.addServer({
      address: '10.0.0.14:8091',
      setupToken: 'demo-setup-token',
      allowUnpinned: true,
    })
    await waitFor(async () => expect((await endpoints.getTask(added.id)).state).toBe('done'))
    mockConfig.taskDurationMs = 20000
    await endpoints.startRebalance({})
    await router.push('/rebalance')
    await user.click(await screen.findByRole('button', { name: /stop rebalance/i }))
    const dialog = await screen.findByRole('dialog', { name: /stop the rebalance/i })
    await user.click(within(dialog).getByRole('button', { name: /^stop rebalance$/i }))
    await waitFor(() => expect(getState().rebalance.state).toBe('stopped'), { timeout: 5000 })
    expect(await screen.findByText(/stopped [(]start again to go on[)]/i)).toBeInTheDocument()
    expect(await screen.findByRole('button', { name: /start again/i })).toBeInTheDocument()
  })
})

describe('query workbench', () => {
  it('runs a statement and shows rows, types and the command tag', async () => {
    const user = userEvent.setup()
    await renderApp('/workbench')
    await screen.findByRole('option', { name: 'shop' })
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    const result = await screen.findByTestId('result')
    expect(within(result).getByText(/SELECT 40/)).toBeInTheDocument()
    expect(within(result).getByRole('columnheader', { name: /customer_id/ })).toBeInTheDocument()
  })

  it('shows SQL errors with their SQLSTATE and truncates at the row limit', async () => {
    const user = userEvent.setup()
    await renderApp('/workbench')
    await screen.findByRole('option', { name: 'shop' })
    const sql = screen.getByLabelText(/^sql/i)
    await user.clear(sql)
    await user.type(sql, 'select * from missing_table')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    expect(await screen.findByText(/relation "missing_table" does not exist/i)).toBeInTheDocument()
    expect(screen.getByText(/SQLSTATE 42P01/)).toBeInTheDocument()

    await user.clear(sql)
    await user.type(sql, 'select * from orders')
    const limit = screen.getByLabelText(/row limit/i)
    await user.clear(limit)
    await user.type(limit, '5')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    expect(
      await screen.findByText(/cut at the row limit or at the 16 MiB size limit/i),
    ).toBeInTheDocument()
  })
})

describe('security', () => {
  it('creates a role with a cluster role and refuses to remove the last administrator', async () => {
    const user = userEvent.setup()
    await renderApp('/security')
    await user.click(await screen.findByRole('button', { name: /create role/i }))
    const dialog = await screen.findByRole('dialog', { name: /create role/i })
    await user.type(within(dialog).getByLabelText(/^name/i), 'support')
    await user.type(within(dialog).getByLabelText('Password'), 'longenough')
    await user.click(await within(dialog).findByRole('checkbox', { name: /kingres_readonly/ }))
    await user.click(within(dialog).getByRole('button', { name: /^create role$/i }))
    expect(await screen.findByRole('rowheader', { name: 'support' })).toBeInTheDocument()
    expect(getState().roles.find((r) => r.name === 'support')?.clusterRoles).toEqual([
      'kingres_readonly',
    ])

    await expect(endpoints.setRoleClusterRoles('admin', ['kingres_ops'])).rejects.toMatchObject({
      code: 'last_admin',
    })
    await expect(endpoints.deleteRole('admin')).rejects.toBeInstanceOf(ApiError)
  })

  it('toggles a grant in the matrix', async () => {
    const user = userEvent.setup()
    await renderApp('/security')
    await user.click(await screen.findByRole('tab', { name: 'Access' }))
    await screen.findByRole('option', { name: 'app_rw' })
    await user.selectOptions(screen.getByLabelText('Role'), 'app_rw')
    await user.selectOptions(screen.getByLabelText('Database'), 'shop')
    const box = await screen.findByRole('checkbox', { name: /delete on table public\.orders/i })
    expect(box).not.toBeChecked()
    await user.click(box)
    await waitFor(() =>
      expect(
        screen.getByRole('checkbox', { name: /delete on table public\.orders/i }),
      ).toBeChecked(),
    )
    expect(
      getState().grants.find((g) => g.grantee === 'app_rw' && g.table === 'orders')?.privileges,
    ).toContain('delete')
  })
})

describe('ledger', () => {
  it('generates a digest, verifies and looks up an inclusion proof', async () => {
    const user = userEvent.setup()
    await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    const before = getState().ledger.digests.length
    await user.click(screen.getByRole('button', { name: /generate digest/i }))
    await waitFor(() => expect(getState().ledger.digests.length).toBe(before + 1))

    await user.click(screen.getByRole('button', { name: /start verification/i }))
    expect(await screen.findByRole('progressbar', { name: /task progress/i })).toBeInTheDocument()
    await waitFor(() => expect(getState().ledger.verifications[0]?.state).toBe('passed'), {
      timeout: 4000,
    })

    await user.click(screen.getByRole('button', { name: /look up proof/i }))
    const proof = await screen.findByTestId('proof')
    expect(within(proof).getByText('Row hash')).toBeInTheDocument()
  })

  it('explains an unimplemented operation calmly', async () => {
    mockConfig.unsupported.add('createInclusionProof')
    const user = userEvent.setup()
    await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    await user.click(screen.getByRole('button', { name: /look up proof/i }))
    expect(await screen.findByText(/not available on this server version yet/i)).toBeInTheDocument()
  })
})

describe('settings', () => {
  it('saves the replica count, which starts the standby task and asks for no rebalance', async () => {
    const user = userEvent.setup()
    await renderApp('/settings')
    const replicas = await screen.findByLabelText(/default replica count/i)
    await user.clear(replicas)
    await user.type(replicas, '2')
    await user.click(screen.getByRole('button', { name: /save settings/i }))
    expect(await screen.findByText(/settings saved/i)).toBeInTheDocument()
    expect(getState().settings.defaultReplicaCount).toBe(2)
    // 0.7.0: the change starts the task standby.reconcile; it is not a rebalance.
    expect(getState().rebalance.reasons).not.toContain('replicaCountChanged')
    expect(getState().tasks.some((t) => t.task.kind === 'standby.reconcile')).toBe(true)
    // Features that are on cannot be turned off.
    expect(screen.getByRole('checkbox', { name: /sharding/i })).toBeDisabled()
  })

  it('accepts a replica count the servers cannot all place (as far as the servers allow)', async () => {
    const user = userEvent.setup()
    await renderApp('/settings')
    const replicas = await screen.findByLabelText(/default replica count/i)
    await user.clear(replicas)
    await user.type(replicas, '3')
    await user.click(screen.getByRole('button', { name: /save settings/i }))
    expect(await screen.findByText(/settings saved/i)).toBeInTheDocument()
    expect(getState().settings.defaultReplicaCount).toBe(3)
  })

  it('refuses a replica count above 3 before sending it', async () => {
    const user = userEvent.setup()
    await renderApp('/settings')
    const replicas = await screen.findByLabelText(/default replica count/i)
    await user.clear(replicas)
    await user.type(replicas, '4')
    await user.click(screen.getByRole('button', { name: /save settings/i }))
    expect(await screen.findByText(/between 0 and 3/i)).toBeInTheDocument()
    expect(getState().settings.defaultReplicaCount).toBe(1)
  })
})

describe('theme', () => {
  it('cycles system, light and dark and remembers the choice', async () => {
    const user = userEvent.setup()
    await renderApp('/dashboard')
    const button = await screen.findByRole('button', { name: /switch theme/i })
    expect(document.documentElement.hasAttribute('data-theme')).toBe(false)
    await user.click(button)
    expect(document.documentElement.getAttribute('data-theme')).toBe('light')
    await user.click(screen.getByRole('button', { name: /switch theme/i }))
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark')
    expect(localStorage.getItem('kingres.theme')).toBe('dark')
    await user.click(screen.getByRole('button', { name: /switch theme/i }))
    expect(document.documentElement.hasAttribute('data-theme')).toBe(false)
  })
})

describe('database detail', () => {
  it('distributes a plain table and drops a database behind a type-the-name confirmation', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const notes = (await screen.findByRole('rowheader', { name: 'public.notes' })).closest(
      'tr',
    ) as HTMLElement
    await user.click(within(notes).getByRole('button', { name: /distribute table/i }))
    const dialog = await screen.findByRole('dialog', { name: /distribute public\.notes/i })
    await user.type(within(dialog).getByLabelText(/distribution column/i), 'note_id')
    await user.click(within(dialog).getByRole('button', { name: /^distribute table$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: /^close$/i }))
    await waitFor(() =>
      expect(
        within(
          screen.getByRole('rowheader', { name: 'public.notes' }).closest('tr') as HTMLElement,
        ).getByText('Distributed'),
      ).toBeInTheDocument(),
    )

    await user.click(screen.getByRole('button', { name: /drop database/i }))
    const drop = await screen.findByRole('dialog', { name: /drop database shop/i })
    const confirm = within(drop).getByRole('button', { name: /^drop database$/i })
    expect(confirm).toBeDisabled()
    await user.type(within(drop).getByLabelText(/type shop to confirm/i), 'shop')
    expect(confirm).toBeEnabled()
    await user.click(confirm)
    await waitFor(
      () => expect(getState().databases.some((d) => d.info.name === 'shop')).toBe(false),
      { timeout: 4000 },
    )
  })
})
