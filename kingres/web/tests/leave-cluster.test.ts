// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Leaving the cluster (contract 0.4.0): each refusal is explained as what to do first, the
// confirmation is typed, "force" exists only for an unreachable leader and states its consequence,
// and while the server resets itself the panel stays calm and ends at the setup wizard.
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { mockConfig, createTask, type MockState } from '@/mocks/state'
import { availability, leavePolling } from '@/api/availability'
import { useSessionStore } from '@/stores/session'
import { renderApp } from './helpers'

const LOCAL = '10.0.0.11'
const HOSTNAME = 'kdb-1.example.internal'
const saved = { ...leavePolling }
beforeEach(() => {
  leavePolling.intervalMs = 30
  mockConfig.leaveResetMs = 250
})
afterEach(() => {
  Object.assign(leavePolling, saved)
  mockConfig.leaveResetMs = 2000
  availability.leftNotice = false
  mockServer.events.removeAllListeners()
})

/** Takes the server out of the map group, so that only the check under test can refuse. */
function freeOfMapGroup(s: MockState): void {
  const map = s.groups.find((g) => g.isMapGroup)
  if (!map) return
  map.primaryServerId = '10.0.0.12'
  map.preferredPrimaryServerId = '10.0.0.12'
  map.standbyServerIds = ['10.0.0.13']
}

/** Takes the server out of every group, so that it holds no bucket copy and no map group. */
function leavable(s: MockState): void {
  freeOfMapGroup(s)
  for (const g of s.groups) {
    if (g.isMapGroup) continue
    if (g.primaryServerId === LOCAL) g.primaryServerId = g.standbyServerIds.shift() ?? '10.0.0.12'
    g.standbyServerIds = g.standbyServerIds.filter((x) => x !== LOCAL)
  }
}

function trackRequests(): string[] {
  const seen: string[] = []
  mockServer.events.on('request:start', ({ request }) => {
    seen.push(`${request.method} ${new URL(request.url).pathname.replace('/api/v1', '')}`)
  })
  return seen
}

async function openLeave(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: /^leave cluster$/i }))
  return screen.findByRole('dialog', { name: /leave the cluster/i })
}
async function confirmLeave(
  user: ReturnType<typeof userEvent.setup>,
  dialog: HTMLElement,
  name = /^leave cluster$/i,
) {
  const box = within(dialog).getByRole('textbox')
  if ((box as HTMLInputElement).value === '') await user.type(box, HOSTNAME)
  await user.click(within(dialog).getByRole('button', { name }))
}

describe('leave cluster: before the request', () => {
  it('lists what must be done first, needs the typed server name, and offers no force', async () => {
    const user = userEvent.setup()
    await renderApp('/this-server')
    const dialog = await openLeave(user)
    expect(dialog).toHaveTextContent(/before you leave/i)
    expect(dialog).toHaveTextContent(/holds a copy of any bucket/i)
    expect(within(dialog).queryByRole('checkbox')).toBeNull()
    const button = within(dialog).getByRole('button', { name: /^leave cluster$/i })
    expect(button).toBeDisabled()
    await user.type(within(dialog).getByRole('textbox'), HOSTNAME)
    expect(button).toBeEnabled()
  })
})

describe('leave cluster: refusals are explained as what to do first', () => {
  const cases: {
    name: string
    arrange: (s: MockState) => void
    heading: RegExp
    todo: RegExp
  }[] = [
    {
      name: 'server_hosts_map_group',
      arrange: () => undefined,
      heading: /server hosts the cluster metadata/i,
      todo: /first move the manager role and the map group/i,
    },
    {
      name: 'server_owns_buckets',
      arrange: freeOfMapGroup,
      heading: /server still holds data/i,
      todo: /turn off "should hold buckets".*start a rebalance/i,
    },
    {
      name: 'last_server',
      arrange: (s) => {
        s.servers = s.servers.slice(0, 1)
      },
      heading: /last server of the cluster/i,
      todo: /only server of the cluster/i,
    },
    {
      name: 'rebalance_in_progress',
      arrange: (s) => {
        leavable(s)
        s.rebalance.state = 'running'
      },
      heading: /rebalance in progress/i,
      todo: /wait until it has finished, or stop it/i,
    },
    {
      name: 'operation_in_progress',
      arrange: (s) => {
        leavable(s)
        createTask(s, {
          kind: 'server.leave',
          subject: { type: 'server', id: LOCAL },
          steps: ['Check again'],
          apply: () => undefined,
        })
        // createTask caps the duration; this earlier leave must still be running.
        const rec = s.tasks[0]
        if (rec) rec.durationMs = 600_000
      },
      heading: /operation in progress/i,
      todo: /still running.*tasks page/i,
    },
  ]
  for (const c of cases) {
    it(c.name, async () => {
      c.arrange(getState())
      const user = userEvent.setup()
      await renderApp('/this-server')
      const dialog = await openLeave(user)
      await confirmLeave(user, dialog)
      await within(dialog).findByText(c.heading)
      expect(dialog).toHaveTextContent(c.todo)
      // A refusal is final: no force option, and the server is still part of the cluster.
      expect(within(dialog).queryByRole('checkbox')).toBeNull()
      expect(getState().leave).toBeNull()
    })
  }

  it('names the databases that still have data on the server', async () => {
    freeOfMapGroup(getState())
    const user = userEvent.setup()
    await renderApp('/this-server')
    const dialog = await openLeave(user)
    await confirmLeave(user, dialog)
    await within(dialog).findByText(/server still holds data/i)
    expect(dialog).toHaveTextContent(/shop: \d+ bucket copies/i)
  })
})

describe('leave cluster: an unreachable leader', () => {
  it('force is offered only after that refusal, states its consequence and needs a tick', async () => {
    leavable(getState())
    getState().leaderReachable = false
    const user = userEvent.setup()
    const seen = trackRequests()
    const { router, pinia } = await renderApp('/this-server')
    const dialog = await openLeave(user)
    expect(within(dialog).queryByRole('checkbox')).toBeNull()
    await confirmLeave(user, dialog)
    await within(dialog).findByText(/service unavailable/i)
    const force = await within(dialog).findByRole('checkbox', { name: /force/i })
    expect(dialog).toHaveTextContent(/not revoked/i)
    expect(dialog).toHaveTextContent(/keeps listing this server/i)
    expect(dialog).toHaveTextContent(/remove server/i)
    expect(dialog).toHaveTextContent(/checks above\s+still apply/i)
    const anyway = within(dialog).getByRole('button', { name: /^leave anyway$/i })
    expect(anyway).toBeDisabled()
    await user.click(force)
    expect(anyway).toBeEnabled()
    await user.click(anyway)
    // The forced answer is a finished task with id 0; it is never fetched.
    await waitFor(() => expect(router.currentRoute.value.name).toBe('setup'), { timeout: 4000 })
    expect(seen.filter((r) => r === 'GET /tasks/0')).toEqual([])
    expect(useSessionStore(pinia).isSignedIn).toBe(false)
    expect(seen).not.toContain('POST /auth/logout')
  })

  it('a refusal by the local checks stays final even with force', async () => {
    getState().leaderReachable = false
    const user = userEvent.setup()
    await renderApp('/this-server')
    const dialog = await openLeave(user)
    await confirmLeave(user, dialog)
    // The map group refuses before the leader is asked.
    await within(dialog).findByText(/server hosts the cluster metadata/i)
    expect(within(dialog).queryByRole('checkbox')).toBeNull()
  })
})

describe('leave cluster: while the server resets itself', () => {
  it('follows the task, then the 503s, ends at the setup wizard with the notice, with few requests', async () => {
    leavable(getState())
    const user = userEvent.setup()
    const seen = trackRequests()
    const { router, pinia } = await renderApp('/this-server')
    const dialog = await openLeave(user)
    await confirmLeave(user, dialog)
    await waitFor(() => expect(router.currentRoute.value.name).toBe('setup'), { timeout: 5000 })
    expect(useSessionStore(pinia).isSignedIn).toBe(false)
    expect(await screen.findByText(/left the cluster and was reset/i)).toBeInTheDocument()
    expect(document.body).toHaveTextContent(/set it up again to create or join a cluster/i)
    expect(availability.resetting).toBe(false)
    // After the accepted request only the task and the setup state were asked, a bounded number of times.
    const after = seen.slice(seen.indexOf('POST /this-server/leave-cluster') + 1)
    const unexpected = after.filter(
      (r) => !/^GET \/tasks\/\d+$/.test(r) && r !== 'GET /setup/state',
    )
    expect(unexpected).toEqual([])
    expect(after.length).toBeLessThan(40)
    expect(seen).not.toContain('POST /auth/logout')
    expect(screen.queryAllByRole('alert')).toHaveLength(0)
  })
})
