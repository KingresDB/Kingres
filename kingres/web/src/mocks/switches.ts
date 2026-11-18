// SPDX-License-Identifier: AGPL-3.0-only
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
//   ?config=failed:<server>       0.8.0: the next configuration change fails on that server (a retry works).
//   ?config=waiting:<server>      the server cannot be reached for a configuration change: the task waits for it.
//   ?config=restart-conflict      a restart is pending that mixes a setting going up and one going down.
//   ?commit=none | hostile | <text>   0.8.0: the server reports no commit (the footer links to the repository) / text that is not a commit id / that text.
//   ?rebalance=refuse:<code>      0.9.0: the next start answers 409 with that code (nothing_to_rebalance, cluster_inconsistent,
//                                 version_mismatch, failover_in_progress, no_receiving_server, operation_in_progress ...).
//   ?rebalance=fail               0.9.0: the next run ends failed: one bucket is given up after three attempts.
//   ?rebalance=slow               0.9.0: moves take about three times as long (time to stop one in the middle).
//   ?failover=dataloss            0.10.0: the plan of a hard failover says writes may be lost: the request is refused with 409
//                                 failover_needs_decision until it says allowDataLoss (a dry run shows the plan).
//   ?failover=<a>,<b>,...         0.10.0 (the console of the failover; the values can be combined with a comma):
//                                 needs-decision   the task pauses at "Choose the standby" until a repeat of the request says allowDataLoss
//                                 blocked:rebalance | blocked:lease | blocked:quorum | blocked:noreplica   the refusal (and the readiness blocker)
//                                 fail:<step>[:<group>]   the task ends failed at that step (check, freeze, stop, lease, choose, handover,
//                                                         promote, catalog, repoint, lift, verify, pause, report)
//                                 slow             steps take about three times as long (time to cancel and to see the phases)
//                                 map              the map group can be failed over (the real server refuses it for now): the bootstrap, no task id at first
//                                 graceful         the planned switchover is accepted (the real server refuses it for now)
//                                 old:rebuilding | old:waiting | old:running | old:failed   the state of the old primary in the report
//                                 auto:<server>    an automatic failover of that server is under way (trigger automatic)
//                                 limit            the counter of automatic failovers is at its maximum
//                                 switchover:behind  the verification of a switchover fails: rolled back, standby_behind
//   ?clock=off:<server>:<ms>      the clock of that server is that many milliseconds ahead (negative: behind).
//   ?clock=unreachable:<server>   the clock of that server cannot be read.
import type { MockState } from './state'
import { loseLease, mockConfig, silenceServer, stallStandby } from './state'
import { FAIL_STEPS, seedAutomaticFailover } from './failover'
import {
  failNextApply,
  seedRestartConflict,
  setClockOffset,
  setClockReachable,
  takeConfigServerDown,
} from './config'

/** One value of `?failover=` (several: comma). Unknown values are ignored. */
function applyFailoverSwitch(entry: string, s: MockState): void {
  if (entry === 'dataloss') mockConfig.failoverDataLoss = true
  if (entry === 'needs-decision') mockConfig.failoverNeedsDecision = true
  if (entry === 'slow') mockConfig.failoverSlow = true
  if (entry === 'map') mockConfig.failoverMap = true
  if (entry === 'graceful') mockConfig.failoverGraceful = true
  const blocked = /^blocked:(rebalance|lease|quorum|noreplica)$/.exec(entry)
  if (blocked) mockConfig.failoverBlock = blocked[1] as NonNullable<typeof mockConfig.failoverBlock>
  const fail = /^fail:([a-z]+)(?::(\d+))?$/.exec(entry)
  if (fail && FAIL_STEPS.includes(fail[1] as string))
    mockConfig.failoverFailStep = {
      step: fail[1] as string,
      group: fail[2] ? Number(fail[2]) : null,
    }
  const old = /^old:(rebuilding|waiting|running|failed)$/.exec(entry)
  if (old) mockConfig.failoverOld = old[1] as NonNullable<typeof mockConfig.failoverOld>
  if (entry === 'switchover:behind') {
    mockConfig.failoverGraceful = true
    mockConfig.failoverSwitchoverBehind = true
  }
  if (entry === 'limit')
    s.settings.autoFailover = {
      ...s.settings.autoFailover,
      eventsSinceReset: s.settings.autoFailover.maxCount,
      lastEventAt: new Date(Date.now() - 600_000).toISOString(),
    }
  const auto = /^auto:(.+)$/.exec(entry)
  if (auto) seedAutomaticFailover(s, auto[1] as string)
}

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
  const commit = params.get('commit')
  if (commit === 'none') mockConfig.commit = null
  else if (commit === 'hostile') mockConfig.commit = 'abc"><script>x</script>'
  else if (commit) mockConfig.commit = commit
  if (params.get('workbench') === 'busy') mockConfig.workbenchBusy = true
  if (params.get('leader') === 'lost') mockConfig.leaderLost = true
  const client = params.get('client')
  if (client === 'on' || client === 'standby') mockConfig.clientEntryPoint = client
  const ledger = params.get('ledger')
  if (ledger === 'all' || ledger === 'absent') mockConfig.ledgerStatusCoverage = ledger
  for (const entry of (params.get('config') ?? '').split(',')) {
    const m = /^(failed|waiting):(.+)$/.exec(entry)
    if (m && m[1] === 'failed') failNextApply(s, m[2] as string)
    if (m && m[1] === 'waiting') takeConfigServerDown(s, m[2] as string)
    if (entry === 'restart-conflict') seedRestartConflict(s)
  }
  for (const entry of (params.get('rebalance') ?? '').split(',')) {
    const refuse = /^refuse:([a-z_]+)$/.exec(entry)
    if (refuse) mockConfig.rebalanceRefuse = refuse[1] as string
    if (entry === 'fail') mockConfig.rebalanceFail = true
    if (entry === 'slow') mockConfig.rebalanceSlow = true
  }
  for (const entry of (params.get('failover') ?? '').split(',')) applyFailoverSwitch(entry, s)
  for (const entry of (params.get('clock') ?? '').split(',')) {
    const off = /^off:([^:]+):(-?\d+(?:\.\d+)?)$/.exec(entry)
    if (off) setClockOffset(s, off[1] as string, Number(off[2]))
    const gone = /^unreachable:(.+)$/.exec(entry)
    if (gone) setClockReachable(s, gone[1] as string, false)
  }
}
