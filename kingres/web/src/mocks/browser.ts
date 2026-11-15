// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Starts the mock API in the browser (development only; see main.ts). Dev switches in the URL:
//   ?mock=uninitialized          start as a fresh server, so the setup wizard is shown
//   ?unsupported=op1,op2         answer those operations with `unsupported_operation`
//   ?mock=hostile                add names and messages as a hostile database user could make them
//   ?partial=10.0.0.12:timeout   logs, alerts and metrics answer as partial (id:reason, comma separated;
//                                reasons timeout, unreachable, refused, invalidAnswer)
//   ?cursors=reject              every request with a cursor answers 400 (a 0.4.0 cursor on a 0.5.0 server)
//   ?latency=0                   no artificial delay
//   ?silent=, ?lease=, ?standby=, ?workbench=busy, ?leader=lost, ?client=on, ?ledger=   states of contract 0.7.0
//                                (see switches.ts and the README)
import { setupWorker } from 'msw/browser'
import type { OperationId } from '@/api/operations.generated'
import { operations } from '@/api/operations.generated'
import { createHandlers } from './handlers'
import { getState, resetMockState } from './http'
import { HOSTILE_MISSING, seedHostile } from './hostile'
import { loseLease, mockConfig, regainLease, reviveServer, silenceServer } from './state'
import { applySwitches } from './switches'

export async function startBrowserMocks(): Promise<void> {
  const params = new URLSearchParams(location.search)
  resetMockState({ uninitialized: params.get('mock') === 'uninitialized' })
  if (params.get('mock') === 'hostile') seedHostile(getState())
  for (const op of (params.get('unsupported') ?? '').split(',')) {
    if (op in operations) mockConfig.unsupported.add(op as OperationId)
  }
  const reasons = ['timeout', 'unreachable', 'refused', 'invalidAnswer'] as const
  const missing = (params.get('partial') ?? '')
    .split(',')
    .map((entry) => entry.split(':'))
    .filter(([id, reason]) => id && reasons.includes(reason as (typeof reasons)[number]))
    .map(([id, reason]) => ({ serverId: id as string, reason: reason as (typeof reasons)[number] }))
  if (missing.length > 0) mockConfig.partial = missing
  else if (params.get('mock') === 'hostile') mockConfig.partial = HOSTILE_MISSING
  applySwitches(params, getState())
  mockConfig.rejectCursors = params.get('cursors') === 'reject'
  mockConfig.latencyMs = Number(params.get('latency') ?? 150)
  // For the browser tests and for trying states by hand: `__kingresMock.config.leaderLost = true`, `.silence('10.0.0.12')`.
  ;(window as unknown as { __kingresMock?: unknown }).__kingresMock = {
    config: mockConfig,
    state: getState,
    silence: (id: string) => silenceServer(getState(), id),
    revive: (id: string) => reviveServer(getState(), id),
    loseLease: (group: number) => loseLease(getState(), group),
    regainLease: (group: number) => regainLease(getState(), group),
  }
  const worker = setupWorker(...createHandlers())
  await worker.start({ onUnhandledFrame: 'bypass', quiet: true })
  console.info(
    '[kingres] Mock API active. Accounts: admin, ops, viewer, metrics; password "demo-password".',
  )
}
