// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The hostile seed (`?mock=hostile`) over the places that 0.5.0 added: the partial-answer notice (server
// ids chosen by whoever controls a host name), alerts that name a server, and the certificate dialogs.
// Hidden characters are shown as U+ codes, markup stays text, nothing runs. One test per place.
import { afterEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { HOSTILE, HOSTILE_MISSING, seedHostile } from '@/mocks/hostile'
import { mockConfig } from '@/mocks/state'
import { renderApp, settled } from './helpers'

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

function expectSafePage(): void {
  const texts = [document.body.textContent ?? '']
  for (const el of Array.from(document.body.querySelectorAll('*')))
    for (const a of ['title', 'aria-label', 'placeholder']) {
      const v = el.getAttribute(a)
      if (v) texts.push(v)
    }
  for (const t of texts) expect(t).not.toMatch(RAW)
  expect(document.querySelector('img[src="x"]')).toBeNull()
  expect(document.querySelector('svg[onload]')).toBeNull()
  expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined()
}

afterEach(() => {
  delete (window as unknown as { __pwn?: number }).__pwn
})

function hostilePartial(): void {
  seedHostile(getState())
  mockConfig.partial = HOSTILE_MISSING
}

describe('the partial-answer notice shows hostile server ids safely', () => {
  for (const [path, page] of [
    ['/logs', 'Logs'],
    ['/alerts', 'Alerts'],
    ['/dashboard', 'Dashboard'],
  ] as const) {
    it(`${page}: every id of missingServers goes through SafeName`, async () => {
      hostilePartial()
      await renderApp(path)
      await settled()
      const notices = await screen.findAllByTestId('partial-notice')
      const text = notices.map((n) => n.textContent ?? '').join(' ')
      expect(text).toContain('U+202E') // the reversed id, visible
      expect(text).toContain('U+200B') // the zero-width id, visible
      expect(text).toContain('<img src=x onerror=window.__pwn=1>') // markup stayed text
      expect(text).toMatch(/Show all 5000 characters/) // the 5000-character id is folded
      expect(text).toMatch(/did not answer in time/)
      expect(text).toMatch(/could not be reached/)
      expect(text).toMatch(/refused the request/)
      expect(text).toMatch(/sent an answer that could not be used/)
      expectSafePage()
    })
  }

  it('what a screen reader is told names the hidden characters too', async () => {
    hostilePartial()
    await renderApp('/alerts')
    await screen.findAllByTestId('partial-notice')
    const polite = () => document.querySelector('.sr-only[aria-live="polite"]')?.textContent ?? ''
    await waitFor(() => expect(polite()).toMatch(/is incomplete/))
    expect(polite()).toContain('U+202E')
    expect(polite()).not.toMatch(RAW)
  })
})

describe('alerts that name a server with a hostile id', () => {
  it('shows the id through SafeName, as a link only when it can be addressed', async () => {
    seedHostile(getState())
    await renderApp('/alerts')
    await settled()
    const lines = await screen.findAllByTestId('alert-server')
    const reversed = lines.find((l) => l.textContent?.includes('U+202E'))
    expect(reversed).toBeTruthy()
    // The reversed id can be addressed (it is percent-encoded), so it is a link; `..` is not.
    expect(within(reversed as HTMLElement).getByRole('link')).toBeInTheDocument()
    const dots = lines.find((l) => (l.textContent ?? '').trim() === 'Server ..')
    expect(dots).toBeTruthy()
    expect(within(dots as HTMLElement).queryByRole('link')).toBeNull()
    expectSafePage()
  })
})

describe('the certificate dialogs and marks with a hostile server', () => {
  const bad = `host${String.fromCharCode(0x202e)}.example`

  it('the revoke dialog: id, address and title are escaped, and the id must still be typed exactly', async () => {
    const user = userEvent.setup()
    const rec = getState().servers.find((s) => s.id === '10.0.0.13')!
    rec.id = bad
    rec.address = `${HOSTILE.reversed}:8091`
    await renderApp(`/servers/${encodeURIComponent(bad)}`)
    await user.click(await screen.findByRole('button', { name: /^revoke certificates$/i }))
    const dialog = await screen.findByRole('dialog')
    expect(dialog.textContent).toContain('U+202E')
    expectSafePage()
    const confirm = within(dialog).getByRole('button', { name: /^revoke certificates$/i })
    expect(confirm).toBeDisabled()
    // The hint for hidden characters is shown, and the real id (not the drawn one) enables the button.
    expect(dialog).toHaveTextContent(/hidden characters/i)
    await user.click(within(dialog).getByRole('textbox'))
    await user.paste(bad)
    expect(confirm).toBeEnabled()
  })

  it('the renew dialog escapes the address', async () => {
    const user = userEvent.setup()
    getState().servers.find((s) => s.id === '10.0.0.13')!.address = `${HOSTILE.reversed}:8091`
    await renderApp('/servers/10.0.0.13')
    await user.click(await screen.findByRole('button', { name: /^renew now$/i }))
    const dialog = await screen.findByRole('dialog')
    expect(dialog.textContent).toContain('U+202E')
    expectSafePage()
  })

  it('the revoked mark in the list and on the page, with a hostile address', async () => {
    const rec = getState().servers.find((s) => s.id === '10.0.0.12')!
    rec.address = `${HOSTILE.reversed}:8091`
    rec.certificateRevokedAt = '2026-11-14T10:00:00.000Z'
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.12')
    expect(within(row).getByTestId('revoked-badge')).toBeInTheDocument()
    expectSafePage()
  })
})
