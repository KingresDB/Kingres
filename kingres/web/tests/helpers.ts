// SPDX-License-Identifier: AGPL-3.0-only
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
import { getState } from '@/mocks/http'

export interface AppHarness extends RenderResult {
  router: Router
  pinia: Pinia
  queryClient: QueryClient
}

/**
 * Builds the real app graph (see bootstrap.ts) with a memory history and test query defaults:
 * no retries, nothing kept between tests.
 */
const built: { dispose: () => void }[] = []

/** Removes the global hooks of every app built by the test that just ended. */
export function disposeBuiltApps(): void {
  for (const app of built.splice(0)) app.dispose()
}

function build() {
  const kingres = createKingres(createMemoryHistory())
  built.push(kingres)
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

/**
 * Puts every clock of the mock in line with the answering server's. The seed has one server whose clock is 1.82 s off
 * (contract 0.8.0), which raises the alert `clockDifference`; tests of alerts that count or list alerts do not want it.
 */
export function calmClocks(): void {
  for (const clock of Object.values(getState().config.clocks)) clock.offsetMs = 0
}
