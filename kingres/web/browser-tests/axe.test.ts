// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-09, W-16: axe-core in a real browser (where colour contrast, scrollable regions and layout can
// be computed, which jsdom cannot) on every page, in light and dark, at 1366 and 320 pixels,
// with hostile data on the page, and with a dialog open.
import { createRequire } from 'node:module'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import type { Browser, Page } from 'puppeteer-core'
import {
  HA_SWITCHES,
  ROUTES,
  SECURITY_TABS,
  STATES,
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

describe('axe in a real browser', () => {
  it('the sign-in page', async () => {
    const failures: string[] = []
    await page.goto(`${server.url}/?mock=hostile&latency=0&${HA_SWITCHES}`, {
      waitUntil: 'networkidle0',
    })
    await page.waitForSelector('input[autocomplete="username"]')
    for (const theme of ['light', 'dark']) {
      await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }])
      for (const width of [1366, 320]) {
        await page.setViewport({ width, height: 700 })
        failures.push(...(await axeViolations(`login ${theme} ${width}`)))
      }
    }
    expect(failures).toEqual([])
  })

  it('every page, light and dark, wide and narrow, with hostile data', async () => {
    await signIn(page)
    await settled(page)
    const failures: string[] = []
    for (const theme of ['light', 'dark']) {
      await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }])
      for (const width of [1366, 320]) {
        await page.setViewport({ width, height: 800 })
        for (const route of ROUTES) {
          await go(page, route)
          const where = `${route} ${theme} ${width}`
          if (route === '/workbench') {
            await page.$eval('#w-sql', (el) => {
              ;(el as HTMLTextAreaElement).value = 'select * from hostile'
              el.dispatchEvent(new Event('input', { bubbles: true }))
            })
            await clickText(page, 'Run')
            await page.waitForSelector('[data-testid="result"]', { timeout: 20_000 })
          }
          if (route === '/security') {
            for (const tab of SECURITY_TABS) {
              await clickText(page, tab, '[role="tab"]')
              await settled(page)
              failures.push(...(await axeViolations(`${where} ${tab}`)))
            }
            continue
          }
          failures.push(...(await axeViolations(where)))
        }
      }
    }
    expect(failures).toEqual([])
  })

  it('with a dialog open (the page behind it is inert)', async () => {
    await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }])
    await page.setViewport({ width: 1366, height: 800 })
    const failures: string[] = []
    await go(page, '/databases/shop')
    await clickText(page, 'Drop database')
    await page.waitForSelector('[role="dialog"]')
    await sleep(200)
    failures.push(...(await axeViolations('drop database dialog')))
    expect(await page.evaluate(() => Boolean(document.querySelector('body > [inert]')))).toBe(true)
    await clickText(page, 'Cancel')
    await go(page, '/security')
    await clickText(page, 'Network access', '[role="tab"]')
    await settled(page)
    await page.$eval('input[aria-label="Rule 1 comment"]', (el) => {
      ;(el as HTMLInputElement).value = 'changed'
      el.dispatchEvent(new Event('input', { bubbles: true }))
    })
    await clickText(page, 'Review and apply')
    await page.waitForSelector('[role="dialog"]')
    await sleep(200)
    failures.push(...(await axeViolations('network review dialog')))
    expect(failures).toEqual([])
  })

  it('the 0.4.0 and 0.5.0 states: certificate dialogs, proof and digests, add server, leave cluster, stopped system database', async () => {
    const failures: string[] = []
    for (const [theme, width] of [
      ['light', 1366],
      ['dark', 320],
    ] as const) {
      await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }])
      await page.setViewport({ width, height: 800 })
      for (const state of STATES) {
        await go(page, state.route)
        await state.open(page)
        failures.push(...(await axeViolations(`${state.name} ${theme} ${width}`)))
        await state.close?.(page)
      }
    }
    expect(failures).toEqual([])
  })

  it('the setup wizard', async () => {
    const failures: string[] = []
    await page.goto(`${server.url}/?mock=uninitialized&latency=0`, { waitUntil: 'networkidle0' })
    await page.waitForSelector('input')
    for (const theme of ['light', 'dark']) {
      await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }])
      for (const width of [1366, 320]) {
        await page.setViewport({ width, height: 800 })
        failures.push(...(await axeViolations(`setup token ${theme} ${width}`)))
      }
    }
    await page.type('input', 'demo-setup-token')
    await clickText(page, 'Continue')
    await clickText(page, 'Create new cluster')
    await page.waitForSelector('form')
    await sleep(300)
    failures.push(...(await axeViolations('setup create form')))
    expect(failures).toEqual([])
  })
})
