// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Plumbing for the mock API: the state singleton, problem responses, pagination, and the wrapper
// that applies the same rules the real server has (setup state, session, CSRF, role from the
// contract, "not implemented" answers) before an operation handler runs.
import { http, HttpResponse, delay, type HttpHandler } from 'msw'
import { API_BASE_PATH } from '@/api/client'
import { auditActions, operations, type OperationId } from '@/api/operations.generated'
import { canCall } from '@/api/roles'
import type { AuditAction, Problem, ProblemCode, Task } from '@/api/types'
import { syncLedgerAlerts } from './ledger-view'
import { createMockState, mockConfig, tickTasks, tickHa, findTask, type MockState } from './state'

let state: MockState = createMockState()

export function getState(): MockState {
  return state
}

/** Replaces the mock cluster with a fresh one (tests call this before each case). */
export function resetMockState(options: { uninitialized?: boolean } = {}): MockState {
  state = createMockState(options)
  mockConfig.unsupported.clear()
  mockConfig.commit = '0123456789abcdef0123456789abcdef01234567'
  mockConfig.partial = null
  mockConfig.rejectCursors = false
  mockConfig.caUnavailable = false
  mockConfig.internalApiDisabled = false
  mockConfig.setupFingerprintVisible = true
  mockConfig.hostileSetupText = null
  mockConfig.omitSetupDetail = false
  mockConfig.auditUnwritable = false
  mockConfig.incompleteLedger = false
  mockConfig.ledgerStatusCoverage = 'local'
  mockConfig.clientAddress = '203.0.113.7'
  mockConfig.workbenchBusy = false
  mockConfig.leaderLost = false
  mockConfig.leaseAbsent = false
  mockConfig.failoverDataLoss = false
  mockConfig.failoverNeedsDecision = false
  mockConfig.failoverBlock = null
  mockConfig.failoverFailStep = null
  mockConfig.failoverSlow = false
  mockConfig.failoverMap = false
  mockConfig.failoverGraceful = false
  mockConfig.failoverOld = null
  mockConfig.failoverSwitchoverBehind = false
  mockConfig.failoverWarnings = false
  mockConfig.failoverNoPlan = false
  mockConfig.failoverReadiness = {}
  mockConfig.anchorTest = null
  mockConfig.anchorRefuse = null
  mockConfig.clientEntryPoint = false
  mockConfig.suspectMs = 5000
  mockConfig.failureMs = 10000
  mockConfig.syncGraceMs = 30000
  mockConfig.leaseDurationMs = 10000
  mockConfig.defaultStatementTimeoutMs = 30000
  return state
}

const STATUS: Record<ProblemCode, number> = {
  bad_request: 400,
  validation_failed: 400,
  unauthenticated: 401,
  invalid_credentials: 401,
  session_expired: 401,
  setup_token_invalid: 401,
  csrf_token_invalid: 403,
  forbidden: 403,
  not_found: 404,
  method_not_allowed: 405,
  conflict: 409,
  already_exists: 409,
  immutable_field: 409,
  rebalance_in_progress: 409,
  operation_in_progress: 409,
  task_not_cancellable: 409,
  last_admin: 409,
  role_in_use: 409,
  optimistic_lock_failed: 409,
  no_replica_available: 409,
  instance_already_running: 409,
  instance_already_stopped: 409,
  server_owns_buckets: 409,
  server_hosts_map_group: 409,
  last_server: 409,
  server_group_not_empty: 409,
  ledger_entry_pending: 409,
  system_database_stopped: 503,
  certificate_revoked: 409,
  certificate_authority_unavailable: 503,
  internal_api_disabled: 409,
  config_parameter_unknown: 404,
  config_parameter_managed: 422,
  config_parameter_refused: 422,
  config_level_not_allowed: 422,
  config_value_below_floor: 422,
  config_change_superseded: 409,
  config_restart_order_conflict: 409,
  nothing_pending: 409,
  nothing_to_rebalance: 409,
  rebalance_not_running: 409,
  cluster_inconsistent: 409,
  version_mismatch: 409,
  failover_in_progress: 409,
  group_degraded: 409,
  failover_needs_decision: 409,
  lease_settings_differ: 409,
  ledger_not_published: 409,
  origin_prefix_locked: 409,
  signing_key_missing: 409,
  last_signing_key: 409,
  chain_not_diverged: 409,
  insufficient_disk: 409,
  no_receiving_server: 409,
  unprocessable: 422,
  sql_error: 422,
  unsupported_operation: 422,
  node_state_invalid: 422,
  rate_limited: 429,
  workbench_busy: 429,
  node_uninitialized: 503,
  map_group_unavailable: 503,
  leader_unavailable: 503,
  quorum_unavailable: 503,
  service_unavailable: 503,
  internal_error: 500,
}

const TITLES: Partial<Record<ProblemCode, string>> = {
  validation_failed: 'Validation failed',
  not_found: 'Not found',
  unsupported_operation: 'Operation not supported',
}

/** Thrown by handlers to answer with a Problem. */
export class MockProblem extends Error {
  constructor(
    readonly code: ProblemCode,
    detail?: string,
    readonly extra: Partial<Problem> = {},
  ) {
    super(detail ?? code)
  }

  toProblem(requestId: string): Problem {
    return {
      type: `https://kingresdb.example/problems/${this.code.replace(/_/g, '-')}`,
      title: TITLES[this.code] ?? this.code.replace(/_/g, ' '),
      status: STATUS[this.code],
      code: this.code,
      detail: this.message === this.code ? undefined : this.message,
      requestId,
      ...this.extra,
    }
  }
}

export const problem = (
  code: ProblemCode,
  detail?: string,
  extra?: Partial<Problem>,
): MockProblem => new MockProblem(code, detail, extra)

export const validationFailed = (
  errors: { field: string; message: string; code?: string }[],
): MockProblem =>
  new MockProblem('validation_failed', 'One or more fields are not valid.', { errors })

export interface Reply {
  status: number
  body?: unknown
  /** A JSON text sent byte for byte (a document that a verifier reads must not be re-serialized). */
  rawJson?: string
  /** Bytes sent as they are (an archive, an evidence file); `Content-Type` comes from `headers`. */
  rawBody?: Uint8Array
  headers?: Record<string, string>
}

export const ok = (body: unknown): Reply => ({ status: 200, body })
export const created = (body: unknown): Reply => ({ status: 201, body })
export const noContent = (): Reply => ({ status: 204 })
export const accepted = (task: Task): Reply => ({
  status: 202,
  body: task,
  headers: { Location: `${API_BASE_PATH}/tasks/${task.id}` },
})

export interface Ctx {
  s: MockState
  params: Record<string, string>
  url: URL
  request: Request
  body: unknown
}

export type Handler = (ctx: Ctx) => Reply | Promise<Reply>

const SETUP_OPS = new Set<OperationId>([
  'getHealth',
  'getReadiness',
  'getNodeState',
  'checkSetupToken',
  'initCluster',
  'joinCluster',
])
const MUTATING = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])

/** Returns `limit` items from the cursor position and the cursor of the next page. */
export function paginate<T>(items: T[], url: URL): { items: T[]; nextCursor: string | null } {
  const limit = Math.min(500, Math.max(1, Number(url.searchParams.get('limit') ?? 50)))
  const cursor = url.searchParams.get('cursor')
  if (cursor && mockConfig.rejectCursors)
    throw problem(
      'bad_request',
      'The cursor is not valid: it was issued by another revision of the API.',
    )
  const offset = cursor ? Number(atob(cursor)) : 0
  const page = items.slice(offset, offset + limit)
  const next = offset + limit < items.length ? btoa(String(offset + limit)) : null
  return { items: page, nextCursor: next }
}

function toMswPath(path: string): string {
  return `*${API_BASE_PATH}${path.replace(/\{(\w+)\}/g, ':$1')}`
}

let requestCounter = 0

/** Registers one operation. Method and path come from the contract, not from this file. */
export function register(operationId: OperationId, handler: Handler): HttpHandler {
  const op = operations[operationId]
  const method = op.method.toLowerCase() as 'get' | 'post' | 'put' | 'patch' | 'delete'
  return http[method](toMswPath(op.path), async ({ request, params }) => {
    const requestId = `mock-${++requestCounter}`
    if (mockConfig.latencyMs > 0) await delay(mockConfig.latencyMs)
    const s = getState()
    tickTasks(s)
    tickHa(s)
    syncLedgerAlerts(s)
    try {
      precheck(s, operationId, request)
      const dryRun = new URL(request.url).searchParams.get('dryRun') === 'true'
      // Contract 0.6.0: an audited mutation is refused before anything is done when the audit log cannot be written.
      if (
        mockConfig.auditUnwritable &&
        MUTATING.has(request.method) &&
        !dryRun &&
        operationId !== 'login' &&
        AUDIT_ACTIONS.includes(operationId)
      )
        throw problem(
          'service_unavailable',
          'The audit log cannot be written; nothing was changed.',
        )
      if (mockConfig.unsupported.has(operationId)) {
        throw problem(
          'unsupported_operation',
          'This operation is not implemented by this server version.',
        )
      }
      let body: unknown = undefined
      if (MUTATING.has(request.method) && request.headers.get('content-type')?.includes('json')) {
        body = await request.json().catch(() => {
          throw problem('bad_request', 'The request body is not valid JSON.')
        })
      }
      const flat: Record<string, string> = {}
      // MSW has already percent-decoded the path parameters; decoding them again would change
      // every name that contains a percent sign.
      for (const [k, v] of Object.entries(params)) flat[k] = String(v)
      const reply = await handler({ s, params: flat, url: new URL(request.url), request, body })
      // A dry run changes nothing and writes no audit event.
      if (MUTATING.has(request.method) && operationId !== 'login' && !dryRun)
        audit(s, operationId, flat, reply.status)
      const headers = { 'X-Request-Id': requestId, ...reply.headers }
      if (reply.rawJson !== undefined)
        return new HttpResponse(reply.rawJson, {
          status: reply.status,
          headers: { ...headers, 'Content-Type': 'application/json' },
        })
      if (reply.rawBody !== undefined)
        return new HttpResponse(reply.rawBody as BodyInit, { status: reply.status, headers })
      if (reply.status === 204 || reply.body === undefined)
        return new HttpResponse(null, { status: reply.status, headers })
      return HttpResponse.json(reply.body as Record<string, unknown>, {
        status: reply.status,
        headers,
      })
    } catch (error) {
      if (error instanceof MockProblem) {
        return HttpResponse.json(error.toProblem(requestId) as unknown as Record<string, unknown>, {
          status: STATUS[error.code],
          headers: {
            'Content-Type': 'application/problem+json',
            'X-Request-Id': requestId,
            ...(error.code === 'system_database_stopped' ? { 'Retry-After': '10' } : {}),
            ...(error.code === 'workbench_busy' ? { 'Retry-After': '2' } : {}),
            ...(error.code === 'leader_unavailable' ? { 'Retry-After': '5' } : {}),
          },
        })
      }
      throw error
    }
  })
}

/**
 * The operations that work while the system database is stopped (0.6.0). All of them need a session this
 * server checked before the stop (here: the one the mock holds); without one the answer is the 503.
 */
const WORKS_WHILE_STOPPED = new Set<OperationId>([
  'startThisServerInstance',
  'getSession',
  'listThisServerInstances',
])

/** Operations that need no session check against the leader (contract 0.7.0, `leader_unavailable`). */
const LEADER_FREE = new Set<OperationId>([
  'getHealth',
  'getReadiness',
  'getNodeState',
  'getSession',
  'startThisServerInstance',
  'listThisServerInstances',
])

/** Setup state, authentication, CSRF and role checks. */
/** Answered by a server that is stopped on purpose or resetting; these keep answering. */
const ALWAYS_UP = new Set<OperationId>(['getHealth', 'getReadiness', 'getNodeState'])

function precheck(s: MockState, operationId: OperationId, request: Request): void {
  const role = operations[operationId].role
  if (s.leave) {
    if (Date.now() >= s.leave.until) {
      // The server finished resetting itself: it is uninitialized and has no sessions.
      s.leave = null
      s.node.state = 'uninitialized'
      s.session = null
    } else if (!ALWAYS_UP.has(operationId)) {
      throw problem('service_unavailable', 'The server is resetting after it left the cluster.')
    }
  }
  if (
    s.systemDatabaseStopped &&
    !ALWAYS_UP.has(operationId) &&
    !(WORKS_WHILE_STOPPED.has(operationId) && (s.session !== null || SETUP_OPS.has(operationId)))
  )
    throw problem(
      'system_database_stopped',
      'The instance that hosts the system database is stopped. Start it with startThisServerInstance.',
    )
  if (s.node.state === 'uninitialized') {
    if (SETUP_OPS.has(operationId)) return
    // GET /tasks/{id} is not available here: setup progress is read from GET /setup/state.
    throw problem('node_uninitialized')
  }
  if (role === 'none') return
  // 0.7.0: a follower that has not heard the leader for the leader-loss bound refuses what needs a session check.
  // The probes, the node state, `getSession` (it answers from memory) and the start of a stopped instance still work.
  if (mockConfig.leaderLost && !LEADER_FREE.has(operationId))
    throw problem(
      'leader_unavailable',
      'This server has not heard from the manager leader for longer than the leader-loss bound.',
    )
  if (!s.session) throw problem('unauthenticated')
  if (new Date(s.session.expiresAt).getTime() < Date.now()) {
    s.session = null
    throw problem('session_expired')
  }
  if (MUTATING.has(request.method) && request.headers.get('X-CSRF-Token') !== s.session.csrfToken) {
    throw problem('csrf_token_invalid')
  }
  if (!canCall(s.session.clusterRoles, operationId)) throw problem('forbidden')
}

const AUDIT_ACTIONS: readonly string[] = auditActions

function audit(
  s: MockState,
  operationId: OperationId,
  params: Record<string, string>,
  status: number,
): void {
  // Only operations in the closed AuditAction enum are audited (not plans, proofs or token checks).
  if (!AUDIT_ACTIONS.includes(operationId)) return
  const [resourceType, resourceId] = Object.entries(params)[0] ?? []
  s.audit.unshift({
    id: s.nextAuditId++,
    timestamp: new Date().toISOString(),
    actor: s.session?.username ?? 'setup',
    source: 'panel',
    serverId: '10.0.0.11',
    action: operationId as AuditAction,
    resourceType: resourceType?.replace(/(Id|Name)$/, ''),
    resourceId,
    outcome: status < 400 ? 'success' : 'failure',
  })
}

/** Looks up a task and checks that it exists (used by several handlers). */
export function requireTask(s: MockState, id: number) {
  const rec = findTask(s, id)
  if (!rec) throw problem('not_found', `Task ${id} does not exist.`)
  return rec
}
