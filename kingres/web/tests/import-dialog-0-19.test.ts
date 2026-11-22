// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The Import CSV dialog (console increment CN4): the local preview and the guess of the first 64 KiB, the options, the
// statement that the import is all or nothing, the limit of 2 GiB said before the upload, the upload with its progress,
// the stop for the password when the session ends, the import as a task, the whitelisted error texts (never a value), and
// the wording of an import whose outcome is not known.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { endpoints } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { transferSwitches } from '@/mocks/transfer-data'
import { GIB } from '@/lib/transfer'
import { listStoredUploads, rememberUpload } from '@/lib/upload-store'
import { renderApp } from './helpers'

const sw = (query: string) => applySwitches(new URLSearchParams(query), getState())
/** A right-to-left override: hidden text that reorders what is around it. */
const RLO = String.fromCharCode(0x202e)
const CSV = 'note_id,body,created_at\n1,hello,2026-01-01\n2,world,2026-01-02\n3,again,2026-01-03\n'

function csv(text = CSV, name = 'notes.csv'): File {
  return new File([text], name, { type: 'text/csv', lastModified: 1_700_000_000_000 })
}

async function rowOf(table: string): Promise<HTMLElement> {
  return (await screen.findByText(`public.${table}`)).closest('tr') as HTMLElement
}

async function openImport(table = 'notes', user = userEvent.setup()) {
  const row = await rowOf(table)
  await user.click(within(row).getByRole('button', { name: /import csv/i }))
  return screen.findByRole('dialog')
}

async function choose(dialog: HTMLElement, file: File, user = userEvent.setup()) {
  await user.upload(within(dialog).getByLabelText(/choose a csv file/i), file)
  await within(dialog).findByTestId('chosen-file')
}

async function toOptions(dialog: HTMLElement, user = userEvent.setup()) {
  await user.click(within(dialog).getByTestId('to-options'))
  return within(dialog).findByTestId('all-or-nothing')
}

const rows = (table: string) =>
  getState().databases[0]!.tables.find((t) => t.name === table)!.estimatedRows!

beforeEach(() => {
  vi.stubGlobal('scrollTo', vi.fn())
})
afterEach(() => vi.unstubAllGlobals())

describe('step 1: the file', () => {
  it('previews the first records in the browser and says what it guessed, before anything is sent', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openImport('notes', user)
    await choose(dialog, csv(), user)
    await within(dialog).findAllByTestId('csv-preview')
    const preview = within(dialog).getAllByTestId('csv-preview')[0] as HTMLElement
    expect(within(preview).getByRole('columnheader', { name: 'note_id' })).toBeInTheDocument()
    expect(within(preview).getByText('hello')).toBeInTheDocument()
    expect(within(dialog).getByTestId('import-guess')).toHaveTextContent(
      /UTF-8.*comma.*a header line/i,
    )
    // Nothing was sent: no upload exists on the server.
    expect(getState().transfer.uploads).toEqual([])
  })

  it('guesses a semicolon and no header, and the preview follows the correction in the next step', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openImport('notes', user)
    await choose(dialog, csv('1;hello;2026-01-01\n2;world;2026-01-02\n'), user)
    expect(within(dialog).getByTestId('import-guess')).toHaveTextContent(
      /semicolon.*no header line/i,
    )
    await toOptions(dialog, user)
    expect(within(dialog).getByTestId('import-delimiter')).toHaveValue(';')
    expect(within(dialog).getByTestId('import-header')).toHaveValue('none')
    // A wrong delimiter shows one wide field per record.
    await user.selectOptions(within(dialog).getByTestId('import-delimiter'), ',')
    const previews = within(dialog).getAllByTestId('csv-preview')
    expect(previews.at(-1)).toHaveTextContent('1;hello;2026-01-01')
  })

  it('refuses a file above 2 GiB before anything is uploaded, saying to split it', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openImport('notes', user)
    const big = csv()
    Object.defineProperty(big, 'size', { value: 3 * GIB })
    await choose(dialog, big, user)
    const refusal = within(dialog).getByTestId('file-refused')
    expect(refusal).toHaveTextContent(/at most 2\.0 GiB of data/i)
    expect(refusal).toHaveTextContent(/split the file/i)
    expect(within(dialog).getByTestId('to-options')).toBeDisabled()
    expect(getState().transfer.uploads).toEqual([])
  })

  it('does not apply the 2 GiB limit to a compressed file and says it cannot be previewed', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openImport('notes', user)
    const gz = new File([new Uint8Array([0x1f, 0x8b, 8, 0, 0, 0, 0, 0])], 'notes.csv.gz', {
      lastModified: 1,
    })
    Object.defineProperty(gz, 'size', { value: 3 * GIB })
    await choose(dialog, gz, user)
    expect(within(dialog).getByTestId('file-compressed')).toHaveTextContent(
      /counts the data after expanding/i,
    )
    expect(within(dialog).getByTestId('to-options')).toBeEnabled()
  })

  it('shows hostile text as text: a cell with markup, a file name with a bidirectional control', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openImport('notes', user)
    await choose(
      dialog,
      csv(`a,b\n<img src=x onerror=alert(1)>,${RLO}evil\n`, `x${RLO}txt.csv`),
      user,
    )
    expect(dialog.querySelector('img')).toBeNull()
    expect(within(dialog).getAllByTestId('csv-preview')[0]).toHaveTextContent(
      '<img src=x onerror=alert(1)>',
    )
    expect(within(dialog).getByTestId('chosen-file').innerHTML).not.toContain(RLO)
  })
})

describe('steps 2 to 4: upload and import', () => {
  it('uploads the file, runs the import as a task, adds the rows and says it was one transaction', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const before = rows('notes')
    const dialog = await openImport('notes', user)
    await choose(dialog, csv(), user)
    const statement = await toOptions(dialog, user)
    expect(statement).toHaveTextContent(/all or nothing/i)
    expect(statement).toHaveTextContent(/nothing is stored/i)
    expect(statement).toHaveTextContent(/2\.0 GiB/)
    await user.click(within(dialog).getByTestId('start-import'))
    expect(
      await within(dialog).findByTestId('import-done', {}, { timeout: 15000 }),
    ).toHaveTextContent(/one transaction/i)
    expect(rows('notes')).toBe(before + 3)
    // The upload was used and deleted; the tab forgot it.
    expect(getState().transfer.uploads).toEqual([])
    expect(listStoredUploads()).toEqual([])
  })

  it('sends the options that were chosen: delimiter, null text, columns in the order of the file', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openImport('notes', user)
    await choose(dialog, csv('1;x\n2;y\n'), user)
    await toOptions(dialog, user)
    await user.type(within(dialog).getByLabelText('Text of a null'), 'NULL')
    await user.click(within(dialog).getByRole('radio', { name: /columns of the file/i }))
    const group = await within(dialog).findByRole('group', { name: 'Columns of the file' })
    await user.click(within(group).getByRole('checkbox', { name: 'created_at' }))
    await user.click(within(group).getByRole('checkbox', { name: 'note_id' }))
    expect(within(dialog).getByTestId('picked-order')).toHaveTextContent('created_at, note_id')
    const seen: unknown[] = []
    const real = endpoints.importTable
    const spy = vi.spyOn(endpoints, 'importTable').mockImplementation((...a) => {
      seen.push(a[3])
      return real(...a)
    })
    await user.click(within(dialog).getByTestId('start-import'))
    await within(dialog).findByTestId('import-done', {}, { timeout: 15000 })
    spy.mockRestore()
    expect(seen[0]).toMatchObject({
      delimiter: ';',
      nullString: 'NULL',
      columns: ['created_at', 'note_id'],
      header: 'none',
      skipBom: true,
      keepUpload: false,
    })
  })

  it('checks the options before the file is sent', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openImport('notes', user)
    await choose(dialog, csv(), user)
    await toOptions(dialog, user)
    await user.type(within(dialog).getByLabelText('Text of a null'), 'a,b')
    await user.click(within(dialog).getByTestId('start-import'))
    expect(await within(dialog).findByText(/does not contain the delimiter/i)).toBeInTheDocument()
    expect(getState().transfer.uploads).toEqual([])
  })

  it('stops for the password when the session ends in the middle of the upload, and goes on after sign-in', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    transferSwitches.loseSessionAtChunk = 1
    const dialog = await openImport('notes', user)
    await choose(dialog, csv(), user)
    await toOptions(dialog, user)
    await user.click(within(dialog).getByTestId('start-import'))
    const prompt = await within(dialog).findByTestId('upload-password')
    expect(prompt).toHaveTextContent(/session ended in the middle of the upload/i)
    await user.type(within(prompt).getByLabelText('Password'), 'demo-password')
    await user.click(within(prompt).getByRole('button', { name: /sign in and continue/i }))
    expect(
      await within(dialog).findByTestId('import-done', {}, { timeout: 15000 }),
    ).toBeInTheDocument()
  })

  it('shows a wrong password in words and keeps the prompt', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    transferSwitches.loseSessionAtChunk = 1
    const dialog = await openImport('notes', user)
    await choose(dialog, csv(), user)
    await toOptions(dialog, user)
    await user.click(within(dialog).getByTestId('start-import'))
    const prompt = await within(dialog).findByTestId('upload-password')
    await user.type(within(prompt).getByLabelText('Password'), 'wrong')
    await user.click(within(prompt).getByRole('button', { name: /sign in and continue/i }))
    expect(
      await within(prompt).findByText(/user name or password is not correct/i),
    ).toBeInTheDocument()
  })

  it('survives a chunk that breaks off: it asks the server where it is and sends the file again', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    transferSwitches.breakChunkAfterBytes = 10
    const dialog = await openImport('notes', user)
    await choose(dialog, csv(), user)
    await toOptions(dialog, user)
    await user.click(within(dialog).getByTestId('start-import'))
    expect(
      await within(dialog).findByTestId('import-done', {}, { timeout: 20000 }),
    ).toBeInTheDocument()
  }, 40000)

  it('continues an upload that this tab remembers instead of starting a new one', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const file = csv()
    const created = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: file.size,
      fileName: file.name,
    })
    rememberUpload({
      uploadId: created.uploadId,
      fileName: file.name,
      sizeBytes: file.size,
      lastModified: file.lastModified,
      database: 'shop',
      schema: 'public',
      table: 'notes',
    })
    const dialog = await openImport('notes', user)
    await choose(dialog, file, user)
    expect(await within(dialog).findByTestId('import-remembered')).toBeInTheDocument()
    await toOptions(dialog, user)
    await user.click(within(dialog).getByTestId('start-import'))
    await within(dialog).findByTestId('import-done', {}, { timeout: 15000 })
    // No second upload was created.
    expect(getState().transfer.nextUploadId).toBe(Number(created.uploadId) + 1)
  })
})

describe('the errors of an import', () => {
  async function run(name: string, text = CSV) {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openImport('notes', user)
    await choose(dialog, csv(text, name), user)
    await toOptions(dialog, user)
    await user.click(within(dialog).getByTestId('start-import'))
    return { dialog, user }
  }

  it('badrow.csv: the whitelisted sentence with the column and the line, never a value, and the options can be changed', async () => {
    const { dialog } = await run(
      'badrow.csv',
      'note_id,body,created_at\n1,SECRET-VALUE,2026-01-01\n',
    )
    const failed = await within(dialog).findByRole('alert', {}, { timeout: 15000 })
    expect(failed).toHaveTextContent(/SQLSTATE 22P02.*column "created_at", line 4/)
    expect(within(dialog).getByTestId('import-step')).not.toHaveTextContent('SECRET-VALUE')
    expect(await within(dialog).findByTestId('import-failed-note')).toHaveTextContent(
      /nothing was stored/i,
    )
    expect(within(dialog).getByTestId('change-options')).toBeInTheDocument()
  })

  it('after a failure the same file is imported again with other options without being sent again', async () => {
    const { dialog, user } = await run('badrow.csv')
    await within(dialog).findByTestId('change-options', {}, { timeout: 15000 })
    const nextId = getState().transfer.nextUploadId
    await user.click(within(dialog).getByTestId('change-options'))
    expect(await within(dialog).findByRole('button', { name: /import again/i })).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: /import again/i }))
    await within(dialog).findByTestId('change-options', {}, { timeout: 15000 })
    expect(getState().transfer.nextUploadId).toBe(nextId)
  })

  it('outcome.csv: says to check the table, that rows may have been loaded, and offers no repeat', async () => {
    const { dialog } = await run('outcome.csv')
    const note = await within(dialog).findByTestId('outcome-unknown', {}, { timeout: 15000 })
    expect(note).toHaveTextContent(/check the table before you repeat the import/i)
    expect(note).toHaveTextContent(/some of the rows may have been loaded/i)
    expect(within(dialog).queryByTestId('change-options')).toBeNull()
    expect(
      within(dialog).queryByRole('button', { name: /import again|upload and import/i }),
    ).toBeNull()
  })

  it('toobig.csv: the server refuses it as too large and the dialog says to split the file', async () => {
    const { dialog } = await run('toobig.csv')
    expect(
      await within(dialog).findByText('The file is too large for one import'),
    ).toBeInTheDocument()
    expect(
      within(dialog).getByText(/split the file into parts of at most 2 GiB/i),
    ).toBeInTheDocument()
  })

  it('endmarker.csv: the refusal names the line', async () => {
    const { dialog } = await run('endmarker.csv')
    expect(await within(dialog).findByText(/on line 7/i)).toBeInTheDocument()
  })
})

describe('the gates', () => {
  it('disables the action with the reason while data transfer is off, and for a role below administrator', async () => {
    sw('transfer=off')
    await renderApp('/databases/shop')
    const row = await rowOf('notes')
    await waitFor(() =>
      expect(within(row).getByTestId('import-csv')).toHaveAttribute('aria-disabled', 'true'),
    )
    expect(within(row).getByTestId('import-csv')).toHaveAttribute(
      'title',
      expect.stringMatching(/turned off/i),
    )
  })

  it('a role below administrator sees the action disabled with the role as the reason', async () => {
    await renderApp('/databases/shop', { user: 'viewer' })
    const row = await rowOf('notes')
    expect(within(row).getByTestId('import-csv')).toHaveAttribute('aria-disabled', 'true')
    expect(within(row).getByTestId('import-csv')).toHaveAttribute(
      'title',
      expect.stringMatching(/kingres_admin/),
    )
  })
})
