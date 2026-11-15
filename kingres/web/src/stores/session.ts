// SPDX-License-Identifier: PostgreSQL
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
  clearSystemDatabaseStoppedSeen,
  forgetSystemInstance,
  recallSystemInstance,
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

/**
 * The session of this tab (what `login` and `getSession` returned, with the CSRF token) is kept in
 * `sessionStorage`, which belongs to this tab and ends with it. Why: while the system database is stopped,
 * `getSession` answers 503, but the contract keeps `startThisServerInstance` working for a session that
 * was valid before the stop. After a reload the panel has no other way to know the CSRF token that the
 * start needs. The copy is used ONLY when `getSession` answers 503 `system_database_stopped`; in every
 * other case the server decides who is signed in. It is removed when the session ends.
 */
const SAVED_KEY = 'kingres.session'

function saveSession(session: Session | null): void {
  try {
    if (session) sessionStorage.setItem(SAVED_KEY, JSON.stringify(session))
    else sessionStorage.removeItem(SAVED_KEY)
  } catch {
    // Storage is blocked: a reload while the system database is stopped then ends at the sign-in page.
  }
}

/** The saved session, if it has the shape of one and has not expired. */
export function readSavedSession(now: number = Date.now()): Session | null {
  try {
    const text = sessionStorage.getItem(SAVED_KEY)
    if (!text) return null
    const v = JSON.parse(text) as Partial<Session> | null
    if (
      typeof v !== 'object' ||
      v === null ||
      typeof v.username !== 'string' ||
      !Array.isArray(v.clusterRoles) ||
      !v.clusterRoles.every((r) => typeof r === 'string') ||
      typeof v.expiresAt !== 'string' ||
      !(Date.parse(v.expiresAt) > now) ||
      !(v.csrfToken === null || typeof v.csrfToken === 'string' || v.csrfToken === undefined)
    )
      return null
    return v as Session
  } catch {
    return null
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
    saveSession(next)
    if (next === null) forgetSystemInstance()
  }

  /** Asks the server whether a valid session cookie exists. */
  async function restore(): Promise<void> {
    const started = epoch
    try {
      const current = await endpoints.getSession()
      clearSystemDatabaseStoppedSeen()
      if (epoch === started) adopt(current)
    } catch (error) {
      // The system database is stopped on purpose: the server cannot say who is signed in, but it still
      // accepts this session for "start instance". Keep the session of this tab (if it has one and it has
      // not expired) and show the stopped state, instead of sending the administrator to a sign-in page
      // where signing in is impossible.
      if (
        error instanceof ApiError &&
        error.code === 'system_database_stopped' &&
        epoch === started
      ) {
        const saved = readSavedSession()
        if (saved) {
          adopt(saved)
          recallSystemInstance()
          setSystemDatabaseStopped(true)
          return
        }
      }
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
      adopt(await endpoints.getSession())
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
