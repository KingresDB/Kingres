// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
import { describe, expect, it } from 'vitest'
import { ApiError, describeProblem, isUnsupported, networkError, shouldRetry } from '@/api/problem'
import type { Problem, ProblemCode } from '@/api/types'

const ALL_CODES: ProblemCode[] = [
  'bad_request',
  'validation_failed',
  'unauthenticated',
  'invalid_credentials',
  'session_expired',
  'csrf_token_invalid',
  'forbidden',
  'setup_token_invalid',
  'not_found',
  'method_not_allowed',
  'conflict',
  'already_exists',
  'immutable_field',
  'rebalance_in_progress',
  'operation_in_progress',
  'task_not_cancellable',
  'last_admin',
  'role_in_use',
  'optimistic_lock_failed',
  'no_replica_available',
  'quorum_unavailable',
  'unprocessable',
  'sql_error',
  'unsupported_operation',
  'node_state_invalid',
  'node_uninitialized',
  'map_group_unavailable',
  'leader_unavailable',
  'rate_limited',
  'service_unavailable',
  'internal_error',
]

function problem(code: ProblemCode, extra: Partial<Problem> = {}): ApiError {
  return new ApiError({ type: 'about:blank', title: code, status: 400, code, ...extra })
}

describe('describeProblem', () => {
  it('has a heading and a message for every code in the contract', () => {
    for (const code of ALL_CODES) {
      const view = describeProblem(problem(code))
      expect(view.code).toBe(code)
      expect(view.heading.length, code).toBeGreaterThan(3)
      expect(view.message.length, code).toBeGreaterThan(10)
    }
  })

  it('shows "not available yet" as calm information, not as a failure', () => {
    const view = describeProblem(problem('unsupported_operation', { status: 422 }))
    expect(view.tone).toBe('info')
    expect(view.heading).toMatch(/not available on this server version yet/i)
    expect(isUnsupported(problem('unsupported_operation'))).toBe(true)
    expect(isUnsupported(problem('not_found'))).toBe(false)
  })

  it('maps field errors of validation_failed by field path (first message wins)', () => {
    const view = describeProblem(
      problem('validation_failed', {
        errors: [
          { field: 'sharding.bucketCount', message: 'too big' },
          { field: 'sharding.bucketCount', message: 'ignored' },
          { field: 'name', message: 'bad name' },
        ],
      }),
    )
    expect(view.fieldErrors).toEqual({ 'sharding.bucketCount': 'too big', name: 'bad name' })
  })

  it('uses the server text for SQL errors and keeps SQLSTATE and request id', () => {
    const view = describeProblem(
      problem('sql_error', {
        detail: 'relation "x" does not exist',
        sqlState: '42P01',
        requestId: 'r-1',
        status: 422,
      }),
    )
    expect(view.message).toBe('relation "x" does not exist')
    expect(view.sqlState).toBe('42P01')
    expect(view.requestId).toBe('r-1')
  })

  it('appends the detail for codes that need it and offers retry only for transient ones', () => {
    const conflict = describeProblem(problem('conflict', { detail: 'Server has buckets.' }))
    expect(conflict.message).toContain('Server has buckets.')
    expect(describeProblem(problem('leader_unavailable')).retryable).toBe(true)
    expect(describeProblem(problem('forbidden')).retryable).toBe(false)
  })

  it('describes errors that are not ApiError without throwing', () => {
    const view = describeProblem(new TypeError('boom'))
    expect(view.tone).toBe('danger')
    expect(view.message).toContain('boom')
    expect(describeProblem('text').heading).toBeTruthy()
  })

  it('turns a network failure into a service_unavailable problem', () => {
    const err = networkError(new TypeError('Failed to fetch'))
    expect(err.code).toBe('service_unavailable')
    expect(describeProblem(err).message).toContain('Failed to fetch')
  })
})

describe('shouldRetry', () => {
  it('retries transient server errors at most twice and never client errors', () => {
    const server = problem('internal_error', { status: 500 })
    expect(shouldRetry(0, server)).toBe(true)
    expect(shouldRetry(2, server)).toBe(false)
    expect(shouldRetry(0, problem('not_found', { status: 404 }))).toBe(false)
    expect(shouldRetry(0, problem('unsupported_operation', { status: 422 }))).toBe(false)
    expect(shouldRetry(0, new Error('x'))).toBe(false)
  })
})
