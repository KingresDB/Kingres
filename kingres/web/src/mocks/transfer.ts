// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The ten operations that contract 0.19.0 added to the tag `Data transfer` (table export and import, resumable uploads,
// downloads) for the mock API. They behave like the server: an upload has an offset that is read from what is held,
// a chunk at another offset is 409 with the right one in the header, a non-final chunk is a multiple of 64 KiB, a
// download answers 200 or 206 with `Content-Length`, `Content-Range` and an ETag, a file that expired is 410 and a
// range past the end is 416. The switches in `transfer-data.ts` make it break on purpose (a cut chunk, a lost
// session, a download that ends early). No value of a file is ever put into an error: the texts below are the fixed
// sentences the server would send. The answers validate against the schemas of the contract (tests/transfer-mock-0-19.test.ts).
import type { components } from '@/api/schema'
import type { OperationId } from '@/api/operations.generated'
import { accepted, created, noContent, ok, paginate, problem, validationFailed } from './http'
import type { Ctx, Handler, Reply } from './http'
import { createTask, findTask, nowIso } from './state'
import type { DbRec, MockState } from './state'
import { hex64 } from './ledger-data'
import { sha256Hex } from './sha256'
import {
  FIXTURE_FILES,
  FIXTURE_TABLES,
  GiB,
  MiB,
  exportText,
  transferSwitches,
  type ExportRec,
  type UploadRec,
} from './transfer-data'

type S = components['schemas']
type Table = S['Table']

const body = <T>(ctx: Ctx): T => (ctx.body ?? {}) as T
const SEGMENT = 65536
/** Files of at most this size are kept in memory; a larger one is only counted. */
const KEEP_BYTES = 8 * MiB
const MAX_ATOMIC = 2 * GiB

function db(s: MockState, name: string): DbRec {
  const rec = s.databases.find((d) => d.info.name === name)
  if (!rec) throw problem('not_found', `Database "${name}" does not exist.`)
  return rec
}

function settings(s: MockState): S['DataTransferSettings'] {
  const d = s.settings.dataTransfer
  if (!d) throw new Error('The mock cluster has no data transfer settings.')
  return d
}

/** The guards every transfer operation has: the transport and the cluster setting. */
function guard(s: MockState): void {
  if (transferSwitches.insecure)
    throw problem('insecure_transport', 'The request did not arrive over TLS.')
  if (!settings(s).enabled)
    throw problem('data_transfer_disabled', 'The data transfer is turned off for this cluster.')
}

function me(s: MockState): string {
  return s.session?.username ?? 'admin'
}

function findTable(c: Ctx): { rec: DbRec; t: Table } {
  const rec = db(c.s, c.params.databaseName ?? '')
  const t = rec.tables.find(
    (x) => x.schema === c.params.schemaName && x.name === c.params.tableName,
  )
  if (!t || t.schema === 'kingres' || t.schema === 'pg_catalog')
    throw problem('not_found', 'Table does not exist.')
  return { rec, t }
}

const subjectOf = (c: Ctx): string =>
  `${c.params.databaseName}/${c.params.schemaName}.${c.params.tableName}`

/** The column names of a table (what `getTable` lists), by the same rule as the table handler. */
export type ColumnsOf = (rec: DbRec, t: Table) => string[]

/** Bytes reserved in the staging area: the declared size of every upload and the size of every export. */
function reserved(s: MockState): number {
  let sum = 0
  for (const u of s.transfer.uploads) sum += u.upload.sizeBytes
  for (const e of s.transfer.exports) sum += e.meta.sizeBytes ?? 4 * MiB
  return sum
}

const isTransfer = (kind: string): boolean => kind === 'table.export' || kind === 'table.import'

function checkConcurrency(s: MockState, subject: string): void {
  const busy = s.tasks.filter((t) => t.task.state === 'running' && isTransfer(t.task.kind))
  if (busy.some((t) => t.task.subject?.id === subject))
    throw problem('operation_in_progress', 'A transfer of this table is running.')
  if (busy.length >= settings(s).maxConcurrent)
    throw problem(
      'transfer_quota_exceeded',
      'The limit of transfers that run at once in the cluster is reached.',
    )
}

/** Sets an export that passed its retention time to `expired` and frees its file (as the janitor does). */
function sweep(s: MockState): void {
  const now = Date.now()
  for (const e of s.transfer.exports) {
    if (e.meta.state === 'ready' && Date.parse(e.meta.expiresAt) <= now) {
      e.meta.state = 'expired'
      e.content = null
    }
  }
}

function ownExport(c: Ctx): ExportRec {
  sweep(c.s)
  const e = c.s.transfer.exports.find((x) => x.meta.exportId === c.params.exportId)
  // Another role's file is 404: its existence is not leaked.
  if (!e || e.creator !== me(c.s)) throw problem('not_found', 'The export does not exist.')
  return e
}

function ownUpload(c: Ctx): UploadRec {
  const u = c.s.transfer.uploads.find((x) => x.upload.uploadId === c.params.uploadId)
  if (!u || u.creator !== me(c.s)) throw problem('not_found', 'The upload does not exist.')
  return u
}

const DELIMITERS = [',', ';', '\t', '|', ':']
const ENCODINGS = [
  'UTF8',
  'LATIN1',
  'LATIN2',
  'LATIN9',
  'WIN1250',
  'WIN1251',
  'WIN1252',
  'WIN1256',
  'ISO_8859_6',
  'SJIS',
  'EUC_JP',
  'GBK',
  'BIG5',
  'KOI8R',
]

interface Field {
  field: string
  message: string
}

function checkCsvOptions(
  b: Pick<S['ExportTableRequest'], 'delimiter' | 'nullString' | 'quote' | 'encoding'>,
  errors: Field[],
): void {
  if (b.delimiter !== undefined && !DELIMITERS.includes(b.delimiter))
    errors.push({
      field: 'delimiter',
      message: 'Use a comma, a semicolon, a tab, a bar or a colon.',
    })
  if (b.encoding !== undefined && !ENCODINGS.includes(b.encoding))
    errors.push({ field: 'encoding', message: 'This encoding is not offered.' })
  if (b.quote !== undefined && new TextEncoder().encode(b.quote).length !== 1)
    errors.push({ field: 'quote', message: 'The quote is one byte.' })
  if (b.nullString !== undefined) {
    const n = b.nullString
    if (
      new TextEncoder().encode(n).length > 32 ||
      /["\r\n]/.test(n) ||
      (b.quote !== undefined && n.includes(b.quote)) ||
      n.includes(b.delimiter ?? ',')
    )
      errors.push({
        field: 'nullString',
        message: 'At most 32 bytes, with no quote, delimiter, carriage return or line feed.',
      })
  }
}

/** The label the file guarantees for a table (the README of the contract, "Data transfer"). */
export function consistencyOf(rec: DbRec, t: Table): S['TableConsistency'] {
  const table = `${t.schema}.${t.name}`
  if (!t.classes.includes('distributed')) return { table, level: 'single-snapshot', groups: 1 }
  const buckets = rec.buckets.get(t.distribution?.colocationId ?? -1) ?? []
  const groups = new Set(buckets.map((b) => b.group)).size
  return groups > 1
    ? { table, level: 'per-group', groups }
    : { table, level: 'single-snapshot', groups: Math.max(groups, 1) }
}

function newExport(
  s: MockState,
  rec: DbRec,
  t: Table,
  b: S['ExportTableRequest'],
  taskId: number,
): ExportRec {
  const hours = b.retentionHours ?? settings(s).retentionHours
  const meta: S['DataExport'] = {
    exportId: String(s.transfer.nextExportId++),
    kind: 'tableExport',
    state: 'building',
    database: rec.info.name,
    schema: t.schema,
    table: t.name,
    sizeBytes: null,
    sha256: null,
    rows: null,
    consistency: null,
    rlsApplied: false,
    format: b.compression === 'gzip' ? 'csv.gz' : 'csv',
    taskId,
    createdAt: nowIso(),
    expiresAt: new Date(Date.now() + hours * 3_600_000).toISOString(),
    downloadedAt: null,
  }
  const e: ExportRec = { meta, creator: me(s), content: null }
  s.transfer.exports.unshift(e)
  return e
}

/** Seals an export: makes the file and fills what the task knows. */
function seal(
  e: ExportRec,
  rec: DbRec,
  t: Table,
  b: S['ExportTableRequest'],
  columns: string[],
): void {
  const rows = Math.min(t.estimatedRows ?? 50, 300)
  const text = exportText(columns, rows, {
    delimiter: b.delimiter ?? ',',
    header: b.header ?? true,
    nullString: b.nullString ?? '',
    spreadsheetSafe: b.spreadsheetSafe ?? true,
  })
  const bytes = new TextEncoder().encode(text)
  const prefix = b.utf8Bom && (b.encoding ?? 'UTF8') === 'UTF8' ? [0xef, 0xbb, 0xbf] : []
  const gzip = b.compression === 'gzip' ? [0x1f, 0x8b, 8, 0] : []
  const content = new Uint8Array(gzip.length + prefix.length + bytes.length)
  content.set(gzip)
  content.set(prefix, gzip.length)
  content.set(bytes, gzip.length + prefix.length)
  e.content = content
  e.meta.state = 'ready'
  e.meta.sizeBytes = content.length
  e.meta.sha256 = sha256Hex(content)
  e.meta.rows = rows
  e.meta.consistency = [consistencyOf(rec, t)]
}

/** What a fixture file makes the import do: the task fails with a whitelisted sentence (never a value). */
function importFailure(
  name: string | null,
): { code: string; message: string; fraction: number } | null {
  if (name === FIXTURE_FILES.badrow)
    return {
      code: 'sql_error',
      message:
        'A value does not have the type of its column (SQLSTATE 22P02), column "created_at", line 4. Nothing was stored.',
      fraction: 0.5,
    }
  if (name === FIXTURE_FILES.outcome)
    return {
      code: 'import_outcome_unknown',
      message:
        'The commit was sent and its outcome is not known. 3 rows may have been loaded. Check the table before you repeat the import.',
      fraction: 0.85,
    }
  return null
}

function concat(parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0))
  let at = 0
  for (const p of parts) {
    out.set(p, at)
    at += p.length
  }
  return out
}

/** 410: a file that expired (the convention of the bundle: status 410, code `not_found`). */
function gone(detail: string): Reply {
  return {
    status: 410,
    headers: { 'Content-Type': 'application/problem+json' },
    body: { type: 'about:blank', title: 'Gone', status: 410, code: 'not_found', detail },
  }
}

function unsatisfiable(size: number): Reply {
  return {
    status: 416,
    headers: { 'Content-Range': `bytes */${size}`, 'Content-Type': 'application/problem+json' },
    body: {
      type: 'about:blank',
      title: 'Range not satisfiable',
      status: 416,
      code: 'bad_request',
      detail: 'The range cannot be served.',
    },
  }
}

export function transferHandlers(columnsOf: ColumnsOf): Partial<Record<OperationId, Handler>> {
  return {
    exportTable: (c) => {
      const s = c.s
      guard(s)
      const { rec, t } = findTable(c)
      const b = body<S['ExportTableRequest']>(c)
      const errors: Field[] = []
      checkCsvOptions(b, errors)
      const all = columnsOf(rec, t)
      for (const col of b.columns ?? [])
        if (!all.includes(col))
          errors.push({ field: 'columns', message: 'This column does not exist.' })
      if (
        b.retentionHours !== undefined &&
        (!Number.isInteger(b.retentionHours) || b.retentionHours < 1 || b.retentionHours > 168)
      )
        errors.push({ field: 'retentionHours', message: 'Between 1 and 168 hours.' })
      if (b.utf8Bom && (b.encoding ?? 'UTF8') !== 'UTF8')
        errors.push({ field: 'utf8Bom', message: 'A byte order mark goes only with UTF8.' })
      if (errors.length) throw validationFailed(errors)
      if (t.name === FIXTURE_TABLES.blockme)
        throw problem('operation_in_progress', 'An operation on this table is running.')
      checkConcurrency(s, subjectOf(c))
      if (t.name === FIXTURE_TABLES.othercode && b.acknowledgeOtherRolesCode !== true)
        throw problem(
          'export_untrusted_code',
          'A generated column of this table calls a function of another role.',
        )
      if (reserved(s) + 4 * MiB > settings(s).stagingQuotaBytes)
        throw problem('transfer_quota_exceeded', 'The staging area has no room for this export.')
      const columns = b.columns?.length ? b.columns : all
      const taskId = s.nextTaskId
      const e = newExport(s, rec, t, b, taskId)
      const fail = t.name === FIXTURE_TABLES.failme
      const task = createTask(s, {
        kind: 'table.export',
        subject: { type: 'table', id: subjectOf(c) },
        steps: ['Check the request', 'Read the table class', 'Write the file', 'Seal'],
        detail: `about ${t.estimatedRows ?? 0} rows (an estimate)`,
        cancellable: true,
        ...(fail
          ? {
              failAt: {
                fraction: 0.6,
                code: 'sql_error',
                message:
                  'The data is not valid in the encoding (SQLSTATE 22021), column "created_at", line 12.',
              },
              onFail: () => {
                e.meta.state = 'failed'
                e.content = null
              },
              apply: () => undefined,
            }
          : {
              apply: () => {
                seal(e, rec, t, b, columns)
                const own = findTask(s, taskId)
                if (own) own.task.detail = `transfer ${e.meta.exportId}`
              },
            }),
      })
      return accepted(task)
    },

    importTable: (c) => {
      const s = c.s
      guard(s)
      const { rec, t } = findTable(c)
      const b = body<S['ImportTableRequest']>(c)
      const errors: Field[] = []
      if (typeof b.uploadId !== 'string' || b.uploadId === '')
        errors.push({ field: 'uploadId', message: 'Name the upload.' })
      checkCsvOptions(b, errors)
      const all = columnsOf(rec, t)
      for (const list of ['columns', 'forceNotNull', 'forceNull'] as const)
        for (const col of b[list] ?? [])
          if (!all.includes(col))
            errors.push({ field: list, message: 'This column does not exist.' })
      if (b.header !== undefined && !['none', 'skip', 'match'].includes(b.header))
        errors.push({ field: 'header', message: 'Use none, skip or match.' })
      if (errors.length) throw validationFailed(errors)
      const u = s.transfer.uploads.find((x) => x.upload.uploadId === b.uploadId)
      if (!u || u.creator !== me(s)) throw problem('not_found', 'The upload does not exist.')
      if (u.upload.state !== 'complete')
        throw problem('upload_incomplete', 'The upload has not received all its bytes.')
      if (t.name === FIXTURE_TABLES.blockme)
        throw problem('operation_in_progress', 'An operation on this table is running.')
      if (u.upload.sizeBytes > MAX_ATOMIC || u.fileName === FIXTURE_FILES.toobig)
        throw problem(
          'import_too_large',
          'The file is larger than 2 GiB. Split the file into parts of at most 2 GiB and import them one after the other.',
        )
      if (u.fileName === FIXTURE_FILES.endmarker)
        throw problem('import_end_marker', 'The file has an unquoted end marker on line 7.')
      if (u.fileName === FIXTURE_FILES.header && b.header === 'match')
        throw problem(
          'import_header_mismatch',
          'The first difference is at column 2: the file has "name", the table has "total".',
        )
      if (u.usedBy !== null) throw problem('operation_in_progress', 'A task uses this upload.')
      checkConcurrency(s, subjectOf(c))
      const loaded = u.data
        ? Math.max(
            0,
            u.data.reduce((n, part) => n + part.filter((byte) => byte === 10).length, 0) -
              (b.header === 'none' ? 0 : 1),
          )
        : 1000
      const failure = importFailure(u.fileName)
      u.usedBy = s.nextTaskId
      const task = createTask(s, {
        kind: 'table.import',
        subject: { type: 'table', id: subjectOf(c) },
        steps: ['Check the upload', 'Check the table', 'Load rows', 'Commit', 'Clean up'],
        detail: `${u.upload.sizeBytes} bytes`,
        cancellable: true,
        ...(failure
          ? {
              failAt: { fraction: failure.fraction, code: failure.code, message: failure.message },
              onFail: () => {
                u.usedBy = null
              },
              apply: () => undefined,
            }
          : {
              apply: () => {
                t.estimatedRows = (t.estimatedRows ?? 0) + loaded
                u.usedBy = null
                if (b.keepUpload !== true)
                  s.transfer.uploads = s.transfer.uploads.filter((x) => x !== u)
              },
            }),
      })
      return accepted(task)
    },

    createDataUpload: (c) => {
      const s = c.s
      guard(s)
      const b = body<S['CreateDataUploadRequest']>(c)
      const errors: Field[] = []
      if (b.purpose !== 'tableImport')
        errors.push({ field: 'purpose', message: 'Use tableImport.' })
      if (!Number.isInteger(b.sizeBytes) || b.sizeBytes < 1)
        errors.push({
          field: 'sizeBytes',
          message: 'The size is a whole number of bytes, at least 1.',
        })
      else if (b.sizeBytes > settings(s).maxFileBytes)
        errors.push({ field: '/sizeBytes', message: 'The size is above the largest staged file.' })
      if (b.sha256 !== undefined && !/^[0-9a-f]{64}$/.test(b.sha256))
        errors.push({ field: 'sha256', message: '64 lower-case hexadecimal characters.' })
      if (errors.length) throw validationFailed(errors)
      if (reserved(s) + b.sizeBytes > settings(s).stagingQuotaBytes)
        throw problem('transfer_quota_exceeded', 'The staging area has no room for this file.')
      const rec: UploadRec = {
        upload: {
          uploadId: String(s.transfer.nextUploadId++),
          state: 'receiving',
          sizeBytes: b.sizeBytes,
          offset: 0,
          chunkBytesMax: transferSwitches.chunkBytesMax,
          chunkAlign: 65536,
          sha256: null,
          expiresAt: new Date(Date.now() + 48 * 3_600_000).toISOString(),
        },
        creator: me(s),
        fileName: b.fileName ?? null,
        data: b.sizeBytes <= KEEP_BYTES ? [] : null,
        usedBy: null,
      }
      s.transfer.uploads.push(rec)
      return {
        ...created(structuredClone(rec.upload)),
        headers: { Location: `/api/v1/data-uploads/${rec.upload.uploadId}` },
      }
    },

    getDataUpload: (c) => {
      guard(c.s)
      return ok(structuredClone(ownUpload(c).upload))
    },

    uploadDataChunk: async (c): Promise<Reply> => {
      const s = c.s
      guard(s)
      const sw = transferSwitches
      sw.chunkRequests += 1
      if (sw.loseSessionAtChunk === sw.chunkRequests) {
        sw.loseSessionAtChunk = null
        throw problem('session_expired')
      }
      const u = ownUpload(c)
      const up = u.upload
      const asked = c.request.headers.get('Upload-Offset')
      if (asked === null || !/^\d{1,15}$/.test(asked))
        throw validationFailed([
          { field: 'Upload-Offset', message: 'Send the offset as a number.' },
        ])
      const bytes = new Uint8Array(await c.request.arrayBuffer())
      if (Number(asked) !== up.offset)
        return {
          status: 409,
          headers: {
            'Upload-Offset': String(up.offset),
            'Content-Type': 'application/problem+json',
          },
          body: {
            type: 'https://kingresdb.example/problems/upload-offset-mismatch',
            title: 'upload offset mismatch',
            status: 409,
            code: 'upload_offset_mismatch',
            detail: `The upload is at offset ${up.offset}.`,
          },
        }
      if (up.state !== 'receiving') throw problem('conflict', 'The upload is complete.')
      if (bytes.length > up.chunkBytesMax || up.offset + bytes.length > up.sizeBytes)
        throw problem('chunk_too_large', 'The chunk is larger than the server accepts.')
      const last = up.offset + bytes.length === up.sizeBytes
      if (bytes.length % SEGMENT !== 0 && !last)
        throw validationFailed([
          {
            field: 'Content-Length',
            message: 'A chunk is a multiple of 65536 bytes unless it is the last.',
          },
        ])
      // A request that breaks off keeps the whole sealed segments it delivered and drops the rest.
      let take = bytes
      if (sw.breakChunkAfterBytes !== null) {
        const whole = Math.floor(Math.min(sw.breakChunkAfterBytes, bytes.length) / SEGMENT)
        take = bytes.slice(0, whole * SEGMENT)
        sw.breakChunkAfterBytes = null
      }
      if (take.length > 0) {
        u.data?.push(take)
        up.offset += take.length
      }
      if (take !== bytes) return { status: 0, networkError: true }
      if (up.offset === up.sizeBytes) {
        up.state = 'complete'
        up.sha256 = u.data
          ? sha256Hex(concat(u.data))
          : hex64(`upload-${up.uploadId}-${up.sizeBytes}`)
        up.expiresAt = new Date(Date.now() + 24 * 3_600_000).toISOString()
      }
      return { status: 204, headers: { 'Upload-Offset': String(up.offset) } }
    },

    deleteDataUpload: (c) => {
      guard(c.s)
      const u = ownUpload(c)
      if (u.usedBy !== null) {
        const t = findTask(c.s, u.usedBy)
        if (t && t.task.state === 'running')
          throw problem('operation_in_progress', 'A task uses this upload.')
      }
      c.s.transfer.uploads = c.s.transfer.uploads.filter((x) => x !== u)
      return noContent()
    },

    listDataExports: (c) => {
      guard(c.s)
      sweep(c.s)
      const mine = c.s.transfer.exports.filter((e) => e.creator === me(c.s))
      return ok(paginate(structuredClone(mine.map((e) => e.meta)), c.url))
    },

    getDataExport: (c) => {
      guard(c.s)
      return ok(structuredClone(ownExport(c).meta))
    },

    deleteDataExport: (c) => {
      guard(c.s)
      sweep(c.s)
      // The creator or any administrator may delete (to free the disk of a colleague who left).
      const e = c.s.transfer.exports.find((x) => x.meta.exportId === c.params.exportId)
      if (!e) throw problem('not_found', 'The export does not exist.')
      c.s.transfer.exports = c.s.transfer.exports.filter((x) => x !== e)
      return noContent()
    },

    downloadDataExport: (c): Reply => {
      const s = c.s
      guard(s)
      const sw = transferSwitches
      sw.downloadRequests += 1
      const e = ownExport(c)
      if (e.meta.state === 'expired' || e.meta.state === 'deleted')
        return gone('The file has expired.')
      if (e.meta.state !== 'ready' || e.content === null)
        throw problem('not_found', 'The export has no file.')
      const all = e.content
      const size = all.length
      let first = 0
      let last = size - 1
      let partial = false
      const range = sw.ignoreRange ? null : c.request.headers.get('Range')
      if (range !== null) {
        const m = /^bytes=(\d*)-(\d*)$/.exec(range.trim())
        if (!m || (m[1] === '' && m[2] === '')) return unsatisfiable(size)
        if (m[1] === '') {
          const n = Number(m[2])
          if (n === 0) return unsatisfiable(size)
          first = Math.max(0, size - n)
        } else {
          first = Number(m[1])
          if (m[2] !== '') last = Math.min(last, Number(m[2]))
        }
        if (first >= size || first > last) return unsatisfiable(size)
        partial = true
      }
      const slice = all.slice(first, last + 1)
      const tag =
        sw.changeEtagOnResume && sw.downloadRequests > 1 ? hex64('other') : (e.meta.sha256 ?? '')
      const ext = e.meta.format === 'csv.gz' ? 'csv.gz' : 'csv'
      const headers: Record<string, string> = {
        'Content-Type': e.meta.format === 'csv.gz' ? 'application/gzip' : 'text/csv; charset=utf-8',
        'Content-Length': String(slice.length),
        'Accept-Ranges': 'bytes',
        ETag: `"${tag}"`,
        'Content-Disposition': `attachment; filename="${e.meta.database}-${e.meta.table}.${ext}"`,
        'Cache-Control': 'no-store',
        'X-Content-Type-Options': 'nosniff',
        ...(partial ? { 'Content-Range': `bytes ${first}-${last}/${size}` } : {}),
      }
      const status = partial ? 206 : 200
      const cutBreak = sw.breakDownloadAfterBytes
      const cutShort = sw.truncateDownloadAt
      sw.breakDownloadAfterBytes = null
      sw.truncateDownloadAt = null
      if (cutBreak !== null || cutShort !== null) {
        const cut = Math.min(cutBreak ?? cutShort ?? slice.length, slice.length)
        const fail = cutBreak !== null
        return {
          status,
          headers,
          stream: new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(slice.slice(0, cut))
              if (fail) controller.error(new TypeError('network error'))
              else controller.close()
            },
          }),
        }
      }
      if (last === size - 1) e.meta.downloadedAt = nowIso()
      return { status, headers, rawBody: slice }
    },
  }
}
