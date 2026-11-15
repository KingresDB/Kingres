// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The raw slice extractor: the exact text of a value inside a JSON document, so that a file that
// `kingres-verify` reads can be saved byte for byte.
import { describe, expect, it } from 'vitest'
import { rawSlices } from '@/lib/raw-json'

const doc = '{ "z":1 ,\n\t"a" : 9007199254740993,"s":"} ] \\" , {","n":[1, 2 ]}'

describe('rawSlices', () => {
  it('returns the exact characters of the value, not a re-serialization', () => {
    const text = `{"items":[{"chains":[{"shardGroupId":1,"document":${doc}},{"document":  ${doc} ,"shardGroupId":2}]}],"nextCursor":null}`
    const found = rawSlices(text, (p) => p.length === 5 && p[0] === 'items' && p[4] === 'document')
    expect(found.map((f) => f.raw)).toEqual([doc, doc])
    expect(found[0]?.path).toEqual(['items', 0, 'chains', 0, 'document'])
    expect(found[1]?.path).toEqual(['items', 0, 'chains', 1, 'document'])
    // A float cannot hold this integer: JSON.parse would have changed it.
    expect(String(JSON.parse(doc).a)).not.toBe('9007199254740993')
    expect(found[0]?.raw).toContain('9007199254740993')
  })

  it('is not confused by quotes, brackets and escapes inside strings', () => {
    const text = '{"a":"x\\"y{[","document":{"k":"}"},"b":["document"]}'
    expect(rawSlices(text, (p) => p.at(-1) === 'document').map((f) => f.raw)).toEqual(['{"k":"}"}'])
  })

  it('finds a scalar at the top level', () => {
    const text = '{"ledger_txid": 12345678901234567890 ,"x":1}'
    expect(rawSlices(text, (p) => p.length === 1 && p[0] === 'ledger_txid')[0]?.raw).toBe(
      '12345678901234567890',
    )
  })
})
