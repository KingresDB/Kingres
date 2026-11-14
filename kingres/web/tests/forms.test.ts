// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-01 (audit of every form), W-07 and W-11 (fields that are re-encoded on save), and stale dialog
// state: no destructive or cluster-wide action is submitted by Enter in a field, and what was not
// edited is not sent.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState, resetMockState } from '@/mocks/http'
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
const settle = () => new Promise((r) => setTimeout(r, 120))

describe('Enter in a field does not save cluster settings', () => {
  it('the settings page', async () => {
    const writes = recordWrites()
    const user = userEvent.setup()
    await renderApp('/settings')
    const replicas = await screen.findByLabelText(/default replica count/i)
    await user.clear(replicas)
    await user.type(replicas, '2{Enter}')
    await settle()
    expect(writes).toHaveLength(0)
    expect(getState().settings.defaultReplicaCount).toBe(1)
  })

  it('turning on a feature, which cannot be undone, is confirmed first', async () => {
    getState().settings.features.ledger = false
    const writes = recordWrites()
    const user = userEvent.setup()
    await renderApp('/settings')
    await user.click(await screen.findByRole('checkbox', { name: /ledger/i }))
    await user.click(screen.getByRole('button', { name: /save settings/i }))
    const dialog = await screen.findByRole('dialog', { name: /turn on/i })
    expect(dialog).toHaveTextContent(/cannot be turned off again/i)
    expect(writes).toHaveLength(0)
    await user.click(within(dialog).getByRole('button', { name: /^turn on permanently$/i }))
    await waitFor(() => expect(writes.some((w) => w.method === 'PATCH')).toBe(true))
    expect(writes.find((w) => w.method === 'PATCH')?.body).toEqual({ features: { ledger: true } })
  })
})

describe('this server', () => {
  it('Enter in the memory field saves nothing, and only the changed field is sent', async () => {
    const writes = recordWrites()
    const user = userEvent.setup()
    await renderApp('/this-server')
    const memory = await screen.findByLabelText(/memory quota/i)
    await user.clear(memory)
    await user.type(memory, '8192{Enter}')
    await settle()
    expect(writes).toHaveLength(0)
    await user.click(screen.getByRole('button', { name: /save local settings/i }))
    await waitFor(() => expect(writes.some((w) => w.method === 'PATCH')).toBe(true))
    expect(writes.find((w) => w.method === 'PATCH')?.body).toEqual({
      memoryQuota: { totalMegabytes: 8192 },
    })
  })

  it('"force" ticked for one action is not still ticked for the next dialog', async () => {
    const user = userEvent.setup()
    await renderApp('/this-server')
    const rows = await screen.findAllByRole('button', { name: /^restart$/i })
    await user.click(rows[0] as HTMLElement)
    let dialog = await screen.findByRole('dialog', { name: /restart instance/i })
    await user.click(within(dialog).getByRole('checkbox', { name: /force/i }))
    await user.click(within(dialog).getByRole('button', { name: /^cancel$/i }))
    await user.click(screen.getByRole('button', { name: /leave cluster/i }))
    dialog = await screen.findByRole('dialog', { name: /leave the cluster/i })
    expect(within(dialog).getByRole('checkbox', { name: /force/i })).not.toBeChecked()
  })
})

describe('the setup wizard', () => {
  it('Enter in a field of the create form does not create the cluster', async () => {
    resetMockState({ uninitialized: true })
    const writes = recordWrites()
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await user.type(await screen.findByLabelText(/setup token/i), 'demo-setup-token')
    await user.click(screen.getByRole('button', { name: /continue/i }))
    await user.click(await screen.findByRole('button', { name: /^create new cluster$/i }))
    await user.type(await screen.findByLabelText(/cluster name/i), 'prod-eu')
    // Everything the form needs is filled in, so that nothing but the Enter key is missing.
    await user.type(screen.getByLabelText(/^password/i), 'a-long-enough-password')
    await user.type(screen.getByLabelText(/repeat password/i), 'a-long-enough-password{Enter}')
    await settle()
    expect(writes.filter((w) => w.path.endsWith('/setup/init-cluster'))).toHaveLength(0)
  })
})

describe('the ledger proof form', () => {
  it('refuses a number that cannot be sent exactly, instead of sending another number', async () => {
    const writes = recordWrites()
    const user = userEvent.setup()
    await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    const key = screen.getByLabelText(/primary key/i)
    await user.clear(key)
    await user.click(key)
    await user.paste('{"id": 9007199254740993}')
    await user.click(screen.getByRole('button', { name: /look up proof/i }))
    expect(await screen.findByText(/larger than 2\^53/i)).toBeInTheDocument()
    expect(writes.filter((w) => w.path.endsWith('/inclusion-proofs'))).toHaveLength(0)
  })
})

describe('a refresh from the server does not overwrite what the person is editing', () => {
  it('the settings page keeps an unsaved value', async () => {
    const user = userEvent.setup()
    const { queryClient } = await renderApp('/settings')
    const replicas = (await screen.findByLabelText(/default replica count/i)) as HTMLInputElement
    await user.clear(replicas)
    await user.type(replicas, '2')
    // Someone else changes another setting: the page gets new data while the field is being edited.
    getState().settings.autoFailover.timeoutSeconds = 99
    await queryClient.invalidateQueries({ queryKey: ['clusterSettings'] })
    await settle()
    expect(replicas.value).toBe('2')
  })

  it('the server page keeps the group that was chosen but not saved', async () => {
    const user = userEvent.setup()
    const { queryClient } = await renderApp('/servers/10.0.0.11')
    const group = (await screen.findByLabelText('Group')) as HTMLSelectElement
    await waitFor(() => expect(group.value).toBe('rack-a'))
    await user.selectOptions(group, 'rack-b')
    // The server's own group changes while the choice is not saved yet.
    getState().servers[0]!.serverGroup = null
    await queryClient.invalidateQueries({ queryKey: ['server', '10.0.0.11'] })
    await settle()
    expect(group.value).toBe('rack-b')
  })
})
