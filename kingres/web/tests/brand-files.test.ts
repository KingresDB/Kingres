// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
// @vitest-environment node
//
// The committed copies of the brand files must match kingres/brand (the source of truth), the web
// app manifest must point at files that exist, and the old product spelling must be gone.
import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
// @ts-expect-error The script is a plain ES module without type declarations.
import { COPIES, staleBrandFiles } from '../scripts/brand-sync.mjs'

const root = fileURLToPath(new URL('..', import.meta.url))

describe('brand files', () => {
  it('are up to date with kingres/brand (run `npm run brand:sync` if not)', () => {
    expect(staleBrandFiles() as string[]).toEqual([])
  })

  it('copy the favicon, touch icon and manifest icon at fixed URLs', () => {
    const targets = (COPIES as [string, string][]).map(([, to]) => to).sort()
    expect(targets).toEqual([
      'public/apple-touch-icon.png',
      'public/favicon-32.png',
      'public/favicon.svg',
      'public/icon-512.png',
    ])
  })

  it('have a manifest with the product name and existing icons', () => {
    const manifest = JSON.parse(
      readFileSync(join(root, 'public/manifest.webmanifest'), 'utf8'),
    ) as {
      name: string
      icons: { src: string; sizes: string }[]
    }
    expect(manifest.name).toBe('KingresDB')
    expect(manifest.icons.some((i) => i.sizes === '512x512')).toBe(true)
    for (const icon of manifest.icons) expect(existsSync(join(root, 'public', icon.src))).toBe(true)
  })

  it('are linked from index.html with theme colours for light and dark', () => {
    const html = readFileSync(join(root, 'index.html'), 'utf8')
    for (const needle of [
      'href="/favicon.svg"',
      'href="/favicon-32.png"',
      'rel="apple-touch-icon"',
      'rel="manifest"',
      'prefers-color-scheme: light',
      'prefers-color-scheme: dark',
    ]) {
      expect(html).toContain(needle)
    }
  })
})

describe('product spelling', () => {
  // Written in two parts so that this file does not contain the old spelling itself.
  const OLD = 'king' + 'ress'

  it('has no occurrence of the old spelling in the web project (case-insensitive)', () => {
    const skip = new Set(['node_modules', 'dist', 'coverage'])
    const hits: string[] = []
    const walk = (dir: string) => {
      for (const name of readdirSync(dir)) {
        if (skip.has(name)) continue
        const path = join(dir, name)
        if (OLD && name.toLowerCase().includes(OLD)) hits.push(`${relative(root, path)} (name)`)
        if (statSync(path).isDirectory()) walk(path)
        else if (
          !/\.(png|ico)$/.test(name) &&
          readFileSync(path, 'utf8').toLowerCase().includes(OLD)
        )
          hits.push(relative(root, path))
      }
    }
    walk(root)
    expect(hits).toEqual([])
  })
})
