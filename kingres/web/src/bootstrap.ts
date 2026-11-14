// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Builds the application object graph. main.ts mounts it; tests build the same graph with a
// memory history so that they exercise the real wiring (session expiry included).
import { createPinia, type Pinia } from 'pinia'
import { VueQueryPlugin, type QueryClient } from '@tanstack/vue-query'
import type { App } from 'vue'
import type { Router, RouterHistory } from 'vue-router'
import { createAppQueryClient } from './api/queries'
import { registerServerStateReset, setSessionLostHandler } from './api/server-state'
import { createAppRouter } from './router'
import { useSessionStore } from './stores/session'

export interface Kingres {
  pinia: Pinia
  router: Router
  queryClient: QueryClient
  /** Removes the global hooks of this instance (tests build many; the real page has one). */
  dispose: () => void
  /** Installs pinia, the query client and the router on the root app. */
  install: (app: App) => void
}

export function createKingres(history?: RouterHistory): Kingres {
  const pinia = createPinia()
  /**
   * Any request, query, mutation or task poll that reports an expired session ends the session
   * (which drops all server data, see session.adopt) and sends the user to the sign-in page.
   */
  const onSessionLost = (): void => {
    const session = useSessionStore(pinia)
    if (!session.isSignedIn || session.probing) return
    const redirect = router.currentRoute.value.fullPath
    session.expire()
    void router.push({ name: 'login', query: { redirect, expired: '1' } })
  }
  const queryClient = createAppQueryClient(onSessionLost)
  const router = createAppRouter(queryClient, pinia, history)
  setSessionLostHandler(onSessionLost)

  // Everything TanStack Query holds is server data: cancel it and drop it when a session ends.
  const unregisterReset = registerServerStateReset(() => {
    void queryClient.cancelQueries()
    queryClient.clear()
    queryClient.getMutationCache().clear()
  })

  /** Another tab may have signed in as someone else (the cookie is shared): look when we regain focus. */
  const recheck = (): void => {
    if (typeof document !== 'undefined' && document.visibilityState === 'hidden') return
    void useSessionStore(pinia)
      .verify()
      .then((changed) => {
        if (changed) void router.push({ name: 'dashboard' })
      })
  }
  if (typeof window !== 'undefined') {
    window.addEventListener('focus', recheck)
    document.addEventListener('visibilitychange', recheck)
  }
  const dispose = (): void => {
    unregisterReset()
    setSessionLostHandler(null)
    if (typeof window !== 'undefined') {
      window.removeEventListener('focus', recheck)
      document.removeEventListener('visibilitychange', recheck)
    }
    queryClient.clear()
  }

  const install = (app: App): void => {
    app.use(pinia).use(VueQueryPlugin, { queryClient }).use(router)
  }
  return { pinia, router, queryClient, dispose, install }
}
