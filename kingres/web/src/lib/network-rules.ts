// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Helpers for the network access rules (pg_hba style): reading and writing the lists of database
// and user names, comparing two rule sets, and answering "would this address still be let in?".
import type { NetworkAccessRule } from '@/api/types'

// --- lists of names ------------------------------------------------------------------------

const NEEDS_QUOTES = /[\s,"]/

/** Writes names as a comma-separated line; a name with a comma, a space or a quote is quoted. */
export function formatList(items: string[]): string {
  return items
    .map((item) =>
      item === '' || NEEDS_QUOTES.test(item) ? `"${item.replace(/"/g, '""')}"` : item,
    )
    .join(', ')
}

/** Reads what `formatList` writes. Names are never altered; only separators and quotes are read. */
export function parseList(text: string): string[] {
  const out: string[] = []
  let i = 0
  while (i < text.length) {
    while (i < text.length && /[\s,]/.test(text.charAt(i))) i += 1
    if (i >= text.length) break
    let item = ''
    if (text.charAt(i) === '"') {
      i += 1
      while (i < text.length) {
        const c = text.charAt(i)
        if (c === '"') {
          if (text.charAt(i + 1) === '"') {
            item += '"'
            i += 2
            continue
          }
          i += 1
          break
        }
        item += c
        i += 1
      }
    } else {
      while (i < text.length && text.charAt(i) !== ',') {
        item += text.charAt(i)
        i += 1
      }
      item = item.trim()
    }
    out.push(item)
  }
  return out
}

// --- comparing rule sets -------------------------------------------------------------------

export function describeRule(r: NetworkAccessRule): string {
  const base = `${r.type} ${formatList(r.databases)} ${formatList(r.users)} ${r.address} ${r.method}`
  return r.comment ? `${base} (${r.comment})` : base
}

const same = (a: NetworkAccessRule, b: NetworkAccessRule): boolean =>
  a.type === b.type &&
  a.address === b.address &&
  a.method === b.method &&
  (a.comment ?? '') === (b.comment ?? '') &&
  a.databases.length === b.databases.length &&
  a.users.length === b.users.length &&
  a.databases.every((x, i) => x === b.databases[i]) &&
  a.users.every((x, i) => x === b.users[i])

export interface RuleDiff {
  added: NetworkAccessRule[]
  removed: NetworkAccessRule[]
  /** A rule with the same comment (or address) whose other fields differ. */
  changed: { before: NetworkAccessRule; after: NetworkAccessRule }[]
  /** The same rules in a different order (the first match wins, so order matters). */
  reordered: boolean
}

export function diffRules(before: NetworkAccessRule[], after: NetworkAccessRule[]): RuleDiff {
  const removed = [...before]
  const added: NetworkAccessRule[] = []
  for (const rule of after) {
    const i = removed.findIndex((r) => same(r, rule))
    if (i >= 0) removed.splice(i, 1)
    else added.push(rule)
  }
  const changed: RuleDiff['changed'] = []
  // A removed and an added rule that share a comment (or else an address) are one rule that changed.
  for (const rule of [...added]) {
    let i = rule.comment ? removed.findIndex((r) => r.comment === rule.comment) : -1
    if (i < 0) i = removed.findIndex((r) => r.address === rule.address)
    if (i < 0) continue
    changed.push({ before: removed[i] as NetworkAccessRule, after: rule })
    removed.splice(i, 1)
    added.splice(added.indexOf(rule), 1)
  }
  const reordered =
    added.length === 0 &&
    removed.length === 0 &&
    changed.length === 0 &&
    before.some((r, i) => !same(r, after[i] as NetworkAccessRule))
  return { added, removed, changed, reordered }
}

export const isUnchanged = (d: RuleDiff): boolean =>
  d.added.length === 0 && d.removed.length === 0 && d.changed.length === 0 && !d.reordered

// --- addresses -----------------------------------------------------------------------------

interface Net {
  family: 4 | 6
  bits: bigint
  prefix: number
}

function parseIPv4(text: string): bigint | null {
  const parts = text.split('.')
  if (parts.length !== 4) return null
  let value = 0n
  for (const p of parts) {
    if (!/^\d{1,3}$/.test(p) || Number(p) > 255) return null
    value = (value << 8n) | BigInt(p)
  }
  return value
}

function parseIPv6(text: string): bigint | null {
  let t = text
  const zone = t.indexOf('%')
  if (zone >= 0) t = t.slice(0, zone)
  if (t.includes('.')) {
    // An embedded IPv4 tail (::ffff:10.0.0.1).
    const cut = t.lastIndexOf(':')
    const v4 = parseIPv4(t.slice(cut + 1))
    if (v4 === null) return null
    t = `${t.slice(0, cut + 1)}${((v4 >> 16n) & 0xffffn).toString(16)}:${(v4 & 0xffffn).toString(16)}`
  }
  const halves = t.split('::')
  if (halves.length > 2) return null
  const read = (s: string): number[] | null => {
    if (s === '') return []
    const groups = s.split(':')
    const out: number[] = []
    for (const g of groups) {
      if (!/^[0-9a-fA-F]{1,4}$/.test(g)) return null
      out.push(parseInt(g, 16))
    }
    return out
  }
  const head = read(halves[0] ?? '')
  const tail = halves.length === 2 ? read(halves[1] ?? '') : []
  if (head === null || tail === null) return null
  const missing = 8 - head.length - tail.length
  if (halves.length === 1 ? missing !== 0 : missing < 1) return null
  const all = [...head, ...Array<number>(Math.max(0, missing)).fill(0), ...tail]
  let value = 0n
  for (const g of all) value = (value << 16n) | BigInt(g)
  return value
}

function parseAddress(text: string): { family: 4 | 6; bits: bigint } | null {
  const v4 = parseIPv4(text)
  if (v4 !== null) return { family: 4, bits: v4 }
  const v6 = text.includes(':') ? parseIPv6(text) : null
  return v6 === null ? null : { family: 6, bits: v6 }
}

function parseNet(text: string): Net | 'all' | null {
  if (text === 'all') return 'all'
  const [addr, prefix, ...rest] = text.split('/')
  if (!addr || rest.length > 0) return null
  const a = parseAddress(addr)
  if (!a) return null
  const max = a.family === 4 ? 32 : 128
  const p = prefix === undefined ? max : /^\d{1,3}$/.test(prefix) ? Number(prefix) : -1
  if (p < 0 || p > max) return null
  return { family: a.family, bits: a.bits, prefix: p }
}

/** True/false, or null when the rule's address (or the client address) is not understood. */
export function addressMatches(ruleAddress: string, client: string): boolean | null {
  const net = parseNet(ruleAddress.trim())
  if (net === null) return null
  if (net === 'all') return true
  const c = parseAddress(client.trim())
  if (!c) return null
  if (c.family !== net.family) return false
  const width = c.family === 4 ? 32n : 128n
  const shift = width - BigInt(net.prefix)
  return c.bits >> shift === net.bits >> shift
}

// --- evaluating a rule set -----------------------------------------------------------------

export type Access =
  | { result: 'allowed'; rule: number }
  | { result: 'rejected'; rule: number }
  | { result: 'no-rule' }
  | { result: 'unknown' }

/** Does a user name in a rule's list stand for `user`? null when the entry is a group or a file. */
function userMatches(list: string[], user: string): boolean | null {
  let unknown = false
  for (const entry of list) {
    if (entry === 'all' || entry === user) return true
    if (entry.startsWith('+') || entry.startsWith('@')) unknown = true
  }
  return unknown ? null : false
}

/**
 * Walks the rules from the top like PostgreSQL does (the first match wins) for one client address
 * and user. The connection type (TLS or not) and the database are not considered: the panel does
 * not know how the person connects.
 */
export function evaluateAccess(rules: NetworkAccessRule[], client: string, user: string): Access {
  for (const [i, rule] of rules.entries()) {
    const address = addressMatches(rule.address, client)
    if (address === null) return { result: 'unknown' }
    if (!address) continue
    const named = userMatches(rule.users, user)
    if (named === null) return { result: 'unknown' }
    if (!named) continue
    return rule.method === 'reject'
      ? { result: 'rejected', rule: i }
      : { result: 'allowed', rule: i }
  }
  return { result: 'no-rule' }
}

/** True when at least one rule lets one of `admins` in (a group entry counts as "maybe", so true). */
export function allowsAnyAdministrator(rules: NetworkAccessRule[], admins: string[]): boolean {
  return rules.some(
    (rule) =>
      rule.method !== 'reject' &&
      (rule.users.some((u) => u.startsWith('+') || u.startsWith('@')) ||
        admins.some((a) => userMatches(rule.users, a) === true)),
  )
}
