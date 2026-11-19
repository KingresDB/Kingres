// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The data side of the ledger of phase 7 in the mock (contract 0.11.0): the cluster settings of publication and
// anchoring, the signing keys with their events, the anchor targets and records, client keys, bundles and the
// chains that diverged. No logic of requests lives here; `ledger.ts` has it. This file imports no value from
// `state.ts`, so `state.ts` can create the state without a cycle.
import type { components } from '@/api/schema'

type S = components['schemas']

/** The 21 operations the real `kingresd` does not build yet: they answer 422 `unsupported_operation` there. */
export const NOT_BUILT_YET = [
  'getLedgerPublication',
  'listLedgerAnchorTargets',
  'createLedgerAnchorTarget',
  'getLedgerAnchorTarget',
  'updateLedgerAnchorTarget',
  'deleteLedgerAnchorTarget',
  'testLedgerAnchorTarget',
  'getLedgerAggregate',
  'listLedgerAggregates',
  'listLedgerAnchors',
  'anchorLedgerNow',
  'retryLedgerAnchor',
  'downloadLedgerAnchor',
  'createLedgerBundle',
  'getLedgerBundle',
  'listLedgerClientKeys',
  'registerLedgerClientKey',
  'revokeLedgerClientKey',
  'setTableSignaturePolicy',
  'upgradeLedgerExtension',
  'acknowledgeLedgerDivergence',
] as const

/** Deterministic 64 hexadecimal characters (not cryptographic; this is mock data). */
export function hex64(seed: string): string {
  let h = 0x811c9dc5
  let out = ''
  for (let round = 0; round < 8; round++) {
    for (let i = 0; i < seed.length; i++) {
      h ^= seed.charCodeAt(i) + round
      h = Math.imul(h, 0x01000193) >>> 0
    }
    out += h.toString(16).padStart(8, '0')
  }
  return out
}

/** A 32-byte public key, standard base64 with padding (derived from the seed; no relation to a real key). */
export function fakePublicKey(seed: string): string {
  const hex = hex64(`pk-${seed}`)
  let bin = ''
  for (let i = 0; i < 64; i += 2) bin += String.fromCharCode(parseInt(hex.slice(i, i + 2), 16))
  return btoa(bin)
}

/** What a key task does when the next one is made to fail (`?signing=fail:<code>`). */
export interface KeyFailure {
  code: string
  message: string
}

export interface Ledger7State {
  /** True: the 21 operations that the real server does not build yet answer `unsupported_operation`. */
  today: boolean
  settings: S['LedgerSettingsCluster']
  signing: {
    clusterId: string | null
    mode: S['LedgerSigning']['mode']
    passphraseProtected: boolean
    holders: string[]
    keys: S['LedgerSigningKey'][]
    events: S['LedgerSigningEvent'][]
    backupAdvised: boolean
    discontinuity: boolean
    signerAlarm: string | null
  }
  /** True: a database with ledger tables and no enabled target raises `ledgerNotAnchored` (the alerts of a plain mock are none). */
  alertNotAnchored: boolean
  /** 0.12.0: the system database is older than what was published: the alert `ledgerSignerUnavailable` with the subject `published-state`. */
  publishedStateAlarm: boolean
  /** Text a hostile server could send in the statement and in the flags of an aggregate (`?mock=hostile`). Null: the normal text. */
  hostileText: string | null
  /** The next key task ends failed with this code. */
  keyFailure: KeyFailure | null
  publication: {
    listener: S['LedgerPublication']['listener']
    /** 'ok', 'behind' (the newest checkpoint is old) or 'unread' (a group could not be read). */
    health: 'ok' | 'behind' | 'unread'
  }
  /** Shard groups whose chain diverged, as `${database}:${group}`. */
  diverged: string[]
  /** The epoch of a chain after an acknowledged divergence, as `${database}:${group}` (absent: 1). */
  epochs: Record<string, number>
  targets: S['LedgerAnchorTarget'][]
  anchors: (S['LedgerAnchor'] & { databaseName: string })[]
  nextTargetId: number
  nextAnchorId: number
  clientKeys: Record<string, S['LedgerClientKey'][]>
  /** `${database}:${schema}.${table}` of the tables whose policy is `required`. */
  policies: string[]
  bundles: { id: number; databaseName: string; createdAt: string }[]
  nextBundleId: number
}

const MINUTE = 60_000
const DAY = 86_400_000
const at = (msAgo: number): string => new Date(Date.now() - msAgo).toISOString()
const whole = (msAgo: number): string => `${at(msAgo).slice(0, 19)}Z`

/** One key of the set, as the server reports it. */
export function signingKey(
  seed: string,
  state: S['LedgerSigningKeyState'],
  createdMsAgo: number,
  extra: Partial<S['LedgerSigningKey']> = {},
): S['LedgerSigningKey'] {
  return {
    fingerprint: hex64(`key-${seed}`),
    publicKey: fakePublicKey(seed),
    state,
    createdAt: at(createdMsAgo),
    activatedAt: state === 'next' ? null : at(createdMsAgo - MINUTE),
    retiredAt: null,
    revokedAt: null,
    compromisedSince: null,
    maxTreeSize: null,
    ...extra,
  }
}

/** The key set of the default mock: an old key that was retired by a rotation and the key in force. */
function defaultSigning(): Ledger7State['signing'] {
  const old = signingKey('first', 'retired', 120 * DAY, {
    retiredAt: at(60 * DAY),
    maxTreeSize: 1800,
  })
  const current = signingKey('second', 'active', 65 * DAY)
  return {
    clusterId: hex64('cluster-id'),
    mode: 'file',
    passphraseProtected: true,
    holders: ['10.0.0.11', '10.0.0.12'],
    keys: [old, current],
    events: [
      { seq: 0, type: 'key-added', time: whole(120 * DAY) },
      { seq: 1, type: 'key-added', time: whole(65 * DAY) },
      { seq: 2, type: 'key-retired', time: whole(60 * DAY) },
    ],
    backupAdvised: false,
    discontinuity: false,
    signerAlarm: null,
  }
}

/** The ledger of phase 7 as the default mock has it: signed and published, no anchor target. */
export function createLedger7State(): Ledger7State {
  return {
    today: false,
    settings: {
      publication: {
        enabled: true,
        originPrefix: '10.0.0.11/kingres-ledger',
        publicUrl: 'https://ledger.example.com/kingres-ledger',
        intervalSeconds: 60,
        originPrefixLocked: true,
      },
      anchor: { intervalSeconds: 3600, minimumAnchored: 1, required: false },
    },
    signing: defaultSigning(),
    alertNotAnchored: false,
    publishedStateAlarm: false,
    hostileText: null,
    keyFailure: null,
    publication: {
      listener: { address: '0.0.0.0:8444', tls: true, running: true },
      health: 'ok',
    },
    diverged: [],
    epochs: {},
    targets: [],
    anchors: [],
    nextTargetId: 1,
    nextAnchorId: 1,
    clientKeys: {},
    policies: [],
    bundles: [],
    nextBundleId: 1,
  }
}

/** No key exists yet: the first key is not created, the origin prefix is still free (the default is only a proposal). */
export function withoutKeys(l: Ledger7State): void {
  l.signing = {
    clusterId: null,
    mode: 'file',
    passphraseProtected: true,
    holders: ['10.0.0.11', '10.0.0.12'],
    keys: [],
    events: [],
    backupAdvised: false,
    discontinuity: false,
    signerAlarm: null,
  }
  l.settings.publication.enabled = false
  l.settings.publication.originPrefixLocked = false
}

/** True once a key or a checkpoint exists: the origin prefix is the name of every signature. */
export function prefixLocked(l: Ledger7State): boolean {
  return l.signing.keys.length > 0
}
