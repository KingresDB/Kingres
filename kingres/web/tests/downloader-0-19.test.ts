// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The download of an export file (console increment CN3): it resumes with `Range` from the byte it holds after a break,
// checks the status, `Content-Range`, length and ETag of every answer, skips the bytes it has when a server ignores
// `Range`, reports a short file, a changed file, an expired file and a lost session in words that are true, and saves
// nothing unless every byte arrived and the SHA-256 matched.
import { webcrypto } from 'node:crypto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia } from 'pinia'
import { endpoints, type ExportStream } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { transferSwitches } from '@/mocks/transfer-data'
import { sha256Hex } from '@/mocks/sha256'
import { MemorySink, parseContentRange, runDownload, type DownloadOptions } from '@/lib/downloader'
import { describeDownloadFailure } from '@/lib/transfer'
import { signInAs } from './helpers'
import { runTask } from './transfer-helpers'

beforeEach(async () => {
  vi.stubGlobal('crypto', webcrypto)
  await signInAs(createPinia())
})
afterEach(() => vi.unstubAllGlobals())

async function makeExport() {
  const task = await runTask(
    await endpoints.exportTable('shop', 'public', 'notes', { columns: ['note_id', 'created_at'] }),
  )
  const id = /^transfer (\d+)$/.exec(task.detail ?? '')?.[1] ?? ''
  return endpoints.getDataExport(id)
}

function options(
  e: Awaited<ReturnType<typeof makeExport>>,
  sink: MemorySink,
  over: Partial<DownloadOptions> = {},
): DownloadOptions {
  return {
    expectedSize: e.sizeBytes as number,
    sha256: e.sha256 ?? null,
    open: (from, signal) => endpoints.downloadDataExport(e.exportId, from, signal),
    sink,
    signal: new AbortController().signal,
    pause: async () => undefined,
    digest: () => sink.sha256(),
    ...over,
  }
}

/**
 * An `open` whose first answer delivers `cut` bytes and then fails, as a connection does that breaks in the middle of a
 * body. (The mock's own switch breaks the connection before the body: a stream that errors after a chunk loses the chunk
 * in the interceptor of the test server.)
 */
function cutAfter(
  e: Awaited<ReturnType<typeof makeExport>>,
  cut: number,
  tweak: (from: number, answer: ExportStream) => ExportStream = (_, a) => a,
): DownloadOptions['open'] {
  let first = true
  return async (from, signal) => {
    const answer = await endpoints.downloadDataExport(e.exportId, from, signal)
    if (!first) return tweak(from, answer)
    first = false
    const reader = (answer.body as ReadableStream<Uint8Array>).getReader()
    const head = await reader.read()
    await reader.cancel()
    let sent = false
    return {
      ...answer,
      body: new ReadableStream<Uint8Array>({
        pull(controller) {
          if (!sent) {
            sent = true
            controller.enqueue((head.value as Uint8Array).slice(0, cut))
          } else controller.error(new TypeError('network error'))
        },
      }),
    }
  }
}

describe('runDownload', () => {
  it('completes a download, counts the progress and checks the SHA-256 of what arrived', async () => {
    const e = await makeExport()
    const sink = new MemorySink()
    const progress: number[] = []
    const out = await runDownload(options(e, sink, { onProgress: (n) => progress.push(n) }))
    expect(out).toMatchObject({ kind: 'complete', bytes: e.sizeBytes, verified: true, resumes: 0 })
    expect(progress.at(-1)).toBe(e.sizeBytes)
    expect(sha256Hex(new Uint8Array(await sink.blob().arrayBuffer()))).toBe(e.sha256)
  })

  it('resumes with Range from the byte it holds after the network breaks, and the file is whole', async () => {
    const e = await makeExport()
    const sink = new MemorySink()
    const asked: number[] = []
    const open = cutAfter(e, 100)
    const out = await runDownload(
      options(e, sink, {
        open: (from, signal) => {
          asked.push(from)
          return open(from, signal)
        },
      }),
    )
    expect(out).toMatchObject({ kind: 'complete', bytes: e.sizeBytes, verified: true, resumes: 1 })
    // The first answer was cut after 100 bytes; the second asked for the rest, from byte 100, with Range.
    expect(asked).toEqual([0, 100])
    expect(transferSwitches.downloadRequests).toBe(2)
  })

  it('resumes after a stream that ends early without an error (a short answer), and again if it keeps doing it', async () => {
    const e = await makeExport()
    transferSwitches.truncateDownloadAt = 50
    const out = await runDownload(options(e, new MemorySink()))
    expect(out).toMatchObject({ kind: 'complete', resumes: 1, verified: true })
  })

  it('reports a file that stays short as short, with the bytes that arrived, and saves nothing', async () => {
    const e = await makeExport()
    const sink = new MemorySink()
    let calls = 0
    const out = await runDownload(
      options(e, sink, {
        maxResumes: 2,
        open: async (from, signal) => {
          calls += 1
          const full = await endpoints.downloadDataExport(e.exportId, from, signal)
          // Every answer carries 10 bytes and then ends cleanly.
          const reader = full.body!.getReader()
          const first = await reader.read()
          await reader.cancel()
          return {
            ...full,
            body: new ReadableStream<Uint8Array>({
              start(c) {
                c.enqueue((first.value as Uint8Array).slice(0, 10))
                c.close()
              },
            }),
          }
        },
      }),
    )
    expect(calls).toBe(3)
    expect(out).toMatchObject({ kind: 'failed', reason: 'short', received: 30, resumes: 2 })
    expect(describeDownloadFailure('short', 30, e.sizeBytes as number)).toMatch(
      /30 of \d+ bytes arrived.*not saved/s,
    )
  })

  it('reports a connection that cannot be re-established as a network failure', async () => {
    const e = await makeExport()
    const out = await runDownload(
      options(e, new MemorySink(), {
        maxResumes: 1,
        open: async () => {
          throw new TypeError('offline')
        },
      }),
    )
    expect(out).toMatchObject({ kind: 'failed', reason: 'network', received: 0, resumes: 1 })
  })

  it('skips the bytes it already has when the server ignores Range and sends the whole file', async () => {
    const e = await makeExport()
    transferSwitches.ignoreRange = true
    const sink = new MemorySink()
    const out = await runDownload(options(e, sink, { open: cutAfter(e, 100) }))
    expect(out).toMatchObject({ kind: 'complete', verified: true, resumes: 1 })
    expect(sink.blob().size).toBe(e.sizeBytes)
  })

  it('refuses a file whose ETag changed between the first answer and the resume', async () => {
    const e = await makeExport()
    transferSwitches.changeEtagOnResume = true
    const out = await runDownload(options(e, new MemorySink(), { open: cutAfter(e, 100) }))
    expect(out).toMatchObject({ kind: 'failed', reason: 'changed', received: 100 })
  })

  it('refuses a file of another size than the record says', async () => {
    const e = await makeExport()
    const out = await runDownload(
      options(e, new MemorySink(), { expectedSize: (e.sizeBytes as number) + 5 }),
    )
    expect(out).toMatchObject({ kind: 'failed', reason: 'changed', received: 0 })
  })

  it('does not save a file whose SHA-256 differs from the export, although every byte arrived', async () => {
    const e = await makeExport()
    const sink = new MemorySink()
    const out = await runDownload(options(e, sink, { digest: async () => 'f'.repeat(64) }))
    expect(out).toMatchObject({ kind: 'failed', reason: 'changed' })
    expect(sink.closed).toBe(false)
  })

  it('says it could not check the SHA-256 when the browser has no Web Crypto, and still completes', async () => {
    const e = await makeExport()
    const out = await runDownload(options(e, new MemorySink(), { digest: async () => null }))
    expect(out).toMatchObject({ kind: 'complete', verified: null })
  })

  it('reports an expired file (410), a deleted one (404), a refusal (403) and a lost session (401)', async () => {
    const e = await makeExport()
    const rec = getState().transfer.exports[0]!
    rec.meta.expiresAt = new Date(Date.now() - 1000).toISOString()
    expect(await runDownload(options(e, new MemorySink()))).toMatchObject({
      kind: 'failed',
      reason: 'gone',
    })
    getState().transfer.exports = []
    expect(await runDownload(options(e, new MemorySink()))).toMatchObject({
      kind: 'failed',
      reason: 'not_found',
    })
    getState().settings.dataTransfer!.enabled = false
    expect(await runDownload(options(e, new MemorySink()))).toMatchObject({
      kind: 'failed',
      reason: 'refused',
    })
    getState().settings.dataTransfer!.enabled = true
    getState().session = null
    expect(await runDownload(options(e, new MemorySink()))).toMatchObject({
      kind: 'failed',
      reason: 'session',
    })
  })

  it('stops when the person cancels, and says nothing was saved', async () => {
    const e = await makeExport()
    const controller = new AbortController()
    controller.abort()
    const out = await runDownload(options(e, new MemorySink(), { signal: controller.signal }))
    expect(out).toMatchObject({ kind: 'failed', reason: 'cancelled' })
    expect(describeDownloadFailure('cancelled', 0, 10)).toMatch(/nothing was saved/i)
  })

  it('refuses a range answer that starts at another byte than the one asked for', async () => {
    const e = await makeExport()
    const open = cutAfter(e, 100, (from, answer) => ({
      ...answer,
      contentRange: `bytes ${from + 1}-${(e.sizeBytes as number) - 1}/${e.sizeBytes}`,
    }))
    const out = await runDownload(options(e, new MemorySink(), { open }))
    expect(out).toMatchObject({ kind: 'failed', reason: 'range' })
  })
})

describe('parseContentRange', () => {
  it('reads the three numbers and nothing else', () => {
    expect(parseContentRange('bytes 10-19/100')).toEqual({ first: 10, last: 19, size: 100 })
    expect(parseContentRange('bytes */100')).toBeNull()
    expect(parseContentRange('bytes 1-2/x')).toBeNull()
    expect(parseContentRange(null)).toBeNull()
  })
})
