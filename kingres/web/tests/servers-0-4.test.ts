// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.4.0 on the server pages: certificate and fingerprint properties, the expected
// fingerprint of add-server and join, server groups, and the removal rule (no draining).
import { describe, expect, it } from 'vitest'
import { screen, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState, resetMockState } from '@/mocks/http'
import { fakeHash } from '@/mocks/state'
import { endpoints } from '@/api/endpoints'
import {
  certificateState,
  fingerprintError,
  groupFingerprint,
  normalizeFingerprint,
} from '@/lib/fingerprint'
import { renderApp } from './helpers'

const colons = (hex: string) => hex.toUpperCase().replace(/(..)(?!$)/g, '$1:')

describe('fingerprint helpers', () => {
  const hex = 'ab'.repeat(32)
  it('accepts 64 hex digits with or without colons and any case, and rejects the rest', () => {
    expect(fingerprintError('')).toBeUndefined()
    expect(fingerprintError(hex)).toBeUndefined()
    expect(fingerprintError(colons(hex))).toBeUndefined()
    expect(fingerprintError(hex.slice(2))).toMatch(/64 hexadecimal/)
    expect(fingerprintError('zz'.repeat(32))).toMatch(/64 hexadecimal/)
    expect(normalizeFingerprint('AB:CD')).toBe('abcd')
    expect(groupFingerprint(hex)).toBe(Array(8).fill('abababab').join(' '))
  })
  it('classifies certificate expiry', () => {
    const now = Date.parse('2026-11-15T00:00:00Z')
    const at = (days: number) => new Date(now + days * 86_400_000).toISOString()
    expect(certificateState(null, now)).toBe('unknown')
    expect(certificateState(at(-1), now)).toBe('expired')
    expect(certificateState(at(29), now)).toBe('expiring')
    expect(certificateState(at(31), now)).toBe('ok')
  })
})

describe('certificate and fingerprint on the server pages', () => {
  it('flags a certificate that expires soon in the list, and only that one', async () => {
    await renderApp('/servers')
    const soon = await screen.findByTestId('server-row-10.0.0.13')
    expect(within(soon).getByTestId('cert-flag')).toHaveTextContent(/expires within 30 days/i)
    expect(
      within(screen.getByTestId('server-row-10.0.0.12')).queryByTestId('cert-flag'),
    ).not.toBeInTheDocument()
  })

  it('shows a null certificate end as not available', async () => {
    getState().servers[1]!.certificateExpiresAt = null
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.12')
    expect(within(row).getAllByText('Not available').length).toBeGreaterThan(0)
  })

  it('says on the detail page whether the fingerprint is pinned, not pinned or absent', async () => {
    await renderApp('/servers/10.0.0.12')
    expect(await screen.findByTestId('fingerprint-state')).toHaveTextContent(/pinned: checked/i)
    expect(screen.getByTestId('fingerprint')).toHaveTextContent(
      groupFingerprint(fakeHash('bootstrap-12')),
    )
  })

  it('says "not pinned" when the fingerprint was only seen', async () => {
    await renderApp('/servers/10.0.0.13')
    expect(await screen.findByTestId('fingerprint-state')).toHaveTextContent(/not pinned/i)
  })

  it('says that no first contact is recorded for the creator of the cluster', async () => {
    await renderApp('/servers/10.0.0.11')
    expect(await screen.findByTestId('fingerprint-state')).toHaveTextContent(
      /no first contact recorded/i,
    )
  })
})

async function openAdd(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: /add server/i }))
  const dialog = await screen.findByRole('dialog', { name: /add server/i })
  await user.type(within(dialog).getByLabelText(/server address/i), '10.0.0.14:8091')
  await user.type(within(dialog).getByLabelText(/setup token/i), 'demo-setup-token')
  return dialog
}

describe('add server: expected fingerprint', () => {
  it('explains where to read the fingerprint and what it protects against', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openAdd(user)
    await user.click(within(dialog).getByText(/where do i find the fingerprint/i))
    expect(dialog).toHaveTextContent(/bootstrap certificate of the internal API/)
    expect(dialog).toHaveTextContent(/pki\/bootstrap\.crt/)
    expect(dialog).toHaveTextContent(/refused before anything secret is sent/i)
  })

  it('rejects a malformed fingerprint before sending', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openAdd(user)
    await user.type(within(dialog).getByLabelText(/expected fingerprint/i), 'not-a-fingerprint')
    await user.click(within(dialog).getByRole('button', { name: /^add server$/i }))
    expect(await within(dialog).findByText(/64 hexadecimal digits/i)).toBeInTheDocument()
    expect(getState().servers.some((s) => s.id === '10.0.0.14')).toBe(false)
  })

  it('sends the fingerprint in the body and shows it as pinned afterwards', async () => {
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
    const good = fakeHash('bootstrap-10.0.0.14')
    await user.type(within(dialog).getByLabelText(/expected fingerprint/i), colons(good))
    await user.click(within(dialog).getByRole('button', { name: /^add server$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    // The panel sends the normalized form: 64 lower-case hex digits, no colons (W2-04).
    expect(bodies[0]).toMatchObject({ expectedFingerprint: good })
    expect(await within(dialog).findByTestId('fingerprint')).toHaveTextContent(
      groupFingerprint(good),
    )
    expect(within(dialog).getByTestId('fingerprint-state')).toHaveTextContent(/pinned: checked/i)
  })

  it('shows the fingerprint the server saw, as not pinned, when none was given', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openAdd(user)
    await user.click(within(dialog).getByLabelText(/do not check the certificate/i))
    await user.click(within(dialog).getByRole('button', { name: /^add server$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    expect(await within(dialog).findByTestId('fingerprint-state')).toHaveTextContent(/not pinned/i)
    expect(within(dialog).getByTestId('fingerprint')).toHaveTextContent(
      groupFingerprint(fakeHash('bootstrap-10.0.0.14')),
    )
  })

  it('reports a different certificate as a failed task, and adds nothing', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openAdd(user)
    await user.type(within(dialog).getByLabelText(/expected fingerprint/i), 'cd'.repeat(32))
    await user.click(within(dialog).getByRole('button', { name: /^add server$/i }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/not the expected one/i)
    expect(getState().servers.some((s) => s.id === '10.0.0.14')).toBe(false)
  })
})

describe('join cluster: expected fingerprint', () => {
  it('has the field with its help and sends it in the body', async () => {
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    const bodies: unknown[] = []
    mockServer.use(
      http.post('*/api/v1/setup/join-cluster', async ({ request }) => {
        bodies.push(await request.clone().json())
        return undefined
      }),
    )
    await renderApp('/', { user: null })
    await user.type(await screen.findByLabelText(/setup token/i), 'demo-setup-token')
    await user.click(screen.getByRole('button', { name: /continue/i }))
    await user.click(await screen.findByRole('button', { name: /^join existing cluster$/i }))
    await user.click(screen.getByText(/where do i find the fingerprint/i))
    expect(document.body).toHaveTextContent(/pki\/server\.crt/)
    expect(document.body).toHaveTextContent(/administrator's password/i)
    await user.type(screen.getByLabelText(/address of any existing server/i), '10.0.0.11:8091')
    await user.type(screen.getByLabelText(/administrator password/i), 'demo-password')
    await user.type(screen.getByLabelText(/expected fingerprint/i), 'zz')
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))
    expect(await screen.findByText(/64 hexadecimal digits/i)).toBeInTheDocument()
    const fp = fakeHash('internal-10.0.0.11')
    const field = screen.getByLabelText(/expected fingerprint/i)
    await user.clear(field)
    await user.type(field, fp)
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))
    expect(await screen.findByRole('button', { name: /continue to sign in/i })).toBeInTheDocument()
    expect(bodies[0]).toMatchObject({ expectedFingerprint: fp })
    // The only claim about the pin is the text the server reported (0.6.0 `detail`).
    expect(screen.getByTestId('join-fingerprint-note')).toHaveTextContent(
      /what the server reports about the first contact: the certificate of the existing server matched the pinned fingerprint/i,
    )
  })

  it('without a fingerprint, tells the administrator to compare it afterwards', async () => {
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await user.type(await screen.findByLabelText(/setup token/i), 'demo-setup-token')
    await user.click(screen.getByRole('button', { name: /continue/i }))
    await user.click(await screen.findByRole('button', { name: /^join existing cluster$/i }))
    await user.type(screen.getByLabelText(/address of any existing server/i), '10.0.0.11:8091')
    await user.type(screen.getByLabelText(/administrator password/i), 'demo-password')
    await user.click(screen.getByLabelText(/do not check the certificate/i))
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))
    expect(await screen.findByTestId('join-fingerprint-note')).toHaveTextContent(
      /no pin was checked/i,
    )
  })

  it('a different certificate fails the join and the wizard can try again', async () => {
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await user.type(await screen.findByLabelText(/setup token/i), 'demo-setup-token')
    await user.click(screen.getByRole('button', { name: /continue/i }))
    await user.click(await screen.findByRole('button', { name: /^join existing cluster$/i }))
    await user.type(screen.getByLabelText(/address of any existing server/i), '10.0.0.11:8091')
    await user.type(screen.getByLabelText(/administrator password/i), 'demo-password')
    await user.type(screen.getByLabelText(/expected fingerprint/i), 'cd'.repeat(32))
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/not the expected one/i)
    expect(screen.getByRole('button', { name: /back to the settings/i })).toBeInTheDocument()
  })
})

describe('server groups are real', () => {
  it('explains a duplicate name', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    await user.click(await screen.findByRole('button', { name: /create server group/i }))
    const dialog = await screen.findByRole('dialog', { name: /create server group/i })
    await user.type(within(dialog).getByLabelText(/^name/i), 'rack-a')
    await user.click(within(dialog).getByRole('button', { name: /^create server group$/i }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/already exists/i)
  })

  it('names the servers that block the deletion of a group and says what to do', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const row = (await screen.findByRole('rowheader', { name: 'rack-a' })).closest(
      'tr',
    ) as HTMLElement
    await user.click(within(row).getByRole('button', { name: /delete/i }))
    const dialog = await screen.findByRole('dialog', { name: /delete server group rack-a/i })
    await user.click(within(dialog).getByRole('button', { name: /delete group/i }))
    const alert = await within(dialog).findByRole('alert')
    expect(alert).toHaveTextContent(/server group still has servers/i)
    expect(alert).toHaveTextContent(/10\.0\.0\.11/)
    expect(alert).toHaveTextContent(/move each of them/i)
    expect(getState().serverGroups.some((g) => g.name === 'rack-a')).toBe(true)
  })

  it('answers like the server: unknown group on assign is 422, unknown group on delete is 404', async () => {
    await renderApp('/servers')
    await expect(
      endpoints.updateServer('10.0.0.11', { serverGroup: 'nope' }),
    ).rejects.toMatchObject({ code: 'unprocessable' })
    const same = await endpoints.updateServer('10.0.0.11', { serverGroup: 'rack-a' })
    expect(same.serverGroup).toBe('rack-a')
    await expect(endpoints.deleteServerGroup('nope')).rejects.toMatchObject({ code: 'not_found' })
  })

  it('changes whether a server should hold buckets', async () => {
    const user = userEvent.setup()
    await renderApp('/servers/10.0.0.13')
    const box = await screen.findByRole('checkbox', { name: /should hold buckets/i })
    expect(box).toBeChecked()
    await user.click(box)
    await user.click(screen.getByRole('button', { name: /save placement/i }))
    expect(await screen.findByText(/placement saved/i)).toBeInTheDocument()
    expect(getState().servers.find((s) => s.id === '10.0.0.13')?.shouldHaveShards).toBe(false)
  })

  it('explains a refusal to change a server while a rebalance runs', async () => {
    const user = userEvent.setup()
    getState().rebalance.state = 'running'
    await renderApp('/servers/10.0.0.13')
    await user.click(await screen.findByRole('checkbox', { name: /should hold buckets/i }))
    await user.click(screen.getByRole('button', { name: /save placement/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/rebalance is running/i)
  })
})

describe('removing a server no longer drains it', () => {
  async function openRemove(user: ReturnType<typeof userEvent.setup>, id: string) {
    await renderApp('/servers')
    const row = await screen.findByTestId(`server-row-${id}`)
    await user.click(within(row).getByRole('button', { name: /remove/i }))
    return screen.findByRole('dialog', { name: /remove server/i })
  }
  async function confirm(
    user: ReturnType<typeof userEvent.setup>,
    dialog: HTMLElement,
    id: string,
  ) {
    await user.type(within(dialog).getByLabelText(/to confirm/i), `${id}:8091`)
    await user.click(within(dialog).getByRole('button', { name: /^remove server$/i }))
  }

  it('says that the server must hold no bucket copy and points to the rebalance', async () => {
    const user = userEvent.setup()
    const dialog = await openRemove(user, '10.0.0.13')
    expect(dialog).toHaveTextContent(/must not hold any bucket copy/i)
    expect(dialog).not.toHaveTextContent(/is drained first/i)
    expect(within(dialog).getByRole('link', { name: /rebalance/i })).toHaveAttribute(
      'href',
      '/rebalance',
    )
  })

  it('explains server_owns_buckets, naming the databases', async () => {
    const user = userEvent.setup()
    const dialog = await openRemove(user, '10.0.0.13')
    await confirm(user, dialog, '10.0.0.13')
    const alert = await within(dialog).findByRole('alert')
    expect(alert).toHaveTextContent(/server still holds data/i)
    expect(alert).toHaveTextContent(/shop/)
    expect(alert).toHaveTextContent(/run a rebalance/i)
  })

  it('explains that the map group server cannot be removed', async () => {
    const user = userEvent.setup()
    const dialog = await openRemove(user, '10.0.0.11')
    await confirm(user, dialog, '10.0.0.11')
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      /server hosts the cluster metadata/i,
    )
  })

  it('explains the refusal for a dead server: fail it over first', async () => {
    const user = userEvent.setup()
    getState().servers[2]!.status = 'dead'
    const dialog = await openRemove(user, '10.0.0.13')
    await confirm(user, dialog, '10.0.0.13')
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/fail it over first/i)
  })

  it('removes a server that holds no bucket copy', async () => {
    const user = userEvent.setup()
    const s = getState()
    s.servers.push({ ...s.servers[2]!, id: '10.0.0.15', address: '10.0.0.15:8091', hostname: 'x' })
    const dialog = await openRemove(user, '10.0.0.15')
    await confirm(user, dialog, '10.0.0.15')
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    expect(getState().servers.some((x) => x.id === '10.0.0.15')).toBe(false)
  })
})

describe('operations the server does not implement yet', () => {
  it('failover says so from the 422 answer, and works without a panel change when it does not answer so', async () => {
    const { mockConfig } = await import('@/mocks/state')
    mockConfig.unsupported.add('failoverServer')
    const user = userEvent.setup()
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.12')
    await user.click(within(row).getByRole('button', { name: /failover/i }))
    const dialog = await screen.findByRole('dialog', { name: /fail over server/i })
    await user.click(within(dialog).getByRole('button', { name: /graceful failover/i }))
    expect(
      await within(dialog).findByText(/not available on this server version yet/i),
    ).toBeInTheDocument()
  })
})
