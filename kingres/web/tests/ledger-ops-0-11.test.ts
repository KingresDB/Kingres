// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Console D of the Ledger page (contract 0.11.0 and 0.12.0): the bundle dialog and its download, the acknowledgement of a diverged
// chain (what it means for the history, said before anything is typed), the audit of the publication, the verification with
// `checks`, the alert of a diverged chain, each refusal in words, the roles, hostile text and axe.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { axe } from 'vitest-axe'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { ApiError } from '@/api/problem'
import { bundleIdOf, opsRefusal, verifyBody } from '@/lib/ledger-ops'
import { calmClocks, renderApp } from './helpers'

const sw = (q: string) => applySwitches(new URLSearchParams(q), getState())
type User = ReturnType<typeof userEvent.setup>
const dialog = () => screen.findByRole('dialog')

const saved: { name: string; blob: Blob }[] = []
function captureDownloads(): void {
  saved.length = 0
  URL.createObjectURL = (blob: Blob) => {
    saved.push({ name: '', blob })
    return `blob:test/${saved.length}`
  }
  URL.revokeObjectURL = () => undefined
  HTMLAnchorElement.prototype.click = function (this: HTMLAnchorElement) {
    const last = saved[saved.length - 1]
    if (last) last.name = this.download
  }
}

async function openBundle(user: User, user_ = 'admin') {
  await renderApp('/ledger?tab=digests&db=shop', { user: user_ })
  await user.click(await screen.findByRole('button', { name: /^Create a bundle/ }))
  return within(await dialog())
}

describe('the bundle', () => {
  it('says what it is, what a proof reveals, and creates it with the options chosen', async () => {
    const user = userEvent.setup()
    sw('publication=ok')
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.post('*/api/v1/databases/:db/ledger/bundles', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    const d = await openBundle(user)
    expect(d.getByTestId('bundle-text')).toHaveTextContent(/kingres-verify.*kept for 24 hours/s)
    expect(d.queryByTestId('proofs-warning')).not.toBeInTheDocument()
    await user.click(d.getByTestId('bundle-proof'))
    expect(d.getByTestId('proofs-warning')).toHaveTextContent(/shows the hash of the row/i)
    await user.click(d.getByRole('button', { name: 'Create the bundle' }))
    expect(await d.findByText('Name the table.')).toBeInTheDocument()
    expect(d.getByText(/primary key as a JSON object/)).toBeInTheDocument()
    expect(bodies).toHaveLength(0)
    await user.type(d.getByLabelText(/^table/i), 'orders')
    await user.type(d.getByLabelText(/primary key/i), '{{"id": 7}')
    await user.type(d.getByLabelText(/only the part after/i), '12')
    await user.click(d.getByRole('button', { name: 'Create the bundle' }))
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]).toMatchObject({
      includeAnchors: true,
      sinceSize: 12,
      proofs: [{ schema: 'public', table: 'orders', primaryKey: { id: 7 } }],
    })
    expect(bodies[0]).not.toHaveProperty('logs')
  })

  it('when it is done the bundle is downloaded as the bytes the server sent', async () => {
    const user = userEvent.setup()
    captureDownloads()
    const d = await openBundle(user)
    await user.click(d.getByRole('button', { name: 'Create the bundle' }))
    expect(await d.findByTestId('bundle-ready')).toHaveTextContent(/kept for 24 hours/)
    await user.click(d.getByTestId('bundle-download'))
    await waitFor(() => expect(saved).toHaveLength(1))
    expect(saved[0]?.name).toMatch(/^ledger-bundle-\d+\.tar\.gz$/)
    expect(new Uint8Array(await saved[0]!.blob.arrayBuffer())).toEqual(
      new Uint8Array([0x1f, 0x8b, 8, 0]),
    )
  })

  it('a bundle that has expired is told in words (410)', async () => {
    const user = userEvent.setup()
    captureDownloads()
    sw('bundle=expired')
    const d = await openBundle(user)
    await user.click(d.getByRole('button', { name: 'Create the bundle' }))
    await user.click(await d.findByTestId('bundle-download'))
    expect(await d.findByText('The bundle has expired')).toBeInTheDocument()
    expect(saved).toHaveLength(0)
  })

  const refusals: [string, RegExp][] = [
    ['ledger_not_published', /The ledger is not published yet/],
    ['not_found', /^Not found$/],
    ['forbidden', /^Not allowed$/],
    ['unsupported_operation', /Not available on this server version yet/],
  ]
  for (const [code, words] of refusals)
    it(`${code}: creating a bundle tells it`, async () => {
      const user = userEvent.setup()
      sw(`bundle=refuse:${code}`)
      const d = await openBundle(user)
      await user.click(d.getByRole('button', { name: 'Create the bundle' }))
      expect(await d.findByText(words)).toBeInTheDocument()
    })

  it('kingres_ops may create a bundle, kingres_readonly sees the button disabled with the reason', async () => {
    const user = userEvent.setup()
    await renderApp('/ledger?tab=digests&db=shop', { user: 'viewer' })
    const b = await screen.findByRole('button', { name: /^Create a bundle/ })
    expect(b).toHaveAttribute('aria-disabled', 'true')
    expect(b).toHaveTextContent(/kingres_ops/)
    void user
  })

  it('?phase7=today: the bundle is not available, nothing changed', async () => {
    const user = userEvent.setup()
    sw('phase7=today')
    const d = await openBundle(user)
    await user.click(d.getByRole('button', { name: 'Create the bundle' }))
    expect(await d.findByText(/not available on this server version yet/i)).toBeInTheDocument()
    expect(getState().ledger7.bundles).toHaveLength(0)
  })
})

describe('the acknowledgement of a diverged chain', () => {
  async function openAck(user: User, u = 'admin') {
    await renderApp('/ledger?db=shop', { user: u })
    await user.click(
      await screen.findByRole('button', { name: /^Acknowledge the divergence of shard group 2/ }),
    )
    return within(await dialog())
  }
  it('is offered only for a chain that diverged, with the words about the history', async () => {
    const user = userEvent.setup()
    await renderApp('/ledger?db=shop')
    await screen.findByText(/status of shop/i)
    expect(screen.queryByTestId('diverged-chains')).not.toBeInTheDocument()
    sw('publication=diverged')
    const d = await openAck(user)
    const text = d.getByTestId('ack-text').textContent ?? ''
    expect(text).toMatch(/does not repair anything and does not hide anything/)
    expect(text).toMatch(/old log is closed as it is/)
    expect(text).toMatch(/part of the history/)
    expect(text).toMatch(/cannot be undone/)
  })
  it('needs what happened, a reason and the name of the database typed; then a new epoch starts', async () => {
    const user = userEvent.setup()
    sw('publication=diverged')
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.post('*/api/v1/databases/:db/ledger/chains/:g/acknowledge', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    const d = await openAck(user)
    const go = d.getByRole('button', { name: 'Acknowledge and start a new epoch' })
    expect(go).toBeDisabled()
    await user.type(d.getByLabelText(/to confirm/i), 'shop')
    await user.click(go)
    expect(await d.findByText('Choose what happened.')).toBeInTheDocument()
    expect(d.getByText('Give a reason of 1 to 200 characters.')).toBeInTheDocument()
    expect(bodies).toHaveLength(0)
    await user.selectOptions(d.getByLabelText(/what happened/i), 'restore')
    await user.type(d.getByLabelText(/^reason/i), 'Restored from the backup of Monday.')
    await user.click(go)
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]).toEqual({
      kind: 'restore',
      reason: 'Restored from the backup of Monday.',
      confirm: 'shop',
    })
    await waitFor(() => expect(getState().ledger7.diverged).not.toContain('shop:2'), {
      timeout: 8000,
    })
  })
  it('chain_not_diverged and validation_failed are told in their own words', async () => {
    const user = userEvent.setup()
    sw('publication=diverged')
    const d = await openAck(user)
    getState().ledger7.diverged = [] // someone else acknowledged it meanwhile
    await user.type(d.getByLabelText(/to confirm/i), 'shop')
    await user.selectOptions(d.getByLabelText(/what happened/i), 'other')
    await user.type(d.getByLabelText(/^reason/i), 'x')
    await user.click(d.getByRole('button', { name: 'Acknowledge and start a new epoch' }))
    expect(await d.findByText('This chain has not diverged')).toBeInTheDocument()
  })
  it('only kingres_admin may acknowledge', async () => {
    sw('publication=diverged')
    await renderApp('/ledger?db=shop', { user: 'ops' })
    const b = await screen.findByRole('button', {
      name: /^Acknowledge the divergence of shard group 2/,
    })
    expect(b).toHaveAttribute('aria-disabled', 'true')
    expect(b).toHaveTextContent(/kingres_admin/)
  })
  it('the alert of a closed log that answers again names the group, the height and why; its link leads to the Overview', async () => {
    sw('publication=reopened')
    calmClocks()
    await renderApp('/alerts')
    expect((await screen.findAllByText(/closed at height 120/)).length).toBeGreaterThan(0)
    expect(await screen.findByTestId('alert-diverged')).toHaveTextContent(
      /shard group, the height and why the log ended/,
    )
    const link = await screen.findByRole('link', {
      name: 'Acknowledge the divergence on the Overview tab',
    })
    expect(link.getAttribute('href')).toContain('db=shop')
    expect(link.getAttribute('href')).not.toContain('tab=')
  })
})

describe('the verification with checks and the audit of the publication', () => {
  it('the checks are chosen; the chain alone is the default and sends no checks; none chosen blocks the start', async () => {
    const user = userEvent.setup()
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.post('*/api/v1/databases/:db/ledger/verifications', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    await renderApp('/ledger?db=shop')
    await screen.findByTestId('verify-checks')
    expect(screen.getByTestId('check-chain')).toBeChecked()
    await user.click(screen.getByRole('button', { name: 'Start verification' }))
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]).not.toHaveProperty('checks')
    await waitFor(
      () => expect(getState().ledger.verifications.every((v) => v.state !== 'running')).toBe(true),
      { timeout: 8000 },
    )
    await user.click(screen.getByTestId('check-publication'))
    await user.click(screen.getByTestId('check-anchors'))
    await user.click(screen.getByRole('button', { name: 'Start verification' }))
    await waitFor(() => expect(bodies).toHaveLength(2), { timeout: 8000 })
    expect(bodies[1]?.checks).toEqual(['chain', 'publication', 'anchors'])
    await user.click(screen.getByTestId('check-chain'))
    await user.click(screen.getByTestId('check-publication'))
    await user.click(screen.getByTestId('check-anchors'))
    const b = screen.getByRole('button', { name: /^Start verification/ })
    expect(b).toHaveAttribute('aria-disabled', 'true')
    expect(b).toHaveTextContent(/at least one thing to check/)
  })
  it('the audit starts a verification of the publication of the chosen database, from block 0', async () => {
    const user = userEvent.setup()
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.post('*/api/v1/databases/:db/ledger/verifications', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    await renderApp('/ledger?tab=publication')
    const card = await screen.findByTestId('publication-audit')
    expect(await within(card).findByTestId('audit-text')).toHaveTextContent(/from block 0/)
    await user.click(await within(card).findByRole('button', { name: 'Audit the publication now' }))
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]).toEqual({ scope: 'full', checks: ['publication'] })
  })
  it('a verification that runs already is told in words; roles: kingres_ops may audit, read-only may not', async () => {
    const user = userEvent.setup()
    mockServer.use(
      http.post('*/api/v1/databases/:db/ledger/verifications', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'x',
            status: 409,
            code: 'operation_in_progress',
            detail: 'd',
          },
          { status: 409 },
        ),
      ),
    )
    const app = await renderApp('/ledger?tab=publication', { user: 'ops' })
    const card = await screen.findByTestId('publication-audit')
    await user.click(await within(card).findByRole('button', { name: 'Audit the publication now' }))
    expect(await within(card).findByText('A verification is already running')).toBeInTheDocument()
    app.unmount()
    await renderApp('/ledger?tab=publication', { user: 'viewer' })
    const b = await within(await screen.findByTestId('publication-audit')).findByRole('button', {
      name: /^Audit the publication now/,
    })
    expect(b).toHaveAttribute('aria-disabled', 'true')
  })
})

describe('accessibility and hostile text', () => {
  it('the bundle dialog, with a proof', async () => {
    const user = userEvent.setup()
    sw('publication=diverged')
    const d = await openBundle(user)
    await user.click(d.getByTestId('bundle-proof'))
    expect(await axe(await dialog())).toHaveNoViolations()
  })
  it('the audit card', async () => {
    const { container } = await renderApp('/ledger?tab=publication')
    await screen.findByTestId('publication-audit')
    expect(await axe(container)).toHaveNoViolations()
  })
  it('the acknowledgement dialog', async () => {
    const user = userEvent.setup()
    sw('publication=diverged')
    await renderApp('/ledger?db=shop')
    await user.click(
      await screen.findByRole('button', { name: /^Acknowledge the divergence of shard group 2/ }),
    )
    expect(await axe(await dialog())).toHaveNoViolations()
  })
})

describe('the words and checks of the library', () => {
  it('the body of a verification and the number of a bundle', () => {
    expect(verifyBody('full', ['chain'])).toEqual({ scope: 'full' })
    expect(verifyBody('full', [])).toEqual({ scope: 'full' })
    expect(verifyBody('sinceLastVerified', ['publication'])).toEqual({
      scope: 'sinceLastVerified',
      checks: ['publication'],
    })
    expect(bundleIdOf('bundle 3')).toBe(3)
    expect(bundleIdOf('made bundle 14 today')).toBe(14)
    expect(bundleIdOf('nothing')).toBeNull()
    expect(bundleIdOf(null)).toBeNull()
  })
  it('the refusals by action', () => {
    const e = (code: string, status = 409) =>
      new ApiError({ type: 'x', title: 'x', status, code: code as never } as never)
    expect(opsRefusal(e('chain_not_diverged'), 'acknowledge')?.heading).toBe(
      'This chain has not diverged',
    )
    expect(opsRefusal(e('ledger_not_published'), 'bundle')?.heading).toBe(
      'The ledger is not published yet',
    )
    expect(opsRefusal(e('not_found', 410), 'download')?.heading).toBe('The bundle has expired')
    expect(opsRefusal(e('not_found', 404), 'download')?.heading).toBe('Not found')
    expect(opsRefusal(e('operation_in_progress'), 'verify')?.heading).toBe(
      'A verification is already running',
    )
    expect(opsRefusal(e('forbidden'), 'bundle')).toBeNull()
    expect(opsRefusal(new Error('x'), 'bundle')).toBeNull()
  })
})
