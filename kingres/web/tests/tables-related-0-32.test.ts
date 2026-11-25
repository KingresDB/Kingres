// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Distribute with related tables and the operation indicator of a table (contract 0.32.0, a placeholder revision): the mock API, the dialog
// that distributes the table together with the tables its foreign keys link to it, the badge on the list of tables, and the table page that
// shows what runs and holds the other changes.
import { beforeEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { axe } from 'vitest-axe'
import { mockServer } from '@/mocks/server'
import { endpoints } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { operationReason, operationProgress, operationShort } from '@/lib/tables'
import type { TableOperation } from '@/api/types'
import { createPinia } from 'pinia'
import { renderApp, signInAs } from './helpers'

const sw = (q: string) => applySwitches(new URLSearchParams(q), getState())
const page = (db: string, table: string) => `/databases/${db}/tables/public/${table}`
const shopTable = (name: string) =>
  getState()
    .databases.find((d) => d.info.name === 'shop')!
    .tables.find((t) => t.name === name)!
const dialogOf = async (name: RegExp) => within(await screen.findByRole('dialog', { name }))
const done = (id: number) =>
  waitFor(async () => expect((await endpoints.getTask(id)).state).toBe('done'), { timeout: 30_000 })
const enabled = async (button: HTMLElement) => waitFor(() => expect(button).toBeEnabled())

/** orders and customers are linked by a foreign key; both are plain here, so that they can be distributed together. */
function makePlain(...names: string[]): void {
  for (const name of names) {
    const t = shopTable(name)
    t.classes = ['plain']
    t.distribution = null
    t.ledger = null
    t.homeShardGroupId = 0
  }
}

const together = {
  method: 'hash' as const,
  distributionColumn: 'customer_id',
  relatedTables: [{ schema: 'public', table: 'customers', distributionColumn: 'customer_id' }],
}

describe('the mock of listRelatedTables and of the operation of a table', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
    mockConfig.taskDurationMs = 60
  })

  it('lists the tables a foreign key links to the table, with the column each gets and the keys', async () => {
    makePlain('orders', 'customers')
    const plan = await endpoints.listRelatedTables('shop', 'public', 'orders', 'customer_id')
    expect(plan.tables).toEqual([
      expect.objectContaining({
        schema: 'public',
        table: 'customers',
        distributionColumn: 'customer_id',
      }),
    ])
    expect(plan.keys).toEqual([
      expect.objectContaining({ table: 'orders', referencesTable: 'customers' }),
    ])
    expect(plan.colocateWith).toEqual({ schema: 'public', table: 'orders' })
    expect(plan.blockers).toEqual([])
  })

  it('leaves the column empty when it does not follow, and a table without links has no related tables', async () => {
    makePlain('orders', 'customers', 'notes')
    const plan = await endpoints.listRelatedTables('shop', 'public', 'orders', 'order_id')
    expect(plan.tables[0]!.distributionColumn).toBeNull()
    expect(plan.blockers.map((b) => b.code)).toEqual(['related_column_unknown'])
    expect((await endpoints.listRelatedTables('shop', 'public', 'notes')).tables).toEqual([])
  })

  it('refuses a partial list and a table that is not linked, in the preview and at the start', async () => {
    makePlain('orders', 'customers', 'notes')
    const missing = await endpoints.previewTableChange('shop', 'public', 'orders', {
      kind: 'distribute',
      distribute: {
        method: 'hash',
        distributionColumn: 'customer_id',
        relatedTables: [{ schema: 'public', table: 'notes' }],
      },
    })
    expect(missing.blockers.map((b) => b.code)).toEqual(
      expect.arrayContaining(['related_table_missing', 'related_table_unrelated']),
    )
    await expect(
      endpoints.distributeTable('shop', 'public', 'orders', {
        method: 'hash',
        distributionColumn: 'customer_id',
        relatedTables: [{ schema: 'public', table: 'notes' }],
      }),
    ).rejects.toMatchObject({ problem: { code: 'table_not_eligible' } })
  })

  it('previews the whole set: the related table, the keys, the blocking copy', async () => {
    makePlain('orders', 'customers')
    const p = await endpoints.previewTableChange('shop', 'public', 'orders', {
      kind: 'distribute',
      distribute: together,
    })
    expect(p.blockers).toEqual([])
    expect(p.affectedTables.map((t) => [t.name, t.role])).toEqual([
      ['orders', 'source'],
      ['customers', 'related'],
    ])
    expect(p.notes.map((n) => n.code)).toContain('related_key')
    expect(p.warnings.map((n) => n.code)).toContain('related_keys_not_enforced')
    const online = await endpoints.previewTableChange('shop', 'public', 'orders', {
      kind: 'distribute',
      distribute: { ...together, mode: 'online' },
    })
    expect(online.blockers.map((b) => b.code)).toContain('related_online')
  })

  it('distributes the set in one task: both tables end up in one colocation group', async () => {
    makePlain('orders', 'customers')
    mockConfig.taskDurationMs = 60
    const task = await endpoints.distributeTable('shop', 'public', 'orders', together)
    expect(task.kind).toBe('table.distribute')
    expect(task.progress.steps.map((s) => s.name)).toEqual(
      expect.arrayContaining([
        'Take the foreign keys between the tables out',
        'Distribute public.customers',
      ]),
    )
    await done(task.id)
    expect(shopTable('customers').classes).toContain('distributed')
    expect(shopTable('orders').distribution?.colocationId).toBe(
      shopTable('customers').distribution?.colocationId,
    )
  })

  it('names the operation of a table while its task runs, and of the table it takes along', async () => {
    makePlain('orders', 'customers')
    sw('tables=slow')
    const task = await endpoints.distributeTable('shop', 'public', 'orders', together)
    const orders = await endpoints.getTable('shop', 'public', 'orders')
    expect(orders.operation).toMatchObject({
      kind: 'distribute',
      role: 'subject',
      source: 'task',
      taskId: task.id,
    })
    const customers = await endpoints.getTable('shop', 'public', 'customers')
    expect(customers.operation).toMatchObject({
      kind: 'distribute',
      role: 'related',
      taskId: task.id,
    })
    const list = await endpoints.listTables('shop')
    expect(list.items.find((t) => t.name === 'notes')!.operation).toBeNull()
    expect(list.items.find((t) => t.name === 'customers')!.operation?.role).toBe('related')
  })

  it('refuses a change of a table that another change takes along', async () => {
    makePlain('orders', 'customers')
    sw('tables=slow')
    await endpoints.distributeTable('shop', 'public', 'orders', together)
    await expect(
      endpoints.alterTableDistribution('shop', 'public', 'customers', { bucketCount: 64 }),
    ).rejects.toMatchObject({ problem: { code: 'operation_in_progress' } })
  })
})

describe('the words of the operation indicator', () => {
  const op = (o: Partial<TableOperation>): TableOperation => ({
    kind: 'distribute',
    role: 'subject',
    source: 'task',
    taskId: 7,
    state: 'running',
    startedAt: null,
    percent: 41.6,
    step: null,
    ...o,
  })
  it('says what runs, how far, and which task', () => {
    expect(operationShort(op({}))).toBe('Distributing')
    expect(operationShort(op({ kind: 'teleport' }))).toBe('teleport')
    expect(operationProgress(op({}))).toBe('42%')
    expect(operationProgress(op({ state: 'pending', percent: null }))).toBe('pending')
    expect(operationReason(null)).toBe('')
    expect(operationReason(op({}))).toBe(
      'A change of this table is running (task 7): distribution.',
    )
    expect(operationReason(op({ role: 'related' }))).toMatch(/takes this table along/)
    expect(operationReason(op({ source: 'extension', taskId: null }))).toMatch(/started from SQL/)
  })
})

describe('the dialog of distribute with related tables', () => {
  async function open(user: ReturnType<typeof userEvent.setup>) {
    makePlain('orders', 'customers')
    await renderApp(page('shop', 'orders'))
    await user.click(await screen.findByRole('button', { name: /^distribute table/i }))
    const dialog = await dialogOf(/^distribute public\.orders/i)
    await user.selectOptions(
      await dialog.findByRole('combobox', { name: /distribution column/i }),
      'customer_id',
    )
    return dialog
  }

  it('lists the related tables and the keys, and sends them with the request', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 80
    const bodies: unknown[] = []
    mockServer.events.on('request:start', async ({ request }) => {
      if (request.method === 'PUT' && request.url.endsWith('/distribution'))
        bodies.push(await request.clone().json())
    })
    const dialog = await open(user)
    const list = await dialog.findByTestId('related-list')
    expect(list).toHaveTextContent('public.customers')
    expect(list).toHaveTextContent(/by\s+customer_id/)
    expect(dialog.getByTestId('related-key')).toHaveTextContent(/orders_customers_fk/)
    expect(dialog.getByTestId('related-tables')).toHaveTextContent(
      /no key between them is enforced/i,
    )
    // the online copy is not offered for tables distributed together
    expect(dialog.getByRole('radio', { name: /^online/i })).toBeDisabled()
    await dialog.findByTestId('table-change-eligible')
    const start = dialog.getByRole('button', { name: /^distribute table$/i })
    await enabled(start)
    await user.click(start)
    expect(await dialog.findByText('Finished.')).toBeInTheDocument()
    expect(bodies).toEqual([
      expect.objectContaining({
        method: 'hash',
        distributionColumn: 'customer_id',
        relatedTables: [
          { schema: 'public', table: 'customers', distributionColumn: 'customer_id' },
        ],
      }),
    ])
    expect(shopTable('customers').classes).toContain('distributed')
  })

  it('distributes the table alone when the related tables are turned off', async () => {
    const user = userEvent.setup()
    const bodies: unknown[] = []
    mockServer.events.on('request:start', async ({ request }) => {
      if (request.method === 'PUT' && request.url.endsWith('/distribution'))
        bodies.push(await request.clone().json())
    })
    const dialog = await open(user)
    await dialog.findByTestId('related-list')
    await user.click(dialog.getByTestId('related-together'))
    expect(dialog.queryByTestId('related-list')).not.toBeInTheDocument()
    await waitFor(() => expect(dialog.getByRole('radio', { name: /^online/i })).toBeEnabled())
    // the mock refuses the table alone: the foreign key to customers is a blocker the preview shows
    await dialog.findByText(/foreign key/i)
    expect(bodies).toEqual([])
  })

  it('asks for the column of a related table that does not follow from the keys', async () => {
    const user = userEvent.setup()
    makePlain('orders', 'customers')
    await renderApp(page('shop', 'orders'))
    await user.click(await screen.findByRole('button', { name: /^distribute table/i }))
    const dialog = await dialogOf(/^distribute public\.orders/i)
    await user.selectOptions(
      await dialog.findByRole('combobox', { name: /distribution column/i }),
      'order_id',
    )
    const input = await dialog.findByTestId('related-column-customers')
    // the preview names the table whose column is missing, and the start is held until the column is typed
    expect(
      await dialog.findByText(/name the distribution column of this table/i),
    ).toBeInTheDocument()
    const start = dialog.getByRole('button', { name: /^distribute table$/i })
    expect(start).toBeDisabled()
    await user.type(input, 'customer_id')
    await dialog.findByTestId('table-change-eligible')
    await enabled(start)
  })

  it('has no accessibility violations with the related tables showing', async () => {
    const user = userEvent.setup()
    const dialog = await open(user)
    await dialog.findByTestId('related-list')
    expect(await axe(document.body)).toHaveNoViolations()
  })
})

describe('the operation on the list of tables and on the table page', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
  })

  it('shows a badge on the row, holds the actions of the row, and shows the related table too', async () => {
    makePlain('orders', 'customers')
    sw('tables=slow')
    const task = await endpoints.distributeTable('shop', 'public', 'orders', together)
    await renderApp('/databases/shop')
    const badges = await screen.findAllByTestId('table-operation-badge')
    expect(badges).toHaveLength(2)
    expect(badges[0]).toHaveTextContent(/distributing/i)
    expect(badges.some((b) => /with another table/i.test(b.textContent ?? ''))).toBe(true)
    const distribute = screen.getByTestId('distribute-orders')
    expect(distribute).toHaveAttribute('aria-disabled', 'true')
    expect(distribute).toHaveTextContent(new RegExp(`running \\(task ${task.id}\\)`))
    // a table that nothing runs on keeps its actions
    expect(screen.getByTestId('distribute-notes')).not.toHaveAttribute('aria-disabled', 'true')
  })

  it('shows what runs on the table page and holds the changes, also for the table that is taken along', async () => {
    makePlain('orders', 'customers')
    sw('tables=slow')
    await endpoints.distributeTable('shop', 'public', 'orders', together)
    await renderApp(page('shop', 'customers'))
    const running = await screen.findByTestId('table-operation')
    expect(running).toHaveTextContent(/distributing/i)
    expect(running).toHaveTextContent(/with another table/i)
    const alter = await screen.findByRole('button', { name: /^distribute table/i })
    await waitFor(() => expect(alter).toHaveAttribute('aria-disabled', 'true'))
    expect(alter).toHaveTextContent(/takes this table along/i)
    expect(screen.getByRole('button', { name: /^convert storage/i })).toHaveAttribute(
      'aria-disabled',
      'true',
    )
  })

  it('shows nothing when no operation runs', async () => {
    await renderApp(page('shop', 'notes'))
    await screen.findByTestId('table-home')
    expect(screen.queryByTestId('table-operation')).not.toBeInTheDocument()
  })

  it('has no accessibility violations with an operation showing', async () => {
    makePlain('orders', 'customers')
    sw('tables=slow')
    await endpoints.distributeTable('shop', 'public', 'orders', together)
    await renderApp('/databases/shop')
    await screen.findAllByTestId('table-operation-badge')
    expect(await axe(document.body)).toHaveNoViolations()
  })
})
