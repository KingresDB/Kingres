// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Local settings of this server (contract 0.4.0): only changes are sent, the restart-required
// state is explained and persistent until the instance restarts, the server's field errors land on
// their fields, and the role of the account decides who may save.
import { afterEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { renderApp } from './helpers'

interface Write {
  method: string
  path: string
  body: Record<string, unknown> | null
}
function recordWrites(): Write[] {
  const seen: Write[] = []
  mockServer.events.on('request:start', async ({ request }) => {
    if (request.method === 'GET' || request.url.includes('/auth/')) return
    const text = await request.clone().text()
    seen.push({
      method: request.method,
      path: new URL(request.url).pathname,
      body: text ? (JSON.parse(text) as Record<string, unknown>) : null,
    })
  })
  return seen
}
afterEach(() => mockServer.events.removeAllListeners())

const patches = (writes: Write[]) => writes.filter((w) => w.method === 'PATCH')

describe('local settings', () => {
  it('a log level change is sent alone, applies at once and needs no restart', async () => {
    const writes = recordWrites()
    const user = userEvent.setup()
    await renderApp('/this-server')
    await user.selectOptions(await screen.findByLabelText(/^log level$/i), 'debug')
    await user.click(screen.getByRole('button', { name: /save local settings/i }))
    await waitFor(() => expect(patches(writes)).toHaveLength(1))
    expect(patches(writes)[0]?.body).toEqual({ logLevel: 'debug' })
    await screen.findByText(/^saved\.$/i)
    expect(screen.queryByText(/a restart is required/i)).toBeNull()
    expect(getState().localSettings.logLevel).toBe('debug')
  })

  it('a memory change shows the restart-required state, which stays until the instance restarts', async () => {
    const user = userEvent.setup()
    await renderApp('/this-server')
    const memory = await screen.findByLabelText(/memory quota/i)
    await user.clear(memory)
    await user.type(memory, '8192')
    await user.click(screen.getByRole('button', { name: /save local settings/i }))
    const banner = await screen.findByText(/a restart is required/i)
    expect(banner.closest('[role=status]')).toHaveTextContent(
      /takes effect when the instance is restarted/i,
    )
    expect(banner.closest('[role=status]')).toHaveTextContent(/log level applies at\s+once/i)
    // Persistent: still there after the data is read again.
    await waitFor(() => expect(getState().localSettings.restartRequired).toBe(true))
    // The banner offers the restart; it ends the state.
    await user.click(
      screen.getAllByRole('button', { name: /^restart instance #11$/i })[0] as HTMLElement,
    )
    const dialog = await screen.findByRole('dialog', { name: /restart instance #11/i })
    await user.click(within(dialog).getByRole('button', { name: /^restart instance$/i }))
    await within(dialog).findByText(/finished/i)
    await waitFor(() => expect(screen.queryByText(/a restart is required/i)).toBeNull())
  })

  it("the server's 422 for the quota and the log directory lands on the field", async () => {
    const writes = recordWrites()
    const user = userEvent.setup()
    await renderApp('/this-server')
    const memory = await screen.findByLabelText(/memory quota/i)
    await user.clear(memory)
    await user.type(memory, '70000')
    await user.click(screen.getByRole('button', { name: /save local settings/i }))
    expect((await screen.findAllByText(/more than the machine has/i)).length).toBeGreaterThan(0)
    expect(memory).toHaveAttribute('aria-invalid', 'true')
    expect(patches(writes)[0]?.body).toEqual({ memoryQuota: { totalMegabytes: 70000 } })
    await user.clear(memory)
    await user.type(memory, '16384')
    const dir = screen.getByLabelText(/log directory/i)
    await user.clear(dir)
    await user.type(dir, '/missing/dir')
    await user.click(screen.getByRole('button', { name: /save local settings/i }))
    expect(
      (await screen.findAllByText(/not an absolute path of an existing, writable directory/i))
        .length,
    ).toBeGreaterThan(0)
    expect(patches(writes).at(-1)?.body).toEqual({ logDirectory: '/missing/dir' })
    // Nothing changed on the server after a refusal.
    expect(getState().localSettings.dataPaths.logDirectory).toBe('/var/log/kingresdb')
  })

  it('a relative log directory is refused before anything is sent', async () => {
    const writes = recordWrites()
    const user = userEvent.setup()
    await renderApp('/this-server')
    const dir = await screen.findByLabelText(/log directory/i)
    await user.clear(dir)
    await user.type(dir, 'logs/here')
    expect(screen.getByText(/use an absolute path/i)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /save local settings/i }))
    expect(patches(writes)).toHaveLength(0)
  })

  it('says that the data directory and the ports cannot be changed, and shows a missing read-only port as n/a', async () => {
    getState().localSettings.clientPorts = { readWritePort: 5432, readOnlyPort: null }
    await renderApp('/this-server')
    await screen.findByLabelText(/memory quota/i)
    expect(document.body).toHaveTextContent(/cannot be changed after the server was created/i)
    expect(document.body).toHaveTextContent(/read-write 5432, read-only\s*n\/a/i)
    expect(screen.queryByLabelText(/data directory/i)).toBeNull()
  })

  it('an account below kingres_admin can read the settings but not save them', async () => {
    const user = userEvent.setup()
    await renderApp('/this-server', { user: 'viewer' })
    const memory = await screen.findByLabelText(/memory quota/i)
    expect(memory).toBeInTheDocument()
    const save = screen.getByRole('button', { name: /save local settings/i })
    await user.type(memory, '0')
    expect(save).toHaveAttribute('aria-disabled', 'true')
  })
})
