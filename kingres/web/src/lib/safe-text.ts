// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Names and messages that come from the database are chosen by whoever may create a role, a table
// or a column, and by whoever can make the server log something. They can contain characters that
// make one string look like another: bidirectional controls (U+202E reverses what follows),
// zero-width and other invisible characters, control characters (ESC, NUL, BEL, line breaks) and
// look-alike letters of other scripts. This module finds them. Nothing here changes what is sent
// to the server: it only decides how a string is shown.

export interface Piece {
  /** Plain text to show, or the escape label when `escape` is set. */
  text: string
  escape?: { code: string; description: string }
}

const NAMES: Record<number, string> = {
  0x00: 'null',
  0x07: 'bell',
  0x08: 'backspace',
  0x09: 'tab',
  0x0a: 'line feed',
  0x0b: 'vertical tab',
  0x0c: 'form feed',
  0x0d: 'carriage return',
  0x1b: 'escape',
  0x7f: 'delete',
  0x85: 'next line',
  0xa0: 'no-break space',
  0xad: 'soft hyphen',
  0x061c: 'arabic letter mark',
  0x180e: 'mongolian vowel separator',
  0x200b: 'zero width space',
  0x200c: 'zero width non-joiner',
  0x200d: 'zero width joiner',
  0x200e: 'left-to-right mark',
  0x200f: 'right-to-left mark',
  0x2028: 'line separator',
  0x2029: 'paragraph separator',
  0x202a: 'left-to-right embedding',
  0x202b: 'right-to-left embedding',
  0x202c: 'pop directional formatting',
  0x202d: 'left-to-right override',
  0x202e: 'right-to-left override',
  0x2060: 'word joiner',
  0x2066: 'left-to-right isolate',
  0x2067: 'right-to-left isolate',
  0x2068: 'first strong isolate',
  0x2069: 'pop directional isolate',
  0x3164: 'hangul filler',
  0xfeff: 'zero width no-break space',
  0xffa0: 'halfwidth hangul filler',
}

/** Code points that draw nothing or change how their neighbours are drawn, beyond the categories. */
const INVISIBLE_POINTS: ReadonlySet<number> = new Set([
  0x034f, // combining grapheme joiner
  0x115f, // hangul choseong filler
  0x1160, // hangul jungseong filler
  0x17b4, // khmer vowel inherent aq
  0x17b5, // khmer vowel inherent aa
  0x2800, // braille pattern blank
])
/** Ranges of the same kind: Mongolian free variation selectors and the variation selectors. */
const INVISIBLE_RANGES: readonly (readonly [number, number])[] = [
  [0x180b, 0x180d],
  [0xfe00, 0xfe0f],
  [0xe0100, 0xe01ef],
]
const ESCAPED = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}\p{Zs}\p{Co}\p{Cs}]/u

function needsEscape(char: string): boolean {
  if (char === ' ') return false
  if (ESCAPED.test(char)) return true
  const cp = char.codePointAt(0) ?? 0
  return INVISIBLE_POINTS.has(cp) || INVISIBLE_RANGES.some(([from, to]) => cp >= from && cp <= to)
}

function codeOf(char: string): string {
  const cp = char.codePointAt(0) ?? 0
  return `U+${cp.toString(16).toUpperCase().padStart(4, '0')}`
}

function describe(char: string): string {
  const cp = char.codePointAt(0) ?? 0
  const known = NAMES[cp]
  if (known) return known
  if (/\p{Cc}/u.test(char)) return 'control character'
  if (/\p{Zs}/u.test(char)) return 'space character'
  if (/\p{Co}/u.test(char)) return 'private use character'
  if (/\p{Cs}/u.test(char)) return 'unpaired surrogate'
  return 'invisible formatting character'
}

/**
 * Splits `value` into runs of ordinary text and hidden characters. A hidden character becomes
 * an escape piece (shown as `U+202E` with a description), so nothing invisible reaches the page.
 */
export function pieces(value: string, limit = Infinity): Piece[] {
  const out: Piece[] = []
  let run = ''
  let count = 0
  for (const char of value) {
    if (count >= limit) break
    count += 1
    if (needsEscape(char)) {
      if (run) out.push({ text: run })
      run = ''
      out.push({ text: codeOf(char), escape: { code: codeOf(char), description: describe(char) } })
    } else run += char
  }
  if (run) out.push({ text: run })
  return out
}

/** A plain-string version for places that cannot hold markup (option text, aria-label, title). */
export function safeText(value: string, limit = 200): string {
  const shown = pieces(value, limit)
    .map((p) => (p.escape ? `<${p.text}>` : p.text))
    .join('')
  return [...value].length > limit ? `${shown}…` : shown
}

/** True when `pieces` would show an escape. */
export function hasHidden(value: string): boolean {
  for (const char of value) if (needsEscape(char)) return true
  return false
}

// --- look-alike detection ------------------------------------------------------------------

const SCRIPTS = [
  'Latin',
  'Cyrillic',
  'Greek',
  'Armenian',
  'Georgian',
  'Hebrew',
  'Arabic',
  'Syriac',
  'Thaana',
  'Devanagari',
  'Bengali',
  'Gurmukhi',
  'Gujarati',
  'Tamil',
  'Telugu',
  'Kannada',
  'Malayalam',
  'Sinhala',
  'Thai',
  'Lao',
  'Tibetan',
  'Myanmar',
  'Khmer',
  'Mongolian',
  'Ethiopic',
  'Cherokee',
  'Han',
  'Hiragana',
  'Katakana',
  'Hangul',
  'Bopomofo',
] as const
const SCRIPT_RES = SCRIPTS.map((s) => [s, new RegExp(`\\p{Script_Extensions=${s}}`, 'u')] as const)

/** Script sets that are normally written together and are not suspicious. */
const COMPATIBLE: ReadonlySet<string>[] = [
  new Set(['Han', 'Hiragana', 'Katakana']),
  new Set(['Han', 'Hangul']),
  new Set(['Han', 'Bopomofo']),
]

/** Letters of other scripts that are drawn like a Latin letter or digit. (A subset of UTS #39.) */
const LOOKALIKE: Record<string, string> = {
  // Cyrillic
  а: 'a',
  в: 'B',
  г: 'r',
  е: 'e',
  з: '3',
  і: 'i',
  ї: 'i',
  ј: 'j',
  к: 'k',
  м: 'M',
  н: 'H',
  о: 'o',
  р: 'p',
  с: 'c',
  т: 'T',
  у: 'y',
  х: 'x',
  ѕ: 's',
  ԁ: 'd',
  ԛ: 'q',
  ԝ: 'w',
  ӏ: 'l',
  һ: 'h',
  ь: 'b',
  А: 'A',
  В: 'B',
  Е: 'E',
  З: '3',
  І: 'I',
  Ј: 'J',
  К: 'K',
  М: 'M',
  Н: 'H',
  О: 'O',
  Р: 'P',
  С: 'C',
  Т: 'T',
  Х: 'X',
  У: 'Y',
  Ѕ: 'S',
  Ԛ: 'Q',
  Ԝ: 'W',
  // Greek
  α: 'a',
  β: 'B',
  γ: 'y',
  ε: 'e',
  ι: 'i',
  κ: 'k',
  ν: 'v',
  ο: 'o',
  ρ: 'p',
  τ: 't',
  υ: 'u',
  χ: 'x',
  ω: 'w',
  Α: 'A',
  Β: 'B',
  Ε: 'E',
  Ζ: 'Z',
  Η: 'H',
  Ι: 'I',
  Κ: 'K',
  Μ: 'M',
  Ν: 'N',
  Ο: 'O',
  Ρ: 'P',
  Τ: 'T',
  Υ: 'Y',
  Χ: 'X',
  // Armenian, Cherokee, others that are often used
  ո: 'n',
  ս: 'u',
  օ: 'o',
  ԑ: 'e',
  Ꭺ: 'A',
  Ᏼ: 'B',
  Ꮯ: 'C',
  Ꭰ: 'D',
  Ꭼ: 'E',
  Ꮋ: 'H',
  Ꮖ: 'I',
  Ꮶ: 'K',
  Ꮇ: 'M',
  Ꮪ: 'S',
  Ꭲ: 'T',
  Ꮩ: 'V',
  Ꮃ: 'W',
  Ꮓ: 'Z',
  // Latin letters that are not ASCII
  ı: 'i',
  ɡ: 'g',
  ǀ: 'l',
  ⅼ: 'l',
  ⅰ: 'i',
  ό: 'o',
}

export interface NameAnalysis {
  /** Scripts of the letters, in order of first appearance. */
  scripts: string[]
  /** Letters from more than one script that are not normally written together. */
  mixedScripts: boolean
  /** The ASCII string this one looks like, when its non-ASCII letters are all look-alikes. */
  lookalikeOf: string | null
  /** The string changes under compatibility normalization (full-width letters, ligatures, ...). */
  compatibility: boolean
  /** Number of hidden characters. */
  hidden: number
}

/** Examines at most the first 300 characters. */
export function analyseName(value: string): NameAnalysis {
  const scripts: string[] = []
  let hidden = 0
  let nonAscii = 0
  let allLookalike = true
  let skeleton = ''
  let n = 0
  for (const char of value) {
    if (n++ >= 300) break
    if (needsEscape(char)) {
      hidden += 1
      continue
    }
    if (/\p{L}/u.test(char)) {
      const script = SCRIPT_RES.find(([, re]) => re.test(char))?.[0]
      if (script && !scripts.includes(script)) scripts.push(script)
    }
    const cp = char.codePointAt(0) ?? 0
    if (cp < 0x80) {
      skeleton += char
      continue
    }
    nonAscii += 1
    const like = LOOKALIKE[char]
    if (like === undefined) allLookalike = false
    else skeleton += like
  }
  const mixedScripts =
    scripts.length > 1 && !COMPATIBLE.some((set) => scripts.every((s) => set.has(s)))
  const head = value.slice(0, 600)
  const compatibility =
    head.normalize('NFKC') !== head && /^[\x20-\x7e]*$/.test(head.normalize('NFKC'))
  return {
    scripts,
    mixedScripts,
    lookalikeOf: nonAscii > 0 && allLookalike ? skeleton : null,
    compatibility,
    hidden,
  }
}

/** Plain sentences for the reasons a name should be looked at twice; empty for an ordinary name. */
export function nameWarnings(value: string): string[] {
  const a = analyseName(value)
  const out: string[] = []
  if (a.hidden > 0)
    out.push(
      `Contains ${a.hidden} hidden character${a.hidden === 1 ? '' : 's'}, shown as U+ codes. It can look like another name.`,
    )
  if (a.mixedScripts)
    out.push(
      `Mixes ${a.scripts.join(' and ')} letters, which can imitate another name${a.lookalikeOf ? ` such as "${a.lookalikeOf}"` : ''}.`,
    )
  else if (a.lookalikeOf)
    out.push(
      `Uses letters that look like Latin letters; it can be mistaken for "${a.lookalikeOf}".`,
    )
  if (a.compatibility)
    out.push('Uses full-width or compatibility characters that look like plain letters.')
  return out
}
