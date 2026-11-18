// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The mock states of the failover console (design 8.7): every URL switch reaches its state, and what the mock answers in
// it satisfies the schemas of the contract. The mock is the contract's executable form, so the console is built against
// answers that the real server can give, and it refuses what the real server refuses for now. T20.
import { beforeEach, describe, expect, it } from 'vitest'
import { waitFor } from '@testing-library/vue'
import { createPinia } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { FAIL_STEPS, resetFailoverState } from '@/mocks/failover'
import { ok } from './schema-check'
import { signInAs } from './helpers'

const sw = (text: string) => applySwitches(new URLSearchParams(`failover=${text}`), getState())
const settle = (id: number, state: 'done' | 'failed' | 'paused') =>
  waitFor(async () => expect((await endpoints.getTask(id)).state).toBe(state))

describe('the states of the failover in the mock', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
  })

  it('refuses what the real server refuses for now: graceful and the map group, with the problem code', async () => {
    await expect(
      endpoints.previewFailover('10.0.0.12', { mode: 'graceful' }),
    ).rejects.toMatchObject({
      code: 'unsupported_operation',
      status: 422,
    })
    await expect(endpoints.previewFailover('10.0.0.11', { mode: 'hard' })).rejects.toMatchObject({
      code: 'unsupported_operation',
    })
    await expect(endpoints.failoverServer('10.0.0.11', 'hard')).rejects.toMatchObject({
      code: 'unsupported_operation',
    })
    // The groups can be named, so the server that has the map group can still lose its other group.
    ok(
      await endpoints.previewFailover('10.0.0.11', { mode: 'hard', shardGroupIds: [1] }),
      'FailoverPlan',
    )
  })

  it('failover=graceful and failover=map turn on what the real server does not do yet', async () => {
    sw('graceful,map')
    const plan = await endpoints.previewFailover('10.0.0.11', { mode: 'graceful' })
    ok(plan, 'FailoverPlan')
    expect(plan.groups[0]).toMatchObject({
      shardGroupId: 0,
      isMapGroup: true,
      movesTheManager: true,
    })
    expect(plan.groups[0]?.writesPausedSeconds).toBeGreaterThan(0)
    // A graceful failover needs a live server.
    getState().servers[1]!.status = 'dead'
    await expect(
      endpoints.previewFailover('10.0.0.12', { mode: 'graceful' }),
    ).rejects.toMatchObject({
      code: 'node_state_invalid',
    })
  })

  it('failover=dataloss: the plan says possible and the request is refused until it accepts', async () => {
    sw('dataloss')
    const plan = await endpoints.previewFailover('10.0.0.12', { mode: 'hard' })
    ok(plan, 'FailoverPlan')
    expect(plan.groups[0]?.dataLoss).toMatchObject({
      verdict: 'possible',
      groupRanWithoutSynchronousStandby: true,
    })
    expect(plan.groups[0]?.candidates[0]?.syncState).toBe('async')
    await expect(endpoints.failoverServer('10.0.0.12', 'hard')).rejects.toMatchObject({
      code: 'failover_needs_decision',
      status: 409,
    })
  })

  it('failover=needs-decision: the task pauses at "Choose the standby", the status says what it waits for', async () => {
    sw('needs-decision')
    const task = await endpoints.failoverServer('10.0.0.12', 'hard')
    await settle(task.id, 'paused')
    const t = await endpoints.getTask(task.id)
    ok(t, 'Task')
    expect(t.progress.currentStep).toBe('Group 2: Choose the standby')
    expect(t.cancellable).toBe(true)
    // Steps carry their group (0.10.0) and the paused task shows the step it will go on with as pending.
    expect(t.progress.steps.find((s) => s.name === 'Group 2: Choose the standby')).toMatchObject({
      state: 'pending',
      shardGroupId: 2,
    })
    const status = await endpoints.getFailoverStatus()
    ok(status, 'FailoverStatus')
    expect(status).toMatchObject({
      state: 'paused',
      taskId: task.id,
      trigger: 'manual',
      kind: 'failover',
    })
    expect(status.pausedReason).toBeTruthy()
    expect(status.needsDecision[0]).toMatchObject({
      code: 'failoverNeedsDecision',
      shardGroupId: 2,
    })
    expect(status.groups[0]).toMatchObject({ shardGroupId: 2, state: 'fenced' })
    const report = await endpoints.getFailoverReport(task.id)
    ok(report, 'FailoverReport')
    expect(report.outcome).toBe('paused')
    // A repeat of the request without the decision is refused; with it the task goes on.
    await expect(endpoints.failoverServer('10.0.0.12', 'hard')).rejects.toMatchObject({
      code: 'failover_needs_decision',
    })
    const resumed = await endpoints.failoverServer('10.0.0.12', {
      mode: 'hard',
      allowDataLoss: true,
    })
    expect(resumed.id).toBe(task.id)
    await settle(task.id, 'done')
    ok(await endpoints.getFailoverReport(task.id), 'FailoverReport')
    expect((await endpoints.getFailoverReport(task.id)).outcome).toBe('completed')
  })

  for (const [what, code, status] of [
    ['rebalance', 'rebalance_in_progress', 409],
    ['lease', 'lease_settings_differ', 409],
    ['quorum', 'quorum_unavailable', 503],
    ['noreplica', 'no_replica_available', 409],
  ] as const) {
    it(`failover=blocked:${what}: ${code}, and the cards say so`, async () => {
      sw(`blocked:${what}`)
      if (what === 'quorum')
        await expect(
          endpoints.previewFailover('10.0.0.12', { mode: 'hard' }),
        ).rejects.toMatchObject({
          code,
          status,
        })
      else {
        const plan = await endpoints.previewFailover('10.0.0.12', { mode: 'hard' })
        ok(plan, 'FailoverPlan')
        expect(plan.canProceed).toBe(false)
        expect(plan.blockers.some((b) => b.code === code)).toBe(true)
      }
      await expect(endpoints.failoverServer('10.0.0.12', 'hard')).rejects.toMatchObject({
        code,
        status,
      })
      const topology = await endpoints.getReplicationTopology()
      ok(topology, 'ReplicationTopology')
      for (const g of topology.groups) {
        expect(g.failover?.possible).toBe(false)
        expect(g.failover?.blockers[0]?.code).toBe(code)
      }
    })
  }

  for (const step of FAIL_STEPS.filter((s) => s !== 'verify' && s !== 'pause')) {
    it(`failover=fail:${step}: the task ends failed at that step, with a code, and a report says where`, async () => {
      sw(`fail:${step}`)
      const task = await endpoints.failoverServer('10.0.0.12', 'hard')
      await settle(task.id, 'failed')
      const t = await endpoints.getTask(task.id)
      ok(t, 'Task')
      expect(t.error?.code).toBeTruthy()
      expect(t.error?.step).toBeTruthy()
      expect(t.progress.steps.filter((s) => s.state === 'failed')).toHaveLength(1)
      const status = await endpoints.getFailoverStatus()
      ok(status, 'FailoverStatus')
      expect(status.state).toBe('failed')
      const report = await endpoints.getFailoverReport(task.id)
      ok(report, 'FailoverReport')
      expect(['failed', 'partial']).toContain(report.outcome)
    })
  }

  it('failover=slow: steps take about three times as long, and the run can be cancelled in the middle', async () => {
    mockConfig.taskDurationMs = 20_000
    sw('slow')
    const task = await endpoints.failoverServer('10.0.0.12', 'hard')
    const rec = getState().tasks.find((t) => t.task.id === task.id)!
    expect(rec.durationMs).toBe(60_000)
    const status = await endpoints.getFailoverStatus()
    ok(status, 'FailoverStatus')
    expect(status).toMatchObject({ state: 'running', cancellable: true })
    expect(status.groups[0]?.state).toBe('pending')
    const cancelled = await endpoints.cancelTask(task.id)
    expect(cancelled.error?.code).toBe('cancelled')
    expect((await endpoints.getFailoverReport(task.id)).outcome).toBe('cancelled')
  })

  it('the status of a run goes through the phases and the states of its group, and cancel is refused after the promotion', async () => {
    mockConfig.taskDurationMs = 60_000
    const task = await endpoints.failoverServer('10.0.0.12', 'hard')
    const rec = getState().tasks.find((t) => t.task.id === task.id)!
    const seen: string[] = []
    // Steps: check, then the nine of group 2, then the report: eleven in all.
    for (const at of [0.5, 3.5, 4.5, 5.5, 6.5, 7.5, 8.5, 9.5, 10.5]) {
      rec.startedAt = Date.now() - (at / 11) * rec.durationMs
      const status = await endpoints.getFailoverStatus()
      ok(status, 'FailoverStatus')
      seen.push(`${status.phase}/${status.groups[0]?.state}`)
      if (at > 6) expect(status.cancellable).toBe(false)
    }
    expect(seen).toEqual([
      'checking/pending',
      'waitingForLease/fenced',
      'choosing/fenced',
      'promoting/pinned',
      'promoting/pinned',
      'followingCatalog/promoted',
      'repointing/promoted',
      'finishing/promoted',
      'finishing/done',
    ])
    await expect(endpoints.cancelTask(task.id)).rejects.toMatchObject({
      code: 'task_not_cancellable',
    })
  })

  it('failover=graceful with switchover:behind: the verification fails, the report says standby_behind', async () => {
    sw('switchover:behind')
    const task = await endpoints.failoverServer('10.0.0.12', 'graceful')
    await settle(task.id, 'failed')
    const t = await endpoints.getTask(task.id)
    expect(t.error).toMatchObject({ code: 'standby_behind', step: 'Group 2: Verify the standby' })
    ok(await endpoints.getFailoverReport(task.id), 'FailoverReport')
    // The old primary leads again: nothing was promoted.
    expect(getState().groups.find((g) => g.id === 2)?.primaryServerId).toBe('10.0.0.12')
  })

  it('failover=graceful: writes are paused, and the status says since when', async () => {
    mockConfig.taskDurationMs = 60_000
    sw('graceful')
    const task = await endpoints.failoverServer('10.0.0.12', 'graceful')
    const rec = getState().tasks.find((t) => t.task.id === task.id)!
    // Steps: check, then the seven of group 2, then the report: nine; at 2.5 the group waits for open transactions.
    rec.startedAt = Date.now() - (2.5 / 9) * rec.durationMs
    const status = await endpoints.getFailoverStatus()
    ok(status, 'FailoverStatus')
    expect(status.phase).toBe('waitingForTransactions')
    expect(status.groups[0]?.writesPausedSince).toBeTruthy()
  })

  it('failover=map: the manager moves first, the run has no task id until the catalog follows', async () => {
    mockConfig.taskDurationMs = 60_000
    sw('map')
    const task = await endpoints.failoverServer('10.0.0.11', { mode: 'hard' })
    const rec = getState().tasks.find((t) => t.task.id === task.id)!
    // Check, group 0 (nine steps), group 1 (nine steps), report: twenty in all; step 5 of group 0 is at 5.5.
    rec.startedAt = Date.now() - (5.5 / 20) * rec.durationMs
    let status = await endpoints.getFailoverStatus()
    ok(status, 'FailoverStatus')
    expect(status.taskId).toBeNull()
    expect(status.phase).toBe('bootstrap')
    expect(status.groups[0]).toMatchObject({ shardGroupId: 0, phase: 'bootstrap' })
    rec.startedAt = Date.now() - (9.5 / 20) * rec.durationMs
    status = await endpoints.getFailoverStatus()
    expect(status.taskId).toBe(task.id)
    expect(status.phase).not.toBe('bootstrap')
  })

  for (const [old, state] of [
    ['rebuilding', 'rebuilding'],
    ['waiting', 'waitingForServer'],
    ['running', 'failed'],
    ['failed', 'failed'],
  ] as const) {
    it(`failover=old:${old}: the report says ${state}`, async () => {
      sw(`old:${old}`)
      const task = await endpoints.failoverServer('10.0.0.12', 'hard')
      await settle(task.id, 'done')
      const report = await endpoints.getFailoverReport(task.id)
      ok(report, 'FailoverReport')
      expect(report.groups[0]?.oldPrimary.state).toBe(state)
      if (old === 'rebuilding')
        expect(report.groups[0]?.oldPrimary).toMatchObject({
          method: 'pgRewind',
          rewindBytes: 12 * 1024 * 1024,
        })
    })
  }

  it('failover=auto:<server>: an automatic failover is under way, started by the system', async () => {
    mockConfig.taskDurationMs = 60_000
    sw('auto:10.0.0.12')
    const status = await endpoints.getFailoverStatus()
    ok(status, 'FailoverStatus')
    expect(status).toMatchObject({ state: 'running', trigger: 'automatic', serverId: '10.0.0.12' })
    expect(getState().tasks[0]?.task.createdBy).toBe('system')
  })

  it('failover=limit: the counter is at its maximum, in the overview and in the settings', async () => {
    sw('limit')
    const cluster = await endpoints.getCluster()
    ok(cluster, 'ClusterOverview')
    expect(cluster.autoFailover).toMatchObject({
      limitReached: true,
      eventsSinceReset: 1,
      maxCount: 1,
    })
    expect(cluster.autoFailover.lastEventAt).toBeTruthy()
    const settings = await endpoints.getClusterSettings()
    ok(settings, 'ClusterSettings')
    expect(settings.autoFailover.eventsSinceReset).toBe(1)
    expect((await endpoints.resetAutoFailoverCounter()).autoFailover.eventsSinceReset).toBe(0)
    expect((await endpoints.getCluster()).autoFailover.limitReached).toBe(false)
  })

  it('values can be combined, and a value that is not known changes nothing', async () => {
    sw('slow,old:waiting,nonsense,fail:nothing,blocked:nowhere')
    expect(mockConfig.failoverSlow).toBe(true)
    expect(mockConfig.failoverOld).toBe('waiting')
    expect(mockConfig.failoverFailStep).toBeNull()
    expect(mockConfig.failoverBlock).toBeNull()
  })

  it('the readiness of a group is in the topology and follows what the mock is told', async () => {
    const before = await endpoints.getReplicationTopology()
    ok(before, 'ReplicationTopology')
    for (const g of before.groups) {
      expect(g.failover).toMatchObject({ possible: true, dataLossVerdict: 'none', blockers: [] })
      expect(g.failover?.candidateServerId).toBe(g.standbys[0]?.serverId)
    }
    mockConfig.failoverReadiness = {
      0: 'possibleLoss',
      1: 'unknownLoss',
      2: 'noneReady',
      3: 'versionsDiffer',
    }
    const after = await endpoints.getReplicationTopology()
    ok(after, 'ReplicationTopology')
    expect(
      after.groups.map((g) => g.failover?.dataLossVerdict ?? g.failover?.blockers[0]?.code),
    ).toEqual(['possible', 'unknown', 'no_replica_available', 'version_mismatch'])
    mockConfig.failoverReadiness = {}
    // While a failover runs no group is ready.
    mockConfig.taskDurationMs = 60_000
    await endpoints.failoverServer('10.0.0.12', 'hard')
    const during = await endpoints.getReplicationTopology()
    for (const g of during.groups)
      expect(g.failover?.blockers[0]?.code).toBe('failover_in_progress')
  })

  it('a second failover while one runs is refused, and the cluster can be put back for another try', async () => {
    mockConfig.taskDurationMs = 60_000
    await endpoints.failoverServer('10.0.0.12', 'hard')
    await expect(endpoints.failoverServer('10.0.0.13', 'hard')).rejects.toMatchObject({
      code: 'failover_in_progress',
    })
    resetFailoverState(getState())
    expect((await endpoints.getFailoverStatus()).state).toBe('idle')
    expect(getState().groups.find((g) => g.id === 2)?.standbyServerIds).toEqual(['10.0.0.13'])
  })
})
