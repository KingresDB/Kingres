// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// `importDatabase` (contract 0.41.0, a placeholder revision, task `database.import`) for the mock API. It behaves like the
// server: the request is checked first (the role, the upload, the name, the archive read again from its bytes, the ledger
// acknowledgement), the task walks the thirteen steps and shows its progress in `detail` during the load, and it ends with
// the new database in the list of databases. A failed task has dropped its private database, and says so. The text of a
// statement of an archive is never part of an answer: a refused statement is a kind, a position and a fingerprint. The
// fixtures are the file names of `FIXTURE_ARCHIVES`; `?transfer=superuser` makes the role dangerous.
import type { components } from '@/api/schema'
import type { OperationId } from '@/api/operations.generated'
import { accepted, problem, validationFailed } from './http'
import type { Ctx, Handler } from './http'
import { createTask } from './state'
import type { DbRec, MockState } from './state'
import { FIXTURE_ARCHIVES, MiB, transferSwitches } from './transfer-data'
import { checkConcurrency, guard, me } from './transfer'
import { reportFor } from './transfer-archive'

type S = components['schemas']

/** The steps of the task, as the contract lists them (one more step for each other server follows the second). */
export const IMPORT_STEPS = [
  'Check the archive',
  'Create the private database',
  'Install the extensions',
  'Check the functions',
  'Create the tables',
  'Distribute the tables',
  'Create the views',
  'Load the data',
  'Create the indexes and constraints',
  'Set the sequences',
  'Enable the ledger and the policies',
  'Verify',
  'Take the final name',
] as const

const NAME_RE = /^[a-z_][a-z0-9_]{0,62}$/

/** The text of the refusal of a dangerous role (the workbench says the same). */
export const DANGEROUS_ROLE_TEXT =
  'An import runs the statements of the archive as the signed-in role, so it is refused for a role with the SUPERUSER, REPLICATION or BYPASSRLS attribute or with access to server files, programs or logs; sign in as another administrator.'

/** The tables the load step goes through in the mock. */
const LOAD_TABLES = ['public.customers', 'public.orders', 'public.items', 'app.events']

function newDb(s: MockState, name: string, ledger: boolean): DbRec {
  const info: S['Database'] = {
    name,
    status: 'ready',
    general: {
      owner: me(s),
      encoding: 'UTF8',
      lcCollate: 'en_US.UTF-8',
      lcCtype: 'en_US.UTF-8',
      connectionLimit: -1,
    },
    sharding: { enabled: true, bucketCount: 128, mode: 'row', distributeNewSchemas: false },
    replication: {
      durability: s.settings.defaultDurability,
      replicaCount: s.settings.defaultReplicaCount,
    },
    ledger: { mode: ledger ? 'optIn' : 'off', digestIntervalSeconds: 3600 },
    stats: { sizeBytes: 41 * MiB, tableCount: 0, activeConnections: 0, transactionsPerSecond: 0 },
    health: { underReplicatedBuckets: 0, unavailableBuckets: 0 },
    ledgerSummary: { verificationState: 'never', lastVerifiedBlockHeight: null },
    rebalanceRequired: false,
    createdBy: me(s),
    createdAt: new Date().toISOString(),
  } as S['Database']
  return { info, tables: [], colocations: [], buckets: new Map() } as unknown as DbRec
}

/** The name a file suggests when the request names no database (the server takes the name in the manifest). */
export function nameFromFile(fileName: string | null): string {
  const base = (fileName ?? 'imported').replace(/\.kdx\.zip$|\.zip$/i, '').toLowerCase()
  const clean = base.replace(/[^a-z0-9_]/g, '_').replace(/^[0-9]/, '_$&')
  return (clean || 'imported').slice(0, 63)
}

function refuseByReport(report: S['ArchiveReport'], ack: boolean): void {
  if (report.statements?.refused?.length)
    throw problem(
      'archive_statement_refused',
      `The archive has ${report.statements.refused.length} statements that are not allowed.`,
      {
        reasons: report.statements.refused.map((r) => ({
          code: 'statement_refused',
          object: `${r.kind} at ${r.position}`,
          message: `${r.reason} (fingerprint ${r.fingerprint})`,
        })),
      },
    )
  if (!report.valid) {
    const code = report.reasons.some((r) => r.code === 'archive_incompatible')
      ? 'archive_incompatible'
      : 'archive_invalid'
    throw problem(code, 'The archive cannot be imported.', { reasons: report.reasons })
  }
  if (report.needs?.ledgerRestart && !ack)
    throw problem(
      'ledger_restart_not_acknowledged',
      'The archive has ledger tables; an import starts a new chain for them. Set acknowledgeLedgerRestart to accept it.',
    )
}

export function importHandlers(): Partial<Record<OperationId, Handler>> {
  return {
    importDatabase: (c: Ctx) => {
      const s = c.s
      guard(s)
      const b = (c.body ?? {}) as S['ImportDatabaseRequest']
      const errors: { field: string; message: string }[] = []
      if (typeof b.uploadId !== 'string' || b.uploadId === '')
        errors.push({ field: 'uploadId', message: 'Required.' })
      if (b.database !== undefined && !NAME_RE.test(b.database))
        errors.push({ field: 'database', message: 'Use lower-case letters, digits and "_".' })
      if (b.expectedSha256 !== undefined && !/^[0-9a-f]{64}$/.test(b.expectedSha256))
        errors.push({ field: 'expectedSha256', message: '64 lower-case hexadecimal digits.' })
      if (
        b.onForeignKeyViolation !== undefined &&
        b.onForeignKeyViolation !== 'fail' &&
        b.onForeignKeyViolation !== 'keepNotValid'
      )
        errors.push({ field: 'onForeignKeyViolation', message: 'Use fail or keepNotValid.' })
      if (errors.length) throw validationFailed(errors)
      if (transferSwitches.dangerousRole) throw problem('forbidden', DANGEROUS_ROLE_TEXT)
      const u = s.transfer.uploads.find((x) => x.upload.uploadId === b.uploadId)
      if (!u || u.creator !== me(s)) throw problem('not_found', 'The upload does not exist.')
      if (u.purpose !== 'databaseArchive')
        throw validationFailed([
          { field: 'uploadId', message: 'This upload is not a database archive.' },
        ])
      if (u.upload.state === 'receiving')
        throw problem('upload_incomplete', 'The upload has not received all its bytes.')
      if (u.usedBy !== null) throw problem('operation_in_progress', 'A task uses this upload.')
      const name = b.database ?? nameFromFile(u.fileName)
      if (s.databases.some((d) => d.info.name === name))
        throw problem('already_exists', `Database "${name}" already exists.`)
      // The archive is read again from its bytes, whatever a validation said before.
      const sha = u.upload.sha256 ?? ''
      if (b.expectedSha256 !== undefined && b.expectedSha256 !== sha)
        throw problem('archive_invalid', 'The file is not the one that expectedSha256 names.')
      const report = reportFor(u.fileName, sha)
      refuseByReport(report, b.acknowledgeLedgerRestart === true)
      checkConcurrency(s, name)
      const keepNotValid = b.onForeignKeyViolation === 'keepNotValid'
      const keep = b.keepOnFailure === true
      const loadAt = IMPORT_STEPS.indexOf('Load the data')
      const total = IMPORT_STEPS.length
      const taskId = s.nextTaskId
      const privateName = `kingres_import_${taskId}`
      const gone = keep
        ? `The private database ${privateName} was kept; drop it when you have looked at it.`
        : 'The private database was removed and nothing of the final name exists.'
      let failAt: { fraction: number; code: string; message: string } | undefined
      if (u.fileName === FIXTURE_ARCHIVES.failload)
        failAt = {
          fraction: (loadAt + 0.5) / total,
          code: 'row_count_mismatch',
          message: `The database loaded 118 rows of table public.orders and the archive has 120; nothing of the table was stored. ${gone}`,
        }
      else if (u.fileName === FIXTURE_ARCHIVES.fkviolation && !keepNotValid)
        failAt = {
          fraction: (IMPORT_STEPS.indexOf('Create the indexes and constraints') + 0.5) / total,
          code: 'foreign_key_violation',
          message: `The foreign key orders_customer_fk is violated by 7 rows. ${gone}`,
        }
      const lines: string[] = []
      if (report.needs?.ledgerRestart)
        lines.push(
          `${report.tables.ledger} ledger tables started a new chain at import time; keep the digests of the source database.`,
        )
      if (u.fileName === FIXTURE_ARCHIVES.ledger)
        lines.push(
          'Time-partition policy of public.events was imported disabled; enable it from the partition policy of the table.',
        )
      if (u.fileName === FIXTURE_ARCHIVES.fkviolation && keepNotValid)
        lines.push('Foreign key orders_customer_fk stays NOT VALID: 7 rows violate it.')
      u.usedBy = taskId
      const task = createTask(s, {
        kind: 'database.import',
        subject: { type: 'database', id: name },
        steps: [...IMPORT_STEPS],
        detail: null,
        cancellable: true,
        durationMs: 5000,
        onProgress: (fraction) => {
          const own = s.tasks.find((x) => x.task.id === taskId)
          if (!own || own.task.state !== 'running') return
          const at = Math.floor(fraction * total)
          // Past the sequences a cancel is no longer accepted.
          if (at >= IMPORT_STEPS.indexOf('Set the sequences')) own.task.cancellable = false
          if (at === loadAt) {
            const frac = fraction * total - loadAt
            const i = Math.min(LOAD_TABLES.length - 1, Math.floor(frac * LOAD_TABLES.length))
            own.task.detail = `${LOAD_TABLES[i]} (${i + 1} of ${LOAD_TABLES.length}), ${Math.round(frac * 41 * MiB)} bytes so far`
          } else own.task.detail = null
        },
        ...(failAt
          ? {
              failAt,
              onFail: () => {
                u.usedBy = null
              },
              apply: () => undefined,
            }
          : {
              apply: () => {
                u.usedBy = null
                s.databases.push(newDb(s, name, report.tables.ledger > 0))
                const own = s.tasks.find((x) => x.task.id === taskId)
                if (own) own.task.detail = lines.length ? lines.join('\n') : `Imported ${name}.`
              },
            }),
      })
      return accepted(task)
    },
  }
}
