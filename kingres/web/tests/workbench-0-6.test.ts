// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.6.0, executeQuery: a result cut at the limit of a statement that changes data means the statement
// WAS committed; a statement that ran into `timeoutMs` is a 422 `sql_error` with SQLSTATE 57014; a value over
// 1 MiB is 422 `unprocessable`; a third concurrent statement of a role answers 503 `service_unavailable`.
import { describe, expect, it } from 'vitest'
import { fireEvent, screen } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { renderApp } from './helpers'

async function run(sql: string) {
  const user = userEvent.setup()
  await renderApp('/workbench')
  await screen.findByRole('option', { name: 'shop' })
  const box = screen.getByLabelText(/^SQL/)
  await fireEvent.update(box, sql)
  await user.click(screen.getByRole('button', { name: /^run$/i }))
}

describe('a result cut at the limit', () => {
  it('says prominently that a truncated UPDATE was still applied', async () => {
    await run('update orders set seen = true returning bulk')
    const note = await screen.findByTestId('truncated-applied')
    expect(note).toHaveTextContent(/the statement was applied/i)
    expect(note).toHaveTextContent(/UPDATE 5000/)
    expect(note).toHaveTextContent(/do not run it again/i)
    expect(note.getAttribute('role')).toBe('status')
    // Inside the live region of the results.
    expect(note.closest('[aria-live="polite"]')).not.toBeNull()
    expect(screen.getByText(/the statement was still applied/i)).toBeInTheDocument()
  })

  it('shows the real tag of a truncated INSERT', async () => {
    await run('insert into t select 1 returning bulk')
    expect(await screen.findByTestId('truncated-applied')).toHaveTextContent(/INSERT 0 5000/)
  })

  it('says a cut SELECT only read data', async () => {
    const user = userEvent.setup()
    await renderApp('/workbench')
    await screen.findByRole('option', { name: 'shop' })
    await fireEvent.update(screen.getByLabelText(/Row limit/), '2')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    await screen.findByText(/cut at the row limit or at the 16/i)
    expect(screen.getByText(/the statement only read data/i)).toBeInTheDocument()
    expect(screen.queryByTestId('truncated-applied')).toBeNull()
  })

  it('the mode notice already says a cut does not stop a data change', async () => {
    await renderApp('/workbench')
    expect(await screen.findByTestId('mode')).toHaveTextContent(
      /does not stop a statement that changes data/i,
    )
  })
})

describe('errors of a statement', () => {
  it('explains the time limit as a cancelled statement, not a server fault', async () => {
    await run('select pg_sleep(100)')
    const hint = await screen.findByTestId('query-error-hint')
    expect(hint).toHaveTextContent(/cancelled because it ran longer than the time limit/i)
    expect(hint).toHaveTextContent(/not a fault of the server/i)
    expect(screen.queryByText(/service unavailable/i)).toBeNull()
  })

  it('explains a value over 1 MiB or a row over 16 MiB', async () => {
    await run('select hugevalue from t')
    expect(await screen.findByTestId('query-error-hint')).toHaveTextContent(
      /value above 1 MiB, or a row above 16 MiB/i,
    )
  })

  it('explains the limit of two statements per role', async () => {
    await run('select busyrole from t')
    expect(await screen.findByTestId('query-error-hint')).toHaveTextContent(
      /at most two statements at a time/i,
    )
  })
})
