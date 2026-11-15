// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.4.0: `QueryResult.truncated` is also true when the 16 MiB byte budget is reached, and a
// single value larger than 1 MiB makes the statement fail with 422 `unprocessable`.
import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { renderApp } from './helpers'

async function runStatement() {
  const user = userEvent.setup()
  await renderApp('/workbench')
  await screen.findByRole('option', { name: 'shop' })
  await user.click(screen.getByRole('button', { name: /^run$/i }))
}

describe('workbench result limits', () => {
  it('says a truncated result was cut at the row limit or the size limit', async () => {
    mockServer.use(
      http.post('*/api/v1/query', () =>
        HttpResponse.json({
          columns: [{ name: 'a', type: 'text' }],
          rows: [['x']],
          rowCount: 1,
          truncated: true,
          commandTag: 'SELECT 1',
          durationMs: 1,
        }),
      ),
    )
    await runStatement()
    const badge = await screen.findByText(/cut at the row limit or at the/i)
    expect(badge.textContent).toMatch(/16 MiB/)
  })

  it('shows the explanation of a 422 for a value over 1 MiB', async () => {
    mockServer.use(
      http.post('*/api/v1/query', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Unprocessable',
            status: 422,
            code: 'unprocessable',
            detail: 'A value of column "doc" is larger than 1 MiB.',
          },
          { status: 422 },
        ),
      ),
    )
    await runStatement()
    expect(await screen.findByText(/larger than 1 MiB/)).toBeInTheDocument()
    expect(screen.getByText('Cannot be done')).toBeInTheDocument()
  })
})
