// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Starts the mock API in the browser (development only; see main.ts). Dev switches in the URL:
//   ?mock=uninitialized          start as a fresh server, so the setup wizard is shown
//   ?unsupported=op1,op2         answer those operations with `unsupported_operation`
//   ?mock=hostile                add names and messages as a hostile database user could make them
//   ?latency=0                   no artificial delay
import { setupWorker } from 'msw/browser'
import type { OperationId } from '@/api/operations.generated'
import { operations } from '@/api/operations.generated'
import { createHandlers } from './handlers'
import { getState, resetMockState } from './http'
import { seedHostile } from './hostile'
import { mockConfig } from './state'

export async function startBrowserMocks(): Promise<void> {
  const params = new URLSearchParams(location.search)
  resetMockState({ uninitialized: params.get('mock') === 'uninitialized' })
  if (params.get('mock') === 'hostile') seedHostile(getState())
  for (const op of (params.get('unsupported') ?? '').split(',')) {
    if (op in operations) mockConfig.unsupported.add(op as OperationId)
  }
  mockConfig.latencyMs = Number(params.get('latency') ?? 150)
  const worker = setupWorker(...createHandlers())
  await worker.start({ onUnhandledFrame: 'bypass', quiet: true })
  console.info(
    '[kingres] Mock API active. Accounts: admin, ops, viewer, metrics; password "demo-password".',
  )
}
