// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The check of a PostgreSQL parameter value, written once for the edit dialog and for the mock API (design of phase 13,
// "Validation", layer 2). It is a narrow subset of PostgreSQL's own syntax on purpose: PostgreSQL reads `0x10` as 16,
// `010` as 8 and `1e3` as 1000, and rounds `1.5` in a kB setting, so a value that means something else than it looks
// like is refused here and by the server instead of being guessed. What this accepts has one meaning.
//
//  - bool:    on, off, true, false, yes, no, 1, 0 in any letter case; normalized to `on` / `off`.
//  - integer: a decimal number with an optional sign and no leading zero (except `0`); for a parameter with a unit also
//             an optional fraction and an optional unit after an optional space (kB MB GB TB B for sizes; us ms s min h d
//             for times). The value is converted to the parameter's base unit (`pg_settings.unit`, which may be `8kB`),
//             must be an exact whole number there (no silent rounding) and lie within min..max.
//  - real:    a decimal number with an optional sign and fraction, within min..max.
//  - enum:    one of the enum values, ignoring letter case.
//  - string:  valid text of at most 4096 bytes without NUL, CR or LF.
//
// A value must have no space around it; this is a deliberate refusal, not PostgreSQL's rule.

/** What the check needs to know about a parameter: the members of `ConfigParameter` it reads. */
export interface ValueSpec {
  type: 'bool' | 'integer' | 'real' | 'string' | 'enum'
  unit: string | null
  min: string | null
  max: string | null
  enumValues: string[] | null
}

export type ValueCheck =
  | {
      ok: true
      /** The text that is stored and shown: `on`, `64MB`, `Asia/Tehran`. */
      normalized: string
      /** The value in the parameter's base unit (an integer or real as text), or the text for other types. */
      base: string
    }
  | { ok: false; message: string }

export const MAX_VALUE_BYTES = 4096

// Units as an exact number of the smallest unit of their family: bytes for sizes, microseconds for times.
const SIZE_FACTOR: Record<string, bigint> = {
  B: 1n,
  kB: 1024n,
  MB: 1024n ** 2n,
  GB: 1024n ** 3n,
  TB: 1024n ** 4n,
}
const TIME_FACTOR: Record<string, bigint> = {
  us: 1n,
  ms: 1000n,
  s: 1_000_000n,
  min: 60_000_000n,
  h: 3_600_000_000n,
  d: 86_400_000_000n,
}

/** The base unit of a parameter as an exact factor of its family, or null when the unit is not one we know. */
function baseUnit(unit: string): { family: 'size' | 'time'; factor: bigint } | null {
  const m = /^(\d+)?(B|kB|MB|GB|TB)$/.exec(unit)
  if (m)
    return { family: 'size', factor: BigInt(m[1] ?? '1') * (SIZE_FACTOR[m[2] as string] as bigint) }
  const t = TIME_FACTOR[unit]
  return t === undefined ? null : { family: 'time', factor: t }
}

/** Units a person may type for a parameter whose base unit is `unit`. */
export function typableUnits(unit: string | null): string[] {
  const base = unit === null ? null : baseUnit(unit)
  if (!base) return []
  return base.family === 'size' ? Object.keys(SIZE_FACTOR) : Object.keys(TIME_FACTOR)
}

/**
 * Writes a number of base units the way PostgreSQL's `SHOW` does: in the largest unit that divides it exactly
 * (`131072` in `8kB` is `1GB`, `120000` ms is `2min`). A parameter without a unit is written as the plain number.
 */
export function formatBase(base: string, unit: string | null): string {
  if (unit === null) return base
  const info = baseUnit(unit)
  // Zero and the negative "off" values (-1) are shown as the bare number, as `SHOW` does.
  if (!info || !/^\d+$/.test(base) || BigInt(base) === 0n) return base
  const total = BigInt(base) * info.factor
  const ladder =
    info.family === 'size'
      ? (['TB', 'GB', 'MB', 'kB', 'B'] as const)
      : (['d', 'h', 'min', 's', 'ms', 'us'] as const)
  const table = info.family === 'size' ? SIZE_FACTOR : TIME_FACTOR
  for (const u of ladder) {
    const f = table[u] as bigint
    if (total % f === 0n) return `${total / f}${u}`
  }
  return `${base}${unit}`
}

const INTEGER_RE = /^[+-]?(0|[1-9][0-9]*)$/
const DECIMAL_RE = /^([+-]?)(0|[1-9][0-9]*)(?:\.([0-9]+))?$/
const WITH_UNIT_RE = /^([+-]?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?) ?([A-Za-z]+)?$/
const BOOL_WORDS: Record<string, 'on' | 'off'> = {
  on: 'on',
  off: 'off',
  true: 'on',
  false: 'off',
  yes: 'on',
  no: 'off',
  '1': 'on',
  '0': 'off',
}

const fail = (message: string): ValueCheck => ({ ok: false, message })

/** A decimal as an exact fraction numerator / 10^digits. */
function toFraction(text: string): { num: bigint; den: bigint } | null {
  const m = DECIMAL_RE.exec(text)
  if (!m) return null
  const frac = m[3] ?? ''
  const num = BigInt(`${m[2]}${frac}`) * (m[1] === '-' ? -1n : 1n)
  return { num, den: 10n ** BigInt(frac.length) }
}

function rangeWords(spec: ValueSpec): string {
  const u = spec.unit ?? ''
  // PostgreSQL's largest double is a bound nobody can reach: it says "no upper limit", not 1.79769e+308.
  const max = spec.max !== null && Number(spec.max) >= 1e300 ? null : spec.max
  if (spec.min !== null && max !== null) return `between ${spec.min}${u} and ${max}${u}`
  if (spec.min !== null) return `at least ${spec.min}${u}`
  if (max !== null) return `at most ${max}${u}`
  return ''
}

/** A sentence that says what a parameter of this type accepts (shown beside the control). */
export function describeSpec(spec: ValueSpec): string {
  switch (spec.type) {
    case 'bool':
      return 'on or off (also true, false, yes, no, 1, 0).'
    case 'enum':
      return `One of: ${(spec.enumValues ?? []).join(', ')}.`
    case 'string':
      return `Text of at most ${MAX_VALUE_BYTES} bytes, on one line.`
    case 'real': {
      const r = rangeWords(spec)
      return `A decimal number${r ? ` ${r}` : ''}.`
    }
    case 'integer': {
      const r = rangeWords(spec)
      const unit = spec.unit
        ? ` in ${spec.unit}, or with a unit (${typableUnits(spec.unit).join(', ')})`
        : ''
      return `A whole number${unit}${r ? `, ${r}` : ''}.`
    }
  }
}

/** Checks `input` against a parameter. Never throws. */
export function checkConfigValue(input: string, spec: ValueSpec): ValueCheck {
  switch (spec.type) {
    case 'bool':
      return checkBool(input)
    case 'enum':
      return checkEnum(input, spec)
    case 'string':
      return checkString(input)
    case 'real':
      return checkReal(input, spec)
    case 'integer':
      return checkInteger(input, spec)
  }
}

function spaces(input: string): string | null {
  return input !== input.trim() || input === ''
    ? input === ''
      ? 'The value is empty.'
      : 'Remove the spaces before and after the value.'
    : null
}

function checkBool(input: string): ValueCheck {
  const problem = spaces(input)
  if (problem) return fail(problem)
  const word = BOOL_WORDS[input.toLowerCase()]
  return word
    ? { ok: true, normalized: word, base: word }
    : fail('Use on, off, true, false, yes, no, 1 or 0.')
}

function checkEnum(input: string, spec: ValueSpec): ValueCheck {
  const problem = spaces(input)
  if (problem) return fail(problem)
  const hit = (spec.enumValues ?? []).find((v) => v.toLowerCase() === input.toLowerCase())
  return hit === undefined
    ? fail(`Use one of: ${(spec.enumValues ?? []).join(', ')}.`)
    : { ok: true, normalized: hit, base: hit }
}

function hasLoneSurrogate(text: string): boolean {
  for (let i = 0; i < text.length; i += 1) {
    const c = text.charCodeAt(i)
    if (c >= 0xd800 && c <= 0xdbff) {
      const next = text.charCodeAt(i + 1)
      if (next >= 0xdc00 && next <= 0xdfff) i += 1
      else return true
    } else if (c >= 0xdc00 && c <= 0xdfff) return true
  }
  return false
}

function checkString(input: string): ValueCheck {
  if (input.includes('\u0000')) return fail('The value must not contain a NUL character.')
  if (/[\r\n]/.test(input))
    return fail('The value must be on one line: no carriage return or line feed.')
  if (hasLoneSurrogate(input)) return fail('The value is not valid text (it has a lone surrogate).')
  const bytes = new TextEncoder().encode(input).length
  if (bytes > MAX_VALUE_BYTES)
    return fail(`The value has ${bytes} bytes; at most ${MAX_VALUE_BYTES} are allowed.`)
  return { ok: true, normalized: input, base: input }
}

const NOT_DECIMAL =
  'Write the number in decimal: hexadecimal, octal, exponent forms and leading zeros are not accepted.'

function checkReal(input: string, spec: ValueSpec): ValueCheck {
  const problem = spaces(input)
  if (problem) return fail(problem)
  const f = toFraction(input)
  if (!f) return fail(`A decimal number is expected. ${NOT_DECIMAL}`)
  const value = Number(input)
  if (!Number.isFinite(value)) return fail('The number is too large.')
  const min = spec.min === null ? null : Number(spec.min)
  const max = spec.max === null ? null : Number(spec.max)
  if ((min !== null && value < min) || (max !== null && value > max))
    return fail(`The value must be a decimal number ${rangeWords(spec)}.`)
  const normalized = String(value)
  return { ok: true, normalized, base: normalized }
}

function checkInteger(input: string, spec: ValueSpec): ValueCheck {
  const problem = spaces(input)
  if (problem) return fail(problem)
  const info = spec.unit === null ? null : baseUnit(spec.unit)
  // A unit that this checker does not know (`8kB` and `MB` are known): the number is taken as it is.
  const withUnit = spec.unit !== null && info !== null
  let base: bigint
  if (!withUnit) {
    if (!INTEGER_RE.test(input)) {
      // Hexadecimal, octal, exponent and leading-zero forms are named as such before "no unit" is guessed.
      const otherBase = /^[+-]?0[xXoObB]|^[+-]?\d+(?:\.\d+)?[eE][+-]?\d+$|^[+-]?0\d/.test(input)
      return fail(
        otherBase
          ? `A whole number is expected. ${NOT_DECIMAL}`
          : /^[+-]?\d+\.\d*$/.test(input) || /^[+-]?\d*\.\d+$/.test(input)
            ? 'A whole number is expected: this setting has no fraction.'
            : /^[+-]?\d+(?:\.\d+)? ?[A-Za-z]+$/.test(input)
              ? 'A whole number without a unit is expected: this setting has no unit.'
              : `A whole number is expected. ${NOT_DECIMAL}`,
      )
    }
    base = BigInt(input)
  } else {
    const m = WITH_UNIT_RE.exec(input)
    if (!m) return fail(`A number, with an optional unit, is expected. ${NOT_DECIMAL}`)
    const number = m[1] as string
    const typed = m[2]
    const family = (info as NonNullable<typeof info>).family
    const table = family === 'size' ? SIZE_FACTOR : TIME_FACTOR
    // Without a unit the number is in the parameter's own unit; the factor is then the base unit itself.
    const factor = typed === undefined ? (info as NonNullable<typeof info>).factor : table[typed]
    if (factor === undefined) {
      return fail(
        `The unit "${typed}" is not known for this setting. Use ${Object.keys(table).join(', ')}.`,
      )
    }
    const f = toFraction(number)
    if (!f) return fail(`A number is expected. ${NOT_DECIMAL}`)
    const scaled = f.num * factor
    const divisor = f.den * (info as NonNullable<typeof info>).factor
    if (scaled % divisor !== 0n) {
      return fail(
        `The value is not a whole number of ${spec.unit}. KingresDB does not round: write it in a smaller unit or as a whole number.`,
      )
    }
    base = scaled / divisor
  }
  const min = spec.min === null ? null : BigInt(spec.min)
  const max = spec.max === null ? null : BigInt(spec.max)
  if ((min !== null && base < min) || (max !== null && base > max)) {
    return fail(`The value must be a whole number ${rangeWords(spec)}.`)
  }
  return { ok: true, normalized: formatBase(String(base), spec.unit), base: String(base) }
}

/** The members of a `ConfigParameter` the check reads. */
export function specOf(p: ValueSpec): ValueSpec {
  return { type: p.type, unit: p.unit, min: p.min, max: p.max, enumValues: p.enumValues }
}

/**
 * The value of a stored or shown text (`64MB`, `on`) as a number of base units, or null when it is not a number.
 * Used to compare two values and to tell which way a value goes.
 */
export function baseNumber(text: string, spec: ValueSpec): number | null {
  const check = checkConfigValue(text, spec)
  if (!check.ok) return null
  const n = Number(check.base)
  return Number.isFinite(n) ? n : null
}
