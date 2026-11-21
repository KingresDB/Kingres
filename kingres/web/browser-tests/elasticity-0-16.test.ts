// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.16.0 in a real browser: the dialogs of the drain, the weight, the split and the schemas, and the automatic card
// of the Rebalance page, pass axe (colour contrast included, in light and dark) and do not push the page sideways at 1366 and 320 pixels.
import { createRequire } from 'node:module'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import type { Browser, Page } from 'puppeteer-core'
import {
  clickText,
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

beforeAll(async () => {
  server = await startDevServer()
  browser = await launch()
  page = await browser.newPage()
  await page.goto(`${server.url}/?latency=0`, { waitUntil: 'networkidle0' })
  await signIn(page)
  await settled(page)
})
afterAll(async () => {
  await browser?.close()
  await server?.close()
})

async function violations(where: string): Promise<string[]> {
  if (!(await page.evaluate(() => typeof window.axe !== 'undefined')))
    await page.addScriptTag({ path: AXE })
  const result = await page.evaluate(
    (tags) => window.axe.run(document, { runOnly: { type: 'tag', values: tags } }),
    TAGS,
  )
  const out = result.violations.map(
    (v) =>
      `${where}: ${v.id} (${v.impact}) ${v.help}: ${v.nodes
        .slice(0, 2)
        .map((n) => n.target.join(' '))
        .join(' | ')}`,
  )
  const over = await page.evaluate(
    () =>
      Math.max(document.documentElement.scrollWidth, document.body.scrollWidth) - window.innerWidth,
  )
  if (over > 0) out.push(`${where}: the page is ${over}px wider than the window`)
  return out
}

/** Opens something, checks the page in both themes and widths, and closes the dialog again. */
async function check(
  failures: string[],
  route: string,
  name: string,
  open: () => Promise<void>,
  close: string | null = 'Cancel',
) {
  for (const theme of ['light', 'dark']) {
    await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }])
    for (const width of [1366, 320]) {
      await page.setViewport({ width, height: 800 })
      await go(page, route)
      await settled(page)
      await open()
      await sleep(300)
      failures.push(...(await violations(`${name} ${theme} ${width}`)))
      if (close) await clickText(page, close).catch(() => undefined)
    }
  }
}

describe('contract 0.16.0 in a real browser', () => {
  it('the dialogs of the servers page and the automatic card', async () => {
    const failures: string[] = []
    await check(failures, '/servers', 'drain dialog', async () => {
      await clickText(page, 'Drain')
      await page.waitForSelector('[data-testid="drain-preview"]')
    })
    await check(failures, '/servers', 'weight dialog', async () => {
      await clickText(page, 'Weight', 'button')
      await page.waitForSelector('[role="dialog"]')
    })
    await check(
      failures,
      '/rebalance',
      'automatic card',
      async () => {
        await page.waitForSelector('[data-testid="automatic-card"]')
        if (!(await page.$('[data-testid="auto-form"]')))
          await page.click('[data-testid="auto-edit"]')
        await page.waitForSelector('[data-testid="auto-form"]')
      },
      null,
    )
    expect(failures).toEqual([])
  })

  it('the dialogs of the schemas and of the split', async () => {
    const failures: string[] = []
    await check(failures, '/databases/shop', 'distribute schema dialog', async () => {
      await clickText(page, 'Distribute schema')
      await page.waitForSelector(
        '[data-testid="schema-blockers"], [role="dialog"] .field, [role="dialog"] p',
      )
    })
    await check(failures, '/databases/shop', 'move schema dialog', async () => {
      await clickText(page, 'Move schema')
      await page.waitForSelector('[role="dialog"]')
    })
    await check(failures, '/databases/shop', 'make local dialog', async () => {
      await clickText(page, 'Make local')
      await page.waitForSelector('[role="dialog"]')
    })
    await check(failures, '/shards', 'split dialog', async () => {
      await page.waitForSelector('.bucket-grid .cell')
      await page.click('.bucket-grid .cell')
      await page
        .waitForSelector('[data-testid="bucket-detail"]', { timeout: 8000 })
        .catch(async () => {
          throw new Error(
            await page.evaluate(
              () => document.querySelector('main')?.innerText.slice(0, 600) ?? 'no main',
            ),
          )
        })
      await page.click('[data-testid="split-bucket"]')
      await page.waitForSelector('[data-testid="split-preview"]')
    })
    expect(failures).toEqual([])
  })
})
