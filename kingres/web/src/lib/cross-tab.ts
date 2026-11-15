// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Tells the other tabs of this browser that the system database was stopped or runs again, so that
// every tab shows the same state at once instead of each finding out from its own failing request.
//
// What travels is one boolean. Never a session, a CSRF token, a user name or any API data: every
// message is validated on receipt and anything else is ignored. A tab that receives "stopped" only
// switches its own view; it still checks with the server itself (the stopped page asks on its own
// timer) before it believes that the state has ended.
//
// BroadcastChannel is used where it exists. Without it the same message goes through a `storage`
// event: the key is written and removed at once, so nothing stays in the browser's storage.

const CHANNEL = 'kingres-system-database'
export const STORAGE_KEY = 'kingres.system-database-state'
const KIND = 'system-database-state'

export interface SystemStateSharing {
  /** Tells the other tabs. */
  share: (stopped: boolean) => void
  /** Closes the channel and removes the listener. */
  dispose: () => void
}

/** The only shape accepted from another tab; returns the state, or null for anything else. */
export function readSharedState(data: unknown): boolean | null {
  if (typeof data !== 'object' || data === null) return null
  const record = data as Record<string, unknown>
  if (record.kind !== KIND || typeof record.stopped !== 'boolean') return null
  return record.stopped
}

export function createSystemStateSharing(onPeer: (stopped: boolean) => void): SystemStateSharing {
  const message = (stopped: boolean) => ({ kind: KIND, stopped })

  if (typeof BroadcastChannel !== 'undefined') {
    const channel = new BroadcastChannel(CHANNEL)
    channel.onmessage = (event: MessageEvent<unknown>) => {
      const stopped = readSharedState(event.data)
      if (stopped !== null) onPeer(stopped)
    }
    return {
      share: (stopped) => channel.postMessage(message(stopped)),
      dispose: () => channel.close(),
    }
  }

  if (typeof window === 'undefined') return { share: () => undefined, dispose: () => undefined }
  const onStorage = (event: StorageEvent): void => {
    if (event.key !== STORAGE_KEY || event.newValue === null) return
    try {
      const stopped = readSharedState(JSON.parse(event.newValue))
      if (stopped !== null) onPeer(stopped)
    } catch {
      // Not our message.
    }
  }
  window.addEventListener('storage', onStorage)
  return {
    share: (stopped) => {
      try {
        // The event reaches the other tabs when the value is set; removing it leaves nothing behind.
        localStorage.setItem(STORAGE_KEY, JSON.stringify({ ...message(stopped), at: Date.now() }))
        localStorage.removeItem(STORAGE_KEY)
      } catch {
        // Storage is blocked: the other tabs find out from their own requests.
      }
    },
    dispose: () => window.removeEventListener('storage', onStorage),
  }
}
