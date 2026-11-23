// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Export CSV and Import CSV in the header of the table page of phase 9 (increment 9.7): the same component as the tables
// list, with the same gates (role, the cluster setting, an unfinished task on the table) and the same dialogs.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { createTask } from '@/mocks/state'
import { renderApp } from './helpers'

const PATH = '/databases/shop/tables/public/notes'

async function header(user?: string) {
  await renderApp(PATH, user ? { user } : {})
  return screen.findByTestId('export-csv')
}

describe('the table page header', () => {
  it('has Export CSV and Import CSV beside the actions of the table, and opens the export dialog for this table', async () => {
    const user = userEvent.setup()
    const exportButton = await header()
    expect(screen.getByTestId('import-csv')).toBeInTheDocument()
    expect(exportButton).not.toHaveClass('btn-small')
    await user.click(exportButton)
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent(/export public\.notes as csv/i)
    expect(within(dialog).getByTestId('export-consistency-before')).toHaveAttribute(
      'data-kind',
      'local',
    )
  })

  it('opens the import dialog for this table', async () => {
    const user = userEvent.setup()
    await header()
    await user.click(screen.getByTestId('import-csv'))
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent(/import csv into public\.notes/i)
  })

  it('is disabled with the reason for a role below administrator and while data transfer is off', async () => {
    const viewer = await header('viewer')
    expect(viewer).toHaveAttribute('aria-disabled', 'true')
    expect(viewer).toHaveAttribute('title', expect.stringMatching(/kingres_admin/))
  })

  it('is disabled while data transfer is off', async () => {
    applySwitches(new URLSearchParams('transfer=off'), getState())
    const button = await header()
    await waitFor(() => expect(button).toHaveAttribute('aria-disabled', 'true'))
    expect(button).toHaveAttribute('title', expect.stringMatching(/turned off/i))
  })

  it('is disabled while the table has an unfinished task, and enabled again after it', async () => {
    const button = await header()
    createTask(getState(), {
      kind: 'table.alter',
      subject: { type: 'table', id: 'shop/public.notes' },
      steps: ['a', 'b'],
      durationMs: 600,
      apply: () => undefined,
    })
    await waitFor(() => expect(button).toHaveAttribute('aria-disabled', 'true'), { timeout: 15000 })
    await waitFor(() => expect(button).not.toHaveAttribute('aria-disabled'), { timeout: 20000 })
  })
})
