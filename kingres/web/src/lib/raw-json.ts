// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Exact slices of a JSON text. `JSON.parse` followed by `JSON.stringify` drops the original white
// space and changes any integer a 64-bit float cannot hold, and a file
// that `kingres-verify` reads must be saved as the server sent it. This scanner walks a text that
// `JSON.parse` has already accepted and returns the original characters of the values asked for.

export type JsonPath = (string | number)[]

export interface RawSlice {
  path: JsonPath
  raw: string
}

/** Returns the raw text of every value whose path satisfies `want` (object keys and array indexes). */
export function rawSlices(text: string, want: (path: JsonPath) => boolean): RawSlice[] {
  const found: RawSlice[] = []
  const ws = (i: number): number => {
    while (i < text.length && ' \t\n\r'.includes(text[i] as string)) i++
    return i
  }
  const string = (i: number): number => {
    i++ // opening quote
    while (text[i] !== '"') i += text[i] === '\\' ? 2 : 1
    return i + 1
  }
  const value = (start: number, path: JsonPath): number => {
    const c = text[start]
    let end: number
    if (c === '{') {
      let i = ws(start + 1)
      while (text[i] !== '}') {
        const keyEnd = string(i)
        const key = JSON.parse(text.slice(i, keyEnd)) as string
        i = ws(keyEnd)
        i = value(ws(i + 1), [...path, key]) // skips the colon
        i = ws(i)
        if (text[i] === ',') i = ws(i + 1)
      }
      end = i + 1
    } else if (c === '[') {
      let i = ws(start + 1)
      let n = 0
      while (text[i] !== ']') {
        i = ws(value(i, [...path, n++]))
        if (text[i] === ',') i = ws(i + 1)
      }
      end = i + 1
    } else if (c === '"') {
      end = string(start)
    } else {
      end = start
      while (end < text.length && !' \t\n\r,]}'.includes(text[end] as string)) end++
    }
    if (want(path)) found.push({ path, raw: text.slice(start, end) })
    return end
  }
  value(ws(0), [])
  return found
}
