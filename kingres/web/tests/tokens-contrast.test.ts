// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
// @vitest-environment node
//
// Colour contrast of the design tokens in both themes (WCAG 2.1 AA: 4.5:1 for text). jsdom has no
// layout engine, so axe cannot check contrast; this test computes it from tokens.css instead.
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'

const css = readFileSync(new URL('../src/styles/tokens.css', import.meta.url), 'utf8')

/** Returns the custom properties declared in the first block that starts with `selector`. */
function block(selector: string): Record<string, string> {
  const start = css.indexOf(selector)
  if (start < 0) throw new Error(`selector not found: ${selector}`)
  const open = css.indexOf('{', start)
  let depth = 0
  let end = open
  for (let i = open; i < css.length; i++) {
    if (css[i] === '{') depth += 1
    if (css[i] === '}') depth -= 1
    if (depth === 0) {
      end = i
      break
    }
  }
  const vars: Record<string, string> = {}
  for (const m of css.slice(open, end).matchAll(/--([\w-]+):\s*(#[0-9a-fA-F]{6})\s*;/g)) {
    vars[m[1] as string] = m[2] as string
  }
  return vars
}

function luminance(hex: string): number {
  const channels = [1, 3, 5].map((i) => {
    const c = parseInt(hex.slice(i, i + 2), 16) / 255
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
  })
  return (
    0.2126 * (channels[0] as number) +
    0.7152 * (channels[1] as number) +
    0.0722 * (channels[2] as number)
  )
}

function ratio(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number]
  return (hi + 0.05) / (lo + 0.05)
}

const light = block(':root {')
const dark = block(":root[data-theme='dark']")
// The @media (prefers-color-scheme: dark) block must hold the same values as the forced dark theme.
const darkMedia = block(":root:not([data-theme='light'])")

const PAIRS: [string, string][] = [
  ['c-text', 'c-bg'],
  ['c-text', 'c-surface'],
  ['c-text', 'c-surface-2'],
  ['c-text-muted', 'c-bg'],
  ['c-text-muted', 'c-surface'],
  ['c-text-muted', 'c-surface-2'],
  ['c-accent', 'c-surface'],
  ['c-accent', 'c-bg'],
  ['c-accent-text', 'c-accent'],
  ['c-ok-fg', 'c-ok-bg'],
  ['c-warn-fg', 'c-warn-bg'],
  ['c-danger-fg', 'c-danger-bg'],
  ['c-info-fg', 'c-info-bg'],
  ['c-neutral-fg', 'c-neutral-bg'],
  ['c-danger-fg', 'c-surface'],
  ['c-info-fg', 'c-surface'],
]

describe.each([
  ['light', light],
  ['dark', dark],
])('%s theme', (_name, vars) => {
  it.each(PAIRS)('%s on %s has at least 4.5:1', (fg, bg) => {
    expect(vars[fg], fg).toBeDefined()
    expect(vars[bg], bg).toBeDefined()
    expect(ratio(vars[fg] as string, vars[bg] as string)).toBeGreaterThanOrEqual(4.5)
  })

  it('the text colour on every shard group colour and on the solid danger colour has at least 4.5:1', () => {
    for (let i = 1; i <= 8; i++) {
      expect(
        ratio(vars['g-text'] as string, vars[`g-${i}`] as string),
        `g-${i}`,
      ).toBeGreaterThanOrEqual(4.5)
    }
    expect(ratio(vars['g-text'] as string, vars['g-map'] as string)).toBeGreaterThanOrEqual(4.5)
    expect(
      ratio(vars['c-danger-solid-text'] as string, vars['c-danger-solid'] as string),
    ).toBeGreaterThanOrEqual(4.5)
  })
})

describe('dark theme', () => {
  it('has identical values in the system-preference block and the forced block', () => {
    for (const [name, value] of Object.entries(dark)) {
      if (name === 'shadow') continue
      expect(darkMedia[name], name).toBe(value)
    }
  })
})

describe('brand tokens', () => {
  const lightAll = light
  const darkAll = { ...light, ...dark }

  it('define the colours of the brand guide', () => {
    expect(light['brand-gold-light']).toBe('#f6d365')
    expect(light['brand-gold-deep']).toBe('#c8962e')
    expect(light['brand-ink']).toBe('#14171f')
    expect(light['brand-night']).toBe('#0b0d12')
    expect(light['brand-cream']).toBe('#f1eadb')
  })

  it('keep the logo letters readable: ink on the light surface, cream on the dark surface', () => {
    expect(
      ratio(lightAll['brand-ink'] as string, lightAll['c-surface'] as string),
    ).toBeGreaterThanOrEqual(4.5)
    expect(
      ratio(darkAll['brand-cream'] as string, darkAll['c-surface'] as string),
    ).toBeGreaterThanOrEqual(4.5)
    expect(
      ratio(darkAll['brand-gold-light'] as string, darkAll['c-surface'] as string),
    ).toBeGreaterThanOrEqual(4.5)
  })

  it('give the small UI gold accent at least 4.5:1 on the surfaces in both themes', () => {
    for (const vars of [lightAll, darkAll]) {
      expect(ratio(vars['c-brand'] as string, vars['c-surface'] as string)).toBeGreaterThanOrEqual(
        4.5,
      )
      expect(ratio(vars['c-brand'] as string, vars['c-bg'] as string)).toBeGreaterThanOrEqual(4.5)
    }
  })

  it('show why deep gold is never used for text: it is below 4.5:1 on white', () => {
    expect(ratio(light['brand-gold-deep'] as string, '#ffffff')).toBeLessThan(4.5)
  })

  it('are never used as a text colour in the source', () => {
    const files: string[] = []
    const walk = (dir: string) => {
      for (const name of readdirSync(dir)) {
        const path = join(dir, name)
        if (statSync(path).isDirectory()) walk(path)
        else if (name.endsWith('.vue') || name.endsWith('.css')) files.push(path)
      }
    }
    walk(join(process.cwd(), 'src'))
    const offenders = files.filter((f) =>
      /(^|[^-])color:\s*var\(--(brand-gold|c-brand)/m.test(readFileSync(f, 'utf8')),
    )
    expect(offenders).toEqual([])
  })

  it('keep the semantic status colours: the brand gold is not one of them', () => {
    for (const name of ['c-ok-fg', 'c-warn-fg', 'c-danger-fg', 'c-info-fg']) {
      expect(light[name]).not.toBe(light['brand-gold-deep'])
      expect(light[name]).not.toBe(light['brand-gold-light'])
    }
  })
})
