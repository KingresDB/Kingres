// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
import '@testing-library/jest-dom/vitest'
import { afterAll, afterEach, beforeAll, beforeEach, expect } from 'vitest'
import * as axeMatchers from 'vitest-axe/matchers'
import { cleanup, configure } from '@testing-library/vue'
import { mockServer } from '@/mocks/server'
import { resetMockState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { taskPolling } from '@/composables/useTask'
import { setCsrfToken, setSetupToken } from '@/api/client'

expect.extend(axeMatchers)
// Pages are lazy-loaded and transformed on first use; give async queries room on a busy machine.
configure({ asyncUtilTimeout: 5000 })

beforeAll(() => mockServer.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => {
  resetMockState()
  mockConfig.taskDurationMs = 60
  mockConfig.latencyMs = 0
  taskPolling.intervalMs = 25
  setCsrfToken(null)
  setSetupToken(null)
  if (typeof localStorage !== 'undefined') localStorage.clear()
})
afterEach(() => {
  cleanup()
  mockServer.resetHandlers()
})
afterAll(() => mockServer.close())
