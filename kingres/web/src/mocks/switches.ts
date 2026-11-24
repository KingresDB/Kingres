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
//   ?phase7=today                 0.11.0: the 21 ledger operations that kingresd does not build yet answer 422 unsupported_operation, as it
//                                 does today (only settings, signing and the trust file work). Without it the mock answers all 28.
//   ?signing=<a>,<b>,...          0.11.0 (the signing keys; combinable with a comma):
//                                 none            no key exists: the origin prefix is not locked and publication is off
//                                 rotating        two keys are active (the overlap of a rotation)
//                                 revoked         a key was revoked (compromised since a time) and a third one signs
//                                 plain           the key files are not sealed with a passphrase
//                                 backup          a backup of the key files is advised
//                                 alarm           this server cannot sign (a damaged key set): alert ledgerSignerUnavailable
//                                 discontinuity   the key ring was replaced after the loss of every key
//                                 command         the signing mode is an external signer (command)
//                                 fail:<code>     the next key task ends failed with that code (key_files_remain, passphrase_mismatch, ...)
//   ?publication=<a>,<b>,...      0.11.0: off | plain (the listener has no TLS) | nolistener | behind (the newest checkpoint is old)
//                                 | unread (a group could not be read: a stale member) | diverged (the chain of group 2 of shop diverged)
//   ?anchors=all|disabled|testfail|testwarn|refuse:<code>   0.11.0 (console part B): see the README
//   ?anchors=<a>,<b>,...          0.11.0: none (alert ledgerNotAnchored) | required (the same, a warning) | ok | behind | failed
//                                 (a WORM target "archive-worm" exists, with the newest aggregate anchored in time, late or not)
//   ?analytics=<a>,<b>,...        0.18.0 (phase 12; combinable with a comma): on (approximate aggregates are on in shop) | timeseries (in shop: the table
//                                 clicks that can be converted, and metrics, partitioned by day, with a policy, its runs and columnar partitions)
//                                 | ahead-low (the same, with one day ahead and the alert partitionsAheadLow) | mixed (after timeseries: one partition of metrics is
//                                 half converted, accessMethod mixed, contract 0.25.0) | sketch:missing|mismatch|hash
//                                 | columnar:missing | timepart:missing | convert:fail | run:lock|name|fail | publication
//                                 | old (the 11 new operations answer 422 unsupported_operation)
//   ?transfer=<a>,<b>,...         0.19.0 (the data transfer; combinable with a comma):
//                                 off          the cluster setting is off: every transfer operation answers 403 data_transfer_disabled
//                                 insecure     every transfer operation answers 403 insecure_transport
//                                 full         the staging area has no room: 409 transfer_quota_exceeded
//                                 small        the chunk size the server accepts is 128 KiB (a long upload in a short time)
//                                 session      the next chunk of an upload is answered 401 (the session ended in the middle)
//                                 cut          the next chunk is cut after 100 KiB; the next download breaks after 1 KiB
//                                 busy         the next two chunks are answered 409 with the offset unchanged (another request still writes)
//                                 cut400       the next chunk is cut and answered 400 with Upload-Offset
//                                 short        the next download ends early, without an error
//                                 exports      three exports exist: ready, building and expired
//                                 lobs         0.29.0: the database has large objects (export_database: 422 large_objects_present)
//                                 unreadable   0.29.0: the role cannot read some tables (422 export_not_readable)
//                                 dbfail       0.29.0: the next database export fails in its data step
//                                 changed      0.29.0: the next database export fails with schema_changed
//   ?clock=off:<server>:<ms>      the clock of that server is that many milliseconds ahead (negative: behind).
//   ?clock=unreachable:<server>   the clock of that server cannot be read.
import { seedPartitioned } from './tables'
import { applyAnalyticsSwitch } from './analytics'
import type { MockState } from './state'
import { applyTransferSwitch } from './transfer-switches'
import { loseLease, mockConfig, silenceServer, stallStandby } from './state'
import { FAIL_STEPS, raiseAutoFailoverLimit, seedAutomaticFailover } from './failover'
import {
  answerAsTheServerDoesToday,
  seedAnchors,
  seedAnchorsAll,
  seedAnchorsDisabled,
  seedClientKeys,
} from './ledger'
import { signingKey, withoutKeys } from './ledger-data'
import {
  failNextApply,
  seedRestartConflict,
  setClockOffset,
  setClockReachable,
  takeConfigServerDown,
} from './config'

/** One value of `?failover=` (several: comma). Unknown values are ignored. */
/**
 * `?tables=` (0.14.0, increment 9.7): `partitions` adds a distributed table that the user partitioned by range (with three partitions) and a
 * table that can be attached to it; `fail:space` and `fail:copy` make the next changes of a table fail; `slow` makes them take a minute;
 * `busy` says that another operation holds the colocation groups; `blockme` makes the next change wait (paused) at its switch for a
 * transaction that holds the table, `diffme` makes it end with `verification_failed`, `pending` leaves it `pending`, and `undo:<ms>` or
 * `undo:fail` set how long the undo of a cancelled task takes to be reported and whether one server could not be reached; `fk:<table>` gives a table a foreign key to an ordinary table.
 */
function applyTablesSwitch(entry: string, s: MockState): void {
  if (entry === 'partitions') seedPartitioned(s)
  if (entry === 'fail:space') mockConfig.tableFail = 'space'
  if (entry === 'fail:copy') mockConfig.tableFail = 'copy'
  if (entry === 'slow') mockConfig.tableSlow = true
  if (entry === 'busy') mockConfig.tableBusy = true
  if (entry === 'blockme') mockConfig.tableBlocked = true
  if (entry === 'diffme') mockConfig.tableVerifyFails = true
  if (entry === 'pending') mockConfig.tablePending = true
  if (entry === 'undo:fail') mockConfig.tableUndoFails = true
  const undo = /^undo:(d{1,6})$/.exec(entry)
  if (undo) mockConfig.tableUndoMs = Number(undo[1])
  const fk = /^fk:(.+)$/.exec(entry)
  if (fk) mockConfig.tableForeignKeys.push(fk[1] as string)
}

function applyFailoverSwitch(entry: string, s: MockState): void {
  if (entry === 'dataloss') mockConfig.failoverDataLoss = true
  if (entry === 'needs-decision') mockConfig.failoverNeedsDecision = true
  if (entry === 'slow') mockConfig.failoverSlow = true
  if (entry === 'warnings') mockConfig.failoverWarnings = true
  if (entry === 'noplan') mockConfig.failoverNoPlan = true
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
  if (entry === 'limit') raiseAutoFailoverLimit(s)
  // 0.10.0 (console part 2): automatic failover turned off; the counter at n of m (the limit when n reaches m).
  if (entry === 'off') s.settings.autoFailover = { ...s.settings.autoFailover, enabled: false }
  const count = /^count:(\d{1,3})(?::(\d{1,3}))?$/.exec(entry)
  if (count) {
    const max = Math.max(1, Number(count[2] ?? s.settings.autoFailover.maxCount))
    const events = Number(count[1])
    s.settings.autoFailover = {
      ...s.settings.autoFailover,
      maxCount: max,
      eventsSinceReset: events,
      lastEventAt: events > 0 ? new Date(Date.now() - 3_600_000).toISOString() : null,
    }
    if (events >= max) raiseAutoFailoverLimit(s)
  }
  const auto = /^auto:(.+)$/.exec(entry)
  if (auto) seedAutomaticFailover(s, auto[1] as string)
}

/** The messages of the task errors that a key task can end with (design 14, 127 and 131). */
const KEY_FAILURE: Record<string, string> = {
  key_files_remain:
    'The key file could not be removed from server 10.0.0.12. Nothing is lost; try again when the server answers.',
  passphrase_mismatch: 'Server 10.0.0.12 cannot open the key file with its passphrase.',
  signer_unavailable: 'The signer does not answer.',
  term_over: 'Another manager leader took over; try again.',
}

/** One value of `?signing=`. Unknown values are ignored. */
function applySigningSwitch(entry: string, s: MockState): void {
  const l = s.ledger7
  if (entry === 'none') withoutKeys(l)
  if (entry === 'rotating') l.signing.keys.push(signingKey('rotated', 'active', 2 * 86_400_000))
  if (entry === 'revoked') {
    const stolen = l.signing.keys.find((k) => k.state === 'active')
    if (stolen) {
      stolen.state = 'revoked'
      stolen.revokedAt = new Date(Date.now() - 86_400_000).toISOString()
      stolen.compromisedSince = new Date(Date.now() - 9 * 86_400_000).toISOString()
    }
    l.signing.keys.push(signingKey('third', 'active', 86_400_000))
    l.signing.events.push(
      {
        seq: 3,
        type: 'key-added',
        time: `${new Date(Date.now() - 86_400_000).toISOString().slice(0, 19)}Z`,
      },
      {
        seq: 4,
        type: 'key-revoked',
        time: `${new Date(Date.now() - 86_000_000).toISOString().slice(0, 19)}Z`,
      },
    )
    l.signing.backupAdvised = true
  }
  if (entry === 'plain') l.signing.passphraseProtected = false
  if (entry === 'backup') l.signing.backupAdvised = true
  if (entry === 'alarm')
    l.signing.signerAlarm = 'The key file of the active key is missing on this server.'
  if (entry === 'discontinuity') l.signing.discontinuity = true
  // 0.12.0: the system database was restored from a backup older than what was published.
  if (entry === 'restored') l.publishedStateAlarm = true
  if (entry === 'command') l.signing.mode = 'command'
  const fail = /^fail:([a-z_]+)$/.exec(entry)
  if (fail)
    l.keyFailure = {
      code: fail[1] as string,
      message: KEY_FAILURE[fail[1] as string] ?? 'The task failed.',
    }
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
  for (const entry of (params.get('tables') ?? '').split(',')) applyTablesSwitch(entry, s)
  for (const entry of (params.get('analytics') ?? '').split(',')) applyAnalyticsSwitch(entry, s)
  if (params.get('phase7') === 'today') answerAsTheServerDoesToday(s)
  for (const entry of (params.get('signing') ?? '').split(',')) applySigningSwitch(entry, s)
  for (const entry of (params.get('publication') ?? '').split(',')) {
    const l = s.ledger7
    if (entry === 'off') l.settings.publication.enabled = false
    if (entry === 'plain') l.publication.listener.tls = false
    if (entry === 'nolistener')
      l.publication.listener = { address: null, tls: false, running: false }
    if (entry === 'behind') l.publication.health = 'behind'
    if (entry === 'unread') l.publication.health = 'unread'
    if (entry === 'diverged') l.diverged.push('shop:2')
    // A log that was closed and answers again: diverged, with the height and why the log ended in the text of the alert.
    if (entry === 'reopened') {
      l.diverged.push('shop:2')
      l.divergedWhy =
        'the log of an earlier epoch was closed at height 120, and the chain answers again at height 121'
    }
  }
  for (const entry of (params.get('anchors') ?? '').split(',')) {
    const l = s.ledger7
    if (entry === 'none' || entry === 'required') l.alertNotAnchored = true
    if (entry === 'required') l.settings.anchor.required = true
    if (entry === 'ok' || entry === 'behind' || entry === 'failed') seedAnchors(s, entry)
    // 0.11.0 (console part B): the three kinds with a record in every state; a target that is turned off; what a test
    // finds; a refusal of every change.
    if (entry === 'all') seedAnchorsAll(s)
    if (entry === 'disabled') {
      seedAnchorsDisabled(s)
      l.alertNotAnchored = true
    }
    if (entry === 'testfail') mockConfig.anchorTest = 'fail'
    if (entry === 'testwarn') mockConfig.anchorTest = 'warn'
    const refuse = /^refuse:([a-z_]+)$/.exec(entry)
    if (refuse) mockConfig.anchorRefuse = refuse[1] as string
  }
  // 0.11.0 (Console C): `keys` (three client keys: any role, bound to a role, revoked), `legacy` (the extension is older than 0.3),
  // `nossl` (the server cannot attest), `refuse:<code>` (every change answers that problem code).
  for (const entry of (params.get('clientkeys') ?? '').split(',')) {
    if (entry === 'keys') seedClientKeys(s)
    if (entry === 'legacy') mockConfig.clientLegacy = true
    if (entry === 'nossl') mockConfig.clientNoSsl = true
    const refuse = /^refuse:([a-z_]+)$/.exec(entry)
    if (refuse) mockConfig.clientRefuse = refuse[1] as string
  }
  // Console D: `expired` (a bundle is gone: 410), `refuse:<code>` (a request for a bundle or an acknowledgement answers that code).
  for (const entry of (params.get('bundle') ?? '').split(',')) {
    if (entry === 'expired') mockConfig.bundleExpired = true
    const refuse = /^refuse:([a-z_]+)$/.exec(entry)
    if (refuse) mockConfig.opsRefuse = refuse[1] as string
  }
  for (const entry of (params.get('transfer') ?? '').split(',')) applyTransferSwitch(entry, s)
  for (const entry of (params.get('clock') ?? '').split(',')) {
    const off = /^off:([^:]+):(-?\d+(?:\.\d+)?)$/.exec(entry)
    if (off) setClockOffset(s, off[1] as string, Number(off[2]))
    const gone = /^unreachable:(.+)$/.exec(entry)
    if (gone) setClockReachable(s, gone[1] as string, false)
  }
}
