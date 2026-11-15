// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Standbys (contract 0.7.0): what the replication view says about them, and the rule that decides whether a commit
// waits for one. Nothing here parses a message: the rule is read from the members of the topology and from the alert
// kinds the contract defines.
import type { Alert, ReplicationMember, ShardGroup } from '@/api/types'
import type { Tone } from './status'

/** How the replica-count dialog looks for the task a change started (the leader starts it a moment after the answer). */
export const taskDiscovery = { attempts: 10, intervalMs: 1000 }

/**
 * The instance id of a standby that kingresd manages and that is not in the sharding catalog yet is 1048576 plus the
 * standby's own id (contract text of `ReplicationMember.instanceId`); the console treats an id as opaque, but this one
 * can be told apart for a label.
 */
export const MANAGED_STANDBY_BASE = 1_048_576

/** The own id of a managed standby, or null for a standby that is in the catalog. */
export function managedStandbyId(
  member: Pick<ReplicationMember, 'role' | 'instanceId'>,
): number | null {
  if (member.role !== 'standby' || member.instanceId <= MANAGED_STANDBY_BASE) return null
  return member.instanceId - MANAGED_STANDBY_BASE
}

export interface CommitRule {
  /** `yes`: a commit waits for a standby. `no`: it does not. `unknown`: a standby is not streaming (yet). */
  state: 'yes' | 'no' | 'unknown'
  tone: Tone
  icon: string
  label: string
  text: string
}

const counts = (m: ReplicationMember): boolean => m.syncState === 'sync' || m.syncState === 'quorum'

/**
 * Whether a commit of the group waits for a standby, from what the primary reports for its standbys (their `syncState`:
 * `quorum` or `sync` for the standbys that are in `synchronous_standby_names`) and from the open alert
 * `groupWithoutSynchronousStandby` of the group (raised when the grace period ran out and the primary stopped waiting).
 */
export function commitRule(group: ShardGroup, alerts: Alert[] | null): CommitRule {
  const standbys = group.standbys
  if (standbys.length === 0)
    return {
      state: 'no',
      tone: 'neutral',
      icon: '○',
      label: 'No',
      text: 'This group has no standby, so a commit is acknowledged by the primary alone and is lost with it.',
    }
  const waiting = standbys.filter(counts)
  if (waiting.length > 0)
    return {
      state: 'yes',
      tone: 'ok',
      icon: '✓',
      label: 'Yes',
      text:
        waiting.length === 1
          ? 'A commit waits until the standby that streams has flushed it, so it is on two servers when it is acknowledged.'
          : `A commit waits until any one of the ${waiting.length} standbys that stream has flushed it (the rule is ANY 1 of them), so it is on two servers when it is acknowledged.`,
    }
  const dropped =
    alerts?.some(
      (a) =>
        a.kind === 'groupWithoutSynchronousStandby' &&
        a.state !== 'resolved' &&
        a.subject?.shardGroupId === group.id,
    ) ?? false
  if (dropped)
    return {
      state: 'no',
      tone: 'danger',
      icon: '✖',
      label: 'No, not any more',
      text: 'No standby has streamed for longer than the grace period (30 seconds by default), so the primary stopped waiting for one: commits are acknowledged by one server only. The wait comes back as soon as a standby streams again.',
    }
  if (standbys.every((m) => m.state === 'starting'))
    return {
      state: 'no',
      tone: 'info',
      icon: 'ⓘ',
      label: 'Not yet',
      text: 'The standby is being created. A standby does not count until it streams, so a commit does not wait for it while its base backup is taken.',
    }
  return {
    state: 'unknown',
    tone: 'warn',
    icon: '▲',
    label: 'Not streaming',
    text: 'No standby streams now. During the grace period (30 seconds by default) the primary still waits for the standby that stopped, so commits can be held up; after it the primary stops waiting and the alert "Commits no longer wait for a standby" is raised.',
  }
}

/** The text a step records when it was skipped on purpose: `skipped: needs kingres_shard 0.4`. */
const SKIPPED = /^skipped:\s*(.*)$/i

/** The reason after "skipped:" in a task or step detail, or null when the text does not start with it. */
export function skippedReason(detail: string | null | undefined): string | null {
  if (!detail) return null
  const m = SKIPPED.exec(detail.trim())
  return m ? (m[1] ?? '').trim() || 'no reason given' : null
}
