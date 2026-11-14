// SPDX-License-Identifier: PostgreSQL
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
import { createMockState, mockConfig, tickTasks, findTask, type MockState } from './state'

let state: MockState = createMockState()

export function getState(): MockState {
  return state
}

/** Replaces the mock cluster with a fresh one (tests call this before each case). */
export function resetMockState(options: { uninitialized?: boolean } = {}): MockState {
  state = createMockState(options)
  mockConfig.unsupported.clear()
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
  unprocessable: 422,
  sql_error: 422,
  unsupported_operation: 422,
  node_state_invalid: 422,
  rate_limited: 429,
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

export const validationFailed = (errors: { field: string; message: string }[]): MockProblem =>
  new MockProblem('validation_failed', 'One or more fields are not valid.', { errors })

export interface Reply {
  status: number
  body?: unknown
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
    try {
      precheck(s, operationId, request)
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
      for (const [k, v] of Object.entries(params)) flat[k] = decodeURIComponent(String(v))
      const reply = await handler({ s, params: flat, url: new URL(request.url), request, body })
      if (MUTATING.has(request.method) && operationId !== 'login')
        audit(s, operationId, flat, reply.status)
      const headers = { 'X-Request-Id': requestId, ...reply.headers }
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
          headers: { 'Content-Type': 'application/problem+json', 'X-Request-Id': requestId },
        })
      }
      throw error
    }
  })
}

/** Setup state, authentication, CSRF and role checks. */
function precheck(s: MockState, operationId: OperationId, request: Request): void {
  const role = operations[operationId].role
  if (s.node.state === 'uninitialized') {
    if (SETUP_OPS.has(operationId)) return
    // GET /tasks/{id} is not available here: setup progress is read from GET /setup/state.
    throw problem('node_uninitialized')
  }
  if (role === 'none') return
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
