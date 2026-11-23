// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The preview that every dialog of the table lifecycle shows before it starts a change (contract 0.14.0, `previewTableChange`).
// The request of the dialog is read again when it changes, a little after the last keystroke, so that typing a bucket count does
// not ask the server for every digit. The start button stays off while the preview is being read, and while it lists a blocker.
// A server that cannot preview (422 `unsupported_operation`) does not stop the change: the server still refuses a table that
// is not eligible, with the same lines.
import { computed, onBeforeUnmount, ref, watch, type MaybeRefOrGetter, toValue } from 'vue'
import { useTableChangePreview } from '@/api/queries'
import { isUnsupported } from '@/api/problem'
import type { TableChangeRequest } from '@/api/types'
import { previewBlocked } from '@/lib/tables'

/** How long the request must stay as it is before it is sent to the server for a preview. */
export const PREVIEW_DELAY_MS = 250

export function useTableChange(
  db: MaybeRefOrGetter<string>,
  schema: MaybeRefOrGetter<string>,
  table: MaybeRefOrGetter<string>,
  request: MaybeRefOrGetter<TableChangeRequest | null>,
) {
  const settled = ref<TableChangeRequest | null>(toValue(request))
  let timer: ReturnType<typeof setTimeout> | undefined
  watch(
    () => JSON.stringify(toValue(request)),
    () => {
      if (timer !== undefined) clearTimeout(timer)
      // The first complete request (the dialog has just got what it needed to make one) is sent at once.
      if (settled.value === null) {
        settled.value = toValue(request)
        return
      }
      timer = setTimeout(() => {
        settled.value = toValue(request)
      }, PREVIEW_DELAY_MS)
    },
  )
  onBeforeUnmount(() => {
    if (timer !== undefined) clearTimeout(timer)
  })
  const query = useTableChangePreview(db, schema, table, settled)
  /** The request has changed and the preview below is still the one of the request before. */
  const stale = computed(() => JSON.stringify(toValue(request)) !== JSON.stringify(settled.value))
  const unavailable = computed(() => isUnsupported(query.error.value))
  const blocked = computed(() => previewBlocked(query.data.value))
  const loading = computed(
    () => toValue(request) !== null && (stale.value || query.isFetching.value),
  )
  /** True while the dialog must not start the change, and the reason is on the panel. */
  const holdsStart = computed(() => {
    if (toValue(request) === null) return false
    if (loading.value) return true
    if (query.error.value) return !unavailable.value
    return blocked.value
  })
  return {
    preview: computed(() => (stale.value ? undefined : query.data.value)),
    loading,
    error: computed(() => (stale.value ? null : query.error.value)),
    holdsStart,
    unavailable,
    retry: () => void query.refetch(),
  }
}
