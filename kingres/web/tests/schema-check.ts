// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// A check of a value against a schema of the contract (the subset of JSON Schema the contract uses), so that a test can say
// that what the mock answers is what the YAML says.
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect } from 'vitest'
import { parse } from 'yaml'

const specFile = process.env.KINGRES_API_SPEC ?? join(process.cwd(), '../api/openapi.yaml')

export type Schema = {
  $ref?: string
  type?: string | string[]
  enum?: unknown[]
  required?: string[]
  properties?: Record<string, Schema>
  items?: Schema
  allOf?: Schema[]
  oneOf?: Schema[]
  minimum?: number
  minItems?: number
}
export const spec = parse(readFileSync(specFile, 'utf8')) as {
  components: { schemas: Record<string, Schema> }
}

/** Why `value` does not satisfy `schema` (the subset of JSON Schema the contract uses), or null. */
export function violation(value: unknown, schema: Schema, at = '$'): string | null {
  if (schema.$ref) {
    const name = schema.$ref.replace('#/components/schemas/', '')
    return violation(value, spec.components.schemas[name] as Schema, at)
  }
  for (const part of schema.allOf ?? []) {
    const v = violation(value, part, at)
    if (v) return v
  }
  if (schema.oneOf) {
    const passing = schema.oneOf.filter((s) => violation(value, s, at) === null).length
    if (passing !== 1) return `${at}: matches ${passing} of the oneOf branches`
  }
  const types =
    schema.type === undefined ? [] : Array.isArray(schema.type) ? schema.type : [schema.type]
  if (types.length > 0) {
    const actual =
      value === null
        ? 'null'
        : Array.isArray(value)
          ? 'array'
          : Number.isInteger(value)
            ? 'integer'
            : typeof value
    const fits = types.some((t) => t === actual || (t === 'number' && actual === 'integer'))
    if (!fits) return `${at}: is ${actual}, the contract says ${types.join('|')}`
  }
  if (schema.enum && !schema.enum.includes(value))
    return `${at}: ${String(value)} is not in the enum`
  if (typeof value === 'number' && schema.minimum !== undefined && value < schema.minimum)
    return `${at}: below the minimum`
  if (Array.isArray(value)) {
    if (schema.minItems !== undefined && value.length < schema.minItems) return `${at}: too short`
    for (const [i, item] of value.entries()) {
      const v = schema.items ? violation(item, schema.items, `${at}[${i}]`) : null
      if (v) return v
    }
  }
  if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
    const o = value as Record<string, unknown>
    for (const key of schema.required ?? []) if (!(key in o)) return `${at}: missing ${key}`
    for (const [key, sub] of Object.entries(schema.properties ?? {}))
      if (key in o) {
        const v = violation(o[key], sub, `${at}.${key}`)
        if (v) return v
      }
  }
  return null
}

/** Fails the test when `value` does not satisfy the named schema of the contract. */
export const ok = (value: unknown, schema: string) =>
  expect(violation(value, { $ref: `#/components/schemas/${schema}` })).toBeNull()
