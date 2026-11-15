// SPDX-License-Identifier: PostgreSQL
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
  RebalanceReportSummary,
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
   * The address the mock server sees the client at (`Session.clientAddress`); null: the server does not report
   * it (an older revision). Documentation range, never a real host.
   */
  clientAddress: '203.0.113.7' as string | null,
}

// --- types of the internal records ------------------------------------------------------------

interface ServerRec {
  id: string
  address: string
  hostname: string
  status: ServerStatus
  serverGroup: string | null
  shouldHaveShards: boolean
  version: string
  joinedAt: string
  startedAt: number
  /** False when the server has not reported resource usage and uptime (they are null then). */
  telemetry: boolean
  certificateExpiresAt?: string | null
  seenFingerprint?: string | null
  fingerprintPinned?: boolean
  certificateRevokedAt?: string | null
}

interface GroupRec {
  id: number
  isMapGroup: boolean
  preferredPrimaryServerId: string
  primaryServerId: string | null
  standbyServerIds: string[]
  failedServerIds: string[]
}

interface BucketRec {
  bucket: number
  group: number
  state: 'active' | 'moving' | 'orphan'
  movingTo: number | null
  /** Null when the size of the bucket is not known (shown as "not available"). */
  size: number | null
}

interface DbRec {
  info: Database
  tables: Table[]
  colocations: ColocationGroup[]
  buckets: Map<number, BucketRec[]>
  /** Columns to report instead of the built-in ones, by table name (hostile-data tests). */
  extraColumns?: Record<string, S['TableColumn'][]>
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
  rebalance: {
    needed: boolean
    reasons: S['RebalanceReason'][]
    state: RebalanceStatus['state']
    taskId: number | null
    startedAt: string | null
    options: S['RebalanceOptions'] | null
    moves: S['PlannedMove'][]
    reports: (RebalanceReportSummary & { moves: S['RebalanceReport']['moves'] })[]
  }
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
  session: SessionRec | null
  /** Result sets the workbench can select from, by table name (hostile-data tests). */
  extraQueryTables?: Record<string, { columns: S['QueryColumn'][]; rows: unknown[][] }>
}

// --- helpers ----------------------------------------------------------------------------------

export const nowIso = (): string => new Date().toISOString()
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
    sharding: { enabled: false, bucketCount: 128 },
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
      sharding: { enabled: true, bucketCount: 128 },
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
        raisedAt: ago(86400 * 3),
        resolvedAt: ago(86400 * 3 - 40),
      },
    ],
    settings: {
      defaultReplicaCount: 1,
      defaultDurability: 'sync',
      autoFailover: { enabled: true, timeoutSeconds: 120, maxCount: 1, eventsSinceReset: 0 },
      rebalance: { maxConcurrentMoves: 4 },
      features: { sharding: true, ledger: true },
      restart: { required: false, serverIds: [] },
    },
    rebalance: {
      needed: false,
      reasons: [],
      state: 'idle',
      taskId: null,
      startedAt: null,
      options: null,
      moves: [],
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
    session: null,
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
  const dead = server?.status === 'dead'
  for (const g of s.groups) {
    const make = (role: 'primary' | 'standby', failed: boolean): Instance => ({
      id: instanceId(g.id, serverId),
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
    status: rec.status,
    serverGroup: rec.serverGroup,
    shouldHaveShards: rec.shouldHaveShards,
    isManagerLeader: rec.id === '10.0.0.11',
    certificateExpiresAt: rec.certificateExpiresAt ?? null,
    certificateRevokedAt: rec.certificateRevokedAt ?? null,
    seenFingerprint: rec.seenFingerprint ?? null,
    fingerprintPinned: rec.seenFingerprint ? (rec.fingerprintPinned ?? false) : false,
    // The version of a server that has not reported yet is unknown.
    version: rec.telemetry ? rec.version : null,
    uptimeSeconds: rec.telemetry ? Math.floor((Date.now() - rec.startedAt) / 1000) : null,
    joinedAt: rec.joinedAt,
    lastHeartbeatAt: rec.status === 'dead' ? ago(600) : ago(1),
    instances,
    buckets: { primary, replica },
    sizeBytes,
    replicationLagMaxSeconds: lags.length ? Math.max(...lags) : null,
    resources: rec.telemetry
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

export function groupView(s: MockState, g: GroupRec): ShardGroup {
  const member = (
    serverId: string,
    role: 'primary' | 'standby',
    index: number,
  ): S['ReplicationMember'] => {
    const inst = instancesOfServer(s, serverId).find((i) => i.shardGroupId === g.id)
    return {
      instanceId: instanceId(g.id, serverId),
      serverId,
      role,
      state: inst?.state ?? 'running',
      port: portFor(g.id, serverId),
      timeline: 3,
      syncState: role === 'primary' ? null : index === 0 ? 'quorum' : 'async',
      lagBytes: role === 'primary' ? null : (inst?.replicationLagBytes ?? 0),
      lagSeconds: role === 'primary' ? null : (inst?.replicationLagSeconds ?? 0),
      isOnPreferredServer: role === 'primary' ? serverId === g.preferredPrimaryServerId : true,
    }
  }
  const primary = g.primaryServerId === null ? null : member(g.primaryServerId, 'primary', 0)
  const standbys = g.standbyServerIds.map((id, i) => member(id, 'standby', i))
  const replicaCount = s.settings.defaultReplicaCount
  return {
    id: g.id,
    isMapGroup: g.isMapGroup,
    preferredPrimaryServerId: g.preferredPrimaryServerId,
    health: !primary ? 'unavailable' : standbys.length < replicaCount ? 'degraded' : 'healthy',
    primary,
    standbys,
    slots: standbys.map((m) => ({
      name: `kdb_slot_g${g.id}_s${m.serverId}`,
      type: 'physical' as const,
      active: true,
      instanceId: m.instanceId,
      retainedWalBytes: 16 * MiB,
    })),
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
  const count = (st: ServerStatus) => s.servers.filter((x) => x.status === st).length
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
    },
    openAlerts: { critical, warning, info },
    features: s.settings.features,
    updatedAt: nowIso(),
  }
}

export function ledgerStatusOf(s: MockState, db: DbRec): S['LedgerStatus'] {
  const verified = s.ledger.verifications.filter((v) => v.databaseName === db.info.name)
  const last = verified[0]
  const groups = db.info.sharding.enabled ? dataGroups(s).map((g) => g.id) : [0]
  return {
    databaseName: db.info.name,
    mode: db.info.ledger.mode ?? 'off',
    digestIntervalSeconds: db.info.ledger.digestIntervalSeconds ?? 3600,
    verificationState:
      (db.info.ledger.mode ?? 'off') === 'off' ? 'never' : (last?.state ?? 'never'),
    lastVerifiedAt: last?.finishedAt ?? null,
    ledgerTableCount: db.tables.filter((t) => t.classes.includes('ledger')).length,
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
  onProgress?: (fraction: number) => void
  setup?: boolean
  cancellable?: boolean
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
    durationMs: Math.min(
      init.durationMs ?? mockConfig.taskDurationMs,
      mockConfig.taskDurationMs * 2.5,
    ),
    apply: init.apply,
    onProgress: init.onProgress,
    failWith: init.failWith,
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
    const fraction = rec.durationMs <= 0 ? 1 : Math.min(1, (now - rec.startedAt) / rec.durationMs)
    const stepCount = t.progress.steps.length
    const current = Math.min(stepCount - 1, Math.floor(fraction * stepCount))
    // A failing task stops half way through its work.
    if (rec.failWith && fraction >= 0.5) {
      t.state = 'failed'
      t.error = {
        code: 'unprocessable',
        message: rec.failWith,
        step: t.progress.steps[current]?.name ?? null,
        retryable: false,
      }
      t.progress.percent = 50
      t.progress.steps = t.progress.steps.map((st, i) => ({
        ...st,
        state: i < current ? 'done' : i === current ? 'failed' : 'skipped',
      }))
      t.cancellable = false
      t.updatedAt = nowIso()
      continue
    }
    rec.onProgress?.(fraction)
    t.progress.percent = Math.round(fraction * 1000) / 10
    t.progress.currentStep = fraction >= 1 ? null : (t.progress.steps[current]?.name ?? null)
    t.progress.steps = t.progress.steps.map((st, i) => {
      const state = fraction >= 1 || i < current ? 'done' : i === current ? 'running' : 'pending'
      // The running step repeats the task's detail (contract 0.4.0).
      const detail = state === 'running' ? t.detail : null
      const { detail: _old, ...rest } = st
      void _old
      return { ...rest, state, ...(detail ? { detail } : {}) }
    })
    t.updatedAt = nowIso()
    if (fraction >= 1 && !rec.applied) {
      rec.applied = true
      rec.apply()
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
