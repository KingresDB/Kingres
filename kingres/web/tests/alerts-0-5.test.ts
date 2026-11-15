// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.5.0 on alerts and paging: the new alert kinds and the link to the server an alert names,
// alert ids that are large integers (below 2^53) and must never be rounded or changed, and cursors,
// which changed format in 0.5.0: never kept anywhere, and a 400 for one starts the list again.
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { mockConfig } from '@/mocks/state'
import { fetchAllPages } from '@/api/queries'
import { ApiError } from '@/api/problem'
import type { Alert, LogEntry } from '@/api/types'
import { alertKindLabel } from '@/lib/status'
import { disposeBuiltApps, renderApp } from './helpers'

afterEach(() => mockServer.events.removeAllListeners())

const alert = (over: Partial<Alert>): Alert => ({
  id: 500,
  kind: 'certificateRenewalFailed',
  severity: 'warning',
  state: 'open',
  title: 'Certificate renewal failed',
  message: 'The renewal job failed three times in a row.',
  serverId: '10.0.0.12',
  databaseName: null,
  raisedAt: new Date().toISOString(),
  ...over,
})

describe('alert kinds of 0.5.0', () => {
  it('has words for every kind, none shown as the raw name', () => {
    for (const kind of [
      'certificateRenewalFailed',
      'certificateExpiring',
      'ledgerPendingTooOld',
      'versionMismatch',
    ])
      expect(alertKindLabel(kind), kind).not.toBe(kind)
    expect(alertKindLabel('certificateRenewalFailed')).toBe('Certificate renewal failed')
    // A kind of a newer server is shown as sent.
    expect(alertKindLabel('somethingNew')).toBe('somethingNew')
  })

  it('links an alert to the server it names, and shows the server through SafeName', async () => {
    getState().alerts.unshift(alert({ id: 501, serverId: '10.0.0.12' }))
    await renderApp('/alerts')
    const where = (await screen.findAllByTestId('alert-server'))[0] as HTMLElement
    const link = within(where).getByRole('link', { name: '10.0.0.12' })
    expect(link).toHaveAttribute('href', '/servers/10.0.0.12')
    expect(screen.getAllByText('Certificate renewal failed').length).toBeGreaterThan(0)
  })

  it('does not make a link of a server id that no URL can carry', async () => {
    getState().alerts.unshift(alert({ id: 502, serverId: '..' }))
    await renderApp('/alerts')
    const where = (await screen.findAllByTestId('alert-server'))[0] as HTMLElement
    expect(where).toHaveTextContent('..')
    expect(within(where).queryByRole('link')).toBeNull()
  })

  it('an alert without a server has no server line', async () => {
    getState().alerts = [alert({ id: 503, serverId: null, kind: 'versionMismatch' })]
    await renderApp('/alerts')
    await screen.findByText('Servers run different versions')
    expect(screen.queryByTestId('alert-server')).toBeNull()
  })
})

describe('alert ids are large integers (below 2^53)', () => {
  const BIG = 2 ** 52 + 1 // 4503599627370497

  it('lists, and acknowledges, an alert with id 2^52+1 without changing a digit', async () => {
    const user = userEvent.setup()
    const seen: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      seen.push(`${request.method} ${new URL(request.url).pathname}`)
    })
    const bodies: string[] = []
    const realFetch = globalThis.fetch
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const res = await realFetch(input, init)
      if (String(res.url).includes('/alerts') && !String(res.url).includes('acknowledgement'))
        bodies.push(await res.clone().text())
      return res
    }) as typeof fetch
    try {
      getState().alerts = [alert({ id: BIG, title: 'Big id' })]
      await renderApp('/alerts')
      await screen.findByText('Big id')
      // The server really sent that exact number, and the panel read it back exactly.
      expect(bodies.join('')).toContain(`"id":${BIG}`)
      expect(String(BIG)).toBe('4503599627370497')
      await user.click(screen.getByRole('button', { name: /acknowledge alert big id/i }))
      await waitFor(() => expect(getState().alerts[0]?.state).toBe('acknowledged'))
      expect(seen).toContain(`PUT /api/v1/alerts/${BIG}/acknowledgement`)
      expect(getState().alerts[0]?.id).toBe(BIG)
    } finally {
      globalThis.fetch = realFetch
    }
  })

  it('does not send an acknowledgement for an id that a JSON number cannot hold exactly', async () => {
    const seen: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      seen.push(`${request.method} ${new URL(request.url).pathname}`)
    })
    getState().alerts = [alert({ id: 2 ** 53 + 2, title: 'Too big' })]
    await renderApp('/alerts')
    await screen.findByText('Too big')
    expect(screen.queryByRole('button', { name: /acknowledge/i })).toBeNull()
    expect(document.body).toHaveTextContent(/cannot acknowledge this alert/i)
    expect(seen.filter((r) => r.startsWith('PUT'))).toEqual([])
  })
})

describe('cursors (changed format in 0.5.0)', () => {
  const manyAlerts = (n: number): Alert[] =>
    Array.from({ length: n }, (_, i) => alert({ id: 1000 + i, title: `Alert number ${i}` }))
  const manyLogs = (n: number): LogEntry[] =>
    Array.from({ length: n }, (_, i) => ({
      timestamp: new Date(Date.now() - i * 1000).toISOString(),
      level: 'info' as const,
      component: 'api' as const,
      serverId: '10.0.0.11',
      instanceId: null,
      message: `line ${i}`,
    }))

  it('never keeps a cursor: not in storage, not in the address', async () => {
    const user = userEvent.setup()
    getState().alerts = manyAlerts(150)
    const { router } = await renderApp('/alerts')
    await user.click(await screen.findByRole('button', { name: /load more/i }))
    await screen.findByText('Alert number 120')
    expect(router.currentRoute.value.fullPath).toBe('/alerts')
    expect(location.href).not.toMatch(/cursor/i)
    for (const store of [localStorage, sessionStorage])
      for (let i = 0; i < store.length; i += 1) {
        const key = store.key(i) ?? ''
        expect(key.toLowerCase()).not.toContain('cursor')
        expect(store.getItem(key) ?? '').not.toMatch(/cursor/i)
      }
    // A reload (a new app) starts at the first page again.
    cleanup()
    disposeBuiltApps()
    const again = await renderApp('/alerts')
    await screen.findAllByText('Alert number 0')
    expect(screen.queryByText('Alert number 120')).toBeNull()
    expect(again.router.currentRoute.value.fullPath).toBe('/alerts')
  })

  it('Alerts: a 400 for a cursor starts again from the first page, with a short notice and no error', async () => {
    const user = userEvent.setup()
    getState().alerts = manyAlerts(150)
    await renderApp('/alerts')
    await screen.findByText('Alert number 0')
    mockConfig.rejectCursors = true // the server was updated: the old cursor is refused
    await user.click(screen.getByRole('button', { name: /load more/i }))
    const notice = await screen.findByTestId('cursor-reset-notice')
    expect(notice).toHaveTextContent(/started again from the first page/i)
    // The first page is shown, not an error dead end.
    await screen.findByText('Alert number 0')
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.queryByText('Alert number 120')).toBeNull()
    // And it works again once the server accepts cursors.
    mockConfig.rejectCursors = false
    await user.click(await screen.findByRole('button', { name: /load more/i }))
    await screen.findByText('Alert number 120')
    await user.click(within(notice).getByRole('button', { name: /dismiss/i }))
    expect(screen.queryByTestId('cursor-reset-notice')).toBeNull()
  })

  it('Logs: the same', async () => {
    const user = userEvent.setup()
    getState().logs = manyLogs(150)
    await renderApp('/logs')
    await screen.findByText('line 0')
    mockConfig.rejectCursors = true
    await user.click(await screen.findByRole('button', { name: /load more/i }))
    expect(await screen.findByTestId('cursor-reset-notice')).toBeInTheDocument()
    await screen.findByText('line 0')
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('the first page carries no cursor, so a server that refuses cursors does not affect it', async () => {
    getState().alerts = manyAlerts(3)
    mockConfig.rejectCursors = true // the first page carries no cursor: unaffected
    await renderApp('/alerts')
    await screen.findByText('Alert number 0')
    expect(screen.queryByTestId('cursor-reset-notice')).toBeNull()
  })

  it('fetchAllPages reads the list again from the first page, once, when a cursor is refused', async () => {
    const calls: (string | undefined)[] = []
    let refused = false
    const result = await fetchAllPages<number>(async (cursor) => {
      calls.push(cursor)
      if (cursor === 'c2' && !refused) {
        refused = true
        throw new ApiError({ type: 'about:blank', title: 'bad', status: 400, code: 'bad_request' })
      }
      if (cursor === undefined) return { items: [1], nextCursor: 'c2' }
      return { items: [2], nextCursor: null }
    })
    expect(calls).toEqual([undefined, 'c2', undefined, 'c2'])
    expect(result).toEqual({ items: [1, 2], moreNotShown: false, restarted: true })
    // A second refusal is an error: no endless restart.
    await expect(
      fetchAllPages<number>(async (cursor) => {
        if (cursor)
          throw new ApiError({
            type: 'about:blank',
            title: 'bad',
            status: 400,
            code: 'bad_request',
          })
        return { items: [1], nextCursor: 'c2' }
      }),
    ).rejects.toBeInstanceOf(ApiError)
  })
})
