// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.6.0, `replaceNetworkAccess?dryRun=true` and `Session.clientAddress`: the lock-out check
// before saving network rules is the server's (a dry run that changes nothing); the panel's own address
// check is only a hint; the address shown is the one the server reports for the session.
import { afterEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { mockConfig } from '@/mocks/state'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { useSessionStore } from '@/stores/session'
import { renderApp } from './helpers'

afterEach(() => mockServer.events.removeAllListeners())

interface Seen {
  method: string
  url: string
}
function recordPuts(): Seen[] {
  const puts: Seen[] = []
  mockServer.events.on('request:start', ({ request }) => {
    if (request.method === 'PUT' && request.url.includes('/settings/network-access'))
      puts.push({ method: request.method, url: request.url })
  })
  return puts
}

async function openReview(edit: () => Promise<void> | void = () => undefined) {
  const user = userEvent.setup()
  const app = await renderApp('/security')
  await user.click(await screen.findByRole('tab', { name: 'Network access' }))
  await screen.findByLabelText('Rule 1 comment')
  await edit()
  return { user, ...app }
}

const open = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(screen.getByRole('button', { name: /review and apply/i }))
  return screen.findByRole('dialog', { name: /network access rules/i })
}

describe('dry run of the network rules (0.6.0)', () => {
  it('asks the server with dryRun=true when the review opens, and changes nothing', async () => {
    const puts = recordPuts()
    const { user } = await openReview()
    const before = { tasks: getState().tasks.length, audit: getState().audit.length }
    await user.type(screen.getByLabelText('Rule 1 comment'), ' (renamed)')
    const dialog = await open(user)
    const check = await within(dialog).findByTestId('server-check')
    await within(check).findByText(/accepts them/i)
    expect(check).toHaveTextContent(/nothing is saved yet/i)
    // One request, a dry run: no task, no audit event, no new revision, rules untouched.
    expect(puts).toHaveLength(1)
    expect(new URL(puts[0]!.url).searchParams.get('dryRun')).toBe('true')
    expect(getState().tasks.length).toBe(before.tasks)
    expect(getState().audit.length).toBe(before.audit)
    expect(getState().network.revision).toBe(7)
    expect(getState().network.rules[0]?.comment).toBe('cluster network')
    // The real change is a second, separate request, made only after the confirmation.
    await user.click(within(dialog).getByRole('button', { name: /^apply on all servers$/i }))
    await waitFor(() => expect(puts).toHaveLength(2))
    expect(new URL(puts[1]!.url).searchParams.has('dryRun')).toBe(false)
    await waitFor(() => expect(getState().network.revision).toBe(8))
  })

  it('shows the server refusing a lock-out (422) and does not let the change be applied', async () => {
    getState().network.rules[0]!.address = '203.0.113.0/24' // lets the client address in
    const puts = recordPuts()
    const { user } = await openReview()
    await user.click(screen.getByRole('button', { name: /remove rule 1/i }))
    const dialog = await open(user)
    const check = await within(dialog).findByTestId('server-check')
    await within(check).findByText(/lock out your own address/i)
    expect(within(check).getByText(/would refuse this change/i)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: /^apply on all servers$/i })).toBeDisabled()
    // Only the dry run was sent; nothing was applied.
    expect(puts).toHaveLength(1)
    expect(getState().network.revision).toBe(7)
  })

  it('explains a revision conflict (409) from the dry run', async () => {
    const { user } = await openReview()
    await user.type(screen.getByLabelText('Rule 1 comment'), '!')
    getState().network.revision = 8 // someone else saved meanwhile
    const dialog = await open(user)
    const check = await within(dialog).findByTestId('server-check')
    await within(check).findByText(/would refuse this change/i)
    expect(check).toHaveTextContent(/changed by someone else/i)
    expect(check).toHaveTextContent(/discard changes/i)
    expect(within(dialog).getByRole('button', { name: /^apply on all servers$/i })).toBeDisabled()
  })

  it('shows the field errors of a 400 from the dry run', async () => {
    const { user } = await openReview()
    await user.type(screen.getByLabelText('Rule 1 address'), 'x')
    const dialog = await open(user)
    const check = await within(dialog).findByTestId('server-check')
    await within(check).findByText(/rules\[0\]\.address/)
    expect(within(dialog).getByRole('button', { name: /^apply on all servers$/i })).toBeDisabled()
  })

  it('treats its own address check as a hint: the server accepts, so nothing blocks', async () => {
    // The client address (203.0.113.7) is allowed neither before nor after: the panel's own check
    // says "not allowed", the server's dry run says valid.
    const { user } = await openReview()
    await user.type(screen.getByLabelText('Rule 1 comment'), ' x')
    const dialog = await open(user)
    await within(dialog).findByText(/accepts them/i)
    const hint = within(dialog).getByTestId('address-hint')
    expect(hint).toHaveTextContent(/hint only/i)
    expect(hint).toHaveTextContent(/203\.0\.113\.7/)
    expect(hint).toHaveAttribute('role', 'status')
    // No typed confirmation is asked for because of the hint; the apply button is on.
    expect(within(dialog).queryByLabelText(/to confirm/i)).not.toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: /^apply on all servers$/i })).toBeEnabled()
  })

  it('does not block when the server cannot answer the dry run (503), and says so', async () => {
    mockServer.use(
      http.put('*/api/v1/settings/network-access', ({ request }) =>
        new URL(request.url).searchParams.get('dryRun') === 'true'
          ? HttpResponse.json(
              {
                type: 'about:blank',
                title: 'x',
                status: 503,
                code: 'leader_unavailable',
                requestId: 'r1',
              },
              { status: 503 },
            )
          : undefined,
      ),
    )
    const { user } = await openReview()
    await user.type(screen.getByLabelText('Rule 1 comment'), ' x')
    const dialog = await open(user)
    await within(dialog).findByText(/could not check these rules just now/i)
    // No answer is not a refusal: nothing is blocked. Without the server's word the panel's own hint
    // (the address is not let in) falls back to the old caution, the typed confirmation.
    const apply = within(dialog).getByRole('button', { name: /^apply on all servers$/i })
    expect(apply).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/to confirm/i), 'lock out')
    expect(apply).toBeEnabled()
  })

  it('the mock refuses a dryRun value other than true or false (400), like the contract', async () => {
    const { pinia } = await openReview()
    const csrf = useSessionStore(pinia).session?.csrfToken ?? ''
    const rules = getState().network
    const send = (value: string) =>
      fetch(`/api/v1/settings/network-access?dryRun=${value}`, {
        method: 'PUT',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
        body: JSON.stringify({
          revision: rules.revision,
          tlsRequired: rules.tlsRequired,
          rules: rules.rules,
        }),
      })
    expect((await send('maybe')).status).toBe(400)
    expect((await send('1')).status).toBe(400)
    const dry = await send('true')
    expect(dry.status).toBe(200)
    expect(await dry.json()).toEqual({ valid: true, revision: 7 })
    expect(getState().network.revision).toBe(7)
  })

  it('the endpoint function returns the check and raises the same problem as a real change', async () => {
    const { pinia } = await openReview()
    expect(useSessionStore(pinia).session?.username).toBe('admin')
    const rules = getState().network
    const answer = await endpoints.checkNetworkAccess({
      revision: rules.revision,
      tlsRequired: rules.tlsRequired,
      rules: rules.rules,
    })
    expect(answer).toEqual({ valid: true, revision: 7 })
    const stale = endpoints.checkNetworkAccess({
      revision: 3,
      tlsRequired: false,
      rules: rules.rules,
    })
    await expect(stale).rejects.toBeInstanceOf(ApiError)
    await expect(stale).rejects.toMatchObject({ status: 409 })
  })
})

describe('the address of the account (Session.clientAddress, 0.6.0)', () => {
  it('is the one the session reports, and the dialog says so without asking the audit log', async () => {
    const seen: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      seen.push(`${request.method} ${new URL(request.url).pathname}${new URL(request.url).search}`)
    })
    getState().network.rules[0]!.address = '203.0.113.0/24'
    const { user, pinia } = await openReview()
    expect(useSessionStore(pinia).session?.clientAddress).toBe('203.0.113.7')
    await user.click(screen.getByRole('button', { name: /remove rule 2/i }))
    const dialog = await open(user)
    await within(dialog).findByText(/accepts them/i)
    // Rule 1 still lets the address in: no warning, and the address is not looked up in the audit log.
    expect(within(dialog).queryByTestId('address-hint')).not.toBeInTheDocument()
    expect(seen.some((r) => r.startsWith('GET /api/v1/audit'))).toBe(false)
  })

  it('falls back to the last panel address in the audit log when the server does not report one', async () => {
    mockConfig.clientAddress = null
    getState().audit.unshift({
      id: 900,
      timestamp: new Date().toISOString(),
      actor: 'admin',
      actorAddress: '10.0.0.50',
      source: 'panel',
      action: 'login',
      outcome: 'success',
    })
    const { user } = await openReview()
    await user.click(screen.getByRole('button', { name: /remove rule 1/i }))
    const dialog = await open(user)
    expect(await within(dialog).findByTestId('address-hint')).toHaveTextContent(
      /no longer allow.*10\.0\.0\.50/i,
    )
  })

  it('says it cannot tell when neither the session nor the audit log has an address', async () => {
    mockConfig.clientAddress = null
    getState().audit = []
    const { user } = await openReview()
    await user.type(screen.getByLabelText('Rule 1 comment'), 'x')
    const dialog = await open(user)
    expect(
      await within(dialog).findByText(/cannot tell which address you connect from/i),
    ).toBeInTheDocument()
  })
})
