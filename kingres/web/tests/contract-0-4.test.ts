// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
// @vitest-environment node
//
// Guards for revision 0.4.0 of the contract: every problem code has words, the mock answers with the
// contract's statuses, and the mock's tasks and servers carry the properties 0.4.0 made required.
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { parse } from 'yaml'
import { operations } from '@/api/operations.generated'
import { ApiError, describeProblem } from '@/api/problem'
import { MockProblem, getState, resetMockState } from '@/mocks/http'
import { createTask, serverView } from '@/mocks/state'
import type { ProblemCode } from '@/api/types'

const root = fileURLToPath(new URL('..', import.meta.url))
// The same file the generator reads: KINGRES_API_SPEC, or the working tree of the contract.
const specFile = process.env.KINGRES_API_SPEC ?? join(root, '../api/openapi.yaml')
interface Spec {
  info: { version: string }
  components: {
    schemas: Record<string, { enum?: string[]; required?: string[] }>
  }
}
const spec = parse(readFileSync(specFile, 'utf8')) as Spec
const codes = spec.components.schemas.ProblemCode!.enum as ProblemCode[]

describe('contract 0.4.0', () => {
  // The working tree of the contract moved on to a 0.5.0 draft (two certificate operations, two
  // problem codes) while this panel was adapted; 0.4.0 has 86 operations, the draft 88.
  // 0.7.0 added one operation (cancelStatement): 89.
  it('is revision 0.4.0 (86 operations) or a later one (88, from 0.7.0 89)', () => {
    expect([
      '0.4.0',
      '0.5.0',
      '0.6.0',
      '0.7.0',
      '0.8.0',
      '0.9.0',
      '0.10.0',
      '0.11.0',
      '0.12.0',
      '0.13.0',
      '0.14.0',
      '0.15.0',
      '0.16.0',
      '0.17.0',
      '0.18.0',
      '0.19.0',
      '0.24.0',
      '0.25.0',
      '0.26.0',
      '0.27.0',
      '0.28.0',
      '0.29.0',
      '0.30.0',
      '0.31.0',
      '0.32.0',
    ]).toContain(spec.info.version)
    // 0.8.0 added the eleven operations of the Configuration tag: 100. 0.10.0 added four (getFailoverStatus,
    // listFailoverReports, getFailoverReport, resetAutoFailoverCounter): 104. 0.11.0 added the 28 operations of the ledger of
    // phase 7 (signing, publication, anchoring, aggregates, bundles, client keys, divergence): 132.
    const operationsOf: Record<string, number> = {
      '0.4.0': 86,
      '0.5.0': 88,
      '0.6.0': 88,
      '0.7.0': 89,
      '0.8.0': 100,
      '0.9.0': 100,
      '0.10.0': 104,
      '0.11.0': 132,
      '0.12.0': 132,
      '0.13.0': 132,
      '0.14.0': 140,
      '0.15.0': 149,
      '0.16.0': 149,
      '0.17.0': 160,
      '0.18.0': 161,
      '0.19.0': 171,
      '0.24.0': 171,
      '0.25.0': 171,
      '0.26.0': 174,
      '0.27.0': 174,
      '0.28.0': 174,
      '0.29.0': 176,
      '0.30.0': 177,
      '0.31.0': 177,
      '0.32.0': 178,
    }
    expect(Object.keys(operations)).toHaveLength(operationsOf[spec.info.version]!)
  })

  it('every problem code has a heading and an explanation, and none shows as the raw code', () => {
    expect(codes).toContain('system_database_stopped')
    for (const code of codes) {
      const view = describeProblem(
        new ApiError({ type: 'about:blank', title: code, status: 400, code }),
      )
      expect(view.heading.length, code).toBeGreaterThan(3)
      expect(view.message.length, code).toBeGreaterThan(20)
      expect(view.heading, code).not.toBe(code)
      expect(view.message, code).not.toContain(code)
    }
  })

  it('the mock answers with the contract statuses of the new codes', () => {
    const want: Record<string, number> = {
      instance_already_running: 409,
      instance_already_stopped: 409,
      server_owns_buckets: 409,
      server_hosts_map_group: 409,
      last_server: 409,
      server_group_not_empty: 409,
      ledger_entry_pending: 409,
      no_replica_available: 409,
      system_database_stopped: 503,
      unsupported_operation: 422,
      failover_needs_decision: 409,
      lease_settings_differ: 409,
    }
    for (const [code, status] of Object.entries(want))
      expect(new MockProblem(code as ProblemCode).toProblem('r').status, code).toBe(status)
    // Every code of the contract has a status in the mock.
    for (const code of codes)
      expect(new MockProblem(code).toProblem('r').status, code).toBeGreaterThanOrEqual(400)
  })

  it('mock tasks carry the required properties of Task', () => {
    resetMockState()
    const s = getState()
    const t = createTask(s, {
      kind: 'database.create',
      subject: null,
      steps: ['a'],
      apply: () => undefined,
    })
    for (const key of spec.components.schemas.Task!.required ?? [])
      expect(t, key).toHaveProperty(key)
    expect(t.detail).toBeNull()
    for (const rec of s.tasks)
      for (const key of spec.components.schemas.Task!.required ?? [])
        expect(rec.task, key).toHaveProperty(key)
  })

  it('mock servers carry the required properties of Server, including the certificate ones', () => {
    resetMockState()
    const s = getState()
    for (const rec of s.servers) {
      const view = serverView(s, rec as never)
      for (const key of spec.components.schemas.Server!.required ?? [])
        expect(view, key).toHaveProperty(key)
      if (!view.seenFingerprint) expect(view.fingerprintPinned).toBe(false)
    }
  })
})
