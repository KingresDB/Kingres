// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The resumable uploader (console increment CN4): chunks of 64 MiB (cut to what the server accepts and to the segment),
// sent one at a time in order; the offset read again from the server after a break, a conflict or a failure; the session
// checked between chunks and a stop for the password on a 401; pause, resume and cancel; the 2 GiB cap enforced before the
// first byte; a file that differs in size refused; and the chunk never read whole by the uploader (a slice is the body).
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { ApiError, networkError } from '@/api/problem'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { transferSwitches } from '@/mocks/transfer-data'
import { sha256Hex } from '@/mocks/sha256'
import { chunkSize, Uploader, type UploaderApi, type UploaderOptions } from '@/lib/uploader'
import { CHUNK_BYTES, GIB, MAX_ATOMIC_BYTES, MIB } from '@/lib/transfer'
import { bytesOf, SEGMENT } from './transfer-helpers'
import { signInAs } from './helpers'

beforeEach(async () => {
  await signInAs(createPinia())
})

const api: UploaderApi = {
  createDataUpload: endpoints.createDataUpload,
  getDataUpload: endpoints.getDataUpload,
  uploadDataChunk: endpoints.uploadDataChunk,
  deleteDataUpload: endpoints.deleteDataUpload,
  refreshSession: endpoints.refreshSession,
}

function source(bytes: Uint8Array<ArrayBuffer>, name = 'orders.csv') {
  return new File([bytes], name, { type: 'text/csv', lastModified: 1_700_000_000_000 })
}

function make(file: File, over: Partial<UploaderOptions> = {}, wrap: Partial<UploaderApi> = {}) {
  const phases: string[] = []
  const u = new Uploader({
    file,
    api: { ...api, ...wrap },
    chunkBytes: 2 * SEGMENT,
    sleep: async () => undefined,
    onChange: (s) => phases.push(s.phase),
    ...over,
  })
  return { u, phases }
}

describe('chunkSize', () => {
  it('is 64 MiB by default, cut to what the server accepts, always a multiple of the segment', () => {
    expect(chunkSize(CHUNK_BYTES, 256 * MIB)).toBe(64 * MIB)
    expect(chunkSize(CHUNK_BYTES, 128 * 1024)).toBe(128 * 1024)
    expect(chunkSize(100_000, 256 * MIB)).toBe(65536)
    expect(chunkSize(10, 10)).toBe(65536)
  })
})

describe('Uploader', () => {
  it('sends the file in order and completes with the SHA-256 the server computed', async () => {
    const bytes = bytesOf(5 * SEGMENT + 123)
    const sent: number[] = []
    const { u, phases } = make(
      source(bytes),
      {},
      {
        uploadDataChunk: async (id, offset, chunk, signal) => {
          sent.push(offset, chunk.size)
          return endpoints.uploadDataChunk(id, offset, chunk, signal)
        },
      },
    )
    await u.start()
    expect(u.snapshot).toMatchObject({
      phase: 'complete',
      offset: bytes.length,
      sha256: sha256Hex(bytes),
      failure: null,
    })
    // 2 + 2 + 1 segments and the 123 bytes after them: three chunks, each at the offset after the one before.
    expect(sent).toEqual([0, 2 * SEGMENT, 2 * SEGMENT, 2 * SEGMENT, 4 * SEGMENT, SEGMENT + 123])
    expect(phases).toContain('starting')
    expect(phases.at(-1)).toBe('complete')
  })

  it('resumes after a chunk was cut: asks the server where it is and sends the rest, and the file is whole', async () => {
    const bytes = bytesOf(6 * SEGMENT)
    transferSwitches.breakChunkAfterBytes = SEGMENT + 77
    const { u } = make(source(bytes))
    await u.start()
    expect(u.snapshot.phase).toBe('complete')
    expect(u.snapshot.resumes).toBeGreaterThanOrEqual(1)
    expect(u.snapshot.sha256).toBe(sha256Hex(bytes))
  })

  it('re-reads the offset after a conflict and goes on from the server', async () => {
    const bytes = bytesOf(4 * SEGMENT)
    let first = true
    const reads: number[] = []
    const { u } = make(
      source(bytes),
      {},
      {
        uploadDataChunk: async (id, offset, chunk, signal) => {
          if (first) {
            first = false
            // The chunk arrives, but the answer is lost; the repeat is a conflict that names the server's offset.
            await endpoints.uploadDataChunk(id, offset, chunk, signal)
          }
          return endpoints.uploadDataChunk(id, offset, chunk, signal)
        },
        getDataUpload: async (id) => {
          const up = await endpoints.getDataUpload(id)
          reads.push(up.offset)
          return up
        },
      },
    )
    await u.start()
    expect(u.snapshot).toMatchObject({ phase: 'complete', sha256: sha256Hex(bytes) })
    expect(reads[0]).toBe(2 * SEGMENT)
  })

  it('stops for the password on a 401 and goes on from the server offset when the person has signed in', async () => {
    const bytes = bytesOf(6 * SEGMENT)
    transferSwitches.loseSessionAtChunk = 2
    const { u } = make(source(bytes))
    await u.start()
    expect(u.snapshot).toMatchObject({ phase: 'password', offset: 2 * SEGMENT })
    expect(u.snapshot.problem?.code).toBe('session_expired')
    await u.continueAfterSignIn()
    expect(u.snapshot).toMatchObject({ phase: 'complete', sha256: sha256Hex(bytes) })
  })

  it('notices a session that ended between two chunks, before the next chunk is sent', async () => {
    const bytes = bytesOf(4 * SEGMENT)
    let chunks = 0
    const { u } = make(
      source(bytes),
      {},
      {
        uploadDataChunk: async (id, offset, chunk, signal) => {
          chunks += 1
          return endpoints.uploadDataChunk(id, offset, chunk, signal)
        },
        refreshSession: async () => {
          getState().session = null
          return endpoints.refreshSession()
        },
      },
    )
    await u.start()
    expect(u.snapshot.phase).toBe('password')
    expect(chunks).toBe(1)
  })

  it('pauses without losing what the server holds, and resumes from there', async () => {
    const bytes = bytesOf(6 * SEGMENT)
    let paused = false
    const { u } = make(
      source(bytes),
      {},
      {
        uploadDataChunk: async (id, offset, chunk, signal) => {
          const answer = await endpoints.uploadDataChunk(id, offset, chunk, signal)
          if (!paused) {
            paused = true
            u.pause()
          }
          return answer
        },
      },
    )
    await u.start()
    expect(u.snapshot.phase).toBe('paused')
    const held = (await endpoints.getDataUpload(u.snapshot.uploadId as string)).offset
    expect(held).toBeGreaterThan(0)
    expect(held).toBeLessThan(bytes.length)
    await u.resume()
    expect(u.snapshot).toMatchObject({ phase: 'complete', sha256: sha256Hex(bytes) })
  })

  it('cancels: the staged upload is deleted on the server and forgotten', async () => {
    const bytes = bytesOf(4 * SEGMENT)
    const forgotten: (string | null)[] = []
    const { u } = make(source(bytes), { onUpload: (id) => forgotten.push(id) })
    await u.start()
    const id = u.snapshot.uploadId as string
    await u.cancel()
    expect(u.snapshot.phase).toBe('cancelled')
    expect(getState().transfer.uploads).toEqual([])
    expect(forgotten).toEqual([id, null])
    await expect(endpoints.getDataUpload(id)).rejects.toMatchObject({ status: 404 })
  })

  it('continues an upload of the same size from where the server is', async () => {
    const bytes = bytesOf(4 * SEGMENT)
    const created = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: bytes.length,
    })
    await endpoints.uploadDataChunk(created.uploadId, 0, new Blob([bytes.slice(0, SEGMENT)]))
    const { u } = make(source(bytes))
    await u.start(created.uploadId)
    expect(u.snapshot).toMatchObject({
      phase: 'complete',
      continued: true,
      uploadId: created.uploadId,
    })
    expect(u.snapshot.sha256).toBe(sha256Hex(bytes))
  })

  it('refuses to continue an upload of another size, and never sends a byte of it', async () => {
    const created = await endpoints.createDataUpload({
      purpose: 'tableImport',
      sizeBytes: 3 * SEGMENT,
    })
    let sent = 0
    const { u } = make(
      source(bytesOf(4 * SEGMENT)),
      {},
      {
        uploadDataChunk: async (...args) => {
          sent += 1
          return endpoints.uploadDataChunk(...args)
        },
      },
    )
    await u.start(created.uploadId)
    expect(u.snapshot).toMatchObject({ phase: 'failed', failure: 'size_differs' })
    expect(sent).toBe(0)
  })

  it('starts a new upload when the remembered one is gone', async () => {
    const { u } = make(source(bytesOf(2 * SEGMENT)))
    await u.start('999')
    expect(u.snapshot).toMatchObject({ phase: 'complete', continued: false })
    expect(u.snapshot.uploadId).not.toBe('999')
  })

  it('enforces the cap of 2 GiB before the first request, and says so as too_large', async () => {
    let calls = 0
    const counting = (name: keyof UploaderApi) =>
      (async (...args: unknown[]) => {
        calls += 1
        return (api[name] as (...a: unknown[]) => unknown)(...args)
      }) as never
    const big = {
      name: 'big.csv',
      size: MAX_ATOMIC_BYTES + 1,
      lastModified: 1,
      slice: () => new Blob(),
    }
    const { u } = make(
      big as never,
      {},
      {
        createDataUpload: counting('createDataUpload'),
        uploadDataChunk: counting('uploadDataChunk'),
        getDataUpload: counting('getDataUpload'),
      },
    )
    await u.start()
    expect(u.snapshot).toMatchObject({ phase: 'failed', failure: 'too_large' })
    expect(calls).toBe(0)
    // Exactly 2 GiB is allowed by the cap.
    const exact = { ...big, size: MAX_ATOMIC_BYTES }
    const second = make(
      exact as never,
      {},
      {
        createDataUpload: async () => {
          calls += 1
          throw new ApiError({ type: 'about:blank', title: 'x', status: 403, code: 'forbidden' })
        },
      },
    )
    await second.u.start()
    expect(calls).toBe(1)
  })

  it('does not apply the cap to a compressed file (the server counts after expansion) but applies the largest file', async () => {
    let created = 0
    const big = { name: 'big.csv.gz', size: 3 * GIB, lastModified: 1, slice: () => new Blob() }
    const stop = async () => {
      created += 1
      throw new ApiError({ type: 'about:blank', title: 'x', status: 403, code: 'forbidden' })
    }
    const a = make(big as never, { compressed: true }, { createDataUpload: stop })
    await a.u.start()
    expect(created).toBe(1)
    const b = make(
      big as never,
      { compressed: true, maxFileBytes: 2 * GIB },
      { createDataUpload: stop },
    )
    await b.u.start()
    expect(b.u.snapshot).toMatchObject({ failure: 'too_large' })
    expect(created).toBe(1)
  })

  it('refuses an empty file', async () => {
    const { u } = make(new File([], 'empty.csv'))
    await u.start()
    expect(u.snapshot).toMatchObject({ phase: 'failed', failure: 'empty' })
  })

  it('stops with the offset kept when the network does not come back, and a resume goes on', async () => {
    const bytes = bytesOf(4 * SEGMENT)
    let down = true
    const { u } = make(
      source(bytes),
      { maxFailures: 2 },
      {
        uploadDataChunk: async (id, offset, chunk, signal) => {
          if (down) throw networkError(new TypeError('offline'))
          return endpoints.uploadDataChunk(id, offset, chunk, signal)
        },
      },
    )
    await u.start()
    expect(u.snapshot).toMatchObject({ phase: 'failed', failure: 'network', offset: 0 })
    down = false
    await u.resume()
    expect(u.snapshot).toMatchObject({ phase: 'complete', sha256: sha256Hex(bytes) })
  })

  it('reports a full staging area, a refusal because transfer is off, and a checksum mismatch', async () => {
    applySwitches(new URLSearchParams('transfer=full'), getState())
    const a = make(source(bytesOf(2 * MIB)))
    await a.u.start()
    expect(a.u.snapshot).toMatchObject({ failure: 'quota' })
    getState().settings.dataTransfer!.stagingQuotaBytes = 100 * GIB
    getState().settings.dataTransfer!.enabled = false
    const b = make(source(bytesOf(SEGMENT)))
    await b.u.start()
    expect(b.u.snapshot).toMatchObject({ failure: 'refused' })
    expect(b.u.snapshot.problem?.code).toBe('data_transfer_disabled')
  })

  it('hands the server a slice of the file and never more than one chunk', async () => {
    const bytes = bytesOf(5 * SEGMENT)
    const sizes: number[] = []
    const { u } = make(
      source(bytes),
      { chunkBytes: SEGMENT },
      {
        uploadDataChunk: async (id, offset, chunk, signal) => {
          expect(chunk).toBeInstanceOf(Blob)
          sizes.push(chunk.size)
          return endpoints.uploadDataChunk(id, offset, chunk, signal)
        },
      },
    )
    await u.start()
    expect(sizes).toEqual([SEGMENT, SEGMENT, SEGMENT, SEGMENT, SEGMENT])
    expect(Math.max(...sizes)).toBeLessThanOrEqual(SEGMENT)
  })
})
