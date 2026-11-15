// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Announcements for screen readers. Most screen readers do not read text that is present at the
// moment its live region is inserted, so the regions are rendered once (by the shell and by every
// dialog, because a dialog makes the page behind it inert) and the text is changed afterwards.
import { inject, provide, ref, type InjectionKey } from 'vue'

export type Announce = (message: string, level?: 'polite' | 'assertive') => void

const announceKey: InjectionKey<Announce> = Symbol('announce')

/** Creates the text of the two regions and the function that changes it. Render both regions. */
export function createAnnouncer() {
  const polite = ref('')
  const assertive = ref('')
  const announce: Announce = (message, level = 'polite') => {
    const target = level === 'assertive' ? assertive : polite
    // Clearing first makes a repeated message be read again.
    target.value = ''
    setTimeout(() => {
      target.value = message
    }, 60)
  }
  provide(announceKey, announce)
  return { polite, assertive, announce }
}

/** The announcer of the nearest dialog or shell; a no-op outside both. */
export function useAnnounce(): Announce {
  return inject(announceKey, () => undefined)
}
