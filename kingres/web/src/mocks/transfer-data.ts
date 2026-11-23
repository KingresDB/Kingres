// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The data side of the table export and import of phase 14a in the mock (contract 0.19.0): the staged uploads and
// exports, the switches that make the mock break the way a network and a server do, and the fixtures. No logic of
// requests lives here; `transfer.ts` has it. This file imports no value from `state.ts`, so `state.ts` can create the
// state without a cycle.
import type { components } from '@/api/schema'

type S = components['schemas']

export const KiB = 1024
export const MiB = 1024 * KiB
export const GiB = 1024 * MiB

/** The tables the mock adds to the database `shop`, by the behaviour they trigger (see the design, "Mock API"). */
export const FIXTURE_TABLES = {
  /** The export of this table fails (a value that is not valid in the encoding). */
  failme: 'failme',
  /** An unfinished table task: both operations answer 409 `operation_in_progress`. */
  blockme: 'blockme',
  /** A distributed table whose groups are read at their own moments (`per-group`). */
  skewme: 'skewme',
  /** A table of another role whose read would run code of that role (`export_untrusted_code`). */
  othercode: 'othercode',
} as const

/** The files that make an import behave in a fixed way, by their file name (display name of the upload). */
export const FIXTURE_FILES = {
  /** The import fails with a whitelisted error that names a column and a line. */
  badrow: 'badrow.csv',
  /** The import is refused: the file is above 2 GiB. */
  toobig: 'toobig.csv',
  /** The import ends with `import_outcome_unknown`. */
  outcome: 'outcome.csv',
  /** The import is refused: the file has a line that is only a backslash and a dot. */
  endmarker: 'endmarker.csv',
  /** The header does not match the columns (`import_header_mismatch`). */
  header: 'header.csv',
} as const

/** The archives (contract 0.40.0) that make a validation answer in a fixed way, by their file name. */
export const FIXTURE_ARCHIVES = {
  /** The structure of the zip file is wrong: `archive_invalid`. */
  hostile: 'hostile.kdx.zip',
  /** The versions do not fit this cluster: `reasons` lists them. */
  old: 'old.kdx.zip',
  /** The archive has ledger tables: `needs.ledgerRestart`. */
  ledger: 'ledger.kdx.zip',
  /** A statement of the SQL is refused by the shape grammar: kind, position and fingerprint, never its text. */
  code: 'code.kdx.zip',
} as const

export interface UploadRec {
  upload: S['DataUpload']
  creator: string
  fileName: string | null
  /** What the upload is for (contract 0.40.0). */
  purpose: 'tableImport' | 'databaseArchive'
  /** The report of the last validation of an archive. */
  report: S['ArchiveReport'] | null
  /** The bytes held, when the file is small enough to keep (a mock does not store gigabytes). */
  data: Uint8Array[] | null
  /** The task that uses the upload (an import), or null. */
  usedBy: number | null
}

export interface ExportRec {
  meta: S['DataExport']
  creator: string
  /** The plaintext file; null while it is built and after it is deleted or expired. */
  content: Uint8Array | null
  /** The encoding of the file, for the charset of the download. */
  encoding?: string
}

export interface TransferState {
  nextUploadId: number
  nextExportId: number
  uploads: UploadRec[]
  exports: ExportRec[]
}

export function createTransferState(): TransferState {
  return { nextUploadId: 42, nextExportId: 57, uploads: [], exports: [] }
}

/**
 * What the mock does on purpose, for a test or for the demo. Each "once" switch is cleared when it has acted.
 * `resetTransferSwitches` puts everything back (the tests call it before each case).
 */
export const transferSwitches = {
  /** The request does not arrive over TLS (403 `insecure_transport`). */
  insecure: false,
  /** The largest chunk the server says it accepts. */
  chunkBytesMax: 64 * MiB,
  /** The number of `uploadDataChunk` requests since the reset. */
  chunkRequests: 0,
  /** The chunk request with this number (1 is the first) is answered 401 `session_expired`, once; the session stays. */
  loseSessionAtChunk: null as number | null,
  /** The next chunk request is cut after this many bytes: whole 64 KiB segments are kept, the answer is a network error. */
  breakChunkAfterBytes: null as number | null,
  /** The next this many chunk requests are answered 409 with the offset where it was: another request still writes the upload. */
  busyWriterChunks: 0,
  /** The next download request is answered 401 `session_expired`, once; the session of the mock stays. */
  sessionEndsOnDownload: false,
  /** A cut chunk (`breakChunkAfterBytes`) is answered 400 with `Upload-Offset` (a proxy cut the body but the answer came) instead of a dropped connection. */
  cutAnswers400: false,
  /** The next download stream ends with a network error after this many bytes (the headers promised the whole file). */
  breakDownloadAfterBytes: null as number | null,
  /** The next download stream ends cleanly, early, at this many bytes (a short file; the headers promised more). */
  truncateDownloadAt: null as number | null,
  /** Every download answers with the bytes of the whole file, also when it was asked for a range (a server that ignores `Range`). */
  ignoreRange: false,
  /** Every download of the export that is served answers with a changed ETag from the second request on. */
  changeEtagOnResume: false,
  /** The number of `downloadDataExport` requests since the reset. */
  downloadRequests: 0,
  /** The database export finds large objects (422 `large_objects_present`) unless it is acknowledged. */
  largeObjects: false,
  /** The role cannot read some tables: 422 `export_not_readable`. */
  unreadable: false,
  /** The next database export fails in its data step. */
  dbExportFails: false,
  /** The next database export fails with `schema_changed` (the fingerprint differs at the end). */
  schemaChanges: false,
}

export function resetTransferSwitches(): void {
  transferSwitches.insecure = false
  transferSwitches.chunkBytesMax = 64 * MiB
  transferSwitches.chunkRequests = 0
  transferSwitches.loseSessionAtChunk = null
  transferSwitches.breakChunkAfterBytes = null
  transferSwitches.busyWriterChunks = 0
  transferSwitches.sessionEndsOnDownload = false
  transferSwitches.cutAnswers400 = false
  transferSwitches.breakDownloadAfterBytes = null
  transferSwitches.truncateDownloadAt = null
  transferSwitches.ignoreRange = false
  transferSwitches.changeEtagOnResume = false
  transferSwitches.downloadRequests = 0
  transferSwitches.largeObjects = false
  transferSwitches.unreadable = false
  transferSwitches.dbExportFails = false
  transferSwitches.schemaChanges = false
}

/** The default `dataTransfer` setting of the mock: on, so that the console can be tried; tests turn it off. */
export function defaultDataTransferSettings(): S['DataTransferSettings'] {
  return {
    enabled: true,
    retentionHours: 24,
    maxFileBytes: 20 * GiB,
    stagingQuotaBytes: 100 * GiB,
    maxConcurrent: 2,
    maxAtomicBytes: 2 * GiB,
  }
}

/** The text of an exported file: a header and `rows` records, with the options that change the text. */
export function exportText(
  columns: string[],
  rows: number,
  opts: { delimiter: string; header: boolean; nullString: string; spreadsheetSafe: boolean },
): string {
  const d = opts.delimiter
  const lines: string[] = []
  if (opts.header) lines.push(columns.join(d))
  for (let i = 1; i <= rows; i += 1) {
    lines.push(
      columns
        .map((c, k) => {
          if (k === 0) return String(i)
          if (/created|time|date/.test(c))
            return `2026-01-01 00:${String(i % 60).padStart(2, '0')}:00+00`
          // The third record has a text that a spreadsheet would run as a formula.
          if (i === 3) return opts.spreadsheetSafe ? "'=1+1" : '=1+1'
          return i % 7 === 0 ? opts.nullString : `${c}-${i}`
        })
        .join(d),
    )
  }
  return `${lines.join('\n')}\n`
}
