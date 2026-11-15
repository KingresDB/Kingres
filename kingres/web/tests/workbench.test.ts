// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-08 and W-11: the workbench tells NULL, the text NULL and the empty string apart, does not keep
// a result under a statement or database it does not belong to, can stop waiting, lets the person
// choose a timeout, says that it can change data, and does not freeze on a large result.
import { describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse, delay } from 'msw'
import { mockServer } from '@/mocks/server'
import { renderApp } from './helpers'

interface Reply {
  columns: { name: string; type: string }[]
  rows: unknown[][]
  truncated?: boolean
}
const reply = (r: Reply) =>
  HttpResponse.json({
    columns: r.columns,
    rows: r.rows,
    rowCount: r.rows.length,
    truncated: r.truncated ?? false,
    commandTag: `SELECT ${r.rows.length}`,
    durationMs: 1.5,
  })

function recordQueries(): Record<string, unknown>[] {
  const seen: Record<string, unknown>[] = []
  mockServer.events.on('request:start', async ({ request }) => {
    if (request.method === 'POST' && request.url.endsWith('/api/v1/query')) {
      seen.push((await request.clone().json()) as Record<string, unknown>)
    }
  })
  return seen
}

async function open() {
  const user = userEvent.setup()
  const app = await renderApp('/workbench')
  await screen.findByRole('option', { name: 'shop' })
  return { user, ...app }
}
const run = (user: ReturnType<typeof userEvent.setup>) =>
  user.click(screen.getByRole('button', { name: /^run$/i }))

describe('cells (W-08)', () => {
  it('tells SQL NULL, the text NULL and the empty string apart without colour', async () => {
    mockServer.use(
      http.post('*/api/v1/query', () =>
        reply({
          columns: [
            { name: 'a', type: 'text' },
            { name: 'b', type: 'text' },
            { name: 'c', type: 'text' },
            { name: 'd', type: 'text' },
          ],
          rows: [[null, 'NULL', '', 'plain']],
        }),
      ),
    )
    const { user } = await open()
    await run(user)
    const result = await screen.findByTestId('result')
    const cells = within(result).getAllByRole('cell')
    expect(cells[0]).toHaveTextContent(/null value/i)
    expect(cells[0]).not.toHaveTextContent('"NULL"')
    expect(cells[1]).toHaveTextContent('"NULL"')
    expect(cells[1]).not.toHaveTextContent(/null value/i)
    expect(cells[2]).toHaveTextContent(/empty string/i)
    expect(cells[2]).toHaveTextContent('""')
    expect(cells[3]?.textContent?.trim()).toBe('plain')
  })

  it('shows duplicate column names, each once, without Vue key warnings', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined)
    mockServer.use(
      http.post('*/api/v1/query', () =>
        reply({
          columns: [
            { name: 'a', type: 'int4' },
            { name: 'a', type: 'text' },
          ],
          rows: [[1, 'x']],
        }),
      ),
    )
    const { user } = await open()
    await run(user)
    const result = await screen.findByTestId('result')
    expect(within(result).getAllByRole('columnheader')).toHaveLength(2)
    expect(warn.mock.calls.flat().join(' ')).not.toMatch(/duplicate keys/i)
    warn.mockRestore()
  })
})

describe('results that no longer belong (W-11a)', () => {
  it('clears the result when the database changes, and marks it stale when the statement changes', async () => {
    const { user } = await open()
    await run(user)
    await screen.findByTestId('result')
    const sql = screen.getByLabelText(/^sql/i)
    await user.type(sql, ' where 1 = 1')
    expect(screen.getByTestId('result')).toHaveAttribute('data-stale', 'true')
    expect(screen.getByText(/does not match the statement now in the editor/i)).toBeInTheDocument()

    await run(user)
    await waitFor(() => expect(screen.getByTestId('result')).toHaveAttribute('data-stale', 'false'))
    await user.selectOptions(screen.getByLabelText('Database'), 'reports')
    expect(screen.queryByTestId('result')).not.toBeInTheDocument()
  })
})

describe('running (W-11b, W-11c, W-11f)', () => {
  it('sends the chosen timeout, and refuses an empty row limit', async () => {
    const seen = recordQueries()
    const { user } = await open()
    const timeout = screen.getByLabelText(/timeout/i)
    await user.clear(timeout)
    await user.type(timeout, '5')
    await run(user)
    await waitFor(() => expect(seen).toHaveLength(1))
    expect(seen[0]).toMatchObject({ timeoutMs: 5000, rowLimit: 1000 })

    const limit = screen.getByLabelText(/row limit/i)
    await user.clear(limit)
    expect(screen.getByRole('button', { name: /^run$/i })).toHaveAttribute('aria-disabled', 'true')
    expect(screen.getByText(/whole number between 1 and 10,?000/i)).toBeInTheDocument()
  })

  it('does not run when Enter is pressed in the row limit or timeout field', async () => {
    const seen = recordQueries()
    const { user } = await open()
    await user.type(screen.getByLabelText(/row limit/i), '{Enter}')
    await user.type(screen.getByLabelText(/timeout/i), '{Enter}')
    await new Promise((r) => setTimeout(r, 100))
    expect(seen).toHaveLength(0)
  })

  it('runs with Ctrl+Enter in the editor', async () => {
    const seen = recordQueries()
    const { user } = await open()
    await user.click(screen.getByLabelText(/^sql/i))
    await user.keyboard('{Control>}{Enter}{/Control}')
    await waitFor(() => expect(seen).toHaveLength(1))
  })

  it('says that statements run for real, as the signed-in role', async () => {
    await open()
    expect(screen.getByText(/mode: read-write/i)).toBeInTheDocument()
    expect(screen.getByText(/insert, update, delete and ddl/i)).toBeInTheDocument()
  })

  it('can stop waiting, says the statement may still run, and ignores a late answer', async () => {
    mockServer.use(
      http.post('*/api/v1/query', async () => {
        await delay(400)
        return reply({ columns: [{ name: 'late', type: 'text' }], rows: [['x']] })
      }),
    )
    const { user } = await open()
    await run(user)
    expect(screen.getByLabelText(/^sql/i)).toHaveAttribute('readonly')
    await user.click(await screen.findByRole('button', { name: /stop waiting/i }))
    expect(await screen.findByText(/may still be running on the server/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^run$/i })).toBeEnabled()
    await delay(500)
    expect(screen.queryByTestId('result')).not.toBeInTheDocument()
  })
})

describe('large results (W-11d)', () => {
  it('renders a part of the rows and says how many are not shown', async () => {
    const rows = Array.from({ length: 5000 }, (_, i) => [i, `row ${i}`])
    mockServer.use(
      http.post('*/api/v1/query', () =>
        reply({
          columns: [
            { name: 'id', type: 'int4' },
            { name: 'label', type: 'text' },
          ],
          rows,
        }),
      ),
    )
    const { user } = await open()
    await run(user)
    const result = await screen.findByTestId('result')
    expect(within(result).getAllByRole('row').length).toBeLessThanOrEqual(501)
    expect(within(result).getByText(/showing the first 500 of 5,000 rows/i)).toBeInTheDocument()
    await user.click(within(result).getByRole('button', { name: /show 500 more rows/i }))
    expect(within(result).getAllByRole('row').length).toBeLessThanOrEqual(1001)
    expect(within(result).getAllByRole('row').length).toBeGreaterThan(900)
  })

  it('shows a cell of 100000 characters in part, with a way to see all of it', async () => {
    mockServer.use(
      http.post('*/api/v1/query', () =>
        reply({ columns: [{ name: 'big', type: 'text' }], rows: [['y'.repeat(100000)]] }),
      ),
    )
    const { user } = await open()
    await run(user)
    const result = await screen.findByTestId('result')
    const cell = within(result).getAllByRole('cell')[0] as HTMLElement
    expect((cell.textContent ?? '').length).toBeLessThan(2000)
    expect(within(cell).getByRole('button', { name: /show all 100000 characters/i })).toBeVisible()
    await fireEvent.click(within(cell).getByRole('button', { name: /show all/i }))
    expect((cell.textContent ?? '').length).toBeGreaterThan(100000)
  })
})
