// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The findings of the second independent review (kingres/docs/reviews/2026-11-15-web-panel-0.5.md,
// W2-01 to W2-16 and the residuals W-05, W-13, W-15, W-18). Each test names its finding; each one
// failed before the fix.
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { createPinia } from 'pinia'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { mockConfig } from '@/mocks/state'
import { HOSTILE, seedHostile } from '@/mocks/hostile'
import { availability, recovery } from '@/api/availability'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { rawSlices } from '@/lib/raw-json'
import {
  DigestDocumentError,
  parseProof,
  withRawDocuments,
  type RawProof,
} from '@/lib/ledger-proof'
import { hasHidden, nameWarnings, spaceIssues } from '@/lib/safe-text'
import { normalizeFingerprint } from '@/lib/fingerprint'
import { SERVER_STATUS, TASK_STATE } from '@/lib/status'
import TypeToConfirm from '@/components/TypeToConfirm.vue'
import ForbiddenPage from '@/pages/ForbiddenPage.vue'
import ProofResult from '@/components/ledger/ProofResult.vue'
import { useSessionStore } from '@/stores/session'
import { disposeBuiltApps, renderApp, renderComponent, settled } from './helpers'

type User = ReturnType<typeof userEvent.setup>

afterEach(() => {
  mockServer.events.removeAllListeners()
  vi.restoreAllMocks()
})

const savedRecovery = { ...recovery }
beforeEach(() => {
  recovery.watchMs = 40
  recovery.afterStartMs = 40
})
afterEach(() => {
  Object.assign(recovery, savedRecovery)
})

function track(): { path: string; csrf: string | null }[] {
  const seen: { path: string; csrf: string | null }[] = []
  mockServer.events.on('request:start', ({ request }) => {
    seen.push({
      path: `${request.method} ${new URL(request.url).pathname.replace('/api/v1', '')}`,
      csrf: request.headers.get('X-CSRF-Token'),
    })
  })
  return seen
}

/** A reload: the old page is gone, a new one starts with the same browser (cookie, tab storage). */
function reload(): void {
  cleanup()
  disposeBuiltApps()
}

// --- W2-01 ------------------------------------------------------------------------------------

describe('W2-01 Start instance addresses the instance that hosts the system database', () => {
  it('starts instance #11 (the host), says which one it is, and never instance 1', async () => {
    const user = userEvent.setup()
    // Opens the dashboard only: the This server page was never visited.
    const { queryClient } = await renderApp('/dashboard')
    await waitFor(() => expect(availability.instanceId).toBe(11))
    getState().systemDatabaseStopped = true
    getState().instanceStates.set(11, 'stopped')
    await queryClient.invalidateQueries({ queryKey: ['cluster'] })
    const seen = track()
    await screen.findByRole('heading', { name: /system database is stopped/i })
    expect(screen.getByTestId('instance-known')).toHaveTextContent(/instance #11/i)
    await user.click(screen.getByRole('button', { name: /^start instance #11$/i }))
    await waitFor(() =>
      expect(seen.map((s) => s.path)).toContain('POST /this-server/instances/11/start'),
    )
    expect(seen.map((s) => s.path)).not.toContain('POST /this-server/instances/1/start')
  })

  it('offers no Start button when the id was never read, and says to use the machine', async () => {
    mockConfig.unsupported.add('listThisServerInstances')
    const { queryClient } = await renderApp('/dashboard')
    await settled()
    getState().systemDatabaseStopped = true
    getState().instanceStates.set(11, 'stopped')
    await queryClient.invalidateQueries({ queryKey: ['cluster'] })
    await screen.findByRole('heading', { name: /system database is stopped/i })
    expect(availability.instanceId).toBeNull()
    expect(screen.getByTestId('instance-unknown')).toHaveTextContent(/does not guess/i)
    expect(screen.queryByRole('button', { name: /start instance/i })).toBeNull()
  })
})

// --- W2-02 ------------------------------------------------------------------------------------

describe('W2-02 a reload while the system database is stopped keeps the session', () => {
  it('shows the stopped state with a working Start button instead of the sign-in page', async () => {
    const user = userEvent.setup()
    await renderApp('/dashboard')
    await waitFor(() => expect(availability.instanceId).toBe(11))
    getState().systemDatabaseStopped = true
    getState().instanceStates.set(11, 'stopped')
    reload()
    const seen = track()
    const { pinia, router } = await renderApp('/dashboard', { user: null })
    await screen.findByRole('heading', { name: /system database is stopped/i })
    expect(router.currentRoute.value.name).not.toBe('login')
    expect(useSessionStore(pinia).isSignedIn).toBe(true)
    // The start carries the CSRF token of the session (the mock checks it against its own).
    await user.click(await screen.findByRole('button', { name: /^start instance #11$/i }))
    await waitFor(() => expect(getState().systemDatabaseStopped).toBe(false))
    const start = seen.find((s) => s.path === 'POST /this-server/instances/11/start')
    expect(start?.csrf).toBeTruthy()
    // And the panel returns to the page it was on.
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: /system database is stopped/i })).toBeNull(),
    )
  })

  it('a tab that never had the session still ends at the sign-in page, with what to do', async () => {
    getState().systemDatabaseStopped = true
    getState().instanceStates.set(11, 'stopped')
    const { router } = await renderApp('/dashboard', { user: null })
    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'))
    const help = await screen.findByTestId('system-database-stopped-signin')
    expect(help).toHaveTextContent(/pg_ctl start/)
    expect(help).toHaveTextContent(/kingresd/)
    expect(help).toHaveTextContent(/another server starts only the instances of that server/i)
  })

  it('ignores a session planted in browser storage: only the server decides who is signed in', async () => {
    sessionStorage.setItem(
      'kingres.session',
      JSON.stringify({
        username: 'ghost',
        clusterRoles: ['kingres_admin'],
        expiresAt: '2999-01-01T00:00:00Z',
        csrfToken: 'x',
      }),
    )
    getState().systemDatabaseStopped = true
    getState().instanceStates.set(11, 'stopped')
    const { router, pinia } = await renderApp('/dashboard', { user: null })
    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'))
    expect(useSessionStore(pinia).isSignedIn).toBe(false)
  })
})

// --- W2-03, W2-04 -----------------------------------------------------------------------------

describe('W2-04 the fingerprint is sent normalized', () => {
  const hex = 'ab12'.repeat(16)
  it('strips colons, spaces and no-break spaces and lower-cases', () => {
    const grouped = hex.replace(/(.{8})(?=.)/g, '$1 ')
    expect(normalizeFingerprint(grouped)).toBe(hex)
    expect(normalizeFingerprint(hex.toUpperCase().replace(/(..)(?!$)/g, '$1:'))).toBe(hex)
    expect(
      normalizeFingerprint(`${hex.slice(0, 10)}${String.fromCharCode(0xa0)}${hex.slice(10)}`),
    ).toBe(hex)
  })

  it('the add-server dialog sends 64 lower-case digits for the display format of the panel', async () => {
    const user = userEvent.setup()
    const bodies: { expectedFingerprint?: string }[] = []
    mockServer.use(
      http.post('*/api/v1/servers', async ({ request }) => {
        bodies.push((await request.clone().json()) as { expectedFingerprint?: string })
        return undefined
      }),
    )
    await renderApp('/servers')
    await user.click(await screen.findByRole('button', { name: /add server/i }))
    const dialog = await screen.findByRole('dialog', { name: /add server/i })
    await user.type(within(dialog).getByLabelText(/server address/i), '10.0.0.14:8091')
    await user.type(within(dialog).getByLabelText(/setup token/i), 'demo-setup-token')
    const grouped = hex.replace(/(.{8})(?=.)/g, '$1 ').toUpperCase()
    await user.type(within(dialog).getByLabelText(/expected fingerprint/i), grouped)
    await user.click(within(dialog).getByRole('button', { name: /^add server$/i }))
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]?.expectedFingerprint).toMatch(/^[0-9a-f]{64}$/)
    expect(bodies[0]?.expectedFingerprint).toBe(hex)
  })

  it('the join wizard sends 64 lower-case digits too', async () => {
    const { resetMockState } = await import('@/mocks/http')
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    const bodies: { expectedFingerprint?: string }[] = []
    mockServer.use(
      http.post('*/api/v1/setup/join-cluster', async ({ request }) => {
        bodies.push((await request.clone().json()) as { expectedFingerprint?: string })
        return undefined
      }),
    )
    await renderApp('/', { user: null })
    await user.type(await screen.findByLabelText(/setup token/i), 'demo-setup-token')
    await user.click(screen.getByRole('button', { name: /continue/i }))
    await user.click(await screen.findByRole('button', { name: /^join existing cluster$/i }))
    await user.type(screen.getByLabelText(/address of any existing server/i), '10.0.0.11:8091')
    await user.type(screen.getByLabelText(/administrator password/i), 'demo-password')
    await user.type(
      screen.getByLabelText(/expected fingerprint/i),
      hex.toUpperCase().replace(/(..)(?!$)/g, '$1:'),
    )
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]?.expectedFingerprint).toBe(hex)
  })

  it('the mock is not more permissive than the contract: spaces are refused, colons and case are fine', async () => {
    await renderApp('/dashboard')
    const base = { address: '10.0.0.14:8091', setupToken: 'demo-setup-token' }
    await expect(
      endpoints.addServer({ ...base, expectedFingerprint: hex.replace(/(.{8})(?=.)/g, '$1 ') }),
    ).rejects.toMatchObject({ code: 'validation_failed' })
    await expect(
      endpoints.addServer({ ...base, expectedFingerprint: `sha256:${hex}` }),
    ).rejects.toBeInstanceOf(ApiError)
    // Colons between byte pairs, upper case: allowed ("hex, colons allowed").
    const upperColons = hex.toUpperCase().replace(/(..)(?!$)/g, '$1:')
    const task = await endpoints.addServer({ ...base, expectedFingerprint: upperColons })
    expect(task.kind).toBe('server.add')
  })
})

describe('W2-03 the wizard never claims a match the server did not report', () => {
  it('says the pin was sent and that the server does not report a check', async () => {
    const { resetMockState } = await import('@/mocks/http')
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await user.type(await screen.findByLabelText(/setup token/i), 'demo-setup-token')
    await user.click(screen.getByRole('button', { name: /continue/i }))
    await user.click(await screen.findByRole('button', { name: /^join existing cluster$/i }))
    await user.type(screen.getByLabelText(/address of any existing server/i), '10.0.0.11:8091')
    await user.type(screen.getByLabelText(/administrator password/i), 'demo-password')
    // A fingerprint of the right form that a server which ignores the member would accept anyway.
    await user.type(screen.getByLabelText(/expected fingerprint/i), 'cd'.repeat(32))
    mockServer.use(
      http.post('*/api/v1/setup/join-cluster', () => undefined), // handled by the mock below
    )
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))
    // Whatever the outcome of the mock (the pin differs, so it refuses): no sentence says "matched".
    await waitFor(() =>
      expect(document.body).toHaveTextContent(/not the expected one|continue to sign in/i),
    )
    expect(document.body).not.toHaveTextContent(/matched the fingerprint/i)
  })
})

// --- W2-05, W2-06 ------------------------------------------------------------------------------

function proofText(over: Record<string, unknown> = {}, rawTail = ''): string {
  const h = (n: string) => n.repeat(64).slice(0, 64)
  const doc = {
    format: 1,
    chain_id: 'c'.repeat(64),
    relation: 'public.orders',
    table_id: 16389,
    ledger_txid: 48000123,
    seq: 2,
    op: 'I',
    row_hash: h('a'),
    entry_index: 1,
    entry_count: 3,
    entry_path: [h('b')],
    tx_root: h('c'),
    user_name: 'admin',
    tx_index: 5,
    tx_path: [h('d')],
    block: {
      height: 1000,
      hash_version: 1,
      prev_hash: h('e'),
      tx_merkle_root: h('f'),
      tx_count: 7,
      closed_at_us: 1,
      block_hash: h('1'),
    },
    chain: [],
    shard_group_id: 1,
    ...over,
  }
  return `${JSON.stringify(doc, null, '\t')}${rawTail}\n`
}

describe('W2-05 the verification command can be pasted into a shell', () => {
  it('has a backslash and a real line break, not a backslash and the letter n', () => {
    const view = render(ProofResult, { props: { proof: parseProof(proofText()) } })
    const text = view.getByTestId('verify-command').textContent ?? ''
    expect(text).toBe(
      'kingres-verify proof --proof proof.json --digest digest.json --table-id 16389 \\\n  --row-hash <hash recomputed from your own copy of the row>',
    )
    expect(text).not.toContain('\\n')
  })
})

describe('W2-06 proof numbers come from the text, and an odd document is not guessed at', () => {
  it('shows an integer above 2^53 as written (a float would round it)', () => {
    const raw = proofText().replace('"ledger_txid": 48000123', '"ledger_txid": 9007199254740993')
    const proof = parseProof(raw)
    expect(String(proof.view.ledger_txid)).toBe('9007199254740992') // the parsed copy is rounded
    const view = render(ProofResult, { props: { proof } })
    expect(view.getByTestId('proof')).toHaveTextContent('9007199254740993')
    expect(view.getByTestId('proof')).not.toHaveTextContent('9007199254740992')
  })

  it('a proof without a block is shown as raw text with the download, not as a broken page', () => {
    const raw = proofText({ block: undefined })
    const proof: RawProof = parseProof(raw)
    expect(proof.shapeError).toMatch(/block/)
    const view = render(ProofResult, { props: { proof } })
    expect(view.getByTestId('proof-shape-error')).toHaveTextContent(
      /not a proof document the panel can show/i,
    )
    expect(view.getByRole('button', { name: /download proof\.json/i })).toBeInTheDocument()
    expect(view.getByTestId('proof-raw')).toBeInTheDocument()
  })

  it('a proof with a missing list does not throw either', () => {
    expect(parseProof(proofText({ entry_path: undefined })).shapeError).toMatch(/entry_path/)
    expect(parseProof(proofText({ chain: 'x' })).shapeError).toMatch(/chain/)
    expect(parseProof(proofText()).shapeError).toBeNull()
  })
})

// --- W2-07 ------------------------------------------------------------------------------------

describe('W2-07 raw-json on text that is not what JSON.parse would accept', () => {
  it('ends in a SyntaxError for a string that is never closed (it used to loop for ever)', () => {
    expect(() => rawSlices('{"a":"abc', () => true)).toThrow(SyntaxError)
    expect(() => rawSlices('{"a":', () => true)).toThrow(SyntaxError)
    expect(() => rawSlices('[1,2', () => true)).toThrow(SyntaxError)
    expect(() => rawSlices('{"a" 1}', () => true)).toThrow(SyntaxError)
  })

  it('refuses a document with the same key twice (JSON.parse keeps the last, a scanner would find both)', () => {
    expect(() => rawSlices('{"document":{"x":1},"document":{"x":2}}', () => true)).toThrow(/twice/)
    // The same name in two different objects is fine.
    expect(rawSlices('{"a":{"k":1},"b":{"k":2}}', (p) => p.at(-1) === 'k')).toHaveLength(2)
  })

  it('never saves a re-serialized digest: a missing document is an error', () => {
    const text =
      '{"items":[{"id":1,"chains":[{"shardGroupId":1,"blockHeight":1,"blockHash":"x"}]}]}'
    const parsed = JSON.parse(text)
    expect(() => withRawDocuments(parsed, text)).toThrow(DigestDocumentError)
    expect(() => withRawDocuments(parsed, text)).toThrow(/cannot be saved exactly/)
  })
})

// --- W2-08 ------------------------------------------------------------------------------------

describe('W2-08 spaces that are easy to miss, and hidden characters anywhere in a name', () => {
  it('flags leading, trailing and doubled spaces', () => {
    for (const name of ['orders ', ' orders', 'or  ders'])
      expect(nameWarnings(name).join(' '), JSON.stringify(name)).toMatch(/easy to miss/)
    expect(nameWarnings('orders')).toEqual([])
    expect(nameWarnings('my orders')).toEqual([])
    expect(spaceIssues('a  b')).toEqual(['has two spaces in a row'])
  })

  it('counts a hidden character after position 300', () => {
    const name = `${'x'.repeat(300)}‮`
    expect(hasHidden(name)).toBe(true)
    expect(nameWarnings(name).join(' ')).toMatch(/hidden character/)
  })

  it('the typed confirmation explains a name with a trailing space', () => {
    const view = render(TypeToConfirm, { props: { expected: 'orders ', modelValue: '' } })
    expect(view.container).toHaveTextContent(/spaces that are easy to miss/i)
    expect(view.getByRole('button', { name: /copy the name/i })).toBeInTheDocument()
  })
})

// --- W2-09 ------------------------------------------------------------------------------------

/** Characters that draw nothing or reorder text: none may reach the page raw. */
const RAW = new RegExp(
  `[${[
    '202a-202e',
    '2066-2069',
    '200b-200f',
    '2028-2029',
    '2060',
    'feff',
    '0000-0008',
    '000b-001f',
    '007f',
  ]
    .map((r) =>
      r
        .split('-')
        .map((h) => String.fromCharCode(parseInt(h, 16)))
        .join('-'),
    )
    .join('')}]`,
)
function expectNoRawHidden(): void {
  const texts = [document.body.textContent ?? '']
  for (const el of Array.from(document.body.querySelectorAll('*')))
    for (const a of ['title', 'aria-label']) {
      const v = el.getAttribute(a)
      if (v) texts.push(v)
    }
  for (const t of texts) expect(t).not.toMatch(RAW)
}

describe('W2-09 places that used to bypass SafeName', () => {
  it('the cluster name in the top bar', async () => {
    getState().clusterName = HOSTILE.reversed
    await renderApp('/dashboard')
    await screen.findByRole('heading', { name: /dashboard/i })
    expect(document.querySelector('.cluster-name')?.textContent).toContain('U+202E')
    expectNoRawHidden()
  })

  it('the user name on the not-allowed page', async () => {
    const { pinia } = await renderComponent(ForbiddenPage)
    const session = useSessionStore(pinia)
    session.session = { ...session.session!, username: HOSTILE.reversed }
    await waitFor(() => expect(document.body.textContent).toContain('U+202E'))
    expectNoRawHidden()
  })

  it('the server address in the remove confirmation', async () => {
    const user = userEvent.setup()
    getState().servers.find((s) => s.id === '10.0.0.13')!.address = HOSTILE.reversed
    await renderApp('/servers/10.0.0.13')
    await user.click(await screen.findByRole('button', { name: /^remove$/i }))
    const dialog = await screen.findByRole('dialog')
    expect(dialog.textContent).toContain('U+202E')
    expectNoRawHidden()
  })

  it('anchor destinations in the digests table', async () => {
    getState().ledger.digests[0]!.anchors = [
      { target: HOSTILE.reversed, status: HOSTILE.zeroWidth } as never,
    ]
    await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    await waitFor(() => expect(document.body.textContent).toContain('U+202E'))
    expectNoRawHidden()
  })

  it('the leader and the server id on the dashboard and This server', async () => {
    seedHostile(getState())
    getState().servers[0]!.address = HOSTILE.reversed
    await renderApp('/dashboard')
    await settled()
    expectNoRawHidden()
  })

  it('the remaining templates no longer print these server texts bare (source check)', () => {
    const read = (p: string) => readFileSync(join(process.cwd(), 'src', p), 'utf8')
    expect(read('components/CreateDatabaseDialog.vue')).not.toMatch(/{{ [ec].name }}/)
    expect(read('components/security/RoleDialog.vue')).not.toMatch(
      /\{\{ c\.(name|description) \}\}/,
    )
    expect(read('pages/ThisServerPage.vue')).not.toMatch(
      /\{\{ info\.data\.value\.(nodeState|version) \}\}/,
    )
    expect(read('pages/RebalancePage.vue')).not.toMatch(/\{\{ m\.phase \}\}/)
    expect(read('layouts/ShellLayout.vue')).not.toMatch(/\{\{ session\.session\?\.clusterName/)
    expect(read('pages/ForbiddenPage.vue')).not.toMatch(/\{\{ session\.username \}\}/)
  })
})

// --- W2-10 ------------------------------------------------------------------------------------

async function openLeave(user: User) {
  await user.click(await screen.findByRole('button', { name: /^leave cluster$/i }))
  return screen.findByRole('dialog', { name: /leave the cluster/i })
}
function leavable(): void {
  const s = getState()
  const map = s.groups.find((g) => g.isMapGroup)
  if (map) {
    map.primaryServerId = '10.0.0.12'
    map.preferredPrimaryServerId = '10.0.0.12'
    map.standbyServerIds = ['10.0.0.13']
  }
  for (const g of s.groups) {
    if (g.isMapGroup) continue
    if (g.primaryServerId === '10.0.0.11')
      g.primaryServerId = g.standbyServerIds.shift() ?? '10.0.0.12'
    g.standbyServerIds = g.standbyServerIds.filter((x) => x !== '10.0.0.11')
  }
}

describe('W2-10 force leave only for an answer of the server', () => {
  it('a network failure (no answer) does not offer force, and says to check membership first', async () => {
    leavable()
    const user = userEvent.setup()
    mockServer.use(http.post('*/api/v1/this-server/leave-cluster', () => HttpResponse.error()))
    await renderApp('/this-server')
    const dialog = await openLeave(user)
    await user.type(within(dialog).getByRole('textbox'), 'kdb-1.example.internal')
    await user.click(within(dialog).getByRole('button', { name: /^leave cluster$/i }))
    expect(await within(dialog).findByTestId('leave-no-answer')).toHaveTextContent(
      /may or may not have been carried out/i,
    )
    expect(within(dialog).queryByRole('checkbox')).toBeNull()
    expect(within(dialog).queryByTestId('force-leave')).toBeNull()
  })

  it('a proxy page with status 503 (not a problem document) does not offer force either', async () => {
    leavable()
    const user = userEvent.setup()
    mockServer.use(
      http.post('*/api/v1/this-server/leave-cluster', () =>
        HttpResponse.text('<html>Bad gateway</html>', { status: 503 }),
      ),
    )
    await renderApp('/this-server')
    const dialog = await openLeave(user)
    await user.type(within(dialog).getByRole('textbox'), 'kdb-1.example.internal')
    await user.click(within(dialog).getByRole('button', { name: /^leave cluster$/i }))
    await within(dialog).findByTestId('leave-no-answer')
    expect(within(dialog).queryByTestId('force-leave')).toBeNull()
  })

  it("the server's own 503 for the leader still offers force, with the new wording", async () => {
    leavable()
    getState().leaderReachable = false
    const user = userEvent.setup()
    await renderApp('/this-server')
    const dialog = await openLeave(user)
    await user.type(within(dialog).getByRole('textbox'), 'kdb-1.example.internal')
    await user.click(within(dialog).getByRole('button', { name: /^leave cluster$/i }))
    const force = await within(dialog).findByTestId('force-leave')
    expect(force).toHaveTextContent(/could not reach the manager leader, or the answer was lost/i)
  })
})

// --- W2-11 ------------------------------------------------------------------------------------

describe('W2-11 an unknown status does not break the page', () => {
  it('has a neutral "Unknown" badge for a value the panel does not know', () => {
    const meta = (SERVER_STATUS as Record<string, { label: string; tone: string }>)['quarantined']
    expect(meta?.tone).toBe('neutral')
    expect(meta?.label).toBe('Unknown (quarantined)')
    expect((TASK_STATE as Record<string, { label: string }>)['evaporated']?.label).toMatch(
      /Unknown/,
    )
    expect(SERVER_STATUS.live.label).toBe('LIVE')
    expect(Object.keys(SERVER_STATUS)).toHaveLength(6)
  })

  it('/servers renders every row, with no Vue error, when one server has an unknown status', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined)
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined)
    getState().servers[1]!.status = 'quarantined' as never
    await renderApp('/servers')
    await screen.findByTestId('server-row-10.0.0.13')
    expect(screen.getByTestId('server-row-10.0.0.12')).toHaveTextContent('Unknown (quarantined)')
    expect(screen.getAllByTestId(/^server-row-/)).toHaveLength(3)
    expect(error).not.toHaveBeenCalled()
    expect(warn).not.toHaveBeenCalled()
  })
})

// --- W2-12 ------------------------------------------------------------------------------------

describe('W2-12 the setup token does not outlive the wizard', () => {
  it('leaving the page without finishing stops sending X-Setup-Token', async () => {
    const { resetMockState } = await import('@/mocks/http')
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await user.type(await screen.findByLabelText(/setup token/i), 'demo-setup-token')
    await user.click(screen.getByRole('button', { name: /continue/i }))
    await screen.findByRole('button', { name: /^join existing cluster$/i })
    const headers: (string | null)[] = []
    mockServer.events.on('request:start', ({ request }) => {
      headers.push(request.headers.get('X-Setup-Token'))
    })
    await endpoints.getNodeState()
    expect(headers.at(-1)).toBe('demo-setup-token') // attached while the wizard is open
    cleanup() // the page is left: unmounted
    await endpoints.getNodeState()
    expect(headers.at(-1)).toBeNull()
  })
})

// --- W2-13 ------------------------------------------------------------------------------------

describe('W2-13 consequence wording', () => {
  it('dropping a ledger database says that its chains, blocks and digests go with it', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    await user.click(await screen.findByRole('button', { name: /^drop/i }))
    const dialog = await screen.findByRole('dialog', { name: /drop database/i })
    const note = within(dialog).getByTestId('drop-ledger-note')
    expect(note).toHaveTextContent(/hash chains, blocks and digests are deleted with it/i)
    expect(note).toHaveTextContent(/audit log keeps a record/i)
  })

  it('dropping a database without a ledger does not say that', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/reports')
    await user.click(await screen.findByRole('button', { name: /^drop/i }))
    const dialog = await screen.findByRole('dialog', { name: /drop database/i })
    expect(within(dialog).queryByTestId('drop-ledger-note')).toBeNull()
  })

  it('removing a server says that its PostgreSQL instance and data on the machine are deleted', async () => {
    const user = userEvent.setup()
    await renderApp('/servers/10.0.0.13')
    await user.click(await screen.findByRole('button', { name: /^remove$/i }))
    const dialog = await screen.findByRole('dialog', { name: /remove server/i })
    expect(dialog).toHaveTextContent(
      /told to delete its PostgreSQL\s+instance and the data of it on that machine/i,
    )
  })

  it('restarting the instance of the system database says that sign-in pauses meanwhile', async () => {
    const user = userEvent.setup()
    await renderApp('/this-server')
    const row = (await screen.findByRole('rowheader', { name: '#11' })).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: /^restart$/i }))
    const dialog = await screen.findByRole('dialog', { name: /restart instance #11/i })
    expect(within(dialog).getByTestId('restart-system-note')).toHaveTextContent(
      /including sign-in, are paused/i,
    )
  })
})

// --- W2-14, W-15 ------------------------------------------------------------------------------

describe('W2-14 and W-15 a database named ".." cannot be opened by address', () => {
  it('is listed without a link, and says so', async () => {
    seedHostile(getState())
    await renderApp('/databases')
    await settled()
    const cell = (await screen.findAllByText(/cannot be opened by address/i))[0] as HTMLElement
    expect(cell).toBeInTheDocument()
    for (const link of screen.getAllByRole('link')) {
      expect(link.getAttribute('href') ?? '').not.toMatch(/\/databases\/\.\.?$/)
    }
  })

  it('the router itself refuses such a name (a link made anywhere else lands on the list)', async () => {
    const { router } = await renderApp('/dashboard')
    await router.push({ name: 'database', params: { name: '..' } })
    expect(router.currentRoute.value.name).toBe('databases')
    await router.push({ name: 'database', params: { name: 'shop' } })
    expect(router.currentRoute.value.name).toBe('database')
  })
})

// --- W-13 -------------------------------------------------------------------------------------

describe('W-13 the confirmations that are not typed still state their consequence', () => {
  it('restore preferred primaries asks first and says connections are interrupted', async () => {
    const user = userEvent.setup()
    const seen = track()
    await renderApp('/servers')
    await user.click(await screen.findByRole('button', { name: /restore preferred primaries/i }))
    const dialog = await screen.findByRole('dialog', { name: /restore preferred primaries/i })
    expect(dialog).toHaveTextContent(/connections to those primaries are interrupted/i)
    expect(seen.map((s) => s.path)).not.toContain('POST /replication/restore-preferred-primaries')
  })
})

// --- W-18 -------------------------------------------------------------------------------------

describe('W-18 DEPENDENCIES.md states what npm audit reports', () => {
  it('says that the findings are development-only and none is in the production tree', () => {
    const text = readFileSync(join(process.cwd(), 'DEPENDENCIES.md'), 'utf8')
    expect(text).toMatch(/npm audit/)
    expect(text).toMatch(/development/i)
    expect(text).toMatch(/--omit=dev/)
  })
})

// A pinia is built per test by the harness; this keeps the import used for type-only helpers.
void createPinia
