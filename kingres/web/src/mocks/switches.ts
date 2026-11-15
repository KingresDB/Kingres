// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// The URL switches of the mock API for the states of contract 0.7.0 that are hard to reach by hand (development
// only; the tests call `applySwitches` with the same strings). The README lists them.
//
//   ?silent=10.0.0.12             the server stops sending heartbeats when the page loads: suspect after 5 s, dead after
//                                 10 s. `10.0.0.12:suspect` and `10.0.0.12:dead` start in that state. Several: comma.
//   ?lease=lost:1                 the primary of shard group 1 (it must run on the answering server, 10.0.0.11) lost its
//                                 lease; `lost:1,lost:0` for several; `absent`: the server reports no leases at all.
//   ?standby=stalled:2@10.0.0.13  the standby of group 2 on that server does not stream; `:past` at the end puts the
//                                 stop beyond the grace period (the primary no longer waits, the alert is raised).
//   ?workbench=busy               every workbench statement answers 429 workbench_busy with Retry-After.
//   ?leader=lost                  this server has not heard the manager leader: 503 leader_unavailable for requests
//                                 that need a session check.
//   ?client=on | ?client=standby  the client entry point is on (and the standby entry point too): its metrics exist.
//   ?ledger=all | ?ledger=absent  the ledger status covers every shard group / carries no coverage (older server).
import type { MockState } from './state'
import { loseLease, mockConfig, silenceServer, stallStandby } from './state'

/** Applies the 0.7.0 switches of a query string to the mock. Unknown values are ignored. */
export function applySwitches(params: URLSearchParams, s: MockState): void {
  for (const entry of (params.get('silent') ?? '').split(',')) {
    const [id, how] = entry.split(':')
    if (!id) continue
    const age =
      how === 'dead'
        ? mockConfig.failureMs + 1000
        : how === 'suspect'
          ? mockConfig.suspectMs + 1000
          : 0
    silenceServer(s, id, Date.now() - age)
  }
  for (const entry of (params.get('lease') ?? '').split(',')) {
    if (entry === 'absent') mockConfig.leaseAbsent = true
    const m = /^lost:(\d+)$/.exec(entry)
    if (m) loseLease(s, Number(m[1]))
  }
  for (const entry of (params.get('standby') ?? '').split(',')) {
    const m = /^stalled:(\d+)@([^:]+)(:past)?$/.exec(entry)
    if (m)
      stallStandby(
        s,
        Number(m[1]),
        m[2] as string,
        Date.now() - (m[3] ? mockConfig.syncGraceMs + 1000 : 0),
      )
  }
  if (params.get('workbench') === 'busy') mockConfig.workbenchBusy = true
  if (params.get('leader') === 'lost') mockConfig.leaderLost = true
  const client = params.get('client')
  if (client === 'on' || client === 'standby') mockConfig.clientEntryPoint = client
  const ledger = params.get('ledger')
  if (ledger === 'all' || ledger === 'absent') mockConfig.ledgerStatusCoverage = ledger
}
