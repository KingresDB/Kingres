// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The production build, served with the exact headers the server must send (see the README), runs
// every page without a single content security policy violation. The API is answered by the mock
// handlers in this process (the production build contains no mock), which are reached through the
// browser's request interception.
import { readdir, readFile } from 'node:fs/promises'
import path from 'node:path'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import type { Browser, HTTPRequest, Page } from 'puppeteer-core'
import { createHandlers } from '@/mocks/handlers'
import { getState, resetMockState } from '@/mocks/http'
import { seedHostile } from '@/mocks/hostile'
import { mockConfig } from '@/mocks/state'
import { applySwitches } from '@/mocks/switches'
import { setupServer } from 'msw/node'
import { CONTENT_SECURITY_POLICY, COMMON_HEADERS } from './server-headers'
import {
  HA_SWITCHES,
  ROUTES,
  SECURITY_TABS,
  STATES,
  buildProduction,
  clickText,
  launch,
  settled,
  signIn,
  sleep,
  startStaticServer,
  type RunningServer,
} from './harness'

const mock = setupServer(...createHandlers())
let server: RunningServer
let cleanup: () => Promise<void>
let distDir = ''
let browser: Browser
let page: Page
const violations: string[] = []
const consoleMessages: string[] = []
const indexHeaders: Record<string, string>[] = []

/** Answers /api/ requests with the mock handlers of this process; everything else is the build. */
async function intercept(req: HTTPRequest): Promise<void> {
  const url = new URL(req.url())
  if (!url.pathname.startsWith('/api/v1/')) {
    await req.continue()
    return
  }
  const headers = Object.fromEntries(
    Object.entries(req.headers()).filter(
      ([k]) => !['host', 'content-length', 'connection', 'origin', 'referer'].includes(k),
    ),
  )
  const body = req.postData()
  const res = await fetch(req.url(), {
    method: req.method(),
    headers,
    body: body && req.method() !== 'GET' ? body : undefined,
  })
  const buffer = Buffer.from(await res.arrayBuffer())
  await req.respond({
    status: res.status,
    headers: {
      ...Object.fromEntries(res.headers.entries()),
      ...COMMON_HEADERS,
      'Cache-Control': 'no-store',
    },
    body: buffer,
  })
}

beforeAll(async () => {
  mock.listen({ onUnhandledFrame: 'bypass' })
  mockConfig.latencyMs = 0
  mockConfig.taskDurationMs = 600
  resetMockState()
  seedHostile(getState())
  // The states of contract 0.7.0: a lost lease, a dead server, a standby that does not stream, the client entry point.
  applySwitches(new URLSearchParams(HA_SWITCHES), getState())
  const built = await buildProduction()
  cleanup = built.cleanup
  distDir = built.dir
  server = await startStaticServer(built.dir)
  browser = await launch()
  page = await browser.newPage()
  await page.setRequestInterception(true)
  page.on('request', (req) => void intercept(req).catch(() => req.abort().catch(() => undefined)))
  page.on('response', (res) => {
    if (res.request().resourceType() === 'document') indexHeaders.push(res.headers())
  })
  page.on('console', (m) => consoleMessages.push(`${m.type()}: ${m.text()}`))
  page.on('pageerror', (e) => consoleMessages.push(`pageerror: ${(e as Error).message}`))
  await page.evaluateOnNewDocument(() => {
    window.__csp = []
    document.addEventListener('securitypolicyviolation', (event: Event) => {
      const e = event as SecurityPolicyViolationEvent
      window.__csp?.push(`${e.violatedDirective} blocked ${e.blockedURI} ${e.sample}`)
    })
  })
})
afterAll(async () => {
  await browser?.close()
  await server?.close()
  await cleanup?.()
  mock.close()
})

async function collect(where: string): Promise<void> {
  const found = await page.evaluate(() => {
    const list = window.__csp ?? []
    window.__csp = []
    return list
  })
  for (const v of found) violations.push(`${where}: ${v}`)
}

describe('the production build under the content security policy', () => {
  it('detects a violation when there is one (the check itself works)', async () => {
    await page.goto(`${server.url}/login`, { waitUntil: 'networkidle0' })
    await page.evaluate(() => {
      document.body.insertAdjacentHTML('beforeend', '<img src="http://blocked.invalid/x.png">')
    })
    await sleep(200)
    const found = await page.evaluate(() => window.__csp ?? [])
    expect(found.join(' ')).toMatch(/img-src/)
    // An inline style attribute (what a statically hoisted template would produce) is reported too.
    await page.evaluate(() => {
      window.__csp = []
      document.body.insertAdjacentHTML('beforeend', '<p style="color: red">x</p>')
    })
    await sleep(200)
    expect((await page.evaluate(() => window.__csp ?? [])).join(' ')).toMatch(/style-src/)
    await page.evaluate(() => {
      window.__csp = []
    })
    // The deliberate violation above is not a finding.
    consoleMessages.length = 0
  })

  it('the build holds no mock API and no service worker', async () => {
    const files = await readdir(path.join(distDir, 'assets'))
    const text = (
      await Promise.all(
        files
          .filter((f) => f.endsWith('.js'))
          .map((f) => readFile(path.join(distDir, 'assets', f), 'utf8')),
      )
    ).join(' ')
    expect(text).not.toMatch(/mockServiceWorker|demo-password|failme/)
    expect(files.some((f) => f.startsWith('browser-'))).toBe(false)
  })

  it('is served with the policy of the README', async () => {
    expect(indexHeaders[0]?.['content-security-policy']).toBe(CONTENT_SECURITY_POLICY)
    expect(indexHeaders[0]?.['x-frame-options']).toBe('DENY')
  })

  it('signs in and shows every page, with hostile data, without a violation', async () => {
    await page.goto(`${server.url}/login`, { waitUntil: 'networkidle0' })
    await collect('login')
    await signIn(page)
    await settled(page)
    await collect('dashboard')
    for (const route of ROUTES.filter((r) => r !== '/dashboard')) {
      // The production build has no router hook: navigate by the address, which reloads the
      // application, so sign in again each time through the cookie-less mock session (kept in
      // this process).
      await page.goto(`${server.url}${route}`, { waitUntil: 'networkidle0' })
      await settled(page)
      if (route === '/workbench') {
        await page.type('#w-sql', ' where 1 = 1')
        await clickText(page, 'Run')
        await sleep(400)
      }
      if (route === '/security') {
        for (const tab of SECURITY_TABS) {
          await clickText(page, tab, '[role="tab"]')
          await settled(page)
          await collect(`${route} ${tab}`)
        }
      }
      await collect(route)
    }
  })

  it('opens the dialogs, runs a task and a result table without a violation', async () => {
    await page.goto(`${server.url}/databases`, { waitUntil: 'networkidle0' })
    await settled(page)
    await clickText(page, 'Create database')
    await page.waitForSelector('[role="dialog"]')
    await collect('create database dialog')
    await clickText(page, 'Cancel')

    await page.goto(`${server.url}/servers`, { waitUntil: 'networkidle0' })
    await settled(page)
    await clickText(page, 'Restore preferred primaries')
    await page.waitForSelector('[role="dialog"]')
    await clickText(page, 'Restore primaries')
    await page.waitForFunction(
      () => document.querySelector('[role="dialog"]')?.textContent?.includes('Finished'),
      { timeout: 20_000 },
    )
    await collect('task dialog')
    await clickText(page, 'Close')

    await page.goto(`${server.url}/security`, { waitUntil: 'networkidle0' })
    await settled(page)
    await clickText(page, 'Network access', '[role="tab"]')
    await settled(page)
    await page.type('input[aria-label="Rule 1 comment"]', '!')
    await clickText(page, 'Review and apply')
    await page.waitForSelector('[role="dialog"]')
    await collect('network review dialog')
  })

  it('shows the 0.4.0 and 0.5.0 states (certificate dialogs, proof and digests, add server, leave cluster, stopped system database) without a violation', async () => {
    for (const state of STATES) {
      await page.goto(`${server.url}${state.route}`, { waitUntil: 'networkidle0' })
      await settled(page)
      await state.open(page)
      await collect(state.name)
      await state.close?.(page)
      await collect(`${state.name} (closed)`)
    }
  })

  it('shows the 0.6.0 states (reload while stopped, incomplete ledger, partial audit log) without a violation', async () => {
    // Reload while the system database is stopped: nothing is kept in the browser, the server answers
    // getSession with degraded: true and the instance to start comes from listThisServerInstances.
    const stopped = STATES.find((s) => s.name === 'system database stopped')
    if (!stopped) throw new Error('state missing')
    await page.goto(`${server.url}/this-server`, { waitUntil: 'networkidle0' })
    await settled(page)
    await stopped.open(page)
    await page.reload({ waitUntil: 'networkidle0' })
    await page.waitForSelector('[data-testid="instance-known"]', { timeout: 30_000 })
    await collect('stopped state after a reload (degraded session)')
    const stored = await page.evaluate(() => [
      ...Object.keys(localStorage),
      ...Object.keys(sessionStorage),
    ])
    expect(stored.filter((k) => k !== 'kingres.theme')).toEqual([])
    await stopped.close?.(page)
    await collect('stopped state ended')

    mockConfig.incompleteLedger = true
    await page.goto(`${server.url}/ledger`, { waitUntil: 'networkidle0' })
    await settled(page)
    await clickText(page, 'Generate digest')
    await page.waitForSelector('[data-testid^="digest-coverage-"]', { timeout: 20_000 })
    await collect('incomplete digest')
    mockConfig.incompleteLedger = false

    mockConfig.partial = [{ serverId: '10.0.0.13', reason: 'timeout' }]
    await page.goto(`${server.url}/security`, { waitUntil: 'networkidle0' })
    await settled(page)
    await clickText(page, 'Audit log', '[role="tab"]')
    await page.waitForSelector('[data-testid="partial-notice"]', { timeout: 20_000 })
    await collect('partial audit log')
    mockConfig.partial = null
  })

  it('runs the sign-in page and the first-run wizard without a violation', async () => {
    resetMockState({ uninitialized: true })
    await page.goto(`${server.url}/`, { waitUntil: 'networkidle0' })
    await page.waitForSelector('input')
    await page.type('input', 'demo-setup-token')
    await clickText(page, 'Continue')
    await clickText(page, 'Create new cluster')
    await page.waitForSelector('form')
    await collect('setup wizard')
    resetMockState()
  })

  it('had no violation anywhere, and no console error from the page', async () => {
    expect(violations).toEqual([])
    expect(consoleMessages.filter((m) => /Content Security Policy|Refused to/i.test(m))).toEqual([])
    expect(consoleMessages.filter((m) => m.startsWith('pageerror'))).toEqual([])
  })
})
