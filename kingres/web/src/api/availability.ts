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
  /** This server is leaving the cluster and resetting itself. */
  resetting: false,
  /** The next page the user sees (the setup wizard) says that this server left the cluster. */
  leftNotice: false,
  /** The instance to start after the system database was stopped (remembered from the list). */
  instanceId: 1,
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

export function setSystemDatabaseStopped(stopped: boolean): void {
  availability.systemDatabaseStopped = stopped
  sync()
}

export function setResetting(resetting: boolean): void {
  availability.resetting = resetting
  sync()
}

/** Remembers which instance hosts the system database, so that it can be started when it is down. */
export function rememberInstances(items: { id: number; isMapGroup: boolean }[]): void {
  const host = items.find((i) => i.isMapGroup)
  if (host) availability.instanceId = host.id
}

/** Back to normal (the session ended or the test is over). The notice is kept for the wizard. */
export function resetAvailability(): void {
  availability.systemDatabaseStopped = false
  availability.resetting = false
  sync()
}

registerServerStateReset(resetAvailability)
