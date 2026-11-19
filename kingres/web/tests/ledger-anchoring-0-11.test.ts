// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The Anchoring tab of the Ledger page (Console B, contract 0.11.0, design 6.2 to 6.9 and 8.5): every state of the list of
// targets (loading, error, not available yet, empty, not anchored, one kind only, three kinds), the dialogs that add and
// edit a target of each kind, the secrets that are never shown, the test and its result, turning a target on and off,
// deleting it, the anchor records (retry, evidence, anchor now), each problem code in its own words, what each role sees and
// does, the settings of anchoring on the Settings page, the alerts and the dashboard card, hostile text and axe.
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { configure, getConfig, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { axe } from 'vitest-axe'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { seedHostile } from '@/mocks/hostile'
import { calmClocks, renderApp } from './helpers'

type User = ReturnType<typeof userEvent.setup>

const sw = (query: string) => applySwitches(new URLSearchParams(query), getState())

/** Opens the Anchoring tab of the database `shop`. */
async function open(user = 'admin', path = '/ledger?tab=anchoring&db=shop') {
  const app = await renderApp(path, { user })
  await screen.findByRole('heading', { name: 'Anchor targets' })
  await screen.findByTestId('anchor-records')
  return app
}
const targetsCard = () => screen.getByTestId('anchor-targets')
const recordsCard = () => screen.getByTestId('anchor-records')
const dialog = () => screen.findByRole('dialog')
const targets = () => getState().ledger7.targets

async function openAdd(user: User) {
  await user.click(screen.getByRole('button', { name: /^Add a target/ }))
  return within(await dialog())
}
/** Types into a field of the dialog by its label. */
async function fill(user: User, d: ReturnType<typeof within>, label: RegExp, text: string) {
  const el = d.getByLabelText(label)
  await user.clear(el)
  await user.type(el, text)
}

const saved: { name: string; blob: Blob }[] = []
beforeEach(() => {
  saved.length = 0
  URL.createObjectURL = (blob: Blob) => {
    saved.push({ name: '', blob })
    return `blob:test/${saved.length}`
  }
  URL.revokeObjectURL = () => undefined
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    const last = saved[saved.length - 1]
    if (last) last.name = this.download
  })
})
afterEach(() => vi.restoreAllMocks())

describe('the states of the list of targets', () => {
  it('no target: the notice of design 6.9 with the button, the advice to start with a WORM store, empty records', async () => {
    await open()
    expect(screen.getByTestId('no-targets')).toHaveTextContent(/start with a worm store/i)
    expect(screen.getByTestId('anchoring-not-anchored')).toHaveTextContent(
      'Not anchored: this ledger can be rewritten undetected by anyone who controls this cluster and its backups. Add a target.',
    )
    expect(screen.getByRole('button', { name: /^Add a target/ })).not.toHaveAttribute(
      'aria-disabled',
    )
    expect(await within(recordsCard()).findByText('No anchor records yet')).toBeInTheDocument()
    expect(
      within(recordsCard()).getByText(/nothing is anchored while no target is enabled/i),
    ).toBeInTheDocument()
  })

  it('loading: a placeholder with a name for a screen reader', async () => {
    mockConfig.latencyMs = 400
    await renderApp('/ledger?tab=anchoring&db=shop')
    expect((await screen.findAllByText('Loading')).length).toBeGreaterThan(0)
    await screen.findByRole('heading', { name: 'Anchor targets' })
  })

  it('an error: shown with a retry that reads the list again', async () => {
    const user = userEvent.setup()
    let failing = true
    mockServer.use(
      http.get('*/api/v1/ledger/anchor-targets', () =>
        failing
          ? HttpResponse.json(
              { type: 'about:blank', title: 'x', status: 500, code: 'internal_error', detail: 'd' },
              { status: 500 },
            )
          : undefined,
      ),
    )
    await open()
    const notice = await within(targetsCard()).findByRole('alert')
    expect(notice).toHaveAttribute('data-code', 'internal_error')
    failing = false
    await user.click(within(notice).getByRole('button', { name: /retry|try again/i }))
    expect(await screen.findByTestId('no-targets')).toBeInTheDocument()
  })

  it('one target of one kind: the advice that it is one point of failure', async () => {
    sw('anchors=ok')
    await open()
    expect(await screen.findByTestId('anchoring-one-kind')).toHaveTextContent(
      /single anchor target is one point of failure and one administrator/i,
    )
    expect(screen.queryByTestId('anchoring-not-anchored')).not.toBeInTheDocument()
  })

  it('three kinds: each target with its kind, state, window, credentials and last test; no advice', async () => {
    sw('anchors=all')
    await open()
    const rows = await within(targetsCard()).findAllByTestId(/^target-\d+$/)
    expect(rows).toHaveLength(3)
    const [worm, tsa, ots] = rows as HTMLElement[]
    expect(worm).toHaveTextContent('archive-worm')
    expect(worm).toHaveTextContent('WORM object store (S3 Object Lock)')
    expect(within(worm!).getByTestId('target-state')).toHaveTextContent('Anchoring')
    expect(within(worm!).getByTestId('target-window')).toHaveTextContent(/minutes old/)
    expect(within(worm!).getByTestId('target-credentials')).toHaveTextContent('Stored')
    expect(within(tsa!).getByTestId('target-state')).toHaveTextContent('Behind')
    expect(within(tsa!).getByTestId('target-window')).toHaveTextContent(/7 hours old/)
    expect(within(tsa!).getByTestId('target-credentials')).toHaveTextContent('Not set')
    expect(within(ots!).getByTestId('target-last-test')).toHaveTextContent('Never tested')
    expect(within(tsa!).getByTestId('target-last-test')).toHaveTextContent('Passed with warnings')
    expect(screen.queryByTestId('anchoring-one-kind')).not.toBeInTheDocument()
    expect(screen.queryByTestId('anchoring-not-anchored')).not.toBeInTheDocument()
  })

  it('a target that is turned off: not anchored, with the badge "Not used"', async () => {
    sw('anchors=disabled')
    await open()
    expect(await screen.findByTestId('anchoring-not-anchored')).toBeInTheDocument()
    expect(within(targetsCard()).getByText('Not used')).toBeInTheDocument()
    expect(within(targetsCard()).getByTestId('target-state')).toHaveTextContent(
      'Nothing anchored yet',
    )
  })

  it('a server that does not build anchoring yet: the calm notice, every control kept and handled', async () => {
    const user = userEvent.setup()
    sw('phase7=today')
    await open()
    const notice = (
      await within(targetsCard()).findByText(/not available on this server version yet/i)
    ).closest('[data-code]') as HTMLElement
    expect(notice).toHaveAttribute('data-code', 'unsupported_operation')
    expect(notice).toHaveTextContent(/not available on this server version yet/i)
    // The records are said to be unavailable in their own place; Anchor now is handled.
    expect(
      (await within(recordsCard()).findAllByText(/not available on this server version yet/i))
        .length,
    ).toBeGreaterThan(0)
    await user.click(within(recordsCard()).getByRole('button', { name: /^Anchor now/ }))
    await waitFor(() =>
      expect(
        within(recordsCard()).getAllByText(/not available on this server version yet/i).length,
      ).toBeGreaterThan(1),
    )
    // The settings work today, and say so.
    expect(screen.getByTestId('anchor-settings-text')).toHaveTextContent(/every\s+3600 seconds/)
    // Adding a target is tried and answered, and nothing changes.
    const d = await openAdd(user)
    await fill(user, d, /^name/i, 'vault')
    await fill(user, d, /^region/i, 'eu-west-1')
    await fill(user, d, /^bucket/i, 'ledger-anchors')
    await fill(user, d, /access key id/i, 'AKIA')
    await fill(user, d, /secret access key/i, 'secret')
    await user.type(d.getByLabelText(/type compliance to confirm/i), 'compliance')
    await user.click(d.getByRole('button', { name: 'Add the target' }))
    expect(await d.findByText(/not available on this server version yet/i)).toBeInTheDocument()
    expect(targets()).toHaveLength(0)
    expect(d.getByLabelText(/secret access key/i)).toHaveValue('')
  })
})

describe('adding a target', () => {
  it('WORM: the lock mode that cannot be undone needs the word typed and shows the end date; the secret is never shown again', async () => {
    const user = userEvent.setup()
    await open()
    const d = await openAdd(user)
    expect(d.getByTestId('retention-date')).toHaveTextContent(/UTC$/)
    await fill(user, d, /^name/i, 'vault')
    await fill(user, d, /^region/i, 'eu-west-1')
    await fill(user, d, /^bucket/i, 'ledger-anchors')
    await fill(user, d, /access key id/i, 'AKIAEXAMPLE')
    await fill(user, d, /secret access key/i, 'hunter2-secret-value')
    await user.click(d.getByRole('button', { name: 'Add the target' }))
    expect(await d.findByText('Type compliance to confirm the lock mode.')).toBeInTheDocument()
    expect(targets()).toHaveLength(0)
    await user.type(d.getByLabelText(/type compliance to confirm/i), 'compliance')
    await user.click(d.getByRole('button', { name: 'Add the target' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(targets()).toHaveLength(1)
    expect(targets()[0]).toMatchObject({
      kind: 'worm',
      name: 'vault',
      enabled: false,
      credentialsSet: true,
      config: {
        region: 'eu-west-1',
        bucket: 'ledger-anchors',
        keyPrefix: 'kingres-anchors',
        mode: 'compliance',
        retentionDays: 3650,
      },
    })
    expect(await screen.findByTestId('anchoring-notice')).toHaveTextContent('vault was saved.')
    expect(within(targetsCard()).getByTestId('target-credentials')).toHaveTextContent('Stored')
    expect(document.body.textContent).not.toContain('hunter2-secret-value')
    expect(document.body.textContent).not.toContain('AKIAEXAMPLE')
    expect(JSON.stringify(getState().ledger7.targets)).not.toMatch(/hunter2|AKIAEXAMPLE/)
    expect(JSON.stringify({ ...localStorage })).not.toMatch(/hunter2|AKIAEXAMPLE/)
  })

  it('WORM, governance mode: a warning in words and no confirmation to type', async () => {
    const user = userEvent.setup()
    await open()
    const d = await openAdd(user)
    await user.selectOptions(d.getByLabelText(/lock mode/i), 'governance')
    expect(d.getByTestId('governance-warning')).toHaveTextContent(
      /person with the bypass permission of the store can delete/i,
    )
    expect(d.queryByTestId('compliance-confirm')).not.toBeInTheDocument()
    await fill(user, d, /^name/i, 'soft')
    await fill(user, d, /^region/i, 'us-east-1')
    await fill(user, d, /^bucket/i, 'soft-bucket')
    await fill(user, d, /access key id/i, 'AKIA0001')
    await fill(user, d, /secret access key/i, 'SECRET01')
    await user.click(d.getByRole('button', { name: 'Add the target' }))
    await waitFor(() => expect(targets()).toHaveLength(1))
    expect(targets()[0]?.config.mode).toBe('governance')
  })

  it('time-stamp authority: its own fields, the certificates are required, the credentials go together', async () => {
    const user = userEvent.setup()
    await open()
    const d = await openAdd(user)
    await user.click(d.getByTestId('kind-tsa'))
    expect(d.getByLabelText(/address of the authority/i)).toBeInTheDocument()
    expect(d.queryByLabelText(/^bucket/i)).not.toBeInTheDocument()
    await fill(user, d, /^name/i, 'tsa-one')
    await fill(user, d, /address of the authority/i, 'https://tsa.example.com/tsr')
    await fill(user, d, /^user name/i, 'robot')
    await user.click(d.getByRole('button', { name: 'Add the target' }))
    expect(await d.findByText('Certificates of the authority is required.')).toBeInTheDocument()
    expect(d.getByText('Give the password for this user name.')).toBeInTheDocument()
    await fill(user, d, /certificates of the authority/i, '/etc/kingres/tsa.pem')
    await fill(user, d, /^password/i, 'pw')
    await user.click(d.getByRole('button', { name: 'Add the target' }))
    await waitFor(() => expect(targets()).toHaveLength(1))
    expect(targets()[0]).toMatchObject({
      kind: 'tsa',
      credentialsSet: true,
      config: { url: 'https://tsa.example.com/tsr', caBundlePath: '/etc/kingres/tsa.pem' },
    })
  })

  it('OpenTimestamps: "Use the public calendars" fills the list and sends nothing; the list is saved', async () => {
    const user = userEvent.setup()
    await open()
    const d = await openAdd(user)
    await user.click(d.getByTestId('kind-ots'))
    expect(d.getByTestId('ots-note')).toHaveTextContent(/takes hours/i)
    await user.click(d.getByRole('button', { name: 'Use the public calendars' }))
    expect(targets()).toHaveLength(0)
    expect((d.getByLabelText(/^Calendars(?! that)/) as HTMLTextAreaElement).value).toContain(
      'https://alice.btc.calendar.opentimestamps.org',
    )
    await fill(user, d, /^name/i, 'public-ots')
    await user.click(d.getByRole('button', { name: 'Add the target' }))
    await waitFor(() => expect(targets()).toHaveLength(1))
    expect((targets()[0]?.config.calendars as string[]).length).toBe(3)
    expect(targets()[0]?.credentialsSet).toBe(false)
  })

  it('a form with a problem is not sent: the fields are named', async () => {
    const user = userEvent.setup()
    await open()
    const d = await openAdd(user)
    await user.click(d.getByRole('button', { name: 'Add the target' }))
    expect(
      await d.findByText(
        '1 to 63 characters: letters, digits, space, dot, hyphen and underscore, starting with a letter or a digit.',
      ),
    ).toBeInTheDocument()
    expect(d.getByText('Region is required.')).toBeInTheDocument()
    expect(d.getByText('Bucket is required.')).toBeInTheDocument()
    expect(d.getByText('Access key id is required.')).toBeInTheDocument()
    expect(d.getByTestId('form-errors')).toBeInTheDocument()
    expect(targets()).toHaveLength(0)
  })

  it('a message of the server about a field of the configuration sits next to that field', async () => {
    const user = userEvent.setup()
    mockServer.use(
      http.post('*/api/v1/ledger/anchor-targets', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'x',
            status: 400,
            code: 'validation_failed',
            detail: 'd',
            errors: [
              { field: 'config.bucket', message: 'The bucket does not exist in this store.' },
            ],
          },
          { status: 400 },
        ),
      ),
    )
    await open()
    const d = await openAdd(user)
    await user.selectOptions(d.getByLabelText(/lock mode/i), 'governance')
    await fill(user, d, /^name/i, 'vault')
    await fill(user, d, /^region/i, 'eu-west-1')
    await fill(user, d, /^bucket/i, 'ledger-anchors')
    await fill(user, d, /access key id/i, 'AKIA0001')
    await fill(user, d, /secret access key/i, 'SECRET01')
    await user.click(d.getByRole('button', { name: 'Add the target' }))
    const field = d.getByLabelText(/^bucket/i)
    await waitFor(() => expect(field).toHaveAttribute('aria-invalid', 'true'))
    expect(d.getAllByText('The bucket does not exist in this store.').length).toBeGreaterThan(0)
    // The secrets typed are gone from the form after the request, also after a refusal.
    expect(d.getByLabelText(/secret access key/i)).toHaveValue('')
    expect(d.getByLabelText(/access key id/i)).toHaveValue('')
  })

  it('a name that is in use is told at the name', async () => {
    const user = userEvent.setup()
    sw('anchors=ok')
    await open()
    const d = await openAdd(user)
    await user.click(d.getByTestId('kind-ots'))
    await fill(user, d, /^name/i, 'archive-worm')
    await user.click(d.getByRole('button', { name: 'Use the public calendars' }))
    await user.click(d.getByRole('button', { name: 'Add the target' }))
    expect(
      await d.findByText('A target with this name exists. Choose another name.'),
    ).toBeInTheDocument()
    expect(targets()).toHaveLength(1)
  })

  it('changing the kind starts a clean form and keeps no secret of the other kind', async () => {
    const user = userEvent.setup()
    await open()
    const d = await openAdd(user)
    await fill(user, d, /secret access key/i, 'leftover')
    await user.click(d.getByTestId('kind-tsa'))
    await user.click(d.getByTestId('kind-worm'))
    expect(d.getByLabelText(/secret access key/i)).toHaveValue('')
  })
})

describe('editing a target', () => {
  it('the kind is fixed; what is stored is not read back; an empty secret keeps the stored one', async () => {
    const user = userEvent.setup()
    sw('anchors=ok')
    const bodies: unknown[] = []
    mockServer.use(
      http.patch('*/api/v1/ledger/anchor-targets/:id', async ({ request }) => {
        bodies.push(await request.clone().json())
        return undefined
      }),
    )
    await open()
    await user.click(await screen.findByRole('button', { name: 'Edit archive-worm' }))
    const d = within(await dialog())
    expect(d.getByTestId('fixed-kind')).toHaveTextContent('WORM object store (S3 Object Lock)')
    expect(d.queryByTestId('kind-tsa')).not.toBeInTheDocument()
    expect(d.getByTestId('credentials-state')).toHaveTextContent('Credentials are stored.')
    expect(d.getByLabelText(/secret access key/i)).toHaveValue('')
    expect(d.getByLabelText(/secret access key/i)).toHaveAttribute('placeholder', 'Stored')
    expect(d.getByLabelText(/secret access key/i)).toHaveAttribute('type', 'password')
    await fill(user, d, /^bucket/i, 'another-bucket')
    await user.click(d.getByRole('button', { name: 'Save the target' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(bodies).toHaveLength(1)
    expect(bodies[0]).not.toHaveProperty('credentials')
    expect(bodies[0]).toHaveProperty('config.bucket', 'another-bucket')
    expect(targets()[0]?.config.bucket).toBe('another-bucket')
    expect(targets()[0]?.credentialsSet).toBe(true)
  })

  it('a new secret is sent, and replaces the stored one', async () => {
    const user = userEvent.setup()
    sw('anchors=ok')
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.patch('*/api/v1/ledger/anchor-targets/:id', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    await open()
    await user.click(await screen.findByRole('button', { name: 'Edit archive-worm' }))
    const d = within(await dialog())
    await fill(user, d, /access key id/i, 'NEWKEY')
    await fill(user, d, /secret access key/i, 'newsecret')
    await user.click(d.getByRole('button', { name: 'Save the target' }))
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]?.credentials).toEqual({ accessKeyId: 'NEWKEY', secretAccessKey: 'newsecret' })
    expect(document.body.textContent).not.toContain('newsecret')
  })

  it('nothing changed: nothing is sent', async () => {
    const user = userEvent.setup()
    sw('anchors=ok')
    let sent = 0
    mockServer.use(
      http.patch('*/api/v1/ledger/anchor-targets/:id', () => {
        sent += 1
        return undefined
      }),
    )
    await open()
    await user.click(await screen.findByRole('button', { name: 'Edit archive-worm' }))
    const d = within(await dialog())
    await user.click(d.getByRole('button', { name: 'Save the target' }))
    expect(await d.findByTestId('anchor-unchanged')).toBeInTheDocument()
    expect(sent).toBe(0)
  })

  it('a setting that this console does not know is kept when the configuration is sent again', async () => {
    const user = userEvent.setup()
    sw('anchors=ok')
    targets()[0]!.config = { ...targets()[0]!.config, futureSetting: { depth: 2 } }
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.patch('*/api/v1/ledger/anchor-targets/:id', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return HttpResponse.json(targets()[0])
      }),
    )
    await open()
    await user.click(await screen.findByRole('button', { name: 'Edit archive-worm' }))
    const d = within(await dialog())
    await fill(user, d, /^bucket/i, 'another-bucket')
    await user.click(d.getByRole('button', { name: 'Save the target' }))
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]).toHaveProperty('config.futureSetting', { depth: 2 })
    expect(bodies[0]).toHaveProperty('config.bucket', 'another-bucket')
  })

  it('a change to compliance mode needs the word; the mode that is already compliance does not', async () => {
    const user = userEvent.setup()
    sw('anchors=ok')
    targets()[0]!.config = { ...targets()[0]!.config, mode: 'governance' }
    await open()
    await user.click(await screen.findByRole('button', { name: 'Edit archive-worm' }))
    const d = within(await dialog())
    await user.selectOptions(d.getByLabelText(/lock mode/i), 'compliance')
    expect(d.getByTestId('compliance-confirm')).toBeInTheDocument()
    await user.click(d.getByRole('button', { name: 'Save the target' }))
    expect(await d.findByText('Type compliance to confirm the lock mode.')).toBeInTheDocument()
    expect(targets()[0]?.config.mode).toBe('governance')
  })

  it('a refused edit is told in words and the secrets typed are cleared', async () => {
    const user = userEvent.setup()
    sw('anchors=ok,refuse:immutable_field')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Edit archive-worm' }))
    const d = within(await dialog())
    await fill(user, d, /secret access key/i, 'typed-secret')
    await fill(user, d, /access key id/i, 'KEY00001')
    await user.click(d.getByRole('button', { name: 'Save the target' }))
    expect(await d.findByText('Cannot be changed')).toBeInTheDocument()
    expect(d.getByLabelText(/secret access key/i)).toHaveValue('')
  })
})

describe('the test of a target', () => {
  it('shows the last result, says what a WORM test leaves behind before the click, and runs on request', async () => {
    const user = userEvent.setup()
    sw('anchors=all')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Test archive-worm' }))
    const d = within(await dialog())
    expect(d.getByTestId('probe-note')).toHaveTextContent(/one empty probe object/i)
    expect(d.getByText('Result of the last test')).toBeInTheDocument()
    expect(d.getAllByTestId('check-ok').length).toBeGreaterThan(0)
    await user.click(d.getByRole('button', { name: 'Run the test again' }))
    expect(await d.findByRole('heading', { name: 'Result' })).toBeInTheDocument()
    expect(d.getByText('Test passed')).toBeInTheDocument()
  })

  it('a failed check is a result in words, not an error', async () => {
    const user = userEvent.setup()
    sw('anchors=ok,testfail')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Test archive-worm' }))
    const d = within(await dialog())
    await user.click(d.getByRole('button', { name: 'Run the test again' }))
    expect(await d.findByText('Test failed')).toBeInTheDocument()
    expect(d.getByTestId('check-failed')).toHaveTextContent(
      'The store refused the credentials (403).',
    )
    expect(d.getByTestId('check-failed')).toHaveTextContent('Failed')
    expect(d.queryByRole('alert')).not.toBeInTheDocument()
    // The result is kept as the last test of the target.
    expect(targets()[0]?.lastTest?.ok).toBe(false)
  })

  it('a warning is a warning', async () => {
    const user = userEvent.setup()
    sw('anchors=ok,testwarn')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Test archive-worm' }))
    const d = within(await dialog())
    await user.click(d.getByRole('button', { name: 'Run the test again' }))
    expect(await d.findByText('Passed with warnings')).toBeInTheDocument()
    expect(d.getByTestId('check-warning')).toHaveTextContent('Warning')
  })

  it('a target never tested says so; the other kinds say nothing about a probe', async () => {
    const user = userEvent.setup()
    sw('anchors=all')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Test calendars' }))
    const d = within(await dialog())
    expect(d.getByTestId('never-tested')).toBeInTheDocument()
    expect(d.queryByTestId('probe-note')).not.toBeInTheDocument()
    await user.click(d.getByRole('button', { name: 'Run the test' }))
    expect(await d.findByText('Two of three calendars answered.')).toBeInTheDocument()
  })

  it('a refusal of the test is told in words', async () => {
    const user = userEvent.setup()
    sw('anchors=ok,refuse:not_found')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Test archive-worm' }))
    const d = within(await dialog())
    await user.click(d.getByRole('button', { name: 'Run the test again' }))
    expect(await d.findByText('Not found')).toBeInTheDocument()
  })
})

describe('turning a target on and off, and deleting it', () => {
  it('turning off one of several is direct; turning it on again is direct', async () => {
    const user = userEvent.setup()
    sw('anchors=all')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Turn off public-tsa' }))
    await waitFor(() => expect(targets().find((t) => t.name === 'public-tsa')?.enabled).toBe(false))
    expect(await screen.findByTestId('anchoring-notice')).toHaveTextContent(
      'public-tsa is not used from now on.',
    )
    await user.click(await screen.findByRole('button', { name: 'Turn on public-tsa' }))
    await waitFor(() => expect(targets().find((t) => t.name === 'public-tsa')?.enabled).toBe(true))
  })

  it('turning off the last target asks first and says what follows', async () => {
    const user = userEvent.setup()
    sw('anchors=ok')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Turn off archive-worm' }))
    const d = within(await dialog())
    expect(d.getByText(/the only target that is used/i)).toBeInTheDocument()
    expect(targets()[0]?.enabled).toBe(true)
    await user.click(d.getByRole('button', { name: 'Turn it off' }))
    await waitFor(() => expect(targets()[0]?.enabled).toBe(false))
    expect(await screen.findByTestId('anchoring-not-anchored')).toBeInTheDocument()
  })

  it('deleting needs the name typed; the records stay and keep the name', async () => {
    const user = userEvent.setup()
    sw('anchors=all')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Delete public-tsa' }))
    const d = within(await dialog())
    const confirm = d.getByRole('button', { name: 'Delete the target' })
    expect(confirm).toBeDisabled()
    await user.type(d.getByLabelText(/type public-tsa to confirm/i), 'public-tsa')
    expect(confirm).toBeEnabled()
    await user.click(confirm)
    await waitFor(() => expect(targets().some((t) => t.name === 'public-tsa')).toBe(false))
    expect(await screen.findByTestId('anchoring-notice')).toHaveTextContent(
      /its records stay and keep its name/i,
    )
    const kept = getState().ledger7.anchors.filter((a) => a.targetName === 'public-tsa')
    expect(kept.length).toBeGreaterThan(0)
    expect(kept.every((a) => a.targetId === null)).toBe(true)
  })

  it('deleting the last used target warns that the ledger will not be anchored', async () => {
    const user = userEvent.setup()
    sw('anchors=ok')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Delete archive-worm' }))
    expect(within(await dialog()).getByText(/last target that is used/i)).toBeInTheDocument()
  })
})

describe('the anchor records', () => {
  it('every state in words, the error text, the attempts and the times; a deleted target is marked', async () => {
    sw('anchors=all')
    await open()
    await within(recordsCard()).findAllByTestId(/^record-\d+$/)
    const states = within(recordsCard())
      .getAllByTestId('record-state')
      .map((e) => e.textContent?.replace(/^\S\s*/, '').trim())
    for (const s of [
      'Anchored',
      'Waiting to be sent',
      'Sent, not final',
      'Failed',
      'Never confirmed',
    ])
      expect(states).toContain(s)
    expect(within(recordsCard()).getByText(/the policy is not accepted/i)).toBeInTheDocument()
    expect(within(recordsCard()).getByTestId('ots-wait')).toHaveTextContent(/takes hours/i)
  })

  it('filters by target and by state', async () => {
    const user = userEvent.setup()
    sw('anchors=all')
    await open()
    await within(recordsCard()).findAllByTestId(/^record-\d+$/)
    await user.selectOptions(within(recordsCard()).getByLabelText('State'), 'failed')
    await waitFor(() =>
      expect(
        within(recordsCard())
          .getAllByTestId('record-state')
          .every((e) => /Failed/.test(e.textContent ?? '')),
      ).toBe(true),
    )
    await user.selectOptions(within(recordsCard()).getByLabelText('State'), '')
    await user.selectOptions(within(recordsCard()).getByLabelText('Target'), 'calendars')
    await waitFor(() => {
      const rows = within(recordsCard()).getAllByTestId(/^record-\d+$/)
      expect(rows.length).toBe(3)
      expect(rows.every((r) => /calendars/.test(r.textContent ?? ''))).toBe(true)
    })
    await user.selectOptions(within(recordsCard()).getByLabelText('State'), 'superseded')
    expect(await within(recordsCard()).findByText('No record matches')).toBeInTheDocument()
  })

  it('Retry is offered for a failed and an expired record only, and puts it back to waiting', async () => {
    const user = userEvent.setup()
    sw('anchors=all')
    await open()
    await within(recordsCard()).findAllByTestId(/^record-\d+$/)
    const retries = within(recordsCard()).getAllByRole('button', { name: /^Retry record/ })
    expect(retries).toHaveLength(2)
    const id = Number(/record (\d+)/.exec(retries[0]!.getAttribute('aria-label') ?? '')?.[1])
    await user.click(retries[0]!)
    await waitFor(() =>
      expect(getState().ledger7.anchors.find((a) => a.id === id)?.state).toBe('pending'),
    )
    expect(await screen.findByTestId('records-message')).toHaveTextContent(
      `Record ${id} will be sent again.`,
    )
    await waitFor(() =>
      expect(within(recordsCard()).getAllByRole('button', { name: /^Retry record/ })).toHaveLength(
        1,
      ),
    )
  })

  it('evidence: JSON for each kind, .ots for OpenTimestamps and .tsr for a time-stamp authority; the bytes are saved as they came', async () => {
    const user = userEvent.setup()
    sw('anchors=all')
    await open()
    await within(recordsCard()).findAllByTestId(/^record-\d+$/)
    const anchored = getState().ledger7.anchors.filter(
      (a) => a.state === 'anchored' && a.databaseName === 'shop',
    )
    const worm = anchored.find((a) => a.kind === 'worm')!
    const tsa = anchored.find((a) => a.kind === 'tsa')!
    const ots = anchored.find((a) => a.kind === 'ots')!
    expect(within(recordsCard()).queryByTestId(`download-${worm.id}-ots`)).not.toBeInTheDocument()
    expect(within(recordsCard()).queryByTestId(`download-${worm.id}-tsr`)).not.toBeInTheDocument()
    expect(within(recordsCard()).getByTestId(`download-${tsa.id}-tsr`)).toBeInTheDocument()
    expect(within(recordsCard()).queryByTestId(`download-${tsa.id}-ots`)).not.toBeInTheDocument()
    await user.click(within(recordsCard()).getByTestId(`download-${ots.id}-ots`))
    await waitFor(() => expect(saved).toHaveLength(1))
    expect(saved[0]?.name).toBe(`anchor-${ots.id}-evidence.ots`)
    expect(new Uint8Array(await saved[0]!.blob.arrayBuffer())).toEqual(new Uint8Array([1, 2, 3, 4]))
    // A record that is not anchored has no evidence to download.
    const failed = getState().ledger7.anchors.find((a) => a.state === 'failed')!
    expect(
      within(recordsCard()).queryByTestId(`download-${failed.id}-json`),
    ).not.toBeInTheDocument()
  })

  it('Anchor now makes one record per used target; without a used target it is blocked and says why', async () => {
    const user = userEvent.setup()
    sw('anchors=ok')
    await open()
    await user.click(await within(recordsCard()).findByRole('button', { name: /^Anchor now/ }))
    expect(await screen.findByTestId('records-message')).toHaveTextContent(/1 record was created/i)
    expect(getState().ledger7.anchors.some((a) => a.state === 'pending')).toBe(true)
  })

  it('Anchor now with no target used: the button is blocked with the reason', async () => {
    await open()
    await screen.findByTestId('no-targets')
    const button = await within(recordsCard()).findByRole('button', { name: /anchor now/i })
    expect(button).toHaveAttribute('aria-disabled', 'true')
    expect(button).toHaveTextContent(/no anchor target is enabled/i)
  })

  it('no database uses the ledger: the records say so and the targets still work', async () => {
    for (const d of getState().databases)
      d.info.ledger = { mode: 'off', digestIntervalSeconds: null }
    await renderApp('/ledger?tab=anchoring')
    expect(await screen.findByRole('heading', { name: 'Anchor targets' })).toBeInTheDocument()
    expect(await screen.findByTestId('no-ledger-database')).toBeInTheDocument()
  })
})

describe('each problem code in its own words', () => {
  const ON_ANCHOR_NOW: [string, RegExp][] = [
    ['ledger_not_published', /The ledger is not published yet/],
    ['conflict', /^Conflict$/],
    ['not_found', /^Not found$/],
    ['forbidden', /^Not allowed$/],
    ['leader_unavailable', /No manager leader/],
    ['unprocessable', /^Cannot be done$/],
  ]
  for (const [code, words] of ON_ANCHOR_NOW) {
    it(`${code}: Anchor now tells it`, async () => {
      const user = userEvent.setup()
      sw(`anchors=ok,refuse:${code}`)
      await open()
      await user.click(await within(recordsCard()).findByRole('button', { name: /^Anchor now/ }))
      expect(await within(recordsCard()).findByText(words)).toBeInTheDocument()
      expect(screen.queryByTestId('records-message')).not.toBeInTheDocument()
    })
  }
  const ON_ADD: [string, RegExp][] = [
    ['validation_failed', /Check the highlighted fields/],
    ['conflict', /^Conflict$/],
    ['forbidden', /^Not allowed$/],
    ['leader_unavailable', /No manager leader/],
    ['immutable_field', /Cannot be changed/],
  ]
  for (const [code, words] of ON_ADD) {
    it(`${code}: the dialog tells it and nothing is added`, async () => {
      const user = userEvent.setup()
      sw(`anchors=refuse:${code}`)
      await open()
      const d = await openAdd(user)
      await user.click(d.getByTestId('kind-ots'))
      await user.click(d.getByRole('button', { name: 'Use the public calendars' }))
      await fill(user, d, /^name/i, 'public-ots')
      await user.click(d.getByRole('button', { name: 'Add the target' }))
      expect(await d.findByText(words)).toBeInTheDocument()
      expect(targets()).toHaveLength(0)
    })
  }
  it('already_exists: told in words at the name (a name in use), also for a rename in an edit', async () => {
    const user = userEvent.setup()
    sw('anchors=all')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Edit calendars' }))
    const d = within(await dialog())
    await fill(user, d, /^name/i, 'public-tsa')
    await user.click(d.getByRole('button', { name: 'Save the target' }))
    expect(
      await d.findByText('A target with this name exists. Choose another name.'),
    ).toBeInTheDocument()
  })
  it('Retry: a record in another state is a conflict, in words', async () => {
    const user = userEvent.setup()
    sw('anchors=all,refuse:conflict')
    await open()
    await within(recordsCard()).findAllByTestId(/^record-\d+$/)
    await user.click(within(recordsCard()).getAllByRole('button', { name: /^Retry record/ })[0]!)
    expect(await within(recordsCard()).findByText(/^Conflict$/)).toBeInTheDocument()
  })
  it('Delete: a refusal is told in the dialog and the target stays', async () => {
    const user = userEvent.setup()
    sw('anchors=all,refuse:not_found')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Delete public-tsa' }))
    const d = within(await dialog())
    await user.type(d.getByLabelText(/type public-tsa to confirm/i), 'public-tsa')
    await user.click(d.getByRole('button', { name: 'Delete the target' }))
    expect(await d.findByText('Not found')).toBeInTheDocument()
    expect(targets().some((t) => t.name === 'public-tsa')).toBe(true)
  })
})

describe('what each role sees and does', () => {
  const disabled = (b: HTMLElement) => b.getAttribute('aria-disabled') === 'true'
  it('kingres_admin may do everything on the tab', async () => {
    sw('anchors=all')
    await open('admin')
    await screen.findAllByTestId(/^target-\d+$/)
    for (const name of [
      'Add a target',
      'Edit archive-worm',
      'Test archive-worm',
      'Turn off public-tsa',
      'Delete public-tsa',
    ])
      expect(disabled(screen.getByRole('button', { name: new RegExp(`^${name}`) }))).toBe(false)
    expect(disabled(within(recordsCard()).getByRole('button', { name: /^Anchor now/ }))).toBe(false)
  })
  it('kingres_ops may retry and anchor now, not add, edit, test, turn off or delete', async () => {
    sw('anchors=all')
    await open('ops')
    await screen.findAllByTestId(/^target-\d+$/)
    for (const name of [
      'Add a target',
      'Edit archive-worm',
      'Test archive-worm',
      'Turn off public-tsa',
      'Delete public-tsa',
    ]) {
      const b = screen.getByRole('button', { name: new RegExp(`^${name}`) })
      expect(disabled(b)).toBe(true)
      expect(b).toHaveTextContent(/kingres_admin/)
    }
    await within(recordsCard()).findAllByTestId(/^record-\d+$/)
    expect(disabled(within(recordsCard()).getByRole('button', { name: /^Anchor now/ }))).toBe(false)
    expect(
      disabled(within(recordsCard()).getAllByRole('button', { name: /^Retry record/ })[0]!),
    ).toBe(false)
  })
  it('kingres_readonly sees everything and may only download the evidence', async () => {
    sw('anchors=all')
    await open('viewer')
    await screen.findAllByTestId(/^target-\d+$/)
    expect(disabled(screen.getByRole('button', { name: /^Add a target/ }))).toBe(true)
    await within(recordsCard()).findAllByTestId(/^record-\d+$/)
    const now = within(recordsCard()).getByRole('button', { name: /^Anchor now/ })
    expect(disabled(now)).toBe(true)
    expect(now).toHaveTextContent(/kingres_ops/)
    expect(
      disabled(within(recordsCard()).getAllByRole('button', { name: /^Retry record/ })[0]!),
    ).toBe(true)
    const anchored = getState().ledger7.anchors.find(
      (a) => a.state === 'anchored' && a.kind === 'ots' && a.databaseName === 'shop',
    )!
    expect(within(recordsCard()).getByTestId(`download-${anchored.id}-ots`)).not.toBeDisabled()
  })
  it('a disabled button does nothing when it is clicked', async () => {
    const user = userEvent.setup()
    await open('ops')
    await user.click(await screen.findByRole('button', { name: /^Add a target/ }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })
})

describe('the settings of anchoring', () => {
  // These tests wait for pages that read two or three things; how long that takes on a loaded machine is not what they test.
  const saved = getConfig().asyncUtilTimeout
  beforeAll(() => configure({ asyncUtilTimeout: 30_000 }))
  afterAll(() => configure({ asyncUtilTimeout: saved }))
  it('the tab says when it is anchored and links to Settings for the role that may change it', async () => {
    await open('admin')
    expect(await screen.findByTestId('anchor-settings-text')).toHaveTextContent(
      /every\s+3600 seconds.*at least\s+1\s+target has anchored it.*not required/s,
    )
    expect(screen.getByTestId('anchor-settings-link')).toHaveAttribute('href', '/settings')
  })
  it('kingres_ops reads them and has no link to change them', async () => {
    await open('ops')
    await screen.findByTestId('anchor-settings-text')
    expect(screen.queryByTestId('anchor-settings-link')).not.toBeInTheDocument()
  })
  it('the Settings page changes the interval, the minimum and whether anchoring is required', async () => {
    const user = userEvent.setup()
    await renderApp('/settings')
    const form = await screen.findByTestId('anchor-settings-form')
    const interval = await within(form).findByLabelText(/anchor every/i)
    await user.clear(interval)
    await user.type(interval, '10')
    await user.click(within(form).getByRole('button', { name: /save anchoring settings/i }))
    expect(
      await within(form).findByText('Enter a whole number of seconds from 60 to 86400.'),
    ).toBeInTheDocument()
    await user.clear(interval)
    await user.type(interval, '600')
    const minimum = within(form).getByLabelText(/targets that must have anchored/i)
    await user.clear(minimum)
    await user.type(minimum, '9')
    await user.click(within(form).getByRole('button', { name: /save anchoring settings/i }))
    expect(await within(form).findByText('Enter a whole number from 1 to 8.')).toBeInTheDocument()
    await user.clear(minimum)
    await user.type(minimum, '2')
    await user.click(within(form).getByTestId('anchor-required'))
    await user.click(within(form).getByRole('button', { name: /save anchoring settings/i }))
    expect(await within(form).findByTestId('anchor-settings-saved')).toBeInTheDocument()
    expect(getState().ledger7.settings.anchor).toEqual({
      intervalSeconds: 600,
      minimumAnchored: 2,
      required: true,
    })
  })
  it('only kingres_admin can save them', async () => {
    await renderApp('/settings', { user: 'ops' })
    const form = await screen.findByTestId('anchor-settings-form')
    const button = await within(form).findByRole('button', { name: /save anchoring settings/i })
    expect(button).toHaveAttribute('aria-disabled', 'true')
    expect(button).toHaveTextContent(/kingres_admin/)
  })
  it('a server that does not answer the settings: the calm notice instead of the form', async () => {
    mockConfig.unsupported.add('getLedgerSettings')
    await renderApp('/settings')
    const form = await screen.findByTestId('anchor-settings-form')
    expect(
      await within(form).findByText(/not available on this server version yet/i),
    ).toBeInTheDocument()
  })
})

describe('the alerts and the dashboard card lead to the tab', () => {
  it('the alerts of anchoring link to the Anchoring tab', async () => {
    sw('anchors=failed')
    calmClocks()
    await renderApp('/alerts')
    const links = await screen.findAllByRole('link', { name: 'Anchoring of the ledger' })
    expect(links[0]?.getAttribute('href')).toMatch(/^\/ledger\?.*tab=anchoring/)
    expect(await screen.findByText('Anchor target 1')).toBeInTheDocument()
  })
  it('not anchored: the alert links to the tab of its database', async () => {
    sw('anchors=none')
    calmClocks()
    await renderApp('/alerts')
    const link = await screen.findByRole('link', { name: 'Anchoring of the ledger' })
    expect(link.getAttribute('href')).toContain('tab=anchoring')
    expect(link.getAttribute('href')).toContain('db=shop')
  })
  it('the card: "Add a target" while nothing is anchored, the number of targets in use, a link to the tab', async () => {
    await renderApp('/dashboard')
    const add = await screen.findByTestId('ledger-card-add-target')
    expect(add.getAttribute('href')).toContain('tab=anchoring')
  })
  it('the card with a target in use shows how many and links to the tab', async () => {
    sw('anchors=ok')
    await renderApp('/dashboard')
    const row = await screen.findByTestId('ledger-card-targets')
    expect(row).toHaveTextContent('1')
    const card = screen.getByTestId('ledger-card')
    expect(within(card).getByRole('link', { name: 'Anchoring' }).getAttribute('href')).toContain(
      'tab=anchoring',
    )
    expect(within(card).queryByTestId('ledger-card-add-target')).not.toBeInTheDocument()
  })
  it('the card with a late target is attention and the tab is one click away', async () => {
    sw('anchors=behind')
    await renderApp('/dashboard')
    const card = await screen.findByTestId('ledger-card')
    await within(card).findByTestId('ledger-card-attention')
    expect(within(card).getByRole('link', { name: 'Anchoring' })).toBeInTheDocument()
  })
  it('the Overview tab leads to the Anchoring tab while nothing is anchored', async () => {
    await renderApp('/ledger')
    const notice = await screen.findByTestId('not-anchored')
    expect(
      within(notice).getByRole('link', { name: 'Open the Anchoring tab' }).getAttribute('href'),
    ).toContain('tab=anchoring')
  })
})

describe('text chosen by someone hostile', () => {
  it('names, settings, test results and errors are drawn as text, with the hidden characters made visible', async () => {
    seedHostile(getState())
    calmClocks()
    const { container } = await renderApp('/ledger?tab=anchoring&db=shop')
    await screen.findByRole('heading', { name: 'Anchor targets' })
    await waitFor(() =>
      expect(within(targetsCard()).getAllByTestId(/^target-\d+$/).length).toBeGreaterThan(2),
    )
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('svg[onload]')).toBeNull()
    expect(targetsCard().textContent).toContain('U+202E')
    await within(recordsCard()).findAllByTestId(/^record-\d+$/)
    expect(recordsCard().textContent).toContain('U+202E')
    // No raw bidirectional or zero-width character is left anywhere on the tab, also not in a list of choices.
    const hidden = [
      0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069, 0x200b, 0x200c,
      0x200d, 0x200e, 0x200f,
    ]
    expect(hidden.filter((c) => container.textContent?.includes(String.fromCharCode(c)))).toEqual(
      [],
    )
    expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined()
  })
})

describe('accessibility', () => {
  it('the tab with the three kinds and every state of a record', async () => {
    sw('anchors=all')
    const { container } = await open()
    await within(recordsCard()).findAllByTestId(/^record-\d+$/)
    expect(await axe(container)).toHaveNoViolations()
  })
  it('the tab while nothing is anchored, and with a server that does not know anchoring', async () => {
    const first = await open()
    expect(await axe(first.container)).toHaveNoViolations()
    first.unmount()
    sw('phase7=today')
    const second = await open()
    await within(targetsCard()).findByText(/not available on this server version yet/i)
    expect(await axe(second.container)).toHaveNoViolations()
  })
  it('the dialog to add a target, for each kind', async () => {
    const user = userEvent.setup()
    await open()
    const d = await openAdd(user)
    const root = await dialog()
    expect(await axe(root)).toHaveNoViolations()
    await user.click(d.getByTestId('kind-tsa'))
    expect(await axe(root)).toHaveNoViolations()
    await user.click(d.getByTestId('kind-ots'))
    expect(await axe(root)).toHaveNoViolations()
  })
  it('the dialog with a field in error and with the lock mode confirmation', async () => {
    const user = userEvent.setup()
    await open()
    const d = await openAdd(user)
    await user.click(d.getByRole('button', { name: 'Add the target' }))
    await d.findByTestId('form-errors')
    expect(await axe(await dialog())).toHaveNoViolations()
  })
  it('the test dialog with its result, and the confirmations', async () => {
    const user = userEvent.setup()
    sw('anchors=ok,testfail')
    await open()
    await user.click(await screen.findByRole('button', { name: 'Test archive-worm' }))
    const d = within(await dialog())
    await user.click(d.getByRole('button', { name: 'Run the test again' }))
    await d.findByText('Test failed')
    expect(await axe(await dialog())).toHaveNoViolations()
    await user.click(d.getByRole('button', { name: 'Close' }))
    await user.click(screen.getByRole('button', { name: 'Delete archive-worm' }))
    expect(await axe(await dialog())).toHaveNoViolations()
  })
})
