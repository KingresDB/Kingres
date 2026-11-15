// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.5.0: renewing and revoking the certificates of a server from its detail page. Renewal has
// an ordinary confirmation and follows the task. Revocation is severe: the dialog says what happens
// (the words of the contract and of the control plane README), asks for the server id, and has nothing
// pre-ticked. A revoked server is marked with words and an icon (not by colour alone) in the list and on
// its page, and the actions that cannot work for it say why.
import { afterEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { renderApp } from './helpers'

afterEach(() => mockServer.events.removeAllListeners())

function trackRequests(): string[] {
  const seen: string[] = []
  mockServer.events.on('request:start', ({ request }) => {
    seen.push(`${request.method} ${new URL(request.url).pathname.replace('/api/v1', '')}`)
  })
  return seen
}

type User = ReturnType<typeof userEvent.setup>

async function openRevoke(user: User) {
  await user.click(await screen.findByRole('button', { name: /^revoke certificates$/i }))
  return screen.findByRole('dialog', { name: /revoke certificates of server/i })
}

describe('renew now', () => {
  it('has an ordinary confirmation (nothing to type), follows the task and shows the new end date', async () => {
    const user = userEvent.setup()
    const seen = trackRequests()
    await renderApp('/servers/10.0.0.13')
    const before = getState().servers.find((s) => s.id === '10.0.0.13')!.certificateExpiresAt
    await user.click(await screen.findByRole('button', { name: /^renew now$/i }))
    const dialog = await screen.findByRole('dialog', { name: /renew certificates of server/i })
    expect(within(dialog).queryByRole('textbox')).toBeNull()
    expect(dialog).toHaveTextContent(/nothing is restarted/i)
    await user.click(within(dialog).getByRole('button', { name: /^renew now$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    expect(seen).toContain('POST /servers/10.0.0.13/certificates/renewals')
    expect(getState().servers.find((s) => s.id === '10.0.0.13')!.certificateExpiresAt).not.toBe(
      before,
    )
    // The task is a certificate.renew task for that server.
    const task = getState().tasks.find((t) => t.task.kind === 'certificate.renew')
    expect(task?.task.subject).toEqual({ type: 'server', id: '10.0.0.13' })
  })

  it('explains a refusal because the certificates are revoked, in plain words', async () => {
    const user = userEvent.setup()
    await renderApp('/servers/10.0.0.13')
    await user.click(await screen.findByRole('button', { name: /^renew now$/i }))
    const dialog = await screen.findByRole('dialog', { name: /renew certificates of server/i })
    // Revoked by someone else after this page was drawn.
    getState().servers.find((s) => s.id === '10.0.0.13')!.certificateRevokedAt =
      new Date().toISOString()
    await user.click(within(dialog).getByRole('button', { name: /^renew now$/i }))
    const notice = await within(dialog).findByRole('alert')
    expect(notice).toHaveTextContent(/certificates revoked/i)
    expect(notice).toHaveTextContent(/join it again/i)
    expect(notice).not.toHaveTextContent(/certificate_revoked/)
  })

  it('explains that the manager leader does not hold the CA key, and that nothing was changed', async () => {
    const user = userEvent.setup()
    mockConfig.caUnavailable = true
    await renderApp('/servers/10.0.0.13')
    await user.click(await screen.findByRole('button', { name: /^renew now$/i }))
    const dialog = await screen.findByRole('dialog', { name: /renew certificates of server/i })
    await user.click(within(dialog).getByRole('button', { name: /^renew now$/i }))
    const notice = await within(dialog).findByRole('alert')
    expect(notice).toHaveTextContent(/certificate authority not available/i)
    expect(notice).toHaveTextContent(/manager leader does not hold the key/i)
    expect(notice).toHaveTextContent(/nothing was changed/i)
    expect(notice).toHaveTextContent(/try again/i)
    expect(notice).not.toHaveTextContent(/certificate_authority_unavailable/)
  })
})

describe('revoke certificates', () => {
  it('says exactly what happens, before anything is sent', async () => {
    const user = userEvent.setup()
    const seen = trackRequests()
    await renderApp('/servers/10.0.0.13')
    const dialog = await openRevoke(user)
    // What the contract and the control plane README say.
    expect(dialog).toHaveTextContent(/cannot be undone/i)
    expect(dialog).toHaveTextContent(/every other server and every database instance refuses/i)
    expect(dialog).toHaveTextContent(/in both directions/i)
    expect(dialog).toHaveTextContent(/open now keep running until they end/i)
    expect(dialog).toHaveTextContent(/never gets a new certificate/i)
    expect(dialog).toHaveTextContent(/remove the server and join it again with a new setup token/i)
    expect(dialog).toHaveTextContent(/only for a server that holds no bucket copy/i)
    expect(dialog).toHaveTextContent(/certificates of the manager leader cannot be revoked/i)
    expect(dialog).toHaveTextContent(/waits for a server that is down/i)
    // This server holds buckets: the numbers are shown, and it is called a one-way door.
    expect(dialog).toHaveTextContent(/one-way door/i)
    expect(dialog).toHaveTextContent(/holds\s*\d+\s*primary and\s*\d+\s*replica buckets/i)
    expect(seen.filter((r) => r.includes('certificates'))).toEqual([])
  })

  it('asks for the server id, with nothing pre-ticked, and only then sends', async () => {
    const user = userEvent.setup()
    const seen = trackRequests()
    await renderApp('/servers/10.0.0.13')
    const dialog = await openRevoke(user)
    // Nothing is ticked or selected in advance.
    expect(
      dialog.querySelectorAll('input[type="checkbox"]:checked, input[type="radio"]:checked'),
    ).toHaveLength(0)
    const confirm = within(dialog).getByRole('button', { name: /^revoke certificates$/i })
    expect(confirm).toBeDisabled()
    const box = within(dialog).getByRole('textbox')
    // The address is not the id: it does not enable the button.
    await user.type(box, '10.0.0.13:8091')
    expect(confirm).toBeDisabled()
    await user.clear(box)
    await user.type(box, '10.0.0.13')
    expect(confirm).toBeEnabled()
    await user.click(confirm)
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    expect(seen).toContain('POST /servers/10.0.0.13/certificates/revocations')
    const task = getState().tasks.find((t) => t.task.kind === 'certificate.revoke')
    expect(task?.task.subject).toEqual({ type: 'server', id: '10.0.0.13' })
  })

  it('marks the revoked server with words and an icon on its page, and says why renew and revoke are not possible', async () => {
    const user = userEvent.setup()
    await renderApp('/servers/10.0.0.13')
    const dialog = await openRevoke(user)
    await user.type(within(dialog).getByRole('textbox'), '10.0.0.13')
    await user.click(within(dialog).getByRole('button', { name: /^revoke certificates$/i }))
    await within(dialog).findByText('Finished.')
    await user.click(within(dialog).getByRole('button', { name: /^close$/i }))

    const notice = await screen.findByTestId('revoked-notice')
    expect(notice).toHaveTextContent(/certificates of this server are revoked/i)
    expect(notice).toHaveTextContent(/remove the server and join it again/i)
    const badge = screen.getByTestId('revoked-badge')
    expect(badge).toHaveTextContent('Certificates revoked')
    // Not colour alone: an icon that screen readers skip, and the words.
    expect(badge.querySelector('[aria-hidden="true"]')).not.toBeNull()

    const renew = screen.getByRole('button', { name: /^renew now/i })
    const revoke = screen.getByRole('button', { name: /^revoke certificates/i })
    expect(renew).toHaveAttribute('aria-disabled', 'true')
    expect(revoke).toHaveAttribute('aria-disabled', 'true')
    expect(screen.getByTestId('renew-blocked')).toHaveTextContent(/gets no new certificate/i)
    expect(screen.getByTestId('revoke-blocked')).toHaveTextContent(/already revoked/i)
    // A disabled action does not open anything.
    await user.click(renew)
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('marks a revoked server in the list in words and with an icon', async () => {
    getState().servers.find((s) => s.id === '10.0.0.12')!.certificateRevokedAt =
      '2026-11-14T10:00:00.000Z'
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.12')
    const badge = within(row).getByTestId('revoked-badge')
    expect(badge).toHaveTextContent('Certificates revoked')
    expect(badge.querySelector('[aria-hidden="true"]')).not.toBeNull()
    expect(row).toHaveTextContent(/revoked 14 nov 2026/i)
    expect(
      within(screen.getByTestId('server-row-10.0.0.13')).queryByTestId('revoked-badge'),
    ).toBeNull()
  })

  it('cannot be started for the manager leader: the button says why', async () => {
    const user = userEvent.setup()
    await renderApp('/servers/10.0.0.11')
    const revoke = await screen.findByRole('button', { name: /^revoke certificates/i })
    expect(revoke).toHaveAttribute('aria-disabled', 'true')
    expect(screen.getByTestId('revoke-blocked')).toHaveTextContent(
      /certificates of the manager leader cannot be revoked/i,
    )
    // Renewing the leader's own certificates is fine.
    expect(screen.getByRole('button', { name: /^renew now/i })).not.toHaveAttribute('aria-disabled')
    await user.click(revoke)
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('shows the answer for a server that hosts the cluster metadata (409) in plain words', async () => {
    const user = userEvent.setup()
    mockServer.use(
      http.post('*/api/v1/servers/:id/certificates/revocations', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'conflict',
            status: 409,
            code: 'server_hosts_map_group',
            detail: 'The leader cannot revoke its own certificates.',
          },
          { status: 409, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    )
    await renderApp('/servers/10.0.0.13')
    const dialog = await openRevoke(user)
    await user.type(within(dialog).getByRole('textbox'), '10.0.0.13')
    await user.click(within(dialog).getByRole('button', { name: /^revoke certificates$/i }))
    const notice = await within(dialog).findByRole('alert')
    expect(notice).toHaveTextContent(/server hosts the cluster metadata/i)
    expect(notice).not.toHaveTextContent(/server_hosts_map_group/)
  })

  it('explains that the manager leader has no CA key and that nothing was revoked', async () => {
    const user = userEvent.setup()
    mockConfig.caUnavailable = true
    await renderApp('/servers/10.0.0.13')
    const dialog = await openRevoke(user)
    await user.type(within(dialog).getByRole('textbox'), '10.0.0.13')
    await user.click(within(dialog).getByRole('button', { name: /^revoke certificates$/i }))
    const notice = await within(dialog).findByRole('alert')
    expect(notice).toHaveTextContent(/certificate authority not available/i)
    expect(notice).toHaveTextContent(/nothing was changed/i)
    expect(getState().servers.find((s) => s.id === '10.0.0.13')!.certificateRevokedAt).toBeFalsy()
  })

  it('the remove dialog of a revoked server says that removing it is the way back, and the condition', async () => {
    const user = userEvent.setup()
    getState().servers.find((s) => s.id === '10.0.0.13')!.certificateRevokedAt =
      new Date().toISOString()
    await renderApp('/servers/10.0.0.13')
    await user.click(await screen.findByRole('button', { name: /^remove$/i }))
    const dialog = await screen.findByRole('dialog', { name: /remove server/i })
    expect(dialog).toHaveTextContent(/certificates of this server are revoked/i)
    expect(dialog).toHaveTextContent(/only way back/i)
    expect(dialog).toHaveTextContent(/holds no bucket copy/i)
  })
})

describe('roles', () => {
  it('both certificate actions need kingres_admin: an ops account sees them disabled with the reason', async () => {
    await renderApp('/servers/10.0.0.13', { user: 'ops' })
    const renew = await screen.findByRole('button', { name: /^renew now/i })
    const revoke = screen.getByRole('button', { name: /^revoke certificates/i })
    for (const b of [renew, revoke]) {
      expect(b).toHaveAttribute('aria-disabled', 'true')
      expect(b).toHaveTextContent(/requires the kingres_admin cluster role/i)
    }
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })
})
