// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// The response headers the server must send with the panel. The README section "What the server
// must send" lists exactly these values; `tests/server-headers.test.ts` fails when they differ, and
// the browser tests serve the production build with them and fail on any policy violation.

/** Content-Security-Policy of `index.html` and of the files under `/assets/`. */
export const CONTENT_SECURITY_POLICY = [
  "default-src 'self'",
  "script-src 'self'",
  "style-src 'self'",
  "img-src 'self'",
  "connect-src 'self'",
  "font-src 'none'",
  "object-src 'none'",
  "base-uri 'none'",
  "form-action 'self'",
  "frame-ancestors 'none'",
].join('; ')

/** Headers for every response of the panel (static files and API). */
export const COMMON_HEADERS: Record<string, string> = {
  'Content-Security-Policy': CONTENT_SECURITY_POLICY,
  'X-Frame-Options': 'DENY',
  'X-Content-Type-Options': 'nosniff',
  'Referrer-Policy': 'no-referrer',
  'Cross-Origin-Opener-Policy': 'same-origin',
  'Permissions-Policy': 'camera=(), microphone=(), geolocation=()',
}

/** Caching headers by kind of response. */
export const CACHE_HEADERS = {
  indexHtml: 'no-cache',
  hashedAsset: 'public, max-age=31536000, immutable',
  api: 'no-store',
} as const

/** The attributes of the session cookie. */
export const SESSION_COOKIE = 'kingres_session=<value>; HttpOnly; Secure; SameSite=Strict; Path=/'
