// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The checker of parameter values (design of phase 13, "Validation", layer 2): every vector of the shared table, the
// way values are written back, and the sentences that name the type, the unit and the range.
import { describe, expect, it } from 'vitest'
import {
  baseNumber,
  checkConfigValue,
  describeSpec,
  formatBase,
  specOf,
  typableUnits,
} from '@/lib/config-value'
import { SPECS, VECTORS } from './config-value-vectors'

describe('the shared test vectors', () => {
  it('has a case of every type and of both outcomes', () => {
    const types = new Set(VECTORS.map((v) => SPECS[v.spec].type))
    expect([...types].sort()).toEqual(['bool', 'enum', 'integer', 'real', 'string'])
    expect(VECTORS.filter((v) => v.normalized === null).length).toBeGreaterThan(50)
    expect(VECTORS.filter((v) => v.normalized !== null).length).toBeGreaterThan(50)
  })

  for (const v of VECTORS) {
    const shown = v.input.length > 40 ? `${v.input.slice(0, 12)}... (${v.input.length})` : v.input
    const label = `${v.spec}: ${JSON.stringify(shown)}`
    it(v.normalized === null ? `refuses ${label}` : `accepts ${label}`, () => {
      const r = checkConfigValue(v.input, SPECS[v.spec])
      if (v.normalized === null) {
        expect(r.ok).toBe(false)
        if (!r.ok) expect(r.message.length).toBeGreaterThan(5)
      } else {
        expect(r.ok).toBe(true)
        if (r.ok) {
          expect(r.normalized).toBe(v.normalized)
          if (v.base !== undefined) expect(r.base).toBe(v.base)
          // What comes back is itself accepted and means the same: a value can be stored and read again.
          const again = checkConfigValue(r.normalized, SPECS[v.spec])
          expect(again.ok).toBe(true)
          if (again.ok) expect(again.base).toBe(r.base)
        }
      }
    })
  }
})

describe('the messages', () => {
  it('say the type, the unit and the range', () => {
    expect(checkConfigValue('63kB', SPECS.kb)).toEqual({
      ok: false,
      message: expect.stringMatching(/whole number between 64kB and 2147483647kB/),
    })
    expect(checkConfigValue('1.5', SPECS.count)).toEqual({
      ok: false,
      message: expect.stringMatching(/no fraction/i),
    })
    expect(checkConfigValue('5MB', SPECS.count)).toEqual({
      ok: false,
      message: expect.stringMatching(/no unit/i),
    })
    expect(checkConfigValue('0x10', SPECS.count)).toEqual({
      ok: false,
      message: expect.stringMatching(/hexadecimal.*not accepted/i),
    })
    expect(checkConfigValue('0.5kB', SPECS.kb)).toEqual({
      ok: false,
      message: expect.stringMatching(/does not round/i),
    })
    expect(checkConfigValue('4 xb', SPECS.kb)).toEqual({
      ok: false,
      message: expect.stringMatching(/unit "xb" is not known.*B, kB, MB, GB, TB/),
    })
    expect(checkConfigValue(' on', SPECS.bool)).toEqual({
      ok: false,
      message: expect.stringMatching(/spaces/i),
    })
    expect(checkConfigValue('maybe', SPECS.mode)).toEqual({
      ok: false,
      message: expect.stringMatching(/local, remote_write/),
    })
  })

  it('describe what each type accepts', () => {
    expect(describeSpec(SPECS.bool)).toMatch(/on or off/)
    expect(describeSpec(SPECS.kb)).toMatch(
      /whole number in kB.*kB, MB, GB, TB.*between 64kB and 2147483647kB/s,
    )
    expect(describeSpec(SPECS.mode)).toMatch(/One of: local/)
    expect(describeSpec(SPECS.text)).toMatch(/4096 bytes/)
    expect(describeSpec(SPECS.fraction)).toMatch(/decimal number between 0 and 1/)
  })
})

describe('units and writing values back', () => {
  it('writes a number of base units in the largest unit that divides it', () => {
    expect(formatBase('16384', '8kB')).toBe('128MB')
    expect(formatBase('131072', '8kB')).toBe('1GB')
    expect(formatBase('120000', 'ms')).toBe('2min')
    expect(formatBase('1500', 'ms')).toBe('1500ms')
    expect(formatBase('90', 's')).toBe('90s')
    expect(formatBase('1048576', 'kB')).toBe('1GB')
    expect(formatBase('0', 'ms')).toBe('0')
    expect(formatBase('-1', 'ms')).toBe('-1')
    expect(formatBase('5', null)).toBe('5')
  })

  it('offers the units of the parameter family only', () => {
    expect(typableUnits('kB')).toEqual(['B', 'kB', 'MB', 'GB', 'TB'])
    expect(typableUnits('8kB')).toEqual(['B', 'kB', 'MB', 'GB', 'TB'])
    expect(typableUnits('ms')).toEqual(['us', 'ms', 's', 'min', 'h', 'd'])
    expect(typableUnits(null)).toEqual([])
  })

  it('compares two values in the base unit', () => {
    const spec = specOf({ ...SPECS.kb })
    expect(baseNumber('4MB', spec)).toBe(4096)
    expect(baseNumber('nonsense', spec)).toBeNull()
    expect(baseNumber('8MB', spec)! > baseNumber('4MB', spec)!).toBe(true)
  })
})
