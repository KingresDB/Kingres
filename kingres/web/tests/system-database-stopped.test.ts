// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Stopping the instance that hosts the system database: afterwards the server answers 503
// `system_database_stopped` to everything except "start instance". The panel shows one clear state
// with a working Start button, keeps the session, and does not ask the server again on its own
// timers; when the instance is back it returns to normal.
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { availability, recovery } from '@/api/availability'
import { shouldRetry, ApiError } from '@/api/problem'
import { useSessionStore } from '@/stores/session'
import { renderApp } from './helpers'

const saved = { ...recovery }
beforeEach(() => {
  recovery.watchMs = 40
  recovery.afterStartMs = 40
})
afterEach(() => {
  Object.assign(recovery, saved)
  mockServer.events.removeAllListeners()
})

function trackRequests(): string[] {
  const seen: string[] = []
  mockServer.events.on('request:start', ({ request }) => {
    seen.push(`${request.method} ${new URL(request.url).pathname.replace('/api/v1', '')}`)
  })
  return seen
}

async function stopSystemInstance(user: ReturnType<typeof userEvent.setup>) {
  const row = (await screen.findByRole('rowheader', { name: '#11' })).closest('tr') as HTMLElement
  await user.click(within(row).getByRole('button', { name: /^stop$/i }))
  const dialog = await screen.findByRole('dialog', { name: /stop instance #11/i })
  await user.type(within(dialog).getByRole('textbox'), 'stop')
  await user.click(within(dialog).getByRole('button', { name: /^stop instance$/i }))
}

describe('the system database is stopped', () => {
  it('shows one clear state with a Start button; the session stays; the server is not asked again on every timer', async () => {
    const user = userEvent.setup()
    const { pinia, router } = await renderApp('/this-server')
    await stopSystemInstance(user)
    const heading = await screen.findByRole('heading', { name: /system database is stopped/i })
    expect(heading).toBeInTheDocument()
    expect(getState().systemDatabaseStopped).toBe(true)
    // Not signed out, not on the sign-in page, and no wall of errors.
    expect(useSessionStore(pinia).isSignedIn).toBe(true)
    expect(router.currentRoute.value.name).not.toBe('login')
    expect(screen.queryAllByRole('alert')).toHaveLength(0)
    expect(screen.getByRole('button', { name: /^start instance #11$/i })).toBeEnabled()
    expect(document.body).toHaveTextContent(/can take a while/i)
    // Calm: while the state is shown, the only call is the cheap watch on the session.
    const seen = trackRequests()
    await new Promise((r) => setTimeout(r, 400))
    const others = seen.filter((r) => r !== 'GET /auth/session')
    expect(others).toEqual([])
    expect(seen.length).toBeLessThan(20)
  })

  it('Start instance starts it, and the panel returns to the page it was on', async () => {
    const user = userEvent.setup()
    const { pinia } = await renderApp('/this-server')
    await stopSystemInstance(user)
    await screen.findByRole('heading', { name: /system database is stopped/i })
    const seen = trackRequests()
    await user.click(screen.getByRole('button', { name: /^start instance #11$/i }))
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: /system database is stopped/i })).toBeNull(),
    )
    expect(seen).toContain('POST /this-server/instances/11/start')
    expect(getState().systemDatabaseStopped).toBe(false)
    expect(availability.systemDatabaseStopped).toBe(false)
    expect(useSessionStore(pinia).isSignedIn).toBe(true)
    // Back to normal: the page reloads its data and shows the instance running again.
    const row = (await screen.findByRole('rowheader', { name: '#11' })).closest('tr') as HTMLElement
    await waitFor(() => expect(row).toHaveTextContent(/running/i))
  })

  it('a start that finds the instance already running is treated as recovery in progress', async () => {
    const user = userEvent.setup()
    await renderApp('/this-server')
    await stopSystemInstance(user)
    await screen.findByRole('heading', { name: /system database is stopped/i })
    // An operator started it on the machine just before the button was pressed.
    getState().systemDatabaseStopped = false
    getState().instanceStates.set(11, 'running')
    await user.click(screen.getByRole('button', { name: /^start instance #11$/i }))
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: /system database is stopped/i })).toBeNull(),
    )
  })

  it('sign-in explains the state instead of saying the credentials are wrong', async () => {
    getState().systemDatabaseStopped = true
    const user = userEvent.setup()
    await renderApp('/login', { user: null })
    await user.type(await screen.findByLabelText(/user name|username/i), 'admin')
    await user.type(screen.getByLabelText(/^password/i), 'demo-password')
    await user.click(screen.getByRole('button', { name: /sign in/i }))
    const notice = await screen.findByRole('alert')
    expect(notice).toHaveTextContent(/system database is stopped/i)
    expect(notice).not.toHaveTextContent(/not correct/i)
  })

  it('the client does not retry the code on its own', () => {
    const error = new ApiError({
      type: 'about:blank',
      title: 'x',
      status: 503,
      code: 'system_database_stopped',
    })
    expect(shouldRetry(0, error)).toBe(false)
  })
})
