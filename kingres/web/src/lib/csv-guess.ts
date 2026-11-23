// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The preview of a CSV file before it is sent: the first 64 KiB are read in the browser (`File.slice`), and a guess of the
// encoding, the delimiter and the header is made from them for the person to correct. Nothing here is sent anywhere, and
// nothing here decides what the server does with the file: the server's own parser reads the whole file with the options
// the person confirms. The preview is text from a file, so the dialog draws it through `SafeName`.
import type { CsvDelimiter, CsvEncoding } from '@/api/types'

/** How much of the file is read for the preview. */
export const PREVIEW_BYTES = 64 * 1024

const DELIMITERS: CsvDelimiter[] = [',', ';', '\t', '|', ':']

/** The label the browser's `TextDecoder` knows for each encoding of the contract. */
const DECODER_LABEL: Record<CsvEncoding, string> = {
  UTF8: 'utf-8',
  LATIN1: 'iso-8859-1',
  LATIN2: 'iso-8859-2',
  LATIN9: 'iso-8859-15',
  WIN1250: 'windows-1250',
  WIN1251: 'windows-1251',
  WIN1252: 'windows-1252',
  WIN1256: 'windows-1256',
  ISO_8859_6: 'iso-8859-6',
  SJIS: 'shift_jis',
  EUC_JP: 'euc-jp',
  GBK: 'gbk',
  BIG5: 'big5',
  KOI8R: 'koi8-r',
}

export interface CsvGuess {
  /** The file starts with the gzip signature: it cannot be previewed, and the import detects the compression. */
  compressed: boolean
  /** The file starts with a UTF-8 byte order mark. */
  bom: boolean
  encoding: CsvEncoding
  delimiter: CsvDelimiter
  /** The first record looks like column names. */
  header: boolean
  /** True when the file is longer than what was read, so that the preview is a part of it. */
  partial: boolean
  /** The file has no bytes. */
  empty: boolean
}

/** Decodes bytes with an encoding; bytes that the encoding cannot read become U+FFFD (never an exception). */
export function decodeAs(bytes: Uint8Array, encoding: CsvEncoding): string {
  try {
    return new TextDecoder(DECODER_LABEL[encoding]).decode(bytes)
  } catch {
    // A browser without this encoding: show what UTF-8 makes of it, which is still safe text.
    return new TextDecoder('utf-8').decode(bytes)
  }
}

function validUtf8(bytes: Uint8Array): boolean {
  try {
    // `stream` lets a character cut by the end of the preview pass.
    new TextDecoder('utf-8', { fatal: true }).decode(bytes, { stream: true })
    return true
  } catch {
    return false
  }
}

/** Splits text into records and fields with the quote rules of CSV; only for the preview (the server parses the file). */
export function parsePreview(
  text: string,
  delimiter: string,
  quote = '"',
  maxRows = 8,
): string[][] {
  const rows: string[][] = []
  let row: string[] = []
  let field = ''
  let inQuotes = false
  for (let i = 0; i < text.length && rows.length < maxRows; i += 1) {
    const c = text[i] as string
    if (inQuotes) {
      if (c === quote) {
        if (text[i + 1] === quote) {
          field += quote
          i += 1
        } else inQuotes = false
      } else field += c
    } else if (c === quote && field === '') inQuotes = true
    else if (c === delimiter) {
      row.push(field)
      field = ''
    } else if (c === '\n' || c === '\r') {
      if (c === '\r' && text[i + 1] === '\n') i += 1
      row.push(field)
      rows.push(row)
      row = []
      field = ''
    } else field += c
  }
  if (rows.length < maxRows && (field !== '' || row.length > 0)) {
    row.push(field)
    rows.push(row)
  }
  return rows
}

/** The delimiter that splits the first lines into the same number (above one) of fields most often. */
export function guessDelimiter(text: string): CsvDelimiter {
  const lines = text
    .split(/\r?\n/)
    .filter((l) => l !== '')
    .slice(0, 20)
  let best: CsvDelimiter = ','
  let bestScore = 0
  for (const d of DELIMITERS) {
    const counts = lines.map((l) => parsePreview(l, d, '"', 1)[0]?.length ?? 1)
    const common = counts[0] ?? 1
    if (common < 2) continue
    const agree = counts.filter((n) => n === common).length
    const score = agree * 1000 + common
    if (score > bestScore) {
      bestScore = score
      best = d
    }
  }
  return best
}

const NUMERIC = /^[-+]?(\d+([.,]\d*)?|[.,]\d+)([eE][-+]?\d+)?$/

/** The first record is column names when it has no number and a later record has one where it has a word. */
export function guessHeader(rows: string[][]): boolean {
  const first = rows[0]
  if (!first || first.length === 0 || rows.length < 2) return false
  if (first.some((f) => f.trim() === '' || NUMERIC.test(f.trim()))) return false
  if (new Set(first.map((f) => f.trim().toLowerCase())).size !== first.length) return false
  // A later record with a number under a name is the sign of a header.
  const rest = rows.slice(1)
  if (rest.some((r) => r.some((f) => NUMERIC.test(f.trim())))) return true
  // A file of text only proves little: the first record is read as names when each field looks like an identifier and
  // the next record differs from it in every field.
  const identifier = /^[A-Za-z_][A-Za-z0-9_ .-]{0,60}$/
  const second = rest[0] ?? []
  return first.every((f) => identifier.test(f.trim())) && first.every((f, i) => f !== second[i])
}

/** What the first bytes of a file say about it. */
export function guessCsv(bytes: Uint8Array, fileSize: number): CsvGuess {
  const empty = bytes.length === 0
  const compressed = bytes.length >= 2 && bytes[0] === 0x1f && bytes[1] === 0x8b
  const bom = bytes.length >= 3 && bytes[0] === 0xef && bytes[1] === 0xbb && bytes[2] === 0xbf
  const partial = fileSize > bytes.length
  if (empty || compressed)
    return { compressed, bom, encoding: 'UTF8', delimiter: ',', header: true, partial, empty }
  const encoding: CsvEncoding = validUtf8(bytes) ? 'UTF8' : 'WIN1252'
  const text = decodeAs(bytes, encoding)
  const delimiter = guessDelimiter(text)
  // A preview that ends in the middle of a record must not take that record for a whole one.
  const usable = partial ? text.slice(0, Math.max(0, text.lastIndexOf('\n'))) : text
  const rows = parsePreview(usable, delimiter, '"', 8)
  return { compressed, bom, encoding, delimiter, header: guessHeader(rows), partial, empty }
}

/** The first records to show, read with the options the person has now (the preview follows their corrections). */
export function previewRows(
  bytes: Uint8Array,
  encoding: CsvEncoding,
  delimiter: string,
  quote: string,
  partial: boolean,
  maxRows = 6,
): string[][] {
  const text = decodeAs(bytes, encoding)
  const usable = partial ? text.slice(0, Math.max(0, text.lastIndexOf('\n'))) : text
  return parsePreview(usable, delimiter, quote === '' ? '"' : quote, maxRows)
}
