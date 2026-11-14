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

  function adopt(next: Session | null): void {
    epoch += 1
    session.value = next
    setCsrfToken(next?.csrfToken ?? null)
  }

  /** Asks the server whether a valid session cookie exists. */
  async function restore(): Promise<void> {
    const started = epoch
    try {
      const current = await endpoints.getSession()
      if (epoch === started) adopt(current)
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
    // Prove that the cookie came back: a Secure cookie set over plain HTTP (not localhost) is dropped.
    try {
      await endpoints.getSession()
    } catch (error) {
      if (error instanceof ApiError && error.code === 'unauthenticated') {
        adopt(null)
        throw new SessionCookieError()
      }
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

  /** Called when any request reports that the session is gone. */
  function expire(): void {
    adopt(null)
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
    signIn,
    signOut,
    expire,
    can,
  }
})
