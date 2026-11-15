// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-06, W-09, W-10: in a real browser, with hostile data (a 5000-character name among others),
// no page scrolls sideways at 1366, 683 (200 percent zoom) or 320 pixels, the actions of a row
// stay reachable, "Sign out" is reachable, and a scrolling table can be reached and scrolled
// with the keyboard.
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

interface Geometry {
  scrollWidth: number
  innerWidth: number
  offenders: string[]
}

/** How far the page is wider than the window, and which elements stick out. */
async function geometry(): Promise<Geometry> {
  return page.evaluate(() => {
    const width = window.innerWidth
    const offenders: string[] = []
    for (const el of Array.from(document.body.querySelectorAll<HTMLElement>('*'))) {
      const r = el.getBoundingClientRect()
      if (r.width === 0 || r.height === 0) continue
      // Inside a scrolling box an element may stick out of the box, not of the page.
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

/** Is the element inside the window and inside every scrolling box around it, without scrolling? */
async function reachable(label: string, scope = 'body'): Promise<boolean> {
  return page.evaluate(
    (text, within) => {
      const root = document.querySelector(within) ?? document.body
      const el = [...root.querySelectorAll<HTMLElement>('button, a')].find(
        (e) => e.textContent?.trim() === text,
      )
      if (!el) return false
      const r = el.getBoundingClientRect()
      if (r.left < 0 || r.right > window.innerWidth + 1 || r.width === 0) return false
      for (let p = el.parentElement; p; p = p.parentElement) {
        const o = getComputedStyle(p).overflowX
        if (o === 'auto' || o === 'scroll') {
          const b = p.getBoundingClientRect()
          if (r.left < b.left - 1 || r.right > b.right + 1) return false
        }
      }
      return true
    },
    label,
    scope,
  )
}

async function visitAll(visit: (route: string) => Promise<void>): Promise<void> {
  for (const route of ROUTES) {
    await go(page, route)
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
        await visit(`${route} (${tab})`)
      }
      continue
    }
    await visit(route)
  }
}

describe('a very long name does not push the actions away (W-06)', () => {
  it('the roles table: Edit, Disable and Drop of the 5000-character role are in reach at 1366 px', async () => {
    await page.setViewport({ width: 1366, height: 820 })
    await go(page, '/security')
    const row = await page.evaluate(() => {
      const tr = [...document.querySelectorAll('tr')].find((r) =>
        r.textContent?.includes('Show all 5000 characters'),
      )
      if (!tr) return null
      tr.setAttribute('data-long', 'true')
      return { width: tr.getBoundingClientRect().width }
    })
    expect(row).not.toBeNull()
    for (const label of ['Edit', 'Disable', 'Drop']) {
      expect(await reachable(label, 'tr[data-long="true"]'), label).toBe(true)
    }
    const g = await geometry()
    expect(g.scrollWidth, g.offenders.join(', ')).toBeLessThanOrEqual(g.innerWidth)
  })

  it('the full value can be seen on demand', async () => {
    await clickText(page, 'Show all 5000 characters')
    const length = await page.evaluate(
      () => document.querySelector('tr[data-long="true"] th')?.textContent?.length ?? 0,
    )
    expect(length).toBeGreaterThan(5000)
    const g = await geometry()
    expect(g.scrollWidth, g.offenders.join(', ')).toBeLessThanOrEqual(g.innerWidth)
  })
})

for (const [label, width, height] of [
  ['1366 px', 1366, 820],
  ['683 px (200 percent zoom)', 683, 410],
  ['320 px', 320, 640],
] as const) {
  describe(`no sideways scrolling of the page at ${label} (W-10)`, () => {
    it('every page, with hostile data, fits the window and keeps "Sign out" reachable', async () => {
      await page.setViewport({ width, height })
      const failures: string[] = []
      await visitAll(async (route) => {
        const g = await geometry()
        if (g.scrollWidth > g.innerWidth)
          failures.push(
            `${route}: page ${g.scrollWidth} > window ${g.innerWidth} (${g.offenders.join(', ')})`,
          )
        if (!(await reachable('Sign out'))) failures.push(`${route}: "Sign out" is not reachable`)
      })
      expect(failures).toEqual([])
    })
  })
}

for (const [label, width, height] of [
  ['1366 px', 1366, 820],
  ['320 px', 320, 640],
] as const) {
  describe(`the 0.4.0 and 0.5.0 states fit the window at ${label} (W2-16)`, () => {
    it('dialogs, the proof with its digests and the stopped state do not push the page sideways or out of the window', async () => {
      await page.setViewport({ width, height })
      const failures: string[] = []
      for (const state of STATES) {
        await go(page, state.route)
        await state.open(page)
        const g = await geometry()
        if (g.scrollWidth > g.innerWidth)
          failures.push(
            `${state.name}: page ${g.scrollWidth} > window ${g.innerWidth} (${g.offenders.join(', ')})`,
          )
        const dialog = await page.evaluate(() => {
          const el = document.querySelector('[role="dialog"]')
          if (!el) return null
          const r = el.getBoundingClientRect()
          return { left: r.left, right: r.right, width: window.innerWidth }
        })
        if (dialog && (dialog.left < -1 || dialog.right > dialog.width + 1))
          failures.push(`${state.name}: the dialog is outside the window`)
        await state.close?.(page)
      }
      expect(failures).toEqual([])
    })
  })
}

describe('a scrolling table is reachable and scrollable by keyboard (W-09)', () => {
  it('Tab reaches the log table, which has a name, and the arrow keys scroll it', async () => {
    await page.setViewport({ width: 320, height: 640 })
    await go(page, '/logs')
    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur())
    let found: { name: string | null } | null = null
    for (let i = 0; i < 80 && !found; i++) {
      await page.keyboard.press('Tab')
      found = await page.evaluate(() => {
        const el = document.activeElement as HTMLElement | null
        return el?.classList.contains('table-wrap') ? { name: el.getAttribute('aria-label') } : null
      })
    }
    expect(found?.name).toMatch(/log lines/i)
    const before = await page.evaluate(() => (document.activeElement as HTMLElement).scrollLeft)
    for (let i = 0; i < 5; i++) await page.keyboard.press('ArrowRight')
    await sleep(100)
    const after = await page.evaluate(() => (document.activeElement as HTMLElement).scrollLeft)
    expect(after).toBeGreaterThan(before)
  })
})

describe('the top bar is one black row at every width', () => {
  for (const width of [1366, 683, 390, 320]) {
    it(`at ${width} px nothing in it wraps to a second row`, async () => {
      await page.setViewport({ width, height: 700 })
      await go(page, '/dashboard')
      const bar = await page.evaluate(() => {
        const el = document.querySelector('header.topbar') as HTMLElement
        const box = el.getBoundingClientRect()
        const rows = new Set<number>()
        for (const child of Array.from(el.children) as HTMLElement[]) {
          const r = child.getBoundingClientRect()
          if (r.width === 0 || r.height === 0) continue
          // Children of one row have their centres at the same height.
          rows.add(Math.round((r.top + r.height / 2) / 6))
        }
        return { height: box.height, rows: rows.size, right: box.right, window: innerWidth }
      })
      expect(bar.rows).toBe(1)
      expect(bar.height).toBeLessThanOrEqual(56)
      expect(bar.right).toBeLessThanOrEqual(bar.window + 1)
      expect(await reachable('Sign out')).toBe(true)
    })
  }

  it('is the same black in the light and in the dark theme', async () => {
    await page.setViewport({ width: 1366, height: 700 })
    const colours: string[] = []
    for (const scheme of ['light', 'dark']) {
      await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: scheme }])
      await go(page, '/dashboard')
      colours.push(
        await page.evaluate(
          () =>
            getComputedStyle(document.querySelector('header.topbar') as HTMLElement)
              .backgroundColor,
        ),
      )
    }
    await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }])
    expect(colours[0]).toBe('rgb(10, 10, 11)')
    expect(colours[1]).toBe(colours[0])
  })
})

describe('the controls of a form row are on one line', () => {
  // A hint under one control (the timeout of the workbench) used to lift that control above
  // its neighbours, and a button next to a labelled field sat at the height of the label.
  for (const [route, selectors] of [
    ['/workbench', ['#w-db', '#w-limit', '#w-timeout', '.form-row > button']],
    ['/ledger', ['#v-scope', '.form-row > button']],
    ['/security', ['#role-search', '.form-row > button']],
  ] as const) {
    it(`${route}: controls that share a line start at the same height`, async () => {
      await page.setViewport({ width: 1366, height: 820 })
      await go(page, route)
      const boxes = await page.evaluate((list) => {
        const first = document.querySelector(list[0])
        const row = first?.closest('.form-row')
        if (!row) return null
        return list.map((s) => {
          const r = row.querySelector(s)?.getBoundingClientRect()
          return r ? { top: Math.round(r.top), bottom: Math.round(r.bottom) } : null
        })
      }, selectors)
      expect(boxes).not.toBeNull()
      const found = (boxes ?? []).filter((b): b is { top: number; bottom: number } => b !== null)
      expect(found.length).toBe(selectors.length)
      // The mock data has a very long database name, so the row may wrap: only controls whose
      // boxes overlap vertically are on one line, and those must start at the same height.
      const uneven: string[] = []
      for (let i = 0; i < found.length; i++)
        for (let j = i + 1; j < found.length; j++) {
          const a = found[i]!
          const b = found[j]!
          const overlap = Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top)
          if (overlap > 0 && Math.abs(a.top - b.top) > 2)
            uneven.push(`${selectors[i]} at ${a.top} and ${selectors[j]} at ${b.top}`)
        }
      expect(uneven).toEqual([])
    })
  }
})

describe('the legend of a chart keeps each name and value whole', () => {
  it('a line of the legend is as high as one line of text', async () => {
    await page.setViewport({ width: 1366, height: 820 })
    await go(page, '/dashboard')
    await page.waitForSelector('.legend li')
    const heights = await page.evaluate(() =>
      [...document.querySelectorAll('.legend li')].map((li) => li.getBoundingClientRect().height),
    )
    expect(heights.length).toBeGreaterThan(0)
    expect(Math.max(...heights)).toBeLessThan(28)
  })
})

for (const [label, width, height] of [
  ['1366 px', 1366, 820],
  ['683 px (200 percent zoom)', 683, 410],
  ['320 px', 320, 640],
] as const) {
  describe(`the parts of contract 0.7.0 fit the window at ${label}`, () => {
    it('Replication: the lease, the commit rule and the standbys stay inside the window and say what happened', async () => {
      await page.setViewport({ width, height })
      await go(page, '/replication')
      await page.waitForSelector('[data-testid="group-lease"]')
      // The alerts arrive after the topology: the group that lost its standby says so once they are there.
      await page.waitForFunction(
        () =>
          [...document.querySelectorAll('[data-testid="commit-rule"]')].some((e) =>
            /not any more/i.test(e.textContent ?? ''),
          ),
        { timeout: 30_000 },
      )
      const found = await page.evaluate(() => {
        const out: string[] = []
        const w = window.innerWidth
        for (const sel of [
          '[data-testid="group-lease"]',
          '[data-testid="commit-rule"]',
          '[data-testid="standby-task"]',
        ])
          for (const el of document.querySelectorAll(sel)) {
            const r = el.getBoundingClientRect()
            if (r.left < -1 || r.right > w + 1)
              out.push(sel + ' ' + Math.round(r.left) + '..' + Math.round(r.right))
          }
        const lost = [...document.querySelectorAll('[data-testid="group-lease"]')].find(
          (e) => (e as HTMLElement).dataset.leaseState === 'lost',
        )
        return {
          outside: out,
          leases: document.querySelectorAll('[data-testid="group-lease"]').length,
          lostText: lost?.textContent ?? '',
          dropped: [...document.querySelectorAll('[data-testid="commit-rule"]')].some((e) =>
            /not any more/i.test(e.textContent ?? ''),
          ),
        }
      })
      expect(found.outside).toEqual([])
      expect(found.leases).toBeGreaterThanOrEqual(4)
      expect(found.lostText).toMatch(/stopped on purpose/i)
      expect(found.dropped).toBe(true)
      const g = await geometry()
      expect(g.scrollWidth, g.offenders.join(', ')).toBeLessThanOrEqual(g.innerWidth)
    })

    it('Dashboard: the client entry point group has its charts, one legend line per series, and a not-live list', async () => {
      await page.setViewport({ width, height })
      await go(page, '/dashboard')
      await page.waitForSelector('[data-testid="client-entry-point"]', { timeout: 30_000 })
      await page.waitForFunction(
        () => document.querySelectorAll('[data-testid^="metric-client"] .legend li').length >= 16,
        { timeout: 30_000 },
      )
      const found = await page.evaluate(() => ({
        charts: document.querySelectorAll('[data-testid^="metric-client"]').length,
        tallest: Math.max(
          ...[...document.querySelectorAll('[data-testid^="metric-client"] .legend li')].map(
            (li) => li.getBoundingClientRect().height,
          ),
        ),
        notLive: document.querySelector('[data-testid="not-live-servers"]')?.textContent ?? '',
      }))
      expect(found.charts).toBe(16)
      expect(found.tallest).toBeLessThan(28)
      expect(found.notLive).toMatch(/DEAD/)
      const g = await geometry()
      expect(g.scrollWidth, g.offenders.join(', ')).toBeLessThanOrEqual(g.innerWidth)
    })

    it('Servers and the server page: the heartbeat age and the notice fit, the lease list is in reach', async () => {
      await page.setViewport({ width, height })
      await go(page, '/servers')
      await page.waitForSelector('[data-testid="last-heartbeat"]')
      let g = await geometry()
      expect(g.scrollWidth, g.offenders.join(', ')).toBeLessThanOrEqual(g.innerWidth)
      await go(page, '/servers/10.0.0.13')
      await page.waitForSelector('[data-testid="liveness-notice"]')
      g = await geometry()
      expect(g.scrollWidth, g.offenders.join(', ')).toBeLessThanOrEqual(g.innerWidth)
      await go(page, '/servers/10.0.0.11')
      await page.waitForSelector('[data-testid="server-leases"] [data-testid="group-lease"]')
      g = await geometry()
      expect(g.scrollWidth, g.offenders.join(', ')).toBeLessThanOrEqual(g.innerWidth)
    })

    it('the banner of a lost leader keeps the top bar to one row and fits the window', async () => {
      await page.setViewport({ width, height })
      await go(page, '/servers')
      await page.evaluate(() => {
        ;(
          window as unknown as { __kingresMock: { config: { leaderLost: boolean } } }
        ).__kingresMock.config.leaderLost = true
      })
      try {
        await page.waitForSelector('[data-testid="banner-leader"]', { timeout: 30_000 })
        const g = await geometry()
        expect(g.scrollWidth, g.offenders.join(', ')).toBeLessThanOrEqual(g.innerWidth)
        const rows = await page.evaluate(() => {
          const el = document.querySelector('header.topbar') as HTMLElement
          const set = new Set<number>()
          for (const child of Array.from(el.children) as HTMLElement[]) {
            const r = child.getBoundingClientRect()
            if (r.width > 0 && r.height > 0) set.add(Math.round((r.top + r.height / 2) / 6))
          }
          return set.size
        })
        expect(rows).toBe(1)
        expect(await reachable('Sign out')).toBe(true)
      } finally {
        await page.evaluate(() => {
          ;(
            window as unknown as { __kingresMock: { config: { leaderLost: boolean } } }
          ).__kingresMock.config.leaderLost = false
        })
      }
    })
  })
}
