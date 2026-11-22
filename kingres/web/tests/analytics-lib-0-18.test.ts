// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The words and rules of the analytics console (contract 0.18.0, phase 12): ISO 8601 durations, the policy form and its request,
// when a policy deletes data, the storage column, the refusal reasons, the health line, and the one-line summary of an EXPLAIN
// (4.12 of the query design, 12 of 2b).
import { describe, expect, it } from 'vitest'
import {
  actionCounts,
  actionDetailText,
  durationText,
  formOf,
  healthText,
  isoOf,
  isoSeconds,
  lowersRetention,
  matchesStorage,
  percentToRate,
  pointerField,
  rateToPercent,
  ratioText,
  refusalReasons,
  requestOf,
  splitIso,
  storageKind,
  storageText,
  type PartitionPolicyRequest,
} from '@/lib/analytics'
import { planLines, summarizePlan, summaryText } from '@/lib/plan-summary'

describe('durations', () => {
  it.each([
    ['P7D', 7, 'day'],
    ['PT6H', 6, 'hour'],
    ['P1M', 1, 'month'],
    ['P2W', 2, 'week'],
    ['P1Y', 1, 'year'],
  ])('%s is %i %s', (text, count, unit) => {
    expect(splitIso(text)).toEqual({ count, unit })
    expect(isoOf(count, unit as 'day')).toBe(text)
  })
  it.each(['', 'P', 'PT', 'P1DT12H', 'PT30M', 'PT10S', 'P0D', '7 days', 'p1d', 'P-1D'])(
    '%j is not a duration of one supported unit',
    (text) => expect(splitIso(text)).toBeNull(),
  )
  it('says a duration in words and keeps what it cannot read', () => {
    expect(durationText('P7D')).toBe('7 days')
    expect(durationText('PT1H')).toBe('1 hour')
    expect(durationText('P1DT12H')).toBe('P1DT12H')
    expect(durationText(null)).toBe('')
  })
  it('compares two durations by length', () => {
    expect(isoSeconds('P1D')).toBe(86400)
    expect(isoSeconds('P1M')).toBeGreaterThan(isoSeconds('P4W') as number)
    expect(isoSeconds('nonsense')).toBeNull()
  })
})

describe('the error rate of approximate aggregates', () => {
  it('is a fraction in the contract and a percent in the dialog, without floating point noise', () => {
    expect(rateToPercent(0.01)).toBe(1)
    expect(rateToPercent(0.07)).toBe(7)
    expect(percentToRate(7)).toBe(0.07)
    expect(percentToRate(0.05)).toBe(0.0005)
    expect(percentToRate(10)).toBe(0.1)
  })
})

describe('the policy form', () => {
  const stored: PartitionPolicyRequest = {
    partitionEvery: 'P1D',
    createAhead: 'P7D',
    retention: 'P90D',
    retentionAction: 'drop',
    columnarAfter: 'P7D',
    columnarSortBy: ['ts', 'device'],
    compression: 'zstd',
    compressionLevel: 3,
    timezone: 'Europe/Berlin',
    runEvery: 'PT1H',
    enabled: true,
  }
  it('is the same request when it is read and sent again', () => {
    expect(requestOf(formOf(stored))).toEqual(stored)
  })
  it('starts from the defaults for a table without a policy, with retention and conversion off', () => {
    const r = requestOf(formOf(null))
    expect(r).toMatchObject({
      partitionEvery: 'P1D',
      createAhead: 'P7D',
      retention: null,
      columnarAfter: null,
      columnarSortBy: null,
      compression: null,
      compressionLevel: null,
      timezone: 'UTC',
      runEvery: 'PT1H',
      enabled: true,
    })
  })
  it('sends a custom interval as hours or days', () => {
    const f = formOf({ ...stored, partitionEvery: 'PT6H' })
    expect(f.interval).toBe('custom')
    expect(requestOf(f).partitionEvery).toBe('PT6H')
  })
  it('sends no compression options when conversion is off, and no level for another codec', () => {
    const f = formOf(stored)
    f.columnarOn = false
    expect(requestOf(f)).toMatchObject({
      columnarAfter: null,
      compression: null,
      compressionLevel: null,
    })
    const g = formOf(stored)
    g.compression = 'lz4'
    expect(requestOf(g).compressionLevel).toBeNull()
  })
})

describe('when a policy deletes data', () => {
  const base: PartitionPolicyRequest = {
    partitionEvery: 'P1D',
    retention: 'P90D',
    retentionAction: 'drop',
  }
  it('a new retention does', () => {
    expect(lowersRetention(null, base)).toBe(true)
    expect(lowersRetention({ partitionEvery: 'P1D', retention: null }, base)).toBe(true)
  })
  it('a shorter retention does, an equal or longer one does not', () => {
    expect(lowersRetention(base, { ...base, retention: 'P30D' })).toBe(true)
    expect(lowersRetention(base, { ...base, retention: 'P90D' })).toBe(false)
    expect(lowersRetention(base, { ...base, retention: 'P1Y' })).toBe(false)
  })
  it('turning detach into drop does', () => {
    expect(lowersRetention({ ...base, retentionAction: 'detach' }, base)).toBe(true)
  })
  it('no retention does not', () => {
    expect(lowersRetention(base, { ...base, retention: null })).toBe(false)
  })
})

describe('the storage column', () => {
  const heap = { storage: { accessMethod: 'heap', partitionFormats: null, columnar: null } }
  const col = { storage: { accessMethod: 'columnar', partitionFormats: null, columnar: null } }
  const mixed = {
    storage: {
      accessMethod: null,
      partitionFormats: { heap: 28, columnar: 3, other: 0 },
      columnar: null,
    },
  }
  const allHeap = {
    storage: {
      accessMethod: null,
      partitionFormats: { heap: 5, columnar: 0, other: 0 },
      columnar: null,
    },
  }
  it('names the format', () => {
    expect(storageKind(heap)).toBe('row')
    expect(storageKind(col)).toBe('columnar')
    expect(storageKind(mixed)).toBe('mixed')
    expect(storageKind(allHeap)).toBe('row')
    expect(storageKind({ storage: null })).toBe('unknown')
    expect(storageKind({})).toBe('unknown')
    expect(storageText(mixed)).toBe('Mixed (3 columnar of 31)')
    expect(storageText({ storage: null })).toBe('Not available')
  })
  it('filters: a partitioned table matches columnar when any partition is columnar', () => {
    expect(matchesStorage(mixed, 'columnar')).toBe(true)
    expect(matchesStorage(mixed, 'heap')).toBe(true)
    expect(matchesStorage(allHeap, 'columnar')).toBe(false)
    expect(matchesStorage(col, 'heap')).toBe(false)
    expect(matchesStorage(heap, 'columnar')).toBe(false)
    expect(matchesStorage({ storage: null }, 'all')).toBe(true)
  })
  it('shows a missing ratio as n/a, never as 0', () => {
    expect(ratioText(null)).toBeNull()
    expect(ratioText(undefined)).toBeNull()
    expect(ratioText(6.24)).toBe('6.2 x')
    expect(ratioText(0)).toBe('0.0 x')
  })
})

describe('refusals and details', () => {
  it('splits the reasons of conversion_refused, one per line or per semicolon', () => {
    expect(refusalReasons('is a ledger table\nhas a trigger (a_trg)')).toEqual([
      'is a ledger table',
      'has a trigger (a_trg)',
    ])
    expect(refusalReasons('- one; two')).toEqual(['one', 'two'])
    expect(refusalReasons(null)).toEqual([])
    expect(refusalReasons('')).toEqual([])
  })
  it('puts the scheduler codes in words and leaves the others to the component', () => {
    expect(actionDetailText({ detail: 'swap_lock_timeout', partition: 'p' }, 'events')).toMatch(
      /lock on `events` and gave up; will retry at the next run/,
    )
    expect(actionDetailText({ detail: 'lock_timeout', partition: 'p' }, 'events')).toMatch(
      /gave up/,
    )
    expect(actionDetailText({ detail: 'name_conflict', partition: 'events_p20260101' })).toMatch(
      /another table is called `events_p20260101`/,
    )
    expect(actionDetailText({ detail: 'something_new', partition: 'p' })).toBeNull()
    expect(actionDetailText({ detail: null, partition: 'p' })).toBeNull()
  })
  it('names a path of the pointer of a refused field', () => {
    expect(pointerField('/createAhead')).toBe('createAhead')
    expect(pointerField('/columnarSortBy/0')).toBe('columnarSortBy')
    expect(pointerField('timezone')).toBe('timezone')
  })
  it('counts the actions of a run', () => {
    const a = (
      kind: 'create' | 'convert' | 'drop' | 'detach',
      state: 'done' | 'failed' | 'skipped',
    ) => ({
      seq: 1,
      kind,
      partition: 'p',
      state,
    })
    expect(
      actionCounts([
        a('create', 'done'),
        a('create', 'done'),
        a('convert', 'failed'),
        a('drop', 'done'),
        a('detach', 'done'),
      ]),
    ).toEqual({ created: 2, converted: 0, dropped: 2, failed: 1 })
  })
})

describe('the health line', () => {
  it('says how far ahead, and warns when that is below what the policy keeps', () => {
    expect(healthText({ partitionsAhead: 'P7D', ok: true, problems: [] }, 'P7D')).toBe(
      '7 days ahead.',
    )
    expect(healthText({ partitionsAhead: 'P1D', ok: false, problems: ['aheadLow'] }, 'P7D')).toBe(
      '1 day ahead, below the 7 days the policy keeps.',
    )
    expect(healthText({ partitionsAhead: null, ok: false, problems: ['aheadLow'] }, 'P7D')).toBe(
      'No partition ahead yet.',
    )
  })
})

// The texts that the sharding extension prints (4.12 of the query design, 12 of 2b, 9.8).
const REPARTITIONED = [
  'Custom Scan (Kingres Fragment Scan)',
  '  Pushed: join (orders, events, users_ref: repartitioned)',
  '  Repartition: events by user_id -> placement of orders (colocation 1, 128 buckets), est 1200000 rows, 96 MB; orders anchored; users_ref replicated',
  '  Groups: 3 of 3 (local + 2 remote)',
  '  Remote SQL: SELECT ks_t1.id, ks_t2.kind FROM public.orders AS ks_t1 JOIN (...)',
]
const COLOCATED = [
  'Custom Scan (Kingres Fragment Scan)',
  '  Pushed: join (orders, customers: key class customer_id; customers_ref reference)',
  '  Groups: 3 of 3 (local + 2 remote)',
]
const PARTIAL = [
  'Finalize HashAggregate',
  '  Group Key: ks_t1.region',
  '  ->  Custom Scan (Kingres Fragment Scan)',
  '        Pushed: aggregate (partial)',
  '        Groups: 3 of 3 (local + 2 remote)',
]
const SCAN = [
  'Custom Scan (Kingres Distributed Scan) on orders',
  '  Remote groups: 2 of 3 (pruned by orders.customer_id = 42)',
]
const NOT_PUSHED = [
  'Custom Scan (Kingres Distributed Scan) on orders',
  '  Remote groups: 3 of 3',
  '  Pushdown: not used: a join is not an equality on a hash-compatible type',
  '  Repartition: not used: estimated 800 MB per group exceeds kingres_shard.repartition_max_bytes',
]
const REWRITTEN = [
  'Finalize Aggregate',
  '  Rewritten: count(DISTINCT customer_id) -> approximate (p = 14)',
  '  ->  Custom Scan (Kingres Fragment Scan)',
  '        Pushed: aggregate (partial)',
]

describe('the summary of an EXPLAIN', () => {
  it.each([
    ['a repartitioned join', REPARTITIONED, 'repartitioned join'],
    ['a colocated join', COLOCATED, 'colocated join'],
    ['a partial aggregate', PARTIAL, 'partial aggregate'],
    ['a scan', SCAN, 'scan'],
  ])('reads %s', (_n, lines, path) => {
    expect(summarizePlan(lines)?.path).toBe(path)
  })
  it('keeps the first reason of each line that says a path was not used', () => {
    const s = summarizePlan(NOT_PUSHED)
    expect(s?.path).toBe('scan')
    expect(s?.notUsed).toEqual([
      'a join is not an equality on a hash-compatible type',
      'estimated 800 MB per group exceeds kingres_shard.repartition_max_bytes',
    ])
  })
  it('reads the rewrite of count(DISTINCT), without the precision', () => {
    const s = summarizePlan(REWRITTEN)
    expect(s?.rewritten).toEqual(['count(DISTINCT) -> approximate'])
    expect(s?.path).toBe('partial aggregate')
  })
  it('reads the groups that were asked', () => {
    expect(summarizePlan(SCAN)?.groups).toBe('2 of 3')
    expect(summarizePlan(COLOCATED)?.groups).toBe('3 of 3')
  })
  it('says the whole thing in one line', () => {
    const s = summarizePlan(NOT_PUSHED)
    expect(summaryText(s!)).toBe(
      'Path: scan. Not used: a join is not an equality on a hash-compatible type. Not used: estimated 800 MB per group exceeds kingres_shard.repartition_max_bytes.',
    )
  })
  it('gives no summary for a text with no recognized line', () => {
    expect(summarizePlan(['Seq Scan on t  (cost=0.00..1.00 rows=1 width=4)'])).toBeNull()
    expect(summarizePlan([])).toBeNull()
    expect(summarizePlan(['Pushed lines are not Pushed: lines', 'Remote SQL: SELECT 1'])).toBeNull()
  })
  it('does not guess at a line it does not know', () => {
    expect(summarizePlan(['  Pushed: something new (a, b)'])?.path).toBe('pushed query')
  })
  it('reads only the result of an EXPLAIN (first column QUERY PLAN), line by line', () => {
    const col = (name: string) => [{ name, type: 'text' }]
    expect(planLines({ columns: col('id'), rows: [['Pushed: join (a)']] })).toBeNull()
    expect(planLines({ columns: [], rows: [] })).toBeNull()
    expect(planLines({ columns: col('QUERY PLAN'), rows: [['a'], ['b\nc'], [null]] })).toEqual([
      'a',
      'b',
      'c',
    ])
  })
})
