// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.6.0, changelog Breaking 1 and 2 and Additive 1 to 5 as far as the servers are concerned:
// add-server is administrator-only, the pin is required (or an explicit unpinned choice), the server's
// own fingerprint is shown, the setup task `detail` is the only claim about the pin check, and
// `internal_api_disabled` has its own explanation.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState, resetMockState } from '@/mocks/http'
import { mockConfig, fakeHash } from '@/mocks/state'
import { HOSTILE } from '@/mocks/hostile'
import { endpoints } from '@/api/endpoints'
import { ApiError, describeProblem } from '@/api/problem'
import { operations } from '@/api/operations.generated'
import { groupFingerprint } from '@/lib/fingerprint'
import OwnFingerprint from '@/components/OwnFingerprint.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import { renderApp } from './helpers'

type User = ReturnType<typeof userEvent.setup>
const hex = 'ab'.repeat(32)

async function openAdd(user: User) {
  await user.click(await screen.findByRole('button', { name: /add server/i }))
  const dialog = await screen.findByRole('dialog', { name: /add server/i })
  await user.type(within(dialog).getByLabelText(/server address/i), '10.0.0.14:8091')
  await user.type(within(dialog).getByLabelText(/setup token/i), 'demo-setup-token')
  return dialog
}

async function openJoin(user: User) {
  await user.type(await screen.findByLabelText(/setup token/i), 'demo-setup-token')
  await user.click(screen.getByRole('button', { name: /continue/i }))
  await user.click(await screen.findByRole('button', { name: /^join existing cluster$/i }))
  await user.type(screen.getByLabelText(/address of any existing server/i), '10.0.0.11:8091')
  await user.type(screen.getByLabelText(/administrator password/i), 'demo-password')
}

describe('Breaking 1: addServer needs kingres_admin', () => {
  it('is an administrator operation in the generated table', () => {
    expect(operations.addServer.role).toBe('kingres_admin')
  })

  it('an ops account is not offered add-server: the control is disabled and says which role it needs', async () => {
    const user = userEvent.setup()
    await renderApp('/servers', { user: 'ops' })
    const button = await screen.findByRole('button', { name: /add server/i })
    expect(button).toHaveAttribute('aria-disabled', 'true')
    expect(button).toHaveTextContent(/requires the kingres_admin cluster role/i)
    await user.click(button)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('an administrator can open the dialog', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const button = await screen.findByRole('button', { name: /add server/i })
    expect(button).not.toHaveAttribute('aria-disabled')
    await user.click(button)
    expect(await screen.findByRole('dialog', { name: /add server/i })).toBeInTheDocument()
  })

  it('a 403 from the server for an ops account is explained in plain words', async () => {
    await renderApp('/dashboard', { user: 'ops' })
    const error = await endpoints
      .addServer({ address: '10.0.0.14:8091', setupToken: 'demo-setup-token', allowUnpinned: true })
      .catch((e: unknown) => e)
    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).code).toBe('forbidden')
    expect(getState().servers.some((s) => s.id === '10.0.0.14')).toBe(false)
    render(ProblemNotice, { props: { error } })
    expect(screen.getByRole('alert')).toHaveTextContent(
      /not allowed.*does not have the cluster role needed/i,
    )
  })
})

describe('Breaking 2: the pin of add-server', () => {
  it('has no choice pre-selected and sends nothing without a fingerprint or an explicit choice', async () => {
    const user = userEvent.setup()
    const bodies: unknown[] = []
    mockServer.use(
      http.post('*/api/v1/servers', async ({ request }) => {
        bodies.push(await request.clone().json())
        return undefined
      }),
    )
    await renderApp('/servers')
    const dialog = await openAdd(user)
    expect(within(dialog).getByLabelText(/do not check the certificate/i)).not.toBeChecked()
    await user.click(within(dialog).getByRole('button', { name: /^add server$/i }))
    expect(
      await within(dialog).findByText(/enter the fingerprint of the new server/i),
    ).toBeVisible()
    expect(within(dialog).getByLabelText(/expected fingerprint/i)).toHaveAttribute(
      'aria-invalid',
      'true',
    )
    expect(bodies).toHaveLength(0)
  })

  it('the unpinned choice is worded as what it means and sends allowUnpinned without a pin', async () => {
    const user = userEvent.setup()
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.post('*/api/v1/servers', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    await renderApp('/servers')
    const dialog = await openAdd(user)
    await user.click(within(dialog).getByLabelText(/do not check the certificate/i))
    expect(within(dialog).getByTestId('unpinned-warning')).toHaveTextContent(
      /whatever answers there is trusted/i,
    )
    expect(within(dialog).getByLabelText(/expected fingerprint/i)).toBeDisabled()
    await user.click(within(dialog).getByRole('button', { name: /^add server$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    expect(bodies[0]).toMatchObject({ allowUnpinned: true })
    expect(bodies[0]).not.toHaveProperty('expectedFingerprint')
  })

  it('a fingerprint is the normal path and does not send allowUnpinned', async () => {
    const user = userEvent.setup()
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.post('*/api/v1/servers', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    await renderApp('/servers')
    const dialog = await openAdd(user)
    await user.type(
      within(dialog).getByLabelText(/expected fingerprint/i),
      fakeHash('bootstrap-10.0.0.14'),
    )
    await user.click(within(dialog).getByRole('button', { name: /^add server$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    expect(bodies[0]).toMatchObject({ expectedFingerprint: fakeHash('bootstrap-10.0.0.14') })
    expect(bodies[0]).not.toHaveProperty('allowUnpinned')
  })

  it("tells the administrator to read the fingerprint on the new server's own panel", async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openAdd(user)
    await user.click(within(dialog).getByText(/where do i find the fingerprint/i))
    expect(dialog).toHaveTextContent(/setup wizard shows the fingerprint/i)
  })

  it('maps the server 400 at /expectedFingerprint onto the field', async () => {
    const user = userEvent.setup()
    mockServer.use(
      http.post('*/api/v1/servers', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Validation failed',
            status: 400,
            code: 'validation_failed',
            errors: [
              { field: '/expectedFingerprint', code: 'required', message: 'A pin is required.' },
            ],
          },
          { status: 400, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    )
    await renderApp('/servers')
    const dialog = await openAdd(user)
    await user.type(within(dialog).getByLabelText(/expected fingerprint/i), hex)
    await user.click(within(dialog).getByRole('button', { name: /^add server$/i }))
    const field = within(dialog).getByLabelText(/expected fingerprint/i)
    await waitFor(() => expect(field).toHaveAttribute('aria-invalid', 'true'))
    expect(field.closest('.field')).toHaveTextContent('A pin is required.')
  })

  it('the mock answers like the contract: no pin is 400 at /expectedFingerprint, code required', async () => {
    await renderApp('/dashboard')
    const error = await endpoints
      .addServer({ address: '10.0.0.14:8091', setupToken: 'demo-setup-token' })
      .catch((e: unknown) => e)
    expect(error).toBeInstanceOf(ApiError)
    const problem = (error as ApiError).problem
    expect(problem.status).toBe(400)
    expect(problem.errors?.[0]).toMatchObject({ field: '/expectedFingerprint', code: 'required' })
    expect(describeProblem(error).fieldErrors['/expectedFingerprint']).toBeTruthy()
    await expect(
      endpoints.addServer({
        address: '10.0.0.14:8091',
        setupToken: 'demo-setup-token',
        allowUnpinned: true,
      }),
    ).resolves.toMatchObject({ kind: 'server.add' })
  })
})

describe('Breaking 2: the pin of join-cluster', () => {
  it('has no choice pre-selected and blocks the submit without a fingerprint', async () => {
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await openJoin(user)
    expect(screen.getByLabelText(/do not check the certificate/i)).not.toBeChecked()
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))
    expect(await screen.findByText(/enter the fingerprint of the existing server/i)).toBeVisible()
    expect(getState().node.state).toBe('uninitialized')
  })

  it('says in plain words that the administrator password is sent to whatever answers', async () => {
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.post('*/api/v1/setup/join-cluster', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    await renderApp('/', { user: null })
    await openJoin(user)
    await user.click(screen.getByLabelText(/do not check the certificate/i))
    expect(screen.getByTestId('unpinned-warning')).toHaveTextContent(
      /whatever answers there is trusted, and the administrator password you type here is sent to it/i,
    )
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))
    expect(await screen.findByRole('button', { name: /continue to sign in/i })).toBeInTheDocument()
    expect(bodies[0]).toMatchObject({ allowUnpinned: true })
    expect(bodies[0]).not.toHaveProperty('expectedFingerprint')
  })

  it('the mock refuses a join without pin and a malformed pin with a 400 at /expectedFingerprint', async () => {
    resetMockState({ uninitialized: true })
    const { setSetupToken } = await import('@/api/client')
    setSetupToken('demo-setup-token')
    const base = {
      existingNodeAddress: '10.0.0.11:8091',
      admin: { username: 'admin', password: 'demo-password' },
      dataPaths: { dataDirectory: '/var/lib/kingres' },
    }
    const none = await endpoints.joinCluster(base).catch((e: unknown) => e)
    expect((none as ApiError).problem.errors?.[0]).toMatchObject({
      field: '/expectedFingerprint',
      code: 'required',
    })
    const bad = await endpoints
      .joinCluster({ ...base, expectedFingerprint: 'nope' })
      .catch((e: unknown) => e)
    expect((bad as ApiError).status).toBe(400)
  })
})

describe('Additive 3: the fingerprint of this server', () => {
  function clipboard() {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    return writeText
  }

  it('the wizard shows it grouped after the token, with a copy button that copies the plain digits', async () => {
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    const writeText = clipboard()
    await renderApp('/', { user: null })
    await user.type(await screen.findByLabelText(/setup token/i), 'demo-setup-token')
    await user.click(screen.getByRole('button', { name: /continue/i }))
    const value = await screen.findByTestId('own-fingerprint-value')
    expect(value).toHaveTextContent(groupFingerprint(fakeHash('bootstrap-10.0.0.11')))
    await user.click(screen.getByRole('button', { name: /copy fingerprint/i }))
    expect(writeText).toHaveBeenCalledWith(fakeHash('bootstrap-10.0.0.11'))
    expect(await screen.findByText('Copied.')).toBeInTheDocument()
  })

  it('the wizard keeps the log-line text as the fallback when the server does not report it', async () => {
    resetMockState({ uninitialized: true })
    mockConfig.setupFingerprintVisible = false
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await user.type(await screen.findByLabelText(/setup token/i), 'demo-setup-token')
    await user.click(screen.getByRole('button', { name: /continue/i }))
    expect(await screen.findByTestId('own-fingerprint-fallback')).toHaveTextContent(
      /bootstrap certificate of the internal API/,
    )
    expect(screen.queryByTestId('own-fingerprint-value')).not.toBeInTheDocument()
  })

  it('This server shows it to a read-only account too', async () => {
    await renderApp('/this-server', { user: 'viewer' })
    const section = await screen.findByTestId('this-server-fingerprint')
    expect(within(section).getByTestId('own-fingerprint-value')).toHaveTextContent(
      groupFingerprint(fakeHash('internal-10.0.0.11')),
    )
    expect(within(section).getByRole('button', { name: /copy fingerprint/i })).toBeInTheDocument()
  })

  it('shows the fallback text when the field is null', () => {
    render(OwnFingerprint, {
      props: { fingerprint: null, purpose: 'For others.', fallback: 'Read pki/server.crt.' },
    })
    expect(screen.getByTestId('own-fingerprint-fallback')).toHaveTextContent('Read pki/server.crt.')
    expect(screen.queryByRole('button', { name: /copy/i })).not.toBeInTheDocument()
  })

  it('draws a hostile fingerprint as text', async () => {
    mockConfig.hostileSetupText = HOSTILE.markup
    await renderApp('/this-server')
    const value = await screen.findByTestId('own-fingerprint-value')
    expect(value.querySelector('img')).toBeNull()
    expect(value.textContent).toContain('<img')
  })
})

describe('Additive 4: the join wizard says only what the server reported', () => {
  it('claims nothing when the server reported no text', async () => {
    resetMockState({ uninitialized: true })
    mockConfig.omitSetupDetail = true
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await openJoin(user)
    await user.type(screen.getByLabelText(/expected fingerprint/i), fakeHash('internal-10.0.0.11'))
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))
    const note = await screen.findByTestId('join-fingerprint-note')
    expect(note).toHaveTextContent(/did not report whether the certificate .* was checked/i)
    expect(note).not.toHaveTextContent(/matched|was checked against/i)
  })

  it('draws a hostile detail as text', async () => {
    resetMockState({ uninitialized: true })
    mockConfig.hostileSetupText = HOSTILE.markup
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await openJoin(user)
    await user.click(screen.getByLabelText(/do not check the certificate/i))
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))
    const note = await screen.findByTestId('join-fingerprint-note')
    expect(note.querySelector('img')).toBeNull()
    expect(note.textContent).toContain('<img')
  })
})

describe('Additive 3 (problem code): internal_api_disabled', () => {
  it('is configuration, not a missing feature', () => {
    const view = describeProblem(
      new ApiError({
        type: 'about:blank',
        title: 'x',
        status: 409,
        code: 'internal_api_disabled',
      }),
    )
    expect(view.heading).toMatch(/internal api switched off/i)
    expect(view.message).toMatch(/not a missing feature/i)
    expect(view.message).toMatch(/without its internal API/i)
    expect(view.message).not.toMatch(/not implemented|version yet/i)
  })

  it('is shown on add and on remove, and answered by leave, renew and revoke', async () => {
    mockConfig.internalApiDisabled = true
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openAdd(user)
    await user.click(within(dialog).getByLabelText(/do not check the certificate/i))
    await user.click(within(dialog).getByRole('button', { name: /^add server$/i }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      /internal api switched off on this server/i,
    )
    await user.click(within(dialog).getByRole('button', { name: /cancel/i }))

    const row = await screen.findByTestId('server-row-10.0.0.13')
    await user.click(within(row).getByRole('button', { name: /remove/i }))
    const remove = await screen.findByRole('dialog', { name: /remove server/i })
    await user.type(within(remove).getByLabelText(/to confirm/i), '10.0.0.13:8091')
    await user.click(within(remove).getByRole('button', { name: /^remove server$/i }))
    expect(await within(remove).findByRole('alert')).toHaveTextContent(
      /internal api switched off on this server/i,
    )

    await expect(endpoints.leaveCluster(false)).rejects.toMatchObject({
      code: 'internal_api_disabled',
    })
    await expect(endpoints.renewServerCertificates('10.0.0.12')).rejects.toMatchObject({
      code: 'internal_api_disabled',
    })
    await expect(endpoints.revokeServerCertificates('10.0.0.12')).rejects.toMatchObject({
      code: 'internal_api_disabled',
    })
  })
})
