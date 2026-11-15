// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
import { describe, expect, it } from 'vitest'
import {
  addressMatches,
  allowsAnyAdministrator,
  diffRules,
  evaluateAccess,
  formatList,
  isUnchanged,
  parseList,
} from '@/lib/network-rules'
import type { NetworkAccessRule } from '@/api/types'

const rule = (o: Partial<NetworkAccessRule> = {}): NetworkAccessRule => ({
  type: 'host',
  databases: ['all'],
  users: ['all'],
  address: '10.0.0.0/24',
  method: 'scram-sha-256',
  comment: null,
  ...o,
})

describe('lists of names', () => {
  it('round-trips names that contain commas, quotes and spaces', () => {
    for (const items of [
      ['all'],
      ['a,b', 'c'],
      ['say "hi"', ' lead', 'trail '],
      ['', 'x'],
      ['‮nimda'],
    ]) {
      expect(parseList(formatList(items))).toEqual(items)
    }
  })

  it('reads a hand-written line', () => {
    expect(parseList('shop, "a,b" ,  reports')).toEqual(['shop', 'a,b', 'reports'])
  })
})

describe('addresses', () => {
  it('matches IPv4 and IPv6 networks', () => {
    expect(addressMatches('10.0.0.0/24', '10.0.0.50')).toBe(true)
    expect(addressMatches('10.0.0.0/24', '10.0.1.50')).toBe(false)
    expect(addressMatches('all', '203.0.113.9')).toBe(true)
    expect(addressMatches('fd00::/8', 'fd12:3456::1')).toBe(true)
    expect(addressMatches('fd00::/8', '10.0.0.1')).toBe(false)
    expect(addressMatches('10.0.0.1', '10.0.0.1')).toBe(true)
    expect(addressMatches('not-an-address', '10.0.0.1')).toBeNull()
  })

  it('applies the first matching rule, like PostgreSQL', () => {
    const rules = [
      rule({ address: '10.0.0.50/32', method: 'reject' }),
      rule({ address: '10.0.0.0/24' }),
    ]
    expect(evaluateAccess(rules, '10.0.0.50', 'admin')).toEqual({ result: 'rejected', rule: 0 })
    expect(evaluateAccess(rules, '10.0.0.51', 'admin')).toEqual({ result: 'allowed', rule: 1 })
    expect(evaluateAccess(rules, '192.168.1.1', 'admin')).toEqual({ result: 'no-rule' })
    expect(evaluateAccess([rule({ users: ['+staff'] })], '10.0.0.5', 'admin')).toEqual({
      result: 'unknown',
    })
  })

  it('finds out whether any rule lets an administrator in', () => {
    expect(allowsAnyAdministrator([rule({ users: ['app'] })], ['admin'])).toBe(false)
    expect(allowsAnyAdministrator([rule({ users: ['admin'] })], ['admin'])).toBe(true)
    expect(allowsAnyAdministrator([rule({ method: 'reject' })], ['admin'])).toBe(false)
    expect(allowsAnyAdministrator([rule({ users: ['+ops'] })], ['admin'])).toBe(true)
  })
})

describe('comparing rule sets', () => {
  it('reports added, removed, changed and reordered rules', () => {
    const a = rule({ comment: 'one' })
    const b = rule({ comment: 'two', address: '10.1.0.0/16' })
    expect(isUnchanged(diffRules([a, b], [a, b]))).toBe(true)
    expect(diffRules([a, b], [b, a]).reordered).toBe(true)
    expect(diffRules([a, b], [a]).removed).toEqual([b])
    expect(diffRules([a], [a, b]).added).toEqual([b])
    const changed = diffRules([a, b], [a, { ...b, address: '10.9.0.0/16' }])
    expect(changed.changed).toHaveLength(1)
    expect(changed.added).toHaveLength(0)
  })
})
