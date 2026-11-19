// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The Anchoring tab against the server code that exists now (`internal/ledgeranchor`): the member names and the checks of each
// kind as the server has them, the header source of OpenTimestamps, the credentials with the limits of each member, a credentials
// file in place of typed credentials, removing the stored credentials, the pattern of the name, and the alert of a system database
// that is older than what was published.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import {
  buildConfig,
  initialValues,
  NAME_PATTERN,
  validateForm,
  valuesFromConfig,
  extraMembers,
} from '@/lib/ledger-anchor'
import { calmClocks, renderApp } from './helpers'

const sw = (q: string) => applySwitches(new URLSearchParams(q), getState())
const worm = () => ({ ...initialValues('worm'), region: 'eu-west-1', bucket: 'ledger-anchors' })
const ots = (over: Record<string, string | boolean> = {}) => ({
  ...initialValues('ots'),
  calendars: 'https://a.example',
  ...over,
})
const goodCreds = { accessKeyId: 'AKIA0001', secretAccessKey: 'SECRET01' }

describe('the name, as the server checks it', () => {
  it('starts with a letter or a digit, then letters, digits, space . _ -, at most 63', () => {
    for (const ok of ['a', '9x', 'archive worm', 'a.b_c-d', 'x'.repeat(63)])
      expect(NAME_PATTERN.test(ok), ok).toBe(true)
    for (const bad of ['', '-a', ' a', '.a', 'a/b', 'a:b', 'x'.repeat(64)])
      expect(NAME_PATTERN.test(bad), bad).toBe(false)
    expect(validateForm('worm', '-bad', worm(), goodCreds, true).name).toMatch(
      /starting with a letter or a digit/,
    )
  })
})

describe('the credentials, with the limits of each member', () => {
  const e = (c: Record<string, string>, v = worm()) => validateForm('worm', 'n', v, c, true)
  it('access key id 4 to 128, secret 4 to 256, session token 4 to 4096', () => {
    expect(e({ accessKeyId: 'abc', secretAccessKey: 'abcd' })['credentials.accessKeyId']).toBe(
      'Access key id must have 4 to 128 characters.',
    )
    expect(
      e({ accessKeyId: 'a'.repeat(129), secretAccessKey: 'abcd' })['credentials.accessKeyId'],
    ).toBeTruthy()
    expect(e({ accessKeyId: 'abcd', secretAccessKey: 'abc' })['credentials.secretAccessKey']).toBe(
      'Secret access key must have 4 to 256 characters.',
    )
    expect(e({ ...goodCreds, sessionToken: 'abc' })['credentials.sessionToken']).toBe(
      'Session token must have 4 to 4096 characters.',
    )
    expect(
      e({ ...goodCreds, sessionToken: 'a'.repeat(4096) })['credentials.sessionToken'],
    ).toBeUndefined()
    expect(e(goodCreds)).toEqual({})
  })
  it('printable ASCII only, no space (a password may hold one)', () => {
    expect(e({ accessKeyId: 'ab cd', secretAccessKey: 'abcd' })['credentials.accessKeyId']).toMatch(
      /printable ASCII characters only, without spaces/,
    )
    expect(
      e({ accessKeyId: 'abcd', secretAccessKey: 'cafécafé' })['credentials.secretAccessKey'],
    ).toMatch(/printable ASCII/)
    const tsa = { ...initialValues('tsa'), url: 'https://t.example', caBundlePath: '/c.pem' }
    expect(validateForm('tsa', 't', tsa, { username: 'u', password: 'a b c' }, true)).toEqual({})
    expect(
      validateForm('tsa', 't', tsa, { username: 'u s', password: 'abc' }, true)[
        'credentials.username'
      ],
    ).toBeTruthy()
  })
  it('a credentials file in place of typed credentials: they exclude each other, and it makes them optional', () => {
    const withFile = { ...worm(), credentialsPath: '/run/secrets/anchor.json' }
    expect(validateForm('worm', 'n', withFile, {}, true)).toEqual({})
    expect(validateForm('worm', 'n', withFile, goodCreds, true)['credentials.accessKeyId']).toBe(
      'Typed credentials and a credentials file exclude each other.',
    )
  })
})

describe('the members of each kind, as the server has them', () => {
  it('OpenTimestamps: headerSource.url is sent as {"url": ...}, read back, and checked', () => {
    const v = ots({ 'headerSource.url': 'https://blocks.example/api' })
    expect(buildConfig('ots', v)).toEqual({
      calendars: ['https://a.example'],
      headerSource: { url: 'https://blocks.example/api' },
    })
    const back = valuesFromConfig('ots', {
      calendars: ['https://a.example'],
      headerSource: { url: 'https://blocks.example/api' },
    })
    expect(back['headerSource.url']).toBe('https://blocks.example/api')
    expect(extraMembers('ots', { calendars: [], headerSource: { url: 'x' } })).toEqual({})
    expect(
      validateForm('ots', 'o', ots({ 'headerSource.url': 'http://blocks.example' }), {}, true)[
        'headerSource.url'
      ],
    ).toMatch(/needs the box/)
    expect(
      validateForm(
        'ots',
        'o',
        ots({ 'headerSource.url': 'http://blocks.example', allowInsecure: true }),
        {},
        true,
      )['headerSource.url'],
    ).toBeUndefined()
    expect(
      validateForm('ots', 'o', ots({ 'headerSource.url': 'ftp://x' }), {}, true)[
        'headerSource.url'
      ],
    ).toBeTruthy()
    expect(
      validateForm('ots', 'o', ots({ contradictionConfirmMinutes: '0' }), {}, true)
        .contradictionConfirmMinutes,
    ).toBeTruthy()
    expect(
      validateForm('ots', 'o', ots({ contradictionConfirmMinutes: '10080' }), {}, true)
        .contradictionConfirmMinutes,
    ).toBeUndefined()
    expect(
      buildConfig('ots', ots({ contradictionConfirmMinutes: '90' })).contradictionConfirmMinutes,
    ).toBe(90)
  })
  it('time-stamp authority: an http address is fine without credentials; with a user name it needs the box', () => {
    const tsa = { ...initialValues('tsa'), url: 'http://tsa.example/tsr', caBundlePath: '/c.pem' }
    expect(validateForm('tsa', 't', tsa, {}, true)).toEqual({})
    expect(
      validateForm('tsa', 't', tsa, { username: 'u', password: 'p' }, true)['credentials.username'],
    ).toMatch(/needs the box/)
    expect(
      validateForm(
        'tsa',
        't',
        { ...tsa, allowInsecure: true },
        { username: 'u', password: 'p' },
        true,
      ),
    ).toEqual({})
  })
  it('a time of 0 means the default and is accepted, as the server accepts it', () => {
    expect(
      validateForm('worm', 'n', { ...worm(), maxSkewSeconds: '0' }, goodCreds, true).maxSkewSeconds,
    ).toBeUndefined()
    const tsa = {
      ...initialValues('tsa'),
      url: 'https://t.example',
      caBundlePath: '/c.pem',
      timeoutSeconds: '0',
    }
    expect(validateForm('tsa', 't', tsa, {}, true).timeoutSeconds).toBeUndefined()
  })
})

async function open(user = 'admin') {
  const app = await renderApp('/ledger?tab=anchoring&db=shop', { user })
  await screen.findByTestId('anchor-records')
  return app
}

describe('the dialog', () => {
  it('OpenTimestamps: the header source says what it is for, and is saved as headerSource', async () => {
    const u = userEvent.setup()
    await open()
    await u.click(await screen.findByRole('button', { name: /^Add a target/ }))
    const d = within(await screen.findByRole('dialog'))
    await u.click(d.getByTestId('kind-ots'))
    expect(d.getByTestId('header-source-note')).toHaveTextContent(
      /without it a claim stays a claim/i,
    )
    await u.type(d.getByLabelText(/^name/i), 'bitcoin')
    await u.click(d.getByRole('button', { name: 'Use the public calendars' }))
    await u.type(d.getByLabelText(/block header source/i), 'https://blocks.example/api')
    await u.click(d.getByRole('button', { name: 'Add the target' }))
    await waitFor(() => expect(getState().ledger7.targets).toHaveLength(1))
    expect(getState().ledger7.targets[0]?.config.headerSource).toEqual({
      url: 'https://blocks.example/api',
    })
  })

  it('the stored credentials can be removed: the request carries {} and nothing else about them', async () => {
    const u = userEvent.setup()
    sw('anchors=all')
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.patch('*/api/v1/ledger/anchor-targets/:id', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    await open()
    // The time-stamp authority has none; give it some, then remove them.
    const tsa = getState().ledger7.targets.find((t) => t.name === 'public-tsa')!
    tsa.credentialsSet = true
    await u.click(await screen.findByRole('button', { name: 'Edit public-tsa' }))
    const d = within(await screen.findByRole('dialog'))
    await u.click(d.getByTestId('remove-credentials'))
    await u.click(d.getByRole('button', { name: 'Save the target' }))
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]).toEqual({ credentials: {} })
    expect(tsa.credentialsSet).toBe(false)
  })

  it("a message of the server at /config and at /credentials is shown in the dialog, in the server's words", async () => {
    const u = userEvent.setup()
    await open()
    await u.click(await screen.findByRole('button', { name: /^Add a target/ }))
    const d = within(await screen.findByRole('dialog'))
    await u.click(d.getByTestId('kind-tsa'))
    await u.type(d.getByLabelText(/^name/i), 'tsa-one')
    await u.type(d.getByLabelText(/address of the authority/i), 'https://t.example/tsr')
    await u.type(d.getByLabelText(/certificates of the authority/i), 'relative.pem')
    await u.click(d.getByRole('button', { name: 'Add the target' }))
    expect(
      (await d.findAllByText(/the bundle cannot be read by this server/)).length,
    ).toBeGreaterThan(0)
  })
})

describe('the alert of a system database that is older than what was published', () => {
  it('has its own words and a link to the Publication tab', async () => {
    sw('signing=restored')
    calmClocks()
    await renderApp('/alerts')
    const note = await screen.findByTestId('alert-published-state')
    expect(note).toHaveTextContent(/older state than what the ledger published/i)
    expect(note).toHaveTextContent(/Nothing is signed/i)
    expect(
      screen.getAllByText('The system database is older than what was published').length,
    ).toBeGreaterThan(0)
    const link = await screen.findByRole('link', { name: 'What was published and how to go on' })
    expect(link.getAttribute('href')).toMatch(/tab=publication/)
  })
  it('an ordinary signer alarm keeps the Signing link', async () => {
    sw('signing=alarm')
    calmClocks()
    await renderApp('/alerts')
    const link = await screen.findByRole('link', { name: 'Signing keys of the ledger' })
    expect(link.getAttribute('href')).toMatch(/tab=signing/)
    expect(screen.queryByTestId('alert-published-state')).not.toBeInTheDocument()
  })
})

describe('the records of OpenTimestamps', () => {
  it('a claim that a calendar made arrives as "submitted" and is said to be not final', async () => {
    sw('anchors=all')
    await open()
    await within(screen.getByTestId('anchor-records')).findAllByTestId(/^record-\d+$/)
    const states = within(screen.getByTestId('anchor-records'))
      .getAllByTestId('record-state')
      .map((e) => e.textContent ?? '')
    expect(states.some((s) => s.includes('Sent, not final'))).toBe(true)
    expect(screen.getByTestId('ots-wait')).toHaveTextContent(
      /without a block header source it never counts as anchored/i,
    )
  })
})
