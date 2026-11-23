// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The words of the failover console (contract 0.10.0, design `ha-phase5-part2.md` section 8): the data-loss verdict
// in plain language, the phases and the states of a run, the readiness of a group, the refusals of a failover request
// and the state of the old primary. Nothing here parses a message from the server: every statement is chosen by a
// typed member of the contract, and a value a newer server adds is shown as it was sent (through SafeName).
import { ApiError } from '@/api/problem'
import type {
  DataLossAssessment,
  FailoverBlocker,
  FailoverReadiness,
  FailoverStatus,
  OldPrimaryReport,
  TaskStep,
} from '@/api/types'
import { formatDateTime } from './format'
import type { StatusMeta, Tone } from './status'
import { safeText } from './safe-text'

const OK = '✓'
const WARN = '▲'
const BAD = '✖'
const INFO = 'ⓘ'
const RING = '○'
const HALF = '◐'

export interface DataLossWords {
  tone: Tone
  icon: string
  label: string
  /** One sentence; it can name servers, so a view draws it through SafeName. */
  text: string
}

/**
 * What a failover of one group can lose, in words (design 2.6). `at` is the time of the plan or report the assessment
 * belongs to: the time of the last position the leader knew is that time less `lastKnownAgeSeconds`.
 */
export function dataLossWords(
  loss: DataLossAssessment,
  groupId: number | null,
  at: string | null = null,
): DataLossWords {
  switch (loss.verdict) {
    case 'none':
      return {
        tone: 'ok',
        icon: OK,
        label: 'No data loss',
        text: 'No acknowledged commit can be lost.',
      }
    case 'asyncOnly':
      return {
        tone: 'info',
        icon: INFO,
        label: 'Async commits at risk',
        text: 'Commits written under asynchronous durability in the last seconds can be lost.',
      }
    case 'possible': {
      if (loss.groupRanWithoutSynchronousStandby) {
        const since = sinceText(loss, at)
        return {
          tone: 'danger',
          icon: BAD,
          label: 'Data can be lost',
          text: `Commits acknowledged since ${since} can be lost: ${groupId === null ? 'the group' : `group ${groupId}`} ran without its synchronous standby.`,
        }
      }
      return {
        tone: 'danger',
        icon: BAD,
        label: 'Data can be lost',
        text: 'The chosen standby is behind the best one; commits that only the better standby holds can be lost.',
      }
    }
    case 'unknown': {
      const names = loss.unreadableServerIds.map((s) => safeText(s, 60))
      return {
        tone: 'warn',
        icon: WARN,
        label: 'Not known',
        text:
          names.length > 0
            ? `The standby on ${names.join(', ')} could not be read; commits that only it holds can be lost.`
            : 'A standby could not be read; commits that only it holds can be lost.',
      }
    }
    default:
      return {
        tone: 'warn',
        icon: WARN,
        label: 'Not known',
        text: 'This server reports a verdict that this console does not know; read its message.',
      }
  }
}

function sinceText(loss: DataLossAssessment, at: string | null): string {
  if (at === null || loss.lastKnownAgeSeconds === null) return 'the last check of the leader'
  const t = Date.parse(at)
  if (Number.isNaN(t)) return 'the last check of the leader'
  return formatDateTime(new Date(t - loss.lastKnownAgeSeconds * 1000).toISOString())
}

/** The verdicts that `allowDataLoss` has to accept: everything but `none`. */
export function needsDecision(loss: DataLossAssessment): boolean {
  return loss.verdict !== 'none'
}

/** The sentence that says what a group's readiness is, with its tone. */
export interface ReadinessWords {
  state: 'possible' | 'possibleLoss' | 'blocked'
  tone: Tone
  icon: string
  label: string
  text: string
}

/** The readiness line of a group card (design 8.1). `candidate` is the server of the standby that would be promoted. */
export function readinessWords(r: FailoverReadiness): ReadinessWords {
  const who =
    r.candidateServerId === null
      ? 'a standby'
      : `the standby on ${safeText(r.candidateServerId, 60)}`
  if (!r.possible)
    return {
      state: 'blocked',
      tone: 'danger',
      icon: BAD,
      label: 'Failover not possible',
      text: 'Failover is not possible',
    }
  switch (r.dataLossVerdict) {
    case 'none':
      return {
        state: 'possible',
        tone: 'ok',
        icon: OK,
        label: 'Failover possible',
        text: `Failover is possible: ${who}, no data loss.`,
      }
    case 'asyncOnly':
      return {
        state: 'possibleLoss',
        tone: 'info',
        icon: INFO,
        label: 'Failover possible',
        text: `Failover is possible: ${who}; commits written under asynchronous durability in the last seconds could be lost.`,
      }
    default:
      return {
        state: 'possibleLoss',
        tone: 'warn',
        icon: WARN,
        label: 'Failover possible, with a risk',
        text: 'Failover is possible, but commits made without a synchronous standby could be lost.',
      }
  }
}

/** Words for the codes of a `FailoverBlocker` (a problem code the request would get, or `failoverNeedsDecision`). */
export const BLOCKER_LABEL: Record<string, string> = {
  failoverNeedsDecision: 'Needs your decision',
  failover_needs_decision: 'Needs your decision',
  no_replica_available: 'No standby is ready',
  cluster_inconsistent: 'The catalog is not consistent',
  quorum_unavailable: 'No majority of servers',
  lease_settings_differ: 'Lease settings differ',
  version_mismatch: 'Versions differ',
  failover_in_progress: 'A failover is running',
  rebalance_in_progress: 'A rebalance is running',
  operation_in_progress: 'Another operation is running',
  unsupported_operation: 'Not available yet',
}

export function blockerLabel(b: FailoverBlocker): string {
  return BLOCKER_LABEL[b.code] ?? 'Blocked'
}

/** The blockers that `allowDataLoss` does not lift: they disable the start button. */
export function hardBlockers(blockers: FailoverBlocker[]): FailoverBlocker[] {
  return blockers.filter((b) => b.code !== 'failoverNeedsDecision')
}

export const PHASE_LABEL: Record<string, string> = {
  checking: 'Checking that the failover can start',
  waitingForLease: "Waiting for the old primary's lease to run out",
  choosing: 'Choosing the standby',
  promoting: 'Promoting the standby',
  followingCatalog: 'Making the catalog follow',
  repointing: 'Pointing the other standbys at the new primary',
  finishing: 'Finishing and writing the report',
  quiescing: 'Pausing writes',
  waitingForTransactions: 'Waiting for open transactions',
  stopping: 'Stopping the old primary',
  verifying: 'Verifying the new primary',
  bootstrap: 'Moving the manager of the cluster',
}

/** A phase from a newer server is shown as sent. */
export function phaseLabel(phase: string | null | undefined): string | null {
  if (!phase) return null
  return PHASE_LABEL[phase] ?? safeText(phase, 60)
}

type GroupState = NonNullable<FailoverStatus['groups'][number]['state']>

export const GROUP_STATE: Record<GroupState, StatusMeta> = {
  pending: { tone: 'neutral', icon: RING, label: 'Waiting' },
  frozen: { tone: 'info', icon: HALF, label: 'Frozen' },
  fenced: { tone: 'info', icon: HALF, label: 'Old primary stopped' },
  pinned: { tone: 'info', icon: HALF, label: 'Standby chosen' },
  promoted: { tone: 'ok', icon: HALF, label: 'Promoted' },
  done: { tone: 'ok', icon: OK, label: 'Done' },
  failed: { tone: 'danger', icon: BAD, label: 'Failed' },
  skipped: { tone: 'neutral', icon: RING, label: 'Skipped' },
}

export function groupStateMeta(state: string): StatusMeta {
  return (
    (GROUP_STATE as Record<string, StatusMeta | undefined>)[state] ?? {
      tone: 'neutral',
      icon: '?',
      label: `Unknown (${safeText(state, 40)})`,
    }
  )
}

/** The state of a run (`FailoverStatus.state`). */
export const RUN_STATE: Record<string, StatusMeta> = {
  idle: { tone: 'neutral', icon: RING, label: 'No failover' },
  running: { tone: 'info', icon: HALF, label: 'Running' },
  paused: { tone: 'warn', icon: WARN, label: 'Waiting for you' },
  failed: { tone: 'danger', icon: BAD, label: 'Failed' },
}

export function runStateMeta(state: string): StatusMeta {
  return (
    RUN_STATE[state] ?? {
      tone: 'neutral',
      icon: '?',
      label: `Unknown (${safeText(state, 40)})`,
    }
  )
}

/** How a run ended (`FailoverReportSummary.outcome`). */
export const OUTCOME: Record<string, StatusMeta> = {
  completed: { tone: 'ok', icon: OK, label: 'Completed' },
  // 0.12.0: the failover ended and the check of the cluster afterwards reported something; the report says what.
  completedWithWarnings: {
    tone: 'ok',
    icon: OK,
    label: 'Completed, with remarks',
    hint: 'The failover ended. The check of the cluster afterwards reported something: open the report to read it.',
  },
  partial: { tone: 'warn', icon: WARN, label: 'Partly done' },
  failed: { tone: 'danger', icon: BAD, label: 'Failed' },
  cancelled: { tone: 'neutral', icon: RING, label: 'Cancelled' },
  paused: { tone: 'warn', icon: WARN, label: 'Waiting for a decision' },
}

export function outcomeMeta(outcome: string): StatusMeta {
  return (
    OUTCOME[outcome] ?? {
      tone: 'neutral',
      icon: '?',
      label: `Unknown (${safeText(outcome, 40)})`,
    }
  )
}

/** The outcome of one group of a report. */
export const GROUP_OUTCOME: Record<string, StatusMeta> = {
  done: GROUP_STATE.done,
  failed: GROUP_STATE.failed,
  skipped: GROUP_STATE.skipped,
  pending: GROUP_STATE.pending,
}

export function groupOutcomeMeta(outcome: string): StatusMeta {
  return GROUP_OUTCOME[outcome] ?? groupStateMeta(outcome)
}

export const KIND_LABEL: Record<string, string> = {
  failover: 'Failover',
  restorePreferredPrimaries: 'Restore of the preferred primaries',
}

export const TRIGGER_LABEL: Record<string, string> = {
  manual: 'started by a person',
  automatic: 'started automatically by the cluster',
}

export const MODE_LABEL: Record<string, string> = {
  hard: 'Hard (the server is down or not answering)',
  graceful: 'Graceful (hand over without loss)',
  restore: 'Restore of the preferred primaries',
}

/** What the extension and the old primary's server did with the old primary. */
const OLD_PRIMARY_STATE: Record<OldPrimaryReport['state'], string> = {
  notScheduled:
    'No rebuild of it is scheduled. It stays stopped and fenced (an earlier version of the server never rebuilt it, or it has no directory to rebuild).',
  rebuildScheduled: 'It will be rebuilt as a standby.',
  rebuilding: 'It is being rebuilt as a standby',
  standby: 'It is a standby of the new primary now',
  waitingForServer: 'It is rebuilt when its server is back',
  removed: 'It was removed',
  failed: 'Rebuilding it failed',
}

const OLD_PRIMARY_TONE: Record<OldPrimaryReport['state'], Tone> = {
  notScheduled: 'neutral',
  rebuildScheduled: 'info',
  rebuilding: 'info',
  standby: 'ok',
  waitingForServer: 'warn',
  removed: 'neutral',
  failed: 'danger',
}

export interface OldPrimaryWords {
  tone: Tone
  icon: string
  label: string
  text: string
}

const METHOD_LABEL: Record<string, string> = {
  clean: 'after a clean stop',
  pgRewind: 'with pg_rewind',
  baseBackup: 'with a new base backup',
}

/** The state of the old primary of a group in words, with the numbers the report has ("rebuilding, pg_rewind, 12 MiB"). */
export function oldPrimaryWords(
  old: OldPrimaryReport,
  formatBytes: (n: number) => string,
): OldPrimaryWords {
  const state = old.state as OldPrimaryReport['state']
  const known = state in OLD_PRIMARY_STATE
  const tone = known ? OLD_PRIMARY_TONE[state] : 'neutral'
  const parts: string[] = [
    known ? OLD_PRIMARY_STATE[state] : `State ${safeText(String(old.state), 40)}`,
  ]
  if (old.method) parts.push(METHOD_LABEL[old.method] ?? safeText(String(old.method), 40))
  if (old.rewindBytes !== null) parts.push(`${formatBytes(old.rewindBytes)} rewound`)
  if (old.divergenceBytes !== null)
    parts.push(`${formatBytes(old.divergenceBytes)} written after the failover point`)
  const icon =
    tone === 'ok'
      ? OK
      : tone === 'danger'
        ? BAD
        : tone === 'warn'
          ? WARN
          : tone === 'info'
            ? HALF
            : RING
  return {
    tone,
    icon,
    label:
      state === 'standby'
        ? 'Standby'
        : state === 'rebuilding'
          ? 'Rebuilding'
          : state === 'failed'
            ? 'Rebuild failed'
            : state === 'waitingForServer'
              ? 'Waiting for its server'
              : state === 'notScheduled'
                ? 'Stopped'
                : state === 'removed'
                  ? 'Removed'
                  : known
                    ? 'Rebuild planned'
                    : 'Unknown',
    text: parts.join(', ') + (parts.length > 0 && !parts[0]?.endsWith('.') ? '.' : ''),
  }
}

/** Steps of a multi-group task, one block per group; the steps of the whole task have `groupId` null. */
export interface StepBlock {
  groupId: number | null
  steps: TaskStep[]
}

/** The steps in the order the server sent them, in blocks of the same group (before, between and after the groups). */
export function stepBlocks(steps: TaskStep[]): StepBlock[] {
  const blocks: StepBlock[] = []
  for (const step of steps) {
    const groupId = step.shardGroupId ?? null
    const last = blocks[blocks.length - 1]
    if (last && last.groupId === groupId) last.steps.push(step)
    else blocks.push({ groupId, steps: [step] })
  }
  return blocks
}

/** True when the task has steps that belong to a group (a failover); then the steps are shown in blocks. */
export function hasGroupSteps(steps: TaskStep[]): boolean {
  return steps.some((s) => s.shardGroupId !== undefined && s.shardGroupId !== null)
}

/** The name of a step without its "Group 2: " prefix (the heading of the block says the group). */
export function stepName(step: TaskStep): string {
  return step.shardGroupId === undefined || step.shardGroupId === null
    ? step.name
    : step.name.replace(/^Group \d+:\s*/, '')
}

/** How a task kind that is a failover is told apart from the other tasks. */
export function isFailoverTaskKind(kind: string): boolean {
  return kind === 'server.failover' || kind === 'replication.restorePreferredPrimaries'
}

/** The refusal of a failover request, in words of its own (design 2, 7 and 8.2). */
export interface FailoverRefusal {
  code: string
  tone: 'info' | 'warning' | 'danger'
  heading: string
  message: string
  /** True when the server's own `detail` adds something (it is shown below the words, never instead of them). */
  withDetail: boolean
}

export interface RefusalContext {
  mode: 'hard' | 'graceful' | 'restore'
  /** The dialog is the switchover of one group (the words do not send the person to the hard mode). */
  switchover?: boolean
  /** The request names the map group (the manager of the cluster moves with it). */
  includesMapGroup: boolean
}

/**
 * What a refusal of `failoverServer` means for the person at the dialog. A code that has no words here falls back to
 * the generic ones of `describeProblem` (the caller does that), so the function answers null for it.
 */
export function failoverRefusal(error: unknown, ctx: RefusalContext): FailoverRefusal | null {
  if (!(error instanceof ApiError)) return null
  switch (error.code) {
    case 'failover_needs_decision':
      return {
        code: error.code,
        tone: 'warning',
        heading: 'The failover needs your decision',
        message:
          'The plan says commits can be lost. Read the plan below (it was read again just now), tick that you accept the loss, and start again.',
        withDetail: true,
      }
    case 'no_replica_available':
      return {
        code: error.code,
        tone: 'danger',
        heading: 'No standby is ready',
        message:
          'A group has no standby that is ready to be promoted, so promoting would leave it without a copy of its data. Wait for a standby to stream, or remove the group from the request.',
        withDetail: true,
      }
    case 'lease_settings_differ':
      return {
        code: error.code,
        tone: 'danger',
        heading: 'The servers use different lease settings',
        message:
          'The safety of a failover rests on every server using the same lease duration and margin, and these differ. Make them equal on every server (Configuration), then try again.',
        withDetail: true,
      }
    case 'quorum_unavailable':
      return {
        code: error.code,
        tone: 'danger',
        heading: 'No majority of servers',
        message:
          'The server that answered cannot hear a majority of the servers, so it cannot know that the old primary is gone. Nothing was changed. Restore the network or use a server that sees the majority.',
        withDetail: true,
      }
    case 'version_mismatch':
      return {
        code: error.code,
        tone: 'danger',
        heading: 'The servers run different versions',
        message:
          'A failover is done only while every server runs the same version of the sharding extension and the same PostgreSQL major version. Finish the upgrade first.',
        withDetail: true,
      }
    case 'cluster_inconsistent':
      return {
        code: error.code,
        tone: 'danger',
        heading: 'The sharding catalog is not consistent',
        message:
          'A standby is not registered in the catalog of a database, or the primary names one it does not know. Nothing was changed. Wait for the leader to repair the registration, or resolve it, then try again.',
        withDetail: true,
      }
    case 'failover_in_progress':
      return {
        code: error.code,
        tone: 'warning',
        heading: 'A failover is already running',
        message:
          'Only one failover or restore runs at a time in the cluster. Follow the running one; it is shown on this page.',
        withDetail: true,
      }
    case 'rebalance_in_progress':
      return {
        code: error.code,
        tone: 'warning',
        heading: 'A rebalance is running',
        message:
          'A rebalance did not stop within a minute. Stop it on the Rebalance page and wait until it has stopped, then start the failover again.',
        withDetail: true,
      }
    case 'operation_in_progress':
      return {
        code: error.code,
        tone: 'warning',
        heading: 'Another operation is running on this server',
        message:
          'An instance stop or restart, or a restart for settings, is pending or running. Wait for it to finish; the failover never runs beside it.',
        withDetail: true,
      }
    case 'node_state_invalid':
      return {
        code: error.code,
        tone: 'warning',
        heading: 'Not possible in this server state',
        message:
          ctx.mode === 'graceful'
            ? 'A graceful failover needs a server that is live. Use a hard failover for a server that is down.'
            : 'The server is in a state where this failover is not allowed.',
        withDetail: true,
      }
    case 'unprocessable':
      return {
        code: error.code,
        tone: 'warning',
        heading: 'The request does not fit the cluster',
        message:
          'A group is not led by this server, or a chosen server is not a standby of its group. Choose the groups and standbys again.',
        withDetail: true,
      }
    case 'unsupported_operation': {
      const parts: string[] = []
      if (ctx.mode === 'restore')
        parts.push(
          'Restoring the preferred primaries (a planned switchover of every group that runs away from its home server) is not part of this server version yet.',
        )
      else if (ctx.mode === 'graceful' && ctx.switchover)
        parts.push('The planned switchover of a group is not part of this server version yet.')
      else if (ctx.mode === 'graceful')
        parts.push(
          'The planned switchover (graceful) is not part of this server version yet. A server that is down needs the hard mode.',
        )
      if (ctx.includesMapGroup)
        parts.push(
          'The map group (the manager of the cluster) is moved by the request that is sent to the manager leader, for the server of the leader itself only. Ask the leader, or choose only the other groups.',
        )
      if (parts.length === 0)
        parts.push('This server version does not do what the request asks for yet.')
      return {
        code: error.code,
        tone: 'info',
        heading: 'Not available on this server version yet',
        message: `${parts.join(' ')} Nothing was changed.`,
        withDetail: true,
      }
    }
    default:
      return null
  }
}

/** The words of an old-primary or readiness text for a screen reader are the same as for the eye. */
export function groupTitle(id: number, isMapGroup: boolean): string {
  return isMapGroup ? `Map group (group ${id})` : `Group ${id}`
}

export interface FailoverBannerWords {
  tone: 'info' | 'danger'
  /** `\u2716` for a run that waits for a person, `\u25D0` for one that goes on. */
  icon: string
  heading: string
  detail: string
}

/**
 * The banner of design 8.3: what a run does now, in one sentence, from the typed members of the status. Nothing is
 * shown for an idle or a failed run (the progress card and the alerts say that); a run that waits for a person is
 * danger and says so; while the manager of the cluster moves, the banner says where it moves to.
 */
export function failoverBanner(run: FailoverStatus | null | undefined): FailoverBannerWords | null {
  if (!run || (run.state !== 'running' && run.state !== 'paused')) return null
  const what =
    run.kind === 'restorePreferredPrimaries'
      ? 'Restoring the preferred primaries'
      : run.trigger === 'automatic'
        ? `Automatic failover of server ${safeText(run.serverId ?? 'unknown', 60)}`
        : run.serverId
          ? `Failover of server ${safeText(run.serverId, 60)}`
          : 'Failover'
  if (run.state === 'paused') {
    const n = run.needsDecision.length
    return {
      tone: 'danger',
      icon: '\u2716',
      heading: `${what} is waiting for you.`,
      detail:
        n > 0
          ? `A decision is needed for ${n === 1 ? '1 group' : `${n} groups`}.`
          : 'It is paused until someone goes on or cancels it.',
    }
  }
  if (run.phase === 'bootstrap') {
    const to = run.groups.find((g) => g.toServerId)?.toServerId
    return {
      tone: 'info',
      icon: '\u25D0',
      heading: to
        ? `The manager is moving to ${safeText(to, 60)}.`
        : 'The manager of the cluster is moving.',
      detail: 'Changes to the cluster wait until it has moved.',
    }
  }
  const total = run.groups.length
  const open = run.groups.findIndex((g) => !['done', 'skipped', 'failed'].includes(g.state))
  const phase = phaseLabel(run.phase)
  const place = total > 0 ? `group ${open < 0 ? total : open + 1} of ${total}` : null
  return {
    tone: 'info',
    icon: '\u25D0',
    heading: `${what} is running${place ? `: ${place}` : ''}.`,
    detail: phase ?? '',
  }
}

export interface AutoFailoverCounterWords {
  /** "1 of 3". */
  counter: string
  /** When the last automatic failover was made, or that there was none. */
  last: string
  limitReached: boolean
  /** The counter is at 0: there is nothing to reset. */
  empty: boolean
}

/**
 * The counter of automatic failovers in words (design 8.4). The limit is read from the numbers, so it also holds for a
 * server that does not send `limitReached` (that member exists on the cluster summary only).
 */
export function autoFailoverCounterWords(af: {
  eventsSinceReset: number
  maxCount: number
  lastEventAt?: string | null
}): AutoFailoverCounterWords {
  return {
    counter: `${af.eventsSinceReset} of ${af.maxCount}`,
    last: af.lastEventAt
      ? `Last automatic failover: ${formatDateTime(af.lastEventAt)}.`
      : 'No automatic failover has been made since the cluster was formed or the counter was reset.',
    limitReached: af.eventsSinceReset >= af.maxCount,
    empty: af.eventsSinceReset === 0,
  }
}

/** What automatic failover needs, in words (design 8.4). */
export const AUTO_FAILOVER_REQUIREMENTS: readonly string[] = [
  'At least three servers, so that a majority can agree that a server has failed.',
  'A standby for every shard group the failed server leads; a group without one cannot be failed over.',
  'A synchronous standby for a failover that cannot lose data. Without one the failover waits for a person to accept the loss.',
]

/**
 * What an operator does about a hard failover of the map group that paused after the old leader was stopped (0.22.0): the server that runs the bootstrap names itself in the
 * message ("Send the request to server X"), because the old leader's own system database is stopped and cannot take the repeated request. Null for any other message.
 */
export function mapGroupPauseHint(message: string | null | undefined): { server: string } | null {
  const m = /Send the request to server (\S+?),? which runs the bootstrap/.exec(message ?? '')
  return m ? { server: m[1]! } : null
}
