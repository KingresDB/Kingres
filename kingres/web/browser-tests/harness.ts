// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Shared pieces of the browser tests: a Vite dev server (the mock API runs in the page as a
// service worker), a static server for the production build with the headers the real server
// must send, a headless Chrome or Edge, and page helpers.
import { existsSync } from 'node:fs'
import { mkdtemp, readFile, rm, stat } from 'node:fs/promises'
import { createServer as createHttpServer, type Server } from 'node:http'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import puppeteer, { type Browser, type Page } from 'puppeteer-core'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import { createServer as createViteServer } from 'vite'
import { CACHE_HEADERS, COMMON_HEADERS } from './server-headers'

export const WEB_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

const CANDIDATES = [
  process.env.CHROME_PATH,
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
]

export function findBrowser(): string {
  const found = CANDIDATES.find((p) => p && existsSync(p))
  if (!found) throw new Error('No Chrome or Edge found. Set CHROME_PATH.')
  return found
}

export async function launch(): Promise<Browser> {
  return puppeteer.launch({
    executablePath: findBrowser(),
    headless: true,
    args: ['--no-sandbox'],
    defaultViewport: { width: 1366, height: 820 },
  })
}

export interface RunningServer {
  url: string
  close: () => Promise<void>
}

/** The Vite dev server; the mock API is started in the page (development build only). */
export async function startDevServer(): Promise<RunningServer> {
  const server = await createViteServer({
    root: WEB_ROOT,
    configFile: path.join(WEB_ROOT, 'vite.config.ts'),
    logLevel: 'error',
    server: { port: 5290, strictPort: false, hmr: false },
  })
  await server.listen()
  const url = server.resolvedUrls?.local[0]
  if (!url) throw new Error('The dev server has no address.')
  return { url: url.replace(/\/$/, ''), close: () => server.close() }
}

const TYPES: Record<string, string> = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.webmanifest': 'application/manifest+json',
}

/**
 * Builds the production bundle into a temporary directory, with the same command as `npm run build`
 * (a child process: inside the test runner NODE_ENV is "test", which would put the mock API into
 * the build).
 */
export async function buildProduction(): Promise<{ dir: string; cleanup: () => Promise<void> }> {
  const dir = await mkdtemp(path.join(tmpdir(), 'kingres-dist-'))
  const vite = path.join(WEB_ROOT, 'node_modules', 'vite', 'bin', 'vite.js')
  await promisify(execFile)(process.execPath, [vite, 'build', '--outDir', dir, '--emptyOutDir'], {
    cwd: WEB_ROOT,
    env: { ...process.env, NODE_ENV: 'production', VITEST: undefined, MODE: undefined },
  })
  return { dir, cleanup: () => rm(dir, { recursive: true, force: true }) }
}

/**
 * Serves a production build the way the real server must: the headers of `server-headers.ts`,
 * `no-cache` for index.html, long-lived caching for the hashed files, and the single-page
 * fallback for paths that are not files and not under /api/.
 */
export async function startStaticServer(
  dir: string,
  headers: Record<string, string> = COMMON_HEADERS,
): Promise<RunningServer> {
  const server: Server = createHttpServer(async (req, res) => {
    const url = new URL(req.url ?? '/', 'http://localhost')
    const send = async (file: string, cache: string) => {
      const body = await readFile(file)
      res.writeHead(200, {
        ...headers,
        'Content-Type': TYPES[path.extname(file)] ?? 'application/octet-stream',
        'Cache-Control': cache,
      })
      res.end(body)
    }
    try {
      if (url.pathname.startsWith('/api/')) {
        res.writeHead(404, { ...headers, 'Cache-Control': CACHE_HEADERS.api })
        res.end('{}')
        return
      }
      const file = path.join(dir, path.normalize(url.pathname))
      if (file.startsWith(dir) && (await stat(file).catch(() => null))?.isFile()) {
        await send(
          file,
          url.pathname.startsWith('/assets/') ? CACHE_HEADERS.hashedAsset : 'no-cache',
        )
        return
      }
      await send(path.join(dir, 'index.html'), CACHE_HEADERS.indexHtml)
    } catch {
      res.writeHead(500)
      res.end()
    }
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  const address = server.address()
  if (!address || typeof address === 'string') throw new Error('No address')
  return {
    url: `http://127.0.0.1:${address.port}`,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  }
}

export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

declare global {
  interface Window {
    __router?: { push: (to: string) => Promise<unknown> }
    axe: {
      run: (
        context: Document,
        options: object,
      ) => Promise<{
        violations: {
          id: string
          impact: string | null
          help: string
          nodes: { target: unknown[]; html: string }[]
        }[]
      }>
    }
    __csp?: string[]
  }
}

/** Waits until the loading placeholders are gone and the page has been quiet for a moment. */
export async function settled(page: Page): Promise<void> {
  await page.waitForFunction(() => !document.querySelector('.skeleton'), { timeout: 30_000 })
  await sleep(250)
  await page.waitForFunction(() => !document.querySelector('.skeleton'), { timeout: 30_000 })
}

/** Signs in through the sign-in form. */
export async function signIn(page: Page, user = 'admin'): Promise<void> {
  await page.waitForSelector('input[autocomplete="username"]', { timeout: 60_000 })
  await page.type('input[autocomplete="username"]', user)
  await page.type('input[type="password"]', 'demo-password')
  await Promise.all([
    page.waitForFunction(() => !location.pathname.startsWith('/login'), { timeout: 30_000 }),
    page.click('button[type="submit"]'),
  ])
}

/** Navigates inside the application without a reload (the mock state lives in the page). */
export async function go(page: Page, to: string): Promise<void> {
  await page.evaluate((target) => window.__router?.push(target), to)
  await settled(page)
}

/** Clicks the first button (or tab or link) whose visible text is `text`. */
export async function clickText(
  page: Page,
  text: string,
  selector = 'button, [role="tab"], a',
): Promise<void> {
  const ok = await page.evaluate(
    (t, sel) => {
      const el = [...document.querySelectorAll<HTMLElement>(sel)].find(
        (e) => e.textContent?.trim() === t,
      )
      el?.click()
      return !!el
    },
    text,
    selector,
  )
  if (!ok) throw new Error(`No element with the text ${JSON.stringify(text)}`)
  await sleep(150)
}

export const ROUTES = [
  '/dashboard',
  '/servers',
  '/servers/10.0.0.11',
  '/servers/10.0.0.13',
  '/databases',
  '/databases/shop',
  '/shards',
  '/replication',
  '/rebalance',
  '/security',
  '/ledger',
  '/workbench',
  '/tasks',
  '/alerts',
  '/logs',
  '/settings',
  '/this-server',
  '/this-server/logs',
]

export const SECURITY_TABS = ['Roles', 'Access', 'Network access', 'Audit log']

/** A page state that needs more than a visit: a dialog opened, a result shown, a special state reached. */
export interface PageState {
  name: string
  /** The route to open first (inside the application, or by address in the production build). */
  route: string
  /** Gets into the state. */
  open: (page: Page) => Promise<void>
  /** Gets out again (closes the dialog); the next state starts from its own route anyway. */
  close?: (page: Page) => Promise<void>
}

async function dialogOpen(page: Page): Promise<void> {
  await page.waitForSelector('[role="dialog"]', { timeout: 20_000 })
  await sleep(250)
}
const cancel = async (page: Page): Promise<void> => {
  await clickText(page, 'Cancel')
  await sleep(150)
}

/**
 * The 0.4.0 and 0.5.0 states that the page loop does not reach: the certificate dialogs of a server, the
 * inclusion proof with the digests compared against it, the add-server dialog with the fingerprint, the
 * leave-cluster and instance dialogs, and the state of a stopped system database. Used by the axe, layout
 * and content-security-policy runs (W2-16).
 */
export const STATES: PageState[] = [
  {
    name: 'renew certificates dialog',
    route: '/servers/10.0.0.13',
    open: async (page) => {
      await clickText(page, 'Renew now')
      await dialogOpen(page)
    },
    close: cancel,
  },
  {
    name: 'revoke certificates dialog',
    route: '/servers/10.0.0.13',
    open: async (page) => {
      await clickText(page, 'Revoke certificates')
      await dialogOpen(page)
    },
    close: cancel,
  },
  {
    name: 'remove server dialog',
    route: '/servers/10.0.0.13',
    open: async (page) => {
      await clickText(page, 'Remove')
      await dialogOpen(page)
    },
    close: cancel,
  },
  {
    name: 'add server dialog with the fingerprint field',
    route: '/servers',
    open: async (page) => {
      await clickText(page, 'Add server')
      await dialogOpen(page)
    },
    close: cancel,
  },
  {
    name: 'add server dialog with the explicit unpinned choice ticked',
    route: '/servers',
    open: async (page) => {
      await clickText(page, 'Add server')
      await dialogOpen(page)
      await page.click('[role="dialog"] [data-testid="allow-unpinned"]')
      await page.waitForSelector('[role="dialog"] [data-testid="unpinned-warning"]')
      await sleep(150)
    },
    close: cancel,
  },
  {
    name: 'workbench with a truncated statement that was applied',
    route: '/workbench',
    open: async (page) => {
      await page.click('#w-sql', { count: 3 })
      await page.keyboard.press('Backspace')
      await page.type('#w-sql', 'update orders set seen = true returning bulk')
      await clickText(page, 'Run')
      await page.waitForFunction(() => document.body.textContent?.includes('WAS applied'), {
        timeout: 20_000,
      })
      await sleep(250)
    },
  },
  {
    name: 'ledger inclusion proof with the digests compared',
    route: '/ledger',
    open: async (page) => {
      await clickText(page, 'Look up proof')
      await page.waitForSelector('[data-testid="proof"]', { timeout: 20_000 })
      await page.waitForSelector('[data-testid="digest-proof-summary"]', { timeout: 20_000 })
      await sleep(250)
    },
  },
  {
    name: 'restart instance dialog',
    route: '/this-server',
    open: async (page) => {
      await clickText(page, 'Restart')
      await dialogOpen(page)
    },
    close: cancel,
  },
  {
    name: 'leave cluster dialog',
    route: '/this-server',
    open: async (page) => {
      await clickText(page, 'Leave cluster')
      await dialogOpen(page)
    },
    close: cancel,
  },
  {
    name: 'system database stopped',
    route: '/this-server',
    open: async (page) => {
      // The Stop button of instance #11, the one that hosts the system database.
      const ok = await page.evaluate(() => {
        const row = [...document.querySelectorAll('tr')].find((r) => r.textContent?.includes('#11'))
        const button = [...(row?.querySelectorAll('button') ?? [])].find(
          (b) => b.textContent?.trim() === 'Stop',
        )
        button?.click()
        return !!button
      })
      if (!ok) throw new Error('No Stop button for instance #11')
      await dialogOpen(page)
      await page.type('[role="dialog"] input[autocomplete="off"]', 'stop')
      await clickText(page, 'Stop instance')
      await page.waitForFunction(
        () => document.body.textContent?.includes('The system database is stopped'),
        { timeout: 30_000 },
      )
      await sleep(300)
    },
    close: async (page) => {
      await clickText(page, 'Start instance #11')
      await page.waitForFunction(
        () => !document.body.textContent?.includes('The system database is stopped'),
        { timeout: 60_000 },
      )
      await sleep(300)
    },
  },
]
