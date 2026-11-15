// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
import { onScopeDispose, ref, type Ref } from 'vue'

/** True while the media query matches. Always false where matchMedia does not exist (tests). */
export function useMediaQuery(query: string): Ref<boolean> {
  const matches = ref(false)
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return matches
  const list = window.matchMedia(query)
  matches.value = list.matches
  const onChange = (event: MediaQueryListEvent) => {
    matches.value = event.matches
  }
  list.addEventListener('change', onChange)
  onScopeDispose(() => list.removeEventListener('change', onChange))
  return matches
}
