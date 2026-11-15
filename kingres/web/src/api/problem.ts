// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// The one place that turns API errors (RFC 9457 `Problem`) into text for people.
// Clients branch on `Problem.code`, never on the HTTP status or the English `title`.
import type { Problem, ProblemCode } from './types'

/** An error response from the API, or a failure to reach it. */
export class ApiError extends Error {
  readonly problem: Problem

  constructor(problem: Problem) {
    super(problem.detail ?? problem.title)
    this.name = 'ApiError'
    this.problem = problem
  }

  get code(): ProblemCode {
    return this.problem.code
  }

  get status(): number {
    return this.problem.status
  }
}

/** Builds the error for a request that never got an HTTP response (network down, CORS, abort). */
export function networkError(cause: unknown): ApiError {
  const detail =
    cause instanceof Error && cause.name === 'AbortError'
      ? 'The request was cancelled.'
      : cause instanceof Error
        ? cause.message
        : String(cause)
  return new ApiError({
    type: 'about:blank',
    title: 'Cannot reach the server',
    status: 503,
    code: 'service_unavailable',
    detail,
  })
}

/** Narrows a response body to Problem (a proxy may answer with an HTML error page instead). */
export function isProblem(value: unknown): value is Problem {
  if (typeof value !== 'object' || value === null) return false
  const v = value as Record<string, unknown>
  return typeof v.code === 'string' && typeof v.status === 'number' && typeof v.title === 'string'
}

/** How the UI presents an error: drives colour, icon and whether retry is offered. */
export type ProblemTone = 'info' | 'warning' | 'danger'

export interface ProblemView {
  code: ProblemCode
  tone: ProblemTone
  /** Short heading, for example "Not available on this server version yet". */
  heading: string
  /** One or two sentences that say what happened and what to do. */
  message: string
  /** Field-level problems from `validation_failed`, keyed by the API field path. */
  fieldErrors: Record<string, string>
  sqlState?: string
  requestId?: string
  /** True when trying again later may help without any change by the user. */
  retryable: boolean
}

interface Entry {
  tone: ProblemTone
  heading: string
  message: string
  retryable?: boolean
  /** Append the server's `detail` text to the message when it is present. */
  withDetail?: boolean
}

/**
 * One entry per ProblemCode. `Record<ProblemCode, ...>` makes the compiler fail when the contract
 * adds a code that has no message, so a new code cannot ship without user-facing text.
 */
const entries: Record<ProblemCode, Entry> = {
  bad_request: {
    tone: 'danger',
    heading: 'Request rejected',
    message: 'The server could not understand the request.',
    withDetail: true,
  },
  validation_failed: {
    tone: 'warning',
    heading: 'Check the highlighted fields',
    message: 'Some values are not valid.',
  },
  unauthenticated: {
    tone: 'warning',
    heading: 'Sign in required',
    message: 'Your session has ended. Sign in again to continue.',
  },
  invalid_credentials: {
    tone: 'warning',
    heading: 'Sign-in failed',
    message: 'The user name or password is not correct.',
  },
  session_expired: {
    tone: 'warning',
    heading: 'Session expired',
    message: 'Your session has expired. Sign in again to continue.',
  },
  csrf_token_invalid: {
    tone: 'warning',
    heading: 'Session check failed',
    message: 'The request was rejected by the security check. Reload the page or sign in again.',
  },
  forbidden: {
    tone: 'warning',
    heading: 'Not allowed',
    message: 'Your account does not have the cluster role needed for this action.',
  },
  setup_token_invalid: {
    tone: 'warning',
    heading: 'Setup token rejected',
    message: 'The setup token is not valid. Use the token from the file written at install time.',
  },
  not_found: {
    tone: 'warning',
    heading: 'Not found',
    message: 'This item does not exist or was removed.',
  },
  method_not_allowed: {
    tone: 'danger',
    heading: 'Request not allowed',
    message:
      'The server does not accept this kind of request here. Reload the page; if it persists, the panel and the server versions do not match.',
  },
  conflict: {
    tone: 'warning',
    heading: 'Conflict',
    message: 'The current state of the cluster does not allow this change.',
    withDetail: true,
  },
  already_exists: {
    tone: 'warning',
    heading: 'Already exists',
    message: 'An item with this name already exists.',
  },
  immutable_field: {
    tone: 'warning',
    heading: 'Cannot be changed',
    message: 'This setting is fixed after creation.',
    withDetail: true,
  },
  rebalance_in_progress: {
    tone: 'warning',
    heading: 'Rebalance in progress',
    message: 'A rebalance is running. Wait until it finishes or stop it first.',
    retryable: true,
  },
  operation_in_progress: {
    tone: 'warning',
    heading: 'Operation in progress',
    message: 'Another operation is already running on this item. Wait for it to finish.',
    retryable: true,
  },
  task_not_cancellable: {
    tone: 'warning',
    heading: 'Cannot cancel',
    message: 'This task cannot be cancelled in its current state.',
  },
  last_admin: {
    tone: 'warning',
    heading: 'Last administrator',
    message:
      'The cluster needs at least one kingres_admin. Assign the role to another account first.',
  },
  role_in_use: {
    tone: 'warning',
    heading: 'Role in use',
    message: 'The role still owns objects or is referenced elsewhere. Reassign them first.',
    withDetail: true,
  },
  optimistic_lock_failed: {
    tone: 'warning',
    heading: 'Changed by someone else',
    message: 'This data changed while you were editing. Reload it and apply your change again.',
    retryable: true,
  },
  no_replica_available: {
    tone: 'warning',
    heading: 'No replica available',
    message:
      'A graceful operation needs a healthy replica and none is available. Use a hard failover only if you accept the risk.',
  },
  instance_already_running: {
    tone: 'info',
    heading: 'Instance already running',
    message: 'The instance is already running or starting, so there is nothing to start.',
  },
  instance_already_stopped: {
    tone: 'info',
    heading: 'Instance already stopped',
    message: 'The instance is already stopped or stopping, so there is nothing to stop or restart.',
  },
  server_owns_buckets: {
    tone: 'warning',
    heading: 'Server still holds data',
    message:
      'The server still holds a copy of at least one bucket. Move the data away first: turn off "should hold buckets" for it, run a rebalance, then try again.',
    withDetail: true,
  },
  server_hosts_map_group: {
    tone: 'warning',
    heading: 'Server hosts the cluster metadata',
    message:
      'The server runs an instance of the map group (shard group 0), which holds the cluster metadata, or it is the manager leader. It cannot leave or be removed while it does.',
    withDetail: true,
  },
  last_server: {
    tone: 'warning',
    heading: 'Last server of the cluster',
    message:
      'This is the only server of the cluster. A cluster cannot be left or emptied this way; keep the server or reset it by hand.',
  },
  server_group_not_empty: {
    tone: 'warning',
    heading: 'Server group still has servers',
    message:
      'Servers are still assigned to this group. Move each of them to another group (or to no group) first, then delete the group.',
    withDetail: true,
  },
  ledger_entry_pending: {
    tone: 'info',
    heading: 'Row not in a block yet',
    message:
      'The ledger has not put this row into a block yet, so it cannot be proven yet. Generate a digest (or wait for the next block) and ask again.',
    retryable: true,
  },
  system_database_stopped: {
    tone: 'warning',
    heading: 'The system database is stopped',
    message:
      'The instance that hosts the cluster system database was stopped on purpose. Every operation except starting the instance is paused until it runs again.',
    retryable: true,
  },
  certificate_revoked: {
    tone: 'warning',
    heading: 'Certificates revoked',
    message:
      'The certificates of this server were revoked, so it gets no new certificate. Remove the server (or let it leave) and join it again with a new setup token.',
    withDetail: true,
  },
  certificate_authority_unavailable: {
    tone: 'danger',
    heading: 'Certificate authority not available',
    message:
      'The manager leader does not hold the certificate authority key, so certificates cannot be issued or revoked right now. Try again when the leader that holds the key is reachable.',
    retryable: true,
  },
  quorum_unavailable: {
    tone: 'danger',
    heading: 'No quorum',
    message: 'Too few servers are reachable to make this change safely.',
    retryable: true,
  },
  unprocessable: {
    tone: 'warning',
    heading: 'Cannot be done',
    message: 'The request is valid but cannot be carried out.',
    withDetail: true,
  },
  sql_error: { tone: 'danger', heading: 'SQL error', message: 'The statement failed.' },
  unsupported_operation: {
    tone: 'info',
    heading: 'Not available on this server version yet',
    message:
      'This server version does not implement this feature yet. It will appear after an upgrade.',
  },
  node_state_invalid: {
    tone: 'warning',
    heading: 'Not possible in this server state',
    message: 'The server is in a state where this action is not allowed.',
    withDetail: true,
  },
  node_uninitialized: {
    tone: 'info',
    heading: 'Server not set up',
    message: 'This server has not been initialized yet. Complete the setup wizard first.',
  },
  map_group_unavailable: {
    tone: 'danger',
    heading: 'Map group unavailable',
    message:
      'The group that holds the cluster metadata is not reachable. Changes are paused until it recovers.',
    retryable: true,
  },
  leader_unavailable: {
    tone: 'danger',
    heading: 'No manager leader',
    message: 'The cluster manager leader cannot be reached, so changes cannot be made right now.',
    retryable: true,
  },
  rate_limited: {
    tone: 'warning',
    heading: 'Too many attempts',
    message: 'Wait a moment and try again.',
    retryable: true,
  },
  service_unavailable: {
    tone: 'danger',
    heading: 'Service unavailable',
    message: 'The server cannot answer right now.',
    retryable: true,
    withDetail: true,
  },
  internal_error: {
    tone: 'danger',
    heading: 'Unexpected server error',
    message: 'Something went wrong on the server. Quote the request id when you report it.',
    retryable: true,
  },
}

const unknownEntry: Entry = {
  tone: 'danger',
  heading: 'Something went wrong',
  message: 'An unexpected error occurred in the panel.',
}

/** Turns any thrown value into text the UI can show. */
export function describeProblem(error: unknown): ProblemView {
  if (!(error instanceof ApiError)) {
    const extra = error instanceof Error ? ` (${error.message})` : ''
    return {
      code: 'internal_error',
      tone: unknownEntry.tone,
      heading: unknownEntry.heading,
      message: unknownEntry.message + extra,
      fieldErrors: {},
      retryable: false,
    }
  }
  const p = error.problem
  // A code this panel does not know (a newer server) still shows the server's own explanation.
  const entry: Entry = entries[p.code] ?? { ...unknownEntry, withDetail: true }
  const fieldErrors: Record<string, string> = {}
  for (const issue of p.errors ?? []) {
    if (!(issue.field in fieldErrors)) fieldErrors[issue.field] = issue.message
  }
  let message = entry.message
  // For SQL errors the server's text is the useful part; elsewhere it is an addition.
  if (p.code === 'sql_error' && p.detail) message = p.detail
  else if (entry.withDetail && p.detail) message = `${entry.message} ${p.detail}`
  return {
    code: p.code,
    tone: entry.tone,
    heading: entry.heading,
    message,
    fieldErrors,
    sqlState: p.sqlState,
    requestId: p.requestId,
    retryable: entry.retryable ?? false,
  }
}

/** The backend answers with this code for operations it has not implemented yet. */
export function isUnsupported(error: unknown): boolean {
  return error instanceof ApiError && error.code === 'unsupported_operation'
}

/** Codes that mean the browser session is no longer valid. */
export function isSessionLost(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    (error.code === 'unauthenticated' || error.code === 'session_expired')
  )
}

/** Retry policy for queries: transient server-side failures only, and only a couple of times. */
export function shouldRetry(failureCount: number, error: unknown): boolean {
  if (failureCount >= 2) return false
  if (!(error instanceof ApiError)) return false
  return (
    error.status >= 500 &&
    error.code !== 'node_uninitialized' &&
    // The server says it is stopped on purpose: asking again changes nothing until it is started.
    error.code !== 'system_database_stopped' &&
    error.code !== 'unsupported_operation'
  )
}
