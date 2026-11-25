// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The two operations that contract 0.29.0 added to the tag `Data transfer` for the mock API:
// `exportDatabase` (task `database.export`, a zip archive of a database) and `validateDatabaseArchive` (task
// `archive.validate`, the report of an uploaded archive). They behave like the server: the checks of the request come
// first (the large objects, the privileges of the role on every table, a table that is left out), the task writes one
// entry per table and reports the consistency label of each, and a validation never shows the text of a statement of an
// archive: a refused statement is a kind, a position and a fingerprint. The fixtures are the file names of
// `FIXTURE_ARCHIVES` and the switches `lobs`, `unreadable`, `dbfail` and `changed` of `transfer-switches.ts`.
import type { components } from '@/api/schema'
import type { OperationId } from '@/api/operations.generated'
import { accepted, problem, validationFailed } from './http'
import type { Ctx, Handler } from './http'
import { createTask, findTask, nowIso } from './state'
import type { DbRec, MockState } from './state'
import { sha256Hex } from './sha256'
import {
  FIXTURE_ARCHIVES,
  MiB,
  transferSwitches,
  type ExportRec,
  type UploadRec,
} from './transfer-data'
import {
  checkConcurrency,
  consistencyOf,
  db,
  guard,
  me,
  ownUpload,
  reserved,
  settings,
} from './transfer'

type S = components['schemas']
type Table = S['Table']
type Task = S['Task']

const body = <T>(ctx: Ctx): T => (ctx.body ?? {}) as T

/** The tables of the archive: user tables of user schemas (the mock lists no bucket partition). */
const userTables = (rec: DbRec): Table[] =>
  rec.tables.filter((t) => t.schema !== 'kingres' && t.schema !== 'pg_catalog')

/** What the role cannot read in the mock: every table but the first, and some schemas. The server names at most 20 objects. */
function unreadableObjects(rec: DbRec): string[] {
  const out = userTables(rec)
    .slice(1)
    .map((t) => `${t.schema}.${t.name}`)
  for (let i = out.length; i < 22; i += 1) out.push(`private_${String(i).padStart(2, '0')}.t`)
  return out.slice(0, 20)
}

/** The consistency labels of an archive: one snapshot for the whole database, or one label per table. */
function labels(rec: DbRec, tables: Table[]): S['TableConsistency'][] {
  const each = tables.map((t) => consistencyOf(rec, t))
  if (each.every((c) => c.level === 'single-snapshot'))
    return each.map((c) => ({ ...c, level: 'archive-single-snapshot' as const }))
  return each
}

/** A small file that starts like a zip archive: the mock keeps no real archive, the console never reads one. */
function archiveBytes(tables: number, text: string): Uint8Array {
  const head = new TextEncoder().encode(`PK\u0003\u0004${text}`)
  const out = new Uint8Array(head.length + 512 * Math.max(tables, 1))
  out.set(head)
  return out
}

function newArchiveExport(s: MockState, rec: DbRec, taskId: number, hours: number): ExportRec {
  const meta: S['DataExport'] = {
    exportId: String(s.transfer.nextExportId++),
    kind: 'databaseExport',
    state: 'building',
    database: rec.info.name,
    schema: null,
    table: null,
    sizeBytes: null,
    sha256: null,
    rows: null,
    consistency: null,
    rlsApplied: false,
    spreadsheetSafe: null,
    format: 'archive/1',
    taskId,
    createdAt: nowIso(),
    expiresAt: new Date(Date.now() + hours * 3_600_000).toISOString(),
    downloadedAt: null,
  }
  const e: ExportRec = { meta, creator: me(s), content: null }
  s.transfer.exports.unshift(e)
  return e
}

/** The warnings of the manifest: what the archive does not carry and how the tables were read. */
function warningsOf(rec: DbRec, tables: Table[], lost: boolean, left: string[]): string[] {
  const w: string[] = [
    'Roles, tablespaces and the placement of buckets are not part of the archive.',
  ]
  for (const t of tables) {
    const c = consistencyOf(rec, t)
    if (c.level === 'per-group')
      w.push(`table ${c.table} was read per group (${c.groups} groups), not as one snapshot`)
  }
  if (lost) w.push('Large objects were present in the source and are not part of the archive.')
  for (const k of left)
    w.push(`the rows of table ${k} are not in the archive (left out by the request)`)
  return w
}

function seal(
  e: ExportRec,
  rec: DbRec,
  tables: Table[],
  b: S['ExportDatabaseRequest'],
  lost: boolean,
): void {
  const none = b.dataMode === 'none'
  const left = new Set(
    none
      ? tables.map((t) => `${t.schema}.${t.name}`)
      : (b.excludeTables ?? []).map((t) => `${t.schema}.${t.table}`),
  )
  const rows = tables.reduce(
    (n, t) => n + (left.has(`${t.schema}.${t.name}`) ? 0 : Math.min(t.estimatedRows ?? 50, 300)),
    0,
  )
  const content = archiveBytes(tables.length, `manifest ${rec.info.name}`)
  e.content = content
  e.meta.state = 'ready'
  e.meta.sizeBytes = content.length
  e.meta.sha256 = sha256Hex(content)
  e.meta.rows = rows
  e.meta.consistency = labels(rec, tables)
  e.meta.warnings = warningsOf(rec, tables, lost, none ? [] : [...left])
}

const GOOD_VERSIONS: S['ArchiveReport']['versions'] = {
  postgresMajor: 18,
  kingresVersion: '0.29.0',
  extensions: [
    { name: 'kingres_shard', version: '0.7', schema: 'kingres', installed: '0.7' },
    { name: 'citext', version: '1.6', schema: 'public', installed: '1.6' },
  ],
  encoding: 'UTF8',
  lcCollate: 'en_US.UTF-8',
  lcCtype: 'en_US.UTF-8',
}

/** The report a fixture file makes the validation produce; any other file is a good archive. */
export function reportFor(name: string | null, sha256: string): S['ArchiveReport'] {
  const good: S['ArchiveReport'] = {
    valid: true,
    format: 'kingres-export/1',
    sha256,
    versions: structuredClone(GOOD_VERSIONS),
    tables: {
      local: 3,
      hash: 2,
      reference: 1,
      ledger: 0,
      columnar: 0,
      rows: 23_000,
      bytes: 41 * MiB,
    },
    statements: {
      byKind: {
        'CREATE TABLE': 6,
        'CREATE INDEX': 4,
        'ALTER TABLE': 5,
        'CREATE VIEW': 1,
        'COMMENT ON': 2,
      },
      refused: [],
    },
    needs: { ledgerRestart: false, largeObjectsLost: false },
    warnings: ['table public.orders was read per group (3 groups), not as one snapshot'],
    reasons: [],
  }
  switch (name) {
    case FIXTURE_ARCHIVES.hostile:
      return {
        ...good,
        valid: false,
        format: '',
        versions: {
          postgresMajor: 0,
          kingresVersion: '',
          extensions: [],
          encoding: '',
          lcCollate: '',
          lcCtype: '',
        },
        tables: { local: 0, hash: 0, reference: 0, ledger: 0, columnar: 0, rows: 0, bytes: 0 },
        statements: { byKind: {}, refused: [] },
        warnings: [],
        reasons: [
          {
            code: 'archive_invalid',
            object: 'data/00003.csv',
            message: 'The local header of this entry does not agree with the central directory.',
          },
        ],
      }
    case FIXTURE_ARCHIVES.old:
      return {
        ...good,
        valid: false,
        versions: {
          ...good.versions,
          postgresMajor: 19,
          extensions: [
            { name: 'kingres_shard', version: '0.9', schema: 'kingres', installed: '0.7' },
            { name: 'kingres_timepart', version: '0.2', schema: 'public', installed: null },
          ],
        },
        reasons: [
          {
            code: 'archive_incompatible',
            object: 'postgres',
            message: 'The archive is from PostgreSQL 19 and this cluster runs 18.',
          },
          {
            code: 'archive_incompatible',
            object: 'kingres_shard',
            message: 'Version 0.9 is needed; 0.7 is installed.',
          },
          {
            code: 'archive_incompatible',
            object: 'kingres_timepart',
            message: 'The extension is not installed.',
          },
        ],
      }
    case FIXTURE_ARCHIVES.ledger:
      return {
        ...good,
        tables: { ...good.tables, ledger: 2 },
        needs: { ledgerRestart: true, largeObjectsLost: false },
      }
    case FIXTURE_ARCHIVES.code:
      return {
        ...good,
        valid: false,
        statements: {
          byKind: { 'CREATE TABLE': 6, 'CREATE FUNCTION': 1, 'CREATE TRIGGER': 1 },
          refused: [
            {
              kind: 'CREATE FUNCTION',
              position: 'schema/pre-data.sql#14',
              fingerprint: 'length 212, sha256 3f9a1c0be2d7',
              reason:
                'Code in a schema is not part of an archive; use pg_dump for such a database.',
            },
            {
              kind: 'CREATE TRIGGER',
              position: 'schema/post-data.sql#3',
              fingerprint: 'length 96, sha256 a07be4410c55',
              reason:
                'Code in a schema is not part of an archive; use pg_dump for such a database.',
            },
          ],
        },
        reasons: [],
      }
    default:
      return good
  }
}

function validationRunning(s: MockState, uploadId: string): boolean {
  return s.tasks.some(
    (x) =>
      x.task.kind === 'archive.validate' &&
      x.task.state === 'running' &&
      x.task.subject?.id === uploadId,
  )
}

export function archiveHandlers(): Partial<Record<OperationId, Handler>> {
  return {
    exportDatabase: (c) => {
      const s = c.s
      guard(s)
      const rec = db(s, c.params.databaseName ?? '')
      const b = body<S['ExportDatabaseRequest']>(c)
      const tables = userTables(rec)
      const errors: { field: string; message: string }[] = []
      if (b.dataMode !== undefined && b.dataMode !== 'all' && b.dataMode !== 'none')
        errors.push({ field: 'dataMode', message: 'Use all or none.' })
      for (const [i, t] of (b.excludeTables ?? []).entries())
        if (!tables.some((x) => x.schema === t.schema && x.name === t.table))
          errors.push({ field: `excludeTables/${i}`, message: 'The database has no such table.' })
      if (
        b.retentionHours !== undefined &&
        (!Number.isInteger(b.retentionHours) || b.retentionHours < 1 || b.retentionHours > 168)
      )
        errors.push({ field: 'retentionHours', message: 'Between 1 and 168 hours.' })
      if (errors.length) throw validationFailed(errors)
      // The role must read every table, whatever is left out of the data (the archive holds all the structure).
      if (transferSwitches.unreadable || rec.info.name === 'reports') {
        const names = unreadableObjects(rec)
        throw problem(
          'export_not_readable',
          `The role cannot read ${names.length} objects; the first ones are: ${names.join(', ')}.`,
        )
      }
      if (transferSwitches.largeObjects && b.acknowledgeLargeObjectsLost !== true)
        throw problem(
          'large_objects_present',
          'The database has 3 large objects. acknowledgeLargeObjectsLost accepts that they are not carried.',
        )
      checkConcurrency(s, rec.info.name)
      if (reserved(s) + 4 * MiB > settings(s).stagingQuotaBytes)
        throw problem('transfer_quota_exceeded', 'The staging area has no room for this export.')
      const hours = b.retentionHours ?? settings(s).retentionHours
      const taskId = s.nextTaskId
      const e = newArchiveExport(s, rec, taskId, hours)
      const lost = transferSwitches.largeObjects
      const fail = transferSwitches.dbExportFails
        ? {
            code: 'sql_error',
            message: 'The database refused the statement (SQLSTATE 42501), table "failme".',
          }
        : transferSwitches.schemaChanges
          ? {
              code: 'schema_changed',
              message: 'A table or a column changed while the export ran. No file was kept.',
            }
          : null
      transferSwitches.dbExportFails = false
      transferSwitches.schemaChanges = false
      const total = tables.length
      const task = createTask(s, {
        kind: 'database.export',
        subject: { type: 'database', id: rec.info.name },
        steps: [
          'Check the request',
          'Take the structure',
          'Normalise the schema',
          'Write the data',
          'Check the archive',
          'Seal',
        ],
        detail: `${total} tables`,
        cancellable: true,
        onProgress: (fraction) => {
          // Not `findTask`: it advances the tasks, which called this.
          const own = s.tasks.find((x) => x.task.id === taskId)
          if (!own || own.task.state !== 'running' || total === 0) return
          // The data step is the fourth of six: from half to two thirds of the run. Its detail names the table.
          if (fraction >= 0.5 && fraction < 0.67) {
            const at = Math.min(total - 1, Math.floor(((fraction - 0.5) / 0.17) * total))
            const t = tables[at]
            if (t) own.task.detail = `${t.schema}.${t.name} (${at + 1} of ${total})`
          }
        },
        ...(fail
          ? {
              failAt: { fraction: 0.7, ...fail },
              onFail: () => {
                e.meta.state = 'failed'
                e.content = null
              },
              apply: () => undefined,
            }
          : {
              apply: () => {
                seal(e, rec, tables, b, lost)
                const own = findTask(s, taskId)
                if (own) own.task.detail = `transfer ${e.meta.exportId}`
              },
            }),
      })
      return accepted(task)
    },

    validateDatabaseArchive: (c) => {
      const s = c.s
      guard(s)
      const u: UploadRec = ownUpload(c)
      if (u.purpose !== 'databaseArchive')
        throw validationFailed([
          { field: 'uploadId', message: 'This upload is not a database archive.' },
        ])
      if (u.upload.state === 'receiving')
        throw problem('upload_incomplete', 'The upload has not received all its bytes.')
      if (u.usedBy !== null || validationRunning(s, u.upload.uploadId))
        throw problem('operation_in_progress', 'A task uses this upload.')
      const sha = u.upload.sha256 ?? ''
      u.upload.state = 'validating'
      const task = createTask(s, {
        kind: 'archive.validate',
        // The contract's Task.subject.type has no value for an upload (its README says `upload`): the mock says what the README says.
        subject: { type: 'upload', id: u.upload.uploadId } as unknown as Task['subject'],
        steps: ['Check the upload', 'Read the archive', 'Check the plan', 'Store the report'],
        detail: `${u.upload.sizeBytes} bytes`,
        cancellable: true,
        onFail: () => {
          u.upload.state = 'complete'
          u.usedBy = null
        },
        apply: () => {
          u.report = reportFor(u.fileName, sha)
          u.upload.state = u.report.valid ? 'valid' : 'invalid'
          u.usedBy = null
        },
      })
      u.usedBy = task.id
      return accepted(task)
    },
  }
}
