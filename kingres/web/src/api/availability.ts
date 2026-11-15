// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Whether the server this panel talks to can answer at all, as one piece of state for the whole
// application. Two situations make every call fail on purpose and for a short while:
//
// * the instance that hosts the system database was stopped by an administrator
//   (503 `system_database_stopped`): only "start instance" works;
// * this server left the cluster and is resetting itself (503, then `node_uninitialized`).
//
// In both, the panel must not retry every query on its own timers (an error storm) and must not
// sign anybody out. While either is set, the query client is told it is offline: it pauses its
// polling and refetching, and resumes (refetching what it holds) when the state ends.
import { reactive } from 'vue'
import { onlineManager } from '@tanstack/vue-query'
import { registerServerStateReset } from './server-state'

export const availability = reactive({
  /** The system database is stopped on purpose. */
  systemDatabaseStopped: false,
  /**
   * The system database was seen stopped (by this tab or, told through the channel, by another one),
   * also while nobody is signed in: the sign-in page then explains what to do. Cleared when a sign-in
   * or a session check succeeds, and when a tab says it runs again.
   */
  stoppedSeen: false,
  /** This server is leaving the cluster and resetting itself. */
  resetting: false,
  /** The next page the user sees (the setup wizard) says that this server left the cluster. */
  leftNotice: false,
  /**
   * The instance that hosts the system database on this server, learned from the list of instances
   * while the server could still answer; null when it is not known. It is never guessed: the Start
   * button is offered only for an id that was read, and says which instance it starts.
   */
  instanceId: null as number | null,
})

/** How often the stopped-state page looks whether the server answers again (tests shorten it). */
export const recovery = { watchMs: 5_000, afterStartMs: 2_000, slowAfterMs: 120_000 }

/** How the leave-cluster dialog follows the reset of the server (tests shorten it). */
export const leavePolling = { intervalMs: 1_500, giveUpMs: 120_000 }

/** True while the server cannot be asked anything but its own recovery. */
export function isPaused(): boolean {
  return availability.systemDatabaseStopped || availability.resetting
}

function sync(): void {
  onlineManager.setOnline(!isPaused())
}

/** Tells the other tabs of this browser (see lib/cross-tab.ts); set by bootstrap. */
let shareState: ((stopped: boolean) => void) | null = null

export function setSystemStateSharer(share: ((stopped: boolean) => void) | null): void {
  shareState = share
}

/**
 * Sets the state of the whole panel. `fromPeer` is true when another tab said so: it is then not
 * told to the other tabs again.
 */
export function setSystemDatabaseStopped(stopped: boolean, fromPeer = false): void {
  const changed = availability.systemDatabaseStopped !== stopped
  availability.systemDatabaseStopped = stopped
  availability.stoppedSeen = stopped
  sync()
  if (changed && !fromPeer) shareState?.(stopped)
}

/** The system database answered 503 while nobody is signed in (sign-in itself, or a reload). */
export function noteSystemDatabaseStoppedSignedOut(fromPeer = false): void {
  const first = !availability.stoppedSeen
  availability.stoppedSeen = true
  if (first && !fromPeer) shareState?.(true)
}

/** A sign-in or a session check worked: the server can answer, whatever was seen before. */
export function clearSystemDatabaseStoppedSeen(): void {
  availability.stoppedSeen = false
}

export function setResetting(resetting: boolean): void {
  availability.resetting = resetting
  sync()
}

/** The id of the host instance is a plain number, not a secret; it is kept for this tab so that a reload still knows it. */
const INSTANCE_KEY = 'kingres.system-instance'

/** Remembers which instance hosts the system database, so that it can be started when it is down. */
export function rememberInstances(items: { id: number; isMapGroup: boolean }[]): void {
  const host = items.find((i) => i.isMapGroup)
  if (!host) return
  availability.instanceId = host.id
  try {
    sessionStorage.setItem(INSTANCE_KEY, String(host.id))
  } catch {
    // Storage is blocked: the id stays in memory for this page load.
  }
}

/** After a reload: the id remembered by this tab, if any. */
export function recallSystemInstance(): void {
  try {
    const id = Number(sessionStorage.getItem(INSTANCE_KEY))
    if (Number.isSafeInteger(id) && id > 0) availability.instanceId = id
  } catch {
    // Nothing remembered.
  }
}

/** Forgets the instance (the session ended). */
export function forgetSystemInstance(): void {
  availability.instanceId = null
  try {
    sessionStorage.removeItem(INSTANCE_KEY)
  } catch {
    // Nothing to forget.
  }
}

/** Back to normal (the session ended or the test is over). The notice is kept for the wizard. */
export function resetAvailability(): void {
  availability.systemDatabaseStopped = false
  availability.resetting = false
  availability.instanceId = null
  sync()
}

registerServerStateReset(resetAvailability)
