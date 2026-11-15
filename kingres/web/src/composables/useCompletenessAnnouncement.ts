// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Tells a screen reader, once, that a cluster-wide list became incomplete (or complete again). It
// speaks when the set of missing servers changes, not on every poll that finds the same servers
// missing, and it says nothing for a list that was complete from the start.
import { watch, toValue, type MaybeRefOrGetter } from 'vue'
import { useAnnounce } from './useAnnounce'
import { completenessAnnouncement, completenessSignature, type Completeness } from '@/lib/partial'

export function useCompletenessAnnouncement(
  completeness: MaybeRefOrGetter<Completeness>,
  what: string,
  enabled: MaybeRefOrGetter<boolean> = true,
): void {
  const announce = useAnnounce()
  let last = ''
  watch(
    () => ({ c: toValue(completeness), on: toValue(enabled) }),
    ({ c, on }) => {
      if (!on) return
      const signature = completenessSignature(c)
      if (signature === last) return
      const before = last
      last = signature
      if (signature === '' && before === '') return
      announce(completenessAnnouncement(c, what))
    },
    { immediate: true },
  )
}
