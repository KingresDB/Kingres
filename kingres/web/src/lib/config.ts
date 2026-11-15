// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Words and small rules for the Configuration page (contract 0.8.0): the four states of a server, the levels, where a
// value comes from, how a change takes effect, the state of a clock and the sentences that must be said the same way
// everywhere (the dialog, the restart dialog, the server page).
import type { ConfigEffect, ConfigLevel, ConfigServerState, ConfigValueSource } from '@/api/types'
import { tolerant, type StatusMeta } from './status'

export const CONFIG_SERVER_STATE: Record<ConfigServerState, StatusMeta> = tolerant({
  applied: { tone: 'ok', icon: '✓', label: 'Applied' },
  pendingRestart: { tone: 'warn', icon: '▲', label: 'Pending restart' },
  failed: { tone: 'danger', icon: '✖', label: 'Failed' },
  waiting: { tone: 'info', icon: '◐', label: 'Waiting for the server' },
  unknown: { tone: 'neutral', icon: '○', label: 'Not known' },
})

export const LEVEL_LABEL: Record<ConfigLevel, string> = {
  cluster: 'Cluster',
  serverGroup: 'Server group',
  server: 'Server',
  database: 'Database',
}

/** The words after "from" for the level an effective value comes from. */
export function levelWords(level: string | null): string {
  return level === null ? 'no override' : ((LEVEL_LABEL as Record<string, string>)[level] ?? level)
}

export const SOURCE_LABEL: Record<ConfigValueSource, string> = {
  default: 'built-in default',
  initdb: 'set by initdb',
  derived: 'derived from the memory quota',
  cluster: 'cluster override',
  serverGroup: 'server group override',
  server: 'server override',
  managed: 'managed by KingresDB',
  other: 'postgresql.conf or the command line',
}

/** Words for a source; a source of a newer server is shown as sent. */
export function sourceWords(source: string): string {
  return (SOURCE_LABEL as Record<string, string>)[source] ?? source
}

export const TAKES_EFFECT_LABEL: Record<ConfigEffect['takesEffect'], string> = {
  unchanged: 'No change',
  reload: 'Takes effect after a reload',
  restart: 'Waits for a restart',
  waits: 'Server is down: will wait',
}

export function takesEffectWords(how: string): string {
  return (TAKES_EFFECT_LABEL as Record<string, string>)[how] ?? how
}

/** The top-level categories of PostgreSQL's settings, for the category filter. */
export const TOP_CATEGORIES = [
  'Autovacuum',
  'Client Connection Defaults',
  'Connections and Authentication',
  'Customized Options',
  'Developer Options',
  'Error Handling',
  'File Locations',
  'Lock Management',
  'Preset Options',
  'Process Title',
  'Query Tuning',
  'Replication',
  'Reporting and Logging',
  'Resource Usage',
  'Statistics',
  'Version and Platform Compatibility',
  'Write-Ahead Log',
] as const

/** Said wherever a restart is offered or needed: restarting a primary is downtime and nothing takes over. */
export const DOWNTIME_NOTE =
  'A primary that restarts is downtime for its shard group: writes to its buckets fail until it is back, typically a few seconds, longer after a crash or with a long recovery. Failover is not available yet, so nothing takes over.'

/** "3 of 3 servers will change. 1 needs a restart; nothing is restarted by this change." */
export function changeSentence(changing: number, total: number, restarting: number): string {
  const need =
    restarting === 1
      ? '1 needs a restart'
      : restarting === 0
        ? '0 need a restart'
        : `${restarting} need a restart`
  return `${changing} of ${total} servers will change. ${need}; nothing is restarted by this change.`
}

// --- clocks ---------------------------------------------------------------------------------------------

export type ClockState = 'inSync' | 'differs' | 'farOff' | 'cannotRead'

export const CLOCK_STATE: Record<ClockState, StatusMeta> = tolerant({
  inSync: { tone: 'ok', icon: '✓', label: 'In sync' },
  differs: { tone: 'warn', icon: '▲', label: 'Differs' },
  farOff: { tone: 'danger', icon: '✖', label: 'Far off' },
  cannotRead: { tone: 'neutral', icon: '○', label: 'Cannot be read' },
})

/**
 * The state of a clock from the cluster's threshold: the difference less the uncertainty of the measurement is
 * compared with the threshold (as the alert does), so a slow network never makes a clock look wrong by itself.
 * "Far off" is ten times the threshold, where the alert becomes critical.
 */
export function clockState(
  reachable: boolean,
  differenceMs: number | null,
  uncertaintyMs: number | null,
  maxDifferenceMs: number,
): ClockState {
  if (!reachable || differenceMs === null) return 'cannotRead'
  const excess = Math.abs(differenceMs) - (uncertaintyMs ?? 0)
  if (excess > maxDifferenceMs * 10) return 'farOff'
  return excess > maxDifferenceMs ? 'differs' : 'inSync'
}

/** A difference smaller than its uncertainty cannot be told from zero. */
export function withinMeasurement(
  differenceMs: number | null,
  uncertaintyMs: number | null,
): boolean {
  return differenceMs !== null && uncertaintyMs !== null && Math.abs(differenceMs) <= uncertaintyMs
}

/** 1820 -> "1.82 s", 0.3 -> "0.3 ms", with a sign when `signed`. */
export function formatMs(ms: number, signed = false): string {
  const sign = ms < 0 ? '-' : signed && ms > 0 ? '+' : ''
  const abs = Math.abs(ms)
  if (abs >= 60_000) return `${sign}${(abs / 60_000).toFixed(1)} min`
  if (abs >= 1000) return `${sign}${(abs / 1000).toFixed(2)} s`
  if (abs === 0) return '0 ms'
  const digits = abs >= 100 ? abs.toFixed(0) : abs.toFixed(1).replace(/\.0$/, '')
  return `${sign}${digits} ms`
}

/** "+1.82 s ± 0.4 ms". */
export function formatDifference(differenceMs: number, uncertaintyMs: number | null): string {
  const base = formatMs(differenceMs, true)
  return uncertaintyMs === null ? base : `${base} ± ${formatMs(uncertaintyMs)}`
}

/** The text of the panel that says what to do about a clock that differs. */
export const CLOCK_ADVICE = {
  summary:
    'KingresDB does not set the system clock. Setting it needs the rights of the operating system administrator, which kingresd does not have and must not have, and a clock that is set once drifts again. Run time synchronization (NTP or chrony) on every server.',
  chrony:
    'With chrony: chronyc tracking and chronyc sources. With systemd-timesyncd: timedatectl timesync-status.',
  checks:
    'Check that the servers use the same time sources and are allowed to reach them, and that a virtual machine does not also take its clock from the host.',
  step: 'A clock that is far off is stepped by the synchronization service (chrony makestep, or a restart of the service), not by hand with date, which leaves the service fighting the change.',
  after:
    'After the fix the difference shrinks within a few monitor intervals and the alert clears by itself.',
} as const

// --- the filters of the Parameters tab, in the address ---------------------------------------------------

export interface ParameterFilters {
  q: string
  category: string
  changed: boolean
  differing: boolean
  pendingRestart: boolean
  state: string
  kind: string
  /** Show managed and refused parameters too (without a `kind` filter). */
  all: boolean
}

export const NO_FILTERS: ParameterFilters = {
  q: '',
  category: '',
  changed: false,
  differing: false,
  pendingRestart: false,
  state: '',
  kind: '',
  all: false,
}

const flag = (v: unknown): boolean => v === '1' || v === 'true'
const text = (v: unknown): string =>
  typeof v === 'string' ? v : Array.isArray(v) ? String(v[0] ?? '') : ''

export function filtersFromQuery(query: Record<string, unknown>): ParameterFilters {
  return {
    q: text(query.q),
    category: text(query.category),
    changed: flag(text(query.changed)),
    differing: flag(text(query.differing)),
    pendingRestart: flag(text(query.pendingRestart)),
    state: text(query.state),
    kind: text(query.kind),
    all: flag(text(query.all)),
  }
}

/** Only what is set, so that an address with no filter is the plain page. */
export function queryFromFilters(f: ParameterFilters): Record<string, string> {
  const out: Record<string, string> = {}
  if (f.q) out.q = f.q
  if (f.category) out.category = f.category
  if (f.changed) out.changed = '1'
  if (f.differing) out.differing = '1'
  if (f.pendingRestart) out.pendingRestart = '1'
  if (f.state) out.state = f.state
  if (f.kind) out.kind = f.kind
  if (f.all) out.all = '1'
  return out
}

/** What the server is asked for: the default page lists what can be set, the toggle and `kind` list the rest. */
export function serverFilters(f: ParameterFilters): {
  q: string
  category: string
  changed: boolean
  differing: boolean
  pendingRestart: boolean
  state: string
  kind: string
} {
  return {
    q: f.q,
    category: f.category,
    changed: f.changed,
    differing: f.differing,
    pendingRestart: f.pendingRestart,
    state: f.state,
    kind: f.kind || (f.all ? '' : 'settable'),
  }
}

export function hasFilters(f: ParameterFilters): boolean {
  return Object.keys(queryFromFilters(f)).length > 0
}
