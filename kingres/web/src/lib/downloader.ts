// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The download of an export file (contract 0.19.0, `downloadDataExport`). It reads the stream, writes it to a sink
// (a file the person chose, or memory for a small file), and when the network breaks it asks for the rest with `Range`
// from the byte it holds. It never says "done" for a file that is short: every answer is checked against the size the
// export record gives, the status and `Content-Range` of a resumed answer are checked, and an ETag that changed means
// the file is not the one that was started. What it reports is one of a few outcomes, in words that are true:
// `complete` only when every byte of the declared size arrived (and, for a file kept in memory, the SHA-256 matched).
import { ApiError, isSessionLost } from '@/api/problem'
import type { ExportStream } from '@/api/endpoints'
import { MEMORY_DOWNLOAD_BYTES } from './transfer'

/** Where the bytes go. `close` seals the file; `abort` throws away what was written. */
export interface DownloadSink {
  write(chunk: Uint8Array): Promise<void>
  close(): Promise<void>
  abort(): Promise<void>
}

/** Why a download did not complete. */
export type DownloadFailure =
  /** The file has expired (410). */
  | 'gone'
  /** The export does not exist (404): deleted, or another role's. */
  | 'not_found'
  /** The server cannot serve the range, or ignored it and the console could not skip to the byte. */
  | 'range'
  /** The file is not the one the download started with (ETag, size or SHA-256 differs). */
  | 'changed'
  /** The server ended the stream before the last byte, again and again. */
  | 'short'
  /** The server could not be reached again after the allowed number of tries. */
  | 'network'
  /** The session ended (401). The sink is left open and the bytes held are kept: after the sign-in the download goes on from them. */
  | 'session'
  /** The place the file is written to failed (the disk is full, the permission was withdrawn). Nothing is resumed. */
  | 'save'
  /** The server refused (the setting is off, the role is too low, no TLS). */
  | 'refused'
  /** The person cancelled. */
  | 'cancelled'
  /** Anything else. */
  | 'other'

export type DownloadOutcome =
  | {
      kind: 'complete'
      bytes: number
      /** True when the SHA-256 of the received bytes equals the export's; null when it could not be checked (a large file). */
      verified: boolean | null
      resumes: number
    }
  | { kind: 'failed'; reason: DownloadFailure; received: number; resumes: number; status?: number }

export interface DownloadOptions {
  /** The size the export record gives, in bytes (`DataExport.sizeBytes`). */
  expectedSize: number
  /** The SHA-256 of the export (`DataExport.sha256`), lower-case hexadecimal; the ETag of the file is this in quotes. */
  sha256: string | null
  /** Opens the file from a byte: `endpoints.downloadDataExport` bound to the export id. */
  open: (from: number, signal: AbortSignal) => Promise<ExportStream>
  sink: DownloadSink
  signal: AbortSignal
  onProgress?: (received: number) => void
  /** Bytes that are already in the sink (a download that goes on after a sign-in). */
  startAt?: number
  /** How many times the download is resumed after a break before it gives up. */
  maxResumes?: number
  /** Waits before a resume; replaceable so that tests do not wait. */
  pause?: (attempt: number) => Promise<void>
  /** Computes the SHA-256 of the received bytes (memory sinks); null when the browser cannot. */
  digest?: (() => Promise<string | null>) | null
}

const defaultPause = (attempt: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, Math.min(8000, 500 * 2 ** attempt)))

/** Reads `bytes first-last/size` of a `Content-Range`, or null. */
export function parseContentRange(
  value: string | null,
): { first: number; last: number; size: number } | null {
  const m = value === null ? null : /^bytes (\d{1,15})-(\d{1,15})\/(\d{1,15})$/.exec(value.trim())
  if (!m) return null
  return { first: Number(m[1]), last: Number(m[2]), size: Number(m[3]) }
}

function failureOf(error: unknown): DownloadFailure | 'break' {
  if (!(error instanceof ApiError)) return 'break'
  if (isSessionLost(error)) return 'session'
  if (error.status === 410) return 'gone'
  if (error.status === 404) return 'not_found'
  if (error.status === 416) return 'range'
  if (error.status === 403) return 'refused'
  // A request that never got an answer is a break; so is a server that is not there for a moment.
  if (error.synthetic || error.status === 503 || error.status === 502 || error.status === 504)
    return 'break'
  return 'other'
}

/** Runs a download to its end and says what happened. It never throws for a failed download. */
export async function runDownload(o: DownloadOptions): Promise<DownloadOutcome> {
  const maxResumes = o.maxResumes ?? 5
  const pause = o.pause ?? defaultPause
  const etag = o.sha256 === null ? null : `"${o.sha256}"`
  let received = o.startAt ?? 0
  let resumes = 0
  const fail = async (reason: DownloadFailure, status?: number): Promise<DownloadOutcome> => {
    await o.sink.abort().catch(() => undefined)
    return {
      kind: 'failed',
      reason,
      received,
      resumes,
      ...(status === undefined ? {} : { status }),
    }
  }
  /** The session ended: the sink stays open with the bytes it has, so that the download can go on after the sign-in. */
  const sessionEnded = (status?: number): DownloadOutcome => ({
    kind: 'failed',
    reason: 'session',
    received,
    resumes,
    ...(status === undefined ? {} : { status }),
  })
  /** A break: wait and ask again, or give up when the tries are used. */
  const breakOrGiveUp = async (reason: DownloadFailure): Promise<'again' | DownloadOutcome> => {
    if (o.signal.aborted) return fail('cancelled')
    if (resumes >= maxResumes) return fail(reason)
    resumes += 1
    await pause(resumes)
    return o.signal.aborted ? fail('cancelled') : 'again'
  }

  for (;;) {
    if (o.signal.aborted) return fail('cancelled')
    let stream: ExportStream
    try {
      stream = await o.open(received, o.signal)
    } catch (error) {
      if (o.signal.aborted) return fail('cancelled')
      const why = failureOf(error)
      if (why === 'break') {
        const next = await breakOrGiveUp('network')
        if (next === 'again') continue
        return next
      }
      if (why === 'session')
        return sessionEnded(error instanceof ApiError ? error.status : undefined)
      return fail(why, error instanceof ApiError ? error.status : undefined)
    }

    // What the server says about the answer must be what the record says about the file.
    if (etag !== null && stream.etag !== null && stream.etag !== etag) return fail('changed')
    let skip = 0
    if (received === 0) {
      if (stream.status === 206) {
        const r = parseContentRange(stream.contentRange)
        if (!r || r.first !== 0 || r.size !== o.expectedSize) return fail('changed')
      } else if (stream.contentLength !== null && stream.contentLength !== o.expectedSize) {
        return fail('changed')
      }
    } else if (stream.status === 206) {
      const r = parseContentRange(stream.contentRange)
      if (!r || r.first !== received) return fail('range')
      if (r.size !== o.expectedSize) return fail('changed')
      if (stream.contentLength !== null && stream.contentLength !== o.expectedSize - received)
        return fail('changed')
    } else if (stream.status === 200) {
      // The server ignored `Range` and sends the whole file: the bytes already written are skipped, not written twice.
      if (stream.contentLength !== null && stream.contentLength !== o.expectedSize)
        return fail('changed')
      skip = received
    } else return fail('range', stream.status)
    if (stream.body === null) return fail('short')

    const reader = stream.body.getReader()
    let skipped = 0
    let broke = false
    try {
      for (;;) {
        if (o.signal.aborted) {
          await reader.cancel().catch(() => undefined)
          return fail('cancelled')
        }
        const { done, value } = await reader.read()
        if (done) break
        let chunk = value
        if (skipped < skip) {
          const drop = Math.min(skip - skipped, chunk.length)
          skipped += drop
          chunk = chunk.subarray(drop)
          if (chunk.length === 0) continue
        }
        // More bytes than the record says: this is not the file that was started.
        if (received + chunk.length > o.expectedSize) {
          await reader.cancel().catch(() => undefined)
          return fail('changed')
        }
        try {
          await o.sink.write(chunk)
        } catch {
          // A failure of the file is not a break of the network: it is not resumed, and the person is told what it is.
          await reader.cancel().catch(() => undefined)
          return fail('save')
        }
        received += chunk.length
        o.onProgress?.(received)
      }
    } catch (error) {
      if (o.signal.aborted) return fail('cancelled')
      // A stream that fails half way: the bytes that arrived are kept, the rest is asked for.
      if (error instanceof ApiError && failureOf(error) !== 'break') {
        if (failureOf(error) === 'session') return sessionEnded(error.status)
        return fail(failureOf(error) as DownloadFailure)
      }
      broke = true
    }

    // Every byte arrived: a stream that failed after the last byte is not a reason to ask again (the server would answer 416).
    if (received === o.expectedSize) break
    // Short: the stream failed or ended before the declared size. Ask for the rest.
    const next = await breakOrGiveUp(broke ? 'network' : 'short')
    if (next !== 'again') return next
  }

  // Every byte of the declared size arrived. A file in memory is checked against the SHA-256 of the record.
  let verified: boolean | null = null
  if (o.sha256 !== null && o.digest) {
    const sum = await o.digest().catch(() => null)
    if (sum !== null) {
      verified = sum === o.sha256
      if (!verified) return fail('changed')
    }
  }
  try {
    await o.sink.close()
  } catch {
    // A full disk is often reported only here, when the file is sealed.
    return fail('save')
  }
  return { kind: 'complete', bytes: received, verified, resumes }
}

/** Collects the bytes in memory (a file of at most `MEMORY_DOWNLOAD_BYTES`); the person gets it as a download at the end. */
export class MemorySink implements DownloadSink {
  private parts: Uint8Array[] = []
  private size = 0
  private done = false

  async write(chunk: Uint8Array): Promise<void> {
    // A reader may reuse its buffer: keep a copy.
    if (this.size + chunk.length > MEMORY_DOWNLOAD_BYTES)
      throw new Error('The file is too large to keep in memory.')
    this.parts.push(chunk.slice())
    this.size += chunk.length
  }

  async close(): Promise<void> {
    this.done = true
  }

  async abort(): Promise<void> {
    this.parts = []
    this.size = 0
  }

  get closed(): boolean {
    return this.done
  }

  /** The bytes as one file. */
  blob(type = 'application/octet-stream'): Blob {
    return new Blob(this.parts as BlobPart[], { type })
  }

  /** SHA-256 of what was received in lower-case hexadecimal, or null when the browser has no Web Crypto. */
  async sha256(): Promise<string | null> {
    const subtle = globalThis.crypto?.subtle
    if (!subtle) return null
    const all = new Uint8Array(this.size)
    let at = 0
    for (const p of this.parts) {
      all.set(p, at)
      at += p.length
    }
    const sum = new Uint8Array(await subtle.digest('SHA-256', all))
    return Array.from(sum, (b) => b.toString(16).padStart(2, '0')).join('')
  }
}

/** A file the person chose with the browser's file picker; the bytes are written as they arrive, so the size is not limited by memory. */
export class FileSink implements DownloadSink {
  constructor(private readonly writable: FileSystemWritableFileStream) {}

  async write(chunk: Uint8Array): Promise<void> {
    await this.writable.write(chunk as unknown as FileSystemWriteChunkType)
  }

  async close(): Promise<void> {
    await this.writable.close()
  }

  async abort(): Promise<void> {
    await this.writable.abort().catch(() => undefined)
  }
}

/** How a file of `size` bytes can be saved here: to a file as it arrives, from memory, or only by the browser's own download. */
export type SaveMethod = 'file' | 'memory' | 'link'

/** The picker of the browser, when it has one (Chromium). */
export function canPickFile(): boolean {
  return (
    typeof window !== 'undefined' &&
    typeof (window as { showSaveFilePicker?: unknown }).showSaveFilePicker === 'function'
  )
}

export function chooseSaveMethod(size: number): SaveMethod {
  if (canPickFile()) return 'file'
  return size <= MEMORY_DOWNLOAD_BYTES ? 'memory' : 'link'
}

/** Asks the person where to save. Must run in the handler of a click. Null when the person cancels the picker. */
export async function pickFileSink(suggestedName: string): Promise<FileSink | null> {
  const pick = (
    window as unknown as {
      showSaveFilePicker: (o: { suggestedName: string }) => Promise<{
        createWritable: () => Promise<FileSystemWritableFileStream>
      }>
    }
  ).showSaveFilePicker
  try {
    const handle = await pick.call(window, { suggestedName })
    return new FileSink(await handle.createWritable())
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') return null
    throw error
  }
}
