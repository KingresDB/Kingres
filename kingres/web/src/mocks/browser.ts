// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Starts the mock API in the browser (development only; see main.ts). Dev switches in the URL:
//   ?mock=uninitialized          start as a fresh server, so the setup wizard is shown
//   ?unsupported=op1,op2         answer those operations with `unsupported_operation`
import { setupWorker } from 'msw/browser'
import type { OperationId } from '@/api/operations.generated'
import { operations } from '@/api/operations.generated'
import { createHandlers } from './handlers'
import { resetMockState } from './http'
import { mockConfig } from './state'

export async function startBrowserMocks(): Promise<void> {
  const params = new URLSearchParams(location.search)
  resetMockState({ uninitialized: params.get('mock') === 'uninitialized' })
  for (const op of (params.get('unsupported') ?? '').split(',')) {
    if (op in operations) mockConfig.unsupported.add(op as OperationId)
  }
  mockConfig.latencyMs = 150
  const worker = setupWorker(...createHandlers())
  await worker.start({ onUnhandledFrame: 'bypass', quiet: true })
  console.info(
    '[kingres] Mock API active. Accounts: admin, ops, viewer, metrics; password "demo-password".',
  )
}
