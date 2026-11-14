// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-01: replacing the network access rules goes through a confirmation that shows what changes
// and warns about lock-out, and never happens by pressing Enter in a field.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { renderApp } from './helpers'

function recordPuts(): { body: unknown }[] {
  const puts: { body: unknown }[] = []
  mockServer.events.on('request:start', async ({ request }) => {
    if (request.method === 'PUT' && request.url.endsWith('/settings/network-access')) {
      puts.push({ body: await request.clone().json() })
    }
  })
  return puts
}

function knownAddress(address: string | null): void {
  const s = getState()
  s.audit = s.audit.filter((e) => e.actor !== 'admin')
  if (address) {
    s.audit.unshift({
      id: 900,
      timestamp: new Date().toISOString(),
      actor: 'admin',
      actorAddress: address,
      source: 'panel',
      action: 'login',
      outcome: 'success',
    })
  }
}

async function openNetwork() {
  const user = userEvent.setup()
  const app = await renderApp('/security')
  await user.click(await screen.findByRole('tab', { name: 'Network access' }))
  await screen.findByLabelText('Rule 1 comment')
  return { user, ...app }
}

describe('network access: no implicit submission (W-01)', () => {
  it('pressing Enter in a field of the editor changes nothing and opens nothing', async () => {
    knownAddress('10.0.0.50')
    const puts = recordPuts()
    const { user } = await openNetwork()
    await user.type(screen.getByLabelText('Rule 1 comment'), ' edited{Enter}')
    await user.type(screen.getByLabelText('Rule 1 address'), '{Enter}')
    await new Promise((r) => setTimeout(r, 100))
    expect(puts).toHaveLength(0)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(getState().network.revision).toBe(7)
  })

  it('applies only from the confirmation dialog, which shows what changes', async () => {
    knownAddress('10.0.0.50')
    const puts = recordPuts()
    const { user } = await openNetwork()
    await user.type(screen.getByLabelText('Rule 1 comment'), ' (renamed)')
    await user.click(screen.getByRole('button', { name: /review and apply/i }))
    const dialog = await screen.findByRole('dialog', { name: /network access rules/i })
    expect(puts).toHaveLength(0)
    expect(within(dialog).getByText(/1 rule changed/i)).toBeInTheDocument()
    expect(within(dialog).getByText(/cluster network \(renamed\)/)).toBeInTheDocument()
    // Nothing risky changed: the button is enabled without typing anything.
    expect(within(dialog).queryByLabelText(/to confirm/i)).not.toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: /^apply on all servers$/i }))
    await waitFor(() => expect(puts).toHaveLength(1))
    await waitFor(() =>
      expect(getState().network.rules[0]?.comment).toBe('cluster network (renamed)'),
    )
  })

  it('does not alter names it did not edit (a name with a comma survives a save)', async () => {
    knownAddress('10.0.0.50')
    getState().network.rules[1]!.databases = ['shop,eu', 'say "hi"']
    const puts = recordPuts()
    const { user } = await openNetwork()
    await user.type(screen.getByLabelText('Rule 1 comment'), '!')
    await user.click(screen.getByRole('button', { name: /review and apply/i }))
    const dialog = await screen.findByRole('dialog', { name: /network access rules/i })
    await user.click(within(dialog).getByRole('button', { name: /^apply on all servers$/i }))
    await waitFor(() => expect(puts).toHaveLength(1))
    expect(getState().network.rules[1]?.databases).toEqual(['shop,eu', 'say "hi"'])
  })
})

describe('network access: lock-out warnings (W-01)', () => {
  it('warns that the address of the administrator is no longer allowed, and asks to type to confirm', async () => {
    knownAddress('10.0.0.50')
    const { user } = await openNetwork()
    await user.click(screen.getByRole('button', { name: /remove rule 1/i }))
    await user.click(screen.getByRole('button', { name: /review and apply/i }))
    const dialog = await screen.findByRole('dialog', { name: /network access rules/i })
    expect(within(dialog).getByText(/1 rule removed/i)).toBeInTheDocument()
    expect(dialog).toHaveTextContent(/no longer allow.*10.0.0.50/i)
    const apply = within(dialog).getByRole('button', { name: /^apply on all servers$/i })
    expect(apply).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/to confirm/i), 'lock out')
    expect(apply).toBeEnabled()
  })

  it('warns when no rule allows administrators and when the list is empty', async () => {
    knownAddress('10.0.0.50')
    const { user } = await openNetwork()
    await user.click(screen.getByRole('button', { name: /remove rule 1/i }))
    await user.click(screen.getByRole('button', { name: /remove rule 1/i }))
    await user.click(screen.getByRole('button', { name: /review and apply/i }))
    const dialog = await screen.findByRole('dialog', { name: /network access rules/i })
    expect(within(dialog).getByText(/nobody will be able to connect/i)).toBeInTheDocument()
    expect(within(dialog).getByText(/no rule that allows administrators/i)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: /^apply on all servers$/i })).toBeDisabled()
  })

  it('says what it cannot know when the address of the administrator is not known', async () => {
    knownAddress(null)
    const { user } = await openNetwork()
    await user.type(screen.getByLabelText('Rule 1 comment'), 'x')
    await user.click(screen.getByRole('button', { name: /review and apply/i }))
    const dialog = await screen.findByRole('dialog', { name: /network access rules/i })
    expect(
      within(dialog).getByText(/cannot tell which address you connect from/i),
    ).toBeInTheDocument()
  })

  it('does not warn when the address is still allowed after the change', async () => {
    knownAddress('10.0.0.50')
    const { user } = await openNetwork()
    await user.click(screen.getByRole('button', { name: /remove rule 2/i }))
    await user.click(screen.getByRole('button', { name: /review and apply/i }))
    const dialog = await screen.findByRole('dialog', { name: /network access rules/i })
    expect(within(dialog).queryByText(/no longer allow/i)).not.toBeInTheDocument()
    expect(within(dialog).queryByLabelText(/to confirm/i)).not.toBeInTheDocument()
  })
})
