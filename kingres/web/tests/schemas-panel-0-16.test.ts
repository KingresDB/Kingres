// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
// What the panel of the distributed schemas (contract 0.16.0) says beyond the dialogs of the first pass: the group and the replica
// identity of a distribution, the setting "distribute new schemas" when the mode changes, the words of a schema task that ends in
// a refusal or fails, and the schemas per server group on the Shards page. Against the mock API.
import { describe, expect, it } from 'vitest'
import { screen, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import TaskProgress from '@/components/TaskProgress.vue'
import type { Task } from '@/api/types'
import { renderApp, renderComponent, settled } from './helpers'

async function openSchemas() {
  const u = userEvent.setup()
  await renderApp('/databases/shop')
  await settled()
  const section = await screen.findByTestId('schemas-section')
  return { u, section }
}

describe('Distribute schema: the group and the replica identity', () => {
  it('sends the group and the replica identity that were chosen, and the schema ends up there', async () => {
    const bodies: unknown[] = []
    mockServer.events.on('request:start', ({ request }) => {
      if (request.method === 'PUT' && request.url.endsWith('/schemas/legacy/distribution'))
        void request
          .clone()
          .json()
          .then((b) => bodies.push(b))
    })
    const { u, section } = await openSchemas()
    const row = (await within(section).findByText('legacy')).closest('tr')!
    await u.click(within(row).getByTestId('distribute-schema'))
    const dialog = await screen.findByRole('dialog', { name: /distribute schema/i })
    await within(dialog).findByText(/the data stays where it is/i)
    await u.selectOptions(await within(dialog).findByLabelText(/place on server group/i), '2')
    await u.click(within(dialog).getByRole('checkbox', { name: /replica identity full/i }))
    await u.click(within(dialog).getByRole('button', { name: /^distribute schema$/i }))
    expect(await within(dialog).findByText('Finished.', {}, { timeout: 8000 })).toBeInTheDocument()
    expect(bodies).toEqual([{ shardGroupId: 2, setReplicaIdentityFull: true }])
    const shop = getState().databases.find((d) => d.info.name === 'shop')!
    const legacy = (
      shop as unknown as { schemas: { name: string; group: number | null }[] }
    ).schemas.find((m) => m.name === 'legacy')!
    expect(legacy.group).toBe(2)
  })

  it('offers no choice while the schema has blockers', async () => {
    const { u, section } = await openSchemas()
    const blocked = (await within(section).findByText('analytics')).closest('tr')!
    await u.click(within(blocked).getByTestId('distribute-schema'))
    const dialog = await screen.findByRole('dialog', { name: /distribute schema/i })
    await within(dialog).findByTestId('schema-blockers')
    expect(within(dialog).queryByLabelText(/place on server group/i)).not.toBeInTheDocument()
    expect(within(dialog).queryByRole('checkbox')).not.toBeInTheDocument()
  })

  it('sends an empty body when nothing is chosen', async () => {
    const bodies: unknown[] = []
    mockServer.events.on('request:start', ({ request }) => {
      if (request.method === 'PUT' && request.url.endsWith('/schemas/legacy/distribution'))
        void request
          .clone()
          .json()
          .then((b) => bodies.push(b))
    })
    const { u, section } = await openSchemas()
    const row = (await within(section).findByText('legacy')).closest('tr')!
    await u.click(within(row).getByTestId('distribute-schema'))
    const dialog = await screen.findByRole('dialog', { name: /distribute schema/i })
    await within(dialog).findByLabelText(/place on server group/i)
    await u.click(within(dialog).getByRole('button', { name: /^distribute schema$/i }))
    await within(dialog).findByText('Finished.', {}, { timeout: 8000 })
    expect(bodies).toEqual([{}])
  })
})

describe('Settings of a database: distribute new schemas follows the mode', () => {
  it('switches the box off with Rows, keeps it off in the other modes, and saves what the box says', async () => {
    const shop = getState().databases.find((d) => d.info.name === 'shop')!
    shop.info.sharding.distributeNewSchemas = true
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    await settled()
    await user.click(await screen.findByRole('button', { name: /edit settings/i }))
    const dialog = await screen.findByRole('dialog', { name: /edit shop/i })
    const box = within(within(dialog).getByTestId('edit-sharding-mode')).getByRole('checkbox', {
      name: /distribute new schemas/i,
    })
    expect(box).toBeChecked()
    await user.selectOptions(within(dialog).getByLabelText(/shard by/i), 'row')
    expect(box).not.toBeChecked()
    expect(box).toBeDisabled()
    await user.selectOptions(within(dialog).getByLabelText(/shard by/i), 'schema')
    expect(box).not.toBeChecked()
    expect(box).toBeEnabled()
    await user.selectOptions(within(dialog).getByLabelText(/shard by/i), 'both')
    await user.click(within(dialog).getByRole('button', { name: /^save changes$/i }))
    await within(dialog).findByText('Finished.', {}, { timeout: 8000 })
    expect(shop.info.sharding.mode).toBe('both')
    expect(shop.info.sharding.distributeNewSchemas).toBe(false)
  })
})

function task(extra: Partial<Task>): Task {
  const now = new Date().toISOString()
  return {
    id: 7,
    kind: 'schema.move',
    state: 'failed',
    progress: {
      percent: 40,
      currentStep: 'Copy the data',
      steps: [
        { name: 'Check the move', state: 'done' },
        { name: 'Copy the data', state: 'failed' },
      ],
    },
    error: {
      code: 'step_failed',
      message: 'the copy failed',
      retryable: true,
      step: 'Copy the data',
    },
    detail: null,
    subject: { type: 'schema', id: 'shop/tenant_1' },
    cancellable: false,
    createdAt: now,
    updatedAt: now,
    createdBy: 'admin',
    ...extra,
  } as Task
}

describe('A schema task that ends badly', () => {
  it('says that a failed move left the schema in place', async () => {
    await renderComponent(TaskProgress, { task: task({}) })
    expect(await screen.findByTestId('move-failed-note')).toHaveTextContent(
      /still on its original group/i,
    )
  })

  it('does not say it for a move that is done, or for another kind that failed', async () => {
    const done = await renderComponent(TaskProgress, {
      task: task({ state: 'done', error: null }),
    })
    expect(done.queryByTestId('move-failed-note')).not.toBeInTheDocument()
    done.unmount()
    const other = await renderComponent(TaskProgress, { task: task({ kind: 'schema.distribute' }) })
    expect(other.queryByTestId('move-failed-note')).not.toBeInTheDocument()
  })

  it('shows a pin that appeared during the task as the notice of its code, the reason as text', async () => {
    await renderComponent(TaskProgress, {
      task: task({
        error: {
          code: 'schema_pinned',
          message: 'schema "x" holds the ledger table <b>docs</b>',
          retryable: false,
          step: 'Check the move',
        },
      }),
    })
    const notice = await screen.findByText(/keeps it on its shard group/i)
    expect(notice).toBeInTheDocument()
    expect(screen.getByText(/ledger table <b>docs<\/b>/)).toBeInTheDocument()
    expect(document.querySelector('b')).toBeNull()
  })
})

describe('Shards page: distributed schemas per server group', () => {
  it('counts the distributed schemas of the database for each group, local ones not at all', async () => {
    await renderApp('/shards?db=shop')
    await settled()
    const table = await screen.findByTestId('schemas-per-group')
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent(/Group 1/)
    expect(within(rows[0]!).getAllByRole('cell')[1]).toHaveTextContent('3')
    expect(rows[1]).toHaveTextContent(/Group 2/)
    expect(within(rows[1]!).getAllByRole('cell')[1]).toHaveTextContent('1')
  })
})
