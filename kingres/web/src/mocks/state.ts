// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// In-memory mock cluster behind the mock API. It is small but consistent: three servers with
// chained placement (a primary on every server, a standby of the neighbouring group), a map group,
// three shard groups, two databases (`shop` is sharded with ledger tables). Mutations return tasks
// that progress over time and change this state when they finish, so the console visibly reacts.
//
// All objects are typed with the generated contract types, so a drift from the spec fails to compile.
import type { components } from '@/api/schema'
import type { OperationId } from '@/api/operations.generated'
import type {
  Alert,
  Bucket,
  ClusterOverview,
  ClusterSettings,
  Database,
  Grant,
  Instance,
  MissingServer,
  LedgerBlock,
  LedgerDigest,
  LedgerVerification,
  LogEntry,
  NetworkAccess,
  RebalancePlan,
  RebalanceStatus,
  ReplicationTopology,
  Role,
  Server,
  ServerStatus,
  Table,
  Task,
  TaskKind,
  AuditEvent,
  ColocationGroup,
  ShardGroup,
  ClusterRoleName,
} from '@/api/types'
import { createConfigState, type ConfigState } from './config-data'
import { createLedger7State, type Ledger7State } from './ledger-data'

type S = components['schemas']

export const MOCK_SETUP_TOKEN = 'demo-setup-token'
export const MOCK_PASSWORD = 'demo-password'
const MiB = 1024 * 1024
const GiB = 1024 * MiB

/** Knobs for the mock; tests and the dev tools change them. */
export const mockConfig = {
  /** How long a task takes from 0 to 100 percent. */
  taskDurationMs: 4000,
  /** Operations that answer 422 `unsupported_operation`. */
  unsupported: new Set<OperationId>(),
  /** Artificial network delay for every request. */
  latencyMs: 0,
  /** How long a server that has left the cluster answers 503 before it is uninitialized. */
  leaveResetMs: 2000,
  /**
   * Servers that did not answer a cluster-wide read (contract 0.5.0): `listLogs`, `listAlerts` and
   * `queryMetrics` then answer `partial: true` with these in `missingServers`. Null: complete answers.
   */
  partial: null as MissingServer[] | null,
  /** True: every request that carries a cursor answers 400, as a 0.5.0 server does for a 0.4.0 cursor. */
  rejectCursors: false,
  /** True: the manager leader does not hold the CA key, so renew and revoke answer 503. */
  caUnavailable: false,
  /** True: the server runs without its internal API, so add, remove, leave, renew and revoke answer 409 `internal_api_disabled`. */
  internalApiDisabled: false,
  /**
   * False: the caller of `getNodeState` is not on the machine and has no setup token, so `internalFingerprint` is
   * null (the contract answers it only to a loopback caller or with a valid setup token).
   */
  setupFingerprintVisible: true,
  /** Text a hostile server could send as the setup task `detail` and as the fingerprint of this server. Null: the normal text. */
  hostileSetupText: null as string | null,
  /** True: the setup task has no `detail` (an older server, or one that does not report the pin check). */
  omitSetupDetail: false,
  /**
   * True: the audit log cannot be written, so every audited mutation answers 503 `service_unavailable` before
   * anything is done (contract 0.6.0).
   */
  auditUnwritable: false,
  /**
   * True: the ledger of the last shard group of a database is not covered by new digests and verifications
   * (contract 0.6.0 `complete: false`, `uncoveredShardGroupIds`, no aggregate hash).
   */
  incompleteLedger: false,
  /**
   * 0.7.0: what `getLedgerStatus` says about its coverage. 'local' (default, as kingresd 0.7.0): only the chain of the
   * shard group of the answering server is covered; 'all': every group is covered; 'absent': an older server that
   * sends neither `complete` nor `uncoveredShardGroupIds`.
   */
  ledgerStatusCoverage: 'local' as 'local' | 'all' | 'absent',
  /**
   * The address the mock server sees the client at (`Session.clientAddress`); null: the server does not report
   * it (an older revision). Documentation range, never a real host.
   */
  clientAddress: '203.0.113.7' as string | null,
  /**
   * 0.7.0: every `executeQuery` answers 429 `workbench_busy` with `Retry-After`, as if the role's two statements
   * (or the server's four) were taken by other sessions.
   */
  workbenchBusy: false,
  /**
   * 0.9.0: `?rebalance=refuse:<code>` makes the next start of a rebalance answer 409 with that problem code (every code
   * of the refusals); `?rebalance=fail` makes the next run end failed (one bucket gives up after three attempts);
   * `?rebalance=slow` makes moves take about three times as long.
   */
  rebalanceRefuse: null as string | null,
  rebalanceFail: false,
  rebalanceSlow: false,
  /**
   * 0.10.0: `?failover=dataloss` makes the plan of every hard failover say that writes may be lost (verdict `possible`):
   * the request is refused with 409 `failover_needs_decision` until it says `allowDataLoss`.
   */
  failoverDataLoss: false,
  /**
   * 0.10.0, the console of the failover (every one has a URL switch, see switches.ts): `needs-decision` pauses the task at
   * "Choose the standby" (the verdict turned "possible" while it ran, as after a restart of the leader); `blocked` makes a
   * request refuse for that reason; `failStep` ends the task failed at that step; `slow` makes steps take three times as
   * long; `map` and `graceful` switch on what the real server refuses for now (the map group, the switchover); `old` sets
   * the state of the old primary in the report; `limit` puts the counter at its maximum; `readiness` overrides what a group's
   * card says about failover (`window.__kingresMock.setReadiness`).
   */
  failoverNeedsDecision: false,
  failoverBlock: null as null | 'rebalance' | 'lease' | 'quorum' | 'noreplica',
  failoverFailStep: null as null | { step: string; group: number | null },
  failoverSlow: false,
  failoverMap: false,
  failoverGraceful: false,
  failoverOld: null as null | 'rebuilding' | 'waiting' | 'running' | 'failed',
  failoverSwitchoverBehind: false,
  /** 0.12.0: a finished failover ends `completedWithWarnings` (the check of the cluster reported something). */
  failoverWarnings: false,
  /** 0.12.0: a refusal `failover_needs_decision` carries no plan (a server of 0.11.0): the console asks with a dry run. */
  failoverNoPlan: false,
  failoverReadiness: {} as Record<number, FailoverReadinessState>,
  /** 0.11.0 (console part B): what the test of an anchor target finds: nothing wrong, a warning, or a failure. */
  anchorTest: null as null | 'warn' | 'fail',
  /** 0.11.0 (console part B): every change to a target, a test, an anchor-now and a retry answers this problem code. */
  anchorRefuse: null as null | string,
  /** 0.11.0 (console part C): the ledger extension is older than 0.3: every client-key operation answers 422 `unsupported_operation`. */
  clientLegacy: false,
  /** The server has no OpenSSL: a key cannot be registered (422 `unsupported_operation`). */
  clientNoSsl: false,
  /** Every change of a client key or of a signature policy, and the upgrade, answers this problem code. */
  clientRefuse: null as null | string,
  /** Console D: a bundle is gone when it is downloaded (410), or every bundle/acknowledge/verify request answers a problem code. */
  bundleExpired: false,
  opsRefuse: null as null | string,
  /** 0.8.0: the source commit the server reports in GET /setup/state: a hex id, null (the build does not know it) or text that is not a hex id. */
  commit: '0123456789abcdef0123456789abcdef01234567' as string | null,
  /**
   * 0.7.0: this server is a follower that has not heard the manager leader for longer than the leader-loss bound:
   * every request that needs a session check answers 503 `leader_unavailable` (with `Retry-After: 5`).
   */
  leaderLost: false,
  /** 0.7.0: the answering server knows nothing about leases (`ShardGroup.lease` is absent). */
  leaseAbsent: false,
  /**
   * 0.7.0: the client entry point of the servers is on. 'on': the client metrics of the entry point exist; 'standby':
   * the metrics of the standby entry point exist too; false (the default of kingresd): no client metric exists.
   */
  clientEntryPoint: false as false | 'on' | 'standby',
  /** Heartbeat bounds of the mock (the defaults of kingresd): silent for this long is suspect, then dead. */
  suspectMs: 5000,
  failureMs: 10000,
  /** How long no standby may stream before the group stops waiting for it (`standby-sync-grace`, 30 s). */
  syncGraceMs: 30000,
  /** The time a lease granted just now is valid (`lease-duration`, 10 s). */
  leaseDurationMs: 10000,
  /** How long a statement of the mock that runs for ever is allowed to run when no timeout was sent. */
  defaultStatementTimeoutMs: 30000,
}

// --- types of the internal records ------------------------------------------------------------

interface ServerRec {
  id: string
  address: string
  hostname: string
  status: ServerStatus
  serverGroup: string | null
  shouldHaveShards: boolean
  /** The capacity of the server relative to the others (contract 0.16.0); absent means 1. */
  weight?: number
  version: string
  joinedAt: string
  startedAt: number
  /** False when the server has not reported resource usage and uptime (they are null then). */
  telemetry: boolean
  certificateExpiresAt?: string | null
  seenFingerprint?: string | null
  fingerprintPinned?: boolean
  certificateRevokedAt?: string | null
  /**
   * 0.7.0: when the server stopped sending heartbeats (the process was stopped), or null. It is suspect after
   * `mockConfig.suspectMs` and dead after `mockConfig.failureMs`, whatever the catalog says.
   */
  silentSince?: number | null
}

export interface GroupRec {
  id: number
  isMapGroup: boolean
  preferredPrimaryServerId: string
  primaryServerId: string | null
  standbyServerIds: string[]
  failedServerIds: string[]
  /** 0.7.0: the id of the standby on a server (`kingres_cp.standby`); missing for a standby of the catalog. */
  standbyIds?: Record<string, number>
  /** 0.7.0: servers whose standby of this group is managed by kingresd (not in the sharding catalog yet). */
  managed?: string[]
  /** 0.7.0: servers whose standby is being created by the task `standby.reconcile` (not streaming yet). */
  creating?: string[]
  /** 0.7.0: servers whose standby does not stream, with the time it stopped (ms). */
  stalled?: Record<string, number>
  /** 0.7.0: the primary of the group runs here but could not renew its lease (it was stopped when enforced). */
  leaseLost?: boolean
}

interface BucketRec {
  bucket: number
  group: number
  state: 'active' | 'moving' | 'orphan'
  movingTo: number | null
  /** Null when the size of the bucket is not known (shown as "not available"). */
  size: number | null
}

export interface DbRec {
  info: Database
  tables: Table[]
  colocations: ColocationGroup[]
  buckets: Map<number, BucketRec[]>
  /** Columns to report instead of the built-in ones, by table name (hostile-data tests). */
  extraColumns?: Record<string, S['TableColumn'][]>
}

/** What a group's card says about failover (the mock's override, `setReadiness`). */
export type FailoverReadinessState =
  | 'possible'
  | 'possibleLoss'
  | 'unknownLoss'
  | 'noStandby'
  | 'noneReady'
  | 'noMajority'
  | 'versionsDiffer'

/** A failover run the mock drives: what the status and the report are drawn from (the task carries the steps). */
export interface FailoverRun {
  taskId: number
  trigger: 'manual' | 'automatic'
  serverId: string
  mode: 'hard' | 'graceful'
  req: S['FailoverRequest']
  plan: S['FailoverPlan']
  /** The names of the steps of one group, and of the whole task (check, every group's block, the report). */
  groupSteps: string[]
  stepNames: string[]
  /** The index of the step the task fails at, and what it says. */
  failAt: { index: number; code: string; message: string } | null
  /** The task is paused at "Choose the standby" until a repeat of the request accepts the loss. */
  needsDecision: boolean
  accepted: boolean
  mapBootstrap: boolean
  startedAtIso: string
  /** The verdict of the groups turned "possible" while the task ran (the leader restarted). */
  lossForced: boolean
}

export interface TaskRec {
  task: Task
  startedAt: number
  durationMs: number
  /** Applied once when the task reaches 100 percent. */
  apply: () => void
  /** Called with the completed fraction (0..1) while the task runs. */
  onProgress?: (fraction: number) => void
  /** When set, the task fails with this message instead of applying. */
  failWith?: string
  /**
   * The task stops at this fraction (0..1) and stays running with `holdDetail` until `holdAt` is cleared: a step that
   * waits for a server that cannot be reached (`config.apply`).
   */
  holdAt?: number
  holdDetail?: string
  /** The task does all its work and then ends failed, naming the step that failed (`config.apply`: a server failed). */
  failAtEnd?: { code: string; message: string; step: string; detail: string }
  /** 0.10.0: the task pauses (state `paused`) when it reaches this fraction, until the owner clears it and resumes it. */
  pauseAt?: number
  /** 0.10.0: the task fails with this code and message when it reaches this fraction (a step of a failover). */
  failAt?: { fraction: number; code: string; message: string }
  /** Called once when the task fails (`failAt` or `failWith`) or pauses. */
  onFail?: () => void
  onPause?: () => void
  /** 0.9.0: the step that skips itself on purpose; the task records it as `skipped` (the reason is the task detail). */
  skippedStep?: string
  applied: boolean
  /** True for tasks created before the node was initialized (readable with the setup token). */
  setup: boolean
}

export interface SessionRec {
  username: string
  clusterRoles: ClusterRoleName[]
  csrfToken: string
  expiresAt: string
}

export interface MockState {
  node: {
    state: 'uninitialized' | 'standalone' | 'clustered'
    leftCluster: boolean
  }
  clusterName: string
  clusterId: string
  mapVersion: number
  servers: ServerRec[]
  groups: GroupRec[]
  serverGroups: { name: string; description: string }[]
  databases: DbRec[]
  tasks: TaskRec[]
  nextTaskId: number
  roles: Role[]
  passwords: Map<string, string>
  grants: Grant[]
  network: NetworkAccess
  audit: AuditEvent[]
  nextAuditId: number
  alerts: Alert[]
  settings: ClusterSettings
  /** The server a drain is emptying, null otherwise (the status says trigger drain meanwhile). */
  drain: string | null
  rebalance: {
    needed: boolean
    reasons: S['RebalanceReason'][]
    state: RebalanceStatus['state']
    taskId: number | null
    startedAt: string | null
    options: S['RebalanceOptions'] | null
    moves: S['PlannedMove'][]
    /** Set while a stop is under way: the time it was asked (the move in flight ends about 1.5 s later). */
    stopAskedAt: number | null
    /** Why the run is stopped: operator or safetyRule. */
    stoppedBy: string | null
    pausedReason: string | null
    /** Fraction of the work that was done when the run was stopped (a start again goes on from it). */
    fractionAtStop: number
    reports: S['RebalanceReport'][]
  }
  /** 0.10.0: the reports of failover runs, newest first (a report has the identifier of its task). */
  failover: { reports: S['FailoverReport'][]; runs: FailoverRun[] }
  ledger: {
    digests: LedgerDigest[]
    verifications: LedgerVerification[]
    heights: Record<number, number>
  }
  logs: LogEntry[]
  localSettings: S['LocalSettings']
  instanceStates: Map<number, Instance['state']>
  /** True while the instance that hosts the system database is stopped on purpose. */
  systemDatabaseStopped: boolean
  /** Set when this server has left the cluster: it answers 503 until `until`, then is uninitialized. */
  leave: { until: number } | null
  /** False simulates a manager leader that cannot be reached (leaving needs `force` then). */
  leaderReachable: boolean
  /** 0.7.0: statements the workbench runs now, by statement id (one role's id is unique on a server). */
  statements: Map<string, { username: string; startedAt: number; cancelled: boolean }>
  /** 0.7.0: the id of the next standby the task `standby.reconcile` creates. */
  nextStandbyId: number
  session: SessionRec | null
  /** 0.8.0: the configuration of the servers (parameters, overrides, history, what each server runs, the clocks). */
  config: ConfigState
  /** 0.11.0: the ledger of phase 7 (the settings, the signing keys, the anchor targets, the divergence of chains). */
  ledger7: Ledger7State
  /** Result sets the workbench can select from, by table name (hostile-data tests). */
  extraQueryTables?: Record<string, { columns: S['QueryColumn'][]; rows: unknown[][] }>
}

// --- helpers ----------------------------------------------------------------------------------

export const nowIso = (): string => new Date().toISOString()

/** The automatic rebalance as a cluster that never changed it has it (contract 0.16.0): off. */
export const DEFAULT_AUTOMATIC: NonNullable<S['RebalanceSettings']['automatic']> = {
  enabled: false,
  strategy: 'byBucketCount',
  threshold: 0.15,
  checkIntervalSeconds: 60,
  settleSeconds: 300,
  settleAfterTopologySeconds: 600,
  minIntervalSeconds: 3600,
  failoverSettleSeconds: 600,
  window: null,
}
const ago = (seconds: number): string => new Date(Date.now() - seconds * 1000).toISOString()
const ahead = (seconds: number): string => new Date(Date.now() + seconds * 1000).toISOString()

/** Deterministic 64-hex-character hash-like string (not cryptographic; this is mock data). */
export function fakeHash(seed: string): string {
  let h1 = 0x811c9dc5
  let out = ''
  for (let round = 0; round < 8; round++) {
    for (let i = 0; i < seed.length; i++) {
      h1 ^= seed.charCodeAt(i) + round
      h1 = Math.imul(h1, 0x01000193) >>> 0
    }
    out += h1.toString(16).padStart(8, '0')
  }
  return out
}

/** A small number derived from a host id ("10.0.0.12" gives 12), for instance ids and ports. */
export function hostNumber(host: string): number {
  const m = /(\d+)\D*$/.exec(host)
  if (m) return Number(m[1]) % 90
  let h = 0
  for (const c of host) h = (h * 31 + c.charCodeAt(0)) % 90
  return h
}

function instanceId(groupId: number, serverId: string): number {
  return groupId * 100 + hostNumber(serverId)
}

function portFor(groupId: number, serverId: string): number {
  return 5440 + groupId * 10 + (hostNumber(serverId) % 10)
}

function seededRandom(seed: number): () => number {
  let s = seed >>> 0
  return () => {
    s = (Math.imul(s, 1664525) + 1013904223) >>> 0
    return s / 0xffffffff
  }
}

// --- seed ---------------------------------------------------------------------------------------

export function makeBuckets(
  offset: number,
  count: number,
  groupIds: number[],
  seed: number,
): BucketRec[] {
  const rnd = seededRandom(seed)
  return Array.from({ length: count }, (_, i) => ({
    bucket: i,
    group: groupIds[(i + offset) % groupIds.length] as number,
    state: 'active' as const,
    movingTo: null,
    // Every 17th bucket has no size yet: the contract allows null for it.
    size: i % 17 === 16 ? null : Math.round((3 + rnd() * 6) * MiB),
  }))
}

function seedDatabases(): DbRec[] {
  const base = (name: string, overrides: Partial<Database>): Database => ({
    name,
    status: 'ready',
    general: {
      owner: 'app_rw',
      encoding: 'UTF8',
      lcCollate: 'en_US.UTF-8',
      lcCtype: 'en_US.UTF-8',
      connectionLimit: -1,
    },
    sharding: { enabled: false, bucketCount: 128, mode: 'row', distributeNewSchemas: false },
    replication: { durability: 'sync', replicaCount: 1 },
    ledger: { mode: 'off', digestIntervalSeconds: 3600 },
    stats: { sizeBytes: 0, tableCount: 0, activeConnections: 0, transactionsPerSecond: 0 },
    health: { underReplicatedBuckets: 0, unavailableBuckets: 0 },
    ledgerSummary: { verificationState: 'never', lastVerifiedBlockHeight: null },
    rebalanceRequired: false,
    createdBy: 'admin',
    createdAt: ago(86400 * 40),
    ...overrides,
  })
  const table = (t: Partial<Table> & Pick<Table, 'name' | 'classes'>): Table => ({
    schema: 'public',
    distribution: null,
    ledger: null,
    homeShardGroupId: 0,
    estimatedRows: 0,
    sizeBytes: 0,
    ...t,
  })
  const shop: DbRec = {
    info: base('shop', {
      sharding: { enabled: true, bucketCount: 128, mode: 'both', distributeNewSchemas: false },
      ledger: { mode: 'appendOnly', digestIntervalSeconds: 3600 },
      ledgerSummary: { verificationState: 'passed', lastVerifiedBlockHeight: 1790 },
    }),
    tables: [
      table({
        name: 'orders',
        classes: ['distributed', 'ledger'],
        distribution: { method: 'hash', column: 'customer_id', colocationId: 1, bucketCount: 128 },
        ledger: { mode: 'appendOnly', historyTable: null },
        homeShardGroupId: null,
        estimatedRows: 4_812_330,
        sizeBytes: 720 * MiB,
      }),
      table({
        name: 'customers',
        classes: ['distributed'],
        distribution: { method: 'hash', column: 'customer_id', colocationId: 1, bucketCount: 128 },
        homeShardGroupId: null,
        estimatedRows: 310_204,
        sizeBytes: 96 * MiB,
      }),
      table({
        name: 'events',
        classes: ['distributed'],
        distribution: { method: 'hash', column: 'session_id', colocationId: 2, bucketCount: 128 },
        homeShardGroupId: null,
        estimatedRows: 22_904_118,
        sizeBytes: 1_450 * MiB,
      }),
      table({
        name: 'countries',
        classes: ['reference'],
        distribution: { method: 'reference', column: null, colocationId: 3, bucketCount: 1 },
        homeShardGroupId: null,
        estimatedRows: 249,
        sizeBytes: 64 * 1024,
      }),
      table({ name: 'notes', classes: ['plain'], estimatedRows: 1_203, sizeBytes: 2 * MiB }),
    ],
    colocations: [
      {
        colocationId: 1,
        bucketCount: 128,
        distributionColumnType: 'bigint',
        tableCount: 2,
        sizeBytes: 816 * MiB,
      },
      {
        colocationId: 2,
        bucketCount: 128,
        distributionColumnType: 'uuid',
        tableCount: 1,
        sizeBytes: null,
      },
    ],
    buckets: new Map([
      [1, makeBuckets(0, 128, [1, 2, 3], 11)],
      [2, makeBuckets(1, 128, [1, 2, 3], 29)],
    ]),
  }
  shop.info.stats = {
    sizeBytes: 2_340 * MiB,
    tableCount: 5,
    activeConnections: 37,
    transactionsPerSecond: 842.5,
  }
  const reports: DbRec = {
    info: base('reports', {
      // Created outside the control plane: no record of who created it, or when.
      createdBy: null,
      createdAt: null,
      general: {
        owner: 'analyst',
        encoding: 'UTF8',
        lcCollate: 'C',
        lcCtype: 'C',
        connectionLimit: 20,
      },
      replication: { durability: 'async', replicaCount: 1 },
      stats: {
        sizeBytes: 180 * MiB,
        tableCount: 2,
        activeConnections: 4,
        // Statistics the server cannot provide yet are null, not 0.
        transactionsPerSecond: null,
      },
    }),
    tables: [
      table({
        name: 'monthly_totals',
        classes: ['plain'],
        estimatedRows: 9_400,
        sizeBytes: 12 * MiB,
      }),
      table({ name: 'exports', classes: ['plain'], estimatedRows: null, sizeBytes: 168 * MiB }),
    ],
    colocations: [],
    buckets: new Map(),
  }
  return [shop, reports]
}

function seedLogs(): LogEntry[] {
  const components: LogEntry['component'][] = [
    'api',
    'agent',
    'manager',
    'router',
    'etcd',
    'patroni',
    'postgres',
    'metrics',
  ]
  const messages: [LogEntry['level'], string][] = [
    ['info', 'heartbeat from server 2 received'],
    ['info', 'bucket map version advanced'],
    ['debug', 'connection pool resized'],
    ['warning', 'replication lag above 1s on instance 223'],
    ['info', 'checkpoint complete'],
    ['error', 'could not connect to etcd member, retrying'],
    ['info', 'leader lease renewed'],
    ['info', 'session established for admin'],
  ]
  return Array.from({ length: 240 }, (_, i) => {
    const [level, message] = messages[i % messages.length] as [LogEntry['level'], string]
    return {
      timestamp: ago(i * 37),
      level,
      component: components[(i * 3) % components.length] as LogEntry['component'],
      serverId: `10.0.0.${11 + (i % 3)}`,
      instanceId: i % 4 === 0 ? 111 : null,
      message,
    }
  })
}

function seedRoles(): Role[] {
  const r = (name: string, o: Partial<Role> = {}): Role => ({
    name,
    canLogin: true,
    isSuperuser: false,
    canCreateDatabase: false,
    canCreateRole: false,
    connectionLimit: -1,
    validUntil: null,
    memberOf: [],
    clusterRoles: [],
    ...o,
  })
  return [
    r('admin', {
      isSuperuser: true,
      canCreateDatabase: true,
      canCreateRole: true,
      clusterRoles: ['kingres_admin'],
    }),
    r('ops', { clusterRoles: ['kingres_ops'] }),
    r('viewer', { clusterRoles: ['kingres_readonly'] }),
    r('metrics', { clusterRoles: ['kingres_stats'] }),
    r('app_rw', { canCreateDatabase: true, connectionLimit: 200 }),
    r('analyst', {
      memberOf: ['app_ro'],
      validUntil: new Date(Date.now() + 86400000 * 60).toISOString(),
    }),
    r('app_ro', { canLogin: false }),
  ]
}

/** Recreates the whole mock cluster. `uninitialized` starts the setup wizard flow. */
export function createMockState(options: { uninitialized?: boolean } = {}): MockState {
  const servers: ServerRec[] = [11, 12, 13].map((n) => ({
    id: `10.0.0.${n}`,
    address: `10.0.0.${n}:8091`,
    hostname: `kdb-${n - 10}.example.internal`,
    status: 'live' as const,
    serverGroup: n === 13 ? 'rack-b' : 'rack-a',
    shouldHaveShards: true,
    version: '0.1.0',
    joinedAt: ago(86400 * 40),
    startedAt: Date.now() - (86400 * 12 + (n - 10) * 3600) * 1000,
    // The third server's agent has not reported resources and uptime yet: those values are null.
    telemetry: n !== 13,
    // The creator of the cluster has no first contact on record; the other two were added.
    certificateExpiresAt: n === 13 ? ahead(86400 * 9) : ahead(86400 * 300),
    seenFingerprint: n === 11 ? null : fakeHash(`bootstrap-${n}`),
    fingerprintPinned: n === 12,
  }))
  const groups: GroupRec[] = [
    {
      id: 0,
      isMapGroup: true,
      preferredPrimaryServerId: '10.0.0.11',
      primaryServerId: '10.0.0.11',
      standbyServerIds: ['10.0.0.12'],
      failedServerIds: [],
    },
    {
      id: 1,
      isMapGroup: false,
      preferredPrimaryServerId: '10.0.0.11',
      primaryServerId: '10.0.0.11',
      standbyServerIds: ['10.0.0.12'],
      failedServerIds: [],
    },
    {
      id: 2,
      isMapGroup: false,
      preferredPrimaryServerId: '10.0.0.12',
      primaryServerId: '10.0.0.12',
      standbyServerIds: ['10.0.0.13'],
      failedServerIds: [],
    },
    {
      id: 3,
      isMapGroup: false,
      preferredPrimaryServerId: '10.0.0.13',
      primaryServerId: '10.0.0.13',
      standbyServerIds: ['10.0.0.11'],
      failedServerIds: [],
    },
  ]
  const passwords = new Map<string, string>()
  for (const name of ['admin', 'ops', 'viewer', 'metrics', 'app_rw', 'analyst'])
    passwords.set(name, MOCK_PASSWORD)

  const uninitialized = options.uninitialized === true
  return {
    node: { state: uninitialized ? 'uninitialized' : 'clustered', leftCluster: false },
    clusterName: uninitialized ? '' : 'prod-eu',
    clusterId: '7d6d0f4e-3c3a-4a52-9f0b-2d6a1c8e5b11',
    mapVersion: 4172,
    servers,
    groups,
    serverGroups: [
      { name: 'rack-a', description: 'Rack A, row 1' },
      { name: 'rack-b', description: 'Rack B, row 2' },
    ],
    databases: seedDatabases(),
    tasks: [],
    nextTaskId: 100,
    roles: seedRoles(),
    passwords,
    grants: [
      {
        grantee: 'app_rw',
        level: 'database',
        database: 'shop',
        schema: null,
        table: null,
        privileges: ['connect', 'create', 'temporary'],
      },
      {
        grantee: 'app_rw',
        level: 'schema',
        database: 'shop',
        schema: 'public',
        table: null,
        privileges: ['usage', 'create'],
      },
      {
        grantee: 'app_rw',
        level: 'table',
        database: 'shop',
        schema: 'public',
        table: 'orders',
        privileges: ['select', 'insert', 'update'],
      },
      {
        grantee: 'app_ro',
        level: 'database',
        database: 'reports',
        schema: null,
        table: null,
        privileges: ['connect'],
      },
      {
        grantee: 'app_ro',
        level: 'table',
        database: 'reports',
        schema: 'public',
        table: 'monthly_totals',
        privileges: ['select'],
      },
    ],
    network: {
      revision: 7,
      tlsRequired: false,
      rules: [
        {
          type: 'hostssl',
          databases: ['all'],
          users: ['all'],
          address: '10.0.0.0/24',
          method: 'scram-sha-256',
          comment: 'cluster network',
        },
        {
          type: 'host',
          databases: ['shop'],
          users: ['app_rw'],
          address: '10.1.0.0/16',
          method: 'scram-sha-256',
          comment: 'application servers',
        },
      ],
      applyStatus: servers.map((s) => ({ serverId: s.id, appliedRevision: 7, error: null })),
    },
    audit: [
      {
        id: 3,
        timestamp: ago(600),
        actor: 'admin',
        source: 'panel',
        action: 'createRole',
        resourceType: 'role',
        resourceId: 'analyst',
        outcome: 'success',
        actorAddress: '10.0.0.50',
        serverId: '10.0.0.11',
      },
      {
        id: 2,
        timestamp: ago(4000),
        actor: 'ops',
        source: 'cli',
        action: 'failoverServer',
        resourceType: 'server',
        resourceId: '2',
        outcome: 'denied',
        actorAddress: '10.2.0.9',
        serverId: '10.0.0.12',
      },
      {
        id: 1,
        timestamp: ago(9000),
        actor: 'admin',
        source: 'panel',
        action: 'login',
        outcome: 'success',
        actorAddress: '10.2.0.5',
        serverId: '10.0.0.11',
      },
    ],
    nextAuditId: 4,
    alerts: [
      {
        id: 12,
        kind: 'replicationLagHigh',
        severity: 'warning',
        state: 'open',
        title: 'Replication lag is high',
        message: 'Instance 112 is 1.8 s behind its primary.',
        serverId: '10.0.0.12',
        databaseName: null,
        subject: {
          serverId: '10.0.0.12',
          behindServerId: null,
          shardGroupId: null,
          databaseName: null,
          aspect: null,
        },
        raisedAt: ago(1500),
      },
      {
        id: 11,
        kind: 'diskSpaceLow',
        severity: 'info',
        state: 'acknowledged',
        title: 'Disk space below 30 percent',
        message: 'Server 10.0.0.13 data volume has 28 percent free.',
        serverId: '10.0.0.13',
        databaseName: null,
        subject: {
          serverId: '10.0.0.13',
          behindServerId: null,
          shardGroupId: null,
          databaseName: null,
          aspect: null,
        },
        raisedAt: ago(86400),
        acknowledgedAt: ago(80000),
        acknowledgedBy: 'ops',
      },
      {
        id: 10,
        kind: 'serverSuspect',
        severity: 'critical',
        state: 'resolved',
        title: 'Server 10.0.0.12 was suspect',
        message: 'Heartbeats from server 10.0.0.12 were missing for 14 seconds.',
        serverId: '10.0.0.12',
        databaseName: null,
        subject: {
          serverId: '10.0.0.12',
          behindServerId: null,
          shardGroupId: null,
          databaseName: null,
          aspect: null,
        },
        raisedAt: ago(86400 * 3),
        resolvedAt: ago(86400 * 3 - 40),
      },
    ],
    settings: {
      defaultReplicaCount: 1,
      defaultDurability: 'sync',
      autoFailover: { enabled: true, timeoutSeconds: 120, maxCount: 1, eventsSinceReset: 0 },
      rebalance: {
        maxConcurrentMoves: 4,
        strategy: 'byBucketCount',
        threshold: null,
        maxMovesPerServer: 2,
        maxMoveAttempts: 3,
        maxDiskUsedFraction: 0.85,
        maxStandbyLagBytes: 67108864,
        automatic: { ...DEFAULT_AUTOMATIC },
      },
      features: { sharding: true, ledger: true },
      clock: { maxDifferenceMs: 1000 },
      restart: { required: false, serverIds: [] },
    },
    failover: { reports: [], runs: [] },
    drain: null,
    rebalance: {
      needed: false,
      reasons: [],
      state: 'idle',
      taskId: null,
      startedAt: null,
      options: null,
      moves: [],
      stopAskedAt: null,
      stoppedBy: null,
      pausedReason: null,
      fractionAtStop: 0,
      reports: [
        {
          id: 2,
          outcome: 'completed',
          startedAt: ago(86400 * 20),
          finishedAt: ago(86400 * 20 - 1260),
          totals: {
            movesTotal: 86,
            movesDone: 86,
            movesFailed: 0,
            bytesMoved: 560 * MiB,
            durationSeconds: 1260,
          },
          moves: [
            {
              databaseName: 'shop',
              colocationId: 1,
              bucket: 127,
              fromShardGroupId: 1,
              toShardGroupId: 3,
              sizeBytes: 6 * MiB,
              outcome: 'done',
              durationSeconds: 14,
            },
            {
              databaseName: 'shop',
              colocationId: 1,
              bucket: 124,
              fromShardGroupId: 2,
              toShardGroupId: 3,
              sizeBytes: 5 * MiB,
              outcome: 'done',
              durationSeconds: 11,
            },
            {
              databaseName: 'shop',
              colocationId: 2,
              bucket: 120,
              fromShardGroupId: 1,
              toShardGroupId: 2,
              outcome: 'done',
              durationSeconds: 9,
            },
            {
              databaseName: 'shop',
              colocationId: 2,
              bucket: 119,
              fromShardGroupId: 3,
              toShardGroupId: 2,
              sizeBytes: 7 * MiB,
              outcome: 'failed',
              durationSeconds: 31,
              error: 'Replication slot lagged too far behind; the move was rolled back.',
            },
            {
              databaseName: 'shop',
              colocationId: 2,
              bucket: 118,
              fromShardGroupId: 3,
              toShardGroupId: 1,
              sizeBytes: 4 * MiB,
              outcome: 'skipped',
            },
          ],
        },
        {
          id: 1,
          outcome: 'stopped',
          startedAt: ago(86400 * 30),
          finishedAt: ago(86400 * 30 - 300),
          totals: {
            movesTotal: 128,
            movesDone: 31,
            movesFailed: 0,
            bytesMoved: 180 * MiB,
            durationSeconds: 300,
          },
          moves: [],
        },
      ],
    },
    ledger: {
      digests: [
        {
          id: 3,
          databaseName: 'shop',
          generatedAt: ago(3500),
          generatedBy: null,
          hashVersion: 1,
          chains: [1, 2, 3].map((g) => ({
            shardGroupId: g,
            blockHeight: 1800 + g * 10,
            blockHash: fakeHash(`b${g}-${1800 + g * 10}`),
            document: digestDocument(g, 1800 + g * 10),
          })),
          aggregateHash: fakeHash('agg-3'),
          aggregateSize: 41,
          anchors: [],
          complete: true,
          uncoveredShardGroupIds: [],
        },
      ],
      // Newest first, as the contract says. Ids grow with time; id 8 was recorded without a task.
      verifications: [
        ...[16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6].map((id): S['LedgerVerification'] => {
          const failed = id === 12
          return {
            id,
            databaseName: 'shop',
            state: failed ? 'failed' : 'passed',
            scope: id % 3 === 0 ? 'full' : 'sinceLastVerified',
            digestId: null,
            taskId: id === 8 ? null : 80 + id,
            startedAt: ago(600 * (17 - id) + 300),
            finishedAt: ago(600 * (17 - id)),
            startedBy: id % 2 ? 'ops' : 'system',
            blocksChecked: 40 + id,
            rowsChecked: 1000 * id,
            complete: true,
            uncoveredShardGroupIds: [],
            inconsistencies: failed
              ? [
                  {
                    shardGroupId: 2,
                    blockHeight: 1805,
                    kind: 'blockHashMismatch',
                    table: null,
                    detail: 'The stored hash of block 1805 differs from the recomputed hash.',
                  },
                  {
                    shardGroupId: 3,
                    blockHeight: null,
                    kind: 'other',
                    table: 'public.orders',
                    detail:
                      'ledger_check_x: the check reported a mismatch the contract has no kind for.',
                  },
                ]
              : [],
          }
        }),
        {
          id: 5,
          databaseName: 'shop',
          state: 'passed',
          scope: 'full',
          digestId: null,
          taskId: 90,
          startedAt: ago(86400),
          finishedAt: ago(86400 - 300),
          startedBy: 'ops',
          blocksChecked: 5_460,
          rowsChecked: 4_812_330,
          complete: true,
          uncoveredShardGroupIds: [],
          inconsistencies: [],
        },
      ],
      heights: { 1: 1810, 2: 1820, 3: 1830 },
    },
    logs: seedLogs(),
    localSettings: {
      dataPaths: {
        dataDirectory: '/var/lib/kingresdb/data',
        walDirectory: null,
        logDirectory: '/var/log/kingresdb',
      },
      memoryQuota: { totalMegabytes: 16384 },
      logLevel: 'info',
      clientPorts: { readWritePort: 5432, readOnlyPort: 5433 },
      restartRequired: false,
    },
    instanceStates: new Map(),
    systemDatabaseStopped: false,
    leave: null,
    leaderReachable: true,
    statements: new Map(),
    nextStandbyId: 5,
    session: null,
    config: createConfigState(servers.map((x) => ({ id: x.id, serverGroup: x.serverGroup }))),
    ledger7: createLedger7State(),
  }
}

// --- heartbeats, leases and standbys (contract 0.7.0) ------------------------------------------

/** The server that answers the requests of the mock (its own heartbeat is always now). */
export const ANSWERING_SERVER = '10.0.0.11'

/**
 * The status the answering server reports: the catalog status, overlaid with what the heartbeats say. A server
 * silent for `suspectMs` is `suspect`, for `failureMs` `dead`; the status comes back when it is heard again.
 */
export function effectiveStatus(rec: ServerRec): ServerStatus {
  if (isSilent(rec)) {
    const age = Date.now() - (rec.silentSince as number)
    if (age >= mockConfig.failureMs) return 'dead'
    if (age >= mockConfig.suspectMs) return 'suspect'
  }
  return rec.status
}

/** True while the server sends no heartbeat (the answering server cannot hear it). */
export function isSilent(rec: ServerRec): boolean {
  return rec.silentSince !== null && rec.silentSince !== undefined && rec.id !== ANSWERING_SERVER
}

/** Stops the heartbeats of a server (as if its kingresd process were stopped). Returns false for an unknown id. */
export function silenceServer(s: MockState, id: string, since: number = Date.now()): boolean {
  const rec = s.servers.find((x) => x.id === id)
  if (!rec || rec.id === ANSWERING_SERVER) return false
  rec.silentSince = since
  return true
}

/** The server is heard again. */
export function reviveServer(s: MockState, id: string): void {
  const rec = s.servers.find((x) => x.id === id)
  if (rec) rec.silentSince = null
}

/** Marks the lease of a group as lost (the primary runs on the answering server and could not renew it). */
export function loseLease(s: MockState, groupId: number): boolean {
  const g = s.groups.find((x) => x.id === groupId)
  if (!g || g.primaryServerId !== ANSWERING_SERVER) return false
  g.leaseLost = true
  return true
}

/** The lease is obtained again. */
export function regainLease(s: MockState, groupId: number): void {
  const g = s.groups.find((x) => x.id === groupId)
  if (g) g.leaseLost = false
}

/** Stops the standby of a group on a server from streaming (its instance is stopped, or the link is cut). */
export function stallStandby(
  s: MockState,
  groupId: number,
  serverId: string,
  since = Date.now(),
): void {
  const g = s.groups.find((x) => x.id === groupId)
  if (!g) return
  g.stalled = { ...(g.stalled ?? {}), [serverId]: since }
}

export function resumeStandby(s: MockState, groupId: number, serverId: string): void {
  const g = s.groups.find((x) => x.id === groupId)
  if (g?.stalled) delete g.stalled[serverId]
}

/** A group has a standby (what the lease enforcement rule asks): at least one standby is known to it. */
export function hasStandby(g: GroupRec): boolean {
  return g.standbyServerIds.length > 0
}

/** Whether the standby of `g` on `serverId` streams now. */
export function standbyStreams(s: MockState, g: GroupRec, serverId: string): boolean {
  if (g.creating?.includes(serverId)) return false
  if (g.stalled && serverId in g.stalled) return false
  const rec = s.servers.find((x) => x.id === serverId)
  if (rec && (isSilent(rec) || rec.status === 'dead')) return false
  return !g.failedServerIds.includes(serverId)
}

/** When the standbys of the group last all stopped streaming (ms), or null while one streams or is being made. */
function noStreamSince(s: MockState, g: GroupRec): number | null {
  if (g.standbyServerIds.length === 0) return null
  let latest = 0
  for (const id of g.standbyServerIds) {
    if (g.creating?.includes(id)) return null
    if (standbyStreams(s, g, id)) return null
    const rec = s.servers.find((x) => x.id === id)
    const since = Math.min(
      g.stalled?.[id] ?? Infinity,
      rec && isSilent(rec) ? (rec.silentSince as number) : Infinity,
    )
    latest = Math.max(latest, Number.isFinite(since) ? since : 0)
  }
  return latest
}

/** True when the primary no longer waits for a standby: none streams and the grace period is over. */
export function syncRuleEmptied(s: MockState, g: GroupRec): boolean {
  const since = noStreamSince(s, g)
  return since !== null && Date.now() - since >= mockConfig.syncGraceMs
}

/** The instance id of a standby: 1048576 plus its own id while kingresd manages it (it is not in the catalog). */
export function standbyInstanceId(g: GroupRec, serverId: string): number {
  if (g.managed?.includes(serverId)) return 1_048_576 + (g.standbyIds?.[serverId] ?? 0)
  return instanceId(g.id, serverId)
}

interface AlertSubjectKey {
  serverId?: string | undefined
  shardGroupId?: number | undefined
  behindServerId?: string | undefined
}

const sameSubject = (a: Alert, subject: AlertSubjectKey): boolean =>
  (a.subject?.serverId ?? null) === (subject.serverId ?? null) &&
  (a.subject?.shardGroupId ?? null) === (subject.shardGroupId ?? null)

/** Raises an alert once per kind and subject; an acknowledged alert stays acknowledged. */
export function raise(
  s: MockState,
  kind: Alert['kind'],
  severity: Alert['severity'],
  subject: AlertSubjectKey,
  title: string,
  message: string,
): void {
  if (s.alerts.some((a) => a.kind === kind && a.state !== 'resolved' && sameSubject(a, subject)))
    return
  s.alerts.unshift({
    id: Math.max(0, ...s.alerts.map((a) => a.id)) + 1,
    kind,
    severity,
    state: 'open',
    title,
    message,
    serverId: subject.serverId ?? null,
    databaseName: null,
    raisedAt: nowIso(),
    subject: {
      serverId: subject.serverId ?? null,
      behindServerId: subject.behindServerId ?? null,
      shardGroupId: subject.shardGroupId ?? null,
      databaseName: null,
      aspect: null,
    },
  })
}

/** Resolves the open alerts of a kind about a subject. Returns how many were resolved. */
export function clear(s: MockState, kind: Alert['kind'], subject: AlertSubjectKey): number {
  let n = 0
  for (const a of s.alerts) {
    if (a.kind === kind && a.state !== 'resolved' && sameSubject(a, subject)) {
      a.state = 'resolved'
      a.resolvedAt = nowIso()
      n += 1
    }
  }
  return n
}

function systemAudit(s: MockState, action: AuditEvent['action'], groupId: number): void {
  s.audit.unshift({
    id: s.nextAuditId++,
    timestamp: nowIso(),
    actor: 'system',
    source: 'system',
    serverId: ANSWERING_SERVER,
    action,
    resourceType: 'shardGroup',
    resourceId: String(groupId),
    outcome: 'success',
  })
}

/**
 * 0.8.0: the alert `clockDifference` for a server whose clock differs from the leader's by more than the cluster
 * threshold, reduced by the uncertainty of the measurement; critical above ten times the threshold. It clears when the
 * server is back within the threshold.
 */
function clockAlerts(s: MockState): void {
  const limit = s.settings.clock?.maxDifferenceMs ?? 1000
  for (const rec of s.servers) {
    const clock = s.config.clocks[rec.id]
    if (!clock || rec.id === ANSWERING_SERVER) continue
    const subject = { serverId: rec.id }
    const excess = clock.reachable ? Math.abs(clock.offsetMs) - clock.uncertaintyMs : 0
    if (excess > limit) {
      const secs = (clock.offsetMs / 1000).toFixed(2)
      raise(
        s,
        'clockDifference',
        excess > limit * 10 ? 'critical' : 'warning',
        subject,
        `Clock of server ${rec.id} differs from the leader`,
        `The clock of server ${rec.id} is ${clock.offsetMs > 0 ? '+' : ''}${secs} s from the clock of the manager leader. KingresDB does not set the system clock: run time synchronization (NTP or chrony) on the server.`,
      )
    } else clear(s, 'clockDifference', subject)
  }
}

/**
 * Brings the alerts and the audit log in line with what the heartbeats, leases and standbys say now (what the
 * manager leader does every few seconds). Called before every request of the mock, so it stays cheap.
 */
export function tickHa(s: MockState): void {
  if (s.node.state !== 'clustered') return
  clockAlerts(s)
  for (const rec of s.servers) {
    if (rec.id === ANSWERING_SERVER) continue
    const st = effectiveStatus(rec)
    const subject = { serverId: rec.id }
    if (isSilent(rec)) {
      if (st === 'suspect')
        raise(
          s,
          'serverSuspect',
          'warning',
          subject,
          `Server ${rec.id} is not answering`,
          `No heartbeat from server ${rec.id} for more than ${Math.round(mockConfig.suspectMs / 1000)} seconds.`,
        )
      if (st === 'dead') {
        clear(s, 'serverSuspect', subject)
        raise(
          s,
          'serverDown',
          'critical',
          subject,
          `Server ${rec.id} is down`,
          `No heartbeat from server ${rec.id} for more than ${Math.round(mockConfig.failureMs / 1000)} seconds.`,
        )
      }
    } else if (rec.status !== 'dead') {
      // Heard again: the manager clears both alerts about heartbeats.
      clear(s, 'serverSuspect', subject)
      for (const a of s.alerts)
        if (a.kind === 'serverDown' && a.state !== 'resolved' && a.title.endsWith(' is down'))
          if (a.subject?.serverId === rec.id) {
            a.state = 'resolved'
            a.resolvedAt = nowIso()
          }
    }
  }
  const wanted = s.settings.defaultReplicaCount
  const others = s.servers.length - 1
  for (const g of s.groups) {
    const key = { shardGroupId: g.id, serverId: g.primaryServerId ?? undefined }
    if (g.leaseLost === true && g.primaryServerId === ANSWERING_SERVER) {
      const known = s.alerts.some(
        (a) => a.kind === 'leaseLost' && a.state !== 'resolved' && a.subject?.shardGroupId === g.id,
      )
      raise(
        s,
        'leaseLost',
        'critical',
        key,
        `The primary of shard group ${g.id} lost its lease`,
        hasStandby(g)
          ? `The primary on server ${g.primaryServerId} could not renew the lease of shard group ${g.id} and was stopped, so that no second primary can run.`
          : `The primary on server ${g.primaryServerId} could not renew the lease of shard group ${g.id}. The group has no standby, so the primary keeps running.`,
      )
      if (!known) {
        systemAudit(s, 'leaseLost', g.id)
        if (hasStandby(g)) systemAudit(s, 'leaseFenced', g.id)
      }
    } else if (clear(s, 'leaseLost', key) > 0) {
      systemAudit(s, 'leaseAcquired', g.id)
    }
    if (syncRuleEmptied(s, g)) {
      raise(
        s,
        'groupWithoutSynchronousStandby',
        'critical',
        key,
        `Shard group ${g.id} has no synchronous standby`,
        `No standby of shard group ${g.id} has streamed for more than ${Math.round(mockConfig.syncGraceMs / 1000)} seconds, so its primary no longer waits for one: commits are acknowledged by one server only.`,
      )
    } else {
      clear(s, 'groupWithoutSynchronousStandby', key)
    }
    // Fewer standbys stream than the replica count allows (never in a one-server cluster).
    const target = Math.min(wanted, others)
    const streaming = g.standbyServerIds.filter((id) => standbyStreams(s, g, id)).length
    if (others > 0 && wanted > 0 && !g.creating?.length && streaming < wanted) {
      raise(
        s,
        'groupUnderReplicated',
        'warning',
        key,
        `Shard group ${g.id} has fewer standbys than the replica count`,
        `Shard group ${g.id} has ${streaming} streaming standby(s); the replica count of the cluster is ${wanted}${target < wanted ? ` and the servers allow ${target}` : ''}.`,
      )
    } else {
      clear(s, 'groupUnderReplicated', key)
    }
  }
}

// --- derived views ------------------------------------------------------------------------------

export function dataGroups(s: MockState): GroupRec[] {
  return s.groups.filter((g) => !g.isMapGroup)
}

export function bucketView(s: MockState, b: BucketRec): Bucket {
  const group = s.groups.find((g) => g.id === b.group)
  return {
    bucket: b.bucket,
    shardGroupId: b.group,
    state: b.state,
    primaryServerId: group?.primaryServerId ?? null,
    replicaServerIds: group?.standbyServerIds ?? [],
    sizeBytes: b.size,
    movingToShardGroupId: b.movingTo,
  }
}

function allBuckets(s: MockState): BucketRec[] {
  return s.databases.flatMap((d) => [...d.buckets.values()].flat())
}

export function instancesOfServer(s: MockState, serverId: string): Instance[] {
  const out: Instance[] = []
  const server = s.servers.find((x) => x.id === serverId)
  const dead = server ? effectiveStatus(server) === 'dead' : false
  for (const g of s.groups) {
    const make = (role: 'primary' | 'standby', failed: boolean): Instance => ({
      id: role === 'standby' ? standbyInstanceId(g, serverId) : instanceId(g.id, serverId),
      serverId,
      shardGroupId: g.id,
      isMapGroup: g.isMapGroup,
      role,
      state:
        failed || dead ? 'failed' : (s.instanceStates.get(instanceId(g.id, serverId)) ?? 'running'),
      port: portFor(g.id, serverId),
      dataDirectory: `/var/lib/kingresdb/data/g${g.id}`,
      version: '18.1',
      sizeBytes: g.isMapGroup ? 48 * MiB : 700 * MiB + g.id * 40 * MiB,
      upstreamInstanceId:
        role === 'standby' && g.primaryServerId ? instanceId(g.id, g.primaryServerId) : null,
      replicationLagBytes: role === 'standby' ? 4096 * (hostNumber(serverId) - 10 + g.id) : null,
      replicationLagSeconds:
        role === 'standby'
          ? Number(
              (
                0.05 * (hostNumber(serverId) - 10 + g.id) +
                (g.id === 1 && serverId === '10.0.0.12' ? 1.6 : 0)
              ).toFixed(2),
            )
          : null,
    })
    if (g.primaryServerId === serverId) out.push(make('primary', false))
    else if (g.standbyServerIds.includes(serverId)) out.push(make('standby', false))
    else if (g.failedServerIds.includes(serverId)) out.push(make('standby', true))
  }
  return out
}

export function serverView(s: MockState, rec: ServerRec): Server {
  const instances = instancesOfServer(s, rec.id)
  const k = hostNumber(rec.id) - 10
  let primary = 0
  let replica = 0
  for (const b of allBuckets(s)) {
    const g = s.groups.find((x) => x.id === b.group)
    if (g?.primaryServerId === rec.id) primary += 1
    if (g?.standbyServerIds.includes(rec.id)) replica += 1
  }
  const sizeBytes = instances.reduce((n, i) => n + (i.sizeBytes ?? 0), 0)
  const lags = instances
    .map((i) => i.replicationLagSeconds)
    .filter((x): x is number => typeof x === 'number')
  return {
    id: rec.id,
    address: rec.address,
    hostname: rec.hostname,
    status: effectiveStatus(rec),
    serverGroup: rec.serverGroup,
    shouldHaveShards: rec.shouldHaveShards,
    weight: rec.weight ?? 1,
    isManagerLeader: rec.id === '10.0.0.11',
    certificateExpiresAt: rec.certificateExpiresAt ?? null,
    certificateRevokedAt: rec.certificateRevokedAt ?? null,
    seenFingerprint: rec.seenFingerprint ?? null,
    fingerprintPinned: rec.seenFingerprint ? (rec.fingerprintPinned ?? false) : false,
    // The version of a server that has not reported yet is unknown.
    version: rec.telemetry ? rec.version : null,
    // A server whose agent cannot be reached reports no uptime (contract 0.6.0, 0.7.0: heartbeats).
    uptimeSeconds:
      rec.telemetry && !isSilent(rec) ? Math.floor((Date.now() - rec.startedAt) / 1000) : null,
    joinedAt: rec.joinedAt,
    // 0.7.0: the time of the last heartbeat; for the answering server it is now.
    lastHeartbeatAt: isSilent(rec)
      ? new Date(rec.silentSince as number).toISOString()
      : rec.status === 'dead'
        ? ago(600)
        : ago(rec.id === ANSWERING_SERVER ? 0 : 1),
    instances,
    buckets: { primary, replica },
    sizeBytes,
    replicationLagMaxSeconds: lags.length ? Math.max(...lags) : null,
    resources:
      rec.telemetry && !isSilent(rec)
        ? {
            cpuUsagePercent: 18 + k * 9,
            memoryUsedBytes: (6 + k) * GiB,
            memoryTotalBytes: 16 * GiB,
            diskUsedBytes: (120 + k * 35) * GiB,
            diskTotalBytes: 500 * GiB,
          }
        : {
            cpuUsagePercent: null,
            memoryUsedBytes: null,
            memoryTotalBytes: 16 * GiB,
            diskUsedBytes: null,
            diskTotalBytes: null,
          },
  }
}

/** The lease of a group as the answering server knows it (contract 0.7.0), or undefined for an older server. */
export function leaseView(s: MockState, g: GroupRec): S['GroupLease'] | undefined {
  if (mockConfig.leaseAbsent) return undefined
  const enforced = hasStandby(g)
  if (g.primaryServerId === null)
    return { holder: g.preferredPrimaryServerId, state: 'none', enforced }
  const holder = g.primaryServerId
  if (holder === ANSWERING_SERVER) {
    if (g.leaseLost === true) return { holder, state: 'lost', enforced }
    const left = mockConfig.leaseDurationMs - (Date.now() % 2000)
    return { holder, state: 'held', remainingMillis: left, enforced }
  }
  // A group whose primary runs elsewhere: the answering server only knows what it granted.
  const rec = s.servers.find((x) => x.id === holder)
  if (rec && isSilent(rec)) {
    const left = mockConfig.leaseDurationMs - (Date.now() - (rec.silentSince as number))
    if (left <= 0) return { holder, state: 'none', enforced }
    return { holder, state: 'granted', remainingMillis: left, enforced }
  }
  const left = mockConfig.leaseDurationMs - (Date.now() % 2000)
  return { holder, state: 'granted', remainingMillis: left, enforced }
}

/** Whether a failover or a restore is running or waits for a person. */
export function failoverRunning(s: MockState): boolean {
  return s.tasks.some(
    (t) =>
      (t.task.kind === 'server.failover' ||
        t.task.kind === 'replication.restorePreferredPrimaries') &&
      (t.task.state === 'running' || t.task.state === 'paused'),
  )
}

/**
 * 0.10.0: what the leader would say about a failover of the group (`ShardGroup.failover`), from the standbys the mock
 * knows, the switches and `window.__kingresMock.setReadiness`. Never possible while a failover runs.
 */
export function readinessOf(s: MockState, g: GroupRec): S['FailoverReadiness'] {
  const checkedAt = new Date(Date.now() - 4000).toISOString()
  const blocked = (code: string, message: string): S['FailoverReadiness'] => ({
    possible: false,
    candidateServerId: null,
    dataLossVerdict: null,
    blockers: [{ code, shardGroupId: g.id, message }],
    checkedAt,
  })
  const forced = mockConfig.failoverReadiness[g.id]
  const candidate = g.standbyServerIds.find((id) => standbyStreams(s, g, id)) ?? null
  const possible = (
    verdict: 'none' | 'asyncOnly' | 'possible' | 'unknown',
  ): S['FailoverReadiness'] => ({
    possible: true,
    candidateServerId: candidate ?? g.standbyServerIds[0] ?? null,
    dataLossVerdict: verdict,
    blockers: [],
    checkedAt,
  })
  if (failoverRunning(s))
    return blocked('failover_in_progress', 'A failover is running; another one starts after it.')
  switch (forced) {
    case 'possible':
      return possible('none')
    case 'possibleLoss':
      return possible('possible')
    case 'unknownLoss':
      return possible('unknown')
    case 'noStandby':
      return blocked('no_replica_available', `Shard group ${g.id} has no standby.`)
    case 'noneReady':
      return blocked('no_replica_available', `No standby of shard group ${g.id} is ready.`)
    case 'noMajority':
      return blocked('quorum_unavailable', 'This server cannot hear a majority of the servers.')
    case 'versionsDiffer':
      return blocked('version_mismatch', 'The servers do not run the same extension version.')
    default:
      break
  }
  if (mockConfig.failoverBlock === 'quorum')
    return blocked('quorum_unavailable', 'This server cannot hear a majority of the servers.')
  if (mockConfig.failoverBlock === 'lease')
    return blocked(
      'lease_settings_differ',
      'The servers do not use the same lease duration and margin.',
    )
  if (mockConfig.failoverBlock === 'rebalance')
    return blocked(
      'rebalance_in_progress',
      'A rebalance is running and did not stop within a minute.',
    )
  if (g.standbyServerIds.length === 0 || mockConfig.failoverBlock === 'noreplica')
    return blocked('no_replica_available', `Shard group ${g.id} has no standby that is ready.`)
  if (candidate === null)
    return blocked(
      'no_replica_available',
      `No standby of shard group ${g.id} is ready: none streams.`,
    )
  return possible(mockConfig.failoverDataLoss || syncRuleEmptied(s, g) ? 'possible' : 'none')
}

export function groupView(s: MockState, g: GroupRec): ShardGroup {
  const emptied = syncRuleEmptied(s, g)
  const member = (serverId: string, role: 'primary' | 'standby'): S['ReplicationMember'] => {
    const inst = instancesOfServer(s, serverId).find((i) => i.shardGroupId === g.id)
    const streams = role === 'standby' && standbyStreams(s, g, serverId)
    const creating = role === 'standby' && (g.creating?.includes(serverId) ?? false)
    // A primary that lost an enforced lease was stopped on purpose.
    const fenced = role === 'primary' && g.leaseLost === true && hasStandby(g)
    const recSrv = s.servers.find((x) => x.id === serverId)
    const unheard = recSrv !== undefined && effectiveStatus(recSrv) === 'dead'
    return {
      instanceId: role === 'standby' ? standbyInstanceId(g, serverId) : instanceId(g.id, serverId),
      serverId,
      role,
      state: fenced
        ? 'stopped'
        : creating
          ? 'starting'
          : unheard
            ? 'failed'
            : (inst?.state ?? 'running'),
      port: portFor(g.id, serverId),
      timeline: 3,
      // `ANY 1 (...)` makes every streaming standby a quorum member; after the grace period nothing is waited for.
      syncState: role === 'primary' ? null : streams && !emptied ? 'quorum' : 'async',
      lagBytes:
        role === 'primary' ? null : streams || !creating ? (inst?.replicationLagBytes ?? 0) : null,
      lagSeconds:
        role === 'primary'
          ? null
          : streams || !creating
            ? (inst?.replicationLagSeconds ?? 0)
            : null,
      isOnPreferredServer: role === 'primary' ? serverId === g.preferredPrimaryServerId : true,
    }
  }
  const primary = g.primaryServerId === null ? null : member(g.primaryServerId, 'primary')
  const standbys = g.standbyServerIds.map((id) => member(id, 'standby'))
  const replicaCount = s.settings.defaultReplicaCount
  const lease = leaseView(s, g)
  const primaryDown =
    primary !== null && (primary.state === 'stopped' || primary.state === 'failed')
  const standbyDown = g.standbyServerIds.some((id) => !standbyStreams(s, g, id))
  return {
    id: g.id,
    isMapGroup: g.isMapGroup,
    preferredPrimaryServerId: g.preferredPrimaryServerId,
    health:
      !primary || primaryDown
        ? 'unavailable'
        : standbys.length < replicaCount || standbyDown
          ? 'degraded'
          : 'healthy',
    primary,
    standbys,
    slots: standbys.map((m) => ({
      name: g.managed?.includes(m.serverId)
        ? `kingres_standby_${g.standbyIds?.[m.serverId] ?? 0}`
        : `kdb_slot_g${g.id}_s${m.serverId}`,
      type: 'physical' as const,
      active: standbyStreams(s, g, m.serverId),
      instanceId: m.instanceId,
      retainedWalBytes: standbyStreams(s, g, m.serverId) ? 16 * MiB : 480 * MiB,
    })),
    ...(lease ? { lease } : {}),
    failover: readinessOf(s, g),
  }
}

export function topology(s: MockState): ReplicationTopology {
  return { generatedAt: nowIso(), groups: s.groups.map((g) => groupView(s, g)) }
}

export function bucketCounts(s: MockState): ClusterOverview['buckets'] {
  let underReplicated = 0
  let unavailable = 0
  let moving = 0
  let orphan = 0
  const all = allBuckets(s)
  for (const b of all) {
    const g = s.groups.find((x) => x.id === b.group)
    if (!g || g.primaryServerId === null) unavailable += 1
    else if (g.standbyServerIds.length < s.settings.defaultReplicaCount) underReplicated += 1
    if (b.state === 'moving') moving += 1
    if (b.state === 'orphan') orphan += 1
  }
  return { total: all.length, underReplicated, unavailable, moving, orphan }
}

export function overview(s: MockState): ClusterOverview {
  const count = (st: ServerStatus) => s.servers.filter((x) => effectiveStatus(x) === st).length
  const buckets = bucketCounts(s)
  const critical = s.alerts.filter((a) => a.state === 'open' && a.severity === 'critical').length
  const warning = s.alerts.filter((a) => a.state === 'open' && a.severity === 'warning').length
  const info = s.alerts.filter((a) => a.state === 'open' && a.severity === 'info').length
  const dead = count('dead')
  return {
    clusterId: s.clusterId,
    name: s.clusterName,
    health:
      buckets.unavailable > 0 || dead > 0
        ? 'critical'
        : buckets.underReplicated > 0 || count('suspect') > 0
          ? 'degraded'
          : 'healthy',
    version: '0.1.0',
    mapVersion: s.mapVersion,
    mapGroupAvailable: s.groups.find((g) => g.isMapGroup)?.primaryServerId !== null,
    managerLeader: {
      serverId: '10.0.0.11',
      address: s.servers[0]?.address ?? '',
      since: ago(86400 * 5),
    },
    servers: {
      total: s.servers.length,
      live: count('live'),
      suspect: count('suspect'),
      draining: count('draining'),
      decommissioning: count('decommissioning'),
      dead,
      pendingRebalance: count('pendingRebalance'),
    },
    buckets,
    databaseCount: s.databases.length,
    rebalance: {
      needed: s.rebalance.needed,
      reasons: s.rebalance.reasons,
      running: s.rebalance.state === 'running' || s.rebalance.state === 'stopping',
      taskId: s.rebalance.taskId,
    },
    autoFailover: {
      enabled: s.settings.autoFailover.enabled,
      eventsSinceReset: s.settings.autoFailover.eventsSinceReset,
      maxCount: s.settings.autoFailover.maxCount,
      limitReached: s.settings.autoFailover.eventsSinceReset >= s.settings.autoFailover.maxCount,
      lastEventAt: s.settings.autoFailover.lastEventAt ?? null,
    },
    openAlerts: { critical, warning, info },
    features: s.settings.features,
    updatedAt: nowIso(),
  }
}

/**
 * Contract 0.7.0: the chains of a status are those of the shard group of the answering server; the other groups are
 * listed as not covered. A database that is not sharded has one chain. `mockConfig.incompleteLedger` makes the last
 * shard group uncovered as the digest does; otherwise a status covers the group of the answering server only.
 */
function ledgerCoverage(
  s: MockState,
  db: DbRec,
  groups: number[],
): { complete?: boolean; uncoveredShardGroupIds?: number[] } {
  if (mockConfig.ledgerStatusCoverage === 'absent') return {}
  if (!db.info.sharding.enabled || groups.length <= 1)
    return { complete: true, uncoveredShardGroupIds: [] }
  const local = s.groups.find((g) => g.primaryServerId === ANSWERING_SERVER && !g.isMapGroup)
  const covered =
    mockConfig.ledgerStatusCoverage === 'all' ? groups : local ? [local.id] : groups.slice(0, 1)
  const uncovered = groups.filter((g) => !covered.includes(g))
  return { complete: uncovered.length === 0, uncoveredShardGroupIds: uncovered }
}

/**
 * The status of a database as `getLedgerStatus` answers it. `full` reads the chains of every shard group, which the
 * mock needs for what the real server computes by asking every group (digests, verifications, the database summary).
 */
export function ledgerStatusOf(s: MockState, db: DbRec, full = false): S['LedgerStatus'] {
  const verified = s.ledger.verifications.filter((v) => v.databaseName === db.info.name)
  const last = verified[0]
  const all = db.info.sharding.enabled ? dataGroups(s).map((g) => g.id) : [0]
  const coverage = full
    ? { complete: true, uncoveredShardGroupIds: [] }
    : ledgerCoverage(s, db, all)
  const groups = all.filter((g) => !(coverage.uncoveredShardGroupIds ?? []).includes(g))
  return {
    databaseName: db.info.name,
    mode: db.info.ledger.mode ?? 'off',
    digestIntervalSeconds: db.info.ledger.digestIntervalSeconds ?? 3600,
    verificationState:
      (db.info.ledger.mode ?? 'off') === 'off' ? 'never' : (last?.state ?? 'never'),
    lastVerifiedAt: last?.finishedAt ?? null,
    ledgerTableCount: db.tables.filter((t) => t.classes.includes('ledger')).length,
    ...coverage,
    chains:
      db.info.ledger.mode === 'off'
        ? []
        : groups.map((g) => ({
            shardGroupId: g,
            latestBlockHeight: s.ledger.heights[g] ?? 1000,
            latestBlockHash: fakeHash(`b${g}-${s.ledger.heights[g] ?? 1000}`),
            latestBlockAt: ago(40 + g * 7),
            lastVerifiedBlockHeight:
              last?.state === 'passed' ? (s.ledger.heights[g] ?? 1000) - 20 : null,
            verificationState: last?.state ?? 'never',
          })),
  }
}

/** The digest of one chain as the extension returns it (what `kingres-verify --digest` reads). */
export function digestDocument(g: number, height: number): Record<string, unknown> {
  return {
    format: 1,
    chain_id: `chain-${g}`,
    height,
    block_hash: fakeHash(`b${g}-${height}`),
    hash_version: 1,
    generated_at_us: 1_700_000_000_000_000 + height,
  }
}

export function ledgerBlock(g: number, height: number): LedgerBlock {
  return {
    shardGroupId: g,
    height,
    prevHash: height === 0 ? null : fakeHash(`b${g}-${height - 1}`),
    txMerkleRoot: fakeHash(`m${g}-${height}`),
    blockHash: fakeHash(`b${g}-${height}`),
    timestamp: ago((1900 - height) * 60 + g),
    hashVersion: 1,
    transactionCount: 3 + ((height * 7 + g) % 40),
    signed: false,
  }
}

// --- tasks --------------------------------------------------------------------------------------

export interface TaskInit {
  kind: TaskKind
  subject: Task['subject']
  steps: string[]
  detail?: string | null
  apply: () => void
  user?: string
  durationMs?: number
  failWith?: string
  holdAt?: number
  holdDetail?: string
  failAtEnd?: TaskRec['failAtEnd']
  skippedStep?: string
  onProgress?: (fraction: number) => void
  setup?: boolean
  cancellable?: boolean
  /** 0.10.0: the shard group of each step (null: a step of the whole task); the steps of a failover come in blocks. */
  stepGroups?: (number | null)[]
  /** Keeps the duration as given (a slow run), where the default caps it at 2.5 times the usual one. */
  uncapped?: boolean
  pauseAt?: number
  failAt?: TaskRec['failAt']
  onFail?: () => void
  onPause?: () => void
}

/** Creates a task. It starts running at once and is advanced by `tickTasks`. */
export function createTask(s: MockState, init: TaskInit): Task {
  const id = s.nextTaskId++
  const created = nowIso()
  const task: Task = {
    id,
    kind: init.kind,
    state: 'running',
    progress: {
      percent: 0,
      currentStep: init.steps[0] ?? null,
      steps: init.steps.map((name, i) => ({
        name,
        state: i === 0 ? 'running' : 'pending',
        ...(i === 0 && init.detail ? { detail: init.detail } : {}),
        ...(init.stepGroups ? { shardGroupId: init.stepGroups[i] ?? null } : {}),
      })),
    },
    error: null,
    detail: init.detail ?? null,
    subject: init.subject,
    cancellable: init.cancellable ?? false,
    createdAt: created,
    updatedAt: created,
    createdBy: init.user ?? s.session?.username ?? 'system',
  }
  s.tasks.unshift({
    task,
    startedAt: Date.now(),
    // Tasks that are slower than the default stay proportionally slower, but never more than 2.5x.
    durationMs: init.uncapped
      ? (init.durationMs ?? mockConfig.taskDurationMs)
      : Math.min(init.durationMs ?? mockConfig.taskDurationMs, mockConfig.taskDurationMs * 2.5),
    pauseAt: init.pauseAt,
    failAt: init.failAt,
    onFail: init.onFail,
    onPause: init.onPause,
    apply: init.apply,
    onProgress: init.onProgress,
    failWith: init.failWith,
    holdAt: init.holdAt,
    holdDetail: init.holdDetail,
    failAtEnd: init.failAtEnd,
    skippedStep: init.skippedStep,
    applied: false,
    setup: init.setup ?? false,
  })
  tickTasks(s)
  return structuredClone(task)
}

/** Advances every running task according to the wall clock; applies results when one finishes. */
export function tickTasks(s: MockState): void {
  const now = Date.now()
  for (const rec of s.tasks) {
    const t = rec.task
    if (t.state !== 'running') continue
    let fraction = rec.durationMs <= 0 ? 1 : Math.min(1, (now - rec.startedAt) / rec.durationMs)
    // A step that waits for a server holds the whole task, which never gives up (`config.apply`).
    const held = rec.holdAt !== undefined && fraction >= rec.holdAt
    if (held) fraction = rec.holdAt as number
    if (held && rec.holdDetail) t.detail = rec.holdDetail
    else if (rec.holdDetail && t.detail === rec.holdDetail) t.detail = null
    const stepCount = t.progress.steps.length
    const current = Math.min(stepCount - 1, Math.floor(fraction * stepCount))
    // A failing task stops half way through its work (a failover stops at the step it was told to).
    const failFraction = rec.failAt?.fraction ?? (rec.failWith ? 0.5 : undefined)
    if (failFraction !== undefined && fraction >= failFraction) {
      const failedAt = Math.min(stepCount - 1, Math.floor(failFraction * stepCount))
      const at = rec.failAt ? failedAt : current
      t.state = 'failed'
      t.error = {
        code: rec.failAt?.code ?? 'unprocessable',
        message: rec.failAt?.message ?? (rec.failWith as string),
        step: t.progress.steps[at]?.name ?? null,
        retryable: false,
      }
      t.progress.percent = Math.round(failFraction * 1000) / 10
      t.progress.currentStep = null
      t.progress.steps = t.progress.steps.map((st, i) => ({
        ...st,
        state: i < at ? 'done' : i === at ? 'failed' : 'skipped',
      }))
      t.cancellable = false
      t.updatedAt = nowIso()
      rec.onFail?.()
      continue
    }
    // A task that waits for a person stops at its step and is paused; its step is shown as the one it goes on with.
    if (rec.pauseAt !== undefined && fraction >= rec.pauseAt) {
      const at = Math.min(stepCount - 1, Math.floor(rec.pauseAt * stepCount))
      t.state = 'paused'
      t.progress.percent = Math.round(rec.pauseAt * 1000) / 10
      t.progress.currentStep = t.progress.steps[at]?.name ?? null
      t.progress.steps = t.progress.steps.map((st, i) => ({
        ...st,
        state: i < at ? 'done' : 'pending',
      }))
      t.updatedAt = nowIso()
      rec.onPause?.()
      continue
    }
    rec.onProgress?.(fraction)
    t.progress.percent = Math.round(fraction * 1000) / 10
    t.progress.currentStep = fraction >= 1 ? null : (t.progress.steps[current]?.name ?? null)
    t.progress.steps = t.progress.steps.map((st, i) => {
      let state: 'done' | 'running' | 'pending' | 'skipped' =
        fraction >= 1 || i < current ? 'done' : i === current ? 'running' : 'pending'
      if (state === 'done' && st.name === rec.skippedStep) state = 'skipped'
      // The running step repeats the task's detail (contract 0.4.0); a skipped step shows why (0.9.0).
      const detail = state === 'running' || state === 'skipped' ? t.detail : null
      const { detail: _old, ...rest } = st
      void _old
      return { ...rest, state, ...(detail ? { detail } : {}) }
    })
    t.updatedAt = nowIso()
    if (fraction >= 1 && !rec.applied) {
      rec.applied = true
      rec.apply()
      if (rec.failAtEnd) {
        const f = rec.failAtEnd
        t.state = 'failed'
        t.error = { code: f.code, message: f.message, step: f.step, retryable: true }
        t.progress.percent = 100
        t.progress.currentStep = null
        t.progress.steps = t.progress.steps.map((st) =>
          st.name === f.step ? { ...st, state: 'failed' as const, detail: f.detail } : st,
        )
        t.cancellable = false
        t.updatedAt = nowIso()
        s.mapVersion += 1
        continue
      }
      t.state = 'done'
      t.cancellable = false
      s.mapVersion += 1
    }
  }
}

export function findTask(s: MockState, id: number): TaskRec | undefined {
  tickTasks(s)
  return s.tasks.find((t) => t.task.id === id)
}

export function activeTask(s: MockState, kind: TaskKind, subjectId?: string): TaskRec | undefined {
  tickTasks(s)
  return s.tasks.find(
    (t) =>
      t.task.kind === kind &&
      t.task.state === 'running' &&
      (subjectId === undefined || t.task.subject?.id === subjectId),
  )
}

// --- standbys: the task `standby.reconcile` (contract 0.7.0) ---------------------------------------

/** The text kingresd records while the catalog step cannot do its work (the sharding extension has no standby nodes). */
export const CATALOG_SKIPPED_DETAIL = 'skipped: needs kingres_shard 0.5'
export const CATALOG_STEP = 'standby.register_in_catalog'

interface ReconcilePlan {
  remove: { group: GroupRec; server: string }[]
  add: { group: GroupRec; server: string; id: number }[]
}

/**
 * The placement of kingresd: the standbys of the group whose primary runs on server i go to the next servers after i
 * in the ring, servers of another server group first, never two instances of one group on a server. A standby
 * that works stays; a lower count removes the least preferred.
 */
function planReconcile(s: MockState): ReconcilePlan {
  const plan: ReconcilePlan = { remove: [], add: [] }
  const wanted = s.settings.defaultReplicaCount
  const ring = s.servers.map((x) => x.id)
  let nextId = s.nextStandbyId
  for (const g of s.groups) {
    if (g.primaryServerId === null) continue
    const primary = s.servers.find((x) => x.id === g.primaryServerId)
    const at = ring.indexOf(g.primaryServerId)
    const after = [...ring.slice(at + 1), ...ring.slice(0, at)]
    const otherGroup = after.filter(
      (id) => s.servers.find((x) => x.id === id)?.serverGroup !== primary?.serverGroup,
    )
    const sameGroup = after.filter((id) => !otherGroup.includes(id))
    const preference = [...otherGroup, ...sameGroup]
    const rank = (id: string) => preference.indexOf(id)
    const existing = g.standbyServerIds
      .filter((id) => rank(id) >= 0)
      .sort((a, b) => rank(a) - rank(b))
    const keep = existing.slice(0, wanted)
    for (const id of g.standbyServerIds)
      if (!keep.includes(id)) plan.remove.push({ group: g, server: id })
    for (const id of preference) {
      if (keep.length >= wanted) break
      if (keep.includes(id)) continue
      const rec = s.servers.find((x) => x.id === id)
      if (!rec || rec.status === 'dead' || isSilent(rec)) continue
      keep.push(id)
      plan.add.push({ group: g, server: id, id: nextId++ })
    }
  }
  return plan
}

/** True while a `standby.reconcile` task runs (at most one runs at a time). */
export function reconcileRunning(s: MockState): boolean {
  return s.tasks.some((t) => t.task.kind === 'standby.reconcile' && t.task.state === 'running')
}

/**
 * What the manager leader does when `defaultReplicaCount` is patched: it checks the standbys against the setting and,
 * when there is work, starts the task. Returns the task, or null when nothing has to be done (or one already runs).
 */
export function startStandbyReconcile(s: MockState, user = 'system'): Task | null {
  if (reconcileRunning(s)) return null
  const plan = planReconcile(s)
  if (plan.remove.length === 0 && plan.add.length === 0) return null
  s.nextStandbyId += plan.add.length
  const names = [
    ...plan.remove.map(
      (r) => `Remove the standby of shard group ${r.group.id} from server ${r.server}`,
    ),
    ...plan.add.map((a) => `Create the standby of shard group ${a.group.id} on server ${a.server}`),
    'Send the list of standbys to every server',
    CATALOG_STEP,
  ]
  const removed = new Set<string>()
  const started = new Set<string>()
  const ready = new Set<string>()
  let taskId = -1
  const advance = (fraction: number): void => {
    const total = names.length
    plan.remove.forEach((r, i) => {
      const key = `${r.group.id}:${r.server}`
      if (fraction < i / total || removed.has(key)) return
      removed.add(key)
      r.group.standbyServerIds = r.group.standbyServerIds.filter((x) => x !== r.server)
      r.group.managed = (r.group.managed ?? []).filter((x) => x !== r.server)
      r.group.creating = (r.group.creating ?? []).filter((x) => x !== r.server)
      standbyAudit(s, 'standbyRemoved', r.group.id, r.server)
    })
    plan.add.forEach((a, i) => {
      const key = `${a.group.id}:${a.server}`
      const index = plan.remove.length + i
      if (fraction >= index / total && !started.has(key)) {
        started.add(key)
        a.group.standbyServerIds.push(a.server)
        a.group.standbyIds = { ...(a.group.standbyIds ?? {}), [a.server]: a.id }
        a.group.managed = [...(a.group.managed ?? []), a.server]
        a.group.creating = [...(a.group.creating ?? []), a.server]
      }
      if (fraction >= (index + 1) / total && !ready.has(key)) {
        ready.add(key)
        a.group.creating = (a.group.creating ?? []).filter((x) => x !== a.server)
        standbyAudit(s, 'standbyCreated', a.group.id, a.server)
      }
    })
    // The last step records why it did nothing, on the task (it succeeds).
    if (fraction >= (total - 1) / total && plan.add.length > 0) {
      const rec = s.tasks.find((t) => t.task.id === taskId)
      if (rec) rec.task.detail = CATALOG_SKIPPED_DETAIL
    }
  }
  const task = createTask(s, {
    kind: 'standby.reconcile',
    subject: { type: 'replication', id: 'standbys' },
    steps: names,
    skippedStep: CATALOG_STEP,
    user,
    durationMs: 6000,
    onProgress: advance,
    apply: () => advance(1),
  })
  taskId = task.id
  return task
}

function standbyAudit(
  s: MockState,
  action: AuditEvent['action'],
  groupId: number,
  server: string,
): void {
  s.audit.unshift({
    id: s.nextAuditId++,
    timestamp: nowIso(),
    actor: 'system',
    source: 'system',
    serverId: ANSWERING_SERVER,
    action,
    resourceType: 'standby',
    resourceId: `${groupId}@${server}`,
    outcome: 'success',
  })
}

// --- cluster operations (applied when tasks finish) ------------------------------------------------

/** Buckets that must move to even out the groups; used by the plan preview and by start. */
export function planMoves(s: MockState, databases?: string[]): S['PlannedMove'][] {
  const groupIds = dataGroups(s)
    .filter(
      (g) => s.servers.find((x) => x.id === g.preferredPrimaryServerId)?.shouldHaveShards !== false,
    )
    .map((g) => g.id)
  const moves: S['PlannedMove'][] = []
  for (const db of s.databases) {
    if (databases?.length && !databases.includes(db.info.name)) continue
    for (const [colocationId, buckets] of db.buckets) {
      const target = Math.floor(buckets.length / groupIds.length)
      const counts = new Map<number, number>(groupIds.map((g) => [g, 0]))
      for (const b of buckets) counts.set(b.group, (counts.get(b.group) ?? 0) + 1)
      const surplus = buckets
        .filter((b) => !groupIds.includes(b.group) || (counts.get(b.group) ?? 0) > target)
        .reverse()
      const needy = groupIds.filter((g) => (counts.get(g) ?? 0) < target)
      let rank = 1000
      for (const b of surplus) {
        const to = needy.find((g) => (counts.get(g) ?? 0) < target)
        if (to === undefined) break
        if (groupIds.includes(b.group)) {
          if ((counts.get(b.group) ?? 0) <= target) continue
          counts.set(b.group, (counts.get(b.group) ?? 0) - 1)
        }
        counts.set(to, (counts.get(to) ?? 0) + 1)
        moves.push({
          databaseName: db.info.name,
          colocationId,
          bucket: b.bucket,
          fromShardGroupId: b.group,
          toShardGroupId: to,
          estimatedSizeBytes: b.size,
          rank: Math.max(0, rank--),
        })
      }
    }
  }
  return moves
}

export function planView(s: MockState, limit: number, databases?: string[]): RebalancePlan {
  const moves = planMoves(s, databases)
  const bytes = moves.reduce((n, m) => n + (m.estimatedSizeBytes ?? 0), 0)
  const groupTargets = dataGroups(s).map((g) => g.id)
  return {
    needed: moves.length > 0,
    totals: {
      moveCount: moves.length,
      totalBytes: bytes,
      estimatedDurationSeconds: Math.round(bytes / (40 * MiB)) + moves.length,
    },
    truncated: moves.length > limit,
    moves: moves.slice(0, limit),
    serverImpacts: s.servers.map((srv) => {
      const g = s.groups.find((x) => !x.isMapGroup && x.preferredPrimaryServerId === srv.id)
      const before = g ? allBuckets(s).filter((b) => b.group === g.id).length : 0
      const delta = g
        ? moves.filter((m) => m.toShardGroupId === g.id).length -
          moves.filter((m) => m.fromShardGroupId === g.id).length
        : 0
      return {
        serverId: srv.id,
        primaryBucketsBefore: before,
        primaryBucketsAfter: groupTargets.includes(g?.id ?? -1) ? before + delta : 0,
      }
    }),
    warnings:
      moves.length > 100 ? ['More than 100 buckets will move; run this outside peak hours.'] : [],
  }
}

/** Applies planned moves up to `count`, marks the next few as moving. */
export function applyMoveProgress(
  s: MockState,
  moves: S['PlannedMove'][],
  done: number,
  inFlight: number,
): void {
  moves.forEach((m, i) => {
    const db = s.databases.find((d) => d.info.name === m.databaseName)
    const rec = db?.buckets.get(m.colocationId)?.find((b) => b.bucket === m.bucket)
    if (!rec) return
    if (i < done) {
      rec.group = m.toShardGroupId
      rec.state = 'active'
      rec.movingTo = null
    } else if (i < done + inFlight) {
      rec.state = 'moving'
      rec.movingTo = m.toShardGroupId
    }
  })
}
