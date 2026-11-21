// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.15.0 on the Shards page and the Rebalance page (design 14.2): only the hash colocation groups are asked for,
// the distributed schemas are summed per server group (a size the server does not know is "n/a", never 0), the moves of
// schemas that run now are listed with the schema name.
import { afterEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import { endpoints } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { beginSchemaMove } from '@/mocks/schemas'
import { mockServer } from '@/mocks/server'
import { mockConfig } from '@/mocks/state'
import { renderApp, settled } from './helpers'

afterEach(() => mockServer.events.removeAllListeners())

const tenants = () => getState().databases.find((d) => d.info.name === 'tenants')!
const schemaRec = (name: string) => tenants().schemas!.find((s) => s.name === name)!

describe('Shards page', () => {
  it('asks only for the colocation groups of kind hash', async () => {
    const seen: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      const url = new URL(request.url)
      if (url.pathname.endsWith('/colocation-groups')) seen.push(url.searchParams.get('kind') ?? '')
    })
    await renderApp('/shards?db=shop')
    await settled()
    await screen.findByRole('combobox', { name: /colocation group/i })
    expect(seen.length).toBeGreaterThan(0)
    expect(new Set(seen)).toEqual(new Set(['hash']))
  })

  it('does not wait for a bucket map for a database that has only distributed schemas, and says so', async () => {
    await renderApp('/shards?db=tenants')
    await settled()
    expect(
      await screen.findByText(/no hash-distributed tables, so there is no bucket map/i),
    ).toBeInTheDocument()
    expect(document.querySelector('.skeleton')).toBeNull()
  })

  it('sums the distributed schemas per server group: a count and a size, with n/a for a size nobody knows', async () => {
    await renderApp('/shards?db=tenants')
    const table = await screen.findByRole('table', {
      name: /distributed schemas, their number and size/i,
    })
    const row = (name: string) =>
      within(table).getByRole('rowheader', { name }).closest('tr') as HTMLElement
    // Group 1: tenant_acme (52 MiB) and ledgered (1 MiB).
    expect(row('Group 1')).toHaveTextContent('10.0.0.11')
    expect(within(row('Group 1')).getAllByRole('cell')[1]).toHaveTextContent('2')
    expect(within(row('Group 1')).getAllByRole('cell')[2]).toHaveTextContent(/53(\.0)? MiB/)
    // Group 2: failme (9 MiB) and tenant_globex, whose size is not known here.
    expect(within(row('Group 2')).getAllByRole('cell')[1]).toHaveTextContent('2')
    expect(within(row('Group 2')).getAllByRole('cell')[2]).toHaveTextContent(/9(\.0)? MiB/)
    expect(within(row('Group 2')).getAllByRole('cell')[2]).toHaveTextContent('(and 1 not known)')
    expect(within(row('Group 3')).getAllByRole('cell')[2]).toHaveTextContent(/3(\.0)? MiB/)
  })

  it('shows a group whose schemas have no known size as n/a, never as 0', async () => {
    schemaRec('tenant_initech').sizeBytes = null
    await renderApp('/shards?db=tenants')
    const table = await screen.findByRole('table', {
      name: /distributed schemas, their number and size/i,
    })
    const size = within(
      within(table).getByRole('rowheader', { name: 'Group 3' }).closest('tr') as HTMLElement,
    ).getAllByRole('cell')[2] as HTMLElement
    expect(within(size).getByTestId('na')).toBeInTheDocument()
    expect(size).not.toHaveTextContent(/\b0\b/)
  })

  it('counts the schemas of each server among its primary buckets', async () => {
    await renderApp('/shards?db=tenants')
    const table = await screen.findByRole('table', {
      name: /primary and replica buckets per server/i,
    })
    await waitFor(() => expect(within(table).getAllByRole('row').length).toBe(4))
    const rows = within(table).getAllByRole('row').slice(1)
    // After the server (the row header): primary buckets, of them schemas.
    expect(
      rows.map((r) => [0, 1].map((i) => within(r).getAllByRole('cell')[i]?.textContent)),
    ).toEqual([
      ['2', '2'],
      ['2', '2'],
      ['1', '1'],
    ])
  })

  it('has no schema section for a database that distributes none', async () => {
    await renderApp('/shards?db=shop')
    await settled()
    await screen.findByRole('table', { name: /primary and replica buckets per server/i })
    expect(
      screen.queryByRole('table', { name: /distributed schemas, their number and size/i }),
    ).toBeNull()
    expect(screen.queryByText(/of them schemas/i)).toBeNull()
  })

  it('lists the schema moves that run now, with the schema name', async () => {
    mockConfig.taskDurationMs = 20_000
    beginSchemaMove(getState(), tenants(), schemaRec('tenant_acme'), 3, false)
    await renderApp('/shards?db=tenants')
    const table = await screen.findByRole('table', {
      name: /moves of distributed schemas that run now/i,
    })
    const row = within(table)
      .getByRole('rowheader', { name: 'tenant_acme' })
      .closest('tr') as HTMLElement
    expect(row).toHaveTextContent('Group 1')
    expect(row).toHaveTextContent('Group 3')
  })
})

describe('Rebalance page', () => {
  it('shows the move of a schema among the moves in flight, told by the name of the schema', async () => {
    mockConfig.taskDurationMs = 20_000
    await renderApp('/rebalance')
    await endpoints.moveSchema('tenants', 'tenant_acme', { targetShardGroupId: 2 })
    const inFlight = await screen.findByRole(
      'table',
      { name: /moves in flight/i },
      { timeout: 10_000 },
    )
    const cell = within(inFlight).getByTestId('schema-move')
    expect(cell).toHaveTextContent('Schema tenant_acme')
    expect(within(cell.closest('tr') as HTMLElement).getByText('tenants')).toBeInTheDocument()
  })

  it('does not tell schema moves in its plan: the contract has no schema unit in a plan preview', async () => {
    // PlannedMove has no kind and no schema name in 0.15.0, so a plan lists buckets only (the bucket column is a number).
    const plan = await (async () => {
      await renderApp('/dashboard')
      return endpoints.previewRebalancePlan({})
    })()
    for (const move of plan.moves) expect(move).not.toHaveProperty('schemaName')
  })
})
