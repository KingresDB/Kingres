// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-02 and W-12: every way a session can end clears all server state held by the client, and
// the next user to sign in on the same tab starts from nothing.
import { describe, expect, it } from 'vitest'
import { effectScope } from 'vue'
import type { QueryClient } from '@tanstack/vue-query'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse, delay } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { useSessionStore } from '@/stores/session'
import { useTask } from '@/composables/useTask'
import { renderApp } from './helpers'

const expired = () =>
  HttpResponse.json(
    { type: 'about:blank', title: 'x', status: 401, code: 'session_expired' },
    { status: 401 },
  )

/** Queries that hold data (a cleared client has none). */
// (Whether the server is set up is public; the router asks again on every navigation.)
const withData = (queryClient: QueryClient) =>
  queryClient
    .getQueryCache()
    .getAll()
    .filter((q) => q.state.data !== undefined && q.queryKey[0] !== 'nodeState')

async function loadSecurity() {
  const app = await renderApp('/security')
  await screen.findByRole('rowheader', { name: 'admin' })
  expect(withData(app.queryClient).length).toBeGreaterThan(0)
  return app
}

describe('every way a session can end clears the server state', () => {
  it('sign-out', async () => {
    const user = userEvent.setup()
    const { queryClient, router } = await loadSecurity()
    await user.click(screen.getByRole('button', { name: /sign out/i }))
    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'))
    expect(withData(queryClient)).toHaveLength(0)
  })

  it('expiry reported by a query', async () => {
    const { queryClient, router } = await loadSecurity()
    getState().session!.expiresAt = new Date(Date.now() - 1000).toISOString()
    await queryClient.invalidateQueries({ queryKey: keys.roles })
    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'))
    expect(withData(queryClient)).toHaveLength(0)
    expect(router.currentRoute.value.query.expired).toBe('1')
  })

  it('a 401 on a mutation sends the user to sign-in and clears the cache (W-12)', async () => {
    const user = userEvent.setup()
    const { queryClient, router } = await loadSecurity()
    mockServer.use(http.delete('*/api/v1/roles/:name', expired))
    const row = screen.getByRole('rowheader', { name: 'app_ro' }).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: /drop/i }))
    const dialog = await screen.findByRole('dialog', { name: /drop role app_ro/i })
    await user.type(within(dialog).getByLabelText(/to confirm/i), 'app_ro')
    await user.click(within(dialog).getByRole('button', { name: /^drop role$/i }))
    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'), { timeout: 3000 })
    expect(withData(queryClient)).toHaveLength(0)
  })

  it('a 401 on a task poll sends the user to sign-in (W-12)', async () => {
    const user = userEvent.setup()
    const { queryClient, router } = await renderApp('/databases/reports')
    await user.click(await screen.findByRole('button', { name: /drop database/i }))
    const dialog = await screen.findByRole('dialog', { name: /drop database reports/i })
    await user.type(within(dialog).getByLabelText(/to confirm/i), 'reports')
    mockServer.use(http.get('*/api/v1/tasks/:id', expired))
    await user.click(within(dialog).getByRole('button', { name: /^drop database$/i }))
    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'), { timeout: 3000 })
    expect(withData(queryClient)).toHaveLength(0)
  })

  it('another user signing in on the same tab does not see the previous user data', async () => {
    const user = userEvent.setup()
    const { queryClient, router } = await loadSecurity()
    getState().session!.expiresAt = new Date(Date.now() - 1000).toISOString()
    await queryClient.invalidateQueries({ queryKey: keys.roles })
    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'))

    await user.type(await screen.findByLabelText('User name'), 'viewer')
    await user.type(screen.getByLabelText('Password'), 'demo-password')
    const signedInAt = Date.now()
    await user.click(screen.getByRole('button', { name: /^sign in$/i }))
    await waitFor(() => expect(router.currentRoute.value.name).toBe('dashboard'))
    // Nothing the administrator loaded is left, and nothing admin-only is held at all.
    const queries = queryClient.getQueryCache().getAll()
    expect(
      queries.filter((q) => ['roles', 'networkAccess', 'audit'].includes(String(q.queryKey[0]))),
    ).toHaveLength(0)
    for (const q of queries.filter((x) => x.state.data !== undefined)) {
      expect(q.state.dataUpdatedAt).toBeGreaterThanOrEqual(signedInAt)
    }
    expect(screen.queryByText('app_ro')).not.toBeInTheDocument()
  })

  it('discards the late response of a request that was in flight', async () => {
    const { queryClient, pinia } = await renderApp('/dashboard')
    mockServer.use(
      http.get('*/api/v1/server-groups', async () => {
        await delay(250)
        return HttpResponse.json({ items: [{ name: 'late', description: '', serverIds: [] }] })
      }),
    )
    const pending = queryClient.fetchQuery({
      queryKey: keys.serverGroups,
      queryFn: endpoints.listServerGroups,
    })
    pending.catch(() => undefined)
    // A request made outside the query client (a dialog that saves with a direct call) is cut as well.
    const direct = endpoints.listServerGroups()
    direct.catch(() => undefined)
    // Both requests have really been sent when the session ends.
    await delay(60)
    useSessionStore(pinia).expire()
    await expect(pending).rejects.toBeDefined()
    await expect(direct).rejects.toMatchObject({ name: 'ApiError' })
    await delay(300)
    expect(queryClient.getQueryData(keys.serverGroups)).toBeUndefined()
  })

  it('stops following tasks', async () => {
    const { pinia } = await renderApp('/dashboard')
    let polls = 0
    const scope = effectScope()
    const tracker = scope.run(() =>
      useTask({
        fetchTask: async () => {
          polls += 1
          throw new Error('should not be polled after the session ended')
        },
      }),
    )!
    tracker.track(1)
    useSessionStore(pinia).expire()
    await delay(120)
    expect(polls).toBe(0)
    scope.stop()
  })

  it('another tab signing in as a different user is noticed when this tab regains focus', async () => {
    const { queryClient, router } = await loadSecurity()
    // The browser shares one session cookie: the other tab replaces the session.
    await endpoints.login({ username: 'viewer', password: 'demo-password' })
    window.dispatchEvent(new Event('focus'))
    await waitFor(() => expect(useSessionStore().username).toBe('viewer'))
    expect(queryClient.getQueryCache().findAll({ queryKey: keys.roles })).toHaveLength(0)
    await waitFor(() => expect(router.currentRoute.value.name).toBe('dashboard'))
    expect(screen.queryByText('app_ro')).not.toBeInTheDocument()
  })
})
