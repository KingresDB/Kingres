// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.15.0, distributed schemas (design 14.2 and 14.3): the Schemas section of the database page (columns, null sizes,
// the moving badge, hostile names), what each role sees, the three dialogs (Distribute with its blockers, Move with the
// choice about tables that have no key, Make local with the typed name), the task states (running, failed with the
// failed step and the words that the schema is still in use), the mock operations, and the generated types.
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import TaskProgress from '@/components/TaskProgress.vue'
import { endpoints } from '@/api/endpoints'
import { ApiError, describeProblem } from '@/api/problem'
import { operations } from '@/api/operations.generated'
import { getState } from '@/mocks/http'
import { seedHostile, HOSTILE } from '@/mocks/hostile'
import { beginSchemaMove } from '@/mocks/schemas'
import { mockServer } from '@/mocks/server'
import { mockConfig } from '@/mocks/state'
import type { Task } from '@/api/types'
import { renderApp, renderComponent, settled } from './helpers'

afterEach(() => mockServer.events.removeAllListeners())

function recordBodies(method: string, path: string): Record<string, unknown>[] {
  const bodies: Record<string, unknown>[] = []
  mockServer.events.on('request:start', ({ request }) => {
    if (request.method !== method || new URL(request.url).pathname !== `/api/v1${path}`) return
    void request
      .clone()
      .json()
      .then((b: Record<string, unknown>) => bodies.push(b))
  })
  return bodies
}

const tenants = () => getState().databases.find((d) => d.info.name === 'tenants')!
const schemaRec = (name: string) => tenants().schemas!.find((s) => s.name === name)!

/** The page of the database `tenants`, once its schemas are listed. */
async function openPage(user = 'admin') {
  const u = userEvent.setup()
  const app = await renderApp('/databases/tenants', { user })
  await settled()
  const table = await screen.findByRole('table', { name: /schemas of the database/i })
  return { user: u, app, table }
}

const rowOf = (table: HTMLElement, name: string): HTMLElement =>
  within(table).getByRole('rowheader', { name }).closest('tr') as HTMLElement

describe('the Schemas section', () => {
  it('lists the schemas by name with owner, distribution, server group and server, tables and size', async () => {
    const { table } = await openPage()
    const heads = within(table)
      .getAllByRole('columnheader')
      .map((h) => h.textContent)
    expect(heads).toEqual([
      'Name',
      'Owner',
      'Distribution',
      'Server group and server',
      'Tables',
      'Size',
      'Actions',
    ])
    const names = within(table)
      .getAllByRole('rowheader')
      .map((h) => h.textContent)
    expect(names).toEqual([...names].sort())
    expect(names).toContain('tenant_acme')

    const acme = rowOf(table, 'tenant_acme')
    expect(within(acme).getByText('Distributed')).toBeInTheDocument()
    expect(within(acme).getByText('app_rw')).toBeInTheDocument()
    expect(acme).toHaveTextContent('Group 1')
    expect(acme).toHaveTextContent('10.0.0.11')
    expect(within(acme).getAllByRole('cell')[3]).toHaveTextContent('12')
    expect(acme).toHaveTextContent(/52(\.0)? MiB/)

    const local = rowOf(table, 'reporting')
    expect(within(local).getByText('Local')).toBeInTheDocument()
    expect(within(local).queryByText('Distributed')).not.toBeInTheDocument()
    expect(within(local).getAllByRole('cell')[2]).toHaveTextContent('-')
  })

  it('shows a size that the server does not know as n/a, never as 0', async () => {
    const { table } = await openPage()
    for (const name of ['tenant_globex', 'unsized']) {
      const row = rowOf(table, name)
      const size = within(row).getAllByRole('cell')[4] as HTMLElement
      expect(within(size).getByTestId('na')).toBeInTheDocument()
      expect(size).not.toHaveTextContent(/\b0\b/)
    }
    // A size of zero bytes is a size, and is shown as one.
    const scratch = within(rowOf(table, 'scratch')).getAllByRole('cell')[4] as HTMLElement
    expect(within(scratch).queryByTestId('na')).not.toBeInTheDocument()
    expect(scratch).toHaveTextContent('0')
  })

  it('shows a moving schema as "Moving to group N" with a link to its task', async () => {
    mockConfig.taskDurationMs = 20_000
    beginSchemaMove(getState(), tenants(), schemaRec('tenant_acme'), 3, false)
    const taskId = schemaRec('tenant_acme').taskId
    const { table } = await openPage()
    const row = rowOf(table, 'tenant_acme')
    expect(within(row).getByTestId('moving-badge')).toHaveTextContent('Moving to group 3')
    expect(within(row).queryByText('Distributed')).not.toBeInTheDocument()
    const link = await within(row).findByRole('link', { name: `Task ${taskId}` })
    expect(link).toHaveAttribute('href', `/tasks?task=${taskId}`)
  })

  it('disables the actions of a moving schema with the reason', async () => {
    mockConfig.taskDurationMs = 20_000
    beginSchemaMove(getState(), tenants(), schemaRec('tenant_acme'), 3, false)
    const { user, table } = await openPage()
    const row = rowOf(table, 'tenant_acme')
    for (const name of [/move schema/i, /make local/i]) {
      const button = within(row).getByRole('button', { name })
      expect(button).toHaveAttribute('aria-disabled', 'true')
      expect(button).toHaveAttribute('title', expect.stringMatching(/moving to group 3/i))
      await user.click(button)
    }
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('shows hostile names through SafeName: nothing is drawn as markup and hidden characters are shown', async () => {
    seedHostile(getState())
    const { table } = await openPage()
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect(document.querySelector('svg[onload]')).toBeNull()
    expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined()
    // The markup is text.
    expect(within(table).getAllByText(HOSTILE.markup).length).toBeGreaterThan(0)
    // The right-to-left override is made visible and named.
    expect(
      within(table).getAllByRole('img', { name: /hidden character U\+202E/i }).length,
    ).toBeGreaterThan(0)
    // A look-alike of another name is flagged.
    expect(
      within(table).getAllByText(/mixed scripts|look-alike|hidden characters/i).length,
    ).toBeGreaterThan(0)
    // The very long name is cut, with a way to see all of it.
    expect(
      within(table).getAllByRole('button', { name: /show all 5000 characters/i }),
    ).toHaveLength(1)
  })

  it('tells a database in the mode Rows that schemas are not distributed, and disables Distribute with the reason', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    await settled()
    const table = await screen.findByRole('table', { name: /schemas of the database/i })
    expect(screen.getByTestId('schemas-section')).toHaveTextContent(/shards by rows/i)
    const row = rowOf(table, 'public')
    const button = within(row).getByRole('button', { name: /distribute schema/i })
    expect(button).toHaveAttribute('aria-disabled', 'true')
    expect(button).toHaveAttribute('title', expect.stringMatching(/mode of this database is rows/i))
    await user.click(button)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('says sharding is off for a database without it, and lists its schemas as local', async () => {
    await renderApp('/databases/reports')
    await settled()
    const table = await screen.findByRole('table', { name: /schemas of the database/i })
    expect(screen.getByTestId('schemas-section')).toHaveTextContent(/sharding is off/i)
    expect(within(rowOf(table, 'public')).getByText('Local')).toBeInTheDocument()
    expect(
      within(rowOf(table, 'public')).getByRole('button', { name: /distribute schema/i }),
    ).toHaveAttribute('title', expect.stringMatching(/turn on sharding/i))
  })

  it('states the mode, the default and the number of distributed schemas on the page, whatever the role', async () => {
    await renderApp('/databases/tenants', { user: 'viewer' })
    await settled()
    expect(await screen.findByTestId('sharding-mode')).toHaveTextContent('Schemas')
    expect(screen.getByTestId('distribute-new')).toHaveTextContent(/distributed automatically/i)
    expect(screen.getByText(/not used in the mode schemas/i)).toBeInTheDocument()
  })
})

describe('what each role sees', () => {
  const need: [string, RegExp, string][] = [
    ['Distribute schema', /distribute schema/i, 'kingres_admin'],
    ['Move schema', /move schema/i, 'kingres_ops'],
    ['Make local', /make local/i, 'kingres_admin'],
  ]

  it('kingres_readonly sees the list and the buttons disabled, each naming the role it needs', async () => {
    const { user, table } = await openPage('viewer')
    expect(within(table).getAllByRole('rowheader').length).toBeGreaterThan(5)
    for (const [label, name, role] of need) {
      const row = rowOf(table, label === 'Distribute schema' ? 'reporting' : 'tenant_acme')
      const button = within(row).getByRole('button', { name })
      expect(button).toHaveAttribute('aria-disabled', 'true')
      expect(button).toHaveAttribute('title', `Requires the ${role} cluster role.`)
      await user.click(button)
    }
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('kingres_ops can move, and still needs kingres_admin to distribute and to make local', async () => {
    const { user, table } = await openPage('ops')
    const acme = rowOf(table, 'tenant_acme')
    expect(within(acme).getByRole('button', { name: /move schema/i })).not.toHaveAttribute(
      'aria-disabled',
    )
    expect(within(acme).getByRole('button', { name: /make local/i })).toHaveAttribute(
      'title',
      'Requires the kingres_admin cluster role.',
    )
    expect(
      within(rowOf(table, 'reporting')).getByRole('button', { name: /distribute schema/i }),
    ).toHaveAttribute('title', 'Requires the kingres_admin cluster role.')
    await user.click(within(acme).getByRole('button', { name: /move schema/i }))
    expect(
      await screen.findByRole('dialog', { name: /move schema tenant_acme/i }),
    ).toBeInTheDocument()
  })

  it('kingres_admin can use all three', async () => {
    const { table } = await openPage('admin')
    for (const [label, name] of need) {
      const row = rowOf(table, label === 'Distribute schema' ? 'reporting' : 'tenant_acme')
      expect(within(row).getByRole('button', { name })).not.toHaveAttribute('aria-disabled')
    }
  })

  it('kingres_stats has no access to the page', async () => {
    await renderApp('/databases/tenants', { user: 'metrics' })
    expect(await screen.findByText(/your account cannot open this page/i)).toBeInTheDocument()
    expect(screen.queryByTestId('schemas-section')).not.toBeInTheDocument()
  })
})

describe('Distribute schema', () => {
  async function open(name: string) {
    const { user, table } = await openPage()
    await user.click(within(rowOf(table, name)).getByRole('button', { name: /distribute schema/i }))
    const dialog = await screen.findByRole('dialog', { name: /distribute schema/i })
    return { user, dialog }
  }

  it('lists the blockers and keeps the button disabled, saying why', async () => {
    const puts = recordBodies('PUT', '/databases/tenants/schemas/blocked_mv/distribution')
    const { user, dialog } = await open('blocked_mv')
    const blockers = await within(dialog).findByRole('table', {
      name: /keep the schema from being distributed/i,
    })
    expect(within(blockers).getAllByRole('row')).toHaveLength(3)
    expect(blockers).toHaveTextContent('blocked_mv.orders_mv')
    expect(blockers).toHaveTextContent(
      'materialized views are not supported in distributed schemas yet',
    )
    expect(blockers).toHaveTextContent('blocked_mv.items_order_fk')
    const button = within(dialog).getByRole('button', { name: /^distribute schema$/i })
    expect(button).toBeDisabled()
    expect(button).toHaveAttribute(
      'title',
      expect.stringMatching(/2 objects that distributed schemas do not support/i),
    )
    expect(button).toHaveAccessibleDescription(/2 objects that distributed schemas do not support/i)
    expect(within(dialog).queryByLabelText(/place on server group/i)).not.toBeInTheDocument()
    await user.click(button)
    expect(puts).toHaveLength(0)
  })

  it('names the schema public as its own blocker', async () => {
    const { dialog } = await open('public')
    expect(
      await within(dialog).findByText('the schema public cannot be distributed'),
    ).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: /^distribute schema$/i })).toBeDisabled()
  })

  it('does not draw hostile blockers as markup', async () => {
    seedHostile(getState())
    const { user, table } = await openPage()
    // The local schema whose blockers have hostile kinds, names and reasons (its own name ends in a right-to-left override).
    const row = within(table)
      .getAllByRole('row')
      .find((r) => r.textContent?.startsWith('blocked') && !r.textContent.startsWith('blocked_mv'))!
    await user.click(within(row).getByRole('button', { name: /distribute schema/i }))
    const dialog = await screen.findByRole('dialog', { name: /distribute schema/i })
    const blockers = await within(dialog).findByRole('table', { name: /keep the schema/i })
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect(document.querySelector('svg[onload]')).toBeNull()
    expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined()
    expect(within(blockers).getAllByText(HOSTILE.markup).length).toBeGreaterThan(0)
    expect(within(blockers).getAllByText(HOSTILE.breakout).length).toBeGreaterThan(0)
    expect(
      within(blockers).getAllByRole('img', { name: /hidden character/i }).length,
    ).toBeGreaterThan(0)
    expect(within(dialog).getByRole('button', { name: /^distribute schema$/i })).toBeDisabled()
  })

  it('says what happens to the data, offers the groups and starts the task: an empty schema goes to the chosen group', async () => {
    const puts = recordBodies('PUT', '/databases/tenants/schemas/scratch/distribution')
    const { user, dialog } = await open('scratch')
    expect(
      await within(dialog).findByText(
        /the data stays where it is\. other servers receive empty tables\. you can move the schema afterwards\./i,
      ),
    ).toBeInTheDocument()
    const select = within(dialog).getByLabelText(/place on server group/i)
    expect(
      within(select)
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual([
      'Automatic (in place)',
      'Group 1 (server 10.0.0.11)',
      'Group 2 (server 10.0.0.12)',
      'Group 3 (server 10.0.0.13)',
    ])
    await user.selectOptions(select, '2')
    await user.click(within(dialog).getByRole('button', { name: /^distribute schema$/i }))
    expect(
      await within(dialog).findByRole('progressbar', { name: /task progress/i }),
    ).toBeInTheDocument()
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await waitFor(() => expect(puts).toHaveLength(1))
    expect(puts[0]).toEqual({ shardGroupId: 2 })
    expect(schemaRec('scratch')).toMatchObject({ group: 2, state: 'active' })
    // The steps of the task are named as the contract names them.
    for (const step of [
      'Check the schema',
      'Set REPLICA IDENTITY FULL',
      'Make the schema distributed',
      'Verify on every server',
    ])
      expect(within(dialog).getByText(step)).toBeInTheDocument()
    expect(within(dialog).getByText('Set REPLICA IDENTITY FULL').closest('li')).toHaveAttribute(
      'data-state',
      'skipped',
    )
    // The list shows the schema as distributed once the dialog is closed.
    await user.click(within(dialog).getByRole('button', { name: /^close$/i }))
    const table = await screen.findByRole('table', { name: /schemas of the database/i })
    await waitFor(() =>
      expect(within(rowOf(table, 'scratch')).getByText('Distributed')).toBeInTheDocument(),
    )
  })

  it('without a group the placement rule chooses, and sends no group', async () => {
    const puts = recordBodies('PUT', '/databases/tenants/schemas/scratch/distribution')
    const { user, dialog } = await open('scratch')
    await user.click(await within(dialog).findByRole('button', { name: /^distribute schema$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await waitFor(() => expect(puts).toHaveLength(1))
    expect(puts[0]).toEqual({})
    // Group 3 holds the fewest distributed schemas.
    expect(schemaRec('scratch').group).toBe(3)
  })

  it('can also set REPLICA IDENTITY FULL on the tables without a key, and then runs that step', async () => {
    const puts = recordBodies('PUT', '/databases/tenants/schemas/scratch/distribution')
    const { user, dialog } = await open('scratch')
    await user.click(
      await within(dialog).findByRole('checkbox', {
        name: /set replica identity full on the tables that have no primary key/i,
      }),
    )
    await user.click(within(dialog).getByRole('button', { name: /^distribute schema$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await waitFor(() => expect(puts).toHaveLength(1))
    expect(puts[0]).toEqual({ setReplicaIdentityFull: true })
    expect(within(dialog).getByText('Set REPLICA IDENTITY FULL').closest('li')).toHaveAttribute(
      'data-state',
      'done',
    )
  })

  it('shows the refusal that lists the blockers when one appeared after the dialog was opened', async () => {
    const { user, dialog } = await open('scratch')
    await within(dialog).findByLabelText(/place on server group/i)
    schemaRec('scratch').blockers = [
      { objectKind: 'view', objectName: 'scratch.v', reason: 'views are not supported yet' },
    ]
    await user.click(within(dialog).getByRole('button', { name: /^distribute schema$/i }))
    const notice = await within(dialog).findByRole('alert')
    expect(notice).toHaveAttribute('data-code', 'schema_not_distributable')
    expect(notice).toHaveTextContent('scratch.v')
    expect(notice).toHaveTextContent('view scratch.v: views are not supported yet')
    expect(schemaRec('scratch').group).toBeNull()
  })

  it('keeps the button disabled, saying why, when the schema could not be read', async () => {
    mockServer.use(
      http.get('*/api/v1/databases/tenants/schemas/scratch', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 503, code: 'leader_unavailable' },
          { status: 503, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    )
    const { dialog } = await open('scratch')
    const button = await within(dialog).findByRole('button', { name: /^distribute schema$/i })
    await waitFor(() =>
      expect(button).toHaveAttribute('title', expect.stringMatching(/could not be read/i)),
    )
    expect(button).toBeDisabled()
  })
})

describe('Move schema', () => {
  async function open(name: string, form = true) {
    const { user: u, table } = await openPage()
    await u.click(within(rowOf(table, name)).getByRole('button', { name: /move schema/i }))
    const dialog = await screen.findByRole('dialog', { name: /move schema/i })
    // A schema that is pinned has no choice of group to wait for.
    if (form) await within(dialog).findByLabelText(/move to server group/i)
    else await within(dialog).findByRole('alert')
    return { user: u, dialog }
  }
  const groupOptions = (dialog: HTMLElement) =>
    within(within(dialog).getByLabelText(/move to server group/i))
      .getAllByRole('option')
      .map((o) => o.textContent)

  it('offers the groups that may hold shards, without the one the schema is on and without the map group', async () => {
    const { dialog } = await open('tenant_acme')
    expect(groupOptions(dialog)).toEqual([
      'Choose a group',
      'Group 2 (server 10.0.0.12)',
      'Group 3 (server 10.0.0.13)',
    ])
  })

  it('leaves out a group whose server should not hold shards', async () => {
    getState().servers.find((x) => x.id === '10.0.0.13')!.shouldHaveShards = false
    const { dialog } = await open('tenant_acme')
    expect(groupOptions(dialog)).toEqual(['Choose a group', 'Group 2 (server 10.0.0.12)'])
  })

  it('warns that the writers wait for a moment, and that other schemas are not affected', async () => {
    const { dialog } = await open('tenant_acme')
    expect(within(dialog).getByTestId('move-warning')).toHaveTextContent(
      'Writers of this schema wait for a moment while the move switches over (normally well under a second). Other schemas are not affected.',
    )
  })

  it('asks for a target before it sends anything', async () => {
    const posts = recordBodies('POST', '/databases/tenants/schemas/tenant_acme/moves')
    const { user, dialog } = await open('tenant_acme')
    await user.click(within(dialog).getByRole('button', { name: /^move schema$/i }))
    expect(
      await within(dialog).findByText(/choose the group the schema moves to/i),
    ).toBeInTheDocument()
    expect(posts).toHaveLength(0)
  })

  it('moves the schema, shows the copy in bytes and ends with the schema on the target group', async () => {
    mockConfig.taskDurationMs = 1500
    const posts = recordBodies('POST', '/databases/tenants/schemas/tenant_acme/moves')
    const { user, dialog } = await open('tenant_acme')
    await user.selectOptions(within(dialog).getByLabelText(/move to server group/i), '2')
    await user.click(within(dialog).getByRole('button', { name: /^move schema$/i }))
    // Running: the progress and the steps of the contract.
    expect(
      await within(dialog).findByRole('progressbar', { name: /task progress/i }),
    ).toBeInTheDocument()
    for (const step of [
      'Check the move',
      'Set REPLICA IDENTITY FULL',
      'Copy the data',
      'Switch',
      'Clean up the old copy',
    ])
      expect(await within(dialog).findByText(step)).toBeInTheDocument()
    expect(getState().tasks[0]?.task.state).toBe('running')
    expect(schemaRec('tenant_acme')).toMatchObject({ state: 'moving', movingTo: 2 })
    // The copy counts bytes: a size that is not zero, of the 52 MiB of the schema.
    await waitFor(() =>
      expect(within(dialog).getByTestId('step-items')).toHaveTextContent(
        /^(?!0 B)[\d.]+ (KiB|MiB) of 52(\.0)? MiB$/,
      ),
    )
    expect(
      await within(dialog).findByText('Finished.', {}, { timeout: 10_000 }),
    ).toBeInTheDocument()
    expect(posts).toEqual([{ targetShardGroupId: 2 }])
    expect(schemaRec('tenant_acme')).toMatchObject({ group: 2, state: 'active', movingTo: null })
  })

  it('shows a failed move with the failed step, the error and the words that the schema is still in use', async () => {
    const { user, dialog } = await open('failme')
    await user.selectOptions(within(dialog).getByLabelText(/move to server group/i), '1')
    await user.click(within(dialog).getByRole('button', { name: /^move schema$/i }))
    const error = await within(dialog).findByText(/ran out of disk space/i, {}, { timeout: 10_000 })
    expect(error.closest('[role="alert"]')).toHaveTextContent('(step: Copy the data)')
    // The failed step is the one marked failed in the list of steps.
    const failed = dialog.querySelectorAll('li[data-state="failed"]')
    expect(failed).toHaveLength(1)
    expect(failed[0]).toHaveTextContent('Copy the data')
    expect(within(dialog).getByTestId('move-failed-note')).toHaveTextContent(
      'The schema is still on its original group and in use. You can start the move again.',
    )
    expect(within(dialog).queryByText('Finished.')).not.toBeInTheDocument()
    expect(schemaRec('failme')).toMatchObject({
      group: 2,
      state: 'active',
      movingTo: null,
      taskId: null,
    })
  })

  it('asks the server and, when it names tables without a key, requires the replica identity choice before it moves again', async () => {
    const posts = recordBodies('POST', '/databases/tenants/schemas/tenant_initech/moves')
    const { user, dialog } = await open('tenant_initech')
    await user.selectOptions(within(dialog).getByLabelText(/move to server group/i), '1')
    expect(within(dialog).queryByTestId('keyless-tables')).not.toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: /^move schema$/i }))

    const keyless = await within(dialog).findByTestId('keyless-tables')
    expect(keyless).toHaveTextContent('tenant_initech.audit_log, tenant_initech.sessions')
    const box = within(keyless).getByRole('checkbox', {
      name: /^set replica identity full on these tables$/i,
    })
    expect(box).not.toBeChecked()
    const move = within(dialog).getByRole('button', { name: /^move schema$/i })
    expect(move).toBeDisabled()
    expect(move).toHaveAttribute(
      'title',
      expect.stringMatching(/set replica identity full on these tables/i),
    )
    await user.click(move)
    expect(posts).toHaveLength(1)

    await user.click(box)
    expect(move).toBeEnabled()
    await user.click(move)
    expect(
      await within(dialog).findByText('Finished.', {}, { timeout: 10_000 }),
    ).toBeInTheDocument()
    expect(posts).toEqual([
      { targetShardGroupId: 1 },
      { targetShardGroupId: 1, setReplicaIdentityFull: true },
    ])
    expect(schemaRec('tenant_initech')).toMatchObject({ group: 1, keyless: [] })
    expect(within(dialog).getByText('Set REPLICA IDENTITY FULL').closest('li')).toHaveAttribute(
      'data-state',
      'done',
    )
  })

  it('shows the pin of a ledger table as the problem notice and does not offer the move', async () => {
    const { dialog } = await open('ledgered', false)
    const notice = await within(dialog).findByRole('alert')
    expect(notice).toHaveAttribute('data-code', 'schema_pinned')
    expect(notice).toHaveTextContent(/the schema holds a ledger table/i)
    expect(within(dialog).getByRole('button', { name: /^move schema$/i })).toBeDisabled()
    expect(within(dialog).queryByLabelText(/move to server group/i)).not.toBeInTheDocument()
  })

  it('shows the refusal while a rebalance runs as the problem notice', async () => {
    const { user, dialog } = await open('tenant_acme')
    getState().rebalance.state = 'running'
    await user.selectOptions(within(dialog).getByLabelText(/move to server group/i), '2')
    await user.click(within(dialog).getByRole('button', { name: /^move schema$/i }))
    expect(await within(dialog).findByRole('alert')).toHaveAttribute(
      'data-code',
      'rebalance_in_progress',
    )
    expect(schemaRec('tenant_acme').state).toBe('active')
  })
})

describe('Make local', () => {
  async function open(name: string) {
    const { user, table } = await openPage()
    await user.click(within(rowOf(table, name)).getByRole('button', { name: /make local/i }))
    const dialog = await screen.findByRole('dialog', { name: /make schema .* local/i })
    await within(dialog).findByLabelText(/move to group first/i)
    return { user, dialog }
  }

  it('says where the schema ends up, and asks for the name before the button works', async () => {
    const dels: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      if (request.method === 'DELETE')
        dels.push(new URL(request.url).pathname + new URL(request.url).search)
    })
    const { user, dialog } = await open('tenant_acme')
    expect(within(dialog).getByTestId('make-local-sentence')).toHaveTextContent(
      /becomes a local schema on server group 1 \(server 10\.0\.0\.11\)/i,
    )
    expect(within(dialog).getByTestId('make-local-sentence')).toHaveTextContent(
      /clients connected to other servers will no longer see it/i,
    )
    const button = within(dialog).getByRole('button', { name: /^make schema local$/i })
    expect(button).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/type .*tenant_acme.* to confirm/i), 'tenant_acm')
    expect(button).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/type .*tenant_acme.* to confirm/i), 'e')
    expect(button).toBeEnabled()
    await user.click(button)
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    expect(dels).toEqual(['/api/v1/databases/tenants/schemas/tenant_acme/distribution'])
    expect(schemaRec('tenant_acme').group).toBeNull()
    expect(within(dialog).getByText('Move first').closest('li')).toHaveAttribute(
      'data-state',
      'skipped',
    )
  })

  it('can move the schema to another group first, and says that the schema ends up there', async () => {
    const dels: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      if (request.method === 'DELETE') dels.push(new URL(request.url).search)
    })
    const { user, dialog } = await open('tenant_acme')
    await user.selectOptions(within(dialog).getByLabelText(/move to group first/i), '3')
    expect(within(dialog).getByTestId('make-local-sentence')).toHaveTextContent(
      /becomes a local schema on server group 3 \(server 10\.0\.0\.13\)/i,
    )
    await user.type(
      within(dialog).getByLabelText(/type .*tenant_acme.* to confirm/i),
      'tenant_acme',
    )
    await user.click(within(dialog).getByRole('button', { name: /^make schema local$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    expect(dels).toEqual(['?shardGroupId=3'])
    expect(within(dialog).getByText('Move first').closest('li')).toHaveAttribute(
      'data-state',
      'done',
    )
  })

  it('shows the pin of a ledger table as the problem notice, with the button disabled', async () => {
    const { user, table } = await openPage()
    await user.click(within(rowOf(table, 'ledgered')).getByRole('button', { name: /make local/i }))
    const dialog = await screen.findByRole('dialog', { name: /make schema .* local/i })
    const notice = await within(dialog).findByRole('alert')
    expect(notice).toHaveAttribute('data-code', 'schema_pinned')
    expect(within(dialog).getByRole('button', { name: /^make schema local$/i })).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/to confirm/i), 'ledgered')
    expect(within(dialog).getByRole('button', { name: /^make schema local$/i })).toBeDisabled()
  })

  it('draws the name of a hostile schema as text in the title and in the confirmation, with the way to copy it', async () => {
    seedHostile(getState())
    const { user, table } = await openPage()
    // The schema whose name is the markup is distributed (the hostile seed distributes every other one).
    const row = within(table)
      .getAllByRole('row')
      .find(
        (r) => r.textContent?.includes(HOSTILE.markup) && r.textContent.includes('Distributed'),
      )!
    await user.click(within(row).getByRole('button', { name: /make local/i }))
    const dialog = await screen.findByRole('dialog', { name: /make schema/i })
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined()
    expect(within(dialog).getAllByText(HOSTILE.markup).length).toBeGreaterThan(0)
  })
})

describe('task states', () => {
  it('shows a pin that appears during a task as the problem notice of schema_pinned', async () => {
    const task: Task = {
      id: 77,
      kind: 'schema.undistribute',
      state: 'failed',
      progress: {
        percent: 25,
        currentStep: null,
        steps: [
          { name: 'Check the schema', state: 'failed' },
          { name: 'Make the schema local', state: 'skipped' },
        ],
      },
      error: {
        code: 'schema_pinned',
        message: 'The schema holds a ledger table.',
        step: 'Check the schema',
        retryable: false,
      },
      detail: null,
      subject: { type: 'schema', id: 'tenants/ledgered' },
      cancellable: false,
      createdAt: '2026-11-22T10:00:00Z',
      updatedAt: '2026-11-22T10:00:01Z',
      createdBy: 'admin',
    }
    await renderComponent(TaskProgress, { task })
    const notice = await screen.findByRole('alert')
    expect(notice).toHaveAttribute('data-code', 'schema_pinned')
    expect(notice).toHaveTextContent('The schema holds a ledger table.')
    expect(screen.queryByTestId('move-failed-note')).not.toBeInTheDocument()
  })

  it('names the failed move of a schema in the details of the task page', async () => {
    const user = userEvent.setup()
    const app = await renderApp('/tasks')
    const task = await endpoints.moveSchema('tenants', 'failme', { targetShardGroupId: 1 })
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('failed'))
    await app.queryClient.invalidateQueries({ queryKey: ['tasks'] })
    await user.click(await screen.findByRole('button', { name: /details/i }))
    const dialog = await screen.findByRole('dialog', { name: new RegExp(`task ${task.id}`, 'i') })
    expect(await within(dialog).findByTestId('move-failed-note')).toHaveTextContent(
      /the schema is still on its original group and in use/i,
    )
  })

  it('has words for the three task kinds in the task list', async () => {
    const app = await renderApp('/tasks')
    await endpoints.moveSchema('tenants', 'tenant_acme', { targetShardGroupId: 2 })
    await app.queryClient.invalidateQueries({ queryKey: ['tasks'] })
    expect(await screen.findByText('Move a schema to another server group')).toBeInTheDocument()
    expect(screen.getByText('schema tenants/tenant_acme')).toBeInTheDocument()
  })
})

describe('the mock operations follow the contract', () => {
  const code = async (p: Promise<unknown>) => {
    try {
      await p
    } catch (e) {
      return e instanceof ApiError ? `${e.status} ${e.code}` : String(e)
    }
    return 'ok'
  }

  async function signedIn() {
    await renderApp('/dashboard')
  }

  it('lists schemas with the filters of the contract and pages them by cursor', async () => {
    await signedIn()
    const all = await endpoints.listSchemas('tenants')
    expect(all.items.map((s) => s.name)).toEqual([...all.items.map((s) => s.name)].sort())
    const local = await endpoints.listSchemas('tenants', { distributed: false })
    expect(local.items.every((s) => s.distribution === null)).toBe(true)
    const dist = await endpoints.listSchemas('tenants', { distributed: true })
    expect(dist.items.every((s) => s.distribution !== null)).toBe(true)
    expect(local.items.length + dist.items.length).toBe(all.items.length)
    const g2 = await endpoints.listSchemas('tenants', { shardGroupId: 2 })
    expect(g2.items.map((s) => s.name)).toEqual(['failme', 'tenant_globex'])
    expect((await endpoints.listSchemas('tenants', { state: 'moving' })).items).toEqual([])
    const first = await endpoints.listSchemas('tenants', { limit: 2 })
    expect(first.items).toHaveLength(2)
    expect(first.nextCursor).not.toBeNull()
    const second = await endpoints.listSchemas('tenants', { limit: 2, cursor: first.nextCursor! })
    expect(second.items[0]?.name).not.toBe(first.items[0]?.name)
    // A database without sharding lists its schemas with `distribution: null`.
    expect(
      (await endpoints.listSchemas('reports')).items.every((s) => s.distribution === null),
    ).toBe(true)
  })

  it('reports the blockers, the pin and the move in progress of one schema', async () => {
    await signedIn()
    expect((await endpoints.getSchema('tenants', 'blocked_mv')).distributionBlockers).toHaveLength(
      2,
    )
    expect((await endpoints.getSchema('tenants', 'ledgered')).pinned?.reason).toMatch(
      /ledger table/,
    )
    expect((await endpoints.getSchema('tenants', 'tenant_acme')).distributionBlockers).toEqual([])
    expect(await code(endpoints.getSchema('tenants', 'nope'))).toBe('404 not_found')
    mockConfig.taskDurationMs = 20_000
    const task = await endpoints.moveSchema('tenants', 'tenant_acme', { targetShardGroupId: 2 })
    const detail = await endpoints.getSchema('tenants', 'tenant_acme')
    expect(detail.moveInProgress).toBe(task.id)
    expect(detail.distribution).toMatchObject({ state: 'moving', movingToShardGroupId: 2 })
  })

  it('refuses what the contract refuses', async () => {
    await signedIn()
    // distributeSchema
    expect(await code(endpoints.distributeSchema('shop', 'public', {}))).toBe('409 conflict')
    expect(await code(endpoints.distributeSchema('reports', 'public', {}))).toBe('409 conflict')
    expect(await code(endpoints.distributeSchema('tenants', 'tenant_acme', {}))).toBe(
      '409 conflict',
    )
    expect(await code(endpoints.distributeSchema('tenants', 'blocked_mv', {}))).toBe(
      '422 schema_not_distributable',
    )
    expect(await code(endpoints.distributeSchema('tenants', 'scratch', { shardGroupId: 9 }))).toBe(
      '400 validation_failed',
    )
    // undistributeSchema
    expect(await code(endpoints.undistributeSchema('tenants', 'reporting'))).toBe('409 conflict')
    expect(await code(endpoints.undistributeSchema('tenants', 'ledgered'))).toBe(
      '409 schema_pinned',
    )
    // moveSchema
    expect(await code(endpoints.moveSchema('tenants', 'ledgered', { targetShardGroupId: 2 }))).toBe(
      '409 schema_pinned',
    )
    expect(
      await code(endpoints.moveSchema('tenants', 'reporting', { targetShardGroupId: 2 })),
    ).toBe('409 conflict')
    expect(
      await code(endpoints.moveSchema('tenants', 'tenant_acme', { targetShardGroupId: 1 })),
    ).toBe('409 conflict')
    expect(
      await code(endpoints.moveSchema('tenants', 'tenant_initech', { targetShardGroupId: 1 })),
    ).toBe('422 unprocessable')
    mockConfig.taskDurationMs = 20_000
    await endpoints.moveSchema('tenants', 'tenant_acme', { targetShardGroupId: 2 })
    expect(
      await code(endpoints.moveSchema('tenants', 'tenant_acme', { targetShardGroupId: 3 })),
    ).toBe('409 schema_move_in_progress')
    expect(await code(endpoints.undistributeSchema('tenants', 'tenant_acme'))).toBe(
      '409 schema_move_in_progress',
    )
  })

  it('stops a move before the switch, leaves the schema on the source, and answers 409 after the switch', async () => {
    await signedIn()
    mockConfig.taskDurationMs = 20_000
    const early = await endpoints.moveSchema('tenants', 'tenant_acme', { targetShardGroupId: 2 })
    expect(early.cancellable).toBe(true)
    const cancelled = await endpoints.cancelTask(early.id)
    expect(cancelled).toMatchObject({ state: 'failed', error: { code: 'cancelled' } })
    expect(schemaRec('tenant_acme')).toMatchObject({ group: 1, state: 'active', movingTo: null })
    // Past 60 percent the task is at the switch.
    const late = await endpoints.moveSchema('tenants', 'tenant_acme', { targetShardGroupId: 2 })
    getState().tasks.find((t) => t.task.id === late.id)!.startedAt = Date.now() - 3000
    expect(await code(endpoints.cancelTask(late.id))).toBe('409 task_not_cancellable')
  })

  it('lists the move of a schema among the moves in flight, with the schema name, and the schemas per server', async () => {
    await signedIn()
    mockConfig.taskDurationMs = 20_000
    await endpoints.moveSchema('tenants', 'tenant_acme', { targetShardGroupId: 2 })
    const status = await endpoints.getRebalanceStatus()
    expect(status.inFlightMoves).toEqual([
      expect.objectContaining({
        kind: 'schema',
        schemaName: 'tenant_acme',
        databaseName: 'tenants',
        bucket: 0,
        fromShardGroupId: 1,
        toShardGroupId: 2,
      }),
    ])
    const dist = await endpoints.getBucketDistribution('tenants')
    expect(dist.servers.map((s) => s.primarySchemas)).toEqual([2, 2, 1])
    expect(dist.servers.map((s) => s.primaryBuckets)).toEqual([2, 2, 1])
  })

  it('lists the colocation groups of kind schema, and only the hash groups when asked for them', async () => {
    await signedIn()
    const all = (await endpoints.listColocationGroups('tenants')).items
    expect(
      all
        .filter((g) => g.kind === 'schema')
        .map((g) => g.schemaName)
        .sort(),
    ).toEqual(['failme', 'ledgered', 'tenant_acme', 'tenant_globex', 'tenant_initech'])
    expect(all.every((g) => g.bucketCount === 1 && g.distributionColumnType === null)).toBe(true)
    expect((await endpoints.listColocationGroups('tenants', 'hash')).items).toEqual([])
    const shopHash = await endpoints.listColocationGroups('shop', 'hash')
    expect(shopHash.items.length).toBeGreaterThan(0)
    expect(shopHash.items.every((g) => g.kind === undefined)).toBe(true)
    expect((await endpoints.listColocationGroups('shop', 'schema')).items).toEqual([])
  })

  it('changes the mode as the contract says', async () => {
    await signedIn()
    // Rows is refused while distributed schemas exist, Schemas while hash tables exist; Both is always allowed.
    expect(await code(endpoints.updateDatabase('tenants', { sharding: { mode: 'row' } }))).toBe(
      '409 sharding_mode_in_use',
    )
    expect(await code(endpoints.updateDatabase('shop', { sharding: { mode: 'schema' } }))).toBe(
      '409 sharding_mode_in_use',
    )
    expect(await code(endpoints.updateDatabase('shop', { sharding: { mode: 'both' } }))).toBe('ok')
    expect(await code(endpoints.updateDatabase('tenants', { sharding: { mode: 'both' } }))).toBe(
      'ok',
    )
    expect(await code(endpoints.updateDatabase('shop', { sharding: { mode: 'row' } }))).toBe('ok')
    // The default for new schemas needs a mode that has schemas.
    expect(
      await code(endpoints.updateDatabase('shop', { sharding: { distributeNewSchemas: true } })),
    ).toBe('400 validation_failed')
    expect(await code(endpoints.updateDatabase('reports', { sharding: { mode: 'schema' } }))).toBe(
      '409 conflict',
    )
    const shop = await endpoints.getDatabase('shop')
    expect(shop.sharding).toMatchObject({ mode: 'row', distributeNewSchemas: false })
    expect((await endpoints.getDatabase('tenants')).stats.distributedSchemaCount).toBe(5)
  })

  it('creates a database in the mode Schemas without a bucket count, and refuses the default for new schemas in the mode Rows', async () => {
    await signedIn()
    const task = await endpoints.createDatabase({
      name: 'n1',
      sharding: { enabled: true, mode: 'schema', bucketCount: 64, distributeNewSchemas: true },
    })
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('done'))
    expect((await endpoints.getDatabase('n1')).sharding).toMatchObject({
      mode: 'schema',
      bucketCount: null,
      distributeNewSchemas: true,
    })
    expect(
      await code(
        endpoints.createDatabase({
          name: 'n2',
          sharding: { enabled: true, distributeNewSchemas: true },
        }),
      ),
    ).toBe('400 validation_failed')
    expect((await endpoints.getDatabaseCreateOptions()).shardingModes).toEqual({
      modes: ['row', 'schema', 'both'],
      default: 'row',
    })
  })
})

describe('the generated types and the words of 0.15.0', () => {
  // `npm run api:check` (and tests/contract.test.ts) compare the generated files with the contract as a whole; this names what
  // 0.15.0 put in them, so that a revert of the regeneration is told by its own test.
  it('carry the paths, schemas and codes of 0.15.0 (kept up to date by `npm run api:generate`)', () => {
    const types = readFileSync(resolve('src/api/schema.d.ts'), 'utf8')
    for (const part of [
      '"/databases/{databaseName}/schemas"',
      '"/databases/{databaseName}/schemas/{schemaName}/moves"',
      'SchemaSummary: {',
      'SchemaDetail: ',
      'MoveSchemaRequest: {',
      '"schema.move"',
      '"schema_pinned"',
      'primarySchemas?: number',
    ])
      expect(types, part).toContain(part)
  })

  it('has the five operations with the roles of the contract', () => {
    expect(operations.listSchemas.role).toBe('kingres_readonly')
    expect(operations.getSchema.role).toBe('kingres_readonly')
    expect(operations.distributeSchema.role).toBe('kingres_admin')
    expect(operations.undistributeSchema.role).toBe('kingres_admin')
    expect(operations.moveSchema.role).toBe('kingres_ops')
    expect(Object.keys(operations)).toHaveLength(137)
  })

  it('has a heading and an explanation for each new problem code, none showing as the raw code', () => {
    for (const code of [
      'sharding_mode_in_use',
      'schema_not_distributable',
      'schema_pinned',
      'schema_move_in_progress',
    ] as const) {
      const view = describeProblem(
        new ApiError({ type: 'about:blank', title: code, status: 409, code }),
      )
      expect(view.heading, code).not.toBe(code)
      expect(view.message.length, code).toBeGreaterThan(30)
      expect(view.message, code).not.toContain(code)
    }
  })
})
