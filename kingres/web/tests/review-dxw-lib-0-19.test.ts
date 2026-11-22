// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The tests that the independent review of the console of phase 14a asked for (DXW-01 to DXW-17): the uploader (a busy
// writer, a 400 that carries the offset, the offset of the answer, a refused delete on cancel), the downloader (a failing
// file, a stream that fails after the last byte, a session that ends and is resumed, every check of an answer), the save
// methods, the abort of a download when the session of the console ends, and the mock where it differs from the server.
import { webcrypto } from 'node:crypto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia } from 'pinia'
import { http, HttpResponse } from 'msw'
import { endpoints, type ExportStream } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { resetServerState } from '@/api/server-state'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { transferSwitches } from '@/mocks/transfer-data'
import { sha256Hex } from '@/mocks/sha256'
import { useExportDownload } from '@/composables/useExportDownload'
import {
  chooseSaveMethod,
  MemorySink,
  runDownload,
  type DownloadOptions,
  type DownloadSink,
} from '@/lib/downloader'
import { MEMORY_DOWNLOAD_BYTES, MIB } from '@/lib/transfer'
import { Uploader, type UploaderApi, type UploaderOptions } from '@/lib/uploader'
import { bytesOf, readAll, runTask, SEGMENT } from './transfer-helpers'
import { signInAs } from './helpers'

beforeEach(async () => {
  vi.stubGlobal('crypto', webcrypto)
  await signInAs(createPinia())
})
afterEach(() => vi.unstubAllGlobals())

const api: UploaderApi = {
  createDataUpload: endpoints.createDataUpload,
  getDataUpload: endpoints.getDataUpload,
  uploadDataChunk: endpoints.uploadDataChunk,
  deleteDataUpload: endpoints.deleteDataUpload,
  refreshSession: endpoints.refreshSession,
}

function make(
  bytes: Uint8Array<ArrayBuffer>,
  over: Partial<UploaderOptions> = {},
  wrap: Partial<UploaderApi> = {},
) {
  const sleeps: number[] = []
  const u = new Uploader({
    file: new File([bytes], 'a.csv', { lastModified: 1 }),
    api: { ...api, ...wrap },
    chunkBytes: 2 * SEGMENT,
    sleep: async (ms) => {
      sleeps.push(ms)
    },
    ...over,
  })
  return { u, sleeps }
}

const unauthorized = () =>
  new ApiError({ type: 'about:blank', title: 'x', status: 401, code: 'session_expired' })

describe('DXW-01: a conflict that does not move the offset is a failure with a pause and a cap', () => {
  it('stops as busy after the tries, with a growing pause, and sends no more than the cap', async () => {
    const bytes = bytesOf(4 * SEGMENT)
    let sent = 0
    const { u, sleeps } = make(
      bytes,
      { maxFailures: 3 },
      {
        // Another request still writes the upload: 409 with the offset where it was, every time.
        uploadDataChunk: async (id) => {
          sent += 1
          const held = (await endpoints.getDataUpload(id)).offset
          return { offset: held, conflict: true }
        },
      },
    )
    await u.start()
    expect(u.snapshot).toMatchObject({ phase: 'failed', failure: 'busy', offset: 0 })
    expect(sent).toBe(4)
    expect(sleeps).toEqual([1000, 2000, 4000])
    // The upload is kept: a resume goes on.
    expect(u.snapshot.uploadId).not.toBeNull()
  })

  it('goes on when the writer lets go (the mock answers 409 twice, then takes the chunk)', async () => {
    transferSwitches.busyWriterChunks = 2
    const bytes = bytesOf(4 * SEGMENT)
    const { u, sleeps } = make(bytes)
    await u.start()
    expect(u.snapshot).toMatchObject({ phase: 'complete', sha256: sha256Hex(bytes) })
    expect(sleeps.length).toBe(2)
  })

  it('treats a 204 that does not advance the offset the same way, and one that goes past the end', async () => {
    const bytes = bytesOf(2 * SEGMENT)
    const stuck = make(
      bytes,
      { maxFailures: 1 },
      { uploadDataChunk: async (_id, offset) => ({ offset, conflict: false }) },
    )
    await stuck.u.start()
    expect(stuck.u.snapshot).toMatchObject({ phase: 'failed', failure: 'busy' })
    const past = make(
      bytes,
      { maxFailures: 1 },
      { uploadDataChunk: async () => ({ offset: 99 * SEGMENT, conflict: false }) },
    )
    await past.u.start()
    expect(past.u.snapshot.failure).toBe('busy')
  })

  it('a failure resets when the offset moves (the cap is for failures in a row)', async () => {
    const bytes = bytesOf(6 * SEGMENT)
    let calls = 0
    const { u } = make(
      bytes,
      { maxFailures: 1, chunkBytes: SEGMENT },
      {
        uploadDataChunk: async (id, offset, chunk, signal) => {
          calls += 1
          // Every other request is refused as busy; the others work. Six failures in all, never two in a row.
          if (calls % 2 === 1) return { offset, conflict: true }
          return endpoints.uploadDataChunk(id, offset, chunk, signal)
        },
      },
    )
    await u.start()
    expect(u.snapshot).toMatchObject({ phase: 'complete', sha256: sha256Hex(bytes) })
    expect(calls).toBeGreaterThan(6)
  })
})

describe('DXW-09: a 400 that carries the offset is an answer about the offset', () => {
  it('the endpoint returns it as a conflict, and the uploader goes on from it', async () => {
    transferSwitches.breakChunkAfterBytes = SEGMENT + 9
    transferSwitches.cutAnswers400 = true
    const bytes = bytesOf(4 * SEGMENT)
    const created = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: bytes.length,
    })
    const cut = await endpoints.uploadDataChunk(created.uploadId, 0, new Blob([bytes]))
    expect(cut).toEqual({ offset: SEGMENT, conflict: true })
    transferSwitches.breakChunkAfterBytes = SEGMENT + 9
    const { u } = make(bytes)
    await u.start(created.uploadId)
    expect(u.snapshot).toMatchObject({ phase: 'complete', sha256: sha256Hex(bytes) })
  })

  it('a 400 without the header is still a refusal', async () => {
    mockServer.use(
      http.patch('*/api/v1/data-uploads/:id', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 400, code: 'bad_request' },
          { status: 400, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    )
    const created = await endpoints.createDataUpload({ purpose: 'tableImport', sizeBytes: 10 })
    await expect(
      endpoints.uploadDataChunk(created.uploadId, 0, new Blob(['0123456789'])),
    ).rejects.toMatchObject({
      status: 400,
    })
  })
})

describe('the offset of the answer is the one that counts (U4, E4, U11, U12)', () => {
  it('takes the offset the server holds when it kept less than was sent, with no conflict on the way', async () => {
    const bytes = bytesOf(4 * SEGMENT)
    const { u } = make(
      bytes,
      {},
      {
        uploadDataChunk: (id, offset, chunk, signal) =>
          // The server keeps only the first segment of each chunk.
          endpoints.uploadDataChunk(id, offset, chunk.slice(0, SEGMENT), signal),
      },
    )
    await u.start()
    expect(u.snapshot).toMatchObject({ phase: 'complete', resumes: 0, sha256: sha256Hex(bytes) })
  })

  it('an answer without Upload-Offset is an error, not an offset of 0', async () => {
    mockServer.use(
      http.patch('*/api/v1/data-uploads/:id', () => new HttpResponse(null, { status: 204 })),
    )
    const created = await endpoints.createDataUpload({ purpose: 'tableImport', sizeBytes: 10 })
    await expect(
      endpoints.uploadDataChunk(created.uploadId, 0, new Blob(['0123456789'])),
    ).rejects.toThrow(/did not say where the upload is/)
  })

  it('does not call an upload complete that the server still has as receiving', async () => {
    const bytes = bytesOf(2 * SEGMENT)
    const { u } = make(
      bytes,
      {},
      {
        getDataUpload: async (id) => {
          const up = await endpoints.getDataUpload(id)
          return up.offset === bytes.length ? { ...up, state: 'receiving' } : up
        },
      },
    )
    await u.start()
    expect(u.snapshot).toMatchObject({ phase: 'failed', failure: 'other' })
  })

  it('starts a new upload when the remembered one is lost or deleted on the server', async () => {
    const created = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: 2 * SEGMENT,
    })
    for (const state of ['lost', 'deleted'] as const) {
      const { u } = make(
        bytesOf(2 * SEGMENT),
        {},
        {
          getDataUpload: async (id) => {
            const up = await endpoints.getDataUpload(id)
            return id === created.uploadId ? { ...up, state } : up
          },
        },
      )
      await u.start(created.uploadId)
      expect(u.snapshot).toMatchObject({ phase: 'complete', continued: false })
      expect(u.snapshot.uploadId).not.toBe(created.uploadId)
    }
  })
})

describe('DXW-02: a cancel that could not delete does not forget the upload', () => {
  it('keeps the upload (and says so) when the delete is refused, forgets it on 404 and on success', async () => {
    const forgotten: (string | null)[] = []
    const refuse = make(
      bytesOf(2 * SEGMENT),
      { onUpload: (id) => forgotten.push(id) },
      {
        deleteDataUpload: async () => {
          throw unauthorized()
        },
      },
    )
    await refuse.u.start()
    await refuse.u.cancel()
    expect(refuse.u.snapshot).toMatchObject({ phase: 'cancelled', deletePending: true })
    expect(forgotten).not.toContain(null)
    expect(getState().transfer.uploads.length).toBe(1)

    const missing = make(
      bytesOf(2 * SEGMENT),
      { onUpload: (id) => forgotten.push(id) },
      {
        deleteDataUpload: async () => {
          throw new ApiError({ type: 'about:blank', title: 'x', status: 404, code: 'not_found' })
        },
      },
    )
    await missing.u.start()
    await missing.u.cancel()
    expect(missing.u.snapshot.deletePending).toBe(false)
    expect(forgotten.at(-1)).toBeNull()

    const fine = make(bytesOf(2 * SEGMENT), { onUpload: (id) => forgotten.push(id) })
    await fine.u.start()
    await fine.u.cancel()
    expect(fine.u.snapshot.deletePending).toBe(false)
  })
})

async function makeExport(table = 'notes') {
  const task = await runTask(await endpoints.exportTable('shop', 'public', table, {}))
  const id = /^transfer (\d+)$/.exec(task.detail ?? '')?.[1] ?? ''
  return endpoints.getDataExport(id)
}
type Export = Awaited<ReturnType<typeof makeExport>>

function opts(e: Export, sink: DownloadSink, over: Partial<DownloadOptions> = {}): DownloadOptions {
  return {
    expectedSize: e.sizeBytes as number,
    sha256: e.sha256 ?? null,
    open: (from, signal) => endpoints.downloadDataExport(e.exportId, from, signal),
    sink,
    signal: new AbortController().signal,
    pause: async () => undefined,
    ...over,
  }
}

/** A sink that records what happens to it and can be made to fail. */
class SpySink implements DownloadSink {
  written = 0
  closed = false
  aborted = false
  constructor(
    private readonly failWrite = false,
    private readonly failClose = false,
  ) {}
  async write(chunk: Uint8Array): Promise<void> {
    if (this.failWrite) throw new Error('disk full')
    this.written += chunk.length
  }
  async close(): Promise<void> {
    if (this.failClose) throw new Error('disk full')
    this.closed = true
  }
  async abort(): Promise<void> {
    this.aborted = true
  }
}

function streamOf(
  parts: Uint8Array[],
  then: 'close' | 'error' = 'close',
): ReadableStream<Uint8Array> {
  let at = 0
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      const part = parts[at]
      if (part !== undefined) {
        at += 1
        controller.enqueue(part)
      } else if (then === 'error') controller.error(new TypeError('reset'))
      else controller.close()
    },
  })
}

describe('DXW-05: a failing file is a save error, and nothing hangs', () => {
  it('a write that fails is reported as save, is not resumed, and the file is thrown away', async () => {
    const e = await makeExport()
    const sink = new SpySink(true)
    let opened = 0
    const out = await runDownload(
      opts(e, sink, {
        open: (from, signal) => {
          opened += 1
          return endpoints.downloadDataExport(e.exportId, from, signal)
        },
      }),
    )
    expect(out).toMatchObject({ kind: 'failed', reason: 'save', resumes: 0 })
    expect(opened).toBe(1)
    expect(sink.aborted).toBe(true)
  })

  it('a close that fails (where a full disk is often found) is reported as save and does not throw', async () => {
    const e = await makeExport()
    const sink = new SpySink(false, true)
    const out = await runDownload(opts(e, sink))
    expect(out).toMatchObject({ kind: 'failed', reason: 'save' })
    expect(sink.aborted).toBe(true)
  })

  it('the memory sink refuses a chunk that would pass its limit before it keeps it', async () => {
    const sink = new MemorySink()
    await expect(sink.write(new Uint8Array(MEMORY_DOWNLOAD_BYTES + 1))).rejects.toThrow(/too large/)
    expect(sink.blob().size).toBe(0)
  })

  it('the composable does not hang when the sink fails: the state is failed with the reason', async () => {
    const e = await makeExport()
    const dl = useExportDownload(() => e, { pickSink: async () => new SpySink(false, true) })
    // With a picker the method is a file; without one it is memory. Use the picker path.
    ;(window as unknown as { showSaveFilePicker: () => void }).showSaveFilePicker = () => undefined
    await dl.start()
    delete (window as unknown as { showSaveFilePicker?: unknown }).showSaveFilePicker
    expect(dl.state.value).toBe('failed')
    expect(dl.outcome.value).toMatchObject({ kind: 'failed', reason: 'save' })
  })
})

describe('DXW-10: every byte arrived, then the stream failed', () => {
  it('is complete, with one request, and not a range failure', async () => {
    const e = await makeExport()
    const all = await readAll((await endpoints.downloadDataExport(e.exportId)).body)
    const sink = new MemorySink()
    let opened = 0
    const out = await runDownload(
      opts(e, sink, {
        digest: () => sink.sha256(),
        open: async () => {
          opened += 1
          return {
            status: 200,
            body: streamOf([all], 'error'),
            contentLength: all.length,
            contentRange: null,
            etag: `"${e.sha256}"`,
          }
        },
      }),
    )
    expect(out).toMatchObject({ kind: 'complete', bytes: all.length, verified: true })
    expect(opened).toBe(1)
  })
})

describe('the checks of an answer (D4, D6, D7, D13, D14)', () => {
  const answer = (over: Partial<ExportStream>, body: Uint8Array[] = []): ExportStream => ({
    status: 200,
    body: streamOf(body),
    contentLength: null,
    contentRange: null,
    etag: null,
    ...over,
  })

  it('more bytes than the record says is a changed file (also when the length is not given)', async () => {
    const e = await makeExport()
    const size = e.sizeBytes as number
    const out = await runDownload(
      opts(e, new SpySink(), { open: async () => answer({}, [new Uint8Array(size + 10)]) }),
    )
    expect(out).toMatchObject({ kind: 'failed', reason: 'changed' })
  })

  it('a first 206 must start at 0 and name the total of the record', async () => {
    const e = await makeExport()
    const size = e.sizeBytes as number
    for (const range of [
      `bytes 5-${size - 1}/${size}`,
      `bytes 0-${size - 1}/${size + 1}`,
      'junk',
    ]) {
      const out = await runDownload(
        opts(e, new SpySink(), { open: async () => answer({ status: 206, contentRange: range }) }),
      )
      expect(out, range).toMatchObject({ kind: 'failed', reason: 'changed' })
    }
  })

  it('a resumed 206 must name the same total and the length of the rest', async () => {
    const e = await makeExport()
    const size = e.sizeBytes as number
    const first = new Uint8Array(100)
    const resumed = (over: Partial<ExportStream>) => {
      let n = 0
      return async (): Promise<ExportStream> => {
        n += 1
        return n === 1
          ? answer({ contentLength: size }, [first])
          : answer({
              status: 206,
              contentRange: `bytes 100-${size - 1}/${size}`,
              contentLength: size - 100,
              ...over,
            })
      }
    }
    const total = await runDownload(
      opts(e, new SpySink(), {
        open: resumed({ contentRange: `bytes 100-${size - 1}/${size + 5}` }),
      }),
    )
    expect(total).toMatchObject({ kind: 'failed', reason: 'changed' })
    const length = await runDownload(
      opts(e, new SpySink(), { open: resumed({ contentLength: 7 }) }),
    )
    expect(length).toMatchObject({ kind: 'failed', reason: 'changed' })
  })

  it('an answer without a body is a short file', async () => {
    const e = await makeExport()
    const out = await runDownload(
      opts(e, new SpySink(), { maxResumes: 0, open: async () => answer({ body: null }) }),
    )
    expect(out).toMatchObject({ kind: 'failed', reason: 'short' })
  })
})

describe('DXW-07: a session that ends keeps the bytes and the download goes on from them', () => {
  it('stops with the sink open, then a second run from the kept byte makes the whole file', async () => {
    const e = await makeExport()
    const all = await readAll((await endpoints.downloadDataExport(e.exportId)).body)
    const sink = new MemorySink()
    let call = 0
    const first = await runDownload(
      opts(e, sink, {
        open: async (from, signal) => {
          call += 1
          if (call === 1) {
            const s = await endpoints.downloadDataExport(e.exportId, from, signal)
            const head = (await readAll(s.body)).slice(0, 100)
            return { ...s, body: streamOf([head], 'error') }
          }
          throw unauthorized()
        },
      }),
    )
    expect(first).toMatchObject({ kind: 'failed', reason: 'session', received: 100 })
    expect(sink.closed).toBe(false)
    const second = await runDownload(opts(e, sink, { startAt: 100, digest: () => sink.sha256() }))
    expect(second).toMatchObject({ kind: 'complete', bytes: all.length, verified: true })
    expect(sha256Hex(new Uint8Array(await sink.blob().arrayBuffer()))).toBe(e.sha256)
  })

  it('the composable offers a resume after a lost session and saves the file when it is done', async () => {
    const e = await makeExport()
    const saved: number[] = []
    const dl = useExportDownload(() => e, { save: (_n, blob) => saved.push(blob.size) })
    transferSwitches.sessionEndsOnDownload = true
    await dl.start()
    expect(dl.outcome.value).toMatchObject({ kind: 'failed', reason: 'session', received: 0 })
    expect(dl.canResume.value).toBe(true)
    expect(saved).toEqual([])
    await dl.resume()
    expect(dl.state.value).toBe('done')
    expect(saved).toEqual([e.sizeBytes])
  })
})

describe('D10: the composable saves nothing after a failed download', () => {
  it('does not hand a file over when the download failed, and does when it is whole', async () => {
    const e = await makeExport()
    const saved: number[] = []
    const dl = useExportDownload(() => e, { save: (_n, blob) => saved.push(blob.size) })
    getState().transfer.exports[0]!.meta.expiresAt = new Date(Date.now() - 1000).toISOString()
    await dl.start()
    expect(dl.outcome.value).toMatchObject({ kind: 'failed', reason: 'gone' })
    expect(saved).toEqual([])
    const ok = await makeExport()
    const dl2 = useExportDownload(() => ok, { save: (_n, blob) => saved.push(blob.size) })
    await dl2.start()
    expect(saved).toEqual([ok.sizeBytes])
  })
})

describe('DXW-11: the download stops when the session of the console ends', () => {
  it('aborts the request and reports a cancel', async () => {
    const e = await makeExport()
    const spy = vi.spyOn(endpoints, 'downloadDataExport').mockImplementation((_id, _from, signal) =>
      Promise.resolve({
        status: 200,
        body: new ReadableStream<Uint8Array>({
          start(controller) {
            signal?.addEventListener('abort', () =>
              controller.error(new DOMException('x', 'AbortError')),
            )
          },
        }),
        contentLength: e.sizeBytes ?? null,
        contentRange: null,
        etag: `"${e.sha256}"`,
      }),
    )
    const dl = useExportDownload(() => e)
    const running = dl.start()
    await vi.waitFor(() => expect(dl.state.value).toBe('running'))
    resetServerState()
    await running
    expect(dl.outcome.value).toMatchObject({ kind: 'failed', reason: 'cancelled' })
    spy.mockRestore()
  })
})

describe('the save methods (D11, D12)', () => {
  afterEach(() => {
    delete (window as unknown as { showSaveFilePicker?: unknown }).showSaveFilePicker
  })
  it('is memory up to 256 MiB, the browser link above it, and a file when the browser has a picker', () => {
    expect(chooseSaveMethod(MEMORY_DOWNLOAD_BYTES)).toBe('memory')
    expect(chooseSaveMethod(MEMORY_DOWNLOAD_BYTES + 1)).toBe('link')
    expect(chooseSaveMethod(0)).toBe('memory')
    ;(window as unknown as { showSaveFilePicker: () => void }).showSaveFilePicker = () => undefined
    expect(chooseSaveMethod(20 * 1024 * MIB)).toBe('file')
  })
})

describe('the mock where it used to differ from the server (DXW-15, DXW-04)', () => {
  it('serves the whole file for a Range header that is not one range', async () => {
    const e = await makeExport()
    for (const range of ['bytes=0-1,5-6', 'items=0-5', 'bytes=', 'junk']) {
      const r = await fetch(`${location.origin}/api/v1/data-exports/${e.exportId}/content`, {
        headers: { Range: range },
      })
      expect(r.status, range).toBe(200)
      expect(Number(r.headers.get('Content-Length'))).toBe(e.sizeBytes)
    }
  })

  it('answers 404 to a chunk for an upload that is complete', async () => {
    const created = await endpoints.createDataUpload({ purpose: 'tableImport', sizeBytes: 10 })
    await endpoints.uploadDataChunk(created.uploadId, 0, new Blob(['0123456789']))
    await expect(
      endpoints.uploadDataChunk(created.uploadId, 10, new Blob(['x'])),
    ).rejects.toMatchObject({ status: 404 })
  })

  it('names the encoding of the export in the charset of the download', async () => {
    const task = await runTask(
      await endpoints.exportTable('shop', 'public', 'notes', { encoding: 'WIN1256' }),
    )
    const id = /^transfer (\d+)$/.exec(task.detail ?? '')?.[1] as string
    const r = await fetch(`${location.origin}/api/v1/data-exports/${id}/content`)
    expect(r.headers.get('Content-Type')).toBe('text/csv; charset=windows-1256')
  })

  it('refuses a file above 256 MiB for a reference table (and takes it for another table), and skipBom with another encoding', async () => {
    const big = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: 300 * MIB,
      fileName: 'x.csv',
    })
    getState().transfer.uploads[0]!.upload.state = 'complete'
    getState().transfer.uploads[0]!.upload.offset = 300 * MIB
    await expect(
      endpoints.importTable('shop', 'public', 'countries', { uploadId: big.uploadId }),
    ).rejects.toMatchObject({ code: 'import_too_large' })
    const ok = await endpoints.importTable('shop', 'public', 'notes', { uploadId: big.uploadId })
    expect(ok.kind).toBe('table.import')
    const small = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: 5,
      fileName: 'x.csv',
    })
    await endpoints.uploadDataChunk(small.uploadId, 0, new Blob(['a,b\n1']))
    const bad = await endpoints
      .importTable('shop', 'public', 'notes', {
        uploadId: small.uploadId,
        encoding: 'LATIN1',
        skipBom: true,
      })
      .catch((e: ApiError) => e)
    expect((bad as ApiError).problem.errors?.[0]?.field).toBe('skipBom')
  })

  it('the steps of the import task are the ones of the contract', async () => {
    const small = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: 5,
      fileName: 'x.csv',
    })
    await endpoints.uploadDataChunk(small.uploadId, 0, new Blob(['a,b\n1']))
    const task = await endpoints.importTable('shop', 'public', 'notes', {
      uploadId: small.uploadId,
    })
    expect(task.progress.steps.map((s) => s.name)).toEqual([
      'Check the upload',
      'Check the table',
      'Load rows',
      'Confirm the commit',
      'Clean up',
    ])
  })
})
