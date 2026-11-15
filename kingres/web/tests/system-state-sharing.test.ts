// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The "system database is stopped" state is shared between the tabs of one browser (BroadcastChannel,
// with a `storage` event as fallback). What travels is one boolean: no session, no CSRF token, no name.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/vue'
import { getState } from '@/mocks/http'
import { availability } from '@/api/availability'
import { createSystemStateSharing, readSharedState, STORAGE_KEY } from '@/lib/cross-tab'
import { useSessionStore } from '@/stores/session'
import { renderApp } from './helpers'

/** A BroadcastChannel stand-in: every instance of a name sees what any other instance posts. */
class FakeChannel {
  static instances: FakeChannel[] = []
  static posted: unknown[] = []
  onmessage: ((event: MessageEvent<unknown>) => void) | null = null
  closed = false
  constructor(readonly name: string) {
    FakeChannel.instances.push(this)
  }
  postMessage(data: unknown): void {
    FakeChannel.posted.push(data)
    for (const other of FakeChannel.instances)
      if (other !== this && !other.closed && other.name === this.name)
        other.onmessage?.({ data } as MessageEvent<unknown>)
  }
  close(): void {
    this.closed = true
  }
  /** A message from a tab that is not part of the test. */
  static deliver(data: unknown): void {
    for (const c of FakeChannel.instances)
      if (!c.closed) c.onmessage?.({ data } as MessageEvent<unknown>)
  }
}

function useFakeChannel(): void {
  FakeChannel.instances = []
  FakeChannel.posted = []
  vi.stubGlobal('BroadcastChannel', FakeChannel)
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('what is accepted from another tab', () => {
  it('only {kind, stopped: boolean}', () => {
    expect(readSharedState({ kind: 'system-database-state', stopped: true })).toBe(true)
    expect(readSharedState({ kind: 'system-database-state', stopped: false })).toBe(false)
    for (const bad of [
      null,
      'stopped',
      42,
      {},
      { stopped: true },
      { kind: 'other', stopped: true },
      { kind: 'system-database-state', stopped: 'yes' },
      { kind: 'system-database-state' },
    ])
      expect(readSharedState(bad)).toBeNull()
  })
})

describe('BroadcastChannel', () => {
  it('shares one boolean and nothing else, and ignores malformed messages', () => {
    useFakeChannel()
    const heard: boolean[] = []
    const a = createSystemStateSharing((s) => heard.push(s))
    const b = createSystemStateSharing(() => undefined)
    b.share(true)
    b.share(false)
    expect(heard).toEqual([true, false])
    expect(FakeChannel.posted).toEqual([
      { kind: 'system-database-state', stopped: true },
      { kind: 'system-database-state', stopped: false },
    ])
    FakeChannel.deliver({ kind: 'system-database-state', stopped: 'x', csrfToken: 'secret' })
    FakeChannel.deliver('hello')
    expect(heard).toEqual([true, false])
    a.dispose()
    b.dispose()
    expect(FakeChannel.instances.every((c) => c.closed)).toBe(true)
  })
})

describe('storage event fallback (no BroadcastChannel)', () => {
  it('writes the message and removes it at once, so nothing stays in storage', () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const set = vi.spyOn(Storage.prototype, 'setItem')
    const remove = vi.spyOn(Storage.prototype, 'removeItem')
    const sharing = createSystemStateSharing(() => undefined)
    sharing.share(true)
    expect(set).toHaveBeenCalledTimes(1)
    const [key, value] = set.mock.calls[0] as [string, string]
    expect(key).toBe(STORAGE_KEY)
    const payload = JSON.parse(value) as Record<string, unknown>
    expect(payload.kind).toBe('system-database-state')
    expect(payload.stopped).toBe(true)
    // Nothing else: no session, no token, no name.
    expect(Object.keys(payload).sort()).toEqual(['at', 'kind', 'stopped'])
    expect(remove).toHaveBeenCalledWith(STORAGE_KEY)
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
    sharing.dispose()
  })

  it('hears another tab, and ignores other keys and malformed values', () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    const heard: boolean[] = []
    const sharing = createSystemStateSharing((s) => heard.push(s))
    const fire = (key: string | null, newValue: string | null) =>
      window.dispatchEvent(new StorageEvent('storage', { key, newValue }))
    fire(STORAGE_KEY, JSON.stringify({ kind: 'system-database-state', stopped: true, at: 1 }))
    fire(STORAGE_KEY, null) // the removal that follows the write
    fire('another-key', JSON.stringify({ kind: 'system-database-state', stopped: false }))
    fire(STORAGE_KEY, 'not json')
    fire(STORAGE_KEY, JSON.stringify({ kind: 'system-database-state', stopped: 1 }))
    expect(heard).toEqual([true])
    sharing.dispose()
    fire(STORAGE_KEY, JSON.stringify({ kind: 'system-database-state', stopped: false, at: 2 }))
    expect(heard).toEqual([true]) // disposed: no listener
  })
})

describe('between two tabs of the panel', () => {
  it('a tab that meets the stopped state tells the others, with nothing but the boolean', async () => {
    useFakeChannel()
    const { queryClient } = await renderApp('/dashboard')
    await screen.findByRole('heading', { name: /dashboard/i })
    getState().systemDatabaseStopped = true
    getState().instanceStates.set(11, 'stopped')
    await queryClient.invalidateQueries({ queryKey: ['cluster'] })
    await screen.findByRole('heading', { name: /system database is stopped/i })
    expect(FakeChannel.posted).toEqual([{ kind: 'system-database-state', stopped: true }])
    expect(JSON.stringify(FakeChannel.posted)).not.toMatch(/csrf|token|admin|session/i)
  })

  it('a signed-in tab shows the stopped state when another tab says so, and leaves it when told it runs', async () => {
    useFakeChannel()
    await renderApp('/dashboard')
    await screen.findByRole('heading', { name: /dashboard/i })
    FakeChannel.deliver({ kind: 'system-database-state', stopped: true })
    await screen.findByRole('heading', { name: /system database is stopped/i })
    expect(availability.systemDatabaseStopped).toBe(true)
    // The state was heard, not made here: it is not echoed back.
    expect(FakeChannel.posted).toEqual([])
    FakeChannel.deliver({ kind: 'system-database-state', stopped: false })
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: /system database is stopped/i })).toBeNull(),
    )
    expect(availability.systemDatabaseStopped).toBe(false)
    expect(useSessionStore(await import('pinia').then((p) => p.getActivePinia()!)).isSignedIn).toBe(
      true,
    )
  })

  it('a tab that is not signed in learns it, and the sign-in page explains what to do', async () => {
    useFakeChannel()
    const { router } = await renderApp('/login', { user: null })
    await screen.findByRole('heading', { name: /sign in to kingresdb/i })
    expect(screen.queryByTestId('system-database-stopped-signin')).toBeNull()
    FakeChannel.deliver({ kind: 'system-database-state', stopped: true })
    expect(await screen.findByTestId('system-database-stopped-signin')).toBeInTheDocument()
    expect(router.currentRoute.value.name).toBe('login')
    FakeChannel.deliver({ kind: 'system-database-state', stopped: false })
    await waitFor(() => expect(screen.queryByTestId('system-database-stopped-signin')).toBeNull())
  })

  it('malformed messages change nothing', async () => {
    useFakeChannel()
    await renderApp('/dashboard')
    await screen.findByRole('heading', { name: /dashboard/i })
    FakeChannel.deliver({ kind: 'system-database-state', stopped: 'true' })
    FakeChannel.deliver({ stopped: true })
    expect(availability.systemDatabaseStopped).toBe(false)
    expect(screen.queryByRole('heading', { name: /system database is stopped/i })).toBeNull()
  })
})
