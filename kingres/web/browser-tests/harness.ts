// SPDX-License-Identifier: AGPL-3.0-only
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
  '/databases/shop/tables/public/orders',
  '/shards',
  '/replication',
  '/rebalance',
  '/security',
  '/ledger',
  '/ledger?tab=digests',
  '/ledger?tab=anchoring',
  '/ledger?tab=clients',
  '/ledger?tab=signing',
  '/ledger?tab=publication',
  '/workbench',
  '/tasks',
  '/alerts',
  '/logs',
  '/settings',
  '/configuration',
  '/configuration?tab=changes',
  '/configuration?tab=time',
  '/this-server',
  '/this-server/logs',
]

/**
 * The URL switches of the mock for the states of contract 0.7.0: a lost lease on group 1, a server that is dead, a
 * standby that stopped streaming for longer than the grace period, and the client entry point (and its standby one) on.
 */
export const HA_SWITCHES =
  'lease=lost:1&silent=10.0.0.13:dead&standby=stalled:3@10.0.0.11:past&client=standby'

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
  /** The state needs the mock's switches and its `__kingresMock`, which the production build of the content security policy run lacks. */
  needsMock?: boolean
}

/** A value for the configuration dialogs that differs from the one before it, so that Apply is never a no-op. */
let configCounter = 0
const nextConfigValue = (): string => `${30 + configCounter++}MB`

/** Types into the value field of the configuration dialog the way a person does (the input event). */
async function setConfigValue(page: Page, text: string): Promise<void> {
  await page.$eval(
    '#cfg-value',
    (el, v) => {
      ;(el as HTMLInputElement).value = v
      el.dispatchEvent(new Event('input', { bubbles: true }))
    },
    text,
  )
}

/** Waits for the element (the list loads after the page is quiet) and clicks it. */
async function clickWhenThere(page: Page, selector: string): Promise<void> {
  await page.waitForSelector(selector, { timeout: 20_000 })
  await page.click(selector)
}

async function dialogOpen(page: Page): Promise<void> {
  await page.waitForSelector('[role="dialog"]', { timeout: 20_000 })
  await sleep(250)
}
const cancel = async (page: Page): Promise<void> => {
  await clickText(page, 'Cancel')
  await sleep(150)
}

/** Sets members of the mock's configuration in the page (see src/mocks/browser.ts); no code is built from text, the pages run under a CSP. */
const setMock = (page: Page, settings: Record<string, unknown>) =>
  page.evaluate((values) => {
    const mock = (window as unknown as { __kingresMock: { config: Record<string, unknown> } })
      .__kingresMock
    Object.assign(mock.config, values)
  }, settings)

/** Applies a value of `?anchors=` while the page is open (src/mocks/browser.ts), and makes the pages read again. */
const anchorsSwitch = async (page: Page, value: string) => {
  await page.evaluate((v) => {
    ;(
      window as unknown as { __kingresMock: { anchorsSwitch: (text: string) => void } }
    ).__kingresMock.anchorsSwitch(v)
    void (
      window as unknown as { __queryClient: { invalidateQueries: () => unknown } }
    ).__queryClient.invalidateQueries()
  }, value)
  await sleep(300)
}

/** Applies a value of a URL switch of the mock while the page is open, and makes the pages read again. */
const urlSwitch = async (page: Page, name: string, value: string) => {
  await page.evaluate(
    (n, v) => {
      ;(
        window as unknown as { __kingresMock: { urlSwitch: (n: string, t: string) => void } }
      ).__kingresMock.urlSwitch(n, v)
      void (
        window as unknown as { __queryClient: { invalidateQueries: () => unknown } }
      ).__queryClient.invalidateQueries()
    },
    name,
    value,
  )
  await sleep(300)
}

/** Opens the Client keys tab with three keys (once). */
async function clientKeys(page: Page): Promise<void> {
  await urlSwitch(page, 'clientkeys', 'keys')
  await page.waitForSelector('[data-testid="client-keys"] tbody tr', { timeout: 30_000 })
  await page.waitForSelector('[data-testid="client-tables"] tbody tr', { timeout: 30_000 })
  await sleep(250)
}

/** Clicks the button with this accessible name (an `aria-label`) or visible text. */
async function clickLabeled(page: Page, label: string): Promise<void> {
  const ok = await page.evaluate((l) => {
    const b = [...document.querySelectorAll<HTMLElement>('button')].find(
      (e) => (e.getAttribute('aria-label') ?? e.textContent?.trim()) === l,
    )
    b?.click()
    return !!b
  }, label)
  if (!ok) throw new Error(`No button named ${JSON.stringify(label)}`)
  await sleep(200)
}

/** Opens the Anchoring tab with the three kinds of target and a record in every state. */
async function anchoringAll(page: Page): Promise<void> {
  await anchorsSwitch(page, 'all')
  await page.waitForSelector('[data-testid="anchor-records"] tbody tr', { timeout: 30_000 })
  await sleep(250)
}

/** Applies a value of `?failover=` while the page is open (src/mocks/browser.ts). */
const mockSwitch = async (page: Page, value: string) => {
  await page.evaluate((v) => {
    ;(
      window as unknown as { __kingresMock: { failoverSwitch: (text: string) => void } }
    ).__kingresMock.failoverSwitch(v)
    // The pages keep what they read for a few seconds: ask again.
    void (
      window as unknown as { __queryClient: { invalidateQueries: () => unknown } }
    ).__queryClient.invalidateQueries()
  }, value)
  await sleep(300)
}

/** Puts two groups on the standby of their home server, as after a failover, and makes the pages read again. */
async function awayGroups(page: Page): Promise<void> {
  await page.evaluate(() => {
    const mock = (
      window as unknown as {
        __kingresMock: {
          state: () => {
            groups: { id: number; primaryServerId: string; standbyServerIds: string[] }[]
          }
        }
      }
    ).__kingresMock
    for (const g of mock.state().groups)
      if ((g.id === 1 || g.id === 2) && g.standbyServerIds[0])
        g.primaryServerId = g.standbyServerIds[0]
    void (
      window as unknown as { __queryClient: { invalidateQueries: () => unknown } }
    ).__queryClient.invalidateQueries()
  })
  await sleep(500)
}

/** Opens the failover dialog of a server on the Servers page. */
async function openFailoverDialog(page: Page, server: string): Promise<void> {
  const ok = await page.evaluate((id) => {
    const row = document.querySelector(`[data-testid="server-row-${id}"]`)
    const button = [...(row?.querySelectorAll('button') ?? [])].find((b) =>
      /^fail ?over$/i.test(b.textContent?.trim() ?? ''),
    )
    button?.click()
    return !!button
  }, server)
  if (!ok) throw new Error(`No failover button for ${server}`)
  await dialogOpen(page)
}

async function chooseHard(page: Page, mode = 'Hard:'): Promise<void> {
  await page.evaluate((start) => {
    const label = [...document.querySelectorAll('[role="dialog"] label')].find((l) =>
      l.textContent?.trim().startsWith(start),
    )
    ;(label?.querySelector('input') as HTMLInputElement | null)?.click()
  }, mode)
  await sleep(150)
}

/** From the choice to the plan, and (when asked) through the decision, the typed address and the start; the dialog is closed after. */
async function startFailover(page: Page, server: string, accept = false): Promise<void> {
  await openFailoverDialog(page, server)
  await chooseHard(page)
  await clickText(page, 'Show the plan', '[role="dialog"] button')
  await page.waitForSelector('[role="dialog"] [data-testid="failover-plan"]', { timeout: 20_000 })
  if (accept) await page.click('[role="dialog"] [data-testid="accept-loss"] input')
  await page.type('[role="dialog"] input.mono', `${server}:8091`)
  await clickText(page, 'Hard failover', '[role="dialog"] button')
  await page.waitForSelector('[role="dialog"] [role="progressbar"]', { timeout: 20_000 })
}

const closeDialogs = async (page: Page): Promise<void> => {
  await page.evaluate(() => {
    for (const b of document.querySelectorAll<HTMLElement>('[role="dialog"] .dlg-foot button'))
      if (/^(close|hide \(keeps running\)|cancel)$/i.test(b.textContent?.trim() ?? '')) {
        b.click()
        break
      }
  })
  await sleep(200)
}

/** Leaves nothing of a run behind: the cluster as it was, and no notice of a finished task (a notice has a Dismiss button). */
const leaveFailover = async (page: Page): Promise<void> => {
  await freshFailover(page)
  await setMock(page, { taskDurationMs: 4000 })
  await page.evaluate(() => {
    for (const b of document.querySelectorAll<HTMLElement>('button'))
      if (b.textContent?.trim() === 'Dismiss') b.click()
  })
  await sleep(200)
}

/** Puts the cluster back as it was, and the switches of the failover off, so that each state starts from the same place. */
const freshFailover = async (page: Page): Promise<void> => {
  await page.evaluate(() =>
    (
      window as unknown as { __kingresMock: { resetFailover: () => void } }
    ).__kingresMock.resetFailover(),
  )
  await setMock(page, {
    failoverDataLoss: false,
    failoverNeedsDecision: false,
    failoverWarnings: false,
    failoverGraceful: false,
    failoverNoPlan: false,
    failoverOld: null,
    failoverFailStep: null,
    failoverBlock: null,
    taskDurationMs: 800,
  })
}

/** Clicks a button of the table row whose first cell says `name` (the tables list of a database). */
async function clickRowButton(page: Page, name: string, label: string): Promise<void> {
  await page.waitForSelector('[data-testid="export-csv"]', { timeout: 30_000 })
  const ok = await page.evaluate(
    (n, l) => {
      const row = [...document.querySelectorAll('tbody tr')].find(
        (r) => r.querySelector('th')?.textContent?.trim() === n,
      )
      const button = [...(row?.querySelectorAll('button') ?? [])].find(
        (b) => b.textContent?.trim() === l,
      )
      button?.click()
      return !!button
    },
    name,
    label,
  )
  if (!ok) throw new Error(`No button ${label} in the row ${name}`)
  await dialogOpen(page)
}

/** Chooses a CSV file in the import dialog the way a person does: a file in the input, and its change event. */
async function chooseCsv(page: Page, name: string, text: string): Promise<void> {
  await page.evaluate(
    (n, t) => {
      const input = document.querySelector<HTMLInputElement>('#import-file')
      if (!input) throw new Error('No file input')
      const transfer = new DataTransfer()
      transfer.items.add(new File([t], n, { type: 'text/csv', lastModified: 1 }))
      input.files = transfer.files
      input.dispatchEvent(new Event('change', { bubbles: true }))
    },
    name,
    text,
  )
  await page.waitForSelector('[data-testid="csv-preview"]', { timeout: 20_000 })
  await sleep(250)
}

const NOTES_CSV = 'note_id,body,created_at\n1,hello,2026-01-01\n2,world,2026-01-02\n'
const fastTasks = (page: Page) => setMock(page, { taskDurationMs: 400 })
const slowTasks = (page: Page) => setMock(page, { taskDurationMs: 4000 })

/** Closes the open dialog with its close button (a step of the import dialog has no Cancel). */
const closeX = async (page: Page): Promise<void> => {
  await page.click('[role="dialog"] [data-dialog-close]')
  await sleep(200)
}

/** Goes through the options step of the import dialog and starts it. */
async function startImport(page: Page): Promise<void> {
  await page.click('[data-testid="to-options"]')
  await page.waitForSelector('[data-testid="all-or-nothing"]', { timeout: 20_000 })
  await sleep(200)
  // A file whose earlier import had no known outcome asks for the tick before it is imported again.
  const ack = await page.$('[data-testid="ack-repeat"]')
  if (ack) await ack.click()
  await page.click('[data-testid="start-import"]')
}

/** Puts the tables of increment 9.7 back as they were: the switches off, and the partitioned tables the states added gone. */
const freshTables = async (page: Page): Promise<void> => {
  await setMock(page, {
    tableFail: null,
    tableSlow: false,
    tableBusy: false,
    tableForeignKeys: [],
    taskDurationMs: 800,
  })
  await page.evaluate(() => {
    const mock = (
      window as unknown as {
        __kingresMock: {
          state: () => {
            databases: {
              info: { name: string }
              tables: { name: string; distribution: { colocationId: number | null } | null }[]
              colocations: { colocationId: number }[]
              buckets: Map<number, unknown>
            }[]
            partitions?: Record<string, unknown>
          }
        }
      }
    ).__kingresMock
    const s = mock.state()
    const shop = s.databases.find((d) => d.info.name === 'shop')
    if (!shop) return
    const gone = shop.tables.filter((t) => t.name.startsWith('measurements'))
    const groups = new Set(gone.map((t) => t.distribution?.colocationId))
    shop.tables = shop.tables.filter((t) => !t.name.startsWith('measurements'))
    shop.colocations = shop.colocations.filter((g) => !groups.has(g.colocationId))
    for (const id of groups) if (id != null) shop.buckets.delete(id)
    delete s.partitions
  })
}

/** Opens the page of a table of the database `shop` through the list of its tables. */
async function openTablePage(page: Page, table: string): Promise<void> {
  await page.evaluate((name) => {
    ;(
      window as unknown as { __queryClient: { invalidateQueries: () => unknown } }
    ).__queryClient.invalidateQueries()
    void name
  }, table)
  await sleep(500)
  await page.waitForFunction(
    (name) =>
      [...document.querySelectorAll('a')].some((a) => a.textContent?.trim() === `public.${name}`),
    { timeout: 20_000 },
    table,
  )
  await clickText(page, `public.${table}`, 'a')
  await page.waitForSelector('[data-testid="table-facts"]', { timeout: 20_000 })
}

/**
 * The 0.4.0 and 0.5.0 states that the page loop does not reach: the certificate dialogs of a server, the
 * inclusion proof with the digests compared against it, the add-server dialog with the fingerprint, the
 * leave-cluster and instance dialogs, and the state of a stopped system database. Used by the axe, layout
 * and content-security-policy runs (W2-16).
 */
export const STATES: PageState[] = [
  {
    // 0.9.0: the plan of a rebalance listed with its servers, and a start that the server refuses
    // (the mock refuses an empty plan with its own notice).
    name: 'rebalance: plan preview and a refused start',
    route: '/rebalance',
    open: async (page) => {
      await clickText(page, 'Preview plan')
      await page.waitForSelector('[data-testid="plan"]', { timeout: 20_000 })
      await clickText(page, 'Start rebalance')
      await sleep(600)
    },
  },
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
    route: '/ledger?tab=digests',
    open: async (page) => {
      await clickText(page, 'Look up proof')
      await page.waitForSelector('[data-testid="proof"]', { timeout: 20_000 })
      await page.waitForSelector('[data-testid="digest-proof-summary"]', { timeout: 20_000 })
      await sleep(250)
    },
  },
  {
    // 0.11.0 (Console A): an aggregate checkpoint with its members, the statement and the signed note.
    name: 'ledger aggregate dialog',
    route: '/ledger?tab=digests',
    open: async (page) => {
      await clickText(page, 'Open aggregate')
      await dialogOpen(page)
      await page.waitForSelector('[role="dialog"] [data-testid="aggregate"]', { timeout: 20_000 })
      await sleep(250)
    },
    close: async (page) => {
      await clickText(page, 'Close', '[role="dialog"] button')
      await sleep(150)
    },
  },
  {
    name: 'ledger rotate-the-key dialog',
    route: '/ledger?tab=signing',
    open: async (page) => {
      await clickText(page, 'Rotate the key')
      await dialogOpen(page)
    },
    close: cancel,
  },
  {
    // The only key in force cannot be revoked, so the state rotates first (once: two keys stay active afterwards).
    name: 'ledger revoke-the-key dialog',
    route: '/ledger?tab=signing',
    open: async (page) => {
      await page.waitForSelector('[data-testid="key-active"]', { timeout: 20_000 })
      const active = () =>
        page.evaluate(() => document.querySelectorAll('[data-testid="key-active"]').length)
      if ((await active()) < 2) {
        await clickText(page, 'Rotate the key')
        await dialogOpen(page)
        await clickText(page, 'Rotate the key', '[role="dialog"] button')
        await page.waitForFunction(
          () => document.querySelector('[role="dialog"]')?.textContent?.includes('Finished'),
          { timeout: 30_000 },
        )
        await clickText(page, 'Close', '[role="dialog"] button')
        await page.waitForFunction(
          () => document.querySelectorAll('[data-testid="key-active"]').length >= 2,
          { timeout: 30_000 },
        )
      }
      await page.evaluate(() => {
        const row = document.querySelector('[data-testid="key-active"]')
        const button = [...(row?.querySelectorAll('button') ?? [])].find((b) =>
          /^revoke/i.test(b.textContent?.trim() ?? ''),
        )
        button?.click()
      })
      await dialogOpen(page)
    },
    close: cancel,
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
    name: 'replica count dialog (0.7.0)',
    route: '/replication',
    open: async (page) => {
      await clickText(page, 'Change replica count')
      await dialogOpen(page)
    },
    close: cancel,
  },
  {
    name: 'replica count dialog following the standby task (0.7.0)',
    route: '/replication',
    open: async (page) => {
      // The state can run several times in one session: it toggles between one and two standbys, so there is always work.
      const current = await page.evaluate(
        () => document.querySelector('[data-testid="replica-count"]')?.textContent?.trim() ?? '1',
      )
      await clickText(page, 'Change replica count')
      await dialogOpen(page)
      await page.click('[role="dialog"] input[type="number"]', { count: 3 })
      await page.keyboard.type(current === '2' ? '1' : '2')
      await clickText(page, 'Change replica count', '[role="dialog"] button')
      await page.waitForSelector('[data-testid="replica-done"]', { timeout: 40_000 })
      await sleep(250)
    },
    close: async (page) => {
      await clickText(page, 'Close', '[role="dialog"] .dlg-foot button')
      await sleep(150)
    },
  },
  {
    name: 'workbench with a statement running (Cancel statement, Stop waiting) (0.7.0)',
    route: '/workbench',
    open: async (page) => {
      await page.$eval('#w-sql', (el) => {
        ;(el as HTMLTextAreaElement).value = 'select pg_sleep(30)'
        el.dispatchEvent(new Event('input', { bubbles: true }))
      })
      await clickText(page, 'Run')
      await page.waitForFunction(
        () =>
          [...document.querySelectorAll('button')].some(
            (b) => b.textContent?.trim() === 'Cancel statement',
          ),
        { timeout: 20_000 },
      )
      await sleep(250)
    },
    close: async (page) => {
      await clickText(page, 'Cancel statement')
      await page.waitForSelector('[data-testid="query-error-hint"]', { timeout: 20_000 })
      await sleep(150)
    },
  },
  {
    name: 'edit parameter dialog with the preview (0.8.0)',
    route: '/configuration?q=work_mem',
    open: async (page) => {
      await clickWhenThere(page, 'button[aria-label="Edit work_mem"]')
      await dialogOpen(page)
      await setConfigValue(page, nextConfigValue())
      await page.waitForSelector('[data-testid="preview-sentence"]', { timeout: 20_000 })
      await sleep(150)
    },
    close: cancel,
  },
  {
    name: 'edit parameter dialog: a restart is needed (0.8.0)',
    route: '/configuration?q=max_connections',
    open: async (page) => {
      await clickWhenThere(page, 'button[aria-label="Edit max_connections"]')
      await dialogOpen(page)
      await setConfigValue(page, String(200 + configCounter))
      await page.waitForSelector('[data-testid="restart-note"]', { timeout: 20_000 })
      await sleep(150)
    },
    close: cancel,
  },
  {
    name: 'edit parameter dialog: the state of every server after Apply (0.8.0)',
    route: '/configuration?q=work_mem',
    open: async (page) => {
      await clickWhenThere(page, 'button[aria-label="Edit work_mem"]')
      await dialogOpen(page)
      await setConfigValue(page, nextConfigValue())
      await page.waitForSelector('[data-testid="preview-sentence"]', { timeout: 20_000 })
      await clickText(page, 'Apply', '[role="dialog"] button')
      await page.waitForFunction(
        () =>
          document
            .querySelector('[role="dialog"]')
            ?.textContent?.includes('Applied on every server'),
        { timeout: 40_000 },
      )
      await sleep(250)
    },
    close: async (page) => {
      await clickText(page, 'Close', '[role="dialog"] .dlg-foot button')
      await sleep(150)
    },
  },
  {
    name: 'revert dialog (0.8.0)',
    route: '/configuration?tab=changes',
    open: async (page) => {
      await clickWhenThere(page, 'button[aria-label^="Revert change"]')
      await dialogOpen(page)
      await page.waitForSelector('[data-testid="revert-summary"]', { timeout: 20_000 })
      await sleep(900)
    },
    close: cancel,
  },
  {
    name: 'restart pending instances dialog (0.8.0)',
    route: '/configuration',
    open: async (page) => {
      await clickText(page, 'Restart pending instances')
      await dialogOpen(page)
      await page.waitForSelector(
        '[data-testid="waiting-servers"], [data-testid="nothing-waiting"]',
        {
          timeout: 20_000,
        },
      )
    },
    close: cancel,
  },
  {
    name: 'cluster time zone dialog (0.8.0)',
    route: '/configuration?tab=time',
    open: async (page) => {
      await clickText(page, 'Apply')
      await dialogOpen(page)
      await sleep(900)
    },
    close: cancel,
  },
  {
    name: 'database time zone dialog (0.8.0)',
    route: '/configuration?tab=time',
    open: async (page) => {
      await clickWhenThere(page, '[data-testid="dbzone-shop"] button')
      await dialogOpen(page)
      await sleep(900)
    },
    close: cancel,
  },
  {
    // 0.10.0: the failover dialog (design 8.2), step 1: the mode, the groups (the server that is down), the standby.
    name: 'failover dialog: the choice (0.10.0)',
    needsMock: true,
    route: '/servers',
    open: async (page) => {
      await freshFailover(page)
      await openFailoverDialog(page, '10.0.0.11')
      await page.waitForSelector('[role="dialog"] [data-testid="failover-groups"]', {
        timeout: 20_000,
      })
      await sleep(250)
    },
    close: cancel,
  },
  {
    name: 'failover dialog: a refusal in words (0.10.0)',
    needsMock: true,
    route: '/servers',
    open: async (page) => {
      await freshFailover(page)
      await openFailoverDialog(page, '10.0.0.12')
      // The server list may still show the status it had before the reset, so the mode is chosen, not left to its default.
      await chooseHard(page, 'Graceful:')
      await clickText(page, 'Show the plan', '[role="dialog"] button')
      await page.waitForSelector('[role="dialog"] [data-testid="failover-refusal"]', {
        timeout: 20_000,
      })
      await sleep(250)
    },
    close: cancel,
  },
  {
    name: 'failover dialog: the plan, with a decision about lost data (0.10.0)',
    needsMock: true,
    route: '/servers',
    open: async (page) => {
      await freshFailover(page)
      await setMock(page, { failoverDataLoss: true })
      await openFailoverDialog(page, '10.0.0.12')
      await chooseHard(page)
      await clickText(page, 'Show the plan', '[role="dialog"] button')
      await page.waitForSelector('[role="dialog"] [data-testid="accept-loss"]', { timeout: 20_000 })
      await sleep(250)
    },
    close: async (page) => {
      await cancel(page)
      await setMock(page, { failoverDataLoss: false })
    },
  },
  {
    name: 'failover progress: waiting for a decision (0.10.0)',
    needsMock: true,
    route: '/servers',
    open: async (page) => {
      await freshFailover(page)
      await setMock(page, { failoverNeedsDecision: true })
      await startFailover(page, '10.0.0.12')
      await closeDialogs(page)
      await page.waitForSelector('[data-testid="waiting-for-you"]', { timeout: 30_000 })
      await page.waitForSelector('[data-testid="decision-loss-2"]', { timeout: 30_000 })
      await sleep(400)
    },
    close: async (page) => {
      await setMock(page, { failoverNeedsDecision: false })
      await page.evaluate(() =>
        (
          document.querySelector(
            '[data-testid="waiting-for-you"] [data-testid="cancel-failover"]',
          ) as HTMLElement | null
        )?.click(),
      )
      await page.waitForFunction(
        () => !document.querySelector('[data-testid="failover-progress"]'),
        {
          timeout: 30_000,
        },
      )
      await sleep(150)
      await leaveFailover(page)
    },
  },
  {
    name: 'failover report with the numbers, the incidents and the old primary (0.10.0)',
    needsMock: true,
    route: '/servers',
    open: async (page) => {
      await freshFailover(page)
      await setMock(page, { failoverDataLoss: true, failoverOld: 'rebuilding' })
      await startFailover(page, '10.0.0.12', true)
      await page.waitForFunction(
        () => document.querySelector('[role="dialog"]')?.textContent?.includes('Finished.'),
        { timeout: 30_000 },
      )
      await closeDialogs(page)
      await go(page, '/replication')
      await clickWhenThere(page, '[data-testid="last-failover-report"]')
      await page.waitForSelector('[role="dialog"] [data-testid="report-old-primary"]', {
        timeout: 20_000,
      })
      await sleep(300)
    },
    close: async (page) => {
      await page.click('[role="dialog"] [data-dialog-close]')
      await sleep(150)
      await leaveFailover(page)
    },
  },
  {
    // 0.10.0, console part 2: the counter of automatic failovers at its limit, and the reset with its confirmation.
    name: 'settings: automatic failover at its limit (0.10.0)',
    needsMock: true,
    route: '/settings',
    open: async (page) => {
      await freshFailover(page)
      await mockSwitch(page, 'limit')
      await go(page, '/dashboard')
      await go(page, '/settings')
      await page.waitForSelector('[data-testid="af-limit"]', { timeout: 20_000 })
      await sleep(250)
    },
    close: freshFailover,
  },
  {
    name: 'settings: reset the counter dialog (0.10.0)',
    needsMock: true,
    route: '/settings',
    open: async (page) => {
      await freshFailover(page)
      await mockSwitch(page, 'limit')
      await go(page, '/dashboard')
      await go(page, '/settings')
      await page.waitForSelector('[data-testid="af-limit"]', { timeout: 20_000 })
      await clickText(page, 'Reset counter')
      await dialogOpen(page)
    },
    close: async (page) => {
      await cancel(page)
      await freshFailover(page)
    },
  },
  {
    name: 'banner: a failover that goes on (0.10.0)',
    needsMock: true,
    route: '/dashboard',
    open: async (page) => {
      await freshFailover(page)
      await setMock(page, { taskDurationMs: 60_000 })
      await mockSwitch(page, 'auto:10.0.0.12')
      await page.waitForSelector('[data-testid="banner-failover"]', { timeout: 30_000 })
      await sleep(300)
    },
    close: leaveFailover,
  },
  {
    name: 'banner: waiting for you (0.10.0)',
    needsMock: true,
    route: '/servers',
    open: async (page) => {
      await freshFailover(page)
      await setMock(page, { failoverNeedsDecision: true })
      await startFailover(page, '10.0.0.12')
      await closeDialogs(page)
      await page.waitForSelector('[data-testid="banner-failover"].tone-danger', { timeout: 30_000 })
      await sleep(300)
    },
    close: async (page) => {
      await setMock(page, { failoverNeedsDecision: false })
      await leaveFailover(page)
    },
  },
  {
    // 0.11.0, console part B: the Anchoring tab with the three kinds of target, a record in every state and (the page runs
    // with the hostile mock) targets and records with hostile names.
    name: 'anchoring tab: three kinds and every state of a record (0.11.0)',
    needsMock: true,
    route: '/ledger?tab=anchoring',
    open: anchoringAll,
  },
  {
    name: 'anchoring: add a WORM target (0.11.0)',
    needsMock: true,
    route: '/ledger?tab=anchoring',
    open: async (page) => {
      await anchoringAll(page)
      await clickText(page, 'Add a target')
      await dialogOpen(page)
      await page.waitForSelector('[data-testid="compliance-confirm"]', { timeout: 10_000 })
    },
    close: cancel,
  },
  {
    name: 'anchoring: add an OpenTimestamps target (0.11.0)',
    needsMock: true,
    route: '/ledger?tab=anchoring',
    open: async (page) => {
      await anchoringAll(page)
      await clickText(page, 'Add a target')
      await dialogOpen(page)
      await page.click('[data-testid="kind-ots"]')
      await clickText(page, 'Use the public calendars', '[role="dialog"] button')
      await sleep(250)
    },
    close: cancel,
  },
  {
    name: 'anchoring: a test that failed (0.11.0)',
    needsMock: true,
    route: '/ledger?tab=anchoring',
    open: async (page) => {
      await anchoringAll(page)
      await setMock(page, { anchorTest: 'fail' })
      await clickLabeled(page, 'Test archive-worm')
      await dialogOpen(page)
      await clickText(page, 'Run the test again', '[role="dialog"] button')
      await page.waitForSelector('[data-testid="check-failed"]', { timeout: 20_000 })
      await sleep(250)
    },
    close: async (page) => {
      await setMock(page, { anchorTest: null })
      await clickText(page, 'Close', '[role="dialog"] button')
    },
  },
  {
    // 0.12.0: the alert of a system database that is older than what was published (its own words and a link).
    name: 'alert: the system database is older than what was published (0.12.0)',
    needsMock: true,
    route: '/alerts',
    open: async (page) => {
      await page.evaluate(() => {
        ;(
          window as unknown as { __kingresMock: { signingSwitch: (t: string) => void } }
        ).__kingresMock.signingSwitch('restored')
        void (
          window as unknown as { __queryClient: { invalidateQueries: () => unknown } }
        ).__queryClient.invalidateQueries()
      })
      await page.waitForSelector('[data-testid="alert-published-state"]', { timeout: 30_000 })
      await sleep(250)
    },
  },
  {
    name: 'anchoring: delete a target (0.11.0)',
    needsMock: true,
    route: '/ledger?tab=anchoring',
    open: async (page) => {
      await anchoringAll(page)
      await clickLabeled(page, 'Delete public-tsa')
      await dialogOpen(page)
    },
    close: cancel,
  },
  {
    // 0.12.0: a run that ended well and whose check of the cluster reported something: "Completed, with remarks", with the report.
    name: 'failover report: completed with remarks (0.12.0)',
    needsMock: true,
    route: '/servers',
    open: async (page) => {
      await freshFailover(page)
      await setMock(page, { failoverWarnings: true })
      await startFailover(page, '10.0.0.12')
      await page.waitForFunction(
        () => document.querySelector('[role="dialog"]')?.textContent?.includes('Finished.'),
        { timeout: 30_000 },
      )
      await closeDialogs(page)
      await go(page, '/replication')
      await clickWhenThere(page, '[data-testid="last-failover-report"]')
      await page.waitForFunction(
        () =>
          document
            .querySelector('[role="dialog"]')
            ?.textContent?.includes('Completed, with remarks'),
        { timeout: 20_000 },
      )
      await sleep(300)
    },
    close: async (page) => {
      await page.click('[role="dialog"] [data-dialog-close]')
      await sleep(150)
      await leaveFailover(page)
    },
  },
  {
    // 0.12.0, failover console part 3: the planned switchover of one group (the server version that builds it, and one that does not).
    name: 'switchover of a group: the plan (0.12.0)',
    needsMock: true,
    route: '/replication',
    open: async (page) => {
      await freshFailover(page)
      await mockSwitch(page, 'graceful')
      await clickLabeled(page, 'Switch over Shard group 2')
      await dialogOpen(page)
      await clickText(page, 'Show the plan', '[role="dialog"] button')
      await page.waitForSelector('[role="dialog"] [data-testid="failover-plan"]', {
        timeout: 20_000,
      })
      await sleep(250)
    },
    close: async (page) => {
      await cancel(page)
      await freshFailover(page)
    },
  },
  {
    name: 'switchover of a group: not available on this server version (0.12.0)',
    needsMock: true,
    route: '/replication',
    open: async (page) => {
      await freshFailover(page)
      await clickLabeled(page, 'Switch over Shard group 2')
      await dialogOpen(page)
      await clickText(page, 'Show the plan', '[role="dialog"] button')
      await page.waitForSelector('[role="dialog"] [data-testid="failover-refusal"]', {
        timeout: 20_000,
      })
      await sleep(250)
    },
    close: cancel,
  },
  {
    name: 'restore preferred primaries: the groups and the plan (0.12.0)',
    needsMock: true,
    route: '/replication',
    open: async (page) => {
      await freshFailover(page)
      await awayGroups(page)
      await clickText(page, 'Restore preferred primaries')
      await dialogOpen(page)
      await page.waitForSelector('[data-testid="restore-groups"]', { timeout: 20_000 })
      await sleep(200)
      await clickText(page, 'Show the plan', '[role="dialog"] button')
      await page.waitForSelector('[role="dialog"] [data-testid="failover-plan"]', {
        timeout: 20_000,
      })
      await sleep(250)
    },
    close: async (page) => {
      await cancel(page)
      await freshFailover(page)
    },
  },
  {
    // 0.14.0, increment 9.7: a table the user partitioned by range, with its partitions and the buttons that change them.
    name: 'table page: the partitions of a table (0.14.0)',
    needsMock: true,
    route: '/databases/shop',
    open: async (page) => {
      await freshTables(page)
      await page.evaluate(() =>
        (
          window as unknown as {
            __kingresMock: { urlSwitch: (name: string, text: string) => void }
          }
        ).__kingresMock.urlSwitch('tables', 'partitions'),
      )
      await openTablePage(page, 'measurements')
      await page.waitForSelector('[data-testid="partition-list"]', { timeout: 20_000 })
      await sleep(250)
    },
    close: freshTables,
  },
  {
    name: 'table dialog: a change that the table cannot take, with the lines that stop it (0.14.0)',
    needsMock: true,
    route: '/databases/shop/tables/public/customers',
    open: async (page) => {
      await freshTables(page)
      await clickWhenThere(page, '[data-testid="undistribute-table"]')
      await dialogOpen(page)
      await page.waitForSelector('[role="dialog"] [data-testid="table-blockers"]', {
        timeout: 20_000,
      })
      await sleep(250)
    },
    close: cancel,
  },
  {
    name: 'table dialog: distribute a table, with the preview of the change (0.14.0)',
    needsMock: true,
    route: '/databases/shop/tables/public/notes',
    open: async (page) => {
      await freshTables(page)
      await clickWhenThere(page, '[data-testid="distribute-table"]')
      await dialogOpen(page)
      await page.waitForSelector('[role="dialog"] [data-testid="table-change-eligible"]', {
        timeout: 20_000,
      })
      await sleep(250)
    },
    close: cancel,
  },
  {
    name: 'table dialog: a task that failed, in words (0.14.0)',
    needsMock: true,
    route: '/databases/shop/tables/public/notes',
    open: async (page) => {
      await freshTables(page)
      await setMock(page, { tableFail: 'space' })
      await clickWhenThere(page, '[data-testid="distribute-table"]')
      await dialogOpen(page)
      await page.waitForSelector('[role="dialog"] [data-testid="table-change-eligible"]', {
        timeout: 20_000,
      })
      await clickText(page, 'Distribute table', '[role="dialog"] button')
      await page.waitForFunction(
        () =>
          document.querySelector('[role="dialog"]')?.textContent?.includes('Not enough free space'),
        { timeout: 30_000 },
      )
      await sleep(300)
    },
    close: async (page) => {
      await clickText(page, 'Close', '[role="dialog"] button')
      await freshTables(page)
    },
  },
  {
    // 0.11.0, Console C: the Client keys tab (keys of three states, the tables with their policy) and its dialogs.
    name: 'client keys tab (0.11.0)',
    needsMock: true,
    route: '/ledger?tab=clients',
    open: clientKeys,
  },
  {
    name: 'client keys: register a key (0.11.0)',
    needsMock: true,
    route: '/ledger?tab=clients',
    open: async (page) => {
      await clientKeys(page)
      await clickText(page, 'Register a key')
      await dialogOpen(page)
      await clickText(page, 'Register the key', '[role="dialog"] button')
      await page.waitForSelector('[role="dialog"] .error', { timeout: 10_000 })
    },
    close: cancel,
  },
  {
    name: 'client keys: require signatures for a table (0.11.0)',
    needsMock: true,
    route: '/ledger?tab=clients',
    open: async (page) => {
      await clientKeys(page)
      await page.evaluate(() => {
        const b = [...document.querySelectorAll<HTMLElement>('button')].find((e) =>
          (e.getAttribute('aria-label') ?? '').startsWith('Require signatures for'),
        )
        b?.click()
      })
      await dialogOpen(page)
      await page.waitForSelector('[data-testid="policy-consequences"]', { timeout: 10_000 })
    },
    close: cancel,
  },
  {
    name: 'client keys: revoke a key (0.11.0)',
    needsMock: true,
    route: '/ledger?tab=clients',
    open: async (page) => {
      await clientKeys(page)
      await page.evaluate(() => {
        const b = [...document.querySelectorAll<HTMLElement>('button')].find((e) =>
          (e.getAttribute('aria-label') ?? '').startsWith('Revoke key'),
        )
        b?.click()
      })
      await dialogOpen(page)
    },
    close: cancel,
  },
  {
    // 0.11.0, Console D: the bundle dialog with a proof (and its warning), the acknowledgement of a diverged chain, the audit card.
    name: 'bundle dialog with a proof (0.11.0)',
    needsMock: true,
    route: '/ledger?tab=digests',
    open: async (page) => {
      await clickText(page, 'Create a bundle')
      await dialogOpen(page)
      await page.click('[data-testid="bundle-proof"]')
      await page.waitForSelector('[data-testid="proofs-warning"]', { timeout: 10_000 })
      await sleep(200)
    },
    close: cancel,
  },
  {
    name: 'acknowledge a diverged chain (0.11.0)',
    needsMock: true,
    route: '/ledger',
    open: async (page) => {
      await urlSwitch(page, 'publication', 'diverged')
      await page.waitForSelector('[data-testid="diverged-chains"]', { timeout: 30_000 })
      await clickLabeled(page, 'Acknowledge the divergence of shard group 2')
      await dialogOpen(page)
    },
    close: cancel,
  },
  {
    name: 'publication: audit and bundle (0.11.0)',
    needsMock: true,
    route: '/ledger?tab=publication',
    open: async (page) => {
      await page.waitForSelector('[data-testid="publication-audit"]', { timeout: 30_000 })
      await sleep(250)
    },
  },
  {
    name: 'settings: data transfer group (0.19.0)',
    route: '/settings',
    open: async (page) => {
      await page.waitForSelector('[data-testid="data-transfer-settings"]', { timeout: 30_000 })
      await sleep(250)
    },
  },
  {
    name: 'transfers panel with export files (0.19.0)',
    needsMock: true,
    route: '/databases',
    open: async (page) => {
      await urlSwitch(page, 'transfer', 'exports')
      await page.waitForSelector('[data-testid="transfers-panel"] tbody tr', { timeout: 30_000 })
      await sleep(250)
    },
  },
  {
    name: 'transfers panel while data transfer is off (0.19.0)',
    needsMock: true,
    route: '/databases',
    open: async (page) => {
      await urlSwitch(page, 'transfer', 'off')
      await page.waitForSelector('[data-testid="transfers-off"]', { timeout: 30_000 })
      await sleep(250)
    },
    close: async (page) => {
      await page.evaluate(() => {
        const mock = (
          window as unknown as {
            __kingresMock: { state: () => { settings: { dataTransfer?: { enabled: boolean } } } }
          }
        ).__kingresMock
        const dt = mock.state().settings.dataTransfer
        if (dt) dt.enabled = true
        void (
          window as unknown as { __queryClient: { invalidateQueries: () => unknown } }
        ).__queryClient.invalidateQueries()
      })
      await sleep(300)
    },
  },
  {
    name: 'export CSV dialog (0.19.0)',
    needsMock: true,
    route: '/databases/shop',
    open: async (page) => {
      await clickRowButton(page, 'public.orders', 'Export CSV')
      await page.waitForSelector('[data-testid="export-consistency-before"]', { timeout: 20_000 })
    },
    close: cancel,
  },
  {
    name: 'export CSV dialog, spreadsheet guard off and an acknowledgement (0.19.0)',
    needsMock: true,
    route: '/databases/shop',
    open: async (page) => {
      await clickRowButton(page, 'public.othercode', 'Export CSV')
      await page.click('[data-testid="spreadsheet-safe"]')
      await page.waitForSelector('[data-testid="spreadsheet-warning"]', { timeout: 20_000 })
      await clickText(page, 'Export table', '[role="dialog"] button')
      await page.waitForSelector('[data-testid="acknowledge-code"]', { timeout: 20_000 })
      await sleep(250)
    },
    close: cancel,
  },
  {
    name: 'export CSV result with the download (0.19.0)',
    needsMock: true,
    route: '/databases/shop',
    open: async (page) => {
      await fastTasks(page)
      await clickRowButton(page, 'public.skewme', 'Export CSV')
      await clickText(page, 'Export table', '[role="dialog"] button')
      await page.waitForSelector('[data-testid="download-start"]', { timeout: 30_000 })
      await sleep(250)
    },
    close: async (page) => {
      await closeDialogs(page)
      await slowTasks(page)
    },
  },
  {
    name: 'export CSV failed (0.19.0)',
    needsMock: true,
    route: '/databases/shop',
    open: async (page) => {
      await fastTasks(page)
      await clickRowButton(page, 'public.failme', 'Export CSV')
      await clickText(page, 'Export table', '[role="dialog"] button')
      await page.waitForSelector('[role="dialog"] [role="alert"]', { timeout: 30_000 })
      await sleep(250)
    },
    close: async (page) => {
      await closeDialogs(page)
      await slowTasks(page)
    },
  },
  {
    name: 'import CSV dialog, file chosen with the preview (0.19.0)',
    needsMock: true,
    route: '/databases/shop',
    open: async (page) => {
      await clickRowButton(page, 'public.notes', 'Import CSV')
      await chooseCsv(page, 'notes.csv', NOTES_CSV)
    },
    close: cancel,
  },
  {
    name: 'import CSV dialog, the options and what all or nothing means (0.19.0)',
    needsMock: true,
    route: '/databases/shop',
    open: async (page) => {
      await clickRowButton(page, 'public.notes', 'Import CSV')
      await chooseCsv(page, 'notes.csv', NOTES_CSV)
      await page.click('[data-testid="to-options"]')
      await page.waitForSelector('[data-testid="all-or-nothing"]', { timeout: 20_000 })
      await sleep(250)
    },
    close: closeX,
  },
  {
    name: 'import CSV dialog, the session ended in the middle of the upload (0.19.0)',
    needsMock: true,
    route: '/databases/shop',
    open: async (page) => {
      await urlSwitch(page, 'transfer', 'session')
      await clickRowButton(page, 'public.notes', 'Import CSV')
      await chooseCsv(page, 'notes.csv', NOTES_CSV)
      await startImport(page)
      await page.waitForSelector('[data-testid="upload-password"]', { timeout: 30_000 })
      await sleep(250)
    },
    close: async (page) => {
      await clickText(page, 'Cancel the upload', '[role="dialog"] button')
      await closeX(page)
    },
  },
  {
    name: 'import CSV dialog, the file is too large (0.19.0)',
    needsMock: true,
    route: '/databases/shop',
    open: async (page) => {
      await clickRowButton(page, 'public.notes', 'Import CSV')
      // A file of 3 GiB: the size is what the browser reports; only its first bytes are read.
      await page.evaluate((t) => {
        const input = document.querySelector<HTMLInputElement>('#import-file')
        if (!input) throw new Error('No file input')
        const file = new File([t], 'big.csv', { type: 'text/csv' })
        Object.defineProperty(file, 'size', { value: 3 * 1024 ** 3 })
        const transfer = new DataTransfer()
        transfer.items.add(file)
        input.files = transfer.files
        input.dispatchEvent(new Event('change', { bubbles: true }))
      }, NOTES_CSV)
      await page.waitForSelector('[data-testid="file-refused"]', { timeout: 20_000 })
      await sleep(250)
    },
    close: cancel,
  },
  {
    name: 'import CSV failed with a whitelisted sentence (0.19.0)',
    needsMock: true,
    route: '/databases/shop',
    open: async (page) => {
      await fastTasks(page)
      await clickRowButton(page, 'public.notes', 'Import CSV')
      await chooseCsv(page, 'badrow.csv', NOTES_CSV)
      await startImport(page)
      await page.waitForSelector('[data-testid="import-failed-note"]', { timeout: 30_000 })
      await sleep(250)
    },
    close: async (page) => {
      await closeDialogs(page)
      await slowTasks(page)
    },
  },
  {
    name: 'import CSV, the outcome is not known (0.19.0)',
    needsMock: true,
    route: '/databases/shop',
    open: async (page) => {
      await fastTasks(page)
      await clickRowButton(page, 'public.notes', 'Import CSV')
      await chooseCsv(page, 'outcome.csv', NOTES_CSV)
      await startImport(page)
      await page.waitForSelector('[data-testid="outcome-unknown"]', { timeout: 30_000 })
      await sleep(250)
    },
    close: async (page) => {
      await closeDialogs(page)
      await slowTasks(page)
    },
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

/** The states of the data transfer (contract 0.19.0): they are run apart from the older ones, so that each run stays inside the time of one test. */
export const TRANSFER_STATES = STATES.filter((s) => s.name.includes('(0.19.0)'))
/** Every state that is not one of the data transfer. */
export const OLDER_STATES = STATES.filter((s) => !s.name.includes('(0.19.0)'))
