// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.10.0 (failover started by hand): every new alert kind, audit action and problem code has words, the
// mock answers the four new operations with bodies that satisfy the contract's schemas, a dry run answers a plan, the
// decision about data loss is asked for, and the two operations that start a failover narrow their answer to a task.
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia } from 'pinia'
import { waitFor } from '@testing-library/vue'
import { api } from '@/api/client'
import { endpoints } from '@/api/endpoints'
import { ApiError, describeProblem } from '@/api/problem'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import {
  ALERT_KIND_LABEL,
  alertKindLabel,
  AUDIT_ACTION_LABEL,
  auditActionLabel,
} from '@/lib/status'
import { auditActions } from '@/api/operations.generated'
import type { ProblemCode } from '@/api/types'
import { signInAs } from './helpers'

import { ok, spec } from './schema-check'

const NEW_ALERT_KINDS = [
  'failoverNeedsDecision',
  'failoverBlocked',
  'failoverDataLoss',
  'splitPromotion',
  'oldPrimaryRunning',
  'oldPrimaryNotStopped',
  'oldPrimaryNotRebuilt',
] as const
const NEW_ACTIONS = [
  'resetAutoFailoverCounter',
  'failoverGroupStarted',
  'failoverGroupPromoted',
  'failoverGroupFailed',
  'oldPrimaryRebuilt',
] as const

describe('words of 0.10.0', () => {
  it('every alert kind of the contract has words, and the seven new ones are in the contract', () => {
    const kinds = spec.components.schemas.AlertKind?.enum as string[]
    for (const k of NEW_ALERT_KINDS) expect(kinds).toContain(k)
    for (const k of kinds) {
      expect(ALERT_KIND_LABEL[k as keyof typeof ALERT_KIND_LABEL], k).toBeTruthy()
      expect(alertKindLabel(k), k).not.toBe(k)
    }
  })

  it('every audit action of the contract has words, and the five new ones are in the contract', () => {
    for (const a of NEW_ACTIONS) {
      expect(auditActions).toContain(a)
      expect(AUDIT_ACTION_LABEL[a]).toBeTruthy()
      expect(auditActionLabel(a)).not.toBe(a)
    }
  })

  it('the two new problem codes have a heading and an explanation', () => {
    for (const code of ['failover_needs_decision', 'lease_settings_differ'] as ProblemCode[]) {
      const view = describeProblem(
        new ApiError({ type: 'about:blank', title: code, status: 409, code }),
      )
      expect(view.heading).not.toBe(code)
      expect(view.message.length).toBeGreaterThan(20)
      expect(view.message).not.toContain(code)
    }
  })
})

describe('the mock of the failover operations', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
  })

  /**
   * A server that is primary of an ordinary group only: the real server refuses the map group for now (and so does the
   * mock, unless `?failover=map`), and the server of the map group is the one that has it.
   */
  function victim() {
    return '10.0.0.12'
  }

  it('answers an idle status, an empty list and a missing report as the contract says', async () => {
    const status = await endpoints.getFailoverStatus()
    ok(status, 'FailoverStatus')
    expect(status.state).toBe('idle')
    ok(await endpoints.listFailoverReports(), 'FailoverReportPage')
    await expect(endpoints.getFailoverReport(999)).rejects.toMatchObject({ code: 'not_found' })
  })

  it('answers a dry run with a plan that changes nothing', async () => {
    const id = victim()
    const before = JSON.stringify(getState().groups)
    const plan = await endpoints.previewFailover(id, { mode: 'hard' })
    ok(plan, 'FailoverPlan')
    expect(plan.kind).toBe('failover')
    expect(plan.canProceed).toBe(true)
    expect(plan.groups.length).toBeGreaterThan(0)
    expect(JSON.stringify(getState().groups)).toBe(before)
    expect(getState().tasks.some((t) => t.task.kind === 'server.failover')).toBe(false)
  })

  it('writes a report when a failover finishes; the report is listed and read back', async () => {
    const id = victim()
    const task = await endpoints.failoverServer(id, 'hard')
    expect(task.kind).toBe('server.failover')
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('done'))
    const page = await endpoints.listFailoverReports()
    ok(page, 'FailoverReportPage')
    expect(page.items[0]?.id).toBe(task.id)
    const report = await endpoints.getFailoverReport(task.id)
    ok(report, 'FailoverReport')
    expect(report.serverId).toBe(id)
    const status = await endpoints.getFailoverStatus()
    ok(status, 'FailoverStatus')
    expect(status.lastReport?.id).toBe(task.id)
  })

  it('shows a running failover in the status', async () => {
    const task = await endpoints.failoverServer(victim(), 'hard')
    const status = await endpoints.getFailoverStatus()
    ok(status, 'FailoverStatus')
    expect(status).toMatchObject({ state: 'running', taskId: task.id, kind: 'failover' })
  })

  it('asks for the decision about data loss, and goes on when the request accepts it', async () => {
    applySwitches(new URLSearchParams('failover=dataloss'), getState())
    const id = victim()
    const plan = await endpoints.previewFailover(id, { mode: 'hard' })
    ok(plan, 'FailoverPlan')
    expect(plan.canProceed).toBe(false)
    expect(plan.blockers[0]?.code).toBe('failoverNeedsDecision')
    await expect(endpoints.failoverServer(id, 'hard')).rejects.toMatchObject({
      code: 'failover_needs_decision',
      status: 409,
    })
    const accepted = await endpoints.previewFailover(id, { mode: 'hard', allowDataLoss: true })
    ok(accepted, 'FailoverPlan')
  })

  it('refuses a group the server is not primary of, and a target that is not a standby', async () => {
    const id = victim()
    await expect(
      endpoints.previewFailover(id, { mode: 'hard', shardGroupIds: [99] }),
    ).rejects.toMatchObject({ code: 'unprocessable' })
    await expect(
      endpoints.previewFailover(id, {
        mode: 'hard',
        targets: [{ shardGroupId: 99, serverId: id }],
      }),
    ).rejects.toMatchObject({ code: 'unprocessable' })
  })

  it('resets the counter and answers the cluster settings', async () => {
    const s = getState()
    s.settings.autoFailover = { ...s.settings.autoFailover, eventsSinceReset: 3 }
    const settings = await endpoints.resetAutoFailoverCounter()
    ok(settings, 'ClusterSettings')
    expect(settings.autoFailover.eventsSinceReset).toBe(0)
    expect(getState().audit[0]?.action).toBe('resetAutoFailoverCounter')
  })

  it('answers a dry run of the restore with a plan and a request without one with a task', async () => {
    const id = victim()
    await endpoints
      .failoverServer(id, 'hard')
      .then((t) => waitFor(async () => expect((await endpoints.getTask(t.id)).state).toBe('done')))
    const res = await api.POST('/replication/restore-preferred-primaries', {
      body: { dryRun: true },
    })
    expect(res.response.status).toBe(200)
    ok(res.data, 'FailoverPlan')
    expect(res.data).toMatchObject({ kind: 'restorePreferredPrimaries', mode: 'restore' })
    const task = await endpoints.restorePreferredPrimaries()
    expect(task.kind).toBe('replication.restorePreferredPrimaries')
  })
})
