// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
// The console of contract 0.16.0: strategies, weights, drain, automatic mode and split (phase 10) and the schemas of a
// database (phase 11), against the mock API.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { renderApp, settled } from './helpers'

describe('Rebalance page: strategy, threshold and the automatic mode', () => {
  it('previews by disk size with the threshold and says why each move is made', async () => {
    const user = userEvent.setup()
    await renderApp('/rebalance')
    await settled()
    await user.selectOptions(await screen.findByLabelText('Strategy'), 'byDiskSize')
    await user.click(screen.getByRole('button', { name: /preview plan/i }))
    const plan = await screen.findByTestId('plan')
    expect(within(plan).getByTestId('plan-strategy')).toHaveTextContent(/by disk size/i)
    expect(within(plan).getByTestId('plan-strategy')).toHaveTextContent(/threshold 10 percent/i)
  })

  it('shows the automatic card off by default and lets an administrator turn it on', async () => {
    const user = userEvent.setup()
    await renderApp('/rebalance')
    await settled()
    const card = await screen.findByTestId('automatic-card')
    expect(within(card).getByTestId('auto-state')).toHaveTextContent('Off')
    expect(within(card).getByText(/it is off by default/i)).toBeInTheDocument()
    await user.click(within(card).getByTestId('auto-toggle'))
    await waitFor(() => expect(within(card).getByTestId('auto-state')).toHaveTextContent('On'))
    expect(getState().settings.rebalance.automatic?.enabled).toBe(true)
    expect(await within(card).findByTestId('auto-last')).toHaveTextContent(/too young/i)
  })

  it('keeps the buttons of the automatic mode disabled for a read-only role', async () => {
    await renderApp('/rebalance', { user: 'viewer' })
    await settled()
    const card = await screen.findByTestId('automatic-card')
    expect(within(card).getByTestId('auto-toggle')).toHaveAttribute('aria-disabled', 'true')
    expect(within(card).getByTestId('auto-edit')).toHaveAttribute('aria-disabled', 'true')
  })
})

describe('Servers page: weight, drain and return to service', () => {
  it('changes the weight of a server', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    await settled()
    const [first] = await screen.findAllByTestId('server-weight-edit')
    await user.click(first!)
    const dialog = await screen.findByRole('dialog', { name: /weight of server/i })
    const input = within(dialog).getByLabelText(/^weight/i)
    await user.clear(input)
    await user.type(input, '2.5')
    await user.click(within(dialog).getByRole('button', { name: /save weight/i }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(getState().servers[0]!.weight).toBe(2.5)
    await waitFor(() => expect(screen.getAllByTestId('server-weight')[0]).toHaveTextContent('2.5'))
  })

  it('refuses a weight of zero before sending it', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    await settled()
    await user.click((await screen.findAllByTestId('server-weight-edit'))[0]!)
    const dialog = await screen.findByRole('dialog', { name: /weight of server/i })
    const input = within(dialog).getByLabelText(/^weight/i)
    await user.clear(input)
    await user.type(input, '0')
    await user.click(within(dialog).getByRole('button', { name: /save weight/i }))
    expect(await within(dialog).findByText(/above 0 and at most 1000/i)).toBeInTheDocument()
  })

  it('previews a drain, starts it and shows the task', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    await settled()
    await user.click((await screen.findAllByTestId('drain-server'))[0]!)
    const dialog = await screen.findByRole('dialog', { name: /drain server/i })
    const preview = await within(dialog).findByTestId('drain-preview')
    expect(preview).toHaveTextContent(/buckets will move/i)
    expect(within(dialog).getByTestId('drain-standbys')).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: /^drain server$/i }))
    expect(
      await within(dialog).findByRole('progressbar', { name: /task progress/i }),
    ).toBeInTheDocument()
    expect(getState().tasks[0]!.task.kind).toBe('rebalance')
  })

  it('gives a drained server back to service', async () => {
    const user = userEvent.setup()
    getState().servers[1]!.shouldHaveShards = false
    await renderApp('/servers')
    await settled()
    await user.click(await screen.findByTestId('return-to-service'))
    await waitFor(() => expect(getState().servers[1]!.shouldHaveShards).toBe(true))
  })

  it('does not offer the drain to a read-only role', async () => {
    await renderApp('/servers', { user: 'viewer' })
    await settled()
    const [btn] = await screen.findAllByTestId('drain-server')
    expect(btn).toHaveAttribute('aria-disabled', 'true')
  })
})

describe('Shards page: split a bucket', () => {
  it('previews the new buckets and splits', async () => {
    const user = userEvent.setup()
    await renderApp('/shards')
    await settled()
    const lookup = await screen.findByLabelText(/find bucket number/i)
    await user.type(lookup, '3')
    await user.click(screen.getByRole('button', { name: /^find$/i }))
    await user.click(await screen.findByTestId('split-bucket'))
    const dialog = await screen.findByRole('dialog', { name: /split bucket 3/i })
    const table = await within(dialog).findByTestId('split-preview')
    expect(within(table).getAllByRole('row')).toHaveLength(3) // header and two children
    expect(within(dialog).getByText(/cannot be undone/i)).toBeInTheDocument()
    await user.selectOptions(within(dialog).getByLabelText(/number of new buckets/i), '4')
    await waitFor(() =>
      expect(within(within(dialog).getByTestId('split-preview')).getAllByRole('row')).toHaveLength(
        5,
      ),
    )
    await user.click(within(dialog).getByRole('button', { name: /^split bucket$/i }))
    expect(
      await within(dialog).findByRole('progressbar', { name: /task progress/i }),
    ).toBeInTheDocument()
    expect(getState().tasks[0]!.task.kind).toBe('bucket.split')
  })
})

async function openSchemas(user = 'admin') {
  const u = userEvent.setup()
  await renderApp('/databases/shop', { user })
  await settled()
  const section = await screen.findByTestId('schemas-section')
  return { u, section }
}

describe('Schemas of a database', () => {
  it('lists local and distributed schemas, a moving one, and a hostile name as text', async () => {
    const { section } = await openSchemas()
    const rows = await within(section).findAllByTestId('schema-row')
    expect(rows.length).toBeGreaterThanOrEqual(8)
    expect(within(section).getAllByText('Distributed').length).toBeGreaterThan(0)
    expect(within(section).getAllByText('Local').length).toBeGreaterThan(0)
    expect(within(section).getByText(/<img src=x onerror=alert\(1\)>/)).toBeInTheDocument()
    expect(section.querySelector('img')).toBeNull()
  })

  it('lists the blockers of a schema and keeps the button disabled', async () => {
    const { u, section } = await openSchemas()
    const row = (await within(section).findByText('analytics')).closest('tr')!
    await u.click(within(row).getByTestId('distribute-schema'))
    const dialog = await screen.findByRole('dialog', { name: /distribute schema/i })
    const blockers = await within(dialog).findByTestId('schema-blockers')
    expect(blockers).toHaveTextContent(/materialized view/i)
    expect(within(dialog).getByRole('button', { name: /^distribute schema$/i })).toBeDisabled()
    expect(within(dialog).getByTestId('schema-why')).toBeInTheDocument()
  })

  it('distributes a schema and follows the task', async () => {
    const { u, section } = await openSchemas()
    const row = (await within(section).findByText('legacy')).closest('tr')!
    await u.click(within(row).getByTestId('distribute-schema'))
    const dialog = await screen.findByRole('dialog', { name: /distribute schema/i })
    await within(dialog).findByText(/the data stays where it is/i)
    await u.click(within(dialog).getByRole('button', { name: /^distribute schema$/i }))
    expect(await within(dialog).findByText('Finished.', {}, { timeout: 8000 })).toBeInTheDocument()
  })

  it('shows the failed step of a schema that fails', async () => {
    const { u, section } = await openSchemas()
    const row = (await within(section).findByText('failme')).closest('tr')!
    await u.click(within(row).getByTestId('distribute-schema'))
    const dialog = await screen.findByRole('dialog', { name: /distribute schema/i })
    await within(dialog).findByText(/the data stays where it is/i)
    await u.click(within(dialog).getByRole('button', { name: /^distribute schema$/i }))
    expect(
      await within(dialog).findByText(/does not exist on a server/i, {}, { timeout: 8000 }),
    ).toBeInTheDocument()
  })

  it('needs REPLICA IDENTITY FULL for a table without a key before a move', async () => {
    const { u, section } = await openSchemas()
    const row = (await within(section).findByText('nokey')).closest('tr')!
    await u.click(within(row).getByTestId('move-schema'))
    const dialog = await screen.findByRole('dialog', { name: /move schema/i })
    expect(await within(dialog).findByTestId('schema-keyless')).toHaveTextContent(/events/)
    await u.selectOptions(within(dialog).getByLabelText(/target shard group/i), '2')
    const go = within(dialog).getByRole('button', { name: /^move schema$/i })
    expect(go).toBeDisabled()
    await u.click(within(dialog).getByRole('checkbox', { name: /replica identity full/i }))
    expect(go).toBeEnabled()
  })

  it('says that a pinned schema cannot move', async () => {
    const { u, section } = await openSchemas()
    const row = (await within(section).findByText('ledgerdocs')).closest('tr')!
    await u.click(within(row).getByTestId('move-schema'))
    const dialog = await screen.findByRole('dialog', { name: /move schema/i })
    expect(await within(dialog).findByTestId('schema-pinned')).toHaveTextContent(/ledger/i)
    expect(within(dialog).getByRole('button', { name: /^move schema$/i })).toBeDisabled()
  })

  it('asks for the name before a schema becomes local', async () => {
    const { u, section } = await openSchemas()
    const row = (await within(section).findByText('tenant_2')).closest('tr')!
    await u.click(within(row).getByTestId('make-local'))
    const dialog = await screen.findByRole('dialog', { name: /make schema local/i })
    const go = within(dialog).getByRole('button', { name: /^make schema local$/i })
    expect(go).toBeDisabled()
    await u.type(within(dialog).getByRole('textbox'), 'tenant_2')
    await waitFor(() => expect(go).toBeEnabled())
  })

  it('shows the buttons disabled with the role they need to a read-only role', async () => {
    const { section } = await openSchemas('viewer')
    const [btn] = await within(section).findAllByTestId('distribute-schema')
    expect(btn).toHaveAttribute('aria-disabled', 'true')
  })
})

describe('Create database: the sharding mode', () => {
  it('offers rows, schemas and both, and sends the mode', async () => {
    const user = userEvent.setup()
    await renderApp('/databases')
    await settled()
    await user.click(await screen.findByRole('button', { name: /create database/i }))
    const dialog = await screen.findByRole('dialog', { name: /create database/i })
    await user.type(within(dialog).getByLabelText(/^name/i), 'tenants')
    await user.click(within(dialog).getByRole('checkbox', { name: /spread this database/i }))
    const mode = within(dialog).getByTestId('sharding-mode')
    expect(within(mode).getByRole('radio', { name: /rows/i })).toBeChecked()
    await user.click(within(mode).getByRole('radio', { name: /schemas/i }))
    expect(within(dialog).queryByLabelText(/bucket count/i)).not.toBeInTheDocument()
    // Schemas: the box is checked by itself (design 10); Both: unchecked; back to Schemas: checked again
    const box = () => within(dialog).getByTestId('distribute-new').querySelector('input')!
    expect(box()).toBeChecked()
    await user.click(within(mode).getByRole('radio', { name: /both/i }))
    expect(box()).not.toBeChecked()
    await user.click(within(mode).getByRole('radio', { name: /schemas/i }))
    expect(box()).toBeChecked()
    await user.click(within(dialog).getByRole('button', { name: /^create database$/i }))
    await within(dialog).findByText('Finished.', {}, { timeout: 8000 })
    const created = getState().databases.find((d) => d.info.name === 'tenants')!
    expect(created.info.sharding.mode).toBe('schema')
    expect(created.info.sharding.distributeNewSchemas).toBe(true)
  })
})

describe('Rebalance page: units that are not buckets', () => {
  it('shows a schema move and a split in the list of moves in flight, the schema name as text', async () => {
    const move = (kind: string, bucket: number, schemaName: string | null) => ({
      databaseName: 'shop',
      colocationId: 7,
      bucket,
      fromShardGroupId: 1,
      toShardGroupId: 2,
      kind,
      schemaName,
      phase: 'copy',
      percent: 40,
      sizeBytes: 1000,
    })
    mockServer.use(
      http.get('*/api/v1/rebalance', () =>
        HttpResponse.json({
          state: 'running',
          needed: false,
          reasons: [],
          taskId: 5,
          startedAt: new Date().toISOString(),
          options: null,
          progress: {
            movesTotal: 2,
            movesDone: 0,
            movesInFlight: 2,
            movesFailed: 0,
            movesSkipped: 0,
            bytesMoved: 0,
            percent: 10,
          },
          inFlightMoves: [move('schema', 0, '<b>tenant</b>'), move('split', 17, null)],
          stoppedBy: null,
          pausedReason: null,
          lastReport: null,
        }),
      ),
    )
    await renderApp('/rebalance')
    await settled()
    const cell = await screen.findByText('<b>tenant</b>')
    expect(cell.closest('td')).toHaveTextContent(/^Schema /)
    expect(cell.closest('td')?.querySelector('b')).toBeNull()
    expect(await screen.findByText(/splitting/)).toBeInTheDocument()
  })
})
