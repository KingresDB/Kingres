// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// The README section "What the server must send" holds exactly the header values that the browser
// test serves the production build with, so the server's engineer implements what was tested.
import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import {
  CACHE_HEADERS,
  COMMON_HEADERS,
  CONTENT_SECURITY_POLICY,
  SESSION_COOKIE,
} from '../browser-tests/server-headers'

const readme = readFileSync('README.md', 'utf8')
const section = readme.slice(readme.indexOf('## What the server must send'))

describe('README: What the server must send', () => {
  it('has the exact line of every header', () => {
    for (const [name, value] of Object.entries(COMMON_HEADERS)) {
      expect(section, name).toContain(`${name}: ${value}`)
    }
  })

  it('has the content security policy, which allows no inline script, style or eval', () => {
    expect(section).toContain(CONTENT_SECURITY_POLICY)
    expect(CONTENT_SECURITY_POLICY).not.toMatch(/unsafe-inline|unsafe-eval|\*|data:/)
    expect(CONTENT_SECURITY_POLICY).toContain("frame-ancestors 'none'")
  })

  it('has the caching headers and the cookie attributes', () => {
    for (const value of Object.values(CACHE_HEADERS)) expect(section).toContain(value)
    expect(section).toContain('HttpOnly; Secure; SameSite=Strict; Path=/')
    expect(SESSION_COOKIE).toContain('SameSite=Strict')
    expect(section).toMatch(/\/api\/.*never HTML/s)
  })
})
