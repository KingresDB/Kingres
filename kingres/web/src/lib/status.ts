// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// How each status in the API looks: a tone (colour), an icon glyph and a text label. Status is never
// shown by colour alone; the badge always renders icon and label.
import type {
  AlertKind,
  AlertSeverity,
  TaskKind,
  ServerStatus,
  TaskState,
  Instance,
  LedgerVerification,
  ShardGroup,
  ReplicationMember,
  Database,
  ClusterOverview,
} from '@/api/types'

export type Tone = 'ok' | 'warn' | 'danger' | 'info' | 'neutral'

export interface StatusMeta {
  tone: Tone
  icon: string
  label: string
}

const OK = '✓' // check
const WARN = '▲' // triangle
const BAD = '✖' // heavy x
const INFO = 'ⓘ' // circled i
const DOT = '●' // filled circle
const RING = '○' // empty circle
const HALF = '◐' // half circle
const CLOCK = '⏳' // hourglass

export const SERVER_STATUS: Record<ServerStatus, StatusMeta> = {
  live: { tone: 'ok', icon: OK, label: 'LIVE' },
  suspect: { tone: 'warn', icon: WARN, label: 'SUSPECT' },
  draining: { tone: 'info', icon: HALF, label: 'DRAINING' },
  decommissioning: { tone: 'info', icon: HALF, label: 'DECOMMISSIONING' },
  dead: { tone: 'danger', icon: BAD, label: 'DEAD' },
  pendingRebalance: { tone: 'info', icon: CLOCK, label: 'PENDING REBALANCE' },
}

export const TASK_STATE: Record<TaskState, StatusMeta> = {
  pending: { tone: 'neutral', icon: RING, label: 'Pending' },
  running: { tone: 'info', icon: HALF, label: 'Running' },
  paused: { tone: 'warn', icon: WARN, label: 'Paused' },
  failed: { tone: 'danger', icon: BAD, label: 'Failed' },
  done: { tone: 'ok', icon: OK, label: 'Done' },
}

export const SEVERITY: Record<AlertSeverity, StatusMeta> = {
  info: { tone: 'info', icon: INFO, label: 'Info' },
  warning: { tone: 'warn', icon: WARN, label: 'Warning' },
  critical: { tone: 'danger', icon: BAD, label: 'Critical' },
}

export const HEALTH: Record<ClusterOverview['health'], StatusMeta> = {
  healthy: { tone: 'ok', icon: OK, label: 'Healthy' },
  degraded: { tone: 'warn', icon: WARN, label: 'Degraded' },
  critical: { tone: 'danger', icon: BAD, label: 'Critical' },
}

export const GROUP_HEALTH: Record<ShardGroup['health'], StatusMeta> = {
  healthy: { tone: 'ok', icon: OK, label: 'Healthy' },
  degraded: { tone: 'warn', icon: WARN, label: 'Degraded' },
  unavailable: { tone: 'danger', icon: BAD, label: 'Unavailable' },
}

export const INSTANCE_STATE: Record<Instance['state'], StatusMeta> = {
  running: { tone: 'ok', icon: DOT, label: 'Running' },
  starting: { tone: 'info', icon: HALF, label: 'Starting' },
  stopping: { tone: 'info', icon: HALF, label: 'Stopping' },
  restarting: { tone: 'info', icon: HALF, label: 'Restarting' },
  stopped: { tone: 'neutral', icon: RING, label: 'Stopped' },
  failed: { tone: 'danger', icon: BAD, label: 'Failed' },
}

export const SERVICE_STATE: Record<'running' | 'stopped' | 'degraded' | 'disabled', StatusMeta> = {
  running: { tone: 'ok', icon: DOT, label: 'Running' },
  stopped: { tone: 'danger', icon: BAD, label: 'Stopped' },
  degraded: { tone: 'warn', icon: WARN, label: 'Degraded' },
  disabled: { tone: 'neutral', icon: RING, label: 'Disabled' },
}

export const SYNC_STATE: Record<NonNullable<ReplicationMember['syncState']>, StatusMeta> = {
  sync: { tone: 'ok', icon: OK, label: 'Sync' },
  quorum: { tone: 'ok', icon: OK, label: 'Quorum' },
  potential: { tone: 'info', icon: HALF, label: 'Potential' },
  async: { tone: 'neutral', icon: RING, label: 'Async' },
}

export const DATABASE_STATUS: Record<Database['status'], StatusMeta> = {
  ready: { tone: 'ok', icon: OK, label: 'Ready' },
  creating: { tone: 'info', icon: HALF, label: 'Creating' },
  dropping: { tone: 'info', icon: HALF, label: 'Dropping' },
  failed: { tone: 'danger', icon: BAD, label: 'Failed' },
}

export const LEDGER_STATE: Record<LedgerVerification['state'], StatusMeta> = {
  never: { tone: 'neutral', icon: RING, label: 'Never verified' },
  running: { tone: 'info', icon: HALF, label: 'Verifying' },
  passed: { tone: 'ok', icon: OK, label: 'Verified' },
  failed: { tone: 'danger', icon: BAD, label: 'Inconsistent' },
}

export const DURABILITY_LABEL = {
  async: 'Asynchronous',
  sync: 'Synchronous (quorum)',
  syncApply: 'Synchronous with apply',
} as const

export const LEDGER_MODE_LABEL = {
  off: 'Off',
  optIn: 'Opt-in per table',
  appendOnly: 'On by default, append-only',
  updatable: 'On by default, updatable',
} as const

export const REBALANCE_REASON_LABEL = {
  serverAdded: 'A server was added',
  serverRemoved: 'A server was removed',
  replicaCountChanged: 'The replica count changed',
  replicasMissing: 'Some replicas are missing',
  skewed: 'Buckets are unevenly spread',
  serverGroupChanged: 'A server group changed',
} as const

/** What each kind of alert means, in plain words. One entry per `AlertKind` of the contract. */
export const ALERT_KIND_LABEL: Record<AlertKind, string> = {
  serverDown: 'Server down',
  serverSuspect: 'Server not answering',
  underReplicatedBuckets: 'Buckets with too few copies',
  unavailableBuckets: 'Buckets unavailable',
  replicationLagHigh: 'Replication lag high',
  diskSpaceLow: 'Disk space low',
  memoryPressure: 'Memory pressure',
  rebalanceNeeded: 'Rebalance needed',
  autoFailoverTriggered: 'Automatic failover happened',
  autoFailoverLimitReached: 'Automatic failover limit reached',
  mapGroupUnavailable: 'Map group unavailable',
  ledgerVerificationFailed: 'Ledger verification failed',
  ledgerPendingTooOld: 'Ledger entries waiting too long for a block',
  versionMismatch: 'Servers run different versions',
  certificateExpiring: 'Server certificate expiring',
  certificateRenewalFailed: 'Certificate renewal failed',
  taskFailed: 'Task failed',
}

/** The label of an alert kind; a kind from a newer server is shown as sent (through SafeName). */
export function alertKindLabel(kind: string): string {
  return (ALERT_KIND_LABEL as Record<string, string>)[kind] ?? kind
}

/** What each task kind does, in plain words. One entry per `TaskKind` of the contract. */
export const TASK_KIND_LABEL: Record<TaskKind, string> = {
  'cluster.init': 'Create the cluster',
  'cluster.join': 'Join a cluster',
  'server.add': 'Add a server',
  'server.remove': 'Remove a server',
  'server.failover': 'Fail over a server',
  'server.leave': 'Leave the cluster',
  'instance.start': 'Start an instance',
  'instance.stop': 'Stop an instance',
  'instance.restart': 'Restart an instance',
  'database.create': 'Create a database',
  'database.update': 'Change a database',
  'database.drop': 'Drop a database',
  'table.distribute': 'Distribute a table',
  rebalance: 'Rebalance',
  'replication.restorePreferredPrimaries': 'Restore preferred primaries',
  'ledger.verify': 'Verify the ledger',
  'networkAccess.apply': 'Apply network rules',
  'replicate.apply': 'Bring a server up to date (roles, grants)',
  'certificate.renew': 'Renew server certificates',
  'certificate.revoke': 'Revoke server certificates',
}

/** The label of a task kind; a kind from a newer server is shown as sent (through SafeName). */
export function taskKindLabel(kind: string): string {
  return (TASK_KIND_LABEL as Record<string, string>)[kind] ?? kind
}
