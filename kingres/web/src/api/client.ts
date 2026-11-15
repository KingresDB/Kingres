// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// The typed REST client. Types come from the generated schema; this file adds the session
// concerns (CSRF header, setup token), the one place where path parameters are encoded, the
// cancelling of requests when the session ends, and the conversion of failures to ApiError.
import createClient, { defaultPathSerializer, type Middleware } from 'openapi-fetch'
import type { paths } from './schema'
import { ApiError, isProblem, isSessionLost, networkError } from './problem'
import {
  registerServerStateReset,
  reportSessionLost,
  reportSystemDatabaseStopped,
} from './server-state'
import { isAddressableSegment } from '@/lib/path-segment'

export const API_BASE_PATH = '/api/v1'

/** CSRF token of the current session; the session store sets it after sign-in. */
let csrfToken: string | null = null
/** One-time setup token, held in memory only while the setup wizard is open. */
let setupToken: string | null = null

export function setCsrfToken(token: string | null): void {
  csrfToken = token
}

export function setSetupToken(token: string | null): void {
  setupToken = token
}

const MUTATING = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])

const sessionHeaders: Middleware = {
  onRequest({ request }) {
    if (csrfToken && MUTATING.has(request.method)) request.headers.set('X-CSRF-Token', csrfToken)
    if (setupToken) request.headers.set('X-Setup-Token', setupToken)
    return request
  },
}

/** Absolute base URL: the Node-based test runner cannot resolve relative URLs. */
function baseUrl(): string {
  const origin = typeof location === 'undefined' ? 'http://localhost' : location.origin
  return `${origin}${API_BASE_PATH}`
}

/** Requests that have been sent and have not answered yet; they are cancelled when a session ends. */
const inFlight = new Set<AbortController>()

/** Cancels every request in flight, so that a late answer can never reach the next user. */
export function abortInFlightRequests(): void {
  for (const controller of [...inFlight]) controller.abort()
  inFlight.clear()
}
registerServerStateReset(abortInFlightRequests)

/**
 * Serializes path parameters. Every value is percent-encoded as one path segment, so that a name
 * with `/`, `?`, `#` or `%` in it addresses exactly that object. A value that no URL can carry
 * (`.`, `..` and the empty string are removed or collapsed by the URL parser, also when written
 * `%2E`) is refused: sending it would address a different object than the one shown.
 */
function pathSerializer(pathname: string, params: Record<string, unknown>): string {
  for (const value of Object.values(params)) {
    if (typeof value === 'string' && !isAddressableSegment(value)) {
      throw new ApiError({
        type: 'about:blank',
        title: 'This name cannot be used in a request',
        status: 400,
        code: 'bad_request',
        detail: `The name ${JSON.stringify(value)} cannot be sent in a URL path, so no request was made.`,
      })
    }
  }
  return defaultPathSerializer(pathname, params)
}

export const api = createClient<paths>({
  baseUrl: baseUrl(),
  credentials: 'same-origin',
  pathSerializer,
  // Looked up at call time so that tests and the mock service worker can replace global fetch.
  fetch: async (request) => {
    // One controller per request: the session ending aborts it, and so does the caller's own signal.
    const controller = new AbortController()
    const fromCaller = () => controller.abort(request.signal.reason)
    if (request.signal.aborted) fromCaller()
    else request.signal.addEventListener('abort', fromCaller, { once: true })
    inFlight.add(controller)
    try {
      return await globalThis.fetch(new Request(request, { signal: controller.signal }))
    } catch (cause) {
      throw networkError(cause)
    } finally {
      request.signal.removeEventListener('abort', fromCaller)
      inFlight.delete(controller)
    }
  },
})
api.use(sessionHeaders)

interface Result<T> {
  data?: T
  error?: unknown
  response: Response
}

/** Problem code for an answer that is not a problem document, from its HTTP status. */
function codeForStatus(status: number): ApiError['problem']['code'] {
  if (status === 401) return 'unauthenticated'
  if (status === 403) return 'forbidden'
  if (status === 404) return 'not_found'
  if (status === 405) return 'method_not_allowed'
  if (status === 429) return 'rate_limited'
  if ([502, 503, 504].includes(status)) return 'service_unavailable'
  return status >= 500 ? 'internal_error' : 'bad_request'
}

/**
 * Awaits an openapi-fetch call and returns its data, or throws an ApiError.
 * Every query, mutation and task poll goes through this, so components never see raw responses,
 * and an answer that says "the session is gone" is noticed in one place whoever asked.
 */
export async function unwrap<T>(call: Promise<Result<T>>): Promise<T> {
  let result: Result<T>
  try {
    result = await call
  } catch (cause) {
    throw cause instanceof ApiError ? cause : networkError(cause)
  }
  if (result.response.ok) return result.data as T
  const error = isProblem(result.error)
    ? new ApiError(result.error)
    : new ApiError({
        type: 'about:blank',
        title: result.response.statusText || 'Unexpected response',
        status: result.response.status >= 400 ? result.response.status : 500,
        // A proxy answers with its own page (not a problem document) when the server is down or
        // when it ends the session itself.
        code: codeForStatus(result.response.status),
        detail: 'The server answered with something that is not a problem document.',
      })
  if (isSessionLost(error)) reportSessionLost()
  if (error.code === 'system_database_stopped') reportSystemDatabaseStopped()
  throw error
}
