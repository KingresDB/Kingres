// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// One handler per operation in the contract, registered by operationId (see http.ts). Request and
// response bodies are typed with the generated schema types.
import type { HttpHandler } from 'msw'
import type { components } from '@/api/schema'
import type { OperationId } from '@/api/operations.generated'
import type { Alert, Database, Role, Table } from '@/api/types'
import {
  accepted,
  created,
  getState,
  noContent,
  ok,
  paginate,
  problem,
  register,
  requireTask,
  validationFailed,
  type Ctx,
  type Handler,
} from './http'
import {
  applyMoveProgress,
  bucketCounts,
  bucketView,
  createTask,
  dataGroups,
  fakeHash,
  hostNumber,
  groupView,
  instancesOfServer,
  ledgerBlock,
  ledgerStatusOf,
  makeBuckets,
  MOCK_PASSWORD,
  MOCK_SETUP_TOKEN,
  nowIso,
  overview,
  planMoves,
  planView,
  serverView,
  tickTasks,
  topology,
  activeTask,
  type MockState,
} from './state'

type S = components['schemas']
const NAME_RE = /^[a-z_][a-z0-9_]{0,62}$/
const ADDRESS_RE = /^(\[[0-9a-fA-F:]+\]|[A-Za-z0-9.-]+):([0-9]{1,5})$/

/** host:port with the port in 1..65535 (the tightened NodeAddress rule of contract 0.2.0). */
function validAddress(address: string): boolean {
  const m = ADDRESS_RE.exec(address)
  const port = Number(m?.[2])
  return m !== null && port >= 1 && port <= 65535
}
const MiB = 1024 * 1024

const body = <T>(ctx: Ctx): T => (ctx.body ?? {}) as T
const num = (ctx: Ctx, name: string): number => {
  const n = Number(ctx.params[name])
  if (!Number.isInteger(n))
    throw problem('bad_request', `Path parameter ${name} must be an integer.`)
  return n
}

function db(s: MockState, name: string) {
  const rec = s.databases.find((d) => d.info.name === name)
  if (!rec) throw problem('not_found', `Database "${name}" does not exist.`)
  return rec
}

/** The server that serves this panel in the mock. */
const LOCAL = '10.0.0.11'

/** The summary of the accepted setup request, as returned by GET /setup/state. */
function setupTaskSummary(s: MockState): S['SetupTaskSummary'] | null {
  const rec = s.tasks.find((x) => x.setup)
  if (!rec) return null
  return {
    id: rec.task.id,
    kind: rec.task.kind === 'cluster.join' ? 'cluster.join' : 'cluster.init',
    state: rec.task.state,
    progressPercent: rec.task.progress.percent,
    error: rec.task.error,
    steps: rec.task.progress.steps.map((st) => ({
      name: st.name,
      state: st.state === 'skipped' ? 'pending' : st.state,
    })),
    createdAt: rec.task.createdAt,
    updatedAt: rec.task.updatedAt,
  }
}

/** The newest report without its moves (RebalanceStatus.lastReport). */
function lastReportOf(s: MockState): S['RebalanceReportSummary'] | null {
  const latest = s.rebalance.reports[0]
  if (!latest) return null
  const { moves: _moves, ...summary } = latest
  return summary
}

function server(s: MockState, id: string) {
  const rec = s.servers.find((x) => x.id === id)
  if (!rec) throw problem('not_found', `Server ${id} does not exist.`)
  return rec
}

function markRebalance(s: MockState, reason: S['RebalanceReason']): void {
  s.rebalance.needed = true
  if (!s.rebalance.reasons.includes(reason)) s.rebalance.reasons.push(reason)
}

function requireNoRebalance(s: MockState): void {
  if (s.rebalance.state === 'running' || s.rebalance.state === 'stopping') {
    throw problem('rebalance_in_progress', 'A rebalance is running.')
  }
}

/** Gives each data group as many standbys as the replica count asks for. */
function fixReplicas(s: MockState): void {
  const live = s.servers.filter((x) => x.status !== 'dead')
  for (const g of dataGroups(s)) {
    const wanted = s.settings.defaultReplicaCount
    g.standbyServerIds = g.standbyServerIds.filter(
      (id) => live.some((x) => x.id === id) && id !== g.primaryServerId,
    )
    for (const candidate of live) {
      if (g.standbyServerIds.length >= wanted) break
      if (candidate.id !== g.primaryServerId && !g.standbyServerIds.includes(candidate.id))
        g.standbyServerIds.push(candidate.id)
    }
    g.standbyServerIds = g.standbyServerIds.slice(0, wanted)
    g.failedServerIds = []
  }
}

// --- authentication and setup ---------------------------------------------------------------------

const login: Handler = (ctx) => {
  const { username, password, issueToken } = body<S['LoginRequest']>(ctx)
  const role = ctx.s.roles.find((r) => r.name === username)
  if (!role || !role.canLogin || ctx.s.passwords.get(username) !== password) {
    ctx.s.audit.unshift({
      id: ctx.s.nextAuditId++,
      timestamp: nowIso(),
      actor: username,
      source: 'panel',
      action: 'loginFailed',
      outcome: 'failure',
    })
    throw problem('invalid_credentials')
  }
  const csrfToken = fakeHash(`${username}${Date.now()}`).slice(0, 32)
  const session = {
    username,
    clusterRoles: role.clusterRoles,
    csrfToken,
    expiresAt: new Date(Date.now() + 8 * 3600_000).toISOString(),
  }
  ctx.s.session = session
  ctx.s.audit.unshift({
    id: ctx.s.nextAuditId++,
    timestamp: nowIso(),
    actor: username,
    source: 'panel',
    action: 'login',
    outcome: 'success',
  })
  const out: S['Session'] = {
    ...session,
    serverId: LOCAL,
    clusterId: ctx.s.clusterId,
    clusterName: ctx.s.clusterName,
    token: issueToken ? `mock-token-${csrfToken}` : null,
  }
  return ok(out)
}

function sessionBody(s: MockState): S['Session'] {
  const sess = s.session
  if (!sess) throw problem('unauthenticated')
  return {
    ...sess,
    serverId: LOCAL,
    clusterId: s.clusterId,
    clusterName: s.clusterName,
    token: null,
  }
}

function requireSetupToken(ctx: Ctx): void {
  if (ctx.request.headers.get('X-Setup-Token') !== MOCK_SETUP_TOKEN)
    throw problem('setup_token_invalid')
}

/** A second setup request while one runs is a conflict (the token stays valid after a failure). */
function requireNoRunningSetup(s: MockState): void {
  if (s.tasks.some((x) => x.setup && x.task.state === 'running'))
    throw problem('operation_in_progress', 'A setup request is already running.')
}

const checkSetupToken: Handler = (ctx) => {
  if (ctx.s.node.state !== 'uninitialized') throw problem('node_state_invalid')
  requireSetupToken(ctx)
  return noContent()
}

const initCluster: Handler = (ctx) => {
  requireSetupToken(ctx)
  requireNoRunningSetup(ctx.s)
  const b = body<S['InitClusterRequest']>(ctx)
  const errors: { field: string; message: string }[] = []
  if (!/^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$/.test(b.clusterName ?? ''))
    errors.push({
      field: 'clusterName',
      message: 'Use letters, digits, "-" and "_" (up to 63 characters).',
    })
  if (!/^[a-z_][a-z0-9_]{0,62}$/.test(b.admin?.username ?? ''))
    errors.push({ field: 'admin.username', message: 'Use lower-case letters, digits and "_".' })
  if ((b.admin?.password ?? '').length < 12)
    errors.push({
      field: 'admin.password',
      message: 'The password must have at least 12 characters.',
    })
  if (!b.dataPaths?.dataDirectory?.startsWith('/'))
    errors.push({ field: 'dataPaths.dataDirectory', message: 'Use an absolute path.' })
  if ((b.memoryQuota?.totalMegabytes ?? 0) < 512)
    errors.push({ field: 'memoryQuota.totalMegabytes', message: 'At least 512 MB.' })
  if (errors.length) throw validationFailed(errors)
  const s = ctx.s
  return accepted(
    createTask(s, {
      kind: 'cluster.init',
      subject: { type: 'cluster', id: b.clusterName },
      steps: [
        'Initialize map group',
        'Initialize first shard group',
        'Start etcd',
        'Create administrator',
      ],
      setup: true,
      user: 'setup',
      // A cluster named "failme" makes setup fail, to see how the wizard reports it.
      failWith:
        b.clusterName === 'failme'
          ? 'Could not initialize the map group: the data directory is not writable.'
          : undefined,
      apply: () => {
        s.node.state = 'clustered'
        s.clusterName = b.clusterName
        s.settings.defaultReplicaCount = b.defaultReplicaCount ?? 1
        s.settings.features = b.features ?? { sharding: true, ledger: true }
        s.roles = [
          {
            name: b.admin.username,
            canLogin: true,
            isSuperuser: true,
            canCreateDatabase: true,
            canCreateRole: true,
            connectionLimit: -1,
            validUntil: null,
            memberOf: [],
            clusterRoles: ['kingres_admin'],
          },
        ]
        s.passwords = new Map([[b.admin.username, b.admin.password]])
        s.databases = []
        s.session = null
      },
    }),
  )
}

const joinCluster: Handler = (ctx) => {
  requireSetupToken(ctx)
  requireNoRunningSetup(ctx.s)
  const b = body<S['JoinClusterRequest']>(ctx)
  const errors: { field: string; message: string }[] = []
  if (!validAddress(b.existingNodeAddress ?? ''))
    errors.push({
      field: 'existingNodeAddress',
      message: 'Use host:port, for example 10.0.0.11:8091.',
    })
  if (!b.admin?.username) errors.push({ field: 'admin.username', message: 'Required.' })
  if (!b.dataPaths?.dataDirectory?.startsWith('/'))
    errors.push({ field: 'dataPaths.dataDirectory', message: 'Use an absolute path.' })
  if (errors.length) throw validationFailed(errors)
  if (b.existingNodeAddress.includes('unreachable'))
    throw problem('unprocessable', `Cannot reach ${b.existingNodeAddress}.`)
  if (b.admin.password !== MOCK_PASSWORD) throw problem('invalid_credentials')
  const s = ctx.s
  return accepted(
    createTask(s, {
      kind: 'cluster.join',
      subject: { type: 'cluster', id: b.existingNodeAddress },
      steps: [
        'Contact existing server',
        'Receive certificates',
        'Create shard group instance',
        'Register with manager',
      ],
      setup: true,
      user: 'setup',
      apply: () => {
        s.node.state = 'clustered'
        s.session = null
      },
    }),
  )
}

// --- this server -----------------------------------------------------------------------------------

function thisServerInstance(s: MockState, id: number) {
  const inst = instancesOfServer(s, LOCAL).find((i) => i.id === id)
  if (!inst) throw problem('not_found', `Instance ${id} does not run on this server.`)
  return inst
}

function instanceAction(action: 'restart' | 'stop' | 'start'): Handler {
  return (ctx) => {
    const id = num(ctx, 'instanceId')
    const s = ctx.s
    thisServerInstance(s, id)
    if (activeTask(s, `instance.${action}`, String(id))) throw problem('operation_in_progress')
    const steps =
      action === 'stop'
        ? ['Stop PostgreSQL']
        : action === 'start'
          ? ['Start PostgreSQL', 'Wait for recovery']
          : ['Stop PostgreSQL', 'Start PostgreSQL']
    if (action !== 'start') s.instanceStates.set(id, action === 'stop' ? 'stopping' : 'restarting')
    return accepted(
      createTask(s, {
        kind: `instance.${action}`,
        subject: { type: 'instance', id: String(id) },
        steps,
        durationMs: 2500,
        apply: () => s.instanceStates.set(id, action === 'stop' ? 'stopped' : 'running'),
      }),
    )
  }
}

// --- databases ----------------------------------------------------------------------------------------

/** Choices and defaults for the create-database dialog (GET /database-create-options). */
function createOptions(s: MockState): S['DatabaseCreateOptions'] {
  return {
    encodings: [
      { name: 'UTF8', isDefault: true },
      { name: 'LATIN1', isDefault: false },
      { name: 'SQL_ASCII', isDefault: false },
    ],
    collations: [
      { name: 'en_US.UTF-8', encodings: ['UTF8'], isDefault: true },
      { name: 'de_DE.UTF-8', encodings: ['UTF8'], isDefault: false },
      { name: 'de_DE.ISO-8859-1', encodings: ['LATIN1'], isDefault: true },
      { name: 'C', encodings: null, isDefault: false },
    ],
    bucketCount: { default: 128, minimum: 1, maximum: 4096 },
    replicaCount: s.settings.defaultReplicaCount,
    defaultDurability: s.settings.defaultDurability,
    ledgerDigestIntervalSeconds: { default: 3600, minimum: 60, maximum: 86400 },
  }
}

function validateDatabase(s: MockState, b: S['DatabaseCreateRequest']): void {
  const options = createOptions(s)
  const errors: { field: string; message: string }[] = []
  if (!NAME_RE.test(b.name ?? ''))
    errors.push({
      field: 'name',
      message:
        'Use lower-case letters, digits and "_", starting with a letter or "_" (up to 63 characters).',
    })
  const buckets = b.sharding?.bucketCount
  if (buckets !== undefined && (!Number.isInteger(buckets) || buckets < 1 || buckets > 4096))
    errors.push({
      field: 'sharding.bucketCount',
      message: 'The bucket count must be between 1 and 4096.',
    })
  const interval = b.ledger?.digestIntervalSeconds
  if (interval !== undefined && (interval < 60 || interval > 86400))
    errors.push({
      field: 'ledger.digestIntervalSeconds',
      message: 'The digest interval must be between 60 and 86400 seconds.',
    })
  const limit = b.general?.connectionLimit
  if (limit !== undefined && limit < -1)
    errors.push({
      field: 'general.connectionLimit',
      message: 'Use -1 for unlimited or a positive number.',
    })
  if (b.general?.owner && !s.roles.some((r) => r.name === b.general?.owner))
    errors.push({ field: 'general.owner', message: `Role "${b.general.owner}" does not exist.` })
  const encoding = b.general?.encoding
  if (encoding !== undefined && !options.encodings.some((e) => e.name === encoding))
    errors.push({ field: 'general.encoding', message: `Encoding ${encoding} is not offered.` })
  if (errors.length) throw validationFailed(errors)
}

function newDatabase(s: MockState, b: S['DatabaseCreateRequest']): Database {
  return {
    name: b.name,
    status: 'ready',
    general: {
      owner: b.general?.owner ?? s.session?.username ?? 'admin',
      encoding: b.general?.encoding ?? 'UTF8',
      lcCollate: b.general?.lcCollate ?? 'en_US.UTF-8',
      lcCtype: b.general?.lcCtype ?? 'en_US.UTF-8',
      connectionLimit: b.general?.connectionLimit ?? -1,
    },
    sharding: {
      enabled: b.sharding?.enabled ?? false,
      bucketCount: b.sharding?.bucketCount ?? 128,
    },
    replication: {
      durability: b.replication?.durability ?? s.settings.defaultDurability,
      replicaCount: s.settings.defaultReplicaCount,
    },
    ledger: {
      mode: b.ledger?.mode ?? 'off',
      digestIntervalSeconds: b.ledger?.digestIntervalSeconds ?? 3600,
    },
    stats: { sizeBytes: 8 * MiB, tableCount: 0, activeConnections: 0, transactionsPerSecond: 0 },
    health: { underReplicatedBuckets: 0, unavailableBuckets: 0 },
    ledgerSummary: { verificationState: 'never', lastVerifiedBlockHeight: null },
    rebalanceRequired: false,
    createdBy: s.session?.username ?? 'admin',
    createdAt: nowIso(),
  }
}

/** Database as the API returns it: health and ledger summary are derived from the live state. */
function databaseView(s: MockState, name: string): Database {
  const rec = db(s, name)
  const counts = [...rec.buckets.values()].flat()
  let under = 0
  let unavailable = 0
  for (const b of counts) {
    const g = s.groups.find((x) => x.id === b.group)
    if (!g || g.primaryServerId === null) unavailable += 1
    else if (g.standbyServerIds.length < s.settings.defaultReplicaCount) under += 1
  }
  const ledger = ledgerStatusOf(s, rec)
  const lastVerified = Math.min(...ledger.chains.map((c) => c.lastVerifiedBlockHeight ?? Infinity))
  return {
    ...rec.info,
    replication: { ...rec.info.replication, replicaCount: s.settings.defaultReplicaCount },
    stats: { ...rec.info.stats, tableCount: rec.tables.length },
    health: { underReplicatedBuckets: under, unavailableBuckets: unavailable },
    rebalanceRequired: s.rebalance.needed && rec.info.sharding.enabled,
    ledgerSummary: {
      verificationState: ledger.verificationState,
      lastVerifiedBlockHeight: Number.isFinite(lastVerified) ? lastVerified : null,
    },
  }
}

// --- roles and grants ----------------------------------------------------------------------------------

const COLUMN_DEFS: Record<string, { name: string; type: string; key?: boolean }[]> = {
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
}

/** Columns of a table with the reason a column cannot be the distribution column (or null). */
function columnsFor(rec: ReturnType<typeof db>, t: Table): S['TableColumn'][] {
  const defs = COLUMN_DEFS[t.name] ?? [
    { name: 'id', type: 'bigint', key: true },
    { name: 'created_at', type: 'timestamptz' },
  ]
  return defs.map((d) => {
    let reason: S['DistributionBlockedReason'] | null = null
    if (!rec.info.sharding.enabled) reason = 'shardingDisabled'
    else if (!t.classes.includes('plain')) reason = 'tableNotPlain'
    else if (['jsonb', 'json', 'bytea', 'xml'].includes(d.type)) reason = 'unsupportedType'
    else if (!d.key) reason = 'notInUniqueKey'
    return {
      name: d.name,
      type: d.type,
      nullable: !d.key,
      // The unique keys that lack the column (named in the panel's explanation).
      blockingKeys: reason === 'notInUniqueKey' ? [`${t.name}_pkey`] : [],
      canBeDistributionColumn: reason === null,
      distributionBlockedReason: reason,
    }
  })
}

function role(s: MockState, name: string): Role {
  const r = s.roles.find((x) => x.name === name)
  if (!r) throw problem('not_found', `Role "${name}" does not exist.`)
  return r
}

function adminCount(s: MockState, excluding?: string): number {
  return s.roles.filter(
    (r) => r.name !== excluding && r.canLogin && r.clusterRoles.includes('kingres_admin'),
  ).length
}

const TEMPLATE_PRIVS: Record<
  S['GrantTemplateRequest']['template'],
  { db: S['Privilege'][]; schema: S['Privilege'][]; table: S['Privilege'][] }
> = {
  readOnly: { db: ['connect'], schema: ['usage'], table: ['select'] },
  readWrite: {
    db: ['connect', 'temporary'],
    schema: ['usage'],
    table: ['select', 'insert', 'update', 'delete'],
  },
  owner: {
    db: ['connect', 'create', 'temporary'],
    schema: ['usage', 'create'],
    table: ['select', 'insert', 'update', 'delete', 'truncate', 'references', 'trigger'],
  },
}

function mergeGrant(s: MockState, g: S['Grant'], remove = false): S['Grant'] {
  const same = (x: S['Grant']) =>
    x.grantee === g.grantee &&
    x.level === g.level &&
    x.database === g.database &&
    x.schema === g.schema &&
    x.table === g.table
  const existing = s.grants.find(same)
  if (remove) {
    if (existing) {
      existing.privileges = existing.privileges.filter((p) => !g.privileges.includes(p))
      if (existing.privileges.length === 0) s.grants.splice(s.grants.indexOf(existing), 1)
    }
    return existing ?? { ...g, privileges: [] }
  }
  if (existing) {
    existing.privileges = [...new Set([...existing.privileges, ...g.privileges])]
    return existing
  }
  s.grants.push(g)
  return g
}

function grantFromRequest(s: MockState, r: S['GrantRequest']): S['Grant'] {
  role(s, r.grantee)
  db(s, r.database)
  const errors: { field: string; message: string }[] = []
  if (r.level !== 'database' && !r.schema)
    errors.push({ field: 'schema', message: 'A schema is required for schema and table grants.' })
  if (r.level === 'table' && !r.table)
    errors.push({ field: 'table', message: 'A table is required for table grants.' })
  if (!r.privileges?.length)
    errors.push({ field: 'privileges', message: 'Select at least one privilege.' })
  if (errors.length) throw validationFailed(errors)
  return {
    grantee: r.grantee,
    level: r.level,
    database: r.database,
    schema: r.level === 'database' ? null : (r.schema ?? null),
    table: r.level === 'table' ? (r.table ?? null) : null,
    privileges: r.privileges,
  }
}

// --- metrics, logs, query ------------------------------------------------------------------------------------

function metricValue(metric: S['MetricName'], serverId: number, t: number): number {
  const wave = (period: number, phase: number) =>
    (Math.sin((t / 1000 / period) * 2 * Math.PI + phase + serverId) + 1) / 2
  switch (metric) {
    case 'cpuUsagePercent':
      return 12 + serverId * 8 + wave(900, 0) * 25
    case 'memoryUsedBytes':
      return (5 + serverId + wave(3600, 1) * 1.5) * 1024 * MiB
    case 'diskUsedBytes':
      return (120 + serverId * 35 + ((t / 1000 / 86400) % 5)) * 1024 * MiB
    case 'connections':
      return 30 + serverId * 6 + wave(600, 2) * 20
    case 'replicationLagBytes':
      return 2048 * (serverId + wave(300, 3) * 6)
    case 'replicationLagSeconds':
      return (
        0.05 * serverId + wave(300, 3) * 0.4 + (serverId === 2 && t % 7200000 < 600000 ? 1.2 : 0)
      )
    case 'databaseSizeBytes':
      return (2300 + ((t / 1000 / 3600) % 40)) * MiB
    case 'transactionsPerSecond':
      return 500 + serverId * 60 + wave(450, 4) * 350
    case 'queriesPerSecond':
      return 1800 + serverId * 120 + wave(500, 5) * 900
    case 'bucketsUnderReplicated':
      return 0
  }
}

const METRIC_UNIT: Record<S['MetricName'], S['MetricQueryResult']['unit']> = {
  cpuUsagePercent: 'percent',
  memoryUsedBytes: 'bytes',
  diskUsedBytes: 'bytes',
  connections: 'count',
  replicationLagBytes: 'bytes',
  replicationLagSeconds: 'seconds',
  databaseSizeBytes: 'bytes',
  transactionsPerSecond: 'countPerSecond',
  queriesPerSecond: 'countPerSecond',
  bucketsUnderReplicated: 'count',
}

const LEVEL_ORDER = ['debug', 'info', 'warning', 'error']

function filterLogs(s: MockState, url: URL, serverId?: string) {
  const level = url.searchParams.get('level')
  const component = url.searchParams.get('component')
  const q = url.searchParams.get('q')?.toLowerCase()
  const sid = serverId ?? url.searchParams.get('serverId') ?? undefined
  const items = s.logs.filter(
    (l) =>
      (!level || LEVEL_ORDER.indexOf(l.level) >= LEVEL_ORDER.indexOf(level)) &&
      (!component || l.component === component) &&
      (!q || l.message.toLowerCase().includes(q)) &&
      (sid === undefined || l.serverId === sid),
  )
  return paginate(items, url)
}

const TABLES: Record<string, { columns: S['QueryColumn'][]; rows: unknown[][] }> = {
  orders: {
    columns: [
      { name: 'id', type: 'int8' },
      { name: 'customer_id', type: 'int8' },
      { name: 'total', type: 'numeric' },
      { name: 'created_at', type: 'timestamptz' },
    ],
    rows: Array.from({ length: 40 }, (_, i) => [
      String(9_000_000_000 + i),
      100 + (i % 7),
      (19.9 + i * 3.1).toFixed(2),
      new Date(Date.now() - i * 3_600_000).toISOString(),
    ]),
  },
  customers: {
    columns: [
      { name: 'customer_id', type: 'int8' },
      { name: 'name', type: 'text' },
      { name: 'country', type: 'text' },
    ],
    rows: Array.from({ length: 20 }, (_, i) => [
      100 + i,
      `Customer ${i + 1}`,
      ['DE', 'FR', 'NL', 'SE'][i % 4],
    ]),
  },
  countries: {
    columns: [
      { name: 'code', type: 'text' },
      { name: 'name', type: 'text' },
    ],
    rows: [
      ['DE', 'Germany'],
      ['FR', 'France'],
      ['NL', 'Netherlands'],
      ['SE', 'Sweden'],
    ],
  },
}

function runQuery(ctx: Ctx): S['QueryResult'] {
  const b = body<S['QueryRequest']>(ctx)
  db(ctx.s, b.database)
  const sql = b.sql.trim().replace(/;+\s*$/, '')
  const started = Date.now()
  if (sql.includes(';')) throw problem('bad_request', 'Only one statement per request is allowed.')
  const done = (columns: S['QueryColumn'][], rows: unknown[][], tag?: string): S['QueryResult'] => {
    const limit = b.rowLimit ?? 1000
    return {
      columns,
      rows: rows.slice(0, limit),
      rowCount: Math.min(rows.length, limit),
      truncated: rows.length > limit,
      commandTag: tag ?? `SELECT ${Math.min(rows.length, limit)}`,
      durationMs: Date.now() - started + 1.5,
    }
  }
  let m = /^select\s+1(\s+as\s+(\w+))?$/i.exec(sql)
  if (m) return done([{ name: m[2] ?? '?column?', type: 'int4' }], [[1]])
  if (/^select\s+version\(\)$/i.test(sql))
    return done([{ name: 'version', type: 'text' }], [['PostgreSQL 18.1 (KingresDB 0.1.0)']])
  if (/^select\s+now\(\)$/i.test(sql))
    return done([{ name: 'now', type: 'timestamptz' }], [[new Date().toISOString()]])
  m = /^select\s+(.+?)\s+from\s+([\w.]+)/i.exec(sql)
  if (m) {
    const table = (m[2] ?? '').split('.').pop() ?? ''
    const data = ctx.s.extraQueryTables?.[table] ?? TABLES[table]
    if (!data)
      throw problem('sql_error', `relation "${m[2]}" does not exist`, {
        sqlState: '42P01',
        sqlPosition: sql.toLowerCase().indexOf(m[2]?.toLowerCase() ?? '') + 1,
      })
    return done(data.columns, data.rows)
  }
  throw problem('sql_error', `syntax error at or near "${sql.split(/\s+/)[0] ?? ''}"`, {
    sqlState: '42601',
    sqlPosition: 1,
  })
}

// --- the table of handlers -----------------------------------------------------------------------------

export function createHandlers(): HttpHandler[] {
  const h: Partial<Record<OperationId, Handler>> = {
    getHealth: () => ok({ status: 'ok' }),
    getReadiness: (c) =>
      ok({
        status: 'ready',
        nodeState: c.s.node.state,
        checks: [
          { name: 'postgres', ok: true },
          { name: 'etcd', ok: true },
        ],
      }),
    login,
    logout: (c) => {
      c.s.session = null
      return noContent()
    },
    getSession: (c) => ok(sessionBody(c.s)),
    getNodeState: (c) =>
      ok({
        state: c.s.node.leftCluster ? 'standalone' : c.s.node.state,
        address: '10.0.0.11:8091',
        hostname: 'kdb-1.example.internal',
        version: '0.1.0',
        postgresVersion: '18.1',
        setupTokenRequired: c.s.node.state === 'uninitialized',
        localhostOnly: c.s.node.state === 'uninitialized',
        serverId: c.s.node.state === 'uninitialized' ? null : LOCAL,
        clusterId: c.s.node.state === 'uninitialized' ? null : c.s.clusterId,
        setupTask: setupTaskSummary(c.s),
      } satisfies S['NodeStateInfo']),
    checkSetupToken,
    initCluster,
    joinCluster,
    getDatabaseCreateOptions: (c) => ok(createOptions(c.s)),

    // --- this server
    getThisServer: (c) =>
      ok({
        serverId: c.s.node.leftCluster ? null : LOCAL,
        address: c.s.servers[0]?.address ?? '',
        hostname: c.s.servers[0]?.hostname ?? '',
        version: '0.1.0',
        postgresVersion: c.s.node.leftCluster ? null : '18.1',
        nodeState: c.s.node.leftCluster ? 'standalone' : 'clustered',
        clusterId: c.s.node.leftCluster ? null : c.s.clusterId,
        uptimeSeconds: Math.floor((Date.now() - (c.s.servers[0]?.startedAt ?? Date.now())) / 1000),
        isManagerLeader: true,
        services: (
          ['api', 'agent', 'manager', 'router', 'etcd', 'patroni', 'metrics'] as const
        ).map((name) => ({
          name,
          state:
            name === 'patroni' && c.s.servers[0]?.status === 'dead'
              ? ('stopped' as const)
              : ('running' as const),
          since: new Date(Date.now() - 86400_000 * 12).toISOString(),
        })),
      } satisfies S['LocalServerInfo']),
    getThisServerResources: (c) => {
      const r = serverView(c.s, c.s.servers[0] as never).resources
      return ok({
        cpu: { cores: 8, usagePercent: r.cpuUsagePercent, loadAverage1m: 1.7 },
        memory: {
          totalBytes: r.memoryTotalBytes,
          usedBytes: r.memoryUsedBytes,
          quotaBytes: 12 * 1024 * MiB,
        },
        disks: [
          {
            path: '/var/lib/kingresdb/data',
            purpose: 'data',
            totalBytes: r.diskTotalBytes,
            usedBytes: r.diskUsedBytes,
          },
          {
            path: '/var/lib/kingresdb/wal',
            purpose: 'wal',
            totalBytes: 100 * 1024 * MiB,
            usedBytes: 21 * 1024 * MiB,
          },
          {
            path: '/var/log/kingresdb',
            purpose: 'log',
            totalBytes: 50 * 1024 * MiB,
            usedBytes: 6 * 1024 * MiB,
          },
        ],
        sampledAt: nowIso(),
      } satisfies S['ServerResources'])
    },
    listThisServerInstances: (c) => ok({ items: instancesOfServer(c.s, LOCAL) }),
    getThisServerInstance: (c) => ok(thisServerInstance(c.s, num(c, 'instanceId'))),
    restartThisServerInstance: instanceAction('restart'),
    stopThisServerInstance: instanceAction('stop'),
    startThisServerInstance: instanceAction('start'),
    getThisServerSettings: (c) => ok(c.s.localSettings),
    updateThisServerSettings: (c) => {
      const b = body<S['LocalSettingsUpdate']>(c)
      const l = c.s.localSettings
      if (b.memoryQuota) {
        if (b.memoryQuota.totalMegabytes < 512)
          throw validationFailed([
            { field: 'memoryQuota.totalMegabytes', message: 'At least 512 MB.' },
          ])
        l.memoryQuota = b.memoryQuota
        l.restartRequired = true
      }
      if (b.logLevel) l.logLevel = b.logLevel
      if (b.logDirectory !== undefined)
        l.dataPaths = { ...l.dataPaths, logDirectory: b.logDirectory }
      return ok(l)
    },
    leaveCluster: (c) =>
      accepted(
        createTask(c.s, {
          kind: 'server.leave',
          subject: { type: 'server', id: '1' },
          steps: ['Drain local instances', 'Deregister from manager', 'Detach from cluster'],
          apply: () => {
            c.s.node.leftCluster = true
          },
        }),
      ),
    listThisServerLogs: (c) => ok(filterLogs(c.s, c.url, LOCAL)),

    // --- cluster and servers
    getCluster: (c) => ok(overview(c.s)),
    getClusterSummary: (c) => {
      const o = overview(c.s)
      return ok({
        clusterId: o.clusterId,
        name: o.name,
        health: o.health,
        serverCount: o.servers.total,
        databaseCount: o.databaseCount,
        openAlerts: o.openAlerts.critical + o.openAlerts.warning + o.openAlerts.info,
      } satisfies S['ClusterSummary'])
    },
    listServers: (c) => {
      const status = c.url.searchParams.get('status')
      const group = c.url.searchParams.get('serverGroup')
      return ok({
        items: c.s.servers
          .filter((x) => (!status || x.status === status) && (!group || x.serverGroup === group))
          .map((x) => serverView(c.s, x)),
      })
    },
    getServer: (c) => {
      const rec = server(c.s, c.params.serverId ?? '')
      const view = serverView(c.s, rec)
      const byDb = c.s.databases
        .filter((d) => d.info.sharding.enabled)
        .map((d) => {
          let pb = 0,
            rb = 0,
            ps = 0,
            rs = 0
          for (const b of [...d.buckets.values()].flat()) {
            const g = c.s.groups.find((x) => x.id === b.group)
            if (g?.primaryServerId === rec.id) {
              pb += 1
              ps += b.size ?? 0
            }
            if (g?.standbyServerIds.includes(rec.id)) {
              rb += 1
              rs += b.size ?? 0
            }
          }
          return {
            databaseName: d.info.name,
            primaryBuckets: pb,
            replicaBuckets: rb,
            primarySizeBytes: ps,
            replicaSizeBytes: rs,
          }
        })
      return ok({ ...view, bucketsByDatabase: byDb } satisfies S['ServerDetail'])
    },
    addServer: (c) => {
      const b = body<S['AddServerRequest']>(c)
      const s = c.s
      if (!validAddress(b.address ?? ''))
        throw validationFailed([
          { field: 'address', message: 'Use host:port, for example 10.0.0.14:8091.' },
        ])
      if (b.setupToken !== MOCK_SETUP_TOKEN) throw problem('setup_token_invalid')
      if (s.servers.some((x) => x.address === b.address))
        throw problem(
          'already_exists',
          `A server with address ${b.address} is already in the cluster.`,
        )
      if (b.address.includes('unreachable'))
        throw problem('unprocessable', `Cannot reach ${b.address}.`)
      requireNoRebalance(s)
      // The server id is the host of its address (IPv6 without brackets).
      const id = b.address.replace(/:\d+$/, '').replace(/^\[|\]$/g, '')
      return accepted(
        createTask(s, {
          kind: 'server.add',
          subject: { type: 'server', id: String(id) },
          steps: [
            'Contact server',
            'Issue certificates',
            'Create shard group primary',
            'Create standby',
            'Register',
          ],
          apply: () => {
            s.servers.push({
              id,
              address: b.address,
              hostname: id,
              status: 'pendingRebalance',
              serverGroup: b.serverGroup ?? null,
              shouldHaveShards: true,
              version: '0.1.0',
              joinedAt: nowIso(),
              startedAt: Date.now(),
              telemetry: true,
            })
            const gid = Math.max(...s.groups.map((g) => g.id)) + 1
            s.groups.push({
              id: gid,
              isMapGroup: false,
              preferredPrimaryServerId: id,
              primaryServerId: id,
              standbyServerIds: [],
              failedServerIds: [],
            })
            fixReplicas(s)
            markRebalance(s, 'serverAdded')
          },
        }),
      )
    },
    updateServer: (c) => {
      const rec = server(c.s, c.params.serverId ?? '')
      const b = body<S['ServerUpdateRequest']>(c)
      if (b.serverGroup !== undefined) {
        if (b.serverGroup !== null && !c.s.serverGroups.some((g) => g.name === b.serverGroup))
          throw problem('not_found', `Server group "${b.serverGroup}" does not exist.`)
        rec.serverGroup = b.serverGroup
        markRebalance(c.s, 'serverGroupChanged')
      }
      if (b.shouldHaveShards !== undefined) rec.shouldHaveShards = b.shouldHaveShards
      return ok(serverView(c.s, rec))
    },
    removeServer: (c) => {
      const s = c.s
      const rec = server(s, c.params.serverId ?? '')
      if (s.servers.length <= 1)
        throw problem(
          'unprocessable',
          'The last server cannot be removed. Use "leave cluster" on it.',
        )
      requireNoRebalance(s)
      if (activeTask(s, 'server.remove', String(rec.id))) throw problem('operation_in_progress')
      rec.status = 'draining'
      return accepted(
        createTask(s, {
          kind: 'server.remove',
          subject: { type: 'server', id: String(rec.id) },
          steps: ['Mark as draining', 'Move buckets away', 'Remove instances', 'Deregister'],
          apply: () => {
            const rest = dataGroups(s).filter((g) => g.preferredPrimaryServerId !== rec.id)
            const gone = s.groups.find(
              (g) => !g.isMapGroup && g.preferredPrimaryServerId === rec.id,
            )
            let i = 0
            for (const d of s.databases)
              for (const bs of d.buckets.values())
                for (const b of bs)
                  if (gone && b.group === gone.id)
                    b.group = (rest[i++ % rest.length] as { id: number }).id
            s.groups = s.groups.filter((g) => g !== gone)
            s.servers = s.servers.filter((x) => x !== rec)
            for (const g of s.groups) {
              if (g.primaryServerId === rec.id)
                g.primaryServerId = g.standbyServerIds.shift() ?? null
              g.standbyServerIds = g.standbyServerIds.filter((x) => x !== rec.id)
              if (g.isMapGroup && g.preferredPrimaryServerId === rec.id)
                g.preferredPrimaryServerId = g.primaryServerId ?? LOCAL
            }
            fixReplicas(s)
          },
        }),
      )
    },
    failoverServer: (c) => {
      const s = c.s
      const rec = server(s, c.params.serverId ?? '')
      const { mode } = body<S['FailoverRequest']>(c)
      requireNoRebalance(s)
      const affected = s.groups.filter((g) => g.primaryServerId === rec.id)
      if (mode === 'graceful' && affected.some((g) => g.standbyServerIds.length === 0))
        throw problem(
          'no_replica_available',
          'A group on this server has no healthy standby to promote.',
        )
      if (activeTask(s, 'server.failover', String(rec.id))) throw problem('operation_in_progress')
      return accepted(
        createTask(s, {
          kind: 'server.failover',
          subject: { type: 'server', id: String(rec.id) },
          steps:
            mode === 'graceful'
              ? [
                  'Pause writes',
                  'Wait for standby to catch up',
                  'Promote standby',
                  'Demote old primary',
                ]
              : ['Promote standbys', 'Update routing'],
          durationMs: 3000,
          apply: () => {
            for (const g of s.groups) {
              if (g.primaryServerId === rec.id) {
                const next = g.standbyServerIds.shift() ?? null
                if (mode === 'graceful' && next !== null) g.standbyServerIds.push(rec.id)
                else g.failedServerIds.push(rec.id)
                g.primaryServerId = next
              } else if (mode === 'hard' && g.standbyServerIds.includes(rec.id)) {
                g.standbyServerIds = g.standbyServerIds.filter((x) => x !== rec.id)
                g.failedServerIds.push(rec.id)
              }
            }
            if (mode === 'hard') {
              rec.status = 'dead'
              s.settings.autoFailover = {
                ...s.settings.autoFailover,
                eventsSinceReset: s.settings.autoFailover.eventsSinceReset + 1,
              }
              s.alerts.unshift({
                id: Math.max(...s.alerts.map((a) => a.id)) + 1,
                kind: 'serverDown',
                severity: 'critical',
                state: 'open',
                title: `Server ${rec.id} was failed over`,
                message: `Standbys of ${affected.length} shard group(s) were promoted. Restore preferred primaries after the server returns.`,
                serverId: rec.id,
                databaseName: null,
                raisedAt: nowIso(),
              })
            }
          },
        }),
      )
    },
    listServerGroups: (c) =>
      ok({
        items: c.s.serverGroups.map((g) => ({
          ...g,
          serverIds: c.s.servers.filter((x) => x.serverGroup === g.name).map((x) => x.id),
        })),
      }),
    createServerGroup: (c) => {
      const b = body<S['ServerGroupCreateRequest']>(c)
      if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$/.test(b.name ?? ''))
        throw validationFailed([{ field: 'name', message: 'Invalid name.' }])
      if (c.s.serverGroups.some((g) => g.name === b.name)) throw problem('already_exists')
      c.s.serverGroups.push({ name: b.name, description: b.description ?? '' })
      return created({ name: b.name, description: b.description, serverIds: [] })
    },
    deleteServerGroup: (c) => {
      const name = c.params.serverGroupName ?? ''
      if (c.s.servers.some((x) => x.serverGroup === name))
        throw problem('conflict', 'The server group still has servers.')
      c.s.serverGroups = c.s.serverGroups.filter((g) => g.name !== name)
      return noContent()
    },

    // --- databases
    listDatabases: (c) => ok({ items: c.s.databases.map((d) => databaseView(c.s, d.info.name)) }),
    getDatabase: (c) => ok(databaseView(c.s, c.params.databaseName ?? '')),
    createDatabase: (c) => {
      const s = c.s
      const b = body<S['DatabaseCreateRequest']>(c)
      validateDatabase(s, b)
      if (s.databases.some((d) => d.info.name === b.name))
        throw problem('already_exists', `Database "${b.name}" already exists.`)
      if (b.sharding?.enabled && !s.settings.features.sharding)
        throw problem(
          'unprocessable',
          'Sharding is not enabled for this cluster. Enable it in the cluster settings first.',
        )
      if (b.ledger?.mode && b.ledger.mode !== 'off' && !s.settings.features.ledger)
        throw problem(
          'unprocessable',
          'The ledger is not enabled for this cluster. Enable it in the cluster settings first.',
        )
      return accepted(
        createTask(s, {
          kind: 'database.create',
          subject: { type: 'database', id: b.name },
          steps: [
            'Create database on every group',
            'Install extensions',
            'Record settings',
            'Apply durability',
          ],
          failWith:
            b.name === 'failme'
              ? 'Could not create the database on shard group 2: out of disk space.'
              : undefined,
          apply: () => {
            const info = newDatabase(s, b)
            const rec = { info, tables: [], colocations: [], buckets: new Map() } as ReturnType<
              typeof db
            >
            s.databases.push(rec)
          },
        }),
      )
    },
    updateDatabase: (c) => {
      const s = c.s
      const rec = db(s, c.params.databaseName ?? '')
      const b = body<S['DatabaseUpdateRequest']>(c)
      const info = rec.info
      const errors: { field: string; message: string }[] = []
      if (b.general?.connectionLimit !== undefined && b.general.connectionLimit < -1)
        errors.push({
          field: 'general.connectionLimit',
          message: 'Use -1 for unlimited or a positive number.',
        })
      if (b.general?.owner && !s.roles.some((r) => r.name === b.general?.owner))
        errors.push({
          field: 'general.owner',
          message: `Role "${b.general.owner}" does not exist.`,
        })
      if (
        b.ledger?.digestIntervalSeconds !== undefined &&
        (b.ledger.digestIntervalSeconds < 60 || b.ledger.digestIntervalSeconds > 86400)
      )
        errors.push({
          field: 'ledger.digestIntervalSeconds',
          message: 'Between 60 and 86400 seconds.',
        })
      if (errors.length) throw validationFailed(errors)
      if (b.ledger?.mode === 'off' && info.ledger.mode !== 'off')
        throw problem('immutable_field', 'The ledger cannot be turned off once it is on.')
      const needsTask =
        (b.sharding?.enabled === true && !info.sharding.enabled) ||
        (b.ledger?.mode && b.ledger.mode !== 'off' && info.ledger.mode === 'off')
      if (needsTask && b.sharding?.enabled && !s.settings.features.sharding)
        throw problem('unprocessable', 'Sharding is not enabled for this cluster.')
      const applyNow = () => {
        if (b.general?.owner) info.general.owner = b.general.owner
        if (b.general?.connectionLimit !== undefined)
          info.general.connectionLimit = b.general.connectionLimit
        if (b.replication?.durability) info.replication.durability = b.replication.durability
        if (b.ledger?.mode) info.ledger.mode = b.ledger.mode
        if (b.ledger?.digestIntervalSeconds !== undefined)
          info.ledger.digestIntervalSeconds = b.ledger.digestIntervalSeconds
        if (b.sharding?.enabled) info.sharding.enabled = true
      }
      if (!needsTask) {
        applyNow()
        return ok(databaseView(s, info.name))
      }
      return accepted(
        createTask(s, {
          kind: 'database.update',
          subject: { type: 'database', id: info.name },
          steps: ['Prepare extensions', 'Apply settings', 'Backfill metadata'],
          apply: applyNow,
        }),
      )
    },
    dropDatabase: (c) => {
      const s = c.s
      const name = c.params.databaseName ?? ''
      const rec = db(s, name)
      if (c.url.searchParams.get('confirm') !== name)
        throw validationFailed([
          { field: 'confirm', message: 'Repeat the database name to confirm.' },
        ])
      return accepted(
        createTask(s, {
          kind: 'database.drop',
          subject: { type: 'database', id: name },
          steps: ['Close connections', 'Drop on every group', 'Remove registry entry'],
          apply: () => {
            s.databases = s.databases.filter((d) => d !== rec)
          },
        }),
      )
    },
    listTables: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const schema = c.url.searchParams.get('schema')
      const cls = c.url.searchParams.get('class')
      return ok(
        paginate(
          rec.tables.filter(
            (t) =>
              (!schema || t.schema === schema) &&
              (!cls || t.classes.includes(cls as Table['classes'][number])),
          ),
          c.url,
        ),
      )
    },
    getTable: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const t = rec.tables.find(
        (x) => x.schema === c.params.schemaName && x.name === c.params.tableName,
      )
      if (!t) throw problem('not_found', 'Table does not exist.')
      return ok({
        ...t,
        columns: rec.extraColumns?.[t.name] ?? columnsFor(rec, t),
      } satisfies S['TableDetail'])
    },
    distributeTable: (c) => {
      const s = c.s
      const rec = db(s, c.params.databaseName ?? '')
      const t = rec.tables.find(
        (x) => x.schema === c.params.schemaName && x.name === c.params.tableName,
      )
      if (!t) throw problem('not_found', 'Table does not exist.')
      const b = body<S['DistributeTableRequest']>(c)
      if (!rec.info.sharding.enabled)
        throw problem('unprocessable', 'Sharding is not enabled for this database.')
      if (!t.classes.includes('plain'))
        throw problem('conflict', 'The table is already distributed or a reference table.')
      if (b.method === 'hash' && !b.distributionColumn)
        throw validationFailed([
          { field: 'distributionColumn', message: 'Choose the column to distribute by.' },
        ])
      if (b.method === 'hash') {
        const col = columnsFor(rec, t).find((x) => x.name === b.distributionColumn)
        if (!col)
          throw validationFailed([
            {
              field: 'distributionColumn',
              message: `The table has no column "${b.distributionColumn}".`,
            },
          ])
        if (!col.canBeDistributionColumn)
          throw problem(
            'unprocessable',
            `Column ${col.name} cannot be the distribution column (${col.distributionBlockedReason}).`,
          )
      }
      const target = b.colocateWith
        ? rec.tables.find(
            (x) => x.schema === b.colocateWith?.schema && x.name === b.colocateWith.table,
          )
        : undefined
      if (b.colocateWith && !target?.distribution)
        throw problem('unprocessable', 'The table to colocate with is not distributed.')
      return accepted(
        createTask(s, {
          kind: 'table.distribute',
          subject: { type: 'table', id: `${rec.info.name}.${t.schema}.${t.name}` },
          steps: ['Create shards', 'Copy rows', 'Switch over', 'Clean up'],
          apply: () => {
            const bucketCount = rec.info.sharding.bucketCount ?? 128
            if (b.method === 'reference') {
              t.classes = ['reference']
              t.distribution = {
                method: 'reference',
                column: null,
                colocationId: 3,
                bucketCount: 1,
              }
            } else {
              const colocationId =
                target?.distribution?.colocationId ??
                Math.max(
                  0,
                  ...rec.tables.map((x) => x.distribution?.colocationId ?? 0),
                  ...rec.colocations.map((x) => x.colocationId),
                ) + 1
              t.classes = t.ledger ? ['distributed', 'ledger'] : ['distributed']
              t.distribution = {
                method: 'hash',
                column: b.distributionColumn ?? null,
                colocationId,
                bucketCount,
              }
              const existing = rec.colocations.find((x) => x.colocationId === colocationId)
              const group = existing ?? {
                colocationId,
                bucketCount,
                distributionColumnType: 'bigint',
                tableCount: 0,
                sizeBytes: 0,
              }
              if (!existing) {
                rec.colocations.push(group)
                rec.buckets.set(
                  colocationId,
                  makeBuckets(
                    colocationId,
                    bucketCount,
                    dataGroups(s).map((g) => g.id),
                    colocationId * 7,
                  ),
                )
              }
              group.tableCount += 1
            }
            t.homeShardGroupId = null
          },
        }),
      )
    },
    enableTableLedger: (c) => {
      const s = c.s
      const rec = db(s, c.params.databaseName ?? '')
      const t = rec.tables.find(
        (x) => x.schema === c.params.schemaName && x.name === c.params.tableName,
      )
      if (!t) throw problem('not_found', 'Table does not exist.')
      const { mode } = body<S['EnableTableLedgerRequest']>(c)
      if (mode !== 'appendOnly' && mode !== 'updatable')
        throw validationFailed([{ field: 'mode', message: 'Use appendOnly or updatable.' }])
      if (!s.settings.features.ledger)
        throw problem('unprocessable', 'The ledger is not enabled for this cluster.')
      if (t.ledger)
        throw problem('conflict', 'The table already has a ledger. It cannot be changed.')
      t.ledger = { mode, historyTable: mode === 'updatable' ? `${t.name}_history` : null }
      // `plain` never combines with another class.
      t.classes = t.classes.includes('plain') ? ['ledger'] : [...t.classes, 'ledger']
      if (rec.info.ledger.mode === 'off' || rec.info.ledger.mode === null)
        rec.info.ledger.mode = 'optIn'
      return ok(t)
    },
    listColocationGroups: (c) => ok({ items: db(c.s, c.params.databaseName ?? '').colocations }),
    listBuckets: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const buckets = rec.buckets.get(num(c, 'colocationId'))
      if (!buckets) throw problem('not_found', 'Colocation group does not exist.')
      const sg = c.url.searchParams.get('shardGroupId')
      const srv = c.url.searchParams.get('serverId')
      const st = c.url.searchParams.get('state')
      const items = buckets
        .map((b) => bucketView(c.s, b))
        .filter(
          (b) =>
            (!sg || b.shardGroupId === Number(sg)) &&
            (!srv || b.primaryServerId === srv || b.replicaServerIds.includes(srv)) &&
            (!st || b.state === st),
        )
      return ok({ mapVersion: c.s.mapVersion, ...paginate(items, c.url) })
    },
    getBucketDistribution: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const only = c.url.searchParams.get('colocationId')
      const sets = [...rec.buckets.entries()]
        .filter(([id]) => !only || id === Number(only))
        .flatMap(([, v]) => v)
      return ok({
        mapVersion: c.s.mapVersion,
        servers: c.s.servers.map((srv) => {
          let pb = 0,
            rb = 0,
            ps = 0,
            rs = 0
          for (const b of sets) {
            const g = c.s.groups.find((x) => x.id === b.group)
            if (g?.primaryServerId === srv.id) {
              pb += 1
              ps += b.size ?? 0
            }
            if (g?.standbyServerIds.includes(srv.id)) {
              rb += 1
              rs += b.size ?? 0
            }
          }
          return {
            serverId: srv.id,
            primaryBuckets: pb,
            replicaBuckets: rb,
            primarySizeBytes: ps,
            replicaSizeBytes: rs,
          }
        }),
      } satisfies S['BucketDistribution'])
    },

    // --- replication and rebalance
    getReplicationTopology: (c) => ok(topology(c.s)),
    restorePreferredPrimaries: (c) => {
      const s = c.s
      requireNoRebalance(s)
      if (activeTask(s, 'replication.restorePreferredPrimaries'))
        throw problem('operation_in_progress')
      return accepted(
        createTask(s, {
          kind: 'replication.restorePreferredPrimaries',
          subject: { type: 'replication', id: 'cluster' },
          steps: ['Rebuild standbys', 'Switch primaries back'],
          apply: () => {
            for (const x of s.servers)
              if (x.status === 'dead' || x.status === 'suspect') x.status = 'live'
            for (const g of s.groups) {
              g.primaryServerId = g.preferredPrimaryServerId
            }
            fixReplicas(s)
            s.settings.autoFailover = { ...s.settings.autoFailover, eventsSinceReset: 0 }
          },
        }),
      )
    },
    getRebalanceStatus: (c) => {
      const s = c.s
      const r = s.rebalance
      const rec = r.taskId ? requireTask(s, r.taskId) : undefined
      const total = r.moves.length
      const percent = rec?.task.progress.percent ?? 0
      const done =
        r.state === 'running' || r.state === 'stopped' ? Math.floor((percent / 100) * total) : 0
      const inFlight = r.state === 'running' ? r.moves.slice(done, done + 4) : []
      const phases = ['prepare', 'copy', 'catchUp', 'switch', 'cleanup'] as const
      return ok({
        state: r.state,
        needed: r.needed,
        reasons: r.reasons,
        taskId: r.taskId,
        startedAt: r.startedAt,
        options: r.options,
        progress:
          r.state === 'idle'
            ? null
            : {
                movesTotal: total,
                movesDone: done,
                movesInFlight: inFlight.length,
                movesFailed: 0,
                bytesMoved: r.moves
                  .slice(0, done)
                  .reduce((n, m) => n + (m.estimatedSizeBytes ?? 0), 0),
                percent,
              },
        inFlightMoves: inFlight.map((m, i) => ({
          databaseName: m.databaseName,
          colocationId: m.colocationId,
          bucket: m.bucket,
          fromShardGroupId: m.fromShardGroupId,
          toShardGroupId: m.toShardGroupId,
          phase: phases[(done + i) % phases.length] as (typeof phases)[number],
          percent: Math.min(99, (percent * 7 + i * 23) % 100),
          sizeBytes: m.estimatedSizeBytes,
        })),
        lastReport: lastReportOf(s),
      } satisfies S['RebalanceStatus'])
    },
    previewRebalancePlan: (c) => {
      const b = body<S['RebalancePlanRequest']>(c)
      return ok(planView(c.s, b.moveLimit ?? 200, b.databases))
    },
    startRebalance: (c) => {
      const s = c.s
      requireNoRebalance(s)
      const opts = body<S['RebalanceOptions']>(c)
      const moves = planMoves(s, opts.databases)
      if (moves.length === 0) {
        // Nothing to move: finish quickly but still do the replica and status bookkeeping.
        s.rebalance.needed = false
      }
      const total = moves.length
      const r = s.rebalance
      r.state = 'running'
      r.moves = moves
      r.options = opts
      r.startedAt = nowIso()
      const startedAt = Date.now()
      const task = createTask(s, {
        kind: 'rebalance',
        subject: { type: 'rebalance', id: 'cluster' },
        steps: ['Plan moves', 'Move buckets', 'Verify placement', 'Finish'],
        durationMs: Math.max(5000, total * 70),
        cancellable: true,
        onProgress: (f) => {
          const done = Math.floor(f * total)
          applyMoveProgress(s, moves, done, Math.min(4, total - done))
        },
        apply: () => {
          applyMoveProgress(s, moves, total, 0)
          fixReplicas(s)
          for (const x of s.servers) if (x.status === 'pendingRebalance') x.status = 'live'
          s.rebalance.state = 'idle'
          s.rebalance.needed = false
          s.rebalance.reasons = []
          s.settings.autoFailover = { ...s.settings.autoFailover, eventsSinceReset: 0 }
          const finished = nowIso()
          const bytes = moves.reduce((n, m) => n + (m.estimatedSizeBytes ?? 0), 0)
          s.rebalance.reports.unshift({
            id: (s.rebalance.reports[0]?.id ?? 0) + 1,
            outcome: 'completed',
            startedAt: new Date(startedAt).toISOString(),
            finishedAt: finished,
            options: opts,
            totals: {
              movesTotal: total,
              movesDone: total,
              movesFailed: 0,
              bytesMoved: bytes,
              durationSeconds: Math.round((Date.now() - startedAt) / 1000),
            },
            moves: moves.slice(0, 50).map((m) => ({
              databaseName: m.databaseName,
              colocationId: m.colocationId,
              bucket: m.bucket,
              fromShardGroupId: m.fromShardGroupId,
              toShardGroupId: m.toShardGroupId,
              ...(m.estimatedSizeBytes === null ? {} : { sizeBytes: m.estimatedSizeBytes }),
              outcome: 'done' as const,
              durationSeconds: 3,
            })),
          })
        },
      })
      r.taskId = task.id
      return accepted(task)
    },
    stopRebalance: (c) => {
      const s = c.s
      if (s.rebalance.state !== 'running')
        throw problem('node_state_invalid', 'No rebalance is running.')
      const rec = s.rebalance.taskId ? requireTask(s, s.rebalance.taskId) : undefined
      if (rec) {
        rec.task.state = 'paused'
        rec.task.cancellable = false
      }
      s.rebalance.state = 'stopped'
      for (const d of s.databases)
        for (const bs of d.buckets.values())
          for (const b of bs)
            if (b.state === 'moving') {
              b.state = 'active'
              b.movingTo = null
            }
      return ok({ state: 'stopped', needed: true, reasons: s.rebalance.reasons })
    },
    listRebalanceReports: (c) =>
      ok(
        paginate(
          c.s.rebalance.reports.map(({ moves: _moves, ...summary }) => summary),
          c.url,
        ),
      ),
    getRebalanceReport: (c) => {
      const r = c.s.rebalance.reports.find((x) => x.id === num(c, 'reportId'))
      if (!r) throw problem('not_found')
      return ok(r)
    },

    // --- tasks
    listTasks: (c) => {
      const state = c.url.searchParams.get('state')
      const kind = c.url.searchParams.get('kind')
      return ok(
        paginate(
          c.s.tasks
            .filter(
              (t) =>
                !t.setup && (!state || t.task.state === state) && (!kind || t.task.kind === kind),
            )
            .map((t) => t.task),
          c.url,
        ),
      )
    },
    getTask: (c) => ok(requireTask(c.s, num(c, 'taskId')).task),
    cancelTask: (c) => {
      const rec = requireTask(c.s, num(c, 'taskId'))
      if (!rec.task.cancellable || rec.task.state !== 'running')
        throw problem('task_not_cancellable')
      rec.task.state = 'paused'
      rec.task.cancellable = false
      if (rec.task.kind === 'rebalance') c.s.rebalance.state = 'stopped'
      return ok(rec.task)
    },

    // --- security
    listClusterRoles: () =>
      ok({
        items: [
          { name: 'kingres_admin', description: 'Everything, including users and security.' },
          {
            name: 'kingres_ops',
            description: 'Servers, rebalance and failover. No user management.',
          },
          { name: 'kingres_readonly', description: 'View everything except security.' },
          { name: 'kingres_stats', description: 'Metrics only.' },
        ],
      }),
    listRoles: (c) => {
      const search = c.url.searchParams.get('search')?.toLowerCase()
      const login = c.url.searchParams.get('canLogin')
      return ok(
        paginate(
          c.s.roles.filter(
            (r) =>
              (!search || r.name.includes(search)) &&
              (login === null || String(r.canLogin) === login),
          ),
          c.url,
        ),
      )
    },
    createRole: (c) => {
      const b = body<S['RoleCreateRequest']>(c)
      const errors: { field: string; message: string }[] = []
      if (!NAME_RE.test(b.name ?? ''))
        errors.push({
          field: 'name',
          message: 'Use lower-case letters, digits and "_", starting with a letter or "_".',
        })
      if (b.password !== undefined && b.password.length < 8)
        errors.push({ field: 'password', message: 'The password must have at least 8 characters.' })
      if (errors.length) throw validationFailed(errors)
      if (c.s.roles.some((r) => r.name === b.name))
        throw problem('already_exists', `Role "${b.name}" already exists.`)
      const r: Role = {
        name: b.name,
        canLogin: b.canLogin ?? true,
        isSuperuser: false,
        canCreateDatabase: b.canCreateDatabase ?? false,
        canCreateRole: b.canCreateRole ?? false,
        connectionLimit: b.connectionLimit ?? -1,
        validUntil: b.validUntil ?? null,
        memberOf: b.memberOf ?? [],
        clusterRoles: b.clusterRoles ?? [],
      }
      c.s.roles.push(r)
      if (b.password) c.s.passwords.set(b.name, b.password)
      return created(r)
    },
    getRole: (c) => ok(role(c.s, c.params.roleName ?? '')),
    updateRole: (c) => {
      const r = role(c.s, c.params.roleName ?? '')
      const b = body<S['RoleUpdateRequest']>(c)
      if (b.password !== undefined) {
        if (b.password.length < 8)
          throw validationFailed([
            { field: 'password', message: 'The password must have at least 8 characters.' },
          ])
        c.s.passwords.set(r.name, b.password)
      }
      if (
        b.canLogin === false &&
        r.clusterRoles.includes('kingres_admin') &&
        adminCount(c.s, r.name) === 0
      )
        throw problem('last_admin')
      if (b.canLogin !== undefined) r.canLogin = b.canLogin
      if (b.canCreateDatabase !== undefined) r.canCreateDatabase = b.canCreateDatabase
      if (b.canCreateRole !== undefined) r.canCreateRole = b.canCreateRole
      if (b.connectionLimit !== undefined) r.connectionLimit = b.connectionLimit
      if (b.validUntil !== undefined) r.validUntil = b.validUntil
      return ok(r)
    },
    deleteRole: (c) => {
      const r = role(c.s, c.params.roleName ?? '')
      if (r.clusterRoles.includes('kingres_admin') && adminCount(c.s, r.name) === 0)
        throw problem('last_admin')
      if (c.s.databases.some((d) => d.info.general.owner === r.name))
        throw problem('role_in_use', `Role "${r.name}" owns databases.`)
      c.s.roles = c.s.roles.filter((x) => x !== r)
      c.s.grants = c.s.grants.filter((g) => g.grantee !== r.name)
      return noContent()
    },
    setRoleMemberships: (c) => {
      const r = role(c.s, c.params.roleName ?? '')
      const { memberOf } = body<S['RoleMembershipsRequest']>(c)
      for (const m of memberOf) role(c.s, m)
      r.memberOf = memberOf
      return ok(r)
    },
    setRoleClusterRoles: (c) => {
      const r = role(c.s, c.params.roleName ?? '')
      const { clusterRoles } = body<S['RoleClusterRolesRequest']>(c)
      if (
        r.clusterRoles.includes('kingres_admin') &&
        !clusterRoles.includes('kingres_admin') &&
        adminCount(c.s, r.name) === 0
      )
        throw problem('last_admin')
      r.clusterRoles = clusterRoles
      return ok(r)
    },
    listGrants: (c) => {
      const q = (k: string) => c.url.searchParams.get(k)
      return ok(
        paginate(
          c.s.grants.filter(
            (g) =>
              (!q('grantee') || g.grantee === q('grantee')) &&
              (!q('database') || g.database === q('database')) &&
              (!q('level') || g.level === q('level')),
          ),
          c.url,
        ),
      )
    },
    createGrant: (c) => ok(mergeGrant(c.s, grantFromRequest(c.s, body<S['GrantRequest']>(c)))),
    revokeGrants: (c) => {
      mergeGrant(c.s, grantFromRequest(c.s, body<S['GrantRequest']>(c)), true)
      return noContent()
    },
    applyGrantTemplate: (c) => {
      const b = body<S['GrantTemplateRequest']>(c)
      role(c.s, b.grantee)
      const rec = db(c.s, b.database)
      const p = TEMPLATE_PRIVS[b.template]
      mergeGrant(c.s, {
        grantee: b.grantee,
        level: 'database',
        database: b.database,
        schema: null,
        table: null,
        privileges: p.db,
      })
      mergeGrant(c.s, {
        grantee: b.grantee,
        level: 'schema',
        database: b.database,
        schema: 'public',
        table: null,
        privileges: p.schema,
      })
      for (const t of rec.tables)
        mergeGrant(c.s, {
          grantee: b.grantee,
          level: 'table',
          database: b.database,
          schema: t.schema,
          table: t.name,
          privileges: p.table,
        })
      return noContent()
    },
    getNetworkAccess: (c) => ok(c.s.network),
    replaceNetworkAccess: (c) => {
      const s = c.s
      const b = body<S['NetworkAccessUpdateRequest']>(c)
      if (b.revision !== s.network.revision)
        throw problem(
          'optimistic_lock_failed',
          'The rules were changed by someone else. Reload and apply your change again.',
        )
      if (b.tlsRequired && b.rules.some((r) => r.type === 'host'))
        throw problem(
          'unprocessable',
          'TLS is required, so plain "host" rules are not allowed. Use "hostssl".',
        )
      const errors = b.rules.flatMap((r, i) =>
        r.address && /^[0-9a-fA-F:.]+\/\d{1,3}$|^all$/.test(r.address)
          ? []
          : [
              {
                field: `rules[${i}].address`,
                message: 'Use CIDR notation such as 10.0.0.0/24, or "all".',
              },
            ],
      )
      if (errors.length) throw validationFailed(errors)
      return accepted(
        createTask(s, {
          kind: 'networkAccess.apply',
          subject: { type: 'networkAccess', id: 'cluster' },
          steps: s.servers.map((x) => `Apply on server ${x.id}`),
          apply: () => {
            const revision = s.network.revision + 1
            s.network = {
              revision,
              tlsRequired: b.tlsRequired,
              rules: b.rules,
              applyStatus: s.servers.map((x) => ({
                serverId: x.id,
                appliedRevision: revision,
                error: null,
              })),
            }
          },
        }),
      )
    },
    listAuditEvents: (c) => {
      const q = (k: string) => c.url.searchParams.get(k)
      return ok(
        paginate(
          c.s.audit.filter(
            (e) =>
              (!q('actor') || e.actor === q('actor')) &&
              (!q('action') || e.action.includes(q('action') ?? '')) &&
              (!q('outcome') || e.outcome === q('outcome')),
          ),
          c.url,
        ),
      )
    },

    // --- settings
    getClusterSettings: (c) => ok(c.s.settings),
    updateClusterSettings: (c) => {
      const s = c.s
      const b = body<S['ClusterSettingsUpdateRequest']>(c)
      const errors: { field: string; message: string }[] = []
      if (Object.keys(b).length === 0)
        throw validationFailed([{ field: '/', message: 'at least one property is required' }])
      for (const group of ['autoFailover', 'rebalance', 'features'] as const) {
        const g = b[group]
        if (g !== undefined && Object.keys(g).length === 0)
          errors.push({ field: `/${group}`, message: 'at least one property is required' })
      }
      if (
        b.defaultReplicaCount !== undefined &&
        (b.defaultReplicaCount < 0 || b.defaultReplicaCount > 3)
      )
        errors.push({ field: 'defaultReplicaCount', message: 'Between 0 and 3.' })
      if (
        b.autoFailover?.timeoutSeconds !== undefined &&
        (b.autoFailover.timeoutSeconds < 1 || b.autoFailover.timeoutSeconds > 3600)
      )
        errors.push({
          field: 'autoFailover.timeoutSeconds',
          message: 'Between 1 and 3600 seconds.',
        })
      if (
        b.autoFailover?.maxCount !== undefined &&
        (b.autoFailover.maxCount < 1 || b.autoFailover.maxCount > 100)
      )
        errors.push({ field: 'autoFailover.maxCount', message: 'Between 1 and 100.' })
      if (
        b.rebalance?.maxConcurrentMoves !== undefined &&
        (b.rebalance.maxConcurrentMoves < 1 || b.rebalance.maxConcurrentMoves > 64)
      )
        errors.push({ field: 'rebalance.maxConcurrentMoves', message: 'Between 1 and 64.' })
      if (errors.length) throw validationFailed(errors)
      if (
        (b.features?.sharding === false && s.settings.features.sharding) ||
        (b.features?.ledger === false && s.settings.features.ledger)
      )
        throw problem('immutable_field', 'A feature cannot be turned off once it is on.')
      if (b.defaultReplicaCount !== undefined && b.defaultReplicaCount > s.servers.length - 1)
        throw problem(
          'unprocessable',
          `A replica count of ${b.defaultReplicaCount} needs at least ${b.defaultReplicaCount + 1} servers.`,
        )
      const cur = s.settings
      if (
        b.defaultReplicaCount !== undefined &&
        b.defaultReplicaCount !== cur.defaultReplicaCount
      ) {
        cur.defaultReplicaCount = b.defaultReplicaCount
        markRebalance(s, 'replicaCountChanged')
      }
      if (b.defaultDurability) cur.defaultDurability = b.defaultDurability
      if (b.autoFailover)
        cur.autoFailover = {
          ...cur.autoFailover,
          ...Object.fromEntries(Object.entries(b.autoFailover).filter(([, v]) => v !== undefined)),
        }
      if (b.rebalance) cur.rebalance = { ...cur.rebalance, ...b.rebalance }
      if (b.features?.sharding && !cur.features.sharding) {
        cur.features.sharding = true
        cur.restart = { required: true, serverIds: s.servers.map((x) => x.id) }
      }
      if (b.features?.ledger && !cur.features.ledger) {
        cur.features.ledger = true
        cur.restart = { required: true, serverIds: s.servers.map((x) => x.id) }
      }
      return ok(cur)
    },

    // --- ledger
    listLedgerStatus: (c) =>
      ok({
        items: c.s.databases
          .filter((d) => d.info.ledger.mode !== 'off')
          .map((d) => ledgerStatusOf(c.s, d)),
      }),
    getLedgerStatus: (c) => ok(ledgerStatusOf(c.s, db(c.s, c.params.databaseName ?? ''))),
    listLedgerBlocks: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const status = ledgerStatusOf(c.s, rec)
      const gid = Number(
        c.url.searchParams.get('shardGroupId') ?? status.chains[0]?.shardGroupId ?? 0,
      )
      const head = c.s.ledger.heights[gid] ?? 1000
      const blocks = Array.from({ length: Math.min(300, head + 1) }, (_, i) =>
        ledgerBlock(gid, head - i),
      )
      return ok(paginate(blocks, c.url))
    },
    listLedgerDigests: (c) =>
      ok(
        paginate(
          c.s.ledger.digests.filter((d) => d.databaseName === c.params.databaseName),
          c.url,
        ),
      ),
    generateLedgerDigest: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      if (rec.info.ledger.mode === 'off')
        throw problem('unprocessable', 'The ledger is off for this database.')
      const status = ledgerStatusOf(c.s, rec)
      const id = (c.s.ledger.digests[0]?.id ?? 0) + 1
      const digest: S['LedgerDigest'] = {
        id,
        databaseName: rec.info.name,
        generatedAt: nowIso(),
        generatedBy: c.s.session?.username ?? 'system',
        hashVersion: 1,
        chains: status.chains.map((ch) => ({
          shardGroupId: ch.shardGroupId,
          blockHeight: ch.latestBlockHeight ?? 0,
          blockHash: ch.latestBlockHash ?? fakeHash('none'),
        })),
        aggregateHash: rec.info.sharding.enabled ? fakeHash(`agg-${id}`) : null,
        anchors: [],
      }
      c.s.ledger.digests.unshift(digest)
      return created(digest)
    },
    listLedgerVerifications: (c) =>
      ok(
        paginate(
          c.s.ledger.verifications.filter((v) => v.databaseName === c.params.databaseName),
          c.url,
        ),
      ),
    startLedgerVerification: (c) => {
      const s = c.s
      const rec = db(s, c.params.databaseName ?? '')
      if (rec.info.ledger.mode === 'off')
        throw problem('unprocessable', 'The ledger is off for this database.')
      if (
        s.ledger.verifications.some(
          (v) => v.databaseName === rec.info.name && v.state === 'running',
        )
      )
        throw problem(
          'operation_in_progress',
          'A verification is already running for this database.',
        )
      const b = body<S['LedgerVerifyRequest']>(c)
      const id = (s.ledger.verifications[0]?.id ?? 0) + 1
      const v: S['LedgerVerification'] = {
        id,
        databaseName: rec.info.name,
        state: 'running',
        scope: b.scope ?? 'full',
        digestId: b.digestId ?? null,
        taskId: 0,
        startedAt: nowIso(),
        finishedAt: null,
        startedBy: s.session?.username ?? 'system',
        blocksChecked: 0,
        rowsChecked: 0,
        inconsistencies: [],
      }
      s.ledger.verifications.unshift(v)
      const task = createTask(s, {
        kind: 'ledger.verify',
        subject: { type: 'ledgerVerification', id: String(id) },
        steps: [
          'Check block hashes',
          'Check Merkle roots',
          'Check row hashes',
          'Compare with digest',
        ],
        durationMs: 6000,
        apply: () => {
          v.state = 'passed'
          v.finishedAt = nowIso()
          v.blocksChecked = Object.values(s.ledger.heights).reduce((n, h) => n + h, 0)
          v.rowsChecked = 4_812_330
        },
      })
      v.taskId = task.id
      return accepted(task)
    },
    getLedgerVerification: (c) => {
      const v = c.s.ledger.verifications.find(
        (x) => x.id === num(c, 'verificationId') && x.databaseName === c.params.databaseName,
      )
      if (!v) throw problem('not_found')
      return ok(v)
    },
    createInclusionProof: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const b = body<S['InclusionProofRequest']>(c)
      const t = rec.tables.find((x) => x.schema === b.schema && x.name === b.table)
      if (!t) throw problem('not_found', `Table ${b.schema}.${b.table} does not exist.`)
      if (!t.ledger)
        throw problem('unprocessable', `Table ${b.schema}.${b.table} is not a ledger table.`)
      if (Object.keys(b.primaryKey ?? {}).length === 0)
        throw validationFailed([
          { field: 'primaryKey', message: 'Give at least one primary key column.' },
        ])
      const key = JSON.stringify(b.primaryKey)
      const gid = (dataGroups(c.s)[Number(Object.values(b.primaryKey)[0]) % 3] ?? { id: 1 }).id
      const height = (c.s.ledger.heights[gid] ?? 1000) - 12
      const step = (n: string, position: 'left' | 'right') => ({
        hash: fakeHash(`${key}${n}`),
        position,
      })
      return ok({
        schema: b.schema,
        table: b.table,
        primaryKey: b.primaryKey,
        shardGroupId: gid,
        op: b.op ?? 'I',
        ledgerTxId: b.ledgerTxId ?? 48_000_123,
        leafHash: fakeHash(`leaf${key}`),
        leafIndex: 5,
        merklePath: [step('a', 'right'), step('b', 'left'), step('c', 'right')],
        transactionMerkleRoot: fakeHash(`m${gid}-${height}`),
        block: ledgerBlock(gid, height),
        shardHeadPath: rec.info.sharding.enabled ? [step('x', 'left'), step('y', 'right')] : null,
        aggregateHash: rec.info.sharding.enabled ? fakeHash('agg-3') : null,
        digestId: c.s.ledger.digests[0]?.id ?? null,
        hashVersion: 1,
      } satisfies S['InclusionProof'])
    },

    // --- workbench, alerts, logs, metrics
    executeQuery: (c) => ok(runQuery(c)),
    listAlerts: (c) => {
      const state = c.url.searchParams.get('state')
      const sev = c.url.searchParams.get('severity')
      return ok(
        paginate(
          c.s.alerts.filter((a) => (!state || a.state === state) && (!sev || a.severity === sev)),
          c.url,
        ),
      )
    },
    acknowledgeAlert: (c) => {
      const a = c.s.alerts.find((x) => x.id === num(c, 'alertId'))
      if (!a) throw problem('not_found')
      if (a.state === 'open') {
        a.state = 'acknowledged'
        a.acknowledgedAt = nowIso()
        a.acknowledgedBy = c.s.session?.username ?? null
      }
      return ok(a satisfies Alert)
    },
    listLogs: (c) => ok(filterLogs(c.s, c.url)),
    queryMetrics: (c) => {
      const q = (k: string) => c.url.searchParams.get(k) ?? ''
      const metric = q('metric') as S['MetricName']
      const from = new Date(q('from')).getTime()
      const to = new Date(q('to')).getTime()
      const step = Math.max(1, Number(q('step')))
      if (Number.isNaN(from) || Number.isNaN(to) || to <= from)
        throw validationFailed([{ field: 'from', message: 'The period is not valid.' }])
      const count = Math.min(5000, Math.floor((to - from) / (step * 1000)) + 1)
      const ids = q('serverId') ? [q('serverId')] : c.s.servers.map((x) => x.id)
      const byServer = q('groupBy') === 'server' || q('serverId')
      const points = (sid: string) =>
        Array.from({ length: count }, (_, i) => {
          const t = from + i * step * 1000
          return {
            timestamp: new Date(t).toISOString(),
            value: Math.round(metricValue(metric, hostNumber(sid) - 10, t) * 100) / 100,
          }
        })
      const series: S['MetricSeries'][] = byServer
        ? ids.map((sid) => ({ labels: { serverId: sid }, points: points(sid) }))
        : [
            {
              labels: {},
              points: points(ids[0] ?? '').map((p, i) => ({
                ...p,
                value:
                  Math.round(
                    (ids.reduce((n, sid) => n + (points(sid)[i]?.value ?? 0), 0) / ids.length) *
                      100,
                  ) / 100,
              })),
            },
          ]
      return ok({
        metric,
        unit: METRIC_UNIT[metric],
        from: new Date(from).toISOString(),
        to: new Date(to).toISOString(),
        step,
        series,
      } satisfies S['MetricQueryResult'])
    },
  }

  return (Object.entries(h) as [OperationId, Handler][]).map(([id, fn]) => register(id, fn))
}

// Re-exported for tests that want to inspect derived data without going through HTTP.
export { bucketCounts, groupView, tickTasks, getState, instancesOfServer }
export const SETUP_TOKEN = MOCK_SETUP_TOKEN
