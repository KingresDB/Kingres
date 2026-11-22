// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The table lifecycle of contract 0.14.0 in the mock (increment 9.7): the preview of a change (`previewTableChange`) with the
// blockers, warnings and notes of the sharding extension's report, the tasks `table.distribute`, `table.undistribute`,
// `table.alter` and `table.partition`, and the user partitions of a distributed table. The mock is the contract in executable
// form: a table that is not eligible is refused with 422 `table_not_eligible` and the same lines that the preview lists, a
// change of a table that another change holds is refused with 409 `operation_in_progress`, and a task can be cancelled until its switch.
import type { components } from '@/api/schema'
import type { Table, Task } from '@/api/types'
import { problem, validationFailed } from './http'
import {
  createTask,
  dataGroups,
  makeBuckets,
  mockConfig,
  tickTasks,
  type DbRec,
  type MockState,
} from './state'

type S = components['schemas']
type Line = S['TableBlocker']
type Preview = S['TableChangePreview']
type Mode = S['TableChangeMode']
type Views = S['DependentViews']

/** The columns of the tables the mock knows (a table without an entry has an id and a time). `key`: part of every unique key. */
export const COLUMN_DEFS: Record<string, { name: string; type: string; key?: boolean }[]> = {
  notes: [
    { name: 'note_id', type: 'bigint', key: true },
    { name: 'body', type: 'text' },
    { name: 'tags', type: 'jsonb' },
    { name: 'created_at', type: 'timestamptz' },
  ],
  monthly_totals: [
    { name: 'month', type: 'date', key: true },
    { name: 'total', type: 'numeric' },
    { name: 'region', type: 'text' },
  ],
  exports: [
    { name: 'export_id', type: 'bigint', key: true },
    { name: 'payload', type: 'bytea' },
    { name: 'created_at', type: 'timestamptz' },
  ],
  orders: [
    { name: 'order_id', type: 'bigint', key: true },
    { name: 'customer_id', type: 'bigint', key: true },
    { name: 'total', type: 'numeric(12,2)' },
    { name: 'placed_at', type: 'timestamptz' },
  ],
  customers: [
    { name: 'customer_id', type: 'bigint', key: true },
    { name: 'name', type: 'text' },
    { name: 'email', type: 'text' },
  ],
  events: [
    { name: 'session_id', type: 'uuid', key: true },
    { name: 'event_id', type: 'bigint', key: true },
    { name: 'payload', type: 'jsonb' },
  ],
  countries: [
    { name: 'code', type: 'text', key: true },
    { name: 'name', type: 'text' },
  ],
  measurements: [
    { name: 'sensor_id', type: 'bigint', key: true },
    { name: 'taken_at', type: 'timestamptz', key: true },
    { name: 'value', type: 'double precision' },
  ],
  measurements_old: [
    { name: 'sensor_id', type: 'bigint', key: true },
    { name: 'taken_at', type: 'timestamptz', key: true },
    { name: 'value', type: 'double precision' },
  ],
}
const DEFAULT_COLUMNS = [
  { name: 'id', type: 'bigint', key: true },
  { name: 'created_at', type: 'timestamptz' },
]
export const columnDefsOf = (t: Table) => COLUMN_DEFS[t.name] ?? DEFAULT_COLUMNS
const keyColumns = (t: Table): string[] =>
  columnDefsOf(t)
    .filter((c) => c.key)
    .map((c) => c.name)

/** The views that use a table (a plain table with one is refused unless the views are made again). */
const VIEWS_OF: Record<string, string[]> = {
  monthly_totals: ['monthly_totals_by_region'],
  customers: ['customer_names'],
}
/** Triggers on a table (a table with one cannot be distributed). */
const TRIGGERS_OF: Record<string, string[]> = { exports: ['exports_audit'] }
/** The foreign keys among the tables: the first has a key to the second. */
const FOREIGN_KEYS: [string, string][] = [['orders', 'customers']]

const MiB = 1024 * 1024
const name = (t: Table) => `${t.schema}.${t.name}`
const line = (code: string, object: string, message: string): Line => ({ code, object, message })

/** The tables of the database that a foreign key links to this one, either way. */
function linked(rec: DbRec, t: Table): Table[] {
  const out: Table[] = []
  for (const [from, to] of FOREIGN_KEYS) {
    const other = t.name === from ? to : t.name === to ? from : null
    const found = other ? rec.tables.find((x) => x.name === other) : undefined
    if (found) out.push(found)
  }
  return out
}
/** The tables of the colocation group of a table, itself left out. */
function colocated(rec: DbRec, t: Table): Table[] {
  const id = t.distribution?.colocationId
  return id == null
    ? []
    : rec.tables.filter(
        (x) => x !== t && x.distribution?.method === 'hash' && x.distribution.colocationId === id,
      )
}
const hasForeignKeys = (rec: DbRec, t: Table): boolean =>
  mockConfig.tableForeignKeys.includes(t.name) || linked(rec, t).length > 0
const isPartitioned = (t: Table): boolean => t.partitioning != null

// --- the partitions the mock keeps (by table) ------------------------------------------------------------

/** The partitions of every user-partitioned table: `database/schema.table` to its list. */
export const partitionsOf = (s: MockState, rec: DbRec, t: Table): S['TablePartition'][] =>
  s.partitions?.[`${rec.info.name}/${name(t)}`] ?? []
function setPartitions(s: MockState, rec: DbRec, t: Table, list: S['TablePartition'][]): void {
  s.partitions ??= {}
  s.partitions[`${rec.info.name}/${name(t)}`] = list
  if (t.partitioning) t.partitioning = { ...t.partitioning, partitionCount: list.length }
}
const partitionOf = (bound: string, method: 'range' | 'list', leafCount: number, n: string) =>
  ({
    name: n,
    method,
    bound,
    isDefault: /^default$/i.test(bound.trim()),
    leafCount,
    sizeBytes: 40 * MiB,
  }) satisfies S['TablePartition']

/** `?tables=partitions`: a distributed table that the user partitioned by range, with three partitions, and a table to attach. */
export function seedPartitioned(s: MockState): void {
  const rec = s.databases.find((d) => d.info.name === 'shop')
  if (!rec || rec.tables.some((t) => t.name === 'measurements')) return
  const colocationId = Math.max(0, ...rec.colocations.map((x) => x.colocationId)) + 1
  const group = {
    colocationId,
    bucketCount: 32,
    distributionColumnType: 'bigint',
    tableCount: 2,
    sizeBytes: 300 * MiB,
  }
  rec.colocations.push(group)
  rec.buckets.set(
    colocationId,
    makeBuckets(
      colocationId,
      32,
      dataGroups(s).map((g) => g.id),
      colocationId * 7,
    ),
  )
  const base = {
    schema: 'public',
    classes: ['distributed'] as Table['classes'],
    distribution: {
      method: 'hash' as const,
      column: 'sensor_id',
      colocationId,
      bucketCount: 32,
    },
    ledger: null,
    homeShardGroupId: null,
  }
  const m: Table = {
    ...base,
    name: 'measurements',
    estimatedRows: 8_400_000,
    sizeBytes: 640 * MiB,
    partitioning: { method: 'range', key: 'taken_at', partitionCount: 3 },
  }
  rec.tables.push(m, {
    ...base,
    name: 'measurements_old',
    estimatedRows: 1_200_000,
    sizeBytes: 90 * MiB,
  })
  rec.info.stats.tableCount = (rec.info.stats.tableCount ?? 0) + 2
  setPartitions(s, rec, m, [
    partitionOf("FROM ('2026-01-01') TO ('2026-02-01')", 'range', 32, 'measurements_2026_01'),
    partitionOf("FROM ('2026-02-01') TO ('2026-03-01')", 'range', 32, 'measurements_2026_02'),
    partitionOf('DEFAULT', 'range', 32, 'measurements_default'),
  ])
}

// --- the preview ---------------------------------------------------------------------------------------

interface Chosen {
  mode: Mode
  views: Views
}

function estimate(
  t: Table,
  chosen: Chosen,
  opts: { catalogOnly?: boolean; rows?: number | null; bytes?: number | null } = {},
): Preview['estimate'] {
  const keyed = keyColumns(t).length > 0
  const rows = opts.rows !== undefined ? opts.rows : t.estimatedRows
  const recommended: 'online' | 'blocking' = keyed && (rows ?? 0) > 100_000 ? 'online' : 'blocking'
  const effective = chosen.mode === 'auto' ? recommended : chosen.mode
  return {
    rows: opts.catalogOnly ? 0 : rows,
    bytes: opts.catalogOnly ? 0 : opts.bytes !== undefined ? opts.bytes : t.sizeBytes,
    modeRecommended: recommended,
    onlineAvailable: keyed,
    onlineUnavailableReason: keyed
      ? null
      : 'The table has no primary key and no unique index on columns that are NOT NULL.',
    writersBlocked: opts.catalogOnly ? 'none' : effective === 'online' ? 'switchOnly' : 'wholeCopy',
  }
}

/** A change that moves no row between servers is done in one step (a column change always moves rows). */
function catalogOnlyChange(t: Table, a: S['AlterTableDistributionRequest']): boolean {
  if (a.distributionColumn !== undefined && a.distributionColumn !== t.distribution?.column)
    return false
  const old = t.distribution?.bucketCount ?? null
  return a.bucketCount === undefined || (old !== null && a.bucketCount % old === 0)
}

const affected = (t: Table, role: S['TableChangeTable']['role']): S['TableChangeTable'] => ({
  schema: t.schema,
  name: t.name,
  role,
  sizeBytes: t.sizeBytes,
  estimatedRows: t.estimatedRows,
})

/** The request of a change with the defaults of the contract filled in. */
function chosenOf(mode: Mode | undefined, views: Views | undefined): Chosen {
  return { mode: mode ?? 'auto', views: views ?? 'refuse' }
}

function findTable(rec: DbRec, ref: S['TableRef']): Table | undefined {
  return rec.tables.find((x) => x.schema === ref.schema && x.name === ref.table)
}

/** What the sharding extension's report says about a change. A blocker is what 422 `table_not_eligible` would list. */
export function previewChange(
  s: MockState,
  rec: DbRec,
  t: Table,
  req: S['TableChangeRequest'],
): Preview {
  const blockers: Line[] = []
  const warnings: Line[] = []
  const notes: Line[] = []
  const touched: S['TableChangeTable'][] = [affected(t, 'source')]
  let chosen = chosenOf(undefined, undefined)
  let est: Preview['estimate'] = estimate(t, chosen)
  const views = (v: Views, verb: string) => {
    for (const view of VIEWS_OF[t.name] ?? []) {
      if (v === 'refuse')
        blockers.push(
          line(
            'dependent_object',
            `view ${view}`,
            `The view ${view} uses ${name(t)}. Ask for the views to be made again, or drop the view.`,
          ),
        )
      else
        warnings.push(
          line(
            'dependent_view',
            `view ${view}`,
            `The view ${view} is dropped in the switch and made again on the new table (${verb}).`,
          ),
        )
    }
  }
  const busy = (what: S['TableChangeTable'][]) => {
    if (mockConfig.tableBusy)
      blockers.push(
        line(
          'group_busy',
          `colocation group ${t.distribution?.colocationId ?? '-'}`,
          'A bucket move or another table operation uses this colocation group. Try again when it has ended.',
        ),
      )
    void what
  }
  switch (req.kind) {
    case 'distribute': {
      const d = req.distribute ?? { method: 'hash' as const }
      chosen = chosenOf(d.mode, d.dependentViews)
      est = estimate(t, chosen)
      if (!rec.info.sharding.enabled)
        blockers.push(
          line('sharding_disabled', rec.info.name, 'Sharding is not enabled for this database.'),
        )
      if (!t.classes.includes('plain'))
        blockers.push(
          line('not_plain', name(t), 'The table is already distributed or a reference table.'),
        )
      for (const trg of TRIGGERS_OF[t.name] ?? [])
        blockers.push(
          line(
            'trigger',
            `trigger ${trg}`,
            `The trigger ${trg} is on ${name(t)}; triggers are not carried to the shards.`,
          ),
        )
      if (d.method === 'hash') {
        const col = d.distributionColumn
        if (col && !keyColumns(t).includes(col) && keyColumns(t).length > 0)
          blockers.push(
            line(
              'not_in_unique_key',
              col,
              `The column ${col} is not part of every unique key of ${name(t)}.`,
            ),
          )
        const target = d.colocateWith ? findTable(rec, d.colocateWith) : undefined
        if (d.colocateWith && !target?.distribution)
          blockers.push(
            line(
              'not_distributed',
              d.colocateWith.table,
              'The table to colocate with is not distributed.',
            ),
          )
      }
      if (chosen.mode === 'online' && !est.onlineAvailable)
        blockers.push(
          line(
            'no_unique_key',
            name(t),
            est.onlineUnavailableReason ?? 'An online copy needs a key.',
          ),
        )
      views(chosen.views, 'distribute')
      if (hasForeignKeys(rec, t))
        blockers.push(
          line('foreign_key_linked', name(t), `${name(t)} has a foreign key to an ordinary table.`),
        )
      if ((t.estimatedRows ?? 0) > 0)
        notes.push(
          line(
            'rows_copied',
            name(t),
            `About ${t.estimatedRows} rows are copied. Reads go on during the copy.`,
          ),
        )
      if (isPartitioned(t))
        notes.push(
          line(
            'partitioned_table',
            name(t),
            'A RANGE or LIST partitioned table becomes a user-partitioned distributed table (blocking only).',
          ),
        )
      break
    }
    case 'undistribute': {
      const u = req.undistribute ?? {}
      chosen = chosenOf(u.mode, u.dependentViews)
      est = estimate(t, chosen)
      if (t.classes.includes('plain'))
        blockers.push(line('not_distributed', name(t), 'The table is already a plain table.'))
      if (isPartitioned(t))
        blockers.push(
          line(
            'user_partitioned',
            name(t),
            'A user-partitioned table cannot be undistributed in this version.',
          ),
        )
      const links = linked(rec, t).filter((x) => !x.classes.includes('plain'))
      for (const l of links) {
        if (u.cascadeViaForeignKeys) touched.push(affected(l, 'linked'))
        else
          blockers.push(
            line(
              'foreign_key_linked',
              name(l),
              `A foreign key links ${name(t)} and ${name(l)}. Undistribute both together.`,
            ),
          )
      }
      if (u.targetGroupId !== undefined && !s.groups.some((g) => g.id === u.targetGroupId))
        blockers.push(
          line('no_such_group', `group ${u.targetGroupId}`, 'The shard group does not exist.'),
        )
      views(chosen.views, 'undistribute')
      if (chosen.mode === 'online' && !est.onlineAvailable)
        blockers.push(
          line(
            'no_unique_key',
            name(t),
            est.onlineUnavailableReason ?? 'An online copy needs a key.',
          ),
        )
      notes.push(
        line(
          'target',
          `group ${u.targetGroupId ?? 0}`,
          `The table is held by the primary of group ${u.targetGroupId ?? 0} afterwards.`,
        ),
      )
      break
    }
    case 'alter': {
      const a = req.alter ?? {}
      chosen = chosenOf(a.mode, a.dependentViews)
      est = estimate(t, chosen, { catalogOnly: catalogOnlyChange(t, a) })
      if (t.classes.includes('plain') || t.distribution?.method !== 'hash')
        blockers.push(
          line(
            'not_distributed',
            name(t),
            'Only a hash-distributed table can change its distribution.',
          ),
        )
      if (isPartitioned(t))
        blockers.push(
          line(
            'user_partitioned',
            name(t),
            'A user-partitioned table cannot be altered in this version.',
          ),
        )
      if (a.distributionColumn && !keyColumns(t).includes(a.distributionColumn))
        blockers.push(
          line(
            'not_in_unique_key',
            a.distributionColumn,
            `The column ${a.distributionColumn} is not part of every unique key of ${name(t)}.`,
          ),
        )
      if (a.cascadeToColocated) {
        for (const c of colocated(rec, t)) touched.push(affected(c, 'colocated'))
        if (chosen.mode === 'online')
          blockers.push(
            line(
              'online_not_allowed',
              name(t),
              'A change of the tables of a colocation group together is blocking only.',
            ),
          )
      }
      busy(touched)
      views(chosen.views, 'alter')
      if (a.bucketCount !== undefined && t.distribution?.bucketCount != null) {
        const old = t.distribution.bucketCount
        if (a.bucketCount % old === 0 && a.bucketCount !== old)
          notes.push(
            line(
              'multiple_buckets',
              name(t),
              `${a.bucketCount} is a multiple of ${old}: no row moves between servers.`,
            ),
          )
      }
      break
    }
    case 'createPartition': {
      est = estimate(t, chosen, { catalogOnly: true })
      const p = req.partition ?? {}
      if (!isPartitioned(t))
        blockers.push(line('not_partitioned', name(t), 'The table is not partitioned by the user.'))
      busy(touched)
      if (p.bound !== undefined && !/^(default|from\s|in\s|with\s)/i.test(p.bound.trim()))
        blockers.push(
          line(
            'invalid_bound',
            p.bound,
            'The bound is not the text of a partition bound (FROM ... TO ..., IN (...) or DEFAULT).',
          ),
        )
      if (p.name && partitionsOf(s, rec, t).some((x) => x.name === p.name))
        warnings.push(line('exists', p.name, `A partition named ${p.name} exists already.`))
      notes.push(
        line(
          'leaves',
          name(t),
          'One leaf is created in every bucket partition on every server, in one transaction.',
        ),
      )
      break
    }
    case 'attachPartition': {
      est = estimate(t, chosen, { catalogOnly: true })
      const p = req.partition ?? {}
      const other = p.table ? findTable(rec, p.table) : undefined
      if (!isPartitioned(t))
        blockers.push(line('not_partitioned', name(t), 'The table is not partitioned by the user.'))
      if (!other)
        blockers.push(line('not_found', p.table?.table ?? '-', 'The table does not exist.'))
      else {
        touched.push(affected(other, 'colocated'))
        if (
          other.distribution?.colocationId !== t.distribution?.colocationId ||
          other.distribution?.column !== t.distribution?.column
        )
          blockers.push(
            line(
              'not_in_colocation_group',
              name(other),
              `${name(other)} is not distributed in the colocation group of ${name(t)} by the same column.`,
            ),
          )
      }
      busy(touched)
      break
    }
    case 'detachPartition':
    case 'dropPartition': {
      est = estimate(t, chosen, { catalogOnly: true })
      const p = req.partition ?? {}
      const found = partitionsOf(s, rec, t).find((x) => x.name === p.name)
      if (!found) blockers.push(line('not_found', p.name ?? '-', 'The partition does not exist.'))
      if (req.kind === 'detachPartition' && hasForeignKeys(rec, t))
        blockers.push(
          line('foreign_key', name(t), 'A table with foreign keys cannot detach a partition.'),
        )
      busy(touched)
      if (req.kind === 'dropPartition' && found)
        warnings.push(line('rows_deleted', found.name, `The rows of ${found.name} are deleted.`))
      break
    }
  }
  return { blockers, warnings, notes, affectedTables: touched, estimate: est }
}

// --- the tasks -------------------------------------------------------------------------------------------

const TABLE_KINDS = ['table.distribute', 'table.undistribute', 'table.alter', 'table.partition']
const subjectOf = (rec: DbRec, t: Table) => ({
  type: 'table' as const,
  id: `${rec.info.name}/${name(t)}`,
})

/** 409 `operation_in_progress` while another change holds the table or its colocation group. */
function refuseWhenHeld(s: MockState, rec: DbRec, t: Table): void {
  tickTasks(s)
  const group = t.distribution?.colocationId
  const held = s.tasks.some((r) => {
    if (
      !['pending', 'running', 'paused'].includes(r.task.state) ||
      !TABLE_KINDS.includes(r.task.kind)
    )
      return false
    const id = r.task.subject?.id ?? ''
    if (!id.startsWith(`${rec.info.name}/`)) return false
    if (id === `${rec.info.name}/${name(t)}`) return true
    const other = rec.tables.find((x) => `${rec.info.name}/${name(x)}` === id)
    return group != null && other?.distribution?.colocationId === group
  })
  if (held || (mockConfig.tableBusy && t.distribution))
    throw problem(
      'operation_in_progress',
      'A change of this table, or of a table in its colocation group, is running. Try again when it has ended.',
    )
}

/** The refusal of a change that is not eligible: the lines of the report, as the contract says. */
function refuseWhenBlocked(p: Preview): void {
  if (p.blockers.length > 0)
    throw problem('table_not_eligible', 'The table cannot take this change as it is.', {
      blockers: p.blockers,
    })
}

/** A task that ends in the verification after its switch (the old copy is kept); the text is the one of the control plane. */
const VERIFY_FAIL = {
  fraction: 0.85,
  code: 'verification_failed',
  message:
    'The new table differs from the old copy, which is kept for inspection (kingres.table_operations names it).',
}

/** The switch is the point from which a task goes on to its end. */
const SWITCH_AT = 0.7

function startTask(
  s: MockState,
  rec: DbRec,
  t: Table,
  kind: Task['kind'],
  steps: string[],
  apply: () => void,
  opts: { cancellable: boolean },
): Task {
  const fail = mockConfig.tableFail
  const failAt =
    fail === 'space'
      ? {
          fraction: 0.5 / steps.length,
          code: 'insufficient_space',
          message:
            'Not enough free space on server 10.0.0.13: the copy needs 2.1 GiB and 0.8 GiB is free. Nothing was changed.',
        }
      : fail === 'copy'
        ? {
            fraction: 0.4,
            code: 'copy_failed',
            message:
              'The copy of the shard of bucket 41 failed: the connection to server 10.0.0.12 was closed. The table is as it was before.',
          }
        : undefined
  const id = s.nextTaskId
  const blockedDetail =
    'Waiting to switch: pid 4123 (role app) holds a lock on the table and has run for 3 minutes.'
  const created = createTask(s, {
    kind,
    subject: subjectOf(rec, t),
    steps,
    cancellable: opts.cancellable,
    durationMs: mockConfig.tableSlow ? 60_000 : undefined,
    uncapped: mockConfig.tableSlow,
    failAt: failAt ?? (mockConfig.tableVerifyFails ? VERIFY_FAIL : undefined),
    ...(mockConfig.tableBlocked
      ? {
          pauseAt: 0.5,
          onPause: () => {
            const r = s.tasks.find((x) => x.task.id === id)
            if (r) r.task.detail = blockedDetail
          },
        }
      : {}),
    onProgress: (fraction) => {
      const r = s.tasks.find((x) => x.task.id === id)
      if (r && r.task.state === 'running' && fraction >= SWITCH_AT && opts.cancellable)
        r.task.cancellable = false
    },
    apply,
  })
  if (!mockConfig.tablePending) return created
  // Accepted and not started: the task engine has not picked it up yet.
  const pending = s.tasks.find((x) => x.task.id === id)
  if (!pending) return created
  pending.task.state = 'pending'
  pending.task.cancellable = false
  pending.task.progress.percent = 0
  pending.task.progress.currentStep = null
  pending.task.progress.steps = pending.task.progress.steps.map((st) => ({
    ...st,
    state: 'pending' as const,
  }))
  return structuredClone(pending.task)
}

function newColocation(
  s: MockState,
  rec: DbRec,
  t: Table,
  column: string | null,
  bucketCount: number,
): number {
  const colocationId =
    Math.max(
      0,
      ...rec.tables.map((x) => x.distribution?.colocationId ?? 0),
      ...rec.colocations.map((x) => x.colocationId),
    ) + 1
  rec.colocations.push({
    colocationId,
    bucketCount,
    distributionColumnType: columnDefsOf(t).find((c) => c.name === column)?.type ?? 'bigint',
    tableCount: 0,
    sizeBytes: 0,
  })
  rec.buckets.set(
    colocationId,
    makeBuckets(
      colocationId,
      bucketCount,
      dataGroups(s).map((g) => g.id),
      colocationId * 7,
    ),
  )
  return colocationId
}
function groupOf(rec: DbRec, id: number | null | undefined) {
  return rec.colocations.find((x) => x.colocationId === id)
}
function recount(rec: DbRec): void {
  for (const g of rec.colocations)
    g.tableCount = rec.tables.filter(
      (x) => x.distribution?.method === 'hash' && x.distribution.colocationId === g.colocationId,
    ).length
}

export function startDistribute(
  s: MockState,
  rec: DbRec,
  t: Table,
  b: S['DistributeTableRequest'],
): Task {
  if (
    b.bucketCount !== undefined &&
    (!Number.isInteger(b.bucketCount) || b.bucketCount < 1 || b.bucketCount > 4096)
  )
    throw validationFailed([{ field: 'bucketCount', message: 'Use a number from 1 to 4096.' }])
  if (b.method === 'reference' && (b.colocateWith || b.distributionColumn))
    throw validationFailed([
      {
        field: b.colocateWith ? 'colocateWith' : 'distributionColumn',
        message: 'A reference table has no distribution column and no colocation.',
      },
    ])
  refuseWhenHeld(s, rec, t)
  refuseWhenBlocked(previewChange(s, rec, t, { kind: 'distribute', distribute: b }))
  const target = b.colocateWith ? findTable(rec, b.colocateWith) : undefined
  const est = estimate(t, chosenOf(b.mode, b.dependentViews))
  const online =
    (b.mode ?? 'auto') === 'auto' ? est.modeRecommended === 'online' : b.mode === 'online'
  const steps = [
    'Check free space',
    'Create shards',
    online ? 'Copy rows (online)' : 'Copy rows',
    'Switch over',
    'Clean up',
  ]
  return startTask(
    s,
    rec,
    t,
    'table.distribute',
    steps,
    () => {
      const bucketCount = b.bucketCount ?? rec.info.sharding.bucketCount ?? 128
      if (b.method === 'reference') {
        t.classes = t.ledger ? ['reference', 'ledger'] : ['reference']
        t.distribution = {
          method: 'reference',
          column: null,
          colocationId: null,
          bucketCount: null,
        }
        t.homeShardGroupId = null
      } else {
        const colocationId =
          target?.distribution?.colocationId ??
          newColocation(s, rec, t, b.distributionColumn ?? null, bucketCount)
        t.classes = t.ledger ? ['distributed', 'ledger'] : ['distributed']
        t.distribution = {
          method: 'hash',
          column: b.distributionColumn ?? null,
          colocationId,
          bucketCount: groupOf(rec, colocationId)?.bucketCount ?? bucketCount,
        }
        t.homeShardGroupId = null
        recount(rec)
      }
    },
    { cancellable: true },
  )
}

export function startUndistribute(
  s: MockState,
  rec: DbRec,
  t: Table,
  q: {
    cascadeViaForeignKeys?: boolean
    targetGroupId?: number
    mode?: Mode
    dependentViews?: Views
  },
): Task {
  if (q.targetGroupId !== undefined && (!Number.isInteger(q.targetGroupId) || q.targetGroupId < 0))
    throw validationFailed([
      { field: 'targetGroupId', message: 'Use a shard group number, 0 or more.' },
    ])
  refuseWhenHeld(s, rec, t)
  refuseWhenBlocked(previewChange(s, rec, t, { kind: 'undistribute', undistribute: q }))
  const targets = [
    t,
    ...(q.cascadeViaForeignKeys ? linked(rec, t).filter((x) => !x.classes.includes('plain')) : []),
  ]
  return startTask(
    s,
    rec,
    t,
    'table.undistribute',
    ['Check free space', 'Copy rows', 'Switch over', 'Clean up'],
    () => {
      for (const x of targets) {
        x.classes = x.ledger ? ['ledger'] : ['plain']
        x.distribution = null
        x.homeShardGroupId = q.targetGroupId ?? 0
      }
      recount(rec)
    },
    { cancellable: true },
  )
}

export function startAlter(
  s: MockState,
  rec: DbRec,
  t: Table,
  b: S['AlterTableDistributionRequest'],
): Task {
  if (
    b.distributionColumn === undefined &&
    b.bucketCount === undefined &&
    b.colocateWith === undefined
  )
    throw validationFailed([
      {
        field: 'distributionColumn',
        message: 'Say what to change: the column, the bucket count or the colocation.',
      },
    ])
  if (
    b.bucketCount !== undefined &&
    (!Number.isInteger(b.bucketCount) || b.bucketCount < 1 || b.bucketCount > 4096)
  )
    throw validationFailed([{ field: 'bucketCount', message: 'Use a number from 1 to 4096.' }])
  if (b.cascadeToColocated && b.mode === 'online')
    throw validationFailed([
      {
        field: 'mode',
        message: 'A change of the tables of a colocation group together is blocking only.',
      },
    ])
  refuseWhenHeld(s, rec, t)
  refuseWhenBlocked(previewChange(s, rec, t, { kind: 'alter', alter: b }))
  const old = t.distribution?.bucketCount ?? null
  const catalogOnly = catalogOnlyChange(t, b)
  const steps = catalogOnly
    ? ['Change the catalog']
    : ['Check free space', 'Copy rows', 'Switch over', 'Clean up']
  const group = [t, ...(b.cascadeToColocated ? colocated(rec, t) : [])]
  return startTask(
    s,
    rec,
    t,
    'table.alter',
    steps,
    () => {
      const bucketCount = b.bucketCount ?? t.distribution?.bucketCount ?? 128
      let colocationId = t.distribution?.colocationId ?? null
      if (b.colocateWith && typeof b.colocateWith === 'object') {
        colocationId = findTable(rec, b.colocateWith)?.distribution?.colocationId ?? colocationId
      } else if (
        b.colocateWith === 'none' ||
        (b.colocateWith === undefined && b.bucketCount !== undefined && b.bucketCount !== old)
      ) {
        colocationId = newColocation(
          s,
          rec,
          t,
          b.distributionColumn ?? t.distribution?.column ?? null,
          bucketCount,
        )
      } else if (b.colocateWith === 'default') {
        colocationId =
          rec.colocations.find(
            (x) => x.bucketCount === bucketCount && x.colocationId !== t.distribution?.colocationId,
          )?.colocationId ??
          newColocation(
            s,
            rec,
            t,
            b.distributionColumn ?? t.distribution?.column ?? null,
            bucketCount,
          )
      }
      for (const x of group) {
        if (!x.distribution) continue
        x.distribution = {
          method: 'hash',
          column: x === t ? (b.distributionColumn ?? x.distribution.column) : x.distribution.column,
          colocationId,
          bucketCount: groupOf(rec, colocationId)?.bucketCount ?? bucketCount,
        }
      }
      recount(rec)
    },
    { cancellable: !catalogOnly },
  )
}

type PartitionChange =
  | { kind: 'create'; body: S['CreateTablePartitionRequest'] }
  | { kind: 'attach'; body: S['AttachTablePartitionRequest'] }
  | { kind: 'detach'; partition: string }
  | { kind: 'drop'; partition: string }

export function startPartition(s: MockState, rec: DbRec, t: Table, change: PartitionChange): Task {
  const list = partitionsOf(s, rec, t)
  const method = t.partitioning?.method ?? 'range'
  // The existence of a partition is checked before the lock, as the extension does: 404 and 409 first.
  if (change.kind === 'detach' || change.kind === 'drop') {
    if (!list.some((x) => x.name === change.partition))
      throw problem('not_found', `Partition "${change.partition}" does not exist.`)
  }
  if (change.kind === 'create') {
    const n = change.body.name
    if (typeof n !== 'string' || n === '' || new TextEncoder().encode(n).length > 57)
      throw validationFailed([{ field: 'name', message: 'Use a name of 1 to 57 bytes.' }])
    if (typeof change.body.bound !== 'string' || change.body.bound.trim() === '')
      throw validationFailed([{ field: 'bound', message: 'Give the bound of the partition.' }])
    if (list.some((x) => x.name === n) && change.body.ifNotExists !== true)
      throw problem('already_exists', `A partition named "${n}" exists already.`)
  }
  refuseWhenHeld(s, rec, t)
  const req: S['TableChangeRequest'] =
    change.kind === 'create'
      ? { kind: 'createPartition', partition: { name: change.body.name, bound: change.body.bound } }
      : change.kind === 'attach'
        ? {
            kind: 'attachPartition',
            partition: { table: change.body.table, bound: change.body.bound },
          }
        : change.kind === 'detach'
          ? { kind: 'detachPartition', partition: { name: change.partition } }
          : { kind: 'dropPartition', partition: { name: change.partition } }
  refuseWhenBlocked(previewChange(s, rec, t, req))
  return startTask(
    s,
    rec,
    t,
    'table.partition',
    ['Check the request', 'Change the partitions on every server'],
    () => {
      const now = partitionsOf(s, rec, t)
      const leaves = t.distribution?.bucketCount ?? 32
      if (change.kind === 'create') {
        if (!now.some((x) => x.name === change.body.name))
          setPartitions(s, rec, t, [
            ...now,
            partitionOf(change.body.bound.trim(), method, leaves, change.body.name),
          ])
      } else if (change.kind === 'attach') {
        const other = findTable(rec, change.body.table)
        if (other) {
          setPartitions(s, rec, t, [
            ...now,
            partitionOf(change.body.bound.trim(), method, leaves, other.name),
          ])
          rec.tables = rec.tables.filter((x) => x !== other)
          rec.info.stats.tableCount = Math.max(0, (rec.info.stats.tableCount ?? 1) - 1)
          recount(rec)
        }
      } else if (change.kind === 'detach') {
        const gone = now.find((x) => x.name === change.partition)
        setPartitions(
          s,
          rec,
          t,
          now.filter((x) => x.name !== change.partition),
        )
        if (gone && t.distribution) {
          rec.tables.push({
            schema: t.schema,
            name: gone.name,
            classes: ['distributed'],
            distribution: { ...t.distribution },
            ledger: null,
            homeShardGroupId: null,
            estimatedRows: 0,
            sizeBytes: gone.sizeBytes,
          })
          rec.info.stats.tableCount = (rec.info.stats.tableCount ?? 0) + 1
          recount(rec)
        }
      } else {
        setPartitions(
          s,
          rec,
          t,
          now.filter((x) => x.name !== change.partition),
        )
      }
    },
    { cancellable: false },
  )
}
