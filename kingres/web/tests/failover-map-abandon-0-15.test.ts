// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.15.0: the operation that gives up the bootstrap of the map group. The mock answers a body that satisfies the contract's schema, says that no bootstrap
// is in progress when none is, refuses without a reason, and its audit action has words.
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { AUDIT_ACTION_LABEL, auditActionLabel } from '@/lib/status'
import { auditActions, operations } from '@/api/operations.generated'
import { signInAs } from './helpers'
import { ok } from './schema-check'

describe('abandonMapGroupBootstrap (0.15.0)', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
  })

  it('is an operation of kingres_admin, and its audit action has words', () => {
    expect(operations.abandonMapGroupBootstrap).toMatchObject({
      method: 'POST',
      path: '/failover/map/abandon',
      role: 'kingres_admin',
    })
    expect(auditActions).toContain('abandonMapGroupBootstrap')
    expect(AUDIT_ACTION_LABEL.abandonMapGroupBootstrap).toBeTruthy()
    expect(auditActionLabel('abandonMapGroupBootstrap')).not.toBe('abandonMapGroupBootstrap')
  })

  it('answers abandoned: false when no bootstrap is in progress, and records the action', async () => {
    const r = await endpoints.abandonMapGroupBootstrap('a test')
    ok(r, 'AbandonMapGroupBootstrapResult')
    expect(r.abandoned).toBe(false)
    expect(getState().audit[0]?.action).toBe('abandonMapGroupBootstrap')
  })

  it('refuses a request without a reason', async () => {
    await expect(endpoints.abandonMapGroupBootstrap('')).rejects.toMatchObject({
      code: 'unprocessable',
    })
  })
})
