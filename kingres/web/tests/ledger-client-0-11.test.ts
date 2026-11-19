// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The Client keys tab of the Ledger page (Console C, contract 0.11.0, design 3.6 to 3.8 and 8.5): every state of the list of keys,
// registering a public key (never a private one), revoking, the signature policy of a table (what it waits for and cannot undo,
// said before the click) and each refusal in words, the upgrade of the extension, what each role sees and does, hostile text, axe,
// and the words and checks of `lib/ledger-client.ts`.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { axe } from 'vitest-axe'
import { createPinia } from 'pinia'
import { mockServer } from '@/mocks/server'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { seedHostile } from '@/mocks/hostile'
import {
  clientRefusal,
  keyRole,
  keyState,
  registerBody,
  revocationText,
  shortKey,
  validateRegister,
} from '@/lib/ledger-client'
import { ok } from './schema-check'
import { renderApp, signInAs } from './helpers'

const sw = (q: string) => applySwitches(new URLSearchParams(q), getState())
type User = ReturnType<typeof userEvent.setup>

const GOOD_KEY = 'q83vEjRWeJASNFZ4kBI0VniQEjRWeJASNFZ4kBI0Vng='
const IDENTITY = 'AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='

async function open(user = 'admin', path = '/ledger?tab=clients&db=shop') {
  const app = await renderApp(path, { user })
  await screen.findByTestId('client-keys')
  return app
}
const keysCard = () => screen.getByTestId('client-keys')
const tablesCard = () => screen.getByTestId('client-tables')
const dialog = () => screen.findByRole('dialog')
const ledgerTable = () =>
  getState()
    .databases.find((d) => d.info.name === 'shop')!
    .tables.find((t) => t.ledger)!
const tableLabel = () => `${ledgerTable().schema}.${ledgerTable().name}`

async function openRegister(user: User) {
  await user.click(await screen.findByRole('button', { name: /^Register a key/ }))
  return within(await dialog())
}
async function fill(user: User, d: ReturnType<typeof within>, label: RegExp, text: string) {
  const el = d.getByLabelText(label)
  await user.clear(el)
  await user.type(el, text)
}

describe('the states of the list of keys', () => {
  it('no key: an empty state that says what to do; the explanation of what a client key is', async () => {
    await open()
    expect(await within(keysCard()).findByText('No client key is registered')).toBeInTheDocument()
    expect(keysCard()).toHaveTextContent(/never signs for a client and never sees a private key/i)
    expect(screen.getByTestId('attestation-limits')).toHaveTextContent(/recompute the row hash/i)
  })
  it('keys: label, id, role, registrar, active and revoked with the reason; Revoke only for an active key', async () => {
    sw('clientkeys=keys')
    await open()
    const rows = await within(keysCard()).findAllByTestId(/^client-key-[0-9a-f]{8}$/)
    expect(rows).toHaveLength(3)
    expect(rows[0]).toHaveTextContent('orders service')
    expect(rows[0]).toHaveTextContent('Any role')
    expect(rows[1]).toHaveTextContent('kingres_ops')
    expect(within(rows[0]!).getByTestId('client-key-state')).toHaveTextContent('Active')
    expect(within(rows[2]!).getByTestId('client-key-state')).toHaveTextContent(
      /Revoked .*The laptop that held the key was lost\./,
    )
    expect(within(rows[0]!).getByRole('button', { name: /^Revoke key/ })).toBeInTheDocument()
    expect(within(rows[2]!).queryByRole('button', { name: /^Revoke key/ })).not.toBeInTheDocument()
  })
  it('loading, then an error with a retry', async () => {
    const user = userEvent.setup()
    let failing = true
    mockServer.use(
      http.get('*/api/v1/databases/:db/ledger/client-keys', () =>
        failing
          ? HttpResponse.json(
              { type: 'about:blank', title: 'x', status: 500, code: 'internal_error', detail: 'd' },
              { status: 500 },
            )
          : undefined,
      ),
    )
    await open()
    const notice = await within(keysCard()).findByRole('alert')
    expect(notice).toHaveAttribute('data-code', 'internal_error')
    failing = false
    await user.click(within(notice).getByRole('button', { name: /retry|try again/i }))
    expect(await within(keysCard()).findByText('No client key is registered')).toBeInTheDocument()
  })
  it('a server that does not build the operations yet: the calm notice in the keys, the tables still listed, every button kept', async () => {
    sw('phase7=today')
    await open()
    const notice = (
      await within(keysCard()).findByText(/not available on this server version yet/i)
    ).closest('[data-code]')
    expect(notice).toHaveAttribute('data-code', 'unsupported_operation')
    expect(screen.getByRole('button', { name: /^Register a key/ })).toBeInTheDocument()
    expect(await within(tablesCard()).findAllByTestId(/^ledger-table-/)).not.toHaveLength(0)
  })
  it('an extension older than 0.3: the keys say so in words', async () => {
    sw('clientkeys=legacy')
    await open()
    expect(
      (await within(keysCard()).findAllByText(/older than version 0[.]3|older than 0[.]3/i)).length,
    ).toBeGreaterThan(0)
  })
  it('no database uses the ledger: the tab says so', async () => {
    for (const d of getState().databases)
      d.info.ledger = { mode: 'off', digestIntervalSeconds: null }
    await renderApp('/ledger?tab=clients')
    expect(await screen.findByTestId('client-no-database')).toBeInTheDocument()
  })
})

describe('registering a key', () => {
  it('there is no field for a private key, and the text says where the key pair is made', async () => {
    const user = userEvent.setup()
    await open()
    const d = await openRegister(user)
    expect(d.queryByLabelText(/private|secret|seed|passphrase/i)).not.toBeInTheDocument()
    expect(d.getByText(/Never paste a private key here/)).toBeInTheDocument()
    expect(await dialog()).toHaveTextContent(
      /the server never signs for a client|never sees a private key|register only the public key/i,
    )
  })
  it('a form with a problem is not sent; a good one registers the key, with no role when none is given', async () => {
    const user = userEvent.setup()
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.post('*/api/v1/databases/:db/ledger/client-keys', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    await open()
    const d = await openRegister(user)
    await user.click(d.getByRole('button', { name: 'Register the key' }))
    expect(await d.findByText(/44 characters ending with/)).toBeInTheDocument()
    expect(d.getByText('Give a label of 1 to 100 characters.')).toBeInTheDocument()
    expect(bodies).toHaveLength(0)
    await fill(user, d, /^public key/i, GOOD_KEY)
    await fill(user, d, /^label/i, 'orders service')
    await user.click(d.getByRole('button', { name: 'Register the key' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(bodies[0]).toEqual({ publicKey: GOOD_KEY, label: 'orders service' })
    expect(await screen.findByTestId('client-notice')).toHaveTextContent(
      /The key [0-9a-f]{8} was registered/,
    )
    expect(await within(keysCard()).findByText('orders service')).toBeInTheDocument()
  })
  it('a role is sent when given; a role that does not exist is told at the field', async () => {
    const user = userEvent.setup()
    await open()
    const d = await openRegister(user)
    await fill(user, d, /^public key/i, GOOD_KEY)
    await fill(user, d, /^label/i, 'job')
    await fill(user, d, /^role/i, 'no_such_role')
    await user.click(d.getByRole('button', { name: 'Register the key' }))
    expect((await d.findAllByText('This role does not exist.')).length).toBeGreaterThan(0)
    const role = getState().roles[0]!.name
    await fill(user, d, /^role/i, role)
    await user.click(d.getByRole('button', { name: 'Register the key' }))
    await waitFor(() => expect(getState().ledger7.clientKeys.shop?.[0]?.roleName).toBe(role))
  })
  it('a key of small order is refused by the server, in words at the field', async () => {
    const user = userEvent.setup()
    await open()
    const d = await openRegister(user)
    await fill(user, d, /^public key/i, IDENTITY)
    await fill(user, d, /^label/i, 'forged')
    await user.click(d.getByRole('button', { name: 'Register the key' }))
    expect(
      (await d.findAllByText(/point of the curve that does not have small order/)).length,
    ).toBeGreaterThan(0)
    expect(getState().ledger7.clientKeys.shop ?? []).toHaveLength(0)
  })
  it('the same key twice: "registered already", and a revoked key is never registered again', async () => {
    const user = userEvent.setup()
    sw('clientkeys=keys')
    await open()
    const existing = getState().ledger7.clientKeys.shop![2]!
    const d = await openRegister(user)
    await fill(user, d, /^public key/i, existing.publicKey)
    await fill(user, d, /^label/i, 'again')
    await user.click(d.getByRole('button', { name: 'Register the key' }))
    const notice = await d.findByTestId('ledger-refusal')
    expect(notice).toHaveTextContent('This key is registered already')
    expect(notice).toHaveTextContent(/a key that was revoked is never registered again/i)
  })
  it('a server without OpenSSL, and an old extension: not available, in words, nothing changed', async () => {
    const user = userEvent.setup()
    sw('clientkeys=nossl')
    await open()
    const d = await openRegister(user)
    await fill(user, d, /^public key/i, GOOD_KEY)
    await fill(user, d, /^label/i, 'job')
    await user.click(d.getByRole('button', { name: 'Register the key' }))
    const notice = await d.findByTestId('ledger-refusal')
    expect(notice).toHaveTextContent('Not available on this server version yet')
    expect(notice).toHaveTextContent(/no OpenSSL/i)
    expect(notice).toHaveTextContent(/Nothing was changed/)
    expect(getState().ledger7.clientKeys.shop ?? []).toHaveLength(0)
  })
})

describe('revoking a key', () => {
  it('needs a reason, says what a revocation means, and shows the key as revoked with the reason', async () => {
    const user = userEvent.setup()
    sw('clientkeys=keys')
    await open()
    const k = getState().ledger7.clientKeys.shop![0]!
    await user.click(await screen.findByRole('button', { name: `Revoke key ${k.keyId}` }))
    const d = within(await dialog())
    expect(
      d.getByText(/The entries it signed before the revocation stay valid/),
    ).toBeInTheDocument()
    await user.click(d.getByRole('button', { name: 'Revoke the key' }))
    expect(await d.findByText('Give a reason of 1 to 200 characters.')).toBeInTheDocument()
    expect(k.revokedAt).toBeNull()
    await user.type(d.getByLabelText(/^reason/i), 'The key was copied.')
    await user.click(d.getByRole('button', { name: 'Revoke the key' }))
    await waitFor(() => expect(k.revokedAt).not.toBeNull())
    expect(await within(keysCard()).findByText(/The key was copied\./)).toBeInTheDocument()
  })
  it('a key that is revoked already is told, not failed', async () => {
    const user = userEvent.setup()
    sw('clientkeys=keys')
    await open()
    const k = getState().ledger7.clientKeys.shop![0]!
    await user.click(await screen.findByRole('button', { name: `Revoke key ${k.keyId}` }))
    const d = within(await dialog())
    k.revokedAt = new Date().toISOString() // revoked by someone else meanwhile
    await user.type(d.getByLabelText(/^reason/i), 'Too late.')
    await user.click(d.getByRole('button', { name: 'Revoke the key' }))
    const notice = await d.findByTestId('ledger-refusal')
    expect(notice).toHaveTextContent('The key is revoked already')
    expect(notice).toHaveAttribute('data-code', 'conflict')
  })
})

describe('the signature policy of a table', () => {
  async function openPolicy(user: User) {
    await user.click(
      await screen.findByRole('button', { name: `Require signatures for ${tableLabel()}` }),
    )
    return within(await dialog())
  }
  it('lists the ledger tables, says the policy is not reported, and says before the click what the switch waits for and cannot undo', async () => {
    const user = userEvent.setup()
    await open()
    await within(tablesCard()).findAllByTestId(/^ledger-table-/)
    expect(screen.getByTestId('policy-not-reported')).toBeInTheDocument()
    expect(within(tablesCard()).getByTestId(`ledger-table-${tableLabel()}`)).toHaveTextContent(
      'Not reported',
    )
    const d = await openPolicy(user)
    const text = d.getByTestId('policy-consequences').textContent ?? ''
    expect(text).toMatch(/waits for the open writers of the table/i)
    expect(text).toMatch(/KL010/)
    expect(text).toMatch(/cannot be switched back/i)
    expect(text).toMatch(/committed before the switch are not asked/i)
    expect(d.getByRole('button', { name: 'Require signatures' })).toBeDisabled()
  })
  it('needs the name of the table typed; then the table is Required, and its button is blocked', async () => {
    const user = userEvent.setup()
    await open()
    await within(tablesCard()).findAllByTestId(/^ledger-table-/)
    const d = await openPolicy(user)
    await user.type(
      d.getByLabelText(new RegExp(`type ${tableLabel().replace('.', '\\.')} to confirm`, 'i')),
      tableLabel(),
    )
    await user.click(d.getByRole('button', { name: 'Require signatures' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(getState().ledger7.policies).toContain(`shop:${tableLabel()}`)
    expect(within(tablesCard()).getByTestId(`ledger-table-${tableLabel()}`)).toHaveTextContent(
      'Required',
    )
    const b = screen.getByRole('button', { name: `Require signatures for ${tableLabel()}` })
    expect(b).toHaveAttribute('aria-disabled', 'true')
    expect(b).toHaveTextContent(/cannot go back/i)
  })
  it('says that it waits while the request is open', async () => {
    const user = userEvent.setup()
    let release: () => void = () => undefined
    const gate = new Promise<void>((r) => (release = r))
    mockServer.use(
      http.put('*/api/v1/databases/:db/schemas/:s/tables/:t/ledger/signature-policy', async () => {
        await gate
        return undefined
      }),
    )
    await open()
    await within(tablesCard()).findAllByTestId(/^ledger-table-/)
    const d = await openPolicy(user)
    await user.type(d.getByLabelText(/to confirm/i), tableLabel())
    await user.click(d.getByRole('button', { name: 'Require signatures' }))
    expect(await d.findByTestId('policy-waiting')).toHaveTextContent(/Waiting for the open writers/)
    release()
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })
  const refusals: [string, number, RegExp, boolean][] = [
    ['conflict', 409, /The policy is "required" already/, true],
    ['unprocessable', 422, /This table is not a ledger table/, false],
    ['not_found', 404, /The table does not exist/, false],
    ['unsupported_operation', 422, /Not available on this server version yet/, false],
  ]
  for (const [code, status, words, required] of refusals) {
    it(`${code}: told in its own words${required ? ' and the table is shown as Required' : ''}`, async () => {
      const user = userEvent.setup()
      mockServer.use(
        http.put('*/api/v1/databases/:db/schemas/:s/tables/:t/ledger/signature-policy', () =>
          HttpResponse.json(
            { type: 'about:blank', title: 'x', status, code, detail: 'd' },
            { status },
          ),
        ),
      )
      await open()
      await within(tablesCard()).findAllByTestId(/^ledger-table-/)
      const d = await openPolicy(user)
      await user.type(d.getByLabelText(/to confirm/i), tableLabel())
      await user.click(d.getByRole('button', { name: 'Require signatures' }))
      expect(await d.findByText(words)).toBeInTheDocument()
      await user.click(d.getByRole('button', { name: 'Cancel' }))
      const cell = within(tablesCard()).getByTestId(`ledger-table-${tableLabel()}`)
      expect(cell).toHaveTextContent(required ? 'Required' : 'Not reported')
    })
  }
})

describe('the upgrade of the extension', () => {
  it('says what it does before the click, then follows the task', async () => {
    const user = userEvent.setup()
    await open()
    await user.click(await screen.findByRole('button', { name: /^Upgrade the extension/ }))
    const d = within(await dialog())
    expect(d.getByTestId('upgrade-text')).toHaveTextContent(/ALTER EXTENSION kingres_ledger UPDATE/)
    expect(d.getByTestId('upgrade-text')).toHaveTextContent(/error names its server/)
    await user.click(d.getByRole('button', { name: 'Upgrade the extension' }))
    await d.findByRole('progressbar')
    await waitFor(() =>
      expect(getState().tasks.some((t) => t.task.kind === 'ledger.upgrade')).toBe(true),
    )
  })
  it('an upgrade that runs already, and a server that does not build it, in words', async () => {
    const user = userEvent.setup()
    sw('clientkeys=refuse:operation_in_progress')
    await open()
    await user.click(await screen.findByRole('button', { name: /^Upgrade the extension/ }))
    const d = within(await dialog())
    await user.click(d.getByRole('button', { name: 'Upgrade the extension' }))
    expect(await d.findByText('An upgrade is already running')).toBeInTheDocument()
  })
})

describe('each problem code of a change, in its own words', () => {
  const CODES: [string, RegExp][] = [
    ['forbidden', /^Not allowed$/],
    ['leader_unavailable', /No manager leader/],
    ['validation_failed', /Check the highlighted fields/],
    ['conflict', /^Conflict$/],
  ]
  for (const [code, words] of CODES)
    it(`${code}: registering shows it and registers nothing`, async () => {
      const user = userEvent.setup()
      sw(`clientkeys=refuse:${code}`)
      await open()
      const d = await openRegister(user)
      await fill(user, d, /^public key/i, GOOD_KEY)
      await fill(user, d, /^label/i, 'job')
      await user.click(d.getByRole('button', { name: 'Register the key' }))
      expect(await d.findByText(words)).toBeInTheDocument()
      expect(getState().ledger7.clientKeys.shop ?? []).toHaveLength(0)
    })
})

describe('what each role sees and does', () => {
  const blocked = (b: HTMLElement) => b.getAttribute('aria-disabled') === 'true'
  it('kingres_admin may do everything', async () => {
    sw('clientkeys=keys')
    await open('admin')
    await within(keysCard()).findAllByTestId(/^client-key-[0-9a-f]{8}$/)
    for (const name of [/^Register a key/, /^Upgrade the extension/, /^Revoke key/])
      expect(blocked(screen.getAllByRole('button', { name })[0]!)).toBe(false)
    await within(tablesCard()).findAllByTestId(/^ledger-table-/)
    expect(blocked(screen.getAllByRole('button', { name: /^Require signatures for/ })[0]!)).toBe(
      false,
    )
  })
  for (const user of ['ops', 'viewer'])
    it(`${user === 'ops' ? 'kingres_ops' : 'kingres_readonly'} reads everything and may change nothing here`, async () => {
      sw('clientkeys=keys')
      await open(user)
      await within(keysCard()).findAllByTestId(/^client-key-[0-9a-f]{8}$/)
      await within(tablesCard()).findAllByTestId(/^ledger-table-/)
      for (const name of [
        /^Register a key/,
        /^Upgrade the extension/,
        /^Revoke key/,
        /^Require signatures for/,
      ]) {
        const b = screen.getAllByRole('button', { name })[0]!
        expect(blocked(b)).toBe(true)
        expect(b).toHaveTextContent(/kingres_admin/)
      }
    })
})

describe('text chosen by someone hostile', () => {
  it('labels, roles, registrars and reasons are drawn as text with the hidden characters made visible', async () => {
    seedHostile(getState())
    const { container } = await renderApp('/ledger?tab=clients&db=shop')
    await within(await screen.findByTestId('client-keys')).findAllByTestId(
      /^client-key-[0-9a-f]{8}$/,
    )
    expect(container.querySelector('img')).toBeNull()
    expect(keysCard().textContent).toContain('U+202E')
    const hidden = [
      0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069, 0x200b, 0x200c,
      0x200d, 0x200e, 0x200f,
    ]
    expect(hidden.filter((c) => container.textContent?.includes(String.fromCharCode(c)))).toEqual(
      [],
    )
  })
})

describe('accessibility', () => {
  it('the tab with keys and tables, and each dialog', async () => {
    const user = userEvent.setup()
    sw('clientkeys=keys')
    const { container } = await open()
    await within(keysCard()).findAllByTestId(/^client-key-[0-9a-f]{8}$/)
    await within(tablesCard()).findAllByTestId(/^ledger-table-/)
    expect(await axe(container)).toHaveNoViolations()
    const d = await openRegister(user)
    expect(await axe(await dialog())).toHaveNoViolations()
    await user.click(d.getByRole('button', { name: 'Register the key' }))
    await d.findByText(/44 characters ending with/)
    expect(await axe(await dialog())).toHaveNoViolations()
    await user.click(d.getByRole('button', { name: 'Cancel' }))
    await user.click(screen.getAllByRole('button', { name: /^Revoke key/ })[0]!)
    expect(await axe(await dialog())).toHaveNoViolations()
    await user.click(within(await dialog()).getByRole('button', { name: 'Cancel' }))
    await user.click(screen.getAllByRole('button', { name: /^Require signatures for/ })[0]!)
    expect(await axe(await dialog())).toHaveNoViolations()
    await user.click(within(await dialog()).getByRole('button', { name: 'Cancel' }))
    await user.click(screen.getByRole('button', { name: /^Upgrade the extension/ }))
    expect(await axe(await dialog())).toHaveNoViolations()
  })
})

describe('the words and checks of the library', () => {
  const key = {
    keyId: 'abcd1234',
    publicKey: GOOD_KEY,
    roleName: null,
    label: 'l',
    registeredBy: 'a',
    registeredAt: '2026-11-19T10:00:00Z',
    revokedAt: null,
    revocationReason: null,
  }
  it('a key in words: state, role, revocation, a short form', () => {
    expect(keyState(key)).toBe('active')
    expect(keyState({ ...key, revokedAt: '2026-11-19T11:00:00Z' })).toBe('revoked')
    expect(keyRole(key)).toBe('Any role')
    expect(keyRole({ roleName: 'app' })).toBe('app')
    expect(revocationText({ revokedAt: '2026-11-19T11:00:00Z', revocationReason: 'lost' })).toBe(
      'Revoked 19 Nov 2026, 11:00:00 UTC: lost',
    )
    expect(revocationText(key)).toBe('')
    expect(shortKey(GOOD_KEY)).toBe('q83vEjRWeJ...kBI0Vng=')
  })
  it('the checks of a registration and what is sent', () => {
    expect(validateRegister({ publicKey: GOOD_KEY, roleName: '', label: 'x' })).toEqual({})
    expect(validateRegister({ publicKey: 'short', roleName: '', label: '' })).toMatchObject({
      publicKey: expect.any(String),
      label: expect.any(String),
    })
    expect(
      validateRegister({ publicKey: GOOD_KEY, roleName: 'r'.repeat(64), label: 'x' }).roleName,
    ).toBeTruthy()
    expect(
      validateRegister({ publicKey: GOOD_KEY, roleName: '', label: 'x'.repeat(101) }).label,
    ).toBeTruthy()
    expect(registerBody({ publicKey: ` ${GOOD_KEY} `, roleName: ' ', label: ' job ' })).toEqual({
      publicKey: GOOD_KEY,
      label: 'job',
    })
    expect(registerBody({ publicKey: GOOD_KEY, roleName: 'app', label: 'job' })).toEqual({
      publicKey: GOOD_KEY,
      label: 'job',
      roleName: 'app',
    })
  })
  it('the refusals by action and code; a code without words falls back', () => {
    const e = (code: string) =>
      new ApiError({ type: 'x', title: 'x', status: 409, code: code as never } as never)
    expect(clientRefusal(e('conflict'), 'policy')?.alreadyDone).toBe(true)
    expect(clientRefusal(e('conflict'), 'revoke')?.alreadyDone).toBe(true)
    expect(clientRefusal(e('conflict'), 'register')).toBeNull()
    expect(clientRefusal(e('already_exists'), 'register')?.heading).toBe(
      'This key is registered already',
    )
    expect(clientRefusal(e('unprocessable'), 'policy')?.heading).toBe(
      'This table is not a ledger table',
    )
    expect(clientRefusal(e('operation_in_progress'), 'upgrade')?.heading).toBe(
      'An upgrade is already running',
    )
    expect(clientRefusal(e('unsupported_operation'), 'register')?.message).toMatch(
      /older than version 0\.3/,
    )
    expect(clientRefusal(e('unsupported_operation'), 'upgrade')?.message).not.toMatch(
      /Upgrade the extension of the database/,
    )
    expect(clientRefusal(new Error('x'), 'register')).toBeNull()
  })
})

describe('the mock answers as the contract says', () => {
  it('the keys, the registration, the revocation and the policy satisfy the schemas', async () => {
    await signInAs(createPinia())
    const k = await endpoints.registerLedgerClientKey('shop', { publicKey: GOOD_KEY, label: 'job' })
    ok(k, 'LedgerClientKey')
    ok(await endpoints.listLedgerClientKeys('shop'), 'LedgerClientKeyList')
    ok(await endpoints.revokeLedgerClientKey('shop', k.keyId, 'done'), 'LedgerClientKey')
    await expect(endpoints.revokeLedgerClientKey('shop', k.keyId, 'again')).rejects.toMatchObject({
      code: 'conflict',
    })
    mockConfig.clientLegacy = true
    await expect(endpoints.listLedgerClientKeys('shop')).rejects.toMatchObject({
      code: 'unsupported_operation',
      status: 422,
    })
  })
})
