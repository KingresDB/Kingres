// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The Ledger page of contract 0.11.0, "Console A" (design of phase 7, step 9): the tab bar, the status with what is
// published and anchored, the signed column of the blocks, the digests with the aggregate dialog, the Signing tab (keys,
// rotation, retirement, revocation, holders, events, trust file), the Publication tab, and the dashboard card. An operation
// that the server does not build yet is shown as what it is. A private key, a seed or a passphrase is never asked for or
// shown, and what the server sends is text.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { configureAxe } from 'vitest-axe'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { applySwitches } from '@/mocks/switches'
import { seedHostile } from '@/mocks/hostile'
import { renderApp } from './helpers'

const axe = configureAxe({ rules: { 'color-contrast': { enabled: false } } })

function switches(query: string): void {
  applySwitches(new URLSearchParams(query), getState())
}

/** Files the page asked the browser to save. */
const saved: { name: string; blob: Blob }[] = []
async function savedText(index = saved.length - 1): Promise<string> {
  const blob = saved[index]?.blob
  if (!blob) throw new Error('nothing was saved')
  return await new Promise((resolve) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.readAsText(blob)
  })
}
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

async function openSigning(user?: string) {
  const app = await renderApp('/ledger?tab=signing', user ? { user } : {})
  await screen.findByRole('heading', { name: 'Signing keys' })
  return app
}
async function openPublication() {
  const app = await renderApp('/ledger?tab=publication')
  await screen.findByRole('heading', { name: 'Publication' })
  return app
}
const rows = (testId: string) => screen.queryAllByTestId(testId)

describe('the tab bar of the Ledger page', () => {
  it('has Overview, Digests, Anchoring, Client keys, Signing and Publication, and keeps the database and the tab in the address', async () => {
    const user = userEvent.setup()
    const { router } = await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    const tabs = screen.getAllByRole('tab').map((t) => t.textContent?.trim())
    expect(tabs).toEqual([
      'Overview',
      'Digests',
      'Anchoring',
      'Client keys',
      'Signing',
      'Publication',
    ])
    expect(screen.getByRole('tab', { name: 'Overview' })).toHaveAttribute('aria-selected', 'true')
    await user.click(screen.getByRole('tab', { name: 'Digests' }))
    await screen.findByRole('heading', { name: 'Digests' })
    await waitFor(() =>
      expect(router.currentRoute.value.query).toEqual({ db: 'shop', tab: 'digests' }),
    )
    await user.click(screen.getByRole('tab', { name: 'Signing' }))
    await screen.findByRole('heading', { name: 'Signing keys' })
    // The tabs about the cluster have no database selector.
    expect(screen.queryByLabelText('Database')).not.toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: 'Overview' }))
    await screen.findByText(/status of shop/i)
    expect(router.currentRoute.value.query.tab).toBeUndefined()
  })

  it('moves between the tabs with the arrow keys', async () => {
    const user = userEvent.setup()
    await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    screen.getByRole('tab', { name: 'Overview' }).focus()
    await user.keyboard('{ArrowRight}')
    expect(screen.getByRole('tab', { name: 'Digests' })).toHaveAttribute('aria-selected', 'true')
    await user.keyboard('{End}')
    expect(screen.getByRole('tab', { name: 'Publication' })).toHaveAttribute(
      'aria-selected',
      'true',
    )
  })

  it('reaches Signing and Publication when no database uses the ledger', async () => {
    for (const d of getState().databases) d.info.ledger.mode = 'off'
    await renderApp('/ledger?tab=signing')
    expect(await screen.findByRole('heading', { name: 'Signing keys' })).toBeInTheDocument()
    expect(screen.queryByText(/no database uses the ledger/i)).not.toBeInTheDocument()
  })
})

describe('the Overview tab', () => {
  it('says what is published and anchored, and that a ledger without anchors can be rewritten', async () => {
    await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    const pub = await screen.findByTestId('status-publication')
    expect(pub).toHaveTextContent('Published')
    expect(pub).toHaveTextContent(/newest signed checkpoint/i)
    expect(screen.getByTestId('status-anchoring')).toHaveTextContent('Not anchored')
    expect(screen.getByTestId('status-anchoring')).toHaveTextContent('0 targets enabled')
    expect(screen.getByTestId('not-anchored')).toHaveTextContent(
      'this ledger can be rewritten undetected by anyone who controls this cluster and its backups',
    )
  })

  it('shows an anchored ledger without the notice, and a late or failed one in words', async () => {
    switches('anchors=ok')
    const first = await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    expect(await screen.findByTestId('status-anchoring')).toHaveTextContent('Anchored')
    expect(screen.getByTestId('status-anchoring')).toHaveTextContent('1 target enabled')
    expect(screen.queryByTestId('not-anchored')).not.toBeInTheDocument()
    first.unmount()
    switches('anchors=failed')
    await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    await waitFor(() =>
      expect(screen.getByTestId('status-anchoring')).toHaveTextContent('Anchoring failed'),
    )
  })

  it('shows a diverged publication, and a server that does not report publication says so', async () => {
    switches('publication=diverged')
    await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    expect(await screen.findByText('Diverged')).toBeInTheDocument()
  })

  it('has a column "Signed" for the blocks, with words and not only a mark', async () => {
    await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    const blocks = (await screen.findByRole('heading', { name: 'Blocks' })).closest(
      'section',
    ) as HTMLElement
    expect(await within(blocks).findByRole('columnheader', { name: 'Signed' })).toBeInTheDocument()
    await waitFor(() => expect(within(blocks).getAllByText('Signed').length).toBeGreaterThan(1))
    expect(within(blocks).getAllByText(/not signed yet/i).length).toBeGreaterThan(0)
    expect(blocks.textContent).toMatch(/covers the block/)
  })
})

describe('the Digests tab: aggregates', () => {
  it('shows "Not anchored" in the column of the anchors and the aggregate size beside the hash', async () => {
    await renderApp('/ledger?tab=digests')
    await screen.findByRole('heading', { name: 'Digests' })
    expect(await screen.findByTestId('digest-not-anchored')).toHaveTextContent('Not anchored')
    expect(screen.getByTestId('digest-aggregate-3')).toHaveTextContent('Aggregate of 41 rounds')
  })

  it('opens the aggregate of a digest: the members, the statement, the anchors and the signed checkpoint', async () => {
    const user = userEvent.setup()
    await renderApp('/ledger?tab=digests')
    await user.click(await screen.findByRole('button', { name: /open aggregate 41/i }))
    const dialog = await screen.findByRole('dialog', { name: /aggregate checkpoint 41/i })
    const agg = await within(dialog).findByTestId('aggregate')
    expect(within(agg).getAllByRole('row').length).toBe(1 + 3)
    expect(within(agg).getByTestId('aggregate-statement')).toHaveTextContent(/does NOT prove/)
    expect(within(agg).getByTestId('aggregate-not-anchored')).toHaveTextContent(/can be rewritten/)
    const note = within(agg).getByLabelText('The signed checkpoint')
    expect(note.textContent).toContain('/a/')
    expect(note.textContent).toContain('\n41\n')
    // Copy and download give the note as it is.
    await user.click(within(agg).getByRole('button', { name: /download checkpoint/i }))
    expect(saved[0]?.name).toBe('aggregate-checkpoint-41.txt')
    expect(await savedText()).toBe(note.textContent)
  })

  it('names a diverged chain, a stale member and a group the aggregate does not cover', async () => {
    switches('publication=diverged,unread')
    mockConfig.incompleteLedger = true
    getState().ledger.digests[0]!.aggregateSize = 42
    const user = userEvent.setup()
    await renderApp('/ledger?tab=digests')
    await user.click(await screen.findByRole('button', { name: /open aggregate 42/i }))
    const dialog = await screen.findByRole('dialog', { name: /aggregate checkpoint 42/i })
    expect(await within(dialog).findByTestId('aggregate-incomplete')).toHaveTextContent(
      /does not cover shard group 3/,
    )
    expect(within(dialog).getByTestId('flag-diverged')).toHaveTextContent(/chain log is stopped/)
    // Group 3 is not a member; group 2 is diverged, and the stale flag is on the last member (group 2).
    expect(within(dialog).getByTestId('flag-stale')).toHaveTextContent(/could not be read/)
  })

  it('lists the aggregate checkpoints, a page of ten, with a way to open and to download each', async () => {
    const user = userEvent.setup()
    await renderApp('/ledger?tab=digests')
    const section = (await screen.findByRole('heading', { name: 'Aggregate checkpoints' })).closest(
      'section',
    ) as HTMLElement
    await within(section).findAllByRole('button', { name: /^open aggregate checkpoint/i })
    expect(
      within(section).getAllByRole('button', { name: /^open aggregate checkpoint/i }),
    ).toHaveLength(10)
    await user.click(within(section).getByRole('button', { name: /load more aggregate/i }))
    await waitFor(() =>
      expect(
        within(section).getAllByRole('button', { name: /^open aggregate checkpoint/i }).length,
      ).toBe(20),
    )
    await user.click(
      within(section).getAllByRole('button', { name: /^download checkpoint \d+/i })[0]!,
    )
    expect(saved[0]?.name).toBe('aggregate-checkpoint-42.txt')
  })

  it('says calmly that the server does not build aggregates yet, and keeps the rest of the tab', async () => {
    switches('phase7=today')
    const user = userEvent.setup()
    await renderApp('/ledger?tab=digests')
    const section = (await screen.findByRole('heading', { name: 'Aggregate checkpoints' })).closest(
      'section',
    ) as HTMLElement
    const notice = await within(section).findByText(/not available on this server version yet/i)
    expect(notice.closest('[data-code]')).toHaveAttribute('data-code', 'unsupported_operation')
    // The digest list still works, and opening an aggregate says the same in its own dialog.
    await user.click(await screen.findByRole('button', { name: /open aggregate 41/i }))
    const dialog = await screen.findByRole('dialog')
    expect(
      await within(dialog).findByText(/not available on this server version yet/i),
    ).toBeInTheDocument()
  })

  it('says that nothing is published when the database has no published aggregate', async () => {
    switches('publication=off')
    const user = userEvent.setup()
    await renderApp('/ledger?tab=digests')
    await user.click(await screen.findByRole('button', { name: /open aggregate 41/i }))
    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText(/ledger is not published yet/i)).toBeInTheDocument()
  })
})

describe('the Signing tab', () => {
  it('shows the keys with fingerprints in groups of four, the holders, the events and the trust file', async () => {
    await openSigning()
    expect(screen.getByTestId('signing-mode')).toHaveTextContent(
      /key file.*sealed with a passphrase/i,
    )
    expect(screen.getByTestId('cluster-id').textContent).toMatch(/^[0-9a-f]{64}$/)
    const fingerprints = screen.getAllByTestId('key-fingerprint').map((e) => e.textContent ?? '')
    expect(fingerprints).toHaveLength(2)
    for (const f of fingerprints) expect(f).toMatch(/^([0-9a-f]{4} ){15}[0-9a-f]{4}$/)
    expect(screen.getByTestId('key-active')).toHaveTextContent('Active')
    expect(screen.getByTestId('key-retired')).toHaveTextContent('Retired')
    expect(screen.getByTestId('key-retired')).toHaveTextContent(/may sign up to size 1,?800/)
    const holders = within(screen.getByTestId('holders')).getAllByRole('listitem')
    expect(holders.map((h) => h.textContent?.trim())).toEqual(['10.0.0.11', '10.0.0.12'])
    const events = rows('event-type').map((e) => e.textContent ?? '')
    expect(events[0]).toMatch(/Key added/)
    expect(events[2]).toMatch(/Key retired/)
    expect(screen.getByRole('button', { name: /download trust file/i })).not.toHaveAttribute(
      'aria-disabled',
      'true',
    )
    expect(screen.getByTestId('keep-outside').querySelectorAll('li')).toHaveLength(6)
    await waitFor(() =>
      expect(screen.getByTestId('signing-prefix')).toHaveTextContent('10.0.0.11/kingres-ledger'),
    )
    expect(screen.getByTestId('signing-prefix')).toHaveTextContent('(locked)')
  })

  it('never asks for or shows a private key, a seed or a passphrase', async () => {
    const user = userEvent.setup()
    await openSigning()
    expect(document.body.textContent).not.toMatch(/seed\b|private key:|BEGIN .*PRIVATE/i)
    await user.click(screen.getByRole('button', { name: /^rotate the key$/i }))
    const dialog = await screen.findByRole('dialog')
    for (const box of within(dialog).queryAllByRole('textbox'))
      expect(box.getAttribute('aria-labelledby') ?? '').not.toMatch(/passphrase|seed|private/i)
    expect(
      within(dialog).queryByRole('textbox', { name: /passphrase|seed|private/i }),
    ).not.toBeInTheDocument()
    expect(dialog.querySelectorAll('input[type="password"]')).toHaveLength(0)
    expect(dialog.textContent).toMatch(/never shown here|never leaves/i)
  })

  it('downloads the trust file exactly as the server sent it, and says it is not a source of first trust', async () => {
    const user = userEvent.setup()
    await openSigning()
    await user.click(screen.getByRole('button', { name: /download trust file/i }))
    await screen.findByTestId('trust-saved')
    expect(saved[0]?.name).toBe('trust.json')
    const text = await savedText()
    const file = JSON.parse(text) as { origin_prefix: string; keys: { fingerprint: string }[] }
    expect(file.origin_prefix).toBe('10.0.0.11/kingres-ledger')
    expect(file.keys).toHaveLength(2)
    expect(text).not.toMatch(/seed|passphrase/i)
    expect(document.body.textContent).toMatch(/not a source of first trust/i)
    expect(document.body.textContent).toMatch(/obtained out of band/i)
  })

  it('creates the first key after the origin prefix was confirmed, and the prefix is locked afterwards', async () => {
    switches('signing=none')
    const user = userEvent.setup()
    await openSigning()
    expect(screen.getByTestId('no-key')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /download trust file/i })).toHaveAttribute(
      'aria-disabled',
      'true',
    )
    await user.click(screen.getByRole('button', { name: /create the first key/i }))
    const dialog = await screen.findByRole('dialog', { name: /create the first signing key/i })
    // The default is shown for confirmation; it cannot be created before it is confirmed.
    const prefix = within(dialog).getByRole('textbox', {
      name: /origin prefix/i,
    }) as HTMLInputElement
    expect(prefix.value).toBe('10.0.0.11/kingres-ledger')
    const confirm = within(dialog).getByRole('button', { name: /^create the key$/i })
    expect(confirm).toBeDisabled()
    await user.clear(prefix)
    await user.type(prefix, 'ledger.example.com/kingres')
    await user.click(within(dialog).getByRole('checkbox', { name: /i confirm the origin prefix/i }))
    expect(confirm).toBeEnabled()
    await user.click(confirm)
    await within(dialog).findByText('Finished.', {}, { timeout: 4000 })
    expect(within(dialog).getByTestId('create-key-next')).toHaveTextContent(/trust file/i)
    expect(getState().ledger7.settings.publication.originPrefix).toBe('ledger.example.com/kingres')
    expect(getState().ledger7.signing.keys).toHaveLength(1)
    await user.click(within(dialog).getByRole('button', { name: /^close$/i }))
    await waitFor(() => expect(rows('key-active')).toHaveLength(1))
    expect(screen.getByTestId('signing-prefix')).toHaveTextContent('(locked)')
  })

  it('refuses a malformed origin prefix before anything is sent', async () => {
    switches('signing=none')
    const user = userEvent.setup()
    await openSigning()
    await user.click(screen.getByRole('button', { name: /create the first key/i }))
    const dialog = await screen.findByRole('dialog')
    const prefix = within(dialog).getByRole('textbox', { name: /origin prefix/i })
    await user.clear(prefix)
    await user.type(prefix, 'https://example.com/')
    await user.click(within(dialog).getByRole('checkbox'))
    await user.click(within(dialog).getByRole('button', { name: /^create the key$/i }))
    expect(await within(dialog).findByText(/without a scheme/i)).toBeInTheDocument()
    expect(getState().ledger7.signing.keys).toHaveLength(0)
  })

  it('rotates: the overlap is checked, two keys are active afterwards, and the backup tick clears the reminder', async () => {
    switches('signing=backup')
    const user = userEvent.setup()
    await openSigning()
    expect(screen.getByTestId('backup-advised')).toHaveTextContent(/off the cluster/)
    await user.click(screen.getByRole('button', { name: /^rotate the key$/i }))
    const dialog = await screen.findByRole('dialog', { name: /rotate the signing key/i })
    const days = within(dialog).getByLabelText(/overlap in days/i)
    await user.clear(days)
    await user.type(days, '400')
    await user.click(within(dialog).getByRole('button', { name: /^rotate the key$/i }))
    expect((await within(dialog).findAllByText(/from 1 to 365/i)).length).toBeGreaterThan(0)
    await user.clear(days)
    await user.type(days, '7')
    await user.click(within(dialog).getByRole('checkbox', { name: /i have made a backup/i }))
    await user.click(within(dialog).getByRole('button', { name: /^rotate the key$/i }))
    await within(dialog).findByText('Finished.', {}, { timeout: 4000 })
    await user.click(within(dialog).getByRole('button', { name: /^close$/i }))
    await waitFor(() => expect(rows('key-active')).toHaveLength(2))
    expect(getState().ledger7.signing.keys.filter((k) => k.state === 'active')).toHaveLength(2)
    await waitFor(() => expect(screen.queryByTestId('backup-advised')).not.toBeInTheDocument())
  })

  it('offers no retirement and no revocation of the only key in force, and says why', async () => {
    await openSigning()
    const retire = screen.getByRole('button', { name: /retire now/i })
    expect(retire).toHaveAttribute('aria-disabled', 'true')
    expect(retire.textContent).toMatch(/only key in force/i)
    const revoke = within(screen.getByTestId('key-active')).getByRole('button', {
      name: /^revoke/i,
    })
    expect(revoke).toHaveAttribute('aria-disabled', 'true')
    expect(revoke.textContent).toMatch(/never creates one|rotate first/i)
  })

  it('retires a key after saying what it does, then shows it retired', async () => {
    switches('signing=rotating')
    const user = userEvent.setup()
    await openSigning()
    const active = rows('key-active')
    expect(active).toHaveLength(2)
    await user.click(within(active[0]!).getByRole('button', { name: /retire now/i }))
    const dialog = await screen.findByRole('dialog', { name: /retire the signing key/i })
    expect(dialog.textContent).toMatch(/deleted from every server/i)
    expect(dialog.textContent).toMatch(/stays valid for ever/i)
    await user.click(within(dialog).getByRole('button', { name: /^retire the key$/i }))
    await within(dialog).findByText('Finished.', {}, { timeout: 4000 })
    await user.click(within(dialog).getByRole('button', { name: /^close$/i }))
    await waitFor(() => expect(rows('key-retired')).toHaveLength(2))
    expect(rows('key-active')).toHaveLength(1)
  })

  it('revokes only when the fingerprint is typed, says what it means for old signatures, and refuses odd text', async () => {
    switches('signing=rotating')
    const user = userEvent.setup()
    await openSigning()
    const target = getState().ledger7.signing.keys.filter((k) => k.state === 'active')[0]!
    await user.click(within(rows('key-active')[0]!).getByRole('button', { name: /^revoke/i }))
    const dialog = await screen.findByRole('dialog', { name: /revoke the signing key/i })
    const meaning = dialog.textContent ?? ''
    expect(meaning).toMatch(/unless an anchor proves/i)
    expect(meaning).toMatch(/back-dated checkpoint/i)
    expect(meaning).toMatch(/earlier time is the safe direction/i)
    expect(meaning).toMatch(/deleted from every server/i)
    expect(meaning).toMatch(/needs a new trust file, obtained out of band/i)
    const confirm = within(dialog).getByRole('button', { name: /^revoke the key$/i })
    expect(confirm).toBeDisabled()
    await user.type(
      within(dialog).getByRole('textbox', { name: /^type .* to confirm/i }),
      target.fingerprint,
    )
    expect(confirm).toBeEnabled()
    // A time later than now is refused here, and so is a reason that is not printable ASCII.
    const since = within(dialog).getByLabelText(/compromised since/i)
    await fireEvent.update(since, '2999-01-01T00:00')
    await user.type(within(dialog).getByLabelText(/^reason/i), `caf${String.fromCharCode(0xe9)}`)
    await user.click(confirm)
    expect(await within(dialog).findByText(/cannot be later than now/i)).toBeInTheDocument()
    expect(within(dialog).getByText(/printable ASCII only/i)).toBeInTheDocument()
    expect(
      getState().ledger7.signing.keys.find((k) => k.fingerprint === target.fingerprint)?.state,
    ).toBe('active')
    await fireEvent.update(since, '2026-11-01T10:00')
    const reason = within(dialog).getByLabelText(/^reason/i)
    await user.clear(reason)
    await user.type(reason, 'laptop lost')
    await user.click(confirm)
    await within(dialog).findByText('Finished.', {}, { timeout: 4000 })
    expect(within(dialog).getByTestId('revoke-next')).toHaveTextContent(/new trust file/i)
    const revoked = getState().ledger7.signing.keys.find(
      (k) => k.fingerprint === target.fingerprint,
    )
    expect(revoked?.state).toBe('revoked')
    expect(revoked?.compromisedSince?.slice(0, 10)).toBe(
      new Date('2026-11-01T10:00').toISOString().slice(0, 10),
    )
    await user.click(within(dialog).getByRole('button', { name: /^close$/i }))
    await waitFor(() => expect(rows('key-revoked')).toHaveLength(1))
    expect(
      screen.getAllByTestId('event-type').some((e) => /Key revoked/.test(e.textContent ?? '')),
    ).toBe(true)
  })

  it('shows a revoked key with the time since which it is compromised', async () => {
    switches('signing=revoked')
    await openSigning()
    const row = await screen.findByTestId('key-revoked')
    expect(row).toHaveTextContent('Revoked')
    expect(row).toHaveTextContent(/compromised since/i)
    expect(screen.getByTestId('backup-advised')).toBeInTheDocument()
  })

  it('says in words how a retirement fails when a holder still has the file, and that it can be tried again', async () => {
    switches('signing=rotating,fail:key_files_remain')
    const user = userEvent.setup()
    await openSigning()
    await user.click(within(rows('key-active')[0]!).getByRole('button', { name: /retire now/i }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /^retire the key$/i }))
    const hint = await within(dialog).findByTestId('task-error-hint', {}, { timeout: 4000 })
    expect(hint).toHaveTextContent(/still on that server/i)
    expect(hint).toHaveTextContent(/retried/i)
    // The task error is shown with the server's own text too.
    expect(dialog.textContent).toMatch(/10\.0\.0\.12/)
  })

  it('says which holder cannot open its key file when a passphrase does not fit', async () => {
    switches('signing=rotating,fail:passphrase_mismatch')
    const user = userEvent.setup()
    await openSigning()
    await user.click(screen.getByRole('button', { name: /^rotate the key$/i }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /^rotate the key$/i }))
    const hint = await within(dialog).findByTestId('task-error-hint', {}, { timeout: 4000 })
    expect(hint).toHaveTextContent(/cannot open it with its own passphrase/i)
    expect(hint).toHaveTextContent(/names the server/i)
    expect(getState().ledger7.signing.keys.filter((k) => k.state === 'active')).toHaveLength(2)
  })

  it('warns when the key files are not sealed, when this server cannot sign, and after a discontinuity', async () => {
    switches('signing=plain,alarm,discontinuity')
    await openSigning()
    expect(screen.getByTestId('not-protected')).toHaveTextContent(/not protected by a passphrase/i)
    expect(screen.getByTestId('signer-alarm')).toHaveTextContent(/does not sign/i)
    expect(screen.getByTestId('signer-alarm')).toHaveTextContent(
      /key file of the active key is missing/i,
    )
    expect(screen.getByTestId('discontinuity')).toHaveTextContent(
      /every auditor must replace their trust file/i,
    )
  })

  it('asks for the public key of an external signer, and nothing else that is secret', async () => {
    switches('signing=none,command')
    const user = userEvent.setup()
    await openSigning()
    await user.click(screen.getByRole('button', { name: /create the first key/i }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByLabelText(/public key of the external signer/i)).toBeInTheDocument()
    expect(within(dialog).getByText(/this is a public key/i)).toBeInTheDocument()
  })

  it('says there is no key to rotate when none is active, and does not offer to replace a lost key', async () => {
    for (const k of getState().ledger7.signing.keys) k.state = 'retired'
    await openSigning()
    expect(screen.getByTestId('no-active-key')).toHaveTextContent(
      /replacing a lost key is not available/i,
    )
    expect(screen.getByRole('button', { name: /^rotate the key/i })).toHaveAttribute(
      'aria-disabled',
      'true',
    )
  })

  it('says it as it is when the server does not build the signing state (a calm notice, not a failure)', async () => {
    mockConfig.unsupported.add('getLedgerSigning')
    await renderApp('/ledger?tab=signing')
    const notice = await screen.findByText(/not available on this server version yet/i)
    expect(notice.closest('[data-code]')).toHaveAttribute('data-code', 'unsupported_operation')
    expect(screen.queryByRole('button', { name: /create the first key/i })).not.toBeInTheDocument()
  })

  it('refuses a rotation the server refuses, in words, and never as success', async () => {
    const user = userEvent.setup()
    await openSigning()
    // Another administrator started a key task a moment ago.
    await user.click(screen.getByRole('button', { name: /^rotate the key$/i }))
    const dialog = await screen.findByRole('dialog')
    await (await import('@/api/endpoints')).endpoints.createLedgerSigningKey()
    await user.click(within(dialog).getByRole('button', { name: /^rotate the key$/i }))
    expect((await within(dialog).findAllByText(/in progress/i)).length).toBeGreaterThan(0)
    expect(within(dialog).queryByText('Finished.')).not.toBeInTheDocument()
  })

  it('does not let a read-only account act: the buttons say which role is needed', async () => {
    await openSigning('viewer')
    for (const name of [/^rotate the key/i, /^revoke/i]) {
      const b = screen.getAllByRole('button', { name })[0]!
      expect(b).toHaveAttribute('aria-disabled', 'true')
      expect(b.textContent).toMatch(/requires the kingres_admin cluster role/i)
    }
    expect(screen.getByRole('button', { name: /download trust file/i })).not.toHaveAttribute(
      'aria-disabled',
      'true',
    )
  })
})

describe('the Publication tab', () => {
  it('shows the listener, the logs, the addresses for auditors and what the publication reveals', async () => {
    await openPublication()
    expect(await screen.findByTestId('publication-enabled')).toHaveTextContent('Publication is on')
    await screen.findByTestId('listener')
    expect(screen.getByTestId('listener')).toHaveTextContent('0.0.0.0:8444')
    expect(screen.getByTestId('listener')).toHaveTextContent('Running')
    expect(screen.queryByTestId('plain-http')).not.toBeInTheDocument()
    const logs = await screen.findAllByTestId('log-row')
    // One chain log per shard group (the three groups of shop) and the aggregate log of shop (1 + 3).
    expect(logs.length).toBeGreaterThanOrEqual(4)
    expect(logs.some((l) => /Aggregate log/.test(l.textContent ?? ''))).toBe(true)
    const addresses = screen.getByTestId('addresses').textContent ?? ''
    expect(addresses).toContain('https://ledger.example.com/kingres-ledger')
    expect(addresses).toContain('/c/')
    expect(addresses).toContain('/a/')
    expect(addresses).not.toContain('10.0.0.11/kingres-ledger/c/')
    expect(screen.getByTestId('exposes').querySelectorAll('li')).toHaveLength(4)
    expect(document.body.textContent).toMatch(/does not reveal table names/i)
    expect(document.body.textContent).toMatch(/kingres-verify/)
  })

  it('shows the origin prefix as locked once a key exists, with the reason, and sends no prefix', async () => {
    const user = userEvent.setup()
    await openPublication()
    const prefix = (await screen.findByLabelText('Origin prefix')) as HTMLInputElement
    expect(prefix).toHaveAttribute('readonly')
    expect(screen.getByTestId('prefix-locked')).toBeInTheDocument()
    expect(document.body.textContent).toMatch(/another prefix needs a new cluster generation/i)
    const interval = screen.getByLabelText(/checkpoint interval/i)
    await user.clear(interval)
    await user.type(interval, '5')
    await user.click(screen.getByRole('button', { name: /save the settings/i }))
    expect((await screen.findAllByText(/from 10 to 3600/i)).length).toBeGreaterThan(0)
    await user.clear(interval)
    await user.type(interval, '120')
    await user.click(screen.getByRole('button', { name: /save the settings/i }))
    await waitFor(() => expect(getState().ledger7.settings.publication.intervalSeconds).toBe(120))
    expect(getState().ledger7.settings.publication.originPrefix).toBe('10.0.0.11/kingres-ledger')
  })

  it('lets the origin prefix be edited before the first key exists, and says it is only a proposal', async () => {
    switches('signing=none')
    const user = userEvent.setup()
    await openPublication()
    const prefix = (await screen.findByLabelText('Origin prefix')) as HTMLInputElement
    expect(prefix).not.toHaveAttribute('readonly')
    expect(document.body.textContent).toMatch(/proposal until then/i)
    expect(screen.queryByTestId('prefix-locked')).not.toBeInTheDocument()
    await user.clear(prefix)
    await user.type(prefix, 'x/y/')
    await user.click(screen.getByRole('button', { name: /save the settings/i }))
    expect(await screen.findByText(/without a trailing slash/i)).toBeInTheDocument()
    await user.clear(prefix)
    await user.type(prefix, 'ledger.example.com/kingres')
    await user.click(screen.getByRole('button', { name: /save the settings/i }))
    await waitFor(() =>
      expect(getState().ledger7.settings.publication.originPrefix).toBe(
        'ledger.example.com/kingres',
      ),
    )
  })

  it('says what is revealed before publication is turned on, and turns it on only when asked', async () => {
    switches('publication=off')
    const user = userEvent.setup()
    await openPublication()
    expect(await screen.findByTestId('publication-enabled')).toHaveTextContent('Publication is off')
    await user.click(screen.getByRole('button', { name: /turn publication on/i }))
    const dialog = await screen.findByRole('dialog', { name: /turn publication on/i })
    expect(dialog.textContent).toMatch(/time of every block/i)
    expect(dialog.textContent).toMatch(/does not reveal table names/i)
    expect(getState().ledger7.settings.publication.enabled).toBe(false)
    await user.click(within(dialog).getByRole('button', { name: /^turn publication on$/i }))
    await waitFor(() => expect(getState().ledger7.settings.publication.enabled).toBe(true))
    await waitFor(() =>
      expect(screen.getByTestId('publication-enabled')).toHaveTextContent('is on'),
    )
  })

  it('warns about plain HTTP and about a missing listener', async () => {
    switches('publication=plain')
    const first = await openPublication()
    expect(await screen.findByTestId('plain-http')).toHaveTextContent(/plain http/i)
    first.unmount()
    switches('publication=nolistener')
    await openPublication()
    expect(await screen.findByTestId('no-listener')).toHaveTextContent(/no listener is configured/i)
  })

  it('names a diverged log and keeps the settings usable when the server does not build the logs yet', async () => {
    switches('publication=diverged')
    const first = await openPublication()
    await waitFor(() =>
      expect(
        screen.getAllByTestId('log-row').some((l) => /Diverged/.test(l.textContent ?? '')),
      ).toBe(true),
    )
    first.unmount()
    switches('phase7=today')
    const user = userEvent.setup()
    await openPublication()
    const notice = await screen.findByText(/not available on this server version yet/i)
    expect(notice.closest('[data-code]')).toHaveAttribute('data-code', 'unsupported_operation')
    // The settings work on this server today.
    const interval = await screen.findByLabelText(/checkpoint interval/i)
    await user.clear(interval)
    await user.type(interval, '90')
    await user.click(screen.getByRole('button', { name: /save the settings/i }))
    await waitFor(() => expect(getState().ledger7.settings.publication.intervalSeconds).toBe(90))
  })

  it('asks for a public URL before it shows addresses', async () => {
    getState().ledger7.settings.publication.publicUrl = null
    await openPublication()
    expect(await screen.findByTestId('no-public-url')).toBeInTheDocument()
  })
})

describe('the alerts of the ledger', () => {
  it('link to the tab of the Ledger page where they are handled', async () => {
    switches('signing=alarm&publication=diverged')
    await renderApp('/alerts')
    const links = await screen.findAllByTestId('alert-ledger')
    const hrefs = links.map((l) => l.querySelector('a')?.getAttribute('href') ?? '')
    expect(hrefs.some((h) => h.includes('tab=signing'))).toBe(true)
    // A diverged chain is handled (acknowledged) on the Overview tab, which is the page without a tab in its address (console part D).
    expect(hrefs.some((h) => !h.includes('tab=') && h.includes('db=shop'))).toBe(true)
  })
})

describe('the dashboard card "Ledger integrity"', () => {
  async function card() {
    await renderApp('/dashboard')
    return await screen.findByTestId('ledger-card')
  }

  it('says "Not anchored" as information, never as healthy, when no target exists', async () => {
    const c = await card()
    await waitFor(() =>
      expect(within(c).getByTestId('ledger-card-state')).toHaveTextContent('Not anchored'),
    )
    expect(within(c).getByTestId('ledger-card-not-anchored')).toHaveTextContent(
      /can be rewritten undetected/,
    )
    expect(within(c).queryByText('Healthy')).not.toBeInTheDocument()
    expect(within(c).getByTestId('ledger-card-anchor')).toHaveTextContent('none')
    // The status of the answering server covers one group of three: the card does not claim the whole database.
    expect(within(c).getByTestId('ledger-card-chains')).toHaveTextContent('1 of 3')
    expect(within(c).getByTestId('ledger-card-verification')).toHaveTextContent(/ago/)
  })

  it('says "Healthy" with the age of the newest anchored aggregate when a target anchored it', async () => {
    switches('anchors=ok')
    mockConfig.ledgerStatusCoverage = 'all'
    const c = await card()
    await waitFor(() =>
      expect(within(c).getByTestId('ledger-card-state')).toHaveTextContent('Healthy'),
    )
    expect(within(c).getByTestId('ledger-card-anchor')).toHaveTextContent(/ago/)
    expect(within(c).getByTestId('ledger-card-chains')).toHaveTextContent('3 of 3')
  })

  it('says "Off" when no database uses the ledger', async () => {
    for (const d of getState().databases) d.info.ledger.mode = 'off'
    const c = await card()
    await waitFor(() => expect(within(c).getByTestId('ledger-card-state')).toHaveTextContent('Off'))
    expect(within(c).queryByTestId('ledger-card-anchor')).not.toBeInTheDocument()
  })

  it('says "Attention" with the worst alert and a link to the alerts', async () => {
    switches('anchors=failed,signing=alarm')
    const c = await card()
    const attention = await within(c).findByTestId('ledger-card-attention')
    expect(attention).toHaveTextContent('Critical')
    expect(attention).toHaveTextContent(
      /Ledger checkpoints cannot be signed|Ledger anchoring failed/,
    )
    expect(within(attention).getByRole('link', { name: /all alerts/i })).toBeInTheDocument()
    expect(within(c).getByTestId('ledger-card-state')).toHaveTextContent('Attention')
  })

  it('links to the tabs of the Ledger page', async () => {
    const c = await card()
    await within(c).findByRole('link', { name: 'Signing' })
    expect(within(c).getByRole('link', { name: 'Signing' }).getAttribute('href')).toContain(
      'tab=signing',
    )
    expect(within(c).getByRole('link', { name: 'Publication' }).getAttribute('href')).toContain(
      'tab=publication',
    )
  })

  it('is not shown to an account that cannot read the ledger status', async () => {
    await renderApp('/dashboard', { user: 'metrics' })
    await screen.findByRole('heading', { name: 'Dashboard' })
    expect(screen.queryByTestId('ledger-card')).not.toBeInTheDocument()
  })

  it('shows what the server says when it cannot read the status, and does not claim "Healthy"', async () => {
    mockConfig.unsupported.add('listLedgerStatus')
    mockConfig.unsupported.add('getLedgerStatus')
    const c = await card()
    await waitFor(() =>
      expect(within(c).getByText(/not available on this server version yet/i)).toBeInTheDocument(),
    )
    expect(within(c).queryByText('Healthy')).not.toBeInTheDocument()
  })
})

describe('text the server sends is only text', () => {
  it('on the Signing and Publication tabs and in the aggregate dialog', async () => {
    seedHostile(getState())
    const user = userEvent.setup()
    await openSigning()
    await screen.findByTestId('signer-alarm')
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect(document.querySelector('svg[onload]')).toBeNull()
    const holders = screen.getByTestId('holders').textContent ?? ''
    expect(holders).toMatch(/U\+202E/)
    expect(
      screen.getAllByTestId('event-type').some((e) => /U\+202E/.test(e.textContent ?? '')),
    ).toBe(true)
    expect(screen.getByTestId('cluster-id').textContent).toContain('<svg')
    expect(screen.getByTestId('signer-alarm').textContent).toContain(
      '2026-11-14 12:00:00 ERROR fake second log line',
    )
    await user.click(screen.getByRole('tab', { name: 'Publication' }))
    await screen.findByTestId('listener')
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect(screen.getByTestId('listener').textContent).toContain('<img')
    expect(document.querySelector('svg[onload]')).toBeNull()
    await user.click(screen.getByRole('tab', { name: 'Digests' }))
    await user.click((await screen.findAllByRole('button', { name: /open aggregate 41/i }))[0]!)
    const dialog = await screen.findByRole('dialog')
    await within(dialog).findByTestId('aggregate')
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect(within(dialog).getByTestId('aggregate-statement').textContent).toContain('<img')
    expect(
      within(dialog)
        .getAllByTestId(/^flag-/)
        .some((e) => e.textContent?.includes('<img')),
    ).toBe(true)
  })
})

describe('accessibility', () => {
  async function clean(container: Element) {
    expect(await axe(container)).toHaveNoViolations()
  }

  it('the four tabs have no violations', async () => {
    for (const path of [
      '/ledger',
      '/ledger?tab=digests',
      '/ledger?tab=signing',
      '/ledger?tab=publication',
    ]) {
      const { container, unmount } = await renderApp(path)
      await screen.findByRole('tablist', { name: 'Ledger' })
      await screen.findAllByRole('heading', { level: 2 })
      await new Promise((r) => setTimeout(r, 300))
      await clean(container)
      unmount()
    }
  })

  it('the dialogs have no violations: first key, rotation, retirement, revocation, aggregate', async () => {
    switches('signing=rotating')
    const user = userEvent.setup()
    const { container } = await openSigning()
    for (const [button, title] of [
      [/^rotate the key$/i, /rotate the signing key/i],
      [/retire now/i, /retire the signing key/i],
      [/^revoke/i, /revoke the signing key/i],
    ] as const) {
      await user.click(screen.getAllByRole('button', { name: button })[0]!)
      const dialog = await screen.findByRole('dialog', { name: title })
      await clean(container)
      await user.click(within(dialog).getByRole('button', { name: /^cancel$/i }))
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    }
  })

  it('the aggregate dialog and the first-key dialog have no violations', async () => {
    const user = userEvent.setup()
    const { container } = await renderApp('/ledger?tab=digests')
    await user.click(await screen.findByRole('button', { name: /open aggregate 41/i }))
    const dialog = await screen.findByRole('dialog')
    await within(dialog).findByTestId('aggregate')
    await clean(container)
    await user.click(within(dialog).getByRole('button', { name: /^close$/i }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    switches('signing=none')
    await user.click(screen.getByRole('tab', { name: 'Signing' }))
    await user.click(await screen.findByRole('button', { name: /create the first key/i }))
    await screen.findByRole('dialog', { name: /create the first signing key/i })
    await clean(container)
  })
})
