// SPDX-License-Identifier: AGPL-3.0-only
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
//   ?config=, ?clock=            states of contract 0.8.0 (configuration and clocks; see switches.ts and the README)
//   ?failover=                   0.10.0: the console of the failover (see switches.ts and the README)
//   ?silent=, ?lease=, ?standby=, ?workbench=busy, ?leader=lost, ?client=on, ?ledger=   states of contract 0.7.0
//                                (see switches.ts and the README)
import { setupWorker } from 'msw/browser'
import type { OperationId } from '@/api/operations.generated'
import { operations } from '@/api/operations.generated'
import { createHandlers } from './handlers'
import { getState, resetMockState } from './http'
import { HOSTILE_MISSING, seedHostile } from './hostile'
import {
  loseLease,
  mockConfig,
  regainLease,
  reviveServer,
  silenceServer,
  type FailoverReadinessState,
} from './state'
import { resetAutoFailoverCounter, resetFailoverState } from './failover'
import { applySwitches } from './switches'
import {
  bringConfigServerBack,
  failNextApply,
  setClockOffset,
  setClockReachable,
  takeConfigServerDown,
} from './config'

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
    // 0.10.0 (failover): a server stops answering for longer than the failure timeout / answers again; what a group's
    // card says about failover (possible, possibleLoss, unknownLoss, noStandby, noneReady, noMajority, versionsDiffer;
    // null clears it); the counter of automatic failovers back to zero.
    failServer: (id: string) =>
      silenceServer(getState(), id, Date.now() - mockConfig.failureMs - 1000),
    reviveServer: (id: string) => reviveServer(getState(), id),
    setReadiness: (group: number, state: FailoverReadinessState | null) => {
      if (state === null) delete mockConfig.failoverReadiness[group]
      else mockConfig.failoverReadiness[group] = state
    },
    resetFailover: () => resetFailoverState(getState()),
    // 0.10.0 (console part 2): the values of `?failover=` while the page is open (`limit`, `count:1:3`, `off`, `auto:10.0.0.12`).
    failoverSwitch: (text: string) =>
      applySwitches(new URLSearchParams({ failover: text }), getState()),
    resetCounter: () => {
      resetAutoFailoverCounter(getState())
    },
    loseLease: (group: number) => loseLease(getState(), group),
    regainLease: (group: number) => regainLease(getState(), group),
    // 0.8.0 (configuration): the next change fails on a server; a server cannot be reached / is back; a clock is off.
    failConfigOn: (id: string, text?: string) => failNextApply(getState(), id, text),
    configServerDown: (id: string) => takeConfigServerDown(getState(), id),
    configServerBack: (id: string) => bringConfigServerBack(getState(), id),
    clockOff: (id: string, ms: number) => setClockOffset(getState(), id, ms),
    clockUnreachable: (id: string, gone = true) => setClockReachable(getState(), id, gone),
  }
  const worker = setupWorker(...createHandlers())
  await worker.start({ onUnhandledFrame: 'bypass', quiet: true })
  console.info(
    '[kingres] Mock API active. Accounts: admin, ops, viewer, metrics; password "demo-password".',
  )
}
