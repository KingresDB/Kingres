// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The `redirect` query parameter of the sign-in page. Only a path inside the panel is followed.

const SAFE_ORIGIN = 'http://panel.invalid'
/** A backslash or a control character. */
// eslint-disable-next-line no-control-regex
const UNSAFE = /[\\\u0000-\u001f\u007f]/

/** Returns `value` when it is a path inside this application, otherwise `/`. */
export function safeRedirect(value: unknown): string {
  if (typeof value !== 'string' || value === '') return '/'
  // One leading slash only; no backslash (browsers treat it like a slash), no control character,
  // also not written as a percent escape.
  if (!value.startsWith('/') || value.startsWith('//')) return '/'
  let decoded: string
  try {
    decoded = decodeURIComponent(value)
  } catch {
    return '/'
  }
  if (UNSAFE.test(value) || UNSAFE.test(decoded)) return '/'
  if (decoded.startsWith('//')) return '/'
  try {
    if (new URL(value, SAFE_ORIGIN).origin !== SAFE_ORIGIN) return '/'
  } catch {
    return '/'
  }
  return value
}
