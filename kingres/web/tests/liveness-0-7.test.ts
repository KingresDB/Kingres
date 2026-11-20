// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.7.0, liveness: a server that sends no heartbeat is `suspect` and then `dead` (the view of the server that
// answers), the time since its last heartbeat is shown wherever a server that is not live is shown, the counts and the
// list agree, and a follower that has not heard the leader answers 503 `leader_unavailable`.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import {
  effectiveStatus,
  mockConfig,
  nowIso,
  overview,
  reviveServer,
  serverView,
  silenceServer,
} from '@/mocks/state'
import { applySwitches } from '@/mocks/switches'
import { heartbeatAge } from '@/lib/liveness'
import { renderApp } from './helpers'

describe('the mock heartbeats', () => {
  it('a silent server is suspect after the suspect timeout and dead after the failure timeout, and live when heard again', () => {
    const s = getState()
    const rec = s.servers[1]!
    expect(effectiveStatus(rec)).toBe('live')
    silenceServer(s, rec.id, Date.now() - (mockConfig.suspectMs + 100))
    expect(effectiveStatus(rec)).toBe('suspect')
    silenceServer(s, rec.id, Date.now() - (mockConfig.failureMs + 100))
    expect(effectiveStatus(rec)).toBe('dead')
    reviveServer(s, rec.id)
    expect(effectiveStatus(rec)).toBe('live')
  })

  it('the server that answers cannot be silent (its own heartbeat is now)', () => {
    expect(silenceServer(getState(), '10.0.0.11')).toBe(false)
  })

  it('the URL switch starts a server silent, suspect or dead', () => {
    const s = getState()
    applySwitches(new URLSearchParams('silent=10.0.0.12:dead,10.0.0.13:suspect'), s)
    expect(effectiveStatus(s.servers[1]!)).toBe('dead')
    expect(effectiveStatus(s.servers[2]!)).toBe('suspect')
  })
})

describe('Servers page', () => {
  it('shows the time since the last heartbeat for a server that is not live, and not for a live one', async () => {
    silenceServer(getState(), '10.0.0.12', Date.now() - (mockConfig.suspectMs + 2000))
    await renderApp('/servers')
    const row12 = within(await screen.findByTestId('server-row-10.0.0.12'))
    expect(row12.getByText('SUSPECT')).toBeInTheDocument()
    expect(row12.getByTestId('last-heartbeat')).toHaveTextContent(/last heartbeat \d+s ago/i)
    const row11 = within(screen.getByTestId('server-row-10.0.0.11'))
    expect(row11.queryByTestId('last-heartbeat')).toBeNull()
    expect(screen.getByTestId('liveness-note')).toHaveTextContent(
      /view of the server that answered/i,
    )
  })

  it('a dead server is DEAD with its heartbeat age, and a silent server shows no uptime or resources', async () => {
    silenceServer(getState(), '10.0.0.13', Date.now() - (mockConfig.failureMs + 3000))
    await renderApp('/servers')
    const row = within(await screen.findByTestId('server-row-10.0.0.13'))
    expect(row.getByText('DEAD')).toBeInTheDocument()
    expect(row.getByTestId('last-heartbeat')).toHaveTextContent(/\ds ago/)
    // No uptime and no resources: "n/a", never 0.
    expect(row.getAllByTestId('na').length).toBeGreaterThan(0)
  })

  it('goes from live to suspect to dead and back while the page is open', async () => {
    const s = getState()
    await renderApp('/servers')
    const row = within(await screen.findByTestId('server-row-10.0.0.12'))
    expect(row.getByText('LIVE')).toBeInTheDocument()
    silenceServer(s, '10.0.0.12', Date.now() - (mockConfig.suspectMs + 200))
    await waitFor(() => expect(row.getByText('SUSPECT')).toBeInTheDocument(), { timeout: 30_000 })
    expect(row.getByTestId('last-heartbeat')).toBeInTheDocument()
    silenceServer(s, '10.0.0.12', Date.now() - (mockConfig.failureMs + 200))
    await waitFor(() => expect(row.getByText('DEAD')).toBeInTheDocument(), { timeout: 30_000 })
    reviveServer(s, '10.0.0.12')
    await waitFor(() => expect(row.getByText('LIVE')).toBeInTheDocument(), { timeout: 30_000 })
    expect(row.queryByTestId('last-heartbeat')).toBeNull()
  }, 90_000)

  it('the manager raises serverSuspect, then serverDown with the server as subject, and clears them', async () => {
    const s = getState()
    silenceServer(s, '10.0.0.12', Date.now() - (mockConfig.suspectMs + 500))
    await renderApp('/alerts')
    const suspect = await screen.findByText('Server 10.0.0.12 is not answering')
    expect(
      within(suspect.closest('tr') as HTMLElement).getByRole('link', { name: '10.0.0.12' }),
    ).toBeInTheDocument()
    expect(s.alerts.find((a) => a.kind === 'serverSuspect')?.subject?.serverId).toBe('10.0.0.12')
    silenceServer(s, '10.0.0.12', Date.now() - (mockConfig.failureMs + 500))
    await renderApp('/alerts')
    await screen.findByText('Server 10.0.0.12 is down')
    expect(
      s.alerts.find((a) => a.kind === 'serverSuspect' && a.state !== 'resolved'),
    ).toBeUndefined()
    reviveServer(s, '10.0.0.12')
    await renderApp('/alerts')
    await waitFor(() =>
      expect(
        s.alerts.filter((a) => a.title === 'Server 10.0.0.12 is down' && a.state !== 'resolved'),
      ).toHaveLength(0),
    )
  })
})

describe('Server detail page', () => {
  it('says what suspect and dead mean, from whose view, and when the last heartbeat was', async () => {
    silenceServer(getState(), '10.0.0.12', Date.now() - (mockConfig.suspectMs + 2000))
    await renderApp('/servers/10.0.0.12')
    const notice = await screen.findByTestId('liveness-notice')
    expect(notice).toHaveTextContent(/heartbeats are late/i)
    expect(notice).toHaveTextContent(/last heard a heartbeat from this server \d+s ago/i)
    expect(notice).toHaveTextContent(/view of the answering server/i)
    expect(screen.getByTestId('last-heartbeat')).toHaveTextContent(/\(\d+s ago\)/)
  })

  it('a dead server has its own notice, a live one has none', async () => {
    silenceServer(getState(), '10.0.0.13', Date.now() - (mockConfig.failureMs + 2000))
    await renderApp('/servers/10.0.0.13')
    expect(await screen.findByTestId('liveness-notice')).toHaveTextContent(
      /no heartbeat for the failure timeout/i,
    )
  })

  it('no notice for a live server', async () => {
    await renderApp('/servers/10.0.0.12')
    await screen.findByTestId('last-heartbeat')
    expect(screen.queryByTestId('liveness-notice')).toBeNull()
  })

  it('a server with no heartbeat on record says so, in the list and on its page', async () => {
    const s = getState()
    s.servers[1]!.status = 'suspect'
    const view = () => ({ ...serverView(s, s.servers[1]!), lastHeartbeatAt: null })
    mockServer.use(
      http.get('*/api/v1/servers', () => HttpResponse.json({ items: [view()] })),
      http.get('*/api/v1/servers/10.0.0.12', () =>
        HttpResponse.json({ ...view(), bucketsByDatabase: [] }),
      ),
    )
    await renderApp('/servers')
    expect(await screen.findByText('No heartbeat on record')).toBeInTheDocument()
    await renderApp('/servers/10.0.0.12')
    expect(await screen.findByText(/no heartbeat from this server on record/i)).toBeInTheDocument()
  })
})

describe('Dashboard', () => {
  it('counts suspect and dead servers from the same snapshot as the list, and names the ones that are not live', async () => {
    const s = getState()
    silenceServer(s, '10.0.0.12', Date.now() - (mockConfig.suspectMs + 1000))
    silenceServer(s, '10.0.0.13', Date.now() - (mockConfig.failureMs + 1000))
    await renderApp('/dashboard')
    const card = (await screen.findByText(/1 \/ 3 live/)).closest('.card') as HTMLElement
    expect(card).toHaveTextContent(/1 suspect, 1 dead/)
    const list = within(await screen.findByTestId('not-live-servers'))
    expect(list.getByText('SUSPECT')).toBeInTheDocument()
    expect(list.getByText('DEAD')).toBeInTheDocument()
    expect(list.getAllByText(/last heartbeat \d+s ago/)).toHaveLength(2)
    expect(list.getByRole('link', { name: '10.0.0.12' })).toHaveAttribute(
      'href',
      '/servers/10.0.0.12',
    )
  })

  it('names no server when every server is live', async () => {
    await renderApp('/dashboard')
    await screen.findByText(/3 \/ 3 live/)
    expect(screen.queryByTestId('not-live-servers')).toBeNull()
  })

  it('the overview and the list agree, whatever the heartbeats say', async () => {
    const s = getState()
    silenceServer(s, '10.0.0.12', Date.now() - (mockConfig.failureMs + 1000))
    const { overview, serverView } = await import('@/mocks/state')
    const o = overview(s)
    const listed = s.servers.map((x) => serverView(s, x).status)
    expect(o.servers.dead).toBe(listed.filter((x) => x === 'dead').length)
    expect(o.servers.live).toBe(listed.filter((x) => x === 'live').length)
    expect(o.health).toBe('critical')
  })
})

describe('leader_unavailable', () => {
  it('shows a banner that says what it means and names the other servers, with the manager leader as last known', async () => {
    await renderApp('/servers')
    await screen.findByTestId('server-row-10.0.0.12')
    // The first answers worked (the panel knows the servers); now this server has lost the leader.
    mockConfig.leaderLost = true
    const banner = await screen.findByTestId('banner-leader', {}, { timeout: 30_000 })
    expect(banner).toHaveTextContent(/lost contact with the manager leader/i)
    expect(banner).toHaveTextContent(/nothing can be changed here/i)
    expect(banner).toHaveTextContent('10.0.0.12:8091')
    expect(banner).toHaveTextContent('10.0.0.13:8091')
    // The server that answers (the session's own) is not offered as the place to go.
    expect(banner.textContent).not.toContain('10.0.0.11:8091')
  })

  it('names the manager leader as last known when it is another server', async () => {
    const s = getState()
    const problem = {
      type: 'about:blank',
      title: 'No leader',
      status: 503,
      code: 'leader_unavailable',
    }
    mockServer.use(
      http.get('*/api/v1/cluster', () =>
        mockConfig.leaderLost
          ? HttpResponse.json(problem, { status: 503, headers: { 'Retry-After': '5' } })
          : HttpResponse.json({
              ...overview(s),
              managerLeader: { serverId: '10.0.0.12', address: '10.0.0.12:8091', since: nowIso() },
            }),
      ),
    )
    await renderApp('/servers')
    await screen.findByTestId('server-row-10.0.0.12')
    mockConfig.leaderLost = true
    const banner = await screen.findByTestId('banner-leader', {}, { timeout: 30_000 })
    expect(banner).toHaveTextContent(/10\.0\.0\.12:8091 \(the manager leader, as last known\)/)
  })

  it('shows a hostile address of another server as text', async () => {
    getState().servers[1]!.address = '<img src=x onerror=window.__pwn=1>'
    await renderApp('/servers')
    await screen.findByTestId('server-row-10.0.0.12')
    mockConfig.leaderLost = true
    const banner = await screen.findByTestId('banner-leader', {}, { timeout: 30_000 })
    expect(banner.querySelector('img')).toBeNull()
    expect(banner.textContent).toContain('<img src=x')
  })

  it('the pages show the problem in words, and it goes away when the leader is heard again', async () => {
    mockConfig.leaderLost = true
    await renderApp('/servers')
    expect(await screen.findByText('No manager leader')).toBeInTheDocument()
    expect(screen.getAllByText(/open the console on another server/i).length).toBeGreaterThan(0)
    mockConfig.leaderLost = false
    await waitFor(() => expect(screen.queryByTestId('banner-leader')).toBeNull(), {
      timeout: 30_000,
    })
    await screen.findByTestId('server-row-10.0.0.12', {}, { timeout: 30_000 })
  }, 90_000)

  it('has no banner when the leader is there', async () => {
    await renderApp('/servers')
    await screen.findByTestId('server-row-10.0.0.12')
    expect(screen.queryByTestId('banner-leader')).toBeNull()
  })

  it('still answers the probes and the session while the leader is lost', async () => {
    mockConfig.leaderLost = true
    const { endpoints } = await import('@/api/endpoints')
    await renderApp('/dashboard')
    await expect(endpoints.getSession()).resolves.toMatchObject({ username: 'admin' })
    await expect(endpoints.getCluster()).rejects.toMatchObject({
      code: 'leader_unavailable',
      retryAfterSeconds: 5,
    })
  })
})

describe('helpers', () => {
  it('heartbeatAge', () => {
    const now = Date.parse('2026-11-15T10:00:10Z')
    expect(heartbeatAge({ lastHeartbeatAt: '2026-11-15T10:00:03Z' }, now)?.ago).toBe('7s ago')
    expect(heartbeatAge({ lastHeartbeatAt: null })).toBeNull()
  })
})
