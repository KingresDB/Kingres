// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Partial cluster-wide answers (contract 0.5.0). `listLogs`, `listAlerts` and `queryMetrics` are
// answered by asking every server; a server that did not answer is named in `missingServers` with a
// reason and the answer is flagged `partial`. This module turns that into one shape for the views,
// the words for each reason, and a signature that changes only when what is missing changes (so a
// poll that finds the same servers missing is not announced again).
import type { MissingServer } from '@/api/types'
import { safeText } from './safe-text'

export interface Completeness {
  partial: boolean
  missing: MissingServer[]
}

export interface MaybePartial {
  partial?: boolean
  missingServers?: MissingServer[]
}

export const COMPLETE: Completeness = { partial: false, missing: [] }

/** The reasons of the contract in plain words. A reason from a newer server gets a general sentence. */
const REASON_WORDS: Record<string, string> = {
  timeout: 'did not answer in time',
  unreachable: 'could not be reached',
  refused: 'refused the request',
  invalidAnswer: 'sent an answer that could not be used',
}

export function reasonWords(reason: string): string {
  return REASON_WORDS[reason] ?? 'did not give an answer for a reason this panel does not know'
}

/** One answer's completeness. `partial` alone (without names) is still incomplete. */
export function completenessOf(answer: MaybePartial | undefined): Completeness {
  if (!answer) return COMPLETE
  const missing = answer.missingServers ?? []
  return { partial: answer.partial === true || missing.length > 0, missing: [...missing] }
}

/** Several answers shown together (pages of one list, or the charts of one view). */
export function mergeCompleteness(answers: (MaybePartial | undefined)[]): Completeness {
  let partial = false
  const seen = new Set<string>()
  const missing: MissingServer[] = []
  for (const answer of answers) {
    const one = completenessOf(answer)
    partial ||= one.partial
    for (const m of one.missing) {
      const key = `${m.serverId}\u0000${m.reason}`
      if (seen.has(key)) continue
      seen.add(key)
      missing.push(m)
    }
  }
  missing.sort((a, b) => (a.serverId < b.serverId ? -1 : a.serverId > b.serverId ? 1 : 0))
  return { partial, missing }
}

/** Equal for equal sets of missing servers and reasons; empty when the answer is complete. */
export function completenessSignature(c: Completeness): string {
  if (!c.partial) return ''
  return `partial|${c.missing.map((m) => `${m.serverId}:${m.reason}`).join('|')}`
}

/** The sentence a screen reader gets: server ids go through `safeText`, so hidden characters are named. */
export function completenessAnnouncement(c: Completeness, what: string): string {
  if (!c.partial) return `The ${what} is complete again.`
  if (c.missing.length === 0) return `This ${what} is incomplete. Some servers did not answer.`
  const names = c.missing.map((m) => `server ${safeText(m.serverId, 60)} ${reasonWords(m.reason)}`)
  return `This ${what} is incomplete. ${names.join('; ')}.`
}
