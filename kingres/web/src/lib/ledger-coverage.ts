// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Which shard groups a ledger digest or a verification covered (contract 0.6.0: `complete` and
// `uncoveredShardGroupIds`). A verification that passed over only some of the chains says nothing about the
// others, so the panel never presents a digest or a verification as covering the whole database unless the
// server said `complete: true` and named no uncovered group. A server that sends neither member (an older
// one) did not say, and the panel does not guess.
import type { LedgerDigest, LedgerVerification } from '@/api/types'
import type { StatusMeta } from './status'

export type Coverage =
  { state: 'complete' } | { state: 'incomplete'; uncovered: number[] } | { state: 'unknown' }

type Covering = Pick<LedgerDigest | LedgerVerification, 'complete' | 'uncoveredShardGroupIds'>

export function coverageOf(x: Covering): Coverage {
  const uncovered = Array.isArray(x.uncoveredShardGroupIds)
    ? x.uncoveredShardGroupIds.filter((n): n is number => typeof n === 'number')
    : []
  // A list that names groups wins over `complete: true`: the safe reading of a contradiction.
  if (x.complete === false || uncovered.length > 0) return { state: 'incomplete', uncovered }
  if (x.complete === true) return { state: 'complete' }
  return { state: 'unknown' }
}

/** "shard group 3" or "shard groups 3, 5, 7" (numbers only, so nothing the server sent is interpreted as text). */
export function groupList(ids: number[]): string {
  if (ids.length === 0) return 'one or more shard groups (the server did not name them)'
  return `${ids.length === 1 ? 'shard group' : 'shard groups'} ${ids.map((n) => String(n)).join(', ')}`
}

/** One sentence for a digest; empty when it covers every chain. */
export function digestCoverageText(c: Coverage): string {
  if (c.state === 'complete') return ''
  if (c.state === 'unknown')
    return 'This server did not say whether every shard group is covered by this digest.'
  return `This digest does not cover ${groupList(c.uncovered)}: it proves nothing about them.`
}

/** One sentence for a verification; empty when it covered every chain. */
export function verificationCoverageText(c: Coverage): string {
  if (c.state === 'complete') return ''
  if (c.state === 'unknown')
    return 'This server did not say whether every shard group is covered by this verification, so it is not shown as a check of the whole database.'
  return `This verification did not cover ${groupList(c.uncovered)}: it says nothing about them, and it is not a check of the whole database.`
}

/**
 * The badge of a verification. "Passed" with a complete cover has a check mark; a pass that is
 * incomplete or of unknown cover has another symbol and other words, so the difference is never colour alone.
 */
export function verificationBadge(v: Pick<LedgerVerification, 'state'> & Covering): StatusMeta {
  const cover = coverageOf(v)
  if (v.state === 'passed') {
    if (cover.state === 'incomplete')
      return { tone: 'warn', icon: '◩', label: 'Passed, incomplete' }
    if (cover.state === 'unknown')
      return { tone: 'info', icon: '?', label: 'Passed, coverage not stated' }
    return { tone: 'ok', icon: '✓', label: 'Passed, all groups' }
  }
  return STATE_ONLY[v.state] ?? { tone: 'neutral', icon: '?', label: String(v.state) }
}

const STATE_ONLY: Record<string, StatusMeta> = {
  never: { tone: 'neutral', icon: '○', label: 'Never verified' },
  running: { tone: 'info', icon: '◐', label: 'Verifying' },
  failed: { tone: 'danger', icon: '✖', label: 'Inconsistent' },
}
