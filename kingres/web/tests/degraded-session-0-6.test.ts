// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.6.0, additive 11 and 12: `getSession` and `listThisServerInstances` answer while the system
// database is stopped (`Session.degraded`, `degradedReason`, `clientAddress`). The panel keeps nothing in
// browser storage for this case: after a reload it asks the server.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, screen, waitFor } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { MOCK_CLIENT_ADDRESS } from '@/mocks/handlers'
import { availability, recovery } from '@/api/availability'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { useSessionStore } from '@/stores/session'
import { disposeBuiltApps, renderApp } from './helpers'

const saved = { ...recovery }
beforeEach(() => {
  recovery.watchMs = 40
  recovery.afterStartMs = 40
})
afterEach(() => {
  Object.assign(recovery, saved)
  mockServer.events.removeAllListeners()
  vi.restoreAllMocks()
})

function stopTheDatabase(): void {
  getState().systemDatabaseStopped = true
  getState().instanceStates.set(11, 'stopped')
}

function reload(): void {
  cleanup()
  disposeBuiltApps()
}

/** Everything the page could read: the keys and values of both web storages. */
function storageDump(): string[] {
  const rows: string[] = []
  for (const store of [localStorage, sessionStorage])
    for (let i = 0; i < store.length; i++) {
      const key = store.key(i) as string
      rows.push(`${key}=${store.getItem(key)}`)
    }
  return rows
}

function expectOnlyTheme(): void {
  expect(storageDump().filter((r) => !r.startsWith('kingres.theme='))).toEqual([])
}

const BIDI = String.fromCharCode(0x202e)
const ZERO_WIDTH = String.fromCharCode(0x200b)

describe('the mock follows the contract for the stopped state', () => {
  it('answers getSession (degraded) and the instance list, and 503 for everything else', async () => {
    await renderApp('/dashboard')
    stopTheDatabase()
    const session = await endpoints.getSession()
    expect(session.degraded).toBe(true)
    expect(session.degradedReason).toBeTruthy()
    expect(session.clientAddress).toBe(MOCK_CLIENT_ADDRESS)
    const list = await endpoints.listThisServerInstances()
    expect(list.items.length).toBeGreaterThan(0)
    expect(list.items.every((i) => i.sizeBytes === null)).toBe(true)
    for (const call of [
      () => endpoints.listServers(),
      () => endpoints.logout(),
      () => endpoints.login({ username: 'admin', password: 'demo-password' }),
    ]) {
      const error = await call().catch((e: unknown) => e)
      expect(error).toBeInstanceOf(ApiError)
      expect((error as ApiError).code).toBe('system_database_stopped')
    }
  })
})

describe('a reload while the system database is stopped', () => {
  it('calls getSession, sees degraded, shows the stopped state and starts the instance from the list', async () => {
    const user = userEvent.setup()
    await renderApp('/dashboard')
    stopTheDatabase()
    reload()
    const seen: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      seen.push(`${request.method} ${new URL(request.url).pathname.replace('/api/v1', '')}`)
    })
    const { pinia, router } = await renderApp('/dashboard', { user: null })
    await screen.findByRole('heading', { name: /system database is stopped/i })
    expect(router.currentRoute.value.name).not.toBe('login')
    expect(useSessionStore(pinia).session?.degraded).toBe(true)
    expect(seen).toContain('GET /auth/session')
    expect(seen).toContain('GET /this-server/instances')
    // Nothing was read from web storage: the instance is the one the server listed.
    await waitFor(() => expect(availability.instanceId).toBe(11))
    expect(screen.getByTestId('instance-known')).toHaveTextContent(/instance #11/i)
    await user.click(screen.getByRole('button', { name: /^start instance #11$/i }))
    await waitFor(() => expect(getState().systemDatabaseStopped).toBe(false))
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: /system database is stopped/i })).toBeNull(),
    )
    await waitFor(() => expect(useSessionStore(pinia).session?.degraded).toBeUndefined())
  })

  it('keeps the stopped state while later getSession answers stay degraded', async () => {
    await renderApp('/dashboard')
    stopTheDatabase()
    reload()
    const { pinia } = await renderApp('/dashboard', { user: null })
    await screen.findByRole('heading', { name: /system database is stopped/i })
    // The stopped page asks again every few milliseconds here; it must stay.
    await new Promise((r) => setTimeout(r, 200))
    expect(availability.systemDatabaseStopped).toBe(true)
    expect(useSessionStore(pinia).isSignedIn).toBe(true)
  })

  it('without a list of instances there is no Start button and the page says what to do', async () => {
    await renderApp('/dashboard')
    stopTheDatabase()
    reload()
    mockServer.use(
      http.get('*/api/v1/this-server/instances', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Service unavailable',
            status: 503,
            code: 'system_database_stopped',
          },
          { status: 503 },
        ),
      ),
    )
    await renderApp('/dashboard', { user: null })
    await screen.findByRole('heading', { name: /system database is stopped/i })
    expect(availability.instanceId).toBeNull()
    expect(screen.getByTestId('instance-unknown')).toHaveTextContent(/does not guess/i)
    expect(screen.queryByRole('button', { name: /start instance/i })).toBeNull()
  })

  it('shows the reason of the server through SafeName (hostile text)', async () => {
    await renderApp('/dashboard')
    stopTheDatabase()
    reload()
    mockServer.use(
      http.get('*/api/v1/auth/session', () =>
        HttpResponse.json({
          username: 'admin',
          clusterRoles: ['kingres_admin'],
          expiresAt: '2999-01-01T00:00:00Z',
          csrfToken: 'tok',
          serverId: '10.0.0.11',
          degraded: true,
          degradedReason: `stopped${BIDI}evil${ZERO_WIDTH} reason`,
          clientAddress: `198.51.100.${BIDI}9`,
        }),
      ),
    )
    await renderApp('/dashboard', { user: null })
    const shown = await screen.findByTestId('degraded-reason')
    expect(shown.textContent).not.toContain(BIDI)
    expect(shown.textContent).not.toContain(ZERO_WIDTH)
    expect(shown).toHaveTextContent(/stopped/)
  })

  it('switches off everything else with a reason: navigation and sign-out make no request', async () => {
    const user = userEvent.setup()
    await renderApp('/dashboard')
    stopTheDatabase()
    reload()
    await renderApp('/dashboard', { user: null })
    await screen.findByRole('heading', { name: /system database is stopped/i })
    expect(screen.getByTestId('nav-off-reason')).toHaveTextContent(/accepts only/i)
    expect(screen.getAllByTestId('nav-disabled').length).toBeGreaterThan(3)
    expect(screen.queryByRole('link', { name: /^servers$/i })).toBeNull()
    const signOut = screen.getByRole('button', { name: /sign out/i })
    expect(signOut).toHaveAttribute('aria-disabled', 'true')
    expect(signOut).toHaveAccessibleDescription(
      /not possible while the system database is stopped/i,
    )
    const seen: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      seen.push(`${request.method} ${new URL(request.url).pathname}`)
    })
    await user.click(signOut)
    expect(seen.some((s) => s.includes('/auth/logout'))).toBe(false)
    expect(screen.getByRole('heading', { name: /system database is stopped/i })).toBeVisible()
  })
})

describe('nothing but the theme is kept in web storage', () => {
  it('after sign-in, in the degraded state, and after sign-out', async () => {
    const user = userEvent.setup()
    await renderApp('/dashboard')
    // The theme is the one thing that may be stored.
    await user.click(screen.getByRole('button', { name: /switch theme/i }))
    await waitFor(() =>
      expect(storageDump().some((r) => r.startsWith('kingres.theme='))).toBe(true),
    )
    expectOnlyTheme()

    // The degraded state: the panel was reloaded while the database is stopped.
    stopTheDatabase()
    reload()
    const second = await renderApp('/dashboard', { user: null })
    await screen.findByRole('heading', { name: /system database is stopped/i })
    const store = useSessionStore(second.pinia)
    expect(store.session?.degraded).toBe(true)
    expectOnlyTheme()
    expect(storageDump().join('\n')).not.toContain(store.session?.csrfToken ?? 'no-token')

    // After sign-out (the instance runs again).
    getState().systemDatabaseStopped = false
    getState().instanceStates.set(11, 'running')
    await store.refresh()
    await store.signOut()
    expectOnlyTheme()
  })
})
