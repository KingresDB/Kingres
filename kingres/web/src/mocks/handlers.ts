// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// One handler per operation in the contract, registered by operationId (see http.ts). Request and
// response bodies are typed with the generated schema types.
import { evaluateAccess } from '@/lib/network-rules'
import { applyPending, configHandlers } from './config'
import { ledgerHandlers } from './ledger'
import { AGGREGATE_SIZE, anchoringOf, digestAnchorsOf, l7, publicationOf } from './ledger-view'
import type { HttpHandler } from 'msw'
import type { components } from '@/api/schema'
import type { OperationId } from '@/api/operations.generated'
import type { Alert, Database, Problem, Role, Table, Task } from '@/api/types'
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
  digestDocument,
  fakeHash,
  hostNumber,
  groupView,
  instancesOfServer,
  ledgerBlock,
  ledgerStatusOf,
  makeBuckets,
  MOCK_PASSWORD,
  MOCK_SETUP_TOKEN,
  mockConfig,
  nowIso,
  overview,
  planMoves,
  planView,
  serverView,
  effectiveStatus,
  startStandbyReconcile,
  tickTasks,
  topology,
  activeTask,
  failoverRunning,
  type MockState,
} from './state'
import {
  cancelFailover,
  failoverPlan,
  failoverStatusView,
  resumeFailover,
  resetAutoFailoverCounter,
  restorePlan,
  startFailoverRun,
} from './failover'

type S = components['schemas']

/**
 * A digest document as the extension prints it: its own key order and spacing, and an integer a
 * float cannot hold. The panel must save these characters unchanged.
 */
function oddDocument(doc: Record<string, unknown>): string {
  const members = Object.entries(doc)
    .reverse()
    .map(([k, v]) => `"${k}" : ${JSON.stringify(v)}`)
  members.push('"entries_total":  9007199254740993')
  return `{ ${members.join(' ,\n\t')} }`
}

/** JSON text of a digest or of a page of digests, with every chain document written by oddDocument. */
function digestJson(value: unknown): string {
  const docs: string[] = []
  const text = JSON.stringify(value, (k, v: unknown) => {
    if (k !== 'document') return v
    docs.push(oddDocument(v as Record<string, unknown>))
    return `@@${docs.length - 1}@@`
  })
  return text.replace(/"@@(\d+)@@"/g, (_, i: string) => docs[Number(i)] as string)
}

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

/** 0.11.0: what a status says about the publication and the anchoring of a database (only when a ledger exists). */
function withPhase7(
  s: MockState,
  rec: ReturnType<typeof db>,
  status: S['LedgerStatus'],
): S['LedgerStatus'] {
  return {
    ...status,
    publication: publicationOf(s, rec),
    anchoring: anchoringOf(s, rec),
  }
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
    detail: rec.task.detail,
    steps: rec.task.progress.steps.map((st) => ({
      name: st.name,
      state: st.state === 'skipped' ? 'pending' : st.state,
    })),
    createdAt: rec.task.createdAt,
    updatedAt: rec.task.updatedAt,
  }
}

const REFUSAL_TEXT: Record<string, string> = {
  nothing_to_rebalance: 'The buckets are placed as the plan would place them; nothing to move.',
  cluster_inconsistent:
    'Database shop: kingres.check_cluster() reports stalled_move (node 4). Resolve it before moving buckets.',
  version_mismatch:
    'Database shop: the servers do not run the same extension version and PostgreSQL major: extension_version_differs (node 6).',
  failover_in_progress: 'A failover is in progress (server.failover 10.0.0.12).',
  no_receiving_server: 'No server should hold shards in database shop.',
  operation_in_progress: 'Another operation changes the servers (server.add 10.0.0.14).',
  rebalance_in_progress: 'A rebalance is already running (task 118).',
}

function serverOfGroup(s: MockState, g: number): string | undefined {
  return s.groups.find((x) => x.id === g)?.primaryServerId ?? undefined
}

/** Primary buckets per database and server (the placement a report shows before and after). */
function placementOf(s: MockState): S['PlacementEntry'][] {
  const out: S['PlacementEntry'][] = []
  for (const d of s.databases) {
    if (!d.info.sharding.enabled) continue
    const counts = new Map<string, number>()
    for (const b of [...d.buckets.values()].flat()) {
      const sv = serverOfGroup(s, b.group)
      if (sv) counts.set(sv, (counts.get(sv) ?? 0) + 1)
    }
    for (const [serverId, primaryBuckets] of [...counts].sort())
      out.push({ databaseName: d.info.name, serverId, primaryBuckets })
  }
  return out
}

/** Writes (or, for a stopped run, replaces) the report of a run. */
function pushReport(
  s: MockState,
  o: {
    id: number
    outcome: 'completed' | 'stopped' | 'failed' | 'cancelled'
    startedAt: string
    moves: S['PlannedMove'][]
    doneCount: number
    failedLast: boolean
    before: S['PlacementEntry'][]
  },
): void {
  const total = o.moves.length
  const listed = o.moves.slice(0, o.outcome === 'stopped' ? o.doneCount + 1 : total)
  const bytes = o.moves.slice(0, o.doneCount).reduce((n, m) => n + (m.estimatedSizeBytes ?? 0), 0)
  s.rebalance.reports = s.rebalance.reports.filter((x) => x.id !== o.id)
  s.rebalance.reports.unshift({
    id: o.id,
    outcome: o.outcome,
    startedAt: o.startedAt,
    finishedAt: nowIso(),
    options: s.rebalance.options ?? {},
    stoppedBy: o.outcome === 'stopped' || o.outcome === 'cancelled' ? 'operator' : null,
    error:
      o.outcome === 'failed' ? '1 bucket move failed and was given up after 3 attempts.' : null,
    totals: {
      movesTotal: total,
      movesDone: o.doneCount,
      movesFailed: o.failedLast ? 1 : 0,
      movesSkipped: o.outcome === 'stopped' ? 1 : 0,
      bytesMoved: bytes,
      durationSeconds: Math.max(1, Math.round((Date.now() - Date.parse(o.startedAt)) / 1000)),
    },
    placementBefore: o.before,
    placementAfter: placementOf(s),
    moves: (o.failedLast ? listed.slice(-50) : listed.slice(0, 50)).map((m, j) => {
      const i = j + (o.failedLast ? Math.max(0, listed.length - 50) : 0)
      const base = {
        databaseName: m.databaseName,
        colocationId: m.colocationId,
        bucket: m.bucket,
        fromShardGroupId: m.fromShardGroupId,
        toShardGroupId: m.toShardGroupId,
        fromServerId: serverOfGroup(s, m.fromShardGroupId),
        toServerId: serverOfGroup(s, m.toShardGroupId),
        ...(m.estimatedSizeBytes === null ? {} : { sizeBytes: m.estimatedSizeBytes }),
      }
      if (i < o.doneCount)
        return {
          ...base,
          outcome: 'done' as const,
          attempts: 1,
          skipReason: null,
          durationSeconds: 3,
          error: null,
        }
      if (o.outcome === 'stopped')
        return {
          ...base,
          outcome: 'skipped' as const,
          attempts: 1,
          skipReason: 'stopped',
          durationSeconds: 1,
          error: null,
        }
      return {
        ...base,
        outcome: 'failed' as const,
        attempts: 3,
        skipReason: null,
        durationSeconds: 95,
        error: 'kingres.move_bucket: the subscription could not copy the table (simulated).',
      }
    }),
  })
}

/** A stop that was asked a moment ago has ended: the move in flight is undone and the run is stopped. */
function settleStop(s: MockState): void {
  const r = s.rebalance
  if (r.state !== 'stopping' || r.stopAskedAt === null || Date.now() - r.stopAskedAt < 1500) return
  const done = Math.floor(r.fractionAtStop * r.moves.length)
  applyMoveProgress(s, r.moves, done, 0)
  r.state = 'stopped'
  r.stopAskedAt = null
  r.pausedReason = 'Stopped by the operator; start the rebalance again to continue.'
  if (r.taskId !== null) {
    // The task of a stopped run is paused, and a paused task can be cancelled (contract 0.9.0).
    const rec = requireTask(s, r.taskId)
    rec.task.state = 'paused'
    rec.task.cancellable = true
    pushReport(s, {
      id: r.taskId,
      outcome: 'stopped',
      startedAt: r.startedAt ?? nowIso(),
      moves: r.moves,
      doneCount: done,
      failedLast: false,
      before: [],
    })
  }
}

function rebalanceStatusView(s: MockState): S['RebalanceStatus'] {
  settleStop(s)
  const r = s.rebalance
  const rec = r.taskId ? requireTask(s, r.taskId) : undefined
  const total = r.moves.length
  const fraction =
    r.state === 'stopped' || r.state === 'stopping'
      ? r.fractionAtStop
      : (rec?.task.progress.percent ?? 0) / 100
  const done = Math.floor(fraction * total)
  const active = r.state === 'running' || r.state === 'stopping'
  const inFlight = active && done < total ? r.moves.slice(done, done + 1) : []
  const phases = ['prepare', 'copy', 'catchUp', 'switch', 'cleanup'] as const
  const failed = r.state === 'failed' ? 1 : 0
  return {
    state: r.state,
    needed: r.needed,
    reasons: r.reasons,
    taskId: r.state === 'idle' || r.state === 'failed' ? null : r.taskId,
    startedAt: r.state === 'idle' || r.state === 'failed' ? null : r.startedAt,
    options: r.state === 'idle' || r.state === 'failed' ? null : r.options,
    progress:
      r.state === 'idle' || r.state === 'failed'
        ? null
        : {
            movesTotal: total,
            movesDone: done,
            movesInFlight: inFlight.length,
            movesFailed: failed,
            movesSkipped: 0,
            bytesMoved: r.moves.slice(0, done).reduce((n, m) => n + (m.estimatedSizeBytes ?? 0), 0),
            percent: Math.round(fraction * 1000) / 10,
          },
    inFlightMoves: inFlight.map((m) => {
      // One move at a time: its phase follows the position inside the move.
      const within = fraction * total - done
      const phase = phases[Math.min(4, Math.floor(within * 5))] as (typeof phases)[number]
      return {
        databaseName: m.databaseName,
        colocationId: m.colocationId,
        bucket: m.bucket,
        fromShardGroupId: m.fromShardGroupId,
        toShardGroupId: m.toShardGroupId,
        fromServerId: serverOfGroup(s, m.fromShardGroupId),
        toServerId: serverOfGroup(s, m.toShardGroupId),
        phase,
        percent: [5, 30, 85, 95, 100][phases.indexOf(phase)] as number,
        sizeBytes: m.estimatedSizeBytes,
      }
    }),
    stoppedBy: r.state === 'stopped' || r.state === 'stopping' ? r.stoppedBy : null,
    pausedReason: r.state === 'stopped' ? r.pausedReason : null,
    lastReport: lastReportOf(s),
  } satisfies S['RebalanceStatus']
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

const LEADER = '10.0.0.11'

function ownsCopy(g: { primaryServerId: string | null; standbyServerIds: string[] }, id: string) {
  return g.primaryServerId === id || g.standbyServerIds.includes(id)
}

/** The sharded databases in which the server holds a copy of at least one bucket. */
function bucketsHeldBy(s: MockState, id: string): { database: string; buckets: number }[] {
  const out: { database: string; buckets: number }[] = []
  for (const d of s.databases) {
    if (!d.info.sharding.enabled) continue
    let n = 0
    for (const b of [...d.buckets.values()].flat()) {
      const g = s.groups.find((x) => x.id === b.group)
      if (g && ownsCopy(g, id)) n += 1
    }
    if (n > 0) out.push({ database: d.info.name, buckets: n })
  }
  return out
}

/** An unfinished add, remove or leave of this server. */
function isOpenServerTask(
  t: { kind: string; state: string; subject: { id: string } | null },
  id: string,
): boolean {
  return (
    t.state === 'running' &&
    ['server.add', 'server.remove', 'server.leave'].includes(t.kind) &&
    t.subject?.id === id
  )
}

function markRebalance(s: MockState, reason: S['RebalanceReason']): void {
  s.rebalance.needed = true
  if (!s.rebalance.reasons.includes(reason)) s.rebalance.reasons.push(reason)
}

/** 0.12.0: the refusal of a request that needs a decision carries the plan (`?failover=noplan`: a server that does not yet). */
function decisionExtra(plan: S['FailoverPlan']): Partial<Problem> {
  return mockConfig.failoverNoPlan ? {} : { plan }
}

/** 0.12.0: a database is not created, dropped or distributed while a failover or a restore runs (409 `failover_in_progress`). */
function refuseDuringFailover(s: MockState): void {
  if (failoverRunning(s))
    throw problem('failover_in_progress', 'A failover is running (server.failover).')
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
      serverId: LOCAL,
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
    serverId: LOCAL,
    outcome: 'success',
  })
  const out: S['Session'] = {
    ...session,
    serverId: LOCAL,
    clusterId: ctx.s.clusterId,
    clusterName: ctx.s.clusterName,
    token: issueToken ? `mock-token-${csrfToken}` : null,
    ...(mockConfig.clientAddress === null ? {} : { clientAddress: mockConfig.clientAddress }),
  }
  return ok(out)
}

/** The address the mock "sees" the request come from (documentation range, never a real host). */
export const MOCK_CLIENT_ADDRESS = '203.0.113.7'

function sessionBody(s: MockState): S['Session'] {
  const sess = s.session
  if (!sess) throw problem('unauthenticated')
  return {
    ...sess,
    serverId: LOCAL,
    clusterId: s.clusterId,
    clusterName: s.clusterName,
    token: null,
    ...(mockConfig.clientAddress === null ? {} : { clientAddress: mockConfig.clientAddress }),
    // 0.6.0: while the system database is stopped the answer comes from memory and says so.
    ...(s.systemDatabaseStopped
      ? {
          degraded: true,
          degradedReason:
            'The instance that hosts the system database of this server is stopped; this answer comes from the memory of the server.',
        }
      : {}),
  }
}

function requireSetupToken(ctx: Ctx): void {
  if (ctx.request.headers.get('X-Setup-Token') !== MOCK_SETUP_TOKEN)
    throw problem('setup_token_invalid')
}

/** A second setup request while one runs is a conflict (the token stays valid after a failure). */
/**
 * The contract says "SHA-256 fingerprint (hex, colons allowed)": 64 hex digits, with or without colons
 * between the pairs, in either case. Nothing else (no spaces, no prefix) is accepted, so the mock is not
 * more permissive than the contract, and a client that sends the display format is found out.
 */
const FINGERPRINT_SHAPE = /^[0-9a-fA-F]{2}(:?[0-9a-fA-F]{2}){31}$/
function requireFingerprintShape(value: string | null | undefined): void {
  if (value === undefined || value === null || value === '') return
  if (!FINGERPRINT_SHAPE.test(value))
    throw validationFailed([
      {
        field: '/expectedFingerprint',
        message: 'Use 64 hexadecimal digits; colons between byte pairs are allowed.',
        code: 'pattern',
      },
    ])
}
/**
 * Contract 0.6.0: a request without a pin (body member or header `X-Kingres-Pin-Sha256`) and without
 * `allowUnpinned: true` is 400 at `/expectedFingerprint`; a malformed pin is 400 as well. Returns the pin.
 */
function requirePin(
  ctx: Ctx,
  b: { expectedFingerprint?: string | null; allowUnpinned?: boolean },
): string | null {
  const header = ctx.request.headers.get('X-Kingres-Pin-Sha256')
  const pin = b.expectedFingerprint || header || null
  requireFingerprintShape(pin)
  if (b.expectedFingerprint && header && normalizeFp(b.expectedFingerprint) !== normalizeFp(header))
    throw validationFailed([
      {
        field: '/expectedFingerprint',
        message: 'The body and the header X-Kingres-Pin-Sha256 name different fingerprints.',
        code: 'mismatch',
      },
    ])
  if (!pin && b.allowUnpinned !== true)
    throw validationFailed([
      {
        field: '/expectedFingerprint',
        message:
          'Give the fingerprint of the server, or set allowUnpinned to trust whatever answers.',
        code: 'required',
      },
    ])
  return pin
}

/** The server runs without its internal API (contract 0.6.0, `-internal-listen` not given). */
function requireInternalApi(): void {
  if (mockConfig.internalApiDisabled)
    throw problem(
      'internal_api_disabled',
      'The server was started without its internal API (no internal listen address).',
    )
}

function normalizeFp(text: string): string {
  return text.replace(/:/g, '').toLowerCase()
}

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
  const pin = requirePin(ctx, b)
  if (b.existingNodeAddress.includes('unreachable'))
    throw problem('unprocessable', `Cannot reach ${b.existingNodeAddress}.`)
  if (b.admin.password !== MOCK_PASSWORD) throw problem('invalid_credentials')
  const s = ctx.s
  // The certificate the existing server presents; a pin that differs refuses the join before the
  // password is sent.
  const presented = fakeHash(`internal-${b.existingNodeAddress.replace(/:[0-9]+$/, '')}`)
  const pinMismatch = !!pin && normalizeFp(pin) !== presented
  return accepted(
    createTask(s, {
      kind: 'cluster.join',
      // What the server says about the pin check (SetupTaskSummary.detail, Task.detail).
      detail: mockConfig.omitSetupDetail
        ? null
        : mockConfig.hostileSetupText
          ? mockConfig.hostileSetupText
          : pinMismatch
            ? null
            : pin
              ? `The certificate of the existing server matched the pinned fingerprint ${presented}.`
              : `No pin was checked (allowUnpinned): the certificate fingerprint seen on first contact is ${presented}.`,
      failWith: pinMismatch
        ? 'The certificate presented by the existing server is not the expected one. Nothing was sent.'
        : undefined,
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

/** The group of the instance has a standby that is not caught up: this is the only current copy. */
function isOnlyUpToDateCopy(s: MockState, inst: S['Instance']): boolean {
  if (inst.role !== 'primary') return false
  return s.servers
    .filter((x) => x.id !== LOCAL)
    .flatMap((x) => instancesOfServer(s, x.id))
    .some(
      (other) =>
        other.shardGroupId === inst.shardGroupId &&
        other.role === 'standby' &&
        (other.replicationLagSeconds ?? 0) >= 1,
    )
}

function instanceAction(action: 'restart' | 'stop' | 'start'): Handler {
  return (ctx) => {
    const id = num(ctx, 'instanceId')
    const s = ctx.s
    const inst = thisServerInstance(s, id)
    const force = action !== 'start' && body<{ force?: boolean }>(ctx).force === true
    const state = inst.state
    if (action === 'start') {
      if (state === 'running' || state === 'starting' || state === 'restarting')
        throw problem('instance_already_running', `Instance ${id} is ${state}.`)
      if (state === 'stopping') throw problem('operation_in_progress')
    } else {
      if (state === 'stopped' || state === 'stopping' || state === 'failed')
        throw problem('instance_already_stopped', `Instance ${id} is ${state}.`)
      if (state === 'starting' || state === 'restarting') throw problem('operation_in_progress')
    }
    if (activeTask(s, `instance.${action}`, String(id))) throw problem('operation_in_progress')
    if (action !== 'start' && !force && isOnlyUpToDateCopy(s, inst))
      throw problem(
        'no_replica_available',
        `Instance ${id} is the only up-to-date copy of shard group ${inst.shardGroupId}; a standby is behind.`,
      )
    const steps =
      action === 'stop'
        ? ['Stop the instance']
        : action === 'start'
          ? ['Start the instance']
          : ['Restart the instance']
    s.instanceStates.set(
      id,
      action === 'stop' ? 'stopping' : action === 'start' ? 'starting' : 'restarting',
    )
    return accepted(
      createTask(s, {
        kind: `instance.${action}`,
        subject: { type: 'instance', id: String(id) },
        steps,
        durationMs: 2500,
        apply: () => {
          s.instanceStates.set(id, action === 'stop' ? 'stopped' : 'running')
          if (inst.isMapGroup) s.systemDatabaseStopped = action === 'stop'
          // A new postmaster has read the saved settings.
          if (action !== 'stop') s.localSettings.restartRequired = false
          // A new postmaster reads the configuration file: what waited for a restart is running now.
          if (action !== 'stop') applyPending(s, inst.serverId)
        },
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
  const ledger = ledgerStatusOf(s, rec, true)
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
    default:
      return clientMetricValue(metric, serverId, t)
  }
}

/**
 * The client entry point (0.7.0): totals since the start of kingresd, except the `Active` ones. The standby entry
 * point carries about a fifth of the traffic.
 */
function clientMetricValue(metric: S['MetricName'], serverId: number, t: number): number {
  const standby = metric.startsWith('clientStandby')
  const name = metric.replace(/^client(Standby)?/, '')
  const factor = standby ? 0.2 : 1
  const hours = t / 3_600_000
  const wave = (period: number, phase: number) =>
    (Math.sin((t / 1000 / period) * 2 * Math.PI + phase + serverId) + 1) / 2
  switch (name) {
    case 'ConnectionsActive':
      return Math.round((12 + serverId * 4 + wave(400, 1) * 14) * factor)
    case 'ConnectionsAccepted':
      return Math.round((20_000 + serverId * 1_500 + (hours % 5000) * 90) * factor)
    case 'ConnectionsRefused':
      return Math.round((3 + serverId + (hours % 5000) * 0.02) * factor)
    case 'ConnectFailures':
      return Math.round((8 + serverId * 2 + (hours % 5000) * 0.05) * factor)
    case 'ConnectionsMoved':
      return Math.round((140 + serverId * 10 + (hours % 5000) * 1.5) * factor)
    case 'ConnectionsIdleClosed':
      return Math.round((900 + serverId * 60 + (hours % 5000) * 12) * factor)
    case 'BytesIn':
      return Math.round((38 + serverId * 4 + (hours % 5000) * 0.8) * 1024 * MiB * factor)
    case 'BytesOut':
      return Math.round((120 + serverId * 9 + (hours % 5000) * 2.1) * 1024 * MiB * factor)
    default:
      return 0
  }
}

/** The metrics of the client entry point of a given kind of entry point. */
export const CLIENT_METRICS = {
  on: [
    'clientConnectionsAccepted',
    'clientConnectionsActive',
    'clientConnectionsRefused',
    'clientConnectFailures',
    'clientConnectionsMoved',
    'clientConnectionsIdleClosed',
    'clientBytesIn',
    'clientBytesOut',
  ],
  standby: [
    'clientStandbyConnectionsAccepted',
    'clientStandbyConnectionsActive',
    'clientStandbyConnectionsRefused',
    'clientStandbyConnectFailures',
    'clientStandbyConnectionsMoved',
    'clientStandbyConnectionsIdleClosed',
    'clientStandbyBytesIn',
    'clientStandbyBytesOut',
  ],
} as const satisfies Record<string, readonly S['MetricName'][]>

/** Whether the mock reports a metric: the client ones exist only while the client entry point is on. */
function metricExists(metric: S['MetricName']): boolean {
  if ((CLIENT_METRICS.on as readonly string[]).includes(metric))
    return mockConfig.clientEntryPoint !== false
  if ((CLIENT_METRICS.standby as readonly string[]).includes(metric))
    return mockConfig.clientEntryPoint === 'standby'
  return true
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
  clientConnectionsAccepted: 'count',
  clientConnectionsActive: 'count',
  clientConnectionsRefused: 'count',
  clientConnectFailures: 'count',
  clientConnectionsMoved: 'count',
  clientConnectionsIdleClosed: 'count',
  clientBytesIn: 'bytes',
  clientBytesOut: 'bytes',
  clientStandbyConnectionsAccepted: 'count',
  clientStandbyConnectionsActive: 'count',
  clientStandbyConnectionsRefused: 'count',
  clientStandbyConnectFailures: 'count',
  clientStandbyConnectionsMoved: 'count',
  clientStandbyConnectionsIdleClosed: 'count',
  clientStandbyBytesIn: 'bytes',
  clientStandbyBytesOut: 'bytes',
}

const LEVEL_ORDER = ['debug', 'info', 'warning', 'error']

/** `partial` and `missingServers` of a cluster-wide answer while `mockConfig.partial` is set. */
function partialAnswer(): { partial?: boolean; missingServers?: S['MissingServer'][] } {
  const missing = mockConfig.partial
  return missing ? { partial: true, missingServers: structuredClone(missing) } : {}
}

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
  if (/\bhugevalue\b/i.test(sql))
    throw problem('unprocessable', 'A value of column "doc" is larger than 1 MiB.')
  if (/\bbusyrole\b/i.test(sql))
    throw problem('workbench_busy', 'This role already runs two statements on this server.')
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
  // Contract 0.6.0: a statement that changes data is never cut short; its rows are dropped beyond the limit,
  // but it is committed and answers its real command tag with truncated: true.
  m = /^(insert|update|delete)\b.*\bbulk\b/i.exec(sql)
  if (m) {
    const verb = (m[1] ?? '').toUpperCase()
    const affected = 5000
    const limit = b.rowLimit ?? 1000
    return {
      columns: [{ name: 'id', type: 'int8' }],
      rows: Array.from({ length: Math.min(affected, limit) }, (_, i) => [i + 1]),
      rowCount: affected,
      truncated: affected > limit,
      commandTag: verb === 'INSERT' ? `INSERT 0 ${affected}` : `${verb} ${affected}`,
      durationMs: Date.now() - started + 1.5,
    }
  }
  throw problem('sql_error', `syntax error at or near "${sql.split(/\s+/)[0] ?? ''}"`, {
    sqlState: '42601',
    sqlPosition: 1,
  })
}

/** Seconds of `pg_sleep(n)` in a statement, or null when it is not one. */
function sleepSeconds(sql: string): number | null {
  const m = /^select\s+pg_sleep\(\s*(\d+(?:\.\d+)?)\s*\)$/i.exec(sql.trim().replace(/;+\s*$/, ''))
  return m ? Number(m[1]) : null
}

const wait = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms))

/**
 * `executeQuery` (0.7.0): the role runs at most two statements on this server and the server four in all
 * (429 `workbench_busy` otherwise); a request that names its statement can be cancelled with `cancelStatement`
 * while it runs (it ends with 422 `sql_error`, SQLSTATE 57014); a second running statement with the same id is
 * 409 `conflict`. `select pg_sleep(n)` is the statement that runs for `n` seconds.
 */
async function executeStatement(ctx: Ctx): Promise<S['QueryResult']> {
  const { s } = ctx
  const b = body<S['QueryRequest']>(ctx)
  const username = s.session?.username ?? ''
  const busy = () =>
    problem(
      'workbench_busy',
      'This role already runs two statements on this server, or the server runs four.',
    )
  if (mockConfig.workbenchBusy) throw busy()
  const key = `${username}\u0000${b.statementId ?? `anonymous-${Math.random()}`}`
  if (b.statementId !== undefined && s.statements.has(key))
    throw problem('conflict', 'A statement with this id is already running.')
  const mine = [...s.statements.values()].filter((x) => x.username === username).length
  if (mine >= 2 || s.statements.size >= 4) throw busy()
  const seconds = sleepSeconds(b.sql)
  if (seconds === null) return runQuery(ctx)
  db(s, b.database)
  const rec = { username, startedAt: Date.now(), cancelled: false }
  s.statements.set(key, rec)
  try {
    const timeoutMs = b.timeoutMs ?? mockConfig.defaultStatementTimeoutMs
    const end = rec.startedAt + Math.min(seconds * 1000, timeoutMs)
    while (Date.now() < end && !rec.cancelled) await wait(20)
    if (rec.cancelled)
      throw problem('sql_error', 'canceling statement due to user request', {
        sqlState: '57014',
        sqlPosition: 1,
      })
    if (seconds * 1000 > timeoutMs)
      throw problem('sql_error', 'canceling statement due to statement timeout', {
        sqlState: '57014',
        sqlPosition: 1,
      })
    return {
      columns: [{ name: 'pg_sleep', type: 'void' }],
      rows: [[null]],
      rowCount: 1,
      truncated: false,
      commandTag: 'SELECT 1',
      durationMs: Date.now() - rec.startedAt,
    }
  } finally {
    s.statements.delete(key)
  }
}

// --- the table of handlers -----------------------------------------------------------------------------

export function createHandlers(): HttpHandler[] {
  const h: Partial<Record<OperationId, Handler>> = {
    ...configHandlers(),
    ...ledgerHandlers(),
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
        commit: mockConfig.commit,
        postgresVersion: '18.1',
        setupTokenRequired: c.s.node.state === 'uninitialized',
        localhostOnly: c.s.node.state === 'uninitialized',
        serverId: c.s.node.state === 'uninitialized' ? null : LOCAL,
        clusterId: c.s.node.state === 'uninitialized' ? null : c.s.clusterId,
        setupTask: setupTaskSummary(c.s),
        // Only to a loopback caller or with a valid setup token, and only before setup (contract 0.6.0).
        internalFingerprint:
          c.s.node.state === 'uninitialized' && mockConfig.setupFingerprintVisible
            ? (mockConfig.hostileSetupText ?? fakeHash('bootstrap-10.0.0.11'))
            : null,
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
        internalFingerprint: c.s.node.leftCluster
          ? null
          : (mockConfig.hostileSetupText ?? fakeHash(`internal-${LOCAL}`)),
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
    listThisServerInstances: (c) =>
      ok({
        items: instancesOfServer(c.s, LOCAL).map((i) =>
          // Stopped state: answered from the memory of the instance manager, size and lag unknown.
          c.s.systemDatabaseStopped ? { ...i, sizeBytes: null, replicationLagSeconds: null } : i,
        ),
      }),
    getThisServerInstance: (c) => ok(thisServerInstance(c.s, num(c, 'instanceId'))),
    restartThisServerInstance: instanceAction('restart'),
    stopThisServerInstance: instanceAction('stop'),
    startThisServerInstance: instanceAction('start'),
    getThisServerSettings: (c) => ok(c.s.localSettings),
    updateThisServerSettings: (c) => {
      const b = body<Record<string, unknown>>(c) as S['LocalSettingsUpdate']
      const l = c.s.localSettings
      const known = ['memoryQuota', 'logLevel', 'logDirectory']
      const unknown = Object.keys(b).filter((k) => !known.includes(k))
      if (unknown.length || Object.keys(b).length === 0)
        throw validationFailed(
          unknown.length
            ? unknown.map((k) => ({
                field: `/${k}`,
                message:
                  k === 'dataPaths' || k === 'clientPorts'
                    ? 'This setting cannot be changed after the server was created.'
                    : 'Unknown setting.',
              }))
            : [{ field: '/', message: 'Send at least one setting.' }],
        )
      if (b.memoryQuota) {
        const mb = b.memoryQuota.totalMegabytes
        if (!(mb >= 512 && mb <= 16_777_216))
          throw validationFailed([
            { field: '/memoryQuota/totalMegabytes', message: 'Use 512 to 16777216 MB.' },
          ])
        if (mb > 65_536)
          throw problem('unprocessable', 'The machine has 64 GiB of memory.', {
            errors: [
              { field: '/memoryQuota/totalMegabytes', message: 'More than the machine has.' },
            ],
          })
      }
      if (b.logDirectory !== undefined && b.logDirectory !== null) {
        if (b.logDirectory === '')
          throw validationFailed([{ field: '/logDirectory', message: 'Must not be empty.' }])
        if (!b.logDirectory.startsWith('/') || b.logDirectory.includes('missing'))
          throw problem('unprocessable', 'The log directory must be an absolute, existing path.', {
            errors: [
              {
                field: '/logDirectory',
                message: 'Not an absolute path of an existing, writable directory.',
              },
            ],
          })
      }
      if (b.memoryQuota && b.memoryQuota.totalMegabytes !== l.memoryQuota.totalMegabytes) {
        l.memoryQuota = b.memoryQuota
        l.restartRequired = true
      }
      if (b.logLevel) l.logLevel = b.logLevel
      if (b.logDirectory !== undefined && b.logDirectory !== l.dataPaths.logDirectory) {
        l.dataPaths = { ...l.dataPaths, logDirectory: b.logDirectory ?? null }
        l.restartRequired = true
      }
      return ok(l)
    },
    leaveCluster: (c) => {
      const s = c.s
      requireInternalApi()
      const force = body<{ force?: boolean }>(c).force === true
      if (s.servers.length <= 1) throw problem('last_server')
      if (
        s.groups.some(
          (g) =>
            g.isMapGroup && (g.primaryServerId === LOCAL || g.standbyServerIds.includes(LOCAL)),
        )
      )
        throw problem('server_hosts_map_group', 'This server runs an instance of shard group 0.')
      const held = s.databases
        .map((d) => ({
          name: d.info.name,
          copies: [...d.buckets.values()].flat().filter((b) => {
            const g = s.groups.find((x) => x.id === b.group)
            return g?.primaryServerId === LOCAL || g?.standbyServerIds.includes(LOCAL)
          }).length,
        }))
        .filter((x) => x.copies > 0)
      if (held.length)
        throw problem(
          'server_owns_buckets',
          held.map((x) => `${x.name}: ${x.copies} bucket copies`).join('; '),
        )
      if (s.rebalance.state === 'running') throw problem('rebalance_in_progress')
      if (activeTask(s, 'server.leave', LOCAL)) throw problem('operation_in_progress')
      const resetAfterMs = mockConfig.leaveResetMs
      if (!s.leaderReachable) {
        if (!force) throw problem('service_unavailable', 'The manager leader cannot be reached.')
        // No task can exist on a cluster that cannot be reached: a finished task with id 0.
        s.leave = { until: Date.now() + resetAfterMs }
        const now = nowIso()
        return accepted({
          id: 0,
          kind: 'server.leave',
          state: 'done',
          progress: {
            percent: 100,
            currentStep: null,
            steps: [{ name: 'Reset this server', state: 'done' }],
          },
          error: null,
          detail: null,
          subject: { type: 'server', id: LOCAL },
          cancellable: false,
          createdAt: now,
          updatedAt: now,
          createdBy: s.session?.username ?? 'system',
        })
      }
      return accepted(
        createTask(s, {
          kind: 'server.leave',
          subject: { type: 'server', id: LOCAL },
          steps: ['Check again', 'Unregister the server', 'Revoke the certificate'],
          apply: () => {
            s.leave = { until: Date.now() + resetAfterMs }
          },
        }),
      )
    },
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
          .filter(
            (x) =>
              (!status || effectiveStatus(x) === status) && (!group || x.serverGroup === group),
          )
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
      requireInternalApi()
      const pin = requirePin(c, b)
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
      // The bootstrap certificate the new server presents. A pin that differs refuses it before the
      // setup token is sent.
      const seen = fakeHash(`bootstrap-${id}`)
      const pinned = !!pin
      const mismatch = pinned && normalizeFp(pin ?? '') !== seen
      return accepted(
        createTask(s, {
          kind: 'server.add',
          subject: { type: 'server', id: String(id) },
          detail: mismatch
            ? null
            : `Certificate fingerprint seen on first contact: ${seen}${pinned ? ' (pinned)' : ' (not pinned)'}.`,
          failWith: mismatch
            ? 'The certificate presented by the new server is not the expected one. The setup token was not sent.'
            : undefined,
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
              certificateExpiresAt: new Date(Date.now() + 365 * 86_400_000).toISOString(),
              seenFingerprint: seen,
              fingerprintPinned: pinned,
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
      const s = c.s
      const rec = server(s, c.params.serverId ?? '')
      const b = body<S['ServerUpdateRequest']>(c)
      const known = ['serverGroup', 'shouldHaveShards']
      const keys = Object.keys(b)
      if (keys.length === 0)
        throw validationFailed([{ field: '', message: 'Give serverGroup or shouldHaveShards.' }])
      const stray = keys.find((k) => !known.includes(k))
      if (stray)
        throw validationFailed([
          { field: `/${stray}`, message: 'This member is read-only or unknown.' },
        ])
      if (b.serverGroup != null && !s.serverGroups.some((g) => g.name === b.serverGroup))
        throw problem('unprocessable', `Server group "${b.serverGroup}" does not exist.`, {
          errors: [{ field: '/serverGroup', message: 'Unknown server group.' }],
        })
      const groupChanges = b.serverGroup !== undefined && b.serverGroup !== rec.serverGroup
      const placementChanges =
        b.shouldHaveShards !== undefined && b.shouldHaveShards !== rec.shouldHaveShards
      // An equal value is a no-op, also while the server is being added or removed.
      if (groupChanges || placementChanges) {
        if (s.tasks.some((t) => isOpenServerTask(t.task, rec.id)))
          throw problem('operation_in_progress')
        requireNoRebalance(s)
      }
      if (groupChanges) {
        rec.serverGroup = b.serverGroup ?? null
        markRebalance(s, 'serverGroupChanged')
      }
      if (placementChanges) rec.shouldHaveShards = b.shouldHaveShards as boolean
      return ok(serverView(s, rec))
    },
    removeServer: (c) => {
      const s = c.s
      requireInternalApi()
      const rec = server(s, c.params.serverId ?? '')
      if (rec.status === 'dead')
        throw problem(
          'node_state_invalid',
          'The server is dead. Fail it over first, then remove it.',
        )
      if (s.servers.length <= 1) throw problem('last_server')
      if (s.groups.some((g) => g.isMapGroup && ownsCopy(g, rec.id)) || rec.id === LEADER)
        throw problem(
          'server_hosts_map_group',
          'The server has an instance of shard group 0 or is the manager leader.',
        )
      const owned = bucketsHeldBy(s, rec.id)
      if (owned.length > 0)
        throw problem(
          'server_owns_buckets',
          `Holds copies of buckets of: ${owned.map((o) => `${o.database} (${o.buckets})`).join(', ')}.`,
        )
      requireNoRebalance(s)
      if (
        activeTask(s, 'server.remove', String(rec.id)) ||
        activeTask(s, 'server.leave', String(rec.id))
      )
        throw problem('operation_in_progress')
      return accepted(
        createTask(s, {
          kind: 'server.remove',
          subject: { type: 'server', id: String(rec.id) },
          steps: [
            'Check that the server holds no bucket',
            'Unregister the node',
            'Reset the server',
            'Revoke its certificate',
          ],
          apply: () => {
            s.groups = s.groups.filter((g) => g.isMapGroup || g.primaryServerId !== rec.id)
            s.servers = s.servers.filter((x) => x !== rec)
            for (const g of s.groups) {
              if (g.primaryServerId === rec.id)
                g.primaryServerId = g.standbyServerIds.shift() ?? null
              g.standbyServerIds = g.standbyServerIds.filter((x) => x !== rec.id)
            }
            fixReplicas(s)
          },
        }),
      )
    },
    renewServerCertificates: (c) => {
      const s = c.s
      requireInternalApi()
      if (mockConfig.caUnavailable) throw problem('certificate_authority_unavailable')
      const rec = server(s, c.params.serverId ?? '')
      if (rec.certificateRevokedAt) throw problem('certificate_revoked')
      if (activeTask(s, 'certificate.renew', String(rec.id))) throw problem('operation_in_progress')
      return accepted(
        createTask(s, {
          kind: 'certificate.renew',
          subject: { type: 'server', id: String(rec.id) },
          steps: ['Issue the certificate', 'Install it on the server'],
          apply: () => {
            rec.certificateExpiresAt = new Date(Date.now() + 365 * 86400_000).toISOString()
          },
        }),
      )
    },
    revokeServerCertificates: (c) => {
      const s = c.s
      requireInternalApi()
      if (mockConfig.caUnavailable) throw problem('certificate_authority_unavailable')
      const rec = server(s, c.params.serverId ?? '')
      if (rec.id === '10.0.0.11') throw problem('server_hosts_map_group')
      if (rec.certificateRevokedAt) throw problem('certificate_revoked')
      if (activeTask(s, 'certificate.revoke', String(rec.id)))
        throw problem('operation_in_progress')
      return accepted(
        createTask(s, {
          kind: 'certificate.revoke',
          subject: { type: 'server', id: String(rec.id) },
          steps: ['Revoke the certificates', 'Send the revocation list to every server'],
          apply: () => {
            rec.certificateRevokedAt = nowIso()
          },
        }),
      )
    },
    failoverServer: (c) => {
      const s = c.s
      const rec = server(s, c.params.serverId ?? '')
      const req = body<S['FailoverRequest']>(c)
      const mode = req.mode
      requireNoRebalance(s)
      const plan = failoverPlan(s, rec.id, mode, req)
      // A dry run answers the plan and changes nothing (200, not a task).
      if (req.dryRun === true) return ok(plan)
      // A task that waits for a decision is resumed by a repeat of the request that accepts the loss.
      const waiting = s.tasks.find(
        (t) =>
          t.task.kind === 'server.failover' &&
          t.task.state === 'paused' &&
          t.task.subject?.id === String(rec.id),
      )
      if (waiting) {
        if (req.allowDataLoss !== true)
          throw problem(
            'failover_needs_decision',
            'The failover waits for a decision: commits that only the old primary holds can be lost.',
            decisionExtra(plan),
          )
        return accepted(resumeFailover(s, waiting))
      }
      if (activeTask(s, 'server.failover', String(rec.id))) throw problem('operation_in_progress')
      if (failoverRunning(s)) throw problem('failover_in_progress')
      const hard = plan.blockers.find(
        (x) => x.code !== 'failoverNeedsDecision' && x.code !== 'no_replica_available',
      )
      if (hard) throw problem(hard.code as Parameters<typeof problem>[0], hard.message)
      const stuck = plan.blockers.find((x) => x.code === 'no_replica_available')
      if (stuck) throw problem('no_replica_available', stuck.message)
      const decision = plan.blockers.find((x) => x.code === 'failoverNeedsDecision')
      if (decision && req.allowDataLoss !== true)
        throw problem('failover_needs_decision', decision.message, decisionExtra(plan))
      return accepted(startFailoverRun(s, { serverId: rec.id, mode, req, plan, trigger: 'manual' }))
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
      if (c.s.serverGroups.some((g) => g.name === b.name))
        throw problem('already_exists', `A server group named "${b.name}" already exists.`)
      c.s.serverGroups.push({ name: b.name, description: b.description ?? '' })
      return created({ name: b.name, description: b.description, serverIds: [] })
    },
    deleteServerGroup: (c) => {
      const name = c.params.serverGroupName ?? ''
      if (!c.s.serverGroups.some((g) => g.name === name))
        throw problem('not_found', `Server group "${name}" does not exist.`)
      const members = c.s.servers.filter((x) => x.serverGroup === name).map((x) => x.id)
      if (members.length > 0)
        throw problem(
          'server_group_not_empty',
          `Servers still in the group: ${members.join(', ')}.`,
        )
      c.s.serverGroups = c.s.serverGroups.filter((g) => g.name !== name)
      return noContent()
    },

    // --- databases
    listDatabases: (c) => ok({ items: c.s.databases.map((d) => databaseView(c.s, d.info.name)) }),
    getDatabase: (c) => ok(databaseView(c.s, c.params.databaseName ?? '')),
    createDatabase: (c) => {
      refuseDuringFailover(c.s)
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
          detail: `Creating ${b.name} on ${s.servers.length} servers.`,
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
      refuseDuringFailover(c.s)
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
    // The table lifecycle of contract 0.14.0: the mock answers with the shape of the contract and changes little (increment 9.7 gives the panel its dialogs).
    previewTableChange: (c) => {
      db(c.s, c.params.databaseName ?? '')
      return ok({
        blockers: [],
        warnings: [],
        notes: [],
        affectedTables: [],
        estimate: {
          rows: null,
          bytes: null,
          modeRecommended: 'blocking',
          onlineAvailable: false,
          onlineUnavailableReason: 'The mock has no online copy.',
          writersBlocked: 'wholeCopy',
        },
      } satisfies S['TableChangePreview'])
    },
    listTablePartitions: () =>
      ok({ items: [], nextCursor: null } satisfies S['TablePartitionPage']),
    undistributeTable: (c) => {
      refuseDuringFailover(c.s)
      const rec = db(c.s, c.params.databaseName ?? '')
      const t = rec.tables.find(
        (x) => x.schema === c.params.schemaName && x.name === c.params.tableName,
      )
      if (!t) throw problem('not_found', 'Table does not exist.')
      return accepted(
        createTask(c.s, {
          kind: 'table.undistribute',
          subject: { type: 'table', id: `${rec.info.name}.${t.schema}.${t.name}` },
          steps: ['Check the table', 'Copy rows', 'Switch'],
          apply: () => {
            t.classes = ['plain']
            t.distribution = null
          },
        }),
      )
    },
    alterTableDistribution: (c) => {
      refuseDuringFailover(c.s)
      const rec = db(c.s, c.params.databaseName ?? '')
      const t = rec.tables.find(
        (x) => x.schema === c.params.schemaName && x.name === c.params.tableName,
      )
      if (!t) throw problem('not_found', 'Table does not exist.')
      return accepted(
        createTask(c.s, {
          kind: 'table.alter',
          subject: { type: 'table', id: `${rec.info.name}.${t.schema}.${t.name}` },
          steps: ['Check the table', 'Copy rows', 'Switch'],
          apply: () => {
            void t
          },
        }),
      )
    },
    createTablePartition: (c) => {
      refuseDuringFailover(c.s)
      const rec = db(c.s, c.params.databaseName ?? '')
      const t = rec.tables.find(
        (x) => x.schema === c.params.schemaName && x.name === c.params.tableName,
      )
      if (!t) throw problem('not_found', 'Table does not exist.')
      return accepted(
        createTask(c.s, {
          kind: 'table.partition',
          subject: { type: 'table', id: `${rec.info.name}.${t.schema}.${t.name}` },
          steps: ['Check the request', 'Change the partitions on every server'],
          apply: () => {
            void t
          },
        }),
      )
    },
    attachTablePartition: (c) => {
      refuseDuringFailover(c.s)
      const rec = db(c.s, c.params.databaseName ?? '')
      const t = rec.tables.find(
        (x) => x.schema === c.params.schemaName && x.name === c.params.tableName,
      )
      if (!t) throw problem('not_found', 'Table does not exist.')
      return accepted(
        createTask(c.s, {
          kind: 'table.partition',
          subject: { type: 'table', id: `${rec.info.name}.${t.schema}.${t.name}` },
          steps: ['Check the request', 'Change the partitions on every server'],
          apply: () => {
            void t
          },
        }),
      )
    },
    detachTablePartition: (c) => {
      refuseDuringFailover(c.s)
      const rec = db(c.s, c.params.databaseName ?? '')
      const t = rec.tables.find(
        (x) => x.schema === c.params.schemaName && x.name === c.params.tableName,
      )
      if (!t) throw problem('not_found', 'Table does not exist.')
      return accepted(
        createTask(c.s, {
          kind: 'table.partition',
          subject: { type: 'table', id: `${rec.info.name}.${t.schema}.${t.name}` },
          steps: ['Check the request', 'Change the partitions on every server'],
          apply: () => {
            void t
          },
        }),
      )
    },
    dropTablePartition: (c) => {
      refuseDuringFailover(c.s)
      const rec = db(c.s, c.params.databaseName ?? '')
      const t = rec.tables.find(
        (x) => x.schema === c.params.schemaName && x.name === c.params.tableName,
      )
      if (!t) throw problem('not_found', 'Table does not exist.')
      return accepted(
        createTask(c.s, {
          kind: 'table.partition',
          subject: { type: 'table', id: `${rec.info.name}.${t.schema}.${t.name}` },
          steps: ['Check the request', 'Change the partitions on every server'],
          apply: () => {
            void t
          },
        }),
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
      refuseDuringFailover(c.s)
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
      const req = body<S['RestorePreferredPrimariesRequest']>(c)
      requireNoRebalance(s)
      const planned = restorePlan(s, req.shardGroupIds)
      if (req.dryRun === true) return ok(planned)
      if (activeTask(s, 'replication.restorePreferredPrimaries'))
        throw problem('operation_in_progress')
      if (failoverRunning(s)) throw problem('failover_in_progress')
      const stuck = planned.blockers[0]
      if (stuck) throw problem(stuck.code as Parameters<typeof problem>[0], stuck.message)
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
            resetAutoFailoverCounter(s)
          },
        }),
      )
    },
    getRebalanceStatus: (c) => ok(rebalanceStatusView(c.s)),
    previewRebalancePlan: (c) => {
      const b = body<S['RebalancePlanRequest']>(c)
      const plan = planView(c.s, b.moveLimit ?? 200, b.databases)
      const named = {
        ...plan,
        moves: plan.moves.map((m) => ({
          ...m,
          fromServerId: serverOfGroup(c.s, m.fromShardGroupId),
          toServerId: serverOfGroup(c.s, m.toShardGroupId),
        })),
      }
      return ok(named)
    },
    startRebalance: (c) => {
      const s = c.s
      settleStop(s)
      const r = s.rebalance
      if (r.state === 'running' || r.state === 'stopping')
        throw problem('rebalance_in_progress', 'A rebalance is running.')
      // The refusals, each with its problem code (the switch forces one).
      if (mockConfig.rebalanceRefuse) {
        const code = mockConfig.rebalanceRefuse as Parameters<typeof problem>[0]
        mockConfig.rebalanceRefuse = null
        throw problem(code, REFUSAL_TEXT[code] ?? 'The rebalance cannot start now.')
      }
      if (!s.servers.some((x) => x.shouldHaveShards && x.status !== 'dead'))
        throw problem('no_receiving_server', 'No server should hold shards.')
      const opts = body<S['RebalanceOptions']>(c)
      // A stopped run goes on: the same task, the same moves, from where it was.
      if (r.state === 'stopped' && r.taskId !== null) {
        const rec = requireTask(s, r.taskId)
        rec.task.state = 'running'
        rec.task.cancellable = true
        rec.startedAt = Date.now() - r.fractionAtStop * rec.durationMs
        r.state = 'running'
        r.stoppedBy = null
        r.pausedReason = null
        r.reports = r.reports.filter((x) => x.id !== r.taskId)
        return accepted(rec.task)
      }
      const moves = planMoves(s, opts.databases)
      if (moves.length === 0)
        throw problem(
          'nothing_to_rebalance',
          'The buckets are placed as the plan would place them.',
        )
      const total = moves.length
      const failing = mockConfig.rebalanceFail
      mockConfig.rebalanceFail = false
      const before = placementOf(s)
      r.state = 'running'
      r.moves = moves
      r.options = opts
      r.startedAt = nowIso()
      r.stoppedBy = null
      r.pausedReason = null
      r.fractionAtStop = 0
      const startedAt = Date.now()
      const task = createTask(s, {
        kind: 'rebalance',
        subject: { type: 'cluster', id: 'cluster' },
        steps: [
          'Check that the cluster can rebalance',
          'Compute the plan',
          'Move buckets',
          'Write the report',
        ],
        durationMs: Math.max(5000, total * 70) * (mockConfig.rebalanceSlow ? 3 : 1),
        cancellable: true,
        onProgress: (f) => {
          const done = Math.floor(f * (failing ? Math.max(1, total - 1) : total))
          applyMoveProgress(s, moves, Math.min(done, failing ? total - 1 : total), 0)
        },
        failAtEnd: failing
          ? {
              code: 'step_failed',
              message: '1 bucket move failed and was given up after 3 attempts.',
              step: 'Write the report',
              detail: 'Bucket move failed: the extension reported an error.',
            }
          : undefined,
        apply: () => {
          const moved = failing ? moves.slice(0, total - 1) : moves
          applyMoveProgress(s, moves, moved.length, 0)
          fixReplicas(s)
          for (const x of s.servers) if (x.status === 'pendingRebalance') x.status = 'live'
          s.rebalance.state = failing ? 'failed' : 'idle'
          s.rebalance.needed = failing
          if (!failing) s.rebalance.reasons = []
          resetAutoFailoverCounter(s)
          pushReport(s, {
            id: s.rebalance.taskId as number,
            outcome: failing ? 'failed' : 'completed',
            startedAt: new Date(startedAt).toISOString(),
            moves,
            doneCount: moved.length,
            failedLast: failing,
            before,
          })
        },
      })
      r.taskId = task.id
      return accepted(task)
    },
    stopRebalance: (c) => {
      const s = c.s
      settleStop(s)
      const r = s.rebalance
      if (r.state !== 'running') throw problem('rebalance_not_running', 'No rebalance is running.')
      const rec = r.taskId ? requireTask(s, r.taskId) : undefined
      r.fractionAtStop = (rec?.task.progress.percent ?? 0) / 100
      r.state = 'stopping'
      r.stoppedBy = 'operator'
      r.stopAskedAt = Date.now()
      if (rec) {
        // The task keeps its progress; it is paused when the move in flight has ended.
        rec.task.state = 'paused'
        rec.task.cancellable = false
      }
      return ok(rebalanceStatusView(s))
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

    getFailoverStatus: (c) => ok(failoverStatusView(c.s)),
    listFailoverReports: (c) =>
      ok(
        paginate(
          c.s.failover.reports.map(
            ({ mode: _m, options: _o, groups: _g, events: _e, ...summary }) => summary,
          ),
          c.url,
        ),
      ),
    getFailoverReport: (c) => {
      const r = c.s.failover.reports.find((x) => x.id === num(c, 'reportId'))
      if (!r) throw problem('not_found')
      return ok(r)
    },
    resetAutoFailoverCounter: (c) => {
      resetAutoFailoverCounter(c.s)
      return ok(c.s.settings)
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
      settleStop(c.s)
      const rec = requireTask(c.s, num(c, 'taskId'))
      if (!rec.task.cancellable) throw problem('task_not_cancellable')
      const r = c.s.rebalance
      if (rec.task.kind === 'rebalance' && rec.task.state === 'paused') {
        // A stopped run that a person ends for good: cancelled, not failed, and no alert.
        rec.task.state = 'failed'
        rec.task.cancellable = false
        rec.task.error = {
          code: 'cancelled',
          message: 'The task was canceled before it started.',
          retryable: false,
        }
        const report = r.reports.find((x) => x.id === rec.task.id)
        if (report) {
          report.outcome = 'cancelled'
          report.stoppedBy = 'operator'
        }
        r.state = 'idle'
        r.taskId = null
        r.pausedReason = null
        return ok(rec.task)
      }
      // A failover stops before the promotion: its freezes are lifted and its report says cancelled (running or paused).
      if (rec.task.kind === 'server.failover') return ok(cancelFailover(c.s, rec))
      if (rec.task.state !== 'running') throw problem('task_not_cancellable')
      rec.task.state = 'paused'
      rec.task.cancellable = false
      if (rec.task.kind === 'rebalance') r.state = 'stopped'
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
      const dryParam = c.url.searchParams.get('dryRun')
      if (dryParam !== null && dryParam !== 'true' && dryParam !== 'false')
        throw problem('bad_request', 'The query parameter dryRun must be true or false.')
      const dryRun = dryParam === 'true'
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
      // 422: the new rules would lock out the address of the caller (it was let in, and no longer is).
      const user = s.session?.username ?? ''
      const own = mockConfig.clientAddress
      const before = own ? evaluateAccess(s.network.rules, own, user) : null
      const after = own ? evaluateAccess(b.rules, own, user) : null
      if (
        before?.result === 'allowed' &&
        (after?.result === 'rejected' || after?.result === 'no-rule')
      )
        throw problem(
          'unprocessable',
          `These rules would lock out your own address (${own}), so they were refused.`,
        )
      if (dryRun) return ok({ valid: true, revision: s.network.revision })
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
      return ok({
        ...paginate(
          c.s.audit.filter(
            (e) =>
              (!q('actor') || e.actor === q('actor')) &&
              (!q('action') || e.action.includes(q('action') ?? '')) &&
              (!q('outcome') || e.outcome === q('outcome')),
          ),
          c.url,
        ),
        ...partialAnswer(),
      })
    },

    // --- settings
    getClusterSettings: (c) => ok(c.s.settings),
    updateClusterSettings: (c) => {
      const s = c.s
      const b = body<S['ClusterSettingsUpdateRequest']>(c)
      const errors: { field: string; message: string }[] = []
      if (Object.keys(b).length === 0)
        throw validationFailed([{ field: '/', message: 'at least one property is required' }])
      for (const group of ['autoFailover', 'rebalance', 'features', 'clock'] as const) {
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
        (b.autoFailover.timeoutSeconds < 30 || b.autoFailover.timeoutSeconds > 3600)
      )
        errors.push({
          field: 'autoFailover.timeoutSeconds',
          message: 'Between 30 and 3600 seconds.',
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
      if (
        b.clock?.maxDifferenceMs !== undefined &&
        (!Number.isInteger(b.clock.maxDifferenceMs) ||
          b.clock.maxDifferenceMs < 10 ||
          b.clock.maxDifferenceMs > 600000)
      )
        errors.push({
          field: 'clock.maxDifferenceMs',
          message: 'Between 10 and 600000 milliseconds.',
        })
      if (errors.length) throw validationFailed(errors)
      if (
        (b.features?.sharding === false && s.settings.features.sharding) ||
        (b.features?.ledger === false && s.settings.features.ledger)
      )
        throw problem('immutable_field', 'A feature cannot be turned off once it is on.')
      const cur = s.settings
      // 0.7.0: a replica count (even the current one) starts the standby check, a task that adds or removes standbys
      // as far as the servers allow. It is not a rebalance.
      let started: Task | null = null
      if (b.defaultReplicaCount !== undefined) {
        cur.defaultReplicaCount = b.defaultReplicaCount
        started = startStandbyReconcile(s, s.session?.username)
      }
      if (b.defaultDurability) cur.defaultDurability = b.defaultDurability
      if (b.autoFailover)
        cur.autoFailover = {
          ...cur.autoFailover,
          ...Object.fromEntries(Object.entries(b.autoFailover).filter(([, v]) => v !== undefined)),
        }
      if (b.rebalance) cur.rebalance = { ...cur.rebalance, ...b.rebalance }
      if (b.clock?.maxDifferenceMs !== undefined)
        cur.clock = { maxDifferenceMs: b.clock.maxDifferenceMs }
      if (b.features?.sharding && !cur.features.sharding) {
        cur.features.sharding = true
        cur.restart = { required: true, serverIds: s.servers.map((x) => x.id) }
      }
      if (b.features?.ledger && !cur.features.ledger) {
        cur.features.ledger = true
        cur.restart = { required: true, serverIds: s.servers.map((x) => x.id) }
      }
      // 0.9.0: an update that started a task answers 202 with it to a request that asked for it.
      if (started && /respond-async/i.test(c.request.headers.get('prefer') ?? '')) {
        const r = accepted(started)
        return { ...r, headers: { ...r.headers, 'Preference-Applied': 'respond-async' } }
      }
      return ok(cur)
    },

    // --- ledger
    listLedgerStatus: (c) =>
      ok({
        items: c.s.databases
          .filter((d) => d.info.ledger.mode !== 'off')
          .map((d) => withPhase7(c.s, d, ledgerStatusOf(c.s, d))),
      }),
    getLedgerStatus: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      return ok(withPhase7(c.s, rec, ledgerStatusOf(c.s, rec)))
    },
    listLedgerBlocks: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const status = ledgerStatusOf(c.s, rec, true)
      const gid = Number(
        c.url.searchParams.get('shardGroupId') ?? status.chains[0]?.shardGroupId ?? 0,
      )
      const head = c.s.ledger.heights[gid] ?? 1000
      // 0.11.0: a block is signed when a signed checkpoint of the chain log covers it (the newest few are not covered yet).
      const signedUpTo =
        l7(c.s).settings.publication.enabled &&
        l7(c.s).signing.keys.some((k) => k.state === 'active')
          ? head - 3
          : -1
      const blocks = Array.from({ length: Math.min(300, head + 1) }, (_, i) => ({
        ...ledgerBlock(gid, head - i),
        signed: head - i <= signedUpTo,
      }))
      return ok(paginate(blocks, c.url))
    },
    listLedgerDigests: (c) => ({
      status: 200,
      rawJson: digestJson(
        paginate(
          c.s.ledger.digests
            .filter((d) => d.databaseName === c.params.databaseName)
            .map((d) =>
              d.aggregateSize && d.anchors.length === 0
                ? { ...d, anchors: digestAnchorsOf(c.s, d.databaseName, d.aggregateSize) }
                : d,
            ),
          c.url,
        ),
      ),
    }),
    generateLedgerDigest: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      if (rec.info.ledger.mode === 'off')
        throw problem('unprocessable', 'The ledger is off for this database.')
      const status = ledgerStatusOf(c.s, rec, true)
      const id = (c.s.ledger.digests[0]?.id ?? 0) + 1
      const covered = mockConfig.incompleteLedger ? status.chains.slice(0, -1) : status.chains
      const uncovered = mockConfig.incompleteLedger
        ? status.chains.slice(-1).map((ch) => ch.shardGroupId)
        : []
      const digest: S['LedgerDigest'] = {
        id,
        databaseName: rec.info.name,
        generatedAt: nowIso(),
        generatedBy: c.s.session?.username ?? 'system',
        hashVersion: 1,
        chains: covered.map((ch) => ({
          shardGroupId: ch.shardGroupId,
          blockHeight: ch.latestBlockHeight ?? 0,
          blockHash: ch.latestBlockHash ?? fakeHash('none'),
          document: digestDocument(ch.shardGroupId, ch.latestBlockHeight ?? 0),
        })),
        aggregateHash:
          rec.info.sharding.enabled && uncovered.length === 0 ? fakeHash(`agg-${id}`) : null,
        ...(rec.info.sharding.enabled && uncovered.length === 0
          ? { aggregateSize: AGGREGATE_SIZE }
          : {}),
        anchors: [],
        complete: uncovered.length === 0,
        uncoveredShardGroupIds: uncovered,
      }
      c.s.ledger.digests.unshift(digest)
      return { status: 201, rawJson: digestJson(digest) }
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
        complete: !mockConfig.incompleteLedger,
        uncoveredShardGroupIds: mockConfig.incompleteLedger
          ? ledgerStatusOf(s, rec, true)
              .chains.slice(-1)
              .map((ch) => ch.shardGroupId)
          : [],
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
      if (b.op === 'D' && b.ledgerTxId === undefined)
        throw validationFailed([
          { field: '/ledgerTxId', message: 'A removed version is named by ledgerTxId.' },
        ])
      if ((b.op === 'D' || b.ledgerTxId !== undefined) && t.ledger.mode === 'appendOnly')
        throw problem(
          'unprocessable',
          `Table ${b.schema}.${b.table} is append-only: it has no removed versions.`,
        )
      // Special keys let tests and the demo reach each refusal of the contract.
      const first = Object.values(b.primaryKey)[0]
      if (first === 404404 || first === '404404')
        throw problem('not_found', 'No such row, or a policy hides it.')
      if (first === 403403 || first === '403403')
        throw problem('forbidden', `The role has no privilege on ${b.schema}.${b.table}.`)
      // Pending until a digest closes the block (the seed has digest 3 only).
      if ((first === 424242 || first === '424242') && (c.s.ledger.digests[0]?.id ?? 0) <= 3)
        throw problem('ledger_entry_pending', 'The row is not in a block yet.')
      const key = JSON.stringify(b.primaryKey)
      const gid = (dataGroups(c.s)[Number(Object.values(b.primaryKey)[0]) % 3] ?? { id: 1 }).id
      const height = (c.s.ledger.heights[gid] ?? 1000) - 12
      const hash = (n: string) => fakeHash(`${key}${n}`)
      const header = (h: number) => ({
        height: h,
        hash_version: 1 as const,
        prev_hash: fakeHash(`b${gid}-${h - 1}`),
        tx_merkle_root: fakeHash(`m${gid}-${h}`),
        tx_count: 3 + ((h * 7 + gid) % 40),
        closed_at_us: 800_000_000_000_000 + h * 1_000_000,
      })
      // The generated type of `block` carries an `additionalProperties: false` artefact (`& Record<string, never>`).
      const proof = {
        format: 1,
        chain_id: `chain-${gid}`,
        relation: `${b.schema}.${b.table}`,
        table_id: 16_384 + t.name.length,
        ledger_txid: b.ledgerTxId ?? 48_000_123,
        seq: 2,
        op: b.op ?? 'I',
        row_hash: hash('row'),
        entry_index: 1,
        entry_count: 3,
        entry_path: [hash('e0'), hash('e1')],
        tx_root: hash('tx'),
        user_name: c.s.session?.username ?? 'admin',
        tx_index: 5,
        tx_path: [hash('t0'), hash('t1'), hash('t2')],
        block: { ...header(height), block_hash: fakeHash(`b${gid}-${height}`) },
        chain: [header(height + 1), header(height + 2)].map((h) => ({
          ...h,
          block_hash: fakeHash(`b${gid}-${h.height}`),
        })),
        shard_group_id: gid,
      } as unknown as S['InclusionProof']
      return { status: 200, rawJson: `${JSON.stringify(proof, null, '\t')}\n` }
    },

    // --- workbench, alerts, logs, metrics
    executeQuery: async (c) => ok(await executeStatement(c)),
    cancelStatement: (c) => {
      const username = c.s.session?.username ?? ''
      const rec = c.s.statements.get(`${username}\u0000${c.params.statementId ?? ''}`)
      // Another role's statement, an ended one and an unknown id look the same.
      if (!rec || rec.cancelled)
        throw problem('not_found', 'No running statement of yours has this id on this server.')
      rec.cancelled = true
      return noContent()
    },
    listAlerts: (c) => {
      const state = c.url.searchParams.get('state')
      const sev = c.url.searchParams.get('severity')
      return ok({
        ...paginate(
          c.s.alerts.filter((a) => (!state || a.state === state) && (!sev || a.severity === sev)),
          c.url,
        ),
        ...partialAnswer(),
      })
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
    listLogs: (c) => ok({ ...filterLogs(c.s, c.url), ...partialAnswer() }),
    queryMetrics: (c) => {
      const q = (k: string) => c.url.searchParams.get(k) ?? ''
      const metric = q('metric') as S['MetricName']
      const from = new Date(q('from')).getTime()
      const to = new Date(q('to')).getTime()
      const step = Math.max(1, Number(q('step')))
      if (Number.isNaN(from) || Number.isNaN(to) || to <= from)
        throw validationFailed([{ field: 'from', message: 'The period is not valid.' }])
      const count = Math.min(5000, Math.floor((to - from) / (step * 1000)) + 1)
      // 0.7.0: a metric of the client entry point exists only on a server whose entry point is on; the others
      // have nothing to report, so the answer has no series (the contract does not say more).
      if (!metricExists(metric))
        return ok({
          metric,
          unit: METRIC_UNIT[metric],
          from: new Date(from).toISOString(),
          to: new Date(to).toISOString(),
          step,
          series: [],
          ...partialAnswer(),
        } satisfies S['MetricQueryResult'])
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
        ...partialAnswer(),
      } satisfies S['MetricQueryResult'])
    },
  }

  return (Object.entries(h) as [OperationId, Handler][]).map(([id, fn]) => register(id, fn))
}

// Re-exported for tests that want to inspect derived data without going through HTTP.
export { bucketCounts, groupView, tickTasks, getState, instancesOfServer }
export const SETUP_TOKEN = MOCK_SETUP_TOKEN
