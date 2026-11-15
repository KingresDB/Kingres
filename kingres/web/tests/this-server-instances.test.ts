// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Start, stop and restart of the local instances (contract 0.4.0): the buttons follow the state
// of the instance, each refusal is explained in terms of what to do next, and "force" is a
// consequence the administrator has to accept.
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

// Instances of the local server in the mock: #11 primary of the map group, #111 primary of
// group 1 (its standby lags), #311 standby of group 3.
const LAGGING = 111
const STANDBY = 311

async function rowOf(id: number): Promise<HTMLElement> {
  const header = await screen.findByRole('rowheader', { name: `#${id}` })
  return header.closest('tr') as HTMLElement
}

describe('instance buttons follow the state of the instance', () => {
  it('a running instance offers Restart and Stop; a stopped one offers Start; one in transition offers nothing', async () => {
    getState().instanceStates.set(STANDBY, 'stopped')
    getState().instanceStates.set(LAGGING, 'restarting')
    await renderApp('/this-server')
    const stopped = await rowOf(STANDBY)
    expect(within(stopped).getByRole('button', { name: /^start$/i })).toBeInTheDocument()
    expect(within(stopped).queryByRole('button', { name: /^stop$/i })).toBeNull()
    const moving = await rowOf(LAGGING)
    expect(within(moving).queryByRole('button')).toBeNull()
    expect(moving).toHaveTextContent(/restarting/i)
    const running = await rowOf(11)
    expect(within(running).getByRole('button', { name: /^restart$/i })).toBeInTheDocument()
    expect(within(running).getByRole('button', { name: /^stop$/i })).toBeInTheDocument()
  })
})

describe('refusals of stop and restart are explained', () => {
  it('the only up-to-date copy: explained, and force is sent only after the consequence is accepted', async () => {
    const writes = recordWrites()
    const user = userEvent.setup()
    await renderApp('/this-server')
    const row = await rowOf(LAGGING)
    await user.click(within(row).getByRole('button', { name: /^stop$/i }))
    const dialog = await screen.findByRole('dialog', { name: /stop instance/i })
    await user.click(within(dialog).getByRole('button', { name: /^stop instance$/i }))
    await within(dialog).findByText(/no replica available/i)
    expect(dialog).toHaveTextContent(/only up-to-date copy of its shard group/i)
    expect(dialog).toHaveTextContent(/wait until the standby has caught up/i)
    expect(writes.find((w) => w.path.endsWith('/stop'))?.body).toEqual({ force: false })
    const force = within(dialog).getByRole('checkbox', { name: /force/i })
    expect(force).not.toBeChecked()
    expect(dialog).toHaveTextContent(/can be lost/i)
    await user.click(force)
    await user.click(within(dialog).getByRole('button', { name: /^stop instance$/i }))
    await waitFor(() => expect(writes.filter((w) => w.path.endsWith('/stop'))).toHaveLength(2))
    expect(writes.filter((w) => w.path.endsWith('/stop'))[1]?.body).toEqual({ force: true })
    await within(dialog).findByText(/finished/i)
  })

  it('"already stopped" says there is nothing to stop and what to do instead', async () => {
    const user = userEvent.setup()
    await renderApp('/this-server')
    const row = await rowOf(STANDBY)
    await user.click(within(row).getByRole('button', { name: /^stop$/i }))
    const dialog = await screen.findByRole('dialog', { name: /stop instance/i })
    // Someone else stopped it while the dialog was open.
    getState().instanceStates.set(STANDBY, 'stopped')
    await user.click(within(dialog).getByRole('button', { name: /^stop instance$/i }))
    await within(dialog).findByText(/instance already stopped/i)
    expect(dialog).toHaveTextContent(/start it first if you need it running/i)
  })

  it('"operation in progress" says to wait for the other operation', async () => {
    const user = userEvent.setup()
    await renderApp('/this-server')
    const row = await rowOf(STANDBY)
    await user.click(within(row).getByRole('button', { name: /^restart$/i }))
    const dialog = await screen.findByRole('dialog', { name: /restart instance/i })
    getState().instanceStates.set(STANDBY, 'restarting')
    await user.click(within(dialog).getByRole('button', { name: /^restart instance$/i }))
    await within(dialog).findByText(/operation in progress/i)
    expect(dialog).toHaveTextContent(/wait until it has finished/i)
  })

  it('"already running" is explained for start', async () => {
    getState().instanceStates.set(STANDBY, 'stopped')
    const user = userEvent.setup()
    await renderApp('/this-server')
    const row = await rowOf(STANDBY)
    await user.click(within(row).getByRole('button', { name: /^start$/i }))
    const dialog = await screen.findByRole('dialog', { name: /start instance/i })
    // The instance came up on its own (an operator started it on the machine).
    getState().instanceStates.set(STANDBY, 'running')
    await user.click(within(dialog).getByRole('button', { name: /^start instance$/i }))
    await within(dialog).findByText(/instance already running/i)
    expect(dialog).toHaveTextContent(/nothing to start/i)
  })

  it('stopping the instance of the system database says what it does and asks for typing', async () => {
    const user = userEvent.setup()
    await renderApp('/this-server')
    const row = await rowOf(11)
    await user.click(within(row).getByRole('button', { name: /^stop$/i }))
    const dialog = await screen.findByRole('dialog', { name: /stop instance #11/i })
    expect(dialog).toHaveTextContent(/hosts the cluster system database/i)
    const confirm = within(dialog).getByRole('button', { name: /^stop instance$/i })
    expect(confirm).toBeDisabled()
    await user.type(within(dialog).getByRole('textbox'), 'stop')
    expect(confirm).toBeEnabled()
  })
})
