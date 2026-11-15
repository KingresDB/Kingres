// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Exact slices of a JSON text. `JSON.parse` followed by `JSON.stringify` drops the original white
// space and changes any integer a 64-bit float cannot hold, and a file
// that `kingres-verify` reads must be saved as the server sent it. This scanner walks a text that
// `JSON.parse` has already accepted and returns the original characters of the values asked for.
//
// It is still safe on text that is not JSON: every loop is bounded and ends in a `SyntaxError`, and an
// object with the same key twice is refused (`JSON.parse` keeps the last one, a scanner would find
// both, and the file that is saved and the document that is shown could then differ).

export type JsonPath = (string | number)[]

export interface RawSlice {
  path: JsonPath
  raw: string
}

function fail(message: string): never {
  throw new SyntaxError(`Not JSON that can be sliced: ${message}`)
}

/** Returns the raw text of every value whose path satisfies `want` (object keys and array indexes). */
export function rawSlices(text: string, want: (path: JsonPath) => boolean): RawSlice[] {
  const found: RawSlice[] = []
  const ws = (i: number): number => {
    while (i < text.length && ' \t\n\r'.includes(text[i] as string)) i++
    return i
  }
  const string = (start: number): number => {
    if (text[start] !== '"') return fail('a string was expected')
    let i = start + 1 // opening quote
    while (i < text.length && text[i] !== '"') i += text[i] === '\\' ? 2 : 1
    if (i >= text.length) return fail('a string is not closed')
    return i + 1
  }
  const value = (start: number, path: JsonPath): number => {
    const c = text[start]
    let end: number
    if (c === '{') {
      let i = ws(start + 1)
      const keys = new Set<string>()
      while (text[i] !== '}') {
        if (i >= text.length) return fail('an object is not closed')
        const keyEnd = string(i)
        const key = JSON.parse(text.slice(i, keyEnd)) as string
        if (keys.has(key)) return fail('a key appears twice')
        keys.add(key)
        i = ws(keyEnd)
        if (text[i] !== ':') return fail('a colon was expected')
        i = ws(value(ws(i + 1), [...path, key]))
        if (text[i] === ',') i = ws(i + 1)
        else if (text[i] !== '}') return fail('a comma or a closing brace was expected')
      }
      end = i + 1
    } else if (c === '[') {
      let i = ws(start + 1)
      let n = 0
      while (text[i] !== ']') {
        if (i >= text.length) return fail('an array is not closed')
        i = ws(value(i, [...path, n++]))
        if (text[i] === ',') i = ws(i + 1)
        else if (text[i] !== ']') return fail('a comma or a closing bracket was expected')
      }
      end = i + 1
    } else if (c === '"') {
      end = string(start)
    } else {
      if (start >= text.length) return fail('a value was expected')
      end = start
      while (end < text.length && !' \t\n\r,]}'.includes(text[end] as string)) end++
    }
    if (want(path)) found.push({ path, raw: text.slice(start, end) })
    return end
  }
  value(ws(0), [])
  return found
}
