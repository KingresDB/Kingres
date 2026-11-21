// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.15.0 (distributed schemas) in a real browser, with the hostile mock: the Schemas section and its dialogs stay inside
// the window at 1366, 683 and 320 pixels, the actions of a row are in reach, and the section and the dialogs work by keyboard
// alone (the row actions, the radios of "Shard by", the focus that stays in a dialog and returns to the button). The pages and
// the dialogs are also run through axe by axe.test.ts, because /databases/tenants, /shards?db=tenants and the states of the
// dialogs are in ROUTES and STATES of the harness.
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import type { Browser, Page } from 'puppeteer-core'
import {
  STATES,
  clickText,
  go,
  launch,
  schemaAction,
  signIn,
  settled,
  sleep,
  startDevServer,
  type RunningServer,
} from './harness'

let server: RunningServer
let browser: Browser
let page: Page

beforeAll(async () => {
  server = await startDevServer()
  browser = await launch()
  page = await browser.newPage()
  await page.goto(`${server.url}/?mock=hostile&latency=0`, { waitUntil: 'networkidle0' })
  await signIn(page)
  await settled(page)
})
afterAll(async () => {
  await browser?.close()
  await server?.close()
})

/** How far the page is wider than the window, and which elements stick out (as layout.test.ts measures it). */
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
        if ((o === 'auto' || o === 'scroll' || o === 'hidden') && p.scrollWidth >= p.clientWidth) {
          clipped = true
          break
        }
      }
      if (!clipped && r.right > width + 1 && getComputedStyle(el).position !== 'fixed')
        offenders.push(`${el.tagName.toLowerCase()}.${el.className} ${Math.round(r.right)}`)
    }
    return {
      scrollWidth: Math.max(document.documentElement.scrollWidth, document.body.scrollWidth),
      innerWidth: width,
      offenders: offenders.slice(0, 8),
    }
  })
}

const schemasListed = () =>
  page.waitForSelector('[data-testid="schemas-table"] tbody tr', { timeout: 30_000 })

for (const [label, width, height] of [
  ['1366 px', 1366, 820],
  ['683 px (200 percent zoom)', 683, 410],
  ['320 px', 320, 640],
] as const) {
  describe(`the Schemas section at ${label}`, () => {
    it('does not push the page sideways, with the hostile schema names (one of 5000 characters) on it', async () => {
      await page.setViewport({ width, height })
      await go(page, '/databases/tenants')
      await schemasListed()
      const hostile = await page.evaluate(() =>
        [...document.querySelectorAll('[data-testid="schemas-table"] tbody th')].some((th) =>
          th.textContent?.includes('Show all 5000 characters'),
        ),
      )
      expect(hostile).toBe(true)
      const g = await overflow()
      expect(g.scrollWidth, g.offenders.join(', ')).toBeLessThanOrEqual(g.innerWidth)
    })

    it('the schemas table is a keyboard-reachable region with a name, and its actions can be reached', async () => {
      await page.setViewport({ width, height })
      await go(page, '/databases/tenants')
      await schemasListed()
      const region = await page.evaluate(() => {
        const wrap = document
          .querySelector('[data-testid="schemas-table"]')
          ?.closest('.table-wrap') as HTMLElement | null
        return wrap
          ? {
              name: wrap.getAttribute('aria-label'),
              tabindex: wrap.getAttribute('tabindex'),
              wider: wrap.scrollWidth > wrap.clientWidth,
            }
          : null
      })
      expect(region?.name).toMatch(/schemas of the database/i)
      expect(region?.tabindex).toBe('0')
      // The row actions are in the scrolling region, so a box that scrolls can be scrolled to them with the keyboard.
      if (region?.wider) {
        await page.evaluate(() =>
          (
            document
              .querySelector('[data-testid="schemas-table"]')
              ?.closest('.table-wrap') as HTMLElement
          ).focus(),
        )
        const before = await page.evaluate(() => (document.activeElement as HTMLElement).scrollLeft)
        for (let i = 0; i < 6; i++) await page.keyboard.press('ArrowRight')
        await sleep(100)
        const after = await page.evaluate(() => (document.activeElement as HTMLElement).scrollLeft)
        expect(after).toBeGreaterThan(before)
      }
    })

    it('the dialogs of the section fit the window', async () => {
      await page.setViewport({ width, height })
      const failures: string[] = []
      for (const state of STATES.filter((s) => s.name.includes('(0.15.0)'))) {
        await go(page, state.route)
        await state.open(page)
        const g = await overflow()
        if (g.scrollWidth > g.innerWidth)
          failures.push(`${state.name}: page ${g.scrollWidth} > window ${g.innerWidth}`)
        const dialog = await page.evaluate(() => {
          const el = document.querySelector('[role="dialog"]')
          if (!el) return null
          const r = el.getBoundingClientRect()
          return { left: r.left, right: r.right, width: window.innerWidth }
        })
        if (!dialog) failures.push(`${state.name}: no dialog`)
        else if (dialog.left < -1 || dialog.right > dialog.width + 1)
          failures.push(`${state.name}: the dialog is outside the window`)
        await state.close?.(page)
      }
      expect(failures).toEqual([])
    })
  })
}

describe('by keyboard alone', () => {
  it('a row action opens its dialog on Enter, the focus stays inside it, and Escape gives the focus back to the button', async () => {
    await page.setViewport({ width: 1366, height: 820 })
    await go(page, '/databases/tenants')
    await schemasListed()
    // Reach the button of the row of tenant_acme the way a person does: it is a button of the page, focused, then Enter.
    await page.evaluate(() => {
      const row = [...document.querySelectorAll('[data-testid="schemas-table"] tbody tr')].find(
        (r) => r.querySelector('th')?.textContent?.trim() === 'tenant_acme',
      )
      const button = [...(row?.querySelectorAll('button') ?? [])].find((b) =>
        /^Move schema/.test(b.textContent?.trim() ?? ''),
      ) as HTMLElement
      button.setAttribute('data-test-origin', 'true')
      button.focus()
    })
    await page.keyboard.press('Enter')
    await page.waitForSelector('[role="dialog"] select', { timeout: 20_000 })
    const inside = () => page.evaluate(() => !!document.activeElement?.closest('[role="dialog"]'))
    expect(await inside()).toBe(true)
    for (let i = 0; i < 25; i++) {
      await page.keyboard.press('Tab')
      expect(await inside(), `after ${i + 1} Tab`).toBe(true)
    }
    await page.keyboard.press('Escape')
    await page.waitForFunction(() => !document.querySelector('[role="dialog"]'), {
      timeout: 10_000,
    })
    expect(
      await page.evaluate(() => document.activeElement?.getAttribute('data-test-origin')),
    ).toBe('true')
  })

  it('the group is chosen with the keyboard and the move is refused and then allowed with the checkbox, with no mouse', async () => {
    await page.setViewport({ width: 1366, height: 820 })
    await go(page, '/databases/tenants')
    await schemasListed()
    await schemaAction(page, 'tenant_initech', 'Move schema')
    await page.waitForSelector('[role="dialog"] select', { timeout: 20_000 })
    await page.focus('[role="dialog"] select')
    // The select: the first option is the prompt, the next one is group 1 (the schema is on group 3).
    await page.keyboard.press('ArrowDown')
    expect(
      await page.$eval('[role="dialog"] select', (el) => (el as HTMLSelectElement).value),
    ).toBe('1')
    // Tab to the "Move schema" button (the select is followed by Cancel and the button) and press it.
    await page.focus('[role="dialog"] .dlg-foot .btn-primary')
    await page.keyboard.press('Enter')
    await page.waitForSelector('[role="dialog"] [data-testid="keyless-tables"]', {
      timeout: 20_000,
    })
    const disabled = await page.$eval(
      '[role="dialog"] .dlg-foot .btn-primary',
      (el) => (el as HTMLButtonElement).disabled,
    )
    expect(disabled).toBe(true)
    await page.focus('[role="dialog"] [data-testid="keyless-tables"] input[type="checkbox"]')
    await page.keyboard.press('Space')
    expect(
      await page.$eval(
        '[role="dialog"] .dlg-foot .btn-primary',
        (el) => (el as HTMLButtonElement).disabled,
      ),
    ).toBe(false)
    await clickText(page, 'Cancel', '[role="dialog"] button')
  })

  it('the radios of "Shard by" are chosen with the arrow keys, and the default for new schemas follows the mode', async () => {
    await page.setViewport({ width: 1366, height: 820 })
    await go(page, '/databases')
    await clickText(page, 'Create database')
    await page.waitForSelector('[role="dialog"] input[type="checkbox"]', { timeout: 20_000 })
    // The sharding checkbox is the first one: Space turns it on.
    await page.focus('[role="dialog"] fieldset input[type="checkbox"]')
    const sharding = await page.evaluate(() => {
      const label = [...document.querySelectorAll('[role="dialog"] label')].find((l) =>
        l.textContent?.trim().startsWith('Spread this database'),
      )
      const input = label?.querySelector('input') as HTMLInputElement
      input.focus()
      return !!input
    })
    expect(sharding).toBe(true)
    await page.keyboard.press('Space')
    await page.waitForSelector('[role="dialog"] input[type="radio"]', { timeout: 20_000 })
    const state = () =>
      page.evaluate(() => {
        const radio = [
          ...document.querySelectorAll<HTMLInputElement>('[role="dialog"] input[type="radio"]'),
        ]
        const box = [...document.querySelectorAll('[role="dialog"] label')]
          .find((l) => l.textContent?.includes('Distribute new schemas automatically'))
          ?.querySelector('input') as HTMLInputElement | null
        return {
          mode: radio.findIndex((r) => r.checked),
          box: box ? box.checked : null,
          buckets: !!document.querySelector('[role="dialog"] input[type="number"][min="1"]'),
        }
      })
    expect(await state()).toMatchObject({ mode: 0, box: null, buckets: true })
    await page.focus('[role="dialog"] input[type="radio"]')
    await page.keyboard.press('ArrowDown')
    expect(await state()).toMatchObject({ mode: 1, box: true, buckets: false })
    await page.keyboard.press('ArrowDown')
    expect(await state()).toMatchObject({ mode: 2, box: false, buckets: true })
    await clickText(page, 'Cancel', '[role="dialog"] button')
  })
})
