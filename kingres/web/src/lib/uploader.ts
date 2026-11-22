// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The resumable uploader of a CSV file (contract 0.19.0: `createDataUpload`, `uploadDataChunk`, `getDataUpload`,
// `deleteDataUpload`). The file is sent in `Blob.slice` chunks of 64 MiB, one at a time, in order, so the offset the server
// holds is the only rule of ordering and a break loses at most one chunk. A chunk is never read whole into a variable: the
// slice is the body of the request. After a break, a conflict or a failure the offset is read again from the server (which
// reads it from the staged file), never guessed. The session is checked between chunks; when the server says it ended, the
// uploader stops where it is and waits for the password (`phase: 'password'`), and goes on from the server's offset. The
// cap of an import (2 GiB of data; a compressed file is checked by the server after expansion) is enforced before the first
// byte is sent. Nothing here reads the file's content.
import { ApiError, isSessionLost } from '@/api/problem'
import type { CreateDataUploadRequest, DataUpload } from '@/api/types'
import { CHUNK_ALIGN, CHUNK_BYTES, MAX_ATOMIC_BYTES } from './transfer'

export type UploadPhase =
  | 'idle'
  | 'starting'
  | 'sending'
  | 'paused'
  /** The server said the session ended: waiting for the password, then `continueAfterSignIn`. */
  | 'password'
  | 'complete'
  | 'failed'
  | 'cancelled'

export type UploadFailure =
  | 'empty'
  | 'too_large'
  | 'size_differs'
  | 'lost'
  | 'refused'
  | 'quota'
  | 'checksum'
  | 'rejected'
  | 'network'
  /** Another request still writes this upload and did not let go within the tries. */
  | 'busy'
  | 'other'

export interface UploadSnapshot {
  phase: UploadPhase
  /** Bytes the server holds (read from the server, not counted here). */
  offset: number
  size: number
  uploadId: string | null
  failure: UploadFailure | null
  /** The refusal the server gave, for a failure that has one. */
  problem: ApiError | null
  /** The SHA-256 the server computed when the last byte was in. */
  sha256: string | null
  /** Breaks survived and resumed in this run (a chunk that was cut, a conflict). */
  resumes: number
  /** True when this upload went on from an upload that already existed. */
  continued: boolean
  /** A cancel could not delete the upload on the server (the session ended, the server was not reached): it stays listed. */
  deletePending: boolean
}

export interface UploaderApi {
  createDataUpload: (body: CreateDataUploadRequest) => Promise<DataUpload>
  getDataUpload: (uploadId: string) => Promise<DataUpload>
  uploadDataChunk: (
    uploadId: string,
    offset: number,
    chunk: Blob,
    signal?: AbortSignal,
  ) => Promise<{ offset: number; conflict: boolean }>
  deleteDataUpload: (uploadId: string) => Promise<unknown>
  /** Asks the server whether the session is alive; throws the 401 as an error. */
  refreshSession: () => Promise<unknown>
}

/** The part of a `File` the uploader needs (tests give a plain object). */
export interface UploadSource {
  name: string
  size: number
  lastModified: number
  slice(start: number, end: number): Blob
}

export interface UploaderOptions {
  file: UploadSource
  api: UploaderApi
  /** Chunk size; a multiple of 64 KiB. Default 64 MiB. */
  chunkBytes?: number
  /** The most data of one import (default 2 GiB). Not applied to a compressed file, which the server counts after expansion. */
  maxAtomicBytes?: number
  /** The largest staged file of the cluster (`dataTransfer.maxFileBytes`), when known. */
  maxFileBytes?: number | null
  compressed?: boolean
  onChange?: (snapshot: UploadSnapshot) => void
  /** Called with the id when an upload exists, and with null when it is finished or gone (the tab's memory of it). */
  onUpload?: (uploadId: string | null) => void
  /** Consecutive failures without progress before the uploader stops (the person can resume). */
  maxFailures?: number
  sleep?: (ms: number) => Promise<void>
  /** Check the session between chunks (default true). */
  checkSession?: boolean
}

const defaultSleep = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms))

/** The chunk size to use: the wish of the console, cut to what the server accepts, aligned to the segment. */
export function chunkSize(wish: number, serverMax: number, align = CHUNK_ALIGN): number {
  const size = Math.min(wish, serverMax)
  return Math.max(align, Math.floor(size / align) * align)
}

export class Uploader {
  private readonly o: UploaderOptions
  private state: UploadSnapshot
  private upload: DataUpload | null = null
  private controller: AbortController | null = null
  /** Counts runs: a run that is no longer the current one stops at its next step. */
  private run = 0
  private wantPause = false

  constructor(options: UploaderOptions) {
    this.o = options
    this.state = {
      phase: 'idle',
      offset: 0,
      size: options.file.size,
      uploadId: null,
      failure: null,
      problem: null,
      sha256: null,
      resumes: 0,
      continued: false,
      deletePending: false,
    }
  }

  get snapshot(): UploadSnapshot {
    return this.state
  }

  private set(patch: Partial<UploadSnapshot>): void {
    this.state = { ...this.state, ...patch }
    this.o.onChange?.(this.state)
  }

  private fail(failure: UploadFailure, problem: ApiError | null = null): void {
    this.set({ phase: 'failed', failure, problem })
  }

  /**
   * Starts the upload, or goes on with an upload that exists (`existing`: the id the tab remembered). Resolves when the
   * upload is complete, paused, waiting for the password, cancelled or failed; `snapshot` says which.
   */
  async start(existing?: string): Promise<void> {
    const file = this.o.file
    if (this.state.phase === 'sending' || this.state.phase === 'starting') return
    this.set({ failure: null, problem: null })
    if (file.size < 1) return this.fail('empty')
    // The cap is enforced here, before a byte is sent: a file above it is refused, not uploaded and then refused.
    if (!this.o.compressed && file.size > (this.o.maxAtomicBytes ?? MAX_ATOMIC_BYTES))
      return this.fail('too_large')
    if (this.o.maxFileBytes && file.size > this.o.maxFileBytes) return this.fail('too_large')
    const mine = ++this.run
    this.wantPause = false
    this.set({ phase: 'starting' })
    try {
      if (existing !== undefined) {
        const found = await this.read(existing, true)
        if (found === 'gone') return this.startNew(mine)
        if (found === 'stopped') return
      } else return this.startNew(mine)
      return await this.send(mine)
    } catch (error) {
      return this.stopOn(error)
    }
  }

  /** The upload that was remembered is gone (expired, deleted, lost): a new one is started and the person is told. */
  private async startNew(mine: number): Promise<void> {
    try {
      this.upload = await this.o.api.createDataUpload({
        purpose: 'tableImport',
        sizeBytes: this.o.file.size,
        fileName: this.o.file.name,
      })
      this.o.onUpload?.(this.upload.uploadId)
      this.set({ uploadId: this.upload.uploadId, offset: this.upload.offset, continued: false })
      return await this.send(mine)
    } catch (error) {
      return this.stopOn(error)
    }
  }

  /** Reads the upload from the server and takes its offset. `gone`: it does not exist (any more). `stopped`: the run ended. */
  private async read(id: string, initial = false): Promise<'ok' | 'gone' | 'stopped'> {
    try {
      const found = await this.o.api.getDataUpload(id)
      if (found.state === 'lost' || found.state === 'deleted') return 'gone'
      if (found.sizeBytes !== this.o.file.size) {
        // Another file of the same name: it is never continued.
        this.fail('size_differs')
        return 'stopped'
      }
      this.upload = found
      this.set({
        uploadId: found.uploadId,
        offset: found.offset,
        sha256: found.sha256 ?? null,
        ...(initial ? { continued: found.offset > 0 } : {}),
      })
      return 'ok'
    } catch (error) {
      if (error instanceof ApiError && error.status === 404) return 'gone'
      this.stopOn(error)
      return 'stopped'
    }
  }

  /** Turns an error that ended a request into the state the uploader goes to. */
  private stopOn(error: unknown): void {
    if (this.wantPause) return this.set({ phase: 'paused' })
    if (!(error instanceof ApiError)) return this.fail('other')
    if (isSessionLost(error)) return this.set({ phase: 'password', problem: error })
    if (error.status === 403) return this.fail('refused', error)
    if (error.status === 404) return this.fail('lost', error)
    if (error.code === 'transfer_quota_exceeded') return this.fail('quota', error)
    if (error.code === 'upload_checksum_mismatch') return this.fail('checksum', error)
    if (error.status === 413 || error.status === 422 || error.status === 400)
      return this.fail(
        error.code === 'validation_failed' &&
          error.problem.errors?.some((e) => e.field === '/sizeBytes')
          ? 'too_large'
          : 'rejected',
        error,
      )
    return this.fail(error.synthetic || error.status >= 500 ? 'network' : 'other', error)
  }

  private async send(mine: number): Promise<void> {
    const sleep = this.o.sleep ?? defaultSleep
    const maxFailures = this.o.maxFailures ?? 5
    const file = this.o.file
    let failures = 0
    this.set({ phase: 'sending' })
    while (this.run === mine) {
      const up = this.upload
      if (up === null) return this.fail('other')
      if (this.state.offset >= file.size) break
      if (this.wantPause) return this.set({ phase: 'paused' })
      const step = chunkSize(this.o.chunkBytes ?? CHUNK_BYTES, up.chunkBytesMax, up.chunkAlign)
      const from = this.state.offset
      const to = Math.min(file.size, from + step)
      this.controller = new AbortController()
      try {
        const answer = await this.o.api.uploadDataChunk(
          up.uploadId,
          from,
          file.slice(from, to),
          this.controller.signal,
        )
        this.controller = null
        // An answer that moves the offset forward and not past the end is progress. Anything else (a 409 with the offset
        // where it was, a 400 that carries it, a 204 that did not advance) means the server is somewhere else, or another
        // request still writes the upload: ask it, and wait before the next try, with a cap.
        if (answer.conflict || answer.offset <= from || answer.offset > file.size) {
          this.set({ resumes: this.state.resumes + 1 })
          const r = await this.read(up.uploadId)
          if (r === 'gone') return this.fail('lost')
          if (r === 'stopped') return
          if (this.state.offset > from) {
            failures = 0
            continue
          }
          failures += 1
          if (failures > maxFailures) return this.fail('busy')
          await sleep(Math.min(8000, 500 * 2 ** failures))
          continue
        }
        failures = 0
        this.set({ offset: answer.offset })
      } catch (error) {
        this.controller = null
        if (this.run !== mine) return
        if (this.wantPause) return this.set({ phase: 'paused' })
        const retryable =
          error instanceof ApiError &&
          !isSessionLost(error) &&
          (error.synthetic || error.status >= 500)
        if (!retryable) return this.stopOn(error)
        // A break: the whole segments of the chunk that arrived are kept by the server. Ask where it is, and go on.
        failures += 1
        if (failures > maxFailures) return this.stopOn(error)
        await sleep(Math.min(8000, 500 * 2 ** failures))
        if (this.run !== mine) return
        const before = this.state.offset
        const r = await this.read(up.uploadId)
        if (r === 'gone') return this.fail('lost')
        if (r === 'stopped') return
        if (this.state.offset > before) failures = 0
        this.set({ resumes: this.state.resumes + 1 })
        continue
      }
      // The session is checked between chunks, so that one that ends is noticed here and not in the middle of 64 MiB.
      if (this.state.offset < file.size && this.o.checkSession !== false) {
        try {
          await this.o.api.refreshSession()
        } catch (error) {
          if (isSessionLost(error))
            return this.set({ phase: 'password', problem: error as ApiError })
        }
      }
    }
    if (this.run !== mine) return
    // Every byte is in. The server computes the SHA-256 and says the upload is complete.
    const id = this.state.uploadId
    if (id === null) return this.fail('other')
    const finished = await this.read(id)
    if (finished === 'stopped') return
    if (finished === 'gone') return this.fail('lost')
    if (this.upload?.state !== 'complete') return this.fail('other')
    this.set({ phase: 'complete', sha256: this.upload.sha256 ?? null })
  }

  /** Stops after the chunk that is in flight is abandoned; the server keeps what it has. `start` or `resume` goes on. */
  pause(): void {
    if (this.state.phase !== 'sending' && this.state.phase !== 'starting') return
    this.wantPause = true
    this.controller?.abort()
    this.set({ phase: 'paused' })
  }

  /** Goes on from the offset the server holds. */
  async resume(): Promise<void> {
    const id = this.state.uploadId
    if (id === null) return this.start()
    this.state = { ...this.state, phase: 'idle' }
    return this.start(id)
  }

  /** The person signed in again: go on from the offset the server holds. */
  continueAfterSignIn(): Promise<void> {
    return this.resume()
  }

  /**
   * Stops and deletes the staged upload on the server. A missing upload (404) is gone, which is what was wanted. Any other
   * refusal (the session ended, the server cannot be reached) leaves the upload on the server with its room reserved: it is
   * not forgotten, `deletePending` says so, and the Transfers panel can delete it later.
   */
  async cancel(): Promise<void> {
    this.run += 1
    this.wantPause = true
    this.controller?.abort()
    const id = this.state.uploadId
    this.set({ phase: 'cancelled' })
    if (id === null) return
    try {
      await this.o.api.deleteDataUpload(id)
    } catch (error) {
      if (!(error instanceof ApiError && error.status === 404)) {
        this.set({ deletePending: true })
        return
      }
    }
    this.o.onUpload?.(null)
  }

  /** Releases the in-flight request without deleting the upload (the dialog was closed; the tab remembers the upload). */
  detach(): void {
    this.run += 1
    this.wantPause = true
    this.controller?.abort()
  }
}
