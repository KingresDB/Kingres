// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// One download of an export file: the choice of how it is saved, the run, the progress and the outcome. The run itself
// (resume with `Range`, the checks, the short file) is `runDownload` in lib/downloader.ts.
import { computed, getCurrentScope, onScopeDispose, ref, shallowRef } from 'vue'
import { endpoints } from '@/api/endpoints'
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
}

export function useExportDownload(getExport: () => DataExport, hooks: DownloadHooks = {}) {
  const state = ref<DownloadState>('idle')
  const received = ref(0)
  const outcome = shallowRef<DownloadOutcome | null>(null)
  const method = computed<SaveMethod>(() => chooseSaveMethod(getExport().sizeBytes ?? 0))
  let controller: AbortController | null = null

  /** Starts (or restarts) the download. It must be called from the handler of a click: the file picker needs the gesture. */
  async function start(): Promise<void> {
    const e = getExport()
    if (state.value === 'running' || e.sizeBytes === null || e.sizeBytes === undefined) return
    const size = e.sizeBytes
    const name = exportFileName(e)
    let sink: DownloadSink
    let memory: MemorySink | null = null
    if (method.value === 'file') {
      const picked = await pickFileSink(name)
      if (picked === null) return
      sink = picked
    } else if (method.value === 'memory') {
      memory = new MemorySink()
      sink = memory
    } else return
    received.value = 0
    outcome.value = null
    state.value = 'running'
    controller = new AbortController()
    const result = await runDownload({
      expectedSize: size,
      sha256: e.sha256 ?? null,
      open: (from, signal) => endpoints.downloadDataExport(e.exportId, from, signal),
      sink,
      signal: controller.signal,
      onProgress: (n) => {
        received.value = n
      },
      pause: hooks.pause,
      maxResumes: hooks.maxResumes,
      digest: memory ? () => memory.sha256() : null,
    })
    controller = null
    outcome.value = result
    state.value = result.kind === 'complete' ? 'done' : 'failed'
    // A file kept in memory is handed over only when every byte arrived and the SHA-256 matched.
    if (result.kind === 'complete' && memory) (hooks.save ?? saveBlobFile)(name, memory.blob())
  }

  function cancel(): void {
    controller?.abort()
  }

  if (getCurrentScope()) onScopeDispose(cancel)

  return { state, received, outcome, method, start, cancel }
}
