// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-18: DEPENDENCIES.md is true. Every package that is installed without development dependencies
// and every development package with an install script or a licence outside MIT, BSD, Apache-2.0
// and ISC is named there with its version and licence, and the installed tree has nothing the
// file does not know.
import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

interface LockPackage {
  version?: string
  license?: string
  dev?: boolean
  hasInstallScript?: boolean
}
const lock = JSON.parse(readFileSync('package-lock.json', 'utf8')) as {
  packages: Record<string, LockPackage>
}
const doc = readFileSync('DEPENDENCIES.md', 'utf8')
const PERMISSIVE = new Set(['MIT', 'ISC', 'Apache-2.0', 'BSD-2-Clause', 'BSD-3-Clause'])

const packages = Object.entries(lock.packages)
  .filter(([key]) => key !== '')
  .map(([key, p]) => ({ name: key.replace(/^.*node_modules\//, ''), ...p }))

/** Text that is matched literally inside a regular expression. */
function literal(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, (c) => `\\${c}`)
}

/** Is there a table row with this name, version and licence, whatever the padding of the columns? */
function inTable(p: { name: string; version?: string; license?: string }): boolean {
  const cells = [`\`${p.name}\``, p.version ?? '', p.license ?? '?'].map(
    (cell) => `\\s*${literal(cell)}\\s*`,
  )
  return new RegExp(`\\|${cells.join('\\|')}\\|`).test(doc)
}

describe('DEPENDENCIES.md', () => {
  it('lists every package of the production install tree with its version and licence', () => {
    const missing = packages
      .filter((p) => !p.dev)
      .filter((p) => !inTable(p))
      .map((p) => `${p.name}@${p.version} (${p.license})`)
    expect(missing).toEqual([])
  })

  it('names every development package with an install script or a licence outside the usual four', () => {
    const missing = packages
      .filter((p) => p.dev && (p.hasInstallScript || !PERMISSIVE.has(p.license ?? '?')))
      .filter((p) => !doc.includes(p.name) || !doc.includes(p.license ?? '?'))
      .map((p) => `${p.name}@${p.version} (${p.license})`)
    expect(missing).toEqual([])
  })

  it('names the install scripts and the audit state', () => {
    for (const p of packages.filter((x) => x.hasInstallScript)) expect(doc).toContain(p.name)
    expect(doc).toMatch(/npm audit --omit=dev.*0 vulnerabilities/s)
    expect(doc).toMatch(/GHSA-g3hv-75wg-m249/)
  })

  it('states the licences that are in the lockfile', () => {
    const found = new Set(packages.map((p) => p.license ?? '?'))
    for (const licence of found) expect(doc).toContain(licence)
  })
})
