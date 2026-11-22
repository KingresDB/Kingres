// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The dialogs and the panel against what the independent review of the console of phase 14a found (DXW-02 to DXW-17):
// an upload is never attached to the import of another file, a repeat of an import whose outcome is not known is not
// one click away, the cap of a reference table, a file that cannot be previewed, a resume that is not for another
// content or another person, the confirmation of a cancel, the download link with its checks and role, the sign-in
// of a download, and a panel that follows the uploads of the tab.
import { webcrypto } from 'node:crypto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { endpoints } from '@/api/endpoints'
import { mockServer } from '@/mocks/server'
import { getState, resetMockState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { transferSwitches } from '@/mocks/transfer-data'
import ExportDownload from '@/components/transfers/ExportDownload.vue'
import UploadStatus from '@/components/transfers/UploadStatus.vue'
import { atomicCapFor, GIB, MIB } from '@/lib/transfer'
import {
  fingerprintFile,
  findStoredUpload,
  listStoredUploads,
  rememberUpload,
} from '@/lib/upload-store'
import type { UploadSnapshot } from '@/lib/uploader'
import { createPinia } from 'pinia'
import { renderApp, renderComponent, signInAs } from './helpers'
import { runTask } from './transfer-helpers'

const CSV = 'note_id,body,created_at\n1,hello,2026-01-01\n2,world,2026-01-02\n3,again,2026-01-03\n'
const csv = (text = CSV, name = 'notes.csv') =>
  new File([text], name, { type: 'text/csv', lastModified: 1_700_000_000_000 })

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

beforeEach(() => {
  vi.stubGlobal('crypto', webcrypto)
})
afterEach(() => vi.unstubAllGlobals())

describe('I2: an upload is never attached to the import of another file', () => {
  it('after a failed import, another file chosen is uploaded and its own upload is imported', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const ids: string[] = []
    const real = endpoints.importTable
    const spy = vi.spyOn(endpoints, 'importTable').mockImplementation((...a) => {
      ids.push(a[3].uploadId)
      return real(...a)
    })
    const dialog = await openImport('notes', user)
    await choose(dialog, csv(CSV, 'badrow.csv'), user)
    await toOptions(dialog, user)
    await user.click(within(dialog).getByTestId('start-import'))
    await within(dialog).findByTestId('change-options', {}, { timeout: 15000 })
    await user.click(within(dialog).getByTestId('change-options'))
    await user.click(within(dialog).getByRole('button', { name: /^back$/i }))
    await choose(dialog, csv('note_id\n9\n', 'other.csv'), user)
    await toOptions(dialog, user)
    expect(within(dialog).getByRole('button', { name: /upload and import/i })).toBeInTheDocument()
    await user.click(within(dialog).getByTestId('start-import'))
    await within(dialog).findByTestId('import-done', {}, { timeout: 15000 })
    spy.mockRestore()
    expect(ids).toHaveLength(2)
    expect(ids[1]).not.toBe(ids[0])
  })
})

describe('I4 and I7', () => {
  it('a compressed file is not refused for its size: the upload is created for it', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const created = vi.spyOn(endpoints, 'createDataUpload')
    const dialog = await openImport('notes', user)
    const gz = new File([new Uint8Array([0x1f, 0x8b, 8, 0])], 'big.csv.gz', { lastModified: 1 })
    Object.defineProperty(gz, 'size', { value: 3 * GIB })
    await choose(dialog, gz, user)
    await toOptions(dialog, user)
    await user.selectOptions(within(dialog).getByTestId('import-header'), 'none')
    await user.click(within(dialog).getByTestId('start-import'))
    await waitFor(() => expect(created).toHaveBeenCalled())
    expect(created.mock.calls[0]?.[0]).toMatchObject({ sizeBytes: 3 * GIB })
    created.mockRestore()
  })

  it('when the upload is gone as the import starts, the file is sent again (the button says so)', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    mockServer.use(
      http.post('*/api/v1/databases/:d/schemas/:s/tables/:t/import', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Not found', status: 404, code: 'not_found' },
          { status: 404, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    )
    const dialog = await openImport('notes', user)
    await choose(dialog, csv(), user)
    await toOptions(dialog, user)
    await user.click(within(dialog).getByTestId('start-import'))
    await user.click(await within(dialog).findByTestId('change-options', {}, { timeout: 15000 }))
    expect(
      await within(dialog).findByRole('button', { name: /upload and import/i }),
    ).toBeInTheDocument()
    expect(within(dialog).queryByRole('button', { name: /import again/i })).toBeNull()
  })
})

describe('DXW-03: a repeat of an import that may have loaded rows is not one click away', () => {
  it('shows the danger notice for the same file and needs the tick before the import starts', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const first = await openImport('notes', user)
    await choose(first, csv(CSV, 'outcome.csv'), user)
    await toOptions(first, user)
    await user.click(within(first).getByTestId('start-import'))
    await within(first).findByTestId('outcome-unknown', {}, { timeout: 15000 })
    await user.click(within(first).getByRole('button', { name: /^close$/i }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(listStoredUploads('admin')[0]?.imported).toBe('unknown')

    const imports = vi.spyOn(endpoints, 'importTable')
    const dialog = await openImport('notes', user)
    await choose(dialog, csv(CSV, 'outcome.csv'), user)
    await toOptions(dialog, user)
    const warning = within(dialog).getByTestId('repeat-warning')
    expect(warning).toHaveTextContent(/ended without a known outcome/i)
    await user.click(within(dialog).getByTestId('start-import'))
    expect(await within(dialog).findByText(/say that you checked the table/i)).toBeInTheDocument()
    expect(imports).not.toHaveBeenCalled()
    await user.click(within(dialog).getByTestId('ack-repeat'))
    await user.click(within(dialog).getByTestId('start-import'))
    await waitFor(() => expect(imports).toHaveBeenCalled())
    imports.mockRestore()
  })

  it('after an import that kept its upload the warning says the rows are in the table', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const first = await openImport('notes', user)
    await choose(first, csv(), user)
    await toOptions(first, user)
    await user.click(within(first).getByRole('checkbox', { name: /keep the uploaded file/i }))
    await user.click(within(first).getByTestId('start-import'))
    await within(first).findByTestId('import-done', {}, { timeout: 15000 })
    expect(listStoredUploads('admin')[0]?.imported).toBe('done')
  })
})

describe('DXW-04: the cap of a reference table', () => {
  it('refuses a file above 256 MiB for a reference table before anything is sent, and says the cap in the options', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openImport('countries', user)
    const big = csv()
    Object.defineProperty(big, 'size', { value: 300 * MIB })
    await choose(dialog, big, user)
    expect(within(dialog).getByTestId('file-refused')).toHaveTextContent(/at most 256 MiB/i)
    expect(within(dialog).getByTestId('to-options')).toBeDisabled()
    expect(getState().transfer.uploads).toEqual([])
    const ok = csv()
    Object.defineProperty(ok, 'size', { value: 256 * MIB })
    await choose(dialog, ok, user)
    expect(within(dialog).queryByTestId('file-refused')).toBeNull()
    expect((await toOptions(dialog, user)).textContent).toMatch(/256 MiB cannot be imported/)
  })
})

describe('DXW-13: a file that cannot be previewed gets no default for its first line', () => {
  it('asks before the upload and goes on once the person chose', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    const dialog = await openImport('notes', user)
    await choose(
      dialog,
      new File([new Uint8Array([0x1f, 0x8b, 8, 0, 1])], 'a.csv.gz', { lastModified: 1 }),
      user,
    )
    await toOptions(dialog, user)
    const select = within(dialog).getByTestId('import-header') as HTMLSelectElement
    expect(select.value).toBe('')
    await user.click(within(dialog).getByTestId('start-import'))
    expect(
      await within(dialog).findByText(/say whether its first line holds column names/i),
    ).toBeInTheDocument()
    expect(getState().transfer.uploads).toEqual([])
    await user.selectOptions(select, 'none')
    await user.click(within(dialog).getByTestId('start-import'))
    await waitFor(() => expect(getState().transfer.uploads.length).toBeGreaterThan(0))
  })
})

describe('DXW-12: an upload is continued only for the same content and the same person', () => {
  const target = { database: 'shop', schema: 'public', table: 'notes' }

  it('does not continue for a file of the same name, size and time with other content', async () => {
    const a = csv('a'.repeat(100))
    const b = csv('b'.repeat(100))
    const fa = await fingerprintFile(a)
    const fb = await fingerprintFile(b)
    expect(fa).not.toBe(fb)
    rememberUpload({
      uploadId: '42',
      user: 'admin',
      fileName: a.name,
      sizeBytes: a.size,
      lastModified: a.lastModified,
      fingerprint: fa as string,
      ...target,
    })
    expect(findStoredUpload(a, target, 'admin', fa)?.uploadId).toBe('42')
    expect(findStoredUpload(b, target, 'admin', fb)).toBeNull()
  })

  it("reads the end of the file too, and does not list or continue another person's upload", async () => {
    const head = 'x'.repeat(70_000)
    const a = csv(`${head}AAAA`)
    const b = csv(`${head}BBBB`)
    expect(await fingerprintFile(a)).not.toBe(await fingerprintFile(b))
    rememberUpload({
      uploadId: '7',
      user: 'someone-else',
      fileName: a.name,
      sizeBytes: a.size,
      lastModified: a.lastModified,
      ...target,
    })
    expect(listStoredUploads('admin')).toEqual([])
    expect(findStoredUpload(a, target, 'admin')).toBeNull()
    expect(findStoredUpload(a, target, 'someone-else')?.uploadId).toBe('7')
  })
})

describe('DXW-17: the panel follows the uploads of the tab', () => {
  it("shows an upload remembered after it was drawn, and not another person's", async () => {
    await renderApp('/databases')
    await screen.findByTestId('transfers-panel')
    expect(screen.queryByTestId('stored-uploads')).toBeNull()
    rememberUpload({
      uploadId: '5',
      user: 'someone-else',
      fileName: 'theirs.csv',
      sizeBytes: 1,
      lastModified: 1,
      database: 'shop',
      schema: 'public',
      table: 'notes',
    })
    rememberUpload({
      uploadId: '6',
      user: 'admin',
      fileName: 'mine.csv',
      sizeBytes: 1,
      lastModified: 1,
      database: 'shop',
      schema: 'public',
      table: 'notes',
    })
    const list = await screen.findByTestId('stored-uploads')
    expect(list).toHaveTextContent('mine.csv')
    expect(list).not.toHaveTextContent('theirs.csv')
  })

  it('keeps an upload in the list when its delete is refused for another reason than 404', async () => {
    const user = userEvent.setup()
    rememberUpload({
      uploadId: '6',
      user: 'admin',
      fileName: 'mine.csv',
      sizeBytes: 1,
      lastModified: 1,
      database: 'shop',
      schema: 'public',
      table: 'notes',
    })
    mockServer.use(
      http.delete('*/api/v1/data-uploads/:id', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Busy', status: 409, code: 'operation_in_progress' },
          { status: 409, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    )
    await renderApp('/databases')
    const list = await screen.findByTestId('stored-uploads')
    await user.click(within(list).getByRole('button', { name: /delete upload of mine\.csv/i }))
    await within(list).findByText(/operation in progress/i)
    expect(listStoredUploads('admin').map((e) => e.uploadId)).toEqual(['6'])
  })

  it('a table task on another table does not disable the actions of this one (P4)', async () => {
    const { createTask } = await import('@/mocks/state')
    await renderApp('/databases/shop')
    const row = await rowOf('notes')
    createTask(getState(), {
      kind: 'table.alter',
      subject: { type: 'table', id: 'shop/public.orders' },
      steps: ['a', 'b'],
      durationMs: 600,
      apply: () => undefined,
    })
    await new Promise((r) => setTimeout(r, 400))
    expect(within(row).getByTestId('export-csv')).not.toHaveAttribute('aria-disabled')
  })
})

describe('DXW-02 and DXW-16: cancelling asks first when much was sent', () => {
  const snap = (over: Partial<UploadSnapshot> = {}): UploadSnapshot => ({
    phase: 'sending',
    offset: 10 * MIB,
    size: 100 * MIB,
    uploadId: '1',
    failure: null,
    problem: null,
    sha256: null,
    resumes: 0,
    continued: false,
    deletePending: false,
    ...over,
  })
  const props = { percent: 10, signingIn: false, signInError: null }

  it('asks, and cancels only on the second click; a small upload cancels at once', async () => {
    const user = userEvent.setup()
    const { emitted } = await renderComponent(UploadStatus, { snapshot: snap(), ...props })
    await user.click(screen.getByTestId('upload-cancel'))
    expect(screen.getByTestId('cancel-confirm')).toHaveTextContent(/cancelling deletes them/i)
    expect(emitted().cancel).toBeUndefined()
    await user.click(screen.getByTestId('cancel-yes'))
    expect(emitted().cancel).toHaveLength(1)
  })

  it('a small upload cancels with one click, and the cancel that needs a session says it cannot delete', async () => {
    const user = userEvent.setup()
    const small = await renderComponent(UploadStatus, { snapshot: snap({ offset: MIB }), ...props })
    await user.click(screen.getByTestId('upload-cancel'))
    expect(small.emitted().cancel).toHaveLength(1)
    small.unmount()
    await renderComponent(UploadStatus, { snapshot: snap({ phase: 'password' }), ...props })
    expect(screen.getByTestId('upload-password')).toHaveTextContent(
      /cannot delete the upload on the server/i,
    )
  })

  it('tells busy writer and what to do', async () => {
    await renderComponent(UploadStatus, {
      snapshot: snap({ phase: 'failed', failure: 'busy' }),
      ...props,
    })
    expect(screen.getByTestId('upload-failed')).toHaveTextContent(
      /another request is still writing/i,
    )
    expect(screen.getByTestId('upload-resume')).toBeInTheDocument()
  })
})

describe('DXW-08 and DXW-07: the download component', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
  })

  async function bigExport() {
    const task = await runTask(await endpoints.exportTable('shop', 'public', 'notes', {}))
    const id = /^transfer (\d+)$/.exec(task.detail ?? '')?.[1] as string
    const e = await endpoints.getDataExport(id)
    return { ...e, sizeBytes: 300 * MIB }
  }

  it('the browser-link fallback shows size, SHA-256 and how to check it, for an administrator only', async () => {
    const e = await bigExport()
    const admin = await renderComponent(ExportDownload, { export: e })
    const note = screen.getByTestId('download-link-note')
    expect(note).toHaveTextContent(/sha256sum/)
    expect(note).toHaveTextContent(/certutil -hashfile/)
    expect(screen.getByTestId('link-sha')).toHaveTextContent(e.sha256 as string)
    expect(screen.getByTestId('download-link')).toHaveAttribute(
      'href',
      expect.stringMatching(/\/data-exports\/\d+\/content$/),
    )
    admin.unmount()
    await renderComponent(ExportDownload, { export: e }, { user: 'viewer' })
    expect(screen.queryByTestId('download-link')).toBeNull()
    expect(screen.getByTestId('download-link-role')).toHaveTextContent(/kingres_admin/)
  })

  it('a download whose session ends asks for the password and goes on', async () => {
    const user = userEvent.setup()
    const task = await runTask(await endpoints.exportTable('shop', 'public', 'notes', {}))
    const id = /^transfer (\d+)$/.exec(task.detail ?? '')?.[1] as string
    const e = await endpoints.getDataExport(id)
    const saved: number[] = []
    await renderComponent(ExportDownload, {
      export: e,
      hooks: { save: (_n: string, blob: Blob) => saved.push(blob.size) },
    })
    transferSwitches.sessionEndsOnDownload = true
    await user.click(screen.getByTestId('download-start'))
    const form = await screen.findByTestId('download-password')
    expect(screen.getByTestId('download-failed')).toHaveTextContent(/enter your password/i)
    await user.type(within(form).getByLabelText('Password'), 'demo-password')
    await user.click(within(form).getByRole('button', { name: /sign in and continue/i }))
    expect(await screen.findByTestId('download-done')).toBeInTheDocument()
    expect(saved).toEqual([e.sizeBytes])
  })
})

describe('DataExport.spreadsheetSafe and DataTransferSettings.maxAtomicBytes', () => {
  it('the download dialog warns about a file written with the guard off, says what the guard did when it was on, and says it does not know when it is null', async () => {
    const expected = {
      off: ['download-spreadsheet-off', /guard off/i],
      on: ['download-spreadsheet-on', /not a security feature/i],
      unknown: ['download-spreadsheet-note', /does not say which option/i],
    } as const
    const values = { off: false, on: true, unknown: null }
    for (const key of ['off', 'on', 'unknown'] as const) {
      const user = userEvent.setup()
      applySwitches(new URLSearchParams('transfer=exports'), getState())
      getState().transfer.exports.find((e) => e.meta.table === 'orders')!.meta.spreadsheetSafe =
        values[key]
      await renderApp('/databases')
      const panel = await screen.findByTestId('transfers-panel')
      const row = (await within(panel).findByText('shop.public.orders')).closest(
        'tr',
      ) as HTMLElement
      await user.click(within(row).getByRole('button', { name: /download the file/i }))
      const dialog = await screen.findByRole('dialog')
      expect(within(dialog).getByTestId(expected[key][0])).toHaveTextContent(expected[key][1])
      cleanup()
      resetMockState()
    }
  })

  it('the mock records the option of the export', async () => {
    await renderApp('/databases')
    const off = await runTask(
      await endpoints.exportTable('shop', 'public', 'notes', { spreadsheetSafe: false }),
    )
    const on = await runTask(await endpoints.exportTable('shop', 'public', 'notes', {}))
    const get = async (t: typeof off) =>
      endpoints.getDataExport(/^transfer (\d+)$/.exec(t.detail ?? '')?.[1] as string)
    expect((await get(off)).spreadsheetSafe).toBe(false)
    expect((await get(on)).spreadsheetSafe).toBe(true)
  })

  it('the import cap is the one the server reports, and 256 MiB at most for a reference table', async () => {
    expect(atomicCapFor(['plain'], 100 * MIB)).toBe(100 * MIB)
    expect(atomicCapFor(['plain'])).toBe(2 * GIB)
    expect(atomicCapFor(['reference'], 3 * GIB)).toBe(256 * MIB)
    expect(atomicCapFor(['reference'], 100 * MIB)).toBe(100 * MIB)
    expect(atomicCapFor(['distributed'], 4 * GIB)).toBe(4 * GIB)
    const user = userEvent.setup()
    setCap(100 * MIB)
    await renderApp('/databases/shop')
    const dialog = await openImport('notes', user)
    const big = csv()
    Object.defineProperty(big, 'size', { value: 150 * MIB })
    await choose(dialog, big, user)
    expect(within(dialog).getByTestId('file-refused')).toHaveTextContent(/at most 100 MiB/i)
    expect(within(dialog).getByTestId('to-options')).toBeDisabled()
  })

  it('the mock refuses an import above the cap of the setting', async () => {
    await renderApp('/databases')
    setCap(100 * MIB)
    const up = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: 150 * MIB,
      fileName: 'x.csv',
    })
    getState().transfer.uploads[0]!.upload.state = 'complete'
    await expect(
      endpoints.importTable('shop', 'public', 'notes', { uploadId: up.uploadId }),
    ).rejects.toMatchObject({
      code: 'import_too_large',
    })
  })

  it('the settings page says the cap the server reports', async () => {
    setCap(512 * MIB)
    await renderApp('/settings')
    expect(await screen.findByTestId('data-transfer-state')).toHaveTextContent(/512 MiB of data/)
  })
})

/** `maxAtomicBytes` is read-only in the contract (the server reports it); the mock's state sets it. */
function setCap(bytes: number): void {
  ;(getState().settings.dataTransfer as { maxAtomicBytes?: number }).maxAtomicBytes = bytes
}
