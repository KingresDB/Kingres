// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Helpers of the tests of the data transfer: bytes and files of a size that is a whole number of segments, a task
// followed to its end, a stream read to its end, and a complete upload.
import { endpoints } from '@/api/endpoints'
import type { Task } from '@/api/types'
import { getState } from '@/mocks/http'
import { tickTasks } from '@/mocks/state'

export const SEGMENT = 65536

/** `length` bytes of printable text (the same every time). */
export function bytesOf(length: number): Uint8Array {
  const out = new Uint8Array(length)
  for (let i = 0; i < length; i += 1) out[i] = 97 + (i % 26)
  return out
}

/** A `File` of the text, with the name the mock's fixtures react to. */
export function csvFile(text: string, name: string): File {
  return new File([text], name, { type: 'text/csv' })
}

/** Follows a task to the end of its run (the mock ticks tasks by the clock). */
export async function runTask(task: Task): Promise<Task> {
  for (let i = 0; i < 400; i += 1) {
    tickTasks(getState())
    const current = await endpoints.getTask(task.id)
    if (current.state !== 'running') return current
    await new Promise((resolve) => setTimeout(resolve, 20))
  }
  throw new Error('The task did not end.')
}

/** Reads a stream to its end; a stream that fails rejects. */
export async function readAll(stream: ReadableStream<Uint8Array> | null): Promise<Uint8Array> {
  if (stream === null) return new Uint8Array()
  const parts: Uint8Array[] = []
  const reader = stream.getReader()
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    parts.push(value)
  }
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0))
  let at = 0
  for (const p of parts) {
    out.set(p, at)
    at += p.length
  }
  return out
}

/** Creates an upload for the file and sends it whole, in one chunk; returns the upload id. */
export async function uploadAll(api: typeof endpoints, file: File): Promise<string> {
  const created = await api.createDataUpload({
    purpose: 'tableImport',
    sizeBytes: file.size,
    fileName: file.name,
  })
  await api.uploadDataChunk(created.uploadId, 0, file)
  return created.uploadId
}
