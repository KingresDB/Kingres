// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The failover of contract 0.10.0 in the mock (design `ha-phase5-part2.md`, 8.7): plans, runs with one block of steps per
// group, the pause for a decision, refusals, a failure at a step, a cancel, the map group's bootstrap, the state of the
// old primary, and a report for every run that is paused, failed, cancelled or done. The mock is the contract's
// executable form: what the mock refuses by default (the planned switchover, the map group, which the real server does) it refuses here too,
// unless a URL switch (switches.ts) turns it on for trying the console that will need it.
import type { components } from '@/api/schema'
import type { Task } from '@/api/types'
import { problem } from './http'
import {
  createMockState,
  createTask,
  mockConfig,
  nowIso,
  raise,
  tickTasks,
  type FailoverRun,
  type GroupRec,
  type MockState,
  type TaskRec,
} from './state'

type S = components['schemas']
type GroupState = S['FailoverGroupProgress']['state']

export const HARD_STEPS = [
  'Freeze the group',
  'Stop the old primary',
  'Wait for the old lease',
  'Choose the standby',
  'Hand the lease to the new server',
  'Promote the standby',
  'Make the catalog follow',
  'Point the other standbys at the new primary',
  'Lift the freeze',
]
export const GRACEFUL_STEPS = [
  'Pause writes',
  'Wait for open transactions',
  'Stop the old primary',
  'Verify the standby',
  'Promote the standby',
  'Make the catalog follow',
  'Lift the freeze',
]
const CHECK = 'Check that the failover can start'
const REPORT = 'Write the report'

/** The phase while a step runs, and the state of the group once it is over. */
const STEP: Record<string, { phase: string; after: GroupState }> = {
  'Freeze the group': { phase: 'checking', after: 'frozen' },
  'Stop the old primary': { phase: 'checking', after: 'fenced' },
  'Wait for the old lease': { phase: 'waitingForLease', after: 'fenced' },
  'Choose the standby': { phase: 'choosing', after: 'pinned' },
  'Hand the lease to the new server': { phase: 'promoting', after: 'pinned' },
  'Promote the standby': { phase: 'promoting', after: 'promoted' },
  'Make the catalog follow': { phase: 'followingCatalog', after: 'promoted' },
  'Point the other standbys at the new primary': { phase: 'repointing', after: 'promoted' },
  'Lift the freeze': { phase: 'finishing', after: 'done' },
  'Pause writes': { phase: 'quiescing', after: 'frozen' },
  'Wait for open transactions': { phase: 'waitingForTransactions', after: 'frozen' },
  'Verify the standby': { phase: 'verifying', after: 'pinned' },
}
/** What a person writes in `?failover=fail:<step>`, and the error code the task then carries. */
const STEP_ALIAS: Record<string, { step: string; code: string }> = {
  check: { step: CHECK, code: 'quorum_unavailable' },
  freeze: { step: 'Freeze the group', code: 'superseded' },
  stop: { step: 'Stop the old primary', code: 'target_not_available' },
  lease: { step: 'Wait for the old lease', code: 'lease_still_held' },
  choose: { step: 'Choose the standby', code: 'no_candidate' },
  handover: { step: 'Hand the lease to the new server', code: 'lease_held_elsewhere' },
  promote: { step: 'Promote the standby', code: 'promote_incomplete' },
  catalog: { step: 'Make the catalog follow', code: 'promote_incomplete' },
  repoint: { step: 'Point the other standbys at the new primary', code: 'step_failed' },
  lift: { step: 'Lift the freeze', code: 'step_failed' },
  pause: { step: 'Pause writes', code: 'step_failed' },
  verify: { step: 'Verify the standby', code: 'standby_behind' },
  report: { step: REPORT, code: 'step_failed' },
}
export const FAIL_STEPS = Object.keys(STEP_ALIAS)

const isFailoverKind = (kind: string): boolean =>
  kind === 'server.failover' || kind === 'replication.restorePreferredPrimaries'

function candidatesOf(g: GroupRec, chosen: string | null, loss: boolean): S['FailoverCandidate'][] {
  return g.standbyServerIds.map((id, i) => ({
    serverId: id,
    standbyId: g.standbyIds?.[id] ?? 0,
    readable: true,
    eligible: true,
    ineligibleReason: null,
    receiveLsn: loss && i === 0 ? '0/4FFC000' : '0/5000000',
    replayLsn: loss && i === 0 ? '0/4FFC000' : '0/5000000',
    lagBytes: loss && i === 0 ? 16384 : 0,
    syncState: loss ? ('async' as const) : ('quorum' as const),
    chosen: id === chosen,
  }))
}

function dataLossOf(mode: string, chosen: string | null, forced = false): S['DataLossAssessment'] {
  const possible = (mockConfig.failoverDataLoss || forced) && mode !== 'graceful' && chosen !== null
  return {
    verdict: possible ? 'possible' : 'none',
    message: possible
      ? forced
        ? 'The leader restarted while the failover ran, so it no longer knows how far the old primary had written; commits that only the old primary holds can be lost.'
        : 'The group ran without its synchronous standby; the standby may miss the last writes of the old primary.'
      : 'Every standby was read and the chosen one holds all acknowledged writes.',
    lastKnownPrimaryLsn: '0/5000000',
    chosenReceiveLsn: chosen === null ? null : possible ? '0/4FFC000' : '0/5000000',
    lastKnownAgeSeconds: possible && !forced ? 38 : 2,
    groupRanWithoutSynchronousStandby: possible && !forced,
    asyncDatabases: [],
    unreadableServerIds: [],
  }
}

const stepsOf = (mode: string): string[] => (mode === 'graceful' ? GRACEFUL_STEPS : HARD_STEPS)

/**
 * What a failover of `serverId` would do, for the groups the request names (default: all it is primary of). By default it refuses: the planned switchover and the map group answer 422 `unsupported_operation` unless the
 * URL switches (`graceful`, `map`) turn them on.
 */
export function failoverPlan(
  s: MockState,
  serverId: string,
  mode: S['FailoverRequest']['mode'],
  req: S['FailoverRequest'],
): S['FailoverPlan'] {
  const server = s.servers.find((x) => x.id === serverId)
  const own = s.groups.filter((g) => g.primaryServerId === serverId)
  const wanted = req.shardGroupIds
  for (const id of wanted ?? [])
    if (!own.some((g) => g.id === id))
      throw problem('unprocessable', `Server ${serverId} is not the primary of shard group ${id}.`)
  for (const tg of req.targets ?? []) {
    const g = own.find((x) => x.id === tg.shardGroupId)
    if (!g || !g.standbyServerIds.includes(tg.serverId))
      throw problem(
        'unprocessable',
        `Server ${tg.serverId} is not a standby of shard group ${tg.shardGroupId}.`,
      )
  }
  const chosenGroups = own.filter((g) => !wanted || wanted.includes(g.id))
  if (mode === 'graceful' && !mockConfig.failoverGraceful)
    throw problem(
      'unsupported_operation',
      'The planned switchover (graceful) is not implemented by this revision of kingresd.',
    )
  if (chosenGroups.some((g) => g.isMapGroup) && !mockConfig.failoverMap)
    throw problem(
      'unsupported_operation',
      'Moving the map group is not implemented by this revision of kingresd.',
    )
  if (mode === 'graceful' && server && server.status !== 'live')
    throw problem('node_state_invalid', `Server ${serverId} is not live.`)
  if (mockConfig.failoverBlock === 'quorum')
    throw problem('quorum_unavailable', 'This server cannot hear a majority of the servers.')

  const blockers: S['FailoverBlocker'][] = []
  if (mockConfig.failoverBlock === 'lease')
    blockers.push({
      code: 'lease_settings_differ',
      shardGroupId: null,
      message:
        'The servers do not use the same lease duration and margin (10 s and 4 s on 10.0.0.12).',
    })
  if (mockConfig.failoverBlock === 'rebalance')
    blockers.push({
      code: 'rebalance_in_progress',
      shardGroupId: null,
      message: 'A rebalance is running and did not stop within a minute.',
    })
  const groups = [...chosenGroups]
    .sort((a, b) => Number(b.isMapGroup) - Number(a.isMapGroup) || a.id - b.id)
    .map((g): S['FailoverGroupPlan'] => {
      const asked = req.targets?.find((x) => x.shardGroupId === g.id)?.serverId
      const none = mockConfig.failoverBlock === 'noreplica'
      const chosen = none ? null : (asked ?? g.standbyServerIds[0] ?? null)
      const dataLoss = dataLossOf(mode, chosen)
      if (chosen === null)
        blockers.push({
          code: 'no_replica_available',
          shardGroupId: g.id,
          message: `Shard group ${g.id} has no standby that is ready.`,
        })
      else if (dataLoss.verdict !== 'none')
        blockers.push({
          code: 'failoverNeedsDecision',
          shardGroupId: g.id,
          message: `Shard group ${g.id}: ${dataLoss.message}`,
        })
      return {
        shardGroupId: g.id,
        isMapGroup: g.isMapGroup,
        fromServerId: serverId,
        toServerId: chosen,
        candidates: none ? [] : candidatesOf(g, chosen, dataLoss.verdict !== 'none'),
        dataLoss,
        estimatedSeconds: chosen === null ? null : mode === 'graceful' ? 8 : 20,
        writesPausedSeconds: mode === 'graceful' ? 2 : null,
        steps: stepsOf(mode),
        movesTheManager: g.isMapGroup,
      }
    })
  return {
    generatedAt: nowIso(),
    kind: 'failover',
    mode,
    serverId,
    canProceed: blockers.length === 0,
    blockers,
    willStopRebalance: false,
    groups,
  }
}

/** What a restore of the preferred primaries would do (the groups that run away from their home server). */
export function restorePlan(s: MockState, ids?: number[]): S['FailoverPlan'] {
  // The same switches as a failover (`?failover=blocked:...`) reach a restore: a lost majority is a refusal, the others are blockers.
  if (mockConfig.failoverBlock === 'quorum')
    throw problem('quorum_unavailable', 'This server cannot hear a majority of the servers.')
  const blockers: S['FailoverBlocker'][] = []
  if (mockConfig.failoverBlock === 'lease')
    blockers.push({
      code: 'lease_settings_differ',
      shardGroupId: null,
      message:
        'The servers do not use the same lease duration and margin (10 s and 4 s on 10.0.0.12).',
    })
  if (mockConfig.failoverBlock === 'rebalance')
    blockers.push({
      code: 'rebalance_in_progress',
      shardGroupId: null,
      message: 'A rebalance is running and did not stop within a minute.',
    })
  const groups = s.groups
    .filter((g) => g.primaryServerId !== g.preferredPrimaryServerId && (!ids || ids.includes(g.id)))
    .map((g): S['FailoverGroupPlan'] => ({
      shardGroupId: g.id,
      isMapGroup: g.isMapGroup,
      fromServerId: g.primaryServerId ?? g.preferredPrimaryServerId,
      toServerId: g.preferredPrimaryServerId,
      candidates: candidatesOf(g, g.preferredPrimaryServerId, false),
      dataLoss: dataLossOf('restore', g.preferredPrimaryServerId),
      estimatedSeconds: 15,
      writesPausedSeconds: 2,
      steps: GRACEFUL_STEPS,
      movesTheManager: g.isMapGroup,
    }))
  return {
    generatedAt: nowIso(),
    kind: 'restorePreferredPrimaries',
    mode: 'restore',
    serverId: null,
    canProceed: blockers.length === 0,
    blockers,
    willStopRebalance: false,
    groups,
  }
}

function oldPrimaryOf(): S['OldPrimaryReport'] {
  switch (mockConfig.failoverOld) {
    case 'rebuilding':
      return {
        state: 'rebuilding',
        method: 'pgRewind',
        divergenceBytes: 2 * 1024 * 1024,
        rewindBytes: 12 * 1024 * 1024,
        message: 'pg_rewind is copying the blocks that changed on the old primary.',
      }
    case 'waiting':
      return {
        state: 'waitingForServer',
        method: null,
        divergenceBytes: null,
        rewindBytes: null,
        message: 'The server of the old primary does not answer; it is rebuilt when it does.',
      }
    case 'running':
      return {
        state: 'failed',
        method: null,
        divergenceBytes: null,
        rewindBytes: null,
        message:
          'The old primary still runs and could not be stopped; it is fenced by its lease and must be stopped by hand.',
      }
    case 'failed':
      return {
        state: 'failed',
        method: 'baseBackup',
        divergenceBytes: 6 * 1024 * 1024,
        rewindBytes: null,
        message: 'The new base backup failed: no space left on the device (simulated).',
      }
    default:
      return {
        state: 'notScheduled',
        method: null,
        divergenceBytes: null,
        rewindBytes: null,
        message: null,
      }
  }
}

function indexOfStep(run: FailoverRun, name: string | null | undefined): number {
  return name ? run.stepNames.indexOf(name) : -1
}

/** Where a group's block starts in the steps of the task. */
const blockStart = (run: FailoverRun, gi: number): number => 1 + gi * run.groupSteps.length

interface GroupSituation {
  state: GroupState
  phase: string | null
  /** The index of the step in the group, when it is the group the task is in. */
  at: number | null
}

/** Where each group of a run is, from the step the task is at (`index`, -1 when it is over) and how it ended. */
function situations(run: FailoverRun, task: Task): GroupSituation[] {
  const n = run.groupSteps.length
  const finished = task.state === 'done'
  const stopped = task.state === 'failed'
  const index = finished
    ? run.stepNames.length
    : Math.max(0, indexOfStep(run, stopped ? task.error?.step : task.progress.currentStep))
  const cancelled = stopped && task.error?.code === 'cancelled'
  return run.plan.groups.map((_, gi) => {
    const b = blockStart(run, gi)
    if (index >= b + n) return { state: 'done' as const, phase: null, at: null }
    if (index < b)
      return { state: stopped ? ('skipped' as const) : ('pending' as const), phase: null, at: null }
    const at = index - b
    if (stopped)
      return {
        state: cancelled ? ('skipped' as const) : ('failed' as const),
        phase: null,
        at,
      }
    const name = run.groupSteps[at] as string
    const before =
      at === 0 ? 'pending' : (STEP[run.groupSteps[at - 1] as string]?.after ?? 'pending')
    return { state: before, phase: STEP[name]?.phase ?? null, at }
  })
}

function summaryOf(r: S['FailoverReport']): S['FailoverReportSummary'] {
  const { mode: _m, options: _o, groups: _g, events: _e, ...summary } = r
  void [_m, _o, _g, _e]
  return summary
}

/** Writes (or replaces) the report of a run that is paused, failed, cancelled or done. */
export function writeReport(s: MockState, run: FailoverRun, finished = false): void {
  const rec = s.tasks.find((t) => t.task.id === run.taskId)
  if (!rec) return
  // The task is marked done after `apply` returns, and the report is written inside it.
  const task = finished ? { ...rec.task, state: 'done' as const } : rec.task
  const sits = situations(run, task)
  const cancelled = task.state === 'failed' && task.error?.code === 'cancelled'
  const finishedAt = task.state === 'paused' ? null : nowIso()
  const done = sits.filter((x) => x.state === 'done').length
  const failedCount = sits.filter((x) => x.state === 'failed').length
  const skippedCount = sits.filter((x) => x.state === 'skipped').length
  const outcome: S['FailoverReportSummary']['outcome'] =
    task.state === 'paused'
      ? 'paused'
      : task.state === 'done'
        ? mockConfig.failoverWarnings
          ? 'completedWithWarnings'
          : 'completed'
        : cancelled
          ? 'cancelled'
          : done > 0
            ? 'partial'
            : 'failed'
  const startedMs = Date.parse(run.startedAtIso)
  const seconds = Math.max(1, Math.round((Date.now() - startedMs) / 1000))
  const events: S['FailoverEvent'][] = [
    {
      at: run.startedAtIso,
      shardGroupId: null,
      step: CHECK,
      kind: 'started',
      message: `Failover of server ${run.serverId} started (${run.trigger}).`,
    },
  ]
  const groups = run.plan.groups.map((pg, gi): S['FailoverGroupReport'] => {
    const sit = sits[gi] as GroupSituation
    const forced = run.lossForced
    const dataLoss = forced ? dataLossOf(run.mode, pg.toServerId, true) : pg.dataLoss
    const promoted =
      sit.state === 'done' ||
      (sit.at !== null &&
        sit.at > run.groupSteps.indexOf('Promote the standby') &&
        sit.state === 'failed')
    if (sit.state === 'done')
      events.push({
        at: finishedAt ?? nowIso(),
        shardGroupId: pg.shardGroupId,
        step: 'Promote the standby',
        kind: 'promoted',
        message: `Shard group ${pg.shardGroupId} is now served by ${pg.toServerId ?? 'no server'}.`,
      })
    if (sit.state === 'failed')
      events.push({
        at: finishedAt ?? nowIso(),
        shardGroupId: pg.shardGroupId,
        step: task.error?.step ?? 'unknown',
        kind: 'failed',
        message: task.error?.message ?? 'The step failed.',
      })
    if (task.state === 'paused' && sit.at !== null)
      events.push({
        at: nowIso(),
        shardGroupId: pg.shardGroupId,
        step: 'Choose the standby',
        kind: 'paused',
        message: 'The failover waits for a decision about lost data.',
      })
    return {
      shardGroupId: pg.shardGroupId,
      outcome:
        sit.state === 'done'
          ? 'done'
          : sit.state === 'failed'
            ? 'failed'
            : sit.state === 'skipped'
              ? 'skipped'
              : 'pending',
      skipReason:
        sit.state === 'skipped'
          ? cancelled
            ? 'The failover was cancelled before this group was promoted.'
            : 'An earlier group failed, so this one was not started.'
          : null,
      fromServerId: pg.fromServerId,
      toServerId: pg.toServerId,
      termBefore: sit.state === 'pending' || sit.state === 'skipped' ? null : 4,
      termAfter: promoted ? 5 : null,
      timeline: promoted ? 2 : null,
      leaseWaitMs: run.mode === 'hard' && sit.state !== 'pending' ? 10000 : null,
      durationSeconds: sit.state === 'pending' ? 0 : Math.min(seconds, 3),
      candidates: pg.candidates,
      dataLoss,
      unreachedServerIds:
        run.mode === 'hard' && mockConfig.failoverOld === 'waiting' ? [run.serverId] : [],
      incidents:
        promoted && forced
          ? [
              {
                kind: 'prepared_rolled_back_after_failover',
                message:
                  'One prepared transaction of the old primary was rolled back on the new one.',
              },
            ]
          : [],
      oldPrimary: promoted ? oldPrimaryOf() : oldPrimaryNone(),
      error: sit.state === 'failed' ? (task.error?.message ?? null) : null,
    }
  })
  const report: S['FailoverReport'] = {
    id: run.taskId,
    kind: 'failover',
    trigger: run.trigger,
    serverId: run.serverId,
    outcome,
    startedAt: run.startedAtIso,
    finishedAt,
    requestedBy: run.trigger === 'automatic' ? 'system' : (task.createdBy ?? 'admin'),
    totals: {
      groupsTotal: run.plan.groups.length,
      groupsDone: done,
      groupsFailed: failedCount,
      groupsSkipped: skippedCount,
      durationSeconds: seconds,
    },
    dataLossPossible: groups.some((g) => g.dataLoss.verdict !== 'none'),
    error: task.state === 'failed' ? (task.error?.message ?? null) : null,
    mode: run.mode,
    options: run.req,
    groups,
    events,
  }
  s.failover.reports = s.failover.reports.filter((x) => x.id !== run.taskId)
  s.failover.reports.unshift(report)
}

function oldPrimaryNone(): S['OldPrimaryReport'] {
  return {
    state: 'notScheduled',
    method: null,
    divergenceBytes: null,
    rewindBytes: null,
    message: null,
  }
}

/** Resolves the alert that says a failover of the server waits for a decision. */
function clearDecision(s: MockState, serverId: string): void {
  for (const a of s.alerts)
    if (
      a.kind === 'failoverNeedsDecision' &&
      a.state !== 'resolved' &&
      a.subject?.serverId === serverId
    ) {
      a.state = 'resolved'
      a.resolvedAt = nowIso()
    }
}

/** The groups' roles change when a standby was promoted (the old server drops out of the group). */
function applyGroup(s: MockState, run: FailoverRun, groupId: number): void {
  const rec = s.servers.find((x) => x.id === run.serverId)
  const g = s.groups.find((x) => x.id === groupId)
  const next = run.plan.groups.find((x) => x.shardGroupId === groupId)?.toServerId ?? null
  if (!g || !rec) return
  if (next !== null) g.standbyServerIds = g.standbyServerIds.filter((x) => x !== next)
  if (run.mode === 'graceful' && next !== null) g.standbyServerIds.push(rec.id)
  else g.failedServerIds.push(rec.id)
  g.primaryServerId = next
}

/** What a run that ended well changes in the cluster: the other standbys of the server drop out, the server is dead. */
function finishRun(s: MockState, run: FailoverRun): void {
  const rec = s.servers.find((x) => x.id === run.serverId)
  if (!rec) return
  for (const pg of run.plan.groups) applyGroup(s, run, pg.shardGroupId)
  if (run.mode === 'hard') {
    for (const g of s.groups)
      if (
        !run.plan.groups.some((x) => x.shardGroupId === g.id) &&
        g.standbyServerIds.includes(rec.id)
      ) {
        g.standbyServerIds = g.standbyServerIds.filter((x) => x !== rec.id)
        g.failedServerIds.push(rec.id)
      }
    rec.status = 'dead'
    // A failover that a person started is not counted and not limited (design 4.4); an automatic one is.
    if (run.trigger === 'automatic') countAutomaticFailover(s, rec.id)
    s.alerts.unshift({
      id: Math.max(0, ...s.alerts.map((a) => a.id)) + 1,
      kind: 'serverDown',
      severity: 'critical',
      state: 'open',
      title: `Server ${rec.id} was failed over`,
      message: `Standbys of ${run.plan.groups.length} shard group(s) were promoted. Restore preferred primaries after the server returns.`,
      serverId: rec.id,
      databaseName: null,
      raisedAt: nowIso(),
    })
  }
  const subject = { serverId: rec.id }
  if (mockConfig.failoverOld === 'running')
    raise(
      s,
      'oldPrimaryRunning',
      'critical',
      subject,
      'The old primary runs',
      `The old primary on server ${rec.id} still runs.`,
    )
  if (mockConfig.failoverOld === 'failed')
    raise(
      s,
      'oldPrimaryNotRebuilt',
      'warning',
      subject,
      'The old primary was not rebuilt',
      `The old primary on server ${rec.id} could not be rebuilt as a standby.`,
    )
  if (run.plan.groups.some((g) => g.dataLoss.verdict !== 'none') || run.lossForced)
    raise(
      s,
      'failoverDataLoss',
      'warning',
      subject,
      'A failover may have lost data',
      `The failover of server ${rec.id} was made with data loss accepted.`,
    )
  clearDecision(s, rec.id)
}

/** Starts the task of a failover that was accepted: its steps, its pause, its failure, its end. */
export function startFailoverRun(
  s: MockState,
  o: {
    serverId: string
    mode: S['FailoverRequest']['mode']
    req: S['FailoverRequest']
    plan: S['FailoverPlan']
    trigger: 'manual' | 'automatic'
    user?: string
  },
): Task {
  const groupSteps = stepsOf(o.mode)
  const stepNames = [
    CHECK,
    ...o.plan.groups.flatMap((g) => groupSteps.map((st) => `Group ${g.shardGroupId}: ${st}`)),
    REPORT,
  ]
  const run: FailoverRun = {
    taskId: s.nextTaskId,
    trigger: o.trigger,
    serverId: o.serverId,
    mode: o.mode,
    req: o.req,
    plan: o.plan,
    groupSteps,
    stepNames,
    failAt: null,
    needsDecision: false,
    accepted: o.req.allowDataLoss === true,
    mapBootstrap: o.plan.groups.some((g) => g.isMapGroup),
    startedAtIso: nowIso(),
    lossForced: false,
  }
  const total = stepNames.length
  const absolute = (gi: number, name: string): number =>
    1 + gi * groupSteps.length + groupSteps.indexOf(name)
  const promoteAt = absolute(0, 'Promote the standby')
  const fail = mockConfig.failoverFailStep
  const behind = mockConfig.failoverSwitchoverBehind && o.mode === 'graceful'
  const failSpec = behind
    ? { step: 'Verify the standby', code: 'standby_behind', group: null }
    : fail
      ? { ...(STEP_ALIAS[fail.step] ?? { step: '', code: 'step_failed' }), group: fail.group }
      : null
  if (failSpec && failSpec.step !== '') {
    const gi = Math.max(
      0,
      o.plan.groups.findIndex((g) => g.shardGroupId === failSpec.group),
    )
    const index =
      failSpec.step === CHECK
        ? 0
        : failSpec.step === REPORT
          ? total - 1
          : groupSteps.includes(failSpec.step)
            ? absolute(gi, failSpec.step)
            : -1
    if (index >= 0)
      run.failAt = {
        index,
        code: failSpec.code,
        message:
          failSpec.code === 'standby_behind'
            ? 'The standby had not caught up when writes were stopped; the switchover was rolled back and the old primary leads again.'
            : `The step "${failSpec.step}" failed (simulated).`,
      }
  }
  if (mockConfig.failoverNeedsDecision && o.mode !== 'graceful' && o.req.allowDataLoss !== true)
    run.needsDecision = true
  s.failover.runs.unshift(run)
  const id = run.taskId
  const slow = mockConfig.failoverSlow
  const task = createTask(s, {
    kind: 'server.failover',
    subject: { type: 'server', id: o.serverId },
    steps: stepNames,
    stepGroups: [null, ...o.plan.groups.flatMap((g) => groupSteps.map(() => g.shardGroupId)), null],
    // About 1.2 s per group of work at the usual speed, so that a test with the usual speed is still quick.
    durationMs: mockConfig.taskDurationMs * (slow ? 3 : 1),
    uncapped: true,
    cancellable: true,
    user: o.user,
    pauseAt: run.needsDecision ? (absolute(0, 'Choose the standby') + 0.5) / total : undefined,
    failAt: run.failAt
      ? {
          fraction: (run.failAt.index + 0.5) / total,
          code: run.failAt.code,
          message: run.failAt.message,
        }
      : undefined,
    onProgress: (fraction) => {
      const rec = s.tasks.find((t) => t.task.id === id)
      // From the moment a standby is promoted the failover can only go forward.
      if (rec && rec.task.state === 'running' && fraction * total >= promoteAt)
        rec.task.cancellable = false
    },
    onPause: () => {
      run.lossForced = true
      writeReport(s, run)
      const first = o.plan.groups[0]
      raise(
        s,
        'failoverNeedsDecision',
        'critical',
        { serverId: o.serverId, ...(first ? { shardGroupId: first.shardGroupId } : {}) },
        'A failover waits for a decision',
        `The failover of server ${o.serverId} waits: commits can be lost.`,
      )
    },
    onFail: () => {
      const rec = s.tasks.find((t) => t.task.id === id)
      if (rec) {
        const sits = situations(run, rec.task)
        run.plan.groups.forEach((pg, gi) => {
          const sit = sits[gi] as GroupSituation
          const promoted =
            sit.state === 'done' ||
            (sit.state === 'failed' &&
              sit.at !== null &&
              sit.at > groupSteps.indexOf('Promote the standby'))
          if (promoted) applyGroup(s, run, pg.shardGroupId)
        })
      }
      writeReport(s, run)
      if (rec?.task.error?.code !== 'cancelled')
        raise(
          s,
          'failoverBlocked',
          'critical',
          { serverId: o.serverId },
          'A failover is blocked',
          `The failover of server ${o.serverId} failed: ${rec?.task.error?.message ?? 'a step failed'}`,
        )
    },
    apply: () => {
      finishRun(s, run)
      writeReport(s, run, true)
    },
  })
  return task
}

/** A repeat of the request that accepts the loss: the paused task goes on. */
export function resumeFailover(s: MockState, rec: TaskRec): Task {
  const run = s.failover.runs.find((r) => r.taskId === rec.task.id)
  rec.pauseAt = undefined
  rec.task.state = 'running'
  rec.startedAt = Date.now() - (rec.task.progress.percent / 100) * rec.durationMs
  if (run) {
    run.accepted = true
    run.needsDecision = false
    run.req = { ...run.req, allowDataLoss: true }
    clearDecision(s, run.serverId)
  }
  return structuredClone(rec.task)
}

/** Cancel before the point of no return: the freezes are lifted, no group changes, the report says cancelled. */
export function cancelFailover(s: MockState, rec: TaskRec): Task {
  const run = s.failover.runs.find((r) => r.taskId === rec.task.id)
  const t = rec.task
  const at = Math.max(
    0,
    t.progress.steps.findIndex((st) => st.state !== 'done'),
  )
  t.state = 'failed'
  t.error = {
    code: 'cancelled',
    message: 'The failover was cancelled before a standby was promoted; the freezes are lifted.',
    step: t.progress.steps[at]?.name ?? null,
    retryable: false,
  }
  t.cancellable = false
  t.progress.currentStep = null
  t.progress.steps = t.progress.steps.map((st, i) => ({
    ...st,
    state: i < at ? ('done' as const) : i === at ? ('failed' as const) : ('skipped' as const),
  }))
  t.updatedAt = nowIso()
  if (run) {
    writeReport(s, run)
    clearDecision(s, run.serverId)
  }
  return structuredClone(t)
}

/** `GET /failover`: the run, group by group, from the task the run is. */
export function failoverStatusView(s: MockState): S['FailoverStatus'] {
  tickTasks(s)
  const latest = s.failover.reports[0]
  const lastReport = latest ? summaryOf(latest) : null
  const running = s.tasks.find(
    (t) => isFailoverKind(t.task.kind) && (t.task.state === 'running' || t.task.state === 'paused'),
  )
  const newest = s.tasks.find((t) => isFailoverKind(t.task.kind))
  const rec =
    running ??
    (newest && newest.task.state === 'failed' && newest.task.error?.code !== 'cancelled'
      ? newest
      : undefined)
  if (!rec) return { state: 'idle', needsDecision: [], groups: [], cancellable: false, lastReport }
  const task = rec.task
  const restoring = task.kind === 'replication.restorePreferredPrimaries'
  const run = s.failover.runs.find((r) => r.taskId === task.id)
  const state: S['FailoverStatus']['state'] =
    task.state === 'paused' ? 'paused' : task.state === 'failed' ? 'failed' : 'running'
  const base = {
    state,
    kind: restoring ? ('restorePreferredPrimaries' as const) : ('failover' as const),
    trigger: run?.trigger ?? ('manual' as const),
    serverId: restoring ? null : (task.subject?.id ?? null),
    startedAt: task.createdAt,
    cancellable: task.cancellable,
    lastReport,
  }
  if (!run)
    return {
      ...base,
      phase: state === 'running' ? 'promoting' : null,
      taskId: task.id,
      pausedReason: null,
      needsDecision: [],
      groups: [],
    }
  const sits = situations(run, task)
  const total = run.stepNames.length
  const stepIdx = Math.max(0, indexOfStep(run, task.progress.currentStep))
  const activeGroup = sits.findIndex((x) => x.at !== null && x.state !== 'done')
  const info = activeGroup >= 0 ? sits[activeGroup] : undefined
  // The map group is promoted before the system database can record the task: until the catalog follows, no task id.
  const bootstrapping =
    run.mapBootstrap &&
    state === 'running' &&
    info !== undefined &&
    (run.plan.groups[activeGroup] as S['FailoverGroupPlan']).isMapGroup &&
    (info.at ?? 0) <= run.groupSteps.indexOf('Make the catalog follow')
  const phase =
    state === 'failed'
      ? null
      : bootstrapping
        ? 'bootstrap'
        : stepIdx === 0
          ? 'checking'
          : stepIdx >= total - 1
            ? 'finishing'
            : (info?.phase ?? null)
  const waits = info?.phase === 'waitingForLease'
  const groups = run.plan.groups.map((pg, gi): S['FailoverGroupProgress'] => {
    const sit = sits[gi] as GroupSituation
    const here = sit.at !== null && sit.state !== 'done'
    const blockMs = rec.startedAt + (blockStart(run, gi) / total) * rec.durationMs
    const pausedWrites =
      run.mode === 'graceful' &&
      here &&
      sit.at !== null &&
      sit.at >= run.groupSteps.indexOf('Pause writes') &&
      sit.at < run.groupSteps.indexOf('Lift the freeze') &&
      state === 'running'
    return {
      shardGroupId: pg.shardGroupId,
      state: sit.state,
      phase:
        here && state !== 'failed'
          ? bootstrapping && pg.isMapGroup
            ? 'bootstrap'
            : sit.phase
          : null,
      fromServerId: pg.fromServerId,
      toServerId: pg.toServerId,
      writesPausedSince: pausedWrites ? new Date(blockMs).toISOString() : null,
      message:
        waits && here
          ? `Waiting ${Math.max(1, Math.round(8 * (1 - (((task.progress.percent / 100) * total) % 1))))} s for the old primary's lease to run out.`
          : sit.state === 'failed'
            ? (task.error?.message ?? null)
            : null,
    }
  })
  const pg0 = run.plan.groups[0]
  return {
    ...base,
    phase,
    taskId: bootstrapping ? null : task.id,
    pausedReason:
      state === 'paused'
        ? 'The leader restarted while the failover ran and no longer knows how far the old primary had written: commits that only it holds can be lost. Decide whether to go on.'
        : null,
    needsDecision:
      state === 'paused' && pg0
        ? [
            {
              code: 'failoverNeedsDecision',
              shardGroupId: pg0.shardGroupId,
              message: `Shard group ${pg0.shardGroupId}: ${dataLossOf(run.mode, pg0.toServerId, true).message}`,
            },
          ]
        : [],
    groups,
  }
}

/** Seeds an automatic failover under way (`?failover=auto:<server>`). */
export function seedAutomaticFailover(s: MockState, serverId: string): void {
  const server = s.servers.find((x) => x.id === serverId)
  if (!server) return
  const plan = failoverPlan(s, serverId, 'hard', { mode: 'hard' })
  startFailoverRun(s, {
    serverId,
    mode: 'hard',
    req: { mode: 'hard' },
    plan,
    trigger: 'automatic',
    user: 'system',
  })
}

/**
 * Puts the cluster back as it was before any failover (the groups' roles, the servers' status, the tasks, the reports, the alerts and the
 * counter), so that a state can be tried again and again in one session (the browser tests do; `__kingresMock.resetFailover`).
 */
export function resetFailoverState(s: MockState): void {
  // Only what a failover changes: the primary, the standbys and the failed servers of each group, and the status of a server.
  // A lease or a standby that was made to fail by a switch stays as it is.
  for (const fresh of createMockState().groups) {
    const g = s.groups.find((x) => x.id === fresh.id)
    if (!g) continue
    g.primaryServerId = fresh.primaryServerId
    // The chain of the seed, filled up to the replica count in force (a standby task may have raised it).
    g.standbyServerIds = [...fresh.standbyServerIds]
    for (const server of s.servers) {
      if (g.standbyServerIds.length >= s.settings.defaultReplicaCount) break
      if (server.id !== g.primaryServerId && !g.standbyServerIds.includes(server.id))
        g.standbyServerIds.push(server.id)
    }
    g.failedServerIds = fresh.failedServerIds
  }
  for (const server of s.servers) server.status = 'live'
  s.tasks = s.tasks.filter((t) => !isFailoverKind(t.task.kind))
  s.failover = { reports: [], runs: [] }
  s.alerts = s.alerts.filter(
    (a) =>
      !a.kind.startsWith('failover') &&
      !a.kind.startsWith('oldPrimary') &&
      !a.title.endsWith('was failed over'),
  )
  s.settings.autoFailover = { ...s.settings.autoFailover, eventsSinceReset: 0, lastEventAt: null }
}

/** Raises the alert that the limit of automatic failovers is reached (once; it clears when the counter is reset). */
export function raiseAutoFailoverLimit(s: MockState): void {
  raise(
    s,
    'autoFailoverLimitReached',
    'warning',
    {},
    'Automatic failover limit reached',
    'Automatic failover has been used as often as the settings allow. Further failures need an operator.',
  )
}

/** One automatic failover that promoted a group: the counter goes up, the alert of the event is raised (info). */
export function countAutomaticFailover(s: MockState, serverId: string): void {
  const af = s.settings.autoFailover
  s.settings.autoFailover = {
    ...af,
    eventsSinceReset: af.eventsSinceReset + 1,
    lastEventAt: nowIso(),
  }
  raise(
    s,
    'autoFailoverTriggered',
    'info',
    { serverId },
    'Automatic failover happened',
    `Server ${serverId} was failed over automatically.`,
  )
  if (s.settings.autoFailover.eventsSinceReset >= s.settings.autoFailover.maxCount)
    raiseAutoFailoverLimit(s)
}

/** The counter is set to 0 (by the reset, or by a rebalance that completes); the alert of the limit clears. */
export function resetAutoFailoverCounter(s: MockState): void {
  s.settings.autoFailover = { ...s.settings.autoFailover, eventsSinceReset: 0 }
  for (const a of s.alerts)
    if (a.kind === 'autoFailoverLimitReached' && a.state !== 'resolved') {
      a.state = 'resolved'
      a.resolvedAt = nowIso()
    }
}
