// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Client-side checks that mirror the constraints in the OpenAPI contract, so users get an answer
// before the request is sent. The server stays the authority: its field errors are shown as well.

/** PostgreSQL-style identifier used for databases and roles (contract pattern). */
export const IDENTIFIER_RE = /^[a-z_][a-z0-9_]{0,62}$/
export const CLUSTER_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$/
/** host:port of a server's panel/REST port (contract pattern `NodeAddress`). */
export const NODE_ADDRESS_RE = /^(\[[0-9a-fA-F:]+\]|[A-Za-z0-9.-]+):[0-9]{1,5}$/

export type Validator<T> = (value: T) => string | undefined

export function identifier(label: string): Validator<string> {
  return (v) =>
    IDENTIFIER_RE.test(v)
      ? undefined
      : `${label} must use lower-case letters, digits and "_", start with a letter or "_", and have at most 63 characters.`
}

export function nodeAddress(): Validator<string> {
  return (v) =>
    NODE_ADDRESS_RE.test(v) &&
    Number(v.slice(v.lastIndexOf(':') + 1)) >= 1 &&
    Number(v.slice(v.lastIndexOf(':') + 1)) <= 65535
      ? undefined
      : 'Use host:port of the server panel port, for example 10.0.0.14:8091.'
}

export function required(label: string): Validator<string> {
  return (v) => (v.trim() === '' ? `${label} is required.` : undefined)
}

export function integerBetween(label: string, min: number, max: number): Validator<number | null> {
  return (v) =>
    v === null || !Number.isInteger(v) || v < min || v > max
      ? `${label} must be a whole number between ${min} and ${max}.`
      : undefined
}

export function absolutePath(label: string): Validator<string> {
  return (v) =>
    v.startsWith('/') || /^[A-Za-z]:[\\/]/.test(v)
      ? undefined
      : `${label} must be an absolute path.`
}

export function minLength(label: string, n: number): Validator<string> {
  return (v) => (v.length >= n ? undefined : `${label} must have at least ${n} characters.`)
}

/** Runs validators in order and returns the first message. */
export function firstError<T>(value: T, ...validators: Validator<T>[]): string | undefined {
  for (const check of validators) {
    const message = check(value)
    if (message) return message
  }
  return undefined
}

/** Removes empty entries so an error map can be tested with `Object.keys(...).length`. */
export function compact(errors: Record<string, string | undefined>): Record<string, string> {
  return Object.fromEntries(Object.entries(errors).filter((e): e is [string, string] => !!e[1]))
}
