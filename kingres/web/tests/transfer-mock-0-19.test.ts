// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The API layer and the mock of the data transfer (contract 0.19.0, console increment CN1). The mock behaves like the
// server: an upload has an offset that is read from what is held, a chunk at another offset is a conflict that names
// the right one, a non-final chunk is a multiple of 64 KiB, a download answers 200 or 206 with the headers of the
// contract, a file that expired is 410, a range past the end is 416, a request with the setting off or the role too low
// is refused, and what is answered satisfies the schemas of the contract.
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { transferSwitches } from '@/mocks/transfer-data'
import { sha256Hex } from '@/mocks/sha256'
import { ok } from './schema-check'
import { signInAs } from './helpers'
import { bytesOf, csvFile, readAll, runTask, SEGMENT, uploadAll } from './transfer-helpers'

const sw = (query: string) => applySwitches(new URLSearchParams(query), getState())

async function refused(call: Promise<unknown>): Promise<ApiError> {
  try {
    await call
  } catch (e) {
    expect(e).toBeInstanceOf(ApiError)
    return e as ApiError
  }
  throw new Error('The call was not refused.')
}

beforeEach(async () => {
  await signInAs(createPinia())
})

describe('SHA-256 of the mock', () => {
  it('is the real one (the vectors of FIPS 180-4)', () => {
    expect(sha256Hex(new Uint8Array())).toBe(
      'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
    )
    expect(sha256Hex(new TextEncoder().encode('abc'))).toBe(
      'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad',
    )
    // 56 bytes: the padding needs a second block.
    expect(
      sha256Hex(
        new TextEncoder().encode('abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq'),
      ),
    ).toBe('248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1')
  })
})

describe('uploads', () => {
  it('create, send in order, and complete with the SHA-256 of the bytes', async () => {
    const file = bytesOf(3 * SEGMENT + 100)
    const created = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: file.length,
      fileName: 'orders.csv',
    })
    ok(created, 'DataUpload')
    expect(created).toMatchObject({ state: 'receiving', offset: 0, chunkAlign: 65536 })
    const first = await endpoints.uploadDataChunk(
      created.uploadId,
      0,
      new Blob([file.slice(0, 2 * SEGMENT)]),
    )
    expect(first).toEqual({ offset: 2 * SEGMENT, conflict: false })
    expect(await endpoints.getDataUpload(created.uploadId)).toMatchObject({
      state: 'receiving',
      offset: 2 * SEGMENT,
    })
    const last = await endpoints.uploadDataChunk(
      created.uploadId,
      2 * SEGMENT,
      new Blob([file.slice(2 * SEGMENT)]),
    )
    expect(last.offset).toBe(file.length)
    const done = await endpoints.getDataUpload(created.uploadId)
    ok(done, 'DataUpload')
    expect(done).toMatchObject({ state: 'complete', sha256: sha256Hex(file) })
  })

  it('a chunk at another offset is a conflict that says the right one; nothing is written', async () => {
    const created = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: 3 * SEGMENT,
    })
    await endpoints.uploadDataChunk(created.uploadId, 0, new Blob([bytesOf(SEGMENT)]))
    const wrong = await endpoints.uploadDataChunk(created.uploadId, 0, new Blob([bytesOf(SEGMENT)]))
    expect(wrong).toEqual({ offset: SEGMENT, conflict: true })
    const ahead = await endpoints.uploadDataChunk(
      created.uploadId,
      2 * SEGMENT,
      new Blob([bytesOf(SEGMENT)]),
    )
    expect(ahead).toEqual({ offset: SEGMENT, conflict: true })
    expect((await endpoints.getDataUpload(created.uploadId)).offset).toBe(SEGMENT)
  })

  it('a chunk that is not a multiple of 64 KiB is refused unless it is the last; one past the size is 413', async () => {
    const created = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: 3 * SEGMENT,
    })
    const odd = await refused(
      endpoints.uploadDataChunk(created.uploadId, 0, new Blob([bytesOf(SEGMENT + 1)])),
    )
    expect(odd.code).toBe('validation_failed')
    expect(odd.problem.errors?.[0]?.field).toBe('Content-Length')
    const big = await refused(
      endpoints.uploadDataChunk(created.uploadId, 0, new Blob([bytesOf(4 * SEGMENT)])),
    )
    expect(big.code).toBe('chunk_too_large')
    expect(big.status).toBe(413)
  })

  it('a chunk that breaks off keeps the whole segments; the offset says which, and the upload goes on from it', async () => {
    const file = bytesOf(4 * SEGMENT)
    const created = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: file.length,
    })
    transferSwitches.breakChunkAfterBytes = SEGMENT + 5000
    const broken = await refused(endpoints.uploadDataChunk(created.uploadId, 0, new Blob([file])))
    expect(broken.synthetic).toBe(true)
    // One whole segment was sealed; the rest of it was dropped.
    const asked = await endpoints.getDataUpload(created.uploadId)
    expect(asked.offset).toBe(SEGMENT)
    // Resume after the gap: the same bytes from the offset the server holds.
    const resumed = await endpoints.uploadDataChunk(
      created.uploadId,
      asked.offset,
      new Blob([file.slice(asked.offset)]),
    )
    expect(resumed.offset).toBe(file.length)
    expect((await endpoints.getDataUpload(created.uploadId)).sha256).toBe(sha256Hex(file))
  })

  it('answers 401 in the middle of an upload; the session of the mock stays, the next chunk works', async () => {
    transferSwitches.loseSessionAtChunk = 2
    const file = bytesOf(2 * SEGMENT)
    const created = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: file.length,
    })
    await endpoints.uploadDataChunk(created.uploadId, 0, new Blob([file.slice(0, SEGMENT)]))
    const lost = await refused(
      endpoints.uploadDataChunk(created.uploadId, SEGMENT, new Blob([file.slice(SEGMENT)])),
    )
    expect(lost.status).toBe(401)
    expect(lost.code).toBe('session_expired')
    const again = await endpoints.uploadDataChunk(
      created.uploadId,
      SEGMENT,
      new Blob([file.slice(SEGMENT)]),
    )
    expect(again.offset).toBe(file.length)
  })

  it('refuses a size above the largest file with the field named, and a quota that is used up with 409', async () => {
    const big = await refused(
      endpoints.createDataUpload({ purpose: 'tableImport', sizeBytes: 21 * 1024 ** 3 }),
    )
    expect(big.code).toBe('validation_failed')
    expect(big.problem.errors?.[0]?.field).toBe('/sizeBytes')
    sw('transfer=full')
    const full = await refused(
      endpoints.createDataUpload({ purpose: 'tableImport', sizeBytes: 2 * 1024 * 1024 }),
    )
    expect(full.code).toBe('transfer_quota_exceeded')
    expect(full.status).toBe(409)
  })

  it('belongs to its creator: another role gets 404; a delete frees it', async () => {
    const created = await endpoints.createDataUpload({ purpose: 'tableImport', sizeBytes: 100 })
    getState().transfer.uploads[0]!.creator = 'someone-else'
    expect((await refused(endpoints.getDataUpload(created.uploadId))).status).toBe(404)
    getState().transfer.uploads[0]!.creator = 'admin'
    await endpoints.deleteDataUpload(created.uploadId)
    expect((await refused(endpoints.getDataUpload(created.uploadId))).status).toBe(404)
  })
})

describe('export, download and the list', () => {
  const request = { columns: ['note_id', 'created_at'] as string[], spreadsheetSafe: true }

  async function exported(table = 'notes', body: Record<string, unknown> = {}) {
    const task = await endpoints.exportTable('shop', 'public', table, body)
    expect(task.kind).toBe('table.export')
    const done = await runTask(task)
    return { task: done, id: /^transfer (\d+)$/.exec(done.detail ?? '')?.[1] ?? '' }
  }

  it('is a task that ends with `transfer <id>`; the export has what the contract says', async () => {
    const { task, id } = await exported('notes', request)
    expect(task.state).toBe('done')
    expect(id).not.toBe('')
    const e = await endpoints.getDataExport(id)
    ok(e, 'DataExport')
    expect(e).toMatchObject({ state: 'ready', table: 'notes', format: 'csv', rlsApplied: false })
    expect(e.consistency).toEqual([{ table: 'public.notes', level: 'single-snapshot', groups: 1 }])
    const page = await endpoints.listDataExports()
    ok(page, 'DataExportPage')
    expect(page.items.map((x) => x.exportId)).toEqual([id])
  })

  it('labels a distributed table per-group and names the groups', async () => {
    const { id } = await exported('skewme')
    expect((await endpoints.getDataExport(id)).consistency).toEqual([
      { table: 'public.skewme', level: 'per-group', groups: 3 },
    ])
  })

  it('serves 200 with Content-Length, ETag and the headers of the contract; the bytes match the SHA-256', async () => {
    const { id } = await exported('notes', request)
    const meta = await endpoints.getDataExport(id)
    const whole = await endpoints.downloadDataExport(id)
    expect(whole.status).toBe(200)
    expect(whole.contentLength).toBe(meta.sizeBytes)
    expect(whole.etag).toBe(`"${meta.sha256}"`)
    const bytes = await readAll(whole.body)
    expect(bytes.length).toBe(meta.sizeBytes)
    expect(sha256Hex(bytes)).toBe(meta.sha256)
    expect(new TextDecoder().decode(bytes).split('\n')[0]).toBe('note_id,created_at')
    // The download that reached its last byte is recorded.
    expect((await endpoints.getDataExport(id)).downloadedAt).not.toBeNull()
  })

  it('serves a range as 206 with Content-Range, and a range past the end as 416', async () => {
    const { id } = await exported('notes', request)
    const meta = await endpoints.getDataExport(id)
    const size = meta.sizeBytes as number
    const tail = await endpoints.downloadDataExport(id, 10)
    expect(tail.status).toBe(206)
    expect(tail.contentRange).toBe(`bytes 10-${size - 1}/${size}`)
    expect(tail.contentLength).toBe(size - 10)
    expect((await readAll(tail.body)).length).toBe(size - 10)
    const past = await refused(endpoints.downloadDataExport(id, size))
    expect(past.status).toBe(416)
  })

  it('answers 410 for a file that expired, and keeps listing it as expired', async () => {
    const { id } = await exported('notes', request)
    const rec = getState().transfer.exports.find((e) => e.meta.exportId === id)!
    rec.meta.expiresAt = new Date(Date.now() - 1000).toISOString()
    const gone = await refused(endpoints.downloadDataExport(id))
    expect(gone.status).toBe(410)
    expect((await endpoints.getDataExport(id)).state).toBe('expired')
  })

  it('shows another role no file (404), lets any administrator delete one', async () => {
    const { id } = await exported('notes', request)
    getState().transfer.exports[0]!.creator = 'colleague'
    expect((await refused(endpoints.getDataExport(id))).status).toBe(404)
    expect((await refused(endpoints.downloadDataExport(id))).status).toBe(404)
    expect((await endpoints.listDataExports()).items).toEqual([])
    await endpoints.deleteDataExport(id)
    expect(getState().transfer.exports).toEqual([])
    expect((await refused(endpoints.deleteDataExport(id))).status).toBe(404)
  })

  it('the table failme fails the export with a sentence that holds no value; the file is gone', async () => {
    const task = await runTask(await endpoints.exportTable('shop', 'public', 'failme', {}))
    expect(task.state).toBe('failed')
    expect(task.error?.message).toContain('SQLSTATE 22021')
    expect(getState().transfer.exports[0]?.meta.state).toBe('failed')
  })

  it('the table blockme waits for another operation (409), and othercode needs the acknowledgement (422)', async () => {
    const wait = await refused(endpoints.exportTable('shop', 'public', 'blockme', {}))
    expect(wait.code).toBe('operation_in_progress')
    const code = await refused(endpoints.exportTable('shop', 'public', 'othercode', {}))
    expect(code.code).toBe('export_untrusted_code')
    const task = await runTask(
      await endpoints.exportTable('shop', 'public', 'othercode', {
        acknowledgeOtherRolesCode: true,
      }),
    )
    expect(task.state).toBe('done')
  })

  it('refuses options that are not valid, with the fields named', async () => {
    const bad = await refused(
      endpoints.exportTable('shop', 'public', 'notes', {
        delimiter: 'x' as never,
        nullString: 'a"b',
        columns: ['nope'],
        retentionHours: 500,
        utf8Bom: true,
        encoding: 'LATIN1',
      }),
    )
    expect(bad.code).toBe('validation_failed')
    expect(bad.problem.errors?.map((e) => e.field).sort()).toEqual([
      'columns',
      'delimiter',
      'nullString',
      'retentionHours',
      'utf8Bom',
    ])
  })

  it('writes the apostrophe of the spreadsheet guard, and leaves the text alone when it is off', async () => {
    const guarded = await exported('notes', { spreadsheetSafe: true })
    const raw = await exported('notes', { spreadsheetSafe: false })
    const text = async (id: string) =>
      new TextDecoder().decode(await readAll((await endpoints.downloadDataExport(id)).body))
    expect(await text(guarded.id)).toContain("'=1+1")
    expect(await text(raw.id)).not.toContain("'=1+1")
    expect(await text(raw.id)).toContain('=1+1')
  })
})

describe('import', () => {
  async function upload(
    name: string,
    text = 'id,created_at\n1,2026-01-01\n2,2026-01-02\n3,2026-01-03\n',
  ) {
    return uploadAll(endpoints, csvFile(text, name))
  }

  it('is all or nothing: the table has its rows added only when the task is done, and the upload is deleted', async () => {
    const before = getState().databases[0]!.tables.find((t) => t.name === 'notes')!.estimatedRows!
    const id = await upload('notes.csv')
    const task = await endpoints.importTable('shop', 'public', 'notes', {
      uploadId: id,
      header: 'skip',
    })
    expect(task.kind).toBe('table.import')
    expect(getState().databases[0]!.tables.find((t) => t.name === 'notes')!.estimatedRows).toBe(
      before,
    )
    const done = await runTask(task)
    expect(done.state).toBe('done')
    expect(getState().databases[0]!.tables.find((t) => t.name === 'notes')!.estimatedRows).toBe(
      before + 3,
    )
    expect(getState().transfer.uploads).toEqual([])
  })

  it('refuses an upload that is not complete, one of another role, and one that does not exist', async () => {
    const created = await endpoints.createDataUpload({ purpose: 'tableImport', sizeBytes: 500 })
    const early = await refused(
      endpoints.importTable('shop', 'public', 'notes', { uploadId: created.uploadId }),
    )
    expect(early.code).toBe('upload_incomplete')
    expect(
      (await refused(endpoints.importTable('shop', 'public', 'notes', { uploadId: '999' }))).status,
    ).toBe(404)
  })

  it('badrow.csv fails with a whitelisted sentence that names a column and a line, never a value', async () => {
    const id = await upload('badrow.csv', 'id,created_at\n1,SECRET-VALUE\n')
    const done = await runTask(
      await endpoints.importTable('shop', 'public', 'notes', { uploadId: id }),
    )
    expect(done.state).toBe('failed')
    expect(done.error?.message).toMatch(/SQLSTATE 22P02.*column "created_at", line 4/)
    expect(JSON.stringify(done)).not.toContain('SECRET-VALUE')
  })

  it('outcome.csv ends with import_outcome_unknown, which is not retryable', async () => {
    const id = await upload('outcome.csv')
    const done = await runTask(
      await endpoints.importTable('shop', 'public', 'notes', { uploadId: id }),
    )
    expect(done.state).toBe('failed')
    expect(done.error).toMatchObject({ code: 'import_outcome_unknown', retryable: false })
    expect(done.error?.message).toContain('3 rows may have been loaded')
  })

  it('toobig.csv is refused as too large, endmarker.csv for its line; header.csv fails the task when the header must match', async () => {
    const big = await upload('toobig.csv')
    expect(
      (await refused(endpoints.importTable('shop', 'public', 'notes', { uploadId: big }))).code,
    ).toBe('import_too_large')
    const marker = await upload('endmarker.csv')
    expect(
      (await refused(endpoints.importTable('shop', 'public', 'notes', { uploadId: marker }))).code,
    ).toBe('import_end_marker')
    // A header mismatch is not a refusal of the request: PostgreSQL checks it while loading, so the task fails with the code.
    const header = await upload('header.csv')
    const done = await runTask(
      await endpoints.importTable('shop', 'public', 'notes', { uploadId: header, header: 'match' }),
    )
    expect(done.state).toBe('failed')
    expect(done.error?.code).toBe('import_header_mismatch')
  })

  it('a table with another operation under way (blockme) is refused; unknown columns are named', async () => {
    const id = await upload('notes.csv')
    expect(
      (await refused(endpoints.importTable('shop', 'public', 'blockme', { uploadId: id }))).code,
    ).toBe('operation_in_progress')
    const bad = await refused(
      endpoints.importTable('shop', 'public', 'notes', { uploadId: id, columns: ['zzz'] }),
    )
    expect(bad.problem.errors?.[0]?.field).toBe('columns')
  })
})

describe('the setting, the transport and the role', () => {
  const calls: [string, () => Promise<unknown>][] = [
    ['exportTable', () => endpoints.exportTable('shop', 'public', 'notes', {})],
    ['importTable', () => endpoints.importTable('shop', 'public', 'notes', { uploadId: '1' })],
    [
      'createDataUpload',
      () => endpoints.createDataUpload({ purpose: 'tableImport', sizeBytes: 100 }),
    ],
    ['getDataUpload', () => endpoints.getDataUpload('1')],
    ['uploadDataChunk', () => endpoints.uploadDataChunk('1', 0, new Blob([new Uint8Array(10)]))],
    ['deleteDataUpload', () => endpoints.deleteDataUpload('1')],
    ['listDataExports', () => endpoints.listDataExports()],
    ['getDataExport', () => endpoints.getDataExport('1')],
    ['downloadDataExport', () => endpoints.downloadDataExport('1')],
    ['deleteDataExport', () => endpoints.deleteDataExport('1')],
  ]

  it('every one of the ten answers 403 data_transfer_disabled while the setting is off, and insecure_transport off TLS', async () => {
    sw('transfer=off')
    for (const [name, call] of calls) {
      const e = await refused(call())
      expect([name, e.code, e.status]).toEqual([name, 'data_transfer_disabled', 403])
    }
    sw('transfer=insecure')
    getState().settings.dataTransfer!.enabled = true
    for (const [name, call] of calls) {
      const e = await refused(call())
      expect([name, e.code]).toEqual([name, 'insecure_transport'])
    }
  })

  it('every one of the ten answers 403 forbidden to a role below kingres_admin', async () => {
    for (const user of ['ops', 'viewer']) {
      await signInAs(createPinia(), user)
      for (const [name, call] of calls) {
        const e = await refused(call())
        expect([user, name, e.code]).toEqual([user, name, 'forbidden'])
      }
    }
  })

  it('updates the setting with the limits of the contract, and refuses a value outside them with the field named', async () => {
    const bad = await refused(
      endpoints.updateClusterSettings({
        dataTransfer: { retentionHours: 500, maxConcurrent: 0, maxFileBytes: 5 },
      }),
    )
    expect(bad.problem.errors?.map((e) => e.field).sort()).toEqual([
      'dataTransfer.maxConcurrent',
      'dataTransfer.maxFileBytes',
      'dataTransfer.retentionHours',
    ])
    const saved = await endpoints.updateClusterSettings({
      dataTransfer: { enabled: false, retentionHours: 48 },
    })
    ok(saved, 'ClusterSettings')
    expect(saved.dataTransfer).toMatchObject({
      enabled: false,
      retentionHours: 48,
      maxConcurrent: 2,
    })
  })
})
