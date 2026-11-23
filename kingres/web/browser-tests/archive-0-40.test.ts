// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The database export and the archive validation of phase 14b (contract 0.40.0, a placeholder revision) in a real browser, with the
// mock API in the page: axe-core in light and dark at 1366, 683 and 320 pixels for every state (the actions in the header of the database
// page, the export dialog, its result with the download, a role that cannot read every table, a failed export, the validation dialog and
// the report of a valid archive, of one with refused statements, of one that does not fit, with ledger tables, and of an invalid one),
// no sideways scrolling of the page, and a dialog that fits the window and keeps its buttons reachable.
import { createRequire } from 'node:module'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import type { Browser, Page } from 'puppeteer-core'
import {
  ARCHIVE_STATES,
  HA_SWITCHES,
  go,
  launch,
  settled,
  signIn,
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
  await page.goto(`${server.url}/?mock=hostile&latency=0&${HA_SWITCHES}`, {
    waitUntil: 'networkidle0',
  })
  await signIn(page)
  await settled(page)
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

/** How far the page is wider than the window, and which elements stick out of it (outside a scrolling box). */
async function overflow(): Promise<{
  scrollWidth: number
  innerWidth: number
  offenders: string[]
}> {
  return page.evaluate(() => {
    const width = window.innerWidth
    const offenders: string[] = []
    for (const el of Array.from(document.body.querySelectorAll<HTMLElement>('*'))) {
      const r = el.getBoundingClientRect()
      if (r.width === 0 || r.height === 0) continue
      let clipped = false
      for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
        const o = getComputedStyle(p).overflowX
        if (o === 'auto' || o === 'scroll' || o === 'hidden') {
          clipped = true
          break
        }
      }
      if (!clipped && r.right > width + 1)
        offenders.push(`${el.tagName.toLowerCase()}.${String(el.className).slice(0, 30)}`)
    }
    return {
      scrollWidth: document.documentElement.scrollWidth,
      innerWidth: width,
      offenders: offenders.slice(0, 5),
    }
  })
}

describe('phase 14b (contract 0.40.0) in a real browser', () => {
  it('every state has no axe violation, in light and dark, at 1366 and 320 pixels', async () => {
    const failures: string[] = []
    for (const theme of ['light', 'dark']) {
      await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }])
      for (const width of [1366, 683, 320]) {
        await page.setViewport({ width, height: 800 })
        for (const state of ARCHIVE_STATES) {
          await go(page, state.route)
          await state.open(page)
          failures.push(...(await axeViolations(`${state.name} ${theme} ${width}`)))
          await state.close?.(page)
        }
      }
    }
    expect(failures).toEqual([])
  }, 600_000)

  it('no state pushes the page sideways at 1366 or 320 pixels, and a dialog fits the window with its buttons in reach', async () => {
    const failures: string[] = []
    await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }])
    for (const [width, height] of [
      [1366, 820],
      [683, 820],
      [320, 640],
    ] as const) {
      await page.setViewport({ width, height })
      for (const state of ARCHIVE_STATES) {
        await go(page, state.route)
        await state.open(page)
        const g = await overflow()
        if (g.scrollWidth > g.innerWidth)
          failures.push(
            `${state.name} ${width}: page ${g.scrollWidth} > window ${g.innerWidth} (${g.offenders.join(', ')})`,
          )
        const dialog = await page.evaluate(() => {
          const d = document.querySelector('[role="dialog"]')
          if (!d) return null
          const r = d.getBoundingClientRect()
          const foot = [...d.querySelectorAll('button')].filter((b) => {
            const br = b.getBoundingClientRect()
            return br.width > 0 && br.height > 0
          })
          const last = foot[foot.length - 1]?.getBoundingClientRect()
          return {
            left: r.left,
            right: r.right,
            width: window.innerWidth,
            buttons: foot.length,
            lastVisible: last ? last.left >= 0 && last.right <= window.innerWidth + 1 : false,
          }
        })
        if (dialog) {
          if (dialog.left < -1 || dialog.right > dialog.width + 1)
            failures.push(`${state.name} ${width}: the dialog is outside the window`)
          if (!dialog.lastVisible)
            failures.push(`${state.name} ${width}: a button is outside the window`)
        }
        await state.close?.(page)
      }
    }
    expect(failures).toEqual([])
  }, 600_000)
})
