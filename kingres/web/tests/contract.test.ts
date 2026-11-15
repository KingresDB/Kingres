// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
// @vitest-environment node
//
// Guards that keep the panel, the mock API and the contract in step.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { parse } from 'yaml'
import { auditActions, operations } from '@/api/operations.generated'
import { createHandlers } from '@/mocks/handlers'
// @ts-expect-error The generator is a plain ES module script without type declarations.
import { staleFiles } from '../scripts/gen-api.mjs'

const root = fileURLToPath(new URL('..', import.meta.url))
// The same file the generator reads: KINGRES_API_SPEC, or the working tree of the contract.
const specFile = process.env.KINGRES_API_SPEC ?? join(root, '../api/openapi.yaml')

describe('generated API files', () => {
  it('are up to date with kingres/api/openapi.yaml (run `npm run api:generate` if not)', async () => {
    const stale = (await staleFiles()) as string[]
    expect(stale.map((f) => relative(root, f))).toEqual([])
  })

  it('cover every operation of the contract (the count comes from the spec, not from this file)', () => {
    const spec = parse(readFileSync(specFile, 'utf8')) as {
      info: { version: string }
      paths: Record<string, Record<string, { operationId?: string }>>
    }
    const ids = Object.values(spec.paths).flatMap((item) =>
      Object.values(item).flatMap((op) => (op?.operationId ? [op.operationId] : [])),
    )
    expect(ids.length).toBeGreaterThan(0)
    expect(Object.keys(operations).sort()).toEqual([...ids].sort())
  })

  it('knows the closed list of audit actions of the contract', () => {
    const spec = parse(readFileSync(specFile, 'utf8')) as {
      components: { schemas: { AuditAction: { enum: string[] } } }
    }
    expect([...auditActions]).toEqual(spec.components.schemas.AuditAction.enum)
  })
})

describe('mock API', () => {
  it('has a handler for every operation in the contract, on the contract path and method', () => {
    const handled = new Set(
      createHandlers().map(
        (h) => `${h.info.method} ${String(h.info.path).replace(/^\*\/api\/v1/, '')}`,
      ),
    )
    const missing = Object.entries(operations)
      .map(([id, op]) => ({ id, key: `${op.method} ${op.path.replace(/\{(\w+)\}/g, ':$1')}` }))
      .filter(({ key }) => !handled.has(key))
      .map(({ id }) => id)
    expect(missing).toEqual([])
  })
})

describe('source headers', () => {
  const SOURCE = /\.(ts|vue|css|mjs|js|html)$/
  function walk(dir: string, out: string[] = []): string[] {
    for (const name of readdirSync(dir)) {
      if (['node_modules', 'dist', 'coverage'].includes(name)) continue
      const path = join(dir, name)
      if (statSync(path).isDirectory()) walk(path, out)
      else if (SOURCE.test(name)) out.push(path)
    }
    return out
  }

  it('start with an SPDX-License-Identifier: PostgreSQL comment', () => {
    const files = [
      ...walk(join(root, 'src')),
      ...walk(join(root, 'tests')),
      ...walk(join(root, 'scripts')),
      join(root, 'vite.config.ts'),
      join(root, 'eslint.config.js'),
      join(root, 'index.html'),
    ]
    const missing = files
      .filter(
        (f) =>
          !readFileSync(f, 'utf8')
            .split('\n')
            .slice(0, 3)
            .join('\n')
            .includes('SPDX-License-Identifier: PostgreSQL'),
      )
      .map((f) => relative(root, f))
    expect(missing).toEqual([])
  })
})
