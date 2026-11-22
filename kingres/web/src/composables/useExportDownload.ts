// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// One download of an export file: the choice of how it is saved, the run, the progress and the outcome. The run itself
// (resume with `Range`, the checks, the short file) is `runDownload` in lib/downloader.ts. When the session ends in the
// middle, the sink and the bytes in it are kept and `resume()` goes on from them after the person has signed in. When the
// session of the console ends (sign-out, another user) the request and the file are released, so that a late answer cannot
// reach the next person.
import { computed, getCurrentScope, onScopeDispose, ref, shallowRef } from 'vue'
import { endpoints } from '@/api/endpoints'
import { registerServerStateReset } from '@/api/server-state'
import type { DataExport } from '@/api/types'
import {
  chooseSaveMethod,
  MemorySink,
  pickFileSink,
  runDownload,
  type DownloadOutcome,
  type DownloadSink,
  type SaveMethod,
} from '@/lib/downloader'
import { saveBlobFile } from '@/lib/download'
import { exportFileName } from '@/lib/transfer'

export type DownloadState = 'idle' | 'running' | 'done' | 'failed'

export interface DownloadHooks {
  /** Replaceable for tests: how a file in memory reaches the person. */
  save?: (name: string, blob: Blob) => void
  pause?: (attempt: number) => Promise<void>
  maxResumes?: number
  /** Replaceable for tests: where the file is written when the browser has a picker. */
  pickSink?: (name: string) => Promise<DownloadSink | null>
}

export function useExportDownload(getExport: () => DataExport, hooks: DownloadHooks = {}) {
  const state = ref<DownloadState>('idle')
  const received = ref(0)
  const outcome = shallowRef<DownloadOutcome | null>(null)
  const method = computed<SaveMethod>(() => chooseSaveMethod(getExport().sizeBytes ?? 0))
  let controller: AbortController | null = null
  /** What a download that stopped for the password keeps: where the bytes are. */
  const kept = shallowRef<{ sink: DownloadSink; memory: MemorySink | null; name: string } | null>(
    null,
  )

  async function run(
    sink: DownloadSink,
    memory: MemorySink | null,
    name: string,
    startAt: number,
  ): Promise<void> {
    const e = getExport()
    state.value = 'running'
    controller = new AbortController()
    let result: DownloadOutcome
    try {
      result = await runDownload({
        expectedSize: e.sizeBytes as number,
        sha256: e.sha256 ?? null,
        open: (from, signal) => endpoints.downloadDataExport(e.exportId, from, signal),
        sink,
        signal: controller.signal,
        startAt,
        onProgress: (n) => {
          received.value = n
        },
        pause: hooks.pause,
        maxResumes: hooks.maxResumes,
        digest: memory ? () => memory.sha256() : null,
      })
    } catch {
      // `runDownload` does not throw for a failed download; anything that does is a defect, and the dialog must not hang.
      await sink.abort().catch(() => undefined)
      result = { kind: 'failed', reason: 'other', received: received.value, resumes: 0 }
    }
    controller = null
    outcome.value = result
    state.value = result.kind === 'complete' ? 'done' : 'failed'
    kept.value =
      result.kind === 'failed' && result.reason === 'session' ? { sink, memory, name } : null
    // A file kept in memory is handed over only when every byte arrived and the SHA-256 matched.
    if (result.kind === 'complete' && memory) (hooks.save ?? saveBlobFile)(name, memory.blob())
  }

  /** Starts (or restarts) the download. It must be called from the handler of a click: the file picker needs the gesture. */
  async function start(): Promise<void> {
    const e = getExport()
    if (state.value === 'running' || e.sizeBytes === null || e.sizeBytes === undefined) return
    await kept.value?.sink.abort().catch(() => undefined)
    kept.value = null
    const name = exportFileName(e)
    let sink: DownloadSink
    let memory: MemorySink | null = null
    if (method.value === 'file') {
      const picked = await (hooks.pickSink ?? pickFileSink)(name)
      if (picked === null) return
      sink = picked
    } else if (method.value === 'memory') {
      memory = new MemorySink()
      sink = memory
    } else return
    received.value = 0
    outcome.value = null
    await run(sink, memory, name, 0)
  }

  /** True when the download stopped because the session ended and the bytes are still there. */
  const canResume = computed(() => kept.value !== null && state.value === 'failed')

  /** Goes on from the bytes that are kept (after the person signed in again). */
  async function resume(): Promise<void> {
    const k = kept.value
    if (!k || state.value === 'running') return
    kept.value = null
    outcome.value = null
    await run(k.sink, k.memory, k.name, received.value)
  }

  function cancel(): void {
    controller?.abort()
  }

  /** The console's session ended: the request, the bytes and the file go. */
  function release(): void {
    controller?.abort()
    void kept.value?.sink.abort().catch(() => undefined)
    kept.value = null
  }
  const unregister = registerServerStateReset(release)
  if (getCurrentScope())
    onScopeDispose(() => {
      unregister()
      cancel()
      // A dialog that goes away with a download that waits for the password leaves nothing open.
      void kept.value?.sink.abort().catch(() => undefined)
    })

  return { state, received, outcome, method, canResume, start, resume, cancel }
}
