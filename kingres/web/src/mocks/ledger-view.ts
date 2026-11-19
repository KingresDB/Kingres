// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// What the ledger of phase 7 derives from the state of the mock (contract 0.11.0): the logs of a database, the aggregate
// checkpoints, what the status says about publication and anchoring, and the alerts of the kinds of design 6.9. No
// request is handled here; `ledger.ts` does that. This file does not import `http.ts`, so `http.ts` can raise the alerts.
import type { components } from '@/api/schema'
import { dataGroups, fakeHash, mockConfig, nowIso } from './state'
import type { DbRec, MockState } from './state'
import { fakePublicKey, hex64, type Ledger7State } from './ledger-data'

type S = components['schemas']
const DAY = 86_400_000

/** The state of the ledger of phase 7. */
export const l7 = (s: MockState): Ledger7State => s.ledger7

// --- the logs of a database ----------------------------------------------------------------------

/** The latest size of the aggregate log of every database (the mock does not grow it). */
export const AGGREGATE_SIZE = 42

/** The shard groups whose chains a database has (a database that is not sharded has the chain of group 0). */
export function groupsOf(s: MockState, rec: DbRec): number[] {
  return rec.info.sharding.enabled ? dataGroups(s).map((g) => g.id) : [0]
}

/** A database that has a ledger: its chains and its aggregate exist when publication is on and a key signs. */
export function hasLedger(rec: DbRec): boolean {
  return (rec.info.ledger.mode ?? 'off') !== 'off'
}

/** Whether the aggregate of a database is published: publication on, a key in force, a ledger. */
export function published(s: MockState, rec: DbRec): boolean {
  const l = l7(s)
  return (
    l.settings.publication.enabled &&
    l.signing.keys.some((k) => k.state === 'active') &&
    hasLedger(rec)
  )
}

const chainId = (database: string, group: number): string => hex64(`chain-${database}-${group}`)
const aggregateId = (s: MockState, database: string): string =>
  hex64(`aggregate-${l7(s).signing.clusterId ?? 'none'}-${database}`)
export const epochOf = (s: MockState, database: string, group: number): number =>
  l7(s).epochs[`${database}:${group}`] ?? 1
export const isDiverged = (s: MockState, database: string, group: number): boolean =>
  l7(s).diverged.includes(`${database}:${group}`)

/** The signed checkpoint text of a log: origin, size, root, an empty line, a signature line. ASCII except the dash. */
function note(origin: string, size: number, root: string, fingerprint: string): string {
  let bin = ''
  for (let i = 0; i < 64; i += 2) bin += String.fromCharCode(parseInt(root.slice(i, i + 2), 16))
  return `${origin}\n${size}\n${btoa(bin)}\n\n\u2014 ${origin} ${fakePublicKey(fingerprint).slice(0, 24)}\n`
}

export const STATEMENT =
  'Each member says that the chain of that shard group had a block of the given height with the given hash, and that the ' +
  'chain log of that size has the given root, at some instant of the round that the signer reports as the time of the ' +
  'aggregate. With an anchor: no later than the anchor. Two aggregates of one database show that no history of a chain was ' +
  'rewritten between them, unless a member says diverged. This does NOT prove that the heads were read at the same instant, ' +
  'that a transaction across groups is seen in all its groups at the same time, an order of commits between groups, or that ' +
  'a chain is the whole data of the database. A statement about a consistent state of the whole database at one time needs ' +
  'the global snapshot of a later phase.'

export function aggregateSummary(
  s: MockState,
  rec: DbRec,
  size: number,
): S['LedgerAggregateSummary'] {
  const name = rec.info.name
  const groups = groupsOf(s, rec)
  const uncovered = mockConfig.incompleteLedger && groups.length > 1 ? groups.slice(-1) : []
  const key = l7(s).signing.keys.find((k) => k.state === 'active')
  const root = hex64(`aggregate-root-${name}-${size}`)
  return {
    size,
    root,
    checkpoint: note(
      `${l7(s).settings.publication.originPrefix}/a/${aggregateId(s, name)}`,
      size,
      root,
      key?.fingerprint ?? 'none',
    ),
    generatedAt: new Date(Date.now() - (AGGREGATE_SIZE - size + 1) * 90_000).toISOString(),
    complete: uncovered.length === 0,
    uncoveredShardGroupIds: uncovered,
  }
}

export function aggregateMembers(
  s: MockState,
  rec: DbRec,
  size: number,
): S['LedgerAggregateMember'][] {
  const name = rec.info.name
  const groups = groupsOf(s, rec)
  const uncovered = mockConfig.incompleteLedger && groups.length > 1 ? groups.slice(-1) : []
  const included = groups.filter((g) => !uncovered.includes(g))
  return included.map((g) => {
    const head = s.ledger.heights[g] ?? 1000
    const height = Math.max(0, head - (AGGREGATE_SIZE - size) * 2)
    const flags: string[] = []
    if (isDiverged(s, name, g)) flags.push('diverged')
    if (l7(s).publication.health === 'unread' && g === included[included.length - 1])
      flags.push('stale')
    if (l7(s).hostileText !== null) flags.push(l7(s).hostileText as string)
    return {
      shardGroupId: g,
      chainId: chainId(name, g),
      epoch: epochOf(s, name, g),
      height,
      blockHash: fakeHash(`b${g}-${height}`),
      logRoot: hex64(`log-${name}-${g}-${height}`),
      flags,
    }
  })
}

/** The chain logs and the aggregate logs of every database that has a ledger. */
export function publicationLogs(s: MockState): S['LedgerPublicationLog'][] {
  const l = l7(s)
  const behind = l.publication.health === 'behind'
  const newest = new Date(Date.now() - (behind ? 12 * 60_000 : 20_000)).toISOString()
  const logs: S['LedgerPublicationLog'][] = []
  for (const rec of s.databases.filter((d) => published(s, d))) {
    const name = rec.info.name
    for (const g of groupsOf(s, rec)) {
      const epoch = epochOf(s, name, g)
      const prefix = `${l.settings.publication.originPrefix}/c/${chainId(name, g)}`
      for (let e = 1; e <= epoch; e++) {
        const closed = e < epoch
        logs.push({
          kind: 'chain',
          origin: `${prefix}/e${e}`,
          size: (s.ledger.heights[g] ?? 1000) + 1,
          lastCheckpointAt: closed
            ? new Date(Date.now() - 3 * DAY).toISOString()
            : isDiverged(s, name, g)
              ? new Date(Date.now() - 2 * 3600_000).toISOString()
              : newest,
          state: closed ? 'closed' : isDiverged(s, name, g) ? 'diverged' : 'active',
        })
      }
    }
    logs.push({
      kind: 'aggregate',
      origin: `${l.settings.publication.originPrefix}/a/${aggregateId(s, name)}`,
      size: AGGREGATE_SIZE,
      lastCheckpointAt: newest,
      state: 'active',
    })
  }
  return logs
}

// --- what LedgerStatus says about publication and anchoring ---------------------------------------

/** The `publication` member of the status of one database. */
export function publicationOf(
  s: MockState,
  rec: DbRec,
): NonNullable<S['LedgerStatus']['publication']> {
  const l = l7(s)
  if (!l.settings.publication.enabled)
    return { enabled: false, lastCheckpointAt: null, state: 'off' }
  const groups = groupsOf(s, rec)
  const diverged = groups.some((g) => isDiverged(s, rec.info.name, g))
  const behind = l.publication.health === 'behind'
  return {
    enabled: true,
    lastCheckpointAt: new Date(Date.now() - (behind ? 12 * 60_000 : 20_000)).toISOString(),
    state: diverged ? 'diverged' : behind ? 'behind' : 'ok',
  }
}

/** The `anchoring` member of the status of one database. */
export function anchoringOf(s: MockState, rec: DbRec): NonNullable<S['LedgerStatus']['anchoring']> {
  const l = l7(s)
  const enabled = l.targets.filter((t) => t.enabled)
  const anchored = l.anchors
    .filter((a) => a.databaseName === rec.info.name && a.state === 'anchored')
    .map((a) => a.anchoredAt ?? '')
    .sort()
  const newest = anchored.length > 0 ? (anchored[anchored.length - 1] ?? null) : null
  const states = enabled.map((t) => t.state)
  return {
    targets: enabled.length,
    newestAnchoredAt: newest,
    state:
      enabled.length === 0
        ? 'none'
        : states.includes('failed')
          ? 'failed'
          : states.includes('behind')
            ? 'behind'
            : 'ok',
  }
}

/** The anchors of the digest's aggregate, as the digest lists them (`DigestAnchor`). */
export function digestAnchorsOf(s: MockState, database: string, size: number) {
  return l7(s)
    .anchors.filter((a) => a.databaseName === database && a.aggregateSize === size)
    .map((a) => ({
      target:
        a.kind === 'worm'
          ? ('worm' as const)
          : a.kind === 'tsa'
            ? ('tsa' as const)
            : ('openTimestamps' as const),
      status:
        a.state === 'anchored'
          ? ('anchored' as const)
          : a.state === 'failed' || a.state === 'expired'
            ? ('failed' as const)
            : ('pending' as const),
      anchoredAt: a.anchoredAt,
      reference: a.targetName,
    }))
}

// --- alerts of the kinds of 6.9 ------------------------------------------------------------------

interface LedgerAlertKey {
  kind: S['AlertKind']
  databaseName?: string
  shardGroupId?: number
  aspect?: string
}

function sameKey(a: S['Alert'], k: LedgerAlertKey): boolean {
  return (
    a.kind === k.kind &&
    (a.subject?.databaseName ?? null) === (k.databaseName ?? null) &&
    (a.subject?.shardGroupId ?? null) === (k.shardGroupId ?? null) &&
    (a.subject?.aspect ?? null) === (k.aspect ?? null)
  )
}

function raiseLedger(
  s: MockState,
  k: LedgerAlertKey,
  severity: S['AlertSeverity'],
  title: string,
  message: string,
): void {
  if (s.alerts.some((a) => a.state !== 'resolved' && sameKey(a, k))) return
  s.alerts.unshift({
    id: Math.max(0, ...s.alerts.map((a) => a.id)) + 1,
    kind: k.kind,
    severity,
    state: 'open',
    title,
    message,
    serverId: null,
    databaseName: k.databaseName ?? null,
    raisedAt: nowIso(),
    subject: {
      serverId: null,
      behindServerId: null,
      shardGroupId: k.shardGroupId ?? null,
      databaseName: k.databaseName ?? null,
      aspect: k.aspect ?? null,
    },
  })
}

function resolveLedger(s: MockState, k: LedgerAlertKey): void {
  for (const a of s.alerts)
    if (a.state !== 'resolved' && sameKey(a, k)) {
      a.state = 'resolved'
      a.resolvedAt = nowIso()
    }
}

/** Raises and clears the alerts of the kinds of 6.9 from the state of the ledger (no alert exists in a plain mock). */
export function syncLedgerAlerts(s: MockState): void {
  if (s.node.state !== 'clustered') return
  const l = l7(s)
  const set = (
    on: boolean,
    k: LedgerAlertKey,
    sev: S['AlertSeverity'],
    title: string,
    msg: string,
  ) => (on ? raiseLedger(s, k, sev, title, msg) : resolveLedger(s, k))
  set(
    l.signing.signerAlarm !== null,
    { kind: 'ledgerSignerUnavailable' },
    'critical',
    'The ledger cannot sign checkpoints',
    l.signing.signerAlarm ?? 'No checkpoint could be signed.',
  )
  set(
    l.publishedStateAlarm,
    { kind: 'ledgerSignerUnavailable', aspect: 'published-state' },
    'critical',
    'The system database is older than what was published',
    'Nothing is signed: the system database is older than what the ledger published.',
  )
  for (const rec of s.databases.filter(hasLedger)) {
    const name = rec.info.name
    set(
      l.alertNotAnchored && l.targets.filter((t) => t.enabled).length === 0,
      { kind: 'ledgerNotAnchored', databaseName: name },
      l.settings.anchor.required ? 'warning' : 'info',
      `The ledger of ${name} is not anchored`,
      'No anchor target is enabled.',
    )
    const groups = groupsOf(s, rec)
    for (const g of groups) {
      set(
        isDiverged(s, name, g),
        { kind: 'ledgerChainDiverged', databaseName: name, shardGroupId: g },
        'critical',
        `The chain of group ${g} of ${name} diverged`,
        'The chain no longer extends its published log.',
      )
    }
    const last = groups[groups.length - 1] as number
    set(
      l.publication.health === 'unread',
      { kind: 'ledgerGroupUnread', databaseName: name, shardGroupId: last },
      'warning',
      `The chain of group ${last} of ${name} could not be read`,
      'The aggregate repeats its last good state for this group.',
    )
    set(
      l.publication.health === 'behind',
      { kind: 'ledgerPublicationBehind', databaseName: name, shardGroupId: groups[0] as number },
      'warning',
      `The publication of ${name} is behind`,
      'The newest signed checkpoint is older than five publication intervals.',
    )
  }
  for (const t of l.targets) {
    const aspect = `target:${t.id}`
    set(
      t.enabled && t.state === 'failed',
      { kind: 'ledgerAnchorFailed', aspect },
      'critical',
      `Anchoring at ${t.name} failed`,
      'A record failed or expired.',
    )
    set(
      t.enabled && t.state === 'behind',
      { kind: 'ledgerAnchorBehind', aspect },
      'warning',
      `Anchoring at ${t.name} is behind`,
      'The newest checkpoint has not reached the target in time.',
    )
  }
}
