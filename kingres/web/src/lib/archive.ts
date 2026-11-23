// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The words and rules of the database archive in the console (contract 0.40.0, a placeholder revision): the request of
// the export, the sentence about consistency that is shown before the person confirms, the file name the console offers,
// and what the report of a validation says in one line. No text of a statement of an archive is ever handled here:
// the report carries a kind, a position and a fingerprint, and that is all the console shows.
import type { ArchiveReport, DataExport, ExportDatabaseRequest, Table } from '@/api/types'

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

export const IMPORT_NOT_AVAILABLE_TEXT =
  'Importing an archive is not available in this version. The archive can be inspected, and its SQL and CSV files can be loaded by hand.'

/** The shape of a file name the validation accepts for choosing: a zip archive. */
export const isZipName = (name: string): boolean => /\.zip$/i.test(name)
