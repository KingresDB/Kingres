// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Distribute with related tables and the operation indicator of a table (contract 0.32.0, a placeholder revision) in a real browser, with the
// mock API in the page: axe-core in light and dark at 1366, 683 and 320 pixels for the Distribute dialog with the related tables, the list of
// tables of the database with the badge of a running change, and the table page with the line "Running now" (for the table that the change takes
// along too), no sideways scrolling of the page, and a dialog that fits the window and keeps its buttons reachable.
import { createRequire } from 'node:module'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import type { Browser, Page } from 'puppeteer-core'
import {
  HA_SWITCHES,
  go,
  launch,
  settled,
  signIn,
  sleep,
  startDevServer,
  type RunningServer,
} from './harness'

const require = createRequire(import.meta.url)
const AXE = require.resolve('axe-core/axe.min.js')
const TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice']

let server: RunningServer
let browser: Browser
let page: Page

/** Opens the application with a switch of the mock (`tables=related` or `tables=related:running`) and signs in. */
async function start(tables: string): Promise<void> {
  await page.goto(`${server.url}/?mock=hostile&latency=0&${HA_SWITCHES}&tables=${tables}`, {
    waitUntil: 'networkidle0',
  })
  await signIn(page)
  await settled(page)
}

beforeAll(async () => {
  server = await startDevServer()
  browser = await launch()
  page = await browser.newPage()
})
afterAll(async () => {
  await browser?.close()
  await server?.close()
})

async function axeViolations(where: string): Promise<string[]> {
  if (!(await page.evaluate(() => typeof window.axe !== 'undefined')))
    await page.addScriptTag({ path: AXE })
  const result = await page.evaluate(
    (tags) => window.axe.run(document, { runOnly: { type: 'tag', values: tags } }),
    TAGS,
  )
  return result.violations.map(
    (v) =>
      `${where}: ${v.id} (${v.impact}) ${v.help}: ${v.nodes
        .slice(0, 2)
        .map((n) => `${n.target.join(' ')} ${n.html.slice(0, 80)}`)
        .join(' | ')}`,
  )
}

async function overflow(): Promise<{ scrollWidth: number; innerWidth: number }> {
  return page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    innerWidth: window.innerWidth,
  }))
}

interface State {
  name: string
  route: string
  open: () => Promise<void>
  close?: () => Promise<void>
}

const closeDialog = async (): Promise<void> => {
  await page.evaluate(() => {
    const cancel = [...document.querySelectorAll<HTMLElement>('[role="dialog"] button')].find(
      (b) => b.textContent?.trim() === 'Cancel',
    )
    cancel?.click()
  })
  await sleep(150)
}

const DIALOG: State[] = [
  {
    name: 'distribute dialog with the related tables',
    route: '/databases/shop/tables/public/orders',
    open: async () => {
      await page.waitForSelector('[data-testid="distribute-table"]', { timeout: 30_000 })
      await page.click('[data-testid="distribute-table"]')
      await page.waitForSelector('[role="dialog"] select', { timeout: 20_000 })
      // the column the keys tie to the related table
      await page.select('[role="dialog"] select', 'customer_id')
      await page.waitForSelector('[data-testid="related-list"]', { timeout: 20_000 })
      await page.waitForSelector('[data-testid="table-change-eligible"]', { timeout: 20_000 })
      await sleep(250)
    },
    close: closeDialog,
  },
]

const RUNNING: State[] = [
  {
    name: 'list of tables with the badge of a running change',
    route: '/databases/shop',
    open: async () => {
      await page.waitForSelector('[data-testid="table-operation-badge"]', { timeout: 30_000 })
      await sleep(250)
    },
  },
  {
    name: 'table page with the running change',
    route: '/databases/shop/tables/public/orders',
    open: async () => {
      await page.waitForSelector('[data-testid="table-operation"]', { timeout: 30_000 })
      await sleep(250)
    },
  },
  {
    name: 'table page of the table that the change takes along',
    route: '/databases/shop/tables/public/customers',
    open: async () => {
      await page.waitForSelector('[data-testid="table-operation"]', { timeout: 30_000 })
      await sleep(250)
    },
  },
]

async function axeAll(states: State[]): Promise<string[]> {
  const failures: string[] = []
  for (const theme of ['light', 'dark']) {
    await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }])
    for (const width of [1366, 683, 320]) {
      await page.setViewport({ width, height: 800 })
      for (const state of states) {
        await go(page, state.route)
        await state.open()
        failures.push(...(await axeViolations(`${state.name} ${theme} ${width}`)))
        await state.close?.()
      }
    }
  }
  return failures
}

async function fits(states: State[]): Promise<string[]> {
  const failures: string[] = []
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }])
  for (const [width, height] of [
    [1366, 820],
    [683, 820],
    [320, 640],
  ] as const) {
    await page.setViewport({ width, height })
    for (const state of states) {
      await go(page, state.route)
      await state.open()
      const g = await overflow()
      if (g.scrollWidth > g.innerWidth)
        failures.push(`${state.name} ${width}: page ${g.scrollWidth} > window ${g.innerWidth}`)
      const dialog = await page.evaluate(() => {
        const d = document.querySelector('[role="dialog"]')
        if (!d) return null
        const r = d.getBoundingClientRect()
        return { left: r.left, right: r.right, width: window.innerWidth }
      })
      if (dialog && (dialog.left < -1 || dialog.right > dialog.width + 1))
        failures.push(`${state.name} ${width}: the dialog is outside the window`)
      await state.close?.()
    }
  }
  return failures
}

describe('distribute with related tables (contract 0.32.0) in a real browser', () => {
  it('the dialog has no axe violation, in light and dark, at 1366, 683 and 320 pixels, and fits the window', async () => {
    await start('related')
    expect(await axeAll(DIALOG)).toEqual([])
    expect(await fits(DIALOG)).toEqual([])
  }, 600_000)
})

describe('the operation indicator of a table (contract 0.32.0) in a real browser', () => {
  it('the list and the table pages have no axe violation while a change runs, and do not push the page sideways', async () => {
    await start('related:running')
    expect(await axeAll(RUNNING)).toEqual([])
    expect(await fits(RUNNING)).toEqual([])
  }, 600_000)
})
