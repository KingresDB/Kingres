// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The words and rules of the database archive in the console (contract 0.40.0, a placeholder revision): the request of
// the export, the sentence about consistency that is shown before the person confirms, the file name the console offers,
// and what the report of a validation says in one line. No text of a statement of an archive is ever handled here:
// the report carries a kind, a position and a fingerprint, and that is all the console shows.
import type {
  ArchiveReport,
  DataExport,
  ExportDatabaseRequest,
  ImportDatabaseRequest,
  Table,
} from '@/api/types'

/** How many tables a database has by the way the export reads them. */
export interface ReadPlan {
  tables: number
  /** Distributed tables, which may be read per group (the export decides when it knows the buckets). */
  distributed: number
  /** Names of the distributed tables, for the sentence. */
  distributedNames: string[]
  groups: number
}

export function readPlan(tables: readonly Table[], groups: number): ReadPlan {
  const dist = tables.filter((t) => t.classes.includes('distributed'))
  return {
    tables: tables.length,
    distributed: dist.length,
    distributedNames: dist.map((t) => `${t.schema}.${t.name}`),
    groups,
  }
}

/** The sentence shown before the export is confirmed: what the archive guarantees for this database. */
export function archiveConsistencyBefore(plan: ReadPlan): string {
  if (plan.distributed === 0 || plan.groups <= 1)
    return 'Every table of this database is local, a reference table, or on one group: the data is read in one transaction, so the archive is one snapshot (archive-single-snapshot).'
  return `${plan.distributed} of the ${plan.tables} tables are distributed over ${plan.groups} shard groups. If their buckets lie in more than one group, each table is read by one statement and each group is read at its own moment, so a transaction that touched two groups can appear in one and not in the other. If they lie in one group, the whole archive is one snapshot. There is no global snapshot; the result says which label every table got.`
}

/** True when a tick of the order matters: tables are read one after another unless the archive is one snapshot. */
export const READ_ORDER_TEXT =
  'Unless the archive is one snapshot, tables are read one after another, so tables that refer to each other may be read at different moments. An import checks the foreign keys at its end.'

export function exportDatabaseRequest(form: {
  dataMode: 'all' | 'none'
  excluded: readonly string[]
  acknowledgeLargeObjectsLost: boolean
  retentionHours: number
}): ExportDatabaseRequest {
  const request: ExportDatabaseRequest = {}
  if (form.dataMode !== 'all') request.dataMode = form.dataMode
  if (form.excluded.length > 0 && form.dataMode === 'all')
    request.excludeTables = form.excluded.map((key) => {
      const [schema, table] = JSON.parse(key) as [string, string]
      return { schema, table }
    })
  if (form.acknowledgeLargeObjectsLost) request.acknowledgeLargeObjectsLost = true
  request.retentionHours = form.retentionHours
  return request
}

/** The key of a table in the list of excluded tables: the pair, never the two names joined (a dot in a name would make two tables one). */
export const tableKey = (t: Pick<Table, 'schema' | 'name'>): string =>
  JSON.stringify([t.schema, t.name])

/** The text that shows a table. */
export const tableLabel = (t: Pick<Table, 'schema' | 'name'>): string => `${t.schema}.${t.name}`

/** The file name the console offers for an archive (built from the identifier of the database only). */
export function archiveFileName(e: Pick<DataExport, 'database' | 'exportId'>): string {
  const safe = (e.database ?? '').replace(/[^A-Za-z0-9._-]/g, '_').slice(0, 40)
  return `${safe || `export-${e.exportId}`}.kdx.zip`
}

/** The names of the objects an `export_not_readable` problem lists, from its detail (the server names up to 20). */
export function unreadableObjects(detail: string | undefined): string[] {
  if (!detail) return []
  const at = detail.indexOf(':')
  const list = at >= 0 ? detail.slice(at + 1) : detail
  return list
    .split(',')
    .map((s) => s.trim().replace(/\.$/, ''))
    .filter((s) => s !== '')
    .slice(0, 20)
}

/** One line for the head of the report: valid or not, the format and the number of tables. */
export function reportHeadline(r: ArchiveReport): string {
  const n = r.tables.local + r.tables.hash + r.tables.reference
  const what = `${r.format || 'unknown format'}, ${n} ${n === 1 ? 'table' : 'tables'}`
  return r.valid ? `The archive is valid (${what}).` : `The archive is not valid (${what}).`
}

/** The kinds of table the report counts, in the order the report page lists them. */
export const TABLE_KINDS: readonly {
  key: 'local' | 'hash' | 'reference' | 'ledger' | 'columnar'
  label: string
}[] = [
  { key: 'local', label: 'Local tables' },
  { key: 'hash', label: 'Distributed (hash) tables' },
  { key: 'reference', label: 'Reference tables' },
  { key: 'ledger', label: 'Ledger tables' },
  { key: 'columnar', label: 'Columnar tables' },
]

export const LEDGER_RESTART_TEXT =
  'The archive has ledger tables. Their rows are carried as data. An import makes a new chain from them at import time: it does not carry the history that proves the data was not changed before. Keep the digests of the source database.'

export const IMPORT_HINT_TEXT =
  'To load this archive into a new database, use Import database on the Databases page.'

/** The sentence of ADR 0028 that the person accepts when an archive has ledger tables. */
export const LEDGER_NEW_CHAIN_TEXT =
  'The imported ledger starts a new chain at import time. It does not carry the history that proves the data was not changed before; keep the digests of the source database.'

export const POLICIES_DISABLED_TEXT =
  'Time-partition policies of the archive are imported disabled: a retention policy must not drop data on a schedule that nobody chose. The result lists them.'

export const DANGEROUS_ROLE_TEXT =
  'An import runs the statements of the archive as the signed-in role, so it is refused for a role with the SUPERUSER, REPLICATION or BYPASSRLS attribute or with access to server files, programs or logs. Sign in as another administrator to import a database.'

const DB_NAME_RE = /^[a-z_][a-z0-9_]{0,62}$/

/** Why a name cannot be the name of the new database, or null. */
export function databaseNameError(name: string, existing: readonly string[]): string | null {
  if (name === '') return 'Choose a name for the new database.'
  if (!DB_NAME_RE.test(name))
    return 'Use lower-case letters, digits and an underscore; start with a letter or an underscore; at most 63 characters.'
  if (existing.includes(name)) return 'A database with this name exists.'
  return null
}

/** The name a file suggests for the new database: its name without .kdx.zip, made into a valid identifier. */
export function suggestDatabaseName(fileName: string): string {
  const base = fileName.replace(/\.kdx\.zip$|\.zip$/i, '').toLowerCase()
  const clean = base.replace(/[^a-z0-9_]/g, '_').replace(/^[0-9]/, '_$&')
  return (clean || 'imported').slice(0, 63)
}

/** The lines of the detail of a finished import (one per thing that needs a decision). */
export function importResultLines(detail: string | null | undefined): string[] {
  return (detail ?? '')
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l !== '')
}

/** The sentence about the private database after a failed import. */
export function privateDatabaseSentence(keepOnFailure: boolean): string {
  return keepOnFailure
    ? 'The private database was kept, as you asked; drop it when you have looked at it. The name you chose was never taken.'
    : 'The private database was removed. The name you chose was never taken and nothing of it exists.'
}

/** The request of the import, from the form (an empty optional field is left out; a default is not sent). */
export function importDatabaseRequest(form: {
  uploadId: string
  database: string
  expectedSha256: string
  acknowledgeLedgerRestart: boolean
  onForeignKeyViolation: 'fail' | 'keepNotValid'
  keepOnFailure: boolean
}): ImportDatabaseRequest {
  const r: ImportDatabaseRequest = { uploadId: form.uploadId, database: form.database }
  const sha = form.expectedSha256.trim().toLowerCase()
  if (sha !== '') r.expectedSha256 = sha
  if (form.acknowledgeLedgerRestart) r.acknowledgeLedgerRestart = true
  if (form.onForeignKeyViolation !== 'fail') r.onForeignKeyViolation = form.onForeignKeyViolation
  if (form.keepOnFailure) r.keepOnFailure = true
  return r
}

/** Why the checksum field blocks the import, or null: it is empty, or 64 hex digits equal to the archive's. */
export function checksumError(entered: string, actual: string | undefined): string | null {
  const v = entered.trim().toLowerCase()
  if (v === '') return null
  if (!/^[0-9a-f]{64}$/.test(v)) return 'A SHA-256 is 64 hexadecimal digits.'
  if (actual && v !== actual) return 'This is not the SHA-256 of the archive that was sent.'
  return null
}

/** The shape of a file name the validation accepts for choosing: a zip archive. */
export const isZipName = (name: string): boolean => /\.zip$/i.test(name)
