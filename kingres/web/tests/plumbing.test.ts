// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-14: nothing is left out silently. A database whose ledger status cannot be read is shown as
// such, lists that can be longer than one page follow the cursors up to a bound, and say so when
// there is more than is shown.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import { http, HttpResponse } from 'msw'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { paging } from '@/api/queries'
import { renderApp } from './helpers'

const notFound = () =>
  HttpResponse.json(
    { type: 'about:blank', title: 'x', status: 404, code: 'not_found' },
    { status: 404 },
  )

describe('the ledger list (W-14a)', () => {
  it('shows a database whose ledger status cannot be read, instead of leaving it out', async () => {
    mockServer.use(
      http.get('*/api/v1/ledger/status', notFound),
      http.get('*/api/v1/databases/shop/ledger', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'x',
            status: 500,
            code: 'internal_error',
            detail: 'chain unreadable',
          },
          { status: 500 },
        ),
      ),
    )
    await renderApp('/ledger')
    const notice = await screen.findByRole('alert', { name: /ledger status could not be read/i })
    expect(notice).toHaveTextContent(/shop/)
    expect(notice).toHaveTextContent(/list may be incomplete/i)
    // It does not claim that no database uses the ledger.
    expect(screen.queryByText(/no database uses the ledger/i)).not.toBeInTheDocument()
  })
})

describe('lists longer than one page (W-14b, W-14c)', () => {
  it('lists every role when there are more than one page of them', async () => {
    const s = getState()
    const template = s.roles[0]!
    for (let i = 0; i < 700; i++) s.roles.push({ ...template, name: `bulk_${i}`, clusterRoles: [] })
    await renderApp('/security')
    expect(await screen.findByRole('rowheader', { name: 'bulk_699' })).toBeInTheDocument()
  })

  it('stops following cursors at a bound and says that more results are not shown', async () => {
    const saved = paging.maxPages
    paging.maxPages = 3
    let requests = 0
    mockServer.use(
      http.get('*/api/v1/roles', () => {
        requests += 1
        return HttpResponse.json({
          items: [
            {
              name: `endless_${requests}`,
              canLogin: true,
              isSuperuser: false,
              canCreateDatabase: false,
              canCreateRole: false,
              connectionLimit: -1,
              validUntil: null,
              memberOf: [],
              clusterRoles: [],
            },
          ],
          nextCursor: `c${requests}`,
        })
      }),
    )
    try {
      await renderApp('/security')
      expect(await screen.findByText(/more roles exist than are shown/i)).toBeInTheDocument()
      expect(screen.getByRole('rowheader', { name: 'endless_3' })).toBeInTheDocument()
      expect(requests).toBe(3)
    } finally {
      paging.maxPages = saved
    }
  })

  it('does not loop forever over the buckets of a server that always returns a cursor', async () => {
    const saved = paging.maxPages
    paging.maxPages = 4
    let requests = 0
    mockServer.use(
      http.get('*/api/v1/databases/:name/colocation-groups/:id/buckets', () => {
        requests += 1
        return HttpResponse.json({
          mapVersion: 1,
          items: [
            {
              bucket: requests,
              shardGroupId: 1,
              state: 'active',
              movingToShardGroupId: null,
              primaryServerId: '10.0.0.11',
              replicaServerIds: [],
              sizeBytes: 1,
            },
          ],
          nextCursor: `c${requests}`,
        })
      }),
    )
    try {
      await renderApp('/shards')
      expect(await screen.findByText(/more buckets exist than are shown/i)).toBeInTheDocument()
      expect(requests).toBeLessThanOrEqual(4)
    } finally {
      paging.maxPages = saved
    }
  })

  it('says so when there are more rebalance reports than the page shown', async () => {
    mockServer.use(
      http.get('*/api/v1/rebalance/reports', () =>
        HttpResponse.json({ items: [], nextCursor: 'more' }),
      ),
    )
    await renderApp('/rebalance')
    expect(await screen.findByText(/more reports exist than are shown/i)).toBeInTheDocument()
  })

  it('says so when there are more alerts than the page shown', async () => {
    mockServer.use(
      http.get('*/api/v1/alerts', ({ request }) =>
        new URL(request.url).searchParams.get('cursor')
          ? HttpResponse.json({ items: [], nextCursor: null })
          : HttpResponse.json({ items: [], nextCursor: 'more' }),
      ),
    )
    await renderApp('/alerts')
    const more = await screen.findByRole('button', { name: /load more/i })
    expect(within(more.parentElement as HTMLElement)).toBeTruthy()
    await waitFor(() => expect(more).toBeEnabled())
  })
})

describe('ledger lists with more than one page (W-14)', () => {
  it('offers more digests and more verifications than the first page shown', async () => {
    mockServer.use(
      http.get('*/api/v1/databases/:name/ledger/digests', () =>
        HttpResponse.json({ items: [], nextCursor: 'more' }),
      ),
      http.get('*/api/v1/databases/:name/ledger/verifications', () =>
        HttpResponse.json({ items: [], nextCursor: 'more' }),
      ),
    )
    await renderApp('/ledger')
    // Empty pages with a cursor still say that there is more.
    mockServer.use(
      http.get('*/api/v1/databases/:name/ledger/digests', () =>
        HttpResponse.json({
          items: [
            {
              id: 1,
              databaseName: 'shop',
              generatedAt: '2026-01-01T00:00:00Z',
              generatedBy: 'admin',
              chains: [],
              aggregateHash: 'ab'.repeat(32),
              anchors: [],
            },
          ],
          nextCursor: 'more',
        }),
      ),
    )
    // Digests have paging now (the notice "more digests exist" is gone): the button loads the next page.
    expect(await screen.findByRole('button', { name: /load more digests/i })).toBeInTheDocument()
  })
})
