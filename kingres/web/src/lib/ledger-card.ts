// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// What the dashboard card "Ledger integrity" says (contract 0.11.0, design 8.5 and 6.9): one line of state, the age of the
// newest anchored aggregate, how many chains were read of how many are expected, the last verification and its result.
// The states that exist without anchors are shown on purpose: `Off` (no database uses the ledger), `Not anchored` (the
// ledger works but nothing outside the cluster vouches for it: information, never green), `Healthy` and `Attention`.
import type { Alert, LedgerStatus } from '@/api/types'
import { coverageOf } from './ledger-coverage'
import { LEDGER_ALERT_KINDS, type CardState } from './ledger-signing'

export interface LedgerCard {
  state: CardState
  /** The open alert of a ledger kind with the highest severity, when state is `attention`. */
  worstAlert: Alert | null
  /** Why `attention`, when no alert says it (a verification that failed, a log that diverged). */
  reasons: string[]
  /** Evidence time of the newest anchored aggregate over every database; null when none was anchored. */
  newestAnchoredAt: string | null
  /** Chains read, and chains expected; `expected` is null when a server did not say what is missing. */
  chainsRead: number
  chainsExpected: number | null
  lastVerifiedAt: string | null
  lastVerificationState: LedgerStatus['verificationState']
  /** Databases whose ledger is not anchored (no enabled target). */
  notAnchored: string[]
}

const RANK: Record<string, number> = { critical: 3, warning: 2, info: 1 }

export function ledgerCard(
  statuses: readonly LedgerStatus[],
  alerts: readonly Alert[],
  unreadable = 0,
): LedgerCard {
  const open = alerts.filter(
    (a) =>
      a.state === 'open' && LEDGER_ALERT_KINDS.includes(a.kind) && a.kind !== 'ledgerNotAnchored',
  )
  const worstAlert =
    [...open].sort((a, b) => (RANK[b.severity] ?? 0) - (RANK[a.severity] ?? 0))[0] ?? null
  const worst = worstAlert !== null && (RANK[worstAlert.severity] ?? 0) >= 2 ? worstAlert : null

  let chainsRead = 0
  let expected: number | null = 0
  let newest: string | null = null
  let lastAt: string | null = null
  let lastState: LedgerStatus['verificationState'] = null
  const reasons: string[] = []
  const notAnchored: string[] = []
  for (const s of statuses) {
    chainsRead += s.chains.length
    const c = coverageOf(s)
    if (c.state === 'complete') expected = expected === null ? null : expected + s.chains.length
    else if (c.state === 'incomplete') {
      expected = expected === null ? null : expected + s.chains.length + c.uncovered.length
      // The groups it names are known; an unnamed gap is not a number.
      if (c.uncovered.length === 0) expected = null
    } else expected = null
    const at = s.anchoring?.newestAnchoredAt ?? null
    if (at !== null && (newest === null || Date.parse(at) > Date.parse(newest))) newest = at
    if (s.anchoring?.state === 'none') notAnchored.push(s.databaseName)
    if (s.anchoring?.state === 'failed') reasons.push('Anchoring failed for a database.')
    if (s.anchoring?.state === 'behind') reasons.push('Anchoring is behind for a database.')
    if (s.publication?.state === 'diverged')
      reasons.push('A chain diverged from its published log.')
    if (s.publication?.state === 'behind') reasons.push('Publication is behind for a database.')
    if (s.verificationState === 'failed')
      reasons.push('The last verification found inconsistencies.')
    if (
      s.lastVerifiedAt !== null &&
      (lastAt === null || Date.parse(s.lastVerifiedAt) > Date.parse(lastAt))
    ) {
      lastAt = s.lastVerifiedAt
      lastState = s.verificationState
    }
  }
  const unique = [...new Set(reasons)]
  let state: CardState
  if (statuses.length === 0 && unreadable === 0) state = 'off'
  else if (worst !== null || unique.length > 0 || unreadable > 0) state = 'attention'
  else if (notAnchored.length > 0) state = 'notAnchored'
  else state = 'healthy'
  return {
    state,
    worstAlert: worst,
    reasons:
      unreadable > 0 ? [...unique, 'The ledger status of a database could not be read.'] : unique,
    newestAnchoredAt: newest,
    chainsRead,
    chainsExpected: expected,
    lastVerifiedAt: lastAt,
    lastVerificationState: lastState,
    notAnchored,
  }
}
