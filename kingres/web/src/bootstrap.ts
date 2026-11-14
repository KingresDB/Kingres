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
import { createAppRouter } from './router'
import { useSessionStore } from './stores/session'

export interface Kingres {
  pinia: Pinia
  router: Router
  queryClient: QueryClient
  /** Installs pinia, the query client and the router on the root app. */
  install: (app: App) => void
}

export function createKingres(history?: RouterHistory): Kingres {
  const pinia = createPinia()
  const queryClient = createAppQueryClient(() => {
    // Any request that reports an expired session sends the user back to the sign-in page.
    const session = useSessionStore(pinia)
    if (!session.isSignedIn) return
    session.expire()
    void router.push({
      name: 'login',
      query: { redirect: router.currentRoute.value.fullPath, expired: '1' },
    })
  })
  const router = createAppRouter(queryClient, pinia, history)

  const install = (app: App): void => {
    app.use(pinia).use(VueQueryPlugin, { queryClient }).use(router)
  }
  return { pinia, router, queryClient, install }
}
