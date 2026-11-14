// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Small helpers around puppeteer-core for the end-to-end flow (development dependency only).
import { existsSync } from 'node:fs'
import puppeteer from 'puppeteer-core'

const CANDIDATES = [
  process.env.CHROME_PATH,
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
]

export function findBrowser() {
  const found = CANDIDATES.find((p) => p && existsSync(p))
  if (!found) throw new Error('No Chrome or Edge found. Set CHROME_PATH.')
  return found
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

/** Launches a headless browser and a page that records every failed request and console error. */
export async function openBrowser({ width = 1366, height = 820, theme = 'light' } = {}) {
  const browser = await puppeteer.launch({
    executablePath: findBrowser(),
    headless: true,
    args: ['--no-sandbox'],
    defaultViewport: { width, height },
  })
  const page = await browser.newPage()
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }])
  const log = { responses: [], consoleErrors: [], pageErrors: [] }
  page.on('response', async (res) => {
    if (res.status() >= 400 && res.url().includes('/api/v1/')) {
      const code = await res
        .json()
        .then((body) => body.code ?? '')
        .catch(() => '(no problem body)')
      log.responses.push({
        method: res.request().method(),
        path: new URL(res.url()).pathname.replace('/api/v1', ''),
        status: res.status(),
        code,
      })
    }
  })
  page.on('console', (m) => {
    if (m.type() === 'error') log.consoleErrors.push(m.text())
  })
  page.on('pageerror', (e) => log.pageErrors.push(e.message))
  return { browser, page, log }
}

/** Page helpers that find controls the way a user does: by visible text or by label. */
export function helpers(page) {
  const api = {
    async goto(url) {
      await page.goto(url, { waitUntil: 'networkidle0' })
    },
    async navigate(path) {
      await page.evaluate((p) => {
        window.history.pushState({}, '', p)
        window.dispatchEvent(new PopStateEvent('popstate'))
      }, path)
      await sleep(600)
    },
    async text() {
      return page.evaluate(() => document.body.innerText)
    },
    async waitForText(text, timeout = 15000) {
      await page.waitForFunction((t) => document.body.innerText.includes(t), { timeout }, text)
    },
    async waitForGone(text, timeout = 15000) {
      await page.waitForFunction((t) => !document.body.innerText.includes(t), { timeout }, text)
    },
    /** Clicks the first visible button or link whose text matches exactly (or by regex source). */
    async click(label, { within = 'body', role = 'button, a' } = {}) {
      const ok = await page.evaluate(
        (label, within, role) => {
          const root = document.querySelector(within) ?? document.body
          const els = [...root.querySelectorAll(role)].filter((e) => e.offsetParent !== null)
          const match = els.find((e) => e.textContent.replace(/\s+/g, ' ').trim() === label)
          if (!match) return false
          match.click()
          return true
        },
        label,
        within,
        role,
      )
      if (!ok) throw new Error(`No visible control with the text "${label}" in ${within}`)
      await sleep(250)
    },
    async fill(label, value, { within = 'body' } = {}) {
      const id = await page.evaluate(
        (label, within) => {
          const root = document.querySelector(within) ?? document.body
          const l = [...root.querySelectorAll('label')].find(
            (x) => x.textContent.replace(/[\s*]+/g, ' ').trim() === label,
          )
          return l?.getAttribute('for') ?? null
        },
        label,
        within,
      )
      if (!id) throw new Error(`No label "${label}" in ${within}`)
      await page.$eval(`[id="${id}"]`, (el) => {
        el.focus()
        el.select?.()
      })
      await page.keyboard.press('Backspace')
      await page.type(`[id="${id}"]`, String(value))
    },
    async select(label, value, { within = 'body' } = {}) {
      const id = await page.evaluate(
        (label, within) => {
          const root = document.querySelector(within) ?? document.body
          return (
            [...root.querySelectorAll('label')]
              .find((x) => x.textContent.replace(/[\s*]+/g, ' ').trim() === label)
              ?.getAttribute('for') ?? null
          )
        },
        label,
        within,
      )
      if (!id) throw new Error(`No label "${label}" in ${within}`)
      await page.select(`[id="${id}"]`, value)
    },
    async check(label, on = true, { within = 'body' } = {}) {
      const ok = await page.evaluate(
        (label, on, within) => {
          const root = document.querySelector(within) ?? document.body
          const l = [...root.querySelectorAll('label')].find((x) =>
            x.textContent.replace(/\s+/g, ' ').trim().startsWith(label),
          )
          const box = l?.querySelector('input') ?? null
          if (!box) return false
          if (box.checked !== on) box.click()
          return true
        },
        label,
        on,
        within,
      )
      if (!ok) throw new Error(`No checkbox or radio "${label}" in ${within}`)
    },
    /** The text of the visible alert notices (errors the user is told about). */
    async alerts() {
      return page.evaluate(() =>
        [...document.querySelectorAll('[role=alert]')]
          .filter((e) => e.offsetParent !== null)
          .map((e) => e.textContent.replace(/\s+/g, ' ').trim().slice(0, 160)),
      )
    },
    async shot(path) {
      await page.screenshot({ path })
    },
  }
  return api
}
