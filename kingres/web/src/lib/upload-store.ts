// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// What the console remembers of an unfinished upload, so that a reload or a closed dialog can go on from the server's
// offset: the id, the file's name and size, and the table it was for. It is kept in `sessionStorage` (this tab only, gone
// with it) inside `try/catch`, because storage can be absent, full or blocked, and everything works without it. A file
// itself cannot be kept: the person chooses it again, and one of another size is refused.
export interface StoredUpload {
  uploadId: string
  fileName: string
  sizeBytes: number
  lastModified: number
  database: string
  schema: string
  table: string
  savedAt: number
}

const KEY = 'kingres.uploads'

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
}

export function listStoredUploads(): StoredUpload[] {
  return read()
}

export function rememberUpload(entry: Omit<StoredUpload, 'savedAt'>): void {
  write([...read().filter((e) => e.uploadId !== entry.uploadId), { ...entry, savedAt: Date.now() }])
}

export function forgetUpload(uploadId: string): void {
  write(read().filter((e) => e.uploadId !== uploadId))
}

/** The unfinished upload of this file for this table, if the tab remembers one (same name, size and modification time). */
export function findStoredUpload(
  file: { name: string; size: number; lastModified: number },
  target: { database: string; schema: string; table: string },
): StoredUpload | null {
  return (
    read()
      .filter(
        (e) =>
          e.fileName === file.name &&
          e.sizeBytes === file.size &&
          e.lastModified === file.lastModified &&
          e.database === target.database &&
          e.schema === target.schema &&
          e.table === target.table,
      )
      .at(-1) ?? null
  )
}
