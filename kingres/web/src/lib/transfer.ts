// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The numbers and words of the data transfer (contract 0.19.0) that the console and its tests share: the size of a
// chunk, the limit of one import, the labels of the encodings and of the consistency levels.
import type { CsvEncoding, DataExport, TableConsistency } from '@/api/types'

export const KIB = 1024
export const MIB = 1024 * KIB
export const GIB = 1024 * MIB

/** The console sends 64 MiB chunks (a multiple of the 64 KiB segment of the server), one at a time. */
export const CHUNK_BYTES = 64 * MIB
/** Every chunk but the last is a multiple of this (`DataUpload.chunkAlign`). */
export const CHUNK_ALIGN = 64 * KIB
/** An import is all or nothing and takes at most 2 GiB of decompressed data in this version. */
export const MAX_ATOMIC_BYTES = 2 * GIB
/** The most data of one import into a reference table (written on every group, so the server's cap is smaller). */
export const MAX_ATOMIC_REFERENCE_BYTES = 256 * MIB

/** The most data one import takes for a table with these classes (contract text of `importTable`). */
export function atomicCapFor(classes: readonly string[], fromServer?: number | null): number {
  // The server says its own cap (`dataTransfer.maxAtomicBytes`); a server that does not has the default of the contract text.
  const cap =
    fromServer !== undefined && fromServer !== null && fromServer > 0
      ? fromServer
      : MAX_ATOMIC_BYTES
  // A reference table is written on every group: 256 MiB whatever the flag says.
  return classes.includes('reference') ? Math.min(cap, MAX_ATOMIC_REFERENCE_BYTES) : cap
}

/** The most a download keeps in memory when the browser cannot write to a file as it goes. */
export const MEMORY_DOWNLOAD_BYTES = 256 * MIB

/** A size in gibibytes for a settings field, to three decimals (1 MiB is 0.001). */
export function toGib(bytes: number): number {
  return Number((bytes / GIB).toFixed(3))
}

/** The bytes of a size in gibibytes. */
export function fromGib(gib: number): number {
  return Math.round(gib * GIB)
}

/**
 * The encodings the dialogs offer, by the name PostgreSQL gives them. `WIN1256` and `ISO_8859_6` are Arabic
 * (and Persian) encodings and are offered under their common names too.
 */
export const ENCODING_LABEL: Record<CsvEncoding, string> = {
  UTF8: 'UTF-8 (recommended)',
  LATIN1: 'Latin 1 (ISO 8859-1)',
  LATIN2: 'Latin 2 (ISO 8859-2)',
  LATIN9: 'Latin 9 (ISO 8859-15)',
  WIN1250: 'Windows-1250 (Central European)',
  WIN1251: 'Windows-1251 (Cyrillic)',
  WIN1252: 'Windows-1252 (Western European)',
  WIN1256: 'Windows-1256 (Arabic, Persian)',
  ISO_8859_6: 'ISO 8859-6 (Arabic)',
  SJIS: 'Shift JIS (Japanese)',
  EUC_JP: 'EUC-JP (Japanese)',
  GBK: 'GBK (Chinese)',
  BIG5: 'Big5 (Chinese)',
  KOI8R: 'KOI8-R (Russian)',
}

/** The encodings that exist on the client side only: a byte-oriented scanner cannot tell a trail byte from a quote. */
export const CLIENT_ONLY_ENCODINGS: readonly CsvEncoding[] = ['SJIS', 'BIG5', 'GBK']

export const DELIMITER_LABEL: Record<string, string> = {
  ',': 'Comma (,)',
  ';': 'Semicolon (;)',
  '\t': 'Tab',
  '|': 'Bar (|)',
  ':': 'Colon (:)',
}

/** What a consistency level means for a person who reads the file, in one sentence. */
export function consistencySentence(c: TableConsistency): string {
  if (c.level === 'single-snapshot') return 'The file is one snapshot of the table.'
  if (c.level === 'archive-single-snapshot')
    return 'This table was read in the one transaction that read every table of the archive.'
  if (c.level === 'per-group')
    return `The table is spread over ${c.groups} groups. Each group was read at its own moment, so a transaction that touched two groups can appear in one and not in the other.`
  return `The table is spread over ${c.groups} groups. Each group was read at its own moment, and the partitions of one group may be different snapshots.`
}

/** The sentence the export dialog shows before the person confirms, from what the console knows of the table. */
export function consistencyBefore(
  kind: 'local' | 'reference' | 'distributed',
  groups: number,
): string {
  if (kind === 'reference')
    return 'This table is read as one snapshot (a reference table is read from one group).'
  if (kind === 'local') return 'This table is read as one snapshot.'
  return `This table is distributed. If its buckets lie in more than one of the ${groups} shard groups, each group is read at its own moment, so a transaction that touched two groups can appear in one and not in the other. If they lie in one group, it is read as one snapshot. The result of the export says which.`
}

/** The label of the file format of an export. */
export function formatLabel(format: DataExport['format']): string {
  if (format === 'archive/1') return 'Database archive (zip)'
  return format === 'csv.gz' ? 'CSV, compressed (gzip)' : 'CSV'
}

/** The file name the console offers for an export, built from identifiers only (the server builds the real one). */
export function exportFileName(
  e: Pick<DataExport, 'database' | 'table' | 'format' | 'exportId'>,
): string {
  if (e.format === 'archive/1') {
    const db = (e.database ?? '').replace(/[^A-Za-z0-9._-]/g, '_').slice(0, 40)
    return `${db || `export-${e.exportId}`}.kdx.zip`
  }
  const safe = (v: string | null | undefined): string =>
    (v ?? '').replace(/[^A-Za-z0-9._-]/g, '_').slice(0, 40)
  const base = [safe(e.database), safe(e.table)].filter(Boolean).join('-') || `export-${e.exportId}`
  return `${base}.${e.format === 'csv.gz' ? 'csv.gz' : 'csv'}`
}

/** What a failed download says, in words that are true about how many bytes arrived. Nothing was saved. */
export function describeDownloadFailure(
  reason: string,
  received: number,
  expected: number,
): string {
  const of = `${received} of ${expected} bytes arrived`
  switch (reason) {
    case 'gone':
      return 'The file has expired and the server deleted it. Export the table again.'
    case 'not_found':
      return 'The export no longer exists. It was deleted, or it belongs to another role.'
    case 'range':
      return `The server could not continue the download from the byte where it stopped (${of}). The file was not saved.`
    case 'changed':
      return `The file is not the one the download started with: its size, tag or SHA-256 differs from the export (${of}). The file was not saved.`
    case 'short':
      return `The server ended the download before the end of the file (${of}). The file was not saved. Try again.`
    case 'network':
      return `The connection broke and could not be resumed (${of}). The file was not saved. Try again.`
    case 'session':
      return `Your session ended (${of}). Enter your password to go on from the bytes that are kept.`
    case 'save':
      return `The file could not be written to the place you chose, for example because the disk is full (${of}). Nothing was saved. Free some room or choose another place, and try again.`
    case 'refused':
      return 'The server refused the download: data transfer is turned off, your role is too low, or the connection is not encrypted.'
    case 'cancelled':
      return 'The download was cancelled. Nothing was saved.'
    default:
      return `The download failed (${of}). The file was not saved.`
  }
}
