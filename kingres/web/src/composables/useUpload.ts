// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// One upload of a file for the dialog: the uploader of lib/uploader.ts with its state in refs, the tab's memory of the
// upload (so a reload or a closed dialog can go on), and the end of it when the dialog goes away.
import { computed, getCurrentScope, onScopeDispose, shallowRef } from 'vue'
import { endpoints } from '@/api/endpoints'
import { Uploader, type UploadSnapshot, type UploadSource } from '@/lib/uploader'
import { forgetUpload, rememberUpload } from '@/lib/upload-store'
import { useSessionStore } from '@/stores/session'

export interface UploadTarget {
  database: string
  schema: string
  table: string
}

const idle = (size: number): UploadSnapshot => ({
  phase: 'idle',
  offset: 0,
  size,
  uploadId: null,
  failure: null,
  problem: null,
  sha256: null,
  resumes: 0,
  continued: false,
  deletePending: false,
})

export function useUpload(target: UploadTarget) {
  const session = useSessionStore()
  const snapshot = shallowRef<UploadSnapshot>(idle(0))
  let uploader: Uploader | null = null
  let source: UploadSource | null = null

  const percent = computed(() =>
    snapshot.value.size > 0 ? Math.floor((snapshot.value.offset / snapshot.value.size) * 100) : 0,
  )
  const running = computed(
    () => snapshot.value.phase === 'sending' || snapshot.value.phase === 'starting',
  )

  /** Starts sending `file`, or continues the upload with the id the tab remembered. */
  async function start(
    file: UploadSource,
    options: {
      existing?: string
      compressed?: boolean
      purpose?: 'tableImport' | 'databaseArchive'
      maxFileBytes?: number | null
      maxAtomicBytes?: number
      fingerprint?: string | null
    } = {},
  ): Promise<void> {
    uploader?.detach()
    source = file
    snapshot.value = idle(file.size)
    uploader = new Uploader({
      file,
      api: {
        createDataUpload: endpoints.createDataUpload,
        getDataUpload: endpoints.getDataUpload,
        uploadDataChunk: endpoints.uploadDataChunk,
        deleteDataUpload: endpoints.deleteDataUpload,
        refreshSession: endpoints.refreshSession,
      },
      compressed: options.compressed,
      ...(options.purpose ? { purpose: options.purpose } : {}),
      maxFileBytes: options.maxFileBytes,
      maxAtomicBytes: options.maxAtomicBytes,
      onChange: (s) => {
        snapshot.value = s
      },
      onUpload: (id) => {
        if (id === null) {
          if (snapshot.value.uploadId) forgetUpload(snapshot.value.uploadId)
          return
        }
        if (source)
          rememberUpload({
            uploadId: id,
            fileName: source.name,
            ...(options.purpose ? { purpose: options.purpose } : {}),
            sizeBytes: source.size,
            lastModified: source.lastModified,
            user: session.username ?? '',
            ...(options.fingerprint ? { fingerprint: options.fingerprint } : {}),
            ...target,
          })
      },
    })
    await uploader.start(options.existing)
  }

  const pause = (): void => uploader?.pause()
  const resume = async (): Promise<void> => uploader?.resume()
  const continueAfterSignIn = async (): Promise<void> => uploader?.continueAfterSignIn()
  const cancel = async (): Promise<void> => uploader?.cancel()

  // The dialog goes away: the request in flight is released and the upload stays on the server and in the tab's memory.
  if (getCurrentScope()) onScopeDispose(() => uploader?.detach())

  return { snapshot, percent, running, start, pause, resume, continueAfterSignIn, cancel }
}
