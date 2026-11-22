// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The words and the small rules of the analytics console (contract 0.18.0, phase 12): approximate aggregates, the storage format
// of a table and of its partitions, and the time-partitioning policy with its runs. Everything here is plain text and numbers;
// a name or a code from a newer server is never guessed, the component shows it as it was sent.
import type { components } from '@/api/schema'
import type { StatusMeta } from './status'
import type { Table, TablePartition } from '@/api/types'

type S = components['schemas']
export type PartitionPolicy = S['PartitionPolicy']
export type PartitionPolicyRequest = S['PartitionPolicyRequest']
export type PartitionAction = S['PartitionAction']
export type PartitionRun = S['PartitionRun']
export type PartitionPlan = S['PartitionPlan']
export type ColumnarCompression = S['ColumnarCompression']
export type TableStorageRequest = S['TableStorageRequest']
export type PartitionStorageRequest = S['PartitionStorageRequest']
export type ApproximateAggregates = S['ApproximateAggregates']
export type ApproximateAggregatesRequest = S['ApproximateAggregatesRequest']

// --- approximate aggregates ------------------------------------------------------------------------------

/** The error rate of the contract is a fraction (0.0005 to 0.1); the dialog speaks in percent (0.05 to 10). */
export const ERROR_RATE_MIN_PERCENT = 0.05
export const ERROR_RATE_MAX_PERCENT = 10
export const ERROR_RATE_DEFAULT_PERCENT = 1

/** 0.01 -> 1, without the noise of floating point (0.07 * 100 is 7.000000000000001). */
export const rateToPercent = (rate: number): number => Math.round(rate * 100 * 1e6) / 1e6
export const percentToRate = (percent: number): number => Math.round(percent * 1e-2 * 1e8) / 1e8

export const SERVER_STATE: Record<
  S['ApproximateAggregates']['servers'][number]['state'],
  StatusMeta
> = {
  ok: { tone: 'ok', icon: '✓', label: 'Installed' },
  notInstalled: { tone: 'warn', icon: '▲', label: 'Not installed' },
  versionMismatch: { tone: 'danger', icon: '✖', label: 'Different version' },
  unreachable: { tone: 'warn', icon: '▲', label: 'Not reachable' },
}

export const COUNT_DISTINCT_LABEL: Record<'exact' | 'approximate', string> = {
  exact: 'Exact',
  approximate: 'Approximate',
}

// --- storage ---------------------------------------------------------------------------------------------

export type StorageFilter = 'all' | 'heap' | 'columnar'

export const STORAGE_ROW: StatusMeta = {
  tone: 'neutral',
  icon: '○',
  label: 'Row',
  hint: 'Stored as rows (the heap format).',
}
export const STORAGE_COLUMNAR: StatusMeta = {
  tone: 'info',
  icon: '●',
  label: 'Columnar',
  hint: 'Stored by column, compressed. Fast to scan, no indexes, no UPDATE or DELETE.',
}

/** A table's format: columnar, row, a mix of both (a partitioned table), or unknown (null: not available). */
export type StorageKind = 'columnar' | 'row' | 'mixed' | 'other' | 'unknown'

export function storageKind(t: Pick<Table, 'storage'>): StorageKind {
  const s = t.storage
  if (!s) return 'unknown'
  const pf = s.partitionFormats
  if (pf) {
    const total = pf.heap + pf.columnar + pf.other
    if (total === 0) return 'row'
    if (pf.columnar === 0 && pf.other === 0) return 'row'
    if (pf.columnar === total) return 'columnar'
    return 'mixed'
  }
  if (s.accessMethod === 'columnar') return 'columnar'
  if (s.accessMethod === 'heap') return 'row'
  return s.accessMethod ? 'other' : 'unknown'
}

/** What the Storage column says about a table, as text: `Mixed (3 columnar of 31)` for a partitioned one. */
export function storageText(t: Pick<Table, 'storage'>): string {
  const kind = storageKind(t)
  const pf = t.storage?.partitionFormats
  if (kind === 'mixed' && pf)
    return `Mixed (${pf.columnar} columnar of ${pf.heap + pf.columnar + pf.other})`
  if (kind === 'columnar') return 'Columnar'
  if (kind === 'row') return 'Row'
  if (kind === 'other') return t.storage?.accessMethod ?? 'Other'
  return 'Not available'
}

/** The filter of the tables list: a partitioned table matches `columnar` when any of its partitions is columnar. */
export function matchesStorage(t: Pick<Table, 'storage'>, filter: StorageFilter): boolean {
  if (filter === 'all') return true
  const kind = storageKind(t)
  const pf = t.storage?.partitionFormats
  if (filter === 'columnar') return kind === 'columnar' || (pf ? pf.columnar > 0 : false)
  return kind === 'row' || (pf ? pf.heap > 0 : false)
}

/** The compression ratio as text (`6.2 x`); null is `n/a`, never 0 (the contract's null rule). */
export function ratioText(ratio: number | null | undefined): string | null {
  return ratio === null || ratio === undefined ? null : `${ratio.toFixed(1)} x`
}

export const COMPRESSION_LABEL: Record<ColumnarCompression, string> = {
  zstd: 'zstd (best ratio)',
  lz4: 'lz4 (fastest)',
  pglz: 'pglz',
  none: 'None',
}
export const COMPRESSION_LEVEL_MIN = 1
export const COMPRESSION_LEVEL_MAX = 19
export const STRIPE_ROWS_MIN = 1000
export const CHUNK_ROWS_MIN = 100

/** The partition's own format, as the badge meta (an unknown access method is shown as sent, by the component). */
export function partitionStorageMeta(p: Pick<TablePartition, 'accessMethod'>): StatusMeta | null {
  if (p.accessMethod === 'columnar') return STORAGE_COLUMNAR
  if (p.accessMethod === 'heap') return STORAGE_ROW
  return null
}

/** The reasons of a `conversion_refused` (409): the detail lists them, one per line or separated by semicolons. */
export function refusalReasons(detail: string | null | undefined): string[] {
  if (!detail) return []
  return detail
    .split(/\r?\n|;\s*/)
    .map((l) => l.replace(/^[-*]\s*/, '').trim())
    .filter((l) => l !== '')
}

// --- durations (ISO 8601) --------------------------------------------------------------------------------

export type DurationUnit = 'hour' | 'day' | 'week' | 'month' | 'year'
export const DURATION_UNITS: DurationUnit[] = ['hour', 'day', 'week', 'month', 'year']
const ISO_LETTER: Record<DurationUnit, string> = {
  hour: 'T#H',
  day: '#D',
  week: '#W',
  month: '#M',
  year: '#Y',
}

/** (7, day) -> `P7D`; (6, hour) -> `PT6H`. */
export function isoOf(count: number, unit: DurationUnit): string {
  return 'P' + ISO_LETTER[unit].replace('#', String(count))
}

/** `P7D` -> (7, day); a duration of several units (`P1DT12H`), or one that is not ISO 8601, is null. */
export function splitIso(
  text: string | null | undefined,
): { count: number; unit: DurationUnit } | null {
  if (!text) return null
  const m =
    /^P(?:(\d+)Y)?(?:(\d+)M)?(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$/.exec(
      text,
    )
  if (!m || text === 'P' || text.endsWith('T')) return null
  const parts: [DurationUnit | 'minute' | 'second', string | undefined][] = [
    ['year', m[1]],
    ['month', m[2]],
    ['week', m[3]],
    ['day', m[4]],
    ['hour', m[5]],
    ['minute', m[6]],
    ['second', m[7]],
  ]
  const present = parts.filter(([, v]) => v !== undefined)
  if (present.length !== 1) return null
  const [unit, value] = present[0] as [string, string]
  if (unit === 'minute' || unit === 'second') return null
  const count = Number(value)
  return count > 0 ? { count, unit: unit as DurationUnit } : null
}

/** `P7D` -> `7 days`, `PT1H` -> `1 hour`; anything else is returned as it was given. */
export function durationText(text: string | null | undefined): string {
  const s = splitIso(text)
  if (!s) return text ?? ''
  return `${s.count} ${s.unit}${s.count === 1 ? '' : 's'}`
}

/** An approximate length in seconds, to compare two durations (a month is 30 days, a year 365). */
export function isoSeconds(text: string | null | undefined): number | null {
  const s = splitIso(text)
  if (!s) return null
  const unit = { hour: 3600, day: 86400, week: 604800, month: 2_592_000, year: 31_536_000 }[s.unit]
  return s.count * unit
}

/** The partition intervals that a policy may use: one of each unit, plus `every N` for the divisors (the dialog's select). */
export const INTERVAL_PRESETS: { value: string; label: string }[] = [
  { value: 'PT1H', label: 'Hour' },
  { value: 'P1D', label: 'Day' },
  { value: 'P1W', label: 'Week' },
  { value: 'P1M', label: 'Month' },
  { value: 'P1Y', label: 'Year' },
  { value: 'custom', label: 'Every N hours or days...' },
]

// --- the policy form -------------------------------------------------------------------------------------

export interface PolicyForm {
  interval: string
  customCount: number | string
  customUnit: 'hour' | 'day'
  aheadCount: number | string
  aheadUnit: DurationUnit
  retentionOn: boolean
  retentionCount: number | string
  retentionUnit: DurationUnit
  retentionAction: 'drop' | 'detach'
  columnarOn: boolean
  columnarCount: number | string
  columnarUnit: DurationUnit
  compression: ColumnarCompression
  compressionLevel: number | string
  sortBy: string
  timezone: string
  runEveryCount: number | string
  runEveryUnit: DurationUnit
  enabled: boolean
}

export const DEFAULT_POLICY: PolicyForm = {
  interval: 'P1D',
  customCount: 6,
  customUnit: 'hour',
  aheadCount: 7,
  aheadUnit: 'day',
  retentionOn: false,
  retentionCount: 90,
  retentionUnit: 'day',
  retentionAction: 'drop',
  columnarOn: false,
  columnarCount: 7,
  columnarUnit: 'day',
  compression: 'zstd',
  compressionLevel: 3,
  sortBy: '',
  timezone: 'UTC',
  runEveryCount: 1,
  runEveryUnit: 'hour',
  enabled: true,
}

/** The form of a stored policy (a duration of several units is not offered: the form starts from the default for it). */
export function formOf(p: PartitionPolicyRequest | null | undefined): PolicyForm {
  const f: PolicyForm = { ...DEFAULT_POLICY }
  if (!p) return f
  const preset = INTERVAL_PRESETS.some((x) => x.value === p.partitionEvery && x.value !== 'custom')
  if (preset) f.interval = p.partitionEvery
  else {
    const s = splitIso(p.partitionEvery)
    f.interval = 'custom'
    if (s && (s.unit === 'hour' || s.unit === 'day')) {
      f.customCount = s.count
      f.customUnit = s.unit
    }
  }
  const ahead = splitIso(p.createAhead ?? 'P7D')
  if (ahead) {
    f.aheadCount = ahead.count
    f.aheadUnit = ahead.unit
  }
  const ret = splitIso(p.retention)
  f.retentionOn = Boolean(p.retention)
  if (ret) {
    f.retentionCount = ret.count
    f.retentionUnit = ret.unit
  }
  f.retentionAction = p.retentionAction ?? 'drop'
  const col = splitIso(p.columnarAfter)
  f.columnarOn = Boolean(p.columnarAfter)
  if (col) {
    f.columnarCount = col.count
    f.columnarUnit = col.unit
  }
  f.compression = p.compression ?? 'zstd'
  f.compressionLevel = p.compressionLevel ?? 3
  f.sortBy = (p.columnarSortBy ?? []).join(', ')
  f.timezone = p.timezone ?? 'UTC'
  const every = splitIso(p.runEvery ?? 'PT1H')
  if (every) {
    f.runEveryCount = every.count
    f.runEveryUnit = every.unit
  }
  f.enabled = p.enabled ?? true
  return f
}

/** The request of a form. A number that is not a whole number is sent as it is; the server answers with the JSON pointer. */
export function requestOf(f: PolicyForm): PartitionPolicyRequest {
  const n = (v: number | string): number => (typeof v === 'number' ? v : Number(v))
  const every = f.interval === 'custom' ? isoOf(n(f.customCount), f.customUnit) : f.interval
  const sort = f.sortBy
    .split(',')
    .map((x) => x.trim())
    .filter((x) => x !== '')
  return {
    partitionEvery: every,
    createAhead: isoOf(n(f.aheadCount), f.aheadUnit),
    retention: f.retentionOn ? isoOf(n(f.retentionCount), f.retentionUnit) : null,
    retentionAction: f.retentionAction,
    columnarAfter: f.columnarOn ? isoOf(n(f.columnarCount), f.columnarUnit) : null,
    columnarSortBy: f.columnarOn && sort.length > 0 ? sort : null,
    compression: f.columnarOn ? f.compression : null,
    compressionLevel: f.columnarOn && f.compression === 'zstd' ? n(f.compressionLevel) : null,
    timezone: f.timezone.trim() === '' ? 'UTC' : f.timezone.trim(),
    runEvery: isoOf(n(f.runEveryCount), f.runEveryUnit),
    enabled: f.enabled,
  }
}

/** True when saving this request deletes or hides data that the stored policy keeps (a new retention, or a shorter one). */
export function lowersRetention(
  stored: PartitionPolicyRequest | null | undefined,
  next: PartitionPolicyRequest,
): boolean {
  if (!next.retention) return false
  if (!stored || !stored.retention) return true
  const a = isoSeconds(stored.retention)
  const b = isoSeconds(next.retention)
  if (a === null || b === null) return stored.retention !== next.retention
  return (
    b < a || (stored.retentionAction !== next.retentionAction && next.retentionAction === 'drop')
  )
}

// --- problems of a request, by field --------------------------------------------------------------------

/** A JSON pointer of the contract (`/createAhead`) to the field of the form that shows it. */
export function pointerField(pointer: string): string {
  return pointer.replace(/^\//, '').split('/')[0] ?? ''
}

// --- runs and actions ------------------------------------------------------------------------------------

export const RUN_STATE: Record<S['PartitionRunState'], StatusMeta> = {
  running: { tone: 'info', icon: '◐', label: 'Running' },
  done: { tone: 'ok', icon: '✓', label: 'Done' },
  partial: { tone: 'warn', icon: '▲', label: 'Partly done' },
  failed: { tone: 'danger', icon: '✖', label: 'Failed' },
}

export const ACTION_STATE: Record<PartitionAction['state'], StatusMeta> = {
  planned: { tone: 'neutral', icon: '○', label: 'Planned' },
  running: { tone: 'info', icon: '◐', label: 'Running' },
  done: { tone: 'ok', icon: '✓', label: 'Done' },
  failed: { tone: 'danger', icon: '✖', label: 'Failed' },
  skipped: { tone: 'neutral', icon: '○', label: 'Skipped' },
}

export const ACTION_KIND_LABEL: Record<PartitionAction['kind'], string> = {
  create: 'Create',
  convert: 'Convert to columnar',
  drop: 'Drop',
  detach: 'Detach',
}

export const TRIGGER_LABEL: Record<PartitionRun['trigger'], string> = {
  schedule: 'Scheduled',
  manual: 'Run now',
}

/** The detail of an action in words, for the codes the scheduler is known to use. Others are shown as sent. */
export function actionDetailText(
  action: Pick<PartitionAction, 'detail' | 'partition'>,
  table = '',
): string | null {
  const d = action.detail
  if (!d) return null
  switch (d) {
    case 'lock_timeout':
    case 'swap_lock_timeout':
      return `Waited for the lock on ${table ? `\`${table}\`` : 'the table'} and gave up; will retry at the next run.`
    case 'retry_later':
      return 'Not done now (a bucket move or another change holds it); will retry at the next run.'
    case 'name_conflict':
      return `Cannot create the partition: another table is called \`${action.partition}\`.`
    default:
      return null
  }
}

/** Counts of the actions of a run by kind and failures (the runs list shows them in one line). */
export function actionCounts(actions: PartitionAction[]): {
  created: number
  converted: number
  dropped: number
  failed: number
} {
  const done = (k: PartitionAction['kind']) =>
    actions.filter((a) => a.kind === k && a.state === 'done').length
  return {
    created: done('create'),
    converted: done('convert'),
    dropped: done('drop') + done('detach'),
    failed: actions.filter((a) => a.state === 'failed').length,
  }
}

/** The health line of a policy, in words. */
export function healthText(h: PartitionPolicy['health'], createAhead: string): string {
  const ahead = h.partitionsAhead ? durationText(h.partitionsAhead) : null
  if (!ahead) return 'No partition ahead yet.'
  const floor = isoSeconds(createAhead)
  const have = isoSeconds(h.partitionsAhead)
  if (h.problems.includes('aheadLow') && floor !== null && have !== null)
    return `${ahead} ahead, below the ${durationText(createAhead)} the policy keeps.`
  return `${ahead} ahead.`
}

export const HEALTH_PROBLEM_LABEL: Record<PartitionPolicy['health']['problems'][number], string> = {
  aheadLow: 'Fewer partitions ahead than the policy keeps',
  lastRunFailed: 'The last run failed',
  defaultPartitionPresent: 'The table has a default partition',
  misaligned: 'Existing partitions are not aligned to the interval',
}

/** The tasks that hold a table for a storage change or a policy run (the page disables the buttons while one runs). */
export const ANALYTICS_TASK_KINDS = ['table.setStorage', 'partition.maintain'] as const
