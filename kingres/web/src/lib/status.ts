// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// How each status in the API looks: a tone (colour), an icon glyph and a text label. Status is never
// shown by colour alone; the badge always renders icon and label.
import type {
  AlertKind,
  AlertSeverity,
  AuditAction,
  GroupLease,
  TaskKind,
  ServerStatus,
  TaskState,
  Instance,
  LedgerVerification,
  ShardGroup,
  ReplicationMember,
  Database,
  ClusterOverview,
  DataExport,
  DataUpload,
} from '@/api/types'
import { safeText } from './safe-text'

/**
 * A status map that does not break on a value the panel does not know (a newer server can add one:
 * 0.5.0 added members to enums). An unknown value gets a neutral badge that says so and shows the
 * value, with hidden characters made visible.
 */
export function tolerant<T extends Record<string, StatusMeta>>(map: T): T {
  return new Proxy(map, {
    get(target, key, receiver) {
      if (typeof key !== 'string') return Reflect.get(target, key, receiver)
      const known = (target as Record<string, StatusMeta | undefined>)[key]
      if (known) return known
      // Names of Object.prototype members (toString, ...) are not statuses either.
      return {
        tone: 'neutral',
        icon: '?',
        label: `Unknown (${safeText(key, 40)})`,
      } satisfies StatusMeta
    },
  })
}

export type Tone = 'ok' | 'warn' | 'danger' | 'info' | 'neutral'

export interface StatusMeta {
  tone: Tone
  icon: string
  label: string
  /** Longer explanation, shown as the tooltip of the badge (and in the text near it where there is room). */
  hint?: string
}

const OK = '✓' // check
const WARN = '▲' // triangle
const BAD = '✖' // heavy x
const INFO = 'ⓘ' // circled i
const DOT = '●' // filled circle
const RING = '○' // empty circle
const HALF = '◐' // half circle
const CLOCK = '⏳' // hourglass

export const SERVER_STATUS: Record<ServerStatus, StatusMeta> = tolerant({
  live: { tone: 'ok', icon: OK, label: 'LIVE' },
  suspect: {
    tone: 'warn',
    icon: WARN,
    label: 'SUSPECT',
    hint: 'The server that answered has heard no heartbeat from this server for the suspect timeout (5 seconds by default). It is live again as soon as it is heard.',
  },
  draining: { tone: 'info', icon: HALF, label: 'DRAINING' },
  decommissioning: { tone: 'info', icon: HALF, label: 'DECOMMISSIONING' },
  dead: {
    tone: 'danger',
    icon: BAD,
    label: 'DEAD',
    hint: 'No heartbeat for the failure timeout (10 seconds by default), as the server that answered sees it. It is live again as soon as it is heard.',
  },
  pendingRebalance: {
    tone: 'info',
    icon: CLOCK,
    label: 'PENDING REBALANCE',
    hint: 'The server should hold shards, is the primary of no bucket yet, and buckets exist: a rebalance has to give it data.',
  },
})

export const TASK_STATE: Record<TaskState, StatusMeta> = tolerant({
  pending: { tone: 'neutral', icon: RING, label: 'Pending' },
  running: { tone: 'info', icon: HALF, label: 'Running' },
  paused: { tone: 'warn', icon: WARN, label: 'Paused' },
  failed: { tone: 'danger', icon: BAD, label: 'Failed' },
  done: { tone: 'ok', icon: OK, label: 'Done' },
})

/** The state of a staged export file (contract 0.19.0). */
export const EXPORT_STATE: Record<DataExport['state'], StatusMeta> = tolerant({
  building: { tone: 'info', icon: HALF, label: 'Being written' },
  ready: { tone: 'ok', icon: OK, label: 'Ready' },
  failed: {
    tone: 'danger',
    icon: BAD,
    label: 'Failed',
    hint: 'The task failed and the partial file was removed.',
  },
  expired: {
    tone: 'neutral',
    icon: RING,
    label: 'Expired',
    hint: 'The retention time has passed; the file is deleted.',
  },
  lost: {
    tone: 'warn',
    icon: WARN,
    label: 'Lost',
    hint: 'The file or its key is gone from the server.',
  },
  deleted: { tone: 'neutral', icon: RING, label: 'Deleted' },
})

/** The state of an upload (contract 0.19.0). */
export const UPLOAD_STATE: Record<DataUpload['state'], StatusMeta> = tolerant({
  receiving: { tone: 'info', icon: HALF, label: 'Receiving' },
  complete: { tone: 'ok', icon: OK, label: 'Complete' },
  lost: {
    tone: 'warn',
    icon: WARN,
    label: 'Lost',
    hint: 'The staged file is gone; start a new upload.',
  },
  deleted: { tone: 'neutral', icon: RING, label: 'Deleted' },
})

export const SEVERITY: Record<AlertSeverity, StatusMeta> = tolerant({
  info: { tone: 'info', icon: INFO, label: 'Info' },
  warning: { tone: 'warn', icon: WARN, label: 'Warning' },
  critical: { tone: 'danger', icon: BAD, label: 'Critical' },
})

export const HEALTH: Record<ClusterOverview['health'], StatusMeta> = tolerant({
  healthy: { tone: 'ok', icon: OK, label: 'Healthy' },
  degraded: { tone: 'warn', icon: WARN, label: 'Degraded' },
  critical: { tone: 'danger', icon: BAD, label: 'Critical' },
})

export const GROUP_HEALTH: Record<ShardGroup['health'], StatusMeta> = tolerant({
  healthy: { tone: 'ok', icon: OK, label: 'Healthy' },
  degraded: { tone: 'warn', icon: WARN, label: 'Degraded' },
  unavailable: { tone: 'danger', icon: BAD, label: 'Unavailable' },
})

export const INSTANCE_STATE: Record<Instance['state'], StatusMeta> = tolerant({
  running: { tone: 'ok', icon: DOT, label: 'Running' },
  starting: { tone: 'info', icon: HALF, label: 'Starting' },
  stopping: { tone: 'info', icon: HALF, label: 'Stopping' },
  restarting: { tone: 'info', icon: HALF, label: 'Restarting' },
  stopped: { tone: 'neutral', icon: RING, label: 'Stopped' },
  failed: { tone: 'danger', icon: BAD, label: 'Failed' },
})

export const SERVICE_STATE: Record<'running' | 'stopped' | 'degraded' | 'disabled', StatusMeta> =
  tolerant({
    running: { tone: 'ok', icon: DOT, label: 'Running' },
    stopped: { tone: 'danger', icon: BAD, label: 'Stopped' },
    degraded: { tone: 'warn', icon: WARN, label: 'Degraded' },
    disabled: { tone: 'neutral', icon: RING, label: 'Disabled' },
  })

export const SYNC_STATE: Record<NonNullable<ReplicationMember['syncState']>, StatusMeta> = tolerant(
  {
    sync: { tone: 'ok', icon: OK, label: 'Sync' },
    quorum: { tone: 'ok', icon: OK, label: 'Quorum' },
    potential: { tone: 'info', icon: HALF, label: 'Potential' },
    async: { tone: 'neutral', icon: RING, label: 'Async' },
  },
)

export const DATABASE_STATUS: Record<Database['status'], StatusMeta> = tolerant({
  ready: { tone: 'ok', icon: OK, label: 'Ready' },
  creating: { tone: 'info', icon: HALF, label: 'Creating' },
  dropping: { tone: 'info', icon: HALF, label: 'Dropping' },
  failed: { tone: 'danger', icon: BAD, label: 'Failed' },
})

export const LEDGER_STATE: Record<LedgerVerification['state'], StatusMeta> = tolerant({
  never: { tone: 'neutral', icon: RING, label: 'Never verified' },
  running: { tone: 'info', icon: HALF, label: 'Verifying' },
  // A summary (database list, status) does not say which shard groups the last check covered, so it never claims
  // a verified database: the verification itself says (see lib/ledger-coverage.ts).
  passed: { tone: 'info', icon: OK, label: 'Last check passed' },
  failed: { tone: 'danger', icon: BAD, label: 'Inconsistent' },
})

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
  replicationPending: 'A change has not reached a server yet',
  taskFailed: 'Task failed',
  rebalanceFailed: 'Automatic rebalance failed',
  leaseLost: 'Primary lost its lease',
  groupWithoutSynchronousStandby: 'Commits no longer wait for a standby',
  groupUnderReplicated: 'Shard group has too few standbys',
  clockDifference: 'Server clock differs from the leader',
  failoverNeedsDecision: 'A failover waits for a decision',
  failoverBlocked: 'A failover is blocked',
  failoverDataLoss: 'A failover may have lost data',
  splitPromotion: 'A shard group was promoted on more than one server',
  oldPrimaryRunning: 'The old primary is running again',
  oldPrimaryNotStopped: 'The old primary could not be stopped',
  oldPrimaryNotRebuilt: 'The old primary was not rebuilt as a standby',
  ledgerNotAnchored: 'Ledger not anchored',
  ledgerAnchorBehind: 'Ledger anchoring is behind',
  ledgerAnchorFailed: 'Ledger anchoring failed',
  ledgerAnchorConflict: 'An anchor store holds a different object',
  ledgerChainDiverged: 'A ledger chain no longer matches its published log',
  ledgerGroupUnread: 'A ledger chain could not be read',
  ledgerPublicationBehind: 'Ledger publication is behind',
  ledgerSignerUnavailable: 'Ledger checkpoints cannot be signed',
  ledgerDigestOverdue: 'No ledger digest for too long',
  certificateAuthorityUnavailable: 'The certificate authority is unavailable',
  partitionsAheadLow: 'Partitions ahead are running low',
  partitionPolicyFailed: 'A partitioning policy failed',
  transferStagingLow: 'The staging area of the data transfer is nearly full',
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
  'table.undistribute': 'Undistribute a table',
  'table.alter': 'Change the distribution of a table',
  'table.partition': 'Change the partitions of a table',
  'bucket.split': 'Split a bucket',
  'schema.distribute': 'Distribute a schema',
  'schema.move': 'Move a schema',
  'schema.undistribute': 'Make a schema local',

  'database.approximateAggregates': 'Change approximate aggregates',
  'table.setStorage': 'Change the storage of a table',
  'partition.maintain': 'Maintain the partitions of a table',
  'table.export': 'Export a table as CSV',
  'table.import': 'Import a CSV file into a table',
  rebalance: 'Rebalance',
  'replication.restorePreferredPrimaries': 'Restore preferred primaries',
  'ledger.verify': 'Verify the ledger',
  'networkAccess.apply': 'Apply network rules',
  'replicate.apply': 'Bring a server up to date (roles, grants)',
  'certificate.renew': 'Renew server certificates',
  'certificate.revoke': 'Revoke server certificates',
  'standby.reconcile': 'Add or remove standbys',
  'config.apply': 'Apply a configuration change',
  'config.restart': 'Restart instances for a configuration change',
  'ledger.key.create': 'Create the first ledger signing key',
  'ledger.key.rotate': 'Rotate the ledger signing key',
  'ledger.key.retire': 'Retire a ledger signing key',
  'ledger.key.revoke': 'Revoke a ledger signing key',
  'ledger.key.distribute': 'Bring the holders of the ledger signing key up to date',
  'ledger.bundle.create': 'Create a ledger bundle',
  'ledger.epoch.start': 'Start a new ledger epoch',
  'ledger.upgrade': 'Upgrade the ledger extension',
}

/**
 * What a task is about, in words, for the subjects the panel can explain (contract 0.11.0: `ledgerKey` with the id
 * `signing`: the signing keys of the ledger, one key task at a time in the cluster). Any other subject is shown as
 * sent: its type and id.
 */
export function taskSubjectText(subject: { type: string; id: string }): string {
  if (subject.type === 'ledgerKey' && subject.id === 'signing') return 'Signing keys of the ledger'
  return `${subject.type} ${subject.id}`
}

/** The label of a task kind; a kind from a newer server is shown as sent (through SafeName). */
export function taskKindLabel(kind: string): string {
  return (TASK_KIND_LABEL as Record<string, string>)[kind] ?? kind
}

/**
 * A sentence for the kinds that need one beyond their label. `replicationPending` (0.6.0): until the change
 * reaches the server, an account that was disabled, changed or dropped may still work there.
 */
const ALERT_KIND_HINT: Partial<Record<AlertKind, string>> = {
  leaseLost:
    'The server that runs the primary of this shard group could not renew the lease of the group in time. If the group has a standby, the primary was stopped on purpose, so that two primaries can never run for one group; it starts again when the lease is obtained again. If the group has no standby, the primary keeps running and this alert is the only effect.',
  groupWithoutSynchronousStandby:
    'The standby of this shard group has been unreachable for longer than the grace period, so the primary no longer waits for it: a commit is acknowledged by one server only and is lost if that server is lost. The wait comes back as soon as a standby streams again.',
  groupUnderReplicated:
    'Fewer standbys stream for this shard group than the cluster replica count asks for. Standbys are added by the standby check; a group never has two instances on one server, so a small cluster allows fewer.',
  serverSuspect:
    'The manager leader has heard no heartbeat from this server for the suspect timeout (5 seconds by default). The alert clears when the server is heard again.',
  serverDown:
    'No heartbeat from this server for the failure timeout (10 seconds by default), or its instance does not run. A server that is heard again is live again.',
  clockDifference:
    'The clock of this server differs from the clock of the manager leader by more than the cluster threshold, less the uncertainty of the measurement. KingresDB does not set the system clock: run time synchronization (NTP or chrony) on every server.',
  replicationPending:
    'The leader is still waiting to hand a change of roles, grants or settings to a server it could not reach. Until it arrives, a role that was disabled, changed or dropped may still work on that server.',
  ledgerNotAnchored:
    'This database has ledger tables and no anchor target is enabled, so the ledger can be rewritten undetected by anyone who controls this cluster and its backups. Nothing is blocked. Add a target on the Anchoring tab of the Ledger page.',
  ledgerAnchorBehind:
    'The newest checkpoint of the ledger has not reached this anchor target for longer than the anchoring interval allows. Records are retried by the server.',
  ledgerAnchorFailed:
    'An anchor record at this target failed or expired. Look at the last error of the record, fix the target, then retry the record.',
  ledgerAnchorConflict:
    'The store holds a different object under the name of an anchor. Do not overwrite it: someone else wrote it, or the cluster was restored from an older state.',
  ledgerChainDiverged:
    'The chain of this shard group no longer extends the log that was published for it (a restore, a lost tail of blocks, or a changed block). The server stopped publishing this log and never overwrites what it published. The way out is the explicit procedure for a restore, not a repair.',
  ledgerGroupUnread:
    'The chain of this shard group could not be read in the last rounds, so the aggregate repeats its last good state for it.',
  ledgerPublicationBehind:
    'The newest signed checkpoint of this chain is older than five publication intervals although its head has advanced.',
  ledgerSignerUnavailable:
    'No checkpoint could be signed: the key set is damaged, a key file is missing on the leader, or the signer does not answer. Published history is not harmed, but nothing new is signed until this is fixed. See the Signing tab of the Ledger page.',
  ledgerDigestOverdue: 'No digest was made within twice the digest interval of this database.',
}

/** The extra sentence for an alert kind, or null. */
export function alertKindHint(kind: string): string | null {
  return (ALERT_KIND_HINT as Record<string, string | undefined>)[kind] ?? null
}

/** How the state of a shard group lease looks (contract 0.7.0). */
export const LEASE_STATE: Record<GroupLease['state'], StatusMeta> = tolerant({
  held: { tone: 'ok', icon: OK, label: 'Lease held' },
  granted: { tone: 'info', icon: INFO, label: 'Lease granted' },
  lost: { tone: 'danger', icon: BAD, label: 'Lease lost' },
  none: { tone: 'neutral', icon: RING, label: 'No lease' },
})

/** What each audit action means, in plain words. One entry per `AuditAction` of the contract. */
export const AUDIT_ACTION_LABEL: Record<AuditAction, string> = {
  login: 'Sign in',
  logout: 'Sign out',
  loginFailed: 'Failed sign-in',
  setupTokenRejected: 'Setup token rejected',
  initCluster: 'Create the cluster',
  joinCluster: 'Join a cluster',
  updateThisServerSettings: 'Change the settings of a server',
  restartThisServerInstance: 'Restart an instance',
  stopThisServerInstance: 'Stop an instance',
  startThisServerInstance: 'Start an instance',
  leaveCluster: 'Leave the cluster',
  addServer: 'Add a server',
  updateServer: 'Change a server',
  removeServer: 'Remove a server',
  failoverServer: 'Fail over a server',
  abandonMapGroupBootstrap: 'Give up the bootstrap of the map group',
  autoFailover: 'Automatic failover',
  createServerGroup: 'Create a server group',
  deleteServerGroup: 'Delete a server group',
  createDatabase: 'Create a database',
  updateDatabase: 'Change a database',
  dropDatabase: 'Drop a database',
  distributeTable: 'Distribute a table',
  previewTableChange: 'Preview a change of a table',
  undistributeTable: 'Undistribute a table',
  alterTableDistribution: 'Change the distribution of a table',
  createTablePartition: 'Create a partition',
  attachTablePartition: 'Attach a partition',
  detachTablePartition: 'Detach a partition',
  dropTablePartition: 'Drop a partition',
  drainServer: 'Drain a server',
  splitBucket: 'Split a bucket',
  previewBucketSplit: 'Preview the split of a bucket',
  distributeSchema: 'Distribute a schema',
  undistributeSchema: 'Make a schema local',
  moveSchema: 'Move a schema',

  configureApproximateAggregates: 'Change approximate aggregates',
  setTableStorage: 'Change the storage of a table',
  setPartitionStorage: 'Change the storage of a partition',
  putPartitionPolicy: 'Set a partitioning policy',
  deletePartitionPolicy: 'Remove a partitioning policy',
  runPartitionPolicy: 'Run a partitioning policy',
  enableTableLedger: 'Enable the ledger of a table',
  restorePreferredPrimaries: 'Restore preferred primaries',
  startRebalance: 'Start a rebalance',
  stopRebalance: 'Stop a rebalance',
  cancelTask: 'Cancel a task',
  createRole: 'Create a role',
  updateRole: 'Change a role',
  deleteRole: 'Delete a role',
  setRoleMemberships: 'Set role memberships',
  setRoleClusterRoles: 'Set cluster roles of a role',
  createGrant: 'Grant privileges',
  revokeGrants: 'Revoke privileges',
  applyGrantTemplate: 'Apply a grant template',
  replaceNetworkAccess: 'Replace the network rules',
  updateClusterSettings: 'Change the cluster settings',
  generateLedgerDigest: 'Generate a ledger digest',
  startLedgerVerification: 'Start a ledger verification',
  executeQuery: 'Run a workbench statement',
  acknowledgeAlert: 'Acknowledge an alert',
  renewServerCertificates: 'Renew server certificates',
  revokeServerCertificates: 'Revoke server certificates',
  certificateRenewed: 'Certificates renewed by the server',
  certificateRenewalFailed: 'Certificate renewal failed',
  cancelStatement: 'Cancel a workbench statement',
  leaseLost: 'Shard group lease lost',
  leaseFenced: 'Primary stopped because its lease was lost',
  leaseAcquired: 'Shard group lease obtained',
  standbyCreated: 'Standby created',
  standbyRemoved: 'Standby removed',
  standbyRebuilt: 'Standby rebuilt',
  standbyInstanceStarted: 'Standby instance started',
  standbyInstanceRemoved: 'Standby instance removed',
  standbyInstanceRebuilt: 'Standby instance rebuilt',
  synchronousStandbyNamesChanged: 'Synchronous standby list changed',
  replicationSlotDropped: 'Replication slot dropped',
  applyConfigChange: 'Change PostgreSQL parameters',
  revertConfigChange: 'Revert a configuration change',
  restartPendingInstances: 'Restart instances that wait for a restart',
  resetAutoFailoverCounter: 'Reset the counter of automatic failovers',
  failoverGroupStarted: 'Failover of a shard group started',
  failoverGroupPromoted: 'A standby was promoted to primary',
  failoverGroupFailed: 'Failover of a shard group failed',
  oldPrimaryRebuilt: 'Old primary rebuilt as a standby',
  updateLedgerSettings: 'Change the ledger publication and anchoring settings',
  createLedgerSigningKey: 'Create or rotate a ledger signing key',
  retireLedgerSigningKey: 'Retire a ledger signing key',
  revokeLedgerSigningKey: 'Revoke a ledger signing key',
  createLedgerAnchorTarget: 'Add a ledger anchor target',
  updateLedgerAnchorTarget: 'Change a ledger anchor target',
  deleteLedgerAnchorTarget: 'Delete a ledger anchor target',
  testLedgerAnchorTarget: 'Test a ledger anchor target',
  anchorLedgerNow: 'Anchor the ledger now',
  retryLedgerAnchor: 'Retry a ledger anchor',
  createLedgerBundle: 'Create a ledger bundle',
  registerLedgerClientKey: 'Register a ledger client key',
  revokeLedgerClientKey: 'Revoke a ledger client key',
  setTableSignaturePolicy: 'Set the signature policy of a ledger table',
  upgradeLedgerExtension: 'Upgrade the ledger extension',
  acknowledgeLedgerDivergence: 'Acknowledge a diverged ledger chain',
  exportTable: 'Export a table as CSV',
  importTable: 'Import a CSV file into a table',
  createDataUpload: 'Start an upload',
  deleteDataUpload: 'Delete an upload',
  downloadDataExport: 'Download an export file',
  deleteDataExport: 'Delete an export file',
  'dataTransfer.completed': 'A data transfer ended',
  'dataTransfer.purged': 'Staged transfer files were removed',
}

/** The words for an audit action; an action from a newer server is shown as sent. */
export function auditActionLabel(action: string): string {
  return (AUDIT_ACTION_LABEL as Record<string, string>)[action] ?? action
}
