// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The options of an export and of an import as the dialogs hold them, their checks (the same limits as the contract, so that
// the person sees the refusal before the request, and the server still decides), and the request each one makes.
import type { CsvDelimiter, CsvEncoding, ExportTableRequest, ImportTableRequest } from '@/api/types'
import { CLIENT_ONLY_ENCODINGS } from './transfer'

/** The number of bytes of a text in UTF-8 (the contract counts bytes, not characters). */
export function utf8Length(text: string): number {
  return new TextEncoder().encode(text).length
}

/** Why a null string is not valid, or undefined: at most 32 bytes, no quote, delimiter, carriage return or line feed. */
export function nullStringError(
  nullString: string,
  delimiter: string,
  quote = '"',
): string | undefined {
  if (utf8Length(nullString) > 32) return 'The text of a null is at most 32 bytes.'
  if (/[\r\n]/.test(nullString)) return 'The text of a null has no line break.'
  if (quote !== '' && nullString.includes(quote))
    return 'The text of a null has no quote character.'
  if (nullString.includes(delimiter)) return 'The text of a null does not contain the delimiter.'
  return undefined
}

/** Why a quote character is not valid, or undefined: one byte; with a client-only encoding an ASCII byte below 0x40. */
export function quoteError(quote: string, encoding: CsvEncoding): string | undefined {
  if (utf8Length(quote) !== 1) return 'The quote is one byte.'
  if (CLIENT_ONLY_ENCODINGS.includes(encoding) && quote.charCodeAt(0) >= 0x40)
    return 'With this encoding the quote is an ASCII character below @ (such as a double quote or an apostrophe).'
  return undefined
}

export interface ExportForm {
  /** Null: every column in table order. */
  columns: string[] | null
  delimiter: CsvDelimiter
  nullString: string
  encoding: CsvEncoding
  utf8Bom: boolean
  header: boolean
  compression: 'none' | 'gzip'
  spreadsheetSafe: boolean
  retentionHours: number
  acknowledgeOtherRolesCode: boolean
}

export function defaultExportForm(retentionHours: number): ExportForm {
  return {
    columns: null,
    delimiter: ',',
    nullString: '',
    encoding: 'UTF8',
    utf8Bom: false,
    header: true,
    compression: 'none',
    spreadsheetSafe: true,
    retentionHours,
    acknowledgeOtherRolesCode: false,
  }
}

/** The request of an export: every option is sent, so that the audit event records what was chosen, not what a default was. */
export function exportRequest(f: ExportForm): ExportTableRequest {
  return {
    ...(f.columns === null ? {} : { columns: f.columns }),
    delimiter: f.delimiter,
    nullString: f.nullString,
    encoding: f.encoding,
    ...(f.encoding === 'UTF8' ? { utf8Bom: f.utf8Bom } : {}),
    header: f.header,
    compression: f.compression,
    spreadsheetSafe: f.spreadsheetSafe,
    retentionHours: f.retentionHours,
    ...(f.acknowledgeOtherRolesCode ? { acknowledgeOtherRolesCode: true } : {}),
  }
}

export interface ImportForm {
  header: 'none' | 'skip' | 'match'
  /** Null: the non-generated columns in table order. */
  columns: string[] | null
  delimiter: CsvDelimiter
  nullString: string
  quote: string
  encoding: CsvEncoding
  /** Null: detected from the upload. */
  compression: 'none' | 'gzip' | null
  forceNotNull: string[]
  forceNull: string[]
  skipBom: boolean
  keepUpload: boolean
}

export function defaultImportForm(): ImportForm {
  return {
    header: 'skip',
    columns: null,
    delimiter: ',',
    nullString: '',
    quote: '"',
    encoding: 'UTF8',
    compression: null,
    forceNotNull: [],
    forceNull: [],
    skipBom: true,
    keepUpload: false,
  }
}

/** The request of an import for an upload that is complete. */
export function importRequest(uploadId: string, f: ImportForm): ImportTableRequest {
  return {
    uploadId,
    header: f.header,
    ...(f.columns === null ? {} : { columns: f.columns }),
    delimiter: f.delimiter,
    nullString: f.nullString,
    quote: f.quote,
    encoding: f.encoding,
    ...(f.compression === null ? {} : { compression: f.compression }),
    ...(f.forceNotNull.length ? { forceNotNull: f.forceNotNull } : {}),
    ...(f.forceNull.length ? { forceNull: f.forceNull } : {}),
    skipBom: f.skipBom,
    keepUpload: f.keepUpload,
  }
}
