// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The client: path parameters (W-15), answers that are not problem documents (W-14d), unknown
// problem codes (W-14e), and the sign-in redirect allow-list (W-17).
import { describe, expect, it } from 'vitest'
import { screen, waitFor } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { endpoints } from '@/api/endpoints'
import { ApiError, describeProblem } from '@/api/problem'
import { useSessionStore } from '@/stores/session'
import { renderApp } from './helpers'

function recordRequests(): URL[] {
  const seen: URL[] = []
  mockServer.events.on('request:start', ({ request }) => {
    seen.push(new URL(request.url))
  })
  return seen
}

describe('path parameters address exactly the object that is shown (W-15)', () => {
  const hostile = [
    ['a/b', 'a%2Fb'],
    ['what?', 'what%3F'],
    ['a#b', 'a%23b'],
    ['100%', '100%25'],
    ['%2e%2e', '%252e%252e'],
    ['a b', 'a%20b'],
    ['tab\u0000nul', 'tab%00nul'],
  ] as const

  for (const [name, encoded] of hostile) {
    it(`sends ${JSON.stringify(name)} as one encoded segment`, async () => {
      await renderApp('/dashboard')
      const seen = recordRequests()
      let received: string | undefined
      mockServer.use(
        http.get('*/api/v1/databases/:databaseName', ({ params }) => {
          received = String(params.databaseName)
          return HttpResponse.json(
            { code: 'not_found', title: 'x', status: 404, type: 'about:blank' },
            { status: 404 },
          )
        }),
      )
      await expect(endpoints.getDatabase(name)).rejects.toBeInstanceOf(ApiError)
      const url = seen.find((u) => u.pathname.includes('/databases/'))
      expect(url?.pathname).toBe(`/api/v1/databases/${encoded}`)
      expect(url?.search).toBe('')
      expect(url?.hash).toBe('')
      expect(received).toBe(name)
    })
  }

  for (const name of ['.', '..', '']) {
    it(`refuses ${JSON.stringify(name)}, which no URL can carry, without sending a request`, async () => {
      await renderApp('/dashboard')
      const seen = recordRequests()
      await expect(endpoints.getDatabase(name)).rejects.toMatchObject({
        name: 'ApiError',
        code: 'bad_request',
      })
      await expect(endpoints.getTable('shop', 'public', name)).rejects.toBeInstanceOf(ApiError)
      await expect(
        endpoints.enableTableLedger('shop', 'public', name, 'appendOnly'),
      ).rejects.toBeInstanceOf(ApiError)
      expect(seen.filter((u) => u.pathname.startsWith('/api/v1/databases'))).toHaveLength(0)
    })
  }
})

describe('answers that are not problem documents (W-14)', () => {
  it('treats a 401 page from a proxy as the end of the session', async () => {
    const { router } = await renderApp('/servers')
    await screen.findByTestId('server-row-10.0.0.11')
    mockServer.use(
      http.get(
        '*/api/v1/servers',
        () =>
          new HttpResponse('<html>Unauthorized</html>', {
            status: 401,
            headers: { 'Content-Type': 'text/html' },
          }),
      ),
    )
    await router.push('/dashboard')
    await router.push('/servers')
    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'), { timeout: 3000 })
  })

  it('maps a 403 page to "forbidden", not to "bad request"', async () => {
    await renderApp('/dashboard')
    mockServer.use(http.get('*/api/v1/servers', () => new HttpResponse('nope', { status: 403 })))
    await expect(endpoints.listServers()).rejects.toMatchObject({ code: 'forbidden' })
  })

  it('keeps the detail of a problem code this panel does not know', () => {
    const error = new ApiError({
      type: 'about:blank',
      title: 'New thing',
      status: 409,
      code: 'brand_new_code' as never,
      detail: 'The thing is in the way.',
    })
    const view = describeProblem(error)
    expect(view.message).toContain('The thing is in the way.')
  })
})

describe('the sign-in redirect stays inside the panel (W-17)', () => {
  const cases: [string, string][] = [
    ['/servers', 'servers'],
    ['//evil.example', 'dashboard'],
    ['https://evil.example/', 'dashboard'],
    ['/\\evil.example', 'dashboard'],
    ['\\\\evil.example', 'dashboard'],
    ['javascript:alert(1)', 'dashboard'],
    ['/%0d%0aSet-Cookie:x', 'dashboard'],
    ['/login', 'dashboard'],
  ]
  for (const [redirect, landing] of cases) {
    it(`${JSON.stringify(redirect)} lands on ${landing}`, async () => {
      const user = userEvent.setup()
      const { router } = await renderApp('/login?redirect=' + encodeURIComponent(redirect), {
        user: null,
      })
      await user.type(await screen.findByLabelText('User name'), 'admin')
      await user.type(screen.getByLabelText('Password'), 'demo-password')
      await user.click(screen.getByRole('button', { name: /^sign in$/i }))
      await waitFor(() => expect(useSessionStore().isSignedIn).toBe(true))
      await waitFor(() => expect(router.currentRoute.value.name).toBe(landing))
      expect(router.currentRoute.value.path).not.toContain('evil')
    })
  }
})
