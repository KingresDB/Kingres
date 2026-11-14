// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Test helpers: render the real app (router, pinia, query client) against the mock API.
import { createMemoryHistory, type Router } from 'vue-router'
import { setActivePinia, type Pinia } from 'pinia'
import { VueQueryPlugin, type QueryClient } from '@tanstack/vue-query'
import { render, waitFor, type RenderResult } from '@testing-library/vue'
import type { Component } from 'vue'
import App from '@/App.vue'
import { createKingres } from '@/bootstrap'
import { useSessionStore } from '@/stores/session'

export interface AppHarness extends RenderResult {
  router: Router
  pinia: Pinia
  queryClient: QueryClient
}

/**
 * Builds the real app graph (see bootstrap.ts) with a memory history and test query defaults:
 * no retries, nothing kept between tests.
 */
function build() {
  const kingres = createKingres(createMemoryHistory())
  setActivePinia(kingres.pinia)
  const { queryClient } = kingres
  queryClient.setDefaultOptions({
    ...queryClient.getDefaultOptions(),
    queries: { ...queryClient.getDefaultOptions().queries, retry: false, gcTime: 0, staleTime: 0 },
  })
  return kingres
}

/** Signs in through the mock API and returns once the session store knows the user. */
export async function signInAs(pinia: Pinia, username = 'admin'): Promise<void> {
  await useSessionStore(pinia).signIn(username, 'demo-password')
}

/** Renders the whole app at `path`, signed in as `user` (or signed out with `user: null`). */
export async function renderApp(
  path: string,
  options: { user?: string | null } = {},
): Promise<AppHarness> {
  const { pinia, queryClient, router } = build()
  const user = options.user === undefined ? 'admin' : options.user
  if (user) await signInAs(pinia, user)
  await router.push(path)
  await router.isReady()
  const result = render(App as Component, {
    global: { plugins: [pinia, [VueQueryPlugin, { queryClient }], router] },
  })
  return { ...result, router, pinia, queryClient }
}

/** Renders a single component with the app plugins (no router outlet). */
export async function renderComponent(
  component: Component,
  props: Record<string, unknown> = {},
  options: { user?: string } = {},
): Promise<AppHarness> {
  const { pinia, queryClient, router } = build()
  await signInAs(pinia, options.user ?? 'admin')
  await router.push('/dashboard')
  const result = render(component, {
    props,
    global: { plugins: [pinia, [VueQueryPlugin, { queryClient }], router] },
  })
  return { ...result, router, pinia, queryClient }
}

/** Waits until the loading placeholders are gone. */
export async function settled(): Promise<void> {
  await waitFor(() => {
    if (document.querySelector('.skeleton')) throw new Error('still loading')
  })
}
