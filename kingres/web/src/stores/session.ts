// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Client-only state shared between pages: who is signed in. (Server data lives in the query cache.)
import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { setCsrfToken } from '@/api/client'
import { canCall, highestRole, type OperationId } from '@/api/roles'
import type { Session } from '@/api/types'
import { ApiError } from '@/api/problem'
import { resetServerState } from '@/api/server-state'
import {
  availability,
  clearSystemDatabaseStoppedSeen,
  forgetSystemInstance,
  rememberInstances,
  setSystemDatabaseStopped,
} from '@/api/availability'

/**
 * The server accepted the sign-in but the next request was not signed in: the browser did not keep
 * the session cookie. The cookie is `Secure`, which browsers accept only over HTTPS or on localhost.
 */
export class SessionCookieError extends Error {
  constructor() {
    super('The browser did not keep the session cookie')
    this.name = 'SessionCookieError'
  }
}

export const useSessionStore = defineStore('session', () => {
  const session = ref<Session | null>(null)
  /** True once the initial session check has finished (successfully or not). */
  const restored = ref(false)

  const username = computed(() => session.value?.username ?? null)
  const clusterRoles = computed(() => session.value?.clusterRoles ?? [])
  const role = computed(() => highestRole(clusterRoles.value))
  const isSignedIn = computed(() => session.value !== null)

  /** Counts session changes, so that a slow restore cannot overwrite a newer sign-in. */
  let epoch = 0
  /** True while sign-in checks that the browser kept the cookie; a 401 then is not an expiry. */
  const probing = ref(false)
  /** Who was signed in when the session expired; the next sign-in as someone else ignores the old page. */
  const expiredUser = ref<string | null>(null)
  /** Set when another tab signed in as a different user; the shell tells the person. */
  const identityNotice = ref<string | null>(null)

  /**
   * The one place where the session changes. When the identity changes (signed out, expired, or a
   * different user) everything the client holds from the server is dropped first, so data of one
   * user can never be shown to the next.
   */
  function adopt(next: Session | null): void {
    epoch += 1
    if ((session.value?.username ?? null) !== (next?.username ?? null)) resetServerState()
    session.value = next
    setCsrfToken(next?.csrfToken ?? null)
    if (next === null) forgetSystemInstance()
  }

  /**
   * Takes what `getSession` answered. From 0.6.0 the server answers it while the system database is
   * stopped, marked `degraded` (a session it checked before the stop): the panel then shows the stopped
   * state and asks for the instance to start. Nothing of the session is kept in browser storage.
   */
  function takeAnswer(current: Session): void {
    adopt(current)
    if (current.degraded === true) {
      setSystemDatabaseStopped(true)
      void learnInstance()
    } else {
      clearSystemDatabaseStoppedSeen()
    }
  }

  /**
   * Reads which instance hosts the system database. `listThisServerInstances` also works while the
   * system database is stopped (it answers from the memory of the instance manager). If it cannot
   * answer (503, nothing read before), the id stays unknown and the page says what to do instead.
   */
  async function learnInstance(): Promise<void> {
    const started = epoch
    try {
      const list = await endpoints.listThisServerInstances()
      if (epoch === started) rememberInstances(list.items)
    } catch {
      // Unknown: the stopped page offers no Start button and explains why.
    }
  }

  /** Asks the server whether a valid session cookie exists. */
  async function restore(): Promise<void> {
    const started = epoch
    try {
      const current = await endpoints.getSession()
      if (epoch === started) takeAnswer(current)
    } catch (error) {
      if (epoch === started) adopt(null)
      // Anything but "not signed in" is worth knowing about, but must not block the sign-in page.
      if (!(error instanceof ApiError)) console.error(error)
    } finally {
      restored.value = true
    }
  }

  async function signIn(username: string, password: string): Promise<void> {
    adopt(await endpoints.login({ username, password }))
    clearSystemDatabaseStoppedSeen()
    // Prove that the cookie came back: a Secure cookie set over plain HTTP (not localhost) is dropped.
    probing.value = true
    try {
      await endpoints.getSession()
    } catch (error) {
      if (error instanceof ApiError && error.code === 'unauthenticated') {
        adopt(null)
        throw new SessionCookieError()
      }
    } finally {
      probing.value = false
    }
    restored.value = true
  }

  async function signOut(): Promise<void> {
    try {
      await endpoints.logout()
    } finally {
      adopt(null)
    }
  }

  /** Ends the session without a sign-out call (the server reset itself: there is nothing to sign out of). */
  function end(): void {
    adopt(null)
  }

  /** Asks the server for the session again and takes it (after the system database runs again). */
  async function refresh(): Promise<void> {
    try {
      const current = await endpoints.getSession()
      if (current.degraded === true) {
        // Still stopped: keep the stopped state, ask for the instance again if it is not known.
        adopt(current)
        setSystemDatabaseStopped(true)
        if (availability.instanceId === null) void learnInstance()
      } else adopt(current)
    } catch {
      // A lost session is handled centrally.
    }
  }

  /** Called when any request reports that the session is gone. */
  function expire(): void {
    expiredUser.value = session.value?.username ?? expiredUser.value
    adopt(null)
  }

  /**
   * Asks the server who the cookie belongs to (called when the tab regains focus). Another tab can
   * sign in as someone else and replace the shared cookie: then this tab must not keep showing the
   * old user's name, role and data. Returns true when the identity changed.
   */
  async function verify(): Promise<boolean> {
    const before = session.value
    if (!before) return false
    const started = epoch
    let current: Session
    try {
      current = await endpoints.getSession()
    } catch {
      // Not signed in any more: the client reports that centrally and ends the session.
      return false
    }
    if (epoch !== started || !session.value) return false
    if (current.degraded === true) {
      // The system database is stopped (another tab started it, or this one saw it first).
      adopt(current)
      setSystemDatabaseStopped(true)
      if (availability.instanceId === null) void learnInstance()
      return false
    }
    if (current.username !== before.username) {
      adopt(current)
      identityNotice.value = current.username
      return true
    }
    if (
      current.csrfToken !== before.csrfToken ||
      current.clusterRoles.join() !== before.clusterRoles.join()
    ) {
      adopt(current)
    }
    return false
  }

  /** True when the signed-in account may call the operation (role from the contract). */
  function can(operation: OperationId): boolean {
    return canCall(clusterRoles.value, operation)
  }

  return {
    session,
    restored,
    username,
    clusterRoles,
    role,
    isSignedIn,
    restore,
    refresh,
    learnInstance,
    verify,
    probing,
    expiredUser,
    identityNotice,
    signIn,
    signOut,
    end,
    expire,
    can,
  }
})
