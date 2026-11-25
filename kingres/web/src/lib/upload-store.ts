// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// What the console remembers of an unfinished upload, so that a reload or a closed dialog can go on from the server's
// offset: the id, the file's name, size, modification time and a fingerprint of its first and last 64 KiB, the table it was
// for, the person it belongs to, and whether an import of it ended without a known outcome. It is kept in `sessionStorage`
// (this tab only, gone with it) inside `try/catch`, because storage can be absent, full or blocked, and everything works
// without it. A file itself cannot be kept: the person chooses it again, and one that differs is not continued.
import { ref } from 'vue'

export interface StoredUpload {
  uploadId: string
  /** The person who started it: another person's uploads are never listed or continued. */
  user: string
  fileName: string
  sizeBytes: number
  lastModified: number
  /** A hash of the first and last 64 KiB of the file (see `fingerprintFile`); absent when it could not be read. */
  fingerprint?: string
  database: string
  schema: string
  table: string
  savedAt: number
  /** What the upload is for; absent means a CSV for an import. A database archive has empty schema and table. */
  purpose?: 'tableImport' | 'databaseArchive'
  /** `unknown`: an import of this upload ended with `import_outcome_unknown`; `done`: it was imported and kept. */
  imported?: 'unknown' | 'done'
}

const KEY = 'kingres.uploads'

/** Changes with every write, so that a list that shows the uploads of the tab follows the dialogs. */
export const storeVersion = ref(0)

function read(): StoredUpload[] {
  try {
    const raw = globalThis.sessionStorage?.getItem(KEY)
    if (!raw) return []
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed)) return []
    return parsed.filter(
      (e): e is StoredUpload =>
        typeof e === 'object' &&
        e !== null &&
        typeof (e as StoredUpload).uploadId === 'string' &&
        /^\d{1,20}$/.test((e as StoredUpload).uploadId) &&
        typeof (e as StoredUpload).user === 'string' &&
        typeof (e as StoredUpload).fileName === 'string' &&
        Number.isFinite((e as StoredUpload).sizeBytes),
    )
  } catch {
    return []
  }
}

function write(items: StoredUpload[]): void {
  try {
    globalThis.sessionStorage?.setItem(KEY, JSON.stringify(items.slice(-20)))
  } catch {
    // Storage is not available: the upload still works, it just cannot be found again after a reload.
  }
  storeVersion.value += 1
}

/** The uploads this tab remembers for this person. */
export function listStoredUploads(user: string): StoredUpload[] {
  void storeVersion.value
  return read().filter((e) => e.user === user)
}

export function rememberUpload(entry: Omit<StoredUpload, 'savedAt'>): void {
  const before = read().find((e) => e.uploadId === entry.uploadId)
  write([
    ...read().filter((e) => e.uploadId !== entry.uploadId),
    { ...entry, ...(before?.imported ? { imported: before.imported } : {}), savedAt: Date.now() },
  ])
}

export function forgetUpload(uploadId: string): void {
  write(read().filter((e) => e.uploadId !== uploadId))
}

/** Marks what became of an import of the upload (see `StoredUpload.imported`). */
export function markUpload(uploadId: string, imported: 'unknown' | 'done'): void {
  write(read().map((e) => (e.uploadId === uploadId ? { ...e, imported } : e)))
}

/**
 * The unfinished upload of this file for this table and this person, if the tab remembers one: the same name, size and
 * modification time, and (when both sides have one) the same fingerprint of the first and last 64 KiB.
 */
export function findStoredUpload(
  file: { name: string; size: number; lastModified: number },
  target: { database: string; schema: string; table: string },
  user: string,
  fingerprint?: string | null,
): StoredUpload | null {
  return (
    read()
      .filter(
        (e) =>
          e.user === user &&
          e.fileName === file.name &&
          e.sizeBytes === file.size &&
          e.lastModified === file.lastModified &&
          e.database === target.database &&
          e.schema === target.schema &&
          e.table === target.table &&
          (!e.fingerprint || !fingerprint || e.fingerprint === fingerprint),
      )
      .at(-1) ?? null
  )
}

/** The part of a file the fingerprint reads, at each end. */
export const FINGERPRINT_BYTES = 64 * 1024

function fnv(bytes: Uint8Array, seed: number): number {
  let h = seed >>> 0
  for (const b of bytes) {
    h ^= b
    h = Math.imul(h, 0x01000193) >>> 0
  }
  return h
}

/**
 * A hash of the first and the last 64 KiB of a file, cheap to make and read from `Blob.slice`. It is not a security
 * measure (the server computes the SHA-256 of the whole upload); it tells a file that was replaced by another of the same
 * name, size and time from the one the upload was made from. Null when the file cannot be read.
 */
export async function fingerprintFile(file: {
  size: number
  slice(start: number, end: number): Blob
}): Promise<string | null> {
  try {
    const head = new Uint8Array(await file.slice(0, FINGERPRINT_BYTES).arrayBuffer())
    const tail = new Uint8Array(
      await file.slice(Math.max(0, file.size - FINGERPRINT_BYTES), file.size).arrayBuffer(),
    )
    const parts = [
      fnv(head, 0x811c9dc5),
      fnv(tail, 0x811c9dc5),
      fnv(head, 0x9747b28c),
      fnv(tail, 0x9747b28c),
    ]
    return parts.map((n) => n.toString(16).padStart(8, '0')).join('')
  } catch {
    return null
  }
}
