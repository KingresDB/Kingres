// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The console of the database import (contract 0.41.0, a placeholder revision, increment C3): the action in the header of the
// databases page and its gates (role, the cluster setting), the dialog from the file to the result (the report, the name, the
// checksum, the ledger acknowledgement, the options), a failed import that says what happened to the private database, a
// dangerous role, and the rule that the text of a statement of an archive is never shown.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { renderApp } from './helpers'

const sw = (query: string) => applySwitches(new URLSearchParams(query), getState())
const archive = (name: string, size = 70_000) =>
  new File([new Uint8Array(size)], name, { type: 'application/zip', lastModified: 1 })

async function openImport(as?: string) {
  const user = userEvent.setup()
  await renderApp('/databases', as ? { user: as } : {})
  await user.click(await screen.findByTestId('import-database'))
  return { user, dialog: await screen.findByRole('dialog') }
}

/** Sends the file, waits for the report and returns the dialog. */
async function validated(name: string) {
  const { user, dialog } = await openImport()
  await user.upload(within(dialog).getByTestId('import-file'), archive(name))
  await user.click(within(dialog).getByTestId('import-start'))
  const report = await within(dialog).findByTestId('archive-report', {}, { timeout: 30000 })
  return { user, dialog, report }
}

const confirm = (dialog: HTMLElement) => within(dialog).getByTestId('import-confirm')

describe('the action in the header of the databases page', () => {
  it('is there for an administrator', async () => {
    await renderApp('/databases')
    const button = await screen.findByTestId('import-database')
    expect(button).toHaveTextContent('Import database')
    expect(button).not.toHaveAttribute('aria-disabled')
  })

  it.each(['viewer', 'ops'])('is disabled with the reason for the role %s', async (role) => {
    await renderApp('/databases', { user: role })
    const button = await screen.findByTestId('import-database')
    expect(button).toHaveAttribute('aria-disabled', 'true')
    expect(button).toHaveAttribute('title', expect.stringMatching(/kingres_admin/))
  })

  it('is disabled with the reason while data transfer is off', async () => {
    sw('transfer=off')
    await renderApp('/databases')
    const button = await screen.findByTestId('import-database')
    await waitFor(() => expect(button).toHaveAttribute('aria-disabled', 'true'))
    expect(button).toHaveAttribute('title', expect.stringMatching(/turned off/i))
  })
})

describe('the import dialog', () => {
  it('says what the import does before a file is chosen, and starts disabled', async () => {
    const { dialog } = await openImport()
    expect(dialog).toHaveTextContent(/new.*database/i)
    expect(dialog).toHaveTextContent(/private database is removed/i)
    expect(dialog).toHaveTextContent(/run as your role/i)
    expect(within(dialog).getByTestId('import-start')).toBeDisabled()
  })

  it('refuses an empty file and warns about a name that is not .zip', async () => {
    const { dialog } = await openImport()
    const user = userEvent.setup({ applyAccept: false })
    await user.upload(within(dialog).getByTestId('import-file'), new File([], 'empty.zip'))
    expect(await within(dialog).findByTestId('import-refused')).toHaveTextContent(/empty/i)
    await user.upload(
      within(dialog).getByTestId('import-file'),
      new File([new Uint8Array(9)], 'dump.sql'),
    )
    expect(await within(dialog).findByTestId('import-not-zip')).toBeInTheDocument()
  })

  it('validates the archive, shows the report and the options, and suggests a name from the file', async () => {
    const { dialog, report } = await validated('Orders Copy.kdx.zip')
    expect(report).toHaveAttribute('data-valid', 'true')
    expect(within(report).queryByTestId('report-no-import')).not.toBeInTheDocument()
    expect(within(dialog).getByTestId('import-name')).toHaveValue('orders_copy')
    expect(within(dialog).getByTestId('import-policies')).toHaveTextContent(/imported disabled/i)
    expect(within(dialog).getByTestId('import-fk-fail')).toBeChecked()
    expect(within(dialog).queryByTestId('import-ledger')).not.toBeInTheDocument()
    await waitFor(() => expect(confirm(dialog)).toBeEnabled())
  })

  it('blocks the confirmation for a name that is not valid, with the reason, and for one that exists', async () => {
    const { user, dialog } = await validated('ok.kdx.zip')
    const input = within(dialog).getByTestId('import-name')
    await user.clear(input)
    expect(confirm(dialog)).toBeDisabled()
    expect(within(dialog).getByTestId('import-name-error')).toHaveTextContent(/choose a name/i)
    await user.type(input, 'Bad-Name')
    expect(within(dialog).getByTestId('import-name-error')).toHaveTextContent(/lower-case/i)
    expect(confirm(dialog)).toBeDisabled()
    await user.clear(input)
    await user.type(input, 'shop')
    expect(within(dialog).getByTestId('import-name-error')).toHaveTextContent(/exists/i)
    expect(confirm(dialog)).toBeDisabled()
    await user.clear(input)
    await user.type(input, 'fresh_one')
    expect(within(dialog).getByTestId('import-name-error')).toHaveTextContent('')
    expect(confirm(dialog)).toBeEnabled()
  })

  it('blocks a checksum that is not the one of the archive', async () => {
    const { user, dialog } = await validated('sum.kdx.zip')
    const sha = within(dialog).getByTestId('import-sha')
    await user.type(sha, 'abc')
    expect(within(dialog).getByTestId('import-sha-error')).toHaveTextContent(/64/)
    expect(confirm(dialog)).toBeDisabled()
    await user.clear(sha)
    await user.type(sha, 'f'.repeat(64))
    expect(within(dialog).getByTestId('import-sha-error')).toHaveTextContent(/not the SHA-256/)
    expect(confirm(dialog)).toBeDisabled()
    await user.clear(sha)
    expect(confirm(dialog)).toBeEnabled()
  })

  it('needs the acknowledgement for ledger tables, and says that the chain starts again', async () => {
    const { user, dialog } = await validated('ledger.kdx.zip')
    const box = within(dialog).getByTestId('import-ledger')
    expect(box).toHaveTextContent(
      /starts a new chain at import time.*does not carry the history.*keep the digests of the source database/i,
    )
    expect(confirm(dialog)).toBeDisabled()
    expect(within(dialog).getByTestId('import-blockers')).toHaveTextContent(/new chain/i)
    await user.click(within(dialog).getByTestId('import-ack'))
    expect(confirm(dialog)).toBeEnabled()
    await user.click(within(dialog).getByTestId('import-ack'))
    expect(confirm(dialog)).toBeDisabled()
  })

  it('shows the refused statements as kind, position and fingerprint, never as text, and offers no import', async () => {
    const { dialog, report } = await validated('code.kdx.zip')
    const list = within(report).getByTestId('report-refused')
    expect(within(list).getAllByRole('listitem')).toHaveLength(2)
    expect(report).toHaveTextContent(/pg_dump/)
    expect(report.textContent).not.toMatch(/RETURNS|LANGUAGE|\$\$/)
    expect(within(dialog).getByTestId('import-cannot')).toBeInTheDocument()
    expect(within(dialog).queryByTestId('import-confirm')).not.toBeInTheDocument()
    expect(within(dialog).queryByTestId('import-options')).not.toBeInTheDocument()
  })

  it.each(['hostile.kdx.zip', 'old.kdx.zip'])(
    'stops at the reasons for %s and offers no import',
    async (name) => {
      const { dialog, report } = await validated(name)
      expect(report).toHaveAttribute('data-valid', 'false')
      expect(within(report).getByTestId('report-reasons')).toBeInTheDocument()
      expect(within(dialog).queryByTestId('import-confirm')).not.toBeInTheDocument()
    },
  )

  it('shows a file name with hidden characters as visible escapes', async () => {
    const { dialog } = await openImport()
    const user = userEvent.setup({ applyAccept: false })
    await user.upload(within(dialog).getByTestId('import-file'), archive('a‮b\u0000c.zip'))
    const chosen = await within(dialog).findByTestId('import-chosen')
    expect(chosen.textContent).not.toContain('‮')
    expect(chosen.textContent).not.toContain('\u0000')
  })

  it('imports a good archive: the progress, then the result with a link to the new database', async () => {
    const { user, dialog } = await validated('shop-copy.kdx.zip')
    await waitFor(() => expect(confirm(dialog)).toBeEnabled())
    await user.click(confirm(dialog))
    const done = await within(dialog).findByTestId('import-done', {}, { timeout: 30000 })
    expect(done).toHaveTextContent(/shop_copy.*was imported/)
    expect(within(done).getByTestId('import-open')).toHaveAttribute('href', '/databases/shop_copy')
    expect(within(dialog).queryByTestId('import-failed')).not.toBeInTheDocument()
  })

  it('imports a ledger archive and lists the new chain and the disabled policy in the result', async () => {
    const { user, dialog } = await validated('ledger.kdx.zip')
    await user.click(within(dialog).getByTestId('import-ack'))
    await user.click(confirm(dialog))
    const lines = await within(dialog).findByTestId('import-lines', {}, { timeout: 30000 })
    expect(lines).toHaveTextContent(/new chain/i)
    expect(lines).toHaveTextContent(/imported disabled/i)
  })

  it('names the step and says the private database was removed when the load fails', async () => {
    const { user, dialog } = await validated('failload.kdx.zip')
    await user.click(confirm(dialog))
    const failed = await within(dialog).findByTestId('import-failed', {}, { timeout: 30000 })
    expect(failed).toHaveTextContent(/failed at the step\s*Load the data/i)
    expect(failed).toHaveTextContent(/private database was removed/i)
    expect(within(dialog).queryByTestId('import-done')).not.toBeInTheDocument()
  })

  it('says the private database was kept when keepOnFailure is set', async () => {
    const { user, dialog } = await validated('failload.kdx.zip')
    await user.click(within(dialog).getByTestId('import-keep'))
    await user.click(confirm(dialog))
    const failed = await within(dialog).findByTestId('import-failed', {}, { timeout: 30000 })
    expect(failed).toHaveTextContent(/private database was kept/i)
  })

  it('fails on a foreign key violation, and with keepNotValid finishes and lists the violations', async () => {
    const first = await validated('fkviolation.kdx.zip')
    await first.user.click(confirm(first.dialog))
    const failed = await within(first.dialog).findByTestId('import-failed', {}, { timeout: 30000 })
    expect(failed.parentElement).toHaveTextContent(/orders_customer_fk/)
  })

  it('finishes with keepNotValid and lists the violations', async () => {
    const { user, dialog } = await validated('fkviolation.kdx.zip')
    await user.click(within(dialog).getByTestId('import-fk-keep'))
    await user.type(within(dialog).getByTestId('import-name'), '_kept')
    await user.click(confirm(dialog))
    const lines = await within(dialog).findByTestId('import-lines', {}, { timeout: 30000 })
    expect(lines).toHaveTextContent(/NOT VALID: 7 rows/)
  })

  it('tells a dangerous role to sign in as another administrator', async () => {
    sw('transfer=superuser')
    const { user, dialog } = await validated('d.kdx.zip')
    await user.click(confirm(dialog))
    const notice = await within(dialog).findByTestId('import-dangerous')
    expect(notice).toHaveTextContent(/sign in as another administrator/i)
    expect(notice).toHaveTextContent(/SUPERUSER, REPLICATION or BYPASSRLS/)
  })

  it('shows the refusal of a server that refuses the request (the name was taken meanwhile)', async () => {
    const { user, dialog } = await validated('late.kdx.zip')
    await user.clear(within(dialog).getByTestId('import-name'))
    await user.type(within(dialog).getByTestId('import-name'), 'late_one')
    getState().databases.push(structuredClone(getState().databases[0]!))
    const last = getState().databases[getState().databases.length - 1]!
    last.info.name = 'late_one'
    await user.click(confirm(dialog))
    const problem = await within(dialog).findByRole('alert', {}, { timeout: 30000 })
    expect(problem).toBeInTheDocument()
    expect(within(dialog).queryByTestId('import-done')).not.toBeInTheDocument()
  })
})
