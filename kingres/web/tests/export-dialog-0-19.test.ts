// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The Export CSV dialog (console increment CN3): the sentence about what the file guarantees for the kind of table before
// the person confirms, the spreadsheet text, the options and their checks, the progress as a task, the result with size,
// rows, SHA-256 and the download, the refusals in words, and the gates (role, setting, unfinished task on the table).
import { webcrypto } from 'node:crypto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { createTask } from '@/mocks/state'
import { renderApp } from './helpers'

const sw = (query: string) => applySwitches(new URLSearchParams(query), getState())

const saved: number[] = []
beforeEach(() => {
  saved.length = 0
  vi.stubGlobal('crypto', webcrypto)
  URL.createObjectURL = vi.fn((blob: Blob | MediaSource) => {
    saved.push((blob as Blob).size)
    return 'blob:mock'
  })
  URL.revokeObjectURL = vi.fn()
})
afterEach(() => vi.unstubAllGlobals())

async function rowOf(table: string): Promise<HTMLElement> {
  const cell = await screen.findByText(`public.${table}`)
  return cell.closest('tr') as HTMLElement
}

async function openExport(table: string, user = userEvent.setup()) {
  const row = await rowOf(table)
  await user.click(within(row).getByRole('button', { name: /export csv/i }))
  return screen.findByRole('dialog')
}

describe('before the export', () => {
  it('says that a local table is read as one snapshot, a reference table too, and a distributed table per group', async () => {
    await renderApp('/databases/shop')
    const dialog = await openExport('notes')
    expect(within(dialog).getByTestId('export-consistency-before')).toHaveTextContent(
      /read as one snapshot/i,
    )
    expect(within(dialog).getByTestId('export-consistency-before')).toHaveAttribute(
      'data-kind',
      'local',
    )
    await userEvent.click(within(dialog).getByRole('button', { name: /^cancel$/i }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    const ref = await openExport('countries')
    expect(within(ref).getByTestId('export-consistency-before')).toHaveAttribute(
      'data-kind',
      'reference',
    )
    await userEvent.click(within(ref).getByRole('button', { name: /^cancel$/i }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    const dist = await openExport('orders')
    const note = within(dist).getByTestId('export-consistency-before')
    expect(note).toHaveAttribute('data-kind', 'distributed')
    expect(note).toHaveTextContent(/each group is read at its own moment/i)
    expect(note).toHaveTextContent(/transaction that touched two groups/i)
  })

  it('says what the spreadsheet guard does and that it is not a security feature, and warns when it is turned off', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openExport('notes', user)
    const text = within(dialog).getByTestId('spreadsheet-text')
    expect(text).toHaveTextContent(/prefixed with an apostrophe/i)
    expect(text).toHaveTextContent(/changes those values/i)
    expect(text).toHaveTextContent(/convenience, not a security feature/i)
    expect(within(dialog).getByTestId('spreadsheet-safe')).toBeChecked()
    expect(within(dialog).queryByTestId('spreadsheet-warning')).toBeNull()
    await user.click(within(dialog).getByTestId('spreadsheet-safe'))
    expect(within(dialog).getByTestId('spreadsheet-warning')).toHaveTextContent(
      /do not open it in a spreadsheet/i,
    )
  })

  it('fills the retention from the cluster setting and offers the byte order mark only for UTF-8', async () => {
    const user = userEvent.setup()
    getState().settings.dataTransfer!.retentionHours = 72
    await renderApp('/databases/shop')
    const dialog = await openExport('notes', user)
    await waitFor(() =>
      expect(within(dialog).getByLabelText('Keep the file (hours)')).toHaveValue(72),
    )
    expect(within(dialog).getByLabelText(/byte order mark/i)).toBeInTheDocument()
    await user.selectOptions(within(dialog).getByLabelText('Encoding'), 'WIN1256')
    expect(within(dialog).queryByLabelText(/byte order mark/i)).toBeNull()
    // The Arabic and Persian encodings are offered under their common names.
    expect(
      within(dialog).getByRole('option', { name: /windows-1256 \(arabic, persian\)/i }),
    ).toBeInTheDocument()
  })

  it('checks the text of a null before anything is sent', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openExport('notes', user)
    await user.type(within(dialog).getByLabelText('Text of a null'), 'a,b')
    await user.click(within(dialog).getByRole('button', { name: /export table/i }))
    expect(await within(dialog).findByText(/does not contain the delimiter/i)).toBeInTheDocument()
    expect(getState().transfer.exports).toEqual([])
  })

  it('needs at least one column when the person chooses columns', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openExport('notes', user)
    await user.click(within(dialog).getByRole('radio', { name: /choose columns/i }))
    await user.click(within(dialog).getByRole('button', { name: /export table/i }))
    expect(await within(dialog).findByText(/choose at least one column/i)).toBeInTheDocument()
  })
})

describe('the export', () => {
  it('runs as a task, then shows size, rows, SHA-256, expiry and the sentence about exporting twice, and the download works', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openExport('notes', user)
    await user.selectOptions(within(dialog).getByLabelText('Delimiter'), ';')
    await user.click(within(dialog).getByRole('checkbox', { name: /compress the file/i }))
    await user.click(within(dialog).getByRole('button', { name: /export table/i }))
    // The progress of the task is shown (steps of the contract).
    expect(await within(dialog).findByLabelText(/task \d+ progress/i)).toBeInTheDocument()
    const result = await within(dialog).findByTestId('export-result')
    const rec = getState().transfer.exports[0]!
    expect(rec.meta.format).toBe('csv.gz')
    expect(await within(result).findByTestId('export-sha')).toHaveTextContent(
      rec.meta.sha256 as string,
    )
    expect(within(result).getByText('CSV, compressed (gzip)')).toBeInTheDocument()
    expect(within(result).getByTestId('export-twice')).toHaveTextContent(
      /compare the two sha-256 values/i,
    )
    expect(within(result).getByTestId('export-lossy')).toHaveTextContent(
      /puts the apostrophe into the table/i,
    )
    expect(within(result).getByTestId('export-consistency')).toHaveAttribute(
      'data-level',
      'single-snapshot',
    )
    await user.click(within(result).getByTestId('download-start'))
    expect(await within(result).findByTestId('download-done')).toHaveTextContent(
      /matches the export/i,
    )
    expect(saved).toEqual([rec.meta.sizeBytes])
  })

  it('names the per-group guarantee of a distributed table in the result', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openExport('skewme', user)
    await user.click(within(dialog).getByRole('button', { name: /export table/i }))
    const consistency = await within(dialog).findByTestId('export-consistency')
    expect(consistency).toHaveAttribute('data-level', 'per-group')
    expect(consistency).toHaveTextContent(/spread over 3 groups/i)
    expect(consistency).toHaveTextContent(
      /transaction that touched two groups can appear in one and not in the other/i,
    )
  })

  it('says with the spreadsheet guard off that the file must not be opened in a spreadsheet', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openExport('notes', user)
    await user.click(within(dialog).getByTestId('spreadsheet-safe'))
    await user.click(within(dialog).getByRole('button', { name: /export table/i }))
    expect(await within(dialog).findByTestId('export-not-for-spreadsheet')).toBeInTheDocument()
  })

  it('shows a failed export with the whitelisted sentence and offers no download', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openExport('failme', user)
    await user.click(within(dialog).getByRole('button', { name: /export table/i }))
    const failed = await within(dialog).findByRole('alert')
    expect(failed).toHaveTextContent(/not valid in the encoding \(SQLSTATE 22021\)/i)
    expect(within(dialog).queryByTestId('export-result')).toBeNull()
    expect(within(dialog).queryByTestId('download-start')).toBeNull()
  })

  it('shows the refusal for a table with another operation in words, and keeps the dialog open', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openExport('blockme', user)
    await user.click(within(dialog).getByRole('button', { name: /export table/i }))
    expect(await within(dialog).findByText('Operation in progress')).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: /export table/i })).toBeEnabled()
  })

  it('asks for the acceptance of code of another role, and goes on when it is given', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openExport('othercode', user)
    await user.click(within(dialog).getByRole('button', { name: /export table/i }))
    expect(await within(dialog).findByText(/would run code of another role/i)).toBeInTheDocument()
    await user.click(
      within(dialog).getByRole('checkbox', { name: /accept that this export runs code/i }),
    )
    await user.click(within(dialog).getByRole('button', { name: /export table/i }))
    expect(await within(dialog).findByTestId('export-result')).toBeInTheDocument()
  })
})

describe('the gates', () => {
  it('disables both actions with the reason while data transfer is off', async () => {
    sw('transfer=off')
    await renderApp('/databases/shop')
    const row = await rowOf('notes')
    await waitFor(() =>
      expect(within(row).getByTestId('export-csv')).toHaveAttribute('aria-disabled', 'true'),
    )
    expect(within(row).getByTestId('export-csv')).toHaveAttribute(
      'title',
      expect.stringMatching(/turned off/i),
    )
  })

  it('disables both actions for a role below administrator with the role as the reason', async () => {
    await renderApp('/databases/shop', { user: 'ops' })
    const row = await rowOf('notes')
    expect(within(row).getByTestId('export-csv')).toHaveAttribute('aria-disabled', 'true')
    expect(within(row).getByTestId('export-csv')).toHaveAttribute(
      'title',
      expect.stringMatching(/kingres_admin/),
    )
  })

  it('disables them while the table has an unfinished task, and enables them again when it ends', async () => {
    await renderApp('/databases/shop')
    const row = await rowOf('notes')
    createTask(getState(), {
      kind: 'table.alter',
      subject: { type: 'table', id: 'shop/public.notes' },
      steps: ['a', 'b'],
      durationMs: 600,
      apply: () => undefined,
    })
    await waitFor(
      () => expect(within(row).getByTestId('export-csv')).toHaveAttribute('aria-disabled', 'true'),
      {
        timeout: 15000,
      },
    )
    expect(within(row).getByTestId('export-csv')).toHaveAttribute(
      'title',
      expect.stringMatching(/waiting for the operation on this table/i),
    )
    await waitFor(
      () => expect(within(row).getByTestId('export-csv')).not.toHaveAttribute('aria-disabled'),
      {
        timeout: 20000,
      },
    )
  })
})
