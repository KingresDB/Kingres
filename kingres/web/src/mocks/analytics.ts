// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Phase 12 in the mock (contract 0.18.0): approximate aggregates of a database, the storage format of a table and of one partition,
// and the time-partitioning policy of a table with its runs, its plan (a dry run) and its alerts. The mock is the contract in
// executable form: every task is a 202 with a task of the kind the contract names, a request that the contract refuses is refused
// with the problem code the contract names (and nothing is changed), a second request for a subject that a task holds is
// 409 `operation_in_progress`, and `configureApproximateAggregates` and `putPartitionPolicy` are idempotent. The URL switch
// `?analytics=` (see `applyAnalyticsSwitch`) reaches the states that are hard to reach by hand.
import type { components } from '@/api/schema'
import type { Table, Task } from '@/api/types'
import { isoSeconds, splitIso, type DurationUnit } from '@/lib/analytics'
import { accepted, noContent, ok, paginate, problem, validationFailed, type Handler } from './http'
import type { OperationId } from '@/api/operations.generated'
import { COLUMN_DEFS, columnDefsOf, partitionsOf, setPartitionList } from './tables'
import { createTask, mockConfig, nowIso, tickTasks, type DbRec, type MockState } from './state'

type S = components['schemas']
type Policy = S['PartitionPolicy']
type PolicyRequest = S['PartitionPolicyRequest']
type Action = S['PartitionAction']
type Run = S['PartitionRun']

const MiB = 1024 * 1024
const DAY = 86_400_000

/** What the mock keeps for phase 12, by database or by `database/schema.table`. */
export interface AnalyticsState {
  aggregates: Record<
    string,
    {
      enabled: boolean
      countDistinct: 'exact' | 'approximate'
      errorRate: number | null
      versions: Record<string, string | null>
    }
  >
  policies: Record<string, { req: Required<PolicyRequest>; createdBy: string; updatedAt: string }>
  runs: Record<string, Run[]>
  nextRunId: number
  knobs: {
    /** `missing`: a server has no package for kingres_sketch; `mismatch`: two versions; `hash`: the known answer differs. */
    sketch: 'ok' | 'missing' | 'mismatch' | 'hash'
    /** kingres_columnar is not installed in the database (or not preloaded): 409 `extension_missing` for columnar. */
    columnarMissing: boolean
    /** kingres_timepart is not installed: a policy or a partition conversion is 409 `extension_missing`. */
    timepartMissing: boolean
    /** The next conversion fails in its copy; the next run has an action that fails with this detail. */
    convertFails: boolean
    runAction: null | 'lock_timeout' | 'name_conflict'
    /** The whole next run fails (the task ends failed). */
    runFails: boolean
    /** A table is in a publication: a conversion of it is refused. */
    publication: boolean
  }
}

export function analyticsOf(s: MockState): AnalyticsState {
  s.analytics ??= {
    aggregates: {},
    policies: {},
    runs: {},
    nextRunId: 1,
    knobs: {
      sketch: 'ok',
      columnarMissing: false,
      timepartMissing: false,
      convertFails: false,
      runAction: null,
      runFails: false,
      publication: false,
    },
  }
  return s.analytics
}

const keyOf = (rec: DbRec, t: Pick<Table, 'schema' | 'name'>) =>
  `${rec.info.name}/${t.schema}.${t.name}`

// --- the time arithmetic of the scheduler ----------------------------------------------------------------

const UNIT_STEP: Record<DurationUnit, number> = {
  hour: 3_600_000,
  day: DAY,
  week: 7 * DAY,
  month: 30 * DAY,
  year: 365 * DAY,
}
function addInterval(d: Date, count: number, unit: DurationUnit): Date {
  const r = new Date(d.getTime())
  if (unit === 'month') r.setUTCMonth(r.getUTCMonth() + count)
  else if (unit === 'year') r.setUTCFullYear(r.getUTCFullYear() + count)
  else r.setTime(r.getTime() + count * UNIT_STEP[unit])
  return r
}
function floorTo(d: Date, unit: DurationUnit): Date {
  const r = new Date(d.getTime())
  r.setUTCMinutes(0, 0, 0)
  if (unit === 'hour') return r
  r.setUTCHours(0)
  if (unit === 'week') r.setUTCDate(r.getUTCDate() - ((r.getUTCDay() + 6) % 7))
  if (unit === 'month' || unit === 'year') r.setUTCDate(1)
  if (unit === 'year') r.setUTCMonth(0)
  return r
}
const iso = (d: Date) => d.toISOString().replace('.000Z', 'Z')
/** The text of a bound: a date at midnight is `2026-01-01`, anything else the full timestamp. */
const boundText = (d: Date) =>
  d.getUTCHours() + d.getUTCMinutes() + d.getUTCSeconds() === 0 ? iso(d).slice(0, 10) : iso(d)
const parseBound = (text: string | null | undefined): Date | null => {
  if (!text) return null
  const d = new Date(text.length === 10 ? `${text}T00:00:00Z` : text)
  return Number.isNaN(d.getTime()) ? null : d
}
const pad = (n: number) => String(n).padStart(2, '0')
const partitionName = (table: string, from: Date, unit: DurationUnit) =>
  `${table}_p${from.getUTCFullYear()}${pad(from.getUTCMonth() + 1)}${pad(from.getUTCDate())}${unit === 'hour' ? pad(from.getUTCHours()) : ''}`
/** A length of time as an ISO 8601 duration (the health line): whole days, or whole hours below a day. */
function isoFromMs(ms: number): string {
  const m = Math.max(0, ms)
  if (m >= DAY) return `P${Math.floor(m / DAY)}D`
  return `PT${Math.floor(m / 3_600_000)}H`
}

const TIME_TYPES = ['date', 'timestamp', 'timestamptz']
/** The type of the partition column when it is a time column of a range-partitioned table, else null. */
export function timeColumnType(t: Table): 'date' | 'timestamp' | 'timestamptz' | null {
  const key = t.partitioning?.key
  if (!key || t.partitioning?.method !== 'range') return null
  const type = columnDefsOf(t).find((c) => c.name === key)?.type
  return type && TIME_TYPES.includes(type) ? (type as 'date' | 'timestamp' | 'timestamptz') : null
}

// --- tables as the contract shows them: storage, partitions, policy -----------------------------------

/** Tables that have no index (a columnar table has none in this version); every other table has its keys' indexes. */
const NO_INDEXES = new Set(['clicks', 'metrics', 'readings'])
const TRIGGERS: Record<string, string[]> = { exports: ['exports_audit'] }

function columnarStats(p: S['TablePartition'][]): S['ColumnarStats'] | null {
  const col = p.filter((x) => x.accessMethod === 'columnar')
  if (col.length === 0) return null
  const rows = col.reduce((a, x) => a + (x.estimatedRows ?? 0), 0)
  const onDisk = col.reduce((a, x) => a + (x.sizeBytes ?? 0), 0)
  const ratios = col.map((x) => x.compressionRatio ?? 1)
  const ratio = ratios.reduce((a, b) => a + b, 0) / ratios.length
  return {
    compression: 'zstd',
    compressionLevel: 3,
    stripeRowCount: 150_000,
    chunkRowCount: 10_000,
    stripes: Math.max(1, Math.ceil(rows / 150_000)),
    rows,
    bytesOnDisk: onDisk,
    bytesUncompressed: Math.round(onDisk * ratio),
    compressionRatio: Math.round(ratio * 10) / 10,
  }
}

/** A table with what 0.18.0 adds: its `storage`, and the time key, policy and default partition of its `partitioning`. */
export function describeTable(s: MockState, rec: DbRec, t: Table): Table {
  const a = analyticsOf(s)
  if (!t.partitioning) {
    return {
      ...t,
      storage: t.storage ?? { accessMethod: 'heap', partitionFormats: null, columnar: null },
    }
  }
  const list = partitionsOf(s, rec, t)
  const heap = list.filter((x) => (x.accessMethod ?? 'heap') === 'heap').length
  const columnar = list.filter((x) => x.accessMethod === 'columnar').length
  const columnType = timeColumnType(t)
  return {
    ...t,
    partitioning: {
      ...t.partitioning,
      ...(columnType ? { columnType } : {}),
      hasPolicy: keyOf(rec, t) in a.policies,
      defaultPartition: list.some((x) => x.isDefault),
    },
    storage: {
      accessMethod: null,
      partitionFormats: { heap, columnar, other: list.length - heap - columnar },
      columnar: columnarStats(list),
    },
  }
}

/** The partitions with what 0.18.0 adds, for the listing. */
export const describePartitions = (s: MockState, rec: DbRec, t: Table): S['TablePartition'][] =>
  partitionsOf(s, rec, t).map((p) => ({
    ...p,
    accessMethod: p.accessMethod ?? 'heap',
    rangeFrom: p.rangeFrom ?? null,
    rangeTo: p.rangeTo ?? null,
    estimatedRows: p.estimatedRows ?? Math.round((p.sizeBytes ?? 0) / 96),
    compressionRatio: p.compressionRatio ?? null,
  }))

// --- approximate aggregates -----------------------------------------------------------------------------

const serverIds = (s: MockState): string[] => s.servers.map((x) => x.id)

function aggregatesOf(s: MockState, name: string) {
  const a = analyticsOf(s)
  return (a.aggregates[name] ??= {
    enabled: false,
    countDistinct: 'exact',
    errorRate: null,
    versions: Object.fromEntries(serverIds(s).map((id) => [id, null])),
  })
}

function aggregatesView(s: MockState, name: string): S['ApproximateAggregates'] {
  const rec = aggregatesOf(s, name)
  const ids = serverIds(s)
  const versions = ids.map((id) => rec.versions[id] ?? null)
  const present = versions.filter((v): v is string => v !== null)
  const same = present.length === ids.length && new Set(present).size === 1
  const extensionVersion = same ? (present[0] ?? null) : null
  return {
    enabled: rec.enabled,
    extensionVersion,
    countDistinct: rec.countDistinct,
    errorRate: rec.countDistinct === 'approximate' ? rec.errorRate : null,
    servers: ids.map((serverId, i) => {
      const v = versions[i] ?? null
      return {
        serverId,
        installedVersion: v,
        state:
          v === null
            ? ('notInstalled' as const)
            : same
              ? ('ok' as const)
              : ('versionMismatch' as const),
      }
    }),
  }
}

function databaseOf(s: MockState, name: string): DbRec {
  const rec = s.databases.find((d) => d.info.name === name)
  if (!rec) throw problem('not_found', `Database "${name}" does not exist.`)
  return rec
}

function configureAggregates(
  s: MockState,
  name: string,
  b: S['ApproximateAggregatesRequest'],
): Task {
  const rec = databaseOf(s, name)
  const errors: { field: string; message: string }[] = []
  if (typeof b.enabled !== 'boolean')
    errors.push({ field: 'enabled', message: 'Say true or false.' })
  const mode = b.countDistinct ?? 'exact'
  if (mode !== 'exact' && mode !== 'approximate')
    errors.push({ field: 'countDistinct', message: 'Use exact or approximate.' })
  if (b.enabled === false && mode === 'approximate')
    errors.push({
      field: 'countDistinct',
      message: 'The approximate mode needs the setting to be on.',
    })
  const rate = b.errorRate ?? 0.01
  if (typeof rate !== 'number' || !(rate >= 0.0005 && rate <= 0.1))
    errors.push({
      field: 'errorRate',
      message: 'Use a rate from 0.0005 to 0.1 (0.05 to 10 percent).',
    })
  if (errors.length > 0) throw validationFailed(errors)
  tickTasks(s)
  const busy = s.tasks.some(
    (r) =>
      r.task.state === 'running' &&
      r.task.kind.startsWith('database.') &&
      r.task.subject?.id === name,
  )
  if (busy)
    throw problem(
      'operation_in_progress',
      'Another change of this database is running. Try again when it has ended.',
    )
  const knobs = analyticsOf(s).knobs
  const ids = serverIds(s)
  if (b.enabled && knobs.sketch === 'missing')
    throw problem(
      'extension_unavailable',
      `Server ${ids[ids.length - 1] ?? ''} has no package for kingres_sketch. Nothing was changed.`,
    )
  if (b.enabled && knobs.sketch === 'mismatch')
    throw problem(
      'extension_version_mismatch',
      `Servers ${ids.slice(0, 2).join(' and ')} run different versions of kingres_sketch (1.0 and 0.9). Nothing was changed.`,
    )
  const steps = b.enabled
    ? [
        'Check the servers',
        ...ids.map((id) => `Install the extension on server ${id}`),
        'Verify versions and hashes',
        'Apply the database settings',
        'Record the settings',
      ]
    : ['Apply the database settings', 'Record the settings']
  const hashFails = b.enabled === true && knobs.sketch === 'hash'
  return createTask(s, {
    kind: 'database.approximateAggregates',
    subject: { type: 'database', id: name },
    steps,
    cancellable: false,
    failAt: hashFails
      ? {
          fraction: ((ids.length + 1) / steps.length) * 0.99,
          code: 'sketch_hash_mismatch',
          message: `The known-answer values of kingres_sketch hash differently on server ${ids[ids.length - 1] ?? ''}. Nothing was recorded.`,
        }
      : undefined,
    apply: () => {
      const cur = aggregatesOf(s, name)
      cur.enabled = b.enabled
      if (b.enabled) for (const id of ids) cur.versions[id] = '1.0'
      cur.countDistinct = b.enabled ? mode : 'exact'
      cur.errorRate = cur.countDistinct === 'approximate' ? rate : null
      rec.info.approximateAggregates = {
        enabled: cur.enabled,
        countDistinct: cur.countDistinct,
        errorRate: cur.errorRate,
      } as typeof rec.info.approximateAggregates
    },
  })
}

// --- storage --------------------------------------------------------------------------------------------

function findTable(rec: DbRec, schema: string, name: string): Table {
  const t = rec.tables.find((x) => x.schema === schema && x.name === name)
  if (!t) throw problem('not_found', 'Table does not exist.')
  return t
}

const holdsTable = (s: MockState, id: string, kinds: string[]): boolean => {
  tickTasks(s)
  return s.tasks.some(
    (r) => r.task.state === 'running' && kinds.includes(r.task.kind) && r.task.subject?.id === id,
  )
}

function checkStorageBody(b: S['PartitionStorageRequest']): void {
  const errors: { field: string; message: string }[] = []
  if (b.accessMethod !== 'heap' && b.accessMethod !== 'columnar')
    errors.push({ field: 'accessMethod', message: 'Use heap or columnar.' })
  if (b.compression !== undefined && !['none', 'pglz', 'lz4', 'zstd'].includes(b.compression))
    errors.push({ field: 'compression', message: 'Use none, pglz, lz4 or zstd.' })
  if (
    b.compressionLevel !== undefined &&
    !(Number.isInteger(b.compressionLevel) && b.compressionLevel >= 1 && b.compressionLevel <= 19)
  )
    errors.push({ field: 'compressionLevel', message: 'Use a whole number from 1 to 19.' })
  if (
    b.stripeRowCount !== undefined &&
    !(Number.isInteger(b.stripeRowCount) && b.stripeRowCount >= 1000)
  )
    errors.push({ field: 'stripeRowCount', message: 'Use a whole number of 1000 or more.' })
  if (
    b.chunkRowCount !== undefined &&
    !(Number.isInteger(b.chunkRowCount) && b.chunkRowCount >= 100)
  )
    errors.push({ field: 'chunkRowCount', message: 'Use a whole number of 100 or more.' })
  if (errors.length > 0) throw validationFailed(errors)
}

function refuseConversion(reasons: string[]): void {
  if (reasons.length > 0) throw problem('conversion_refused', reasons.join('\n'), undefined)
}

function requireExtensions(s: MockState, target: string, partition: boolean): void {
  const k = analyticsOf(s).knobs
  if (target === 'columnar' && k.columnarMissing)
    throw problem(
      'extension_missing',
      'kingres_columnar is not installed in this database, or its library is not preloaded on server 10.0.0.12 (shared_preload_libraries).',
    )
  if (partition && k.timepartMissing)
    throw problem('extension_missing', 'kingres_timepart is not installed in this database.')
}

/** What a conversion of a table or partition cannot be done for: the reasons the 409 lists. */
function reasonsFor(s: MockState, t: Table, target: string): string[] {
  const r: string[] = []
  if (t.classes.includes('ledger')) r.push('is a ledger table')
  if (target === 'columnar') {
    if (!NO_INDEXES.has(t.name) && !t.name.startsWith('metrics_'))
      r.push(`has an index (${t.name}_pkey): columnar tables have no indexes in this version`)
    for (const trg of TRIGGERS[t.name] ?? []) r.push(`has a trigger (${trg})`)
  }
  if (analyticsOf(s).knobs.publication) r.push('is in a publication')
  return r
}

function startSetStorage(s: MockState, rec: DbRec, t: Table, b: S['TableStorageRequest']): Task {
  checkStorageBody(b)
  if (t.partitioning)
    throw problem(
      'conversion_refused',
      'a partition that is a parent: convert one partition at a time, or set a time-partitioning policy',
    )
  if (t.distribution)
    throw problem(
      'conversion_refused',
      'is distributed: converting a distributed table is not available in this version',
    )
  requireExtensions(s, b.accessMethod, false)
  if ((t.storage?.accessMethod ?? 'heap') === b.accessMethod && b.accessMethod === 'heap')
    throw problem('conversion_refused', 'is stored as rows already')
  refuseConversion(reasonsFor(s, t, b.accessMethod))
  const id = keyOf(rec, t)
  if (
    holdsTable(s, id, [
      'table.setStorage',
      'table.distribute',
      'table.undistribute',
      'table.alter',
      'table.partition',
    ])
  )
    throw problem(
      'operation_in_progress',
      'A change of this table is running. Try again when it has ended.',
    )
  const fails = analyticsOf(s).knobs.convertFails
  const taskId = s.nextTaskId
  return createTask(s, {
    kind: 'table.setStorage',
    subject: { type: 'table', id },
    steps: ['check', 'convert', 'analyze'],
    detail:
      'ALTER TABLE ... SET ACCESS METHOD takes an ACCESS EXCLUSIVE lock on the table until it ends.',
    cancellable: true,
    durationMs: mockConfig.tableSlow ? 60_000 : undefined,
    uncapped: mockConfig.tableSlow,
    failAt: fails
      ? {
          fraction: 0.5,
          code: 'insufficient_space',
          message:
            'Not enough free space on server 10.0.0.12 for the new copy of the table. The table is as it was.',
        }
      : undefined,
    onProgress: (f) => {
      const r = s.tasks.find((x) => x.task.id === taskId)
      if (r && r.task.state === 'running' && f >= 0.7) r.task.cancellable = false
    },
    apply: () => {
      if (b.accessMethod === 'columnar') {
        const size = t.sizeBytes ?? 0
        const ratio = 6.2
        const onDisk = Math.round(size / ratio)
        const rows = t.estimatedRows ?? 0
        t.storage = {
          accessMethod: 'columnar',
          partitionFormats: null,
          columnar: {
            compression: b.compression ?? 'zstd',
            compressionLevel: b.compressionLevel ?? 3,
            stripeRowCount: b.stripeRowCount ?? 150_000,
            chunkRowCount: b.chunkRowCount ?? 10_000,
            stripes: Math.max(1, Math.ceil(rows / (b.stripeRowCount ?? 150_000))),
            rows,
            bytesOnDisk: onDisk,
            bytesUncompressed: size,
            compressionRatio: ratio,
          },
        }
        t.sizeBytes = onDisk
      } else {
        const col = t.storage?.columnar
        t.storage = { accessMethod: 'heap', partitionFormats: null, columnar: null }
        if (col) t.sizeBytes = col.bytesUncompressed
      }
    },
  })
}

function startSetPartitionStorage(
  s: MockState,
  rec: DbRec,
  t: Table,
  partition: string,
  b: S['PartitionStorageRequest'],
): Task {
  const list = partitionsOf(s, rec, t)
  const p = list.find((x) => x.name === partition)
  if (!t.partitioning || !p) throw problem('not_found', `Partition "${partition}" does not exist.`)
  checkStorageBody(b)
  const known = columnDefsOf(t).map((c) => c.name)
  const bad = (b.sortBy ?? []).find((c) => !known.includes(c))
  if (bad !== undefined)
    throw validationFailed([{ field: 'sortBy', message: `The table has no column "${bad}".` }])
  requireExtensions(s, b.accessMethod, true)
  const reasons: string[] = []
  if (p.isDefault) reasons.push('is a default partition')
  if (t.distribution)
    reasons.push(
      'belongs to a distributed table: converting its partitions is not available in this version',
    )
  if ((p.accessMethod ?? 'heap') === b.accessMethod)
    reasons.push(`is stored as ${b.accessMethod === 'heap' ? 'rows' : 'columns'} already`)
  if (b.accessMethod === 'columnar') {
    for (const trg of TRIGGERS[t.name] ?? []) reasons.push(`has a trigger (${trg})`)
    if (!NO_INDEXES.has(t.name))
      reasons.push(
        `the table has an index (${t.name}_pkey): columnar partitions have no indexes in this version`,
      )
  }
  if (analyticsOf(s).knobs.publication) reasons.push('is in a publication')
  refuseConversion(reasons)
  const subject = `${rec.info.name}/${t.schema}.${partition}`
  if (
    holdsTable(s, subject, ['table.setStorage']) ||
    holdsTable(s, keyOf(rec, t), ['partition.maintain'])
  )
    throw problem(
      'operation_in_progress',
      'A change of this partition, or a run of its policy, is running.',
    )
  const fails = analyticsOf(s).knobs.convertFails
  return createTask(s, {
    kind: 'table.setStorage',
    subject: { type: 'table', id: subject },
    steps: ['check', 'convert', 'analyze'],
    cancellable: true,
    failAt: fails
      ? {
          fraction: 0.5,
          code: 'insufficient_space',
          message: `Not enough free space to build the new copy of ${partition}. The partition is as it was.`,
        }
      : undefined,
    apply: () => {
      convertPartition(s, rec, t, partition, b.accessMethod, b.compression)
    },
  })
}

function convertPartition(
  s: MockState,
  rec: DbRec,
  t: Table,
  name: string,
  to: 'heap' | 'columnar',
  _compression?: string,
): { before: number; after: number } {
  void _compression
  const list = partitionsOf(s, rec, t)
  const p = list.find((x) => x.name === name)
  if (!p) return { before: 0, after: 0 }
  const before = p.sizeBytes ?? 0
  if (to === 'columnar') {
    p.accessMethod = 'columnar'
    p.compressionRatio = 6.2
    p.sizeBytes = Math.round(before / 6.2)
  } else {
    const ratio = p.compressionRatio ?? 1
    p.accessMethod = 'heap'
    p.compressionRatio = null
    p.sizeBytes = Math.round(before * ratio)
  }
  setPartitionList(s, rec, t, list)
  return { before, after: p.sizeBytes ?? 0 }
}

// --- the policy -----------------------------------------------------------------------------------------

const ISO_RE = /^P(?!$)(\d+Y)?(\d+M)?(\d+W)?(\d+D)?(T(?=\d)(\d+H)?(\d+M)?(\d+S)?)?$/
const HOUR_DIVISORS = [1, 2, 3, 4, 6, 8, 12]

function validatePolicy(b: Partial<PolicyRequest>, timezones = true): Required<PolicyRequest> {
  const errors: { field: string; message: string }[] = []
  const bad = (field: string, message: string) => errors.push({ field: `/${field}`, message })
  const duration = (field: string, v: unknown, required: boolean): string | null => {
    if (v === null || v === undefined) {
      if (required) bad(field, 'Give a duration such as P1D, P1M or PT6H.')
      return null
    }
    if (typeof v !== 'string' || !ISO_RE.test(v) || isoSeconds(v) === null) {
      bad(field, 'Use a duration of one unit in ISO 8601, such as P1D, P1M or PT6H.')
      return null
    }
    return v
  }
  const every = duration('partitionEvery', b.partitionEvery, true)
  if (every) {
    const sp = splitIso(every)
    if (sp?.unit === 'hour' && !HOUR_DIVISORS.includes(sp.count))
      bad('partitionEvery', 'Use a number of hours that divides a day: 1, 2, 3, 4, 6, 8 or 12.')
    if (sp && sp.unit !== 'hour' && sp.unit !== 'day' && sp.count !== 1)
      bad('partitionEvery', 'A week, a month and a year can be used once: P1W, P1M, P1Y.')
    if (sp?.unit === 'day' && sp.count > 31) bad('partitionEvery', 'Use at most 31 days.')
  }
  const ahead = duration('createAhead', b.createAhead ?? 'P7D', false) ?? 'P7D'
  const retention = duration('retention', b.retention, false)
  const columnar = duration('columnarAfter', b.columnarAfter, false)
  if (retention && columnar) {
    const r = isoSeconds(retention)
    const c = isoSeconds(columnar)
    if (r !== null && c !== null && c > r)
      bad(
        'columnarAfter',
        'Convert before the retention drops the partition: use a shorter time than the retention.',
      )
  }
  const run = duration('runEvery', b.runEvery ?? 'PT1H', false) ?? 'PT1H'
  const action = b.retentionAction ?? 'drop'
  if (action !== 'drop' && action !== 'detach') bad('retentionAction', 'Use drop or detach.')
  const level = b.compressionLevel ?? null
  if (level !== null && !(Number.isInteger(level) && level >= 1 && level <= 19))
    bad('compressionLevel', 'Use a whole number from 1 to 19.')
  if (b.compression != null && !['none', 'pglz', 'lz4', 'zstd'].includes(b.compression))
    bad('compression', 'Use none, pglz, lz4 or zstd.')
  const tz = b.timezone ?? 'UTC'
  if (timezones) {
    try {
      new Intl.DateTimeFormat('en', { timeZone: tz })
    } catch {
      bad('timezone', 'This is not a time zone name known to the server.')
    }
  }
  if (b.enabled !== undefined && typeof b.enabled !== 'boolean')
    bad('enabled', 'Say true or false.')
  if (errors.length > 0) throw validationFailed(errors)
  return {
    partitionEvery: every as string,
    createAhead: ahead,
    retention: retention,
    retentionAction: action,
    columnarAfter: columnar,
    columnarSortBy: b.columnarSortBy ?? null,
    compression: b.compression ?? null,
    compressionLevel: level,
    timezone: tz,
    runEvery: run,
    enabled: b.enabled ?? true,
  }
}

function lastRunSummary(s: MockState, key: string): S['PartitionRunSummary'] | null {
  syncRuns(s)
  const r = analyticsOf(s).runs[key]?.[0]
  return r ? { id: r.id, state: r.state, startedAt: r.startedAt, finishedAt: r.finishedAt } : null
}

function policyView(s: MockState, rec: DbRec, t: Table): Policy {
  const a = analyticsOf(s)
  const key = keyOf(rec, t)
  const p = a.policies[key]
  if (!p) throw problem('not_found', 'The table has no time-partitioning policy.')
  const list = partitionsOf(s, rec, t)
  const ends = list.filter((x) => !x.isDefault).map((x) => parseBound(x.rangeTo)?.getTime() ?? 0)
  const last = Math.max(0, ...ends)
  const now = Date.now()
  const ahead = ends.length > 0 ? isoFromMs(last - now) : null
  const lastRun = lastRunSummary(s, key)
  const floor = (isoSeconds(p.req.createAhead) ?? 0) / 2
  const problems: Policy['health']['problems'] = []
  if (ahead !== null && (last - now) / 1000 < floor) problems.push('aheadLow')
  if (ahead === null) problems.push('aheadLow')
  if (lastRun?.state === 'failed') problems.push('lastRunFailed')
  if (list.some((x) => x.isDefault)) problems.push('defaultPartitionPresent')
  const type = timeColumnType(t) ?? 'timestamptz'
  return {
    ...p.req,
    partitionColumn: t.partitioning?.key ?? '',
    partitionColumnType: type,
    nextRunAt: p.req.enabled
      ? new Date(
          Math.max(now, Date.parse(lastRun?.startedAt ?? nowIso())) +
            (isoSeconds(p.req.runEvery) ?? 3600) * 1000,
        ).toISOString()
      : null,
    lastRun,
    health: { partitionsAhead: ahead, ok: problems.length === 0, problems },
    createdBy: p.createdBy,
    updatedAt: p.updatedAt,
  }
}

function putPolicy(s: MockState, rec: DbRec, t: Table, b: Partial<PolicyRequest>): Policy {
  const req = validatePolicy(b)
  if (!timeColumnType(t))
    throw problem(
      'partition_policy_conflict',
      'The table is not range-partitioned on a date, timestamp or timestamptz column.',
    )
  if (analyticsOf(s).knobs.timepartMissing)
    throw problem('extension_missing', 'kingres_timepart is not installed in this database.')
  const key = keyOf(rec, t)
  const a = analyticsOf(s)
  const before = a.policies[key]
  a.policies[key] = {
    req,
    createdBy: before?.createdBy ?? s.session?.username ?? 'admin',
    updatedAt: nowIso(),
  }
  // The scheduler takes the table at its next tick: the next run is now.
  const view = policyView(s, rec, t)
  return { ...view, nextRunAt: req.enabled ? nowIso() : null }
}

/** The actions that a run would take at `asOf` with this policy (a plan: nothing is changed). */
export function planActions(
  s: MockState,
  rec: DbRec,
  t: Table,
  p: Required<PolicyRequest>,
  asOf: Date,
): Action[] {
  const sp = splitIso(p.partitionEvery)
  if (!sp) return []
  const list = partitionsOf(s, rec, t).filter((x) => !x.isDefault)
  const actions: Action[] = []
  const add = (a: Omit<Action, 'seq' | 'state'> & { state?: Action['state'] }) =>
    actions.push({ seq: actions.length + 1, state: 'planned', ...a })
  // create: from the end of the last partition (or the interval that holds `asOf`) up to `createAhead` from `asOf`
  const aheadMs = (isoSeconds(p.createAhead) ?? 0) * 1000
  let from =
    list
      .map((x) => parseBound(x.rangeTo))
      .filter((d): d is Date => d !== null)
      .sort((x, y) => y.getTime() - x.getTime())[0] ?? floorTo(asOf, sp.unit)
  const taken = new Set([...list.map((x) => x.name), ...rec.tables.map((x) => x.name)])
  let guard = 0
  while (from.getTime() < asOf.getTime() + aheadMs && guard < 400) {
    const to = addInterval(from, sp.count, sp.unit)
    const name = partitionName(t.name, from, sp.unit)
    const conflict = taken.has(name)
    add({
      kind: 'create',
      partition: name,
      rangeFrom: iso(from),
      rangeTo: iso(to),
      ...(conflict ? { state: 'skipped' as const, detail: 'name_conflict' } : {}),
      bytesBefore: null,
      bytesAfter: null,
    })
    from = to
    guard += 1
  }
  const retired = new Set<string>()
  const retention = isoSeconds(p.retention)
  if (retention !== null) {
    for (const x of list) {
      const end = parseBound(x.rangeTo)
      if (end && end.getTime() <= asOf.getTime() - retention * 1000) {
        retired.add(x.name)
      }
    }
  }
  const colAfter = isoSeconds(p.columnarAfter)
  if (colAfter !== null) {
    for (const x of list) {
      const end = parseBound(x.rangeTo)
      if (
        end &&
        (x.accessMethod ?? 'heap') === 'heap' &&
        !retired.has(x.name) &&
        end.getTime() <= asOf.getTime() - colAfter * 1000
      )
        add({
          kind: 'convert',
          partition: x.name,
          rangeFrom: parseBound(x.rangeFrom)?.toISOString() ?? null,
          rangeTo: end.toISOString(),
          bytesBefore: x.sizeBytes,
          bytesAfter: x.sizeBytes === null ? null : Math.round(x.sizeBytes / 6.2),
        })
    }
  }
  for (const x of list)
    if (retired.has(x.name))
      add({
        kind: p.retentionAction === 'detach' ? 'detach' : 'drop',
        partition: x.name,
        rangeFrom: parseBound(x.rangeFrom)?.toISOString() ?? null,
        rangeTo: parseBound(x.rangeTo)?.toISOString() ?? null,
        bytesBefore: x.sizeBytes,
        bytesAfter: null,
      })
  return actions
}

/** A run that was left `running` by a task that ended without finishing it (failed, cancelled) is closed here. */
function syncRuns(s: MockState): void {
  for (const list of Object.values(analyticsOf(s).runs))
    for (const r of list) {
      if (r.state !== 'running' || r.taskId === null) continue
      const rec = s.tasks.find((x) => x.task.id === r.taskId)
      if (rec?.task.state === 'failed') {
        r.state = 'failed'
        r.finishedAt = rec.task.updatedAt
        r.error = rec.task.error?.message ?? 'The task failed.'
        r.actions = r.actions.map((x) =>
          x.state === 'done' || x.state === 'failed' ? x : { ...x, state: 'skipped' },
        )
      }
    }
}

function startRun(s: MockState, rec: DbRec, t: Table, asOfText?: string): Task {
  const a = analyticsOf(s)
  const key = keyOf(rec, t)
  const policy = a.policies[key]
  if (!policy) throw problem('not_found', 'The table has no time-partitioning policy.')
  let asOf = new Date()
  if (asOfText !== undefined) {
    const d = new Date(asOfText)
    if (Number.isNaN(d.getTime()))
      throw validationFailed([{ field: '/asOf', message: 'Give a date and time.' }])
    asOf = d
  }
  tickTasks(s)
  if (holdsTable(s, key, ['partition.maintain']))
    throw problem(
      'operation_in_progress',
      'A run of this policy is going on. Try again when it has ended.',
    )
  const planned = planActions(s, rec, t, policy.req, asOf)
  const run: Run = {
    id: a.nextRunId++,
    asOf: iso(asOf),
    trigger: 'manual',
    state: 'running',
    startedAt: nowIso(),
    finishedAt: null,
    startedBy: s.session?.username ?? 'admin',
    taskId: null,
    error: null,
    actions: planned,
  }
  const runs = (a.runs[key] ??= [])
  runs.unshift(run)
  const knobs = a.knobs
  const task = createTask(s, {
    kind: 'partition.maintain',
    subject: { type: 'table', id: key },
    steps: ['begin', 'create', 'convert', 'retire', 'finish'],
    cancellable: true,
    failWith: knobs.runFails
      ? 'The run could not take the lock on the table and gave up.'
      : undefined,
    apply: () => finishRun(s, rec, t, run),
  })
  run.taskId = task.id
  return task
}

/** Does what the plan said, action by action, and closes the run (`done`, `partial` or `failed`). */
function finishRun(s: MockState, rec: DbRec, t: Table, run: Run): void {
  const knobs = analyticsOf(s).knobs
  const method = t.partitioning?.method ?? 'range'
  const now = nowIso()
  let failures = 0
  run.actions = run.actions.map((act) => {
    const base = { ...act, startedAt: now, finishedAt: now }
    if (act.state === 'skipped') {
      failures += 1
      return { ...base, state: 'failed' as const }
    }
    if (
      knobs.runAction &&
      act.kind === (knobs.runAction === 'name_conflict' ? 'create' : 'convert')
    ) {
      const detail = knobs.runAction
      knobs.runAction = null
      failures += 1
      return { ...base, state: 'failed' as const, detail }
    }
    const list = partitionsOf(s, rec, t)
    if (act.kind === 'create') {
      setPartitionList(s, rec, t, [
        ...list.filter((x) => !x.isDefault),
        {
          name: act.partition,
          method,
          bound: `FROM ('${boundText(new Date(act.rangeFrom as string))}') TO ('${boundText(new Date(act.rangeTo as string))}')`,
          isDefault: false,
          leafCount: t.distribution?.bucketCount ?? 1,
          sizeBytes: 0,
          accessMethod: 'heap',
          rangeFrom: boundText(new Date(act.rangeFrom as string)),
          rangeTo: boundText(new Date(act.rangeTo as string)),
          estimatedRows: 0,
          compressionRatio: null,
        },
        ...list.filter((x) => x.isDefault),
      ])
      return { ...base, state: 'done' as const }
    }
    if (act.kind === 'convert') {
      const r = convertPartition(s, rec, t, act.partition, 'columnar')
      return { ...base, state: 'done' as const, bytesBefore: r.before, bytesAfter: r.after }
    }
    setPartitionList(
      s,
      rec,
      t,
      list.filter((x) => x.name !== act.partition),
    )
    return { ...base, state: 'done' as const }
  })
  // A convert that the knob made fail keeps its detail in words the scheduler uses.
  run.actions = run.actions.map((x) =>
    x.state === 'failed' && x.detail == null
      ? { ...x, detail: x.kind === 'create' ? 'name_conflict' : 'lock_timeout' }
      : x,
  )
  const total = run.actions.length
  run.state = failures === 0 ? 'done' : failures === total && total > 0 ? 'failed' : 'partial'
  run.finishedAt = now
  run.error =
    failures > 0
      ? `${failures} of ${total} actions did not finish. They are tried again at the next run.`
      : null
  if (failures > 0 && run.state !== 'done') raisePolicyAlert(s, rec, t, 'partitionPolicyFailed')
}

// --- alerts ---------------------------------------------------------------------------------------------

export function raisePolicyAlert(
  s: MockState,
  rec: DbRec,
  t: Table,
  kind: 'partitionsAheadLow' | 'partitionPolicyFailed',
): void {
  const aspect = `table:${t.schema}.${t.name}`
  if (
    s.alerts.some((a) => a.kind === kind && a.state !== 'resolved' && a.subject?.aspect === aspect)
  )
    return
  s.alerts.unshift({
    id: Math.max(0, ...s.alerts.map((a) => a.id)) + 1,
    kind,
    severity: 'warning',
    state: 'open',
    title:
      kind === 'partitionsAheadLow'
        ? 'Partitions ahead are running low'
        : 'A partitioning policy failed',
    message:
      kind === 'partitionsAheadLow'
        ? `The last partition of ${t.schema}.${t.name} ends less than the policy keeps ahead.`
        : `The last run of the policy of ${t.schema}.${t.name} did not finish all its actions.`,
    serverId: null,
    databaseName: rec.info.name,
    raisedAt: nowIso(),
    subject: {
      serverId: null,
      behindServerId: null,
      shardGroupId: null,
      databaseName: rec.info.name,
      aspect,
    },
  })
}

// --- seeds ----------------------------------------------------------------------------------------------

/** The partition record of a daily partition `days` ago (negative: ahead), `n` days long. */
function daily(table: string, from: Date, ratio: number | null, size: number): S['TablePartition'] {
  const to = addInterval(from, 1, 'day')
  return {
    name: partitionName(table, from, 'day'),
    method: 'range',
    bound: `FROM ('${boundText(from)}') TO ('${boundText(to)}')`,
    isDefault: false,
    leafCount: 1,
    sizeBytes: size,
    accessMethod: ratio === null ? 'heap' : 'columnar',
    rangeFrom: boundText(from),
    rangeTo: boundText(to),
    estimatedRows: Math.round((size * (ratio ?? 1)) / 96),
    compressionRatio: ratio,
  }
}

/**
 * `?analytics=timeseries`: in the database `shop`, a plain table `clicks` with no index (it can be converted), and a plain table
 * `metrics` that is range-partitioned by day on `ts` (timestamptz): ten partitions, the two oldest columnar, a policy, and the
 * history of three runs (one of them partial, with an action that waited for a lock).
 */
export function seedTimeseries(s: MockState, ahead = 3): void {
  const rec = s.databases.find((d) => d.info.name === 'shop')
  if (!rec || rec.tables.some((t) => t.name === 'metrics')) return
  COLUMN_DEFS.clicks = [
    { name: 'click_id', type: 'bigint' },
    { name: 'url', type: 'text' },
    { name: 'clicked_at', type: 'timestamptz' },
  ]
  COLUMN_DEFS.metrics = [
    { name: 'ts', type: 'timestamptz' },
    { name: 'device', type: 'text' },
    { name: 'reading', type: 'double precision' },
  ]
  const base: Table = {
    schema: 'public',
    name: 'metrics',
    classes: ['plain'],
    distribution: null,
    ledger: null,
    homeShardGroupId: 0,
    estimatedRows: 41_000_000,
    sizeBytes: 3_400 * MiB,
    partitioning: { method: 'range', key: 'ts', partitionCount: 0 },
  }
  rec.tables.push(
    {
      schema: 'public',
      name: 'clicks',
      classes: ['plain'],
      distribution: null,
      ledger: null,
      homeShardGroupId: 0,
      estimatedRows: 2_400_000,
      sizeBytes: 380 * MiB,
    },
    base,
  )
  rec.info.stats.tableCount = (rec.info.stats.tableCount ?? 0) + 2
  const today = floorTo(new Date(), 'day')
  const list: S['TablePartition'][] = []
  // From 7 days back to `ahead` days ahead (the two oldest are columnar; the policy converts what is older than 4 days).
  for (let d = -7; d < ahead; d += 1) {
    const col = d <= -6
    list.push(
      daily('metrics', addInterval(today, d, 'day'), col ? 6.2 : null, col ? 52 * MiB : 320 * MiB),
    )
  }
  setPartitionList(s, rec, base, list)
  const a = analyticsOf(s)
  const key = keyOf(rec, base)
  a.policies[key] = {
    req: {
      partitionEvery: 'P1D',
      createAhead: 'P7D',
      retention: 'P90D',
      retentionAction: 'drop',
      columnarAfter: 'P4D',
      columnarSortBy: ['ts'],
      compression: 'zstd',
      compressionLevel: 3,
      timezone: 'UTC',
      runEvery: 'PT1H',
      enabled: true,
    },
    createdBy: 'admin',
    updatedAt: new Date(Date.now() - 3 * DAY).toISOString(),
  }
  const ago = (ms: number) => new Date(Date.now() - ms).toISOString()
  const mk = (
    ageMs: number,
    trigger: Run['trigger'],
    state: Run['state'],
    actions: Partial<Action>[],
    error: string | null = null,
  ): Run => ({
    id: a.nextRunId++,
    asOf: ago(ageMs),
    trigger,
    state,
    startedAt: ago(ageMs),
    finishedAt: ago(ageMs - 4000),
    startedBy: trigger === 'schedule' ? 'system' : 'admin',
    taskId: null,
    error,
    actions: actions.map((x, i) => ({
      seq: i + 1,
      kind: 'create',
      partition: 'x',
      state: 'done',
      detail: null,
      rangeFrom: null,
      rangeTo: null,
      bytesBefore: null,
      bytesAfter: null,
      startedAt: ago(ageMs),
      finishedAt: ago(ageMs - 1000),
      ...x,
    })),
  })
  const p = (d: number) => partitionName('metrics', addInterval(today, d, 'day'), 'day')
  a.runs[key] = [
    mk(2 * 3_600_000, 'schedule', 'done', []),
    mk(
      26 * 3_600_000,
      'manual',
      'partial',
      [
        { kind: 'create', partition: p(2) },
        {
          kind: 'convert',
          partition: p(-5),
          state: 'failed',
          detail: 'swap_lock_timeout',
          bytesBefore: 320 * MiB,
        },
      ],
      '1 of 2 actions did not finish. They are tried again at the next run.',
    ),
    mk(50 * 3_600_000, 'schedule', 'done', [
      { kind: 'create', partition: p(1) },
      { kind: 'convert', partition: p(-6), bytesBefore: 320 * MiB, bytesAfter: 52 * MiB },
    ]),
  ]
  if (ahead <= 1) raisePolicyAlert(s, rec, base, 'partitionsAheadLow')
}

/** `?analytics=on`: approximate aggregates are on in the database `shop` (on every server, version 1.0, error rate 1 percent). */
export function seedAggregatesOn(s: MockState): void {
  const rec = aggregatesOf(s, 'shop')
  rec.enabled = true
  rec.countDistinct = 'approximate'
  rec.errorRate = 0.01
  for (const id of serverIds(s)) rec.versions[id] = '1.0'
  const db = s.databases.find((d) => d.info.name === 'shop')
  if (db)
    db.info.approximateAggregates = {
      enabled: true,
      countDistinct: 'approximate',
      errorRate: 0.01,
    } as typeof db.info.approximateAggregates
}

/**
 * `?analytics=` values, comma separated: `on`, `timeseries`, `ahead-low`, `sketch:missing|mismatch|hash`, `columnar:missing`,
 * `timepart:missing`, `convert:fail`, `run:lock|name|fail`, `publication`, `old` (every operation of 0.18.0 answers
 * 422 `unsupported_operation`).
 */
export function applyAnalyticsSwitch(entry: string, s: MockState): void {
  const k = analyticsOf(s).knobs
  if (entry === 'on') seedAggregatesOn(s)
  if (entry === 'timeseries') seedTimeseries(s)
  if (entry === 'ahead-low') seedTimeseries(s, 1)
  const sk = /^sketch:(missing|mismatch|hash)$/.exec(entry)
  if (sk) k.sketch = sk[1] as typeof k.sketch
  if (entry === 'columnar:missing') k.columnarMissing = true
  if (entry === 'timepart:missing') k.timepartMissing = true
  if (entry === 'convert:fail') k.convertFails = true
  if (entry === 'run:lock') k.runAction = 'lock_timeout'
  if (entry === 'run:name') k.runAction = 'name_conflict'
  if (entry === 'run:fail') k.runFails = true
  if (entry === 'publication') k.publication = true
  if (entry === 'old') for (const op of PHASE12_OPERATIONS) mockConfig.unsupported.add(op)
}

export const PHASE12_OPERATIONS: OperationId[] = [
  'getApproximateAggregates',
  'configureApproximateAggregates',
  'setTableStorage',
  'setPartitionStorage',
  'getPartitionPolicy',
  'putPartitionPolicy',
  'deletePartitionPolicy',
  'runPartitionPolicy',
  'listPartitionPolicyRuns',
  'getPartitionPolicyRun',
  'planPartitionPolicy',
]

// --- EXPLAIN in the workbench -------------------------------------------------------------------------

/**
 * The text of an EXPLAIN of a SELECT, in the lines the sharding extension prints (section 12 of 2b, 4.12 of the query design,
 * 9.8): a join of tables of different colocation groups is repartitioned, a join inside one group is colocated, an aggregate is
 * a partial aggregate, and count(DISTINCT) is rewritten when the database has the approximate mode on. `null`: not an EXPLAIN.
 */
export function explainPlan(s: MockState, database: string, sql: string): string[] | null {
  const m = /^explain\s*(\([^)]*\)|analyze|verbose)*\s*(select\b[\s\S]*)$/i.exec(sql.trim())
  if (!m) return null
  const text = m[2] ?? ''
  const verbose = /verbose/i.test(m[1] ?? '')
  const agg = analyticsOf(s).aggregates[database]
  const approximate = agg?.enabled === true && agg.countDistinct === 'approximate'
  const distinct = /count\s*\(\s*distinct\b/i.test(text)
  const remote = 'Remote SQL: SELECT ... FROM public.orders AS ks_t1'
  if (/\bjoin\b/i.test(text) && /\bevents\b/i.test(text) && /\borders\b/i.test(text))
    return [
      'Custom Scan (Kingres Fragment Scan)',
      '  Pushed: join (orders, events, countries: repartitioned)',
      '  Repartition: events by customer_id -> placement of orders (colocation 1, 128 buckets), est 1200000 rows, 96 MB',
      '  Groups: 3 of 3 (local + 2 remote)',
      '  ' + remote,
    ]
  if (/\bjoin\b/i.test(text))
    return [
      'Custom Scan (Kingres Fragment Scan)',
      '  Pushed: join (orders, customers: key class customer_id)',
      '  Groups: 3 of 3 (local + 2 remote)',
      '  ' + remote,
    ]
  if (distinct && approximate)
    return [
      'Finalize Aggregate',
      '  Output: approx_count_distinct(ks_t1.customer_id, 14)',
      '  Rewritten: count(DISTINCT customer_id) -> approximate (p = 14)',
      '  ->  Custom Scan (Kingres Fragment Scan)',
      '        Pushed: aggregate (partial)',
      '        Groups: 3 of 3 (local + 2 remote)',
    ]
  if (/\b(sum|avg|count|min|max)\s*\(|\bgroup\s+by\b/i.test(text))
    return [
      'Finalize HashAggregate',
      '  Group Key: ks_t1.region',
      '  ->  Custom Scan (Kingres Fragment Scan)',
      '        Pushed: aggregate (partial)',
      '        Groups: 3 of 3 (local + 2 remote)',
      '        ' + remote,
    ]
  return [
    'Custom Scan (Kingres Distributed Scan) on orders',
    '  Remote groups: 3 of 3',
    '  ' + remote,
    ...(verbose ? ['  Pushdown: not used: the statement has no join or aggregate to push'] : []),
  ]
}

// --- the handlers ---------------------------------------------------------------------------------------

type Params = Record<string, string | undefined>
const tableOf = (s: MockState, params: Params) => {
  const rec = databaseOf(s, params.databaseName ?? '')
  return { rec, t: findTable(rec, params.schemaName ?? '', params.tableName ?? '') }
}

export function analyticsHandlers(): Partial<Record<OperationId, Handler>> {
  return {
    getApproximateAggregates: (c) => {
      databaseOf(c.s, c.params.databaseName ?? '')
      return ok(aggregatesView(c.s, c.params.databaseName ?? ''))
    },
    configureApproximateAggregates: (c) =>
      accepted(
        configureAggregates(
          c.s,
          c.params.databaseName ?? '',
          (c.body ?? {}) as S['ApproximateAggregatesRequest'],
        ),
      ),
    setTableStorage: (c) => {
      const { rec, t } = tableOf(c.s, c.params)
      return accepted(startSetStorage(c.s, rec, t, (c.body ?? {}) as S['TableStorageRequest']))
    },
    setPartitionStorage: (c) => {
      const { rec, t } = tableOf(c.s, c.params)
      return accepted(
        startSetPartitionStorage(
          c.s,
          rec,
          t,
          c.params.partitionName ?? '',
          (c.body ?? {}) as S['PartitionStorageRequest'],
        ),
      )
    },
    getPartitionPolicy: (c) => {
      const { rec, t } = tableOf(c.s, c.params)
      return ok(policyView(c.s, rec, t))
    },
    putPartitionPolicy: (c) => {
      const { rec, t } = tableOf(c.s, c.params)
      return ok(putPolicy(c.s, rec, t, (c.body ?? {}) as Partial<PolicyRequest>))
    },
    deletePartitionPolicy: (c) => {
      const { rec, t } = tableOf(c.s, c.params)
      const a = analyticsOf(c.s)
      const key = keyOf(rec, t)
      if (!a.policies[key]) throw problem('not_found', 'The table has no time-partitioning policy.')
      if (holdsTable(c.s, key, ['partition.maintain']))
        throw problem(
          'operation_in_progress',
          'A run of this policy is going on. Try again when it has ended.',
        )
      delete a.policies[key]
      return noContent()
    },
    runPartitionPolicy: (c) => {
      const { rec, t } = tableOf(c.s, c.params)
      const b = (c.body ?? {}) as { asOf?: string }
      return accepted(startRun(c.s, rec, t, b.asOf))
    },
    listPartitionPolicyRuns: (c) => {
      const { rec, t } = tableOf(c.s, c.params)
      syncRuns(c.s)
      const state = c.url.searchParams.get('state')
      if (state !== null && !['running', 'done', 'partial', 'failed'].includes(state))
        throw validationFailed([
          { field: 'state', message: 'Use running, done, partial or failed.' },
        ])
      const runs = (analyticsOf(c.s).runs[keyOf(rec, t)] ?? [])
        .filter((r) => state === null || r.state === state)
        .map((r) => ({ ...r, actions: [] }))
      return ok(paginate(runs, c.url))
    },
    getPartitionPolicyRun: (c) => {
      const { rec, t } = tableOf(c.s, c.params)
      syncRuns(c.s)
      const id = Number(c.params.runId)
      const run = (analyticsOf(c.s).runs[keyOf(rec, t)] ?? []).find((r) => r.id === id)
      if (!Number.isInteger(id) || !run) throw problem('not_found', 'The run does not exist.')
      return ok(run)
    },
    planPartitionPolicy: (c) => {
      const { rec, t } = tableOf(c.s, c.params)
      const b = (c.body ?? {}) as { asOf?: string; policy?: Partial<PolicyRequest> }
      let asOf = new Date()
      if (b.asOf !== undefined) {
        asOf = new Date(b.asOf)
        if (Number.isNaN(asOf.getTime()))
          throw validationFailed([{ field: '/asOf', message: 'Give a date and time.' }])
      }
      const stored = analyticsOf(c.s).policies[keyOf(rec, t)]
      if (!b.policy && !stored)
        throw problem('not_found', 'The table has no policy, and the request has none.')
      const req = b.policy ? validatePolicy(b.policy) : (stored as NonNullable<typeof stored>).req
      if (!timeColumnType(t))
        throw problem(
          'partition_policy_conflict',
          'The table is not range-partitioned on a date, timestamp or timestamptz column.',
        )
      return ok({
        asOf: iso(asOf),
        actions: planActions(c.s, rec, t, req, asOf),
      } satisfies S['PartitionPlan'])
    },
  }
}
