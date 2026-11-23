// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The console of the database export and the archive validation (contract 0.40.0, a placeholder revision, increment B5): the
// two actions in the header of the database page with their gates (role, the cluster setting, an unfinished task), the export
// dialog (what the archive does not carry, the consistency sentence before the confirmation, the tables that are left out,
// the acknowledgement of large objects, the refusal for a role that cannot read every table, the result with its SHA-256 and
// download), the validation dialog (the file, the upload, the report of every fixture) and the rule that the text of a
// statement of an archive is never shown.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { createTask } from '@/mocks/state'
import { renderApp } from './helpers'

const sw = (query: string) => applySwitches(new URLSearchParams(query), getState())

/** The footer of a dialog (its X button is also named Close). */
const footer = (dialog: HTMLElement): HTMLElement =>
  dialog.querySelector('.dlg-foot') as HTMLElement

async function openExport(database = 'shop', as?: string) {
  const user = userEvent.setup()
  await renderApp(`/databases/${database}`, as ? { user: as } : {})
  await user.click(await screen.findByTestId('export-database'))
  return { user, dialog: await screen.findByRole('dialog') }
}

async function openValidate() {
  const user = userEvent.setup()
  await renderApp('/databases/shop')
  await user.click(await screen.findByTestId('validate-archive'))
  return { user, dialog: await screen.findByRole('dialog') }
}

/** A small file; the mock reacts to its name. */
const archive = (name: string, size = 70_000) =>
  new File([new Uint8Array(size)], name, { type: 'application/zip', lastModified: 1 })

async function validateFile(name: string) {
  const { user, dialog } = await openValidate()
  await user.upload(within(dialog).getByTestId('archive-file'), archive(name))
  await user.click(within(dialog).getByTestId('start-validation'))
  const report = await within(dialog).findByTestId('archive-report', {}, { timeout: 30000 })
  return { user, dialog, report }
}

describe('the actions in the header of the database page', () => {
  it('has Export database and Validate an archive beside Edit settings', async () => {
    await renderApp('/databases/shop')
    expect(await screen.findByTestId('export-database')).toBeInTheDocument()
    expect(screen.getByTestId('validate-archive')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Edit settings' })).toBeInTheDocument()
  })

  it.each(['viewer', 'ops'])(
    'are disabled with the reason for the role %s, which is below administrator',
    async (role) => {
      await renderApp('/databases/shop', { user: role })
      for (const id of ['export-database', 'validate-archive']) {
        const button = await screen.findByTestId(id)
        expect(button).toHaveAttribute('aria-disabled', 'true')
        expect(button).toHaveAttribute('title', expect.stringMatching(/kingres_admin/))
      }
    },
  )

  it('are disabled with the reason while data transfer is off', async () => {
    sw('transfer=off')
    await renderApp('/databases/shop')
    for (const id of ['export-database', 'validate-archive']) {
      const button = await screen.findByTestId(id)
      await waitFor(() => expect(button).toHaveAttribute('aria-disabled', 'true'))
      expect(button).toHaveAttribute('title', expect.stringMatching(/turned off/i))
    }
  })

  it('disables the export, and only the export, while a task of a table of the database runs', async () => {
    await renderApp('/databases/shop')
    const button = await screen.findByTestId('export-database')
    createTask(getState(), {
      kind: 'table.alter',
      subject: { type: 'table', id: 'shop/public.notes' },
      steps: ['a', 'b'],
      durationMs: 600,
      apply: () => undefined,
    })
    await waitFor(() => expect(button).toHaveAttribute('aria-disabled', 'true'), { timeout: 15000 })
    expect(button).toHaveAttribute('title', expect.stringMatching(/waiting for the operation/i))
    expect(screen.getByTestId('validate-archive')).not.toHaveAttribute('aria-disabled')
  })
})

describe('the export database dialog', () => {
  it('says what the archive does not carry and that importing is not available, before anything is confirmed', async () => {
    const { dialog } = await openExport()
    expect(dialog).toHaveTextContent(/export database shop to an archive/i)
    expect(within(dialog).getByTestId('export-db-not-carried')).toHaveTextContent(
      /roles, tablespaces.*functions, triggers, policies or rules.*importing it is not available/i,
    )
    expect(within(dialog).getByTestId('export-db-read-order')).toHaveTextContent(
      /one after another.*foreign keys at its end/i,
    )
  })

  it('shows the consistency sentence before the confirmation: per group for a database with distributed tables, one snapshot otherwise', async () => {
    const { dialog } = await openExport()
    const before = await within(dialog).findByTestId('export-db-consistency-before')
    await waitFor(() => expect(before).toHaveAttribute('data-skewed', 'true'))
    expect(before).toHaveTextContent(/distributed over 3 shard groups/i)
    expect(before).toHaveTextContent(/no global snapshot/i)
  })

  it('warns about recent write activity when the database is being written to', async () => {
    const { dialog } = await openExport()
    expect(await within(dialog).findByTestId('export-db-busy')).toHaveTextContent(
      /being written to now/i,
    )
  })

  it('lists the tables with sizes, and hides the list when no data is wanted', async () => {
    const { user, dialog } = await openExport()
    const list = await within(dialog).findByTestId('export-db-tables')
    expect(within(list).getByText('public.orders')).toBeInTheDocument()
    expect(within(list).getAllByRole('checkbox').length).toBeGreaterThan(5)
    await user.click(within(dialog).getByTestId('mode-none'))
    expect(within(dialog).queryByTestId('export-db-tables')).not.toBeInTheDocument()
  })

  it('exports, follows the task by table, and shows the archive with its SHA-256, labels, warnings and a download link', async () => {
    const { user, dialog } = await openExport()
    await user.click(within(dialog).getByRole('button', { name: /^export database$/i }))
    const result = await within(dialog).findByTestId('export-db-result', {}, { timeout: 30000 })
    const facts = await within(result).findByTestId('export-db-facts')
    expect(facts).toHaveTextContent(/shop/)
    expect(facts).toHaveTextContent(/database archive \(zip\)/i)
    expect((await within(result).findByTestId('export-db-sha')).textContent).toMatch(
      /^[0-9a-f]{64}$/,
    )
    expect(await within(result).findByTestId('export-db-consistency')).toHaveAttribute(
      'data-level',
      'per-table',
    )
    expect((await within(result).findAllByTestId('export-db-skewed-table')).length).toBeGreaterThan(
      0,
    )
    expect(await within(result).findByTestId('export-db-warnings')).toHaveTextContent(
      /read per group/i,
    )
    expect(await within(result).findByTestId('export-db-compare')).toHaveTextContent(
      /compare it with the value above/i,
    )
    expect(await within(result).findByTestId('export-db-compare')).toHaveTextContent(
      /export the database a second time/i,
    )
    const exportRec = getState().transfer.exports[0]
    expect(exportRec?.meta.kind).toBe('databaseExport')
    expect(
      await within(result).findByRole('button', { name: /download file/i }),
    ).toBeInTheDocument()
  })

  it('sends the tables that are left out, and the retention time', async () => {
    const { user, dialog } = await openExport()
    const list = await within(dialog).findByTestId('export-db-tables')
    await user.click(within(list).getByRole('checkbox', { name: /public\.notes/ }))
    const hours = within(dialog).getByLabelText(/keep the file/i)
    await user.clear(hours)
    await user.type(hours, '48')
    await user.click(within(dialog).getByRole('button', { name: /^export database$/i }))
    const result = await within(dialog).findByTestId('export-db-result', {}, { timeout: 30000 })
    expect(await within(result).findByTestId('export-db-warnings')).toHaveTextContent(
      /public\.notes.*left out/i,
    )
    const meta = getState().transfer.exports[0]?.meta
    const hoursLeft = (Date.parse(meta?.expiresAt ?? '') - Date.now()) / 3_600_000
    expect(hoursLeft).toBeGreaterThan(47)
    expect(hoursLeft).toBeLessThan(49)
  })

  it('refuses a retention time outside 1 to 168 in the form and sends nothing', async () => {
    const { user, dialog } = await openExport()
    const hours = within(dialog).getByLabelText(/keep the file/i)
    await user.clear(hours)
    await user.type(hours, '500')
    await user.click(within(dialog).getByRole('button', { name: /^export database$/i }))
    expect(
      await within(dialog).findByText(/retention time must be a whole number between 1 and 168/i),
    ).toBeInTheDocument()
    expect(getState().transfer.exports).toHaveLength(0)
  })

  it('asks for the acknowledgement of large objects and then goes on', async () => {
    sw('transfer=lobs')
    const { user, dialog } = await openExport()
    await user.click(within(dialog).getByRole('button', { name: /^export database$/i }))
    const ack = await within(dialog).findByTestId('acknowledge-lobs')
    await user.click(within(ack).getByRole('checkbox'))
    await user.click(within(dialog).getByRole('button', { name: /export database|try again/i }))
    const result = await within(dialog).findByTestId('export-db-result', {}, { timeout: 30000 })
    expect(await within(result).findByTestId('export-db-warnings')).toHaveTextContent(
      /large objects/i,
    )
  })

  it('names the objects a role cannot read, at most 20, and writes nothing', async () => {
    const { user, dialog } = await openExport('reports')
    await user.click(within(dialog).getByRole('button', { name: /^export database$/i }))
    const list = await within(dialog).findByTestId('unreadable-objects')
    expect(within(list).getAllByRole('listitem').length).toBeGreaterThan(0)
    expect(within(list).getAllByRole('listitem').length).toBeLessThanOrEqual(20)
    expect(getState().transfer.exports).toHaveLength(0)
  })

  it('shows a failed export with the whitelisted sentence, and says no file was kept', async () => {
    sw('transfer=dbfail')
    const { user, dialog } = await openExport()
    await user.click(within(dialog).getByRole('button', { name: /^export database$/i }))
    expect(
      await within(dialog).findByText(/SQLSTATE 42501/, {}, { timeout: 30000 }),
    ).toBeInTheDocument()
    expect(getState().transfer.exports[0]?.meta.state).toBe('failed')
  })

  it('says that the structure changed during the export (schema_changed)', async () => {
    sw('transfer=changed')
    const { user, dialog } = await openExport()
    await user.click(within(dialog).getByRole('button', { name: /^export database$/i }))
    expect(
      await within(dialog).findByText(/changed while the export ran/i, {}, { timeout: 30000 }),
    ).toBeInTheDocument()
  })

  it('the Transfers panel lists the archive with its name, size, SHA-256 and consistency', async () => {
    const { user, dialog } = await openExport()
    await user.click(within(dialog).getByRole('button', { name: /^export database$/i }))
    await within(dialog).findByTestId('export-db-result', {}, { timeout: 30000 })
    await user.click(within(footer(dialog)).getByRole('button', { name: 'Close' }))
    const panel = await screen.findByTestId('transfers-panel')
    const row = await within(panel).findByText('shop (archive)')
    expect(row.closest('tr')).toHaveTextContent(/database archive \(zip\)/i)
    expect(row.closest('tr')).toHaveTextContent(/tables, \d+ per group/i)
  })
})

describe('the validate an archive dialog', () => {
  it('explains that no database is touched and that importing is not available', async () => {
    const { dialog } = await openValidate()
    expect(dialog).toHaveTextContent(/no database is touched/i)
    expect(dialog).toHaveTextContent(/importing an archive is not available in this version/i)
    expect(within(dialog).getByTestId('start-validation')).toBeDisabled()
  })

  it('sends a good archive and shows a valid report with the versions, the extensions and the tables', async () => {
    const { report } = await validateFile('shop.kdx.zip')
    expect(report).toHaveAttribute('data-valid', 'true')
    expect(within(report).getByTestId('report-headline')).toHaveTextContent(/archive is valid/i)
    expect(within(report).getByTestId('report-versions')).toHaveTextContent(/kingres-export\/1/)
    expect(within(report).getByTestId('report-extensions')).toHaveTextContent(/citext/)
    expect(within(report).getByTestId('report-tables')).toHaveTextContent(
      /distributed \(hash\) tables\s*2/i,
    )
    expect(within(report).getByTestId('report-kinds')).toHaveTextContent(/CREATE TABLE/)
    expect(within(report).getByTestId('report-no-import')).toHaveTextContent(
      /not available in this version/i,
    )
    expect(within(report).queryByTestId('report-ledger')).not.toBeInTheDocument()
  })

  it('shows an invalid archive with the reason and no tables', async () => {
    const { report } = await validateFile('hostile.kdx.zip')
    expect(report).toHaveAttribute('data-valid', 'false')
    expect(within(report).getByTestId('report-headline')).toHaveTextContent(/not valid/i)
    expect(within(report).getByTestId('report-reasons')).toHaveTextContent(/central directory/i)
  })

  it('lists every reason an archive does not fit, and an extension that is not installed', async () => {
    const { report } = await validateFile('old.kdx.zip')
    const reasons = within(report).getByTestId('report-reasons')
    expect(within(reasons).getAllByRole('listitem')).toHaveLength(3)
    expect(reasons).toHaveTextContent(/PostgreSQL 19/)
    expect(within(report).getByTestId('report-extensions')).toHaveTextContent(/not installed/i)
  })

  it('tells that ledger tables restart their chain', async () => {
    const { report } = await validateFile('ledger.kdx.zip')
    expect(within(report).getByTestId('report-ledger')).toHaveTextContent(
      /new chain.*at import time.*keep the digests of the source database/i,
    )
  })

  it('shows a refused statement as kind, position and fingerprint, never as text', async () => {
    const { report } = await validateFile('code.kdx.zip')
    const list = within(report).getByTestId('report-refused')
    const items = within(list).getAllByRole('listitem')
    expect(items).toHaveLength(2)
    expect(items[0]).toHaveTextContent(
      /CREATE FUNCTION.*schema\/pre-data\.sql#14.*fingerprint.*length 212/s,
    )
    expect(report).toHaveTextContent(/the text of a statement is not shown/i)
    expect(report.textContent).not.toMatch(/RETURNS|LANGUAGE|\$\$/)
  })

  it('warns about a file that is not named .zip, and refuses an empty file', async () => {
    const { dialog } = await openValidate()
    const user = userEvent.setup({ applyAccept: false })
    await user.upload(
      within(dialog).getByTestId('archive-file'),
      new File([new Uint8Array(10)], 'dump.sql'),
    )
    expect(await within(dialog).findByTestId('archive-not-zip')).toBeInTheDocument()
    await user.upload(within(dialog).getByTestId('archive-file'), new File([], 'empty.zip'))
    expect(await within(dialog).findByTestId('archive-refused')).toHaveTextContent(/empty/i)
    expect(within(dialog).getByTestId('start-validation')).toBeDisabled()
  })

  it('refuses a file above the largest staged file', async () => {
    const { user, dialog } = await openValidate()
    const big = archive('big.zip', 10)
    Object.defineProperty(big, 'size', { value: 21 * 1024 ** 3 })
    await user.upload(within(dialog).getByTestId('archive-file'), big)
    expect(await within(dialog).findByTestId('archive-refused')).toHaveTextContent(
      /larger than the 20(\.0)? GiB/,
    )
  })

  it('shows a file name with hidden characters as visible escapes, and leaves nothing that can reorder the text', async () => {
    const { user, dialog } = await openValidate()
    await user.upload(within(dialog).getByTestId('archive-file'), archive('evil‮gnp.zip'))
    const chosen = await within(dialog).findByTestId('archive-chosen')
    expect(chosen.textContent).not.toContain('‮')
    expect(chosen).toHaveTextContent(/right-to-left override|202E/i)
  })

  it('shows hostile strings of a report (names, messages) through SafeName', async () => {
    // An archive is a file from anyone: a reason that carries a bidirectional control must not reach the page as it is.
    const { report } = await validateFile('old.kdx.zip')
    expect(report.textContent).not.toMatch(/[‪-‮⁦-⁩]/)
  })
})

describe('an archive that was validated before', () => {
  it('is complete when the same file is chosen again: the upload is continued and validated again', async () => {
    const first = await validateFile('shop.kdx.zip')
    expect(first.report).toHaveAttribute('data-valid', 'true')
    expect(getState().transfer.uploads[0]?.upload.state).toBe('valid')
    // Closed and opened again in the same tab: the tab remembers the upload and the server says it is valid.
    await userEvent
      .setup()
      .click(within(footer(first.dialog)).getByRole('button', { name: 'Close' }))
    const second = await validateFile('shop.kdx.zip')
    expect(second.report).toHaveAttribute('data-valid', 'true')
    expect(getState().transfer.uploads).toHaveLength(1)
  })
})
