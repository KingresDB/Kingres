// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// The typed REST client. Types come from the generated schema; this file adds the session
// concerns (CSRF header, setup token) and converts failures to ApiError.
import createClient, { type Middleware } from 'openapi-fetch'
import type { paths } from './schema'
import { ApiError, isProblem, networkError } from './problem'

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

export const api = createClient<paths>({
  baseUrl: baseUrl(),
  credentials: 'same-origin',
  // Looked up at call time so that tests and the mock service worker can replace global fetch.
  fetch: async (request) => {
    try {
      return await globalThis.fetch(request)
    } catch (cause) {
      throw networkError(cause)
    }
  },
})
api.use(sessionHeaders)

interface Result<T> {
  data?: T
  error?: unknown
  response: Response
}

/**
 * Awaits an openapi-fetch call and returns its data, or throws an ApiError.
 * Every query and mutation goes through this, so components never see raw responses.
 */
export async function unwrap<T>(call: Promise<Result<T>>): Promise<T> {
  let result: Result<T>
  try {
    result = await call
  } catch (cause) {
    throw cause instanceof ApiError ? cause : networkError(cause)
  }
  if (result.response.ok) return result.data as T
  if (isProblem(result.error)) throw new ApiError(result.error)
  throw new ApiError({
    type: 'about:blank',
    title: result.response.statusText || 'Unexpected response',
    status: result.response.status >= 400 ? result.response.status : 500,
    // A proxy answers 502/503/504 with its own page when the server is down.
    code: [502, 503, 504].includes(result.response.status)
      ? 'service_unavailable'
      : result.response.status >= 500
        ? 'internal_error'
        : 'bad_request',
    detail: 'The server answered with something that is not a problem document.',
  })
}
